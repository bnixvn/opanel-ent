"""
OpenLiteSpeed (OLS) webserver service layer for OPanel Enterprise.

Manages vhost configs under /usr/local/lsws/conf/opanel-ent/,
included from the main httpd_config.conf.
Replaces the former nginx.py service.
"""

import os
import re
import tempfile
from pathlib import Path
from typing import Iterable, Optional

from jinja2 import Environment, FileSystemLoader

from app.core.config import settings
from app.services import site_users
from app.services.shell import shell

# ---------------------------------------------------------------------------
# Paths
# ---------------------------------------------------------------------------
OLS_CONF_ROOT = Path("/usr/local/lsws/conf")
OPANEL_ENT_CONF_DIR = OLS_CONF_ROOT / "opanel-ent"
OPANEL_ENT_VHOSTS_DIR = OPANEL_ENT_CONF_DIR / "vhosts"
OPANEL_ENT_MODSEC_DIR = OPANEL_ENT_CONF_DIR / "waf"
OPANEL_ENT_CUSTOM_DIR = OPANEL_ENT_CONF_DIR / "custom"
OPANEL_ENT_SSL_DIR = OPANEL_ENT_CONF_DIR / "ssl" / "sites"
TEMPLATE_DIR = Path(__file__).resolve().parent.parent / "templates" / "openlitespeed"

# ---------------------------------------------------------------------------
# Constants
# ---------------------------------------------------------------------------
ALLOWED_PHP_VERSIONS = {"5.6", "7.4", "8.0", "8.1", "8.2", "8.3", "8.4", "8.5"}
ALLOWED_APP_TYPES = {"wordpress", "php", "static"}
ALLOWED_REWRITE_MODES = {"none", "front_controller", "laravel", "codeigniter", "seohburl"}
ALLOWED_LOG_KINDS = {"access", "error"}
DOMAIN_RE = re.compile(r"[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?(?:\.[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?)+")
MAX_FULL_CONFIG_BYTES = 128 * 1024

ACME_WEBROOT = "/var/www/opanel-ent-acme"

WORDPRESS_CSP = (
    "default-src 'self' https: data: blob:; "
    "script-src 'self' 'unsafe-inline' 'unsafe-eval' https:; "
    "style-src 'self' 'unsafe-inline' https:; "
    "img-src 'self' data: https: blob:; "
    "font-src 'self' data: https:; "
    "connect-src 'self' https:; "
    "frame-src 'self' https: blob:; "
    "worker-src 'self' blob:; "
    "object-src 'none'; "
    "base-uri 'self'; "
    "form-action 'self' https:; "
    "upgrade-insecure-requests"
)

SECURITY_HEADERS = (
    "X-Content-Type-Options: nosniff\n"
    "Referrer-Policy: strict-origin-when-cross-origin\n"
    "Permissions-Policy: accelerometer=(), autoplay=(), camera=(), display-capture=(), "
    "encrypted-media=(), fullscreen=(), geolocation=(), gyroscope=(), magnetometer=(), "
    "microphone=(), midi=(), payment=(), usb=()"
)
HSTS_HEADER = "Strict-Transport-Security: max-age=31536000; includeSubDomains"

# ---------------------------------------------------------------------------
# Jinja2 renderer
# ---------------------------------------------------------------------------
_jinja_env: Optional[Environment] = None


def _get_jinja_env() -> Environment:
    global _jinja_env
    if _jinja_env is None:
        _jinja_env = Environment(
            loader=FileSystemLoader(str(TEMPLATE_DIR)),
            keep_trailing_newline=True,
            trim_blocks=True,
            lstrip_blocks=True,
        )
    return _jinja_env


# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------
def _safe_domain(domain: str) -> str:
    safe_domain = (domain or "").strip().lower()
    if not DOMAIN_RE.fullmatch(safe_domain):
        raise ValueError("Invalid domain")
    return safe_domain


def _safe_alias_domains(aliases: list[str] | tuple[str, ...] | None) -> list[str]:
    safe_aliases: list[str] = []
    seen: set[str] = set()
    for alias in aliases or []:
        safe_alias = _safe_domain(str(alias))
        if safe_alias in seen:
            continue
        safe_aliases.append(safe_alias)
        seen.add(safe_alias)
    return safe_aliases


