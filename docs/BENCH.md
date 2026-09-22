# Syncthis benchmarks

Phase 0 baseline: run Syncthis against the corpus and record what happened.
This document defines the hardware, the layout, and the network scenarios. It
does not report results yet; every item below marked "unmeasured" is a
placeholder until a run fills it in.

## Reference hardware

One reference machine per run, recorded alongside every result so a number can
be traced back to what produced it.

- Two identical machines, `bench-a` and `bench-b`, connected over the same
  network path as the scenario under test.
- CPU, RAM, disk, OS and kernel version: unmeasured, to be filled in once the
  hardware is provisioned.
- Both machines run the same OS image and the same Syncthis version for a
  given corpus version.

Until real hardware is picked, `internal/bench/report.HardwareProfile` records
whatever `os.Hostname`, `runtime.GOARCH` and `runtime.NumCPU` a run actually
used, so a result is never silently attributed to the wrong box.

## Versioned corpus and results directory

```
bench/corpus/v1/       fixture files the runner reads, never written to by a run
bench/results/v1/      one JSON file per run, named by scenario and timestamp
```

`v1`, `v2`, ... is a directory, not a tag: a corpus version is regenerated
rarely and only when the fixture shape changes (new file types, new tree
depths). A results version tracks the corpus version it was measured against,
so `bench/results/v1/` is only ever compared against `bench/corpus/v1/`.
Comparing a result to a different corpus version is a mistake the directory
layout makes visible.

The corpus generator is `internal/bench/fixture`; the runner that drives a
scenario end to end is `internal/bench/runner`, run through the
`syncthis-bench` command below. `fixture.Generate` refuses to write into a
non-empty directory, so a corpus directory that already holds a tree from an
earlier run is reused rather than regenerated.

## Network scenarios

Three scenarios cover the paths Syncthis actually runs on. Each one is a
network setup, not a code path: the same runner and the same corpus run
against all three.

### LAN

What it measures: convergence time and throughput with no NAT traversal and no
relay, the fastest path available to the other two scenarios.

Setup: both machines on the same local network segment, wired if possible.
Syncthis binds to the LAN interface directly, Tailscale is not involved.

### Tailscale direct

What it measures: convergence time and throughput over a real Tailscale
tailnet when the two nodes hold a direct (peer-to-peer) connection, the path
Syncthis expects in normal operation. See `TailnetDiscovery` in
[ARCHITECTURE.md](ARCHITECTURE.md).

Setup: both machines joined to the same tailnet, `tailscale status --json`
confirms a direct connection (no `relay` field set on the peer), and the
listener binds to the Tailscale interface per the rule in ARCHITECTURE.md.

### Tailscale DERP

What it measures: convergence time and throughput when NAT traversal fails and
traffic falls back to a DERP relay, the path Syncthis falls back to when a
direct connection is not available.

Setup: same tailnet as the direct scenario, but with direct connections
forced off (for example by blocking UDP between the two machines at the
firewall) so `tailscale status --json` reports a `relay` value for the peer.

## Running a scenario

`syncthis-bench` (built from `cmd/syncthis-bench`) generates the corpus into
the versioned corpus directory, starts the devices a scenario needs, pairs
them, shares a folder, waits for convergence, applies the mutation set
(rename, delete, modify, editor save), waits again, measures a recovery by
killing one daemon mid-transfer and restarting it, and writes one
`internal/bench/report.Result` to the versioned results directory. It prints
the result path on success, followed by one `unmeasured: <field>: <reason>`
line for every field it could not fill in on this run.

### LAN

The `lan` scenario is self-contained: it starts two devices in one process
over loopback and needs nothing beyond the defaults.

```console
$ go run ./cmd/syncthis-bench --scenario lan --name baseline
bench/results/v1/20260922T135004Z-lan-baseline.json
scenario lan, name baseline, took 11s
```

