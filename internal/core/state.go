package core

import (
	"archive/zip"
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"classsend/internal/ipc"
	"classsend/internal/network"
	"classsend/internal/protocol"
)

type Role string

const (
	RoleTeacher Role = "teacher"
	RoleStudent Role = "student"
)

// ClassState holds the current class-wide flags — sent to late joiners on connect
type ClassState struct {
	ChatBlocked  bool
	FilesBlocked bool
	HandsBlocked bool
	ScreenLocked bool
	Muted        bool
	Monitoring   bool
	Casting      bool
}

// ChatMessage is a stored chat message
type ChatMessage struct {
	ID        string
	From      string
	Content   string
	Timestamp time.Time
	Pinned    bool
	Blocked   bool
	Reported  bool
	FileID      string // non-empty if this message carries a file attachment
	FileName    string // original filename for display
	FileSize    int64  // size of attached file in bytes
	MediaPinned bool   // pinned at top of teacher's media library (teacher-local, not broadcast)
}

// pendingFile accumulates chunks for an in-progress file receive
type pendingFile struct {
	name     string
	size     int64
	autoOpen bool
	chunks   map[int][]byte
}

const fileChunkSize = 32 * 1024 // 32 KB per chunk

// App is the central state — owns all subsystems, wires them together
type App struct {
	mu sync.RWMutex

	Role     Role
	Nickname string
	Hostname string
	DataDir  string

	// Network subsystems
	Cache    *network.MACCache
	Server   *network.Server        // teacher only
	Scanner  *network.Scanner       // teacher only
	Client   *network.Client        // student only
	Probe    *network.ProbeListener // student only

	agentConn net.Conn // set when student TUI talks to background agent instead of teacher directly

	devMode    bool
	scanCancel context.CancelFunc

	Blacklist []string
	Whitelist []string

	// Class state (teacher tracks authoritative state; student mirrors it)
	State    ClassState
	Messages []ChatMessage

	pendingFiles sync.Map // fileID → *pendingFile (in-progress receive)

	// Events — UI layer subscribes to these
	OnStudentJoin      func(*network.Student)
	OnStudentLeave     func(*network.Student)
	OnChatMessage      func(ChatMessage)
	OnCommand          func(protocol.CommandPayload)
	OnStateChange      func(ClassState)
	OnConnected        func()
	OnDisconnected     func()
	OnMessagesUpdated  func([]ChatMessage)
	OnStudentMissing   func(mac, nickname, hostname string, count int)
	OnFileReceived     func(fileID, name string)
	OnSysMsg           func(text string) // student-side system notifications
	OnScreenshot       func(studentID string, jpegData []byte)
	OnCommandFailed    func(nickname, action, errMsg string) // teacher-side: student reported failure after retries
	OnRawMessage       func(protocol.Message)               // agent-side: forward every received message to TUI pipe

	// Monitoring session hooks (teacher side only)
	OnMonitoringStart func() // called when monitoring transitions to active
	OnMonitoringStop  func() // called when monitoring transitions to inactive

	// Casting hooks — teacher side only.
	// OnStartCasting is called when casting begins; it should start the dedicated
	// cast TCP server and return its LAN address ("host:port").
	// OnStopCasting is called when casting ends; it should stop the server.
	OnStartCasting func() (string, error)
	OnStopCasting  func()

	// Platform settings hooks (student side only, wired by main)
	SetAutostart       func(enable bool) error
	IsAutostartEnabled func() bool
}

func NewApp(role Role, dataDir string, devMode bool) (*App, error) {
	hostname, _ := os.Hostname()
	if hostname == "" {
		hostname = "PC"
	}

	cache := network.NewMACCache(dataDir)

	// Teacher is always "Δάσκαλος" — students use their machine hostname.
	// In dev mode both roles run on the same machine, so using hostname for both
	// would make From fields identical; role names guarantee uniqueness.
	nickname := hostname
	if role == RoleTeacher {
		nickname = "Δάσκαλος"
	}

	app := &App{
		Role:     role,
		Nickname: nickname,
		Hostname: hostname,
		DataDir:  dataDir,
		Cache:    cache,
		devMode:  devMode,
	}
	app.loadSettings() // override defaults with any persisted settings
	return app, nil
}

// StartTeacher initialises the server and scanner — call after NewApp for teacher role
func (a *App) StartTeacher() error {
	a.loadMessages()
	a.loadLists()

	srv := network.NewServer()

	// Wire server events
	srv.OnJoin = func(st *network.Student) {
		// Update MAC cache with latest IP and identity
		a.Cache.Upsert(st.MAC, st.IP, st.Nickname, st.Hostname)

		// Send current class state so late joiners are in sync
		a.mu.RLock()
		state := a.State
		bl := append([]string(nil), a.Blacklist...)
		wl := append([]string(nil), a.Whitelist...)
		a.mu.RUnlock()
		stateMsg, _ := protocol.Encode(protocol.TypeState, protocol.StatePayload{
			ChatBlocked:  state.ChatBlocked,
			FilesBlocked: state.FilesBlocked,
			HandsBlocked: state.HandsBlocked,
			ScreenLocked: state.ScreenLocked,
			Muted:        state.Muted,
			Monitoring:   state.Monitoring,
			Casting:      state.Casting,
			Blacklist:    bl,
			Whitelist:    wl,
		})
		st.Send(stateMsg)

		// Replay message history so late/re-joiners are in sync.
		// For file messages, also re-stream the chunks so the student has the file locally.
		a.mu.RLock()
		history := make([]ChatMessage, len(a.Messages))
		copy(history, a.Messages)
		a.mu.RUnlock()
		for _, cm := range history {
			hm, err := protocol.Encode(protocol.TypeChat, protocol.ChatPayload{
				ID:        cm.ID,
				From:      cm.From,
				Content:   cm.Content,
				Timestamp: cm.Timestamp.Unix(),
				Pinned:    cm.Pinned,
				FileID:    cm.FileID,
				FileName:  cm.FileName,
				FileSize:  cm.FileSize,
			})
			if err == nil {
				st.Send(hm)
			}
			// Re-send file bytes if the teacher still has them on disk
			if cm.FileID != "" && a.HasFile(cm.FileID, cm.FileName) {
				go a.sendChunksServer(cm.FileID, cm.FileName, cm.FileSize,
					mustReadFile(a.GetFilePath(cm.FileID, cm.FileName)),
					st.ID, false)
			}
		}

		if a.OnStudentJoin != nil {
			a.OnStudentJoin(st)
		}
	}

	srv.OnLeave = func(st *network.Student) {
		// Queue this IP for fast reprobing
		a.Scanner.AddRetry(st.IP)

		if a.OnStudentLeave != nil {
			a.OnStudentLeave(st)
		}
	}

	srv.OnMessage = func(st *network.Student, msg protocol.Message) {
		a.handleStudentMessage(st, msg)
	}

	if err := srv.Start(network.ServerPort); err != nil {
		return fmt.Errorf("start server: %w", err)
	}
	a.Server = srv

	// Determine our server address for the scanner probes
	nics := network.GetLocalNICs()
	if len(nics) == 0 {
		return fmt.Errorf("no active network interfaces found")
	}
	serverAddr := fmt.Sprintf("%s:%d", nics[0].IP.String(), network.ServerPort)

	// onFound is called when scanner gets a handshake preview from a student
	// The student will connect back to us on their own — we just log the preview
	onFound := func(ip string, hs protocol.HandshakePayload) {
		// Cache the identity even before full connection
		a.Cache.Upsert(hs.MAC, ip, hs.Nickname, hs.Hostname)
	}

	scanner := network.NewScanner(serverAddr, a.devMode, onFound)
	scanner.OnMissing = func(mac, nickname, hostname string, count int) {
		if a.OnStudentMissing != nil {
			a.OnStudentMissing(mac, nickname, hostname, count)
		}
	}
	a.Scanner = scanner

	ctx, cancel := context.WithCancel(context.Background())
	a.scanCancel = cancel
	go scanner.RunLoop(ctx, a.Cache)

	return nil
}

