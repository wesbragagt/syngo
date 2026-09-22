package e2edocker

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wesbragagt/syncthis/internal/adapters/blake3hash"
	"github.com/wesbragagt/syncthis/internal/adapters/jsonconfig"
	"github.com/wesbragagt/syncthis/internal/app/status"
	"github.com/wesbragagt/syncthis/internal/domain/conflict"
	"github.com/wesbragagt/syncthis/internal/testutil/tree"
)

// Topology is the set of pairings of a cluster, as index pairs. Every pair is
// paired in both directions, which is what two `peer add` calls do.
type Topology func(n int) [][2]int

// Mesh pairs every device with every other one.
func Mesh(n int) [][2]int {
	var edges [][2]int
	for i := 0; i < n; i++ {
		for j := i + 1; j < n; j++ {
			edges = append(edges, [2]int{i, j})
		}
	}
	return edges
}

// Star pairs every device with the hub and no two spokes with each other, so
// anything a spoke has reaches the other spokes only by being relayed.
func Star(hub int) Topology {
	return func(n int) [][2]int {
		var edges [][2]int
		for i := 0; i < n; i++ {
			if i != hub {
				edges = append(edges, [2]int{hub, i})
			}
		}
		return edges
	}
}

// DeviceSpec is how one device of a cluster is configured. The zero value is
// a sendreceive device with one folder, bound with --allow-public.
type DeviceSpec struct {
	Name string
	// Mode is the folder mode: sendreceive, sendonly or receiveonly.
	Mode string
	// DeletePolicy is propagate or never.
	DeletePolicy string
	// NoFolder starts the device with nothing configured, so a test can
	// add the folder through the CLI while the daemon runs.
	NoFolder bool
	// ExtraFolder gives the device a second sendreceive folder at /extra,
	// which carries the markers a negative assertion needs.
	ExtraFolder bool
	// UID and GID are the user the container runs as. Zero means the user
	// running the test, which keeps the bind mounts readable from the
	// host; a test that wants devices to disagree about ownership names
	// its own.
	UID, GID int
	// ReadOnlyRoot mounts the synchronized folder read-only, which is a
	// device that may serve the folder and not write it.
	ReadOnlyRoot bool
	// BindIface runs the daemon with --interface eth0 rather than
	// --allow-public, which is the other way to reach a bindable address.
	BindIface bool
}

// ClusterOptions is how a cluster is built.
type ClusterOptions struct {
	Devices  []DeviceSpec
	Topology Topology
	// Config changes the settings written to config.json before init, on
	// every device.
	Config func(*jsonconfig.Config)
}

// Cluster is a set of devices, one container each, on a network of their own.
type Cluster struct {
	T       *testing.T
	RunID   string
	Network string
	Subnet  string
	Devices []*Device

	d   docker
	tmp string
	// isolated and netem remember the faults a test caused, so Heal can
	// undo exactly those.
	isolated map[int]bool
	netem    map[int]bool
	started  time.Time
}

// Device is one daemon in one container.
type Device struct {
	Index     int
	Name      string
	ID        string
	IP        string
	Ctr       string
	HostData  string
	HostRoot  string
	HostExtra string
	Spec      DeviceSpec

	c *Cluster
}

// Start brings up n sendreceive devices in a mesh, each with the docs folder.
func Start(t *testing.T, n int) *Cluster {
	t.Helper()
	specs := make([]DeviceSpec, n)
	for i := range specs {
		specs[i] = DeviceSpec{Name: string(rune('a' + i))}
	}
	return StartWith(t, ClusterOptions{Devices: specs})
}

