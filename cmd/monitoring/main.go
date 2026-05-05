// monitoring.exe — ClassSend teacher monitoring window (WebView2 backend).
//
// Build:
//
//	go build -ldflags="-H windowsgui" -o monitoring.exe ./cmd/monitoring
//
// Replaces the previous ~1100-line hand-rolled GDI grid with a thin WebView2
// host. The pipe protocol with classsend.exe is identical (MsgInit / MsgShot /
// MsgOffline / MsgStop / MsgFocus), so the teacher side did not change.
//
// Architecture:
//
//	classsend.exe ──named pipe── monitoring.exe ──Eval(js)── WebView2 (HTML)
//	                                  ▲
//	                                  └── Bind("onCellClick") ── click events
//
// Each MsgShot from the pipe is base64-encoded and pushed to the page via
// w.Eval("applyShot(idx, '...')"). The page applies it as an <img> src. Clicks
// arrive via the bound `onCellClick` function and are forwarded to classsend
// as MsgFocus on the same pipe.
package main

import (
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"classsend/internal/buildinfo"
	"classsend/internal/devlog"

	webview2 "github.com/jchv/go-webview2"
)

// ── Pipe protocol (must match internal/monitoring/session_windows.go) ─────────

const (
	msgInit    uint32 = 1
	msgShot    uint32 = 2
	msgOffline uint32 = 3
	msgStop    uint32 = 4
	msgFocus   uint32 = 5

	focusUnset uint32 = 0xFFFFFFFF
)

// ── Win32 API for pipe I/O (same overlapped helpers as before) ────────────────

var (
	kernel32 = syscall.NewLazyDLL("kernel32.dll")
	user32   = syscall.NewLazyDLL("user32.dll")

	procSetWindowPos = user32.NewProc("SetWindowPos")

	procCreateFileW         = kernel32.NewProc("CreateFileW")
	procReadFile            = kernel32.NewProc("ReadFile")
	procWriteFile           = kernel32.NewProc("WriteFile")
	procCloseHandle         = kernel32.NewProc("CloseHandle")
	procWaitNamedPipe       = kernel32.NewProc("WaitNamedPipeW")
	procCreateEventW        = kernel32.NewProc("CreateEventW")
	procWaitForSingleObject = kernel32.NewProc("WaitForSingleObject")
	procCancelIoEx          = kernel32.NewProc("CancelIoEx")
	procGetOverlappedResult = kernel32.NewProc("GetOverlappedResult")
	procResetEvent          = kernel32.NewProc("ResetEvent")
)

