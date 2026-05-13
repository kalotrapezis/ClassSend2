package tui

import (
	"archive/zip"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/atotto/clipboard"
	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"classsend/internal/buildinfo"
	"classsend/internal/core"
	"classsend/internal/devlog"
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

	aboutOpen   bool
	aboutScroll int

	favoritesOpen   bool
	favoritesCursor int
	favoritesScroll int

	// Scheduled-commands overlay (^X). Shows the same jobs as `--sched` but
	// in a navigable panel with per-row cancel.
	schedOpen   bool
	schedCursor int
	schedScroll int

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

	// Pending tool command awaiting --y / --n confirmation. Set when the
	// teacher types `| HH:MM` for a time already past today — the command
	// is held here and only scheduled (for tomorrow) on --y. Cleared on
	// --n or any other input that isn't --y/--n.
	pendingSchedule *pendingScheduledCmd

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
	// Disable the textarea's built-in Enter→newline binding. Enter is the send
	// key in this app; we never want it to insert a newline into the buffer.
	ta.KeyMap.InsertNewline.SetEnabled(false)

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

	// Bootstrap: if the agent already reported "connected to teacher" before
	// OnConnected was wired (race when the agent had cached state and replayed
	// a TypeConnected frame the instant ConnectViaAgent dialed), synthesise
	// the event now so the TUI leaves the "searching" screen on first paint.
	if app.Role == core.RoleStudent && app.IsConnectedToTeacher() {
		// Non-blocking — m.events has buffer; if it ever doesn't, a goroutine
		// keeps us out of the constructor's call path.
		select {
		case m.events <- evConnected{}:
		default:
			go func() { m.events <- evConnected{} }()
		}
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

// PushSysMsg surfaces a system message in the chat scroll, from outside the
// model goroutine (e.g. the scheduler's fire callback). Non-blocking: drops
// the message if the event queue is saturated rather than stalling the
// caller — sys messages are advisory, not critical.
func (m *Model) PushSysMsg(text string) {
	select {
	case m.events <- evSysMsg{text: text}:
	default:
	}
}

func (m *Model) Init() tea.Cmd {
	role := string(m.app.Role)
	if role != "" {
		role = strings.ToUpper(role[:1]) + role[1:]
	}
	title := "ClassSend 2 — " + role + "  [" + buildinfo.String() + "]"
	return tea.Batch(
		m.spinner.Tick,
		m.waitForEvent(),
		m.input.Focus(),
		tea.SetWindowTitle(title),
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
	aboutWasOpen      := m.aboutOpen      // same guard for about overlay
	favoritesWasOpen  := m.favoritesOpen  // same guard for favorites overlay
	schedWasOpen      := m.schedOpen      // same guard for scheduled-commands overlay
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
	noOverlay := !m.toolsOpen && !toolsWereOpen && !m.filePickerOpen && !filePickerWasOpen && !m.helpOpen && !helpWasOpen && !m.aboutOpen && !aboutWasOpen && !m.favoritesOpen && !favoritesWasOpen && !m.schedOpen && !schedWasOpen && !m.listOpen && !listWasOpen
	isEnter := func() bool {
		k, ok := msg.(tea.KeyMsg)
		if !ok {
			return false
		}
		s := k.String()
		return s == "enter" || s == "shift+enter" || s == "ctrl+j" || s == "ctrl+m"
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
		// ^W toggles classroom monitoring (tvon/tvoff). Teacher-only — on
		// students this key is unbound. Repurposed from the previous
		// "focus input" binding (input is already focused most of the time;
		// the monitoring toggle is much more frequently useful).
		if m.screen == screenChat && m.app.Role == core.RoleTeacher {
			if m.state.Monitoring {
				return m.handleToolCmd("tvoff")
			}
			return m.handleToolCmd("tvon")
		}

	case "ctrl+f":
		// ^F primes a focus-window command. Teacher fills in the window title
		// after the prefix, e.g. "--t focus Chrome", and the agent calls
		// SetForegroundWindow on the matched window. Pre-staging the prefix
		// keeps the syntax discoverable without a separate prompt.
		if m.screen == screenChat && m.app.Role == core.RoleTeacher {
			m.input.SetValue("--t focus ")
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
		// ^L toggles the screen-lock state on every student. Repurposed from
		// the old list overlay (now on ^G). Reads m.state.ScreenLocked which
		// the teacher mirrors authoritatively, so the toggle stays correct
		// even if the state was changed via --t lock from the command line.
		if m.screen == screenChat && m.app.Role == core.RoleTeacher {
			if m.state.ScreenLocked {
				return m.handleToolCmd("unlock")
			}
			return m.handleToolCmd("lock")
		}

	case "ctrl+g":
		// ^G — blacklist/whitelist overlay (formerly ^L).
		if m.screen == screenChat && m.app.Role == core.RoleTeacher {
			m.listOpen = !m.listOpen
			m.listScroll = 0
		}

	case "ctrl+z":
		// ^Z toggles class-wide mute. Mirror of ^L for sound.
		if m.screen == screenChat && m.app.Role == core.RoleTeacher {
			if m.state.Muted {
				return m.handleToolCmd("unmute")
			}
			return m.handleToolCmd("mute")
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
		if m.aboutOpen {
			m.aboutOpen = false
			return nil
		}
		if m.favoritesOpen {
			m.favoritesPlaceSelected()
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

	case "ctrl+n":
		// ^N opens Path Notes — saved URLs / file paths the teacher has
		// push-opened or attached. Teacher only.
		if m.app.Role == core.RoleTeacher && m.screen == screenChat {
			m.favoritesOpen = !m.favoritesOpen
			m.favoritesCursor = 0
			m.favoritesScroll = 0
		}

	case "ctrl+x":
		// ^X opens the scheduled-commands panel — every pending `--t … | …`
		// job. Teacher-only since only the teacher schedules. Toggles open
		// when no other overlay is up; closes itself if already open. `d` /
		// Delete cancels the highlighted row; ↑↓ navigate; Esc closes.
		if m.app.Role == core.RoleTeacher && m.screen == screenChat {
			if m.schedOpen {
				m.schedOpen = false
			} else if !m.listOpen && !m.helpOpen && !m.aboutOpen && !m.favoritesOpen && !m.filePickerOpen && !m.toolsOpen {
				m.schedOpen = true
				m.schedCursor = 0
				m.schedScroll = 0
			}
		}

	case "ctrl+b":
		if m.app.Role == core.RoleTeacher && m.screen == screenChat {
			return m.shortcutAction("black")
		}

	case "ctrl+o":
		if m.screen == screenChat {
			// Teacher: if a file is staged (just attached via ^A), ^O fires
			// the keyboard equivalent of "--op this > *" — sends to all with
			// auto-open in one stroke. Lets the user do attach→push without
			// touching the textarea. Falls through to the existing pass-by-
			// reference shortcut if nothing is staged.
			if m.app.Role == core.RoleTeacher && m.stagedFile != "" {
				staged := m.stagedFile
				m.stagedFile = ""
				if err := m.app.SendFile(staged, "", "", true); err != nil {
					m.pushSysMsg("⚠ " + err.Error())
				} else {
					m.pushSysMsg(fmt.Sprintf("📤→📂 %s → όλους", filepath.Base(staged)))
				}
				return nil
			}
			if m.app.Role == core.RoleTeacher {
				return m.shortcutAction("pass")
			}
			return m.shortcutAction("op")
		}

	case "esc":
		if m.listOpen {
			m.listOpen = false
		} else if m.helpOpen {
			m.helpOpen = false
		} else if m.aboutOpen {
			m.aboutOpen = false
		} else if m.favoritesOpen {
			m.favoritesOpen = false
		} else if m.schedOpen {
			m.schedOpen = false
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
		} else if m.aboutOpen {
			if m.aboutScroll > 0 {
				m.aboutScroll--
			}
		} else if m.favoritesOpen {
			if m.favoritesCursor > 0 {
				m.favoritesCursor--
				if m.favoritesCursor < m.favoritesScroll {
					m.favoritesScroll = m.favoritesCursor
				}
			}
		} else if m.schedOpen {
			if m.schedCursor > 0 {
				m.schedCursor--
				if m.schedCursor < m.schedScroll {
					m.schedScroll = m.schedCursor
				}
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
		} else if m.aboutOpen {
			lines := m.aboutLines()
			const maxVis = 22
			if m.aboutScroll < len(lines)-maxVis {
				m.aboutScroll++
			}
		} else if m.favoritesOpen {
			n := len(m.app.FavoritesSnapshot())
			if m.favoritesCursor < n-1 {
				m.favoritesCursor++
				const maxVis = 18
				if m.favoritesCursor >= m.favoritesScroll+maxVis {
					m.favoritesScroll = m.favoritesCursor - maxVis + 1
				}
			}
		} else if m.schedOpen {
			n := len(m.app.Sched.List())
			if m.schedCursor < n-1 {
				m.schedCursor++
				const maxVis = 14
				if m.schedCursor >= m.schedScroll+maxVis {
					m.schedScroll = m.schedCursor - maxVis + 1
				}
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

	case " ", "pgdown":
		// Space / PgDn = page down inside help & about overlays.
		// When neither overlay is open the case is a no-op; the parent's
		// noOverlay gate forwards the keypress to the textarea normally.
		const helpVis = 22
		page := helpVis - 2 // keep 2 lines of overlap between pages
		if m.helpOpen {
			lines := m.helpLines()
			m.helpScroll += page
			if max := len(lines) - helpVis; m.helpScroll > max {
				m.helpScroll = max
			}
			if m.helpScroll < 0 {
				m.helpScroll = 0
			}
			return nil
		}
		if m.aboutOpen {
			lines := m.aboutLines()
			m.aboutScroll += page
			if max := len(lines) - helpVis; m.aboutScroll > max {
				m.aboutScroll = max
			}
			if m.aboutScroll < 0 {
				m.aboutScroll = 0
			}
			return nil
		}

	case "pgup":
		const helpVis = 22
		page := helpVis - 2
		if m.helpOpen {
			m.helpScroll -= page
			if m.helpScroll < 0 {
				m.helpScroll = 0
			}
			return nil
		}
		if m.aboutOpen {
			m.aboutScroll -= page
			if m.aboutScroll < 0 {
				m.aboutScroll = 0
			}
			return nil
		}

	case "k":
		if !m.focusInput && !m.helpOpen && !m.aboutOpen && !m.favoritesOpen && !m.schedOpen && !m.filePickerOpen && !m.toolsOpen {
			if m.selectedSt > 0 {
				m.selectedSt--
			}
		}

	case "j":
		if !m.focusInput && !m.helpOpen && !m.aboutOpen && !m.favoritesOpen && !m.schedOpen && !m.filePickerOpen && !m.toolsOpen {
			if m.selectedSt < len(m.students)-1 {
				m.selectedSt++
			}
		}

	case "d", "delete":
		// Inside the favorites overlay, 'd' (or Delete) removes the highlighted
		// entry. Inside the sched overlay, the same key cancels the highlighted
		// scheduled job. Outside any overlay this falls through to the input
		// field as a normal letter.
		if m.favoritesOpen {
			m.favoritesDeleteSelected()
			return nil
		}
		if m.schedOpen {
			m.schedCancelSelected()
			return nil
		}

	case "s":
		// Inside the favorites overlay, 's' pins/unpins the highlighted entry.
		// Pinned entries float to the top and survive the cap. Outside the
		// overlay this falls through to chat input.
		if m.favoritesOpen {
			m.favoritesTogglePinSelected()
			return nil
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

	// --y / --n: confirm or reject a pending scheduled command (HH:MM rolled
	// to tomorrow). Any other input cancels the pending command.
	if m.app.Role == core.RoleTeacher && m.pendingSchedule != nil {
		switch text {
		case "--y", "--yes":
			job := m.pendingSchedule.job
			m.pendingSchedule = nil
			id := m.app.Sched.Add(job)
			m.pushSysMsg(fmt.Sprintf("⏰ %s → %s @ %s [%s]",
				job.Label, job.TargetText, job.When.Format("Mon 15:04"), id))
			return nil
		case "--n", "--no":
			m.pendingSchedule = nil
			m.pushSysMsg("✓ Ακυρώθηκε — η εντολή δεν προγραμματίστηκε.")
			return nil
		default:
			// Drop the pending command but DO let the user's new input fall
			// through to its normal handler — most natural UX: typing
			// something else means "I changed my mind, do this instead".
			m.pendingSchedule = nil
			m.pushSysMsg("✓ Η εκκρεμής εντολή ακυρώθηκε.")
			// fall through
		}
	}

	// --sched: list pending scheduled jobs, or cancel one by id.
	if m.app.Role == core.RoleTeacher {
		if text == "--sched" || text == "--schedule" || text == "--sch" {
			if m.app.Sched == nil {
				m.pushSysMsg("ℹ Κανένας προγραμματισμός.")
				return nil
			}
			jobs := m.app.Sched.List()
			if len(jobs) == 0 {
				m.pushSysMsg("ℹ Καμία προγραμματισμένη εντολή.")
				return nil
			}
			m.pushSysMsg(fmt.Sprintf("📋 Προγραμματισμένα (%d):", len(jobs)))
			for _, j := range jobs {
				m.pushSysMsg(fmt.Sprintf("  [%s] %s → %s @ %s (σε %s)",
					j.ID, j.Label, j.TargetText,
					j.When.Format("Mon 15:04"),
					humanDuration(time.Until(j.When))))
			}
			return nil
		}
		if strings.HasPrefix(text, "--sched cancel ") ||
			strings.HasPrefix(text, "--schedule cancel ") ||
			strings.HasPrefix(text, "--sch cancel ") {
			id := strings.TrimSpace(text[strings.Index(text, "cancel")+6:])
			if id == "" {
				m.pushSysMsg("Χρήση: --sched cancel <id>  (π.χ. --sched cancel S1)")
				return nil
			}
			if job, ok := m.app.Sched.Cancel(id); ok {
				m.pushSysMsg(fmt.Sprintf("🗑 Ακυρώθηκε [%s] %s → %s @ %s",
					job.ID, job.Label, job.TargetText, job.When.Format("Mon 15:04")))
			} else {
				m.pushSysMsg(fmt.Sprintf("⚠ Δεν βρέθηκε προγραμματισμός: %s", id))
			}
			return nil
		}
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
		delCmd, delHasArg, delArg := parseAliasCmd(text, "--rm", "--del", "--rem", "--delete")
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

	// Student: --cast  re-open the cast viewer window
	if text == "--cast" && m.app.Role == core.RoleStudent {
		if m.app.HasAgentConn() {
			m.app.SendShowCast()
			m.pushSysMsg("📡 Ενεργοποίηση παραθύρου cast...")
		} else {
			m.pushSysMsg("⚠ --cast: απαιτεί τον πράκτορα (classsend-agent.exe)")
		}
		return nil
	}

	// --mon / --monitor  shortcut for "--t tvon" — re-opens the monitoring
	// window if the teacher closed it by mistake. Symmetric to --cast for
	// students. Note: tvoff still works to stop monitoring.
	if m.app.Role == core.RoleTeacher && (text == "--mon" || text == "--monitor") {
		return m.handleToolCmd("tvon")
	}

	// --pa / --path  Path Notes manager (teacher only).
	//   --pa | --path | --path open      → open the ^N overlay
	//   --path save  <url-or-path>       → save value, surfaces at top
	//   --path delete <url-or-path>      → remove value
	//   --path remove                    → alias for delete
	if m.app.Role == core.RoleTeacher && isPathCmd(text) {
		return m.handlePathCmd(text)
	}

	// --log  show the active session log path (or warn if logging is off)
	if text == "--log" {
		if p := devlog.Path(); p != "" {
			m.pushSysMsg("📝 Log: " + p)
		} else {
			m.pushSysMsg("⚠ Logging απενεργοποιημένο ή απέτυχε η αρχικοποίηση")
		}
		return nil
	}

	// Easter eggs
	if text == "--coffee" {
		m.pushSysMsg("☕ Κάνε ένα διάλειμμα... αξίζεις έναν καφέ!")
		return nil
	}

	// About / version. Reads about.md next to the running .exe so we can
	// update the page after deployment without rebuilding. Falls back to a
	// minimal one-liner if the file is missing.
	if text == "--ver" || text == "--version" || text == "--about" {
		m.aboutOpen = true
		m.aboutScroll = 0
		return nil
	}

	// --bug / --report — bundles every recent .log file from the install
	// dir's logs/ folder into a single zip in Downloads, then tells the
	// user where it is and how to share it. Goal: make "send me your logs"
	// a 5-second copy-paste, not a 5-minute file hunt.
	if text == "--bug" || text == "--report" {
		zipPath, count, err := bundleBugReport()
		if err != nil {
			m.pushSysMsg("⚠ Δεν δημιουργήθηκε η αναφορά: " + err.Error())
			return nil
		}
		m.pushSysMsg("📦 Αναφορά σφάλματος: " + zipPath)
		m.pushSysMsg(fmt.Sprintf("   (%d αρχεία logs συμπεριλήφθηκαν, build=%s)", count, buildinfo.String()))
		m.pushSysMsg("Στείλε το zip στο: kalotrapezis@gmail.com")
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
				return m.handleToolCmd(tArg)
			}
			m.pushSysMsg("Χρήση: --t <lock|unlock|mute|…> [>N]  —  Tab για λίστα")
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

	// Student: check against local blacklist copy before sending
	if m.app.Role == core.RoleStudent {
		if blocked := m.app.CheckBlacklist(text); blocked != "" {
			m.pushSysMsg(fmt.Sprintf("🚫 Η λέξη \"%s\" δεν επιτρέπεται", blocked))
			return nil
		}
	}

	// If a file is staged, send it with the message text as caption.
	// Special teacher syntax — combine attach + push-open in one action:
	//   "--op this > *"   send file + auto-open on every student
	//   "--op this > N"   send file + auto-open on student #N
	//   "--op > *" / "--op > N" also accepted as a shorthand
	if m.stagedFile != "" {
		staged := m.stagedFile
		caption := text

		autoOpen := false
		var targetID, targetLabel string
		if m.app.Role == core.RoleTeacher {
			if destStr, ok := parseStagedPushOpen(text); ok {
				id, label, err := m.resolveStudentTarget(destStr)
				if err != nil {
					m.pushSysMsg("⚠ " + err.Error())
					return nil
				}
				autoOpen = true
				targetID, targetLabel = id, label
				caption = "" // drop the --op directive from the caption — fall back to filename
			}
		}

		m.stagedFile = ""
		if err := m.app.SendFile(staged, caption, targetID, autoOpen); err != nil {
			m.pushSysMsg("⚠ " + err.Error())
			return nil
		}
		if autoOpen {
			m.pushSysMsg(fmt.Sprintf("📤→📂 %s → %s", filepath.Base(staged), targetLabel))
		} else {
			m.pushSysMsg(fmt.Sprintf("📤 Αποστολή: %s", filepath.Base(staged)))
		}
		return nil
	}

	// Block unrecognised -- commands from being sent as chat messages.
	// Without this, typing e.g. "--lock" falls through and broadcasts as
	// a regular message to all students, piling up in the viewport.
	if strings.HasPrefix(text, "--") {
		m.pushSysMsg(fmt.Sprintf("⚠ Άγνωστη εντολή: %s  (--help για λίστα)", text))
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

// toolActionEntry is one row of the tool action table.
type toolActionEntry struct {
	cmd   string
	label string
	param bool // expects a param (e.g. exe path)
}

// toolActions maps short and full action codes to their protocol command +
// display label. Both forms are accepted at runtime and shown in the help.
// Keep aliases in sync with toolActionNames (input-coloring) and toolNames
// (Tab cycling).
var toolActions = map[string]toolActionEntry{
	"lk":               {protocol.CmdLockScreen, "🔒 Κλείδωμα", false},
	"lock":             {protocol.CmdLockScreen, "🔒 Κλείδωμα", false},
	"ulk":              {protocol.CmdUnlockScreen, "🔓 Ξεκλείδωμα", false},
	"unlock":           {protocol.CmdUnlockScreen, "🔓 Ξεκλείδωμα", false},
	"mu":               {protocol.CmdMute, "🔇 Σίγαση", false},
	"mute":             {protocol.CmdMute, "🔇 Σίγαση", false},
	"umu":              {protocol.CmdUnmute, "🔊 Κατάργηση σίγασης", false},
	"unmute":           {protocol.CmdUnmute, "🔊 Κατάργηση σίγασης", false},
	"tvon":             {protocol.CmdStartMonitor, "👁 Παρακολούθηση on", false},
	"start-monitoring": {protocol.CmdStartMonitor, "👁 Παρακολούθηση on", false},
	"tvoff":            {protocol.CmdStopMonitor, "👁 Παρακολούθηση off", false},
	"stop-monitoring":  {protocol.CmdStopMonitor, "👁 Παρακολούθηση off", false},
	"sh":               {protocol.CmdRequestShot, "📷 Στιγμιότυπο", false},
	"shot":             {protocol.CmdRequestShot, "📷 Στιγμιότυπο", false},
	"cl":               {protocol.CmdCloseApps, "❌ Κλείσιμο εφαρμογών", false},
	"close":            {protocol.CmdCloseApps, "❌ Κλείσιμο εφαρμογών", false},
	"sd":               {protocol.CmdShutdown, "⚡ Τερματισμός", false},
	"shutdown":         {protocol.CmdShutdown, "⚡ Τερματισμός", false},
	"bl":               {protocol.CmdBlockChat, "🚫 Αποκλεισμός chat", false},
	"block":            {protocol.CmdBlockChat, "🚫 Αποκλεισμός chat", false},
	"ubl":              {protocol.CmdUnblockChat, "✅ Αποδέσμευση chat", false},
	"unblock":          {protocol.CmdUnblockChat, "✅ Αποδέσμευση chat", false},
	"fc":               {protocol.CmdFocusApp, "🔍 Εστίαση", true},
	"focus":            {protocol.CmdFocusApp, "🔍 Εστίαση", true},
	"ln":               {protocol.CmdLaunchApp, "🚀 Εκκίνηση", true},
	"launch":           {protocol.CmdLaunchApp, "🚀 Εκκίνηση", true},
}

// pendingScheduledCmd holds a parsed tool command that needs --y / --n
// confirmation before being added to the scheduler. Today we use it for one
// thing only: an HH:MM in the past, which rolls forward to tomorrow.
type pendingScheduledCmd struct {
	rawForRetry string // original raw text, used for the system message
	job         core.ScheduledJob
}

// schedulableActions is the set of action codes (short or full form) that
// accept a `| <when>` clause. Focus and shot are deliberately excluded — a
// scheduled focus is meaningless, a scheduled one-shot screenshot is too
// transient to be useful. Casting is excluded because it binds to the
// teacher's screen and a scheduled future cast is fragile to wire.
var schedulableActions = map[string]bool{
	"lk": true, "lock": true,
	"ulk": true, "unlock": true,
	"mu": true, "mute": true,
	"umu": true, "unmute": true,
	"sd": true, "shutdown": true,
	"cl": true, "close": true,
	"bl": true, "block": true,
	"ubl": true, "unblock": true,
	"ln": true, "launch": true,
	"tvon": true, "start-monitoring": true,
	"tvoff": true, "stop-monitoring": true,
}

// defaultTimeForAction returns the value Tab-completion inserts after a bare
// `|`. Lock defaults to ":15" (typical "lock the class for 15 minutes" use);
// everything else defaults to ":3".
func defaultTimeForAction(action string) string {
	switch action {
	case "lk", "lock":
		return ":15"
	}
	return ":3"
}

// splitToolTimeClause splits a tool-command raw string on the first ` | `
// (space-pipe-space). Returns (body, timeRaw, hasClause). Trims whitespace
// off both sides.
func splitToolTimeClause(raw string) (body, timeRaw string, ok bool) {
	idx := strings.Index(raw, " | ")
	if idx < 0 {
		// Also accept a trailing "|" without surrounding spaces if the user
		// typed " |X" or "|" — let the time parser surface the error.
		if i := strings.LastIndex(raw, "|"); i >= 0 {
			body = strings.TrimSpace(raw[:i])
			timeRaw = strings.TrimSpace(raw[i+1:])
			ok = true
			return
		}
		return raw, "", false
	}
	return strings.TrimSpace(raw[:idx]), strings.TrimSpace(raw[idx+3:]), true
}

// parsedToolBody is the action+target+param decomposition shared by the
// immediate and scheduled command paths.
type parsedToolBody struct {
	action      string // raw action code as typed (kept for default-time lookup)
	entry       toolActionEntry
	targetID    string
	targetLabel string
	param       string
}

// parseToolBody runs the same action/target/param parse used by the immediate
// path, but returns an error+message instead of pushing system messages. The
// caller decides whether to push a warning or surface the error in a
// scheduling context.
func (m *Model) parseToolBody(body string) (parsedToolBody, string) {
	parts := strings.Fields(body)
	if len(parts) == 0 {
		return parsedToolBody{}, "κενή εντολή"
	}
	actionCode := parts[0]
	rest := parts[1:]
	entry, ok := toolActions[actionCode]
	if !ok {
		return parsedToolBody{}, fmt.Sprintf("Άγνωστη εντολή: %s", actionCode)
	}
	out := parsedToolBody{action: actionCode, entry: entry, targetLabel: "όλους"}
	i := 0
	for i < len(rest) {
		if strings.HasPrefix(rest[i], ">") {
			num := 0
			if _, err := fmt.Sscanf(strings.TrimPrefix(rest[i], ">"), "%d", &num); err != nil || num < 1 || num > len(m.students) {
				return parsedToolBody{}, fmt.Sprintf("Άκυρος στόχος: %s (1–%d)", rest[i], len(m.students))
			}
			st := m.students[num-1]
			out.targetID = st.id
			out.targetLabel = fmt.Sprintf("%s (>%d)", st.name, num)
			i++
		} else {
			out.param = strings.Join(rest[i:], " ")
			break
		}
	}
	if entry.param && out.param == "" {
		return parsedToolBody{}, fmt.Sprintf("Η εντολή %s απαιτεί παράμετρο", actionCode)
	}
	return out, ""
}

// handleScheduledToolCmd is the scheduling counterpart of handleToolCmd. The
// `| <when>` clause has already been split off; body is everything to the
// left and timeRaw is what was right of the pipe.
func (m *Model) handleScheduledToolCmd(body, timeRaw string) tea.Cmd {
	parsed, errMsg := m.parseToolBody(body)
	if errMsg != "" {
		m.pushSysMsg("⚠ " + errMsg)
		return nil
	}
	if !schedulableActions[parsed.action] {
		m.pushSysMsg(fmt.Sprintf("⚠ Η εντολή %s δεν προγραμματίζεται (focus, shot, casting)", parsed.action))
		return nil
	}
	when, err := core.ParseWhen(timeRaw, time.Now())
	if err != nil {
		m.pushSysMsg("⚠ " + err.Error())
		return nil
	}

	// Lock-with-duration: send the lock now and queue the unlock at +N min.
	// We model "now" as a direct SendCommand so the existing immediate-path
	// rendering applies; only the unlock is queued.
	isLockDuration := (parsed.action == "lk" || parsed.action == "lock") && when.IsDuration
	if isLockDuration {
		if err := m.app.SendCommand(parsed.entry.cmd, parsed.param, parsed.targetID); err != nil {
			m.pushSysMsg("⚠ " + err.Error())
			return nil
		}
		unlockEntry := toolActions["unlock"]
		unlockJob := core.ScheduledJob{
			Action:     unlockEntry.cmd,
			Param:      "",
			TargetID:   parsed.targetID,
			TargetText: parsed.targetLabel,
			Label:      unlockEntry.label,
			When:       when.Absolute,
		}
		id := m.app.Sched.Add(unlockJob)
		m.pushSysMsg(fmt.Sprintf("🔒 Κλείδωμα → %s · 🔓 αυτόματο ξεκλείδωμα σε %dλ [%s]",
			parsed.targetLabel, when.DurationMin, id))
		return nil
	}

	job := core.ScheduledJob{
		Action:     parsed.entry.cmd,
		Param:      parsed.param,
		TargetID:   parsed.targetID,
		TargetText: parsed.targetLabel,
		Label:      parsed.entry.label,
		When:       when.Absolute,
	}

	if when.RollOver {
		// HH:MM already past today — require explicit Y/N. Stash the job and
		// surface a single-line confirmation prompt.
		m.pendingSchedule = &pendingScheduledCmd{rawForRetry: body + " | " + timeRaw, job: job}
		m.pushSysMsg(fmt.Sprintf("⚠ Η ώρα %s έχει περάσει σήμερα. Προγραμματισμός για αύριο %s; (--y / --n)",
			timeRaw, when.Absolute.Format("Mon 15:04")))
		return nil
	}

	id := m.app.Sched.Add(job)
	m.pushSysMsg(fmt.Sprintf("⏰ %s → %s @ %s (σε %s) [%s]",
		parsed.entry.label, parsed.targetLabel,
		when.Absolute.Format("15:04"),
		humanDuration(time.Until(when.Absolute)),
		id))
	return nil
}

// humanDuration renders a Duration as Greek "Xώ Yλ" / "Yλ" / "Zδ".
func humanDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	if d < time.Minute {
		return fmt.Sprintf("%dδ", int(d.Seconds()))
	}
	h := int(d / time.Hour)
	m := int((d % time.Hour) / time.Minute)
	if h > 0 {
		return fmt.Sprintf("%dώ %dλ", h, m)
	}
	return fmt.Sprintf("%dλ", m)
}

// handleToolCmd parses: <action> [-a | -s <num>] [param]
// Returns a tea.Cmd: nil for normal commands, tea.ClearScreen for actions
// that spawn a subprocess (start-monitoring/tvon/stop-monitoring/tvoff). The
// spawn briefly disrupts the Windows console state and bubbletea's incremental
// renderer leaves stale rows on screen — ClearScreen forces a full repaint.
func (m *Model) handleToolCmd(raw string) tea.Cmd {
	// Time clause: `… | HH:MM` or `… | :NN`. Strip it first; the body is
	// parsed by the existing action/target/param logic and then scheduled.
	body, timeRaw, hasTime := splitToolTimeClause(raw)
	if hasTime {
		return m.handleScheduledToolCmd(body, timeRaw)
	}
	parts := strings.Fields(raw)
	if len(parts) == 0 {
		m.pushSysMsg("Χρήση: --t <action> [-a | -s <num>] [param]  —  --help για λίστα")
		return nil
	}

	actionCode := parts[0]
	parts = parts[1:]

	// Casting uses a dedicated TCP server — handle before the generic action map.
	switch actionCode {
	case "start-casting", "cast", "caston":
		if m.state.Casting {
			m.pushSysMsg("⚠ Casting ήδη ενεργό — χρήση ^S ή --t stop-casting για διακοπή")
			return nil
		}
		if _, err := m.app.StartCasting(); err != nil {
			m.pushSysMsg("⚠ Casting: " + err.Error())
		} else {
			m.pushSysMsg("📡 Casting on")
		}
		return nil
	case "stop-casting", "castoff":
		m.app.StopCasting()
		m.pushSysMsg("📡 Casting off")
		return nil
	}

	entry, ok := toolActions[actionCode]
	if !ok {
		m.pushSysMsg(fmt.Sprintf("Άγνωστη εντολή: %s — --help για λίστα", actionCode))
		return nil
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
				return nil
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
		return nil
	}

	// Warn if targeting an offline student
	if targetID != "" {
		for _, st := range m.students {
			if st.id == targetID && !st.online {
				m.pushSysMsg(fmt.Sprintf("⚠ %s δεν είναι συνδεδεμένος", targetLabel))
				return nil
			}
		}
	}

	if err := m.app.SendCommand(entry.cmd, param, targetID); err != nil {
		m.pushSysMsg("⚠ " + err.Error())
		return nil
	}
	m.pushSysMsg(fmt.Sprintf("%s → %s", entry.label, targetLabel))

	// tvon/tvoff spawn (or kill) monitoring.exe on the teacher side. The
	// child-process churn briefly disrupts the Windows console mode and
	// bubbletea's incremental renderer leaves stale rows on the terminal
	// (the user sees their --t tvon / --t tvoff text "stuck" above the input
	// box even though the textarea value is empty). A delayed ClearScreen
	// forces a full repaint after the spawn settles.
	if entry.cmd == protocol.CmdStartMonitor || entry.cmd == protocol.CmdStopMonitor {
		return tea.Tick(400*time.Millisecond, func(time.Time) tea.Msg {
			return tea.ClearScreen()
		})
	}
	return nil
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
	if m.app.Role == core.RoleStudent {
		return helpLinesStudent()
	}
	return helpLinesTeacher()
}

func helpLinesTeacher() []string {
	return []string{
		"  ΓΕΝΙΚΕΣ ΣΥΝΤΟΜΕΥΣΕΙΣ",
		"  Enter           αποστολή μηνύματος",
		"  ^H              βοήθεια (αυτό το παράθυρο)",
		"  ^A              browser αρχείων (Esc κλείσιμο)",
		"  ^D              λήψη αρχείου  (@X.Y ή @fN στο πεδίο)",
		"  ↑/↓             ιστορικό εντολών (bash-style) · scroll στο help",
		"  Space / PgDn    επόμενη σελίδα στο help",
		"  PgUp            προηγούμενη σελίδα στο help",
		"  Tab             αυτόματη συμπλήρωση εντολής",
		"  ^X / Esc        κλείσιμο overlay",
		"  ^C              έξοδος",
		"",
		"  ΣΥΝΤΟΜΕΥΣΕΙΣ ΔΑΣΚΑΛΟΥ",
		"  ^U  λίστα μαθητών   ^T  εργαλεία   ^G  μαύρη/λευκή",
		"  ^L  κλειδ./ξεκλ.    ^Z  σίγαση     ^W  παρακολούθηση",
		"  ^S  casting         ^F  εστίαση    ^N  Path Notes",
		"  ^P  καρφίτσωμα      ^B  μαύρη @X.Y ^O  pass @X.Y",
		"  ^X  προγραμματισμένες εντολές    ^A→^O αρχείο + auto-open",
		"",
		"  ΑΝΑΦΟΡΑ ΜΗΝΥΜΑΤΩΝ",
		"  @X.Y  X=αποστολέας (0=εγώ, 1=πρώτος άλλος…)",
		"        Y=θέση στο παράθυρο 1-10",
		"  @pN   Nth καρφιτσωμένο μήνυμα (p1, p2…)",
		"  @fN   Nth καρφιτσωμένο ΑΡΧΕΙΟ (f1, f2…)",
		"",
		"  ────────────────────────────────────────────",
		"  ΕΝΤΟΛΕΣ ΕΡΓΑΛΕΙΩΝ   --t | --tool <action> [>N] [param]",
		"  ────────────────────────────────────────────",
		"  >N = σε μαθητή #N (από τη λίστα ^U) · χωρίς >N = όλοι",
		"",
		"  --t lk     / lock                  🔒 κλείδωμα οθόνης",
		"  --t ulk    / unlock                🔓 ξεκλείδωμα",
		"      π.χ.  --t lk          (όλοι)",
		"            --t lk >3       (μόνο μαθητή #3)",
		"  --t mu     / mute                  🔇 σίγαση",
		"  --t umu    / unmute                🔊 κατάργηση σίγασης",
		"  --t sh     / shot       [>N]       📷 στιγμιότυπο",
		"      π.χ.  --t sh         (στιγμιότυπο όλων)",
		"            --t sh >2      (μόνο μαθητή #2)",
		"  --t tvon   / start-monitoring      👁 παρακολούθηση on",
		"  --t tvoff  / stop-monitoring       👁 παρακολούθηση off",
		"  --t cast   / start-casting         📡 casting on (caston επίσης)",
		"  --t castoff/ stop-casting          📡 casting off",
		"  --t bl     / block                 🚫 αποκλεισμός chat",
		"  --t ubl    / unblock               ✅ αποδέσμευση chat",
		"  --t cl     / close                 ❌ κλείσιμο εφαρμογών",
		"  --t sd     / shutdown              ⚡ τερματισμός PC",
		"  --t fc     / focus    <τίτλος>     🔍 εστίαση παραθύρου εφαρμογής",
		"      π.χ.  --t fc Notepad           (φέρνει Notepad μπροστά σε όλους)",
		"            --t fc >2 Chrome         (στον μαθητή #2)",
		"  --t ln     / launch   <exe>        🚀 εκκίνηση εφαρμογής",
		"      π.χ.  --t ln notepad.exe       (σε όλους)",
		"            --t ln >3 calc.exe       (στον μαθητή #3)",
		"",
		"  ⏰ ΧΡΟΝΟΠΡΟΓΡΑΜΜΑΤΙΣΜΟΣ   (πρόσθεσε  | HH:MM  ή  | :λεπτά)",
		"  Tab μετά το `|` συμπληρώνει αυτόματα `:3` (`:15` για lock).",
		"  Δεν προγραμματίζονται: focus, shot, casting.",
		"      π.χ.  --t shutdown >* | 13:15   (τερματισμός όλων στις 13:15)",
		"            --t sd | :30              (τερματισμός σε 30 λεπτά)",
		"            --t lock >* | :15         (lock για 15 λεπτά — αυτο-unlock)",
		"            --t lock >* | 09:30       (lock στις 09:30, μένει κλειδωμένο)",
		"            --t ln >2 notepad.exe | :5  (εκκίνηση σε 5 λεπτά)",
		"  Αν η HH:MM έχει περάσει σήμερα, ζητείται επιβεβαίωση (--y / --n)",
		"  για να προγραμματιστεί την επόμενη μέρα.",
		"",
		"  ^X                                   panel με όλες τις προγραμματισμένες",
		"      (μέσα στο panel: ↑↓ επιλογή · d ακύρωση · Esc κλείσιμο)",
		"  --sched                              λίστα ως system messages",
		"  --sched cancel <id>                  ακύρωση (π.χ. --sched cancel S1)",
		"",
		"  ────────────────────────────────────────────",
		"  ΔΙΑΧΕΙΡΙΣΗ ΚΑΙ ΑΠΟΣΤΟΛΗ ΜΗΝΥΜΑΤΩΝ",
		"  ────────────────────────────────────────────",
		"  Παράμετροι: @X.Y = μήνυμα · @pN = pinned · @fN = pinned αρχείο",
		"",
		"  --pin                                  καρφίτσωμα τελευταίου δικού σου ^P",
		"  --pin     @X.Y                         καρφίτσωμα συγκεκριμένου",
		"  κείμενο --pin                          αποστολή + καρφίτσωμα",
		"      π.χ.  --pin @0.1            (καρφίτσωμα του 1ου δικού μου)",
		"            --pin @1.3            (καρφίτσωμα 3ου από τον 1ο μαθητή)",
		"            Διαβάστε σελ.4 --pin  (στέλνει + καρφιτσώνει)",
		"",
		"  --upin / --unpin                       αποκαρφίτσωμα τελευταίου",
		"  --upin / --unpin   @X.Y | @pN          αποκαρφίτσωμα συγκεκριμένου",
		"      π.χ.  --upin @p1            (αποκαρφίτσωμα 1ου pinned)",
		"            --upin @0.2           (αποκαρφίτσωμα 2ου δικού μου)",
		"",
		"  --cp / --copy      @X.Y                αντιγραφή στο clipboard",
		"      π.χ.  --cp @1.2            (αντιγραφή 2ου του μαθητή #1)",
		"            --cp @p1             (αντιγραφή 1ου pinned)",
		"",
		"  --op / --open      @X.Y | @fN          άνοιγμα URL/αρχείου             ^O",
		"      π.χ.  --op @1.3            (άνοιγμα URL/αρχείου του μαθητή #1)",
		"            --op @f1             (άνοιγμα 1ου pinned αρχείου)",
		"",
		"  --dl / --download  @X.Y | @fN          λήψη αρχείου                    ^D",
		"  --dl / --download  *                   λήψη ΟΛΩΝ σε zip (Downloads/)",
		"      π.χ.  --dl @1.2            (λήψη του 2ου από τον μαθητή #1)",
		"            --dl @f2             (λήψη 2ου pinned αρχείου)",
		"            --dl *               (όλα μαζί σε zip)",
		"",
		"  --a / --attach                         file picker (ίδιο με ^A)",
		"  --a / --attach     <path>              επισύναψη αρχείου",
		"      π.χ.  --a C:\\notes\\paper.pdf",
		"",
		"  --rm / --del | --delete    @X.Y        διαγραφή μηνύματος",
		"      π.χ.  --rm @2.1            (διαγραφή 1ου μηνύματος του μαθητή #2)",
		"",
		"  --clr / --clear                        διαγραφή όλου του chat",
		"  --clr / --clear    @s                  καθαρισμός μηνυμάτων συστήματος",
		"      π.χ.  --clr @s             (κρατάει chat, σβήνει τα γκρι system)",
		"",
		"  --ps / --pass      @X.Y                pass αναφοράς                   ^O",
		"      π.χ.  --ps @3.2            (pass της αναφοράς του μαθητή #3)",
		"",
		"  --rep / --report   @X.Y                αναφορά μηνύματος",
		"      π.χ.  --rep @1.4            (αναφορά 4ου μηνύματος του μαθητή #1)",
		"",
		"  PUSH-OPEN  (silent, χωρίς μήνυμα στο chat)",
		"  --op / --open  <url>    >N | >*        άνοιγμα URL σε μαθητή/όλους",
		"  --op / --open  @X.Y     >N             push αρχείου/URL → μαθητής",
		"  <url> --op     [>N]                    shorthand: push URL",
		"  --op / --open  this  >* | >N           (με 📎 staged) αποστολή + auto-open",
		"      π.χ.  --op https://wikipedia.org >*    (URL σε όλους)",
		"            --op https://example.com >2     (μόνο μαθητής #2)",
		"            https://yt.com --op             (shorthand σε όλους)",
		"            https://yt.com --op >3          (shorthand μαθητή #3)",
		"            --op @f1 >2                     (push pinned αρχείου)",
		"            --op this >*                    (📎 staged σε όλους + auto-open)",
		"",
		"  PATH NOTES  (^N για το παράθυρο)",
		"  --pa / --path  [open]                  άνοιγμα Path Notes",
		"  --pa / --path  save    <url|path>      αποθήκευση",
		"  --pa / --path  delete  <ακριβής>       αφαίρεση εγγραφής",
		"      π.χ.  --pa save https://docs.gov/intro",
		"            --pa save C:\\school\\worksheet.pdf",
		"            --pa delete https://docs.gov/intro",
		"",
		"  ────────────────────────────────────────────",
		"  ΦΙΛΤΡΑΡΙΣΜΑ ΠΕΡΙΕΧΟΜΕΝΟΥ",
		"  ────────────────────────────────────────────",
		"  Παράμετροι: @X.Y = μήνυμα · @BN = blacklist entry · @WN = whitelist entry",
		"",
		"  --blk / --black    @X.Y                μαύρη λίστα + διαγραφή           ^B",
		"  --blk / --black    <λέξεις>            προσθήκη στη μαύρη λίστα",
		"  --blk / --black    @BN                 αφαίρεση @BN από μαύρη λίστα",
		"      π.χ.  --blk @2.1                  (παίρνει λέξεις του μηνύματος, σβήνει)",
		"            --blk βρισιά1 βρισιά2       (προσθήκη πολλαπλών λέξεων)",
		"            --blk @B5                    (αφαίρεση 5ης εγγραφής μαύρης)",
		"",
		"  --wh / --white     <λέξεις>            προσθήκη στη λευκή λίστα",
		"  --wh / --white     @WN                 αφαίρεση @WN από λευκή λίστα",
		"      π.χ.  --wh αξιολόγηση κριτική     (επιτρέπεται παρά τη μαύρη)",
		"            --wh @W3                    (αφαίρεση 3ης εγγραφής λευκής)",
		"",
		"  ^G    άνοιγμα overlay μαύρης/λευκής (βλέπεις τα @BN/@WN ids)",
		"",
		"  ────────────────────────────────────────────",
		"  ΡΥΘΜΙΣΕΙΣ  --set <key> <τιμή>",
		"  ────────────────────────────────────────────",
		"  --set  nickname     <όνομα>            αλλαγή ονόματος",
		"  --set  autostart    on | off           εκκίνηση με Windows",
		"  --set  list import  <αρχείο>           εισαγωγή λιστών",
		"  --set  list export  [αρχείο]           εξαγωγή λιστών (προεπ: Downloads/)",
		"      π.χ.  --set nickname Κυρία Νίκη",
		"            --set autostart on",
		"            --set list import C:\\Users\\Νίκη\\Downloads\\lists.json",
		"            --set list export                    (στο Downloads/)",
		"",
		"  ────────────────────────────────────────────",
		"  TAB COMPLETION",
		"  ────────────────────────────────────────────",
		"  --h help (πάντα --h)   ·   όλα τα άλλα 2 γράμματα σε στυλ Linux",
		"  Tab: h→--help · cp/c→--copy · op/o→--open · dl/d→--download",
		"       a→--attach · p→--pin · u→--upin · r→--report · s→--set",
		"       t→--t   ·   --t <Tab> κύκλος ενεργειών",
		"  --set <Tab> κύκλος ρυθμίσεων: nickname / autostart / list",
	}
}

func helpLinesStudent() []string {
	return []string{
		"  ΓΕΝΙΚΕΣ ΣΥΝΤΟΜΕΥΣΕΙΣ",
		"  Enter           αποστολή μηνύματος",
		"  ^H              βοήθεια (αυτό το παράθυρο)",
		"  ^A              browser αρχείων (Esc κλείσιμο)",
		"  ^D              λήψη αρχείου  (@X.Y ή @fN στο πεδίο)",
		"  ↑/↓             ιστορικό εντολών (bash-style) · scroll στο help",
		"  Space / PgDn    επόμενη σελίδα στο help",
		"  PgUp            προηγούμενη σελίδα στο help",
		"  Tab             αυτόματη συμπλήρωση εντολής",
		"  ^X / Esc        κλείσιμο overlay",
		"  ^C              έξοδος",
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
		"  ────────────────────────────────────────────",
		"  ΔΙΑΧΕΙΡΙΣΗ ΚΑΙ ΑΠΟΣΤΟΛΗ ΜΗΝΥΜΑΤΩΝ",
		"  ────────────────────────────────────────────",
		"  Παράμετροι: @X.Y = μήνυμα · @pN = pinned · @fN = pinned αρχείο",
		"",
		"  --rep / --report   @X.Y                αναφορά μηνύματος               ^R",
		"      π.χ.  --rep @0.3            (αναφορά 3ου δικού μου)",
		"            --rep @1.2            (αναφορά 2ου του δασκάλου)",
		"",
		"  --cp / --copy      @X.Y                αντιγραφή στο clipboard",
		"      π.χ.  --cp @1.4             (αντιγραφή 4ου του δασκάλου)",
		"            --cp @p1              (αντιγραφή 1ου pinned)",
		"",
		"  --op / --open      @X.Y | @fN          άνοιγμα URL/αρχείου             ^O",
		"      π.χ.  --op @1.2             (άνοιγμα URL/αρχείου του δασκάλου)",
		"            --op @f1              (άνοιγμα 1ου pinned αρχείου)",
		"",
		"  --dl / --download  @X.Y | @fN          λήψη αρχείου                    ^D",
		"      π.χ.  --dl @1.3             (λήψη αρχείου του δασκάλου)",
		"            --dl @f2              (λήψη 2ου pinned αρχείου)",
		"",
		"  --a / --attach                         file picker (ίδιο με ^A)",
		"  --a / --attach     <path>              επισύναψη αρχείου",
		"      π.χ.  --a C:\\school\\εργασία.pdf",
		"",
		"  --clr / --clear    @s                  καθαρισμός μηνυμάτων συστήματος",
		"      π.χ.  --clr @s              (κρατάει chat, σβήνει τα γκρι system)",
		"",
		"  ────────────────────────────────────────────",
		"  ΡΥΘΜΙΣΕΙΣ  --set <key> <τιμή>",
		"  ────────────────────────────────────────────",
		"  --set  nickname     <όνομα>            αλλαγή ονόματος",
		"  --set  autostart    on | off           εκκίνηση με Windows",
		"  --set  list import  <αρχείο>           εισαγωγή λιστών",
		"  --set  list export  [αρχείο]           εξαγωγή λιστών (προεπ: Downloads/)",
		"  --cast                                 επαναφορά παραθύρου casting",
		"      π.χ.  --set nickname Νίκη",
		"            --set autostart on",
		"            --set list import C:\\Users\\Νίκη\\Downloads\\lists.json",
		"            --set list export             (στο Downloads/)",
		"",
		"  ────────────────────────────────────────────",
		"  TAB COMPLETION",
		"  ────────────────────────────────────────────",
		"  --h help (πάντα --h)   ·   όλα τα άλλα 2 γράμματα σε στυλ Linux",
		"  Tab: h→--help · c→--copy · o→--open · d→--download",
		"       a→--attach · r→--report · s→--set",
	}
}

// aboutLines is the body of the --about / --ver / --version overlay. The
// running build string and live log path come from buildinfo / devlog so the
// overlay always reflects the actual binary; the rest of the page is read
// from about.md beside the .exe (so it can be edited post-install).
func (m *Model) aboutLines() []string {
	var lines []string
	lines = append(lines, "ClassSend 2  •  "+buildinfo.String())
	lines = append(lines, "Ρόλος: "+string(m.app.Role)+"  •  Logs: "+devlog.Path())
	lines = append(lines, "")
	if path, content, ok := readAboutFile(); ok {
		lines = append(lines, "── about.md ("+path+") ──")
		for _, line := range strings.Split(strings.TrimRight(content, "\r\n"), "\n") {
			lines = append(lines, strings.TrimRight(line, "\r"))
		}
	} else {
		lines = append(lines, "(about.md δεν βρέθηκε δίπλα στο .exe)")
	}
	return lines
}

func (m *Model) overlayAbout(base string) string {
	const overlayW = 60
	const maxVisible = 22

	lines := m.aboutLines()
	total := len(lines)

	start := m.aboutScroll
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
	out = append(out, styleTitle.Render("  ClassSend 2.0 — Σχετικά"))
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

// truncateNote shortens a value to fit width w with the right tail visible:
//
//   - URLs (http://, https://, www., or "host.tld..." with no slash before TLD)
//     keep their HEAD so the protocol + domain stay readable. The truncation
//     mark is appended at the end.
//   - Everything else is treated as a file path: the TAIL is kept so the
//     filename / app name at the end stays readable. The truncation mark goes
//     at the front.
//
// w must be >= 2 to make room for the ellipsis itself; callers pass overlay
// width minus padding.
func truncateNote(s string, w int) string {
	if w < 2 {
		w = 2
	}
	if len(s) <= w {
		return s
	}
	low := strings.ToLower(s)
	isURL := strings.HasPrefix(low, "http://") ||
		strings.HasPrefix(low, "https://") ||
		strings.HasPrefix(low, "www.") ||
		strings.HasPrefix(low, "ftp://")
	if isURL {
		return s[:w-1] + "…"
	}
	return "…" + s[len(s)-(w-1):]
}

// overlayFavorites is the ^N panel: a scrollable list of saved push-open
// targets (URLs and attached file paths). Enter places the highlighted entry
// into the input field as `--op "<value>" >` (incomplete on purpose — the
// teacher fills in `*` or a student number); 'd'/Delete removes the entry.
func (m *Model) overlayFavorites(base string) string {
	const overlayW = 70
	const maxVisible = 18

	favs := m.app.FavoritesSnapshot()

	var out []string
	out = append(out, styleTitle.Render("  📌 Path (N)otes — αποθηκευμένα URL/αρχεία"))
	out = append(out, strings.Repeat("─", overlayW))

	if len(favs) == 0 {
		out = append(out, styleHint.Render("  (κενό — auto-add όταν στέλνεις αρχείο ή --op URL · ή --path save <τιμή>)"))
	} else {
		// Clamp cursor in case entries were just deleted
		if m.favoritesCursor >= len(favs) {
			m.favoritesCursor = len(favs) - 1
		}
		if m.favoritesCursor < 0 {
			m.favoritesCursor = 0
		}
		start := m.favoritesScroll
		if start > len(favs)-maxVisible {
			start = len(favs) - maxVisible
		}
		if start < 0 {
			start = 0
		}
		end := start + maxVisible
		if end > len(favs) {
			end = len(favs)
		}
		for i := start; i < end; i++ {
			f := favs[i]
			marker := "  "
			if f.Pinned {
				marker = "★ "
			}
			label := truncateNote(f.Value, overlayW-4)
			line := marker + label
			if i == m.favoritesCursor {
				line = styleSelected.Width(overlayW).Render(line)
			} else {
				line = lipgloss.NewStyle().Width(overlayW).Foreground(colText).Render(line)
			}
			out = append(out, line)
		}
	}

	out = append(out, strings.Repeat("─", overlayW))
	hint := "[↑↓] επιλογή  [Enter] τοποθέτηση  [s] ★ pin  [d] διαγραφή  [Esc] κλείσιμο"
	out = append(out, styleHint.Render(hint))

	panel := styleBorder.Padding(1, 2).Render(strings.Join(out, "\n"))
	return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center,
		panel,
		lipgloss.WithWhitespaceChars(" "),
		lipgloss.WithWhitespaceForeground(colBg),
	)
}

// isPathCmd returns true if text is a --path / --pa form. Bare --pa also opens
// the overlay; --path with a sub-verb routes to handlePathCmd.
func isPathCmd(text string) bool {
	if text == "--pa" || text == "--path" {
		return true
	}
	return strings.HasPrefix(text, "--pa ") || strings.HasPrefix(text, "--path ")
}

// handlePathCmd dispatches the --path family. Recognised forms:
//
//	--pa  /  --path  /  --path open      open the Path Notes overlay (^N)
//	--path save   <value>                save URL or absolute path; floats to top
//	--path delete <value>                remove an exact match
//	--path remove <value>                alias for delete
//
// Manual saves bump the entry's AddedAt to now, so they sort to the top above
// any auto-tracked entries — the persistence layer already does move-to-front
// for duplicates and survives across sessions via favorites.json.
func (m *Model) handlePathCmd(text string) tea.Cmd {
	body := strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(text, "--path"), "--pa"))
	if body == "" || body == "open" {
		m.favoritesOpen = true
		m.favoritesCursor = 0
		m.favoritesScroll = 0
		return nil
	}
	parts := strings.SplitN(body, " ", 2)
	verb := strings.ToLower(parts[0])
	val := ""
	if len(parts) == 2 {
		val = strings.TrimSpace(parts[1])
		val = strings.Trim(val, `"`)
	}
	switch verb {
	case "save", "add":
		if val == "" {
			m.pushSysMsg("⚠ Χρήση: --path save <url-ή-διαδρομή>")
			return nil
		}
		m.app.AddFavorite(val)
		m.pushSysMsg("📌 Αποθηκεύτηκε στο Path Notes: " + val)
	case "delete", "remove", "del", "rm":
		if val == "" {
			m.pushSysMsg("⚠ Χρήση: --path delete <ακριβής-τιμή>")
			return nil
		}
		if m.app.RemoveFavorite(val) {
			m.pushSysMsg("🗑 Αφαιρέθηκε από Path Notes: " + val)
		} else {
			m.pushSysMsg("⚠ Δεν βρέθηκε στο Path Notes: " + val)
		}
	default:
		m.pushSysMsg("⚠ Άγνωστη εντολή: --path " + verb + "  (open/save/delete)")
	}
	return nil
}

// favoritesPlaceSelected inserts the highlighted favorite into the input as a
// teacher push-open template. The trailing ">" is intentionally left bare so
// the teacher types ">*" or ">3" before pressing Enter — explicit destination
// avoids accidental broadcasts.
func (m *Model) favoritesPlaceSelected() {
	favs := m.app.FavoritesSnapshot()
	if len(favs) == 0 || m.favoritesCursor < 0 || m.favoritesCursor >= len(favs) {
		m.favoritesOpen = false
		return
	}
	val := favs[m.favoritesCursor].Value
	// Quote only when the value contains whitespace. Unconditional quoting
	// broke bare URLs: `cmd /c start "" "google.gr"` made Windows look up a
	// protocol literally named `"google.gr"` instead of the URL.
	var insert string
	if strings.ContainsAny(val, " \t") {
		insert = fmt.Sprintf(`--op "%s" >`, val)
	} else {
		insert = fmt.Sprintf(`--op %s >`, val)
	}
	m.input.SetValue(insert)
	m.favoritesOpen = false
	m.focusInput = true
}

// overlayScheduled renders the ^X panel: every pending scheduled command,
// newest-first by fire time, with countdown. 'd' / Delete cancels the
// highlighted row; ↑↓ navigate; Esc closes.
func (m *Model) overlayScheduled(base string) string {
	const overlayW = 78
	const maxVisible = 14

	jobs := m.app.Sched.List()

	var out []string
	out = append(out, styleTitle.Render("  ⏰ (X)ρονοπρογραμματισμένες εντολές"))
	out = append(out, strings.Repeat("─", overlayW))

	if len(jobs) == 0 {
		out = append(out, styleHint.Render("  (καμία προγραμματισμένη εντολή — προσθήκη με --t … | HH:MM ή | :λεπτά)"))
	} else {
		// Clamp cursor in case the list shrank.
		if m.schedCursor >= len(jobs) {
			m.schedCursor = len(jobs) - 1
		}
		if m.schedCursor < 0 {
			m.schedCursor = 0
		}
		start := m.schedScroll
		if start > len(jobs)-maxVisible {
			start = len(jobs) - maxVisible
		}
		if start < 0 {
			start = 0
		}
		end := start + maxVisible
		if end > len(jobs) {
			end = len(jobs)
		}
		for i := start; i < end; i++ {
			j := jobs[i]
			// Format: [S1]  🔒 Κλείδωμα → όλους · @ Wed 13:15 · σε 1ώ 22λ
			line := fmt.Sprintf("  [%s]  %s → %s  ·  @ %s  ·  σε %s",
				j.ID, j.Label, j.TargetText,
				j.When.Format("Mon 15:04"),
				humanDuration(time.Until(j.When)))
			if len(line) > overlayW-2 {
				line = line[:overlayW-2] + "…"
			}
			if i == m.schedCursor {
				line = styleSelected.Width(overlayW).Render(line)
			} else {
				line = lipgloss.NewStyle().Width(overlayW).Foreground(colText).Render(line)
			}
			out = append(out, line)
		}
	}

	out = append(out, strings.Repeat("─", overlayW))
	hint := "[↑↓] επιλογή  [d] ακύρωση  [Esc] κλείσιμο"
	out = append(out, styleHint.Render(hint))

	panel := styleBorder.Padding(1, 2).Render(strings.Join(out, "\n"))
	return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center,
		panel,
		lipgloss.WithWhitespaceChars(" "),
		lipgloss.WithWhitespaceForeground(colBg),
	)
}

// schedCancelSelected cancels the highlighted scheduled job and pushes a
// confirmation message. Keeps the cursor in bounds afterwards.
func (m *Model) schedCancelSelected() {
	jobs := m.app.Sched.List()
	if len(jobs) == 0 || m.schedCursor < 0 || m.schedCursor >= len(jobs) {
		return
	}
	j := jobs[m.schedCursor]
	if cancelled, ok := m.app.Sched.Cancel(j.ID); ok {
		m.pushSysMsg(fmt.Sprintf("🗑 Ακυρώθηκε [%s] %s → %s @ %s",
			cancelled.ID, cancelled.Label, cancelled.TargetText,
			cancelled.When.Format("Mon 15:04")))
	}
	remaining := len(jobs) - 1
	if m.schedCursor >= remaining {
		m.schedCursor = remaining - 1
	}
	if m.schedCursor < 0 {
		m.schedCursor = 0
	}
}

// favoritesTogglePinSelected flips the Pinned flag on the highlighted entry.
// Pinned entries float above non-pinned and survive the 50-entry cap.
func (m *Model) favoritesTogglePinSelected() {
	favs := m.app.FavoritesSnapshot()
	if len(favs) == 0 || m.favoritesCursor < 0 || m.favoritesCursor >= len(favs) {
		return
	}
	val := favs[m.favoritesCursor].Value
	pinned, ok := m.app.ToggleFavoritePinned(val)
	if !ok {
		return
	}
	if pinned {
		m.pushSysMsg("★ Καρφιτσώθηκε στο Path Notes: " + val)
		// Pinned entries sort to top — jump the cursor up so the user can see
		// where it landed.
		m.favoritesCursor = 0
		m.favoritesScroll = 0
	} else {
		m.pushSysMsg("☆ Ξεκαρφιτσώθηκε από Path Notes: " + val)
	}
}

// favoritesDeleteSelected removes the highlighted entry from the persisted list.
func (m *Model) favoritesDeleteSelected() {
	favs := m.app.FavoritesSnapshot()
	if len(favs) == 0 || m.favoritesCursor < 0 || m.favoritesCursor >= len(favs) {
		return
	}
	val := favs[m.favoritesCursor].Value
	if m.app.RemoveFavorite(val) {
		m.pushSysMsg("⭐ Αφαιρέθηκε από αγαπημένα: " + val)
	}
	// Keep cursor in bounds; the list just got shorter.
	remaining := len(favs) - 1
	if m.favoritesCursor >= remaining {
		m.favoritesCursor = remaining - 1
	}
	if m.favoritesCursor < 0 {
		m.favoritesCursor = 0
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
	out = append(out, styleTitle.Render("  ClassSend 2.0 — (H)elp / Βοήθεια"))
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
	if m.aboutOpen {
		view = m.overlayAbout(view)
	}
	if m.favoritesOpen {
		view = m.overlayFavorites(view)
	}
	if m.schedOpen {
		view = m.overlayScheduled(view)
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
			if msg.Blocked {
				prefix = lipgloss.NewStyle().Foreground(lipgloss.Color("#FF4444")).Bold(true).Render("[🚫] ") + prefix
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
	lines = append(lines, styleTitle.Render("⚙ (T)ools — Εργαλεία Δασκάλου"))
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
	lines = append(lines, styleTitle.Render("📎 File (A)ttachment — Επισύναψη Αρχείου"))
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
			m.sortStudents()
			return
		}
	}
	m.students = append(m.students, studentEntry{id: id, name: name, ip: ip, online: online})
	m.sortStudents()
}

// sortStudents re-orders the sidebar so hostnames with numeric suffixes
// (Lab1, Lab02, PC3, …) line up by their number instead of by MAC. Tracks
// the currently-selected student by ID so the highlight follows the row
// across the sort. Same-prefix groups are numeric; cross-prefix groups
// are alphabetical (so Lab* and PC* still cluster).
func (m *Model) sortStudents() {
	var selID string
	if m.selectedSt >= 0 && m.selectedSt < len(m.students) {
		selID = m.students[m.selectedSt].id
	}
	sort.SliceStable(m.students, func(i, j int) bool {
		return hostnameLess(m.students[i].name, m.students[j].name)
	})
	m.selectedSt = 0
	if selID != "" {
		for i, st := range m.students {
			if st.id == selID {
				m.selectedSt = i
				break
			}
		}
	}
}

// hostnameLess compares two display names with awareness of a trailing
// integer. "Lab2" < "Lab10" (numeric), but "Lab*" still groups before "PC*"
// alphabetically. Names with no trailing digits sort lexicographically.
func hostnameLess(a, b string) bool {
	pa, na, hasA := splitHostNum(a)
	pb, nb, hasB := splitHostNum(b)
	if !strings.EqualFold(pa, pb) {
		return strings.ToLower(pa) < strings.ToLower(pb)
	}
	if hasA && hasB {
		return na < nb
	}
	if hasA != hasB {
		// Within the same prefix, numeric entries come before non-numeric:
		// "Lab1, Lab2, Lab" puts unnumbered names at the bottom of the group.
		return hasA
	}
	return strings.ToLower(a) < strings.ToLower(b)
}

// splitHostNum walks back from the end of s collecting trailing digits. Empty
// digit run → no numeric suffix. "Lab07" → ("Lab", 7, true);
// "DESKTOP-RAHDSB6" → ("DESKTOP-RAHDSB", 6, true) — trailing-digit semantics
// are intentional, so a MAC-derived hostname still groups with its prefix.
func splitHostNum(s string) (prefix string, n int, ok bool) {
	end := len(s)
	start := end
	for start > 0 && s[start-1] >= '0' && s[start-1] <= '9' {
		start--
	}
	if start == end {
		return s, 0, false
	}
	v, err := strconv.Atoi(s[start:])
	if err != nil {
		return s, 0, false
	}
	return s[:start], v, true
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

// splitQuoted splits s on whitespace, treating "..."-quoted runs as a single
// token. Quotes are preserved on the returned tokens so callers can strip
// them explicitly (keeps the token-vs-literal distinction visible).
func splitQuoted(s string) []string {
	var out []string
	var cur strings.Builder
	inQuote := false
	flush := func() {
		if cur.Len() > 0 {
			out = append(out, cur.String())
			cur.Reset()
		}
	}
	for _, r := range s {
		switch {
		case r == '"':
			inQuote = !inQuote
			cur.WriteRune(r)
		case !inQuote && (r == ' ' || r == '\t'):
			flush()
		default:
			cur.WriteRune(r)
		}
	}
	flush()
	return out
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
		// Quote-aware split so `--op "C:\Path with spaces\f.pdf" >3` parses
		// with target = `C:\Path with spaces\f.pdf` and dest = `3`. Bare
		// `--op google.gr >*` still works because unquoted tokens split on
		// whitespace as before.
		parts := splitQuoted(form1Tail)
		if len(parts) == 0 {
			return
		}
		for _, p := range parts[1:] {
			if strings.HasPrefix(p, ">") {
				target = strings.Trim(parts[0], `"`)
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
	target = strings.Trim(strings.TrimSpace(text[:idx]), `"`)
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

// parseStagedPushOpen detects the "send-and-open this attachment" syntax used
// while a file is staged. Recognised forms (case-insensitive on the keyword):
//
//	--op this > *      --open this > *      --op > *      --open > *
//	--op this > N      --open this > N      --op > N      --open > N
//
// Returns the destination string ("*" or numeric "N") and ok=true on match.
// On match the caller should treat the staged file as the target and clear
// the message text so the directive doesn't end up as the file caption.
func parseStagedPushOpen(text string) (destStr string, ok bool) {
	s := strings.TrimSpace(text)
	var tail string
	switch {
	case strings.HasPrefix(s, "--op "):
		tail = strings.TrimSpace(strings.TrimPrefix(s, "--op "))
	case strings.HasPrefix(s, "--open "):
		tail = strings.TrimSpace(strings.TrimPrefix(s, "--open "))
	default:
		return
	}
	// Optional "this" keyword right after --op
	if strings.HasPrefix(tail, "this") {
		tail = strings.TrimSpace(strings.TrimPrefix(tail, "this"))
	}
	if !strings.HasPrefix(tail, ">") {
		return
	}
	destStr = strings.TrimSpace(strings.TrimPrefix(tail, ">"))
	if destStr == "" {
		destStr = "*"
	}
	// Reject anything trailing — keeps the syntax tight; if a teacher wants a
	// caption with the push, they can do it as the two-step flow.
	if strings.ContainsAny(destStr, " \t") {
		return "", false
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
	{"--se", "--set"},
	{"--s", "--set"},
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
	{"s", "--set"},
}

// setSubcommands is the ordered list cycled when Tab is pressed after "--set ".
var setSubcommands = []string{"nickname", "autostart", "list"}

// toolNames is the ordered list of --t keywords cycled by Tab. One canonical
// name per action — duplicates (long forms vs short aliases, start-monitoring
// vs tvon, etc.) just bloat the cycle without adding discoverability. Shorts
// still work at runtime; users who know them type them directly.
var toolNames = []string{
	"lock", "unlock", "mute", "unmute", "shot",
	"close", "shutdown", "block", "unblock", "focus", "launch",
	"tvon", "tvoff", "cast", "castoff",
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

	// Time-clause completion: any --t / --tool command ending in `|` or `| `
	// expands to the action's default time (`:15` for lock, `:3` otherwise).
	// Lets the teacher type `--t lock >* |<Tab>` to get a one-keystroke lock.
	if strings.HasPrefix(cur, "--t ") || strings.HasPrefix(cur, "--tool ") {
		trimmedRight := strings.TrimRight(cur, " ")
		if strings.HasSuffix(trimmedRight, "|") {
			// Find the action token (first word after the prefix) to pick
			// the right default. We're tolerant about target/param noise.
			afterPrefix := strings.TrimPrefix(cur, "--t ")
			afterPrefix = strings.TrimPrefix(afterPrefix, "--tool ")
			fields := strings.Fields(afterPrefix)
			action := ""
			if len(fields) > 0 {
				action = fields[0]
			}
			def := defaultTimeForAction(action)
			// Normalize trailing whitespace so the inserted default sits
			// exactly one space after `|`.
			base := strings.TrimRight(trimmedRight, "|")
			base = strings.TrimRight(base, " ")
			m.input.SetValue(base + " | " + def)
			m.input.CursorEnd()
			return
		}
	}

	trimmed := strings.TrimSpace(cur)
	if trimmed == "" {
		return
	}

	// Special case: --set <partial> → cycle through setting names
	for _, pfx := range []string{"--set ", "--set"} {
		if trimmed == "--set" || strings.HasPrefix(trimmed, "--set ") {
			partial := ""
			if strings.HasPrefix(trimmed, "--set ") {
				partial = strings.TrimSpace(trimmed[len("--set "):])
			}
			var matches []string
			for _, s := range setSubcommands {
				if strings.HasPrefix(s, partial) {
					matches = append(matches, "--set "+s)
				}
			}
			if len(matches) > 0 {
				m.tabMatches = matches
				m.tabIdx = 0
				m.input.SetValue(matches[0])
				m.input.CursorEnd()
			}
			_ = pfx
			return
		}
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
	"--rm": true, "--del": true, "--rem": true, "--delete": true,
	"--clr": true, "--clear": true,
	"--t": true, "--tool": true,
	"--send": true,
	"--set":  true,
	"--cast": true,
	"--mon":  true, "--monitor": true,
	"--log": true,
	"--pa":  true, "--path": true,
	"--y": true, "--yes": true,
	"--n": true, "--no": true,
	"--sched": true, "--schedule": true, "--sch": true,
}

// toolActionNames is the canonical set of valid --t actions. Used both for
// Tab cycling and for prefix-validation of partial input ("--t loc" → still a
// prefix of "lock", so blue; "--t locc" → not, so plain).
var toolActionNames = []string{
	"lk", "lock", "ulk", "unlock",
	"mu", "mute", "umu", "unmute",
	"sh", "shot",
	"cl", "close", "sd", "shutdown",
	"bl", "block", "ubl", "unblock",
	"fc", "focus", "ln", "launch",
	"tvon", "tvoff", "start-monitoring", "stop-monitoring",
	"start-casting", "cast", "caston", "stop-casting", "castoff",
}

// looksLikeCommand returns true when the input is currently a syntactically
// valid command — or a valid prefix of one. The first `--xxx` token in the
// text is treated as the command head; tokens before it are free text
// (e.g. "Διαβάστε σελ.4 --pin" is still a command).
//
// For commands with a known sub-grammar (--t, --set) the args are validated
// too: "--t loc" stays blue (prefix of "lock"), "--t locc" turns plain.
func looksLikeCommand(text string) bool {
	tokens := strings.Fields(text)
	for i, tok := range tokens {
		if !strings.HasPrefix(tok, "--") {
			continue
		}
		return validateCmdHead(tokens[i:])
	}
	return false
}

// validateCmdHead checks that `toks[0]` is a known --command (or a prefix of
// one) and that any following tokens fit the command's grammar.
func validateCmdHead(toks []string) bool {
	if len(toks) == 0 {
		return false
	}
	cmd := toks[0]
	if knownCmdTokens[cmd] {
		return validateCmdArgs(cmd, toks[1:])
	}
	// Partial command head — accept only if it's a strict prefix of a known
	// command and nothing has been typed after it yet.
	if len(toks) == 1 && isCmdPrefix(cmd) {
		return true
	}
	return false
}

func isCmdPrefix(s string) bool {
	if !strings.HasPrefix(s, "--") || len(s) < 3 {
		return false
	}
	for k := range knownCmdTokens {
		if len(k) > len(s) && strings.HasPrefix(k, s) {
			return true
		}
	}
	return false
}

// validateCmdArgs validates the args of a fully-typed --command. Permissive
// for commands without an enumerable grammar (--pin, --open, --download…).
func validateCmdArgs(cmd string, rest []string) bool {
	switch cmd {
	case "--t", "--tool":
		return validateToolArgs(rest)
	case "--set":
		return validateSetArgs(rest)
	default:
		return true
	}
}

func validateToolArgs(rest []string) bool {
	if len(rest) == 0 {
		return true
	}
	action := rest[0]
	for _, a := range toolActionNames {
		if a == action {
			return true // full action: any further tokens (>N, params) accepted
		}
	}
	// Partial action — only valid if it's the last token typed.
	if len(rest) == 1 {
		for _, a := range toolActionNames {
			if strings.HasPrefix(a, action) {
				return true
			}
		}
	}
	return false
}

func validateSetArgs(rest []string) bool {
	if len(rest) == 0 {
		return true
	}
	key := rest[0]
	for _, k := range setSubcommands {
		if k == key {
			return true
		}
	}
	if len(rest) == 1 {
		for _, k := range setSubcommands {
			if strings.HasPrefix(k, key) {
				return true
			}
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
	out = append(out, styleTitle.Render("  📋 Content (G)ate — Λίστες"))
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
	// Short-form aliases
	switch setting {
	case "nick":
		setting = "nickname"
	case "auto":
		setting = "autostart"
	case "ls":
		setting = "list"
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
			m.app.SendNicknameUpdateToAgent(m.app.Nickname)
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

// readAboutFile finds about.md next to the running .exe (or in the cwd as
// a fallback for `go run`) and returns its contents. The caller can update
// the page in the install directory without rebuilding any binary.
func readAboutFile() (path, content string, ok bool) {
	candidates := []string{}
	if exe, err := os.Executable(); err == nil {
		candidates = append(candidates, filepath.Join(filepath.Dir(exe), "about.md"))
	}
	candidates = append(candidates, "about.md")
	for _, p := range candidates {
		if data, err := os.ReadFile(p); err == nil {
			return p, string(data), true
		}
	}
	return "", "", false
}

// bundleBugReport zips every .log next to the running .exe (the logs/ dir
// devlog writes to) plus about.md if present, into a timestamped archive in
// Downloads. Returns the absolute path of the zip, the number of log files
// included, or an error. Designed for `--bug` / `--report` so a user with a
// problem can share a single file instead of hunting through %APPDATA%.
func bundleBugReport() (string, int, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", 0, fmt.Errorf("locate exe: %w", err)
	}
	exeDir := filepath.Dir(exe)
	logsDir := filepath.Join(exeDir, "logs")

	// Pick the destination: %USERPROFILE%\Downloads if it exists, else home,
	// else the install dir. Same fallback ladder as the export commands.
	home, _ := os.UserHomeDir()
	destDir := exeDir
	if home != "" {
		dl := filepath.Join(home, "Downloads")
		if info, err := os.Stat(dl); err == nil && info.IsDir() {
			destDir = dl
		} else {
			destDir = home
		}
	}

	stamp := time.Now().Format("20060102-150405")
	zipName := fmt.Sprintf("classsend-bugreport-%s.zip", stamp)
	zipPath := filepath.Join(destDir, zipName)

	zf, err := os.Create(zipPath)
	if err != nil {
		return "", 0, fmt.Errorf("create zip: %w", err)
	}
	defer zf.Close()

	zw := zip.NewWriter(zf)
	defer zw.Close()

	// Copy each *.log file from the logs/ dir. Bundle the most recent N to
	// keep the archive small even if logs have been accumulating for months.
	const maxLogs = 30
	logCount := 0
	if entries, err := os.ReadDir(logsDir); err == nil {
		// Sort newest-first so we ship the most relevant ones if we hit the cap.
		sort.Slice(entries, func(i, j int) bool {
			ii, _ := entries[i].Info()
			jj, _ := entries[j].Info()
			return ii.ModTime().After(jj.ModTime())
		})
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(strings.ToLower(e.Name()), ".log") {
				continue
			}
			if logCount >= maxLogs {
				break
			}
			src := filepath.Join(logsDir, e.Name())
			data, rerr := os.ReadFile(src)
			if rerr != nil {
				continue
			}
			w, werr := zw.Create("logs/" + e.Name())
			if werr != nil {
				continue
			}
			if _, werr := w.Write(data); werr == nil {
				logCount++
			}
		}
	}

	// Include about.md so the report carries the running build's published
	// description — useful when the file has been edited post-install.
	if data, err := os.ReadFile(filepath.Join(exeDir, "about.md")); err == nil {
		if w, werr := zw.Create("about.md"); werr == nil {
			_, _ = w.Write(data)
		}
	}

	// Add a small manifest so the recipient can see version, role, and where
	// the report came from without unzipping every file.
	manifest := fmt.Sprintf(
		"ClassSend 2 bug report\nbuild: %s\nrole: (see startup line in any log)\nexe dir: %s\nlogs dir: %s\nlogs included: %d\ngenerated: %s\n",
		buildinfo.String(), exeDir, logsDir, logCount, time.Now().Format(time.RFC3339))
	if w, werr := zw.Create("MANIFEST.txt"); werr == nil {
		_, _ = w.Write([]byte(manifest))
	}

	if err := zw.Close(); err != nil {
		return "", 0, fmt.Errorf("close zip: %w", err)
	}
	if err := zf.Close(); err != nil {
		return "", 0, fmt.Errorf("close file: %w", err)
	}
	return zipPath, logCount, nil
}
