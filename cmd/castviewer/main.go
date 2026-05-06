// castviewer.exe — student-side cast viewer (WebView2 backend).
//
// Build:
//
//	go build -ldflags="-H windowsgui" -o castviewer.exe ./cmd/castviewer
//
// Replaces the previous Win32 GDI cast window inside classsend-agent.exe with
// a thin WebView2 host. The agent spawns this binary on CmdStartCast and
// kills it on CmdStopCast. Frames are pushed into the page via Eval as base64
// JPEG data URLs — the same pattern monitoring.exe uses.
//
// Args:
//
//	-addr host:port   teacher's CastServer address (mandatory)
//	-title "..."      window title override (optional)
//
// Lifecycle:
//   - Agent spawns one of these per active cast.
//   - On TCP disconnect (teacher stopped, or network failure), the viewer
//     stays open showing the last frame so the student isn't slammed with a
//     window-close on a transient hiccup. The agent decides when to kill it.
//   - Window has the standard X button: clicking it just closes this process;
//     the student can reopen via --cast in the TUI (agent re-spawns us).

package main

import (
	"bufio"
	"encoding/base64"
	"encoding/binary"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"runtime"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"classsend/internal/buildinfo"
	"classsend/internal/devlog"

	webview2 "github.com/jchv/go-webview2"
)

// SetWindowPos lets the T-key callback toggle always-on-top from the JS
// side: WebView2 can request fullscreen itself, but z-order on the host
// HWND has to come from us.
var (
	user32           = syscall.NewLazyDLL("user32.dll")
	procSetWindowPos = user32.NewProc("SetWindowPos")
)

const (
	hwndTopmost   = ^uintptr(0) // HWND_TOPMOST   = (HWND)-1
	hwndNotopmost = ^uintptr(1) // HWND_NOTOPMOST = (HWND)-2
	swpNosize     = 0x0001
	swpNomove     = 0x0002
	swpNoactivate = 0x0010
)

func setWindowTopmost(hwnd uintptr, on bool) {
	zorder := uintptr(hwndNotopmost)
	if on {
		zorder = hwndTopmost
	}
	procSetWindowPos.Call(hwnd, zorder, 0, 0, 0, 0, swpNomove|swpNosize|swpNoactivate)
}

var (
	wv          webview2.WebView
	frameCount  atomic.Uint64
	bytesTotal  atomic.Uint64
	connectedAt atomic.Int64
)

func main() {
	addr := flag.String("addr", "", "teacher's CastServer address, host:port (required)")
	title := flag.String("title", "", "window title override")
	flag.Parse()

	runtime.LockOSThread()
	devlog.Init("castviewer")
	defer devlog.Close()
	devlog.Logf("startup pid=%d build=%s addr=%s", os.Getpid(), buildinfo.String(), *addr)

	if *addr == "" {
		fmt.Fprintln(os.Stderr, "castviewer: -addr is required (host:port)")
		os.Exit(2)
	}

	if *title == "" {
		*title = "ClassSend - Μετάδοση Δασκάλου  [" + buildinfo.String() + "]"
	}

	w := webview2.NewWithOptions(webview2.WebViewOptions{
		Debug: false,
		WindowOptions: webview2.WindowOptions{
			Title:  *title,
			Width:  960,
			Height: 600,
			Center: true,
		},
	})
	if w == nil {
		fmt.Fprintln(os.Stderr, "WebView2 runtime not available. Install Microsoft Edge WebView2 Runtime.")
		os.Exit(1)
	}
	defer w.Destroy()
	wv = w

	// Bind the topmost toggle so the T key in JS can flip the host HWND's
	// z-order. The viewer remembers the choice across frames; a relaunch
	// resets to non-topmost.
	if err := w.Bind("setTopmost", func(on bool) {
		hwnd := uintptr(unsafe.Pointer(w.Window()))
		setWindowTopmost(hwnd, on)
		devlog.Logf("setTopmost: on=%v hwnd=%x", on, hwnd)
	}); err != nil {
		devlog.Logf("Bind setTopmost: %v", err)
	}

	w.SetHtml(pageHTML)

	// Stream loop runs in its own goroutine. It owns the TCP connection;
	// closing the WebView (Run() returns) does NOT auto-close the TCP, so
	// the agent must Process.Kill us when CmdStopCast arrives. That's clean.
	go streamLoop(*addr)

	w.Run()
}

