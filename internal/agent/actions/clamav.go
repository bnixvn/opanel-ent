package actions

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/bnixvn/opanel-ent/internal/agent"
	"github.com/bnixvn/opanel-ent/internal/platform/linuxuser"
	"github.com/bnixvn/opanel-ent/internal/platform/pkgmgr"
	"github.com/bnixvn/opanel-ent/internal/platform/run"
)

// Where ClamAV lives, and where the panel puts what it finds.
const (
	clamScanBin  = "/usr/bin/clamscan"
	freshclamBin = "/usr/bin/freshclam"
	clamDBDir    = "/var/lib/clamav"

	// QuarantineDir holds infected files, owned by root and readable by
	// nobody else. A quarantine a customer can read is a quarantine that
	// still serves the malware over HTTP.
	QuarantineDir = "/var/lib/opanel/quarantine"
)

// ClamScanBudget bounds one scan. A customer's home with a large WordPress
// in it takes minutes; a whole server takes considerably longer.
const ClamScanBudget = 4 * time.Hour

// ClamUpdateBudget bounds a signature update. The first one downloads the
// full database, which is several hundred megabytes.
const ClamUpdateBudget = 30 * time.Minute

// ClamAVStatus reports what is installed and how current it is.
type ClamAVStatus struct {
	Installed bool   `json:"installed"`
	Version   string `json:"version,omitempty"`
	// Signatures is how many the database holds, and DBUpdated when it was
	// last written. An out-of-date database is the failure mode nobody
	// notices: scans keep passing, and they mean nothing.
	Signatures int       `json:"signatures,omitempty"`
	DBUpdated  time.Time `json:"db_updated,omitempty"`
	// MemoryMB is what the host has free. ClamAV loads the whole signature
	// database into memory, so a small VPS can fail a scan in a way that
	// looks like a bug rather than a shortage.
	MemoryMB int    `json:"memory_mb"`
	Warning  string `json:"warning,omitempty"`
}

// ClamScanRequest walks one directory.
type ClamScanRequest struct {
	// Path is absolute. The panel resolves it from an account, so the agent
	// only has to refuse the obvious.
	Path string `json:"path"`
	// MaxFileMB skips anything larger. Malware is small; a 2 GB video is
	// minutes of reading for nothing.
	MaxFileMB int `json:"max_file_mb,omitempty"`
}

// Validate refuses paths no scan should ever walk.
func (r *ClamScanRequest) Validate() error {
	if !filepath.IsAbs(r.Path) || strings.Contains(r.Path, "..") {
		return fmt.Errorf("%q is not an absolute path", r.Path)
	}
	clean := filepath.Clean(r.Path)
	// Scanning these walks device nodes and kernel interfaces, which at best
	// wastes hours and at worst blocks on a fifo forever.
	for _, bad := range []string{"/proc", "/sys", "/dev", "/run"} {
		if clean == bad || strings.HasPrefix(clean, bad+"/") {
			return fmt.Errorf("%s is not a directory worth scanning", clean)
		}
	}
	if r.MaxFileMB < 0 || r.MaxFileMB > 4096 {
		return errors.New("the file size limit is out of range")
	}
	return nil
}

// ClamFinding is one infected file.
type ClamFinding struct {
	Path      string `json:"path"`
	Signature string `json:"signature"`
}

// ClamScanResult is what a scan found.
type ClamScanResult struct {
	FilesScanned int           `json:"files_scanned"`
	Infected     int           `json:"infected"`
	Findings     []ClamFinding `json:"findings"`
	Duration     string        `json:"duration"`
	// Truncated is set when the scan found more than the panel will store.
	Truncated bool `json:"truncated,omitempty"`
}

// MaxFindings caps what one scan reports. A home directory that is entirely
// malware is a rebuild, not a list to page through.
const MaxFindings = 5000

