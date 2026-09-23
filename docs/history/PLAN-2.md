# Syncthis implementation plan, pass 2

> **Historical.** All six items of this pass are built. It records what was
> intended at the time and is not a description of the code. Its line that
> `docs/PROTOCOL.md` stays at version 1 is no longer true: the protocol is at
> version 4. See [PROTOCOL.md](PROTOCOL.md) and [CLI.md](CLI.md).

Module `github.com/wesbragagt/syncthis`, Go 1.26. Baseline: `main` at `ecefb9d`, milestones 1–9 of `docs/PLAN.md` done. Every item below is one commit series that leaves `make lint && make test` green on its own; the order is the dependency order. The rules of `docs/PLAN.md` section 0 hold: `internal/domain` and `internal/app` import no `os`, `net`, `os/exec`, `database/sql`, `syscall`; adapters import only domain, `app/ports` and third-party code; `internal/cli` imports app, domain, compose and cobra; `internal/compose` wires everything.

Out of scope for this pass: benchmarks, hardware tests, TUI, per-folder sync interval (see item 3 for why), a wire protocol change (none of the items needs one; `docs/PROTOCOL.md` stays at version 1).

Item order: 1 Glossary, 2 Bandwidth limits, 3 Sync windows, 4 Config import/export, 5 Tailscale detection, 6 Mid-scan crash test. Items 2 and 3 introduce the settings port and the folder fields that item 4 exports; items 5 and 6 are independent of everything else and come last because they carry the two external dependencies (a `tailscale` binary, a `go` toolchain at test time).

---

## Item 1 — Glossary

**Goal.** One page that defines the domain language, linked from the architecture doc, so later docs and code comments can use the words without redefining them. Every later item adds its new terms here.

**Files.**

```
docs/GLOSSARY.md            create
docs/ARCHITECTURE.md        replace the inline "## Glossary" section with a two-line pointer; keep the "Valid path" text where it is used (move it into GLOSSARY under "Valid path" and leave a link)
README.md                   add the glossary to the docs list (after ARCHITECTURE.md)
.specs/tasks.md             tick "Document the domain language in a glossary."
```

**Content.** One or two sentences per term, in this order, each cross-linked to the doc that specifies it (`CLI.md#…`, `STATE.md#…`, `PROTOCOL.md#…`): device, device ID (with `Short()` and the dashed display form), folder, folder ID, mode (`sendreceive`, `sendonly`, `receiveonly`), policy (delete policy, symlink policy), ignore rules, entry, kind, tombstone, valid path, manifest, sequence, synced hash / synced mode, scan, change (create/modify/delete/rename), plan, action, reason, chunk, transfer, journal, partial file, install, conflict, conflict copy, resolution, stale conflict, peer, pairing, probe, tailnet, MagicDNS, data directory, state directory, control socket, reload, daemon. Items 2–5 add: bandwidth limit, token bucket, sync window, scheduled state, bundle (config export), tailnet peer.

Existing definitions in `docs/ARCHITECTURE.md` lines 60–99 move verbatim; do not reword what is already precise.

**Tests.** None; docs only. `make lint` (gofmt/vet) is unaffected.

**Verification.**

```
grep -c '^\*\*' docs/GLOSSARY.md                       # >= 35 terms
grep -n 'GLOSSARY.md' docs/ARCHITECTURE.md README.md   # both link it
make lint && make test
```

---

## Item 2 — Bandwidth limits

**Goal.** A global upload limit and a global download limit, in bytes per second, `0` = unlimited (the default). Stored in `config.json`, overridable per run with `daemon run --upload-limit/--download-limit`, changeable while the daemon runs with `config set`, applied by a token bucket behind a port. Limits cover chunk payloads only: manifests, hellos, pings and control-socket traffic are not metered (they are small and metering them would slow the very cycle that tells the peer to stop).

**Semantics (decided).**

- One download bucket and one upload bucket per device, shared by every peer connection and every folder. Per-peer or per-folder limits are not in scope.
- Download pacing happens on the request side: the puller waits for `chunk.Length` tokens before it asks for a chunk, so the peer never sends faster than this device admits and nothing buffers. Upload pacing happens in `syncer.Server.Chunk` after the bytes are read and before they are returned, so a slow peer holds only its own connection goroutine.
- Bucket capacity (burst) is 100 ms of traffic at the configured rate (`rate/10`, at least 1 byte). A wait for more bytes than the capacity is granted by letting the level go negative and sleeping off the debt, so a 1 MiB chunk is always admitted at any rate. This is what makes "20 MiB at 4 MiB/s takes about 5 s" true to within the burst (4.9 s of sleeping).
- A rate given as a flag on `daemon run` holds for that run; `config set` rewrites `config.json` and asks the daemon to reload; the daemon re-reads the settings through the settings port and applies the file value only to a limit that was not pinned by a flag. The daemon never reads `config.json` itself; the composition root gives it a `ports.SettingsStore`.
- Rates are parsed by the domain: a plain integer is bytes per second; suffixes `KB`, `MB`, `GB` are decimal, `KiB`, `MiB`, `GiB` binary, a trailing `/s` is accepted and ignored, a decimal mantissa (`1.5MiB`) is accepted, negative or garbage is an error, `0` and `unlimited` mean unlimited. `config.json` stores the integer.

**Files.**

```
internal/domain/bandwidth/rate.go, rate_test.go                       create
internal/app/ports/limiter.go                                          create: Limiter
internal/app/ports/settings.go                                         create: Settings, SettingsStore
internal/app/fakes/limiter.go, settings.go, fakes.go                   create fakes; add compile-time checks
internal/adapters/tokenbucket/limiter.go, limiter_test.go              create
internal/adapters/jsonconfig/config.go                                 add UploadLimit, DownloadLimit; add Store implementing ports.SettingsStore; config_test.go
internal/app/syncer/puller.go                                          PullerConfig.Download; Wait before each chunk request
internal/app/syncer/server.go                                          SetUploadLimiter; Wait before returning chunk data
internal/app/syncer/limit_test.go                                      create
internal/app/daemon/daemon.go, api.go                                  Deps.Settings, Deps.Upload, Deps.Download; applySettings on Run and Reload; limits in live()
internal/app/status/report.go, service.go                              Daemon.UploadLimit/DownloadLimit fields; LiveInfo carries them
internal/compose/compose.go, daemon.go                                 build limiters and settings store; DaemonOptions.UploadLimit/DownloadLimit; overlay; App.Settings, App.Upload, App.Download
internal/cli/daemon.go                                                 --upload-limit, --download-limit
internal/cli/config.go                                                 create: config show, config set
internal/cli/exitcodes.go                                              map bandwidth.ErrInvalidRate -> ExitUsage
internal/cli/config_integration_test.go                                create
internal/cli/cli_json_test.go                                          add config show / config set / config cases
internal/e2e/harness_test.go                                           nodeConfig.ChunkSize, UploadLimit, DownloadLimit
internal/e2e/bandwidth_test.go                                         create
docs/CLI.md, docs/STATE.md, docs/ARCHITECTURE.md, docs/GLOSSARY.md    update
.specs/tasks.md                                                        leave "pause, resume, scheduled sync, and bandwidth limits" unticked until item 3
```

**Key signatures.**

