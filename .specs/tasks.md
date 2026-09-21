---
title: Syncthis Tasks
date: 2026-09-21
status: active
tags:
  - syncthis
  - project
  - tasks
  - tracking
---

# Syncthis tasks

Track every actionable piece of work for the Syncthis build. Mark a task complete only when it is implemented, tested, and recorded in the benchmark corpus.

## How to use this file

- One checkbox per task.
- Group work by phase. Do not start a later phase until the current phase meets its exit metrics.
- Update this file whenever scope, order, or tasks change.
- Reference the spec in [[syncthis-spec]] and the architecture decision in [[adr-001-go-tech-stack]].

## Architecture foundation

These tasks must land before most application work. They are not a phase on their own. They protect every later phase.

- [x] Choose module layout for ports and adapters.
- [x] Define domain entities: device, folder, file version, manifest, chunk, operation, journal, conflict, session, policy.
- [x] Define application use cases: pair, scan, compare, plan, transfer, apply, resolve, repair.
- [x] Define ports: filesystem, metadata store, content store, clock, peer discovery, transport, device identity, configuration, events, logging, UI.
- [x] Define a composition root that wires adapters.
- [x] Add interface tests in the domain package using fake adapters.
- [x] Document the domain language in a glossary.
- [x] Decide the public protocol format and schema versioning rules.

## Phase 0: Baseline

Goal: measure Syncthing and Obsidian Sync before building anything new.

- [ ] Pick the reference hardware for the benchmark corpus.
- [ ] Build a fixture generator for small files, large files, deep trees, renames, deletes, binary files, and editor saves.
- [ ] Build a runner that executes scenarios from one command.
- [ ] Capture JSON results for every run: convergence time, bytes, CPU, memory, disk writes, recovery.
- [ ] Add LAN, Tailscale direct, and Tailscale DERP scenarios.
- [ ] Run Syncthing against the corpus and record numbers.
- [ ] Run Obsidian Sync where allowed and record numbers.
- [ ] Store the corpus and results under a versioned directory.
- [ ] Define the target metrics Syncthis must beat.

## Phase 1: Local single-device engine

Goal: prove safe filesystem tracking without networking.

- [x] Add a folder and persist it in the metadata store.
- [x] Scan a folder and classify operations: create, modify, delete, rename.
- [x] Detect hidden files, Unicode names, empty directories, and symlinks by policy.
- [x] Store metadata and BLAKE3 hashes in the embedded database.
- [x] Use filesystem events for fast change detection.
- [x] Run periodic reconciliation to catch missed events.
- [x] Support include and exclude rules.
- [x] Add ignore file support.
- [x] Implement dry-run, status, rescan, and repair commands.
- [x] Add tests for rename, delete, atomic-save, and symlink behavior.
- [ ] Add a crash test that kills the process mid-scan.
- [ ] Verify a 100,000-file folder reconciles safely.
- [ ] Verify idle memory under 100 MB on the reference device.

## Phase 2: Two-device Tailscale transfer

Goal: transfer arbitrary files between two Tailscale devices.

- [x] Define device identity and a pairing protocol.
- [x] Add a `peer add` CLI command.
- [x] Implement Tailscale peer detection through MagicDNS and Tailscale IPs.
- [x] Bind the sync listener to the Tailscale interface only.
- [x] Verify the expected Tailscale node identity before accepting a device.
- [x] Implement an authenticated transport session.
- [x] Exchange manifests and compare files by content hash.
- [x] Implement fixed-size chunk transfer with BLAKE3 verification.
- [x] Implement resumable transfers with a persisted journal.
- [x] Install files atomically into place after verification.
- [x] Add a `status --json` command.
- [x] Add a `peer test` command that reports latency, route, and failures.
- [ ] Run a 10 GB transfer test with bounded memory.
- [x] Run an interrupted transfer test and verify resume.
- [x] Verify the listener never binds to a public interface.

## Phase 3: One-way and receive-only replication

Goal: dependable backup and mirror workflows.

