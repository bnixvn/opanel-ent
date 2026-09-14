package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/bnixvn/opanel-ent/internal/platform/linuxuser"
	"github.com/bnixvn/opanel-ent/internal/platform/pty"
	"github.com/bnixvn/opanel-ent/internal/termproto"
)

// TerminalSocketPath is where the agent listens for terminal sessions.
const TerminalSocketPath = "/run/opanel/terminal.sock"

// TerminalOptions configure the terminal listener.
type TerminalOptions struct {
	Logger    *slog.Logger
	SocketGID int
	// AllowedUIDs may connect. Same list as the action socket: the panel's
	// own account and root.
	AllowedUIDs []uint32
	// MaxSessions bounds concurrent shells. Each is a process tree owned by
	// a customer, and a panel that will start an unbounded number of them on
	// request is a fork bomb with a login page.
	MaxSessions int
	// IdleTimeout closes a session nothing has typed at or printed to.
	IdleTimeout time.Duration
}

// TerminalServer hands out shells on a pseudo-terminal.
//
// It is deliberately a separate listener from the action socket. The action
// protocol is one request and one response; a terminal is a stream that
// lives for as long as somebody is looking at it, and mixing the two would
// mean every action gained a way to hold a connection open for ever.
type TerminalServer struct {
	opts TerminalOptions
	ln   net.Listener
	sem  chan struct{}
	wg   sync.WaitGroup
	log  *slog.Logger
	once sync.Once
}

// NewTerminalServer prepares a listener.
func NewTerminalServer(opts TerminalOptions) *TerminalServer {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.MaxSessions <= 0 {
		opts.MaxSessions = 8
	}
	if opts.IdleTimeout <= 0 {
		opts.IdleTimeout = 30 * time.Minute
	}
	if len(opts.AllowedUIDs) == 0 {
		opts.AllowedUIDs = []uint32{0}
	}
	return &TerminalServer{
		opts: opts,
		sem:  make(chan struct{}, opts.MaxSessions),
		log:  opts.Logger,
	}
}

// Listen binds the terminal socket with the same ownership as the action one.
func (s *TerminalServer) Listen(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("agent: create socket dir: %w", err)
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("agent: remove stale terminal socket: %w", err)
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return fmt.Errorf("agent: listen %s: %w", path, err)
	}
	if s.opts.SocketGID >= 0 {
		if err := os.Chown(path, 0, s.opts.SocketGID); err != nil {
			_ = ln.Close()
			return fmt.Errorf("agent: chown terminal socket: %w", err)
		}
	}
	if err := os.Chmod(path, 0o660); err != nil {
		_ = ln.Close()
		return fmt.Errorf("agent: chmod terminal socket: %w", err)
	}
	s.ln = ln
	return nil
}

// Serve accepts sessions until ctx is cancelled.
func (s *TerminalServer) Serve(ctx context.Context) error {
	if s.ln == nil {
		return errors.New("agent: terminal server has no listener")
	}
	go func() {
		<-ctx.Done()
		s.once.Do(func() { _ = s.ln.Close() })
	}()
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				s.wg.Wait()
				return nil
			}
			s.log.Warn("agent: terminal accept failed", "err", err)
			continue
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.session(conn)
		}()
	}
}

func (s *TerminalServer) session(conn net.Conn) {
	defer func() { _ = conn.Close() }()

	cred, err := peerCredential(conn)
	if err != nil {
		s.log.Warn("agent: terminal peer credentials unavailable", "err", err)
		return
	}
	if !slices.Contains(s.opts.AllowedUIDs, cred.UID) {
		s.log.Warn("agent: rejected terminal peer", "uid", cred.UID, "pid", cred.PID)
		return
	}

	// The handshake gets a short deadline of its own; the session that
	// follows is bounded by idleness instead, because a shell somebody is
	// reading is a connection that is legitimately quiet.
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	br := bufio.NewReader(conn)
	line, err := br.ReadBytes('\n')
	if err != nil {
		return
	}
	var hello termproto.Hello
	if err := json.Unmarshal(line, &hello); err != nil {
		s.reject(conn, "malformed request")
		return
	}

	acct, err := s.resolve(hello.User)
	if err != nil {
		s.log.Warn("agent: terminal refused", "user", hello.User, "err", err)
		s.reject(conn, err.Error())
		return
	}

	select {
	case s.sem <- struct{}{}:
		defer func() { <-s.sem }()
	default:
		s.reject(conn, "too many terminal sessions are already open on this server")
		return
	}

	if err := json.NewEncoder(conn).Encode(termproto.Ready{OK: true}); err != nil {
		return
	}
	_ = conn.SetDeadline(time.Time{})
	s.run(conn, br, acct, hello)
}

func (s *TerminalServer) reject(conn net.Conn, msg string) {
	_ = json.NewEncoder(conn).Encode(termproto.Ready{OK: false, Error: msg})
}