// StartWith brings up a cluster: containers, identities, folders, daemons and
// pairings, and waits until every edge is connected.
func StartWith(t *testing.T, o ClusterOptions) *Cluster {
	t.Helper()
	if len(o.Devices) == 0 {
		t.Fatal("a cluster needs at least one device")
	}
	if o.Topology == nil {
		o.Topology = Mesh
	}

	c := &Cluster{
		T:        t,
		RunID:    runID(),
		d:        docker{t: t, bin: dockerBin},
		tmp:      t.TempDir(),
		isolated: map[int]bool{},
		netem:    map[int]bool{},
		started:  time.Now(),
	}
	c.Network = "syncthis-e2e-" + c.RunID
	t.Cleanup(c.Close)
	c.createNetwork()

	for i, spec := range o.Devices {
		if spec.Name == "" {
			spec.Name = string(rune('a' + i))
		}
		c.Devices = append(c.Devices, c.createDevice(i, spec, o.Config))
	}
	for _, dev := range c.Devices {
		dev.StartDaemon()
	}
	for _, edge := range o.Topology(len(c.Devices)) {
		c.Pair(edge[0], edge[1])
	}
	c.WaitPeersConnected()
	t.Logf("cluster %s up with %d devices in %s", c.RunID, len(c.Devices), time.Since(c.started).Round(time.Millisecond))
	return c
}

// runID is what every container, network and artifact directory of one
// cluster is named after.
func runID() string {
	var b [3]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("read random: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}

// createNetwork makes the bridge the devices talk over. The subnet is fixed
// so the addresses are known before the containers exist; another network
// already holding it is retried past, because a dev machine runs other
// containers too.
func (c *Cluster) createNetwork() {
	for attempt := 0; attempt < 10; attempt++ {
		subnet := fmt.Sprintf("172.30.%d.0/24", 100+attempt)
		res, err := c.d.try("network", "create",
			"--label", "syncthis.e2e=1", "--label", "syncthis.e2e.run="+c.RunID,
			"--subnet", subnet, c.Network)
		if err != nil {
			c.T.Fatalf("create network %s: %v", c.Network, err)
		}
		if res.code == 0 {
			c.Subnet = subnet
			return
		}
		c.T.Logf("subnet %s unavailable: %s", subnet, strings.TrimSpace(res.stderr))
	}
	c.T.Fatalf("no free subnet in 172.30.100.0/24..172.30.109.0/24 for %s", c.Network)
}

// createDevice starts one container, writes its configuration, creates its
// identity and adds its folder. The daemon is started separately, after every
// device exists, so pairing addresses are all known.
func (c *Cluster) createDevice(i int, spec DeviceSpec, tweak func(*jsonconfig.Config)) *Device {
	c.T.Helper()
	dev := &Device{
		Index: i, Name: spec.Name, Spec: spec, c: c,
		IP:        fmt.Sprintf("%s.%d", strings.TrimSuffix(c.Subnet, ".0/24"), 10+i),
		Ctr:       fmt.Sprintf("syncthis-e2e-%s-%s", c.RunID, spec.Name),
		HostData:  filepath.Join(c.tmp, spec.Name, "data"),
		HostRoot:  filepath.Join(c.tmp, spec.Name, "docs"),
		HostExtra: filepath.Join(c.tmp, spec.Name, "extra"),
	}
	for _, dir := range []string{dev.HostData, dev.HostRoot, dev.HostExtra} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			c.T.Fatalf("make %s: %v", dir, err)
		}
	}

	// config.json is written before init, which preserves a configuration
	// that is already there.
	config := jsonconfig.Default()
	config.Listen = fmt.Sprintf("0.0.0.0:%d", peerPort)
	config.ChunkSize = chunkSize
	config.RescanInterval = jsonconfig.Duration(rescanEvery)
	config.SyncInterval = jsonconfig.Duration(syncEvery)
	config.LogLevel = "debug"
	if tweak != nil {
		tweak(&config)
	}
	if err := jsonconfig.Save(dev.HostData, config); err != nil {
		c.T.Fatalf("write config of %s: %v", dev.Name, err)
	}
	// A container running as a user of its own has to be able to write the
	// mounts and read the configuration the host put there; the host has
	// to be able to read back what that user wrote.
	if spec.UID != 0 || spec.GID != 0 {
		for _, dir := range []string{dev.HostData, dev.HostRoot, dev.HostExtra} {
			if err := os.Chmod(dir, 0o777); err != nil {
				c.T.Fatalf("open %s to every user: %v", dir, err)
			}
		}
		// init rewrites the configuration it was given, so the file
		// the host wrote has to be writable by the container's user
		// too.
		if err := os.Chmod(filepath.Join(dev.HostData, jsonconfig.FileName), 0o666); err != nil {
			c.T.Fatalf("make the config of %s writable: %v", dev.Name, err)
		}
	}

	// A container runs as the host user unless the test asked otherwise,
	// so everything the daemon
	// writes on the two bind mounts is owned by whoever runs the test:
	// the host can hash the tree, read .syncthis and delete the temporary
	// directory afterwards. Commands that need capabilities (iptables,
	// tc) are execed as root instead.
	args := []string{"run", "-d", "--name", dev.Ctr, "--user", dev.user(),
		"--label", "syncthis.e2e=1", "--label", "syncthis.e2e.run=" + c.RunID,
		"--network", c.Network, "--ip", dev.IP,
		"--cap-add", "NET_ADMIN",
		"-v", dev.HostData + ":" + containerData,
	}
	// A read-only root is the shape of a folder the daemon may read and
	// not write: the mount refuses every write rather than the daemon
	// choosing not to try.
	root := dev.HostRoot + ":" + containerRoot
	if spec.ReadOnlyRoot {
		root += ":ro"
	}
	args = append(args, "-v", root)
	if spec.ExtraFolder {
		args = append(args, "-v", dev.HostExtra+":"+containerExtra)
	}
	c.d.mustRun(append(args, image)...)

	var identity struct {
		DeviceID string `json:"device_id"`
	}
	dev.RunJSON(&identity, "init", "--name", dev.Name)
	dev.ID = identity.DeviceID

	if !spec.NoFolder {
		args := []string{"folder", "add", containerRoot, "--id", folderID}
		if spec.Mode != "" {
			args = append(args, "--mode", spec.Mode)
		}
		if spec.DeletePolicy != "" {
			args = append(args, "--delete-policy", spec.DeletePolicy)
		}
		dev.RunOK(args...)
	}
	if spec.ExtraFolder {
		// The marker folder is sendreceive on every device whatever
		// the first folder's mode is: its whole purpose is to cross.
		dev.RunOK("folder", "add", containerExtra, "--id", extraID)
	}
	return dev
}