```go
// internal/domain/bandwidth/rate.go
package bandwidth
type Rate int64                              // bytes per second; 0 means unlimited
const Unlimited Rate = 0
var ErrInvalidRate = errors.New("invalid rate")
func ParseRate(s string) (Rate, error)       // "0" | "unlimited" | "4194304" | "4MiB" | "4MiB/s" | "500KB" | "1.5GiB"
func (r Rate) Unlimited() bool
func (r Rate) String() string                // "unlimited" | "4 MiB/s" | "500 kB/s" (binary units when divisible by 1024)
func (r Rate) TimeFor(n int64) time.Duration // how long n bytes take at r; 0 when unlimited

// internal/app/ports/limiter.go
type Limiter interface {
    // Wait blocks until n bytes may pass or ctx ends. n <= 0 returns at once.
    Wait(ctx context.Context, n int) error
    // SetRate changes the limit; Unlimited wakes every waiter at once.
    SetRate(r bandwidth.Rate)
    Rate() bandwidth.Rate
}

// internal/app/ports/settings.go
type Settings struct {
    Listen, Interface string
    ChunkSize         int
    MaxFileSize       int64
    RescanInterval    time.Duration
    SyncInterval      time.Duration
    LogLevel          string
    UploadLimit       bandwidth.Rate
    DownloadLimit     bandwidth.Rate
}
type SettingsStore interface {
    Load(ctx context.Context) (Settings, error)
    Save(ctx context.Context, s Settings) error   // keeps fields the port does not carry (device_name)
}

// internal/adapters/tokenbucket/limiter.go
type Limiter struct { /* mu, rate, burst, tokens float64, last time.Time, now func() time.Time, sleep func(ctx, time.Duration) error, changed chan struct{} */ }
func New(r bandwidth.Rate) *Limiter
// NewWithClock is for tests: sleep receives the delays Wait decides on and never blocks.
func NewWithClock(r bandwidth.Rate, now func() time.Time, sleep func(context.Context, time.Duration) error) *Limiter
// Wait: under the lock refill tokens from `last` to now, cap at burst, subtract n, compute delay = -tokens/rate when negative;
// release the lock; select on sleep(delay) | ctx.Done() | changed (SetRate closed it: recompute with the new rate).

// internal/adapters/jsonconfig/config.go
type Config struct { ...; UploadLimit int64 `json:"upload_limit"`; DownloadLimit int64 `json:"download_limit"` }
type Store struct{ dir string }
func NewStore(dir string) *Store
func (s *Store) Load(ctx context.Context) (ports.Settings, error)   // Load(dir) then toSettings
func (s *Store) Save(ctx context.Context, set ports.Settings) error // Load(dir), overlay, Save(dir, cfg)
func toSettings(c Config) ports.Settings; func applySettings(c *Config, s ports.Settings)

// internal/app/syncer
type PullerConfig struct { ChunkSize, Concurrency int; Download ports.Limiter /* nil: unlimited */ }
func (s *Server) SetUploadLimiter(l ports.Limiter)   // nil: unlimited

// internal/app/daemon
type Deps struct { ...; Settings ports.SettingsStore; Upload, Download ports.Limiter }
func (d *Daemon) applySettings(ctx context.Context) error   // Load -> SetRate on both; logs "bandwidth limits applied" up/down
// Run: after refresh(ctx) and before ready is closed. Reload: first statement.
// live(): LiveInfo.UploadLimit/DownloadLimit = Upload.Rate()/Download.Rate() when non-nil.

// internal/app/status/report.go
type Daemon struct { ...; UploadLimit int64 `json:"upload_limit"`; DownloadLimit int64 `json:"download_limit"` }

// internal/compose
type DaemonOptions struct { ...; UploadLimit, DownloadLimit *bandwidth.Rate }   // nil: from config.json
type pinnedSettings struct { base ports.SettingsStore; upload, download *bandwidth.Rate } // Load overlays the pinned values; Save passes through
```

Puller change in `fetch`, before `p.chunk(...)`:

```go
if p.download != nil {
    if err := p.download.Wait(ctx, chunk.Length); err != nil { return err }
}
```

Server change at the end of `Chunk`, after `ReadAt` and the length check:

```go
if s.upload != nil {
    if err := s.upload.Wait(ctx, len(data)); err != nil { return ports.ChunkData{}, err }
}
```

A cancelled `Wait` returns `ctx.Err()`, which both callers already treat as "connection went away" (not per-file), so the transfer stays resumable.

**CLI surface.**

```
syncthis daemon run [--upload-limit RATE] [--download-limit RATE]      # one-run pin; exit 2 on a bad RATE
syncthis config show                                                   # effective config.json settings
syncthis config set [--upload-limit RATE] [--download-limit RATE]
                    [--rescan-interval DUR] [--sync-interval DUR]
                    [--listen HOST:PORT] [--interface NAME] [--log-level L]
```

`config set` needs at least one flag (exit 2 otherwise), validates each (rate via `bandwidth.ParseRate`, listen via a new `compose.ValidateListen` that forwards to `tcpwire.ValidateListen`, log level against the global list), writes through `app.Settings.Save`, then calls `reloadDaemon`. Limits apply at once; the other keys are documented as "take effect when the daemon next starts". `config show --json` and `config set --json` print:

```json
{
  "listen": "127.0.0.1:22000", "interface": "", "chunk_size": 1048576,
  "max_file_size": 1099511627776, "rescan_interval": "10m0s", "sync_interval": "30s",
  "log_level": "info", "upload_limit": 0, "download_limit": 0
}
```

Text form: `key = value` lines, rates humanized (`upload_limit = 4 MiB/s`, `download_limit = unlimited`). `config` with no subcommand: help, or exit 2 with `--json` (same as `folder`). `status --json` gains `daemon.upload_limit` and `daemon.download_limit` (0 when no daemon or unlimited); `schema_version` stays 1 (fields are only added).

**Exit codes used.** 2 (bad rate, no flags, bad listen), 0, 1.

**Tests.**

- `internal/domain/bandwidth/rate_test.go`: `TestParseRate_Table` (inputs above, plus `"-1"`, `"4 MiB"`, `"MiB"`, `"1e3"` rejected), `TestRate_String`, `TestRate_TimeFor` (4 MiB at 4 MiB/s = 1 s; unlimited = 0).
- `internal/adapters/tokenbucket/limiter_test.go`:
  - `TestWait_20MiBAt4MiBPerSecondSleepsAboutFiveSeconds`: `NewWithClock`, fake clock advanced by exactly what each `sleep` asked; 20 waits of 1 MiB; assert total sleep in `[4.85s, 4.95s]` (deterministic: 20 MiB − 0.4 MiB burst at 4 MiB/s). Same run with 16 KiB chunks (1280 waits) gives the same total within 1 ms.
  - `TestWait_UnlimitedNeverSleeps` (0 → no sleep calls, returns nil).
  - `TestSetRate_ToUnlimitedWakesAWaiter`: real clock, rate 1 B/s, `Wait(1<<20)` in a goroutine, `SetRate(0)` after 20 ms, assert return within 500 ms.
  - `TestSetRate_LowerRateStretchesAPendingWait` (fake clock: first recompute uses the new rate).
  - `TestWait_ContextCancelledReturnsError`.
  - `TestWait_RealClock_LowerBoundHolds`: real clock, 4 MiB/s, 8 waits of 256 KiB (2 MiB) ≥ 400 ms; upper bound 5 s; skipped with `-short`.
