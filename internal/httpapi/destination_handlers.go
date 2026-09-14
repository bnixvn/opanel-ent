package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/bnixvn/opanel-ent/internal/agent/actions"
	"github.com/bnixvn/opanel-ent/internal/agentclient"
	"github.com/bnixvn/opanel-ent/internal/auth"
	"github.com/bnixvn/opanel-ent/internal/db"
)

// DestinationTestTimeout bounds a connection test. Long enough for a slow
// handshake across the world, short enough that a wrong address fails while
// somebody is still looking at the form.
const DestinationTestTimeout = 3 * time.Minute

// handleDestinationList returns the destinations the caller can see.
func (s *Server) handleDestinationList(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.ListBackupDestinations(r.Context(), auth.ScopeFor(userFrom(r.Context())))
	if err != nil {
		s.log.Error("httpapi: list destinations", "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"destinations": viewDestinations(rows)})
}

// destinationRequest is what the form sends.
//
// The secret is accepted and passed straight to the agent; it is never
// written to the panel's database and never comes back out of this API.
type destinationRequest struct {
	Name       string `json:"name"`
	Kind       string `json:"kind"`
	Enabled    *bool  `json:"enabled"`
	Host       string `json:"host"`
	Port       int    `json:"port"`
	User       string `json:"user"`
	Path       string `json:"path"`
	HostKey    string `json:"host_key"`
	Bucket     string `json:"bucket"`
	Region     string `json:"region"`
	Endpoint   string `json:"endpoint"`
	AccessKey  string `json:"access_key"`
	Secret     string `json:"secret"`
	SecretKind string `json:"secret_kind"`
	// Owner lets staff add a destination for a customer, or for the server
	// itself with the empty string and the server flag.
	Owner  string `json:"owner"`
	Server bool   `json:"server"`
}

// handleDestinationCreate stores a destination and proves it works.
//
// Tested before it is kept: a destination that was never reachable is worse
// than none, because it looks like backups are going somewhere.
func (s *Server) handleDestinationCreate(w http.ResponseWriter, r *http.Request) {
	var req destinationRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	ownerID, ok := s.destinationOwner(w, r, req)
	if !ok {
		return
	}
	if strings.TrimSpace(req.Name) == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "a name is required")
		return
	}

	row, err := s.db.CreateBackupDestination(r.Context(), &db.BackupDestination{
		OwnerID: ownerID, Name: strings.TrimSpace(req.Name), Kind: req.Kind,
		Summary: destinationSummary(req), Enabled: req.Enabled == nil || *req.Enabled,
	})
	if err != nil {
		s.log.Error("httpapi: create destination", "name", req.Name, "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}

	if err := s.saveDestinationConfig(r.Context(), row.ID, req); err != nil {
		_ = s.db.DeleteBackupDestination(r.Context(), row.ID)
		writeError(w, http.StatusBadRequest, "bad_request", trimAgent(err.Error()))
		return
	}
	res, err := agentclient.Call[actions.DestinationResult](r.Context(),
		s.agent.WithTimeout(DestinationTestTimeout), "dest.test", 1, actions.DestinationRef{ID: row.ID})
	if err != nil {
		// The row goes too: half a destination is a destination somebody
		// will believe in.
		_, _ = agentclient.Call[actions.DestinationResult](r.Context(), s.agent,
			"dest.delete", 1, actions.DestinationRef{ID: row.ID})
		_ = s.db.DeleteBackupDestination(r.Context(), row.ID)
		s.audit(r, "destination.create", req.Name, false, err.Error())
		writeError(w, http.StatusBadRequest, "test_failed", trimAgent(err.Error()))
		return
	}
	_ = s.db.SetDestinationResult(r.Context(), row.ID, true, "")

	s.audit(r, "destination.create", req.Name, true, req.Kind)
	updated, _ := s.db.BackupDestinationByID(r.Context(), row.ID)
	writeJSON(w, http.StatusCreated, map[string]any{
		"destination": viewDestination(updated), "message": res.Message,
	})
}

// handleDestinationUpdate changes a destination, credential included when
// one is sent.
func (s *Server) handleDestinationUpdate(w http.ResponseWriter, r *http.Request) {
	row := s.loadDestination(w, r)
	if row == nil {
		return
	}
	var req destinationRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	req.Kind = row.Kind
	if strings.TrimSpace(req.Name) == "" {
		req.Name = row.Name
	}
	if err := s.saveDestinationConfig(r.Context(), row.ID, req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", trimAgent(err.Error()))
		return
	}
	row.Name = strings.TrimSpace(req.Name)
	row.Summary = destinationSummary(req)
	if req.Enabled != nil {
		row.Enabled = *req.Enabled
	}
	if err := s.db.UpdateBackupDestination(r.Context(), row); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	s.audit(r, "destination.update", row.Name, true, "")
	writeJSON(w, http.StatusOK, map[string]any{"destination": viewDestination(row)})
}

// handleDestinationTest connects and writes a probe.
func (s *Server) handleDestinationTest(w http.ResponseWriter, r *http.Request) {
	row := s.loadDestination(w, r)
	if row == nil {
		return
	}
	res, err := agentclient.Call[actions.DestinationResult](r.Context(),
		s.agent.WithTimeout(DestinationTestTimeout), "dest.test", 1, actions.DestinationRef{ID: row.ID})
	if err != nil {
		_ = s.db.SetDestinationResult(r.Context(), row.ID, false, trimAgent(err.Error()))
		s.audit(r, "destination.test", row.Name, false, err.Error())
		writeError(w, http.StatusBadRequest, "test_failed", trimAgent(err.Error()))
		return
	}
	_ = s.db.SetDestinationResult(r.Context(), row.ID, true, "")
	s.audit(r, "destination.test", row.Name, true, "")
	writeJSON(w, http.StatusOK, map[string]any{"ok": res.OK, "message": res.Message})
}

// handleDestinationDelete removes a destination and its stored credential.
func (s *Server) handleDestinationDelete(w http.ResponseWriter, r *http.Request) {
	row := s.loadDestination(w, r)
	if row == nil {
		return
	}
	if _, err := agentclient.Call[actions.DestinationResult](r.Context(), s.agent,
		"dest.delete", 1, actions.DestinationRef{ID: row.ID}); err != nil {
		s.log.Warn("httpapi: could not remove a destination credential",
			"destination", row.ID, "err", err)
	}
	if err := s.db.DeleteBackupDestination(r.Context(), row.ID); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	s.audit(r, "destination.delete", row.Name, true, "")
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// saveDestinationConfig hands the agent everything, credential included.
func (s *Server) saveDestinationConfig(ctx context.Context, id int64, req destinationRequest) error {
	_, err := agentclient.Call[actions.DestinationResult](ctx, s.agent, "dest.save", 1,
		actions.DestinationConfig{
			ID: id, Kind: req.Kind,
			Host: req.Host, Port: req.Port, User: req.User, Path: req.Path,
			HostKey: req.HostKey,
			Bucket:  req.Bucket, Region: req.Region, Endpoint: req.Endpoint,
			AccessKey: req.AccessKey,
			Secret:    req.Secret, SecretKind: req.SecretKind,
		})
	return err
}

// destinationOwner decides whose destination is being created.
func (s *Server) destinationOwner(w http.ResponseWriter, r *http.Request, req destinationRequest) (int64, bool) {
	actor := userFrom(r.Context())

	if req.Server {
		// A server-wide destination receives every account's archives, so it
		// is an administrator's to configure.
		if !auth.Role(actor.Role).AtLeast(auth.RoleAdmin) {
			writeError(w, http.StatusForbidden, "forbidden",
				"only an administrator can add a destination for the whole server")
			return 0, false
		}
		return 0, true
	}
	if req.Owner == "" || req.Owner == actor.Username {
		return actor.ID, true
	}
	if !auth.Role(actor.Role).AtLeast(auth.RoleReseller) {
		writeError(w, http.StatusForbidden, "forbidden",
			"you may only add a destination for your own account")
		return 0, false
	}
	u, err := s.db.UserByUsername(r.Context(), req.Owner)
	if err != nil || !auth.CanSee(actor, u) {
		writeError(w, http.StatusNotFound, "not_found", "no such account")
		return 0, false
	}
	return u.ID, true
}

// loadDestination resolves one the caller may change.
func (s *Server) loadDestination(w http.ResponseWriter, r *http.Request) *db.BackupDestination {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "destination id must be a number")
		return nil
	}
	row, err := s.db.BackupDestinationByID(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "no such destination")
		return nil
	}
	actor := userFrom(r.Context())
	if row.OwnerID == 0 {
		if !auth.Role(actor.Role).AtLeast(auth.RoleAdmin) {
			writeError(w, http.StatusForbidden, "forbidden",
				"the server's own destination is an administrator's to change")
			return nil
		}
		return row
	}
	if !auth.OwnsResource(actor, row.OwnerID, 0) {
		writeError(w, http.StatusNotFound, "not_found", "no such destination")
		return nil
	}
	return row
}

