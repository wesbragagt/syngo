# Syncthis benchmarks

This document defines the hardware, the layout and the network scenarios a run
is measured under, and records what the runs produced: the scale suite, the
Phase 5 benchmarks, the chunk size evaluation and the two-machine tailnet
convergence measurement are all below. An item still marked "unmeasured" is a
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
5,000-file folder reconciles safely" and "idle memory under 100 MB". They
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
| `SCALE_FILES_EACH` | `25` | Files per directory; 200 directories, so 5,000 files |
| `SCALE_IDLE_SECONDS` | `300` | How long an idle daemon is watched |
| `SCALE_TRANSFER_GIB` | `10` | Size of the single-file transfer |
| `SCALE_CYCLES` | `1000` | Kill-and-restart cycles |
| `SCALE_MIRROR_MIB` | `2048` | Total size of the vault-sized mirror |
| `SCALE_MIRROR_NOTES` | `4000` | Note-sized files in the mirror, the rest being 32 MiB attachments |
| `SCALE_RATE_SECONDS` | `120` | Length of the bandwidth measurement window |
| `SCALE_CHUNKEVAL_KIB` | `1024,256,64,16` | Average chunk sizes the chunk size sweep measures |
| `SCALE_CHUNKEVAL_MIB` | `1024` | Size of the file the sweep moves at each average |
| `SCALE_CHUNKEVAL_OFFSET_KIB` | `64` | Where the sweep inserts its megabyte, in KiB from the start |

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

The file counts and the mirror size were rescoped on 2026-09-23 to the folder
this program actually carries, a personal notes vault; adr-006 records why and
what each number was before. The rows marked 2026-09-23 were re-measured at the
new scale on the same machine.

| Claim | Measured | Verdict |
| --- | --- | --- |
| 5,000-file folder reconciles safely | see below (2026-09-23) | pass |
| Idle memory under 100 MB over 5,000 files | 32.8 MiB settled (2026-09-23) | pass |
| 2 GiB mirror resumes after restart without a retransfer | 0 payload bytes pulled (2026-09-23) | pass |
| 10 GiB transfer with bounded memory | 25.2 MiB peak receiver RSS | pass |
| 1,000 interruption cycles without corruption | see below | pass |
| Bandwidth within 5% of the limit | -0.13% over 1m44s | pass |

#### 5,000-file reconcile

200 directories of 25 files, 767 KiB in total. Date: 2026-09-23.

```
built 5000 files in 200 directories (767.2 KiB) in 34.263678ms
scan: 5200 entries, 5000 hashed, 5200 changes, 0 skipped, took 275.127424ms
scan: 5202 entries,  550 hashed,  602 changes, 0 skipped, took 66.759627ms
scan: 5202 entries,    0 hashed,    0 changes, 0 skipped, took 25.497576ms
reconciled 5000 files: 500 modified, 50 deleted, 50 added
```

The three scans are the first record, the reconcile of a mutated tree, and a
repeat that must find nothing. What the test asserts is not that the scans
finished: after each one it compares the stored manifest against the bytes the
test wrote, entry by entry, and checks that `FolderStats` reports exactly
5,000 file rows, 200 directory rows and the right total size. The reconcile
also has to classify the mutation exactly: 500 modifications, 50 deletions, 50
creations and no renames.

The same test at the old 100,000-file default, measured 2026-09-22, recorded
the tree in 3.53 s, reconciled 12,002 changes in 908 ms and found nothing on a
repeat in 504 ms. Nothing about that run was wrong; it was measuring a folder
nobody here syncs. `make scale SCALE_FILES_EACH=500` runs it again.

Consecutive revisions of a fixture file always differ in length, and no two
files ever hold the same bytes. Without the first property a rescan can miss a
change whatever the filesystem reports for the modification time; without the
second the scanner correctly pairs a deletion with a creation and reports a
rename, which would make the expected counts wrong.

