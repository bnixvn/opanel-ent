package actions

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/bnixvn/opanel-ent/internal/agent"
	"github.com/bnixvn/opanel-ent/internal/dbms"
	"github.com/bnixvn/opanel-ent/internal/hostimport"
	"github.com/bnixvn/opanel-ent/internal/platform/linuxuser"
	"github.com/bnixvn/opanel-ent/internal/platform/run"
)

// ImportBudget bounds one import. A hosting account with years of uploads in
// it is gigabytes, and the archive is read twice: once to plan and once to
// unpack.
const ImportBudget = 4 * time.Hour

// ImportStageDir is where an uploaded archive waits. Owned by the API
// account, which is the only thing that writes here.
const ImportStageDir = "/var/lib/opanel/imports"

// ImportInspectRequest reads a staged archive without changing anything.
type ImportInspectRequest struct {
	StagedPath string `json:"staged_path"`
}

// Validate keeps the agent reading only what the API staged.
func (r *ImportInspectRequest) Validate() error {
	clean := filepath.Clean(r.StagedPath)
	if !strings.HasPrefix(clean, ImportStageDir+"/") || strings.Contains(r.StagedPath, "..") {
		return fmt.Errorf("%q is not an uploaded archive", r.StagedPath)
	}
	return nil
}

// ImportExtractRequest unpacks an account's files.
type ImportExtractRequest struct {
	StagedPath string `json:"staged_path"`
	// Owner is the panel account the files become.
	Owner string `json:"owner"`
	// Moves say what to put where: a path inside the archive, and a path
	// relative to the account's home.
	Moves []ImportMove `json:"moves"`
}

// ImportMove is one directory from the archive and where it lands.
type ImportMove struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// Validate checks the account and every destination.
func (r *ImportExtractRequest) Validate() error {
	if err := (&ImportInspectRequest{StagedPath: r.StagedPath}).Validate(); err != nil {
		return err
	}
	if !linuxuser.PlausibleName(r.Owner) {
		return fmt.Errorf("%q is not an acceptable account name", r.Owner)
	}
	if len(r.Moves) == 0 {
		return errors.New("nothing was selected to import")
	}
	for _, m := range r.Moves {
		if m.From == "" || strings.Contains(m.From, "..") {
			return fmt.Errorf("%q is not a path inside the archive", m.From)
		}
		if err := validRelPath(m.To); err != nil {
			return fmt.Errorf("destination %q: %w", m.To, err)
		}
	}
	return nil
}

// ImportExtractResult reports what was written.
type ImportExtractResult struct {
	Files   int      `json:"files"`
	Bytes   int64    `json:"bytes"`
	Skipped []string `json:"skipped,omitempty"`
}

// ImportDBRequest loads one dump out of the archive into a database.
type ImportDBRequest struct {
	StagedPath string `json:"staged_path"`
	// DumpIn is the path inside the archive.
	DumpIn string `json:"dump_in"`
	// Database is the one already created by the panel.
	Database string `json:"database"`
}

// Validate checks the dump and the target.
func (r *ImportDBRequest) Validate() error {
	if err := (&ImportInspectRequest{StagedPath: r.StagedPath}).Validate(); err != nil {
		return err
	}
	if r.DumpIn == "" || strings.Contains(r.DumpIn, "..") {
		return fmt.Errorf("%q is not a path inside the archive", r.DumpIn)
	}
	if !dbms.ValidDatabaseName(r.Database) {
		return fmt.Errorf("%q is not an acceptable database name", r.Database)
	}
	return nil
}

// ImportDBResult reports what was loaded.
type ImportDBResult struct {
	Bytes int64 `json:"bytes"`
}

func registerHostImport(r *agent.Registry) {
	agent.RegisterSlow(r, "import.inspect", 1, 30*time.Minute,
		func(_ context.Context, in ImportInspectRequest) (*hostimport.Plan, error) {
			f, err := os.Open(in.StagedPath)
			if err != nil {
				return nil, err
			}
			defer func() { _ = f.Close() }()
			return hostimport.Inspect(f)
		})

	agent.RegisterSlow(r, "import.extract", 1, ImportBudget,
		func(ctx context.Context, in ImportExtractRequest) (ImportExtractResult, error) {
			return importExtract(ctx, in)
		})

	agent.RegisterSlow(r, "import.database", 1, ImportBudget,
		func(ctx context.Context, in ImportDBRequest) (ImportDBResult, error) {
			return importDatabase(ctx, in)
		})

	agent.Register(r, "import.discard", 1,
		func(_ context.Context, in ImportInspectRequest) (struct{}, error) {
			err := os.Remove(in.StagedPath)
			if err != nil && !os.IsNotExist(err) {
				return struct{}{}, err
			}
			return struct{}{}, nil
		})
}

