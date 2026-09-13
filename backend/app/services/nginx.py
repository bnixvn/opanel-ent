import hashlib
import json
import math
import re
import tempfile
from pathlib import Path
from typing import Optional

from jinja2 import Environment, FileSystemLoader

from app.core.config import settings
from app.services import site_users
from app.services.shell import shell

TEMPLATE_DIR = Path(__file__).resolve().parent.parent / "templates" / "nginx"
CUSTOM_INCLUDE_DIR = Path("/etc/nginx/opanel-ent/custom")

ALLOWED_PHP_VERSIONS = {"5.6", "7.4", "8.0", "8.1", "8.2", "8.3", "8.4", "8.5"}
ALLOWED_APP_TYPES = {"wordpress", "php", "static"}
ALLOWED_REWRITE_MODES = {"none", "front_controller", "laravel", "codeigniter", "seohburl"}
ALLOWED_LOG_KINDS = {"access", "error"}
DOMAIN_RE = re.compile(r"[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?(?:\.[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?)+")
MAX_FULL_CONFIG_BYTES = 128 * 1024
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
WORDPRESS_CSP_HEADER = f'    add_header Content-Security-Policy "{WORDPRESS_CSP}" always;'


def waf_rules_file(domain: str) -> str:
    safe_domain = _safe_domain(domain)
    return f"/etc/nginx/modsec/sites/{safe_domain}.conf"


def _waf_block(domain: str) -> str:
    return f"""    # OPanel Enterprise WAF BEGIN
    modsecurity on;
    modsecurity_rules_file {waf_rules_file(domain)};
    # OPanel Enterprise WAF END"""


FASTCGI_CACHE_SERVER_BLOCK = """    # OPanel Enterprise FASTCGI CACHE SERVER BEGIN
    set $opanel_ent_skip_cache 0;
    if ($request_method = POST) { set $opanel_ent_skip_cache 1; }
    if ($query_string != "") { set $opanel_ent_skip_cache 1; }
    if ($http_cache_control ~* "no-cache|no-store|max-age=0") { set $opanel_ent_skip_cache 1; }
    if ($http_pragma = "no-cache") { set $opanel_ent_skip_cache 1; }
    if ($request_uri ~* "/wp-admin/|/wp-login.php|/xmlrpc.php|wp-.*.php|/feed/|sitemap(_index)?\\.xml") { set $opanel_ent_skip_cache 1; }
    if ($http_cookie ~* "comment_author|wordpress_[a-f0-9]+|wordpress_logged_in|wp-postpass|woocommerce_items_in_cart|woocommerce_cart_hash|wp_woocommerce_session|edd_items_in_cart") { set $opanel_ent_skip_cache 1; }
    add_header X-FastCGI-Cache $upstream_cache_status always;
    # OPanel Enterprise FASTCGI CACHE SERVER END"""
FASTCGI_CACHE_LOCATION_BLOCK = """        # OPanel Enterprise FASTCGI CACHE LOCATION BEGIN
        fastcgi_cache opanel_ent_FASTCGI;
        fastcgi_cache_methods GET HEAD;
        fastcgi_cache_valid 200 15s;
        fastcgi_cache_min_uses 2;
        fastcgi_cache_bypass $opanel_ent_skip_cache;
        fastcgi_no_cache $opanel_ent_skip_cache;
        fastcgi_no_cache $upstream_http_set_cookie;
        fastcgi_cache_lock on;
        # OPanel Enterprise FASTCGI CACHE LOCATION END"""

# Block-opening directives that nest scopes; matched against the original
# text so the trailing ``{`` is preserved.
DANGEROUS_BLOCKS = re.compile(
    r"(?mi)(?:^|[;{}\s])\s*("
    r"server\s*\{|"
    r"http\s*\{|"
    r"events\s*\{|"
    r"stream\s*\{|"
    r"upstream\s+[@A-Za-z0-9_][A-Za-z0-9_]*\s*\{"
    r")"
)

# Single-line directives. Matched against text where ``{``, ``}``, and ``;``
# are turned into newlines so directives written on the same physical line
# as their enclosing block are still seen by the line-start anchor.
DANGEROUS_DIRECTIVES = re.compile(
    r"(?mi)^\s*("
    r"include\s+|"            # arbitrary file inclusion
    r"load_module\b|"         # load shared object
    r"user\s+|"               # change worker UID
    r"daemon\s+|"
    r"pid\s+|"
    r"working_directory\b|"
    r"lua_|"                  # ngx_lua
    r"perl_|"                 # ngx_http_perl
    r"js_|"                   # njs scripting
    r"pcre_jit\b|"
    # ---- routing / upstream subversion ----
    r"proxy_pass\b|"
    r"fastcgi_pass\b|"
    r"uwsgi_pass\b|"
    r"scgi_pass\b|"
    r"grpc_pass\b|"
    # ---- arbitrary file read / serve ----
    r"alias\s+|"
    r"root\s+|"
    r"auth_basic_user_file\b|"
    # ---- arbitrary file write via logging ----
    r"access_log\s+|"
    r"error_log\s+|"
    # ---- HTTP response control / phishing primitives ----
    r"return\s+|"             # forced redirects, body injection
    r"error_page\s+|"         # remap error responses to attacker URI
    r"more_set_headers\b|"
    r"more_clear_headers\b|"
    r"auth_request\b|"        # delegate auth to attacker endpoint
    r"sub_filter\b|"          # rewrite response body
    r"sub_filter_once\b|"
    r"addition_before_body\b|"
    r"addition_after_body\b|"
    # ---- cert path override ----
    r"ssl_certificate\b|"
    r"ssl_certificate_key\b|"
    r"ssl_trusted_certificate\b"
    r")"
)

