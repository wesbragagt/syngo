package cli_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/wesbragagt/syngo/internal/app/ports"
	"github.com/wesbragagt/syngo/internal/cli"
	"github.com/wesbragagt/syngo/internal/compose"
)

// The two versions of the conflict every test below resolves, and the name of
// the copy holding the local one.
const (
	localVersion  = "what this device wrote"
	remoteVersion = "what the peer wrote"
	copyPath      = "a.sync-conflict-20260921-153000-PEERDEV.txt"
)

// withConflict initializes a data directory with a folder holding both
// versions of one both-modified conflict, as the puller would leave them.
func withConflict(t *testing.T) env {
	t.Helper()
	e := newEnv(t)
	setupFolder(t, e)
	writeConflictFiles(t, e)

	ctx := context.Background()
	app, err := compose.Build(ctx, compose.Options{DataDir: e.dataDir, RequireIdentity: true})
	if err != nil {
		t.Fatalf("open data dir: %v", err)
	}
	defer func() { _ = app.Close() }()

	record := ports.ConflictRecord{
		ID: "c1", FolderID: "docs", Path: "a.txt", ConflictPath: copyPath,
		LocalHash:  app.Hasher.SumBytes([]byte(localVersion)),
		RemoteHash: app.Hasher.SumBytes([]byte(remoteVersion)),
		PeerID:     "PEERDEVICEID",
		DetectedAt: time.Date(2026, time.September, 21, 15, 30, 0, 0, time.UTC),
		Reason:     "both-modified",
	}
	if err := app.Store.SaveConflict(ctx, record); err != nil {
		t.Fatalf("save conflict: %v", err)
	}
	return e
}

