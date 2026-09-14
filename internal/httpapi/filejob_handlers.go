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
	"github.com/bnixvn/opanel-ent/internal/filemanager"
)

// fileJobBudget bounds one background file operation. Generous, because
// packing a customer's whole account legitimately takes a while; bounded,
// because a wedged one otherwise holds a goroutine and a row for ever.
const fileJobBudget = 2 * time.Hour

type fileJobView struct {
	ID         int64  `json:"id"`
	Kind       string `json:"kind"`
	Status     string `json:"status"`
	Target     string `json:"target"`
	Detail     string `json:"detail"`
	Error      string `json:"error,omitempty"`
	CreatedAt  string `json:"created_at"`
	FinishedAt string `json:"finished_at,omitempty"`
}

func viewFileJob(j *db.FileJob) fileJobView {
	v := fileJobView{
		ID: j.ID, Kind: j.Kind, Status: j.Status, Target: j.Target,
		Detail: j.Detail, Error: j.Error,
		CreatedAt: j.CreatedAt.Format(time.RFC3339),
	}
	if !j.FinishedAt.IsZero() {
		v.FinishedAt = j.FinishedAt.Format(time.RFC3339)
	}
	return v
}

// startFileJob opens a row and runs work against a context of its own.
//
// The request's context is the wrong one to use: it is cancelled when the
// browser goes away, and going away is exactly what somebody does while a
// long archive runs. Every one of these was previously run on it, so closing
// the tab stopped the work halfway and left no record of why.
func (s *Server) startFileJob(r *http.Request, owner filemanager.Owner, kind, target string,
	work func(ctx context.Context) (string, error)) (*db.FileJob, error) {
	job, err := s.db.CreateFileJob(r.Context(), &db.FileJob{
		OwnerID: owner.UserID, Kind: kind, Target: target,
	})
	if err != nil {
		return nil, err
	}
	actor := userFrom(r.Context()).Username

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), fileJobBudget)
		defer cancel()

		detail, err := work(ctx)
		failure := ""
		if err != nil {
			failure = err.Error()
		}
		if uerr := s.db.FinishFileJob(ctx, job.ID, detail, failure); uerr != nil {
			s.log.Error("httpapi: could not close out a file job",
				"job", job.ID, "err", uerr)
		}
		s.log.Info("httpapi: file job finished",
			"job", job.ID, "kind", kind, "owner", owner.Username,
			"actor", actor, "target", target, "err", failure)
	}()
	return job, nil
}

func (s *Server) handleFileJobList(w http.ResponseWriter, r *http.Request) {
	o, ok := s.owner(w, r)
	if !ok {
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	rows, err := s.db.ListFileJobs(r.Context(), o.UserID, limit)
	if err != nil {
		s.log.Error("httpapi: list file jobs", "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	out := make([]fileJobView, 0, len(rows))
	for _, j := range rows {
		out = append(out, viewFileJob(j))
	}
	writeJSON(w, http.StatusOK, map[string]any{"jobs": out})
}

func (s *Server) handleFileJobGet(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "no such job")
		return
	}
	job, err := s.db.FileJobByID(r.Context(), id)
	if errors.Is(err, db.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "no such job")
		return
	}
	if err != nil {
		s.log.Error("httpapi: file job", "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	actor := userFrom(r.Context())
	if job.OwnerID != actor.ID {
		owner, err := s.db.UserByID(r.Context(), job.OwnerID)
		if err != nil || !auth.CanManage(actor, owner) {
			writeError(w, http.StatusNotFound, "not_found", "no such job")
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"job": viewFileJob(job)})
}

// describePaths names what an archive is being made from, for the job row.
func describePaths(paths []string) string {
	if len(paths) == 0 {
		return ""
	}
	if len(paths) == 1 {
		return paths[0]
	}
	return paths[0] + " and " + strconv.Itoa(len(paths)-1) + " more"
}

// trimDetail keeps a job's detail line short enough to belong in a table.
func trimDetail(s string) string {
	s = strings.TrimSpace(s)
	if len(s) <= 300 {
		return s
	}
	return s[:297] + "…"
}
