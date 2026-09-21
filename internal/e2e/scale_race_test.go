//go:build race

package e2e

// timeoutScale multiplies every waiting time in the end-to-end tests. The race
// detector slows every memory access down, and `go test -race ./...` tests
// other packages at the same time, so a sync cycle needs several times longer
// than it does in a normal build.
const timeoutScale = 4