// ClamQuarantineRequest moves a file out of reach, or puts it back.
type ClamQuarantineRequest struct {
	// Op is "quarantine", "restore" or "delete".
	Op string `json:"op"`
	// Path is the file, for quarantine and delete. For restore it is where
	// the file should go back to.
	Path string `json:"path"`
	// Owner is the account the file belongs to, so a restored file comes
	// back owned correctly rather than owned by root.
	Owner string `json:"owner"`
	// QuarantinePath is what quarantine returned, for restore and delete.
	QuarantinePath string `json:"quarantine_path,omitempty"`
}

// Validate checks the operation and the paths.
func (r *ClamQuarantineRequest) Validate() error {
	switch r.Op {
	case "quarantine", "restore", "delete":
	default:
		return fmt.Errorf("%q is not an operation", r.Op)
	}
	if !linuxuser.PlausibleName(r.Owner) {
		return fmt.Errorf("%q is not an acceptable account name", r.Owner)
	}
	home := linuxuser.Home(r.Owner)
	if !strings.HasPrefix(filepath.Clean(r.Path), home+"/") || strings.Contains(r.Path, "..") {
		// The panel only ever acts on a customer's own files. A finding
		// outside a home is reported and left alone: moving something out of
		// /usr because a scanner disliked it is how a server stops booting.
		return fmt.Errorf("%q is not inside %s", r.Path, home)
	}
	if r.Op != "quarantine" {
		if r.QuarantinePath == "" {
			return errors.New("which quarantined file?")
		}
		if !strings.HasPrefix(filepath.Clean(r.QuarantinePath), QuarantineDir+"/") ||
			strings.Contains(r.QuarantinePath, "..") {
			return fmt.Errorf("%q is not in the quarantine", r.QuarantinePath)
		}
	}
	return nil
}

// ClamQuarantineResult reports where the file went.
type ClamQuarantineResult struct {
	QuarantinePath string `json:"quarantine_path,omitempty"`
}

func registerClamAV(r *agent.Registry) {
	agent.Register(r, "clam.status", 1, func(_ context.Context, _ struct{}) (ClamAVStatus, error) {
		return clamStatus(), nil
	})

	agent.RegisterSlow(r, "clam.install", 1, ClamUpdateBudget,
		func(ctx context.Context, _ struct{}) (ClamAVStatus, error) {
			if err := installClamAV(ctx); err != nil {
				return ClamAVStatus{}, err
			}
			return clamStatus(), nil
		})

	agent.RegisterSlow(r, "clam.remove", 1, ClamUpdateBudget,
		func(ctx context.Context, in ClamRemoveRequest) (ClamRemoveResult, error) {
			return removeClamAV(ctx, in)
		})

	agent.RegisterSlow(r, "clam.update", 1, ClamUpdateBudget,
		func(ctx context.Context, _ struct{}) (ClamAVStatus, error) {
			if err := updateSignatures(ctx); err != nil {
				return clamStatus(), err
			}
			return clamStatus(), nil
		})

	agent.RegisterSlow(r, "clam.scan", 1, ClamScanBudget,
		func(ctx context.Context, in ClamScanRequest) (ClamScanResult, error) {
			return runScan(ctx, in)
		})

	agent.Register(r, "clam.quarantine", 1,
		func(_ context.Context, in ClamQuarantineRequest) (ClamQuarantineResult, error) {
			return quarantine(in)
		})
}

