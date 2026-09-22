# Syncthis plan: Docker multi-device end-to-end tests

## 1. Decisions

| Decision | Choice | Why |
|---|---|---|
| Driver | Go test package `internal/e2edocker` driving the `docker` CLI via `os/exec` | Scenarios need per-test topologies, mid-test partitions, kills and polling; compose is static YAML and would need a second language (shell) for the actions. The Docker SDK (`github.com/docker/docker`) would add dozens of indirect modules to `go.mod`; the CLI is already present on dev machines and every CI runner. Podman is a fallback via `SYNCTHIS_E2E_DOCKER_BIN=podman` (same CLI surface for the subcommands used). |
| Gate | Skipped unless `SYNCTHIS_E2E_DOCKER=1`; all code in `_test.go` files | Same pattern as `internal/e2e`: a test-only package nothing can import. `go test ./...` and `go vet ./...` compile it in milliseconds and skip. No build tag, so `gofmt`/`vet`/golangci-lint still see the code. |
| Container per device | One container per device, PID 1 = `sleep infinity`, daemon started with `docker exec -d`, CLI commands with `docker exec` | Lets the suite kill the daemon process alone (`kill -9 <daemon.pid>`) or the whole container (`docker kill`), restart either, and run `status --json` after a crash without a running daemon. A daemon-as-PID1 design cannot exec `status` into a dead container. |
| Image | Multi-stage: `golang:1.26-alpine` builds a static `CGO_ENABLED=0` binary; runtime `alpine:3.21` with `iproute2` and `iptables` | Static binary is possible (modernc sqlite and blake3 are pure Go). Alpine over distroless because the harness needs `sh`, `kill`, `tc`, `iptables`, `chown`; ~15 MB. |
| Network | One user-defined bridge per cluster with a fixed `/24` and static IPs (`172.30.<k>.10+i`); `NET_ADMIN` added to every container | Static IPs make peer addresses deterministic and survive `docker network disconnect/connect` and restarts. Pairwise partition uses `iptables -I INPUT -s <ip> -j DROP` on both sides (needs `NET_ADMIN`, which netem needs anyway). Full isolation uses `docker network disconnect` (no capability). Latency/loss/rate use `tc qdisc ... netem`. |
| Listen address | `config.json` `listen: "0.0.0.0:22000"`, daemon run with `--allow-public` for most tests; the star-convergence test uses `--interface eth0` instead | Exercises both binding paths; port 22000 is fixed in every container, no host publishing. |
| Storage | Bind mounts under `t.TempDir()`: `<tmp>/<device>/data` -> `/data`, `<tmp>/<device>/docs` -> `/docs` (plus `/extra`) | Host can hash trees directly (reusing the tree package), read `daemon.log`, and inspect `.syncthis/tmp`. Identity, DB and pid file persist across `docker kill`/`docker start` and across daemon re-exec. A named-volume variant is not needed. |
| Pairing | `init --name <d> --json` on each device; `peer add <ip>:22000 --device-id <id> --json` on each side of each edge, after all daemons are up | Matches the documented pairing walkthrough; `--device-id` pins the identity so no prompt. Topologies are edge lists (`Mesh(n)`, `Star(hub)`). |
| Tree comparison | Move `readTree`/`treeDiff` into `internal/testutil/tree` (non-test package, importable by both `internal/e2e` and `internal/e2edocker`), plus an arch rule that only `_test.go` files import `internal/testutil/...` | "Reuse the invariant checker where possible" without duplicating 150 lines. |
| Clock skew | Simulated with mtimes (`touch -d '@<future>'` inside a container) and by asserting conflict-copy names and convergence are unaffected; documented as a limitation | See the code facts above. A `SYNCTHIS_FAKE_NOW` hook in `sysclock` is rejected: test code would leak into production. |
| Artifacts | On failure or `SYNCTHIS_E2E_KEEP=1`, per-device `daemon.log`, `status --json`, `transfer list --json`, `conflict list --json`, `docker inspect`, `iptables -S`, `tc qdisc show` are written to `$SYNCTHIS_E2E_ARTIFACTS` (default `bin/e2e-artifacts/<TestName>/<device>/`) | `bin/` is already gitignored; CI uploads the directory. |
| Cleanup | Every container and network carries label `syncthis.e2e.run=<runid>`; `t.Cleanup` removes them with `docker rm -f` and `docker network rm`; `make e2e-docker-clean` removes anything with label `syncthis.e2e` from aborted runs | A `go test` killed by Ctrl-C does not run cleanups. |

Out of scope: compose file, Kubernetes, Windows/macOS containers, a TUI, a slow-filesystem device (needs FUSE; `--device-read-bps` does not apply to tmpfs/bind mounts). Replace the in-process suite: no; `make e2e` and `make test` are untouched.

## 2. File list

