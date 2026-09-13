from datetime import datetime, timedelta, timezone
from pathlib import Path

import pytest
from cryptography import x509
from cryptography.hazmat.primitives import hashes, serialization
from cryptography.hazmat.primitives.asymmetric import rsa
from cryptography.x509.oid import NameOID

from app.services import nginx, ssl

PROJECT_ROOT = Path(__file__).resolve().parents[3]


def _cert_pair(domain="example.test", *, days=30, key=None, aliases=None):
    key = key or rsa.generate_private_key(public_exponent=65537, key_size=2048)
    subject = issuer = x509.Name([x509.NameAttribute(NameOID.COMMON_NAME, domain)])
    now = datetime.now(timezone.utc)
    san_names = [x509.DNSName(domain)]
    for alias in aliases or []:
        san_names.append(x509.DNSName(alias))
    cert = (
        x509.CertificateBuilder()
        .subject_name(subject)
        .issuer_name(issuer)
        .public_key(key.public_key())
        .serial_number(x509.random_serial_number())
        .not_valid_before(now - timedelta(days=2))
        .not_valid_after(now + timedelta(days=days))
        .add_extension(x509.SubjectAlternativeName(san_names), critical=False)
        .sign(key, hashes.SHA256())
    )
    cert_pem = cert.public_bytes(serialization.Encoding.PEM)
    key_pem = key.private_bytes(
        serialization.Encoding.PEM,
        serialization.PrivateFormat.PKCS8,
        serialization.NoEncryption(),
    )
    return cert_pem, key_pem


def test_validate_manual_ssl_accepts_matching_cert_key_and_optional_ca():
    cert_pem, key_pem = _cert_pair()

    ssl.validate_manual_ssl("example.test", cert_pem, key_pem, cert_pem)


def test_validate_manual_ssl_accepts_alias_san():
    cert_pem, key_pem = _cert_pair("example.test", aliases=["www.example.test"])

    ssl.validate_manual_ssl("example.test", cert_pem, key_pem, aliases=["www.example.test"])


def test_validate_manual_ssl_rejects_mismatched_private_key():
    cert_pem, _key_pem = _cert_pair()
    _other_cert, other_key = _cert_pair("other.test")

    with pytest.raises(ValueError, match="private_key does not match"):
        ssl.validate_manual_ssl("example.test", cert_pem, other_key)


def test_validate_manual_ssl_rejects_wrong_domain():
    cert_pem, key_pem = _cert_pair("other.test")

    with pytest.raises(ValueError, match="CN/SAN"):
        ssl.validate_manual_ssl("example.test", cert_pem, key_pem)


def test_validate_manual_ssl_rejects_missing_alias_domain():
    cert_pem, key_pem = _cert_pair()

    with pytest.raises(ValueError, match="CN/SAN"):
        ssl.validate_manual_ssl("example.test", cert_pem, key_pem, aliases=["alias.example.test"])


def test_validate_manual_ssl_rejects_expired_certificate():
    cert_pem, key_pem = _cert_pair(days=-1)

    with pytest.raises(ValueError, match="expired"):
        ssl.validate_manual_ssl("example.test", cert_pem, key_pem)


def test_apply_manual_ssl_config_adds_https_server_and_ca():
    rendered = nginx.apply_manual_ssl_config(
        nginx.render_vhost(
        "example.test",
        "/home/bp_example_test/example.test",
        app_type="wordpress",
        php_version="8.3",
        ),
        "/etc/nginx/opanel-ent/ssl/sites/example.test/cert.crt",
        "/etc/nginx/opanel-ent/ssl/sites/example.test/privkey.key",
        "/etc/nginx/opanel-ent/ssl/sites/example.test/ca.crt",
    )

    assert "return 301 https://$host$request_uri;" in rendered
    assert "listen 443 ssl http2;" in rendered
    assert "ssl_certificate /etc/nginx/opanel-ent/ssl/sites/example.test/fullchain.crt;" in rendered
    assert "ssl_certificate_key /etc/nginx/opanel-ent/ssl/sites/example.test/privkey.key;" in rendered
    assert "ssl_trusted_certificate /etc/nginx/opanel-ent/ssl/sites/example.test/ca.crt;" in rendered


