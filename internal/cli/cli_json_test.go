package cli_test

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/wesbragagt/syncthis/internal/cli"
	"github.com/wesbragagt/syncthis/internal/domain/transfer"
)

// jsonCase is one command run with --json, together with the environment it
// needs. wantCode is the exit code the run must produce.
type jsonCase struct {
	name     string
	wantCode int
	// setup returns the environment and the arguments, without --json.
	setup func(t *testing.T) (env, []string)
}

// jsonCases covers every command that prints machine-readable output, once on
// a path that succeeds and once on a path that fails. `completion` is left
// out: it emits a shell script by definition, and `daemon run` has a case of
// its own because it does not return until it is stopped.
func jsonCases() []jsonCase {
	initialized := func(t *testing.T) env {
		e := newEnv(t)
		e.init(t)
		return e
	}
	withDocs := func(t *testing.T) env {
		e := newEnv(t)
		setupFolder(t, e)
		return e
	}

	return []jsonCase{
		{"version", cli.ExitOK, func(t *testing.T) (env, []string) {
			return newEnv(t), []string{"version"}
		}},
		{"init", cli.ExitOK, func(t *testing.T) (env, []string) {
			return newEnv(t), []string{"init", "--name", "testdev"}
		}},
		{"status", cli.ExitOK, func(t *testing.T) (env, []string) {
			return initialized(t), []string{"status"}
		}},
		{"status before init", cli.ExitNotInitialized, func(t *testing.T) (env, []string) {
			return newEnv(t), []string{"status"}
		}},
		{"folder add", cli.ExitOK, func(t *testing.T) (env, []string) {
			e := initialized(t)
			return e, []string{"folder", "add", e.folderPath, "--id", "docs"}
		}},
		{"folder add with a bad id", cli.ExitUsage, func(t *testing.T) (env, []string) {
			e := initialized(t)
			return e, []string{"folder", "add", e.folderPath, "--id", "Docs!"}
		}},
		{"folder list", cli.ExitOK, func(t *testing.T) (env, []string) {
			return withDocs(t), []string{"folder", "list"}
		}},
		{"folder list when empty", cli.ExitOK, func(t *testing.T) (env, []string) {
			return initialized(t), []string{"folder", "list"}
		}},
		{"folder set", cli.ExitOK, func(t *testing.T) (env, []string) {
			return withDocs(t), []string{"folder", "set", "docs", "--mode", "sendonly"}
		}},
		{"folder set on an unknown folder", cli.ExitNotFound, func(t *testing.T) (env, []string) {
			return initialized(t), []string{"folder", "set", "gone", "--mode", "sendonly"}
		}},
		{"folder remove", cli.ExitOK, func(t *testing.T) (env, []string) {
			return withDocs(t), []string{"folder", "remove", "docs", "--yes"}
		}},
		{"folder remove without a confirmation", cli.ExitConfirmationRequired, func(t *testing.T) (env, []string) {
			return withDocs(t), []string{"folder", "remove", "docs"}
		}},
		{"folder without a subcommand", cli.ExitUsage, func(t *testing.T) (env, []string) {
			return initialized(t), []string{"folder"}
		}},
		{"scan", cli.ExitOK, func(t *testing.T) (env, []string) {
			return withDocs(t), []string{"scan", "docs"}
		}},
		{"scan of an unknown folder", cli.ExitNotFound, func(t *testing.T) (env, []string) {
			return initialized(t), []string{"scan", "gone"}
		}},
		{"rescan", cli.ExitOK, func(t *testing.T) (env, []string) {
			return withDocs(t), []string{"rescan"}
		}},
		{"rescan of an unknown folder", cli.ExitNotFound, func(t *testing.T) (env, []string) {
			return initialized(t), []string{"rescan", "gone"}
		}},
		{"repair", cli.ExitOK, func(t *testing.T) (env, []string) {
			return withDocs(t), []string{"repair", "docs"}
		}},
		{"repair of an unknown folder", cli.ExitNotFound, func(t *testing.T) (env, []string) {
			return initialized(t), []string{"repair", "gone"}
		}},
		{"peer list", cli.ExitOK, func(t *testing.T) (env, []string) {
			return initialized(t), []string{"peer", "list"}
		}},
		{"peer add of an unreachable address", cli.ExitPeerUnreachable, func(t *testing.T) (env, []string) {
			return initialized(t), []string{"peer", "add", closedPort(t), "--yes"}
		}},
		{"peer remove of an unknown device", cli.ExitNotFound, func(t *testing.T) (env, []string) {
			return initialized(t), []string{"peer", "remove", strings.Repeat("A", 52), "--yes"}
		}},
		{"peer test of an unreachable address", cli.ExitPeerUnreachable, func(t *testing.T) (env, []string) {
			return initialized(t), []string{"peer", "test", closedPort(t)}
		}},
		{"transfer list", cli.ExitOK, func(t *testing.T) (env, []string) {
			e := withDocs(t)
			seedTransfer(t, e, transfer.ID("a1"), "big.bin", transfer.StateActive, 4, 2)
			return e, []string{"transfer", "list"}
		}},
		{"transfer list when empty", cli.ExitOK, func(t *testing.T) (env, []string) {
			return withDocs(t), []string{"transfer", "list"}
		}},
		{"transfer show", cli.ExitOK, func(t *testing.T) (env, []string) {
			e := withDocs(t)
			seedTransfer(t, e, transfer.ID("a1"), "big.bin", transfer.StateActive, 4, 2)
			return e, []string{"transfer", "show", "a1"}
		}},
		{"transfer show of an unknown id", cli.ExitNotFound, func(t *testing.T) (env, []string) {
			return withDocs(t), []string{"transfer", "show", "nope"}
		}},
		{"conflict list", cli.ExitOK, func(t *testing.T) (env, []string) {
			return withConflict(t), []string{"conflict", "list"}
		}},
		{"conflict list when empty", cli.ExitOK, func(t *testing.T) (env, []string) {
			return withDocs(t), []string{"conflict", "list"}
		}},
		{"conflict list of an unknown folder", cli.ExitNotFound, func(t *testing.T) (env, []string) {
			return withDocs(t), []string{"conflict", "list", "gone"}
		}},
		{"conflict show", cli.ExitOK, func(t *testing.T) (env, []string) {
			return withConflict(t), []string{"conflict", "show", "c1"}
		}},
		{"conflict show of an unknown id", cli.ExitNotFound, func(t *testing.T) (env, []string) {
			return withConflict(t), []string{"conflict", "show", "nope"}
		}},
		{"conflict resolve", cli.ExitOK, func(t *testing.T) (env, []string) {
			return withConflict(t), []string{"conflict", "resolve", "c1", "--keep", "remote", "--yes"}
		}},
		{"conflict resolve with an unknown version", cli.ExitUsage, func(t *testing.T) (env, []string) {
			return withConflict(t), []string{"conflict", "resolve", "c1", "--keep", "mine", "--yes"}
		}},
		{"conflict export", cli.ExitOK, func(t *testing.T) (env, []string) {
			return withConflict(t), []string{"conflict", "export", "c1", t.TempDir()}
		}},
		{"conflict export of an unknown id", cli.ExitNotFound, func(t *testing.T) (env, []string) {
			return withConflict(t), []string{"conflict", "export", "nope", t.TempDir()}
		}},
		{"conflict without a subcommand", cli.ExitUsage, func(t *testing.T) (env, []string) {
			return initialized(t), []string{"conflict"}
		}},
		{"sync-plan without peers", cli.ExitOK, func(t *testing.T) (env, []string) {
			return withDocs(t), []string{"sync-plan", "docs"}
		}},
		{"sync-plan with a bad peer id", cli.ExitUsage, func(t *testing.T) (env, []string) {
			return withDocs(t), []string{"sync-plan", "docs", "--peer", "nope"}
		}},
		{"pause", cli.ExitOK, func(t *testing.T) (env, []string) {
			return withDocs(t), []string{"pause", "docs"}
		}},
		{"resume", cli.ExitOK, func(t *testing.T) (env, []string) {
			return withDocs(t), []string{"resume", "docs"}
		}},
		{"pause of an unknown folder", cli.ExitNotFound, func(t *testing.T) (env, []string) {
			return initialized(t), []string{"pause", "gone"}
		}},
		{"an unknown command", cli.ExitUsage, func(t *testing.T) (env, []string) {
			return initialized(t), []string{"nonsense"}
		}},
		{"an unknown flag", cli.ExitUsage, func(t *testing.T) (env, []string) {
			return initialized(t), []string{"status", "--nonsense"}
		}},
		{"an extra positional argument", cli.ExitUsage, func(t *testing.T) (env, []string) {
			return initialized(t), []string{"status", "extra"}
		}},
	}
}

