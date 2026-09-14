package wordpress

import "testing"

func TestDBSuffix(t *testing.T) {
	cases := map[string]string{
		"shop.example.com":              "shop",
		"example.com":                   "example",
		"WWW.Example.COM":               "www",
		"my-shop.example.com":           "my_shop",
		"123shop.example.com":           "wp123shop",
		"----.example.com":              "wp",
		"averyveryverylongfirstlabel.x": "averyveryverylon",
	}
	for domain, want := range cases {
		if got := dbSuffix(domain); got != want {
			t.Errorf("dbSuffix(%q) = %q, want %q", domain, got, want)
		}
	}
}

// The suffix reaches MariaDB as part of an identifier, so whatever it
// produces has to survive the validator the database service applies.
func TestDBSuffixAlwaysUsable(t *testing.T) {
	for _, domain := range []string{
		"shop.example.com", "123.example.com", "_.example.com", "-", "",
		"ÄÖÜ.example.com", "a.b.c.d.e.f",
	} {
		got := dbSuffix(domain)
		if got == "" {
			t.Errorf("dbSuffix(%q) produced an empty name", domain)
			continue
		}
		if c := got[0]; c < 'a' || c > 'z' {
			t.Errorf("dbSuffix(%q) = %q, which does not start with a letter", domain, got)
		}
		for _, r := range got {
			ok := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_'
			if !ok {
				t.Errorf("dbSuffix(%q) = %q, which contains %q", domain, got, r)
				break
			}
		}
	}
}
