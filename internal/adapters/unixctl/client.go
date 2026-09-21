package unixctl

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"

	"github.com/wesbragagt/syncthis/internal/app/ports"
)

var _ ports.ControlClient = (*Client)(nil)

// Client sends control requests to the daemon owning a control socket.
type Client struct {
	path string
}

// NewClient returns a client for the socket at path. No connection is made
// until Call runs, so a client is cheap to build even without a daemon.
func NewClient(path string) *Client { return &Client{path: path} }

// Call sends one request and returns the daemon's answer. When no daemon is
// listening the error wraps ports.ErrDaemonUnavailable.
func (c *Client) Call(ctx context.Context, req ports.ControlRequest) (ports.ControlResponse, error) {
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "unix", c.path)
	if err != nil {
		return ports.ControlResponse{}, fmt.Errorf("control socket %s: %v: %w", c.path, err, ports.ErrDaemonUnavailable)
	}
	defer func() { _ = conn.Close() }()

	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	data, err := json.Marshal(toWireRequest(req))
	if err != nil {
		return ports.ControlResponse{}, fmt.Errorf("encode control request: %w", err)
	}
	if _, err := conn.Write(append(data, '\n')); err != nil {
		return ports.ControlResponse{}, fmt.Errorf("write control request: %w", err)
	}

	line, err := bufio.NewReader(io.LimitReader(conn, maxRequestBytes)).ReadBytes('\n')
	if err != nil && len(line) == 0 {
		return ports.ControlResponse{}, fmt.Errorf("read control response: %w", err)
	}
	var wire wireResponse
	if err := json.Unmarshal(line, &wire); err != nil {
		return ports.ControlResponse{}, fmt.Errorf("decode control response: %w", err)
	}
	return wire.response(), nil
}
