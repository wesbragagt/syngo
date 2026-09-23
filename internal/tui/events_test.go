package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// openLogs puts the model on the log screen.
func openLogs(t *testing.T, m Model) Model {
	t.Helper()
	m = press(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'l'}})
	if m.screen != screenEvents {
		t.Fatalf("screen is %v, want the log screen", m.screen)
	}
	return m
}

// TestEvents_RecordsWhatChangedBetweenTwoReports is the source that needs no
// setup: the TUI's own reading of the daemon, one poll against the next.
func TestEvents_RecordsWhatChangedBetweenTwoReports(t *testing.T) {
	m := dashboard(t, newFakeRunner(), runningStatus())

	next := runningStatus()
	next.Folders[0].State = "syncing"
	next.Peers[0].State = "unreachable"
	peerErr := "dial 127.0.0.1:22002: connection refused"
	next.Peers[0].Error = &peerErr
	next.Transfers = Transfers{Active: 1, Items: []Transfer{{
		ID: "a1b2", FolderID: "docs", Path: "big.bin", State: "active",
		PeerID: "ETI3RRZCMWYFD54X22RLMR5SYEWJTA3UANTRKYNN3KAAX5ZGPAFA",
		Size:   4 << 20, ChunksTotal: 64,
	}}}
	next.Conflicts.Unresolved = 1
	m, _ = drive(t, m, statusMsg{status: next})

	m = openLogs(t, m)
	view := m.View()
	for _, want := range []string{
		"folder docs: idle → syncing",
		"peer nas: connected → unreachable",
		"peer nas: dial 127.0.0.1:22002: connection refused",
		"transfer docs/big.bin: started, 4.0 MiB from ETI3RRZ",
		"conflict: 1 unresolved, was 0",
	} {
		if !strings.Contains(view, want) {
			t.Errorf("the log does not carry %q:\n%s", want, view)
		}
	}
}

// TestEvents_TheFirstReportIsNotNews checks a TUI started against a running
// device does not open with a page of invented changes.
func TestEvents_TheFirstReportIsNotNews(t *testing.T) {
	m := dashboard(t, newFakeRunner(), busyStatus())
	if len(m.events.events) != 0 {
		t.Errorf("the first report produced %d lines, want none: %v",
			len(m.events.events), m.events.events)
	}
}

// TestEvents_TailsTheDaemonLogFile checks the other source: a file the
// operator sent `daemon run`'s stderr to.
func TestEvents_TailsTheDaemonLogFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.log")
	first := `{"time":"2026-09-21T08:32:15Z","level":"INFO","msg":"peer connected","peer":"ETI3RRZ"}` + "\n"
	if err := os.WriteFile(path, []byte(first), 0o600); err != nil {
		t.Fatalf("write the log: %v", err)
	}

	m := New(Options{Client: fakeClient(newFakeRunner()), LogFile: path})
	m, _ = drive(t, m, statusMsg{status: runningStatus()})
	m, _ = drive(t, m, tailLog(path, 0)())
	m = openLogs(t, m)

	view := m.View()
	if !strings.Contains(view, "peer connected peer=ETI3RRZ") {
		t.Errorf("the log line is not shown:\n%s", view)
	}
	if !strings.Contains(view, "08:32:15") {
		t.Errorf("the log line's own time is not shown:\n%s", view)
	}
	// The header names the file, cut to the width of the terminal.
	if !strings.Contains(view, "tailing "+truncate(path, maxInt(m.width-30, 20))) {
		t.Errorf("the screen does not say which file it tails:\n%s", view)
	}

	// A second read takes only what was appended since the first.
	second := `{"time":"2026-09-21T08:32:20Z","level":"ERROR","msg":"scan failed","folder":"docs"}` + "\n"
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("append to the log: %v", err)
	}
	if _, err := f.WriteString(second); err != nil {
		t.Fatalf("append to the log: %v", err)
	}
	_ = f.Close()

	m, _ = drive(t, m, tailLog(path, m.events.offset)())
	if got := len(m.events.events); got != 2 {
		t.Fatalf("the log holds %d lines, want 2: %v", got, m.events.events)
	}
	if !strings.Contains(m.View(), "scan failed folder=docs") {
		t.Errorf("the appended line is not shown:\n%s", m.View())
	}
}

// TestEvents_ATextLogLineIsKeptAsItStands checks the default log format is
// shown rather than half-parsed by a second definition of it.
func TestEvents_ATextLogLineIsKeptAsItStands(t *testing.T) {
	raw := `time=2026-09-21T08:32:15Z level=INFO msg="peer connected" peer=ETI3RRZ`
	got := parseLogLine(raw)
	if got.text != raw {
		t.Errorf("a text line was rewritten to %q", got.text)
	}
	if got.level != "log" {
		t.Errorf("a text line got level %q", got.level)
	}
}

