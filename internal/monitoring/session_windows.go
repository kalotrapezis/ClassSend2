//go:build windows

package monitoring

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"image/jpeg"
	"os/exec"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"classsend/internal/devlog"
)

// isShotMostlyBlack decodes the JPEG and samples a 16×16 pixel grid to compute
// average Rec.601 luma. Returns true if the image is dark enough that BitBlt
// likely returned a stale/cleared back-buffer instead of the real desktop —
// the well-known DWM compositor quirk on certain Win10 Intel iGPU drivers,
// where a fraction of BitBlts (sometimes 2-of-3 in a clockwork pattern) come
// back as solid black even though the screen has real content.
//
// We can't detect this condition on the agent side reliably (the ratio shifts
// with load, monitor count, and other compositor state — engineering for a
// specific number is fragile). Instead the teacher inspects every shot and
// drops the black ones, asking that student again immediately.
//
// Decode cost: ~2-5 ms for a 640 px thumbnail, ~30-50 ms for a 2400 px focus
// shot. Sampling 256 pixels after decode is microseconds.
func isShotMostlyBlack(jpegData []byte) bool {
	img, err := jpeg.Decode(bytes.NewReader(jpegData))
	if err != nil {
		// Couldn't decode → assume non-black so we don't drop legit frames
		// just because the JPEG is in some weird format.
		return false
	}
	bounds := img.Bounds()
	w := bounds.Dx()
	h := bounds.Dy()
	if w == 0 || h == 0 {
		return true
	}
	const grid = 16
	const threshold = 8 // 0..255 — same threshold the WebView2 monitoring used
	var sum, count int64
	for gy := 0; gy < grid; gy++ {
		y := bounds.Min.Y + gy*h/grid
		for gx := 0; gx < grid; gx++ {
			x := bounds.Min.X + gx*w/grid
			r, g, b, _ := img.At(x, y).RGBA()
			// RGBA() returns 16-bit values; shift to 8-bit for luma calc.
			r8, g8, b8 := int64(r>>8), int64(g>>8), int64(b>>8)
			sum += (r8*299 + g8*587 + b8*114) / 1000
			count++
		}
	}
	return sum/count < threshold
}

// Pipe protocol message types — must match monitoring.exe.
//
// MsgInit, MsgShot, MsgOffline, MsgStop go teacher → monitoring.
// MsgFocus goes monitoring → teacher: payload is one uint32 = focused cell
// index, or 0xFFFFFFFF to leave focus mode and resume normal grid polling.
const (
	MsgInit    uint32 = 1
	MsgShot    uint32 = 2
	MsgOffline uint32 = 3
	MsgStop    uint32 = 4
	MsgFocus       uint32 = 5
	MsgStopRequest uint32 = 6 // monitoring → teacher: user pressed ^W in the window; clean up the session
)

// FocusUnset is the sentinel index meaning "leave focus mode".
const FocusUnset uint32 = 0xFFFFFFFF

// PipeName is the named pipe path used between classsend and monitoring.exe.
const PipeName = `\\.\pipe\ClassSendMonitor`

var (
	kernel32dll = syscall.NewLazyDLL("kernel32.dll")

	procCreateNamedPipe     = kernel32dll.NewProc("CreateNamedPipeW")
	procConnectNamedPipe    = kernel32dll.NewProc("ConnectNamedPipe")
	procCloseHandle         = kernel32dll.NewProc("CloseHandle")
	procWriteFile           = kernel32dll.NewProc("WriteFile")
	procReadFile            = kernel32dll.NewProc("ReadFile")
	procCreateEventW        = kernel32dll.NewProc("CreateEventW")
	procWaitForSingleObject = kernel32dll.NewProc("WaitForSingleObject")
	procCancelIoEx          = kernel32dll.NewProc("CancelIoEx")
	procGetOverlappedResult = kernel32dll.NewProc("GetOverlappedResult")
	procResetEvent          = kernel32dll.NewProc("ResetEvent")
)

const (
	pipeAccessDuplex   = 0x00000003 // both ends can read/write
	pipeTypeByte       = 0x00000000
	pipeWait           = 0x00000000
	maxPipeInstances   = uint32(1)
	invalidHandle      = ^uintptr(0)
	fileFlagOverlapped = 0x40000000
)

