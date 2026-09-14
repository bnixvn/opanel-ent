package actions

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/bnixvn/opanel-ent/internal/agent"
	"github.com/bnixvn/opanel-ent/internal/platform/linuxuser"
	"github.com/bnixvn/opanel-ent/internal/platform/run"
)

// Where the kernel is told which directory belongs to which project. XFS
// project quota reads these two files, so they are the panel's source of
// truth for the mapping and are rewritten from the account list.
const (
	projectsFile = "/etc/projects"
	projidFile   = "/etc/projid"
)

// QuotaMount is the filesystem the homes live on. Not a separate mount on the
// target host, so this is the root filesystem and the quota has to be enabled
// from the kernel command line.
const QuotaMount = "/"

// QuotaStatus says whether the kernel is enforcing anything.
type QuotaStatus struct {
	// Enforced is true when the filesystem is mounted with project quota
	// accounting *and* enforcement.
	Enforced bool `json:"enforced"`
	// Mounted reports the quota-related mount options actually in effect,
	// so the panel can show why enforcement is off rather than just that it
	// is.
	MountOptions string `json:"mount_options"`
	// PendingReboot is true when the boot loader has been told to enable
	// quota but the running kernel has not been restarted into it.
	PendingReboot bool   `json:"pending_reboot"`
	Filesystem    string `json:"filesystem"`
}

// QuotaSetRequest applies a hard limit to one account.
type QuotaSetRequest struct {
	Username string `json:"username"`
	// LimitMB is the hard limit. Zero removes the limit.
	LimitMB int64 `json:"limit_mb"`
}

// Validate checks the account and the size.
func (r *QuotaSetRequest) Validate() error {
	if !linuxuser.ValidOwner(r.Username) {
		return fmt.Errorf("%q is not an acceptable account name", r.Username)
	}
	if r.LimitMB < 0 {
		return errors.New("a quota cannot be negative")
	}
	return nil
}

// QuotaReport is what an account is using and what it may use.
type QuotaReport struct {
	Username  string `json:"username"`
	UsedBytes int64  `json:"used_bytes"`
	LimitMB   int64  `json:"limit_mb"`
	// Enforced distinguishes a real kernel limit from the du figure the
	// panel falls back to. The UI must say which it is showing.
	Enforced bool `json:"enforced"`
}

func registerXFSQuota(r *agent.Registry) {
	agent.Register(r, "quota.status", 1, func(ctx context.Context, _ struct{}) (QuotaStatus, error) {
		return quotaStatus(ctx), nil
	})

	agent.Register(r, "quota.set", 1, func(ctx context.Context, in QuotaSetRequest) (QuotaReport, error) {
		acct, err := linuxuser.Lookup(in.Username)
		if err != nil {
			return QuotaReport{}, err
		}
		if acct == nil {
			return QuotaReport{}, fmt.Errorf("account %q does not exist", in.Username)
		}
		st := quotaStatus(ctx)
		if !st.Enforced {
			// Recorded anyway: the mapping files are written so that the
			// limits take effect the moment the operator reboots, rather
			// than needing every account to be touched again afterwards.
			if err := writeProjectFiles(ctx); err != nil {
				return QuotaReport{}, err
			}
			return QuotaReport{Username: in.Username, LimitMB: in.LimitMB, Enforced: false},
				fmt.Errorf("quota is not enforced on %s (%s); reboot to apply",
					QuotaMount, st.MountOptions)
		}
		if err := applyProjectQuota(ctx, acct, in.LimitMB); err != nil {
			return QuotaReport{}, err
		}
		return quotaReport(ctx, in.Username, st.Enforced)
	})

	agent.Register(r, "quota.report", 1, func(ctx context.Context, in DiskUsageRequest) (QuotaReport, error) {
		return quotaReport(ctx, in.Username, quotaStatus(ctx).Enforced)
	})
}

// quotaStatus inspects the live mount rather than a configuration file: what
// matters is what the running kernel is doing, not what the boot loader was
// told at some point in the past.
func quotaStatus(ctx context.Context) QuotaStatus {
	st := QuotaStatus{Filesystem: QuotaMount}

	res, err := run.Cmd(ctx, []string{"findmnt", "-no", "SOURCE,OPTIONS", QuotaMount})
	if err == nil {
		fields := strings.Fields(res.Stdout)
		if len(fields) >= 2 {
			st.Filesystem = fields[0]
			st.MountOptions = fields[1]
		} else if len(fields) == 1 {
			st.MountOptions = fields[0]
		}
	}
	// prjquota means accounting and enforcement; pqnoenforce accounts but
	// does not refuse a write, which for our purpose is the same as off.
	st.Enforced = strings.Contains(st.MountOptions, "prjquota")

	if !st.Enforced {
		if cmdline, err := os.ReadFile("/proc/cmdline"); err == nil {
			// The argument is in the boot loader but not in the running
			// kernel: somebody has run the installer step and not rebooted.
			if !strings.Contains(string(cmdline), "pquota") && bootLoaderHasPquota(ctx) {
				st.PendingReboot = true
			}
		}
	}
	return st
}

