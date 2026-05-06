//go:build windows

package main

import (
	"bufio"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
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

	// castFPS is the target capture rate. ffmpeg's libx264 ultrafast on a
	// modest CPU sustains 1080p30 at ~10-15% CPU; pushing past 30 buys little
	// for a classroom whiteboard use case and costs bandwidth.
	castFPS = 30

	// castGOP is the H.264 GOP size in frames (= keyframe interval). At 30
	// fps this is one keyframe per second, which means a late-joining viewer
	// waits ≤1 s for a sync point. Fragment index N is a keyframe iff
	// N % castGOP == 0 — we depend on this 1:1 mapping below.
	castGOP = 30
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

// screenSize returns the primary desktop dimensions in physical pixels.
// Called once at cast start; we don't re-query mid-stream (a resolution
// change requires restarting the cast).
func screenSize() (w, h int) {
	sw, _, _ := tGetSystemMetrics.Call(tSmCxScreen)
	sh, _, _ := tGetSystemMetrics.Call(tSmCyScreen)
	return int(sw), int(sh)
}

// captureRawInto BitBlts the desktop into the caller's pre-allocated BGRA
// buffer. The buffer must be exactly w*h*4 bytes. Reusing one buffer across
// frames avoids ~240 MB/s of GC pressure at 1080p30. Returns true on success.
func captureRawInto(buf []byte, w, h int) bool {
	if len(buf) != w*h*4 {
		return false
	}
	desktop, _, _ := tGetDesktopWindow.Call()
	hdcScreen, _, _ := tGetDC.Call(desktop)
	defer tReleaseDC.Call(desktop, hdcScreen)

	hdcMem, _, _ := tCreateCompatibleDC.Call(hdcScreen)
	defer tDeleteDC.Call(hdcMem)

	hBmp, _, _ := tCreateCompatibleBitmap.Call(hdcScreen, uintptr(w), uintptr(h))
	defer tDeleteObject.Call(hBmp)

	tSelectObject.Call(hdcMem, hBmp)
	tBitBlt.Call(hdcMem, 0, 0, uintptr(w), uintptr(h), hdcScreen, 0, 0, tSrcCopy)

	bi := tBitmapInfoHeader{
		biSize:        40,
		biWidth:       int32(w),
		biHeight:      -int32(h), // negative = top-down DIB
		biPlanes:      1,
		biBitCount:    32,
		biCompression: tBiRgb,
	}
	r, _, _ := tGetDIBits.Call(
		hdcScreen, hBmp, 0, uintptr(h),
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(unsafe.Pointer(&bi)),
		tDibRgbColors,
	)
	// GetDIBits returns BGRA in memory order, which is exactly what
	// `-pix_fmt bgra` on the ffmpeg side expects. No swap needed.
	return r != 0
}

// findFFmpegExe locates ffmpeg.exe via, in order:
//
//  1. CLASSSEND_FFMPEG env var (development override)
//  2. ffmpeg.exe next to the running classsend.exe (production install)
//  3. ffmpeg.exe on PATH (some dev / system installs)
//
// Returns "" if none found.
func findFFmpegExe() string {
	if env := os.Getenv("CLASSSEND_FFMPEG"); env != "" {
		if _, err := os.Stat(env); err == nil {
			return env
		}
	}
	if exe, err := os.Executable(); err == nil {
		candidate := filepath.Join(filepath.Dir(exe), "ffmpeg.exe")
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	if p, err := exec.LookPath("ffmpeg.exe"); err == nil {
		return p
	}
	return ""
}

// runCastCapture is the teacher-side capture + encode + broadcast loop.
//
// Pipeline:
//
//	BitBlt → BGRA buffer → ffmpeg.stdin
//	                       ffmpeg [libx264] → fMP4 chunks → ffmpeg.stdout
//	                                                        ↓
//	                                              FMP4Splitter
//	                                                        ↓
//	                                              CastServer.SendFrame(kind)
//
// Three goroutines: this function (capture → stdin), drainStderr, parseStdout.
// On stop we close stdin, which makes ffmpeg flush and exit; the parser sees
// EOF; we Wait() with a kill fallback.
func runCastCapture(srv *network.CastServer, stop <-chan struct{}) {
	ffmpegPath := findFFmpegExe()
	if ffmpegPath == "" {
		log.Printf("cast: ffmpeg.exe not found beside classsend.exe or on PATH; cast disabled")
		<-stop
		return
	}

	w, h := screenSize()
	if w == 0 || h == 0 {
		log.Printf("cast: GetSystemMetrics returned 0×0, cast disabled")
		<-stop
		return
	}
	log.Printf("cast: starting %dx%d @ %d fps via %s", w, h, castFPS, ffmpegPath)

	args := []string{
		"-hide_banner", "-loglevel", "warning",
		// Input: BGRA raw video at the captured resolution / framerate.
		"-f", "rawvideo",
		"-pix_fmt", "bgra",
		"-s", fmt.Sprintf("%dx%d", w, h),
		"-r", fmt.Sprintf("%d", castFPS),
		"-i", "pipe:0",
		// Encoder: H.264 baseline so MSE / WebView2 decodes it without fuss.
		// ultrafast + zerolatency keeps CPU low and end-to-end latency down
		// to ~1 frame on the encoder side.
		"-c:v", "libx264",
		"-preset", "ultrafast",
		"-tune", "zerolatency",
		"-bf", "0", // no B-frames — they require lookahead which adds latency
		"-g", fmt.Sprintf("%d", castGOP),
		"-keyint_min", fmt.Sprintf("%d", castGOP),
		"-profile:v", "baseline",
		"-level", "3.1",
		"-pix_fmt", "yuv420p",
		// Output: fMP4 with empty moov (init only), default base moof, one
		// fragment per frame. This means: parser sees ftyp+moov first (init
		// segment), then moof+mdat per frame. 1:1 mapping with our frame
		// counter for keyframe classification.
		"-f", "mp4",
		"-movflags", "+empty_moov+default_base_moof+frag_every_frame",
		"pipe:1",
	}
	cmd := exec.Command(ffmpegPath, args...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		log.Printf("cast: stdin pipe: %v", err)
		<-stop
		return
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		log.Printf("cast: stdout pipe: %v", err)
		<-stop
		return
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		log.Printf("cast: stderr pipe: %v", err)
		<-stop
		return
	}

	if err := cmd.Start(); err != nil {
		log.Printf("cast: ffmpeg start: %v", err)
		<-stop
		return
	}
	log.Printf("cast: ffmpeg pid=%d started", cmd.Process.Pid)

	// Goroutine 1: drain stderr to log so a chatty ffmpeg can't deadlock by
	// filling its stderr pipe.
	go drainFFmpegStderr(stderr)

	// Goroutine 2: parse fMP4 boxes off stdout, classify, hand to srv.
	parserDone := make(chan struct{})
	var (
		framesEncoded atomic.Int64
		bytesOut      atomic.Int64
	)
	go func() {
		defer close(parserDone)
		sp := network.NewFMP4Splitter(bufio.NewReaderSize(stdout, 4*1024*1024))
		var mediaIdx int64
		for {
			chunk, err := sp.NextChunk()
			if err != nil {
				if err != io.EOF {
					log.Printf("cast: fMP4 parser: %v", err)
				}
				return
			}
			bytesOut.Add(int64(len(chunk.Data)))
			if chunk.Init {
				log.Printf("cast: init segment %d bytes ready", len(chunk.Data))
				srv.SendFrame(chunk.Data, network.FrameInit)
				continue
			}
			kind := network.FrameDelta
			if mediaIdx%int64(castGOP) == 0 {
				kind = network.FrameKeyframe
			}
			srv.SendFrame(chunk.Data, kind)
			mediaIdx++
			framesEncoded.Add(1)
		}
	}()

	// Stats goroutine: every 5 s log frames-out + bytes-out + delivered + dropped.
	statsStop := make(chan struct{})
	go func() {
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		var lastFrames, lastBytes int64
		for {
			select {
			case <-statsStop:
				return
			case <-t.C:
				f := framesEncoded.Load()
				b := bytesOut.Load()
				sent, drops := srv.DrainStats()
				log.Printf("cast: stats Δframes=%d Δbytes=%d sent=%d dropped=%d clients=%d",
					f-lastFrames, b-lastBytes, sent, drops, srv.ClientCount())
				lastFrames = f
				lastBytes = b
			}
		}
	}()

	// Pre-allocated capture buffer — reused every frame so we don't allocate
	// w*h*4 bytes (8 MB at 1080p) per frame and crush the GC.
	buf := make([]byte, w*h*4)

	frameBudget := time.Second / time.Duration(castFPS)
	ticker := time.NewTicker(frameBudget)
	defer ticker.Stop()

captureLoop:
	for {
		select {
		case <-stop:
			break captureLoop
		case <-ticker.C:
		}
		if !captureRawInto(buf, w, h) {
			continue
		}
		// stdin.Write may block if ffmpeg's input buffer is full. With
		// ultrafast preset that's rare; if it happens, we naturally throttle
		// the capture loop, which is the right behavior — dropping input
		// frames would just cause the encoder to encode duplicates.
		if _, err := stdin.Write(buf); err != nil {
			log.Printf("cast: stdin write: %v (ffmpeg likely exited)", err)
			break captureLoop
		}
	}

	// Shutdown sequence:
	//  1. Close stdin → ffmpeg sees EOF, flushes remaining frames, exits.
	//  2. Wait for parser goroutine to drain remaining boxes.
	//  3. Wait for ffmpeg to exit, with a timeout fallback to Kill.
	close(statsStop)
	_ = stdin.Close()

	select {
	case <-parserDone:
	case <-time.After(3 * time.Second):
		log.Printf("cast: parser did not drain in 3s, forcing kill")
	}

	waitDone := make(chan error, 1)
	go func() { waitDone <- cmd.Wait() }()
	select {
	case err := <-waitDone:
		log.Printf("cast: ffmpeg exited: %v", err)
	case <-time.After(2 * time.Second):
		log.Printf("cast: ffmpeg did not exit in 2s, killing pid=%d", cmd.Process.Pid)
		_ = cmd.Process.Kill()
		<-waitDone
	}
}

// drainFFmpegStderr reads ffmpeg's stderr line-by-line and forwards each line
// to log. Keeps the pipe drained (a full pipe deadlocks ffmpeg) and surfaces
// encoder diagnostics in the teacher log.
func drainFFmpegStderr(r io.Reader) {
	br := bufio.NewReader(r)
	for {
		line, err := br.ReadString('\n')
		if line != "" {
			line = strings.TrimRight(line, "\r\n")
			if line != "" {
				log.Printf("cast: ffmpeg stderr: %s", line)
			}
		}
		if err != nil {
			return
		}
	}
}
