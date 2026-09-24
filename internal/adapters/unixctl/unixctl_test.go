package unixctl_test

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wesbragagt/syngo/internal/adapters/unixctl"
	"github.com/wesbragagt/syngo/internal/app/ports"
)

// handlerFunc adapts a function to ports.ControlHandler.
type handlerFunc func(ctx context.Context, req ports.ControlRequest) ports.ControlResponse

// Handle calls the function.
func (f handlerFunc) Handle(ctx context.Context, req ports.ControlRequest) ports.ControlResponse {
	return f(ctx, req)
}

// serve starts a server on a temporary socket and stops it when the test ends.
func serve(t *testing.T, h ports.ControlHandler) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "control.sock")
	server, err := unixctl.Listen(path, nil)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := server.Serve(ctx, h); err != nil {
			t.Errorf("serve: %v", err)
		}
	}()
	t.Cleanup(func() {
		cancel()
		wg.Wait()
	})
	return path
}

// echoHandler answers every request with the request itself.
func echoHandler() ports.ControlHandler {
	return handlerFunc(func(_ context.Context, req ports.ControlRequest) ports.ControlResponse {
		switch req.Cmd {
		case "status":
			payload, err := json.Marshal(map[string]any{"folder_id": req.FolderID, "dry_run": req.DryRun})
			if err != nil {
				return ports.ControlResponse{OK: false, Error: err.Error(), Code: 1}
			}
			return ports.ControlResponse{OK: true, Result: payload}
		default:
			return ports.ControlResponse{OK: false, Error: "unknown command " + req.Cmd, Code: 2}
		}
	})
}

func TestRoundTrip_Status(t *testing.T) {
	path := serve(t, echoHandler())
	client := unixctl.NewClient(path)

	resp, err := client.Call(context.Background(), ports.ControlRequest{Cmd: "status", FolderID: "docs", DryRun: true})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if !resp.OK {
		t.Fatalf("ok = false: %s", resp.Error)
	}
	var payload map[string]any
	if err := json.Unmarshal(resp.Result, &payload); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if payload["folder_id"] != "docs" || payload["dry_run"] != true {
		t.Fatalf("result = %v, want the request fields back", payload)
	}
}

func TestRoundTrip_UnknownCommandIsNotOK(t *testing.T) {
	path := serve(t, echoHandler())

	resp, err := unixctl.NewClient(path).Call(context.Background(), ports.ControlRequest{Cmd: "frobnicate"})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if resp.OK || resp.Code != 2 || !strings.Contains(resp.Error, "frobnicate") {
		t.Fatalf("response = %+v, want ok:false with code 2", resp)
	}
}

func TestRoundTrip_ManyRequestsOnOneSocket(t *testing.T) {
	path := serve(t, echoHandler())
	client := unixctl.NewClient(path)

	for range 20 {
		if _, err := client.Call(context.Background(), ports.ControlRequest{Cmd: "status"}); err != nil {
			t.Fatalf("call: %v", err)
		}
	}
}

func TestClient_NoSocketReturnsDaemonUnavailable(t *testing.T) {
	client := unixctl.NewClient(filepath.Join(t.TempDir(), "control.sock"))

	_, err := client.Call(context.Background(), ports.ControlRequest{Cmd: "status"})
	if !errors.Is(err, ports.ErrDaemonUnavailable) {
		t.Fatalf("err = %v, want ErrDaemonUnavailable", err)
	}
}

func TestClient_StaleSocketReturnsDaemonUnavailable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "control.sock")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatalf("write stale socket: %v", err)
	}

	_, err := unixctl.NewClient(path).Call(context.Background(), ports.ControlRequest{Cmd: "status"})
	if !errors.Is(err, ports.ErrDaemonUnavailable) {
		t.Fatalf("err = %v, want ErrDaemonUnavailable", err)
	}
}

func TestServer_MalformedRequestReturnsError(t *testing.T) {
	path := serve(t, echoHandler())

	conn, err := net.Dial("unix", path)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()
	if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("set deadline: %v", err)
	}
	if _, err := conn.Write([]byte("not json\n")); err != nil {
		t.Fatalf("write: %v", err)
	}

	buf := make([]byte, 512)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var resp struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
		Code  int    `json:"code"`
	}
	if err := json.Unmarshal(buf[:n], &resp); err != nil {
		t.Fatalf("decode response %q: %v", buf[:n], err)
	}
	if resp.OK || !strings.Contains(resp.Error, "malformed") {
		t.Fatalf("response = %+v, want a malformed-request failure", resp)
	}
}

func TestServer_CloseRemovesTheSocket(t *testing.T) {
	path := filepath.Join(t.TempDir(), "control.sock")
	server, err := unixctl.Listen(path, nil)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("socket missing while serving: %v", err)
	}

	if err := server.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stat after close = %v, want the socket to be gone", err)
	}
	if err := server.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
}

func TestServer_ListenFailsWhenSocketIsTaken(t *testing.T) {
	path := serve(t, echoHandler())

	if _, err := unixctl.Listen(path, nil); err == nil {
		t.Fatalf("second Listen succeeded, want an address-in-use failure")
	}
}
