package tui

import (
	"fmt"
	"strings"
)

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

// deref reads an optional string field of a JSON document, giving fallback
// where the document had null.
func deref(s *string, fallback string) string {
	if s == nil || *s == "" {
		return fallback
	}
	return *s
}

// bytesHuman renders a byte count in the largest binary unit that keeps it
// above one, which is what a transfer row has room for.
func bytesHuman(n int64) string {
	const unit = 1024
	if n < unit {
		return sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for n/div >= unit && exp < 4 {
		div *= unit
		exp++
	}
	return sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTP"[exp])
}

// displayWidth is the number of columns a cell takes, ignoring the escape
// sequences lipgloss wrapped it in. A coloured cell is padded to the same
// width as a plain one this way, so a table stays a table.
func displayWidth(s string) int {
	return len([]rune(stripANSI(s)))
}

// stripANSI removes the escape sequences a style added.
func stripANSI(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != 0x1b {
			b.WriteByte(s[i])
			continue
		}
		// Skip to the end of the sequence, which is its first letter.
		for i++; i < len(s); i++ {
			if (s[i] >= 'a' && s[i] <= 'z') || (s[i] >= 'A' && s[i] <= 'Z') {
				break
			}
		}
	}
	return b.String()
}

// pad right-pads a cell to width with spaces, so a column stays a column
// whatever a file is called.
func pad(s string, width int) string {
	if n := width - displayWidth(s); n > 0 {
		return s + strings.Repeat(" ", n)
	}
	return s
}

// columns renders rows under headers as a padded table. Each column is as wide
// as its widest cell.
func columns(headers []string, rows [][]string) string {
	widths := make([]int, len(headers))
	for i, h := range headers {
		widths[i] = displayWidth(h)
	}
	for _, row := range rows {
		for i, cell := range row {
			if i < len(widths) && displayWidth(cell) > widths[i] {
				widths[i] = displayWidth(cell)
			}
		}
	}

	var b strings.Builder
	head := make([]string, len(headers))
	for i, h := range headers {
		head[i] = pad(h, widths[i])
	}
	b.WriteString(styleHeader.Render(strings.TrimRight(strings.Join(head, "  "), " ")) + "\n")
	for _, row := range rows {
		cells := make([]string, len(row))
		for i, cell := range row {
			cells[i] = pad(cell, widths[i])
		}
		b.WriteString(strings.TrimRight(strings.Join(cells, "  "), " ") + "\n")
	}
	return b.String()
}
