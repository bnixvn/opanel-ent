package httpapi

import (
	"context"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/bnixvn/opanel-ent/internal/agent/actions"
	"github.com/bnixvn/opanel-ent/internal/agentclient"
	"github.com/bnixvn/opanel-ent/internal/auth"
	"github.com/bnixvn/opanel-ent/internal/cron"
	"github.com/bnixvn/opanel-ent/internal/db"
)

// handleCronList returns an account's jobs.
//
// The first time an account is looked at, whatever is already in its crontab
// is imported into rows. Taking ownership of the file without that would
// throw away everything a customer set up over SSH on the next save.
func (s *Server) handleCronList(w http.ResponseWriter, r *http.Request) {
	target, ok := s.cronTarget(w, r)
	if !ok {
		return
	}

	state, err := agentclient.Call[actions.CronResult](r.Context(), s.agent, "cron.read", 1,
		actions.CronRequest{Owner: target.Username})
	if err != nil {
		s.agentError(w, err)
		return
	}

	if err := s.importCrontabOnce(r.Context(), target, state.Text); err != nil {
		s.log.Warn("httpapi: could not import an existing crontab",
			"user", target.Username, "err", err)
	}

	jobs, err := s.db.CronJobsFor(r.Context(), target.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"owner":   target.Username,
		"jobs":    viewCronJobs(jobs),
		"presets": cron.Presets,
		"allowed": state.Allowed,
		"max":     cron.MaxJobs,
	})
}

// importCrontabOnce turns an existing crontab into rows, exactly once.
func (s *Server) importCrontabOnce(ctx context.Context, target *db.User, text string) error {
	done, err := s.db.CronImported(ctx, target.ID)
	if err != nil || done {
		return err
	}
	for _, j := range cron.Parse(text) {
		if _, err := s.db.CreateCronJob(ctx, &db.CronJob{
			UserID: target.ID, Schedule: j.Schedule, Command: j.Command,
			Comment: j.Comment, Enabled: j.Enabled,
		}); err != nil {
			return err
		}
	}
	return s.db.MarkCronImported(ctx, target.ID)
}

