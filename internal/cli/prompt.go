package cli

import (
	"bufio"
	"fmt"
	"strings"
)

// confirm asks the user to confirm a destructive action and is the only place
// the CLI reads from stdin. --yes answers the question in advance; --no-input
// and --json, which implies it, refuse to ask and fail with
// ExitConfirmationRequired, and so does an input stream with no answer in it.
func (d *Deps) confirm(prompt string) error {
	if d.Global.Yes {
		return nil
	}
	if d.Global.NoInput || d.Global.JSON {
		return Errorf(ExitConfirmationRequired, "%s: pass --yes to confirm", prompt)
	}

	_, _ = fmt.Fprintf(d.Stdout, "%s [y/N]: ", prompt)
	line, err := bufio.NewReader(d.Stdin).ReadString('\n')
	if err != nil && line == "" {
		return Errorf(ExitConfirmationRequired, "%s: no answer given", prompt)
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return nil
	default:
		return Errorf(ExitConfirmationRequired, "%s: cancelled", prompt)
	}
}
