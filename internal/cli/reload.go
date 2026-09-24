package cli

import (
	"context"
	"errors"
	"fmt"

	"github.com/wesbragagt/syngo/internal/app/ports"
	"github.com/wesbragagt/syngo/internal/compose"
)

// reloadDaemon tells a running daemon that the folder or device table changed,
// so it starts or stops folder loops, dials peers that were added and tells
// its connected peers which folders this device serves now. It is best effort:
// without a daemon there is nothing to tell, and a daemon that refuses leaves
// the command itself successful, because the change is already written. A
// refusal is reported on stderr so it is not lost.
func reloadDaemon(ctx context.Context, deps *Deps, app *compose.App) {
	err := app.Control.Reload(ctx)
	if err == nil || errors.Is(err, ports.ErrDaemonUnavailable) {
		return
	}
	_, _ = fmt.Fprintf(deps.Stderr, "warning: the daemon did not reload its configuration: %v\n", err)
}
