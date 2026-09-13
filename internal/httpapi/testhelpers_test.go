package httpapi_test

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
)

// discardLogger keeps test output readable; failures are asserted on, not read
// out of the log.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

func newJar() http.CookieJar {
	j, err := cookiejar.New(nil)
	if err != nil {
		panic(err)
	}
	return j
}