```
internal/testutil/tree/tree.go                 create: Entry, Read(root) (map[string]Entry, error), Diff(want, got) []string, Hash(path)
internal/e2e/harness_test.go                   modify: delete readTree/treeEntry/treeDiff/absDuration; call tree.Read/tree.Diff
internal/arch/deps_test.go                     modify: add TestTestutilIsImportedOnlyByTests
internal/e2edocker/Dockerfile                  create: multi-stage image
internal/e2edocker/doc_test.go                 create: package doc, env gate (TestMain), constants and timings
internal/e2edocker/docker_test.go              create: thin docker CLI wrapper (run, exec, kill, start, network, cp, inspect, logs)
internal/e2edocker/cluster_test.go             create: Cluster, Device, Start, topology, pairing, cleanup, artifacts
internal/e2edocker/net_test.go                 create: Partition/Heal/Isolate/Rejoin/Netem/ClearNetem
internal/e2edocker/wait_test.go                create: WaitConverged, WaitPeersConnected, WaitJournalEmpty, Settle, checkInvariants
internal/e2edocker/convergence_test.go         create: three-device mesh/star, concurrent edits, 3-writer conflict, folder added live, peer removed/re-added
internal/e2edocker/partition_test.go           create: partition then heal, long offline, netem latency/loss
internal/e2edocker/crash_test.go               create: kill -9 sender/receiver mid-transfer, restart storm, container kill
internal/e2edocker/modes_test.go               create: send-only hub + receive-only spokes, delete-policy never spoke, read-only spoke, different UIDs
internal/e2edocker/scale_test.go               create: 10,000 files, large file with netem rate + kill
internal/e2edocker/clock_test.go               create: mtime skew
internal/e2edocker/soak_test.go                create: TestSoak with SYNCTHIS_E2E_SOAK_MINUTES
.dockerignore                                  create: bin/, .direnv/, .git/, docs/, .specs/, **/*_test.go excluded? (no: keep tests out of the image by copying only what `go build` needs)
Makefile                                       modify: e2e-docker-image, e2e-docker, e2e-docker-soak, e2e-docker-clean
.github/workflows/e2e-docker.yml               create (optional; there is no CI today)
docs/ARCHITECTURE.md                           modify: add internal/testutil and internal/e2edocker rows to the layer table
README.md                                      modify: one paragraph under Build about `make e2e-docker`
.specs/tasks.md                                modify: tick "Support three or more devices per folder" only if the three-device tests pass unchanged; otherwise file the failures
```

## 3. Dockerfile sketch

`internal/e2edocker/Dockerfile`, built with context at the repo root:

```dockerfile
# syntax=docker/dockerfile:1
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY cmd ./cmd
COPY internal ./internal
ARG VERSION=e2e
ARG COMMIT=e2e
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath \
      -ldflags "-s -w -X github.com/wesbragagt/syncthis/internal/cli.Version=${VERSION} -X github.com/wesbragagt/syncthis/internal/cli.Commit=${COMMIT}" \
      -o /out/syncthis ./cmd/syncthis

FROM alpine:3.21
RUN apk add --no-cache iproute2 iptables coreutils
COPY --from=build /out/syncthis /usr/local/bin/syncthis
ENV SYNCTHIS_DATA_DIR=/data
VOLUME ["/data", "/docs"]
EXPOSE 22000
ENTRYPOINT ["sleep", "infinity"]
```

`coreutils` gives `touch -d` for the clock test; busybox `sleep infinity` works. The root `.dockerignore` lists `bin/`, `.direnv/`, `.git/`, `docs/`, `.specs/`, `*.md`.

Container creation (from the harness):

```
docker run -d --name syncthis-e2e-<run>-<dev> \
  --label syncthis.e2e=1 --label syncthis.e2e.run=<run> \
  --network syncthis-e2e-<run> --ip 172.30.<k>.<10+i> \
  --cap-add NET_ADMIN [--user UID:GID] [--read-only --tmpfs /tmp] \
  -v <tmp>/<dev>/data:/data -v <tmp>/<dev>/docs:/docs[:ro] \
  <image>
```

Daemon start (from the harness):

```
docker exec -d <ctr> sh -c 'exec syncthis --data-dir /data --log-level debug --log-format json daemon run --allow-public >>/data/daemon.stdout 2>>/data/daemon.log'
```

## 4. Harness Go API (`internal/e2edocker`, all `_test.go`)

