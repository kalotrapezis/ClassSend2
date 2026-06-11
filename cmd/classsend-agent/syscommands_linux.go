//go:build linux

// Linux student-side system commands.
//
// Design notes:
//
//   - Lock screen is a POLL, not a one-shot. CmdLockScreen sets lockState=true
//     and a 2 s ticker keeps invoking `loginctl lock-session` until
//     CmdUnlockScreen clears the flag. The student CAN dismiss the lock by
//     entering their password, but within ≤2 s it relocks. This sidesteps
//     X11/Wayland — `loginctl` is systemd-logind, display-server agnostic.
//
//   - Monitoring banner is a system notification (`notify-send`). Wayland
//     has no portable always-on-top window API, and a notification reads as
//     "the OS is telling you something" which fits the UX better anyway.
//
//   - Mute uses `pactl set-sink-mute … toggle`. Pipewire-pulse provides pactl
//     on Mint 22, real PulseAudio on Mint 21 — same command both ways.
//
//   - Screenshot tries gnome-screenshot → scrot → import in order. Wayland
//     compositor screenshot portals are deferred (xdg-desktop-portal D-Bus)
//     until X11 actually goes away.
//
//   - Cast viewer is mpv as a subprocess; the agent dials the teacher's
//     CastServer directly and pipes fMP4 bytes to mpv stdin. No separate
//     castviewer binary on Linux.

package main

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"classsend/internal/core"
	"classsend/internal/devlog"
	"classsend/internal/network"
	"classsend/internal/protocol"
)

// ── Stress / busy guard (mirror of Windows file) ──────────────────────────────

const stressInflightLimit = 6

var (
	stressSem      = make(chan struct{}, stressInflightLimit)
	stressBusyDrop atomic.Uint64
	errAgentBusy   = errors.New("agent busy — command dropped")
)

// ── Lock: poll-based ──────────────────────────────────────────────────────────

var (
	lockState        atomic.Bool
	lockEnforcerOnce sync.Once
)

// startLockEnforcer launches the 2 s ticker that re-locks the session while
// lockState is true. Called once at agent startup (from setupStudentCommands).
func startLockEnforcer() {
	lockEnforcerOnce.Do(func() {
		go func() {
			t := time.NewTicker(2 * time.Second)
			defer t.Stop()
			for range t.C {
				if !lockState.Load() {
					continue
				}
				if err := exec.Command("loginctl", "lock-session").Run(); err != nil {
					devlog.Logf("lock-enforcer: loginctl failed: %v", err)
				}
			}
		}()
	})
}

func lockScreen() error {
	lockState.Store(true)
	// Lock immediately too — the ticker only fires every 2 s.
	if err := exec.Command("loginctl", "lock-session").Run(); err != nil {
		return fmt.Errorf("loginctl lock-session: %w", err)
	}
	devlog.Logf("lockScreen: state=true (re-lock every 2s)")
	return nil
}

func unlockScreen() {
	// We do NOT actually unlock — that requires the user's password.
	// Just stop the re-lock loop so the next time they unlock manually it
	// stays unlocked.
	lockState.Store(false)
	devlog.Logf("unlockScreen: state=false (re-lock disarmed)")
}

// ── Mute toggle ───────────────────────────────────────────────────────────────

func muteAudio() {
	if err := exec.Command("pactl", "set-sink-mute", "@DEFAULT_SINK@", "toggle").Run(); err != nil {
		devlog.Logf("muteAudio: pactl failed: %v", err)
	}
}

// ── Launch / focus / close ────────────────────────────────────────────────────

func launchApp(path string) error {
	// Use a shell so the teacher can pass args inline ("firefox --kiosk URL").
	cmd := exec.Command("sh", "-c", path)
	cmd.Stdout = nil
	cmd.Stderr = nil
	return cmd.Start()
}