// TestEvents_SearchFiltersAsItIsTyped is the claim of a searchable log.
func TestEvents_SearchFiltersAsItIsTyped(t *testing.T) {
	m := dashboard(t, newFakeRunner(), runningStatus())
	next := runningStatus()
	next.Folders[0].State = "scanning"
	next.Peers[0].State = "connecting"
	m, _ = drive(t, m, statusMsg{status: next})
	m = openLogs(t, m)

	if got := len(m.events.filtered()); got != 2 {
		t.Fatalf("the log holds %d lines before filtering, want 2", got)
	}

	m = press(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}})
	if !m.events.searching {
		t.Fatal("/ did not open the search box")
	}
	m = typeText(t, m, "peer")

	matches := m.events.filtered()
	if len(matches) != 1 {
		t.Fatalf("filtering on \"peer\" matched %d lines: %v", len(matches), matches)
	}
	if !strings.Contains(matches[0].text, "peer nas") {
		t.Errorf("the wrong line matched: %q", matches[0].text)
	}
	if !strings.Contains(m.View(), "1 of 2 lines") {
		t.Errorf("the screen does not say how much the filter hid:\n%s", m.View())
	}

	// The search is case-insensitive.
	m = press(t, m, tea.KeyMsg{Type: tea.KeyBackspace})
	m = press(t, m, tea.KeyMsg{Type: tea.KeyBackspace})
	m = press(t, m, tea.KeyMsg{Type: tea.KeyBackspace})
	m = press(t, m, tea.KeyMsg{Type: tea.KeyBackspace})
	m = typeText(t, m, "SCANNING")
	if got := len(m.events.filtered()); got != 1 {
		t.Errorf("an uppercase query matched %d lines, want 1", got)
	}
}

// TestEvents_EscapeLeavesTheSearchBeforeTheScreen checks the one key that has
// two jobs here does the nearer one first.
func TestEvents_EscapeLeavesTheSearchBeforeTheScreen(t *testing.T) {
	m := dashboard(t, newFakeRunner(), runningStatus())
	m = openLogs(t, m)
	m = press(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}})
	m = typeText(t, m, "docs")

	m, _ = drive(t, m, tea.KeyMsg{Type: tea.KeyEsc})
	if m.screen != screenEvents {
		t.Fatal("escape left the screen while the search box had the keyboard")
	}
	if m.events.searching {
		t.Error("escape did not leave the search box")
	}
	if m.events.query.Value() != "docs" {
		t.Errorf("escape threw the filter away: %q", m.events.query.Value())
	}

	m, _ = drive(t, m, tea.KeyMsg{Type: tea.KeyEsc})
	if m.screen != screenDashboard {
		t.Error("a second escape did not go back to the dashboard")
	}
}

// TestEvents_ClearingTheFilterShowsEverythingAgain checks x.
func TestEvents_ClearingTheFilterShowsEverythingAgain(t *testing.T) {
	m := dashboard(t, newFakeRunner(), runningStatus())
	next := runningStatus()
	next.Folders[0].State = "scanning"
	next.Peers[0].State = "connecting"
	m, _ = drive(t, m, statusMsg{status: next})
	m = openLogs(t, m)

	m = press(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}})
	m = typeText(t, m, "peer")
	m = press(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	m = press(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'x'}})

	if got := len(m.events.filtered()); got != 2 {
		t.Errorf("clearing the filter left %d lines, want 2", got)
	}
}

// TestEvents_TheBufferIsBounded checks a TUI left running for days does not
// grow without limit.
func TestEvents_TheBufferIsBounded(t *testing.T) {
	e := newEvents("")
	for i := range eventBufferSize + 500 {
		e.append(time.Time{}, "seen", sprintf("line %d", i))
	}
	if got := len(e.events); got != eventBufferSize {
		t.Errorf("the buffer holds %d lines, want %d", got, eventBufferSize)
	}
	if !strings.Contains(e.events[len(e.events)-1].text, sprintf("line %d", eventBufferSize+499)) {
		t.Errorf("the newest line was dropped: %q", e.events[len(e.events)-1].text)
	}
}

// TestEvents_TheLogIsFilledWhicheverScreenIsShowing checks that opening the
// log shows what happened rather than only what happens next.
func TestEvents_TheLogIsFilledWhicheverScreenIsShowing(t *testing.T) {
	m := dashboard(t, newFakeRunner(), runningStatus())
	next := runningStatus()
	next.Folders[0].State = "syncing"
	m, _ = drive(t, m, statusMsg{status: next})

	if m.screen != screenDashboard {
		t.Fatalf("the test left the dashboard")
	}
	m = openLogs(t, m)
	if !strings.Contains(m.View(), "idle → syncing") {
		t.Errorf("a change that happened before the log was opened is missing:\n%s", m.View())
	}
}
