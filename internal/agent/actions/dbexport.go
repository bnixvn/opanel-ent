package actions

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/bnixvn/opanel-ent/internal/agent"
	"github.com/bnixvn/opanel-ent/internal/dbms"
)

// DBExportBudget bounds a dump. A database large enough to take longer than
// this belongs in a backup, which streams and reports progress.
const DBExportBudget = 30 * time.Minute

// DBExportRequest asks for a dump of one database.
type DBExportRequest struct {
	Name string `json:"name"`
	// Compress writes gzip, which for SQL text is usually a tenth the size.
	Compress bool `json:"compress"`
}

// Validate checks the database name.
func (r *DBExportRequest) Validate() error {
	if !dbms.ValidDatabaseName(r.Name) {
		return fmt.Errorf("%q is not an acceptable database name", r.Name)
	}
	return nil
}

func registerDBExport(r *agent.Registry) {
	// Staged rather than streamed for the same reason as every other
	// download: the API runs unprivileged and cannot reach the socket that
	// mariadb-dump uses, and the agent protocol carries JSON, not bytes.
	agent.RegisterSlow(r, "db.export", 1, DBExportBudget,
		func(ctx context.Context, in DBExportRequest) (FileStageResult, error) {
			if err := os.MkdirAll(UploadStageDir, 0o750); err != nil {
				return FileStageResult{}, err
			}
			if err := chownToAPI(UploadStageDir); err != nil {
				return FileStageResult{}, err
			}
			tmp, err := os.CreateTemp(UploadStageDir, "dump-*")
			if err != nil {
				return FileStageResult{}, err
			}
			defer func() { _ = tmp.Close() }()

			if err := dumpTo(ctx, tmp, in.Name, in.Compress); err != nil {
				_ = os.Remove(tmp.Name())
				return FileStageResult{}, err
			}
			info, err := tmp.Stat()
			if err != nil {
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

// dumpTo writes a dump, optionally gzipped by piping through gzip rather
// than compressing in Go: the dump is already a subprocess, and one more in
// the pipeline costs nothing while keeping the memory flat.
func dumpTo(ctx context.Context, dst io.Writer, name string, compress bool) error {
	args := []string{
		"--single-transaction", "--quick", "--routines", "--events", "--triggers",
		"--default-character-set=utf8mb4", "--databases", name,
	}
	if compress {
		return runPipeline(ctx, dst, [][]string{
			append([]string{"mariadb-dump"}, args...),
			{"gzip", "-c"},
		})
	}
	return runPipeline(ctx, dst, [][]string{append([]string{"mariadb-dump"}, args...)})
}

// ExportFileName is what the browser should save the dump as.
func ExportFileName(database string, compress bool) string {
	name := database + "-" + time.Now().UTC().Format("20060102-150405") + ".sql"
	if compress {
		name += ".gz"
	}
	return strings.ReplaceAll(name, "/", "_")
}

// runPipeline runs commands connected by pipes, writing the last one's
// output to dst.
//
// Built here rather than with a shell string: the database name reaches
// mariadb-dump as an argv element, so a name containing a quote is a name
// and not a second command.
func runPipeline(ctx context.Context, dst io.Writer, stages [][]string) error {
	if len(stages) == 0 {
		return fmt.Errorf("no command given")
	}
	cmds := make([]*exec.Cmd, len(stages))
	var stderr strings.Builder

	for i, argv := range stages {
		cmds[i] = exec.CommandContext(ctx, argv[0], argv[1:]...)
		cmds[i].Stderr = &stderr
		if i > 0 {
			pipe, err := cmds[i-1].StdoutPipe()
			if err != nil {
				return err
			}
			cmds[i].Stdin = pipe
		}
	}
	cmds[len(cmds)-1].Stdout = dst

	// Started from the last stage backwards so every reader is waiting
	// before its writer produces anything.
	for i := len(cmds) - 1; i >= 0; i-- {
		if err := cmds[i].Start(); err != nil {
			return fmt.Errorf("start %s: %w", stages[i][0], err)
		}
	}
	for i, c := range cmds {
		if err := c.Wait(); err != nil {
			return fmt.Errorf("%s: %w: %s", stages[i][0], err,
				strings.TrimSpace(firstLines(stderr.String(), 3)))
		}
	}
	return nil
}
