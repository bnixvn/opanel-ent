package actions

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/bnixvn/opanel-ent/internal/agent"
	"github.com/bnixvn/opanel-ent/internal/platform/linuxuser"
)

// Limits on unpacking. An archive is attacker-controlled input -- a customer
// uploads one, or restores somebody else's backup -- so the cost of
// unpacking it has to be bounded before it is read, not after.
const (
	// MaxExtractBytes is the total written, uncompressed. A few gigabytes of
	// zeros compresses to almost nothing, which is the whole trick.
	MaxExtractBytes = 4 << 30
	// MaxExtractEntries bounds the file count, since millions of empty files
	// exhaust inodes without ever approaching the byte limit.
	MaxExtractEntries = 200000
	// ArchiveBudget and ExtractBudget are wall-clock: these run on customer
	// data of unknown size and must not hold an agent slot forever.
	ArchiveBudget = 30 * time.Minute
	ExtractBudget = 30 * time.Minute
)

// FileCopyRequest copies a file or a whole directory within one home.
type FileCopyRequest struct {
	Owner string `json:"owner"`
	From  string `json:"from"`
	To    string `json:"to"`
}

// Validate checks both ends.
func (r *FileCopyRequest) Validate() error {
	if err := (&FileRenameRequest{Owner: r.Owner, From: r.From, To: r.To}).Validate(); err != nil {
		return err
	}
	// Copying a directory into itself walks forever, writing until the disk
	// is full. The quota would stop it eventually; the customer would still
	// be left with a home full of nested copies.
	if r.To == r.From || strings.HasPrefix(r.To, r.From+"/") {
		return fmt.Errorf("cannot copy %s into itself", r.From)
	}
	return nil
}

// FileArchiveRequest packs paths into one archive.
type FileArchiveRequest struct {
	Owner string `json:"owner"`
	// Paths are relative to the home, and are packed with their base names
	// so an archive of one directory unpacks as that directory.
	Paths []string `json:"paths"`
	// Dest is the archive to write. Its extension chooses the format.
	Dest string `json:"dest"`
}

// Validate checks every path and the destination's format.
func (r *FileArchiveRequest) Validate() error {
	if len(r.Paths) == 0 {
		return errors.New("nothing was selected to archive")
	}
	if len(r.Paths) > 5000 {
		return errors.New("too many items selected; archive a directory instead")
	}
	for _, p := range r.Paths {
		if err := (&FilePathRequest{Owner: r.Owner, Path: p}).Validate(); err != nil {
			return err
		}
		if p == "" {
			return errors.New("cannot archive the home directory itself")
		}
	}
	if err := (&FilePathRequest{Owner: r.Owner, Path: r.Dest}).Validate(); err != nil {
		return err
	}
	if _, err := archiveFormat(r.Dest); err != nil {
		return err
	}
	return nil
}

// FileExtractRequest unpacks an archive into a directory.
type FileExtractRequest struct {
	Owner string `json:"owner"`
	Path  string `json:"path"`
	// Dest is the directory to unpack into, relative to the home. Empty
	// means the archive's own directory.
	Dest string `json:"dest"`
}

// Validate checks the archive path and the destination.
func (r *FileExtractRequest) Validate() error {
	if err := (&FilePathRequest{Owner: r.Owner, Path: r.Path}).Validate(); err != nil {
		return err
	}
	if r.Path == "" {
		return errors.New("no archive was given")
	}
	if r.Dest != "" {
		if err := (&FilePathRequest{Owner: r.Owner, Path: r.Dest}).Validate(); err != nil {
			return err
		}
	}
	if _, err := archiveFormat(r.Path); err != nil {
		return err
	}
	return nil
}

// FileExtractResult reports what came out.
type FileExtractResult struct {
	Entries int    `json:"entries"`
	Bytes   int64  `json:"bytes"`
	Dest    string `json:"dest"`
}