// writeConflictFiles puts the peer's version at the path and the local one
// beside it.
func writeConflictFiles(t *testing.T, e env) {
	t.Helper()
	for name, content := range map[string]string{"a.txt": remoteVersion, copyPath: localVersion} {
		if err := os.WriteFile(filepath.Join(e.folderPath, name), []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
}

// conflictJSON runs a conflict command with --json and decodes its output.
func conflictJSON(t *testing.T, e env, v any, args ...string) int {
	t.Helper()
	code, stdout, stderr := e.run("", append(args, "--json")...)
	if stdout == "" {
		t.Fatalf("%v produced no output (exit %d, stderr: %s)", args, code, stderr)
	}
	if err := json.Unmarshal([]byte(stdout), v); err != nil {
		t.Fatalf("%v output is not json: %v\n%s", args, err, stdout)
	}
	return code
}

// content returns what a file of the folder holds, or "" when it is gone.
func content(t *testing.T, e env, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(e.folderPath, name))
	if os.IsNotExist(err) {
		return ""
	}
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(data)
}

func TestConflictList_TextAndJSON(t *testing.T) {
	e := withConflict(t)

	code, stdout, stderr := e.run("", "conflict", "list")
	if code != cli.ExitOK {
		t.Fatalf("conflict list exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	for _, want := range []string{"c1", "docs", "a.txt", "both-modified", copyPath} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("conflict list output = %q, want it to name %q", stdout, want)
		}
	}

	var list []struct {
		ID           string `json:"id"`
		FolderID     string `json:"folder_id"`
		Path         string `json:"path"`
		ConflictPath string `json:"conflict_path"`
		Reason       string `json:"reason"`
		LocalHash    string `json:"local_hash"`
		RemoteHash   string `json:"remote_hash"`
		Resolved     bool   `json:"resolved"`
		DetectedAt   string `json:"detected_at"`
	}
	if code := conflictJSON(t, e, &list, "conflict", "list"); code != cli.ExitOK {
		t.Fatalf("conflict list --json exit = %d", code)
	}
	if len(list) != 1 || list[0].ID != "c1" || list[0].ConflictPath != copyPath {
		t.Fatalf("list = %+v, want the one conflict", list)
	}
	if list[0].Reason != "both-modified" || list[0].Resolved || list[0].DetectedAt == "" {
		t.Fatalf("conflict = %+v, want an open both-modified conflict with a time", list[0])
	}
	if list[0].LocalHash == "" || list[0].LocalHash == list[0].RemoteHash {
		t.Fatalf("hashes = %q and %q, want the two versions", list[0].LocalHash, list[0].RemoteHash)
	}
}

func TestConflictList_EmptyAndFiltered(t *testing.T) {
	e := withConflict(t)

	if code, stdout, _ := e.run("", "conflict", "list", "docs"); code != cli.ExitOK ||
		!strings.Contains(stdout, "c1") {
		t.Fatalf("conflict list docs = %d, %q", code, stdout)
	}
	if code, _, stderr := e.run("", "conflict", "list", "gone"); code != cli.ExitNotFound {
		t.Fatalf("conflict list of an unknown folder = %d, want %d (stderr: %s)",
			code, cli.ExitNotFound, stderr)
	}

	if code, _, stderr := e.run("", "conflict", "resolve", "c1", "--keep", "both"); code != cli.ExitOK {
		t.Fatalf("resolve exit = %d (stderr: %s)", code, stderr)
	}
	code, stdout, _ := e.run("", "conflict", "list")
	if code != cli.ExitOK || !strings.Contains(stdout, "no conflicts") {
		t.Fatalf("conflict list after resolving = %d, %q", code, stdout)
	}
	if code, stdout, _ = e.run("", "conflict", "list", "--all"); code != cli.ExitOK ||
		!strings.Contains(stdout, "c1") {
		t.Fatalf("conflict list --all = %d, %q", code, stdout)
	}
}

func TestConflictShow_NamesBothVersions(t *testing.T) {
	e := withConflict(t)

	code, stdout, stderr := e.run("", "conflict", "show", "c1")
	if code != cli.ExitOK {
		t.Fatalf("conflict show exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	for _, want := range []string{"a.txt", copyPath, "both-modified", "local version", "remote version"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("conflict show output = %q, want it to name %q", stdout, want)
		}
	}

	var view struct {
		ID   string `json:"id"`
		File *struct {
			Path    string `json:"path"`
			Size    int64  `json:"size"`
			Hash    string `json:"hash"`
			MTime   string `json:"mtime"`
			Version string `json:"version"`
		} `json:"file"`
		Copy *struct {
			Path    string `json:"path"`
			Size    int64  `json:"size"`
			Hash    string `json:"hash"`
			Version string `json:"version"`
		} `json:"copy"`
	}
	if code := conflictJSON(t, e, &view, "conflict", "show", "c1"); code != cli.ExitOK {
		t.Fatalf("conflict show --json exit = %d", code)
	}
	if view.File == nil || view.Copy == nil {
		t.Fatalf("view = %+v, want both files described", view)
	}
	if view.File.Size != int64(len(remoteVersion)) || view.Copy.Size != int64(len(localVersion)) {
		t.Fatalf("sizes = %d and %d, want %d and %d", view.File.Size, view.Copy.Size,
			len(remoteVersion), len(localVersion))
	}
	if view.File.MTime == "" || view.File.Hash == "" {
		t.Fatalf("the file at the path = %+v, want a time and a hash", view.File)
	}
	// The file at the path holds the peer's version and the copy holds
	// this device's, and each says so.
	if view.File.Version != "remote" || view.Copy.Version != "local" {
		t.Fatalf("versions = %q and %q, want remote at the path and local in the copy",
			view.File.Version, view.Copy.Version)
	}
}

func TestConflictShow_UnknownIDExits4(t *testing.T) {
	e := withConflict(t)
	if code, _, stderr := e.run("", "conflict", "show", "nope"); code != cli.ExitNotFound {
		t.Fatalf("exit = %d, want %d (stderr: %s)", code, cli.ExitNotFound, stderr)
	}
}

func TestConflictResolve_KeepsTheVersionThatWasChosen(t *testing.T) {
	cases := []struct {
		keep      string
		wantPath  string
		wantCopy  string
		wantWords string
	}{
		{"local", localVersion, "", "local version kept"},
		{"remote", remoteVersion, "", "remote version kept"},
		{"both", remoteVersion, localVersion, "both versions kept"},
	}
	for _, tc := range cases {
		t.Run(tc.keep, func(t *testing.T) {
			e := withConflict(t)
			code, stdout, stderr := e.run("", "conflict", "resolve", "c1", "--keep", tc.keep, "--yes")
			if code != cli.ExitOK {
				t.Fatalf("exit = %d, want 0 (stderr: %s)", code, stderr)
			}
			if !strings.Contains(stdout, tc.wantWords) {
				t.Fatalf("output = %q, want it to say %q", stdout, tc.wantWords)
			}
			if got := content(t, e, "a.txt"); got != tc.wantPath {
				t.Fatalf("a.txt = %q, want %q", got, tc.wantPath)
			}
			if got := content(t, e, copyPath); got != tc.wantCopy {
				t.Fatalf("%s = %q, want %q", copyPath, got, tc.wantCopy)
			}

			// The conflict is closed, and a resolution that changed
			// the folder rescanned it, so status agrees with what is
			// on disk. Keeping both changes no file and scans nothing.
			var report struct {
				Conflicts struct {
					Unresolved int `json:"unresolved"`
				} `json:"conflicts"`
				Folders []struct {
					Files int64 `json:"files"`
				} `json:"folders"`
			}
			if code := conflictJSON(t, e, &report, "status"); code != cli.ExitOK {
				t.Fatalf("status exit = %d", code)
			}
			if report.Conflicts.Unresolved != 0 {
				t.Fatalf("unresolved = %d, want 0", report.Conflicts.Unresolved)
			}
			wantFiles := int64(1)
			if tc.keep == "both" {
				wantFiles = 0
			}
			if len(report.Folders) != 1 || report.Folders[0].Files != wantFiles {
				t.Fatalf("folders = %+v, want %d file(s) recorded", report.Folders, wantFiles)
			}
		})
	}
}

func TestConflictResolve_JSONReportsWhatItDid(t *testing.T) {
	e := withConflict(t)
	var result struct {
		ID          string `json:"id"`
		Kept        string `json:"kept"`
		KeptPath    string `json:"kept_path"`
		MovedFrom   string `json:"moved_from"`
		RemovedPath string `json:"removed_path"`
		Resolved    bool   `json:"resolved"`
	}
	code := conflictJSON(t, e, &result, "conflict", "resolve", "c1", "--keep", "local", "--yes")
	if code != cli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if result.ID != "c1" || result.Kept != "local" || result.KeptPath != "a.txt" {
		t.Fatalf("result = %+v, want the local version at the path", result)
	}
	if result.MovedFrom != copyPath || !result.Resolved {
		t.Fatalf("result = %+v, want the copy moved back and the conflict closed", result)
	}
}

func TestConflictResolve_RefusesAVersionThatIsGone(t *testing.T) {
	e := withConflict(t)
	if err := os.WriteFile(filepath.Join(e.folderPath, copyPath), []byte("edited since"), 0o644); err != nil {
		t.Fatalf("edit the copy: %v", err)
	}

	code, _, stderr := e.run("", "conflict", "resolve", "c1", "--keep", "local", "--yes")
	if code != cli.ExitIntegrity {
		t.Fatalf("exit = %d, want %d (stderr: %s)", code, cli.ExitIntegrity, stderr)
	}
	if got := content(t, e, "a.txt"); got != remoteVersion {
		t.Fatalf("a.txt = %q, want it untouched", got)
	}
	if got := content(t, e, copyPath); got != "edited since" {
		t.Fatalf("the copy = %q, want it untouched", got)
	}
}

func TestConflictResolve_BadArguments(t *testing.T) {
	e := withConflict(t)
	cases := []struct {
		name string
		args []string
		want int
	}{
		{"an unknown version", []string{"conflict", "resolve", "c1", "--keep", "mine", "--yes"}, cli.ExitUsage},
		{"no --keep", []string{"conflict", "resolve", "c1", "--yes"}, cli.ExitUsage},
		{"an unknown id", []string{"conflict", "resolve", "nope", "--keep", "both"}, cli.ExitNotFound},
		{"no id", []string{"conflict", "resolve", "--keep", "both"}, cli.ExitUsage},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if code, _, stderr := e.run("", tc.args...); code != tc.want {
				t.Fatalf("exit = %d, want %d (stderr: %s)", code, tc.want, stderr)
			}
		})
	}
}

