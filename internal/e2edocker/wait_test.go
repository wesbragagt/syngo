package e2edocker

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wesbragagt/syncthis/internal/testutil/tree"
)

// waitFor polls cond until it holds or the timeout runs out. Every wait in
// this package is a poll: a fixed sleep either wastes time or fails under
// load.
func waitFor(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for %s", timeout, what)
		}
		time.Sleep(pollInterval)
	}
}

// indexes returns the devices a wait covers: the ones named, or all of them.
func (c *Cluster) indexes(devices []int) []int {
	if len(devices) > 0 {
		return devices
	}
	all := make([]int, len(c.Devices))
	for i := range all {
		all[i] = i
	}
	return all
}

// diffAgainstFirst describes how the named devices differ from the first one.
func (c *Cluster) diffAgainstFirst(devices []int) []string {
	want := c.Devices[devices[0]].Tree()
	var problems []string
	for _, i := range devices[1:] {
		for _, problem := range tree.Diff(want, c.Devices[i].Tree()) {
			problems = append(problems, fmt.Sprintf("%s vs %s: %s",
				c.Devices[devices[0]].Name, c.Devices[i].Name, problem))
		}
	}
	return problems
}

// quiet reports whether no device of the set has a transfer in its journal.
func (c *Cluster) quiet(devices []int) bool {
	for _, i := range devices {
		if len(c.Devices[i].Transfers()) != 0 {
			return false
		}
	}
	return true
}

// WaitConverged waits until the named devices hold the same tree with nothing
// left in their journals, and then until they have stayed that way for
// settle. Without names it covers every device.
func (c *Cluster) WaitConverged(devices ...int) {
	c.T.Helper()
	set := c.indexes(devices)
	if len(set) < 2 {
		c.T.Fatalf("convergence needs at least two devices, got %v", set)
	}

	start := time.Now()
	var last []string
	deadline := time.Now().Add(convergeTimeout)
	for {
		last = c.diffAgainstFirst(set)
		if len(last) == 0 && c.quiet(set) {
			break
		}
		if time.Now().After(deadline) {
			c.T.Fatalf("devices %v did not converge within %s:\n%s",
				c.names(set), convergeTimeout, strings.Join(last, "\n"))
		}
		time.Sleep(pollInterval)
	}

	// Equal once can mean the next change has not arrived yet, so the
	// trees must hold still for a while before the wait is over.
	until := time.Now().Add(settle)
	for time.Now().Before(until) {
		if problems := c.diffAgainstFirst(set); len(problems) != 0 {
			c.T.Fatalf("devices %v diverged again while settling:\n%s",
				c.names(set), strings.Join(problems, "\n"))
		}
		if !c.quiet(set) {
			c.T.Fatalf("devices %v started a transfer while settling", c.names(set))
		}
		time.Sleep(pollInterval)
	}
	c.T.Logf("devices %v converged in %s", c.names(set), time.Since(start).Round(time.Millisecond))
}

// WaitPeersConnected waits until every peer each device knows is connected.
func (c *Cluster) WaitPeersConnected() {
	c.T.Helper()
	for _, dev := range c.Devices {
		waitFor(c.T, "peers of "+dev.Name+" connected", convergeTimeout, func() bool {
			peers := dev.Status().Peers
			if len(peers) == 0 {
				return false
			}
			for _, peer := range peers {
				if !peer.Connected {
					return false
				}
			}
			return true
		})
	}
}

// CheckInvariants asserts what a converged cluster must look like: the same
// tree everywhere, no partial downloads left behind, empty journals and no
// error the daemon did not expect.
func (c *Cluster) CheckInvariants(devices ...int) {
	c.T.Helper()
	set := c.indexes(devices)
	if problems := c.diffAgainstFirst(set); len(problems) != 0 {
		c.T.Errorf("devices %v hold different trees:\n%s", c.names(set), strings.Join(problems, "\n"))
	}
	for _, i := range set {
		dev := c.Devices[i]
		if parts := dev.PartFiles(); len(parts) != 0 {
			c.T.Errorf("%s left partial downloads: %v", dev.Name, parts)
		}
		if rows := dev.Transfers(); len(rows) != 0 {
			c.T.Errorf("%s has %d transfers left in its journal: %+v", dev.Name, len(rows), rows)
		}
		for _, line := range dev.LogLines("error") {
			if expectedError(line.Msg) {
				continue
			}
			c.T.Errorf("%s logged an error: %s", dev.Name, line.Raw)
		}
	}
}

// expectedError reports whether an error line is one a test may cause on
// purpose. A peer that is being killed, partitioned or restarted is dialled
// by the others while it is down, and that is the sync working, not failing.
func expectedError(msg string) bool {
	allowed := []string{
		"dial", "connect", "connection refused", "connection reset",
		"broken pipe", "EOF", "no route to host", "i/o timeout",
		"peer unavailable", "handshake",
	}
	lower := strings.ToLower(msg)
	for _, word := range allowed {
		if strings.Contains(lower, strings.ToLower(word)) {
			return true
		}
	}
	return false
}

// names turns device indexes into names for a message.
func (c *Cluster) names(devices []int) []string {
	out := make([]string, 0, len(devices))
	for _, i := range devices {
		out = append(out, c.Devices[i].Name)
	}
	return out
}

// lastLines is the tail of a log, for a failure message that should not carry
// the whole run.
func lastLines(text string, n int) string {
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// Has reports whether a path is present in the device's folder.
func (d *Device) Has(rel string) bool {
	d.c.T.Helper()
	_, err := os.Lstat(filepath.Join(d.HostRoot, rel))
	switch {
	case err == nil:
		return true
	case os.IsNotExist(err):
		return false
	default:
		d.c.T.Fatalf("%s: stat %s: %v", d.Name, rel, err)
		return false
	}
}

// WaitPresent waits until a path is in the device's folder.
func (d *Device) WaitPresent(rel string) {
	d.c.T.Helper()
	waitFor(d.c.T, rel+" on "+d.Name, convergeTimeout, func() bool { return d.Has(rel) })
}

// WaitAbsent waits until a path is gone from the device's folder.
func (d *Device) WaitAbsent(rel string) {
	d.c.T.Helper()
	waitFor(d.c.T, rel+" gone from "+d.Name, convergeTimeout, func() bool { return !d.Has(rel) })
}

// Marker writes a file in the second folder of one device and waits for it in
// the second folder of another. Its arrival proves the receiving device ran a
// sync cycle against the writer after the marker was written, which is what a
// test needs before it can say that something else did not cross. Both
// devices need ExtraFolder.
func (c *Cluster) Marker(from, to int, name string) {
	c.T.Helper()
	writer, reader := c.Devices[from], c.Devices[to]
	path := filepath.Join(writer.HostExtra, name)
	if err := os.WriteFile(path, []byte(name), 0o644); err != nil {
		c.T.Fatalf("%s: write marker %s: %v", writer.Name, name, err)
	}
	waitFor(c.T, "marker "+name+" from "+writer.Name+" on "+reader.Name, convergeTimeout, func() bool {
		_, err := os.Lstat(filepath.Join(reader.HostExtra, name))
		return err == nil
	})
}

// upTo is the first n+1 device indexes, for a wait that covers the devices
// that have joined so far.
func (c *Cluster) upTo(n int) []int {
	out := make([]int, 0, n+1)
	for i := 0; i <= n; i++ {
		out = append(out, i)
	}
	return out
}
