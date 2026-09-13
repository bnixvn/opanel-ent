from pathlib import Path
from tempfile import NamedTemporaryFile
from fastapi import APIRouter, Depends, HTTPException, Request
from fastapi.responses import FileResponse, JSONResponse
from starlette.background import BackgroundTask
from sqlalchemy.exc import SQLAlchemyError
from sqlalchemy.orm import Session
from hmac import compare_digest
from typing import List
import ipaddress
import logging

from app.api.deps import get_current_user
from app.core.config import settings
from app.core.database import get_db
from app.core.permissions import Role, ensure_role, is_admin_role
from app.core.secrets import decrypt, encrypt
from app.models.entities import DatabaseAccount, User, Website
from app.schemas.schemas import DatabaseCreate, DatabaseCreatedOut, DatabaseOut, DatabasePasswordUpdate
from app.services import mariadb, panel_urls
from app.services.audit import log_action
from app.services.sso_tokens import consume_phpmyadmin_token, create_phpmyadmin_token

router = APIRouter(prefix="/databases", tags=["databases"])

logger = logging.getLogger("opanel-ent.databases")


def _is_loopback_peer(request: Request) -> bool:
    """Return True if the immediate TCP peer is loopback.

    With ``--forwarded-allow-ips 127.0.0.1`` (see installer/install.sh) any
    X-Forwarded-For from a non-trusted peer is dropped, so request.client.host
    is the real connecting address.
    """
    if not request.client:
        return False
    try:
        return ipaddress.ip_address(request.client.host).is_loopback
    except ValueError:
        return False


def get_accessible_database(database_id: int, db: Session, current_user: User) -> DatabaseAccount:
    item = db.query(DatabaseAccount).filter(DatabaseAccount.id == database_id).first()
    if not item:
        raise HTTPException(status_code=404, detail="Database not found")
    if item.owner_id != current_user.id:
        ensure_role(current_user.role, Role.admin)
    return item


@router.get("", response_model=List[DatabaseOut])
def list_databases(db: Session = Depends(get_db), current_user: User = Depends(get_current_user)):
    query = db.query(DatabaseAccount)
    if not is_admin_role(current_user.role):
        query = query.filter(DatabaseAccount.owner_id == current_user.id)
    return query.order_by(DatabaseAccount.id.desc()).all()


@router.post("", response_model=DatabaseCreatedOut)
def create_database(payload: DatabaseCreate, db: Session = Depends(get_db), current_user: User = Depends(get_current_user)):
    db_name = payload.db_name
    db_user = payload.db_user or db_name
    db_password = payload.db_password or mariadb.random_password()

    if db.query(DatabaseAccount).filter(DatabaseAccount.db_name == db_name).first():
        raise HTTPException(status_code=409, detail="Database name already exists")
    if db.query(DatabaseAccount).filter(DatabaseAccount.db_user == db_user).first():
        raise HTTPException(status_code=409, detail="Database user already exists")
    # The panel table only knows what the panel made. Ask MariaDB as well, or a
    # name it already holds would be silently taken over.
    in_use = mariadb.identifier_in_use(db_name, db_user)
    if in_use:
        raise HTTPException(status_code=409, detail=in_use)

    try:
        mariadb.create_database_credentials(db_name, db_user, db_password)
    except ValueError as exc:
        raise HTTPException(status_code=400, detail=str(exc)) from exc
    except Exception as exc:
        logger.exception("Failed to create MariaDB database/user")
        raise HTTPException(status_code=500, detail=f"MariaDB error: {exc}") from exc

    item = DatabaseAccount(
        owner_id=current_user.id,
        db_name=db_name,
        db_user=db_user,
        db_password=encrypt(db_password),
    )
    db.add(item)
    db.commit()
    db.refresh(item)
    return DatabaseCreatedOut(
        id=item.id,
        owner_id=item.owner_id,
        website_id=item.website_id,
        db_name=item.db_name,
        db_user=item.db_user,
        db_password=db_password,
    )


@router.delete("/{database_id}")
def delete_database_record(database_id: int, db: Session = Depends(get_db), current_user: User = Depends(get_current_user)):
    item = get_accessible_database(database_id, db, current_user)
    try:
        mariadb.drop_database(item.db_name, item.db_user)
    except Exception as exc:
        logger.exception("Failed to delete MariaDB database/user")
        raise HTTPException(status_code=500, detail=f"MariaDB error: {exc}") from exc
    try:
        db.delete(item)
        db.commit()
    except SQLAlchemyError as exc:
        db.rollback()
        logger.exception("Failed to delete database record")
        raise HTTPException(status_code=500, detail="Panel database error") from exc
    return {"ok": True}


