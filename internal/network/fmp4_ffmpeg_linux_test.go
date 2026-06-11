//go:build linux

package network

import (
	"bufio"
	"bytes"
	"os/exec"
	"testing"
)

// Integration check for the Linux teacher cast path: drive this machine's real
// ffmpeg with the exact encode/fragment settings runCastCapture uses, and feed
// its fMP4 output through FMP4Splitter. Confirms the splitter recognises a real
// init segment followed by per-frame media fragments — i.e. the wire format the
// student viewer expects is actually produced here.
//
// Skips cleanly if ffmpeg or an H.264 encoder isn't installed (CI / minimal
// boxes), so it never fails for environmental reasons.
func TestFMP4Splitter_RealFFmpegOutput(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not installed")
	}

	// Match runCastCapture's encoder preference: libx264, else libopenh264.
	encOut, _ := exec.Command(ffmpeg, "-hide_banner", "-encoders").Output()
	var encArgs []string
	switch {
	case bytes.Contains(encOut, []byte("libx264")):
		encArgs = []string{"-c:v", "libx264", "-preset", "ultrafast", "-tune", "zerolatency",
			"-bf", "0", "-g", "30", "-keyint_min", "30", "-profile:v", "baseline",
			"-level", "3.1", "-pix_fmt", "yuv420p"}
	case bytes.Contains(encOut, []byte("libopenh264")):
		encArgs = []string{"-c:v", "libopenh264", "-profile:v", "constrained_baseline",
			"-g", "30", "-b:v", "2M", "-pix_fmt", "yuv420p"}
	default:
		t.Skip("no libx264/libopenh264 encoder available")
	}

	args := []string{"-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc=size=640x360:rate=30", "-t", "1"}
	args = append(args, encArgs...)
	args = append(args, "-f", "mp4",
		"-movflags", "+empty_moov+default_base_moof+frag_every_frame", "pipe:1")

	out, err := exec.Command(ffmpeg, args...).Output()
	if err != nil {
		t.Skipf("ffmpeg encode failed (codec/runtime issue, not our parser): %v", err)
	}
	if len(out) == 0 {
		t.Fatal("ffmpeg produced no output")
	}

	sp := NewFMP4Splitter(bufio.NewReader(bytes.NewReader(out)))
	var inits, fragments int
	for {
		chunk, err := sp.NextChunk()
		if err != nil {
			break
		}
		if chunk.Init {
			inits++
		} else {
			fragments++
		}
		if len(chunk.Data) == 0 {
			t.Fatal("splitter returned an empty chunk")
		}
	}

	if inits != 1 {
		t.Errorf("want exactly 1 init segment, got %d", inits)
	}
	if fragments < 10 {
		t.Errorf("want many media fragments for 1s@30fps, got %d", fragments)
	}
	t.Logf("parsed %d init + %d media fragments from real ffmpeg fMP4", inits, fragments)
}