```go
// doc_test.go
func TestMain(m *testing.M)                       // skips whole package unless SYNCTHIS_E2E_DOCKER=1; verifies `docker version` and the image exist
var (
    image        = envOr("SYNCTHIS_E2E_IMAGE", "syncthis-e2e:dev")
    dockerBin    = envOr("SYNCTHIS_E2E_DOCKER_BIN", "docker")
    timeoutScale = envScale("SYNCTHIS_E2E_TIMEOUT_SCALE")  // same semantics as internal/e2e
    convergeTimeout = 120 * time.Second * timeoutScale
    daemonTimeout   = 30 * time.Second * timeoutScale
    settle          = 3 * time.Second * timeoutScale
    pollInterval    = 250 * time.Millisecond
)
const (
    folderID   = "docs"
    extraID    = "extra"
    chunkSize  = 64 << 10        // config.json chunk_size; small so a kill lands mid-file
    rescanEvery = 2 * time.Second
    syncEvery   = time.Second
    peerPort    = 22000
)

// docker_test.go — thin wrapper, every call logs the command with t.Logf
type docker struct{ t *testing.T; bin string }
func (d docker) run(ctx context.Context, args ...string) (stdout, stderr string, code int, err error)
func (d docker) mustRun(args ...string) string                // t.Fatalf on non-zero
func (d docker) exec(ctr string, user string, args ...string) (stdout, stderr string, code int)
func (d docker) inspect(ctr, format string) string

// cluster_test.go
type Topology func(n int) [][2]int            // edges; Mesh(n), Star(hub int)(n)
type DeviceSpec struct {
    Name         string
    Mode         string        // sendreceive|sendonly|receiveonly
    DeletePolicy string        // propagate|never
    NoFolder     bool
    ExtraFolder  bool
    UID, GID     int           // 0 = host uid/gid
    ReadOnlyRoot bool          // mount /docs :ro
    ReadOnlyFS   bool          // --read-only container
    BindIface    bool          // daemon run --interface eth0 instead of --allow-public
}
type ClusterOptions struct {
    Devices  []DeviceSpec
    Topology Topology           // default Mesh
    Config   func(*jsonconfig.Config) // tweaks beyond the defaults (chunk/intervals)
}

type Cluster struct {
    T       *testing.T
    RunID   string
    Network string
    Subnet  string
    Devices []*Device
    d       docker
    started time.Time
}
func Start(t *testing.T, n int) *Cluster                        // n sendreceive devices, mesh
func StartWith(t *testing.T, o ClusterOptions) *Cluster         // init, folder add, daemon up, pair per topology, WaitPeersConnected
func (c *Cluster) Device(i int) *Device
func (c *Cluster) Pair(a, b int)                                 // peer add both ways
func (c *Cluster) Unpair(a, b int)                               // peer remove both ways
func (c *Cluster) Partition(a, b int)                            // iptables DROP both directions on both containers
func (c *Cluster) Heal()                                         // flush iptables on every container, rejoin isolated devices, clear netem
func (c *Cluster) Isolate(i int)                                 // docker network disconnect
func (c *Cluster) Rejoin(i int)                                  // docker network connect --ip <same ip>
func (c *Cluster) Netem(i int, spec string)                      // e.g. "delay 100ms 20ms loss 1%" or "rate 50mbit"
func (c *Cluster) ClearNetem(i int)
func (c *Cluster) WaitConverged(devices ...int)                  // tree.Diff empty pairwise against devices[0] until convergeTimeout, then Settle
func (c *Cluster) WaitPeersConnected()                           // status.peers[].connected true on every edge
func (c *Cluster) CheckInvariants(devices ...int)                // trees equal, no .part, transfer list == [], no daemon.log line at level error unless allowlisted
func (c *Cluster) SaveArtifacts(reason string)                   // called from Cleanup when t.Failed() or SYNCTHIS_E2E_KEEP=1
func (c *Cluster) Close()                                        // registered with t.Cleanup

type Device struct {
    Index    int
    Name     string
    ID       string           // device id from init --json
    IP       string
    Ctr      string           // container name
    HostData string           // <tmp>/<name>/data
    HostRoot string           // <tmp>/<name>/docs
    HostExtra string
    Spec     DeviceSpec
    c        *Cluster
}
func (d *Device) Run(args ...string) (stdout string, code int)             // docker exec syncthis --data-dir /data <args>; stderr to t.Logf
func (d *Device) RunOK(args ...string) string                              // fatal on non-zero
func (d *Device) RunJSON(v any, args ...string)                            // appends --json, unmarshals
func (d *Device) Status() status.Report                                    // status --json (works with or without daemon)
func (d *Device) Transfers() []transferRow
func (d *Device) Conflicts() []conflictRow
func (d *Device) StartDaemon()                                             // exec -d; waits for daemon.pid and status.daemon.running
func (d *Device) StopDaemon()                                              // kill -TERM <pid>; waits for pid file to vanish
func (d *Device) KillDaemon()                                              // kill -KILL <pid>
func (d *Device) KillContainer()                                           // docker kill -s KILL; then docker start; daemon is NOT running after
func (d *Device) Restart()                                                 // StopDaemon + StartDaemon
func (d *Device) DaemonPID() int
func (d *Device) WriteFile(rel string, data []byte)                        // host side, into HostRoot, 0644
func (d *Device) WriteFileAs(rel string, data []byte, mode fs.FileMode)    // via docker exec --user for the UID scenario
func (d *Device) Remove(rel string)
func (d *Device) Touch(rel string, at time.Time)                           // docker exec touch -d for the clock test
func (d *Device) Tree() map[string]tree.Entry                              // tree.Read(HostRoot)
func (d *Device) PartFiles() []string
func (d *Device) Logs() string                                             // <HostData>/daemon.log
func (d *Device) LogLines(level string) []slogLine                         // parsed JSON log lines

// helpers shared with internal/e2e by convention (copied, small)
func payload(n int, seed byte) []byte
func waitFor(t *testing.T, what string, timeout time.Duration, cond func() bool)
```