CACHE_HEADER_NAMES = frozenset(
    {
        "cache-control",
        "cdn-cache-control",
        "expires",
        "pragma",
        "surrogate-control",
        "vary",
        "x-cache",
        "x-cache-status",
        "x-fastcgi-cache",
    }
)
ADD_HEADER_NAME_RE = re.compile(
    r"(?mi)(?:^|[;{}])\s*add_header\s+"
    r"([!#$%&'*+\-.^_`|~0-9A-Za-z]+)(?=\s)"
)
TRY_FILES_RE = re.compile(r"(?mi)^\s*try_files\s+([^;{}]+);")


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
        raise ValueError(f"Unsupported nginx rewrite mode: {mode}")
    return value


def _effective_document_root(document_root: str, rewrite_mode: str) -> str:
    safe_root = site_users.validate_document_root(document_root)
    if rewrite_mode in {"laravel", "codeigniter"} and safe_root.rstrip("/") == "public_html":
        return "public_html/public"
    return safe_root


def _vhost_path(domain: str) -> Path:
    safe_domain = (domain or "").lower()
    if not DOMAIN_RE.fullmatch(safe_domain):
        raise ValueError("Invalid domain")
    return Path(settings.nginx_sites_available) / f"{safe_domain}.conf"


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


def _server_names(domain: str, aliases: list[str] | tuple[str, ...] | None = None) -> list[str]:
    safe_domain = _safe_domain(domain)
    names = [safe_domain, f"www.{safe_domain}"]
    seen = set(names)
    for alias in _safe_alias_domains(aliases):
        if alias == safe_domain or alias in seen:
            continue
        names.append(alias)
        seen.add(alias)
    return names


def _redirect_vhost_blocks(
    domain: str,
    redirects: list[str] | tuple[str, ...] | None = None,
    ssl_cert_path: str | None = None,
    ssl_key_path: str | None = None,
    ssl_ca_path: str | None = None,
) -> str:
    safe_domain = _safe_domain(domain)
    reserved = {safe_domain, f"www.{safe_domain}"}
    cert = key = ca = None
    if ssl_cert_path or ssl_key_path:
        cert, key, ca = _manual_ssl_paths(ssl_cert_path, ssl_key_path, ssl_ca_path)
        if ca:
            cert = cert.rsplit("/", 1)[0] + "/fullchain.crt"
    blocks: list[str] = []
    for redirect_domain in _safe_alias_domains(redirects):
        if redirect_domain in reserved:
            continue
        block_lines = [
            "server {",
            "    listen 80;",
            f"    server_name {redirect_domain};",
            "",
            "    # OPanel Enterprise ACME CHALLENGE",
            "    location ^~ /.well-known/acme-challenge/ {",
            "        root /var/www/opanel-ent-acme;",
            "        default_type text/plain;",
            "        try_files $uri =404;",
            "        access_log off;",
            "        auth_basic off;",
            "    }",
            "",
            f"    return 301 https://{safe_domain}$request_uri;",
            "}",
        ]
        if cert and key:
            block_lines.extend([
                "",
                "server {",
                "    listen 443 ssl http2;",
                f"    server_name {redirect_domain};",
                f"    ssl_certificate {cert};",
                f"    ssl_certificate_key {key};",
                "    ssl_protocols TLSv1.2 TLSv1.3;",
                "    ssl_prefer_server_ciphers off;",
            ])
            if ca:
                block_lines.append(f"    ssl_trusted_certificate {ca};")
            block_lines.extend([
                f"    return 301 https://{safe_domain}$request_uri;",
                "}",
            ])
        blocks.append("\n".join(block_lines))
    return "\n\n".join(blocks)


def _certbot_ssl_lines(content: str) -> list[str]:
    lines: list[str] = []
    seen: set[str] = set()
    for line in content.splitlines():
        stripped = line.strip()
        if (
            "ssl_certificate" in line
            or "include /etc/letsencrypt/options-ssl-nginx.conf" in line
            or "ssl_dhparam /etc/letsencrypt/ssl-dhparams.pem" in line
        ) and stripped not in seen:
            lines.append(line)
            seen.add(stripped)
    return lines


def _append_certbot_redirect_vhosts(content: str, domain: str, redirects: list[str] | tuple[str, ...] | None = None) -> str:
    safe_domain = _safe_domain(domain)
    reserved = {safe_domain, f"www.{safe_domain}"}
    ssl_lines = _certbot_ssl_lines(content)
    blocks: list[str] = []
    for redirect_domain in _safe_alias_domains(redirects):
        if redirect_domain in reserved:
            continue
        block_lines = [
            "server {",
            "    listen 80;",
            f"    server_name {redirect_domain};",
            "",
            "    # OPanel Enterprise ACME CHALLENGE",
            "    location ^~ /.well-known/acme-challenge/ {",
            "        root /var/www/opanel-ent-acme;",
            "        default_type text/plain;",
            "        try_files $uri =404;",
            "        access_log off;",
            "        auth_basic off;",
            "    }",
            "",
            f"    return 301 https://{safe_domain}$request_uri;",
            "}",
        ]
        if ssl_lines:
            block_lines.extend([
                "",
                "server {",
                "    listen 443 ssl http2;",
                f"    server_name {redirect_domain};",
                *ssl_lines,
                f"    return 301 https://{safe_domain}$request_uri;",
                "}",
            ])
        blocks.append("\n".join(block_lines))
    if not blocks:
        return content
    return content.rstrip() + "\n\n" + "\n\n".join(blocks) + "\n"


