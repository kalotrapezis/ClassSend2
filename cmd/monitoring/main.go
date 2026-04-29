// monitoring.exe — ClassSend teacher monitoring window.
//
// Build:
//
//	go build -ldflags="-H windowsgui" -o monitoring.exe ./cmd/monitoring
//
// This program opens a native Win32 window with a grid of cells — one per
// connected student. It connects to the named pipe \\.\pipe\ClassSendMonitor
// created by classsend.exe and receives screenshots one-by-one, displaying
// them as they arrive. Old frames are replaced in-place to keep memory use
// flat.
package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"image"
	"image/draw"
	"image/jpeg"
	"io"
	"math"
	"os"
	"runtime"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

// ── Win32 API bindings ────────────────────────────────────────────────────────

var (
	user32   = syscall.NewLazyDLL("user32.dll")
	gdi32    = syscall.NewLazyDLL("gdi32.dll")
	kernel32 = syscall.NewLazyDLL("kernel32.dll")

	// Window lifecycle
	procRegisterClassExW = user32.NewProc("RegisterClassExW")
	procCreateWindowExW  = user32.NewProc("CreateWindowExW")
	procShowWindow       = user32.NewProc("ShowWindow")
	procDefWindowProcW   = user32.NewProc("DefWindowProcW")
	procPostQuitMessage  = user32.NewProc("PostQuitMessage")
	procGetMessageW      = user32.NewProc("GetMessageW")
	procTranslateMessage = user32.NewProc("TranslateMessage")
	procDispatchMessageW = user32.NewProc("DispatchMessageW")
	procInvalidateRect   = user32.NewProc("InvalidateRect")
	procGetClientRect    = user32.NewProc("GetClientRect")
	procPostMessageW     = user32.NewProc("PostMessageW")
	procGetModuleHandleW = kernel32.NewProc("GetModuleHandleW")
	procGetSystemMetrics = user32.NewProc("GetSystemMetrics")
	procSetWindowTextW   = user32.NewProc("SetWindowTextW")

	// Painting / GDI
	procBeginPaint             = user32.NewProc("BeginPaint")
	procEndPaint               = user32.NewProc("EndPaint")
	procCreateCompatibleDC     = gdi32.NewProc("CreateCompatibleDC")
	procCreateCompatibleBitmap = gdi32.NewProc("CreateCompatibleBitmap")
	procSelectObject           = gdi32.NewProc("SelectObject")
	procDeleteObject           = gdi32.NewProc("DeleteObject")
	procDeleteDC               = gdi32.NewProc("DeleteDC")
	procBitBlt                 = gdi32.NewProc("BitBlt")
	procStretchDIBits          = gdi32.NewProc("StretchDIBits")
	procFillRect               = user32.NewProc("FillRect")
	procCreateSolidBrush       = gdi32.NewProc("CreateSolidBrush")
	procDrawTextW              = user32.NewProc("DrawTextW")
	procSetTextColor           = gdi32.NewProc("SetTextColor")
	procSetBkMode              = gdi32.NewProc("SetBkMode")
	procCreateFontW            = gdi32.NewProc("CreateFontW")
	procSetStretchBltMode      = gdi32.NewProc("SetStretchBltMode")
	procSetBrushOrgEx          = gdi32.NewProc("SetBrushOrgEx")

	// Named pipe (client side)
	procCreateFileW    = kernel32.NewProc("CreateFileW")
	procReadFile       = kernel32.NewProc("ReadFile")
	procCloseHandle    = kernel32.NewProc("CloseHandle")
	procWaitNamedPipe  = kernel32.NewProc("WaitNamedPipeW")
)

// ── Win32 constants ───────────────────────────────────────────────────────────

const (
	wsOverlappedWindow = 0x00CF0000
	wsVisible          = 0x10000000
	csHredraw          = 0x0002
	csVredraw          = 0x0001
	swShow             = 5
	smCxScreen         = 0
	smCyScreen         = 1

	wmDestroy = 0x0002
	wmPaint   = 0x000F
	wmSize    = 0x0005
	wmClose   = 0x0010
	wmUser    = 0x0400
	wmUpdate  = wmUser + 1 // custom: grid data changed — repaint
	wmPipeEOF = wmUser + 2 // custom: pipe closed by classsend

	srcCopy      = 0x00CC0020
	dibRgbColors = 0
	transparent  = 1
	halftone     = 4 // HALFTONE stretch mode for quality downscaling

	dtCenter    = 0x00000001
	dtVcenter   = 0x00000004
	dtSingleline = 0x00000020
	dtWordBreak  = 0x00000010
	dtLeft       = 0x00000000

	genericRead     = 0x80000000
	openExisting    = 3
	fileAttrNormal  = 0x00000080
	invalidHandle   = ^uintptr(0)

	// Pipe protocol types — must match session_windows.go
	msgInit    uint32 = 1
	msgShot    uint32 = 2
	msgOffline uint32 = 3
	msgStop    uint32 = 4
)