Polling policy: every wait polls at `pollInterval` until its timeout; convergence is followed by `settle` during which trees must stay equal and `transfer list` must stay empty (same idea as `settle` in `internal/e2e/twoway_test.go`). Every timeout is multiplied by `SYNCTHIS_E2E_TIMEOUT_SCALE`. A test that needs a partition or netem calls `c.requireNetAdmin()` which runs `iptables -S` once and `t.Skip`s if the capability is missing.

`tree` package signature (`internal/testutil/tree/tree.go`):

```go
type Entry struct { Path string; Kind manifest.Kind; Size int64; Mode fs.FileMode; Hash string; Target string; ModTime time.Time }
func Read(root string) (map[string]Entry, error)           // skips .syncthis, tolerates ErrNotExist mid-walk
func Diff(want, got map[string]Entry) []string             // sorted problems, same rules as today's treeDiff
```

`internal/e2e/harness_test.go` keeps `readTree(t, root)`/`treeDiff(t, a, b)` as two-line wrappers so no other e2e file changes.

## 5. Scenario catalog

Timing defaults: `chunk_size` 64 KiB, `rescan_interval` 2 s, `sync_interval` 1 s. "Converged" always means: pairwise `tree.Diff` empty across the named devices, held for `settle`, no `.part`, `transfer list --json == []` on every device. Evidence on failure: artifacts as in section 1.