// clamStatus reads the installation without running a scan.
func clamStatus() ClamAVStatus {
	st := ClamAVStatus{MemoryMB: availableMemoryMB()}
	if _, err := os.Stat(clamScanBin); err != nil {
		return st
	}
	st.Installed = true

	if res, err := run.Cmd(context.Background(), []string{clamScanBin, "--version"},
		run.Timeout(30*time.Second)); err == nil {
		st.Version = strings.TrimSpace(res.Stdout)
	}

	// Counting signatures by reading the database files rather than asking
	// clamscan: --version loads nothing, and a full load costs a gigabyte.
	entries, err := os.ReadDir(clamDBDir)
	if err == nil {
		for _, e := range entries {
			name := e.Name()
			if !strings.HasSuffix(name, ".cvd") && !strings.HasSuffix(name, ".cld") {
				continue
			}
			info, err := e.Info()
			if err != nil {
				continue
			}
			if info.ModTime().After(st.DBUpdated) {
				st.DBUpdated = info.ModTime()
			}
			st.Signatures++
		}
	}
	if st.Signatures == 0 {
		st.Warning = "No signature database yet. Update the signatures before " +
			"the first scan, or it will find nothing and look clean."
	} else if !st.DBUpdated.IsZero() && time.Since(st.DBUpdated) > 7*24*time.Hour {
		st.Warning = "The signature database is more than a week old. Scans will " +
			"keep passing and will not mean much."
	}
	// The database is loaded into memory for every scan. Below this a scan
	// dies with an allocation failure, which reads as a broken panel.
	if st.MemoryMB > 0 && st.MemoryMB < 1400 {
		st.Warning = strings.TrimSpace(st.Warning + " ClamAV needs roughly 1.5 GB " +
			"free to load its signatures; this server has " +
			strconv.Itoa(st.MemoryMB) + " MB. Scans may fail until there is more.")
	}
	return st
}

// availableMemoryMB reads MemAvailable, which is what a scan can actually
// use -- MemFree ignores the page cache the kernel will hand back.
func availableMemoryMB() int {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0
	}
	defer func() { _ = f.Close() }()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "MemAvailable:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return 0
		}
		kb, err := strconv.Atoi(fields[1])
		if err != nil {
			return 0
		}
		return kb / 1024
	}
	return 0
}

// ClamRemoveRequest asks for the scanner to be taken off the server.
type ClamRemoveRequest struct {
	// DropQuarantine also deletes the quarantine directory.
	//
	// Off unless asked for, and asked for separately from the removal. What
	// is in there is the customer's own files -- a plugin that tripped a
	// signature is still their plugin, and a false positive is still their
	// site's code. Removing the scanner is a decision about software;
	// deleting what it took is a decision about somebody else's data.
	DropQuarantine bool `json:"drop_quarantine"`
}

// Validate accepts anything: both fields are booleans.
func (r *ClamRemoveRequest) Validate() error { return nil }

// ClamRemoveResult reports what was left behind.
type ClamRemoveResult struct {
	Status ClamAVStatus `json:"status"`
	// QuarantineKept is how many files are still in quarantine, and where.
	QuarantineKept int    `json:"quarantine_kept"`
	QuarantineDir  string `json:"quarantine_dir,omitempty"`
}

// removeClamAV takes the scanner off the server.
//
// The signature database goes with it, which is the point: it is a few
// hundred megabytes that a server not scanning anything has no use for, and
// leaving freshclam behind to keep downloading it would be the one part of
// ClamAV that carried on running after it was removed.
func removeClamAV(ctx context.Context, in ClamRemoveRequest) (ClamRemoveResult, error) {
	// The updater first. Removing the package while its timer is due to fire
	// leaves a failing unit behind for the operator to find later.
	for _, unit := range []string{"clamav-freshclam.service", "clamav-freshclam.timer"} {
		_, _ = run.Cmd(ctx, []string{"systemctl", "disable", "--now", unit},
			run.Timeout(60*time.Second))
	}

	if err := pkgmgr.Remove(ctx, "clamav", "clamav-freshclam"); err != nil {
		return ClamRemoveResult{}, fmt.Errorf("remove clamav: %w", err)
	}
	// The database is the package's data, not its files, so rpm leaves it.
	if err := os.RemoveAll("/var/lib/clamav"); err != nil && !os.IsNotExist(err) {
		return ClamRemoveResult{}, fmt.Errorf("remove the signature database: %w", err)
	}

	out := ClamRemoveResult{QuarantineDir: QuarantineDir}
	if in.DropQuarantine {
		if err := os.RemoveAll(QuarantineDir); err != nil && !os.IsNotExist(err) {
			return out, fmt.Errorf("remove the quarantine: %w", err)
		}
		out.QuarantineDir = ""
	} else {
		entries, err := os.ReadDir(QuarantineDir)
		if err == nil {
			out.QuarantineKept = len(entries)
		}
		if out.QuarantineKept == 0 {
			// An empty quarantine is not worth keeping or mentioning.
			_ = os.Remove(QuarantineDir)
			out.QuarantineDir = ""
		}
	}
	out.Status = clamStatus()
	return out, nil
}

