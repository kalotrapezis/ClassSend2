// classsend-agent is the student background process.
// It runs hidden (no window), connects to the teacher over TCP, executes system
// commands (lock, mute, screenshot…) and relays chat messages to/from the TUI
// via a local IPC connection on 127.0.0.1:14789.
package main

import (
	"encoding/json"
	"flag"
	"log"
	"net"
	"os"
	"path/filepath"
	"sync"

	"classsend/internal/core"
	"classsend/internal/ipc"
	"classsend/internal/protocol"
)

var (
	tuiMu   sync.Mutex
	tuiConn net.Conn // current TUI connection; at most one at a time
)

func sendToTUI(f ipc.Frame) {
	tuiMu.Lock()
	c := tuiConn
	tuiMu.Unlock()
	if c != nil {
		ipc.WriteFrame(c, f) //nolint:errcheck
	}
}

func main() {
	dev := flag.Bool("dev", false, "Dev mode: skip autostart, scan localhost")
	flag.Parse()

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
		sendToTUI(ipc.Frame{Type: ipc.TypeConnected})
	}
	app.OnDisconnected = func() {
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
// freshly-connected TUI, so it starts with the full picture.
func replayHistoryToTUI(app *core.App, conn net.Conn) {
	app.Mu().RLock()
	msgs := make([]core.ChatMessage, len(app.Messages))
	copy(msgs, app.Messages)
	state := app.State
	connected := app.Client != nil && app.Client.IsConnected()
	app.Mu().RUnlock()

	if connected {
		ipc.WriteFrame(conn, ipc.Frame{Type: ipc.TypeConnected}) //nolint:errcheck
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
		ipc.WriteFrame(conn, ipc.Frame{Type: ipc.TypeForward, Data: raw}) //nolint:errcheck
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
		ipc.WriteFrame(conn, ipc.Frame{Type: ipc.TypeForward, Data: raw}) //nolint:errcheck
	}
}

// serveTUI reads TypeSend frames from the TUI and routes them to the teacher.
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
		if f.Type == ipc.TypeSend {
			var p ipc.SendPayload
			if json.Unmarshal(f.Data, &p) == nil && p.Text != "" {
				app.SendChat(p.Text) //nolint:errcheck
			}
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
