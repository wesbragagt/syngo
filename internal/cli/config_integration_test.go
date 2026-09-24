package cli_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wesbragagt/syngo/internal/cli"
)

// configReport is what `config show --json` prints.
type configReport struct {
	Listen        string `json:"listen"`
	LogLevel      string `json:"log_level"`
	UploadLimit   int64  `json:"upload_limit"`
	DownloadLimit int64  `json:"download_limit"`
}

// showConfig runs `config show --json` and decodes it.
func showConfig(t *testing.T, e env) configReport {
	t.Helper()
	code, stdout, stderr := e.run("", "config", "show", "--json")
	if code != cli.ExitOK {
		t.Fatalf("config show exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	var got configReport
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("config show output is not json: %v\n%s", err, stdout)
	}
	return got
}

func TestConfigSet_UploadLimitIsWrittenAndShown(t *testing.T) {
	e := newEnv(t)
	e.init(t)

	if code, _, stderr := e.run("", "config", "set", "--upload-limit", "4MiB"); code != cli.ExitOK {
		t.Fatalf("config set exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	if got := showConfig(t, e); got.UploadLimit != 4<<20 || got.DownloadLimit != 0 {
		t.Fatalf("limits = %d/%d, want 4194304/0", got.UploadLimit, got.DownloadLimit)
	}

	// The settings port carries no device name, so writing through it must
	// not drop the one init wrote.
	raw, err := os.ReadFile(filepath.Join(e.dataDir, "config.json"))
	if err != nil {
		t.Fatalf("read config.json: %v", err)
	}
	var file map[string]any
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatalf("config.json is not json: %v\n%s", err, raw)
	}
	if file["device_name"] != "testdev" {
		t.Fatalf("device_name = %v, want testdev", file["device_name"])
	}
}

func TestConfigSet_TextFormHumanizesTheRates(t *testing.T) {
	e := newEnv(t)
	e.init(t)

	code, stdout, stderr := e.run("", "config", "set", "--upload-limit", "4MiB")
	if code != cli.ExitOK {
		t.Fatalf("config set exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	for _, want := range []string{"upload_limit = 4 MiB/s", "download_limit = unlimited"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("output = %q, want a line %q", stdout, want)
		}
	}
}

func TestConfigSet_BadRateExit2(t *testing.T) {
	e := newEnv(t)
	e.init(t)
	for _, bad := range []string{"nonsense", "-1", "4 MiB"} {
		if code, _, _ := e.run("", "config", "set", "--download-limit", bad); code != cli.ExitUsage {
			t.Fatalf("config set --download-limit %q exit = %d, want %d", bad, code, cli.ExitUsage)
		}
	}
	if got := showConfig(t, e); got.DownloadLimit != 0 {
		t.Fatalf("download_limit = %d after refused values, want it unchanged", got.DownloadLimit)
	}
}

func TestConfigSet_NoFlagsExit2(t *testing.T) {
	e := newEnv(t)
	e.init(t)
	if code, _, _ := e.run("", "config", "set"); code != cli.ExitUsage {
		t.Fatalf("config set exit = %d, want %d", code, cli.ExitUsage)
	}
}

func TestConfigSet_BadListenExit2(t *testing.T) {
	e := newEnv(t)
	e.init(t)
	if code, _, _ := e.run("", "config", "set", "--listen", "nonsense"); code != cli.ExitUsage {
		t.Fatalf("config set --listen nonsense exit = %d, want %d", code, cli.ExitUsage)
	}
	if got := showConfig(t, e); got.Listen != "127.0.0.1:22000" {
		t.Fatalf("listen = %q, want it unchanged", got.Listen)
	}
}

func TestDaemonRun_BadLimitExit2(t *testing.T) {
	e := newEnv(t)
	setupFolder(t, e)
	code, _, _ := e.run("", "daemon", "run", "--listen", "127.0.0.1:0", "--download-limit", "nonsense")
	if code != cli.ExitUsage {
		t.Fatalf("daemon run --download-limit nonsense exit = %d, want %d", code, cli.ExitUsage)
	}
}

// TestConfigSet_WhileDaemonRunsIsReflectedInStatus is the runtime half of the
// feature: the limit reaches the daemon through the reload the command asks
// for, without restarting it.
func TestConfigSet_WhileDaemonRunsIsReflectedInStatus(t *testing.T) {
	e := newEnv(t)
	setupFolder(t, e)
	startDaemon(t, e)
	waitForDaemon(t, e)

	if code, _, stderr := e.run("", "config", "set", "--download-limit", "1MiB"); code != cli.ExitOK {
		t.Fatalf("config set exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	waitForStatus(t, e, "the daemon to report the new limit", func(r statusReport) bool {
		return r.Daemon.DownloadLimit == 1<<20
	})
}

// TestDaemonRun_FlagPinsLimitAgainstReload pins the decision that a rate given
// on the command line belongs to that run: config.json may move under it, and
// the run keeps what it was started with.
func TestDaemonRun_FlagPinsLimitAgainstReload(t *testing.T) {
	e := newEnv(t)
	setupFolder(t, e)
	startDaemon(t, e, "--upload-limit", "2MiB")
	waitForDaemon(t, e)
	waitForStatus(t, e, "the pinned limit", func(r statusReport) bool {
		return r.Daemon.UploadLimit == 2<<20
	})

	if code, _, stderr := e.run("", "config", "set", "--upload-limit", "8MiB"); code != cli.ExitOK {
		t.Fatalf("config set exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	if got := showConfig(t, e); got.UploadLimit != 8<<20 {
		t.Fatalf("config.json upload_limit = %d, want 8388608", got.UploadLimit)
	}
	if got := status(t, e); got.Daemon.UploadLimit != 2<<20 {
		t.Fatalf("daemon.upload_limit = %d, want the pinned 2097152", got.Daemon.UploadLimit)
	}
}
