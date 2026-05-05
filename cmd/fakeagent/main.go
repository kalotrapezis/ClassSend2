// fakeagent — synthetic student for testing the teacher + monitoring grid
// without four real PCs. Connects directly to the teacher's TCP server,
// completes the handshake, and replies to CmdRequestShot with a generated
// JPEG (gradient + hostname + frame counter).
//
// Usage:
//
//	fakeagent -teacher 127.0.0.1:47820 -name FakePC1 -mac aa:bb:cc:dd:ee:01
//
// To simulate three students, start three of these in parallel with
// different -name and -mac values, then run teacher.exe and `--t tvon`.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"log"
	"math/rand"
	"net"
	"os"
	"sync/atomic"
	"time"

	"classsend/internal/protocol"
)

func main() {
	teacherAddr := flag.String("teacher", "127.0.0.1:47820", "teacher server address")
	name := flag.String("name", "FakePC", "hostname / display name")
	mac := flag.String("mac", "", "MAC address (random if empty)")
	w := flag.Int("w", 1280, "synthetic screenshot width")
	h := flag.Int("h", 720, "synthetic screenshot height")
	hueDeg := flag.Int("hue", -1, "background hue in degrees (random if <0)")
	flag.Parse()

	if *mac == "" {
		b := make([]byte, 6)
		rand.Read(b)
		*mac = fmt.Sprintf("%02x:%02x:%02x:%02x:%02x:%02x", b[0], b[1], b[2], b[3], b[4], b[5])
	}
	hue := *hueDeg
	if hue < 0 {
		hue = rand.Intn(360)
	}

	log.Printf("fakeagent %s mac=%s hue=%d → %s", *name, *mac, hue, *teacherAddr)

	for {
		if err := runOne(*teacherAddr, *name, *mac, *w, *h, hue); err != nil {
			log.Printf("session ended: %v — reconnecting in 3 s", err)
			time.Sleep(3 * time.Second)
			continue
		}
		return
	}
}

func runOne(teacherAddr, name, mac string, w, h, hue int) error {
	conn, err := net.DialTimeout("tcp", teacherAddr, 5*time.Second)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer conn.Close()

	c := protocol.NewConn(conn)

	hs, err := protocol.Encode(protocol.TypeHandshake, protocol.HandshakePayload{
		MAC:      mac,
		Hostname: name,
		Nickname: "",
		Role:     "student",
	})
	if err != nil {
		return err
	}
	if err := c.Send(hs); err != nil {
		return fmt.Errorf("send hs: %w", err)
	}

	ack, err := c.Recv()
	if err != nil {
		return fmt.Errorf("recv ack: %w", err)
	}
	if ack.Type != protocol.TypeAck {
		return fmt.Errorf("expected ACK, got %s", ack.Type)
	}
	log.Printf("[%s] connected", name)

	var frame atomic.Uint32

	for {
		msg, err := c.Recv()
		if err != nil {
			return fmt.Errorf("recv: %w", err)
		}
		switch msg.Type {
		case protocol.TypeCommand:
			cmd, err := protocol.Decode[protocol.CommandPayload](msg)
			if err != nil {
				continue
			}
			if cmd.Action != protocol.CmdRequestShot {
				// Ack other commands as ok so the teacher's queue clears.
				ackMsg, _ := protocol.Encode(protocol.TypeCmdAck, protocol.CmdAckPayload{
					CmdID:  cmd.CmdID,
					Action: cmd.Action,
					OK:     true,
				})
				_ = c.Send(ackMsg)
				continue
			}
			f := frame.Add(1)
			jpegBytes := renderShot(name, hue, w, h, int(f), cmd.Param == "hi")
			shot, _ := protocol.Encode(protocol.TypeScreenshot, protocol.ScreenshotPayload{
				StudentID: name, // hostname-keyed, matches waitForShot()
				Data:      jpegBytes,
			})
			if err := c.Send(shot); err != nil {
				return fmt.Errorf("send shot: %w", err)
			}
			log.Printf("[%s] shot #%d sent (%d B, hi=%v)", name, f, len(jpegBytes), cmd.Param == "hi")
		case protocol.TypeChat, protocol.TypeState, protocol.TypeRoster, protocol.TypePin, protocol.TypeDelete:
			// silently ignore
		default:
			// ignore other message types
		}
	}
}

// renderShot generates a synthetic screenshot: hue-tinted background with
// a frame counter and the hostname stamped large enough to read from a
// thumbnail. Each fakeagent gets a different hue so the cells are visually
// distinguishable in the monitoring grid.
func renderShot(name string, hue, w, h, frame int, hi bool) []byte {
	if hi {
		w *= 2
		h *= 2
	}
	img := image.NewRGBA(image.Rect(0, 0, w, h))

	// Diagonal-gradient fill — animates with frame so we can confirm the
	// teacher is actually pulling fresh frames, not a cached image.
	r0, g0, b0 := hsvToRGB(hue, 0.55, 0.35)
	r1, g1, b1 := hsvToRGB((hue+30)%360, 0.55, 0.20)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			t := float64((x+y+frame*8)%(w+h)) / float64(w+h)
			r := uint8(float64(r0)*(1-t) + float64(r1)*t)
			g := uint8(float64(g0)*(1-t) + float64(g1)*t)
			b := uint8(float64(b0)*(1-t) + float64(b1)*t)
			img.SetRGBA(x, y, color.RGBA{r, g, b, 255})
		}
	}

	// Big block letters spelling the hostname + frame number. We don't pull
	// in a font package — paint each glyph as a 5×7 bitmap scaled up.
	caption := fmt.Sprintf("%s #%d", name, frame)
	scale := w / 40 // glyph cell ≈ 1/40th of the width
	if scale < 4 {
		scale = 4
	}
	drawString(img, caption, scale*2, scale*2, scale, color.RGBA{255, 255, 255, 255})

	stamp := time.Now().Format("15:04:05.000")
	if hi {
		stamp = "[HI] " + stamp
	}
	drawString(img, stamp, scale*2, h-scale*9, scale, color.RGBA{220, 220, 220, 255})

	var buf bytes.Buffer
	q := 65
	if hi {
		q = 80
	}
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: q}); err != nil {
		// Fallback: return the smallest valid JPEG we can.
		return []byte{0xFF, 0xD8, 0xFF, 0xD9}
	}
	return buf.Bytes()
}

