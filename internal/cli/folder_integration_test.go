package cli_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	statusreport "github.com/wesbragagt/syncthis/internal/app/status"
	"github.com/wesbragagt/syncthis/internal/cli"
)

// folderJSON is the folder object `folder add`, `folder set` and `folder list`
// print.
type folderJSON struct {
	ID          string   `json:"id"`
	Mode        string   `json:"mode"`
	SyncWindows []string `json:"sync_windows"`
}

// decodeFolder reads one folder object out of a command's output.
func decodeFolder(t *testing.T, out string) folderJSON {
	t.Helper()
	var f folderJSON
	if err := json.Unmarshal([]byte(out), &f); err != nil {
		t.Fatalf("output is not a folder object: %v\n%s", err, out)
	}
	return f
}

// closedWindow returns a one-minute window that starts six hours from now in
// local time, so the folder is outside it for the whole test.
func closedWindow(t *testing.T) string {
	t.Helper()
	start := time.Now().Add(6 * time.Hour)
	return start.Format("15:04") + "-" + start.Add(time.Minute).Format("15:04")
}

func TestFolderAdd_SyncWindowsRoundTrip(t *testing.T) {
	e := newEnv(t)
	e.init(t)

	code, stdout, stderr := e.run("", "folder", "add", e.folderPath, "--id", "docs",
		"--sync-window", "22:00-06:00", "--sync-window", "9:05-13:00", "--json")
	if code != cli.ExitOK {
		t.Fatalf("folder add exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	added := decodeFolder(t, stdout)
	if len(added.SyncWindows) != 2 || added.SyncWindows[0] != "22:00-06:00" || added.SyncWindows[1] != "09:05-13:00" {
		t.Fatalf("sync_windows = %v, want the two canonical windows", added.SyncWindows)
	}

	code, stdout, stderr = e.run("", "folder", "list", "--json")
	if code != cli.ExitOK {
		t.Fatalf("folder list exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	var list []folderJSON
	if err := json.Unmarshal([]byte(stdout), &list); err != nil {
		t.Fatalf("folder list output is not json: %v\n%s", err, stdout)
	}
	if len(list) != 1 || len(list[0].SyncWindows) != 2 {
		t.Fatalf("folder list = %+v, want the folder with both windows", list)
	}

	// The text table names the windows too.
	code, stdout, _ = e.run("", "folder", "list")
	if code != cli.ExitOK || !strings.Contains(stdout, "22:00-06:00,09:05-13:00") {
		t.Fatalf("folder list text = %q", stdout)
	}
}

func TestFolderAdd_BadWindowExit2(t *testing.T) {
	for _, window := range []string{"22:00-22:00", "25:00-01:00", "2200-0600"} {
		e := newEnv(t)
		e.init(t)
		code, _, _ := e.run("", "folder", "add", e.folderPath, "--id", "docs", "--sync-window", window)
		if code != cli.ExitUsage {
			t.Fatalf("folder add --sync-window %q exit = %d, want %d", window, code, cli.ExitUsage)
		}
	}
}

func TestFolderSet_ClearSyncWindows(t *testing.T) {
	e := newEnv(t)
	setupFolder(t, e, "--sync-window", "22:00-06:00")

	code, stdout, stderr := e.run("", "folder", "set", "docs", "--sync-window", "12:00-13:00", "--json")
	if code != cli.ExitOK {
		t.Fatalf("folder set exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	// A new list replaces the old one rather than adding to it.
	if got := decodeFolder(t, stdout).SyncWindows; len(got) != 1 || got[0] != "12:00-13:00" {
		t.Fatalf("sync_windows = %v, want only the window that was just set", got)
	}

	code, stdout, stderr = e.run("", "folder", "set", "docs", "--clear-sync-windows", "--json")
	if code != cli.ExitOK {
		t.Fatalf("folder set --clear-sync-windows exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	if got := decodeFolder(t, stdout).SyncWindows; len(got) != 0 {
		t.Fatalf("sync_windows = %v, want them cleared", got)
	}

	code, stdout, _ = e.run("", "folder", "list")
	if code != cli.ExitOK || !strings.Contains(stdout, "always") {
		t.Fatalf("folder list text = %q, want the windows column to read always", stdout)
	}
}

func TestFolderSet_ClearAndSetConflictExit2(t *testing.T) {
	e := newEnv(t)
	setupFolder(t, e)

	code, _, _ := e.run("", "folder", "set", "docs", "--sync-window", "22:00-06:00", "--clear-sync-windows")
	if code != cli.ExitUsage {
		t.Fatalf("exit = %d, want %d", code, cli.ExitUsage)
	}
}

func TestStatus_ScheduledWithoutDaemon(t *testing.T) {
	e := newEnv(t)
	setupFolder(t, e, "--sync-window", closedWindow(t))

	code, stdout, stderr := e.run("", "status", "--json")
	if code != cli.ExitOK {
		t.Fatalf("status exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	var report statusreport.Report
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatalf("status output is not json: %v\n%s", err, stdout)
	}
	if len(report.Folders) != 1 {
		t.Fatalf("folders = %+v, want one", report.Folders)
	}
	f := report.Folders[0]
	if f.State != statusreport.StateScheduled || f.InWindow || f.NextWindow == nil {
		t.Fatalf("folder = %+v, want it scheduled with a next window", f)
	}

	code, stdout, _ = e.run("", "status")
	if code != cli.ExitOK || !strings.Contains(stdout, statusreport.StateScheduled) {
		t.Fatalf("status text = %q, want the scheduled state", stdout)
	}
}
