// monitortest — drive monitoring.exe directly through its named pipe so the
// WebView2 grid can be exercised without a teacher.exe + real students.
//
// What it does:
//
//  1. Creates the \\.\pipe\ClassSendMonitor named pipe just like the teacher.
//  2. Spawns monitoring.exe.
//  3. Sends MsgInit with three synthetic student names.
//  4. Loops: every ~700 ms picks a student, sends a MsgShot with a freshly
//     generated JPEG (gradient + name + frame counter so each student is
//     visually distinct and updating).
//  5. Every ~10 s reorders the student list and re-sends MsgInit. THIS IS
//     THE KEY REGRESSION TEST: with the old monitoring.exe, this would wipe
//     all screenshots; with the fixed one, screenshots follow their student
//     by name across reorders.
//  6. Every ~15 s marks one student offline (red tint, image preserved).
//
// Usage:
//
//	monitortest                 # spawns monitoring.exe from cwd
//	monitortest -exe path.exe   # use a specific monitoring.exe
//	monitortest -duration 60s   # auto-quit after this long (default: forever)
//
//go:build windows

package main

import (
	"bytes"
	"encoding/binary"
	"flag"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"log"
	"math/rand"
	"os/exec"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

// ── Pipe protocol (matches internal/monitoring/session_windows.go) ────────────

const (
	msgInit    uint32 = 1
	msgShot    uint32 = 2
	msgOffline uint32 = 3
	msgStop    uint32 = 4
)

const pipeName = `\\.\pipe\ClassSendMonitor`

// ── Win32 pipe API ────────────────────────────────────────────────────────────

var (
	kernel32                = syscall.NewLazyDLL("kernel32.dll")
	procCreateNamedPipe     = kernel32.NewProc("CreateNamedPipeW")
	procConnectNamedPipe    = kernel32.NewProc("ConnectNamedPipe")
	procWriteFile           = kernel32.NewProc("WriteFile")
	procCloseHandle         = kernel32.NewProc("CloseHandle")
	procCreateEventW        = kernel32.NewProc("CreateEventW")
	procWaitForSingleObject = kernel32.NewProc("WaitForSingleObject")
	procCancelIoEx          = kernel32.NewProc("CancelIoEx")
	procGetOverlappedResult = kernel32.NewProc("GetOverlappedResult")
	procResetEvent          = kernel32.NewProc("ResetEvent")
)

const (
	pipeAccessDuplex   = 0x00000003
	pipeTypeByte       = 0x00000000
	pipeWait           = 0x00000000
	fileFlagOverlapped = 0x40000000
	invalidHandle      = ^uintptr(0)

	errorIoPending     = 997
	errorPipeConnected = 535
	waitObject0        = 0x00000000
	waitTimeout        = 0x00000102
	infinite           = 0xFFFFFFFF
)

func main() {
	exePath := flag.String("exe", "monitoring.exe", "path to monitoring.exe")
	duration := flag.Duration("duration", 0, "auto-quit after this long (0 = forever)")
	badRate := flag.Float64("bad-rate", 0.0, "fraction of frames to corrupt (0.0–1.0). At 0.05, ~1-in-20 shots is a bad JPEG; the cell should keep its previous good frame, never go black. Use this to verify the bad-frame guard.")
	flag.Parse()

	log.SetFlags(log.LstdFlags | log.Lmicroseconds)

	pipe, err := createPipe()
	if err != nil {
		log.Fatalf("CreateNamedPipe: %v", err)
	}
	defer closeHandle(pipe)

	cmd := exec.Command(*exePath)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    false,
		CreationFlags: 0x00000008, // DETACHED_PROCESS
	}
	if err := cmd.Start(); err != nil {
		log.Fatalf("spawn %s: %v", *exePath, err)
	}
	log.Printf("spawned monitoring.exe pid=%d", cmd.Process.Pid)
	defer func() {
		_ = cmd.Process.Kill()
	}()

	if err := waitConnect(pipe, 12*time.Second); err != nil {
		log.Fatalf("ConnectNamedPipe: %v", err)
	}
	log.Printf("monitoring.exe connected to pipe")

	// Three synthetic students — name format mimics real hostnames.
	students := []student{
		{name: "PC-LAB-01", hue: 200},
		{name: "PC-LAB-02", hue: 110},
		{name: "PC-LAB-03", hue: 0},
	}
	if err := sendInit(pipe, students); err != nil {
		log.Fatalf("init: %v", err)
	}
	log.Printf("sent INIT with %d students", len(students))

	var deadline <-chan time.Time
	if *duration > 0 {
		deadline = time.After(*duration)
	}

	shotTicker := time.NewTicker(700 * time.Millisecond)
	defer shotTicker.Stop()
	reorderTicker := time.NewTicker(10 * time.Second)
	defer reorderTicker.Stop()
	offlineTicker := time.NewTicker(15 * time.Second)
	defer offlineTicker.Stop()

	frame := 0
	var orderMu sync.Mutex
	currentOrder := make([]int, len(students)) // currentOrder[idx] = original student index
	for i := range currentOrder {
		currentOrder[i] = i
	}

	for {
		select {
		case <-deadline:
			log.Printf("duration reached — sending stop")
			_ = sendFrame(pipe, msgStop, nil)
			time.Sleep(500 * time.Millisecond)
			return
		case <-shotTicker.C:
			frame++
			orderMu.Lock()
			pos := rand.Intn(len(currentOrder))
			origIdx := currentOrder[pos]
			orderMu.Unlock()
			st := students[origIdx]

			// With probability bad-rate, emit a corrupted frame instead of
			// a real screenshot. Mix the four kinds of garbage we'd
			// realistically see from the agent in production: empty
			// payload, truncated JPEG (SOI but no EOI), raw garbage that
			// looks nothing like a JPEG, and a one-byte sliver. The
			// monitoring fix is supposed to drop all four and keep the
			// last good frame visible.
			var jpegBytes []byte
			isBad := *badRate > 0 && rand.Float64() < *badRate
			if isBad {
				switch rand.Intn(4) {
				case 0:
					jpegBytes = nil // zero bytes
				case 1:
					good := renderShot(st.name, st.hue, 1280, 720, frame)
					jpegBytes = good[:len(good)/3] // truncated
				case 2:
					jpegBytes = []byte{0xDE, 0xAD, 0xBE, 0xEF, 0xCA, 0xFE} // garbage
				case 3:
					jpegBytes = []byte{0xFF} // sliver
				}
				log.Printf("BAD FRAME injected pos=%d kind=%d size=%d (cell %s should keep previous frame, NOT go black)",
					pos, rand.Intn(4), len(jpegBytes), st.name)
			} else {
				jpegBytes = renderShot(st.name, st.hue, 1280, 720, frame)
			}

			if err := sendShot(pipe, uint32(pos), jpegBytes); err != nil {
				log.Printf("sendShot pos=%d: %v — exiting", pos, err)
				return
			}
		case <-reorderTicker.C:
			orderMu.Lock()
			rand.Shuffle(len(currentOrder), func(i, j int) {
				currentOrder[i], currentOrder[j] = currentOrder[j], currentOrder[i]
			})
			reordered := make([]student, len(currentOrder))
			for i, oi := range currentOrder {
				reordered[i] = students[oi]
			}
			orderMu.Unlock()
			if err := sendInit(pipe, reordered); err != nil {
				log.Printf("re-init: %v", err)
				return
			}
			log.Printf("REORDERED → %v (screenshots should follow names, not slots)",
				orderNames(reordered))
		case <-offlineTicker.C:
			orderMu.Lock()
			pos := rand.Intn(len(currentOrder))
			orderMu.Unlock()
			if err := sendOffline(pipe, uint32(pos)); err != nil {
				log.Printf("offline: %v", err)
				return
			}
			log.Printf("marked pos=%d offline (cell stays red, image kept)", pos)
		}
	}
}