// installClamAV adds the scanner and the updater, but not the daemon.
//
// clamscan rather than clamd deliberately: clamd keeps the whole signature
// database resident for ever, which on a 2 GB hosting VPS is most of the
// memory the customers paid for. A scan that costs a gigabyte for ten
// minutes is a better trade than one that costs it permanently.
func installClamAV(ctx context.Context) error {
	if err := pkgmgr.Install(ctx, "clamav", "clamav-freshclam"); err != nil {
		return fmt.Errorf("install clamav: %w", err)
	}
	if err := os.MkdirAll(QuarantineDir, 0o700); err != nil {
		return err
	}
	// freshclam ships with a config that refuses to run until a line is
	// removed, which is a deliberate "read me first" and not something a
	// customer should have to discover.
	const conf = "/etc/freshclam.conf"
	body, err := os.ReadFile(conf)
	if err == nil && strings.Contains(string(body), "\nExample\n") {
		fixed := strings.Replace(string(body), "\nExample\n", "\n#Example\n", 1)
		if err := os.WriteFile(conf, []byte(fixed), 0o644); err != nil {
			return fmt.Errorf("enable freshclam: %w", err)
		}
	}
	return updateSignatures(ctx)
}

// updateSignatures refreshes the database.
func updateSignatures(ctx context.Context) error {
	if _, err := os.Stat(freshclamBin); err != nil {
		return errors.New("freshclam is not installed")
	}
	res, err := run.Cmd(ctx, []string{freshclamBin, "--quiet"}, run.Timeout(ClamUpdateBudget))
	if err != nil {
		// freshclam exits non-zero when the database is already current,
		// which is a success as far as anybody asking is concerned.
		combined := res.Stdout + res.Stderr
		if strings.Contains(combined, "up-to-date") || strings.Contains(combined, "up to date") {
			return nil
		}
		return fmt.Errorf("update signatures: %s", firstLines(combined, 3))
	}
	return nil
}

// runScan walks a directory and reports what matched.
func runScan(ctx context.Context, in ClamScanRequest) (ClamScanResult, error) {
	if _, err := os.Stat(clamScanBin); err != nil {
		return ClamScanResult{}, errors.New("ClamAV is not installed on this server")
	}
	if _, err := os.Stat(in.Path); err != nil {
		return ClamScanResult{}, fmt.Errorf("%s does not exist", in.Path)
	}
	maxFile := in.MaxFileMB
	if maxFile == 0 {
		maxFile = 64
	}

	started := time.Now()
	argv := []string{
		// Niced and io-niced: a scan that makes every website on the host
		// slow is a scan customers will ask to have turned off.
		"nice", "-n", "15", "ionice", "-c", "3",
		clamScanBin,
		"--recursive",
		"--infected",      // only report what matched
		"--no-summary=no", // keep the summary; it carries the file count
		"--stdout",
		"--max-filesize=" + strconv.Itoa(maxFile) + "M",
		"--max-scansize=" + strconv.Itoa(maxFile*4) + "M",
		// A deeply nested archive is a decompression bomb more often than it
		// is a customer's backup.
		"--max-recursion=8",
		"--exclude-dir=^/proc", "--exclude-dir=^/sys", "--exclude-dir=^/dev",
		"--exclude-dir=" + QuarantineDir,
		in.Path,
	}
	res, err := run.Cmd(ctx, argv, run.Timeout(ClamScanBudget))

	out := ClamScanResult{Duration: time.Since(started).Round(time.Second).String()}
	parseScanOutput(res.Stdout, &out)

	// clamscan's exit codes: 0 clean, 1 found something, 2 an error. Only 2
	// is a failure -- treating 1 as one would make every successful
	// detection look like a broken scan.
	if err != nil && out.Infected == 0 && res.ExitCode != 1 {
		return out, fmt.Errorf("scan failed: %s", firstLines(res.Stderr+res.Stdout, 4))
	}
	return out, nil
}

