package db

import "testing"

func TestScopeWhere(t *testing.T) {
	cases := []struct {
		name  string
		scope Scope
		want  string
		args  int
	}{
		{"administrator", ScopeAll(), "1 = 1", 0},
		{"end user", ScopeSelf(7), "s.owner_id = ?", 1},
		{"reseller", ScopeSubtree(3), "(s.owner_id = ? OR u.parent_id = ?)", 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, args := tc.scope.Where("s.owner_id", "u.parent_id")
			if got != tc.want {
				t.Errorf("Where = %q, want %q", got, tc.want)
			}
			if len(args) != tc.args {
				t.Errorf("got %d args, want %d", len(args), tc.args)
			}
		})
	}
}

// The table that matters: who can see whose rows. Getting one of these
// backwards shows a customer another customer's data.
func TestScopeCovers(t *testing.T) {
	const (
		admin     = 1
		resellerA = 2
		resellerB = 3
		custA     = 10 // belongs to resellerA
		custB     = 11 // belongs to resellerB
		direct    = 12 // belongs to the server itself
	)
	parent := map[int64]int64{custA: resellerA, custB: resellerB, direct: 0}

	cases := []struct {
		name  string
		scope Scope
		owner int64
		want  bool
	}{
		{"admin sees a reseller's customer", ScopeAll(), custA, true},
		{"admin sees a direct account", ScopeAll(), direct, true},

		{"reseller sees own customer", ScopeSubtree(resellerA), custA, true},
		{"reseller sees themselves", ScopeSubtree(resellerA), resellerA, true},
		{"reseller does NOT see another reseller's customer", ScopeSubtree(resellerA), custB, false},
		{"reseller does NOT see another reseller", ScopeSubtree(resellerA), resellerB, false},
		{"reseller does NOT see a direct account", ScopeSubtree(resellerA), direct, false},
		{"reseller does NOT see the administrator", ScopeSubtree(resellerA), admin, false},

		{"end user sees themselves", ScopeSelf(custA), custA, true},
		{"end user does NOT see a sibling", ScopeSelf(custA), custB, false},
		{"end user does NOT see their reseller", ScopeSelf(custA), resellerA, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.scope.Covers(tc.owner, parent[tc.owner]); got != tc.want {
				t.Fatalf("Covers(%d) = %v, want %v", tc.owner, got, tc.want)
			}
		})
	}
}

func TestScopeAnd(t *testing.T) {
	q, args := ScopeSelf(5).And("SELECT 1 FROM t", "owner_id", "parent_id", nil)
	if q != "SELECT 1 FROM t WHERE owner_id = ?" {
		t.Fatalf("got %q", q)
	}
	if len(args) != 1 {
		t.Fatalf("got %d args", len(args))
	}

	// An existing WHERE must be extended, not replaced.
	q, _ = ScopeSelf(5).And("SELECT 1 FROM t WHERE x = 1", "owner_id", "parent_id", nil)
	if q != "SELECT 1 FROM t WHERE x = 1 AND owner_id = ?" {
		t.Fatalf("got %q", q)
	}
}
