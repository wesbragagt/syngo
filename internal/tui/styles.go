package tui

import "github.com/charmbracelet/lipgloss"

// The palette is the sixteen terminal colours, not a truecolour scheme: the
// TUI is meant to be readable over SSH, in tmux and on a terminal with the
// user's own theme, and a fixed hex colour is the thing that stops being
// readable there.
var (
	styleTitle  = lipgloss.NewStyle().Bold(true)
	styleHeader = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("4"))
	styleSel    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("6"))
	styleDim    = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	styleOK     = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	styleWarn   = lipgloss.NewStyle().Foreground(lipgloss.Color("3"))
	styleErr    = lipgloss.NewStyle().Foreground(lipgloss.Color("1"))
	styleCmd    = lipgloss.NewStyle().Foreground(lipgloss.Color("5"))
)

// stateStyle colours a folder or peer state word. Anything unrecognized is
// left plain rather than guessed at.
func stateStyle(state string) lipgloss.Style {
	switch state {
	case "idle", "connected":
		return styleOK
	case "scanning", "syncing", "connecting":
		return styleWarn
	case "error", "unreachable":
		return styleErr
	default:
		return styleDim
	}
}

// pathStyle colours a network path kind: direct, relay or unknown.
func pathStyle(kind string) lipgloss.Style {
	switch kind {
	case "direct":
		return styleOK
	case "relay":
		return styleWarn
	default:
		return styleDim
	}
}
