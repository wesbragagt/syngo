package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/wesbragagt/syngo/internal/cli"
)

// TestMainDelegatesToCLI checks the entry point's only job: running the CLI
// and reporting its exit code.
func TestMainDelegatesToCLI(t *testing.T) {
	var stdout, stderr bytes.Buffer

	code := cli.Execute([]string{"version"}, strings.NewReader(""), &stdout, &stderr)

	if code != cli.ExitOK {
		t.Fatalf("Execute(version) = %d, want %d (stderr: %s)", code, cli.ExitOK, stderr.String())
	}
	if !strings.HasPrefix(stdout.String(), "syngo ") {
		t.Fatalf("stdout = %q, want it to start with %q", stdout.String(), "syngo ")
	}
}
