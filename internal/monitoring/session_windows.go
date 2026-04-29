//go:build windows

package monitoring

import (
	"encoding/binary"
	"fmt"
	"os/exec"
	"syscall"
	"time"
	"unsafe"
)

// Pipe protocol message types — must match monitoring.exe.
const (
	MsgInit    uint32 = 1
	MsgShot    uint32 = 2
	MsgOffline uint32 = 3
	MsgStop    uint32 = 4
)

// PipeName is the named pipe path used between classsend and monitoring.exe.
const PipeName = `\\.\pipe\ClassSendMonitor`

var (
	kernel32dll = syscall.NewLazyDLL("kernel32.dll")

	procCreateNamedPipe  = kernel32dll.NewProc("CreateNamedPipeW")
	procConnectNamedPipe = kernel32dll.NewProc("ConnectNamedPipe")
	procCloseHandle      = kernel32dll.NewProc("CloseHandle")
	procWriteFile        = kernel32dll.NewProc("WriteFile")
)

const (
	pipeAccessOutbound = 0x00000002
	pipeTypeByte       = 0x00000000
	pipeWait           = 0x00000000
	maxPipeInstances   = uint32(1)
	invalidHandle      = ^uintptr(0)
)

// StartSession creates the named pipe, launches monitoring.exe, and starts the
// sequential screenshot loop. Returns a stop function.
//
//   - getStudents   – returns the current connected-student snapshot
//   - sendCmd       – sends CmdRequestShot to one student by ID
//   - shotCh        – receives screenshots forwarded by core (buffered)
//   - exePath       – path to monitoring.exe
func StartSession(
	getStudents func() []StudentInfo,
	sendCmd func(studentID string) error,
	shotCh <-chan ShotMsg,
	exePath string,
) (stop func(), err error) {
	// Create the named pipe (outbound: classsend writes, monitoring reads)
	namePtr, _ := syscall.UTF16PtrFromString(PipeName)
	pipe, _, e := procCreateNamedPipe.Call(
		uintptr(unsafe.Pointer(namePtr)),
		pipeAccessOutbound,
		uintptr(pipeTypeByte|pipeWait),
		uintptr(maxPipeInstances),
		1<<16, // out buffer 64 KB
		0,     // in buffer (we never read)
		5000,  // default timeout ms
		0,     // security attrs
	)
	if pipe == invalidHandle {
		return nil, fmt.Errorf("CreateNamedPipe: %w", e)
	}

	// Launch monitoring.exe before blocking on ConnectNamedPipe
	cmd := exec.Command(exePath)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: false}
	if startErr := cmd.Start(); startErr != nil {
		procCloseHandle.Call(pipe)
		return nil, fmt.Errorf("launch monitoring.exe: %w", startErr)
	}

	// ConnectNamedPipe blocks until monitoring.exe calls CreateFile on the pipe.
	connResult := make(chan error, 1)
	go func() {
		r, _, ce := procConnectNamedPipe.Call(pipe, 0)
		if r == 0 {
			connResult <- fmt.Errorf("ConnectNamedPipe: %w", ce)
		} else {
			connResult <- nil
		}
	}()

	select {
	case connErr := <-connResult:
		if connErr != nil {
			procCloseHandle.Call(pipe)
			cmd.Process.Kill() //nolint:errcheck
			return nil, connErr
		}
	case <-time.After(12 * time.Second):
		procCloseHandle.Call(pipe)
		cmd.Process.Kill() //nolint:errcheck
		return nil, fmt.Errorf("monitoring.exe did not connect within 12 s")
	}

	// Send initial student list
	students := getStudents()
	if initErr := pipeInit(pipe, students); initErr != nil {
		procCloseHandle.Call(pipe)
		cmd.Process.Kill() //nolint:errcheck
		return nil, fmt.Errorf("send init: %w", initErr)
	}

	stopCh := make(chan struct{})

	go func() {
		defer func() {
			// Best-effort: tell monitoring.exe we're done, then close pipe
			pipeMsg(pipe, MsgStop, nil) //nolint:errcheck
			procCloseHandle.Call(pipe)
			cmd.Process.Kill() //nolint:errcheck
		}()

		lastStudents := students
		for {
			current := getStudents()

			// Re-send INIT whenever the student list changes
			if studentsChanged(lastStudents, current) {
				if err := pipeInit(pipe, current); err != nil {
					return // pipe broken — monitoring.exe closed
				}
				lastStudents = current
			}

			for i, st := range current {
				if err := sendCmd(st.ID); err != nil {
					// Student likely disconnected — mark offline and move on
					if err := pipeOffline(pipe, uint32(i)); err != nil {
						return
					}
					continue
				}

				// Wait for this specific student's screenshot (1.5 s timeout)
				got := waitForShot(shotCh, st.Hostname, 1500*time.Millisecond, stopCh)
				if got == nil {
					if err := pipeOffline(pipe, uint32(i)); err != nil {
						return
					}
				} else {
					if err := pipeShot(pipe, uint32(i), got); err != nil {
						return
					}
				}

				// Brief pause between individual requests — keeps network calm
				select {
				case <-time.After(150 * time.Millisecond):
				case <-stopCh:
					return
				}
			}

			// Pause between full rounds
			select {
			case <-time.After(2 * time.Second):
			case <-stopCh:
				return
			}
		}
	}()

	return func() {
		select {
		case <-stopCh: // already stopped
		default:
			close(stopCh)
		}
	}, nil
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

// ── Low-level pipe helpers ─────────────────────────────────────────────────────

func pipeWriteAll(handle uintptr, data []byte) error {
	if len(data) == 0 {
		return nil
	}
	var written uint32
	r, _, e := procWriteFile.Call(
		handle,
		uintptr(unsafe.Pointer(&data[0])),
		uintptr(len(data)),
		uintptr(unsafe.Pointer(&written)),
		0,
	)
	if r == 0 {
		return fmt.Errorf("WriteFile: %w", e)
	}
	return nil
}

func pipeMsg(handle uintptr, msgType uint32, payload []byte) error {
	hdr := make([]byte, 8)
	binary.LittleEndian.PutUint32(hdr[0:4], msgType)
	binary.LittleEndian.PutUint32(hdr[4:8], uint32(len(payload)))
	if err := pipeWriteAll(handle, hdr); err != nil {
		return err
	}
	return pipeWriteAll(handle, payload)
}

func pipeInit(handle uintptr, students []StudentInfo) error {
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
	return pipeMsg(handle, MsgInit, payload)
}

func pipeShot(handle uintptr, index uint32, jpeg []byte) error {
	payload := make([]byte, 8+len(jpeg))
	binary.LittleEndian.PutUint32(payload[0:4], index)
	binary.LittleEndian.PutUint32(payload[4:8], uint32(len(jpeg)))
	copy(payload[8:], jpeg)
	return pipeMsg(handle, MsgShot, payload)
}

func pipeOffline(handle uintptr, index uint32) error {
	payload := make([]byte, 4)
	binary.LittleEndian.PutUint32(payload, index)
	return pipeMsg(handle, MsgOffline, payload)
}
