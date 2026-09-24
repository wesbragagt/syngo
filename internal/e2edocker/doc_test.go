// Package e2edocker runs one daemon per container and checks that a cluster
// of devices converges while daemons are killed, links are cut and clocks
// disagree. It is opt-in: without SYNGO_E2E_DOCKER=1 every test skips, so
// `go test ./...` on a machine without docker costs a compile and nothing
// more.
//
// The image is built by `make e2e-docker-image`; `make e2e-docker` builds it
// and runs this package. Every container and network carries the label
// syngo.e2e, so `make e2e-docker-clean` removes what a killed run left
// behind.
//
// The package holds only _test.go files, so nothing can import it.
package e2edocker

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"testing"
	"time"
)

// Environment. The image and the docker binary are overridable so the same
// suite runs against a prebuilt image or podman.
var (
	image     = envOr("SYNGO_E2E_IMAGE", "syngo-e2e:dev")
	dockerBin = envOr("SYNGO_E2E_DOCKER_BIN", "docker")
	// keepArtifacts writes the per-device evidence even for a test that
	// passed.
	keepArtifacts = os.Getenv("SYNGO_E2E_KEEP") == "1"
)

// timeoutScale multiplies every waiting time, for a machine too loaded to
// keep up with the defaults. Anything unset, unreadable or below one means no
// stretching: a scale may only give the tests more room, never less.
var timeoutScale = envTimeoutScale()

// Waiting times. They are variables because the scale they are multiplied by
// is read from the environment.
var (
	convergeTimeout = 120 * time.Second * timeoutScale
	daemonTimeout   = 30 * time.Second * timeoutScale
	dockerTimeout   = 120 * time.Second * timeoutScale
	// settle is how long a converged cluster must stay converged before a
	// wait returns: a tree that is equal for one poll may be equal only
	// because the next change has not arrived yet.
	settle       = 3 * time.Second * timeoutScale
	pollInterval = 250 * time.Millisecond
)

// Cluster defaults. The chunk size is small so a kill lands mid-file, and the
// intervals are short so a cycle happens within a test.
const (
	folderID = "docs"
	// extraID is a second folder every device keeps in sendreceive mode.
	// It carries markers: a file that arrives in it proves the device ran
	// a sync cycle against the peer that wrote it, which is how a test
	// says something did *not* cross without sleeping and hoping.
	extraID     = "extra"
	chunkSize   = 64 << 10
	rescanEvery = 2 * time.Second
	syncEvery   = time.Second
	peerPort    = 22000
	// containerRoot is where the synchronized folder is mounted.
	containerRoot = "/docs"
	// containerData is where the identity, the database and the log live.
	containerData = "/data"
	// containerExtra is where the second folder is mounted.
	containerExtra = "/extra"
)

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envTimeoutScale() time.Duration {
	scale, err := strconv.Atoi(os.Getenv("SYNGO_E2E_TIMEOUT_SCALE"))
	if err != nil || scale < 1 {
		return 1
	}
	return time.Duration(scale)
}

// TestMain gates the whole package. The gate is checked before docker is, so
// a machine without docker never runs a command.
func TestMain(m *testing.M) {
	if os.Getenv("SYNGO_E2E_DOCKER") != "1" {
		fmt.Println("skipping container end-to-end tests: set SYNGO_E2E_DOCKER=1 (see make e2e-docker)")
		os.Exit(0)
	}
	if err := checkDocker(); err != nil {
		fmt.Fprintf(os.Stderr, "syngo container tests need docker: %v\n", err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

// checkDocker reports whether the docker CLI answers and the image is there.
// A missing image is a hard failure rather than a skip: the gate was set on
// purpose, and silently skipping would report a green run that tested nothing.
func checkDocker() error {
	ctx, cancel := context.WithTimeout(context.Background(), dockerTimeout)
	defer cancel()

	out, err := exec.CommandContext(ctx, dockerBin, "version", "--format", "{{.Server.Version}}").CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s version: %v: %s", dockerBin, err, out)
	}
	if out, err := exec.CommandContext(ctx, dockerBin, "image", "inspect", image).CombinedOutput(); err != nil {
		return fmt.Errorf("image %s is missing, build it with `make e2e-docker-image`: %v: %s", image, err, out)
	}
	return nil
}
