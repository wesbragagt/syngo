package e2e

import (
	"context"
	"encoding/json"
	"maps"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/wesbragagt/syncthis/internal/adapters/blake3hash"
	"github.com/wesbragagt/syncthis/internal/cli"
	"github.com/wesbragagt/syncthis/internal/domain/conflict"
	"github.com/wesbragagt/syncthis/internal/domain/folder"
)

// settle is how long a converged pair is watched to confirm that it stays that
// way and stops transferring.
var settle = 500 * time.Millisecond * timeoutScale

// conflictRow is one row of `conflict list --json`.
type conflictRow struct {
	ID           string `json:"id"`
	FolderID     string `json:"folder_id"`
	Path         string `json:"path"`
	ConflictPath string `json:"conflict_path"`
	Reason       string `json:"reason"`
	Resolved     bool   `json:"resolved"`
	Stale        bool   `json:"stale"`
}

// pauseFolder stops a node from syncing and serving its folder, which is how
// these tests make two devices diverge.
func pauseFolder(t *testing.T, n *node) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), daemonTimeout)
	defer cancel()
	if err := n.App.Control.Pause(ctx, folderID); err != nil {
		t.Fatalf("pause %s: %v", n.name, err)
	}
}

// resumeFolder lets a node sync again and rescans what changed meanwhile.
func resumeFolder(t *testing.T, n *node) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), daemonTimeout)
	defer cancel()
	if err := n.App.Control.Resume(ctx, folderID); err != nil {
		t.Fatalf("resume %s: %v", n.name, err)
	}
}

// conflictsOf returns what `conflict list --json` reports on a node.
func conflictsOf(t *testing.T, n *node) []conflictRow {
	t.Helper()
	out := cliOK(t, n, "conflict", "list", "--json")
	var rows []conflictRow
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("conflict list on %s is not json: %v\n%s", n.name, err, out)
	}
	return rows
}

// contentsOf returns the content of every file of a folder, keyed by path.
func contentsOf(t *testing.T, n *node) map[string]string {
	t.Helper()
	out := make(map[string]string)
	for rel, entry := range readTree(t, n.Root) {
		if entry.Kind != 1 {
			continue
		}
		data, err := os.ReadFile(filepath.Join(n.Root, rel))
		if err != nil {
			t.Fatalf("read %s on %s: %v", rel, n.name, err)
		}
		out[rel] = string(data)
	}
	return out
}

// holds reports whether any file of a node holds the given content.
func holds(files map[string]string, content string) bool {
	for _, got := range files {
		if got == content {
			return true
		}
	}
	return false
}

// copiesIn returns the conflict copies among a set of paths.
func copiesIn(files map[string]string) []string {
	var out []string
	for rel := range files {
		if conflict.IsCopy(rel) {
			out = append(out, rel)
		}
	}
	sort.Strings(out)
	return out
}

// keepsPath reports whether the first content is the one that keeps the path
// when both versions collide, which is the rule both devices apply.
func keepsPath(a, b string) bool {
	hasher := blake3hash.New()
	return conflict.KeepsPath(hasher.SumBytes([]byte(a)), hasher.SumBytes([]byte(b)))
}

// idle reports whether a node is neither transferring nor waiting to.
func idle(t *testing.T, n *node) bool {
	t.Helper()
	report := n.Status()
	return report.Transfers.Active == 0 && report.Transfers.Pending == 0
}

// waitConverged waits until both nodes hold the same tree and neither is
// transferring any more.
func waitConverged(t *testing.T, a, b *node) {
	t.Helper()
	waitTreeEqual(t, a, b)
	waitFor(t, "both nodes to stop transferring", convergeTimeout, func() bool {
		return idle(t, a) && idle(t, b)
	})
}

// diverged is the state the conflict tests start from: both nodes held the
// same file, then each changed it while the other could not be reached.
type diverged struct {
	a, b *node
	// keeper is the node whose version keeps the path, loser the one whose
	// version is written to a conflict copy.
	keeper, loser *node
	kept, moved   string
}

