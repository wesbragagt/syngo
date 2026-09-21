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
| 11 | tailnet unavailable | a command that can only answer from the local Tailscale client found none, or found one whose backend is not running |

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
| `--sync-window HH:MM-HH:MM` | none | local-time window this folder pulls in; repeatable, see [Sync windows](#sync-windows) |

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
  "sync_windows": [],
  "paused": false
}
```

Exits 5 when the id or the path is already configured, and 2 when the path is
not an existing directory or a flag value is not one of the listed words.

### `syncthis folder list`

Prints the columns `ID`, `PATH`, `LABEL`, `MODE`, `DELETE POLICY`, `WINDOWS`,
`PAUSED`. `WINDOWS` reads `always` for a folder without sync windows. With
`--json` it prints an array of the object `folder add` returns.

### `syncthis folder set ID [--mode M] [--delete-policy P] [--sync-window W] [--clear-sync-windows]`

Changes the mode, the delete policy or the sync windows of a configured folder
and prints the folder in the shape `folder add` uses. At least one flag is
required, otherwise the command exits 2. A running daemon is asked to reload,
so it applies the new setting to its connected peers right away; see
[Lifecycle](#lifecycle).

`--sync-window` replaces the whole window list, it does not add to it.
`--clear-sync-windows` empties the list, which makes the folder pull at any
time again. Passing both is a usage error, exit 2.

```console
$ syncthis folder set docs --mode sendonly
folder docs: mode sendonly, delete policy propagate, windows always
$ syncthis folder set docs --sync-window 22:00-06:00
folder docs: mode sendonly, delete policy propagate, windows 22:00-06:00
```

#### Sync windows

A sync window is a span of the device's local wall-clock time in which this
device pulls a folder from its peers. `--sync-window 22:00-06:00` is the night;
the start is inclusive, the end is exclusive, and a start after the end wraps
past midnight. Repeating the flag makes the folder open during the union of the
windows. A folder with no window is open at all times. A window whose start
equals its end is refused with exit 2, as is anything that is not `HH:MM-HH:MM`.

Outside every window the folder still scans, so the local manifest and what the
peers are told about this device stay current, and it still serves, so a peer
may download from it: whether the other device pulls is the other device's
decision, exactly as folder modes work. What stops is the pull. A transfer in
flight when the window closes stops within one chunk, exactly like a pause,
stays `pending` with the error `outside sync window`, and the first cycle after
the window opens continues it from the journal.

A window opening is noticed at the next sync tick, so within `sync_interval`
(30 s by default). There is no extra timer. A window change through `folder
set` reloads the daemon, which runs a cycle at once, so removing a window takes
effect immediately.

The zone is the daemon process's local time, taken when the process starts; a
zone change needs a daemon restart. Across daylight saving the windows are read
as wall-clock times: a start inside the hour spring forward skips opens when
the wall clock reaches the normalized time (02:30 becomes 03:30), and a window
spanning the hour autumn repeats is one hour longer that night. There is no
per-folder sync interval: the global `sync_interval` covers it, and windows
plus the peers' change notifications do the rest.

### `syncthis folder remove ID [--yes]`

Stops synchronizing a folder. The files stay on disk; only the configuration
and the database rows go. It asks first, so it needs `--yes` when there is no
terminal.

```console
$ syncthis folder remove docs --json --yes
{"removed": "docs"}
```

### `syncthis config show`

Prints the device settings from `config.json`, the ones commands and the daemon
read. It does not need a running daemon.

```console
$ syncthis config show
listen = 127.0.0.1:22000
interface =
chunk_size = 1048576
max_file_size = 1099511627776
rescan_interval = 10m0s
sync_interval = 30s
log_level = info
upload_limit = 4 MiB/s
download_limit = unlimited
```

With `--json` the same settings, with the rates as bytes per second and `0` for
unlimited:

```json
{
  "listen": "127.0.0.1:22000", "interface": "", "chunk_size": 1048576,
  "max_file_size": 1099511627776, "rescan_interval": "10m0s", "sync_interval": "30s",
  "log_level": "info", "upload_limit": 4194304, "download_limit": 0
}
```

### `syncthis config set [FLAGS]`

Writes one or more settings and asks a running daemon to reload. It needs at
least one flag and exits 2 without one. It prints the settings afterwards, in
the same shape as `config show`.

| Flag | Meaning |
|---|---|
| `--upload-limit RATE` | outgoing chunk rate; `0` or `unlimited` lifts the limit |
| `--download-limit RATE` | incoming chunk rate; `0` or `unlimited` lifts the limit |
| `--rescan-interval DUR` | how often every folder is fully rescanned |
| `--sync-interval DUR` | how often a connected peer is asked what changed |
| `--listen HOST:PORT` | address peers connect to |
| `--interface NAME` | network interface to bind the first IPv4 address of |
| `--log-level L` | level the daemon logs at: `debug`, `info`, `warn` or `error` |

A `RATE` is a whole number of bytes per second, or a number with a unit:
`KB`, `MB` and `GB` are decimal, `KiB`, `MiB` and `GiB` binary, a trailing
`/s` is accepted, and a fraction such as `1.5MiB` is allowed. A rate that
cannot be read exits 2, as does a `--listen` that cannot be bound or a
`--log-level` outside the list, and nothing is written.

The bandwidth limits take effect at once, on the connections the daemon
already has. The other settings take effect when the daemon next starts.

```console
$ syncthis config set --download-limit 4MiB
...
download_limit = 4 MiB/s
$ syncthis config set --download-limit 0 --json | jq .download_limit
0
```

### `syncthis config export [--output FILE]`

Writes the configuration of this device as one JSON document: the settings, the
folders with their ignore patterns and sync windows, and the known peers.
Without `--output`, or with `--output -`, it goes to stdout; the document is
JSON already, so `--json` changes nothing about it. `--output FILE` writes the
file with mode 0600, also when the file was already there with a looser mode,
and prints a summary instead.

The identity is never part of a bundle: not the key pair, not the certificate,
not the device ID, and not the device name, which is baked into the
certificate. Neither are the database contents or the times a folder was
created, last scanned, added or last seen. A bundle describes a configuration,
not a device.

```console
$ syncthis config export --output /tmp/a.json
exported 1 folder and 1 peer to /tmp/a.json
$ syncthis config export --output /tmp/a.json --json
{"written": "/tmp/a.json", "folders": 1, "peers": 1}
```

The document:

```json
{
  "schema_version": 1,
  "exported_at": "2026-09-21T08:32:15Z",
  "settings": {
    "listen": "127.0.0.1:22000", "interface": "", "chunk_size": 262144,
    "max_file_size": 1099511627776, "rescan_interval": "10m0s",
    "sync_interval": "30s", "log_level": "info",
    "upload_limit": 0, "download_limit": 1048576
  },
  "folders": [
    {"id": "docs", "path": "/home/u/docs", "label": "", "mode": "sendreceive",
     "delete_policy": "propagate", "symlinks": "skip", "include_hidden": false,
     "keep_empty_dirs": true, "ignore_patterns": ["*.tmp"],
     "sync_windows": ["22:00-06:00"], "paused": false}
  ],
  "peers": [
    {"id": "ETI3RRZ…", "name": "nas", "address": "10.0.0.2:22000", "verified": true}
  ]
}
```

`exported_at` is informational and ignored on import.

### `syncthis config import FILE [--replace] [--dry-run] [--yes]`

Applies a bundle to this device. `FILE` may be `-` to read the document from
stdin; `--replace` then needs `--yes`, because stdin is already taken by the
document.

The whole document is validated before anything is written: it must decode
with no unknown field, carry `schema_version` 1, and every folder must pass the
same checks `folder add` makes, including that its path is an existing
directory on *this* machine. A document that fails any of these exits 2 and
writes nothing.

Conflicts are reported all at once, one line each on stderr, and nothing is
written: exit 5.

| Conflict | With `--replace` |
|---|---|
| a folder id is already configured | overwritten |
| a peer id is already known | overwritten |
| a folder path is already synchronized under another id | still a conflict |

Two ids cannot synchronize the same directory, which is why the last one stands
whatever the flags say. `--replace` is destructive, so it asks first and needs
`--yes` when there is no terminal. What it overwrites is the configuration
only: when the folder was created, when it was last scanned, the rows of its
files, and when a peer was added and last seen are kept. A folder or a peer the
bundle repeats exactly as this device already has it is left alone and is not
reported.

`--dry-run` validates, reports what would change and writes nothing. It exits
10 when something would change, 5 when the bundle conflicts with what is
configured and `--replace` was not given, because conflicts are found before
the plan, and 0 when there is nothing left to do. It never asks to confirm
`--replace`, having nothing to overwrite.

Imports of one data directory run one at a time: the second one waits on
`import.lock` in the data directory and then sees what the first one wrote, so
only one of them reports a folder or a peer as added.

A successful import asks a running daemon to reload, so the folders, the peers
and the limits take effect at once; see [Lifecycle](#lifecycle).

```console
$ syncthis config import /tmp/a.json
folder docs: added
peer ETI3RRZ…: added
settings: changed
$ syncthis config import /tmp/a.json
folder "docs": id already configured
peer ETI3RRZ…: already configured
syncthis: folder "docs": id already configured; peer ETI3RRZ…: already configured
$ echo $?
5
```

The three stores are written one after another, folders first, then peers, then
the settings; it is not one transaction. Validation and conflict detection both
run before the first write, so the only partial outcome is a store that fails
midway, and that reports what was written before it stopped, with exit 1.

**Exit codes.** 2 for an unreadable file, a document that does not decode, an
unknown field, another schema version or a configuration this device cannot
use, which includes a listen address or a log level `config set` would refuse
and a path two folders of the bundle share; 5 for conflicts; 9 for `--replace`
without a confirmation; 10 for a `--dry-run` that would change something; 1 for
a write that failed midway.

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
             "started_at": "2026-09-21T08:32:15Z",
             "upload_limit": 0, "download_limit": 4194304},
  "folders": [
    {"id": "docs", "path": "/home/u/docs", "label": "", "mode": "sendreceive",
     "delete_policy": "propagate", "paused": false, "state": "idle",
     "files": 1, "dirs": 0, "symlinks": 0, "bytes": 13,
     "last_scan_at": "2026-09-21T08:32:15Z", "pending_changes": 0, "error": null,
     "sync_windows": ["22:00-06:00"], "in_window": false,
     "next_window": "2026-09-21T20:00:00Z"}
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

`daemon.upload_limit` and `daemon.download_limit` are the rates in force in the
running daemon, in bytes per second, `0` for unlimited and for no daemon.
`conflicts.unresolved` counts the paths both sides changed that nobody has
decided about yet; `conflict list` shows them. `folders[].state` is `idle`,
`scanning`, `syncing`, `paused`, `scheduled` or `error`; `scheduled` is a
folder outside its [sync windows](#sync-windows). `folders[].sync_windows`
lists them in canonical form, `folders[].in_window` says whether the folder is
open now, and `folders[].next_window` is when it opens next, or `null` when it
is open already or has no window. Times
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
| `--upload-limit RATE` | outgoing chunk rate for this run; default from `config.json` |
| `--download-limit RATE` | incoming chunk rate for this run; default from `config.json` |

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

#### Bandwidth limits

One upload budget and one download budget per device, shared by every folder
and every peer connection, in bytes per second, `0` for unlimited, which is the
default. Only chunk payloads are metered: manifests, hellos, pings and the
control socket are not, because they are small and slowing them would slow the
very exchange that tells a peer to stop.

The budget is a token bucket holding 100 ms of traffic, so a whole chunk goes
out in one go and an idle hour cannot be spent at once. A wait for more bytes
than the bucket holds is granted by letting the level go negative and sleeping
off the debt, so a 1 MiB chunk is admitted at any rate. That is what makes 20
MiB at 4 MiB/s take about five seconds: the transfer is the burst short of the
full five.

A rate given to `daemon run` holds for that run and survives every reload; a
rate written with `config set` is picked up by the reload the command asks for,
without dropping a connection, unless a flag pinned that direction. The
download side is paced on the request side, so a peer is never asked faster
than this device admits and nothing is buffered waiting for the limit.

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
rescan interval. `folder add`, `folder set`, `folder remove`, `peer add`,
`peer remove` and `config set` do not wait for that tick: each asks the running daemon to reload
over the control socket, which starts and stops folder loops, dials peers that
were added, drops peers that were removed together with the connections those
peers had opened, announces the new folder list to the peers that are already
connected, and runs a sync cycle against each of them. Without a daemon the commands say nothing
and only write the database. The daemon never has to be restarted for a
configuration change.

### `syncthis peer add HOST[:PORT]`

Contacts the device at the address, shows the identity it presents and stores
it. The port defaults to 22000.

`HOST` may be an address literal, a bare Tailscale host name (`nas`) or a
MagicDNS name (`nas.tail1234.ts.net`). A host that is not a literal is looked
up in the tailnet first, through the same `tailscale status --json` the
`network` commands use; the address that answers is what gets stored, because
tailnet addresses are stable and storing the name would make every later dial
depend on the system resolver knowing MagicDNS. When Tailscale is missing, not
running, or does not know the name, the host is kept as typed and resolved by
system DNS at dial time.

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
  "accepted": false,
  "resolved_from": "nas"
}
```

