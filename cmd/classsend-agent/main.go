// classsend-agent is the student background process.
// It runs hidden (no window), connects to the teacher over TCP, executes system
// commands (lock, mute, screenshot…) and relays chat messages to/from the TUI
// via a local IPC connection on 127.0.0.1:14789.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"sync"

	"classsend/internal/buildinfo"
	"classsend/internal/core"
	"classsend/internal/devlog"
	"classsend/internal/ipc"
	"classsend/internal/protocol"
)

var (
	tuiMu   sync.Mutex
	tuiConn net.Conn // current TUI connection; at most one at a time

	// tuiWriteMu serialises every write to tuiConn. Without it, replayHistoryToTUI
	// (history dump on TUI connect) races OnConnected/OnDisconnected/OnRawMessage
	// (event hooks fired from network goroutines) — both write JSON+newline frames
	// to the same loopback conn and the bytes can interleave. The TUI's
	// bufio.Scanner then reads a half-frame, fails to parse, and the read loop
	// exits → the TUI is stuck on "searching" until restart. This is the bug
	// where the agent is ready+connected but the student is not updated.
	tuiWriteMu sync.Mutex
)

// writeToTUI is the only path that touches tuiConn for output. The lock is
// held briefly across one WriteFrame call so concurrent producers can't tear
// each other's frames.
func writeToTUI(c net.Conn, f ipc.Frame) {
	if c == nil {
		return
	}
	tuiWriteMu.Lock()
	defer tuiWriteMu.Unlock()
	ipc.WriteFrame(c, f) //nolint:errcheck
}

func sendToTUI(f ipc.Frame) {
	tuiMu.Lock()
	c := tuiConn
	tuiMu.Unlock()
	writeToTUI(c, f)
}

func main() {
	dev := flag.Bool("dev", false, "Dev mode: skip autostart, scan localhost")
	showVer := flag.Bool("version", false, "Print version and exit")
	showVerShort := flag.Bool("ver", false, "Print version and exit (alias)")
	flag.Parse()

	if *showVer || *showVerShort {
		fmt.Println("ClassSend 2 Agent  " + buildinfo.String())
		os.Exit(0)
	}

	devlog.Init("agent")
	defer devlog.Close()
	devlog.Logf("startup  dev=%v  build=%s  exe=%s", *dev, buildinfo.String(), os.Args[0])

	hideConsole() // Windows: hide the console window so the agent is invisible

	dataDir := dataDirectory()
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		log.Fatalf("data dir: %v", err)
	}

	app, err := core.NewApp(core.RoleStudent, dataDir, *dev)
	if err != nil {
		log.Fatalf("init: %v", err)
	}

	// System commands: lock screen, mute, screenshot, etc.
	setupStudentCommands(app, *dev)
	app.SetAutostart = setAutostart
	app.IsAutostartEnabled = isAutostartEnabled
	if !*dev {
		ensureAutostart()
	}

	// Forward connection state changes to any connected TUI
	app.OnConnected = func() {
		devlog.Logf("connected to teacher")
		sendToTUI(ipc.Frame{Type: ipc.TypeConnected})
	}
	app.OnDisconnected = func() {
		devlog.Logf("disconnected from teacher")
		sendToTUI(ipc.Frame{Type: ipc.TypeDisconnected})
	}

	// Forward every received message verbatim to the TUI
	app.OnRawMessage = func(msg protocol.Message) {
		raw, err := json.Marshal(msg)
		if err != nil {
			return
		}
		sendToTUI(ipc.Frame{Type: ipc.TypeForward, Data: raw})
	}

	if err := app.StartStudent(); err != nil {
		log.Fatalf("start student: %v", err)
	}

	// IPC server — accept TUI connections
	ln, err := ipc.Listen()
	if err != nil {
		log.Fatalf("ipc listen: %v", err)
	}

	for {
		conn, err := ln.Accept()
		if err != nil {
			continue
		}

		tuiMu.Lock()
		if tuiConn != nil {
			tuiConn.Close() // replace old TUI with new one
		}
		tuiConn = conn
		tuiMu.Unlock()

		// Greet the new TUI: replay history + current state
		go func(c net.Conn) {
			replayHistoryToTUI(app, c)
			serveTUI(app, c)
		}(conn)
	}
}