Fields a `lan` run fills in: `convergence_time_ns`, `bytes_transferred`,
and, on Linux and macOS, `cpu_seconds` and `peak_memory_bytes`. `disk_writes`
is read from `/proc/self/io` on Linux; it is a real number, but a temporary
directory backed by tmpfs can report zero because nothing was ever written to
a block device.

`recovery_time_ns` means the time from restarting a killed daemon until the
folder converges again, measured only when the kill actually landed on a
daemon with a transfer in progress. After the mutation set, the run polls b
for up to 10s for `status.Transfers.Active > 0`; only once that is true does
it kill b, restart it, and start the recovery clock. When nothing is caught
moving within that window — a plausible outcome for a small corpus, since
loopback can finish a mutation's transfer before the daemon is even killed —
no daemon is killed, `recovery_time_ns` stays `0`, and the run prints an
`unmeasured: recovery_time_ns: ...` line explaining why. A `0` without that
line, or a positive number without a kill having actually hit a transfer in
flight, would be a plain restart mislabeled as a recovery; the runner never
reports one.

### Tailscale direct and Tailscale DERP

These scenarios measure two machines. The command drives the local side in
this process and the peer machine over `ssh`, so both sides are observable and
convergence, recovery and the network path are real measurements rather than
two results an operator compares by hand.

```console
$ go run ./cmd/syncthis-bench --scenario tailscale-direct --name two-sided \
    --peer-host macpro-nixos \
    --peer-dir '~/syncthis-bench-remote' \
    --corpus-dir bench/corpus/v1 --results-dir bench/results/v1
bench/results/v1/20260922T145929Z-tailscale-direct-two-sided.json
scenario tailscale-direct, name two-sided, took 19s
unmeasured: bytes_transferred: every byte moved from this machine to macpro-nixos, ...
```

The operator gives three things: the scenario, the peer host and the peer
directory. `--interface` names the interface both daemons bind to and defaults
to `tailscale0`; `--local-data-dir` is optional and defaults to a temporary
directory removed when the run ends.

#### What the operator prepares

- Both machines joined to the same tailnet, `tailscale` on `PATH` on both.
- `ssh <peer-host>` works from this machine with no passphrase and no prompt:
  the runner uses `BatchMode=yes` and fails rather than waiting for one.
- `--peer-dir` names one directory on the peer machine. The run creates
  nothing outside it: the binary lands in `<dir>/bin/syncthis`, the data
  directory in `<dir>/data`, the folder root in `<dir>/root`, and the daemon's
  pid and log in `<dir>/daemon.pid` and `<dir>/daemon.log`. A `--peer-dir` of
  `/`, `~`, or one holding `..` is refused.
- Nothing else. The runner builds and copies the binary, initializes the data
  directory, adds the folder, pairs the two devices, starts the peer's daemon
  on the tailnet interface and stops it again, including when the run fails
  partway through.

Every run clears the peer's state first: it deletes `<dir>/root`,
`<dir>/data` and `<dir>/daemon.pid` and recreates the directories empty, so
the peer starts each run with no folder content, a fresh identity and no stale
process id. A `--peer-dir` may therefore be reused between runs. Nothing else
on the peer machine is deleted: the three paths sit under the directory the
operator named, and a directory resolving to less than two levels below the
root of the peer machine is refused before a run removes anything. The binary
in `<dir>/bin` is kept and overwritten, and so is anything else the operator
keeps in `<dir>`.

Without this a run would inherit whatever an earlier run left in the peer's
folder root: the peer serves that content back, this machine pulls it, and the
recorded convergence time and byte count describe data that is not the corpus
under test.

The DERP scenario is the same command with `--scenario tailscale-derp`, run
with direct connections blocked between the two machines, per the setup
section above. Nothing else changes: the run reports the path it actually
observed, so a DERP run that quietly held a direct connection is visible in
`network_path` rather than assumed away.

#### The static build requirement

The binary copied to the peer is built with `CGO_ENABLED=0`, the same flags
`make release` uses. A dynamically linked build made on one NixOS machine
names its interpreter by an absolute `/nix/store` path that does not exist on
the other machine, and a copy of it dies with exit 127 before printing
anything. The runner also reads `uname -s -m` on the peer and builds for that
platform, so a peer of another architecture gets a binary that runs.

