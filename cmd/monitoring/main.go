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
	"runtime/debug"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"classsend/internal/buildinfo"
	"classsend/internal/devlog"
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
	procLoadCursorW      = user32.NewProc("LoadCursorW")
	procSetWindowPos     = user32.NewProc("SetWindowPos")
	procGetWindowLongW   = user32.NewProc("GetWindowLongW")
	procSetWindowLongW   = user32.NewProc("SetWindowLongW")
	procGetWindowRect    = user32.NewProc("GetWindowRect")
	procGetKeyState      = user32.NewProc("GetKeyState")

	// Painting / GDI
	procBeginPaint             = user32.NewProc("BeginPaint")
	procEndPaint               = user32.NewProc("EndPaint")
	procCreateCompatibleDC     = gdi32.NewProc("CreateCompatibleDC")
	procCreateCompatibleBitmap = gdi32.NewProc("CreateCompatibleBitmap")
	procSelectObject           = gdi32.NewProc("SelectObject")
	procDeleteObject           = gdi32.NewProc("DeleteObject")
	procDeleteDC               = gdi32.NewProc("DeleteDC")
	procBitBlt                 = gdi32.NewProc("BitBlt")
	procSetDIBits              = gdi32.NewProc("SetDIBits")
	procSetDIBitsToDevice      = gdi32.NewProc("SetDIBitsToDevice")
	procStretchBlt             = gdi32.NewProc("StretchBlt")
	procStretchDIBits          = gdi32.NewProc("StretchDIBits")
	procFillRect               = user32.NewProc("FillRect")
	procCreateSolidBrush       = gdi32.NewProc("CreateSolidBrush")
	procDrawTextW              = user32.NewProc("DrawTextW")
	procSetTextColor           = gdi32.NewProc("SetTextColor")
	procSetBkMode              = gdi32.NewProc("SetBkMode")
	procCreateFontW            = gdi32.NewProc("CreateFontW")
	procSetStretchBltMode      = gdi32.NewProc("SetStretchBltMode")
	procSetBrushOrgEx          = gdi32.NewProc("SetBrushOrgEx")

	// Named pipe (client side) — overlapped I/O on the data path so a wedged
	// read or write can be cancelled instead of freezing the whole process.
	procCreateFileW         = kernel32.NewProc("CreateFileW")
	procReadFile            = kernel32.NewProc("ReadFile")
	procWriteFile           = kernel32.NewProc("WriteFile")
	procCloseHandle         = kernel32.NewProc("CloseHandle")
	procWaitNamedPipe       = kernel32.NewProc("WaitNamedPipeW")
	procCreateEventW        = kernel32.NewProc("CreateEventW")
	procWaitForSingleObject = kernel32.NewProc("WaitForSingleObject")
	procCancelIoEx          = kernel32.NewProc("CancelIoEx")
	procGetOverlappedResult = kernel32.NewProc("GetOverlappedResult")
	procResetEvent          = kernel32.NewProc("ResetEvent")
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

	wmDestroy      = 0x0002
	wmPaint        = 0x000F
	wmSize         = 0x0005
	wmClose        = 0x0010
	wmKeydown      = 0x0100
	wmLbuttondown  = 0x0201
	wmUser         = 0x0400
	wmUpdate       = wmUser + 1 // custom: grid data changed — repaint
	wmPipeEOF      = wmUser + 2 // custom: pipe closed by classsend

	vkEscape  = 0x1B
	vkControl = 0x11
	vkF       = 0x46
	vkT       = 0x54
	vkW       = 0x57

	// SetWindowPos flags
	swpNoSize     = 0x0001
	swpNoMove     = 0x0002
	swpNoZorder   = 0x0004
	swpFrameChanged = 0x0020
	swpShowWindow = 0x0040

	// HWND sentinel values used by SetWindowPos. The Win32 docs cast these
	// to HWND; in Go uintptr arithmetic, -1 = ^uintptr(0), -2 = ^uintptr(1).
	hwndTopmost   = ^uintptr(0) // HWND_TOPMOST = (HWND)-1
	hwndNoTopmost = ^uintptr(1) // HWND_NOTOPMOST = (HWND)-2

	wsPopup      = 0x80000000
	wsCaption    = 0x00C00000
	wsThickFrame = 0x00040000

	// Pipe protocol — back-channel (monitoring → classsend)
	msgFocus       uint32 = 5
	msgStopRequest uint32 = 6 // monitoring → teacher: ^W in window; clean up session

	// Sentinel: leave focus mode and resume the grid round-robin.
	focusUnset uint32 = 0xFFFFFFFF

	srcCopy      = 0x00CC0020
	dibRgbColors = 0
	transparent  = 1
	// HALFTONE (4) silently returns 0 for every StretchDIBits after the
	// first when the destination is a memory DC with non-integer scaling
	// — observed on real hardware, confirmed in logs. COLORONCOLOR (3) is
	// the safe choice; quality difference is invisible at thumbnail size.
	colorOnColor = 3

	dtCenter    = 0x00000001
	dtVcenter   = 0x00000004
	dtSingleline = 0x00000020
	dtWordBreak  = 0x00000010
	dtLeft       = 0x00000000

	genericRead        = 0x80000000
	genericWrite       = 0x40000000
	openExisting       = 3
	fileAttrNormal     = 0x00000080
	fileFlagOverlapped = 0x40000000
	invalidHandle      = ^uintptr(0)

	errorIoPending = 997
	waitObject0    = 0x00000000
	waitTimeout    = 0x00000102
	infinite       = 0xFFFFFFFF

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

	// Pipe handle for the back-channel (monitoring → classsend) — set once
	// the readPipe goroutine has CreateFile'd it. Click handlers use it to
	// send MsgFocus events.
	pipeMu      sync.Mutex
	pipeHandle  uintptr

	// Focus state: -1 = grid mode, otherwise index of the focused cell.
	focusMu  sync.RWMutex
	focusIdx int = -1
)