// StartStudent initialises the probe listener and client — call after NewApp for student role
func (a *App) StartStudent() error {
	getHS := func() protocol.HandshakePayload {
		a.mu.RLock()
		defer a.mu.RUnlock()
		return protocol.HandshakePayload{
			MAC:      network.PrimaryMAC(),
			Hostname: a.Hostname,
			Nickname: a.Nickname,
			Role:     string(RoleStudent),
		}
	}

	client := network.NewClient()
	client.OnMessage = func(msg protocol.Message) {
		a.handleTeacherMessage(msg)
	}
	client.OnDisconnect = func() {
		if a.OnDisconnected != nil {
			a.OnDisconnected()
		}
		// Probe listener is still running — teacher will find us again on next scan
	}
	a.Client = client

	probe := network.NewProbeListener(getHS, func(serverAddr string) {
		if client.IsConnected() {
			return // already connected, ignore duplicate probes
		}
		hs := getHS()
		if err := client.Connect(serverAddr, hs); err != nil {
			return // will be probed again on next scan cycle
		}
		if a.OnConnected != nil {
			a.OnConnected()
		}
	})

	if err := probe.Start(); err != nil {
		return fmt.Errorf("start probe listener: %w", err)
	}
	a.Probe = probe

	return nil
}

// Mu returns the App's mutex so the agent can lock it for history replay.
func (a *App) Mu() *sync.RWMutex { return &a.mu }

// HasAgentConn reports whether the student TUI is connected to a background agent.
func (a *App) HasAgentConn() bool { return a.agentConn != nil }

// SendNicknameUpdateToAgent pushes a nickname change to the background agent so its
// outgoing chat messages use the updated name immediately (no restart needed).
func (a *App) SendNicknameUpdateToAgent(name string) {
	if a.agentConn == nil {
		return
	}
	raw, _ := json.Marshal(ipc.SetNicknamePayload{Name: name})
	ipc.WriteFrame(a.agentConn, ipc.Frame{Type: ipc.TypeSetNickname, Data: raw}) //nolint:errcheck
}

// SendShowCast asks the background agent to re-show the cast viewer window.
func (a *App) SendShowCast() {
	if a.agentConn == nil {
		return
	}
	ipc.WriteFrame(a.agentConn, ipc.Frame{Type: ipc.TypeShowCast}) //nolint:errcheck
}

// CheckBlacklist returns the first blacklist word that fuzzy-matches a word in
// content, or "" if the message is clean.  Whitelist entries always pass.
// Matching uses Levenshtein distance scaled to word length to catch typos.
func (a *App) CheckBlacklist(content string) string {
	a.mu.RLock()
	blacklist := append([]string(nil), a.Blacklist...)
	whitelist := append([]string(nil), a.Whitelist...)
	a.mu.RUnlock()

	if len(blacklist) == 0 {
		return ""
	}

	for _, mw := range strings.Fields(strings.ToLower(content)) {
		mwRunes := []rune(mw)
		if len(mwRunes) < 3 {
			continue // too short — too many false positives
		}
		// whitelist override: if the word exactly matches a whitelist entry, skip it
		whitelisted := false
		for _, ww := range whitelist {
			if strings.EqualFold(mw, ww) {
				whitelisted = true
				break
			}
		}
		if whitelisted {
			continue
		}
		for _, bw := range blacklist {
			bwRunes := []rune(strings.ToLower(bw))
			if len(bwRunes) < 2 {
				if strings.EqualFold(mw, bw) {
					return bw
				}
				continue
			}
			// threshold: 0 for ≤4-char words, 1 for ≤7, 2 for ≤10, 3 for longer
			threshold := 0
			switch {
			case len(bwRunes) > 10:
				threshold = 3
			case len(bwRunes) > 7:
				threshold = 2
			case len(bwRunes) > 4:
				threshold = 1
			}
			if levenshtein(mwRunes, bwRunes) <= threshold {
				return bw
			}
		}
	}
	return ""
}

func levenshtein(a, b []rune) int {
	la, lb := len(a), len(b)
	if la == 0 {
		return lb
	}
	if lb == 0 {
		return la
	}
	prev := make([]int, lb+1)
	curr := make([]int, lb+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= la; i++ {
		curr[0] = i
		for j := 1; j <= lb; j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			curr[j] = min3(curr[j-1]+1, prev[j]+1, prev[j-1]+cost)
		}
		prev, curr = curr, prev
	}
	return prev[lb]
}

func min3(a, b, c int) int {
	if a < b {
		if a < c {
			return a
		}
		return c
	}
	if b < c {
		return b
	}
	return c
}

