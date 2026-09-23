package tui

import (
	"context"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
)

// setupStep is where the guided walk has got to. The order is the order of
// docs/CLI.md's "Quick start": an identity, a folder, a daemon, a peer, an
// authorization, and then waiting for the two devices to converge.
type setupStep int

const (
	// stepName asks for the device name, and stepListen for the address
	// peers connect to. Together they are `syncthis init`.
	stepName setupStep = iota
	stepListen
	// stepFolderPath and stepFolderID are `syncthis folder add`.
	stepFolderPath
	stepFolderID
	// stepDaemon waits for a daemon. Nothing the TUI can run starts one:
	// `daemon run` is a process that stays in the foreground, and a TUI
	// that forked it would own a daemon that dies with the terminal, which
	// is the opposite of what this interface promises.
	stepDaemon
	// stepPeerHost, stepPeerConfirm and stepPeerAdd are `peer test` and
	// then `peer add`: the identity is shown before it is trusted, which is
	// the decision `peer add` asks about on a terminal.
	stepPeerHost
	stepPeerConfirm
	// stepConverge waits until the peer is connected and the folder has
	// been scanned, which is what "paired and synchronizing" means.
	stepConverge
	stepDone
)

// setupModel is the guided walk from an empty data directory to a paired,
// synchronized folder. It runs the same commands docs/CLI.md documents, in the
// order its quick start gives them, and shows each one before it runs it.
type setupModel struct {
	client Client

	step setupStep
	// inputs are the text fields, one per step that has one.
	inputs map[setupStep]textinput.Model
	// working is a command in flight; the walk takes no key while one is.
	working bool
	// err is the last failure, shown with the command that produced it and
	// cleared by the next attempt.
	err error

	// The answers collected so far.
	deviceID string
	folderID string
	peerID   string
	peerName string
	peerAddr string
	// tested is what `peer test` reported about the address, which is what
	// stepPeerConfirm shows.
	tested PeerTested

	// status is the last report, which stepDaemon and stepConverge wait on.
	status Status
	// done marks the walk finished, after which the root model takes the
	// user to the dashboard.
	done bool
}

// folderIDPattern is the folder id `folder add --id` accepts, from
// docs/CLI.md. The walk proposes an id derived from the directory name and
// only proposes one that would be accepted.
var folderIDPattern = regexp.MustCompile(`[^a-z0-9._-]+`)

// newSetup builds the walk. It asks nothing until begin is called.
func newSetup(c Client) setupModel {
	m := setupModel{client: c, inputs: map[setupStep]textinput.Model{}}
	for _, spec := range []struct {
		step        setupStep
		placeholder string
	}{
		{stepName, "this machine's host name"},
		{stepListen, "127.0.0.1:22000"},
		{stepFolderPath, "/home/you/docs"},
		{stepFolderID, "docs"},
		{stepPeerHost, "nas  or  100.64.0.3:22000"},
	} {
		in := textinput.New()
		in.Placeholder = spec.placeholder
		in.CharLimit = 512
		in.Width = 48
		m.inputs[spec.step] = in
	}
	return m
}

// begin puts the walk on its first step with the keyboard in the field.
func (s *setupModel) begin() {
	if s.step != stepName || s.done {
		return
	}
	s.focus(stepName)
}

// focus moves to a step and gives its field, if it has one, the keyboard.
func (s *setupModel) focus(step setupStep) {
	s.step = step
	for key, in := range s.inputs {
		if key == step {
			in.Focus()
		} else {
			in.Blur()
		}
		s.inputs[key] = in
	}
}

// value is what the user typed for a step, trimmed.
func (s setupModel) value(step setupStep) string {
	return strings.TrimSpace(s.inputs[step].Value())
}

// observe takes the latest report. It is what moves the two waiting steps on:
// stepDaemon when a daemon appears, stepConverge when the peer is connected.
func (s *setupModel) observe(report Status) {
	s.status = report
	switch s.step {
	case stepDaemon:
		if report.Daemon.Running {
			s.focus(stepPeerHost)
		}
	case stepConverge:
		if s.paired(report) {
			s.step = stepDone
			s.done = true
		}
	}
}

// paired reports whether the peer the walk added is connected. That is the
// end of the walk: the folder is configured on both sides and the daemons are
// talking, so anything put in the folder now travels.
func (s setupModel) paired(report Status) bool {
	for _, p := range report.Peers {
		if p.ID == s.peerID && p.State == "connected" {
			return true
		}
	}
	return false
}