func TestJSON_EveryCommandPrintsOneDocumentAndNothingElse(t *testing.T) {
	for _, tc := range jsonCases() {
		t.Run(tc.name, func(t *testing.T) {
			e, args := tc.setup(t)
			code, stdout, stderr := e.run("", append(args, "--json")...)
			if code != tc.wantCode {
				t.Fatalf("exit = %d, want %d (stdout %q, stderr %q)", code, tc.wantCode, stdout, stderr)
			}
			assertOneJSONDocument(t, stdout)
			if tc.wantCode != cli.ExitOK {
				assertErrorObject(t, stdout, tc.wantCode)
			}
		})
	}
}

func TestJSON_DaemonRunAnnouncesItselfAsOneDocument(t *testing.T) {
	e := newEnv(t)
	setupFolder(t, e)
	p := startDaemon(t, e, "--json")
	waitForDaemon(t, e)
	p.stop()

	stdout := p.stdout.String()
	assertOneJSONDocument(t, stdout)

	var started struct {
		Running bool   `json:"running"`
		PID     int    `json:"pid"`
		DataDir string `json:"data_dir"`
	}
	if err := json.Unmarshal([]byte(stdout), &started); err != nil {
		t.Fatalf("stdout is not json: %v\n%s", err, stdout)
	}
	if !started.Running || started.PID == 0 || started.DataDir != e.dataDir {
		t.Fatalf("daemon announcement = %+v", started)
	}
}

