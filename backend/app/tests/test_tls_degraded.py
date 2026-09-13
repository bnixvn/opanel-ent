"""A panel that cannot load TLS must not serve itself over plain HTTP.

Cookie flags follow the request scheme (app/api/auth.py:90-92), so the old
fallback set the session and CSRF cookies without Secure and carried an admin
login across the network in clear text. The port still has to answer -- the
panel is how the box gets repaired -- but only with the recovery page.
"""
import asyncio
from pathlib import Path

import pytest

from app.core import tls_degraded

SERVER_SOURCE = (Path(__file__).resolve().parents[1] / "server.py").read_text(encoding="utf-8")


def _request(path: str = "/", method: str = "GET") -> tuple[int, dict, bytes]:
    sent = []

    async def receive():
        return {"type": "http.request", "body": b"", "more_body": False}

    async def send(message):
        sent.append(message)

    scope = {"type": "http", "method": method, "path": path, "headers": []}
    asyncio.run(tls_degraded.app(scope, receive, send))

    start = next(m for m in sent if m["type"] == "http.response.start")
    body = b"".join(m.get("body", b"") for m in sent if m["type"] == "http.response.body")
    headers = {k.decode().lower(): v.decode() for k, v in start["headers"]}
    return start["status"], headers, body


def test_every_path_gets_the_same_503():
    for path in ("/", "/api/health", "/api/auth/login", "/sso/anything", "/assets/index.js"):
        status, _, _ = _request(path)
        assert status == 503, path


def test_post_to_the_login_path_is_not_special_cased():
    status, _, body = _request("/api/auth/login", method="POST")

    assert status == 503
    assert b"token" not in body.lower()
    assert b"password" not in body.lower()


def test_the_page_says_what_broke_and_how_to_fix_it():
    _, headers, body = _request()
    text = body.decode("utf-8")

    assert headers["content-type"].startswith("text/html")
    assert headers["cache-control"] == "no-store"
    assert headers["x-robots-tag"] == "noindex, nofollow"
    assert "panel-cert-sync" in text
    assert "journalctl -u opanel-ent-api" in text
    assert "systemctl restart opanel-ent-api" in text
    # Sites keep serving; say so, or the operator hunts a bigger outage.
    assert "Websites on this server are unaffected" in text


def test_it_pulls_in_no_application_code():
    # Importing the app would give this module a database session, the auth
    # routes and the session cookie machinery. It must have none of them.
    source = (Path(tls_degraded.__file__)).read_text(encoding="utf-8")

    for forbidden in ("app.main", "fastapi", "sqlalchemy", "app.api", "get_db"):
        assert forbidden not in source, forbidden


def test_lifespan_completes_so_uvicorn_can_start_it():
    messages = ["lifespan.startup", "lifespan.shutdown"]
    sent = []

    async def receive():
        return {"type": messages.pop(0)}

    async def send(message):
        sent.append(message["type"])

    asyncio.run(tls_degraded.app({"type": "lifespan"}, receive, send))

    assert sent == ["lifespan.startup.complete", "lifespan.shutdown.complete"]


def test_server_never_serves_the_panel_without_tls():
    assert 'app="app.core.tls_degraded:app"' in SERVER_SOURCE
    # The panel app is named exactly once, in the TLS-backed config.
    assert SERVER_SOURCE.count('app="app.main:app"') == 1


def test_an_app_import_error_is_not_disguised_as_a_cert_problem():
    """config.load() imports the ASGI app as well as loading the certificate.

    Wrapping it would file a broken import as a TLS failure and hide the real
    bug behind the recovery page, so the panel's load() must sit outside any
    try/except.
    """
    body = SERVER_SOURCE[SERVER_SOURCE.index("def main()"):]

    assert "try:" not in body
    assert "config.load()" in body


def test_sni_callback_is_only_attached_when_tls_is_live():
    body = SERVER_SOURCE[SERVER_SOURCE.index("def main()"):]

    assert body.index("    else:") < body.index("config.ssl.sni_callback")


@pytest.mark.parametrize("scope_type", ["websocket", "something-else"])
def test_unknown_scope_types_are_dropped_quietly(scope_type):
    async def receive():
        raise AssertionError("must not read from an unsupported scope")

    async def send(message):
        raise AssertionError("must not reply on an unsupported scope")

    asyncio.run(tls_degraded.app({"type": scope_type}, receive, send))


# --------------------------------------------------------------------------
# usable_cert_pair(): the single decision that picks degraded mode
# --------------------------------------------------------------------------

def _write_self_signed(directory: Path) -> None:
    from datetime import datetime, timedelta, timezone

    from cryptography import x509
    from cryptography.hazmat.primitives import hashes, serialization
    from cryptography.hazmat.primitives.asymmetric import rsa
    from cryptography.x509.oid import NameOID

    key = rsa.generate_private_key(public_exponent=65537, key_size=2048)
    name = x509.Name([x509.NameAttribute(NameOID.COMMON_NAME, "panel.test")])
    now = datetime.now(timezone.utc)
    cert = (
        x509.CertificateBuilder()
        .subject_name(name)
        .issuer_name(name)
        .public_key(key.public_key())
        .serial_number(x509.random_serial_number())
        .not_valid_before(now - timedelta(days=1))
        .not_valid_after(now + timedelta(days=30))
        .sign(key, hashes.SHA256())
    )
    directory.mkdir(parents=True, exist_ok=True)
    (directory / "fullchain.pem").write_bytes(cert.public_bytes(serialization.Encoding.PEM))
    (directory / "privkey.pem").write_bytes(
        key.private_bytes(
            serialization.Encoding.PEM,
            serialization.PrivateFormat.TraditionalOpenSSL,
            serialization.NoEncryption(),
        )
    )


