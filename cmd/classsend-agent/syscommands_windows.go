//go:build windows

package main

import (
	"bytes"
	"fmt"
	"image"
	"image/draw"
	"image/jpeg"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"classsend/internal/core"
	"classsend/internal/network"
	"classsend/internal/protocol"
)

// ── Windows API ───────────────────────────────────────────────────────────────

var (
	user32   = syscall.NewLazyDLL("user32.dll")
	gdi32    = syscall.NewLazyDLL("gdi32.dll")
	kernel32 = syscall.NewLazyDLL("kernel32.dll")
	advapi32 = syscall.NewLazyDLL("advapi32.dll")

	// Screen capture
	procGetDesktopWindow       = user32.NewProc("GetDesktopWindow")
	procGetDC                  = user32.NewProc("GetDC")
	procReleaseDC              = user32.NewProc("ReleaseDC")
	procGetSystemMetrics       = user32.NewProc("GetSystemMetrics")
	procCreateCompatibleDC     = gdi32.NewProc("CreateCompatibleDC")
	procCreateCompatibleBitmap = gdi32.NewProc("CreateCompatibleBitmap")
	procSelectObject           = gdi32.NewProc("SelectObject")
	procBitBlt                 = gdi32.NewProc("BitBlt")
	procDeleteObject           = gdi32.NewProc("DeleteObject")
	procDeleteDC               = gdi32.NewProc("DeleteDC")
	procGetDIBits              = gdi32.NewProc("GetDIBits")

	// Window creation / management
	procRegisterClassExW    = user32.NewProc("RegisterClassExW")
	procCreateWindowExW     = user32.NewProc("CreateWindowExW")
	procShowWindow          = user32.NewProc("ShowWindow")
	procSetForegroundWindow = user32.NewProc("SetForegroundWindow")
	procSetWindowPos        = user32.NewProc("SetWindowPos")
	procDefWindowProcW      = user32.NewProc("DefWindowProcW")
	procPostQuitMessage     = user32.NewProc("PostQuitMessage")
	procGetMessageW         = user32.NewProc("GetMessageW")
	procTranslateMessage    = user32.NewProc("TranslateMessage")
	procDispatchMessageW    = user32.NewProc("DispatchMessageW")
	procPostMessageW        = user32.NewProc("PostMessageW")
	procGetModuleHandleW    = kernel32.NewProc("GetModuleHandleW")
	procGetConsoleWindow    = kernel32.NewProc("GetConsoleWindow")

	// Registry
	procRegCreateKeyExW  = advapi32.NewProc("RegCreateKeyExW")
	procRegOpenKeyExW    = advapi32.NewProc("RegOpenKeyExW")
	procRegSetValueExW   = advapi32.NewProc("RegSetValueExW")
	procRegQueryValueExW = advapi32.NewProc("RegQueryValueExW")
	procRegDeleteValueW  = advapi32.NewProc("RegDeleteValueW")
	procRegCloseKey      = advapi32.NewProc("RegCloseKey")

	// GDI drawing
	procBeginPaint       = user32.NewProc("BeginPaint")
	procEndPaint         = user32.NewProc("EndPaint")
	procGetClientRect    = user32.NewProc("GetClientRect")
	procFillRect         = user32.NewProc("FillRect")
	procCreateSolidBrush = gdi32.NewProc("CreateSolidBrush")
	procSetTextColor     = gdi32.NewProc("SetTextColor")
	procSetBkMode        = gdi32.NewProc("SetBkMode")
	procDrawTextW        = user32.NewProc("DrawTextW")
	procCreateFontW      = gdi32.NewProc("CreateFontW")
	procInvalidateRect   = user32.NewProc("InvalidateRect")
	procDestroyWindow    = user32.NewProc("DestroyWindow")
	procSetDIBits        = gdi32.NewProc("SetDIBits")
	procStretchBlt       = gdi32.NewProc("StretchBlt")

	// Keyboard hook
	procSetWindowsHookExW   = user32.NewProc("SetWindowsHookExW")
	procUnhookWindowsHookEx = user32.NewProc("UnhookWindowsHookEx")
	procCallNextHookEx      = user32.NewProc("CallNextHookEx")
	procGetAsyncKeyState    = user32.NewProc("GetAsyncKeyState")

	// Mute key simulation
	procKeybdEvent = user32.NewProc("keybd_event")

	// Close-apps enumeration
	procEnumWindows     = user32.NewProc("EnumWindows")
	procIsWindowVisible = user32.NewProc("IsWindowVisible")
	procGetWindowTextW  = user32.NewProc("GetWindowTextW")
	procGetClassNameW   = user32.NewProc("GetClassNameW")
)

// ── Constants ─────────────────────────────────────────────────────────────────

