package httpapi

import (
	"crypto/tls"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/bnixvn/opanel-ent/internal/cloudlinux"
)

// lveManagerProxy serves CloudLinux Manager through the panel.
//
// The Manager is CloudLinux's own interface -- current usage, users,
// statistics, options, packages, the selectors -- and reimplementing it would
// be a worse version of something that already exists. So the panel serves
// it rather than copying it.
//
// Through the panel rather than on its own port, for the reason the firewall
// gives when asked to open one: the Manager authenticates with system
// accounts, and a web login for root on the public internet is a different
// proposition from a panel page. Here it is reachable only by somebody the
// panel has already signed in as an administrator.
func (s *Server) lveManagerProxy() http.Handler {
	target := &url.URL{
		Scheme: "https",
		Host:   "127.0.0.1:" + strconv.Itoa(cloudlinux.ManagerPort),
	}
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.URL.Path = strings.TrimPrefix(pr.In.URL.Path, cloudlinux.ManagerPrefix)
			if pr.Out.URL.Path == "" {
				pr.Out.URL.Path = "/"
			}
			pr.Out.Host = pr.In.Host
			pr.Out.Header.Set("X-Forwarded-Proto", "https")
			pr.Out.Header.Set("X-Forwarded-Prefix", cloudlinux.ManagerPrefix)
		},
		// The Manager serves itself over TLS with the panel's own
		// certificate, and this connection never leaves the machine: it is
		// dialled at 127.0.0.1, where the name on the certificate cannot
		// match and does not need to.
		Transport: loopbackTLS(),
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

// loopbackTLS dials the Manager over TLS without checking the name.
//
// Not a weakened check: the connection is to 127.0.0.1 and never leaves the
// machine, and no certificate can carry a name that matches a loopback dial
// of a service that also answers on the panel's hostname. Anything able to
// intercept this is already root here.
func loopbackTLS() *http.Transport {
	return &http.Transport{
		TLSClientConfig:     &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // loopback only
		TLSHandshakeTimeout: 10 * time.Second,
	}
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
