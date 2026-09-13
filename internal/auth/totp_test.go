package auth

import (
	"testing"

	"github.com/pquerna/otp/totp"
)

func TestTOTPSetupAndVerify(t *testing.T) {
	s, err := NewTOTPSetup("OPanel", "alice@example.test")
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	if s.Secret == "" || s.URI == "" {
		t.Fatal("setup returned an empty secret or URI")
	}
	if len(s.RecoveryCodes) != RecoveryCodeCount {
		t.Fatalf("got %d recovery codes, want %d", len(s.RecoveryCodes), RecoveryCodeCount)
	}

	code, err := totp.GenerateCode(s.Secret, timeNow())
	if err != nil {
		t.Fatalf("generate code: %v", err)
	}
	if !VerifyTOTP(s.Secret, code) {
		t.Fatal("a freshly generated code did not verify")
	}
	if VerifyTOTP(s.Secret, "000000") {
		t.Fatal("an arbitrary code verified")
	}
	if VerifyTOTP("", code) {
		t.Fatal("verification succeeded against an empty secret")
	}
}

func TestRecoveryCodesAreDistinct(t *testing.T) {
	codes, err := NewRecoveryCodes(20)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	seen := make(map[string]bool, len(codes))
	for _, c := range codes {
		if seen[c] {
			t.Fatalf("duplicate recovery code %q", c)
		}
		seen[c] = true
	}
}

func TestRecoveryCodeNormalisation(t *testing.T) {
	// Users retype these off paper, so dashes and stray spaces must not
	// change the hash.
	want := HashRecoveryCode("abcd1234")
	for _, variant := range []string{" abcd1234 ", "abcd-1234", "ab cd 12 34"} {
		if got := HashRecoveryCode(variant); got != want {
			t.Fatalf("variant %q hashed differently", variant)
		}
	}
}
