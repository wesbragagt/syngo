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
internal/testutil   test-only helpers shared by test packages, never shipped
internal/e2e        two daemons in one process, talking over loopback
internal/e2edocker  N daemons, one container each; opt-in, only _test.go files
```

| Package prefix | May import |
|---|---|
| `internal/domain/...` | stdlib, except `os`, `net`, `database/sql`, `syscall`, `os/exec` |
| `internal/app/...` | `internal/domain/...`, `internal/app/...`, and the same stdlib subset |
| `internal/adapters/X` | `internal/domain/...`, `internal/app/ports`, third-party packages |
| `internal/cli` | `internal/app/...`, `internal/compose`, `internal/domain/...`, cobra |
| `internal/compose` | everything |
| `cmd/syncthis` | `internal/cli` |
| `internal/testutil/...` | `internal/domain/...`, `internal/adapters/...`; imported only by `_test.go` files |
| `internal/e2edocker` | anything; it is test-only and nothing can import it |

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
| `Binder` | `tcpwire` | turning a listen address and an interface name into a bound socket, and saying whether the machine still has the address |
| `Identity` | `identityfile` | the device key, certificate and derived ID |
| `ControlServer`, `ControlClient` | `unixctl` | the control socket |
| `EventSink` | the daemon, the tests | what happened, as named events |
| `Limiter` | `tokenbucket` | waiting for a byte budget |
| `SettingsStore` | `jsonconfig` | the settings in `config.json` that change while the process runs |
| `TailnetDiscovery` | `tailscalecli` | the local Tailscale client, through `tailscale status --json`: this device, the other nodes, and the path to each of them |

The application never constructs an adapter. `compose.Build` wires them and
returns an `App`; every command asks the `App` for the service it needs.

## Recovering from network changes

A daemon outlives the address it started on. `internal/app/network.Rebinder`
implements `ports.Listener` and owns the socket: it serves through a
`ports.Binder`, checks that the machine still has the address it is bound to,
closes the socket when it does not, and binds again with a bounded backoff.
`tcpwire.Binder` is the only thing that knows what an address is: it re-resolves
the configured interface on every attempt, so a renumbered interface is bound at
its new address, and it keeps the port the first bind ended up on, so the
listener comes back where its peers already look for it. A Tailscale client
restarting, a suspend, and a link that drops and returns are the same event
seen from here.

Two things end a serving cycle and both are spaced by the same backoff: a bind
that failed, and a socket that was bound and then stopped serving. They are
told apart by how long the socket served, not by why it stopped. A socket that
served longer than the settle time was healthy, so whatever ended it is a new
event and it is bound again at once; one that stopped sooner is failing as fast
as it is bound, and the wait is what keeps the loop from spinning.

The reconnect waits are one type, `domain/backoff.Policy`, used by the rebinder
and by the peer loops: doubling from a floor to a ceiling, never zero, never
unbounded. `internal/app/daemon` reports the result in `status --json`, in
`daemon.listener` and in `peers[].state`.

Which path a peer is reached over is a question only the local Tailscale client
can answer, so it is behind `TailnetDiscovery.Paths`. `network.Service.PeerPaths`
matches configured peers to tailnet nodes by address or name, and the daemon
reads it on a timer rather than per request. A node the client describes with
neither an endpoint nor a relay is reported as unknown with the reason; the
path is never inferred from anything else.

## Glossary

The domain language, one page: [GLOSSARY.md](GLOSSARY.md). It defines the words
this document and the code use, from device and folder through manifest, plan
and chunk to the control socket. `manifest.ValidRelPath`, the one statement of
which paths a manifest may name, is defined there under "Valid path".
