package runner

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/wesbragagt/syncthis/internal/bench/fixture"
)

// entry is what treeSnapshot records about one file, enough to tell whether
// two folder roots hold the same content without hashing every byte on every
// poll.
type entry struct {
	size int64
}

// treeSnapshot walks root and describes every regular file in it, keyed by
// its path relative to root. The bench state directory ".syncthis" is
// skipped: it is bookkeeping, not synchronized content.
func treeSnapshot(root string) (map[string]entry, error) {
	out := make(map[string]entry)
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			if info.Name() == ".syncthis" {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		out[filepath.ToSlash(rel)] = entry{size: info.Size()}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("read tree %s: %w", root, err)
	}
	return out, nil
}

// treesEqual reports whether two folder roots hold the same paths and sizes.
func treesEqual(a, b map[string]entry) bool {
	if len(a) != len(b) {
		return false
	}
	for path, ea := range a {
		eb, ok := b[path]
		if !ok || ea.size != eb.size {
			return false
		}
	}
	return true
}

// waitConverged polls both nodes until their folders hold the same content
// and neither is transferring, or the timeout expires.
func waitConverged(ctx context.Context, a, b *node, timeout, poll time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		treeA, err := treeSnapshot(a.root)
		if err != nil {
			return err
		}
		treeB, err := treeSnapshot(b.root)
		if err != nil {
			return err
		}
		idleA, err := a.idle(ctx)
		if err != nil {
			return err
		}
		idleB, err := b.idle(ctx)
		if err != nil {
			return err
		}
		if treesEqual(treeA, treeB) && idleA && idleB {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s and %s did not converge within %s (want %d files vs %d files)",
				a.name, b.name, timeout, len(treeA), len(treeB))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(poll):
		}
	}
}

// mutationTargets deterministically picks distinct files from a corpus for
// the mutation set: one to rename, one to delete, one to modify and one to
// editor-save. It returns fewer than four only when the corpus itself holds
// fewer than four files.
func mutationTargets(files []fixture.FileResult, seed int64) []fixture.FileResult {
	if len(files) == 0 {
		return nil
	}
	// A tiny linear congruential shuffle is enough here: the goal is a
	// stable, seed-derived pick, not statistical quality.
	order := make([]int, len(files))
	for i := range order {
		order[i] = i
	}
	state := uint64(seed)
	for i := len(order) - 1; i > 0; i-- {
		state = state*6364136223846793005 + 1442695040888963407
		j := int(state % uint64(i+1))
		order[i], order[j] = order[j], order[i]
	}
	n := 4
	if n > len(order) {
		n = len(order)
	}
	out := make([]fixture.FileResult, n)
	for i := 0; i < n; i++ {
		out[i] = files[order[i]]
	}
	return out
}

// waitActiveTransfer polls n until its status shows an active transfer, or
// the timeout expires. It reports whether one was observed, so a caller can
// tell "the kill really hit a transfer in flight" from "nothing was moving,
// so there is nothing to recover from and no recovery number to report."
func waitActiveTransfer(ctx context.Context, n *node, timeout, poll time.Duration) (bool, error) {
	deadline := time.Now().Add(timeout)
	for {
		active, err := n.hasActiveTransfer(ctx)
		if err != nil {
			return false, err
		}
		if active {
			return true, nil
		}
		if time.Now().After(deadline) {
			return false, nil
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(poll):
		}
	}
}
