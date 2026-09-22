// Package e2e runs two daemons in one process, talking to each other over
// loopback, and checks that files converge.
package e2e

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/wesbragagt/syncthis/internal/adapters/jsonconfig"
	"github.com/wesbragagt/syncthis/internal/adapters/tcpwire"
	"github.com/wesbragagt/syncthis/internal/app/fakes"
	"github.com/wesbragagt/syncthis/internal/app/status"
	"github.com/wesbragagt/syncthis/internal/compose"
	"github.com/wesbragagt/syncthis/internal/domain/bandwidth"
	"github.com/wesbragagt/syncthis/internal/domain/device"
	"github.com/wesbragagt/syncthis/internal/domain/folder"
	"github.com/wesbragagt/syncthis/internal/testutil/tree"
)

// Test timings. They are short so a cycle happens within a test, and long
// enough that a loaded machine still makes progress. The waiting times are
// multiplied by timeoutScale, which the race build raises on its own and
// SYNCTHIS_E2E_TIMEOUT_SCALE raises further: a machine running something else
// at the same time can need several times longer for a folder to converge.
const (
	testChunkSize      = 16 << 10
	testRescanInterval = 300 * time.Millisecond
	testSyncInterval   = 200 * time.Millisecond
	testDebounce       = 50 * time.Millisecond
	// testHoldCheck is how often a recoverable listener checks that the
	// machine still has the address it is bound to.
	testHoldCheck = 20 * time.Millisecond
	folderID      = folder.ID("docs")
	// extraID is the second folder some tests configure. It is always
	// sendreceive on both sides, so a file put in it and waited for proves
	// that a sync cycle ran, which is what a test needs before it can say
	// that something else did not happen.
	extraID = folder.ID("extra")
)

// Waiting times. They are variables rather than constants because the scale
// they are multiplied by is read from the environment.
var (
	convergeTimeout = 60 * time.Second * timeoutScale
	daemonTimeout   = 30 * time.Second * timeoutScale
)

// timeoutScale multiplies every waiting time: what this build needs, times
// what SYNCTHIS_E2E_TIMEOUT_SCALE asks for on top of it.
var timeoutScale = buildTimeoutScale * envTimeoutScale()

// envTimeoutScale reads SYNCTHIS_E2E_TIMEOUT_SCALE, a whole number of times to
// stretch every waiting time by, for a machine too loaded to keep up with the
// defaults. Anything unset, unreadable or below one means no stretching: a
// scale is only ever allowed to give the tests more room, never less.
func envTimeoutScale() time.Duration {
	scale, err := strconv.Atoi(os.Getenv("SYNCTHIS_E2E_TIMEOUT_SCALE"))
	if err != nil || scale < 1 {
		return 1
	}
	return time.Duration(scale)
}

// node is one daemon with its own data directory and synchronized folder.
type node struct {
	t             *testing.T
	name          string
	mode          folder.Mode
	Events        *fakes.Events
	App           *compose.App
	DataDir, Root string
	// Extra is the root of the second folder, empty unless the node was
	// configured with one.
	Extra  string
	ID     device.ID
	Listen string
	rescan time.Duration
	// recoverable makes the node bind its peer port through a binder whose
	// address the test can take away and give back.
	recoverable bool
	// binder is that binder, replaced on every start.
	binder *flappingBinder

	cancel context.CancelFunc
	done   chan error
}

// nodeConfig is how a node and its folder are set up. A zero DeletePolicy
// means the default, propagate, and a zero RescanInterval means the short one
// the tests share.
type nodeConfig struct {
	Mode         folder.Mode
	DeletePolicy folder.DeletePolicy
	// NoFolder starts the daemon with nothing configured, so a test can
	// add the folder through the CLI while the daemon runs.
	NoFolder bool
	// ExtraFolder adds a second, always sendreceive folder to the node.
	ExtraFolder bool
	// RescanInterval overrides how often the node re-reads its tables. A
	// long one proves that something other than the tick did the work.
	RescanInterval time.Duration
	// ChunkSize overrides the chunk size of the node; zero takes the small
	// one the other tests share.
	ChunkSize int
	// UploadLimit and DownloadLimit are bytes per second, 0 for unlimited.
	UploadLimit, DownloadLimit bandwidth.Rate
	// RecoverableListener binds the peer port through a binder whose
	// address the test can take away, so a Tailscale client restarting
	// under the daemon can be reproduced without a tailnet.
	RecoverableListener bool
}

// startNode initializes a data directory, adds one folder in the given mode
// and runs a daemon against it until the test ends.
func startNode(t *testing.T, name string, mode folder.Mode) *node {
	t.Helper()
	return startNodeWith(t, name, nodeConfig{Mode: mode})
}