def _append_redirect_vhosts(
    content: str,
    domain: str,
    redirects: list[str] | tuple[str, ...] | None = None,
    ssl_cert_path: str | None = None,
    ssl_key_path: str | None = None,
    ssl_ca_path: str | None = None,
) -> str:
    redirect_blocks = _redirect_vhost_blocks(domain, redirects, ssl_cert_path, ssl_key_path, ssl_ca_path)
    if not redirect_blocks:
        return content
    return content.rstrip() + "\n\n" + redirect_blocks + "\n"


def _custom_include_path(domain: str) -> Path:
    return CUSTOM_INCLUDE_DIR / f"{_safe_domain(domain)}.conf"


def custom_include_path(domain: str) -> str:
    return f"/etc/nginx/opanel-ent/custom/{_safe_domain(domain)}.conf"


def _custom_include_block(domain: str) -> str:
    return f"    # OPanel Enterprise CUSTOM INCLUDE\n    include {custom_include_path(domain)};"


def _ensure_custom_include_position(content: str, domain: str) -> str:
    block = _custom_include_block(domain)
    include_path = re.escape(custom_include_path(domain))
    existing_block = re.compile(
        rf"\n?[ \t]*# OPanel Enterprise CUSTOM INCLUDE[ \t]*\n"
        rf"[ \t]*include[ \t]+{include_path};[ \t]*\n?",
        re.MULTILINE,
    )
    cleaned = existing_block.sub("\n", content)
    main_location_pattern = re.compile(
        r"\n    location / \{\n"
        r"(?:        [^\n]*\n)*"
        r"    \}\n"
    )
    main_location_match = main_location_pattern.search(cleaned)
    main_location = ""
    if main_location_match:
        main_location = main_location_match.group(0).strip("\n")
        cleaned = (
            cleaned[:main_location_match.start()].rstrip()
            + "\n"
            + cleaned[main_location_match.end():].lstrip("\n")
        )
    static_location = "\n    location ~* \\.(jpg|jpeg|gif|png|css|js|ico|webp|svg|woff|woff2|ttf|eot)$ {"
    insert_at = cleaned.find(static_location)
    if insert_at == -1:
        insert_at = cleaned.rfind("\n}")
    if insert_at != -1:
        before = cleaned[:insert_at].rstrip()
        after = cleaned[insert_at + 1:].lstrip("\n")
        main_block = f"\n\n{main_location}" if main_location else ""
        return f"{before}\n\n{block}{main_block}\n\n{after}"
    return cleaned.rstrip() + f"\n\n{block}\n"


def read_custom_include(domain: str) -> str:
    try:
        return _custom_include_path(domain).read_text(encoding="utf-8")
    except OSError:
        return ""


def _custom_include_snapshot(domain: str) -> tuple[bool, str]:
    target = _custom_include_path(domain)
    try:
        return True, target.read_text(encoding="utf-8")
    except FileNotFoundError:
        return False, ""


def _write_custom_include(domain: str, custom_directives: str) -> str:
    safe_domain = _safe_domain(domain)
    safe_custom = validate_custom_nginx(custom_directives)
    target = _custom_include_path(safe_domain)
    if settings.command_dry_run:
        return str(target)
    shell.privileged(
        "ols-custom-write",
        helper_args=[safe_domain],
        input=safe_custom,
        fallback=[
            "bash",
            "-lc",
            (
                "install -d -m 0775 /usr/local/lsws/conf/opanel-ent/custom && "
                f"cat > /usr/local/lsws/conf/opanel-ent/custom/{safe_domain}.conf && "
                f"chmod 0664 /usr/local/lsws/conf/opanel-ent/custom/{safe_domain}.conf"
            ),
        ],
    )
    return str(target)


def _delete_custom_include(domain: str) -> None:
    safe_domain = _safe_domain(domain)
    if settings.command_dry_run:
        return
    shell.privileged(
        "ols-custom-delete",
        helper_args=[safe_domain],
        fallback=["rm", "-f", str(_custom_include_path(safe_domain))],
    )


def _restore_custom_include(domain: str, snapshot: tuple[bool, str]) -> None:
    existed, content = snapshot
    if existed:
        _write_custom_include(domain, content)
    else:
        _delete_custom_include(domain)


def _check_log_kind(kind: str) -> str:
    if kind not in ALLOWED_LOG_KINDS:
        raise ValueError("Log kind must be access or error")
    return kind


def _check_tail_lines(lines: int) -> int:
    try:
        value = int(lines)
    except (TypeError, ValueError) as exc:
        raise ValueError("Log lines must be a number") from exc
    if value < 1 or value > 5000:
        raise ValueError("Log lines must be between 1 and 5000")
    return value


def _log_path(domain: str, kind: str) -> Path:
    safe_domain = _safe_domain(domain)
    safe_kind = _check_log_kind(kind)
    return Path("/var/log/nginx") / f"{safe_domain}.{safe_kind}.log"


