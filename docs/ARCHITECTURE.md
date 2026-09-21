# Syncthis architecture

## Layers

```
cmd/syncthis        entry point: calls internal/cli.Execute and exits with its code
internal/domain     pure logic: no I/O, stdlib only
internal/app        use cases, written against ports
internal/app/ports  the interfaces the application needs from the outside world
internal/app/fakes  one in-memory implementation per port, for tests anywhere
internal/adapters   one package per technology, each implementing ports
internal/compose    the composition root: builds an App from a data directory
internal/cli        cobra commands, one file per command
internal/arch       the test that enforces the table below
internal/e2e        two daemons in one process, talking over loopback
```

| Package prefix | May import |
|---|---|
| `internal/domain/...` | stdlib, except `os`, `net`, `database/sql`, `syscall`, `os/exec` |
| `internal/app/...` | `internal/domain/...`, `internal/app/...`, and the same stdlib subset |
| `internal/adapters/X` | `internal/domain/...`, `internal/app/ports`, third-party packages |
| `internal/cli` | `internal/app/...`, `internal/compose`, `internal/domain/...`, cobra |
| `internal/compose` | everything |
| `cmd/syncthis` | `internal/cli` |

Adapters do not import each other. `tcpwire` takes a `*tls.Certificate` rather
than reaching for `identityfile`, and `compose` is what hands it over.
`internal/arch/deps_test.go` parses the import list of every file and fails the
build when a rule is broken.

## Ports

| Port | Adapter | What it hides |
|---|---|---|
| `Clock` | `sysclock` | the wall clock |
| `Hasher` | `blake3hash` | BLAKE3-256 over a reader or a byte slice |
| `FileSystem` | `localfs` | reading, walking, writing and flushing files; which calls follow a symbolic link is the operating system's answer, listed in the package doc |
| `MetadataStore` | `sqlitestore` | the database, as folders, files, devices, transfers and conflicts |
| `Watcher` | `fsnotifywatch` | filesystem change notifications |
| `Dialer`, `Listener`, `PeerConn` | `tcpwire` | the peer protocol |
| `Identity` | `identityfile` | the device key, certificate and derived ID |
| `ControlServer`, `ControlClient` | `unixctl` | the control socket |
| `EventSink` | the daemon, the tests | what happened, as named events |

The application never constructs an adapter. `compose.Build` wires them and
returns an `App`; every command asks the `App` for the service it needs.

## Glossary

**Device ID.** 52 base32 characters, the SHA-256 of the device certificate's
SubjectPublicKeyInfo. It is how a peer is named everywhere.

**Folder.** A directory plus an id shared by every device that synchronizes it,
a mode, a delete policy and its ignore patterns.

**Manifest.** Everything a device knows about one folder: one entry per path,
tombstones included, each with a content hash and a per-folder sequence number.

**Entry kind.** File, directory or symbolic link.

**Valid path.** `manifest.ValidRelPath` is the one statement of which paths a
manifest may name: relative, slash-separated, no `.` or `..` or empty segment,
no NUL byte, and not the reserved `.syncthis` or `.syncthisignore`. It is pure
domain code because three layers apply it to the same peer input: the wire
refuses such a manifest, `syncplan.Plan` skips the entry with reason
`invalid-path`, and `syncer.Installer` refuses the call. `ValidLinkTarget` does
the same for where a stored symbolic link may point. The installer adds what
only a filesystem can answer: it refuses to write through a parent that is a
symbolic link.

**Tombstone.** An entry marked deleted. It stays in the manifest so a peer can
tell "deleted" from "never had it".

**Synced hash.** The content hash two devices last agreed on for a path. A file
whose hash still equals it has not been touched locally since the last sync;
one whose hash moved away from it is a local change. It travels with the
manifest, so a peer can tell a version written on top of its own from one that
diverged from it. **Synced mode** is the same idea for the permission bits, and
stays local.

**Sequence.** A per-folder counter raised on every write, which is how a peer
decides whether it has already seen a version.

**Plan.** What `syncplan.Plan` returns for a folder: one action per path the
peer has. The actions are listed in [CLI.md](CLI.md#syncthis-sync-plan-folder---peer-id).

**Conflict.** A path two devices changed independently. Neither version is
overwritten: the greater content hash keeps the path, the other version is
written beside it as a **conflict copy** named
`<stem>.sync-conflict-<date>-<time>-<peer short id><ext>`, and the copy syncs
like any other file, so both devices end up with both versions. `conflict
resolve` is what closes one.

**Transfer.** One inbound file download, with a journal row per chunk. The
sender keeps no state.

**Chunk.** A fixed-size slice of a file, 1 MiB by default, verified against the
hash the sender announced before it is written.
