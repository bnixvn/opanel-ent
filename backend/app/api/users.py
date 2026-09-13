import json

from fastapi import APIRouter, Depends, HTTPException, Query, Request
from sqlalchemy.orm import Session
from typing import List, Optional

from app.api.deps import get_current_user
from app.core.database import get_db
from app.core.permissions import Role, ensure_role
from app.core.security import hash_password
from app.core.step_up import require_sensitive_action_step_up
from app.models.entities import AuditLog, BackupSchedule, DatabaseAccount, User, Website
from app.schemas.schemas import (
    AuditLogOut,
    UserCreate,
    UserOut,
    UserPasswordUpdate,
    UserUpdate,
)
from app.services.audit import log_action
from app.services import mariadb, openlitespeed, site_users, storage_quota, wordpress

router = APIRouter(prefix="/users", tags=["users"])


def _user_out(user: User, db: Session) -> dict:
    data = UserOut.model_validate(user).model_dump()
    data.update(storage_quota.storage_usage_summary(db, user))
    return data


def _decode_schedule_user_ids(raw: str | None) -> list[int]:
    if not raw:
        return []
    try:
        value = json.loads(raw)
    except json.JSONDecodeError:
        value = [item for item in raw.split(",") if item]
    if isinstance(value, int):
        value = [value]
    return [int(item) for item in value if int(item) > 0]


def _remove_user_from_backup_schedules(db: Session, user_id: int) -> None:
    for schedule in db.query(BackupSchedule).all():
        changed = False
        if schedule.user_id == user_id:
            schedule.user_id = None
            changed = True
        user_ids = _decode_schedule_user_ids(schedule.user_ids)
        if user_id in user_ids:
            user_ids = [item for item in user_ids if item != user_id]
            schedule.user_ids = json.dumps(user_ids)
            changed = True
        if changed and not schedule.all_users and schedule.user_id is None and not user_ids:
            db.delete(schedule)


def _delete_owned_website(db: Session, website: Website) -> None:
    db_item = db.query(DatabaseAccount).filter(DatabaseAccount.website_id == website.id).first()
    if db_item:
        mariadb.drop_database(db_item.db_name, db_item.db_user)
    openlitespeed.remove_vhost(website.domain)
    wordpress.delete_wordpress(website.root_path)
    if db_item:
        db.delete(db_item)
    db.delete(website)


@router.post("", response_model=UserOut)
def create_user(payload: UserCreate, request: Request, db: Session = Depends(get_db), current_user: User = Depends(get_current_user)):
    ensure_role(current_user.role, Role.admin)
    # Only the username has to be unique -- one contact email may be shared by
    # several panel users.
    if db.query(User).filter(User.username == payload.username).first():
        raise HTTPException(status_code=409, detail="Username already taken")
    try:
        site_users.ensure_panel_user(payload.username, payload.password)
    except ValueError as exc:
        raise HTTPException(status_code=400, detail=str(exc)) from exc
    except RuntimeError as exc:
        raise HTTPException(status_code=500, detail=str(exc)) from exc
    user = User(
        username=payload.username,
        email=payload.email,
        hashed_password=hash_password(payload.password),
        role=payload.role,
        website_limit=payload.website_limit,
        storage_limit_mb=payload.storage_limit_mb,
    )
    db.add(user)
    db.commit()
    db.refresh(user)
    log_action(db, current_user.id, "create_user", user.username, request=request)
    return _user_out(user, db)


@router.get("", response_model=List[UserOut])
def list_users(db: Session = Depends(get_db), current_user: User = Depends(get_current_user)):
    ensure_role(current_user.role, Role.admin)
    return [_user_out(user, db) for user in db.query(User).order_by(User.id.desc()).all()]


@router.get("/me", response_model=UserOut)
def me(db: Session = Depends(get_db), current_user: User = Depends(get_current_user)):
    return _user_out(current_user, db)

@router.patch("/me", response_model=UserOut)
def update_me(payload: UserUpdate, request: Request, db: Session = Depends(get_db), current_user: User = Depends(get_current_user)):
    """Let the current user update their own email."""
    if payload.email is not None and payload.email != current_user.email:
        current_user.email = payload.email
        db.commit()
        db.refresh(current_user)
    log_action(db, current_user.id, "update_profile_email", current_user.username, request=request)
    return _user_out(current_user, db)