const (
	smCxScreen   = 0
	smCyScreen   = 1
	srcCopy      = 0x00CC0020
	dibRgbColors = 0
	biRgb        = 0

	wsExTopmost = 0x00000008
	wsPopup     = 0x80000000
	wsVisible   = 0x10000000
	csHredraw   = 0x0002
	csVredraw   = 0x0001

	wmDestroy      = 0x0002
	wmPaint        = 0x000F
	wmClose        = 0x0010
	wmKeydown      = 0x0100
	wmSyskeydown   = 0x0104
	wmLbuttondown  = 0x0201
	wmRbuttondown  = 0x0204
	wmMbuttondown  = 0x0207
	wmNclbuttondown = 0x00A1

	hwndTopmost = ^uintptr(0) // HWND_TOPMOST = (HWND)(-1)

	swpNomove    = 0x0002
	swpNosize    = 0x0001
	swpShowWindow = 0x0040

	dtCenter    = 0x00000001
	dtVcenter   = 0x00000004
	dtSingleline = 0x00000020
	dtWordBreak  = 0x00000010
	dtLeft       = 0x00000000

	transparent = 1 // SetBkMode TRANSPARENT

	whKeyboardLl = 13
	hcAction      = 0

	vkLwin    = 0x5B
	vkRwin    = 0x5C
	vkTab     = 0x09
	vkEscape  = 0x1B
	vkF4      = 0x73
	vkMenu    = 0x12 // Alt
	vkControl = 0x11

	vkVolumeMute   = 0xAD
	keyeventfKeyup = 0x0002

	wsExToolWindow = 0x00000080
	wsExNoActivate = 0x08000000
	wmErasebkgnd   = 0x0014
)

// ── Structures ────────────────────────────────────────────────────────────────

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

