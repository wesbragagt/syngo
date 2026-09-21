package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/wesbragagt/syncthis/internal/app/ports"
	"github.com/wesbragagt/syncthis/internal/app/repair"
	"github.com/wesbragagt/syncthis/internal/app/status"
	"github.com/wesbragagt/syncthis/internal/domain/folder"
)

// Control commands understood by a running daemon.
const (
	CmdStatus   = "status"
	CmdPause    = "pause"
	CmdResume   = "resume"
	CmdRescan   = "rescan"
	CmdRepair   = "repair"
	CmdReload   = "reload"
	CmdShutdown = "shutdown"
)

// Response codes mirror the process exit codes documented in docs/CLI.md. The
// application layer cannot import the CLI, so they are repeated here.
const (
	codeError    = 1
	codeUsage    = 2
	codeNotFound = 4
)

// API is what a control client may ask a running daemon to do. An empty
// folder id means "every folder".
type API interface {
	Status(ctx context.Context) (status.Report, error)
	Pause(ctx context.Context, id folder.ID) error
	Resume(ctx context.Context, id folder.ID) error
	// Rescan queues a full scan; it does not wait for the scan to finish.
	Rescan(ctx context.Context, id folder.ID) error
	Repair(ctx context.Context, id folder.ID, dryRun bool) (repair.Report, error)
	// Reload re-reads the folder and device tables.
	Reload(ctx context.Context) error
	Shutdown(ctx context.Context) error
}

var (
	_ API                  = (*Daemon)(nil)
	_ ports.ControlHandler = (*Daemon)(nil)
)

// Status reports the state of the device, including what only the running
// daemon knows.
func (d *Daemon) Status(ctx context.Context) (status.Report, error) {
	if d.deps.Status == nil {
		return status.Report{}, errors.New("status is not available")
	}
	return d.deps.Status.Report(ctx, d.live())
}

// Pause pauses a folder, or every folder when id is empty.
func (d *Daemon) Pause(ctx context.Context, id folder.ID) error {
	return d.setPaused(ctx, id, true)
}

// Resume resumes a folder, or every folder when id is empty, and queues a scan
// so it catches up with what changed while it was paused.
func (d *Daemon) Resume(ctx context.Context, id folder.ID) error {
	if err := d.setPaused(ctx, id, false); err != nil {
		return err
	}
	if err := d.resumeTransfers(ctx, id); err != nil {
		return err
	}
	return d.Rescan(ctx, id)
}

// resumeTransfers takes the transfers of the resumed folders out of the paused
// state, so the journal stops calling paused what the folder no longer is.
func (d *Daemon) resumeTransfers(ctx context.Context, id folder.ID) error {
	if d.deps.Syncer == nil {
		return nil
	}
	targets, err := d.targets(ctx, id)
	if err != nil {
		return err
	}
	for _, target := range targets {
		f, err := d.deps.Store.GetFolder(ctx, target)
		if err != nil {
			return err
		}
		if err := d.deps.Syncer.ResumeAll(ctx, f); err != nil {
			return err
		}
	}
	return nil
}

// setPaused writes the paused flag of one or every folder.
func (d *Daemon) setPaused(ctx context.Context, id folder.ID, paused bool) error {
	targets, err := d.targets(ctx, id)
	if err != nil {
		return err
	}
	for _, target := range targets {
		if err := d.deps.Store.SetFolderPaused(ctx, target, paused); err != nil {
			return err
		}
	}
	return nil
}

// Rescan queues a full scan of a folder, or of every folder when id is empty.
func (d *Daemon) Rescan(ctx context.Context, id folder.ID) error {
	targets, err := d.targets(ctx, id)
	if err != nil {
		return err
	}
	for _, target := range targets {
		loop, ok := d.loop(target)
		if !ok {
			continue
		}
		loop.requestRescan()
	}
	return nil
}

