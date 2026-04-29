package tui

import (
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/atotto/clipboard"
	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"classsend/internal/core"
	"classsend/internal/protocol"
)

type screen int

const (
	screenWaiting screen = iota
	screenChat
)

type studentEntry struct {
	id      string
	name    string
	ip      string
	online  bool
	handUp  bool
	blocked bool
}

// Model is the root Bubbletea model
type Model struct {
	app    *core.App
	events chan tea.Msg

	screen screen
	width  int
	height int

	spinner  spinner.Model
	viewport viewport.Model
	input    textarea.Model

	messages    []core.ChatMessage
	students    []studentEntry
	selectedSt  int
	focusInput  bool
	showSidebar bool

	toolsOpen   bool
	toolsCursor int

	helpOpen   bool
	helpScroll int

	listOpen   bool
	listScroll int

	filePickerOpen    bool
	filePickerDir     string
	filePickerEntries []os.DirEntry
	filePickerCursor  int
	stagedFile        string // full path staged for send, "" if none

	// Command history (bash-style Up/Down)
	history       []string
	historyIdx    int    // -1 = not browsing; 0 = oldest entry
	historySaved  string // input saved before entering history mode

	// Tab completion state
	tabMatches []string
	tabIdx     int

	state     core.ClassState
	connected bool

	// Easter eggs
	matrixActive bool
	matrixFrame  int
	matrixHeads  []int // column head positions for rain effect

	// Rolling message window: sender name → last 10 message IDs
	msgWindow  map[string][]string
	senderNums []string // ordered list of seen sender names (1-based index = their number)
}

var tools = []struct {
	key    string
	label  string
	action string
}{
	{"1", "Κλείδωμα οθονών", protocol.CmdLockScreen},
	{"2", "Ξεκλείδωμα", protocol.CmdUnlockScreen},
	{"3", "Αποκλ. μηνυμάτων", protocol.CmdBlockChat},
	{"4", "Αποδ. μηνυμάτων", protocol.CmdUnblockChat},
	{"5", "Κλείσιμο εφαρμογών", protocol.CmdCloseApps},
	{"6", "Σίγαση", protocol.CmdMute},
	{"7", "Κατάργηση σίγασης", protocol.CmdUnmute},
	{"8", "Τερματισμός", protocol.CmdShutdown},
	{"9", "Παρακολούθηση", protocol.CmdStartMonitor},
	{"0", "Διακοπή παρακολ.", protocol.CmdStopMonitor},
}

func New(app *core.App) *Model {
	sp := spinner.New()
	sp.Spinner = spinner.Dot
	sp.Style = lipgloss.NewStyle().Foreground(colAccent)

	ta := textarea.New()
	ta.Placeholder = "Γράψτε μήνυμα... (--help για βοήθεια)"
	ta.SetHeight(2)
	ta.ShowLineNumbers = false
	ta.CharLimit = 500

	m := &Model{
		app:         app,
		events:      make(chan tea.Msg, 64),
		screen:      screenWaiting,
		spinner:     sp,
		input:       ta,
		focusInput:  true,
		showSidebar: true,
		msgWindow:   make(map[string][]string),
		historyIdx:  -1,
	}
	// Set initial text colour (plain text = normal).
	// Both Text (non-cursor lines) and CursorLine (the active typing line) must be
	// set together; the textarea renders the cursor line with CursorLine, not Text.
	initialStyle := lipgloss.NewStyle().Foreground(colText)
	m.input.FocusedStyle.Text = initialStyle
	m.input.FocusedStyle.CursorLine = initialStyle

	// Wire app events → channel → TUI
	app.OnChatMessage = func(msg core.ChatMessage) { m.events <- evChatMsg{msg: msg} }
	app.OnConnected = func() { m.events <- evConnected{} }
	app.OnDisconnected = func() { m.events <- evDisconnected{} }
	app.OnStateChange = func(s core.ClassState) { m.events <- evStateChange{state: s} }
	app.OnMessagesUpdated = func(msgs []core.ChatMessage) { m.events <- evMessagesUpdated{msgs: msgs} }
	app.OnStudentMissing = func(mac, nickname, hostname string, count int) {
		m.events <- evStudentMissing{mac: mac, nickname: nickname, hostname: hostname, count: count}
	}
	app.OnFileReceived = func(fileID, name string) {
		m.events <- evFileReceived{fileID: fileID, name: name}
	}
	app.OnSysMsg = func(text string) {
		m.events <- evSysMsg{text: text}
	}
	app.OnCommandFailed = func(nickname, action, errMsg string) {
		m.events <- evCmdFailed{nickname: nickname, action: action, errMsg: errMsg}
	}

	// Teacher is always "connected" (they own the class)
	if app.Role == core.RoleTeacher {
		m.connected = true
		m.screen = screenChat
		// Restore persisted history
		for _, msg := range app.GetMessages() {
			m.messages = append(m.messages, msg)
			if msg.From != "system" {
				m.trackMessage(msg.From, msg.ID)
			}
		}
	}

	return m
}

func (m *Model) PushStudentJoin(id, name, ip string) {
	m.events <- evStudentJoin{id: id, name: name, ip: ip}
}

func (m *Model) PushStudentLeave(id string) {
	m.events <- evStudentLeave{id: id}
}

func (m *Model) Init() tea.Cmd {
	return tea.Batch(
		m.spinner.Tick,
		m.waitForEvent(),
		m.input.Focus(),
	)
}

func (m *Model) waitForEvent() tea.Cmd {
	return func() tea.Msg { return <-m.events }
}

// ── Update ────────────────────────────────────────────────────────────────────

func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd
	toolsWereOpen     := m.toolsOpen      // capture before key handling closes the panel
	filePickerWasOpen := m.filePickerOpen // same guard for file picker
	helpWasOpen       := m.helpOpen       // same guard for help overlay
	listWasOpen       := m.listOpen       // same guard for list overlay

	switch msg := msg.(type) {

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.resizeComponents()

	case tea.KeyMsg:
		if cmd := m.handleKey(msg); cmd != nil {
			cmds = append(cmds, cmd)
		}

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		cmds = append(cmds, cmd)

	case evConnected:
		m.connected = true
		m.screen = screenChat
		m.resizeComponents()
		cmds = append(cmds, m.input.Focus())

	case evDisconnected:
		m.connected = false
		m.screen = screenWaiting
		cmds = append(cmds, m.spinner.Tick)

	case evChatMsg:
		m.messages = append(m.messages, msg.msg)
		if msg.msg.From != "system" {
			m.trackMessage(msg.msg.From, msg.msg.ID)
		}
		m.refreshViewport()

	case evMessagesUpdated:
		m.syncMessages(msg.msgs)
		m.resizeComponents() // pinned section height may have changed

	case evStudentJoin:
		m.upsertStudent(msg.id, msg.name, msg.ip, true)

	case evStudentLeave:
		m.setStudentOnline(msg.id, false)

	case evStateChange:
		m.state = msg.state

	case evStudentMissing:
		name := msg.nickname
		if name == "" {
			name = msg.hostname
		}
		if name == "" {
			name = msg.mac
		}
		switch {
		case msg.count == 1:
			m.pushSysMsg(fmt.Sprintf("⚠ %s δεν βρέθηκε — αναζήτηση...", name))
		case msg.count == 5:
			m.pushSysMsg(fmt.Sprintf("❌ %s — PC εκτός λειτουργίας", name))
			// silence after this — teacher already knows
		}

	case evFileReceived:
		m.pushSysMsg(fmt.Sprintf("✅ %s — λήφθηκε", msg.name))

	case evSysMsg:
		m.pushSysMsg(msg.text)

	case evCmdFailed:
		m.pushSysMsg(fmt.Sprintf("❌ %s: %s — αποτυχία", msg.nickname, msg.action))

	case evMatrixTick:
		if m.matrixActive {
			m.matrixFrame++
			rows := m.height
			for i := range m.matrixHeads {
				m.matrixHeads[i]++
				if m.matrixHeads[i] > rows+6 {
					m.matrixHeads[i] = -rand.Intn(rows/2 + 1)
				}
			}
			if m.matrixFrame >= 72 {
				m.matrixActive = false
				m.matrixHeads = nil
				m.refreshViewport()
			} else {
				cmds = append(cmds, matrixTick())
			}
		}
	}

	cmds = append(cmds, m.waitForEvent())

	// Forward remaining key events to input only when focused and no overlay is open.
	// Enter is never forwarded — it's the send key, not a newline, and would leave a
	// blank line in the textarea after Reset() clears the value.
	noOverlay := !m.toolsOpen && !toolsWereOpen && !m.filePickerOpen && !filePickerWasOpen && !m.helpOpen && !helpWasOpen && !m.listOpen && !listWasOpen
	isEnter := func() bool {
		k, ok := msg.(tea.KeyMsg)
		return ok && k.String() == "enter"
	}()
	if m.focusInput && m.screen == screenChat && noOverlay && !isEnter {
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(msg)
		if cmd != nil {
			cmds = append(cmds, cmd)
		}
	}

	if m.screen == screenChat && noOverlay {
		var cmd tea.Cmd
		m.viewport, cmd = m.viewport.Update(msg)
		if cmd != nil {
			cmds = append(cmds, cmd)
		}
	}

	m.updateInputStyle()

	return m, tea.Batch(cmds...)
}

