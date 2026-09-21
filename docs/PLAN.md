# Syncthis implementation plan

Module: `github.com/wesbragagt/syncthis` (Go 1.26). Target: pure-Go CLI + daemon, fully testable on one Linux machine over loopback.

## 0. Package layout and dependency rules

```
cmd/syncthis/main.go                  entry point: calls internal/cli.Execute, maps ExitError -> os.Exit

internal/domain/                      pure domain, stdlib only (no os, net, database, third-party)
  device/     device.go                ID, ParseID, IDFromPublicKey, Device
  folder/     folder.go                ID, Folder, Mode, DeletePolicy, SymlinkPolicy, Validate
  manifest/   hash.go entry.go manifest.go diff.go   Hash, Entry, Kind, Manifest, Change, Diff (rename detection)
  ignore/     matcher.go               Matcher (gitignore-subset patterns)
  transfer/   transfer.go chunk.go     Transfer, Chunk, State, ChunkCount, MissingChunks
  syncplan/   plan.go                  Action, Plan(local, remote, folder) — compare-by-hash rules
  conflict/   conflict.go              (M9) Record, ConflictName

internal/app/                         use cases; imports domain + app/ports + stdlib (context, io, log/slog, time)
  ports/      clock.go hasher.go filesystem.go store.go watcher.go transport.go identity.go events.go
  fakes/      one in-memory fake per port, exported for tests in any package
  folders/    service.go               add/list/remove/pause/resume
  scan/       scanner.go               walk + hash + Diff + persist; dry-run; partial scan
  status/     service.go report.go     Report struct (the status --json shape)
  peers/      service.go               add/list/remove/test
  syncer/     puller.go server.go installer.go   manifest exchange, chunk pull, journal, atomic install
  repair/     service.go
  daemon/     daemon.go api.go         orchestrates watcher, reconcile ticker, listener, pullers, control API

internal/adapters/                    one package per technology; imports domain + ports + third-party
  localfs/        FileSystem over os
  blake3hash/     Hasher over lukechampine.com/blake3
  sqlitestore/    MetadataStore over modernc.org/sqlite (+ migrations/*.sql embedded)
  fsnotifywatch/  Watcher over fsnotify
  identityfile/   Identity: ed25519 key + self-signed cert on disk
  tcpwire/        Dialer/Listener over TLS-on-TCP; frame.go codec.go handshake.go client.go server.go
  unixctl/        control socket server (daemon side) and client (CLI side), JSON lines
  sysclock/       Clock
  jsonconfig/     config.json load/save

internal/compose/   compose.go        composition root: Build(Options) (*App, error)
internal/cli/       root.go flags.go exitcodes.go output.go <one file per command>
internal/arch/      deps_test.go      import-boundary test (go/parser based)
internal/e2e/       *_test.go         two daemons on loopback in one process
docs/               PLAN.md PROTOCOL.md STATE.md CLI.md
```

Dependency direction (enforced by `internal/arch/deps_test.go` from M1 onward):

| Package prefix | May import |
|---|---|
| `internal/domain/...` | stdlib only, excluding `os`, `net`, `database/sql`, `syscall`, `os/exec` |
| `internal/app/...` | `internal/domain/...`, `internal/app/...`, stdlib excluding `os`, `net`, `database/sql`, `syscall` |
| `internal/adapters/X` | `internal/domain/...`, `internal/app/ports`, `internal/app/fakes` (tests only), third-party |
| `internal/cli` | `internal/app/...`, `internal/compose`, `internal/domain/...`, cobra |
| `internal/compose` | everything |
| `cmd/syncthis` | `internal/cli` only |

Adapters never import each other except `tcpwire` importing `identityfile` is forbidden too: compose passes the `*tls.Certificate` into `tcpwire.New`.

Third-party modules to add (exact import paths):

- `github.com/spf13/cobra` (v1.10.2) — CLI, completion
- `modernc.org/sqlite` (v1.59.0) — driver name `"sqlite"`, pure Go
- `lukechampine.com/blake3` (v1.4.1) — `blake3.New(32, nil)`, pure Go with asm fast paths
- `github.com/fsnotify/fsnotify` (v1.10.1)

No other dependencies. UUIDs via `crypto/rand` + hex. JSON via `encoding/json`. Logging via `log/slog`. TLS via `crypto/tls`.

## 1. Global specifications (used across milestones)

### 1.1 Data directory

`--data-dir` flag, else `$SYNCTHIS_DATA_DIR`, else `$XDG_DATA_HOME/syncthis`, else `~/.local/share/syncthis`. Contents:

```
config.json      device_name, listen, interface, chunk_size, rescan_interval, sync_interval, log_level
identity.key     PKCS#8 PEM ed25519 private key, mode 0600
identity.crt     self-signed X.509 (ed25519), 100-year validity, CN=syncthis
state.db         SQLite (WAL)
control.sock     unix socket, exists only while daemon runs
daemon.pid
```

Every folder root gets `<root>/.syncthis/tmp/` for partial downloads (same filesystem, so rename is atomic). Path `.syncthis` is always ignored by the scanner.

### 1.2 SQLite schema (migration `001_init.sql`, applied by `sqlitestore.Open`)

Pragmas on open: `journal_mode=WAL`, `synchronous=NORMAL`, `busy_timeout=5000`, `foreign_keys=ON`. `db.SetMaxOpenConns(1)` inside the daemon so goroutines serialize in Go rather than fighting in SQLite. Every multi-row write is a transaction of at most 500 rows.

```sql
CREATE TABLE schema_version (version INTEGER NOT NULL);

CREATE TABLE folders (
  id             TEXT PRIMARY KEY,               -- folder.ID, [a-z0-9._-]{1,64}
  path           TEXT NOT NULL UNIQUE,           -- absolute, cleaned
  label          TEXT NOT NULL DEFAULT '',
  mode           TEXT NOT NULL CHECK (mode IN ('sendreceive','sendonly','receiveonly')),
  delete_policy  TEXT NOT NULL CHECK (delete_policy IN ('propagate','never')),
  include_hidden INTEGER NOT NULL DEFAULT 0,
  symlink_policy TEXT NOT NULL CHECK (symlink_policy IN ('skip','store')),
  keep_empty_dirs INTEGER NOT NULL DEFAULT 1,
  paused         INTEGER NOT NULL DEFAULT 0,
  last_scan_at   INTEGER,                        -- unix ns, NULL = never
  last_error     TEXT,
  created_at     INTEGER NOT NULL
);

CREATE TABLE folder_ignore_patterns (
  folder_id TEXT NOT NULL REFERENCES folders(id) ON DELETE CASCADE,
  position  INTEGER NOT NULL,
  pattern   TEXT NOT NULL,
  PRIMARY KEY (folder_id, position)
);

CREATE TABLE files (
  folder_id    TEXT NOT NULL REFERENCES folders(id) ON DELETE CASCADE,
  rel_path     TEXT NOT NULL,                    -- slash-separated, relative to folder root
  kind         INTEGER NOT NULL,                 -- 1 file, 2 dir, 3 symlink
  size         INTEGER NOT NULL DEFAULT 0,
  mtime_ns     INTEGER NOT NULL DEFAULT 0,
  mode         INTEGER NOT NULL DEFAULT 0,       -- permission bits only
  hash         TEXT NOT NULL DEFAULT '',         -- 64 hex chars, '' for dirs/symlinks/deleted
  link_target  TEXT NOT NULL DEFAULT '',
  inode        INTEGER NOT NULL DEFAULT 0,
  deleted      INTEGER NOT NULL DEFAULT 0,       -- tombstone
  modified_by  TEXT NOT NULL DEFAULT '',         -- device.ID that produced this version
  sequence     INTEGER NOT NULL,                 -- per-folder monotonic, from folder_sequence
  synced_hash  TEXT NOT NULL DEFAULT '',         -- last hash known to match the peer (M6/M9)
  updated_at   INTEGER NOT NULL,
  PRIMARY KEY (folder_id, rel_path)
);
CREATE INDEX files_folder_hash ON files(folder_id, hash);
CREATE INDEX files_folder_seq  ON files(folder_id, sequence);

CREATE TABLE folder_sequence (folder_id TEXT PRIMARY KEY REFERENCES folders(id) ON DELETE CASCADE, next INTEGER NOT NULL);

CREATE TABLE changes (                            -- scan classification log, feeds status/"recent changes"
  seq       INTEGER PRIMARY KEY AUTOINCREMENT,
  folder_id TEXT NOT NULL REFERENCES folders(id) ON DELETE CASCADE,
  type      TEXT NOT NULL CHECK (type IN ('create','modify','delete','rename')),
  rel_path  TEXT NOT NULL,
  old_path  TEXT NOT NULL DEFAULT '',
  old_hash  TEXT NOT NULL DEFAULT '',
  new_hash  TEXT NOT NULL DEFAULT '',
  at        INTEGER NOT NULL
);

CREATE TABLE devices (
  id           TEXT PRIMARY KEY,                 -- device.ID (52-char base32)
  name         TEXT NOT NULL DEFAULT '',
  address      TEXT NOT NULL,                    -- host:port
  verified     INTEGER NOT NULL DEFAULT 0,
  added_at     INTEGER NOT NULL,
  last_seen_at INTEGER
);

CREATE TABLE transfers (                          -- inbound only; sender is stateless
  id          TEXT PRIMARY KEY,                  -- 32 hex chars
  folder_id   TEXT NOT NULL REFERENCES folders(id) ON DELETE CASCADE,
  rel_path    TEXT NOT NULL,
  peer_id     TEXT NOT NULL,
  hash        TEXT NOT NULL,                     -- expected whole-file hash
  size        INTEGER NOT NULL,
  mtime_ns    INTEGER NOT NULL,
  mode        INTEGER NOT NULL,
  chunk_size  INTEGER NOT NULL,
  chunk_count INTEGER NOT NULL,
  temp_path   TEXT NOT NULL,                     -- <root>/.syncthis/tmp/<id>.part
  state       TEXT NOT NULL CHECK (state IN ('pending','active','paused','verifying','done','failed')),
  error       TEXT NOT NULL DEFAULT '',
  created_at  INTEGER NOT NULL,
  updated_at  INTEGER NOT NULL
);
CREATE UNIQUE INDEX transfers_folder_path_live ON transfers(folder_id, rel_path) WHERE state NOT IN ('done','failed');

CREATE TABLE transfer_chunks (
  transfer_id TEXT NOT NULL REFERENCES transfers(id) ON DELETE CASCADE,
  idx         INTEGER NOT NULL,
  hash        TEXT NOT NULL DEFAULT '',          -- chunk BLAKE3 once verified
  verified    INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (transfer_id, idx)
);

CREATE TABLE conflicts (                          -- M9
  id            TEXT PRIMARY KEY,
  folder_id     TEXT NOT NULL REFERENCES folders(id) ON DELETE CASCADE,
  rel_path      TEXT NOT NULL,
  conflict_path TEXT NOT NULL,
  local_hash    TEXT NOT NULL,
  remote_hash   TEXT NOT NULL,
  peer_id       TEXT NOT NULL,
  detected_at   INTEGER NOT NULL,
  resolved      INTEGER NOT NULL DEFAULT 0
);
```