def validate_custom_nginx(content: Optional[str]) -> str:
    """Sanitize and validate a custom nginx block before rendering it inside a server { } scope."""
    if not content:
        return ""
    text = content.replace("\r\n", "\n").strip()
    if len(text) > 16 * 1024:
        raise ValueError("Custom nginx block is too large")
    # Balanced braces only (allow nested blocks inside server scope).
    depth = 0
    for ch in text:
        if ch == "{":
            depth += 1
        elif ch == "}":
            depth -= 1
            if depth < 0:
                raise ValueError("Unbalanced braces in custom nginx block")
    if depth != 0:
        raise ValueError("Unbalanced braces in custom nginx block")
    if DANGEROUS_BLOCKS.search(text):
        raise ValueError(
            "Custom block must not nest server/http/events/stream/upstream blocks"
        )
    # Normalize so the line-anchored deny-list also catches directives
    # written on the same line as their enclosing block, e.g.
    # ``location /api { proxy_pass http://attacker; }`` would otherwise
    # slip past a plain ``^\s*proxy_pass`` match.
    normalized = re.sub(r"[{};]", "\n", text)
    if DANGEROUS_DIRECTIVES.search(normalized):
        raise ValueError(
            "Custom block must not contain disallowed directives (proxy_pass, alias, "
            "return, ssl_certificate, ...)"
        )
    add_header_count = len(re.findall(r"(?mi)(?:^|[;{}])\s*add_header\b", text))
    add_header_names = ADD_HEADER_NAME_RE.findall(text)
    if add_header_count != len(add_header_names):
        raise ValueError("Malformed or unsupported add_header directive")
    unsupported_headers = sorted(
        {
            header
            for header in add_header_names
            if header.lower() not in CACHE_HEADER_NAMES
        },
        key=str.lower,
    )
    if unsupported_headers:
        raise ValueError(
            "Custom add_header may only set cache-related headers; disallowed: "
            + ", ".join(unsupported_headers)
        )
    try_files_matches = list(TRY_FILES_RE.finditer(text))
    for match in try_files_matches:
        arguments = match.group(1).split()
        if not arguments or any(
            token.startswith("file:")
            or token.startswith("http:")
            or token.startswith("https:")
            or ".." in token.split("/")
            for token in arguments
        ):
            raise ValueError("Custom try_files directives must not use traversal or external URLs")
    try_files_count = len(re.findall(r"(?mi)(?:^|[;{}])\s*try_files\b", text))
    if try_files_count != len(try_files_matches):
        raise ValueError("Malformed or unsupported try_files directive")
    for match in re.finditer(r"(?mi)^\s*rewrite\s+\S+\s+(\S+)(?:\s+(\S+))?\s*;", text):
        replacement, flag = match.groups()
        if (
            not replacement.startswith("/")
            or replacement.startswith("//")
            or "://" in replacement
            or ".." in replacement.split("/")
            or (flag and flag not in {"last", "break"})
        ):
            raise ValueError("Custom rewrite directives must target a local URI and use last or break")
    rewrite_count = len(re.findall(r"(?mi)(?:^|[;{}])\s*rewrite\b", text))
    if rewrite_count != len(re.findall(r"(?mi)^\s*rewrite\s+\S+\s+\S+(?:\s+\S+)?\s*;", text)):
        raise ValueError("Malformed or unsupported rewrite directive")
    if "\x00" in text:
        raise ValueError("Custom nginx block contains a NUL byte")
    return text


def validate_full_nginx_config(content: Optional[str]) -> str:
    if content is None:
        raise ValueError("Nginx config is required")
    text = content.replace("\r\n", "\n").strip() + "\n"
    if len(text.encode("utf-8")) > MAX_FULL_CONFIG_BYTES:
        raise ValueError("Nginx config is too large")
    if "\x00" in text:
        raise ValueError("Nginx config contains a NUL byte")
    if "server" not in text or "{" not in text:
        raise ValueError("Nginx config must contain a server block")
    return text


def _has_ssl_config(content: str) -> bool:
    return "ssl_certificate" in content or "listen 443" in content


def _manual_ssl_paths(cert_path: str | None, key_path: str | None, ca_path: str | None = None) -> tuple[str, str, str | None]:
    if not cert_path or not key_path:
        raise ValueError("Manual SSL certificate and key paths are required")
    cert = str(cert_path).replace("\\", "/")
    key = str(key_path).replace("\\", "/")
    ca = str(ca_path).replace("\\", "/") if ca_path else None
    for value in (cert, key, ca):
        if not value:
            continue
        if "\x00" in value or "\n" in value or "\r" in value:
            raise ValueError("Manual SSL path contains unsafe characters")
        if not value.startswith("/etc/nginx/opanel-ent/ssl/sites/"):
            raise ValueError("Manual SSL paths must be under /etc/nginx/opanel-ent/ssl/sites")
    return cert, key, ca


def _write_backup(target: Path, content: str) -> Path:
    backup = target.with_suffix(target.suffix + ".bak")
    temp_path: Path | None = None
    try:
        with tempfile.NamedTemporaryFile(
            mode="w",
            encoding="utf-8",
            dir=target.parent,
            prefix=f".{target.name}.bak-",
            delete=False,
        ) as handle:
            handle.write(content)
            temp_path = Path(handle.name)
        temp_path.chmod(0o640)
        temp_path.replace(backup)
    finally:
        if temp_path is not None:
            temp_path.unlink(missing_ok=True)
    return backup


