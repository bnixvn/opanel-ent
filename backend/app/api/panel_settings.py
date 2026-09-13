from fastapi import APIRouter, Depends, File, HTTPException, Request, UploadFile
from sqlalchemy.orm import Session

from app.api.deps import get_current_user
from app.core.database import get_db
from app.core.permissions import Role, ensure_role
from app.models.entities import User
from app.schemas.schemas import (
    MalwareScanJob,
    MalwareScanJobsOut,
    MalwareScanRun,
    MalwareScanSchedulesIn,
    MalwareScanSchedulesOut,
    MalwareScanStatus,
    QuarantineList,
    QuarantineRequest,
    MalwareScanToggle,
    NetworkStatusOut,
    Ipv6Toggle,
    PanelSettingsOut,
    PanelSettingsUpdate,
    PanelSslInstall,
)
from app.services import panel_settings
from app.services.audit import log_action


router = APIRouter(prefix="/panel-settings", tags=["panel-settings"])


@router.get("/public", response_model=PanelSettingsOut)
def public_panel_settings():
    return panel_settings.current_settings()


@router.get("", response_model=PanelSettingsOut)
def get_panel_settings(current_user: User = Depends(get_current_user)):
    ensure_role(current_user.role, Role.admin)
    return panel_settings.current_settings()


@router.patch("", response_model=PanelSettingsOut)
def update_panel_settings(
    payload: PanelSettingsUpdate,
    request: Request,
    db: Session = Depends(get_db),
    current_user: User = Depends(get_current_user),
):
    ensure_role(current_user.role, Role.admin)
    try:
        result = panel_settings.update_settings(
            payload.app_name,
            payload.panel_hostname,
            payload.panel_port,
            payload.panel_url,
        )
    except ValueError as exc:
        raise HTTPException(status_code=400, detail=str(exc)) from exc
    except RuntimeError as exc:
        raise HTTPException(status_code=500, detail=str(exc)) from exc
    log_action(db, current_user.id, "update_panel_settings", result.get("panel_url") or "panel", request=request)
    return result


@router.post("/logo", response_model=PanelSettingsOut)
async def upload_logo(
    request: Request,
    file: UploadFile = File(...),
    db: Session = Depends(get_db),
    current_user: User = Depends(get_current_user),
):
    ensure_role(current_user.role, Role.admin)
    try:
        result = await panel_settings.save_asset("logo", file)
    except ValueError as exc:
        raise HTTPException(status_code=400, detail=str(exc)) from exc
    log_action(db, current_user.id, "upload_panel_logo", "panel", request=request)
    return result


@router.post("/favicon", response_model=PanelSettingsOut)
async def upload_favicon(
    request: Request,
    file: UploadFile = File(...),
    db: Session = Depends(get_db),
    current_user: User = Depends(get_current_user),
):
    ensure_role(current_user.role, Role.admin)
    try:
        result = await panel_settings.save_asset("favicon", file)
    except ValueError as exc:
        raise HTTPException(status_code=400, detail=str(exc)) from exc
    log_action(db, current_user.id, "upload_panel_favicon", "panel", request=request)
    return result


@router.post("/ssl", response_model=PanelSettingsOut)
def install_panel_ssl(
    payload: PanelSslInstall,
    request: Request,
    db: Session = Depends(get_db),
    current_user: User = Depends(get_current_user),
):
    ensure_role(current_user.role, Role.admin)
    try:
        result = panel_settings.install_panel_ssl(
            str(payload.email or ""),
            panel_hostname=payload.panel_hostname,
            panel_port=payload.panel_port,
            panel_url=payload.panel_url,
        )
    except ValueError as exc:
        raise HTTPException(status_code=400, detail=str(exc)) from exc
    except RuntimeError as exc:
        raise HTTPException(status_code=500, detail=str(exc)) from exc
    log_action(db, current_user.id, "install_panel_ssl", result.get("panel_url") or payload.panel_hostname or "panel", request=request)
    return result


@router.get("/network", response_model=NetworkStatusOut)
def get_network_status(current_user: User = Depends(get_current_user)):
    ensure_role(current_user.role, Role.admin)
    return panel_settings.network_status()


@router.post("/network/ipv6", response_model=NetworkStatusOut)
def toggle_ipv6(
    payload: Ipv6Toggle,
    request: Request,
    db: Session = Depends(get_db),
    current_user: User = Depends(get_current_user),
):
    ensure_role(current_user.role, Role.admin)
    try:
        result = panel_settings.set_ipv6(payload.enabled)
    except ValueError as exc:
        raise HTTPException(status_code=400, detail=str(exc)) from exc
    except RuntimeError as exc:
        raise HTTPException(status_code=500, detail=str(exc)) from exc
    log_action(db, current_user.id, "enable_ipv6" if payload.enabled else "disable_ipv6", "network", request=request)
    return result


