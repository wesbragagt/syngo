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
| 0x20 | ChunkRequest | client → server | `{"folder_id","path","hash","index","chunk_size"}` |
| 0x21 | ChunkData | server → client | `{"folder_id","path","index","offset","length","chunk_hash"}` + payload |
| 0x22 | ChunkError | server → client | `{"folder_id","path","index","reason":…}` |
| 0xF0 | Error | either | `{"code":"…","message":"…"}`; the connection closes after it |

Types 0x10 to 0x22 belong to the sync engine. Until it lands, a peer answers
them with an Error frame and closes.

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
   client read loop routes `ManifestChanged` frames to a notification channel
   and every other frame to the pending request.
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
