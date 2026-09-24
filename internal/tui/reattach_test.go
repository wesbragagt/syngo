package tui

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// TestRun_ACancelledContextIsACleanExit is what a dropped SSH session or a
// terminal that went away looks like from here. It must not be reported as a
// syngo failure, because nothing about synchronization failed.
func TestRun_ACancelledContextIsACleanExit(t *testing.T) {
	runner := newFakeRunner().ok("status", runningStatus())
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Options{Client: fakeClient(runner), Poll: 10 * time.Millisecond},
			strings.NewReader(""), io.Discard)
	}()

	// Give the program a moment to be running, then take the terminal
	// away under it.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("losing the terminal was reported as a failure: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the program did not return after its context was cancelled")
	}
}

// TestCleanExit_OnlyHidesTheWaysATerminalGoesAway keeps a real failure a real
// failure.
func TestCleanExit_OnlyHidesTheWaysATerminalGoesAway(t *testing.T) {
	if got := cleanExit(context.Canceled); got != nil {
		t.Errorf("a cancelled context returned %v", got)
	}
	if got := cleanExit(tea.ErrProgramKilled); got != nil {
		t.Errorf("a killed program returned %v", got)
	}
	broken := errors.New("the output is not a terminal")
	if got := cleanExit(broken); !errors.Is(got, broken) {
		t.Errorf("a real failure was hidden: %v", got)
	}
}

// TestModel_HoldsNothingTheDaemonNeeds is the reattach claim stated as a unit
// test: a second model, built from nothing and given the same report, renders
// the same live view as one that has been running. If the interface had
// accumulated any synchronization state of its own, the two frames would
// differ.
func TestModel_HoldsNothingTheDaemonNeeds(t *testing.T) {
	report := busyStatus()
	paths := pathsMsg{paths: []NetPath{{
		PeerID: "ETI3RRZCMWYFD54X22RLMR5SYEWJTA3UANTRKYNN3KAAX5ZGPAFA",
		Name:   "nas", Kind: "direct", Endpoint: "192.168.1.20:41641", Active: true,
	}}}

	// One that has been running for a while: several refreshes, a trip to
	// the conflict screen and back, a filter typed into the log.
	long := New(Options{Client: fakeClient(conflictRunner())})
	for range 5 {
		long, _ = drive(t, long, statusMsg{status: runningStatus()})
	}
	long, _ = drive(t, long, statusMsg{status: report})
	long, _ = drive(t, long, paths)

	// One just started, given the same answers.
	fresh := New(Options{Client: fakeClient(conflictRunner())})
	fresh, _ = drive(t, fresh, statusMsg{status: report})
	fresh, _ = drive(t, fresh, paths)

	if long.dashboardView() != fresh.dashboardView() {
		t.Errorf("a restarted TUI shows something different:\n--- running ---\n%s\n--- fresh ---\n%s",
			long.dashboardView(), fresh.dashboardView())
	}
}

// TestRun_WritesOnlyToTheStreamItWasGiven checks the interface draws where it
// was told to and nowhere else, which is what lets it run under tmux, under
// SSH and under a test harness with pipes rather than a terminal.
func TestRun_WritesOnlyToTheStreamItWasGiven(t *testing.T) {
	runner := newFakeRunner().ok("status", runningStatus())
	// The terminal is taken away rather than quit from, because a 'q' on
	// an input that is not a terminal arrives before the first refresh.
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	var out safeBuffer
	if err := Run(ctx, Options{Client: fakeClient(runner), Poll: 10 * time.Millisecond},
		strings.NewReader(""), &out); err != nil {
		t.Fatalf("run: %v", err)
	}
	if out.String() == "" {
		t.Fatal("the interface drew nothing on the stream it was given")
	}
	if !strings.Contains(stripANSI(out.String()), "laptop") {
		t.Errorf("the frames do not describe the device:\n%s", stripANSI(out.String()))
	}
}

// safeBuffer is a bytes.Buffer a test can read while Bubble Tea's renderer
// goroutine is still writing to it.
type safeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

// Write implements io.Writer.
func (b *safeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

// String returns everything written so far.
func (b *safeBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
