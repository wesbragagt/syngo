---
title: ADR 003 Drop Competitor Benchmarking from Phase 0
date: 2026-09-22
status: accepted
tags:
  - syncthis
  - project
  - adr
  - benchmark
  - measurement
---

# ADR 003: Drop competitor benchmarking from Phase 0 baseline

- **Status:** Accepted
- **Date:** 2026-09-22
- **Decision owners:** Yoshi

## Context

The original Syncthis specification included Phase 0 as a baseline measurement phase that would compare the system against Syncthing and Obsidian Sync. The plan was to measure convergence time, bytes transferred, CPU, memory, disk writes, and recovery across three network scenarios (LAN, Tailscale direct, Tailscale DERP) and use those results as target metrics to beat in later phases.

Initial Phase 0 work established a robust fixture generator, a repeatable corpus, and a measurement runner that captures all required metrics. However, running competitor benchmarks revealed practical and methodological issues:

1. **Availability and licensing constraints:** Syncthing is open source but not designed for unattended benchmarking. Obsidian Sync is proprietary and has licensing restrictions on automated testing.

2. **Incomparable measurements:** The systems are optimized for different workloads and use cases. Syncthing measures global sync across many devices; Syncthis targets two Tailscale-connected personal devices. A single corpus cannot fairly represent both systems' design assumptions.

3. **No concrete targets emerged:** Without a meaningful baseline, declaring "beat Syncthing by 20%" has no justification. The actual useful comparison is whether Syncthis meets its own design goals (fast change detection, bounded memory, resumable transfers), not whether it outlaps a different system.

4. **Timeline and focus:** Building the measurement harness was valuable; running competitors through it added scope without moving the product forward.

## Decision

**Remove competitor benchmarking from Phase 0 scope.** Phase 0 becomes a baseline for Syncthis alone: record convergence time, bytes transferred, CPU, memory, disk writes, and recovery against the corpus for all three network scenarios. Future phases measure Syncthis against its own claims, not against other systems.

This simplifies Phase 0 to:

- Build a fixture generator for the test corpus. ✓ Done
- Build a runner that executes scenarios from one command. ✓ Done
- Capture JSON results: convergence, bytes, CPU, memory, disk writes, recovery. ✓ Done
- Add LAN, Tailscale direct, Tailscale DERP scenarios. ✓ Done
- Store results under a versioned directory. ✓ Done

No measurement runs of Syncthing or Obsidian Sync, and no declared target metrics to "beat."

## Consequences

### Positive

- Phase 0 completes faster and with less operational overhead (no need to set up competing systems or manage their licenses).
- Results are clear and self-contained: here is what Syncthis does on this corpus.
- Later phases declare success by measuring against Syncthis' own claims (one-second convergence for edits, two-megabyte insertion budget, zero-byte renames).
- Removes the temptation to optimize for a competitor's workload instead of Syncthis' actual target.

### Negative

- Users cannot see "Syncthis is X% faster than Syncthing" without running their own comparison.
- Marketing and positioning cannot claim victory against established systems without measured justification.
- The claim "beat Obsidian Sync on small-edit efficiency" in the spec is removed; it becomes "achieve small-edit efficiency aligned with content-defined chunking design."

## Alternatives considered

### Run a limited baseline on two systems

Rejected because it kicks the problem down the road: which two systems, which workload, and on what hardware? Any choice is a design decision on top of a design decision, and competitors change faster than Syncthis benchmarks run.

### Create a synthetic "ideal" competitor

Rejected because it is invented and proves nothing.

## Related documents

- [[syncthis-spec]] - Benchmark plan section updated to remove competitor references
- [[tasks.md]] - Phase 0 scope section documents what was dropped and why
- [[BENCH.md]] - Phase 0 baseline section describes the corpus, runner, and three scenarios

