//go:build windows

package main

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"os/exec"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"classsend/internal/core"
	"classsend/internal/devlog"
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
	procSetProcessDPIAware     = user32.NewProc("SetProcessDPIAware")
	procGetGuiResources        = user32.NewProc("GetGuiResources") // diagnostic: per-process GDI/USER handle count
	procCreateCompatibleDC     = gdi32.NewProc("CreateCompatibleDC")
	procCreateCompatibleBitmap = gdi32.NewProc("CreateCompatibleBitmap")
	procCreateDIBSection       = gdi32.NewProc("CreateDIBSection") // modern alternative to GetDIBits — direct memory pixels
	procGdiFlush               = gdi32.NewProc("GdiFlush")          // commits pending GDI ops before reading DIBSection bits
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
	procSetLayeredWindowAttributes = user32.NewProc("SetLayeredWindowAttributes")
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
	procBeginPaint          = user32.NewProc("BeginPaint")
	procEndPaint            = user32.NewProc("EndPaint")
	procGetClientRect       = user32.NewProc("GetClientRect")
	procFillRect            = user32.NewProc("FillRect")
	procCreateSolidBrush    = gdi32.NewProc("CreateSolidBrush")
	procSetTextColor        = gdi32.NewProc("SetTextColor")
	procSetBkMode           = gdi32.NewProc("SetBkMode")
	procDrawTextW           = user32.NewProc("DrawTextW")
	procCreateFontW         = gdi32.NewProc("CreateFontW")
	procInvalidateRect      = user32.NewProc("InvalidateRect")
	procDestroyWindow       = user32.NewProc("DestroyWindow")
	procIsZoomed            = user32.NewProc("IsZoomed")
	procSetDIBits           = gdi32.NewProc("SetDIBits")
	procStretchBlt          = gdi32.NewProc("StretchBlt")
	procStretchDIBits       = gdi32.NewProc("StretchDIBits")
	procSetStretchBltMode   = gdi32.NewProc("SetStretchBltMode")
	procSetBrushOrgEx       = gdi32.NewProc("SetBrushOrgEx")

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

	procLoadCursorW = user32.NewProc("LoadCursorW")
)

// ── Constants ─────────────────────────────────────────────────────────────────

const (
	smCxScreen   = 0
	smCyScreen   = 1
	srcCopy      = 0x00CC0020
	captureBlt   = 0x40000000 // include layered/DWM-composited windows; without this Win10 BitBlt can return black for hardware-accelerated content
	dibRgbColors = 0
	biRgb        = 0

	wsExTopmost        = 0x00000008
	wsPopup            = 0x80000000
	wsVisible          = 0x10000000
	wsOverlappedWindow = 0x00CF0000
	csHredraw          = 0x0002
	csVredraw          = 0x0001

	wmDestroy      = 0x0002
	wmPaint        = 0x000F
	wmSize         = 0x0005
	wmClose        = 0x0010
	wmKeydown      = 0x0100
	wmSyskeydown   = 0x0104
	wmLbuttondown  = 0x0201
	wmRbuttondown  = 0x0204
	wmMbuttondown  = 0x0207
	wmNclbuttondown = 0x00A1

	hwndTopmost    = ^uintptr(0) // HWND_TOPMOST  = (HWND)(-1)
	hwndNotopmost  = ^uintptr(1) // HWND_NOTOPMOST = (HWND)(-2)

	swHide     = 0
	swMaximize = 3
	swShow     = 5
	swRestore  = 9

	swpNomove    = 0x0002
	swpNosize    = 0x0001
	swpShowWindow = 0x0040

	dtCenter    = 0x00000001
	dtVcenter   = 0x00000004
	dtSingleline = 0x00000020
	dtWordBreak  = 0x00000010
	dtLeft       = 0x00000000

	transparent = 1 // SetBkMode TRANSPARENT
	// HALFTONE silently returns 0 from StretchDIBits after the first call
	// on memory DCs with non-integer scale ratios. COLORONCOLOR is reliable.
	colorOnColor = 3

	whKeyboardLl = 13
	hcAction      = 0

	vkLwin    = 0x5B
	vkRwin    = 0x5C
	vkTab     = 0x09
	vkEscape  = 0x1B
	vkF4      = 0x73
	vkMenu    = 0x12 // Alt
	vkControl = 0x11

	vkCharF = 0x46 // 'F' key — toggle fullscreen in cast viewer
	vkCharT = 0x54 // 'T' key — toggle stay-on-top in cast viewer

	vkVolumeMute   = 0xAD
	keyeventfKeyup = 0x0002

	wsExToolWindow = 0x00000080
	wsExNoActivate = 0x08000000
	wsExLayered    = 0x00080000
	lwaAlpha       = 0x00000002
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
	wndProcCB   uintptr
	keyHookCB   uintptr
	notifProcCB uintptr
)

