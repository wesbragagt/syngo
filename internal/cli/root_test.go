package cli_test

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/wesbragagt/syngo/internal/cli"
)

// run executes the CLI and returns the exit code with both output streams.
func run(args ...string) (int, string, string) {
	var stdout, stderr bytes.Buffer
	code := cli.Execute(args, strings.NewReader(""), &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func TestExecute_NoArgsPrintsHelpExit0(t *testing.T) {
	code, stdout, stderr := run()

	if code != cli.ExitOK {
		t.Fatalf("exit = %d, want %d (stderr: %s)", code, cli.ExitOK, stderr)
	}
	for _, want := range []string{"Usage:", "syngo", "Available Commands:"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("stdout does not contain %q:\n%s", want, stdout)
		}
	}
}

func TestExecute_UnknownCommandExit2(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{"unknown command", []string{"bogus"}},
		{"unknown flag", []string{"--bogus"}},
		{"invalid log level", []string{"--log-level", "loud", "version"}},
		{"completion without shell", []string{"completion"}},
		{"completion with two shells", []string{"completion", "bash", "zsh"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, _, stderr := run(tt.args...)
			if code != cli.ExitUsage {
				t.Fatalf("exit = %d, want %d", code, cli.ExitUsage)
			}
			if !strings.HasPrefix(stderr, "syngo:") {
				t.Fatalf("stderr = %q, want it to start with %q", stderr, "syngo:")
			}
		})
	}
}

func TestExecute_JSONErrorGoesToStdout(t *testing.T) {
	code, stdout, stderr := run("--json", "bogus")

	if code != cli.ExitUsage {
		t.Fatalf("exit = %d, want %d", code, cli.ExitUsage)
	}
	if stderr != "" {
		t.Fatalf("stderr = %q, want empty in json mode", stderr)
	}
	var payload struct {
		Error struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(stdout), &payload); err != nil {
		t.Fatalf("stdout is not json: %v\n%s", err, stdout)
	}
	if payload.Error.Code != cli.ExitUsage || payload.Error.Message == "" {
		t.Fatalf("payload = %+v, want code %d and a message", payload.Error, cli.ExitUsage)
	}
}

func TestVersionJSON(t *testing.T) {
	code, stdout, stderr := run("version", "--json")

	if code != cli.ExitOK {
		t.Fatalf("exit = %d, want %d (stderr: %s)", code, cli.ExitOK, stderr)
	}
	var info struct {
		Version string `json:"version"`
		Commit  string `json:"commit"`
		Go      string `json:"go"`
	}
	if err := json.Unmarshal([]byte(stdout), &info); err != nil {
		t.Fatalf("stdout is not json: %v\n%s", err, stdout)
	}
	if info.Version != cli.Version || info.Commit != cli.Commit || !strings.HasPrefix(info.Go, "go") {
		t.Fatalf("info = %+v, want version %q commit %q and a go version", info, cli.Version, cli.Commit)
	}
}

func TestCompletionBash_Emits(t *testing.T) {
	tests := []struct {
		shell string
		want  string
	}{
		{"bash", "bash completion"},
		{"zsh", "#compdef syngo"},
		{"fish", "syngo"},
		{"powershell", "Register-ArgumentCompleter"},
	}
	for _, tt := range tests {
		t.Run(tt.shell, func(t *testing.T) {
			code, stdout, stderr := run("completion", tt.shell)
			if code != cli.ExitOK {
				t.Fatalf("exit = %d, want %d (stderr: %s)", code, cli.ExitOK, stderr)
			}
			if !strings.Contains(stdout, tt.want) {
				t.Fatalf("%s completion does not contain %q:\n%s", tt.shell, tt.want, stdout[:min(len(stdout), 200)])
			}
		})
	}
}

func TestPrinter_Table(t *testing.T) {
	var buf bytes.Buffer
	p := cli.Printer{W: &buf}

	if err := p.Table([]string{"ID", "PATH"}, [][]string{{"docs", "/home/u/docs"}}); err != nil {
		t.Fatalf("Table error: %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, "ID") || !strings.Contains(out, "/home/u/docs") {
		t.Fatalf("table output = %q", out)
	}
}

func TestPrinter_TableJSON(t *testing.T) {
	var buf bytes.Buffer
	p := cli.Printer{JSON: true, W: &buf}

	if err := p.Table([]string{"ID", "PATH"}, [][]string{{"docs", "/home/u/docs"}}); err != nil {
		t.Fatalf("Table error: %v", err)
	}

	var rows []map[string]string
	if err := json.Unmarshal(buf.Bytes(), &rows); err != nil {
		t.Fatalf("output is not json: %v\n%s", err, buf.String())
	}
	if len(rows) != 1 || rows[0]["id"] != "docs" || rows[0]["path"] != "/home/u/docs" {
		t.Fatalf("rows = %v", rows)
	}
}

func TestPrinterTable_EscapesControlCharacters(t *testing.T) {
	var out bytes.Buffer
	p := cli.Printer{W: &out}
	if err := p.Table([]string{"TYPE", "PATH"}, [][]string{
		{"create", "notes/plain.txt"},
		{"create", "notes/two\nlines.txt"},
	}); err != nil {
		t.Fatalf("Table error: %v", err)
	}

	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("table has %d lines, want 3:\n%s", len(lines), out.String())
	}
	if !strings.Contains(lines[1], "notes/plain.txt") {
		t.Fatalf("a name without control characters must print unchanged: %q", lines[1])
	}
	if !strings.Contains(lines[2], `"notes/two\nlines.txt"`) {
		t.Fatalf("a name with a newline must be escaped: %q", lines[2])
	}
}
