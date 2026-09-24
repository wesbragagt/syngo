package tui

import (
	"context"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// conflictView is which of the conflict panels is showing.
type conflictView int

const (
	// conflictList is the list of paths both sides changed.
	conflictList conflictView = iota
	// conflictDetail is one conflict with both versions as they are on
	// disk now, and the three resolutions.
	conflictDetail
	// conflictConfirm asks before a resolution that removes a file.
	conflictConfirm
)

// conflictsModel reviews the paths both sides changed and restores one of the
// two versions. It offers exactly the three resolutions
// `conflict resolve --keep` accepts and invents no fourth: what happens to
// each file is decided by the application, not here.
type conflictsModel struct {
	client Client

	panel conflictView
	items []Conflict
	// cursor is the row of items the selection is on.
	cursor int
	// shown is the answer to `conflict show` for the selected conflict,
	// which describes both files as they are on disk now.
	shown Conflict
	// pending is the resolution conflictConfirm is asking about.
	pending string
	// working is a command in flight.
	working bool
	// err is the last failure, cleared by the next attempt.
	err error
	// loaded says a listing has been fetched at least once, so an empty
	// list reads as "none" rather than "not asked yet".
	loaded bool
}

// newConflicts builds the screen. It asks nothing until enter is called.
func newConflicts(c Client) conflictsModel {
	return conflictsModel{client: c}
}

// Conflict screen messages.
type (
	conflictListMsg struct {
		items []Conflict
		err   error
	}
	conflictShowMsg struct {
		item Conflict
		err  error
	}
	conflictResolvedMsg struct {
		result ConflictResolved
		err    error
	}
)

// conflictCtx bounds one conflict command. A listing hashes both files of
// every conflict to answer `stale`, so it is not instant on a large folder.
func conflictCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 60*time.Second)
}

// enter is what the screen runs when it is opened: the listing.
func (c conflictsModel) enter() tea.Cmd { return c.list() }

// list runs `syngo conflict list --json`.
func (c conflictsModel) list() tea.Cmd {
	client := c.client
	return func() tea.Msg {
		ctx, cancel := conflictCtx()
		defer cancel()
		items, err := client.ConflictList(ctx)
		return conflictListMsg{items: items, err: err}
	}
}

// show runs `syngo conflict show ID`, which describes both files as they
// are on disk now rather than as they were when the conflict was recorded.
func (c conflictsModel) show(id string) tea.Cmd {
	client := c.client
	return func() tea.Msg {
		ctx, cancel := conflictCtx()
		defer cancel()
		item, err := client.ConflictShow(ctx, id)
		return conflictShowMsg{item: item, err: err}
	}
}

// resolve runs `syngo conflict resolve ID --keep local|remote|both`.
func (c conflictsModel) resolve(id, keep string) tea.Cmd {
	client := c.client
	return func() tea.Msg {
		ctx, cancel := conflictCtx()
		defer cancel()
		result, err := client.ConflictResolve(ctx, id, keep)
		return conflictResolvedMsg{result: result, err: err}
	}
}

// selected is the conflict the cursor is on.
func (c conflictsModel) selected() (Conflict, bool) {
	if c.cursor < 0 || c.cursor >= len(c.items) {
		return Conflict{}, false
	}
	return c.items[c.cursor], true
}

// update handles one message for the screen.
func (c conflictsModel) update(msg tea.Msg) (conflictsModel, tea.Cmd) {
	switch msg := msg.(type) {
	case conflictListMsg:
		c.working, c.loaded = false, true
		if c.err = msg.err; msg.err != nil {
			return c, nil
		}
		c.items = msg.items
		if c.cursor >= len(c.items) {
			c.cursor = max(len(c.items)-1, 0)
		}
		return c, nil

	case conflictShowMsg:
		c.working = false
		if c.err = msg.err; msg.err != nil {
			c.panel = conflictList
			return c, nil
		}
		c.shown = msg.item
		c.panel = conflictDetail
		return c, nil

	case conflictResolvedMsg:
		c.working = false
		c.panel = conflictList
		if c.err = msg.err; msg.err != nil {
			return c, nil
		}
		// The listing is what says which conflicts are left, so it is
		// asked again rather than edited here.
		return c, c.list()
	}
	return c, nil
}