type kbdllHookStruct struct {
	vkCode      uint32
	scanCode    uint32
	flags       uint32
	time        uint32
	dwExtraInfo uintptr
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

// ── Callback allocation ───────────────────────────────────────────────────────

var (
	wndProcCB    uintptr
	keyHookCB    uintptr
	notifProcCB  uintptr
	castViewProcCB uintptr
)

func init() {
	wndProcCB      = syscall.NewCallback(overlayWndProc)
	keyHookCB      = syscall.NewCallback(keyboardHookProc)
	notifProcCB    = syscall.NewCallback(monitorNotifWndProc)
	castViewProcCB = syscall.NewCallback(castViewWndProc)
}

// ── Screen capture ────────────────────────────────────────────────────────────

func captureScreen() ([]byte, error) {
	desktop, _, _ := procGetDesktopWindow.Call()
	hdcScreen, _, _ := procGetDC.Call(desktop)
	defer procReleaseDC.Call(desktop, hdcScreen)

	w, _, _ := procGetSystemMetrics.Call(smCxScreen)
	h, _, _ := procGetSystemMetrics.Call(smCyScreen)

	hdcMem, _, _ := procCreateCompatibleDC.Call(hdcScreen)
	defer procDeleteDC.Call(hdcMem)

	hBmp, _, _ := procCreateCompatibleBitmap.Call(hdcScreen, w, h)
	defer procDeleteObject.Call(hBmp)

	procSelectObject.Call(hdcMem, hBmp)
	procBitBlt.Call(hdcMem, 0, 0, w, h, hdcScreen, 0, 0, srcCopy)

	bi := bitmapInfoHeader{
		biSize:        40,
		biWidth:       int32(w),
		biHeight:      -int32(h),
		biPlanes:      1,
		biBitCount:    32,
		biCompression: biRgb,
	}

	raw := make([]byte, w*h*4)
	procGetDIBits.Call(
		hdcScreen, hBmp, 0, h,
		uintptr(unsafe.Pointer(&raw[0])),
		uintptr(unsafe.Pointer(&bi)),
		dibRgbColors,
	)

	for i := 0; i < len(raw); i += 4 {
		raw[i+0], raw[i+2] = raw[i+2], raw[i+0]
		raw[i+3] = 255
	}

	img := &image.NRGBA{Pix: raw, Stride: int(w) * 4, Rect: image.Rect(0, 0, int(w), int(h))}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 40}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// ── Mute toggle ───────────────────────────────────────────────────────────────

func muteAudio() {
	procKeybdEvent.Call(vkVolumeMute, 0, 0, 0)
	procKeybdEvent.Call(vkVolumeMute, 0, keyeventfKeyup, 0)
}

// ── Launch / focus app ────────────────────────────────────────────────────────

func launchApp(path string) error {
	return exec.Command("cmd", "/c", "start", "", path).Start()
}

func focusApp(titleSubstr string) error {
	needle := strings.ToLower(titleSubstr)
	found := false
	cb := syscall.NewCallback(func(hwnd, _ uintptr) uintptr {
		vis, _, _ := procIsWindowVisible.Call(hwnd)
		if vis == 0 {
			return 1
		}
		title := make([]uint16, 512)
		tn, _, _ := procGetWindowTextW.Call(hwnd, uintptr(unsafe.Pointer(&title[0])), 512)
		if tn == 0 {
			return 1
		}
		if strings.Contains(strings.ToLower(syscall.UTF16ToString(title[:tn])), needle) {
			procShowWindow.Call(hwnd, 9)
			procSetForegroundWindow.Call(hwnd)
			found = true
			return 0
		}
		return 1
	})
	procEnumWindows.Call(cb, 0)
	if !found {
		return fmt.Errorf("window '%s' not found", titleSubstr)
	}
	return nil
}

// ── Close visible apps ────────────────────────────────────────────────────────

func closeVisibleApps() {
	cb := syscall.NewCallback(func(hwnd, _ uintptr) uintptr {
		vis, _, _ := procIsWindowVisible.Call(hwnd)
		if vis == 0 {
			return 1
		}
		cls := make([]uint16, 256)
		n, _, _ := procGetClassNameW.Call(hwnd, uintptr(unsafe.Pointer(&cls[0])), 256)
		switch syscall.UTF16ToString(cls[:n]) {
		case "ConsoleWindowClass", "CASCADIA_HOSTING_WINDOW_CLASS",
			"Shell_TrayWnd", "Progman", "WorkerW", "DV2ControlHost":
			return 1
		}
		title := make([]uint16, 256)
		tn, _, _ := procGetWindowTextW.Call(hwnd, uintptr(unsafe.Pointer(&title[0])), 256)
		if tn == 0 {
			return 1
		}
		if syscall.UTF16ToString(title[:tn]) == "Program Manager" {
			return 1
		}
		procPostMessageW.Call(hwnd, wmClose, 0, 0)
		return 1
	})
	procEnumWindows.Call(cb, 0)
}

// ── Lock overlay ──────────────────────────────────────────────────────────────

var (
	overlayMu   sync.Mutex
	overlayHwnd uintptr
	lockStop    chan struct{}
)

func overlayWndProc(hwnd, msg, wParam, lParam uintptr) uintptr {
	switch msg {
	case wmPaint:
		drawLockScreen(hwnd)
		return 0
	case wmDestroy:
		procPostQuitMessage.Call(0)
		return 0
	case wmKeydown, wmSyskeydown,
		wmLbuttondown, wmRbuttondown, wmMbuttondown,
		wmNclbuttondown:
		return 0
	}
	r, _, _ := procDefWindowProcW.Call(hwnd, msg, wParam, lParam)
	return r
}

func keyboardHookProc(code, wParam, lParam uintptr) uintptr {
	overlayMu.Lock()
	active := overlayHwnd != 0
	overlayMu.Unlock()

	if code == hcAction && active {
		ks := (*kbdllHookStruct)(unsafe.Pointer(lParam))
		vk := ks.vkCode

		if vk == vkLwin || vk == vkRwin {
			return 1
		}
		alt, _, _ := procGetAsyncKeyState.Call(vkMenu)
		if alt&0x8000 != 0 && (vk == vkTab || vk == vkF4 || vk == vkEscape) {
			return 1
		}
		ctrl, _, _ := procGetAsyncKeyState.Call(vkControl)
		if ctrl&0x8000 != 0 && vk == vkEscape {
			return 1
		}
	}
	r, _, _ := procCallNextHookEx.Call(0, code, wParam, lParam)
	return r
}

func i32(n int32) uintptr { return uintptr(uint32(n)) }

func utf16(s string) *uint16 {
	p, _ := syscall.UTF16PtrFromString(s)
	return p
}

func drawLockScreen(hwnd uintptr) {
	var ps paintStruct
	hdc, _, _ := procBeginPaint.Call(hwnd, uintptr(unsafe.Pointer(&ps)))
	defer procEndPaint.Call(hwnd, uintptr(unsafe.Pointer(&ps)))

	var rc winRect
	procGetClientRect.Call(hwnd, uintptr(unsafe.Pointer(&rc)))
	midY := rc.Bottom / 2

	bg, _, _ := procCreateSolidBrush.Call(0x00000A1A)
	procFillRect.Call(hdc, uintptr(unsafe.Pointer(&rc)), bg)
	procDeleteObject.Call(bg)

	procSetBkMode.Call(hdc, transparent)

	hFontTitle, _, _ := procCreateFontW.Call(
		i32(-72), 0, 0, 0,
		700, 0, 0, 0,
		1, 0, 0, 4, 0,
		uintptr(unsafe.Pointer(utf16("Segoe UI"))),
	)
	origFont, _, _ := procSelectObject.Call(hdc, hFontTitle)

	procSetTextColor.Call(hdc, 0x002070E0)
	titleStr, _ := syscall.UTF16PtrFromString("Κλειδωμένος")
	titleRect := winRect{rc.Left, rc.Top, rc.Right, midY + 10}
	procDrawTextW.Call(hdc, uintptr(unsafe.Pointer(titleStr)), ^uintptr(0),
		uintptr(unsafe.Pointer(&titleRect)), dtCenter|dtVcenter|dtSingleline)

	hFontSub, _, _ := procCreateFontW.Call(
		i32(-30), 0, 0, 0,
		400, 0, 0, 0,
		1, 0, 0, 4, 0,
		uintptr(unsafe.Pointer(utf16("Segoe UI"))),
	)
	procSelectObject.Call(hdc, hFontSub)

	procSetTextColor.Call(hdc, 0x00406080)
	subStr, _ := syscall.UTF16PtrFromString(
		"Ο υπολογιστής σου κλειδώθηκε από τον δάσκαλο")
	subRect := winRect{rc.Left + 60, midY + 20, rc.Right - 60, midY + 90}
	procDrawTextW.Call(hdc, uintptr(unsafe.Pointer(subStr)), ^uintptr(0),
		uintptr(unsafe.Pointer(&subRect)), dtCenter|dtWordBreak)

	procSelectObject.Call(hdc, origFont)
	procDeleteObject.Call(hFontTitle)
	procDeleteObject.Call(hFontSub)
}

func runLockOverlay(hwndOut chan<- uintptr) {
	runtime.LockOSThread()

	hInst, _, _ := procGetModuleHandleW.Call(0)
	className := utf16("ClassSendLock")

	wc := wndClassExW{
		cbSize:        uint32(unsafe.Sizeof(wndClassExW{})),
		style:         csHredraw | csVredraw,
		lpfnWndProc:   wndProcCB,
		hInstance:     hInst,
		hbrBackground: 0,
		lpszClassName: className,
	}
	procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc)))

	sw, _, _ := procGetSystemMetrics.Call(smCxScreen)
	sh, _, _ := procGetSystemMetrics.Call(smCyScreen)

	hwnd, _, _ := procCreateWindowExW.Call(
		wsExTopmost,
		uintptr(unsafe.Pointer(className)),
		uintptr(unsafe.Pointer(utf16("ClassSend"))),
		wsPopup|wsVisible,
		0, 0, sw, sh,
		0, 0, hInst, 0,
	)
	procShowWindow.Call(hwnd, 5)
	procSetWindowPos.Call(hwnd, hwndTopmost, 0, 0, sw, sh, swpShowWindow)
	procSetForegroundWindow.Call(hwnd)

	hwndOut <- hwnd

	hHook, _, _ := procSetWindowsHookExW.Call(whKeyboardLl, keyHookCB, hInst, 0)

	var msg winMsg
	for {
		r, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&msg)), 0, 0, 0)
		if r == 0 || r == ^uintptr(0) {
			break
		}
		procTranslateMessage.Call(uintptr(unsafe.Pointer(&msg)))
		procDispatchMessageW.Call(uintptr(unsafe.Pointer(&msg)))
	}

	if hHook != 0 {
		procUnhookWindowsHookEx.Call(hHook)
	}
	overlayMu.Lock()
	overlayHwnd = 0
	overlayMu.Unlock()
}