// archiveFormat maps a filename to a format, and is the whitelist: a name
// that is not one of these is refused before anything is opened.
func archiveFormat(name string) (string, error) {
	lower := strings.ToLower(name)
	switch {
	case strings.HasSuffix(lower, ".zip"):
		return "zip", nil
	case strings.HasSuffix(lower, ".tar.gz"), strings.HasSuffix(lower, ".tgz"):
		return "tar.gz", nil
	case strings.HasSuffix(lower, ".tar"):
		return "tar", nil
	}
	return "", fmt.Errorf("%s is not a .zip, .tar.gz or .tar", path.Base(name))
}

func registerFilesPlus(r *agent.Registry) {
	agent.RegisterSlow(r, "fs.copy", 1, ArchiveBudget,
		func(_ context.Context, in FileCopyRequest) (struct{}, error) {
			root, acct, err := openHome(in.Owner)
			if err != nil {
				return struct{}{}, err
			}
			defer func() { _ = root.Close() }()
			return struct{}{}, copyInRoot(root, acct, in.From, in.To)
		})

	// Recursive chmod is its own action rather than a flag, because the
	// rule it applies is not the one the caller gave: a directory needs the
	// execute bit to be enterable, and applying a file's 0644 to a whole
	// tree makes every directory in it unreadable.
	agent.RegisterSlow(r, "fs.chmod_recursive", 1, ArchiveBudget,
		func(_ context.Context, in FileChmodRequest) (struct{}, error) {
			root, _, err := openHome(in.Owner)
			if err != nil {
				return struct{}{}, err
			}
			defer func() { _ = root.Close() }()
			m, _ := strconv.ParseUint(in.Mode, 8, 32)
			return struct{}{}, chmodTree(root, rel(in.Path), fs.FileMode(m))
		})

	agent.RegisterSlow(r, "fs.archive", 1, ArchiveBudget,
		func(ctx context.Context, in FileArchiveRequest) (struct{}, error) {
			root, acct, err := openHome(in.Owner)
			if err != nil {
				return struct{}{}, err
			}
			defer func() { _ = root.Close() }()
			return struct{}{}, writeArchive(ctx, root, acct, in)
		})

	agent.RegisterSlow(r, "fs.extract", 1, ExtractBudget,
		func(ctx context.Context, in FileExtractRequest) (FileExtractResult, error) {
			root, acct, err := openHome(in.Owner)
			if err != nil {
				return FileExtractResult{}, err
			}
			defer func() { _ = root.Close() }()
			return extractArchive(ctx, root, acct, in)
		})
}

// copyInRoot copies a file or tree, giving everything to the account.
func copyInRoot(root *os.Root, acct *linuxuser.Account, from, to string) error {
	info, err := root.Lstat(from)
	if err != nil {
		return err
	}
	// An existing destination is a mistake worth refusing: the customer
	// picked the name, and silently replacing a directory is not recoverable.
	if _, err := root.Lstat(to); err == nil {
		return fmt.Errorf("%s already exists", to)
	}

	switch {
	case info.IsDir():
		return copyTreeInRoot(root, acct, from, to)
	case info.Mode()&os.ModeSymlink != 0:
		// Symlinks are copied as links, not as what they point at: following
		// one would copy something from outside the home into it.
		target, err := root.Readlink(from)
		if err != nil {
			return err
		}
		if err := root.Symlink(target, to); err != nil {
			return err
		}
		return root.Lchown(to, int(acct.UID), int(acct.GID))
	case !info.Mode().IsRegular():
		return fmt.Errorf("%s is not a regular file", from)
	}
	return copyFileInRoot(root, acct, from, to, info.Mode().Perm())
}

func copyFileInRoot(root *os.Root, acct *linuxuser.Account, from, to string, perm fs.FileMode) error {
	src, err := root.Open(from)
	if err != nil {
		return err
	}
	defer func() { _ = src.Close() }()

	dst, err := root.OpenFile(to, os.O_CREATE|os.O_EXCL|os.O_WRONLY, perm)
	if err != nil {
		return err
	}
	if _, err := io.Copy(dst, src); err != nil {
		_ = dst.Close()
		_ = root.Remove(to)
		return err
	}
	if err := dst.Close(); err != nil {
		return err
	}
	return root.Chown(to, int(acct.UID), int(acct.GID))
}