#### Idle memory: the 100 MB claim holds at 5,000 files

A daemon over a 5,000-file folder, left idle for five minutes after it
reported the folder scanned and no change pending, settles at **32.8 MiB**
resident against a 100 MiB limit. Date: 2026-09-23.

```
peak resident memory including the startup scan: 33.7 MiB
idle over 5m0s: 600 samples, first 33.7 MiB, highest 33.9 MiB,
  settled 32.8 MiB (last quarter), last 25.1 MiB, limit 100.0 MiB (5000 files)
settled 32.8 MiB over 5000 files = 6885 B per file
```

The settled figure is the highest reading over the last quarter of the window,
not a single sample and not the reading right after the scan. The per-file
figure is high here because at 5,000 files the reading is almost all fixed
overhead; the marginal cost is the 1.45 KiB per file measured below.

The same test at 100,000 files, measured 2026-09-22, settled at **148.8 MiB**
and failed:

```
peak resident memory including the startup scan: 174.0 MiB
idle over 5m0s: 600 samples, first 159.8 MiB, highest 160.0 MiB,
  settled 148.8 MiB (last quarter), last 148.8 MiB, limit 100.0 MiB
```

A five-minute window is long enough for the Go scavenger to have run several
times; it returned about 11 MiB over that window and then stopped, so the
remaining 149 MiB was memory the daemon held, not memory the runtime had yet
to give back. That number is real and it is not the claim: adr-006 rescoped
the claim to the file count this program is used at. `make scale
SCALE_FILES_EACH=500` reproduces the failing run.

Resident memory grows linearly with the file count, measured over a
two-minute window at four sizes:

| Files | Settled RSS | Bytes per file |
| --- | --- | --- |
| 10,000 | 36.8 MiB | 3857 |
| 30,000 | 64.5 MiB | 2254 |
| 50,000 | 93.7 MiB | 1966 |
| 100,000 | 161.0 MiB | 1688 |

The marginal cost is about 1.45 KiB per file on top of roughly 22 MiB of fixed
overhead, which puts the 100 MiB crossing at somewhere near 54,000 files. A
vault of 5,000 files sits an order of magnitude below that crossing, which is
why the rescoped claim passes with 67 MiB of headroom rather than passing by a
hair.

The test asserts 100 MiB, not whatever this machine happens to measure. The
limit under test is the claim; lowering it to the measurement would turn the
test into a description of the current behaviour instead of a check on it.

#### 2 GiB mirror, restarted

The Phase 3 claim is that a mirror resumes after a restart instead of pulling
the folder again. The size is the vault-scale target of adr-006: 2 GiB of
notes and attachments, which is an attachment-heavy version of the folder this
program carries, twice over. Date: 2026-09-23.

```
built a vault of 4062 files (4000 notes, 62 attachments of 32.0 MiB)
  totalling 2.00 GiB in 7.842197408s
mirrored 4062 files (2.00 GiB) in 12.081947837s; the receiver pulled
  2.00 GiB from the peer
after restarting both daemons and waiting 1m30s (3 sync intervals of 30s):
  the receiver pulled 0 payload bytes, 0 transfers active, 0 pending
```

Three things make the zero mean something. The first mirror is checked to have
really moved the whole 2.00 GiB over the network, so the fixture cannot have
deduplicated its way to a small transfer. Both daemons are then restarted with
fresh event recorders, so every byte counted afterwards was pulled by the
restarted daemon and not carried over from the first pass. And the wait is
three shipped sync intervals rather than one moment: a retransfer that the
periodic cycle would have started has had the chance to start, three times
over. At the end the receiver's tree still matches the source hash for hash
and no partial download is left behind.

#### 10 GiB transfer

One 10 GiB file of random content between two daemon processes. This run
predates the chunk size change below and used the 1 MiB average that was the
default then; the sweep in "Choosing the default chunk size" repeats the memory
measurement at four averages and finds it flat.

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