// ── Win32 structures ──────────────────────────────────────────────────────────

type wndClassExW struct {
	cbSize        uint32
	style         uint32
	lpfnWndProc   uintptr
	cbClsExtra    int32
	cbWndExtra    int32
	hInstance     uintptr
	hIcon         uintptr
	hCursor       uintptr
	hbrBackground uintptr
	lpszMenuName  *uint16
	lpszClassName *uint16
	hIconSm       uintptr
}

type paintStruct struct {
	hdc         uintptr
	fErase      int32
	rcPaint     winRect
	fRestore    int32
	fIncUpdate  int32
	rgbReserved [32]byte
}

type winRect struct {
	Left, Top, Right, Bottom int32
}

type winMsg struct {
	hwnd    uintptr
	message uint32
	wParam  uintptr
	lParam  uintptr
	time    uint32
	pt      [2]int32
}

type bitmapInfoHeader struct {
	biSize          uint32
	biWidth         int32
	biHeight        int32
	biPlanes        uint16
	biBitCount      uint16
	biCompression   uint32
	biSizeImage     uint32
	biXPelsPerMeter int32
	biYPelsPerMeter int32
	biClrUsed       uint32
	biClrImportant  uint32
}

type bitmapInfo struct {
	bmiHeader bitmapInfoHeader
	bmiColors [1]uint32
}

// ── Grid state ────────────────────────────────────────────────────────────────

type cellState struct {
	name    string
	pixels  []byte // BGRA top-down, len = imgW*imgH*4; nil = no screenshot yet
	imgW    int32
	imgH    int32
	offline bool
}

var (
	gridMu   sync.RWMutex
	cells    []cellState
	mainHwnd uintptr

	wndProcCallback uintptr
)

func init() {
	wndProcCallback = syscall.NewCallback(monitorWndProc)
}

// ── Window procedure ──────────────────────────────────────────────────────────

func monitorWndProc(hwnd, msg, wParam, lParam uintptr) uintptr {
	switch msg {
	case wmPaint:
		paintGrid(hwnd)
		return 0
	case wmSize:
		procInvalidateRect.Call(hwnd, 0, 1)
		return 0
	case wmUpdate:
		procInvalidateRect.Call(hwnd, 0, 0)
		return 0
	case wmPipeEOF:
		// classsend closed the pipe (monitoring stopped or crashed)
		procSetWindowTextW.Call(hwnd,
			uintptr(unsafe.Pointer(utf16("ClassSend - Παρακολούθηση (Εκτός Σύνδεσης)"))))
		procInvalidateRect.Call(hwnd, 0, 0)
		return 0
	case wmClose, wmDestroy:
		procPostQuitMessage.Call(0)
		return 0
	}
	r, _, _ := procDefWindowProcW.Call(hwnd, msg, wParam, lParam)
	return r
}

// ── Painting ──────────────────────────────────────────────────────────────────

const nameBarH = int32(24)

