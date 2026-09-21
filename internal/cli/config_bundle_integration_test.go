package cli_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wesbragagt/syncthis/internal/app/configbundle"
	"github.com/wesbragagt/syncthis/internal/cli"
	"github.com/wesbragagt/syncthis/internal/compose"
	"github.com/wesbragagt/syncthis/internal/domain/device"
)

// bundlePeerID is the peer the export tests write into the database directly:
// `peer add` needs a live peer, and the bundle only carries the row.
const bundlePeerID = device.ID("ETI3RRZAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA")

// savePeer writes a peer row into a data directory, as pairing would.
func savePeer(t *testing.T, e env) {
	t.Helper()
	ctx := context.Background()
	app, err := compose.Build(ctx, compose.Options{DataDir: e.dataDir})
	if err != nil {
		t.Fatalf("open %s: %v", e.dataDir, err)
	}
	defer func() { _ = app.Close() }()

	err = app.Store.SaveDevice(ctx, device.Device{
		ID: bundlePeerID, Name: "nas", Address: "10.0.0.2:22000",
		Verified: true, AddedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("save peer: %v", err)
	}
}

// exportBundle runs `config export --output FILE` and returns the file path.
func exportBundle(t *testing.T, e env) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "bundle.json")
	if code, _, stderr := e.run("", "config", "export", "--output", path); code != cli.ExitOK {
		t.Fatalf("config export exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	return path
}

// decodeBundleFile reads a bundle written to disk.
func decodeBundleFile(t *testing.T, path string) configbundle.Bundle {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer func() { _ = f.Close() }()
	b, err := configbundle.Decode(f)
	if err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	return b
}

// sourceEnv returns an initialized data directory with one folder, one peer
// and a changed setting, which is what an export has to carry.
func sourceEnv(t *testing.T) env {
	t.Helper()
	e := newEnv(t)
	setupFolder(t, e, "--ignore", "*.tmp", "--sync-window", "22:00-06:00")
	if code, _, stderr := e.run("", "config", "set", "--download-limit", "1MiB"); code != cli.ExitOK {
		t.Fatalf("config set exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	savePeer(t, e)
	return e
}

func TestConfigExportImport_RoundTripBetweenDataDirs(t *testing.T) {
	a := sourceEnv(t)
	file := exportBundle(t, a)

	c := newEnv(t)
	c.init(t)
	code, stdout, stderr := c.run("", "config", "import", file)
	if code != cli.ExitOK {
		t.Fatalf("config import exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	if !strings.Contains(stdout, "folder docs: added") || !strings.Contains(stdout, "settings: changed") {
		t.Fatalf("config import output = %q", stdout)
	}

	// Exporting the importing device gives the same document back.
	want := decodeBundleFile(t, file)
	got := decodeBundleFile(t, exportBundle(t, c))
	want.ExportedAt, got.ExportedAt = "", ""
	if mustMarshal(t, want) != mustMarshal(t, got) {
		t.Fatalf("the round trip changed the configuration:\n%s\n%s", mustMarshal(t, want), mustMarshal(t, got))
	}
	if len(got.Folders) != 1 || len(got.Folders[0].SyncWindows) != 1 ||
		len(got.Folders[0].IgnorePatterns) != 1 || len(got.Peers) != 1 {
		t.Fatalf("the imported configuration = %s", mustMarshal(t, got))
	}
}

func TestConfigImport_ConflictExit5ListsAll(t *testing.T) {
	a := sourceEnv(t)
	file := exportBundle(t, a)

	// The same data directory the bundle came from: every item collides.
	code, _, stderr := a.run("", "config", "import", file)
	if code != cli.ExitAlreadyExists {
		t.Fatalf("exit = %d, want %d (stderr: %s)", code, cli.ExitAlreadyExists, stderr)
	}
	if !strings.Contains(stderr, `folder "docs": id already configured`) {
		t.Fatalf("stderr = %q, want the folder conflict listed", stderr)
	}
	if !strings.Contains(stderr, "peer "+string(bundlePeerID)+": already configured") {
		t.Fatalf("stderr = %q, want the peer conflict listed", stderr)
	}
}

func TestConfigImport_ReplaceWithoutYesExit9(t *testing.T) {
	a := sourceEnv(t)
	file := exportBundle(t, a)

	code, _, _ := a.run("", "--no-input", "config", "import", file, "--replace")
	if code != cli.ExitConfirmationRequired {
		t.Fatalf("exit = %d, want %d", code, cli.ExitConfirmationRequired)
	}
}

func TestConfigImport_ReplaceWithYesOverwrites(t *testing.T) {
	a := sourceEnv(t)
	file := exportBundle(t, a)

	// The folder is changed after the export, so the import has something
	// to put back.
	if code, _, stderr := a.run("", "folder", "set", "docs", "--mode", "sendonly"); code != cli.ExitOK {
		t.Fatalf("folder set exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	code, stdout, stderr := a.run("", "config", "import", file, "--replace", "--yes")
	if code != cli.ExitOK {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	if !strings.Contains(stdout, "folder docs: replaced") {
		t.Fatalf("output = %q, want the folder reported as replaced", stdout)
	}
	if got := decodeBundleFile(t, exportBundle(t, a)); got.Folders[0].Mode != "sendreceive" {
		t.Fatalf("mode = %q, want the one the bundle carried", got.Folders[0].Mode)
	}
}

func TestConfigImport_DryRunExit10ThenExit0AfterImport(t *testing.T) {
	a := sourceEnv(t)
	file := exportBundle(t, a)

	c := newEnv(t)
	c.init(t)
	code, stdout, _ := c.run("", "config", "import", file, "--dry-run")
	if code != cli.ExitDryRun {
		t.Fatalf("dry run exit = %d, want %d", code, cli.ExitDryRun)
	}
	if !strings.Contains(stdout, "folder docs: added") {
		t.Fatalf("dry run output = %q", stdout)
	}
	if code, _, _ := c.run("", "folder", "list", "--json"); code != cli.ExitOK {
		t.Fatalf("folder list exit = %d", code)
	}

	if code, _, stderr := c.run("", "config", "import", file); code != cli.ExitOK {
		t.Fatalf("import exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	// Everything is in place now, so the same bundle would change nothing:
	// --replace finds the folder and the peer already as the bundle
	// describes them and reports neither, so the dry run exits 0.
	code, stdout, _ = c.run("", "config", "import", file, "--dry-run", "--replace", "--yes")
	if code != cli.ExitOK {
		t.Fatalf("second dry run exit = %d, want 0 (stdout: %s)", code, stdout)
	}
}

func TestConfigImport_UnknownFieldExit2(t *testing.T) {
	c := newEnv(t)
	c.init(t)
	path := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(path, []byte(`{"schema_version":1,"nonsense":true}`), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	if code, _, _ := c.run("", "config", "import", path); code != cli.ExitUsage {
		t.Fatalf("exit = %d, want %d", code, cli.ExitUsage)
	}
	if code, _, _ := c.run("", "config", "import", filepath.Join(t.TempDir(), "gone.json")); code != cli.ExitUsage {
		t.Fatalf("exit for a missing file = %d, want %d", code, cli.ExitUsage)
	}
}

func TestConfigImport_MissingDirectoryExit2(t *testing.T) {
	a := sourceEnv(t)
	file := exportBundle(t, a)

	bundle := decodeBundleFile(t, file)
	bundle.Folders[0].Path = filepath.Join(t.TempDir(), "not-there")
	rewritten := filepath.Join(t.TempDir(), "moved.json")
	f, err := os.Create(rewritten)
	if err != nil {
		t.Fatalf("create %s: %v", rewritten, err)
	}
	if err := configbundle.Encode(f, bundle); err != nil {
		t.Fatalf("encode: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	c := newEnv(t)
	c.init(t)
	if code, _, _ := c.run("", "config", "import", rewritten); code != cli.ExitUsage {
		t.Fatalf("exit = %d, want %d", code, cli.ExitUsage)
	}
}

func TestConfigExport_OutputFileIs0600(t *testing.T) {
	a := sourceEnv(t)
	file := exportBundle(t, a)
	info, err := os.Stat(file)
	if err != nil {
		t.Fatalf("stat %s: %v", file, err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600", info.Mode().Perm())
	}
}

func TestConfigExport_StdoutIsTheBundleWithAndWithoutJSON(t *testing.T) {
	a := sourceEnv(t)
	for _, args := range [][]string{{"config", "export"}, {"config", "export", "--json"}, {"config", "export", "--output", "-"}} {
		code, stdout, stderr := a.run("", args...)
		if code != cli.ExitOK {
			t.Fatalf("%v exit = %d (stderr: %s)", args, code, stderr)
		}
		b, err := configbundle.Decode(strings.NewReader(stdout))
		if err != nil {
			t.Fatalf("%v printed %q, which is not a bundle: %v", args, stdout, err)
		}
		if b.SchemaVersion != configbundle.SchemaVersion || len(b.Folders) != 1 {
			t.Fatalf("%v printed %s", args, mustMarshal(t, b))
		}
	}
}

// mustMarshal renders a value as indented JSON.
func mustMarshal(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(raw)
}

// writeBundle re-encodes a bundle into a new file and returns its path.
func writeBundle(t *testing.T, b configbundle.Bundle) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "rewritten.json")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create %s: %v", path, err)
	}
	if err := configbundle.Encode(f, b); err != nil {
		t.Fatalf("encode: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return path
}

func TestConfigImport_TwoFoldersSharingAPathExit2BeforeAnyWrite(t *testing.T) {
	a := sourceEnv(t)
	bundle := decodeBundleFile(t, exportBundle(t, a))
	bundle.Folders = append(bundle.Folders, configbundle.Folder{
		ID: "notes", Path: bundle.Folders[0].Path, Mode: "sendreceive",
		DeletePolicy: "propagate", Symlinks: "skip",
		IgnorePatterns: []string{}, SyncWindows: []string{},
	})
	file := writeBundle(t, bundle)

	c := newEnv(t)
	c.init(t)
	code, _, stderr := c.run("", "config", "import", file)
	if code != cli.ExitUsage {
		t.Fatalf("exit = %d, want %d (stderr: %s)", code, cli.ExitUsage, stderr)
	}
	for _, want := range []string{bundle.Folders[0].Path, `"docs"`, `"notes"`} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("stderr = %q, want it to name %s", stderr, want)
		}
	}
	if _, stdout, _ := c.run("", "folder", "list", "--json"); !strings.Contains(stdout, "[]") {
		t.Fatalf("folder list = %q, want no folder written", stdout)
	}
}

func TestConfigImport_SettingsTheCommandWouldRefuseExit2(t *testing.T) {
	a := sourceEnv(t)
	source := decodeBundleFile(t, exportBundle(t, a))

	cases := []struct {
		name  string
		apply func(*configbundle.Bundle)
		field string
	}{
		{"log level", func(b *configbundle.Bundle) { b.Settings.LogLevel = "loud" }, "log_level"},
		{"listen", func(b *configbundle.Bundle) { b.Settings.Listen = "not an address" }, "listen"},
		{"empty listen", func(b *configbundle.Bundle) { b.Settings.Listen = "" }, "listen"},
	}
	for _, tc := range cases {
		bundle := source
		tc.apply(&bundle)
		file := writeBundle(t, bundle)

		c := newEnv(t)
		c.init(t)
		code, _, stderr := c.run("", "config", "import", file)
		if code != cli.ExitUsage {
			t.Fatalf("%s: exit = %d, want %d (stderr: %s)", tc.name, code, cli.ExitUsage, stderr)
		}
		if !strings.Contains(stderr, tc.field) {
			t.Fatalf("%s: stderr = %q, want it to name %s", tc.name, stderr, tc.field)
		}
	}
}

func TestConfigExport_OutputOverALooserFileIs0600(t *testing.T) {
	a := sourceEnv(t)
	file := filepath.Join(t.TempDir(), "existing.json")
	if err := os.WriteFile(file, []byte("{}\n"), 0o644); err != nil {
		t.Fatalf("write %s: %v", file, err)
	}
	if code, _, stderr := a.run("", "config", "export", "--output", file); code != cli.ExitOK {
		t.Fatalf("export exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	info, err := os.Stat(file)
	if err != nil {
		t.Fatalf("stat %s: %v", file, err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600", info.Mode().Perm())
	}
}

func TestConfigImport_DryRunReplaceDoesNotAskToConfirm(t *testing.T) {
	a := sourceEnv(t)
	file := exportBundle(t, a)

	c := newEnv(t)
	c.init(t)
	// --no-input would fail any confirmation, and a dry run writes
	// nothing, so there is nothing to confirm.
	code, stdout, stderr := c.run("", "--no-input", "config", "import", file, "--dry-run", "--replace")
	if code != cli.ExitDryRun {
		t.Fatalf("exit = %d, want %d (stderr: %s)", code, cli.ExitDryRun, stderr)
	}
	if !strings.Contains(stdout, "folder docs: added") {
		t.Fatalf("output = %q, want the plan", stdout)
	}
	if strings.Contains(stdout, "[y/N]") {
		t.Fatalf("output = %q, want no prompt", stdout)
	}
}

func TestConfigImport_ConcurrentImportsOnlyOneAdds(t *testing.T) {
	a := sourceEnv(t)
	file := exportBundle(t, a)

	c := newEnv(t)
	c.init(t)

	type outcome struct {
		code   int
		stdout string
		stderr string
	}
	results := make([]outcome, 2)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			code, stdout, stderr := c.run("", "config", "import", file)
			results[i] = outcome{code, stdout, stderr}
		}()
	}
	close(start)
	wg.Wait()

	var added, conflicted int
	for _, r := range results {
		switch {
		case r.code == cli.ExitOK && strings.Contains(r.stdout, "folder docs: added"):
			added++
		case r.code == cli.ExitAlreadyExists:
			conflicted++
		}
	}
	if added != 1 || conflicted != 1 {
		t.Fatalf("outcomes = %+v, want one import that added and one that found conflicts", results)
	}
}
