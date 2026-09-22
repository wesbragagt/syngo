# Syncthis benchmarks

Phase 0 baseline: run Syncthis against the corpus before optimizing anything,
so later phases have our own first recorded run to compare against. This
document defines the hardware, the layout, the network scenarios, and the
target metrics. It does not report results yet; every number below marked
"unmeasured" is a placeholder until a run fills it in.

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

Nothing under `bench/corpus/` or `internal/bench/fixture` is built by this
task; see the corpus generator and runner, owned separately.

## Network scenarios

Three scenarios cover the paths Syncthis actually runs on. Each one is a
network setup, not a code path: the same runner and the same corpus run
against all three.

### LAN

What it measures: convergence time and throughput with no NAT traversal and no
relay, the fastest path available. This is the ceiling every other scenario is
compared against.

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
traffic falls back to a DERP relay, the worst realistic path. This bounds the
degradation Syncthis has to tolerate when direct connections are not
available.

Setup: same tailnet as the direct scenario, but with direct connections
forced off (for example by blocking UDP between the two machines at the
firewall) so `tailscale status --json` reports a `relay` value for the peer.

## Target metrics

Absolute targets for Syncthis, taken from the Phase 5 benchmark lines of
`.specs/tasks.md`. These are not comparisons against another product: the
baseline a later run is measured against is Syncthis's own first recorded run
of the corpus, stored in `bench/results/v1/`. The targets below are fixed
regardless of what that first run measures.

| Metric | Target |
|---|---|
| Convergence time for an ordinary document change | under 1 second on a healthy Tailscale direct path |
| Bytes transferred for a 1 MiB insertion into a 10 GB file, after caching | less than 2 MiB |
| Bytes transferred for a 10 GB rename | zero payload bytes |
| Status latency for a 100,000-file folder after an incremental change | under 2 seconds |
| Bytes transferred for small edits vs. whole-file transfer | at least 90% fewer bytes |

These numbers are unmeasured until Syncthis exists to measure; this table
states the targets a Phase 5 run has to hit.