func lockScreen() error {
	overlayMu.Lock()
	defer overlayMu.Unlock()
	if overlayHwnd != 0 {
		return nil
	}

	ready := make(chan uintptr, 1)
	go runLockOverlay(ready)
	overlayHwnd = <-ready

	if overlayHwnd == 0 {
		return fmt.Errorf("failed to create lock overlay")
	}

	lockStop = make(chan struct{})
	stop := lockStop
	go func() {
		ticker := time.NewTicker(1 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				overlayMu.Lock()
				hwnd := overlayHwnd
				overlayMu.Unlock()
				if hwnd == 0 {
					return
				}
				procSetWindowPos.Call(hwnd, hwndTopmost, 0, 0, 0, 0,
					swpNomove|swpNosize|swpShowWindow)
				procSetForegroundWindow.Call(hwnd)
			}
		}
	}()
	return nil
}

func unlockScreen() {
	overlayMu.Lock()
	hwnd := overlayHwnd
	stop := lockStop
	lockStop = nil
	overlayMu.Unlock()

	if hwnd == 0 {
		return
	}
	if stop != nil {
		close(stop)
	}
	procPostMessageW.Call(hwnd, wmClose, 0, 0)
}

// ── Monitoring notification ───────────────────────────────────────────────────

var (
	notifMu   sync.Mutex
	notifHwnd uintptr
)

func monitorNotifWndProc(hwnd, msg, wParam, lParam uintptr) uintptr {
	switch msg {
	case wmPaint:
		drawMonitorNotif(hwnd)
		return 0
	case wmErasebkgnd:
		return 1
	case wmDestroy:
		procPostQuitMessage.Call(0)
		return 0
	}
	r, _, _ := procDefWindowProcW.Call(hwnd, msg, wParam, lParam)
	return r
}

