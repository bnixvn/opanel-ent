"""The page served on the panel port when TLS could not be brought up.

The panel used to fall back to serving its whole self over plain HTTP whenever
the certificate failed to load, on the reasoning that a reachable panel beats an
encrypted one because the panel is the tool used to repair the box. The
reachability half of that is right; serving the login form over plaintext is
not. Cookie flags follow the request scheme (app/api/auth.py), so in that mode
the session and CSRF cookies went out without ``Secure`` and an admin session
travelled in the clear, with nothing but a log line to say so.

So the port still answers -- the operator can see what is wrong from a browser
-- but it answers with this and nothing else. No application, no database, no
authentication: this module deliberately imports none of them, so there is no
route to sign in through even by mistake.
"""

from __future__ import annotations

RECOVERY_COMMAND = (
    "sudo -u opanel-ent env HOME=/opt/opanel-ent sudo -n "
    "/usr/local/sbin/opanel-ent-helper panel-cert-sync"
)

_PAGE = f"""<!doctype html>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>OPanel Enterprise unavailable</title>
<style>
  :root {{ color-scheme: light dark; }}
  body {{
    margin: 0; min-height: 100vh; display: grid; place-items: center;
    background: #11151c; color: #e6e9ef; padding: 24px;
    font: 15px/1.6 ui-sans-serif, system-ui, -apple-system, "Segoe UI", sans-serif;
  }}
  main {{ max-width: 46rem; }}
  h1 {{ font-size: 1.35rem; margin: 0 0 .75rem; color: #ffb4a2; }}
  p {{ margin: 0 0 .9rem; color: #c3c9d4; }}
  code, pre {{
    font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace;
    font-size: 13px;
  }}
  pre {{
    background: #0b0e13; border: 1px solid #262d3a; border-radius: 6px;
    padding: 12px 14px; overflow-x: auto; color: #dfe4ec;
  }}
  ol {{ color: #c3c9d4; padding-left: 1.25rem; }}
  li {{ margin-bottom: .5rem; }}
</style>
<main>
  <h1>OPanel Enterprise is not serving right now</h1>
  <p>
    The panel could not load a usable TLS certificate, and generating a
    self-signed one to fall back on failed too. It will not serve itself over
    an unencrypted connection: signing in here would send your session cookie
    across the network in clear text.
  </p>
  <p>
    Something is wrong with the certificate store itself &mdash; it is usually
    missing, not writable by root, or <code>openssl</code> is unavailable.
  </p>
  <p>Websites on this server are unaffected &mdash; only the panel is down.</p>
  <p>To fix it, over SSH:</p>
  <ol>
    <li>
      Read why the certificate was rejected:
      <pre>journalctl -u opanel-ent-api -n 50 --no-pager</pre>
    </li>
    <li>
      Rebuild the certificate store, which also regenerates the self-signed
      fallback:
      <pre>{RECOVERY_COMMAND}</pre>
    </li>
    <li>
      Start the panel again:
      <pre>systemctl restart opanel-ent-api</pre>
    </li>
  </ol>
</main>
"""

_BODY = _PAGE.encode("utf-8")


async def app(scope, receive, send) -> None:
    """A complete ASGI app in its own right: every request gets the same 503."""
    if scope["type"] == "lifespan":
        while True:
            message = await receive()
            if message["type"] == "lifespan.startup":
                await send({"type": "lifespan.startup.complete"})
            elif message["type"] == "lifespan.shutdown":
                await send({"type": "lifespan.shutdown.complete"})
                return
        return

    if scope["type"] != "http":
        return

    await send({
        "type": "http.response.start",
        "status": 503,
        "headers": [
            (b"content-type", b"text/html; charset=utf-8"),
            (b"content-length", str(len(_BODY)).encode("ascii")),
            (b"cache-control", b"no-store"),
            # Nothing here is meant to be framed, indexed or sniffed.
            (b"x-content-type-options", b"nosniff"),
            (b"x-frame-options", b"DENY"),
            (b"x-robots-tag", b"noindex, nofollow"),
        ],
    })
    await send({"type": "http.response.body", "body": _BODY})
