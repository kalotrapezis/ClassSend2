//go:build linux

// Linux teacher-side screen casting.
//
// The cross-platform half of the pipeline (fMP4 box splitting in
// internal/network, fragment classification, CastServer fan-out) is identical
// to Windows. The only platform piece is *capture + encode*, and on Linux we
// let ffmpeg do both: it grabs the X11 root window directly and emits the same
// fragmented-MP4 (empty_moov + frag_every_frame) stream the parser expects.
//
// Capture support by session:
//
//   - X11 (Mint/Debian Cinnamon·MATE·XFCE, Fedora GNOME-on-X11): ffmpeg
//     -f x11grab grabs the whole screen. Fully works.
//   - Wayland (Fedora KDE/GNOME default): compositors don't expose the screen
//     to outside grabbers; the only route is the xdg-desktop-portal/PipeWire
//     ScreenCast API, which pops an interactive "share your screen?" consent
//     dialog — unworkable for an unattended classroom cast. We detect Wayland
//     and bail with a clear log instead of streaming a black/partial frame.
//
// Encoder: prefer libx264 (Debian/Mint, or Fedora with RPM Fusion's full
// ffmpeg). Fall back to libopenh264 — the baseline H.264 encoder Fedora ships
// by default in ffmpeg-free — which the mpv/MSE viewers decode fine. Both are
// driven to H.264 baseline + per-frame fragments so the wire format is
// byte-for-byte what the Windows teacher produces.

package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"time"

	"classsend/internal/devlog"
	"classsend/internal/network"
)

const (
	castFPS = 30
	castGOP = 30 // one keyframe per second; fragment N is a keyframe iff N%castGOP==0
)

func castSessionIsWayland() bool {
	return strings.EqualFold(os.Getenv("XDG_SESSION_TYPE"), "wayland") ||
		(os.Getenv("WAYLAND_DISPLAY") != "" && os.Getenv("DISPLAY") == "")
}

// findFFmpeg locates the ffmpeg binary: CLASSSEND_FFMPEG override, then PATH.
func findFFmpeg() string {
	if env := os.Getenv("CLASSSEND_FFMPEG"); env != "" {
		if _, err := os.Stat(env); err == nil {
			return env
		}
	}
	if p, err := exec.LookPath("ffmpeg"); err == nil {
		return p
	}
	return ""
}

// pickH264Encoder returns the encoder name and its encoder-specific ffmpeg args
// (everything except -c:v). Prefers libx264; falls back to libopenh264.
func pickH264Encoder(ffmpeg string) (name string, args []string, ok bool) {
	out, err := exec.Command(ffmpeg, "-hide_banner", "-encoders").Output()
	if err != nil {
		return "", nil, false
	}
	enc := string(out)
	switch {
	case strings.Contains(enc, "libx264"):
		return "libx264", []string{
			"-preset", "ultrafast",
			"-tune", "zerolatency",
			"-bf", "0",
			"-g", fmt.Sprintf("%d", castGOP),
			"-keyint_min", fmt.Sprintf("%d", castGOP),
			"-profile:v", "baseline",
			"-level", "3.1",
			"-pix_fmt", "yuv420p",
		}, true
	case strings.Contains(enc, "libopenh264"):
		// openh264 is bitrate-driven (no CRF/preset). 4 Mbit is plenty for a
		// classroom desktop and keeps a software encode light.
		return "libopenh264", []string{
			"-profile:v", "constrained_baseline",
			"-g", fmt.Sprintf("%d", castGOP),
			"-b:v", "4M",
			"-pix_fmt", "yuv420p",
		}, true
	}
	return "", nil, false
}

// runCastCapture: ffmpeg x11grab → H.264 → fMP4 on stdout → splitter → CastServer.
func runCastCapture(srv *network.CastServer, stop <-chan struct{}) {
	if castSessionIsWayland() {
		devlog.Logf("cast: screen capture is not supported on Wayland — the teacher " +
			"must run an X11 session (log out and pick 'X11'/'Xorg' at login). Cast disabled.")
		<-stop
		return
	}

	ffmpeg := findFFmpeg()
	if ffmpeg == "" {
		devlog.Logf("cast: ffmpeg not found on PATH (Debian/Mint: apt install ffmpeg; " +
			"Fedora: dnf install ffmpeg, or ffmpeg-free for libopenh264). Cast disabled.")
		<-stop
		return
	}

	encName, encArgs, ok := pickH264Encoder(ffmpeg)
	if !ok {
		devlog.Logf("cast: no H.264 encoder in this ffmpeg (need libx264 or libopenh264). Cast disabled.")
		<-stop
		return
	}

	display := os.Getenv("DISPLAY")
	if display == "" {
		display = ":0.0"
	}
	devlog.Logf("cast: starting x11grab display=%s @ %d fps via %s (%s)", display, castFPS, ffmpeg, encName)

	args := []string{"-hide_banner", "-loglevel", "warning",
		"-f", "x11grab", "-framerate", fmt.Sprintf("%d", castFPS), "-i", display,
		"-c:v", encName}
	args = append(args, encArgs...)
	args = append(args,
		"-f", "mp4",
		"-movflags", "+empty_moov+default_base_moof+frag_every_frame",
		"pipe:1")

	cmd := exec.Command(ffmpeg, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		devlog.Logf("cast: stdout pipe: %v", err)
		<-stop
		return
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		devlog.Logf("cast: stderr pipe: %v", err)
		<-stop
		return
	}
	if err := cmd.Start(); err != nil {
		devlog.Logf("cast: ffmpeg start: %v", err)
		<-stop
		return
	}
	devlog.Logf("cast: ffmpeg pid=%d started", cmd.Process.Pid)

	go drainCastStderr(stderr)

	parserDone := make(chan struct{})
	var framesEncoded atomic.Int64
	go func() {
		defer close(parserDone)
		sp := network.NewFMP4Splitter(bufio.NewReaderSize(stdout, 4*1024*1024))
		var mediaIdx int64
		for {
			chunk, err := sp.NextChunk()
			if err != nil {
				if err != io.EOF {
					devlog.Logf("cast: fMP4 parser: %v", err)
				}
				return
			}
			if chunk.Init {
				devlog.Logf("cast: init segment %d bytes ready", len(chunk.Data))
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

	// Wait for stop, then tear down: ffmpeg has no stdin to close, so signal it
	// with SIGINT (clean flush) and fall back to Kill.
	<-stop
	if cmd.Process != nil {
		_ = cmd.Process.Signal(os.Interrupt)
	}

	select {
	case <-parserDone:
	case <-time.After(3 * time.Second):
		devlog.Logf("cast: parser did not drain in 3s, forcing kill")
	}

	waitDone := make(chan error, 1)
	go func() { waitDone <- cmd.Wait() }()
	select {
	case err := <-waitDone:
		devlog.Logf("cast: ffmpeg exited: %v (frames=%d)", err, framesEncoded.Load())
	case <-time.After(2 * time.Second):
		devlog.Logf("cast: ffmpeg did not exit in 2s, killing pid=%d", cmd.Process.Pid)
		_ = cmd.Process.Kill()
		<-waitDone
	}
}

func drainCastStderr(r io.Reader) {
	br := bufio.NewReader(r)
	for {
		line, err := br.ReadString('\n')
		if line != "" {
			if line = strings.TrimRight(line, "\r\n"); line != "" {
				devlog.Logf("cast: ffmpeg stderr: %s", line)
			}
		}
		if err != nil {
			return
		}
	}
}
