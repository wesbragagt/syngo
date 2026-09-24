package cli_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/wesbragagt/syngo/internal/cli"
	"github.com/wesbragagt/syngo/internal/compose"
	"github.com/wesbragagt/syngo/internal/domain/device"
)

// authPeerID is the device the authorization tests grant and revoke. It is
// written into the database directly: `peer add` needs a live peer, and these
// tests only need the row.
const authPeerID = device.ID("ETI3RRZBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB")

// otherPeerID is a device that parses but was never paired.
const otherPeerID = device.ID("ETI3RRZCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC")

// saveAuthPeer writes authPeerID into a data directory, as pairing would.
func saveAuthPeer(t *testing.T, e env) {
	t.Helper()
	ctx := context.Background()
	app, err := compose.Build(ctx, compose.Options{DataDir: e.dataDir})
	if err != nil {
		t.Fatalf("open %s: %v", e.dataDir, err)
	}
	defer func() { _ = app.Close() }()

	err = app.Store.SaveDevice(ctx, device.Device{
		ID: authPeerID, Name: "nas", Address: "10.0.0.2:22000",
		Verified: true, AddedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("save peer: %v", err)
	}
}

// authEnv returns an initialized data directory holding one paired device and
// one folder authorized for nobody.
func authEnv(t *testing.T) env {
	t.Helper()
	e := newEnv(t)
	e.init(t)
	saveAuthPeer(t, e)
	if code, _, stderr := e.run("", "folder", "add", e.folderPath, "--id", "docs"); code != cli.ExitOK {
		t.Fatalf("folder add exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	return e
}

// listDevices runs `folder devices --json` and returns the authorized ids.
func listDevices(t *testing.T, e env, id string) []string {
	t.Helper()
	code, stdout, stderr := e.run("", "folder", "devices", id, "--json")
	if code != cli.ExitOK {
		t.Fatalf("folder devices exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	var view struct {
		Folder  string `json:"folder"`
		Devices []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"devices"`
	}
	if err := json.Unmarshal([]byte(stdout), &view); err != nil {
		t.Fatalf("decode %q: %v", stdout, err)
	}
	if view.Folder != id {
		t.Fatalf("folder = %q, want %q", view.Folder, id)
	}
	out := make([]string, 0, len(view.Devices))
	for _, d := range view.Devices {
		out = append(out, d.ID)
	}
	return out
}

func TestFolderAdd_WithoutDeviceAuthorizesNobody(t *testing.T) {
	e := authEnv(t)

	if got := listDevices(t, e, "docs"); len(got) != 0 {
		t.Fatalf("a new folder is authorized for %v, want nobody", got)
	}
	// The text output says so rather than printing an empty table.
	_, stdout, _ := e.run("", "folder", "devices", "docs")
	if !strings.Contains(stdout, "no device") {
		t.Fatalf("output = %q, want it to say the folder is authorized for no device", stdout)
	}
}

func TestFolderGrantThenRevoke_ChangesWhatFolderDevicesReports(t *testing.T) {
	e := authEnv(t)

	if code, stdout, stderr := e.run("", "folder", "grant", "docs", string(authPeerID)); code != cli.ExitOK {
		t.Fatalf("grant exit = %d, want 0 (stdout: %s, stderr: %s)", code, stdout, stderr)
	}
	got := listDevices(t, e, "docs")
	if len(got) != 1 || got[0] != string(authPeerID) {
		t.Fatalf("authorized = %v, want [%s]", got, authPeerID)
	}

	// Granting twice is not an error and does not duplicate the row.
	if code, _, stderr := e.run("", "folder", "grant", "docs", string(authPeerID)); code != cli.ExitOK {
		t.Fatalf("second grant exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	if got := listDevices(t, e, "docs"); len(got) != 1 {
		t.Fatalf("authorized = %v after granting twice, want one entry", got)
	}

	if code, _, stderr := e.run("", "folder", "revoke", "docs", string(authPeerID)); code != cli.ExitOK {
		t.Fatalf("revoke exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	if got := listDevices(t, e, "docs"); len(got) != 0 {
		t.Fatalf("authorized = %v after revoking, want nobody", got)
	}
}

func TestFolderAdd_DeviceFlagAuthorizesAsTheFolderIsCreated(t *testing.T) {
	e := newEnv(t)
	e.init(t)
	saveAuthPeer(t, e)

	code, _, stderr := e.run("", "folder", "add", e.folderPath, "--id", "docs", "--device", string(authPeerID))
	if code != cli.ExitOK {
		t.Fatalf("folder add exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	got := listDevices(t, e, "docs")
	if len(got) != 1 || got[0] != string(authPeerID) {
		t.Fatalf("authorized = %v, want [%s]", got, authPeerID)
	}
}

// TestFolderAdd_UnpairedDeviceLeavesNothingBehind is the reason the devices
// are checked before the folder is written: a typo must not leave a folder
// configured and half authorized.
func TestFolderAdd_UnpairedDeviceLeavesNothingBehind(t *testing.T) {
	e := newEnv(t)
	e.init(t)

	code, _, _ := e.run("", "folder", "add", e.folderPath, "--id", "docs", "--device", string(otherPeerID))
	if code != cli.ExitNotFound {
		t.Fatalf("exit = %d, want %d", code, cli.ExitNotFound)
	}
	if code, _, _ := e.run("", "folder", "devices", "docs"); code != cli.ExitNotFound {
		t.Fatalf("the folder was created anyway: folder devices exit = %d, want %d",
			code, cli.ExitNotFound)
	}
}

func TestFolderAuth_ExitCodes(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want int
	}{
		{"grant on an unknown folder", []string{"folder", "grant", "nope", string(authPeerID)}, cli.ExitNotFound},
		{"grant to an unpaired device", []string{"folder", "grant", "docs", string(otherPeerID)}, cli.ExitNotFound},
		{"grant with a malformed folder id", []string{"folder", "grant", "NOPE!", string(authPeerID)}, cli.ExitUsage},
		{"grant with a malformed device id", []string{"folder", "grant", "docs", "not-a-device"}, cli.ExitUsage},
		{"grant with one argument", []string{"folder", "grant", "docs"}, cli.ExitUsage},
		{"revoke a device that was never granted", []string{"folder", "revoke", "docs", string(authPeerID)}, cli.ExitNotFound},
		{"revoke on an unknown folder", []string{"folder", "revoke", "nope", string(authPeerID)}, cli.ExitNotFound},
		{"revoke with a malformed device id", []string{"folder", "revoke", "docs", "not-a-device"}, cli.ExitUsage},
		{"devices of an unknown folder", []string{"folder", "devices", "nope"}, cli.ExitNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := authEnv(t)
			code, stdout, stderr := e.run("", tc.args...)
			if code != tc.want {
				t.Fatalf("%v exit = %d, want %d (stdout: %s, stderr: %s)",
					tc.args, code, tc.want, stdout, stderr)
			}
		})
	}
}