def _safe_redirects(redirects, target_domain: str) -> list[dict]:
    """Normalise redirects to the {source,target,code} shape the template reads.

    The API passes plain domain strings. Jinja renders a missing attribute as an
    empty string, so those produced `RewriteCond %{HTTP_HOST} ^(www\\.)?$` --
    a condition no real host can match, and the redirect silently did nothing.
    """
    normalised: list[dict] = []
    seen: set[str] = set()
    for entry in redirects or []:
        if isinstance(entry, dict):
            source = _safe_domain(str(entry.get("source", "")))
            target = str(entry.get("target") or f"https://{target_domain}").rstrip("/")
            code = int(entry.get("code") or 301)
        else:
            source = _safe_domain(str(entry))
            target = f"https://{target_domain}"
            code = 301
        if source in seen:
            continue
        seen.add(source)
        if code not in {301, 302, 307, 308}:
            code = 301
        normalised.append(
            # `host` goes in vhAliases so LiteSpeed routes the old domain to this
            # vhost at all; `source` is the regex-escaped form for RewriteCond.
            {"host": source, "source": re.escape(source), "target": target, "code": code}
        )
    return normalised


def _safe_hostnames(hostnames: Iterable[str]) -> list[str]:
    safe_hosts: list[str] = []
    seen: set[str] = set()
    for hostname in hostnames:
        try:
            safe_hostname = _safe_domain(str(hostname))
        except ValueError:
            continue
        if safe_hostname in seen:
            continue
        safe_hosts.append(safe_hostname)
        seen.add(safe_hostname)
    return safe_hosts


def _redirect_source_domains(redirects: list | tuple | None) -> list[str]:
    domains: list[str] = []
    for redirect in redirects or []:
        if isinstance(redirect, dict):
            source = redirect.get("source") or redirect.get("domain")
        else:
            source = redirect
        if source:
            domains.append(str(source))
    return domains


def _vhost_hostnames(domain: str, aliases: list[str] | tuple[str, ...] | None, redirects: list | tuple | None) -> list[str]:
    return _safe_hostnames([domain, *(aliases or []), *_redirect_source_domains(redirects)])


def _check_php_version(php_version: str | None) -> str | None:
    if php_version is None:
        return None
    if php_version not in ALLOWED_PHP_VERSIONS:
        raise ValueError(f"Unsupported PHP version: {php_version}")
    return php_version


def _check_app_type(app_type: str) -> str:
    if app_type not in ALLOWED_APP_TYPES:
        raise ValueError(f"Unsupported app type: {app_type}")
    return app_type


def _check_rewrite_mode(mode: str | None) -> str:
    value = (mode or "none").strip().lower()
    if value not in ALLOWED_REWRITE_MODES:
        raise ValueError(f"Unsupported rewrite mode: {mode}")
    return value


def _check_log_kind(kind: str) -> str:
    value = (kind or "").strip().lower()
    if value not in ALLOWED_LOG_KINDS:
        raise ValueError("Log kind must be access or error")
    return value


def _check_tail_lines(lines: int) -> int:
    try:
        value = int(lines)
    except (TypeError, ValueError) as exc:
        raise ValueError("Log lines must be a number") from exc
    if value < 1 or value > 5000:
        raise ValueError("Log lines must be between 1 and 5000")
    return value


def _effective_document_root(document_root: str, rewrite_mode: str) -> str:
    safe_root = site_users.validate_document_root(document_root)
    if rewrite_mode in {"laravel", "codeigniter"} and safe_root.rstrip("/") == "public_html":
        return "public_html/public"
    return safe_root


def _lsphp_binary(php_version: str) -> str:
    """Return the path to the LSPHP binary for a given PHP version."""
    ver = php_version.replace(".", "")
    return f"/usr/local/lsws/lsphp{ver}/bin/lsphp"


def _lsphp_listener_name(php_version: str) -> str:
    """Return the OLS external app / listener name for a PHP version."""
    ver = php_version.replace(".", "")
    return f"lsphp{ver}"


# ---------------------------------------------------------------------------
# WAF paths
# ---------------------------------------------------------------------------
def waf_rules_file(domain: str) -> str:
    safe_domain = _safe_domain(domain)
    return (OPANEL_ENT_MODSEC_DIR / "sites" / f"{safe_domain}.conf").as_posix()


