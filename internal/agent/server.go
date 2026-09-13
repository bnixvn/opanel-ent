package agent

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
	"path/filepath"
	"sync"
	"time"
)

// AuditFunc records one completed action. It runs on the agent side so a
// compromised or buggy api process cannot omit entries from the trail.
type AuditFunc func(ctx context.Context, ev AuditEvent)

// AuditEvent describes one served action.
type AuditEvent struct {
	Action   string
	PeerUID  uint32
	PeerPID  int32
	OK       bool
	Err      string
	Duration time.Duration
}

// ServerOptions configures a Server. Only Registry is required.
type ServerOptions struct {
	Registry *Registry
	// AllowedUIDs lists uids permitted to call. Empty means "root only".
	// Membership is checked with SO_PEERCRED, which the kernel fills in --
	// the client cannot forge it.
	AllowedUIDs []uint32
	// SocketMode is applied to the socket file. Default 0660.
	SocketMode os.FileMode
	// SocketGID, when non-negative, is chowned onto the socket so the
	// unprivileged api user can connect through group permission.
	SocketGID int
	// ActionTimeout bounds one handler. Default 2 minutes.
	ActionTimeout time.Duration
	// MaxConcurrent bounds handlers running at once. Default 16.
	MaxConcurrent int
	Logger        *slog.Logger
	Audit         AuditFunc
}

// Server serves the action registry over a unix socket.
type Server struct {
	opts ServerOptions
	ln   net.Listener
	sem  chan struct{}
	log  *slog.Logger

	wg       sync.WaitGroup
	closeOne sync.Once
}

// NewServer prepares a Server. Call Serve to accept connections.
func NewServer(opts ServerOptions) (*Server, error) {
	if opts.Registry == nil {
		return nil, errors.New("agent: registry is required")
	}
	if opts.SocketMode == 0 {
		opts.SocketMode = 0o660
	}
	if opts.ActionTimeout == 0 {
		opts.ActionTimeout = 2 * time.Minute
	}
	if opts.MaxConcurrent == 0 {
		opts.MaxConcurrent = 16
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if len(opts.AllowedUIDs) == 0 {
		opts.AllowedUIDs = []uint32{0}
	}
	return &Server{opts: opts, sem: make(chan struct{}, opts.MaxConcurrent), log: opts.Logger}, nil
}

// Listen binds the unix socket, replacing a stale one left by a crash.
func (s *Server) Listen(path string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("agent: create socket dir: %w", err)
	}
	// The directory must be traversable by the api group, not just the
	// socket readable. Permission on the socket inode is irrelevant if the
	// caller cannot enter the directory holding it -- connect() fails with
	// EACCES either way, which reads as a socket problem and is not.
	//
	// Done here rather than left to the unit file so the agent behaves the
	// same when started by hand during recovery.
	if s.opts.SocketGID >= 0 {
		if err := os.Chown(dir, 0, s.opts.SocketGID); err != nil {
			return fmt.Errorf("agent: chown socket dir: %w", err)
		}
		if err := os.Chmod(dir, 0o750); err != nil {
			return fmt.Errorf("agent: chmod socket dir: %w", err)
		}
	}
	// A leftover socket file from an unclean shutdown would make Listen fail
	// with EADDRINUSE even though nothing is serving it.
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("agent: remove stale socket: %w", err)
	}

	ln, err := net.Listen("unix", path)
	if err != nil {
		return fmt.Errorf("agent: listen %s: %w", path, err)
	}
	// Order matters: widen the group before relaxing the mode, so there is no
	// window where the socket is group-writable by the wrong group.
	if s.opts.SocketGID >= 0 {
		if err := os.Chown(path, 0, s.opts.SocketGID); err != nil {
			_ = ln.Close()
			return fmt.Errorf("agent: chown socket: %w", err)
		}
	}
	if err := os.Chmod(path, s.opts.SocketMode); err != nil {
		_ = ln.Close()
		return fmt.Errorf("agent: chmod socket: %w", err)
	}
	s.ln = ln
	return nil
}

// Serve accepts connections until ctx is cancelled or the listener closes.
func (s *Server) Serve(ctx context.Context) error {
	if s.ln == nil {
		return errors.New("agent: Listen must be called before Serve")
	}
	go func() {
		<-ctx.Done()
		s.closeOne.Do(func() { _ = s.ln.Close() })
	}()

	for {
		conn, err := s.ln.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				s.wg.Wait()
				return nil
			}
			// A single bad accept must not kill the agent; the panel would
			// lose every privileged operation at once.
			s.log.Warn("agent: accept failed", "err", err)
			continue
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.handleConn(ctx, conn)
		}()
	}
}

