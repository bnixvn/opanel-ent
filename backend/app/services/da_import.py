"""Import DirectAdmin user backups into OPanel Enterprise.

Supports .tar.gz, .tar.bz2, .tar.xz, .tar.zst, and plain .tar archives
uploaded to the DA backup directory (default: /home/admin/opanel-ent-backups/da).

The import logic is adapted from the standalone bpanel-directadmin-import
CLI tool, but runs in-process so it can be triggered from the web panel.
"""

from __future__ import annotations

import bz2
import datetime as _dt
import gzip
import hashlib
import logging
import os
import re
import secrets
import shutil
import subprocess
import tarfile
import tempfile
from pathlib import Path
from typing import Optional

from app.core.config import settings
from app.core.secrets import encrypt
from app.core.security import hash_password
from app.models.entities import DatabaseAccount, User, Website, WebsiteAlias
from app.services import mariadb, openlitespeed, site_users, waf

logger = logging.getLogger("opanel-ent.da_import")

# ---------------------------------------------------------------------------
# Constants
# ---------------------------------------------------------------------------
ARCHIVE_SUFFIXES = (".tar.zst", ".tar.gz", ".tgz", ".tar.bz2", ".tbz2", ".tar.xz", ".txz", ".tar")
SQL_SUFFIXES = (".sql.gz", ".sql.bz2", ".sql.zst", ".sql")
RESERVED_USERS = frozenset({
    "root", "daemon", "bin", "sys", "sync", "games", "man", "lp", "mail",
    "news", "uucp", "proxy", "www-data", "backup", "list", "irc", "_apt",
    "nobody", "opanel-ent", "opanel-ent-sites", "opanel-ent-sftp", "mysql", "redis",
    "lsws", "lsadm", "admin", "bpanel",
})
USER_RE = re.compile(r"^[a-z_][a-z0-9_-]{2,31}$")
DOMAIN_RE = re.compile(r"^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?(?:\.[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?)+$")
DB_RE = re.compile(r"^[a-z0-9_]{1,64}$")
SQL_DATABASE_DIRECTIVE_RE = re.compile(r"^\s*(CREATE|USE)\s+DATABASE\b", re.IGNORECASE)
SQL_DEFINER_RE = re.compile(r"DEFINER\s*=\s*[^ ]+\s+(?=\S)", re.IGNORECASE)
MAX_UPLOAD_BYTES = 1024 * 1024 * 1024  # 1 GB


class DAImportError(RuntimeError):
    """Raised when a DirectAdmin import step fails."""


# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

def _log(msg: str) -> None:
    logger.info("[da-import] %s", msg)
    print(f"[da-import] {msg}", flush=True)


def _strip_archive_suffix(name: str) -> str:
    lower = name.lower()
    for suffix in ARCHIVE_SUFFIXES:
        if lower.endswith(suffix):
            return name[:-len(suffix)]
    return Path(name).stem


def _safe_name(value: str, fallback: str = "backup") -> str:
    cleaned = re.sub(r"[^A-Za-z0-9_.-]+", "_", value).strip("._-")
    return (cleaned or fallback)[:80]


def _is_archive(path: Path) -> bool:
    name = path.name.lower()
    return path.is_file() and any(name.endswith(suffix) for suffix in ARCHIVE_SUFFIXES)


def _resolve_da_backup_path(backup_file: str) -> Path:
    """Resolve a DA backup filename or path inside the configured backup dir."""
    backup_dir = Path(settings.da_backup_dir).resolve()
    raw = Path(backup_file or "")
    if not raw.name:
        raise FileNotFoundError("Backup not found")

    if raw.is_absolute():
        path = raw.resolve()
    else:
        if raw.name != str(raw):
            raise FileNotFoundError("Backup not found")
        path = (backup_dir / raw.name).resolve()

    if backup_dir != path.parent:
        raise FileNotFoundError("Backup not found")
    if not path.exists() or not path.is_file() or not _is_archive(path):
        raise FileNotFoundError("Backup not found")
    return path


def _read_key_values(path: Path) -> dict[str, str]:
    values: dict[str, str] = {}
    if not path.exists() or not path.is_file():
        return values
    for raw in path.read_text(encoding="utf-8", errors="ignore").splitlines():
        line = raw.strip()
        if not line or line.startswith("#"):
            continue
        if "=" in line:
            key, value = line.split("=", 1)
        elif ":" in line:
            key, value = line.split(":", 1)
        else:
            continue
        values[key.strip().lower()] = value.strip().strip("'\"")
    return values


def _normalize_username(raw: str, archive_name: str = "") -> str:
    value = (raw or "").strip().lower()
    value = re.sub(r"[^a-z0-9_-]+", "_", value).strip("_-")
    if not value or not re.match(r"^[a-z_]", value):
        value = f"da_{value}" if value else "da_user"
    value = value[:32]
    if len(value) < 3:
        value = f"{value}_da"[:32]
    if value in RESERVED_USERS or not USER_RE.fullmatch(value):
        digest = hashlib.sha1((raw + archive_name).encode("utf-8")).hexdigest()[:8]
        stem = re.sub(r"[^a-z0-9_]+", "_", value).strip("_") or "user"
        value = f"da_{stem[:18]}_{digest}"[:32]
    return value


def _normalize_domain(value: str) -> Optional[str]:
    domain = (value or "").strip().lower().rstrip(".")
    return domain if DOMAIN_RE.fullmatch(domain) else None


def _normalize_db_identifier(raw: str, fallback: str, existing: set[str]) -> str:
    value = (raw or fallback).strip().lower()
    value = re.sub(r"[^a-z0-9_]+", "_", value).strip("_")
    if not value:
        value = fallback
    if not re.match(r"^[a-z0-9_]+$", value):
        value = re.sub(r"[^a-z0-9_]+", "_", value)
    value = value[:64]
    if value not in existing and DB_RE.fullmatch(value):
        return value
    digest = hashlib.sha1((raw + fallback).encode("utf-8")).hexdigest()[:6]
    stem = (value[:55].strip("_") or fallback[:55].strip("_") or "db")
    candidate = f"{stem}_{digest}"[:64]
    counter = 2
    while candidate in existing or not DB_RE.fullmatch(candidate):
        suffix = f"_{counter}"
        candidate = f"{stem[:64 - len(suffix)]}{suffix}"
        counter += 1
    return candidate


