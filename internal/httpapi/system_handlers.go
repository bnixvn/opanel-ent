package httpapi

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/bnixvn/opanel-ent/internal/agent/actions"
	"github.com/bnixvn/opanel-ent/internal/agentclient"
	"github.com/bnixvn/opanel-ent/internal/platform/svc"
	"github.com/bnixvn/opanel-ent/internal/version"
)

type healthResponse struct {
	OK      bool   `json:"ok"`
	Version string `json:"version"`
	DB      string `json:"db"`
	Agent   string `json:"agent"`
}

// handleHealth reports liveness plus the state of each dependency. It stays
// unauthenticated so a monitor can reach it, and therefore says nothing that
// is not already visible from the outside.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	resp := healthResponse{OK: true, Version: version.String(), DB: "ok", Agent: "ok"}

	if err := s.db.PingContext(ctx); err != nil {
		resp.OK, resp.DB = false, "unavailable"
	}
	if err := s.agent.Ping(ctx); err != nil {
		resp.OK, resp.Agent = false, "unavailable"
	}

	status := http.StatusOK
	if !resp.OK {
		status = http.StatusServiceUnavailable
	}
	writeJSON(w, status, resp)
}

func (s *Server) handleSystemInfo(w http.ResponseWriter, r *http.Request) {
	info, err := agentclient.Call[actions.SysInfoResult](r.Context(), s.agent, "sysinfo", 1, struct{}{})
	if err != nil {
		s.agentError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"panel_version":     version.String(),
		"distro":            info.Distro,
		"webserver_backend": s.cfg.WebserverBackend,
		"php_provider":      s.cfg.PHPProvider,
	})
}

func (s *Server) handleServiceList(w http.ResponseWriter, r *http.Request) {
	res, err := agentclient.Call[actions.UnitListResult](r.Context(), s.agent, "systemd.list", 1, struct{}{})
	if err != nil {
		s.agentError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleServiceAction(w http.ResponseWriter, r *http.Request) {
	unit := chi.URLParam(r, "unit")
	action := chi.URLParam(r, "action")

	st, err := agentclient.Call[svc.Status](r.Context(), s.agent, "systemd.action", 1,
		actions.UnitActionRequest{Unit: unit, Action: action})
	if err != nil {
		s.audit(r, "system.service."+action, unit, false, err.Error())
		s.agentError(w, err)
		return
	}
	s.audit(r, "system.service."+action, unit, true, "")
	writeJSON(w, http.StatusOK, st)
}

type auditView struct {
	ID        int64     `json:"id"`
	At        time.Time `json:"at"`
	ActorType string    `json:"actor_type"`
	ActorName string    `json:"actor_name"`
	Action    string    `json:"action"`
	Target    string    `json:"target"`
	OK        bool      `json:"ok"`
	Detail    string    `json:"detail"`
	IP        string    `json:"ip"`
}

func (s *Server) handleAuditList(w http.ResponseWriter, r *http.Request) {
	limit := 100
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			limit = n
		}
	}
	entries, err := s.db.RecentAudit(r.Context(), limit)
	if err != nil {
		s.log.Error("httpapi: read audit", "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	out := make([]auditView, 0, len(entries))
	for _, e := range entries {
		out = append(out, auditView{
			ID: e.ID, At: e.At, ActorType: e.ActorType, ActorName: e.ActorName,
			Action: e.Action, Target: e.Target, OK: e.OK, Detail: e.Detail, IP: e.IP,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": out})
}

// agentError maps an agent failure onto an HTTP status. A rejected payload is
// the caller's fault; anything else is ours.
func (s *Server) agentError(w http.ResponseWriter, err error) {
	var ae *agentclient.Error
	if !asAgentError(err, &ae) {
		s.log.Error("httpapi: agent unreachable", "err", err)
		writeError(w, http.StatusServiceUnavailable, "agent_unavailable",
			"privileged agent is not reachable")
		return
	}
	switch ae.Code {
	case "bad_payload":
		writeError(w, http.StatusBadRequest, "bad_request", ae.Msg)
	case "denied":
		writeError(w, http.StatusForbidden, "forbidden", ae.Msg)
	case "unknown_action", "version_mismatch":
		// The api and agent binaries are out of step, which is an
		// operational problem rather than anything the caller did.
		s.log.Error("httpapi: agent contract mismatch", "err", err)
		writeError(w, http.StatusInternalServerError, "agent_mismatch",
			"panel and agent versions disagree")
	case "timeout":
		writeError(w, http.StatusGatewayTimeout, "timeout", ae.Msg)
	default:
		s.log.Error("httpapi: agent action failed", "err", err)
		writeError(w, http.StatusInternalServerError, "internal", ae.Msg)
	}
}

// asAgentError unwraps err into an *agentclient.Error when it is one.
func asAgentError(err error, target **agentclient.Error) bool {
	return errors.As(err, target)
}