type student struct {
	name string
	hue  int
}

func orderNames(s []student) []string {
	out := make([]string, len(s))
	for i, st := range s {
		out[i] = st.name
	}
	return out
}

// ── Pipe creation + connect ───────────────────────────────────────────────────

func createPipe() (uintptr, error) {
	namePtr, _ := syscall.UTF16PtrFromString(pipeName)
	h, _, e := procCreateNamedPipe.Call(
		uintptr(unsafe.Pointer(namePtr)),
		pipeAccessDuplex|fileFlagOverlapped,
		uintptr(pipeTypeByte|pipeWait),
		1,
		1<<20, // 1 MB out buffer
		1<<12,
		5000,
		0,
	)
	if h == invalidHandle {
		return 0, e
	}
	return h, nil
}

func waitConnect(pipe uintptr, timeout time.Duration) error {
	op, err := newOp()
	if err != nil {
		return err
	}
	defer op.close()
	op.reset()
	cRet, _, cErr := procConnectNamedPipe.Call(pipe, uintptr(unsafe.Pointer(&op.ov)))
	if cRet != 0 {
		return nil
	}
	if cErr == syscall.Errno(errorPipeConnected) {
		return nil
	}
	if cErr != syscall.Errno(errorIoPending) {
		return fmt.Errorf("ConnectNamedPipe: %w", cErr)
	}
	waitMs := uint32(timeout / time.Millisecond)
	wRet, _, _ := procWaitForSingleObject.Call(op.event, uintptr(waitMs))
	if wRet == waitTimeout {
		procCancelIoEx.Call(pipe, uintptr(unsafe.Pointer(&op.ov)))
		procWaitForSingleObject.Call(op.event, infinite)
		return fmt.Errorf("connect timeout after %v", timeout)
	}
	if wRet != waitObject0 {
		return fmt.Errorf("WaitForSingleObject: 0x%x", wRet)
	}
	return nil
}

