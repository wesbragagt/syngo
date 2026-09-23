package tui

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
)

// fakeRunner answers invocations from a table keyed by the command line,
// without the global flags. It records every call, so a test can assert that a
// screen ran the command docs/TUI.md says it runs.
type fakeRunner struct {
	mu sync.Mutex
	// answers maps a joined argument list ("status", "conflict list") to
	// what the command printed and the code it exited with.
	answers map[string]fakeAnswer
	// calls are the invocations made, with the global flags stripped.
	calls []string
	// fail is returned instead of running anything when it is not nil.
	fail error
}

// fakeAnswer is one canned command result.
type fakeAnswer struct {
	stdout string
	code   int
}

// newFakeRunner builds a runner that knows nothing; add answers with set.
func newFakeRunner() *fakeRunner {
	return &fakeRunner{answers: map[string]fakeAnswer{}}
}

// ok makes a command succeed with the given document.
func (r *fakeRunner) ok(key string, doc any) *fakeRunner {
	encoded, err := json.Marshal(doc)
	if err != nil {
		panic(err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.answers[key] = fakeAnswer{stdout: string(encoded)}
	return r
}

// failsWith makes a command exit with a code and a message, in the failure
// shape docs/CLI.md documents.
func (r *fakeRunner) failsWith(key string, code int, message string) *fakeRunner {
	doc, err := json.Marshal(map[string]any{
		"error": map[string]any{"code": code, "message": message},
	})
	if err != nil {
		panic(err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.answers[key] = fakeAnswer{stdout: string(doc), code: code}
	return r
}

// Run implements Runner.
func (r *fakeRunner) Run(_ context.Context, args []string) ([]byte, int, error) {
	if r.fail != nil {
		return nil, 0, r.fail
	}
	key := commandKey(args)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, key)
	answer, ok := r.answers[key]
	if !ok {
		return []byte(`{"error":{"code":2,"message":"fake runner: no answer for ` + key + `"}}`), 2, nil
	}
	return []byte(answer.stdout), answer.code, nil
}

// called reports whether an invocation was made.
func (r *fakeRunner) called(key string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, call := range r.calls {
		if call == key {
			return true
		}
	}
	return false
}

// commandKey strips the flags the client adds to every call so a test keys on
// the command itself.
func commandKey(args []string) string {
	kept := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--json", "--yes":
			continue
		case "--data-dir":
			i++
			continue
		}
		kept = append(kept, args[i])
	}
	return strings.Join(kept, " ")
}

// fakeClient builds a client over a runner.
func fakeClient(r Runner) Client { return Client{Runner: r, DataDir: "/tmp/data"} }

// runningStatus is a device with a daemon, one folder and one peer, in the
// shape docs/CLI.md documents for `status --json`.
func runningStatus() Status {
	started := "2026-09-21T08:32:15Z"
	seen := "2026-09-21T08:32:15Z"
	observed := "2026-09-21T08:32:45Z"
	return Status{
		SchemaVersion: 1,
		Device:        StatusDevice{ID: "RCUFGP7AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", Name: "laptop", DataDir: "/tmp/data"},
		Daemon: StatusDaemon{
			Running: true, PID: 3776214, Listen: "127.0.0.1:22001", StartedAt: &started,
			Listener: Listener{Bound: true, Address: "127.0.0.1:22001", Interface: "tailscale0"},
		},
		Folders: []Folder{{
			ID: "docs", Path: "/home/u/docs", Mode: "sendreceive", DeletePolicy: "propagate",
			State: "idle", Files: 1, Bytes: 13, InWindow: true,
		}},
		Peers: []Peer{{
			ID: "ETI3RRZCMWYFD54X22RLMR5SYEWJTA3UANTRKYNN3KAAX5ZGPAFA", Name: "nas",
			Address: "127.0.0.1:22002", Verified: true, Connected: true, LastSeenAt: &seen,
			State: "connected",
			Path:  Path{Kind: "direct", Endpoint: "192.168.1.20:41641", Active: true, ObservedAt: &observed},
		}},
		Transfers: Transfers{},
	}
}

// drive applies a message to the model and returns the model and the command,
// saving every test the type assertion.
func drive(t *testing.T, m Model, msg any) (Model, func() any) {
	t.Helper()
	next, cmd := m.Update(msg)
	out, ok := next.(Model)
	if !ok {
		t.Fatalf("Update returned %T, want tui.Model", next)
	}
	return out, func() any {
		if cmd == nil {
			return nil
		}
		return cmd()
	}
}
