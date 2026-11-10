---
title: ADR 006 Rescope the Scale and Reliability Targets to a Personal Vault
date: 2026-09-23
status: accepted
tags:
  - syngo
  - project
  - adr
  - scale
  - measurement
---

# ADR 006: Rescope the scale and reliability targets to a personal vault

- **Status:** Accepted
- **Date:** 2026-09-23
- **Decision owners:** Yoshi

## Context

The exit metrics in [[syngo-spec]] and the matching lines in [[tasks.md]] were
written as if this were a product sold to fleets: a 100,000-file folder, a 1 TB
mirror, a 30-day soak, 10,000 forced network interruptions, and 99.99%
automatic convergence measured two ways. None of those numbers came from the
workload this program exists to carry.

The real workload is one person's notes vault, today on Obsidian Live Sync,
synchronized between a few machines they own over Tailscale, with possibly a
NAS or a third machine later. The vault is a few
hundred to a few thousand markdown files plus attachments, well under a
gigabyte in total. The changes are small and frequent: a note saved while
typing, a few kilobytes at a time. The devices are personal machines that are
awake in bursts, not a datacenter that never sleeps.

Three things go wrong when the targets are written for a scale nobody has.

1. **They measure the wrong failure.** The idle-memory claim is the clearest
   case. Measured over 100,000 files an idle daemon settled at 148.8 MiB and
   the 100 MB target failed, so the task stayed open all through the build.
   Resident memory grows about 1.45 KiB per file, so the number that failed was
   almost entirely the size of a folder nobody syncs. At the real file count
   the same daemon holds 32.8 MiB and the target passes with room to spare.
   The open task was recording a folder that does not exist.

2. **They cannot be run, so they are never run.** A 1 TB mirror test and a
   30-day soak are not measurements, they are intentions. A target that is
   never executed proves nothing and quietly licenses shipping without
   evidence.

3. **99.99% is a statistical claim that needs thousands of trials.** Proving
   four nines of automatic convergence to any confidence needs tens of
   thousands of generated operations and a statistical argument about the
   sample. That is a reasonable thing for a vendor to do and a waste of effort
   for three machines the owner controls, where the property that matters is
   not a rate at all: version vectors, keep-both and the total order on content
   make convergence deterministic. A convergence *rate* below 100% would mean
   the algorithm is wrong, not that the odds are long.

## Decision

**Rescope every scale and reliability target to the vault this program
actually carries, and measure the rescoped version rather than relabelling a
number.** Eight targets change.

| Was | Is | Measured |
| --- | --- | --- |
| Reconcile a 100,000-file folder | Reconcile a 5,000-file folder | Yes |
| Idle memory under 100 MB (at 100,000 files) | Idle memory under 100 MB at 5,000 files | Yes |
| 100,000-file status under two seconds | 5,000-file status under two seconds | Yes |
| A 1 TB mirror resumes without a full retransfer | A 2 GiB mirror resumes without a full retransfer | Yes |
| 30-day soak | 8-hour soak across three devices | No, not run |
| 10,000 forced network interruptions | 1,000 forced interruptions | Yes, already passing |
| 99.99% two-way convergence for non-conflicting changes | 200 non-conflicting concurrent-edit trials, all converging cleanly | Yes |
| 99.99% three-device convergence | 200 rounds of 15 simultaneous non-conflicting edits across three devices, all converging cleanly | Yes |

### 5,000 files

Larger than any folder the owner syncs, with headroom: a vault of a few
thousand markdown files plus attachments, and room to double. The scale fixture
keeps its shape, 200 directories, and holds 25 files each instead of 500.
`SYNGO_SCALE_FILES_EACH` still runs the same tests over a larger tree on a
machine with time to spare, and a run that uses another number says so in its
own output.

### 2 GiB mirror