const (
	genericRead        = 0x80000000
	genericWrite       = 0x40000000
	openExisting       = 3
	fileAttrNormal     = 0x00000080
	fileFlagOverlapped = 0x40000000
	invalidHandle      = ^uintptr(0)

	errorIoPending = 997
	waitObject0    = 0x00000000
	waitTimeout    = 0x00000102
	infinite       = 0xFFFFFFFF

	// SetWindowPos constants for the always-on-top toggle.
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

type pipeOp struct {
	ov    syscall.Overlapped
	event uintptr
}

func newPipeOp() (*pipeOp, error) {
	h, _, e := procCreateEventW.Call(0, 1, 0, 0)
	if h == 0 {
		return nil, fmt.Errorf("CreateEvent: %w", e)
	}
	op := &pipeOp{event: h}
	op.ov.HEvent = syscall.Handle(h)
	return op, nil
}

func (op *pipeOp) close() {
	if op == nil || op.event == 0 {
		return
	}
	procCloseHandle.Call(op.event)
	op.event = 0
}

func (op *pipeOp) reset() {
	op.ov = syscall.Overlapped{HEvent: syscall.Handle(op.event)}
	procResetEvent.Call(op.event)
}

func pipeWriteAll(handle uintptr, op *pipeOp, data []byte, timeout time.Duration) error {
	if len(data) == 0 {
		return nil
	}
	op.reset()
	var written uint32
	ret, _, e := procWriteFile.Call(
		handle,
		uintptr(unsafe.Pointer(&data[0])),
		uintptr(len(data)),
		uintptr(unsafe.Pointer(&written)),
		uintptr(unsafe.Pointer(&op.ov)),
	)
	if ret != 0 {
		if int(written) != len(data) {
			return fmt.Errorf("WriteFile partial: %d/%d", written, len(data))
		}
		return nil
	}
	if e != syscall.Errno(errorIoPending) {
		return fmt.Errorf("WriteFile: %w", e)
	}
	waitMs := uint32(timeout / time.Millisecond)
	if waitMs == 0 {
		waitMs = 1
	}
	wRet, _, _ := procWaitForSingleObject.Call(op.event, uintptr(waitMs))
	if wRet == waitTimeout {
		procCancelIoEx.Call(handle, uintptr(unsafe.Pointer(&op.ov)))
		procWaitForSingleObject.Call(op.event, infinite)
		return fmt.Errorf("WriteFile timeout after %v", timeout)
	}
	if wRet != waitObject0 {
		return fmt.Errorf("WriteFile WaitForSingleObject: 0x%x", wRet)
	}
	var transferred uint32
	gRet, _, gErr := procGetOverlappedResult.Call(
		handle,
		uintptr(unsafe.Pointer(&op.ov)),
		uintptr(unsafe.Pointer(&transferred)),
		0,
	)
	if gRet == 0 {
		return fmt.Errorf("GetOverlappedResult: %w", gErr)
	}
	if int(transferred) != len(data) {
		return fmt.Errorf("WriteFile partial: %d/%d", transferred, len(data))
	}
	return nil
}

func pipeReadFull(handle uintptr, op *pipeOp, buf []byte, timeout time.Duration) error {
	if len(buf) == 0 {
		return nil
	}
	deadline := time.Now().Add(timeout)
	read := 0
	for read < len(buf) {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return fmt.Errorf("ReadFile timeout")
		}
		op.reset()
		var nRead uint32
		ret, _, e := procReadFile.Call(
			handle,
			uintptr(unsafe.Pointer(&buf[read])),
			uintptr(len(buf)-read),
			uintptr(unsafe.Pointer(&nRead)),
			uintptr(unsafe.Pointer(&op.ov)),
		)
		if ret == 0 && e != syscall.Errno(errorIoPending) {
			return fmt.Errorf("ReadFile: %w", e)
		}
		if ret == 0 {
			waitMs := uint32(remaining / time.Millisecond)
			if waitMs == 0 {
				waitMs = 1
			}
			wRet, _, _ := procWaitForSingleObject.Call(op.event, uintptr(waitMs))
			if wRet == waitTimeout {
				procCancelIoEx.Call(handle, uintptr(unsafe.Pointer(&op.ov)))
				procWaitForSingleObject.Call(op.event, infinite)
				return fmt.Errorf("ReadFile timeout")
			}
			if wRet != waitObject0 {
				return fmt.Errorf("ReadFile WaitForSingleObject: 0x%x", wRet)
			}
			var transferred uint32
			gRet, _, gErr := procGetOverlappedResult.Call(
				handle,
				uintptr(unsafe.Pointer(&op.ov)),
				uintptr(unsafe.Pointer(&transferred)),
				0,
			)
			if gRet == 0 {
				return fmt.Errorf("GetOverlappedResult (read): %w", gErr)
			}
			nRead = transferred
		}
		if nRead == 0 {
			return io.EOF
		}
		read += int(nRead)
	}
	return nil
}

func connectPipe() (uintptr, error) {
	namePtr, _ := syscall.UTF16PtrFromString(`\\.\pipe\ClassSendMonitor`)
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		h, _, _ := procCreateFileW.Call(
			uintptr(unsafe.Pointer(namePtr)),
			uintptr(genericRead|genericWrite),
			0, 0,
			openExisting,
			fileAttrNormal|fileFlagOverlapped,
			0,
		)
		if h != invalidHandle {
			return h, nil
		}
		procWaitNamedPipe.Call(uintptr(unsafe.Pointer(namePtr)), 1000)
		time.Sleep(100 * time.Millisecond)
	}
	return 0, fmt.Errorf("pipe not available after 15 s")
}

// ── State ─────────────────────────────────────────────────────────────────────

var (
	pipeMu     sync.Mutex
	pipeHandle uintptr

	wv webview2.WebView
)

