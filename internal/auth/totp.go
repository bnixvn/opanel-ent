package auth

import (
	"fmt"
	"strings"

	"github.com/pquerna/otp"
	"github.com/pquerna/otp/totp"
)

// RecoveryCodeCount is how many single-use codes are issued when 2FA is
// enabled or regenerated.
const RecoveryCodeCount = 10

// TOTPSetup is the material handed to a user once, at enrolment.
type TOTPSetup struct {
	Secret        string   // base32, to be stored on the user
	URI           string   // otpauth:// URI for QR rendering
	RecoveryCodes []string // plaintext, shown once and never again
}

// NewTOTPSetup generates a secret and a fresh batch of recovery codes.
// Nothing is persisted here; the caller stores the secret and the code hashes
// only after the user proves they can produce a valid code.
func NewTOTPSetup(issuer, account string) (*TOTPSetup, error) {
	key, err := totp.Generate(totp.GenerateOpts{
		Issuer:      issuer,
		AccountName: account,
		Period:      30,
		Digits:      otp.DigitsSix,
		Algorithm:   otp.AlgorithmSHA1, // what Google Authenticator and Authy accept
	})
	if err != nil {
		return nil, fmt.Errorf("auth: generate totp: %w", err)
	}
	codes, err := NewRecoveryCodes(RecoveryCodeCount)
	if err != nil {
		return nil, err
	}
	return &TOTPSetup{Secret: key.Secret(), URI: key.URL(), RecoveryCodes: codes}, nil
}

// VerifyTOTP checks a 6-digit code against the secret, tolerating one period
// of clock skew in each direction.
func VerifyTOTP(secret, code string) bool {
	if secret == "" {
		return false
	}
	return totp.Validate(strings.TrimSpace(code), secret)
}

// NewRecoveryCodes returns n plaintext recovery codes.
func NewRecoveryCodes(n int) ([]string, error) {
	out := make([]string, 0, n)
	for range n {
		// 10 bytes -> 16 base64url chars: enough entropy that these need no
		// rate limiting of their own beyond the login limiter.
		c, err := randomToken(10)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, nil
}

// HashRecoveryCodes maps plaintext codes to their stored form.
func HashRecoveryCodes(codes []string) []string {
	out := make([]string, len(codes))
	for i, c := range codes {
		out[i] = hashSecret(normalizeRecoveryCode(c))
	}
	return out
}

// HashRecoveryCode prepares a user-supplied code for lookup, applying the
// same normalisation used at generation time.
func HashRecoveryCode(code string) string {
	return hashSecret(normalizeRecoveryCode(code))
}

// normalizeRecoveryCode strips whitespace and the dashes users often type
// when copying a code off paper.
func normalizeRecoveryCode(c string) string {
	c = strings.TrimSpace(c)
	c = strings.ReplaceAll(c, "-", "")
	c = strings.ReplaceAll(c, " ", "")
	return c
}
