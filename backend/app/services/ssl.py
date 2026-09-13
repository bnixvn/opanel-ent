import json
import shlex
from datetime import datetime, timezone
from pathlib import Path
from typing import BinaryIO

from cryptography import x509
from cryptography.hazmat.primitives import hashes, serialization
from cryptography.hazmat.primitives.asymmetric import ec, ed25519, ed448, padding, rsa
from cryptography.x509.oid import ExtensionOID, NameOID

from app.core.config import settings
from app.services.shell import CommandResult, shell


MAX_SSL_PART_BYTES = 256 * 1024
ALLOWED_SSL_EXTENSIONS = {".crt", ".pem", ".key", ".ca"}
MANUAL_SSL_ROOT = Path("/usr/local/lsws/conf/opanel-ent/ssl/sites")
ACME_WEBROOT = "/var/www/opanel-ent-acme"


class ManualSslSnapshot:
    def __init__(self, cert_path: str, key_path: str, ca_path: str | None, domain: str | None = None):
        cert = Path(cert_path)
        self.domain = _safe_domain(domain or cert.parent.name)
        self.cert_path = cert
        self.key_path = Path(key_path)
        self.ca_path = Path(ca_path) if ca_path else None
        self.paths = [cert, Path(key_path), cert.with_name("fullchain.crt")]
        if ca_path:
            self.paths.append(Path(ca_path))
        self.contents: dict[Path, bytes | None] = {}

    def capture(self) -> None:
        for path in self.paths:
            try:
                self.contents[path] = path.read_bytes()
            except OSError:
                self.contents[path] = None

    def restore(self) -> None:
        cert = self.contents.get(self.cert_path)
        key = self.contents.get(self.key_path)
        ca = self.contents.get(self.ca_path) if self.ca_path else None
        if cert and key:
            _write_manual_ssl_files(self.domain, cert, key, ca or b"")
        else:
            remove_manual_ssl(self.domain)


def _safe_domain_list(domain: str, aliases: list[str] | tuple[str, ...] | None = None) -> list[str]:
    names = [_safe_domain(domain)]
    seen = set(names)
    for alias in aliases or []:
        safe_alias = _safe_domain(str(alias))
        if safe_alias in seen:
            continue
        names.append(safe_alias)
        seen.add(safe_alias)
    return names


def issue_ssl(domain: str, aliases: list[str] | tuple[str, ...] | None = None) -> CommandResult:
    domains = _safe_domain_list(domain, aliases)
    helper_args = domains[:]
    fallback_domains = " ".join(f"-d {shlex.quote(name)}" for name in domains)
    fallback = [
        "bash",
        "-lc",
        (
            "install -d -o root -g opanel-ent -m 0755 "
            f"{shlex.quote(ACME_WEBROOT)}/.well-known/acme-challenge && "
            f"certbot certonly --webroot -w {shlex.quote(ACME_WEBROOT)} "
            f"--cert-name {shlex.quote(domains[0])} --non-interactive --agree-tos --expand {fallback_domains}"
        ),
    ]
    if settings.ssl_email:
        helper_args.append(settings.ssl_email)
        fallback[-1] += f" --email {shlex.quote(settings.ssl_email)}"
    else:
        fallback[-1] += " --register-unsafely-without-email"
    return shell.privileged("certbot-issue", helper_args=helper_args, check=False, fallback=fallback)


def issue_wildcard_ssl(domain: str, cf_token: str, *, email: str | None = None) -> CommandResult:
    """Issue a Let's Encrypt cert covering ``domain`` and ``*.domain`` via the
    Cloudflare DNS-01 challenge.

    ``cf_token`` is a **scoped Cloudflare API Token** (Zone -> DNS -> Edit for the
    zone), NOT the account-wide Global API Key. The helper writes it as
    ``dns_cloudflare_api_token = ...`` in a root-only credentials file that
    certbot re-reads on every renewal; it is passed on stdin only (never argv /
    logs, ``sensitive=True``).
    """
    safe_domain = _safe_domain(domain)
    helper_args = [safe_domain]
    quoted_email = ""
    if email:
        helper_args.append(email)
        quoted_email = f" --email {shlex.quote(email)}"
    else:
        quoted_email = " --register-unsafely-without-email"
    fallback = [
        "bash",
        "-lc",
        (
            "install -d -o root -g root -m 0700 /etc/opanel-ent/acme-dns && "
            "umask 077 && "
            f"printf 'dns_cloudflare_api_token = %s\\n' \"$(cat)\" > /etc/opanel-ent/acme-dns/{shlex.quote(safe_domain)}.ini && "
            "certbot certonly --dns-cloudflare "
            f"--dns-cloudflare-credentials /etc/opanel-ent/acme-dns/{shlex.quote(safe_domain)}.ini "
            "--dns-cloudflare-propagation-seconds 30 "
            f"--cert-name {shlex.quote(safe_domain)} --non-interactive --agree-tos --expand "
            f"-d {shlex.quote(safe_domain)} -d {shlex.quote('*.' + safe_domain)}"
            f"{quoted_email}"
        ),
    ]
    return shell.privileged(
        "certbot-dns-cloudflare",
        helper_args=helper_args,
        check=False,
        input=cf_token,
        sensitive=True,
        fallback=fallback,
    )


