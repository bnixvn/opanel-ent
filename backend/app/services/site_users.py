import hashlib
import re
from pathlib import Path, PurePosixPath
from typing import Optional

from app.services.shell import shell


LINUX_USER_RE = re.compile(r"^[a-z_][a-z0-9_-]{2,31}$")
DOMAIN_RE = re.compile(r"^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?(?:\.[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?)+$")
PHP_VERSION_RE = re.compile(r"^(?:7\.4|8\.[1-5])$")
HOME_ROOT = Path("/home")
PUBLIC_DIR = "public_html"
RESERVED_LINUX_USERS = {
    "root", "daemon", "bin", "sys", "sync", "games", "man", "lp", "mail",
    "news", "uucp", "proxy", "www-data", "backup", "list", "irc", "_apt",
    "nobody", "OPanel Enterprise", "opanel-ent-sites", "opanel-ent-sftp",
    "opanel", "opanel-sites", "opanel-sftp", "mysql", "redis",
    "lsws", "lsadm",
}


def linux_user_for_domain(domain: str) -> str:
    """Return a deterministic, Linux-safe username for a website domain."""
    normalized = (domain or "").strip().lower()
    slug = re.sub(r"[^a-z0-9]+", "_", normalized).strip("_") or "site"
    digest = hashlib.sha256(normalized.encode("utf-8")).hexdigest()[:8]
    username = f"op_{slug[:18]}_{digest}"
    return username[:32]


def validate_linux_user(username: str) -> str:
    if not LINUX_USER_RE.fullmatch(username or "") or username in RESERVED_LINUX_USERS:
        raise ValueError("Invalid panel Linux user")
    return username


def linux_user_for_panel_username(username: str) -> str:
    return validate_linux_user((username or "").strip().lower())


def validate_php_version(php_version: str) -> str:
    if not PHP_VERSION_RE.fullmatch(php_version or ""):
        raise ValueError("Invalid PHP version")
    return php_version


def lsphp_app_name(username: str, php_version: str, root_path: str | Path | None = None) -> str:
    """Return the LSPHP external app name for a site user + PHP version."""
    safe_user = validate_linux_user(username)
    safe_version = validate_php_version(php_version).replace(".", "")
    if root_path is not None:
        resolved_root = str(Path(root_path).resolve())
        site_hash = hashlib.sha256(resolved_root.encode("utf-8")).hexdigest()[:12]
        return f"opanel-ent-{safe_user}-{site_hash}-lsphp{safe_version}"
    return f"opanel-ent-{safe_user}-lsphp{safe_version}"


# backward-compat alias
php_fpm_pool_name = lsphp_app_name


def lsphp_socket(
    username: Optional[str],
    php_version: Optional[str] = None,
    root_path: str | Path | None = None,
) -> Optional[str]:
    """Return the LSPHP Unix socket path for a site."""
    if not username:
        return None
    if php_version:
        return f"/tmp/lshttpd/{lsphp_app_name(username, php_version, root_path)}.sock"
    safe_user = validate_linux_user(username)
    return f"/tmp/lshttpd/opanel-ent-{safe_user}.sock"


# backward-compat alias
php_fpm_socket = lsphp_socket


def site_lsphp_socket(username: Optional[str], root_path: str | Path, php_version: Optional[str]) -> Optional[str]:
    return lsphp_socket(username, php_version, root_path) if php_version else None


# backward-compat alias
site_php_fpm_socket = site_lsphp_socket


def site_root_for_domain(domain: str) -> str:
    username = linux_user_for_domain(domain)
    return str(HOME_ROOT / username / domain.strip().lower())


def site_root_for_panel_user(username: str, domain: str) -> str:
    return str(HOME_ROOT / linux_user_for_panel_username(username) / domain.strip().lower())


def validate_document_root(value: str | None) -> str:
    cleaned = (value or PUBLIC_DIR).strip().replace("\\", "/")
    if cleaned.startswith("/") or re.match(r"^[A-Za-z]:/", cleaned):
        raise ValueError("document_root must be relative to the website root")
    cleaned = cleaned.strip("/")
    if not cleaned or len(cleaned) > 255:
        raise ValueError("document_root must be a relative path up to 255 characters")
    relative = PurePosixPath(cleaned)
    if relative.is_absolute():
        raise ValueError("document_root must be relative to the website root")
    parts = relative.parts
    if any(part in {"", ".", ".."} or not re.fullmatch(r"[A-Za-z0-9._-]+", part) for part in parts):
        raise ValueError("document_root must be a safe relative path such as public_html/public")
    return "/".join(parts)


def document_root(root_path: str | Path, relative_path: str = PUBLIC_DIR) -> Path:
    root = Path(root_path).resolve()
    target = (root / validate_document_root(relative_path)).resolve()
    try:
        target.relative_to(root)
    except ValueError as exc:
        raise ValueError("document_root must stay inside the website root") from exc
    return target