# ---------------------------------------------------------------------------
# Config paths
# ---------------------------------------------------------------------------
def _vhost_dir(domain: str) -> Path:
    safe_domain = _safe_domain(domain)
    return OPANEL_ENT_VHOSTS_DIR / safe_domain


def _vhost_conf_path(domain: str) -> Path:
    return _vhost_dir(domain) / "vhost.conf"


def _log_path(domain: str, kind: str) -> Path:
    safe_domain = _safe_domain(domain)
    safe_kind = _check_log_kind(kind)
    return Path("/var/log/openlitespeed") / f"{safe_domain}.{safe_kind}.log"


def _php_error_log_path(domain: str) -> Path:
    """PHP's own error_log. The site's PHP runs as its Linux user, which cannot
    write the OLS-owned ``<domain>.error.log`` -- so PHP logs into a per-domain
    directory the site user owns. This is why a site with a PHP fatal used to
    leave no trace in the panel's Error tab."""
    return Path("/var/log/openlitespeed") / _safe_domain(domain) / "php_error.log"


# ---------------------------------------------------------------------------
# Custom config validation
# ---------------------------------------------------------------------------
DANGEROUS_DIRECTIVES_RE = re.compile(
    r"(?mi)^\s*("
    r"include\b|"
    r"loadModule\b|"
    r"user\s|"
    r"daemon\s|"
    r"pid\s|"
    r"workingDir\b|"
    r"extprocessor\s|"
    r"context\s|"
    r"vhDomain\s|"
    r"docRoot\s|"
    r"errorlog\s|"
    r"accesslog\s|"
    r"realm\s|"
    r"authName\s|"
    r"allowOverride\s"
    r")"
)


def validate_custom_directives(content: Optional[str]) -> str:
    """Sanitize and validate custom OLS directives."""
    if not content:
        return ""
    text = content.replace("\r\n", "\n").strip()
    if len(text) > 16 * 1024:
        raise ValueError("Custom directives block is too large")
    if "\x00" in text:
        raise ValueError("Custom directives block contains a NUL byte")
    # Check for balanced braces
    depth = 0
    for ch in text:
        if ch == "{":
            depth += 1
        elif ch == "}":
            depth -= 1
        if depth < 0:
            raise ValueError("Unbalanced braces in custom directives block")
    if depth != 0:
        raise ValueError("Unbalanced braces in custom directives block")
    # Reject dangerous directives
    for line in text.splitlines():
        stripped = line.strip()
        if stripped.startswith("#"):
            continue
        if DANGEROUS_DIRECTIVES_RE.match(stripped):
            raise ValueError(f"Dangerous directive rejected: {stripped.split()[0] if stripped.split() else stripped}")
    return text


def validate_full_config(content: Optional[str]) -> str:
    """Validate a full OLS vhost config block."""
    if not content:
        raise ValueError("Config is required")
    text = content.replace("\r\n", "\n").strip()
    if len(text) > MAX_FULL_CONFIG_BYTES:
        raise ValueError("Config is too large")
    if "\x00" in text:
        raise ValueError("Config contains a NUL byte")
    # Must contain a vhost definition
    if "docRoot" not in text and "vhRoot" not in text:
        raise ValueError("Config must contain a vhost definition with docRoot or vhRoot")
    return text


# ---------------------------------------------------------------------------
# Rewrite rules per mode
# ---------------------------------------------------------------------------
# Kept for callers that still read it; the renderer no longer emits context-scope
# rewrites, see REWRITE_RULE_LINES and _build_context.
REWRITE_RULES = {mode: "" for mode in ALLOWED_REWRITE_MODES}

