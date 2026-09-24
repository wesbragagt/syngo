// Package unixctl carries control requests between the CLI and a running
// daemon over a unix socket, one newline-delimited JSON request and response
// per connection.
package unixctl

import (
	"encoding/json"

	"github.com/wesbragagt/syngo/internal/app/ports"
)

// maxRequestBytes bounds how much of a request line is read.
const maxRequestBytes = 1 << 20

// wireRequest is the on-the-wire form of a ports.ControlRequest.
type wireRequest struct {
	Cmd      string `json:"cmd"`
	FolderID string `json:"folder_id,omitempty"`
	DryRun   bool   `json:"dry_run,omitempty"`
}

// wireResponse is the on-the-wire form of a ports.ControlResponse.
type wireResponse struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  string          `json:"error,omitempty"`
	Code   int             `json:"code,omitempty"`
}

// toWireRequest converts a port request to its wire form.
func toWireRequest(r ports.ControlRequest) wireRequest {
	return wireRequest{Cmd: r.Cmd, FolderID: r.FolderID, DryRun: r.DryRun}
}

// request converts a wire request back to its port form.
func (w wireRequest) request() ports.ControlRequest {
	return ports.ControlRequest{Cmd: w.Cmd, FolderID: w.FolderID, DryRun: w.DryRun}
}

// toWireResponse converts a port response to its wire form.
func toWireResponse(r ports.ControlResponse) wireResponse {
	return wireResponse{OK: r.OK, Result: r.Result, Error: r.Error, Code: r.Code}
}

// response converts a wire response back to its port form.
func (w wireResponse) response() ports.ControlResponse {
	return ports.ControlResponse{OK: w.OK, Result: w.Result, Error: w.Error, Code: w.Code}
}