All tables land in migration 001 even though later milestones populate them; this avoids schema churn. `schema_version` = 1.

### 1.3 Device identity

- Key: ed25519. Cert: self-signed, `PublicKey` = ed25519 key, `SubjectKeyId` unset, long validity.
- `device.IDFromPublicKey(spkiDER []byte) device.ID` = `base32.StdEncoding.WithPadding(NoPadding).EncodeToString(sha256(spkiDER))` — 52 uppercase chars. `spkiDER` is `x509.Certificate.RawSubjectPublicKeyInfo`. Same function is used for our own ID and to derive the remote ID from its TLS leaf cert.
- `device.ParseID` accepts the 52-char form and the display form with dashes every 13 chars (`XXXXXXXXXXXXX-XXXXXXXXXXXXX-...`); `ID.Short()` returns first 7 chars for logs/conflict names.

### 1.4 Wire protocol (tcpwire), version 1

Transport: TLS 1.3 over TCP, ALPN `syncthis/1`. Both sides present their identity cert. `InsecureSkipVerify: true` (no CA), `ClientAuth: tls.RequireAnyClientCert`, and a `VerifyPeerCertificate` callback that computes `device.IDFromPublicKey(leaf.RawSubjectPublicKeyInfo)`; the client aborts with `ErrDeviceMismatch` if an expected ID was given and differs. Tailscale provides encryption in production; TLS is still used so loopback tests exercise the exact same code, and so auth is independent of the network.

Frame:

```
uint32 BE  length      = 1 + 4 + len(header) + len(payload); max 64 MiB
uint8      type
uint32 BE  header_len
[bytes]    header      JSON object (UTF-8)
[bytes]    payload     raw bytes; empty for all types except ChunkData
```

Message types and header fields:

| Type | Name | Direction | Header |
|---|---|---|---|
| 0x01 | Hello | both, first frame after TLS | `{"protocol_version":1,"device_id":"…","device_name":"…","folders":[{"id":"…","mode":"…"}]}` |
| 0x02 | Rejected | server → client, then close | `{"reason":"unknown_device"\|"protocol_version"\|"id_mismatch"}` |
| 0x03 | Ping | client → server | `{"nonce":123}` |
| 0x04 | Pong | server → client | `{"nonce":123}` |
| 0x10 | ManifestRequest | client → server | `{"folder_id":"…"}` |
| 0x11 | ManifestResponse | server → client | `{"folder_id":"…","sequence":N,"entries":[{"path","kind":1\|2\|3,"size","mtime_ns","mode","hash","link_target","deleted","modified_by","sequence"}]}` |
| 0x12 | ManifestChanged | server → client, unsolicited | `{"folder_id":"…","sequence":N}` |
| 0x20 | ChunkRequest | client → server | `{"folder_id","path","hash","index","chunk_size"}` |
| 0x21 | ChunkData | server → client | `{"folder_id","path","index","offset","length","chunk_hash"}` + payload = chunk bytes |
| 0x22 | ChunkError | server → client | `{"folder_id","path","index","reason":"not_found"\|"hash_changed"\|"folder_unknown"\|"folder_receive_only"\|"paused"}` |
| 0xF0 | Error | either | `{"code":"…","message":"…"}`; connection closes after |

Handshake sequence:

1. TCP connect, TLS 1.3 handshake, both sides derive the peer's ID from its leaf cert.
2. Both sides immediately write `Hello`. Each side reads the other's `Hello`; if `device_id` != cert-derived ID → `Rejected{id_mismatch}` and close; if `protocol_version` != 1 → `Rejected{protocol_version}`.
3. Server checks the client ID against its `devices` table (`DeviceLookup` callback). Unknown → `Rejected{unknown_device}` and close. This is intentional: a `peer add` probe from an unpaired device still learns the server's ID and name from the server's `Hello`, which is how pairing works (each side runs `peer add` against the other).
4. Connection is established. The client drives: one request in flight at a time; the client read loop routes `ManifestChanged` frames to a notification channel and everything else to the pending request. Client sends `Ping` every 30 s when idle; either side closes after 90 s of silence (`SetReadDeadline`).
5. Each daemon dials every known peer and acts as a client (pulling). Each pair of devices therefore has two connections, one per pull direction. No multiplexing needed.

`docs/PROTOCOL.md` (M8) reproduces this table.

### 1.5 Control socket (unixctl)

Unix socket `<datadir>/control.sock`, newline-delimited JSON, one request per connection.

Request `{"cmd":"status"|"pause"|"resume"|"rescan"|"repair"|"shutdown","folder_id":"…","dry_run":false}`.
Response `{"ok":true,"result":{…}}` or `{"ok":false,"error":"…","code":N}`.

CLI rule: `status`, `transfer list`, `pause`, `resume` work with or without a running daemon. Without one they read/write the DB directly and set `daemon.running=false`. With one, `status` asks the daemon (live peer connectivity), and `pause`/`resume` write the DB then notify the daemon.

### 1.6 Exit codes (`internal/cli/exitcodes.go`, documented in `docs/CLI.md`)

| Code | Const | Meaning |
|---|---|---|
| 0 | `ExitOK` | success |
| 1 | `ExitError` | unexpected/internal error |
| 2 | `ExitUsage` | bad flags/arguments (cobra usage errors are mapped here) |
| 3 | `ExitNotInitialized` | data dir has no identity; run `syncthis init` |
| 4 | `ExitNotFound` | folder / peer / transfer does not exist |
| 5 | `ExitAlreadyExists` | folder id/path or peer already configured |
| 6 | `ExitDaemonUnavailable` | command needs the daemon (`daemon run` conflict, `rescan --wait`) and none is running |
| 7 | `ExitPeerUnreachable` | dial/TLS/auth failure or `Rejected` |
| 8 | `ExitIntegrity` | hash verification failed, repair found unrecoverable state |
| 9 | `ExitConfirmationRequired` | interactive confirmation needed but `--no-input` set without `--yes` |
| 10 | `ExitDryRun` | dry run completed; changes would have been made (0 if nothing would change) |

`type ExitError struct{ Code int; Err error }`; `cli.Execute(args, stdin, stdout, stderr) int`. `main` calls `os.Exit(cli.Execute(...))`. All errors go to stderr; in `--json` mode errors are also emitted as `{"error":{"code":N,"message":"…"}}` on stdout and nothing else is printed.

### 1.7 `status --json` shape (`internal/app/status/report.go`)

```json
{
  "schema_version": 1,
  "device":  {"id": "ABC…", "name": "laptop", "data_dir": "/home/u/.local/share/syncthis"},
  "daemon":  {"running": true, "pid": 4242, "listen": "127.0.0.1:22000", "started_at": "2026-09-20T10:00:00Z"},
  "folders": [
    {"id": "docs", "path": "/home/u/docs", "label": "", "mode": "sendreceive", "delete_policy": "propagate",
     "paused": false, "state": "idle", "files": 120, "dirs": 8, "symlinks": 0, "bytes": 1048576,
     "last_scan_at": "2026-09-20T10:01:00Z", "pending_changes": 0, "error": null}
  ],
  "peers": [
    {"id": "DEF…", "name": "nas", "address": "100.64.0.2:22000", "verified": true,
     "connected": true, "last_seen_at": "2026-09-20T10:01:30Z", "latency_ms": 0.42, "error": null}
  ],
  "transfers": {
    "active": 1, "pending": 0, "paused": 0, "failed": 0,
    "items": [
      {"id": "a1b2…", "folder_id": "docs", "path": "big.iso", "peer_id": "DEF…", "direction": "in",
       "state": "active", "size": 734003200, "chunk_size": 1048576, "chunks_total": 700, "chunks_done": 312,
       "bytes_done": 327155712, "updated_at": "…", "error": null}
    ]
  },
  "conflicts": {"unresolved": 0}
}
```

`state` per folder is one of `idle|scanning|syncing|paused|error`. Times are RFC3339 UTC strings or `null`. Field set is frozen once M6 lands; new fields may be added but never renamed.

### 1.8 CLI surface (final)

```
syncthis init [--name NAME] [--listen HOST:PORT]
syncthis folder add PATH --id ID [--label L] [--mode sendreceive|sendonly|receiveonly]
                    [--delete-policy propagate|never] [--include-hidden] [--symlinks skip|store]
                    [--no-empty-dirs] [--ignore PATTERN]...
syncthis folder list | remove ID [--yes]
syncthis scan ID [--dry-run]
syncthis rescan [ID]
syncthis repair [ID] [--dry-run]
syncthis status [--json]
syncthis daemon run [--listen HOST:PORT] [--interface NAME] [--rescan-interval DUR] [--allow-public]
syncthis peer add HOST[:PORT] [--device-id ID] [--name N] [--verify]
syncthis peer list | remove ID | test ID|HOST[:PORT]
syncthis pause [FOLDER] | resume [FOLDER]
syncthis transfer list [--state S] | show TRANSFER_ID
syncthis conflict list                                       (M9)
syncthis completion bash|zsh|fish|powershell
syncthis version
Global: --data-dir DIR, --json, --no-input, --yes, --log-level debug|info|warn|error, --log-format text|json
```

---

## Milestone 1 — Skeleton, domain core, ports, fakes, CLI shell

**Goal.** Establish the layout and boundary rules, write the pure domain (no I/O), define every port, ship fakes, and get a cobra binary with global flags, exit codes and completion. Nothing touches disk yet except the arch test reading source files.

**Files to create.**

