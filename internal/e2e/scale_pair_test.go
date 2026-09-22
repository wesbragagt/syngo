package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/wesbragagt/syncthis/internal/adapters/jsonconfig"
	"github.com/wesbragagt/syncthis/internal/app/status"
	"github.com/wesbragagt/syncthis/internal/compose"
	"github.com/wesbragagt/syncthis/internal/domain/device"
	"github.com/wesbragagt/syncthis/internal/domain/folder"
)

// The scale transfer tests drive two real daemon processes rather than two
// daemons inside the test binary. Two things need that: a memory measurement
// of "the daemon" has to exclude the test that is generating a 10 GiB file,
// and an interruption has to be a SIGKILL, which is what leaves a half
// written partial file and a journal row behind. A cancelled goroutine would
// prove something weaker.

// scalePairConfig is how a pair is set up. Zero values take the defaults
// below.
type scalePairConfig struct {
	// ChunkSize is the transfer chunk size of both daemons.
	ChunkSize int
	// RescanInterval is how often each daemon rescans its folder.
	RescanInterval time.Duration
	// SyncInterval is how often the receiver plans a sync cycle.
	SyncInterval time.Duration
	// UploadLimit is the sender's byte budget, DownloadLimit the
	// receiver's, in bytes per second. Zero is unlimited.
	UploadLimit, DownloadLimit int64
}

// scaleNode is one daemon process with its own data directory and folder.
type scaleNode struct {
	t       *testing.T
	name    string
	dataDir string
	root    string
	id      device.ID
	listen  string
	child   *child
}

// startScalePair prepares a send-only sender and a receive-only receiver
// inside dir, makes them trust each other and starts both daemons.
func startScalePair(t *testing.T, dir string, cfg scalePairConfig) (sender, receiver *scaleNode) {
	t.Helper()
	short := scaleShortDir(t)
	sender = newScaleNode(t, dir, short, "a", folder.ModeSendOnly, cfg)
	receiver = newScaleNode(t, dir, short, "b", folder.ModeReceiveOnly, cfg)
	scalePair(t, sender, receiver)
	scalePair(t, receiver, sender)
	sender.Start()
	receiver.Start()
	return sender, receiver
}

// newScaleNode initializes a data directory, writes the configuration the
// pair needs and adds the folder. It does not start a daemon.
func newScaleNode(t *testing.T, dir, short, name string, mode folder.Mode, cfg scalePairConfig) *scaleNode {
	t.Helper()
	n := &scaleNode{
		t: t, name: name,
		dataDir: filepath.Join(short, name),
		root:    filepath.Join(dir, name, "root"),
		listen:  fmt.Sprintf("127.0.0.1:%d", freePort(t)),
	}
	if err := os.MkdirAll(n.root, 0o755); err != nil {
		t.Fatalf("create the folder root of %s: %v", name, err)
	}

	code, out := runBinary(t, n.dataDir, "init", "--name", name, "--json")
	if code != 0 {
		t.Fatalf("init %s exit = %d, want 0 (%s)", name, code, out)
	}
	var initialized struct {
		DeviceID string `json:"device_id"`
	}
	if err := json.Unmarshal([]byte(out), &initialized); err != nil {
		t.Fatalf("init %s output is not json: %v\n%s", name, err, out)
	}
	n.id = device.ID(initialized.DeviceID)

	config, err := jsonconfig.Load(n.dataDir)
	if err != nil {
		t.Fatalf("read the config of %s: %v", name, err)
	}
	config.Listen = n.listen
	if cfg.ChunkSize > 0 {
		config.ChunkSize = cfg.ChunkSize
	}
	if cfg.RescanInterval > 0 {
		config.RescanInterval = jsonconfig.Duration(cfg.RescanInterval)
	}
	if cfg.SyncInterval > 0 {
		config.SyncInterval = jsonconfig.Duration(cfg.SyncInterval)
	}
	if mode == folder.ModeSendOnly {
		config.UploadLimit = cfg.UploadLimit
	} else {
		config.DownloadLimit = cfg.DownloadLimit
	}
	if err := jsonconfig.Save(n.dataDir, config); err != nil {
		t.Fatalf("write the config of %s: %v", name, err)
	}

	code, out = runBinary(t, n.dataDir, "folder", "add", n.root,
		"--id", string(folderID), "--mode", string(mode), "--json")
	if code != 0 {
		t.Fatalf("folder add on %s exit = %d, want 0 (%s)", name, code, out)
	}
	return n
}

// scalePair makes one node trust another, in that direction only. It writes
// the device row directly, which is what `peer add` persists, so no pairing
// handshake has to be driven from a test.
func scalePair(t *testing.T, from, to *scaleNode) {
	t.Helper()
	app, err := compose.Build(context.Background(), compose.Options{DataDir: from.dataDir})
	if err != nil {
		t.Fatalf("open the data directory of %s: %v", from.name, err)
	}
	defer func() {
		if err := app.Close(); err != nil {
			t.Errorf("close the data directory of %s: %v", from.name, err)
		}
	}()
	err = app.Store.SaveDevice(context.Background(), device.Device{
		ID: to.id, Name: to.name, Address: to.listen, Verified: true, AddedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("pair %s with %s: %v", from.name, to.name, err)
	}
}

// Start runs the daemon and waits until it answers the control socket.
func (n *scaleNode) Start() {
	n.t.Helper()
	n.child = startChild(n.t, n.dataDir, "daemon", "run")
	waitFor(n.t, "the daemon of "+n.name+" to answer", daemonTimeout, func() bool {
		if n.child.finished() {
			n.t.Fatalf("the daemon of %s exited: %s", n.name, n.child.errorOutput())
		}
		return statusJSON(n.t, n.dataDir).Daemon.Running
	})
}

// Kill sends SIGKILL and waits for the process to be reaped. Nothing is
// cleaned up, which is the point: the next start finds the partial file, the
// journal row, the stale pid file and the stale control socket.
func (n *scaleNode) Kill() {
	n.t.Helper()
	if n.child == nil || n.child.finished() {
		return
	}
	if !n.child.signal(syscall.SIGKILL) {
		return
	}
	select {
	case <-n.child.done:
	case <-time.After(daemonTimeout):
		n.t.Fatalf("the daemon of %s did not die after SIGKILL", n.name)
	}
}

// PID is the process id of the running daemon.
func (n *scaleNode) PID() int { return n.child.cmd.Process.Pid }

// Status asks the node for its report over the control socket.
func (n *scaleNode) Status() status.Report { return statusJSON(n.t, n.dataDir) }

// partialFiles lists the partial downloads of the node.
func (n *scaleNode) partialFiles() []string { return partFiles(n.t, n.root) }

// freePort asks the kernel for a port nothing is listening on. A restarted
// node has to come back on the address its peer knows, so the port is chosen
// before the first start rather than by binding :0.
func freePort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("find a free port: %v", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatalf("release the port: %v", err)
	}
	return port
}
