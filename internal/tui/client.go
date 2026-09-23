// Package tui is the interactive terminal interface. It holds no
// synchronization state of its own and no business logic: every screen is a
// view of a `syncthis ... --json` document, and every action it offers is one
// command a user could have typed instead. docs/TUI.md lists the command
// behind each key.
//
// The package deliberately imports nothing from internal/app, internal/domain
// or internal/compose. Its only contract with the rest of the program is the
// Runner below, which internal/cli implements; internal/arch/deps_test.go
// enforces that.
package tui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Exit codes of the command line, from docs/CLI.md. The TUI reads them to
// tell "no identity yet" and "no daemon" apart from a real failure.
const (
	ExitOK                = 0
	ExitError             = 1
	ExitUsage             = 2
	ExitNotInitialized    = 3
	ExitNotFound          = 4
	ExitAlreadyExists     = 5
	ExitDaemonUnavailable = 6
	ExitPeerUnreachable   = 7
	ExitIntegrity         = 8
	ExitConfirmation      = 9
	ExitDryRun            = 10
	ExitTailnetGone       = 11
)

// Runner runs one syncthis invocation and reports what it wrote to stdout and
// the code it exited with. It is the whole of the TUI's dependency on the rest
// of the program: the TUI cannot reach the daemon except by asking for a
// command that docs/CLI.md documents.
//
// A Runner returns a non-nil error only when the invocation could not be made
// at all. A command that ran and failed is reported through the exit code, the
// way a shell reports it.
type Runner interface {
	Run(ctx context.Context, args []string) (stdout []byte, code int, err error)
}

// RunnerFunc adapts a function to a Runner.
type RunnerFunc func(ctx context.Context, args []string) ([]byte, int, error)

// Run implements Runner.
func (f RunnerFunc) Run(ctx context.Context, args []string) ([]byte, int, error) {
	return f(ctx, args)
}

// CmdError is a syncthis command that ran and failed. Code is the process exit
// code from docs/CLI.md and Message is what the command reported.
type CmdError struct {
	// Command is the invocation, for the error screen and for the log, so a
	// failure inside the TUI can be reproduced by typing it.
	Command string
	Code    int
	Message string
}

// Error implements the error interface.
func (e *CmdError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("%s: exit %d", e.Command, e.Code)
	}
	return e.Message
}

// Client turns the documented commands into typed results. It adds the global
// flags every call needs and nothing else: --json, so the answer is a
// document, and --data-dir, so a TUI started with one talks to that device.
type Client struct {
	// Runner performs the invocation.
	Runner Runner
	// DataDir is passed as --data-dir when it is not empty.
	DataDir string
}

// Argv is the full argument list of a call, the one a user would type after
// `syncthis`. It exists so a screen can show the command behind a key.
func (c Client) Argv(args ...string) []string {
	out := make([]string, 0, len(args)+3)
	if c.DataDir != "" {
		out = append(out, "--data-dir", c.DataDir)
	}
	out = append(out, args...)
	return out
}

// Command renders a call the way a user would type it.
func (c Client) Command(args ...string) string {
	return "syncthis " + strings.Join(c.Argv(args...), " ")
}