// rootUser is the user a docker exec takes to keep the capabilities the
// container was given; the containers themselves run as the host user.
const rootUser = "0:0"

// hostUser is the uid:gid a container runs as unless the test asked for
// another one: everything the daemon writes on the bind mounts is then owned
// by whoever runs the test, so the host can hash the tree and delete it
// afterwards.
func hostUser() string { return fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()) }

// user is the uid:gid this device's container runs as.
func (d *Device) user() string {
	if d.Spec.UID == 0 && d.Spec.GID == 0 {
		return hostUser()
	}
	return fmt.Sprintf("%d:%d", d.Spec.UID, d.Spec.GID)
}

// Device returns one device of the cluster.
func (c *Cluster) Device(i int) *Device { return c.Devices[i] }

// Pair makes two devices know each other, which is what `peer add` on both
// sides does.
func (c *Cluster) Pair(a, b int) {
	c.T.Helper()
	c.Devices[a].addPeer(c.Devices[b])
	c.Devices[b].addPeer(c.Devices[a])
}

// Unpair makes two devices forget each other.
func (c *Cluster) Unpair(a, b int) {
	c.T.Helper()
	c.Devices[a].RunOK("peer", "remove", c.Devices[b].ID, "--yes")
	c.Devices[b].RunOK("peer", "remove", c.Devices[a].ID, "--yes")
}