func (m *Model) handleKey(msg tea.KeyMsg) tea.Cmd {
	k := msg.String()

	// Any non-Tab key resets tab-completion cycle
	if k != "tab" {
		m.tabMatches = nil
		m.tabIdx = 0
	}

	switch k {

	case "ctrl+c":
		m.app.Stop()
		return tea.Quit

	// ── Navigation shortcuts ──────────────────────────────────────────────────

	case "ctrl+u":
		if m.screen == screenChat && m.app.Role == core.RoleTeacher {
			m.showSidebar = !m.showSidebar
			m.resizeComponents()
		}

	case "ctrl+t":
		if m.screen == screenChat && m.app.Role == core.RoleTeacher {
			m.toolsOpen = !m.toolsOpen
			m.toolsCursor = 0
		}

	case "ctrl+a":
		if m.screen == screenChat {
			if m.filePickerOpen {
				m.filePickerOpen = false
			} else {
				m.openFilePicker()
			}
		}

	case "ctrl+w":
		if m.screen == screenChat {
			m.focusInput = true
			return m.input.Focus()
		}

	case "ctrl+h":
		// ^H opens/closes help (Ctrl+H = Backspace on some terminals; on Windows it works)
		if m.screen == screenChat {
			m.helpOpen = !m.helpOpen
			m.helpScroll = 0
		}

	case "ctrl+l":
		if m.screen == screenChat && m.app.Role == core.RoleTeacher {
			m.listOpen = !m.listOpen
			m.listScroll = 0
		}

	case "ctrl+s":
		if m.screen == screenChat && m.app.Role == core.RoleTeacher {
			return m.toggleCasting()
		}

	case "enter":
		if m.listOpen {
			m.listOpen = false
			return nil
		}
		if m.helpOpen {
			m.helpOpen = false
			return nil
		}
		if m.filePickerOpen {
			m.filePickerEnter()
			return nil
		}
		if m.toolsOpen {
			m.executeSelectedTool()
			return nil
		}
		if m.screen == screenChat && m.focusInput {
			return m.trySend()
		}

	case "tab":
		if m.screen == screenChat && m.focusInput {
			m.doTabComplete()
		}

	case "ctrl+r":
		if m.app.Role == core.RoleStudent && m.screen == screenChat {
			return m.shortcutAction("rep")
		}

	case "ctrl+d":
		if m.screen == screenChat {
			return m.shortcutAction("dl")
		}

	case "ctrl+p":
		if m.app.Role == core.RoleTeacher && m.screen == screenChat {
			return m.doPinLast()
		}

	case "ctrl+b":
		if m.app.Role == core.RoleTeacher && m.screen == screenChat {
			return m.shortcutAction("black")
		}

	case "ctrl+o":
		if m.screen == screenChat {
			if m.app.Role == core.RoleTeacher {
				return m.shortcutAction("pass")
			}
			return m.shortcutAction("op")
		}

	case "esc", "ctrl+x":
		if m.listOpen {
			m.listOpen = false
		} else if m.helpOpen {
			m.helpOpen = false
		} else if m.filePickerOpen {
			m.filePickerOpen = false
		} else if m.stagedFile != "" {
			m.stagedFile = ""
		} else {
			m.toolsOpen = false
		}

	case "backspace":
		if m.filePickerOpen {
			m.filePickerBack()
			return nil
		}

	case "up":
		if m.listOpen {
			if m.listScroll > 0 {
				m.listScroll--
			}
		} else if m.helpOpen {
			if m.helpScroll > 0 {
				m.helpScroll--
			}
		} else if m.filePickerOpen {
			if m.filePickerCursor > 0 {
				m.filePickerCursor--
			}
		} else if m.toolsOpen {
			if m.toolsCursor > 0 {
				m.toolsCursor--
			}
		} else if m.focusInput && m.screen == screenChat {
			m.historyUp()
			return nil
		} else if !m.focusInput {
			if m.selectedSt > 0 {
				m.selectedSt--
			}
		}

	case "down":
		if m.listOpen {
			total := len(m.app.Blacklist) + len(m.app.Whitelist) + 4 // headings + entries
			const maxVis = 20
			if m.listScroll < total-maxVis {
				m.listScroll++
			}
		} else if m.helpOpen {
			lines := m.helpLines()
			const maxVis = 22
			if m.helpScroll < len(lines)-maxVis {
				m.helpScroll++
			}
		} else if m.filePickerOpen {
			if m.filePickerCursor < len(m.filePickerEntries) {
				m.filePickerCursor++
			}
		} else if m.toolsOpen {
			if m.toolsCursor < len(tools)-1 {
				m.toolsCursor++
			}
		} else if m.focusInput && m.screen == screenChat {
			m.historyDown()
			return nil
		} else if !m.focusInput {
			if m.selectedSt < len(m.students)-1 {
				m.selectedSt++
			}
		}

	case "k":
		if !m.focusInput && !m.helpOpen && !m.filePickerOpen && !m.toolsOpen {
			if m.selectedSt > 0 {
				m.selectedSt--
			}
		}

	case "j":
		if !m.focusInput && !m.helpOpen && !m.filePickerOpen && !m.toolsOpen {
			if m.selectedSt < len(m.students)-1 {
				m.selectedSt++
			}
		}
	}

	// Number shortcuts inside tools panel
	if m.toolsOpen {
		for i, t := range tools {
			if k == t.key {
				m.toolsCursor = i
				m.executeSelectedTool()
				return nil
			}
		}
	}

	return nil
}

// ── Send & command system ─────────────────────────────────────────────────────

func (m *Model) trySend() tea.Cmd {
	text := strings.TrimSpace(m.input.Value())
	if text == "" {
		return nil
	}
	// Add to history (skip duplicates of the last entry)
	if len(m.history) == 0 || m.history[len(m.history)-1] != text {
		m.history = append(m.history, text)
		if len(m.history) > 100 {
			m.history = m.history[1:]
		}
	}
	m.historyIdx = -1
	m.input.Reset()

	// Built-in commands
	if text == "--help" || text == "--h" {
		m.helpOpen = true
		m.helpScroll = 0
		return nil
	}

	// Strip optional --send suffix
	if strings.HasSuffix(text, " --send") {
		text = strings.TrimSuffix(text, " --send")
	}

	// "text --pin" — send and pin
	pinAfterSend := false
	if m.app.Role == core.RoleTeacher && strings.HasSuffix(text, " --pin") {
		text = strings.TrimSuffix(text, " --pin")
		pinAfterSend = true
	}

	// Teacher: --del / --rem / --delete @X.Y  delete a message
	if m.app.Role == core.RoleTeacher {
		delCmd, delHasArg, delArg := parseAliasCmd(text, "--del", "--rem", "--delete")
		if delCmd {
			if !delHasArg {
				m.pushSysMsg("Χρήση: --del @X.Y")
			} else {
				msgID, err := m.parseMsgPos(delArg)
				if err != nil {
					m.pushSysMsg("⚠ " + err.Error())
				} else if err := m.app.DeleteMessage(msgID); err != nil {
					m.pushSysMsg("⚠ " + err.Error())
				} else {
					m.pushSysMsg(fmt.Sprintf("🗑 %s διαγράφηκε", delArg))
				}
			}
			return nil
		}
	}

	// Teacher: --pass / --ps @X.Y  dismiss a report
	if m.app.Role == core.RoleTeacher {
		passCmd, passHasArg, passArg := parseAliasCmd(text, "--pass", "--ps")
		if passCmd {
			if !passHasArg {
				m.pushSysMsg("Χρήση: --pass @X.Y")
			} else {
				arg := strings.TrimPrefix(passArg, "@")
				msgID, err := m.parseMsgPos(arg)
				if err != nil {
					m.pushSysMsg("⚠ " + err.Error())
				} else if err := m.app.ClearReport(msgID); err != nil {
					m.pushSysMsg("⚠ " + err.Error())
				} else {
					m.pushSysMsg(fmt.Sprintf("✅ @%s πέρασε", arg))
				}
			}
			return nil
		}
	}

	// Teacher: --black / --blk
	//   (no arg)   → open ^L list overlay
	//   @          → add last message to blacklist
	//   @BN        → remove entry N from blacklist
	//   @X.Y       → add message @X.Y words to blacklist
	//   <words>    → add words directly to blacklist
	if m.app.Role == core.RoleTeacher {
		blkCmd, blkHasArg, blkArg := parseAliasCmd(text, "--black", "--blk")
		if blkCmd {
			if !blkHasArg {
				m.listOpen = true
			} else if blkArg == "@" {
				return m.doBlacklistLast()
			} else if n, ok := parseListRef(blkArg, 'B'); ok {
				if err := m.app.RemoveBlacklistEntry(n - 1); err != nil {
					m.pushSysMsg("⚠ " + err.Error())
				} else {
					m.pushSysMsg(fmt.Sprintf("🗑 @B%d αφαιρέθηκε από τη μαύρη λίστα", n))
				}
			} else if strings.HasPrefix(blkArg, "@") {
				return m.doBlacklist(blkArg)
			} else {
				words := strings.Fields(blkArg)
				m.app.AddToBlacklist(words)
				m.pushSysMsg(fmt.Sprintf("🚫 +%d λέξεις στη μαύρη λίστα: %s", len(words), strings.Join(words, ", ")))
			}
			return nil
		}
	}

	// Teacher: --white / --wh
	//   (no arg)   → open ^L list overlay
	//   @          → add last message words to whitelist
	//   @WN        → remove entry N from whitelist
	//   @X.Y       → add message @X.Y words to whitelist
	//   <words>    → add words directly to whitelist
	if m.app.Role == core.RoleTeacher {
		whCmd, whHasArg, whArg := parseAliasCmd(text, "--white", "--wh")
		if whCmd {
			if !whHasArg {
				m.listOpen = true
			} else if whArg == "@" {
				return m.doWhitelistLast()
			} else if n, ok := parseListRef(whArg, 'W'); ok {
				if err := m.app.RemoveWhitelistEntry(n - 1); err != nil {
					m.pushSysMsg("⚠ " + err.Error())
				} else {
					m.pushSysMsg(fmt.Sprintf("🗑 @W%d αφαιρέθηκε από τη λευκή λίστα", n))
				}
			} else if strings.HasPrefix(whArg, "@") {
				return m.doWhitelistMsg(whArg)
			} else {
				words := strings.Fields(whArg)
				m.app.AddToWhitelist(words)
				m.pushSysMsg(fmt.Sprintf("✅ +%d λέξεις στη λευκή λίστα: %s", len(words), strings.Join(words, ", ")))
			}
			return nil
		}
	}

	// Student: --rep / --report @X.Y  report a message
	if m.app.Role == core.RoleStudent {
		repCmd, repHasArg, repArg := parseAliasCmd(text, "--rep", "--report")
		if repCmd {
			if repHasArg {
				return m.doReport(repArg)
			}
			return nil
		}
	}

	// --set <setting> <value>  — pass original case so file paths are preserved
	if strings.HasPrefix(strings.ToLower(text), "--set ") {
		m.handleSet(text[len("--set "):])
		return nil
	}

	// Easter eggs
	if text == "--coffee" {
		m.pushSysMsg("☕ Κάνε ένα διάλειμμα... αξίζεις έναν καφέ!")
		return nil
	}
	if text == "--matrix" {
		return m.startMatrix()
	}

	// --clr @s / --clr @S / --clear @s / --clear @system — clear only local system messages
	if norm := strings.ToLower(text); norm == "--clr @s" || norm == "--clear @s" ||
		norm == "--clr @system" || norm == "--clear @system" {
		var kept []core.ChatMessage
		for _, msg := range m.messages {
			if msg.From != "system" {
				kept = append(kept, msg)
			}
		}
		m.messages = kept
		m.refreshViewport()
		return nil
	}

	// Teacher: --clr / --clear  wipe all chat on all PCs
	if m.app.Role == core.RoleTeacher && (text == "--clr" || text == "--clear") {
		m.app.SendCommand(protocol.CmdClearChat, "", "")
		return nil
	}

	// Teacher tool commands: --t / --tool <action> [>N] [param]
	if m.app.Role == core.RoleTeacher {
		if tCmd, tHasArg, tArg := parseAliasCmd(text, "--t", "--tool"); tCmd {
			if tHasArg {
				m.handleToolCmd(tArg)
			} else {
				m.pushSysMsg("Χρήση: --t <lock|unlock|mute|…> [>N]  —  Tab για λίστα")
			}
			return nil
		}
	}

	// Teacher push-open: "--op <target> >N" or "<url> --op [>N]"
	// A ">" in the args marks push-to-students; without ">" it falls through to local open.
	if m.app.Role == core.RoleTeacher {
		if target, destStr, ok := parsePushOpenCmd(text); ok {
			return m.doPushOpen(target, destStr)
		}
	}

	// Copy message: --cp / --copy @X.Y or @pN
	if cpCmd, cpHasArg, cpArg := parseAliasCmd(text, "--cp", "--copy"); cpCmd && cpHasArg {
		return m.doCopy(cpArg)
	}

	// Open message content: --op / --open @X.Y or @pN
	if opCmd, opHasArg, opArg := parseAliasCmd(text, "--op", "--open"); opCmd && opHasArg {
		return m.doOpen(opArg)
	}

	// Pin message (teacher): --pin [@X.Y or @pN or @fN]
	if m.app.Role == core.RoleTeacher && (text == "--pin" || strings.HasPrefix(text, "--pin ")) {
		arg := strings.TrimPrefix(strings.TrimSpace(strings.TrimPrefix(text, "--pin")), "@")
		if arg == "" {
			return m.doPinLast()
		}
		return m.doPin(arg)
	}

	// Unpin message (teacher): --upin / --unpin [@X.Y or @pN]
	if m.app.Role == core.RoleTeacher {
		upinCmd, _, upinArg := parseAliasCmd(text, "--upin", "--unpin")
		if upinCmd {
			arg := strings.TrimPrefix(upinArg, "@")
			if arg == "" {
				return m.doUnpinLast()
			}
			return m.doUnpin(arg)
		}
	}

	// File attachment: "--a" / "--attach" alone opens the file picker overlay; with path stages directly
	if attachCmd, attachHasArg, attachArg := parseAliasCmd(text, "--a", "--attach"); attachCmd {
		if attachHasArg {
			m.stageFileByPath(attachArg)
		} else {
			m.openFilePicker()
		}
		return nil
	}

	// --dl / --download @X.Y — download a specific file (same as ^D shortcut)
	// --dl * / --download * — teacher: zip all files to Downloads
	if dlCmd, dlHasArg, dlArg := parseAliasCmd(text, "--dl", "--download"); dlCmd && dlHasArg {
		if dlArg == "*" {
			if m.app.Role == core.RoleTeacher {
				go func() {
					zipPath, err := m.app.DownloadAll()
					if err != nil {
						m.events <- evSysMsg{text: "⚠ " + err.Error()}
					} else {
						m.events <- evSysMsg{text: fmt.Sprintf("📦 Αποθηκεύτηκε: %s", zipPath)}
					}
				}()
				m.pushSysMsg("📦 Δημιουργία zip αρχείων...")
			}
			return nil
		}
		return m.doDownload(dlArg)
	}

	// If a file is staged, send it with the message text as caption
	if m.stagedFile != "" {
		staged := m.stagedFile
		m.stagedFile = ""
		caption := text // may be empty — SendFile defaults to filename
		if err := m.app.SendFile(staged, caption, ""); err != nil {
			m.pushSysMsg("⚠ " + err.Error())
		} else {
			m.pushSysMsg(fmt.Sprintf("📤 Αποστολή: %s", filepath.Base(staged)))
		}
		return nil
	}

	m.app.SendChat(text)
	if pinAfterSend {
		if err := m.app.PinLastMessage(); err != nil {
			m.pushSysMsg("⚠ " + err.Error())
		} else {
			m.pushSysMsg("📌 Μήνυμα καρφιτσώθηκε")
		}
	}
	return nil
}

