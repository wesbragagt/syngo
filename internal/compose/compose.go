// Package compose is the composition root: it resolves the data directory,
// opens the adapters and wires them into the application services.
package compose

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"

	"github.com/wesbragagt/syngo/internal/adapters/blake3hash"
	"github.com/wesbragagt/syngo/internal/adapters/identityfile"
	"github.com/wesbragagt/syngo/internal/adapters/jsonconfig"
	"github.com/wesbragagt/syngo/internal/adapters/localfs"
	"github.com/wesbragagt/syngo/internal/adapters/sqlitestore"
	"github.com/wesbragagt/syngo/internal/adapters/sysclock"
	"github.com/wesbragagt/syngo/internal/adapters/tailscalecli"
	"github.com/wesbragagt/syngo/internal/adapters/tcpwire"
	"github.com/wesbragagt/syngo/internal/adapters/tokenbucket"
	"github.com/wesbragagt/syngo/internal/adapters/unixctl"
	"github.com/wesbragagt/syngo/internal/app/configbundle"
	"github.com/wesbragagt/syngo/internal/app/conflicts"
	"github.com/wesbragagt/syngo/internal/app/daemon"
	"github.com/wesbragagt/syngo/internal/app/folders"
	"github.com/wesbragagt/syngo/internal/app/mergebase"
	"github.com/wesbragagt/syngo/internal/app/network"
	"github.com/wesbragagt/syngo/internal/app/peers"
	"github.com/wesbragagt/syngo/internal/app/ports"
	"github.com/wesbragagt/syngo/internal/app/repair"
	"github.com/wesbragagt/syngo/internal/app/scan"
	"github.com/wesbragagt/syngo/internal/app/status"
	"github.com/wesbragagt/syngo/internal/app/syncer"
	"github.com/wesbragagt/syngo/internal/app/transfers"
	"github.com/wesbragagt/syngo/internal/domain/bandwidth"
	"github.com/wesbragagt/syngo/internal/domain/counterfloor"
	"github.com/wesbragagt/syngo/internal/domain/device"
)

// ErrNotInitialized is returned when the data directory holds no identity and
// one is required. Commands map it to the "run syngo init" exit code.
var ErrNotInitialized = identityfile.ErrNotInitialized

// ErrPublicAddress is returned when the daemon is asked to listen on an
// address reachable from outside this machine without --allow-public.
var ErrPublicAddress = tcpwire.ErrPublicAddress

// ErrListenAddress is returned when the daemon is given a listen address or
// interface it cannot make sense of.
var ErrListenAddress = tcpwire.ErrListenAddress

// ValidateListen reports whether a listen address can be bound at all. It is
// what `config set --listen` checks before writing a value the daemon would
// choke on at its next start.
func ValidateListen(listen string) error { return tcpwire.ValidateListen(listen) }

// DatabaseFile is the name of the SQLite database inside the data directory.
const DatabaseFile = "state.db"

// Options configures how the application is assembled.
type Options struct {
	// DataDir overrides the data directory; empty means "resolve it".
	DataDir string
	Logger  *slog.Logger
	Events  ports.EventSink
	// RequireIdentity makes Build fail with ErrNotInitialized when the data
	// directory has no device identity yet.
	RequireIdentity bool
	// MaxOpenConns limits the SQLite connection pool; 0 means one connection.
	MaxOpenConns int
}