def _merge_certbot_ssl_config(new_content: str, existing_content: str) -> str:
    server_name = "_"
    if "server_name " in new_content:
        server_name = new_content.split("server_name ", 1)[1].split(";", 1)[0].strip()
    ssl_lines = []
    seen_ssl_lines = set()
    for line in existing_content.splitlines():
        if (
            "ssl_certificate" in line
            or "include /etc/letsencrypt/options-ssl-nginx.conf" in line
            or "ssl_dhparam /etc/letsencrypt/ssl-dhparams.pem" in line
        ) and line.strip() not in seen_ssl_lines:
            ssl_lines.append(line)
            seen_ssl_lines.add(line.strip())
    https_lines = []
    for line in new_content.splitlines():
        if "listen 80;" in line:
            https_lines.append(line.replace("listen 80;", "listen 443 ssl http2;"))
        else:
            https_lines.append(line)
        if "server_name" in line and ssl_lines:
            https_lines.extend(ssl_lines)
    redirect_block = "\n".join([
        "server {",
        "    listen 80;",
        f"    server_name {server_name};",
        "    return 301 https://$host$request_uri;",
        "}",
        "",
    ])
    return redirect_block + "\n".join(https_lines) + "\n"


def apply_manual_ssl_config(new_content: str, cert_path: str, key_path: str, ca_path: str | None = None) -> str:
    cert, key, ca = _manual_ssl_paths(cert_path, key_path, ca_path)
    fullchain = cert.rsplit("/", 1)[0] + "/fullchain.crt"
    server_name = "_"
    if "server_name " in new_content:
        server_name = new_content.split("server_name ", 1)[1].split(";", 1)[0].strip()
    ssl_lines = [
        f"    ssl_certificate {fullchain if ca else cert};",
        f"    ssl_certificate_key {key};",
        "    ssl_protocols TLSv1.2 TLSv1.3;",
        "    ssl_prefer_server_ciphers off;",
    ]
    if ca:
        ssl_lines.append(f"    ssl_trusted_certificate {ca};")
    https_lines = []
    inserted = False
    for line in new_content.splitlines():
        if "listen 80;" in line:
            https_lines.append(line.replace("listen 80;", "listen 443 ssl http2;"))
        else:
            https_lines.append(line)
        if not inserted and "server_name" in line:
            https_lines.extend(ssl_lines)
            inserted = True
    redirect_block = "\n".join([
        "server {",
        "    listen 80;",
        f"    server_name {server_name};",
        "    return 301 https://$host$request_uri;",
        "}",
        "",
    ])
    return redirect_block + "\n".join(https_lines) + "\n"


def _ensure_hsts_header(content: str) -> str:
    security_headers = [
        ('Strict-Transport-Security', '    add_header Strict-Transport-Security "max-age=31536000; includeSubDomains" always;'),
        ('Permissions-Policy', '    add_header Permissions-Policy "camera=(), microphone=(), geolocation=(), payment=(), usb=(), bluetooth=(), magnetometer=(), gyroscope=(), accelerometer=()" always;'),
        ('Content-Security-Policy', WORDPRESS_CSP_HEADER),
    ]
    headers_to_add = [header for name, header in security_headers if name not in content]
    if not headers_to_add:
        return content
    marker = '    add_header X-XSS-Protection "1; mode=block" always;'
    if marker in content:
        return content.replace(marker, f"{marker}\n" + "\n".join(headers_to_add), 1)
    server_marker = "    server_tokens off;"
    if server_marker in content:
        return content.replace(server_marker, f"{server_marker}\n" + "\n".join(headers_to_add), 1)
    return content


def _php_fpm_socket(php_version: str | None = None) -> str:
    version = _check_php_version(php_version) or settings.default_php_version
    if version not in ALLOWED_PHP_VERSIONS:
        raise ValueError(f"Unsupported PHP version: {version}")
    return f"/run/php/php{version}-fpm.sock"


def _replace_php_fpm_socket(content: str, php_version: str) -> str:
    _check_php_version(php_version)
    return re.sub(
        r"fastcgi_pass\s+unix:/run/php/php[0-9.]+-fpm\.sock;",
        f"fastcgi_pass unix:{_php_fpm_socket(php_version)};",
        content,
    )


def _replace_fastcgi_socket(content: str, socket_path: str) -> str:
    if not re.fullmatch(r"/run/php/[A-Za-z0-9_.-]+\.sock", socket_path or ""):
        raise ValueError("Invalid PHP-FPM socket path")
    return re.sub(
        r"fastcgi_pass\s+unix:[^;]+;",
        f"fastcgi_pass unix:{socket_path};",
        content,
    )


def _domain_from_vhost(content: str) -> str:
    match = re.search(r"(?m)^\s*server_name\s+([^;]+);", content or "")
    if not match:
        return "example.com"
    first = match.group(1).split()[0]
    if first.startswith("www."):
        first = first[4:]
    try:
        return _safe_domain(first)
    except ValueError:
        return "example.com"


