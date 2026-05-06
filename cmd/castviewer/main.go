// castviewer.exe — student-side cast viewer (WebView2 + MSE backend).
//
// Build:
//
//	go build -ldflags="-H windowsgui" -o castviewer.exe ./cmd/castviewer
//
// v0.0.6 wire layer: the teacher's CastServer streams fragmented MP4 / H.264
// chunks (ftyp+moov init segment first, then moof+mdat fragments). This
// process feeds them into a <video> element via Media Source Extensions and
// lets Chromium's hardware-accelerated decoder do the heavy lifting.
// Bandwidth is ~10× lower than the v0.0.5 JPEG-per-frame pipeline at the
// same perceptual quality.
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
	"flag"
	"fmt"
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
	"classsend/internal/network"

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
	chunkCount  atomic.Uint64
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

// streamLoop dials the cast server and pumps fMP4 chunks into the WebView
// until the connection drops. The first chunk is always the init segment
// (ftyp+moov) — JS uses it to configure the SourceBuffer. Subsequent chunks
// are media fragments (moof+mdat).
func streamLoop(addr string) {
	defer func() {
		if r := recover(); r != nil {
			devlog.Logf("streamLoop PANIC: %v", r)
		}
	}()

	dispatchEval(`setStatus('Σύνδεση στον δάσκαλο...')`)

	// Multi-NIC teacher: -addr may be a comma-separated list of "host:port"
	// pairs (one per teacher NIC). Try each in order; the first that dials is
	// the one on our subnet.
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
	dispatchEval(`setStatus('Αναμονή πρώτου καρέ...')`)
	devlog.Logf("connected to %s", addr)

	r := bufio.NewReaderSize(conn, 4*1024*1024)
	first := true
	for {
		chunk, err := network.CastReadFrame(r)
		if err != nil {
			devlog.Logf("read: %v", err)
			dispatchEval(`setStatus('Η μετάδοση τερματίστηκε')`)
			return
		}
		chunkCount.Add(1)
		bytesTotal.Add(uint64(len(chunk)))

		// First chunk must be the init segment. JS routes it to
		// `applyInit`; subsequent chunks go to `applyFragment`. Server
		// guarantees this ordering.
		jsFn := "applyFragment"
		if first {
			jsFn = "applyInit"
			first = false
		}

		// Push to JS as base64. ~7-280 KB per fragment at 1080p; well within
		// WebView2's Eval throughput. We could move to a localhost WebSocket
		// for better binary efficiency later, but base64 keeps parity with
		// monitoring.exe and is good enough for 30 fps.
		b64 := base64.StdEncoding.EncodeToString(chunk)
		dispatchEval(jsFn + `('` + b64 + `')`)
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

// pageHTML is the entire viewer UI. The <video> element is fed via Media
// Source Extensions: applyInit() supplies the codec config + ftyp/moov, and
// applyFragment() pushes each moof+mdat fragment. Chromium's H.264 decoder
// (hardware-accelerated where available) handles the rest.
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
    background: #000;
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
  <div id="stage"><video id="frame" autoplay muted playsinline></video></div>
  <div id="status">Αναμονή σύνδεσης...</div>
  <div id="hint">F = πλήρης οθόνη · T = πάντα μπροστά · Esc = έξοδος πλήρους οθόνης</div>

<script>
  // Codec string for H.264 baseline profile, level 3.1 — matches what ffmpeg
  // is asked to emit on the teacher side. If the runtime ever rejects this,
  // adjust both ends together.
  const CODEC = 'video/mp4; codecs="avc1.42E01F"';

  const video = document.getElementById('frame');
  const status = document.getElementById('status');

  let mediaSource = null;
  let sourceBuffer = null;
  // Pending init/fragments queued before sourceopen, plus fragments queued
  // while sourceBuffer.updating is true. Bounded so a stalled appender can't
  // run us out of memory.
  const pending = [];
  const PENDING_MAX = 120;     // ~4 seconds at 30 fps
  let everReceivedFragment = false;

  if (!('MediaSource' in window)) {
    setStatus('MediaSource δεν υποστηρίζεται από αυτή την έκδοση WebView2');
    throw new Error('MSE not supported');
  }
  if (!MediaSource.isTypeSupported(CODEC)) {
    setStatus('Ο κωδικοποιητής H.264 baseline δεν είναι διαθέσιμος');
    throw new Error('codec not supported: ' + CODEC);
  }

  mediaSource = new MediaSource();
  video.src = URL.createObjectURL(mediaSource);

  mediaSource.addEventListener('sourceopen', () => {
    sourceBuffer = mediaSource.addSourceBuffer(CODEC);
    // 'sequence' tells MSE to append in arrival order regardless of the
    // fragment timestamps. Our encoder uses contiguous monotonic timestamps
    // anyway, but this avoids any edge-case rejection.
    sourceBuffer.mode = 'sequence';
    sourceBuffer.addEventListener('updateend', flushPending);
    flushPending();
  });

  function flushPending() {
    if (!sourceBuffer || sourceBuffer.updating || pending.length === 0) return;
    const next = pending.shift();
    try {
      sourceBuffer.appendBuffer(next);
    } catch (e) {
      console.error('appendBuffer failed:', e);
      // Drop and try the next one on the next updateend.
    }
  }

  function enqueueChunk(bytes) {
    if (sourceBuffer && !sourceBuffer.updating && pending.length === 0) {
      try {
        sourceBuffer.appendBuffer(bytes);
        return;
      } catch (e) {
        console.error('appendBuffer (direct) failed:', e);
      }
    }
    pending.push(bytes);
    while (pending.length > PENDING_MAX) {
      pending.shift(); // drop oldest queued fragments under runaway buffering
    }
  }

  // applyInit / applyFragment — the Go side calls these from the TCP loop.
  // applyInit fires exactly once per stream (first chunk = ftyp+moov).
  window.applyInit = function(b64) {
    const bytes = base64ToBytes(b64);
    enqueueChunk(bytes);
  };
  window.applyFragment = function(b64) {
    const bytes = base64ToBytes(b64);
    enqueueChunk(bytes);
    if (!everReceivedFragment) {
      everReceivedFragment = true;
      setStatus('');
    }
  };

  function base64ToBytes(b64) {
    const bin = atob(b64);
    const len = bin.length;
    const bytes = new Uint8Array(len);
    for (let i = 0; i < len; i++) bytes[i] = bin.charCodeAt(i);
    return bytes;
  }

  window.setStatus = function(text) {
    if (text) {
      status.textContent = text;
      status.classList.remove('hidden');
    } else {
      status.classList.add('hidden');
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
      setTimeout(() => window.setStatus(''), 1200);
      ev.preventDefault();
    }
  });
</script>
</body>
</html>`