const (
	errorIoPending      = 997
	errorPipeConnected  = 535
	errorOperationAbort = 995
	waitObject0         = 0x00000000
	waitTimeout         = 0x00000102
	waitFailed          = 0xFFFFFFFF
	infinite            = 0xFFFFFFFF
)

// ── Overlapped pipe I/O ──────────────────────────────────────────────────────
//
// Why overlapped: synchronous WriteFile/ReadFile on a Win32 byte-mode named
// pipe can wedge the calling thread for minutes when the peer's I/O state
// machine gets out of sync — even with PIPE_WAIT and an empty buffer. We hit
// this in production: pipeShot blocked 60+ seconds for a 13 KB write that
// should have completed in microseconds. With FILE_FLAG_OVERLAPPED we issue
// the I/O asynchronously, wait on an event with a real timeout, and call
// CancelIoEx if the operation refuses to complete.

// pipeOp holds the OVERLAPPED struct + event handle pair used to drive one
// async pipe operation. The event is manual-reset so we can ResetEvent and
// reuse it across calls. Each goroutine that does pipe I/O owns one pipeOp;
// they MUST NOT be shared across goroutines because OVERLAPPED is per-call.
type pipeOp struct {
	ov    syscall.Overlapped
	event uintptr
}

func newPipeOp() (*pipeOp, error) {
	// Manual-reset event, initially nonsignaled.
	h, _, e := procCreateEventW.Call(0, 1, 0, 0)
	if h == 0 {
		return nil, fmt.Errorf("CreateEvent: %w", e)
	}
	op := &pipeOp{event: h}
	op.ov.HEvent = syscall.Handle(h)
	return op, nil
}

func (op *pipeOp) close() {
	if op == nil || op.event == 0 {
		return
	}
	procCloseHandle.Call(op.event)
	op.event = 0
}

// reset prepares the OVERLAPPED for a fresh I/O call. The Internal/Offset
// fields must be zeroed (the kernel writes status into them); the event
// must be reset to nonsignaled so WaitForSingleObject will block.
func (op *pipeOp) reset() {
	op.ov = syscall.Overlapped{HEvent: syscall.Handle(op.event)}
	procResetEvent.Call(op.event)
}

// pipeWriteAll writes the full buffer to the pipe, with a per-call timeout.
// On timeout it calls CancelIoEx so the kernel doesn't keep the I/O queued.
func pipeWriteAll(handle uintptr, op *pipeOp, data []byte, timeout time.Duration) error {
	if len(data) == 0 {
		return nil
	}
	op.reset()
	var written uint32
	ret, _, e := procWriteFile.Call(
		handle,
		uintptr(unsafe.Pointer(&data[0])),
		uintptr(len(data)),
		uintptr(unsafe.Pointer(&written)),
		uintptr(unsafe.Pointer(&op.ov)),
	)
	if ret != 0 {
		// Completed synchronously — written reflects the byte count.
		if int(written) != len(data) {
			return fmt.Errorf("WriteFile partial: %d/%d", written, len(data))
		}
		return nil
	}
	if e != syscall.Errno(errorIoPending) {
		return fmt.Errorf("WriteFile: %w", e)
	}
	// Operation pending — wait on the event with the caller's timeout.
	waitMs := uint32(timeout / time.Millisecond)
	if waitMs == 0 {
		waitMs = 1
	}
	wRet, _, _ := procWaitForSingleObject.Call(op.event, uintptr(waitMs))
	if wRet == waitTimeout {
		// The write didn't complete in time. Cancel it and wait for the
		// cancellation to actually finish (otherwise the OVERLAPPED struct
		// may be touched by the kernel after this function returns).
		procCancelIoEx.Call(handle, uintptr(unsafe.Pointer(&op.ov)))
		procWaitForSingleObject.Call(op.event, infinite)
		return fmt.Errorf("WriteFile timeout after %v", timeout)
	}
	if wRet != waitObject0 {
		return fmt.Errorf("WriteFile WaitForSingleObject failed: 0x%x", wRet)
	}
	var transferred uint32
	gRet, _, gErr := procGetOverlappedResult.Call(
		handle,
		uintptr(unsafe.Pointer(&op.ov)),
		uintptr(unsafe.Pointer(&transferred)),
		0, // bWait = FALSE: event already signaled
	)
	if gRet == 0 {
		return fmt.Errorf("GetOverlappedResult: %w", gErr)
	}
	if int(transferred) != len(data) {
		return fmt.Errorf("WriteFile partial: %d/%d", transferred, len(data))
	}
	return nil
}