@router.get("/malware-scan", response_model=MalwareScanStatus)
def get_malware_scan_status(current_user: User = Depends(get_current_user)):
    ensure_role(current_user.role, Role.admin)
    return panel_settings.malware_scan_status()


@router.post("/malware-scan/toggle", response_model=PanelSettingsOut)
def toggle_malware_scan(
    payload: MalwareScanToggle,
    request: Request,
    db: Session = Depends(get_db),
    current_user: User = Depends(get_current_user),
):
    ensure_role(current_user.role, Role.admin)
    try:
        result = panel_settings.set_malware_scan(payload.enabled)
    except RuntimeError as exc:
        raise HTTPException(status_code=500, detail=str(exc)) from exc
    action = "enable_malware_scan" if payload.enabled else "disable_malware_scan"
    log_action(db, current_user.id, action, "malware-scan", request=request)
    return result


@router.post("/malware-scan/auto-quarantine", response_model=PanelSettingsOut)
def toggle_malware_auto_quarantine(
    payload: MalwareScanToggle,
    request: Request,
    db: Session = Depends(get_db),
    current_user: User = Depends(get_current_user),
):
    ensure_role(current_user.role, Role.admin)
    result = panel_settings.set_malware_auto_quarantine(payload.enabled)
    action = "enable_malware_auto_quarantine" if payload.enabled else "disable_malware_auto_quarantine"
    log_action(db, current_user.id, action, "malware-scan", request=request)
    return result


@router.get("/malware-scan/quarantine", response_model=QuarantineList)
def list_quarantine(current_user: User = Depends(get_current_user)):
    ensure_role(current_user.role, Role.admin)
    from app.services import malware_scan

    return {"entries": malware_scan.list_quarantine()}


@router.post("/malware-scan/quarantine", response_model=QuarantineList)
def quarantine_a_file(
    payload: QuarantineRequest,
    request: Request,
    db: Session = Depends(get_db),
    current_user: User = Depends(get_current_user),
):
    ensure_role(current_user.role, Role.admin)
    from app.services import malware_scan

    try:
        malware_scan.quarantine_file(payload.path, payload.signature)
    except RuntimeError as exc:
        raise HTTPException(status_code=400, detail=str(exc)) from exc
    log_action(db, current_user.id, "malware_quarantine_add", payload.path, request=request)
    return {"entries": malware_scan.list_quarantine()}


@router.post("/malware-scan/quarantine/{quarantine_id}/restore", response_model=QuarantineList)
def restore_from_quarantine(
    quarantine_id: str,
    request: Request,
    db: Session = Depends(get_db),
    current_user: User = Depends(get_current_user),
):
    ensure_role(current_user.role, Role.admin)
    from app.services import malware_scan

    try:
        malware_scan.restore_quarantine(quarantine_id)
    except RuntimeError as exc:
        raise HTTPException(status_code=400, detail=str(exc)) from exc
    log_action(db, current_user.id, "malware_quarantine_restore", quarantine_id, request=request)
    return {"entries": malware_scan.list_quarantine()}


@router.delete("/malware-scan/quarantine/{quarantine_id}", response_model=QuarantineList)
def delete_from_quarantine(
    quarantine_id: str,
    request: Request,
    db: Session = Depends(get_db),
    current_user: User = Depends(get_current_user),
):
    ensure_role(current_user.role, Role.admin)
    from app.services import malware_scan

    try:
        malware_scan.drop_quarantine(quarantine_id)
    except RuntimeError as exc:
        raise HTTPException(status_code=400, detail=str(exc)) from exc
    log_action(db, current_user.id, "malware_quarantine_delete", quarantine_id, request=request)
    return {"entries": malware_scan.list_quarantine()}


@router.post("/malware-scan/install-lmd", response_model=PanelSettingsOut)
def install_linux_malware_detect(
    request: Request,
    db: Session = Depends(get_db),
    current_user: User = Depends(get_current_user),
):
    """Add Linux Malware Detect to a box that already runs ClamAV but was set up
    before LMD was bundled. Fresh installs get LMD with ClamAV automatically."""
    ensure_role(current_user.role, Role.admin)
    try:
        result = panel_settings.add_linux_malware_detect()
    except ValueError as exc:
        raise HTTPException(status_code=400, detail=str(exc)) from exc
    except RuntimeError as exc:
        raise HTTPException(status_code=500, detail=str(exc)) from exc
    log_action(db, current_user.id, "install_linux_malware_detect", "malware-scan", request=request)
    return result