// destinationSummary is what the list shows: enough to recognise a
// destination, never enough to reach it.
func destinationSummary(req destinationRequest) string {
	if req.Kind == actions.DestSFTP {
		port := req.Port
		if port == 0 {
			port = 22
		}
		return fmt.Sprintf("%s@%s:%d%s", req.User, req.Host, port, pathOrRoot(req.Path))
	}
	endpoint := req.Endpoint
	if endpoint == "" {
		endpoint = "s3.amazonaws.com"
	}
	return fmt.Sprintf("%s/%s%s", endpoint, req.Bucket, pathOrRoot(req.Path))
}

func pathOrRoot(p string) string {
	if p == "" {
		return "/"
	}
	if !strings.HasPrefix(p, "/") {
		return "/" + p
	}
	return p
}

func viewDestination(d *db.BackupDestination) map[string]any {
	return map[string]any{
		"id": d.ID, "owner": d.OwnerName, "server": d.OwnerID == 0,
		"name": d.Name, "kind": d.Kind, "summary": d.Summary,
		"enabled": d.Enabled, "last_ok_at": d.LastOKAt, "last_error": d.LastError,
	}
}

func viewDestinations(rows []*db.BackupDestination) []map[string]any {
	out := make([]map[string]any, 0, len(rows))
	for _, d := range rows {
		out = append(out, viewDestination(d))
	}
	return out
}