func init() {
	wndProcCB   = syscall.NewCallback(overlayWndProc)
	keyHookCB   = syscall.NewCallback(keyboardHookProc)
	notifProcCB = syscall.NewCallback(monitorNotifWndProc)
}

// ── Screen capture ────────────────────────────────────────────────────────────

// dpiAwareOnce ensures we tell Windows we want physical pixels exactly once.
// Without this, Go's default DPI-unaware mode makes GetSystemMetrics return
// the logical (scaled-down) resolution on hi-DPI displays — the captured
// screenshot is then a downscaled fragment of the real desktop. After we
// declare DPI awareness, GetSystemMetrics returns the full pixel count and
// BitBlt of the desktop captures everything visible to the user.
var dpiAwareOnce sync.Once

func ensureDPIAware() {
	dpiAwareOnce.Do(func() {
		procSetProcessDPIAware.Call()
	})
}

// captureScreen takes a default thumbnail-sized JPEG (used in normal monitoring).
func captureScreen() ([]byte, error) { return captureScreenSized(640, 50) }

// captureScreenHi takes a higher-resolution JPEG for the teacher's focus mode.
// 2400px on the longer edge with quality 80 — text on the student's screen
// stays readable on a 1080p+ teacher monitor. ~120-200 KB per frame on
// typical desktops, still well under the 1 MB pipe buffer.
func captureScreenHi() ([]byte, error) { return captureScreenSized(2400, 80) }

// ── "Technical difficulties" placeholder ────────────────────────────────────
//
// When BitBlt produces a zero buffer (luma == 0 — the agent literally has
// nothing real to send), we substitute a clearly-marked placeholder JPEG so
// the teacher sees "this PC's capture is broken" instead of an unhelpful
// black thumbnail indistinguishable from "the user has a black wallpaper".
// Generated once and cached by (width, quality).

// font5x7 — bitmap font for the placeholder text. Each glyph is a 5-bit-wide,
// 7-row pattern. Bit 4 (0x10) is the leftmost pixel; bit 0 (0x01) the right.
// Only the characters we need for the message.
var font5x7 = map[rune][7]byte{
	' ': {0, 0, 0, 0, 0, 0, 0},
	'A': {0x0E, 0x11, 0x11, 0x1F, 0x11, 0x11, 0x11},
	'B': {0x1E, 0x11, 0x11, 0x1E, 0x11, 0x11, 0x1E},
	'C': {0x0E, 0x11, 0x10, 0x10, 0x10, 0x11, 0x0E},
	'D': {0x1E, 0x11, 0x11, 0x11, 0x11, 0x11, 0x1E},
	'E': {0x1F, 0x10, 0x10, 0x1E, 0x10, 0x10, 0x1F},
	'F': {0x1F, 0x10, 0x10, 0x1E, 0x10, 0x10, 0x10},
	'H': {0x11, 0x11, 0x11, 0x1F, 0x11, 0x11, 0x11},
	'I': {0x0E, 0x04, 0x04, 0x04, 0x04, 0x04, 0x0E},
	'L': {0x10, 0x10, 0x10, 0x10, 0x10, 0x10, 0x1F},
	'N': {0x11, 0x19, 0x15, 0x13, 0x11, 0x11, 0x11},
	'O': {0x0E, 0x11, 0x11, 0x11, 0x11, 0x11, 0x0E},
	'P': {0x1E, 0x11, 0x11, 0x1E, 0x10, 0x10, 0x10},
	'R': {0x1E, 0x11, 0x11, 0x1E, 0x14, 0x12, 0x11},
	'S': {0x0F, 0x10, 0x10, 0x0E, 0x01, 0x01, 0x1E},
	'T': {0x1F, 0x04, 0x04, 0x04, 0x04, 0x04, 0x04},
	'U': {0x11, 0x11, 0x11, 0x11, 0x11, 0x11, 0x0E},
	'Y': {0x11, 0x11, 0x11, 0x0A, 0x04, 0x04, 0x04},
	'-': {0x00, 0x00, 0x00, 0x0E, 0x00, 0x00, 0x00},
	'.': {0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x04},
	':': {0x00, 0x04, 0x00, 0x00, 0x00, 0x04, 0x00},
}