def is_managed_site_path(path: str | Path) -> bool:
    resolved = Path(path).resolve()
    home_root = HOME_ROOT.resolve()
    try:
        relative = resolved.relative_to(home_root)
    except ValueError:
        return False
    parts = relative.parts
    return len(parts) >= 2 and bool(LINUX_USER_RE.fullmatch(parts[0])) and parts[0] not in RESERVED_LINUX_USERS and bool(DOMAIN_RE.fullmatch(parts[1]))


def is_site_root_for_domain(path: str | Path, domain: str) -> bool:
    resolved = Path(path).resolve()
    home_root = HOME_ROOT.resolve()
    try:
        relative = resolved.relative_to(home_root)
    except ValueError:
        return False
    parts = relative.parts
    return (
        len(parts) == 2
        and bool(LINUX_USER_RE.fullmatch(parts[0]))
        and parts[0] not in RESERVED_LINUX_USERS
        and parts[1] == domain.strip().lower()
    )


def ensure_panel_user(username: str, password: Optional[str] = None) -> str:
    linux_user = linux_user_for_panel_username(username)
    shell.privileged(
        "panel-user-ensure",
        helper_args=[linux_user],
        fallback=["mkdir", "-p", str(HOME_ROOT / linux_user)],
    )
    if password is not None:
        set_panel_user_password(linux_user, password)
    return linux_user


def set_panel_user_password(username: str, password: str) -> None:
    linux_user = linux_user_for_panel_username(username)
    shell.privileged(
        "panel-user-password",
        helper_args=[linux_user],
        input=f"{password}\n",
        sensitive=True,
        fallback=["true"],
    )


def delete_panel_user(username: str) -> None:
    linux_user = linux_user_for_panel_username(username)
    shell.privileged(
        "panel-user-delete",
        helper_args=[linux_user],
        check=False,
        fallback=["true"],
    )


def ensure_site_runtime(domain: str, root_path: str, php_version: Optional[str] = None, linux_user: Optional[str] = None) -> str:
    username = validate_linux_user(linux_user) if linux_user else linux_user_for_domain(domain)
    helper_args = [username, root_path, php_version or "none"]
    shell.privileged(
        "site-runtime-ensure",
        helper_args=helper_args,
        fallback=["mkdir", "-p", str(document_root(root_path))],
    )
    return username


def ensure_document_root(root_path: str, relative_path: str, linux_user: Optional[str]) -> Path:
    target = document_root(root_path, relative_path)
    if not linux_user:
        target.mkdir(parents=True, exist_ok=True)
        return target
    username = validate_linux_user(linux_user)
    shell.privileged(
        "site-document-root-ensure",
        helper_args=[username, root_path, validate_document_root(relative_path)],
        fallback=["mkdir", "-p", str(target)],
    )
    return target


def import_site_files(root_path: str, relative_path: str, linux_user: str, staging_source: Path) -> None:
    """Copy an already-extracted, opanel-ent-owned staging directory into a
    site's document root.

    The document root is created chown'd to the site's own Linux user (see
    ensure_document_root), so the opanel-ent-api process has no write access to
    it — this goes through the same root-privileged trampoline used for
    website backup restores.
    """
    username = validate_linux_user(linux_user)
    shell.privileged(
        "site-import-copy",
        helper_args=[username, root_path, validate_document_root(relative_path), str(staging_source)],
        fallback=["cp", "-a", "--no-target-directory", str(staging_source), str(document_root(root_path, relative_path))],
    )


def move_site_runtime(old_root_path: str, new_root_path: str, linux_user: str, php_version: Optional[str] = None) -> str:
    username = validate_linux_user(linux_user)
    shell.privileged(
        "site-runtime-move",
        helper_args=[username, old_root_path, new_root_path, php_version or "none"],
        fallback=["mv", old_root_path, new_root_path],
    )
    return new_root_path


def site_user_from_path(path: str | Path) -> Optional[str]:
    """The owning Linux user of a managed site path (/home/<user>/<domain>/...)."""
    if not is_managed_site_path(path):
        return None
    return Path(path).resolve().relative_to(HOME_ROOT.resolve()).parts[0]


def fix_site_permissions(root_path: str, linux_user: Optional[str]) -> None:
    # Never fall back to www-data: giving one shared account ownership of a site
    # tree lets any other site's PHP reach these files.
    owner = validate_linux_user(linux_user) if linux_user else site_user_from_path(root_path)
    if not owner:
        raise ValueError(f"Cannot tell which site user owns {root_path}")
    shell.privileged(
        "fix-permissions",
        helper_args=[root_path, owner],
        check=False,
        fallback=["chown", "-R", f"{owner}:{owner}", root_path],
    )


def fix_site_path(path: str, linux_user: Optional[str], check: bool = False) -> None:
    if not linux_user:
        return
    shell.privileged(
        "site-path-fix",
        helper_args=[path, validate_linux_user(linux_user)],
        check=check,
        fallback=["chown", "-R", f"{linux_user}:{linux_user}", path],
    )


def delete_site_runtime(root_path: str, linux_user: Optional[str]) -> None:
    if not linux_user:
        return
    shell.privileged(
        "site-runtime-delete",
        helper_args=[validate_linux_user(linux_user), root_path],
        check=False,
        fallback=["true"],
    )
