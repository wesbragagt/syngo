package tui

import (
	"context"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// DefaultPathPoll is how often `network path --json` is asked. It is much
// slower than the status poll because the answer can only come from the local
// Tailscale client, and a running daemon already refreshes the same thing in
// `status.peers[].path` every thirty seconds. The separate call exists so a
// device whose daemon has not refreshed yet, or which is showing a stale
// observation, still gets the current answer.
const DefaultPathPoll = 30 * time.Second

// pathsMsg is the answer to `network path --json`.
type pathsMsg struct {
	paths []NetPath
	err   error
}

// pathTickMsg asks for the next path refresh.
type pathTickMsg struct{}

// fetchPaths asks how traffic reaches every configured peer.
func fetchPaths(c Client) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		paths, err := c.NetworkPath(ctx)
		return pathsMsg{paths: paths, err: err}
	}
}

// pathTick schedules the next path refresh.
func pathTick(d time.Duration) tea.Cmd {
	return tea.Tick(d, func(time.Time) tea.Msg { return pathTickMsg{} })
}

// pathFor returns what `network path` last said about a peer, or false when it
// said nothing about it. The dashboard falls back to `status.peers[].path`,
// which is the same shape read by the daemon on its own timer.
func (m Model) pathFor(peerID string) (NetPath, bool) {
	for _, p := range m.paths {
		if p.PeerID == peerID {
			return p, true
		}
	}
	return NetPath{}, false
}

// dashboardView is the live view: the device and its daemon, the peers with
// the path to each of them, the folders, the transfer queue, and everything
// that is currently wrong.
func (m Model) dashboardView() string {
	var b strings.Builder
	b.WriteString(styleTitle.Render("syncthis") + "  " +
		m.status.Device.Name + " " + styleDim.Render("("+short(m.status.Device.ID)+")") + "\n")
	b.WriteString(styleDim.Render(m.status.Device.DataDir) + "\n\n")
	b.WriteString(m.daemonLine() + "\n\n")
	b.WriteString(m.peersView())
	b.WriteString("\n")
	b.WriteString(m.foldersView())
	b.WriteString("\n")
	b.WriteString(m.queueView())
	b.WriteString("\n")
	b.WriteString(m.conflictsLine())
	b.WriteString("\n")
	b.WriteString(m.errorsView())
	return b.String()
}

// daemonLine says how the daemon and its peer port are doing, from
// status.daemon and status.daemon.listener.
func (m Model) daemonLine() string {
	d := m.status.Daemon
	if !d.Running {
		return styleErr.Render("daemon not running")
	}
	line := styleOK.Render("daemon running") +
		styleDim.Render(sprintf(" pid %d, listening on %s", d.PID, d.Listen))
	l := d.Listener
	if !l.Bound {
		line += "\n" + styleErr.Render("listener not bound, rebinding") +
			styleDim.Render(" last address "+l.Address)
		return line
	}
	iface := l.Interface
	if iface == "" {
		iface = "no interface configured"
	}
	line += "\n" + styleDim.Render(sprintf("listener bound on %s (%s)", l.Address, iface))
	if l.Rebinds > 0 || l.Failures > 0 {
		line += styleWarn.Render(sprintf(", %d rebinds, %d failed attempts", l.Rebinds, l.Failures))
	}
	return line
}

// peersView is the device table: who this device is paired with, whether it is
// connected, and whether traffic reaches it peer-to-peer or through a relay.
func (m Model) peersView() string {
	if len(m.status.Peers) == 0 {
		return styleHeader.Render("DEVICES") + "\n" + styleDim.Render("no peers configured") + "\n"
	}
	rows := make([][]string, 0, len(m.status.Peers))
	for _, p := range m.status.Peers {
		kind, detail := m.pathOf(p)
		latency := "-"
		if p.LatencyMS > 0 {
			latency = sprintf("%.1f ms", p.LatencyMS)
		}
		rows = append(rows, []string{
			p.Name, short(p.ID), p.Address,
			stateStyle(p.State).Render(pad(p.State, 12)),
			pathStyle(kind).Render(pad(kind, 7)),
			detail, latency,
		})
	}
	return styleHeader.Render("DEVICES") + "\n" +
		columns([]string{"NAME", "ID", "ADDRESS", "STATE", "PATH", "DETAIL", "LATENCY"}, rows)
}

// pathOf answers how traffic reaches a peer, preferring the direct
// `network path` reading over the one the daemon last folded into `status`.
// Nothing is inferred: a path neither source can describe is unknown.
func (m Model) pathOf(p Peer) (kind, detail string) {
	if fresh, ok := m.pathFor(p.ID); ok {
		return fresh.Kind, pathDetail(fresh.Kind, fresh.Endpoint, fresh.Relay, fresh.Reason)
	}
	return p.Path.Kind, pathDetail(p.Path.Kind, p.Path.Endpoint, p.Path.Relay, p.Path.Reason)
}

// pathDetail is the one cell that says which endpoint or which relay, or why
// there is no answer.
func pathDetail(kind, endpoint, relay string, reason *string) string {
	switch kind {
	case "direct":
		return endpoint
	case "relay":
		return "DERP " + relay
	default:
		// The reason a client gives can be a sentence; the column is
		// one cell wide, so it is cut here and the full text stays in
		// `network path`.
		return styleDim.Render(truncate(deref(reason, "no reading yet"), 28))
	}
}