// resolve decides whether a shell may run as this account.
//
// This is the security boundary, and it is here rather than in the panel on
// purpose: the panel runs unprivileged and could be talked into asking for
// anything, so the process that can actually grant it is the one that has to
// say no. Only accounts the panel itself provisioned qualify -- membership
// of the SFTP group is what marks them -- which excludes root, every system
// account, and anything an operator made by hand.
func (s *TerminalServer) resolve(username string) (*linuxuser.Account, error) {
	// The shape only. Whether this account may have a shell is the next
	// check, and it is the one that means something: ValidName refuses names
	// a customer may not take, which is a different question and would bar
	// an operator from their own.
	if !linuxuser.ValidOwner(username) {
		return nil, errors.New("not an account name")
	}
	managed, err := linuxuser.List(context.Background())
	if err != nil {
		return nil, errors.New("cannot read the account list")
	}
	if !slices.ContainsFunc(managed, func(a linuxuser.Account) bool { return a.Username == username }) {
		return nil, errors.New("that account is not one this panel manages")
	}
	acct, err := linuxuser.Lookup(username)
	if err != nil || acct == nil {
		return nil, errors.New("no such account")
	}
	if acct.UID == 0 {
		// Unreachable while the check above holds, and kept because the cost
		// of being wrong about that is a root shell.
		return nil, errors.New("refusing a root shell")
	}
	return acct, nil
}

func (s *TerminalServer) run(conn net.Conn, br *bufio.Reader, acct *linuxuser.Account, hello termproto.Hello) {
	shell := firstShell()

	policy, err := BuildSessionPolicy(hello.Commands, hello.Unrestricted)
	if err != nil {
		s.log.Error("agent: cannot prepare a terminal session", "err", err)
		_ = termproto.WriteFrame(conn, termproto.FrameExit,
			[]byte("could not prepare the session: "+err.Error()))
		return
	}
	defer policy.Cleanup()

	args := []string{filepath.Base(shell), "-i"}
	if policy.RC != "" {
		args = []string{filepath.Base(shell), "--rcfile", policy.RC, "-i"}
	}

	term, err := pty.Start(pty.Options{
		Path: shell,
		// Interactive with our own startup file rather than a login shell.
		// A login shell reads /etc/profile and then the account's own
		// ~/.bash_profile, and the account owns that file -- so the last
		// word on PATH would belong to the person the PATH is there to
		// bound. --rcfile also replaces ~/.bashrc, for the same reason.
		Args: args,
		Dir:  acct.Home,
		Env: []string{
			"HOME=" + acct.Home,
			"USER=" + acct.Username,
			"LOGNAME=" + acct.Username,
			"SHELL=" + shell,
			"PATH=" + policy.PATH,
			"TERM=xterm-256color",
			"LANG=C.UTF-8",
		},
		UID: uint32(acct.UID),
		GID: uint32(acct.GID),
		// Exactly one group. Leaving this nil would hand the shell root's
		// supplementary groups, which is the whole game.
		Groups: []uint32{uint32(acct.GID)},
		Cols:   hello.Cols,
		Rows:   hello.Rows,
	})
	if err != nil {
		s.log.Error("agent: cannot start terminal", "user", acct.Username, "err", err)
		_ = termproto.WriteFrame(conn, termproto.FrameExit, []byte("could not start a shell: "+err.Error()))
		return
	}
	defer term.Close()
	s.log.Info("agent: terminal opened", "user", acct.Username, "shell", shell,
		"restricted", policy.RC != "", "commands", len(policy.Linked))

	var (
		mu   sync.Mutex
		idle = time.Now()
	)
	touch := func() {
		mu.Lock()
		idle = time.Now()
		mu.Unlock()
	}

	done := make(chan struct{})
	var closeOnce sync.Once
	finish := func() { closeOnce.Do(func() { close(done) }) }

	// The shell's output, frame by frame, until it exits.
	go func() {
		defer finish()
		buf := make([]byte, 32*1024)
		for {
			n, err := term.Master.Read(buf)
			if n > 0 {
				touch()
				if werr := termproto.WriteFrame(conn, termproto.FrameData, buf[:n]); werr != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()

	// What the person types, and the occasional window resize.
	go func() {
		defer finish()
		for {
			kind, payload, err := termproto.ReadFrame(br)
			if err != nil {
				return
			}
			touch()
			switch kind {
			case termproto.FrameData:
				if _, err := term.Master.Write(payload); err != nil {
					return
				}
			case termproto.FrameResize:
				if cols, rows, ok := termproto.Size(payload); ok {
					_ = term.Resize(cols, rows)
				}
			}
		}
	}()

	// An abandoned browser tab leaves a shell running as a customer for as
	// long as the agent does. Idleness is measured on traffic in either
	// direction, so a long-running command keeps its own session alive.
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			_ = termproto.WriteFrame(conn, termproto.FrameExit, []byte("session ended"))
			s.log.Info("agent: terminal closed", "user", acct.Username)
			return
		case <-ticker.C:
			mu.Lock()
			quiet := time.Since(idle)
			mu.Unlock()
			if quiet > s.opts.IdleTimeout {
				_ = termproto.WriteFrame(conn, termproto.FrameExit,
					[]byte("closed after "+strconv.Itoa(int(s.opts.IdleTimeout.Minutes()))+" idle minutes"))
				s.log.Info("agent: terminal closed as idle", "user", acct.Username)
				return
			}
		}
	}
}

// firstShell picks an interactive shell.
//
// The accounts themselves have nologin as their shell, which is what stops
// them getting one over SSH; this names a shell explicitly rather than
// reading theirs, because reading theirs would always answer nologin.
func firstShell() string {
	for _, p := range []string{"/bin/bash", "/usr/bin/bash", "/bin/sh"} {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p
		}
	}
	return "/bin/sh"
}
