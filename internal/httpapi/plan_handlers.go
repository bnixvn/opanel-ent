package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/bnixvn/opanel-ent/internal/auth"
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
	rows, err := s.db.ListPlans(r.Context(), auth.ScopeFor(userFrom(r.Context())))
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
	// A package a reseller creates belongs to them and is theirs alone to
	// see and assign; an administrator's is a system package everyone can use.
	actor := userFrom(r.Context())
	plan := &db.Plan{
		Name: strings.TrimSpace(req.Name), Description: req.Description,
		MaxSites: req.MaxSites, MaxDatabases: req.MaxDatabases,
		DiskQuotaMB: req.DiskQuotaMB, DefaultPHP: req.DefaultPHP,
	}
	if auth.Role(actor.Role) == auth.RoleReseller {
		limits, err := s.db.ResellerLimitsFor(r.Context(), actor.ID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal", "internal error")
			return
		}
		if !limits.CanCreatePlans {
			writeError(w, http.StatusForbidden, "forbidden",
				"your account is not allowed to create packages")
			return
		}
		plan.OwnerID = &actor.ID
	}
	p, err := s.db.CreatePlan(r.Context(), plan)
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
	if !s.mayEditPlan(w, r, existing) {
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

	before := existing.DiskQuotaMB
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
	//
	// The disk figure is different: it is enforced by the filesystem, so
	// every account on this package needs its limit rewritten or the change
	// is cosmetic.
	if existing.DiskQuotaMB != before {
		s.reapplyQuotas(r, existing.ID)
	}
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
	if !s.mayEditPlan(w, r, p) {
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
	if !s.requireManage(w, r, target) {
		return
	}

	// What the account has now, so a reseller is charged only for the extra
	// space a bigger package promises rather than for the whole of it again.
	before := 0
	if cur, err := s.db.UserPlan(r.Context(), target.ID); err == nil && cur != nil {
		before = cur.DiskQuotaMB
	}
	after := 0
	if req.PlanID != nil {
		plan, err := s.db.PlanByID(r.Context(), *req.PlanID)
		if err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "no such package")
			return
		}
		if !s.mayEditPlan(w, r, plan) {
			return
		}
		after = plan.DiskQuotaMB
	}
	if err := s.users.CheckCanAllocate(r.Context(), userFrom(r.Context()), after-before); err != nil {
		s.audit(r, "user.plan", target.Username, false, err.Error())
		writeError(w, http.StatusConflict, "allocation_exceeded", err.Error())
		return
	}

	if err := s.db.SetUserPlan(r.Context(), target.ID, req.PlanID); err != nil {
		s.audit(r, "user.plan", target.Username, false, err.Error())
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}

	// Push the new limit to the filesystem. A failure here is reported:
	// telling an operator the package changed while the disk still enforces
	// the old number is how a server fills up.
	updated, err := s.db.UserByID(r.Context(), target.ID)
	if err == nil {
		if qerr := s.plans.ApplyQuota(r.Context(), updated); qerr != nil {
			s.audit(r, "user.plan", target.Username, true, "quota not applied: "+qerr.Error())
			writeJSON(w, http.StatusOK, map[string]any{
				"ok": true, "quota_warning": trimAgent(qerr.Error()),
			})
			return
		}
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

// mayEditPlan refuses a change to a package the caller does not own.
//
// A reseller may only touch their own packages. Letting one edit a system
// package would change what every other reseller on the server is selling.
func (s *Server) mayEditPlan(w http.ResponseWriter, r *http.Request, p *db.Plan) bool {
	actor := userFrom(r.Context())
	if auth.Role(actor.Role).AtLeast(auth.RoleAdmin) {
		return true
	}
	if p.OwnerID != nil && *p.OwnerID == actor.ID {
		return true
	}
	writeError(w, http.StatusForbidden, "forbidden",
		"that package belongs to the server, not to you")
	return false
}

// reapplyQuotas pushes a changed package size to every account on it.
//
// In the background: a package with fifty accounts means fifty xfs_quota
// calls, each of which walks a home directory, and an operator who clicked
// Save should not wait for that. Failures are logged per account rather than
// abandoning the rest.
func (s *Server) reapplyQuotas(r *http.Request, planID int64) {
	users, err := s.db.ListUsers(r.Context(), db.ScopeAll())
	if err != nil {
		s.log.Error("httpapi: cannot list users to re-apply quotas", "err", err)
		return
	}
	targets := make([]*db.User, 0, 8)
	for _, u := range users {
		id, err := s.db.UserPlanID(r.Context(), u.ID)
		if err == nil && id != nil && *id == planID {
			targets = append(targets, u)
		}
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		defer cancel()
		for _, u := range targets {
			if err := s.plans.ApplyQuota(ctx, u); err != nil {
				s.log.Error("httpapi: cannot re-apply quota",
					"user", u.Username, "err", err)
			}
		}
	}()
}