- `internal/adapters/jsonconfig/config_test.go`: `TestLoad_MissingLimitsAreUnlimited` (old file without the keys), `TestStore_SaveKeepsDeviceName`, `TestStore_LoadRoundTripsLimits`, `TestLoad_NegativeLimitIsAnError`.
- `internal/app/syncer/limit_test.go`: `TestPull_WaitsForEveryChunkOnTheDownloadLimiter` (fakes.Limiter records `[]int`; sum == file size, count == chunk count, waits happen before the matching `RequestChunk`, checked with a handler that fails when the recorded wait count is behind), `TestChunk_WaitsForItsLengthOnTheUploadLimiter`, `TestPull_CancelledWaitLeavesTransferPending` (fakes.Limiter blocks; cancel; state pending, no `failed` event), `TestPull_NilLimiterNeverWaits`.
- `internal/app/daemon/daemon_test.go`: `TestRun_AppliesBandwidthLimitsFromSettings`, `TestReload_AppliesChangedBandwidthLimits` (fakes.Settings returns 4 MiB/s then 1 MiB/s; fakes.Limiter records `SetRate` calls), `TestStatus_ReportsLimits`.
- `internal/cli/config_integration_test.go`: `TestConfigSet_UploadLimitIsWrittenAndShown` (`config set --upload-limit 4MiB` then `config show --json` → 4194304; `config.json` still has `device_name`), `TestConfigSet_BadRateExit2`, `TestConfigSet_NoFlagsExit2`, `TestConfigSet_BadListenExit2`, `TestDaemonRun_BadLimitExit2`, `TestConfigSet_WhileDaemonRunsIsReflectedInStatus` (startDaemon; `config set --download-limit 1MiB`; `status --json` shows `daemon.download_limit == 1048576` within `convergeTimeout`), `TestDaemonRun_FlagPinsLimitAgainstReload` (daemon run `--upload-limit 2MiB`; `config set --upload-limit 8MiB`; status still 2097152). JSON purity list: `config`, `config show`, `config set --upload-limit 1MiB`, `config set` (exit 2).
- `internal/e2e/bandwidth_test.go` (skipped with `-short`): `TestDownloadLimit_20MiBAt4MiBPerSecondTakesAboutFiveSeconds`: node b `DownloadLimit: 4 MiB/s`, both nodes `ChunkSize: 1 MiB`; a writes one 20 MiB file; measure from `writeFile` to the file being complete on b (`waitPresent` + hash equal); assert `elapsed >= 4.5s` and `elapsed <= 5s + 10s*timeoutScale`. `TestUploadLimit_SenderSideHoldsToo`: same with `UploadLimit` on a and no limit on b. `TestNoLimit_StatusReportsZero`: `status --json` on both nodes shows both limits 0 (the "0 means unlimited" e2e assertion; the timing claim for unlimited is made at the adapter level, where it is deterministic).

**Verification.**

```
make lint && make test
go test ./internal/adapters/tokenbucket/ -race -count=5
go test ./internal/e2e/ -run 'Limit' -count=1 -v
syncthis --data-dir /tmp/b/data config set --download-limit 4MiB && syncthis --data-dir /tmp/b/data status --json | jq .daemon.download_limit
dd if=/dev/urandom of=/tmp/a/docs/big.bin bs=1M count=20; time (until cmp -s /tmp/a/docs/big.bin /tmp/b/docs/big.bin; do sleep 0.2; done)   # about 5-6 s
```

**Docs.** `CLI.md`: `daemon run` flag table, new `config show` / `config set` sections (with the JSON above), `status` fields, a "Bandwidth limits" paragraph stating what is metered and the burst. `STATE.md`: `config.json` gains `upload_limit`, `download_limit` (bytes per second, 0 unlimited). `ARCHITECTURE.md` ports table: `Limiter` / `tokenbucket` ("waiting for a byte budget"), `SettingsStore` / `jsonconfig`. `GLOSSARY.md`: bandwidth limit, token bucket.

---

## Item 3 — Scheduled sync (per-folder sync windows)

**Goal.** A folder may carry sync windows in local time. Outside every window the folder keeps scanning and keeps serving, but this device does not pull for it. `status` says so.

**Semantics (decided).**

- `--sync-window "HH:MM-HH:MM"`, repeatable; the union of the windows is when the folder is open. Empty list means always open. Minutes only. `Start == End` is invalid (exit 2). `Start > End` wraps past midnight (`22:00-06:00`). Start inclusive, end exclusive. Local time is the daemon process's `time.Local`; the domain never touches `time.Local` itself, callers pass `clock.Now()` and the domain reads the wall-clock fields in that time's `Location`, so tests use fixed zones.
- Outside a window the folder scans (watcher and rescans keep the local manifest current; this is disk work, not network work, and a stale manifest would make the first in-window cycle slower and the peers' picture of this device wrong) and serves (whether the other device downloads is that device's decision, exactly as folder modes work; refusing would need a new wire reason and would make every peer log a failed cycle each sync tick for the whole closed period). It does not pull: `peerLoop.syncable` leaves it out and `Puller.SyncFolder` returns an empty summary as a second guard. A transfer in flight when the window closes stops within one chunk, exactly like a pause, and stays `pending` with the error `outside sync window`; the first cycle after the window opens continues it from the journal.
- The window opening is noticed at the next sync tick (`sync_interval`, 30 s default). No extra timer. A window change through `folder set` reloads the daemon, which runs a cycle at once, so removing a window takes effect immediately.
- DST: computations build `time.Date(y, m, d, hh, mm, 0, 0, loc)` and let Go normalize. A start inside a skipped hour opens when the wall clock reaches the normalized time; a window spanning a repeated hour is one hour longer that night. `Contains` compares wall-clock minutes, so nothing depends on offsets.
- No per-folder `--sync-interval`. The global `sync_interval` is now changeable with `config set` (item 2); a per-folder interval would need per-folder tickers in the peer loop and would buy little on top of windows and notifications. Recorded as a decision, not a deferral.
- Status: `folders[].sync_windows` (array of canonical strings), `folders[].in_window` (bool; true when the list is empty), `folders[].next_window` (RFC3339 UTC of the next opening, or `null` when in a window or always open), and `state` = `scheduled` when the folder is not paused, has no error, is not scanning or syncing, and is outside its windows. Computed by `status.Service` from its clock, so it works without a daemon.

**Files.**

```
internal/domain/folder/window.go, window_test.go                 create; Folder.SyncWindows, Validate, InWindow
internal/adapters/sqlitestore/migrations/004_sync_windows.sql    create
internal/adapters/sqlitestore/migrate.go                         schemaVersion = 4; migrationFiles += 004
internal/adapters/sqlitestore/folders.go                         column sync_windows in folderColumns, SaveFolder, scanFolder
internal/adapters/sqlitestore/migrate_test.go, folders_test.go   tests
internal/app/fakes/store.go                                      SaveFolder already copies the struct; nothing else
internal/app/folders/service.go                                  Changes.SyncWindows *folder.Windows
internal/app/syncer/puller.go                                    ErrOutsideWindow; window checks in SyncFolder and fetch; pullAll treats it like ErrPaused
internal/app/syncer/window_test.go                               create
internal/app/daemon/peerloop.go                                  syncable() skips folders outside their window
internal/app/status/report.go, service.go                        fields above; StateScheduled
internal/cli/folder.go                                           --sync-window (add/set), --clear-sync-windows (set); folderView.SyncWindows
internal/cli/folder_integration_test.go (or cli_integration_test.go)  tests
internal/e2e/schedule_test.go                                    create
docs/CLI.md, docs/STATE.md, docs/GLOSSARY.md                     update
.specs/tasks.md                                                  tick "Implement pause, resume, scheduled sync, and bandwidth limits."
```

**Key signatures.**

