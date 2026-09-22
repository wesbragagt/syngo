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
