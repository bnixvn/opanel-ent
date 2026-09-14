// Package backups creates, restores and prunes account archives.
//
// A backup is per hosting account, not per site: restoring a site without the
// database it reads leaves a broken site, and the two are only linked through
// the account. The archive holds the account's whole home directory plus a
// dump of each database it owns.
package backups

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/bnixvn/opanel-ent/internal/agent/actions"
	"github.com/bnixvn/opanel-ent/internal/agentclient"
	"github.com/bnixvn/opanel-ent/internal/auth"
	"github.com/bnixvn/opanel-ent/internal/backuparchive"
	"github.com/bnixvn/opanel-ent/internal/db"
)

// ErrNoAccount means the panel user has no Linux account, so there is
// nothing to archive.
var ErrNoAccount = errors.New("backups: this account has no home directory on the server")

// ErrForbidden means the caller may not act on that account.
var ErrForbidden = errors.New("backups: not permitted to manage that account's backups")

// ErrBusy means a backup for that account is already running.
var ErrBusy = errors.New("backups: a backup for this account is already running")

// AgentTimeout is how long the service waits on the agent for an archive or a
// restore. Slightly under the agent's own budget so the agent's timeout wins
// the race and the caller gets a real reason rather than a dead socket.
const AgentTimeout = actions.BackupBudget - time.Minute

// Service owns the backup lifecycle.
type Service struct {
	db    *db.DB
	agent *agentclient.Client
	// slow is the same socket with a deadline long enough for an archive.
	slow *agentclient.Client
	log  *slog.Logger

	// running guards against two archives of one account at once: they would
	// race over the same temporary directory and produce two half-consistent
	// dumps of the same databases.
	mu      sync.Mutex
	running map[int64]bool
}

// New builds the service.
func New(database *db.DB, ac *agentclient.Client, log *slog.Logger) *Service {
	if log == nil {
		log = slog.Default()
	}
	return &Service{
		db: database, agent: ac, slow: ac.WithTimeout(AgentTimeout),
		log: log, running: make(map[int64]bool),
	}
}

// Owner names the account a backup belongs to.
type Owner struct {
	ID       int64
	Username string
}

// ResolveOwner decides whose backups the caller is asking about, on the same
// rule as the file manager: an end user only ever reaches their own.
func (s *Service) ResolveOwner(ctx context.Context, actor *db.User, requested string) (Owner, error) {
	target := actor
	if requested != "" && requested != actor.Username {
		if !auth.Role(actor.Role).AtLeast(auth.RoleReseller) {
			return Owner{}, ErrForbidden
		}
		u, err := s.db.UserByUsername(ctx, requested)
		if errors.Is(err, db.ErrNotFound) {
			return Owner{}, fmt.Errorf("no such user %q", requested)
		}
		if err != nil {
			return Owner{}, err
		}
		target = u
	}
	if target.LinuxUID == nil || *target.LinuxUID == 0 {
		return Owner{}, ErrNoAccount
	}
	return Owner{ID: target.ID, Username: target.Username}, nil
}

// CanReach reports whether the actor may act on an existing backup row.
func CanReach(actor *db.User, b *db.Backup) bool {
	return b.OwnerID == actor.ID || auth.Role(actor.Role).AtLeast(auth.RoleReseller)
}

// Request describes a backup to take.
type Request struct {
	Owner            Owner
	IncludeFiles     bool
	IncludeDatabases bool
	Kind             string
}

// Start records a backup and runs it in the background, returning the row
// immediately.
//
// Asynchronous because an archive of a real account takes minutes and an HTTP
// request that waits that long is a request that a proxy, a laptop lid or an
// impatient customer will kill halfway through.
func (s *Service) Start(ctx context.Context, req Request) (*db.Backup, error) {
	if !req.IncludeFiles && !req.IncludeDatabases {
		return nil, errors.New("choose the files, the databases, or both")
	}
	if req.Kind == "" {
		req.Kind = db.BackupManual
	}

	dbs, err := s.ownerDatabases(ctx, req.Owner.ID, req.IncludeDatabases)
	if err != nil {
		return nil, err
	}
	if !req.IncludeFiles && len(dbs) == 0 {
		return nil, errors.New("this account has no databases to back up")
	}

	sites, err := s.ownerSites(ctx, req.Owner.ID)
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	if s.running[req.Owner.ID] {
		s.mu.Unlock()
		return nil, ErrBusy
	}
	s.running[req.Owner.ID] = true
	s.mu.Unlock()

	row, err := s.db.CreateBackup(ctx, &db.Backup{
		OwnerID: req.Owner.ID, Filename: archiveName(req.Owner.Username),
		Kind: req.Kind, Databases: dbs, HasFiles: req.IncludeFiles,
	})
	if err != nil {
		s.release(req.Owner.ID)
		return nil, err
	}

	go s.run(row, req, dbs, sites)
	return row, nil
}