# The same rules as bare lines, for emitting at vhost scope instead of inside
# `context /`.  Inside a context, LiteSpeed fails to resolve %{REQUEST_FILENAME}
# once the document root sits below the site root (Laravel's public_html/public),
# so !-f never matches and *every* request — including real static assets — is
# rewritten to index.php.  At vhost scope the file tests resolve correctly.
# Targets are absolute here because there is no context prefix to resolve against.
REWRITE_RULE_LINES = {
    "none": "",
    "front_controller": (
        "RewriteCond %{REQUEST_FILENAME} !-f\n"
        "RewriteCond %{REQUEST_FILENAME} !-d\n"
        "RewriteRule ^(.*)$ /index.php [QSA,L]"
    ),
    "laravel": (
        "RewriteCond %{REQUEST_FILENAME} !-f\n"
        "RewriteCond %{REQUEST_FILENAME} !-d\n"
        "RewriteRule ^(.*)$ /index.php [QSA,L]"
    ),
    "codeigniter": (
        "RewriteCond %{REQUEST_FILENAME} !-f\n"
        "RewriteCond %{REQUEST_FILENAME} !-d\n"
        "RewriteCond $1 !^(index\\.php)\n"
        "RewriteRule ^(.*)$ /index.php/$1 [QSA,L]"
    ),
    "seohburl": (
        "RewriteCond %{REQUEST_FILENAME} !-f\n"
        "RewriteCond %{REQUEST_FILENAME} !-d\n"
        "RewriteRule ^([^?]*) /index.php?_url_=$1 [QSA,L]"
    ),
}


# ---------------------------------------------------------------------------
# Core rendering
# ---------------------------------------------------------------------------
def _build_context(
    domain: str,
    root_path: str,
    *,
    app_type: str = "wordpress",
    php_version: str | None = "8.4",
    document_root: str = "public_html",
    rewrite_mode: str = "none",
    custom_directives: str = "",
    ssl_enabled: bool = False,
    ssl_cert_path: str | None = None,
    ssl_key_path: str | None = None,
    ssl_ca_path: str | None = None,
    aliases: list[str] | None = None,
    redirects: list[dict] | None = None,
    waf_enabled: bool = False,
    linux_user: str | None = None,
    lsphp_socket_override: str | None = None,
) -> dict:
    """Build the template context for vhost rendering."""
    safe_domain = _safe_domain(domain)
    checked_app = _check_app_type(app_type)
    checked_php = _check_php_version(php_version) if checked_app != "static" else None
    checked_rewrite = _check_rewrite_mode(rewrite_mode)
    safe_doc_root = _effective_document_root(document_root, checked_rewrite)
    safe_aliases = _safe_alias_domains(aliases)
    has_ssl = bool(ssl_cert_path and ssl_key_path)

    # Determine LSPHP external app
    lsphp_app = ""
    lsphp_path = ""
    lsphp_socket = ""
    if checked_php and checked_app != "static":
        lsphp_app = _lsphp_listener_name(checked_php)
        lsphp_path = _lsphp_binary(checked_php)
        lsphp_socket = lsphp_socket_override or f"/tmp/lshttpd/{lsphp_app}.sock"

    # Front-controller rules go at vhost scope, never inside `context /`.
    # Inside the context LiteSpeed stops resolving %{REQUEST_FILENAME}, so the
    # !-f guard never matches and every request -- real static files included --
    # is handed to index.php. Measured on a live install for all four modes.
    rewrite_block = ""
    vhost_rewrite_rules = REWRITE_RULE_LINES.get(checked_rewrite, "")

    return {
        "domain": safe_domain,
        "root_path": root_path,
        "document_root": safe_doc_root,
        "app_type": checked_app,
        "php_version": checked_php,
        "lsphp_app": lsphp_app,
        "lsphp_path": lsphp_path,
        "lsphp_socket": lsphp_socket,
        "rewrite_mode": checked_rewrite,
        "rewrite_block": rewrite_block,
        "vhost_rewrite_rules": vhost_rewrite_rules,
        "custom_directives": "",
        "ssl_enabled": ssl_enabled,
        "has_ssl": has_ssl,
        "ssl_cert_path": ssl_cert_path or "",
        "ssl_key_path": ssl_key_path or "",
        "ssl_ca_path": ssl_ca_path or "",
        "aliases": safe_aliases,
        "redirects": _safe_redirects(redirects, safe_domain),
        "waf_enabled": waf_enabled,
        "waf_rules_file": waf_rules_file(safe_domain) if waf_enabled else "",
        "linux_user": linux_user or "www-data",
        "access_log": _log_path(safe_domain, "access").as_posix(),
        "error_log": _log_path(safe_domain, "error").as_posix(),
        "php_error_log": _php_error_log_path(safe_domain).as_posix(),
        "acme_webroot": ACME_WEBROOT,
        "security_headers": SECURITY_HEADERS,
        "hsts_header": HSTS_HEADER if has_ssl else "",
        "csp_header": WORDPRESS_CSP if checked_app == "wordpress" else "",
    }


