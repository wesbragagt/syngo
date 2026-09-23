# Syncthis plan 3: Causal versions for N-device convergence, and a journal that empties

> **Historical.** This plan is built. It records what was intended at the time
> and is not a description of the code. It plans the bump to protocol version
> 2; two later versions followed, and the protocol is at 4. See
> [PROTOCOL.md](PROTOCOL.md) and [STATE.md](STATE.md).

Fixes KF-1, KF-2 and KF-3 of `docs/PLAN-E2E-DOCKER.md` section 10 at HEAD `e3724a9`.

## 1. Diagnosis in one paragraph each

**KF-3.** `syncplan.remoteFile` orders two versions with one scalar baseline per file, `synced_hash`. A baseline is a fact about a pair ("the version this device and *some* peer last agreed on"), stored as if it were a fact about the file. With three devices the pair the baseline came from and the pair being compared are different, so "local unchanged since agreement" is read as "remote is newer" for a peer that merely never saw the change. The same rule makes a local tombstone always re-pullable ("local tombstone is pulled again"), so a stale peer also undoes deletions and renames made during its absence.

**KF-2.** Pairwise conflict resolution is not compositional. After a loser installs the winner's content it records `synced_hash = hash`, so the third device's still-concurrent version is then ordered against it by the KF-3 rule and one of the three versions is pulled over without a copy. The name of a copy is also derived from *detection time and the peer detected against*, so the same losing content moved aside on two devices lands under two names, which is why any correct fix must also make copy names a function of the content's provenance, or the docker test's "exactly N-1 copies" cannot hold.

**KF-1.** `Puller.failed` writes `state = failed` for every per-file error, including the two that mean "the plan is stale" (`hash_changed`, `not_found`, and `ErrChangedUnderneath`). Nothing reads failed rows except `transfer list` and `status`, nothing deletes them, and `repair` skips them by design.

## 2. Design choice

Evaluated:

- **(b) per-peer baselines**, `synced_hash` keyed by `(folder, path, peer)`. Fixes KF-3 for direct pairs, but a baseline learned from B says nothing about a version relayed through C; with a 3-mesh every version can arrive via any peer, so the rule "remote.Hash == baseline[peer]" is only correct if every device talks to every other device about every version, which partitions break. It also grows a row per (file, peer) and still cannot order tombstones. Not provably correct for N.
- **(a) per-file version vectors** (device → counter), carried in the manifest, stored per entry. Dominance is a partial order that is correct whatever the relay path: a version made on top of another dominates it; two versions neither of which dominates were made concurrently. Every rule the planner needs (pull / nothing / conflict, for live files, tombstones and kind changes) is one `Compare`. Merge on install makes the outcome of a conflict dominate both inputs, so a third device holding one of the inputs takes the outcome without a second conflict, which is what makes KF-2 compositional.
- **(c) hybrid**: keep `synced_hash` rules as a fallback for peers that do not send vectors. Keeps the unsafe path alive and doubles the planner's rule table.

**Recommendation: (a), with modes kept out of the vector.** The permission-bit baseline `synced_mode` stays exactly as it is (local bookkeeping): a mode-only change must not create a content version, or `TestModeChange_IsNotAppliedToALocallyModifiedFile` (chmod on one side, edit on the other, edit wins untouched) would become a both-modified conflict. The residual is that a stale rejoin can roll a *mode* back the way KF-3 rolled content back; that is not data loss and is recorded in section 12.

`synced_hash` is retired: removed from `manifest.Entry`, the wire and the planner; the column stays (migrations only add) and is no longer read.

Protocol: **bump to `protocol_version: 2`**, additive `version` field on manifest entries, `synced_hash` removed from the entry. A version-1 peer is answered `Rejected{protocol_version}` as today. A receiver cannot order a version-1 entry safely, so speaking to one would be pretending; all devices of a mesh upgrade together.

## 3. Domain

### 3.1 `internal/domain/manifest/version.go` (new)