```
.golangci.yml                       (default linters; keeps `make lint` deterministic when golangci-lint exists)
internal/domain/device/device.go, device_test.go
internal/domain/folder/folder.go, folder_test.go
internal/domain/manifest/hash.go, entry.go, manifest.go, diff.go, diff_test.go, hash_test.go
internal/domain/ignore/matcher.go, matcher_test.go
internal/domain/transfer/transfer.go, transfer_test.go
internal/domain/syncplan/plan.go                (stub: types only; rules land in M6)
internal/app/ports/{clock,hasher,filesystem,store,watcher,transport,identity,events}.go
internal/app/fakes/{clock,hasher,filesystem,store,watcher,events,identity}.go
internal/arch/deps_test.go
internal/cli/root.go, flags.go, exitcodes.go, output.go, version.go, completion.go, root_test.go
cmd/syncthis/main.go                (replace stub), main_test.go (update)
```

**Key types and signatures.**

```go
// domain/device
type ID string
func ParseID(s string) (ID, error)
func IDFromPublicKey(spkiDER []byte) ID
func (id ID) Short() string        // first 7 chars
func (id ID) Display() string      // dashed groups of 13
type Device struct { ID ID; Name, Address string; Verified bool; AddedAt time.Time; LastSeenAt time.Time }

// domain/folder
type ID string
type Mode string          // "sendreceive" | "sendonly" | "receiveonly"
type DeletePolicy string  // "propagate" | "never"
type SymlinkPolicy string // "skip" | "store"
type Folder struct {
    ID ID; Path, Label string; Mode Mode; DeletePolicy DeletePolicy
    IncludeHidden bool; Symlinks SymlinkPolicy; KeepEmptyDirs bool
    IgnorePatterns []string; Paused bool; LastScanAt time.Time; LastError string; CreatedAt time.Time
}
func (f Folder) Validate() error
func (f Folder) CanReceive() bool  // Mode != sendonly
func (f Folder) CanSend() bool     // Mode != receiveonly
func ParseID(s string) (ID, error) // ^[a-z0-9][a-z0-9._-]{0,63}$

// domain/manifest
type Hash [32]byte
func (h Hash) String() string; func (h Hash) IsZero() bool; func ParseHash(hex string) (Hash, error)
type Kind uint8  // KindFile=1 KindDir=2 KindSymlink=3
type Entry struct {
    Path string; Kind Kind; Size int64; ModTime time.Time; Mode uint32; Hash Hash
    LinkTarget string; Inode uint64; Deleted bool; ModifiedBy device.ID; Sequence uint64; SyncedHash Hash
}
type Manifest struct { FolderID folder.ID; Sequence uint64; Entries map[string]Entry }
func New(folderID folder.ID) Manifest
func (m Manifest) Sorted() []Entry              // by Path
func (m Manifest) Live() []Entry                // !Deleted
type ChangeType uint8 // Create, Modify, Delete, Rename
type Change struct { Type ChangeType; Path, OldPath string; Old, New *Entry }
func Diff(old, cur Manifest) []Change
// Diff rules: Path in cur not in old (or old tombstone) -> Create; both live and (Hash|Size|Kind|LinkTarget|Mode) differ -> Modify;
// in old live, absent in cur -> Delete; then rename pass: pair each Delete with a Create whose Kind==File, Size>0,
// Hash equal; only when the (hash,size) bucket has exactly one Delete and one Create -> Rename{OldPath}. Deterministic order: sorted by Path.

// domain/ignore
type Matcher struct{ rules []rule }
func Parse(lines []string) (*Matcher, error)   // '#' comment, blank skipped, '!' negation, trailing '/' dir-only,
                                                 // leading '/' anchors to root, otherwise matches at any depth; '*' no '/', '**' any; last match wins
func (m *Matcher) Match(relPath string, isDir bool) bool
func Combine(ms ...*Matcher) *Matcher

// domain/transfer
const DefaultChunkSize = 1 << 20; const MinChunkSize = 4096
type ID string; type State string // "pending","active","paused","verifying","done","failed"
type Chunk struct { Index int; Offset int64; Length int; Hash manifest.Hash; Verified bool }
type Transfer struct {
    ID ID; FolderID folder.ID; Path string; PeerID device.ID; Hash manifest.Hash; Size, MTimeNS int64; Mode uint32
    ChunkSize int; TempPath string; State State; Chunks []Chunk; Error string; CreatedAt, UpdatedAt time.Time
}
func ChunkCount(size int64, chunkSize int) int  // 0-byte file -> 0 chunks
func NewInbound(id ID, f folder.ID, e manifest.Entry, peer device.ID, chunkSize int, tempPath string, now time.Time) Transfer
func (t Transfer) MissingChunks() []int; func (t Transfer) BytesDone() int64; func (t Transfer) Complete() bool
func (t *Transfer) MarkVerified(idx int, h manifest.Hash, now time.Time) error
func (t *Transfer) ResetChunks()  // all Verified=false (temp file lost)
```

Ports (`internal/app/ports`):

```go
type Clock interface { Now() time.Time }

type Hasher interface {
    Sum(r io.Reader) (manifest.Hash, error)
    SumBytes(b []byte) manifest.Hash
}

type FileInfo struct { Name string; Size int64; ModTime time.Time; Mode fs.FileMode; Inode uint64 }
type WalkFunc func(relPath string, info FileInfo) error
var SkipDir = fs.SkipDir
type TempFile interface { io.WriterAt; io.ReaderAt; Truncate(int64) error; Sync() error; Close() error; Name() string }
type FileSystem interface {
    Lstat(path string) (FileInfo, error)
    Walk(ctx context.Context, root string, fn WalkFunc) error   // lexical order, never follows symlinks, relPath "" for root
    Open(path string) (io.ReadCloser, error)
    ReadAt(path string, off int64, n int) ([]byte, error)      // short read at EOF returns fewer bytes, no error
    ReadLink(path string) (string, error)
    MkdirAll(path string, perm fs.FileMode) error
    CreateTemp(dir, pattern string) (TempFile, error)
    OpenTemp(path string) (TempFile, error)                    // reopen partial file for resume
    Rename(oldPath, newPath string) error
    Remove(path string) error                                  // files and empty dirs
    Chtimes(path string, mtime time.Time) error
    Chmod(path string, mode fs.FileMode) error
}

type WatchOp uint8 // OpCreate, OpWrite, OpRemove, OpRename, OpChmod, OpOverflow
type WatchEvent struct { Path string; Op WatchOp }
type Watcher interface { Watch(ctx context.Context, root string, out chan<- WatchEvent) error } // recursive; adds new subdirs; returns when ctx done

type FolderStore interface {
    SaveFolder(ctx context.Context, f folder.Folder) error
    GetFolder(ctx context.Context, id folder.ID) (folder.Folder, error)   // ErrNotFound
    ListFolders(ctx context.Context) ([]folder.Folder, error)
    DeleteFolder(ctx context.Context, id folder.ID) error
    SetFolderPaused(ctx context.Context, id folder.ID, paused bool) error
    SetFolderScan(ctx context.Context, id folder.ID, at time.Time, lastError string) error
}
type FileStore interface {
    LoadManifest(ctx context.Context, id folder.ID) (manifest.Manifest, error)
    UpsertEntries(ctx context.Context, id folder.ID, entries []manifest.Entry) error   // assigns Sequence, batches of 500
    MarkDeleted(ctx context.Context, id folder.ID, paths []string, by device.ID, now time.Time) error
    SetSyncedHash(ctx context.Context, id folder.ID, path string, h manifest.Hash) error
    FolderStats(ctx context.Context, id folder.ID) (FolderStats, error)  // Files, Dirs, Symlinks, Bytes
    AppendChanges(ctx context.Context, id folder.ID, changes []manifest.Change, now time.Time) error
    RecentChanges(ctx context.Context, id folder.ID, limit int) ([]ChangeRecord, error)
}
type DeviceStore interface { SaveDevice; GetDevice; ListDevices; DeleteDevice; TouchDevice(ctx, id, seenAt) }
type TransferStore interface {
    SaveTransfer(ctx context.Context, t transfer.Transfer) error       // upsert incl. chunk rows
    GetTransfer(ctx context.Context, id transfer.ID) (transfer.Transfer, error)
    FindLiveTransfer(ctx context.Context, f folder.ID, path string) (transfer.Transfer, error)
    ListTransfers(ctx context.Context, filter TransferFilter) ([]transfer.Transfer, error)
    MarkChunkVerified(ctx context.Context, id transfer.ID, idx int, h manifest.Hash, now time.Time) error
    SetTransferState(ctx context.Context, id transfer.ID, s transfer.State, errMsg string, now time.Time) error
    DeleteTransfer(ctx context.Context, id transfer.ID) error
}
type ConflictStore interface { SaveConflict; ListConflicts }
type MetadataStore interface { FolderStore; FileStore; DeviceStore; TransferStore; ConflictStore; Close() error }
var ErrNotFound = errors.New("not found"); var ErrAlreadyExists = errors.New("already exists")

type Identity interface { DeviceID() device.ID; Name() string }

type HelloInfo struct { DeviceID device.ID; Name string; Folders []FolderAdvert }   // FolderAdvert{ID folder.ID; Mode folder.Mode}
type ChunkRequest struct { FolderID folder.ID; Path string; Hash manifest.Hash; Index, ChunkSize int }
type ChunkData struct { Index int; Offset int64; Data []byte; ChunkHash manifest.Hash }
type ChunkErrorReason string
type PeerConn interface {
    RemoteID() device.ID; RemoteHello() HelloInfo
    Ping(ctx context.Context) (time.Duration, error)
    RequestManifest(ctx context.Context, id folder.ID) (manifest.Manifest, error)
    RequestChunk(ctx context.Context, req ChunkRequest) (ChunkData, error)   // returns *ChunkError on 0x22
    Notifications() <-chan folder.ID
    Close() error
}
type ProbeResult struct { DeviceID device.ID; Name string; Accepted bool; RejectReason string; Latency time.Duration }
type Dialer interface {
    Dial(ctx context.Context, addr string, expect device.ID) (PeerConn, error)
    Probe(ctx context.Context, addr string) (ProbeResult, error)
}
type PeerHandler interface {   // implemented by app/syncer.Server, called by the listener adapter
    Hello(ctx context.Context) HelloInfo
    Manifest(ctx context.Context, peer device.ID, id folder.ID) (manifest.Manifest, error)
    Chunk(ctx context.Context, peer device.ID, req ChunkRequest) (ChunkData, error)
}
type DeviceLookup func(ctx context.Context, id device.ID) bool
type Listener interface {
    Serve(ctx context.Context, h PeerHandler, known DeviceLookup) error
    Addr() string                       // valid after Serve has bound; "" before
    Broadcast(id folder.ID, seq uint64) // ManifestChanged to all connected clients
}

type Event struct { Kind string; At time.Time; Fields map[string]any }
type EventSink interface { Publish(Event) }
```

