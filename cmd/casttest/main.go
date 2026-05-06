// casttest — drives the v0.0.6 H.264 cast pipeline end-to-end without a real
// teacher or real students. Instead of BitBlt screen capture, this spawns an
// ffmpeg with a synthetic test pattern (`-f lavfi -i testsrc`) and runs the
// rest of the pipeline (encoder → fMP4 splitter → CastServer → viewers)
// exactly as the production teacher does. Useful for:
//
//   - regression-testing the wire layer without a real desktop session
//   - verifying that N castviewer.exe processes can render the same stream
//   - measuring drop counts under controlled conditions
//
// Usage:
//
//	casttest                              # 3 viewers, default port, runs until Ctrl-C
//	casttest -n 3 -duration 30s           # auto-quit after 30 s
//	casttest -exe ./castviewer.exe        # specific viewer binary
//	casttest -port 47821                  # tweak server port
//	casttest -ffmpeg "C:\path\to\ffmpeg.exe"
//
//go:build windows

package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"classsend/internal/network"
)

const (
	castFPS = 30
	castGOP = 30 // keyframe every 30 frames = 1 second
)

func main() {
	port := flag.Int("port", network.CastPort, "cast server port")
	exePath := flag.String("exe", "castviewer.exe", "path to castviewer.exe")
	n := flag.Int("n", 3, "number of viewer processes to spawn")
	duration := flag.Duration("duration", 0, "auto-quit after this long (0 = forever)")
	w := flag.Int("w", 1280, "frame width")
	h := flag.Int("h", 720, "frame height")
	ffmpegArg := flag.String("ffmpeg", "", "path to ffmpeg.exe (auto-detect if empty)")
	flag.Parse()

	log.SetFlags(log.LstdFlags | log.Lmicroseconds)

	ffmpegPath := *ffmpegArg
	if ffmpegPath == "" {
		ffmpegPath = findFFmpegExe()
	}
	if ffmpegPath == "" {
		log.Fatalf("ffmpeg.exe not found — install it (winget install Gyan.FFmpeg) or pass -ffmpeg <path>")
	}
	log.Printf("ffmpeg: %s", ffmpegPath)

	srv, err := network.NewCastServer()
	if err != nil {
		log.Fatalf("NewCastServer: %v", err)
	}
	defer srv.Close()
	log.Printf("cast server listening on :%d", *port)

	// Spawn ffmpeg with lavfi testsrc as input. The flags after `-i` mirror
	// production exactly so the wire/encoder side is the same code path the
	// real teacher uses. testsrc draws a moving counter + colour bars so a
	// human watching the viewer can verify motion and timing.
	args := []string{
		"-hide_banner", "-loglevel", "warning",
		// `-re` paces lavfi at real-time — without it ffmpeg generates as
		// fast as the CPU can encode, which gives unrealistic stats and
		// can swamp the per-client queue. Production runs from BitBlt
		// which is naturally rate-limited; only the synthetic-source
		// path needs this flag.
		"-re",
		"-f", "lavfi",
		"-i", fmt.Sprintf("testsrc=size=%dx%d:rate=%d", *w, *h, castFPS),
		"-c:v", "libx264",
		"-preset", "ultrafast",
		"-tune", "zerolatency",
		"-bf", "0",
		"-g", fmt.Sprintf("%d", castGOP),
		"-keyint_min", fmt.Sprintf("%d", castGOP),
		"-profile:v", "baseline",
		"-level", "3.1",
		"-pix_fmt", "yuv420p",
		"-f", "mp4",
		"-movflags", "+empty_moov+default_base_moof+frag_every_frame",
		"pipe:1",
	}
	cmd := exec.Command(ffmpegPath, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		log.Fatalf("ffmpeg stdout pipe: %v", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		log.Fatalf("ffmpeg stderr pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		log.Fatalf("ffmpeg start: %v", err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()
	log.Printf("ffmpeg pid=%d started", cmd.Process.Pid)

	// Drain stderr so a chatty ffmpeg can't deadlock its output pipe.
	go func() {
		br := bufio.NewReader(stderr)
		for {
			line, err := br.ReadString('\n')
			if line != "" {
				log.Printf("ffmpeg: %s", strings.TrimRight(line, "\r\n"))
			}
			if err != nil {
				return
			}
		}
	}()

	// Parser goroutine: reads fMP4 boxes and forwards as init/keyframe/delta.
	var (
		framesEncoded atomic.Int64
		bytesOut      atomic.Int64
	)
	parserDone := make(chan struct{})
	go func() {
		defer close(parserDone)
		sp := network.NewFMP4Splitter(bufio.NewReaderSize(stdout, 4*1024*1024))
		var mediaIdx int64
		for {
			chunk, err := sp.NextChunk()
			if err != nil {
				if err != io.EOF {
					log.Printf("parser: %v", err)
				}
				return
			}
			bytesOut.Add(int64(len(chunk.Data)))
			if chunk.Init {
				log.Printf("init segment: %d bytes", len(chunk.Data))
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

	// Wait until init segment is published before spawning viewers — they'll
	// hold in CastServer.acceptLoop otherwise but it's cleaner to launch them
	// when they can connect immediately.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && framesEncoded.Load() < 1 {
		time.Sleep(20 * time.Millisecond)
	}
	if framesEncoded.Load() < 1 {
		log.Fatalf("ffmpeg did not produce any frames in 5 s — encoder problem?")
	}

	// Spawn N viewer processes pointing at the local cast server.
	addr := fmt.Sprintf("127.0.0.1:%d", *port)
	procs := make([]*exec.Cmd, 0, *n)
	for i := 0; i < *n; i++ {
		title := fmt.Sprintf("CastViewer #%d", i+1)
		c := exec.Command(*exePath, "-addr", addr, "-title", title)
		c.SysProcAttr = &syscall.SysProcAttr{HideWindow: false, CreationFlags: 0x00000008} // DETACHED_PROCESS
		if err := c.Start(); err != nil {
			log.Printf("spawn viewer #%d: %v", i+1, err)
			continue
		}
		log.Printf("spawned viewer #%d pid=%d", i+1, c.Process.Pid)
		procs = append(procs, c)
	}
	defer func() {
		for _, p := range procs {
			_ = p.Process.Kill()
		}
	}()

	// Wait for clients to connect.
	connDeadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(connDeadline) && srv.ClientCount() < len(procs) {
		time.Sleep(100 * time.Millisecond)
	}
	log.Printf("clients connected: %d / %d", srv.ClientCount(), len(procs))

	// Stats + lifetime loop.
	statsTicker := time.NewTicker(2 * time.Second)
	defer statsTicker.Stop()

	var auto <-chan time.Time
	if *duration > 0 {
		auto = time.After(*duration)
	}

	var (
		lastFrames int64
		lastBytes  int64
		totalSent  int64
		totalDrops int64
	)

	for {
		select {
		case <-auto:
			log.Printf("duration reached — final: encoded=%d bytes=%d delivered=%d dropped=%d",
				framesEncoded.Load(), bytesOut.Load(), totalSent, totalDrops)
			return
		case <-parserDone:
			log.Printf("parser exited (ffmpeg died?) — final: encoded=%d", framesEncoded.Load())
			return
		case <-statsTicker.C:
			f := framesEncoded.Load()
			b := bytesOut.Load()
			sent, drops := srv.DrainStats()
			totalSent += sent
			totalDrops += drops
			log.Printf("Δframes=%d Δbytes=%d delivered=%d dropped=%d clients=%d",
				f-lastFrames, b-lastBytes, sent, drops, srv.ClientCount())
			lastFrames = f
			lastBytes = b
			for i, p := range procs {
				select {
				case <-procDone(p):
					log.Printf("WARNING: viewer #%d (pid %d) exited unexpectedly", i+1, p.Process.Pid)
				default:
				}
			}
		}
	}
}

// findFFmpegExe — same fallback chain as the teacher producer.
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

// procDone returns a channel that closes when the process exits. We cache
// these because Wait() can only be called once per process.
var (
	doneMu    sync.Mutex
	doneChans = map[*exec.Cmd]chan struct{}{}
)

func procDone(c *exec.Cmd) <-chan struct{} {
	doneMu.Lock()
	defer doneMu.Unlock()
	if ch, ok := doneChans[c]; ok {
		return ch
	}
	ch := make(chan struct{})
	doneChans[c] = ch
	go func() {
		_ = c.Wait()
		close(ch)
	}()
	return ch
}