func copyTreeInRoot(root *os.Root, acct *linuxuser.Account, from, to string) error {
	return fs.WalkDir(root.FS(), from, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		target := to + strings.TrimPrefix(p, from)
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case d.IsDir():
			if err := root.Mkdir(target, info.Mode().Perm()); err != nil && !os.IsExist(err) {
				return err
			}
			return root.Chown(target, int(acct.UID), int(acct.GID))
		case info.Mode()&os.ModeSymlink != 0:
			link, err := root.Readlink(p)
			if err != nil {
				return err
			}
			if err := root.Symlink(link, target); err != nil {
				return err
			}
			return root.Lchown(target, int(acct.UID), int(acct.GID))
		case !info.Mode().IsRegular():
			// Sockets and devices in a customer's home are not worth copying
			// and cannot be recreated meaningfully anyway.
			return nil
		}
		return copyFileInRoot(root, acct, p, target, info.Mode().Perm())
	})
}

// directoryModeFor turns a file mode into the mode its directories get.
//
// Read implies enter, for a directory. Without this a recursive 0644 strips
// the execute bit from every directory in the tree, leaving something the
// customer cannot list -- and cannot repair, because repairing it means
// listing it first.
func directoryModeFor(mode fs.FileMode) fs.FileMode {
	dirMode := mode
	for _, shift := range []uint{6, 3, 0} {
		if mode&(0o4<<shift) != 0 {
			dirMode |= 0o1 << shift
		}
	}
	return dirMode
}

// chmodTree applies a mode to a tree, giving directories the execute bit.
func chmodTree(root *os.Root, base string, mode fs.FileMode) error {
	dirMode := directoryModeFor(mode)
	return fs.WalkDir(root.FS(), base, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		// Chmod on a symlink changes what it points at, which may be
		// anywhere the link can reach.
		if info.Mode()&os.ModeSymlink != 0 {
			return nil
		}
		if d.IsDir() {
			return root.Chmod(p, dirMode)
		}
		return root.Chmod(p, mode)
	})
}

