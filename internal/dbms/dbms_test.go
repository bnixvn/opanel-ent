package dbms

import "testing"

func TestValidDatabaseName(t *testing.T) {
	good := []string{"alice_wordpress", "a", "wp_1", "x_______"}
	for _, s := range good {
		if !ValidDatabaseName(s) {
			t.Errorf("ValidDatabaseName(%q) = false, want true", s)
		}
	}
	// Identifiers are interpolated into SQL, so anything that could end the
	// quoting or start a new statement must be rejected outright.
	bad := []string{
		"", "Alice", "1wp", "wp-1", "wp db", "wp;DROP DATABASE x",
		"wp`", "wp'", `wp"`, "wp\\", "wp\n", "../etc",
	}
	for _, s := range bad {
		if ValidDatabaseName(s) {
			t.Errorf("ValidDatabaseName(%q) = true, want false", s)
		}
	}
	long := make([]byte, MaxDatabaseName+1)
	for i := range long {
		long[i] = 'a'
	}
	if ValidDatabaseName(string(long)) {
		t.Error("a name longer than MariaDB allows was accepted")
	}
}

func TestValidUserName(t *testing.T) {
	if !ValidUserName("alice_wp") {
		t.Error("a normal account name was rejected")
	}
	long := make([]byte, MaxUserName+1)
	for i := range long {
		long[i] = 'a'
	}
	if ValidUserName(string(long)) {
		t.Errorf("an account name longer than %d characters was accepted", MaxUserName)
	}
}

func TestValidPassword(t *testing.T) {
	// MariaDB rejects a placeholder in CREATE USER, so the password is
	// written into the statement. These are the characters that make that
	// safe; anything else must be refused rather than escaped.
	if !ValidPassword("abcDEF123_-xyz") {
		t.Error("a generated-style password was rejected")
	}
	bad := []string{
		"short", "", "has space here",
		"quote'injection",
		`double"quote`,
		`back\slash`,
		"semi;colon",
		"new\nline",
		"tick`mark",
	}
	for _, p := range bad {
		if ValidPassword(p) {
			t.Errorf("ValidPassword(%q) = true, want false", p)
		}
	}
}

func TestQuoteIdent(t *testing.T) {
	if got := quoteIdent("alice_wp"); got != "`alice_wp`" {
		t.Errorf("quoteIdent = %q", got)
	}
	// namePattern already rejects a backtick; the doubling is the second line
	// of defence and must still work.
	if got := quoteIdent("a`b"); got != "`a``b`" {
		t.Errorf("quoteIdent did not double the backtick: %q", got)
	}
}

func TestPrefixed(t *testing.T) {
	if got := Prefixed("alice", "wordpress"); got != "alice_wordpress" {
		t.Errorf("Prefixed = %q", got)
	}
}
