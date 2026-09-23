package tui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// DefaultPoll is how often the dashboard asks the daemon what changed. It is a
// poll rather than a subscription because that is what the command line
// offers: there is no event stream on the control socket, and inventing one
// would put application logic in the presentation layer.
const DefaultPoll = time.Second

// screen names the view that has the keyboard.
type screen int

const (
	// screenLoading is the first frame, before the first status answer.
	screenLoading screen = iota
	// screenFatal is a failure the TUI cannot act on, such as a data
	// directory it may not read.
	screenFatal
	// screenNoDaemon is a device that is initialized with no daemon
	// running. Nothing is live, so the screen says which command starts one.
	screenNoDaemon
	// screenSetup is the guided walk from an empty data directory to a
	// paired, synchronized folder.
	screenSetup
	// screenDashboard shows devices, folders, the queue, the paths and the
	// current error.
	screenDashboard
	// screenConflicts reviews the paths both sides changed and restores
	// one of the two versions.
	screenConflicts
	// screenEvents is the searchable log.
	screenEvents
)

// Options configure a TUI run.
type Options struct {
	// Client is how every screen reaches the daemon.
	Client Client
	// Poll is the refresh interval; zero means DefaultPoll.
	Poll time.Duration
	// PathPoll is how often `network path` is asked; zero means
	// DefaultPathPoll.
	PathPoll time.Duration
	// LogFile is a file the daemon's log is being written to, which the
	// log screen tails. Empty leaves that screen showing only the changes
	// this process saw for itself.
	LogFile string
}

// Model is the root Bubble Tea model. It owns no synchronization state: every
// field below is either what the last `status --json` said or which screen has
// the keyboard, so a TUI that is killed and started again is in the same place
// as one that never stopped.
type Model struct {
	client   Client
	poll     time.Duration
	pathPoll time.Duration

	screen screen
	// previous is the screen 'esc' goes back to.
	previous screen

	width, height int

	// status is the last answer, and statusAt when it arrived.
	status   Status
	statusAt time.Time
	// paths is the last `network path` answer, and pathErr why there is
	// none. A device with no Tailscale client answers exit 11, which is a
	// supported way to run and not a failure.
	paths   []NetPath
	pathErr error
	// folder is the row the folder table's cursor is on.
	folder int
	// fatal is the failure screenFatal shows.
	fatal error
	// notice is the last action's result, shown in the footer of every
	// screen and cleared by the next action.
	notice string
	// noticeErr marks the notice as a failure.
	noticeErr bool
	// quitting stops the poll from rescheduling itself.
	quitting bool

	// setup is the guided walk, which takes over when there is no identity.
	setup setupModel
	// conflicts is the review and restore screen.
	conflicts conflictsModel
	// events is the searchable log.
	events eventsModel
}

// New builds the root model. It does not talk to the daemon: Init does, so a
// test can drive the model with no I/O at all.
func New(opts Options) Model {
	poll := opts.Poll
	if poll <= 0 {
		poll = DefaultPoll
	}
	pathPoll := opts.PathPoll
	if pathPoll <= 0 {
		pathPoll = DefaultPathPoll
	}
	return Model{
		client:    opts.Client,
		poll:      poll,
		pathPoll:  pathPoll,
		screen:    screenLoading,
		previous:  screenDashboard,
		width:     80,
		height:    24,
		setup:     newSetup(opts.Client),
		conflicts: newConflicts(opts.Client),
		events:    newEvents(opts.LogFile),
	}
}

// Messages. Every one of them is the answer to a command the TUI asked for.
type (
	// tickMsg asks for a refresh.
	tickMsg struct{}
	// statusMsg is the answer to `status --json`.
	statusMsg struct {
		status Status
		err    error
	}
	// noticeMsg is the result of a one-off action, for the footer.
	noticeMsg struct {
		text string
		err  error
	}
)

// Init starts the first fetch and the poll.
func (m Model) Init() tea.Cmd {
	return tea.Batch(fetchStatus(m.client), tick(m.poll),
		fetchPaths(m.client), pathTick(m.pathPoll))
}

// tick schedules the next refresh.
func tick(d time.Duration) tea.Cmd {
	return tea.Tick(d, func(time.Time) tea.Msg { return tickMsg{} })
}

// fetchStatus asks the daemon for the report behind every live view.
func fetchStatus(c Client) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		report, err := c.Status(ctx)
		return statusMsg{status: report, err: err}
	}
}

