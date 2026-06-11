package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"

	"classsend/internal/core"
)

// newTeacherModel builds a minimal teacher Model suitable for View() rendering
// tests — no network, just enough state to paint a frame.
func newTeacherModel(t *testing.T, names ...string) *Model {
	t.Helper()
	app, err := core.NewApp(core.RoleTeacher, t.TempDir(), true)
	if err != nil {
		t.Fatalf("NewApp: %v", err)
	}
	m := New(app)
	m.screen = screenChat
	for i, n := range names {
		m.students = append(m.students, studentEntry{
			id:     n,
			name:   n,
			online: true,
		})
		_ = i
	}
	return m
}

func viewHeight(s string) int { return strings.Count(s, "\n") + 1 }

// The teacher shortcut bar has 11 entries and is wider than a typical classroom
// screen. A fixed Width() used to wrap it onto a 2nd line, but the layout
// budgets exactly one row for it — the overflow scrolled the alt-screen frame
// and left ghost rows (a duplicated sidebar) in scrollback. The bar must stay
// one line regardless of width.
func TestBottomBar_SingleLine(t *testing.T) {
	m := newTeacherModel(t)
	for _, w := range []int{40, 60, 80, 100, 132} {
		m.width = w
		if got := viewHeight(m.renderBottomBar()); got != 1 {
			t.Errorf("width=%d: bottom bar rendered %d lines, want 1", w, got)
		}
	}
}

// View() must never emit more rows than the terminal has; an overflowing frame
// scrolls the alt-screen and ghosts the previous render. Exercise a narrow
// classroom-sized screen with a full student list and a staged file.
func TestViewChat_NeverExceedsHeight(t *testing.T) {
	m := newTeacherModel(t,
		"DESKTOP-MDKQOAB", "PC1", "PC3", "PC6", "PC-T8R2", "PC-WMXN", "α-PC")

	cases := []struct {
		w, h   int
		staged bool
	}{
		{80, 24, false},
		{80, 24, true},
		{72, 20, false},
		{100, 30, true},
		{60, 16, false},
	}
	for _, c := range cases {
		m.width, m.height = c.w, c.h
		m.stagedFile = ""
		if c.staged {
			m.stagedFile = "/tmp/Διαδραστικά.zip"
		}
		m.resizeComponents()
		got := viewHeight(lipgloss.NewStyle().MaxWidth(c.w).Render(m.View()))
		if got > c.h {
			t.Errorf("%dx%d staged=%v: View() = %d rows, exceeds height %d",
				c.w, c.h, c.staged, got, c.h)
		}
	}
}