func init() {
	wndProcCallback = syscall.NewCallback(monitorWndProc)
}

// ── Window shortcuts: state + helpers ─────────────────────────────────────────
//
// ^F = toggle fullscreen, ^T = toggle always-on-top, ^W = stop monitoring,
// Esc = leave focus mode OR close window if not focused. Shortcuts fire when
// the monitoring window has keyboard focus; they're not registered as global
// system hotkeys (would conflict with the teacher TUI's existing ^F / ^T / ^W
// bindings on the terminal side).

var (
	// Saved style + position so fullscreen toggle can restore.
	preFullscreenStyle int32
	preFullscreenRect  winRect
	isFullscreen       bool
	isTopmost          bool
)

func isCtrlDown() bool {
	r, _, _ := procGetKeyState.Call(uintptr(vkControl))
	// GetKeyState returns a SHORT; high bit set ⇒ key is down. Mask the low
	// 16 bits and test against 0x8000 to dodge sign-bit casting gymnastics.
	return (uint16(r) & 0x8000) != 0
}

// toggleFullscreen swaps between borderless-fullscreen and the previous
// windowed size/style. Standard Win32 fullscreen pattern: drop WS_CAPTION +
// WS_THICKFRAME, snap to monitor extents, and reverse on exit.
func toggleFullscreen(hwnd uintptr) {
	// GWL_STYLE = -16. Held in an int32 variable (not a const) because Go
	// refuses to evaluate uint32(-16) at compile time; the runtime cast
	// produces the correct two's-complement bit pattern.
	var gwlStyleI int32 = -16
	gwlStyleArg := uintptr(uint32(gwlStyleI))
	if !isFullscreen {
		styleR, _, _ := procGetWindowLongW.Call(hwnd, gwlStyleArg)
		preFullscreenStyle = int32(styleR)
		procGetWindowRect.Call(hwnd, uintptr(unsafe.Pointer(&preFullscreenRect)))

		newStyle := preFullscreenStyle &^ int32(wsCaption|wsThickFrame)
		procSetWindowLongW.Call(hwnd, gwlStyleArg, uintptr(uint32(newStyle)))
		sw, _, _ := procGetSystemMetrics.Call(smCxScreen)
		sh, _, _ := procGetSystemMetrics.Call(smCyScreen)
		procSetWindowPos.Call(hwnd, 0, 0, 0, sw, sh, uintptr(swpNoZorder|swpFrameChanged|swpShowWindow))
		isFullscreen = true
		devlog.Logf("shortcut: fullscreen ON  %dx%d", sw, sh)
		return
	}
	procSetWindowLongW.Call(hwnd, gwlStyleArg, uintptr(uint32(preFullscreenStyle)))
	r := preFullscreenRect
	procSetWindowPos.Call(hwnd, 0,
		uintptr(r.Left), uintptr(r.Top),
		uintptr(r.Right-r.Left), uintptr(r.Bottom-r.Top),
		uintptr(swpNoZorder|swpFrameChanged|swpShowWindow))
	isFullscreen = false
	devlog.Logf("shortcut: fullscreen OFF")
}