// Window control: instead of branching on the session type (which is brittle —
// XDG vars lie, and Xwayland muddies "is this X11 or Wayland"), we run a
// best-effort cascade and let each tool act where it can. Two layers:
//
//   - wmctrl talks to the X server. On an X11 session that's everything; on a
//     Wayland session it's Xwayland, which still backs a lot of apps (browsers,
//     Electron, anything not yet ported), so wmctrl can close/focus those too.
//
//   - KWin's built-in "Window Close" global shortcut, fired via kglobalaccel,
//     closes the focused *native* Wayland window. It's present on every KDE
//     install regardless of X11/Wayland, so it's the reliable universal lever
//     for the windows wmctrl can't see. Plasma 6 killed the old KWin
//     "loadScript" D-Bus trick, but global shortcuts still work.
//
// closeVisibleApps does BOTH, slowly, so X11/Xwayland and native-Wayland windows
// all get swept. focusApp tries wmctrl (the only thing that can target a window
// by title); native-Wayland-only focus isn't exposed by any compositor.

func desktopIsKDE() bool {
	d := strings.ToLower(os.Getenv("XDG_CURRENT_DESKTOP") + " " + os.Getenv("XDG_SESSION_DESKTOP"))
	return strings.Contains(d, "kde") || strings.Contains(d, "plasma")
}