// ── Async write helpers ───────────────────────────────────────────────────────

type pipeOp struct {
	ov    syscall.Overlapped
	event uintptr
}

func newOp() (*pipeOp, error) {
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

func (op *pipeOp) reset() {
	op.ov = syscall.Overlapped{HEvent: syscall.Handle(op.event)}
	procResetEvent.Call(op.event)
}

var writeOpOnce sync.Once
var writeOp *pipeOp

func writeAll(handle uintptr, data []byte, timeout time.Duration) error {
	writeOpOnce.Do(func() {
		writeOp, _ = newOp()
	})
	if writeOp == nil {
		return fmt.Errorf("no write op")
	}
	writeOp.reset()
	var written uint32
	ret, _, e := procWriteFile.Call(
		handle,
		uintptr(unsafe.Pointer(&data[0])),
		uintptr(len(data)),
		uintptr(unsafe.Pointer(&written)),
		uintptr(unsafe.Pointer(&writeOp.ov)),
	)
	if ret != 0 {
		return nil
	}
	if e != syscall.Errno(errorIoPending) {
		return fmt.Errorf("WriteFile: %w", e)
	}
	wRet, _, _ := procWaitForSingleObject.Call(writeOp.event, uintptr(timeout/time.Millisecond))
	if wRet == waitTimeout {
		procCancelIoEx.Call(handle, uintptr(unsafe.Pointer(&writeOp.ov)))
		procWaitForSingleObject.Call(writeOp.event, infinite)
		return fmt.Errorf("WriteFile timeout")
	}
	var transferred uint32
	gRet, _, gErr := procGetOverlappedResult.Call(
		handle, uintptr(unsafe.Pointer(&writeOp.ov)),
		uintptr(unsafe.Pointer(&transferred)), 0)
	if gRet == 0 {
		return fmt.Errorf("GetOverlappedResult: %w", gErr)
	}
	return nil
}

func closeHandle(h uintptr) {
	procCloseHandle.Call(h)
}

// ── Frame senders ─────────────────────────────────────────────────────────────

func sendFrame(pipe uintptr, msgType uint32, payload []byte) error {
	frame := make([]byte, 8+len(payload))
	binary.LittleEndian.PutUint32(frame[0:4], msgType)
	binary.LittleEndian.PutUint32(frame[4:8], uint32(len(payload)))
	copy(frame[8:], payload)
	return writeAll(pipe, frame, 5*time.Second)
}

func sendInit(pipe uintptr, students []student) error {
	var payload []byte
	cnt := make([]byte, 4)
	binary.LittleEndian.PutUint32(cnt, uint32(len(students)))
	payload = append(payload, cnt...)
	for _, st := range students {
		nb := []byte(st.name)
		l := make([]byte, 4)
		binary.LittleEndian.PutUint32(l, uint32(len(nb)))
		payload = append(payload, l...)
		payload = append(payload, nb...)
	}
	return sendFrame(pipe, msgInit, payload)
}

func sendShot(pipe uintptr, idx uint32, jpegData []byte) error {
	payload := make([]byte, 8+len(jpegData))
	binary.LittleEndian.PutUint32(payload[0:4], idx)
	binary.LittleEndian.PutUint32(payload[4:8], uint32(len(jpegData)))
	copy(payload[8:], jpegData)
	return sendFrame(pipe, msgShot, payload)
}

func sendOffline(pipe uintptr, idx uint32) error {
	p := make([]byte, 4)
	binary.LittleEndian.PutUint32(p, idx)
	return sendFrame(pipe, msgOffline, p)
}

// ── Synthetic JPEG ────────────────────────────────────────────────────────────

func renderShot(name string, hue, w, h, frame int) []byte {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	r0, g0, b0 := hsv(hue, 0.55, 0.35)
	r1, g1, b1 := hsv((hue+30)%360, 0.55, 0.18)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			t := float64((x+y+frame*8)%(w+h)) / float64(w+h)
			img.SetRGBA(x, y, color.RGBA{
				lerp(r0, r1, t), lerp(g0, g1, t), lerp(b0, b1, t), 255,
			})
		}
	}
	caption := fmt.Sprintf("%s F%d", name, frame)
	scale := w / 40
	if scale < 4 {
		scale = 4
	}
	drawString(img, caption, scale*2, scale*2, scale, color.RGBA{255, 255, 255, 255})
	stamp := time.Now().Format("15:04:05.000")
	drawString(img, stamp, scale*2, h-scale*9, scale, color.RGBA{220, 220, 220, 255})

	var buf bytes.Buffer
	_ = jpeg.Encode(&buf, img, &jpeg.Options{Quality: 65})
	return buf.Bytes()
}