// startNodeWith initializes a data directory, adds one folder and runs a
// daemon against it until the test ends.
func startNodeWith(t *testing.T, name string, cfg nodeConfig) *node {
	t.Helper()
	if cfg.DeletePolicy == "" {
		cfg.DeletePolicy = folder.DeletePropagate
	}
	if cfg.RescanInterval == 0 {
		cfg.RescanInterval = testRescanInterval
	}
	n := &node{
		t: t, name: name, mode: cfg.Mode, rescan: cfg.RescanInterval,
		DataDir: t.TempDir(), Root: t.TempDir(), recoverable: cfg.RecoverableListener,
	}

	config := jsonconfig.Default()
	config.ChunkSize = testChunkSize
	if cfg.ChunkSize > 0 {
		config.ChunkSize = cfg.ChunkSize
	}
	config.UploadLimit = int64(cfg.UploadLimit)
	config.DownloadLimit = int64(cfg.DownloadLimit)
	config.Listen = "127.0.0.1:0"
	config.RescanInterval = jsonconfig.Duration(cfg.RescanInterval)
	config.SyncInterval = jsonconfig.Duration(testSyncInterval)
	if err := jsonconfig.Save(n.DataDir, config); err != nil {
		t.Fatalf("write config: %v", err)
	}

	app, _, err := compose.Init(context.Background(), n.options(), compose.InitSettings{DeviceName: name})
	if err != nil {
		t.Fatalf("init %s: %v", name, err)
	}
	n.App = app
	n.ID = app.Identity.DeviceID()

	if !cfg.NoFolder {
		n.addFolder(t, folderID, n.Root, cfg.Mode, cfg.DeletePolicy)
	}
	if cfg.ExtraFolder {
		n.Extra = t.TempDir()
		n.addFolder(t, extraID, n.Extra, folder.ModeSendReceive, folder.DeletePropagate)
	}

	n.run()
	t.Cleanup(n.Stop)
	return n
}

// addFolder configures one folder of the node, before the daemon starts.
func (n *node) addFolder(t *testing.T, id folder.ID, root string, mode folder.Mode, policy folder.DeletePolicy) {
	t.Helper()
	_, err := n.App.Folders.Add(context.Background(), folder.Folder{
		ID: id, Path: root, Mode: mode, DeletePolicy: policy,
		Symlinks: folder.SymlinkStore, KeepEmptyDirs: true,
	})
	if err != nil {
		t.Fatalf("add folder %s on %s: %v", id, n.name, err)
	}
}

// options are the composition options of the node, with a fresh event
// recorder each time it starts.
func (n *node) options() compose.Options {
	n.Events = fakes.NewEvents()
	level := slog.LevelError
	if testing.Verbose() {
		level = slog.LevelDebug
	}
	return compose.Options{
		DataDir: n.DataDir,
		Events:  n.Events,
		Logger: slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})).
			With("node", n.name),
	}
}

// run starts the daemon and waits until it answers.
func (n *node) run() {
	n.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	n.cancel = cancel
	n.done = make(chan error, 1)

	// A restarted node keeps the address its peers know it by.
	listen := n.Listen
	if listen == "" {
		listen = "127.0.0.1:0"
	}
	ready := make(chan struct{})
	options := compose.DaemonOptions{
		Listen:         listen,
		Debounce:       testDebounce,
		RescanInterval: n.rescan,
		SyncInterval:   testSyncInterval,
		Ready:          ready,
	}
	if n.recoverable {
		n.binder = newFlappingBinder(tcpwire.NewBinder(
			n.App.Identity.TLSCertificate(), listen, "", false, n.App.Logger))
		options.Binder = n.binder
		options.HoldCheck = testHoldCheck
	}
	go func() { n.done <- n.App.RunDaemon(ctx, options) }()

	select {
	case <-ready:
	case err := <-n.done:
		n.t.Fatalf("daemon %s stopped before it was ready: %v", n.name, err)
	case <-time.After(daemonTimeout):
		n.t.Fatalf("daemon %s did not start", n.name)
	}
	n.Listen = n.Status().Daemon.Listen
	if n.Listen == "" {
		n.t.Fatalf("daemon %s reports no listen address", n.name)
	}
}

// Stop cancels the daemon and closes the database. It is safe to call twice.
func (n *node) Stop() {
	if n.cancel == nil {
		return
	}
	n.cancel()
	select {
	case err := <-n.done:
		if err != nil {
			n.t.Errorf("daemon %s returned %v", n.name, err)
		}
	case <-time.After(daemonTimeout):
		n.t.Fatalf("daemon %s did not stop", n.name)
	}
	n.cancel = nil
	if err := n.App.Close(); err != nil {
		n.t.Errorf("close %s: %v", n.name, err)
	}
}

// Restart stops the node and starts it again from the same data directory,
// with a fresh event recorder.
func (n *node) Restart() {
	n.t.Helper()
	n.Stop()

	app, err := compose.Build(context.Background(), n.options())
	if err != nil {
		n.t.Fatalf("rebuild %s: %v", n.name, err)
	}
	n.App = app
	n.run()
}

