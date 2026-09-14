package httpapi

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/bnixvn/opanel-ent/internal/db"
)

type planView struct {
	ID           int64  `json:"id"`
	Name         string `json:"name"`
	Description  string `json:"description"`
	MaxSites     int    `json:"max_sites"`
	MaxDatabases int    `json:"max_databases"`
	DiskQuotaMB  int    `json:"disk_quota_mb"`
	DefaultPHP   string `json:"default_php"`
	Users        int    `json:"users"`
}

func viewPlan(p *db.Plan) planView {
	return planView{
		ID: p.ID, Name: p.Name, Description: p.Description,
		MaxSites: p.MaxSites, MaxDatabases: p.MaxDatabases,
		DiskQuotaMB: p.DiskQuotaMB, DefaultPHP: p.DefaultPHP, Users: p.Users,
	}
}

func (s *Server) handlePlanList(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.ListPlans(r.Context())
	if err != nil {
		s.log.Error("httpapi: list plans", "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	out := make([]planView, 0, len(rows))
	for _, p := range rows {
		out = append(out, viewPlan(p))
	}
	writeJSON(w, http.StatusOK, map[string]any{"plans": out})
}

type planRequest struct {
	Name         string `json:"name"`
	Description  string `json:"description,omitempty"`
	MaxSites     int    `json:"max_sites"`
	MaxDatabases int    `json:"max_databases"`
	DiskQuotaMB  int    `json:"disk_quota_mb"`
	DefaultPHP   string `json:"default_php,omitempty"`
}

// validate rejects a package that cannot mean anything useful.
func (p planRequest) validate() error {
	if n := len(strings.TrimSpace(p.Name)); n == 0 || n > 64 {
		return errors.New("a package needs a name of at most 64 characters")
	}
	if p.MaxSites < 0 || p.MaxDatabases < 0 || p.DiskQuotaMB < 0 {
		return errors.New("limits cannot be negative; use 0 for unlimited")
	}
	return nil
}

func (s *Server) handlePlanCreate(w http.ResponseWriter, r *http.Request) {
	var req planRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := req.validate(); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	p, err := s.db.CreatePlan(r.Context(), &db.Plan{
		Name: strings.TrimSpace(req.Name), Description: req.Description,
		MaxSites: req.MaxSites, MaxDatabases: req.MaxDatabases,
		DiskQuotaMB: req.DiskQuotaMB, DefaultPHP: req.DefaultPHP,
	})
	if err != nil {
		s.audit(r, "plan.create", req.Name, false, err.Error())
		writeError(w, http.StatusConflict, "name_taken", err.Error())
		return
	}
	s.audit(r, "plan.create", p.Name, true, "")
	writeJSON(w, http.StatusCreated, viewPlan(p))
}

func (s *Server) handlePlanUpdate(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "package id must be a number")
		return
	}
	existing, err := s.db.PlanByID(r.Context(), id)
	if errors.Is(err, db.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "no such package")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	var req planRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := req.validate(); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}

	existing.Name = strings.TrimSpace(req.Name)
	existing.Description = req.Description
	existing.MaxSites = req.MaxSites
	existing.MaxDatabases = req.MaxDatabases
	existing.DiskQuotaMB = req.DiskQuotaMB
	existing.DefaultPHP = req.DefaultPHP

	if err := s.db.UpdatePlan(r.Context(), existing); err != nil {
		s.audit(r, "plan.update", existing.Name, false, err.Error())
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	// Lowering a limit below what accounts already use is allowed: existing
	// sites keep working and the limit applies to the next one. Refusing
	// would leave an operator unable to correct a package they mis-sized.
	s.audit(r, "plan.update", existing.Name, true, "")
	updated, _ := s.db.PlanByID(r.Context(), id)
	writeJSON(w, http.StatusOK, viewPlan(updated))
}

func (s *Server) handlePlanDelete(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "package id must be a number")
		return
	}
	p, err := s.db.PlanByID(r.Context(), id)
	if errors.Is(err, db.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "no such package")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	if err := s.db.DeletePlan(r.Context(), id); err != nil {
		s.audit(r, "plan.delete", p.Name, false, err.Error())
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	s.audit(r, "plan.delete", p.Name, true, "accounts left without a package")
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

type assignPlanRequest struct {
	// A null PlanID clears the assignment, leaving the account unlimited.
	PlanID *int64 `json:"plan_id"`
}

func (s *Server) handleUserPlanAssign(w http.ResponseWriter, r *http.Request) {
	target := s.loadPanelUser(w, r)
	if target == nil {
		return
	}
	var req assignPlanRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.PlanID != nil {
		if _, err := s.db.PlanByID(r.Context(), *req.PlanID); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "no such package")
			return
		}
	}
	if err := s.db.SetUserPlan(r.Context(), target.ID, req.PlanID); err != nil {
		s.audit(r, "user.plan", target.Username, false, err.Error())
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	s.audit(r, "user.plan", target.Username, true, "")
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleUsage reports what an account consumes against its package. A user
// may always read their own; staff may read anyone's.
func (s *Server) handleUsage(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r.Context())
	target := u

	if idStr := chi.URLParam(r, "id"); idStr != "" && idStr != "me" {
		other := s.loadPanelUser(w, r)
		if other == nil {
			return
		}
		if other.ID != u.ID && !roleAtLeastReseller(u) {
			writeError(w, http.StatusNotFound, "not_found", "no such user")
			return
		}
		target = other
	}

	usage, err := s.plans.UsageFor(r.Context(), target)
	if err != nil {
		s.log.Error("httpapi: usage", "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"username":   target.Username,
		"usage":      usage,
		"over_quota": usage.OverQuota(),
	})
}
