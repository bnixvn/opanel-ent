import datetime
import socket
import ssl
import threading

import pytest
from cryptography import x509
from cryptography.hazmat.primitives import hashes, serialization
from cryptography.hazmat.primitives.asymmetric import rsa
from cryptography.x509.oid import NameOID

from app.core import tls


def _write_cert(directory, common_name):
    directory.mkdir(parents=True, exist_ok=True)
    key = rsa.generate_private_key(public_exponent=65537, key_size=2048)
    name = x509.Name([x509.NameAttribute(NameOID.COMMON_NAME, common_name)])
    now = datetime.datetime.now(datetime.timezone.utc)
    cert = (
        x509.CertificateBuilder()
        .subject_name(name)
        .issuer_name(name)
        .public_key(key.public_key())
        .serial_number(x509.random_serial_number())
        .not_valid_before(now - datetime.timedelta(days=1))
        .not_valid_after(now + datetime.timedelta(days=30))
        .add_extension(x509.SubjectAlternativeName([x509.DNSName(common_name)]), critical=False)
        .sign(key, hashes.SHA256())
    )
    (directory / tls.CERT_FILENAME).write_bytes(cert.public_bytes(serialization.Encoding.PEM))
    (directory / tls.KEY_FILENAME).write_bytes(
        key.private_bytes(
            encoding=serialization.Encoding.PEM,
            format=serialization.PrivateFormat.TraditionalOpenSSL,
            encryption_algorithm=serialization.NoEncryption(),
        )
    )
    return directory


@pytest.fixture
def store(tmp_path, monkeypatch):
    monkeypatch.setattr(tls, "CERT_STORE", tmp_path)
    return tmp_path


@pytest.mark.parametrize(
    "hostile",
    ["../../etc/letsencrypt/live/x", "..", "a/b", "-bad.com", "", None, "x" * 300, "localhost"],
)
def test_sni_names_that_must_be_rejected(hostile):
    """server_name comes off the wire and selects a directory, so anything that
    is not plainly a hostname must not reach the filesystem."""
    assert tls.normalize_sni_name(hostile) is None


def test_sni_names_are_normalised():
    assert tls.normalize_sni_name("TapLooks.COM.") == "taplooks.com"


def test_resolver_returns_none_for_unknown_domain(store):
    assert tls.SniResolver().context_for("nothing-here.test") is None


def test_resolver_ignores_a_broken_pair(store):
    broken = store / "broken.test"
    broken.mkdir()
    (broken / tls.CERT_FILENAME).write_text("not a certificate")
    (broken / tls.KEY_FILENAME).write_text("not a key")
    assert tls.SniResolver().context_for("broken.test") is None


def test_resolver_caches_until_the_files_change(store):
    _write_cert(store / "a.test", "a.test")
    resolver = tls.SniResolver()
    first = resolver.context_for("a.test")
    assert first is not None
    assert resolver.context_for("a.test") is first          # cached
    _write_cert(store / "a.test", "a.test")                 # renewal
    assert resolver.context_for("a.test") is not first      # picked up


def test_default_prefers_explicit_cert_then_self_signed(store):
    _write_cert(store / tls.DEFAULT_CERT_NAME, "panel.local")
    explicit = _write_cert(store / "explicit", "explicit.test")
    chosen = tls.default_cert_pair(str(explicit / tls.CERT_FILENAME), str(explicit / tls.KEY_FILENAME))
    assert chosen[0].parent.name == "explicit"
    # An unreadable explicit pair must not take the panel offline.
    fallback = tls.default_cert_pair("/nonexistent/cert.pem", "/nonexistent/key.pem")
    assert fallback[0].parent.name == tls.DEFAULT_CERT_NAME


def test_no_certificate_anywhere_means_no_https(store):
    assert tls.default_cert_pair() is None


def _serve_once(context, ready):
    sock = socket.socket()
    sock.bind(("127.0.0.1", 0))
    sock.listen(1)
    ready.append(sock.getsockname()[1])
    wrapped = context.wrap_socket(sock, server_side=True)
    try:
        conn, _ = wrapped.accept()
        conn.close()
    except (ssl.SSLError, OSError):
        pass
    finally:
        wrapped.close()


def _peer_cn(port, server_hostname):
    client = ssl.SSLContext(ssl.PROTOCOL_TLS_CLIENT)
    client.check_hostname = False
    client.verify_mode = ssl.CERT_NONE
    with socket.create_connection(("127.0.0.1", port), timeout=5) as raw:
        with client.wrap_socket(raw, server_hostname=server_hostname) as tls_sock:
            der = tls_sock.getpeercert(binary_form=True)
    return x509.load_der_x509_certificate(der).subject.get_attributes_for_oid(NameOID.COMMON_NAME)[0].value


def test_real_handshake_serves_a_different_cert_per_domain(store):
    """The whole point of the feature: two domains, two certificates, one port."""
    _write_cert(store / tls.DEFAULT_CERT_NAME, "panel.fallback")
    _write_cert(store / "one.test", "one.test")
    _write_cert(store / "two.test", "two.test")

    default_pair = tls.default_cert_pair()
    context = tls.build_context(*default_pair)
    context.sni_callback = tls.SniResolver()

    for hostname, expected in [("one.test", "one.test"), ("two.test", "two.test"), ("unknown.test", "panel.fallback")]:
        ready: list[int] = []
        thread = threading.Thread(target=_serve_once, args=(context, ready), daemon=True)
        thread.start()
        while not ready:
            pass
        assert _peer_cn(ready[0], hostname) == expected
        thread.join(timeout=5)
