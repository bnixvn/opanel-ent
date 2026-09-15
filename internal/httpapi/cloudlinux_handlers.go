package httpapi

import (
	"context"
	"net/http"

	"github.com/bnixvn/opanel-ent/internal/agent/actions"
	"github.com/bnixvn/opanel-ent/internal/agentclient"
	"github.com/bnixvn/opanel-ent/internal/db"
)

// handleCloudLinux reports everything the CloudLinux page shows.
//
// One request rather than four, because the page is one screen and the four
// answers are useless apart: limits without usage say nothing about whether
// they are the right limits, and usage without names is a list of uids.
func (s *Server) handleCloudLinux(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	status, err := agentclient.Call[actions.CLStatus](ctx, s.agent, "cl.status", 1, struct{}{})
	if err != nil {
		s.agentError(w, err)
		return
	}
	body := map[string]any{"status": status}

	if !status.Installed {
		writeJSON(w, http.StatusOK, body)
		return
	}

	if limits, err := agentclient.Call[struct {
		Limits []actions.CLLimits `json:"limits"`
	}](ctx, s.agent, "cl.limits", 1, struct{}{}); err == nil {
		body["limits"] = limits.Limits
	}

	period := r.URL.Query().Get("period")
	if usage, err := agentclient.Call[struct {
		Usage []actions.CLUsage `json:"usage"`
	}](ctx, s.agent, "cl.usage", 1, actions.CLUsageRequest{Period: period}); err == nil {
		body["usage"] = withUsernames(ctx, s.db, usage.Usage)
	}

	writeJSON(w, http.StatusOK, body)
}

// withUsernames turns uids into the names an operator recognises.
//
// LVE works in uids and the panel works in accounts. Showing the raw number
// would make the page correct and unreadable -- the whole reason to put this
// in the panel rather than leaving it to lveinfo on the command line.
func withUsernames(ctx context.Context, store *db.DB, rows []actions.CLUsage) []actions.CLUsage {
	users, err := store.ListUsers(ctx, db.ScopeAll())
	if err != nil {
		return rows
	}
	byUID := make(map[int64]string, len(users))
	for _, u := range users {
		if u.LinuxUID != nil {
			byUID[*u.LinuxUID] = u.Username
		}
	}
	for i := range rows {
		rows[i].Username = byUID[rows[i].UID]
	}
	return rows
}

// handleCloudLinuxIntegration makes CloudLinux able to read this panel.
func (s *Server) handleCloudLinuxIntegration(w http.ResponseWriter, r *http.Request) {
	status, err := agentclient.Call[actions.CLStatus](r.Context(), s.agent,
		"cl.install_integration", 1, struct{}{})
	if err != nil {
		s.agentError(w, err)
		return
	}
	s.audit(r, "cloudlinux.integration", "install", true, "")
	writeJSON(w, http.StatusOK, status)
}