func drawMonitorNotif(hwnd uintptr) {
	var ps paintStruct
	hdc, _, _ := procBeginPaint.Call(hwnd, uintptr(unsafe.Pointer(&ps)))
	defer procEndPaint.Call(hwnd, uintptr(unsafe.Pointer(&ps)))

	var rc winRect
	procGetClientRect.Call(hwnd, uintptr(unsafe.Pointer(&rc)))

	bg, _, _ := procCreateSolidBrush.Call(0x001A1A1A)
	procFillRect.Call(hdc, uintptr(unsafe.Pointer(&rc)), bg)
	procDeleteObject.Call(bg)

	border, _, _ := procCreateSolidBrush.Call(0x0020A0FF)
	borderRc := winRect{0, 0, rc.Right, 3}
	procFillRect.Call(hdc, uintptr(unsafe.Pointer(&borderRc)), border)
	procDeleteObject.Call(border)

	procSetBkMode.Call(hdc, transparent)
	procSetTextColor.Call(hdc, 0x0080C8FF)

	hFont, _, _ := procCreateFontW.Call(
		i32(-15), 0, 0, 0,
		600, 0, 0, 0,
		1, 0, 0, 4, 0,
		uintptr(unsafe.Pointer(utf16("Segoe UI"))),
	)
	origFont, _, _ := procSelectObject.Call(hdc, hFont)

	textPtr, _ := syscall.UTF16PtrFromString("Παρακολούθηση Ενεργή")
	textRc := winRect{10, 5, rc.Right - 10, rc.Bottom}
	procDrawTextW.Call(hdc, uintptr(unsafe.Pointer(textPtr)), ^uintptr(0),
		uintptr(unsafe.Pointer(&textRc)), dtSingleline|dtVcenter|dtLeft)

	procSelectObject.Call(hdc, origFont)
	procDeleteObject.Call(hFont)
}

func runMonitorNotifWindow(hwndOut chan<- uintptr) {
	runtime.LockOSThread()

	hInst, _, _ := procGetModuleHandleW.Call(0)
	className := utf16("ClassSendMonitorNotif")

	wc := wndClassExW{
		cbSize:        uint32(unsafe.Sizeof(wndClassExW{})),
		style:         csHredraw | csVredraw,
		lpfnWndProc:   notifProcCB,
		hInstance:     hInst,
		hbrBackground: 0,
		lpszClassName: className,
	}
	procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc)))

	sw, _, _ := procGetSystemMetrics.Call(smCxScreen)

	const notifW = uintptr(260)
	const notifH = uintptr(46)
	notifX := sw - notifW - 12
	notifY := uintptr(8)

	hwnd, _, _ := procCreateWindowExW.Call(
		wsExTopmost|wsExToolWindow|wsExNoActivate,
		uintptr(unsafe.Pointer(className)),
		uintptr(unsafe.Pointer(utf16("ClassSend"))),
		wsPopup|wsVisible,
		notifX, notifY, notifW, notifH,
		0, 0, hInst, 0,
	)
	procShowWindow.Call(hwnd, 5)
	procSetWindowPos.Call(hwnd, hwndTopmost, notifX, notifY, notifW, notifH, swpShowWindow)

	hwndOut <- hwnd

	var msg winMsg
	for {
		r, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&msg)), 0, 0, 0)
		if r == 0 || r == ^uintptr(0) {
			break
		}
		procTranslateMessage.Call(uintptr(unsafe.Pointer(&msg)))
		procDispatchMessageW.Call(uintptr(unsafe.Pointer(&msg)))
	}

	notifMu.Lock()
	notifHwnd = 0
	notifMu.Unlock()
}

func showMonitoringNotification() {
	notifMu.Lock()
	defer notifMu.Unlock()
	if notifHwnd != 0 {
		return
	}
	ready := make(chan uintptr, 1)
	go runMonitorNotifWindow(ready)
	notifHwnd = <-ready
}

func hideMonitoringNotification() {
	notifMu.Lock()
	hwnd := notifHwnd
	notifMu.Unlock()
	if hwnd != 0 {
		procPostMessageW.Call(hwnd, wmClose, 0, 0)
	}
}

// ── Casting viewer ────────────────────────────────────────────────────────────
//
// The student's casting viewer is a fullscreen Win32 window that displays JPEG
// frames streamed from the teacher over a dedicated TCP connection (CastServer).
// Frames are decoded on the receiving goroutine; a WM_APP+1 message triggers
// a repaint.  Students cannot close the window — only a CmdStopCast from the
// teacher does.

const (
	wmCastNewFrame = 0x8001 // WM_APP+1: new frame decoded, trigger repaint
	wmCastClose    = 0x8002 // WM_APP+2: teacher stopped casting, destroy window
)

var (
	castViewMu   sync.Mutex
	castViewHwnd uintptr

	castClientMu  sync.Mutex
	castClientConn *network.CastClient // active cast connection, nil when idle

	castFrameMu  sync.RWMutex
	castFramePix []byte // latest decoded frame as BGRA pixels
	castFrameW   int
	castFrameH   int
)