def render_vhost(
    domain: str,
    root_path: str,
    *,
    app_type: str = "wordpress",
    php_version: str | None = "8.4",
    document_root: str = "public_html",
    rewrite_mode: str = "none",
    custom_directives: str = "",
    ssl_enabled: bool = False,
    ssl_cert_path: str | None = None,
    ssl_key_path: str | None = None,
    ssl_ca_path: str | None = None,
    aliases: list[str] | None = None,
    redirects: list[dict] | None = None,
    waf_enabled: bool = False,
    linux_user: str | None = None,
    lsphp_socket_override: str | None = None,
) -> str:
    """Render an OLS vhost config from the template."""
    ctx = _build_context(
        domain, root_path,
        app_type=app_type,
        php_version=php_version,
        document_root=document_root,
        rewrite_mode=rewrite_mode,
        custom_directives=custom_directives,
        ssl_enabled=ssl_enabled,
        ssl_cert_path=ssl_cert_path,
        ssl_key_path=ssl_key_path,
        ssl_ca_path=ssl_ca_path,
        aliases=aliases,
        redirects=redirects,
        waf_enabled=waf_enabled,
        linux_user=linux_user,
        lsphp_socket_override=lsphp_socket_override,
    )
    env = _get_jinja_env()
    template_name = f"{ctx['app_type']}.conf.j2"
    try:
        template = env.get_template(template_name)
    except Exception:
        template = env.get_template("php.conf.j2")
    return template.render(**ctx)


def rewrite_vhost(
    domain: str,
    root_path: str,
    **kwargs,
) -> str:
    """Render and write a vhost config to disk, then restart OLS.

    ``defer_reload=True`` stages the vhost file without syncing the main config
    or restarting OpenLiteSpeed. A bulk caller (e.g. the DirectAdmin import,
    which can write dozens of vhosts in one run) must call ``reload_service()``
    once when it is done instead of paying an OLS restart per vhost.
    """
    kwargs.pop("preserve_existing_ssl", None)
    kwargs.pop("include_ssl", None)
    defer_reload = bool(kwargs.pop("defer_reload", False))
    if kwargs.get("ssl_enabled") and not (kwargs.get("ssl_cert_path") and kwargs.get("ssl_key_path")):
        live_dir = Path("/etc/letsencrypt/live") / _safe_domain(domain)
        kwargs["ssl_cert_path"] = str(live_dir / "fullchain.pem")
        kwargs["ssl_key_path"] = str(live_dir / "privkey.pem")
    content = render_vhost(domain, root_path, **kwargs)
    safe_domain = _safe_domain(domain)
    hostnames = _vhost_hostnames(safe_domain, kwargs.get("aliases"), kwargs.get("redirects"))
    subcommand = "ols-vhost-write-defer" if defer_reload else "ols-vhost-write"
    reload_fallback = (
        "true" if defer_reload else
        "(systemctl restart lshttpd.service 2>/dev/null || "
        "/usr/local/lsws/bin/lswsctrl restart 2>/dev/null || true)"
    )
    shell.privileged(
        subcommand,
        helper_args=[safe_domain, *hostnames],
        input=content,
        fallback=[
            "bash", "-lc",
            "mkdir -p /usr/local/lsws/conf/opanel-ent/vhosts/$1 && "
            "cat > /usr/local/lsws/conf/opanel-ent/vhosts/$1/vhost.conf && "
            f"{reload_fallback}",
            "opanel-ent-ols-vhost-write",
            safe_domain,
        ],
    )
    return content

def remove_vhost(domain: str) -> None:
    """Remove a vhost config directory."""
    safe_domain = _safe_domain(domain)
    shell.privileged(
        "ols-vhost-delete",
        helper_args=[safe_domain],
        check=False,
        fallback=[
            "bash", "-lc",
            "rm -rf /usr/local/lsws/conf/opanel-ent/vhosts/$1 && "
            "(systemctl restart lshttpd.service 2>/dev/null || "
            "/usr/local/lsws/bin/lswsctrl restart 2>/dev/null || true)",
            "opanel-ent-ols-vhost-delete",
            safe_domain,
        ],
    )