// drawText draws s at (px,py) into img using fg, scaled by `scale` (each font
// pixel becomes scale×scale pixels). Characters are 5 wide + 1 spacing × scale.
func drawText(img *image.RGBA, s string, px, py, scale int, fg color.RGBA) {
	for _, r := range s {
		glyph, ok := font5x7[r]
		if !ok {
			glyph = font5x7[' ']
		}
		for row := 0; row < 7; row++ {
			b := glyph[row]
			for col := 0; col < 5; col++ {
				if b&(1<<(4-col)) != 0 {
					for sy := 0; sy < scale; sy++ {
						for sx := 0; sx < scale; sx++ {
							img.SetRGBA(px+col*scale+sx, py+row*scale+sy, fg)
						}
					}
				}
			}
		}
		px += 6 * scale // 5 char + 1 spacing
	}
}

var (
	placeholderMu    sync.Mutex
	placeholderCache = map[int][]byte{} // key: maxEdge encodes both size+quality bucket
)

// makeFailurePlaceholder returns a JPEG showing "TECHNICAL DIFFICULTIES" on
// an amber/black hazard background. Cached per maxEdge — same bytes are
// returned every time so we don't re-encode on every failed capture.
func makeFailurePlaceholder(maxEdge, quality int) []byte {
	placeholderMu.Lock()
	defer placeholderMu.Unlock()
	if b, ok := placeholderCache[maxEdge]; ok {
		return b
	}

	w := 640
	if maxEdge < 640 {
		w = maxEdge
	}
	h := w * 9 / 16

	bg := color.RGBA{30, 18, 0, 255}    // very dark amber
	band := color.RGBA{180, 110, 20, 255} // hazard amber
	stripe := color.RGBA{20, 12, 0, 255}  // dark
	fg := color.RGBA{255, 200, 60, 255}   // bright amber for text

	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetRGBA(x, y, bg)
		}
	}

	// Top + bottom hazard stripes (~15% of height each).
	stripeH := h * 15 / 100
	stripeWidth := stripeH / 2
	if stripeWidth < 8 {
		stripeWidth = 8
	}
	for y := 0; y < stripeH; y++ {
		for x := 0; x < w; x++ {
			pat := ((x + y) / stripeWidth) & 1
			c := band
			if pat == 1 {
				c = stripe
			}
			img.SetRGBA(x, y, c)
			img.SetRGBA(x, h-1-y, c)
		}
	}

	// Center the message.
	const msg = "TECHNICAL DIFFICULTIES"
	scale := w / 200
	if scale < 2 {
		scale = 2
	}
	textW := len(msg) * 6 * scale
	textH := 7 * scale
	tx := (w - textW) / 2
	ty := (h - textH) / 2
	drawText(img, msg, tx, ty, scale, fg)

	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: quality}); err != nil {
		// Should never happen on a freshly-built RGBA, but if it does,
		// return a minimal valid JPEG so callers don't choke. Empty bytes
		// would fail the teacher's JPEG-magic check.
		return nil
	}
	out := buf.Bytes()
	placeholderCache[maxEdge] = out
	return out
}