def _archive_username(name: str) -> str:
    base = _strip_archive_suffix(name)
    pieces = [piece for piece in re.split(r"[._-]+", base) if piece]
    if len(pieces) >= 3 and pieces[0] in {"user", "reseller"}:
        return pieces[-1]
    for piece in reversed(pieces):
        if piece.lower() not in {"backup", "user", "admin", "reseller"}:
            return piece
    return base


# ---------------------------------------------------------------------------
# Archive extraction
# ---------------------------------------------------------------------------

def _ensure_inside(base: Path, target: Path) -> Path:
    resolved = target.resolve()
    if not str(resolved).startswith(str(base.resolve())):
        raise DAImportError(f"Unsafe archive path: {target}")
    return resolved


def _safe_extract_tar(archive: Path, destination: Path) -> None:
    """Extract a tar archive safely, handling .tar.zst via zstd."""
    lower = archive.name.lower()
    proc = None

    if lower.endswith(".zst"):
        zstd = shutil.which("zstdcat") or shutil.which("zstd")
        if not zstd:
            raise DAImportError("zstd is required to extract .tar.zst backups. Install: apt install zstd")
        args = [zstd, str(archive)] if Path(zstd).name == "zstdcat" else [zstd, "-dc", str(archive)]
        proc = subprocess.Popen(args, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        source_fd = proc.stdout
    else:
        source_fd = None

    try:
        if source_fd is not None:
            # "r|" is the streaming mode: sequential-access only, does not
            # seek() the underlying object. Required here since source_fd is
            # a subprocess pipe (zstd -dc), which is not seekable — "r:"
            # looks similar but assumes a seekable file and raises
            # "[Errno 29] Illegal seek" on a pipe.
            tar = tarfile.open(fileobj=source_fd, mode="r|")
        else:
            tar = tarfile.open(str(archive), "r:*")

        with tar:
            for member in tar:
                raw_name = member.name.replace("\\", "/")
                parts = [part for part in raw_name.split("/") if part not in ("", ".")]
                if not parts or raw_name.startswith("/") or ".." in parts:
                    raise DAImportError(f"Unsafe path in {archive.name}: {member.name}")
                target = _ensure_inside(destination, destination.joinpath(*parts))
                if member.isdir():
                    target.mkdir(parents=True, exist_ok=True)
                    continue
                if member.issym() or member.islnk() or member.isdev() or member.isfifo():
                    continue
                if not member.isfile():
                    continue
                target.parent.mkdir(parents=True, exist_ok=True)
                source = tar.extractfile(member)
                if source is None:
                    continue
                with source, target.open("wb") as out:
                    shutil.copyfileobj(source, out, length=1024 * 1024)
                try:
                    os.utime(target, (member.mtime, member.mtime))
                except OSError:
                    pass
    finally:
        if proc is not None:
            _, stderr = proc.communicate(timeout=30)
            if proc.returncode != 0:
                raise DAImportError(
                    stderr.decode("utf-8", errors="replace").strip() or "zstd extraction failed"
                )


# ---------------------------------------------------------------------------
# Discovery functions
# ---------------------------------------------------------------------------

def _find_backup_root(extracted: Path) -> Path:
    if (extracted / "backup").exists() or (extracted / "domains").exists() or (extracted / "mysql").exists():
        return extracted
    candidates = []
    for path in extracted.rglob("backup"):
        if not path.is_dir():
            continue
        parent = path.parent
        score = 1
        if (parent / "domains").exists():
            score += 3
        if (parent / "mysql").exists():
            score += 2
        if (path / "user.conf").exists():
            score += 3
        candidates.append((score, parent))
    if candidates:
        return sorted(candidates, key=lambda item: item[0], reverse=True)[0][1]
    return extracted


def _discover_username(root: Path, archive_name: str) -> tuple[str, str]:
    user_conf = _read_key_values(root / "backup" / "user.conf")
    raw = (
        user_conf.get("username")
        or user_conf.get("user")
        or user_conf.get("account")
        or _archive_username(archive_name)
    )
    email = user_conf.get("email") or user_conf.get("emailaddress") or ""
    return _normalize_username(raw, archive_name), email


def _discover_domains(root: Path) -> list[str]:
    found: list[str] = []
    domains_list = root / "backup" / "domains.list"
    if domains_list.exists():
        for line in domains_list.read_text(encoding="utf-8", errors="ignore").splitlines():
            domain = _normalize_domain(line)
            if domain and domain not in found:
                found.append(domain)
    backup_dir = root / "backup"
    if backup_dir.exists():
        for item in backup_dir.iterdir():
            if item.is_file() and item.name.endswith(".conf"):
                domain = _normalize_domain(item.name[:-5])
                if domain and domain not in found:
                    found.append(domain)
    # Also detect domains from nested archives in backup/ (newer DA format)
    if backup_dir.exists():
        for item in backup_dir.iterdir():
            if item.is_file() and _is_archive(item):
                domain = _normalize_domain(_strip_archive_suffix(item.name))
                if domain and domain not in found:
                    found.append(domain)
    domains_dir = root / "domains"
    if domains_dir.exists():
        for item in domains_dir.iterdir():
            if item.is_dir():
                domain = _normalize_domain(item.name)
                if domain and domain not in found:
                    found.append(domain)
    return found


def _extract_nested_domain_archives(root: Path) -> None:
    """Extract nested domain archives found in backup/ directory.

    Newer DirectAdmin backups store domain files as individual
    .tar.gz/.tar.zst archives (e.g. backup/domain.com.tar.gz) instead
    of extracted directories under domains/.
    """
    backup_dir = root / "backup"
    if not backup_dir.exists():
        return
    domains_dir = root / "domains"
    for item in sorted(backup_dir.iterdir()):
        if not item.is_file() or not _is_archive(item):
            continue
        domain = _normalize_domain(_strip_archive_suffix(item.name))
        if not domain:
            continue
        target_dir = domains_dir / domain
        if target_dir.exists():
            continue
        _log(f"  Extracting nested domain archive: {item.name}")
        try:
            target_dir.mkdir(parents=True, exist_ok=True)
            _safe_extract_tar(item, target_dir)
            # DA archives sometimes wrap content in a subdirectory; flatten if needed
            sub_items = list(target_dir.iterdir())
            if len(sub_items) == 1 and sub_items[0].is_dir():
                inner = sub_items[0]
                for child in sorted(inner.iterdir(), reverse=True):
                    dest = target_dir / child.name
                    shutil.move(str(child), str(dest))
                inner.rmdir()
        except Exception as exc:
            _log(f"  WARNING: Failed to extract {item.name}: {exc}")
            shutil.rmtree(target_dir, ignore_errors=True)


def _source_for_domain(root: Path, domain: str) -> Optional[Path]:
    candidates = [
        root / "domains" / domain / "public_html",
        root / "domains" / domain / "private_html",
        root / "domains" / domain,
        root / domain / "public_html",
        root / domain,
    ]
    for candidate in candidates:
        if candidate.exists() and candidate.is_dir():
            if candidate.name == domain and (candidate / "public_html").exists():
                return candidate / "public_html"
            return candidate
    for path in root.rglob("public_html"):
        if path.is_dir() and domain in {part.lower() for part in path.parts}:
            return path
    return None


def _has_php_files(path: Path) -> bool:
    return any(item.is_file() and item.suffix.lower() in {".php", ".phtml"} for item in path.rglob("*"))


def _detect_app_type(source: Optional[Path]) -> str:
    if not source:
        return "php"
    if (source / "wp-config.php").exists() or (source / "wp-config-sample.php").exists():
        return "wordpress"
    return "php" if _has_php_files(source) else "static"


_SUBDOMAIN_LABEL_RE = re.compile(r"^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$")


def _discover_subdomains(root: Path, parent_domain: str) -> list[str]:
    """DA subdomain labels declared for a parent domain.

    DirectAdmin records them one label per line:
      - newer backups: ``backup/<domain>/subdomain.list``
      - older backups: ``backup/<domain>.subdomains``
    Each label's document root lives at ``domains/<domain>/public_html/<label>``.
    """
    labels: list[str] = []
    candidates = [
        root / "backup" / parent_domain / "subdomain.list",
        root / "backup" / f"{parent_domain}.subdomains",
        root / "backup" / "domains" / f"{parent_domain}.subdomains",
        root / "backup" / "domains" / parent_domain / "subdomain.list",
    ]
    for path in candidates:
        if not path.is_file():
            continue
        for raw in path.read_text(encoding="utf-8", errors="ignore").splitlines():
            label = raw.strip().lower()
            if not label or label.startswith("#"):
                continue
            # DA also stores "label:/custom/path" in some versions; take the label.
            label = label.split(":", 1)[0].split("=", 1)[0].strip()
            if _SUBDOMAIN_LABEL_RE.fullmatch(label) and label not in labels:
                labels.append(label)
    return labels


def _relocate_da_subdomain_sources(root: Path, parent_domains: list[str]) -> dict[str, Optional[Path]]:
    """Move DA subdomain docroots out of their parent's ``public_html``.

    DirectAdmin nests ``sub.example.com`` inside ``example.com``'s public_html
    (``.../public_html/sub``). OPanel Enterprise has no parent/child website concept -- each
    domain is standalone -- so every DA subdomain is imported as its own website
    with its own root. Moving each docroot into a staging sibling keeps the
    parent import from copying the same files again and gives the subdomain
    import a clean source. The move is a rename inside the opanel-ent-owned
    ``/tmp/opanel-ent-da-import-*`` staging tree, so it is cheap and touches no live
    files.

    Returns an ordered map of subdomain FQDN -> staging source dir (or ``None``
    when the label is declared but ships no files, so the loop still creates the
    website).
    """
    holding = root / "_opanel_subdomains"
    sources: dict[str, Optional[Path]] = {}
    for parent in parent_domains:
        labels = _discover_subdomains(root, parent)
        if not labels:
            continue
        parent_public = _source_for_domain(root, parent)
        _log(f"  {parent}: {len(labels)} DirectAdmin subdomain(s) declared")
        for label in labels:
            fqdn = _normalize_domain(f"{label}.{parent}")
            if not fqdn or fqdn in sources:
                continue
            src = (parent_public / label) if parent_public else None
            if src and src.is_dir() and not src.is_symlink():
                dest = holding / fqdn
                dest.parent.mkdir(parents=True, exist_ok=True)
                if dest.exists():
                    shutil.rmtree(dest, ignore_errors=True)
                shutil.move(str(src), str(dest))
                sources[fqdn] = dest
                _log(f"    staged subdomain {fqdn} from {parent}/public_html/{label}")
            else:
                sources[fqdn] = None
                _log(f"    subdomain {fqdn}: no files under public_html/{label}, importing empty")
    return sources


# ---------------------------------------------------------------------------
# Database config parsing
# ---------------------------------------------------------------------------

def _parse_wp_config(path: Path) -> dict[str, str]:
    config = path / "wp-config.php"
    if not config.exists():
        return {}
    text = config.read_text(encoding="utf-8", errors="ignore")
    found: dict[str, str] = {}
    for key in ("DB_NAME", "DB_USER", "DB_PASSWORD", "DB_HOST"):
        match = re.search(r"define\s*\(\s*['\"]" + key + r"['\"]\s*,\s*['\"]([^'\"]*)['\"]\s*\)", text)
        if match:
            found[key] = match.group(1)
    return found


def _parse_dotenv_config(path: Path) -> dict[str, str]:
    env_file = path / ".env"
    if not env_file.exists():
        return {}
    values = _read_key_values(env_file)
    result: dict[str, str] = {}
    if values.get("db_database"):
        result["DB_NAME"] = values["db_database"]
    if values.get("db_username"):
        result["DB_USER"] = values["db_username"]
    if values.get("db_password"):
        result["DB_PASSWORD"] = values["db_password"]
    if values.get("db_host"):
        result["DB_HOST"] = values["db_host"]
    return result


def _parse_php_variable_config(path: Path) -> dict[str, str]:
    config = path / "configuration.php"
    if not config.exists():
        return {}
    text = config.read_text(encoding="utf-8", errors="ignore")
    mappings = {
        "DB_NAME": ("db", "db_name", "database", "db_database"),
        "DB_USER": ("user", "dbuser", "db_user", "db_username", "username"),
        "DB_PASSWORD": ("password", "dbpass", "db_password"),
        "DB_HOST": ("host", "db_host"),
    }
    result: dict[str, str] = {}
    for key, names in mappings.items():
        for name in names:
            match = re.search(r"(?:public\s+)?\$" + re.escape(name) + r"\s*=\s*['\"]([^'\"]*)['\"]\s*;", text)
            if match:
                result[key] = match.group(1)
                break
    return result


# Directories that are never a site config location but are expensive to walk.
APP_CONFIG_SKIP_DIRS = {
    "wp-content", "wp-includes", "wp-admin", "node_modules", "vendor", "cache",
    "storage", "uploads", "images", "img", "assets", "media", "backup", "backups",
    ".git", ".well-known", "logs", "tmp",
}
APP_CONFIG_SCAN_DEPTH = 2


def _candidate_config_dirs(path: Path) -> list[Path]:
    """Directories that may hold the app's database config, closest first.

    Looking only at the top of public_html misses two very common layouts:
    WordPress allows wp-config.php one level above the document root, and
    plenty of accounts keep the site itself in a subfolder. Both hold the
    password the imported database has to match.
    """
    found: list[Path] = []
    seen: set[Path] = set()

    def add(candidate: Path) -> None:
        try:
            if candidate.is_dir() and candidate not in seen:
                seen.add(candidate)
                found.append(candidate)
        except OSError:
            return

    add(path)
    add(path.parent)
    level = [path]
    for _depth in range(APP_CONFIG_SCAN_DEPTH):
        children: list[Path] = []
        for parent in level:
            try:
                entries = sorted(parent.iterdir())
            except OSError:
                continue
            for entry in entries:
                if not entry.is_dir() or entry.name.startswith("."):
                    continue
                if entry.name.lower() in APP_CONFIG_SKIP_DIRS:
                    continue
                add(entry)
                children.append(entry)
        level = children
    return found


def _parse_app_db_config(path: Path) -> dict[str, str]:
    for directory in _candidate_config_dirs(path):
        for parser in (_parse_wp_config, _parse_dotenv_config, _parse_php_variable_config):
            values = parser(directory)
            if values.get("DB_NAME"):
                return values
    return {}


# ---------------------------------------------------------------------------
# SQL helpers
# ---------------------------------------------------------------------------

# mysql_native_password stores "*" followed by 40 hex characters.
DA_PASSWORD_HASH_RE = re.compile(r"^\*[0-9A-Fa-f]{40}$")


def _sql_base_name(path: Path) -> str:
    name = path.name
    for suffix in SQL_SUFFIXES:
        if name.lower().endswith(suffix):
            return name[:-len(suffix)]
    return path.stem


def _discover_sql_files(root: Path) -> dict[str, Path]:
    preferred = [root / "mysql", root / "backup"]
    files: list[Path] = []
    for directory in preferred:
        if directory.exists():
            files.extend(
                path for path in directory.rglob("*")
                if path.is_file() and any(path.name.lower().endswith(s) for s in SQL_SUFFIXES)
            )
    if not files:
        for path in root.rglob("*"):
            if path.is_file() and any(path.name.lower().endswith(s) for s in SQL_SUFFIXES):
                if "domains" not in {part.lower() for part in path.parts}:
                    files.append(path)
    result: dict[str, Path] = {}
    for path in files:
        result.setdefault(_sql_base_name(path).lower(), path)
    return result


def _import_db_password(app_config: dict[str, str], da_credentials: dict[str, str]) -> tuple[str, bool]:
    """Pick the password for an imported database user.

    Reusing the password the site already carries is what keeps it online after
    the import: the panel rewrites wp-config.php and friends, but any config it
    does not recognise -- a custom include, a second copy outside the document
    root -- would still point at the old password. Returns the password and
    whether it came from the backup.
    """
    original = (app_config or {}).get("DB_PASSWORD") or (da_credentials or {}).get("password") or ""
    if original:
        return original, True
    return secrets.token_urlsafe(16), False


def _da_db_conf_candidates(sql_file: Path, root: Path, base: str) -> list[Path]:
    """Every file that could be the DA conf for this dump, closest first.

    DA has moved these around between versions -- beside the dump, under
    backup/, under mysql/ -- and the suffix on the dump is not always part of
    the conf name, so the name is matched case-insensitively rather than
    assumed.
    """
    candidates = [sql_file.with_name(f"{base}.conf"), root / "backup" / f"{base}.conf"]
    seen = {path for path in candidates}
    for directory in (sql_file.parent, root / "backup", root / "mysql"):
        if not directory.is_dir():
            continue
        try:
            entries = sorted(directory.rglob("*.conf"))
        except OSError:
            continue
        for entry in entries:
            if entry in seen:
                continue
            if entry.stem.lower() == base.lower():
                seen.add(entry)
                candidates.append(entry)
    return candidates


def _da_db_credentials(sql_file: Path, root: Path) -> dict[str, str]:
    """Recover the original MySQL password for a DirectAdmin dump.

    DA writes a <db>.conf next to each dump. Older versions store the password
    in clear, newer ones only the mysql_native hash, which cannot be turned
    back into a password the panel could show. Only the clear one is usable.
    """
    base = _sql_base_name(sql_file)
    for candidate in _da_db_conf_candidates(sql_file, root, base):
        values = _read_key_values(candidate)
        if not values:
            continue
        for key in ("passwd", "password", "pass", "db_password", "dbpass"):
            value = values.get(key) or ""
            if not value:
                continue
            if DA_PASSWORD_HASH_RE.match(value):
                return {"password": "", "password_hash": value}
            return {"password": value, "password_hash": ""}
        for value in values.values():
            if DA_PASSWORD_HASH_RE.match(value or ""):
                return {"password": "", "password_hash": value}
    return {"password": "", "password_hash": ""}


def _temporary_sql_file(sql_file: Path) -> Path:
    lower = sql_file.name.lower()
    tmp = Path(tempfile.mkstemp(prefix="opanel-ent-da-", suffix=".sql")[1])

    if lower.endswith(".sql"):
        opener = open
    elif lower.endswith(".gz"):
        opener = gzip.open
    elif lower.endswith(".bz2"):
        opener = bz2.open
    elif lower.endswith(".zst"):
        zstd = shutil.which("zstdcat") or shutil.which("zstd")
        if not zstd:
            tmp.unlink(missing_ok=True)
            raise DAImportError("zstd is required to import .sql.zst files. Install: apt install zstd")
        args = [zstd, str(sql_file)] if Path(zstd).name == "zstdcat" else [zstd, "-dc", str(sql_file)]
        proc = subprocess.Popen(args, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True,
                                encoding="utf-8", errors="replace")
        try:
            with tmp.open("w", encoding="utf-8") as target:
                assert proc.stdout is not None
                for line in proc.stdout:
                    if SQL_DATABASE_DIRECTIVE_RE.match(line):
                        continue
                    target.write(SQL_DEFINER_RE.sub("", line))
            _, stderr = proc.communicate(timeout=60)
            if proc.returncode != 0:
                tmp.unlink(missing_ok=True)
                raise DAImportError(stderr.strip() or "zstd SQL decompression failed")
            return tmp
        finally:
            if proc.poll() is None:
                proc.kill()
    else:
        raise DAImportError(f"Unsupported SQL compression: {sql_file}")

    with opener(sql_file, "rt", encoding="utf-8", errors="replace") as source, \
            tmp.open("w", encoding="utf-8") as target:
        for line in source:
            if SQL_DATABASE_DIRECTIVE_RE.match(line):
                continue
            target.write(SQL_DEFINER_RE.sub("", line))
    return tmp


def _matched_sql_for_config(
    app_config: dict[str, str],
    sql_files: dict[str, Path],
    single_site: bool,
) -> tuple[str, Optional[Path]]:
    if app_config.get("DB_NAME"):
        key = app_config["DB_NAME"].lower()
        if key in sql_files:
            return key, sql_files[key]
    if single_site and len(sql_files) == 1:
        return next(iter(sql_files.items()))
    return "", None


# ---------------------------------------------------------------------------
# App config update
# ---------------------------------------------------------------------------

def _replace_define(text: str, key: str, value: str) -> str:
    pattern = re.compile(
        r"(define\s*\(\s*['\"]" + key + r"['\"]\s*,\s*)['\"][^'\"]*['\"](\s*\)\s*;)"
    )
    if pattern.search(text):
        return pattern.sub(lambda m: f"{m.group(1)}'{value}'{m.group(2)}", text, count=1)
    return text + f"\ndefine('{key}', '{value}');\n"


def _config_targets_database(directory: Path, db_name: str) -> bool:
    """True when the config in *directory* already points at this database.

    Outside the document root itself this is what keeps the rewrite honest: a
    second app living in a subfolder has its own database, and handing it these
    credentials would take it down instead of fixing anything.
    """
    for parser in (_parse_wp_config, _parse_dotenv_config, _parse_php_variable_config):
        name = parser(directory).get("DB_NAME") or ""
        if not name:
            continue
        if name == db_name:
            return True
        try:
            return _normalize_db_identifier(name, name, set()) == db_name
        except ValueError:
            return False
    return False


def _update_app_db_config(public: Path, db_name: str, db_user: str, db_password: str) -> None:
    """Point the site's configs at the imported database.

    The document root is always rewritten; anything further out is only
    rewritten when it already names this database.
    """
    _update_config_dir(public, db_name, db_user, db_password)
    for directory in _candidate_config_dirs(public):
        if directory == public:
            continue
        if not _config_targets_database(directory, db_name):
            continue
        _update_config_dir(directory, db_name, db_user, db_password)


def _update_config_dir(public: Path, db_name: str, db_user: str, db_password: str) -> None:
    wp = public / "wp-config.php"
    if wp.exists():
        text = wp.read_text(encoding="utf-8", errors="ignore")
        text = _replace_define(text, "DB_NAME", db_name)
        text = _replace_define(text, "DB_USER", db_user)
        text = _replace_define(text, "DB_PASSWORD", db_password)
        text = _replace_define(text, "DB_HOST", "localhost")
        wp.write_text(text, encoding="utf-8")

    env_file = public / ".env"
    if env_file.exists():
        lines = env_file.read_text(encoding="utf-8", errors="ignore").splitlines()
        replacements = {
            "DB_DATABASE": db_name,
            "DB_USERNAME": db_user,
            "DB_PASSWORD": db_password,
            "DB_HOST": "127.0.0.1",
        }
        out = []
        seen = set()
        for line in lines:
            stripped = line.strip()
            matched = False
            for env_key, env_val in replacements.items():
                if stripped.startswith(f"{env_key}="):
                    out.append(f"{env_key}={env_val}")
                    seen.add(env_key)
                    matched = True
                    break
            if not matched:
                out.append(line)
        for env_key, env_val in replacements.items():
            if env_key not in seen:
                out.append(f"{env_key}={env_val}")
        env_file.write_text("\n".join(out) + "\n", encoding="utf-8")

    config_php = public / "configuration.php"
    if config_php.exists():
        text = config_php.read_text(encoding="utf-8", errors="ignore")
        var_map = {
            "db": db_name,
            "db_name": db_name,
            "database": db_name,
            "db_database": db_name,
            "user": db_user,
            "dbuser": db_user,
            "db_user": db_user,
            "db_username": db_user,
            "username": db_user,
            "password": db_password,
            "dbpass": db_password,
            "db_password": db_password,
            "host": "localhost",
            "db_host": "localhost",
        }
        for var_name, var_val in var_map.items():
            pattern = re.compile(r"((?:public\s+)?\$" + re.escape(var_name) + r"\s*=\s*)['\"][^'\"]*['\"](\s*;)")
            if pattern.search(text):
                text = pattern.sub(lambda m, v=var_val: f"{m.group(1)}'{v}'{m.group(2)}", text, count=1)
        config_php.write_text(text, encoding="utf-8")


# ---------------------------------------------------------------------------
# Delete existing domain/user (for re-import)
# ---------------------------------------------------------------------------

def _delete_existing_domain(db, domain: str) -> None:
    website = db.query(Website).filter(Website.domain == domain).first()
    if website:
        _log(f"  Removing existing website record for {domain}")
        db.query(DatabaseAccount).filter(DatabaseAccount.website_id == website.id).delete()
        db.query(WebsiteAlias).filter(WebsiteAlias.website_id == website.id).delete()
        db.delete(website)
        db.flush()


def _available_email(db, username: str, preferred: str) -> tuple[str, str | None]:
    """The archive's contact address, or a placeholder when it has none.

    users.email is not unique, so DirectAdmin accounts that share one contact
    mailbox are kept as-is. The second tuple element (a warning) is always None
    now; it is retained for the caller's signature.
    """
    return (preferred or f"{username}@import.local"), None


def _delete_existing_user(db, username: str) -> None:
    user = db.query(User).filter(User.username == username).first()
    if user:
        _log(f"  Removing existing panel user {username}")
        db.query(DatabaseAccount).filter(DatabaseAccount.owner_id == user.id).delete()
        for w in db.query(Website).filter(Website.owner_id == user.id).all():
            db.query(WebsiteAlias).filter(WebsiteAlias.website_id == w.id).delete()
            db.delete(w)
        db.delete(user)
        db.flush()


# ---------------------------------------------------------------------------
# Core import: process a single extracted DA backup
# ---------------------------------------------------------------------------

def _process_archive(
    extracted_dir: Path,
    archive_name: str,
    db,
    credentials: list[str],
    overwrite: bool = False,
) -> dict:
    """Import one DirectAdmin backup from its extracted staging directory."""
    root = _find_backup_root(extracted_dir)

    # Extract nested domain archives for newer DA backup format
    _extract_nested_domain_archives(root)

    domains = _discover_domains(root)

    # DirectAdmin stores each subdomain's files inside its parent domain's
    # public_html (sub.example.com -> domains/example.com/public_html/sub). Pull
    # those out into their own staging dirs and import each as a standalone
    # website -- OPanel Enterprise has no parent/child concept.
    subdomain_sources = _relocate_da_subdomain_sources(root, list(domains))
    for fqdn in subdomain_sources:
        if fqdn not in domains:
            domains.append(fqdn)

    username, email = _discover_username(root, archive_name)

    summary: dict = {
        "archive": archive_name,
        "username": username,
        "domains": domains,
        "subdomains": list(subdomain_sources),
        "imported_domains": [],
        "databases": [],
        "ssl_enabled_domains": [],
        "warnings": [],
    }

    if not domains:
        summary["warnings"].append("No domains found")
        _log(f"Skipping {archive_name}: no domains found")
        return summary

    # Refuse to destroy live records unless the admin asked for it. Importing
    # used to delete any panel user of the same name together with all of their
    # websites, aliases and database records -- a customer already hosted here
    # simply disappeared, and because the deletes were committed partway through
    # the import, a later failure could not bring them back.
    if not overwrite:
        clashes = []
        if db.query(User).filter(User.username == username).first():
            clashes.append(f"panel user '{username}'")
        taken = [
            d for d in domains
            if db.query(Website).filter(Website.domain == d).first()
        ]
        clashes.extend(f"website '{d}'" for d in taken)
        if clashes:
            raise DAImportError(
                "Already on this server: " + ", ".join(clashes)
                + ". Re-run with overwrite enabled to replace them, or rename the"
                " DirectAdmin account first."
            )

    # Remove any existing records for these domains / user
    for domain in domains:
        _delete_existing_domain(db, domain)
    _delete_existing_user(db, username)

    # Create panel user
    password = secrets.token_urlsafe(16)
    hashed = hash_password(password)
    user = db.query(User).filter(User.username == username).first()
    if user is None:
        safe_email, email_warning = _available_email(db, username, email)
        if email_warning:
            summary["warnings"].append(email_warning)
            _log(f"  {email_warning}")
        user = User(
            username=username,
            email=safe_email,
            hashed_password=hashed,
            role="end_user",
            is_active=True,
            website_limit=max(len(domains), 5),
            storage_limit_mb=0,
        )
        db.add(user)
        db.flush()
        _log(f"  Created panel user: {username}")
    else:
        _log(f"  Using existing panel user: {username}")

    # Create Linux user
    linux_user = site_users.ensure_panel_user(username, password)
    credentials.append(f"user username={username} password={password}")

    # Discover SQL files
    sql_files = _discover_sql_files(root)
    imported_sql_keys: set[str] = set()
    websites = []

    for domain in domains:
        if domain in subdomain_sources:
            source = subdomain_sources[domain]
        else:
            source = _source_for_domain(root, domain)
        app_type = _detect_app_type(source)
        app_config = _parse_app_db_config(source) if source else {}
        php_version = settings.default_php_version

        _log(f"  Importing domain {domain} for user {username} ...")

        # Determine root path and document root
        document_root = "public_html"
        root_path = site_users.site_root_for_panel_user(username, domain)

        # Ensure site runtime (PHP-FPM pool, directory structure)
        runtime_php_version = php_version if app_type in {"wordpress", "php"} else None
        site_users.ensure_site_runtime(domain, root_path, runtime_php_version, linux_user)

        # Ensure document root exists
        public = site_users.ensure_document_root(root_path, document_root, linux_user)

        # Copy site files. public_html is owned by the site's own Linux user
        # (see ensure_document_root), so this must go through the privileged
        # helper rather than a plain shutil copy running as the opanel-ent
        # service account.
        if source and source.exists():
            site_users.import_site_files(root_path, document_root, linux_user, source)
            _log(f"    Copied site files to {public}")

        # Create or update website record
        website = db.query(Website).filter(Website.domain == domain).first()
        nginx_rewrite_mode = "front_controller" if app_type in {"wordpress", "php"} else "none"
        if website is None:
            website = Website(
                domain=domain,
                owner_id=user.id,
                root_path=root_path,
                document_root=document_root,
                linux_user=linux_user,
                php_version=php_version,
                app_type=app_type,
                ssl_enabled=False,
                status="active",
                nginx_custom="",
                nginx_config_mode="managed",
                nginx_rewrite_mode=nginx_rewrite_mode,
                waf_enabled=True,
                waf_default_rules="",
                waf_custom_rules="",
            )
            db.add(website)
            db.flush()
        else:
            website.owner_id = user.id
            website.root_path = root_path
            website.document_root = document_root
            website.linux_user = linux_user
            website.php_version = php_version
            website.app_type = app_type
            website.status = "active"
            website.nginx_config_mode = "managed"
            website.nginx_rewrite_mode = nginx_rewrite_mode
            db.flush()
        websites.append(website)

        # Re-parse app config from copied files
        app_config = app_config or _parse_app_db_config(public)

        # Import database
        matched_key, matched_sql = _matched_sql_for_config(app_config, sql_files, len(domains) == 1)
        if matched_sql:
            temp_sql = None
            try:
                db_name = _normalize_db_identifier(
                    app_config.get("DB_NAME") or matched_key,
                    matched_key,
                    set(),
                )
                db_user = _normalize_db_identifier(
                    app_config.get("DB_USER") or matched_key,
                    matched_key,
                    set(),
                )

                existing_account = db.query(DatabaseAccount).filter(DatabaseAccount.db_name == db_name).first()
                if existing_account is not None:
                    # Already imported (e.g. re-running an import, or a
                    # standalone .sql dump that duplicates a domain-matched
                    # one) — skip instead of retrying the INSERT and hitting
                    # the db_name UNIQUE constraint.
                    imported_sql_keys.add(matched_key)
                    _log(f"    Database {db_name} already imported, skipping")
                else:
                    # Reuse the password the site already has. Generating a new
                    # one leaves wp-config.php (or whatever config the app
                    # actually reads) pointing at a password MariaDB no longer
                    # accepts, and the site stays down until someone resets it
                    # by hand in the Databases page.
                    da_credentials = _da_db_credentials(matched_sql, root)
                    db_password, reused_password = _import_db_password(app_config, da_credentials)
                    if not reused_password and da_credentials["password_hash"]:
                        _log(
                            f"    NOTE: {matched_key} only ships a password hash; "
                            "generated a new database password instead"
                        )

                    # Decompress SQL if needed
                    temp_sql = _temporary_sql_file(matched_sql)

                    # Create database and user
                    mariadb.create_database_credentials(db_name, db_user, db_password, allow_existing=True)

                    # Import SQL
                    mariadb.import_database(db_name, str(temp_sql))

                    # Store credentials in panel DB
                    item = DatabaseAccount(
                        owner_id=user.id,
                        website_id=website.id,
                        db_name=db_name,
                        db_user=db_user,
                        db_password=encrypt(db_password),
                    )
                    db.add(item)
                    db.commit()

                    # Update app config files with the DB credentials. This
                    # is best-effort: the database itself is already imported,
                    # so a config the panel cannot parse or write must not be
                    # reported as a failed database import.
                    try:
                        _update_app_db_config(public, db_name, db_user, db_password)
                    except OSError as exc:
                        _log(f"    WARNING: could not update app config for {domain}: {exc}")
                        summary["warnings"].append(f"App config not updated for {domain}: {exc}")

                    imported_sql_keys.add(matched_key)
                    summary["databases"].append({
                        "domain": domain,
                        "source": str(matched_sql),
                        "db_name": db_name,
                    })
                    credentials.append(f"database target={domain} db_name={db_name} db_user={db_user} db_password={db_password}")
                    _log(f"    Imported database: {db_name}")
            except Exception as exc:
                db.rollback()
                _log(f"    WARNING: Database import failed for {domain}: {exc}")
                summary["warnings"].append(f"Database import failed for {domain}: {exc}")
            finally:
                if temp_sql is not None:
                    temp_sql.unlink(missing_ok=True)

        # Fix permissions
        site_users.fix_site_permissions(root_path, linux_user)

        # Configure OLS vhost. defer_reload stages the vhost file only -- a
        # single OLS reload happens after the loop so an archive with dozens of
        # domains/subdomains does not restart OpenLiteSpeed once per site.
        try:
            lsphp_socket = site_users.site_lsphp_socket(linux_user, root_path, runtime_php_version)
            openlitespeed.rewrite_vhost(
                domain,
                root_path,
                app_type=app_type,
                php_version=php_version,
                custom_directives="",
                linux_user=linux_user,
                lsphp_socket_override=lsphp_socket,
                waf_enabled=website.waf_enabled,
                document_root=document_root,
                rewrite_mode=nginx_rewrite_mode,
                aliases=[],
                defer_reload=True,
            )
            _log(f"    Configured OLS vhost for {domain}")
        except Exception as exc:
            _log(f"    WARNING: OLS vhost config failed for {domain}: {exc}")
            summary["warnings"].append(f"OLS vhost failed: {exc}")

        # Configure WAF
        try:
            result = waf.sync_website_rules(website, defer_reload=True)
            if result.returncode != 0:
                _log(f"    WARNING: WAF rules failed for {domain}")
        except Exception as exc:
            _log(f"    WARNING: WAF rules failed for {domain}: {exc}")

        summary["imported_domains"].append(domain)

    # Sync the OLS main config and restart once for every vhost written above.
    if summary["imported_domains"]:
        try:
            openlitespeed.reload_service()
            _log(f"  Reloaded OpenLiteSpeed for {len(summary['imported_domains'])} vhost(s)")
        except Exception as exc:
            _log(f"  WARNING: OpenLiteSpeed reload failed: {exc}")
            summary["warnings"].append(f"OpenLiteSpeed reload failed: {exc}")

    # Import unassigned SQL files as standalone databases
    for key, sql_path in sql_files.items():
        if key in imported_sql_keys:
            continue
        temp_sql = None
        try:
            db_name = _normalize_db_identifier(key, key, set())
            if db.query(DatabaseAccount).filter(DatabaseAccount.db_name == db_name).first() is not None:
                # Already imported via the per-domain match above (or a
                # previous run) — skip instead of retrying the INSERT and
                # hitting the db_name UNIQUE constraint.
                _log(f"    Database {db_name} already imported, skipping")
                continue
            temp_sql = _temporary_sql_file(sql_path)
            db_user = _normalize_db_identifier(key, key, set())
            db_password, _reused = _import_db_password({}, _da_db_credentials(sql_path, root))
            mariadb.create_database_credentials(db_name, db_user, db_password, allow_existing=True)
            mariadb.import_database(db_name, str(temp_sql))
            item = DatabaseAccount(
                owner_id=user.id,
                website_id=None,
                db_name=db_name,
                db_user=db_user,
                db_password=encrypt(db_password),
            )
            db.add(item)
            db.commit()
            summary["databases"].append({
                "domain": None,
                "source": str(sql_path),
                "db_name": db_name,
                "db_user": db_user,
            })
            credentials.append(f"database target={username} db_name={db_name} db_user={db_user} db_password={db_password}")
            _log(f"    Imported unassigned database: {db_name}")
        except Exception as exc:
            db.rollback()
            _log(f"    WARNING: Unassigned database import failed for {key}: {exc}")
        finally:
            if temp_sql is not None:
                temp_sql.unlink(missing_ok=True)

    db.commit()

    # Try to enable SSL when DNS matches server IP. An archive with many
    # subdomains of one domain can ask Let's Encrypt for dozens of certs in a
    # row; after a run of failures (typically the LE rate limit -- 50 certs per
    # registered domain per week) stop trying instead of hammering the CA. The
    # user can issue one *.<domain> wildcard afterwards and reuse it.
    consecutive_ssl_failures = 0
    for website in websites:
        if consecutive_ssl_failures >= 5:
            _log("    SSL: too many consecutive certbot failures, skipping the rest")
            summary["warnings"].append(
                "Automatic SSL stopped after repeated certbot failures (likely the "
                "Let's Encrypt rate limit); issue a wildcard certificate and reuse it"
            )
            break
        try:
            import socket
            server_ip = socket.gethostbyname(socket.gethostname())
            try:
                domain_ip = socket.gethostbyname(website.domain)
            except socket.gaierror:
                continue
            if domain_ip == server_ip:
                try:
                    from app.services import ssl as ssl_service
                    result = ssl_service.issue_ssl(website.domain, [])
                    if result.returncode == 0:
                        website.ssl_enabled = True
                        db.commit()
                        summary["ssl_enabled_domains"].append(website.domain)
                        consecutive_ssl_failures = 0
                        _log(f"    SSL enabled for {website.domain}")
                    else:
                        consecutive_ssl_failures += 1
                        _log(f"    SSL skipped for {website.domain}: certbot returned {result.returncode}")
                except Exception as exc:
                    consecutive_ssl_failures += 1
                    db.rollback()
                    _log(f"    SSL skipped for {website.domain}: {exc}")
        except Exception:
            pass

    return summary


# ---------------------------------------------------------------------------
# Public API
# ---------------------------------------------------------------------------

def da_backup_dir() -> str:
    """Return the DA backup directory path."""
    return settings.da_backup_dir


def list_da_backups() -> list[dict]:
    """List DirectAdmin backup archives in the DA backup directory."""
    backup_dir = Path(settings.da_backup_dir)
    if not backup_dir.exists():
        return []
    items = []
    for path in sorted(backup_dir.iterdir(), key=lambda p: p.stat().st_mtime, reverse=True):
        if _is_archive(path):
            items.append({
                "filename": path.name,
                "path": str(path),
                "size": path.stat().st_size,
                "modified": _dt.datetime.fromtimestamp(path.stat().st_mtime).isoformat() + "Z",
            })
    return items


def save_da_backup(filename: str, source_file) -> dict:
    """Save an uploaded DA backup archive to the DA backup directory."""
    backup_dir = Path(settings.da_backup_dir)
    backup_dir.mkdir(parents=True, exist_ok=True)
    safe_name = Path(filename).name
    if not any(safe_name.lower().endswith(suffix) for suffix in ARCHIVE_SUFFIXES):
        raise ValueError("Unsupported archive format. Supported: .tar.gz, .tar.bz2, .tar.xz, .tar.zst, .tar")
    target = (backup_dir / safe_name).resolve()
    if backup_dir.resolve() not in target.parents:
        raise ValueError("Invalid backup filename")
    # Deduplicate if file exists
    if target.exists():
        stem = _strip_archive_suffix(safe_name)
        ext = safe_name[len(stem):]
        suffix = _dt.datetime.utcnow().strftime("%Y%m%d%H%M%S")
        target = (backup_dir / f"{stem}-{suffix}-{secrets.token_hex(3)}{ext}").resolve()
    written = 0
    with target.open("wb") as buffer:
        while chunk := source_file.read(1024 * 1024):
            written += len(chunk)
            if written > MAX_UPLOAD_BYTES:
                target.unlink(missing_ok=True)
                raise ValueError("Backup file is too large (max 1 GB)")
            buffer.write(chunk)
    return {
        "filename": target.name,
        "path": str(target),
        "size": target.stat().st_size,
        "modified": _dt.datetime.fromtimestamp(target.stat().st_mtime).isoformat() + "Z",
    }


def delete_da_backup(backup_file: str) -> str:
    """Delete a DA backup archive from the DA backup directory."""
    path = _resolve_da_backup_path(backup_file)
    name = path.name
    path.unlink()
    return name


def import_da_backup(backup_file: str, db, overwrite: bool = False) -> dict:
    """Import a single DirectAdmin backup archive.

    Extracts the archive, discovers DA user/domains, creates panel user,
    websites, databases, configures OLS vhosts, and returns a summary.
    """
    path = _resolve_da_backup_path(backup_file)

    archive_name = path.name
    _log(f"Starting DA import: {archive_name}")

    # Extract to temporary staging directory
    stage = Path(tempfile.mkdtemp(prefix="opanel-ent-da-import-"))
    credentials: list[str] = [
        "# OPanel Enterprise DirectAdmin import credentials",
        f"# Archive: {archive_name}",
        f"# Date: {_dt.datetime.utcnow().isoformat()}Z",
        "# Keep this file private.",
    ]

    try:
        _log(f"Extracting {archive_name} ...")
        _safe_extract_tar(path, stage)

        summary = _process_archive(stage, archive_name, db, credentials, overwrite=overwrite)

        # Write credentials file alongside the backup
        cred_file = path.parent / f"{_strip_archive_suffix(archive_name)}-credentials.txt"
        cred_file.write_text("\n".join(credentials) + "\n", encoding="utf-8")
        cred_file.chmod(0o600)

        _log(f"DA import completed: {archive_name}")
        return summary

    except Exception as exc:
        _log(f"DA import failed: {archive_name}: {exc}")
        raise
    finally:
        shutil.rmtree(stage, ignore_errors=True)