## The soak

The task list used to ask for a 30-day soak. adr-006 replaced it with **eight
hours across three devices**: what a soak finds is a function of how many
change-and-break rounds have gone by, and the container soak forces a
partition, a shaped link or a daemon kill every fifth round, which is denser
chaos than a personal tailnet sees in a month.

```console
$ make e2e-docker-soak MINUTES=480
```

**This has not been run.** Nothing in this file reports a soak result, and the
task list line stays open until one exists.

## Convergence trials

The task list used to ask for 99.99% automatic convergence for non-conflicting
changes, two devices and three. adr-006 replaced the rate with a stated number
of real trials: convergence here is decided by version vectors and a total
order on content, both deterministic, so a rate below 100% would mean the
algorithm is wrong rather than that the odds were long. What a trial adds is
evidence that the implementation does not drop an edit or invent a conflict.

These are ordinary tests run repeatedly, not a separate harness.

### Two devices

```console
$ go test ./internal/e2e/ -run 'TestTwoWay_IndependentEditsConverge$' \
    -count=200 -p 1
ok  	github.com/wesbragagt/syncthis/internal/e2e	75.342s
```

**200 of 200 trials converged.** Each trial pairs two devices, converges a
folder, then edits a different path on each device at the same time. The
assertions are that both edits survive with their own content, that no conflict
copy exists, and that neither device recorded a conflict row. Date: 2026-09-23.

### Three devices

```console
$ SYNCTHIS_E2E_DOCKER=1 SYNCTHIS_E2E_IMAGE=syncthis-e2e:dev \
    go test ./internal/e2edocker/ \
    -run 'TestMesh_ConcurrentDisjointEditsConverge$' -count=20 -p 1
ok  	github.com/wesbragagt/syncthis/internal/e2edocker	898.305s
```

**20 of 20 runs converged, over 200 rounds and 3,000 changes.** Each run starts
three containers in a mesh and does ten rounds; in every round all three
devices write five paths apiece at the same instant, some of the writes being
deletes of what an earlier round wrote. Each round must end with the three
devices holding the same tree and no conflict row anywhere, and each run ends
by checking the cluster invariants and that every path holds its last write.
Date: 2026-09-23.

### What this is not

3,000 changes cannot distinguish a fault that happens once in ten thousand. It
is a far smaller sample than a 99.99% claim would need, and it is reported as a
count on purpose. The real safety net is the deterministic algorithm and the
conflict suite that exercises its edges: divergent edits, rename against
delete, case-only renames, stale resolutions and the restore window.

## The Phase 5 benchmarks

Six lines of .specs/tasks.md Phase 5 are claims about what the chunker, the
deduplicator, the rename path and the journal watermarks bought. Each one is
measured here by a test rather than estimated, and each one reports the number
it measured whether or not the claim held.

Two of them cost seconds and run in `make test`
(`internal/e2e/bench_convergence_test.go`). Three cost tens of gigabytes of
disk and minutes of wall time and are opt-in with the rest of the scale suite
(`internal/e2e/bench_scale_test.go`, run by `make scale`, sized by
`SCALE_BIGFILE_GIB`, default 10). The sixth is a corpus run of
`syncthis-bench`.

Every one of them configures the daemon with the timings and the chunk size a
shipped daemon runs at: a 500 ms debounce, a 30 s sync interval, a 10 minute
rescan interval and the shipped average chunk size, which is 64 KiB since the
evaluation recorded below and was 1 MiB before it. The functional tests use far
shorter ones, and a convergence time measured with a 50 ms debounce would be a
measurement of the test harness. The rescan interval being the shipped ten
minutes also means no periodic full scan can carry a change inside any window
measured below: every change travels the watcher, scan, notify and pull path.

### Measured results

