package linuxuser

import "testing"

func TestValidName(t *testing.T) {
	for _, s := range []string{"alice", "bob_1", "a-b-c", "site01"} {
		if !ValidName(s) {
			t.Errorf("ValidName(%q) = false, want true", s)
		}
	}
	// A name becomes a Linux account that PHP runs as and a directory under
	// /home, so anything that could collide with a system account or escape a
	// path must be refused.
	bad := []string{
		"", "ab", "Alice", "1alice", "-alice", "_alice",
		"alice bob", "alice/../root", "root", "mysql", "opanel", "nobody",
		"admin", "lsadm", "www-data", "alice$", "alice.name",
	}
	for _, s := range bad {
		if ValidName(s) {
			t.Errorf("ValidName(%q) = true, want false", s)
		}
	}
	long := make([]byte, 33)
	for i := range long {
		long[i] = 'a'
	}
	if ValidName(string(long)) {
		t.Error("a name longer than 32 characters was accepted")
	}
}

func TestHome(t *testing.T) {
	if got := Home("alice"); got != "/home/alice" {
		t.Errorf("Home = %q", got)
	}
}

// The three name rules answer three different questions, and conflating them
// is what broke the panel for an operator whose account is called admin:
// every action refused to touch it, because the rule for handing names out
// was being asked whether an existing account could be used.
func TestNameRulesAnswerDifferentQuestions(t *testing.T) {
	cases := []struct {
		name                          string
		plausible, validOwner, create bool
	}{
		// A system account: never, by any rule.
		{"root", true, false, false},
		{"mysql", true, false, false},
		{"opanel", true, false, false},
		// Confusing, but a real account the panel may have made. Refused to a
		// new customer; usable once it exists.
		{"admin", true, true, false},
		{"test", true, true, false},
		// An ordinary customer.
		{"alice", true, true, true},
		{"alice_deploy", true, true, true},
		// Not the right shape at all.
		{"Alice", false, false, false},
		{"../etc", false, false, false},
		{"a", false, false, false},
	}
	for _, c := range cases {
		if got := PlausibleName(c.name); got != c.plausible {
			t.Errorf("PlausibleName(%q) = %v, want %v", c.name, got, c.plausible)
		}
		if got := ValidOwner(c.name); got != c.validOwner {
			t.Errorf("ValidOwner(%q) = %v, want %v", c.name, got, c.validOwner)
		}
		if got := ValidName(c.name); got != c.create {
			t.Errorf("ValidName(%q) = %v, want %v", c.name, got, c.create)
		}
	}
}