def remove_wildcard_ssl(domain: str) -> CommandResult:
    """Delete a DNS-01 cert and its stored Cloudflare credentials file."""
    safe_domain = _safe_domain(domain)
    return shell.privileged(
        "certbot-dns-cloudflare-remove",
        helper_args=[safe_domain],
        check=False,
        fallback=[
            "bash",
            "-lc",
            (
                f"certbot delete --cert-name {shlex.quote(safe_domain)} --non-interactive || true; "
                f"rm -f /etc/opanel-ent/acme-dns/{shlex.quote(safe_domain)}.ini"
            ),
        ],
    )


def renew_all() -> CommandResult:
    return shell.privileged("certbot-renew", check=False, fallback=["certbot", "renew", "--quiet"])


# ---------------------------------------------------------------------------
# Reuse an existing certificate that is already on the box
# ---------------------------------------------------------------------------
PANEL_CERT_STORE = Path("/etc/opanel-ent/certs")  # root:opanel-ent mirror of every LE live cert


def _cert_names_and_expiry(cert_pem: bytes) -> tuple[list[str], datetime | None]:
    try:
        cert = x509.load_pem_x509_certificate(cert_pem)
    except ValueError:
        return [], None
    names: set[str] = set()
    try:
        san = cert.extensions.get_extension_for_oid(ExtensionOID.SUBJECT_ALTERNATIVE_NAME).value
        names.update(name.lower() for name in san.get_values_for_type(x509.DNSName))
    except x509.ExtensionNotFound:
        pass
    if not names:
        names.update(attr.value.lower() for attr in cert.subject.get_attributes_for_oid(NameOID.COMMON_NAME))
    try:
        not_after = cert.not_valid_after_utc
    except Exception:  # noqa: BLE001
        not_after = None
    return sorted(names), not_after


def _read_cert_bytes(path: Path) -> bytes | None:
    try:
        return path.read_bytes()
    except OSError:
        return None


def refresh_cert_store() -> None:
    """Re-mirror every Let's Encrypt live cert into the opanel-ent-readable store.

    /etc/letsencrypt/live is root-only, so the panel reads certs from the
    /etc/opanel-ent/certs mirror instead. Certs issued before that mirror existed
    (or on a box where no issue/renew has run since) would be missing, so the
    "use existing" pickers refresh it first. Best-effort.
    """
    try:
        shell.privileged(
            "panel-cert-sync",
            check=False,
            fallback=["bash", "-lc", "true"],
        )
    except Exception:  # noqa: BLE001
        pass