// addPeer stores one peer on this device, pinning the identity so nothing is
// asked.
func (d *Device) addPeer(peer *Device) {
	d.c.T.Helper()
	d.RunOK("peer", "add", fmt.Sprintf("%s:%d", peer.IP, peerPort),
		"--device-id", peer.ID, "--name", peer.Name, "--yes")
}

// Close removes everything the cluster created. It runs from t.Cleanup, after
// the artifacts are saved.
func (c *Cluster) Close() {
	if c.T.Failed() || keepArtifacts {
		c.SaveArtifacts("failed")
	}
	for _, dev := range c.Devices {
		dev.giveMountsBack()
	}
	for _, dev := range c.Devices {
		c.d.quiet("rm", "-f", dev.Ctr)
	}
	if c.Network != "" {
		c.d.quiet("network", "rm", c.Network)
	}
}

// giveMountsBack hands the bind mounts of a device that ran as another user
// back to the user running the test. Without it the files the daemon created
// belong to a uid the host cannot delete, and t.TempDir fails the test while
// cleaning up. A container that is already gone is not an error: the test is
// over either way.
func (d *Device) giveMountsBack() {
	if d.Spec.UID == 0 && d.Spec.GID == 0 {
		return
	}
	d.c.d.quiet("exec", "--user", rootUser, d.Ctr,
		"chown", "-R", hostUser(), containerData, containerRoot)
}

// Run executes a syncthis command in the container and returns its stdout and
// exit code. Anything on stderr is logged.
func (d *Device) Run(args ...string) (string, int) {
	d.c.T.Helper()
	res := d.c.d.exec(d.Ctr, "", append([]string{"syncthis", "--data-dir", containerData}, args...)...)
	if strings.TrimSpace(res.stderr) != "" {
		d.c.T.Logf("%s stderr: %s", d.Name, strings.TrimSpace(res.stderr))
	}
	return res.stdout, res.code
}

// RunOK executes a syncthis command and fails the test unless it exits zero.
func (d *Device) RunOK(args ...string) string {
	d.c.T.Helper()
	out, code := d.Run(args...)
	if code != 0 {
		d.c.T.Fatalf("%s: syncthis %s: exit %d: %s", d.Name, strings.Join(args, " "), code, out)
	}
	return out
}

// RunJSON executes a syncthis command with --json and decodes the document.
func (d *Device) RunJSON(v any, args ...string) {
	d.c.T.Helper()
	out := d.RunOK(append(args, "--json")...)
	if err := json.Unmarshal([]byte(out), v); err != nil {
		d.c.T.Fatalf("%s: decode `syncthis %s --json`: %v: %s", d.Name, strings.Join(args, " "), err, out)
	}
}

// Status reports what the device knows. It works with or without a daemon:
// without one the answer comes from the database.
func (d *Device) Status() status.Report {
	d.c.T.Helper()
	var report status.Report
	d.RunJSON(&report, "status")
	return report
}

// transferRow is one entry of the download journal.
type transferRow struct {
	ID          string `json:"id"`
	FolderID    string `json:"folder_id"`
	Path        string `json:"path"`
	State       string `json:"state"`
	ChunksTotal int    `json:"chunks_total"`
	ChunksDone  int    `json:"chunks_done"`
	Error       any    `json:"error"`
}

// Transfers lists the download journal of the device.
func (d *Device) Transfers() []transferRow {
	d.c.T.Helper()
	var rows []transferRow
	d.RunJSON(&rows, "transfer", "list")
	return rows
}

// conflictRow is one recorded conflict.
type conflictRow struct {
	ID           string `json:"id"`
	FolderID     string `json:"folder_id"`
	Path         string `json:"path"`
	ConflictPath string `json:"conflict_path"`
	Reason       string `json:"reason"`
	LocalHash    string `json:"local_hash"`
	RemoteHash   string `json:"remote_hash"`
	Resolved     bool   `json:"resolved"`
}

