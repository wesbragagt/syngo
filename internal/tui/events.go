package tui

import (
	"bufio"
	"encoding/json"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
)

// eventBufferSize is how many lines the log view keeps. The daemon's own log
// is the record; this is a window on the recent past, and a window that grew
// without bound would be a memory leak in a program meant to be left running
// in a tmux pane for days.
const eventBufferSize = 2000

// event is one line of the log: when it happened, how loud it is, and what it
// says. The searchable text is built once, at append time, because a filter
// that runs on every keystroke should not rebuild it per line per key.
type event struct {
	at    time.Time
	level string
	text  string
	// search is the lowercased form the filter matches against.
	search string
}

// eventsModel is the scrollable, filterable log.
//
// There are two sources and they are honestly different things. The first is
// the daemon's own structured log, which the daemon writes to stderr; a TUI in
// another process cannot read another process's stderr, so it tails a file the
// operator redirected it to (`syngo tui --log-file`). The second is what
// this process saw for itself: every change between one `status --json` and
// the next. The second always works and needs no setup; the first is the full
// record. Lines from each are marked, so neither is mistaken for the other.
//
// There is no event stream on the control socket to subscribe to. Adding one
// would be a change to the daemon's API, which is an application decision and
// not one a presentation layer gets to make; docs/TUI.md says so plainly.
type eventsModel struct {
	// logFile is the file the daemon's log is being written to, empty when
	// the TUI was not told of one.
	logFile string
	// offset is how far into logFile has been read already, so a tail
	// reads only what is new.
	offset int64
	// logErr is why the file could not be read, reported once rather than
	// on every tick.
	logErr string

	events []event
	// query is the search box; filtering happens as it is typed.
	query textinput.Model
	// searching is whether the search box has the keyboard.
	searching bool

	viewport viewport.Model
	// ready is whether the viewport has been given a size yet.
	ready bool
	// previous is the last report, which the deltas are measured against.
	previous Status
	// seen is whether previous holds a report at all; the first one
	// describes a device that was already running and is not news.
	seen bool
}

// newEvents builds the log view.
func newEvents(logFile string) eventsModel {
	query := textinput.New()
	query.Placeholder = "filter"
	query.Prompt = "/"
	query.CharLimit = 128
	query.Width = 40
	return eventsModel{logFile: logFile, query: query, viewport: viewport.New(80, 16)}
}

// resize gives the viewport the room the terminal has, less the header, the
// search line and the footer.
func (e *eventsModel) resize(width, height int) {
	e.viewport.Width = maxInt(width, 20)
	e.viewport.Height = maxInt(height-7, 3)
	e.ready = true
	e.render()
}

// enter is what the screen runs when it is opened: a read of whatever the log
// file has, so a TUI started long after the daemon still shows its history.
func (e eventsModel) enter() tea.Cmd { return nil }

// append records one line, dropping the oldest when the window is full.
func (e *eventsModel) append(at time.Time, level, text string) {
	e.events = append(e.events, event{
		at: at, level: level, text: text,
		search: strings.ToLower(level + " " + text),
	})
	if len(e.events) > eventBufferSize {
		e.events = e.events[len(e.events)-eventBufferSize:]
	}
}

// observe records everything that changed between two reports. It is the
// source that needs no setup: a folder that started scanning, a peer that
// connected or dropped, a transfer that moved, a listener that was rebound, a
// conflict that appeared, an error that arrived or cleared.
func (e *eventsModel) observe(now Status) {
	defer func() {
		e.previous, e.seen = now, true
		e.render()
	}()
	if !e.seen {
		return
	}
	at := time.Now()
	before := e.previous

	if before.Daemon.Running != now.Daemon.Running {
		e.state(at, "daemon", boolWord(now.Daemon.Running, "running", "stopped"))
	}
	if before.Daemon.Listener.Bound != now.Daemon.Listener.Bound {
		e.state(at, "listener", boolWord(now.Daemon.Listener.Bound,
			"bound on "+now.Daemon.Listener.Address, "lost its address, rebinding"))
	}
	if now.Daemon.Listener.Rebinds > before.Daemon.Listener.Rebinds {
		e.state(at, "listener", "came back on "+now.Daemon.Listener.Address)
	}

	e.folderDeltas(at, before, now)
	e.peerDeltas(at, before, now)
	e.transferDeltas(at, before, now)

	if now.Conflicts.Unresolved > before.Conflicts.Unresolved {
		e.state(at, "conflict", sprintf("%d unresolved, was %d",
			now.Conflicts.Unresolved, before.Conflicts.Unresolved))
	}
}

// folderDeltas records folder state changes and folder errors.
func (e *eventsModel) folderDeltas(at time.Time, before, now Status) {
	old := map[string]Folder{}
	for _, f := range before.Folders {
		old[f.ID] = f
	}
	for _, f := range now.Folders {
		was, known := old[f.ID]
		if !known {
			e.state(at, "folder "+f.ID, "added at "+f.Path)
			continue
		}
		if was.State != f.State {
			e.state(at, "folder "+f.ID, was.State+" → "+f.State)
		}
		if deref(was.Error, "") != deref(f.Error, "") {
			if f.Error == nil {
				e.state(at, "folder "+f.ID, "error cleared")
			} else {
				e.problem(at, "folder "+f.ID, *f.Error)
			}
		}
	}
}

