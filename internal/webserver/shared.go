package webserver

import (
	"fmt"
	"os"
	"path/filepath"
)

// EnsureSharedRoots creates the two directories every backend serves from
// regardless of which one is running: the ACME challenge webroot and the
// page a suspended site shows.
//
// Called on every apply rather than at install time, so a directory somebody
// deleted comes back on the next configuration change instead of at the next
// reinstall. It also means switching webserver does not have to remember to
// recreate them -- whichever backend applies first does it.
func EnsureSharedRoots() error {
	if err := ensureSuspendedPage(); err != nil {
		return err
	}
	return ensureACMEWebroot()
}

func ensureSuspendedPage() error {
	if err := os.MkdirAll(SuspendedRoot, 0o755); err != nil {
		return fmt.Errorf("webserver: create suspended page directory: %w", err)
	}
	if err := os.Chmod(SuspendedRoot, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(SuspendedRoot, "index.html"), suspendedPage, 0o644)
}

// ensureACMEWebroot creates the shared challenge directory.
//
// It is written by the panel and read by the webserver worker, which runs as
// a different account, so it is world-readable. The only thing that ever
// lands here is a challenge token, which is public by design.
func ensureACMEWebroot() error {
	dir := filepath.Join(ACMEWebroot, ".well-known", "acme-challenge")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("webserver: create ACME webroot: %w", err)
	}
	for _, p := range []string{ACMEWebroot, filepath.Join(ACMEWebroot, ".well-known"), dir} {
		if err := os.Chmod(p, 0o755); err != nil {
			return err
		}
	}
	return nil
}

var suspendedPage = []byte(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>Account suspended</title>
<style>
  body{font:16px/1.6 system-ui,-apple-system,Segoe UI,Roboto,sans-serif;
       margin:0;min-height:100vh;display:grid;place-items:center;
       background:#fafafa;color:#1a1a1a}
  main{max-width:32rem;padding:2rem;text-align:center}
  h1{font-size:1.35rem;margin:0 0 .5rem;font-weight:600}
  p{margin:0;color:#666}
</style>
</head>
<body>
<main>
  <h1>This site is temporarily unavailable</h1>
  <p>The account has been suspended. Please contact the hosting provider.</p>
</main>
</body>
</html>
`)
