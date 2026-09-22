# Syncthis glossary

The words this project uses for its own concepts, in the order they come up
when a folder is configured, scanned, planned against a peer and downloaded.
Each term links to the document that specifies it. Code comments and the other
documents use these words without redefining them.

## Devices and folders

**Device.** One installation of syncthis: one data directory, one key pair, one
daemon. A device is what peers know each other as, and a machine may host more
than one by giving each its own data directory. See
[STATE.md](STATE.md#data-directory).

**Device ID.** 52 base32 characters, the SHA-256 of the device certificate's
SubjectPublicKeyInfo. It is how a peer is named everywhere. `Short()` is its
first 7 characters, used in logs and conflict copy names; `Display()` writes it
in dashed groups of 13 characters, and both forms are accepted wherever an ID
is typed. See [CLI.md](CLI.md#syncthis-peer-add-hostport).

**Folder.** A directory plus an id shared by every device that synchronizes it,
a mode, a delete policy, its ignore patterns and its sync windows. See
[CLI.md](CLI.md#syncthis-folder-add-path---id-id).

**Folder ID.** The name two devices agree to call the same folder by, matching
`[a-z0-9][a-z0-9._-]{0,63}`. The paths on the two devices need not be alike;
the id is what is on the wire.

**Mode.** Which directions a folder synchronizes in: `sendreceive` both ways,
`sendonly` serves its contents and never pulls, `receiveonly` pulls and never
serves. It is a per-device setting, so one device may be `sendonly` for a
folder the other has as `receiveonly`. See
[CLI.md](CLI.md#syncthis-folder-set-id---mode-m---delete-policy-p---sync-window-w---clear-sync-windows).

**Delete policy.** What happens locally when a peer deleted a path:
`propagate` deletes it here too, `never` keeps the local file and reports the
item with reason `delete-policy-never`.

**Sync window.** A span of the device's local wall-clock time in which it
pulls one folder, written `HH:MM-HH:MM` with the start inclusive and the end
exclusive; a start after the end wraps past midnight. A folder without windows
pulls at any time. Outside its windows a folder still scans and still serves.
See [CLI.md](CLI.md#sync-windows).

**Scheduled.** The state `status` reports for a folder that is outside its sync
windows and is otherwise idle: it is not paused, has no error and is neither
scanning nor syncing, it is waiting for its next window.

**Symlink policy.** What a scan does with a symbolic link it finds: `store`
records it as an entry of kind symlink, `skip` leaves it out of the manifest.

**Ignore rules.** The patterns in `<folder>/.syncthisignore`, gitignore-style.
They belong to the device that wrote them: a path they cover is never scanned,
never sent and never pulled, and the file itself is neither synchronized nor
accepted from a peer. See [STATE.md](STATE.md#folder-state-directory).

## The manifest

**Entry.** What a manifest knows about one path: its kind, size, permission
bits, modification time, content hash, sequence number, tombstone flag and the
device that last changed it.

**Kind.** File, directory or symbolic link.

**Tombstone.** An entry marked deleted. It stays in the manifest so a peer can
tell "deleted" from "never had it".

**Valid path.** `manifest.ValidRelPath` is the one statement of which paths a
manifest may name: relative, slash-separated, no `.` or `..` or empty segment,
no NUL byte, and not the reserved `.syncthis` or `.syncthisignore`. It is pure
domain code because three layers apply it to the same peer input: the wire
refuses such a manifest, `syncplan.Plan` skips the entry with reason
`invalid-path`, and `syncer.Installer` refuses the call. `ValidLinkTarget` does
the same for where a stored symbolic link may point. The installer adds what
only a filesystem can answer: it refuses to write through a parent that is a
symbolic link.

**Manifest.** Everything a device knows about one folder: one entry per path,
tombstones included, each with a content hash and a per-folder sequence number.
See [PROTOCOL.md](PROTOCOL.md#manifest-exchange).

**Sequence.** A per-folder counter raised on every write, which is how a peer
decides whether it has already seen a version.

**Synced hash.** The content hash two devices last agreed on for a path. A file
whose hash still equals it has not been touched locally since the last sync;
one whose hash moved away from it is a local change. It travels with the
manifest, so a peer can tell a version written on top of its own from one that
diverged from it. **Synced mode** is the same idea for the permission bits, and
stays local.

## Scanning and planning

**Scan.** Walking a folder and bringing the manifest in line with the disk. A
full scan covers the whole root; a partial one covers the subtrees the watcher
reported. See [CLI.md](CLI.md#syncthis-scan-id---dry-run).

**Change.** What a scan found for one path: a create, a modify, a delete or a
rename. A rename is a delete and a create that share a content hash.

**Plan.** What `syncplan.Plan` returns for a folder: one action per path the
peer has. The actions are listed in [CLI.md](CLI.md#syncthis-sync-plan-folder---peer-id).

**Action.** One decision of a plan: `pull`, `mkdir`, `symlink`, `chmod`,
`replace`, `delete`, `conflict` or `none`.

**Reason.** Why a plan reached its action, for example `missing-locally`,
`remote-newer`, `both-modified` or `invalid-path`. Every reason is listed with
the actions in [CLI.md](CLI.md#syncthis-sync-plan-folder---peer-id).

## Downloading

**Chunk.** A fixed-size slice of a file, 1 MiB by default, verified against the
hash the sender announced before it is written. See
[PROTOCOL.md](PROTOCOL.md#chunk-exchange).

**Transfer.** One inbound file download, with a journal row per chunk. The
sender keeps no state.

**Journal.** The `transfers` and `transfer_chunks` rows of a transfer: which
chunks are verified and where the partial file is. It is what makes an
interrupted download resume instead of start over. See
[STATE.md](STATE.md#journal-semantics).

**Partial file.** The `.part` file a transfer writes into
`<folder>/.syncthis/tmp`. It lives inside the folder so the finished file is
renamed onto its target without crossing a filesystem boundary.

**Install.** Putting a finished download in place: the permission bits and the
modification time are set on the partial file, then it is renamed onto the
target path, so the path never holds half a file.

**Bandwidth limit.** The device-wide transfer rate one direction is held to,
in bytes per second, `0` for unlimited. Chunk payloads are metered; manifests,
hellos, pings and the control socket are not. See
[CLI.md](CLI.md#bandwidth-limits).

**Token bucket.** How a bandwidth limit is enforced: a bucket holding 100 ms of
traffic at the configured rate, refilled as time passes. A wait for more bytes
than the bucket holds lets the level go negative and sleeps off the debt, so a
chunk larger than the bucket is still admitted.

## Conflicts

**Conflict.** A path two devices changed independently. Neither version is
overwritten: the greater content hash keeps the path, the other version is
written beside it as a **conflict copy** named
`<stem>.sync-conflict-<date>-<time>-<author short id><ext>`, and the copy syncs
like any other file, so both devices end up with both versions. `conflict
resolve` is what closes one. See
[CLI.md](CLI.md#syncthis-conflict-list-folder---all).

**Conflict copy.** The file the losing version is written to. Its name comes
from that version alone, the time it was modified and the device that wrote it,
so every device moving the same version aside picks the same name and a copy
that is already there with the same content is reused. It is an ordinary file:
it is scanned, sent to peers and can be opened, renamed or deleted like any
other.

**Resolution.** Saying which version wins, with `conflict resolve --keep
local|remote|both`. The winner is renamed onto the path; `both` changes nothing
on disk and only closes the conflict. See
[CLI.md](CLI.md#syncthis-conflict-resolve-id---keep-localremoteboth).

**Stale conflict.** A conflict whose files no longer hold either of the two
versions it recorded, because something edited them since. It cannot be
resolved in favour of a version that is gone: `conflict list` marks it `stale`
and `conflict resolve` exits 8 unless it is closed with `--keep both`.

## Peers and the network

**Peer.** Another device this one knows: its device ID, its name, the address
to dial and whether its identity was verified by a human. See
[CLI.md](CLI.md#syncthis-peer-add-hostport).

**Pairing.** Running `peer add` on both devices, so each has the other in its
`devices` table. A device that is not paired is refused after the handshake,
which is what lets a probe learn an identity without being trusted. See
[PROTOCOL.md](PROTOCOL.md#pairing).

**Probe.** One handshake made only to learn who answers at an address: it
returns the peer's device ID and name, and whether the peer already accepts
this device. `peer add` and `peer test` are built on it.

**Tailnet.** The private network a Tailscale installation gives its devices,
addressed out of `100.64.0.0/10`. Syncthis is written for one: bind the daemon
to the `tailscale0` interface and no other network can reach the peer port. See
[CLI.md](CLI.md#syncthis-daemon-run).

**MagicDNS.** Tailscale's name service for a tailnet, which makes a device
reachable by its hostname. A hostname may be given to `peer add` wherever an
address is expected; it is resolved through the local Tailscale client before
system DNS, and the address that answers is what gets stored. See
[CLI.md](CLI.md#syncthis-peer-add-hostport).

**Tailnet peer.** A device the local Tailscale client knows about, which
`network peers` lists with its hostname, MagicDNS name, tailnet addresses and
online state. It is not a syncthis peer until `peer add` has trusted it on both
sides: network reachability and folder access are separate decisions. See
[CLI.md](CLI.md#syncthis-network-peers).

## Running

**Data directory.** The private directory of one device: `config.json`, the key
pair and certificate, `state.db`, and, while a daemon runs, `control.sock` and
`daemon.pid`. Resolved from `--data-dir`, `$SYNCTHIS_DATA_DIR`,
`$XDG_DATA_HOME/syncthis` or `~/.local/share/syncthis`. See
[STATE.md](STATE.md#data-directory).

**State directory.** `<folder>/.syncthis` inside a synchronized folder: the
partial files of its transfers. It is never scanned and never synchronized. See
[STATE.md](STATE.md#folder-state-directory).

**Configuration bundle.** The JSON document `config export` writes and
`config import` reads: the device settings, the folders with their ignore
patterns and sync windows, and the known peers. It carries no identity
material, so it describes a configuration, not a device. See
[CLI.md](CLI.md#syncthis-config-export---output-file).

**Control socket.** `<data-dir>/control.sock`, mode 0600, the unix socket a
running daemon answers commands on. It is also the lock on the data directory:
a daemon that answers there owns it. See [CLI.md](CLI.md#lifecycle).

**Reload.** Asking a running daemon to re-read what a command just wrote:
folder loops are started and stopped, peers are dialled or dropped, the folder
list is announced to connected peers and a sync cycle runs. It is what keeps a
configuration change from waiting for the next rescan tick.

**Daemon.** The process that watches the folders, rescans them, serves peers
and answers the control socket. Exactly one daemon owns a data directory. See
[CLI.md](CLI.md#syncthis-daemon-run).