// Setup messages, one per command the walk runs.
type (
	setupInitMsg struct {
		result InitResult
		err    error
	}
	setupFolderMsg struct {
		result FolderAdded
		err    error
	}
	setupPeerTestMsg struct {
		result PeerTested
		err    error
	}
	setupPeerAddMsg struct {
		result PeerAdded
		err    error
	}
	setupGrantMsg struct {
		err error
	}
)

// setupCtx bounds one command of the walk. `peer test` and `peer add` dial
// another machine, so the bound is generous.
func setupCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 30*time.Second)
}

// update handles one message for the walk.
func (s setupModel) update(msg tea.Msg) (setupModel, tea.Cmd) {
	switch msg := msg.(type) {
	case setupInitMsg:
		s.working = false
		if s.err = msg.err; msg.err != nil {
			s.focus(stepName)
			return s, nil
		}
		s.deviceID = msg.result.DeviceID
		s.focus(stepFolderPath)
		return s, nil

	case setupFolderMsg:
		s.working = false
		if s.err = msg.err; msg.err != nil {
			s.focus(stepFolderID)
			return s, nil
		}
		s.folderID = msg.result.ID
		s.focus(stepDaemon)
		if s.status.Daemon.Running {
			s.focus(stepPeerHost)
		}
		return s, nil

	case setupPeerTestMsg:
		s.working = false
		if s.err = msg.err; msg.err != nil {
			s.focus(stepPeerHost)
			return s, nil
		}
		s.tested = msg.result
		s.focus(stepPeerConfirm)
		return s, nil

	case setupPeerAddMsg:
		s.working = false
		if s.err = msg.err; msg.err != nil {
			s.focus(stepPeerHost)
			return s, nil
		}
		s.peerID, s.peerName, s.peerAddr = msg.result.ID, msg.result.Name, msg.result.Address
		s.working = true
		return s, s.grant()

	case setupGrantMsg:
		s.working = false
		if s.err = msg.err; msg.err != nil {
			s.focus(stepPeerHost)
			return s, nil
		}
		s.step = stepConverge
		if s.paired(s.status) {
			s.step = stepDone
			s.done = true
		}
		return s, nil

	case tea.KeyMsg:
		return s.key(msg)
	}
	return s, nil
}

// key handles one key press.
func (s setupModel) key(msg tea.KeyMsg) (setupModel, tea.Cmd) {
	if s.working {
		return s, nil
	}

	switch s.step {
	case stepPeerConfirm:
		switch msg.String() {
		case "y", "enter":
			s.err = nil
			s.working = true
			return s, s.peerAdd()
		case "n", "esc":
			s.focus(stepPeerHost)
			return s, nil
		}
		return s, nil

	case stepDaemon, stepConverge, stepDone:
		return s, nil
	}

	if msg.Type == tea.KeyEnter {
		return s.advance()
	}

	in, cmd := s.inputs[s.step].Update(msg)
	s.inputs[s.step] = in
	return s, cmd
}

// advance takes the answer of the current field and moves on, running the
// command when the last field of a command has been filled in.
func (s setupModel) advance() (setupModel, tea.Cmd) {
	s.err = nil
	switch s.step {
	case stepName:
		s.focus(stepListen)
		return s, nil

	case stepListen:
		s.working = true
		return s, s.runInit()

	case stepFolderPath:
		path := s.value(stepFolderPath)
		if path == "" {
			return s, nil
		}
		// Propose an id from the directory name, which is what a
		// person would have typed. Both devices must use the same one,
		// so it is shown and editable rather than applied silently.
		id := s.inputs[stepFolderID]
		if strings.TrimSpace(id.Value()) == "" {
			id.SetValue(folderIDFromPath(path))
		}
		s.inputs[stepFolderID] = id
		s.focus(stepFolderID)
		return s, nil

	case stepFolderID:
		if s.value(stepFolderID) == "" {
			return s, nil
		}
		s.working = true
		return s, s.folderAdd()

	case stepPeerHost:
		if s.value(stepPeerHost) == "" {
			return s, nil
		}
		s.working = true
		return s, s.peerTest()
	}
	return s, nil
}

// folderIDFromPath proposes a folder id from a directory name, keeping only
// what `folder add --id` accepts.
func folderIDFromPath(path string) string {
	base := strings.ToLower(filepath.Base(filepath.Clean(path)))
	base = folderIDPattern.ReplaceAllString(base, "-")
	base = strings.Trim(base, "-._")
	if base == "" {
		return "folder"
	}
	if len(base) > 64 {
		base = base[:64]
	}
	return base
}