func TestJSON_DaemonLogFormatIsOneObjectPerLineOnStderr(t *testing.T) {
	e := newEnv(t)
	setupFolder(t, e)
	p := startDaemon(t, e, "--log-format", "json")
	waitForDaemon(t, e)
	p.stop()

	lines := strings.Split(strings.TrimSpace(p.stderr.String()), "\n")
	if len(lines) == 0 || lines[0] == "" {
		t.Fatal("the daemon logged nothing to stderr")
	}
	for _, line := range lines {
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("log line is not json: %v\n%s", err, line)
		}
		if record["time"] == nil || record["level"] == nil || record["msg"] == nil {
			t.Fatalf("log line = %v, want time, level and msg", record)
		}
	}
}

// assertOneJSONDocument checks that out holds exactly one JSON value, with no
// escape sequences and nothing after it.
func assertOneJSONDocument(t *testing.T, out string) {
	t.Helper()
	if strings.ContainsRune(out, 0x1b) {
		t.Fatalf("stdout holds an escape byte: %q", out)
	}

	dec := json.NewDecoder(strings.NewReader(out))
	var doc any
	if err := dec.Decode(&doc); err != nil {
		t.Fatalf("stdout is not json: %v\n%s", err, out)
	}
	rest, err := io.ReadAll(dec.Buffered())
	if err != nil {
		t.Fatalf("read the rest of stdout: %v", err)
	}
	if trailing := bytes.TrimSpace(rest); len(trailing) > 0 {
		t.Fatalf("stdout holds more than one document; after the first: %q", trailing)
	}
}

// assertErrorObject checks that a failing command reported the exit code as a
// JSON error object.
func assertErrorObject(t *testing.T, out string, code int) {
	t.Helper()
	var failure struct {
		Error struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(out), &failure); err != nil {
		t.Fatalf("stdout is not an error object: %v\n%s", err, out)
	}
	if failure.Error.Code != code {
		t.Fatalf("error code = %d, want %d\n%s", failure.Error.Code, code, out)
	}
	if failure.Error.Message == "" {
		t.Fatalf("error message is empty\n%s", out)
	}
}
