package runner

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/wesbragagt/syncthis/internal/adapters/jsonconfig"
	"github.com/wesbragagt/syncthis/internal/app/ports"
	"github.com/wesbragagt/syncthis/internal/app/status"
	"github.com/wesbragagt/syncthis/internal/compose"
	"github.com/wesbragagt/syncthis/internal/domain/device"
	"github.com/wesbragagt/syncthis/internal/domain/folder"
)

// FolderID is the folder every run shares between its devices.
const FolderID = folder.ID("bench")

// node is one running device: its App and the daemon goroutine driving it.
type node struct {
	name    string
	app     *compose.App
	dataDir string
	root    string
	id      device.ID
	listen  string
	events  *byteCounter

	cancel context.CancelFunc
	done   chan error
}

// nodeOptions configure how a node's daemon is started.
type nodeOptions struct {
	Listen         string
	Interface      string
	RescanInterval time.Duration
	SyncInterval   time.Duration
	Debounce       time.Duration
	ReadyTimeout   time.Duration
}

// startNode initializes dataDir if needed and starts a daemon against it. The
// folder at root is added only when addFolder is true, so a node dialing into
// a folder it does not yet have configured can be paired first.
func startNode(ctx context.Context, name, dataDir, root string, o nodeOptions) (*node, error) {
	// A config.json already on disk means this data directory was started
	// by an earlier call, whose actual bound address was pinned back into
	// it; a fresh data directory always asks for an ephemeral port; the
	// alternative, jsonconfig.Load's fixed default of 127.0.0.1:22000,
	// would make every node started without an explicit --data-dir collide
	// on the same port.
	restarting := configExists(dataDir)
	cfg, err := jsonconfig.Load(dataDir)
	if err != nil {
		return nil, fmt.Errorf("load config for %s: %w", name, err)
	}
	listen := o.Listen
	if listen == "" && restarting {
		listen = cfg.Listen
	}
	cfg.Listen = pick(listen, "127.0.0.1:0")
	cfg.RescanInterval = jsonconfig.Duration(pickDuration(o.RescanInterval, 10*time.Second))
	cfg.SyncInterval = jsonconfig.Duration(pickDuration(o.SyncInterval, 2*time.Second))
	if err := jsonconfig.Save(dataDir, cfg); err != nil {
		return nil, fmt.Errorf("write config for %s: %w", name, err)
	}

	events := newByteCounter()
	app, _, err := compose.Init(ctx, compose.Options{
		DataDir: dataDir,
		Events:  events,
		Logger:  slog.New(slog.DiscardHandler),
	}, compose.InitSettings{DeviceName: name, Listen: cfg.Listen})
	if err != nil {
		return nil, fmt.Errorf("init %s: %w", name, err)
	}

	n := &node{name: name, app: app, dataDir: dataDir, root: root, id: app.Identity.DeviceID(), events: events}

	ctx, cancel := context.WithCancel(ctx)
	n.cancel = cancel
	n.done = make(chan error, 1)
	ready := make(chan struct{})
	go func() {
		n.done <- app.RunDaemon(ctx, compose.DaemonOptions{
			Listen:         o.Listen,
			Interface:      o.Interface,
			Debounce:       pickDuration(o.Debounce, 200*time.Millisecond),
			RescanInterval: pickDuration(o.RescanInterval, 10*time.Second),
			SyncInterval:   pickDuration(o.SyncInterval, 2*time.Second),
			Ready:          ready,
		})
	}()

	timeout := o.ReadyTimeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	select {
	case <-ready:
	case err := <-n.done:
		cancel()
		return nil, fmt.Errorf("daemon %s stopped before it was ready: %w", name, err)
	case <-time.After(timeout):
		cancel()
		return nil, fmt.Errorf("daemon %s did not start within %s", name, timeout)
	}

	report, err := n.app.Control.Status(ctx)
	if err != nil {
		n.stop()
		return nil, fmt.Errorf("status of %s: %w", name, err)
	}
	n.listen = report.Daemon.Listen
	if n.listen == "" {
		n.stop()
		return nil, fmt.Errorf("daemon %s reports no listen address", name)
	}
	// Pin the address actually bound (an ephemeral "127.0.0.1:0" resolves
	// to a random port) so a later restart of this data directory keeps the
	// address its peers already store.
	cfg.Listen = n.listen
	if err := jsonconfig.Save(dataDir, cfg); err != nil {
		n.stop()
		return nil, fmt.Errorf("pin listen address for %s: %w", name, err)
	}
	return n, nil
}

// addFolder shares root as FolderID, sendreceive, propagate deletions.
func (n *node) addFolder(ctx context.Context, root string) error {
	_, err := n.app.Folders.Add(ctx, folder.Folder{
		ID: FolderID, Path: root, Mode: folder.ModeSendReceive,
		DeletePolicy: folder.DeletePropagate, Symlinks: folder.SymlinkStore, KeepEmptyDirs: true,
	})
	if err != nil && !errors.Is(err, ports.ErrAlreadyExists) {
		return fmt.Errorf("add folder on %s: %w", n.name, err)
	}
	return nil
}

// trust makes n treat peer as a known, verified device, the effect `peer add`
// has once both sides have run it. It is used for the lan scenario, where
// both device IDs are already known to the runner.
func (n *node) trust(ctx context.Context, peer *node) error {
	return n.app.Store.SaveDevice(ctx, device.Device{
		ID: peer.id, Name: peer.name, Address: peer.listen,
		Verified: true, AddedAt: time.Now(),
	})
}

// status returns the running daemon's report.
func (n *node) status(ctx context.Context) (status.Report, error) {
	return n.app.Control.Status(ctx)
}

// idle reports whether n has nothing active or pending to transfer.
func (n *node) idle(ctx context.Context) (bool, error) {
	report, err := n.status(ctx)
	if err != nil {
		return false, err
	}
	return report.Transfers.Active == 0 && report.Transfers.Pending == 0, nil
}

// stop cancels the daemon and closes the store. It is safe to call more than
// once.
func (n *node) stop() {
	if n.cancel == nil {
		return
	}
	n.cancel()
	select {
	case <-n.done:
	case <-time.After(30 * time.Second):
	}
	n.cancel = nil
	_ = n.app.Close()
}

// configExists reports whether dataDir already holds a config.json, meaning a
// node has run there before.
func configExists(dataDir string) bool {
	_, err := os.Stat(filepath.Join(dataDir, jsonconfig.FileName))
	return err == nil
}

func pick(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

func pickDuration(v, fallback time.Duration) time.Duration {
	if v == 0 {
		return fallback
	}
	return v
}