// ConnectViaAgent connects the student TUI to the background agent instead of
// talking to the teacher directly.  The agent handles all TCP + system commands;
// the TUI just renders events and sends chat through the pipe.
func (a *App) ConnectViaAgent(conn net.Conn) {
	a.agentConn = conn

	go func() {
		defer conn.Close()
		scanner := bufio.NewScanner(conn)
		scanner.Buffer(make([]byte, 4<<20), 4<<20)
		for scanner.Scan() {
			var f ipc.Frame
			if json.Unmarshal(scanner.Bytes(), &f) != nil {
				continue
			}
			switch f.Type {
			case ipc.TypeConnected:
				if a.OnConnected != nil {
					a.OnConnected()
				}
			case ipc.TypeDisconnected:
				if a.OnDisconnected != nil {
					a.OnDisconnected()
				}
			case ipc.TypeForward:
				var msg protocol.Message
				if json.Unmarshal(f.Data, &msg) == nil {
					a.handleTeacherMessage(msg)
				}
			}
		}
		// Pipe closed — agent went away
		if a.OnDisconnected != nil {
			a.OnDisconnected()
		}
	}()
}

// SendChat sends a chat message (teacher broadcasts, student sends to teacher)
func (a *App) SendChat(content string) error {
	msg, err := protocol.Encode(protocol.TypeChat, protocol.ChatPayload{
		From:      a.Nickname,
		Content:   content,
		Timestamp: time.Now().Unix(),
	})
	if err != nil {
		return err
	}

	if a.Role == RoleTeacher {
		a.Server.Broadcast(msg)
		cm := ChatMessage{
			ID:        newMsgID(),
			From:      a.Nickname,
			Content:   content,
			Timestamp: time.Now(),
		}
		a.mu.Lock()
		a.Messages = append(a.Messages, cm)
		a.mu.Unlock()
		if a.OnChatMessage != nil {
			a.OnChatMessage(cm)
		}
		go a.saveMessages()
	} else if a.agentConn != nil {
		// In agent-backed mode: ask the agent to forward to teacher
		raw, _ := json.Marshal(ipc.SendPayload{Text: content})
		return ipc.WriteFrame(a.agentConn, ipc.Frame{Type: ipc.TypeSend, Data: raw})
	} else {
		return a.Client.Send(msg)
	}
	return nil
}

// SendCmdAck sends a command execution result back to the teacher — student only.
// Called after all retry attempts; only sent on failure (success is silent).
func (a *App) SendCmdAck(cmdID, action string, execErr error) {
	if a.Client == nil {
		return
	}
	errMsg := ""
	if execErr != nil {
		errMsg = execErr.Error()
	}
	ack, _ := protocol.Encode(protocol.TypeCmdAck, protocol.CmdAckPayload{
		CmdID:  cmdID,
		Action: action,
		OK:     execErr == nil,
		Error:  errMsg,
	})
	a.Client.Send(ack) //nolint:errcheck
}

// SendCommand sends a class control command — teacher only
func (a *App) SendCommand(action, param, targetStudentID string) error {
	if a.Role != RoleTeacher {
		return fmt.Errorf("only teacher can send commands")
	}

	cmdID := newMsgID()
	payload := protocol.CommandPayload{
		CmdID:  cmdID,
		Target: targetStudentID,
		Action: action,
		Param:  param,
	}
	msg, err := protocol.Encode(protocol.TypeCommand, payload)
	if err != nil {
		return err
	}

	if targetStudentID == "" {
		a.Server.Broadcast(msg)
	} else {
		return a.Server.Send(targetStudentID, msg)
	}

	// Update local class state and push it to all students
	a.updateClassState(action)
	a.broadcastState()

	// Clear teacher's own message list too
	if action == protocol.CmdClearChat {
		a.mu.Lock()
		a.Messages = nil
		a.mu.Unlock()
		if a.OnMessagesUpdated != nil {
			a.OnMessagesUpdated(nil)
		}
	}
	return nil
}

func (a *App) broadcastState() {
	a.mu.RLock()
	state := a.State
	bl := append([]string(nil), a.Blacklist...)
	wl := append([]string(nil), a.Whitelist...)
	a.mu.RUnlock()
	stateMsg, err := protocol.Encode(protocol.TypeState, protocol.StatePayload{
		ChatBlocked:  state.ChatBlocked,
		FilesBlocked: state.FilesBlocked,
		HandsBlocked: state.HandsBlocked,
		ScreenLocked: state.ScreenLocked,
		Muted:        state.Muted,
		Monitoring:   state.Monitoring,
		Casting:      state.Casting,
		Blacklist:    bl,
		Whitelist:    wl,
	})
	if err == nil {
		a.Server.Broadcast(stateMsg)
	}
}

func (a *App) updateClassState(action string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	switch action {
	case protocol.CmdLockScreen:
		a.State.ScreenLocked = true
	case protocol.CmdUnlockScreen:
		a.State.ScreenLocked = false
	case protocol.CmdBlockChat:
		a.State.ChatBlocked = true
	case protocol.CmdUnblockChat:
		a.State.ChatBlocked = false
	case protocol.CmdBlockFiles:
		a.State.FilesBlocked = true
	case protocol.CmdUnblockFiles:
		a.State.FilesBlocked = false
	case protocol.CmdMute:
		a.State.Muted = true
	case protocol.CmdUnmute:
		a.State.Muted = false
	case protocol.CmdStartMonitor:
		if !a.State.Monitoring {
			a.State.Monitoring = true
			if a.OnMonitoringStart != nil {
				go a.OnMonitoringStart()
			}
		}
	case protocol.CmdStopMonitor:
		if a.State.Monitoring {
			a.State.Monitoring = false
			if a.OnMonitoringStop != nil {
				go a.OnMonitoringStop()
			}
		}
	case protocol.CmdStartCast:
		a.State.Casting = true
	case protocol.CmdStopCast:
		a.State.Casting = false
	}
	if a.OnStateChange != nil {
		a.OnStateChange(a.State)
	}
}

