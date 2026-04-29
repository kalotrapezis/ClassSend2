package tui

import "fmt"

// formatSize returns a human-readable file size string.
func formatSize(bytes int64) string {
	switch {
	case bytes == 0:
		return "—"
	case bytes < 1024:
		return fmt.Sprintf("%dB", bytes)
	case bytes < 1024*1024:
		return fmt.Sprintf("%.0fKB", float64(bytes)/1024)
	default:
		return fmt.Sprintf("%.1fMB", float64(bytes)/(1024*1024))
	}
}
