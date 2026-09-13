"""TLS for the panel listener, including per-domain SNI.

The panel serves HTTPS on its own port rather than behind OpenLiteSpeed, so it
has to pick the certificate itself. Certificates live in a store the panel user
can read:

    /etc/opanel-ent/certs/_default/{fullchain,privkey}.pem   <- self-signed fallback
    /etc/opanel-ent/certs/<domain>/{fullchain,privkey}.pem   <- one per SSL-enabled site

The helper installs them there as ``root:opanel-ent 0640`` on issue and on renewal,
which is the same arrangement ``panel-ssl-install`` already used for the single
panel certificate. The panel never reads /etc/letsencrypt directly -- it cannot,
and should not have to.

Nothing in here may raise into the TLS handshake or the server bootstrap: a bad
certificate must degrade to the default one, and a broken store must degrade to
plain HTTP, because losing the panel means losing the way to fix the box.
"""

from __future__ import annotations

import logging
import os
import re
import ssl
import threading
from pathlib import Path

logger = logging.getLogger(__name__)

CERT_STORE = Path(os.environ.get("OPANEL_ENT_CERT_STORE", "/etc/opanel-ent/certs"))
DEFAULT_CERT_NAME = "_default"
CERT_FILENAME = "fullchain.pem"
KEY_FILENAME = "privkey.pem"

# A SNI server_name arrives straight off the wire, so it selects a directory
# only after matching this. No dots-only names, no separators, no traversal.
_DOMAIN_RE = re.compile(r"^(?!-)[a-z0-9-]{1,63}(?<!-)(\.(?!-)[a-z0-9-]{1,63}(?<!-))+$")


def normalize_sni_name(server_name: str | None) -> str | None:
    """Return a safe directory name for an SNI hostname, or None to reject it."""
    if not server_name:
        return None
    name = server_name.strip().lower().rstrip(".")
    if len(name) > 253 or not _DOMAIN_RE.match(name):
        return None
    return name


def cert_pair(name: str) -> tuple[Path, Path] | None:
    """Return (cert, key) for a store entry when both files are readable."""
    directory = CERT_STORE / name
    cert = directory / CERT_FILENAME
    key = directory / KEY_FILENAME
    try:
        if cert.is_file() and key.is_file() and os.access(cert, os.R_OK) and os.access(key, os.R_OK):
            return cert, key
    except OSError:
        return None
    return None


def _fingerprint(cert: Path, key: Path) -> tuple:
    """Cheap change detector so a renewed certificate is picked up without a restart."""
    try:
        return (cert.stat().st_mtime_ns, cert.stat().st_size, key.stat().st_mtime_ns)
    except OSError:
        return ()


def build_context(cert: Path, key: Path) -> ssl.SSLContext | None:
    try:
        context = ssl.create_default_context(ssl.Purpose.CLIENT_AUTH)
        context.minimum_version = ssl.TLSVersion.TLSv1_2
        context.load_cert_chain(certfile=str(cert), keyfile=str(key))
        return context
    except (ssl.SSLError, OSError, ValueError):
        logger.warning("Ignoring unusable certificate pair: %s", cert, exc_info=True)
        return None


class SniResolver:
    """Chooses a certificate per requested hostname, caching by file mtime."""

    def __init__(self) -> None:
        self._lock = threading.Lock()
        self._cache: dict[str, tuple[tuple, ssl.SSLContext]] = {}

    def context_for(self, server_name: str | None) -> ssl.SSLContext | None:
        name = normalize_sni_name(server_name)
        if name is None:
            return None
        pair = cert_pair(name)
        if pair is None:
            return None
        stamp = _fingerprint(*pair)
        with self._lock:
            cached = self._cache.get(name)
            if cached and cached[0] == stamp:
                return cached[1]
        context = build_context(*pair)
        if context is None:
            return None
        with self._lock:
            self._cache[name] = (stamp, context)
        return context

    def __call__(self, sslsocket, server_name, sslcontext) -> None:
        """ssl sni_callback. Returning None keeps the handshake on the default cert."""
        try:
            context = self.context_for(server_name)
            if context is not None:
                sslsocket.context = context
        except Exception:  # never break a handshake over cert selection
            logger.warning("SNI selection failed for %r", server_name, exc_info=True)
        return None


def default_cert_pair(explicit_cert: str = "", explicit_key: str = "") -> tuple[Path, Path] | None:
    """The certificate used when SNI matches nothing.

    An explicitly configured PANEL_SSL_CERT/KEY wins so existing installs keep
    the certificate their admin chose; otherwise the self-signed fallback.

    Every candidate has to prove it loads, not merely that it exists and is
    readable. A configured certificate that is present but corrupt -- a
    truncated renewal, a key that no longer matches its chain -- must give way
    to the self-signed default, exactly as a missing one does. Checking only
    readability sent the panel to the recovery page over a file it was never
    required to use, when a certificate browsers merely warn about was sitting
    right there.
    """
    if explicit_cert and explicit_key:
        pair = (Path(explicit_cert), Path(explicit_key))
        if build_context(*pair) is not None:
            return pair
        logger.warning("PANEL_SSL_CERT/KEY unusable, falling back to %s", DEFAULT_CERT_NAME)

    pair = cert_pair(DEFAULT_CERT_NAME)
    if pair is not None and build_context(*pair) is not None:
        return pair
    return None