Machine: `icebox`, AMD Ryzen 7 9700X (8 cores, 16 threads), 60 GiB RAM, NVMe
(`/dev/nvme0n1p2`, 190 GiB free at the time of the run), NixOS, Linux 7.0.3
x86_64, Go 1.26.7. Date: 2026-09-22. Commits: `949b08e` for the rows measured
before the chunk size change, and the commit that made it for the three rows
re-measured at the 64 KiB default (the insertion, the small edit and the
document save).

| Claim | Measured | Verdict |
| --- | --- | --- |
| Ordinary document change converges under 1 s on a healthy Tailscale direct path | 514 ms one way between icebox and macpro-nixos over a confirmed direct path (2026-09-23); 507.3 ms median over loopback | pass |
| 1 MiB inserted into a 10 GB file transfers less than 2 MiB after caching | 1,254,397 bytes (1.20 MiB) at the 64 KiB default | pass |
| A 10 GB rename transfers zero payload bytes | 0 bytes | pass |
| 5,000-file status under two seconds after incremental change | 15.4 ms slowest, 3.5 ms median (2026-09-23) | pass |
| Small edits use at least 90% fewer bytes than a whole-file transfer | 99.90% fewer | pass |
| Compare against the Phase 0 baseline corpus | +1.0% convergence, same bytes | done, see below |

#### Ordinary document change convergence: loopback only

Ten editor saves of a 24 KiB document into a 200-file folder, timed from the
rename that completes the save to the byte-identical file being readable on
the other device.

```
10 document saves of 24.0 KiB over loopback with the shipped timings
  (debounce 500ms, sync interval 30s, rescan interval 10m0s):
  median 507.303793ms, slowest 509.768434ms
  all [505.602559ms 507.303793ms 508.52687ms 509.768434ms 507.165782ms
       506.915558ms 506.600721ms 507.309034ms 506.802879ms 507.832961ms]
```

Measured at the 64 KiB default. The same benchmark at the old 1 MiB default
had a median of 509.6 ms: a 24 KiB document is one chunk either way.

The spread is 8 ms across ten saves because the number is almost entirely the
500 ms debounce: the watcher reports the save, the folder loop waits for the
burst to end, scans the one path, tells the peer, and the peer pulls 24 KiB.
Everything after the debounce costs about 10 ms.

What this figure on its own does not measure is the claim. The claim names a
healthy Tailscale direct path, which is this path plus a tailnet round trip and
WireGuard encryption on both ends. The two-machine measurement below fills that
in, and is what makes the verdict in the table a pass.

#### Ordinary document change convergence: two machines over Tailscale

Date: 2026-09-23. Machines: `icebox` (AMD Ryzen 7 9700X, NixOS) and
`macpro-nixos`, both on the same tailnet and the same LAN segment. Commit
`bf8e3cb`, built with `make release`.

The two clocks disagree by 38 to 67 ms, and the disagreement is noisy, so a
one-way number taken by comparing timestamps across the two machines would
carry more error than the tailnet hop it is trying to measure. This is
measured as a round trip on one clock instead: `icebox` writes a token into
`ping.md`, a loop on `macpro-nixos` copies whatever token it reads into
`pong.md`, and `icebox` times from its write to reading its own token back.
Both legs are ordinary syncs, so the round trip is two convergences plus the
responder's own poll, and half of it is the one-way figure.

```
8 round trips of a small note, icebox -> macpro-nixos -> icebox:
  round trips: 1029 1028 1032 1026 1029 1027 1026 1029 ms
  median 1028 ms round trip, so about 514 ms one way
  min 1026 ms, max 1032 ms
```

The path was direct, not relayed, confirmed while the daemons were connected:
`syncthis network path` reported `direct` with an endpoint of
`192.168.68.75:41641`, a LAN address, because both machines sit on the same
subnet. `tailscale status --json` agreed: a non-empty `CurAddr`. The `Relay:
"iad"` field on the same peer is its home DERP region, not the path in use.