// peerDeltas records connection state changes and peer errors.
func (e *eventsModel) peerDeltas(at time.Time, before, now Status) {
	old := map[string]Peer{}
	for _, p := range before.Peers {
		old[p.ID] = p
	}
	for _, p := range now.Peers {
		was, known := old[p.ID]
		name := "peer " + p.Name
		if !known {
			e.state(at, name, "added at "+p.Address)
			continue
		}
		if was.State != p.State {
			if p.State == "connected" {
				e.state(at, name, "connected")
			} else {
				e.problem(at, name, was.State+" → "+p.State)
			}
		}
		if was.Path.Kind != p.Path.Kind {
			e.state(at, name, "path "+was.Path.Kind+" → "+p.Path.Kind)
		}
		if deref(was.Error, "") != deref(p.Error, "") && p.Error != nil {
			e.problem(at, name, *p.Error)
		}
	}
}

// transferDeltas records transfers that started, moved on or failed.
func (e *eventsModel) transferDeltas(at time.Time, before, now Status) {
	old := map[string]Transfer{}
	for _, t := range before.Transfers.Items {
		old[t.ID] = t
	}
	seen := map[string]bool{}
	for _, t := range now.Transfers.Items {
		seen[t.ID] = true
		was, known := old[t.ID]
		what := "transfer " + t.FolderID + "/" + t.Path
		switch {
		case !known:
			e.state(at, what, sprintf("started, %s from %s",
				bytesHuman(t.Size), short(t.PeerID)))
		case was.State != t.State:
			if t.State == "failed" {
				e.problem(at, what, "failed: "+deref(t.Error, "no reason given"))
			} else {
				e.state(at, what, was.State+" → "+t.State)
			}
		case t.ChunksDone != was.ChunksDone:
			e.state(at, what, sprintf("%d of %d chunks, %s",
				t.ChunksDone, t.ChunksTotal, bytesHuman(t.BytesDone)))
		}
	}
	for _, t := range before.Transfers.Items {
		if !seen[t.ID] {
			// A transfer leaves the journal when its file is
			// installed, which is the only way it ends well.
			e.state(at, "transfer "+t.FolderID+"/"+t.Path, "left the journal, installed or superseded")
		}
	}
}

// state records an ordinary change this process saw.
func (e *eventsModel) state(at time.Time, what, detail string) {
	e.append(at, "seen", what+": "+detail)
}

// problem records a change that is a failure.
func (e *eventsModel) problem(at time.Time, what, detail string) {
	e.append(at, "error", what+": "+detail)
}

// boolWord picks between two words.
func boolWord(b bool, yes, no string) string {
	if b {
		return yes
	}
	return no
}

// logLinesMsg is what a read of the log file produced.
type logLinesMsg struct {
	lines  []event
	offset int64
	err    string
}

// tailLog reads whatever has been appended to the log file since the last
// read. It is a file read rather than a subscription because the daemon writes
// its log to stderr and a separate process cannot read that; what it can read
// is the file the operator sent stderr to.
func tailLog(path string, from int64) tea.Cmd {
	if path == "" {
		return nil
	}
	return func() tea.Msg {
		f, err := os.Open(path) //nolint:gosec // the path is the operator's own --log-file
		if err != nil {
			return logLinesMsg{offset: from, err: err.Error()}
		}
		defer func() { _ = f.Close() }()

		info, err := f.Stat()
		if err != nil {
			return logLinesMsg{offset: from, err: err.Error()}
		}
		// A file that shrank was rotated or replaced; start again from
		// the beginning rather than reading from a stale offset.
		start := from
		if info.Size() < start {
			start = 0
		}
		if _, err := f.Seek(start, 0); err != nil {
			return logLinesMsg{offset: 0, err: err.Error()}
		}

		var (
			lines []event
			read  int64
		)
		scanner := bufio.NewScanner(f)
		scanner.Buffer(make([]byte, 0, 64<<10), 1<<20)
		for scanner.Scan() {
			raw := scanner.Text()
			read += int64(len(scanner.Bytes())) + 1
			if strings.TrimSpace(raw) == "" {
				continue
			}
			lines = append(lines, parseLogLine(raw))
		}
		if err := scanner.Err(); err != nil {
			return logLinesMsg{lines: lines, offset: start + read, err: err.Error()}
		}
		return logLinesMsg{lines: lines, offset: start + read}
	}
}