| # | Test | Devices | Steps | Invariants |
|---|---|---|---|---|
| 1 | `TestMesh_ThreeDevicesConverge` | 3 mesh | write 20 files on A, 20 on B, 20 on C (disjoint paths, sizes 0..5 chunks) | converged A,B,C; each device `status.peers` all connected |
| 2 | `TestStar_HubRelaysBetweenSpokes` | 3, star(hub=0), spokes use `BindIface` | spokes B, C are not paired; write on B, wait, write on C | B's file reaches C through A and vice versa; converged; `daemon.listen` on spokes is `172.30.k.x:22000` (interface path) |
| 3 | `TestMesh_ConcurrentDisjointEditsConverge` | 3 mesh | 10 rounds: each device writes/modifies/deletes 5 random distinct paths at the same instant (goroutines), wait converge | converged after each round; `conflict list` empty on all; final tree equals the union of last writes |
| 4 | `TestMesh_ThreeWritersOneConflict` | 3 mesh | converge `report.md`; `Partition` all three pairs; each writes a different version; `Heal` | converged; exactly one file at `report.md` holding the greatest hash; two conflict copies whose hashes are the other two versions, present on all devices; `conflict list` non-empty and identical set of (path, local_hash, remote_hash) up to perspective; zero of the three contents lost |
| 5 | `TestPartition_EditBothSidesThenHeal` | 3 mesh | `Partition(0,1)`, `Partition(0,2)` (A alone vs B,C); A edits `x`, B edits `y`, C edits `x`; `Heal` | converged; `y` from B on all; `x` keep-both with copy; B and C agree during partition |
| 6 | `TestOffline_LongAbsenceCatchesUp` | 3 mesh | `Isolate(2)`; 30 rounds of edits/deletes/renames on A and B with converge(A,B) each; `Rejoin(2)` | C converges to A,B; tombstones applied (deleted files absent on C); no conflicts unless C edited (it did not) |
| 7 | `TestCrash_ReceiverKilledMidTransfer` | 2 mesh (A sendonly, B receiveonly) | A writes 64 MiB; wait until B `transfers.items[0].chunks_done` in (1, total); `B.KillDaemon()`; read `transfer show` via CLI without daemon: `chunks_done` recorded; `B.StartDaemon()` | converged; after restart `chunks_done` never below the recorded value; exactly one `.part` during the transfer and zero after; file hash equals A's |
| 8 | `TestCrash_SenderKilledMidTransfer` | same as 7 | kill A's daemon mid-transfer; restart | B's transfer resumes (`state` returns to active), converged, no conflict recorded (mirrors `TestResume_SenderRestartIsNotAConflict`) |
| 9 | `TestCrash_ContainerKilledDuringWrites` | 3 mesh | while A writes 200 files in a loop, `B.KillContainer()` then `B.StartDaemon()`; repeat 3 times | converged; `repair --dry-run` exits 0 on B after convergence (nothing to fix); DB opens |
| 10 | `TestCrash_RestartStorm` | 3 mesh | 20 iterations: pick a random device, `Restart()` with 0..500 ms between, meanwhile a writer goroutine edits on another device | converged at the end; daemon start never exits 6 (stale pid/socket handling); `daemon.log` has no `level=error` lines other than dial failures to a down peer |
| 11 | `TestClock_MtimeSkewDoesNotDriveDecisions` | 2 mesh | A writes `f`, touch to +2 years; converge; B modifies `f` with mtime -3 years; converge | B's content wins (hash-based); mtime preserved within 1 s per `tree.Diff`; conflict-copy names still parse when a conflict is forced with a skewed side |
| 12 | `TestModes_SendOnlyHubReceiveOnlySpokes` | 3 star, hub sendonly, spokes receiveonly | hub writes; spoke B writes a local file; hub deletes one file | spokes hold everything the hub has; B's local file never reaches hub or C; hub deletion propagates |
| 13 | `TestModes_DeletePolicyNeverOnOneSpoke` | 3 star, spoke C `--delete-policy never` | hub deletes `keep.txt` | B loses it, C keeps it; `sync-plan docs` on C lists `none/delete-policy-never`; B and hub converged; C differs only by `keep.txt` |
| 14 | `TestModes_ReadOnlyRootIsReportedNotFatal` | 3 mesh, C `/docs:ro` | A writes files | A,B converged; C `status.folders[0].state == "error"` with non-null `error`, `transfers.failed > 0` or pending, daemon still running; A and B unaffected |
| 15 | `TestModes_DifferentUIDs` | 3 mesh, UIDs 1000/2000/3000, host dirs chmod 0777 | each writes files with modes 0644, 0755 | converged with mode equality; no `chown` errors in logs; files on host readable |
| 16 | `TestReload_FolderAddedWhileAllRun` | 3 mesh, `NoFolder` on all | daemons up and paired; `folder add /docs --id docs` on each in turn with writes between | converged; no restart (daemon pid unchanged on all) |
| 17 | `TestReload_PeerRemovedAndReAdded` | 3 mesh | `Unpair(0,2)`; A writes; C should get it only via B (still a mesh edge); then `Unpair(1,2)`: C isolated by config; A writes again; `Pair` both back | second write absent on C while unpaired (checked after settle); present after re-pair; `peer test` from A to C exits 7 with `unknown_device` while unpaired |
| 18 | `TestScale_TenThousandSmallFiles` | 3 mesh | generate 10,000 files (100-2,000 bytes) in 100 dirs on A on the host | converged within `10 * convergeTimeout`; `status.folders[0].files == 10000` on all; log throughput (files/s) with `t.Logf` |
| 19 | `TestScale_LargeFileUnderRateLimitWithKill` | 2 mesh | `Netem(1, "rate 50mbit")` on receiver; A writes 512 MiB (`SYNCTHIS_E2E_BIG_MIB` overrides; 2048 when the bandwidth flags of PLAN-2 item 2 exist and netem is replaced by `--download-limit`); kill receiver at ~40%; restart | resumed, converged, hash equal, `chunks_done` monotonic, memory: `docker stats --no-stream` RSS of the receiver below 512 MiB at the sampled points |
| 20 | `TestNetem_LatencyAndLoss` | 3 mesh, `Netem(i, "delay 100ms 20ms loss 1%")` on all | scenario 3 with 3 rounds | converged; no `level=error` lines except retry notices |
| 21 | `TestSoak` | 3 mesh (4 with `SYNCTHIS_E2E_SOAK_DEVICES`) | for `SYNCTHIS_E2E_SOAK_MINUTES`: rounds of random ops (create/modify/delete/rename, 1-3 devices, 10 % same-path collisions) with a chaos action every 5th round (kill daemon, kill container, partition a pair for 10 s, netem for a round); every round ends in `WaitConverged`; a ledger records per round, per path, the set of written hashes | after every round: converged, no `.part`, journal empty, for each collided path the winning content is one of the writers' and every losing content exists as a conflict copy on every device (zero loss); final: `repair --dry-run` exits 0 everywhere; `conflict list --all` counts equal across devices |

Ordering by value for implementation: 1, 3, 4, 7, 8, 5, 6, 10, 12, 13, 16, 17, 9, 14, 15, 18, 19, 20, 11, 21.

## 6. Makefile and CI