func TestConflictResolve_KeepingBothAsksNothing(t *testing.T) {
	e := withConflict(t)
	// Nothing is deleted, so there is nothing to confirm, and --no-input
	// does not stand in the way.
	if code, stdout, stderr := e.run("", "conflict", "resolve", "c1", "--keep", "both", "--no-input"); code != cli.ExitOK {
		t.Fatalf("exit = %d, want 0 (stdout %q, stderr %q)", code, stdout, stderr)
	}
}

func TestConflictExport_WritesBothVersionsAndADescription(t *testing.T) {
	e := withConflict(t)
	dir := filepath.Join(t.TempDir(), "review")

	var result struct {
		ID      string   `json:"id"`
		Dir     string   `json:"dir"`
		Files   []string `json:"files"`
		Sidecar string   `json:"sidecar"`
	}
	if code := conflictJSON(t, e, &result, "conflict", "export", "c1", dir); code != cli.ExitOK {
		t.Fatalf("conflict export exit = %d", code)
	}
	if len(result.Files) != 2 || result.Sidecar == "" {
		t.Fatalf("export = %+v, want both versions and a description", result)
	}

	var found []string
	for _, name := range result.Files {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		found = append(found, string(data))
	}
	sort.Strings(found)
	want := []string{localVersion, remoteVersion}
	sort.Strings(want)
	if found[0] != want[0] || found[1] != want[1] {
		t.Fatalf("exported %q, want %q", found, want)
	}

	raw, err := os.ReadFile(filepath.Join(dir, result.Sidecar))
	if err != nil {
		t.Fatalf("read the description: %v", err)
	}
	var doc struct {
		ID     string `json:"id"`
		Reason string `json:"reason"`
		Files  []struct {
			Version string `json:"version"`
		} `json:"files"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("the description is not json: %v\n%s", err, raw)
	}
	if doc.ID != "c1" || doc.Reason != "both-modified" || len(doc.Files) != 2 {
		t.Fatalf("description = %+v, want it to describe both versions", doc)
	}

	// The conflict is untouched by an export.
	if code, stdout, _ := e.run("", "conflict", "list"); code != cli.ExitOK || !strings.Contains(stdout, "c1") {
		t.Fatalf("conflict list after an export = %d, %q", code, stdout)
	}
}

func TestConflictExport_UnknownIDExits4(t *testing.T) {
	e := withConflict(t)
	code, _, stderr := e.run("", "conflict", "export", "nope", t.TempDir())
	if code != cli.ExitNotFound {
		t.Fatalf("exit = %d, want %d (stderr: %s)", code, cli.ExitNotFound, stderr)
	}
}

func TestConflict_WithoutASubcommandIsUsage(t *testing.T) {
	e := withConflict(t)
	code, stdout, _ := e.run("", "conflict")
	if code != cli.ExitOK || !strings.Contains(stdout, "conflict") {
		t.Fatalf("conflict without a subcommand = %d, %q, want the help", code, stdout)
	}
}

// staleConflict edits the path after the conflict was recorded, which is what
// a second round of changes on both sides leaves behind: what is at the path
// is neither of the two recorded versions.
func staleConflict(t *testing.T) env {
	t.Helper()
	e := withConflict(t)
	if err := os.WriteFile(filepath.Join(e.folderPath, "a.txt"), []byte(laterVersion), 0o644); err != nil {
		t.Fatalf("edit the path: %v", err)
	}
	return e
}

// laterVersion is content neither side had when the conflict was recorded.
const laterVersion = "a later round wrote this"

func TestConflictResolve_RefusesToOverwriteAChangedPath(t *testing.T) {
	e := staleConflict(t)

	code, _, stderr := e.run("", "conflict", "resolve", "c1", "--keep", "local", "--yes")
	if code != cli.ExitIntegrity {
		t.Fatalf("exit = %d, want %d (stderr: %s)", code, cli.ExitIntegrity, stderr)
	}
	if !strings.Contains(stderr, "a.txt") || !strings.Contains(stderr, "changed since") {
		t.Fatalf("stderr = %q, want it to name the file that changed", stderr)
	}
	if got := content(t, e, "a.txt"); got != laterVersion {
		t.Fatalf("a.txt = %q, want it untouched", got)
	}
	if got := content(t, e, copyPath); got != localVersion {
		t.Fatalf("the copy = %q, want it untouched", got)
	}
}

func TestConflictResolve_RefusesToDeleteAChangedCopy(t *testing.T) {
	e := withConflict(t)
	if err := os.WriteFile(filepath.Join(e.folderPath, copyPath), []byte("merged by hand"), 0o644); err != nil {
		t.Fatalf("edit the copy: %v", err)
	}

	code, _, stderr := e.run("", "conflict", "resolve", "c1", "--keep", "remote", "--yes")
	if code != cli.ExitIntegrity {
		t.Fatalf("exit = %d, want %d (stderr: %s)", code, cli.ExitIntegrity, stderr)
	}
	if got := content(t, e, copyPath); got != "merged by hand" {
		t.Fatalf("the copy = %q, want it untouched", got)
	}
	if got := content(t, e, "a.txt"); got != remoteVersion {
		t.Fatalf("a.txt = %q, want it untouched", got)
	}
}

func TestConflictResolve_AnAlreadyResolvedConflictExits4(t *testing.T) {
	e := withConflict(t)
	if code, _, stderr := e.run("", "conflict", "resolve", "c1", "--keep", "both"); code != cli.ExitOK {
		t.Fatalf("first resolve exit = %d (stderr: %s)", code, stderr)
	}
	code, _, stderr := e.run("", "conflict", "resolve", "c1", "--keep", "local", "--yes")
	if code != cli.ExitNotFound {
		t.Fatalf("exit = %d, want %d (stderr: %s)", code, cli.ExitNotFound, stderr)
	}
	if !strings.Contains(stderr, "already resolved") {
		t.Fatalf("stderr = %q, want it to say the conflict is already resolved", stderr)
	}
	if got := content(t, e, "a.txt"); got != remoteVersion {
		t.Fatalf("a.txt = %q, want the second resolve to have changed nothing", got)
	}
}

func TestConflictList_MarksAStaleConflict(t *testing.T) {
	e := staleConflict(t)

	code, stdout, stderr := e.run("", "conflict", "list")
	if code != cli.ExitOK {
		t.Fatalf("conflict list exit = %d (stderr: %s)", code, stderr)
	}
	if !strings.Contains(stdout, "STALE") {
		t.Fatalf("conflict list output = %q, want a STALE column", stdout)
	}

	var list []struct {
		ID    string `json:"id"`
		Stale bool   `json:"stale"`
	}
	if code := conflictJSON(t, e, &list, "conflict", "list"); code != cli.ExitOK {
		t.Fatalf("conflict list --json exit = %d", code)
	}
	if len(list) != 1 || !list[0].Stale {
		t.Fatalf("list = %+v, want the conflict marked stale", list)
	}

	var view struct {
		Stale bool `json:"stale"`
	}
	if code := conflictJSON(t, e, &view, "conflict", "show", "c1"); code != cli.ExitOK || !view.Stale {
		t.Fatalf("show = %+v (exit %d), want the conflict marked stale", view, code)
	}
	if _, stdout, _ := e.run("", "conflict", "show", "c1"); !strings.Contains(stdout, "stale:    yes") {
		t.Fatalf("conflict show output = %q, want it to say the conflict is stale", stdout)
	}

	// Keeping both is what closes a stale conflict, and it touches no file.
	if code, _, stderr := e.run("", "conflict", "resolve", "c1", "--keep", "both"); code != cli.ExitOK {
		t.Fatalf("resolve --keep both exit = %d (stderr: %s)", code, stderr)
	}
	if got := content(t, e, "a.txt"); got != laterVersion {
		t.Fatalf("a.txt = %q, want it untouched", got)
	}
}

func TestConflictExport_KeepsTheModeOfEachVersion(t *testing.T) {
	e := withConflict(t)
	if err := os.Chmod(filepath.Join(e.folderPath, "a.txt"), 0o755); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	dir := filepath.Join(t.TempDir(), "review")

	var result struct {
		Files []string `json:"files"`
	}
	if code := conflictJSON(t, e, &result, "conflict", "export", "c1", dir); code != cli.ExitOK {
		t.Fatalf("conflict export exit = %d", code)
	}
	want := map[string]os.FileMode{"path-a.txt": 0o755, "conflict-" + copyPath: 0o644}
	for _, name := range result.Files {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("stat %s: %v", name, err)
		}
		if info.Mode().Perm() != want[name] {
			t.Fatalf("%s has mode %o, want %o", name, info.Mode().Perm(), want[name])
		}
	}
}

func TestConflictExport_IntoADirectoryThatCannotBeCreatedExits1(t *testing.T) {
	e := withConflict(t)
	parent := t.TempDir()
	if err := os.Chmod(parent, 0o500); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(parent, 0o700) })

	code, _, stderr := e.run("", "conflict", "export", "c1", filepath.Join(parent, "review"))
	if code != cli.ExitError {
		t.Fatalf("exit = %d, want %d (stderr: %s)", code, cli.ExitError, stderr)
	}
	if !strings.Contains(stderr, "written to") {
		t.Fatalf("stderr = %q, want it to say the directory cannot be written to", stderr)
	}
}
