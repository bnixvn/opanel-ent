package httpapi

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/coder/websocket"

	"github.com/bnixvn/opanel-ent/internal/agent"
	"github.com/bnixvn/opanel-ent/internal/auth"
	"github.com/bnixvn/opanel-ent/internal/db"
	"github.com/bnixvn/opanel-ent/internal/termproto"
)

// Settings an administrator can change to widen or drop the command list.
//
// Widening it gives nothing away, which is why it is allowed. The list is a
// guard rail against mistakes -- php and node are on it and run whatever
// they are given -- so the thing it is not is the reason an operator may
// turn it off. What cannot be changed from here is who the shell runs as.
const (
	settingTerminalCommands  = "terminal.commands"
	settingTerminalFreeStaff = "terminal.staff_unrestricted"
)

// terminalPolicy reads the effective command list for one session.
func (s *Server) terminalPolicy(r *http.Request, u *db.User) (commands []string, unrestricted bool) {
	if free, err := s.db.Setting(r.Context(), settingTerminalFreeStaff, ""); err == nil && free == "1" {
		if auth.Role(u.Role).AtLeast(auth.RoleReseller) {
			return nil, true
		}
	}
	raw, err := s.db.Setting(r.Context(), settingTerminalCommands, "")
	if err != nil || strings.TrimSpace(raw) == "" {
		return agent.ShellCommands, false
	}
	// Commas, semicolons or any whitespace: an operator editing a list of
	// forty names in a text box separates them however they feel like.
	return strings.FieldsFunc(raw, func(c rune) bool {
		return c == ',' || c == ';' || unicode.IsSpace(c)
	}), false
}

// handleTerminalStatus says whether the signed-in account can have a shell,
// so the page can show a reason rather than a button that fails.
func (s *Server) handleTerminalStatus(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r.Context())
	if u.LinuxUID == nil || *u.LinuxUID == 0 {
		writeJSON(w, http.StatusOK, map[string]any{
			"available":  false,
			"can_enable": true,
			"reason": "This account has no Linux user yet. Adding one gives it a " +
				"home directory on the server, a shell, and an SFTP login.",
		})
		return
	}
	commands, unrestricted := s.terminalPolicy(r, u)
	writeJSON(w, http.StatusOK, map[string]any{
		"available": true,
		"username":  u.Username,
		// What the session may run, so the page can say so before somebody
		// types something and is told no.
		"commands":     commands,
		"unrestricted": unrestricted,
	})
}