The vault is well under a gigabyte. 2 GiB of it is four thousand notes plus
sixty-two attachments of 32 MiB, which is an attachment-heavy vault twice over.
32 MiB is the size of the largest file this workload plausibly holds: an image,
a PDF, an audio note. It is not a disk image and it is not a virtual machine.

### 8-hour soak

The failure modes a soak finds are journal rows that are never dropped, partial
downloads that leak, watchers that stop reporting, and conflict rows that pile
up: all of them are functions of how many change-and-break rounds have gone by,
not of how many days the clock has run. The container soak forces a partition,
a shaped link or a daemon kill every fifth round, which is denser chaos than a
personal tailnet sees in a month. Eight hours of it is proportional coverage
for three machines. It has not been run yet, and the task line says so.

### 1,000 interruptions

The existing 1,000-cycle test is kept as sufficient, not lowered from 10,000
and then ignored. Each cycle kills the receiving daemon with SIGKILL while it
is downloading, restarts it, and checks the hash that arrives, so a cycle is a
crash landing in the middle of a real transfer. Three personal devices over
Tailscale see a handful of those a week: a laptop lid closing, a network
change, a reboot. A thousand of them is years of real use, and the run also
ends with a clean journal, a hash-for-hash identical tree and a clean
`repair --dry-run`. 10,000 would take ten times as long to tell us the same
thing.

### Convergence trials instead of four nines

Convergence is decided by version vectors and a total order on content, both
deterministic. What a test can add is evidence that the implementation of that
algorithm does not drop an edit or invent a conflict, and that evidence comes
from running real concurrent edits and checking every one. The replacement
targets are a real, stated number of real trials, reported as a count and not
as a rate. This is a far smaller sample than a literal 99.99% claim would
require, and the spec now says so where the number is.

### The benchmarks that already fit

The Phase 5 benchmarks were not all mis-scoped. The small-edit benchmark
(a 4 KiB edit costing 66 KiB on the wire, 99.90% fewer bytes than a whole-file
transfer) and the rename benchmark (zero payload bytes) are close proxies for
what a vault does all day: save a note, rename a note. The large-file insertion
benchmark, a 1 MiB insertion into a 10 GiB file, is the least representative of
the three and is kept only because it is the sharpest test of the chunker,
not because the workload has 10 GiB files.

## Consequences

### Positive

- Every rescoped target that can be measured on one machine has been measured,
  and four task lines that were open on numbers nobody could reach are now
  closed on numbers that were really run.
- The idle-memory claim holds honestly rather than failing against a folder
  that does not exist.
- The one target that has not been run, the soak, is visible as not run rather
  than hidden behind a number that was never going to be executed.

### Negative

- The program is no longer claiming to be measured at 100,000 files or a
  terabyte. Someone syncing a photo library or a source-code monorepo has no
  evidence here, and the tests would have to be re-run at their scale. The
  environment knobs make that a command rather than a code change, which is
  why they were kept.
- 200 trials cannot distinguish a fault that happens once in ten thousand
  edits. The argument that no such fault exists rests on the algorithm and on
  the conflict suite, not on the trial count.

## Alternatives considered

### Keep the numbers and mark them "aspirational"

Rejected. An exit metric that is not going to be measured is not an exit
metric. It reads as evidence to anyone who does not know it was never run.

### Keep the numbers and run them on rented hardware

Rejected for the reason adr-003 rejected competitor benchmarking: it adds
operational scope that moves nothing the owner of this vault will ever notice.

### Drop the numeric targets and test behaviour only

Rejected. The numbers are what catch a regression: an idle daemon at 300 MiB or
a mirror that re-pulls two gigabytes after a restart are both regressions a
behavioural test would pass.

## Related documents

- [[syngo-spec]] - exit metrics for Phases 1, 3, 4, 5 and 7 rewritten to these numbers
- [[tasks.md]] - the eight task lines, with the measurement each one now carries
- [[BENCH.md]] - the measured results and the commands that produced them
- [[adr-003-drop-competitor-benchmarking]] - the earlier decision to narrow measurement scope to our own claims