func haveCmd(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

// kwinInvokeShortcut triggers a KWin global action by its locale-independent
// name (e.g. "Window Close") through kglobalaccel. Works on KDE under both X11
// and Wayland; returns an error (so the caller can stop) when KWin/kglobalaccel
// isn't the running shell.
func kwinInvokeShortcut(name string) error {
	return exec.Command("dbus-send", "--session", "--type=method_call",
		"--dest=org.kde.kglobalaccel",
		"/component/kwin",
		"org.kde.kglobalaccel.Component.invokeShortcut",
		"string:"+name).Run()
}

func focusApp(titleSubstr string) error {
	if titleSubstr == "" {
		return errors.New("focusApp: empty substring")
	}
	// wmctrl -a substring-matches the title and raises it. Works on X11, and on
	// Wayland for any app rendered through Xwayland.
	if haveCmd("wmctrl") {
		if err := exec.Command("wmctrl", "-a", titleSubstr).Run(); err == nil {
			return nil
		}
	}
	// No compositor exposes "focus native-Wayland window by title" to outside
	// apps, so for a pure-Wayland target there's nothing more we can do.
	devlog.Logf("focusApp: could not focus %q (wmctrl missing/failed; native "+
		"Wayland windows aren't targetable)", titleSubstr)
	return errors.New("focusApp: unavailable (need wmctrl, X11 or an Xwayland app)")
}

func closeVisibleApps() {
	closedAny := false

	// Layer 1 — X11 / Xwayland windows, closed precisely by id via wmctrl.
	if haveCmd("wmctrl") {
		if closeVisibleAppsWmctrl() {
			closedAny = true
		}
	}

	// Layer 2 — native Wayland windows (and a universal KDE fallback). Fire
	// KWin's "Window Close" at the focused window repeatedly: each call closes
	// the foreground window and KWin focuses the next, peeling the stack. Done
	// slowly so the compositor can refocus between closes. The panel/desktop
	// never take focus, so they're left alone; on a non-KDE shell the first
	// invocation errors and we stop. ("Every distro will have the Wayland way"
	// via KDE — and it no-ops harmlessly where it doesn't apply.)
	if haveCmd("dbus-send") {
		for i := 0; i < 12; i++ {
			if err := kwinInvokeShortcut("Window Close"); err != nil {
				if !closedAny {
					devlog.Logf("closeVisibleApps: KWin Window Close unavailable: %v", err)
				}
				break
			}
			closedAny = true
			time.Sleep(250 * time.Millisecond)
		}
	}

	if !closedAny {
		devlog.Logf("closeVisibleApps: no usable window-control method — install " +
			"wmctrl (X11/Xwayland) or run a KDE session (Wayland)")
	}
}

// closeVisibleAppsWmctrl enumerates X-server top-level windows, skips the
// shell/panel and our own, and closes the rest. Reports whether it ran the
// enumeration (so the caller knows wmctrl was usable).
func closeVisibleAppsWmctrl() bool {
	out, err := exec.Command("wmctrl", "-l").Output()
	if err != nil {
		devlog.Logf("closeVisibleApps: wmctrl -l failed: %v", err)
		return false
	}
	for _, line := range strings.Split(string(out), "\n") {
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		id := fields[0]
		title := strings.Join(fields[3:], " ")
		lower := strings.ToLower(title)
		// Skip the desktop / panel and our own monitor notification.
		if strings.Contains(lower, "desktop") || strings.Contains(lower, "panel") ||
			strings.Contains(lower, "classsend") {
			continue
		}
		if err := exec.Command("wmctrl", "-ic", id).Run(); err != nil {
			devlog.Logf("closeVisibleApps: wmctrl -ic %s failed: %v", id, err)
		}
		time.Sleep(120 * time.Millisecond) // close slowly
	}
	return true
}

// ── Monitoring notification (banner) ──────────────────────────────────────────

var (
	notifMu sync.Mutex
	notifID uint32 // 0 = no active notification
)

func showMonitoringNotification() {
	notifMu.Lock()
	defer notifMu.Unlock()

	args := []string{
		"--urgency=normal",
		"--expire-time=0", // never auto-expire; we close it explicitly
		"--icon=camera-web",
		"--print-id",
	}
	// Replace the previous one if we still have its ID.
	if notifID != 0 {
		args = append(args, fmt.Sprintf("--replace-id=%d", notifID))
	}
	args = append(args, "ClassSend", "Παρακολούθηση οθόνης ενεργή")

	out, err := exec.Command("notify-send", args...).Output()
	if err != nil {
		devlog.Logf("showMonitoringNotification: notify-send failed: %v", err)
		return
	}
	idStr := strings.TrimSpace(string(out))
	if id, perr := strconv.ParseUint(idStr, 10, 32); perr == nil {
		notifID = uint32(id)
	}
}

func hideMonitoringNotification() {
	notifMu.Lock()
	defer notifMu.Unlock()

	if notifID == 0 {
		return
	}
	// CloseNotification via D-Bus. gdbus is in glib2.0-bin (always present
	// on Mint with Cinnamon).
	err := exec.Command("gdbus", "call",
		"--session",
		"--dest=org.freedesktop.Notifications",
		"--object-path=/org/freedesktop/Notifications",
		"--method=org.freedesktop.Notifications.CloseNotification",
		strconv.FormatUint(uint64(notifID), 10),
	).Run()
	if err != nil {
		devlog.Logf("hideMonitoringNotification: gdbus close failed: %v", err)
	}
	notifID = 0
}

// ── Screenshot ────────────────────────────────────────────────────────────────

func captureScreen() ([]byte, error)   { return captureScreenSized(640, 50) }
func captureScreenHi() ([]byte, error) { return captureScreenSized(2400, 80) }

// screenshotTools is tried in order; first one that's installed AND produces a
// non-empty file wins. Wayland-capable tools come first (spectacle for KDE,
// grim for wlroots/Sway, gnome-screenshot for GNOME) because the X11-only
// fallbacks (scrot, ImageMagick's import) silently fail under Wayland — which
// is the default on Fedora KDE, GNOME, etc. Without a Wayland tool the teacher's
// live monitoring grid would show nothing for that student.
var screenshotTools = []struct {
	bin  string
	args func(out string) []string
}{
	{"gnome-screenshot", func(o string) []string { return []string{"-f", o} }},
	{"spectacle", func(o string) []string { return []string{"-b", "-n", "-f", "-o", o} }},
	{"grim", func(o string) []string { return []string{o} }},
	{"scrot", func(o string) []string { return []string{"-z", o} }},
	{"import", func(o string) []string { return []string{"-window", "root", o} }},
}

func captureScreenSized(maxEdge, quality int) ([]byte, error) {
	tmp, err := os.CreateTemp("", "cs-shot-*.png")
	if err != nil {
		return nil, fmt.Errorf("tempfile: %w", err)
	}
	tmp.Close()
	defer os.Remove(tmp.Name())

	var lastErr error
	captured := false
	for _, st := range screenshotTools {
		if _, err := exec.LookPath(st.bin); err != nil {
			continue
		}
		cmd := exec.Command(st.bin, st.args(tmp.Name())...)
		if err := cmd.Run(); err != nil {
			lastErr = fmt.Errorf("%s: %w", st.bin, err)
			continue
		}
		fi, err := os.Stat(tmp.Name())
		if err != nil || fi.Size() == 0 {
			lastErr = fmt.Errorf("%s: empty output", st.bin)
			continue
		}
		captured = true
		break
	}
	if !captured {
		if lastErr == nil {
			lastErr = errors.New("no screenshot tool found (install gnome-screenshot, scrot, or imagemagick)")
		}
		return nil, lastErr
	}

	f, err := os.Open(tmp.Name())
	if err != nil {
		return nil, fmt.Errorf("open shot: %w", err)
	}
	defer f.Close()
	img, err := png.Decode(f)
	if err != nil {
		return nil, fmt.Errorf("decode png: %w", err)
	}

	// Convert to NRGBA so downscaleNRGBA can chew on it.
	b := img.Bounds()
	nrgba := image.NewNRGBA(b)
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			nrgba.Set(x, y, img.At(x, y))
		}
	}
	scaled := downscaleNRGBA(nrgba, maxEdge)

	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, scaled, &jpeg.Options{Quality: quality}); err != nil {
		return nil, fmt.Errorf("jpeg encode: %w", err)
	}
	return buf.Bytes(), nil
}

