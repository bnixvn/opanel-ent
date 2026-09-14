package httpapi

import (
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/bnixvn/opanel-ent/internal/agent/actions"
)

// pmaProxy serves phpMyAdmin through the panel.
//
// Reaching it any other way would mean a hostname, a certificate and an open
// port for a tool that only signed-in panel users should see. Proxying keeps
// it on the panel's own origin, behind the panel's own session check, and
// off the public internet entirely: the vhost listens on the loopback
// address, so nothing outside this machine can connect to it at all.
func (s *Server) pmaProxy() http.Handler {
	target := &url.URL{
		Scheme: "http",
		Host:   "127.0.0.1:" + strconv.Itoa(actions.PMAPort),
	}
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			// phpMyAdmin builds its own links from the request path, so it
			// has to see the path it is actually served at.
			pr.Out.URL.Path = strings.TrimPrefix(pr.In.URL.Path, "/phpmyadmin")
			if pr.Out.URL.Path == "" {
				pr.Out.URL.Path = "/"
			}
			// The panel's own hostname, not the loopback one it is being
			// dialled at: phpMyAdmin puts the host in page titles and builds
			// absolute links from it, and the vhost accepts any host anyway.
			pr.Out.Host = pr.In.Host
			// The panel terminates TLS, so tell PHP the request was secure
			// or phpMyAdmin will build http:// links and mark its cookies
			// insecure.
			pr.Out.Header.Set("X-Forwarded-Proto", "https")
			pr.Out.Header.Set("X-Forwarded-Prefix", "/phpmyadmin")
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			s.log.Warn("httpapi: phpMyAdmin is not reachable", "err", err)
			writeError(w, http.StatusBadGateway, "phpmyadmin_down",
				"phpMyAdmin is not running. Install it from the Databases page.")
		},
		FlushInterval: 250 * time.Millisecond,
	}

	// A redirect from PHP comes back as a path inside the vhost, which would
	// send the browser outside the proxy. pmaResponseWriter puts it back.
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxy.ServeHTTP(&pmaResponseWriter{ResponseWriter: w}, r)
	})
}

// pmaResponseWriter rewrites redirects so they stay under /phpmyadmin.
type pmaResponseWriter struct {
	http.ResponseWriter
}

func (w *pmaResponseWriter) WriteHeader(code int) {
	if loc := w.Header().Get("Location"); loc != "" {
		if strings.HasPrefix(loc, "/") && !strings.HasPrefix(loc, "/phpmyadmin") {
			w.Header().Set("Location", "/phpmyadmin"+loc)
		} else if !strings.Contains(loc, "://") && !strings.HasPrefix(loc, "/") {
			// A relative redirect such as "index.php" resolves against the
			// current directory, which is already inside the proxy.
			w.Header().Set("Location", loc)
		}
	}
	w.ResponseWriter.WriteHeader(code)
}
