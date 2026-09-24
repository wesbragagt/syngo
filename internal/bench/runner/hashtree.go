package runner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// hashTree returns the SHA-256 of every regular file under root, keyed by its
// path relative to root. It is what makes a convergence claim checkable: two
// sides that report the same file count can still hold different bytes, and
// only a content hash can tell.
//
// The ".syngo" state directory is skipped on both sides: it is
// bookkeeping, not synchronized content.
func hashTree(root string) (map[string]string, error) {
	out := make(map[string]string)
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			if info.Name() == ".syngo" {
				return filepath.SkipDir
			}
			return nil
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		sum, err := hashFile(path)
		if err != nil {
			return err
		}
		out[filepath.ToSlash(rel)] = sum
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("hash tree %s: %w", root, err)
	}
	return out, nil
}

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// parseHashLines reads the output of `sha256sum`, one "<hash>  <path>" line
// per file, and returns it keyed by path relative to the folder root. The
// leading "./" find prints is stripped, so the keys match hashTree's.
func parseHashLines(out string) (map[string]string, error) {
	hashes := make(map[string]string)
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		sum, path, found := strings.Cut(line, "  ")
		if !found {
			return nil, fmt.Errorf("unreadable hash line %q, want \"<sha256>  <path>\"", line)
		}
		path = strings.TrimPrefix(path, "./")
		if sum == "" || path == "" {
			return nil, fmt.Errorf("unreadable hash line %q, want \"<sha256>  <path>\"", line)
		}
		hashes[path] = sum
	}
	return hashes, nil
}

// hashesEqual reports whether two folder roots hold exactly the same paths
// with exactly the same content, and describes the first difference it finds
// when they do not.
func hashesEqual(local, remote map[string]string) (bool, string) {
	var missing, differing []string
	for path, sum := range local {
		other, ok := remote[path]
		switch {
		case !ok:
			missing = append(missing, path)
		case other != sum:
			differing = append(differing, path)
		}
	}
	var extra []string
	for path := range remote {
		if _, ok := local[path]; !ok {
			extra = append(extra, path)
		}
	}
	if len(missing) == 0 && len(differing) == 0 && len(extra) == 0 {
		return true, ""
	}
	sort.Strings(missing)
	sort.Strings(differing)
	sort.Strings(extra)
	return false, fmt.Sprintf("%d file(s) only here (e.g. %s), %d only on the peer (e.g. %s), %d with different content (e.g. %s)",
		len(missing), first(missing), len(extra), first(extra), len(differing), first(differing))
}

func first(s []string) string {
	if len(s) == 0 {
		return "-"
	}
	return s[0]
}

// sideState is what one machine reports about the shared folder: enough to
// decide whether it has settled, before the content hashes decide whether the
// two sides actually agree.
type sideState struct {
	files          int64
	pendingChanges int
	active         int
	pending        int
}

// settled reports whether this side has nothing in flight and nothing it
// failed to apply.
func (s sideState) settled() bool {
	return s.active == 0 && s.pending == 0 && s.pendingChanges == 0
}

// localSideState summarizes the in-process node the same way the peer's
// status document is summarized.
func localSideState(ctx context.Context, n *node) (sideState, error) {
	report, err := n.status(ctx)
	if err != nil {
		return sideState{}, err
	}
	s := sideState{active: report.Transfers.Active, pending: report.Transfers.Pending}
	for _, f := range report.Folders {
		if f.ID == string(FolderID) {
			s.files = f.Files
			s.pendingChanges = f.PendingChanges
		}
	}
	return s, nil
}

// waitConvergedRemote polls `status --json` on both machines until neither
// has a pending change or a transfer in flight and both report the same file
// count, then compares the two trees by content hash. Only when the hashes
// match does it report a convergence, so a run can never call two different
// trees converged.
func waitConvergedRemote(ctx context.Context, n *node, r *remoteDevice, timeout, poll time.Duration) error {
	deadline := time.Now().Add(timeout)
	var lastReason string
	for {
		local, err := localSideState(ctx, n)
		if err != nil {
			return fmt.Errorf("local status: %w", err)
		}
		remote, err := r.state(ctx)
		if err != nil {
			return fmt.Errorf("peer status: %w", err)
		}

		switch {
		case !local.settled():
			lastReason = fmt.Sprintf("this machine is still busy (active=%d pending=%d pending_changes=%d)",
				local.active, local.pending, local.pendingChanges)
		case !remote.settled():
			lastReason = fmt.Sprintf("%s is still busy (active=%d pending=%d pending_changes=%d)",
				r.host, remote.active, remote.pending, remote.pendingChanges)
		case local.files != remote.files:
			lastReason = fmt.Sprintf("file counts differ: %d here, %d on %s", local.files, remote.files, r.host)
		default:
			localHashes, err := hashTree(n.root)
			if err != nil {
				return err
			}
			remoteHashes, err := r.hashes(ctx)
			if err != nil {
				return err
			}
			equal, diff := hashesEqual(localHashes, remoteHashes)
			if equal {
				return nil
			}
			lastReason = "both sides are idle but the trees differ: " + diff
		}

		if time.Now().After(deadline) {
			return fmt.Errorf("this machine and %s did not converge within %s: %s", r.host, timeout, lastReason)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(poll):
		}
	}
}
