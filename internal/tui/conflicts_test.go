package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// twoConflicts is a listing with one ordinary conflict and one whose files
// have both moved on, in the shape `conflict list --json` documents.
func twoConflicts() []Conflict {
	detected := "2026-09-21T15:30:00Z"
	return []Conflict{
		{
			ID: "6f1c2f3a4b5c6d7e8f9a0b1c2d3e4f5a", FolderID: "docs",
			Path:         "notes/report.md",
			ConflictPath: "notes/report.sync-conflict-20260921-153000-ETI3RRZ.md",
			Reason:       "both-modified", LocalHash: "3f0a", RemoteHash: "9b12",
			PeerID:     "ETI3RRZCMWYFD54X22RLMR5SYEWJTA3UANTRKYNN3KAAX5ZGPAFA",
			DetectedAt: &detected,
		},
		{
			ID: "aa11bb22cc33dd44ee55ff66aa77bb88", FolderID: "docs",
			Path: "notes/stale.md", Reason: "delete-vs-modify",
			LocalHash: "1111", PeerID: "ETI3RRZCMWYFD54X22RLMR5SYEWJTA3UANTRKYNN3KAAX5ZGPAFA",
			DetectedAt: &detected, Stale: true,
		},
	}
}

// shownConflict is what `conflict show` adds to the first of them: both files
// as they are on disk now.
func shownConflict() Conflict {
	item := twoConflicts()[0]
	local := "2026-09-21T15:29:41Z"
	remote := "2026-09-21T15:29:58Z"
	item.File = &ConflictFile{
		Path: "notes/report.md", Size: 1902, MTime: &remote, Hash: "9b12", Version: "remote",
	}
	item.Copy = &ConflictFile{
		Path: "notes/report.sync-conflict-20260921-153000-ETI3RRZ.md",
		Size: 1841, MTime: &local, Hash: "3f0a", Version: "local",
	}
	return item
}

// conflictRunner answers the three conflict commands.
func conflictRunner() *fakeRunner {
	return newFakeRunner().
		ok("status", func() Status { s := runningStatus(); s.Conflicts.Unresolved = 2; return s }()).
		ok("conflict list", twoConflicts()).
		ok("conflict show 6f1c2f3a4b5c6d7e8f9a0b1c2d3e4f5a", shownConflict()).
		ok("conflict resolve 6f1c2f3a4b5c6d7e8f9a0b1c2d3e4f5a --keep local", ConflictResolved{
			ID: "6f1c2f3a4b5c6d7e8f9a0b1c2d3e4f5a", FolderID: "docs",
			Path: "notes/report.md", Kept: "local", KeptPath: "notes/report.md",
			RemovedPath: "notes/report.sync-conflict-20260921-153000-ETI3RRZ.md",
			Resolved:    true,
		}).
		ok("conflict resolve 6f1c2f3a4b5c6d7e8f9a0b1c2d3e4f5a --keep both", ConflictResolved{
			ID: "6f1c2f3a4b5c6d7e8f9a0b1c2d3e4f5a", Kept: "both", Resolved: true,
		})
}

// openConflicts puts the model on the conflict screen with the listing loaded.
func openConflicts(t *testing.T, runner *fakeRunner) Model {
	t.Helper()
	m := dashboard(t, runner, func() Status {
		s := runningStatus()
		s.Conflicts.Unresolved = 2
		return s
	}())
	m = press(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'c'}})
	if m.screen != screenConflicts {
		t.Fatalf("screen is %v, want the conflict screen", m.screen)
	}
	return m
}

// TestConflicts_ListsThemAndOpensOne walks the review path: c from the
// dashboard, the listing, then one conflict with both versions.
func TestConflicts_ListsThemAndOpensOne(t *testing.T) {
	runner := conflictRunner()
	m := openConflicts(t, runner)

	if !runner.called("conflict list") {
		t.Fatalf("the screen did not run `conflict list`; calls were %v", runner.calls)
	}
	list := m.View()
	for _, want := range []string{"notes/report.md", "both-modified", "notes/stale.md", "stale"} {
		if !strings.Contains(list, want) {
			t.Errorf("the listing does not show %q:\n%s", want, list)
		}
	}

	m = press(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.conflicts.panel != conflictDetail {
		t.Fatalf("enter did not open the conflict; err %v", m.conflicts.err)
	}
	detail := m.View()
	for _, want := range []string{
		"notes/report.md",
		"notes/report.sync-conflict-20260921-153000-ETI3RRZ.md",
		// Both versions, found by hash wherever they sit: the local one
		// is in the conflict copy and the remote one at the path.
		"local : notes/report.sync-conflict-20260921-153000-ETI3RRZ.md",
		"remote: notes/report.md",
		// The three resolutions, with the command behind each.
		"--keep local", "--keep remote", "--keep both",
	} {
		if !strings.Contains(detail, want) {
			t.Errorf("the detail does not show %q:\n%s", want, detail)
		}
	}
}

// TestConflicts_RestoringAVersionConfirmsFirst checks that a resolution which
// removes a file asks, names the command, and then runs exactly it.
func TestConflicts_RestoringAVersionConfirmsFirst(t *testing.T) {
	runner := conflictRunner()
	m := openConflicts(t, runner)
	m = press(t, m, tea.KeyMsg{Type: tea.KeyEnter})

	m = press(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'1'}})
	if m.conflicts.panel != conflictConfirm {
		t.Fatalf("keeping a version did not ask first")
	}
	confirm := m.View()
	if !strings.Contains(confirm, "this removes a file") {
		t.Errorf("the confirmation does not say what it does:\n%s", confirm)
	}
	if !strings.Contains(confirm, "--keep local") {
		t.Errorf("the confirmation does not name the command:\n%s", confirm)
	}
	if runner.called("conflict resolve 6f1c2f3a4b5c6d7e8f9a0b1c2d3e4f5a --keep local") {
		t.Fatal("the resolution ran before it was confirmed")
	}

	m = press(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	if !runner.called("conflict resolve 6f1c2f3a4b5c6d7e8f9a0b1c2d3e4f5a --keep local") {
		t.Fatalf("the resolution did not run; calls were %v", runner.calls)
	}
	if m.conflicts.panel != conflictList {
		t.Errorf("after resolving the screen is %v, want the listing", m.conflicts.panel)
	}
}

