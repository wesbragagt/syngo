// Package e2e runs two daemons in one process, talking to each other over
// loopback, and checks that files converge.
package e2e

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/wesbragagt/syncthis/internal/adapters/blake3hash"
	"github.com/wesbragagt/syncthis/internal/adapters/jsonconfig"
	"github.com/wesbragagt/syncthis/internal/app/fakes"
	"github.com/wesbragagt/syncthis/internal/app/status"
	"github.com/wesbragagt/syncthis/internal/compose"
	"github.com/wesbragagt/syncthis/internal/domain/device"
	"github.com/wesbragagt/syncthis/internal/domain/folder"
	"github.com/wesbragagt/syncthis/internal/domain/manifest"
)

// Test timings. They are short so a cycle happens within a test, and long
// enough that a loaded machine still makes progress. The waiting times are
// multiplied by timeoutScale, which the race build raises: under the race
// detector, and with the other packages of the tree being tested at the same
// time, a folder takes noticeably longer to converge.
const (
	testChunkSize      = 16 << 10
	testRescanInterval = 300 * time.Millisecond
	testSyncInterval   = 200 * time.Millisecond
	testDebounce       = 50 * time.Millisecond
	convergeTimeout    = 60 * time.Second * timeoutScale
	daemonTimeout      = 30 * time.Second * timeoutScale
	folderID           = folder.ID("docs")
	// extraID is the second folder some tests configure. It is always
	// sendreceive on both sides, so a file put in it and waited for proves
	// that a sync cycle ran, which is what a test needs before it can say
	// that something else did not happen.
	extraID = folder.ID("extra")
)

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
		DataDir: t.TempDir(), Root: t.TempDir(),
	}

	config := jsonconfig.Default()
	config.ChunkSize = testChunkSize
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
	go func() {
		n.done <- n.App.RunDaemon(ctx, compose.DaemonOptions{
			Listen:         listen,
			Debounce:       testDebounce,
			RescanInterval: n.rescan,
			SyncInterval:   testSyncInterval,
			Ready:          ready,
		})
	}()

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

// saveDevice makes one node trust another, in that direction only.
func saveDevice(t *testing.T, from, to *node) {
	t.Helper()
	err := from.App.Store.SaveDevice(context.Background(), device.Device{
		ID: to.ID, Name: to.name, Address: to.Listen,
		Verified: true, AddedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("pair %s with %s: %v", from.name, to.name, err)
	}
}

// treeEntry is what a comparison knows about one path of a folder.
type treeEntry struct {
	Path    string
	Kind    manifest.Kind
	Size    int64
	Mode    fs.FileMode
	Hash    string
	Target  string
	ModTime time.Time
}

// readTree walks a folder root and describes everything in it, skipping the
// state directory.
func readTree(t *testing.T, root string) map[string]treeEntry {
	t.Helper()
	hasher := blake3hash.New()
	out := make(map[string]treeEntry)

	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		if rel == ".syncthis" {
			return filepath.SkipDir
		}

		info, err := d.Info()
		if err != nil {
			return err
		}
		entry := treeEntry{Path: rel, Mode: info.Mode().Perm(), ModTime: info.ModTime()}
		switch {
		case info.Mode()&fs.ModeSymlink != 0:
			entry.Kind = manifest.KindSymlink
			if entry.Target, err = os.Readlink(path); err != nil {
				return err
			}
		case info.IsDir():
			entry.Kind = manifest.KindDir
		default:
			entry.Kind = manifest.KindFile
			entry.Size = info.Size()
			f, err := os.Open(path)
			if err != nil {
				return err
			}
			hash, err := hasher.Sum(f)
			_ = f.Close()
			if err != nil {
				return err
			}
			entry.Hash = hash.String()
		}
		out[rel] = entry
		return nil
	})
	if err != nil {
		t.Fatalf("read tree %s: %v", root, err)
	}
	return out
}

// treeDiff describes how two folder trees differ, or is empty when they hold
// the same content.
func treeDiff(t *testing.T, wantRoot, gotRoot string) []string {
	t.Helper()
	want, got := readTree(t, wantRoot), readTree(t, gotRoot)

	var problems []string
	for path, w := range want {
		g, ok := got[path]
		if !ok {
			problems = append(problems, "missing: "+path)
			continue
		}
		switch {
		case w.Kind != g.Kind:
			problems = append(problems, fmt.Sprintf("%s: kind %s, want %s", path, g.Kind, w.Kind))
		case w.Hash != g.Hash:
			problems = append(problems, fmt.Sprintf("%s: hash %s, want %s", path, g.Hash, w.Hash))
		case w.Target != g.Target:
			problems = append(problems, fmt.Sprintf("%s: link to %s, want %s", path, g.Target, w.Target))
		case w.Kind != manifest.KindSymlink && w.Mode != g.Mode:
			problems = append(problems, fmt.Sprintf("%s: mode %v, want %v", path, g.Mode, w.Mode))
		case w.Kind == manifest.KindFile && absDuration(w.ModTime.Sub(g.ModTime)) > time.Second:
			problems = append(problems, fmt.Sprintf("%s: mtime %s, want %s", path, g.ModTime, w.ModTime))
		}
	}
	for path := range got {
		if _, ok := want[path]; !ok {
			problems = append(problems, "unexpected: "+path)
		}
	}
	sort.Strings(problems)
	return problems
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

// payload returns n bytes of content derived from the seed.
func payload(n int, seed byte) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = byte(i*31) ^ seed
	}
	return out
}

// absDuration returns the absolute value of d.
func absDuration(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

// join concatenates lines for a failure message.
func join(lines []string) string {
	out := ""
	for _, line := range lines {
		out += "  " + line + "\n"
	}
	return out
}
