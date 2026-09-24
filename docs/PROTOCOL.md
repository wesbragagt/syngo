# Syngo peer protocol, version 4

This is what `internal/adapters/tcpwire` implements. It is the contract between
two daemons; the application layer sees it through `internal/app/ports`.

## Transport

TLS 1.3 over TCP, ALPN `syngo/1`. Both sides present their identity
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
uint32 BE  length      = 1 + 8 + 4 + len(header) + len(payload)   // at most 64 MiB
uint8      type
uint64 BE  request_id
uint32 BE  header_len
[bytes]    header      JSON object (UTF-8)
[bytes]    payload     raw bytes; empty except for ChunkData and ChunkListReply
```

`request_id` correlates a reply with the request it answers. The client picks
it, starting at one and never repeating on a connection; the server echoes it
unchanged on the frame that answers, whatever that frame's type is. Zero means
the frame answers nothing: `Hello`, `Rejected`, `ManifestChanged` and
`FoldersChanged` all carry zero.

This is what lets a client have several requests outstanding at once. A reply
that arrives after its caller gave up names an id nobody is waiting for and is
dropped, so a request that times out no longer costs the connection.

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
| 0x01 | Hello | both, first frame after TLS | `{"protocol_version":4,"device_id":"…","device_name":"…","folders":[{"id":"…","mode":"…"}]}` |
| 0x02 | Rejected | server → client, then close | `{"reason":"unknown_device"\|"protocol_version"\|"id_mismatch"}` |
| 0x03 | Ping | client → server | `{"nonce":123}` |
| 0x04 | Pong | server → client | `{"nonce":123}` |
| 0x10 | ManifestRequest | client → server | `{"folder_id":"…"}` |
| 0x11 | ManifestResponse | server → client | `{"folder_id":"…","sequence":N,"entries":[…]}` |
| 0x12 | ManifestChanged | server → client, unsolicited | `{"folder_id":"…","sequence":N}` |
| 0x13 | FoldersChanged | either, unsolicited | `{"folders":[{"id":"…","mode":"…"}]}` |
| 0x20 | ChunkRequest | client → server | `{"folder_id","path","hash","index","offset","length"}` |
| 0x21 | ChunkData | server → client | `{"folder_id","path","index","offset","length","chunk_hash"}` + payload |
| 0x22 | ChunkError | server → client | `{"folder_id","path","index","reason":…}` |
| 0x23 | ChunkListRequest | client → server | `{"folder_id","path","hash"}` |
| 0x24 | ChunkListReply | server → client | `{"folder_id","path","count":N}` + payload |
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
and must name neither `.syngo` nor `.syngoignore` nor anything beneath
them. `.syngo` is this device's own state directory; `.syngoignore` is
the local user's statement about what this device wants, so a peer may not
write it. The rules are `manifest.ValidRelPath`, and the plan and the installer
apply them again on their own.

`link_target` is interpreted relative to the directory holding the link and
must stay inside the folder: an absolute target, or one that climbs out with
`..`, or one that reaches the state directory, refuses the manifest
(`manifest.ValidLinkTarget`). The receiver also refuses to write through a
parent that is a symbolic link, whatever the manifest said.

`size` must be between zero and the receiver's `max_file_size`, which defaults
to one tebibyte. The size is what the chunk list is checked against, so it is
not a number a peer may choose freely. A single file cut into more than 1 Mi
chunks is refused on its own, leaving the rest of the cycle to run.

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

### Folder authorization

Each folder names the devices allowed to synchronize it. The folder list in a
`Hello`, and in every `FoldersChanged` that follows it, holds only the folders
the peer being written to is authorized for, so a folder a peer may not have is
never named to it. The list is per connection: two peers of the same device
receive different lists.

The naming is not the protection. Every `ManifestRequest` and every
`ChunkRequest` is checked against the authorization of the device the
connection's certificate identifies, at the moment the request arrives. A
request for a folder the peer is not authorized for is answered exactly as a
request for a folder that does not exist: `folder_unknown`, with the same
message. A peer cannot tell the two apart, and nothing it sends widens what it
may read. Checking per request rather than per handshake is also what makes a
revocation stop a transfer that is already running.

This needed no wire change when it was added and the protocol stayed at version
2 for it. `Hello` and
`FoldersChanged` already carry a folder list, and this only narrows what goes
into it; `folder_unknown` is already the refusal for a folder the peer cannot
have. No frame, field or code is added, removed or reinterpreted, so a device
running the older code and one running this one speak the same protocol: the
older one simply announces and serves more than it should, which is what it did
before. There is nothing for a version bump to protect against.

`ManifestChanged` and `FoldersChanged` are the only unsolicited frames.

### Chunk exchange

Chunking is content-defined: the boundaries of a file come from its bytes, not
from a fixed size, so only the side holding the bytes knows where they fall. A
receiver therefore asks for the chunk list first and then for byte ranges out
of it.

`ChunkListRequest` names the whole-file hash the receiver is downloading.
`ChunkListReply` answers with `count` chunks in a binary payload, in order and
contiguous from offset zero:

```
repeat count times:
  uint32 BE  length        // 1..16 MiB
  [32]byte   chunk_hash    // BLAKE3-256 of those bytes
