package tui

import (
	"encoding/json"
	"testing"
)

// The documents below are copied from docs/CLI.md. The TUI decodes the
// published `--json` shapes and nothing else, so a field renamed there has to
// break this test before it breaks a screen.

// statusExample is the `syncthis status --json` document of docs/CLI.md.
const statusExample = `{
  "schema_version": 1,
  "device": {"id": "RCUFGP7", "name": "laptop", "data_dir": "/home/u/.local/share/syncthis"},
  "daemon": {"running": true, "pid": 3776214, "listen": "127.0.0.1:22001",
             "started_at": "2026-09-21T08:32:15Z",
             "upload_limit": 0, "download_limit": 4194304,
             "listener": {"bound": true, "address": "127.0.0.1:22001",
                          "interface": "tailscale0",
                          "bound_at": "2026-09-21T08:40:02Z",
                          "lost_at": "2026-09-21T08:39:58Z",
                          "rebinds": 1, "failures": 0, "error": null}},
  "folders": [
    {"id": "docs", "path": "/home/u/docs", "label": "", "mode": "sendreceive",
     "delete_policy": "propagate", "paused": false, "state": "idle",
     "files": 1, "dirs": 0, "symlinks": 0, "bytes": 13,
     "last_scan_at": "2026-09-21T08:32:15Z", "pending_changes": 0, "error": null,
     "sync_windows": ["22:00-06:00"], "in_window": false,
     "next_window": "2026-09-21T20:00:00Z"}
  ],
  "peers": [
    {"id": "ETI3RRZ", "name": "nas", "address": "127.0.0.1:22002", "verified": true,
     "connected": true, "last_seen_at": "2026-09-21T08:32:15Z", "latency_ms": 0.022,
     "error": null, "state": "connected", "next_retry_at": null,
     "path": {"kind": "direct", "endpoint": "192.168.1.20:41641", "relay": "",
              "active": true, "reason": null,
              "observed_at": "2026-09-21T08:32:45Z"}}
  ],
  "transfers": {"active": 0, "pending": 0, "paused": 0, "failed": 0, "items": []},
  "conflicts": {"unresolved": 0}
}`

func TestContract_StatusDocumentDecodes(t *testing.T) {
	var got Status
	if err := json.Unmarshal([]byte(statusExample), &got); err != nil {
		t.Fatalf("decode the documented status: %v", err)
	}
	if got.SchemaVersion != 1 {
		t.Errorf("schema_version is %d, want 1", got.SchemaVersion)
	}
	if got.Device.Name != "laptop" || got.Device.DataDir == "" {
		t.Errorf("device is %+v", got.Device)
	}
	if !got.Daemon.Running || got.Daemon.PID != 3776214 || got.Daemon.DownloadLimit != 4194304 {
		t.Errorf("daemon is %+v", got.Daemon)
	}
	if !got.Daemon.Listener.Bound || got.Daemon.Listener.Interface != "tailscale0" || got.Daemon.Listener.Rebinds != 1 {
		t.Errorf("listener is %+v", got.Daemon.Listener)
	}
	if len(got.Folders) != 1 {
		t.Fatalf("folders is %+v", got.Folders)
	}
	f := got.Folders[0]
	if f.ID != "docs" || f.State != "idle" || f.Files != 1 || f.Bytes != 13 || f.InWindow {
		t.Errorf("folder is %+v", f)
	}
	if len(f.SyncWindows) != 1 || f.NextWindow == nil {
		t.Errorf("folder windows are %+v / %v", f.SyncWindows, f.NextWindow)
	}
	if len(got.Peers) != 1 {
		t.Fatalf("peers is %+v", got.Peers)
	}
	p := got.Peers[0]
	if p.State != "connected" || !p.Connected || p.LatencyMS != 0.022 || p.NextRetryAt != nil {
		t.Errorf("peer is %+v", p)
	}
	if p.Path.Kind != "direct" || p.Path.Endpoint != "192.168.1.20:41641" || !p.Path.Active {
		t.Errorf("path is %+v", p.Path)
	}
}

// pathExample is the `syncthis network path --json` document of docs/CLI.md.
const pathExample = `[
  {
    "peer_id": "ETI3RRZ",
    "name": "nas",
    "address": "100.64.0.3:22000",
    "kind": "direct",
    "endpoint": "192.168.1.20:41641",
    "relay": "",
    "active": true,
    "reason": null
  }
]`

