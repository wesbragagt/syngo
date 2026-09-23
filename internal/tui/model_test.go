package tui

import (
	"context"
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// TestModel_ConnectsToTheDaemonAndShowsIt is the skeleton's whole claim: it
// asks the daemon for the report the command line asks for, and the first
// frame reflects the answer.
func TestModel_ConnectsToTheDaemonAndShowsIt(t *testing.T) {
	runner := newFakeRunner().ok("status", runningStatus())
	m := New(Options{Client: fakeClient(runner)})

	msg := m.Init()()
	if _, ok := msg.(statusMsg); !ok {
		// Init batches; run the batch's parts until a status arrives.
		msg = firstStatus(t, m)
	}
	m, _ = drive(t, m, msg)

	if !runner.called("status") {
		t.Fatalf("the model did not run `status`; calls were %v", runner.calls)
	}
	view := m.View()
	for _, want := range []string{"laptop", "daemon running", "127.0.0.1:22001", "tailscale0"} {
		if !strings.Contains(view, want) {
			t.Errorf("view does not mention %q:\n%s", want, view)
		}
	}
}

// firstStatus runs the model's initial commands until one produces a status.
func firstStatus(t *testing.T, m Model) tea.Msg {
	t.Helper()
	return fetchStatus(m.client)()
}

// TestModel_QuitsOnQAndCtrlC checks both documented ways out.
func TestModel_QuitsOnQAndCtrlC(t *testing.T) {
	for _, key := range []tea.KeyMsg{
		{Type: tea.KeyRunes, Runes: []rune{'q'}},
		{Type: tea.KeyCtrlC},
	} {
		m := New(Options{Client: fakeClient(newFakeRunner())})
		next, run := drive(t, m, key)
		if !next.quitting {
			t.Errorf("%v did not set quitting", key)
		}
		if got := run(); got == nil {
			t.Errorf("%v returned no command, want tea.Quit", key)
		}
	}
}

// TestModel_QuittingStopsThePoll proves the poll does not reschedule itself
// after the user asked to leave, so the program can exit.
func TestModel_QuittingStopsThePoll(t *testing.T) {
	m := New(Options{Client: fakeClient(newFakeRunner())})
	m, _ = drive(t, m, tea.KeyMsg{Type: tea.KeyCtrlC})
	if _, cmd := m.Update(tickMsg{}); cmd != nil {
		t.Fatal("a tick after quitting scheduled more work")
	}
}

// TestModel_NoIdentityIsAnErrorScreenNamingInit is the uninitialized data
// directory: exit 3 from the command line.
func TestModel_NoIdentityIsAnErrorScreenNamingInit(t *testing.T) {
	runner := newFakeRunner().failsWith("status", ExitNotInitialized,
		"data directory is not initialized; run syncthis init")
	m := New(Options{Client: fakeClient(runner)})
	m, _ = drive(t, m, fetchStatus(m.client)())

	if m.screen != screenFatal {
		t.Fatalf("screen is %v, want the error screen", m.screen)
	}
	view := m.View()
	if !strings.Contains(view, "syncthis init") {
		t.Errorf("the error screen does not name the command that fixes it:\n%s", view)
	}
	if !strings.Contains(view, "exit code 3") {
		t.Errorf("the error screen does not report the exit code:\n%s", view)
	}
}

// TestModel_NoDaemonIsItsOwnScreen keeps an initialized device with nothing
// running apart from a device that cannot be read at all.
func TestModel_NoDaemonIsItsOwnScreen(t *testing.T) {
	stopped := runningStatus()
	stopped.Daemon = StatusDaemon{Running: false}
	runner := newFakeRunner().ok("status", stopped)

	m := New(Options{Client: fakeClient(runner)})
	m, _ = drive(t, m, fetchStatus(m.client)())

	if m.screen != screenNoDaemon {
		t.Fatalf("screen is %v, want the no-daemon screen", m.screen)
	}
	view := m.View()
	if !strings.Contains(view, "daemon run") {
		t.Errorf("the no-daemon screen does not name `daemon run`:\n%s", view)
	}
	if !strings.Contains(view, "--data-dir /tmp/data") {
		t.Errorf("the command it names does not carry the data directory:\n%s", view)
	}
}

// TestModel_ARunnerThatCannotRunIsFatal covers the case where the invocation
// itself could not be made, which is not a command exit code.
func TestModel_ARunnerThatCannotRunIsFatal(t *testing.T) {
	runner := newFakeRunner()
	runner.fail = errors.New("no such data directory")
	m := New(Options{Client: fakeClient(runner)})
	m, _ = drive(t, m, fetchStatus(m.client)())

	if m.screen != screenFatal {
		t.Fatalf("screen is %v, want the error screen", m.screen)
	}
	if !strings.Contains(m.View(), "no such data directory") {
		t.Errorf("the error screen hides the reason:\n%s", m.View())
	}
}

// TestModel_TickRefreshes checks the poll asks again and reschedules itself.
func TestModel_TickRefreshes(t *testing.T) {
	runner := newFakeRunner().ok("status", runningStatus())
	m := New(Options{Client: fakeClient(runner)})
	if _, cmd := m.Update(tickMsg{}); cmd == nil {
		t.Fatal("a tick scheduled nothing")
	}
}

// TestClient_AddsTheGlobalFlags checks that every call carries --json and the
// data directory, and that the command it reports is the one a user types.
func TestClient_AddsTheGlobalFlags(t *testing.T) {
	var got []string
	c := Client{DataDir: "/tmp/data", Runner: RunnerFunc(
		func(_ context.Context, args []string) ([]byte, int, error) {
			got = args
			return []byte(`{"schema_version":1}`), 0, nil
		})}

	if _, err := c.Status(context.Background()); err != nil {
		t.Fatalf("status: %v", err)
	}
	want := []string{"--data-dir", "/tmp/data", "--json", "status"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("invocation was %v, want %v", got, want)
	}
	if c.Command("status") != "syncthis --data-dir /tmp/data status" {
		t.Errorf("Command reported %q", c.Command("status"))
	}
}

// TestClient_ReportsTheExitCode checks a failure keeps its documented code and
// message, which is what the screens branch on.
func TestClient_ReportsTheExitCode(t *testing.T) {
	runner := newFakeRunner().failsWith("conflict show x", ExitNotFound, `conflict "x": not found`)
	_, err := fakeClient(runner).ConflictShow(context.Background(), "x")
	if err == nil {
		t.Fatal("a failing command returned no error")
	}
	if got := ExitCode(err); got != ExitNotFound {
		t.Errorf("exit code is %d, want %d", got, ExitNotFound)
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("message is %q", err.Error())
	}
}