Fakes: `fakes.Clock{Now}`, `fakes.Hasher` (uses `crypto/sha256` truncated to 32 bytes — any deterministic function is fine for domain tests), `fakes.FileSystem` (in-memory tree keyed by path, supports every method, records `Renames`/`Removes`), `fakes.Store` (maps + mutex), `fakes.Watcher` (test pushes events), `fakes.Events` (records, `Wait(kind, n, timeout)`), `fakes.Identity`.

CLI shell: `cli.Execute(args []string, stdin io.Reader, stdout, stderr io.Writer) int`; `NewRootCmd(deps *Deps)`; global flags defined in `flags.go` bound to a `GlobalOpts` struct; `output.go` with `Printer{JSON bool; W io.Writer}` offering `PrintJSON(v)` and `Table(headers, rows)`; `version` and `completion` commands; root sets `SilenceUsage`, `SilenceErrors`, and translates cobra errors to `ExitUsage`.

**Tests.**

- `device_test.go`: `TestIDFromPublicKey_Stable` (same input, same 52-char output), `TestParseID_AcceptsDisplayForm`, `TestParseID_RejectsBadLength`.
- `folder_test.go`: `TestValidate_RejectsRelativePath`, `TestValidate_RejectsBadID`, `TestCanReceive_CanSend_ByMode`.
- `diff_test.go`: `TestDiff_Create`, `TestDiff_Modify`, `TestDiff_Delete`, `TestDiff_RenameSingle`, `TestDiff_RenameAmbiguousFallsBackToDeleteCreate` (two deletes, one create, same hash), `TestDiff_EmptyFilesNeverRename`, `TestDiff_TombstoneThenRecreateIsCreate`, `TestDiff_DeterministicOrder`.
- `matcher_test.go`: table test with cases for `*.tmp`, `/build`, `build/`, `**/node_modules`, `!keep.tmp`, `docs/**/*.md`, hidden `.git`, last-match-wins.
- `transfer_test.go`: `TestChunkCount` (0, 1, chunk_size, chunk_size+1), `TestMarkVerified_OutOfRange`, `TestMissingChunks_AfterPartialVerify`, `TestComplete`.
- `arch/deps_test.go`: `TestDomainImportsStdlibOnly`, `TestAppDoesNotImportOSOrAdapters`, `TestAdaptersDoNotImportEachOther`, `TestCmdImportsOnlyCLI` — parse every `.go` file under `internal/` with `go/parser` (ImportsOnly) and check the rules table in section 0.
- `cli/root_test.go`: `TestExecute_NoArgsPrintsHelpExit0`, `TestExecute_UnknownCommandExit2`, `TestVersionJSON`, `TestCompletionBash_Emits`.

**Verification.**

```
go mod tidy && make lint && make test
go run ./cmd/syncthis --help
go run ./cmd/syncthis version --json
go run ./cmd/syncthis completion zsh | head
go run ./cmd/syncthis bogus; echo "exit=$?"     # expect 2
```

---

## Milestone 2 — Storage, hashing, filesystem, identity adapters; `init`, `folder add/list/remove`, basic `status`

**Goal.** Real adapters against temp dirs and real SQLite; a composition root; the first user-visible commands.

**Files.**

```
internal/adapters/sqlitestore/store.go, migrate.go, folders.go, files.go, devices.go, transfers.go, conflicts.go,
                              migrations/001_init.sql (embed.FS), store_test.go, files_test.go, transfers_test.go
internal/adapters/blake3hash/hasher.go, hasher_test.go
internal/adapters/localfs/fs.go, fs_linux.go (inode via syscall.Stat_t), fs_test.go
internal/adapters/identityfile/identity.go, identity_test.go
internal/adapters/sysclock/clock.go
internal/adapters/jsonconfig/config.go, config_test.go
internal/compose/compose.go, datadir.go
internal/app/folders/service.go, service_test.go
internal/app/status/service.go, report.go, service_test.go
internal/cli/init.go, folder.go, status.go, cli_integration_test.go
```

**Key signatures.**

```go
// sqlitestore
func Open(ctx context.Context, path string, opts Options{MaxOpenConns int}) (*Store, error)  // runs migrations
var _ ports.MetadataStore = (*Store)(nil)

// blake3hash
type Hasher struct{}; func New() Hasher   // blake3.New(32, nil); SumBytes via blake3.Sum256

// localfs
type FS struct{}; func New() FS           // Walk uses filepath.WalkDir, converts to slash relPath, Lstat semantics
// CreateTemp: os.CreateTemp(dir, pattern); OpenTemp: os.OpenFile(path, O_RDWR, 0)

// identityfile
type Identity struct{ id device.ID; name string; cert tls.Certificate }
func LoadOrCreate(dir, name string) (*Identity, bool /*created*/, error)
func Load(dir string) (*Identity, error)       // ErrNotInitialized if missing
func (i *Identity) DeviceID() device.ID; Name() string; TLSCertificate() tls.Certificate

// jsonconfig
type Config struct {
    DeviceName string `json:"device_name"`; Listen string `json:"listen"`; Interface string `json:"interface"`
    ChunkSize int `json:"chunk_size"`; RescanInterval Duration `json:"rescan_interval"`; SyncInterval Duration `json:"sync_interval"`
    LogLevel string `json:"log_level"`
}
func Default() Config; func Load(dir string) (Config, error); func Save(dir string, c Config) error

// compose
type Options struct { DataDir string; Logger *slog.Logger; Events ports.EventSink; RequireIdentity bool; MaxOpenConns int }
type App struct {
    DataDir string; Config jsonconfig.Config; Store ports.MetadataStore; FS ports.FileSystem; Hasher ports.Hasher
    Clock ports.Clock; Identity *identityfile.Identity; Events ports.EventSink; Logger *slog.Logger
    Folders *folders.Service; Status *status.Service   // later: Scanner, Peers, Repair, Daemon
}
func Build(ctx context.Context, o Options) (*App, error)   // returns cli-mappable ErrNotInitialized
func (a *App) Close() error

// app/folders
type Service struct { store ports.FolderStore; fs ports.FileSystem; clock ports.Clock }
func (s *Service) Add(ctx context.Context, f folder.Folder) (folder.Folder, error)  // validates, checks dir exists, ErrAlreadyExists on id or path
func (s *Service) List(ctx) ([]folder.Folder, error); Remove(ctx, id) error; Get(ctx, id)
func (s *Service) SetPaused(ctx, id, bool) error

// app/status
type Service struct { store ports.MetadataStore; identity ports.Identity; clock ports.Clock; dataDir string }
func (s *Service) Report(ctx context.Context, live *LiveInfo) (Report, error)   // LiveInfo nil when no daemon
```

`init`: creates data dir (0700), identity, config with defaults (`listen: 127.0.0.1:22000`), opens DB (migrates). Idempotent; second run prints existing ID and exits 0. `--name` defaults to `os.Hostname()`.

**Tests.**

- `sqlitestore/store_test.go`: `TestOpen_MigratesFreshDB` (schema_version=1, all tables exist), `TestOpen_IdempotentOnExistingDB`, `TestFolders_CRUD_AndCascade` (deleting a folder removes files/patterns/transfers), `TestFolderAlreadyExists_ByPath`, `TestConcurrentWritersDoNotBusyError` (20 goroutines upserting with `MaxOpenConns:1` and a second `Open` on the same file simulating the CLI).
- `files_test.go`: `TestUpsertEntries_AssignsIncreasingSequence`, `TestUpsertEntries_BatchesOver500`, `TestLoadManifest_RoundTripsAllFields` (mtime ns precision, mode, link target, deleted, synced_hash), `TestMarkDeleted_CreatesTombstone`, `TestFolderStats`.
- `transfers_test.go`: `TestSaveTransfer_UpsertsChunks`, `TestMarkChunkVerified_Persists`, `TestFindLiveTransfer_IgnoresDone`, `TestLiveUniqueIndex_RejectsSecondLive`.
- `blake3hash/hasher_test.go`: `TestSum_KnownVector` (BLAKE3 of empty input = `af1349b9…`, of `"abc"` = `6437b3ac…`), `TestSumReader_EqualsSumBytes`.
- `localfs/fs_test.go`: `TestWalk_SlashPathsLexicalNoFollow` (symlink to dir not descended), `TestReadAt_ShortReadAtEOF`, `TestCreateTemp_Rename_Atomic`, `TestInodePopulated`.
- `identityfile/identity_test.go`: `TestLoadOrCreate_CreatesThenLoadsSameID`, `TestKeyFileMode0600`, `TestIDMatchesCertSPKI`.
- `folders/service_test.go` (with fakes): `TestAdd_RejectsDuplicateID`, `TestAdd_RejectsDuplicatePath`, `TestAdd_RequiresExistingDir`, `TestRemove_NotFound`.
- `cli/cli_integration_test.go` (real temp data dir, runs `cli.Execute`): `TestInit_ThenStatusJSON` (device.id 52 chars, daemon.running=false), `TestFolderAddListRemove_Text` , `TestFolderAdd_DuplicateExit5`, `TestFolderRemove_NoInputWithoutYesExit9`, `TestCommandsBeforeInitExit3`.

**Verification.**

```
make lint && make test
export SYNCTHIS_DATA_DIR=$(mktemp -d); mkdir -p /tmp/s1
go run ./cmd/syncthis init --name dev
go run ./cmd/syncthis folder add /tmp/s1 --id s1
go run ./cmd/syncthis folder list
go run ./cmd/syncthis status --json | python3 -m json.tool
go run ./cmd/syncthis folder remove s1 --yes
```

---

## Milestone 3 — Scan engine: classification, ignore rules, policies, `scan --dry-run`, `rescan`, `repair`

**Goal.** Full and partial scans that hash with BLAKE3, classify create/modify/delete/rename, honour `.syncthisignore` + configured patterns + hidden/symlink/empty-dir policy, and persist to SQLite in bounded batches. `repair` restores consistency between DB and disk.

**Files.**