// sampleLumaBGRA computes average Rec.601 luma over a 16×16 sample grid of
// raw BGRA bytes (BEFORE the in-place swap to RGBA). Returns 0..255. Used
// as a diagnostic so the agent log records whether BitBlt itself returned
// black for each capture, completely independent of network / pipe / decode.
func sampleLumaBGRA(raw []byte, w, h int) int {
	if len(raw) == 0 || w <= 0 || h <= 0 {
		return 0
	}
	const grid = 16
	stride := w * 4
	var sum, count int64
	for gy := 0; gy < grid; gy++ {
		y := gy * h / grid
		for gx := 0; gx < grid; gx++ {
			x := gx * w / grid
			off := y*stride + x*4
			if off+3 >= len(raw) {
				continue
			}
			b := int64(raw[off+0])
			g := int64(raw[off+1])
			r := int64(raw[off+2])
			sum += (r*299 + g*587 + b*114) / 1000
			count++
		}
	}
	if count == 0 {
		return 0
	}
	return int(sum / count)
}

func captureScreenSized(maxEdge int, quality int) ([]byte, error) {
	ensureDPIAware()
	t0 := time.Now()

	desktop, _, _ := procGetDesktopWindow.Call()
	hdcScreen, _, _ := procGetDC.Call(desktop)
	if hdcScreen == 0 {
		devlog.Logf("capture: GetDC FAILED")
		return nil, fmt.Errorf("GetDC returned 0")
	}
	defer procReleaseDC.Call(desktop, hdcScreen)

	w, _, _ := procGetSystemMetrics.Call(smCxScreen)
	h, _, _ := procGetSystemMetrics.Call(smCyScreen)

	hdcMem, _, _ := procCreateCompatibleDC.Call(hdcScreen)
	if hdcMem == 0 {
		devlog.Logf("capture: CreateCompatibleDC FAILED w=%d h=%d", w, h)
		return nil, fmt.Errorf("CreateCompatibleDC returned 0")
	}
	defer procDeleteDC.Call(hdcMem)

	// CreateDIBSection allocates a bitmap whose pixel memory is mapped
	// directly into the process address space. After BitBlt the BGRA bytes
	// are sitting at *pBits — no GetDIBits call needed, no "is the bitmap
	// selected?" dance. This is what every working Win32 screen capture
	// library uses; it sidesteps the silent GetDIBits=0 failure that the
	// older code hit on this Win10 / Intel iGPU box.
	bi := bitmapInfoHeader{
		biSize:        40,
		biWidth:       int32(w),
		biHeight:      -int32(h), // negative = top-down DIB (origin top-left)
		biPlanes:      1,
		biBitCount:    32,
		biCompression: biRgb,
	}
	var pBits uintptr
	hBmp, _, _ := procCreateDIBSection.Call(
		hdcScreen,
		uintptr(unsafe.Pointer(&bi)),
		dibRgbColors,
		uintptr(unsafe.Pointer(&pBits)),
		0, 0,
	)
	if hBmp == 0 || pBits == 0 {
		gdi, user := gdiHandleCount()
		devlog.Logf("capture: CreateDIBSection FAILED w=%d h=%d gdi=%d user=%d", w, h, gdi, user)
		return nil, fmt.Errorf("CreateDIBSection returned 0")
	}
	defer procDeleteObject.Call(hBmp)

	hOld, _, _ := procSelectObject.Call(hdcMem, hBmp)
	defer procSelectObject.Call(hdcMem, hOld)

	setupMs := time.Since(t0).Milliseconds()
	tBlit := time.Now()
	rb, _, _ := procBitBlt.Call(hdcMem, 0, 0, w, h, hdcScreen, 0, 0, srcCopy|captureBlt)
	blitMs := time.Since(tBlit).Milliseconds()
	if rb == 0 {
		devlog.Logf("capture: BitBlt returned 0 w=%d h=%d", w, h)
	}

	// GdiFlush forces all pending GDI ops on this thread to complete before
	// we read pBits. Without it the BitBlt may still be queued in the driver
	// and we'd read pre-blit memory.
	procGdiFlush.Call()

	// Copy the pixels out — pBits memory is freed by DeleteObject(hBmp).
	rawSize := int(w) * int(h) * 4
	raw := make([]byte, rawSize)
	src := unsafe.Slice((*byte)(unsafe.Pointer(pBits)), rawSize)
	copy(raw, src)

	// Sample BEFORE the swap so we measure exactly what BitBlt produced.
	luma := sampleLumaBGRA(raw, int(w), int(h))

	// If BitBlt produced a zero buffer (or essentially zero), substitute the
	// "Technical difficulties" placeholder so the teacher gets a visibly
	// distinct cell rather than a thumbnail of pure black. This happens on
	// hybrid-graphics laptops where our process ends up bound to one GPU
	// while DWM composes on the other — BitBlt succeeds at the API level
	// but the driver leaves our buffer untouched.
	if luma < 2 {
		ph := makeFailurePlaceholder(maxEdge, quality)
		gdi, user := gdiHandleCount()
		devlog.Logf("capture: zero-buffer (luma=%d), sending placeholder  w=%d h=%d setup=%dms blit=%dms total=%dms jpeg=%dB gdi=%d user=%d",
			luma, w, h, setupMs, blitMs, time.Since(t0).Milliseconds(), len(ph), gdi, user)
		return ph, nil
	}

	for i := 0; i < len(raw); i += 4 {
		raw[i+0], raw[i+2] = raw[i+2], raw[i+0]
		raw[i+3] = 255
	}

	img := &image.NRGBA{Pix: raw, Stride: int(w) * 4, Rect: image.Rect(0, 0, int(w), int(h))}

	// Downscale before JPEG encode. Caller picks maxEdge: 640 for thumbnails,
	// 2400 for focus mode (text-readable).
	small := downscaleNRGBA(img, maxEdge)

	tEnc := time.Now()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, small, &jpeg.Options{Quality: quality}); err != nil {
		return nil, err
	}
	encMs := time.Since(tEnc).Milliseconds()

	gdi, user := gdiHandleCount()
	devlog.Logf("capture: w=%d h=%d luma=%d setup=%dms blit=%dms enc=%dms total=%dms jpeg=%dB gdi=%d user=%d",
		w, h, luma, setupMs, blitMs, encMs, time.Since(t0).Milliseconds(), buf.Len(), gdi, user)

	return buf.Bytes(), nil
}