// 5×7 bitmap font — only the chars we actually use (letters, digits, space,
// '#', ':', '.'). Each entry is 7 rows of 5 bits, MSB = leftmost column.
var glyphs = map[rune][7]byte{
	' ': {0, 0, 0, 0, 0, 0, 0},
	'#': {0x0A, 0x1F, 0x0A, 0x1F, 0x0A, 0x00, 0x00},
	':': {0x00, 0x04, 0x00, 0x00, 0x04, 0x00, 0x00},
	'.': {0x00, 0x00, 0x00, 0x00, 0x00, 0x04, 0x00},
	'-': {0x00, 0x00, 0x00, 0x0E, 0x00, 0x00, 0x00},
	'[': {0x0E, 0x08, 0x08, 0x08, 0x08, 0x08, 0x0E},
	']': {0x0E, 0x02, 0x02, 0x02, 0x02, 0x02, 0x0E},
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
	'B': {0x1E, 0x11, 0x11, 0x1E, 0x11, 0x11, 0x1E},
	'C': {0x0E, 0x11, 0x10, 0x10, 0x10, 0x11, 0x0E},
	'D': {0x1E, 0x11, 0x11, 0x11, 0x11, 0x11, 0x1E},
	'E': {0x1F, 0x10, 0x10, 0x1E, 0x10, 0x10, 0x1F},
	'F': {0x1F, 0x10, 0x10, 0x1E, 0x10, 0x10, 0x10},
	'G': {0x0E, 0x11, 0x10, 0x17, 0x11, 0x11, 0x0E},
	'H': {0x11, 0x11, 0x11, 0x1F, 0x11, 0x11, 0x11},
	'I': {0x0E, 0x04, 0x04, 0x04, 0x04, 0x04, 0x0E},
	'J': {0x07, 0x02, 0x02, 0x02, 0x02, 0x12, 0x0C},
	'K': {0x11, 0x12, 0x14, 0x18, 0x14, 0x12, 0x11},
	'L': {0x10, 0x10, 0x10, 0x10, 0x10, 0x10, 0x1F},
	'M': {0x11, 0x1B, 0x15, 0x15, 0x11, 0x11, 0x11},
	'N': {0x11, 0x11, 0x19, 0x15, 0x13, 0x11, 0x11},
	'O': {0x0E, 0x11, 0x11, 0x11, 0x11, 0x11, 0x0E},
	'P': {0x1E, 0x11, 0x11, 0x1E, 0x10, 0x10, 0x10},
	'Q': {0x0E, 0x11, 0x11, 0x11, 0x15, 0x12, 0x0D},
	'R': {0x1E, 0x11, 0x11, 0x1E, 0x14, 0x12, 0x11},
	'S': {0x0F, 0x10, 0x10, 0x0E, 0x01, 0x01, 0x1E},
	'T': {0x1F, 0x04, 0x04, 0x04, 0x04, 0x04, 0x04},
	'U': {0x11, 0x11, 0x11, 0x11, 0x11, 0x11, 0x0E},
	'V': {0x11, 0x11, 0x11, 0x11, 0x11, 0x0A, 0x04},
	'W': {0x11, 0x11, 0x11, 0x15, 0x15, 0x15, 0x0A},
	'X': {0x11, 0x11, 0x0A, 0x04, 0x0A, 0x11, 0x11},
	'Y': {0x11, 0x11, 0x11, 0x0A, 0x04, 0x04, 0x04},
	'Z': {0x1F, 0x01, 0x02, 0x04, 0x08, 0x10, 0x1F},
}

func drawString(img *image.RGBA, s string, x, y, scale int, c color.RGBA) {
	cx := x
	for _, r := range s {
		if r >= 'a' && r <= 'z' {
			r -= 32 // upper-case
		}
		g, ok := glyphs[r]
		if !ok {
			g = glyphs[' ']
		}
		drawGlyph(img, g, cx, y, scale, c)
		cx += 6 * scale
	}
}

func drawGlyph(img *image.RGBA, g [7]byte, x, y, scale int, c color.RGBA) {
	for row := 0; row < 7; row++ {
		bits := g[row]
		for col := 0; col < 5; col++ {
			if bits&(1<<(4-col)) != 0 {
				for sy := 0; sy < scale; sy++ {
					for sx := 0; sx < scale; sx++ {
						img.SetRGBA(x+col*scale+sx, y+row*scale+sy, c)
					}
				}
			}
		}
	}
}

// hsvToRGB converts h ∈ [0,360), s,v ∈ [0,1] to 0-255 RGB.
func hsvToRGB(h int, s, v float64) (uint8, uint8, uint8) {
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

func modF(a, b float64) float64 {
	return a - b*float64(int(a/b))
}

func init() {
	// keep os import alive for log target if user wants to redirect
	_ = os.Stderr
}