```
internal/app/scan/scanner.go, walk.go, result.go, scanner_test.go
internal/app/repair/service.go, service_test.go
internal/cli/scan.go, rescan.go, repair.go
internal/cli/scan_integration_test.go
```

**Key signatures.**

```go
// app/scan
type Options struct { DryRun bool; Paths []string /* relative subtrees; nil = full */ ; ProgressEvery int }
type Result struct { FolderID folder.ID; Changes []manifest.Change; Scanned, Hashed int; Bytes int64; Ignored int; Duration time.Duration; DryRun bool }
type Scanner struct { fs ports.FileSystem; hasher ports.Hasher; store ports.MetadataStore; clock ports.Clock; id ports.Identity; events ports.EventSink; log *slog.Logger }
func New(...) *Scanner
func (s *Scanner) Scan(ctx context.Context, f folder.Folder, o Options) (Result, error)
func LoadIgnore(ctx, fs ports.FileSystem, f folder.Folder) (*ignore.Matcher, error)   // configured patterns + <root>/.syncthisignore; ".syncthis/" and ".syncthisignore" always added first
```

Scan algorithm:

1. Build matcher. Load current DB manifest (`LoadManifest`).
2. `fs.Walk(root)`; for each entry: skip `.syncthis`; apply hidden policy (name starts with `.` and `!IncludeHidden` → skip subtree); apply matcher (dir-only rules prune subtree); symlink: `skip` → ignore, `store` → Entry{Kind: Symlink, LinkTarget}; dir → Entry{Kind: Dir} only if `KeepEmptyDirs` (files inside imply parents anyway); file → if DB entry exists, live, same Size/ModTime/Inode → reuse Hash; else hash via `hasher.Sum(fs.Open)` (`Hashed++`). Check `ctx.Err()` every 100 entries.
3. Build `cur` Manifest; `changes := manifest.Diff(old, cur)`.
4. If `!DryRun`: `UpsertEntries` for creates/modifies/renames (ModifiedBy = own ID), `MarkDeleted` for deletes (and rename old paths), `AppendChanges`, `SetFolderScan(now, "")`. Publish `scan.completed` event with counts.
5. Partial scan (`Paths` non-nil): Walk each subtree only, and only diff entries whose Path has one of those prefixes; the rest of `old` is carried over untouched. If a subtree root no longer exists, treat every DB entry under it as deleted.

Atomic-save handling (editor writes `x.tmp` then renames over `x`): that appears as Modify of `x` (same path, new hash/inode) — a test asserts this rather than Delete+Create+Rename.

`repair.Service.Run(ctx, folderID, dryRun) (repair.Report, error)`: (a) DB entries whose file is missing on disk → mark deleted; (b) entries whose size/mtime differ → re-hash and update; (c) transfers whose temp file is missing → `ResetChunks` and state pending; (d) files under `.syncthis/tmp` without a live transfer → remove; (e) `folder_sequence` lower than max(files.sequence) → fix. Report counts per category.

CLI: `scan ID [--dry-run]` prints the change list (text table or JSON `{"folder_id","changes":[{"type","path","old_path","hash"}],"summary":{...}}`); exit 10 when dry-run and changes non-empty. `rescan [ID]` = full scan of one/all folders (M4 will route through the daemon when running). `repair [ID] [--dry-run]`.

**Tests.**

- `scan/scanner_test.go` with `fakes.FileSystem`: `TestScan_InitialCreatesAll`, `TestScan_ModifyDetectedByMtime`, `TestScan_ReusesHashWhenUnchanged` (hasher call count 0 on second scan), `TestScan_Delete`, `TestScan_RenameSameHash`, `TestScan_EditorAtomicSaveIsModify`, `TestScan_HiddenSkippedByDefault`, `TestScan_HiddenIncludedWithPolicy`, `TestScan_SymlinkSkip`, `TestScan_SymlinkStore`, `TestScan_EmptyDirKeptOrDropped`, `TestScan_IgnoreFileAndConfiguredPatterns`, `TestScan_SyncthisDirAlwaysIgnored`, `TestScan_DryRunWritesNothing`, `TestScan_PartialSubtreeOnlyTouchesPrefix`, `TestScan_UnicodeNames`, `TestScan_CancelMidwayLeavesDBConsistent` (ctx cancelled → error, DB unchanged because persistence happens after the walk).
- `repair/service_test.go`: `TestRepair_MarksMissingFilesDeleted`, `TestRepair_RehashesStale`, `TestRepair_ResetsTransferWithoutTempFile`, `TestRepair_RemovesOrphanTemp`, `TestRepair_DryRunNoWrites`.
- `cli/scan_integration_test.go` (real fs + sqlite): `TestScan_RealTree_1000Files` (generate 1000 files across 50 dirs, scan, status counts match, second scan yields zero changes), `TestScan_DryRunExit10`, `TestScan_RenameOnDisk` (os.Rename then scan → one `rename` change).

**Verification.**

```
make lint && make test
go run ./cmd/syncthis folder add /tmp/s1 --id s1 --ignore '*.tmp'
seq 1 200 | xargs -I{} sh -c 'echo {} > /tmp/s1/f{}.txt'; echo x > /tmp/s1/junk.tmp
go run ./cmd/syncthis scan s1 --dry-run; echo "exit=$?"    # 10, junk.tmp absent
go run ./cmd/syncthis scan s1 && mv /tmp/s1/f1.txt /tmp/s1/g1.txt && go run ./cmd/syncthis scan s1   # shows rename
rm /tmp/s1/f2.txt; go run ./cmd/syncthis repair s1 --dry-run
go run ./cmd/syncthis status --json | python3 -c 'import json,sys; print(json.load(sys.stdin)["folders"][0]["files"])'
```

---

## Milestone 4 — `daemon run` (local only): fsnotify watcher, event coalescing, periodic reconciliation, control socket

**Goal.** A long-running process that keeps folder metadata current via events plus full rescans, and a control socket so the CLI can query/instruct it. No networking yet.

**Files.**

```
internal/adapters/fsnotifywatch/watcher.go, watcher_test.go
internal/adapters/unixctl/server.go, client.go, protocol.go, unixctl_test.go
internal/app/daemon/daemon.go, folderloop.go, coalesce.go, api.go, daemon_test.go, coalesce_test.go
internal/cli/daemon.go, pause_resume.go (pause/resume commands land here but stay hidden until M7? No: implement now, they only flip `paused`)
internal/cli/daemon_integration_test.go
```

**Key signatures.**

```go
// app/daemon
type Config struct { RescanInterval, SyncInterval, Debounce time.Duration; Listen string; Interface string; AllowPublic bool; ChunkSize int }
type Deps struct {
    Store ports.MetadataStore; FS ports.FileSystem; Hasher ports.Hasher; Clock ports.Clock; Identity ports.Identity
    Watcher ports.Watcher; Events ports.EventSink; Log *slog.Logger
    Scanner *scan.Scanner; Repair *repair.Service; Status *status.Service
    Dialer ports.Dialer; Listener ports.Listener   // nil until M5
}
type Daemon struct{ ... }
func New(cfg Config, d Deps) *Daemon
func (d *Daemon) Run(ctx context.Context) error          // blocks; returns nil on ctx cancel after clean shutdown
func (d *Daemon) Ready() <-chan struct{}                 // closed once watchers, listener and control are up (tests)
// api.go — served over the control socket
type API interface {
    Status(ctx context.Context) (status.Report, error)
    Pause(ctx context.Context, id folder.ID) error; Resume(ctx context.Context, id folder.ID) error   // "" = all
    Rescan(ctx context.Context, id folder.ID) error   // enqueue full scan
    Repair(ctx context.Context, id folder.ID, dryRun bool) (repair.Report, error)
    Shutdown(ctx context.Context) error
}
// coalesce.go — pure, testable
type Coalescer struct{ debounce time.Duration; ... }
func (c *Coalescer) Add(ev ports.WatchEvent)             // maps event path -> parent dir rel path; OpOverflow -> full scan flag
func (c *Coalescer) Flush() (paths []string, full bool)  // called on timer tick when quiet for debounce

// adapters/unixctl
func Serve(ctx context.Context, sockPath string, api daemon.API) error
type Client struct{ path string }; func (c *Client) Status(ctx) (status.Report, error) ... mirrors API; returns ErrDaemonUnavailable on connect failure
```

Folder loop per folder (`folderloop.go`): one goroutine owns a folder: starts `Watcher.Watch` into a channel, feeds a `Coalescer`, on flush runs `Scanner.Scan(Paths: dirs)`, every `RescanInterval` runs a full scan, honours `paused` (drains events, skips scans). Errors are logged and stored via `SetFolderScan(now, err)`; loop never exits on scan error. `folder add/remove` while running: the daemon re-reads the folders table every `RescanInterval` and starts/stops loops (simplest correct approach; no hot-reload channel needed).

fsnotify adapter: recursive add on start (walk dirs), add new directories on `Create` events that are dirs, translate ops; on `fsnotify.ErrEventOverflow`/`Errors` channel emit `OpOverflow`. Note the inotify watch-limit risk in the log at startup (count of watches).

Daemon startup: write `daemon.pid`, refuse to start if another pid is alive (`ExitDaemonUnavailable` with message), remove stale socket, install SIGINT/SIGTERM handling in `cli/daemon.go` (not in app), on exit remove socket and pid.

CLI routing: `status` → try `unixctl.Client.Status`; on `ErrDaemonUnavailable` fall back to `status.Service.Report(ctx, nil)`. `rescan`/`repair` → daemon if running else direct. `pause`/`resume` → `folders.Service.SetPaused` then best-effort notify daemon.

**Tests.**

- `coalesce_test.go`: `TestCoalesce_MergesSiblingEventsToParentDir`, `TestCoalesce_OverflowForcesFull`, `TestCoalesce_FlushEmptyWhenNoEvents`.
- `fsnotifywatch/watcher_test.go` (real temp dir): `TestWatch_EmitsCreateWrite`, `TestWatch_NewSubdirIsWatched` (mkdir then create file inside → event), `TestWatch_StopsOnCancel`.
- `daemon/daemon_test.go` (fakes, `fakes.Watcher` pushes events): `TestDaemon_EventTriggersPartialScan` (scanner invoked with Paths=[dir]), `TestDaemon_PeriodicFullScan` (fake clock/ticker or 20ms interval), `TestDaemon_PausedFolderSkipsScans`, `TestDaemon_PicksUpNewFolderOnReload`, `TestDaemon_CleanShutdown`.
- `unixctl_test.go`: `TestRoundTrip_Status`, `TestClient_NoSocketReturnsDaemonUnavailable`, `TestServer_MalformedRequestReturnsError`.
- `cli/daemon_integration_test.go`: `TestDaemonRun_DetectsFileWithinTwoSeconds` (start `daemon run` in a goroutine via `cli.Execute` with a cancellable ctx, write file, poll `status --json` until files=1; assert < 2 s), `TestDaemonRun_SecondInstanceExit6`, `TestPauseResume_ViaCLIReflectedInStatus`.

