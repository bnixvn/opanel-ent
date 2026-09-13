"""API token authentication for the provisioning API.

Tokens are hashed (SHA-256) in the database. Incoming Bearer tokens are hashed
and looked up. Scope checking is done at the dependency level.
"""

import hashlib
import json
from datetime import datetime

from fastapi import Depends, HTTPException, Request, status
from fastapi.security import HTTPAuthorizationCredentials, HTTPBearer
from sqlalchemy.orm import Session

from app.core.database import get_db
from app.models.entities import ApiToken

_bearer_scheme = HTTPBearer(auto_error=True)


def _hash_token(raw: str) -> str:
    return hashlib.sha256(raw.encode("utf-8")).hexdigest()


def get_api_token(
    creds: HTTPAuthorizationCredentials = Depends(_bearer_scheme),
    db: Session = Depends(get_db),
) -> ApiToken:
    """Validate bearer token, return the ApiToken row, update last_used_at."""
    token_hash = _hash_token(creds.credentials)
    token = db.query(ApiToken).filter(ApiToken.token_hash == token_hash).first()
    if token is None:
        raise HTTPException(status_code=status.HTTP_401_UNAUTHORIZED, detail="Invalid API token")
    if token.revoked_at is not None:
        raise HTTPException(status_code=status.HTTP_401_UNAUTHORIZED, detail="API token revoked")
    if token.expires_at is not None and token.expires_at < datetime.utcnow():
        raise HTTPException(status_code=status.HTTP_401_UNAUTHORIZED, detail="API token expired")

    # IP allowlist check
    if token.ip_allowlist:
        allowed = [ip.strip() for ip in token.ip_allowlist.split(",") if ip.strip()]
        if allowed:
            # Lazy import to avoid circular — Request.client is available via the
            # dependency chain, but we re-read it here for clarity.
            pass  # checked below via _check_ip

    token.last_used_at = datetime.utcnow()
    db.commit()
    return token


def require_scope(scope: str):
    """Dependency factory: check that the API token has the required scope."""

    def _dep(token: ApiToken = Depends(get_api_token)) -> ApiToken:
        scopes = json.loads(token.scopes or "[]")
        if scope not in scopes and "provisioning:admin" not in scopes:
            raise HTTPException(
                status_code=status.HTTP_403_FORBIDDEN,
                detail=f"API token missing required scope: {scope}",
            )
        return token

    return _dep


def check_ip_allowlist(request: Request, token: ApiToken = Depends(get_api_token)) -> ApiToken:
    """Enforce IP allowlist if configured on the token."""
    if not token.ip_allowlist:
        return token
    allowed = [ip.strip() for ip in token.ip_allowlist.split(",") if ip.strip()]
    if not allowed:
        return token
    client_ip = (request.client.host if request.client else "") or ""
    if client_ip not in allowed:
        raise HTTPException(status_code=status.HTTP_403_FORBIDDEN, detail="IP not in allowlist")
    return token


def require_provisioning_write(
    request: Request,
    token: ApiToken = Depends(require_scope("provisioning:write")),
) -> ApiToken:
    """Shortcut: provisioning:write scope + IP allowlist."""
    return check_ip_allowlist(request, token)


def require_provisioning_read(
    request: Request,
    token: ApiToken = Depends(require_scope("provisioning:read")),
) -> ApiToken:
    """Shortcut: provisioning:read scope + IP allowlist."""
    return check_ip_allowlist(request, token)