// downscaleNRGBA returns a smaller copy of src such that its longer edge is
// at most maxEdge pixels. Aspect ratio preserved. Uses a fast box-filter
// (averaging) when the scale is integer, falling back to nearest-neighbour
// otherwise. No external deps.
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

	// Nearest-neighbour: fast enough for screen captures, quality is fine
	// at thumbnail size.
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
		wsExTopmost|wsExToolWindow|wsExNoActivate|wsExLayered,
		uintptr(unsafe.Pointer(className)),
		uintptr(unsafe.Pointer(utf16("ClassSend"))),
		wsPopup|wsVisible,
		notifX, notifY, notifW, notifH,
		0, 0, hInst, 0,
	)
	// Start fully opaque.
	procSetLayeredWindowAttributes.Call(hwnd, 0, 255, lwaAlpha)
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
	if notifHwnd != 0 {
		notifMu.Unlock()
		return
	}
	ready := make(chan uintptr, 1)
	go runMonitorNotifWindow(ready)
	hwnd := <-ready
	notifHwnd = hwnd
	notifMu.Unlock()

	// Auto-fade after 4 s. The banner is meant as an "I'm being watched
	// right now" alert, not a permanent badge. Fades over 600 ms (12 steps
	// of 50 ms) then closes the window. If hideMonitoringNotification is
	// called explicitly first (CmdStopMonitor) the window is gone before
	// the timer fires; the fade goroutine then sees notifHwnd == 0 and
	// exits without touching anything.
	go func(target uintptr) {
		time.Sleep(4 * time.Second)
		const steps = 12
		for i := 0; i < steps; i++ {
			notifMu.Lock()
			gone := notifHwnd != target
			notifMu.Unlock()
			if gone {
				return
			}
			alpha := uintptr(255 - 255*(i+1)/steps)
			procSetLayeredWindowAttributes.Call(target, 0, alpha, lwaAlpha)
			time.Sleep(50 * time.Millisecond)
		}
		// Fade complete — close.
		notifMu.Lock()
		stillOurs := notifHwnd == target
		notifMu.Unlock()
		if stillOurs {
			procPostMessageW.Call(target, wmClose, 0, 0)
		}
	}(hwnd)
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
// The student-side cast viewer used to be a hand-rolled Win32 GDI window
// living inside this process (~250 lines of wndproc + drawCastFrame +
// runCastViewWindow). v0.0.4-b moves it out to a separate castviewer.exe
// (WebView2-based, see cmd/castviewer). The agent just spawns and kills it.
//
// Lifecycle:
//   - CmdStartCast(addr): spawn castviewer.exe -addr <addr>. If a viewer is
//     already running for an old cast, it is killed and replaced.
//   - CmdStopCast: kill the viewer process.
//   - TypeShowCast IPC (--cast in the student TUI): respawn using the last
//     known address. If no cast is currently active, this is a no-op.