func downscaleNRGBA(src *image.NRGBA, maxEdge int) *image.NRGBA {
	srcW := src.Rect.Dx()
	srcH := src.Rect.Dy()
	longest := srcW
	if srcH > longest {
		longest = srcH
	}
	if longest <= maxEdge {
		return src
	}
	scale := float64(maxEdge) / float64(longest)
	dstW := int(float64(srcW) * scale)
	dstH := int(float64(srcH) * scale)
	dst := image.NewNRGBA(image.Rect(0, 0, dstW, dstH))
	xRatio := float64(srcW) / float64(dstW)
	yRatio := float64(srcH) / float64(dstH)
	for y := 0; y < dstH; y++ {
		sy := int(float64(y) * yRatio)
		srcRow := src.Pix[sy*src.Stride:]
		dstRow := dst.Pix[y*dst.Stride:]
		for x := 0; x < dstW; x++ {
			sx := int(float64(x) * xRatio)
			off := sx * 4
			dstRow[x*4+0] = srcRow[off+0]
			dstRow[x*4+1] = srcRow[off+1]
			dstRow[x*4+2] = srcRow[off+2]
			dstRow[x*4+3] = srcRow[off+3]
		}
	}
	return dst
}

// ── Cast viewer (mpv subprocess + TCP pump) ───────────────────────────────────

var (
	castMu       sync.Mutex
	castProc     *exec.Cmd
	castStop     chan struct{}
	lastCastAddr string
)

// showCastingViewer re-spawns the cast viewer at the last known teacher
// address. Used by the student TUI's --cast IPC ping. No-op if we never had
// a cast in this session.
func showCastingViewer() {
	castMu.Lock()
	addr := lastCastAddr
	castMu.Unlock()
	if addr == "" {
		devlog.Logf("showCastingViewer: no prior addr, ignoring")
		return
	}
	startCastViewer(addr)
}

// startCastViewer dials the teacher's CastServer at addr (host:port — multiple
// addresses comma-separated for multi-NIC teachers, first reachable wins),
// spawns mpv reading from stdin, and pumps fMP4 frames into mpv until either
// the TCP side closes or hideCastingViewer is called.
func startCastViewer(addrCSV string) {
	castMu.Lock()
	stopExisting := castStop
	castMu.Unlock()
	if stopExisting != nil {
		hideCastingViewer()
	}

	conn, dialedAddr, err := dialCastAny(addrCSV)
	if err != nil {
		devlog.Logf("startCastViewer: all addrs failed (%s): %v", addrCSV, err)
		return
	}
	devlog.Logf("startCastViewer: connected to %s", dialedAddr)

	cmd := exec.Command("mpv",
		"--no-cache",
		"--untimed",
		"--keep-open=yes",
		"--profile=low-latency",
		"--title=ClassSend — Cast",
		"-",
	)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		conn.Close()
		devlog.Logf("startCastViewer: mpv StdinPipe: %v", err)
		return
	}
	if err := cmd.Start(); err != nil {
		conn.Close()
		stdin.Close()
		devlog.Logf("startCastViewer: mpv start: %v", err)
		return
	}

	stop := make(chan struct{})
	castMu.Lock()
	castProc = cmd
	castStop = stop
	lastCastAddr = addrCSV
	castMu.Unlock()

	// Pump TCP frames → mpv stdin.
	go func() {
		defer stdin.Close()
		defer conn.Close()
		br := bufio.NewReader(conn)
		for {
			select {
			case <-stop:
				return
			default:
			}
			payload, err := network.CastReadFrame(br)
			if err != nil {
				devlog.Logf("cast pump: read: %v", err)
				return
			}
			if _, err := stdin.Write(payload); err != nil {
				devlog.Logf("cast pump: mpv write: %v", err)
				return
			}
		}
	}()

	// Reaper: clear castProc when mpv exits on its own (e.g. user closed window).
	go func() {
		_ = cmd.Wait()
		castMu.Lock()
		if castProc == cmd {
			castProc = nil
			castStop = nil
		}
		castMu.Unlock()
	}()
}

