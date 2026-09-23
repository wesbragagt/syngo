package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// typeText feeds a string to the model one key at a time, the way a terminal
// would, and drains any command each key produced.
func typeText(t *testing.T, m Model, text string) Model {
	t.Helper()
	for _, r := range text {
		m, _ = drive(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	return m
}

// press sends one key and keeps applying whatever each command produced, so a
// test walks the wizard the way the event loop does: `peer add` answering with
// a message whose handler runs `folder grant` is one key press to the user and
// two rounds here.
func press(t *testing.T, m Model, key tea.KeyMsg) Model {
	t.Helper()
	var msg tea.Msg = key
	// A bound rather than a plain loop: a screen that answered its own
	// message forever would otherwise hang the test instead of failing it.
	for range 10 {
		next, run := drive(t, m, msg)
		m = next
		produced := run()
		if produced == nil {
			return m
		}
		msg = produced
	}
	t.Fatalf("a key press did not settle after ten rounds")
	return m
}

// enter is the key that advances a step.
var enter = tea.KeyMsg{Type: tea.KeyEnter}

// setupRunner answers every command the guided walk runs, in the shapes
// docs/CLI.md documents.
func setupRunner() *fakeRunner {
	accepted := true
	return newFakeRunner().
		failsWith("status", ExitNotInitialized, "data directory is not initialized").
		ok("init --name laptop --listen 127.0.0.1:22001", InitResult{
			DeviceID: "RCUFGP7AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
			Name:     "laptop", Listen: "127.0.0.1:22001", Created: true,
		}).
		ok("folder add /tmp/docs --id docs", FolderAdded{
			ID: "docs", Path: "/tmp/docs", Mode: "sendreceive", DeletePolicy: "propagate",
		}).
		ok("peer test 127.0.0.1:22002", PeerTested{
			Address: "127.0.0.1:22002", Name: "nas",
			DeviceID:      "ETI3RRZCMWYFD54X22RLMR5SYEWJTA3UANTRKYNN3KAAX5ZGPAFA",
			TLSOK:         true,
			Authenticated: true,
		}).
		ok("peer add 127.0.0.1:22002 --device-id ETI3RRZCMWYFD54X22RLMR5SYEWJTA3UANTRKYNN3KAAX5ZGPAFA",
			PeerAdded{
				ID:      "ETI3RRZCMWYFD54X22RLMR5SYEWJTA3UANTRKYNN3KAAX5ZGPAFA",
				Name:    "nas",
				Address: "127.0.0.1:22002", Verified: true, Accepted: &accepted,
			}).
		ok("folder grant docs ETI3RRZCMWYFD54X22RLMR5SYEWJTA3UANTRKYNN3KAAX5ZGPAFA",
			Granted{Folder: "docs", Device: "ETI3RRZCMWYFD54X22RLMR5SYEWJTA3UANTRKYNN3KAAX5ZGPAFA"})
}

// TestSetup_WalksFromNothingToAPairedFolder drives the whole guided flow
// through Update and View, with no terminal, and checks that it ran exactly
// the commands docs/TUI.md lists and ended paired.
func TestSetup_WalksFromNothingToAPairedFolder(t *testing.T) {
	runner := setupRunner()
	m := New(Options{Client: fakeClient(runner)})

	// The first status answers exit 3, which starts the walk.
	m, _ = drive(t, m, fetchStatus(m.client)())
	if m.screen != screenSetup {
		t.Fatalf("screen is %v, want the guided setup", m.screen)
	}

	// Device name, then listen address; enter on the address runs init.
	m = typeText(t, m, "laptop")
	m = press(t, m, enter)
	m = typeText(t, m, "127.0.0.1:22001")
	m = press(t, m, enter)
	if m.setup.step != stepFolderPath {
		t.Fatalf("after init the step is %v, want the folder path; err %v", m.setup.step, m.setup.err)
	}

	// The folder path proposes an id from the directory name.
	m = typeText(t, m, "/tmp/docs")
	m = press(t, m, enter)
	if got := m.setup.value(stepFolderID); got != "docs" {
		t.Errorf("proposed folder id is %q, want docs", got)
	}
	m = press(t, m, enter)
	if m.setup.step != stepDaemon {
		t.Fatalf("after folder add the step is %v, want the daemon wait; err %v", m.setup.step, m.setup.err)
	}
	if !strings.Contains(m.View(), "daemon run") {
		t.Errorf("the daemon step does not name the command:\n%s", m.View())
	}

	// A daemon appears. The walk moves on by itself.
	empty := runningStatus()
	empty.Peers, empty.Folders = nil, nil
	m, _ = drive(t, m, statusMsg{status: empty})
	if m.setup.step != stepPeerHost {
		t.Fatalf("a running daemon did not advance the walk; step is %v", m.setup.step)
	}

	// The peer address is tested before it is trusted.
	m = typeText(t, m, "127.0.0.1:22002")
	m = press(t, m, enter)
	if m.setup.step != stepPeerConfirm {
		t.Fatalf("after peer test the step is %v; err %v", m.setup.step, m.setup.err)
	}
	confirm := m.View()
	if !strings.Contains(confirm, "ETI3RRZCMWYFD54X22RLMR5SYEWJTA3UANTRKYNN3KAAX5ZGPAFA") {
		t.Errorf("the confirmation does not show the identity it would trust:\n%s", confirm)
	}

	// 'y' runs peer add, whose answer chains straight into folder grant.
	m = press(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	if m.setup.step != stepConverge {
		t.Fatalf("after pairing the step is %v; err %v", m.setup.step, m.setup.err)
	}

	// The peer connects, which is where the walk calls itself finished.
	m, _ = drive(t, m, statusMsg{status: runningStatus()})
	if !m.setup.done {
		t.Fatalf("a connected peer did not finish the walk; step %v", m.setup.step)
	}
	if !strings.Contains(m.View(), "paired and synchronizing") {
		t.Errorf("the last frame does not say the walk succeeded:\n%s", m.View())
	}

	// Exactly the documented commands, and nothing invented.
	for _, want := range []string{
		"init --name laptop --listen 127.0.0.1:22001",
		"folder add /tmp/docs --id docs",
		"peer test 127.0.0.1:22002",
		"peer add 127.0.0.1:22002 --device-id ETI3RRZCMWYFD54X22RLMR5SYEWJTA3UANTRKYNN3KAAX5ZGPAFA",
		"folder grant docs ETI3RRZCMWYFD54X22RLMR5SYEWJTA3UANTRKYNN3KAAX5ZGPAFA",
	} {
		if !runner.called(want) {
			t.Errorf("the walk never ran `syncthis %s`; it ran %v", want, runner.calls)
		}
	}

	// Any key then leaves for the dashboard.
	m, _ = drive(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'x'}})
	if m.screen != screenDashboard {
		t.Errorf("screen after the walk is %v, want the dashboard", m.screen)
	}
}

// TestSetup_ShowsTheCommandOfEveryStep is the promise that nothing in the
// interface is a private operation: the step names the command it runs.
func TestSetup_ShowsTheCommandOfEveryStep(t *testing.T) {
	m := New(Options{Client: fakeClient(setupRunner())})
	m, _ = drive(t, m, fetchStatus(m.client)())

	for step, want := range map[setupStep]string{
		stepName:        "syncthis --data-dir /tmp/data init",
		stepFolderPath:  "syncthis --data-dir /tmp/data folder add",
		stepDaemon:      "syncthis --data-dir /tmp/data daemon run",
		stepPeerHost:    "syncthis --data-dir /tmp/data peer test",
		stepPeerConfirm: "syncthis --data-dir /tmp/data peer add",
	} {
		m.setup.focus(step)
		if got := m.View(); !strings.Contains(got, want) {
			t.Errorf("step %v does not show %q:\n%s", step, want, got)
		}
	}
}

// TestSetup_AFailedStepStaysOnItsField checks that a command that failed
// leaves the user where the mistake is, with the reason.
func TestSetup_AFailedStepStaysOnItsField(t *testing.T) {
	runner := setupRunner().
		failsWith("folder add /tmp/nope --id nope", ExitUsage,
			`folder path "/tmp/nope": not a directory`)
	m := New(Options{Client: fakeClient(runner)})
	m, _ = drive(t, m, fetchStatus(m.client)())

	m = typeText(t, m, "laptop")
	m = press(t, m, enter)
	m = typeText(t, m, "127.0.0.1:22001")
	m = press(t, m, enter)
	m = typeText(t, m, "/tmp/nope")
	m = press(t, m, enter)
	m = press(t, m, enter)

	if m.setup.step != stepFolderID {
		t.Fatalf("a failed folder add moved on to %v", m.setup.step)
	}
	if !strings.Contains(m.View(), "not a directory") {
		t.Errorf("the failure is not shown:\n%s", m.View())
	}
}

// TestSetup_DecliningAnIdentityGoesBackToTheAddress checks the answer that is
// the whole point of showing the identity first.
func TestSetup_DecliningAnIdentityGoesBackToTheAddress(t *testing.T) {
	m := New(Options{Client: fakeClient(setupRunner())})
	m, _ = drive(t, m, fetchStatus(m.client)())
	m.setup.tested = PeerTested{Address: "127.0.0.1:22002", DeviceID: "SOMEOTHERDEVICE"}
	m.setup.focus(stepPeerConfirm)

	m = press(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'n'}})
	if m.setup.step != stepPeerHost {
		t.Fatalf("declining left the walk on %v", m.setup.step)
	}
}

// TestSetup_FolderIDFromPath checks the proposal is always an id the command
// line would accept.
func TestSetup_FolderIDFromPath(t *testing.T) {
	for path, want := range map[string]string{
		"/home/u/docs":      "docs",
		"/home/u/My Docs/":  "my-docs",
		"/home/u/Photos_23": "photos_23",
		"/":                 "folder",
		"/home/u/...":       "folder",
	} {
		if got := folderIDFromPath(path); got != want {
			t.Errorf("folderIDFromPath(%q) = %q, want %q", path, got, want)
		}
	}
}
