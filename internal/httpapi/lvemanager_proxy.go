package httpapi

import (
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/bnixvn/opanel-ent/internal/auth"
	"github.com/bnixvn/opanel-ent/internal/cloudlinux"
)

// lveManagerProxy serves CloudLinux Manager through the panel.
//
// The Manager is CloudLinux's own interface -- current usage, users,
// statistics, options, packages, the selectors -- and reimplementing it would
// be a worse version of something that already exists. So the panel serves
// it rather than copying it.
//
// Three things travel with every proxied request, and together they are why
// the Manager asks for no password of its own:
//
//   - X-OPanel-Auth, the shared secret. vendor.php refuses any request that
//     cannot present it. The Manager's vhost is on the loopback address, and
//     on a hosting server that is not a boundary -- every customer with a
//     shell is already inside it -- so the proxy has to prove it is the panel.
//   - X-OPanel-User and X-OPanel-Role, the identity the panel has already
//     established. The Manager decides what to show from these.
//   - CLSIDTOKEN, a session token. The Manager refuses a request without one.
//     It carries no authority here: the identity is in the headers above, and
//     ui_user_info throws the token away.
func (s *Server) lveManagerProxy() http.Handler {
	target := &url.URL{
		Scheme: "http",
		Host:   "127.0.0.1:" + strconv.Itoa(cloudlinux.ManagerPort),
	}
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.URL.Path = managerPath(pr.In.URL.Path)
			pr.Out.Host = pr.In.Host
			pr.Out.Header.Set("X-Forwarded-Proto", "https")
			pr.Out.Header.Set("X-Forwarded-Prefix", cloudlinux.ManagerPrefix)

			u := userFrom(pr.In.Context())
			secret, err := cloudlinux.ManagerSecret()
			if err != nil || u == nil {
				// Nothing to prove the request with. Strip anything the
				// client may have sent under these names and let vendor.php
				// refuse it, rather than passing a half-formed identity on.
				pr.Out.Header.Del("X-OPanel-Auth")
				pr.Out.Header.Del("X-OPanel-User")
				pr.Out.Header.Del("X-OPanel-Role")
				return
			}
			pr.Out.Header.Set("X-OPanel-Auth", secret)
			pr.Out.Header.Set("X-OPanel-User", u.Username)
			pr.Out.Header.Set("X-OPanel-Role", managerRole(u.Role))
			setManagerToken(pr.Out, cloudlinux.ManagerToken(secret, u.Username))
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			s.log.Warn("httpapi: CloudLinux Manager is not reachable", "err", err)
			writeError(w, http.StatusBadGateway, "lvemanager_down",
				"CloudLinux Manager is not running. Install it from the CloudLinux page.")
		},
		FlushInterval: 250 * time.Millisecond,
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxy.ServeHTTP(&lveManagerResponseWriter{ResponseWriter: w}, r)
	})
}

// managerPath turns a panel path into the path inside the Manager's vhost.
//
// The doubled slash is not hypothetical: the Manager builds its own request
// handler as baseUri + "/" + "send-request.php", and baseUri has to end in a
// slash for the <base href> it also goes into to resolve relative links
// inside the Manager rather than one directory above it.
func managerPath(p string) string {
	out := strings.TrimPrefix(p, cloudlinux.ManagerPrefix)
	for strings.Contains(out, "//") {
		out = strings.ReplaceAll(out, "//", "/")
	}
	if out == "" {
		out = "/"
	}
	return out
}

// managerRole maps a panel role onto the three the Manager knows.
//
// Anything unrecognised becomes "user", the least it can be. vendor.php makes
// the same decision again at the other end: this is the kind of mapping where
// a new role added later should show somebody too little rather than too much.
func managerRole(role string) string {
	switch auth.Role(role) {
	case auth.RoleAdmin:
		return "admin"
	case auth.RoleReseller:
		return "reseller"
	default:
		return "user"
	}
}

// setManagerToken replaces any CLSIDTOKEN the client sent with the panel's.
//
// Replaces rather than adds: a token left over from some earlier arrangement
// -- the vendor's own service used to set one on this origin -- would
// otherwise be the one the Manager read.
func setManagerToken(r *http.Request, token string) {
	kept := make([]string, 0, 4)
	for _, c := range r.Cookies() {
		if c.Name != "CLSIDTOKEN" {
			kept = append(kept, c.Name+"="+c.Value)
		}
	}
	kept = append(kept, "CLSIDTOKEN="+token)
	r.Header.Set("Cookie", strings.Join(kept, "; "))
}

// lveManagerResponseWriter keeps redirects inside the proxied path.
type lveManagerResponseWriter struct {
	http.ResponseWriter
	wrote bool
}

func (w *lveManagerResponseWriter) WriteHeader(code int) {
	if w.wrote {
		return
	}
	w.wrote = true
	if loc := w.Header().Get("Location"); loc != "" {
		if strings.HasPrefix(loc, "/") && !strings.HasPrefix(loc, cloudlinux.ManagerPrefix) {
			w.Header().Set("Location", cloudlinux.ManagerPrefix+loc)
		}
	}
	// The Manager is shown inside a frame on the panel's own page, so it may
	// not forbid framing. Same origin either way: the browser only ever talks
	// to the panel.
	w.Header().Del("X-Frame-Options")
	w.ResponseWriter.WriteHeader(code)
}