def _replace_waf_block(content: str, enabled: bool, domain: str | None = None) -> str:
    pattern = re.compile(
        r"\n?    # OPanel Enterprise WAF BEGIN\n.*?\n    # OPanel Enterprise WAF END",
        re.DOTALL,
    )
    cleaned = pattern.sub("", content)
    if not enabled:
        return cleaned.rstrip() + "\n"
    block = _waf_block(domain or _domain_from_vhost(cleaned))
    if "    server_tokens off;" in cleaned:
        return cleaned.replace("    server_tokens off;", f"    server_tokens off;\n{block}", 1)
    if "    server_name " in cleaned:
        return re.sub(r"(    server_name [^;]+;)", f"\\1\n{block}", cleaned, count=1)
    match = re.search(r"server\s*\{", cleaned)
    if match:
        insert_at = match.end()
        return cleaned[:insert_at] + "\n" + block + cleaned[insert_at:]
    raise ValueError("Cannot find server block for WAF directives")


def _replace_fastcgi_cache_blocks(content: str, enabled: bool = True) -> str:
    server_pattern = re.compile(
        r"\n?    # OPanel Enterprise FASTCGI CACHE SERVER BEGIN\n.*?\n    # OPanel Enterprise FASTCGI CACHE SERVER END",
        re.DOTALL,
    )
    location_pattern = re.compile(
        r"\n?        # OPanel Enterprise FASTCGI CACHE LOCATION BEGIN\n.*?\n        # OPanel Enterprise FASTCGI CACHE LOCATION END",
        re.DOTALL,
    )
    cleaned = server_pattern.sub("", content)
    cleaned = location_pattern.sub("", cleaned)
    cleaned = re.sub(r"\n?\s*add_header\s+X-FastCGI-Cache\s+[^;]+;", "", cleaned)
    if not enabled:
        return cleaned.rstrip() + "\n"
    if "fastcgi_pass" not in cleaned:
        raise ValueError("Cannot find a PHP FastCGI location")

    if "    client_max_body_size " in cleaned:
        cleaned = re.sub(
            r"(    client_max_body_size [^;]+;)",
            lambda match: f"{match.group(1)}\n\n{FASTCGI_CACHE_SERVER_BLOCK}",
            cleaned,
            count=1,
        )
    elif "    server_tokens off;" in cleaned:
        cleaned = cleaned.replace("    server_tokens off;", f"    server_tokens off;\n\n{FASTCGI_CACHE_SERVER_BLOCK}", 1)
    else:
        cleaned = re.sub(
            r"(    server_name [^;]+;)",
            lambda match: f"{match.group(1)}\n\n{FASTCGI_CACHE_SERVER_BLOCK}",
            cleaned,
            count=1,
        )

    if re.search(r"fastcgi_read_timeout\s+[^;]+;", cleaned):
        return re.sub(
            r"(        fastcgi_read_timeout\s+[^;]+;)",
            lambda match: f"{match.group(1)}\n{FASTCGI_CACHE_LOCATION_BLOCK}",
            cleaned,
            count=1,
        )
    return re.sub(
        r"(        fastcgi_pass\s+[^;]+;)",
        lambda match: f"{match.group(1)}\n{FASTCGI_CACHE_LOCATION_BLOCK}",
        cleaned,
        count=1,
    )


def _test_and_reload(
    target: Path,
    old_content: Optional[str],
    custom_domain: Optional[str] = None,
    custom_snapshot: Optional[tuple[bool, str]] = None,
) -> None:
    test = shell.privileged("ols-test", check=False, fallback=["/usr/local/lsws/bin/lswsctrl", "restart"])
    if test.returncode != 0:
        if old_content is not None:
            target.write_text(old_content, encoding="utf-8")
        else:
            target.unlink(missing_ok=True)
        if custom_domain is not None and custom_snapshot is not None:
            _restore_custom_include(custom_domain, custom_snapshot)
        raise RuntimeError((test.stderr or test.stdout or "OpenLiteSpeed config test failed").strip())
    shell.privileged("ols-reload", fallback=["/usr/local/lsws/bin/lswsctrl", "restart"])


def render_vhost(
    domain: str,
    root_path: str,
    app_type: str = "wordpress",
    php_version: Optional[str] = None,
    custom_directives: str = "",
    php_fpm_socket_override: Optional[str] = None,
    waf_enabled: bool = True,
    document_root: str = "public_html",
    rewrite_mode: str | None = None,
    ssl_cert_path: str | None = None,
    ssl_key_path: str | None = None,
    ssl_ca_path: str | None = None,
    aliases: list[str] | tuple[str, ...] | None = None,
    redirects: list[str] | tuple[str, ...] | None = None,
) -> str:
    server_names = _server_names(domain, aliases)
    safe_domain = server_names[0]
    _check_app_type(app_type)
    safe_rewrite_mode = _check_rewrite_mode(rewrite_mode)
    _check_php_version(php_version)
    resolved_root = Path(root_path).resolve()
    if not site_users.is_site_root_for_domain(resolved_root, safe_domain):
        raise ValueError("root_path must be the managed root for this domain")
    validate_custom_nginx(custom_directives)
    effective_document_root = _effective_document_root(document_root, safe_rewrite_mode)
    resolved_document_root = site_users.document_root(resolved_root, effective_document_root)
    include_path = custom_include_path(safe_domain)

    env = Environment(loader=FileSystemLoader(TEMPLATE_DIR), autoescape=False)
    template_name = {
        "wordpress": "wordpress.conf.j2",
        "php": "php.conf.j2",
        "static": "static.conf.j2",
    }[app_type]
    template = env.get_template(template_name)
    php_fpm_socket = php_fpm_socket_override or _php_fpm_socket(php_version)

    rendered = template.render(
        domain=safe_domain,
        server_names=server_names,
        root_path=str(resolved_root),
        document_root_path=str(resolved_document_root),
        php_fpm_socket=php_fpm_socket,
        custom_include_path=include_path,
        waf_enabled=bool(waf_enabled),
        waf_rules_file=waf_rules_file(safe_domain),
        rewrite_mode=safe_rewrite_mode,
    )
    if ssl_cert_path or ssl_key_path:
        rendered = apply_manual_ssl_config(rendered, ssl_cert_path or "", ssl_key_path or "", ssl_ca_path)
    return _append_redirect_vhosts(rendered, domain, redirects, ssl_cert_path, ssl_key_path, ssl_ca_path)


