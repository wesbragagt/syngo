# syngo

Syngo keeps folders in sync between devices you own. It is a single Go
binary: a CLI and a daemon, with no server in the middle and no account. Two
devices authenticate each other by their ed25519 identity over TLS 1.3, compare
folders by BLAKE3 content hash, and transfer files in chunks with a journal, so
an interrupted download resumes where it stopped.

It is written for a tailnet: point the daemon at `tailscale0` and the devices
reach each other wherever they are.

## Get started

Run these steps on both devices. They need Nix, a checkout of this repository,
and network access to each other. The Nix flake supports x86_64 Linux,
aarch64 Linux and aarch64 macOS.

### 1. Build

```console
$ nix build --extra-experimental-features 'nix-command flakes' .#syngo
$ export PATH="$PWD/result/bin:$PATH"
```

Alternatively, run a command without a `result` link with
`nix run --extra-experimental-features 'nix-command flakes' .#syngo -- version`.

### 2. Start each device

Choose a different name for each device. Save the device ID printed by `init`
and give it to the other device's owner. Keep the daemon running.

```console
$ syngo init --name laptop --listen 0.0.0.0:22000
$ syngo daemon run
```

Open another terminal on each device and add `result/bin` to its `PATH` as in
step 1. Allow TCP port 22000 between the devices. On a tailnet, use each
device's Tailscale address.

### 3. Pair and share a folder

On each device, replace `PEER_ADDRESS` and `PEER_ID` with the other device's
reachable address and the ID it printed. Use the same folder ID on both
devices. The folder paths may differ.

```console
$ syngo peer add PEER_ADDRESS:22000 --device-id PEER_ID
$ mkdir -p "$HOME/syngo-docs"
$ syngo folder add "$HOME/syngo-docs" --id docs
$ syngo folder grant docs PEER_ID
$ syngo status
```

Both devices must pair and grant access before the folder can sync. To join an
existing device, run these steps on the new device, then add and grant the new
device on the existing one. The existing device can use its existing folder.
Put a file in either `syngo-docs` folder and check that it appears on the other.

See [docs/INSTALL.md](docs/INSTALL.md) for service setup and other install
methods. With Go and Make, `make build` writes `bin/syngo`.

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