// Conflicts lists the conflicts the device recorded.
func (d *Device) Conflicts() []conflictRow {
	d.c.T.Helper()
	var rows []conflictRow
	d.RunJSON(&rows, "conflict", "list")
	return rows
}

// StartDaemon starts the daemon inside the container and waits until it
// answers. Its output goes to daemon.log on the data mount, so the host reads
// it without docker.
func (d *Device) StartDaemon() {
	d.c.T.Helper()
	// A daemon that refuses to start exits instead of answering, and the
	// exit line tells the two apart at once rather than after a timeout.
	before := len(d.DaemonExits())
	bind := "--allow-public"
	if d.Spec.BindIface {
		bind = "--interface eth0"
	}
	// The exit code is appended to daemon.exit rather than lost, because a
	// daemon that refuses to start (a stale pid file, a socket still
	// bound) exits non-zero in the background and would otherwise look
	// like a slow start. The shell waits for the daemon, so the pid file
	// still holds the daemon's own pid.
	cmd := fmt.Sprintf(
		"syncthis --data-dir %s --log-level debug --log-format json daemon run %s >>%s/daemon.stdout 2>>%s/daemon.log; echo $? >>%s/daemon.exit",
		containerData, bind, containerData, containerData, containerData)
	d.c.d.execDetached(d.Ctr, "", "sh", "-c", cmd)

	waitFor(d.c.T, d.Name+" daemon running", daemonTimeout, func() bool {
		if exits := d.DaemonExits(); len(exits) > before {
			d.c.T.Fatalf("%s: daemon run exited %d instead of starting; log:\n%s",
				d.Name, exits[before], lastLines(d.Logs(), 20))
		}
		return d.Status().Daemon.Running
	})
}

// StopDaemon asks the daemon to shut down and waits until it has.
func (d *Device) StopDaemon() {
	d.c.T.Helper()
	d.signalDaemon("TERM")
}

// KillDaemon takes the daemon down without letting it clean up, which is what
// a crash looks like.
func (d *Device) KillDaemon() {
	d.c.T.Helper()
	d.signalDaemon("KILL")
}

// signalDaemon sends a signal to the daemon process and waits until it is
// gone. The pid comes from the pid file, so a daemon that was killed before
// is noticed rather than signalled.
func (d *Device) signalDaemon(sig string) {
	d.c.T.Helper()
	pid := d.DaemonPID()
	exits := len(d.DaemonExits())
	d.c.d.exec(d.Ctr, "", "kill", "-"+sig, strconv.Itoa(pid))
	waitFor(d.c.T, fmt.Sprintf("%s daemon pid %d gone", d.Name, pid), daemonTimeout, func() bool {
		return d.c.d.exec(d.Ctr, "", "kill", "-0", strconv.Itoa(pid)).code != 0
	})
	// The shell that started the daemon records its exit code; waiting for
	// that line keeps the next start from mistaking it for its own.
	waitFor(d.c.T, d.Name+" daemon exit recorded", daemonTimeout, func() bool {
		return len(d.DaemonExits()) > exits
	})
}

// KillContainer kills the container outright and starts it again. The daemon
// is not running afterwards; the data and folder mounts and the address are
// the same ones.
func (d *Device) KillContainer() {
	d.c.T.Helper()
	d.c.d.mustRun("kill", "-s", "KILL", d.Ctr)
	d.c.d.mustRun("start", d.Ctr)
}

// Restart stops the daemon and starts it again from the same data directory.
func (d *Device) Restart() {
	d.c.T.Helper()
	d.StopDaemon()
	d.StartDaemon()
}