**Verification.**

```
make lint && make test
go run ./cmd/syncthis daemon run --rescan-interval 10s &   # in one terminal
echo hello > /tmp/s1/new.txt; sleep 1; go run ./cmd/syncthis status --json | grep -c '"files"'
go run ./cmd/syncthis pause s1 && go run ./cmd/syncthis status | grep paused
go run ./cmd/syncthis resume s1; go run ./cmd/syncthis rescan s1
kill %1; test ! -e "$SYNCTHIS_DATA_DIR/control.sock"
```

---

## Milestone 5 — Identity-authenticated TCP transport, `peer add/list/remove/test`, listener in the daemon

**Goal.** Two daemons can authenticate each other over TLS on loopback, exchange `Hello`, answer `Ping`. Pairing via `peer add` in both directions. No file data yet.

**Files.**

```
internal/adapters/tcpwire/frame.go, frame_test.go, codec.go, codec_test.go, tlsconfig.go, handshake.go,
                          client.go, server.go, netiface.go, tcpwire_test.go
internal/app/peers/service.go, service_test.go
internal/app/syncer/server.go               (PeerHandler: Hello + Ping only in this milestone; Manifest/Chunk return Error until M6)
internal/cli/peer.go, peer_integration_test.go
docs/PROTOCOL.md                             (framing + handshake + type table from section 1.4)
```

**Key signatures.**

```go
// tcpwire/frame.go
type MsgType uint8  // constants from the table
type Frame struct { Type MsgType; Header []byte; Payload []byte }
const MaxFrame = 64 << 20
func WriteFrame(w io.Writer, f Frame) error
func ReadFrame(r io.Reader) (Frame, error)    // ErrFrameTooLarge, io.ErrUnexpectedEOF
// codec.go: header DTOs (helloHeader, manifestHeader, entryDTO, chunkReqHeader, chunkDataHeader, ...) and
func toWireEntry(manifest.Entry) entryDTO / fromWireEntry(entryDTO) (manifest.Entry, error)
// tlsconfig.go
func ServerTLS(cert tls.Certificate) *tls.Config; func ClientTLS(cert tls.Certificate, expect device.ID) *tls.Config
func PeerIDFromConn(c *tls.Conn) (device.ID, error)
// client.go
type Dialer struct{ cert tls.Certificate; local ports.PeerHandler /* for our Hello */; timeout time.Duration; log *slog.Logger }
func NewDialer(cert tls.Certificate, hello func() ports.HelloInfo, log *slog.Logger) *Dialer
var _ ports.Dialer = (*Dialer)(nil)
type conn struct{ ... }  // implements ports.PeerConn: single in-flight request, mutex, read loop, notifications chan
// server.go
type Listener struct{ cert tls.Certificate; addr string; ln net.Listener; conns map[*serverConn]struct{}; ... }
func NewListener(cert tls.Certificate, addr string, log *slog.Logger) *Listener
var _ ports.Listener = (*Listener)(nil)
// netiface.go
func ResolveListen(listen, iface string, allowPublic bool) (string, error)  // iface non-empty -> first IPv4 of that interface + port from listen; rejects 0.0.0.0/:: unless allowPublic

// app/peers
type Service struct { store ports.DeviceStore; dialer ports.Dialer; clock ports.Clock }
type AddOptions struct { Address string; ExpectID device.ID; Name string; Verify bool; Confirm func(id device.ID, name string) (bool, error) }
func (s *Service) Add(ctx context.Context, o AddOptions) (device.Device, error)
   // Probe(addr); if ExpectID set and differs -> ErrDeviceMismatch; if Verify -> Confirm(id,name) must return true (cli supplies prompt / --yes / --no-input => ErrConfirmationRequired);
   // stores Device{Verified: Verify || ExpectID != ""}; Accepted=false is fine (other side has not added us yet) and is reported
func (s *Service) List(ctx) ([]device.Device, error); Remove(ctx, id) error
type TestResult struct { Address string; DeviceID device.ID; Name string; TLSOK, Authenticated bool; RejectReason string; Latency time.Duration; Err string }
func (s *Service) Test(ctx context.Context, target string) (TestResult, error)  // target = known device ID or host:port
```

Daemon integration (M5 part of `daemon.go`): if `Deps.Listener != nil`, `go Listener.Serve(ctx, syncer.Server, known)` where `known` = closure over `store.GetDevice`. The daemon also runs a `peerloop` per known device that dials with backoff (1 s → 30 s cap), keeps the connection alive with Ping, records `TouchDevice`, and exposes connectivity for `status` (`peers[].connected`, `latency_ms`). Peer table is re-read every `RescanInterval` like folders.

`peer test` output: `address`, `device_id`, `tls_ok`, `authenticated` (true when the remote accepted our Hello), `reject_reason`, `latency_ms`, `error`. Exit 7 when TCP/TLS fails or rejected; 0 otherwise.

**Tests.**

- `frame_test.go`: `TestFrame_RoundTrip`, `TestFrame_TooLargeRejected`, `TestFrame_TruncatedReturnsUnexpectedEOF`, `FuzzReadFrame` (never panics).
- `codec_test.go`: `TestEntryDTO_RoundTripPreservesNanoseconds`, `TestManifestHeader_10kEntriesUnderMaxFrame`.
- `tcpwire_test.go` (real loopback, two identities from `identityfile.LoadOrCreate` in temp dirs): `TestHandshake_KnownPeerAccepted`, `TestHandshake_UnknownPeerRejectedWithReason` (probe still returns server ID), `TestDial_ExpectedIDMismatchFails`, `TestPing_LatencyPositive`, `TestIdleTimeout_ClosesAfterSilence` (with shortened timeouts via unexported setter), `TestResolveListen_RejectsPublicWithoutFlag`, `TestResolveListen_LoopbackInterface` (`lo` → 127.0.0.1).
- `peers/service_test.go` (fake Dialer): `TestAdd_TOFUStoresProbeID`, `TestAdd_ExpectIDMismatch`, `TestAdd_VerifyRequiresConfirm`, `TestAdd_VerifyDeclined`, `TestTest_ReportsRejectReason`.
- `cli/peer_integration_test.go`: `TestPeerAdd_BothDirections_ThenTestAuthenticated` (two data dirs, daemon B running on `127.0.0.1:0` — port discovered via `Daemon.ListenAddr()` exposed for tests; A `peer add`, then B `peer add` A's addr… A isn't listening in this test, so B uses `--device-id` from A's `status --json`; then A `peer test` reports authenticated=true), `TestPeerAdd_NoInputVerifyWithoutYesExit9`, `TestPeerTest_UnreachableExit7`.

**Verification.**

```
make lint && make test
# two data dirs in two terminals
export A=$(mktemp -d) B=$(mktemp -d)
syncthis --data-dir $A init --name a --listen 127.0.0.1:22001
syncthis --data-dir $B init --name b --listen 127.0.0.1:22002
syncthis --data-dir $A daemon run &  syncthis --data-dir $B daemon run &
syncthis --data-dir $A peer add 127.0.0.1:22002 --verify --yes
syncthis --data-dir $B peer add 127.0.0.1:22001 --yes
syncthis --data-dir $A peer test 127.0.0.1:22002 --json      # authenticated:true, latency_ms
syncthis --data-dir $A status --json | python3 -c 'import json,sys; print(json.load(sys.stdin)["peers"])'
```

---

## Milestone 6 — Manifest exchange, chunked transfer with journal, resume, atomic install; e2e one-way sync

**Goal.** Files flow between two daemons. Receiver-driven: each daemon pulls from each peer. Transfers survive process restart without refetching verified chunks. `status --json` includes transfers.

**Files.**

```
internal/domain/syncplan/plan.go, plan_test.go            (rules below)
internal/app/syncer/server.go (complete Manifest/Chunk), puller.go, installer.go, puller_test.go, installer_test.go, server_test.go
internal/app/daemon/peerloop.go (extend: sync cycle), daemon.go (extend)
internal/app/status/report.go (transfers + peers populated)
internal/e2e/harness.go, oneway_test.go, resume_test.go
docs/STATE.md                                              (schema + journal semantics)
```

**Key signatures.**

```go
// domain/syncplan
type Kind uint8 // Pull, MkDir, Symlink, Delete, Skip, Conflict
type Action struct { Kind Kind; Path string; Remote, Local *manifest.Entry; Reason string }
func Plan(local, remote manifest.Manifest, f folder.Folder) []Action
// Rules, per remote entry (sorted by path, dirs before files):
//  - if !f.CanReceive(): no actions at all.
//  - remote live, local absent or tombstone            -> Pull / MkDir / Symlink by Kind
//  - remote live, local live, hashes equal             -> Skip("in-sync")   (caller records SyncedHash)
//  - remote live, local live, hashes differ:
//        local.Hash == local.SyncedHash (local unchanged since last sync) -> Pull
//        remote.Hash == local.SyncedHash (remote unchanged)                -> Skip("local-newer")
//        else                                                              -> Conflict("both-modified")   [M6: treated as Skip + warn; M9: keep-both]
//  - remote tombstone, local live:
//        f.DeletePolicy == never -> Skip("delete-policy-never")
//        local.Hash == local.SyncedHash -> Delete   else -> Conflict("delete-vs-modify") [M6: Skip]
//  - remote tombstone, local absent -> nothing.
//  Local-only live entries are never touched (the peer pulls them).

// app/syncer
type Server struct { store ports.MetadataStore; fs ports.FileSystem; hasher ports.Hasher; identity ports.Identity; folders func(ctx) []folder.Folder }
func (s *Server) Hello(ctx) ports.HelloInfo
func (s *Server) Manifest(ctx, peer device.ID, id folder.ID) (manifest.Manifest, error)    // ErrFolderUnknown, ErrFolderReceiveOnly, ErrPaused
func (s *Server) Chunk(ctx, peer device.ID, req ports.ChunkRequest) (ports.ChunkData, error) // reads from disk; verifies the DB hash for req.Path == req.Hash else ChunkError{hash_changed}; chunk_hash = hasher.SumBytes(data)

type Puller struct { store ports.MetadataStore; fs ports.FileSystem; hasher ports.Hasher; clock ports.Clock; events ports.EventSink; chunkSize int; log *slog.Logger }
func (p *Puller) SyncFolder(ctx context.Context, conn ports.PeerConn, f folder.Folder) (SyncSummary, error)
   // 1. remote := conn.RequestManifest; local := store.LoadManifest
   // 2. actions := syncplan.Plan(local, remote, f)
   // 3. for MkDir/Symlink/Delete: apply via installer; for Skip("in-sync"): SetSyncedHash
   // 4. for Pull: t := store.FindLiveTransfer or transfer.NewInbound(...); p.resume(t); p.fetch(ctx, conn, &t); p.install(t)
func (p *Puller) ResumeAll(ctx context.Context, f folder.Folder) error   // on daemon start: for each live transfer, re-verify chunks (below)
type SyncSummary struct { Pulled, Deleted, Dirs, Skipped, Conflicts int; Bytes int64 }

// installer.go
type Installer struct { fs ports.FileSystem; hasher ports.Hasher; clock ports.Clock }
func (i *Installer) TempDir(root string) string  // <root>/.syncthis/tmp
func (i *Installer) Install(ctx, root string, t transfer.Transfer, expectLocal *manifest.Entry) error
   // whole-file hash of temp == t.Hash else ErrIntegrity; if final exists and its (size,mtime) != expectLocal -> ErrChangedUnderneath;
   // MkdirAll(parent); Chmod; Chtimes(mtime); Rename(temp, final)
func (i *Installer) Delete(ctx, root, path string) error; MkDir(...); Symlink(...) -> (Symlink creation needs a FileSystem method: add `Symlink(target, path string) error` to the port in this milestone.)
```

Chunk fetch loop (`puller.fetch`): for each `idx` in `t.MissingChunks()`: `conn.RequestChunk`; verify `len(Data)` matches expected length and `hasher.SumBytes(Data) == ChunkHash` else `ErrIntegrity` (fail transfer after 3 mismatches); `temp.WriteAt(Data, offset)`; `temp.Sync()` every 16 chunks and at the end; `store.MarkChunkVerified`; publish `transfer.chunk_verified{transfer_id, index}`. On `ChunkError{hash_changed}`: set transfer failed with reason, delete temp; the next cycle re-plans against the new manifest. A `ports.ChunkData` for a 0-byte file: no chunks; create empty temp and install directly.

Resume rule (`puller.resume`): if temp file missing → `ResetChunks`. Otherwise for each chunk marked verified in the journal, re-read that range from the temp file and re-hash; any mismatch → unmark. This makes the journal advisory and the temp file authoritative, so a crash between `WriteAt` and `MarkChunkVerified` (or the reverse ordering) can never produce a wrong file. Whole-file verification before rename is the final guard.

Sync cycle in `peerloop.go`: after connect, for each folder in the intersection of local folders and the peer's `Hello.Folders` where local `CanReceive()` and not paused: `Puller.SyncFolder`. Re-run on `conn.Notifications()` (peer's manifest changed) and every `SyncInterval`. The daemon calls `Listener.Broadcast(folderID, seq)` after every local scan that produced changes. Folder state in status: `syncing` while a cycle runs.