// toggleTopmost flips the WS_EX_TOPMOST extended style via SetWindowPos.
func toggleTopmost(hwnd uintptr) {
	hwndZ := hwndTopmost
	if isTopmost {
		hwndZ = hwndNoTopmost
	}
	procSetWindowPos.Call(hwnd, hwndZ, 0, 0, 0, 0, uintptr(swpNoMove|swpNoSize))
	isTopmost = !isTopmost
	devlog.Logf("shortcut: always-on-top = %v", isTopmost)
}

// ── Window procedure ──────────────────────────────────────────────────────────

func monitorWndProc(hwnd, msg, wParam, lParam uintptr) (ret uintptr) {
	defer func() {
		if r := recover(); r != nil {
			devlog.Logf("wndProc PANIC msg=0x%x: %v\n%s", msg, r, debug.Stack())
			ret = 0
		}
	}()
	switch msg {
	case wmPaint:
		paintGrid(hwnd)
		return 0
	case wmSize:
		procInvalidateRect.Call(hwnd, 0, 0)
		return 0
	case wmUpdate:
		procInvalidateRect.Call(hwnd, 0, 0)
		return 0
	case wmLbuttondown:
		// Hit-test: which cell did we click? Toggle focus.
		x := int32(int16(lParam & 0xFFFF))
		y := int32(int16((lParam >> 16) & 0xFFFF))
		idx := hitTestCell(hwnd, x, y)
		if idx < 0 {
			return 0
		}
		focusMu.Lock()
		var send uint32
		if focusIdx == idx {
			focusIdx = -1
			send = focusUnset
		} else {
			focusIdx = idx
			send = uint32(idx)
		}
		focusMu.Unlock()
		sendFocusBackChannel(send)
		procInvalidateRect.Call(hwnd, 0, 0)
		return 0
	case wmKeydown:
		// Window-level shortcuts. Ctrl+F fullscreen, Ctrl+T always-on-top,
		// Ctrl+W stop monitoring (closes window — teacher detects pipe EOF
		// and cleans up the session), Esc exits focus mode if focused, else
		// closes the window.
		switch wParam {
		case vkEscape:
			focusMu.Lock()
			wasFocused := focusIdx >= 0
			focusIdx = -1
			focusMu.Unlock()
			if wasFocused {
				sendFocusBackChannel(focusUnset)
				procInvalidateRect.Call(hwnd, 0, 0)
				return 0
			}
			// Not in focus mode — Esc closes the window.
			procPostMessageW.Call(hwnd, uintptr(wmClose), 0, 0)
			return 0
		case vkF:
			if isCtrlDown() {
				toggleFullscreen(hwnd)
			}
			return 0
		case vkT:
			if isCtrlDown() {
				toggleTopmost(hwnd)
			}
			return 0
		case vkW:
			if isCtrlDown() {
				devlog.Logf("shortcut: ^W stop monitoring")
				sendStopRequestBackChannel()
				procPostMessageW.Call(hwnd, uintptr(wmClose), 0, 0)
			}
			return 0
		}
		return 0
	case wmPipeEOF:
		// classsend closed the pipe (monitoring stopped or crashed)
		procSetWindowTextW.Call(hwnd,
			uintptr(unsafe.Pointer(utf16("ClassSend - Παρακολούθηση (Εκτός Σύνδεσης)  ["+buildinfo.String()+"]"))))
		procInvalidateRect.Call(hwnd, 0, 0)
		return 0
	case wmClose, wmDestroy:
		procPostQuitMessage.Call(0)
		return 0
	}
	r, _, _ := procDefWindowProcW.Call(hwnd, msg, wParam, lParam)
	return r
}

// hitTestCell returns the cell index at window coords (x,y), or -1 if the
// click was on padding or outside the grid.
func hitTestCell(hwnd uintptr, x, y int32) int {
	var rc winRect
	procGetClientRect.Call(hwnd, uintptr(unsafe.Pointer(&rc)))
	winW, winH := rc.Right, rc.Bottom

	gridMu.RLock()
	n := len(cells)
	gridMu.RUnlock()
	if n == 0 || winW <= 0 || winH <= 0 {
		return -1
	}

	cols := int32(math.Ceil(math.Sqrt(float64(n))))
	rows := (int32(n) + cols - 1) / cols
	cellW := winW / cols
	cellH := winH / rows
	if cellW <= 0 || cellH <= 0 {
		return -1
	}
	col := x / cellW
	row := y / cellH
	idx := int(row*cols + col)
	if idx < 0 || idx >= n {
		return -1
	}
	return idx
}