// pipeReadFull reads exactly len(buf) bytes, looping over partial reads, with
// an overall timeout. Returns the bytes read on success, or an error.
func pipeReadFull(handle uintptr, op *pipeOp, buf []byte, timeout time.Duration) error {
	if len(buf) == 0 {
		return nil
	}
	deadline := time.Now().Add(timeout)
	read := 0
	for read < len(buf) {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return fmt.Errorf("ReadFile timeout")
		}
		op.reset()
		var nRead uint32
		ret, _, e := procReadFile.Call(
			handle,
			uintptr(unsafe.Pointer(&buf[read])),
			uintptr(len(buf)-read),
			uintptr(unsafe.Pointer(&nRead)),
			uintptr(unsafe.Pointer(&op.ov)),
		)
		if ret == 0 && e != syscall.Errno(errorIoPending) {
			return fmt.Errorf("ReadFile: %w", e)
		}
		if ret == 0 {
			waitMs := uint32(remaining / time.Millisecond)
			if waitMs == 0 {
				waitMs = 1
			}
			wRet, _, _ := procWaitForSingleObject.Call(op.event, uintptr(waitMs))
			if wRet == waitTimeout {
				procCancelIoEx.Call(handle, uintptr(unsafe.Pointer(&op.ov)))
				procWaitForSingleObject.Call(op.event, infinite)
				return fmt.Errorf("ReadFile timeout")
			}
			if wRet != waitObject0 {
				return fmt.Errorf("ReadFile WaitForSingleObject failed: 0x%x", wRet)
			}
			var transferred uint32
			gRet, _, gErr := procGetOverlappedResult.Call(
				handle,
				uintptr(unsafe.Pointer(&op.ov)),
				uintptr(unsafe.Pointer(&transferred)),
				0,
			)
			if gRet == 0 {
				return fmt.Errorf("GetOverlappedResult (read): %w", gErr)
			}
			nRead = transferred
		}
		if nRead == 0 {
			return fmt.Errorf("ReadFile: pipe closed")
		}
		read += int(nRead)
	}
	return nil
}

// pipeMsg writes a {msgType,payLen,payload} frame as ONE buffer. Coalescing
// header + body into a single WriteFile is more reliable than two writes
// against an overlapped pipe — the kernel can't interleave writes from a
// concurrent CancelIoEx, and we never end up with a half-written frame.
func pipeMsg(handle uintptr, op *pipeOp, msgType uint32, payload []byte) error {
	frame := make([]byte, 8+len(payload))
	binary.LittleEndian.PutUint32(frame[0:4], msgType)
	binary.LittleEndian.PutUint32(frame[4:8], uint32(len(payload)))
	if len(payload) > 0 {
		copy(frame[8:], payload)
	}
	return pipeWriteAll(handle, op, frame, 5*time.Second)
}