**The tailnet costs about 7 ms.** Loopback converges in 507 ms and two real
machines in 514 ms, so the claim holds with roughly 486 ms of headroom. Almost
the whole number is the 500 ms debounce: the watcher sees the save and waits
for the burst to end before it scans. A deployment that wants a faster note
sync should shorten the debounce, not blame the network. On a remote network,
add the real wide-area round trip to the 514 ms.

#### A 1 MiB insertion into a 10 GiB file: the 2 MiB budget holds at 64 KiB

```
wrote big.bin of 10.00 GiB in 18.9s
the first copy of 10.00 GiB arrived in 1m4.9s
wrote big.bin of 10.00 GiB in 24.5s
a 1.0 MiB insertion into a 10.00 GiB file: 1.2 MiB (1254397 bytes) moved over
  the network, 10.00 GiB taken from the copy already on disk, in 51.3s
  (budget 2.0 MiB, 2097152 bytes)
```

The insertion costs 1,254,397 bytes against a budget of 2,097,152: 40% under
it. Of the 10.00 GiB the receiver installed, all but 1.2 MiB came off its own
disk through the chunk index, and the file it ended with hashes to what the
sender holds. The same measurement at 1 GiB moves the same 1,254,397 bytes,
which is the point: the cost is a function of the insertion and the chunk
size, not of the file.

At the 1 MiB average this benchmark ran at before, the same measurement moved
2,254,262 bytes and the claim failed. What changed is the default, from 1 MiB
to 64 KiB, and why is the next section: the whole evaluation, both sides of
the tradeoff, at eight averages and six insertion positions.

The first copy of the 10 GiB took 1m4.9s here against 1m3.4s at the old 1 MiB
average, 2% slower, which is the throughput cost of the change on the workload
that has nothing to deduplicate: at ten gigabytes the transfer is bound by the
disk and not by the chunker.

#### A 10 GiB rename: zero payload bytes

```
wrote big.bin of 10.00 GiB in 45.6s
the first copy of 10.00 GiB arrived in 1m31.9s
renaming 10.00 GiB: 0 bytes moved over the network,
  10.00 GiB taken from the copy already on disk, in 58.2s
```

Zero is the literal count of payload bytes the receiver verified for the new
path: every one of the 10.00 GiB came from the copy it already had, and the
renamed file hashes to what the sender holds. The 58 s is disk, not network:
the receiver copies the file into its new name locally.

#### 5,000-file status latency

A daemon over 200 directories of 25 files. Once it reported the folder scanned
and idle, 50 of those files were rewritten and `syncthis status --json` was run
as a real command, repeatedly, from the moment of the change. Date: 2026-09-23.

```
built 5000 files in 200 directories (767.2 KiB) in 34.666281ms
5000 files, 50 of them rewritten: 103 status calls while the daemon took the
  change in, slowest 15.38858ms; the change was reported 524.84124ms after it
  was made; 10 calls afterwards, median 3.471668ms, slowest 3.853732ms
  (target 2s)
```

Two numbers matter and both are far under the two seconds. A status call made
while the daemon is scanning and writing the 50 changed rows answers in at most
15 ms; once the daemon has settled a call costs 3.5 ms, which includes starting
the process and a round trip over the control socket. The change itself was
visible in `status` 525 ms after it was made, almost all of it the 500 ms
debounce.

The same benchmark at the old 100,000-file default, measured 2026-09-22,
answered in at most 302 ms during the change and 24.8 ms once settled, and
reported the change 1.53 s after it was made. Both runs pass; adr-006 records
why the smaller folder is the one the claim is about.

The command is measured rather than an in-process call on purpose: what an
operator waits for includes the process start.

#### Small edits against a whole-file transfer

```
a 4.0 KiB edit to a 64.0 MiB file at the shipped 64.0 KiB chunk size moved
  66.0 KiB over the network, 99.90% fewer bytes than the 64.0 MiB a whole-file
  transfer moves
```

