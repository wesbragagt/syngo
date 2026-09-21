package e2edocker

import "strings"

// iface is the interface every container reaches the cluster network through.
const iface = "eth0"

// requireNetAdmin skips the test unless a container may change its own
// networking. Rootless podman and some CI sandboxes deny NET_ADMIN, and a
// partition that silently does nothing would report a green run that tested
// the opposite of what it claims.
func (c *Cluster) requireNetAdmin() {
	c.T.Helper()
	if res := c.d.exec(c.Devices[0].Ctr, rootUser, "iptables", "-S"); res.code != 0 {
		c.T.Skipf("this test needs NET_ADMIN in the containers: iptables -S exited %d: %s",
			res.code, strings.TrimSpace(res.stderr))
	}
}

// Partition cuts the link between two devices while leaving both of them on
// the network: they keep reaching everyone else. The rules are installed on
// both sides and in both directions, so neither a packet nor its answer gets
// through and neither side sees a clean connection close.
func (c *Cluster) Partition(a, b int) {
	c.T.Helper()
	c.drop(c.Devices[a], c.Devices[b].IP)
	c.drop(c.Devices[b], c.Devices[a].IP)
	c.T.Logf("partitioned %s from %s", c.Devices[a].Name, c.Devices[b].Name)
}

// drop installs the firewall rules of one side of a partition.
func (c *Cluster) drop(dev *Device, peerIP string) {
	c.T.Helper()
	for _, rule := range [][]string{
		{"-I", "INPUT", "-s", peerIP, "-j", "DROP"},
		{"-I", "OUTPUT", "-d", peerIP, "-j", "DROP"},
	} {
		res := c.d.exec(dev.Ctr, rootUser, append([]string{"iptables"}, rule...)...)
		if res.code != 0 {
			c.T.Fatalf("%s: iptables %s: exit %d: %s", dev.Name,
				strings.Join(rule, " "), res.code, strings.TrimSpace(res.stderr))
		}
	}
}

// Isolate takes a device off the network entirely. Unlike a partition this
// removes the address, so the device cannot even try: it is the shape of a
// laptop that was closed rather than a link that went bad.
func (c *Cluster) Isolate(i int) {
	c.T.Helper()
	c.d.mustRun("network", "disconnect", c.Network, c.Devices[i].Ctr)
	c.isolated[i] = true
	c.T.Logf("isolated %s from %s", c.Devices[i].Name, c.Network)
}

// Rejoin puts an isolated device back on the network at the address it had, so
// the peers that stored it reach it again without being told anything.
func (c *Cluster) Rejoin(i int) {
	c.T.Helper()
	dev := c.Devices[i]
	c.d.mustRun("network", "connect", "--ip", dev.IP, c.Network, dev.Ctr)
	delete(c.isolated, i)
	c.T.Logf("rejoined %s to %s at %s", dev.Name, c.Network, dev.IP)
}

// Netem shapes the traffic of one device, for instance "delay 100ms 20ms loss
// 1%" or "rate 50mbit". The qdisc sits on the device's own interface, so it
// applies to every peer it talks to.
func (c *Cluster) Netem(i int, spec string) {
	c.T.Helper()
	args := append([]string{"tc", "qdisc", "replace", "dev", iface, "root", "netem"}, strings.Fields(spec)...)
	res := c.d.exec(c.Devices[i].Ctr, rootUser, args...)
	if res.code != 0 {
		c.T.Skipf("this test needs netem in the containers: tc exited %d: %s",
			res.code, strings.TrimSpace(res.stderr))
	}
	c.netem[i] = true
	c.T.Logf("shaped %s with netem %s", c.Devices[i].Name, spec)
}

// ClearNetem removes the shaping from one device. A device that was never
// shaped is left alone.
func (c *Cluster) ClearNetem(i int) {
	c.T.Helper()
	if !c.netem[i] {
		return
	}
	c.d.exec(c.Devices[i].Ctr, rootUser, "tc", "qdisc", "del", "dev", iface, "root")
	delete(c.netem, i)
}

// Heal undoes every network fault the test caused: firewall rules, isolations
// and shaping. It waits until every pairing is connected again, so what comes
// after it measures the sync rather than the reconnection.
func (c *Cluster) Heal() {
	c.T.Helper()
	for i, dev := range c.Devices {
		for _, chain := range []string{"INPUT", "OUTPUT"} {
			c.d.exec(dev.Ctr, rootUser, "iptables", "-F", chain)
		}
		c.ClearNetem(i)
	}
	for i := range c.Devices {
		if c.isolated[i] {
			c.Rejoin(i)
		}
	}
	c.WaitPeersConnected()
	c.T.Logf("healed the network of cluster %s", c.RunID)
}