```make
.PHONY: e2e-docker-image e2e-docker e2e-docker-soak e2e-docker-clean
E2E_IMAGE ?= syncthis-e2e:dev
MINUTES   ?= 30

e2e-docker-image:
	docker build -f internal/e2edocker/Dockerfile \
	  --build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) -t $(E2E_IMAGE) .

# Each device is a container; the suite is opt-in and never part of `make test`.
e2e-docker: e2e-docker-image
	SYNCTHIS_E2E_DOCKER=1 SYNCTHIS_E2E_IMAGE=$(E2E_IMAGE) \
	  go test ./internal/e2edocker/ -count=1 -p 1 -timeout 90m -run 'Test[^S]' -v

e2e-docker-soak: e2e-docker-image
	SYNCTHIS_E2E_DOCKER=1 SYNCTHIS_E2E_IMAGE=$(E2E_IMAGE) SYNCTHIS_E2E_SOAK_MINUTES=$(MINUTES) \
	  go test ./internal/e2edocker/ -count=1 -run TestSoak -timeout $$(( $(MINUTES) + 30 ))m -v

e2e-docker-clean:
	-docker ps -aq --filter label=syncthis.e2e | xargs -r docker rm -f
	-docker network ls -q --filter label=syncthis.e2e | xargs -r docker network rm
```

`make test` and `make lint` stay as they are; `go test $(go list ./... | grep -v '/internal/e2e$')` will now include `internal/e2edocker`, which compiles and skips in `TestMain` (no docker call is made when the gate is unset).

GitHub Actions sketch (`.github/workflows/e2e-docker.yml`; there is no CI in the repo today, so this is a proposal rather than an edit to an existing pipeline):

```yaml
name: e2e-docker
on: { pull_request: {}, workflow_dispatch: {}, schedule: [{ cron: "0 3 * * *" }] }
jobs:
  e2e-docker:
    runs-on: ubuntu-latest
    timeout-minutes: 120
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with: { go-version-file: go.mod }
      - uses: docker/setup-buildx-action@v3
      - uses: docker/build-push-action@v6
        with: { context: ., file: internal/e2edocker/Dockerfile, load: true, tags: syncthis-e2e:dev,
                cache-from: type=gha, cache-to: type=gha,mode=max }
      - run: make e2e-docker SYNCTHIS_E2E_TIMEOUT_SCALE=3
      - if: failure()
        uses: actions/upload-artifact@v4
        with: { name: e2e-docker-artifacts, path: bin/e2e-artifacts }
      - if: always()
        run: make e2e-docker-clean
  soak:
    if: github.event_name == 'schedule'
    ... same steps, `make e2e-docker-soak MINUTES=60`
```

`ubuntu-latest` runners allow `--cap-add NET_ADMIN`, so netem and iptables work there.

## 7. Dependency rules

- `internal/e2edocker` contains only `_test.go` files and a `Dockerfile`; nothing can import it. It imports `internal/app/status` (for the `Report` type), `internal/adapters/jsonconfig` (to write `config.json`), `internal/testutil/tree`, stdlib including `os/exec`. It is outside the prefixes the arch test governs, so no rule needs relaxing.
- `internal/testutil/tree` is a real package (non-test files) so two test packages can share it. New arch rule `TestTestutilIsImportedOnlyByTests`: every file importing `internal/testutil/...` must have `isTest == true`. It may import `internal/domain/manifest` and `internal/adapters/blake3hash` (both needed by `Read`); it is not an adapter, so `TestAdaptersDoNotImportEachOther` does not apply.
- No new module dependency in `go.mod`.
- Update the layer table in `docs/ARCHITECTURE.md` with two rows: `internal/testutil/...` (test-only helpers; imported only by `_test.go` files) and `internal/e2edocker` (N daemons in containers; opt-in).

## 8. Ordered implementation steps

Each step leaves `make lint && make test` green; steps 3 onward also pass `make e2e-docker` for the tests that exist so far.

1. **Extract the tree package.** Create `internal/testutil/tree/tree.go` from `readTree`/`treeDiff`/`absDuration` in `internal/e2e/harness_test.go`; replace those with wrappers; add `TestTestutilIsImportedOnlyByTests` to `internal/arch/deps_test.go`; document in `docs/ARCHITECTURE.md`. Verify: `make test && make e2e`.
2. **Image and Makefile.** Add `internal/e2edocker/Dockerfile`, `.dockerignore`, the four Makefile targets. Verify: `make e2e-docker-image && docker run --rm syncthis-e2e:dev syncthis version --json` prints the stamped version; `docker run --rm --cap-add NET_ADMIN syncthis-e2e:dev sh -c 'tc qdisc show; iptables -S'` succeeds.
3. **Harness core.** `doc_test.go`, `docker_test.go`, `cluster_test.go`, `wait_test.go` with `Start`, `StartWith`, `Device.Run*`, `WriteFile`, `Tree`, `StartDaemon/StopDaemon/KillDaemon/Restart`, `WaitConverged`, `CheckInvariants`, artifacts, cleanup. First test: `TestMesh_ThreeDevicesConverge`. Verify: `make e2e-docker`; then `SYNCTHIS_E2E_KEEP=1` run and inspect `bin/e2e-artifacts`; `make e2e-docker-clean` leaves `docker ps -a --filter label=syncthis.e2e` empty. Also `SYNCTHIS_E2E_DOCKER= go test ./internal/e2edocker/` reports SKIP in under a second.
4. **Convergence and conflicts.** Scenarios 3 and 4 (`convergence_test.go`). If 4 fails because three-way conflict detection only reasons pairwise, record the failure verbatim in the plan follow-up and mark the test with `t.Skip("known: <issue>")` referencing it rather than weakening the invariant.
5. **Crash tests.** Scenarios 7, 8 (`crash_test.go`); they reuse the `transfers.items` shape from `status --json`.
6. **Network faults.** `net_test.go` (`Partition/Heal/Isolate/Rejoin/Netem`, `requireNetAdmin`) and scenarios 5, 6 (`partition_test.go`).
7. **Restart storm and container kill.** Scenarios 9, 10.
8. **Modes and reload.** Scenarios 12, 13, 16, 17 (`modes_test.go`, additions to `convergence_test.go`); star topology support.
9. **Environment variants.** Scenarios 14 (read-only root), 15 (UIDs). Both need `DeviceSpec` fields and `--user`/`:ro` in container creation.
10. **Scale.** Scenarios 18, 19 (`scale_test.go`), with `SYNCTHIS_E2E_BIG_MIB` and a note to switch 19 to `--download-limit` when PLAN-2 item 2 lands.
11. **Netem and clock.** Scenarios 20, 11.
12. **Soak.** Scenario 21 (`soak_test.go`), `make e2e-docker-soak`. Verify with `MINUTES=5` locally.
13. **CI and docs.** `.github/workflows/e2e-docker.yml`, README paragraph, `docs/ARCHITECTURE.md` row for `internal/e2edocker`, tick or file items in `.specs/tasks.md` Phase 7 ("Support three or more devices per folder", "Verify three-device non-conflicting convergence") based on what passed.

