---
title: ADR 004 Change Default Chunk Size to 64 KiB
date: 2026-09-22
status: accepted
tags:
  - syngo
  - project
  - adr
  - chunking
  - performance
---

# ADR 004: Change default chunk size from 1 MiB to 64 KiB

- **Status:** Accepted
- **Date:** 2026-09-22
- **Decision owners:** Yoshi

## Context

Syngo uses content-defined chunking to detect when only part of a file has changed and transfer only those bytes. The average chunk size is a tradeoff between:

- **Insertion cost:** A small change deep in a file requires retransmitting a few chunk boundaries around the insertion.
- **Transfer cost for files with nothing to deduplicate:** Smaller chunks increase the number of boundaries to cut and the overhead of the chunk index.
- **Memory cost:** Bounded by the maximum chunk size (four times the average), so smaller averages reduce per-scan buffers.

The specification declared a Phase 5 claim: "A 1 MiB insertion into a 10 GB file transfers less than 2 MiB." At the 1 MiB average, this claim failed at every insertion position measured, overrunning the budget by 7% to 91%.

Phase 5 work implemented a full evaluation of eight chunk averages (1 MiB, 768 KiB, 512 KiB, 384 KiB, 256 KiB, 128 KiB, 64 KiB, 32 KiB) at six insertion positions into a 1 GiB file, plus the throughput, memory, and index size cost of an ordinary transfer with nothing to deduplicate. Results are in docs/BENCH.md § "Choosing the default chunk size" (lines 598-728).

### Key measured data

**Insertion cost (1 MiB insertion into 1 GiB file, budget 2,097,152 bytes):**

| Average | Budget | Worst case | Headroom |
|---------|--------|------------|----------|
| 1 MiB   | 2.1 MB | 3.9 MB (91% over) | ✗ Fails at every position |
| 256 KiB | 2.1 MB | 2.2 MB (6% over) | ✗ Fails at two positions |
| 128 KiB | 2.1 MB | 2.1 MB (2.2% spare) | Marginal (inside run-to-run variation) |
| 64 KiB  | 2.1 MB | 1.5 MB (30% spare) | ✓ Passes at all positions |

**Transfer cost (1 GiB file, no deduplication):**

| Average | Throughput | Peak RSS | Index size per GiB |
|---------|------------|----------|-------------------|
| 1 MiB   | 456.6 MiB/s | 41.1 MiB sender / 30.3 receiver | 274.9 KiB |
| 64 KiB  | 240.8 MiB/s | 28.1 MiB sender / 30.2 receiver | 5.2 MiB |

The 240.8 MiB/s at 64 KiB is well above a 1 Gbit/s LAN (118 MiB/s) and far above typical Tailscale paths. The 10 GiB transfer test in Phase 3 measured 131.8 MiB/s, and the disk, not the chunk size, was the bottleneck: 64 KiB added only 2% to the wall time of a 10 GiB transfer.

Memory usage is flat from 1 MiB to 64 KiB and only rises at 16 KiB, which is below any chosen default.

## Decision

**Adopt 64 KiB as the default average chunk size**, replacing 1 MiB in `DefaultChunkSize` and `chunker.DefaultAvg`.

This choice:

1. **Passes the insertion budget at all positions measured** (1,454,745 bytes worst case vs. 2,097,152 budget, 30.6% spare).
2. **Keeps transfer throughput well above Tailscale paths** (240.8 MiB/s vs. ~100 MiB/s typical).
3. **Holds memory flat** over the range 1 MiB to 64 KiB (28-30 MiB peak on a 1 GiB transfer).
4. **Increases the chunk index by 5.2 MiB per TiB**, a real cost but one that only applies to large files that are large enough to see deduplication benefit: a 100,000-file folder of small files carries no additional index cost at any average, since each file is a single chunk.

## Consequences

### Positive

- The Phase 5 claim "a 1 MiB insertion into a 10 GB file transfers less than 2 MiB" now passes with measured 1,254,397 bytes (60% of budget).
- Small edits cost 16 times less network traffic than at the old 1 MiB average (66 KiB to edit a single chunk vs. 1 MiB).
- Memory impact is neutral to slightly positive (idling from 80.3 MiB to 75.6 MiB on a 30,000-file corpus).
- Insertion detection is more granular, making renames and minor edits cheaper.

### Negative

- Throughput on large files with no deduplication falls from 456.6 MiB/s to 240.8 MiB/s on loopback, a 47% drop. On real networks (Tailscale, LAN), this does not bind the transfer; the disk does. The 10 GiB test measured a 2% impact.
- The chunk index grows from 274.9 KiB per GiB to 5.2 MiB per GiB (19x), a real cost for folders with many large files that share no content. A 1 TiB folder of distinct large files costs 5 GiB of index instead of 200 MiB.
- Existing devices configured with 1 MiB will not see deduplication against new devices cutting at 64 KiB until both sides agree (or one is re-scanned). This is a performance property, not a correctness one: files still transfer and sync correctly, just without the deduplication benefit for that transfer.

## Alternatives considered

### Keep 1 MiB

Rejected: the insertion claim fails at every position by 7% to 91%. Relaxing the claim to match the chunk size would give up the property content-defined chunking exists to provide.

### 256 KiB (cheapest that passes most positions)

Rejected: it overruns the budget at two of six positions measured (2.2 MB and 2.2 MB moved vs. 2.0 MB budget). A default whose behavior depends on where a user's edit lands is not a default; it is unreliable.

### 128 KiB (maximum that passes with minimal margin)

Rejected on margin: it holds everywhere measured, but by 2.2% at the worst position, which is inside the run-to-run variation of where content-defined boundaries fall. A default 2.2% away from failure is not production-grade.

### Adaptive to file size

Rejected on the data: the workload the budget addresses is a small edit inside a large file, so a rule of "larger average for larger files" makes exactly the failing case worse. The opposite rule, a smaller average for large files, is what lowering the default does already. An adaptive rule would also add a second parameter both devices must agree on for deduplication, with no measured gain.

### 16 KiB (what the dedup test runs at)

Rejected: it buys 1,105,808 bytes against 64 KiB's 1,454,745 bytes (both far under budget), but pays 103.0 MiB/s throughput and 2% of the content in index rows for an unmeasured gain in edge cases. The 64 KiB default already provides 30.6% spare headroom.

## Compatibility

This is not a protocol change and requires no version bump. The chunk size is not carried on the wire. `ChunkListReply` carries the boundaries the sending side actually cut, and `ChunkRequest` names a byte range within those boundaries. A receiver never assumes a size.

Two devices configured with different averages interoperate today and still do after this change. Device A announcing 1 MiB boundaries finds nothing in Device B's 64 KiB index, and the transfer falls back to moving the full bytes—a performance property, not a correctness one. A device configured with 1 MiB keeps it; only new configurations or rescanned files pick 64 KiB.

See docs/BENCH.md § "Compatibility" (line 721) for full details.

## Measurements

- Full tradeoff data: docs/BENCH.md § "Choosing the default chunk size" (lines 598-728)
- Insertion evaluation: Appendix A, table of eight averages at six positions
- Transfer cost: Appendix B, throughput and memory comparison
- Phase 5 results: docs/BENCH.md § "The Phase 5 benchmarks" (lines 445-777)
- Scale suite results: `make scale SCALE_CHUNKEVAL_KIB=1024,512,256,128,64,32,16`

## Related documents

- [[syngo-spec]] - Phase 5 benchmarks section, updated to reflect 64 KiB default
- [[BENCH.md]] - Full measurement suite and decision analysis
- [[tasks.md]] - Phase 5 chunk size evaluation

