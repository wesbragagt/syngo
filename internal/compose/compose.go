// Package compose is the composition root: it resolves the data directory,
// opens the adapters and wires them into the application services.
package compose

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/wesbragagt/syncthis/internal/adapters/blake3hash"
	"github.com/wesbragagt/syncthis/internal/adapters/identityfile"
	"github.com/wesbragagt/syncthis/internal/adapters/jsonconfig"
	"github.com/wesbragagt/syncthis/internal/adapters/localfs"
	"github.com/wesbragagt/syncthis/internal/adapters/sqlitestore"
	"github.com/wesbragagt/syncthis/internal/adapters/sysclock"
	"github.com/wesbragagt/syncthis/internal/app/folders"
	"github.com/wesbragagt/syncthis/internal/app/ports"
	"github.com/wesbragagt/syncthis/internal/app/status"
)

// ErrNotInitialized is returned when the data directory holds no identity and
// one is required. Commands map it to the "run syncthis init" exit code.
var ErrNotInitialized = identityfile.ErrNotInitialized

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

	Folders *folders.Service
	Status  *status.Service
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

// InitSettings are the values `syncthis init` may write into the config.
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
	updated := cfg
	updated.DeviceName = identity.Name()
	if settings.Listen != "" {
		updated.Listen = settings.Listen
	}
	if created || updated != cfg {
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
	app := &App{
		DataDir:  dir,
		Config:   cfg,
		Store:    store,
		FS:       fs,
		Hasher:   blake3hash.New(),
		Clock:    clock,
		Identity: identity,
		Events:   events,
		Logger:   logger,
		Folders:  folders.New(store, fs, clock),
	}
	if identity != nil {
		app.Status = status.New(store, identity, clock, dir)
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

// discardEvents drops every event; used when no sink is configured.
type discardEvents struct{}

// Publish ignores the event.
func (discardEvents) Publish(ports.Event) {}
