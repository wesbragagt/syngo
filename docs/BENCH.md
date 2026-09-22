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

These need two real machines already joined to the same tailnet, set up as
the sections above describe. This command only ever drives the local side of
the pair — there is no way for one process to read a folder on the other
machine — so it needs the peer's address and identity, and the local data
directory to run the daemon against:

```console
$ go run ./cmd/syncthis-bench --scenario tailscale-direct --name baseline \
    --peer-addr 100.64.0.7:22000 \
    --peer-device-id ETI3RRZ... \
    --local-data-dir ~/.local/share/syncthis-bench
```

Run the matching invocation on the peer machine to fill in both sides of a
comparison. A machine without a running, reachable Tailscale client is
refused with an actionable message rather than producing a result: this
runner never fakes a Tailscale measurement.

Fields these scenarios fill in: `bytes_transferred` and, on Linux and macOS,
`cpu_seconds`, `peak_memory_bytes` and `disk_writes`. `convergence_time_ns`
and `recovery_time_ns` are always left at zero with an `unmeasured` line
explaining why: convergence and recovery both need seeing the peer's folder
or restarting the peer's process, and this command only ever runs on one
machine.
