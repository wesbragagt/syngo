//go:build !race

package e2e

// buildTimeoutScale is how much room this build needs on top of the times the
// tests ask for. A normal build needs none.
const buildTimeoutScale = 1

// zeroLossRounds is how many rounds of concurrent edits the no-silent-overwrite
// test runs. Every round waits for both nodes to converge, so the count is the
// runtime of that test.
const zeroLossRounds = 30
