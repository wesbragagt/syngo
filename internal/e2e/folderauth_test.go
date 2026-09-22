package e2e

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wesbragagt/syncthis/internal/app/ports"
	"github.com/wesbragagt/syncthis/internal/app/transfers"
	"github.com/wesbragagt/syncthis/internal/domain/folder"
)

// dialTo opens one connection from one node to another, outside the daemon's
// own peer loop, so a test can ask a question the daemon would never ask.
func dialTo(t *testing.T, from, to *node) ports.PeerConn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), daemonTimeout)
	defer cancel()

	conn, err := from.App.Dialer.Dial(ctx, to.Listen, to.ID)
	if err != nil {
		t.Fatalf("dial %s from %s: %v", to.name, from.name, err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// announced lists the folder ids a peer named in its Hello.
func announced(conn ports.PeerConn) []folder.ID {
	out := make([]folder.ID, 0, len(conn.RemoteHello().Folders))
	for _, a := range conn.RemoteHello().Folders {
		out = append(out, a.ID)
	}
	return out
}

// announces reports whether a folder id is in a list.
func announces(ids []folder.ID, want folder.ID) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

// TestFolderAuth_APeerAuthorizedForOneFolderCannotReachAnother is the whole
// point of folder-level authorization. Three devices are paired with a: b
// shares docs with it, c shares extra with it and nothing else. c must not
// learn that docs exists, and asking for it by name must be refused exactly as
// a folder that does not exist.
func TestFolderAuth_APeerAuthorizedForOneFolderCannotReachAnother(t *testing.T) {
	a := startNodeWith(t, "a", nodeConfig{Mode: folder.ModeSendReceive, ExtraFolder: true})
	b := startNode(t, "b", folder.ModeSendReceive)
	c := startNodeWith(t, "c", nodeConfig{NoFolder: true, ExtraFolder: true})

	// Every device knows every other: pairing is not authorization.
	for _, pair := range [][2]*node{{a, b}, {b, a}, {a, c}, {c, a}, {b, c}, {c, b}} {
		trust(t, pair[0], pair[1])
	}
	// a shares docs with b only, and extra with c only.
	grant(t, a, folderID, b.ID)
	grant(t, b, folderID, a.ID)
	grant(t, a, extraID, c.ID)
	grant(t, c, extraID, a.ID)

	secret := payload(4000, 41)
	writeFile(t, a, "secret.txt", secret)
	writeFileIn(t, a.Extra, "shared.txt", payload(4000, 42))

	// The folder c is authorized for converges, which proves c and a are
	// connected and have run a full cycle. Anything c did not get by then,
	// it was refused rather than merely slow.
	waitFor(t, "shared.txt to arrive on c", convergeTimeout, func() bool {
		_, err := os.Lstat(filepath.Join(c.Extra, "shared.txt"))
		return err == nil
	})
	waitPresent(t, b, "secret.txt")

	// 1. The handshake does not name docs to c.
	conn := dialTo(t, c, a)
	if got := announced(conn); announces(got, folderID) {
		t.Fatalf("a announced %v to c, which is not authorized for %s", got, folderID)
	}
	if got := announced(conn); !announces(got, extraID) {
		t.Fatalf("a announced %v to c, which should hold %s", got, extraID)
	}

	// 2. Asking for it directly is refused, and refused as unknown: c must
	// not be able to tell "no such folder" from "not yours".
	ctx, cancel := context.WithTimeout(context.Background(), daemonTimeout)
	defer cancel()
	_, err := conn.RequestManifest(ctx, folderID)
	if !errors.Is(err, ports.ErrFolderUnknown) {
		t.Fatalf("manifest of %s from a = %v, want %v", folderID, err, ports.ErrFolderUnknown)
	}
	t.Logf("c asking a for %s directly: %v", folderID, err)

	// 3. Asking how the file was cut is refused, so c never sees a
	// boundary, and asking for a chunk of it is refused too, without
	// naming the file.
	var refusal *ports.ChunkError
	list, err := conn.RequestChunks(ctx, ports.ChunkListRequest{
		FolderID: folderID, Path: "secret.txt",
	})
	if len(list) != 0 {
		t.Fatalf("a described %d chunks of %s to c, which is not authorized for it", len(list), folderID)
	}
	if !errors.As(err, &refusal) || refusal.Reason != ports.ChunkFolderUnknown {
		t.Fatalf("chunk list of %s from a = %v, want a %s refusal", folderID, err, ports.ChunkFolderUnknown)
	}
	t.Logf("c asking a how it cut a file of %s: %v", folderID, err)

	_, err = conn.RequestChunk(ctx, ports.ChunkRequest{
		FolderID: folderID, Path: "secret.txt", Index: 0, Offset: 0, Length: testChunkSize,
	})
	if !errors.As(err, &refusal) || refusal.Reason != ports.ChunkFolderUnknown {
		t.Fatalf("chunk of %s from a = %v, want a %s refusal", folderID, err, ports.ChunkFolderUnknown)
	}
	t.Logf("c asking a for a chunk of %s directly: %v", folderID, err)

	// 4. And nothing of docs is on disk on c.
	if _, err := os.Lstat(filepath.Join(c.Extra, "secret.txt")); err == nil {
		t.Fatal("c holds a file from a folder it was never authorized for")
	}
}

// TestFolderAuth_RevokeStopsAnInFlightSync revokes a device in the middle of a
// transfer. The pacing makes the transfer take seconds, so the revocation
// lands while chunks are still moving, and the file must never complete.
func TestFolderAuth_RevokeStopsAnInFlightSync(t *testing.T) {
	if testing.Short() {
		t.Skip("timing test")
	}
	a := startNodeWith(t, "a", nodeConfig{
		Mode: folder.ModeSendReceive, ChunkSize: limitedChunkSize, UploadLimit: limitedRate,
	})
	b := startNodeWith(t, "b", nodeConfig{Mode: folder.ModeSendReceive, ChunkSize: limitedChunkSize})
	pair(t, a, b)

	data := payload(limitedSize, 43)
	writeFile(t, a, "big.bin", data)

	// Wait until b has really moved bytes of the file, so the revocation
	// lands on a transfer in flight rather than on one about to start.
	partial := filepath.Join(b.Root, "big.bin")
	done := progress(t, b, "big.bin")
	waitFor(t, "b to be pulling big.bin", convergeTimeout, func() bool {
		done = progress(t, b, "big.bin")
		return done > 0
	})
	t.Logf("revoking with %d of %d bytes transferred", done, len(data))

	// Only a's grant matters: a is the side that serves. b keeps its own,
	// so the test proves the serving side refused rather than the pulling
	// side giving up.
	if err := a.App.Folders.Revoke(context.Background(), folderID, b.ID); err != nil {
		t.Fatalf("revoke on a: %v", err)
	}

	// The transfer had 20 MiB to move at 4 MiB/s. Five seconds of watching
	// is more than a whole transfer's worth, so a file that is not complete
	// by then was stopped, not merely slowed.
	deadline := time.Now().Add(5*time.Second + 5*time.Second*timeoutScale)
	for time.Now().Before(deadline) {
		if got, err := os.ReadFile(partial); err == nil && len(got) == len(data) {
			t.Fatal("b completed a file after its authorization was revoked")
		}
		time.Sleep(50 * time.Millisecond)
	}

	// The peers are still connected and still paired: only the folder went.
	report := b.Status()
	if len(report.Peers) != 1 {
		t.Fatalf("b knows %d peers, want the one it is still paired with", len(report.Peers))
	}
}

// progress is how many bytes of one file a node's transfer journal says have
// been verified and written. A pull that has moved nothing has no journal row
// and returns zero.
func progress(t *testing.T, n *node, rel string) int64 {
	t.Helper()
	list, err := n.App.Transfers.List(context.Background(), transfers.Filter{})
	if err != nil {
		t.Fatalf("list transfers on %s: %v", n.name, err)
	}
	for _, v := range list {
		if v.Path == rel {
			return v.BytesDone
		}
	}
	return 0
}
