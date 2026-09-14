package actions

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/bnixvn/opanel-ent/internal/agent"
	"github.com/bnixvn/opanel-ent/internal/backuparchive"
	"github.com/bnixvn/opanel-ent/internal/dbms"
	"github.com/bnixvn/opanel-ent/internal/platform/linuxuser"
	"github.com/bnixvn/opanel-ent/internal/version"
)

// BackupRoot is where archives live: outside every customer's home, so a
// compromised site cannot read or delete the backups that would undo it, and
// so an account's own backups do not count against its disk quota.
//
// Not under /var/lib/opanel, even though that is where the panel keeps
// everything else. That path is the API service's systemd StateDirectory, and
// systemd takes ownership of the whole tree when the service starts -- which
// silently handed the unprivileged API account read and delete access to
// every customer's backups. Here they stay root-owned.
const BackupRoot = "/var/backups/opanel"

// BackupBudget is how long an archive or a restore may run. Generous because
// the alternative -- a backup that dies at two minutes -- is a backup that
// silently does not exist when it is needed.
const BackupBudget = 2 * time.Hour

// backupNamePattern is what an archive file may be called. Deliberately
// narrow: these names travel from the database into exec arguments and file
// paths, and the cost of being strict is nothing.
var backupNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{2,120}$`)

// BackupCreateRequest asks for an archive of one account.
type BackupCreateRequest struct {
	Owner string `json:"owner"`
	// Name is the file to write under the owner's backup directory,
	// including the extension.
	Name         string                       `json:"name"`
	IncludeFiles bool                         `json:"include_files"`
	Databases    []string                     `json:"databases"`
	Sites        []backuparchive.ManifestSite `json:"sites"`
}

// Validate checks the account, the file name and the database names.
func (r *BackupCreateRequest) Validate() error {
	if !linuxuser.PlausibleName(r.Owner) {
		return fmt.Errorf("%q is not an acceptable account name", r.Owner)
	}
	if !backupNamePattern.MatchString(r.Name) || !strings.HasSuffix(r.Name, backuparchive.Extension) {
		return fmt.Errorf("%q is not an acceptable backup file name", r.Name)
	}
	if !r.IncludeFiles && len(r.Databases) == 0 {
		return errors.New("a backup must include the files, some databases, or both")
	}
	for _, d := range r.Databases {
		if !dbms.ValidDatabaseName(d) {
			return fmt.Errorf("%q is not an acceptable database name", d)
		}
	}
	return nil
}

// BackupResult reports what was written.
type BackupResult struct {
	Path      string `json:"path"`
	SizeBytes int64  `json:"size_bytes"`
	FileCount int64  `json:"file_count"`
	FileBytes int64  `json:"file_bytes"`
	SHA256    string `json:"sha256"`
}

// BackupPathRequest names an existing archive.
type BackupPathRequest struct {
	Owner string `json:"owner"`
	Name  string `json:"name"`
}

// Validate checks the account and the file name.
func (r *BackupPathRequest) Validate() error {
	if !linuxuser.PlausibleName(r.Owner) {
		return fmt.Errorf("%q is not an acceptable account name", r.Owner)
	}
	if !backupNamePattern.MatchString(r.Name) {
		return fmt.Errorf("%q is not an acceptable backup file name", r.Name)
	}
	return nil
}

// BackupRestoreRequest unpacks an archive back over an account.
type BackupRestoreRequest struct {
	Owner            string `json:"owner"`
	Name             string `json:"name"`
	RestoreFiles     bool   `json:"restore_files"`
	RestoreDatabases bool   `json:"restore_databases"`
}

// Validate checks the account, the file name and that something was asked for.
func (r *BackupRestoreRequest) Validate() error {
	if err := (&BackupPathRequest{Owner: r.Owner, Name: r.Name}).Validate(); err != nil {
		return err
	}
	if !r.RestoreFiles && !r.RestoreDatabases {
		return errors.New("a restore must bring back the files, the databases, or both")
	}
	return nil
}

// BackupRestoreResult reports what came back.
type BackupRestoreResult struct {
	Manifest  *backuparchive.Manifest `json:"manifest"`
	Files     int64                   `json:"files"`
	Databases []string                `json:"databases"`
}

// BackupInstallRequest moves an uploaded archive into the backup directory.
type BackupInstallRequest struct {
	Owner      string `json:"owner"`
	Name       string `json:"name"`
	StagedPath string `json:"staged_path"`
}

// Validate checks the destination and that the staged file is where the API
// is allowed to put things.
func (r *BackupInstallRequest) Validate() error {
	if err := (&BackupPathRequest{Owner: r.Owner, Name: r.Name}).Validate(); err != nil {
		return err
	}
	if !strings.HasSuffix(r.Name, backuparchive.Extension) {
		return fmt.Errorf("a backup file must end in %s", backuparchive.Extension)
	}
	if !strings.HasPrefix(r.StagedPath, UploadStageDir+"/") || strings.Contains(r.StagedPath, "..") {
		return fmt.Errorf("staged file must be under %s", UploadStageDir)
	}
	return nil
}

// backupPath resolves an archive's location. Both components are validated
// before this is called, so the join cannot escape the root.
func backupPath(owner, name string) string {
	return filepath.Join(BackupRoot, owner, name)
}

// BackupPath is where an account's archive lives. Exported so the panel can
// name a file for an upload without rebuilding the layout and getting it
// wrong -- which is exactly what happened the first time.
func BackupPath(owner, name string) string {
	return backupPath(owner, name)
}

func registerBackup(r *agent.Registry) {
	agent.RegisterSlow(r, "backup.create", 1, BackupBudget, createBackup)
	agent.RegisterSlow(r, "backup.restore", 1, BackupBudget, restoreBackup)

	agent.Register(r, "backup.delete", 1, func(_ context.Context, in BackupPathRequest) (struct{}, error) {
		return struct{}{}, os.Remove(backupPath(in.Owner, in.Name))
	})

	agent.Register(r, "backup.inspect", 1, func(_ context.Context, in BackupPathRequest) (*backuparchive.Manifest, error) {
		f, err := os.Open(backupPath(in.Owner, in.Name))
		if err != nil {
			return nil, err
		}
		defer func() { _ = f.Close() }()
		m, _, err := readManifest(f)
		return m, err
	})

	// Staging, for the same reason the file manager needs it: the API runs
	// as an unprivileged account and cannot open a root-owned archive.
	agent.Register(r, "backup.stage", 1, func(_ context.Context, in BackupPathRequest) (FileStageResult, error) {
		src, err := os.Open(backupPath(in.Owner, in.Name))
		if err != nil {
			return FileStageResult{}, err
		}
		defer func() { _ = src.Close() }()
		info, err := src.Stat()
		if err != nil {
			return FileStageResult{}, err
		}
		if err := os.MkdirAll(UploadStageDir, 0o750); err != nil {
			return FileStageResult{}, err
		}
		if err := chownToAPI(UploadStageDir); err != nil {
			return FileStageResult{}, err
		}
		tmp, err := os.CreateTemp(UploadStageDir, "backup-*")
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

	agent.Register(r, "backup.install", 1, func(_ context.Context, in BackupInstallRequest) (BackupResult, error) {
		defer func() { _ = os.Remove(in.StagedPath) }()

		dir := filepath.Join(BackupRoot, in.Owner)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return BackupResult{}, err
		}
		dest := backupPath(in.Owner, in.Name)

		src, err := os.Open(in.StagedPath)
		if err != nil {
			return BackupResult{}, err
		}
		defer func() { _ = src.Close() }()

		// Verified before it is accepted: an archive that turns out to be a
		// photograph should fail at upload, not months later during the
		// restore somebody is depending on.
		if _, _, err := readManifest(src); err != nil {
			return BackupResult{}, err
		}
		if _, err := src.Seek(0, io.SeekStart); err != nil {
			return BackupResult{}, err
		}

		out, err := os.OpenFile(dest, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return BackupResult{}, err
		}
		sum := sha256.New()
		n, err := io.Copy(io.MultiWriter(out, sum), src)
		if cerr := out.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			_ = os.Remove(dest)
			return BackupResult{}, err
		}
		return BackupResult{Path: dest, SizeBytes: n, SHA256: hex.EncodeToString(sum.Sum(nil))}, nil
	})
}

func createBackup(ctx context.Context, in BackupCreateRequest) (BackupResult, error) {
	acct, err := linuxuser.Lookup(in.Owner)
	if err != nil {
		return BackupResult{}, err
	}
	if acct == nil {
		return BackupResult{}, fmt.Errorf("account %q does not exist", in.Owner)
	}

	dir := filepath.Join(BackupRoot, in.Owner)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return BackupResult{}, err
	}
	dest := backupPath(in.Owner, in.Name)

	// Written to a temporary name and renamed at the end, so a backup
	// interrupted by a reboot or a full disk leaves nothing that looks like
	// a usable archive.
	tmp, err := os.CreateTemp(dir, ".partial-*")
	if err != nil {
		return BackupResult{}, err
	}
	partial := tmp.Name()
	committed := false
	defer func() {
		_ = tmp.Close()
		if !committed {
			_ = os.Remove(partial)
		}
	}()

	sum := sha256.New()
	// gzip rather than zstd: no dependency, and an operator can open the
	// result with tar on any machine in the world, including one that is not
	// running this panel. Backups get restored on bad days.
	gz := gzip.NewWriter(io.MultiWriter(tmp, sum))
	tw := tar.NewWriter(gz)

	man := backuparchive.Manifest{
		Format:    backuparchive.FormatVersion,
		Panel:     version.String(),
		CreatedAt: time.Now().UTC(),
		Owner:     in.Owner,
		Home:      acct.Home,
		Sites:     in.Sites,
		Databases: in.Databases,
	}
	// First member, so backup.inspect can answer "whose is this and what is
	// in it" without decompressing the whole archive.
	if err := writeManifest(tw, &man); err != nil {
		return BackupResult{}, err
	}

	// Databases next: they are small, and getting them out of the way before
	// a long file walk means a truncated archive is more likely to still
	// have them.
	var fileCount, fileBytes int64
	for _, name := range in.Databases {
		if err := ctx.Err(); err != nil {
			return BackupResult{}, err
		}
		if err := dumpDatabaseInto(ctx, tw, dir, name); err != nil {
			return BackupResult{}, fmt.Errorf("dump database %s: %w", name, err)
		}
	}

	if in.IncludeFiles {
		fileCount, fileBytes, err = archiveHome(ctx, tw, acct.Home)
		if err != nil {
			return BackupResult{}, err
		}
	}

	if err := tw.Close(); err != nil {
		return BackupResult{}, err
	}
	if err := gz.Close(); err != nil {
		return BackupResult{}, err
	}
	if err := tmp.Sync(); err != nil {
		return BackupResult{}, err
	}
	info, err := tmp.Stat()
	if err != nil {
		return BackupResult{}, err
	}
	if err := tmp.Close(); err != nil {
		return BackupResult{}, err
	}
	if err := os.Chmod(partial, 0o600); err != nil {
		return BackupResult{}, err
	}
	if err := os.Rename(partial, dest); err != nil {
		return BackupResult{}, err
	}
	committed = true

	return BackupResult{
		Path: dest, SizeBytes: info.Size(),
		FileCount: fileCount, FileBytes: fileBytes,
		SHA256: hex.EncodeToString(sum.Sum(nil)),
	}, nil
}

// writeManifest writes the manifest as the archive's first member.
func writeManifest(tw *tar.Writer, m *backuparchive.Manifest) error {
	var buf strings.Builder
	if err := m.Encode(&buf); err != nil {
		return err
	}
	body := buf.String()
	if err := tw.WriteHeader(&tar.Header{
		Name: backuparchive.ManifestName, Mode: 0o600,
		Size: int64(len(body)), ModTime: m.CreatedAt, Typeflag: tar.TypeReg,
	}); err != nil {
		return err
	}
	_, err := io.WriteString(tw, body)
	return err
}

// archiveHome walks an account's home and writes it into the tar.
//
// Symlinks are stored as symlinks, never followed: a customer's link to /etc
// must not pull the host's configuration into their backup, and a restore
// that recreated the link is what they actually had.
func archiveHome(ctx context.Context, tw *tar.Writer, home string) (count, total int64, err error) {
	err = filepath.WalkDir(home, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			// One unreadable file must not lose the whole backup; the
			// manifest counts will show something was skipped.
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		rel, relErr := filepath.Rel(home, p)
		if relErr != nil || rel == "." {
			return nil
		}
		rel = filepath.ToSlash(rel)

		info, infoErr := d.Info()
		if infoErr != nil {
			return nil
		}
		switch {
		case d.IsDir():
			return tw.WriteHeader(&tar.Header{
				Name: backuparchive.FilesPrefix + rel + "/", Mode: int64(info.Mode().Perm()),
				ModTime: info.ModTime(), Typeflag: tar.TypeDir,
			})

		case info.Mode()&os.ModeSymlink != 0:
			target, lerr := os.Readlink(p)
			if lerr != nil {
				return nil
			}
			return tw.WriteHeader(&tar.Header{
				Name: backuparchive.FilesPrefix + rel, Linkname: target,
				Mode: 0o777, ModTime: info.ModTime(), Typeflag: tar.TypeSymlink,
			})

		case info.Mode().IsRegular():
			f, oerr := os.Open(p)
			if oerr != nil {
				return nil
			}
			defer func() { _ = f.Close() }()
			if err := tw.WriteHeader(&tar.Header{
				Name: backuparchive.FilesPrefix + rel, Mode: int64(info.Mode().Perm()),
				Size: info.Size(), ModTime: info.ModTime(), Typeflag: tar.TypeReg,
			}); err != nil {
				return err
			}
			// io.CopyN, not io.Copy: a file that grew since the stat would
			// otherwise write more bytes than the header promised and
			// corrupt the whole archive from that point on.
			n, cerr := io.CopyN(tw, f, info.Size())
			if errors.Is(cerr, io.EOF) {
				// It shrank instead. Pad to the promised length rather than
				// leave the stream short.
				_, cerr = tw.Write(make([]byte, info.Size()-n))
			}
			if cerr != nil {
				return cerr
			}
			count++
			total += info.Size()
			return nil

		default:
			// Sockets, fifos and devices. A PHP-FPM socket in a home
			// directory is not something a restore should recreate.
			return nil
		}
	})
	return count, total, err
}

// dumpDatabaseInto dumps one database to a temporary file and appends it.
//
// Via a file because tar needs the length in the header before the body, and
// a dump's length is not known until it is finished.
func dumpDatabaseInto(ctx context.Context, tw *tar.Writer, workDir, name string) error {
	tmp, err := os.CreateTemp(workDir, ".dump-*")
	if err != nil {
		return err
	}
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
	}()

	var stderr strings.Builder
	cmd := exec.CommandContext(ctx, "mariadb-dump",
		"--single-transaction", // a consistent snapshot without locking the site out
		"--quick",
		"--routines", "--events", "--triggers",
		// The dump drops and recreates the database, so a restore puts it
		// back exactly as it was. Without this a restore is a merge: tables
		// created after the backup survive it, and somebody restoring to
		// undo a bad migration would find the new tables still there.
		//
		// Dropping the database also drops the grants on it, which is why
		// the service re-applies them from the panel's own records once the
		// restore finishes.
		"--add-drop-database",
		"--default-character-set=utf8mb4",
		"--databases", name)
	cmd.Stdout = tmp
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return err
	}
	info, err := tmp.Stat()
	if err != nil {
		return err
	}
	if err := tw.WriteHeader(&tar.Header{
		Name: backuparchive.DatabasesPrefix + name + ".sql", Mode: 0o600,
		Size: info.Size(), ModTime: time.Now(), Typeflag: tar.TypeReg,
	}); err != nil {
		return err
	}
	_, err = io.Copy(tw, tmp)
	return err
}

// readManifest scans an archive for its manifest, returning it along with a
// reader positioned wherever the scan stopped.
func readManifest(r io.Reader) (*backuparchive.Manifest, *tar.Reader, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return nil, nil, fmt.Errorf("this file is not a gzip archive: %w", err)
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil, nil, errors.New("this archive has no manifest; it was not written by OPanel")
		}
		if err != nil {
			return nil, nil, fmt.Errorf("read archive: %w", err)
		}
		if h.Name != backuparchive.ManifestName {
			continue
		}
		m, err := backuparchive.DecodeManifest(tr)
		if err != nil {
			return nil, nil, err
		}
		if err := m.Validate(); err != nil {
			return nil, nil, err
		}
		return m, tr, nil
	}
}

func restoreBackup(ctx context.Context, in BackupRestoreRequest) (BackupRestoreResult, error) {
	acct, err := linuxuser.Lookup(in.Owner)
	if err != nil {
		return BackupRestoreResult{}, err
	}
	if acct == nil {
		return BackupRestoreResult{}, fmt.Errorf("account %q does not exist", in.Owner)
	}

	archivePath := backupPath(in.Owner, in.Name)
	f, err := os.Open(archivePath)
	if err != nil {
		return BackupRestoreResult{}, err
	}
	defer func() { _ = f.Close() }()

	man, _, err := readManifest(f)
	if err != nil {
		return BackupRestoreResult{}, err
	}
	// The archive says whose it is. Unpacking one customer's files into
	// another's home would be a data breach performed by the panel itself.
	if man.Owner != in.Owner {
		return BackupRestoreResult{}, fmt.Errorf(
			"this archive belongs to %q, not %q", man.Owner, in.Owner)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return BackupRestoreResult{}, err
	}

	// Containment on the way in, the same guarantee the file manager gets:
	// an archive member named ../../etc/cron.d/x is refused by the kernel,
	// not by a check that has to anticipate it.
	root, err := os.OpenRoot(acct.Home)
	if err != nil {
		return BackupRestoreResult{}, err
	}
	defer func() { _ = root.Close() }()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return BackupRestoreResult{}, err
	}
	tr := tar.NewReader(gz)

	out := BackupRestoreResult{Manifest: man}
	for {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return out, fmt.Errorf("read archive: %w", err)
		}

		switch {
		case in.RestoreFiles && strings.HasPrefix(h.Name, backuparchive.FilesPrefix):
			rel := strings.TrimPrefix(h.Name, backuparchive.FilesPrefix)
			if rel == "" {
				continue
			}
			if err := restoreMember(root, acct, tr, h, rel); err != nil {
				return out, fmt.Errorf("restore %s: %w", rel, err)
			}
			if h.Typeflag == tar.TypeReg {
				out.Files++
			}

		case in.RestoreDatabases && strings.HasPrefix(h.Name, backuparchive.DatabasesPrefix):
			name := strings.TrimSuffix(strings.TrimPrefix(h.Name, backuparchive.DatabasesPrefix), ".sql")
			if !dbms.ValidDatabaseName(name) {
				return out, fmt.Errorf("archive names an unacceptable database %q", name)
			}
			if err := loadDatabase(ctx, tr, name); err != nil {
				return out, fmt.Errorf("restore database %s: %w", name, err)
			}
			out.Databases = append(out.Databases, name)
		}
	}
	return out, nil
}

// restoreMember writes one archive entry inside the account's home.
func restoreMember(root *os.Root, acct *linuxuser.Account, tr *tar.Reader, h *tar.Header, rel string) error {
	rel = path.Clean(rel)
	uid, gid := int(acct.UID), int(acct.GID)

	switch h.Typeflag {
	case tar.TypeDir:
		if err := root.Mkdir(rel, fs.FileMode(h.Mode).Perm()); err != nil && !errors.Is(err, fs.ErrExist) {
			return err
		}
		_ = root.Chmod(rel, fs.FileMode(h.Mode).Perm())
		return root.Chown(rel, uid, gid)

	case tar.TypeSymlink:
		// Removed first: Symlink fails on an existing name, and a restore
		// over a live account is expected to replace what is there.
		_ = root.Remove(rel)
		if err := root.Symlink(h.Linkname, rel); err != nil {
			return err
		}
		return root.Lchown(rel, uid, gid)

	case tar.TypeReg:
		if dir := path.Dir(rel); dir != "." {
			if err := mkdirAllIn(root, dir, uid, gid); err != nil {
				return err
			}
		}
		f, err := root.OpenFile(rel, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, fs.FileMode(h.Mode).Perm())
		if err != nil {
			return err
		}
		_, err = io.Copy(f, tr)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return err
		}
		_ = root.Chmod(rel, fs.FileMode(h.Mode).Perm())
		return root.Chown(rel, uid, gid)

	default:
		return nil
	}
}

// mkdirAllIn creates a directory chain inside the root, giving each new
// component to the account. Needed because an archive may hold a file whose
// parent directory entry came earlier in the stream, or not at all.
func mkdirAllIn(root *os.Root, dir string, uid, gid int) error {
	var built string
	for _, seg := range strings.Split(dir, "/") {
		if seg == "" || seg == "." {
			continue
		}
		built = path.Join(built, seg)
		if err := root.Mkdir(built, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
			return err
		}
		_ = root.Chown(built, uid, gid)
	}
	return nil
}

// loadDatabase pipes a dump into the server. The dump was written with
// --databases, so it carries its own CREATE DATABASE and USE.
func loadDatabase(ctx context.Context, r io.Reader, name string) error {
	var stderr strings.Builder
	cmd := exec.CommandContext(ctx, "mariadb", "--default-character-set=utf8mb4")
	cmd.Stdin = r
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}
