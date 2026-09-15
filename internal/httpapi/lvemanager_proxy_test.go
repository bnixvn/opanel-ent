package httpapi

import (
	"net/http"
	"testing"

	"github.com/bnixvn/opanel-ent/internal/auth"
)

// The Manager builds its own request handler as baseUri + "/" + handler, and
// baseUri has to end in a slash for the <base href> it also goes into. So the
// doubled slash arrives on real requests, not just contrived ones.
func TestManagerPath(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"/lvemanager", "/"},
		{"/lvemanager/", "/"},
		{"/lvemanager//send-request.php", "/send-request.php"},
		{"/lvemanager/assets/css/style.css", "/assets/css/style.css"},
		{"/lvemanager///a//b", "/a/b"},
	} {
		if got := managerPath(tc.in); got != tc.want {
			t.Errorf("managerPath(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// A role the Manager does not know must come out as the least it can be. The
// failure this guards against is a role added to the panel later silently
// arriving at CloudLinux as an administrator.
func TestManagerRoleFailsLow(t *testing.T) {
	if got := managerRole(string(auth.RoleAdmin)); got != "admin" {
		t.Errorf("admin mapped to %q", got)
	}
	if got := managerRole(string(auth.RoleReseller)); got != "reseller" {
		t.Errorf("reseller mapped to %q", got)
	}
	for _, role := range []string{"", "root", "superuser", "END_USER"} {
		if got := managerRole(role); got != "user" {
			t.Errorf("managerRole(%q) = %q, want user", role, got)
		}
	}
}

// A CLSIDTOKEN the browser already holds -- the vendor's own service used to
// set one on this origin -- must not be the one the Manager reads. The panel's
// token replaces it, and every other cookie survives: the Manager's own CSRF
// cookie is among them.
func TestManagerTokenReplacesTheClientCookie(t *testing.T) {
	r, err := http.NewRequest(http.MethodGet, "/lvemanager/", nil)
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Cookie", "CLSIDTOKEN=stale; csrftoken=keepme")
	setManagerToken(r, "ours")

	var seen []string
	for _, c := range r.Cookies() {
		if c.Name == "CLSIDTOKEN" {
			seen = append(seen, c.Value)
		}
	}
	if len(seen) != 1 || seen[0] != "ours" {
		t.Errorf("CLSIDTOKEN came out as %v, want exactly [ours]", seen)
	}
	if got := readCookie(r, "csrftoken"); got != "keepme" {
		t.Errorf("csrftoken = %q, want keepme", got)
	}
}

func readCookie(r *http.Request, name string) string {
	c, err := r.Cookie(name)
	if err != nil {
		return ""
	}
	return c.Value
}