func castViewWndProc(hwnd, msg, wParam, lParam uintptr) uintptr {
	switch msg {
	case wmCastNewFrame:
		procInvalidateRect.Call(hwnd, 0, 1)
		return 0
	case wmCastClose:
		procDestroyWindow.Call(hwnd)
		return 0
	case wmClose:
		return 0 // block user close — teacher controls this
	case wmPaint:
		drawCastFrame(hwnd)
		return 0
	case wmErasebkgnd:
		return 1
	case wmDestroy:
		castViewMu.Lock()
		castViewHwnd = 0
		castViewMu.Unlock()
		procPostQuitMessage.Call(0)
		return 0
	}
	r, _, _ := procDefWindowProcW.Call(hwnd, msg, wParam, lParam)
	return r
}

func drawCastFrame(hwnd uintptr) {
	castFrameMu.RLock()
	pix := castFramePix
	fw, fh := castFrameW, castFrameH
	castFrameMu.RUnlock()

	var ps paintStruct
	hdc, _, _ := procBeginPaint.Call(hwnd, uintptr(unsafe.Pointer(&ps)))
	defer procEndPaint.Call(hwnd, uintptr(unsafe.Pointer(&ps)))

	var rc winRect
	procGetClientRect.Call(hwnd, uintptr(unsafe.Pointer(&rc)))

	// Black background in case frame hasn't arrived yet
	bg, _, _ := procCreateSolidBrush.Call(0)
	procFillRect.Call(hdc, uintptr(unsafe.Pointer(&rc)), bg)
	procDeleteObject.Call(bg)

	if len(pix) == 0 || fw == 0 || fh == 0 {
		return
	}

	bi := bitmapInfoHeader{
		biSize:        40,
		biWidth:       int32(fw),
		biHeight:      -int32(fh), // top-down DIB
		biPlanes:      1,
		biBitCount:    32,
		biCompression: biRgb,
	}

	hdcMem, _, _ := procCreateCompatibleDC.Call(hdc)
	defer procDeleteDC.Call(hdcMem)

	hBmp, _, _ := procCreateCompatibleBitmap.Call(hdc, uintptr(fw), uintptr(fh))
	defer procDeleteObject.Call(hBmp)

	procSelectObject.Call(hdcMem, hBmp)
	procSetDIBits.Call(
		hdcMem, hBmp, 0, uintptr(fh),
		uintptr(unsafe.Pointer(&pix[0])),
		uintptr(unsafe.Pointer(&bi)),
		dibRgbColors,
	)
	// StretchBlt fills the fullscreen window; HALFTONE for high-quality downscale
	procStretchBlt.Call(
		hdc, 0, 0, uintptr(rc.Right), uintptr(rc.Bottom),
		hdcMem, 0, 0, uintptr(fw), uintptr(fh),
		srcCopy,
	)
}

// updateCastFrame decodes a JPEG frame and signals the Win32 window to repaint.
// Called from the CastClient receive goroutine.
func updateCastFrame(jpegData []byte) {
	src, err := jpeg.Decode(bytes.NewReader(jpegData))
	if err != nil {
		return
	}
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	nrgba := image.NewNRGBA(image.Rect(0, 0, w, h))
	draw.Draw(nrgba, nrgba.Bounds(), src, b.Min, draw.Src)

	// RGBA → BGRA for Windows DIBs
	pix := make([]byte, w*h*4)
	for i := 0; i < len(nrgba.Pix); i += 4 {
		pix[i+0] = nrgba.Pix[i+2]
		pix[i+1] = nrgba.Pix[i+1]
		pix[i+2] = nrgba.Pix[i+0]
		pix[i+3] = 255
	}

	castFrameMu.Lock()
	castFramePix = pix
	castFrameW = w
	castFrameH = h
	castFrameMu.Unlock()

	castViewMu.Lock()
	hwnd := castViewHwnd
	castViewMu.Unlock()
	if hwnd != 0 {
		procPostMessageW.Call(hwnd, wmCastNewFrame, 0, 0)
	}
}

func showCastingViewer() {
	castViewMu.Lock()
	defer castViewMu.Unlock()
	if castViewHwnd != 0 {
		return
	}
	ready := make(chan uintptr, 1)
	go runCastViewWindow(ready)
	castViewHwnd = <-ready
}

func hideCastingViewer() {
	// Disconnect TCP stream
	castClientMu.Lock()
	if castClientConn != nil {
		castClientConn.Close()
		castClientConn = nil
	}
	castClientMu.Unlock()

	// Destroy Win32 window
	castViewMu.Lock()
	hwnd := castViewHwnd
	castViewMu.Unlock()
	if hwnd != 0 {
		procPostMessageW.Call(hwnd, wmCastClose, 0, 0)
	}
}

