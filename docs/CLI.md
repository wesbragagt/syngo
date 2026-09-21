# Syncthis command line

Every command takes the global flags below, writes its result to stdout and its
diagnostics to stderr, and exits with a code from the table in
[Exit codes](#exit-codes). With `--json` the result is one JSON document on
stdout and nothing else.

## Global flags

| Flag | Meaning |
|---|---|
| `--data-dir DIR` | where the device keeps its identity and database; see [Data directory](#data-directory) |
| `--json` | print one JSON document instead of a table, and report failures as an error object |
| `--no-input` | never ask a question; a command that needs one exits 9 |
| `--yes` | answer every question with yes |
| `--log-level debug\|info\|warn\|error` | how much the daemon logs; default `info` |
| `--log-format text\|json` | log line format on stderr; default `text` |

`--json` implies `--no-input`: a command that would have asked exits 9 instead,
so nothing ever waits for a terminal that is not there. The JSON examples below
show the shape of each document; the commands print them indented.

### Data directory

`--data-dir`, else `$SYNCTHIS_DATA_DIR`, else `$XDG_DATA_HOME/syncthis`, else
`~/.local/share/syncthis`. Its contents are described in
[docs/STATE.md](STATE.md).

## Exit codes

| Code | Name | Meaning |
|---|---|---|
| 0 | ok | the command did what it was asked |
| 1 | error | an unexpected failure |
| 2 | usage | bad flags or arguments |
| 3 | not initialized | the data directory has no identity; run `syncthis init` |
| 4 | not found | the folder, peer, transfer or conflict does not exist, or the conflict was already resolved |
| 5 | already exists | the folder id, folder path or peer is already configured |
| 6 | daemon unavailable | the command needs the daemon and none is running, or one already owns the data directory |
| 7 | peer unreachable | the peer could not be dialled, refused the handshake or presented another identity |
| 8 | integrity | content did not hash to what was expected, repair found something it cannot fix, a conflict version asked for is no longer on disk, or a resolution would have deleted a file that changed since the conflict was recorded |
| 9 | confirmation required | a question had to be asked and could not be |
| 10 | dry run | a dry run finished and something would have changed; 0 when nothing would |

In `--json` mode a failure is printed on stdout as

```json
{
  "error": {
    "code": 4,
    "message": "folder \"nope\": not found"
  }
}
```

and nothing else is written there.

## Commands

### `syncthis init [--name NAME] [--listen HOST:PORT]`

Creates the data directory, generates the ed25519 identity, writes
`config.json` and creates the database. Running it again changes nothing and
prints the identity that is already there.

```console
$ syncthis init --name laptop --json
{
  "device_id": "ZFBP44XEKUVRBOA2CSBDZ7QAUEW5DBCGZHBLOSB5C4XWX5UV35JQ",
  "name": "laptop",
  "data_dir": "/home/u/.local/share/syncthis",
  "listen": "127.0.0.1:22000",
  "created": true
}
```

`--name` defaults to the hostname, `--listen` to `127.0.0.1:22000`.

### `syncthis folder add PATH --id ID`

Starts synchronizing a directory.

| Flag | Default | Meaning |
|---|---|---|
| `--id ID` | required | folder id, `[a-z0-9][a-z0-9._-]{0,63}`; both devices must use the same one |
| `--label L` | empty | a name for humans |
| `--mode sendreceive\|sendonly\|receiveonly` | `sendreceive` | `sendonly` never installs what a peer offers, `receiveonly` never serves what it has |
| `--delete-policy propagate\|never` | `propagate` | whether a peer's deletion removes the local file |
| `--symlinks skip\|store` | `skip` | whether symbolic links are synchronized |
| `--include-hidden` | off | synchronize dot files |
| `--no-empty-dirs` | off | do not keep directories that hold nothing |
| `--ignore PATTERN` | none | gitignore-style pattern; repeatable |

The ignore rules are the patterns given here plus the lines of
`<PATH>/.syncthisignore`, and they decide both what is scanned and what is
installed: a path this device ignores is not pulled from a peer either, so an
ignored file cannot arrive, be recorded, and be read as deleted by the next
scan. `.syncthis` and `.syncthisignore` are always ignored and are never
accepted from a peer.

```console
$ syncthis folder add ~/docs --id docs --json
{
  "id": "docs",
  "path": "/home/u/docs",
  "label": "",
  "mode": "sendreceive",
  "delete_policy": "propagate",
  "symlinks": "skip",
  "include_hidden": false,
  "keep_empty_dirs": true,
  "ignore_patterns": [],
  "paused": false
}
```

Exits 5 when the id or the path is already configured, and 2 when the path is
not an existing directory or a flag value is not one of the listed words.

### `syncthis folder list`

Prints the columns `ID`, `PATH`, `LABEL`, `MODE`, `DELETE POLICY`, `PAUSED`.
With `--json` it prints an array of the object `folder add` returns.

### `syncthis folder set ID [--mode M] [--delete-policy P]`

Changes the mode or the delete policy of a configured folder and prints the
folder in the shape `folder add` uses. At least one of the two flags is
required, otherwise the command exits 2. A running daemon is asked to reload,
so it applies the new setting to its connected peers right away; see
[Lifecycle](#lifecycle).

```console
$ syncthis folder set docs --mode sendonly
folder docs: mode sendonly, delete policy propagate
```

### `syncthis folder remove ID [--yes]`

Stops synchronizing a folder. The files stay on disk; only the configuration
and the database rows go. It asks first, so it needs `--yes` when there is no
terminal.

```console
$ syncthis folder remove docs --json --yes
{"removed": "docs"}
```

### `syncthis scan ID [--dry-run]`

Walks the folder, hashes what changed and records it. `--dry-run` classifies
without writing anything and exits 10 when there is something to write.

```console
$ syncthis scan docs --json
{
  "folder_id": "docs",
  "changes": [
    {
      "type": "create",
      "path": "a.txt",
      "old_path": "",
      "hash": "8e4c7c1b99dbfd50e7a95185fead5ee1448fa904a2fdd778eaf5f2dbfd629a99"
    }
  ],
  "summary": {
    "scanned": 1, "hashed": 1, "bytes": 6, "ignored": 0, "skipped": 0,
    "changes": 1, "duration_ms": 0, "dry_run": false
  },
  "warnings": []
}
```

`type` is `create`, `modify`, `delete` or `rename`; a rename carries the old
path. An entry the scan cannot read is counted in `skipped`, named in
`warnings` and does not fail the folder.

### `syncthis rescan [ID]`

Rescans one folder, or every folder that is not paused. With a daemon running
the scan is queued in the daemon, so two processes never write the same folder
at once, and the output is

```json
{"queued": ["docs"]}
```

Without a daemon the scan runs in the command and prints an array of the `scan`
document.

### `syncthis repair [ID] [--dry-run]`

Restores agreement between the database and the files on disk: paths the
database knows and the disk does not become tombstones, files whose size or
modification time moved are rehashed, transfers whose partial file is gone are
reset, partial files no transfer claims are deleted, and the folder sequence is
corrected. It runs inside the daemon when one is running.

A folder whose root does not exist, or is no longer a directory, stops the
command instead: every entry would look missing and the folder would be marked
deleted for its peers. Mount the directory again, or remove the folder.

```console
$ syncthis repair docs --json
[
  {
    "folder_id": "docs",
    "dry_run": false,
    "missing_deleted": [],
    "rehashed": [],
    "transfers_reset": [],
    "orphan_temps": [],
    "sequence_fixed": false,
    "unrecoverable": []
  }
]
```

Exits 10 after a dry run that found something to fix, and 8 when a path is
listed in `unrecoverable`, which is a path repair could neither read nor
classify. It asks no question: what it writes follows what is already on disk.

### `syncthis status [--json]`

Reports the device, the daemon, every folder, every peer and the transfer
journal. With a daemon running the report comes from the daemon and knows which
folders are scanning and whether each peer is connected; without one it is read
from the database and `daemon.running` is `false`.

```json
{
  "schema_version": 1,
  "device": {"id": "RCUFGP7…", "name": "laptop", "data_dir": "/home/u/.local/share/syncthis"},
  "daemon": {"running": true, "pid": 3776214, "listen": "127.0.0.1:22001",
             "started_at": "2026-09-21T08:32:15Z"},
  "folders": [
    {"id": "docs", "path": "/home/u/docs", "label": "", "mode": "sendreceive",
     "delete_policy": "propagate", "paused": false, "state": "idle",
     "files": 1, "dirs": 0, "symlinks": 0, "bytes": 13,
     "last_scan_at": "2026-09-21T08:32:15Z", "pending_changes": 0, "error": null}
  ],
  "peers": [
    {"id": "ETI3RRZ…", "name": "nas", "address": "127.0.0.1:22002", "verified": true,
     "connected": true, "last_seen_at": "2026-09-21T08:32:15Z", "latency_ms": 0.022,
     "error": null}
  ],
  "transfers": {"active": 0, "pending": 0, "paused": 0, "failed": 0, "items": []},
  "conflicts": {"unresolved": 0}
}
```

`conflicts.unresolved` counts the paths both sides changed that nobody has
decided about yet; `conflict list` shows them. `folders[].state` is `idle`,
`scanning`, `syncing`, `paused` or `error`. Times
are RFC3339 in UTC, or `null`. `transfers.items` holds the same objects as
`transfer list`. Fields are added over time but never renamed or removed, and
`schema_version` says which shape this is.

### `syncthis daemon run`

Watches every configured folder, rescans them periodically, serves peers and
answers the control socket until it is interrupted with SIGINT or SIGTERM.

| Flag | Meaning |
|---|---|
| `--listen HOST:PORT` | address peers connect to; default from `config.json` |
| `--interface NAME` | bind the first IPv4 address of a network interface, replacing the host part of `--listen` |
| `--allow-public` | allow binding an address reachable beyond this machine and its private network |
| `--rescan-interval DUR` | how often every folder is fully rescanned; default from `config.json` |

Without `--allow-public` the resolved address must be loopback, link-local,
[RFC 1918](https://www.rfc-editor.org/rfc/rfc1918) private, or in the
Tailscale range `100.64.0.0/10`. A wildcard bind such as `0.0.0.0:22000`
counts as public and is refused with exit 2. On a tailnet this is what you
want:

```console
$ syncthis daemon run --interface tailscale0
```

The daemon then listens on the device's `100.64.0.0/10` address, which only
the tailnet can reach, and `--allow-public` is not needed. Put `"interface":
"tailscale0"` in `config.json` to make it the default.

Exactly one daemon owns a data directory. A second one exits 6. It prints one
line when it is serving, or with `--json` one document:

```json
{"running": true, "pid": 3776214, "data_dir": "/home/u/.local/share/syncthis"}
```

With `--log-format json` every log line on stderr is a JSON object with at
least `time`, `level` and `msg`, plus the fields the event carries (`folder`,
`peer`, `path`, `transfer` and so on). Paths are only logged at `debug` level.

#### Lifecycle

1. `daemon run` binds `<data-dir>/control.sock`. A socket left behind by a dead
   daemon is replaced; one a live daemon still answers means exit 6.
2. It writes `<data-dir>/daemon.pid`. A pid file naming a live process means
   exit 6; a stale one is overwritten.
3. It binds the peer port, starts one loop per folder and one per known peer,
   and closes the readiness channel the banner waits on.
4. On SIGINT or SIGTERM it stops the loops, closes the control socket and the
   listener, and removes the pid file and the socket before it returns.

The folder and device tables are read when the daemon starts and again at every
rescan interval. `folder add`, `folder set`, `folder remove`, `peer add` and
`peer remove` do not wait for that tick: each asks the running daemon to reload
over the control socket, which starts and stops folder loops, dials peers that
were added, drops peers that were removed together with the connections those
peers had opened, announces the new folder list to the peers that are already
connected, and runs a sync cycle against each of them. Without a daemon the commands say nothing
and only write the database. The daemon never has to be restarted for a
configuration change.

### `syncthis peer add HOST[:PORT]`

Contacts the device at the address, shows the identity it presents and stores
it. The port defaults to 22000.

| Flag | Meaning |
|---|---|
| `--device-id ID` | the identity the peer must present; the command fails with exit 7 if it presents another |
| `--name N` | the name to store; default is the name the peer announces |
| `--verify` | ask for confirmation even when `--device-id` pins the identity |

```console
$ syncthis peer add 100.64.0.2:22000 --json --yes
{
  "id": "ETI3RRZCMWYFD54X22RLMR5SYEWJTA3UANTRKYNN3KAAX5ZGPAFA",
  "name": "nas",
  "address": "100.64.0.2:22000",
  "verified": true,
  "added_at": "2026-09-21T08:28:28Z",
  "last_seen_at": null,
  "accepted": false
}
```

`accepted` says whether the peer already trusts this device. Pairing is done
when both sides ran `peer add` and both report `accepted: true`.

### `syncthis peer list`

Prints the columns `PEER`, `NAME`, `ADDRESS`, `VERIFIED`, `LAST SEEN`, or with
`--json` an array of the object above without `accepted`.

### `syncthis peer remove ID [--yes]`

Forgets a peer. It asks first.

### `syncthis peer test ID|HOST[:PORT]`

Runs the handshake and reports what happened. It exits 7 when the peer cannot
be reached or refuses this device.

```console
$ syncthis peer test 100.64.0.2:22000 --json
{
  "address": "100.64.0.2:22000",
  "device_id": "ETI3RRZCMWYFD54X22RLMR5SYEWJTA3UANTRKYNN3KAAX5ZGPAFA",
  "name": "nas",
  "tls_ok": true,
  "authenticated": true,
  "reject_reason": "",
  "reason": "",
  "latency_ms": 0.048,
  "error": null
}
```

`reject_reason` is what the peer answered: `unknown_device`,
`protocol_version` or `id_mismatch`.

### `syncthis pause [FOLDER]` and `syncthis resume [FOLDER]`

Stops or resumes scanning and syncing, for one folder or for all of them. A
running daemon applies it at once, including to a transfer in flight, which
stops within one chunk and keeps its journal. Without a daemon the flag is
written to the database.

```console
$ syncthis pause docs --json
{"folders": ["docs"], "paused": true}
```

A paused transfer stays in the journal in state `paused` and is picked up again
by the first sync cycle after `resume`. `paused` means the folder is paused and
nothing else: on `resume` the transfer becomes `pending`, and a folder that
never pulls, because it is send-only, leaves it `pending` with `folder is
send-only` as its error.

### `syncthis transfer list [--state S]`

Shows the journal of inbound downloads. A transfer leaves it once its file is
installed, so what is left is running, paused or failed. `--state` takes one of
`pending`, `active`, `paused`, `verifying`, `done`, `failed`.

Columns: `ID`, `FOLDER`, `PATH`, `STATE`, `CHUNKS`, `DONE`, `SIZE`, `PEER`.

```console
$ syncthis transfer list --json
[
  {
    "id": "a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6",
    "folder_id": "docs",
    "path": "big.bin",
    "peer_id": "ETI3RRZCMWYFD54X22RLMR5SYEWJTA3UANTRKYNN3KAAX5ZGPAFA",
    "direction": "in",
    "state": "active",
    "size": 4194304,
    "chunk_size": 1048576,
    "chunks_total": 4,
    "chunks_done": 2,
    "bytes_done": 2097152,
    "created_at": "2026-09-21T12:00:00Z",
    "updated_at": "2026-09-21T12:00:03Z",
    "error": null
  }
]
```

### `syncthis transfer show TRANSFER_ID`

The same object plus the chunks the transfer still needs.

```json
{
  "id": "a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6",
  "folder_id": "docs",
  "path": "big.bin",
  "state": "active",
  "chunks_total": 4,
  "chunks_done": 2,
  "chunks_missing": 2,
  "missing_chunks": [2, 3]
}
```

`chunks_missing` is the full count. `missing_chunks` lists at most the first
100 indexes; when there are more, `missing_chunks_truncated` is `true` and the
field is absent otherwise. The text output names the first ten. Exits 4 when no
transfer has that id.

### `syncthis sync-plan FOLDER [--peer ID]`

Asks every known peer, or the one named by `--peer`, for its manifest and
prints what this device would do with it. Nothing is written and nothing is
downloaded. It exits 10 when the folder would change and 0 when it is already
in sync, which makes it the dry run of a destructive sync. It does not need a
running daemon; it opens the database alongside the daemon, which is safe
because the database is in WAL mode with a five second busy timeout.

```console
$ syncthis sync-plan docs --json
{
  "folder_id": "docs",
  "dry_run": true,
  "changes": 1,
  "peers": [
    {
      "peer_id": "ETI3RRZCMWYFD54X22RLMR5SYEWJTA3UANTRKYNN3KAAX5ZGPAFA",
      "name": "nas",
      "address": "127.0.0.1:22002",
      "note": null,
      "changes": 1,
      "items": [
        {"action": "pull", "path": "a.txt", "reason": "missing-locally"}
      ]
    }
  ]
}
```

`note` explains a peer that produced no plan, for example one that could not be
reached. `action` is one of

| Action | Meaning |
|---|---|
| `pull` | download the peer's version of a file |
| `mkdir` | create a directory the peer has |
| `symlink` | create a symbolic link the peer has |
| `chmod` | set the permission bits the peer has; no content moves |
| `replace` | the path is a different kind on each side; the local one goes and the peer's kind is created |
| `delete` | remove a path the peer deleted |
| `conflict` | both sides changed; nothing is touched |
| `none` | nothing to do, with `reason` saying why |

`reason` is one of `in-sync`, `missing-locally`, `remote-newer`, `local-newer`,
`both-modified`, `delete-vs-modify`, `delete-policy-never`,
`symlink-policy-skip`, `remote-deleted`, `kind-changed`, `replace-vs-modify`,
`mode-changed`, `mode-baseline-unknown` or `invalid-path`.

`invalid-path` is a path a peer may not name at all, such as one that climbs
out of the folder or names the state directory, or a symbolic link whose target
leaves the folder. Such an entry is skipped rather than acted on. A path the
folder's ignore rules cover does not appear in the plan at all.

### `syncthis conflict list [FOLDER] [--all]`

Lists the paths both sides changed. Unresolved ones only, unless `--all` adds
the ones already dealt with. Without FOLDER it lists every folder.

Columns: `ID`, `FOLDER`, `PATH`, `REASON`, `COPY`, `PEER`, `DETECTED`,
`RESOLVED`, `STALE`.

```console
$ syncthis conflict list --json
[
  {
    "id": "6f1c2f3a4b5c6d7e8f9a0b1c2d3e4f5a",
    "folder_id": "docs",
    "path": "notes/report.md",
    "conflict_path": "notes/report.sync-conflict-20260921-153000-ETI3RRZ.md",
    "reason": "both-modified",
    "local_hash": "3f0a…",
    "remote_hash": "9b12…",
    "peer_id": "ETI3RRZCMWYFD54X22RLMR5SYEWJTA3UANTRKYNN3KAAX5ZGPAFA",
    "detected_at": "2026-09-21T15:30:00Z",
    "resolved": false,
    "stale": false
  }
]
```

`reason` is why the two versions could not be reconciled:

| Reason | Means |
|---|---|
| `both-modified` | both sides changed the file; both versions are on disk |
| `delete-vs-modify` | the peer deleted a file this device changed; the file is kept |
| `replace-vs-modify` | the peer turned a file this device changed into a directory or a symlink |

`local_hash` is the version this device had, `remote_hash` the one the peer
had; `remote_hash` is empty when the peer deleted the path. `conflict_path` is
empty when nothing had to be moved aside.

`stale` is `true` when neither version can be kept any more, because what is on
disk moved on: a later round of edits changed the path, or the copy was merged
by hand. Both files are hashed to answer it, so a listing says it without being
asked to describe them. A stale conflict is closed with `--keep both`, after
looking at the files; `--keep local` and `--keep remote` refuse it with exit 8.
Nothing resolves it by itself, because a file holding content nobody decided
about is not something a sync tool may throw away.

#### Conflict copies

When both sides change a file, nothing is overwritten: one version stays at the
path and the other is written beside it as

```
<stem>.sync-conflict-<YYYYMMDD>-<HHMMSS>-<peer short id><ext>
```

for example `report.sync-conflict-20260921-153000-ETI3RRZ.md`. The time is UTC,
the peer short id is the first seven characters of the device id the conflict
was detected with, and the extension is kept so the copy opens in the same
application. A name that is already taken gets a counter before the extension
(`…-ETI3RRZ-1.md`).

Which version keeps the path is decided by the content, not by a clock: the
greater content hash wins. Both devices reach that answer from the same two
hashes, so only the side whose version loses writes a copy, and that copy then
syncs like any other file. Both devices end up with the same two files.

### `syncthis conflict show ID`

Prints the conflict and both versions as they are on disk now: the path each
one holds, its size, its modification time and its content hash. Exits 4 when
no conflict has that id.

```console
$ syncthis conflict show 6f1c2f3a4b5c6d7e8f9a0b1c2d3e4f5a
id:       6f1c2f3a4b5c6d7e8f9a0b1c2d3e4f5a
folder:   docs (/home/u/docs)
path:     notes/report.md
copy:     notes/report.sync-conflict-20260921-153000-ETI3RRZ.md
reason:   both-modified
peer:     ETI3RRZCMWYFD54X22RLMR5SYEWJTA3UANTRKYNN3KAAX5ZGPAFA
detected: 2026-09-21T15:30:00Z
resolved: no
stale:    no

local version: 3f0a…
  file:  notes/report.sync-conflict-20260921-153000-ETI3RRZ.md
  size:  1841 bytes
  mtime: 2026-09-21T15:29:41Z

remote version: 9b12…
  file:  notes/report.md
  size:  1902 bytes
  mtime: 2026-09-21T15:29:58Z
```

Which file holds which version is answered by hashing them, not by their
names, so a version is found wherever it sits.

`stale: yes` says that neither version is on disk in a state that could be
kept, which is what `--keep both` is for.

With `--json` the same object as `conflict list` carries a `file` object for
what is at `path` and a `copy` object for what is at `conflict_path`, each with
`path`, `size`, `mtime`, `hash` and the `version` it holds (`local`, `remote`,
or `unknown` when something edited it since). Either is absent when there is no
file there any more.

### `syncthis conflict resolve ID --keep local|remote|both`

Says which version wins. `local` keeps the version this device had, `remote`
the version the peer had, whichever of the two files holds it now; the other
file is removed. `both` changes nothing on disk and only closes the conflict.
Keeping the peer's version of a `delete-vs-modify` conflict means following the
deletion, so the local file goes.

The version that wins is put in place with a rename, so the path never holds
half a file. A version that is no longer on disk, because something edited both
files since, exits 8 and changes nothing.

A resolution only ever deletes or overwrites a file that still holds one of the
two versions the conflict recorded. A file holding anything else was never
decided about, so the resolution exits 8, names that file and changes nothing:

```console
$ syncthis conflict resolve 6f1c2f3a4b5c6d7e8f9a0b1c2d3e4f5a --keep local --yes
syncthis: notes/report.md has changed since the conflict was recorded: keep both versions with --keep both, or resolve again after looking at the file
$ echo $?
8
```

That is what a conflict `conflict list` marks `stale` looks like from the
resolve side. `--keep both` closes it without touching a file.

A conflict that was already resolved is not resolved again: it exits 4 with
`conflict "<id>" is already resolved`, because the files it names may belong to
a later conflict by now.

Removing a file is confirmed like every other destructive command, so `--yes`,
`--no-input` and `--json` apply; `--keep both` deletes nothing and asks
nothing. A resolution that changed the
folder rescans it, through the daemon when one is running.

```console
$ syncthis conflict resolve 6f1c2f3a4b5c6d7e8f9a0b1c2d3e4f5a --keep remote --yes
notes/report.md: remote version kept
notes/report.sync-conflict-20260921-153000-ETI3RRZ.md: removed
```

```json
{
  "id": "6f1c2f3a4b5c6d7e8f9a0b1c2d3e4f5a",
  "folder_id": "docs",
  "path": "notes/report.md",
  "kept": "remote",
  "kept_path": "notes/report.md",
  "removed_path": "notes/report.sync-conflict-20260921-153000-ETI3RRZ.md",
  "resolved": true
}
```

`moved_from` is added when the winning version had to be renamed onto the path.

### `syncthis conflict export ID DIR`

Copies both versions into DIR, together with a JSON file describing the
conflict, for a comparison or a merge by hand. The folder is not touched and
the conflict stays open. The copies are named `path-<file>` and
`conflict-<file>`; the description is `<id>.json` and names, for each copy, the
path it came from, its hash, its size and whether it is the `local` or the
`remote` version. Each copy keeps the permission bits of the file it came from,
so an exported script stays runnable and a private file stays private; the
description is written `0644`.

A directory that cannot be created or written into exits 1 with a message
naming it, for example when DIR is under a directory the user may not write to.

```console
$ syncthis conflict export 6f1c2f3a4b5c6d7e8f9a0b1c2d3e4f5a /tmp/review
/tmp/review/path-report.md
/tmp/review/conflict-report.sync-conflict-20260921-153000-ETI3RRZ.md
/tmp/review/6f1c2f3a4b5c6d7e8f9a0b1c2d3e4f5a.json
```

### `syncthis completion bash|zsh|fish|powershell`

Writes a completion script to stdout. It is a shell script, not JSON, whatever
`--json` says. `--help` is the other exemption: it prints the usage text on
stdout whatever `--json` says, because help is for a person to read. Asking a
command group such as `syncthis folder` for its subcommand list with `--json`
is a usage error (exit 2) rather than help text, so a script never gets prose
where it expects a document.

### `syncthis version`

```console
$ syncthis version
syncthis 0.1.0-dev (4a0a52d, go1.26.7)
$ syncthis version --json
{"version": "0.1.0-dev", "commit": "4a0a52d", "go": "go1.26.7"}
```

`make build` stamps `git describe --tags --always --dirty` and the short commit
into the binary. A binary built without that, by `go install` or `go build`,
reports the module version and the revision the go tool recorded instead.

## Quick start on loopback

Two devices on one machine, which is also what the end-to-end tests do.

```console
$ mkdir -p /tmp/a/data /tmp/a/docs /tmp/b/data /tmp/b/docs
$ syncthis --data-dir /tmp/a/data init --name laptop --listen 127.0.0.1:22001
$ syncthis --data-dir /tmp/b/data init --name nas    --listen 127.0.0.1:22002
$ syncthis --data-dir /tmp/a/data folder add /tmp/a/docs --id docs
$ syncthis --data-dir /tmp/b/data folder add /tmp/b/docs --id docs

$ syncthis --data-dir /tmp/a/data daemon run &
$ syncthis --data-dir /tmp/b/data daemon run &

$ syncthis --data-dir /tmp/a/data peer add 127.0.0.1:22002 --yes
$ syncthis --data-dir /tmp/b/data peer add 127.0.0.1:22001 --yes
```

`peer add` tells each running daemon to reload, so both connect at once. Then:

```console
$ echo "hello from A" > /tmp/a/docs/greeting.txt
$ syncthis --data-dir /tmp/b/data status --json | jq '.folders[0].files'
```

## Pairing two devices over Tailscale

Both devices are on the same tailnet and have a `100.64.0.0/10` address.

1. On each device, initialize and add the folder with the same id:

   ```console
   $ syncthis init --name laptop
   $ syncthis folder add ~/docs --id docs
   ```

2. Bind the daemon to the tailnet interface on each device. Nothing else on the
   network can reach that address, so no extra flag is needed:

   ```console
   $ syncthis daemon run --interface tailscale0
   ```

3. Read each device's ID and tailnet address:

   ```console
   $ syncthis status --json | jq -r '.device.id, .daemon.listen'
   ```

4. Trust the other device from each side, pinning the id you just read:

   ```console
   laptop$ syncthis peer add 100.64.0.3:22000 --device-id <id of nas>
   nas$    syncthis peer add 100.64.0.2:22000 --device-id <id of laptop>
   ```

   Without `--device-id` the command shows the identity the peer presents and
   asks whether to trust it, which is the same decision made by eye.

5. Each `peer add` tells the local daemon to dial the new peer at once. Check:

   ```console
   $ syncthis peer test <id of the other device>
   $ syncthis status
   ```

A backup topology is `folder set docs --mode sendonly` on the laptop and
`--mode receiveonly --delete-policy never` on the nas: the nas then takes every
change the laptop makes and never removes a file, and nothing the nas holds
travels back.