@router.patch("/{user_id}", response_model=UserOut)
def update_user(user_id: int, payload: UserUpdate, request: Request, db: Session = Depends(get_db), current_user: User = Depends(get_current_user)):
    ensure_role(current_user.role, Role.admin)
    user = db.query(User).filter(User.id == user_id).first()
    if not user:
        raise HTTPException(status_code=404, detail="User not found")
    role_changed = False
    if payload.role is not None and payload.role != user.role:
        if user_id == current_user.id:
            raise HTTPException(status_code=400, detail="Cannot change your own role")
        user.role = payload.role
        role_changed = True
    if payload.email is not None and payload.email != user.email:
        user.email = payload.email
    if payload.is_active is not None:
        if user_id == current_user.id and payload.is_active is False:
            raise HTTPException(status_code=400, detail="Cannot deactivate yourself")
        if user.is_active != payload.is_active:
            user.is_active = payload.is_active
            user.token_version = (user.token_version or 0) + 1
    if payload.website_limit is not None:
        user.website_limit = payload.website_limit
    if payload.storage_limit_mb is not None:
        user.storage_limit_mb = payload.storage_limit_mb

    if role_changed:
        # New role -> existing tokens with old role claim should be invalidated.
        user.token_version = (user.token_version or 0) + 1

    db.commit()
    db.refresh(user)
    log_action(db, current_user.id, "update_user", user.username, request=request)
    return _user_out(user, db)


@router.delete("/{user_id}")
def delete_user(user_id: int, request: Request, db: Session = Depends(get_db), current_user: User = Depends(get_current_user)):
    ensure_role(current_user.role, Role.admin)
    user = db.query(User).filter(User.id == user_id).first()
    if not user:
        raise HTTPException(status_code=404, detail="User not found")
    if user.id == current_user.id:
        raise HTTPException(status_code=400, detail="Cannot delete yourself")
    websites = db.query(Website).filter(Website.owner_id == user.id).order_by(Website.id.asc()).all()
    deleted_domains = []
    panel_linux_user = site_users.linux_user_for_panel_username(user.username)
    try:
        for website in websites:
            if website.linux_user and website.linux_user != panel_linux_user:
                raise ValueError(f"Website {website.domain} is not owned by Linux user {panel_linux_user}")
        for website in websites:
            _delete_owned_website(db, website)
            deleted_domains.append(website.domain)
        _remove_user_from_backup_schedules(db, user.id)
        site_users.delete_panel_user(user.username)
    except (RuntimeError, ValueError) as exc:
        raise HTTPException(status_code=400, detail=str(exc)) from exc
    username = user.username
    db.delete(user)
    db.commit()
    log_action(db, current_user.id, "delete_user", username, ",".join(deleted_domains), request=request)
    return {"ok": True, "deleted_websites": deleted_domains}


@router.post("/{user_id}/password")
def update_user_password(user_id: int, payload: UserPasswordUpdate, request: Request, db: Session = Depends(get_db), current_user: User = Depends(get_current_user)):
    if user_id != current_user.id:
        ensure_role(current_user.role, Role.admin)
    else:
        require_sensitive_action_step_up(current_user, payload.current_password, payload.code)
    user = db.query(User).filter(User.id == user_id).first()
    if not user:
        raise HTTPException(status_code=404, detail="User not found")
    try:
        site_users.set_panel_user_password(user.username, payload.password)
    except ValueError as exc:
        raise HTTPException(status_code=400, detail=str(exc)) from exc
    except RuntimeError as exc:
        raise HTTPException(status_code=500, detail=str(exc)) from exc
    user.hashed_password = hash_password(payload.password)
    # Force re-login on all other sessions of this user.
    user.token_version = (user.token_version or 0) + 1
    db.commit()
    log_action(db, current_user.id, "update_user_password", user.username, request=request)
    return {"message": f"Changed password for user {user.username}"}


@router.post("/{user_id}/2fa/reset")
def reset_user_two_factor(user_id: int, request: Request, db: Session = Depends(get_db), current_user: User = Depends(get_current_user)):
    ensure_role(current_user.role, Role.admin)
    user = db.query(User).filter(User.id == user_id).first()
    if not user:
        raise HTTPException(status_code=404, detail="User not found")
    if user_id == current_user.id:
        raise HTTPException(status_code=400, detail="Use the Security page to disable your own 2FA")
    user.totp_enabled = False
    user.totp_secret = None
    user.token_version = (user.token_version or 0) + 1
    db.commit()
    log_action(db, current_user.id, "reset_user_2fa", user.username, request=request)
    return {"message": f"Reset 2FA for user {user.username}"}


@router.get("/audit/log", response_model=List[AuditLogOut])
def list_audit(
    user_id: Optional[int] = Query(default=None),
    action: Optional[str] = Query(default=None, max_length=64),
    limit: int = Query(default=100, ge=1, le=1000),
    offset: int = Query(default=0, ge=0),
    db: Session = Depends(get_db),
    current_user: User = Depends(get_current_user),
):
    ensure_role(current_user.role, Role.admin)
    query = db.query(AuditLog).order_by(AuditLog.id.desc())
    if user_id is not None:
        query = query.filter(AuditLog.user_id == user_id)
    if action:
        query = query.filter(AuditLog.action == action)
    rows = query.offset(offset).limit(limit).all()
    return [AuditLogOut.from_row(row) for row in rows]