```go
// Version is a version vector: for each device, how many content changes it
// has made to a path. Two versions are ordered by dominance; two that neither
// dominate were made concurrently.
type Version struct{ counters []Counter } // sorted by ID, no zero counters

// Counter is one component of a Version.
type Counter struct {
	ID device.ID
	N  uint64
}

// Ordering is the result of comparing two versions.
type Ordering uint8

const (
	Equal      Ordering = iota // same counters
	Before                     // the receiver is dominated: the argument is newer
	After                      // the receiver dominates: the receiver is newer
	Concurrent                 // neither dominates
)

func (o Ordering) String() string

// NewVersion builds a normalized version from counters. It fails on an
// invalid device id, a zero counter or a repeated id.
func NewVersion(counters []Counter) (Version, error)

// Counters returns the components in device-id order. Never nil-aliased.
func (v Version) Counters() []Counter
func (v Version) IsZero() bool
func (v Version) Counter(id device.ID) uint64

// Bump returns v with the counter of id raised by one. An empty id returns v.
func (v Version) Bump(id device.ID) Version

// Merge returns the component-wise maximum of v and o. It dominates both.
func (v Version) Merge(o Version) Version

// Compare orders v against o. Compare(a, b) == Before iff Compare(b, a) == After.
func (v Version) Compare(o Version) Ordering

// Equals reports Compare(o) == Equal.
func (v Version) Equals(o Version) bool

// String writes "ID:N,ID:N" in id order, "" for the zero version. Parse is
// its inverse. Both are pure formatting; the SQLite adapter stores this form.
func (v Version) String() string
func ParseVersion(s string) (Version, error)
```

`Compare` is a merge-walk over the two sorted slices keeping two flags (`less`, `greater`); missing component reads as 0. `Bump` and `Merge` return new slices (value semantics; a `Version` inside an `Entry` copied by value must not share mutation). `Entry` becomes non-comparable (slice field); verify with `go vet` that nothing compares entries with `==`. Current non-test code does not.

### 3.2 `manifest.Entry`

```go
type Entry struct {
	Path       string
	Kind       Kind
	Size       int64
	ModTime    time.Time
	Mode       uint32
	Hash       Hash
	LinkTarget string
	Inode      uint64
	Deleted    bool
	// ModifiedBy is the device that produced this content version. A pulled
	// entry keeps the author the peer announced, not the peer it came from.
	ModifiedBy device.ID
	Sequence   uint64
	// Version orders this entry against the same path on a peer. The scanner
	// raises this device's counter on every content change; an install adopts
	// the peer's vector; a conflict outcome adopts the merge of both.
	Version    Version
	SyncedMode uint32 // unchanged
}
```

`SyncedHash` removed (step 8). Add to `diff.go`:

```go
// ContentDiffers reports whether two live entries differ in what a version
// vector counts: kind, content hash, link target. Mode is not content.
func ContentDiffers(a, b Entry) bool
```

`differs` (Diff) is unchanged: a mode-only change is still a `Modify`.

### 3.3 Planner rules, `internal/domain/syncplan/plan.go`

`remoteFile`:

```
local nil                                  → Pull, missing-locally
local tombstone:
    local.Version.Compare(remote.Version)
      After                                → None, local-newer      (deleted here after the peer's version)
      Before | Equal | Concurrent          → Pull, missing-locally  (peer's content survives; Equal only from backfill)
same hash                                  → sameContent (unchanged mode logic; caller merges vectors)
Compare(local.Version, remote.Version):
      Before                               → Pull, remote-newer
      After                                → None, local-newer
      Concurrent                           → Conflict, both-modified
      Equal                                → Conflict, both-modified   (different content under one vector: only backfilled rows; keep both)
```

The three `SyncedHash` cases and the `ModifiedBy` equality case are deleted.

`sameContent`: drop the `local.Hash != local.SyncedHash` arm ("wait for the content agreement"); the rest is unchanged. `in-sync` is returned even when vectors differ; the puller merges (section 5).

`kindChanged(item, local, remote)`:

```
local is a file:
    Compare(local.Version, remote.Version)
      Before | Equal                       → Replace, kind-changed
      After                                → None, local-newer
      Concurrent                           → Conflict, replace-vs-modify
local is dir/symlink                       → Replace, kind-changed   (as today)
```

`remoteDeleted`:

```
local nil or tombstone                     → nothing (as today)
delete policy never                        → None, delete-policy-never
local dir/symlink                          → Delete, remote-deleted  (as today)
local file:
      Before                               → Delete, remote-deleted
      After                                → None, local-newer
      Concurrent | Equal                   → Conflict, delete-vs-modify
```

No new reason strings; `docs/CLI.md` sync-plan reason list stays valid. Directories and symlinks keep their current rules; they still carry vectors so a file-to-directory change is ordered.

## 4. Scanner, `internal/app/scan`

`writeOf(result, old manifest.Manifest, base, now)` gains the old manifest (also the reloaded one on the `ErrStaleManifest` retry in `persist`):

- `Create`: `e.Version = old.Entries[path].Version.Bump(self)` (a tombstone's vector is carried and bumped, so a recreate dominates the deletion; absent → `{self:1}`).
- `Modify`: `if manifest.ContentDiffers(*c.Old, *c.New) { bump } else { e.Version = c.Old.Version }` (mode-only: no bump).
- `Rename`: new path as `Create`; old path goes to `Deleted` and is bumped by the store.
- `Delete`: `Deleted`; the store bumps.

`walk.go` `addFile`/`addSymlink`/dir entries carry `prev.Version` (and `prev.ModifiedBy`) in both the unchanged and the rehashed branch, like `SyncedMode` today. `carryOver` copies whole entries already.

`ports.FileStore.MarkDeleted(ctx, id, paths, by, now)` keeps its signature and gains the rule: each tombstone's `Version` becomes `Version.Bump(by)`. Callers: scanner (self), repair (self). The puller no longer calls it (section 5).

Repair: `rehashed[i].Version = stored.Version.Bump(by)` beside the existing `ModifiedBy = by`.

## 5. Puller, installer, conflicts, `internal/app/syncer`

`NewPuller(store, fsys, hasher, clock, identity ports.Identity, events, cfg, log)` gains the identity (compose passes the one the scanner has; tests pass `fakes`).

`record`:

```go
// record stores what now sits on disk: the author the peer announced, the
// vector the content carries, and the mode as the new mode baseline.
func (p *Puller) record(ctx context.Context, id folder.ID, e manifest.Entry, remote manifest.Entry, v manifest.Version) error {
	e.ModifiedBy = remote.ModifiedBy   // fall back to the peer id when empty
	e.Version = v
	e.SyncedMode = e.Mode
	return p.store.UpsertEntries(ctx, id, []manifest.Entry{e})
}
```

- Plain pull, replace, mkdir, symlink, chmod: `v = item.Remote.Version`.
- Delete: replace `MarkDeleted` with an upsert of the tombstone `{Path, Kind: local.Kind, Deleted: true, ModifiedBy: remote author, Version: remote.Version, ModTime: now}` so the deletion adopts the peer's vector and a stale peer's live entry is then dominated.
- `recordAgreement` (in-sync): `merged := local.Version.Merge(remote.Version)`; if `!merged.Equals(local.Version)` → `store.SetVersion(ctx, id, path, merged)`. Then the `SyncedMode` write as today. `SetVersion` replaces `SetSyncedHash` in `ports.FileStore` (no sequence bump, no manifest notification: content is already equal, vectors catch up on the next cycle).
- `keepBoth`:
  - copy name: `conflict.Name(item.Path, item.Local.ModTime, item.Local.ModifiedBy)` — the losing version's own modification time and author, not the clock and the peer. Two devices moving the same content aside produce the same name, so the copies are one path and converge instead of duplicating (this is what makes KF-2's "exactly N-1 copies" hold in every reconciliation order, see 9.2 order 2).
  - copy version: `prev.Version.Bump(self)` where `prev = store.GetFile(folder, copyPath)` (tombstone-aware; `ErrNotFound` → zero). Copy entry `ModifiedBy = item.Local.ModifiedBy`.
  - path version after install: `item.Local.Version.Merge(item.Remote.Version)`; author `item.Remote.ModifiedBy`.
  - `Installer.KeepLocalAs` gains: when the candidate name exists and hashes to `local.Hash` (hash the file, do not trust the manifest), the local file is removed rather than renamed and the existing copy's entry is returned; otherwise `WithCounter` as today.