// handleToolCmd parses: <action> [-a | -s <num>] [param]
func (m *Model) handleToolCmd(raw string) {
	parts := strings.Fields(raw)
	if len(parts) == 0 {
		m.pushSysMsg("Χρήση: --t <action> [-a | -s <num>] [param]  —  --help για λίστα")
		return
	}

	actionCode := parts[0]
	parts = parts[1:]

	// Casting uses a dedicated TCP server — handle before the generic action map.
	switch actionCode {
	case "start-casting", "cast", "caston":
		if m.state.Casting {
			m.pushSysMsg("⚠ Casting ήδη ενεργό — χρήση ^S ή --t stop-casting για διακοπή")
			return
		}
		if _, err := m.app.StartCasting(); err != nil {
			m.pushSysMsg("⚠ Casting: " + err.Error())
		} else {
			m.pushSysMsg("📡 Casting on")
		}
		return
	case "stop-casting", "castoff":
		m.app.StopCasting()
		m.pushSysMsg("📡 Casting off")
		return
	}

	// Resolve action code → protocol command
	type actionEntry struct {
		cmd   string
		label string
		param bool // expects a param (e.g. exe path)
	}
	actions := map[string]actionEntry{
		"lock":             {protocol.CmdLockScreen, "🔒 Κλείδωμα", false},
		"unlock":           {protocol.CmdUnlockScreen, "🔓 Ξεκλείδωμα", false},
		"mute":             {protocol.CmdMute, "🔇 Σίγαση", false},
		"unmute":           {protocol.CmdUnmute, "🔊 Κατάργηση σίγασης", false},
		"start-monitoring": {protocol.CmdStartMonitor, "👁 Παρακολούθηση on", false},
		"stop-monitoring":  {protocol.CmdStopMonitor, "👁 Παρακολούθηση off", false},
		"tvon":             {protocol.CmdStartMonitor, "👁 Παρακολούθηση on", false},
		"tvoff":            {protocol.CmdStopMonitor, "👁 Παρακολούθηση off", false},
		"shot":             {protocol.CmdRequestShot, "📷 Στιγμιότυπο", false},
		"close":            {protocol.CmdCloseApps, "❌ Κλείσιμο εφαρμογών", false},
		"shutdown":         {protocol.CmdShutdown, "⚡ Τερματισμός", false},
		"block":            {protocol.CmdBlockChat, "🚫 Αποκλεισμός chat", false},
		"unblock":          {protocol.CmdUnblockChat, "✅ Αποδέσμευση chat", false},
		"focus":            {protocol.CmdFocusApp, "🔍 Εστίαση", true},
		"launch":           {protocol.CmdLaunchApp, "🚀 Εκκίνηση", true},
	}

	entry, ok := actions[actionCode]
	if !ok {
		m.pushSysMsg(fmt.Sprintf("Άγνωστη εντολή: %s — --help για λίστα", actionCode))
		return
	}

	// Parse target: @N for specific student, nothing = all
	targetID := ""
	targetLabel := "όλους"
	param := ""

	i := 0
	for i < len(parts) {
		if strings.HasPrefix(parts[i], ">") {
			num := 0
			if _, err := fmt.Sscanf(strings.TrimPrefix(parts[i], ">"), "%d", &num); err != nil || num < 1 || num > len(m.students) {
				m.pushSysMsg(fmt.Sprintf("Άκυρος στόχος: %s (1–%d)", parts[i], len(m.students)))
				return
			}
			st := m.students[num-1]
			targetID = st.id
			targetLabel = fmt.Sprintf("%s (>%d)", st.name, num)
			i++
		} else {
			param = strings.Join(parts[i:], " ")
			break
		}
	}

	if entry.param && param == "" {
		m.pushSysMsg(fmt.Sprintf("Η εντολή %s απαιτεί παράμετρο  π.χ. --t launch >2 notepad.exe", actionCode))
		return
	}

	// Warn if targeting an offline student
	if targetID != "" {
		for _, st := range m.students {
			if st.id == targetID && !st.online {
				m.pushSysMsg(fmt.Sprintf("⚠ %s δεν είναι συνδεδεμένος", targetLabel))
				return
			}
		}
	}

	if err := m.app.SendCommand(entry.cmd, param, targetID); err != nil {
		m.pushSysMsg("⚠ " + err.Error())
		return
	}
	m.pushSysMsg(fmt.Sprintf("%s → %s", entry.label, targetLabel))
}

func (m *Model) executeSelectedTool() {
	if m.toolsCursor >= len(tools) {
		return
	}
	t := tools[m.toolsCursor]
	targetID := ""
	targetLabel := "όλους"

	if !m.focusInput && m.selectedSt < len(m.students) {
		st := m.students[m.selectedSt]
		if !st.online {
			m.pushSysMsg(fmt.Sprintf("⚠ %s δεν είναι συνδεδεμένος", st.name))
			m.toolsOpen = false
			return
		}
		targetID = st.id
		targetLabel = fmt.Sprintf("%s (@%d)", st.name, m.selectedSt+1)
	}

	if err := m.app.SendCommand(t.action, "", targetID); err != nil {
		m.pushSysMsg("⚠ " + err.Error())
	} else {
		m.pushSysMsg(fmt.Sprintf("%s → %s", t.label, targetLabel))
	}
	m.toolsOpen = false
}

func (m *Model) pushSysMsg(text string) {
	m.messages = append(m.messages, core.ChatMessage{
		ID:        fmt.Sprintf("sys-%d", len(m.messages)),
		From:      "system",
		Content:   text,
		Timestamp: time.Now(),
	})
	m.refreshViewport()
}

func (m *Model) helpLines() []string {
	return []string{
		"  ΓΕΝΙΚΕΣ ΣΥΝΤΟΜΕΥΣΕΙΣ",
		"  Enter        αποστολή μηνύματος",
		"  ^H           βοήθεια (αυτό το παράθυρο)",
		"  ^W           εστίαση πεδίου γραφής",
		"  ^A           browser αρχείων (Esc κλείσιμο)",
		"  ^D           λήψη αρχείου  (@X.Y ή @fN στο πεδίο)",
		"  ↑/↓          ιστορικό εντολών (bash-style)",
		"  Tab          αυτόματη συμπλήρωση εντολής",
		"  ^X / Esc     κλείσιμο overlay",
		"  ^C           έξοδος",
		"",
		"  ΣΥΝΤΟΜΕΥΣΕΙΣ ΔΑΣΚΑΛΟΥ",
		"  ^U    εμφάνιση/απόκρυψη λίστας μαθητών",
		"  ^T    μενού εργαλείων          (^X κλείσιμο)",
		"  ^L    μαύρη/λευκή λίστα        (^X κλείσιμο)",
		"  ^S    casting on/off  (εναλλαγή)",
		"  ^P    καρφίτσωμα τελευταίου δικού σου μηνύματος",
		"  ^B    μαύρη λίστα             (@X.Y στο πεδίο)",
		"  ^O    pass αναφοράς           (@X.Y στο πεδίο)",
		"",
		"  ΣΥΝΤΟΜΕΥΣΕΙΣ ΜΑΘΗΤΗ",
		"  ^R    αναφορά μηνύματος       (@X.Y στο πεδίο)",
		"  ^O    άνοιγμα αρχείου/URL     (@X.Y στο πεδίο)",
		"",
		"  ΑΝΑΦΟΡΑ ΜΗΝΥΜΑΤΩΝ",
		"  @X.Y  X=αποστολέας (0=εγώ, 1=πρώτος άλλος…)",
		"        Y=θέση στο παράθυρο 1-10",
		"  @pN   Nth καρφιτσωμένο μήνυμα (p1, p2…)",
		"  @fN   Nth καρφιτσωμένο ΑΡΧΕΙΟ (f1, f2…)",
		"",
		"  ΕΝΤΟΛΕΣ (ΟΛΟΙ)",
		"  --cp  / --copy    @X.Y    αντιγραφή στο clipboard",
		"  --op  / --open    @X.Y    άνοιγμα URL ή αρχείου        ^O (μαθητής)",
		"  --op  / --open    @fN     άνοιγμα καρφιτσωμένου αρχείου",
		"  --dl  / --download @X.Y   λήψη αρχείου                 ^D",
		"  --dl  / --download @fN    λήψη καρφιτσωμένου αρχείου",
		"  --a   / --attach          άνοιγμα file picker  (ίδιο με ^A)",
		"  --a   / --attach  <path>  επισύναψη αρχείου απευθείας",
		"  --clr @s                  καθαρισμός μηνυμάτων συστήματος",
		"",
		"  ΕΝΤΟΛΕΣ ΜΑΘΗΤΗ",
		"  --rep / --report @X.Y        αναφορά μηνύματος       ^R",
		"  --set nickname <όνομα>       αλλαγή ονόματος στο chat",
		"  --set autostart on|off       εκκίνηση με Windows",
		"  --set list import <αρχείο>   εισαγωγή λιστών (παλιά & νέα μορφή)",
		"  --set list export [αρχείο]   εξαγωγή λιστών (προεπ: Downloads/)",
		"",
		"  ΕΝΤΟΛΕΣ ΔΑΣΚΑΛΟΥ",
		"  --pin              καρφίτσωμα τελευταίου δικού σου  ^P",
		"  --pin @X.Y         καρφίτσωμα συγκεκριμένου",
		"  κείμενο --pin      αποστολή και καρφίτσωμα",
		"  --upin / --unpin              αποκαρφίτσωμα τελευταίου",
		"  --upin / --unpin  @X.Y        αποκαρφίτσωμα συγκεκριμένου",
		"  --pass / --ps     @X.Y        pass αναφοράς            ^O",
		"  --black / --blk   @X.Y        μαύρη λίστα + διαγραφή  ^B",
		"  --blk  <λέξεις>               προσθήκη στη μαύρη λίστα",
		"  --blk  @BN                    αφαίρεση @BN από μαύρη λίστα",
		"  --wh   <λέξεις>               προσθήκη στη λευκή λίστα",
		"  --wh   @WN                    αφαίρεση @WN από λευκή λίστα",
		"  --del / --rem / --delete @X.Y  διαγραφή μηνύματος",
		"  --clr / --clear               διαγραφή όλου του chat",
		"  --dl  / --download *          λήψη ΟΛΩΝ αρχείων σε zip (Downloads/)",
		"",
		"  PUSH-OPEN (δάσκαλος — silent, χωρίς μήνυμα στο chat)",
		"  --op / --open <url> >N    άνοιγμα URL στον μαθητή #N",
		"  --op / --open <url> >*    άνοιγμα URL σε όλους",
		"  --op / --open @X.Y >N     push αρχείου/URL → μαθητής #N",
		"  <url> --op                push URL σε όλους (shorthand)",
		"  <url> --op >N             push URL σε μαθητή #N",
		"",
		"  ΕΡΓΑΛΕΙΑ  --t / --tool <εντολή> [>N] [param]  (Tab: κύκλος εντολών)",
		"  lock / unlock          κλείδωμα/ξεκλείδωμα οθόνης",
		"  mute / unmute          σίγαση/ακύρωση σίγασης",
		"  tvon / start-monitoring    παρακολούθηση on",
		"  tvoff / stop-monitoring    παρακολούθηση off",
		"  shot  [>N]                 στιγμιότυπο (όλοι ή ένας)",
		"  start-casting / cast / caston   casting on  (^S εναλλαγή)",
		"  stop-casting / castoff          casting off (^S εναλλαγή)",
		"  block / unblock        αποκλεισμός/αποδέσμευση chat",
		"  close                  κλείσιμο εφαρμογών",
		"  shutdown               τερματισμός PC",
		"  focus <τίτλος>         εστίαση εφαρμογής",
		"  launch <exe>           εκκίνηση εφαρμογής",
		"  >N = αποστολή στον μαθητή #N · (χωρίς >N) = όλοι",
		"",
		"  TAB COMPLETION (στο πεδίο γραφής)",
		"  h/--h → --help      d/--d → --download   o/--o → --open",
		"  a/--a → --attach    p/--p → --pin         r/--r → --report",
		"  c/--c → --copy      u/--u → --unpin       t → --t",
		"  --t <Tab>  κύκλος εντολών: lock unlock mute unmute shot …",
		"",
		"  Παραδείγματα:",
		"  Διαβάστε σελ.4 --pin",
		"  --t lock >2               κλείδωμα μαθητή #2",
		"  --t launch >3 notepad.exe   εκκίνηση σε μαθητή #3",
		"  --op @f1      άνοιγμα 1ου καρφιτσωμένου αρχείου",
		"  --dl @f2      λήψη 2ου καρφιτσωμένου αρχείου",
		"  --cp @p1      αντιγραφή 1ου καρφιτσωμένου",
		"  --upin @p2    αποκαρφίτσωμα 2ου",
	}
}

