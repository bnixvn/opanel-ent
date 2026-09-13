package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"
)

// randomToken returns n cryptographically random bytes as URL-safe base64.
func randomToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("auth: read random: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// randomHex returns n cryptographically random bytes as lowercase hex.
//
// Used where the value must survive being split on "_": hex has no character
// in common with the base64url alphabet's "-" and "_".
func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("auth: read random: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// hashSecret is the one-way transform applied to every bearer secret before
// storage: session cookies, API tokens and TOTP recovery codes.
//
// These are high-entropy random values, not passwords, so a plain SHA-256 is
// the right tool — there is nothing to brute-force and no reason to pay
// argon2's cost on every authenticated request.
func hashSecret(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// secretsEqual compares two hashes without leaking timing information.
func secretsEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
