package cli_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/wesbragagt/syncthis/internal/cli"
)

// confirming is one command that asks before it acts, together with a way to
// build the environment it needs and the arguments that reach the question.
type confirming struct {
	name string
	// setup returns the environment and the arguments of the command.
	setup func(t *testing.T) (env, []string)
}

// confirmingCommands are every command that asks the user before it acts.
// Adding one without adding it here leaves the flag handling untested.
func confirmingCommands() []confirming {
	return []confirming{
		{
			name: "folder remove",
			setup: func(t *testing.T) (env, []string) {
				e := newEnv(t)
				setupFolder(t, e)
				return e, []string{"folder", "remove", "docs"}
			},
		},
		{
			name: "peer remove",
			setup: func(t *testing.T) (env, []string) {
				e := newEnv(t)
				e.init(t)
				return e, []string{"peer", "remove", strings.Repeat("A", 52)}
			},
		},
		{
			name: "conflict resolve",
			setup: func(t *testing.T) (env, []string) {
				return withConflict(t), []string{"conflict", "resolve", "c1", "--keep", "remote"}
			},
		},
		{
			name: "peer add",
			setup: func(t *testing.T) (env, []string) {
				_, addr := startPeerDaemon(t, "remote")
				e := newEnv(t)
				e.init(t)
				return e, []string{"peer", "add", addr}
			},
		},
	}
}

func TestConfirm_NoInputWithoutYesExits9(t *testing.T) {
	for _, tc := range confirmingCommands() {
		t.Run(tc.name, func(t *testing.T) {
			e, args := tc.setup(t)
			code, stdout, stderr := e.run("", append(args, "--no-input")...)
			if code != cli.ExitConfirmationRequired {
				t.Fatalf("exit = %d, want %d (stdout %q, stderr %q)",
					code, cli.ExitConfirmationRequired, stdout, stderr)
			}
			if !strings.Contains(stderr, "--yes") {
				t.Fatalf("stderr = %q, want it to name --yes", stderr)
			}
		})
	}
}

func TestConfirm_JSONImpliesNoInputAndReportsTheCodeOnStdout(t *testing.T) {
	for _, tc := range confirmingCommands() {
		t.Run(tc.name, func(t *testing.T) {
			e, args := tc.setup(t)
			code, stdout, _ := e.run("", append(args, "--json")...)
			if code != cli.ExitConfirmationRequired {
				t.Fatalf("exit = %d, want %d (stdout %q)", code, cli.ExitConfirmationRequired, stdout)
			}

			var failure struct {
				Error struct {
					Code    int    `json:"code"`
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.Unmarshal([]byte(stdout), &failure); err != nil {
				t.Fatalf("stdout is not one json document: %v\n%s", err, stdout)
			}
			if failure.Error.Code != cli.ExitConfirmationRequired || failure.Error.Message == "" {
				t.Fatalf("error object = %+v", failure.Error)
			}
		})
	}
}

func TestConfirm_YesSkipsTheQuestion(t *testing.T) {
	for _, tc := range confirmingCommands() {
		t.Run(tc.name, func(t *testing.T) {
			e, args := tc.setup(t)
			code, stdout, stderr := e.run("", append(args, "--no-input", "--yes")...)
			if code == cli.ExitConfirmationRequired {
				t.Fatalf("exit = %d with --yes (stdout %q, stderr %q)", code, stdout, stderr)
			}
			if strings.Contains(stdout, "[y/N]") {
				t.Fatalf("stdout = %q, want no question with --yes", stdout)
			}
		})
	}
}

func TestConfirm_AnswerOnStdinDecides(t *testing.T) {
	for _, tc := range confirmingCommands() {
		t.Run(tc.name, func(t *testing.T) {
			e, args := tc.setup(t)
			code, stdout, _ := e.run("n\n", args...)
			if code != cli.ExitConfirmationRequired {
				t.Fatalf("answering no exited %d, want %d", code, cli.ExitConfirmationRequired)
			}
			if !strings.Contains(stdout, "[y/N]") {
				t.Fatalf("stdout = %q, want the question", stdout)
			}

			e, args = tc.setup(t)
			code, stdout, stderr := e.run("y\n", args...)
			if code == cli.ExitConfirmationRequired {
				t.Fatalf("answering yes exited %d (stdout %q, stderr %q)", code, stdout, stderr)
			}
		})
	}
}

func TestConfirm_EmptyStdinIsNotAnAnswer(t *testing.T) {
	for _, tc := range confirmingCommands() {
		t.Run(tc.name, func(t *testing.T) {
			e, args := tc.setup(t)
			if code, stdout, _ := e.run("", args...); code != cli.ExitConfirmationRequired {
				t.Fatalf("exit = %d with nothing to read, want %d (stdout %q)",
					code, cli.ExitConfirmationRequired, stdout)
			}
		})
	}
}