func paintGrid(hwnd uintptr) {
	var ps paintStruct
	hdc, _, _ := procBeginPaint.Call(hwnd, uintptr(unsafe.Pointer(&ps)))
	defer procEndPaint.Call(hwnd, uintptr(unsafe.Pointer(&ps)))

	var rc winRect
	procGetClientRect.Call(hwnd, uintptr(unsafe.Pointer(&rc)))
	winW := rc.Right
	winH := rc.Bottom

	// Double-buffer: draw everything into a memory DC then BitBlt to screen
	hdcMem, _, _ := procCreateCompatibleDC.Call(hdc)
	hBmp, _, _ := procCreateCompatibleBitmap.Call(hdc, uintptr(winW), uintptr(winH))
	oldBmp, _, _ := procSelectObject.Call(hdcMem, hBmp)
	defer func() {
		procBitBlt.Call(hdc, 0, 0, uintptr(winW), uintptr(winH), hdcMem, 0, 0, srcCopy)
		procSelectObject.Call(hdcMem, oldBmp)
		procDeleteObject.Call(hBmp)
		procDeleteDC.Call(hdcMem)
	}()

	// Background
	bgBrush, _, _ := procCreateSolidBrush.Call(0x00111111)
	fullRc := winRect{0, 0, winW, winH}
	procFillRect.Call(hdcMem, uintptr(unsafe.Pointer(&fullRc)), bgBrush)
	procDeleteObject.Call(bgBrush)

	gridMu.RLock()
	snapshot := make([]cellState, len(cells))
	copy(snapshot, cells)
	gridMu.RUnlock()

	n := len(snapshot)
	if n == 0 {
		procSetBkMode.Call(hdcMem, uintptr(transparent))
		procSetTextColor.Call(hdcMem, 0x00666666)
		msg := utf16("Αναμονή σύνδεσης μαθητών...")
		textRc := winRect{0, 0, winW, winH}
		procDrawTextW.Call(hdcMem, uintptr(unsafe.Pointer(msg)), ^uintptr(0),
			uintptr(unsafe.Pointer(&textRc)), dtCenter|dtVcenter|dtSingleline)
		return
	}

	cols := int32(math.Ceil(math.Sqrt(float64(n))))
	rows := (int32(n) + cols - 1) / cols
	cellW := winW / cols
	cellH := winH / rows

	// Load shared font for student name labels
	hFont, _, _ := procCreateFontW.Call(
		i32(-14), 0, 0, 0,
		600, 0, 0, 0,
		1, 0, 0, 4, 0,
		uintptr(unsafe.Pointer(utf16("Segoe UI"))),
	)
	origFont, _, _ := procSelectObject.Call(hdcMem, hFont)
	defer func() {
		procSelectObject.Call(hdcMem, origFont)
		procDeleteObject.Call(hFont)
	}()

	// HALFTONE for quality downscaling
	procSetStretchBltMode.Call(hdcMem, halftone)
	procSetBrushOrgEx.Call(hdcMem, 0, 0, 0)

	const pad = int32(2)
	for idx, cell := range snapshot {
		col := int32(idx) % cols
		row := int32(idx) / cols
		x := col*cellW + pad
		y := row*cellH + pad
		cw := cellW - pad*2
		ch := cellH - pad*2
		paintCell(hdcMem, x, y, cw, ch, &cell)
	}
}

func paintCell(hdc uintptr, x, y, w, h int32, cell *cellState) {
	// Cell background
	bgColor := uintptr(0x00181818)
	if cell.offline {
		bgColor = 0x001A0000
	}
	bg, _, _ := procCreateSolidBrush.Call(bgColor)
	cellRc := winRect{x, y, x + w, y + h}
	procFillRect.Call(hdc, uintptr(unsafe.Pointer(&cellRc)), bg)
	procDeleteObject.Call(bg)

	// Name bar background
	nameBg, _, _ := procCreateSolidBrush.Call(0x00252525)
	nameRc := winRect{x, y, x + w, y + nameBarH}
	procFillRect.Call(hdc, uintptr(unsafe.Pointer(&nameRc)), nameBg)
	procDeleteObject.Call(nameBg)

	// Name text
	procSetBkMode.Call(hdc, uintptr(transparent))
	if cell.offline {
		procSetTextColor.Call(hdc, 0x004444AA)
	} else {
		procSetTextColor.Call(hdc, 0x00DDDDDD)
	}
	namePtr := utf16(cell.name)
	nameTxtRc := winRect{x + 4, y + 4, x + w - 4, y + nameBarH - 2}
	procDrawTextW.Call(hdc, uintptr(unsafe.Pointer(namePtr)), ^uintptr(0),
		uintptr(unsafe.Pointer(&nameTxtRc)), dtSingleline|dtLeft|dtVcenter)

	imgArea := winRect{x, y + nameBarH, x + w, y + h}

	if len(cell.pixels) > 0 {
		bmi := bitmapInfo{
			bmiHeader: bitmapInfoHeader{
				biSize:     40,
				biWidth:    cell.imgW,
				biHeight:   -cell.imgH, // negative → top-down
				biPlanes:   1,
				biBitCount: 32,
			},
		}
		procStretchDIBits.Call(
			hdc,
			uintptr(imgArea.Left), uintptr(imgArea.Top),
			uintptr(imgArea.Right-imgArea.Left),
			uintptr(imgArea.Bottom-imgArea.Top),
			0, 0,
			uintptr(cell.imgW), uintptr(cell.imgH),
			uintptr(unsafe.Pointer(&cell.pixels[0])),
			uintptr(unsafe.Pointer(&bmi)),
			dibRgbColors,
			srcCopy,
		)
	} else {
		// No screenshot yet — show status text
		status := "Αναμονή..."
		if cell.offline {
			status = "Εκτός Σύνδεσης"
		}
		procSetTextColor.Call(hdc, 0x00555555)
		statusPtr := utf16(status)
		procDrawTextW.Call(hdc, uintptr(unsafe.Pointer(statusPtr)), ^uintptr(0),
			uintptr(unsafe.Pointer(&imgArea)), dtCenter|dtVcenter|dtSingleline)
	}

	// Separator line (bottom + right edge of cell)
	sepBrush, _, _ := procCreateSolidBrush.Call(0x00333333)
	botLine := winRect{x, y + h, x + w, y + h + 1}
	procFillRect.Call(hdc, uintptr(unsafe.Pointer(&botLine)), sepBrush)
	rightLine := winRect{x + w, y, x + w + 1, y + h}
	procFillRect.Call(hdc, uintptr(unsafe.Pointer(&rightLine)), sepBrush)
	procDeleteObject.Call(sepBrush)
}

