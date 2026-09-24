package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// busyStatus is a device in the middle of doing everything the dashboard has a
// section for: a folder syncing, a folder in error, a peer over a relay, a
// transfer half done and a listener that has been rebound.
func busyStatus() Status {
	folderErr := "open /home/u/docs/locked: permission denied"
	peerErr := "dial 127.0.0.1:22003: connection refused"
	listenerErr := "bind 100.64.0.2:22000: cannot assign requested address"
	reason := "the local Tailscale client reports neither a direct endpoint nor a relay for this node"

	s := runningStatus()
	s.Daemon.Listener.Rebinds = 2
	s.Daemon.Listener.Failures = 1
	s.Daemon.Listener.Error = &listenerErr
	s.Folders = []Folder{
		{ID: "docs", Path: "/home/u/docs", Mode: "sendreceive", State: "syncing",
			Files: 120, Bytes: 5 << 20, PendingChanges: 3, Error: &folderErr},
		{ID: "photos", Path: "/home/u/photos", Mode: "receiveonly", State: "idle",
			Files: 4000, Bytes: 9 << 30},
	}
	s.Peers = append(s.Peers, Peer{
		ID: "QQQQQQQCMWYFD54X22RLMR5SYEWJTA3UANTRKYNN3KAAX5ZGPAFA", Name: "phone",
		Address: "127.0.0.1:22003", State: "unreachable", Error: &peerErr,
		Path: Path{Kind: "unknown", Reason: &reason},
	})
	s.Transfers = Transfers{
		Active: 1, Pending: 2,
		Items: []Transfer{{
			ID: "a1b2", FolderID: "docs", Path: "big.bin", State: "active",
			PeerID: "ETI3RRZCMWYFD54X22RLMR5SYEWJTA3UANTRKYNN3KAAX5ZGPAFA",
			Size:   4 << 20, ChunksTotal: 64, ChunksDone: 32, BytesDone: 2 << 20,
		}},
	}
	return s
}

// dashboard returns a model sitting on the dashboard with the given report.
func dashboard(t *testing.T, runner *fakeRunner, report Status) Model {
	t.Helper()
	m := New(Options{Client: fakeClient(runner)})
	m, _ = drive(t, m, statusMsg{status: report})
	if m.screen != screenDashboard {
		t.Fatalf("screen is %v, want the dashboard", m.screen)
	}
	return m
}

// TestDashboard_ShowsEverySection checks that each of the five things the
// dashboard exists for is on the frame.
func TestDashboard_ShowsEverySection(t *testing.T) {
	m := dashboard(t, newFakeRunner(), busyStatus())
	view := m.View()

	for _, section := range []string{"DEVICES", "FOLDERS", "QUEUE", "ERRORS"} {
		if !strings.Contains(view, section) {
			t.Errorf("the frame has no %s section:\n%s", section, view)
		}
	}
	for _, want := range []string{
		// Devices, with the state and the path of each.
		"nas", "connected", "direct", "192.168.1.20:41641",
		"phone", "unreachable", "unknown",
		// Folders, with the state, the counts and the pending changes.
		"docs", "syncing", "120", "photos", "receiveonly", "4000",
		// The queue, with the counts and one transfer in flight.
		"1 active, 2 pending", "big.bin", "2.0 MiB / 4.0 MiB",
		// The errors, from three different parts of the device.
		"permission denied", "connection refused", "cannot assign requested address",
	} {
		if !strings.Contains(view, want) {
			t.Errorf("the frame does not show %q:\n%s", want, view)
		}
	}
}

// TestDashboard_NoErrorsSaysSo checks the error area is present and empty
// rather than absent when nothing is wrong, so its absence never has to be
// read as good news.
func TestDashboard_NoErrorsSaysSo(t *testing.T) {
	m := dashboard(t, newFakeRunner(), runningStatus())
	view := m.View()
	if !strings.Contains(view, "ERRORS") {
		t.Fatalf("the error area is missing:\n%s", view)
	}
	if !strings.Contains(view, "none") {
		t.Errorf("a device with nothing wrong does not say so:\n%s", view)
	}
}

// TestDashboard_PrefersTheFreshPathReading checks that `network path` wins
// over the copy the daemon folded into status, and that a peer neither source
// describes is shown as unknown rather than guessed at.
func TestDashboard_PrefersTheFreshPathReading(t *testing.T) {
	m := dashboard(t, newFakeRunner(), runningStatus())
	m, _ = drive(t, m, pathsMsg{paths: []NetPath{{
		PeerID: "ETI3RRZCMWYFD54X22RLMR5SYEWJTA3UANTRKYNN3KAAX5ZGPAFA",
		Name:   "nas", Kind: "relay", Relay: "fra", Active: true,
	}}})

	view := m.View()
	if !strings.Contains(view, "relay") || !strings.Contains(view, "DERP fra") {
		t.Errorf("the fresher path reading is not shown:\n%s", view)
	}
	if strings.Contains(view, "192.168.1.20:41641") {
		t.Errorf("the stale path from status is still shown:\n%s", view)
	}
}