// key handles one key press. back is true when the key means "leave this
// screen", which the root model acts on.
func (c conflictsModel) key(msg tea.KeyMsg) (out conflictsModel, cmd tea.Cmd, back bool) {
	if c.working {
		return c, nil, false
	}
	switch c.panel {
	case conflictConfirm:
		return c.confirmKey(msg)
	case conflictDetail:
		return c.detailKey(msg)
	default:
		return c.listKey(msg)
	}
}

// listKey handles the listing.
func (c conflictsModel) listKey(msg tea.KeyMsg) (conflictsModel, tea.Cmd, bool) {
	switch msg.String() {
	case "esc":
		return c, nil, true
	case "up", "k":
		if c.cursor > 0 {
			c.cursor--
		}
	case "down", "j":
		if c.cursor < len(c.items)-1 {
			c.cursor++
		}
	case "enter":
		item, ok := c.selected()
		if !ok {
			return c, nil, false
		}
		c.err = nil
		c.working = true
		return c, c.show(item.ID), false
	case "R":
		c.working = true
		return c, c.list(), false
	}
	return c, nil, false
}

// detailKey handles one conflict. The three keys are the three words
// `--keep` accepts.
func (c conflictsModel) detailKey(msg tea.KeyMsg) (conflictsModel, tea.Cmd, bool) {
	switch msg.String() {
	case "esc":
		c.panel = conflictList
	case "1":
		return c.ask(KeepLocal)
	case "2":
		return c.ask(KeepRemote)
	case "3":
		return c.ask(KeepBoth)
	}
	return c, nil, false
}

// ask moves to the confirmation, except for keep-both, which deletes nothing
// and is therefore not a destructive command. That is the same rule
// `conflict resolve` applies: --keep both asks nothing.
func (c conflictsModel) ask(keep string) (conflictsModel, tea.Cmd, bool) {
	c.err = nil
	c.pending = keep
	if keep == KeepBoth {
		c.working = true
		return c, c.resolve(c.shown.ID, keep), false
	}
	c.panel = conflictConfirm
	return c, nil, false
}

// confirmKey answers the confirmation.
func (c conflictsModel) confirmKey(msg tea.KeyMsg) (conflictsModel, tea.Cmd, bool) {
	switch msg.String() {
	case "y", "enter":
		c.working = true
		c.panel = conflictDetail
		return c, c.resolve(c.shown.ID, c.pending), false
	case "n", "esc":
		c.panel = conflictDetail
	}
	return c, nil, false
}

// view renders the screen.
func (c conflictsModel) view(width int) string {
	switch c.panel {
	case conflictDetail:
		return c.detailView(width)
	case conflictConfirm:
		return c.confirmView(width)
	default:
		return c.listView(width)
	}
}

// listView is the list of unresolved conflicts, which is
// `syngo conflict list`.
func (c conflictsModel) listView(width int) string {
	var b strings.Builder
	b.WriteString(styleTitle.Render("conflicts") + "  " +
		styleDim.Render(c.client.Command("conflict", "list")) + "\n\n")

	switch {
	case c.err != nil:
		b.WriteString(styleErr.Render(c.err.Error()) + "\n")
		return b.String()
	case c.working || !c.loaded:
		b.WriteString(styleDim.Render("reading…") + "\n")
		return b.String()
	case len(c.items) == 0:
		b.WriteString(styleOK.Render("no unresolved conflicts") + "\n")
		return b.String()
	}

	rows := make([][]string, 0, len(c.items))
	for i, item := range c.items {
		marker := "  "
		if i == c.cursor {
			marker = styleSel.Render("> ")
		}
		stale := ""
		if item.Stale {
			stale = styleWarn.Render("stale")
		}
		rows = append(rows, []string{
			marker + short(item.ID), item.FolderID,
			truncate(item.Path, maxInt(width-52, 16)),
			item.Reason, short(item.PeerID), stale,
		})
	}
	b.WriteString(columns([]string{"  ID", "FOLDER", "PATH", "REASON", "PEER", ""}, rows))
	return b.String()
}