func (a *App) handleStudentMessage(st *network.Student, msg protocol.Message) {
	switch msg.Type {
	case protocol.TypeChat:
		payload, err := protocol.Decode[protocol.ChatPayload](msg)
		if err != nil {
			return
		}
		a.mu.RLock()
		blocked := a.State.ChatBlocked
		a.mu.RUnlock()
		if blocked {
			return
		}
		// Server-side blacklist enforcement — catches bypasses of client-side check
		if match := a.CheckBlacklist(payload.Content); match != "" {
			cm := ChatMessage{
				ID:        newMsgID(),
				From:      payload.From,
				Content:   payload.Content,
				Timestamp: time.Unix(payload.Timestamp, 0),
				Blocked:   true,
			}
			a.mu.Lock()
			a.Messages = append(a.Messages, cm)
			a.mu.Unlock()
			if a.OnChatMessage != nil {
				a.OnChatMessage(cm) // teacher sees it with [🚫] marker
			}
			go a.saveMessages()
			return
		}
		cm := ChatMessage{
			ID:        newMsgID(),
			From:      payload.From,
			Content:   payload.Content,
			Timestamp: time.Unix(payload.Timestamp, 0),
			FileID:    payload.FileID,
			FileName:  payload.FileName,
			FileSize:  payload.FileSize,
		}
		a.mu.Lock()
		a.Messages = append(a.Messages, cm)
		a.mu.Unlock()
		// Broadcast to all other students
		a.Server.Broadcast(msg)
		if a.OnChatMessage != nil {
			a.OnChatMessage(cm)
		}
		go a.saveMessages()

	case protocol.TypeCmdAck:
		ack, err := protocol.Decode[protocol.CmdAckPayload](msg)
		if err != nil {
			return
		}
		if !ack.OK && a.OnCommandFailed != nil {
			a.OnCommandFailed(st.Nickname, ack.Action, ack.Error)
		}

	case protocol.TypeReport:
		payload, err := protocol.Decode[protocol.ReportPayload](msg)
		if err != nil {
			return
		}
		a.mu.Lock()
		for i := range a.Messages {
			if a.Messages[i].ID == payload.MessageID {
				a.Messages[i].Reported = true
				break
			}
		}
		snapshot := a.Messages
		a.mu.Unlock()
		if a.OnMessagesUpdated != nil {
			a.OnMessagesUpdated(snapshot)
		}

	case protocol.TypeFileHdr:
		payload, err := protocol.Decode[protocol.FileHdrPayload](msg)
		if err != nil {
			return
		}
		a.pendingFiles.Store(payload.FileID, &pendingFile{
			name:   payload.Name,
			size:   payload.Size,
			chunks: make(map[int][]byte),
		})

	case protocol.TypeFileChunk:
		payload, err := protocol.Decode[protocol.FileChunkPayload](msg)
		if err != nil {
			return
		}
		if v, ok := a.pendingFiles.Load(payload.FileID); ok {
			v.(*pendingFile).chunks[payload.Index] = payload.Data
		}

	case protocol.TypeFileEnd:
		payload, err := protocol.Decode[protocol.FileEndPayload](msg)
		if err != nil {
			return
		}
		if v, ok := a.pendingFiles.LoadAndDelete(payload.FileID); ok {
			go a.assembleAndStore(payload.FileID, v.(*pendingFile))
		}

	case protocol.TypeHandRaise:
		// TODO: dedicated handler

	case protocol.TypeScreenshot:
		payload, err := protocol.Decode[protocol.ScreenshotPayload](msg)
		if err != nil {
			return
		}
		if a.OnScreenshot != nil {
			a.OnScreenshot(payload.StudentID, payload.Data)
		}
	}
}

func (a *App) handleTeacherMessage(msg protocol.Message) {
	switch msg.Type {
	case protocol.TypeChat:
		payload, err := protocol.Decode[protocol.ChatPayload](msg)
		if err != nil {
			return
		}
		id := payload.ID
		if id == "" {
			id = newMsgID()
		}
		// Skip duplicates (history replay of already-known messages)
		a.mu.RLock()
		for _, existing := range a.Messages {
			if existing.ID == id {
				a.mu.RUnlock()
				return
			}
		}
		a.mu.RUnlock()
		cm := ChatMessage{
			ID:        id,
			From:      payload.From,
			Content:   payload.Content,
			Timestamp: time.Unix(payload.Timestamp, 0),
			Pinned:    payload.Pinned,
			FileID:    payload.FileID,
			FileName:  payload.FileName,
			FileSize:  payload.FileSize,
		}
		a.mu.Lock()
		a.Messages = append(a.Messages, cm)
		a.mu.Unlock()
		if a.OnChatMessage != nil {
			a.OnChatMessage(cm)
		}

	case protocol.TypeCommand:
		payload, err := protocol.Decode[protocol.CommandPayload](msg)
		if err != nil {
			return
		}

		if payload.Action == protocol.CmdPushOpen {
			// Agent executes this; TUI in agent-backed mode skips re-execution
			if a.agentConn == nil {
				go exec.Command("cmd", "/c", "start", "", payload.Param).Start()
			}
			return
		}

		if payload.Action == protocol.CmdClearChat {
			a.mu.Lock()
			a.Messages = nil
			a.mu.Unlock()
			if a.OnMessagesUpdated != nil {
				a.OnMessagesUpdated(nil)
			}
		}

		if a.OnCommand != nil {
			a.OnCommand(payload)
		}

	case protocol.TypePin:
		payload, err := protocol.Decode[protocol.PinPayload](msg)
		if err != nil {
			return
		}
		a.mu.Lock()
		for i := range a.Messages {
			if a.Messages[i].ID == payload.MsgID {
				a.Messages[i].Pinned = payload.Pinned
				break
			}
		}
		snapshot := make([]ChatMessage, len(a.Messages))
		copy(snapshot, a.Messages)
		a.mu.Unlock()
		if a.OnMessagesUpdated != nil {
			a.OnMessagesUpdated(snapshot)
		}

	case protocol.TypeState:
		payload, err := protocol.Decode[protocol.StatePayload](msg)
		if err != nil {
			return
		}
		a.mu.Lock()
		a.State = ClassState{
			ChatBlocked:  payload.ChatBlocked,
			FilesBlocked: payload.FilesBlocked,
			HandsBlocked: payload.HandsBlocked,
			ScreenLocked: payload.ScreenLocked,
			Muted:        payload.Muted,
			Monitoring:   payload.Monitoring,
			Casting:      payload.Casting,
		}
		// Sync blacklist/whitelist from teacher (enables client-side blocking)
		if len(payload.Blacklist) > 0 {
			a.Blacklist = payload.Blacklist
		}
		if len(payload.Whitelist) > 0 {
			a.Whitelist = payload.Whitelist
		}
		a.mu.Unlock()
		if a.OnStateChange != nil {
			a.OnStateChange(a.State)
		}

	case protocol.TypeFileHdr:
		payload, err := protocol.Decode[protocol.FileHdrPayload](msg)
		if err != nil {
			return
		}
		a.pendingFiles.Store(payload.FileID, &pendingFile{
			name:     payload.Name,
			size:     payload.Size,
			autoOpen: payload.AutoOpen,
			chunks:   make(map[int][]byte),
		})

	case protocol.TypeFileChunk:
		payload, err := protocol.Decode[protocol.FileChunkPayload](msg)
		if err != nil {
			return
		}
		if v, ok := a.pendingFiles.Load(payload.FileID); ok {
			v.(*pendingFile).chunks[payload.Index] = payload.Data
		}

	case protocol.TypeFileEnd:
		payload, err := protocol.Decode[protocol.FileEndPayload](msg)
		if err != nil {
			return
		}
		if v, ok := a.pendingFiles.LoadAndDelete(payload.FileID); ok {
			go a.assembleAndStore(payload.FileID, v.(*pendingFile))
		}

	}

	// Forward the raw message to the TUI pipe (agent side only)
	if a.OnRawMessage != nil {
		a.OnRawMessage(msg)
	}
}