Verification commands for the finished suite:

```
make lint && make test                                   # suite compiles and skips
make e2e                                                 # in-process suite unchanged
make e2e-docker                                          # full container suite
SYNCTHIS_E2E_TIMEOUT_SCALE=3 make e2e-docker             # loaded machine
make e2e-docker-soak MINUTES=5
go test ./internal/arch/ -run TestTestutilIsImportedOnlyByTests
docker ps -a --filter label=syncthis.e2e -q | wc -l      # 0 after a run
```

## 9. Risks and mitigations

| Risk | Mitigation |
|---|---|
| Docker missing or not usable without sudo on a dev machine | `TestMain` checks `docker version` and prints how to enable it; NixOS users need `virtualisation.docker.enable` and membership in `docker` (already true here). `SYNCTHIS_E2E_DOCKER_BIN=podman` for rootless podman (`--cap-add NET_ADMIN` works rootless for iptables only inside user namespaces; netem may not; tests that need it skip via `requireNetAdmin`). |
| `NET_ADMIN` denied in a CI sandbox | `requireNetAdmin()` skips partition/netem tests with a clear reason; convergence, crash, mode and scale tests still run. |
| Timing under CI load | Same `SYNCTHIS_E2E_TIMEOUT_SCALE` contract as the in-process suite; `-p 1`; every wait is a poll with a settle window, never a fixed sleep for correctness. |
| Image rebuild cost | BuildKit cache mounts for `/go/pkg/mod` and the build cache; only `cmd` and `internal` are copied; GHA cache in CI. Rebuild after a code change is a Go incremental build. |
| Static IP subnet collision on a machine running other Docker networks | Subnet chosen as `172.30.<k>.0/24` with `k` from the run id; on `docker network create` failure, retry with the next `k` (up to 10). |
| TLS identity persistence across restarts | Identity, DB and config are on the `/data` bind mount; `KillContainer` + `docker start` keeps the mount and the static IP; the test asserts `device.id` unchanged after restart. |
| Bind mounts and UIDs | Device directories are `chmod 0777` when a non-host UID is used; files are written 0644/0755 so the host can hash them. |
| Real clock skew impossible | Documented; scenario 11 tests mtime skew only. |
| Three-device semantics may expose product bugs | The suite's job; failures are recorded as follow-up items rather than loosened invariants. |
| Suite duration | Full run is dominated by scenarios 18, 19, 6; they honor `-short` (`testing.Short()` reduces counts) so `make e2e-docker` can pass `-short` on PRs and the nightly runs full sizes. |

### Critical Files for Implementation
- /home/wesbragagt/dev/syncthing/internal/e2e/harness_test.go (source of `readTree`/`treeDiff` to extract, timing and polling conventions to mirror)
- /home/wesbragagt/dev/syncthing/internal/arch/deps_test.go (new rule for `internal/testutil`)
- /home/wesbragagt/dev/syncthing/Makefile (new `e2e-docker*` targets)
- /home/wesbragagt/dev/syncthing/internal/adapters/tcpwire/netiface.go (listen-address rules the containers must satisfy: `--allow-public` or `--interface eth0`)
- /home/wesbragagt/dev/syncthing/internal/compose/compose.go (`Init` preserves a pre-written `config.json`, which the bootstrap relies on)
## 10. Known failures