// Update handles one message.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.events.resize(msg.Width, msg.Height)
		return m, nil

	case tea.KeyMsg:
		return m.handleKey(msg)

	case tickMsg:
		if m.quitting {
			return m, nil
		}
		return m, tea.Batch(fetchStatus(m.client), tick(m.poll),
			tailLog(m.events.logFile, m.events.offset))

	case statusMsg:
		return m.applyStatus(msg)

	case pathTickMsg:
		if m.quitting {
			return m, nil
		}
		return m, tea.Batch(fetchPaths(m.client), pathTick(m.pathPoll))

	case pathsMsg:
		m.paths, m.pathErr = msg.paths, msg.err
		return m, nil

	case logLinesMsg:
		// The log is filled whichever screen is showing, so opening it
		// shows what happened rather than what happens next.
		var cmd tea.Cmd
		m.events, cmd = m.events.update(msg)
		return m, cmd

	case noticeMsg:
		m.notice = msg.text
		m.noticeErr = msg.err != nil
		if msg.err != nil {
			m.notice = msg.err.Error()
		}
		return m, fetchStatus(m.client)
	}

	return m.routeToScreen(msg)
}

// routeToScreen passes a message to the screen that owns it.
func (m Model) routeToScreen(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd
	switch m.screen {
	case screenSetup:
		m.setup, cmd = m.setup.update(msg)
	case screenConflicts:
		m.conflicts, cmd = m.conflicts.update(msg)
	case screenEvents:
		m.events, cmd = m.events.update(msg)
	}
	return m, cmd
}

// applyStatus takes one status answer and decides which screen it puts the
// user on. The exit code is what decides: 3 means there is no identity yet, so
// the guided setup takes over, and anything else the TUI cannot act on is
// fatal.
func (m Model) applyStatus(msg statusMsg) (tea.Model, tea.Cmd) {
	if msg.err != nil {
		// Exit 3 is a data directory with no identity, which is not a
		// failure but the state the guided walk exists for.
		if ExitCode(msg.err) == ExitNotInitialized {
			m.fatal = nil
			m.screen = screenSetup
			m.setup.begin()
			return m, nil
		}
		m.fatal = msg.err
		m.screen = screenFatal
		return m, nil
	}

	m.fatal = nil
	m.status = msg.status
	m.statusAt = time.Now()
	m.events.observe(msg.status)

	if m.screen == screenSetup {
		// The walk stays in charge once the identity exists: adding the
		// folder, the peer and the authorization is the rest of it.
		m.setup.observe(msg.status)
		return m, nil
	}

	switch m.screen {
	case screenLoading, screenFatal, screenNoDaemon:
		if !msg.status.Daemon.Running {
			m.screen = screenNoDaemon
			return m, nil
		}
		m.screen = screenDashboard
	}
	return m, nil
}

// handleKey routes a key press. Quitting is global; everything else belongs to
// the screen with the keyboard, because a screen with a text field must be
// able to take a plain 'q' as the letter q.
func (m Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if msg.Type == tea.KeyCtrlC {
		m.quitting = true
		return m, tea.Quit
	}

	if m.screen == screenSetup {
		if m.setup.done {
			m.screen = screenDashboard
			// The paths were last read before this device had an
			// identity, so they are asked for again rather than
			// waited for at the next thirty-second tick.
			return m, tea.Batch(fetchStatus(m.client), fetchPaths(m.client))
		}
		return m.routeToScreen(msg)
	}

	if msg.String() == "q" {
		m.quitting = true
		return m, tea.Quit
	}

	if m.screen == screenEvents {
		if m.events.wantsBack(msg) {
			m.screen = screenDashboard
			return m, nil
		}
		return m.routeToScreen(msg)
	}

	if m.screen == screenConflicts {
		next, cmd, back := m.conflicts.key(msg)
		m.conflicts = next
		if back {
			m.screen = screenDashboard
			return m, fetchStatus(m.client)
		}
		return m, cmd
	}

	switch msg.String() {
	case "r":
		return m, fetchStatus(m.client)
	case "d":
		m.screen = screenDashboard
		return m, nil
	case "c":
		m.screen = screenConflicts
		return m, m.conflicts.enter()
	case "l":
		m.screen = screenEvents
		return m, m.events.enter()
	}
	if m.screen == screenDashboard {
		next, cmd, handled := m.dashboardKey(msg)
		if handled {
			return next, cmd
		}
	}
	return m, nil
}

// View renders the current screen.
func (m Model) View() string {
	var body string
	switch m.screen {
	case screenLoading:
		body = "connecting to the daemon…"
	case screenFatal:
		body = m.fatalView()
	case screenNoDaemon:
		body = m.noDaemonView()
	case screenSetup:
		body = m.setup.view(m.width)
	case screenDashboard:
		body = m.dashboardView()
	case screenConflicts:
		body = m.conflicts.view(m.width)
	case screenEvents:
		body = m.events.view(m.width)
	}
	return body + "\n" + m.footer()
}

