package tui

import "fmt"

// sprintf is fmt.Sprintf under a shorter name, because the views call it on
// nearly every line.
func sprintf(format string, a ...any) string { return fmt.Sprintf(format, a...) }