// App holds the wired adapters and services.
type App struct {
	DataDir  string
	Config   jsonconfig.Config
	Store    ports.MetadataStore
	FS       ports.FileSystem
	Hasher   ports.Hasher
	Clock    ports.Clock
	Identity *identityfile.Identity
	Events   ports.EventSink
	Logger   *slog.Logger
	// Settings reads and writes the part of config.json the application is
	// allowed to change while it runs.
	Settings ports.SettingsStore
	// Upload and Download are the device-wide byte budgets, shared by every
	// folder and every peer connection.
	Upload, Download ports.Limiter

	Folders *folders.Service
	Status  *status.Service
	// Transfers reads the transfer journal for `transfer list` and
	// `transfer show`.
	Transfers *transfers.Service
	// Conflicts reads and resolves what the puller recorded as diverged.
	Conflicts *conflicts.Service
	// Bundle exports and imports the configuration as one JSON document.
	Bundle *configbundle.Service
	// Tailnet answers what the local Tailscale client knows. It is always
	// wired: the adapter looks the binary up per call, so a device without
	// Tailscale pays nothing for it.
	Tailnet ports.TailnetDiscovery
	// Network reports on the tailnet for the `network` commands.
	Network *network.Service
	// Control talks to the daemon owning this data directory. Its calls
	// fail with ports.ErrDaemonUnavailable when no daemon is running.
	Control *daemon.Client
	// Scan, Repair, Peers and the peer transport need a device identity, so
	// they are nil until the data directory has been initialized.
	Scan   *scan.Scanner
	Repair *repair.Service
	Peers  *peers.Service
	// Dialer opens connections to peers with this device's certificate.
	Dialer ports.Dialer
	// PeerHandler answers what peers ask of this device.
	PeerHandler ports.PeerHandler
	// Puller fetches from peers what this device is missing.
	Puller *syncer.Puller
	// Planner answers what a sync would do, without doing it.
	Planner *syncer.Planner
	// MergeBases keeps the content a three-way merge merges against. It is
	// nil unless the text_merge setting is on, which is what leaves every
	// conflict keeping both versions.
	MergeBases *mergebase.Recorder
	// CounterFloorTrust says whether a peer has confirmed this device's
	// version counter floor since the process started. It is nil until the
	// data directory has been initialized, like everything else that needs
	// an identity.
	CounterFloorTrust *counterfloor.Trust
}

// Build opens the data directory and wires the application. It does not create
// an identity; use Init for that.
func Build(ctx context.Context, o Options) (*App, error) {
	dir, err := ResolveDataDir(o.DataDir)
	if err != nil {
		return nil, err
	}

	identity, err := identityfile.Load(dir)
	if err != nil {
		if o.RequireIdentity || !errors.Is(err, ErrNotInitialized) {
			return nil, err
		}
		identity = nil
	}
	return assemble(ctx, dir, identity, o)
}

// InitSettings are the values `syngo init` may write into the config.
type InitSettings struct {
	// DeviceName names this device; it is only used when the identity is
	// created, because the name is part of the certificate.
	DeviceName string
	// Listen overrides the daemon listen address; empty keeps the current one.
	Listen string
}

// Init creates the data directory, identity and configuration if they are
// missing, then wires the application. The second result reports whether a new
// identity was generated. Running it again is harmless.
func Init(ctx context.Context, o Options, settings InitSettings) (*App, bool, error) {
	dir, err := ResolveDataDir(o.DataDir)
	if err != nil {
		return nil, false, err
	}
	if settings.Listen != "" {
		if err := tcpwire.ValidateListen(settings.Listen); err != nil {
			return nil, false, err
		}
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, false, fmt.Errorf("create data dir %s: %w", dir, err)
	}

	identity, created, err := identityfile.LoadOrCreate(dir, settings.DeviceName)
	if err != nil {
		return nil, false, err
	}

	cfg, err := jsonconfig.Load(dir)
	if err != nil {
		return nil, false, err
	}
	// The settings only describe a device being created. An initialized
	// data directory keeps the configuration it has, so running init again
	// reports the identity and changes nothing.
	if created {
		updated := cfg
		updated.DeviceName = identity.Name()
		if settings.Listen != "" {
			updated.Listen = settings.Listen
		}
		if err := jsonconfig.Save(dir, updated); err != nil {
			return nil, false, err
		}
	}

	app, err := assemble(ctx, dir, identity, o)
	if err != nil {
		return nil, false, err
	}
	return app, created, nil
}