// parseScanOutput reads clamscan's report.
func parseScanOutput(stdout string, out *ClamScanResult) {
	for _, line := range strings.Split(stdout, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasSuffix(line, " FOUND"):
			// "path/to/file: Signature.Name FOUND"
			body := strings.TrimSuffix(line, " FOUND")
			idx := strings.LastIndex(body, ": ")
			if idx <= 0 {
				continue
			}
			out.Infected++
			if len(out.Findings) >= MaxFindings {
				out.Truncated = true
				continue
			}
			out.Findings = append(out.Findings, ClamFinding{
				Path: body[:idx], Signature: body[idx+2:],
			})
		case strings.HasPrefix(line, "Scanned files:"):
			if n, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "Scanned files:"))); err == nil {
				out.FilesScanned = n
			}
		}
	}
}

// quarantine moves a file out of reach, puts it back, or removes it.
func quarantine(in ClamQuarantineRequest) (ClamQuarantineResult, error) {
	acct, err := linuxuser.Lookup(in.Owner)
	if err != nil || acct == nil {
		return ClamQuarantineResult{}, fmt.Errorf("account %q does not exist", in.Owner)
	}

	switch in.Op {
	case "quarantine":
		if err := os.MkdirAll(QuarantineDir, 0o700); err != nil {
			return ClamQuarantineResult{}, err
		}
		// Named by a hash of the original path, so the same file quarantined
		// twice does not pile up and the name gives nothing away.
		sum := sha256.Sum256([]byte(in.Path))
		dest := filepath.Join(QuarantineDir,
			hex.EncodeToString(sum[:12])+"-"+time.Now().UTC().Format("20060102150405"))

		if err := moveFile(in.Path, dest); err != nil {
			return ClamQuarantineResult{}, err
		}
		// root-owned and unreadable by anyone else: a quarantine the account
		// can still read is a quarantine that still serves the malware.
		if err := os.Chown(dest, 0, 0); err != nil {
			return ClamQuarantineResult{}, err
		}
		if err := os.Chmod(dest, 0o600); err != nil {
			return ClamQuarantineResult{}, err
		}
		return ClamQuarantineResult{QuarantinePath: dest}, nil

	case "restore":
		if err := os.MkdirAll(filepath.Dir(in.Path), 0o755); err != nil {
			return ClamQuarantineResult{}, err
		}
		if err := moveFile(in.QuarantinePath, in.Path); err != nil {
			return ClamQuarantineResult{}, err
		}
		if err := os.Chown(in.Path, int(acct.UID), int(acct.GID)); err != nil {
			return ClamQuarantineResult{}, err
		}
		// Not executable on the way back. A restored file is one somebody
		// decided was a false positive, and it does not need to be a program
		// to be wrong about.
		if err := os.Chmod(in.Path, 0o644); err != nil {
			return ClamQuarantineResult{}, err
		}
		return ClamQuarantineResult{}, nil

	default: // delete
		if err := os.Remove(in.QuarantinePath); err != nil && !os.IsNotExist(err) {
			return ClamQuarantineResult{}, err
		}
		return ClamQuarantineResult{}, nil
	}
}

// moveFile renames where it can and copies where it cannot, because the
// quarantine and a customer's home are usually but not always on one
// filesystem.
func moveFile(from, to string) error {
	if err := os.Rename(from, to); err == nil {
		return nil
	}
	src, err := os.Open(from)
	if err != nil {
		return err
	}
	defer func() { _ = src.Close() }()

	dst, err := os.OpenFile(to, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := dst.ReadFrom(src); err != nil {
		_ = dst.Close()
		_ = os.Remove(to)
		return err
	}
	if err := dst.Close(); err != nil {
		return err
	}
	_ = src.Close()
	return os.Remove(from)
}