var (
	castMu       sync.Mutex
	castProc     *exec.Cmd
	lastCastAddr string
)

func showCastingViewer() {
	// Re-spawn at the last known address if available — used by the student
	// TUI's --cast command. With no prior address this is a no-op.
	castMu.Lock()
	addr := lastCastAddr
	castMu.Unlock()
	if addr == "" {
		devlog.Logf("showCastingViewer: no prior addr, ignoring")
		return
	}
	startCastViewer(addr)
}

func hideCastingViewer() {
	castMu.Lock()
	p := castProc
	castProc = nil
	castMu.Unlock()
	if p != nil && p.Process != nil {
		_ = p.Process.Kill()
		devlog.Logf("hideCastingViewer: killed pid=%d", p.Process.Pid)
	}
}

// startCastViewer launches castviewer.exe pointing at the given teacher
// address. If a previous viewer is still around (from an earlier cast or a
// stale process) it is killed first so the student never sees two windows.
func startCastViewer(addr string) {
	castMu.Lock()
	if castProc != nil && castProc.Process != nil {
		_ = castProc.Process.Kill()
	}
	lastCastAddr = addr
	castMu.Unlock()

	exePath := findCastViewerExe()
	if exePath == "" {
		devlog.Logf("startCastViewer: castviewer.exe not found")
		return
	}
	cmd := exec.Command(exePath, "-addr", addr)
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    false,
		CreationFlags: 0x00000008, // DETACHED_PROCESS
	}
	if err := cmd.Start(); err != nil {
		devlog.Logf("startCastViewer: spawn failed: %v", err)
		return
	}
	devlog.Logf("startCastViewer: spawned %s pid=%d addr=%s", exePath, cmd.Process.Pid, addr)
	castMu.Lock()
	castProc = cmd
	castMu.Unlock()

	// Reap the process so a viewer that exits on its own (TCP closed,
	// student clicked X) doesn't become a zombie. Clear castProc so the
	// next StopCast doesn't try to kill an already-dead PID.
	go func(c *exec.Cmd) {
		_ = c.Wait()
		castMu.Lock()
		if castProc == c {
			castProc = nil
		}
		castMu.Unlock()
		devlog.Logf("castviewer pid=%d exited", c.Process.Pid)
	}(cmd)
}

