import os
import stat
import subprocess
import time
from pathlib import Path

from sqlalchemy.orm import Session

from app.core.permissions import is_admin_role
from app.models.entities import User, Website


BYTES_PER_MB = 1024 * 1024
STATIC_SITE_ESTIMATE_BYTES = 1 * BYTES_PER_MB
WORDPRESS_SITE_ESTIMATE_BYTES = 100 * BYTES_PER_MB

# Walking a customer's whole home in Python is slow (a WordPress install is tens
# of thousands of files); `du` does it in C, and the result is cached briefly so
# the admin user list / dashboard don't recompute it on every request.
_USAGE_TTL_SECONDS = 300
_usage_cache: dict[int, tuple[float, int]] = {}


class StorageQuotaExceeded(ValueError):
    pass


def user_storage_limit_bytes(user: User) -> int | None:
    """Bytes the account may use, or None for unlimited.

    0 means unlimited, the convention every hosting panel uses and the one the
    admin UI offers. Returning 0 bytes here instead made the limit "may store
    nothing", so every write failed with a quota error the moment an admin set
    the field to 0 expecting no cap.
    """
    if is_admin_role(user.role):
        return None
    limit_mb = int(user.storage_limit_mb or 0)
    if limit_mb <= 0:
        return None
    return limit_mb * BYTES_PER_MB


def path_usage_bytes(path: str | Path) -> int:
    root = Path(path)
    total = 0
    stack = [root]
    while stack:
        current = stack.pop()
        try:
            item_stat = current.lstat()
        except OSError:
            continue
        if stat.S_ISLNK(item_stat.st_mode):
            continue
        total += item_stat.st_size
        if stat.S_ISDIR(item_stat.st_mode):
            try:
                stack.extend(current.iterdir())
            except OSError:
                continue
    return total


def _du_bytes(paths: list[str]) -> int | None:
    """Apparent size of ``paths`` via `du`, or None when du is unavailable/failed."""
    real = [p for p in paths if p and Path(p).exists()]
    if not real:
        return 0
    try:
        proc = subprocess.run(
            ["du", "-sbc", "--", *real],
            capture_output=True, text=True, timeout=120, check=False,
        )
    except (OSError, subprocess.TimeoutExpired):
        return None
    total_line = ""
    for line in (proc.stdout or "").splitlines():
        if line.endswith("\ttotal") or line.endswith(" total"):
            total_line = line
    if not total_line and proc.stdout:
        total_line = proc.stdout.splitlines()[-1]
    try:
        return int(total_line.split()[0])
    except (ValueError, IndexError):
        return None


def website_storage_used_bytes(website: Website) -> int:
    if not website.root_path:
        return 0
    fast = _du_bytes([website.root_path])
    return fast if fast is not None else path_usage_bytes(website.root_path)


def user_storage_used_bytes(db: Session, user: User, *, use_cache: bool = True) -> int:
    now = time.monotonic()
    if use_cache:
        cached = _usage_cache.get(user.id)
        if cached and now - cached[0] < _USAGE_TTL_SECONDS:
            return cached[1]
    roots = [w.root_path for w in db.query(Website).filter(Website.owner_id == user.id).all() if w.root_path]
    fast = _du_bytes(roots)
    total = fast if fast is not None else sum(path_usage_bytes(p) for p in roots)
    _usage_cache[user.id] = (now, total)
    return total


def storage_usage_summary(db: Session, user: User) -> dict:
    used_bytes = user_storage_used_bytes(db, user)
    limit_bytes = user_storage_limit_bytes(user)
    percent = 0.0
    if limit_bytes and limit_bytes > 0:
        percent = min(999.0, round((used_bytes / limit_bytes) * 100, 2))
    return {
        "storage_used_bytes": used_bytes,
        "storage_limit_bytes": limit_bytes,
        "storage_percent": percent,
    }


def enforce_user_storage_quota(
    db: Session,
    user: User,
    *,
    incoming_bytes: int = 0,
    replaced_bytes: int = 0,
) -> None:
    limit_bytes = user_storage_limit_bytes(user)
    if limit_bytes is None:
        return
    # Enforcement must not trust a stale cache.
    used_bytes = user_storage_used_bytes(db, user, use_cache=False)
    projected_bytes = max(0, used_bytes - max(0, replaced_bytes)) + max(0, incoming_bytes)
    if projected_bytes > limit_bytes:
        raise StorageQuotaExceeded(
            f"Storage quota exceeded: {projected_bytes // BYTES_PER_MB} MB used/projected, "
            f"limit {limit_bytes // BYTES_PER_MB} MB"
        )


def source_file_size(source_file) -> int | None:
    try:
        position = source_file.tell()
        source_file.seek(0, os.SEEK_END)
        size = source_file.tell()
        source_file.seek(position)
        return max(0, int(size - position))
    except (AttributeError, OSError, ValueError):
        return None