// StartSession creates the named pipe, launches monitoring.exe, and starts the
// sequential screenshot loop. Returns a stop function.
//
//   - getStudents   – returns the current connected-student snapshot
//   - sendCmd       – sends CmdRequestShot to one student by ID
//   - shotCh        – receives screenshots forwarded by core (buffered)
//   - exePath       – path to monitoring.exe
func StartSession(
	getStudents func() []StudentInfo,
	sendCmd func(studentID, param string) error,
	shotCh <-chan ShotMsg,
	exePath string,
	onEnded func(), // called once when the session goroutine exits (window closed, pipe broke, or stop called)
) (stop func(), nudge func(), err error) {
	// Create the duplex named pipe in OVERLAPPED mode. Out buffer 1 MB so a
	// big screenshot never fills it; in buffer 4 KB for focus messages.
	namePtr, _ := syscall.UTF16PtrFromString(PipeName)
	pipe, _, e := procCreateNamedPipe.Call(
		uintptr(unsafe.Pointer(namePtr)),
		pipeAccessDuplex|fileFlagOverlapped,
		uintptr(pipeTypeByte|pipeWait),
		uintptr(maxPipeInstances),
		1<<20, // out buffer 1 MB
		1<<12, // in buffer 4 KB
		5000,  // default timeout ms
		0,     // security attrs
	)
	if pipe == invalidHandle {
		return nil, nil, fmt.Errorf("CreateNamedPipe: %w", e)
	}

	// Launch monitoring.exe before blocking on ConnectNamedPipe.
	// CRITICAL: detach from the parent console. Without DETACHED_PROCESS the
	// child inherits the teacher TUI's console input handle, and after the
	// spawn bubbletea's key reader gets confused — Enter starts inserting
	// newlines into the textarea instead of triggering Send. Stdin/stdout/
	// stderr are explicitly nil'd for the same reason.
	cmd := exec.Command(exePath)
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    false,
		CreationFlags: 0x00000008, // DETACHED_PROCESS
	}
	devlog.Logf("monitoring: spawning %s", exePath)
	if startErr := cmd.Start(); startErr != nil {
		procCloseHandle.Call(pipe)
		return nil, nil, fmt.Errorf("launch monitoring.exe: %w", startErr)
	}

	// ConnectNamedPipe in overlapped mode: it ALWAYS returns 0; check
	// GetLastError. ERROR_IO_PENDING means wait on the event;
	// ERROR_PIPE_CONNECTED means the client raced us and is already connected.
	connOp, err := newPipeOp()
	if err != nil {
		procCloseHandle.Call(pipe)
		cmd.Process.Kill() //nolint:errcheck
		return nil, nil, err
	}
	connOp.reset()
	cRet, _, cErr := procConnectNamedPipe.Call(pipe, uintptr(unsafe.Pointer(&connOp.ov)))
	if cRet == 0 && cErr != syscall.Errno(errorIoPending) && cErr != syscall.Errno(errorPipeConnected) {
		connOp.close()
		procCloseHandle.Call(pipe)
		cmd.Process.Kill() //nolint:errcheck
		return nil, nil, fmt.Errorf("ConnectNamedPipe: %w", cErr)
	}
	if cRet == 0 && cErr == syscall.Errno(errorIoPending) {
		wRet, _, _ := procWaitForSingleObject.Call(connOp.event, 12000)
		if wRet == waitTimeout {
			procCancelIoEx.Call(pipe, uintptr(unsafe.Pointer(&connOp.ov)))
			procWaitForSingleObject.Call(connOp.event, infinite)
			connOp.close()
			procCloseHandle.Call(pipe)
			cmd.Process.Kill() //nolint:errcheck
			devlog.Logf("monitoring: pipe connect TIMEOUT after 12s")
			return nil, nil, fmt.Errorf("monitoring.exe did not connect within 12 s")
		}
	}
	connOp.close()
	devlog.Logf("monitoring: pipe connected")

	// Each goroutine that does pipe I/O needs its own pipeOp because the
	// OVERLAPPED struct is per-operation. The session goroutine writes;
	// the back-channel reader reads. Don't share these.
	writeOp, err := newPipeOp()
	if err != nil {
		procCloseHandle.Call(pipe)
		cmd.Process.Kill() //nolint:errcheck
		return nil, nil, err
	}

	// Send initial student list
	students := getStudents()
	devlog.Logf("monitoring: sending init  students=%d", len(students))
	if initErr := pipeInit(pipe, writeOp, students); initErr != nil {
		writeOp.close()
		procCloseHandle.Call(pipe)
		cmd.Process.Kill() //nolint:errcheck
		return nil, nil, fmt.Errorf("send init: %w", initErr)
	}

	stopCh := make(chan struct{})

	// wakeCh is signalled by the nudge() func returned to the caller. The
	// polling loop uses it to short-circuit its inter-round sleep so a
	// newly-joined student appears in the grid promptly instead of waiting
	// up to ~2 s + (rest of round). Buffered=1 so a nudge fired while the
	// loop is mid-round doesn't block the caller; a single buffered tick is
	// enough since the loop coalesces multiple joins on its next pass.
	wakeCh := make(chan struct{}, 1)

	// focusIdx is updated by the back-channel reader goroutine when
	// monitoring.exe sends MsgFocus. The polling loop reads it on every
	// iteration to decide whether to do a normal grid round or focus on
	// one student at high resolution.
	var focusIdx atomic.Int32
	focusIdx.Store(-1) // -1 = grid mode

	// Back-channel reader: monitoring.exe → teacher. Owns its own pipeOp.
	// Two messages: MsgFocus (cell selection) and MsgStopRequest (^W in the
	// monitoring window). We pass a closure that triggers stop() so the
	// reader can ask for session shutdown without owning stopCh directly.
	stopOnce := sync.Once{}
	requestStop := func() {
		stopOnce.Do(func() {
			devlog.Logf("monitoring: stop requested via back-channel (^W)")
			select {
			case <-stopCh:
			default:
				close(stopCh)
			}
		})
	}
	go readPipeBackChannel(pipe, &focusIdx, stopCh, requestStop)

	// Two decoupled goroutines:
	//
	//   REQUEST PACER  — sends ONE CmdRequestShot every ~2 s in round-
	//   robin order. Sequential, slow, polite to the WiFi. Does NOT wait
	//   for the response.
	//
	//   RESPONSE ROUTER — drains shotCh forever. Every shot that arrives
	//   is routed to its cell by hostname → index lookup, no matter when
	//   it arrived or whose "turn" it was. Also owns all pipe writes
	//   (Init / Shot / Offline / Stop) — a single goroutine on writeOp,
	//   so no concurrency on the OVERLAPPED struct.
	//
	// Why this shape: with ~60% capture-failure rate per call on flaky
	// BitBlt drivers, we want every successful frame the agent ever
	// produces to land in its cell, regardless of timing. We can't fix
	// the agent's per-PC capture quirks; we can stop discarding the
	// frames it does manage to send. Each PC is independent.

	// Per-student state shared by both goroutines.
	//
	//   lastSeen      — last time any shot was routed for this student.
	//                   Used to mark cells offline after a quiet period.
	//   outstanding   — count of CmdRequestShot's sent without ANY shot
	//                   coming back. Reset to 0 every time we hear back.
	//   nextOK        — earliest time the pacer will send to this student
	//                   again. Cleared when a shot arrives.
	//   blackStreak   — consecutive black shots received. Reset on a
	//                   non-black one. Caps the retry-on-black loop so
	//                   we don't spam the agent forever when capture is
	//                   permanently broken.
	//   hasGoodFrame  — true once we've routed a non-black frame from this
	//                   student. The black filter ONLY suppresses frames
	//                   when this is true — so a brand-new cell whose
	//                   first capture is black still gets painted (better
	//                   to show something than the placeholder forever).
	//
	// Backoff schedule (slow classroom hardware — Edge takes 30 s to open,
	// Compress 60 s; we don't want to hammer a stalled PC, but we also can't
	// give up on it for too long):
	//
	//   outstanding < 2  → no delay (send on the next 2 s slot)
	//   outstanding == 2 → wait 10 s before next send
	//   outstanding ≥ 3 → wait 20 min before next send
	type studentState struct {
		lastSeen     time.Time
		outstanding  int
		nextOK       time.Time
		blackStreak  int
		hasGoodFrame bool
	}
	stateMu := &sync.Mutex{}
	states := make(map[string]*studentState, len(students))
	for _, st := range students {
		states[st.Hostname] = &studentState{lastSeen: time.Now()}
	}

	getState := func(hostname string) *studentState {
		stateMu.Lock()
		defer stateMu.Unlock()
		s, ok := states[hostname]
		if !ok {
			s = &studentState{lastSeen: time.Now()}
			states[hostname] = s
		}
		return s
	}

	backoffFor := func(outstanding int) time.Duration {
		switch {
		case outstanding < 2:
			return 0
		case outstanding == 2:
			return 10 * time.Second
		default:
			return 20 * time.Minute
		}
	}

	// Response router.
	go func() {
		defer func() {
			// Best-effort: tell monitoring.exe we're done, then close pipe.
			pipeMsg(pipe, writeOp, MsgStop, nil) //nolint:errcheck
			writeOp.close()
			procCloseHandle.Call(pipe)
			cmd.Process.Kill() //nolint:errcheck
			if onEnded != nil {
				onEnded()
			}
		}()

		lastStudents := students
		offlineTicker := time.NewTicker(2 * time.Second)
		defer offlineTicker.Stop()

		// Cap on how many times we'll re-request from the same student
		// in a row when every response comes back black. After this we
		// give up and let the pacer's normal cadence ask again next slot
		// — the cell keeps its last good thumbnail, no flicker to black.
		const blackRetryCap = 3

		for {
			select {
			case shot := <-shotCh:
				current := getStudents()
				if studentsChanged(lastStudents, current) {
					if err := pipeInit(pipe, writeOp, current); err != nil {
						return
					}
					lastStudents = current
				}

				// Load short-circuit: the student PC is overloaded and chose
				// to skip its capture this round. Bump lastSeen (so the cell
				// isn't marked offline) and don't repaint — the previous
				// thumbnail stays. No retry: the agent's next CmdRequestShot
				// will either capture normally or skip again on its own.
				if shot.Status == "load" {
					stateMu.Lock()
					s, ok := states[shot.StudentID]
					if !ok {
						s = &studentState{}
						states[shot.StudentID] = s
					}
					s.outstanding = 0
					s.nextOK = time.Time{}
					s.lastSeen = time.Now()
					stateMu.Unlock()
					devlog.Logf("monitoring: shot suppressed (under load)  student=%s", shot.StudentID)
					break
				}

				// Filter all-black frames — but only when the cell already
				// has a good thumbnail to keep showing. A brand-new cell
				// whose first capture is black still gets painted (better
				// to show *something* there than to leave the placeholder
				// "Αναμονή..." indefinitely on a PC whose BitBlt is broken).
				black := isShotMostlyBlack(shot.Data)

				stateMu.Lock()
				s, ok := states[shot.StudentID]
				if !ok {
					s = &studentState{}
					states[shot.StudentID] = s
				}
				s.outstanding = 0
				s.nextOK = time.Time{}
				s.lastSeen = time.Now()
				if black {
					s.blackStreak++
				} else {
					s.blackStreak = 0
					s.hasGoodFrame = true
				}
				suppress := black && s.hasGoodFrame
				retryAgain := suppress && s.blackStreak <= blackRetryCap
				streak := s.blackStreak
				stateMu.Unlock()

				if suppress {
					if retryAgain {
						devlog.Logf("monitoring: black shot, retrying  student=%s streak=%d", shot.StudentID, streak)
						go func(id, host string) {
							if err := sendCmd(id, ""); err != nil {
								devlog.Logf("monitoring: black-retry sendCmd failed  student=%s err=%v", host, err)
							}
						}(getIDForHostname(current, shot.StudentID), shot.StudentID)
					} else {
						devlog.Logf("monitoring: black shot, retry cap hit  student=%s streak=%d (giving up this round)", shot.StudentID, streak)
					}
					// Don't paint — cell keeps its last good thumbnail.
					break
				}

				// Either non-black, or first-ever frame for this cell — paint it.
				for i, st := range current {
					if st.Hostname == shot.StudentID {
						if err := pipeShot(pipe, writeOp, uint32(i), shot.Data); err != nil {
							devlog.Logf("monitoring: pipeShot FAILED idx=%d err=%v", i, err)
							return
						}
						tag := "ok"
						if black {
							tag = "first-black"
						}
						devlog.Logf("monitoring: shot routed  student=%s idx=%d jpeg=%dB (%s)", st.Hostname, i, len(shot.Data), tag)
						break
					}
				}
			case <-offlineTicker.C:
				current := getStudents()
				if studentsChanged(lastStudents, current) {
					if err := pipeInit(pipe, writeOp, current); err != nil {
						return
					}
					lastStudents = current
				}
				now := time.Now()
				stateMu.Lock()
				for i, st := range current {
					s, ok := states[st.Hostname]
					if !ok || now.Sub(s.lastSeen) > 30*time.Second {
						if err := pipeOffline(pipe, writeOp, uint32(i)); err != nil {
							stateMu.Unlock()
							return
						}
					}
				}
				stateMu.Unlock()
			case <-stopCh:
				return
			}
		}
	}()

	// Request pacer.
	go func() {
		idx := 0
		for {
			select {
			case <-stopCh:
				return
			default:
			}

			fi := int(focusIdx.Load())
			current := getStudents()

			if fi >= 0 && fi < len(current) {
				// Focus mode — ask only the focused student, hi-res, ~1 fps.
				// Backoff doesn't apply: the teacher is actively watching this
				// PC and explicitly asked for hi-res; we keep trying.
				st := current[fi]
				if err := sendCmd(st.ID, "hi"); err != nil {
					devlog.Logf("monitoring: focus sendCmd failed  student=%s err=%v", st.Hostname, err)
				}
				select {
				case <-time.After(800 * time.Millisecond):
				case <-wakeCh:
				case <-stopCh:
					return
				}
				continue
			}

			// Grid mode — find the next student in round-robin order who
			// isn't currently in backoff. If everyone's backed off, just
			// sleep and try again next tick.
			if len(current) > 0 {
				now := time.Now()
				for tries := 0; tries < len(current); tries++ {
					st := current[(idx+tries)%len(current)]
					s := getState(st.Hostname)

					stateMu.Lock()
					inBackoff := now.Before(s.nextOK)
					stateMu.Unlock()
					if inBackoff {
						continue
					}

					if err := sendCmd(st.ID, ""); err != nil {
						devlog.Logf("monitoring: sendCmd failed  student=%s err=%v", st.Hostname, err)
					} else {
						stateMu.Lock()
						s.outstanding++
						if d := backoffFor(s.outstanding); d > 0 {
							s.nextOK = now.Add(d)
							devlog.Logf("monitoring: backoff  student=%s outstanding=%d wait=%v",
								st.Hostname, s.outstanding, d)
						}
						stateMu.Unlock()
					}
					idx = (idx + tries + 1) % len(current)
					break
				}
			}

			// Pace: one request every 2 s. With 9 students and nobody in
			// backoff that's a full round every ~18 s. Slow agents that miss
			// twice get parked for 10 s, then 20 min if still silent — a
			// realistic window given Edge takes 30 s to open and Compress 60 s.
			select {
			case <-time.After(2 * time.Second):
			case <-wakeCh:
			case <-stopCh:
				return
			}
		}
	}()

	stop = func() {
		select {
		case <-stopCh: // already stopped
		default:
			close(stopCh)
		}
	}
	nudge = func() {
		select {
		case wakeCh <- struct{}{}:
		default: // buffer full — a wake is already pending, no need for another
		}
	}
	return stop, nudge, nil
}

