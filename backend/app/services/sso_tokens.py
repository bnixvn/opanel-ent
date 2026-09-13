import json
import os
import secrets
from datetime import datetime, timedelta, timezone
from pathlib import Path
from typing import Optional


TOKEN_TTL_SECONDS = 120
TOKEN_DIR = Path("/tmp/opanel-ent-phpmyadmin-sso")


def create_phpmyadmin_token(db_user: str, db_password: str, db_name: str) -> str:
    cleanup_expired_tokens()
    token = secrets.token_urlsafe(32)
    TOKEN_DIR.mkdir(mode=0o700, parents=True, exist_ok=True)
    token_path = _token_path(token)
    payload = json.dumps({
        "db_user": db_user,
        "db_password": db_password,
        "db_name": db_name,
        "expires_at": (datetime.now(timezone.utc) + timedelta(seconds=TOKEN_TTL_SECONDS)).isoformat(),
    })
    # Atomic create with mode 0o600 from the start. O_EXCL prevents symlink
    # racing Ã¢â‚¬â€ if a file already exists at that path (collision or attack),
    # we abort.
    flags = os.O_WRONLY | os.O_CREAT | os.O_EXCL
    if hasattr(os, "O_NOFOLLOW"):
        flags |= os.O_NOFOLLOW
    fd = os.open(str(token_path), flags, 0o600)
    try:
        with os.fdopen(fd, "w", encoding="utf-8") as fh:
            fh.write(payload)
    except Exception:
        token_path.unlink(missing_ok=True)
        raise
    return token


def consume_phpmyadmin_token(token: str) -> Optional[dict]:
    cleanup_expired_tokens()
    token_path = _token_path(token)
    if not token_path.exists():
        return None
    try:
        data = json.loads(token_path.read_text(encoding="utf-8"))
    finally:
        token_path.unlink(missing_ok=True)
    expires_at = datetime.fromisoformat(data["expires_at"])
    if expires_at < datetime.now(timezone.utc):
        return None
    return data


def cleanup_expired_tokens() -> None:
    if not TOKEN_DIR.exists():
        return
    now = datetime.now(timezone.utc)
    for token_path in TOKEN_DIR.glob("*.json"):
        try:
            data = json.loads(token_path.read_text(encoding="utf-8"))
            expires_at = datetime.fromisoformat(data["expires_at"])
        except (OSError, ValueError, KeyError):
            token_path.unlink(missing_ok=True)
            continue
        if expires_at < now:
            token_path.unlink(missing_ok=True)


def _token_path(token: str) -> Path:
    if not token.replace("-", "").replace("_", "").isalnum():
        raise ValueError("Invalid token")
    return TOKEN_DIR / f"{token}.json"
