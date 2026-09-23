package tui

import (
	"strings"
)

// dashboardView is the summary of the device and its daemon. The live device,
// folder, queue and path tables are added on top of this in the next task.
func (m Model) dashboardView() string {
	var b strings.Builder
	b.WriteString(styleTitle.Render("syncthis") + "  " +
		m.status.Device.Name + " " + styleDim.Render("("+short(m.status.Device.ID)+")") + "\n")
	b.WriteString(styleDim.Render(m.status.Device.DataDir) + "\n\n")
	b.WriteString(m.daemonLine() + "\n")
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
		if l.Error != nil {
			line += "\n" + styleErr.Render("  "+*l.Error)
		}
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
