package tui

import "fmt"

// sprintf is fmt.Sprintf under a shorter name, because the views call it on
// nearly every line.
func sprintf(format string, a ...any) string { return fmt.Sprintf(format, a...) }

// truncate shortens a string to width, marking what it cut. A width below four
// is treated as four, so the marker always fits.
func truncate(s string, width int) string {
	if width < 4 {
		width = 4
	}
	runes := []rune(s)
	if len(runes) <= width {
		return s
	}
	return string(runes[:width-1]) + "\u2026"
}