def list_available_certificates(target_domain: str | None = None) -> list[dict]:
    """Every certificate already installed on the box that a website could be
    pointed at: Let's Encrypt live certs (via the opanel-ent-readable mirror) and
    certs installed manually through the panel.

    Each entry: name ("<source>:<primary domain>"), source, domains, is_wildcard,
    covers_domain (against ``target_domain``), not_after.
    """
    safe_target = (target_domain or "").strip().lower() or None
    results: list[dict] = []
    seen: set[str] = set()

    def _add(source: str, key_name: str, cert_path: Path) -> None:
        raw = _read_cert_bytes(cert_path)
        if not raw:
            return
        domains, not_after = _cert_names_and_expiry(raw)
        if not domains:
            return
        entry_id = f"{source}:{key_name}"
        if entry_id in seen:
            return
        seen.add(entry_id)
        results.append({
            "name": entry_id,
            "source": source,
            "domains": domains,
            "is_wildcard": any(name.startswith("*.") for name in domains),
            "covers_domain": bool(
                safe_target and any(_hostname_matches(safe_target, name) for name in domains)
            ),
            "not_after": not_after,
        })

    if PANEL_CERT_STORE.is_dir():
        for entry in sorted(PANEL_CERT_STORE.iterdir()):
            if not entry.is_dir() or entry.name.startswith("_"):
                continue
            _add("letsencrypt", entry.name, entry / "fullchain.pem")

    if MANUAL_SSL_ROOT.is_dir():
        for entry in sorted(MANUAL_SSL_ROOT.iterdir()):
            if not entry.is_dir():
                continue
            cert_file = entry / "fullchain.crt"
            if not cert_file.is_file():
                cert_file = entry / "cert.crt"
            _add("manual", entry.name, cert_file)

    return results


def reuse_cert_paths(reuse_name: str) -> dict[str, str | None]:
    """Resolve '<source>:<name>' to the cert/key/ca paths OpenLiteSpeed should use."""
    source, _, name = (reuse_name or "").partition(":")
    safe = _safe_domain(name) if name else ""
    if source == "letsencrypt" and safe:
        base = f"/etc/letsencrypt/live/{safe}"
        return {"cert": f"{base}/fullchain.pem", "key": f"{base}/privkey.pem", "ca": None}
    if source == "manual" and safe:
        base = f"/usr/local/lsws/conf/opanel-ent/ssl/sites/{safe}"
        ca_path = Path(base) / "ca.crt"
        return {
            "cert": f"{base}/fullchain.crt",
            "key": f"{base}/privkey.key",
            "ca": f"{base}/ca.crt" if ca_path.is_file() else None,
        }
    raise ValueError("Invalid certificate name")


def reuse_cert_covers(reuse_name: str, domain: str) -> tuple[bool, str]:
    """(usable, reason) — is the named cert present and does its SAN cover ``domain``."""
    safe_domain = (domain or "").strip().lower()
    for entry in list_available_certificates(safe_domain):
        if entry["name"] == reuse_name:
            if not entry["covers_domain"]:
                return False, f"certificate {reuse_name.split(':', 1)[-1]} does not cover {safe_domain}"
            return True, ""
    return False, "certificate not found on this server"


def manual_ssl_paths(domain: str) -> dict[str, str | None]:
    safe_domain = _safe_domain(domain)
    base = f"/usr/local/lsws/conf/opanel-ent/ssl/sites/{safe_domain}"
    return {
        "cert": f"{base}/cert.crt",
        "key": f"{base}/privkey.key",
        "ca": f"{base}/ca.crt",
    }


def read_ssl_part(value, *, label: str, required: bool = True) -> bytes:
    if value is None:
        if required:
            raise ValueError(f"{label} is required")
        return b""
    filename = getattr(value, "filename", None)
    if filename:
        suffix = Path(filename).suffix.lower()
        if suffix not in ALLOWED_SSL_EXTENSIONS:
            raise ValueError(f"{label} must be .crt, .pem, .key, or .ca")
    if hasattr(value, "file"):
        raw = _read_limited(value.file, label)
    elif isinstance(value, bytes):
        raw = value
    else:
        raw = str(value).encode("utf-8")
    return _normalize_pem(raw, label, required=required)


def validate_manual_ssl(
    domain: str,
    certificate: bytes,
    private_key: bytes,
    ca_bundle: bytes = b"",
    aliases: list[str] | tuple[str, ...] | None = None,
) -> None:
    domains = _safe_domain_list(domain, aliases)
    cert = _load_certificate(certificate, "certificate")
    key = _load_private_key(private_key)
    if ca_bundle:
        _load_ca_bundle(ca_bundle)
    _validate_certificate_time(cert)
    for name in domains:
        _validate_certificate_domain(cert, name)
    _validate_key_matches_certificate(key, cert)


def install_manual_ssl(
    domain: str,
    certificate: bytes,
    private_key: bytes,
    ca_bundle: bytes = b"",
    aliases: list[str] | tuple[str, ...] | None = None,
) -> dict[str, str | None]:
    validate_manual_ssl(domain, certificate, private_key, ca_bundle, aliases=aliases)
    return _write_manual_ssl_files(domain, certificate, private_key, ca_bundle)


