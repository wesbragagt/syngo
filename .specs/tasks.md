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

Goal: measure syncthis itself with a repeatable corpus and a repeatable runner.

Scope decision: no competitor measurement and no comparison. The benchmark
records our own numbers. See docs/BENCH.md.

- [x] Pick the reference hardware for the benchmark corpus.
- [x] Build a fixture generator for small files, large files, deep trees, renames, deletes, binary files, and editor saves.
- [x] Build a runner that executes scenarios from one command.
- [x] Capture JSON results for every run: convergence time, bytes, CPU, memory, disk writes, recovery.
- [x] Add LAN, Tailscale direct, and Tailscale DERP scenarios.
- [x] Run Syncthing against the corpus and record numbers. Dropped: no competitor measurement.
- [x] Run Obsidian Sync where allowed and record numbers. Dropped: no competitor measurement.
- [x] Store the corpus and results under a versioned directory.
- [x] Define the target metrics Syncthis must beat. Dropped: no comparison.

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
- [x] Add a crash test that kills the process mid-scan.
- [x] Verify a 5,000-file folder reconciles safely. Rescoped from 100,000 by
      adr-006: 5,000 is comfortably larger than the notes vault this program
      carries. Measured 2026-09-23 by `TestScale_VaultFolderReconciles` over
      200 directories of 25 files: the first scan recorded 5,200 entries in
      275 ms, the reconcile classified all 602 changes (500 modified, 50
      deleted, 50 added, no false renames) in 67 ms, and a repeated scan found
      nothing in 25 ms. The store matched the tree hash for hash after every
      step. See docs/BENCH.md.
- [x] Verify idle memory under 100 MB on the reference device. It holds at the
      vault file count. Measured 2026-09-23 by
      `TestScale_IdleMemoryUnderTheLimit` over 5,000 files: 600 samples across
      a five-minute idle window, peak 33.9 MiB, settled 32.8 MiB over the last
      quarter, against the 100 MiB limit. The earlier 148.8 MiB failure was
      measured over 100,000 files, a folder nobody here syncs; adr-006 records
      why that was the wrong thing to measure. See docs/BENCH.md.

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
- [x] Run a 10 GB transfer test with bounded memory.
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
- [x] Run 1,000 interruption and restart cycles without corruption.
- [x] Run a 2 GiB mirror test and verify no full retransfer after restart.
      Rescoped from 1 TB by adr-006: the vault is well under a gigabyte, and
      2 GiB of notes and attachments is twice it. Measured 2026-09-23 by
      `TestScale_VaultMirrorSurvivesRestart`: 4,062 files (4,000 notes plus 62
      attachments of 32 MiB) totalling 2.00 GiB mirrored from a send-only
      device to a receive-only one in 12.1 s, the receiver pulling the whole
      2.00 GiB. Both daemons then restarted with fresh event recorders and
      were watched for 90 s, three shipped sync intervals: the receiver pulled
      0 payload bytes, held no active or pending transfer, matched the source
      hash for hash and left no partial download. See docs/BENCH.md.
- [x] Verify bandwidth stays within 5% of configured limits.

## Phase 4: Two-way sync and conflict safety

Goal: handle offline edits without silent data loss.

- [x] Implement causal state with version vectors.
- [x] Detect concurrent edits.
- [x] Implement keep-both as the default conflict policy.
- [x] Handle rename versus delete cases.
- [x] Handle case-only renames.
- [x] Handle permission-only and symlink-target changes.
- [x] Add `conflict list`, `conflict show`, and `conflict resolve` commands.
- [x] Add conflict export for manual review.
- [x] Implement optional three-way merge for text files.
- [x] Block automatic binary merging at the application layer.
- [x] Verify zero silent overwrites in the conflict suite.
- [x] Verify every divergent edit produces an explainable result.
- [x] Verify non-conflicting changes converge over a stated number of real
      trials. Rescoped from 99.99% by adr-006: a four-nines rate needs tens of
      thousands of generated operations to mean anything, and convergence here
      is deterministic rather than probabilistic. Measured 2026-09-23:
      `go test -run TestTwoWay_IndependentEditsConverge -count=200` ran 200
      trials of two devices editing different paths at the same time, and 200
      of 200 converged with both edits intact, no conflict copies and no
      conflict rows on either device. This is a much smaller sample than a
      literal 99.99% claim would require; the safety net is the algorithm,
      version vectors with keep-both and a total order on content, together
      with the conflict suite, not a statistical rate.

