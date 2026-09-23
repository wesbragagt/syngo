---
title: ADR 001 Go Tech Stack
date: 2026-09-21
status: accepted
tags:
  - syncthis
  - project
  - adr
  - go
  - architecture
---

# ADR 001: Use Go for the Syncthis prototype

- **Status:** Accepted
- **Date:** 2026-09-21
- **Decision owners:** Yoshi

## Context

Syncthis is a personal file synchronization system for arbitrary files across devices connected through Tailscale. The first milestone must validate the product quickly. It needs a CLI, an interactive terminal UI, filesystem watching, resumable transfers, conflict visibility, and reliable recovery after network or process interruptions.

Rust is a possible long-term implementation, but choosing it now would increase prototype development time before the product and protocol are validated.

## Decision

Write Syncthis entirely in **Go** for the initial release.

Do not split the system between Go and Rust. Preserve a ports-and-adapters architecture and a strong domain model so a later Rust implementation remains possible without coupling the core rules to Go libraries or infrastructure adapters. Document the protocol and local state format. Treat any rewrite as a separate decision based on benchmark evidence.

## Initial stack

| Area | Choice | Reason |
|---|---|---|
| Language | Go | Fast iteration, simple deployment, strong concurrency and networking support |
| Filesystem events | `fsnotify` | Cross-platform event watcher with a simple API |
| Hashing | BLAKE3 | Fast content verification for arbitrary files |
| Local state | SQLite | Durable metadata, journal, transfer state, and conflict records |
| CLI | Cobra or equivalent | Subcommands, flags, shell completion, and stable automation interface |
| TUI | Bubble Tea with Lip Gloss | Interactive terminal workflow over the same daemon API |
| Logging | Structured logs | Machine-readable diagnostics and stable event fields |
| Network | Authenticated stream over Tailscale | Tailscale provides encrypted reachability; keep the first protocol simple |

The transport choice between TCP and QUIC remains open until benchmarks show a clear benefit. The prototype must not build a custom VPN or public relay.

## Prototype boundaries

The Go prototype targets:

- Linux first
- Two personal devices
- Tailscale hostname or IP pairing
- Tailscale-only listener by default
- Arbitrary file types
- One-way replication before two-way sync
- Filesystem events plus periodic reconciliation
- Whole-file BLAKE3 hashes
- Fixed-size chunks for resumable transfers
- Keep-both conflict handling
- CLI and JSON output
- Interactive TUI
- Recovery after process, network, and Tailscale interruptions

The prototype excludes mobile clients, public relays, hosted accounts, team sharing, custom VPN functionality, content-defined chunking, cross-file deduplication, three-way merging, and automatic binary merging.

## Success metrics

- Pair two Tailscale devices through the CLI in under two minutes.
- Complete setup through the TUI without reading source code.
- Correctly sync a 10,000-file test folder.
- Detect ordinary changes within two seconds.
- Resume an interrupted 10 GB transfer without retransmitting verified chunks.
- Never silently overwrite a conflicting file.
- Reconnect after a Tailscale restart without manual database repair.
- Stay below 150 MB idle memory on the reference Linux device.
- Produce stable JSON status suitable for scripts.
- Record convergence time, bytes transferred, CPU, memory, disk writes, and recovery events for every test.

## Consequences

### Positive

- Faster path to a working two-device prototype.
- One native binary with simple deployment.
- Good fit for a long-running daemon and CLI.
- Straightforward integration with system services and containers.
- The same local API can serve both CLI automation and the TUI.

### Negative

- Higher memory use may appear at scale compared with a tuned Rust implementation.
- Garbage collection and runtime behavior need measurement for large transfers.
- Go does not remove filesystem portability problems.
- A future Rust rewrite could require a protocol and state migration.

## Revisit criteria

Reconsider Go only after the prototype has representative benchmark data. A Rust implementation is justified if Go cannot meet one or more targets after normal optimization:

- Less than 150 MB idle memory.
- Under two-second ordinary change detection.
- Reliable 10 GB resumable transfers.
- 99.99% convergence for non-conflicting changes.
- 30-day soak without unrecovered sync failures.

A rewrite is not part of prototype completion. It must have its own ADR, migration plan, and compatibility tests.

## Related documents

- [[syncthis-spec]]
- [[CONTEXT]]

## Rejected alternatives

### Rust now

Rejected for the prototype because it optimizes for long-term resource control before the product workflow and protocol are validated.

### Go and Rust together

Rejected because it doubles implementation and test surface and creates unnecessary protocol coordination.

### Go prototype followed by an automatic rewrite

Rejected because a rewrite should be driven by measured constraints, not assumed future needs.

## Review date

Review after Phase 5 performance testing or when the prototype meets all initial success metrics.

---

> Note link: https://notes.dory-pentatonic.ts.net/notes/projects/syncthis/adr-001-go-tech-stack/
