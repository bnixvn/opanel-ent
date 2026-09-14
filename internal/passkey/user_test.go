package passkey

import (
	"testing"

	"github.com/go-webauthn/webauthn/webauthn"

	"github.com/bnixvn/opanel-ent/internal/db"
)

// The library refuses a sign-in when the BackupEligible flag on the
// assertion differs from the one recorded at registration. A phone passkey
// is always eligible, so a round trip that loses the flag rejects every one
// of them -- which it did, and this is the guard against it happening again.
func TestRowRoundTripKeepsBackupFlags(t *testing.T) {
	cred := &webauthn.Credential{
		ID:        []byte{1, 2, 3, 4},
		PublicKey: []byte{5, 6, 7, 8},
	}
	cred.Flags.BackupEligible = true
	cred.Flags.BackupState = true

	row := Row(7, "phone", cred)
	if row.BackupEligible == nil || !*row.BackupEligible {
		t.Fatalf("BackupEligible was not stored: %v", row.BackupEligible)
	}
	if row.BackupState == nil || !*row.BackupState {
		t.Fatalf("BackupState was not stored: %v", row.BackupState)
	}

	u := &User{Account: &db.User{ID: 7}, Credentials: []*db.Passkey{row}}
	back := u.WebAuthnCredentials()
	if len(back) != 1 {
		t.Fatalf("want one credential, got %d", len(back))
	}
	if !back[0].Flags.BackupEligible {
		t.Error("BackupEligible did not survive the round trip")
	}
	if !back[0].Flags.BackupState {
		t.Error("BackupState did not survive the round trip")
	}
}

// A credential registered before the columns existed knows neither flag, and
// must not be read as "ineligible" -- that is the state that locked people
// out. It takes the values from the assertion instead.
func TestAdoptUnknownFlags(t *testing.T) {
	unknown := &db.Passkey{ID: "a"}
	known := false
	settled := &db.Passkey{ID: "b", BackupEligible: &known, BackupState: &known}

	u := &User{Account: &db.User{ID: 1}, Credentials: []*db.Passkey{unknown, settled}}

	if !AdoptUnknownFlags(u, true, true) {
		t.Fatal("want the unknown row to be adopted")
	}
	if unknown.BackupEligible == nil || !*unknown.BackupEligible {
		t.Error("the unknown row did not take the assertion's BackupEligible")
	}
	if unknown.BackupState == nil || !*unknown.BackupState {
		t.Error("the unknown row did not take the assertion's BackupState")
	}
	// A row that already knows is not overwritten: its flag is what the
	// authenticator committed to, and adopting over it would defeat the
	// check the library is making.
	if *settled.BackupEligible {
		t.Error("a known flag was overwritten")
	}
	if AdoptUnknownFlags(u, true, true) {
		t.Error("a second pass should have nothing left to adopt")
	}
}
