package panelusers

import (
	"context"
	"errors"
	"fmt"

	"github.com/bnixvn/opanel-ent/internal/auth"
	"github.com/bnixvn/opanel-ent/internal/db"
)

// ErrAllocationExceeded means a reseller has already handed out everything
// their own allowance covers.
var ErrAllocationExceeded = errors.New("panelusers: this would exceed your allowance")

// ErrNotYourCustomer means the caller may see the account but not change it.
var ErrNotYourCustomer = errors.New("panelusers: that account is not yours to manage")

// CheckCanCreateAccount decides whether actor may add one more customer.
//
// Administrators are unlimited. A reseller is measured against what they have
// already *allocated*, not what their customers are using: a customer sitting
// at 1% of a 10 GB package has still had 10 GB promised to them, and promising
// the same gigabyte twice is what oversell means.
func (s *Service) CheckCanCreateAccount(ctx context.Context, actor *db.User, planDiskMB int) error {
	if auth.Role(actor.Role) != auth.RoleReseller {
		return nil
	}
	limits, err := s.db.ResellerLimitsFor(ctx, actor.ID)
	if err != nil {
		return err
	}
	used, err := s.db.AllocationFor(ctx, actor.ID)
	if err != nil {
		return err
	}

	if limits.MaxAccounts > 0 && used.Accounts >= limits.MaxAccounts {
		return fmt.Errorf("%w: %d of %d accounts already created",
			ErrAllocationExceeded, used.Accounts, limits.MaxAccounts)
	}
	if limits.AllowOversell || limits.DiskQuotaMB == 0 {
		return nil
	}
	if used.DiskQuotaMB+planDiskMB > limits.DiskQuotaMB {
		return fmt.Errorf("%w: %d MB of %d MB already allocated, this package needs %d MB more",
			ErrAllocationExceeded, used.DiskQuotaMB, limits.DiskQuotaMB, planDiskMB)
	}
	return nil
}

// CheckCanAllocate is the same test for changing an existing customer's
// package, where the delta rather than the whole package is what is new.
func (s *Service) CheckCanAllocate(ctx context.Context, actor *db.User, deltaDiskMB int) error {
	if auth.Role(actor.Role) != auth.RoleReseller || deltaDiskMB <= 0 {
		return nil
	}
	limits, err := s.db.ResellerLimitsFor(ctx, actor.ID)
	if err != nil {
		return err
	}
	if limits.AllowOversell || limits.DiskQuotaMB == 0 {
		return nil
	}
	used, err := s.db.AllocationFor(ctx, actor.ID)
	if err != nil {
		return err
	}
	if used.DiskQuotaMB+deltaDiskMB > limits.DiskQuotaMB {
		return fmt.Errorf("%w: %d MB of %d MB already allocated",
			ErrAllocationExceeded, used.DiskQuotaMB, limits.DiskQuotaMB)
	}
	return nil
}

// ResellerSummary is what the panel shows a reseller about their own
// allowance, and what an administrator sees for each reseller.
type ResellerSummary struct {
	Limits     *db.ResellerLimits
	Allocation db.ResellerAllocation
}

// SummaryFor gathers a reseller's allowance and what they have used of it.
func (s *Service) SummaryFor(ctx context.Context, resellerID int64) (*ResellerSummary, error) {
	limits, err := s.db.ResellerLimitsFor(ctx, resellerID)
	if err != nil {
		return nil, err
	}
	used, err := s.db.AllocationFor(ctx, resellerID)
	if err != nil {
		return nil, err
	}
	return &ResellerSummary{Limits: limits, Allocation: used}, nil
}

// SetLimits records what a reseller may hand out. Administrators only; the
// handler enforces that, and this refuses a non-reseller so a mistyped id
// cannot quietly create an allowance for an end user.
func (s *Service) SetLimits(ctx context.Context, l *db.ResellerLimits) error {
	target, err := s.db.UserByID(ctx, l.UserID)
	if err != nil {
		return err
	}
	if auth.Role(target.Role) != auth.RoleReseller {
		return fmt.Errorf("%s is not a reseller", target.Username)
	}
	for _, f := range []struct {
		name string
		val  int
	}{
		{"max_accounts", l.MaxAccounts}, {"max_sites", l.MaxSites},
		{"max_databases", l.MaxDatabases}, {"disk_quota_mb", l.DiskQuotaMB},
		{"bandwidth_gb", l.BandwidthGB},
	} {
		if f.val < 0 {
			return fmt.Errorf("%s cannot be negative", f.name)
		}
	}
	return s.db.SaveResellerLimits(ctx, l)
}

// AssignParent puts a customer under a reseller.
func (s *Service) AssignParent(ctx context.Context, userID, parentID int64) error {
	if parentID != 0 {
		parent, err := s.db.UserByID(ctx, parentID)
		if err != nil {
			return err
		}
		if auth.Role(parent.Role) != auth.RoleReseller {
			return fmt.Errorf("%s is not a reseller", parent.Username)
		}
		if parentID == userID {
			return errors.New("an account cannot be its own reseller")
		}
		target, err := s.db.UserByID(ctx, userID)
		if err != nil {
			return err
		}
		// Depth two only. Allowing a reseller under a reseller would turn
		// every ownership check into a recursive walk, and nothing in the
		// product needs it.
		if auth.Role(target.Role) != auth.RoleEndUser {
			return errors.New("only an end user can belong to a reseller")
		}
	}
	return s.db.SetUserParent(ctx, userID, parentID)
}