- `conflictKept` unchanged: the keeper records nothing for `both-modified`, exactly the two-device contract `TestTwoWay_ConcurrentEditKeepsBoth` asserts.

Installer: `Install`, `Chmod`, `KeepLocalAs` stop setting `SyncedHash`; `described` unchanged.

`Server.Chunk` unchanged.

## 6. Storage, `internal/adapters/sqlitestore`

`migrations/005_version.sql`:

```sql
-- The version vector of the entry, "DEVICEID:N,DEVICEID:N" in device-id
-- order. Empty means the row predates vectors: it compares equal to every
-- other empty vector and below every non-empty one. Rows are not backfilled
-- from modified_by or sequence: sequences are per device and not comparable
-- across devices, so any guess could order a stale version above a newer one.
ALTER TABLE files ADD COLUMN version TEXT NOT NULL DEFAULT '';
```

`schemaVersion = 5`, file appended to `migrationFiles`. `files.go`: read/write `version` in `LoadManifest`, `GetFile`, `upsertEntries`, `scanEntry`; stop selecting/writing `synced_hash` (step 8; inserts omit it, the default `''` applies). `MarkDeleted` becomes read-bump-write per row inside the batch transaction. `SetVersion(ctx, id, path, v)` is an `UPDATE files SET version = ?`. `SyncFolderSequence` unchanged.

Backfill consequences, stated in `docs/STATE.md`: two upgraded devices that hold the same content are in sync (`Equal` + same hash); the first content change after the upgrade gives the path a vector; a divergence that already existed at upgrade time is kept as a conflict rather than ordered.

`ports.TransferFilter` gains `Path string` (KF-1, section 7); `ListTransfers` adds `AND rel_path = ?`.

## 7. Wire, `internal/adapters/tcpwire`

`ProtocolVersion = 2`. ALPN string unchanged. `manifestEntry`:

```go
type manifestEntry struct {
	Path, ... as today ...
	ModifiedBy string            `json:"modified_by"`
	Sequence   uint64            `json:"sequence"`
	Version    map[string]uint64 `json:"version,omitempty"` // device id → counter
}
```

`synced_hash` removed. `fromWireManifest` builds `manifest.NewVersion` from the map, and one entry with an unparsable device id, a zero counter or a duplicate refuses the whole manifest, like the other entry checks. An absent `version` is the zero vector (a version-2 peer with a backfilled row). `toWireManifest` writes `Counters()`.

Compatibility rule (in `docs/PROTOCOL.md`): a Hello with `protocol_version` other than 2 is answered `Rejected{protocol_version}`; `peer test` and `peer add` report it as today. There is no mixed-version operation.

## 8. KF-1: the journal policy

Per-file failures are split by what the row is worth afterwards:

| Cause | Meaning | Row afterwards |
|---|---|---|
| `ChunkHashChanged`, `ChunkNotFound` | the peer's version moved on; the next plan starts another transfer | **deleted**, temp removed, event `transfer.superseded` |
| `ErrChangedUnderneath`, `ErrDirNotEmpty` (from `install`) | the local side moved; the next plan re-decides | **deleted**, temp removed, `transfer.superseded` |
| "the peer now offers another version" in `transferFor` | same as the first row | **deleted** (today: `failed` + temp removed) |
| `ErrIntegrity` (chunk or whole-file hash mismatch) | the peer served bytes that do not hash right; worth looking at | `failed`, temp kept, `transfer.failed` (as today) |

`Puller.failed` becomes `Puller.giveUp(ctx, t, cause)` with the table above. `stopped` unchanged otherwise. `summary.Failed` still counts the file for this cycle (the log line becomes Info "transfer superseded").

Retry and cleanup of the rows that remain (`failed` = integrity only):