// DaemonPID reads the pid the daemon wrote.
func (d *Device) DaemonPID() int {
	d.c.T.Helper()
	raw, err := os.ReadFile(filepath.Join(d.HostData, "daemon.pid"))
	if err != nil {
		d.c.T.Fatalf("%s: read pid file: %v", d.Name, err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		d.c.T.Fatalf("%s: pid file holds %q: %v", d.Name, raw, err)
	}
	return pid
}

// DaemonExits is the exit code of every daemon run of this device that has
// finished, oldest first.
func (d *Device) DaemonExits() []int {
	d.c.T.Helper()
	raw, err := os.ReadFile(filepath.Join(d.HostData, "daemon.exit"))
	if err != nil && !os.IsNotExist(err) {
		d.c.T.Fatalf("%s: read daemon.exit: %v", d.Name, err)
	}
	var codes []int
	for _, line := range strings.Fields(string(raw)) {
		code, err := strconv.Atoi(line)
		if err != nil {
			d.c.T.Fatalf("%s: daemon.exit holds %q: %v", d.Name, line, err)
		}
		codes = append(codes, code)
	}
	return codes
}

// WriteFile puts a file in the folder, from the host side of the bind mount.
//
// The content is written beside the path and renamed onto it, so a scan sees
// either the whole new version or the whole old one. A half-written file that
// is scanned and then finished is a version a peer may already be pulling
// when it changes, which is a failure of the test rather than of the product.
// The staged name starts with a dot, which no folder of this suite
// synchronizes, and it exists only until the rename.
func (d *Device) WriteFile(rel string, data []byte) {
	d.c.T.Helper()
	path := filepath.Join(d.HostRoot, rel)
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		d.c.T.Fatalf("%s: make parent of %s: %v", d.Name, rel, err)
	}
	staged, err := os.CreateTemp(dir, ".e2e-write-")
	if err != nil {
		d.c.T.Fatalf("%s: stage %s: %v", d.Name, rel, err)
	}
	_, writeErr := staged.Write(data)
	closeErr := staged.Close()
	if writeErr != nil || closeErr != nil {
		d.c.T.Fatalf("%s: stage %s: %v %v", d.Name, rel, writeErr, closeErr)
	}
	if err := os.Chmod(staged.Name(), 0o644); err != nil {
		d.c.T.Fatalf("%s: chmod staged %s: %v", d.Name, rel, err)
	}
	if err := os.Rename(staged.Name(), path); err != nil {
		d.c.T.Fatalf("%s: put %s in place: %v", d.Name, rel, err)
	}
}

// WriteFileAs writes a file from inside the container, as the user the
// container runs as, with the mode given. It is how a test about ownership
// writes: a file the host created would carry the host's uid whatever the
// daemon does.
func (d *Device) WriteFileAs(rel string, data []byte, mode fs.FileMode) {
	d.c.T.Helper()
	path := containerRoot + "/" + rel
	script := fmt.Sprintf("mkdir -p $(dirname %s) && printf %%s %s | base64 -d > %s && chmod %o %s",
		path, base64.StdEncoding.EncodeToString(data), path, mode.Perm(), path)
	res := d.c.d.exec(d.Ctr, "", "sh", "-c", script)
	if res.code != 0 {
		d.c.T.Fatalf("%s: write %s inside the container: exit %d: %s",
			d.Name, rel, res.code, strings.TrimSpace(res.stderr))
	}
}

// ReadFile is the content of a path of the folder, read from the host side of
// the bind mount.
func (d *Device) ReadFile(rel string) []byte {
	d.c.T.Helper()
	data, err := os.ReadFile(filepath.Join(d.HostRoot, rel))
	if err != nil {
		d.c.T.Fatalf("%s: read %s: %v", d.Name, rel, err)
	}
	return data
}

// Rename moves a path inside the folder.
func (d *Device) Rename(from, to string) {
	d.c.T.Helper()
	target := filepath.Join(d.HostRoot, to)
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		d.c.T.Fatalf("%s: make parent of %s: %v", d.Name, to, err)
	}
	if err := os.Rename(filepath.Join(d.HostRoot, from), target); err != nil {
		d.c.T.Fatalf("%s: rename %s to %s: %v", d.Name, from, to, err)
	}
}