// sendFocusBackChannel writes a MsgFocus message to the duplex pipe so the
// teacher's session loop can switch between grid and focus polling. Runs on
// the UI thread (called from wmLbuttondown), so it MUST NOT block the
// message pump — overlapped I/O with a 2 s timeout guarantees that.
func sendFocusBackChannel(idx uint32) {
	pipeMu.Lock()
	h := pipeHandle
	pipeMu.Unlock()
	if h == 0 {
		devlog.Logf("focus click: no pipe handle yet, dropping")
		return
	}
	frame := make([]byte, 12)
	binary.LittleEndian.PutUint32(frame[0:4], msgFocus)
	binary.LittleEndian.PutUint32(frame[4:8], 4)
	binary.LittleEndian.PutUint32(frame[8:12], idx)
	op, err := newPipeOp()
	if err != nil {
		devlog.Logf("focus click: newPipeOp: %v", err)
		return
	}
	defer op.close()
	if err := pipeWriteAll(h, op, frame, 2*time.Second); err != nil {
		devlog.Logf("focus click: write failed: %v", err)
		return
	}
	if idx == focusUnset {
		devlog.Logf("focus click: sent UNFOCUS")
	} else {
		devlog.Logf("focus click: sent FOCUS idx=%d", idx)
	}
}

// sendStopRequestBackChannel tells the teacher the user pressed ^W in the
// monitoring window. Teacher receives this and closes the session cleanly —
// otherwise the session would keep polling students for shots even though no
// monitoring.exe is alive to display them.
func sendStopRequestBackChannel() {
	pipeMu.Lock()
	h := pipeHandle
	pipeMu.Unlock()
	if h == 0 {
		return
	}
	frame := make([]byte, 8)
	binary.LittleEndian.PutUint32(frame[0:4], msgStopRequest)
	binary.LittleEndian.PutUint32(frame[4:8], 0)
	op, err := newPipeOp()
	if err != nil {
		devlog.Logf("^W: newPipeOp: %v", err)
		return
	}
	defer op.close()
	if err := pipeWriteAll(h, op, frame, 2*time.Second); err != nil {
		devlog.Logf("^W: write failed: %v", err)
		return
	}
	devlog.Logf("^W: sent stop request to teacher")
}

// ── Painting ──────────────────────────────────────────────────────────────────

// Back-buffer state, cached across paints. Recreating the memory DC and
// compatible bitmap on every WM_PAINT (50+ Hz during a window resize) was
// trashing GDI state — SetDIBitsToDevice/StretchDIBits returned 0 on
// random frames. Caching them eliminates the churn.
var (
	bbHdc    uintptr
	bbBmp    uintptr
	bbOldBmp uintptr
	bbW      int32
	bbH      int32
)

// ensureBackBuffer (re)creates the cached memory DC + bitmap if the window
// size changed. Called from paintGrid; safe to call repeatedly.
func ensureBackBuffer(hdc uintptr, w, h int32) {
	if bbHdc != 0 && bbW == w && bbH == h {
		return
	}
	if bbHdc != 0 {
		procSelectObject.Call(bbHdc, bbOldBmp)
		procDeleteObject.Call(bbBmp)
		procDeleteDC.Call(bbHdc)
		bbHdc, bbBmp, bbOldBmp = 0, 0, 0
	}
	bbHdc, _, _ = procCreateCompatibleDC.Call(hdc)
	bbBmp, _, _ = procCreateCompatibleBitmap.Call(hdc, uintptr(w), uintptr(h))
	bbOldBmp, _, _ = procSelectObject.Call(bbHdc, bbBmp)
	bbW, bbH = w, h
}

