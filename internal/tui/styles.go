package tui

import "github.com/charmbracelet/lipgloss"

// Colour palette — dark warm theme matching the old ClassSend
var (
	colBg        = lipgloss.Color("#1a0a00")
	colPanel     = lipgloss.Color("#2a1200")
	colBorder    = lipgloss.Color("#6b2d00")
	colAccent    = lipgloss.Color("#e07020")
	colAccentDim = lipgloss.Color("#804010")
	colText      = lipgloss.Color("#f0d0a0")
	colTextDim   = lipgloss.Color("#806040")
	colGreen     = lipgloss.Color("#40c060")
	colRed       = lipgloss.Color("#c04040")
	colGray      = lipgloss.Color("#504030")
	colTeacher   = lipgloss.Color("#e07020")
	colStudent   = lipgloss.Color("#80c0ff")
	colSystem    = lipgloss.Color("#806040")
	colCmd       = lipgloss.Color("#5bc8f5") // input highlight when typing a command
)

var (
	styleBorder = lipgloss.NewStyle().
			BorderStyle(lipgloss.RoundedBorder()).
			BorderForeground(colBorder)

	styleTitle = lipgloss.NewStyle().
			Foreground(colAccent).
			Bold(true).
			Padding(0, 1)

	styleStatus = lipgloss.NewStyle().
			Foreground(colTextDim).
			Padding(0, 1)

	styleConnected = lipgloss.NewStyle().
			Foreground(colGreen).
			Bold(true)

	styleDisconnected = lipgloss.NewStyle().
				Foreground(colRed).
				Bold(true)

	styleStudentOnline = lipgloss.NewStyle().
				Foreground(colGreen)

	styleStudentOffline = lipgloss.NewStyle().
				Foreground(colGray)

	styleInputBox = lipgloss.NewStyle().
			BorderStyle(lipgloss.NormalBorder()).
			BorderTop(true).
			BorderForeground(colBorder).
			Foreground(colText)

	styleMsgTeacher = lipgloss.NewStyle().
			Foreground(colTeacher).
			Bold(true)

	styleMsgStudent = lipgloss.NewStyle().
			Foreground(colStudent)

	styleMsgSystem = lipgloss.NewStyle().
			Foreground(colSystem).
			Italic(true)

	styleTimestamp = lipgloss.NewStyle().
			Foreground(colTextDim)

	styleHint = lipgloss.NewStyle().
			Foreground(colTextDim).
			Padding(0, 1)

	stylePanel = lipgloss.NewStyle().
			Background(colPanel)

	styleSelected = lipgloss.NewStyle().
			Background(colAccentDim).
			Foreground(colText)

	stylePinned = lipgloss.NewStyle().
			Foreground(colAccent).
			Bold(true)

	styleBlocked = lipgloss.NewStyle().
			Foreground(colRed).
			Italic(true)

	styleMsgNum = lipgloss.NewStyle().
			Foreground(colAccentDim)

	styleMsgReported = lipgloss.NewStyle().
				Foreground(colRed).
				Bold(true)

	styleReportTag = lipgloss.NewStyle().
			Foreground(colRed).
			Bold(true)
)