// bootLoaderHasPquota asks grubby what the next boot will use.
func bootLoaderHasPquota(ctx context.Context) bool {
	res, err := run.Cmd(ctx, []string{"grubby", "--info=DEFAULT"}, run.Timeout(20*time.Second))
	if err != nil {
		return false
	}
	return strings.Contains(res.Stdout, "pquota")
}

// projectIDFor maps an account to a project id.
//
// The uid is reused rather than allocating a separate sequence: the two are
// already one-to-one, and an operator reading `xfs_quota -c report` sees a
// number they can match to a passwd entry instead of one they have to look
// up in the panel's database.
func projectIDFor(acct *linuxuser.Account) int64 { return acct.UID }

// writeProjectFiles rebuilds /etc/projects and /etc/projid from the accounts
// that exist.
//
// Rewritten wholesale rather than appended to: an account that was deleted
// must lose its mapping, and a file that only ever grows would eventually
// point project ids at directories belonging to somebody else.
func writeProjectFiles(ctx context.Context) error {
	accts, err := linuxuser.List(ctx)
	if err != nil {
		return err
	}
	var projects, projid strings.Builder
	projects.WriteString("# Managed by OPanel. Edits are overwritten.\n")
	projid.WriteString("# Managed by OPanel. Edits are overwritten.\n")
	for _, a := range accts {
		id := projectIDFor(&a)
		fmt.Fprintf(&projects, "%d:%s\n", id, a.Home)
		fmt.Fprintf(&projid, "%s:%d\n", a.Username, id)
	}
	if err := os.WriteFile(projectsFile, []byte(projects.String()), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", projectsFile, err)
	}
	return os.WriteFile(projidFile, []byte(projid.String()), 0o644)
}

// applyProjectQuota sets up the project and its limit.
func applyProjectQuota(ctx context.Context, acct *linuxuser.Account, limitMB int64) error {
	if err := writeProjectFiles(ctx); err != nil {
		return err
	}
	// -s walks the tree and stamps the project id on every inode. Needed
	// once per account, and again after a restore drops new files in.
	if _, err := run.Cmd(ctx, []string{
		"xfs_quota", "-x", "-c", "project -s " + acct.Username, QuotaMount,
	}, run.Timeout(10*time.Minute)); err != nil {
		return fmt.Errorf("stamp project on %s: %w", acct.Home, err)
	}

	// bhard is the hard limit; bsoft is set slightly under it so a customer
	// gets ENOSPC at the hard edge but tooling that reads soft limits can
	// warn earlier.
	soft := limitMB * 95 / 100
	limit := fmt.Sprintf("limit -p bsoft=%dm bhard=%dm %s", soft, limitMB, acct.Username)
	if limitMB == 0 {
		limit = "limit -p bsoft=0 bhard=0 " + acct.Username
	}
	if _, err := run.Cmd(ctx, []string{"xfs_quota", "-x", "-c", limit, QuotaMount},
		run.Timeout(time.Minute)); err != nil {
		return fmt.Errorf("set quota for %s: %w", acct.Username, err)
	}
	return nil
}

// quotaReport reads usage, from the kernel when it is enforcing and from du
// when it is not.
func quotaReport(ctx context.Context, username string, enforced bool) (QuotaReport, error) {
	if !enforced {
		used, err := duBytes(ctx, linuxuser.Home(username))
		return QuotaReport{Username: username, UsedBytes: used, Enforced: false}, err
	}

	// -b bytes, -N no header, -p project quota. The numbers come back in
	// kibibytes whatever the flags, which is why they are scaled here.
	res, err := run.Cmd(ctx, []string{
		"xfs_quota", "-x", "-c", "quota -p -N -b " + username, QuotaMount,
	}, run.Timeout(time.Minute))
	if err != nil {
		return QuotaReport{}, err
	}
	fields := strings.Fields(res.Stdout)
	// Layout: <project> <used> <soft> <hard> <warn/grace>
	if len(fields) < 4 {
		return QuotaReport{Username: username, Enforced: true}, nil
	}
	used, _ := strconv.ParseInt(fields[1], 10, 64)
	hard, _ := strconv.ParseInt(fields[3], 10, 64)
	return QuotaReport{
		Username: username, UsedBytes: used * 1024,
		LimitMB: hard / 1024, Enforced: true,
	}, nil
}
