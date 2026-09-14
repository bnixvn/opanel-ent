package actions

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/user"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/bnixvn/opanel-ent/internal/agent"
	"github.com/bnixvn/opanel-ent/internal/platform/linuxuser"
)

// File operations are confined with os.Root.
//
// os.Root (Go 1.24) resolves every path inside a directory in the kernel,
// refusing anything that escapes it — including through a symlink the
// customer created, which is the case hand-written path checks reliably get
// wrong. Every operation here opens the owner's home as a root and works
// inside it, so "../../etc/shadow" is not a string to be sanitised but a path
// the operating system declines to open.

// MaxEditBytes caps what the built-in editor will read or write. Large files
// belong in the upload and download paths, which stream.
const MaxEditBytes = 1 << 20 // 1 MiB

// FilePathRequest addresses a path inside an owner's home.
type FilePathRequest struct {
	Owner string `json:"owner"`
	// Path is relative to the owner's home. Empty means the home itself.
	Path string `json:"path"`
}

// Validate checks the owner and rejects an absolute path, which would be a
// caller mistake rather than something to reinterpret.
func (r *FilePathRequest) Validate() error {
	if !linuxuser.PlausibleName(r.Owner) {
		return fmt.Errorf("%q is not an acceptable account name", r.Owner)
	}
	return validRelPath(r.Path)
}

func validRelPath(p string) error {
	if p == "" {
		return nil
	}
	if path.IsAbs(p) {
		return fmt.Errorf("path %q must be relative to the account's home", p)
	}
	if strings.ContainsRune(p, 0) {
		return fmt.Errorf("path contains a null byte")
	}
	// os.Root would refuse these anyway; saying so here gives a better message
	// than an opaque "path escapes from parent".
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return fmt.Errorf("path %q may not contain %q", p, "..")
		}
	}
	return nil
}

// FileEntry is one item in a directory listing.
type FileEntry struct {
	Name    string    `json:"name"`
	Size    int64     `json:"size"`
	Mode    string    `json:"mode"`
	IsDir   bool      `json:"is_dir"`
	IsLink  bool      `json:"is_link"`
	ModTime time.Time `json:"mod_time"`
	Owner   string    `json:"owner,omitempty"`
}

// FileListResult is a directory listing.
type FileListResult struct {
	Path    string      `json:"path"`
	Entries []FileEntry `json:"entries"`
}

// FileReadResult carries the contents of a text file.
type FileReadResult struct {
	Path    string `json:"path"`
	Content string `json:"content"`
	Size    int64  `json:"size"`
	// Truncated is set when the file was larger than MaxEditBytes; the editor
	// must then refuse to save, or it would destroy the tail.
	Truncated bool `json:"truncated"`
}

// FileWriteRequest replaces a file's contents.
type FileWriteRequest struct {
	Owner   string `json:"owner"`
	Path    string `json:"path"`
	Content string `json:"content"`
}

// Validate checks the target and the size.
func (r *FileWriteRequest) Validate() error {
	if err := (&FilePathRequest{Owner: r.Owner, Path: r.Path}).Validate(); err != nil {
		return err
	}
	if r.Path == "" {
		return errors.New("a file name is required")
	}
	if len(r.Content) > MaxEditBytes {
		return fmt.Errorf("file is larger than %d bytes; use upload instead", MaxEditBytes)
	}
	return nil
}

// FileRenameRequest moves a path within the same home.
type FileRenameRequest struct {
	Owner string `json:"owner"`
	From  string `json:"from"`
	To    string `json:"to"`
}

// Validate checks both ends of the move.
func (r *FileRenameRequest) Validate() error {
	if err := (&FilePathRequest{Owner: r.Owner, Path: r.From}).Validate(); err != nil {
		return err
	}
	if err := validRelPath(r.To); err != nil {
		return err
	}
	if r.From == "" || r.To == "" {
		return errors.New("both a source and a destination are required")
	}
	return nil
}

// FileChmodRequest changes a path's permissions.
type FileChmodRequest struct {
	Owner string `json:"owner"`
	Path  string `json:"path"`
	Mode  string `json:"mode"` // octal, e.g. "0644"
}

// Validate parses the mode and rejects anything that grants setuid.
func (r *FileChmodRequest) Validate() error {
	if err := (&FilePathRequest{Owner: r.Owner, Path: r.Path}).Validate(); err != nil {
		return err
	}
	m, err := strconv.ParseUint(r.Mode, 8, 32)
	if err != nil {
		return fmt.Errorf("mode %q is not octal", r.Mode)
	}
	// setuid, setgid and sticky are never something a hosting customer needs
	// on their own files, and setuid on a file owned by them is a local root
	// escalation waiting for a careless administrator.
	if m&^0o777 != 0 {
		return fmt.Errorf("mode %q may only set permission bits", r.Mode)
	}
	return nil
}