// Remove deletes a path of the folder.
func (d *Device) Remove(rel string) {
	d.c.T.Helper()
	if err := os.RemoveAll(filepath.Join(d.HostRoot, rel)); err != nil {
		d.c.T.Fatalf("%s: remove %s: %v", d.Name, rel, err)
	}
}

// Touch sets the modification time of a path, which is how this suite makes
// clocks disagree: the daemons share the host's clock, so a device that
// believes a file was written in another decade is the closest a container
// gets to a skewed clock. It runs inside the container, as the daemon's own
// view of the file.
func (d *Device) Touch(rel string, at time.Time) {
	d.c.T.Helper()
	res := d.c.d.exec(d.Ctr, "", "touch", "-h", "-d", fmt.Sprintf("@%d", at.Unix()), containerRoot+"/"+rel)
	if res.code != 0 {
		d.c.T.Fatalf("%s: touch %s: exit %d: %s", d.Name, rel, res.code, strings.TrimSpace(res.stderr))
	}
}

// ModTime is when the device believes a path was last written.
func (d *Device) ModTime(rel string) time.Time {
	d.c.T.Helper()
	info, err := os.Lstat(filepath.Join(d.HostRoot, rel))
	if err != nil {
		d.c.T.Fatalf("%s: stat %s: %v", d.Name, rel, err)
	}
	return info.ModTime()
}

// Tree describes what the device holds in its folder.
func (d *Device) Tree() map[string]tree.Entry {
	d.c.T.Helper()
	entries, err := tree.Read(d.HostRoot)
	if err != nil {
		d.c.T.Fatalf("%s: read tree: %v", d.Name, err)
	}
	return entries
}

// PartFiles lists the partial downloads sitting in the state directory. A
// converged folder has none. The listing is made inside the container rather
// than on the host, because the daemon creates its state directory for itself
// alone and a device running as another user would refuse the host the look.
func (d *Device) PartFiles() []string {
	d.c.T.Helper()
	res := d.c.d.exec(d.Ctr, "", "sh", "-c",
		"find "+containerRoot+"/.syncthis -type f -name '*.part' 2>/dev/null || true")
	var parts []string
	for _, line := range strings.Fields(res.stdout) {
		parts = append(parts, strings.TrimPrefix(line, containerRoot+"/.syncthis/"))
	}
	return parts
}

// Logs is everything the daemon has logged in this container so far.
func (d *Device) Logs() string {
	d.c.T.Helper()
	raw, err := os.ReadFile(filepath.Join(d.HostData, "daemon.log"))
	if err != nil && !os.IsNotExist(err) {
		d.c.T.Fatalf("%s: read daemon.log: %v", d.Name, err)
	}
	return string(raw)
}

// slogLine is one JSON log line of the daemon. Changes and ChunksDone are the
// two attributes tests wait on, so they are decoded here rather than through a
// map of everything the daemon logs.
type slogLine struct {
	Time       string `json:"time"`
	Level      string `json:"level"`
	Msg        string `json:"msg"`
	Changes    int    `json:"changes"`
	ChunksDone int    `json:"chunks_done"`
	Raw        string `json:"-"`
}

// LogLines returns the log lines at one level, or every parsable line when
// level is empty.
func (d *Device) LogLines(level string) []slogLine {
	d.c.T.Helper()
	var lines []slogLine
	for _, raw := range strings.Split(d.Logs(), "\n") {
		if strings.TrimSpace(raw) == "" {
			continue
		}
		var line slogLine
		if err := json.Unmarshal([]byte(raw), &line); err != nil {
			// A panic or a runtime message is not a log line; it is
			// still evidence, so it is reported as one.
			line = slogLine{Level: "unparsed", Msg: raw}
		}
		line.Raw = raw
		if level == "" || strings.EqualFold(line.Level, level) {
			lines = append(lines, line)
		}
	}
	return lines
}

