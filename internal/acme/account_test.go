package acme

import (
	"strings"
	"testing"
)

// The bug this prevents: an account key registered at the production CA does
// not exist at the staging one, and lego fails with "KeyID header contained
// an invalid account URL". That turns staging -- the safe way to test an
// issuance -- into the one thing that cannot work on a host which has
// already issued a real certificate.
func TestAccountDirSeparatesStagingFromProduction(t *testing.T) {
	prod := (&Manager{StateDir: "/state", CADirURL: ProductionCA, Email: "ops@example.test"}).accountDir()
	stage := (&Manager{StateDir: "/state", CADirURL: StagingCA, Email: "ops@example.test"}).accountDir()
	if prod == stage {
		t.Fatalf("staging and production share an account directory: %s", prod)
	}
	if !strings.Contains(prod, "production") || !strings.Contains(stage, "staging") {
		t.Fatalf("directories are not named for their CA: %s / %s", prod, stage)
	}
}

// Let's Encrypt sends the expiry warning to the account contact, not to the
// order, so one shared account would send every customer's warning to
// whoever registered first.
func TestAccountDirSeparatesContacts(t *testing.T) {
	a := (&Manager{StateDir: "/state", CADirURL: ProductionCA, Email: "alice@example.test"}).accountDir()
	b := (&Manager{StateDir: "/state", CADirURL: ProductionCA, Email: "bob@example.test"}).accountDir()
	none := (&Manager{StateDir: "/state", CADirURL: ProductionCA}).accountDir()

	for _, pair := range [][2]string{{a, b}, {a, none}, {b, none}} {
		if pair[0] == pair[1] {
			t.Fatalf("two contacts share an account directory: %s", pair[0])
		}
	}
}

// Case is not part of an address's identity, and treating it as one would
// quietly register a second account the first time somebody typed their own
// address with a capital letter.
func TestAccountDirIgnoresCase(t *testing.T) {
	lower := (&Manager{StateDir: "/state", CADirURL: ProductionCA, Email: "ops@example.test"}).accountDir()
	upper := (&Manager{StateDir: "/state", CADirURL: ProductionCA, Email: "OPS@Example.Test"}).accountDir()
	if lower != upper {
		t.Fatalf("case changed the account directory: %s vs %s", lower, upper)
	}
}