// footer is the one line that is on every screen: the last action's result and
// the keys that work here.
func (m Model) footer() string {
	var b strings.Builder
	if m.notice != "" {
		style := styleOK
		if m.noticeErr {
			style = styleErr
		}
		b.WriteString(style.Render(m.notice) + "\n")
	}
	b.WriteString(styleDim.Render(m.keyHelp()))
	return b.String()
}

// keyHelp lists the keys the current screen answers to.
func (m Model) keyHelp() string {
	switch m.screen {
	case screenSetup:
		return m.setup.help()
	case screenConflicts:
		return m.conflicts.help()
	case screenEvents:
		return m.events.help()
	case screenFatal, screenNoDaemon:
		return "r refresh · q quit"
	default:
		return "↑↓ folder · p pause · u resume · s rescan · c conflicts · l logs · r refresh · q quit"
	}
}

// fatalView is a failure the TUI cannot act on. It names the command that
// failed, so the same thing can be tried outside the TUI.
func (m Model) fatalView() string {
	var b strings.Builder
	b.WriteString(styleErr.Render("syncthis could not read this device") + "\n\n")
	if m.fatal == nil {
		return b.String()
	}
	b.WriteString(m.fatal.Error() + "\n")

	var cmdErr *CmdError
	if !errors.As(m.fatal, &cmdErr) {
		return b.String()
	}
	if cmdErr.Code == ExitNotInitialized {
		b.WriteString("\nthis data directory holds no identity yet. Create one with\n\n")
		b.WriteString("  " + styleCmd.Render(m.client.Command("init", "--name", "NAME")) + "\n")
	}
	b.WriteString("\n" + styleDim.Render("the same thing outside the TUI:") + "\n")
	b.WriteString("  " + styleCmd.Render(cmdErr.Command) + "\n")
	b.WriteString(styleDim.Render(fmt.Sprintf("  exit code %d, see docs/CLI.md", cmdErr.Code)) + "\n")
	return b.String()
}

// noDaemonView is an initialized device with nothing running. Everything the
// dashboard shows comes from the daemon, so the screen says how to start one
// rather than showing a dashboard of nulls.
func (m Model) noDaemonView() string {
	var b strings.Builder
	b.WriteString(styleWarn.Render("no daemon is running for this device") + "\n\n")
	b.WriteString("device:  " + m.status.Device.Name + " (" + short(m.status.Device.ID) + ")\n")
	b.WriteString("datadir: " + m.status.Device.DataDir + "\n\n")
	b.WriteString("nothing is being synchronized until a daemon runs. Start one with\n\n")
	b.WriteString("  " + styleCmd.Render(m.client.Command("daemon", "run")) + "\n\n")
	b.WriteString(styleDim.Render("this screen refreshes on its own; r asks now.") + "\n")
	return b.String()
}

// short abbreviates a device id the way the command line does.
func short(id string) string {
	if len(id) <= 7 {
		return id
	}
	return id[:7]
}

// Run starts the TUI against the given streams and returns when the user
// quits. It is the whole entry point: cmd/syncthis's `tui` subcommand calls it
// with the process streams and a Runner that invokes the command line.
//
// Nothing about this process is load-bearing for synchronization. It opens no
// connection it keeps, holds no lock, owns no transfer and writes nothing to
// the data directory: every screen is a command that runs, answers and is
// over. Losing the terminal, detaching a tmux window, dropping an SSH session
// or killing this process therefore cannot touch the daemon or a transfer in
// flight, and a new TUI started against the same data directory is in the same
// place the old one was after its first refresh. That claim is tested end to
// end in internal/e2e/tui_reconnect_test.go.
func Run(ctx context.Context, opts Options, in io.Reader, out io.Writer) error {
	p := tea.NewProgram(
		New(opts),
		tea.WithContext(ctx),
		tea.WithInput(in),
		tea.WithOutput(out),
		tea.WithAltScreen(),
	)
	_, err := p.Run()
	return cleanExit(err)
}

// cleanExit turns the ways a terminal can go away into a successful return. A
// cancelled context is the process being asked to stop, and a killed program
// is Bubble Tea's answer to the same thing; neither is a failure of
// synchronization, and reporting one would make a dropped SSH session look
// like a syncthis error in a log or an exit code.
func cleanExit(err error) error {
	switch {
	case err == nil,
		errors.Is(err, context.Canceled),
		errors.Is(err, tea.ErrProgramKilled):
		return nil
	default:
		return err
	}
}