// readPipeBackChannel reads back-channel messages from monitoring.exe and
// dispatches them: MsgFocus updates focusIdx, MsgStopRequest invokes
// requestStop (the user pressed ^W in the window — clean up the session).
// Exits silently when the pipe closes (window closed by X click or process
// killed) or when stop fires.
func readPipeBackChannel(handle uintptr, focusIdx *atomic.Int32, stop <-chan struct{}, requestStop func()) {
	op, err := newPipeOp()
	if err != nil {
		devlog.Logf("monitoring: back-channel newPipeOp: %v", err)
		return
	}
	defer op.close()
	hdr := make([]byte, 8)
	for {
		select {
		case <-stop:
			return
		default:
		}
		// Use a long timeout (1 minute) per read so the goroutine wakes up
		// periodically to check stopCh even if no focus messages arrive.
		if err := pipeReadFull(handle, op, hdr, time.Minute); err != nil {
			return
		}
		msgType := binary.LittleEndian.Uint32(hdr[0:4])
		payLen := binary.LittleEndian.Uint32(hdr[4:8])
		var payload []byte
		if payLen > 0 {
			payload = make([]byte, payLen)
			if err := pipeReadFull(handle, op, payload, 5*time.Second); err != nil {
				return
			}
		}
		switch msgType {
		case MsgFocus:
			if len(payload) >= 4 {
				idx := binary.LittleEndian.Uint32(payload[0:4])
				if idx == FocusUnset {
					focusIdx.Store(-1)
					devlog.Logf("monitoring: back-channel UNFOCUS")
				} else {
					focusIdx.Store(int32(idx))
					devlog.Logf("monitoring: back-channel FOCUS idx=%d", idx)
				}
			}
		case MsgStopRequest:
			devlog.Logf("monitoring: back-channel STOP request")
			if requestStop != nil {
				requestStop()
			}
			return
		}
	}
}