func (m *Model) overlayHelp(base string) string {
	const overlayW = 60
	const maxVisible = 22

	lines := m.helpLines()
	total := len(lines)

	start := m.helpScroll
	if start > total-maxVisible {
		start = total - maxVisible
	}
	if start < 0 {
		start = 0
	}
	end := start + maxVisible
	if end > total {
		end = total
	}

	var out []string
	out = append(out, styleTitle.Render("  ClassSend 2.0 — Βοήθεια"))
	out = append(out, strings.Repeat("─", overlayW))
	for _, l := range lines[start:end] {
		out = append(out, lipgloss.NewStyle().Width(overlayW).Foreground(colText).Render(l))
	}
	out = append(out, strings.Repeat("─", overlayW))
	if total > maxVisible {
		pct := fmt.Sprintf("%d/%d", start+1, total)
		out = append(out, styleHint.Render(fmt.Sprintf("[↑↓] Κύλιση  %s  [Esc/Enter] Κλείσιμο", pct)))
	} else {
		out = append(out, styleHint.Render("[Esc/Enter] Κλείσιμο"))
	}

	panel := styleBorder.Padding(1, 2).Render(strings.Join(out, "\n"))
	return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center,
		panel,
		lipgloss.WithWhitespaceChars(" "),
		lipgloss.WithWhitespaceForeground(colBg),
	)
}

// ── Layout ────────────────────────────────────────────────────────────────────

func (m *Model) resizeComponents() {
	if m.screen != screenChat {
		return
	}
	sideW := 0
	if m.app.Role == core.RoleTeacher && m.showSidebar {
		sideW = 22
	}
	chatW := m.width - sideW
	bottomH := 4 // input + bottom bar
	headerH := 1
	pinnedH := m.pinnedSectionHeight()
	vpH := m.height - bottomH - headerH - pinnedH
	if vpH < 1 {
		vpH = 1
	}
	m.viewport.Width = chatW
	m.viewport.Height = vpH
	m.input.SetWidth(chatW - 2)
	m.refreshViewport()
}

func (m *Model) pinnedSectionHeight() int {
	n := len(m.pinnedMsgs())
	if n == 0 {
		return 0
	}
	return n + 2 // header line + one line per pin + separator line
}

func (m *Model) refreshViewport() {
	m.viewport.SetContent(m.renderMessages())
	m.viewport.GotoBottom()
}

// ── View ──────────────────────────────────────────────────────────────────────

func (m *Model) View() string {
	if m.screen == screenWaiting {
		return m.viewWaiting()
	}
	return m.viewChat()
}

func (m *Model) viewWaiting() string {
	body := lipgloss.JoinVertical(lipgloss.Center,
		styleTitle.Render("ClassSend 2.0"),
		"",
		styleMsgSystem.Render(m.spinner.View()+" Αναζήτηση δασκάλου..."),
		"",
		styleStatus.Render(fmt.Sprintf("PC: %s", m.app.Hostname)),
		"",
		styleHint.Render("Ctrl+C για έξοδο"),
	)
	box := styleBorder.Width(40).Padding(2, 4).Render(body)
	w, h := m.width, m.height
	if w == 0 {
		w = 80
	}
	if h == 0 {
		h = 24
	}
	return lipgloss.Place(w, h, lipgloss.Center, lipgloss.Center, box)
}

func (m *Model) renderPinnedSection() string {
	pinned := m.pinnedMsgs()
	if len(pinned) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString(styleTitle.Render("///── ΚΑΡΦΙΤΣΩΜΕΝΑ ──///") + "\n")
	for _, id := range pinned {
		for _, msg := range m.messages {
			if msg.ID == id {
				lbl := m.msgWindowLabel(msg.From, msg.ID) // fN for files, pN for others
				label := stylePinned.Render("[" + lbl + "]")
				content := msg.Content
				// truncate long content so it fits on one line
				if len(content) > m.width-12 && m.width > 12 {
					content = content[:m.width-12] + "…"
				}
				sb.WriteString(fmt.Sprintf(" %s %s\n", label, content))
				break
			}
		}
	}
	sb.WriteString(lipgloss.NewStyle().Foreground(colBorder).Render(strings.Repeat("─", m.width)))
	return sb.String()
}

func (m *Model) viewChat() string {
	if m.matrixActive {
		return m.renderMatrix()
	}
	header := m.renderHeader()
	pinnedSection := m.renderPinnedSection()
	bottomBar := m.renderBottomBar()
	inputArea := m.renderInput()

	var body string
	if m.app.Role == core.RoleTeacher && m.showSidebar {
		side := m.renderSidebar()
		chat := lipgloss.JoinVertical(lipgloss.Left, m.viewport.View(), inputArea)
		body = lipgloss.JoinHorizontal(lipgloss.Top, side, chat)
	} else {
		body = lipgloss.JoinVertical(lipgloss.Left, m.viewport.View(), inputArea)
	}

	sections := []string{header}
	if pinnedSection != "" {
		sections = append(sections, pinnedSection)
	}
	sections = append(sections, body, bottomBar)
	view := lipgloss.JoinVertical(lipgloss.Left, sections...)

	if m.toolsOpen {
		view = m.overlayTools(view)
	}
	if m.filePickerOpen {
		view = m.overlayFilePicker(view)
	}
	if m.helpOpen {
		view = m.overlayHelp(view)
	}
	if m.listOpen {
		view = m.overlayList(view)
	}
	return view
}

func (m *Model) renderHeader() string {
	var connLabel string
	var connStyle lipgloss.Style

	if m.app.Role == core.RoleTeacher {
		connLabel = fmt.Sprintf("● Δάσκαλος — %d/%d μαθητές", len(m.onlineStudents()), len(m.students))
		connStyle = styleConnected
	} else if m.connected {
		connLabel = "● Συνδεδεμένος"
		connStyle = styleConnected
	} else {
		connLabel = "● Αποσυνδεδεμένος"
		connStyle = styleDisconnected
	}

	left := styleTitle.Render("ClassSend")
	right := connStyle.Render(connLabel)
	gap := m.width - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 0 {
		gap = 0
	}
	return left + strings.Repeat(" ", gap) + right
}

func (m *Model) renderSidebar() string {
	w := 22
	vpH := m.viewport.Height

	var lines []string
	lines = append(lines, styleTitle.Width(w).Render(
		fmt.Sprintf("ΜΑΘΗΤΕΣ (%d)", len(m.onlineStudents())),
	))

	for i, st := range m.students {
		dot := styleStudentOnline.Render("●")
		if !st.online {
			dot = styleStudentOffline.Render("○")
		}
		name := st.name
		if len(name) > 11 {
			name = name[:10] + "…"
		}
		indicators := ""
		if st.handUp {
			indicators += "✋"
		}
		if st.blocked {
			indicators += "🔇"
		}
		num := lipgloss.NewStyle().Foreground(colAccentDim).Render(fmt.Sprintf("%2d", i+1))
		line := fmt.Sprintf("%s %s %-11s%s", num, dot, name, indicators)
		if i == m.selectedSt && !m.focusInput {
			line = styleSelected.Width(w).Render(line)
		} else {
			line = lipgloss.NewStyle().Width(w).Render(line)
		}
		lines = append(lines, line)
	}

	for len(lines) <= vpH {
		lines = append(lines, strings.Repeat(" ", w))
	}

	return stylePanel.Width(w).Height(vpH + 1).Render(strings.Join(lines, "\n"))
}

func (m *Model) renderInput() string {
	blocked := ""
	if m.state.ChatBlocked && m.app.Role == core.RoleStudent {
		blocked = "  " + styleBlocked.Render("Ο δάσκαλος απέκλεισε τις επικοινωνίες")
	}
	staged := ""
	if m.stagedFile != "" {
		staged = lipgloss.NewStyle().
			Foreground(colAccent).Bold(true).
			Render("📎 "+filepath.Base(m.stagedFile)) +
			styleHint.Render("  (Esc αφαίρεση)") + "\n"
	}
	return styleInputBox.Width(m.viewport.Width).Render(staged + m.input.View() + blocked)
}