def test_install_manual_ssl_uses_helper_without_logging_key(monkeypatch):
    cert_pem, key_pem = _cert_pair()
    captured = {}

    def fake_privileged(helper_command, helper_args=None, **kwargs):
        captured["helper_command"] = helper_command
        captured["helper_args"] = helper_args
        captured["kwargs"] = kwargs
        return type("Result", (), {"returncode": 0, "stdout": "", "stderr": ""})()

    monkeypatch.setattr(ssl.shell, "privileged", fake_privileged)

    paths = ssl.install_manual_ssl("example.test", cert_pem, key_pem)

    assert paths == {
        "cert": "/usr/local/lsws/conf/opanel-ent/ssl/sites/example.test/cert.crt",
        "key": "/usr/local/lsws/conf/opanel-ent/ssl/sites/example.test/privkey.key",
        "ca": None,
    }
    assert captured["helper_command"] == "manual-ssl-install"
    assert captured["helper_args"] == ["example.test"]
    assert captured["kwargs"]["sensitive"] is True
    assert "PRIVATE KEY" in captured["kwargs"]["input"]


def test_issue_ssl_passes_aliases_and_email(monkeypatch):
    captured = {}

    def fake_privileged(helper_command, helper_args=None, **kwargs):
        captured["helper_command"] = helper_command
        captured["helper_args"] = helper_args
        captured["kwargs"] = kwargs
        return type("Result", (), {"returncode": 0, "stdout": "", "stderr": ""})()

    monkeypatch.setattr(ssl.shell, "privileged", fake_privileged)
    monkeypatch.setattr(ssl.settings, "ssl_email", "admin@example.test")

    result = ssl.issue_ssl("example.test", aliases=["www.example.test"])

    assert result.returncode == 0
    assert captured["helper_command"] == "certbot-issue"
    assert captured["helper_args"] == ["example.test", "www.example.test", "admin@example.test"]


def test_issue_wildcard_ssl_passes_token_on_stdin(monkeypatch):
    captured = {}

    def fake_privileged(helper_command, helper_args=None, **kwargs):
        captured["helper_command"] = helper_command
        captured["helper_args"] = helper_args
        captured["kwargs"] = kwargs
        return type("Result", (), {"returncode": 0, "stdout": "", "stderr": ""})()

    monkeypatch.setattr(ssl.shell, "privileged", fake_privileged)

    result = ssl.issue_wildcard_ssl("example.test", "cf-secret-token-abc123", email="admin@example.test")

    assert result.returncode == 0
    assert captured["helper_command"] == "certbot-dns-cloudflare"
    assert captured["helper_args"] == ["example.test", "admin@example.test"]
    assert captured["kwargs"]["input"] == "cf-secret-token-abc123"
    assert captured["kwargs"]["sensitive"] is True
    fallback = " ".join(captured["kwargs"]["fallback"])
    assert "--dns-cloudflare" in fallback and "-d '*.example.test'" in fallback


def test_wildcard_ssl_helper_writes_root_only_credentials():
    helper = (PROJECT_ROOT / "installer" / "files" / "opanel-ent-helper.sh").read_text(encoding="utf-8")
    assert "certbot-dns-cloudflare)" in helper
    assert "certbot-dns-cloudflare-remove)" in helper
    assert "ensure_certbot_dns_cloudflare()" in helper
    assert 'install -d -o root -g root -m 0700 "$ACME_DNS_DIR"' in helper
    assert 'install -o root -g root -m 0600 "$tmp" "$creds"' in helper
    assert 'python3-certbot-dns-cloudflare' in (PROJECT_ROOT / "installer" / "install.sh").read_text(encoding="utf-8")