// detailView is one conflict and both versions as they are on disk now, which
// is `syngo conflict show ID`.
func (c conflictsModel) detailView(width int) string {
	item := c.shown
	var b strings.Builder
	b.WriteString(styleTitle.Render("conflict "+short(item.ID)) + "  " +
		styleDim.Render(c.client.Command("conflict", "show", item.ID)) + "\n\n")
	b.WriteString("folder:   " + item.FolderID + "\n")
	b.WriteString("path:     " + truncate(item.Path, width-12) + "\n")
	if item.ConflictPath != "" {
		b.WriteString("copy:     " + truncate(item.ConflictPath, width-12) + "\n")
	}
	b.WriteString("reason:   " + item.Reason + "\n")
	b.WriteString("peer:     " + item.PeerID + "\n")
	b.WriteString("detected: " + deref(item.DetectedAt, "unknown") + "\n")
	if item.Stale {
		b.WriteString("\n" + styleWarn.Render(
			"neither version is on disk unchanged any more. Only keeping both can\n"+
				"close this one; keeping one version would throw away content nobody\n"+
				"decided about, and the command refuses it with exit 8.") + "\n")
	}

	b.WriteString("\n" + styleHeader.Render("VERSIONS") + "\n")
	b.WriteString(fileLine("local ", item.LocalHash, item.File, item.Copy))
	b.WriteString(fileLine("remote", item.RemoteHash, item.File, item.Copy))

	b.WriteString("\n" + styleHeader.Render("RESTORE") + "\n")
	b.WriteString("  " + styleTitle.Render("1") + "  keep the version this device had     " +
		styleCmd.Render("conflict resolve "+short(item.ID)+" --keep local") + "\n")
	b.WriteString("  " + styleTitle.Render("2") + "  keep the version the peer had        " +
		styleCmd.Render("conflict resolve "+short(item.ID)+" --keep remote") + "\n")
	b.WriteString("  " + styleTitle.Render("3") + "  keep both, touching no file          " +
		styleCmd.Render("conflict resolve "+short(item.ID)+" --keep both") + "\n")

	if c.working {
		b.WriteString("\n" + styleDim.Render("running…") + "\n")
	}
	if c.err != nil {
		b.WriteString("\n" + styleErr.Render(c.err.Error()) + "\n")
	}
	return b.String()
}

// fileLine describes where one of the two versions is on disk now. Which file
// holds which version is answered by hashing them, which `conflict show`
// already did, so the file is found wherever it sits.
func fileLine(label, hash string, file, copyOf *ConflictFile) string {
	if hash == "" {
		return "  " + label + ": " + styleDim.Render("the peer deleted this path") + "\n"
	}
	for _, candidate := range []*ConflictFile{file, copyOf} {
		if candidate == nil || candidate.Hash != hash {
			continue
		}
		return "  " + label + ": " + candidate.Path + "  " +
			styleDim.Render(sprintf("%s, %s", bytesHuman(candidate.Size),
				deref(candidate.MTime, "no modification time"))) + "\n"
	}
	return "  " + label + ": " + styleWarn.Render("no file on disk holds this version any more") + "\n"
}

// confirmView asks before a resolution that removes a file.
func (c conflictsModel) confirmView(width int) string {
	var b strings.Builder
	kept := "the version this device had"
	if c.pending == KeepRemote {
		kept = "the version the peer had"
	}
	b.WriteString(styleWarn.Render("this removes a file") + "\n\n")
	b.WriteString("Keeping " + kept + " puts it at\n  " +
		truncate(c.shown.Path, width-4) + "\nand removes the other file.\n\n")
	b.WriteString(styleDim.Render("the same thing outside the TUI:") + "\n  " +
		styleCmd.Render(c.client.Command("conflict", "resolve", c.shown.ID, "--keep", c.pending)) + "\n\n")
	b.WriteString(styleTitle.Render("y") + " to do it, " + styleTitle.Render("n") + " to go back.\n")
	return b.String()
}

// help lists the keys of the current conflict screen.
func (c conflictsModel) help() string {
	switch c.panel {
	case conflictDetail:
		return "1 keep local · 2 keep remote · 3 keep both · esc back · q quit"
	case conflictConfirm:
		return "y resolve · n back · q quit"
	default:
		return "↑↓ move · enter open · R reread · esc dashboard · q quit"
	}
}

// maxInt returns the larger of two ints.
func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
