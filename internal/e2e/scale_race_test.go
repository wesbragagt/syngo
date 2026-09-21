//go:build race

package e2e

// buildTimeoutScale is how much room this build needs on top of the times the
// tests ask for. The race detector slows every memory access down, so a sync
// cycle needs several times longer than it does in a normal build.
const buildTimeoutScale = 4

// zeroLossRounds is how many rounds of concurrent edits the no-silent-overwrite
// test runs. Every round waits for both nodes to converge, so the count is the
// runtime of that test.
const zeroLossRounds = 10