Status: `transfers.items` from `ListTransfers`, `peers[].connected` from peerloop state. Counters `bytes_received_total` published as events, not in JSON (keeps the schema stable).

**Tests.**

- `syncplan/plan_test.go`: one test per rule above, plus `TestPlan_SendOnlyProducesNothing`, `TestPlan_DirsBeforeFiles`, `TestPlan_DeletePolicyNever`.
- `syncer/puller_test.go` (fakes: an in-memory `fakes.PeerConn` backed by a source `fakes.FileSystem`): `TestSync_PullsNewFile`, `TestSync_ChunkHashMismatchFailsTransfer`, `TestSync_ResumeSkipsVerifiedChunks` (pre-populate journal + temp; assert only missing indices requested), `TestSync_ResumeRehashDetectsCorruptTemp` (flip a byte in a "verified" chunk → refetched), `TestSync_ResumeMissingTempResetsAll`, `TestSync_ZeroByteFile`, `TestSync_HashChangedOnSenderFailsCleanly`, `TestSync_SetsSyncedHashWhenInSync`.
- `syncer/installer_test.go` (real temp dir via localfs): `TestInstall_AtomicRenameSameFS`, `TestInstall_WholeFileHashMismatch`, `TestInstall_RefusesWhenTargetChangedUnderneath`, `TestInstall_PreservesMtimeAndMode`, `TestInstall_CreatesParents`.
- `syncer/server_test.go`: `TestChunk_HashChangedReturnsChunkError`, `TestChunk_LastChunkShort`, `TestManifest_ReceiveOnlyReturnsError`, `TestManifest_IncludesTombstones`.
- `e2e/harness.go`: `type Node struct{ App *compose.App; Daemon *daemon.Daemon; Events *fakes.Events; Cancel func(); DataDir, Root string }`; `StartNode(t, dataDir, opts)`; `Pair(t, a, b)` (writes device rows directly with actual listen addrs); `WaitTreeEqual(t, a.Root, b.Root, timeout)`; `Restart(t, n)`. Test timings: `Debounce 50ms, RescanInterval 300ms, SyncInterval 200ms, ChunkSize 4096`.
- `e2e/oneway_test.go`: `TestOneWay_NewFilesPropagate` (A writes 30 files incl. nested dirs, unicode names, one empty file; B converges; hashes equal), `TestOneWay_ModifyPropagates`, `TestOneWay_DeletePropagates`, `TestOneWay_LargeFileMultipleChunks` (300 KiB with 4 KiB chunks).
- `e2e/resume_test.go`: `TestResume_AfterReceiverRestart`: A has a 128 KiB file (32 chunks); start B; wait until B's `Events` has ≥ 6 `transfer.chunk_verified`; `Cancel` B; assert `transfers` row state active/pending with n verified; `Restart` B with a fresh recorder; wait for convergence; assert second-run `chunk_verified` count == 32 − n (exactly, since the source is unchanged) and B's file hash == A's. `TestResume_AfterSenderRestart`: cancel A mid-transfer, restart A, B converges without a failed transfer.

**Verification.**

```
make lint && make test
go test ./internal/e2e/ -run 'OneWay|Resume' -count=3 -race
# manual with the two daemons from M5, folder id "docs" on both:
mkdir -p /tmp/a-docs /tmp/b-docs
syncthis --data-dir $A folder add /tmp/a-docs --id docs; syncthis --data-dir $B folder add /tmp/b-docs --id docs
head -c 50M /dev/urandom > /tmp/a-docs/big.bin; sleep 3; cmp /tmp/a-docs/big.bin /tmp/b-docs/big.bin && echo SAME
syncthis --data-dir $B status --json | python3 -c 'import json,sys; print(json.load(sys.stdin)["transfers"])'
```

---

## Milestone 7 — Folder modes, delete policy, pause/resume against the daemon, `transfer list/show`, dry-run for destructive sync

**Goal.** Phase 3 semantics: send-only never installs, receive-only never serves, deletes obey policy, pause stops both scanning and syncing immediately (including in-flight transfers, which resume later), and transfers can be inspected.

**Files.**

```
internal/app/syncer/server.go (receive-only + paused checks), puller.go (paused check between chunks -> State paused)
internal/app/daemon/daemon.go (Pause/Resume propagate to folder and peer loops via a per-folder pause channel)
internal/app/transfers/service.go, service_test.go       (List/Show read models)
internal/cli/transfer.go, pause_resume.go (finish), folder.go (--mode/--delete-policy flags already parsed; add `folder set ID --mode ... --delete-policy ...`)
internal/cli/sync_dryrun.go -> `syncthis sync-plan FOLDER PEER --json` : prints syncplan actions without applying (the `--dry-run` for destructive sync)
internal/e2e/modes_test.go, pause_test.go
```

**Key signatures.**

```go
// app/transfers
type Service struct{ store ports.TransferStore }
type Filter = ports.TransferFilter // { FolderID folder.ID; States []transfer.State; Limit int }
func (s *Service) List(ctx, Filter) ([]View, error); func (s *Service) Show(ctx, transfer.ID) (View, error)
type View struct { ID, FolderID, Path, PeerID, State string; Size, BytesDone int64; ChunkSize, ChunksTotal, ChunksDone int; Error string; CreatedAt, UpdatedAt time.Time; MissingChunks []int /* Show only */ }

// daemon pause semantics
// Pause(id): store.SetFolderPaused(true); folderloop stops scanning; peerloop's puller checks `paused(id)` before every chunk
// and returns ErrPaused -> SetTransferState(paused). Resume(id): flips flag, wakes loops; puller.ResumeAll re-verifies and continues.
```

`syncthis folder add ... --mode receiveonly` on B and `--mode sendonly` on A is the backup topology; the e2e tests use exactly that.

**Tests.**

