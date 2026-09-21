package fakes

import "github.com/wesbragagt/syncthis/internal/app/ports"

// Compile-time checks that every fake satisfies its port.
var (
	_ ports.Clock            = (*Clock)(nil)
	_ ports.Hasher           = Hasher{}
	_ ports.Identity         = Identity{}
	_ ports.EventSink        = (*Events)(nil)
	_ ports.Watcher          = (*Watcher)(nil)
	_ ports.FileSystem       = (*FileSystem)(nil)
	_ ports.MetadataStore    = (*Store)(nil)
	_ ports.Dialer           = (*Dialer)(nil)
	_ ports.PeerConn         = (*PeerConn)(nil)
	_ ports.Limiter          = (*Limiter)(nil)
	_ ports.SettingsStore    = (*Settings)(nil)
	_ ports.TailnetDiscovery = (*Tailnet)(nil)
)