@pytest.fixture
def cert_store(tmp_path, monkeypatch):
    from app.core import tls

    monkeypatch.setattr(tls, "CERT_STORE", tmp_path)
    monkeypatch.delenv("PANEL_SSL_CERT", raising=False)
    monkeypatch.delenv("PANEL_SSL_KEY", raising=False)
    return tmp_path


def test_a_good_certificate_keeps_the_panel_on_https(cert_store):
    from app import server

    _write_self_signed(cert_store / "_default")

    pair = server.usable_cert_pair()

    assert pair is not None
    assert pair[0] == cert_store / "_default" / "fullchain.pem"


def test_an_empty_store_regenerates_a_self_signed_certificate(cert_store, monkeypatch):
    from app import server

    def fake_repair():
        _write_self_signed(cert_store / "_default")
        return True

    monkeypatch.setattr(server, "regenerate_self_signed", fake_repair)

    pair = server.usable_cert_pair()

    assert pair == (cert_store / "_default" / "fullchain.pem",
                    cert_store / "_default" / "privkey.pem")


def test_a_corrupt_default_is_replaced_rather_than_giving_up(cert_store, monkeypatch):
    from app import server

    default = cert_store / "_default"
    default.mkdir(parents=True)
    (default / "fullchain.pem").write_text("not a certificate", encoding="utf-8")
    (default / "privkey.pem").write_text("not a key", encoding="utf-8")

    def fake_repair():
        _write_self_signed(default)
        return True

    monkeypatch.setattr(server, "regenerate_self_signed", fake_repair)

    assert server.usable_cert_pair() is not None


def test_a_broken_configured_certificate_falls_back_to_the_self_signed_one(cert_store, monkeypatch):
    """The panel must not lose HTTPS over a file it was never required to use."""
    from app import server

    _write_self_signed(cert_store / "_default")
    broken = cert_store / "configured.pem"
    broken.write_text("not a certificate", encoding="utf-8")
    monkeypatch.setenv("PANEL_SSL_CERT", str(broken))
    monkeypatch.setenv("PANEL_SSL_KEY", str(broken))

    called = []
    monkeypatch.setattr(server, "regenerate_self_signed", lambda: called.append(1) or True)

    pair = server.usable_cert_pair()

    assert pair == (cert_store / "_default" / "fullchain.pem",
                    cert_store / "_default" / "privkey.pem")
    # The self-signed default was already good, so nothing had to be rebuilt.
    assert called == []


def test_a_configured_certificate_that_works_is_preferred(cert_store, monkeypatch):
    from app import server

    _write_self_signed(cert_store / "_default")
    configured = cert_store / "configured"
    _write_self_signed(configured)
    monkeypatch.setenv("PANEL_SSL_CERT", str(configured / "fullchain.pem"))
    monkeypatch.setenv("PANEL_SSL_KEY", str(configured / "privkey.pem"))

    assert server.usable_cert_pair() == (configured / "fullchain.pem", configured / "privkey.pem")


def test_only_a_failed_regeneration_reaches_the_recovery_page(cert_store, monkeypatch):
    from app import server

    monkeypatch.setattr(server, "regenerate_self_signed", lambda: False)

    assert server.usable_cert_pair() is None


def test_regeneration_that_reports_success_but_writes_nothing_still_degrades(cert_store, monkeypatch):
    from app import server

    monkeypatch.setattr(server, "regenerate_self_signed", lambda: True)

    assert server.usable_cert_pair() is None


def test_regenerate_calls_the_helper_through_sudo(monkeypatch):
    from app import server

    seen = {}

    def fake_run(argv, **kwargs):
        seen["argv"] = argv
        seen["timeout"] = kwargs.get("timeout")
        return type("R", (), {"returncode": 0, "stdout": "ok", "stderr": ""})()

    monkeypatch.setattr(server.subprocess, "run", fake_run)

    assert server.regenerate_self_signed() is True
    assert seen["argv"] == ("sudo", "-n", "/usr/local/sbin/opanel-ent-helper", "panel-cert-selfsigned")
    # sudo -n cannot prompt, and the timeout stops a wedged helper hanging boot.
    assert seen["timeout"] == 60


def test_regenerate_survives_a_missing_helper(monkeypatch):
    from app import server

    def boom(argv, **kwargs):
        raise FileNotFoundError("sudo")

    monkeypatch.setattr(server.subprocess, "run", boom)

    assert server.regenerate_self_signed() is False


def test_helper_has_a_cheap_self_signed_only_command():
    helper = (Path(__file__).resolve().parents[3]
              / "installer" / "files" / "opanel-ent-helper.sh").read_text(encoding="utf-8")
    start = helper.index("  panel-cert-selfsigned)")
    block = helper[start:helper.index("# ---- certbot", start)]

    assert "panel_self_signed_ensure" in block
    # A full store sync copies every Let's Encrypt cert on the box; not at boot.
    assert "panel_cert_store_sync" not in block
    # The stale pair must go first or checkend passes on the old file.
    assert 'rm -f "${PANEL_CERT_STORE}/_default/fullchain.pem"' in block