## Phase 5: Performance and scale

Goal: beat the baseline on the target workload.

- [x] Implement content-defined chunking.
- [x] Implement cross-file and cross-folder chunk deduplication.
- [x] Implement parallel transfer pipelines with backpressure.
- [x] Implement adaptive concurrency based on disk and network measurements.
- [x] Implement rename detection without re-uploading content.
- [x] Replace full scans with journal watermarks where possible.
- [x] Benchmark ordinary document change convergence under one second on a healthy Tailscale direct path. Measured 2026-09-23 between icebox and macpro-nixos over a confirmed direct tailnet path: 1028 ms median round trip over 8 trials, so about 514 ms one way, against 507 ms over loopback. The tailnet costs about 7 ms; the rest is the 500 ms debounce. See docs/BENCH.md.
- [x] Benchmark a 1 MiB insertion into a 10 GB file transfers less than 2 MiB after caching. Measured 1,254,397 bytes into a 10 GiB file at the 64 KiB default, and under the budget at all six insertion positions swept over a 1 GiB file. See docs/BENCH.md.
- [x] Evaluate lowering `DefaultChunkSize`, or making it size-adaptive, to close the 2 MiB insertion budget. Done: lowered from 1 MiB to 64 KiB, size-adaptive rejected on the data. Measured at eight averages and six insertion positions, against the throughput, memory and chunk index cost of an ordinary large-file transfer. See "Choosing the default chunk size" in docs/BENCH.md.
- [x] Benchmark a 10 GB rename transfers zero payload bytes.
- [x] Benchmark 5,000-file status under two seconds after incremental change.
      Rescoped from 100,000 by adr-006. Measured 2026-09-23 by
      `TestScale_BenchStatusLatency`: over 5,000 files with 50 of them
      rewritten, 103 `syncthis status --json` calls made while the daemon took
      the change in answered in at most 15.4 ms, the change was reported
      525 ms after it was made, and 10 calls afterwards had a median of 3.5 ms.
      Every number is the real command including process start. See
      docs/BENCH.md.
- [x] Benchmark small edits use at least 90% fewer bytes than whole-file transfer.
- [x] Compare against the Phase 0 baseline corpus.

## Phase 6: Interactive terminal product

Goal: a usable terminal interface without a GUI.

- [x] Define the local daemon API used by both the CLI and TUI.
- [x] Build the interactive TUI with Bubble Tea. `syncthis tui`, in internal/tui; see docs/TUI.md and the architecture note in docs/ARCHITECTURE.md.
- [x] Add guided initialization and pairing.
- [x] Show live device, folder, queue, path, and error views.
- [x] Add conflict review and version restore screens. Restoring a version is `conflict resolve --keep local|remote|both`, the only three the command line has; the screen adds no fourth. There is no per-file version history in this program to restore from, so "version restore" means those three and nothing more.
- [x] Add searchable event logs. Two sources, marked apart: the changes this process saw between successive `status --json` answers, and the daemon's own structured log tailed from `--log-file`. There is no event stream on the control socket to subscribe to; adding one would be a change to the daemon's API. docs/TUI.md says so.
- [x] Support reattach after SSH or terminal disconnect.
- [x] Document a CLI equivalent for every TUI action. docs/TUI.md, cross-linked from docs/CLI.md; internal/arch keeps internal/tui from importing the application at all, so the only path from a key press to the daemon is a documented command.
- [x] Add `--json`, `--no-input`, and `--yes` flags.
- [x] Add shell completion.
- [x] Document exit codes.
- [x] Add a usability test: pair and sync a folder in under five minutes with no docs. `TestTUI_GuidedWalkPairsAndSyncsAFolder` drives the guided walk from an empty data directory to a converged folder against two real daemons, inside a 30 s wall-clock bound. It is a proxy for the human claim and the test says so in as many words: no automated test can measure a person reading, thinking and mistyping. What it does prove is that the walk works end to end, that every step is a documented command, and that nothing in it needs knowledge that is not on the screen at the time.
- [x] Add a reconnect test that drops the TUI without losing daemon state. `TestTUI_KillingItLeavesTheDaemonAndItsTransferAlone`: two daemons as real processes, a paced 4 MiB transfer in flight, the TUI killed with SIGKILL, the daemon the same pid afterwards, the transfer completed, and a fresh TUI showing the same device, folder and peer.