# Back-compat shim for older imports.
def render_wordpress_vhost(domain: str, root_path: str, php_version: Optional[str] = None) -> str:
    return render_vhost(domain, root_path, app_type="wordpress", php_version=php_version)


def write_vhost(
    domain: str,
    root_path: str,
    app_type: str = "wordpress",
    php_version: Optional[str] = None,
    custom_directives: str = "",
    php_fpm_socket_override: Optional[str] = None,
    waf_enabled: bool = True,
    document_root: str = "public_html",
    rewrite_mode: str | None = None,
    ssl_cert_path: str | None = None,
    ssl_key_path: str | None = None,
    ssl_ca_path: str | None = None,
    preserve_existing_ssl: bool = True,
    aliases: list[str] | tuple[str, ...] | None = None,
    redirects: list[str] | tuple[str, ...] | None = None,
) -> str:
    return rewrite_vhost(
        domain,
        root_path,
        app_type=app_type,
        php_version=php_version,
        custom_directives=custom_directives,
        php_fpm_socket_override=php_fpm_socket_override,
        waf_enabled=waf_enabled,
        document_root=document_root,
        rewrite_mode=rewrite_mode,
        ssl_cert_path=ssl_cert_path,
        ssl_key_path=ssl_key_path,
        ssl_ca_path=ssl_ca_path,
        preserve_existing_ssl=preserve_existing_ssl,
        aliases=aliases,
        redirects=redirects,
    )


def write_wordpress_vhost(domain: str, root_path: str, php_version: Optional[str] = None) -> str:
    return write_vhost(domain, root_path, app_type="wordpress", php_version=php_version)


def rewrite_vhost(
    domain: str,
    root_path: str,
    app_type: str,
    php_version: Optional[str],
    custom_directives: str = "",
    php_fpm_socket_override: Optional[str] = None,
    waf_enabled: bool = True,
    document_root: str = "public_html",
    rewrite_mode: str | None = None,
    ssl_cert_path: str | None = None,
    ssl_key_path: str | None = None,
    ssl_ca_path: str | None = None,
    preserve_existing_ssl: bool = True,
    aliases: list[str] | tuple[str, ...] | None = None,
    redirects: list[str] | tuple[str, ...] | None = None,
) -> str:
    target = _vhost_path(domain)
    content = render_vhost(
        domain,
        root_path,
        document_root=document_root,
        app_type=app_type,
        php_version=php_version,
        custom_directives=custom_directives,
        php_fpm_socket_override=php_fpm_socket_override,
        waf_enabled=waf_enabled,
        rewrite_mode=rewrite_mode,
        ssl_cert_path=ssl_cert_path,
        ssl_key_path=ssl_key_path,
        ssl_ca_path=ssl_ca_path,
        aliases=aliases,
        redirects=None,
    )
    if settings.command_dry_run:
        return _append_redirect_vhosts(content, domain, redirects, ssl_cert_path, ssl_key_path, ssl_ca_path)
    custom_snapshot = _custom_include_snapshot(domain)
    _write_custom_include(domain, custom_directives)
    if target.exists():
        existing = target.read_text(encoding="utf-8")
        _write_backup(target, existing)
        if preserve_existing_ssl and _has_ssl_config(existing) and not (ssl_cert_path and ssl_key_path):
            content = _merge_certbot_ssl_config(content, existing)
    if preserve_existing_ssl and not (ssl_cert_path and ssl_key_path):
        content = _append_certbot_redirect_vhosts(content, domain, redirects)
    else:
        content = _append_redirect_vhosts(content, domain, redirects, ssl_cert_path, ssl_key_path, ssl_ca_path)
    old_content = target.read_text(encoding="utf-8") if target.exists() else None
    target.write_text(content, encoding="utf-8")
    _test_and_reload(target, old_content, domain, custom_snapshot)
    return str(target)


def rewrite_wordpress_vhost(domain: str, root_path: str, php_version: str) -> str:
    return rewrite_vhost(domain, root_path, app_type="wordpress", php_version=php_version)


def update_custom_block(domain: str, custom_directives: str) -> str:
    target = _vhost_path(domain)
    safe_custom = validate_custom_nginx(custom_directives)
    if settings.command_dry_run:
        return safe_custom
    if not target.exists():
        raise FileNotFoundError(str(target))
    existing = target.read_text(encoding="utf-8")
    positioned = _ensure_custom_include_position(existing, domain)
    custom_snapshot = _custom_include_snapshot(domain)
    _write_custom_include(domain, safe_custom)
    if positioned != existing:
        target.write_text(positioned, encoding="utf-8")
    _test_and_reload(target, existing, domain, custom_snapshot)
    return custom_include_path(domain)


