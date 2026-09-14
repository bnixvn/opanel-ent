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
