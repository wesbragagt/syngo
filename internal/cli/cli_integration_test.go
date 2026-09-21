package cli_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wesbragagt/syncthis/internal/cli"
)

// env is a temporary data directory plus a folder root to synchronize.
type env struct {
	dataDir    string
	folderPath string
}

// newEnv returns an uninitialized data directory and an existing folder root.
func newEnv(t *testing.T) env {
	t.Helper()
	root := t.TempDir()
	folderPath := filepath.Join(root, "docs")
	if err := os.MkdirAll(folderPath, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", folderPath, err)
	}
	return env{dataDir: filepath.Join(root, "data"), folderPath: folderPath}
}

// run executes the CLI against the environment's data directory.
func (e env) run(stdin string, args ...string) (int, string, string) {
	var stdout, stderr bytes.Buffer
	full := append([]string{"--data-dir", e.dataDir}, args...)
	code := cli.Execute(full, strings.NewReader(stdin), &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

// init runs `syncthis init` and fails the test if it does not succeed.
func (e env) init(t *testing.T) {
	t.Helper()
	if code, _, stderr := e.run("", "init", "--name", "testdev"); code != cli.ExitOK {
		t.Fatalf("init exit = %d, want 0 (stderr: %s)", code, stderr)
	}
}

func TestInit_ThenStatusJSON(t *testing.T) {
	e := newEnv(t)

	code, stdout, stderr := e.run("", "init", "--name", "testdev")
	if code != cli.ExitOK {
		t.Fatalf("init exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	if !strings.Contains(stdout, "device id:") {
		t.Fatalf("init output = %q", stdout)
	}
	for _, name := range []string{"identity.key", "identity.crt", "config.json", "state.db"} {
		if _, err := os.Stat(filepath.Join(e.dataDir, name)); err != nil {
			t.Fatalf("%s missing after init: %v", name, err)
		}
	}

	code, stdout, stderr = e.run("", "status", "--json")
	if code != cli.ExitOK {
		t.Fatalf("status exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	var report struct {
		SchemaVersion int `json:"schema_version"`
		Device        struct {
			ID      string `json:"id"`
			Name    string `json:"name"`
			DataDir string `json:"data_dir"`
		} `json:"device"`
		Daemon struct {
			Running bool `json:"running"`
		} `json:"daemon"`
		Folders []any `json:"folders"`
	}
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatalf("status output is not json: %v\n%s", err, stdout)
	}
	if report.SchemaVersion != 1 {
		t.Fatalf("schema_version = %d, want 1", report.SchemaVersion)
	}
	if len(report.Device.ID) != 52 {
		t.Fatalf("device id = %q, want 52 characters", report.Device.ID)
	}
	if report.Device.Name != "testdev" || report.Device.DataDir != e.dataDir {
		t.Fatalf("device = %+v", report.Device)
	}
	if report.Daemon.Running {
		t.Fatal("daemon.running = true, want false without a daemon")
	}
	if len(report.Folders) != 0 {
		t.Fatalf("folders = %v, want none", report.Folders)
	}
}

func TestInit_IsIdempotent(t *testing.T) {
	e := newEnv(t)
	e.init(t)

	code, first, _ := e.run("", "init", "--json")
	if code != cli.ExitOK {
		t.Fatalf("second init exit = %d, want 0", code)
	}
	var result struct {
		DeviceID string `json:"device_id"`
		Name     string `json:"name"`
		Created  bool   `json:"created"`
	}
	if err := json.Unmarshal([]byte(first), &result); err != nil {
		t.Fatalf("init output is not json: %v\n%s", err, first)
	}
	if result.Created {
		t.Fatal("created = true on the second init")
	}
	if result.Name != "testdev" {
		t.Fatalf("name = %q, want the name from the first init", result.Name)
	}

	info, err := os.Stat(filepath.Join(e.dataDir, "identity.key"))
	if err != nil {
		t.Fatalf("stat identity.key: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("identity.key mode = %v, want 0600", info.Mode().Perm())
	}
}

func TestFolderAddListRemove_Text(t *testing.T) {
	e := newEnv(t)
	e.init(t)

	code, stdout, stderr := e.run("", "folder", "add", e.folderPath, "--id", "docs", "--label", "Docs")
	if code != cli.ExitOK {
		t.Fatalf("folder add exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	if !strings.Contains(stdout, "added folder docs") {
		t.Fatalf("folder add output = %q", stdout)
	}

	code, stdout, stderr = e.run("", "folder", "list")
	if code != cli.ExitOK {
		t.Fatalf("folder list exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	if !strings.Contains(stdout, "docs") || !strings.Contains(stdout, e.folderPath) ||
		!strings.Contains(stdout, "sendreceive") {
		t.Fatalf("folder list output = %q", stdout)
	}

	code, stdout, stderr = e.run("", "folder", "remove", "docs", "--yes")
	if code != cli.ExitOK {
		t.Fatalf("folder remove exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	if !strings.Contains(stdout, "removed folder docs") {
		t.Fatalf("folder remove output = %q", stdout)
	}

	code, stdout, _ = e.run("", "folder", "list")
	if code != cli.ExitOK || !strings.Contains(stdout, "no folders configured") {
		t.Fatalf("folder list after removal = %d, %q", code, stdout)
	}
}

func TestFolderAdd_JSONRoundTrip(t *testing.T) {
	e := newEnv(t)
	e.init(t)

	code, _, stderr := e.run("", "folder", "add", e.folderPath, "--id", "docs",
		"--mode", "sendonly", "--symlinks", "store", "--include-hidden", "--no-empty-dirs",
		"--ignore", "*.tmp", "--ignore", "!keep.tmp")
	if code != cli.ExitOK {
		t.Fatalf("folder add exit = %d, want 0 (stderr: %s)", code, stderr)
	}

	code, stdout, stderr := e.run("", "--json", "folder", "list")
	if code != cli.ExitOK {
		t.Fatalf("folder list exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	var list []struct {
		ID             string   `json:"id"`
		Path           string   `json:"path"`
		Mode           string   `json:"mode"`
		Symlinks       string   `json:"symlinks"`
		IncludeHidden  bool     `json:"include_hidden"`
		KeepEmptyDirs  bool     `json:"keep_empty_dirs"`
		IgnorePatterns []string `json:"ignore_patterns"`
	}
	if err := json.Unmarshal([]byte(stdout), &list); err != nil {
		t.Fatalf("folder list output is not json: %v\n%s", err, stdout)
	}
	if len(list) != 1 {
		t.Fatalf("folders = %+v, want one", list)
	}
	f := list[0]
	if f.ID != "docs" || f.Path != e.folderPath || f.Mode != "sendonly" || f.Symlinks != "store" ||
		!f.IncludeHidden || f.KeepEmptyDirs {
		t.Fatalf("folder = %+v", f)
	}
	if len(f.IgnorePatterns) != 2 || f.IgnorePatterns[0] != "*.tmp" || f.IgnorePatterns[1] != "!keep.tmp" {
		t.Fatalf("ignore patterns = %v", f.IgnorePatterns)
	}
}

func TestFolderAdd_DuplicateExit5(t *testing.T) {
	e := newEnv(t)
	e.init(t)
	if code, _, stderr := e.run("", "folder", "add", e.folderPath, "--id", "docs"); code != cli.ExitOK {
		t.Fatalf("folder add exit = %d, want 0 (stderr: %s)", code, stderr)
	}

	other := filepath.Join(t.TempDir(), "other")
	if err := os.MkdirAll(other, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	tests := []struct {
		name string
		args []string
	}{
		{"same id", []string{"folder", "add", other, "--id", "docs"}},
		{"same path", []string{"folder", "add", e.folderPath, "--id", "other"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, _, stderr := e.run("", tt.args...)
			if code != cli.ExitAlreadyExists {
				t.Fatalf("exit = %d, want %d (stderr: %s)", code, cli.ExitAlreadyExists, stderr)
			}
		})
	}
}

func TestFolderAdd_MissingDirAndBadFlags(t *testing.T) {
	e := newEnv(t)
	e.init(t)

	tests := []struct {
		name string
		args []string
		want int
	}{
		{"no id", []string{"folder", "add", e.folderPath}, cli.ExitUsage},
		{"bad id", []string{"folder", "add", e.folderPath, "--id", "Docs"}, cli.ExitUsage},
		{"bad mode", []string{"folder", "add", e.folderPath, "--id", "docs", "--mode", "sideways"}, cli.ExitUsage},
		{"bad delete policy", []string{"folder", "add", e.folderPath, "--id", "docs", "--delete-policy", "sometimes"}, cli.ExitUsage},
		{"missing dir", []string{"folder", "add", filepath.Join(e.folderPath, "nope"), "--id", "docs"}, cli.ExitUsage},
		{"not a directory", []string{"folder", "add", filepath.Join(e.folderPath, "file.txt"), "--id", "docs"}, cli.ExitUsage},
		{"empty ignore", []string{"folder", "add", e.folderPath, "--id", "docs", "--ignore", ""}, cli.ExitUsage},
		{"blank ignore", []string{"folder", "add", e.folderPath, "--id", "docs", "--ignore", "   "}, cli.ExitUsage},
		{"uncompilable ignore", []string{"folder", "add", e.folderPath, "--id", "docs", "--ignore", "/"}, cli.ExitUsage},
	}
	if err := os.WriteFile(filepath.Join(e.folderPath, "file.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write file.txt: %v", err)
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if code, _, _ := e.run("", tt.args...); code != tt.want {
				t.Fatalf("exit = %d, want %d", code, tt.want)
			}
		})
	}
}

func TestFolderRemove_NoInputWithoutYesExit9(t *testing.T) {
	e := newEnv(t)
	e.init(t)
	if code, _, stderr := e.run("", "folder", "add", e.folderPath, "--id", "docs"); code != cli.ExitOK {
		t.Fatalf("folder add exit = %d, want 0 (stderr: %s)", code, stderr)
	}

	code, _, _ := e.run("", "folder", "remove", "docs", "--no-input")
	if code != cli.ExitConfirmationRequired {
		t.Fatalf("exit = %d, want %d", code, cli.ExitConfirmationRequired)
	}

	code, _, _ = e.run("n\n", "folder", "remove", "docs")
	if code != cli.ExitConfirmationRequired {
		t.Fatalf("declined removal exit = %d, want %d", code, cli.ExitConfirmationRequired)
	}

	code, stdout, stderr := e.run("y\n", "folder", "remove", "docs")
	if code != cli.ExitOK {
		t.Fatalf("confirmed removal exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	if !strings.Contains(stdout, "removed folder docs") {
		t.Fatalf("output = %q", stdout)
	}
}

func TestFolderRemove_UnknownExit4(t *testing.T) {
	e := newEnv(t)
	e.init(t)

	if code, _, _ := e.run("", "folder", "remove", "nope", "--yes"); code != cli.ExitNotFound {
		t.Fatalf("exit = %d, want %d", code, cli.ExitNotFound)
	}
}

func TestCommandsBeforeInitExit3(t *testing.T) {
	e := newEnv(t)

	tests := []struct {
		name string
		args []string
	}{
		{"status", []string{"status"}},
		{"folder list", []string{"folder", "list"}},
		{"folder add", []string{"folder", "add", e.folderPath, "--id", "docs"}},
		{"folder remove", []string{"folder", "remove", "docs", "--yes"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, _, stderr := e.run("", tt.args...)
			if code != cli.ExitNotInitialized {
				t.Fatalf("exit = %d, want %d (stderr: %s)", code, cli.ExitNotInitialized, stderr)
			}
			if !strings.Contains(stderr, "not initialized") {
				t.Fatalf("stderr = %q, want a hint that the data dir is not initialized", stderr)
			}
		})
	}
}

func TestFolderSet_ChangesModeAndDeletePolicy(t *testing.T) {
	e := newEnv(t)
	setupFolder(t, e)

	code, stdout, stderr := e.run("", "folder", "set", "docs", "--mode", "receiveonly", "--delete-policy", "never")
	if code != cli.ExitOK {
		t.Fatalf("folder set exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	if !strings.Contains(stdout, "receiveonly") || !strings.Contains(stdout, "never") {
		t.Fatalf("folder set output = %q", stdout)
	}

	code, stdout, _ = e.run("", "folder", "list", "--json")
	if code != cli.ExitOK {
		t.Fatalf("folder list exit = %d, want 0", code)
	}
	var list []struct {
		ID           string `json:"id"`
		Mode         string `json:"mode"`
		DeletePolicy string `json:"delete_policy"`
	}
	if err := json.Unmarshal([]byte(stdout), &list); err != nil {
		t.Fatalf("folder list is not json: %v\n%s", err, stdout)
	}
	if len(list) != 1 || list[0].Mode != "receiveonly" || list[0].DeletePolicy != "never" {
		t.Fatalf("folder list = %+v, want the new mode and policy", list)
	}

	// One flag at a time leaves the other setting alone.
	if code, _, stderr := e.run("", "folder", "set", "docs", "--mode", "sendonly"); code != cli.ExitOK {
		t.Fatalf("folder set --mode exit = %d (stderr: %s)", code, stderr)
	}
	_, stdout, _ = e.run("", "folder", "list")
	if !strings.Contains(stdout, "sendonly") || !strings.Contains(stdout, "never") {
		t.Fatalf("folder list output = %q, want sendonly and never", stdout)
	}
	if !strings.Contains(stdout, "DELETE POLICY") {
		t.Fatalf("folder list output = %q, want a delete policy column", stdout)
	}
}

func TestFolderSet_BadArguments(t *testing.T) {
	e := newEnv(t)
	setupFolder(t, e)

	tests := []struct {
		name string
		args []string
		want int
	}{
		{"no flags", []string{"folder", "set", "docs"}, cli.ExitUsage},
		{"bad mode", []string{"folder", "set", "docs", "--mode", "sideways"}, cli.ExitUsage},
		{"bad delete policy", []string{"folder", "set", "docs", "--delete-policy", "sometimes"}, cli.ExitUsage},
		{"no folder", []string{"folder", "set", "--mode", "sendonly"}, cli.ExitUsage},
		{"unknown folder", []string{"folder", "set", "nope", "--mode", "sendonly"}, cli.ExitNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if code, _, _ := e.run("", tt.args...); code != tt.want {
				t.Fatalf("exit = %d, want %d", code, tt.want)
			}
		})
	}

	// A rejected change leaves the folder as it was.
	_, stdout, _ := e.run("", "folder", "list")
	if !strings.Contains(stdout, "sendreceive") || !strings.Contains(stdout, "propagate") {
		t.Fatalf("folder list output = %q, want the original settings", stdout)
	}
}