// divergeOn makes both nodes change the same path to different content while
// they cannot see each other, and returns who will keep the path.
func divergeOn(t *testing.T, rel string) diverged {
	t.Helper()
	a := startNode(t, "a", folder.ModeSendReceive)
	b := startNode(t, "b", folder.ModeSendReceive)
	writeFile(t, a, rel, []byte("the version both sides had"))
	pair(t, a, b)
	waitTreeEqual(t, a, b)

	pauseFolder(t, b)
	fromA, fromB := "the version device a wrote", "the version device b wrote"
	writeFile(t, a, rel, []byte(fromA))
	writeFile(t, b, rel, []byte(fromB))
	resumeFolder(t, b)

	d := diverged{a: a, b: b, keeper: a, loser: b, kept: fromA, moved: fromB}
	if !keepsPath(fromA, fromB) {
		d = diverged{a: a, b: b, keeper: b, loser: a, kept: fromB, moved: fromA}
	}
	return d
}

func TestTwoWay_IndependentEditsConverge(t *testing.T) {
	a := startNode(t, "a", folder.ModeSendReceive)
	b := startNode(t, "b", folder.ModeSendReceive)
	writeFile(t, a, "x.txt", []byte("x first"))
	writeFile(t, a, "y.txt", []byte("y first"))
	pair(t, a, b)
	waitTreeEqual(t, a, b)

	writeFile(t, a, "x.txt", []byte("x edited on a"))
	writeFile(t, b, "y.txt", []byte("y edited on b"))
	waitConverged(t, a, b)

	files := contentsOf(t, a)
	if files["x.txt"] != "x edited on a" || files["y.txt"] != "y edited on b" {
		t.Fatalf("files = %v, want both edits", files)
	}
	if copies := copiesIn(files); len(copies) != 0 {
		t.Fatalf("conflict copies = %v, want none: the edits did not collide", copies)
	}
	for _, n := range []*node{a, b} {
		if rows := conflictsOf(t, n); len(rows) != 0 {
			t.Fatalf("%s reports %+v, want no conflicts", n.name, rows)
		}
	}
}

func TestTwoWay_ConcurrentEditKeepsBoth(t *testing.T) {
	d := divergeOn(t, "shared.txt")
	waitConverged(t, d.a, d.b)

	for _, n := range []*node{d.a, d.b} {
		files := contentsOf(t, n)
		if len(files) != 2 {
			t.Fatalf("%s holds %v, want the path and one conflict copy", n.name, files)
		}
		if files["shared.txt"] != d.kept {
			t.Fatalf("%s: shared.txt = %q, want %q", n.name, files["shared.txt"], d.kept)
		}
		copies := copiesIn(files)
		if len(copies) != 1 || files[copies[0]] != d.moved {
			t.Fatalf("%s: copies = %v of %v, want one holding %q", n.name, copies, files, d.moved)
		}
	}

	// The device that moved its version aside is the one with something to
	// decide; the other one received the copy as an ordinary file.
	rows := conflictsOf(t, d.loser)
	if len(rows) != 1 || rows[0].Path != "shared.txt" || rows[0].Reason != "both-modified" {
		t.Fatalf("%s reports %+v, want one both-modified conflict", d.loser.name, rows)
	}
	if rows[0].ConflictPath == "" || !conflict.IsCopy(rows[0].ConflictPath) {
		t.Fatalf("conflict = %+v, want it to name the copy", rows[0])
	}
	if report := d.loser.Status(); report.Conflicts.Unresolved != 1 {
		t.Fatalf("%s reports %d unresolved conflicts, want 1", d.loser.name, report.Conflicts.Unresolved)
	}
	if rows := conflictsOf(t, d.keeper); len(rows) != 0 {
		t.Fatalf("%s reports %+v, want nothing to decide on the side that kept the path",
			d.keeper.name, rows)
	}

	// Nothing keeps moving: no further copy is written and no further file
	// is transferred.
	before := contentsOf(t, d.a)
	completed := d.a.Events.Count("transfer.completed") + d.b.Events.Count("transfer.completed")
	time.Sleep(settle)
	if after := contentsOf(t, d.a); len(after) != len(before) {
		t.Fatalf("the folder went from %v to %v while nothing happened", before, after)
	}
	if now := d.a.Events.Count("transfer.completed") + d.b.Events.Count("transfer.completed"); now != completed {
		t.Fatalf("%d transfers finished after convergence, want none", now-completed)
	}
	if diff := treeDiff(t, d.a.Root, d.b.Root); len(diff) != 0 {
		t.Fatalf("the trees drifted apart again:\n%s", join(diff))
	}
}