func lerp(a, b uint8, t float64) uint8 {
	return uint8(float64(a)*(1-t) + float64(b)*t)
}

func hsv(h int, s, v float64) (uint8, uint8, uint8) {
	c := v * s
	x := c * (1 - absF(modF(float64(h)/60.0, 2)-1))
	m := v - c
	var rf, gf, bf float64
	switch h / 60 % 6 {
	case 0:
		rf, gf, bf = c, x, 0
	case 1:
		rf, gf, bf = x, c, 0
	case 2:
		rf, gf, bf = 0, c, x
	case 3:
		rf, gf, bf = 0, x, c
	case 4:
		rf, gf, bf = x, 0, c
	case 5:
		rf, gf, bf = c, 0, x
	}
	return uint8((rf + m) * 255), uint8((gf + m) * 255), uint8((bf + m) * 255)
}
func absF(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}
func modF(a, b float64) float64 { return a - b*float64(int(a/b)) }

// 5×7 bitmap font (reused from fakeagent for parity)
var glyphs = map[rune][7]byte{
	' ': {0, 0, 0, 0, 0, 0, 0},
	'#': {0x0A, 0x1F, 0x0A, 0x1F, 0x0A, 0x00, 0x00},
	':': {0x00, 0x04, 0x00, 0x00, 0x04, 0x00, 0x00},
	'.': {0x00, 0x00, 0x00, 0x00, 0x00, 0x04, 0x00},
	'-': {0x00, 0x00, 0x00, 0x0E, 0x00, 0x00, 0x00},
	'0': {0x0E, 0x11, 0x13, 0x15, 0x19, 0x11, 0x0E},
	'1': {0x04, 0x0C, 0x04, 0x04, 0x04, 0x04, 0x0E},
	'2': {0x0E, 0x11, 0x01, 0x02, 0x04, 0x08, 0x1F},
	'3': {0x0E, 0x11, 0x01, 0x06, 0x01, 0x11, 0x0E},
	'4': {0x02, 0x06, 0x0A, 0x12, 0x1F, 0x02, 0x02},
	'5': {0x1F, 0x10, 0x1E, 0x01, 0x01, 0x11, 0x0E},
	'6': {0x06, 0x08, 0x10, 0x1E, 0x11, 0x11, 0x0E},
	'7': {0x1F, 0x01, 0x02, 0x04, 0x08, 0x08, 0x08},
	'8': {0x0E, 0x11, 0x11, 0x0E, 0x11, 0x11, 0x0E},
	'9': {0x0E, 0x11, 0x11, 0x0F, 0x01, 0x02, 0x0C},
	'A': {0x0E, 0x11, 0x11, 0x1F, 0x11, 0x11, 0x11},
	'B': {0x1E, 0x11, 0x11, 0x1E, 0x11, 0x11, 0x1E},
	'C': {0x0E, 0x11, 0x10, 0x10, 0x10, 0x11, 0x0E},
	'D': {0x1E, 0x11, 0x11, 0x11, 0x11, 0x11, 0x1E},
	'E': {0x1F, 0x10, 0x10, 0x1E, 0x10, 0x10, 0x1F},
	'F': {0x1F, 0x10, 0x10, 0x1E, 0x10, 0x10, 0x10},
	'G': {0x0E, 0x11, 0x10, 0x17, 0x11, 0x11, 0x0E},
	'H': {0x11, 0x11, 0x11, 0x1F, 0x11, 0x11, 0x11},
	'I': {0x0E, 0x04, 0x04, 0x04, 0x04, 0x04, 0x0E},
	'L': {0x10, 0x10, 0x10, 0x10, 0x10, 0x10, 0x1F},
	'M': {0x11, 0x1B, 0x15, 0x15, 0x11, 0x11, 0x11},
	'N': {0x11, 0x11, 0x19, 0x15, 0x13, 0x11, 0x11},
	'O': {0x0E, 0x11, 0x11, 0x11, 0x11, 0x11, 0x0E},
	'P': {0x1E, 0x11, 0x11, 0x1E, 0x10, 0x10, 0x10},
	'R': {0x1E, 0x11, 0x11, 0x1E, 0x14, 0x12, 0x11},
	'S': {0x0F, 0x10, 0x10, 0x0E, 0x01, 0x01, 0x1E},
	'T': {0x1F, 0x04, 0x04, 0x04, 0x04, 0x04, 0x04},
}

func drawString(img *image.RGBA, s string, x, y, scale int, c color.RGBA) {
	cx := x
	for _, r := range s {
		if r >= 'a' && r <= 'z' {
			r -= 32
		}
		g, ok := glyphs[r]
		if !ok {
			g = glyphs[' ']
		}
		for row := 0; row < 7; row++ {
			bits := g[row]
			for col := 0; col < 5; col++ {
				if bits&(1<<(4-col)) != 0 {
					for sy := 0; sy < scale; sy++ {
						for sx := 0; sx < scale; sx++ {
							img.SetRGBA(cx+col*scale+sx, y+row*scale+sy, c)
						}
					}
				}
			}
		}
		cx += 6 * scale
	}
}
