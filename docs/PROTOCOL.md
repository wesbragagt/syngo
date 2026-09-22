# Syncthis peer protocol, version 2

This is what `internal/adapters/tcpwire` implements. It is the contract between
two daemons; the application layer sees it through `internal/app/ports`.

## Transport

TLS 1.3 over TCP, ALPN `syncthis/1`. Both sides present their identity
certificate; the server requires a client certificate. Certificates are
self-signed, so chain verification is disabled: the identity of a peer is the
device ID derived from its leaf certificate,

```
device.IDFromPublicKey(leaf.RawSubjectPublicKeyInfo)
  = base32(sha256(SubjectPublicKeyInfo))   // 52 uppercase characters
```

A client that was given an expected device ID aborts the TLS handshake when the
peer derives another one. On a tailnet the traffic is already encrypted; TLS is
kept so authentication does not depend on the network and so loopback tests
exercise the production path.

## Frame

```
uint32 BE  length      = 1 + 4 + len(header) + len(payload)   // at most 64 MiB
uint8      type
uint32 BE  header_len
[bytes]    header      JSON object (UTF-8)
[bytes]    payload     raw bytes; empty for every type except ChunkData
```

A frame longer than 64 MiB is refused without reading its body and the
connection is closed: the reader cannot find the next boundary. A frame whose
header is longer than its body is refused the same way.

Until the Hello exchange has finished the limit is 64 KiB, not 64 MiB. Anyone
who can complete a TLS handshake can send a frame, so what a peer may make this
device allocate before it has said who it is stays small; a Hello is a few
hundred bytes. At most 32 connections run their TLS handshake and Hello
exchange at the same time, and one that cannot get a slot within the handshake
timeout is dropped.

## Message types

| Type | Name | Direction | Header |
|---|---|---|---|
| 0x01 | Hello | both, first frame after TLS | `{"protocol_version":2,"device_id":"…","device_name":"…","folders":[{"id":"…","mode":"…"}]}` |
| 0x02 | Rejected | server → client, then close | `{"reason":"unknown_device"\|"protocol_version"\|"id_mismatch"}` |
| 0x03 | Ping | client → server | `{"nonce":123}` |
| 0x04 | Pong | server → client | `{"nonce":123}` |
| 0x10 | ManifestRequest | client → server | `{"folder_id":"…"}` |
| 0x11 | ManifestResponse | server → client | `{"folder_id":"…","sequence":N,"entries":[…]}` |
| 0x12 | ManifestChanged | server → client, unsolicited | `{"folder_id":"…","sequence":N}` |
| 0x13 | FoldersChanged | either, unsolicited | `{"folders":[{"id":"…","mode":"…"}]}` |
| 0x20 | ChunkRequest | client → server | `{"folder_id","path","hash","index","chunk_size"}` |
| 0x21 | ChunkData | server → client | `{"folder_id","path","index","offset","length","chunk_hash"}` + payload |
| 0x22 | ChunkError | server → client | `{"folder_id","path","index","reason":…}` |
| 0xF0 | Error | either | `{"code":"…","message":"…"}` |

### Manifest exchange

`ManifestResponse` carries every entry of the folder, tombstones included, so
the receiver can tell "never had it" from "deleted". One entry is

```json
{"path":"sub/a.txt","kind":1,"size":1234,"mtime_ns":1700000000123456789,
 "mode":420,"hash":"<64 hex>","link_target":"","deleted":false,
 "modified_by":"<device id>","sequence":7,
 "version":{"<device id>":3,"<device id>":1}}
```

Every entry is validated before anything is built from it, and one bad entry
refuses the whole manifest and closes the connection: a peer that sends one is
not a peer to take anything from.

`path` is folder-relative and slash-separated. It must be non-empty, must not
start with a `/`, must contain no `.` or `..` or empty segment and no NUL byte,
and must name neither `.syncthis` nor `.syncthisignore` nor anything beneath
them. `.syncthis` is this device's own state directory; `.syncthisignore` is
the local user's statement about what this device wants, so a peer may not
write it. The rules are `manifest.ValidRelPath`, and the plan and the installer
apply them again on their own.

`link_target` is interpreted relative to the directory holding the link and
must stay inside the folder: an absolute target, or one that climbs out with
`..`, or one that reaches the state directory, refuses the manifest
(`manifest.ValidLinkTarget`). The receiver also refuses to write through a
parent that is a symbolic link, whatever the manifest said.

`size` must be between zero and the receiver's `max_file_size`, which defaults
to one tebibyte. The size decides how many chunks a transfer is split into and
how many journal rows it takes, so it is not a number a peer may choose freely.
A single file that would need more than 4 Mi chunks is refused on its own,
leaving the rest of the cycle to run.

`kind` is 1 for a file, 2 for a directory, 3 for a symlink. `hash` is the
BLAKE3-256 of the contents, empty for directories, symlinks and tombstones.
`mode` carries the permission bits only; a receiver whose copy already has the
announced content applies them with a `chmod` and transfers nothing. A path
that is a file on one side and a directory or a symlink on the other is a
`replace`: the receiver removes its entry and creates the announced kind in its
place. Both decisions are the receiver's; the wire only says what the sender
has.