def _write_cert(dir_path, name, filename, domains):
    from cryptography.hazmat.primitives.asymmetric import rsa as _rsa

    key = _rsa.generate_private_key(public_exponent=65537, key_size=2048)
    cert_pem, _ = _cert_pair(domains[0], key=key, aliases=domains[1:])
    d = dir_path / name
    d.mkdir(parents=True, exist_ok=True)
    (d / filename).write_bytes(cert_pem)


def test_list_available_certificates_and_reuse(monkeypatch, tmp_path):
    le = tmp_path / "certs"
    manual = tmp_path / "manual"
    _write_cert(le, "example.test", "fullchain.pem", ["example.test", "*.example.test"])
    _write_cert(le, "_default", "fullchain.pem", ["opanel-ent.local"])
    _write_cert(manual, "foo.test", "fullchain.crt", ["foo.test"])
    monkeypatch.setattr(ssl, "PANEL_CERT_STORE", le)
    monkeypatch.setattr(ssl, "MANUAL_SSL_ROOT", manual)

    certs = ssl.list_available_certificates("blog.example.test")
    names = {c["name"] for c in certs}
    assert names == {"letsencrypt:example.test", "manual:foo.test"}  # _default skipped
    wildcard = next(c for c in certs if c["name"] == "letsencrypt:example.test")
    assert wildcard["is_wildcard"] is True
    assert wildcard["covers_domain"] is True  # *.example.test covers blog.example.test
    assert next(c for c in certs if c["name"] == "manual:foo.test")["covers_domain"] is False

    assert ssl.reuse_cert_paths("letsencrypt:example.test") == {
        "cert": "/etc/letsencrypt/live/example.test/fullchain.pem",
        "key": "/etc/letsencrypt/live/example.test/privkey.pem",
        "ca": None,
    }
    ok, _ = ssl.reuse_cert_covers("letsencrypt:example.test", "blog.example.test")
    assert ok is True
    bad, reason = ssl.reuse_cert_covers("letsencrypt:example.test", "other.test")
    assert bad is False and "does not cover" in reason


def test_issue_ssl_uses_opanel_acme_webroot(monkeypatch):
    captured = {}

    def fake_privileged(helper_command, helper_args=None, **kwargs):
        captured["helper_command"] = helper_command
        captured["helper_args"] = helper_args
        captured["kwargs"] = kwargs
        return type("Result", (), {"returncode": 0, "stdout": "", "stderr": ""})()

    monkeypatch.setattr(ssl.shell, "privileged", fake_privileged)
    monkeypatch.setattr(ssl.settings, "ssl_email", "")

    result = ssl.issue_ssl("example.test")

    assert result.returncode == 0
    fallback = " ".join(captured["kwargs"]["fallback"])
    assert "/var/www/opanel-ent-acme" in fallback
    assert "/var/www/opanel-ent/acme" not in fallback


def test_panel_ssl_helper_uses_webroot_flow():
    helper = (PROJECT_ROOT / "installer" / "files" / "opanel-ent-helper.sh").read_text(encoding="utf-8")
    assert "panel-ssl-install)" in helper
    assert "certbot_args=(certonly --webroot -w /var/www/opanel-ent-acme" in helper
    assert "certbot_args=(certonly --standalone" not in helper
    assert "--pre-hook \"/usr/local/lsws/bin/lswsctrl stop || true\"" not in helper
    assert "--post-hook \"/usr/local/lsws/bin/lswsctrl start || true\"" not in helper


