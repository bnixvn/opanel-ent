"""API Token management — admin-only CRUD for provisioning tokens."""

import json
from typing import List

from fastapi import APIRouter, Depends, HTTPException, Request
from pydantic import BaseModel, Field
from sqlalchemy.orm import Session

from app.api.deps import get_current_user
from app.core.database import get_db
from app.core.permissions import Role, ensure_role
from app.models.entities import ApiToken, User
from app.services import provisioning
from app.services.audit import log_action

router = APIRouter(prefix="/api-tokens", tags=["api-tokens"])


class ApiTokenCreate(BaseModel):
    name: str = Field(min_length=1, max_length=128)
    scopes: List[str] = Field(default_factory=lambda: ["provisioning:read", "provisioning:write"])
    expires_days: int = Field(default=365, ge=1, le=3650)
    ip_allowlist: str = ""


class ApiTokenOut(BaseModel):
    id: int
    name: str
    scopes: List[str]
    expires_at: str | None = None
    revoked_at: str | None = None
    last_used_at: str | None = None
    ip_allowlist: str | None = None
    created_at: str | None = None
    prefix: str = ""

    class Config:
        from_attributes = True


class ApiTokenCreatedOut(ApiTokenOut):
    token: str = ""


def _token_out(t: ApiToken) -> dict:
    scopes = []
    try:
        scopes = json.loads(t.scopes or "[]")
    except (json.JSONDecodeError, TypeError):
        pass
    return {
        "id": t.id,
        "name": t.name,
        "scopes": scopes,
        "expires_at": t.expires_at.isoformat() if t.expires_at else None,
        "revoked_at": t.revoked_at.isoformat() if t.revoked_at else None,
        "last_used_at": t.last_used_at.isoformat() if t.last_used_at else None,
        "ip_allowlist": t.ip_allowlist,
        "created_at": t.created_at.isoformat() if t.created_at else None,
        "prefix": t.token_hash[:8] if t.token_hash else "",
    }


@router.get("", response_model=List[ApiTokenOut])
def list_tokens(db: Session = Depends(get_db), current_user: User = Depends(get_current_user)):
    ensure_role(current_user.role, Role.admin)
    tokens = provisioning.list_api_tokens(db)
    return [_token_out(t) for t in tokens]


@router.post("", response_model=ApiTokenCreatedOut)
def create_token(
    payload: ApiTokenCreate,
    request: Request,
    db: Session = Depends(get_db),
    current_user: User = Depends(get_current_user),
):
    ensure_role(current_user.role, Role.admin)
    raw_token, token = provisioning.create_api_token(
        db,
        name=payload.name,
        scopes=payload.scopes,
        expires_days=payload.expires_days,
        ip_allowlist=payload.ip_allowlist,
    )
    log_action(db, current_user.id, "create_api_token", payload.name, request=request)
    result = _token_out(token)
    result["token"] = raw_token
    return result


@router.delete("/{token_id}")
def delete_token(
    token_id: int,
    request: Request,
    db: Session = Depends(get_db),
    current_user: User = Depends(get_current_user),
):
    ensure_role(current_user.role, Role.admin)
    if not provisioning.delete_api_token(db, token_id):
        raise HTTPException(status_code=404, detail="Token not found")
    log_action(db, current_user.id, "delete_api_token", str(token_id), request=request)
    return {"ok": True}
