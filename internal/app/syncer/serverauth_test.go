package syncer_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/wesbragagt/syngo/internal/app/fakes"
	"github.com/wesbragagt/syngo/internal/app/ports"
	"github.com/wesbragagt/syngo/internal/domain/device"
	"github.com/wesbragagt/syngo/internal/domain/folder"
)

// strangerID is a device that is paired but was never granted anything.
const strangerID = device.ID("STRANGER")

// stranger adds a device to the source without authorizing it for a folder.
func (s *source) stranger(t *testing.T) device.ID {
	t.Helper()
	err := s.store.SaveDevice(context.Background(), device.Device{ID: strangerID, Name: "stranger"})
	if err != nil {
		t.Fatalf("save device: %v", err)
	}
	return strangerID
}

func TestHello_NamesOnlyTheFoldersThePeerIsAuthorizedFor(t *testing.T) {
	ctx := context.Background()
	s := newSource(t, folder.ModeSendReceive)
	if err := s.store.SaveFolder(ctx, testFolder("photos", false)); err != nil {
		t.Fatalf("save folder: %v", err)
	}
	// photos is granted to nobody, so it is named to nobody.
	hello := s.server.Hello(ctx, askerID)
	for _, a := range hello.Folders {
		if a.ID == "photos" {
			t.Fatalf("hello names %s, which the peer was never granted", a.ID)
		}
	}

	// A paired device with no grant at all is told about nothing.
	if got := s.server.Hello(ctx, s.stranger(t)).Folders; len(got) != 0 {
		t.Fatalf("hello to an unauthorized peer = %+v, want no folders", got)
	}
}

// TestHello_AnUnauthenticatedPeerIsToldNothing covers the empty device id: a
// caller that has not authenticated names nobody and must not be answered as
// if it were everybody.
func TestHello_AnUnauthenticatedPeerIsToldNothing(t *testing.T) {
	s := newSource(t, folder.ModeSendReceive)

	hello := s.server.Hello(context.Background(), "")
	if len(hello.Folders) != 0 {
		t.Fatalf("hello to nobody = %+v, want no folders", hello.Folders)
	}
	if hello.DeviceID != "SOURCE" {
		t.Fatalf("hello = %+v, want the local identity even with no folders", hello)
	}
}

// TestManifest_UnauthorizedPeerIsRefusedAsUnknown is the rule a peer must not
// be able to work around: asking by name for a folder it was never told about
// gets the answer a folder that does not exist gets, so nothing leaks.
func TestManifest_UnauthorizedPeerIsRefusedAsUnknown(t *testing.T) {
	ctx := context.Background()
	s := newSource(t, folder.ModeSendReceive)
	s.addFile(t, "secret.txt", []byte("not yours"))
	stranger := s.stranger(t)

	_, err := s.server.Manifest(ctx, stranger, s.folder.ID)
	if !errors.Is(err, ports.ErrFolderUnknown) {
		t.Fatalf("manifest for an unauthorized peer = %v, want ErrFolderUnknown", err)
	}
	// Word for word what a folder that does not exist answers, once the id
	// each message quotes is taken out: the id is what the peer asked for,
	// so it carries nothing the peer does not already know.
	_, missing := s.server.Manifest(ctx, stranger, "nope")
	refused := strings.Replace(err.Error(), string(s.folder.ID), "ID", 1)
	absent := strings.Replace(missing.Error(), "nope", "ID", 1)
	if refused != absent {
		t.Fatalf("an unauthorized folder answers %q and a missing one %q; a peer can tell them apart",
			refused, absent)
	}
}

func TestChunk_UnauthorizedPeerIsRefusedAsUnknown(t *testing.T) {
	ctx := context.Background()
	s := newSource(t, folder.ModeSendReceive)
	entry := s.addFile(t, "secret.txt", []byte("not yours"))
	stranger := s.stranger(t)

	_, err := s.server.Chunk(ctx, stranger, ports.ChunkRequest{
		FolderID: s.folder.ID, Path: entry.Path, Hash: entry.Hash,
		Index: 0, Offset: 0, Length: int(entry.Size),
	})
	var refusal *ports.ChunkError
	if !errors.As(err, &refusal) || refusal.Reason != ports.ChunkFolderUnknown {
		t.Fatalf("chunk for an unauthorized peer = %v, want a %s refusal", err, ports.ChunkFolderUnknown)
	}
}

// TestChunks_UnauthorizedPeerIsRefusedAsUnknown covers the request that came
// with content-defined chunking: the chunk list is content too, since the
// boundaries describe the bytes. It is refused for an unauthorized peer
// exactly as the content itself is.
func TestChunks_UnauthorizedPeerIsRefusedAsUnknown(t *testing.T) {
	ctx := context.Background()
	s := newSource(t, folder.ModeSendReceive)
	entry := s.addFile(t, "secret.txt", []byte("not yours"))
	stranger := s.stranger(t)

	chunks, err := s.server.Chunks(ctx, stranger, ports.ChunkListRequest{
		FolderID: s.folder.ID, Path: entry.Path, Hash: entry.Hash,
	})
	if len(chunks) != 0 {
		t.Fatalf("chunk list for an unauthorized peer = %+v, want nothing", chunks)
	}
	var refusal *ports.ChunkError
	if !errors.As(err, &refusal) || refusal.Reason != ports.ChunkFolderUnknown {
		t.Fatalf("chunk list for an unauthorized peer = %v, want a %s refusal", err, ports.ChunkFolderUnknown)
	}
}

// TestSendable_RevokingTakesEffectOnTheNextRequest is why the check is made
// per request and not per handshake: a peer that is already connected stops
// being served the moment the grant goes.
func TestSendable_RevokingTakesEffectOnTheNextRequest(t *testing.T) {
	ctx := context.Background()
	s := newSource(t, folder.ModeSendReceive)
	entry := s.addFile(t, "a.txt", []byte("hello"))

	if _, err := s.server.Manifest(ctx, askerID, s.folder.ID); err != nil {
		t.Fatalf("manifest before the revocation: %v", err)
	}

	if err := s.store.RevokeFolderDevice(ctx, s.folder.ID, askerID); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	if _, err := s.server.Manifest(ctx, askerID, s.folder.ID); !errors.Is(err, ports.ErrFolderUnknown) {
		t.Fatalf("manifest after the revocation = %v, want ErrFolderUnknown", err)
	}
	_, err := s.server.Chunk(ctx, askerID, ports.ChunkRequest{
		FolderID: s.folder.ID, Path: entry.Path, Hash: entry.Hash,
		Index: 0, Offset: 0, Length: int(entry.Size),
	})
	var refusal *ports.ChunkError
	if !errors.As(err, &refusal) || refusal.Reason != ports.ChunkFolderUnknown {
		t.Fatalf("chunk after the revocation = %v, want a %s refusal", err, ports.ChunkFolderUnknown)
	}
	_, err = s.server.Chunks(ctx, askerID, ports.ChunkListRequest{
		FolderID: s.folder.ID, Path: entry.Path, Hash: entry.Hash,
	})
	if !errors.As(err, &refusal) || refusal.Reason != ports.ChunkFolderUnknown {
		t.Fatalf("chunk list after the revocation = %v, want a %s refusal", err, ports.ChunkFolderUnknown)
	}
	// The other device keeps what it was granted.
	if _, err := s.server.Manifest(ctx, fakes.SelfID, s.folder.ID); err != nil {
		t.Fatalf("revoking one device stopped another: %v", err)
	}
}
