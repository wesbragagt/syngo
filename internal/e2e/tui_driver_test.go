package e2e

import (
	"bytes"
	"context"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/wesbragagt/syngo/internal/cli"
	"github.com/wesbragagt/syngo/internal/tui"
)

// The TUI tests drive the Bubble Tea model directly rather than through a
// terminal: Update is fed messages, View is read back, and the commands the
// model returns are run exactly as the event loop would run them. That needs
// no pseudo-terminal and no extra dependency, and it exercises the same code a
// real run does, because Bubble Tea's own loop is nothing more than this.

// tuiRunner is tui.Runner over the real command tree, the same adapter
// internal/cli gives the interface in a real run.
func tuiRunner(t *testing.T) tui.Runner {
	t.Helper()
	return tui.RunnerFunc(func(ctx context.Context, args []string) ([]byte, int, error) {
		var stdout bytes.Buffer
		code := cli.ExecuteContext(ctx, args, strings.NewReader(""), &stdout, io.Discard)
		return stdout.Bytes(), code, nil
	})
}

// driver runs a tui.Model the way Bubble Tea's event loop does: every message
// is applied to Update, and every command it returns is run on its own
// goroutine, with what it produced fed back in.
type driver struct {
	t    *testing.T
	mu   sync.Mutex
	m    tea.Model
	msgs chan tea.Msg
	// done is closed when the driver stops, so a command still in flight
	// does not block on a channel nobody reads.
	done chan struct{}
	once sync.Once
}

// newDriver starts a model and runs whatever Init asked for.
func newDriver(t *testing.T, m tea.Model) *driver {
	t.Helper()
	d := &driver{t: t, m: m, msgs: make(chan tea.Msg, 256), done: make(chan struct{})}
	t.Cleanup(d.stop)
	d.run(m.Init())
	return d
}

// stop releases the driver. Commands in flight discard their answers after it.
func (d *driver) stop() { d.once.Do(func() { close(d.done) }) }

// run executes a command off the loop, the way the event loop does, and feeds
// back what it produced. A batch is unfolded into its parts.
func (d *driver) run(cmd tea.Cmd) {
	if cmd == nil {
		return
	}
	go func() {
		msg := cmd()
		switch typed := msg.(type) {
		case nil:
		case tea.BatchMsg:
			for _, part := range typed {
				d.run(part)
			}
		default:
			select {
			case d.msgs <- msg:
			case <-d.done:
			}
		}
	}()
}

// send delivers one message now, without waiting for the pump.
func (d *driver) send(msg tea.Msg) {
	d.mu.Lock()
	next, cmd := d.m.Update(msg)
	d.m = next
	d.mu.Unlock()
	d.run(cmd)
}

// pump applies one queued message, or returns false when none arrived within
// the wait.
func (d *driver) pump(wait time.Duration) bool {
	select {
	case msg := <-d.msgs:
		d.send(msg)
		return true
	case <-time.After(wait):
		return false
	}
}

// view is the frame as it stands, with the styling removed so a test matches
// on what is written rather than on how it is coloured.
func (d *driver) view() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return stripStyles(d.m.View())
}

// waitFrame pumps until the frame contains want, and fails with the frame it
// ended on if it never does.
func (d *driver) waitFrame(what, want string, timeout time.Duration) {
	d.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if strings.Contains(d.view(), want) {
			return
		}
		d.pump(20 * time.Millisecond)
	}
	d.t.Fatalf("timed out waiting for %s (%q). The frame was:\n%s", what, want, d.view())
}

// typeLine types a string and presses enter, which is one answer to one
// question of the guided walk.
func (d *driver) typeLine(text string) {
	for _, r := range text {
		d.send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	d.send(tea.KeyMsg{Type: tea.KeyEnter})
}

// snapshot writes the current frame to the test log. It runs only under -v,
// so an ordinary run is not buried in frames, and it is what makes the
// rendered interface reviewable without a terminal recording.
func (d *driver) snapshot(label string) {
	if !testing.Verbose() {
		return
	}
	d.t.Logf("\n=== %s ===\n%s", label, d.view())
}

// key sends one letter.
func (d *driver) key(r rune) { d.send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}}) }

// stripStyles removes the escape sequences lipgloss added.
func stripStyles(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != 0x1b {
			b.WriteByte(s[i])
			continue
		}
		for i++; i < len(s); i++ {
			if (s[i] >= 'a' && s[i] <= 'z') || (s[i] >= 'A' && s[i] <= 'Z') {
				break
			}
		}
	}
	return b.String()
}

// freeLoopbackAddr picks a loopback address nothing is listening on. The
// guided walk asks for the address peers connect to, and the other device has
// to be told the same one, so it cannot be left to an ephemeral port the
// daemon picks for itself.
func freeLoopbackAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("find a free port: %v", err)
	}
	addr := l.Addr().String()
	if err := l.Close(); err != nil {
		t.Fatalf("release the port: %v", err)
	}
	return addr
}

// runDaemonCLI starts `syngo daemon run` for a data directory, exactly as a
// user would in another terminal, and stops it when the test ends.
func runDaemonCLI(t *testing.T, dataDir string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan int, 1)
	go func() {
		stopped <- cli.ExecuteContext(ctx,
			[]string{"--data-dir", dataDir, "daemon", "run"},
			strings.NewReader(""), io.Discard, io.Discard)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-stopped:
		case <-time.After(daemonTimeout):
			t.Errorf("the daemon for %s did not stop", dataDir)
		}
	})
}

// tighten shortens a device's reconcile and sync intervals to what the rest of
// these tests use, so a cycle happens inside a test rather than at the shipped
// ten-minute and thirty-second defaults. It is `syngo config set`, and it
// is test tuning: nothing about the guided walk depends on it.
func tighten(t *testing.T, dataDir string) {
	t.Helper()
	code := cli.ExecuteContext(context.Background(), []string{
		"--data-dir", dataDir, "config", "set",
		"--rescan-interval", testRescanInterval.String(),
		"--sync-interval", testSyncInterval.String(),
	}, strings.NewReader(""), io.Discard, io.Discard)
	if code != 0 {
		t.Fatalf("config set for %s exited %d", dataDir, code)
	}
}