`version` is the version vector of the entry: for each device, how many
content changes it has made to that path. It is what lets the receiver order
two versions of a path. A version that dominates the receiver's (every counter
at least as high, one higher) was written on top of it, so taking it loses
nothing and it is a pull; a version the receiver's dominates is stale and
changes nothing here; two versions where each is ahead on some device were
written concurrently and the receiver keeps both. Installing a version adopts
its vector, and the outcome of a conflict adopts the merge of both, so a third
device holding either input takes the outcome without a second conflict.

The field is absent for a path whose stored row predates version vectors,
which reads as the zero vector: dominated by every other vector, equal to
every other empty one. A counter naming something that is not a device id, or
counting zero changes, refuses the whole manifest like any other bad entry.
Permission bits are deliberately not counted: a `chmod` is not a content
version, and the synced mode stays local bookkeeping and is never sent.

`ManifestChanged` says a folder moved on; the receiver answers it by requesting
the manifest again, so a missed notification costs nothing but time.

### Folder announcements

`Hello` carries the folder list, but folders are added, removed and given
another mode while daemons run. `FoldersChanged` carries the list again, at any
time: it replaces what the `Hello` announced, and the receiving client runs a
sync cycle over every folder both sides now share. A daemon sends it to the
peers connected to it whenever it reloads its configuration, which is what the
CLI asks for after `folder add`, `folder set` and `folder remove`, and whenever
a folder is paused or resumed. Neither side has to reconnect for a new folder,
a new mode or a resumed folder to take effect.

A paused folder is left out of both the `Hello` and the announcement, and the
client filters its cycles on the list it last received. The announcement on
resume is therefore what puts the folder back: without it a peer that connected
while the folder was paused would never pull it again on that connection.

The frame may travel in either direction. The serving side keeps no view of the
peer's folders, since the peer drives its own pulls, so it accepts the frame and
answers nothing.

`ManifestChanged` and `FoldersChanged` are the only unsolicited frames.

### Chunk exchange

A `ChunkRequest` names the whole-file hash the receiver is downloading. The
sender serves the chunk only while the file still hashes to it, and refuses
with `ChunkError{hash_changed}` once the file has moved on, so two halves of
two versions can never end up in one file. `chunk_hash` is the BLAKE3-256 of
the payload; the receiver verifies it before the bytes reach the disk.

A `path` that breaks the rules above is answered with `ChunkError{not_found}`,
like a request for a file the sender does not have.

`reason` is one of `not_found`, `hash_changed`, `folder_unknown`,
`folder_receive_only` or `paused`.

### Errors

A request the peer refuses for an application reason is answered with an Error
frame whose `code` is `folder_unknown`, `folder_receive_only` or `paused`, and
the connection stays up. `internal` is the code for a failure that is none of
those, such as a file the peer cannot read; the connection stays up for it too.

An Error frame that answers nothing, or one with code `bad_request`, ends the
connection: the peers no longer agree on the protocol. So does `unknown_device`,
which the server sends when the client has been removed from its `devices`
table since the handshake. The server checks the table before every request, so
a peer removed while it is connected stops being served at once.

## Handshake

1. TCP connect, TLS 1.3 handshake. Each side derives the other's device ID from
   its leaf certificate.
2. Both sides write `Hello` immediately and read the other's. A `Hello` that
   announces a device ID other than the certificate's is answered with
   `Rejected{id_mismatch}`; a protocol version other than 2 with
   `Rejected{protocol_version}`. The connection closes either way. Both sides
   check: a version-1 peer sends no version vectors, so its entries cannot be
   ordered, and there is no mixed-version operation. Every device of a mesh
   upgrades together.
3. The server looks the client up in its `devices` table. An unknown device is
   answered with `Rejected{unknown_device}` and closed. The rejection comes
   *after* the server's own `Hello`, on purpose: a `peer add` probe from an
   unpaired device learns the server's identity and name that way, which is how
   pairing starts. Each side runs `peer add` against the other.
4. The client then drives the connection: one request in flight at a time. The
   client read loop routes `ManifestChanged` and `FoldersChanged` frames to a
   notification channel and every other frame to the pending request. Frames
   carry no request id, so a reply has nowhere to go once its caller has given
   up: a request that times out ends the connection rather than leaving a late
   reply to answer the next question. The peer loop dials again.
5. The client pings after 30 seconds of silence; both sides give up on a
   connection that says nothing for 90 seconds.
6. Each daemon dials every known peer and pulls from it, so a pair of devices
   has two connections, one per direction. Nothing is multiplexed.

## Pairing

```
syncthis --data-dir A peer add <host-of-B>:22000 --verify   # A trusts B
syncthis --data-dir B peer add <host-of-A>:22000 --verify   # B trusts A
```

`peer add` probes the address, which yields the peer's device ID and name even
when the peer has not added us yet (`Rejected{unknown_device}`). `peer test`
reports the same handshake as `tls_ok`, `authenticated`, a reject reason and
the Ping round trip, and exits 7 when the peer cannot be reached or refuses.

`peer add` also tells a running daemon to reload, so the new peer is dialled at
once. Neither daemon has to be restarted, before or after pairing.
