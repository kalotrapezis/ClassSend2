//go:build windows

package main

import (
	"bytes"
	"image"
	"image/jpeg"
	"syscall"
	"time"
	"unsafe"

	"classsend/internal/network"
)

var (
	tUser32 = syscall.NewLazyDLL("user32.dll")
	tGdi32  = syscall.NewLazyDLL("gdi32.dll")

	tGetDesktopWindow       = tUser32.NewProc("GetDesktopWindow")
	tGetDC                  = tUser32.NewProc("GetDC")
	tReleaseDC              = tUser32.NewProc("ReleaseDC")
	tGetSystemMetrics       = tUser32.NewProc("GetSystemMetrics")
	tCreateCompatibleDC     = tGdi32.NewProc("CreateCompatibleDC")
	tCreateCompatibleBitmap = tGdi32.NewProc("CreateCompatibleBitmap")
	tSelectObject           = tGdi32.NewProc("SelectObject")
	tBitBlt                 = tGdi32.NewProc("BitBlt")
	tDeleteObject           = tGdi32.NewProc("DeleteObject")
	tDeleteDC               = tGdi32.NewProc("DeleteDC")
	tGetDIBits              = tGdi32.NewProc("GetDIBits")
)

const (
	tSmCxScreen   = 0
	tSmCyScreen   = 1
	tSrcCopy      = 0x00CC0020
	tDibRgbColors = 0
	tBiRgb        = 0
)

type tBitmapInfoHeader struct {
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

// captureRaw captures the screen at native resolution and returns raw BGRA pixels.
func captureRaw() (pix []byte, w, h int, err error) {
	desktop, _, _ := tGetDesktopWindow.Call()
	hdcScreen, _, _ := tGetDC.Call(desktop)
	defer tReleaseDC.Call(desktop, hdcScreen)

	sw, _, _ := tGetSystemMetrics.Call(tSmCxScreen)
	sh, _, _ := tGetSystemMetrics.Call(tSmCyScreen)

	hdcMem, _, _ := tCreateCompatibleDC.Call(hdcScreen)
	defer tDeleteDC.Call(hdcMem)

	hBmp, _, _ := tCreateCompatibleBitmap.Call(hdcScreen, sw, sh)
	defer tDeleteObject.Call(hBmp)

	tSelectObject.Call(hdcMem, hBmp)
	tBitBlt.Call(hdcMem, 0, 0, sw, sh, hdcScreen, 0, 0, tSrcCopy)

	bi := tBitmapInfoHeader{
		biSize:        40,
		biWidth:       int32(sw),
		biHeight:      -int32(sh),
		biPlanes:      1,
		biBitCount:    32,
		biCompression: tBiRgb,
	}

	raw := make([]byte, sw*sh*4)
	tGetDIBits.Call(
		hdcScreen, hBmp, 0, sh,
		uintptr(unsafe.Pointer(&raw[0])),
		uintptr(unsafe.Pointer(&bi)),
		tDibRgbColors,
	)

	// BGRA → RGBA swap + set alpha
	for i := 0; i < len(raw); i += 4 {
		raw[i+0], raw[i+2] = raw[i+2], raw[i+0]
		raw[i+3] = 255
	}
	return raw, int(sw), int(sh), nil
}

// runCastCapture is the teacher-side capture + broadcast loop.
// It adapts JPEG quality based on the server's frame drop rate.
//
// Adaptive quality rules (sampled every 60 frames):
//   drop rate > 30% → quality -= 5  (min 50)
//   drop rate > 10% → quality -= 2
//   drop rate == 0  → quality += 3  (max 90)
//
// This keeps the stream at the highest quality the network can sustain without
// building up a queue (which would cause lag).
func runCastCapture(srv *network.CastServer, stop <-chan struct{}) {
	const (
		targetFPS  = 30
		frameBudget = time.Second / targetFPS

		qualityMin  = 50
		qualityMax  = 90
		qualityInit = 85
	)

	quality := qualityInit
	ticker := time.NewTicker(frameBudget)
	defer ticker.Stop()

	var (
		totalSent    int64
		totalDropped int64
		sampleFrame  int
	)

	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
		}

		pix, w, h, err := captureRaw()
		if err != nil {
			continue
		}

		img := &image.NRGBA{Pix: pix, Stride: w * 4, Rect: image.Rect(0, 0, w, h)}
		var buf bytes.Buffer
		if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: quality}); err != nil {
			continue
		}

		frame := buf.Bytes()
		d, dr := srv.SendFrame(frame)
		totalSent += int64(d)
		totalDropped += int64(dr)
		sampleFrame++

		// Adapt quality every 60 frames (~2 seconds at 30 FPS)
		if sampleFrame >= 60 {
			total := totalSent + totalDropped
			var dropRate float64
			if total > 0 {
				dropRate = float64(totalDropped) / float64(total)
			}
			switch {
			case dropRate > 0.30:
				quality -= 5
			case dropRate > 0.10:
				quality -= 2
			case dropRate == 0 && srv.ClientCount() > 0:
				quality += 3
			}
			if quality < qualityMin {
				quality = qualityMin
			}
			if quality > qualityMax {
				quality = qualityMax
			}
			totalSent = 0
			totalDropped = 0
			sampleFrame = 0
		}
	}
}