func (m *Model) renderBottomBar() string {
	type shortcut struct{ key, label string }

	var shortcuts []shortcut
	if m.app.Role == core.RoleTeacher {
		castLabel := "Casting"
		if m.state.Casting {
			castLabel = "Casting●"
		}
		shortcuts = []shortcut{
			{"Enter", "Στείλε"},
			{"^H", "Βοήθεια"},
			{"^U", "Λίστα"},
			{"^T", "Εργαλεία"},
			{"^L", "Μαύρη/Λευκή"},
			{"^S", castLabel},
			{"^P", "Καρφίτσα"},
			{"^A", "Αρχείο"},
			{"^D", "@Λήψη"},
			{"^B", "@Μαύρη"},
			{"^C", "Έξοδος"},
		}
	} else {
		shortcuts = []shortcut{
			{"Enter", "Στείλε"},
			{"^H", "Βοήθεια"},
			{"^R", "@Αναφορά"},
			{"^D", "@Λήψη"},
			{"^O", "@Άνοιγμα"},
			{"^A", "Αρχείο"},
			{"^C", "Έξοδος"},
		}
	}

	var parts []string
	for _, s := range shortcuts {
		key := lipgloss.NewStyle().Foreground(colAccent).Bold(true).Render(s.key)
		lbl := lipgloss.NewStyle().Foreground(colTextDim).Render(s.label)
		parts = append(parts, key+" "+lbl)
	}

	bar := strings.Join(parts, "  ")
	return lipgloss.NewStyle().
		Width(m.width).
		Background(colPanel).
		Foreground(colTextDim).
		Padding(0, 1).
		Render(bar)
}

func (m *Model) renderMessages() string {
	if len(m.messages) == 0 {
		return styleMsgSystem.Render("  Δεν υπάρχουν μηνύματα ακόμα.")
	}
	var sb strings.Builder
	for _, msg := range m.messages {
		ts := styleTimestamp.Render("[" + msg.Timestamp.Format("15:04") + "]")
		var line string
		if msg.From == "system" {
			line = fmt.Sprintf("%s %s\n", ts, styleMsgSystem.Render(msg.Content))
		} else {
			isSelf := msg.From == m.app.Nickname
			var nameStyle lipgloss.Style
			if isSelf {
				nameStyle = styleMsgTeacher
			} else if msg.Reported {
				nameStyle = styleMsgReported
			} else {
				nameStyle = styleMsgStudent
			}

			prefix := ""
			if msg.Pinned {
				prefix = stylePinned.Render("📌 ") + prefix
			}
			if msg.Reported {
				prefix = styleReportTag.Render("[rep] ") + prefix
			}

			// Rolling window label [X.Y] — shown for all including self (0.Y)
			windowLabel := ""
			if lbl := m.msgWindowLabel(msg.From, msg.ID); lbl != "" {
				windowLabel = styleMsgNum.Render("["+lbl+"] ")
			}

			content := msg.Content

			if msg.FileID != "" {
				avail := "·"
				if m.app.HasFile(msg.FileID, msg.FileName) {
					avail = "✓"
				}
				caption := ""
				if content != "" && content != msg.FileName {
					caption = " \"" + content + "\""
				}
				fileTag := lipgloss.NewStyle().Foreground(colAccent).Render(
					fmt.Sprintf("📎 %s (%s)%s%s", msg.FileName, formatSize(msg.FileSize), avail, caption))
				content = fileTag
			}

			if msg.Reported {
				content = styleMsgReported.Render(content)
			}

			line = fmt.Sprintf("%s %s%s %s%s\n",
				ts, windowLabel, nameStyle.Render(msg.From+":"), prefix, content)
		}
		sb.WriteString(line)
	}
	return sb.String()
}

func (m *Model) overlayTools(base string) string {
	var lines []string
	lines = append(lines, styleTitle.Render("⚙ Εργαλεία Δασκάλου"))
	lines = append(lines, strings.Repeat("─", 34))

	for i, t := range tools {
		line := fmt.Sprintf(" [%s] %s", t.key, t.label)
		if i == m.toolsCursor {
			line = styleSelected.Width(34).Render(line)
		} else {
			line = lipgloss.NewStyle().Width(34).Foreground(colText).Render(line)
		}
		lines = append(lines, line)
	}

	lines = append(lines, strings.Repeat("─", 34))
	target := "Όλοι οι μαθητές"
	if !m.focusInput && m.selectedSt < len(m.students) {
		target = m.students[m.selectedSt].name
	}
	lines = append(lines, styleHint.Render("Στόχος: "+target))
	lines = append(lines, styleHint.Render("[Esc] Κλείσιμο  [↑↓/1-0] Πλοήγηση"))

	panel := styleBorder.Padding(1, 2).Render(strings.Join(lines, "\n"))
	return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center,
		panel,
		lipgloss.WithWhitespaceChars(" "),
		lipgloss.WithWhitespaceForeground(colBg),
	)
}

// ── File picker ───────────────────────────────────────────────────────────────

func (m *Model) openFilePicker() {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	if err := m.loadFilePickerDir(home); err != nil {
		m.pushSysMsg("⚠ Αδύνατο άνοιγμα: " + err.Error())
		return
	}
	m.filePickerOpen = true
}

func (m *Model) loadFilePickerDir(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	// dirs first, then files — both groups sorted alphabetically
	sort.Slice(entries, func(i, j int) bool {
		iDir := entries[i].IsDir()
		jDir := entries[j].IsDir()
		if iDir != jDir {
			return iDir
		}
		return entries[i].Name() < entries[j].Name()
	})
	m.filePickerDir = dir
	m.filePickerEntries = entries
	m.filePickerCursor = 0
	return nil
}

func (m *Model) filePickerEnter() {
	// cursor 0 = ".." (parent)
	if m.filePickerCursor == 0 {
		m.filePickerBack()
		return
	}
	entry := m.filePickerEntries[m.filePickerCursor-1]
	full := filepath.Join(m.filePickerDir, entry.Name())
	if entry.IsDir() {
		if err := m.loadFilePickerDir(full); err != nil {
			m.pushSysMsg("⚠ " + err.Error())
		}
		return
	}
	// It's a file — stage it
	m.stagedFile = full
	m.filePickerOpen = false
}

func (m *Model) filePickerBack() {
	parent := filepath.Dir(m.filePickerDir)
	if parent == m.filePickerDir {
		return // already at root
	}
	if err := m.loadFilePickerDir(parent); err != nil {
		m.pushSysMsg("⚠ " + err.Error())
	}
}

func (m *Model) stageFileByPath(raw string) {
	// expand ~ to home dir
	if strings.HasPrefix(raw, "~/") || raw == "~" {
		home, err := os.UserHomeDir()
		if err == nil {
			raw = filepath.Join(home, raw[2:])
		}
	}
	abs, err := filepath.Abs(raw)
	if err != nil {
		m.pushSysMsg("⚠ Άκυρη διαδρομή: " + raw)
		return
	}
	info, err := os.Stat(abs)
	if err != nil || info.IsDir() {
		m.pushSysMsg("⚠ Δεν βρέθηκε αρχείο: " + abs)
		return
	}
	m.stagedFile = abs
	m.pushSysMsg(fmt.Sprintf("📎 Σταδιακά: %s — Enter για αποστολή", filepath.Base(abs)))
}

func (m *Model) overlayFilePicker(base string) string {
	const w = 52
	const maxVisible = 16

	var lines []string
	lines = append(lines, styleTitle.Render("📎 Επισύναψη Αρχείου"))
	lines = append(lines, strings.Repeat("─", w))

	// Truncate dir path if too long
	dir := m.filePickerDir
	if len(dir) > w-2 {
		dir = "…" + dir[len(dir)-(w-3):]
	}
	lines = append(lines, styleHint.Render(dir))
	lines = append(lines, strings.Repeat("─", w))

	// Build entry list: row 0 = "..", row 1+ = entries
	total := len(m.filePickerEntries) + 1 // +1 for ".."
	cursor := m.filePickerCursor

	// Scroll window
	start := 0
	if total > maxVisible {
		half := maxVisible / 2
		start = cursor - half
		if start < 0 {
			start = 0
		}
		if start+maxVisible > total {
			start = total - maxVisible
		}
	}
	end := start + maxVisible
	if end > total {
		end = total
	}

	for i := start; i < end; i++ {
		var label string
		if i == 0 {
			label = "  .."
		} else {
			e := m.filePickerEntries[i-1]
			if e.IsDir() {
				label = fmt.Sprintf("  📁 %s/", e.Name())
			} else {
				label = fmt.Sprintf("  📄 %s", e.Name())
			}
		}
		if i == cursor {
			lines = append(lines, styleSelected.Width(w).Render(label))
		} else {
			lines = append(lines, lipgloss.NewStyle().Width(w).Foreground(colText).Render(label))
		}
	}

	if total == 0 {
		lines = append(lines, styleHint.Render("  (κενός φάκελος)"))
	}

	lines = append(lines, strings.Repeat("─", w))
	lines = append(lines, styleHint.Render("[↑↓] Πλοήγηση  [Enter] Άνοιγμα/Επιλογή"))
	lines = append(lines, styleHint.Render("[Bksp] Πίσω     [Esc] Άκυρο"))

	panel := styleBorder.Padding(1, 2).Render(strings.Join(lines, "\n"))
	return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center,
		panel,
		lipgloss.WithWhitespaceChars(" "),
		lipgloss.WithWhitespaceForeground(colBg),
	)
}

// ── Helpers ───────────────────────────────────────────────────────────────────

func (m *Model) onlineStudents() []studentEntry {
	var out []studentEntry
	for _, st := range m.students {
		if st.online {
			out = append(out, st)
		}
	}
	return out
}

func (m *Model) upsertStudent(id, name, ip string, online bool) {
	for i := range m.students {
		if m.students[i].id == id {
			m.students[i].online = online
			m.students[i].name = name
			m.students[i].ip = ip
			return
		}
	}
	m.students = append(m.students, studentEntry{id: id, name: name, ip: ip, online: online})
}

func (m *Model) setStudentOnline(id string, online bool) {
	for i := range m.students {
		if m.students[i].id == id {
			m.students[i].online = online
			return
		}
	}
}

// trackMessage adds a message ID to the rolling window for the sender (max 10)
func (m *Model) trackMessage(from, id string) {
	if from != m.app.Nickname {
		// register in ordered sender list (others only — self is always 0)
		found := false
		for _, name := range m.senderNums {
			if name == from {
				found = true
				break
			}
		}
		if !found {
			m.senderNums = append(m.senderNums, from)
		}
	}
	w := m.msgWindow[from]
	w = append(w, id)
	if len(w) > 10 {
		w = w[len(w)-10:]
	}
	m.msgWindow[from] = w
}

// senderNum returns the sender number: 0 = self, 1+ = others in order of first seen
func (m *Model) senderNum(from string) int {
	if from == m.app.Nickname {
		return 0
	}
	for i, name := range m.senderNums {
		if name == from {
			return i + 1
		}
	}
	return -1
}

// msgWindowLabel returns "fN" for pinned file messages, "pN" for other pinned messages,
// "X.Y" for windowed messages, else "".  fN takes priority over pN.
func (m *Model) msgWindowLabel(from, id string) string {
	// Pinned file messages get fN label (highest priority)
	for i, pid := range m.pinnedFileMsgs() {
		if pid == id {
			return fmt.Sprintf("f%d", i+1)
		}
	}
	// Other pinned messages get pN label
	for i, pid := range m.pinnedMsgs() {
		if pid == id {
			return fmt.Sprintf("p%d", i+1)
		}
	}
	x := m.senderNum(from)
	if x == -1 {
		return ""
	}
	for y, mid := range m.msgWindow[from] {
		if mid == id {
			return fmt.Sprintf("%d.%d", x, y+1)
		}
	}
	return ""
}

// pinnedMsgs returns message IDs of all currently pinned messages in chat order
func (m *Model) pinnedMsgs() []string {
	var ids []string
	for _, msg := range m.messages {
		if msg.Pinned {
			ids = append(ids, msg.ID)
		}
	}
	return ids
}

// pinnedFileMsgs returns message IDs of pinned messages that have a file attachment, in chat order
func (m *Model) pinnedFileMsgs() []string {
	var ids []string
	for _, msg := range m.messages {
		if msg.Pinned && msg.FileID != "" {
			ids = append(ids, msg.ID)
		}
	}
	return ids
}

