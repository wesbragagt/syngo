package runner

import (
	"fmt"

	"github.com/wesbragagt/syngo/internal/bench/fixture"
)

// applyMutationSet renames, deletes, modifies and editor-saves one file each
// from targets, in that order, under root. It is the mutation set
// docs/BENCH.md and .specs/tasks.md ask a scenario to exercise.
func applyMutationSet(root string, targets []fixture.FileResult, seed int64) ([]fixture.MutationResult, error) {
	var results []fixture.MutationResult
	apply := func(op string, do func(fixture.FileResult) (fixture.MutationResult, error), i int) error {
		if i >= len(targets) {
			return nil
		}
		result, err := do(targets[i])
		if err != nil {
			return fmt.Errorf("mutation set: %s: %w", op, err)
		}
		results = append(results, result)
		return nil
	}

	if err := apply("rename", func(f fixture.FileResult) (fixture.MutationResult, error) {
		return fixture.Rename(root, f.Path, f.Path+".renamed")
	}, 0); err != nil {
		return nil, err
	}
	if err := apply("delete", func(f fixture.FileResult) (fixture.MutationResult, error) {
		return fixture.Delete(root, f.Path)
	}, 1); err != nil {
		return nil, err
	}
	if err := apply("modify", func(f fixture.FileResult) (fixture.MutationResult, error) {
		return fixture.Modify(root, f.Path, seed, int(max64(f.Size, 1)))
	}, 2); err != nil {
		return nil, err
	}
	if err := apply("editor-save", func(f fixture.FileResult) (fixture.MutationResult, error) {
		return fixture.EditorSave(root, f.Path, seed, int(max64(f.Size, 1)))
	}, 3); err != nil {
		return nil, err
	}
	return results, nil
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