func hideCastingViewer() {
	castMu.Lock()
	cmd := castProc
	stop := castStop
	castProc = nil
	castStop = nil
	castMu.Unlock()
	if stop != nil {
		close(stop)
	}
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}

// dialCastAny parses a comma-separated list of host:port addresses and dials
// each in turn (3 s timeout). First connection wins; the others are skipped.
func dialCastAny(addrCSV string) (net.Conn, string, error) {
	addrs := strings.Split(addrCSV, ",")
	var lastErr error
	for _, raw := range addrs {
		addr := strings.TrimSpace(raw)
		if addr == "" {
			continue
		}
		conn, err := net.DialTimeout("tcp", addr, 3*time.Second)
		if err != nil {
			lastErr = err
			continue
		}
		return conn, addr, nil
	}
	if lastErr == nil {
		lastErr = errors.New("no valid cast addresses")
	}
	return nil, "", lastErr
}

// ── Autostart (XDG) ───────────────────────────────────────────────────────────

func autostartPath() (string, error) {
	u, err := user.Current()
	if err != nil {
		return "", err
	}
	cfg := os.Getenv("XDG_CONFIG_HOME")
	if cfg == "" {
		cfg = filepath.Join(u.HomeDir, ".config")
	}
	return filepath.Join(cfg, "autostart", "classsend-agent.desktop"), nil
}

func isAutostartEnabled() bool {
	p, err := autostartPath()
	if err != nil {
		return false
	}
	_, err = os.Stat(p)
	return err == nil
}

func setAutostart(enable bool) error {
	p, err := autostartPath()
	if err != nil {
		return err
	}
	if !enable {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}

	exe, err := os.Executable()
	if err != nil {
		return err
	}
	exe, _ = filepath.Abs(exe)

	desktop := fmt.Sprintf(`[Desktop Entry]
Type=Application
Name=ClassSend Agent
Comment=ClassSend student-side background agent
Exec=%s
Hidden=false
NoDisplay=true
X-GNOME-Autostart-enabled=true
Terminal=false
`, exe)

	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, []byte(desktop), 0o644)
}

func ensureAutostart() {
	if isAutostartEnabled() {
		return
	}
	if err := setAutostart(true); err != nil {
		devlog.Logf("ensureAutostart: %v", err)
	}
}

// ── Misc ──────────────────────────────────────────────────────────────────────

func hideConsole() { /* no console on Linux daemon */ }

// startMonitoring / stopMonitoring exist on Windows for the GDI handle leak
// watchdog. Linux has no equivalent — these are no-ops kept for symmetry.
func startMonitoring(_ func([]byte)) {}
func stopMonitoring()                {}

// startLoadMonitor / isUnderLoad exist on Windows to back off screenshotting
// when the student PC is CPU-starved. Linux relies on the stressSem inflight
// guard in runGuarded instead, so these are no-ops kept for symmetry with the
// shared agent main.
func startLoadMonitor() {}
func isUnderLoad() bool { return false }

// ── runGuarded (mirror of Windows file) ───────────────────────────────────────

func runGuarded(app *core.App, cmd protocol.CommandPayload, fn func()) {
	select {
	case stressSem <- struct{}{}:
	default:
		stressBusyDrop.Add(1)
		devlog.Logf("stress: BUSY drop  action=%s param=%q inflight=%d drops=%d",
			cmd.Action, cmd.Param, len(stressSem), stressBusyDrop.Load())
		if cmd.CmdID != "" {
			app.SendCmdAck(cmd.CmdID, cmd.Action, errAgentBusy)
		}
		return
	}
	go func() {
		defer func() {
			<-stressSem
			if r := recover(); r != nil {
				devlog.Logf("PANIC in handler  action=%s err=%v\n%s",
					cmd.Action, r, debug.Stack())
			}
		}()
		fn()
	}()
}