// Status asks the running daemon for its report.
func (n *node) Status() status.Report {
	n.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), daemonTimeout)
	defer cancel()

	report, err := n.App.Control.Status(ctx)
	if err != nil {
		n.t.Fatalf("status of %s: %v", n.name, err)
	}
	return report
}

// pair makes two nodes known to each other, which is what `peer add` writes.
func pair(t *testing.T, a, b *node) {
	t.Helper()
	saveDevice(t, a, b)
	saveDevice(t, b, a)
}

// saveDevice makes one node trust another, in that direction only, and
// authorizes it for every folder the node has now. Trusting a device and
// sharing everything with it are two decisions, but these tests only ever want
// both: a test about one folder's authorization grants what it means itself.
func saveDevice(t *testing.T, from, to *node) {
	t.Helper()
	trust(t, from, to)
	grantAll(t, from, to.ID)
}

// trust makes one node know another without authorizing it for any folder,
// which is what `peer add` alone does.
func trust(t *testing.T, from, to *node) {
	t.Helper()
	err := from.App.Store.SaveDevice(context.Background(), device.Device{
		ID: to.ID, Name: to.name, Address: to.Listen,
		Verified: true, AddedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("pair %s with %s: %v", from.name, to.name, err)
	}
}

// grant authorizes one device for one folder of a node.
func grant(t *testing.T, on *node, id folder.ID, dev device.ID) {
	t.Helper()
	if err := on.App.Folders.Grant(context.Background(), id, dev); err != nil {
		t.Fatalf("grant %s on folder %s of %s: %v", dev.Short(), id, on.name, err)
	}
}

// grantAll authorizes one device for every folder a node has configured.
func grantAll(t *testing.T, on *node, dev device.ID) {
	t.Helper()
	ctx := context.Background()
	configured, err := on.App.Folders.List(ctx)
	if err != nil {
		t.Fatalf("list folders of %s: %v", on.name, err)
	}
	for _, f := range configured {
		grant(t, on, f.ID, dev)
	}
}

// readTree walks a folder root and describes everything in it.
func readTree(t *testing.T, root string) map[string]tree.Entry {
	t.Helper()
	entries, err := tree.Read(root)
	if err != nil {
		t.Fatalf("read tree %s: %v", root, err)
	}
	return entries
}

// treeDiff describes how two folder trees differ, or is empty when they hold
// the same content.
func treeDiff(t *testing.T, wantRoot, gotRoot string) []string {
	t.Helper()
	return tree.Diff(readTree(t, wantRoot), readTree(t, gotRoot))
}

// waitTreeEqual waits until the folder of got holds exactly what want holds.
func waitTreeEqual(t *testing.T, want, got *node) {
	t.Helper()
	deadline := time.Now().Add(convergeTimeout)
	var problems []string
	for time.Now().Before(deadline) {
		if problems = treeDiff(t, want.Root, got.Root); len(problems) == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(problems) > 5 {
		problems = append(problems[:5], fmt.Sprintf("and %d more", len(problems)-5))
	}
	t.Fatalf("%s did not converge with %s:\n%s", got.name, want.name, join(problems))
}

// waitFor polls until the condition holds or the timeout expires.
func waitFor(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// writeFile creates a file inside a node's folder, with its parents.
func writeFile(t *testing.T, n *node, rel string, data []byte) {
	t.Helper()
	writeFileIn(t, n.Root, rel, data)
}

// writeFileIn creates a file inside a folder root, with its parents.
func writeFileIn(t *testing.T, root, rel string, data []byte) {
	t.Helper()
	full := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("create parent of %s: %v", rel, err)
	}
	if err := os.WriteFile(full, data, 0o644); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
}

// payload returns n deterministic bytes derived from the seed.
//
// The bytes come from a splitmix64 recurrence rather than a counter, because
// chunking is content-defined: a sequence that repeats every 256 bytes gives
// the rolling hash the same window over and over, so it cuts nowhere and every
// chunk is truncated at the maximum size. A boundary that comes from a length
// rather than from the content is exactly what an insertion shifts, so a
// fixture built that way would not deduplicate. Pseudorandom bytes cut near the
// configured average, which is what a real file does.
func payload(n int, seed byte) []byte {
	out := make([]byte, n)
	state := uint64(seed) + 1
	for i := range out {
		state += 0x9e3779b97f4a7c15
		z := state
		z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
		z = (z ^ (z >> 27)) * 0x94d049bb133111eb
		out[i] = byte(z ^ (z >> 31))
	}
	return out
}

// join concatenates lines for a failure message.
func join(lines []string) string {
	out := ""
	for _, line := range lines {
		out += "  " + line + "\n"
	}
	return out
}