// FileInstallRequest moves an already-staged upload into place.
type FileInstallRequest struct {
	Owner string `json:"owner"`
	Path  string `json:"path"`
	// StagedPath is a file the API wrote under its own directory. The agent
	// reads it, writes the destination as the owner, and removes the staged
	// copy.
	StagedPath string `json:"staged_path"`
}

// Validate checks the destination and that the staged file is where the API
// is allowed to put things.
func (r *FileInstallRequest) Validate() error {
	if err := (&FilePathRequest{Owner: r.Owner, Path: r.Path}).Validate(); err != nil {
		return err
	}
	if r.Path == "" {
		return errors.New("a destination name is required")
	}
	// The staged path comes from the API process, which is unprivileged but
	// still worth constraining: a bug there must not turn into "the agent
	// will copy any file on the host into a customer's web root".
	if !strings.HasPrefix(r.StagedPath, UploadStageDir+"/") || strings.Contains(r.StagedPath, "..") {
		return fmt.Errorf("staged file must be under %s", UploadStageDir)
	}
	return nil
}

// UploadStageDir is where the API writes an upload before the agent installs
// it. Owned by the API account, which is the only thing that writes here.
const UploadStageDir = "/var/lib/opanel/uploads"

// FileStageResult tells the API where it may read a file it cannot open
// directly.
type FileStageResult struct {
	StagedPath string `json:"staged_path"`
	Size       int64  `json:"size"`
}

// openHome returns an os.Root confined to the account's home directory.
func openHome(owner string) (*os.Root, *linuxuser.Account, error) {
	acct, err := linuxuser.Lookup(owner)
	if err != nil {
		return nil, nil, err
	}
	if acct == nil {
		return nil, nil, fmt.Errorf("account %q does not exist", owner)
	}
	root, err := os.OpenRoot(acct.Home)
	if err != nil {
		return nil, nil, fmt.Errorf("open %s: %w", acct.Home, err)
	}
	return root, acct, nil
}

// rel turns an empty path into the current directory, which is what os.Root
// expects for the root itself.
func rel(p string) string {
	if p == "" {
		return "."
	}
	return p
}