// writeArchive packs the selected paths into one file inside the home.
func writeArchive(ctx context.Context, root *os.Root, acct *linuxuser.Account, in FileArchiveRequest) error {
	format, err := archiveFormat(in.Dest)
	if err != nil {
		return err
	}
	if _, err := root.Lstat(in.Dest); err == nil {
		return fmt.Errorf("%s already exists", in.Dest)
	}

	out, err := root.OpenFile(in.Dest, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	fail := func(e error) error {
		_ = out.Close()
		_ = root.Remove(in.Dest)
		return e
	}

	switch format {
	case "zip":
		zw := zip.NewWriter(out)
		if err := packZip(ctx, root, zw, in.Paths, in.Dest); err != nil {
			_ = zw.Close()
			return fail(err)
		}
		if err := zw.Close(); err != nil {
			return fail(err)
		}
	default:
		var w io.Writer = out
		var gz *gzip.Writer
		if format == "tar.gz" {
			gz = gzip.NewWriter(out)
			w = gz
		}
		tw := tar.NewWriter(w)
		if err := packTar(ctx, root, tw, in.Paths, in.Dest); err != nil {
			return fail(err)
		}
		if err := tw.Close(); err != nil {
			return fail(err)
		}
		if gz != nil {
			if err := gz.Close(); err != nil {
				return fail(err)
			}
		}
	}
	if err := out.Close(); err != nil {
		return err
	}
	return root.Chown(in.Dest, int(acct.UID), int(acct.GID))
}

// walkSelection visits every regular file and directory under the selection,
// reporting the name it should carry inside the archive.
func walkSelection(ctx context.Context, root *os.Root, paths []string, skip string, fn func(name string, p string, d fs.DirEntry) error) error {
	for _, sel := range paths {
		base := path.Base(sel)
		err := fs.WalkDir(root.FS(), sel, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			// The archive being written lives in the same tree; packing it
			// into itself would grow until the disk filled.
			if p == skip {
				return nil
			}
			name := base + strings.TrimPrefix(p, sel)
			return fn(name, p, d)
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func packZip(ctx context.Context, root *os.Root, zw *zip.Writer, paths []string, skip string) error {
	return walkSelection(ctx, root, paths, skip, func(name, p string, d fs.DirEntry) error {
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || (!info.IsDir() && !info.Mode().IsRegular()) {
			return nil
		}
		hdr, err := zip.FileInfoHeader(info)
		if err != nil {
			return err
		}
		hdr.Name = name
		hdr.Method = zip.Deflate
		if d.IsDir() {
			hdr.Name += "/"
		}
		w, err := zw.CreateHeader(hdr)
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		f, err := root.Open(p)
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()
		_, err = io.Copy(w, f)
		return err
	})
}

func packTar(ctx context.Context, root *os.Root, tw *tar.Writer, paths []string, skip string) error {
	return walkSelection(ctx, root, paths, skip, func(name, p string, d fs.DirEntry) error {
		info, err := d.Info()
		if err != nil {
			return err
		}
		link := ""
		if info.Mode()&os.ModeSymlink != 0 {
			if link, err = root.Readlink(p); err != nil {
				return err
			}
		} else if !info.IsDir() && !info.Mode().IsRegular() {
			return nil
		}
		hdr, err := tar.FileInfoHeader(info, link)
		if err != nil {
			return err
		}
		hdr.Name = name
		if d.IsDir() {
			hdr.Name += "/"
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if d.IsDir() || link != "" {
			return nil
		}
		f, err := root.Open(p)
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()
		_, err = io.Copy(tw, f)
		return err
	})
}

// extractArchive unpacks into the home.
//
// Every entry name is checked before anything is created, and creation goes
// through the os.Root handle, so a "../../etc/cron.d/x" entry -- the oldest
// archive attack there is -- cannot land outside the customer's home even if
// the name check were wrong.
func extractArchive(ctx context.Context, root *os.Root, acct *linuxuser.Account, in FileExtractRequest) (FileExtractResult, error) {
	format, err := archiveFormat(in.Path)
	if err != nil {
		return FileExtractResult{}, err
	}
	dest := in.Dest
	if dest == "" {
		dest = path.Dir(in.Path)
		if dest == "." {
			dest = ""
		}
	}
	if dest != "" {
		if err := mkdirAllInRoot(root, acct, dest); err != nil {
			return FileExtractResult{}, err
		}
	}

	f, err := root.Open(in.Path)
	if err != nil {
		return FileExtractResult{}, err
	}
	defer func() { _ = f.Close() }()

	res := FileExtractResult{Dest: dest}
	if format == "zip" {
		info, err := f.Stat()
		if err != nil {
			return res, err
		}
		zr, err := zip.NewReader(f, info.Size())
		if err != nil {
			return res, fmt.Errorf("%s is not a readable zip: %w", path.Base(in.Path), err)
		}
		for _, e := range zr.File {
			if ctx.Err() != nil {
				return res, ctx.Err()
			}
			if err := extractOne(root, acct, dest, e.Name, e.FileInfo(), &res, func() (io.ReadCloser, error) {
				return e.Open()
			}); err != nil {
				return res, err
			}
		}
		return res, nil
	}

	var src io.Reader = f
	if format == "tar.gz" {
		gz, err := gzip.NewReader(f)
		if err != nil {
			return res, fmt.Errorf("%s is not gzip: %w", path.Base(in.Path), err)
		}
		defer func() { _ = gz.Close() }()
		src = gz
	}
	tr := tar.NewReader(src)
	for {
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return res, nil
		}
		if err != nil {
			return res, err
		}
		// Links are skipped rather than recreated. A hard link can point at
		// a file the customer does not own, and a symlink written first can
		// redirect a later entry outside the tree.
		if hdr.Typeflag == tar.TypeSymlink || hdr.Typeflag == tar.TypeLink {
			continue
		}
		if err := extractOne(root, acct, dest, hdr.Name, hdr.FileInfo(), &res, func() (io.ReadCloser, error) {
			return io.NopCloser(tr), nil
		}); err != nil {
			return res, err
		}
	}
}

// extractOne writes a single entry, or refuses it.
func extractOne(root *os.Root, acct *linuxuser.Account, dest, name string, info fs.FileInfo, res *FileExtractResult, open func() (io.ReadCloser, error)) error {
	clean, err := safeEntryName(name)
	if err != nil {
		return err
	}
	if clean == "" {
		return nil
	}
	res.Entries++
	if res.Entries > MaxExtractEntries {
		return fmt.Errorf("the archive holds more than %d files", MaxExtractEntries)
	}

	target := clean
	if dest != "" {
		target = dest + "/" + clean
	}
	if info.IsDir() {
		return mkdirAllInRoot(root, acct, target)
	}
	if !info.Mode().IsRegular() {
		return nil
	}
	if parent := path.Dir(target); parent != "." && parent != "" {
		if err := mkdirAllInRoot(root, acct, parent); err != nil {
			return err
		}
	}

	rc, err := open()
	if err != nil {
		return err
	}
	defer func() { _ = rc.Close() }()

	// Only the permission bits, and never setuid: an archive is the classic
	// way to smuggle a setuid binary onto a host.
	out, err := root.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, info.Mode().Perm()&0o777)
	if err != nil {
		return err
	}
	// The limit is enforced on what is written, not on what the header
	// claims, because the header is written by whoever built the archive.
	remaining := MaxExtractBytes - res.Bytes
	n, err := io.Copy(out, io.LimitReader(rc, remaining+1))
	res.Bytes += n
	if err == nil && n > remaining {
		err = fmt.Errorf("the archive unpacks to more than %d bytes", int64(MaxExtractBytes))
	}
	if err != nil {
		_ = out.Close()
		_ = root.Remove(target)
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return root.Chown(target, int(acct.UID), int(acct.GID))
}

// safeEntryName turns an archive entry name into a path inside the tree, or
// refuses it.
func safeEntryName(name string) (string, error) {
	n := strings.ReplaceAll(name, "\\", "/")
	if strings.HasPrefix(n, "/") {
		return "", fmt.Errorf("the archive holds an absolute path: %s", name)
	}
	// A drive letter is how a Windows-built zip smuggles an absolute path
	// past a check that only looks for a leading slash.
	if len(n) > 1 && n[1] == ':' {
		return "", fmt.Errorf("the archive holds an absolute path: %s", name)
	}
	cleaned := path.Clean(n)
	if cleaned == "." || cleaned == "/" {
		return "", nil
	}
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", fmt.Errorf("the archive tries to write outside the destination: %s", name)
	}
	if strings.ContainsRune(cleaned, 0) {
		return "", fmt.Errorf("the archive holds an unusable name: %q", name)
	}
	return cleaned, nil
}

// mkdirAllInRoot creates a directory and its parents inside the root,
// handing each to the account.
func mkdirAllInRoot(root *os.Root, acct *linuxuser.Account, dir string) error {
	if dir == "" || dir == "." {
		return nil
	}
	var built string
	for _, part := range strings.Split(dir, "/") {
		if part == "" {
			continue
		}
		if built == "" {
			built = part
		} else {
			built += "/" + part
		}
		if info, err := root.Lstat(built); err == nil {
			if !info.IsDir() {
				return fmt.Errorf("%s exists and is not a directory", built)
			}
			continue
		}
		if err := root.Mkdir(built, 0o755); err != nil && !os.IsExist(err) {
			return err
		}
		if err := root.Chown(built, int(acct.UID), int(acct.GID)); err != nil {
			return err
		}
	}
	return nil
}
