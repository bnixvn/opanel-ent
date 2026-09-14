package auth

import (
	"testing"

	"github.com/bnixvn/opanel-ent/internal/db"
)

func user(id int64, role Role, parent int64) *db.User {
	u := &db.User{ID: id, Role: string(role)}
	if parent != 0 {
		u.ParentID = &parent
	}
	return u
}

func TestCanManage(t *testing.T) {
	admin := user(1, RoleAdmin, 0)
	admin2 := user(2, RoleAdmin, 0)
	resA := user(3, RoleReseller, 0)
	resB := user(4, RoleReseller, 0)
	custA := user(10, RoleEndUser, resA.ID)
	custB := user(11, RoleEndUser, resB.ID)
	direct := user(12, RoleEndUser, 0)

	cases := []struct {
		name   string
		actor  *db.User
		target *db.User
		want   bool
	}{
		{"admin manages a reseller", admin, resA, true},
		{"admin manages another admin", admin, admin2, true},
		{"admin manages a direct account", admin, direct, true},

		{"reseller manages own customer", resA, custA, true},
		// The three that would be a breach:
		{"reseller does NOT manage another reseller's customer", resA, custB, false},
		{"reseller does NOT manage another reseller", resA, resB, false},
		{"reseller does NOT manage an administrator", resA, admin, false},
		{"reseller does NOT manage a direct account", resA, direct, false},

		{"end user manages nobody", custA, custB, false},
		{"end user does NOT manage their reseller", custA, resA, false},

		// Changing your own role or suspension is not a management action;
		// it goes through the account page, which has its own rules.
		{"nobody manages themselves", admin, admin, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := CanManage(tc.actor, tc.target); got != tc.want {
				t.Fatalf("CanManage = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestScopeForMatchesRole(t *testing.T) {
	if s := ScopeFor(user(1, RoleAdmin, 0)); !s.Everything {
		t.Error("an administrator did not get the whole server")
	}
	s := ScopeFor(user(3, RoleReseller, 0))
	if s.Everything || !s.Children || s.SelfID != 3 {
		t.Errorf("a reseller got %+v", s)
	}
	s = ScopeFor(user(10, RoleEndUser, 3))
	if s.Everything || s.Children || s.SelfID != 10 {
		t.Errorf("an end user got %+v", s)
	}
}