def suspend_vhost(domain: str) -> None:
    """Suspend a vhost by renaming its config to .conf.suspended and restarting OLS."""
    safe_domain = _safe_domain(domain)
    shell.privileged(
        "ols-vhost-suspend",
        helper_args=[safe_domain],
        check=False,
        fallback=[
            "bash", "-lc",
            "conf=/usr/local/lsws/conf/opanel-ent/vhosts/$1/vhost.conf && "
            "test -f $conf && mv $conf ${conf}.suspended && "
            "(systemctl restart lshttpd.service 2>/dev/null || "
            "/usr/local/lsws/bin/lswsctrl restart 2>/dev/null || true)",
            "opanel-ent-ols-vhost-suspend",
            safe_domain,
        ],
    )

def restore_vhost(domain: str) -> None:
    """Restore a suspended vhost by renaming .conf.suspended back to .conf."""
    safe_domain = _safe_domain(domain)
    shell.privileged(
        "ols-vhost-restore",
        helper_args=[safe_domain],
        check=False,
        fallback=[
            "bash", "-lc",
            "conf=/usr/local/lsws/conf/opanel-ent/vhosts/$1/vhost.conf && "
            "test -f ${conf}.suspended && mv ${conf}.suspended $conf && "
            "(systemctl restart lshttpd.service 2>/dev/null || "
            "/usr/local/lsws/bin/lswsctrl restart 2>/dev/null || true)",
            "opanel-ent-ols-vhost-restore",
            safe_domain,
        ],
    )


def get_vhost_config(domain: str) -> str | None:
    """Read the current vhost config for a domain."""
    conf_path = _vhost_conf_path(domain)
    if conf_path.exists():
        return conf_path.read_text(encoding="utf-8")
    return None


def read_vhost_config(domain: str) -> str:
    """Read the current vhost config for a domain."""
    content = get_vhost_config(domain)
    if content is None:
        raise FileNotFoundError(str(_vhost_conf_path(domain)))
    return content


def _first_match(pattern: str, content: str) -> str | None:
    match = re.search(pattern, content)
    return match.group(1).strip() if match else None


def _config_document_root(content: str) -> tuple[str, str]:
    doc_root = _first_match(r"(?m)^\s*docRoot\s+(.+?)\s*$", content)
    if not doc_root:
        return "", "public_html"
    normalized = doc_root.rstrip("/")
    for marker in ("/public_html/public", "/public_html"):
        if normalized.endswith(marker):
            return normalized[: -len(marker)], marker.lstrip("/")
    parent, _, child = normalized.rpartition("/")
    return parent or normalized, child or "public_html"


def _rewrite_mode_from_config(content: str, app_type: str) -> str:
    if app_type == "wordpress":
        return "front_controller"
    if app_type == "static":
        return "none"
    if "index.php/$1" in content:
        return "codeigniter"
    if "index.php?_url_=$1" in content:
        return "seohburl"
    # Both spellings: the target gained a leading slash when the rules moved to
    # vhost scope, and toggling WAF on an older vhost must not read as "none" --
    # that silently drops the front controller and 404s every route but /.
    if "$ index.php [QSA,L]" in content or "$ /index.php [QSA,L]" in content:
        return "laravel" if "/public_html/public" in content else "front_controller"
    return "none"


def _php_version_from_config(content: str) -> str | None:
    digits = _first_match(r"(?m)^\s*path\s+/usr/local/lsws/lsphp([0-9]+)/bin/lsphp\s*$", content)
    if not digits or len(digits) < 2:
        return None
    return f"{digits[:-1]}.{digits[-1]}"


