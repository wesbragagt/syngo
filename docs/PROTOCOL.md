# Syncthis peer protocol, version 1

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

## Message types

| Type | Name | Direction | Header |
|---|---|---|---|
| 0x01 | Hello | both, first frame after TLS | `{"protocol_version":1,"device_id":"…","device_name":"…","folders":[{"id":"…","mode":"…"}]}` |
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
 "modified_by":"<device id>","sequence":7,"synced_hash":"<64 hex>"}
```

`kind` is 1 for a file, 2 for a directory, 3 for a symlink. `hash` is the
BLAKE3-256 of the contents, empty for directories, symlinks and tombstones.
`mode` carries the permission bits only; a receiver whose copy already has the
announced content applies them with a `chmod` and transfers nothing. A path
that is a file on one side and a directory or a symlink on the other is a
`replace`: the receiver removes its entry and creates the announced kind in its
place. Both decisions are the receiver's; the wire only says what the sender
has.

`synced_hash` is the version the sender last agreed on with a peer, described
in [STATE.md](STATE.md#files). It is what lets the receiver tell a newer
version from a diverged one: an entry whose `synced_hash` is the version the
receiver holds was written on top of that version, so taking it loses nothing
and it is a pull rather than a conflict. A sender that omits the field, as one
from before it existed does, is read as "no baseline known", which changes no
other decision. The synced mode stays local bookkeeping and is never sent.

`ManifestChanged` says a folder moved on; the receiver answers it by requesting
the manifest again, so a missed notification costs nothing but time.

### Folder announcements

`Hello` carries the folder list, but folders are added, removed and given
another mode while daemons run. `FoldersChanged` carries the list again, at any
time: it replaces what the `Hello` announced, and the receiving client runs a
sync cycle over every folder both sides now share. A daemon sends it to the
peers connected to it whenever it reloads its configuration, which is what the
CLI asks for after `folder add`, `folder set` and `folder remove`. Neither side
has to reconnect for a new folder or a new mode to take effect.

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

`reason` is one of `not_found`, `hash_changed`, `folder_unknown`,
`folder_receive_only` or `paused`.

### Errors

A request the peer refuses for an application reason is answered with an Error
frame whose `code` is `folder_unknown`, `folder_receive_only` or `paused`, and
the connection stays up. An Error frame that answers nothing, or one with code
`bad_request`, ends the connection: the peers no longer agree on the protocol.

## Handshake

1. TCP connect, TLS 1.3 handshake. Each side derives the other's device ID from
   its leaf certificate.
2. Both sides write `Hello` immediately and read the other's. A `Hello` that
   announces a device ID other than the certificate's is answered with
   `Rejected{id_mismatch}`; a protocol version other than 1 with
   `Rejected{protocol_version}`. The connection closes either way.
3. The server looks the client up in its `devices` table. An unknown device is
   answered with `Rejected{unknown_device}` and closed. The rejection comes
   *after* the server's own `Hello`, on purpose: a `peer add` probe from an
   unpaired device learns the server's identity and name that way, which is how
   pairing starts. Each side runs `peer add` against the other.
4. The client then drives the connection: one request in flight at a time. The
   client read loop routes `ManifestChanged` and `FoldersChanged` frames to a
   notification channel and every other frame to the pending request.
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
