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
| `Limiter` | `tokenbucket` | waiting for a byte budget |
| `SettingsStore` | `jsonconfig` | the settings in `config.json` that change while the process runs |
| `TailnetDiscovery` | `tailscalecli` | the local Tailscale client, through `tailscale status --json` |

The application never constructs an adapter. `compose.Build` wires them and
returns an `App`; every command asks the `App` for the service it needs.

## Glossary

The domain language, one page: [GLOSSARY.md](GLOSSARY.md). It defines the words
this document and the code use, from device and folder through manifest, plan
and chunk to the control socket. `manifest.ValidRelPath`, the one statement of
which paths a manifest may name, is defined there under "Valid path".