#### Fields these scenarios fill in

- `convergence_time_ns`: real. The run polls `status --json` on both machines
  until neither has a pending change or a transfer in flight and both report
  the same file count, then compares the two folder roots file by file by
  SHA-256 hash. Only matching hashes count as a convergence, so two different
  trees with the same file count can never be recorded as converged.
- `recovery_time_ns`: the time from restarting the peer's daemon until the
  folders converge again, measured only when the peer was caught with
  `transfers.active > 0` after the mutation set. When nothing is caught moving
  within 30s, no daemon is killed, the field stays `0`, and an
  `unmeasured: recovery_time_ns: ...` line says why. This is the same rule the
  `lan` scenario follows.
- `network_path`: `direct` when both machines reported a non-empty `CurAddr`
  for each other in `tailscale status --json` *while a transfer was in
  flight*, `relay:<region>` when they fell back to a relay. The reading is
  taken during the transfer on purpose: the same field read after it is empty
  and proves nothing. A run that could not read it while the connection was
  live leaves it empty with an `unmeasured` line.
- `cpu_seconds`, `peak_memory_bytes`, `disk_writes`: the local process only.
- `bytes_transferred` is normally `0` with an `unmeasured` line: the counter
  sums transfers that completed on this machine, the corpus and the mutations
  start here and land on the peer, and the peer's journal drops a transfer as
  soon as its file is installed, so there is nothing left to count once the
  run has converged.

A machine without a running, reachable Tailscale client, or a peer host that
does not answer ssh, is refused with an actionable message rather than
producing a result: this runner never fakes a Tailscale measurement.

## The scale suite

The benchmark scenarios above measure convergence and throughput against a
corpus. The scale suite measures a different set of claims: the ones about
size and resource use that the task list states as absolutes, such as "a
100,000-file folder reconciles safely" and "idle memory under 100 MB". They
are tests rather than benchmarks, because each one has a pass or fail
condition, and they live in `internal/e2e` alongside the tests they share a
harness with.

They are opt-in the same way the container suite is. Without
`SYNCTHIS_SCALE=1` every one of them skips, so `make test` compiles them and
pays nothing else for them.

```console
$ make scale
```

Every size and count is a parameter, so a machine with less disk or less wall
time runs a smaller version of the same measurement and reports the number it
really ran:

```console
$ make scale SCALE_CYCLES=100 SCALE_TRANSFER_GIB=2 SCALE_FILES_EACH=100
```

| Variable | Default | What it sets |
| --- | --- | --- |
| `SCALE_DIR` | `.scale` | Where fixtures, folder roots and staged files go |
| `SCALE_FILES_EACH` | `500` | Files per directory; 200 directories, so 100,000 files |
| `SCALE_IDLE_SECONDS` | `300` | How long an idle daemon is watched |
| `SCALE_TRANSFER_GIB` | `10` | Size of the single-file transfer |
| `SCALE_CYCLES` | `1000` | Kill-and-restart cycles |
| `SCALE_RATE_SECONDS` | `120` | Length of the bandwidth measurement window |

`SCALE_DIR` must be on a filesystem with room for about three times
`SCALE_TRANSFER_GIB`: the sender, the receiver and the staged copy each hold
the file at once. That rules out a `/tmp` backed by tmpfs, which is why the
default is a directory inside the checkout. The transfer test reads the free
space before it starts and skips with both numbers rather than failing on a
write error halfway through.

### Measured results

Machine: `icebox`, AMD Ryzen 7 9700X (8 cores, 16 threads), 60 GiB RAM, NVMe
(`/dev/nvme0n1p2`, 190 GiB free at the time of the run), NixOS, Linux 7.0.3
x86_64, Go 1.26.7. Date: 2026-09-22. Commit: `f108c41`. Run with `make scale`
at the default parameters unless a row says otherwise.