// replayHistoryToTUI sends all stored messages and current class state to a
// freshly-connected TUI, so it starts with the full picture. All writes go
// through writeToTUI so they cannot interleave with concurrent event-hook
// writes (OnConnected, OnRawMessage, OnDisconnected) from network goroutines.
//
// We also re-check IsConnected() at the END and, if it flipped to true while
// we were replaying, send a fresh TypeConnected. This closes a TOCTOU window
// where the agent finished its TCP connect during the replay — without this,
// the TUI would have to wait for the next disconnect/reconnect to learn it's
// online (or be restarted).
func replayHistoryToTUI(app *core.App, conn net.Conn) {
	app.Mu().RLock()
	msgs := make([]core.ChatMessage, len(app.Messages))
	copy(msgs, app.Messages)
	state := app.State
	connectedAtStart := app.Client != nil && app.Client.IsConnected()
	app.Mu().RUnlock()

	if connectedAtStart {
		writeToTUI(conn, ipc.Frame{Type: ipc.TypeConnected})
	}

	// Replay chat history
	for _, cm := range msgs {
		msg, err := protocol.Encode(protocol.TypeChat, protocol.ChatPayload{
			ID:        cm.ID,
			From:      cm.From,
			Content:   cm.Content,
			Timestamp: cm.Timestamp.Unix(),
			Pinned:    cm.Pinned,
			FileID:    cm.FileID,
			FileName:  cm.FileName,
			FileSize:  cm.FileSize,
		})
		if err != nil {
			continue
		}
		raw, _ := json.Marshal(msg)
		writeToTUI(conn, ipc.Frame{Type: ipc.TypeForward, Data: raw})
	}

	// Replay current class state (blocked input, monitoring banner, etc.)
	stateMsg, err := protocol.Encode(protocol.TypeState, protocol.StatePayload{
		ChatBlocked:  state.ChatBlocked,
		FilesBlocked: state.FilesBlocked,
		HandsBlocked: state.HandsBlocked,
		ScreenLocked: state.ScreenLocked,
		Muted:        state.Muted,
		Monitoring:   state.Monitoring,
	})
	if err == nil {
		raw, _ := json.Marshal(stateMsg)
		writeToTUI(conn, ipc.Frame{Type: ipc.TypeForward, Data: raw})
	}

	// Re-check connection state. If we missed a transition during replay
	// (agent finished its TCP connect just now), re-emit TypeConnected so the
	// TUI leaves the "searching" screen. Idempotent on the TUI side.
	if !connectedAtStart && app.Client != nil && app.Client.IsConnected() {
		writeToTUI(conn, ipc.Frame{Type: ipc.TypeConnected})
		devlog.Logf("replay: late TypeConnected (transition during replay)")
	}
}

// serveTUI reads frames from the TUI and routes them to the teacher or local handlers.
func serveTUI(app *core.App, conn net.Conn) {
	defer func() {
		tuiMu.Lock()
		if tuiConn == conn {
			tuiConn = nil
		}
		tuiMu.Unlock()
		conn.Close()
	}()

	for f := range ipc.ReadFrames(conn) {
		switch f.Type {
		case ipc.TypeSend:
			var p ipc.SendPayload
			if json.Unmarshal(f.Data, &p) == nil && p.Text != "" {
				app.SendChat(p.Text) //nolint:errcheck
			}
		case ipc.TypeSetNickname:
			var p ipc.SetNicknamePayload
			if json.Unmarshal(f.Data, &p) == nil && p.Name != "" {
				app.SetNickname(p.Name) //nolint:errcheck
			}
		case ipc.TypeShowCast:
			showCastingViewer()
		}
	}
}

func dataDirectory() string {
	appData := os.Getenv("APPDATA")
	if appData == "" {
		appData, _ = os.UserHomeDir()
	}
	return filepath.Join(appData, "ClassSend")
}
