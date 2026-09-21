package syncer

import (
	"context"
	"errors"
	"fmt"

	"github.com/wesbragagt/syncthis/internal/app/ports"
	"github.com/wesbragagt/syncthis/internal/domain/device"
	"github.com/wesbragagt/syncthis/internal/domain/folder"
	"github.com/wesbragagt/syncthis/internal/domain/syncplan"
)

// PlanStore is the persistence a planner reads. ports.MetadataStore satisfies
// it.
type PlanStore interface {
	ports.FolderStore
	ports.FileStore
	ports.DeviceStore
}

// PeerPlan is what one peer's manifest means for a local folder.
type PeerPlan struct {
	PeerID  device.ID
	Name    string
	Address string
	// Items is what the local side would do. It is empty when the peer
	// does not serve the folder, in which case Note says why.
	Items []syncplan.Item
	Note  string
}

// Changes counts the items that would change something on disk.
func (p PeerPlan) Changes() int {
	n := 0
	for _, item := range p.Items {
		if item.Action != syncplan.ActionNone {
			n++
		}
	}
	return n
}

// Planner answers what a sync would do without doing it. It dials the peers
// itself, so it works whether or not a daemon runs.
type Planner struct {
	store  PlanStore
	dialer ports.Dialer
}

// NewPlanner returns a planner dialing through dialer.
func NewPlanner(store PlanStore, dialer ports.Dialer) *Planner {
	return &Planner{store: store, dialer: dialer}
}

// Plan asks one peer, or every known peer when peer is empty, for its
// manifest and returns what this device would do with it. Nothing is written:
// neither the folder nor the journal is touched.
//
// A peer that cannot be reached fails the call. A peer that is reachable but
// does not serve the folder is reported with a note and no items.
func (p *Planner) Plan(ctx context.Context, id folder.ID, peer device.ID) ([]PeerPlan, error) {
	f, err := p.store.GetFolder(ctx, id)
	if err != nil {
		return nil, err
	}
	local, err := p.store.LoadManifest(ctx, id)
	if err != nil {
		return nil, err
	}
	targets, err := p.targets(ctx, peer)
	if err != nil {
		return nil, err
	}

	out := make([]PeerPlan, 0, len(targets))
	for _, target := range targets {
		plan := PeerPlan{PeerID: target.ID, Name: target.Name, Address: target.Address}
		conn, err := p.dialer.Dial(ctx, target.Address, target.ID)
		if err != nil {
			return nil, fmt.Errorf("contacting %s at %s: %w", target.ID.Short(), target.Address, err)
		}
		remote, err := conn.RequestManifest(ctx, id)
		_ = conn.Close()
		switch {
		case err == nil:
			plan.Items = syncplan.Plan(local, remote, f)
		case refusedFolder(err):
			plan.Note = err.Error()
		default:
			return nil, fmt.Errorf("manifest of %q from %s: %w", id, target.ID.Short(), err)
		}
		out = append(out, plan)
	}
	return out, nil
}

// targets returns the peers to ask: the named one, or every known peer.
func (p *Planner) targets(ctx context.Context, peer device.ID) ([]device.Device, error) {
	if peer != "" {
		known, err := p.store.GetDevice(ctx, peer)
		if err != nil {
			return nil, err
		}
		return []device.Device{known}, nil
	}
	return p.store.ListDevices(ctx)
}

// refusedFolder reports whether a peer answered that it does not serve the
// folder, which is an answer rather than a failure.
func refusedFolder(err error) bool {
	return errors.Is(err, ports.ErrFolderUnknown) ||
		errors.Is(err, ports.ErrFolderReceiveOnly) ||
		errors.Is(err, ports.ErrPaused)
}