// streamLoop dials the cast server and pumps frames into the WebView until
// the connection drops. It logs reconnect attempts but does NOT auto-retry —
// the agent decides whether to relaunch us. A clean exit on TCP close keeps
// the last good frame on screen so a transient failure isn't disorienting.
func streamLoop(addr string) {
	defer func() {
		if r := recover(); r != nil {
			devlog.Logf("streamLoop PANIC: %v", r)
		}
	}()

	dispatchEval(`setStatus('Σύνδεση στον δάσκαλο...')`)

	// Multi-NIC teacher: -addr may be a comma-separated list of "host:port"
	// pairs (one per teacher NIC). Try each in order; the first that dials is
	// the one on our subnet. Whitespace tolerated.
	candidates := splitAddrs(addr)
	var conn net.Conn
	var lastErr error
	var dialedAddr string
	for _, cand := range candidates {
		c, err := net.DialTimeout("tcp", cand, 3*time.Second)
		if err == nil {
			conn = c
			dialedAddr = cand
			break
		}
		devlog.Logf("dial %s: %v", cand, err)
		lastErr = err
	}
	if conn == nil {
		msg := "Δεν βρέθηκε ο δάσκαλος"
		if lastErr != nil {
			msg = msg + ": " + lastErr.Error()
		}
		dispatchEval(jsCall("setStatus", msg))
		return
	}
	defer conn.Close()
	addr = dialedAddr

	if tc, ok := conn.(*net.TCPConn); ok {
		tc.SetNoDelay(true)
		tc.SetReadBuffer(4 * 1024 * 1024)
	}

	connectedAt.Store(time.Now().UnixNano())
	dispatchEval(`setStatus('')`)
	devlog.Logf("connected to %s", addr)

	r := bufio.NewReaderSize(conn, 4*1024*1024)
	hdr := make([]byte, 4)
	for {
		if _, err := io.ReadFull(r, hdr); err != nil {
			devlog.Logf("read hdr: %v", err)
			dispatchEval(`setStatus('Η μετάδοση τερματίστηκε')`)
			return
		}
		size := binary.BigEndian.Uint32(hdr)
		if size == 0 || size > 20*1024*1024 {
			devlog.Logf("invalid frame size: %d", size)
			return
		}
		frame := make([]byte, size)
		if _, err := io.ReadFull(r, frame); err != nil {
			devlog.Logf("read body: %v", err)
			return
		}
		frameCount.Add(1)
		bytesTotal.Add(uint64(size))

		// Push to JS. Eval is async on the UI thread; large strings (~200 KB
		// base64 for a 1080p Q85 JPEG) are fine — the channel from the Go
		// host to the WebView is in-process.
		b64 := base64.StdEncoding.EncodeToString(frame)
		dispatchEval(`applyFrame('` + b64 + `')`)
	}
}

func dispatchEval(js string) {
	if wv == nil {
		return
	}
	wv.Dispatch(func() {
		wv.Eval(js)
	})
}