// connectCastStream dials the teacher's dedicated cast server and streams frames
// into updateCastFrame until the connection closes or hideCastingViewer is called.
func connectCastStream(serverAddr string) {
	client, err := network.DialCast(serverAddr)
	if err != nil {
		return
	}

	castClientMu.Lock()
	if castClientConn != nil {
		castClientConn.Close()
	}
	castClientConn = client
	castClientMu.Unlock()

	client.OnFrame = func(data []byte) {
		updateCastFrame(data)
	}
	client.Run() // blocks until connection closed

	castClientMu.Lock()
	if castClientConn == client {
		castClientConn = nil
	}
	castClientMu.Unlock()
}

func runCastViewWindow(hwndOut chan<- uintptr) {
	runtime.LockOSThread()

	hInst, _, _ := procGetModuleHandleW.Call(0)
	className := utf16("ClassSendCastView")

	wc := wndClassExW{
		cbSize:        uint32(unsafe.Sizeof(wndClassExW{})),
		style:         csHredraw | csVredraw,
		lpfnWndProc:   castViewProcCB,
		hInstance:     hInst,
		hbrBackground: 0,
		lpszClassName: className,
	}
	procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc)))

	// Fullscreen — covers the entire screen
	sw, _, _ := procGetSystemMetrics.Call(smCxScreen)
	sh, _, _ := procGetSystemMetrics.Call(smCyScreen)

	hwnd, _, _ := procCreateWindowExW.Call(
		wsExTopmost,
		uintptr(unsafe.Pointer(className)),
		uintptr(unsafe.Pointer(utf16("Οθόνη Δασκάλου — ClassSend"))),
		wsPopup|wsVisible,
		0, 0, sw, sh,
		0, 0, hInst, 0,
	)
	procSetWindowPos.Call(hwnd, hwndTopmost, 0, 0, 0, 0, swpNomove|swpNosize|swpShowWindow)

	hwndOut <- hwnd

	var m winMsg
	for {
		r, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if r == 0 {
			break
		}
		procTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
		procDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
	}
}

// ── Console visibility ────────────────────────────────────────────────────────

func hideConsole() {
	hwnd, _, _ := procGetConsoleWindow.Call()
	if hwnd != 0 {
		procShowWindow.Call(hwnd, 0) // SW_HIDE
	}
}

// ── Screenshot monitoring ─────────────────────────────────────────────────────

var (
	monitorMu   sync.Mutex
	monitorStop chan struct{}
)

func startMonitoring(sendFn func([]byte)) {
	monitorMu.Lock()
	defer monitorMu.Unlock()
	if monitorStop != nil {
		return
	}
	monitorStop = make(chan struct{})
	go func(stop chan struct{}) {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				if data, err := captureScreen(); err == nil {
					sendFn(data)
				}
			}
		}
	}(monitorStop)
}

func stopMonitoring() {
	monitorMu.Lock()
	defer monitorMu.Unlock()
	if monitorStop != nil {
		close(monitorStop)
		monitorStop = nil
	}
}

// ── Autostart (registry) ──────────────────────────────────────────────────────

const (
	hkcu            = uintptr(0x80000001)
	keyQueryValue   = 0x0001
	keySetValue     = 0x0002
	regSz           = 1
	regDword        = 4
	runKeyPath      = `Software\Microsoft\Windows\CurrentVersion\Run`
	prefKeyPath     = `Software\ClassSend`
	autostartValueName = "ClassSend"
	autostartPrefName  = "Autostart"
)

func regOpenOrCreate(root uintptr, path string, access uint32) (uintptr, error) {
	var hkey uintptr
	r, _, _ := procRegCreateKeyExW.Call(
		root,
		uintptr(unsafe.Pointer(utf16(path))),
		0, 0, 0,
		uintptr(access),
		0,
		uintptr(unsafe.Pointer(&hkey)),
		0,
	)
	if r != 0 {
		return 0, fmt.Errorf("RegCreateKeyEx error %d", r)
	}
	return hkey, nil
}

func regOpen(root uintptr, path string, access uint32) (uintptr, bool) {
	var hkey uintptr
	r, _, _ := procRegOpenKeyExW.Call(
		root,
		uintptr(unsafe.Pointer(utf16(path))),
		0,
		uintptr(access),
		uintptr(unsafe.Pointer(&hkey)),
	)
	return hkey, r == 0
}

func regSetString(root uintptr, path, name, value string) error {
	hkey, err := regOpenOrCreate(root, path, keySetValue)
	if err != nil {
		return err
	}
	defer procRegCloseKey.Call(hkey)
	val, _ := syscall.UTF16FromString(value)
	r, _, _ := procRegSetValueExW.Call(
		hkey,
		uintptr(unsafe.Pointer(utf16(name))),
		0,
		regSz,
		uintptr(unsafe.Pointer(&val[0])),
		uintptr(len(val)*2),
	)
	if r != 0 {
		return fmt.Errorf("RegSetValueEx error %d", r)
	}
	return nil
}

