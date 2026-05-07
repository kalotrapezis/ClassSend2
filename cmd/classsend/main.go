package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"

	tea "github.com/charmbracelet/bubbletea"

	"classsend/internal/buildinfo"
	"classsend/internal/core"
	"classsend/internal/devlog"
	"classsend/internal/ipc"
	"classsend/internal/monitoring"
	"classsend/internal/network"
	"classsend/internal/tui"
)

// defaultRole is overridden at build time via -ldflags "-X main.defaultRole=teacher"
// so we can ship teacher.exe and student.exe from the same source.
var defaultRole = "student"

func main() {
	role := flag.String("role", defaultRole, "Role: teacher or student")
	dev := flag.Bool("dev", false, "Dev mode: scan localhost (same-machine testing)")
	showVer := flag.Bool("version", false, "Print version and exit")
	showVerShort := flag.Bool("ver", false, "Print version and exit (alias)")
	flag.Parse()

	if *showVer || *showVerShort {
		fmt.Println("ClassSend 2  " + buildinfo.String())
		fmt.Println("Role baked in: " + defaultRole)
		os.Exit(0)
	}

	devlog.Init("classsend-" + *role)
	defer devlog.Close()
	devlog.Logf("startup  role=%s  dev=%v  build=%s  exe=%s", *role, *dev, buildinfo.String(), os.Args[0])

	dataDir := dataDirectory()
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		log.Fatalf("cannot create data directory: %v", err)
	}

	app, err := core.NewApp(core.Role(*role), dataDir, *dev)
	if err != nil {
		log.Fatalf("init: %v", err)
	}

	switch core.Role(*role) {
	case core.RoleTeacher:
		if err := app.StartTeacher(); err != nil {
			log.Fatalf("start teacher: %v", err)
		}

	case core.RoleStudent:
		// Try connecting to the background agent first.
		// If it's not running (e.g. dev mode, or agent not yet started),
		// fall back to a direct TCP connection.
		if conn, err := ipc.Dial(); err == nil {
			app.ConnectViaAgent(conn)
		} else {
			if err := app.StartStudent(); err != nil {
				log.Fatalf("start student: %v", err)
			}
		}

	default:
		log.Fatalf("unknown role: %s", *role)
	}

	model := tui.New(app)

	// Teacher: wire student join/leave sidebar events
	if core.Role(*role) == core.RoleTeacher {
		app.OnStudentJoin = func(st *network.Student) {
			model.PushStudentJoin(st.ID, st.Nickname, st.IP)
		}
		app.OnStudentLeave = func(st *network.Student) {
			model.PushStudentLeave(st.ID)
		}
		wireMonitoring(app)
		wireCasting(app)
	}

	p := tea.NewProgram(
		model,
		tea.WithAltScreen(),
		tea.WithMouseCellMotion(),
	)

	if _, err := p.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func dataDirectory() string {
	appData := os.Getenv("APPDATA")
	if appData == "" {
		appData, _ = os.UserHomeDir()
	}
	return filepath.Join(appData, "ClassSend")
}

func wireMonitoring(app *core.App) {
	shotCh := make(chan monitoring.ShotMsg, 8)

	app.OnScreenshot = func(studentID string, jpegData []byte) {
		select {
		case shotCh <- monitoring.ShotMsg{StudentID: studentID, Data: jpegData}:
			devlog.Logf("OnScreenshot: queued  student=%s jpeg=%dB chLen=%d", studentID, len(jpegData), len(shotCh))
		default:
			devlog.Logf("OnScreenshot: DROPPED (channel full)  student=%s jpeg=%dB", studentID, len(jpegData))
		}
	}

	var (
		sessionMu    sync.Mutex
		sessionStop  func()
		sessionNudge func()
	)

	app.OnMonitoringStart = func() {
		devlog.Logf("OnMonitoringStart fired")
		sessionMu.Lock()
		defer sessionMu.Unlock()
		if sessionStop != nil {
			devlog.Logf("OnMonitoringStart: session already running, skip")
			return
		}

		getStudents := func() []monitoring.StudentInfo {
			students := app.Server.Students()
			out := make([]monitoring.StudentInfo, len(students))
			for i, st := range students {
				out[i] = monitoring.StudentInfo{
					ID:       st.ID,
					Hostname: st.Hostname,
					Nickname: st.Nickname,
				}
			}
			return out
		}

		sendCmd := func(studentID, param string) error {
			return app.RequestShotParam(studentID, param)
		}

		exePath := findMonitoringExe()
		devlog.Logf("monitoring: StartSession exePath=%s", exePath)

		// onEnded fires when the session goroutine returns — usually because
		// the user closed the monitoring window. Without resetting state, a
		// subsequent --t tvon would no-op because State.Monitoring stays true.
		onEnded := func() {
			devlog.Logf("monitoring: session ended, clearing state")
			sessionMu.Lock()
			sessionStop = nil
			sessionNudge = nil
			sessionMu.Unlock()
			app.MarkMonitoringEnded()
		}

		stop, nudge, err := monitoring.StartSession(getStudents, sendCmd, shotCh, exePath, onEnded)
		if err != nil {
			devlog.Logf("monitoring: StartSession FAILED: %v", err)
			log.Printf("monitoring: %v", err)
			return
		}
		devlog.Logf("monitoring: StartSession ok")
		sessionStop = stop
		sessionNudge = nudge
	}

	app.OnMonitoringStop = func() {
		devlog.Logf("OnMonitoringStop fired")
		sessionMu.Lock()
		defer sessionMu.Unlock()
		if sessionStop != nil {
			sessionStop()
			sessionStop = nil
			sessionNudge = nil
		}
	}

	// Late-joining students: nudge the monitoring session so it re-INITs
	// the grid immediately instead of waiting up to ~2 s for the next round.
	// Wraps the existing OnStudentJoin set in main(); we capture the prior
	// hook so the TUI sidebar update still fires.
	prevJoin := app.OnStudentJoin
	app.OnStudentJoin = func(st *network.Student) {
		if prevJoin != nil {
			prevJoin(st)
		}
		sessionMu.Lock()
		nudge := sessionNudge
		sessionMu.Unlock()
		if nudge != nil {
			devlog.Logf("monitoring: nudging session for late-join  student=%s", st.Hostname)
			nudge()
		}
	}
}

func wireCasting(app *core.App) {
	var (
		castMu    sync.Mutex
		castSrv   *network.CastServer
		castStop  chan struct{}
	)

	app.OnStartCasting = func() (string, error) {
		castMu.Lock()
		defer castMu.Unlock()
		if castSrv != nil {
			return castSrv.LocalAddr(), nil // already running
		}
		srv, err := network.NewCastServer()
		if err != nil {
			return "", err
		}
		castSrv = srv

		ch := make(chan struct{})
		castStop = ch
		go runCastCapture(srv, ch)

		return srv.LocalAddr(), nil
	}

	app.OnStopCasting = func() {
		castMu.Lock()
		defer castMu.Unlock()
		if castStop != nil {
			close(castStop)
			castStop = nil
		}
		if castSrv != nil {
			castSrv.Close()
			castSrv = nil
		}
	}
}

func findMonitoringExe() string {
	exe, err := os.Executable()
	if err != nil {
		return "monitoring.exe"
	}
	return filepath.Join(filepath.Dir(exe), "monitoring.exe")
}