func TestTwoWay_ResolveKeepRemoteLeavesOneVersion(t *testing.T) {
	d := divergeOn(t, "shared.txt")
	waitConverged(t, d.a, d.b)

	rows := conflictsOf(t, d.loser)
	if len(rows) != 1 {
		t.Fatalf("%s reports %+v, want one conflict", d.loser.name, rows)
	}
	cliOK(t, d.loser, "conflict", "resolve", rows[0].ID, "--keep", "remote", "--yes")
	waitConverged(t, d.a, d.b)

	for _, n := range []*node{d.a, d.b} {
		files := contentsOf(t, n)
		if len(files) != 1 || files["shared.txt"] != d.kept {
			t.Fatalf("%s holds %v, want only shared.txt with %q", n.name, files, d.kept)
		}
	}
	if rows := conflictsOf(t, d.loser); len(rows) != 0 {
		t.Fatalf("%s still reports %+v, want the conflict closed", d.loser.name, rows)
	}
}

func TestTwoWay_ResolveKeepLocalRestoresTheOwnVersion(t *testing.T) {
	d := divergeOn(t, "shared.txt")
	waitConverged(t, d.a, d.b)

	rows := conflictsOf(t, d.loser)
	if len(rows) != 1 {
		t.Fatalf("%s reports %+v, want one conflict", d.loser.name, rows)
	}
	cliOK(t, d.loser, "conflict", "resolve", rows[0].ID, "--keep", "local", "--yes")
	waitConverged(t, d.a, d.b)

	for _, n := range []*node{d.a, d.b} {
		files := contentsOf(t, n)
		if len(files) != 1 || files["shared.txt"] != d.moved {
			t.Fatalf("%s holds %v, want only shared.txt with %q", n.name, files, d.moved)
		}
	}
}

func TestTwoWay_RenameVersusModifyLosesNothing(t *testing.T) {
	a := startNode(t, "a", folder.ModeSendReceive)
	b := startNode(t, "b", folder.ModeSendReceive)
	writeFile(t, a, "notes.txt", []byte("what both sides had"))
	pair(t, a, b)
	waitTreeEqual(t, a, b)

	pauseFolder(t, b)
	// One side renames the file, the other edits it under the old name.
	// With hashes rather than names, the rename arrives as a deletion and
	// a new file, and the edit must survive both.
	if err := os.Rename(filepath.Join(a.Root, "notes.txt"), filepath.Join(a.Root, "renamed.txt")); err != nil {
		t.Fatalf("rename: %v", err)
	}
	writeFile(t, b, "notes.txt", []byte("edited on b"))
	resumeFolder(t, b)
	waitConverged(t, a, b)

	for _, n := range []*node{a, b} {
		files := contentsOf(t, n)
		if files["notes.txt"] != "edited on b" {
			t.Fatalf("%s: notes.txt = %q, want the edit kept", n.name, files["notes.txt"])
		}
		if files["renamed.txt"] != "what both sides had" {
			t.Fatalf("%s: renamed.txt = %q, want the renamed file", n.name, files["renamed.txt"])
		}
		if len(files) != 2 {
			t.Fatalf("%s holds %v, want both names", n.name, files)
		}
	}

	rows := conflictsOf(t, b)
	if len(rows) != 1 || rows[0].Path != "notes.txt" || rows[0].Reason != "delete-vs-modify" {
		t.Fatalf("%s reports %+v, want one delete-vs-modify conflict", b.name, rows)
	}
	if rows[0].ConflictPath != "" {
		t.Fatalf("conflict = %+v, want no copy: nothing had to be moved aside", rows[0])
	}
}

func TestTwoWay_CaseOnlyRenamePropagates(t *testing.T) {
	a := startNode(t, "a", folder.ModeSendReceive)
	b := startNode(t, "b", folder.ModeSendReceive)
	writeFile(t, a, "Notes.txt", []byte("case matters here"))
	pair(t, a, b)
	waitTreeEqual(t, a, b)

	if err := os.Rename(filepath.Join(a.Root, "Notes.txt"), filepath.Join(a.Root, "notes.txt")); err != nil {
		t.Fatalf("rename: %v", err)
	}
	waitConverged(t, a, b)

	for _, n := range []*node{a, b} {
		files := contentsOf(t, n)
		if len(files) != 1 || files["notes.txt"] != "case matters here" {
			t.Fatalf("%s holds %v, want only the lowercase name", n.name, files)
		}
	}
	for _, n := range []*node{a, b} {
		if rows := conflictsOf(t, n); len(rows) != 0 {
			t.Fatalf("%s reports %+v, want no conflict for a rename", n.name, rows)
		}
	}
}

