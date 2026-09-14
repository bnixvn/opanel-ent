package actions

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"

	"github.com/bnixvn/opanel-ent/internal/agent"
	"github.com/bnixvn/opanel-ent/internal/platform/linuxuser"
)

// LogKinds are the two logs OpenLiteSpeed writes per site.
const (
	LogAccess = "access"
	LogError  = "error"
)

// MaxLogTailLines caps a tail request. A customer asking for the last
// million lines would hold the agent for minutes and send the browser more
// than it can render.
const MaxLogTailLines = 5000

// LogTailRequest asks for the end of one of a site's logs.
type LogTailRequest struct {
	Owner string `json:"owner"`
	// Domain names the site directory under the owner's home.
	Domain string `json:"domain"`
	Kind   string `json:"kind"`
	Lines  int    `json:"lines"`
	// Filter keeps only lines containing this text, applied before the line
	// limit so a search reaches back through the whole file.
	Filter string `json:"filter,omitempty"`
}

// Validate checks the account, the site and the log name.
func (r *LogTailRequest) Validate() error {
	if !linuxuser.PlausibleName(r.Owner) {
		return fmt.Errorf("%q is not an acceptable account name", r.Owner)
	}
	// The domain becomes a directory name under the home, so it must not be
	// able to reach anywhere else.
	if r.Domain == "" || strings.ContainsAny(r.Domain, "/\\") || strings.Contains(r.Domain, "..") {
		return fmt.Errorf("%q is not an acceptable site name", r.Domain)
	}
	if r.Kind != LogAccess && r.Kind != LogError {
		return fmt.Errorf("log must be %q or %q", LogAccess, LogError)
	}
	if r.Lines < 0 || r.Lines > MaxLogTailLines {
		return fmt.Errorf("lines must be between 1 and %d", MaxLogTailLines)
	}
	if len(r.Filter) > 200 {
		return errors.New("the filter is too long")
	}
	return nil
}

// LogTailResult is the end of a log.
type LogTailResult struct {
	Domain string `json:"domain"`
	Kind   string `json:"kind"`
	// Lines are oldest first, which is the order they were written and the
	// order a reader following a request through them expects.
	Lines []string `json:"lines"`
	Size  int64    `json:"size"`
	// Truncated says the file was longer than what came back.
	Truncated bool `json:"truncated"`
}

func registerLogs(r *agent.Registry) {
	agent.Register(r, "log.tail", 1, func(_ context.Context, in LogTailRequest) (LogTailResult, error) {
		f, root, err := openSiteLog(in.Owner, in.Domain, in.Kind)
		if err != nil {
			return LogTailResult{}, err
		}
		defer func() { _ = f.Close(); _ = root.Close() }()

		info, err := f.Stat()
		if err != nil {
			return LogTailResult{}, err
		}
		lines := in.Lines
		if lines == 0 {
			lines = 200
		}
		tail, truncated, err := tailLines(f, info.Size(), lines, in.Filter)
		if err != nil {
			return LogTailResult{}, err
		}
		return LogTailResult{
			Domain: in.Domain, Kind: in.Kind, Lines: tail,
			Size: info.Size(), Truncated: truncated,
		}, nil
	})

	// Downloading a whole log goes through the same staging file the file
	// manager uses, for the same reason: the API cannot open a customer's
	// file and the protocol carries JSON, not bytes.
	agent.Register(r, "log.stage", 1, func(_ context.Context, in LogTailRequest) (FileStageResult, error) {
		f, root, err := openSiteLog(in.Owner, in.Domain, in.Kind)
		if err != nil {
			return FileStageResult{}, err
		}
		defer func() { _ = f.Close(); _ = root.Close() }()

		info, err := f.Stat()
		if err != nil {
			return FileStageResult{}, err
		}
		if err := os.MkdirAll(UploadStageDir, 0o750); err != nil {
			return FileStageResult{}, err
		}
		if err := chownToAPI(UploadStageDir); err != nil {
			return FileStageResult{}, err
		}
		tmp, err := os.CreateTemp(UploadStageDir, "log-*")
		if err != nil {
			return FileStageResult{}, err
		}
		defer func() { _ = tmp.Close() }()
		if _, err := io.Copy(tmp, f); err != nil {
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

// openSiteLog opens a log inside the owner's home, with the same kernel-level
// containment the file manager relies on.
func openSiteLog(owner, domain, kind string) (*os.File, *os.Root, error) {
	acct, err := linuxuser.Lookup(owner)
	if err != nil {
		return nil, nil, err
	}
	if acct == nil {
		return nil, nil, fmt.Errorf("account %q does not exist", owner)
	}
	root, err := os.OpenRoot(acct.Home)
	if err != nil {
		return nil, nil, err
	}
	rel := path.Join(domain, "logs", kind+".log")
	f, err := root.Open(rel)
	if err != nil {
		_ = root.Close()
		if errors.Is(err, os.ErrNotExist) {
			// A site nobody has visited has no log yet, which is not a
			// failure worth an error page.
			return nil, nil, fmt.Errorf("no %s log for %s yet", kind, domain)
		}
		return nil, nil, err
	}
	return f, root, nil
}

// tailLines reads the end of a file without loading all of it.
//
// Logs on a busy site reach hundreds of megabytes. Reading from the end in
// chunks means the cost is proportional to what was asked for rather than to
// how long the site has been up -- except when a filter is set, which has to
// look at everything to find matches.
func tailLines(f *os.File, size int64, want int, filter string) ([]string, bool, error) {
	if filter != "" {
		return filterLines(f, want, filter)
	}

	const chunk = 64 << 10
	var (
		buf   []byte
		at    = size
		found int
	)
	for at > 0 && found <= want {
		step := int64(chunk)
		if at < step {
			step = at
		}
		at -= step
		part := make([]byte, step)
		if _, err := f.ReadAt(part, at); err != nil && !errors.Is(err, io.EOF) {
			return nil, false, err
		}
		buf = append(part, buf...)
		found = bytes.Count(buf, []byte{'\n'})
	}

	lines := splitLines(buf)
	truncated := len(lines) > want || at > 0
	if len(lines) > want {
		lines = lines[len(lines)-want:]
	}
	return lines, truncated, nil
}

// filterLines scans the whole file for matches, keeping the last `want`.
func filterLines(f *os.File, want int, filter string) ([]string, bool, error) {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, false, err
	}
	body, err := io.ReadAll(io.LimitReader(f, 256<<20))
	if err != nil {
		return nil, false, err
	}
	var out []string
	needle := strings.ToLower(filter)
	for _, line := range splitLines(body) {
		if strings.Contains(strings.ToLower(line), needle) {
			out = append(out, line)
		}
	}
	truncated := len(out) > want
	if truncated {
		out = out[len(out)-want:]
	}
	return out, truncated, nil
}

func splitLines(b []byte) []string {
	text := strings.TrimRight(string(b), "\n")
	if text == "" {
		return nil
	}
	return strings.Split(text, "\n")
}
