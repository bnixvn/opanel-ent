"""Panel entrypoint.

Runs the API over HTTPS, picking a certificate per requested hostname so any
site that already has SSL can also reach the panel on the panel port. Falls
back, in order: the SNI match, the configured certificate, the self-signed
default, and a self-signed default regenerated on the spot. A certificate
browsers merely warn about is worth far more than an unreachable panel.

Only when even a fresh self-signed certificate cannot be produced does the port
stop serving the panel. It still answers -- the panel is the tool used to repair
the box, so going dark would be its own outage -- but with the recovery page in
app/core/tls_degraded.py and nothing else. It never serves the panel over plain
HTTP: cookie flags follow the request scheme, so that mode put admin session
cookies on the wire unencrypted.
"""

from __future__ import annotations

import logging
import os
import socket
import subprocess
from pathlib import Path

import uvicorn

from app.core import tls

logger = logging.getLogger("opanel-ent.server")


def _bindable_host(host: str) -> str:
    """Fall back to IPv4 when the requested IPv6 bind cannot work.

    Turning IPv6 on writes PANEL_BIND_HOST=:: . If the address family is later
    taken away -- ipv6.disable=1 on the kernel command line, a rebuilt VPS --
    binding it would abort start-up, and the panel is the tool used to fix that
    kind of mistake, so it has to come up anyway.
    """
    if host not in {"::", "[::]"}:
        return host
    if not socket.has_ipv6:
        logger.warning("IPv6 is unavailable on this host; binding 0.0.0.0 instead")
        return "0.0.0.0"
    try:
        with socket.socket(socket.AF_INET6, socket.SOCK_STREAM) as probe:
            probe.bind(("::", 0))
    except OSError as exc:
        logger.warning("Cannot bind IPv6 (%s); binding 0.0.0.0 instead", exc)
        return "0.0.0.0"
    return "::"


def listen_socket(host: str, port: int) -> socket.socket:
    """Open the panel's listening socket, dual-stack whenever IPv6 is asked for.

    asyncio sets IPV6_V6ONLY on every AF_INET6 server socket it creates, so
    letting uvicorn bind "::" would answer IPv6 clients and refuse every IPv4
    one -- including the admin trying to undo the change. Binding it here with
    V6ONLY cleared serves both families from one socket, and a host that cannot
    do IPv6 at all still gets an IPv4 panel.
    """
    if host in {"::", "[::]"}:
        sock = None
        try:
            sock = socket.socket(socket.AF_INET6, socket.SOCK_STREAM)
            sock.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
            sock.setsockopt(socket.IPPROTO_IPV6, socket.IPV6_V6ONLY, 0)
            sock.bind(("::", port))
            sock.listen(2048)
            sock.set_inheritable(True)
            return sock
        except OSError as exc:
            logger.warning("Cannot listen on IPv6 (%s); falling back to 0.0.0.0", exc)
            if sock is not None:
                sock.close()
        host = "0.0.0.0"

    sock = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    sock.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    sock.bind((host, port))
    sock.listen(2048)
    sock.set_inheritable(True)
    return sock


SELF_SIGNED_REPAIR = (
    "sudo", "-n", "/usr/local/sbin/opanel-ent-helper", "panel-cert-selfsigned",
)


def regenerate_self_signed() -> bool:
    """Ask the helper for a fresh self-signed default certificate.

    This module deliberately stays clear of the application's service layer --
    it has to run when the app itself will not import -- so it calls the sudo
    trampoline directly instead of going through app.services.shell.
    """
    try:
        result = subprocess.run(
            SELF_SIGNED_REPAIR, capture_output=True, text=True, timeout=60,
        )
    except (OSError, subprocess.SubprocessError):
        logger.exception("Could not run the certificate repair helper")
        return False
    if result.returncode != 0:
        detail = (result.stderr or result.stdout or "").strip()
        logger.error("Self-signed certificate repair failed: %s", detail[:300])
        return False
    return True


def usable_cert_pair() -> tuple[Path, Path] | None:
    """The certificate to start with, or None if TLS cannot come up at all.

    uvicorn's Config.load() both builds the SSL context and imports the ASGI
    app, so letting it decide would file an application import error as a
    certificate problem and hide a real bug behind the recovery page. TLS is
    settled here, and an app that will not import still crashes loudly.

    A certificate browsers warn about beats no panel, so a broken one falls
    back to the self-signed default, and a self-signed default that is itself
    missing or corrupt gets regenerated. Only when even that fails is there
    nothing left to serve HTTPS with.
    """
    def _select() -> tuple[Path, Path] | None:
        return tls.default_cert_pair(
            os.environ.get("PANEL_SSL_CERT", ""),
            os.environ.get("PANEL_SSL_KEY", ""),
        )

    pair = _select()
    if pair is not None:
        return pair

    logger.error(
        "No usable certificate in %s; regenerating the self-signed default",
        tls.CERT_STORE,
    )
    if not regenerate_self_signed():
        return None

    pair = _select()
    if pair is None:
        return None
    logger.warning(
        "Panel is serving a freshly generated self-signed certificate from %s. "
        "Browsers will warn until a real certificate is issued.",
        pair[0],
    )
    return pair


def build_config(pair: tuple[Path, Path]) -> uvicorn.Config:
    return uvicorn.Config(
        app="app.main:app",
        host=_bindable_host(os.environ.get("PANEL_BIND_HOST", "0.0.0.0")),
        port=int(os.environ.get("PANEL_PORT", "2222")),
        # Only the loopback reverse proxy may set X-Forwarded-*; a direct hit on
        # the panel port cannot spoof the audit log IP or the rate-limit key.
        proxy_headers=True,
        forwarded_allow_ips="127.0.0.1",
        ssl_certfile=str(pair[0]),
        ssl_keyfile=str(pair[1]),
    )


def degraded_config() -> uvicorn.Config:
    """Plain HTTP, but serving only the "TLS is broken" page.

    The port has to keep answering or the operator loses the one screen that
    tells them what went wrong. It must not keep serving the panel: cookie flags
    follow the request scheme, so over HTTP the session and CSRF cookies go out
    without Secure and an admin login crosses the network in clear text.
    """
    config = uvicorn.Config(
        app="app.core.tls_degraded:app",
        host=_bindable_host(os.environ.get("PANEL_BIND_HOST", "0.0.0.0")),
        port=int(os.environ.get("PANEL_PORT", "2222")),
        proxy_headers=True,
        forwarded_allow_ips="127.0.0.1",
    )
    config.load()
    return config


def main() -> None:
    logging.basicConfig(level=logging.INFO)
    pair = usable_cert_pair()

    if pair is None:
        config = degraded_config()
        logger.error(
            "Panel is serving the TLS recovery page only. Sign-in stays disabled "
            "until a certificate loads; run `opanel-ent-helper panel-cert-sync` and "
            "restart opanel-ent-api."
        )
    else:
        config = build_config(pair)
        config.load()
        config.ssl.sni_callback = tls.SniResolver()
        logger.info("Panel listening with HTTPS; per-domain certificates from %s", tls.CERT_STORE)

    sock = listen_socket(
        _bindable_host(os.environ.get("PANEL_BIND_HOST", "0.0.0.0")),
        int(os.environ.get("PANEL_PORT", "2222")),
    )
    uvicorn.Server(config).run(sockets=[sock])


if __name__ == "__main__":
    main()