func registerFiles(r *agent.Registry) {
	agent.Register(r, "fs.list", 1, func(_ context.Context, in FilePathRequest) (FileListResult, error) {
		root, _, err := openHome(in.Owner)
		if err != nil {
			return FileListResult{}, err
		}
		defer func() { _ = root.Close() }()

		f, err := root.Open(rel(in.Path))
		if err != nil {
			return FileListResult{}, err
		}
		defer func() { _ = f.Close() }()

		names, err := f.Readdirnames(-1)
		if err != nil {
			return FileListResult{}, err
		}
		sort.Strings(names)

		out := FileListResult{Path: in.Path, Entries: make([]FileEntry, 0, len(names))}
		for _, name := range names {
			child := path.Join(rel(in.Path), name)
			// Lstat, not Stat: a symlink is shown as a link rather than
			// silently reported as whatever it points at.
			info, err := root.Lstat(child)
			if err != nil {
				continue // vanished between readdir and stat
			}
			e := FileEntry{
				Name:    name,
				Size:    info.Size(),
				Mode:    fmt.Sprintf("%04o", info.Mode().Perm()),
				IsDir:   info.IsDir(),
				IsLink:  info.Mode()&os.ModeSymlink != 0,
				ModTime: info.ModTime(),
			}
			e.Owner = ownerName(info)
			out.Entries = append(out.Entries, e)
		}
		return out, nil
	})

	agent.Register(r, "fs.read", 1, func(_ context.Context, in FilePathRequest) (FileReadResult, error) {
		root, _, err := openHome(in.Owner)
		if err != nil {
			return FileReadResult{}, err
		}
		defer func() { _ = root.Close() }()

		f, err := root.Open(rel(in.Path))
		if err != nil {
			return FileReadResult{}, err
		}
		defer func() { _ = f.Close() }()

		info, err := f.Stat()
		if err != nil {
			return FileReadResult{}, err
		}
		if info.IsDir() {
			return FileReadResult{}, fmt.Errorf("%s is a directory", in.Path)
		}
		data, err := io.ReadAll(io.LimitReader(f, MaxEditBytes))
		if err != nil {
			return FileReadResult{}, err
		}
		return FileReadResult{
			Path: in.Path, Content: string(data), Size: info.Size(),
			Truncated: info.Size() > int64(len(data)),
		}, nil
	})

	agent.Register(r, "fs.write", 1, func(_ context.Context, in FileWriteRequest) (struct{}, error) {
		root, acct, err := openHome(in.Owner)
		if err != nil {
			return struct{}{}, err
		}
		defer func() { _ = root.Close() }()
		return struct{}{}, writeInRoot(root, acct, in.Path, []byte(in.Content))
	})

	agent.Register(r, "fs.mkdir", 1, func(_ context.Context, in FilePathRequest) (struct{}, error) {
		root, acct, err := openHome(in.Owner)
		if err != nil {
			return struct{}{}, err
		}
		defer func() { _ = root.Close() }()
		if in.Path == "" {
			return struct{}{}, errors.New("a directory name is required")
		}
		if err := root.Mkdir(in.Path, 0o755); err != nil {
			return struct{}{}, err
		}
		return struct{}{}, root.Chown(in.Path, int(acct.UID), int(acct.GID))
	})

	agent.Register(r, "fs.delete", 1, func(_ context.Context, in FilePathRequest) (struct{}, error) {
		root, _, err := openHome(in.Owner)
		if err != nil {
			return struct{}{}, err
		}
		defer func() { _ = root.Close() }()
		if in.Path == "" {
			return struct{}{}, errors.New("refusing to delete the home directory itself")
		}
		// RemoveAll on the root handle, so a directory tree goes in one call
		// while still being unable to reach outside the home.
		return struct{}{}, root.RemoveAll(in.Path)
	})

	agent.Register(r, "fs.rename", 1, func(_ context.Context, in FileRenameRequest) (struct{}, error) {
		root, _, err := openHome(in.Owner)
		if err != nil {
			return struct{}{}, err
		}
		defer func() { _ = root.Close() }()
		return struct{}{}, root.Rename(in.From, in.To)
	})

	agent.Register(r, "fs.chmod", 1, func(_ context.Context, in FileChmodRequest) (struct{}, error) {
		root, _, err := openHome(in.Owner)
		if err != nil {
			return struct{}{}, err
		}
		defer func() { _ = root.Close() }()
		m, _ := strconv.ParseUint(in.Mode, 8, 32)
		return struct{}{}, root.Chmod(rel(in.Path), os.FileMode(m))
	})

	agent.Register(r, "fs.install", 1, func(_ context.Context, in FileInstallRequest) (struct{}, error) {
		root, acct, err := openHome(in.Owner)
		if err != nil {
			return struct{}{}, err
		}
		defer func() { _ = root.Close() }()

		data, err := os.ReadFile(in.StagedPath)
		if err != nil {
			return struct{}{}, fmt.Errorf("read staged upload: %w", err)
		}
		defer func() { _ = os.Remove(in.StagedPath) }()
		return struct{}{}, writeInRoot(root, acct, in.Path, data)
	})

	agent.Register(r, "fs.stage", 1, func(_ context.Context, in FilePathRequest) (FileStageResult, error) {
		root, _, err := openHome(in.Owner)
		if err != nil {
			return FileStageResult{}, err
		}
		defer func() { _ = root.Close() }()

		src, err := root.Open(rel(in.Path))
		if err != nil {
			return FileStageResult{}, err
		}
		defer func() { _ = src.Close() }()
		info, err := src.Stat()
		if err != nil {
			return FileStageResult{}, err
		}
		if info.IsDir() {
			return FileStageResult{}, fmt.Errorf("%s is a directory", in.Path)
		}

		// Staged rather than streamed: the API runs as a different account
		// and cannot open a customer's file directly, and the request
		// protocol carries JSON rather than bytes.
		if err := os.MkdirAll(UploadStageDir, 0o750); err != nil {
			return FileStageResult{}, err
		}
		if err := chownToAPI(UploadStageDir); err != nil {
			return FileStageResult{}, err
		}
		tmp, err := os.CreateTemp(UploadStageDir, "download-*")
		if err != nil {
			return FileStageResult{}, err
		}
		defer func() { _ = tmp.Close() }()
		if _, err := io.Copy(tmp, src); err != nil {
			_ = os.Remove(tmp.Name())
			return FileStageResult{}, err
		}
		if err := chownToAPI(tmp.Name()); err != nil {
			_ = os.Remove(tmp.Name())
			return FileStageResult{}, err
		}
		return FileStageResult{StagedPath: tmp.Name(), Size: info.Size()}, nil
	})
}

// writeInRoot writes a file inside the root and gives it to the account, so
// a file the panel created is indistinguishable from one uploaded over SFTP.
func writeInRoot(root *os.Root, acct *linuxuser.Account, name string, data []byte) error {
	f, err := root.OpenFile(name, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return root.Chown(name, int(acct.UID), int(acct.GID))
}

// chownToAPI hands a staged file to the account the API runs as.
func chownToAPI(p string) error {
	u, err := user.Lookup(PanelServiceUser)
	if err != nil {
		return err
	}
	uid, _ := strconv.Atoi(u.Uid)
	gid, _ := strconv.Atoi(u.Gid)
	return os.Chown(p, uid, gid)
}

// ownerName resolves the owning account of a listed file, falling back to the
// numeric id when there is no passwd entry.
func ownerName(info os.FileInfo) string {
	uid, ok := statUID(info)
	if !ok {
		return ""
	}
	if u, err := user.LookupId(strconv.FormatUint(uint64(uid), 10)); err == nil {
		return u.Username
	}
	return strconv.FormatUint(uint64(uid), 10)
}
