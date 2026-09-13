package httpapi

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed web
var webFS embed.FS

// uiHandler serves the panel interface from inside the binary.
//
// This is an interim UI: enough to run a host from a browser, and enough to
// prove the embed-and-serve path that the React port in Phase 5 will use.
func uiHandler() http.Handler {
	sub, err := fs.Sub(webFS, "web")
	if err != nil {
		panic("httpapi: embedded web assets missing: " + err.Error())
	}
	files := http.FileServerFS(sub)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The panel is a single page: any path that is not a real asset gets
		// index.html, so a refresh on a deep link does not 404.
		if r.URL.Path != "/" && !strings.Contains(r.URL.Path, ".") {
			r = r.Clone(r.Context())
			r.URL.Path = "/"
		}
		// The page is regenerated with each release and carries no secrets,
		// but it must not be cached across an upgrade.
		w.Header().Set("Cache-Control", "no-cache")
		files.ServeHTTP(w, r)
	})
}
