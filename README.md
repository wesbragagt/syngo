# syncthis

Syncthis keeps folders in sync between devices you own. It is a single Go
binary: a CLI and a daemon, with no server in the middle and no account. Two
devices authenticate each other by their ed25519 identity over TLS 1.3, compare
folders by BLAKE3 content hash, and transfer files in chunks with a journal, so
an interrupted download resumes where it stopped.

It is written for a tailnet: point the daemon at `tailscale0` and the devices
reach each other wherever they are.

## Build

```console
$ make build      # writes bin/syncthis, stamped with git describe
$ bin/syncthis version
```

`make test` runs the whole tree; `make e2e` runs the end-to-end tests, which
start two daemons on loopback, with the race detector; `make lint` runs gofmt,
`go vet` and golangci-lint when it is installed.

## Quick start

Two devices on one machine, each with its own data directory and folder.

```console
$ mkdir -p /tmp/a/data /tmp/a/docs /tmp/b/data /tmp/b/docs
$ bin/syncthis --data-dir /tmp/a/data init --name laptop --listen 127.0.0.1:22001
$ bin/syncthis --data-dir /tmp/b/data init --name nas    --listen 127.0.0.1:22002
$ bin/syncthis --data-dir /tmp/a/data folder add /tmp/a/docs --id docs
$ bin/syncthis --data-dir /tmp/b/data folder add /tmp/b/docs --id docs

$ bin/syncthis --data-dir /tmp/a/data daemon run &
$ bin/syncthis --data-dir /tmp/b/data daemon run &

$ bin/syncthis --data-dir /tmp/a/data peer add 127.0.0.1:22002 --yes
$ bin/syncthis --data-dir /tmp/b/data peer add 127.0.0.1:22001 --yes
```

`peer add` tells the running daemon to reload, so both sides connect at once;
the same happens after `folder add`, `folder set` and `folder remove`. Nothing
has to be restarted. Write a file on one side and watch it appear on the other:

```console
$ echo "hello from A" > /tmp/a/docs/greeting.txt
$ bin/syncthis --data-dir /tmp/b/data status
```

## What it does

- One folder id per synchronized directory, the same on every device.
- Modes: `sendreceive`, `sendonly` and `receiveonly`, so a backup target can
  take changes without ever sending its own.
- Delete policies: follow a peer's deletion, or never delete.
- Gitignore-style ignore patterns, hidden files off by default, symbolic links
  off by default.
- Comparison by content hash, never by timestamp. A change made on both sides
  is never overwritten: one version keeps the path, the other is written beside
  it as `report.sync-conflict-20260921-153000-ETI3RRZ.md`, and both devices end
  up with both files.
- `conflict list`, `show`, `resolve --keep local|remote|both` and `export` to
  see what diverged and decide what to keep.
- `sync-plan` prints what a sync would do without doing it.
- Every command speaks `--json`, and every failure has its own exit code.

## Documentation

- [docs/CLI.md](docs/CLI.md) — every command, its flags, its JSON shape, the
  exit codes, the daemon lifecycle and a pairing walkthrough over Tailscale.
- [docs/STATE.md](docs/STATE.md) — the data directory, the database schema and
  the transfer journal.
- [docs/PROTOCOL.md](docs/PROTOCOL.md) — the wire protocol between two daemons.
- [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) — the package layout and the
  dependency rules a test enforces.
- [docs/GLOSSARY.md](docs/GLOSSARY.md) — the domain language: every word the
  other documents and the code use, defined once.
- [docs/PLAN.md](docs/PLAN.md) — the implementation plan, milestone by
  milestone.