// call runs one command with --json and decodes its document into out. A
// command that failed becomes a *CmdError carrying its exit code.
func (c Client) call(ctx context.Context, out any, args ...string) error {
	if c.Runner == nil {
		return errors.New("tui: no command runner configured")
	}
	full := c.Argv(append([]string{"--json"}, args...)...)
	stdout, code, err := c.Runner.Run(ctx, full)
	if err != nil {
		return fmt.Errorf("%s: %w", c.Command(args...), err)
	}
	if code != ExitOK {
		return &CmdError{Command: c.Command(args...), Code: code, Message: errorMessage(stdout, code)}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(stdout, out); err != nil {
		return fmt.Errorf("%s: cannot read the answer: %w", c.Command(args...), err)
	}
	return nil
}

// errorPayload is the failure document `--json` prints, from docs/CLI.md.
type errorPayload struct {
	Error struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// errorMessage reads the message out of a failure document, falling back to
// the raw output when the command wrote something else.
func errorMessage(stdout []byte, code int) string {
	var payload errorPayload
	if err := json.Unmarshal(stdout, &payload); err == nil && payload.Error.Message != "" {
		return payload.Error.Message
	}
	if trimmed := strings.TrimSpace(string(stdout)); trimmed != "" {
		return trimmed
	}
	return fmt.Sprintf("exit %d", code)
}

// ExitCode returns the exit code an error carries, or -1 when it is not a
// command failure.
func ExitCode(err error) int {
	var cmdErr *CmdError
	if errors.As(err, &cmdErr) {
		return cmdErr.Code
	}
	return -1
}

// Status reports the device, the daemon, every folder, every peer and the
// transfer journal: `syncthis status --json`.
func (c Client) Status(ctx context.Context) (Status, error) {
	var out Status
	err := c.call(ctx, &out, "status")
	return out, err
}

// NetworkPath reports how traffic reaches every configured peer:
// `syncthis network path --json`. It needs a local Tailscale client and exits
// 11 without one, which is not a failure of the TUI.
func (c Client) NetworkPath(ctx context.Context) ([]NetPath, error) {
	var out []NetPath
	err := c.call(ctx, &out, "network", "path")
	return out, err
}

// Init creates the identity: `syncthis init --name NAME --listen ADDR`.
// An empty name or address leaves the command's own default.
func (c Client) Init(ctx context.Context, name, listen string) (InitResult, error) {
	args := []string{"init"}
	if name != "" {
		args = append(args, "--name", name)
	}
	if listen != "" {
		args = append(args, "--listen", listen)
	}
	var out InitResult
	err := c.call(ctx, &out, args...)
	return out, err
}

// PeerAdd trusts the device at an address: `syncthis peer add HOST[:PORT]`.
// deviceID, when given, pins the identity the peer must present. --yes is
// passed because the confirmation has already been made in the TUI, on the
// identity `peer test` showed; without it the command would exit 9, since
// --json implies --no-input.
func (c Client) PeerAdd(ctx context.Context, host, deviceID string) (PeerAdded, error) {
	args := []string{"peer", "add", host, "--yes"}
	if deviceID != "" {
		args = append(args, "--device-id", deviceID)
	}
	var out PeerAdded
	err := c.call(ctx, &out, args...)
	return out, err
}

// PeerTest runs the handshake against an address and reports the identity it
// presented: `syncthis peer test HOST[:PORT]`. It is what the pairing screen
// shows before it asks whether to trust the device.
func (c Client) PeerTest(ctx context.Context, target string) (PeerTested, error) {
	var out PeerTested
	err := c.call(ctx, &out, "peer", "test", target)
	return out, err
}

// FolderAdd starts synchronizing a directory:
// `syncthis folder add PATH --id ID`.
func (c Client) FolderAdd(ctx context.Context, path, id string) (FolderAdded, error) {
	var out FolderAdded
	err := c.call(ctx, &out, "folder", "add", path, "--id", id)
	return out, err
}

// FolderGrant authorizes one paired device for one folder:
// `syncthis folder grant ID DEVICE-ID`.
func (c Client) FolderGrant(ctx context.Context, folderID, deviceID string) (Granted, error) {
	var out Granted
	err := c.call(ctx, &out, "folder", "grant", folderID, deviceID)
	return out, err
}

// FolderDevicesOf lists the devices authorized for a folder:
// `syncthis folder devices ID`.
func (c Client) FolderDevicesOf(ctx context.Context, folderID string) (FolderDevices, error) {
	var out FolderDevices
	err := c.call(ctx, &out, "folder", "devices", folderID)
	return out, err
}

// ConflictList lists the paths both sides changed:
// `syncthis conflict list --json`.
func (c Client) ConflictList(ctx context.Context) ([]Conflict, error) {
	var out []Conflict
	err := c.call(ctx, &out, "conflict", "list")
	return out, err
}

// ConflictShow describes one conflict and both versions as they are on disk:
// `syncthis conflict show ID`.
func (c Client) ConflictShow(ctx context.Context, id string) (Conflict, error) {
	var out Conflict
	err := c.call(ctx, &out, "conflict", "show", id)
	return out, err
}

// Keep names the version a resolution keeps. They are exactly the three words
// `conflict resolve --keep` accepts; the TUI invents no fourth.
const (
	KeepLocal  = "local"
	KeepRemote = "remote"
	KeepBoth   = "both"
)

// ConflictResolve says which version wins:
// `syncthis conflict resolve ID --keep local|remote|both --yes`. The
// confirmation the command would ask for is the one the screen already asked.
func (c Client) ConflictResolve(ctx context.Context, id, keep string) (ConflictResolved, error) {
	var out ConflictResolved
	err := c.call(ctx, &out, "conflict", "resolve", id, "--keep", keep, "--yes")
	return out, err
}

// Rescan queues a full scan of one folder, or of every folder when id is
// empty: `syncthis rescan [ID]`.
func (c Client) Rescan(ctx context.Context, folderID string) error {
	args := []string{"rescan"}
	if folderID != "" {
		args = append(args, folderID)
	}
	return c.call(ctx, nil, args...)
}

// Pause stops scanning and syncing a folder: `syncthis pause [FOLDER]`.
func (c Client) Pause(ctx context.Context, folderID string) error {
	args := []string{"pause"}
	if folderID != "" {
		args = append(args, folderID)
	}
	return c.call(ctx, nil, args...)
}

// Resume undoes Pause: `syncthis resume [FOLDER]`.
func (c Client) Resume(ctx context.Context, folderID string) error {
	args := []string{"resume"}
	if folderID != "" {
		args = append(args, folderID)
	}
	return c.call(ctx, nil, args...)
}