def _redirects_from_config(content: str, domain: str) -> list[dict]:
    """Read redirects back out of a rendered vhost.

    Handles both the current `RewriteRule ^ <target>%{REQUEST_URI}` form and the
    older `RewriteRule ^(.*)$ <target>$1` one, so re-rendering a vhost written by
    an earlier build does not silently drop its redirects.
    """
    redirects: list[dict] = []
    pattern = re.compile(
        r"RewriteCond\s+%\{HTTP_HOST\}\s+\^\(www\\\.\)\?(.+?)\$\s+\[NC\]\s*\n"
        r"RewriteRule\s+(?:\^\(\.\*\)\$\s+(?P<old>.+?)\$1|\^\s+(?P<new>.+?)%\{REQUEST_URI\})"
        r"\s+\[R=([0-9]+),L\]",
        re.MULTILINE,
    )
    seen: set[str] = set()
    for match in pattern.finditer(content):
        source = match.group(1).replace(r"\.", ".").strip()
        target = (match.group("old") or match.group("new") or "").strip()
        code = int(match.group(4))
        if source and source != domain and source not in seen:
            seen.add(source)
            redirects.append(
                {"host": source, "source": source, "target": target, "code": code}
            )
    return redirects


def _rewrite_existing_vhost(domain: str, **overrides) -> str:
    safe_domain = _safe_domain(domain)
    existing = read_vhost_config(safe_domain)
    root_path, document_root = _config_document_root(existing)
    php_version = _php_version_from_config(existing)
    if "# Static site: no PHP processor needed" in existing:
        app_type = "static"
    else:
        app_type = "wordpress" if "wp-admin" in existing or "wp-content" in existing else "php"
    kwargs = {
        "app_type": app_type,
        "php_version": php_version or ("8.4" if app_type != "static" else None),
        "document_root": document_root,
        "rewrite_mode": _rewrite_mode_from_config(existing, app_type),
        "custom_directives": "",
        # Redirect sources are listed in vhAliases too (LiteSpeed has to route
        # them here at all), so drop them or a redirect would come back as a
        # plain alias and stop redirecting.
        "aliases": [
            host
            for host in re.findall(r"(?m)^\s*vhAliases\s+(.+?)\s*$", existing)
            if host not in {
                entry.get("host") or entry.get("source")
                for entry in _redirects_from_config(existing, safe_domain)
            }
        ],
        "redirects": _redirects_from_config(existing, safe_domain),
        "waf_enabled": "# OPANEL_ENT WAF BEGIN" in existing,
        "linux_user": _first_match(r"(?m)^\s*extUser\s+(.+?)\s*$", existing) or "www-data",
    }
    cert_path = _first_match(r"(?m)^\s*certFile\s+(.+?)\s*$", existing)
    key_path = _first_match(r"(?m)^\s*keyFile\s+(.+?)\s*$", existing)
    ca_path = _first_match(r"(?m)^\s*CACertFile\s+(.+?)\s*$", existing)
    if cert_path and key_path:
        kwargs.update({
            "ssl_cert_path": cert_path,
            "ssl_key_path": key_path,
            "ssl_ca_path": ca_path,
        })
    kwargs.update(overrides)
    return rewrite_vhost(safe_domain, root_path or f"/home/admin/{safe_domain}", **kwargs)


def update_waf_block(domain: str, enabled: bool) -> str:
    """Compatibility API: re-render an OLS vhost with WAF toggled."""
    return _rewrite_existing_vhost(domain, waf_enabled=bool(enabled))


def update_custom_block(domain: str, custom_directives: str) -> str:
    """Per-domain custom OLS directives are disabled; rewrite without them."""
    validate_custom_directives(custom_directives)
    return _rewrite_existing_vhost(domain, custom_directives="")


# ---------------------------------------------------------------------------
# LSCache (replaces FastCGI cache)
# ---------------------------------------------------------------------------
LSCACHE_REWRITE = (
    "# OPANEL_ENT LSCACHE BEGIN\n"
    "rewrite {\n"
    "    enable              1\n"
    "    rewriteRules        <<<END_RULES\n"
    "RewriteCond %{REQUEST_METHOD} POST\n"
    "RewriteRule .* - [E=Cache-Control:v=no-cache]\n"
    "RewriteCond %{QUERY_STRING} !=\"\"\n"
    "RewriteRule .* - [E=Cache-Control:v=no-cache]\n"
    "RewriteCond %{HTTP_COOKIE} (comment_author|wordpress_[a-f0-9]+|wordpress_logged_in|wp-postpass) [NC]\n"
    "RewriteRule .* - [E=Cache-Control:v=no-cache]\n"
    "END_RULES\n"
    "}\n"
    "# OPANEL_ENT LSCACHE END"
)


