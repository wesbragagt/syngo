package cli_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/wesbragagt/syncthis/internal/cli"
	"github.com/wesbragagt/syncthis/internal/compose"
	"github.com/wesbragagt/syncthis/internal/domain/manifest"
	"github.com/wesbragagt/syncthis/internal/domain/transfer"
)

// testChunkSize is the chunk size of the journal rows these tests write.
const testChunkSize = 4096

// transferJSON is what `transfer list --json` and `transfer show --json`
// print.
type transferJSON struct {
	ID            string  `json:"id"`
	FolderID      string  `json:"folder_id"`
	Path          string  `json:"path"`
	PeerID        string  `json:"peer_id"`
	Direction     string  `json:"direction"`
	State         string  `json:"state"`
	Size          int64   `json:"size"`
	ChunkSize     int     `json:"chunk_size"`
	ChunksTotal   int     `json:"chunks_total"`
	ChunksDone    int     `json:"chunks_done"`
	BytesDone     int64   `json:"bytes_done"`
	UpdatedAt     *string `json:"updated_at"`
	Error         *string `json:"error"`
	ChunksMissing *int    `json:"chunks_missing"`
	MissingChunks []int   `json:"missing_chunks"`
}

// seedTransfer writes one journal row into the data directory, as an
// interrupted download would have left it.
func seedTransfer(t *testing.T, e env, id transfer.ID, path string, state transfer.State, chunks, done int) {
	t.Helper()
	ctx := context.Background()
	app, err := compose.Build(ctx, compose.Options{DataDir: e.dataDir, RequireIdentity: true})
	if err != nil {
		t.Fatalf("open data dir: %v", err)
	}
	defer func() { _ = app.Close() }()

	at := time.Date(2026, time.September, 21, 12, 0, 0, 0, time.UTC)
	entry := manifest.Entry{
		Path: path, Kind: manifest.KindFile, Size: int64(chunks) * testChunkSize, ModTime: at,
	}
	row := transfer.NewInbound(id, "docs", entry, "PEERDEVICE", testChunkSize,
		e.folderPath+"/.syncthis/tmp/"+string(id)+".part", at)
	row.State = state
	for i := range done {
		if err := row.MarkVerified(i, manifest.Hash{}, at); err != nil {
			t.Fatalf("mark chunk %d verified: %v", i, err)
		}
	}
	if err := app.Store.SaveTransfer(ctx, row); err != nil {
		t.Fatalf("save transfer: %v", err)
	}
}

// withFolder initializes the data directory and adds the docs folder.
func withFolder(t *testing.T) env {
	t.Helper()
	e := newEnv(t)
	setupFolder(t, e)
	return e
}