// sendFocusBackChannel forwards a click from the JS side to the teacher.
// Called from the bound onCellClick (runs on the WebView UI thread).
func sendFocusBackChannel(idx uint32) {
	pipeMu.Lock()
	h := pipeHandle
	pipeMu.Unlock()
	if h == 0 {
		devlog.Logf("focus click: no pipe handle yet, dropping")
		return
	}
	frame := make([]byte, 12)
	binary.LittleEndian.PutUint32(frame[0:4], msgFocus)
	binary.LittleEndian.PutUint32(frame[4:8], 4)
	binary.LittleEndian.PutUint32(frame[8:12], idx)
	op, err := newPipeOp()
	if err != nil {
		devlog.Logf("focus click: newPipeOp: %v", err)
		return
	}
	defer op.close()
	if err := pipeWriteAll(h, op, frame, 2*time.Second); err != nil {
		devlog.Logf("focus click: write failed: %v", err)
		return
	}
	if idx == focusUnset {
		devlog.Logf("focus click: sent UNFOCUS")
	} else {
		devlog.Logf("focus click: sent FOCUS idx=%d", idx)
	}
}

// ── Pipe reader → JS ──────────────────────────────────────────────────────────

func readPipe(handle uintptr) {
	defer func() {
		if r := recover(); r != nil {
			devlog.Logf("readPipe PANIC: %v\n%s", r, debug.Stack())
		}
	}()
	op, err := newPipeOp()
	if err != nil {
		devlog.Logf("readPipe: newPipeOp: %v", err)
		dispatchEval(`setStatus('Σφάλμα σύνδεσης')`)
		return
	}
	defer op.close()
	hdr := make([]byte, 8)

	for {
		if err := pipeReadFull(handle, op, hdr, time.Minute); err != nil {
			devlog.Logf("readPipe: header read err: %v", err)
			dispatchEval(`setStatus('Εκτός σύνδεσης')`)
			return
		}
		msgType := binary.LittleEndian.Uint32(hdr[0:4])
		payLen := binary.LittleEndian.Uint32(hdr[4:8])

		var payload []byte
		if payLen > 0 {
			payload = make([]byte, payLen)
			if err := pipeReadFull(handle, op, payload, 10*time.Second); err != nil {
				devlog.Logf("readPipe: payload read err: %v", err)
				dispatchEval(`setStatus('Εκτός σύνδεσης')`)
				return
			}
		}

		switch msgType {
		case msgInit:
			handleInit(payload)
		case msgShot:
			handleShot(payload)
		case msgOffline:
			handleOffline(payload)
		case msgStop:
			devlog.Logf("readPipe: msgStop received")
			if wv != nil {
				wv.Terminate()
			}
			return
		default:
			devlog.Logf("readPipe: UNKNOWN msgType=%d", msgType)
		}
	}
}

// dispatchEval pushes a JS snippet to the WebView from any goroutine. Eval
// itself is async on the UI thread; Dispatch ensures we don't race with
// webview teardown.
func dispatchEval(js string) {
	if wv == nil {
		return
	}
	wv.Dispatch(func() {
		wv.Eval(js)
	})
}

func handleInit(payload []byte) {
	if len(payload) < 4 {
		return
	}
	count := binary.LittleEndian.Uint32(payload[0:4])
	offset := 4
	names := make([]string, 0, count)
	for i := uint32(0); i < count; i++ {
		if offset+4 > len(payload) {
			break
		}
		nameLen := int(binary.LittleEndian.Uint32(payload[offset : offset+4]))
		offset += 4
		if offset+nameLen > len(payload) {
			break
		}
		names = append(names, string(payload[offset:offset+nameLen]))
		offset += nameLen
	}

	// Build a JS array literal. JSON-escape names defensively.
	var sb strings.Builder
	sb.WriteString("applyInit([")
	for i, n := range names {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(jsString(n))
	}
	sb.WriteString("])")
	dispatchEval(sb.String())
}

func handleShot(payload []byte) {
	if len(payload) < 8 {
		return
	}
	idx := binary.LittleEndian.Uint32(payload[0:4])
	jpegLen := int(binary.LittleEndian.Uint32(payload[4:8]))
	if 8+jpegLen > len(payload) {
		return
	}
	jpegData := payload[8 : 8+jpegLen]
	b64 := base64.StdEncoding.EncodeToString(jpegData)

	// applyShot(idx, "<base64>") — JS sets the cell <img src="data:image/jpeg;base64,...">
	js := fmt.Sprintf("applyShot(%d,'%s')", idx, b64)
	dispatchEval(js)
}