func regSetDWORD(root uintptr, path, name string, value uint32) error {
	hkey, err := regOpenOrCreate(root, path, keySetValue)
	if err != nil {
		return err
	}
	defer procRegCloseKey.Call(hkey)
	r, _, _ := procRegSetValueExW.Call(
		hkey,
		uintptr(unsafe.Pointer(utf16(name))),
		0,
		regDword,
		uintptr(unsafe.Pointer(&value)),
		4,
	)
	if r != 0 {
		return fmt.Errorf("RegSetValueEx error %d", r)
	}
	return nil
}

func regGetDWORD(root uintptr, path, name string) (uint32, bool) {
	hkey, ok := regOpen(root, path, keyQueryValue)
	if !ok {
		return 0, false
	}
	defer procRegCloseKey.Call(hkey)
	var val, typ uint32
	size := uint32(4)
	r, _, _ := procRegQueryValueExW.Call(
		hkey,
		uintptr(unsafe.Pointer(utf16(name))),
		0,
		uintptr(unsafe.Pointer(&typ)),
		uintptr(unsafe.Pointer(&val)),
		uintptr(unsafe.Pointer(&size)),
	)
	return val, r == 0
}

func regDeleteValue(root uintptr, path, name string) {
	hkey, ok := regOpen(root, path, keySetValue)
	if !ok {
		return
	}
	defer procRegCloseKey.Call(hkey)
	procRegDeleteValueW.Call(hkey, uintptr(unsafe.Pointer(utf16(name))))
}

func isAutostartEnabled() bool {
	val, ok := regGetDWORD(hkcu, prefKeyPath, autostartPrefName)
	if !ok {
		return true
	}
	return val != 0
}

func setAutostart(enable bool) error {
	var pref uint32
	if enable {
		pref = 1
	}
	if err := regSetDWORD(hkcu, prefKeyPath, autostartPrefName, pref); err != nil {
		return err
	}
	if enable {
		exe, err := os.Executable()
		if err != nil {
			return err
		}
		return regSetString(hkcu, runKeyPath, autostartValueName, exe)
	}
	regDeleteValue(hkcu, runKeyPath, autostartValueName)
	return nil
}

func ensureAutostart() {
	if !isAutostartEnabled() {
		return
	}
	exe, err := os.Executable()
	if err != nil {
		return
	}
	regSetString(hkcu, runKeyPath, autostartValueName, exe) //nolint:errcheck
}

// ── Wire-up ───────────────────────────────────────────────────────────────────

func withRetry(attempts int, delay time.Duration, fn func() error) error {
	var lastErr error
	for i := 0; i < attempts; i++ {
		if i > 0 {
			time.Sleep(delay)
		}
		if err := fn(); err == nil {
			return nil
		} else {
			lastErr = err
		}
	}
	return lastErr
}

func setupStudentCommands(app *core.App, devMode bool) {
	sendShot := func(data []byte) {
		msg, err := protocol.Encode(protocol.TypeScreenshot, protocol.ScreenshotPayload{
			StudentID: app.Hostname,
			Data:      data,
		})
		if err == nil && app.Client != nil {
			app.Client.Send(msg) //nolint:errcheck
		}
	}

	report := func(cmd protocol.CommandPayload, err error) {
		app.SendCmdAck(cmd.CmdID, cmd.Action, err)
	}

	app.OnCommand = func(cmd protocol.CommandPayload) {
		switch cmd.Action {

		case protocol.CmdLockScreen:
			go func() {
				if err := withRetry(3, 500*time.Millisecond, lockScreen); err != nil {
					report(cmd, err)
					return
				}
				if devMode {
					time.Sleep(5 * time.Second)
					unlockScreen()
				}
			}()

		case protocol.CmdUnlockScreen:
			go func() { unlockScreen() }()

		case protocol.CmdShutdown:
			exec.Command("shutdown", "/s", "/f", "/t", "0").Start() //nolint:errcheck

		case protocol.CmdLaunchApp:
			if cmd.Param != "" {
				go func(p string) {
					if err := withRetry(3, 500*time.Millisecond, func() error {
						return launchApp(p)
					}); err != nil {
						report(cmd, err)
					}
				}(cmd.Param)
			}

		case protocol.CmdFocusApp:
			if cmd.Param != "" {
				go func(p string) {
					if err := withRetry(3, 500*time.Millisecond, func() error {
						return focusApp(p)
					}); err != nil {
						report(cmd, err)
					}
				}(cmd.Param)
			}

		case protocol.CmdCloseApps:
			go closeVisibleApps()

		case protocol.CmdMute, protocol.CmdUnmute:
			muteAudio()

		case protocol.CmdStartMonitor:
			showMonitoringNotification()

		case protocol.CmdStopMonitor:
			hideMonitoringNotification()
			stopMonitoring()

		case protocol.CmdStartCast:
			showCastingViewer()
			if cmd.Param != "" {
				go connectCastStream(cmd.Param)
			}

		case protocol.CmdStopCast:
			hideCastingViewer()

		case protocol.CmdRequestShot:
			go func() {
				if data, err := captureScreen(); err == nil {
					sendShot(data)
				}
			}()
		}
	}
}