```go
// internal/domain/folder/window.go
type Minute int                                    // minutes since local midnight, 0..1439
type Window struct{ Start, End Minute }            // End exclusive; Start > End wraps midnight
var ErrInvalidWindow = errors.New("invalid sync window")
func ParseWindow(s string) (Window, error)         // "HH:MM-HH:MM"
func (w Window) String() string                    // canonical "22:00-06:00"
func (w Window) Contains(t time.Time) bool         // wall-clock minutes of t in t.Location()
func (w Window) NextStart(t time.Time) time.Time   // first opening strictly after t, same Location
type Windows []Window
func ParseWindows(ss []string) (Windows, error)    // rejects duplicates after canonicalization
func (ws Windows) Always() bool                    // len == 0
func (ws Windows) Contains(t time.Time) bool       // Always() || any
func (ws Windows) Next(t time.Time) (time.Time, bool)  // false when Always() or Contains(t)
func (ws Windows) Strings() []string
func EncodeWindows(ws Windows) string              // "22:00-06:00,12:00-13:00" for the column; "" when Always
func DecodeWindows(s string) (Windows, error)

// Folder
type Folder struct { ...; SyncWindows Windows }
func (f Folder) InWindow(t time.Time) bool
// Validate: every window valid (ParseWindow(w.String()) round-trips), no duplicates

// internal/app/folders
type Changes struct { Mode *folder.Mode; DeletePolicy *folder.DeletePolicy; SyncWindows *folder.Windows /* pointer to empty slice clears */ }

// internal/app/syncer
var ErrOutsideWindow = errors.New("outside sync window")
func (p *Puller) outsideWindowNow(ctx context.Context, id folder.ID) bool   // re-reads the folder like pausedNow

// internal/app/status
const StateScheduled = "scheduled"
type Folder struct { ...; SyncWindows []string `json:"sync_windows"`; InWindow bool `json:"in_window"`; NextWindow *string `json:"next_window"` }
```

Migration `004_sync_windows.sql`:

```sql
-- Sync windows in the device's local time, "HH:MM-HH:MM" separated by commas.
-- Empty means the folder is always open, which is what every row written
-- before this column existed means.
ALTER TABLE folders ADD COLUMN sync_windows TEXT NOT NULL DEFAULT '';
```

**CLI surface.**

```
syncthis folder add PATH --id ID [--sync-window HH:MM-HH:MM]...
syncthis folder set ID [--mode M] [--delete-policy P] [--sync-window W]... [--clear-sync-windows]
```

`folder set` with `--sync-window` replaces the whole list; `--clear-sync-windows` with `--sync-window` is exit 2. The folder JSON object (`folder add/list/set`) gains `"sync_windows": ["22:00-06:00"]`. `folder list` text gains a `WINDOWS` column (`always` when empty). `status` text shows `scheduled` in `STATE`.

**Exit codes used.** 2 (bad window, conflicting flags), 4, 0.

**Tests.**

- `internal/domain/folder/window_test.go` (imports `time/tzdata` so `LoadLocation("Europe/Berlin")` works on a bare CI image): `TestParseWindow_Canonical` (`"9:05-17:30"` → `"09:05-17:30"`), `TestParseWindow_Rejects` (`"25:00-01:00"`, `"22:00"`, `"22:00-22:00"`, `"2200-0600"`, `"22:60-01:00"`), `TestWindow_ContainsOvernight` (22:00 in, 23:30 in, 03:00 in, 05:59 in, 06:00 out, 12:00 out), `TestWindow_ContainsUsesTheTimesLocation` (same instant, `FixedZone(+2)` in, UTC out), `TestWindows_NextCrossesMidnight`, `TestWindows_NextInsideIsFalse`, `TestWindows_NextAcrossSpringForward` (Berlin 2026-03-29 01:00, window `02:30-04:00` → next start 03:30 CEST, i.e. 01:30 UTC), `TestWindows_ContainsAcrossFallBack` (Berlin 2026-10-25, window `02:00-03:00` contains both 02:30 CEST and 02:30 CET), `TestEncodeDecodeWindows_RoundTrip`, `TestFolder_ValidateRejectsDuplicateWindows`.
- `internal/adapters/sqlitestore`: `TestSaveFolder_PersistsSyncWindows`, `TestMigrate_3To4_KeepsFoldersAndReadsThemAsAlwaysOpen` (build a v3 database by applying the first three migration files through the existing test helper, insert a folder row, `Open`, assert version 4 and `SyncWindows.Always()`).
- `internal/app/syncer/window_test.go`: `TestSyncFolder_OutsideWindowRequestsNothing` (fake clock 12:00 UTC, window `22:00-06:00` → zero manifest requests, empty summary, nil error), `TestFetch_StopsWithinOneChunkWhenTheWindowCloses` (chunk handler advances the fake clock past the end after chunk 2 → exactly 3 requests, transfer `pending`, error `outside sync window`, no `transfer.failed` event), `TestSyncFolder_ContinuesFromTheJournalInsideTheWindow`.
- `internal/app/daemon/daemon_test.go`: `TestPeerLoop_SkipsFolderOutsideItsWindow` (two folders, one closed by a window against the fake clock; recordingSyncer sees only the open one), `TestReload_WindowRemovedSyncsAtOnce`.
- `internal/app/status/service_test.go`: `TestReport_ScheduledStateAndNextWindow` (fake clock; `state == "scheduled"`, `in_window == false`, `next_window` equals the UTC form of the next start), `TestReport_ScanningWinsOverScheduled`.
- `internal/app/folders/service_test.go`: `TestUpdate_SyncWindowsReplaceAndClear`.
- CLI: `TestFolderAdd_SyncWindowsRoundTrip`, `TestFolderAdd_BadWindowExit2`, `TestFolderSet_ClearSyncWindows`, `TestFolderSet_ClearAndSetConflictExit2`, `TestStatus_ScheduledWithoutDaemon` (window far from now computed at test time). JSON purity: `folder set docs --sync-window 22:00-06:00`.
- `internal/e2e/schedule_test.go`: `TestSyncWindow_ClosedFolderScansAndServesButDoesNotPull`: a and b with `ExtraFolder`; on b `folder set docs --sync-window W` where W is a 1-minute window six hours from `time.Now()` (never crossed during the test); a writes `docs/x.txt` and `extra/y.txt`; wait for `extra/y.txt` on b (proves cycles run); assert `docs/x.txt` absent on b and b's status for docs is `scheduled` with `in_window false`; b writes `docs/from-b.txt`, wait until a has it (b still serves); `status` on b shows files counted (b still scans); then `folder set docs --clear-sync-windows` on b → `waitTreeEqual(a, b)`.

**Verification.**

```
make lint && make test
go test ./internal/domain/folder/ -run Window -v
TZ=Europe/Berlin go test ./internal/domain/folder/ -run Window          # same result: the domain ignores TZ
syncthis --data-dir /tmp/b/data folder set docs --sync-window 22:00-06:00 && syncthis --data-dir /tmp/b/data status --json | jq '.folders[0] | {state, in_window, next_window, sync_windows}'
```

**Docs.** `CLI.md`: flags, JSON, `scheduled` state, a "Sync windows" section with the semantics paragraph above (what stops, what does not, why, DST behaviour, when a window opening is noticed). `STATE.md`: column, migration table row 4. `GLOSSARY.md`: sync window, scheduled.

---

## Item 4 — Config export and import

