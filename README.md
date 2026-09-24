# syngo

Syngo keeps folders in sync between devices you own. It is a single Go
binary: a CLI and a daemon, with no server in the middle and no account. Two
devices authenticate each other by their ed25519 identity over TLS 1.3, compare
folders by BLAKE3 content hash, and transfer files in chunks with a journal, so
an interrupted download resumes where it stopped.

It is written for a tailnet: point the daemon at `tailscale0` and the devices
reach each other wherever they are.

## Build

```console
$ make build      # writes bin/syngo, stamped with git describe
$ bin/syngo version
```

`make test` runs the whole tree; `make e2e` runs the end-to-end tests, which
start two daemons on loopback, with the race detector; `make lint` runs gofmt,
`go vet` and golangci-lint when it is installed.

`make e2e-docker` runs the container suite: every device gets a container, an
address and a filesystem of its own, so a test can kill a daemon, cut a link
between two devices or slow one down, and then check that the folders agree
again. It needs docker, builds the image itself, and is opt-in; `make test`
compiles it and skips it. `make e2e-docker-soak MINUTES=30` keeps a cluster
changing and breaking for half an hour, with rewrites, deletes, renames,
same-path collisions, killed daemons, partitions and shaped links, and `make
e2e-docker-clean` removes what a run that was interrupted left behind.

## Quick start

Two devices on one machine, each with its own data directory and folder.

```console
$ mkdir -p /tmp/a/data /tmp/a/docs /tmp/b/data /tmp/b/docs
$ bin/syngo --data-dir /tmp/a/data init --name laptop --listen 127.0.0.1:22001
$ bin/syngo --data-dir /tmp/b/data init --name nas    --listen 127.0.0.1:22002
$ bin/syngo --data-dir /tmp/a/data folder add /tmp/a/docs --id docs
$ bin/syngo --data-dir /tmp/b/data folder add /tmp/b/docs --id docs

$ bin/syngo --data-dir /tmp/a/data daemon run &
$ bin/syngo --data-dir /tmp/b/data daemon run &
$ sleep 2      # both daemons return before their listener is up

$ bin/syngo --data-dir /tmp/a/data peer add 127.0.0.1:22002 --yes
$ bin/syngo --data-dir /tmp/b/data peer add 127.0.0.1:22001 --yes

$ A=$(bin/syngo --data-dir /tmp/a/data status --json | jq -r .device.id)
$ B=$(bin/syngo --data-dir /tmp/b/data status --json | jq -r .device.id)
$ bin/syngo --data-dir /tmp/a/data folder grant docs "$B"
$ bin/syngo --data-dir /tmp/b/data folder grant docs "$A"

$ echo "hello from A" > /tmp/a/docs/greeting.txt
$ sleep 10; cat /tmp/b/docs/greeting.txt
hello from A
```

Pairing a device and sharing a folder with it are two decisions: a folder is
served to no device until `folder grant` names one, so without the two grants
above the devices connect and move nothing. `peer add` tells the running daemon
to reload, so both sides connect at once; the same happens after `folder add`,
`folder set`, `folder remove` and `folder grant`. Nothing has to be restarted.

That is the whole thing on loopback. The install for a real device, the same
walkthrough with the output each step prints, the Tailscale path and the exit
codes are in [docs/INSTALL.md](docs/INSTALL.md#run-and-verify).

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
- Per-folder sync windows: `folder set docs --sync-window 22:00-06:00` pulls
  the folder at night only, while it keeps scanning and keeps serving.
- Folder-level device authorization: a paired device sees nothing until
  `folder grant` names it for a folder; `folder revoke` and `folder devices`
  are the other two.
- `config export` and `config import` move a whole configuration, folders,
  peers and settings, to another machine; identity keys are never part of it.
- `network tailscale detect` and `network peers` report what the local
  Tailscale client knows, and `peer add nas` takes a MagicDNS host name.
- `sync-plan` prints what a sync would do without doing it.
- Every command speaks `--json`, and every failure has its own exit code.
- `syngo tui` is the same thing with a screen: live folder, device, queue
  and path views, a guided walk from nothing to a paired folder, a conflict
  review screen and a searchable log. It runs the commands above and nothing
  else, and [docs/TUI.md](docs/TUI.md) says which key runs which.

## Documentation

- [docs/INSTALL.md](docs/INSTALL.md) — how to install, configure and run it
  on NixOS, home-manager, Linux, macOS, a NAS and in a container.
- [docs/CLI.md](docs/CLI.md) — every command, its flags, its JSON shape, the
  exit codes and the daemon lifecycle.
- [docs/TUI.md](docs/TUI.md) — the interactive interface, `syngo tui`, and
  the command behind every key it answers to.
- [docs/STATE.md](docs/STATE.md) — the data directory, the database schema and
  the transfer journal.
- [docs/PROTOCOL.md](docs/PROTOCOL.md) — the wire protocol between two daemons.
- [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) — the package layout and the
  dependency rules a test enforces.
- [docs/GLOSSARY.md](docs/GLOSSARY.md) — the domain language: every word the
  other documents and the code use, defined once.
- [docs/BENCH.md](docs/BENCH.md) — the benchmark scenarios and what the runs
  measured.
- [docs/history/](docs/history/) — the implementation plans, milestone by
  milestone. They are a record of intent, not reference: the work is built, and
  the documents above describe what shipped.