// ── Pipe protocol helpers ─────────────────────────────────────────────────────

type pipeReader struct{ handle uintptr }

func (r *pipeReader) Read(buf []byte) (int, error) {
	if len(buf) == 0 {
		return 0, nil
	}
	var nRead uint32
	ret, _, err := procReadFile.Call(
		r.handle,
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(len(buf)),
		uintptr(unsafe.Pointer(&nRead)),
		0,
	)
	if ret == 0 {
		return 0, fmt.Errorf("ReadFile: %w", err)
	}
	if nRead == 0 {
		return 0, io.EOF
	}
	return int(nRead), nil
}

// connectPipe retries CreateFile on the pipe for up to 15 seconds.
func connectPipe() (uintptr, error) {
	namePtr, _ := syscall.UTF16PtrFromString(`\\.\pipe\ClassSendMonitor`)
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		h, _, _ := procCreateFileW.Call(
			uintptr(unsafe.Pointer(namePtr)),
			genericRead,
			0, 0,
			openExisting,
			fileAttrNormal,
			0,
		)
		if h != invalidHandle {
			return h, nil
		}
		// Wait up to 1 s for the pipe to become available
		procWaitNamedPipe.Call(uintptr(unsafe.Pointer(namePtr)), 1000)
		time.Sleep(100 * time.Millisecond)
	}
	return 0, fmt.Errorf("pipe not available after 15 s")
}

// readPipe reads messages from the pipe and updates the grid state.
// Runs in its own goroutine; posts wmUpdate or wmPipeEOF to hwnd.
func readPipe(handle, hwnd uintptr) {
	r := &pipeReader{handle: handle}
	hdr := make([]byte, 8)

	for {
		if _, err := io.ReadFull(r, hdr); err != nil {
			procPostMessageW.Call(hwnd, wmPipeEOF, 0, 0)
			return
		}
		msgType := binary.LittleEndian.Uint32(hdr[0:4])
		payLen := binary.LittleEndian.Uint32(hdr[4:8])

		var payload []byte
		if payLen > 0 {
			payload = make([]byte, payLen)
			if _, err := io.ReadFull(r, payload); err != nil {
				procPostMessageW.Call(hwnd, wmPipeEOF, 0, 0)
				return
			}
		}

		switch msgType {
		case msgInit:
			handleInit(payload)
		case msgShot:
			handleShot(payload)
		case msgOffline:
			handleOffline(payload)
		case msgStop:
			procPostMessageW.Call(hwnd, wmClose, 0, 0)
			return
		}

		procPostMessageW.Call(hwnd, wmUpdate, 0, 0)
	}
}

func handleInit(payload []byte) {
	if len(payload) < 4 {
		return
	}
	count := binary.LittleEndian.Uint32(payload[0:4])
	offset := 4
	names := make([]string, 0, count)
	for i := uint32(0); i < count; i++ {
		if offset+4 > len(payload) {
			break
		}
		nameLen := int(binary.LittleEndian.Uint32(payload[offset : offset+4]))
		offset += 4
		if offset+nameLen > len(payload) {
			break
		}
		names = append(names, string(payload[offset:offset+nameLen]))
		offset += nameLen
	}

	gridMu.Lock()
	defer gridMu.Unlock()
	newCells := make([]cellState, len(names))
	for i, name := range names {
		// Preserve existing screenshot if the student is in the same slot
		if i < len(cells) && cells[i].name == name {
			newCells[i] = cells[i]
		} else {
			newCells[i] = cellState{name: name}
		}
	}
	cells = newCells
}