**Goal.** `config export` writes one JSON document with the settings, the folders (ignore patterns and sync windows included) and the peers; `config import` reads one, validates it whole, reports conflicts with exit codes, and writes it. Identity material is never part of it.

**Semantics (decided).**

- The bundle carries `settings` (the `ports.Settings` fields: `listen`, `interface`, `chunk_size`, `max_file_size`, `rescan_interval`, `sync_interval`, `log_level`, `upload_limit`, `download_limit`), `folders` (the `folder add` object plus `sync_windows`), `peers` (`id`, `name`, `address`, `verified`). Not included, by design: `device_name` (it is baked into the certificate), the device ID, `identity.key`, `identity.crt`, the database contents, `last_seen_at`/`added_at`/`created_at`.
- Decoding is strict: unknown fields fail, `schema_version` must be 1. Validation runs over the whole document before anything is written: each folder passes `folder.Validate`, its path is an existing directory (`ports.FileSystem.Lstat`), its ignore patterns parse, its windows parse; each peer ID parses and its address is non-empty; settings have non-negative limits, positive intervals, `chunk_size >= transfer.MinChunkSize`. A validation failure is exit 2.
- Conflicts: a folder ID already configured, a folder path already used by a folder with another ID, a peer ID already known. Without `--replace` every conflict is reported (all of them, not the first) and nothing is written: exit 5. With `--replace`, same-ID folders and same-ID peers are overwritten (`created_at`/`last_scan_at` and `added_at`/`last_seen_at` of the existing row are kept, the database rows of the folder are kept: only configuration changes); a path owned by a different ID is still a conflict, because two IDs cannot share a path. `--replace` is confirmed like every destructive command (`--yes`, exit 9 otherwise).
- `--dry-run` validates, computes the result, writes nothing, exits 10 when something would change and 0 otherwise.
- Writes go folder by folder, peer by peer, then settings; a failure midway is reported as exit 1 and the document says which items were written (the result object is printed before the error message in text mode; in JSON mode the error object carries the partial result in `message`). Not transactional across the three stores: stated in the docs.
- A successful import calls `reloadDaemon`, so a running daemon picks the folders and peers up at once and the limits from settings.

**Files.**

```
internal/app/configbundle/bundle.go, service.go, service_test.go     create
internal/compose/compose.go                                          App.Bundle = configbundle.New(store, store, app.Settings, fs, clock)
internal/cli/config.go                                               add export, import subcommands
internal/cli/config_integration_test.go                              extend
internal/cli/cli_json_test.go                                        add export / import cases
internal/cli/exitcodes.go                                            configbundle.ErrInvalidBundle -> 2; ConflictError Is(ports.ErrAlreadyExists) -> 5 already
internal/e2e/reload_test.go                                          TestConfigImport_ReloadsRunningDaemon
docs/CLI.md, docs/STATE.md, docs/GLOSSARY.md                         update
.specs/tasks.md                                                      tick "Support configuration import and export."
```

**Key signatures.**

```go
// internal/app/configbundle/bundle.go
const SchemaVersion = 1
type Bundle struct {
    SchemaVersion int      `json:"schema_version"`
    ExportedAt    string   `json:"exported_at"`   // RFC3339 UTC; ignored on import
    Settings      Settings `json:"settings"`
    Folders       []Folder `json:"folders"`
    Peers         []Peer   `json:"peers"`
}
type Settings struct {
    Listen string `json:"listen"`; Interface string `json:"interface"`; ChunkSize int `json:"chunk_size"`
    MaxFileSize int64 `json:"max_file_size"`; RescanInterval string `json:"rescan_interval"`; SyncInterval string `json:"sync_interval"`
    LogLevel string `json:"log_level"`; UploadLimit int64 `json:"upload_limit"`; DownloadLimit int64 `json:"download_limit"`
}
type Folder struct {
    ID, Path, Label, Mode, DeletePolicy, Symlinks string   // json tags as in the folder add object
    IncludeHidden, KeepEmptyDirs, Paused bool
    IgnorePatterns []string `json:"ignore_patterns"`
    SyncWindows    []string `json:"sync_windows"`
}
type Peer struct { ID, Name, Address string; Verified bool }
var ErrInvalidBundle = errors.New("invalid configuration bundle")
func Decode(r io.Reader) (Bundle, error)          // DisallowUnknownFields; schema_version check; wraps ErrInvalidBundle
func Encode(w io.Writer, b Bundle) error          // indented, trailing newline

// internal/app/configbundle/service.go
type Service struct { folders ports.FolderStore; devices ports.DeviceStore; settings ports.SettingsStore; fs ports.FileSystem; clock ports.Clock }
func New(folders ports.FolderStore, devices ports.DeviceStore, settings ports.SettingsStore, fs ports.FileSystem, clock ports.Clock) *Service
func (s *Service) Export(ctx context.Context) (Bundle, error)
type ImportOptions struct{ Replace, DryRun bool }
type Conflict struct { Kind string /* "folder-id" | "folder-path" | "peer" */; ID, Path, Existing string }
type ConflictError struct{ Conflicts []Conflict }
func (e *ConflictError) Error() string; func (e *ConflictError) Is(target error) bool // target == ports.ErrAlreadyExists
type Result struct {
    DryRun bool `json:"dry_run"`
    FoldersAdded, FoldersReplaced []string `json:"folders_added" / "folders_replaced"`
    PeersAdded, PeersReplaced     []string `json:"peers_added" / "peers_replaced"`
    SettingsChanged bool `json:"settings_changed"`
}
func (r Result) Changed() bool
func (s *Service) Import(ctx context.Context, b Bundle, o ImportOptions) (Result, error)
// order inside Import: validate(b) -> conflicts(ctx, b, o.Replace) -> if DryRun return -> write folders, peers, settings
```

**CLI surface.**

```
syncthis config export [--output FILE]
syncthis config import FILE [--replace] [--dry-run] [--yes]
```

`config export` without `--output` writes the bundle to stdout; it is JSON already, so `--json` changes nothing. With `--output FILE` the file is written `0600` (it holds peer addresses and folder paths, nothing secret, but the data directory is 0700 and the export should not be looser); `-` means stdout. Then it prints `exported 2 folders and 1 peer to FILE`, or with `--json` `{"written": "FILE", "folders": 2, "peers": 1}`. `config import FILE` (`-` for stdin; then `--replace` needs `--yes` because stdin is the document) prints the `Result` object with `--json`, or lines `folder docs: added`, `peer ETI3RRZ…: replaced`, `settings: changed`, `nothing to import`. Conflicts print one line each on stderr before the exit 5 message (`folder "docs": id already configured`, `folder "notes": path /home/u/notes is already synchronized as "docs"`, `peer ETI3RRZ…: already configured`); in JSON mode they are joined with `; ` into the error `message`.

**Exit codes used.** 2 (unreadable file, bad JSON, unknown field, schema, validation), 5 (conflicts), 9 (`--replace` without confirmation), 10 (`--dry-run` with changes), 1 (write failure), 0.

**Tests.**