- `syncer/server_test.go`: `TestManifest_ReceiveOnlyRefused`, `TestChunk_PausedRefused`.
- `syncplan/plan_test.go`: `TestPlan_SendOnlyNeverPulls` (already), `TestPlan_ReceiveOnlyPullsAndDeletes`.
- `transfers/service_test.go`: `TestList_FilterByState`, `TestShow_MissingChunks`.
- `daemon/daemon_test.go`: `TestPause_StopsInFlightTransferWithinOneChunk` (fake conn with blocking chunk channel), `TestResume_ContinuesFromJournal`.
- `e2e/modes_test.go`: `TestSendOnlyReceiveOnly_OneWayOnly` (B edits a file locally; A never changes; B's local edit is not reverted by A either—it stays because `local-newer` skip; assert both), `TestDeletePolicyNever_KeepsFile`, `TestDeletePolicyPropagate_Removes`.
- `e2e/pause_test.go`: `TestPause_HaltsTransfer_ResumeCompletes` (pause B via `daemon.API` mid-transfer, assert `chunk_verified` count stops increasing for 500 ms, resume, converge, total requests == chunk count).
- `cli` integration: `TestTransferList_JSONShape`, `TestSyncPlan_DryRunExit10WhenActions`.

**Verification.**

```
make lint && make test
syncthis --data-dir $B pause docs && syncthis --data-dir $B transfer list --json
syncthis --data-dir $B resume docs
syncthis --data-dir $A folder set docs --mode sendonly; syncthis --data-dir $B folder set docs --mode receiveonly --delete-policy never
rm /tmp/a-docs/big.bin; sleep 3; test -f /tmp/b-docs/big.bin && echo KEPT
```

---

## Milestone 8 — Hardening and documentation: crash safety, `--no-input`/`--yes` audit, JSON purity, `--interface`, docs

**Goal.** Close the cross-cutting requirements and prove crash safety; finalize documentation so the CLI, protocol and state format are specified independently of the code.

**Files.**

```
docs/CLI.md          every command, flags, exit code table, status JSON schema (copy of 1.7), examples
docs/PROTOCOL.md     finalize (1.4)
docs/STATE.md        finalize (1.2, data dir layout, journal semantics, migration policy)
docs/ARCHITECTURE.md layout + dependency rules + glossary of domain terms
internal/cli/prompt.go            Confirm(prompt) honoring --yes / --no-input / non-TTY -> ExitConfirmationRequired
internal/cli/cli_json_test.go     asserts no non-JSON bytes on stdout in --json mode for every command (table-driven over the command list)
internal/e2e/crash_test.go
internal/e2e/interface_test.go
cmd/syncthis/main.go              signal handling, slog handler selection (--log-format), version from `debug.ReadBuildInfo`
Makefile                          add `e2e` target (`go test ./internal/e2e/ -race -count=1`) and `cover`
```

**Work items.**

- Every destructive command (`folder remove`, `peer remove`, `repair` without `--dry-run`, `sync-plan --apply` if added) goes through `Confirm`. `--json` implies `--no-input`.
- Logging: `slog` with `--log-format json|text`, `--log-level`; stable fields `component`, `folder_id`, `peer_id`, `transfer_id`, `path` (paths only at debug level).
- `daemon run --interface lo` path exercised in a test through `ResolveListen`; document `--interface tailscale0` as the production default (`config.interface`).
- Crash tests: `TestCrash_KillDuringScanLeavesConsistentDB` — run the CLI as a subprocess (`os.Executable()` re-exec of the test binary with `SYNCTHIS_TEST_HELPER=1`, or build the binary once in `TestMain` with `go build -o $TMP/syncthis`), start `daemon run` on a 20k-file tree, `SIGKILL` at 300 ms, restart, `repair --dry-run` reports zero fixes after the next full scan, and `status` counts equal the on-disk count. `TestCrash_KillReceiverMidTransfer_SIGKILL` — subprocess variant of the M6 resume test (real process death rather than ctx cancel).
- `TestExitCodes_Table` in `cli`: table of (args, expected code) covering every code in 1.6.
- Update `.specs/tasks.md` checkboxes that are now done (the only spec file edit in the whole plan).

**Verification.**

```
make lint && make test && make e2e
for c in "status" "folder list" "peer list" "transfer list" "version"; do syncthis --data-dir $A $c --json | python3 -m json.tool >/dev/null && echo "ok: $c"; done
syncthis --data-dir $A folder remove docs --no-input; echo "exit=$?"    # 9
syncthis --data-dir $A daemon run --listen 0.0.0.0:22001; echo "exit=$?"   # 2: refuses a wildcard bind without --allow-public
syncthis --data-dir $A daemon run --interface lo --listen 0.0.0.0:22001   # binds 127.0.0.1:22001: the interface replaces the host
```

---

## Milestone 9 (optional, last) — Two-way sync with keep-both conflicts

**Goal.** Turn the `Conflict` actions from the plan into keep-both outcomes and expose them. Include only if M6–M8 are green; otherwise leave the `Conflict → Skip + warn` behaviour and mark this milestone deferred.

**Files.**

```
internal/domain/conflict/conflict.go, conflict_test.go
internal/app/syncer/puller.go (handle Kind==Conflict), conflicts.go
internal/adapters/sqlitestore/conflicts.go (complete)
internal/cli/conflict.go        `conflict list [--json]`
internal/e2e/twoway_test.go
```

**Key signatures.**

```go
// domain/conflict
type Record struct { ID string; FolderID folder.ID; Path, ConflictPath string; LocalHash, RemoteHash manifest.Hash; PeerID device.ID; DetectedAt time.Time; Resolved bool }
func ConflictName(path string, at time.Time, peer device.ID) string
// "report.md" -> "report.sync-conflict-20260920-153000-ABCDEFG.md"; keeps extension; dir part preserved
```

Keep-both rule in the puller: for `Conflict("both-modified")`: pull the remote version into a temp file as usual, install it at `ConflictName(path, now, peer)`, leave the local file untouched, set `SyncedHash` of `path` to the local hash (so the local version then propagates to the peer, which will in turn store *its* conflict copy — both sides end with both versions), record a `conflicts` row, publish `conflict.detected`. For `Conflict("delete-vs-modify")`: keep the local file, record a conflict, no delete. Conflict copies are ordinary files and sync like any other. Because the name embeds the *peer* ID and second-resolution timestamps from two different clocks can differ, each side produces a distinctly named copy; the e2e test asserts the set of live files contains exactly two content versions and both originals' hashes, not exact filenames.

**Tests.**

- `conflict_test.go`: `TestConflictName_KeepsExtension`, `TestConflictName_NestedDir`, `TestConflictName_NoExtension`.
- `syncplan/plan_test.go`: `TestPlan_BothModified_IsConflict`, `TestPlan_DeleteVsModify_IsConflict`.
- `syncer/puller_test.go`: `TestConflict_KeepBothWritesCopyAndRecord`, `TestConflict_LocalUntouched`.
- `e2e/twoway_test.go`: `TestTwoWay_IndependentEditsConverge` (A edits x, B edits y; both end identical), `TestTwoWay_ConcurrentEditKeepsBoth` (pause both, edit same file on A and B with different content, resume; both trees contain the original path plus one `sync-conflict` file; hashes of the two files across both nodes equal; `conflict list --json` non-empty on both), `TestTwoWay_NoSilentOverwrite` (fuzz-ish: 20 rounds of random edits on both sides with pauses; every content version ever written is present on at least one node at the end).

**Verification.**

```
make lint && make test && make e2e
syncthis --data-dir $A conflict list --json
```

---

## Top 5 risks and mitigations

1. **fsnotify event coalescing / missed events (inotify queue overflow, editor save storms, watch limits).** M4's `Coalescer` maps events to parent directories and debounces, so a burst becomes one partial scan; `OpOverflow` forces a full scan; the periodic reconciliation scan (M3 scanner, M4 loop) is the correctness backstop, so events are only an accelerator. M3's `TestScan_EditorAtomicSaveIsModify` and M4's coalesce tests pin the behaviour; the daemon logs the watch count at startup.

2. **Atomic rename across filesystems / partial installs.** M6 places temp files in `<root>/.syncthis/tmp`, which is always on the folder's filesystem, so `rename(2)` is atomic; the scanner ignores `.syncthis` unconditionally (M3). Install verifies the whole-file hash and refuses if the destination changed underneath (`ErrChangedUnderneath`). `repair` (M3/M6) removes orphan temp files.

3. **SQLite locking with concurrent goroutines and a concurrent CLI process.** M2 opens with WAL + `busy_timeout=5000`, uses `SetMaxOpenConns(1)` in the daemon so writers serialize in Go, and batches writes at 500 rows per transaction so scans never hold a long write lock. `TestConcurrentWritersDoNotBusyError` runs two `*sql.DB` handles on one file under load. Scan persistence happens after the walk, not interleaved with hashing.

4. **Chunk resume correctness (journal vs. bytes on disk).** M6 treats the journal as advisory: on resume every "verified" chunk is re-hashed from the temp file and unmarked on mismatch; a missing temp file resets all chunks; chunk hashes travel with the data and are checked before write; whole-file hash is checked before rename. `TestSync_ResumeRehashDetectsCorruptTemp`, `TestResume_AfterReceiverRestart` (exact count of refetched chunks), and M8's SIGKILL variant cover it. Fixed chunk size is stored per transfer so a config change cannot misalign an in-flight transfer.

5. **Rename detection heuristics (false positives, ambiguity).** M1's `Diff` only pairs a Delete and a Create when Kind is File, Size > 0, hashes match, and the (hash,size) bucket contains exactly one of each; anything else degrades safely to Delete+Create. Rename is a *classification* for status/changes only: the sync plan compares by path and hash, so a misclassified rename can never lose data, only cost a re-transfer. Tests: `TestDiff_RenameAmbiguousFallsBackToDeleteCreate`, `TestDiff_EmptyFilesNeverRename`, `TestScan_RenameOnDisk`.

Secondary risks worth a note in `docs/ARCHITECTURE.md`: manifests are sent as one frame (64 MiB cap ≈ 200k entries; paging is a protocol v2 concern); one request in flight per connection limits throughput on high-latency links (pipelining via a `req_id` header field is a compatible extension); clock skew between devices is irrelevant to correctness because the plan uses hashes and `synced_hash`, never mtimes.

---

### Critical Files for Implementation

- /home/wesbragagt/dev/syncthing/internal/app/ports/store.go, filesystem.go, transport.go — the port contracts every adapter and use case is written against (M1)
- /home/wesbragagt/dev/syncthing/internal/domain/manifest/diff.go and /home/wesbragagt/dev/syncthing/internal/domain/syncplan/plan.go — the two pure functions that define classification and sync semantics (M1, M6, M9)
- /home/wesbragagt/dev/syncthing/internal/adapters/sqlitestore/migrations/001_init.sql and store.go — the state format and locking strategy (M2)
- /home/wesbragagt/dev/syncthing/internal/adapters/tcpwire/frame.go, handshake.go, client.go, server.go — wire protocol and mutual auth (M5)
- /home/wesbragagt/dev/syncthing/internal/app/syncer/puller.go and installer.go — chunk fetch, journal, resume re-verification, atomic install (M6)
- /home/wesbragagt/dev/syncthing/internal/compose/compose.go and /home/wesbragagt/dev/syncthing/internal/app/daemon/daemon.go — composition root and orchestration; the e2e harness in /home/wesbragagt/dev/syncthing/internal/e2e/harness.go builds on both (M2, M4, M6)