func TestTwoWay_NoSilentOverwrite(t *testing.T) {
	const files = 20
	a := startNode(t, "a", folder.ModeSendReceive)
	b := startNode(t, "b", folder.ModeSendReceive)
	for i := range files {
		writeFile(t, a, "f"+itoa(i)+".txt", []byte("round 0"))
	}
	pair(t, a, b)
	waitTreeEqual(t, a, b)

	// Every round writes content nothing else can produce, so finding it
	// afterwards proves that this exact version survived. Both sides write
	// while they cannot see each other, which is the only way to lose one.
	random := rand.New(rand.NewSource(20260921))
	for round := 1; round <= zeroLossRounds; round++ {
		pauseFolder(t, b)
		written := make([]string, 0, 2)
		for _, n := range []*node{a, b} {
			rel := "f" + itoa(random.Intn(files)) + ".txt"
			content := "round " + itoa(round) + " on " + n.name
			writeFile(t, n, rel, []byte(content))
			written = append(written, content)
		}
		resumeFolder(t, b)
		waitConverged(t, a, b)

		for _, n := range []*node{a, b} {
			have := contentsOf(t, n)
			for _, content := range written {
				if !holds(have, content) {
					t.Fatalf("round %d: %q is gone from %s, which holds %v",
						round, content, n.name, have)
				}
			}
		}
	}
}

// cliRefused runs a command that must be refused with a given exit code.
func cliRefused(t *testing.T, n *node, want int, args ...string) {
	t.Helper()
	code, stdout := runCLI(t, n.DataDir, args...)
	if code != want {
		t.Fatalf("%v on %s exited %d, want %d:\n%s", args, n.name, code, want, stdout)
	}
}

// staleRow returns the conflict row with the given id, or fails.
func staleRow(t *testing.T, n *node, id string) conflictRow {
	t.Helper()
	for _, row := range conflictsOf(t, n) {
		if row.ID == id {
			return row
		}
	}
	t.Fatalf("%s no longer reports conflict %s", n.name, id)
	return conflictRow{}
}

func TestTwoWay_ResolvingAStaleConflictIsRefused(t *testing.T) {
	d := divergeOn(t, "tw.txt")
	waitConverged(t, d.a, d.b)

	rows := conflictsOf(t, d.loser)
	if len(rows) != 1 {
		t.Fatalf("%s reports %+v, want one conflict", d.loser.name, rows)
	}
	first := rows[0].ID

	// A second round of edits moves the path on while the first conflict
	// is still open, so what is at the path is neither version the first
	// conflict recorded.
	pauseFolder(t, d.b)
	writeFile(t, d.a, "tw.txt", []byte("round two on a"))
	writeFile(t, d.b, "tw.txt", []byte("round two on b"))
	resumeFolder(t, d.b)
	waitConverged(t, d.a, d.b)

	if row := staleRow(t, d.loser, first); !row.Stale {
		t.Fatalf("conflict %s = %+v, want it marked stale", first, row)
	}

	before := map[string]map[string]string{
		d.a.name: contentsOf(t, d.a), d.b.name: contentsOf(t, d.b),
	}
	cliRefused(t, d.loser, cli.ExitIntegrity, "conflict", "resolve", first, "--keep", "local", "--yes")

	// Nothing moved on either side, and the first conflict is still open.
	for _, n := range []*node{d.a, d.b} {
		after := contentsOf(t, n)
		if !maps.Equal(after, before[n.name]) {
			t.Fatalf("%s went from %v to %v, want the refusal to change nothing",
				n.name, before[n.name], after)
		}
	}
	if row := staleRow(t, d.loser, first); row.Resolved {
		t.Fatalf("conflict %s = %+v, want it still open", first, row)
	}

	// Keeping both is what closes it, and it too leaves every file alone.
	cliOK(t, d.loser, "conflict", "resolve", first, "--keep", "both")
	for _, n := range []*node{d.a, d.b} {
		if after := contentsOf(t, n); !maps.Equal(after, before[n.name]) {
			t.Fatalf("%s went from %v to %v, want --keep both to change nothing",
				n.name, before[n.name], after)
		}
	}
}