`resolved_from` is the host name the tailnet turned into `address`; it is
absent when the address was used as typed. The text form says it too: `added
peer nas (ETI3…) at 100.64.0.3:22000 (nas via tailnet)`.

`accepted` says whether the peer already trusts this device. Pairing is done
when both sides ran `peer add` and both report `accepted: true`.

### `syncthis peer list`

Prints the columns `PEER`, `NAME`, `ADDRESS`, `VERIFIED`, `LAST SEEN`, or with
`--json` an array of the object above without `accepted`.

### `syncthis peer remove ID [--yes]`

Forgets a peer. It asks first.

### `syncthis peer test ID|HOST[:PORT]`

Runs the handshake and reports what happened. It exits 7 when the peer cannot
be reached or refuses this device. A `HOST` that is not an address literal is
resolved through the tailnet exactly as in `peer add`, and the result carries
`resolved_from`; a device ID names a peer whose address was already resolved
when it was added, so nothing is looked up.

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

### `syncthis network tailscale detect`

Runs `tailscale status --json` and reports what the local Tailscale client
said. A device without Tailscale is not a failure: the command prints
`present: false` and exits 0, because detection asked a question and "no" is an
answer. The binary is looked up on each call, so installing Tailscale later
needs no restart, and a device without it never shells out.

