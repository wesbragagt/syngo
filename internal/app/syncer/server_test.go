package syncer_test

import (
	"context"
	"testing"

	"github.com/wesbragagt/syncthis/internal/app/fakes"
	"github.com/wesbragagt/syncthis/internal/app/syncer"
	"github.com/wesbragagt/syncthis/internal/domain/folder"
)

// testFolder returns a folder configuration for the fake store.
func testFolder(id folder.ID, paused bool) folder.Folder {
	return folder.Folder{
		ID: id, Path: "/srv/" + string(id), Mode: folder.ModeSendReceive,
		DeletePolicy: folder.DeletePropagate, Symlinks: folder.SymlinkSkip,
		KeepEmptyDirs: true, Paused: paused,
	}
}

func TestHello_AnnouncesIdentityAndRunningFolders(t *testing.T) {
	ctx := context.Background()
	store := fakes.NewStore()
	for _, f := range []folder.Folder{testFolder("docs", false), testFolder("photos", true)} {
		if err := store.SaveFolder(ctx, f); err != nil {
			t.Fatalf("save folder: %v", err)
		}
	}

	server := syncer.New(store, fakes.NewIdentity("DEVICE", "laptop"), nil)
	hello := server.Hello(ctx)

	if hello.DeviceID != "DEVICE" || hello.Name != "laptop" {
		t.Fatalf("hello = %+v, want the local identity", hello)
	}
	if len(hello.Folders) != 1 || hello.Folders[0].ID != "docs" {
		t.Fatalf("folders = %+v, want only the running one", hello.Folders)
	}
}
