// Package cli implements the syncthis command line interface.
package cli

import (
	"errors"
	"fmt"
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
	return ExitError
}