// handleCronCreate adds a job and installs the crontab.
func (s *Server) handleCronCreate(w http.ResponseWriter, r *http.Request) {
	target, ok := s.cronTarget(w, r)
	if !ok {
		return
	}
	var req struct {
		Schedule string `json:"schedule"`
		Command  string `json:"command"`
		Comment  string `json:"comment"`
		Enabled  *bool  `json:"enabled"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	job := cron.Job{
		Schedule: req.Schedule, Command: req.Command, Comment: req.Comment,
		Enabled: req.Enabled == nil || *req.Enabled,
	}
	if err := cron.Validate(&job); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}

	existing, err := s.db.CronJobsFor(r.Context(), target.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	if len(existing) >= cron.MaxJobs {
		writeError(w, http.StatusConflict, "too_many",
			"this account already has the maximum number of scheduled jobs")
		return
	}

	created, err := s.db.CreateCronJob(r.Context(), &db.CronJob{
		UserID: target.ID, Schedule: job.Schedule, Command: job.Command,
		Comment: job.Comment, Enabled: job.Enabled,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	if err := s.installCrontab(r.Context(), target); err != nil {
		// The row is removed again: a job in the database that cron never
		// got would be installed later by an unrelated change.
		_ = s.db.DeleteCronJob(r.Context(), created.ID)
		s.audit(r, "cron.create", target.Username, false, err.Error())
		writeError(w, http.StatusBadRequest, "cron_failed", trimAgent(err.Error()))
		return
	}
	s.audit(r, "cron.create", target.Username, true, job.Schedule+" "+job.Command)
	writeJSON(w, http.StatusCreated, map[string]any{"job": viewCronJob(created)})
}

// handleCronUpdate rewrites a job.
func (s *Server) handleCronUpdate(w http.ResponseWriter, r *http.Request) {
	job, target, ok := s.loadCronJob(w, r)
	if !ok {
		return
	}
	var req struct {
		Schedule *string `json:"schedule"`
		Command  *string `json:"command"`
		Comment  *string `json:"comment"`
		Enabled  *bool   `json:"enabled"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	before := *job
	if req.Schedule != nil {
		job.Schedule = *req.Schedule
	}
	if req.Command != nil {
		job.Command = *req.Command
	}
	if req.Comment != nil {
		job.Comment = *req.Comment
	}
	if req.Enabled != nil {
		job.Enabled = *req.Enabled
	}

	checked := cron.Job{
		Schedule: job.Schedule, Command: job.Command,
		Comment: job.Comment, Enabled: job.Enabled,
	}
	if err := cron.Validate(&checked); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if err := s.db.UpdateCronJob(r.Context(), job); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	if err := s.installCrontab(r.Context(), target); err != nil {
		_ = s.db.UpdateCronJob(r.Context(), &before)
		s.audit(r, "cron.update", target.Username, false, err.Error())
		writeError(w, http.StatusBadRequest, "cron_failed", trimAgent(err.Error()))
		return
	}
	s.audit(r, "cron.update", target.Username, true, job.Schedule+" "+job.Command)
	writeJSON(w, http.StatusOK, map[string]any{"job": viewCronJob(job)})
}

// handleCronDelete removes a job.
func (s *Server) handleCronDelete(w http.ResponseWriter, r *http.Request) {
	job, target, ok := s.loadCronJob(w, r)
	if !ok {
		return
	}
	if err := s.db.DeleteCronJob(r.Context(), job.ID); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	if err := s.installCrontab(r.Context(), target); err != nil {
		s.audit(r, "cron.delete", target.Username, false, err.Error())
		writeError(w, http.StatusBadRequest, "cron_failed", trimAgent(err.Error()))
		return
	}
	s.audit(r, "cron.delete", target.Username, true, job.Command)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// installCrontab renders every row for an account and installs the result.
//
// The whole file every time, never a patch: the rows are the truth, and a
// crontab assembled by editing lines in place is the thing this design
// exists to avoid.
func (s *Server) installCrontab(ctx context.Context, target *db.User) error {
	rows, err := s.db.CronJobsFor(ctx, target.ID)
	if err != nil {
		return err
	}
	jobs := make([]cron.Job, 0, len(rows))
	for _, row := range rows {
		jobs = append(jobs, cron.Job{
			ID: row.ID, Schedule: row.Schedule, Command: row.Command,
			Comment: row.Comment, Enabled: row.Enabled,
		})
	}
	_, err = agentclient.Call[actions.CronResult](ctx, s.agent, "cron.write", 1,
		actions.CronWriteRequest{Owner: target.Username, Text: cron.Render(jobs)})
	return err
}

// cronTarget resolves whose crontab a request is about.
func (s *Server) cronTarget(w http.ResponseWriter, r *http.Request) (*db.User, bool) {
	actor := userFrom(r.Context())
	target := actor

	if name := r.URL.Query().Get("owner"); name != "" && name != actor.Username {
		if !auth.Role(actor.Role).AtLeast(auth.RoleReseller) {
			writeError(w, http.StatusForbidden, "forbidden",
				"you may only manage your own scheduled jobs")
			return nil, false
		}
		u, err := s.db.UserByUsername(r.Context(), name)
		if err != nil || !auth.CanSee(actor, u) {
			writeError(w, http.StatusNotFound, "not_found", "no such account")
			return nil, false
		}
		target = u
	}

	// A job runs as a Linux account. Staff accounts have none, so offering
	// them cron would be offering something that always fails.
	if target.LinuxUID == nil || *target.LinuxUID == 0 {
		writeError(w, http.StatusBadRequest, "no_account",
			"this account has no Linux user to run jobs as; pick a hosting account")
		return nil, false
	}
	return target, true
}

// loadCronJob resolves a job and checks the caller may touch it.
func (s *Server) loadCronJob(w http.ResponseWriter, r *http.Request) (*db.CronJob, *db.User, bool) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "job id must be a number")
		return nil, nil, false
	}
	job, err := s.db.CronJobByID(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "no such job")
		return nil, nil, false
	}
	owner, err := s.db.UserByID(r.Context(), job.UserID)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "no such job")
		return nil, nil, false
	}
	actor := userFrom(r.Context())
	if actor.ID != owner.ID && !auth.CanManage(actor, owner) {
		writeError(w, http.StatusNotFound, "not_found", "no such job")
		return nil, nil, false
	}
	return job, owner, true
}

func viewCronJob(j *db.CronJob) map[string]any {
	return map[string]any{
		"id": j.ID, "owner": j.Username, "schedule": j.Schedule,
		"command": j.Command, "comment": j.Comment, "enabled": j.Enabled,
		"updated_at": j.UpdatedAt,
	}
}

func viewCronJobs(rows []*db.CronJob) []map[string]any {
	out := make([]map[string]any, 0, len(rows))
	for _, j := range rows {
		out = append(out, viewCronJob(j))
	}
	return out
}