```

The offset and the index of each chunk follow from the order, so neither is
sent: one chunk costs 36 bytes, and a million of them, the limit, stay inside
the frame limit. The receiver checks the list partitions a file of the
announced size before it builds anything from it; a list that does not is
refused and only that file is given up on.

The list is also what deduplication works from. The receiver looks every chunk
hash up in the content it already holds, across its files and its folders, and
asks the peer only for what it finds nowhere. Bytes it finds locally are read
back and hashed before they are used, so an index entry that has gone stale
since the last scan costs one read and then a normal `ChunkRequest`.

A `ChunkRequest` names the byte range from that list, together with the
whole-file hash. The sender serves it only while the file still hashes to that,
and refuses with `ChunkError{hash_changed}` once the file has moved on, so two
halves of two versions can never end up in one file, and so does a range that
runs past the end of the file. `chunk_hash` is the BLAKE3-256 of the payload;
the receiver verifies it against the payload and against what the chunk list
announced, before the bytes reach the disk.

A sender that has not yet indexed a path, which is every path of a database
written before the index existed, cuts the file on the spot to answer a
`ChunkListRequest`, so an upgraded device serves its peers before it has
rescanned.

The average chunk size a device is configured with is therefore not part of
this contract and changing it is not a protocol version. The list carries the
lengths the sending side actually cut, and a request names a byte range out of
that list, so no device ever derives a boundary from a size; two devices
configured with different averages interoperate. What a mismatch costs is
deduplication rather than compatibility: each device cuts its own files with
its own average when it scans, so boundaries a peer announces are recognized in
the local index only when both were cut the same way. The default moved from
1 MiB to 64 KiB with no version bump for exactly this reason; the measurements
are in [BENCH.md](BENCH.md).

A `path` that breaks the rules above is answered with `ChunkError{not_found}`,
like a request for a file the sender does not have. A refusal of a chunk list
carries `index` zero: the request is about the whole file.

`reason` is one of `not_found`, `hash_changed`, `folder_unknown`,
`folder_receive_only` or `paused`.

A receiver may ask for no chunk at all. When the manifest a peer sends names a
path whose content this device already holds under another name, which is what
a rename or a copy on the peer looks like from here, the file is built from the
local one and the sender is never asked for a byte. The sender cannot tell the
difference and nothing on the wire changes, so this is not a protocol version:
it is a receiver that needs less of the protocol. See
[STATE.md](STATE.md#renames-and-copies).

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
   `Rejected{id_mismatch}`; a protocol version other than 4 with
   `Rejected{protocol_version}`. The connection closes either way. Both sides
   check, and there is no mixed-version operation: a version-1 peer sends no
   version vectors, so its entries cannot be ordered, a version-2 peer derives
   chunk boundaries from a size, so it would answer a byte-range request with
   the wrong bytes, and a version-3 peer puts no request id in its frames, so
   it reads every field of every frame at the wrong offset. Every device of a
   mesh upgrades together.
3. The server looks the client up in its `devices` table. An unknown device is
   answered with `Rejected{unknown_device}` and closed. The rejection comes
   *after* the server's own `Hello`, on purpose: a `peer add` probe from an
   unpaired device learns the server's identity and name that way, which is how
   pairing starts. Each side runs `peer add` against the other.
4. The client then drives the connection, with as many requests in flight as it
   likes. The client read loop routes `ManifestChanged` and `FoldersChanged`
   frames to a notification channel and every other frame to the request whose
   `request_id` it names; a frame naming nobody is dropped. The server answers
   at most 16 requests of one peer at a time, in whatever order they finish,
   and its read loop does no application work, so a chunk waiting on the disk
   or on the upload limiter does not hold up the one behind it. Those 16 slots
   are also what bounds how much one peer can make a device read into memory
   at once.
5. The client pings after 30 seconds of silence; both sides give up on a
   connection that says nothing for 90 seconds.
6. Each daemon dials every known peer and pulls from it, so a pair of devices
   has two connections, one per direction. Requests are multiplexed on each of
   them; connections are not pooled.

## Fuzzing

Every parser a peer can reach has a Go fuzz target. They run as ordinary tests
over their seed corpus during `go test ./...`, and only search for new inputs
when `-fuzz` names one:

```
go test ./internal/adapters/tcpwire/ -run xxx -fuzz FuzzReadFrame      -fuzztime 60s
go test ./internal/adapters/tcpwire/ -run xxx -fuzz FuzzReadHelloFrame -fuzztime 60s
go test ./internal/adapters/tcpwire/ -run xxx -fuzz FuzzWriteFrame     -fuzztime 60s
go test ./internal/adapters/tcpwire/ -run xxx -fuzz FuzzManifestReply  -fuzztime 60s
go test ./internal/adapters/tcpwire/ -run xxx -fuzz FuzzChunkRequest   -fuzztime 60s
go test ./internal/adapters/tcpwire/ -run xxx -fuzz FuzzHelloHeader    -fuzztime 60s
go test ./internal/adapters/tcpwire/ -run xxx -fuzz FuzzExchangeHello  -fuzztime 60s
go test ./internal/domain/manifest/  -run xxx -fuzz FuzzValidRelPath   -fuzztime 60s
go test ./internal/domain/manifest/  -run xxx -fuzz FuzzValidLinkTarget -fuzztime 60s
go test ./internal/domain/manifest/  -run xxx -fuzz FuzzParseVersion   -fuzztime 60s
go test ./internal/domain/ignore/    -run xxx -fuzz FuzzMatcher        -fuzztime 60s
```

One target at a time: `-fuzz` takes a single name. `-run xxx` keeps the rest of
the package's tests out of the run.

Each target asserts an invariant rather than the absence of a panic alone:

| Target | Invariant |
|---|---|
| `FuzzReadFrame`, `FuzzReadHelloFrame` | a decoded frame re-encodes to the bytes it was read from, fits the limit it was read under, and a refused one names `ErrFrameTooLarge`, `ErrMalformedFrame` or an EOF; a declared length above the limit is refused before the body is allocated |
| `FuzzWriteFrame` | what `WriteFrame` produces, `ReadFrame` returns with the same type, header and payload |
| `FuzzManifestReply` | every entry of an accepted manifest passes `ValidRelPath` and `ValidLinkTarget`, has a size within `0..max_file_size`, has a version vector that round-trips through its text form, and the manifest survives a second pass through the wire form |
| `FuzzChunkRequest` | the hash normalizes to lowercase hex and parses back to the same hash, and the request round-trips to the same header |
| `FuzzHelloHeader` | an accepted Hello names a device id that parses, keeps its folder list, and re-encodes with the current protocol version |
| `FuzzExchangeHello` | this side writes its own Hello first whatever the peer sends, a Hello announcing a device other than the certificate's is never accepted, and a refused exchange returns no device id |
| `FuzzValidRelPath` | an accepted path is already cleaned and, joined to a folder root, stays under that root; it is not absolute, holds no NUL byte and names no reserved path |
| `FuzzValidLinkTarget` | an accepted target resolves, from the directory holding the link, to a path inside the folder that is not reserved |
| `FuzzParseVersion` | a parsed vector round-trips through `String`, and its counters are sorted, non-zero and unique |
| `FuzzMatcher` | matching is total and deterministic, a leading slash does not change the verdict, and an empty rule set ignores nothing |

Corpus entries a run finds live in the build cache, not the repository. A
`testdata/fuzz` file is committed only to record an input that found a defect;
`internal/domain/manifest/testdata/fuzz/FuzzParseVersion` holds one, described
in `TestParseVersionLeadingSpaceDeviceIDDefect`.

## Pairing

```
syngo --data-dir A peer add <host-of-B>:22000 --verify   # A trusts B
syngo --data-dir B peer add <host-of-A>:22000 --verify   # B trusts A
```

`peer add` probes the address, which yields the peer's device ID and name even
when the peer has not added us yet (`Rejected{unknown_device}`). `peer test`
reports the same handshake as `tls_ok`, `authenticated`, a reject reason and
the Ping round trip, and exits 7 when the peer cannot be reached or refuses.

`peer add` also tells a running daemon to reload, so the new peer is dialled at
once. Neither daemon has to be restarted, before or after pairing.