// extractMsgPos finds the first @X.Y token in the current input, returns "X.Y" or ""
func (m *Model) extractMsgPos() string {
	for _, field := range strings.Fields(m.input.Value()) {
		if strings.HasPrefix(field, "@") {
			return strings.TrimPrefix(field, "@")
		}
	}
	return ""
}

// shortcutAction extracts @X.Y from input and runs "rep", "black", or "ok"
func (m *Model) toggleCasting() tea.Cmd {
	if m.state.Casting {
		m.app.StopCasting()
		m.pushSysMsg("📡 Casting off")
	} else {
		if _, err := m.app.StartCasting(); err != nil {
			m.pushSysMsg("⚠ Casting: " + err.Error())
		} else {
			m.pushSysMsg("📡 Casting on")
		}
	}
	return nil
}

func (m *Model) shortcutAction(action string) tea.Cmd {
	pos := m.extractMsgPos()
	if pos == "" {
		m.pushSysMsg("⚠ Γράψε @X.Y στο πεδίο — π.χ. @1.3")
		return nil
	}
	m.input.Reset()
	switch action {
	case "rep":
		return m.doReport(pos)
	case "dl":
		return m.doDownload(pos)
	case "op":
		return m.doOpen(pos)
	case "black":
		return m.doBlacklist(pos)
	case "pass":
		pos = strings.TrimPrefix(pos, "@")
		msgID, err := m.parseMsgPos(pos)
		if err != nil {
			m.pushSysMsg("⚠ " + err.Error())
		} else if err := m.app.ClearReport(msgID); err != nil {
			m.pushSysMsg("⚠ " + err.Error())
		} else {
			m.pushSysMsg(fmt.Sprintf("✅ @%s πέρασε", pos))
		}
	}
	return nil
}

func (m *Model) doReport(pos string) tea.Cmd {
	pos = strings.TrimPrefix(pos, "@")
	msgID, err := m.parseMsgPos(pos)
	if err != nil {
		m.pushSysMsg("⚠ " + err.Error())
	} else if err := m.app.SendReport(msgID); err != nil {
		m.pushSysMsg("⚠ " + err.Error())
	} else {
		m.pushSysMsg(fmt.Sprintf("📢 Αναφορά @%s στάλθηκε", pos))
	}
	return nil
}

func (m *Model) doDownload(pos string) tea.Cmd {
	pos = strings.TrimPrefix(pos, "@")
	msgID, err := m.parseMsgPos(pos)
	if err != nil {
		m.pushSysMsg("⚠ " + err.Error())
		return nil
	}
	for _, msg := range m.messages {
		if msg.ID == msgID {
			if msg.FileID == "" {
				m.pushSysMsg(fmt.Sprintf("⚠ @%s δεν είναι αρχείο", pos))
				return nil
			}
			if !m.app.HasFile(msg.FileID, msg.FileName) {
				m.pushSysMsg(fmt.Sprintf("⚠ %s — δεν βρέθηκε τοπικά", msg.FileName))
				return nil
			}
			go func(fileID, name string) {
				dest, err := m.app.DownloadFile(fileID, name)
				if err != nil {
					m.events <- evSysMsg{text: "⚠ " + err.Error()}
				} else {
					m.events <- evSysMsg{text: fmt.Sprintf("⬇ %s → %s", name, dest)}
				}
			}(msg.FileID, msg.FileName)
			return nil
		}
	}
	m.pushSysMsg(fmt.Sprintf("⚠ @%s δεν βρέθηκε", pos))
	return nil
}

func (m *Model) doPin(pos string) tea.Cmd {
	pos = strings.TrimPrefix(pos, "@")
	msgID, err := m.parseMsgPos(pos)
	if err != nil {
		m.pushSysMsg("⚠ " + err.Error())
		return nil
	}
	if err := m.app.PinMessage(msgID); err != nil {
		m.pushSysMsg("⚠ " + err.Error())
	} else {
		m.pushSysMsg(fmt.Sprintf("📌 @%s καρφιτσώθηκε", pos))
	}
	return nil
}

func (m *Model) doPinLast() tea.Cmd {
	// Pin the teacher's most recent own message
	for i := len(m.messages) - 1; i >= 0; i-- {
		msg := m.messages[i]
		if msg.From == m.app.Nickname {
			if err := m.app.PinMessage(msg.ID); err != nil {
				m.pushSysMsg("⚠ " + err.Error())
			} else {
				preview := msg.Content
				if len(preview) > 30 {
					preview = preview[:30] + "…"
				}
				m.pushSysMsg(fmt.Sprintf("📌 καρφιτσώθηκε: %s", preview))
			}
			return nil
		}
	}
	m.pushSysMsg("⚠ Δεν βρέθηκε δικό σου μήνυμα")
	return nil
}

func (m *Model) doUnpin(pos string) tea.Cmd {
	pos = strings.TrimPrefix(pos, "@")
	msgID, err := m.parseMsgPos(pos)
	if err != nil {
		m.pushSysMsg("⚠ " + err.Error())
		return nil
	}
	if err := m.app.UnpinMessage(msgID); err != nil {
		m.pushSysMsg("⚠ " + err.Error())
	} else {
		m.pushSysMsg(fmt.Sprintf("📌 @%s αποκαρφιτσώθηκε", pos))
	}
	return nil
}

func (m *Model) doUnpinLast() tea.Cmd {
	// Unpin the teacher's most recent pinned message
	for i := len(m.messages) - 1; i >= 0; i-- {
		msg := m.messages[i]
		if msg.Pinned && msg.From == m.app.Nickname {
			if err := m.app.UnpinMessage(msg.ID); err != nil {
				m.pushSysMsg("⚠ " + err.Error())
			} else {
				preview := msg.Content
				if len(preview) > 30 {
					preview = preview[:30] + "…"
				}
				m.pushSysMsg(fmt.Sprintf("📌 αποκαρφιτσώθηκε: %s", preview))
			}
			return nil
		}
	}
	m.pushSysMsg("⚠ Δεν βρέθηκε καρφιτσωμένο δικό σου μήνυμα")
	return nil
}

func (m *Model) doCopy(pos string) tea.Cmd {
	pos = strings.TrimPrefix(pos, "@")
	msgID, err := m.parseMsgPos(pos)
	if err != nil {
		m.pushSysMsg("⚠ " + err.Error())
		return nil
	}
	content := m.msgContent(msgID)
	if content == "" {
		m.pushSysMsg(fmt.Sprintf("⚠ @%s κενό ή δεν βρέθηκε", pos))
		return nil
	}
	if err := clipboard.WriteAll(content); err != nil {
		m.pushSysMsg("⚠ Αδύνατη η αντιγραφή: " + err.Error())
	} else {
		m.pushSysMsg(fmt.Sprintf("📋 @%s αντιγράφηκε", pos))
	}
	return nil
}

func (m *Model) doOpen(pos string) tea.Cmd {
	pos = strings.TrimPrefix(pos, "@")
	msgID, err := m.parseMsgPos(pos)
	if err != nil {
		m.pushSysMsg("⚠ " + err.Error())
		return nil
	}
	for _, msg := range m.messages {
		if msg.ID != msgID {
			continue
		}
		if msg.FileID != "" {
			go func(fileID, name string) {
				if !m.app.HasFile(fileID, name) {
					m.events <- evSysMsg{text: fmt.Sprintf("⚠ %s — δεν βρέθηκε τοπικά", name)}
					return
				}
				dest, err := m.app.DownloadFile(fileID, name)
				if err != nil {
					m.events <- evSysMsg{text: "⚠ " + err.Error()}
					return
				}
				if err := exec.Command("cmd", "/c", "start", "", dest).Start(); err != nil {
					m.events <- evSysMsg{text: "⚠ Αδύνατο άνοιγμα: " + err.Error()}
				} else {
					m.events <- evSysMsg{text: fmt.Sprintf("📂 Άνοιγμα %s", name)}
				}
			}(msg.FileID, msg.FileName)
			return nil
		}
		if url := extractURL(msg.Content); url != "" {
			if err := exec.Command("cmd", "/c", "start", "", url).Start(); err != nil {
				m.pushSysMsg("⚠ Αδύνατο άνοιγμα: " + err.Error())
			} else {
				m.pushSysMsg(fmt.Sprintf("🌐 Άνοιγμα %s", url))
			}
			return nil
		}
		m.pushSysMsg(fmt.Sprintf("⚠ @%s δεν περιέχει αρχείο ή URL", pos))
		return nil
	}
	m.pushSysMsg(fmt.Sprintf("⚠ @%s δεν βρέθηκε", pos))
	return nil
}

// extractURL finds the first URL-like token in text.
// Returns the URL with http:// prepended if no scheme was present.
func extractURL(text string) string {
	for _, token := range strings.Fields(text) {
		t := strings.TrimRight(token, ".,!?;:)")
		lower := strings.ToLower(t)
		if strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://") {
			return t
		}
		// bare domain: has a dot, no spaces, no @, TLD is 2-6 chars
		if strings.Contains(t, ".") && !strings.Contains(t, "@") {
			parts := strings.Split(t, ".")
			tld := strings.ToLower(parts[len(parts)-1])
			// strip any path/port that bled into the TLD check
			tld = strings.SplitN(tld, "/", 2)[0]
			tld = strings.SplitN(tld, ":", 2)[0]
			if len(tld) >= 2 && len(tld) <= 6 {
				return "http://" + t
			}
		}
	}
	return ""
}

// parseAliasCmd checks whether text matches any of the given command names (optionally with an arg).
// Returns (matched, hasArg, arg).
func parseAliasCmd(text string, cmds ...string) (matched, hasArg bool, arg string) {
	for _, cmd := range cmds {
		if text == cmd {
			return true, false, ""
		}
		if strings.HasPrefix(text, cmd+" ") {
			return true, true, strings.TrimSpace(text[len(cmd)+1:])
		}
	}
	return false, false, ""
}

// doBlacklistLast adds the most recent non-self message's words to the blacklist.
func (m *Model) doBlacklistLast() tea.Cmd {
	for i := len(m.messages) - 1; i >= 0; i-- {
		msg := m.messages[i]
		if msg.From != m.app.Nickname && msg.FileID == "" && !msg.Blocked {
			content := msg.Content
			words := strings.Fields(content)
			m.app.AddToBlacklist(words)
			m.app.DeleteMessage(msg.ID)
			m.pushSysMsg(fmt.Sprintf("🚫 διαγράφηκε → +%d λέξεις: %s", len(words), strings.Join(words, ", ")))
			return nil
		}
	}
	m.pushSysMsg("⚠ Δεν βρέθηκε μήνυμα")
	return nil
}

// doWhitelistLast adds the most recent non-self message's words to the whitelist.
func (m *Model) doWhitelistLast() tea.Cmd {
	for i := len(m.messages) - 1; i >= 0; i-- {
		msg := m.messages[i]
		if msg.From != m.app.Nickname && msg.FileID == "" && !msg.Blocked {
			words := strings.Fields(msg.Content)
			m.app.AddToWhitelist(words)
			m.pushSysMsg(fmt.Sprintf("✅ +%d λέξεις στη λευκή λίστα: %s", len(words), strings.Join(words, ", ")))
			return nil
		}
	}
	m.pushSysMsg("⚠ Δεν βρέθηκε μήνυμα")
	return nil
}

// doWhitelistMsg adds the words of message @X.Y to the whitelist.
func (m *Model) doWhitelistMsg(pos string) tea.Cmd {
	pos = strings.TrimPrefix(pos, "@")
	msgID, err := m.parseMsgPos(pos)
	if err != nil {
		m.pushSysMsg("⚠ " + err.Error())
		return nil
	}
	content := m.msgContent(msgID)
	if content == "" {
		m.pushSysMsg("⚠ Δεν βρέθηκε μήνυμα")
		return nil
	}
	words := strings.Fields(content)
	m.app.AddToWhitelist(words)
	m.pushSysMsg(fmt.Sprintf("✅ @%s → +%d λέξεις στη λευκή λίστα: %s", pos, len(words), strings.Join(words, ", ")))
	return nil
}

