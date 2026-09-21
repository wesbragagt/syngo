package unixctl

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"sync"
	"time"

	"github.com/wesbragagt/syncthis/internal/app/ports"
)

var _ ports.ControlServer = (*Server)(nil)

// connTimeout bounds how long one request may take end to end.
const connTimeout = 60 * time.Second

// Server answers control requests on a unix socket. The socket is bound by
// Listen, so a daemon knows at startup whether the address is free.
type Server struct {
	ln   net.Listener
	path string
	log  *slog.Logger

	closeOnce sync.Once
}

// SocketMode is the permission the control socket is given: the owner alone
// may talk to the daemon, because every command the daemon answers is one the
// owner could run themselves.
const SocketMode os.FileMode = 0o600

// Listen binds the control socket at path and restricts it to its owner. The
// caller is responsible for removing a stale socket left behind by a dead
// daemon. log may be nil.
func Listen(path string, log *slog.Logger) (*Server, error) {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("listen on control socket %s: %w", path, err)
	}
	// The socket is created with whatever the umask allows, which can be
	// world-writable. It lives in a directory only the owner may enter, so
	// the window this closes is small, but the mode is what says who may
	// command the daemon.
	if err := os.Chmod(path, SocketMode); err != nil {
		_ = ln.Close()
		return nil, fmt.Errorf("restrict control socket %s: %w", path, err)
	}
	return &Server{ln: ln, path: path, log: log}, nil
}

// Path returns the socket path.
func (s *Server) Path() string { return s.path }

// Serve answers requests until ctx is done or the server is closed. It always
// returns nil after a clean stop.
func (s *Server) Serve(ctx context.Context, h ports.ControlHandler) error {
	var conns sync.WaitGroup
	stopped := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = s.Close()
		case <-stopped:
		}
	}()

	for {
		conn, err := s.ln.Accept()
		if err != nil {
			close(stopped)
			conns.Wait()
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return fmt.Errorf("accept on control socket %s: %w", s.path, err)
		}
		conns.Add(1)
		go func() {
			defer conns.Done()
			s.handle(ctx, conn, h)
		}()
	}
}

// Close stops accepting requests and removes the socket.
func (s *Server) Close() error {
	var err error
	s.closeOnce.Do(func() {
		if cerr := s.ln.Close(); cerr != nil && !errors.Is(cerr, net.ErrClosed) {
			err = fmt.Errorf("close control socket %s: %w", s.path, cerr)
		}
	})
	return err
}

// handle reads one request from conn, answers it and closes the connection.
func (s *Server) handle(ctx context.Context, conn net.Conn, h ports.ControlHandler) {
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(connTimeout))

	line, err := bufio.NewReader(io.LimitReader(conn, maxRequestBytes)).ReadBytes('\n')
	if err != nil && len(line) == 0 {
		s.log.Debug("control request not read", "error", err)
		return
	}

	var wire wireRequest
	var resp ports.ControlResponse
	if err := json.Unmarshal(line, &wire); err != nil {
		resp = ports.ControlResponse{OK: false, Error: fmt.Sprintf("malformed request: %v", err), Code: 2}
	} else {
		resp = h.Handle(ctx, wire.request())
	}

	if err := s.write(conn, resp); err != nil {
		s.log.Debug("control response not written", "error", err)
	}
}

// write encodes one response followed by a newline.
func (s *Server) write(conn net.Conn, resp ports.ControlResponse) error {
	data, err := json.Marshal(toWireResponse(resp))
	if err != nil {
		return fmt.Errorf("encode control response: %w", err)
	}
	if _, err := conn.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("write control response: %w", err)
	}
	return nil
}