def _write_manual_ssl_files(domain: str, certificate: bytes, private_key: bytes, ca_bundle: bytes = b"") -> dict[str, str | None]:
    paths = manual_ssl_paths(domain)
    payload = json.dumps(
        {
            "certificate": certificate.decode("utf-8"),
            "private_key": private_key.decode("utf-8"),
            "ca_bundle": ca_bundle.decode("utf-8") if ca_bundle else "",
        }
    )
    fallback = [
        "python",
        "-c",
        (
            "import json,pathlib,sys;"
            "domain=sys.argv[1];"
            "base=pathlib.Path('/usr/local/lsws/conf/opanel-ent/ssl/sites')/domain;"
            "base.mkdir(parents=True, exist_ok=True);"
            "data=json.load(sys.stdin);"
            "(base/'cert.crt').write_text(data['certificate'], encoding='utf-8');"
            "(base/'privkey.key').write_text(data['private_key'], encoding='utf-8');"
            "ca=data.get('ca_bundle') or '';"
            "(base/'ca.crt').write_text(ca, encoding='utf-8') if ca else (base/'ca.crt').unlink(missing_ok=True)"
        ),
        _safe_domain(domain),
    ]
    result = shell.privileged(
        "manual-ssl-install",
        helper_args=[_safe_domain(domain)],
        check=False,
        input=payload,
        sensitive=True,
        fallback=fallback,
    )
    if result.returncode != 0:
        raise RuntimeError((result.stderr or result.stdout or "Could not install manual SSL").strip())
    if not ca_bundle:
        paths["ca"] = None
    return paths


def snapshot_manual_ssl(cert_path: str | None, key_path: str | None, ca_path: str | None) -> ManualSslSnapshot | None:
    if not cert_path or not key_path:
        return None
    snapshot = ManualSslSnapshot(cert_path, key_path, ca_path)
    snapshot.capture()
    return snapshot


def snapshot_manual_ssl_domain(domain: str) -> ManualSslSnapshot:
    paths = manual_ssl_paths(domain)
    snapshot = ManualSslSnapshot(paths["cert"], paths["key"], paths["ca"], domain=domain)
    snapshot.capture()
    return snapshot


def restore_manual_ssl(snapshot: ManualSslSnapshot | None) -> None:
    if snapshot is not None:
        try:
            snapshot.restore()
        except (RuntimeError, OSError):
            pass


def remove_manual_ssl(domain: str) -> None:
    safe_domain = _safe_domain(domain)
    fallback = [
        "python",
        "-c",
        (
            "import pathlib,sys;"
            "base=pathlib.Path('/usr/local/lsws/conf/opanel-ent/ssl/sites')/sys.argv[1];"
            "[(base/name).unlink(missing_ok=True) for name in ('cert.crt','privkey.key','ca.crt','fullchain.crt')];"
            "base.rmdir() if base.exists() and not any(base.iterdir()) else None"
        ),
        safe_domain,
    ]
    result = shell.privileged("manual-ssl-remove", helper_args=[safe_domain], check=False, fallback=fallback)
    if result.returncode != 0:
        raise RuntimeError((result.stderr or result.stdout or "Could not remove manual SSL").strip())


def remove_manual_ssl_files(cert_path: str | None, key_path: str | None, ca_path: str | None) -> None:
    if cert_path:
        try:
            remove_manual_ssl(Path(cert_path).parent.name)
            return
        except (RuntimeError, ValueError, OSError):
            pass
    for raw_path in (cert_path, key_path, ca_path):
        if raw_path:
            try:
                Path(raw_path).unlink(missing_ok=True)
            except OSError:
                pass
    if cert_path:
        try:
            Path(cert_path).with_name("fullchain.crt").unlink(missing_ok=True)
        except OSError:
            pass


def _read_limited(handle: BinaryIO, label: str) -> bytes:
    raw = handle.read(MAX_SSL_PART_BYTES + 1)
    if len(raw) > MAX_SSL_PART_BYTES:
        raise ValueError(f"{label} is too large")
    return raw