func (m *Model) doBlacklist(pos string) tea.Cmd {
	pos = strings.TrimPrefix(pos, "@")
	msgID, err := m.parseMsgPos(pos)
	if err != nil {
		m.pushSysMsg("⚠ " + err.Error())
		return nil
	}
	content := m.msgContent(msgID)
	if content == "" {
		m.pushSysMsg("⚠ Δεν βρέθηκε μήνυμα")
		return nil
	}
	words := strings.Fields(content)
	m.app.AddToBlacklist(words)
	m.app.DeleteMessage(msgID)
	m.pushSysMsg(fmt.Sprintf("🚫 @%s διαγράφηκε → +%d λέξεις: %s", pos, len(words), strings.Join(words, ", ")))
	return nil
}

// parseMsgPos resolves "@X.Y", "X.Y", "@pN", "pN", "@fN", or "fN" to a message ID.
// X=0 means self; pN = Nth pinned message; fN = Nth pinned file message.
func (m *Model) parseMsgPos(pos string) (string, error) {
	pos = strings.TrimPrefix(pos, "@")

	// @fN — pinned file message reference
	if strings.HasPrefix(pos, "f") {
		var n int
		if _, err := fmt.Sscanf(pos[1:], "%d", &n); err == nil && n >= 1 {
			files := m.pinnedFileMsgs()
			if n > len(files) {
				return "", fmt.Errorf("δεν υπάρχει καρφιτσωμένο αρχείο @f%d", n)
			}
			return files[n-1], nil
		}
	}

	// @pN — pinned message reference
	if strings.HasPrefix(pos, "p") {
		var n int
		if _, err := fmt.Sscanf(pos[1:], "%d", &n); err == nil && n >= 1 {
			pinned := m.pinnedMsgs()
			if n > len(pinned) {
				return "", fmt.Errorf("δεν υπάρχει καρφιτσωμένο @p%d", n)
			}
			return pinned[n-1], nil
		}
	}

	var x, y int
	if _, err := fmt.Sscanf(pos, "%d.%d", &x, &y); err != nil || x < 0 || y < 1 {
		return "", fmt.Errorf("άκυρη θέση '%s' — χρήση @X.Y ή @p1", pos)
	}
	var name string
	if x == 0 {
		name = m.app.Nickname
	} else {
		if x > len(m.senderNums) {
			return "", fmt.Errorf("δεν υπάρχει αποστολέας #%d", x)
		}
		name = m.senderNums[x-1]
	}
	window := m.msgWindow[name]
	if y > len(window) {
		return "", fmt.Errorf("δεν υπάρχει μήνυμα %s", pos)
	}
	return window[y-1], nil
}

// parsePushOpenCmd detects teacher push-open syntax and returns (target, destStr, ok).
// Form 1: "--op / --open <target> >N|>*"  (requires ">" to distinguish from local open)
// Form 2: "<url> --op [>N|>*]"            (text before --op is the URL, does NOT start with --)
func parsePushOpenCmd(text string) (target, destStr string, ok bool) {
	// Form 1: --op or --open with a ">" destination
	var form1Tail string
	if strings.HasPrefix(text, "--op ") {
		form1Tail = strings.TrimPrefix(text, "--op ")
	} else if strings.HasPrefix(text, "--open ") {
		form1Tail = strings.TrimPrefix(text, "--open ")
	}
	if form1Tail != "" {
		parts := strings.Fields(form1Tail)
		if len(parts) == 0 {
			return
		}
		for _, p := range parts[1:] {
			if strings.HasPrefix(p, ">") {
				target = parts[0]
				destStr = strings.TrimPrefix(p, ">")
				if destStr == "" {
					destStr = "*"
				}
				ok = true
				return
			}
		}
		return // no ">": local open, not a push
	}
	// Form 2: text not starting with "--", ending with " --op [>N]"
	if strings.HasPrefix(text, "--") {
		return
	}
	idx := strings.LastIndex(text, " --op")
	if idx < 0 {
		return
	}
	target = strings.TrimSpace(text[:idx])
	rest := strings.TrimSpace(text[idx+5:])
	destStr = "*"
	if strings.HasPrefix(rest, ">") {
		destStr = strings.TrimPrefix(rest, ">")
		if destStr == "" {
			destStr = "*"
		}
	} else if rest != "" {
		return // garbage after --op, not our syntax
	}
	if target == "" {
		return
	}
	ok = true
	return
}

// resolveStudentTarget maps ">N" / "*" to a student ID and display label.
func (m *Model) resolveStudentTarget(destStr string) (id, label string, err error) {
	if destStr == "*" || destStr == "" {
		return "", "όλους", nil
	}
	var n int
	if _, e := fmt.Sscanf(destStr, "%d", &n); e != nil || n < 1 || n > len(m.students) {
		return "", "", fmt.Errorf("άκυρος στόχος >%s (1–%d ή *)", destStr, len(m.students))
	}
	st := m.students[n-1]
	if !st.online {
		return "", "", fmt.Errorf("%s δεν είναι συνδεδεμένος", st.name)
	}
	return st.id, fmt.Sprintf("%s (@%d)", st.name, n), nil
}

// doPushOpen executes a teacher push-open for a URL or file reference.
func (m *Model) doPushOpen(target, destStr string) tea.Cmd {
	targetID, targetLabel, err := m.resolveStudentTarget(destStr)
	if err != nil {
		m.pushSysMsg("⚠ " + err.Error())
		return nil
	}

	// Is target a message reference @X.Y / @pN?
	if strings.HasPrefix(target, "@") {
		pos := strings.TrimPrefix(target, "@")
		msgID, err := m.parseMsgPos(pos)
		if err != nil {
			m.pushSysMsg("⚠ " + err.Error())
			return nil
		}
		for _, msg := range m.messages {
			if msg.ID != msgID {
				continue
			}
			if msg.FileID != "" {
				if err := m.app.PushOpenFile(msg.ID, targetID); err != nil {
					m.pushSysMsg("⚠ " + err.Error())
				} else {
					m.pushSysMsg(fmt.Sprintf("📤→📂 %s → %s", msg.FileName, targetLabel))
				}
				return nil
			}
			// Extract URL from message text
			url := extractURL(msg.Content)
			if url == "" {
				m.pushSysMsg(fmt.Sprintf("⚠ @%s δεν περιέχει URL ή αρχείο", pos))
				return nil
			}
			target = url
			break
		}
	}

	// Push URL silently
	if err := m.app.PushOpenURL(target, targetID); err != nil {
		m.pushSysMsg("⚠ " + err.Error())
	} else {
		m.pushSysMsg(fmt.Sprintf("🌐→ %s → %s", target, targetLabel))
	}
	return nil
}

// ── History & completion ──────────────────────────────────────────────────────

func (m *Model) historyUp() {
	if len(m.history) == 0 {
		return
	}
	if m.historyIdx == -1 {
		// entering history mode — save current input
		m.historySaved = m.input.Value()
		m.historyIdx = len(m.history) - 1
	} else if m.historyIdx > 0 {
		m.historyIdx--
	}
	m.input.SetValue(m.history[m.historyIdx])
	// move cursor to end
	m.input.CursorEnd()
}

func (m *Model) historyDown() {
	if m.historyIdx == -1 {
		return
	}
	if m.historyIdx < len(m.history)-1 {
		m.historyIdx++
		m.input.SetValue(m.history[m.historyIdx])
	} else {
		// past the end — restore saved input
		m.historyIdx = -1
		m.input.SetValue(m.historySaved)
	}
	m.input.CursorEnd()
}

// tabCompletions maps partial inputs to their full --command expansion.
// Single-letter shortcuts let students type like a terminal.
var tabCompletions = []struct{ prefix, full string }{
	// short → long
	{"--h", "--help"},
	{"--dl", "--download"},
	{"--d", "--download"},
	{"--op", "--open"},
	{"--o", "--open"},
	{"--cp", "--copy"},
	{"--c", "--copy"},
	{"--a", "--attach"},
	{"--p", "--pin"},
	{"--u", "--unpin"},
	{"--r", "--report"},
	{"--b", "--black"},
	{"--ps", "--pass"},
	{"--cl", "--clr"},
	// single-letter shortcuts
	{"h", "--help"},
	{"d", "--download"},
	{"o", "--open"},
	{"a", "--attach"},
	{"p", "--pin"},
	{"u", "--unpin"},
	{"r", "--report"},
	{"c", "--copy"},
	{"t", "--t"},
}

// toolNames is the ordered list of --t tool keywords cycled by Tab.
var toolNames = []string{
	"lock", "unlock", "mute", "unmute", "shot",
	"close", "shutdown", "block", "unblock", "focus", "launch",
	"tvon", "tvoff", "start-casting", "stop-casting", "cast", "caston", "castoff",
}

func (m *Model) doTabComplete() {
	cur := m.input.Value()

	// If we already have matches, cycle through them
	if len(m.tabMatches) > 0 {
		m.tabIdx = (m.tabIdx + 1) % len(m.tabMatches)
		m.input.SetValue(m.tabMatches[m.tabIdx])
		m.input.CursorEnd()
		return
	}

	trimmed := strings.TrimSpace(cur)
	if trimmed == "" {
		return
	}

	// Special case: --t / --tool <partial> → cycle through tool names
	for _, pfx := range []string{"--t ", "--tool "} {
		if strings.HasPrefix(trimmed, pfx) || trimmed == strings.TrimSpace(pfx) {
			partial := ""
			if strings.HasPrefix(trimmed, pfx) {
				partial = strings.TrimSpace(trimmed[len(pfx):])
			}
			var matches []string
			for _, t := range toolNames {
				if strings.HasPrefix(t, partial) {
					matches = append(matches, "--t "+t)
				}
			}
			if len(matches) > 0 {
				m.tabMatches = matches
				m.tabIdx = 0
				m.input.SetValue(matches[0])
				m.input.CursorEnd()
			}
			return
		}
	}

	// Build match list for current input from tabCompletions table
	var matches []string
	for _, e := range tabCompletions {
		if e.prefix == trimmed {
			matches = append(matches, e.full)
		} else if strings.HasPrefix(e.full, trimmed) && e.full != trimmed {
			matches = append(matches, e.full)
		}
	}
	if len(matches) == 0 {
		return
	}
	m.tabMatches = matches
	m.tabIdx = 0
	m.input.SetValue(matches[0])
	m.input.CursorEnd()
}

// updateInputStyle sets the textarea text colour to blue when the current text
// looks like a command (any token starting with "--"), otherwise resets to default.
// Both Text (non-cursor lines) and CursorLine (active line) must be kept in sync;
// the textarea renders the cursor line via CursorLine, not Text.
func (m *Model) updateInputStyle() {
	var s lipgloss.Style
	if looksLikeCommand(m.input.Value()) {
		s = lipgloss.NewStyle().Foreground(colCmd).Bold(true)
	} else {
		s = lipgloss.NewStyle().Foreground(colText)
	}
	m.input.FocusedStyle.Text = s
	m.input.FocusedStyle.CursorLine = s
}

// knownCmdTokens is the set of valid --command keywords (exact, no arguments).
// Only text containing one of these tokens turns blue.
var knownCmdTokens = map[string]bool{
	"--help": true, "--h": true,
	"--cp": true, "--copy": true,
	"--op": true, "--open": true,
	"--dl": true, "--download": true,
	"--a": true, "--attach": true,
	"--pin": true,
	"--upin": true, "--unpin": true,
	"--rep": true, "--report": true,
	"--pass": true, "--ps": true,
	"--black": true, "--blk": true,
	"--white": true, "--wh": true,
	"--del": true, "--rem": true, "--delete": true,
	"--clr": true, "--clear": true,
	"--t": true, "--tool": true,
	"--send": true,
}