func paintGrid(hwnd uintptr) {
	var ps paintStruct
	hdc, _, _ := procBeginPaint.Call(hwnd, uintptr(unsafe.Pointer(&ps)))
	defer procEndPaint.Call(hwnd, uintptr(unsafe.Pointer(&ps)))
	devlog.Logf("paintGrid  cells=%d", func() int { gridMu.RLock(); defer gridMu.RUnlock(); return len(cells) }())

	var rc winRect
	procGetClientRect.Call(hwnd, uintptr(unsafe.Pointer(&rc)))
	winW := rc.Right
	winH := rc.Bottom

	// Double-buffer with a CACHED memory DC + bitmap. Only recreated on size
	// change. The final defer just blits the back buffer to the screen.
	ensureBackBuffer(hdc, winW, winH)
	hdcMem := bbHdc
	defer func() {
		procBitBlt.Call(hdc, 0, 0, uintptr(winW), uintptr(winH), hdcMem, 0, 0, srcCopy)
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

	focusMu.RLock()
	curFocus := focusIdx
	focusMu.RUnlock()

	// Focus mode: paint just the focused cell, full window.
	if curFocus >= 0 && curFocus < len(snapshot) {
		hFont, _, _ := procCreateFontW.Call(
			i32(-14), 0, 0, 0,
			600, 0, 0, 0,
			1, 0, 0, 4, 0,
			uintptr(unsafe.Pointer(utf16("Segoe UI"))),
		)
		origFont, _, _ := procSelectObject.Call(hdcMem, hFont)
		procSetStretchBltMode.Call(hdcMem, uintptr(colorOnColor))
		paintCell(hdcMem, 0, 0, winW, winH, &snapshot[curFocus])

		// Top-right exit hint badge so the user knows how to leave focus mode.
		hint := utf16("  Esc ή κλικ για έξοδο  ")
		const hintW, hintH = int32(220), int32(28)
		hintRc := winRect{winW - hintW - 12, 12, winW - 12, 12 + hintH}
		hintBg, _, _ := procCreateSolidBrush.Call(0x00CC2222) // BGR red
		procFillRect.Call(hdcMem, uintptr(unsafe.Pointer(&hintRc)), hintBg)
		procDeleteObject.Call(hintBg)
		procSetBkMode.Call(hdcMem, uintptr(transparent))
		procSetTextColor.Call(hdcMem, 0x00FFFFFF)
		procDrawTextW.Call(hdcMem, uintptr(unsafe.Pointer(hint)), ^uintptr(0),
			uintptr(unsafe.Pointer(&hintRc)), dtCenter|dtVcenter|dtSingleline)

		procSelectObject.Call(hdcMem, origFont)
		procDeleteObject.Call(hFont)
		return
	}

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

	// COLORONCOLOR — reliable across drivers, no brush-origin requirement.
	procSetStretchBltMode.Call(hdcMem, uintptr(colorOnColor))

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
	// Layout: padded cell, image fits at top preserving aspect, name centered
	// below the image. Cell background shows through any letterbox area, so
	// no black bars — the user sees the WHOLE student desktop undistorted.
	const (
		cellPad = int32(10) // padding around image+label inside cell
		nameH   = int32(22) // label strip below image
		nameGap = int32(6)  // gap between image and label
	)

	// Cell background (also fills any letterbox area)
	cellBg := uintptr(0x00181818)
	if cell.offline {
		cellBg = 0x001A0000
	}
	bg, _, _ := procCreateSolidBrush.Call(cellBg)
	cellRc := winRect{x, y, x + w, y + h}
	procFillRect.Call(hdc, uintptr(unsafe.Pointer(&cellRc)), bg)
	procDeleteObject.Call(bg)

	// Image area: top portion of cell, padded; reserve space below for name.
	imgArea := winRect{
		x + cellPad,
		y + cellPad,
		x + w - cellPad,
		y + h - cellPad - nameH - nameGap,
	}
	availW := imgArea.Right - imgArea.Left
	availH := imgArea.Bottom - imgArea.Top

	if len(cell.pixels) > 0 && availW > 0 && availH > 0 {
		// Aspect-preserving fit (letterbox inside imgArea). The bars are the
		// cell background colour, not black — looks intentional, not broken.
		scaleX := float64(availW) / float64(cell.imgW)
		scaleY := float64(availH) / float64(cell.imgH)
		scale := scaleX
		if scaleY < scaleX {
			scale = scaleY
		}
		dstW := int32(float64(cell.imgW) * scale)
		dstH := int32(float64(cell.imgH) * scale)
		if dstW < 1 {
			dstW = 1
		}
		if dstH < 1 {
			dstH = 1
		}
		dstX := imgArea.Left + (availW-dstW)/2
		dstY := imgArea.Top + (availH-dstH)/2

		// CPU-side nearest-neighbour resize to dst size, then 1:1 blit via
		// SetDIBitsToDevice. SetDIBitsToDevice is documented for "DIB →
		// device-dependent rectangle" with no scaling, and unlike
		// StretchDIBits doesn't carry stretch-mode state — it doesn't hit
		// the intermittent ret=0 bug observed during rapid window resize.
		var (
			drawW, drawH int32
			drawPixels   []byte
		)
		if dstW == cell.imgW && dstH == cell.imgH {
			drawW, drawH, drawPixels = cell.imgW, cell.imgH, cell.pixels
		} else {
			drawW, drawH = dstW, dstH
			drawPixels = resizeBGRA(cell.pixels, cell.imgW, cell.imgH, dstW, dstH)
		}

		bi := bitmapInfoHeader{
			biSize:     40,
			biWidth:    drawW,
			biHeight:   -drawH, // negative → top-down
			biPlanes:   1,
			biBitCount: 32,
		}
		ret, _, callErr := procSetDIBitsToDevice.Call(
			hdc,
			uintptr(dstX), uintptr(dstY),
			uintptr(drawW), uintptr(drawH),
			0, 0, // src x/y
			0,                // StartScan
			uintptr(drawH),   // cLines
			uintptr(unsafe.Pointer(&drawPixels[0])),
			uintptr(unsafe.Pointer(&bi)),
			dibRgbColors,
		)
		if int32(ret) <= 0 {
			devlog.Logf("SetDIBitsToDevice FAILED  ret=%d  err=%v  size=%dx%d",
				int32(ret), callErr, drawW, drawH)
		}
	} else if availW > 0 && availH > 0 {
		// No screenshot yet — show status text in the image area.
		status := "Αναμονή..."
		if cell.offline {
			status = "Εκτός Σύνδεσης"
		}
		procSetBkMode.Call(hdc, uintptr(transparent))
		procSetTextColor.Call(hdc, 0x00555555)
		statusPtr := utf16(status)
		procDrawTextW.Call(hdc, uintptr(unsafe.Pointer(statusPtr)), ^uintptr(0),
			uintptr(unsafe.Pointer(&imgArea)), dtCenter|dtVcenter|dtSingleline)
	}

	// Name label below the image (mockup-style: hostname under each cell).
	procSetBkMode.Call(hdc, uintptr(transparent))
	if cell.offline {
		procSetTextColor.Call(hdc, 0x004444AA)
	} else {
		procSetTextColor.Call(hdc, 0x00DDDDDD)
	}
	namePtr := utf16(cell.name)
	nameRc := winRect{
		x + cellPad,
		y + h - cellPad - nameH,
		x + w - cellPad,
		y + h - cellPad,
	}
	procDrawTextW.Call(hdc, uintptr(unsafe.Pointer(namePtr)), ^uintptr(0),
		uintptr(unsafe.Pointer(&nameRc)), dtSingleline|dtCenter|dtVcenter)
}

// resizeBGRA returns a new BGRA buffer of size dstW×dstH, sampled by nearest
// neighbour from src. We do this on the CPU so the actual blit to the back
// buffer is a 1:1 StretchDIBits — that path is reliable, while non-integer
// downscales on memory DCs hit the documented intermittent-failure bug.
//
// Performance: at 1080p destination this is ~2 MB of writes per frame and
// runs in well under a millisecond on any laptop the app would target.
func resizeBGRA(src []byte, srcW, srcH, dstW, dstH int32) []byte {
	dst := make([]byte, int(dstW)*int(dstH)*4)
	if srcW <= 0 || srcH <= 0 || dstW <= 0 || dstH <= 0 {
		return dst
	}
	srcStride := int(srcW) * 4
	dstStride := int(dstW) * 4
	// Pre-compute src x for each dst x to skip the multiply per pixel.
	xMap := make([]int, dstW)
	for x := int32(0); x < dstW; x++ {
		sx := int(int64(x) * int64(srcW) / int64(dstW))
		if sx >= int(srcW) {
			sx = int(srcW) - 1
		}
		xMap[x] = sx * 4
	}
	for y := int32(0); y < dstH; y++ {
		sy := int(int64(y) * int64(srcH) / int64(dstH))
		if sy >= int(srcH) {
			sy = int(srcH) - 1
		}
		srcRow := src[sy*srcStride:]
		dstRow := dst[int(y)*dstStride:]
		for x := int32(0); x < dstW; x++ {
			off := xMap[x]
			dstRow[int(x)*4+0] = srcRow[off+0]
			dstRow[int(x)*4+1] = srcRow[off+1]
			dstRow[int(x)*4+2] = srcRow[off+2]
			dstRow[int(x)*4+3] = srcRow[off+3]
		}
	}
	return dst
}

// ── Pipe protocol helpers (overlapped I/O) ────────────────────────────────────
//
// Why overlapped: synchronous ReadFile/WriteFile on a Win32 byte-mode named
// pipe can wedge for minutes when the peer's I/O state machine drifts —
// observed in production with 60+ second blocks on writes that should take
// microseconds. With FILE_FLAG_OVERLAPPED we wait on an event with a real
// timeout and CancelIoEx if the operation hangs.

type pipeOp struct {
	ov    syscall.Overlapped
	event uintptr
}

func newPipeOp() (*pipeOp, error) {
	h, _, e := procCreateEventW.Call(0, 1, 0, 0) // manual reset, nonsignaled
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

// pipeWriteAll writes the full buffer with a per-call timeout. CancelIoEx on
// timeout so the kernel doesn't keep the I/O queued against the OVERLAPPED.
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
		if int(written) != len(data) {
			return fmt.Errorf("WriteFile partial: %d/%d", written, len(data))
		}
		return nil
	}
	if e != syscall.Errno(errorIoPending) {
		return fmt.Errorf("WriteFile: %w", e)
	}
	waitMs := uint32(timeout / time.Millisecond)
	if waitMs == 0 {
		waitMs = 1
	}
	wRet, _, _ := procWaitForSingleObject.Call(op.event, uintptr(waitMs))
	if wRet == waitTimeout {
		procCancelIoEx.Call(handle, uintptr(unsafe.Pointer(&op.ov)))
		procWaitForSingleObject.Call(op.event, infinite)
		return fmt.Errorf("WriteFile timeout after %v", timeout)
	}
	if wRet != waitObject0 {
		return fmt.Errorf("WriteFile WaitForSingleObject: 0x%x", wRet)
	}
	var transferred uint32
	gRet, _, gErr := procGetOverlappedResult.Call(
		handle,
		uintptr(unsafe.Pointer(&op.ov)),
		uintptr(unsafe.Pointer(&transferred)),
		0,
	)
	if gRet == 0 {
		return fmt.Errorf("GetOverlappedResult: %w", gErr)
	}
	if int(transferred) != len(data) {
		return fmt.Errorf("WriteFile partial: %d/%d", transferred, len(data))
	}
	return nil
}