func handleOffline(payload []byte) {
	if len(payload) < 4 {
		return
	}
	idx := binary.LittleEndian.Uint32(payload[0:4])
	dispatchEval(fmt.Sprintf("applyOffline(%d)", idx))
}

// jsString JSON-encodes a string for safe embedding inside Eval. Names can
// contain quotes, backslashes, or non-ASCII Greek — escape conservatively.
func jsString(s string) string {
	var sb strings.Builder
	sb.WriteByte('"')
	for _, r := range s {
		switch r {
		case '\\':
			sb.WriteString(`\\`)
		case '"':
			sb.WriteString(`\"`)
		case '\n':
			sb.WriteString(`\n`)
		case '\r':
			sb.WriteString(`\r`)
		case '\t':
			sb.WriteString(`\t`)
		default:
			if r < 0x20 {
				fmt.Fprintf(&sb, `\u%04x`, r)
			} else {
				sb.WriteRune(r)
			}
		}
	}
	sb.WriteByte('"')
	return sb.String()
}

// ── HTML page ─────────────────────────────────────────────────────────────────

// pageHTML is the entire UI. CSS Grid auto-fits the cells to the window;
// aspect-ratio:16/9 gives a stable cell shape; <img>'s object-fit:contain
// preserves the screenshot's own aspect ratio inside the cell.
//
// Frames arrive via applyShot(idx, base64). The image element is reused —
// only its src attribute changes, so the browser can decode incrementally
// and the layout doesn't reflow on every frame.
const pageHTML = `<!doctype html>
<html lang="el">
<head>
<meta charset="utf-8">
<title>ClassSend - Παρακολούθηση</title>
<style>
  :root {
    --bg: #111;
    --cell-bg: #181818;
    --cell-bg-offline: #1a0000;
    --label: #ddd;
    --label-offline: #4444aa;
    --muted: #555;
  }
  * { box-sizing: border-box; }
  html, body {
    margin: 0; padding: 0;
    height: 100%; width: 100%;
    background: var(--bg);
    color: var(--label);
    font: 14px/1.3 "Segoe UI", system-ui, sans-serif;
    overflow: hidden;
    user-select: none;
  }
  #status {
    position: fixed; top: 8px; left: 8px;
    color: var(--muted); font-size: 12px;
    pointer-events: none;
  }
  /* Reserve a 26 px strip at the bottom of the viewport for the keyboard
     hint bar so the cells never extend underneath it. */
  #grid {
    display: grid;
    gap: 6px;
    padding: 6px;
    height: calc(100vh - 26px); width: 100vw;
    grid-auto-rows: 1fr;
  }
  #kbd-hint {
    position: fixed;
    left: 0; right: 0; bottom: 0;
    height: 26px;
    display: flex;
    align-items: center;
    justify-content: center;
    background: #0c0c0c;
    color: var(--muted);
    font-size: 12px;
    border-top: 1px solid #1a1a1a;
    pointer-events: none;
    z-index: 5;
  }
  /* Focus mode hides the hint bar so the focused cell really fills the
     viewport — Esc still gets the user out. */
  body.focus #kbd-hint { display: none; }
  body.focus #grid { height: 100vh; }
  #grid.empty {
    display: flex;
    align-items: center;
    justify-content: center;
    color: var(--muted);
    font-size: 16px;
  }
  .cell {
    background: var(--cell-bg);
    border-radius: 8px;
    padding: 10px;
    display: flex;
    flex-direction: column;
    overflow: hidden;
    cursor: pointer;
    transition: transform .08s ease;
  }
  .cell:hover { transform: scale(1.005); }
  .cell.offline { background: var(--cell-bg-offline); }
  .cell .imgwrap {
    flex: 1 1 auto;
    min-height: 0;
    display: flex;
    align-items: center;
    justify-content: center;
    overflow: hidden;
  }
  .cell img {
    max-width: 100%;
    max-height: 100%;
    object-fit: contain;
    border-radius: 4px;
    image-rendering: -webkit-optimize-contrast;
  }
  .cell .placeholder {
    color: var(--muted);
    font-size: 13px;
  }
  .cell .label {
    text-align: center;
    color: var(--label);
    font-weight: 600;
    margin-top: 6px;
    white-space: nowrap;
    overflow: hidden;
    text-overflow: ellipsis;
  }
  .cell.offline .label { color: var(--label-offline); }

  /* Focus mode: one cell fills the viewport. */
  body.focus #grid { display: block; padding: 0; }
  body.focus .cell { display: none; }
  body.focus .cell.focused {
    display: flex;
    width: 100vw; height: 100vh;
    border-radius: 0;
    padding: 0;
  }
  body.focus .cell.focused .imgwrap { padding: 0; }
  body.focus .cell.focused .label {
    position: fixed; bottom: 12px; left: 0; right: 0;
    background: rgba(0,0,0,.55);
    padding: 6px 12px;
    margin: 0;
  }
  #exit-hint {
    display: none;
    position: fixed; top: 12px; right: 12px;
    background: #c22;
    color: #fff;
    padding: 6px 14px;
    border-radius: 6px;
    font-weight: 600;
    z-index: 10;
    pointer-events: none;
  }
  body.focus #exit-hint { display: block; }
</style>
</head>
<body>
  <div id="status">Αναμονή σύνδεσης μαθητών...</div>
  <div id="exit-hint">Esc ή κλικ για έξοδο</div>
  <div id="kbd-hint">F = πλήρης οθόνη · T = πάντα μπροστά</div>
  <div id="grid" class="empty"></div>

<script>
  const grid = document.getElementById('grid');
  const status = document.getElementById('status');
  let cells = [];      // [{name, img, el}] indexed by pipe idx
  let focusIdx = -1;

  function setStatus(text) { status.textContent = text || ''; }
  window.setStatus = setStatus;

  // applyInit rebuilds the grid for a new student list. Existing cells are
  // matched BY NAME (not slot index) so a student joining/leaving doesn't
  // wipe everyone else's screenshot.
  window.applyInit = function(names) {
    const oldByName = {};
    for (const c of cells) {
      if (c.name) oldByName[c.name] = c;
    }
    cells = [];
    grid.innerHTML = '';
    if (!names || !names.length) {
      grid.classList.add('empty');
      grid.textContent = 'Αναμονή σύνδεσης μαθητών...';
      setStatus('');
      return;
    }
    grid.classList.remove('empty');
    // Square-ish layout: ceil(sqrt(N)) columns
    const cols = Math.max(1, Math.ceil(Math.sqrt(names.length)));
    grid.style.gridTemplateColumns = 'repeat(' + cols + ', 1fr)';

    for (let i = 0; i < names.length; i++) {
      const name = names[i];
      const el = document.createElement('div');
      el.className = 'cell';
      el.dataset.idx = i;
      const wrap = document.createElement('div');
      wrap.className = 'imgwrap';
      const img = document.createElement('img');
      img.alt = '';
      const placeholder = document.createElement('div');
      placeholder.className = 'placeholder';
      placeholder.textContent = 'Αναμονή...';
      wrap.appendChild(placeholder);
      const label = document.createElement('div');
      label.className = 'label';
      label.textContent = name;
      el.appendChild(wrap);
      el.appendChild(label);
      el.addEventListener('click', () => toggleFocus(i));

      // Carry over screenshot if same student is still here
      const prev = oldByName[name];
      if (prev && prev.lastSrc) {
        wrap.removeChild(placeholder);
        img.src = prev.lastSrc;
        wrap.appendChild(img);
      }

      cells.push({ name, el, wrap, img, placeholder, lastSrc: prev ? prev.lastSrc : null, offline: false });
      grid.appendChild(el);
    }
    setStatus('');
  };

  // applyShot replaces the image src for cell idx. The previous src is held
  // by the browser until the new one decodes, so there is no flash to black
  // on update — exactly the behaviour the user asked for.
  window.applyShot = function(idx, b64) {
    const c = cells[idx];
    if (!c) return;
    const src = 'data:image/jpeg;base64,' + b64;
    c.lastSrc = src;
    c.img.src = src;
    if (c.placeholder.parentNode) {
      c.wrap.removeChild(c.placeholder);
      c.wrap.appendChild(c.img);
    }
    c.offline = false;
    c.el.classList.remove('offline');
  };

  // applyOffline tints the cell red but KEEPS the last screenshot — no point
  // showing a black box just because the latest poll timed out.
  window.applyOffline = function(idx) {
    const c = cells[idx];
    if (!c) return;
    c.offline = true;
    c.el.classList.add('offline');
  };

  function toggleFocus(idx) {
    if (focusIdx === idx) {
      // Already focused on this one → exit focus mode.
      focusIdx = -1;
      document.body.classList.remove('focus');
      for (const c of cells) c.el.classList.remove('focused');
      // 0xFFFFFFFF
      if (window.onCellClick) window.onCellClick(4294967295);
      return;
    }
    focusIdx = idx;
    document.body.classList.add('focus');
    for (let i = 0; i < cells.length; i++) {
      cells[i].el.classList.toggle('focused', i === idx);
    }
    if (window.onCellClick) window.onCellClick(idx);
  }

  // Keyboard shortcuts (bare letter, no modifier):
  //   F   toggle fullscreen
  //   T   toggle always-on-top (host-window z-order, via Go callback)
  //   Esc exit focus mode (and browser auto-exits fullscreen)
  let topmost = false;
  function flashStatus(text) {
    setStatus(text);
    setTimeout(() => setStatus(''), 1200);
  }
  document.addEventListener('keydown', (ev) => {
    if (ev.target.tagName === 'INPUT' || ev.target.tagName === 'TEXTAREA') return;
    if (ev.key === 'Escape' && focusIdx >= 0) {
      toggleFocus(focusIdx);
      return;
    }
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
      flashStatus(topmost ? 'Πάντα μπροστά: ON' : 'Πάντα μπροστά: OFF');
      ev.preventDefault();
    }
  });
</script>
</body>
</html>`