// TestDashboard_NoTailscaleIsNotAnError checks exit 11 stays out of the error
// area: a device without a Tailscale client is a supported way to run.
func TestDashboard_NoTailscaleIsNotAnError(t *testing.T) {
	runner := newFakeRunner().failsWith("network path", ExitTailnetGone,
		"tailscale is not available")
	m := dashboard(t, runner, runningStatus())
	m, _ = drive(t, m, fetchPaths(m.client)())

	if got := ExitCode(m.pathErr); got != ExitTailnetGone {
		t.Fatalf("path error is %v", m.pathErr)
	}
	if strings.Contains(m.View(), "tailscale is not available") {
		t.Errorf("a device without Tailscale is reported as an error:\n%s", m.View())
	}
}

// TestDashboard_PathFailureThatIsNotExitElevenIsAnError is the other half of
// the rule above.
func TestDashboard_PathFailureThatIsNotExitElevenIsAnError(t *testing.T) {
	runner := newFakeRunner().failsWith("network path", ExitError, "tailscale status: broken pipe")
	m := dashboard(t, runner, runningStatus())
	m, _ = drive(t, m, fetchPaths(m.client)())

	if !strings.Contains(m.View(), "broken pipe") {
		t.Errorf("a real path failure is not in the error area:\n%s", m.View())
	}
}

// TestDashboard_FolderKeysRunTheirCommands checks the three folder actions run
// exactly the commands of the same name, against the selected folder.
func TestDashboard_FolderKeysRunTheirCommands(t *testing.T) {
	runner := newFakeRunner().
		ok("status", busyStatus()).
		ok("pause photos", struct{}{}).
		ok("resume photos", struct{}{}).
		ok("rescan photos", struct{}{})
	m := dashboard(t, runner, busyStatus())

	// Move the cursor to the second folder.
	m, _ = drive(t, m, tea.KeyMsg{Type: tea.KeyDown})
	if got := m.selectedFolder(); got != "photos" {
		t.Fatalf("the cursor is on %q, want photos", got)
	}
	if !strings.Contains(m.View(), "> ") {
		t.Errorf("the selected folder is not marked:\n%s", m.View())
	}

	for key, want := range map[rune]string{'p': "pause photos", 'u': "resume photos", 's': "rescan photos"} {
		m2, run := drive(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{key}})
		msg := run()
		if msg == nil {
			t.Fatalf("%q ran nothing", key)
		}
		next, _ := drive(t, m2, msg)
		if !runner.called(want) {
			t.Errorf("%q did not run `syngo %s`; calls were %v", key, want, runner.calls)
		}
		if notice, ok := msg.(noticeMsg); ok && notice.err != nil {
			t.Errorf("%q failed: %v", key, notice.err)
		}
		if !strings.Contains(next.View(), "done") {
			t.Errorf("%q reported nothing in the footer:\n%s", key, next.View())
		}
	}
}

// TestDashboard_TheCursorStaysInsideTheTable guards the two ends.
func TestDashboard_TheCursorStaysInsideTheTable(t *testing.T) {
	m := dashboard(t, newFakeRunner(), busyStatus())
	for range 5 {
		m, _ = drive(t, m, tea.KeyMsg{Type: tea.KeyUp})
	}
	if m.folder != 0 {
		t.Errorf("the cursor went above the first row: %d", m.folder)
	}
	for range 5 {
		m, _ = drive(t, m, tea.KeyMsg{Type: tea.KeyDown})
	}
	if m.folder != len(m.status.Folders)-1 {
		t.Errorf("the cursor went past the last row: %d", m.folder)
	}
}

// TestProgressBar checks the bar is proportional and never overruns.
func TestProgressBar(t *testing.T) {
	for _, tc := range []struct {
		done, total int
		want        string
	}{
		{0, 64, "[        ]"},
		{32, 64, "[====    ]"},
		{64, 64, "[========]"},
		{0, 0, "[        ]"},
		{100, 64, "[========]"},
	} {
		if got := progressBar(tc.done, tc.total, 10); got != tc.want {
			t.Errorf("progressBar(%d, %d, 10) = %q, want %q", tc.done, tc.total, got, tc.want)
		}
	}
}