// pipeReadFull reads exactly len(buf) bytes with an overall timeout, looping
// over partial reads. The timeout starts fresh from the time of the call.
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
				return fmt.Errorf("ReadFile WaitForSingleObject: 0x%x", wRet)
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
			return io.EOF
		}
		read += int(nRead)
	}
	return nil
}

// connectPipe retries CreateFile on the pipe for up to 15 seconds. Opens the
// handle with FILE_FLAG_OVERLAPPED so all subsequent I/O is async-capable.
func connectPipe() (uintptr, error) {
	namePtr, _ := syscall.UTF16PtrFromString(`\\.\pipe\ClassSendMonitor`)
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		h, _, _ := procCreateFileW.Call(
			uintptr(unsafe.Pointer(namePtr)),
			uintptr(genericRead|genericWrite),
			0, 0,
			openExisting,
			fileAttrNormal|fileFlagOverlapped,
			0,
		)
		if h != invalidHandle {
			return h, nil
		}
		procWaitNamedPipe.Call(uintptr(unsafe.Pointer(namePtr)), 1000)
		time.Sleep(100 * time.Millisecond)
	}
	return 0, fmt.Errorf("pipe not available after 15 s")
}

// readPipe reads messages from the pipe and updates the grid state.
// Runs in its own goroutine; posts wmUpdate or wmPipeEOF to hwnd.
func readPipe(handle, hwnd uintptr) {
	defer func() {
		if r := recover(); r != nil {
			devlog.Logf("readPipe PANIC: %v\n%s", r, debug.Stack())
		}
	}()
	op, err := newPipeOp()
	if err != nil {
		devlog.Logf("readPipe: newPipeOp: %v", err)
		procPostMessageW.Call(hwnd, wmPipeEOF, 0, 0)
		return
	}
	defer op.close()
	hdr := make([]byte, 8)

	for {
		// Long timeout per header read so we wake periodically; the goroutine
		// is happy to sit idle for a minute between teacher messages.
		if err := pipeReadFull(handle, op, hdr, time.Minute); err != nil {
			devlog.Logf("readPipe: header read err: %v", err)
			procPostMessageW.Call(hwnd, wmPipeEOF, 0, 0)
			return
		}
		msgType := binary.LittleEndian.Uint32(hdr[0:4])
		payLen := binary.LittleEndian.Uint32(hdr[4:8])
		devlog.Logf("readPipe: msgType=%d payLen=%d", msgType, payLen)

		var payload []byte
		if payLen > 0 {
			payload = make([]byte, payLen)
			// Once a header arrives, the body should follow within 10 s.
			if err := pipeReadFull(handle, op, payload, 10*time.Second); err != nil {
				devlog.Logf("readPipe: payload read err: %v", err)
				procPostMessageW.Call(hwnd, wmPipeEOF, 0, 0)
				return
			}
		}

		switch msgType {
		case msgInit:
			handleInit(payload)
			devlog.Logf("readPipe: handleInit done")
		case msgShot:
			handleShot(payload)
			devlog.Logf("readPipe: handleShot done")
		case msgOffline:
			handleOffline(payload)
			devlog.Logf("readPipe: handleOffline done")
		case msgStop:
			devlog.Logf("readPipe: msgStop received")
			procPostMessageW.Call(hwnd, wmClose, 0, 0)
			return
		default:
			devlog.Logf("readPipe: UNKNOWN msgType=%d", msgType)
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
	// Preserve existing screenshots BY NAME, not by slot index. The
	// teacher's student list can reorder (sort change, mid-list join,
	// leave) — if we matched by slot, every reorder would wipe the
	// pixels of every cell whose position shifted, and the cell would
	// paint blank until that student's next round-robin shot. This was
	// the long-standing "RGB lights turning each other off" bug: only
	// the most-recently-updated cell ever showed content because every
	// re-init blew the others away. Lookup by name keeps each student's
	// last good thumbnail wherever they end up in the new layout.
	oldByName := make(map[string]cellState, len(cells))
	for _, c := range cells {
		if c.name != "" {
			oldByName[c.name] = c
		}
	}
	newCells := make([]cellState, len(names))
	for i, name := range names {
		if prev, ok := oldByName[name]; ok {
			newCells[i] = prev
			newCells[i].offline = false // freshly listed → not offline
		} else {
			newCells[i] = cellState{name: name}
		}
	}
	cells = newCells
}

func handleShot(payload []byte) {
	defer func() {
		if r := recover(); r != nil {
			devlog.Logf("handleShot PANIC: %v\n%s", r, debug.Stack())
		}
	}()
	if len(payload) < 8 {
		devlog.Logf("handleShot: payload too short  len=%d", len(payload))
		return
	}
	idx := binary.LittleEndian.Uint32(payload[0:4])
	jpegLen := int(binary.LittleEndian.Uint32(payload[4:8]))
	if 8+jpegLen > len(payload) {
		devlog.Logf("handleShot: truncated  jpegLen=%d payload=%d", jpegLen, len(payload))
		return
	}
	jpegData := payload[8 : 8+jpegLen]

	// Decode JPEG → RGBA → BGRA (Windows DIB)
	img, err := jpeg.Decode(bytes.NewReader(jpegData))
	if err != nil {
		devlog.Logf("handleShot: jpeg.Decode err: %v", err)
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
		devlog.Logf("shot received  idx=%d  %dx%d  jpeg=%dB  bgra=%dB",
			idx, imgW, imgH, jpegLen, len(bgra))
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

	devlog.Init("monitoring")
	defer devlog.Close()
	devlog.Logf("startup  pid=%d  build=%s  exe=%s", os.Getpid(), buildinfo.String(), os.Args[0])

	hInst, _, _ := procGetModuleHandleW.Call(0)
	className := utf16("ClassSendMonitor")

	// IDC_ARROW = 32512. Without an hCursor set, Windows shows the wait/busy
	// cursor permanently because the system has no default to draw.
	arrowCursor, _, _ := procLoadCursorW.Call(0, uintptr(32512))

	wc := wndClassExW{
		cbSize:        uint32(unsafe.Sizeof(wndClassExW{})),
		style:         0,
		lpfnWndProc:   wndProcCallback,
		hInstance:     hInst,
		hCursor:       arrowCursor,
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
		uintptr(unsafe.Pointer(utf16("ClassSend - Παρακολούθηση Τάξης  ["+buildinfo.String()+"]"))),
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
		defer func() {
			if r := recover(); r != nil {
				devlog.Logf("pipe goroutine PANIC: %v\n%s", r, debug.Stack())
			}
		}()
		pipe, err := connectPipe()
		if err != nil {
			devlog.Logf("pipe connect FAILED: %v", err)
			procPostMessageW.Call(hwnd, wmPipeEOF, 0, 0)
			return
		}
		pipeMu.Lock()
		pipeHandle = pipe
		pipeMu.Unlock()
		devlog.Logf("pipe connected, entering read loop")
		readPipe(pipe, hwnd)
		devlog.Logf("read loop exited, closing pipe")
		pipeMu.Lock()
		pipeHandle = 0
		pipeMu.Unlock()
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
