//go:build !race

package e2e

// timeoutScale multiplies every waiting time in the end-to-end tests. A normal
// build needs no extra room.
const timeoutScale = 1

// zeroLossRounds is how many rounds of concurrent edits the no-silent-overwrite
// test runs. Every round waits for both nodes to converge, so the count is the
// runtime of that test.
const zeroLossRounds = 30