// foldersView is the folder table, with the selected row marked. `state` is
// the daemon's own word for what the folder is doing.
func (m Model) foldersView() string {
	if len(m.status.Folders) == 0 {
		return styleHeader.Render("FOLDERS") + "\n" + styleDim.Render("no folders configured") + "\n"
	}
	rows := make([][]string, 0, len(m.status.Folders))
	for i, f := range m.status.Folders {
		marker := "  "
		id := f.ID
		if i == m.folder {
			marker, id = styleSel.Render("> "), styleSel.Render(f.ID)
		}
		pending := "-"
		if f.PendingChanges > 0 {
			pending = styleWarn.Render(sprintf("%d", f.PendingChanges))
		}
		rows = append(rows, []string{
			marker + pad(id, 16), f.Path, f.Mode,
			stateStyle(f.State).Render(pad(f.State, 9)),
			sprintf("%d", f.Files), bytesHuman(f.Bytes), pending,
		})
	}
	return styleHeader.Render("FOLDERS") + "\n" +
		columns([]string{"  FOLDER", "PATH", "MODE", "STATE", "FILES", "SIZE", "PENDING"}, rows)
}

// queueView is the transfer journal: the counts the daemon reports and a
// progress line per transfer that is still going.
func (m Model) queueView() string {
	t := m.status.Transfers
	head := styleHeader.Render("QUEUE") + "  " + styleDim.Render(sprintf(
		"%d active, %d pending, %d paused, %d failed", t.Active, t.Pending, t.Paused, t.Failed)) + "\n"
	if len(t.Items) == 0 {
		return head + styleDim.Render("nothing in flight") + "\n"
	}

	rows := make([][]string, 0, len(t.Items))
	for _, item := range t.Items {
		rows = append(rows, []string{
			item.FolderID, truncate(item.Path, 40),
			stateStyle(item.State).Render(pad(item.State, 9)),
			progressBar(item.ChunksDone, item.ChunksTotal, 16),
			sprintf("%s / %s", bytesHuman(item.BytesDone), bytesHuman(item.Size)),
			short(item.PeerID),
		})
	}
	return head + columns([]string{"FOLDER", "PATH", "STATE", "PROGRESS", "BYTES", "PEER"}, rows)
}

// progressBar renders done out of total in width columns. A transfer whose
// total is not known yet gets an empty bar rather than a full one.
func progressBar(done, total, width int) string {
	if width < 3 {
		width = 3
	}
	inner := width - 2
	filled := 0
	if total > 0 {
		filled = done * inner / total
	}
	if filled > inner {
		filled = inner
	}
	return "[" + strings.Repeat("=", filled) + strings.Repeat(" ", inner-filled) + "]"
}

// conflictsLine counts the paths both sides changed and nobody has decided
// about, and points at the screen that settles them.
func (m Model) conflictsLine() string {
	n := m.status.Conflicts.Unresolved
	head := styleHeader.Render("CONFLICTS") + "\n"
	if n == 0 {
		return head + styleOK.Render("none") + "\n"
	}
	return head + styleWarn.Render(sprintf("%d unresolved", n)) +
		styleDim.Render("  press c to review them") + "\n"
}

// errorsView is the one place a failure shows up, whichever part of the device
// it came from: the listener, a folder's last scan or sync, a peer that cannot
// be dialled, and the last command the TUI itself ran.
func (m Model) errorsView() string {
	var lines []string
	if l := m.status.Daemon.Listener; l.Error != nil && *l.Error != "" {
		lines = append(lines, "listener: "+*l.Error)
	}
	for _, f := range m.status.Folders {
		if f.Error != nil && *f.Error != "" {
			lines = append(lines, "folder "+f.ID+": "+*f.Error)
		}
	}
	for _, p := range m.status.Peers {
		if p.Error != nil && *p.Error != "" {
			lines = append(lines, "peer "+p.Name+": "+*p.Error)
		}
	}
	if m.pathErr != nil && ExitCode(m.pathErr) != ExitTailnetGone {
		// Exit 11 is a device with no Tailscale client, which is a
		// supported way to run this program and not a failure. Every
		// other reason the paths could not be read is one.
		lines = append(lines, "network path: "+m.pathErr.Error())
	}

	head := styleHeader.Render("ERRORS") + "\n"
	if len(lines) == 0 {
		return head + styleOK.Render("none") + "\n"
	}
	var b strings.Builder
	b.WriteString(head)
	for _, line := range lines {
		b.WriteString(styleErr.Render(truncate(line, m.width-1)) + "\n")
	}
	return b.String()
}

// dashboardKey handles the keys of the folder table: moving the selection, and
// the three folder commands the command line offers.
func (m Model) dashboardKey(msg tea.KeyMsg) (Model, tea.Cmd, bool) {
	switch msg.String() {
	case "up", "k":
		if m.folder > 0 {
			m.folder--
		}
		return m, nil, true
	case "down", "j":
		if m.folder < len(m.status.Folders)-1 {
			m.folder++
		}
		return m, nil, true
	case "p":
		return m, m.folderCommand("pause"), true
	case "u":
		return m, m.folderCommand("resume"), true
	case "s":
		return m, m.folderCommand("rescan"), true
	}
	return m, nil, false
}

// selectedFolder is the folder the table's cursor is on, or empty when there
// are none.
func (m Model) selectedFolder() string {
	if m.folder < 0 || m.folder >= len(m.status.Folders) {
		return ""
	}
	return m.status.Folders[m.folder].ID
}

// folderCommand runs pause, resume or rescan against the selected folder and
// reports the result in the footer. Each is exactly the command of the same
// name in docs/CLI.md.
func (m Model) folderCommand(name string) tea.Cmd {
	id := m.selectedFolder()
	if id == "" {
		return nil
	}
	c := m.client
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		var err error
		switch name {
		case "pause":
			err = c.Pause(ctx, id)
		case "resume":
			err = c.Resume(ctx, id)
		case "rescan":
			err = c.Rescan(ctx, id)
		}
		return noticeMsg{text: sprintf("%s %s: done", name, id), err: err}
	}
}
