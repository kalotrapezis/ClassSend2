package monitoring

// StudentInfo is a snapshot of a connected student passed to the monitoring session.
type StudentInfo struct {
	ID       string // stable MAC-based ID used to route commands
	Hostname string // OS hostname (matches ScreenshotPayload.StudentID)
	Nickname string // display name (Nickname or Hostname)
}

// ShotMsg carries one screenshot received from a student.
type ShotMsg struct {
	StudentID string // matches StudentInfo.Hostname
	Data      []byte // JPEG bytes
}
