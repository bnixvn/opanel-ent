package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// File job kinds and states.
const (
	FileJobArchive = "archive"
	FileJobExtract = "extract"
	FileJobUpload  = "upload"

	FileJobRunning = "running"
	FileJobDone    = "done"
	FileJobFailed  = "failed"
)

// FileJob is one long-running file operation.
type FileJob struct {
	ID         int64
	OwnerID    int64
	Owner      string
	Kind       string
	Status     string
	Target     string
	Detail     string
	Error      string
	CreatedAt  time.Time
	FinishedAt time.Time
}

const fileJobCols = `j.id, j.owner_id, COALESCE(u.username,''), j.kind, j.status,
	j.target, j.detail, j.error, j.created_at, j.finished_at`

const fileJobJoin = ` FROM file_jobs j LEFT JOIN users u ON u.id = j.owner_id`

func scanFileJob(row interface{ Scan(...any) error }) (*FileJob, error) {
	var j FileJob
	var created, finished string
	err := row.Scan(&j.ID, &j.OwnerID, &j.Owner, &j.Kind, &j.Status,
		&j.Target, &j.Detail, &j.Error, &created, &finished)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	j.CreatedAt, _ = parseTime(created)
	j.FinishedAt, _ = parseTime(finished)
	return &j, nil
}

// CreateFileJob opens a job in the running state.
func (d *DB) CreateFileJob(ctx context.Context, j *FileJob) (*FileJob, error) {
	res, err := d.ExecContext(ctx,
		`INSERT INTO file_jobs (owner_id, kind, status, target, detail, created_at)
		 VALUES (?,?,?,?,?,?)`,
		j.OwnerID, j.Kind, FileJobRunning, j.Target, j.Detail, fmtTime(time.Now()))
	if err != nil {
		return nil, fmt.Errorf("db: create file job: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	return d.FileJobByID(ctx, id)
}

// FileJobByID returns one job.
func (d *DB) FileJobByID(ctx context.Context, id int64) (*FileJob, error) {
	return scanFileJob(d.QueryRowContext(ctx,
		`SELECT `+fileJobCols+fileJobJoin+` WHERE j.id = ?`, id))
}

// FinishFileJob closes a job out. An empty failure means it worked.
func (d *DB) FinishFileJob(ctx context.Context, id int64, detail, failure string) error {
	status := FileJobDone
	if failure != "" {
		status = FileJobFailed
	}
	_, err := d.ExecContext(ctx,
		`UPDATE file_jobs SET status = ?, detail = ?, error = ?, finished_at = ? WHERE id = ?`,
		status, detail, failure, fmtTime(time.Now()), id)
	return err
}

// ListFileJobs returns an owner's recent jobs, newest first.
func (d *DB) ListFileJobs(ctx context.Context, ownerID int64, limit int) ([]*FileJob, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := d.QueryContext(ctx,
		`SELECT `+fileJobCols+fileJobJoin+
			` WHERE j.owner_id = ? ORDER BY j.id DESC LIMIT ?`, ownerID, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	out := make([]*FileJob, 0, limit)
	for rows.Next() {
		j, err := scanFileJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// FailRunningFileJobs closes out jobs left running by a crash or a restart.
//
// Nothing is resumable: the goroutine that was doing the work died with the
// process. Saying so is better than a row that claims to be running for ever
// and a page that waits on it.
func (d *DB) FailRunningFileJobs(ctx context.Context) (int64, error) {
	res, err := d.ExecContext(ctx,
		`UPDATE file_jobs SET status = ?, error = ?, finished_at = ?
		 WHERE status = ?`,
		FileJobFailed, "the panel restarted while this was running",
		fmtTime(time.Now()), FileJobRunning)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
