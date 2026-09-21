package ports

import (
	"context"
	"encoding/json"
	"errors"
)

// ErrDaemonUnavailable reports that no daemon is listening on the control
// socket. Commands that need one map it to their exit code.
var ErrDaemonUnavailable = errors.New("daemon not running")

// ControlRequest is one command sent to a running daemon.
type ControlRequest struct {
	Cmd      string
	FolderID string
	DryRun   bool
}

// ControlResponse answers a ControlRequest. Result carries the already
// encoded, command-specific payload so the transport stays free of
// application types.
type ControlResponse struct {
	OK     bool
	Result json.RawMessage
	Error  string
	// Code is the exit code the CLI should report when OK is false.
	Code int
}

// ControlHandler answers control requests; the daemon implements it.
type ControlHandler interface {
	Handle(ctx context.Context, req ControlRequest) ControlResponse
}

// ControlServer accepts control requests and passes them to a handler.
type ControlServer interface {
	// Serve answers requests until ctx is done.
	Serve(ctx context.Context, h ControlHandler) error
	// Close stops accepting requests and releases the socket.
	Close() error
}

// ControlClient sends a single request to a running daemon. It returns an
// error wrapping ErrDaemonUnavailable when no daemon can be reached.
type ControlClient interface {
	Call(ctx context.Context, req ControlRequest) (ControlResponse, error)
}
