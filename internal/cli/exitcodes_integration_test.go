package cli_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wesbragagt/syncthis/internal/cli"
)

// exitCase is one command and the exit code the CLI contract promises for it.
type exitCase struct {
	name string
	code int
	// setup returns the environment and the arguments to run. A nil skip
	// reason means the case always applies.
	setup func(t *testing.T) (env, []string)
}

// exitCases covers every code in docs/CLI.md. ExitError (1) is not among them:
// it is what an unexpected failure produces, and there is no way to provoke
// one through the CLI without breaking the data directory on purpose.
func exitCases() []exitCase {
	return []exitCase{
		{"ExitOK", cli.ExitOK, func(t *testing.T) (env, []string) {
			e := newEnv(t)
			e.init(t)
			return e, []string{"status"}
		}},
		{"ExitUsage", cli.ExitUsage, func(t *testing.T) (env, []string) {
			e := newEnv(t)
			e.init(t)
			return e, []string{"folder", "add", e.folderPath}
		}},
		{"ExitNotInitialized", cli.ExitNotInitialized, func(t *testing.T) (env, []string) {
			return newEnv(t), []string{"folder", "list"}
		}},
		{"ExitNotFound", cli.ExitNotFound, func(t *testing.T) (env, []string) {
			e := newEnv(t)
			e.init(t)
			return e, []string{"scan", "gone"}
		}},
		{"ExitAlreadyExists", cli.ExitAlreadyExists, func(t *testing.T) (env, []string) {
			e := newEnv(t)
			setupFolder(t, e)
			return e, []string{"folder", "add", e.folderPath, "--id", "docs"}
		}},
		{"ExitDaemonUnavailable", cli.ExitDaemonUnavailable, func(t *testing.T) (env, []string) {
			e := newEnv(t)
			setupFolder(t, e)
			startDaemon(t, e)
			waitForDaemon(t, e)
			return e, []string{"daemon", "run", "--listen", "127.0.0.1:0"}
		}},
		{"ExitPeerUnreachable", cli.ExitPeerUnreachable, func(t *testing.T) (env, []string) {
			e := newEnv(t)
			e.init(t)
			return e, []string{"peer", "test", closedPort(t)}
		}},
		{"ExitIntegrity", cli.ExitIntegrity, func(t *testing.T) (env, []string) {
			if os.Geteuid() == 0 {
				t.Skip("root reads files whatever their permissions say")
			}
			e := newEnv(t)
			locked := filepath.Join(e.folderPath, "locked.txt")
			if err := os.WriteFile(locked, []byte("readable"), 0o644); err != nil {
				t.Fatalf("write locked.txt: %v", err)
			}
			setupFolder(t, e)
			if code, _, stderr := e.run("", "scan", "docs"); code != cli.ExitOK {
				t.Fatalf("scan exit = %d (stderr: %s)", code, stderr)
			}
			// The file changed and can no longer be read, so repair
			// can neither keep nor correct what it knows about it.
			if err := os.WriteFile(locked, []byte("changed content"), 0o644); err != nil {
				t.Fatalf("rewrite locked.txt: %v", err)
			}
			if err := os.Chtimes(locked, time.Time{}, time.Now().Add(time.Minute)); err != nil {
				t.Fatalf("chtimes locked.txt: %v", err)
			}
			if err := os.Chmod(locked, 0o000); err != nil {
				t.Fatalf("chmod locked.txt: %v", err)
			}
			return e, []string{"repair", "docs"}
		}},
		{"ExitConfirmationRequired", cli.ExitConfirmationRequired, func(t *testing.T) (env, []string) {
			e := newEnv(t)
			setupFolder(t, e)
			return e, []string{"folder", "remove", "docs", "--no-input"}
		}},
		{"ExitDryRun", cli.ExitDryRun, func(t *testing.T) (env, []string) {
			e := newEnv(t)
			if err := os.WriteFile(filepath.Join(e.folderPath, "a.txt"), []byte("alpha"), 0o644); err != nil {
				t.Fatalf("write a.txt: %v", err)
			}
			setupFolder(t, e)
			return e, []string{"scan", "docs", "--dry-run"}
		}},
	}
}

func TestExitCodes_Table(t *testing.T) {
	for _, tc := range exitCases() {
		t.Run(tc.name, func(t *testing.T) {
			e, args := tc.setup(t)
			code, stdout, stderr := e.run("", args...)
			if code != tc.code {
				t.Fatalf("exit = %d, want %d (stdout %q, stderr %q)", code, tc.code, stdout, stderr)
			}
		})
	}
}

// TestExitCodes_ExtraArgumentIsAUsageError covers the commands that take no
// positional argument at all: one handed to them is a mistake in what the user
// typed, which is exit 2 and not the generic failure code.
func TestExitCodes_ExtraArgumentIsAUsageError(t *testing.T) {
	cases := [][]string{
		{"status", "extra"},
		{"version", "extra"},
		{"folder", "list", "extra"},
		{"peer", "list", "extra"},
		{"init", "extra"},
		{"daemon", "run", "extra"},
		{"transfer", "list", "extra"},
		{"conflict", "list", "one", "two"},
		{"rescan", "one", "two"},
		{"repair", "one", "two"},
		{"pause", "one", "two"},
	}
	for _, args := range cases {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			e := newEnv(t)
			e.init(t)
			if code, stdout, stderr := e.run("", args...); code != cli.ExitUsage {
				t.Fatalf("exit = %d, want %d (stdout %q, stderr %q)", code, cli.ExitUsage, stdout, stderr)
			}
			code, stdout, stderr := e.run("", append(args, "--json")...)
			if code != cli.ExitUsage {
				t.Fatalf("with --json exit = %d, want %d (stdout %q, stderr %q)",
					code, cli.ExitUsage, stdout, stderr)
			}
			assertOneJSONDocument(t, stdout)
			assertErrorObject(t, stdout, cli.ExitUsage)
		})
	}
}
