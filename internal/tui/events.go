package tui

import "classsend/internal/core"

// Network events delivered to the TUI model via tea.Cmd

type evStudentJoin struct{ name, id, ip string }
type evStudentLeave struct{ id string }
type evChatMsg struct{ msg core.ChatMessage }
type evConnected struct{}
type evDisconnected struct{}
type evStateChange struct{ state core.ClassState }
type evCommandReceived struct{ action, param string }
type evMessagesUpdated struct{ msgs []core.ChatMessage }
type evStudentMissing struct {
	mac, nickname, hostname string
	count                   int
}
type evFileReceived struct{ fileID, name string }
type evSysMsg struct{ text string }
type evCmdFailed struct{ nickname, action, errMsg string }
type evMatrixTick struct{}
