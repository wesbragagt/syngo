package cli_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wesbragagt/syncthis/internal/cli"
)

// scanOutput is the JSON shape `scan --json` prints.
type scanOutput struct {
	FolderID string `json:"folder_id"`
	Changes  []struct {
		Type    string `json:"type"`
		Path    string `json:"path"`
		OldPath string `json:"old_path"`
		Hash    string `json:"hash"`
	} `json:"changes"`
	Summary struct {
		Scanned int  `json:"scanned"`
		Hashed  int  `json:"hashed"`
		Ignored int  `json:"ignored"`
		Changes int  `json:"changes"`
		DryRun  bool `json:"dry_run"`
	} `json:"summary"`
}

// setupFolder initializes the data directory and registers the folder root.
func setupFolder(t *testing.T, e env, args ...string) {
	t.Helper()
	e.init(t)
	full := append([]string{"folder", "add", e.folderPath, "--id", "docs"}, args...)
	if code, _, stderr := e.run("", full...); code != cli.ExitOK {
		t.Fatalf("folder add exit = %d, want 0 (stderr: %s)", code, stderr)
	}
}

// scanJSON runs `scan docs --json` and decodes its output.
func scanJSON(t *testing.T, e env, args ...string) (int, scanOutput) {
	t.Helper()
	full := append([]string{"scan", "docs", "--json"}, args...)
	code, stdout, stderr := e.run("", full...)
	if stdout == "" {
		t.Fatalf("scan produced no output (exit %d, stderr: %s)", code, stderr)
	}
	var out scanOutput
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		t.Fatalf("scan output is not json: %v\n%s", err, stdout)
	}
	return code, out
}

// folderStatus returns the first folder section of `status --json`.
func folderStatus(t *testing.T, e env) map[string]any {
	t.Helper()
	code, stdout, stderr := e.run("", "status", "--json")
	if code != cli.ExitOK {
		t.Fatalf("status exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	var report struct {
		Folders []map[string]any `json:"folders"`
	}
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatalf("status output is not json: %v", err)
	}
	if len(report.Folders) != 1 {
		t.Fatalf("folders = %d, want 1", len(report.Folders))
	}
	return report.Folders[0]
}

func TestScan_RealTree_1000Files(t *testing.T) {
	e := newEnv(t)
	for d := range 50 {
		dir := filepath.Join(e.folderPath, fmt.Sprintf("d%02d", d))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		for f := range 20 {
			name := filepath.Join(dir, fmt.Sprintf("f%02d.txt", f))
			if err := os.WriteFile(name, []byte(fmt.Sprintf("content %d/%d", d, f)), 0o644); err != nil {
				t.Fatalf("write: %v", err)
			}
		}
	}
	setupFolder(t, e)

	code, out := scanJSON(t, e)
	if code != cli.ExitOK {
		t.Fatalf("scan exit = %d, want 0", code)
	}
	if out.Summary.Changes != 1050 || out.Summary.Hashed != 1000 {
		t.Fatalf("changes = %d, hashed = %d, want 1050 and 1000", out.Summary.Changes, out.Summary.Hashed)
	}

	status := folderStatus(t, e)
	if files, ok := status["files"].(float64); !ok || int(files) != 1000 {
		t.Fatalf("status files = %v, want 1000", status["files"])
	}
	if dirs, ok := status["dirs"].(float64); !ok || int(dirs) != 50 {
		t.Fatalf("status dirs = %v, want 50", status["dirs"])
	}
	if status["last_scan_at"] == nil {
		t.Fatal("status should report when the folder was last scanned")
	}

	code, out = scanJSON(t, e)
	if code != cli.ExitOK || out.Summary.Changes != 0 {
		t.Fatalf("second scan exit = %d with %d changes, want 0 and 0", code, out.Summary.Changes)
	}
	if out.Summary.Hashed != 0 {
		t.Fatalf("second scan hashed %d files, want 0: unchanged files reuse their hash", out.Summary.Hashed)
	}
}