// ── Main ──────────────────────────────────────────────────────────────────────

func main() {
	runtime.LockOSThread()

	devlog.Init("monitoring")
	defer devlog.Close()
	devlog.Logf("startup  pid=%d  build=%s  exe=%s", os.Getpid(), buildinfo.String(), os.Args[0])

	w := webview2.NewWithOptions(webview2.WebViewOptions{
		Debug: false,
		WindowOptions: webview2.WindowOptions{
			Title:  "ClassSend - Παρακολούθηση Τάξης  [" + buildinfo.String() + "]",
			Width:  1200,
			Height: 800,
			Center: true,
		},
	})
	if w == nil {
		fmt.Fprintln(os.Stderr, "WebView2 runtime not available. Install Microsoft Edge WebView2 Runtime.")
		os.Exit(1)
	}
	defer w.Destroy()
	wv = w

	// JS → Go bridge: the page calls onCellClick(idx) on click, idx = 0xFFFFFFFF
	// is the unfocus sentinel (matches the pipe protocol).
	if err := w.Bind("onCellClick", func(idx uint32) {
		// Best-effort, on the UI thread; pipeWriteAll has its own 2 s timeout
		// so this can't freeze the message pump.
		go sendFocusBackChannel(idx)
	}); err != nil {
		devlog.Logf("Bind onCellClick failed: %v", err)
	}

	// T-key always-on-top toggle, same shape as castviewer.
	if err := w.Bind("setTopmost", func(on bool) {
		hwnd := uintptr(unsafe.Pointer(w.Window()))
		setWindowTopmost(hwnd, on)
		devlog.Logf("setTopmost: on=%v hwnd=%x", on, hwnd)
	}); err != nil {
		devlog.Logf("Bind setTopmost failed: %v", err)
	}

	w.SetHtml(pageHTML)

	// Connect to the classsend pipe and start streaming frames.
	go func() {
		defer func() {
			if r := recover(); r != nil {
				devlog.Logf("pipe goroutine PANIC: %v\n%s", r, debug.Stack())
			}
		}()
		pipe, err := connectPipe()
		if err != nil {
			devlog.Logf("pipe connect FAILED: %v", err)
			dispatchEval(`setStatus('Δεν βρέθηκε ο δάσκαλος')`)
			return
		}
		pipeMu.Lock()
		pipeHandle = pipe
		pipeMu.Unlock()
		devlog.Logf("pipe connected, entering read loop")
		readPipe(pipe)
		devlog.Logf("read loop exited, closing pipe")
		pipeMu.Lock()
		pipeHandle = 0
		pipeMu.Unlock()
		procCloseHandle.Call(pipe)
	}()

	w.Run()
}