KF-1, KF-2 and KF-3 are fixed; their scenarios run unskipped. What they were
and how they were fixed is `docs/PLAN-3.md`, in one line each:

- **KF-1**, a transfer whose source changed mid-flight stayed `failed`
  forever, so the journal never emptied (`TestMesh_ConcurrentDisjointEditsConverge`,
  `TestNetem_LatencyAndLoss`, `TestPartition_EditBothSidesThenHeal`). A
  superseded transfer now leaves no row, and a `failed` row means an integrity
  failure alone.
- **KF-2**, three-way divergence kept only one of the two losing versions
  (`TestMesh_ThreeWritersOneConflict`). A conflict outcome now carries the
  merge of both vectors, and a copy is named after the version it holds, so
  resolving the pairs in any order ends with the same two copies everywhere.
- **KF-3**, a device rejoining after an absence rolled everyone back
  (`TestOffline_LongAbsenceCatchesUp`,
  `TestOffline_StaleRejoinKeepsTheNewerVersion`). The single `synced_hash`
  baseline is gone; two versions are ordered by their version vectors.

What remains is KF-4 and KF-5. Neither invariant is loosened: the read-only
scenario carries a `t.Skip` that points here, and the scale scenario asserts
everything it can and counts the rest.

### KF-4: a folder whose root cannot be written reports itself idle

Scenario 14 (`TestModes_ReadOnlyRootIsReportedNotFatal`). One device of a
three-device mesh has `/docs` mounted read-only; the other two write five
files.

Expected: the device reports `status.folders[0].state == "error"` with a
non-null `error`, and the work it could not do is somewhere in its journal
(`transfers.failed`, `pending` or `active` above zero).

Observed: the daemon stays up, which is right, and says nothing is wrong. The
folder is `"state": "idle"`, `"error": null`, `"pending_changes": 0`, and
`transfers` is all zeros with an empty `items`. The only trace is in the log,
at WARN, repeated every sync cycle:

```
{"level":"WARN","msg":"plan item not applied","folder":"docs","path":"offered",
 "action":"mkdir","error":"create directory /docs/offered: mkdir /docs/offered: read-only file system"}
{"level":"WARN","msg":"folder sync failed","folder":"docs","peer":"PZKB5FX","error":"context canceled"}
```

So a device that can synchronize nothing at all looks healthy to `status`, and
a user has no way to find out but to read the log. The fix is a decision about
which failures reach the folder state and for how long (one refused item, or
only a whole sync cycle that failed), so it is recorded here rather than
patched. The other two devices converge and are unaffected, which the test
still asserts.

### KF-5: a scan overtaken by the puller is logged as an error and drops the watch

Scenario 18 (`TestScale_TenThousandSmallFiles`) makes this visible every run.
While a device pulls ten thousand files, its own scan loses the race with the
writes the puller is making, and the folder loop reports it:

```
{"level":"ERROR","msg":"scan folder","folder":"docs",
 "error":"folder \"docs\" at sequence 4713, scan started from 4708: manifest changed since it was loaded",
 "paths":[".syncthis","d00", … ]}
```

Eight such lines in one run of the scenario, on both receivers. The sync is
unaffected: all three devices converge, the journals empty and the file counts
agree. Two things are still wrong with it. The level is ERROR for a lost
optimistic-concurrency race that the next tick retries, which is what an
operator is asked to watch for; and `folderloop.go` treats it like a folder
whose root is gone, calling `dropWatch()`, so the device falls back to
interval scanning until a scan succeeds again.

A fix has to tell `ports.ErrStaleManifest` apart from a scan that really
failed, at the one place that logs both, and decide whether the retry is
immediate. Scenario 18 therefore checks the convergence, the journals, the
partial files and every other error line, and counts these, reporting them
with a pointer here rather than asserting a clean log.

### What the soak leaves out, and why

Scenario 21 (`TestSoak`) runs the rounds and the chaos actions the product
survives today, and asks for the rest with `SYNCTHIS_E2E_SOAK_CHAOS=all`. The
full set is blocked, not unwritten: it is in the test, and every part of it
runs into a known failure above.

| Left out of the default set | Blocked by |
|---|---|
| Rewriting a path (`hash_changed`), deleting one, renaming one (`not_found`) | KF-1, fixed in PLAN-3 |
| Two devices writing the same path in one round | KF-1 and KF-2, both fixed in PLAN-3 |
| Partitioning a pair for ten seconds | KF-1, fixed in PLAN-3 |
| Taking a device off the network and back | KF-3, fixed in PLAN-3 |

The measurements behind that table were taken before PLAN-3: with rewrites in
the set a three-device soak failed at round 11 with one `failed` row left in a
journal, and with deletes and renames as well at round three; the folders were
identical every time and the journal was the only thing wrong. The full chaos
set has not been re-measured since the fixes, so it is still opt-in through
`SYNCTHIS_E2E_SOAK_CHAOS=all` rather than the default.