// splitAddrs parses a comma-separated "host:port[,host:port...]" list into a
// trimmed slice. Empty / whitespace-only entries are skipped so a stray comma
// in the teacher's broadcast doesn't add a doomed dial attempt.
func splitAddrs(s string) []string {
	parts := strings.Split(s, ",")
	out := parts[:0]
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// jsCall builds a `name("arg")` JS expression with the arg JSON-quoted.
func jsCall(name, arg string) string {
	var sb []byte
	sb = append(sb, name...)
	sb = append(sb, '(', '"')
	for _, r := range arg {
		switch r {
		case '\\':
			sb = append(sb, '\\', '\\')
		case '"':
			sb = append(sb, '\\', '"')
		case '\n':
			sb = append(sb, '\\', 'n')
		case '\r':
			sb = append(sb, '\\', 'r')
		default:
			if r < 0x20 {
				sb = append(sb, []byte(fmt.Sprintf(`\u%04x`, r))...)
			} else {
				sb = append(sb, []byte(string(r))...)
			}
		}
	}
	sb = append(sb, '"', ')')
	return string(sb)
}

// pageHTML is the entire viewer UI. The image element is reused; only its
// src attribute changes per frame so the browser swaps frames atomically and
// without repainting the whole page. CSS handles letterbox via `object-fit:
// contain` — same approach as monitoring.exe.
const pageHTML = `<!doctype html>
<html lang="el">
<head>
<meta charset="utf-8">
<title>ClassSend - Μετάδοση Δασκάλου</title>
<style>
  * { box-sizing: border-box; }
  html, body {
    margin: 0; padding: 0;
    height: 100%; width: 100%;
    background: #000;
    color: #ccc;
    font: 14px/1.3 "Segoe UI", system-ui, sans-serif;
    overflow: hidden;
    user-select: none;
  }
  /* Reserve a 26 px strip at the bottom for the keyboard-hint bar so the
     teacher's stream is never covered by it. In fullscreen the hint bar
     hides so the broadcast really fills the screen. */
  #stage {
    position: fixed;
    top: 0; left: 0; right: 0; bottom: 26px;
    display: flex; align-items: center; justify-content: center;
  }
  #frame {
    max-width: 100%;
    max-height: 100%;
    object-fit: contain;
    image-rendering: -webkit-optimize-contrast;
  }
  #status {
    position: fixed; top: 12px; left: 12px;
    background: rgba(0,0,0,.55);
    color: #fff;
    padding: 6px 12px;
    border-radius: 6px;
    font-weight: 500;
    pointer-events: none;
    transition: opacity .25s ease;
  }
  #status.hidden { opacity: 0; }
  #hint {
    position: fixed;
    left: 0; right: 0; bottom: 0;
    height: 26px;
    display: flex;
    align-items: center;
    justify-content: center;
    background: #0c0c0c;
    color: #888;
    font-size: 12px;
    border-top: 1px solid #1a1a1a;
    pointer-events: none;
    z-index: 5;
  }
  /* Fullscreen: hide hint and let the stage cover the whole viewport. */
  :fullscreen #hint, :-webkit-full-screen #hint { display: none; }
  :fullscreen #stage, :-webkit-full-screen #stage { bottom: 0; }
</style>
</head>
<body>
  <div id="stage"><img id="frame" alt=""></div>
  <div id="status">Αναμονή σύνδεσης...</div>
  <div id="hint">F = πλήρης οθόνη · T = πάντα μπροστά · Esc = έξοδος πλήρους οθόνης</div>

<script>
  const img = document.getElementById('frame');
  const status = document.getElementById('status');
  let everReceivedFrame = false;

  window.setStatus = function(text) {
    if (text) {
      status.textContent = text;
      status.classList.remove('hidden');
    } else {
      status.classList.add('hidden');
    }
  };

  // applyFrame replaces the <img> src. The browser keeps the previous frame
  // visible until the new one decodes, so there is no flash to black.
  window.applyFrame = function(b64) {
    img.src = 'data:image/jpeg;base64,' + b64;
    if (!everReceivedFrame) {
      everReceivedFrame = true;
      window.setStatus('');
    }
  };

  // Keyboard shortcuts (just the bare letter, no modifier):
  //   F   toggle fullscreen   (browser-level requestFullscreen)
  //   T   toggle always-on-top (host-window z-order, via Go callback)
  //   Esc browsers auto-exit fullscreen on Esc, so no explicit handler
  let topmost = false;
  document.addEventListener('keydown', (ev) => {
    if (ev.target.tagName === 'INPUT' || ev.target.tagName === 'TEXTAREA') return;
    const k = ev.key.toLowerCase();
    if (k === 'f') {
      if (!document.fullscreenElement) {
        document.documentElement.requestFullscreen().catch(() => {});
      } else {
        document.exitFullscreen().catch(() => {});
      }
      ev.preventDefault();
    } else if (k === 't') {
      topmost = !topmost;
      if (window.setTopmost) window.setTopmost(topmost);
      window.setStatus(topmost ? 'Πάντα μπροστά: ON' : 'Πάντα μπροστά: OFF');
      // hide the toast after a moment
      setTimeout(() => window.setStatus(''), 1200);
      ev.preventDefault();
    }
  });
</script>
</body>
</html>`