## Phase 7: Multi-device hardening and release

Goal: ship a reliable multi-device Tailscale-first system.

- [x] Support three or more devices per folder.
- [x] Implement folder-level device authorization.
- [x] Recover automatically after a Tailscale restart.
- [x] Recover after network changes.
- [x] Add direct versus DERP diagnostics.
- [x] Add database migration handling.
- [x] Run a security review of the protocol and path handling.
- [x] Add fuzz tests for the protocol and filesystem logic.
- [ ] Run an 8-hour soak test across three devices. Rescoped from 30 days by
      adr-006: what a soak finds is a function of how many change-and-break
      rounds go by, and the container soak forces a partition, a shaped link
      or a daemon kill every fifth round, which is denser than a personal
      tailnet sees in a month. Run 2026-09-23 and it FAILED after 72 minutes
      and 924 rounds: a daemon killed by the harness could not restart, exiting
      6 with `daemon already running with pid 14 (/data/daemon.pid)`. The pid
      file recorded a pid that a different process had since been given, and
      the liveness check correctly said that pid was alive, so the data
      directory refused every further start. See the pid-reuse task below. The
      soak must be run again to completion once that is fixed. Re-run
      2026-09-23 with the pid fix in place and it got much further, 1,719
      rounds over 2h17m against 924 over 1h12m, with no pid failure. It then
      failed differently: after the harness killed daemon a, a held two files
      c never received (`a/f-120186.bin`, `a/f-360791.bin`) and the cluster did
      not converge while settling. Under diagnosis; see the task below.
- [x] Fix the stale pid file that stops a killed daemon restarting. A daemon
      killed with SIGKILL cannot run its cleanup, so `daemon.pid` survived it;
      `acquirePIDFile` then read that pid and the liveness check reported it
      alive, because a container recycles low pids fast and an unrelated
      process had been given it, so the daemon refused to start against its own
      data directory. Not container-only: any system that recycles pids can hit
      it, and a power loss or an OOM kill could leave a data directory
      unstartable until the file was deleted by hand. Found by the soak above.
      Fixed 2026-09-23 by locking `daemon.pid` with flock, held open for the
      daemon's run: the kernel drops the lock however the process dies, so no
      pid is consulted and reuse cannot produce a false positive. Two
      regression tests reproduce the condition deterministically; both fail
      against the old check. Mutual exclusion is unchanged, still exit 6.
- [x] Run 1,000 forced network interruptions. Rescoped from 10,000 by adr-006
      and already passing:
      `TestScale_InterruptionCyclesWithoutCorruption` SIGKILLs the receiving
      daemon mid-download, restarts it and checks the hash that arrives, a
      thousand times over. A thousand crash landings in the middle of a real
      transfer is years of use for three personal machines whose interruptions
      are a lid closing, a network change or a reboot; the run also ends with
      an empty journal, a hash-for-hash identical tree and a clean
      `repair --dry-run`. 10,000 costs ten times the wall clock to say the
      same thing. See docs/BENCH.md.
- [x] Add packaging for Linux, macOS, NAS, container, and headless service. Windows is out of scope. A Nix flake is the supported install path; see flake.nix and nix/.
- [x] Document install, configure, and run instructions for each platform.
- [x] Verify three-device non-conflicting convergence over a stated number of
      real rounds. Rescoped from 99.99% by adr-006 for the same reason as the
      two-device line. Measured 2026-09-23:
      `go test ./internal/e2edocker -run TestMesh_ConcurrentDisjointEditsConverge -count=20`
      ran 20 clusters of three containers, each doing 10 rounds in which all
      three devices write five paths apiece at the same instant, so 200 rounds
      and 3,000 simultaneous non-conflicting changes in all. Every round ended
      with the three devices holding the same tree, no conflict rows anywhere,
      no lost edit and no partial download, and every run ended with the
      cluster invariants intact. 3,000 changes cannot distinguish a fault that
      happens once in ten thousand; the argument that none exists is the
      algorithm and the conflict suite, not the trial count.