```console
$ syncthis network tailscale detect --json
{
  "present": true,
  "running": true,
  "backend_state": "Running",
  "version": "1.80.2",
  "interface": "tailscale0",
  "hostname": "laptop",
  "dns_name": "laptop.tail1234.ts.net",
  "ips": ["100.64.0.2", "fd7a:115c:a1e0::2"],
  "magic_dns_suffix": "tail1234.ts.net"
}
```

With no Tailscale installed every field is empty or false:

```console
$ syncthis network tailscale detect --json
{
  "present": false, "running": false, "backend_state": "", "version": "",
  "interface": "", "hostname": "", "dns_name": "", "ips": [],
  "magic_dns_suffix": ""
}
```

`interface` is the interface carrying this device's tailnet address. It is
empty when Tailscale runs in userspace networking mode, where there is no
interface to name; the text form prints `userspace networking` there. It is
the value to pass to `daemon run --interface`.

Text form:

```console
$ syncthis network tailscale detect
tailscale:        installed
version:          1.80.2
backend state:    Running
running:          true
interface:        tailscale0
hostname:         laptop
dns name:         laptop.tail1234.ts.net
ips:              100.64.0.2, fd7a:115c:a1e0::2
magic dns suffix: tail1234.ts.net
```

### `syncthis network peers`