func handleShot(payload []byte) {
	if len(payload) < 8 {
		return
	}
	idx := binary.LittleEndian.Uint32(payload[0:4])
	jpegLen := int(binary.LittleEndian.Uint32(payload[4:8]))
	if 8+jpegLen > len(payload) {
		return
	}
	jpegData := payload[8 : 8+jpegLen]

	// Decode JPEG → RGBA → BGRA (Windows DIB)
	img, err := jpeg.Decode(bytes.NewReader(jpegData))
	if err != nil {
		return
	}
	bounds := img.Bounds()
	imgW := bounds.Dx()
	imgH := bounds.Dy()

	// Convert to *image.RGBA efficiently (handles YCbCr, etc.)
	rgba := image.NewRGBA(image.Rect(0, 0, imgW, imgH))
	draw.Draw(rgba, rgba.Bounds(), img, bounds.Min, draw.Src)

	// Swap R↔B to get BGRA (Windows DIB format)
	bgra := make([]byte, imgW*imgH*4)
	for i := 0; i < imgW*imgH; i++ {
		bgra[i*4+0] = rgba.Pix[i*4+2] // B
		bgra[i*4+1] = rgba.Pix[i*4+1] // G
		bgra[i*4+2] = rgba.Pix[i*4+0] // R
		bgra[i*4+3] = 0                // reserved
	}

	gridMu.Lock()
	defer gridMu.Unlock()
	if int(idx) < len(cells) {
		cells[idx].pixels = bgra // old slice is GC-eligible immediately
		cells[idx].imgW = int32(imgW)
		cells[idx].imgH = int32(imgH)
		cells[idx].offline = false
	}
}

func handleOffline(payload []byte) {
	if len(payload) < 4 {
		return
	}
	idx := binary.LittleEndian.Uint32(payload[0:4])
	gridMu.Lock()
	defer gridMu.Unlock()
	if int(idx) < len(cells) {
		cells[idx].offline = true
	}
}

// ── Helpers ───────────────────────────────────────────────────────────────────

func utf16(s string) *uint16 {
	p, _ := syscall.UTF16PtrFromString(s)
	return p
}

func i32(n int32) uintptr { return uintptr(uint32(n)) }

// ── Main ──────────────────────────────────────────────────────────────────────

func main() {
	runtime.LockOSThread()

	hInst, _, _ := procGetModuleHandleW.Call(0)
	className := utf16("ClassSendMonitor")

	wc := wndClassExW{
		cbSize:        uint32(unsafe.Sizeof(wndClassExW{})),
		style:         csHredraw | csVredraw,
		lpfnWndProc:   wndProcCallback,
		hInstance:     hInst,
		hbrBackground: 0, // painted in WM_PAINT
		lpszClassName: className,
	}
	if r, _, _ := procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc))); r == 0 {
		fmt.Fprintln(os.Stderr, "RegisterClassEx failed")
		os.Exit(1)
	}

	sw, _, _ := procGetSystemMetrics.Call(smCxScreen)
	sh, _, _ := procGetSystemMetrics.Call(smCyScreen)
	winW := sw * 3 / 4
	winH := sh * 3 / 4
	winX := (sw - winW) / 2
	winY := (sh - winH) / 2

	hwnd, _, _ := procCreateWindowExW.Call(
		0,
		uintptr(unsafe.Pointer(className)),
		uintptr(unsafe.Pointer(utf16("ClassSend - Παρακολούθηση Τάξης"))),
		wsOverlappedWindow|wsVisible,
		winX, winY, winW, winH,
		0, 0, hInst, 0,
	)
	if hwnd == 0 {
		fmt.Fprintln(os.Stderr, "CreateWindow failed")
		os.Exit(1)
	}
	mainHwnd = hwnd
	procShowWindow.Call(hwnd, swShow)

	// Connect to the classsend pipe in the background
	go func() {
		pipe, err := connectPipe()
		if err != nil {
			procPostMessageW.Call(hwnd, wmPipeEOF, 0, 0)
			return
		}
		readPipe(pipe, hwnd)
		procCloseHandle.Call(pipe)
	}()

	// Win32 message loop
	var msg winMsg
	for {
		r, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&msg)), 0, 0, 0)
		if r == 0 || r == ^uintptr(0) {
			break
		}
		procTranslateMessage.Call(uintptr(unsafe.Pointer(&msg)))
		procDispatchMessageW.Call(uintptr(unsafe.Pointer(&msg)))
	}
}