func withRetry(attempts int, delay time.Duration, fn func() error) error {
	var err error
	for i := 0; i < attempts; i++ {
		if err = fn(); err == nil {
			return nil
		}
		time.Sleep(delay)
	}
	return err
}

// ── Health beacon ─────────────────────────────────────────────────────────────

func startHealthBeacon() {
	go func() {
		var ms runtime.MemStats
		for {
			time.Sleep(30 * time.Second)
			runtime.ReadMemStats(&ms)
			devlog.Logf("health  goroutines=%d  heap=%d KB  inflight=%d  busy_drops=%d  lock=%v",
				runtime.NumGoroutine(),
				ms.HeapAlloc/1024,
				len(stressSem),
				stressBusyDrop.Load(),
				lockState.Load(),
			)
		}
	}()
}

// ── Dispatcher ────────────────────────────────────────────────────────────────

func setupStudentCommands(app *core.App, devMode bool) {
	startLockEnforcer()

	sendShot := func(data []byte) {
		msg, err := protocol.Encode(protocol.TypeScreenshot, protocol.ScreenshotPayload{
			StudentID: app.Hostname,
			Data:      data,
		})
		if err != nil {
			devlog.Logf("sendShot encode failed: %v", err)
			return
		}
		if app.Client == nil {
			devlog.Logf("sendShot DROPPED: app.Client is nil  jpeg=%dB", len(data))
			return
		}
		if sendErr := app.Client.Send(msg); sendErr != nil {
			devlog.Logf("sendShot send failed: %v  jpeg=%dB", sendErr, len(data))
			return
		}
		devlog.Logf("sendShot ok  jpeg=%dB", len(data))
	}

	report := func(cmd protocol.CommandPayload, err error) {
		app.SendCmdAck(cmd.CmdID, cmd.Action, err)
	}

	app.OnCommand = func(cmd protocol.CommandPayload) {
		switch cmd.Action {

		case protocol.CmdLockScreen:
			runGuarded(app, cmd, func() {
				if err := withRetry(3, 500*time.Millisecond, lockScreen); err != nil {
					report(cmd, err)
					return
				}
				if devMode {
					time.Sleep(5 * time.Second)
					unlockScreen()
				}
			})

		case protocol.CmdUnlockScreen:
			runGuarded(app, cmd, func() { unlockScreen() })

		case protocol.CmdShutdown:
			// systemctl poweroff is the modern equivalent of `shutdown /s`.
			// Will fail silently if the user is not in the sudoers/polkit
			// rule for this — on a school deployment we'd configure a
			// polkit policy.
			_ = exec.Command("systemctl", "poweroff").Start()

		case protocol.CmdLaunchApp:
			if cmd.Param != "" {
				p := cmd.Param
				runGuarded(app, cmd, func() {
					if err := withRetry(3, 500*time.Millisecond, func() error {
						return launchApp(p)
					}); err != nil {
						report(cmd, err)
					}
				})
			}

		case protocol.CmdFocusApp:
			if cmd.Param != "" {
				p := cmd.Param
				runGuarded(app, cmd, func() {
					if err := withRetry(3, 500*time.Millisecond, func() error {
						return focusApp(p)
					}); err != nil {
						report(cmd, err)
					}
				})
			}

		case protocol.CmdCloseApps:
			runGuarded(app, cmd, func() { closeVisibleApps() })

		case protocol.CmdMute, protocol.CmdUnmute:
			muteAudio()

		case protocol.CmdStartMonitor:
			showMonitoringNotification()

		case protocol.CmdStopMonitor:
			hideMonitoringNotification()
			stopMonitoring()

		case protocol.CmdStartCast:
			if cmd.Param != "" {
				startCastViewer(cmd.Param)
			} else {
				devlog.Logf("CmdStartCast: empty param, ignoring")
			}

		case protocol.CmdStopCast:
			hideCastingViewer()

		case protocol.CmdRequestShot:
			hires := cmd.Param == "hi"
			devlog.Logf("CmdRequestShot received  hi=%v", hires)
			runGuarded(app, cmd, func() {
				var (
					data []byte
					err  error
				)
				if hires {
					data, err = captureScreenHi()
				} else {
					data, err = captureScreen()
				}
				if err != nil {
					devlog.Logf("captureScreen failed: %v  hi=%v", err, hires)
					return
				}
				sendShot(data)
			})
		}
	}
}