- **When retried:** never by `ResumeAll` (unchanged: the plan decides). When a later cycle still wants that path, `transferFor` finds no live row and starts a new transfer from scratch; **before saving it**, it deletes every `failed` row for `(folder, path)` (`ListTransfers{FolderID, Path, State: failed}` then `DeleteTransfer`) and their temp files. So at most one failed row per path exists, and only while the path is still not converged.
- **`repair`:** new pass over `failed` rows: delete a row whose temp file is gone (nothing left to inspect), or whose path's live `files` entry hashes to the row's expected hash (installed by a later transfer). Report gains `TransfersPruned []string`; `--dry-run` lists them; the CLI line adds "N failed transfers pruned". Rows that still have a partial file and an unconverged path are kept and listed.
- **`transfer list`:** unchanged output; its `Long` text and `docs/CLI.md` say a failed row is now only an integrity failure and how it is removed.
- `status.transfers.failed` therefore counts integrity failures only, and KF-1's mesh converges with an empty journal.

## 9. Worked traces

Vectors written `{A:n,B:m}`. `h(x)` content hash; KF-2 assumes `h(vA) > h(vB) > h(vC)`.

### 9.1 KF-3

1. A writes v0 → `{A:1}`. B, C pull, adopt `{A:1}`.
2. C isolated. A writes v1 → `{A:2}`. B: local `{A:1}` Before `{A:2}` → pull, adopt `{A:2}`.
3. C rejoins. C vs A: `{A:1}` Before `{A:2}` → pull v1. A vs C: `{A:2}` After `{A:1}` → none, local-newer. B vs C: same. No conflict rows anywhere. `TestOffline_StaleRejoinKeepsTheNewerVersion` holds.
4. Long absence: A deletes `a/r00` → tombstone `{A:k+1}`; C live `{A:k}` → C deletes; A's tombstone After C's live → none (today: re-pulled). Rename: old path as delete, new path `{A:1}` missing on C → pull. `TestOffline_LongAbsenceCatchesUp` holds.

### 9.2 KF-2

Start `{A:1}` everywhere. Partition. A: vA `{A:2}`; B: vB `{A:1,B:1}`; C: vC `{A:1,C:1}`. Copy names: `N_B = Name(report.md, mtime(vB), B)`, `N_C` likewise.

Order 1 (everyone meets A first):
- B vs A: Concurrent, B loses → `N_B` (`{B:1}`, vB), path = vA `{A:2,B:1}`, row on B.
- C vs A: Concurrent, C loses → `N_C` (`{C:1}`), path = vA `{A:2,C:1}`, row on C.
- A vs B: same hash, `{A:2}` Before → in-sync, merge to `{A:2,B:1}`; `N_B` missing → pull. A vs C: same hash, Concurrent → merge `{A:2,B:1,C:1}`; `N_C` → pull.
- B vs C, C vs B: path same hash → merge; pull the other copy.
Result: `report.md = vA`, copies `N_B`, `N_C` on all three, rows on B and C.