// looksLikeCommand returns true when any whitespace-separated token exactly
// matches a known command keyword.  "--ata" is NOT a command; "--a" is.
func looksLikeCommand(text string) bool {
	for _, tok := range strings.Fields(text) {
		if knownCmdTokens[tok] {
			return true
		}
	}
	return false
}

// msgContent returns the content of a message by ID from the local messages slice
func (m *Model) msgContent(id string) string {
	for _, msg := range m.messages {
		if msg.ID == id {
			return msg.Content
		}
	}
	return ""
}

// ── List overlay (blacklist / whitelist) ──────────────────────────────────────

// parseListRef detects "@LN" or "@WN" patterns and returns (n, true).
// prefix is 'L' for blacklist, 'W' for whitelist.
func parseListRef(arg string, prefix byte) (int, bool) {
	s := strings.TrimPrefix(arg, "@")
	if len(s) < 2 || s[0] != prefix {
		return 0, false
	}
	var n int
	if _, err := fmt.Sscanf(s[1:], "%d", &n); err != nil || n < 1 {
		return 0, false
	}
	return n, true
}

func (m *Model) overlayList(base string) string {
	const overlayW = 52
	const maxVisible = 20

	// Build the flat line list
	var lines []string
	lines = append(lines, styleTitle.Render("  ΜΑΥΡΗ ΛΙΣΤΑ"))
	if len(m.app.Blacklist) == 0 {
		lines = append(lines, styleHint.Render("  (κενή)"))
	} else {
		for i, w := range m.app.Blacklist {
			lbl := styleMsgNum.Render(fmt.Sprintf("[B%d]", i+1))
			lines = append(lines, fmt.Sprintf("  %s  %s", lbl, w))
		}
	}
	lines = append(lines, "")
	lines = append(lines, styleTitle.Render("  ΛΕΥΚΗ ΛΙΣΤΑ"))
	if len(m.app.Whitelist) == 0 {
		lines = append(lines, styleHint.Render("  (κενή)"))
	} else {
		for i, w := range m.app.Whitelist {
			lbl := styleMsgNum.Render(fmt.Sprintf("[W%d]", i+1))
			lines = append(lines, fmt.Sprintf("  %s  %s", lbl, w))
		}
	}

	total := len(lines)
	start := m.listScroll
	if start > total-maxVisible {
		start = total - maxVisible
	}
	if start < 0 {
		start = 0
	}
	end := start + maxVisible
	if end > total {
		end = total
	}

	var out []string
	out = append(out, styleTitle.Render("  📋 Λίστες"))
	out = append(out, strings.Repeat("─", overlayW))
	for _, l := range lines[start:end] {
		out = append(out, lipgloss.NewStyle().Width(overlayW).Foreground(colText).Render(l))
	}
	out = append(out, strings.Repeat("─", overlayW))
	out = append(out, styleHint.Render("  --blk <λέξεις>  προσθήκη στη μαύρη λίστα"))
	out = append(out, styleHint.Render("  --blk @BN   αφαίρεση από μαύρη λίστα"))
	out = append(out, styleHint.Render("  --wh  <λέξεις>  προσθήκη στη λευκή λίστα"))
	out = append(out, styleHint.Render("  --wh  @WN   αφαίρεση από λευκή λίστα"))
	if total > maxVisible {
		out = append(out, styleHint.Render(fmt.Sprintf("  [↑↓] Κύλιση  %d/%d  [Esc] Κλείσιμο", start+1, total)))
	} else {
		out = append(out, styleHint.Render("  [Esc] Κλείσιμο"))
	}

	panel := styleBorder.Padding(1, 2).Render(strings.Join(out, "\n"))
	return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center,
		panel,
		lipgloss.WithWhitespaceChars(" "),
		lipgloss.WithWhitespaceForeground(colBg),
	)
}

// ── Settings ──────────────────────────────────────────────────────────────────

func (m *Model) handleSet(args string) {
	// Split into setting name (lowercased) + rest (original case for paths)
	args = strings.TrimSpace(args)
	if args == "" {
		m.pushSysMsg("Χρήση: --set <ρύθμιση> <τιμή>  π.χ. --set autostart on")
		return
	}
	spaceIdx := strings.IndexByte(args, ' ')
	var setting, rest string
	if spaceIdx < 0 {
		setting = strings.ToLower(args)
	} else {
		setting = strings.ToLower(args[:spaceIdx])
		rest = strings.TrimSpace(args[spaceIdx+1:])
	}

	switch setting {
	case "autostart":
		if m.app.Role != core.RoleTeacher {
			m.pushSysMsg("⚠ --set autostart: μόνο για τον δάσκαλο")
			return
		}
		value := strings.ToLower(rest)
		if m.app.SetAutostart == nil {
			m.pushSysMsg("⚠ --set autostart: διαθέσιμο μόνο σε Windows")
			return
		}
		switch value {
		case "on":
			if err := m.app.SetAutostart(true); err != nil {
				m.pushSysMsg("⚠ " + err.Error())
			} else {
				m.pushSysMsg("✅ Αυτόματη εκκίνηση: ενεργή")
			}
		case "off":
			if err := m.app.SetAutostart(false); err != nil {
				m.pushSysMsg("⚠ " + err.Error())
			} else {
				m.pushSysMsg("✅ Αυτόματη εκκίνηση: ανενεργή")
			}
		default:
			enabled := m.app.IsAutostartEnabled != nil && m.app.IsAutostartEnabled()
			state := "off"
			if enabled {
				state = "on"
			}
			m.pushSysMsg(fmt.Sprintf("autostart = %s  (--set autostart on|off)", state))
		}

	case "nickname":
		if rest == "" {
			m.pushSysMsg(fmt.Sprintf("nickname = %s  (--set nickname <όνομα>)", m.app.Nickname))
			return
		}
		if err := m.app.SetNickname(rest); err != nil {
			m.pushSysMsg("⚠ " + err.Error())
		} else {
			m.pushSysMsg(fmt.Sprintf("✅ Όνομα: %s", m.app.Nickname))
		}

	case "list":
		if m.app.Role != core.RoleTeacher {
			m.pushSysMsg("⚠ --set list: μόνο για τον δάσκαλο")
			return
		}
		m.handleSetList(rest)

	default:
		m.pushSysMsg(fmt.Sprintf("Άγνωστη ρύθμιση: %s", setting))
	}
}

func (m *Model) handleSetList(rest string) {
	// split sub-command (lowercased) from path (original case)
	rest = strings.TrimSpace(rest)
	spaceIdx := strings.IndexByte(rest, ' ')
	var subCmd, path string
	if spaceIdx < 0 {
		subCmd = strings.ToLower(rest)
	} else {
		subCmd = strings.ToLower(rest[:spaceIdx])
		path = strings.TrimSpace(rest[spaceIdx+1:])
	}

	// expand leading ~
	if strings.HasPrefix(path, "~/") || path == "~" {
		home, _ := os.UserHomeDir()
		path = filepath.Join(home, path[2:])
	}

	switch subCmd {
	case "import":
		if path == "" {
			m.pushSysMsg("Χρήση: --set list import <αρχείο.json>")
			return
		}
		bl, wl, err := m.app.ImportLists(path)
		if err != nil {
			m.pushSysMsg("⚠ " + err.Error())
		} else {
			m.pushSysMsg(fmt.Sprintf("✅ Εισαγωγή: %d μαύρη · %d λευκή  (^L για προβολή)", bl, wl))
		}
	case "export":
		if path == "" {
			home, _ := os.UserHomeDir()
			path = filepath.Join(home, "Downloads",
				fmt.Sprintf("classsend-lists-%s.json", time.Now().Format("2006-01-02")))
		}
		if err := m.app.ExportLists(path); err != nil {
			m.pushSysMsg("⚠ " + err.Error())
		} else {
			m.pushSysMsg(fmt.Sprintf("📤 Εξαγωγή → %s", path))
		}
	default:
		m.pushSysMsg("Χρήση: --set list import <αρχείο> | --set list export [αρχείο]")
	}
}

// ── Matrix easter egg ─────────────────────────────────────────────────────────

var matrixChars = []rune("ｦｧｨｩｪｫｬｭｮｯｱｲｳｴｵｶｷｸｹｺｻｼｽｾｿﾀﾁﾂﾃﾄﾅﾆﾇﾈﾉﾊﾋﾌﾍﾎﾏﾐﾑﾒﾓﾔﾕﾖﾗﾘﾙﾚﾛﾜﾝ0123456789")

func matrixTick() tea.Cmd {
	return tea.Tick(60*time.Millisecond, func(time.Time) tea.Msg {
		return evMatrixTick{}
	})
}

func (m *Model) startMatrix() tea.Cmd {
	m.matrixActive = true
	m.matrixFrame = 0
	cols := m.width
	if cols <= 0 {
		cols = 80
	}
	rows := m.height
	if rows <= 0 {
		rows = 24
	}
	m.matrixHeads = make([]int, cols)
	for i := range m.matrixHeads {
		m.matrixHeads[i] = rand.Intn(rows) - rows
	}
	return matrixTick()
}

func (m *Model) renderMatrix() string {
	cols := m.width
	rows := m.height
	if cols <= 0 || rows <= 0 || len(m.matrixHeads) == 0 {
		return ""
	}

	const (
		ansiReset  = "\033[0m"
		ansiBright = "\033[1;92m" // bright green — rain head
		ansiMid    = "\033[32m"   // green — near tail
		ansiDim    = "\033[2;32m" // dim green — far tail
	)

	var sb strings.Builder
	sb.Grow(rows * cols * 6)

	for row := 0; row < rows; row++ {
		written := 0
		for col := 0; col < cols && col < len(m.matrixHeads); col++ {
			dist := m.matrixHeads[col] - row
			ch := string(matrixChars[rand.Intn(len(matrixChars))])
			switch {
			case dist == 0:
				sb.WriteString(ansiBright + ch + ansiReset)
			case dist == 1 || dist == 2:
				sb.WriteString(ansiMid + ch + ansiReset)
			case dist > 2 && dist <= 6:
				sb.WriteString(ansiDim + ch + ansiReset)
			default:
				sb.WriteByte(' ')
			}
			written++
		}
		for ; written < cols; written++ {
			sb.WriteByte(' ')
		}
		if row < rows-1 {
			sb.WriteByte('\n')
		}
	}
	return sb.String()
}

// syncMessages merges a server snapshot into the local list.
// System messages (local-only) are preserved; deleted messages are removed;
// updated fields (Reported, Pinned) are applied in-place.
func (m *Model) syncMessages(msgs []core.ChatMessage) {
	if len(msgs) == 0 {
		// Full clear — keep only system messages
		var kept []core.ChatMessage
		for _, msg := range m.messages {
			if msg.From == "system" {
				kept = append(kept, msg)
			}
		}
		m.messages = kept
		m.msgWindow = make(map[string][]string)
		m.senderNums = nil
		m.refreshViewport()
		return
	}
	updated := make(map[string]core.ChatMessage, len(msgs))
	for _, msg := range msgs {
		updated[msg.ID] = msg
	}
	result := m.messages[:0]
	for _, msg := range m.messages {
		if msg.From == "system" {
			result = append(result, msg)
			continue
		}
		if u, ok := updated[msg.ID]; ok {
			result = append(result, u)
		}
		// absent from snapshot = deleted, drop it
	}
	m.messages = result
	m.refreshViewport()
}
