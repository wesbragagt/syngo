package fakes

import (
	"context"
	"sync"

	"github.com/wesbragagt/syncthis/internal/app/ports"
)

// Settings is an in-memory ports.SettingsStore.
type Settings struct {
	mu      sync.Mutex
	current ports.Settings
	// queued are returned by Load one after another; the last one stays.
	queued []ports.Settings
	saved  []ports.Settings
	err    error
}

// NewSettings returns a store holding s.
func NewSettings(s ports.Settings) *Settings {
	return &Settings{current: s}
}

// Load returns the settings, or the next queued ones.
func (s *Settings) Load(_ context.Context) (ports.Settings, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return ports.Settings{}, s.err
	}
	if len(s.queued) > 0 {
		s.current = s.queued[0]
		s.queued = s.queued[1:]
	}
	return s.current, nil
}

// Save records the settings.
func (s *Settings) Save(_ context.Context, set ports.Settings) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	s.current = set
	s.saved = append(s.saved, set)
	return nil
}

// Queue makes the next Loads return these settings in order.
func (s *Settings) Queue(list ...ports.Settings) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.queued = append(s.queued, list...)
}

// Saved returns what Save was called with, in order.
func (s *Settings) Saved() []ports.Settings {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]ports.Settings(nil), s.saved...)
}

// FailWith makes every call fail with err.
func (s *Settings) FailWith(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.err = err
}