def _normalize_pem(raw: bytes, label: str, *, required: bool) -> bytes:
    if not raw:
        if required:
            raise ValueError(f"{label} is required")
        return b""
    if b"\x00" in raw:
        raise ValueError(f"{label} contains a NUL byte")
    try:
        text = raw.decode("utf-8", errors="strict").replace("\r\n", "\n").strip()
    except UnicodeDecodeError as exc:
        raise ValueError(f"{label} must be UTF-8 PEM text") from exc
    if not text:
        if required:
            raise ValueError(f"{label} is required")
        return b""
    return (text + "\n").encode("utf-8")


def _safe_domain(domain: str) -> str:
    safe = (domain or "").strip().lower()
    if not safe or "/" in safe or "\\" in safe or ".." in safe:
        raise ValueError("Invalid domain")
    return safe


def _load_certificate(raw: bytes, label: str):
    try:
        return x509.load_pem_x509_certificate(raw)
    except ValueError as exc:
        raise ValueError(f"{label} is not a valid PEM certificate") from exc


def _load_ca_bundle(raw: bytes) -> None:
    try:
        certs = x509.load_pem_x509_certificates(raw)
    except ValueError as exc:
        raise ValueError("ca_bundle is not a valid PEM certificate bundle") from exc
    if not certs:
        raise ValueError("ca_bundle is not a valid PEM certificate bundle")


def _load_private_key(raw: bytes):
    try:
        return serialization.load_pem_private_key(raw, password=None)
    except (TypeError, ValueError) as exc:
        raise ValueError("private_key is not a valid unencrypted PEM private key") from exc


def _validate_certificate_time(cert) -> None:
    now = datetime.now(timezone.utc)
    not_before = cert.not_valid_before_utc
    not_after = cert.not_valid_after_utc
    if now < not_before:
        raise ValueError("certificate is not valid yet")
    if now >= not_after:
        raise ValueError("certificate is expired")


def _validate_certificate_domain(cert, domain: str) -> None:
    names: set[str] = set()
    try:
        san = cert.extensions.get_extension_for_oid(ExtensionOID.SUBJECT_ALTERNATIVE_NAME).value
        names.update(name.lower() for name in san.get_values_for_type(x509.DNSName))
    except x509.ExtensionNotFound:
        pass
    if not names:
        names.update(attr.value.lower() for attr in cert.subject.get_attributes_for_oid(NameOID.COMMON_NAME))
    if not any(_hostname_matches(domain, name) for name in names):
        raise ValueError("certificate CN/SAN does not match the website domain")


def _hostname_matches(domain: str, pattern: str) -> bool:
    if pattern == domain:
        return True
    if not pattern.startswith("*."):
        return False
    suffix = pattern[1:]
    return domain.endswith(suffix) and domain.count(".") == suffix.count(".")


def _validate_key_matches_certificate(private_key, cert) -> None:
    cert_public = cert.public_key()
    try:
        if isinstance(private_key, rsa.RSAPrivateKey) and isinstance(cert_public, rsa.RSAPublicKey):
            message = b"opanel-ent-manual-ssl-check"
            signature = private_key.sign(message, padding.PKCS1v15(), hashes.SHA256())
            cert_public.verify(signature, message, padding.PKCS1v15(), hashes.SHA256())
            return
        if isinstance(private_key, ec.EllipticCurvePrivateKey) and isinstance(cert_public, ec.EllipticCurvePublicKey):
            message = b"opanel-ent-manual-ssl-check"
            signature = private_key.sign(message, ec.ECDSA(hashes.SHA256()))
            cert_public.verify(signature, message, ec.ECDSA(hashes.SHA256()))
            return
        if isinstance(private_key, ed25519.Ed25519PrivateKey) and isinstance(cert_public, ed25519.Ed25519PublicKey):
            message = b"opanel-ent-manual-ssl-check"
            cert_public.verify(private_key.sign(message), message)
            return
        if isinstance(private_key, ed448.Ed448PrivateKey) and isinstance(cert_public, ed448.Ed448PublicKey):
            message = b"opanel-ent-manual-ssl-check"
            cert_public.verify(private_key.sign(message), message)
            return
    except Exception as exc:
        raise ValueError("private_key does not match certificate") from exc
    if _public_bytes(private_key.public_key()) != _public_bytes(cert_public):
        raise ValueError("private_key does not match certificate")


def _public_bytes(public_key) -> bytes:
    return public_key.public_bytes(
        encoding=serialization.Encoding.DER,
        format=serialization.PublicFormat.SubjectPublicKeyInfo,
    )