@router.post("/malware-scan/realtime", response_model=PanelSettingsOut)
def toggle_malware_realtime(
    payload: MalwareScanToggle,
    request: Request,
    db: Session = Depends(get_db),
    current_user: User = Depends(get_current_user),
):
    ensure_role(current_user.role, Role.admin)
    try:
        result = panel_settings.set_malware_realtime(payload.enabled)
    except ValueError as exc:
        raise HTTPException(status_code=400, detail=str(exc)) from exc
    action = "enable_malware_realtime" if payload.enabled else "disable_malware_realtime"
    log_action(db, current_user.id, action, "malware-scan", request=request)
    return result


@router.post("/malware-scan/update-signatures")
def update_malware_signatures(
    request: Request,
    db: Session = Depends(get_db),
    current_user: User = Depends(get_current_user),
):
    ensure_role(current_user.role, Role.admin)
    from app.services import malware_scan

    detail = malware_scan.update_signatures()
    log_action(db, current_user.id, "malware_signatures_update", "malware-scan", request=request)
    return {"detail": detail}


@router.post("/malware-scan/run", response_model=MalwareScanJob)
def run_malware_scan(
    payload: MalwareScanRun,
    request: Request,
    db: Session = Depends(get_db),
    current_user: User = Depends(get_current_user),
):
    ensure_role(current_user.role, Role.admin)
    scope = (payload.scope or "").strip().lower()
    try:
        if scope == "system":
            result = panel_settings.start_system_scan_job(payload.scan_root or "/")
        else:
            if not payload.all and payload.website_id is None:
                raise ValueError("Website is required")
            result = panel_settings.start_scan_job(None if payload.all else payload.website_id, db)
    except ValueError as exc:
        raise HTTPException(status_code=400, detail=str(exc)) from exc
    except RuntimeError as exc:
        raise HTTPException(status_code=503, detail=str(exc)) from exc
    target = "system" if scope == "system" else ("all" if payload.all else str(payload.website_id))
    log_action(db, current_user.id, "malware_scan_run", target, request=request)
    return result


@router.get("/malware-scan/schedule", response_model=MalwareScanSchedulesOut)
def get_malware_scan_schedule(current_user: User = Depends(get_current_user)):
    ensure_role(current_user.role, Role.admin)
    return panel_settings.malware_schedules()


@router.put("/malware-scan/schedule", response_model=MalwareScanSchedulesOut)
def save_malware_scan_schedule(
    payload: MalwareScanSchedulesIn,
    request: Request,
    db: Session = Depends(get_db),
    current_user: User = Depends(get_current_user),
):
    ensure_role(current_user.role, Role.admin)
    sent = payload.model_dump(exclude_none=True)
    if not sent:
        raise HTTPException(status_code=400, detail="No schedule to save")
    try:
        result = panel_settings.set_malware_schedules(sent)
    except ValueError as exc:
        raise HTTPException(status_code=400, detail=str(exc)) from exc
    for scope, entry in sent.items():
        action = "malware_scan_schedule_enable" if entry.get("enabled") else "malware_scan_schedule_disable"
        log_action(db, current_user.id, action, panel_settings._scope_scan_root(scope), request=request)
    return result


@router.get("/malware-scan/jobs", response_model=MalwareScanJobsOut)
def list_malware_scan_jobs(current_user: User = Depends(get_current_user)):
    ensure_role(current_user.role, Role.admin)
    return {"jobs": panel_settings.list_malware_scan_jobs()}


@router.get("/malware-scan/jobs/latest", response_model=MalwareScanJob)
def get_latest_malware_scan_job(current_user: User = Depends(get_current_user)):
    ensure_role(current_user.role, Role.admin)
    try:
        return panel_settings.get_latest_malware_scan_job()
    except ValueError as exc:
        raise HTTPException(status_code=404, detail=str(exc)) from exc


@router.get("/malware-scan/jobs/{job_id}", response_model=MalwareScanJob)
def get_malware_scan_job(job_id: str, current_user: User = Depends(get_current_user)):
    ensure_role(current_user.role, Role.admin)
    try:
        return panel_settings.get_malware_scan_job(job_id)
    except ValueError as exc:
        raise HTTPException(status_code=404, detail=str(exc)) from exc


@router.post("/malware-scan/start")
def start_malware_scan_daemon(
    request: Request,
    db: Session = Depends(get_db),
    current_user: User = Depends(get_current_user),
):
    ensure_role(current_user.role, Role.admin)
    from app.services import malware_scan

    ok = malware_scan.start_clamd()
    if not ok:
        raise HTTPException(status_code=500, detail="Failed to start ClamAV daemon")
    log_action(db, current_user.id, "clamav_start", "malware-scan", request=request)
    return {"message": "ClamAV daemon started successfully"}
