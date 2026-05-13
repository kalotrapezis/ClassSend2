package monitoring

// StudentInfo is a snapshot of a connected student passed to the monitoring session.
type StudentInfo struct {
	ID       string // stable MAC-based ID used to route commands
	Hostname string // OS hostname (matches ScreenshotPayload.StudentID)
	Nickname string // display name (Nickname or Hostname)
}

// ShotMsg carries one screenshot received from a student. When Status=="load"
// the student PC is overloaded — Data is empty and the session router should
// bump lastSeen but not repaint the cell, so the teacher keeps the last good
// thumbnail.
type ShotMsg struct {
	StudentID string // matches StudentInfo.Hostname
	Data      []byte // JPEG bytes (empty when Status=="load")
	Status    string // "" or "ok" = normal; "load" = skip paint
}