@router.get("/{database_id}/download")
def download_database(database_id: int, db: Session = Depends(get_db), current_user: User = Depends(get_current_user)):
    item = get_accessible_database(database_id, db, current_user)
    temp_path = None
    try:
        temp_file = NamedTemporaryFile(prefix=f"{item.db_name}-", suffix=".sql", delete=False)
        temp_file.close()
        temp_path = Path(temp_file.name)
        mariadb.export_database(item.db_name, str(temp_path))
    except ValueError as exc:
        raise HTTPException(status_code=400, detail=str(exc)) from exc
    return FileResponse(
        temp_path,
        filename=f"{item.db_name}.sql",
        media_type="application/sql",
        background=BackgroundTask(lambda path: path.unlink(missing_ok=True), temp_path),
    )


@router.get("/phpmyadmin-sso/{token}")
def consume_phpmyadmin_sso(token: str, request: Request):
    """Consume a one-shot phpMyAdmin SSO token.

    Security model:
      * 256-bit token entropy (secrets.token_urlsafe(32)).
      * One-shot (file removed on read), TTL 60 seconds.
      * Requires a shared secret header (X-OPanel Enterprise-Signon-Secret) that is
        generated at install time and injected into the phpMyAdmin signon
        script. This is more robust than IP-based loopback checking because
        it cannot be bypassed via X-Forwarded-For spoofing.
      * Falls back to loopback peer check when no secret is configured
        (backward compatibility for installs that predate this feature).
    """
    if settings.pma_signon_secret:
        provided = request.headers.get("x-opanel-ent-signon-secret", "")
        if not provided or not compare_digest(provided, settings.pma_signon_secret):
            raise HTTPException(status_code=404, detail="Invalid or expired token")
    elif not _is_loopback_peer(request):
        peer = request.client.host if request.client else "unknown"
        logger.warning("phpmyadmin-sso non-loopback access attempt from %s", peer)
        raise HTTPException(status_code=404, detail="Invalid or expired token")
    data = consume_phpmyadmin_token(token)
    if not data:
        raise HTTPException(status_code=404, detail="Invalid or expired token")
    return JSONResponse(data, headers={"Cache-Control": "no-store"})


@router.post("/{database_id}/phpmyadmin-sso")
def create_phpmyadmin_sso(database_id: int, request: Request, db: Session = Depends(get_db), current_user: User = Depends(get_current_user)):
    item = get_accessible_database(database_id, db, current_user)
    try:
        db_password = decrypt(item.db_password)
    except RuntimeError:
        raise HTTPException(
            status_code=500,
            detail="Failed to access stored database password; please re-save the password in panel settings",
        )
    token = create_phpmyadmin_token(item.db_user, db_password, item.db_name)
    log_action(
        db,
        current_user.id,
        "phpmyadmin_sso",
        f"db={item.db_name}",
        request=request,
    )
    return {"url": f"{panel_urls.tools_base_url(request)}/phpmyadmin/opanel-ent-signon.php?opanel_ent_sso={token}"}


@router.post("/{database_id}/password")
def change_database_password(database_id: int, payload: DatabasePasswordUpdate, db: Session = Depends(get_db), current_user: User = Depends(get_current_user)):
    item = get_accessible_database(database_id, db, current_user)
    mariadb.change_database_password(item.db_user, payload.password)
    item.db_password = encrypt(payload.password)
    db.commit()
    return {"ok": True, "db_user": item.db_user}


# ---------------------------------------------------------------------------
# MariaDB auto-tuning
# ---------------------------------------------------------------------------
@router.get("/mariadb/tuning")
def get_mariadb_tuning(current_user: User = Depends(get_current_user)):
    """Return current OPanel Enterprise MariaDB config + hardware recommendation."""
    ensure_role(current_user.role, Role.admin)
    current = mariadb.read_mariadb_tuning()
    recommendation = mariadb.recommend_mariadb_config()
    return {"current": current, "recommendation": recommendation}


@router.post("/mariadb/tuning")
def apply_mariadb_tuning(db: Session = Depends(get_db), current_user: User = Depends(get_current_user)):
    """Auto-tune MariaDB for the current hardware and restart."""
    ensure_role(current_user.role, Role.admin)
    result = mariadb.apply_mariadb_tuning()
    log_action(db, current_user.id, "mariadb_tuning_apply",
               f"pool={result['innodb_buffer_pool_size']} conn={result['max_connections']}")
    return result