func TestTransferList_TextAndJSON(t *testing.T) {
	e := withFolder(t)
	seedTransfer(t, e, "aaaa", "big.bin", transfer.StateActive, 4, 1)
	seedTransfer(t, e, "bbbb", "other.bin", transfer.StatePaused, 2, 2)

	code, stdout, stderr := e.run("", "transfer", "list")
	if code != cli.ExitOK {
		t.Fatalf("transfer list exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	for _, want := range []string{"ID", "FOLDER", "big.bin", "other.bin", "active", "paused", "1/4"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("transfer list output does not mention %q:\n%s", want, stdout)
		}
	}

	code, stdout, stderr = e.run("", "transfer", "list", "--json")
	if code != cli.ExitOK {
		t.Fatalf("transfer list --json exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	var list []transferJSON
	if err := json.Unmarshal([]byte(stdout), &list); err != nil {
		t.Fatalf("transfer list --json is not json: %v\n%s", err, stdout)
	}
	if len(list) != 2 {
		t.Fatalf("listed %d transfers, want 2", len(list))
	}
	first := list[0]
	if first.ID != "aaaa" || first.Path != "big.bin" || first.FolderID != "docs" {
		t.Fatalf("first transfer = %+v", first)
	}
	if first.Direction != "in" || first.State != "active" {
		t.Fatalf("first transfer = %+v, want an inbound active transfer", first)
	}
	if first.ChunksTotal != 4 || first.ChunksDone != 1 || first.ChunkSize != testChunkSize {
		t.Fatalf("chunks = %d/%d of %d bytes", first.ChunksDone, first.ChunksTotal, first.ChunkSize)
	}
	if first.BytesDone != testChunkSize || first.Size != 4*testChunkSize {
		t.Fatalf("bytes = %d of %d", first.BytesDone, first.Size)
	}
	if first.PeerID != "PEERDEVICE" || first.UpdatedAt == nil || first.Error != nil {
		t.Fatalf("first transfer = %+v", first)
	}
	if first.MissingChunks != nil || first.ChunksMissing != nil {
		t.Fatalf("a listing carries chunk indexes: %+v", first)
	}
}

func TestTransferList_FilterByState(t *testing.T) {
	e := withFolder(t)
	seedTransfer(t, e, "aaaa", "big.bin", transfer.StateActive, 4, 1)
	seedTransfer(t, e, "bbbb", "other.bin", transfer.StatePaused, 2, 2)

	code, stdout, stderr := e.run("", "transfer", "list", "--state", "paused", "--json")
	if code != cli.ExitOK {
		t.Fatalf("transfer list --state exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	var list []transferJSON
	if err := json.Unmarshal([]byte(stdout), &list); err != nil {
		t.Fatalf("output is not json: %v\n%s", err, stdout)
	}
	if len(list) != 1 || list[0].ID != "bbbb" {
		t.Fatalf("filtered list = %+v, want only the paused transfer", list)
	}

	code, _, stderr = e.run("", "transfer", "list", "--state", "sideways")
	if code != cli.ExitUsage {
		t.Fatalf("unknown state exit = %d, want %d", code, cli.ExitUsage)
	}
	if !strings.Contains(stderr, "sideways") {
		t.Fatalf("stderr = %q, want the rejected state named", stderr)
	}
}

func TestTransferList_Empty(t *testing.T) {
	e := withFolder(t)

	code, stdout, _ := e.run("", "transfer", "list")
	if code != cli.ExitOK {
		t.Fatalf("transfer list exit = %d, want 0", code)
	}
	if !strings.Contains(stdout, "no transfers") {
		t.Fatalf("output = %q, want a note that there are none", stdout)
	}

	code, stdout, _ = e.run("", "transfer", "list", "--json")
	if code != cli.ExitOK {
		t.Fatalf("transfer list --json exit = %d, want 0", code)
	}
	if strings.TrimSpace(stdout) != "[]" {
		t.Fatalf("json output = %q, want an empty array", stdout)
	}
}

func TestTransferShow_TextAndJSON(t *testing.T) {
	e := withFolder(t)
	seedTransfer(t, e, "aaaa", "big.bin", transfer.StatePaused, 4, 2)

	code, stdout, stderr := e.run("", "transfer", "show", "aaaa")
	if code != cli.ExitOK {
		t.Fatalf("transfer show exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	for _, want := range []string{"id:", "big.bin", "paused", "PEERDEVICE", "2 of 4", "missing:   2 (2, 3)"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("transfer show output does not mention %q:\n%s", want, stdout)
		}
	}

	code, stdout, stderr = e.run("", "transfer", "show", "aaaa", "--json")
	if code != cli.ExitOK {
		t.Fatalf("transfer show --json exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	var view transferJSON
	if err := json.Unmarshal([]byte(stdout), &view); err != nil {
		t.Fatalf("output is not json: %v\n%s", err, stdout)
	}
	if view.ID != "aaaa" || view.State != "paused" || view.ChunksDone != 2 || view.ChunksTotal != 4 {
		t.Fatalf("view = %+v", view)
	}
	if view.ChunksMissing == nil || *view.ChunksMissing != 2 {
		t.Fatalf("chunks_missing = %v, want 2", view.ChunksMissing)
	}
	if len(view.MissingChunks) != 2 || view.MissingChunks[0] != 2 || view.MissingChunks[1] != 3 {
		t.Fatalf("missing_chunks = %v, want [2 3]", view.MissingChunks)
	}
}

func TestTransferShow_UnknownID(t *testing.T) {
	e := withFolder(t)

	code, _, stderr := e.run("", "transfer", "show", "nope")
	if code != cli.ExitNotFound {
		t.Fatalf("exit = %d, want %d (stderr: %s)", code, cli.ExitNotFound, stderr)
	}

	code, stdout, _ := e.run("", "transfer", "show", "nope", "--json")
	if code != cli.ExitNotFound {
		t.Fatalf("exit = %d, want %d", code, cli.ExitNotFound)
	}
	var failure struct {
		Error struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(stdout), &failure); err != nil {
		t.Fatalf("json failure is not json: %v\n%s", err, stdout)
	}
	if failure.Error.Code != cli.ExitNotFound {
		t.Fatalf("error = %+v, want code %d", failure.Error, cli.ExitNotFound)
	}
}

func TestTransferShow_NeedsAnID(t *testing.T) {
	e := withFolder(t)

	if code, _, _ := e.run("", "transfer", "show"); code != cli.ExitUsage {
		t.Fatalf("exit = %d, want %d", code, cli.ExitUsage)
	}
}