- `internal/app/configbundle/service_test.go` (fakes): `TestExport_ThenImportIntoEmptyStoresRoundTrips` (export A, import into B, export B; equal except `exported_at`), `TestExport_ContainsNoIdentityMaterial` (marshal; assert no key named `device_name`, `device_id`, `identity`, `key`, `certificate`), `TestImport_ReportsEveryConflictAndWritesNothing` (two folders and a peer collide → 3 conflicts of the three kinds; stores unchanged), `TestImport_ReplaceOverwritesAndKeepsTimestamps`, `TestImport_PathOwnedByAnotherIDConflictsEvenWithReplace`, `TestImport_DryRunWritesNothingAndReportsChanges`, `TestImport_FolderPathNotADirectoryIsInvalid`, `TestImport_BadWindowOrPatternIsInvalid`, `TestDecode_RejectsUnknownFieldAndOtherSchemaVersion`, `TestImport_SettingsUnchangedIsNotReported`.
- CLI: `TestConfigExportImport_RoundTripBetweenDataDirs` (init A; `folder add … --ignore '*.tmp' --sync-window 22:00-06:00`; `config set --download-limit 1MiB`; write a peer row through `compose.Build` + `Store.SaveDevice` in the test since `peer add` needs a live peer; export to FILE; init C; import FILE; export C; compare JSON ignoring `exported_at`), `TestConfigImport_ConflictExit5ListsAll`, `TestConfigImport_ReplaceWithoutYesExit9`, `TestConfigImport_ReplaceWithYesOverwrites`, `TestConfigImport_DryRunExit10ThenExit0AfterImport`, `TestConfigImport_UnknownFieldExit2`, `TestConfigImport_MissingDirectoryExit2`, `TestConfigExport_OutputFileIs0600`, `TestConfigExport_StdoutIsTheBundleWithAndWithoutJSON`. JSON purity: `config export`, `config import FILE`, `config import FILE --replace` (exit 9), `config import nope.json` (exit 2).
- `internal/e2e/reload_test.go`: `TestConfigImport_ReloadsRunningDaemon` (b starts with `NoFolder`; a bundle naming `docs` at `b.Root` and a's peer row is imported through `cliOK(b, "config", "import", file)`; a writes a file; `waitTreeEqual(a, b)`).

**Verification.**

```
make lint && make test
syncthis --data-dir /tmp/a/data config export --output /tmp/a.json && jq 'keys' /tmp/a.json     # ["exported_at","folders","peers","schema_version","settings"]
syncthis --data-dir /tmp/c/data init --name c && syncthis --data-dir /tmp/c/data config import /tmp/a.json --dry-run; echo $?    # 10
syncthis --data-dir /tmp/c/data config import /tmp/a.json --json
syncthis --data-dir /tmp/c/data config import /tmp/a.json; echo $?                                # 5
```

**Docs.** `CLI.md`: sections for both commands, the full bundle schema with one example, the conflict rules, the exit codes, the non-transactional note, the identity exclusion. `STATE.md`: one paragraph under Data directory ("what `config export` covers and what it never does"). `GLOSSARY.md`: bundle.

---

## Item 5 — Tailscale peer detection

**Goal.** `network tailscale detect` and `network peers` report what the local Tailscale client knows, through a `TailnetDiscovery` port whose adapter runs `tailscale status --json` when the binary is present. `peer add` and `peer test` accept a bare MagicDNS hostname and resolve it through the same port before falling back to system DNS. Nothing else ever calls Tailscale.

**Semantics (decided).**

- The adapter looks the binary up on every call (`exec.LookPath("tailscale")`); no lookup happens at startup, so a device without Tailscale pays nothing and `daemon run` never shells out. The command runs with a 5 s timeout.
- Absence (no binary) and a backend state other than `Running` both surface as `ports.ErrTailnetUnavailable`, with the reason in the message (`tailscale: not installed`, `tailscale: backend state Stopped`). `network tailscale detect` turns absence into a document with `present: false` and exits 0: detection is the question, "no" is an answer. `network peers` exits with a new code, 11 `tailnet unavailable`, because the listing that was asked for cannot exist; reusing 4 (not found) or 6 (daemon unavailable) would lie about what is missing.
- Parsing is tolerant: unknown fields are ignored (the opposite of item 4's bundle), `Online` is a `*bool` and falls back to `Active` when absent (older clients), the MagicDNS suffix is read from `CurrentTailnet.MagicDNSSuffix` first and the top-level `MagicDNSSuffix` second, `Peer` is a map keyed by node key whose values are sorted by hostname on output, `DNSName` has its trailing dot removed. The interface name is not in the JSON; the adapter finds the interface that carries `Self.TailscaleIPs[0]`, else the first with a `100.64.0.0/10` address, else `""` (userspace networking). The `Version` string is kept in the status for the `--json` output and error messages.
- `Resolve(name)` matches case-insensitively against each node's `HostName`, `DNSName`, and `HostName + "." + suffix`, self included (the peer service then reports `ErrSelf`, which is the right message). IPv4 addresses come first. Unknown names are `ports.ErrNotFound`.
- In `peers.Service.Add` and `Test`, after `address()` splits host and port: when the host is not an IP literal and a tailnet port is configured, ask it; on success replace the host with the first address and remember the name; on `ErrTailnetUnavailable`, `ErrNotFound`, or any other error keep the host as typed so the dialer resolves it through system DNS. The stored `Address` is the resolved IP and port (tailnet IPs are stable; storing the hostname would make every dial depend on the system resolver knowing MagicDNS). `AddResult.ResolvedFrom` and `TestResult.ResolvedFrom` carry the hostname for the output.

**Files.**

```
internal/app/ports/tailnet.go                                             create
internal/app/fakes/tailnet.go, fakes.go                                   create
internal/adapters/tailscalecli/discovery.go, parse.go, runner.go           create
internal/adapters/tailscalecli/discovery_test.go, parse_test.go            create
internal/adapters/tailscalecli/testdata/status_current.json                create (1.80-era shape: CurrentTailnet, Online)
internal/adapters/tailscalecli/testdata/status_old.json                    create (pre-Online shape: top-level MagicDNSSuffix, Active only)
internal/adapters/tailscalecli/testdata/status_stopped.json                create (BackendState "Stopped", no Self IPs)
internal/app/network/service.go, service_test.go                          create
internal/app/peers/service.go, service_test.go                            SetTailnet; resolve step; ResolvedFrom
internal/compose/compose.go                                               App.Tailnet, App.Network; Peers.SetTailnet
internal/cli/network.go, network_integration_test.go                      create
internal/cli/peer.go                                                      resolved_from in JSON; "(nas via tailnet)" in text
internal/cli/exitcodes.go                                                 ExitTailnetUnavailable = 11; codeOf maps ports.ErrTailnetUnavailable
internal/cli/root.go                                                      register newNetworkCmd
internal/cli/cli_json_test.go                                             cases
docs/CLI.md, docs/ARCHITECTURE.md, docs/GLOSSARY.md                       update
.specs/tasks.md                                                           tick "Implement Tailscale peer detection through MagicDNS and Tailscale IPs."
```

**Key signatures.**

```go
// internal/app/ports/tailnet.go
var ErrTailnetUnavailable = errors.New("tailscale is not available")
type TailnetNode struct {
    ID       string   // Tailscale stable node id
    HostName string   // "nas"
    DNSName  string   // "nas.tail1234.ts.net" (no trailing dot)
    IPs      []string // IPv4 first
    Online   bool
    OS       string
}
type TailnetStatus struct {
    Version        string
    BackendState   string   // "Running", "Stopped", "NeedsLogin", ...
    Running        bool     // BackendState == "Running"
    Interface      string   // "" when unknown
    MagicDNSSuffix string
    Self           TailnetNode
}
type TailnetDiscovery interface {
    Status(ctx context.Context) (TailnetStatus, error)          // ErrTailnetUnavailable only when the binary is missing or the command fails
    Peers(ctx context.Context) ([]TailnetNode, error)           // sorted by HostName; excludes Self; ErrTailnetUnavailable when not Running
    Resolve(ctx context.Context, name string) ([]string, error) // ErrNotFound; ErrTailnetUnavailable
}

// internal/adapters/tailscalecli/runner.go
type Runner interface {
    LookPath(file string) (string, error)
    Run(ctx context.Context, path string, args ...string) ([]byte, error)   // stdout; stderr folded into the error
}
type ExecRunner struct{}   // os/exec; the adapter applies the 5 s timeout
// internal/adapters/tailscalecli/discovery.go
type Discovery struct { runner Runner; binary string; interfaces func() ([]net.Interface, error) }
func New(runner Runner) *Discovery      // binary "tailscale"; interfaces = net.Interfaces
func Parse(raw []byte) (ports.TailnetStatus, []ports.TailnetNode, error)   // exported for tests and for the CLI fixture test
// parse.go: statusJSON{Version, BackendState, Self nodeJSON, MagicDNSSuffix, CurrentTailnet *struct{MagicDNSSuffix}, Peer map[string]nodeJSON}
//           nodeJSON{ID, HostName, DNSName, TailscaleIPs []string, Online *bool, Active bool, OS}

// internal/app/network/service.go
type Detection struct {
    Present, Running bool
    BackendState, Version, Interface, MagicDNSSuffix, HostName, DNSName string
    IPs []string
}
type Service struct{ tailnet ports.TailnetDiscovery }
func New(t ports.TailnetDiscovery) *Service
func (s *Service) Detect(ctx context.Context) (Detection, error)      // ErrTailnetUnavailable -> Present false, nil error
func (s *Service) Peers(ctx context.Context) ([]ports.TailnetNode, error)

// internal/app/peers/service.go
func (s *Service) SetTailnet(t ports.TailnetDiscovery)
type AddResult struct { Device device.Device; Accepted bool; ResolvedFrom string }
type TestResult struct { ...; ResolvedFrom string }
func (s *Service) resolve(ctx context.Context, addr string) (resolved string, from string)   // host:port in, host:port out

// internal/cli/exitcodes.go
ExitTailnetUnavailable = 11
```

**CLI surface and JSON.**

```
syncthis network tailscale detect
syncthis network peers
syncthis peer add nas [--device-id ID] ...            # bare hostname or nas.tail1234.ts.net
syncthis peer test nas
```

```json
// network tailscale detect --json (present)
{"present": true, "running": true, "backend_state": "Running", "version": "1.80.2",
 "interface": "tailscale0", "hostname": "laptop", "dns_name": "laptop.tail1234.ts.net",
 "ips": ["100.64.0.2", "fd7a:115c:a1e0::2"], "magic_dns_suffix": "tail1234.ts.net"}
// absent
{"present": false, "running": false, "backend_state": "", "version": "", "interface": "",
 "hostname": "", "dns_name": "", "ips": [], "magic_dns_suffix": ""}
// network peers --json
[{"id": "nNAS123", "hostname": "nas", "dns_name": "nas.tail1234.ts.net",
  "ips": ["100.64.0.3"], "online": true, "os": "linux"}]
```

Text: `detect` prints `key: value` lines (`tailscale: not installed` when absent); `peers` prints columns `HOSTNAME`, `DNS NAME`, `IPS`, `ONLINE`, `OS`. `peer add --json` gains `"resolved_from": "nas"` (omitted when no resolution happened); text prints `added peer nas (ETI3…) at 100.64.0.3:22000 (nas via tailnet)`. `peer test` gets the same field and line.

**Exit codes used.** 0, 11 (`network peers` with no usable Tailscale), 7 (peer add/test failures as before), 2.

**Tests.**

- `internal/adapters/tailscalecli/parse_test.go`: `TestParse_CurrentFixture` (suffix from `CurrentTailnet`, `Online` honoured, peers sorted, self excluded, trailing dot trimmed, IPv4 first), `TestParse_OldFixtureFallsBackToActiveAndTopLevelSuffix`, `TestParse_StoppedFixtureIsNotRunning`, `TestParse_UnknownFieldsAreIgnored`, `TestParse_GarbageIsAnError`.
- `internal/adapters/tailscalecli/discovery_test.go` (fake `Runner`): `TestStatus_BinaryMissingIsUnavailable`, `TestStatus_CommandFailureIsUnavailableWithStderr`, `TestPeers_NotRunningIsUnavailable`, `TestResolve_BareHostnameFQDNAndCaseInsensitive`, `TestResolve_UnknownIsNotFound`, `TestResolve_SelfResolves`, `TestStatus_InterfaceFoundByAddress` (injected `interfaces` func), `TestRun_HonoursTimeout` (runner that blocks until ctx is done; assert the adapter's 5 s deadline is present on the context it passes, by reading `ctx.Deadline()` in the fake).
- `internal/app/network/service_test.go`: `TestDetect_AbsentIsNotAnError`, `TestDetect_RunningCopiesSelf`, `TestPeers_PassesUnavailableThrough`.
- `internal/app/peers/service_test.go`: `TestAdd_ResolvesHostnameThroughTailnetAndStoresTheIP` (fakes.Tailnet resolves `nas` → `100.64.0.3`; fakes.Dialer scripted at `100.64.0.3:22000`; stored address is the IP; `ResolvedFrom == "nas"`), `TestAdd_FallsBackToDNSWhenTailnetUnavailable` (dialer scripted at `nas:22000`), `TestAdd_FallsBackWhenNameUnknownToTailnet`, `TestAdd_IPLiteralNeverAsksTailnet` (fake counts calls), `TestAdd_NoTailnetConfiguredBehavesAsBefore`, `TestTest_ResolvesHostnameToo`.
- `internal/cli/network_integration_test.go`: `TestNetworkTailscaleDetect_AbsentPrintsPresentFalse` (`t.Setenv("PATH", t.TempDir())`), `TestNetworkPeers_AbsentExit11`, `TestNetworkTailscaleDetect_StubBinaryReadsFixture` (write an executable `tailscale` shell script into a temp dir on `PATH` that `cat`s `status_current.json`; skip when `sh` is not on PATH), `TestNetworkPeers_StubBinaryTable`, `TestPeerAdd_BareHostnameResolvedThroughStubTailscale` (fixture variant whose peer `nas` has IP `127.0.0.1`; `startDaemon` on a second env with a fixed free port; `peer add nas:PORT --yes` succeeds and stores `127.0.0.1:PORT`, `resolved_from == "nas"`). JSON purity: `network`, `network tailscale`, `network tailscale detect`, `network peers` (exit 11 as an error object).
- Nothing in `internal/e2e` (the e2e suite must stay free of Tailscale).

**Verification.**

```
make lint && make test
syncthis network tailscale detect --json          # on a tailnet machine: present true, interface tailscale0
syncthis network peers
syncthis peer test nas                            # resolves through tailscale, then probes
PATH=/usr/bin syncthis network peers; echo $?     # 11 when tailscale lives elsewhere
```

**Docs.** `CLI.md`: exit code 11 row, `network` sections with both JSON shapes, `peer add`/`peer test` hostname resolution paragraph (what is stored and why), the pairing-over-Tailscale walkthrough updated to use hostnames. `ARCHITECTURE.md` ports table: `TailnetDiscovery` / `tailscalecli` ("the local Tailscale client, through `tailscale status --json`"). `GLOSSARY.md`: tailnet, MagicDNS, tailnet peer. `PROTOCOL.md`: unchanged.

---

## Item 6 — Mid-scan crash test

**Goal.** A subprocess test proves that a process killed with SIGKILL while scanning a 20,000-file folder leaves a database that opens, that `repair --dry-run` finds nothing unrecoverable, and that a rescan converges. A second test does the same to `daemon run` and also proves the stale pid file and control socket are recovered.

**Semantics of the test (decided).**

- Lives in `internal/e2e` because `make test` runs that package alone with `-p 1`, which is what a test that builds a binary and competes for the disk needs. Skipped with `-short` (`"builds the binary and kills it mid-scan; run without -short"`) and when `go` is not on `PATH`.
- The binary is built once per package run by `TestMain` in a new `internal/e2e/main_test.go` into a temporary directory, without `-race` (the test process may run with `-race`; the child is what is killed, so its build mode is irrelevant to the claim). Build failures fail the package.
- Fixture: 200 directories × 100 files, 32–256 bytes each, deterministic names `d%03d/f%03d.txt`, deterministic content, so a hash check is possible.
- Kill schedule: attempts with delays `5, 20, 60, 150, 400, 1000 ms` (each scaled by `timeoutScale`). Each attempt starts `scan docs --log-level debug --log-format json`, sleeps the delay, and if the process is still running sends SIGKILL and asserts `ProcessState.Sys().(syscall.WaitStatus).Signaled()`. If it already exited, the attempt is recorded as "completed". The test requires at least one attempt to have killed the process mid-scan, otherwise it fails with a message asking to lower the delays (on today's machines 20,000 hashes and a SQLite transaction take far more than 5 ms).
- After every kill, in order: (a) `status --json` exits 0 and `folders[0].files` is 0 or 20000 (the scan write is one transaction, so nothing in between may be visible: this is the atomicity claim, and it fails loudly if `ApplyScan` is ever split); (b) `repair docs --dry-run --json` exits 0 or 10 with `unrecoverable == []`; (c) `scan docs --json` exits 0; (d) `scan docs --dry-run --json` exits 0 with `summary.changes == 0`; (e) `status --json` shows `files == 20000` and `bytes` equal to the fixture total.
- `TestCrash_DaemonKilledDuringStartupScan`: `daemon run --listen 127.0.0.1:0` as a subprocess, SIGKILL 100 ms after the pid file appears; then `daemon run` again as a subprocess must reach `status.daemon.running == true` within `daemonTimeout` (stale socket replaced, stale pid overwritten), receives SIGTERM and exits 0; then steps (b)–(e).

**Files.**

```
internal/e2e/main_test.go             create: TestMain builds the binary (or records the build error) and removes the temp dir
internal/e2e/crash_scan_test.go       create
docs/STATE.md                         add "A crash during a scan" paragraph naming the guarantee and the test
.specs/tasks.md                       tick "Add a crash test that kills the process mid-scan."
```

**Key helpers (test-only).**

```go
var syncthisBinary string   // set by TestMain; "" means the build failed or was skipped
func buildBinary() (string, error)                                          // go build -o <tmp>/syncthis ../../cmd/syncthis
func makeTree(t *testing.T, root string, dirs, files int) (count int, bytes int64)
type child struct{ cmd *exec.Cmd; stderr bytes.Buffer; exited chan struct{} }
func startChild(t *testing.T, dataDir string, args ...string) *child          // stdout discarded, stderr captured
func (c *child) killAfter(d time.Duration) (killed bool)                     // SIGKILL if still running; waits; reports whether it was signaled
func runBinary(t *testing.T, dataDir string, args ...string) (int, string)   // one-shot invocation, returns exit code and stdout
```

**Tests.**

- `TestCrash_ScanKilledMidWayLeavesARecoverableDatabase` (the loop above).
- `TestCrash_DaemonKilledDuringStartupScan`.

**Verification.**

```
make lint && make test                                    # includes both, roughly 30-60 s extra
go test ./internal/e2e/ -run Crash -count=3 -race -v      # the test binary under -race, child without
go test ./internal/e2e/ -run Crash -short                 # skipped, with the reason printed
SYNCTHIS_E2E_TIMEOUT_SCALE=3 go test ./internal/e2e/ -run Crash
```

**Docs.** `STATE.md`: under Journal semantics, a paragraph "A crash during a scan": the scan write is one transaction, a kill leaves the previous manifest or the new one, the next scan converges; names the test.

---

## Cross-cutting notes for implementers

- `internal/arch/deps_test.go` needs no change: `time/tzdata` (item 3 tests) and `time` are stdlib and not banned; `os/exec` and `net` appear only in `internal/adapters/tailscalecli` and in test files under `internal/cli` and `internal/e2e`, where they are allowed.
- Every new port gets a fake in `internal/app/fakes` and a line in `fakes.go`'s compile-time check block: `Limiter`, `Settings`, `Tailnet`.
- Every new command gets a row in `internal/cli/cli_json_test.go`, which is what keeps `--json` output pure.
- `status.SchemaVersion` stays 1 throughout; every status field added here is additive.
- Commit boundaries: item 1 one commit; item 2 as domain+port+adapter, then app, then compose+CLI+docs (three commits, each green); item 3 as domain+migration, then app+CLI+docs; item 4 as app service, then CLI+docs; item 5 as port+adapter+fixtures, then app+CLI+docs; item 6 one commit.

## Top risks

1. **Limiter accuracy under `-race` and load.** Timer granularity and scheduler delay only ever lengthen a wait, so every real-clock assertion states a lower bound (the limit held) and a loose upper bound scaled by `timeoutScale`; the exact "about 5 s" claim is made against an injected clock where it is deterministic. The e2e timing tests skip with `-short`. If the upper bound flakes on CI, raise the scale, never lower the lower bound.
2. **Local-time windows across DST.** `Contains` compares wall-clock minutes in the time's own `Location` and never offsets; `NextStart` builds dates with `time.Date` and lets Go normalize gaps. The behaviour on the two DST nights is stated in `CLI.md` and pinned by tests against `Europe/Berlin` with `time/tzdata` imported, so a CI image without zoneinfo cannot skip them silently. The daemon's zone is `time.Local` at process start; a zone change needs a restart, and that is documented.
3. **Subprocess crash test flakiness.** A kill that lands after the scan finished is a valid iteration, not a failure; the test only requires one mid-scan kill across six increasing delays. The `files ∈ {0, 20000}` check is strict on purpose and is the assertion most likely to reveal a real regression. Building the binary once per package run keeps the cost to one `go build`; the test skips when `go` is absent and under `-short`.
4. **`tailscale status --json` differences across versions.** The parser ignores unknown fields, reads `Online` as optional with `Active` as fallback, reads the suffix from two places, and never fails on an unexpected shape as long as `BackendState` and `Self` exist. Three fixtures (current, old, stopped) are committed; when a user reports a new shape, add its fixture and a case to `parse_test.go`. Nothing else in the binary depends on Tailscale, so a parse failure only costs the `network` commands and the hostname convenience in `peer add`.
5. **Config import writing partially.** Validation and conflict detection run before the first write, so the only partial outcome is a store error midway, which is reported with what was written. Making it transactional would need a cross-store transaction port; deferred and documented.

### Critical Files for Implementation
- /home/wesbragagt/dev/syncthing/internal/app/syncer/puller.go
- /home/wesbragagt/dev/syncthing/internal/app/daemon/daemon.go
- /home/wesbragagt/dev/syncthing/internal/compose/compose.go
- /home/wesbragagt/dev/syncthing/internal/app/peers/service.go
- /home/wesbragagt/dev/syncthing/internal/adapters/jsonconfig/config.go