"""Provisioning API - /api/provisioning/v1

Used by WHMCS bpanel module (and any billing system) to manage hosting accounts.
Auth: Bearer API token (not user JWT).

Response format: raw data objects (no envelope).
Error format: {"detail": "message string"} (FastAPI default).
"""

from typing import Any, Dict, List

from fastapi import APIRouter, Depends, HTTPException
from sqlalchemy.orm import Session

from app.api.provisioning_deps import require_provisioning_read, require_provisioning_write
from app.core.database import get_db
from app.models.entities import ApiToken
from app.schemas.schemas import (
    ProvisioningAccountCreate,
    ProvisioningPackageChange,
    ProvisioningPasswordChange,
    ProvisioningPlanOut,
    ProvisioningSuspendRequest,
)
from app.services import provisioning

router = APIRouter(prefix="/provisioning/v1", tags=["provisioning"])


@router.get("/plans")
def list_plans(
    db: Session = Depends(get_db),
    _token: ApiToken = Depends(require_provisioning_read),
) -> List[Dict[str, Any]]:
    plans = provisioning.list_plans(db)
    return [ProvisioningPlanOut.model_validate(p).model_dump() for p in plans]


@router.get("/accounts/{external_id}")
def get_account(
    external_id: str,
    db: Session = Depends(get_db),
    _token: ApiToken = Depends(require_provisioning_read),
) -> Dict[str, Any]:
    account = provisioning.get_account(db, external_id)
    if account is None:
        raise HTTPException(status_code=404, detail="Account %s not found" % external_id)
    return provisioning._account_dict(db, account)


@router.post("/accounts", status_code=201)
def create_account(
    payload: ProvisioningAccountCreate,
    db: Session = Depends(get_db),
    _token: ApiToken = Depends(require_provisioning_write),
) -> Dict[str, Any]:
    try:
        account, is_new = provisioning.create_account(
            db,
            external_id=payload.external_id,
            username=payload.username,
            password=payload.password,
            domain=payload.domain,
            package_id=payload.package_id,
            php_version=payload.php_version,
            app_type=payload.app_type,
            install_wordpress=payload.install_wordpress,
            enable_ssl=payload.enable_ssl,
        )
    except ValueError as exc:
        raise HTTPException(status_code=400, detail=str(exc))
    except RuntimeError as exc:
        raise HTTPException(status_code=500, detail=str(exc))
    return provisioning._account_dict(db, account)


@router.post("/accounts/{external_id}/suspend")
def suspend_account(
    external_id: str,
    payload: ProvisioningSuspendRequest = ProvisioningSuspendRequest(),
    db: Session = Depends(get_db),
    _token: ApiToken = Depends(require_provisioning_write),
) -> Dict[str, Any]:
    try:
        account = provisioning.suspend_account(db, external_id, payload.reason)
    except ValueError as exc:
        raise HTTPException(status_code=404, detail=str(exc))
    return provisioning._account_dict(db, account)


@router.post("/accounts/{external_id}/unsuspend")
def unsuspend_account(
    external_id: str,
    db: Session = Depends(get_db),
    _token: ApiToken = Depends(require_provisioning_write),
) -> Dict[str, Any]:
    try:
        account = provisioning.unsuspend_account(db, external_id)
    except ValueError as exc:
        raise HTTPException(status_code=404, detail=str(exc))
    return provisioning._account_dict(db, account)


@router.delete("/accounts/{external_id}")
def terminate_account(
    external_id: str,
    db: Session = Depends(get_db),
    _token: ApiToken = Depends(require_provisioning_write),
) -> Dict[str, Any]:
    try:
        provisioning.terminate_account(db, external_id)
    except ValueError as exc:
        raise HTTPException(status_code=404, detail=str(exc))
    return {"terminated": True}


@router.patch("/accounts/{external_id}/password")
def change_password(
    external_id: str,
    payload: ProvisioningPasswordChange,
    db: Session = Depends(get_db),
    _token: ApiToken = Depends(require_provisioning_write),
) -> Dict[str, Any]:
    try:
        provisioning.change_password(db, external_id, payload.password)
    except ValueError as exc:
        raise HTTPException(status_code=404, detail=str(exc))
    return {"changed": True}


@router.patch("/accounts/{external_id}/package")
def change_package(
    external_id: str,
    payload: ProvisioningPackageChange,
    db: Session = Depends(get_db),
    _token: ApiToken = Depends(require_provisioning_write),
) -> Dict[str, Any]:
    try:
        account = provisioning.change_package(db, external_id, payload.package_id)
    except ValueError as exc:
        raise HTTPException(status_code=400, detail=str(exc))
    return provisioning._account_dict(db, account)


@router.get("/accounts/{external_id}/usage")
def get_usage(
    external_id: str,
    db: Session = Depends(get_db),
    _token: ApiToken = Depends(require_provisioning_read),
) -> Dict[str, Any]:
    account = provisioning.get_account(db, external_id)
    if account is None:
        raise HTTPException(status_code=404, detail="Account %s not found" % external_id)
    return provisioning._usage_dict(db, account)


@router.post("/accounts/{external_id}/login")
def create_login(
    external_id: str,
    db: Session = Depends(get_db),
    _token: ApiToken = Depends(require_provisioning_write),
) -> Dict[str, Any]:
    from app.core.config import settings

    panel_url = settings.panel_url or "https://%s:%s" % (settings.panel_domain, settings.panel_port)
    try:
        return provisioning.create_sso_login(db, external_id, panel_url)
    except ValueError as exc:
        raise HTTPException(status_code=404, detail=str(exc))
