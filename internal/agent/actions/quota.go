package actions

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/bnixvn/opanel-ent/internal/agent"
	"github.com/bnixvn/opanel-ent/internal/platform/linuxuser"
	"github.com/bnixvn/opanel-ent/internal/platform/run"
)

// DiskUsageRequest asks how much space an account is using.
type DiskUsageRequest struct {
	Username string `json:"username"`
}

// Validate checks the account name.
func (r *DiskUsageRequest) Validate() error {
	if !linuxuser.ValidName(r.Username) {
		return fmt.Errorf("%q is not an acceptable account name", r.Username)
	}
	return nil
}

// DiskUsageResult reports bytes used under an account's home.
type DiskUsageResult struct {
	Username string `json:"username"`
	Bytes    int64  `json:"bytes"`
}

func registerQuota(r *agent.Registry) {
	// Measured with du rather than a filesystem quota.
	//
	// The target host runs XFS mounted noquota with no separate /home, and
	// XFS cannot enable quota on a remount — it needs rootflags=uquota in the
	// kernel command line and a reboot. Asking an operator to reboot before
	// the panel works is worse than an accounting figure that is a few
	// minutes stale, so this is a soft limit and is documented as one.
	agent.Register(r, "quota.usage", 1, func(ctx context.Context, in DiskUsageRequest) (DiskUsageResult, error) {
		home := linuxuser.Home(in.Username)
		res, err := run.Cmd(ctx, []string{"du", "-sb", "--", home},
			run.Timeout(2*time.Minute), run.AllowExit(1)) // exit 1 is a warning about an unreadable file
		if err != nil {
			return DiskUsageResult{}, fmt.Errorf("measure %s: %w", home, err)
		}
		fields := strings.Fields(res.Stdout)
		if len(fields) == 0 {
			return DiskUsageResult{Username: in.Username}, nil
		}
		n, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil {
			return DiskUsageResult{}, fmt.Errorf("parse du output %q: %w", fields[0], err)
		}
		return DiskUsageResult{Username: in.Username, Bytes: n}, nil
	})
}

// duBytes measures a directory the slow way. Still needed as the fallback for
// a host where project quota has been configured but not yet rebooted into.
func duBytes(ctx context.Context, path string) (int64, error) {
	res, err := run.Cmd(ctx, []string{"du", "-sb", "--", path},
		run.Timeout(2*time.Minute), run.AllowExit(1))
	if err != nil {
		return 0, fmt.Errorf("measure %s: %w", path, err)
	}
	fields := strings.Fields(res.Stdout)
	if len(fields) == 0 {
		return 0, nil
	}
	return strconv.ParseInt(fields[0], 10, 64)
}