def set_php_version(
    domain: str,
    php_version: str,
    php_fpm_socket_override: Optional[str] = None,
) -> str:
    target = _vhost_path(domain)
    socket_path = php_fpm_socket_override or _php_fpm_socket(php_version)
    _check_php_version(php_version)
    if settings.command_dry_run:
        return socket_path
    if not target.exists():
        raise FileNotFoundError(str(target))
    existing = target.read_text(encoding="utf-8")
    new_content = _replace_fastcgi_socket(existing, socket_path)
    if new_content == existing:
        if f"fastcgi_pass unix:{socket_path};" in existing:
            return str(target)
        raise ValueError("Cannot find a PHP FastCGI socket in the Nginx config")
    _write_backup(target, existing)
    target.write_text(new_content, encoding="utf-8")
    _test_and_reload(target, existing)
    return str(target)


def set_wordpress_php_version(domain: str, php_version: str) -> str:
    return set_php_version(domain, php_version)


def harden_existing_wordpress_vhost(
    domain: str,
    root_path: str,
    php_version: str | None = None,
    custom_directives: str = "",
    php_fpm_socket_override: Optional[str] = None,
    waf_enabled: bool = True,
    document_root: str = "public_html",
    rewrite_mode: str | None = None,
    ssl_cert_path: str | None = None,
    ssl_key_path: str | None = None,
    ssl_ca_path: str | None = None,
    aliases: list[str] | tuple[str, ...] | None = None,
    redirects: list[str] | tuple[str, ...] | None = None,
) -> str:
    return rewrite_vhost(
        domain,
        root_path,
        app_type="wordpress",
        php_version=php_version,
        custom_directives=custom_directives,
        php_fpm_socket_override=php_fpm_socket_override,
        waf_enabled=waf_enabled,
        document_root=document_root,
        rewrite_mode=rewrite_mode,
        ssl_cert_path=ssl_cert_path,
        ssl_key_path=ssl_key_path,
        ssl_ca_path=ssl_ca_path,
        aliases=aliases,
        redirects=redirects,
    )


def delete_wordpress_vhost(domain: str):
    target = _vhost_path(domain)
    if settings.command_dry_run:
        return str(target)
    target.unlink(missing_ok=True)
    _delete_custom_include(domain)
    shell.privileged("ols-reload", fallback=["/usr/local/lsws/bin/lswsctrl", "restart"])
    return str(target)


def read_vhost_config(domain: str) -> str:
    target = _vhost_path(domain)
    if not target.exists():
        raise FileNotFoundError(str(target))
    return target.read_text(encoding="utf-8")


def read_site_log(domain: str, kind: str = "access", lines: int = 200) -> dict:
    safe_domain = _safe_domain(domain)
    safe_kind = _check_log_kind(kind)
    safe_lines = _check_tail_lines(lines)
    path = _log_path(safe_domain, safe_kind)
    result = shell.privileged(
        "site-log-read",
        helper_args=[safe_domain, safe_kind, str(safe_lines)],
        check=False,
        fallback=["tail", "-n", str(safe_lines), str(path)],
    )
    missing = "opanel_ent_LOG_MISSING=1" in (result.stderr or "")
    if result.returncode != 0 and not missing:
        raise RuntimeError((result.stderr or result.stdout or "Cannot read log file").strip())
    return {
        "domain": safe_domain,
        "kind": safe_kind,
        "path": str(path),
        "lines": safe_lines,
        "content": result.stdout or "",
        "exists": not missing,
    }


def update_full_config(domain: str, content: str) -> str:
    target = _vhost_path(domain)
    safe_content = validate_full_nginx_config(content)
    if settings.command_dry_run:
        return safe_content
    if not target.exists():
        raise FileNotFoundError(str(target))
    existing = target.read_text(encoding="utf-8")
    _write_backup(target, existing)
    target.write_text(safe_content, encoding="utf-8")
    _test_and_reload(target, existing)
    return str(target)


def update_waf_block(domain: str, enabled: bool) -> str:
    target = _vhost_path(domain)
    if settings.command_dry_run:
        return _replace_waf_block("server {\n    server_name example.com;\n}\n", enabled, domain="example.com")
    if not target.exists():
        raise FileNotFoundError(str(target))
    existing = target.read_text(encoding="utf-8")
    new_content = _replace_waf_block(existing, enabled, domain)
    _write_backup(target, existing)
    target.write_text(new_content, encoding="utf-8")
    _test_and_reload(target, existing)
    return str(target)



def ensure_wordpress_fastcgi_cache(domain: str) -> str:
    target = _vhost_path(domain)
    if settings.command_dry_run:
        return _replace_fastcgi_cache_blocks(
            "server {\n    server_name example.com;\n    client_max_body_size 1100M;\n    location ~ \\.php$ {\n        fastcgi_pass unix:/run/php/php8.3-fpm.sock;\n        fastcgi_read_timeout 300;\n    }\n}\n",
            True,
        )
    if not target.exists():
        raise FileNotFoundError(str(target))
    existing = target.read_text(encoding="utf-8")
    new_content = _replace_fastcgi_cache_blocks(existing, True)
    if new_content == existing:
        return str(target)
    _write_backup(target, existing)
    target.write_text(new_content, encoding="utf-8")
    _test_and_reload(target, existing)
    return str(target)
