package actions

import (
	"testing"

	"github.com/bnixvn/opanel-ent/internal/webserver"
)

func TestAltPHPTag(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"8.4", "84"},
		{"7.4", "74"},
		{"5.3", "53"},
		{"8.10", "810"},
		{"", ""},
		{"8", ""},
		{"8.", ""},
		{".4", ""},
		{"8.4; rm -rf /", ""},
		{"8.x", ""},
		{"native", ""},
	} {
		if got := altPHPTag(tc.in); got != tc.want {
			t.Errorf("altPHPTag(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// The guard's whole job is to tell a site LiteSpeed can run from one it
// cannot. Getting this backwards in either direction is expensive: too strict
// and a switch that was safe is refused, too loose and a site written for 8.4
// runs on the 7.2 LiteSpeed ships with, silently, with a 200 for every
// request.
func TestCanServePHPAsksAboutTheInterpreter(t *testing.T) {
	php := func(domain, version, handler string) webserver.Site {
		return webserver.Site{
			Domain: domain, AppType: webserver.AppPHP,
			PHPVersion: version, LSPHPHandler: handler,
		}
	}
	static := webserver.Site{Domain: "static.test", AppType: webserver.AppStatic}

	for _, tc := range []struct {
		name    string
		backend string
		sites   []webserver.Site
		refuse  bool
	}{
		{"apache never refuses", webserver.BackendApache,
			[]webserver.Site{php("a.test", "8.4", "")}, false},
		{"no sites", webserver.BackendLSWS, nil, false},
		{"static only", webserver.BackendLSWS,
			[]webserver.Site{static}, false},
		{"every php site has an interpreter", webserver.BackendLSWS,
			[]webserver.Site{php("a.test", "8.4", "application/x-httpd-alt-php84"), static}, false},
		{"one site has none", webserver.BackendLSWS,
			[]webserver.Site{php("a.test", "8.4", "application/x-httpd-alt-php84"), php("b.test", "7.4", "")}, true},
	} {
		err := canServePHP(tc.backend, tc.sites)
		if tc.refuse && err == nil {
			t.Errorf("%s: allowed, want refused", tc.name)
		}
		if !tc.refuse && err != nil {
			t.Errorf("%s: refused (%v), want allowed", tc.name, err)
		}
	}
}
