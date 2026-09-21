package daemon

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/wesbragagt/syncthis/internal/app/ports"
	"github.com/wesbragagt/syncthis/internal/app/repair"
	"github.com/wesbragagt/syncthis/internal/app/status"
	"github.com/wesbragagt/syncthis/internal/domain/folder"
)

var _ API = (*Client)(nil)

// RemoteError is a failure reported by the daemon. Code is the exit code the
// command should terminate with.
type RemoteError struct {
	Code    int
	Message string
}

// Error implements the error interface.
func (e *RemoteError) Error() string { return e.Message }

// Client talks to a running daemon over a control transport. Every method
// returns an error wrapping ports.ErrDaemonUnavailable when no daemon answers,
// which is how callers decide to do the work themselves instead.
type Client struct {
	rpc ports.ControlClient
}

// NewClient returns a client speaking through rpc.
func NewClient(rpc ports.ControlClient) *Client { return &Client{rpc: rpc} }

// Status asks the daemon for the status report.
func (c *Client) Status(ctx context.Context) (status.Report, error) {
	var report status.Report
	err := c.call(ctx, ports.ControlRequest{Cmd: CmdStatus}, &report)
	return report, err
}

// Pause pauses a folder, or every folder when id is empty.
func (c *Client) Pause(ctx context.Context, id folder.ID) error {
	return c.call(ctx, ports.ControlRequest{Cmd: CmdPause, FolderID: string(id)}, nil)
}

// Resume resumes a folder, or every folder when id is empty.
func (c *Client) Resume(ctx context.Context, id folder.ID) error {
	return c.call(ctx, ports.ControlRequest{Cmd: CmdResume, FolderID: string(id)}, nil)
}

// Rescan queues a full scan of a folder, or of every folder when id is empty.
func (c *Client) Rescan(ctx context.Context, id folder.ID) error {
	return c.call(ctx, ports.ControlRequest{Cmd: CmdRescan, FolderID: string(id)}, nil)
}

// Repair runs a repair of one folder inside the daemon.
func (c *Client) Repair(ctx context.Context, id folder.ID, dryRun bool) (repair.Report, error) {
	var report repair.Report
	err := c.call(ctx, ports.ControlRequest{Cmd: CmdRepair, FolderID: string(id), DryRun: dryRun}, &report)
	return report, err
}

// Reload asks the daemon to re-read the folder and device tables.
func (c *Client) Reload(ctx context.Context) error {
	return c.call(ctx, ports.ControlRequest{Cmd: CmdReload}, nil)
}

// Shutdown asks the daemon to stop.
func (c *Client) Shutdown(ctx context.Context) error {
	return c.call(ctx, ports.ControlRequest{Cmd: CmdShutdown}, nil)
}

// call sends one request and decodes the result into out, which may be nil.
func (c *Client) call(ctx context.Context, req ports.ControlRequest, out any) error {
	resp, err := c.rpc.Call(ctx, req)
	if err != nil {
		return err
	}
	if !resp.OK {
		return &RemoteError{Code: resp.Code, Message: resp.Error}
	}
	if out == nil || len(resp.Result) == 0 {
		return nil
	}
	if err := json.Unmarshal(resp.Result, out); err != nil {
		return fmt.Errorf("decode %s result: %w", req.Cmd, err)
	}
	return nil
}