Memory is resident set size read from `/proc/<pid>/status`. There is no
portable way to read another process's RSS from the standard library, so on a
platform that does not provide `/proc` these tests skip with that as the
reason rather than passing.

| Claim | Measured | Verdict |
| --- | --- | --- |
| 100,000-file folder reconciles safely | see below | pass |
| Idle memory under 100 MB | 148.8 MiB settled over 100,000 files | **fail** |
| 10 GiB transfer with bounded memory | 25.2 MiB peak receiver RSS | pass |
| 1,000 interruption cycles without corruption | see below | pass |
| Bandwidth within 5% of the limit | -0.13% over 1m44s | pass |

#### 100,000-file reconcile

200 directories of 500 files, 15.1 MiB in total.

```
built 100000 files in 200 directories (15.1 MiB) in 868ms
scan: 100200 entries, 100000 hashed, 100200 changes, 0 skipped, took 3.53s
scan: 100202 entries,  11000 hashed,  12002 changes, 0 skipped, took 908ms
scan: 100202 entries,      0 hashed,      0 changes, 0 skipped, took 504ms
reconciled 100000 files: 10000 modified, 1000 deleted, 1000 added
```

The three scans are the first record, the reconcile of a mutated tree, and a
repeat that must find nothing. What the test asserts is not that the scans
finished: after each one it compares the stored manifest against the bytes the
test wrote, entry by entry, and checks that `FolderStats` reports exactly
100,000 file rows, 200 directory rows and the right total size. The reconcile
also has to classify the mutation exactly: 10,000 modifications, 1,000
deletions, 1,000 creations and no renames.

Consecutive revisions of a fixture file always differ in length, and no two
files ever hold the same bytes. Without the first property a rescan can miss a
change whatever the filesystem reports for the modification time; without the
second the scanner correctly pairs a deletion with a creation and reports a
rename, which would make the expected counts wrong.

#### Idle memory: the 100 MB claim does not hold at 100,000 files

This is the one failing measurement. A daemon over a 100,000-file folder, left
idle for five minutes after it reported the folder scanned and no change
pending, settles at **148.8 MiB** resident. The limit is 100 MiB.

```
peak resident memory including the startup scan: 174.0 MiB
idle over 5m0s: 600 samples, first 159.8 MiB, highest 160.0 MiB,
  settled 148.8 MiB (last quarter), last 148.8 MiB, limit 100.0 MiB
```

The settled figure is the highest reading over the last quarter of the window,
not a single sample and not the reading right after the scan. A five-minute
window is long enough for the Go scavenger to have run several times; it
returned about 11 MiB over that window and then stopped, so the remaining
149 MiB is memory the daemon holds, not memory the runtime has yet to give
back.

Resident memory grows linearly with the file count, measured over a
two-minute window at four sizes:

| Files | Settled RSS | Bytes per file |
| --- | --- | --- |
| 10,000 | 36.8 MiB | 3857 |
| 30,000 | 64.5 MiB | 2254 |
| 50,000 | 93.7 MiB | 1966 |
| 100,000 | 161.0 MiB | 1688 |

The marginal cost is about 1.45 KiB per file on top of roughly 22 MiB of fixed
overhead, which puts the 100 MiB crossing at somewhere near 54,000 files. The
claim holds comfortably for a folder of tens of thousands of files and does
not hold at 100,000. The task list line stays open.

The test asserts 100 MiB and therefore fails today. That is deliberate: the
limit under test is the claim, and lowering it to whatever this machine
happens to measure would turn the test into a description of the current
behaviour instead of a check on it.

#### 10 GiB transfer

One 10 GiB file of random content between two daemon processes, 1 MiB chunks.

```
staged big.bin of 10.00 GiB in 23.5s
10.00 GiB arrived in 1m17.7s (131.8 MiB/s)
peak resident memory while transferring:
  sender 20.6 MiB over 311 samples, receiver 25.2 MiB over 311 samples
kernel-recorded peak (VmHWM): sender 23.0 MiB, receiver 25.7 MiB
```