def test_panel_ssl_webroot_is_served_by_tools_vhost():
    helper = (PROJECT_ROOT / "installer" / "files" / "opanel-ent-helper.sh").read_text(encoding="utf-8")
    installer = (PROJECT_ROOT / "installer" / "install.sh").read_text(encoding="utf-8")
    assert "context /.well-known/acme-challenge/ {" in helper
    assert "location                /var/www/opanel-ent-acme/.well-known/acme-challenge/" in helper
    assert "virtualHost opanel_ent_tools {" in helper
    assert 'lines.append(f"    map                      opanel_ent_tools {host}")' in helper
    assert "map                      opanel_ent_tools {', '.join(tools_hosts)}" not in helper
    assert helper.index('lines.append(f"    map                      opanel_ent_tools {host}")') < helper.index('lines.append(f"    map                      {domain}')
    assert '"$OLS_HTTPD_CONF" "$OLS_VHOSTS_DIR" "$ENV_FILE"' in helper
    assert "00-opanel-ent-tools.conf" in installer
    assert "context /.well-known/acme-challenge/ {" in installer


def test_website_ssl_does_not_clobber_the_panel_certificate():
    """certbot-issue <site-domain> (a website's "Install SSL") used to copy that
    site's cert over /etc/opanel-ent/panel-*.pem, which broke phpMyAdmin SSO with an
    SNI cert mismatch. copy_panel_live_certificate must only touch the panel
    cert for the panel's own hostname."""
    helper = (PROJECT_ROOT / "installer" / "files" / "opanel-ent-helper.sh").read_text(encoding="utf-8")
    fn = helper.split("copy_panel_live_certificate() {", 1)[1].split("\n}", 1)[0]
    assert 'panel_host="$(env_get PANEL_DOMAIN)"' in fn
    assert '[[ -n "$panel_host" && "$domain" == "$panel_host" ]] || return 0' in fn
    # certbot-issue still calls it (it is a no-op for non-panel domains now)
    assert "certbot-issue)" in helper


def test_panel_ssl_tools_vhost_includes_cert_block():
    helper = (PROJECT_ROOT / "installer" / "files" / "opanel-ent-helper.sh").read_text(encoding="utf-8")
    assert "vhssl  {" in helper
    assert 'panel_cert="$(env_get PANEL_SSL_CERT)"' in helper
    assert 'panel_key="$(env_get PANEL_SSL_KEY)"' in helper
    assert "keyFile                 ${panel_key:-/dev/null}" in helper
    assert "certFile                ${panel_cert:-/dev/null}" in helper


def test_installer_panel_ssl_uses_helper_webroot_flow():
    installer = (PROJECT_ROOT / "installer" / "install.sh").read_text(encoding="utf-8")
    assert "opanel-ent-helper panel-ssl-install" in installer
    assert "certbot certonly --standalone" not in installer
    assert "--pre-hook \"/usr/local/lsws/bin/lswsctrl stop || true\"" not in installer
    assert "--post-hook \"/usr/local/lsws/bin/lswsctrl start || true\"" not in installer


def test_panel_update_defaults_to_main_branch():
    update_script = (PROJECT_ROOT / "installer" / "update.sh").read_text(encoding="utf-8")
    helper = (PROJECT_ROOT / "installer" / "files" / "opanel-ent-helper.sh").read_text(encoding="utf-8")
    assert 'UPDATE_CHANNEL="${UPDATE_CHANNEL-branch}"' in update_script
    assert "Update to the newest matching release tag (default)." not in update_script
    assert "Environment=UPDATE_CHANNEL=${UPDATE_CHANNEL:-branch}" in helper
    assert 'UPDATE_CHANNEL="${UPDATE_CHANNEL:-branch}"' in helper


def test_opanelctl_panel_ssl_delegates_to_helper_webroot_flow():
    ctl_script = (PROJECT_ROOT / "installer" / "files" / "opanel-ent-ctl").read_text(encoding="utf-8-sig")
    assert "run_helper panel-ssl-install" in ctl_script
    assert "certbot certonly --standalone" not in ctl_script
    assert "--pre-hook \"/usr/local/lsws/bin/lswsctrl stop || true\"" not in ctl_script
    assert "--post-hook \"/usr/local/lsws/bin/lswsctrl start || true\"" not in ctl_script
