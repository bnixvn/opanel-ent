package httpapi

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net"
	"net/http"
	"time"

	"github.com/bnixvn/opanel-ent/internal/agent/actions"
	"github.com/bnixvn/opanel-ent/internal/agentclient"
	"github.com/bnixvn/opanel-ent/internal/auth"
	"github.com/bnixvn/opanel-ent/internal/db"
)

// SSOTicketTTL is how long a phpMyAdmin link is good for.
//
// Short because the link is in a URL, which ends up in browser history and
// in any proxy log on the way. It only has to survive the redirect.
const SSOTicketTTL = 2 * time.Minute

// SSOSessionTTL is how long the throwaway MariaDB account lives after the
// ticket is redeemed. This is the real session length: dropping the account
// sooner would log the customer out in the middle of a query.
const SSOSessionTTL = 2 * time.Hour

func (s *Server) handlePMAStatus(w http.ResponseWriter, r *http.Request) {
	st, err := agentclient.Call[actions.PMAStatus](r.Context(), s.agent, "pma.status", 1, struct{}{})
	if err != nil {
		s.agentError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"phpmyadmin": st})
}

func (s *Server) handlePMAInstall(w http.ResponseWriter, r *http.Request) {
	st, err := agentclient.Call[actions.PMAStatus](r.Context(), s.agent, "pma.install", 1, struct{}{})
	if err != nil {
		s.audit(r, "phpmyadmin.install", "", false, err.Error())
		writeError(w, http.StatusBadRequest, "install_failed", trimAgent(err.Error()))
		return
	}
	// The vhost that serves it is part of the webserver configuration, so it
	// appears on the next render.
	if err := s.sites.SyncWebserver(r.Context()); err != nil {
		s.log.Warn("httpapi: could not publish the phpMyAdmin vhost", "err", err)
	}
	s.audit(r, "phpmyadmin.install", "", true, st.Version)
	writeJSON(w, http.StatusOK, map[string]any{"phpmyadmin": st})
}

// handlePMASignon mints a one-time link into phpMyAdmin.
func (s *Server) handlePMASignon(w http.ResponseWriter, r *http.Request) {
	actor := userFrom(r.Context())

	// Staff open it for a customer; a customer only ever for themselves.
	target := actor
	if name := r.URL.Query().Get("owner"); name != "" && name != actor.Username {
		if !auth.Role(actor.Role).AtLeast(auth.RoleReseller) {
			writeError(w, http.StatusForbidden, "forbidden", "you may only open your own databases")
			return
		}
		u, err := s.db.UserByUsername(r.Context(), name)
		if err != nil || !auth.CanSee(actor, u) {
			writeError(w, http.StatusNotFound, "not_found", "no such account")
			return
		}
		target = u
	}

	rows, err := s.db.ListDatabases(r.Context(), db.ScopeSelf(target.ID))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	if len(rows) == 0 {
		writeError(w, http.StatusBadRequest, "no_databases",
			"this account has no databases to open")
		return
	}
	names := make([]string, 0, len(rows))
	for _, d := range rows {
		names = append(names, d.Name)
	}

	acct, err := agentclient.Call[actions.PMASignonResult](r.Context(), s.agent, "pma.signon", 1,
		actions.PMASignonRequest{Prefix: target.Username, Databases: names})
	if err != nil {
		s.audit(r, "phpmyadmin.signon", target.Username, false, err.Error())
		writeError(w, http.StatusInternalServerError, "internal", trimAgent(err.Error()))
		return
	}

	token, err := randomToken()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	sum := sha256.Sum256([]byte(token))
	if err := s.db.CreateSSOTicket(r.Context(), &db.SSOTicket{
		TokenHash: hex.EncodeToString(sum[:]), UserID: target.ID,
		DBUser: acct.Username, DBPassword: acct.Password,
		// The row outlives the ticket: it is what the sweep reads to know
		// which throwaway account to drop and when.
		ExpiresAt: time.Now().Add(SSOSessionTTL),
	}); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	s.audit(r, "phpmyadmin.signon", target.Username, true, acct.Username)

	writeJSON(w, http.StatusOK, map[string]any{
		"url":       "/phpmyadmin/opanel-sso.php?token=" + token,
		"account":   acct.Username,
		"expires":   time.Now().Add(SSOTicketTTL).UTC(),
		"databases": names,
	})
}

// handleSSORedeem is called by the phpMyAdmin shim, never by a browser.
//
// Unauthenticated by necessity -- the shim has no session -- so three things
// stand in for authentication: the request must come from this machine, the
// ticket must exist and be unexpired, and redeeming it consumes it.
func (s *Server) handleSSORedeem(w http.ResponseWriter, r *http.Request) {
	if !isLoopback(r.RemoteAddr) {
		writeError(w, http.StatusForbidden, "forbidden", "not available from outside this server")
		return
	}
	var req struct {
		Token string `json:"token"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	sum := sha256.Sum256([]byte(req.Token))
	ticket, err := s.db.RedeemSSOTicket(r.Context(), hex.EncodeToString(sum[:]))
	if errors.Is(err, db.ErrNotFound) {
		// One message for expired, used and never-existed alike: the shim
		// shows it to whoever followed the link, and distinguishing them
		// would tell somebody guessing tokens when they had guessed one.
		writeError(w, http.StatusForbidden, "invalid_ticket", "that link is no longer valid")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}

	// A ticket is only good for the couple of minutes it takes to follow a
	// redirect, even though the account behind it lives for the session.
	if time.Since(ticket.CreatedAt) > SSOTicketTTL {
		writeError(w, http.StatusForbidden, "invalid_ticket", "that link is no longer valid")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"username": ticket.DBUser,
		"password": ticket.DBPassword,
	})
}

// isLoopback reports whether a request came from this machine.
func isLoopback(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// randomToken returns the value that goes in the sign-in URL.
func randomToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// sweepSSOAccounts drops the throwaway MariaDB accounts whose session has
// ended.
//
// On a timer rather than at logout, because a customer closing the tab is
// the normal way a phpMyAdmin session ends and no logout ever arrives.
func (s *Server) sweepSSOAccounts(ctx context.Context) {
	t := time.NewTicker(15 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			expired, err := s.db.ExpiredSSOAccounts(ctx, time.Now())
			if err != nil || len(expired) == 0 {
				continue
			}
			if _, err := agentclient.Call[struct{}](ctx, s.agent, "pma.drop", 1,
				actions.PMADropRequest{Usernames: expired}); err != nil {
				s.log.Warn("httpapi: could not drop expired phpMyAdmin accounts", "err", err)
				continue
			}
			if err := s.db.DeleteSSOTickets(ctx, expired); err != nil {
				s.log.Warn("httpapi: could not clear redeemed tickets", "err", err)
			}
			s.log.Info("httpapi: dropped expired phpMyAdmin accounts", "count", len(expired))
		}
	}
}