The file that arrived was hashed in full and matched what was sent. Memory is
sampled every 250 ms for the whole transfer and cross-checked against the
kernel's own high-water mark, so a spike between two samples cannot be missed.

The asserted bound is 512 MiB, and the reason it is 512 MiB rather than the
30 MiB actually measured: the transfer is chunked, so a daemon needs a small
multiple of the chunk size plus its database cache, and that must not grow
with the file. 512 MiB is two orders of magnitude below the 10 GiB being
moved, so any implementation that buffered the file, or even a twentieth of
it, fails the test; and it is far enough above what a chunked implementation
needs that a pass is not a measurement of how much memory the Go runtime
happened to keep on the day. Tightening the bound to the measured 25 MiB would
make the test fail on a slower disk or a busier machine for no reason worth
acting on.

Peak RSS is 25.2 MiB moving 10 GiB: memory is flat in the size of the file,
which is the claim.

#### 1,000 interruption and restart cycles

Each cycle publishes a new file of about 4 MiB to the sender with a 64 KiB
chunk size, waits until the receiver has a partial download on disk, sends it
`SIGKILL`, restarts it, and waits for the file to arrive and hash correctly.

```
1000 kill-and-restart cycles in 8m20.6s (500ms per cycle);
  1000 of them killed a daemon with a partial download on disk
1 of 1000 cycles left an orphaned partial file behind
  (no journal row refers to it, the tree is intact)
```

All 1,000 kills landed on a transfer in progress; a cycle that killed a daemon
between transfers would prove nothing and the test counts them separately and
fails if none was caught. After the last cycle the two folder trees were
compared file by file, the journal was empty with no failed rows, and
`repair --dry-run` found nothing unrecoverable.

Two findings came out of this run.

The cycle files must differ in length from one cycle to the next. With a
constant 4 MiB, one run in 1,000 stalled at cycle 444 with both daemons idle
and the previous cycle's bytes on disk: two cycles finished within a second of
each other and wrote the same path, so the only thing distinguishing them was
the modification time, and a rescan reading that at second granularity saw an
unchanged file. That is a property of the fixture rather than of an
interruption, and the fixture now rotates through 16 lengths to remove it. It
is worth recording because the same shape would affect any real folder whose
files are rewritten at the same size faster than the timestamp resolution.

Repeated `SIGKILL` can orphan a partial file. The receiver creates the
`.part` file before it saves the journal row that names it (`newTemp` then
`SaveTransfer` in `internal/app/syncer/puller.go`), so a kill in that window
leaves a file under `.syncthis/tmp` that no row refers to and nothing later
deletes. One run of 1,000 cycles left one such file, zero bytes long. It is
wasted disk, not corruption: the installed tree matched the sender hash for
hash and the journal was empty. The cycle test reports the count instead of
failing on it, and says so in a comment; every other test in the package,
including the 10 GiB one, still fails outright on a leftover partial file, so
the leniency applies only where repeated `SIGKILL` is the deliberate
condition.

#### Bandwidth within 5%

A 4 MiB/s download limit, measured by following the size of the partial
download, which is the count of bytes received and verified and so exactly
what the limiter paces.

```
measured 419.0 MiB over 1m44.9s (481 samples):
  4188647 B/s = 4.0 MiB/s, limit 4.0 MiB/s, deviation -0.13%
```

The window is what makes this a real measurement. The existing
`TestDownloadLimit_...` in `internal/e2e` moves 20 MiB at 4 MiB/s, which takes
five seconds, and a 5% assertion over five seconds is an assertion about
scheduler noise rather than about the limiter. This test transfers for
`SCALE_RATE_SECONDS` (120 by default) and measures the rate over everything
after the first 15 seconds, dropping that warm-up because the token bucket
starts with a burst and the first chunk requests arrive before the pacing
settles. It also refuses to report a rate measured over less than half the
requested window. The measured window here was 1m44.9s, and the sustained rate
was 0.13% below the configured limit.