// waitForShot blocks until a screenshot from hostname arrives or deadline/stop.
// Shots from other students are silently discarded — used by focus mode where
// only one student is being polled and stray frames are uninteresting.
func waitForShot(ch <-chan ShotMsg, hostname string, timeout time.Duration, stop <-chan struct{}) []byte {
	deadline := time.After(timeout)
	for {
		select {
		case shot := <-ch:
			if shot.Status == "load" {
				// Overloaded — focus-mode caller treats this as "no shot
				// this round" so the focus pane keeps the last frame.
				continue
			}
			if shot.StudentID == hostname {
				return shot.Data
			}
			// Wrong student (stale shot) — discard and keep waiting
		case <-deadline:
			return nil
		case <-stop:
			return nil
		}
	}
}

// waitForShotDispatch is the grid-mode variant: any shot for *any* student
// in `students` is forwarded to its own cell so a late frame from a slow
// student doesn't end up in the bin just because we've already moved on to
// poll the next one. Returns the target student's frame bytes, or nil on
// timeout. Why this exists: BitBlt on Win10 occasionally takes 1.5–2 s to
// return; the previous waitForShot would discard those frames as "wrong
// student" because the loop had already advanced. Slow students therefore
// updated their cell roughly never. With dispatch-in-flight, every shot that
// makes it across the wire lands in the right cell, regardless of timing.
func waitForShotDispatch(
	ch <-chan ShotMsg,
	target string,
	students []StudentInfo,
	pipe uintptr,
	op *pipeOp,
	timeout time.Duration,
	stop <-chan struct{},
) []byte {
	deadline := time.After(timeout)
	for {
		select {
		case shot := <-ch:
			if shot.Status == "load" {
				// Overloaded student — skip. The main grid loop is the one
				// that owns lastSeen bookkeeping; here we just ignore it.
				continue
			}
			if shot.StudentID == target {
				return shot.Data
			}
			// Late shot from another student — route it to its cell now
			// instead of discarding. If the hostname isn't in the current
			// list (student left), drop silently.
			for i, st := range students {
				if st.Hostname == shot.StudentID {
					if err := pipeShot(pipe, op, uint32(i), shot.Data); err != nil {
						devlog.Logf("monitoring: late-shot pipe failed  student=%s err=%v", shot.StudentID, err)
					} else {
						devlog.Logf("monitoring: late-shot routed  student=%s idx=%d jpeg=%dB", shot.StudentID, i, len(shot.Data))
					}
					break
				}
			}
		case <-deadline:
			return nil
		case <-stop:
			return nil
		}
	}
}