func TestTwoWay_ResolvingWithAnEditedCopyIsRefused(t *testing.T) {
	d := divergeOn(t, "tw.txt")
	waitConverged(t, d.a, d.b)

	rows := conflictsOf(t, d.loser)
	if len(rows) != 1 || rows[0].ConflictPath == "" {
		t.Fatalf("%s reports %+v, want one conflict with a copy", d.loser.name, rows)
	}

	// The copy is edited by hand, as a merge would leave it. Removing it
	// now would be the only copy of that content gone.
	merged := d.kept + "\n" + d.moved
	writeFile(t, d.loser, rows[0].ConflictPath, []byte(merged))
	waitConverged(t, d.a, d.b)

	cliRefused(t, d.loser, cli.ExitIntegrity,
		"conflict", "resolve", rows[0].ID, "--keep", "remote", "--yes")

	for _, n := range []*node{d.a, d.b} {
		files := contentsOf(t, n)
		if files[rows[0].ConflictPath] != merged {
			t.Fatalf("%s: the copy = %q, want the merge untouched",
				n.name, files[rows[0].ConflictPath])
		}
		if files["tw.txt"] != d.kept {
			t.Fatalf("%s: tw.txt = %q, want %q", n.name, files["tw.txt"], d.kept)
		}
	}
}

// TestTwoWay_StaleRestartDoesNotRollBack is KF-3 with two devices: a node that
// was down while the other wrote twice takes the latest version and neither
// side calls it a conflict, because every version came from one device.
func TestTwoWay_StaleRestartDoesNotRollBack(t *testing.T) {
	a := startNode(t, "a", folder.ModeSendReceive)
	b := startNode(t, "b", folder.ModeSendReceive)
	writeFile(t, a, "notes.txt", []byte("the version both sides had"))
	pair(t, a, b)
	// Both nodes are idle before b goes down, so what b misses is only
	// what a writes afterwards.
	waitConverged(t, a, b)

	b.Stop()
	writeFile(t, a, "notes.txt", []byte("the first edit made while b was down"))
	writeFile(t, a, "notes.txt", []byte("the second edit made while b was down"))
	b.Restart()

	waitConverged(t, a, b)
	for _, n := range []*node{a, b} {
		files := contentsOf(t, n)
		if len(files) != 1 || files["notes.txt"] != "the second edit made while b was down" {
			t.Fatalf("%s holds %v, want only the latest version", n.name, files)
		}
		if rows := conflictsOf(t, n); len(rows) != 0 {
			t.Fatalf("%s reports %+v, want no conflict: one device wrote every version",
				n.name, rows)
		}
	}

	// The stale node does not push its old version back afterwards.
	time.Sleep(settle)
	if diff := treeDiff(t, a.Root, b.Root); len(diff) != 0 {
		t.Fatalf("the trees drifted apart again:\n%s", join(diff))
	}
	if got := contentsOf(t, a)["notes.txt"]; got != "the second edit made while b was down" {
		t.Fatalf("notes.txt on a = %q, want the latest version to stay", got)
	}
}

// TestTwoWay_DeletionSurvivesAStalePeer covers the other half of KF-3: a node
// that still holds a file the other deleted while it was down must take the
// deletion, not re-create the file on the device that deleted it.
func TestTwoWay_DeletionSurvivesAStalePeer(t *testing.T) {
	a := startNode(t, "a", folder.ModeSendReceive)
	b := startNode(t, "b", folder.ModeSendReceive)
	writeFile(t, a, "gone.txt", []byte("what both sides had"))
	writeFile(t, a, "kept.txt", []byte("what stays"))
	pair(t, a, b)
	// Both nodes are idle before b goes down, so the deletion is the only
	// thing b has to catch up with.
	waitConverged(t, a, b)

	b.Stop()
	if err := os.Remove(filepath.Join(a.Root, "gone.txt")); err != nil {
		t.Fatalf("remove: %v", err)
	}
	b.Restart()

	waitConverged(t, a, b)
	for _, n := range []*node{a, b} {
		files := contentsOf(t, n)
		if len(files) != 1 || files["kept.txt"] != "what stays" {
			t.Fatalf("%s holds %v, want the deletion applied on both sides", n.name, files)
		}
		if rows := conflictsOf(t, n); len(rows) != 0 {
			t.Fatalf("%s reports %+v, want no conflict: the file was untouched on b",
				n.name, rows)
		}
	}

	// The deleted file does not come back from the node that was away.
	time.Sleep(settle)
	for _, n := range []*node{a, b} {
		if files := contentsOf(t, n); len(files) != 1 {
			t.Fatalf("%s holds %v again, want the file to stay deleted", n.name, files)
		}
	}
}