Lists the other devices of the tailnet, sorted by host name. These are tailnet
peers, not syncthis peers: pair one with `peer add`. Unlike `detect` the
command exits 11 when there is no Tailscale client to ask, or when its backend
is not running, because the listing that was asked for cannot exist.

```console
$ syncthis network peers
HOSTNAME  DNS NAME                 IPS                             ONLINE  OS
desktop   desktop.tail1234.ts.net  100.64.0.5,fd7a:115c:a1e0::5    true    windows
nas       nas.tail1234.ts.net      100.64.0.3,fd7a:115c:a1e0::3    true    linux
phone     phone.tail1234.ts.net    100.64.0.4,fd7a:115c:a1e0::4    false   iOS

$ syncthis network peers --json
[
  {
    "id": "nNAS123CNTRL",
    "hostname": "nas",
    "dns_name": "nas.tail1234.ts.net",
    "ips": ["100.64.0.3"],
    "online": true,
    "os": "linux"
  }
]
```

The document `tailscale status --json` prints changes between releases, so it
is read tolerantly: unknown fields are ignored, `Online` falls back to `Active`
on clients too old to report it, and the MagicDNS suffix is read from
`CurrentTailnet` first and the top-level field second. Nothing but these two
commands and the host name convenience of `peer add` depends on Tailscale.

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

1. Check that Tailscale is up on each device and note the interface:

   ```console
   $ syncthis network tailscale detect
   $ syncthis network peers
   ```

2. On each device, initialize and add the folder with the same id:

   ```console
   $ syncthis init --name laptop
   $ syncthis folder add ~/docs --id docs
   ```

3. Bind the daemon to the tailnet interface on each device. Nothing else on the
   network can reach that address, so no extra flag is needed:

   ```console
   $ syncthis daemon run --interface tailscale0
   ```

4. Read each device's ID and tailnet address:

   ```console
   $ syncthis status --json | jq -r '.device.id, .daemon.listen'
   ```

5. Trust the other device from each side, pinning the id you just read. The
   host names are the ones `network peers` printed:

   ```console
   laptop$ syncthis peer add nas --device-id <id of nas>
   nas$    syncthis peer add laptop --device-id <id of laptop>
   ```

   The name is resolved through the tailnet and the address it answers with is
   what gets stored, so `peer list` shows `100.64.0.3:22000`. Without
   `--device-id` the command shows the identity the peer presents and asks
   whether to trust it, which is the same decision made by eye.

6. Each `peer add` tells the local daemon to dial the new peer at once. Check:

   ```console
   $ syncthis peer test <id of the other device>
   $ syncthis status
   ```

A backup topology is `folder set docs --mode sendonly` on the laptop and
`--mode receiveonly --delete-policy never` on the nas: the nas then takes every
change the laptop makes and never removes a file, and nothing the nas holds
travels back.
