// Package cli implements the syncthis command line interface.
package cli

import (
	"errors"
	"fmt"

	"github.com/wesbragagt/syncthis/internal/app/conflicts"
	"github.com/wesbragagt/syncthis/internal/app/daemon"
	"github.com/wesbragagt/syncthis/internal/app/peers"
	"github.com/wesbragagt/syncthis/internal/app/ports"
	"github.com/wesbragagt/syncthis/internal/compose"
	"github.com/wesbragagt/syncthis/internal/domain/bandwidth"
)

// Process exit codes. They are part of the CLI contract; see docs/CLI.md.
const (
	ExitOK                   = 0
	ExitError                = 1
	ExitUsage                = 2
	ExitNotInitialized       = 3
	ExitNotFound             = 4
	ExitAlreadyExists        = 5
	ExitDaemonUnavailable    = 6
	ExitPeerUnreachable      = 7
	ExitIntegrity            = 8
	ExitConfirmationRequired = 9
	ExitDryRun               = 10
)

// ExitCodeError carries the exit code a command should terminate with. The
// plan calls this type ExitError; that name is taken by the exit code constant
// above, so the type is named ExitCodeError.
type ExitCodeError struct {
	Code int
	Err  error
}

// Error implements the error interface.
func (e *ExitCodeError) Error() string {
	if e.Err == nil {
		return fmt.Sprintf("exit code %d", e.Code)
	}
	return e.Err.Error()
}

// Unwrap returns the underlying error.
func (e *ExitCodeError) Unwrap() error { return e.Err }

// ErrDryRunChanges reports that a dry run finished and would have changed
// something. It carries no message: the command has already printed its
// report, so Execute only turns it into the exit code.
var ErrDryRunChanges = &ExitCodeError{Code: ExitDryRun}

// Errorf returns an ExitError with the given code and a formatted message.
func Errorf(code int, format string, a ...any) error {
	return &ExitCodeError{Code: code, Err: fmt.Errorf(format, a...)}
}

// codeOf returns the exit code an error should produce.
func codeOf(err error) int {
	if err == nil {
		return ExitOK
	}
	var exitErr *ExitCodeError
	if errors.As(err, &exitErr) {
		return exitErr.Code
	}
	// A failure reported by the daemon already carries the exit code the
	// command would have produced had it done the work itself.
	var remote *daemon.RemoteError
	if errors.As(err, &remote) {
		return remote.Code
	}
	switch {
	case errors.Is(err, compose.ErrNotInitialized):
		return ExitNotInitialized
	case errors.Is(err, ports.ErrNotFound):
		return ExitNotFound
	case errors.Is(err, ports.ErrAlreadyExists):
		return ExitAlreadyExists
	case errors.Is(err, ports.ErrDaemonUnavailable), errors.Is(err, compose.ErrDaemonRunning):
		return ExitDaemonUnavailable
	case errors.Is(err, ports.ErrPeerUnreachable), errors.Is(err, ports.ErrDeviceMismatch):
		return ExitPeerUnreachable
	case errors.Is(err, conflicts.ErrVersionGone), errors.Is(err, conflicts.ErrChanged):
		// Neither file holds the version the user asked to keep any
		// more, or a file the resolution would have deleted holds
		// content neither side had.
		return ExitIntegrity
	case errors.Is(err, conflicts.ErrAlreadyResolved):
		// There is no open conflict under that id any more.
		return ExitNotFound
	case errors.Is(err, peers.ErrNotConfirmed):
		return ExitConfirmationRequired
	case errors.Is(err, peers.ErrInvalidAddress), errors.Is(err, peers.ErrSelf):
		// Both are mistakes in what the user typed.
		return ExitUsage
	case errors.Is(err, compose.ErrListenAddress):
		return ExitUsage
	case errors.Is(err, bandwidth.ErrInvalidRate):
		// A rate that cannot be read is a mistake in what the user
		// typed, or in what config.json holds.
		return ExitUsage
	case errors.Is(err, compose.ErrPublicAddress):
		// The refusal is about the flags the daemon was given.
		return ExitUsage
	}
	return ExitError
}
