# Syncthis on-disk state

Everything a device remembers lives in two places: the data directory, which is
private to the device, and one state directory inside every synchronized
folder. Deleting either is safe, and costs a full rescan or a full download.

## Data directory

Resolved from `--data-dir`, else `$SYNCTHIS_DATA_DIR`, else
`$XDG_DATA_HOME/syncthis`, else `~/.local/share/syncthis`.

```
config.json      device name, listen address, chunk size, size cap, intervals
                 and bandwidth limits
identity.key     ed25519 private key, PKCS#8 PEM, mode 0600
identity.crt     self-signed certificate; its public key is the device ID
state.db         SQLite database, WAL mode
control.sock     unix socket, mode 0600, only while the daemon runs
daemon.pid       pid of the running daemon
```

The control socket is chmodded to 0600 as soon as it is bound: every command
the daemon answers is one its owner could run themselves, so no other user of
the machine may speak to it.

`max_file_size` in `config.json` is the largest file this device accepts from a
peer, one tebibyte unless it says otherwise. A manifest announcing a bigger one
is refused whole.

`upload_limit` and `download_limit` in `config.json` are the device-wide
transfer rates in bytes per second, `0` for unlimited, which is the default and
what a file written before they existed means. They cover chunk payloads only.
A negative value is refused when the file is read. `config set` writes them and
a running daemon picks them up at once; see
[CLI.md](CLI.md#bandwidth-limits).

`config export` covers what a second machine needs to synchronize the same
folders: the settings of `config.json`, the folder rows with their ignore
patterns and sync windows, and the device rows of the known peers. It never
covers `identity.key`, `identity.crt`, the device ID, the device name, or
anything in `state.db` beyond the folder and device configuration. A bundle is
therefore not a backup of a device: restoring one into a fresh data directory
gives a device with its own identity and an empty database, which then
synchronizes the folders from its peers. See
[CLI.md](CLI.md#syncthis-config-export---output-file).

## Folder state directory

```
<folder>/.syncthis/tmp/<transfer id>-<random>.part
```

Partial downloads live inside the folder so the finished file is renamed onto
its target without crossing a filesystem boundary, which is what makes the
install atomic. The scanner always ignores `.syncthis`, whatever the ignore
rules say. `syncthis repair` removes partial files no unfinished transfer
claims.

`<folder>/.syncthisignore` is read from the folder root and belongs to this
device alone. It is never scanned, never sent to a peer, and never accepted
from one: a peer that names it in its manifest has its manifest refused.

## Database

`sqlitestore.Open` sets `journal_mode=WAL`, `synchronous=NORMAL`,
`busy_timeout=5000` and `foreign_keys=ON` through the connection string, so the
pragmas hold whichever pooled connection a statement runs on, and transactions
take the write lock up front. The daemon opens the database with one
connection, so writes serialize in Go rather than in SQLite. Every multi-row
write is a transaction of at most 500 rows, the chunk rows of a new transfer
included. A save interrupted between two batches is not a loss: the chunk list
is rebuilt from the chunk count the `transfers` row holds, and a row that was
never written is simply an unverified chunk.

The database path is percent-encoded into the connection string, so a data
directory whose name holds a `?` or a `#` still gets its own database with the
pragmas applied.

Because the database is in WAL mode with a five second busy timeout, a command
such as `syncthis sync-plan` or `syncthis status` reads it while the daemon
holds it open.

### Tables

| Table | Holds |
|---|---|
| `folders` | one row per configured folder, with its mode, policies and sync windows |
| `folder_ignore_patterns` | the ignore patterns of a folder, in order |
| `files` | one row per path of a folder, tombstones included |
| `folder_sequence` | the next per-folder version number to hand out |
| `changes` | the scan classification log behind "recent changes" |
| `devices` | known peers and when they were last seen |
| `transfers`, `transfer_chunks` | the journal of inbound downloads |
| `file_chunks` | how every file of this device is cut, and the hash of each piece |
| `conflicts` | detected divergences, one row per path both sides changed |
| `device_counters` | the highest version counter ever seen for a device |

### Migrations

The schema is at version 9. `Open` reads `schema_version`, applies every
migration above it in one transaction and records the new version:

| Version | Migration | Adds |
|---|---|---|
| 1 | `001_init.sql` | every table, including ones later milestones fill |
| 2 | `002_synced_mode.sql` | `files.synced_mode` |
| 3 | `003_conflict_reason.sql` | `conflicts.reason` |
| 4 | `004_sync_windows.sql` | `folders.sync_windows` |
| 5 | `005_version.sql` | `files.version`, the version vector of the entry |
| 6 | `006_sync_error.sql` | `folders.last_sync_error`, `folders.pending_items` |
| 7 | `007_device_counters.sql` | `device_counters`, the per-device counter floor |
| 8 | `008_folder_devices.sql` | `folder_devices`, the devices each folder is served to |
| 9 | `009_chunks.sql` | `file_chunks`, the chunk index; `transfer_chunks.start_offset` and `.byte_length` |

A database at an older version is migrated in place, keeping its rows. A
database at a version the binary does not know is refused rather than opened,
so an older binary cannot write a newer schema. Migrations only add; a column
is never renamed or dropped.

### folders

`sync_windows` holds the folder's sync windows as canonical `HH:MM-HH:MM`
spans in the device's local time, separated by commas, for example
`22:00-06:00,12:00-13:00`. The empty string means the folder is always open,
which is what every row written before the column existed means. Outside its
windows a folder still scans and still serves; only the pull waits. See
[CLI.md](CLI.md#sync-windows).

Two columns say what is wrong with a folder, and they have one owner each.
`last_error` belongs to the scanner: what the last scan could not read, empty
when it read everything. `last_sync_error` and `pending_items` belong to the
puller: what the last sync cycle could not apply locally and how many items
that was, empty and zero when the cycle applied everything. Neither owner
writes or clears the other's column, so a folder whose root cannot be written
reports that even though its scans succeed, and a folder with unreadable
entries keeps saying so while its downloads land. `status` reports the scan
failure first, because a folder that cannot be read is why a sync applies
nothing, and `pending_changes` in the report is `pending_items`.

A path the cycle cannot apply is counted and skipped, never fatal: a file whose
parent is a directory on the peer and a file here is one of those, so one
conflicted parent costs that subtree and nothing else. The rest of the plan is
applied in the same cycle.

A cycle that ended early, because the peer or the store went away, records the
failures it already collected and clears nothing: it did not get far enough to
disprove what was there. When two peers share a folder, their cycles are
serialized by the puller's per-folder gate and the last one to finish is what
the folder reports.

### files

A row is the version of a path this device knows about. `hash` is the
BLAKE3-256 of the contents; directories, symlinks and tombstones have none.
`sequence` is a per-folder counter, raised on every write, which is what a peer
compares to decide whether it has seen a version.

`version` is what makes comparison between any number of devices safe: it is
the version vector of the entry, written as `DEVICEID:N,DEVICEID:N` in device
id order. The scanner raises this device's counter on every content change, an
install adopts the vector the content carries, and the outcome of a conflict
adopts the merge of both. A version that dominates another was made on top of
it and is the newer one; two that neither dominate were written independently
and are a conflict, never an overwrite. No rule reads a clock.

An empty `version` means the row predates vectors. It compares equal to every
other empty vector and below every non-empty one, and nothing is backfilled
from `modified_by` or `sequence`, because sequences are per device and not
comparable across devices. Two upgraded devices holding the same content are
therefore in sync, the first content change after the upgrade gives the path a
vector, and a divergence that already existed at upgrade time is kept as a
conflict rather than silently ordered.

`synced_hash` is retired. The column is still there, because migrations only
add, but nothing reads or writes it any more and it is no longer sent to peers.

`synced_mode` is the same idea for the permission bits: the mode both sides
last agreed on. A file whose `mode` still equals its `synced_mode` takes the
peer's mode; one that moved away from it was chmodded locally, and the peer
follows instead. A `synced_mode` of 0 means no agreement was ever recorded, in
which case neither side changes anything until the two modes match once.

`modified_by` names the device that produced the version. The puller writes the
peer's id there, together with the size, modification time and inode the file
has after the install, so the next local scan sees an unchanged file and does
not send it back.

Every row the puller writes is written conditionally: the row must still carry
the `version` the plan was made against, and one whose version moved since is
left alone and logged as superseded by a local change. The read and the write
share one transaction, so a scan that recorded a local edit between the cycle's
`LoadManifest` and its write keeps what it wrote, and the next cycle plans
against it. A mode change is held to the same rule on disk as well: the file
must still have the size, modification time and content the plan described,
because the row a chmod writes carries the peer's hash. The row a chmod leaves
holds the merge of the two vectors, not the peer's alone: no content moved, so
neither side's history is dropped.

Unlike `synced_mode`, which is local bookkeeping, `version` is sent to peers as
the `version` field of a manifest entry: it is what lets the other side tell a
version made on top of its own from one that diverged from it. See
[PROTOCOL.md](PROTOCOL.md).

### device_counters

One row per device, holding the highest version counter ever seen for it: the
counters this device handed out in its own scans, and the counters peers hold
for it in the manifests they send. Every version the scanner writes raises this
device's counter above its floor, so a counter is never handed out twice.

The floor is what makes a restore from a backup safe. A restored database holds
counters the peers have long passed; without the floor the first edit after the
restore would carry a counter the peers already know, be dominated by their
version of the path and be overwritten. The first manifest received after the
restore lifts the floor above every counter the peer holds, so the next version
this device writes dominates instead. See "Backup and restore" below.

The floor is recorded after the write it describes. A crash in between leaves
it below what the entries carry, which loses nothing: the next version is
raised from the entry as well as from the floor, and the entry is the higher of
the two.

### conflicts

A row is one path two devices changed independently, written by the puller and
read by `syncthis conflict list`. `rel_path` is the path, `conflict_path` the
copy holding the other version, empty when there is none. `local_hash` and
`remote_hash` say which content each side had, and matching them against what
is on disk is how `conflict resolve` finds the version the user asked to keep,
whichever of the two files holds it. `remote_hash` is empty when the peer
deleted the path. `reason` is the sync plan reason: `both-modified`,
`delete-vs-modify` or `replace-vs-modify`.

The id is derived from what diverged, the folder, the path, the two hashes and
the peer, not from when it was noticed, so a divergence the sync plan reports
once per cycle updates one row instead of adding one every time. `resolved` is
set by `conflict resolve` and is the only field that changes afterwards.

A conflict copy is a file of the folder like any other: it is recorded in
`files` with a version vector of its own, so the scan that follows sees nothing
new and the peer picks the copy up as an ordinary file. Its name is

```
<stem>.sync-conflict-<YYYYMMDD>-<HHMMSS>-<author short id><ext>
```

built from the modification time and the author of the version being moved
aside, in UTC. The name is a function of that version alone, so two devices
that move the same version aside write the same name and end up with one copy
rather than one per pair; a copy already holding exactly that content is reused
instead of written again, and only a name taken by different content gets a
counter before the extension. Which of the two versions keeps the path is
decided by the content: the greater hash wins, so both devices reach the same
answer and only one of them writes a copy.

### transfers and transfer_chunks

Only inbound transfers are recorded; the sender keeps no state at all. A
transfer row holds the expected whole-file hash, size, mode, modification time,
chunk size and the path of its partial file. A partial index makes at most one
transfer per `(folder_id, rel_path)` live, where live means a state other than
`done` or `failed`.

States: `pending` → `active` → `verifying` → `done`, with `failed` for a
transfer given up on and `paused` for one a user stopped (milestone 7). A
finished transfer is removed from the journal once its file is installed and
recorded in `files`; the journal only exists to resume.

`failed` means one thing: the peer served bytes that did not hash right. The
row and its partial file are kept so they can be looked at. A transfer the next
plan supersedes is a different case and leaves nothing: the peer moved on to
another version, or the local path changed under the download, so the row and
its partial file are deleted and a `transfer.superseded` event is published.
A `failed` row is deleted when a later cycle starts a new transfer for the same
path, so at most one exists per path, and `repair` deletes the ones whose
partial file is gone or whose path already holds the content the transfer was
after. A converged mesh therefore ends with an empty journal.

Two more rows are removed rather than left to rot, both of them what a process
killed at the wrong moment leaves behind. A row in `done` state outlived the
install it recorded: what arrived is in `files`, so the next `ResumeAll`, which
runs when a folder loop starts, deletes it with its partial file, and `repair`
does the same. A live row whose path the current plan does not pull, from the
peer whose cycle is running, is deleted at the end of that cycle: no plan will
ever ask for it again, and a folder that keeps such a row is reported as
transferring forever. Rows of other peers are untouched, because only the peer
whose plan was just made can say what it no longer wants.

`transfer_chunks` has one row per chunk, with the BLAKE3-256 the sender
announced for it and the byte range the sender cut it at. The range is stored
because chunking is content-defined: it does not follow from the size and the
chunk size any more. A row written before migration 8 has neither, and is read
back the old way, from the transfer's chunk size, so a download in flight
across the upgrade resumes instead of starting over.

### file_chunks

The chunk index: one row per chunk of every file this device holds, with the
folder, the path, the index, the byte range and the BLAKE3-256 of that range.
An index on `hash` alone is what makes deduplication possible: before asking a
peer for a chunk, the puller looks the hash up here and finds any copy of that
content, in any file of any folder.

The rows are written by the scanner, which reads the file anyway to hash it and
cuts it in the same pass, and by the puller when a download is installed, from
the list it verified every chunk against. They go away with the file through
the foreign key, and a tombstone clears them explicitly, because a tombstone
keeps its `files` row and the cascade does not fire: the index must never offer
bytes of a file the folder no longer has.

The index is advisory. It is only as fresh as the last scan, so the puller
re-reads and re-hashes what it points at before using it, and falls back to the
peer when the bytes are not what was announced. A stale row costs one read.

**Cost.** A row is 6 columns, dominated by the 64 character hash and the path:
about 120 bytes plus the path length in a `WITHOUT ROWID` table, plus roughly
70 bytes in the hash index. Call it 200 bytes per chunk. At the default average
chunk size of 1 MiB that is about 200 bytes per mebibyte of content, or 0.02%:
a 1 TiB folder costs roughly 200 MiB of database. A folder of many small files
costs one row per file instead, since a file below the minimum chunk size is a
single chunk: 100,000 small files is about 20 MiB. Lowering `chunk_size` raises
the cost in proportion.

**Upgrading.** A database written before migration 8 starts with an empty
index. Nothing is re-downloaded for it: a transfer simply finds nothing to
reuse, and a peer asking how a file is cut gets an answer read off the disk on
the spot. The first scan after the upgrade fills the index in, because a file
the index does not cover is read again even when its size, mtime and inode say
it has not changed. That scan costs one pass over the folder; after it,
deduplication is in effect.

### Renames and copies

A path a peer announces whose content this device already holds under another
name is not downloaded. The puller copies the local file into a partial file of
`.syncthis/tmp` and installs that, so a renamed or copied file costs zero
payload bytes on the wire whatever its size. This is local behaviour only: the
sender is asked for nothing and the protocol does not change.

Three rules keep it from losing data:

1. The source is chosen from the local manifest, by content hash and size, and
   the copy is hashed while it is read. A source whose bytes do not hash to
   what the peer announced is dropped and the file is downloaded normally, so a
   stale manifest row costs a download and never a wrong file.
2. The copy is made before the rest of the plan is applied, and the deletion of
   the old path is part of that rest. The content is therefore already in the
   partial file when the old name goes, and the source file itself is only ever
   read.
3. The install is the ordinary one: the whole partial file is hashed again, the
   target is checked against the version the plan was made against, and the
   rename onto it is atomic. A target that moved keeps the local change and the
   copy is discarded.

A copy that cannot be installed is not a failure of the file: the puller falls
back to downloading it from the peer in the same cycle. The partial files of
copies nothing installed are removed when the cycle ends, and `repair` removes
the ones a killed process left behind.

### Journal semantics

The journal is advisory and the partial file is the authority:

1. A chunk is verified against the hash the sender announced before it is
   written, so unverified bytes never reach the disk.
2. The chunk is written at its offset, and the journal row is marked verified
   afterwards. The partial file is flushed every 16 chunks and at the end.
3. On resume, every chunk the journal calls verified is read back from the
   partial file and hashed again. A chunk that does not match is unmarked and
   fetched again, so a crash between the write and the journal update cannot
   leave a wrong byte behind.
4. A partial file that is gone resets the whole transfer.
5. Before the file is installed, the whole partial file is hashed and compared
   with the expected hash. A mismatch fails the transfer and keeps the partial
   file for inspection; nothing is installed.

The install sets the mode and the modification time on the partial file,
flushes it, renames it over the target and then flushes the directory that now
holds it. A crash at any point in that sequence therefore leaves either the old
file or the complete new one, never a half-written target and never a name
pointing at content that has not reached the disk.

A target that is no longer the version the plan was made against is left alone:
the download is discarded rather than the local change.

### Scan watermarks

A folder nothing changed is not walked again. The daemon holds one watermark
per running folder loop: the folder sequence the last scan wrote, plus the
claim that every change since then came through the filesystem watcher. While
the claim holds, the periodic tick reads one row, the folder sequence, instead
of the whole tree, and publishes `scan.skipped`. The watcher still reports what
changes, and each burst still gets its subtrees scanned after the debounce, so
the watermark only removes the pass that would have found nothing.

The watermark lives in memory, not in the database. Nothing has to be stored
for it, because every case in which it could be wrong is a case in which it
does not exist: the loop takes a watermark only from a full scan it ran itself,
with the watch up before that scan started and still up after it.

The claim is dropped, and the whole folder walked again, in every one of these
cases:

1. The folder loop starts. That is every daemon start, and every folder whose
   path, symlink policy or hidden-file policy changed. Nothing watched the
   folder while the process was down, and a data directory put back from a
   backup is put back while the daemon is stopped, so a restored database is
   always read against the disk before it is trusted.
2. The watch is not running, stopped on its own, or had to be attached again.
   The time before a watch is attached is time nobody reported.
3. The watcher dropped events and reported an overflow.
4. A scan failed, or was superseded by another writer and wrote nothing. What
   it read was never recorded, so it stands for nothing.
5. The stored folder sequence is below the watermark. A sequence only ever
   rises, so one that fell is a different database than the one the watermark
   was taken from, which is what a restore under a running daemon looks like.
6. The operator asks, with `syncthis rescan`. A queued rescan is always a full
   scan.

A sequence that rose above the watermark is not a fallback: it is this device's
own puller recording what it installed, and the watcher reports those writes
like any other change.

The cost of being wrong is bounded by the interval itself: the watermark only
decides whether the periodic pass runs, never what a scan writes, so the worst
case is a change found at the next full scan rather than at the next tick.

### A crash during a scan

A scan writes its whole result in one transaction against the folder sequence
it read: the entries it found, the changes it recorded and the new sequence go
in together. A folder that moved on in the meantime, which is what the puller
does to a scan while it installs files, makes the write refuse itself and
nothing is stored. The scan reloads the manifest, re-diffs the tree it already
walked and tries again, five times; a scan that still loses is rescheduled by
the folder loop, which keeps its watch and scans the same subtrees again. It is
not a failed scan and is not recorded on the folder. A process
killed at any moment therefore leaves either the manifest that was there before
the scan or the complete new one, never a part of either, and the database
always opens afterwards. Nothing else has to be done to recover: the next scan
walks the tree again and converges, and `repair --dry-run` finds nothing it
cannot fix. A daemon killed this way also leaves its pid file and control
socket behind; the next `daemon run` takes both over, because a pid that no
process holds and a socket nothing listens on are stale by definition.

This is what `TestCrash_ScanKilledMidWayLeavesARecoverableDatabase` and
`TestCrash_DaemonKilledDuringStartupScan` in `internal/e2e` check, by sending
SIGKILL to a real process scanning 20,000 files and then asserting that the
file count is either 0 or 20,000 and that a rescan reaches 20,000 with nothing
left to write.

The row in `files` is written after the rename. A crash in between leaves a
file the database does not know about; the next scan records it as a local
creation, and because its content equals what the peer has, the following sync
recognizes it as already in sync and writes the agreement down instead of
sending it back.

### Backup and restore

The whole state of a device is its data directory: back it up with the daemon
stopped, so the database has no open transaction, and copy the directory
whole rather than the database file alone.

```console
$ syncthis daemon stop
$ cp -a ~/.local/share/syncthis ~/backups/syncthis-2026-09-22
```

To restore, put the directory back with the daemon stopped, start the daemon
and let it reach its peers before editing anything in the synced folders:

```console
$ syncthis daemon stop
$ rm -rf ~/.local/share/syncthis && cp -a ~/backups/syncthis-2026-09-22 ~/.local/share/syncthis
$ syncthis daemon run &
$ syncthis rescan
$ syncthis status          # wait until the peers are connected
```

The rescan re-reads the folders, so a file that changed while the device was
down is recorded as a local change rather than staying invisible. The contact
with the peers raises the counter floor above every counter of this device they
hold, which is what keeps an edit made after the restore from being dominated
by an older version of the same path.

One window is left: an edit made after the restore and before the first contact
with a peer, on a path that peer also changed. That edit carries a counter the
peer already knows and is a conflict rather than a dominant version, so the two
versions are both kept, one under a conflict name. Nothing is lost either way;
the order of the two is what cannot be recovered. A device that never receives
a manifest at all, because every folder it holds is send-only, raises its floor
only from its own scans, so the rescan above is what lifts it after a restore.