func TestScan_DryRunExit10(t *testing.T) {
	e := newEnv(t)
	if err := os.WriteFile(filepath.Join(e.folderPath, "a.txt"), []byte("alpha"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.WriteFile(filepath.Join(e.folderPath, "junk.tmp"), []byte("junk"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	setupFolder(t, e, "--ignore", "*.tmp")

	code, out := scanJSON(t, e, "--dry-run")
	if code != cli.ExitDryRun {
		t.Fatalf("dry run exit = %d, want %d", code, cli.ExitDryRun)
	}
	if !out.Summary.DryRun || out.Summary.Changes != 1 {
		t.Fatalf("summary = %+v, want one change in a dry run", out.Summary)
	}
	for _, c := range out.Changes {
		if strings.HasSuffix(c.Path, ".tmp") {
			t.Fatalf("ignored file %q appeared in the change list", c.Path)
		}
	}
	if status := folderStatus(t, e); status["files"].(float64) != 0 {
		t.Fatalf("status files = %v, want 0 after a dry run", status["files"])
	}

	// With nothing to change, a dry run exits 0.
	if code, _, _ := e.run("", "scan", "docs"); code != cli.ExitOK {
		t.Fatalf("scan exit = %d, want 0", code)
	}
	if code, _ := scanJSON(t, e, "--dry-run"); code != cli.ExitOK {
		t.Fatalf("clean dry run exit = %d, want 0", code)
	}
}

func TestScan_RenameOnDisk(t *testing.T) {
	e := newEnv(t)
	original := filepath.Join(e.folderPath, "f1.txt")
	if err := os.WriteFile(original, []byte("hello rename"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	setupFolder(t, e)
	if code, _, stderr := e.run("", "scan", "docs"); code != cli.ExitOK {
		t.Fatalf("scan exit = %d (stderr: %s)", code, stderr)
	}

	if err := os.Rename(original, filepath.Join(e.folderPath, "g1.txt")); err != nil {
		t.Fatalf("rename: %v", err)
	}
	code, out := scanJSON(t, e)

	if code != cli.ExitOK {
		t.Fatalf("scan exit = %d, want 0", code)
	}
	if len(out.Changes) != 1 {
		t.Fatalf("changes = %+v, want one rename", out.Changes)
	}
	c := out.Changes[0]
	if c.Type != "rename" || c.Path != "g1.txt" || c.OldPath != "f1.txt" {
		t.Fatalf("change = %+v, want rename g1.txt from f1.txt", c)
	}
}

func TestRescan_DetectsExternalChange(t *testing.T) {
	e := newEnv(t)
	target := filepath.Join(e.folderPath, "a.txt")
	if err := os.WriteFile(target, []byte("alpha"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	setupFolder(t, e)
	if code, _, stderr := e.run("", "rescan"); code != cli.ExitOK {
		t.Fatalf("rescan exit = %d (stderr: %s)", code, stderr)
	}

	// Change the file outside syncthis, then rescan every folder.
	if err := os.WriteFile(target, []byte("alpha changed on disk"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	code, stdout, stderr := e.run("", "rescan", "--json")
	if code != cli.ExitOK {
		t.Fatalf("rescan exit = %d (stderr: %s)", code, stderr)
	}

	var results []scanOutput
	if err := json.Unmarshal([]byte(stdout), &results); err != nil {
		t.Fatalf("rescan output is not json: %v\n%s", err, stdout)
	}
	if len(results) != 1 || len(results[0].Changes) != 1 {
		t.Fatalf("results = %+v, want one folder with one change", results)
	}
	if got := results[0].Changes[0]; got.Type != "modify" || got.Path != "a.txt" {
		t.Fatalf("change = %+v, want modify a.txt", got)
	}
}

func TestRepair_MissingFileAndDryRun(t *testing.T) {
	e := newEnv(t)
	for _, name := range []string{"f1.txt", "f2.txt"} {
		if err := os.WriteFile(filepath.Join(e.folderPath, name), []byte(name), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	setupFolder(t, e)
	if code, _, stderr := e.run("", "scan", "docs"); code != cli.ExitOK {
		t.Fatalf("scan exit = %d (stderr: %s)", code, stderr)
	}
	if err := os.Remove(filepath.Join(e.folderPath, "f2.txt")); err != nil {
		t.Fatalf("remove: %v", err)
	}

	code, stdout, stderr := e.run("", "repair", "docs", "--dry-run")
	if code != cli.ExitDryRun {
		t.Fatalf("repair --dry-run exit = %d, want %d (stderr: %s)", code, cli.ExitDryRun, stderr)
	}
	if !strings.Contains(stdout, "1 missing marked deleted") {
		t.Fatalf("repair output = %q", stdout)
	}
	if status := folderStatus(t, e); status["files"].(float64) != 2 {
		t.Fatalf("status files = %v, want 2 after a dry run", status["files"])
	}

	if code, _, stderr := e.run("", "repair", "docs"); code != cli.ExitOK {
		t.Fatalf("repair exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	if status := folderStatus(t, e); status["files"].(float64) != 1 {
		t.Fatalf("status files = %v, want 1 after the repair", status["files"])
	}
	if code, _, _ := e.run("", "repair", "docs", "--dry-run"); code != cli.ExitOK {
		t.Fatalf("second dry run exit = %d, want 0: nothing left to repair", code)
	}
}

func TestScan_UnknownFolderExits4(t *testing.T) {
	e := newEnv(t)
	e.init(t)

	if code, _, _ := e.run("", "scan", "nope"); code != cli.ExitNotFound {
		t.Fatalf("scan exit = %d, want %d", code, cli.ExitNotFound)
	}
}
