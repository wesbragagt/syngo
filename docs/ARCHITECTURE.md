# Syngo architecture

## Layers

```
cmd/syngo        entry point: calls internal/cli.Execute and exits with its code
internal/domain     pure logic: no I/O, stdlib only
internal/app        use cases, written against ports
internal/app/ports  the interfaces the application needs from the outside world
internal/app/fakes  one in-memory implementation per port, for tests anywhere
internal/adapters   one package per technology, each implementing ports
internal/compose    the composition root: builds an App from a data directory
internal/cli        cobra commands, one file per command
internal/tui        the interactive terminal interface, one file per screen
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
| `internal/cli` | `internal/app/...`, `internal/compose`, `internal/domain/...`, `internal/tui`, cobra |
| `internal/tui` | itself, bubbletea, bubbles, lipgloss, and the same stdlib as any presentation code; nothing else of this module |
| `internal/compose` | everything |
| `cmd/syngo` | `internal/cli` |
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
| `MergeBaseStore` | `sqlitestore` | the recent versions of a text file's content, which a three-way merge runs against |
| `SettingsStore` | `jsonconfig` | the settings in `config.json` that change while the process runs |
| `TailnetDiscovery` | `tailscalecli` | the local Tailscale client, through `tailscale status --json`: this device, the other nodes, and the path to each of them |

The application never constructs an adapter. `compose.Build` wires them and
returns an `App`; every command asks the `App` for the service it needs.

## The terminal interface

`syngo tui` is a subcommand of the one binary, not a second one: `make dist`
builds `./cmd/syngo` alone, the packaging, the container image and the Nix
flake install that single file, and shell completion is generated from this
command tree. `cmd/syngo-bench` is the precedent for a second binary and it
is a development tool that is not shipped.

The interface is a presentation layer and the dependency table above is what
makes that a rule rather than an intention. `internal/tui` may not import
`internal/app`, `internal/domain` or `internal/compose`. It reaches the daemon
through one interface it declares itself, `tui.Runner`, which runs a syngo
invocation and returns what it wrote to stdout and the code it exited with;
`internal/cli` implements it by calling its own `ExecuteContext`. So every
screen is a view of a `--json` document `docs/CLI.md` documents, every key is
one command a user could have typed, and there is no operation the interface
offers that the command line does not. `docs/TUI.md` lists the command behind
each key. The dependency runs `cli -> tui` and never back, which is also what
keeps the two out of an import cycle.

The interface holds no synchronization state: what it shows is the last
`status --json` answer and which screen has the keyboard. Killing it, losing
an SSH session or detaching a tmux window therefore cannot touch the daemon or
a transfer in flight, and a new process is in the same place the old one was.

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

## Chunking and deduplication

Where a file is cut is a rule, not a policy, so it lives in the domain:
`domain/chunker` is FastCDC with normalized chunking, stdlib only, no clock and
no randomness. Two devices deduplicate only while they cut identically, so the
gear table comes from a fixed integer recurrence and the masks from the
configured average, and `TestSplit_GoldenCutPoints` freezes the boundaries of a
known input.

The chunker has one caller on each side of a transfer. The scanner runs it over
every file it reads, in the same pass that hashes the file, and writes the
result to the chunk index (`ports.ChunkStore`). The sender serves that index to
a peer; the receiver looks the peer's chunk hashes up in its own before asking
for anything, and takes what it already holds off its own disk.

The index is a port like any other, so the puller and the scanner never learn
that it is SQLite; `ports.ChunkPlacement` is what the domain sees, a folder, a
path and a byte range. The index is advisory: the puller re-reads and re-hashes
what it points at, so a row that has gone stale since the last scan costs a
read rather than a wrong file.

An index also records the whole-file hash it was cut from, because offsets and
lengths say nothing about which bytes they describe. Two versions of a path that
are the same length produce chunk lists that both partition the file, so a
length check alone would let a cut of the old version be served for the new one.
`transfer.ChunkIndex.Describes` compares the stored hash against the entry
before checking the geometry, and `Server.Chunks` reads the file instead
whenever they disagree.

## The transfer pipeline

A download asks for several chunks of one file at once. Chunks are independent:
each covers a byte range of its own, is verified against the hash the sender
announced before anything is recorded, and is journalled on its own row. So
order is not preserved and does not have to be; completeness is, and the
journal is what states it. A process killed mid-download resumes from the
journal exactly as a sequential one did, because a chunk's row is written after
its bytes and `resume` re-reads and re-hashes every row it finds.

Three numbers bound what this costs, and all three live in `domain/pipeline`,
with no I/O behind them:

- `pipeline.Limiter` is how many chunk requests are outstanding. It compares
  what a chunk costs on the wire against what it costs on this disk, because
  the overlap of the two is the only thing concurrency buys: the limit rises
  when the wire dominates by more than a factor of two, falls when the disk
  does, and stays inside `[Floor, Ceiling]` = `[1, 8]`. The floor is the
  sequential download this replaced, so the pipeline can never be worse than
  what it replaced. `config.json`'s `chunk_pipeline` pins it, which is what a
  constrained device and a measurement ask for.
- `pipeline.Gate` admits holders against that limit, one gate per file. It
  re-reads the limit on every chunk, so a limit that moved while a large file
  was downloading takes effect without restarting anything.
- `pipeline.Budget` is the byte semaphore, and it is the backpressure. A chunk
  is charged against it from before it is asked for until after it is written
  and hashed, so a disk that cannot keep up holds the bytes and the fetchers
  wait rather than asking for more. The default is 64 MiB across every file and
  every folder of the device, which is the memory bound of the download side.

The wire carries this because every frame names the request it answers
(`docs/PROTOCOL.md`, version 4). The serving side answers at most 16 requests
of one peer at a time and checks the peer's folder authorization on each of
them, so a client that pipelines is not a way around the check: there is no
per-connection decision for a second request to inherit.

## Merging two edits of one text file

Two concurrent edits of one file keep both versions. That is the default and
it stays the default. `text_merge` in `config.json` turns on one exception:
when both versions are text, at most a megabyte, and both descend from a
version this device still has the content of, `domain/textmerge` merges them
line by line and the path ends up holding both edits with no conflict.

The algorithm is stdlib only and lives in the domain: Myers' diff of each side
against the base, grouped into hunks over the base's lines. A base region only
one side changed is taken from that side. Two changes with no untouched base
line between them are refused, and so is content with a NUL byte or invalid
UTF-8, a side more than a thousand line edits from the base, and a file that
moved on disk since the plan was made. Every one of those returns an error,
and `syncer.tryMerge` turns any error into the keep-both it replaced: a wrong
automatic merge is worse than a conflict a user resolves by hand.

A merge needs the version both sides came from, and once a file has been
edited over, nothing else on the device holds that content. So with the
setting on, the scanner and the puller keep the last few versions of every
small text file in `ports.MergeBaseStore`, with the version vector each
carried. `mergeBase` picks the newest candidate that both sides dominate
(`manifest.Before` against each), which is what makes it an ancestor of both,
and re-hashes its stored bytes against the hash stored beside them before
using it. The recorder hashed what it wrote, so a row that disagrees with
itself now was changed after it was written, and it is not a version either
side ever had: a base that happens to hold both edits already reads as each
side having reverted the other's, and merging against it succeeds and throws
both edits away. Such a candidate is passed over exactly as an absent one is.
A path with no usable candidate is not merged. With the setting off no recorder
is built, the table stays empty, and the conflict path is the one that was
there before.

Only the side whose version gives up the path merges, the same side keep-both
acted on. The merged file's vector is the merge of both sides bumped by this
device, so it dominates both and the peer pulls it as an ordinary newer
version. It is reported as a `sync.merged` event, never as `sync.conflict`.

## Glossary

The domain language, one page: [GLOSSARY.md](GLOSSARY.md). It defines the words
this document and the code use, from device and folder through manifest, plan
and chunk to the control socket. `manifest.ValidRelPath`, the one statement of
which paths a manifest may name, is defined there under "Valid path".