A four kilobyte rewrite in the middle of a 64 MiB file costs one chunk. The
claim is 90% fewer; the measurement is 99.90% fewer, and the saving grows with
the file, because the cost of a small edit is one chunk whatever the file
size. At the old 1 MiB default the same edit moved 1.0 MiB, 98.43% fewer: the
chunk size change made a small edit sixteen times cheaper.

### Choosing the default chunk size

Date: 2026-09-22. Machine as above. Measured by
`TestScale_BenchChunkSizeInsertionSweep` and
`TestScale_BenchChunkSizeTransferCost` in `internal/e2e/bench_chunksize_test.go`,
over a 1 GiB file, run with:

```console
$ make scale SCALE_CHUNKEVAL_KIB=1024,512,256,128,64 SCALE_CHUNKEVAL_MIB=1024 \
    SCALE_CHUNKEVAL_OFFSET_KIB=64
```

The question was whether to lower `DefaultChunkSize` from 1 MiB, make it
adaptive to the file size, or leave it. Both sides of the tradeoff are measured
below, because lowering it to make one benchmark pass would be paid for by
every ordinary transfer that has nothing to deduplicate.

#### What an insertion costs, by average and by position

A 1 MiB insertion into a 1 GiB file, in bytes moved over the network once the
receiver already holds the file. Six insertion positions, because the cost is
the insertion plus the chunks its two ends re-cut, and how many chunks that is
depends on where the boundaries around that position happen to fall. A dash is
a combination that was not run; the four averages that decide the question were
run at every position.

| Average | 64 KiB in | 133.8 MiB | 286.1 MiB | 293.0 MiB | 683.6 MiB | 927.7 MiB | Worst |
| --- | --- | --- | --- | --- | --- | --- | --- |
| 1 MiB | 2,254,262 | 2,459,392 | 3,926,385 | 2,281,798 | 4,009,666 | 3,635,222 | **3,926,385 over** |
| 768 KiB | 2,060,950 | — | — | 1,763,966 | — | — | 2,060,950 |
| 512 KiB | 2,060,950 | 1,681,832 | 1,634,001 | 1,579,092 | 3,474,727 | 1,912,163 | **3,474,727 over** |
| 384 KiB | 1,254,397 | 1,349,821 | — | 3,291,860 | 2,204,934 | — | **3,291,860 over** |
| 256 KiB | 1,254,397 | 1,349,821 | 1,619,081 | 1,149,411 | 2,237,711 | 2,229,008 | **2,237,711 over** |
| 128 KiB | 1,390,521 | 1,197,022 | 2,051,261 | 1,149,411 | 1,608,508 | 1,442,005 | 2,051,261 (2.2% spare) |
| 64 KiB | 1,254,397 | 1,203,371 | 1,454,745 | 1,233,083 | 1,367,899 | 1,163,171 | 1,454,745 (30.6% spare) |
| 32 KiB | 1,216,754 | — | 1,226,120 | 1,107,147 | — | 1,093,967 | 1,226,120 |
| 16 KiB | 1,105,808 | — | 1,089,764 | 1,062,239 | — | 1,094,919 | 1,105,808 |

The budget is 2,097,152. Read down the worst column: 1 MiB is over at every
position measured, by 7% to 91%. 512 KiB, 384 KiB and 256 KiB each hold at
most positions and blow the budget at one or two of them, which is worse than
failing everywhere: a claim that holds unless the edit lands in the wrong place
is not a claim. 128 KiB holds everywhere with 2.2% to spare, which is inside
the run-to-run variation of where a boundary falls. 64 KiB holds everywhere
with at least 30% to spare.

The shape is what FastCDC predicts. An insertion carries its own bytes plus the
chunks it re-cuts, and re-synchronization after an insertion takes one to four
chunks, so the cost is roughly 1 MiB plus one to four averages. The 2 MiB
budget leaves 1 MiB of slack, so four averages have to fit inside a megabyte:
256 KiB is the arithmetic boundary and 64 KiB is the first value with real
headroom. The measurement agrees with the arithmetic, including on which
averages fail only sometimes.

