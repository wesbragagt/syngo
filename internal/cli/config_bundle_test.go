package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/wesbragagt/syngo/internal/app/configbundle"
	"github.com/wesbragagt/syngo/internal/app/fakes"
	"github.com/wesbragagt/syngo/internal/app/ports"
	"github.com/wesbragagt/syngo/internal/domain/folder"
	"github.com/wesbragagt/syngo/internal/domain/transfer"
)

// errSecondWrite is what the store in this test fails the second write with.
var errSecondWrite = errors.New("disk is gone")

// failingStore writes the first folder and refuses every one after it, which
// is the only way an import ends up half applied.
type failingStore struct {
	*fakes.Store
	writes int
}

func (s *failingStore) SaveFolder(ctx context.Context, f folder.Folder) error {
	s.writes++
	if s.writes > 1 {
		return errSecondWrite
	}
	return s.Store.SaveFolder(ctx, f)
}

// partialImport runs an import whose second folder cannot be written and
// returns what landed and why it stopped.
func partialImport(t *testing.T) (configbundle.Result, error) {
	t.Helper()
	fs := fakes.NewFileSystem()
	fs.AddDir("/home/u/docs", 0o755)
	fs.AddDir("/home/u/notes", 0o755)
	store := &failingStore{Store: fakes.NewStore()}
	settings := ports.Settings{
		Listen: "127.0.0.1:22000", ChunkSize: transfer.DefaultChunkSize, MaxFileSize: 1 << 40,
		RescanInterval: 10 * time.Minute, SyncInterval: 30 * time.Second, LogLevel: "info",
	}
	svc := configbundle.New(store, store.Store, fakes.NewSettings(settings), fs,
		fakes.NewClock(time.Now()), func(string) error { return nil })

	bundle := configbundle.Bundle{
		SchemaVersion: configbundle.SchemaVersion,
		Settings: configbundle.Settings{
			Listen: "127.0.0.1:22000", ChunkSize: transfer.DefaultChunkSize, MaxFileSize: 1 << 40,
			RescanInterval: "10m0s", SyncInterval: "30s", LogLevel: "info",
		},
		Folders: []configbundle.Folder{
			{ID: "docs", Path: "/home/u/docs", Mode: "sendreceive", DeletePolicy: "propagate",
				Symlinks: "skip", IgnorePatterns: []string{}, SyncWindows: []string{}},
			{ID: "notes", Path: "/home/u/notes", Mode: "sendreceive", DeletePolicy: "propagate",
				Symlinks: "skip", IgnorePatterns: []string{}, SyncWindows: []string{}},
		},
	}
	written, err := svc.Import(context.Background(), bundle, configbundle.ImportOptions{})
	if !errors.Is(err, errSecondWrite) {
		t.Fatalf("import error = %v, want the store failure", err)
	}
	if len(written.FoldersAdded) != 1 || written.FoldersAdded[0] != "docs" {
		t.Fatalf("written = %+v, want the first folder only", written)
	}
	return written, err
}

func TestImportFailed_PartialWriteIsReportedWithExit1(t *testing.T) {
	written, cause := partialImport(t)

	var stdout, stderr bytes.Buffer
	deps := &Deps{Stdout: &stdout, Stderr: &stderr}
	err := importFailed(deps, written, cause)
	if codeOf(err) != ExitError {
		t.Fatalf("exit code = %d, want %d", codeOf(err), ExitError)
	}
	if !strings.Contains(stdout.String(), "folder docs: added") {
		t.Fatalf("output = %q, want the folder that was written", stdout.String())
	}
	if strings.Contains(stdout.String(), "notes") {
		t.Fatalf("output = %q, want nothing about the folder that was not written", stdout.String())
	}
	if !errors.Is(err, errSecondWrite) {
		t.Fatalf("error = %v, want the store failure", err)
	}
}

func TestImportFailed_PartialWriteInJSONNamesWhatLanded(t *testing.T) {
	written, cause := partialImport(t)

	var stdout, stderr bytes.Buffer
	deps := &Deps{Stdout: &stdout, Stderr: &stderr, Global: GlobalOpts{JSON: true}}
	err := importFailed(deps, written, cause)
	if codeOf(err) != ExitError {
		t.Fatalf("exit code = %d, want %d", codeOf(err), ExitError)
	}
	if !strings.Contains(err.Error(), "written: folder docs: added") {
		t.Fatalf("error = %q, want it to name what landed", err)
	}
	if !errors.Is(err, errSecondWrite) {
		t.Fatalf("error = %v, want the store failure", err)
	}
}
