package tui

import "github.com/charmbracelet/lipgloss"

// The palette is the sixteen terminal colours, not a truecolour scheme: the
// TUI is meant to be readable over SSH, in tmux and on a terminal with the
// user's own theme, and a fixed hex colour is the thing that stops being
// readable there.
var (
	styleTitle = lipgloss.NewStyle().Bold(true)
	styleDim   = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	styleOK    = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	styleWarn  = lipgloss.NewStyle().Foreground(lipgloss.Color("3"))
	styleErr   = lipgloss.NewStyle().Foreground(lipgloss.Color("1"))
	styleCmd   = lipgloss.NewStyle().Foreground(lipgloss.Color("5"))
)