// The commands the walk runs, one method each, named after the syncthis
// command they are.

func (s setupModel) runInit() tea.Cmd {
	name, listen := s.value(stepName), s.value(stepListen)
	c := s.client
	return func() tea.Msg {
		ctx, cancel := setupCtx()
		defer cancel()
		result, err := c.Init(ctx, name, listen)
		return setupInitMsg{result: result, err: err}
	}
}

func (s setupModel) folderAdd() tea.Cmd {
	path, id := s.value(stepFolderPath), s.value(stepFolderID)
	c := s.client
	return func() tea.Msg {
		ctx, cancel := setupCtx()
		defer cancel()
		result, err := c.FolderAdd(ctx, path, id)
		return setupFolderMsg{result: result, err: err}
	}
}

func (s setupModel) peerTest() tea.Cmd {
	host := s.value(stepPeerHost)
	c := s.client
	return func() tea.Msg {
		ctx, cancel := setupCtx()
		defer cancel()
		result, err := c.PeerTest(ctx, host)
		return setupPeerTestMsg{result: result, err: err}
	}
}

func (s setupModel) peerAdd() tea.Cmd {
	host, id := s.value(stepPeerHost), s.tested.DeviceID
	c := s.client
	return func() tea.Msg {
		ctx, cancel := setupCtx()
		defer cancel()
		result, err := c.PeerAdd(ctx, host, id)
		return setupPeerAddMsg{result: result, err: err}
	}
}

// grant authorizes the peer for the folder the walk added. Trusting a device
// and sharing a folder with it are two decisions in this program, and a folder
// added without a grant is served to nobody, so the walk makes the second one
// too rather than leaving a setup that looks paired and moves nothing.
func (s setupModel) grant() tea.Cmd {
	folderID, peerID := s.folderID, s.peerID
	c := s.client
	return func() tea.Msg {
		ctx, cancel := setupCtx()
		defer cancel()
		_, err := c.FolderGrant(ctx, folderID, peerID)
		return setupGrantMsg{err: err}
	}
}

// commandFor is the syncthis command the current step runs, shown above the
// field so the walk teaches the command line rather than hiding it.
func (s setupModel) commandFor(step setupStep) string {
	switch step {
	case stepName, stepListen:
		return s.client.Command("init", "--name", orPlaceholder(s.value(stepName), "NAME"),
			"--listen", orPlaceholder(s.value(stepListen), "HOST:PORT"))
	case stepFolderPath, stepFolderID:
		return s.client.Command("folder", "add", orPlaceholder(s.value(stepFolderPath), "PATH"),
			"--id", orPlaceholder(s.value(stepFolderID), "ID"))
	case stepDaemon:
		return s.client.Command("daemon", "run")
	case stepPeerHost:
		return s.client.Command("peer", "test", orPlaceholder(s.value(stepPeerHost), "HOST[:PORT]"))
	case stepPeerConfirm:
		return s.client.Command("peer", "add", s.value(stepPeerHost), "--device-id", s.tested.DeviceID)
	case stepConverge, stepDone:
		return s.client.Command("status")
	}
	return ""
}

// orPlaceholder shows what a command would be run with, using the placeholder
// where the user has not typed anything yet.
func orPlaceholder(value, placeholder string) string {
	if value == "" {
		return placeholder
	}
	return value
}

