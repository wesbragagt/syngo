package e2e

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/wesbragagt/syncthis/internal/app/ports"
	"github.com/wesbragagt/syncthis/internal/domain/folder"
)

// waitScanned waits until a node's folder reports at least the given number of
// files, which is how a test knows a local change has reached the manifest the
// peers ask for.
func waitScanned(t *testing.T, n *node, id folder.ID, files int64) {
	t.Helper()
	waitFor(t, fmt.Sprintf("%d files in %s on %s", files, id, n.name), convergeTimeout, func() bool {
		for _, f := range n.Status().Folders {
			if f.ID == string(id) && f.Files >= files {
				return true
			}
		}
		return false
	})
}

// marker puts a file in the second folder of from and waits for it to arrive
// in the second folder of to. Both sides keep that folder sendreceive, so its
// arrival proves that to ran a pull cycle against from. A test that has to
// show something did *not* happen waits for the marker first, instead of
// sleeping for a while and hoping.
func marker(t *testing.T, from, to *node, name string) {
	t.Helper()
	writeFileIn(t, from.Extra, name, []byte(name))
	waitFor(t, name+" in the second folder of "+to.name, convergeTimeout, func() bool {
		_, err := os.Lstat(filepath.Join(to.Extra, name))
		return err == nil
	})
}

// waitPresent waits until a path exists inside a node's folder.
func waitPresent(t *testing.T, n *node, rel string) {
	t.Helper()
	waitFor(t, rel+" on "+n.name, convergeTimeout, func() bool {
		_, err := os.Lstat(filepath.Join(n.Root, rel))
		return err == nil
	})
}

// exists reports whether a path is present inside a node's folder.
func exists(t *testing.T, n *node, rel string) bool {
	t.Helper()
	_, err := os.Lstat(filepath.Join(n.Root, rel))
	switch {
	case err == nil:
		return true
	case errors.Is(err, os.ErrNotExist):
		return false
	default:
		t.Fatalf("stat %s on %s: %v", rel, n.name, err)
		return false
	}
}

func TestSendOnly_NeverPullsFromThePeer(t *testing.T) {
	a := startNodeWith(t, "a", nodeConfig{Mode: folder.ModeSendOnly, ExtraFolder: true})
	b := startNodeWith(t, "b", nodeConfig{Mode: folder.ModeSendReceive, ExtraFolder: true})
	writeFile(t, a, "from-a.txt", []byte("a"))
	writeFile(t, b, "from-b.txt", []byte("b"))
	pair(t, a, b)

	// The send-only side hands its file over, and the file the other side
	// has is in the manifest a asks for.
	waitPresent(t, b, "from-a.txt")
	waitScanned(t, b, folderID, 2)

	// A file put in the second folder afterwards arrives on a, which
	// proves a ran a pull cycle against b with from-b.txt on offer. What a
	// send-only folder does with that offer is nothing.
	marker(t, b, a, "cycle-ran.txt")
	if exists(t, a, "from-b.txt") {
		t.Fatal("the send-only folder pulled a file from the peer")
	}
	if !exists(t, b, "from-b.txt") {
		t.Fatal("the peer lost its own file")
	}
}

func TestReceiveOnly_NeverServesItsManifest(t *testing.T) {
	a := startNodeWith(t, "a", nodeConfig{Mode: folder.ModeSendReceive, ExtraFolder: true})
	b := startNodeWith(t, "b", nodeConfig{Mode: folder.ModeReceiveOnly, ExtraFolder: true})
	writeFile(t, b, "local.txt", []byte("b"))
	pair(t, a, b)
	waitScanned(t, b, folderID, 1)

	ctx, cancel := context.WithTimeout(context.Background(), daemonTimeout)
	defer cancel()
	conn, err := a.App.Dialer.Dial(ctx, b.Listen, b.ID)
	if err != nil {
		t.Fatalf("dial %s: %v", b.name, err)
	}
	defer func() { _ = conn.Close() }()

	if _, err := conn.RequestManifest(ctx, folderID); !errors.Is(err, ports.ErrFolderReceiveOnly) {
		t.Fatalf("manifest request = %v, want ErrFolderReceiveOnly", err)
	}

	// The folder is still announced, with the mode that explains the
	// refusal, so the peer knows not to ask again.
	var advertised *folder.Mode
	for _, advert := range conn.RemoteHello().Folders {
		if advert.ID == folderID {
			mode := advert.Mode
			advertised = &mode
		}
	}
	if advertised == nil || *advertised != folder.ModeReceiveOnly {
		t.Fatalf("hello folders = %+v, want docs announced as receive-only", conn.RemoteHello().Folders)
	}

	// The marker proves a pulled from b after local.txt was in b's
	// manifest; the file itself never crosses.
	marker(t, b, a, "cycle-ran.txt")
	if exists(t, a, "local.txt") {
		t.Fatal("the receive-only folder served a file after all")
	}
}

func TestDeletePolicyNever_KeepsTheFileAndNamesTheReason(t *testing.T) {
	a := startNode(t, "a", folder.ModeSendReceive)
	b := startNodeWith(t, "b", nodeConfig{Mode: folder.ModeReceiveOnly, DeletePolicy: folder.DeleteNever})
	writeFile(t, a, "keep.bin", payload(4096, 41))
	pair(t, a, b)
	waitPresent(t, b, "keep.bin")

	if err := os.Remove(filepath.Join(a.Root, "keep.bin")); err != nil {
		t.Fatalf("delete on %s: %v", a.name, err)
	}
	// A file written after the deletion proves that the receiver ran a
	// sync cycle which saw the tombstone.
	writeFile(t, a, "marker.txt", []byte("after the delete"))
	waitPresent(t, b, "marker.txt")

	if !exists(t, b, "keep.bin") {
		t.Fatal("the delete policy is never, but the file was removed")
	}

	// The skipped delete is reported, with the reason, by a dry run.
	code, stdout := runCLI(t, b.DataDir, "sync-plan", "docs")
	if code != 0 {
		t.Fatalf("sync-plan exit = %d, want 0: a skipped delete changes nothing\n%s", code, stdout)
	}
	if !contains(stdout, "keep.bin") || !contains(stdout, "delete-policy-never") {
		t.Fatalf("sync-plan does not report the skipped delete:\n%s", stdout)
	}
}

func TestDeletePolicyPropagate_RemovesTheFile(t *testing.T) {
	a := startNode(t, "a", folder.ModeSendOnly)
	b := startNode(t, "b", folder.ModeReceiveOnly)
	writeFile(t, a, "gone.bin", payload(4096, 43))
	pair(t, a, b)
	waitPresent(t, b, "gone.bin")

	if err := os.Remove(filepath.Join(a.Root, "gone.bin")); err != nil {
		t.Fatalf("delete on %s: %v", a.name, err)
	}
	waitTreeEqual(t, a, b)
	if exists(t, b, "gone.bin") {
		t.Fatal("the deletion was not applied")
	}
}
