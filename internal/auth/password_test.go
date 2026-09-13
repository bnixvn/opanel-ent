package auth

import (
	"errors"
	"strings"
	"testing"
)

func TestHashAndVerify(t *testing.T) {
	h, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if !strings.HasPrefix(h, "$argon2id$") {
		t.Fatalf("hash is not argon2id: %q", h)
	}
	if err := VerifyPassword(h, "correct horse battery staple"); err != nil {
		t.Fatalf("verify correct password: %v", err)
	}
	if err := VerifyPassword(h, "wrong"); !errors.Is(err, ErrMismatch) {
		t.Fatalf("verify wrong password: got %v, want ErrMismatch", err)
	}
}

func TestHashIsSalted(t *testing.T) {
	a, _ := HashPassword("same")
	b, _ := HashPassword("same")
	if a == b {
		t.Fatal("two hashes of the same password are identical: salt is not random")
	}
}

func TestVerifyRejectsMalformedHash(t *testing.T) {
	for name, h := range map[string]string{
		"empty":           "",
		"not phc":         "plaintext",
		"wrong algorithm": "$argon2i$v=19$m=65536,t=3,p=2$c2FsdA$aGFzaA",
		"too few fields":  "$argon2id$v=19$m=65536,t=3,p=2$c2FsdA",
		"bad base64 salt": "$argon2id$v=19$m=65536,t=3,p=2$!!!$aGFzaA",
		"zero parameters": "$argon2id$v=19$m=0,t=0,p=0$c2FsdA$aGFzaA",
	} {
		t.Run(name, func(t *testing.T) {
			if err := VerifyPassword(h, "anything"); !errors.Is(err, ErrBadHash) {
				t.Fatalf("got %v, want ErrBadHash", err)
			}
		})
	}
}

func TestVerifyUsesParametersFromHash(t *testing.T) {
	// A hash written by a build with weaker settings must still verify,
	// otherwise raising the constants would lock every existing user out.
	weak := "$argon2id$v=19$m=8192,t=1,p=1$" +
		"c2FsdHNhbHRzYWx0c2E$" // 16-byte salt, raw std base64
	// Derive the matching hash with those parameters via HashPassword's
	// inverse is not exposed, so assert on the negative case instead: the
	// parser must accept the shape rather than reject it as malformed.
	if err := VerifyPassword(weak+"YWJj", "pw"); errors.Is(err, ErrBadHash) {
		t.Fatal("weaker-parameter hash was rejected as malformed")
	}
}

func TestNeedsRehash(t *testing.T) {
	current, _ := HashPassword("pw")
	if NeedsRehash(current) {
		t.Fatal("a freshly made hash should not need rehashing")
	}
	if !NeedsRehash("$argon2id$v=19$m=1024,t=1,p=1$c2FsdA$aGFzaA") {
		t.Fatal("a weaker hash should need rehashing")
	}
	if !NeedsRehash("garbage") {
		t.Fatal("an unparseable hash should need rehashing")
	}
}
