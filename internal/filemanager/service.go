// Package filemanager browses and edits the files a hosting account owns.
//
// Nothing here touches the filesystem directly. Every operation is an agent
// action, so containment is enforced once, in one place, by os.Root in the
// agent — rather than by path checks scattered across an HTTP handler where
// a missed case is a customer reading /etc/shadow.
package filemanager

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path"
	"strings"

	"github.com/bnixvn/opanel-ent/internal/agent/actions"
	"github.com/bnixvn/opanel-ent/internal/agentclient"
	"github.com/bnixvn/opanel-ent/internal/auth"
	"github.com/bnixvn/opanel-ent/internal/db"
)

// ErrNoAccount means the panel user has no Linux account, so there are no
// files to browse. Staff accounts are the usual case.
var ErrNoAccount = errors.New("filemanager: this account has no home directory on the server")

// ErrForbidden means the caller may not browse the requested account.
var ErrForbidden = errors.New("filemanager: not permitted to browse that account")

// Service resolves who may browse what and forwards the rest to the agent.
type Service struct {
	db    *db.DB
	agent *agentclient.Client
	log   *slog.Logger
}

// New builds the service.
func New(database *db.DB, ac *agentclient.Client, log *slog.Logger) *Service {
	if log == nil {
		log = slog.Default()
	}
	return &Service{db: database, agent: ac, log: log}
}

// Owner names the account whose files an operation applies to.
type Owner struct {
	UserID   int64
	Username string
}

// ResolveOwner decides whose files the caller is asking for.
//
// An end user may only ever reach their own home; the request cannot name
// another account, so there is no ownership check to forget. Staff may name
// one, and get their own when they do not.
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
	return Owner{UserID: target.ID, Username: target.Username}, nil
}

// List returns one directory's contents.
func (s *Service) List(ctx context.Context, owner Owner, dir string) (actions.FileListResult, error) {
	return agentclient.Call[actions.FileListResult](ctx, s.agent, "fs.list", 1,
		actions.FilePathRequest{Owner: owner.Username, Path: clean(dir)})
}

// Read returns a text file's contents, up to the editor's size cap.
func (s *Service) Read(ctx context.Context, owner Owner, p string) (actions.FileReadResult, error) {
	return agentclient.Call[actions.FileReadResult](ctx, s.agent, "fs.read", 1,
		actions.FilePathRequest{Owner: owner.Username, Path: clean(p)})
}

// Write replaces a file's contents.
func (s *Service) Write(ctx context.Context, owner Owner, p, content string) error {
	_, err := agentclient.Call[struct{}](ctx, s.agent, "fs.write", 1,
		actions.FileWriteRequest{Owner: owner.Username, Path: clean(p), Content: content})
	return err
}

// Mkdir creates a directory.
func (s *Service) Mkdir(ctx context.Context, owner Owner, p string) error {
	_, err := agentclient.Call[struct{}](ctx, s.agent, "fs.mkdir", 1,
		actions.FilePathRequest{Owner: owner.Username, Path: clean(p)})
	return err
}

// Delete removes a file or a directory tree.
func (s *Service) Delete(ctx context.Context, owner Owner, p string) error {
	_, err := agentclient.Call[struct{}](ctx, s.agent, "fs.delete", 1,
		actions.FilePathRequest{Owner: owner.Username, Path: clean(p)})
	return err
}

// Rename moves a path within the same home.
func (s *Service) Rename(ctx context.Context, owner Owner, from, to string) error {
	_, err := agentclient.Call[struct{}](ctx, s.agent, "fs.rename", 1,
		actions.FileRenameRequest{Owner: owner.Username, From: clean(from), To: clean(to)})
	return err
}

// Chmod changes a path's permission bits.
func (s *Service) Chmod(ctx context.Context, owner Owner, p, mode string) error {
	_, err := agentclient.Call[struct{}](ctx, s.agent, "fs.chmod", 1,
		actions.FileChmodRequest{Owner: owner.Username, Path: clean(p), Mode: mode})
	return err
}

// Download copies a file somewhere the API can read, and returns an open
// handle to it. Closing the returned file removes the copy.
//
// The copy exists because the API runs as its own unprivileged account and
// cannot open a customer's file. Streaming the bytes back through the agent
// protocol instead would mean base64 in a JSON message, which costs a third
// again in size and holds the whole file in memory twice.
func (s *Service) Download(ctx context.Context, owner Owner, p string) (*StagedFile, error) {
	res, err := agentclient.Call[actions.FileStageResult](ctx, s.agent, "fs.stage", 1,
		actions.FilePathRequest{Owner: owner.Username, Path: clean(p)})
	if err != nil {
		return nil, err
	}
	f, err := os.Open(res.StagedPath)
	if err != nil {
		_ = os.Remove(res.StagedPath)
		return nil, err
	}
	return &StagedFile{File: f, Size: res.Size, Name: path.Base(clean(p))}, nil
}

// StagedFile is an open copy of a customer's file. It deletes itself on Close.
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

// ErrTooLarge means the upload exceeded the limit the caller set.
var ErrTooLarge = errors.New("filemanager: the file is larger than the upload limit")

// Upload stages an incoming file and asks the agent to install it as the
// owner. The staged copy is removed whether or not the install succeeds.
//
// limit is checked while copying rather than from a Content-Length header,
// which a client is free to understate.
func (s *Service) Upload(ctx context.Context, owner Owner, dest string, src io.Reader, limit int64) error {
	if err := os.MkdirAll(actions.UploadStageDir, 0o750); err != nil {
		return fmt.Errorf("prepare upload directory: %w", err)
	}
	tmp, err := os.CreateTemp(actions.UploadStageDir, "upload-*")
	if err != nil {
		return err
	}
	staged := tmp.Name()
	// Removed here as well as in the agent: the agent only gets the chance
	// when the call reaches it, and a client that hangs up mid-body must not
	// leave the panel's state directory filling with half-written uploads.
	defer func() { _ = os.Remove(staged) }()

	n, err := io.Copy(tmp, io.LimitReader(src, limit+1))
	if err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if n > limit {
		return ErrTooLarge
	}
	// The agent reads this as root, so mode is about the other accounts on
	// the host, not about the agent.
	if err := os.Chmod(staged, 0o640); err != nil {
		return err
	}

	_, err = agentclient.Call[struct{}](ctx, s.agent, "fs.install", 1,
		actions.FileInstallRequest{Owner: owner.Username, Path: clean(dest), StagedPath: staged})
	return err
}

// clean normalises a path from the browser into the relative form the agent
// expects. It is a convenience, not a security boundary: os.Root in the agent
// is what actually refuses an escape, and it does so even for a path this
// function would happily pass through.
func clean(p string) string {
	p = strings.TrimSpace(p)
	p = strings.TrimPrefix(p, "/")
	if p == "" || p == "." {
		return ""
	}
	return path.Clean(p)
}