// SendReport reports a specific message by ID (student side)
func (a *App) SendReport(msgID string) error {
	a.mu.RLock()
	var target *ChatMessage
	for i := range a.Messages {
		if a.Messages[i].ID == msgID {
			target = &a.Messages[i]
			break
		}
	}
	a.mu.RUnlock()

	if target == nil {
		return fmt.Errorf("message not found")
	}

	msg, err := protocol.Encode(protocol.TypeReport, protocol.ReportPayload{
		MessageID: target.ID,
		Content:   target.Content,
		From:      target.From,
		Word:      "",
	})
	if err != nil {
		return err
	}
	return a.Client.Send(msg)
}

// DeleteMessage deletes a message by ID directly — teacher only, no report needed
func (a *App) DeleteMessage(msgID string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	for i := range a.Messages {
		if a.Messages[i].ID == msgID {
			a.Messages = append(a.Messages[:i], a.Messages[i+1:]...)
			if a.OnMessagesUpdated != nil {
				a.OnMessagesUpdated(a.Messages)
			}
			go a.saveMessages()
			return nil
		}
	}
	return fmt.Errorf("message not found")
}

// AddToBlacklist appends words to the blacklist (teacher side)
func (a *App) AddToBlacklist(words []string) {
	a.mu.Lock()
	a.Blacklist = append(a.Blacklist, words...)
	a.mu.Unlock()
	go a.saveLists()
	if a.Server != nil {
		a.broadcastState()
	}
}

// RemoveBlacklistEntry removes the entry at position i (0-based) from the blacklist.
func (a *App) RemoveBlacklistEntry(i int) error {
	a.mu.Lock()
	if i < 0 || i >= len(a.Blacklist) {
		a.mu.Unlock()
		return fmt.Errorf("δεν υπάρχει @B%d", i+1)
	}
	a.Blacklist = append(a.Blacklist[:i], a.Blacklist[i+1:]...)
	a.mu.Unlock()
	go a.saveLists()
	if a.Server != nil {
		a.broadcastState()
	}
	return nil
}

// AddToWhitelist appends words to the whitelist (teacher side)
func (a *App) AddToWhitelist(words []string) {
	a.mu.Lock()
	a.Whitelist = append(a.Whitelist, words...)
	a.mu.Unlock()
	go a.saveLists()
	if a.Server != nil {
		a.broadcastState()
	}
}

// RemoveWhitelistEntry removes the entry at position i (0-based) from the whitelist.
func (a *App) RemoveWhitelistEntry(i int) error {
	a.mu.Lock()
	if i < 0 || i >= len(a.Whitelist) {
		a.mu.Unlock()
		return fmt.Errorf("δεν υπάρχει @W%d", i+1)
	}
	a.Whitelist = append(a.Whitelist[:i], a.Whitelist[i+1:]...)
	a.mu.Unlock()
	go a.saveLists()
	if a.Server != nil {
		a.broadcastState()
	}
	return nil
}

// ClearReport clears the report flag on a message without deleting it (teacher side)
func (a *App) ClearReport(msgID string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	for i := range a.Messages {
		if a.Messages[i].ID == msgID {
			a.Messages[i].Reported = false
			if a.OnMessagesUpdated != nil {
				a.OnMessagesUpdated(a.Messages)
			}
			go a.saveMessages()
			return nil
		}
	}
	return fmt.Errorf("message not found")
}

// PinMessage pins a message by ID (teacher side) and broadcasts to students
func (a *App) PinMessage(msgID string) error {
	a.mu.Lock()
	for i := range a.Messages {
		if a.Messages[i].ID == msgID {
			a.Messages[i].Pinned = true
			snapshot := make([]ChatMessage, len(a.Messages))
			copy(snapshot, a.Messages)
			a.mu.Unlock()
			a.broadcastPin(msgID, true)
			if a.OnMessagesUpdated != nil {
				a.OnMessagesUpdated(snapshot)
			}
			go a.saveMessages()
			return nil
		}
	}
	a.mu.Unlock()
	return fmt.Errorf("message not found")
}

// UnpinMessage clears the pin on a message and broadcasts to students
func (a *App) UnpinMessage(msgID string) error {
	a.mu.Lock()
	for i := range a.Messages {
		if a.Messages[i].ID == msgID {
			a.Messages[i].Pinned = false
			snapshot := make([]ChatMessage, len(a.Messages))
			copy(snapshot, a.Messages)
			a.mu.Unlock()
			a.broadcastPin(msgID, false)
			if a.OnMessagesUpdated != nil {
				a.OnMessagesUpdated(snapshot)
			}
			go a.saveMessages()
			return nil
		}
	}
	a.mu.Unlock()
	return fmt.Errorf("message not found")
}

// PinLastMessage pins the most recent non-system message and broadcasts to students
func (a *App) PinLastMessage() error {
	a.mu.Lock()
	for i := len(a.Messages) - 1; i >= 0; i-- {
		if a.Messages[i].From != "system" {
			a.Messages[i].Pinned = true
			msgID := a.Messages[i].ID
			snapshot := make([]ChatMessage, len(a.Messages))
			copy(snapshot, a.Messages)
			a.mu.Unlock()
			a.broadcastPin(msgID, true)
			if a.OnMessagesUpdated != nil {
				a.OnMessagesUpdated(snapshot)
			}
			go a.saveMessages()
			return nil
		}
	}
	a.mu.Unlock()
	return fmt.Errorf("no message to pin")
}

func (a *App) broadcastPin(msgID string, pinned bool) {
	if a.Server == nil {
		return
	}
	msg, err := protocol.Encode(protocol.TypePin, protocol.PinPayload{
		MsgID:  msgID,
		Pinned: pinned,
	})
	if err != nil {
		return
	}
	a.Server.Broadcast(msg)
}