// LogSize is how much the daemon has logged so far. A wait that starts from
// one of these positions looks only at what happened after a change.
func (d *Device) LogSize() int { return len(d.Logs()) }

// WaitScanRecorded waits until the daemon has finished a scan that found
// something, after the given position in the log. It is how a test knows a
// local write is in the database rather than only on disk, which matters
// before a partition is healed: a version nobody recorded cannot be defended.
func (d *Device) WaitScanRecorded(from int) {
	d.c.T.Helper()
	waitFor(d.c.T, d.Name+" recorded a local change", convergeTimeout, func() bool {
		log := d.Logs()
		if len(log) <= from {
			return false
		}
		for _, raw := range strings.Split(log[from:], "\n") {
			var line slogLine
			if err := json.Unmarshal([]byte(raw), &line); err != nil {
				continue
			}
			if line.Msg == "scan finished" && line.Changes > 0 {
				return true
			}
		}
		return false
	})
}

// SaveArtifacts writes what a failed run needs to be explained: the log, the
// three JSON reports, the container description and the network state of
// every device.
func (c *Cluster) SaveArtifacts(reason string) {
	base := envOr("SYNCTHIS_E2E_ARTIFACTS", filepath.Join("..", "..", "bin", "e2e-artifacts"))
	base = filepath.Join(base, strings.ReplaceAll(c.T.Name(), "/", "_"))
	c.T.Logf("saving artifacts (%s) to %s", reason, base)

	for _, dev := range c.Devices {
		dir := filepath.Join(base, dev.Name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			c.T.Logf("artifacts: make %s: %v", dir, err)
			continue
		}
		c.writeArtifact(dir, "daemon.log", dev.Logs())
		c.writeArtifact(dir, "daemon.exit", fmt.Sprint(dev.DaemonExits()))
		for _, report := range [][]string{
			{"status.json", "status"},
			{"transfers.json", "transfer", "list"},
			{"conflicts.json", "conflict", "list", "--all"},
		} {
			out, _ := dev.Run(append(report[1:], "--json")...)
			c.writeArtifact(dir, report[0], out)
		}
		c.writeArtifact(dir, "iptables.txt", dev.c.d.exec(dev.Ctr, rootUser, "iptables", "-S").stdout)
		c.writeArtifact(dir, "tc.txt", dev.c.d.exec(dev.Ctr, rootUser, "tc", "qdisc", "show").stdout)
		res, err := c.d.try("inspect", dev.Ctr)
		if err == nil {
			c.writeArtifact(dir, "inspect.json", res.stdout)
		}
	}
}

// writeArtifact saves one piece of evidence. A failure to write it is logged
// and not fatal: the test result is the thing being reported.
func (c *Cluster) writeArtifact(dir, name, content string) {
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		c.T.Logf("artifacts: write %s: %v", filepath.Join(dir, name), err)
	}
}

// hashOf is the content hash a tree entry would carry for these bytes, so a
// test can name a version by the same string the comparison uses.
func hashOf(data []byte) string { return blake3hash.New().SumBytes(data).String() }

// equalBytes reports whether two versions of a file hold the same content.
func equalBytes(a, b []byte) bool { return bytes.Equal(a, b) }

// conflictCopies lists the keep-both copies of a tree, sorted.
func conflictCopies(entries map[string]tree.Entry) []string {
	var copies []string
	for path := range entries {
		if strings.Contains(filepath.Base(path), conflict.Marker) {
			copies = append(copies, path)
		}
	}
	sort.Strings(copies)
	return copies
}

// payload builds n bytes that differ per seed, so a wrong version of a file
// is a different hash rather than a coincidence.
func payload(n int, seed byte) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = seed + byte(i%251)
	}
	return out
}