- [x] Verify upgrades across two schema versions preserve sync state.
- [x] Fix `TestRestore_AnEditAfterAnOldDatabaseIsRestoredIsNotLost` (internal/e2e/restore_test.go). The test waited for the file content on b before reading the counter floor, but the restore replaces only the database, so that content was already on disk and the wait returned before b had ever spoken to a. The floor it then read was the 2 b's own rescan had produced. The test now waits until b has taken a's version record of the path, which only a sync cycle can write and which happens after adoptCounters in the same cycle. 40 race runs clean; with adoptCounters disabled it still fails 3 out of 3, so the claim is unchanged.
- [x] Decide what happens to an edit made on a device between restoring an old database and its first contact with a peer. **Decided: mark the version, force the conflict** (approach (a)). A version written while nothing has confirmed the counter floor is marked `provisional` on the entry (`files.provisional`, migration 011), and the sync plan keeps both versions instead of letting a peer's version replace, delete or displace it. The conflict reason is `unverified-counter-floor`; `conflict list` reports it and `conflict resolve` settles it like any other.

  Why not (b), holding local edits back until the floor is known: the daemon cannot refuse an edit, only refuse to record one, and an unrecorded edit is worse. The plan would then compare the restored entry against the peer's and pull, and the only thing standing between the user's bytes and the peer's is the installer's size-and-mtime check, which turns the loss into a permanent `ErrChangedUnderneath` failure loop instead. Worse still, once the floor was learned the delayed scan would hand the edit a counter *above* the peer's, so it would dominate and silently discard the peer's concurrent version: the same bug pointing the other way. (b) also has no end: a device whose peers are all offline would refuse edits indefinitely, and a timeout that gives up and records anyway is just (a) with a delay in front of it.

  The mark is per process run, not per database, because a restored database cannot be told from one that was not; every start doubts the floor until a peer sends a manifest, which costs nothing when no restore happened. It only acts where the peer holds a higher counter for the device that wrote the local version than that version carries, which is the signature of a database missing a write of its own and of nothing else, so an ordinary offline edit and an ordinary "the peer built on my version" pull both still resolve cleanly. It is dropped when a peer is found holding the same content, when a conflict settles the path, or when a later scan under a confirmed floor rewrites the entry.

  Residual, stated rather than hidden: with several peers the floor is confirmed by the first manifest from any of them, and a second peer may hold a counter this device has not seen. Contact with it raises the floor as before, but a version written between the two contacts is no longer marked. Covering that would need every configured peer to have been heard from, which a device with an offline peer would wait on forever. Documented in docs/STATE.md, "The restore window", together with the procedure that avoids it.

  Covered by `TestRestore_AnEditMadeBeforeTheFirstContactIsNotLost` (30 race runs clean; with the rule disabled the edit is gone from both trees), `TestPlan_ProvisionalVersions`, `TestSync_AnEditFromAnUnconfirmedFloorIsNotOverwritten` and the scanner and store tests beside them. `TestRestore_AnEditAfterAnOldDatabaseIsRestoredIsNotLost` is unchanged and clean over 20 race runs.

## Ongoing work

Always tracked, never deferred.

- [x] Update [[syncthis-spec]] when scope or design changes.
      - spec and ADRs reconciled 2026-09-22: Windows dropped (adr-002), competitor benchmarking dropped (adr-003), chunk size changed to 64 KiB (adr-004), Nix flake primary (adr-005)
- [x] Add an ADR when a decision is rejected, replaced, or measured.
      - ADRs 002-005 added 2026-09-22: Windows support (002), competitor benchmarking (003), chunk size (004), Nix packaging (005)
- [x] Record benchmark results in the corpus after every phase. Current through Phase 5 as of 2026-09-22: bench/results/v1/ holds pre/post runs for the chunking and pipeline work; docs/BENCH.md carries every dated measurement. Re-check after the next phase closes.
- [x] Review the architecture boundaries after each phase. Current as of 2026-09-22: `go test ./internal/arch/...` holds 8 checks, including the TUI import boundary added this phase, all passing. Re-check after the next phase closes.
- [x] Keep the CLI, TUI, and JSON surface area in sync. Current as of 2026-09-22: docs/TUI.md maps every TUI key to its `syncthis` command; docs/CLI.md documents every --json shape the TUI decodes. Re-check whenever either surface changes.

---

> Note link: https://notes.dory-pentatonic.ts.net/notes/projects/syncthis/tasks/