func (a *App) GetMessages() []ChatMessage {
	a.mu.RLock()
	defer a.mu.RUnlock()
	out := make([]ChatMessage, len(a.Messages))
	copy(out, a.Messages)
	return out
}

type listsFile struct {
	Blacklist []string `json:"blacklist"`
	Whitelist []string `json:"whitelist"`
}

func (a *App) saveLists() {
	a.mu.RLock()
	data, err := json.Marshal(listsFile{Blacklist: a.Blacklist, Whitelist: a.Whitelist})
	a.mu.RUnlock()
	if err != nil {
		return
	}
	os.WriteFile(filepath.Join(a.DataDir, "lists.json"), data, 0644)
}

// defaultBlacklist is pre-loaded when no lists.json exists yet.
var defaultBlacklist = []string{
	".\n🖕", "μαλάκακα αλβανέ", "sone of a beac", "κόλος", "μαλάκκακακακα",
	"καραγκίοζη", "γελίε", "άχριστε", "παπάρας", "ηλίθιο", "χαζός", "🤕😈🖕",
	"καραγκιόζηςςς", "μακακια", "κακκα", "κκαακα", "κακα", "κακά", "μακακία",
	"makakia", "ο γιωργος ειναι χαζός", "μαλάκακα", "πούτσο", "πίπα", "χαζή",
	"γίφτος", "γαμημένο", "βλαμένο", "βλαμαίνο", "περιορισμένης", "ευθήνης",
	"αρχίδι", "stupid", "bitch", "fuck", "sex", "moring", "φαψκ", "βλαμμένο",
	"πόρνη", "μακάκα", "γαμότο", "πουτσας", "παπαρ", "πούτσα", "παπάρα",
	"fustis", "farts", "dics", "fuccsake", "beech",
}

// defaultWhitelist is pre-loaded when no lists.json exists yet.
var defaultWhitelist = []string{
	"ασδ", "σδσδ", "κάνετε", "κοντός", "καμήλα", "βηβλίο", "βόμβα", "πίτσα",
	"ρούφα", "σάκς", "μασκα", "καλημέρα", "students", "tudent", "student",
	"μαθητές", "studies", "μαθήματα", "μαθήτριας", "φακός", "μαδέρι", "πατέρας",
	"faster", "fast", "parasite", "καλή", "γεια", "what", "περάσετε", "γιεα",
	"γειά", "ghg", "gfd", "xcvcxv", "ccvxvcxv", "cxc", "hi", "καλά", "τασάκι",
	"ηι", "wolf", "go go", "hello",
}

func (a *App) loadLists() {
	data, err := os.ReadFile(filepath.Join(a.DataDir, "lists.json"))
	if err != nil {
		// No file yet — seed with built-in defaults and persist them
		a.mu.Lock()
		a.Blacklist = append([]string(nil), defaultBlacklist...)
		a.Whitelist = append([]string(nil), defaultWhitelist...)
		a.mu.Unlock()
		go a.saveLists()
		return
	}
	var lf listsFile
	if json.Unmarshal(data, &lf) == nil {
		a.mu.Lock()
		a.Blacklist = lf.Blacklist
		a.Whitelist = lf.Whitelist
		a.mu.Unlock()
	}
}

// parseWordList handles both formats:
//   new: ["word1", "word2"]
//   old: [{"word":"word1","addedAt":"...","source":"..."}]
func parseWordList(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	var strs []string
	if json.Unmarshal(raw, &strs) == nil {
		return strs
	}
	var objs []struct {
		Word string `json:"word"`
	}
	if json.Unmarshal(raw, &objs) == nil {
		out := make([]string, 0, len(objs))
		for _, o := range objs {
			if o.Word != "" {
				out = append(out, o.Word)
			}
		}
		return out
	}
	return nil
}

