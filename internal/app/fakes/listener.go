package fakes

import (
	"context"
	"sync"

	"github.com/wesbragagt/syncthis/internal/app/ports"
	"github.com/wesbragagt/syncthis/internal/domain/folder"
)

var _ ports.Listener = (*Listener)(nil)

// change is one folder change a listener was asked to broadcast.
type change struct {
	ID       folder.ID
	Sequence uint64
}

// Listener is a ports.Listener that accepts nothing and records what it was
// asked to tell the peers.
type Listener struct {
	mu        sync.Mutex
	changes   []change
	announced [][]ports.FolderAdvert
}

// NewListener returns a listener that serves no connections.
func NewListener() *Listener { return &Listener{} }

// Serve blocks until ctx is done, like a real listener with no callers.
func (l *Listener) Serve(ctx context.Context, _ ports.PeerHandler, _ ports.DeviceLookup) error {
	<-ctx.Done()
	return nil
}

// Addr returns a fixed address.
func (l *Listener) Addr() string { return "127.0.0.1:0" }

// Broadcast records a folder change notification.
func (l *Listener) Broadcast(id folder.ID, seq uint64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.changes = append(l.changes, change{ID: id, Sequence: seq})
}

// AnnounceFolders records one folder announcement.
func (l *Listener) AnnounceFolders(adverts []ports.FolderAdvert) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.announced = append(l.announced, append([]ports.FolderAdvert(nil), adverts...))
}

// Announcements returns the folder lists AnnounceFolders was called with.
func (l *Listener) Announcements() [][]ports.FolderAdvert {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([][]ports.FolderAdvert(nil), l.announced...)
}
