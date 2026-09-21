# Syncthis on-disk state

Everything a device remembers lives in two places: the data directory, which is
private to the device, and one state directory inside every synchronized
folder. Deleting either is safe, and costs a full rescan or a full download.

## Data directory

Resolved from `--data-dir`, else `$SYNCTHIS_DATA_DIR`, else
`$XDG_DATA_HOME/syncthis`, else `~/.local/share/syncthis`.

```
config.json      device name, listen address, chunk size and intervals
identity.key     ed25519 private key, PKCS#8 PEM, mode 0600
identity.crt     self-signed certificate; its public key is the device ID
state.db         SQLite database, WAL mode
control.sock     unix socket, only while the daemon runs
daemon.pid       pid of the running daemon
```

## Folder state directory

```
<folder>/.syncthis/tmp/<transfer id>-<random>.part
```

Partial downloads live inside the folder so the finished file is renamed onto
its target without crossing a filesystem boundary, which is what makes the
install atomic. The scanner always ignores `.syncthis`, whatever the ignore
rules say. `syncthis repair` removes partial files no unfinished transfer
claims.

## Database

`sqlitestore.Open` sets `journal_mode=WAL`, `synchronous=NORMAL`,
`busy_timeout=5000` and `foreign_keys=ON` through the connection string, so the
pragmas hold whichever pooled connection a statement runs on, and transactions
take the write lock up front. The daemon opens the database with one
connection, so writes serialize in Go rather than in SQLite. Every multi-row
write is a transaction of at most 500 rows.

Because the database is in WAL mode with a five second busy timeout, a command
such as `syncthis sync-plan` or `syncthis status` reads it while the daemon
holds it open.

### Tables

| Table | Holds |
|---|---|
| `folders` | one row per configured folder, with its mode and policies |
| `folder_ignore_patterns` | the ignore patterns of a folder, in order |
| `files` | one row per path of a folder, tombstones included |
| `folder_sequence` | the next per-folder version number to hand out |
| `changes` | the scan classification log behind "recent changes" |
| `devices` | known peers and when they were last seen |
| `transfers`, `transfer_chunks` | the journal of inbound downloads |
| `conflicts` | detected divergences (milestone 9) |

### Migrations

The schema is at version 2. `Open` reads `schema_version`, applies every
migration above it in one transaction and records the new version:

| Version | Migration | Adds |
|---|---|---|
| 1 | `001_init.sql` | every table, including ones later milestones fill |
| 2 | `002_synced_mode.sql` | `files.synced_mode` |

A database at an older version is migrated in place, keeping its rows. A
database at a version the binary does not know is refused rather than opened,
so an older binary cannot write a newer schema. Migrations only add; a column
is never renamed or dropped.

### files

A row is the version of a path this device knows about. `hash` is the
BLAKE3-256 of the contents; directories, symlinks and tombstones have none.
`sequence` is a per-folder counter, raised on every write, which is what a peer
compares to decide whether it has seen a version.

`synced_hash` is what makes two-way comparison safe: it is the hash both sides
last agreed on. A local file whose `hash` still equals its `synced_hash` has
not been touched since the last sync, so the peer's newer version may replace
it. A file whose `hash` moved away from `synced_hash` is a local change, and a
peer's different version is a conflict, never an overwrite.

`synced_mode` is the same idea for the permission bits: the mode both sides
last agreed on. A file whose `mode` still equals its `synced_mode` takes the
peer's mode; one that moved away from it was chmodded locally, and the peer
follows instead. A `synced_mode` of 0 means no agreement was ever recorded, in
which case neither side changes anything until the two modes match once.

`modified_by` names the device that produced the version. The puller writes the
peer's id there, together with the size, modification time and inode the file
has after the install, so the next local scan sees an unchanged file and does
not send it back.

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

`transfer_chunks` has one row per chunk, with the BLAKE3-256 of the chunk once
it has been verified. Offsets and lengths are derived from the size and the
chunk size rather than stored.

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

The row in `files` is written after the rename. A crash in between leaves a
file the database does not know about; the next scan records it as a local
creation, and because its content equals what the peer has, the following sync
recognizes it as already in sync and writes the agreement down instead of
sending it back.