// assemble opens the database and builds the services.
func assemble(ctx context.Context, dir string, identity *identityfile.Identity, o Options) (*App, error) {
	cfg, err := jsonconfig.Load(dir)
	if err != nil {
		return nil, err
	}

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create data dir %s: %w", dir, err)
	}
	store, err := sqlitestore.Open(ctx, filepath.Join(dir, DatabaseFile), sqlitestore.Options{MaxOpenConns: o.MaxOpenConns})
	if err != nil {
		return nil, err
	}

	logger := o.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	events := o.Events
	if events == nil {
		events = discardEvents{}
	}

	fs := localfs.New()
	clock := sysclock.New()
	hasher := blake3hash.New()
	settings := jsonconfig.NewStore(dir)
	tailnet := tailscalecli.New(tailscalecli.ExecRunner{})
	upload := tokenbucket.New(bandwidth.Rate(cfg.UploadLimit))
	download := tokenbucket.New(bandwidth.Rate(cfg.DownloadLimit))
	app := &App{
		DataDir:   dir,
		Config:    cfg,
		Store:     store,
		FS:        fs,
		Hasher:    hasher,
		Clock:     clock,
		Identity:  identity,
		Events:    events,
		Logger:    logger,
		Settings:  settings,
		Upload:    upload,
		Download:  download,
		Folders:   folders.New(store, store, fs, clock),
		Transfers: transfers.New(store),
		Conflicts: conflicts.New(store, fs, hasher),
		Bundle:    configbundle.New(store, store, settings, fs, clock, ValidateListen),
		Tailnet:   tailnet,
		Network:   network.New(tailnet),
		Control:   daemon.NewClient(unixctl.NewClient(ControlSocketPath(dir))),
	}
	if identity != nil {
		app.Status = status.New(store, identity, clock, dir)
		app.Scan = scan.New(fs, app.Hasher, store, clock, identity, events, cfg.ChunkSize, logger)
		app.Repair = repair.New(fs, app.Hasher, store, clock, identity)

		// The merge base history is only kept while the text merge
		// setting is on, so a device that leaves it off writes nothing
		// and every conflict keeps both versions as before.
		if cfg.TextMerge {
			bases := mergebase.New(store, fs, app.Hasher, clock, logger)
			app.Scan.SetMergeBases(bases)
			app.MergeBases = bases
		}

		handler := syncer.New(store, fs, app.Hasher, identity, cfg.ChunkSize, logger)
		handler.SetUploadLimiter(upload)
		app.Puller = syncer.NewPuller(store, fs, app.Hasher, clock, identity, events,
			syncer.PullerConfig{
				ChunkSize: cfg.ChunkSize, Download: download,
				ChunkPipeline: cfg.ChunkPipeline,
			}, logger)
		app.Puller.SetMergeBases(app.MergeBases)
		// The scanner and the puller share one record of whether a peer
		// has confirmed this device's counter floor: the puller sets it
		// on the first manifest it receives, and the scanner marks
		// every version it writes before that as provisional. It starts
		// unverified on every process start, because a database
		// restored from a backup cannot be told from one that was not.
		app.CounterFloorTrust = counterfloor.New()
		app.Scan.SetCounterFloorTrust(app.CounterFloorTrust)
		app.Puller.SetCounterFloorTrust(app.CounterFloorTrust)
		// The Hello is built per connection because the folder list
		// changes while the process runs, and per peer because it
		// names only the folders that peer is authorized for.
		dialer := tcpwire.NewDialer(identity.TLSCertificate(), func(peer device.ID) ports.HelloInfo {
			return handler.Hello(context.Background(), peer)
		}, logger)
		dialer.SetMaxFileSize(cfg.MaxFileSize)
		app.PeerHandler = handler
		app.Dialer = dialer
		app.Peers = peers.New(store, dialer, clock, identity, listenPort(cfg.Listen))
		app.Peers.SetTailnet(tailnet)
		app.Planner = syncer.NewPlanner(store, fs, dialer)
	}
	return app, nil
}

// Close releases everything the app owns.
func (a *App) Close() error {
	if a == nil || a.Store == nil {
		return nil
	}
	return a.Store.Close()
}

// listenPort returns the port of a configured listen address, or the default
// peer port when it has none.
func listenPort(listen string) int {
	_, port, err := net.SplitHostPort(listen)
	if err != nil {
		return peers.DefaultPort
	}
	number, err := strconv.Atoi(port)
	if err != nil || number == 0 {
		return peers.DefaultPort
	}
	return number
}

// discardEvents drops every event; used when no sink is configured.
type discardEvents struct{}

// Publish ignores the event.
func (discardEvents) Publish(ports.Event) {}