func (s *Service) release(ownerID int64) {
	s.mu.Lock()
	delete(s.running, ownerID)
	s.mu.Unlock()
}

// run performs the archive and closes out the row. It takes its own context:
// the HTTP request that asked for the backup is long gone by the time this
// finishes, and cancelling on it would abort every backup at the moment the
// browser tab closed.
func (s *Service) run(row *db.Backup, req Request, dbs []string, sites []backuparchive.ManifestSite) {
	defer s.release(req.Owner.ID)

	ctx, cancel := context.WithTimeout(context.Background(), AgentTimeout)
	defer cancel()

	res, err := agentclient.Call[actions.BackupResult](ctx, s.slow, "backup.create", 1,
		actions.BackupCreateRequest{
			Owner: req.Owner.Username, Name: row.Filename,
			IncludeFiles: req.IncludeFiles, Databases: dbs, Sites: sites,
		})

	msg := ""
	if err != nil {
		msg = err.Error()
		s.log.Error("backups: archive failed", "owner", req.Owner.Username, "err", err)
	} else {
		s.log.Info("backups: archive written", "owner", req.Owner.Username,
			"file", row.Filename, "bytes", res.SizeBytes, "files", res.FileCount)
	}

	finishCtx, finishCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer finishCancel()
	if ferr := s.db.FinishBackup(finishCtx, row.ID, res.SizeBytes, res.FileCount, res.SHA256, msg); ferr != nil {
		s.log.Error("backups: cannot record outcome", "id", row.ID, "err", ferr)
	}
}

// Restore unpacks an archive back over its account.
func (s *Service) Restore(ctx context.Context, b *db.Backup, files, databases bool) (actions.BackupRestoreResult, error) {
	if b.Status != db.BackupReady {
		return actions.BackupRestoreResult{}, fmt.Errorf("this backup is %s, not ready to restore", b.Status)
	}
	res, err := agentclient.Call[actions.BackupRestoreResult](ctx, s.slow, "backup.restore", 1,
		actions.BackupRestoreRequest{
			Owner: b.OwnerUsername, Name: b.Filename,
			RestoreFiles: files, RestoreDatabases: databases,
		})
	if err != nil {
		return res, err
	}
	if len(res.Databases) > 0 {
		s.regrant(ctx, b.OwnerID, res.Databases)
	}
	return res, nil
}

// regrant puts back the database permissions a restore dropped.
//
// The dump recreates each database from scratch, which is what makes a
// restore a true replacement -- and which takes the grants with it. The panel
// knows which accounts were meant to reach which databases, so it re-applies
// them; without this a site comes back with all its data and no way to read
// it, which looks exactly like a failed restore to the customer.
//
// A failure here is logged rather than returned: the data is already back,
// and reporting the whole restore as failed would be worse than reporting
// which grants need attention.
func (s *Service) regrant(ctx context.Context, ownerID int64, restored []string) {
	wanted := make(map[string]bool, len(restored))
	for _, n := range restored {
		wanted[n] = true
	}
	rows, err := s.db.ListDatabases(ctx, ownerID)
	if err != nil {
		s.log.Error("backups: cannot re-apply grants after restore", "err", err)
		return
	}
	for _, row := range rows {
		if !wanted[row.Name] {
			continue
		}
		for _, username := range row.Users {
			if _, err := agentclient.Call[struct{}](ctx, s.agent, "db.grant", 1,
				actions.GrantRequest{Username: username, Database: row.Name}); err != nil {
				s.log.Error("backups: cannot re-grant after restore",
					"database", row.Name, "user", username, "err", err)
				continue
			}
			s.log.Info("backups: re-applied grant after restore",
				"database", row.Name, "user", username)
		}
	}
}

// Delete removes the archive and then the row. In that order: a row without
// an archive shows the customer a backup that is not there, while an archive
// without a row only wastes disk, which a later reconcile can find.
func (s *Service) Delete(ctx context.Context, b *db.Backup) error {
	if b.Status == db.BackupRunning {
		return errors.New("this backup is still running")
	}
	_, err := agentclient.Call[struct{}](ctx, s.agent, "backup.delete", 1,
		actions.BackupPathRequest{Owner: b.OwnerUsername, Name: b.Filename})
	// A missing file is the state we wanted; carry on and drop the row.
	if err != nil && !strings.Contains(err.Error(), "no such file") {
		return err
	}
	return s.db.DeleteBackup(ctx, b.ID)
}