#### What a smaller average costs an ordinary transfer

One 1 GiB file with nothing to deduplicate, between two real daemon processes
over loopback. Time, throughput and peak resident memory are from
`TestScale_BenchChunkSizeTransferCost`; chunk counts and the chunk index from
`TestScale_BenchChunkSizeInsertionSweep`, whose database figure is read with
the database closed so the write-ahead log is checkpointed into the file.

| Average | Chunks per GiB | Wall time | Throughput | Peak RSS sender/receiver | Chunk index | Index per chunk | Index as % of content |
| --- | --- | --- | --- | --- | --- | --- | --- |
| 1 MiB | 913 | 2.24 s | 456.6 MiB/s | 41.1 / 30.3 MiB | 274.9 KiB | 308 B | 0.026% |
| 256 KiB | 3,645 | 2.78 s | 368.9 MiB/s | 28.7 / 27.4 MiB | 1.3 MiB | 362 B | 0.123% |
| 64 KiB | 14,575 | 4.25 s | 240.8 MiB/s | 28.1 / 30.2 MiB | 5.2 MiB | 370 B | 0.503% |
| 16 KiB | 58,210 | 9.94 s | 103.0 MiB/s | 36.6 / 37.4 MiB | 20.9 MiB | 375 B | 2.039% |

Three findings.

Throughput falls, and the fall does not matter where the product runs. 64 KiB
chunks a gibibyte at 240.8 MiB/s against 456.6 MiB/s at 1 MiB, 47% slower.
That ceiling is above a 1 Gbit/s LAN (118 MiB/s) and far above any Tailscale
path, so it binds only on loopback and on a link faster than 2 Gbit/s. The
10 GiB benchmark above is the check on that: at ten gigabytes the transfer is
disk-bound and the change cost it 2%.

The chunk index grows in proportion, and that is the real price. 5.2 MiB per
gibibyte at 64 KiB against 274.9 KiB at 1 MiB, so a 1 TiB folder of large
files costs about 5 GiB of database instead of 200 MiB. Per row the cost is
flat at 308 to 375 bytes whatever the average, so the growth is the row count
and nothing else. A folder of small files pays nothing for the change: a file
below the minimum chunk size is one chunk at any average, so 100,000 small
files is 100,000 rows either way.

Memory does not grow, and at the shipped size it fell. Peak resident memory
over the 1 GiB transfer is 28 to 30 MiB at every average from 1 MiB to 64 KiB;
only 16 KiB raises it, to 37 MiB, which is the cost of many more in-flight
chunk records rather than of buffering. The idle daemon measurement, 30,000
files watched for a minute, settled at 80.3 MiB before the change and 75.6 MiB
after it: the chunker's buffer is the maximum chunk size, four times the
average, so a scan holds 256 KiB per worker instead of 4 MiB. The existing
finding that 100,000 files miss the 100 MiB target is untouched by the chunk
size, because that memory is one record per file.

#### The decision

`DefaultChunkSize` and `chunker.DefaultAvg` are lowered from 1 MiB to 64 KiB.

The alternatives were weighed against the same data:

- **Keep 1 MiB.** Rejected: the claim fails at every insertion position by 7%
  to 91%, and the alternative reading, that the claim should be relaxed to
  match the chunk size, would give up the property the chunker exists for.
- **256 KiB**, the cheapest average that passes at most positions. Rejected:
  it moves 2,237,711 bytes at one of the six positions and 2,229,008 at
  another. A default whose behaviour depends on where a user's edit lands is
  not a default.
- **128 KiB.** Rejected on margin: it holds everywhere measured, but by 2.2% at
  its worst, which is inside the variation between positions.
