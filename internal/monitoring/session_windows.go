//go:build windows

package monitoring

import (
	"encoding/binary"
	"fmt"
	"os/exec"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"classsend/internal/devlog"
)

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
	MsgFocus   uint32 = 5
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
	go readPipeBackChannel(pipe, &focusIdx, stopCh)

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
		for {
			current := getStudents()

			// Re-send INIT whenever the student list changes
			if studentsChanged(lastStudents, current) {
				if err := pipeInit(pipe, writeOp, current); err != nil {
					return // pipe broken — monitoring.exe closed
				}
				lastStudents = current
			}

			fi := int(focusIdx.Load())
			if fi >= 0 && fi < len(current) {
				// Focus mode: only poll the focused student, ask for hi-res.
				st := current[fi]
				if err := sendCmd(st.ID, "hi"); err != nil {
					devlog.Logf("monitoring: focus sendCmd failed  student=%s id=%s err=%v", st.Hostname, st.ID, err)
					if err := pipeOffline(pipe, writeOp, uint32(fi)); err != nil {
						return
					}
				} else {
					got := waitForShot(shotCh, st.Hostname, 2500*time.Millisecond, stopCh)
					if got == nil {
						devlog.Logf("monitoring: focus shot TIMEOUT  student=%s", st.Hostname)
						if err := pipeOffline(pipe, writeOp, uint32(fi)); err != nil {
							return
						}
					} else {
						if err := pipeShot(pipe, writeOp, uint32(fi), got); err != nil {
							devlog.Logf("monitoring: pipeShot send failed: %v", err)
							return
						}
						devlog.Logf("monitoring: focus shot sent  student=%s  jpeg=%dB", st.Hostname, len(got))
					}
				}
				// Tighter loop in focus mode — ~1 fps for live-ish feel
				select {
				case <-time.After(800 * time.Millisecond):
				case <-wakeCh:
				case <-stopCh:
					return
				}
				continue
			}

			// Grid mode: round-robin all students.
			devlog.Logf("monitoring: grid round  students=%d", len(current))
			for i, st := range current {
				if focusIdx.Load() >= 0 {
					break // focus mode entered mid-round, abort
				}
				devlog.Logf("monitoring: sendCmd START  student=%s idx=%d", st.Hostname, i)
				sendStart := time.Now()
				if err := sendCmd(st.ID, ""); err != nil {
					devlog.Logf("monitoring: sendCmd FAILED  student=%s after=%v err=%v", st.Hostname, time.Since(sendStart), err)
					if err := pipeOffline(pipe, writeOp, uint32(i)); err != nil {
						return
					}
					continue
				}
				devlog.Logf("monitoring: sendCmd OK  student=%s after=%v", st.Hostname, time.Since(sendStart))

				devlog.Logf("monitoring: waitForShot START  student=%s", st.Hostname)
				waitStart := time.Now()
				got := waitForShot(shotCh, st.Hostname, 1500*time.Millisecond, stopCh)
				if got == nil {
					devlog.Logf("monitoring: shot TIMEOUT  student=%s after=%v idx=%d", st.Hostname, time.Since(waitStart), i)
					if err := pipeOffline(pipe, writeOp, uint32(i)); err != nil {
						return
					}
				} else {
					devlog.Logf("monitoring: waitForShot OK  student=%s after=%v jpeg=%dB", st.Hostname, time.Since(waitStart), len(got))
					devlog.Logf("monitoring: pipeShot START  idx=%d", i)
					pipeStart := time.Now()
					if err := pipeShot(pipe, writeOp, uint32(i), got); err != nil {
						devlog.Logf("monitoring: pipeShot FAILED after=%v err=%v", time.Since(pipeStart), err)
						return
					}
					devlog.Logf("monitoring: pipeShot OK  student=%s after=%v jpeg=%dB", st.Hostname, time.Since(pipeStart), len(got))
				}

				select {
				case <-time.After(150 * time.Millisecond):
				case <-stopCh:
					return
				}
			}

			// Pause between full rounds
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

// readPipeBackChannel reads MsgFocus messages sent by monitoring.exe and
// updates focusIdx atomically. Exits silently when the pipe closes (which
// happens when the monitoring window closes or the session stops).
func readPipeBackChannel(handle uintptr, focusIdx *atomic.Int32, stop <-chan struct{}) {
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
		if msgType == MsgFocus && len(payload) >= 4 {
			idx := binary.LittleEndian.Uint32(payload[0:4])
			if idx == FocusUnset {
				focusIdx.Store(-1)
				devlog.Logf("monitoring: back-channel UNFOCUS")
			} else {
				focusIdx.Store(int32(idx))
				devlog.Logf("monitoring: back-channel FOCUS idx=%d", idx)
			}
		}
	}
}

// waitForShot blocks until a screenshot from hostname arrives or deadline/stop.
// Shots from other students are silently discarded — they'll be requested again
// in the next monitoring round.
func waitForShot(ch <-chan ShotMsg, hostname string, timeout time.Duration, stop <-chan struct{}) []byte {
	deadline := time.After(timeout)
	for {
		select {
		case shot := <-ch:
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
