package httpapi

import (
	"net/http"
	"strconv"

	"github.com/bnixvn/opanel-ent/internal/auth"
	"github.com/bnixvn/opanel-ent/internal/db"
)

type resellerLimitsView struct {
	MaxAccounts    int  `json:"max_accounts"`
	MaxSites       int  `json:"max_sites"`
	MaxDatabases   int  `json:"max_databases"`
	DiskQuotaMB    int  `json:"disk_quota_mb"`
	BandwidthGB    int  `json:"bandwidth_gb"`
	CanCreatePlans bool `json:"can_create_plans"`
	AllowOversell  bool `json:"allow_oversell"`
}

type resellerUsageView struct {
	Accounts    int `json:"accounts"`
	Sites       int `json:"sites"`
	Databases   int `json:"databases"`
	DiskQuotaMB int `json:"disk_quota_mb"`
}

// handleResellerSummary reports an allowance and what has been handed out of
// it. A reseller reads their own; an administrator reads anyone's.
func (s *Server) handleResellerSummary(w http.ResponseWriter, r *http.Request) {
	actor := userFrom(r.Context())
	target := actor
	if id := r.URL.Query().Get("user"); id != "" {
		if !auth.Role(actor.Role).AtLeast(auth.RoleAdmin) {
			writeError(w, http.StatusForbidden, "forbidden",
				"only an administrator may read another reseller's allowance")
			return
		}
		u := s.userByIDParam(w, r, id)
		if u == nil {
			return
		}
		target = u
	}
	if auth.Role(target.Role) != auth.RoleReseller {
		writeJSON(w, http.StatusOK, map[string]any{"reseller": nil})
		return
	}

	sum, err := s.users.SummaryFor(r.Context(), target.ID)
	if err != nil {
		s.log.Error("httpapi: reseller summary", "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"reseller": map[string]any{
			"username": target.Username,
			"user_id":  target.ID,
			"limits": resellerLimitsView{
				MaxAccounts: sum.Limits.MaxAccounts, MaxSites: sum.Limits.MaxSites,
				MaxDatabases: sum.Limits.MaxDatabases, DiskQuotaMB: sum.Limits.DiskQuotaMB,
				BandwidthGB:    sum.Limits.BandwidthGB,
				CanCreatePlans: sum.Limits.CanCreatePlans,
				AllowOversell:  sum.Limits.AllowOversell,
			},
			"used": resellerUsageView{
				Accounts: sum.Allocation.Accounts, Sites: sum.Allocation.Sites,
				Databases: sum.Allocation.Databases, DiskQuotaMB: sum.Allocation.DiskQuotaMB,
			},
		},
	})
}

// handleResellerLimitsSet records what a reseller may hand out. Admin only.
func (s *Server) handleResellerLimitsSet(w http.ResponseWriter, r *http.Request) {
	target := s.loadPanelUser(w, r)
	if target == nil {
		return
	}
	var req resellerLimitsView
	if !decodeJSON(w, r, &req) {
		return
	}
	l := &db.ResellerLimits{
		UserID: target.ID, MaxAccounts: req.MaxAccounts, MaxSites: req.MaxSites,
		MaxDatabases: req.MaxDatabases, DiskQuotaMB: req.DiskQuotaMB,
		BandwidthGB: req.BandwidthGB, CanCreatePlans: req.CanCreatePlans,
		AllowOversell: req.AllowOversell,
	}
	if err := s.users.SetLimits(r.Context(), l); err != nil {
		s.audit(r, "reseller.limits", target.Username, false, err.Error())
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	s.audit(r, "reseller.limits", target.Username, true, "")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleUserParentSet moves a customer between resellers. Admin only: a
// reseller handing their customer to someone else, or taking one, is not a
// thing the product allows.
func (s *Server) handleUserParentSet(w http.ResponseWriter, r *http.Request) {
	target := s.loadPanelUser(w, r)
	if target == nil {
		return
	}
	var req struct {
		ParentID int64 `json:"parent_id"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := s.users.AssignParent(r.Context(), target.ID, req.ParentID); err != nil {
		s.audit(r, "user.parent", target.Username, false, err.Error())
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	s.audit(r, "user.parent", target.Username, true, "")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// userByIDParam resolves a user id from a query string value.
func (s *Server) userByIDParam(w http.ResponseWriter, r *http.Request, raw string) *db.User {
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "user must be a number")
		return nil
	}
	u, err := s.db.UserByID(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "no such user")
		return nil
	}
	return u
}