// findCastViewerExe looks for castviewer.exe next to the running agent
// (production install layout) and falls back to the cwd (dev layout).
func findCastViewerExe() string {
	if exe, err := os.Executable(); err == nil {
		dir := exe[:strings.LastIndexAny(exe, `/\`)]
		candidate := dir + `\castviewer.exe`
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	if _, err := os.Stat("castviewer.exe"); err == nil {
		return "castviewer.exe"
	}
	return ""
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

// ── Stress guard ──────────────────────────────────────────────────────────
//
// The agent runs on whatever the school issued — sometimes Atom-class
// hardware on classroom WiFi. Without backpressure, every incoming
// command spawned its own goroutine. When a teacher fired several
// commands in quick succession (lock + close-apps + launch + monitoring
// start) on a slow box, handlers (and the GDI/USER handles, sub-
// processes, retry loops they hold) accumulated faster than they
// completed and the agent eventually crashed on resource exhaustion.
//
// Self-protection layered in here:
//
//  1. stressSem caps concurrent handlers. Beyond the limit we don't
//     queue indefinitely — we drop the command and tell the teacher.
//  2. stressGuard returns false when full; the caller skips execution
//     and SendCmdAck reports a "BUSY" failure so the teacher SEES that
//     the agent is overloaded instead of silently losing the command.
//  3. runGuarded recovers from any panic in a handler. One bad syscall
//     can't take the whole agent down anymore — the slot is released
//     and the next command can run.
//  4. startHealthBeacon logs goroutines/heap/inflight every 30 s so a
//     post-mortem on the devlog can spot resource creep across a
//     session.
const stressInflightLimit = 4

var (
	stressSem      = make(chan struct{}, stressInflightLimit)
	stressBusyDrop atomic.Uint64
	errAgentBusy   = errors.New("agent busy — command dropped")
)

// runGuarded wraps an async command handler. Tries to acquire a slot;
// on failure sends a BUSY ack and returns. On success spawns the work
// in a goroutine that is panic-safe and releases the slot when done.
func runGuarded(app *core.App, cmd protocol.CommandPayload, fn func()) {
	select {
	case stressSem <- struct{}{}:
		// got a slot
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

// gdiHandleCount returns the current per-process GDI and USER object
// counts. If either climbs steadily across the health beacon we have a
// handle leak — Win10 caps each at 10 000 per process by default and
// hitting that cap makes the agent unable to allocate any more drawing
// resources (BitBlt fails, the agent appears hung). Pseudo-handle
// (HANDLE)-1 = current process; no real syscall needed for that.
const (
	grGDIObjects  = 0
	grUserObjects = 1
)

func gdiHandleCount() (gdi, user uint32) {
	const currentProcess = ^uintptr(0)
	g, _, _ := procGetGuiResources.Call(currentProcess, grGDIObjects)
	u, _, _ := procGetGuiResources.Call(currentProcess, grUserObjects)
	return uint32(g), uint32(u)
}

// startHealthBeacon kicks off a goroutine that periodically snapshots
// agent process state into the devlog. Cheap, low-frequency — helps a
// lot when reviewing logs after a crash.
func startHealthBeacon() {
	go func() {
		var ms runtime.MemStats
		for {
			time.Sleep(30 * time.Second)
			runtime.ReadMemStats(&ms)
			gdi, user := gdiHandleCount()
			devlog.Logf("health  goroutines=%d  heap=%d KB  inflight=%d  busy_drops=%d  gdi=%d  user=%d",
				runtime.NumGoroutine(),
				ms.HeapAlloc/1024,
				len(stressSem),
				stressBusyDrop.Load(),
				gdi, user,
			)
		}
	}()
}

func setupStudentCommands(app *core.App, devMode bool) {
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
			exec.Command("shutdown", "/s", "/f", "/t", "0").Start() //nolint:errcheck

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
			// Param "hi" => high-res capture for the teacher's focus mode.
			// Empty / anything else => normal thumbnail.
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