func TestContract_NetworkPathDocumentDecodes(t *testing.T) {
	var got []NetPath
	if err := json.Unmarshal([]byte(pathExample), &got); err != nil {
		t.Fatalf("decode the documented network path: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d entries", len(got))
	}
	if got[0].Kind != "direct" || got[0].Name != "nas" || got[0].Reason != nil || !got[0].Active {
		t.Errorf("entry is %+v", got[0])
	}
}

// conflictExample is the `syncthis conflict list --json` document of
// docs/CLI.md.
const conflictExample = `[
  {
    "id": "6f1c2f3a4b5c6d7e8f9a0b1c2d3e4f5a",
    "folder_id": "docs",
    "path": "notes/report.md",
    "conflict_path": "notes/report.sync-conflict-20260921-153000-ETI3RRZ.md",
    "reason": "both-modified",
    "local_hash": "3f0a",
    "remote_hash": "9b12",
    "peer_id": "ETI3RRZ",
    "detected_at": "2026-09-21T15:30:00Z",
    "resolved": false,
    "stale": false
  }
]`

func TestContract_ConflictDocumentDecodes(t *testing.T) {
	var got []Conflict
	if err := json.Unmarshal([]byte(conflictExample), &got); err != nil {
		t.Fatalf("decode the documented conflict list: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d entries", len(got))
	}
	c := got[0]
	if c.Reason != "both-modified" || c.Resolved || c.Stale || c.ConflictPath == "" || c.DetectedAt == nil {
		t.Errorf("conflict is %+v", c)
	}
}

// errorExample is the failure document `--json` prints, from docs/CLI.md.
const errorExample = `{
  "error": {
    "code": 4,
    "message": "folder \"nope\": not found"
  }
}`

func TestContract_FailureDocumentDecodes(t *testing.T) {
	if got := errorMessage([]byte(errorExample), 4); got != `folder "nope": not found` {
		t.Errorf("message is %q", got)
	}
	// Output that is not a failure document is reported as it stands
	// rather than swallowed.
	if got := errorMessage([]byte("panic: nope"), 1); got != "panic: nope" {
		t.Errorf("fallback message is %q", got)
	}
}

// initExample is the `syncthis init --json` document of docs/CLI.md.
const initExample = `{
  "device_id": "ZFBP44XEKUVRBOA2CSBDZ7QAUEW5DBCGZHBLOSB5C4XWX5UV35JQ",
  "name": "laptop",
  "data_dir": "/home/u/.local/share/syncthis",
  "listen": "127.0.0.1:22000",
  "created": true
}`

// peerAddExample is the `syncthis peer add --json` document of docs/CLI.md.
const peerAddExample = `{
  "id": "ETI3RRZCMWYFD54X22RLMR5SYEWJTA3UANTRKYNN3KAAX5ZGPAFA",
  "name": "nas",
  "address": "100.64.0.2:22000",
  "verified": true,
  "added_at": "2026-09-21T08:28:28Z",
  "last_seen_at": null,
  "accepted": false,
  "resolved_from": "nas"
}`

// folderAddExample is the `syncthis folder add --json` document of
// docs/CLI.md.
const folderAddExample = `{
  "id": "docs",
  "path": "/home/u/docs",
  "label": "",
  "mode": "sendreceive",
  "delete_policy": "propagate",
  "symlinks": "skip",
  "include_hidden": false,
  "keep_empty_dirs": true,
  "ignore_patterns": [],
  "sync_windows": [],
  "paused": false
}`

func TestContract_SetupDocumentsDecode(t *testing.T) {
	var initResult InitResult
	if err := json.Unmarshal([]byte(initExample), &initResult); err != nil {
		t.Fatalf("decode the documented init: %v", err)
	}
	if !initResult.Created || initResult.Listen != "127.0.0.1:22000" || initResult.DeviceID == "" {
		t.Errorf("init is %+v", initResult)
	}

	var added PeerAdded
	if err := json.Unmarshal([]byte(peerAddExample), &added); err != nil {
		t.Fatalf("decode the documented peer add: %v", err)
	}
	if added.Name != "nas" || !added.Verified || added.ResolvedFrom != "nas" {
		t.Errorf("peer is %+v", added)
	}
	if added.Accepted == nil || *added.Accepted {
		t.Errorf("accepted is %v, want a present false", added.Accepted)
	}

	var folder FolderAdded
	if err := json.Unmarshal([]byte(folderAddExample), &folder); err != nil {
		t.Fatalf("decode the documented folder add: %v", err)
	}
	if folder.ID != "docs" || folder.Mode != "sendreceive" || folder.Paused {
		t.Errorf("folder is %+v", folder)
	}
}