// Download stages an archive somewhere the API can read and opens it.
func (s *Service) Download(ctx context.Context, b *db.Backup) (*StagedFile, error) {
	if b.Status != db.BackupReady {
		return nil, fmt.Errorf("this backup is %s", b.Status)
	}
	res, err := agentclient.Call[actions.FileStageResult](ctx, s.slow, "backup.stage", 1,
		actions.BackupPathRequest{Owner: b.OwnerUsername, Name: b.Filename})
	if err != nil {
		return nil, err
	}
	f, err := os.Open(res.StagedPath)
	if err != nil {
		_ = os.Remove(res.StagedPath)
		return nil, err
	}
	return &StagedFile{File: f, Size: res.Size, Name: b.Filename}, nil
}

// StagedFile is an open copy of an archive. It deletes itself on Close.
type StagedFile struct {
	*os.File
	Size int64
	Name string
}

// Close closes and removes the staged copy.
func (f *StagedFile) Close() error {
	name := f.File.Name()
	err := f.File.Close()
	if rmErr := os.Remove(name); err == nil {
		err = rmErr
	}
	return err
}

// Upload takes an archive from outside and files it under an account.
//
// This is how a backup comes back after a server rebuild, and the first half
// of migrating an account between two OPanel hosts.
func (s *Service) Upload(ctx context.Context, owner Owner, src io.Reader, limit int64) (*db.Backup, error) {
	if err := os.MkdirAll(actions.UploadStageDir, 0o750); err != nil {
		return nil, fmt.Errorf("prepare upload directory: %w", err)
	}
	tmp, err := os.CreateTemp(actions.UploadStageDir, "backup-upload-*")
	if err != nil {
		return nil, err
	}
	staged := tmp.Name()
	defer func() { _ = os.Remove(staged) }()

	n, err := io.Copy(tmp, io.LimitReader(src, limit+1))
	if err != nil {
		_ = tmp.Close()
		return nil, err
	}
	if err := tmp.Close(); err != nil {
		return nil, err
	}
	if n > limit {
		return nil, fmt.Errorf("the archive is larger than the %d MB upload limit", limit>>20)
	}
	if err := os.Chmod(staged, 0o640); err != nil {
		return nil, err
	}

	name := archiveName(owner.Username)
	res, err := agentclient.Call[actions.BackupResult](ctx, s.slow, "backup.install", 1,
		actions.BackupInstallRequest{Owner: owner.Username, Name: name, StagedPath: staged})
	if err != nil {
		return nil, err
	}

	row, err := s.db.CreateBackup(ctx, &db.Backup{
		OwnerID: owner.ID, Filename: name, Kind: db.BackupUploaded, HasFiles: true,
	})
	if err != nil {
		return nil, err
	}
	if err := s.db.FinishBackup(ctx, row.ID, res.SizeBytes, 0, res.SHA256, ""); err != nil {
		return nil, err
	}
	return s.db.BackupByID(ctx, row.ID)
}

// Inspect reads an archive's manifest, which is how the UI shows what a
// restore would actually bring back before anyone commits to it.
func (s *Service) Inspect(ctx context.Context, b *db.Backup) (*backuparchive.Manifest, error) {
	return agentclient.Call[*backuparchive.Manifest](ctx, s.agent, "backup.inspect", 1,
		actions.BackupPathRequest{Owner: b.OwnerUsername, Name: b.Filename})
}

// ownerDatabases lists the databases to include, empty when they were not
// asked for.
func (s *Service) ownerDatabases(ctx context.Context, ownerID int64, include bool) ([]string, error) {
	if !include {
		return nil, nil
	}
	rows, err := s.db.ListDatabases(ctx, ownerID)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Name)
	}
	return out, nil
}

// ownerSites records the site rows in the manifest, so an account restored
// onto a fresh server does not lose which PHP version each site ran.
func (s *Service) ownerSites(ctx context.Context, ownerID int64) ([]backuparchive.ManifestSite, error) {
	rows, err := s.db.ListSites(ctx, ownerID)
	if err != nil {
		return nil, err
	}
	out := make([]backuparchive.ManifestSite, 0, len(rows))
	for _, r := range rows {
		out = append(out, backuparchive.ManifestSite{
			Domain: r.Domain, Aliases: r.Aliases, AppType: r.AppType,
			PHPVersion: r.PHPVersion, DocumentRoot: r.DocumentRoot,
			RewriteMode: r.RewriteMode,
		})
	}
	return out, nil
}

// safeName strips an account name down to what an archive file may contain.
// The account names the panel issues already satisfy this; the belt and
// braces is for names that predate a validation rule.
var safeName = regexp.MustCompile(`[^a-z0-9_-]+`)

// archiveName builds a file name that sorts chronologically and says at a
// glance whose it is.
func archiveName(owner string) string {
	clean := safeName.ReplaceAllString(strings.ToLower(owner), "-")
	return fmt.Sprintf("%s-%s%s", clean, time.Now().UTC().Format("20060102-150405"),
		backuparchive.Extension)
}