// TestConflicts_DecliningRunsNothing is the other answer to the same question.
func TestConflicts_DecliningRunsNothing(t *testing.T) {
	runner := conflictRunner()
	m := openConflicts(t, runner)
	m = press(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	m = press(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'2'}})
	m = press(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'n'}})

	if m.conflicts.panel != conflictDetail {
		t.Errorf("declining left the screen on %v", m.conflicts.panel)
	}
	for _, call := range runner.calls {
		if strings.HasPrefix(call, "conflict resolve") {
			t.Errorf("declining still ran %q", call)
		}
	}
}

// TestConflicts_KeepBothAsksNothing follows the command line's own rule:
// --keep both deletes nothing, so it is not a destructive command.
func TestConflicts_KeepBothAsksNothing(t *testing.T) {
	runner := conflictRunner()
	m := openConflicts(t, runner)
	m = press(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	m = press(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'3'}})
	if m.conflicts.panel != conflictList {
		t.Errorf("after keeping both the screen is %v, want the listing", m.conflicts.panel)
	}
	if !runner.called("conflict resolve 6f1c2f3a4b5c6d7e8f9a0b1c2d3e4f5a --keep both") {
		t.Fatalf("keep-both did not run; calls were %v", runner.calls)
	}
}

// TestConflicts_AStaleOneSaysWhatIsLeft checks the screen repeats what the
// command line does about a conflict neither version can be kept for.
func TestConflicts_AStaleOneSaysWhatIsLeft(t *testing.T) {
	stale := twoConflicts()[1]
	runner := conflictRunner().ok("conflict show aa11bb22cc33dd44ee55ff66aa77bb88", stale)
	m := openConflicts(t, runner)

	m = press(t, m, tea.KeyMsg{Type: tea.KeyDown})
	m = press(t, m, tea.KeyMsg{Type: tea.KeyEnter})

	detail := m.View()
	if !strings.Contains(detail, "exit 8") {
		t.Errorf("a stale conflict does not say keeping one version is refused:\n%s", detail)
	}
	if !strings.Contains(detail, "the peer deleted this path") {
		t.Errorf("a delete-vs-modify conflict does not say the peer deleted it:\n%s", detail)
	}
}

// TestConflicts_AFailedResolutionIsShown checks exit 8 reaches the user rather
// than being swallowed.
func TestConflicts_AFailedResolutionIsShown(t *testing.T) {
	runner := conflictRunner().
		failsWith("conflict resolve 6f1c2f3a4b5c6d7e8f9a0b1c2d3e4f5a --keep local", ExitIntegrity,
			"notes/report.md has changed since the conflict was recorded")
	m := openConflicts(t, runner)
	m = press(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	m = press(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'1'}})
	m = press(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})

	if !strings.Contains(m.View(), "has changed since the conflict was recorded") {
		t.Errorf("a refused resolution is not reported:\n%s", m.View())
	}
}

// TestConflicts_EscGoesBackToTheDashboard checks the way out.
func TestConflicts_EscGoesBackToTheDashboard(t *testing.T) {
	m := openConflicts(t, conflictRunner())
	m = press(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	m = press(t, m, tea.KeyMsg{Type: tea.KeyEsc})
	if m.conflicts.panel != conflictList {
		t.Fatalf("esc in the detail did not go back to the listing")
	}
	m, _ = drive(t, m, tea.KeyMsg{Type: tea.KeyEsc})
	if m.screen != screenDashboard {
		t.Errorf("esc in the listing did not go back to the dashboard")
	}
}

// TestDashboard_CountsConflicts checks the dashboard points at the screen.
func TestDashboard_CountsConflicts(t *testing.T) {
	s := runningStatus()
	s.Conflicts.Unresolved = 2
	m := dashboard(t, newFakeRunner(), s)
	if !strings.Contains(m.View(), "2 unresolved") {
		t.Errorf("the dashboard does not count conflicts:\n%s", m.View())
	}
}