# ---------------------------------------------------------------------------
# SSL helpers
# ---------------------------------------------------------------------------
def ssl_paths(domain: str) -> dict[str, str]:
    """Return managed SSL paths for a domain."""
    safe_domain = _safe_domain(domain)
    base = (OPANEL_ENT_SSL_DIR / safe_domain).as_posix()
    return {
        "cert": f"{base}/cert.crt",
        "key": f"{base}/privkey.key",
        "ca": f"{base}/ca.crt",
    }


# ---------------------------------------------------------------------------
# Config test
# ---------------------------------------------------------------------------
def test_config() -> bool:
    """Test OLS configuration validity."""
    result = shell.privileged("ols-config-test", check=False, fallback=[
        "bash", "-lc",
        "systemctl restart lshttpd.service 2>/dev/null || "
        "/usr/local/lsws/bin/lswsctrl restart 2>/dev/null",
    ])
    return result.returncode == 0


def reload_service() -> None:
    """Reload OpenLiteSpeed."""
    shell.privileged("ols-reload", check=False, fallback=[
        "bash", "-lc",
        "systemctl restart lshttpd.service 2>/dev/null || "
        "/usr/local/lsws/bin/lswsctrl restart 2>/dev/null || "
        "/usr/local/lsws/bin/openlitespeed restart 2>/dev/null || true",
    ])


# ---------------------------------------------------------------------------
# Log helpers
# ---------------------------------------------------------------------------
def read_log(domain: str, kind: str = "access", lines: int = 200) -> str:
    """Read the last N lines of a site log."""
    safe_domain = _safe_domain(domain)
    safe_kind = _check_log_kind(kind)
    log_file = _log_path(safe_domain, safe_kind)
    if not log_file.exists():
        return ""
    try:
        count = int(lines)
    except (TypeError, ValueError):
        raise ValueError("Log lines must be a number")
    if count < 1 or count > 5000:
        raise ValueError("Log lines must be between 1 and 5000")
    try:
        result = shell.run(["tail", "-n", str(count), str(log_file)], check=False)
        return result.stdout
    except FileNotFoundError:
        return ""


def read_site_log(domain: str, kind: str = "access", lines: int = 200) -> dict:
    safe_domain = _safe_domain(domain)
    safe_kind = _check_log_kind(kind)
    safe_lines = _check_tail_lines(lines)
    path = _log_path(safe_domain, safe_kind)
    if safe_kind == "error":
        # The Error tab merges PHP's error_log (application errors) with the OLS
        # server error log -- see the helper's read_site_log.
        php_log = _php_error_log_path(safe_domain)
        display_path = f"{php_log.as_posix()} + {path.as_posix()}"
        fallback = ["bash", "-lc",
                    f"tail -n {safe_lines} {php_log.as_posix()} {path.as_posix()} 2>/dev/null || true"]
    else:
        display_path = str(path)
        fallback = ["tail", "-n", str(safe_lines), str(path)]
    result = shell.privileged(
        "site-log-read",
        helper_args=[safe_domain, safe_kind, str(safe_lines)],
        check=False,
        fallback=fallback,
    )
    missing = "opanel_ent_LOG_MISSING=1" in (result.stderr or "")
    if result.returncode != 0 and not missing:
        raise RuntimeError((result.stderr or result.stdout or "Cannot read log file").strip())
    return {
        "domain": safe_domain,
        "kind": safe_kind,
        "path": display_path,
        "lines": safe_lines,
        "content": result.stdout or "",
        "exists": not missing,
    }


# ---------------------------------------------------------------------------
# Cache management
# ---------------------------------------------------------------------------
def clear_cache(domain: str | None = None) -> None:
    """Clear LSCache for a domain or all domains."""
    if domain:
        safe_domain = _safe_domain(domain)
        cache_dir = Path(f"/tmp/lscache/{safe_domain}")
        if cache_dir.exists():
            import shutil
            shutil.rmtree(cache_dir)
    else:
        # Clear all OPanel Enterprise-managed cache
        cache_root = Path("/tmp/lscache")
        if cache_root.exists():
            import shutil
            shutil.rmtree(cache_root)
            cache_root.mkdir(parents=True, exist_ok=True)