Order 2 (B and C meet before A):
- C vs B: Concurrent, C loses → `N_C`, path = vB `{A:1,B:1,C:1}`, row on C. B vs C: B keeps, nothing.
- C vs A: local vB `{A:1,B:1,C:1}` vs vA `{A:2}`: Concurrent, C loses again → moves vB aside as `N_B` (author B, B's mtime, `{C:1}`), path = vA `{A:2,B:1,C:1}`, second row on C.
- B vs A: Concurrent, B loses → `N_B` (`{B:1}`, same content) , path = vA `{A:2,B:1}`, row on B.
- B vs C: `N_B` `{B:1}` vs `{C:1}`, same hash → in-sync, merge; `N_C` → pull. C, A converge likewise.
Result: identical to order 1; `N_B` exists once. Without the deterministic name this order yields three copies, two of them vB. The e2edocker test's per-device `Conflicts() != 0` assertion is amended to the two losers (section 11).

### 9.3 Two-device regressions

- **Edit while paused** (`divergeOn`): `{A:1}` both; A → `{A:2}`, B → `{A:1,B:1}`; Concurrent → both-modified. Keeper does nothing; loser moves aside (`Name(path, mtime, loser)`), installs with merge `{A:2,B:1}`; keeper adopts on the next in-sync. Copy pulled. No further movement: all vectors Equal.
- **Delete vs modify / rename vs modify**: A tombstone `{A:2}`, B edit `{A:1,B:1}`: B sees remote tombstone Concurrent → conflict delete-vs-modify, no copy (as asserted); A sees local tombstone Concurrent vs live → pull the edit. `renamed.txt` `{A:1}` missing on B → pull.
- **Kind change**: A replaces file `{A:1}` by a dir → `{A:2}`; B `{A:1}` Before → replace. B had edited (`{A:1,B:1}`) → Concurrent → replace-vs-modify. Dir → file symmetrical.
- **Mode-only**: no bump. Same hash, Equal vectors → `sameContent` with `SyncedMode`, identical to today; a mode change against a local edit: local `{A:1,B:1}` After `{A:1}` → local-newer, the edit stays at 0644.
- **Sender restart**: B installs A's `{A:1}` and adopts it; A's startup scan sees an unchanged file → same vector → Equal. Crash between rename and `files` write: next scan records `{B:1}`; vs A's `{A:1}` same hash → in-sync, merge `{A:1,B:1}`; A adopts. No conflict, no transfer.
- **Stale-conflict refusals** (`conflict resolve`): rows and hashes unchanged; copy names still carry `Marker`.

## 10. Implementation steps (each committable and green)

1. **Domain `Version`**: `version.go`, `ContentDiffers`, `Entry.Version` field (unused). Table tests (section 11.1).
2. **Store**: `005_version.sql`, `schemaVersion = 5`, read/write `version`, `SetVersion` on `ports.FileStore` (alongside `SetSyncedHash` for now), `MarkDeleted` bumps, `TransferFilter.Path`; fakes mirror. Migration test 4→5 keeps rows with empty vectors.
3. **Scanner and repair** write vectors (section 4). Planner still on old rules; vectors are recorded only.
4. **Wire**: protocol 2, `version` field with validation; handshake rejects 1. Codec round-trip tests.
5. **Puller/installer**: `NewPuller` takes identity; `record(e, remote, v)`; delete upserts tombstone; `recordAgreement` merges; `keepBoth` merge and copy vector; author preserved. Old planner rules still active, so existing syncer tests pass with both fields maintained.
6. **Planner on vectors** (the KF-2/KF-3 fix): rewrite `remoteFile`, `kindChanged`, `remoteDeleted`, `sameContent`; rewrite `plan_test.go` table; adjust syncer test helpers (`dest.record` takes a `Version`).
7. **Conflict copy naming** by author and mtime; `KeepLocalAs` same-content reuse; conflict tests; GLOSSARY/STATE/CLI text.
8. **Retire `SyncedHash`**: remove from `Entry`, wire DTO, installer, walker, store reads, fakes; remove `SetSyncedHash` from the port. Column left in place, documented as unread.
9. **KF-1 policy**: `giveUp`, `transferFor` prune, repair prune, `transfer.superseded` event, CLI/doc text, tests.
10. **Un-skip** the four `internal/e2edocker` tests; amend the KF-2 row assertion; delete section 10 of `docs/PLAN-E2E-DOCKER.md` (or replace with a pointer to this plan); tick "Implement causal state with version vectors" in `.specs/tasks.md`; STATE.md schema table.

Steps 1–5 change no observable two-device behaviour; step 6 is the behavioural switch and should be the commit the e2e suite is run against most carefully.

## 11. Tests

### 11.1 Domain, `internal/domain/manifest/version_test.go`

Table over `Compare`: `{}`/`{}` Equal; `{}`/`{A:1}` Before; `{A:1}`/`{A:2}` Before and the mirror After; `{A:1}`/`{B:1}` Concurrent; `{A:2,B:1}`/`{A:1,B:2}` Concurrent; `{A:2,B:1}` vs `{A:2}` After; antisymmetry property over a small generated set. `Merge`: dominates both inputs, idempotent, commutative. `Bump`: raises one counter, empty id is a no-op, does not alias the receiver. `String`/`ParseVersion` round trip, id order independent of input order, invalid id / zero counter / duplicate refused. `ContentDiffers`: mode-only false, hash true, kind true, link target true.

### 11.2 Planner, `internal/domain/syncplan/plan_test.go`

Helper `file(path, content string, v manifest.Version)` replaces the `synced` string. Cases:
- kept, re-expressed: missing pulled; tombstone with dominated vector pulled; same hash in sync; remote dominates → pull; local dominates → none; concurrent → conflict; remote deletion of a dominated file → delete; remote deletion vs concurrent edit → delete-vs-modify; kind change dominated → replace; kind change concurrent → replace-vs-modify; all mode cases unchanged.
- new, KF-3: local `{A:2}` vs stale remote `{A:1}` with different hash → none/local-newer; local tombstone `{A:2}` vs stale live `{A:1}` → none/local-newer; local live `{A:1}` vs remote tombstone `{A:2}` → delete.
- new, KF-2 compositionality: local = installed outcome `{A:2,B:1}` (hash vA) vs third device's `{A:1,C:1}` (hash vC) → conflict; same hash with concurrent vectors → in-sync.
- backfill: `{}` vs `{}` different hash → conflict; same hash → in-sync.
- removed: "a version the peer made on top of ours is pulled" and "an earlier version of our own change is not a conflict" in their `SyncedHash`/`ModifiedBy` form; both re-expressed with vectors (`{A:1}` vs `{A:2}` pull; `{A:3}` vs `{A:2}` none).

### 11.3 Scanner, `internal/app/scan/scanner_test.go`

Create gives `{self:1}`; modify bumps; mode-only chmod does not bump; delete bumps the tombstone; recreate over a tombstone `{self:2}` → `{self:3}`; rename: new path bumped from its own history, old path tombstone bumped; partial scan carries vectors of untouched subtrees; the `ErrStaleManifest` retry bumps from the reloaded manifest.

### 11.4 Store, `internal/adapters/sqlitestore`

`files_test`: version round trip, `MarkDeleted` bumps `by`, `SetVersion`. `migrate_test`: a v4 database with rows migrates to v5 with empty vectors and unchanged hashes. `transfers_test`: `Path` filter.

### 11.5 Wire, `internal/adapters/tcpwire`

Codec round trip with vectors; entry with bad device id refused; `version` absent → zero; Hello 1 → `Rejected{protocol_version}` from both sides.

### 11.6 Syncer, `internal/app/syncer`

- change: `TestSync_SetsSyncedHashWhenInSync` → `TestSync_InSyncAdoptsTheMergedVersion` (concurrent same-hash → stored vector is the merge).
- change: `TestSync_HashChangedOnSenderFailsCleanly` → asserts no journal row and no temp file remain and a `transfer.superseded` event; `dest.failedTransfer` helper is used only by the integrity tests.
- change: `TestSync_LocalChangeIsNeverOverwritten`, `TestSync_DeleteIsPropagatedOnlyWhenTheLocalCopyIsUnchanged`, `TestSync_LocallyModifiedFileIsNotReplacedByADirectory`, `TestSync_ModeChangeIsNotAppliedToALocallyModifiedFile`: `d.record` seeds a concurrent vector instead of a baseline hash.
- new: pull adopts the remote vector; delete adopts the tombstone vector; stale peer manifest (`{A:1}` vs local `{A:2}`) changes nothing; `keepBoth` installs with the merged vector and gives the copy `{self:1}` (or `{self:n+1}` over a tombstone); copy name uses the losing version's mtime and author; a same-content copy already present is reused and the local file removed; `ResumeAll` still ignores failed rows; `transferFor` deletes an older failed row for the path; `ErrChangedUnderneath` deletes the row.
- conflict_test: `TestConflict_CopyNameThatIsTakenGetsACounter` split into "different content gets a counter" and "same content is reused".

### 11.7 Repair, `internal/app/repair/service_test.go`

Failed row with missing temp pruned; failed row whose path now holds the expected hash pruned; failed row with temp and unconverged path kept; dry-run lists without deleting.

### 11.8 e2e two-device, `internal/e2e`

No assertion changes expected in `oneway_test`, `twoway_test`, `resume_test`, `kindchange_test`, `modes_test`, `pause_test`; they run unchanged as the regression gate after step 6. New: `TestTwoWay_StaleRestartDoesNotRollBack` (B stopped, A edits twice, B restarted: B takes the latest, no conflict) and `TestTwoWay_DeletionSurvivesAStalePeer` (B stopped, A deletes, B restarted: file gone on both). `TestResume_SenderRestartFinishesTheTransfer` already asserts `Transfers.Failed == 0` and gains a check that `transfer list` is empty.

### 11.9 e2edocker skips to remove

`TestMesh_ConcurrentDisjointEditsConverge`, `TestMesh_ThreeWritersOneConflict`, `TestOffline_LongAbsenceCatchesUp`, `TestOffline_StaleRejoinKeepsTheNewerVersion`. In `TestMesh_ThreeWritersOneConflict` the loop's `if rows := dev.Conflicts(); len(rows) == 0` moves to the two non-winner devices, matching the two-device contract that the keeper has nothing to decide.

## 12. Documentation

- `docs/PROTOCOL.md`: title "version 2"; entry JSON with `version`; the `synced_hash` paragraph replaced by the vector rule (dominance → pull/none, concurrent → conflict, merged on install); handshake step 2 "other than 2".
- `docs/STATE.md`: `files` section (vector column, backfill rule, `synced_hash` retired and unread, `modified_by` is the author); migrations table row 5; conflicts naming paragraph (author and modification time; same content → same name everywhere); transfers section (`failed` means integrity only; superseded rows are removed; `repair` prunes); the "row written after the rename" paragraph (converges by vector merge).
- `docs/GLOSSARY.md`: **Version vector**, **Dominates**, **Concurrent**; **Synced hash** entry replaced; **Conflict copy** name.
- `docs/CLI.md`: `transfer list`, `repair` output, conflict name example, `sync-plan` reason table wording for `remote-newer`/`local-newer`.
- `docs/PLAN-E2E-DOCKER.md` section 10 removed; `.specs/tasks.md` Phase 4 checkbox; `docs/ARCHITECTURE.md` if it describes the baseline.

## 13. Top risks

1. **Counter drift after restoring `state.db` from a backup.** The restored device's counters fall behind what peers hold; an edit made before the first sync of that path (`{A:6}` against peers' `{A:7}`) is dominated and pulled over. After one cycle every shared path has adopted or merged the peers' vectors, so the window is edits between restore and first contact. Mitigation to document: run `syncthis rescan` after a restore *and* accept that edits made offline before reconnecting may be turned into conflicts rather than kept; a future hardening is a persisted per-device counter floor raised from every remote vector seen. Backfilled empty vectors after the *migration* are not this case: `{}` is dominated by everything, and different content under `Equal` vectors is a conflict, never an overwrite.
2. **Clock-free ordering.** No rule reads a timestamp; the only time in the design is the modification time inside a copy's name, used for naming and never for ordering. Two different contents by the same author on the same path with the same second-resolution mtime get a counter suffix and remain distinct.
3. **Vector growth.** One counter per device that ever changed the path; a removed device's counter stays forever (pruning would break dominance). Wire cost is roughly 60 bytes per counter per entry; 20k files × 3 devices adds a few MiB to a manifest inside the 64 MiB frame. Beyond ~10 devices, introduce a per-manifest device table with small indexes; not needed now.
4. **Migration of existing installs.** Empty vectors mean any divergence pending at upgrade becomes a kept conflict instead of being ordered, and mixed protocol versions do not connect until all devices are upgraded. State both in STATE.md and the release note.
5. **`Entry` becomes non-comparable** and the puller constructor changes; both are compile-time and caught in steps 1 and 5.
6. **Mode rollback by a stale peer** remains (section 2). No content is involved; recorded as a known limitation rather than fixed here.

### Critical Files for Implementation

- `/home/wesbragagt/dev/syncthing/internal/domain/syncplan/plan.go`
- `/home/wesbragagt/dev/syncthing/internal/domain/manifest/entry.go` (plus new `version.go`)
- `/home/wesbragagt/dev/syncthing/internal/app/syncer/puller.go` (with `conflicts.go`, `installer.go`)
- `/home/wesbragagt/dev/syncthing/internal/app/scan/scanner.go` (with `walk.go`)
- `/home/wesbragagt/dev/syncthing/internal/adapters/sqlitestore/files.go` (plus `migrations/005_version.sql`) and `/home/wesbragagt/dev/syncthing/internal/adapters/tcpwire/codec.go`