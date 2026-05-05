// casttest — drives the cast pipeline end-to-end without a real teacher or
// real students. Spins up a fake CastServer, generates synthetic JPEG frames
// at ~20 fps, and spawns N castviewer.exe processes pointing at it. Useful
// for verifying the WebView2 viewer works under the same fan-out the real
// teacher would produce.
//
// Usage:
//
//	casttest                             # 3 viewers, default port, runs until Ctrl-C
//	casttest -n 3 -duration 30s          # auto-quit after 30 s
//	casttest -exe ./castviewer.exe       # specific viewer binary
//	casttest -port 47821 -fps 20         # tweak server / frame rate
//
//go:build windows

package main

import (
	"bytes"
	"flag"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"log"
	"net"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"classsend/internal/network"
)

func main() {
	port := flag.Int("port", network.CastPort, "cast server port")
	exePath := flag.String("exe", "castviewer.exe", "path to castviewer.exe")
	n := flag.Int("n", 3, "number of viewer processes to spawn")
	fps := flag.Int("fps", 20, "frame rate")
	duration := flag.Duration("duration", 0, "auto-quit after this long (0 = forever)")
	w := flag.Int("w", 1280, "frame width")
	h := flag.Int("h", 720, "frame height")
	flag.Parse()

	log.SetFlags(log.LstdFlags | log.Lmicroseconds)

	// Use the project's CastServer rather than re-implementing — tests the
	// real fan-out path. NewCastServer listens on :CastPort by default; we
	// reach into the package's exported listener via a dial-test.
	srv, err := network.NewCastServer()
	if err != nil {
		log.Fatalf("NewCastServer: %v", err)
	}
	defer srv.Close()
	log.Printf("cast server listening, ClientCount=%d", srv.ClientCount())

	addr := fmt.Sprintf("127.0.0.1:%d", *port)

	// Spawn N viewer processes, each pointing at our server.
	procs := make([]*exec.Cmd, 0, *n)
	for i := 0; i < *n; i++ {
		title := fmt.Sprintf("CastViewer #%d", i+1)
		cmd := exec.Command(*exePath, "-addr", addr, "-title", title)
		cmd.SysProcAttr = &syscall.SysProcAttr{
			HideWindow:    false,
			CreationFlags: 0x00000008, // DETACHED_PROCESS
		}
		if err := cmd.Start(); err != nil {
			log.Printf("spawn viewer #%d: %v", i+1, err)
			continue
		}
		log.Printf("spawned viewer #%d pid=%d", i+1, cmd.Process.Pid)
		procs = append(procs, cmd)
	}
	defer func() {
		for _, p := range procs {
			_ = p.Process.Kill()
		}
	}()

	// Wait for clients to connect.
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) && srv.ClientCount() < len(procs) {
		time.Sleep(100 * time.Millisecond)
	}
	log.Printf("clients connected: %d / %d", srv.ClientCount(), len(procs))

	// Frame producer.
	frameInterval := time.Second / time.Duration(*fps)
	ticker := time.NewTicker(frameInterval)
	defer ticker.Stop()

	statsTicker := time.NewTicker(2 * time.Second)
	defer statsTicker.Stop()

	var auto <-chan time.Time
	if *duration > 0 {
		auto = time.After(*duration)
	}

	frame := 0
	gen := newFrameGen(*w, *h)
	for {
		select {
		case <-auto:
			log.Printf("duration reached")
			return
		case <-ticker.C:
			frame++
			data := gen.render(frame)
			delivered, dropped := srv.SendFrame(data)
			if frame%(*fps) == 0 {
				log.Printf("frame %d sent  size=%dB  delivered=%d  dropped=%d  clients=%d",
					frame, len(data), delivered, dropped, srv.ClientCount())
			}
		case <-statsTicker.C:
			sent, drops := srv.DrainStats()
			log.Printf("stats: sent=%d dropped=%d clients=%d", sent, drops, srv.ClientCount())
		}

		// Detect early viewer crashes — if any process exited the test should
		// surface that, not silently keep streaming to fewer clients.
		for i, p := range procs {
			select {
			case <-procDone(p):
				log.Printf("WARNING: viewer #%d (pid %d) exited unexpectedly", i+1, p.Process.Pid)
			default:
			}
		}
	}
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

// ── Synthetic frame generator ─────────────────────────────────────────────────

type frameGen struct {
	w, h int
	tmpl *image.RGBA
}

func newFrameGen(w, h int) *frameGen {
	return &frameGen{w: w, h: h, tmpl: image.NewRGBA(image.Rect(0, 0, w, h))}
}

// render draws a rotating-gradient + frame counter + clock so each frame is
// visually distinct (so we can tell live frames apart from a frozen image).
func (g *frameGen) render(frame int) []byte {
	img := g.tmpl
	hue := (frame * 3) % 360
	r0, g0, b0 := hsv(hue, 0.55, 0.45)
	r1, g1, b1 := hsv((hue+45)%360, 0.55, 0.20)
	w, h := g.w, g.h
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			t := float64((x+y+frame*8)%(w+h)) / float64(w+h)
			img.SetRGBA(x, y, color.RGBA{
				lerp(r0, r1, t), lerp(g0, g1, t), lerp(b0, b1, t), 255,
			})
		}
	}
	scale := w / 30
	if scale < 6 {
		scale = 6
	}
	caption := fmt.Sprintf("CAST %d", frame)
	drawString(img, caption, scale*2, scale*2, scale, color.RGBA{255, 255, 255, 255})
	stamp := time.Now().Format("15:04:05.000")
	drawString(img, stamp, scale*2, h-scale*9, scale, color.RGBA{220, 220, 220, 255})

	var buf bytes.Buffer
	_ = jpeg.Encode(&buf, img, &jpeg.Options{Quality: 80})
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

var glyphs = map[rune][7]byte{
	' ': {0, 0, 0, 0, 0, 0, 0},
	':': {0x00, 0x04, 0x00, 0x00, 0x04, 0x00, 0x00},
	'.': {0x00, 0x00, 0x00, 0x00, 0x00, 0x04, 0x00},
	'#': {0x0A, 0x1F, 0x0A, 0x1F, 0x0A, 0x00, 0x00},
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
	'C': {0x0E, 0x11, 0x10, 0x10, 0x10, 0x11, 0x0E},
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
	_ = os.Stderr
	_ = net.IPv4zero
}