// Repair restores consistency between the database and the files on disk.
func (d *Daemon) Repair(ctx context.Context, id folder.ID, dryRun bool) (repair.Report, error) {
	if d.deps.Repair == nil {
		return repair.Report{}, errors.New("repair is not available")
	}
	if id == "" {
		return repair.Report{}, fmt.Errorf("repair needs a folder id: %w", ports.ErrNotFound)
	}
	if _, err := d.deps.Store.GetFolder(ctx, id); err != nil {
		return repair.Report{}, err
	}
	return d.deps.Repair.Run(ctx, id, dryRun)
}

// Reload re-reads the folder and device tables: it starts and stops folder
// loops, dials peers that were added, drops peers that were removed, and tells
// the peers that are connected which folders this device serves now. It ends
// with a sync cycle against every connected peer, so a folder that was just
// given another mode is acted on now rather than at the next sync tick. It is
// what the CLI calls after it has changed one of those tables, so a running
// daemon does not have to wait for its next rescan tick.
func (d *Daemon) Reload(ctx context.Context) error {
	if err := d.refresh(ctx); err != nil {
		return err
	}
	d.announceFolders(ctx)
	for _, loop := range d.peersSnapshot() {
		loop.requestSync()
	}
	return nil
}

// announceFolders tells every connected peer which folders this device serves
// now, so a peer that is already connected learns about a folder added, given
// another mode or removed without reconnecting.
func (d *Daemon) announceFolders(ctx context.Context) {
	if d.deps.Listener == nil || d.deps.Peers == nil {
		return
	}
	d.deps.Listener.AnnounceFolders(d.deps.Peers.Hello(ctx).Folders)
}

// Shutdown asks the daemon to stop. Run returns once every folder loop and the
// control server have stopped.
func (d *Daemon) Shutdown(_ context.Context) error {
	d.mu.Lock()
	cancel := d.cancel
	d.mu.Unlock()
	if cancel == nil {
		return errors.New("daemon is not running")
	}
	cancel()
	return nil
}

// targets resolves a folder id to the folders a command applies to. An empty
// id means every configured folder.
func (d *Daemon) targets(ctx context.Context, id folder.ID) ([]folder.ID, error) {
	if id != "" {
		if _, err := d.deps.Store.GetFolder(ctx, id); err != nil {
			return nil, err
		}
		return []folder.ID{id}, nil
	}

	all, err := d.deps.Store.ListFolders(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]folder.ID, 0, len(all))
	for _, f := range all {
		out = append(out, f.ID)
	}
	return out, nil
}

// Handle answers one control request. Every failure becomes a response with
// ok:false rather than a transport error, so the client can report the exit
// code the command deserves.
func (d *Daemon) Handle(ctx context.Context, req ports.ControlRequest) ports.ControlResponse {
	id := folder.ID(req.FolderID)
	switch req.Cmd {
	case CmdStatus:
		report, err := d.Status(ctx)
		return result(report, err)
	case CmdPause:
		return result(struct{}{}, d.Pause(ctx, id))
	case CmdResume:
		return result(struct{}{}, d.Resume(ctx, id))
	case CmdRescan:
		return result(struct{}{}, d.Rescan(ctx, id))
	case CmdRepair:
		report, err := d.Repair(ctx, id, req.DryRun)
		return result(report, err)
	case CmdReload:
		return result(struct{}{}, d.Reload(ctx))
	case CmdShutdown:
		return result(struct{}{}, d.Shutdown(ctx))
	default:
		return failure(fmt.Errorf("unknown command %q", req.Cmd), codeUsage)
	}
}

// result encodes a successful payload, or turns an error into a failure.
func result(payload any, err error) ports.ControlResponse {
	if err != nil {
		return failure(err, codeOf(err))
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return failure(fmt.Errorf("encode result: %w", err), codeError)
	}
	return ports.ControlResponse{OK: true, Result: encoded}
}

// failure builds an error response.
func failure(err error, code int) ports.ControlResponse {
	return ports.ControlResponse{OK: false, Error: err.Error(), Code: code}
}

// codeOf maps an error to the exit code the CLI should report.
func codeOf(err error) int {
	if errors.Is(err, ports.ErrNotFound) {
		return codeNotFound
	}
	return codeError
}