// view renders the walk.
func (s setupModel) view(width int) string {
	var b strings.Builder
	b.WriteString(styleTitle.Render("syncthis setup") + "  " +
		styleDim.Render(sprintf("step %d of %d", int(s.step)+1, int(stepDone)+1)) + "\n\n")

	switch s.step {
	case stepName:
		b.WriteString("What is this device called? Other devices see this name.\n")
		b.WriteString(styleDim.Render("Empty takes this machine's host name.") + "\n\n")
		b.WriteString(s.inputs[stepName].View() + "\n")
	case stepListen:
		b.WriteString("Which address should peers connect to?\n")
		b.WriteString(styleDim.Render("On a tailnet this is the tailnet address; empty takes 127.0.0.1:22000.") + "\n\n")
		b.WriteString(s.inputs[stepListen].View() + "\n")
	case stepFolderPath:
		b.WriteString("Which directory should be synchronized?\n")
		b.WriteString(styleDim.Render("It must exist already.") + "\n\n")
		b.WriteString(s.inputs[stepFolderPath].View() + "\n")
	case stepFolderID:
		b.WriteString("What is the folder called?\n")
		b.WriteString(styleDim.Render("Both devices must use the same id. Lower case, digits, . _ or -") + "\n\n")
		b.WriteString(s.inputs[stepFolderID].View() + "\n")
	case stepDaemon:
		b.WriteString(styleWarn.Render("waiting for a daemon") + "\n\n")
		b.WriteString("Nothing synchronizes until a daemon runs, and it has to keep\n")
		b.WriteString("running, so start it in another terminal or as a service:\n\n")
		b.WriteString("  " + styleCmd.Render(s.client.Command("daemon", "run")) + "\n\n")
		b.WriteString(styleDim.Render("This step finishes on its own as soon as one answers.") + "\n")
	case stepPeerHost:
		b.WriteString("Which device should this one pair with?\n")
		b.WriteString(styleDim.Render("A tailnet name, a MagicDNS name or an address. Port 22000 by default.") + "\n\n")
		b.WriteString(s.inputs[stepPeerHost].View() + "\n")
	case stepPeerConfirm:
		b.WriteString(s.confirmView())
	case stepConverge:
		b.WriteString(s.convergeView())
	case stepDone:
		b.WriteString(s.doneView())
	}

	if cmd := s.commandFor(s.step); cmd != "" && s.step != stepDaemon {
		b.WriteString("\n" + styleDim.Render("this step runs:") + "\n  " + styleCmd.Render(truncate(cmd, width-2)) + "\n")
	}
	if s.working {
		b.WriteString("\n" + styleDim.Render("running…") + "\n")
	}
	if s.err != nil {
		b.WriteString("\n" + styleErr.Render(s.err.Error()) + "\n")
	}
	return b.String()
}

// confirmView shows the identity the address presented, which is the decision
// `peer add` asks about on a terminal.
func (s setupModel) confirmView() string {
	var b strings.Builder
	b.WriteString("This device answered at " + s.tested.Address + ":\n\n")
	b.WriteString("  name:      " + s.tested.Name + "\n")
	b.WriteString("  device id: " + s.tested.DeviceID + "\n")
	if s.tested.Authenticated {
		b.WriteString("  " + styleOK.Render("it already trusts this device") + "\n")
	} else {
		reason := s.tested.RejectReason
		if reason == "" {
			reason = "run the same setup there and pair back"
		}
		b.WriteString("  " + styleWarn.Render("it does not trust this device yet: "+reason) + "\n")
	}
	b.WriteString("\nTrust it? " + styleTitle.Render("y") + " to pair, " +
		styleTitle.Render("n") + " to type another address.\n")
	return b.String()
}

// convergeView waits for the two daemons to connect.
func (s setupModel) convergeView() string {
	var b strings.Builder
	b.WriteString(styleWarn.Render("pairing "+s.peerName+" ("+short(s.peerID)+")") + "\n\n")
	b.WriteString("This device now trusts " + s.peerAddr + " and shares folder " +
		styleTitle.Render(s.folderID) + " with it.\n\n")
	b.WriteString("Run the same setup on the other device, pointing it back here, and\n")
	b.WriteString("give folder " + styleTitle.Render(s.folderID) + " the same id there. Pairing is done when both\n")
	b.WriteString("sides have added the other.\n\n")
	for _, p := range s.status.Peers {
		if p.ID != s.peerID {
			continue
		}
		b.WriteString("peer state: " + stateStyle(p.State).Render(p.State) + "\n")
		if p.Error != nil {
			b.WriteString(styleErr.Render("  "+*p.Error) + "\n")
		}
	}
	return b.String()
}

// doneView is the end of the walk.
func (s setupModel) doneView() string {
	var b strings.Builder
	b.WriteString(styleOK.Render("paired and synchronizing") + "\n\n")
	b.WriteString("folder " + styleTitle.Render(s.folderID) + " is shared with " +
		s.peerName + " (" + short(s.peerID) + "), and the two daemons are connected.\n\n")
	b.WriteString("Put a file in the folder and it travels.\n\n")
	b.WriteString(styleDim.Render("press any key for the dashboard") + "\n")
	return b.String()
}

// help lists the keys of the current step.
func (s setupModel) help() string {
	switch s.step {
	case stepPeerConfirm:
		return "y pair · n another address · ctrl+c quit"
	case stepDaemon, stepConverge:
		return "waiting · ctrl+c quit"
	case stepDone:
		return "any key dashboard · ctrl+c quit"
	default:
		return "enter next · ctrl+c quit"
	}
}
