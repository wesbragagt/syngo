package fakes

import (
	"context"
	"sync"

	"github.com/wesbragagt/syngo/internal/app/ports"
	"github.com/wesbragagt/syngo/internal/domain/device"
	"github.com/wesbragagt/syngo/internal/domain/folder"
)

var (
	_ ports.Listener      = (*Listener)(nil)
	_ ports.BoundListener = (*Listener)(nil)
)

// change is one folder change a listener was asked to broadcast.
type change struct {
	ID       folder.ID
	Sequence uint64
}

// Listener is a ports.Listener that accepts nothing and records what it was
// asked to tell the peers.
type Listener struct {
	addr      string
	closeOnce sync.Once
	closed    chan struct{}

	mu        sync.Mutex
	changes   []change
	announced [][]ports.FolderAdvert
	retained  [][]device.ID
}

// NewListener returns a listener that serves no connections.
func NewListener() *Listener { return NewListenerAt("127.0.0.1:0") }

// NewListenerAt returns a listener that reports the given bound address.
func NewListenerAt(addr string) *Listener {
	return &Listener{addr: addr, closed: make(chan struct{})}
}

// Serve blocks until ctx is done or the listener is closed, like a real
// listener with no callers.
func (l *Listener) Serve(ctx context.Context, _ ports.PeerHandler, _ ports.DeviceLookup) error {
	select {
	case <-ctx.Done():
	case <-l.closed:
	}
	return nil
}

// Close makes Serve return, the way closing a socket does. It is safe to call
// more than once.
func (l *Listener) Close() error {
	l.closeOnce.Do(func() { close(l.closed) })
	return nil
}

// Closed reports whether Close was called.
func (l *Listener) Closed() bool {
	select {
	case <-l.closed:
		return true
	default:
		return false
	}
}

// Addr returns the bound address.
func (l *Listener) Addr() string { return l.addr }

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

// RetainDevices records which peers would have been kept connected.
func (l *Listener) RetainDevices(ids []device.ID) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.retained = append(l.retained, append([]device.ID(nil), ids...))
}

// Retained returns the device lists RetainDevices was called with.
func (l *Listener) Retained() [][]device.ID {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([][]device.ID(nil), l.retained...)
}

// Changes returns the folder change notifications Broadcast was asked for.
func (l *Listener) Changes() []change {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]change(nil), l.changes...)
}

// Announcements returns the folder lists AnnounceFolders was called with.
func (l *Listener) Announcements() [][]ports.FolderAdvert {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([][]ports.FolderAdvert(nil), l.announced...)
}