// importExtract unpacks the selected directories into the account's home.
//
// The same rules as the file manager's extractor, for the same reasons: this
// archive came from another company's server and nothing in it is trusted.
// Names are checked, writing goes through the os.Root handle so a bad name
// cannot land outside the home even if the check were wrong, links are
// skipped rather than recreated, and only permission bits survive.
func importExtract(ctx context.Context, in ImportExtractRequest) (ImportExtractResult, error) {
	root, acct, err := openHome(in.Owner)
	if err != nil {
		return ImportExtractResult{}, err
	}
	defer func() { _ = root.Close() }()

	f, err := os.Open(in.StagedPath)
	if err != nil {
		return ImportExtractResult{}, err
	}
	defer func() { _ = f.Close() }()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return ImportExtractResult{}, fmt.Errorf("that file is not a gzip archive: %w", err)
	}
	defer func() { _ = gz.Close() }()

	var res ImportExtractResult
	for _, m := range in.Moves {
		if m.To != "" {
			if err := mkdirAllInRoot(root, acct, m.To); err != nil {
				return res, err
			}
		}
	}

	tr := tar.NewReader(gz)
	for {
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return res, fmt.Errorf("read the archive: %w", err)
		}
		name := path.Clean(strings.TrimPrefix(hdr.Name, "./"))

		// Which move, if any, claims this entry.
		target, ok := destinationFor(in.Moves, name)
		if !ok {
			continue
		}
		// Links are skipped rather than recreated: a symlink written first
		// can redirect a later entry out of the tree, and a hard link can
		// point at a file this account does not own.
		if hdr.Typeflag == tar.TypeSymlink || hdr.Typeflag == tar.TypeLink {
			res.Skipped = append(res.Skipped, name)
			continue
		}
		clean, err := safeEntryName(target)
		if err != nil {
			return res, err
		}
		if clean == "" {
			continue
		}
		if hdr.FileInfo().IsDir() {
			if err := mkdirAllInRoot(root, acct, clean); err != nil {
				return res, err
			}
			continue
		}
		if !hdr.FileInfo().Mode().IsRegular() {
			res.Skipped = append(res.Skipped, name)
			continue
		}
		if parent := path.Dir(clean); parent != "." && parent != "" {
			if err := mkdirAllInRoot(root, acct, parent); err != nil {
				return res, err
			}
		}

		// Only the permission bits, never setuid: an archive from somebody
		// else's server is the classic way to smuggle one in.
		out, err := root.OpenFile(clean, os.O_CREATE|os.O_TRUNC|os.O_WRONLY,
			hdr.FileInfo().Mode().Perm()&0o777)
		if err != nil {
			return res, err
		}
		n, err := io.Copy(out, io.LimitReader(tr, MaxExtractBytes))
		if err != nil {
			_ = out.Close()
			return res, err
		}
		if err := out.Close(); err != nil {
			return res, err
		}
		if err := root.Chown(clean, int(acct.UID), int(acct.GID)); err != nil {
			return res, err
		}
		res.Files++
		res.Bytes += n
	}
	return res, nil
}

// destinationFor maps an archive entry onto the home directory, or reports
// that no move claims it.
func destinationFor(moves []ImportMove, name string) (string, bool) {
	for _, m := range moves {
		from := path.Clean(m.From)
		switch {
		case name == from:
			return m.To, true
		case strings.HasPrefix(name, from+"/"):
			rest := strings.TrimPrefix(name, from+"/")
			if m.To == "" {
				return rest, true
			}
			return path.Join(m.To, rest), true
		}
	}
	return "", false
}

// importDatabase streams one dump out of the archive into MariaDB.
//
// Streamed rather than extracted first: a dump is often the largest thing in
// the archive, and writing it to disk to read it straight back needs as much
// free space again at exactly the moment a migration is filling the server.
func importDatabase(ctx context.Context, in ImportDBRequest) (ImportDBResult, error) {
	f, err := os.Open(in.StagedPath)
	if err != nil {
		return ImportDBResult{}, err
	}
	defer func() { _ = f.Close() }()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return ImportDBResult{}, fmt.Errorf("that file is not a gzip archive: %w", err)
	}
	defer func() { _ = gz.Close() }()

	want := path.Clean(in.DumpIn)
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return ImportDBResult{}, fmt.Errorf("%s is not in that archive", in.DumpIn)
		}
		if err != nil {
			return ImportDBResult{}, err
		}
		if path.Clean(strings.TrimPrefix(hdr.Name, "./")) != want {
			continue
		}

		// The database name is on the command line, never in the SQL: a dump
		// from another server may carry its own USE or CREATE DATABASE, and
		// this makes sure everything lands where the panel decided.
		res, err := run.Cmd(ctx, []string{"mariadb", in.Database},
			run.StdinFrom(tr), run.Timeout(ImportBudget))
		if err != nil {
			return ImportDBResult{}, fmt.Errorf("load %s: %s",
				in.Database, firstLines(res.Stderr+res.Stdout, 3))
		}
		return ImportDBResult{Bytes: hdr.Size}, nil
	}
}