// Close stops accepting and waits for in-flight handlers.
func (s *Server) Close() error {
	if s.ln == nil {
		return nil
	}
	s.closeOne.Do(func() { _ = s.ln.Close() })
	s.wg.Wait()
	return nil
}

func (s *Server) handleConn(ctx context.Context, conn net.Conn) {
	defer func() { _ = conn.Close() }()

	// A caller that connects and then stalls must not hold a slot forever.
	_ = conn.SetDeadline(time.Now().Add(s.opts.ActionTimeout + 30*time.Second))

	cred, err := peerCredential(conn)
	if err != nil {
		s.log.Warn("agent: cannot read peer credentials", "err", err)
		writeResponse(conn, Response{OK: false, Code: CodeDenied, Error: "peer identity unavailable"})
		return
	}
	if !s.uidAllowed(cred.UID) {
		s.log.Warn("agent: rejected peer", "uid", cred.UID, "pid", cred.PID)
		writeResponse(conn, Response{OK: false, Code: CodeDenied, Error: "caller not permitted"})
		return
	}

	req, err := readRequest(conn)
	if err != nil {
		writeResponse(conn, Response{OK: false, Code: CodeBadPayload, Error: err.Error()})
		return
	}

	select {
	case s.sem <- struct{}{}:
		defer func() { <-s.sem }()
	case <-ctx.Done():
		writeResponse(conn, Response{ID: req.ID, OK: false, Code: CodeInternal, Error: "agent shutting down"})
		return
	}

	start := time.Now()
	resp := s.dispatch(ctx, req)
	dur := time.Since(start)

	if s.opts.Audit != nil {
		s.opts.Audit(ctx, AuditEvent{
			Action: req.Action, PeerUID: cred.UID, PeerPID: cred.PID,
			OK: resp.OK, Err: resp.Error, Duration: dur,
		})
	}
	s.log.Info("agent: action",
		"action", req.Action, "ok", resp.OK, "uid", cred.UID, "ms", dur.Milliseconds())

	writeResponse(conn, resp)
}

func (s *Server) uidAllowed(uid uint32) bool {
	for _, a := range s.opts.AllowedUIDs {
		if a == uid {
			return true
		}
	}
	return false
}

func (s *Server) dispatch(ctx context.Context, req Request) Response {
	h, err := s.opts.Registry.lookup(req.Action, req.Version)
	if err != nil {
		code := CodeUnknownAction
		if req.Version != 0 {
			code = CodeVersionMismatch
		}
		return Response{ID: req.ID, OK: false, Code: code, Error: err.Error()}
	}

	actx, cancel := context.WithTimeout(ctx, s.opts.ActionTimeout)
	defer cancel()

	out, err := h.fn(actx, req.Payload)
	if err != nil {
		var pe *PayloadError
		var de *DeniedError
		switch {
		case errors.As(err, &pe):
			return Response{ID: req.ID, OK: false, Code: CodeBadPayload, Error: err.Error()}
		case errors.As(err, &de):
			return Response{ID: req.ID, OK: false, Code: CodeDenied, Error: err.Error()}
		case errors.Is(actx.Err(), context.DeadlineExceeded):
			return Response{ID: req.ID, OK: false, Code: CodeTimeout, Error: "action timed out"}
		default:
			return Response{ID: req.ID, OK: false, Code: CodeInternal, Error: err.Error()}
		}
	}

	raw, err := json.Marshal(out)
	if err != nil {
		return Response{ID: req.ID, OK: false, Code: CodeInternal, Error: "cannot encode result: " + err.Error()}
	}
	return Response{ID: req.ID, OK: true, Payload: raw}
}

func readRequest(r io.Reader) (Request, error) {
	br := bufio.NewReader(io.LimitReader(r, MaxMessageBytes+1))
	line, err := br.ReadBytes('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return Request{}, fmt.Errorf("read request: %w", err)
	}
	if len(line) == 0 {
		return Request{}, errors.New("empty request")
	}
	if len(line) > MaxMessageBytes {
		return Request{}, fmt.Errorf("request exceeds %d bytes", MaxMessageBytes)
	}
	var req Request
	if err := json.Unmarshal(line, &req); err != nil {
		return Request{}, fmt.Errorf("decode request: %w", err)
	}
	if req.Action == "" {
		return Request{}, errors.New("request has no action")
	}
	return req, nil
}

func writeResponse(w io.Writer, resp Response) {
	b, err := json.Marshal(resp)
	if err != nil {
		b, _ = json.Marshal(Response{ID: resp.ID, OK: false, Code: CodeInternal,
			Error: "cannot encode response"})
	}
	b = append(b, '\n')
	_, _ = w.Write(b)
}