// ImportLists reads a JSON file (old or new format) and replaces the current lists.
// Returns the count of blacklist and whitelist entries imported.
func (a *App) ImportLists(path string) (int, int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, 0, err
	}
	var raw struct {
		Blacklist json.RawMessage `json:"blacklist"`
		Whitelist json.RawMessage `json:"whitelist"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return 0, 0, fmt.Errorf("invalid JSON: %w", err)
	}
	bl := parseWordList(raw.Blacklist)
	wl := parseWordList(raw.Whitelist)
	a.mu.Lock()
	a.Blacklist = bl
	a.Whitelist = wl
	a.mu.Unlock()
	go a.saveLists()
	return len(bl), len(wl), nil
}

// ExportLists writes the current lists to a JSON file in the new format.
func (a *App) ExportLists(path string) error {
	a.mu.RLock()
	data, err := json.MarshalIndent(listsFile{Blacklist: a.Blacklist, Whitelist: a.Whitelist}, "", "  ")
	a.mu.RUnlock()
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}

type settingsFile struct {
	Nickname string `json:"nickname,omitempty"`
}

func (a *App) saveSettings() {
	a.mu.RLock()
	data, _ := json.Marshal(settingsFile{Nickname: a.Nickname})
	a.mu.RUnlock()
	os.WriteFile(filepath.Join(a.DataDir, "settings.json"), data, 0644)
}

func (a *App) loadSettings() {
	data, err := os.ReadFile(filepath.Join(a.DataDir, "settings.json"))
	if err != nil {
		return
	}
	var sf settingsFile
	if json.Unmarshal(data, &sf) == nil && sf.Nickname != "" {
		a.mu.Lock()
		a.Nickname = sf.Nickname
		a.mu.Unlock()
	}
}

// SetNickname changes the display name used in chat messages.
func (a *App) SetNickname(name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("το όνομα δεν μπορεί να είναι κενό")
	}
	if len([]rune(name)) > 32 {
		return fmt.Errorf("μέγιστο 32 χαρακτήρες")
	}
	a.mu.Lock()
	a.Nickname = name
	a.mu.Unlock()
	go a.saveSettings()
	return nil
}

func (a *App) saveMessages() {
	a.mu.RLock()
	data, err := json.Marshal(a.Messages)
	a.mu.RUnlock()
	if err != nil {
		return
	}
	os.WriteFile(filepath.Join(a.DataDir, "messages.json"), data, 0644)
}

func (a *App) loadMessages() {
	data, err := os.ReadFile(filepath.Join(a.DataDir, "messages.json"))
	if err != nil {
		return
	}
	var msgs []ChatMessage
	if json.Unmarshal(data, &msgs) == nil {
		a.mu.Lock()
		a.Messages = msgs
		a.mu.Unlock()
	}
}

// ── File transfer ─────────────────────────────────────────────────────────────

// SendFile reads path, attaches it to a new chat message, and streams chunks to
// all students (teacher) or to the teacher (student).  caption is the message
// text; pass "" to default to the filename.
func (a *App) SendFile(path, caption, targetStudentID string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read file: %w", err)
	}
	name := filepath.Base(path)
	size := int64(len(data))
	if caption == "" {
		caption = name
	}

	msgID := newMsgID()
	fileID := "f" + msgID
	ts := time.Now()

	if err := a.storeFile(fileID, name, data); err != nil {
		return fmt.Errorf("store file: %w", err)
	}

	cm := ChatMessage{
		ID: msgID, From: a.Nickname, Content: caption,
		Timestamp: ts, FileID: fileID, FileName: name, FileSize: size,
	}
	chatMsg, err := protocol.Encode(protocol.TypeChat, protocol.ChatPayload{
		ID: msgID, From: a.Nickname, Content: caption,
		Timestamp: ts.Unix(), FileID: fileID, FileName: name, FileSize: size,
	})
	if err != nil {
		return err
	}

	a.mu.Lock()
	a.Messages = append(a.Messages, cm)
	a.mu.Unlock()
	if a.OnChatMessage != nil {
		a.OnChatMessage(cm)
	}
	go a.saveMessages()

	go func() {
		if a.Role == RoleTeacher {
			if targetStudentID == "" {
				a.Server.Broadcast(chatMsg)
			} else {
				a.Server.Send(targetStudentID, chatMsg)
			}
			a.sendChunksServer(fileID, name, size, data, targetStudentID, false)
		} else {
			a.Client.Send(chatMsg)
			a.sendChunksClient(fileID, name, size, data)
		}
	}()
	return nil
}

func (a *App) sendChunksServer(fileID, name string, size int64, data []byte, targetID string, autoOpen bool) {
	send := func(msg protocol.Message) {
		if targetID == "" {
			a.Server.Broadcast(msg)
		} else {
			a.Server.Send(targetID, msg)
		}
	}
	hdr, _ := protocol.Encode(protocol.TypeFileHdr, protocol.FileHdrPayload{
		FileID: fileID, Name: name, Size: size, AutoOpen: autoOpen,
	})
	send(hdr)
	for i := 0; i*fileChunkSize < len(data); i++ {
		lo, hi := i*fileChunkSize, (i+1)*fileChunkSize
		if hi > len(data) {
			hi = len(data)
		}
		chunk, _ := protocol.Encode(protocol.TypeFileChunk, protocol.FileChunkPayload{
			FileID: fileID, Index: i, Data: data[lo:hi],
		})
		send(chunk)
	}
	end, _ := protocol.Encode(protocol.TypeFileEnd, protocol.FileEndPayload{FileID: fileID})
	send(end)
}

func (a *App) sendChunksClient(fileID, name string, size int64, data []byte) {
	hdr, _ := protocol.Encode(protocol.TypeFileHdr, protocol.FileHdrPayload{
		FileID: fileID, Name: name, Size: size,
	})
	a.Client.Send(hdr)
	for i := 0; i*fileChunkSize < len(data); i++ {
		lo, hi := i*fileChunkSize, (i+1)*fileChunkSize
		if hi > len(data) {
			hi = len(data)
		}
		chunk, _ := protocol.Encode(protocol.TypeFileChunk, protocol.FileChunkPayload{
			FileID: fileID, Index: i, Data: data[lo:hi],
		})
		a.Client.Send(chunk)
	}
	end, _ := protocol.Encode(protocol.TypeFileEnd, protocol.FileEndPayload{FileID: fileID})
	a.Client.Send(end)
}

func (a *App) assembleAndStore(fileID string, pf *pendingFile) {
	var assembled []byte
	for i := 0; ; i++ {
		chunk, ok := pf.chunks[i]
		if !ok {
			break
		}
		assembled = append(assembled, chunk...)
	}
	if err := a.storeFile(fileID, pf.name, assembled); err != nil {
		return
	}
	if pf.autoOpen {
		// Silently open with default app — no UI notification
		path := a.GetFilePath(fileID, pf.name)
		exec.Command("cmd", "/c", "start", "", path).Start()
	}
	if a.OnFileReceived != nil {
		a.OnFileReceived(fileID, pf.name)
	}
}

// mustReadFile reads a file from disk and returns its bytes, or nil on error.
func mustReadFile(path string) []byte {
	data, _ := os.ReadFile(path)
	return data
}

// storeFile saves data to DataDir/files/{fileID}/{name}
func (a *App) storeFile(fileID, name string, data []byte) error {
	dir := filepath.Join(a.DataDir, "files", fileID)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, name), data, 0644)
}

// HasFile returns true if the file is stored locally
func (a *App) HasFile(fileID, name string) bool {
	_, err := os.Stat(filepath.Join(a.DataDir, "files", fileID, name))
	return err == nil
}

// GetFilePath returns the local path of a stored file
func (a *App) GetFilePath(fileID, name string) string {
	return filepath.Join(a.DataDir, "files", fileID, name)
}

// DownloadFile copies a stored file to the user's Downloads folder
func (a *App) DownloadFile(fileID, name string) (string, error) {
	src := a.GetFilePath(fileID, name)
	in, err := os.Open(src)
	if err != nil {
		return "", fmt.Errorf("file not available locally")
	}
	defer in.Close()

	dest := filepath.Join(downloadsDir(), name)
	out, err := os.Create(dest)
	if err != nil {
		return "", err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return "", err
	}
	return dest, nil
}

// DownloadAll zips every stored file and saves to Downloads/AllFiles-Date-Time.zip
func (a *App) DownloadAll() (string, error) {
	filesDir := filepath.Join(a.DataDir, "files")
	entries, err := os.ReadDir(filesDir)
	if err != nil || len(entries) == 0 {
		return "", fmt.Errorf("δεν υπάρχουν αρχεία")
	}

	ts := time.Now().Format("2006-01-02-15-04")
	zipPath := filepath.Join(downloadsDir(), "AllFiles-"+ts+".zip")
	f, err := os.Create(zipPath)
	if err != nil {
		return "", err
	}
	defer f.Close()

	w := zip.NewWriter(f)
	defer w.Close()

	seen := map[string]int{} // track duplicate names → add suffix
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		subDir := filepath.Join(filesDir, entry.Name())
		subs, _ := os.ReadDir(subDir)
		for _, sub := range subs {
			data, err := os.ReadFile(filepath.Join(subDir, sub.Name()))
			if err != nil {
				continue
			}
			fname := sub.Name()
			if n := seen[fname]; n > 0 {
				ext := filepath.Ext(fname)
				base := fname[:len(fname)-len(ext)]
				fname = fmt.Sprintf("%s(%d)%s", base, n, ext)
			}
			seen[sub.Name()]++
			fw, err := w.Create(fname)
			if err != nil {
				continue
			}
			fw.Write(data)
		}
	}
	return zipPath, nil
}

// PushOpenURL sends a silent push_open command to students to open a URL in their browser.
// targetStudentID = "" broadcasts to all students.
func (a *App) PushOpenURL(url, targetStudentID string) error {
	if !strings.Contains(url, "://") {
		url = "http://" + url
	}
	msg, err := protocol.Encode(protocol.TypeCommand, protocol.CommandPayload{
		CmdID:  newMsgID(),
		Target: targetStudentID,
		Action: protocol.CmdPushOpen,
		Param:  url,
	})
	if err != nil {
		return err
	}
	if targetStudentID == "" {
		a.Server.Broadcast(msg)
	} else {
		return a.Server.Send(targetStudentID, msg)
	}
	return nil
}

// PushOpenFile re-sends a previously uploaded file with AutoOpen=true so students open it on receipt.
// msgID is the chat message ID that owns the file.
func (a *App) PushOpenFile(msgID, targetStudentID string) error {
	a.mu.RLock()
	var found *ChatMessage
	for i := range a.Messages {
		if a.Messages[i].ID == msgID && a.Messages[i].FileID != "" {
			cp := a.Messages[i]
			found = &cp
			break
		}
	}
	a.mu.RUnlock()

	if found == nil {
		return fmt.Errorf("file message not found")
	}
	data, err := os.ReadFile(a.GetFilePath(found.FileID, found.FileName))
	if err != nil {
		return fmt.Errorf("file not available locally: %s", found.FileName)
	}
	go a.sendChunksServer(found.FileID, found.FileName, found.FileSize, data, targetStudentID, true)
	return nil
}

// MarkMonitoringEnded clears the monitoring class-state flag and broadcasts.
// Used by the teacher when the monitoring.exe session ends unexpectedly
// (window closed by user, pipe broke, etc.) — without this, State.Monitoring
// stays true and a subsequent --t tvon does nothing.
func (a *App) MarkMonitoringEnded() {
	a.mu.Lock()
	wasOn := a.State.Monitoring
	a.State.Monitoring = false
	a.mu.Unlock()
	if wasOn {
		a.broadcastState()
		if a.OnStateChange != nil {
			a.OnStateChange(a.State)
		}
	}
}

// RequestShot sends a one-off screenshot request to a single student.
// Lightweight: no state update or broadcast — just a targeted command.
// StartCasting starts the dedicated cast server (via OnStartCasting hook),
// then broadcasts CmdStartCast with the server address so students can connect.
func (a *App) StartCasting() (string, error) {
	if a.Role != RoleTeacher {
		return "", fmt.Errorf("only teacher can cast")
	}
	a.mu.Lock()
	if a.State.Casting {
		a.mu.Unlock()
		return "", nil
	}
	a.mu.Unlock()

	var serverAddr string
	if a.OnStartCasting != nil {
		addr, err := a.OnStartCasting()
		if err != nil {
			return "", err
		}
		serverAddr = addr
	}

	msg, _ := protocol.Encode(protocol.TypeCommand, protocol.CommandPayload{
		CmdID:  newMsgID(),
		Action: protocol.CmdStartCast,
		Param:  serverAddr,
	})
	a.Server.Broadcast(msg)

	a.mu.Lock()
	a.State.Casting = true
	a.mu.Unlock()
	a.broadcastState()

	if a.OnStateChange != nil {
		a.OnStateChange(a.State)
	}
	return serverAddr, nil
}

// StopCasting broadcasts CmdStopCast and stops the cast server.
func (a *App) StopCasting() {
	if a.Role != RoleTeacher {
		return
	}
	a.mu.Lock()
	if !a.State.Casting {
		a.mu.Unlock()
		return
	}
	a.State.Casting = false
	a.mu.Unlock()

	msg, _ := protocol.Encode(protocol.TypeCommand, protocol.CommandPayload{
		CmdID:  newMsgID(),
		Action: protocol.CmdStopCast,
	})
	a.Server.Broadcast(msg)
	a.broadcastState()

	if a.OnStopCasting != nil {
		a.OnStopCasting()
	}
	if a.OnStateChange != nil {
		a.OnStateChange(a.State)
	}
}

func (a *App) RequestShot(studentID string) error {
	return a.RequestShotParam(studentID, "")
}

// RequestShotParam sends a screenshot request with an optional param. Pass
// "hi" for a higher-resolution capture (used by the teacher's focus mode).
func (a *App) RequestShotParam(studentID, param string) error {
	if a.Server == nil {
		return fmt.Errorf("not a teacher")
	}
	msg, err := protocol.Encode(protocol.TypeCommand, protocol.CommandPayload{
		CmdID:  newMsgID(),
		Target: studentID,
		Action: protocol.CmdRequestShot,
		Param:  param,
	})
	if err != nil {
		return err
	}
	return a.Server.Send(studentID, msg)
}

// PinMediaFile toggles the teacher-local media-library pin on a file message.
func (a *App) PinMediaFile(msgID string) (pinned bool, err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for i := range a.Messages {
		if a.Messages[i].ID == msgID {
			a.Messages[i].MediaPinned = !a.Messages[i].MediaPinned
			pinned = a.Messages[i].MediaPinned
			snapshot := make([]ChatMessage, len(a.Messages))
			copy(snapshot, a.Messages)
			if a.OnMessagesUpdated != nil {
				a.OnMessagesUpdated(snapshot)
			}
			go a.saveMessages()
			return pinned, nil
		}
	}
	return false, fmt.Errorf("message not found")
}

func downloadsDir() string {
	home, _ := os.UserHomeDir()
	dl := filepath.Join(home, "Downloads")
	if info, err := os.Stat(dl); err == nil && info.IsDir() {
		return dl
	}
	return home
}

func (a *App) Stop() {
	if a.scanCancel != nil {
		a.scanCancel()
	}
	if a.Server != nil {
		a.Server.Stop()
	}
	if a.Client != nil {
		a.Client.Disconnect()
	}
	if a.Probe != nil {
		a.Probe.Stop()
	}
}

func newMsgID() string {
	return fmt.Sprintf("%d", time.Now().UnixNano())
}