- **Adaptive to file size.** Rejected on the data, not on principle. The
  workload the budget is about is a small edit inside a large file, so a rule
  of "larger average for larger files" makes exactly the case that fails worse,
  and the opposite rule, a smaller average for large files, is what lowering
  the default already does while costing small files nothing: a file below the
  minimum chunk size is a single chunk at any average. An adaptive rule would
  add a second thing both devices must agree on for deduplication to find
  anything, in exchange for no measured gain.
- **16 KiB**, the value `TestDedup_InsertionMovesOnlyWhatChanged` runs at.
  Rejected: it buys 1,105,808 bytes against 64 KiB's 1,454,745, both far inside
  the budget, and pays 103.0 MiB/s and 2% of the content in index rows for it.

#### Compatibility

This is not a protocol change and needs no version bump. The chunk size is not
on the wire: `ChunkListReply` carries the boundaries the sending side actually
cut, and a `ChunkRequest` names a byte range out of that list, so a receiver
never assumes a size. Version 3 was where deriving boundaries from a size
stopped, as [PROTOCOL.md](PROTOCOL.md) records. Two devices configured with
different averages interoperate today and still do.

What the change does affect is deduplication between devices that disagree.
Each device cuts its own files with its own configured average when it scans,
so a peer announcing 1 MiB boundaries finds nothing in an index built at
64 KiB, and the transfer falls back to moving the bytes. It is a performance
property, not a correctness one, and it applies to any two devices with
different values whether or not the default moves. A device already running
with a written `chunk_size` keeps it; only a `config.json` written from now on
picks 64 KiB. An existing database is not re-cut until the files are rescanned.

### Comparing against the Phase 0 baseline corpus

The Phase 0 corpus is `fixture.DefaultSpec`: 200 small text files, 3 large
files, 5 binary files and a directory chain ten deep, 26 MiB in all, written
from a fixed seed. No corpus tree and no Phase 0 result were ever committed,
and no baseline run had been recorded before this one: everything in this
document above the scale suite described how to run a scenario rather than
what one produced. The corpus is regenerated rather than stored, because the
same seed and the same spec produce the same bytes on any machine;
`bench/corpus/` is therefore ignored by git and `bench/results/` is kept.

With no recorded Phase 0 result to compare against, the comparison made here
is between two revisions of the code over one corpus: `4522f9c`, the last
commit before the Phase 5 chunker landed, and `949b08e`. Both were run with
`syncthis-bench --scenario lan` against the same
`bench/corpus/v1`, three runs each, alternating.

| Run | Convergence | Bytes | Peak RSS |
| --- | --- | --- | --- |
| pre-Phase 5 (`4522f9c`) | 10.24 s, 10.25 s, 10.25 s | 26,270,468 | 44.4, 43.8, 43.0 MiB |
| Phase 5 (`949b08e`) | 10.35 s, 10.35 s, 10.35 s | 26,271,414 / 26,270,468 ×2 | 56.1, 45.2, 47.4 MiB |

Phase 5 converges 100 ms slower on this corpus, 1.0%, and holds a few
megabytes more resident. Neither is surprising and neither is a regression
worth acting on: the corpus is a first full synchronization of files that
share no content, which is the one workload where content-defined chunking
and deduplication have nothing to reuse and only cost. The workloads they were
built for are the four benchmarks above, where they move 0 bytes for a 10 GiB
rename and 1 MiB for an edit to a 64 MiB file.

One limitation of this comparison is worth stating rather than hiding. The
runner's `bytes_transferred` sums the size of each file a node finished
installing, not the payload it pulled over the network, so it cannot show a
deduplication saving at all. The 946 bytes that one run differs by is a file
installed at a different revision of the mutation set, not a payload
difference; runs of the same revision differ by it too. Every payload figure in this document comes from the chunk-level counters in
the benchmarks above, which distinguish a chunk taken from the peer from one
taken off the local disk.