- [x] Add send-only and receive-only folder modes.
- [x] Add read-only replicas.
- [x] Add configurable delete propagation policies.
- [x] Implement pause, resume, scheduled sync, and bandwidth limits.
- [x] Add dry-run for destructive operations.
- [x] Implement crash-safe transfer journals.
- [x] Add atomic destination writes.
- [x] Support configuration import and export.
- [x] Add a CLI command to inspect transfer state.
- [ ] Run 1,000 interruption and restart cycles without corruption.
- [ ] Run a 1 TB mirror test and verify no full retransfer after restart.
- [ ] Verify bandwidth stays within 5% of configured limits.

## Phase 4: Two-way sync and conflict safety

Goal: handle offline edits without silent data loss.

- [ ] Implement causal state with version vectors.
- [x] Detect concurrent edits.
- [x] Implement keep-both as the default conflict policy.
- [x] Handle rename versus delete cases.
- [x] Handle case-only renames.
- [x] Handle permission-only and symlink-target changes.
- [x] Add `conflict list`, `conflict show`, and `conflict resolve` commands.
- [x] Add conflict export for manual review.
- [ ] Implement optional three-way merge for text files.
- [x] Block automatic binary merging at the application layer.
- [x] Verify zero silent overwrites in the conflict suite.
- [x] Verify every divergent edit produces an explainable result.
- [ ] Verify 99.99% automatic convergence for non-conflicting changes.

## Phase 5: Performance and scale

Goal: beat the baseline on the target workload.

- [ ] Implement content-defined chunking.
- [ ] Implement cross-file and cross-folder chunk deduplication.
- [ ] Implement parallel transfer pipelines with backpressure.
- [ ] Implement adaptive concurrency based on disk and network measurements.
- [ ] Implement rename detection without re-uploading content.
- [ ] Replace full scans with journal watermarks where possible.
- [ ] Benchmark ordinary document change convergence under one second on a healthy Tailscale direct path.
- [ ] Benchmark a 1 MiB insertion into a 10 GB file transfers less than 2 MiB after caching.
- [ ] Benchmark a 10 GB rename transfers zero payload bytes.
- [ ] Benchmark 100,000-file status under two seconds after incremental change.
- [ ] Benchmark small edits use at least 90% fewer bytes than whole-file transfer.
- [ ] Compare against the Phase 0 baseline corpus.

## Phase 6: Interactive terminal product

Goal: a usable terminal interface without a GUI.

- [x] Define the local daemon API used by both the CLI and TUI.
- [ ] Build the interactive TUI with Bubble Tea.
- [ ] Add guided initialization and pairing.
- [ ] Show live device, folder, queue, path, and error views.
- [ ] Add conflict review and version restore screens.
- [ ] Add searchable event logs.
- [ ] Support reattach after SSH or terminal disconnect.
- [ ] Document a CLI equivalent for every TUI action.
- [x] Add `--json`, `--no-input`, and `--yes` flags.
- [x] Add shell completion.
- [x] Document exit codes.
- [ ] Add a usability test: pair and sync a folder in under five minutes with no docs.
- [ ] Add a reconnect test that drops the TUI without losing daemon state.

## Phase 7: Multi-device hardening and release

Goal: ship a reliable multi-device Tailscale-first system.

- [ ] Support three or more devices per folder.
- [ ] Implement folder-level device authorization.
- [ ] Recover automatically after a Tailscale restart.
- [ ] Recover after network changes.
- [ ] Add direct versus DERP diagnostics.
- [x] Add database migration handling.
- [x] Run a security review of the protocol and path handling.
- [ ] Add fuzz tests for the protocol and filesystem logic.
- [ ] Run a 30-day soak test.
- [ ] Run 10,000 forced network interruptions.
- [ ] Add packaging for Linux, macOS, Windows, NAS, container, and headless service.
- [ ] Document install, configure, and run instructions for each platform.
- [ ] Verify three-device non-conflicting convergence reaches 99.99%.
- [x] Verify upgrades across two schema versions preserve sync state.

## Ongoing work

Always tracked, never deferred.

- [ ] Update [[syncthis-spec]] when scope or design changes.
- [ ] Add an ADR when a decision is rejected, replaced, or measured.
- [ ] Record benchmark results in the corpus after every phase.
- [ ] Review the architecture boundaries after each phase.
- [ ] Keep the CLI, TUI, and JSON surface area in sync.

---

> Note link: https://notes.dory-pentatonic.ts.net/notes/projects/syncthis/tasks/

        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
        