// handleTerminal bridges a browser WebSocket to a shell on the agent.
//
// The panel is the wrong process to start a shell in: it runs unprivileged
// and cannot become anybody. It is the right process to decide whose shell
// this is, because it is the one holding the session. So it carries bytes
// and nothing else, and the agent -- which can grant a shell -- makes its
// own decision about whether to.
func (s *Server) handleTerminal(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r.Context())
	if u.LinuxUID == nil || *u.LinuxUID == 0 {
		writeError(w, http.StatusBadRequest, "no_linux_account",
			"this account has no Linux user to open a shell as")
		return
	}
	target := u.Username

	// Staff may open a customer's shell. Never anybody else's, and never a
	// shell as somebody senior to them.
	if want := r.URL.Query().Get("user"); want != "" && want != u.Username {
		other := s.userByName(r.Context(), want)
		if other == nil || !mayOpenShellFor(u, other) {
			writeError(w, http.StatusNotFound, "not_found", "no such account")
			return
		}
		if other.LinuxUID == nil || *other.LinuxUID == 0 {
			writeError(w, http.StatusBadRequest, "no_linux_account",
				"that account has no Linux user to open a shell as")
			return
		}
		target = other.Username
	}

	cols := uint16(clampInt(r.URL.Query().Get("cols"), 80, 20, 500))
	rows := uint16(clampInt(r.URL.Query().Get("rows"), 24, 5, 200))

	// The socket first. A browser handed an accepted WebSocket that then
	// fails to reach the agent has no way to be told why, because the reason
	// is gone with the HTTP response.
	sockPath := filepath.Join(filepath.Dir(s.agentSocket()), "terminal.sock")
	conn, err := net.DialTimeout("unix", sockPath, 5*time.Second)
	if err != nil {
		s.log.Error("httpapi: cannot reach the terminal socket", "err", err)
		writeError(w, http.StatusServiceUnavailable, "agent_unreachable",
			"the agent is not answering; terminals need opanel-agent running")
		return
	}
	defer func() { _ = conn.Close() }()

	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	commands, unrestricted := s.terminalPolicy(r, u)
	hello := termproto.Hello{
		User: target, Cols: cols, Rows: rows,
		Commands: commands, Unrestricted: unrestricted,
	}
	if err := json.NewEncoder(conn).Encode(hello); err != nil {
		writeError(w, http.StatusBadGateway, "agent_error", "the agent closed the connection")
		return
	}
	br := bufio.NewReader(conn)
	line, err := br.ReadBytes('\n')
	if err != nil {
		writeError(w, http.StatusBadGateway, "agent_error", "the agent closed the connection")
		return
	}
	var ready termproto.Ready
	if err := json.Unmarshal(line, &ready); err != nil || !ready.OK {
		msg := ready.Error
		if msg == "" {
			msg = "the agent refused the session"
		}
		writeError(w, http.StatusForbidden, "refused", msg)
		return
	}
	_ = conn.SetDeadline(time.Time{})

	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// Same origin only. A terminal reachable from any page somebody can
		// be persuaded to open is a shell handed to whoever wrote that page.
		OriginPatterns: []string{r.Host},
	})
	if err != nil {
		s.log.Warn("httpapi: terminal websocket refused", "err", err)
		return
	}
	defer func() { _ = ws.CloseNow() }()
	s.audit(r, "terminal.open", target, true, "")

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	// Agent to browser.
	go func() {
		defer cancel()
		for {
			kind, payload, err := termproto.ReadFrame(br)
			if err != nil {
				return
			}
			switch kind {
			case termproto.FrameData:
				if err := ws.Write(ctx, websocket.MessageBinary, payload); err != nil {
					return
				}
			case termproto.FrameExit:
				_ = ws.Close(websocket.StatusNormalClosure, string(payload))
				return
			}
		}
	}()

	// Browser to agent. A text message is a control message -- only resize
	// so far -- and a binary one is keystrokes, so a person typing JSON at a
	// shell is not mistaken for a command to the panel.
	for {
		typ, data, err := ws.Read(ctx)
		if err != nil {
			return
		}
		if typ == websocket.MessageText {
			var msg struct {
				Resize *struct {
					Cols uint16 `json:"cols"`
					Rows uint16 `json:"rows"`
				} `json:"resize"`
			}
			if json.Unmarshal(data, &msg) == nil && msg.Resize != nil {
				_ = termproto.WriteFrame(conn, termproto.FrameResize,
					termproto.SizePayload(msg.Resize.Cols, msg.Resize.Rows))
			}
			continue
		}
		if err := termproto.WriteFrame(conn, termproto.FrameData, data); err != nil {
			return
		}
	}
}

// mayOpenShellFor says whether actor may have a shell as target.
//
// Stricter than CanManage: managing an account means changing its package or
// its password, which leaves a trail the owner can see. A shell is being the
// customer, so it is reserved for staff over their own customers and never
// granted sideways or upwards.
func mayOpenShellFor(actor, target *db.User) bool {
	if target.LinuxUID == nil || *target.LinuxUID == 0 {
		return false
	}
	return auth.CanManage(actor, target)
}

// userByName looks an account up, returning nil rather than an error: every
// caller here turns a failure into the same "no such account".
func (s *Server) userByName(ctx context.Context, name string) *db.User {
	u, err := s.db.UserByUsername(ctx, name)
	if err != nil || u == nil {
		return nil
	}
	return u
}

// agentSocket reports the path the panel talks to the agent on, so the
// terminal socket can be found beside it.
func (s *Server) agentSocket() string {
	if p := s.cfg.AgentSocket; p != "" {
		return p
	}
	return agent.SocketPath
}

func clampInt(raw string, fallback, lo, hi int) int {
	n, err := strconv.Atoi(raw)
	if err != nil {
		return fallback
	}
	if n < lo {
		return lo
	}
	if n > hi {
		return hi
	}
	return n
}