// parseLogLine reads one line of the daemon's log. With `--log-format json`
// every line is an object with at least time, level and msg plus the fields
// the event carries; with the default text format it is left as it stands,
// because reinventing a parser for it here would be a second definition of a
// format the daemon owns.
func parseLogLine(raw string) event {
	var doc map[string]any
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		return event{at: time.Time{}, level: "log", text: raw, search: strings.ToLower(raw)}
	}

	at := time.Time{}
	if s, ok := doc["time"].(string); ok {
		if parsed, err := time.Parse(time.RFC3339Nano, s); err == nil {
			at = parsed
		}
	}
	level := "log"
	if s, ok := doc["level"].(string); ok && s != "" {
		level = strings.ToLower(s)
	}
	msg, _ := doc["msg"].(string)

	// The remaining fields in a stable order, so two lines of the same kind
	// read the same way and a search over them is predictable.
	keys := make([]string, 0, len(doc))
	for key := range doc {
		switch key {
		case "time", "level", "msg":
		default:
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)

	var b strings.Builder
	b.WriteString(msg)
	for _, key := range keys {
		b.WriteString(sprintf(" %s=%v", key, doc[key]))
	}
	text := b.String()
	return event{at: at, level: level, text: text, search: strings.ToLower(level + " " + text)}
}

// update handles one message for the log view.
func (e eventsModel) update(msg tea.Msg) (eventsModel, tea.Cmd) {
	switch msg := msg.(type) {
	case logLinesMsg:
		e.offset, e.logErr = msg.offset, msg.err
		e.events = append(e.events, msg.lines...)
		if len(e.events) > eventBufferSize {
			e.events = e.events[len(e.events)-eventBufferSize:]
		}
		e.render()
		return e, nil

	case tea.KeyMsg:
		return e.key(msg)
	}
	return e, nil
}

// key handles one key press. back is true when the key means "leave".
func (e eventsModel) key(msg tea.KeyMsg) (eventsModel, tea.Cmd) {
	if e.searching {
		switch msg.String() {
		case "enter", "esc":
			// The filter stays applied; only the keyboard leaves.
			e.searching = false
			e.query.Blur()
			return e, nil
		}
		var cmd tea.Cmd
		e.query, cmd = e.query.Update(msg)
		e.render()
		return e, cmd
	}

	switch msg.String() {
	case "/":
		e.searching = true
		e.query.Focus()
		return e, textinput.Blink
	case "x":
		e.query.SetValue("")
		e.render()
		return e, nil
	}

	var cmd tea.Cmd
	e.viewport, cmd = e.viewport.Update(msg)
	return e, cmd
}

// wantsBack reports whether a key means "leave this screen". Escape only means
// that when the search box does not have the keyboard, where it means "stop
// typing".
func (e eventsModel) wantsBack(msg tea.KeyMsg) bool {
	return !e.searching && msg.String() == "esc"
}

// filtered is the lines the current query matches. A query is a plain
// case-insensitive substring over the level and the text: a regular expression
// would be a second language to learn for a box that is typed into one key at
// a time.
func (e eventsModel) filtered() []event {
	query := strings.ToLower(strings.TrimSpace(e.query.Value()))
	if query == "" {
		return e.events
	}
	out := make([]event, 0, len(e.events))
	for _, item := range e.events {
		if strings.Contains(item.search, query) {
			out = append(out, item)
		}
	}
	return out
}

// render puts the matching lines in the viewport, newest at the bottom, and
// scrolls to them: a log that did not follow its own tail would have to be
// scrolled by hand on every new line.
func (e *eventsModel) render() {
	matches := e.filtered()
	lines := make([]string, 0, len(matches))
	for _, item := range matches {
		lines = append(lines, e.line(item))
	}
	e.viewport.SetContent(strings.Join(lines, "\n"))
	e.viewport.GotoBottom()
}

// line renders one event.
func (e eventsModel) line(item event) string {
	stamp := "        "
	if !item.at.IsZero() {
		stamp = item.at.Format("15:04:05")
	}
	level := pad(item.level, 5)
	switch item.level {
	case "error":
		level = styleErr.Render(level)
	case "warn":
		level = styleWarn.Render(level)
	case "seen":
		level = styleDim.Render(level)
	default:
		level = styleDim.Render(level)
	}
	return styleDim.Render(stamp) + " " + level + " " + item.text
}

// view renders the log.
func (e eventsModel) view(width int) string {
	var b strings.Builder
	b.WriteString(styleTitle.Render("logs") + "  ")
	if e.logFile != "" {
		b.WriteString(styleDim.Render("tailing " + truncate(e.logFile, maxInt(width-30, 20))))
	} else {
		b.WriteString(styleDim.Render("changes seen by this process; " +
			"start the daemon with --log-format json 2>FILE and run the TUI with --log-file FILE for its own log"))
	}
	b.WriteString("\n")
	if e.logErr != "" {
		b.WriteString(styleErr.Render("log file: "+e.logErr) + "\n")
	}

	matches := len(e.filtered())
	search := e.query.View()
	if !e.searching && strings.TrimSpace(e.query.Value()) == "" {
		search = styleDim.Render("/ to search")
	}
	b.WriteString(search + "  " +
		styleDim.Render(sprintf("%d of %d lines", matches, len(e.events))) + "\n\n")
	b.WriteString(e.viewport.View() + "\n")
	return b.String()
}

// help lists the keys of the log view.
func (e eventsModel) help() string {
	if e.searching {
		return "type to filter · enter keep the filter · ctrl+c quit"
	}
	return "/ search · x clear · ↑↓ pgup pgdn scroll · esc dashboard · q quit"
}
