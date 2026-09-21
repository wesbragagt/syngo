//go:build !race

package e2e

// timeoutScale multiplies every waiting time in the end-to-end tests. A normal
// build needs no extra room.
const timeoutScale = 1
