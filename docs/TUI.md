# Syngo terminal interface

```console
$ syngo tui
```

`syngo tui` is the interactive interface. It is a subcommand of the one
binary rather than a second one, because syngo ships as a single file: `make
dist` builds `./cmd/syngo` alone, the packaging, the container image and the
Nix flake install that file, and shell completion is generated from this command
tree.

| Flag | Meaning |
|---|---|
| `--poll DUR` | how often the live views ask the daemon what changed; default 1s |
| `--log-file FILE` | a file `daemon run` is writing its log to, for the log view |

The global flags of [CLI.md](CLI.md#global-flags) apply, except `--json`: there
is no JSON form of an interactive interface, and asking for one is a usage
error (exit 2). `--data-dir` selects the device, exactly as it does for every
command.

## Nothing here is TUI-only

The interface holds no synchronization state and contains no logic of its own.
Every screen is a view of a `--json` document [CLI.md](CLI.md) documents, and
every key runs one syngo command. The table below is the whole of it: for
each thing the interface can do, the command that does the same without it.

`internal/arch/deps_test.go` keeps this true rather than merely intended.
`internal/tui` may not import `internal/app`, `internal/domain` or
`internal/compose`; it reaches the daemon through one interface it declares
itself, `tui.Runner`, which runs a syngo invocation and hands back its
stdout and its exit code. There is no path from a key press to the daemon that
is not a command.

### Everywhere

| Key | What it does | Command |
|---|---|---|
| `q`, `ctrl+c` | leave | nothing runs |
| `r` | refresh now | [`syngo status --json`](CLI.md#syngo-status---json) |
| `d` | the dashboard | [`syngo status`](CLI.md#syngo-status---json) |
| `c` | the conflict screen | [`syngo conflict list`](CLI.md#syngo-conflict-list-folder---all) |
| `l` | the log screen | see [Logs](#logs) |

The live views refresh on their own every `--poll`. Losing the terminal,
detaching a tmux window, dropping an SSH session or killing the process cannot
touch the daemon or a transfer in flight; see
[Reattaching](#reattaching-after-ssh-or-a-terminal-disconnect).

### Dashboard

The dashboard is `status --json` every second and `network path --json` every
thirty seconds, laid out in five sections: the daemon and its listener, the
devices, the folders, the transfer queue, the conflict count and everything
that is currently wrong.

| Section | Where every number comes from |
|---|---|
| the daemon line | `status.daemon` and `status.daemon.listener` |
| `DEVICES` | `status.peers[]`, with the path from [`network path --json`](CLI.md#syngo-network-path) when it is fresher than `status.peers[].path` |
| `FOLDERS` | `status.folders[]` |
| `QUEUE` | `status.transfers`, the same objects [`transfer list`](CLI.md#syngo-transfer-list---state-s) prints |
| `CONFLICTS` | `status.conflicts.unresolved` |
| `ERRORS` | `status.daemon.listener.error`, `status.folders[].error`, `status.peers[].error` |

A path is never inferred. A peer neither source can describe is `unknown` with
the reason, which is what both commands already say. A device with no Tailscale
client answers exit 11 to `network path`; that is a supported way to run
syngo and it is not shown as an error.

| Key | What it does | Command |
|---|---|---|
| `↑` `↓` `k` `j` | move the folder cursor | nothing runs |
| `p` | pause the selected folder | [`syngo pause FOLDER`](CLI.md#syngo-pause-folder-and-syngo-resume-folder) |
| `u` | resume it | [`syngo resume FOLDER`](CLI.md#syngo-pause-folder-and-syngo-resume-folder) |
| `s` | rescan it | [`syngo rescan FOLDER`](CLI.md#syngo-rescan-id) |

### No daemon

An initialized device with nothing running gets its own screen rather than a
dashboard of nulls, because everything the dashboard shows comes from the
daemon. It names the command that starts one:
[`syngo daemon run`](CLI.md#syngo-daemon-run). The screen leaves by
itself as soon as a daemon answers.

### Guided setup

A data directory with no identity answers exit 3 to `status`. That is not a
failure, it is the state the guided walk exists for, and the TUI starts in it.
The walk is [INSTALL.md's walkthrough](INSTALL.md#run-and-verify) with the
commands filled in for you; each step shows the command it is about to run.

| Step | What it asks | Command |
|---|---|---|
| 1 | the device name | [`syngo init --name NAME --listen HOST:PORT`](CLI.md#syngo-init---name-name---listen-hostport) |
| 2 | the address peers connect to | the same invocation |
| 3 | the directory to synchronize | [`syngo folder add PATH --id ID`](CLI.md#syngo-folder-add-path---id-id) |
| 4 | the folder id, proposed from the directory name | the same invocation |
| 5 | waits for a daemon | [`syngo daemon run`](CLI.md#syngo-daemon-run), which you start yourself |
| 6 | the address of the other device | [`syngo peer test HOST[:PORT]`](CLI.md#syngo-peer-test-idhostport) |
| 7 | shows the identity that answered, and asks whether to trust it | [`syngo peer add HOST[:PORT] --device-id ID --yes`](CLI.md#syngo-peer-add-hostport) |
| 8 | shares the folder with it, with no question | [`syngo folder grant ID DEVICE-ID`](CLI.md#syngo-folder-grant-id-device-id) |
| 9 | waits until the peer is connected | [`syngo status`](CLI.md#syngo-status---json) |

Three things about the walk are decisions, not conveniences:

- **It does not start a daemon.** `daemon run` stays in the foreground, so a
  daemon this process forked would die with the terminal, which is the opposite
  of what the rest of this page promises. Step 5 waits instead, and says what to
  run in the other terminal.
- **It shows the identity before it trusts it.** Step 6 runs `peer test`, which
  dials and reports what the address presented, and step 7 shows that device id
  and asks. `--yes` is then passed to `peer add` because the question has
  already been answered on screen; `--json` implies `--no-input`, so without it
  the command would exit 9.
- **It grants the folder.** Trusting a device and sharing a folder with it are
  two decisions in syngo, and a folder added without a grant is served to
  nobody. Leaving step 8 out would produce a setup that looks paired and moves
  nothing.

Run the walk on both devices, each pointing at the other, with the same folder
id on both. Pairing is done when both sides have added the other.

### Conflicts

| Key | What it does | Command |
|---|---|---|
| on entry, `R` | list the paths both sides changed | [`syngo conflict list --json`](CLI.md#syngo-conflict-list-folder---all) |
| `↑` `↓` `k` `j` | move the cursor | nothing runs |
| `enter` | open one, with both versions as they are on disk now | [`syngo conflict show ID`](CLI.md#syngo-conflict-show-id) |
| `1` | keep the version this device had | [`syngo conflict resolve ID --keep local --yes`](CLI.md#syngo-conflict-resolve-id---keep-localremoteboth) |
| `2` | keep the version the peer had | `syngo conflict resolve ID --keep remote --yes` |
| `3` | keep both, touching no file | `syngo conflict resolve ID --keep both --yes` |
| `esc` | back | nothing runs |

Those three words are the only resolutions there are and the screen invents no
fourth: what happens to each file is the application's decision, not the
interface's.

`1` and `2` remove a file, so the screen asks first and names the command it
would run. `3` deletes nothing and asks nothing, which is the rule
`conflict resolve` already applies. A conflict `conflict list` marks `stale` is
one where neither version is on disk unchanged any more; the screen says so, and
says that keeping one version is refused with exit 8 and that keeping both is
what closes it.

### Logs

The log view scrolls and filters as you type. `/` opens the search, which is a
case-insensitive substring over the level and the text; `x` clears it; `esc`
leaves the search, and a second `esc` leaves the screen. The window is the last
2000 lines.

There are two sources and they are marked apart, because they are different
things.

| Source | What it is | Reproduce it with |
|---|---|---|
| `seen` | what this process saw: every difference between one `status --json` and the next, as folder states, peer states, transfer progress, listener rebinds, conflicts and errors | `while :; do syngo status --json; sleep 1; done` and compare successive documents |
| the daemon's own levels (`info`, `warn`, `error`) | the daemon's structured log, tailed from `--log-file` | `tail -f FILE`, or `jq` over it |

The second source needs setting up, because a daemon writes its log to its own
stderr and no other process can read that:

```console
$ syngo daemon run --log-format json 2>> ~/.local/share/syngo/daemon.log &
$ syngo tui --log-file ~/.local/share/syngo/daemon.log
```

A line in `--log-format json` is shown with its fields in a stable order; a line
in the default text format is shown exactly as it stands, rather than reparsed
by a second definition of a format the daemon owns.

There is no event stream on the control socket to subscribe to. Adding one would
be a change to the daemon's API, which is an application decision and not one a
presentation layer gets to make, so the interface does not pretend to have one:
it says which of the two sources each line came from.

## Reattaching after SSH or a terminal disconnect

The TUI process is not load-bearing. It opens no connection it keeps, holds no
lock, owns no transfer, and writes nothing to the data directory: every screen
is a command that runs, answers and is over, and what is on screen is the last
answer plus which screen has the keyboard.

So all of these are safe, and none of them touches the daemon or a transfer in
flight:

- closing the terminal, or losing the SSH session it ran over;
- detaching the tmux or screen window it is in, and reattaching later;
- killing the TUI process, and starting a new one.

A new TUI started against the same data directory is in the same place the old
one was, after its first refresh. Two of them may run against one device at
once; each is a reader.

`internal/e2e/tui_reconnect_test.go` is that claim as a test: it starts a
transfer between two daemons, kills the TUI with SIGKILL, checks the daemon is
still the same process and that the transfer finished, and then starts a fresh
TUI and checks it shows the same device, the same folder and the same peer.

Two practical notes:

- The daemon has to outlive the terminal too. Start it under a service manager,
  in its own tmux window, or with `setsid`/`nohup`; a `daemon run &` in the shell
  that the SSH session owns is killed with the session, and that is the shell's
  doing and not the TUI's.
- A TUI killed with SIGKILL cannot restore the terminal, because nothing can
  run on the way out of a SIGKILL. The interface draws on the alternate screen,
  so run `reset` if a terminal is left in it. Quitting with `q` or `ctrl+c`,
  and losing the terminal to a hangup, all restore it.

## Which parts of this the tests cover

| Claim | Test |
|---|---|
| every screen's `Update` and `View`, with no terminal | `internal/tui/*_test.go` |
| the published `--json` shapes decode into what the screens read | `internal/tui/contract_test.go` |
| the guided walk reaches a paired, synchronized folder against real daemons | `internal/e2e/tui_guided_test.go` |
| killing the TUI leaves the daemon and its transfer alone, and a new one reattaches | `internal/e2e/tui_reconnect_test.go` |
| the interface imports nothing of the application | `internal/arch/deps_test.go` |