// getIDForHostname looks up a student's ID by hostname in the current list.
// Returns "" if not found, in which case sendCmd will fail harmlessly.
func getIDForHostname(students []StudentInfo, hostname string) string {
	for _, st := range students {
		if st.Hostname == hostname {
			return st.ID
		}
	}
	return ""
}

// studentsChanged returns true if the two lists differ in any way.
func studentsChanged(a, b []StudentInfo) bool {
	if len(a) != len(b) {
		return true
	}
	for i := range a {
		if a[i].ID != b[i].ID {
			return true
		}
	}
	return false
}

// ── Frame builders ───────────────────────────────────────────────────────────

func pipeInit(handle uintptr, op *pipeOp, students []StudentInfo) error {
	var payload []byte
	countBuf := make([]byte, 4)
	binary.LittleEndian.PutUint32(countBuf, uint32(len(students)))
	payload = append(payload, countBuf...)

	for _, st := range students {
		name := st.Nickname
		if name == "" {
			name = st.Hostname
		}
		nameBytes := []byte(name)
		lenBuf := make([]byte, 4)
		binary.LittleEndian.PutUint32(lenBuf, uint32(len(nameBytes)))
		payload = append(payload, lenBuf...)
		payload = append(payload, nameBytes...)
	}
	return pipeMsg(handle, op, MsgInit, payload)
}

func pipeShot(handle uintptr, op *pipeOp, index uint32, jpeg []byte) error {
	payload := make([]byte, 8+len(jpeg))
	binary.LittleEndian.PutUint32(payload[0:4], index)
	binary.LittleEndian.PutUint32(payload[4:8], uint32(len(jpeg)))
	copy(payload[8:], jpeg)
	return pipeMsg(handle, op, MsgShot, payload)
}

func pipeOffline(handle uintptr, op *pipeOp, index uint32) error {
	payload := make([]byte, 4)
	binary.LittleEndian.PutUint32(payload, index)
	return pipeMsg(handle, op, MsgOffline, payload)
}
