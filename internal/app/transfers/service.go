// Package transfers is the read model behind `syncthis transfer list` and
// `syncthis transfer show`: it turns journal rows into what the CLI prints.
package transfers

import (
	"context"
	"fmt"
	"time"

	"github.com/wesbragagt/syncthis/internal/app/ports"
	"github.com/wesbragagt/syncthis/internal/domain/folder"
	"github.com/wesbragagt/syncthis/internal/domain/transfer"
)

// Filter narrows a listing. Zero fields mean "no filter".
type Filter = ports.TransferFilter

// View is one transfer as reported to the user. MissingChunks is only filled
// in by Show, because a listing of large transfers would carry thousands of
// indexes nobody reads.
type View struct {
	ID            string
	FolderID      folder.ID
	Path          string
	PeerID        string
	State         transfer.State
	Size          int64
	BytesDone     int64
	ChunkSize     int
	ChunksTotal   int
	ChunksDone    int
	Error         string
	CreatedAt     time.Time
	UpdatedAt     time.Time
	MissingChunks []int
}

// Service reads the transfer journal.
type Service struct {
	store ports.TransferStore
}

// New returns a service reading from store.
func New(store ports.TransferStore) *Service { return &Service{store: store} }

// List returns the transfers matching filter, ordered by id. A finished
// transfer is removed from the journal once its file is installed, so a
// listing holds the live and the failed ones.
func (s *Service) List(ctx context.Context, f Filter) ([]View, error) {
	list, err := s.store.ListTransfers(ctx, f)
	if err != nil {
		return nil, err
	}
	out := make([]View, 0, len(list))
	for _, t := range list {
		out = append(out, viewOf(t, false))
	}
	return out, nil
}

// Show returns one transfer including the indexes of the chunks it still
// needs. It fails with ports.ErrNotFound when no transfer has that id.
func (s *Service) Show(ctx context.Context, id transfer.ID) (View, error) {
	t, err := s.store.GetTransfer(ctx, id)
	if err != nil {
		return View{}, fmt.Errorf("transfer %q: %w", id, err)
	}
	return viewOf(t, true), nil
}

// viewOf converts a journal row into a view.
func viewOf(t transfer.Transfer, withChunks bool) View {
	missing := t.MissingChunks()
	view := View{
		ID:          string(t.ID),
		FolderID:    t.FolderID,
		Path:        t.Path,
		PeerID:      string(t.PeerID),
		State:       t.State,
		Size:        t.Size,
		BytesDone:   t.BytesDone(),
		ChunkSize:   t.ChunkSize,
		ChunksTotal: len(t.Chunks),
		ChunksDone:  len(t.Chunks) - len(missing),
		Error:       t.Error,
		CreatedAt:   t.CreatedAt,
		UpdatedAt:   t.UpdatedAt,
	}
	if withChunks {
		if missing == nil {
			missing = []int{}
		}
		view.MissingChunks = missing
	}
	return view
}
