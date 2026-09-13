import json
import logging
import threading
import tarfile
import uuid
import zipfile
from concurrent.futures import ThreadPoolExecutor
from datetime import datetime
from pathlib import Path

from fastapi import APIRouter, Depends, File, HTTPException, Query, Request, UploadFile, status
from fastapi.responses import FileResponse
from pydantic import BaseModel
from sqlalchemy.orm import Session

from app.api.deps import get_current_user
from app.core.database import SessionLocal, get_db
from app.core.permissions import Role, ensure_role, is_admin_role
from app.core.secrets import decrypt, encrypt
from app.models.entities import BackupSchedule, DatabaseAccount, SftpBackupTarget, User, Website
from app.schemas.schemas import (
    BackupScheduleCreate,
    BackupScheduleOut,
    BackupCreate,
    CronCreate,
    CronDelete,
    PhpConfigUpdate,
    PhpConfigRestore,
    RestoreBackup,
    SftpBackupRun,
    SftpBackupTargetCreate,
    SftpBackupTargetOut,
    UserBackupCreate,
    UserRestoreBackup,
    WpAction,
)
from app.services import backup, cron, da_import, file_manager, mariadb, openlitespeed, php, site_users, storage_quota, wordpress
from app.services.audit import log_action

router = APIRouter(prefix="/maintenance", tags=["maintenance"])
logger = logging.getLogger(__name__)


FILE_JOB_LIMIT = 50
_file_job_executor = ThreadPoolExecutor(max_workers=2, thread_name_prefix="opanel-ent-file-job")
_file_jobs: dict[str, dict] = {}
_file_jobs_lock = threading.Lock()

BACKUP_JOB_LIMIT = 50
_backup_job_executor = ThreadPoolExecutor(max_workers=2, thread_name_prefix="opanel-ent-backup-job")
_backup_jobs: dict[str, dict] = {}
_backup_jobs_lock = threading.Lock()


class FileWrite(BaseModel):
    website_id: int
    path: str
    content: str


class FileMkdir(BaseModel):
    website_id: int
    path: str = site_users.PUBLIC_DIR
    name: str


class FileCreate(BaseModel):
    website_id: int
    path: str = ""
    name: str


class FileRename(BaseModel):
    website_id: int
    path: str
    new_name: str


class FileChmod(BaseModel):
    website_id: int
    path: str
    mode: str


class FileBulkDelete(BaseModel):
    website_id: int
    paths: list[str]


class FileTransfer(BaseModel):
    website_id: int
    paths: list[str]
    destination_path: str = site_users.PUBLIC_DIR


class FileArchive(BaseModel):
    website_id: int
    base_path: str = site_users.PUBLIC_DIR
    paths: list[str]
    output_name: str = ""
    format: str = "zip"


class FileExtract(BaseModel):
    website_id: int
    archive_path: str
    destination_path: str = ""


def _now_iso() -> str:
    return datetime.utcnow().isoformat(timespec="seconds") + "Z"


def _public_file_job(job: dict) -> dict:
    return {
        "job_id": job["job_id"],
        "kind": job["kind"],
        "status": job["status"],
        "website_id": job["website_id"],
        "archive_path": job.get("archive_path", ""),
        "destination_path": job.get("destination_path", ""),
        "target": job.get("target", ""),
        "message": job.get("message", ""),
        "error": job.get("error", ""),
        "created_at": job.get("created_at", ""),
        "started_at": job.get("started_at", ""),
        "finished_at": job.get("finished_at", ""),
    }


def _set_file_job(job_id: str, **updates) -> None:
    with _file_jobs_lock:
        job = _file_jobs.get(job_id)
        if not job:
            return
        job.update(updates)


def _remember_file_job(job: dict) -> dict:
    with _file_jobs_lock:
        _file_jobs[job["job_id"]] = job
        if len(_file_jobs) > FILE_JOB_LIMIT:
            # Find oldest completed jobs to remove (O(n) instead of O(n log n))
            to_remove = len(_file_jobs) - FILE_JOB_LIMIT
            removable = [
                (job.get("created_at", ""), job_id)
                for job_id, job in _file_jobs.items()
                if job.get("status") not in {"queued", "running"}
            ]
            # Sort only the removable jobs, take the oldest to_remove items
            removable.sort(key=lambda x: x[0])
            for _, job_id in removable[:to_remove]:
                _file_jobs.pop(job_id, None)
    return _public_file_job(job)


def _get_file_job(job_id: str) -> dict | None:
    with _file_jobs_lock:
        job = _file_jobs.get(job_id)
        return dict(job) if job else None


def _list_file_jobs(current_user: User, website_id: int | None = None) -> list[dict]:
    with _file_jobs_lock:
        jobs = [dict(job) for job in _file_jobs.values()]
    visible = []
    for job in jobs:
        if job.get("user_id") != current_user.id and not is_admin_role(current_user.role):
            continue
        if website_id is not None and job.get("website_id") != website_id:
            continue
        visible.append(_public_file_job(job))
    return sorted(visible, key=lambda item: item.get("created_at", ""), reverse=True)[:10]


def _public_backup_job(job: dict) -> dict:
    return {
        "job_id": job["job_id"],
        "kind": job["kind"],
        "status": job["status"],
        "website_id": job.get("website_id"),
        "user_id": job.get("target_user_id"),
        "target_id": job.get("target_id"),
        "backup_file": job.get("backup_file", ""),
        "remote_file": job.get("remote_file", ""),
        "target": job.get("target", ""),
        "message": job.get("message", ""),
        "error": job.get("error", ""),
        "created_at": job.get("created_at", ""),
        "started_at": job.get("started_at", ""),
        "finished_at": job.get("finished_at", ""),
    }


def _set_backup_job(job_id: str, **updates) -> None:
    with _backup_jobs_lock:
        job = _backup_jobs.get(job_id)
        if not job:
            return
        job.update(updates)


def _remember_backup_job(job: dict) -> dict:
    with _backup_jobs_lock:
        _backup_jobs[job["job_id"]] = job
        if len(_backup_jobs) > BACKUP_JOB_LIMIT:
            to_remove = len(_backup_jobs) - BACKUP_JOB_LIMIT
            removable = [
                (job.get("created_at", ""), job_id)
                for job_id, job in _backup_jobs.items()
                if job.get("status") not in {"queued", "running"}
            ]
            removable.sort(key=lambda x: x[0])
            for _, job_id in removable[:to_remove]:
                _backup_jobs.pop(job_id, None)
    return _public_backup_job(job)


def _get_backup_job(job_id: str) -> dict | None:
    with _backup_jobs_lock:
        job = _backup_jobs.get(job_id)
        return dict(job) if job else None


def _list_backup_jobs(current_user: User) -> list[dict]:
    with _backup_jobs_lock:
        jobs = [dict(job) for job in _backup_jobs.values()]
    visible = []
    for job in jobs:
        if job.get("request_user_id") != current_user.id and not is_admin_role(current_user.role):
            continue
        visible.append(_public_backup_job(job))
    return sorted(visible, key=lambda item: item.get("created_at", ""), reverse=True)[:12]


def _queue_backup_job(current_user: User, kind: str, message: str, **extra) -> dict:
    job = {
        "job_id": uuid.uuid4().hex,
        "kind": kind,
        "status": "queued",
        "request_user_id": current_user.id,
        "message": message,
        "error": "",
        "backup_file": "",
        "remote_file": "",
        "target": "",
        "created_at": _now_iso(),
        "started_at": "",
        "finished_at": "",
        **extra,
    }
    return _remember_backup_job(job)



def _run_extract_job(job_id: str, user_id: int, website_id: int, archive_path: str, destination_path: str, allow_executable: bool) -> None:
    _set_file_job(job_id, status="running", started_at=_now_iso(), message="Extracting archive")
    db = SessionLocal()
    try:
        user = db.query(User).filter(User.id == user_id).first()
        website = db.query(Website).filter(Website.id == website_id).first()
        if not user or not user.is_active:
            raise ValueError("User not found")
        if not website:
            raise ValueError("Website not found")
        if website.owner_id != user.id and not is_admin_role(user.role):
            raise ValueError("Access denied")
        target = file_manager.extract_archive(
            website,
            archive_path,
            destination_path,
            allow_executable,
            quota_check=_quota_check_for_website(db, website),
        )
        log_action(db, user.id, "extract_archive", website.domain, archive_path)
        _set_file_job(
            job_id,
            status="done",
            target=target,
            message="Extraction completed",
            finished_at=_now_iso(),
        )
    except Exception as exc:
        db.rollback()
        logger.exception("File extract job failed: job_id=%s website_id=%s", job_id, website_id)
        _set_file_job(
            job_id,
            status="error",
            error=str(exc),
            message="Extraction failed",
            finished_at=_now_iso(),
        )
    finally:
        db.close()


def _queue_extract_job(user: User, website: Website, archive_path: str, destination_path: str, allow_executable: bool) -> dict:
    job_id = uuid.uuid4().hex
    job = {
        "job_id": job_id,
        "kind": "extract_archive",
        "status": "queued",
        "user_id": user.id,
        "website_id": website.id,
        "archive_path": archive_path,
        "destination_path": destination_path,
        "target": "",
        "message": "Extraction queued",
        "error": "",
        "created_at": _now_iso(),
        "started_at": "",
        "finished_at": "",
    }
    public_job = _remember_file_job(job)
    _file_job_executor.submit(
        _run_extract_job,
        job_id,
        user.id,
        website.id,
        archive_path,
        destination_path,
        allow_executable,
    )
    return public_job


def get_owned_website(db: Session, current_user: User, website_id: int) -> Website:
    website = db.query(Website).filter(Website.id == website_id).first()
    if not website:
        raise HTTPException(status_code=404, detail="Website not found")
    if website.owner_id != current_user.id:
        ensure_role(current_user.role, Role.admin)
    return website


def get_backup_user(db: Session, current_user: User, user_id: int) -> User:
    user = db.query(User).filter(User.id == user_id).first()
    if not user:
        raise HTTPException(status_code=404, detail="User not found")
    if user.id != current_user.id:
        ensure_role(current_user.role, Role.admin)
    return user


def upload_archive_to_target(db: Session, target_id: int, archive: str) -> tuple[str, str]:
    target = db.query(SftpBackupTarget).filter(SftpBackupTarget.id == target_id).first()
    if not target or not target.is_active:
        raise HTTPException(status_code=404, detail="SFTP target not found")
    try:
        result = backup.upload_to_sftp(
            archive,
            host=target.host,
            port=target.port,
            username=target.username,
            password=decrypt(target.password) if target.password else None,
            private_key=decrypt(target.private_key) if target.private_key else None,
            remote_path=target.remote_path,
            expected_host_key_type=target.host_key_type,
            expected_host_key_fingerprint=target.host_key_fingerprint,
        )
    except backup.SftpHostKeyMismatch as exc:
        raise HTTPException(status_code=409, detail=str(exc)) from exc
    except Exception as exc:
        raise HTTPException(status_code=500, detail=str(exc)) from exc
    if not target.host_key_fingerprint and result.get("host_key_fingerprint"):
        target.host_key_type = result["host_key_type"]
        target.host_key_fingerprint = result["host_key_fingerprint"]
        db.commit()
    return target.name, result["remote_file"]


def _run_site_backup_job(job_id: str, request_user_id: int, website_id: int) -> None:
    _set_backup_job(job_id, status="running", started_at=_now_iso(), message="Creating website backup")
    db = SessionLocal()
    try:
        user = db.query(User).filter(User.id == request_user_id).first()
        if not user or not user.is_active:
            raise ValueError("User not found")
        website = get_owned_website(db, user, website_id)
        db_item = db.query(DatabaseAccount).filter(DatabaseAccount.website_id == website.id).first()
        archive = backup.create_backup(website, db_item.db_name if db_item else None)
        log_action(db, user.id, "backup", website.domain, archive)
        _set_backup_job(
            job_id,
            status="done",
            backup_file=archive,
            message="Website backup completed",
            finished_at=_now_iso(),
        )
    except Exception as exc:
        db.rollback()
        logger.exception("Website backup job failed: job_id=%s website_id=%s", job_id, website_id)
        _set_backup_job(job_id, status="error", error=str(exc), message="Website backup failed", finished_at=_now_iso())
    finally:
        db.close()


def _run_user_backup_job(job_id: str, request_user_id: int, target_user_id: int, target_id: int | None) -> None:
    _set_backup_job(job_id, status="running", started_at=_now_iso(), message="Creating full user backup")
    db = SessionLocal()
    try:
        request_user = db.query(User).filter(User.id == request_user_id).first()
        if not request_user or not request_user.is_active:
            raise ValueError("User not found")
        user = get_backup_user(db, request_user, target_user_id)
        archive = backup.create_user_backup(user, db)
        remote_file = ""
        target_name = ""
        if target_id:
            ensure_role(request_user.role, Role.admin)
            target_name, remote_file = upload_archive_to_target(db, target_id, archive)
        detail = f"{archive}" + (f" -> {target_name}:{remote_file}" if remote_file else "")
        log_action(db, request_user.id, "backup_user", user.username, detail)
        _set_backup_job(
            job_id,
            status="done",
            backup_file=archive,
            remote_file=remote_file,
            target=target_name,
            message="Full user backup completed",
            finished_at=_now_iso(),
        )
    except Exception as exc:
        db.rollback()
        logger.exception("Full user backup job failed: job_id=%s user_id=%s", job_id, target_user_id)
        _set_backup_job(job_id, status="error", error=str(exc), message="Full user backup failed", finished_at=_now_iso())
    finally:
        db.close()


def _run_sftp_backup_job(job_id: str, request_user_id: int, website_id: int, target_id: int) -> None:
    _set_backup_job(job_id, status="running", started_at=_now_iso(), message="Creating and uploading SFTP backup")
    db = SessionLocal()
    try:
        request_user = db.query(User).filter(User.id == request_user_id).first()
        if not request_user or not request_user.is_active:
            raise ValueError("User not found")
        ensure_role(request_user.role, Role.admin)
        website = get_owned_website(db, request_user, website_id)
        db_item = db.query(DatabaseAccount).filter(DatabaseAccount.website_id == website.id).first()
        archive = backup.create_backup(website, db_item.db_name if db_item else None)
        target_name, remote_file = upload_archive_to_target(db, target_id, archive)
        log_action(db, request_user.id, "backup_sftp", website.domain, f"{target_name}:{remote_file}")
        _set_backup_job(
            job_id,
            status="done",
            backup_file=archive,
            remote_file=remote_file,
            target=target_name,
            message="SFTP backup completed",
            finished_at=_now_iso(),
        )
    except Exception as exc:
        db.rollback()
        logger.exception("SFTP backup job failed: job_id=%s website_id=%s", job_id, website_id)
        _set_backup_job(job_id, status="error", error=str(exc), message="SFTP backup failed", finished_at=_now_iso())
    finally:
        db.close()


def _queue_site_backup(current_user: User, website: Website) -> dict:
    job = _queue_backup_job(current_user, "site_backup", "Website backup queued", website_id=website.id)
    _backup_job_executor.submit(_run_site_backup_job, job["job_id"], current_user.id, website.id)
    return job


def _queue_user_backup(current_user: User, user: User, target_id: int | None) -> dict:
    job = _queue_backup_job(
        current_user,
        "user_backup",
        "Full user backup queued",
        target_user_id=user.id,
        target_id=target_id,
    )
    _backup_job_executor.submit(_run_user_backup_job, job["job_id"], current_user.id, user.id, target_id)
    return job


def _queue_sftp_backup(current_user: User, website: Website, target_id: int) -> dict:
    job = _queue_backup_job(
        current_user,
        "sftp_backup",
        "SFTP backup queued",
        website_id=website.id,
        target_id=target_id,
    )
    _backup_job_executor.submit(_run_sftp_backup_job, job["job_id"], current_user.id, website.id, target_id)
    return job


def _save_user_restore_upload(file: UploadFile) -> dict:
    target = ""
    try:
        target = backup.save_uploaded_user_backup(file.filename or "user-backup.tar.gz", file.file)
        manifest = backup.read_backup_manifest(target)
        if manifest.get("kind") not in backup.RESTORABLE_BACKUP_KINDS:
            raise ValueError("This is not a full user backup")
    except (ValueError, FileNotFoundError) as exc:
        if target:
            try:
                backup.delete_user_backup(target)
            except Exception:
                pass
        raise HTTPException(status_code=400, detail=str(exc)) from exc
    path = backup.user_backup_path(target)
    return {
        "backup_file": target,
        "filename": path.name,
        "username": (manifest.get("user") or {}).get("username"),
        "generated_at": manifest.get("generated_at"),
        "websites": len(manifest.get("websites") or []),
        "size": path.stat().st_size,
        "valid": True,
        "error": "",
    }


def _quota_check_for_website(db: Session, website: Website):
    owner = website.owner

    def check(incoming_bytes: int, replaced_bytes: int = 0) -> None:
        storage_quota.enforce_user_storage_quota(
            db,
            owner,
            incoming_bytes=incoming_bytes,
            replaced_bytes=replaced_bytes,
        )

    return check


@router.post("/backup")
def create_backup(payload: BackupCreate, db: Session = Depends(get_db), current_user: User = Depends(get_current_user)):
    website = get_owned_website(db, current_user, payload.website_id)
    return _queue_site_backup(current_user, website)


@router.get("/backup-jobs")
def list_backup_jobs(current_user: User = Depends(get_current_user)):
    return {"jobs": _list_backup_jobs(current_user)}


@router.get("/backup-jobs/{job_id}")
def get_backup_job(job_id: str, current_user: User = Depends(get_current_user)):
    job = _get_backup_job(job_id)
    if not job:
        raise HTTPException(status_code=404, detail="Backup job not found")
    if job.get("request_user_id") != current_user.id and not is_admin_role(current_user.role):
        raise HTTPException(status_code=403, detail="Access denied")
    return _public_backup_job(job)


@router.post("/restore")
def restore_backup(payload: RestoreBackup, db: Session = Depends(get_db), current_user: User = Depends(get_current_user)):
    website = get_owned_website(db, current_user, payload.website_id)
    try:
        path = backup.restore_backup(website, payload.backup_file)
    except (FileNotFoundError, RuntimeError, ValueError) as exc:
        raise HTTPException(status_code=400, detail=str(exc)) from exc
    if website.linux_user:
        runtime_php_version = website.php_version if (website.app_type or "wordpress") in {"wordpress", "php"} else None
        site_users.ensure_site_runtime(website.domain, website.root_path, runtime_php_version, website.linux_user)
    wordpress.fix_permissions(website.root_path, website.linux_user)
    database_restored = False
    database_note = ""
    account = db.query(DatabaseAccount).filter(DatabaseAccount.website_id == website.id).first()
    if payload.restore_database:
        if not account:
            database_note = "No database is linked to this website, so only files were restored."
        else:
            try:
                database_restored = backup.restore_backup_database(
                    website, payload.backup_file, account.db_name
                )
                if not database_restored:
                    database_note = "This backup contains no SQL dump, so only files were restored."
            except Exception as exc:
                raise HTTPException(
                    status_code=400, detail=f"Files restored, but the database import failed: {exc}"
                ) from exc
    elif backup.archive_has_database(website, payload.backup_file):
        database_note = ("Files restored. This backup also holds a database dump, which was "
                         "left alone -- re-run with restore_database to import it.")
    log_action(db, current_user.id, "restore", website.domain, payload.backup_file)
    return {"restored_to": path, "database_restored": database_restored, "note": database_note}


@router.get("/backups/{website_id}")
def list_backups(website_id: int, db: Session = Depends(get_db), current_user: User = Depends(get_current_user)):
    website = get_owned_website(db, current_user, website_id)
    return {"items": backup.list_backups(website.domain)}


@router.get("/backups/{website_id}/download")
def download_backup(website_id: int, backup_file: str, db: Session = Depends(get_db), current_user: User = Depends(get_current_user)):
    website = get_owned_website(db, current_user, website_id)
    try:
        path = backup.backup_path(website.domain, backup_file)
    except FileNotFoundError as exc:
        raise HTTPException(status_code=404, detail="Backup not found") from exc
    return FileResponse(str(path), filename=path.name, media_type="application/gzip")


@router.delete("/backups/{website_id}")
def delete_backup(website_id: int, backup_file: str, db: Session = Depends(get_db), current_user: User = Depends(get_current_user)):
    website = get_owned_website(db, current_user, website_id)
    try:
        deleted = backup.delete_backup(website.domain, backup_file)
    except FileNotFoundError as exc:
        raise HTTPException(status_code=404, detail="Backup not found")
    log_action(db, current_user.id, "delete_backup", website.domain, deleted)
    return {"deleted": deleted}


@router.post("/backups/{website_id}/upload")
def upload_backup(website_id: int, file: UploadFile = File(...), db: Session = Depends(get_db), current_user: User = Depends(get_current_user)):
    website = get_owned_website(db, current_user, website_id)
    try:
        target = backup.save_uploaded_backup(website.domain, file.filename or "backup.tar.gz", file.file)
    except ValueError as exc:
        raise HTTPException(status_code=400, detail=str(exc)) from exc
    log_action(db, current_user.id, "upload_backup", website.domain, target)
    return {"backup_file": target}


@router.get("/user-restore-backups")
def list_user_restore_backups(current_user: User = Depends(get_current_user)):
    ensure_role(current_user.role, Role.admin)
    return {"directory": backup.user_restore_dir(), "items": backup.list_user_restore_backups()}


@router.post("/user-restore-backups/upload")
def upload_user_restore_backups(
    files: list[UploadFile] = File(...),
    db: Session = Depends(get_db),
    current_user: User = Depends(get_current_user),
):
    ensure_role(current_user.role, Role.admin)
    if not files:
        raise HTTPException(status_code=400, detail="No backup files uploaded")
    items = [_save_user_restore_upload(file) for file in files]
    users = ", ".join(item.get("username") or item.get("filename") or "user" for item in items)
    log_action(db, current_user.id, "upload_user_restore_backups", "restore_folder", users)
    return {"directory": backup.user_restore_dir(), "items": items}


@router.delete("/user-restore-backups")
def delete_user_restore_backup(backup_file: str, request: Request, db: Session = Depends(get_db), current_user: User = Depends(get_current_user)):
    ensure_role(current_user.role, Role.admin)
    try:
        deleted = backup.delete_user_restore_backup(backup_file)
    except FileNotFoundError as exc:
        raise HTTPException(status_code=404, detail="Backup not found") from exc
    log_action(db, current_user.id, "delete_user_restore_backup", "restore_folder", deleted, request=request)
    return {"deleted": deleted}


# ---------------------------------------------------------------------------
# DirectAdmin backup import
# ---------------------------------------------------------------------------

DA_IMPORT_JOB_LIMIT = 10
_da_import_job_executor = ThreadPoolExecutor(max_workers=1, thread_name_prefix="opanel-ent-da-import")
_da_import_jobs: dict[str, dict] = {}
_da_import_jobs_lock = threading.Lock()


def _public_da_import_job(job: dict) -> dict:
    return {
        "job_id": job["job_id"],
        "status": job["status"],
        "backup_file": job.get("backup_file", ""),
        "message": job.get("message", ""),
        "error": job.get("error", ""),
        "summary": job.get("summary"),
        "created_at": job.get("created_at", ""),
        "started_at": job.get("started_at", ""),
        "finished_at": job.get("finished_at", ""),
    }


def _set_da_import_job(job_id: str, **updates) -> None:
    with _da_import_jobs_lock:
        job = _da_import_jobs.get(job_id)
        if not job:
            return
        job.update(updates)


def _remember_da_import_job(job: dict) -> dict:
    with _da_import_jobs_lock:
        _da_import_jobs[job["job_id"]] = job
        if len(_da_import_jobs) > DA_IMPORT_JOB_LIMIT:
            removable = [
                (j.get("created_at", ""), jid)
                for jid, j in _da_import_jobs.items()
                if j.get("status") not in {"queued", "running"}
            ]
            removable.sort(key=lambda x: x[0])
            for _, jid in removable[: len(_da_import_jobs) - DA_IMPORT_JOB_LIMIT]:
                _da_import_jobs.pop(jid, None)
    return _public_da_import_job(job)


def _run_da_import_job(job_id: str, backup_file: str, overwrite: bool = False) -> None:
    _set_da_import_job(job_id, status="running", started_at=datetime.utcnow().isoformat() + "Z",
                       message="Starting DirectAdmin import...")
    db = SessionLocal()
    try:
        summary = da_import.import_da_backup(backup_file, db, overwrite=overwrite)
        imported = len(summary.get("imported_domains", []))
        subs = len(summary.get("subdomains", []))
        message = f"Imported {imported} domain(s)"
        if subs:
            message += f" (incl. {subs} DirectAdmin subdomain(s))"
        _set_da_import_job(
            job_id,
            status="done",
            summary=summary,
            message=message,
            finished_at=datetime.utcnow().isoformat() + "Z",
        )
    except Exception as exc:
        db.rollback()
        logger.exception("DA import job failed: job_id=%s file=%s", job_id, backup_file)
        _set_da_import_job(
            job_id,
            status="error",
            error=str(exc),
            message="Import failed",
            finished_at=datetime.utcnow().isoformat() + "Z",
        )
    finally:
        db.close()


@router.get("/da-backups")
def list_da_backups(current_user: User = Depends(get_current_user)):
    ensure_role(current_user.role, Role.admin)
    return {"directory": da_import.da_backup_dir(), "items": da_import.list_da_backups()}


@router.post("/da-backups/upload")
def upload_da_backups(
    files: list[UploadFile] = File(...),
    db: Session = Depends(get_db),
    current_user: User = Depends(get_current_user),
):
    ensure_role(current_user.role, Role.admin)
    if not files:
        raise HTTPException(status_code=400, detail="No backup files uploaded")
    items = []
    for file in files:
        try:
            item = da_import.save_da_backup(file.filename or "backup.tar.gz", file.file)
            items.append(item)
        except ValueError as exc:
            raise HTTPException(status_code=400, detail=str(exc)) from exc
    log_action(db, current_user.id, "upload_da_backups", "da_backup_dir",
               ", ".join(item["filename"] for item in items))
    return {"directory": da_import.da_backup_dir(), "items": items}


@router.delete("/da-backups")
def delete_da_backup(backup_file: str, request: Request, db: Session = Depends(get_db),
                     current_user: User = Depends(get_current_user)):
    ensure_role(current_user.role, Role.admin)
    try:
        deleted = da_import.delete_da_backup(backup_file)
    except FileNotFoundError as exc:
        raise HTTPException(status_code=404, detail="Backup not found") from exc
    log_action(db, current_user.id, "delete_da_backup", "da_backup_dir", deleted, request=request)
    return {"deleted": deleted}


@router.post("/da-import")
def start_da_import(backup_file: str, request: Request, db: Session = Depends(get_db),
                    overwrite: bool = False,
                    current_user: User = Depends(get_current_user)):
    ensure_role(current_user.role, Role.admin)
    # Validate file exists
    try:
        da_import.list_da_backups()  # triggers dir check
    except Exception:
        pass
    job_id = uuid.uuid4().hex
    job = {
        "job_id": job_id,
        "status": "queued",
        "backup_file": backup_file,
        "message": "Queued",
        "error": "",
        "summary": None,
        "created_at": datetime.utcnow().isoformat() + "Z",
        "started_at": "",
        "finished_at": "",
    }
    _remember_da_import_job(job)
    _da_import_job_executor.submit(_run_da_import_job, job_id, backup_file, overwrite)
    log_action(db, current_user.id, "start_da_import", backup_file, request=request)
    return _public_da_import_job(job)


@router.get("/da-import/jobs")
def list_da_import_jobs(current_user: User = Depends(get_current_user)):
    ensure_role(current_user.role, Role.admin)
    with _da_import_jobs_lock:
        jobs = [_public_da_import_job(dict(j)) for j in _da_import_jobs.values()]
    return {"jobs": sorted(jobs, key=lambda j: j.get("created_at", ""), reverse=True)}


@router.get("/da-import/jobs/{job_id}")
def get_da_import_job(job_id: str, current_user: User = Depends(get_current_user)):
    ensure_role(current_user.role, Role.admin)
    with _da_import_jobs_lock:
        job = _da_import_jobs.get(job_id)
    if not job:
        raise HTTPException(status_code=404, detail="Job not found")
    return _public_da_import_job(dict(job))


@router.get("/user-backups/{user_id}")
def list_user_backups(user_id: int, db: Session = Depends(get_db), current_user: User = Depends(get_current_user)):
    user = get_backup_user(db, current_user, user_id)
    items = backup.list_user_backups(user.username)
    if is_admin_role(current_user.role):
        items.extend(item for item in backup.list_uploaded_user_backups(user.username) if item not in items)
    return {"items": items}


@router.post("/user-backup")
def create_user_backup(payload: UserBackupCreate, request: Request, db: Session = Depends(get_db), current_user: User = Depends(get_current_user)):
    user = get_backup_user(db, current_user, payload.user_id)
    if payload.target_id:
        ensure_role(current_user.role, Role.admin)
        target_exists = db.query(SftpBackupTarget).filter(SftpBackupTarget.id == payload.target_id, SftpBackupTarget.is_active == True).first()  # noqa: E712
        if not target_exists:
            raise HTTPException(status_code=404, detail="SFTP target not found")
    log_action(db, current_user.id, "queue_backup_user", user.username, request=request)
    return _queue_user_backup(current_user, user, payload.target_id)


@router.get("/user-backups-download")
def download_user_backup(backup_file: str, db: Session = Depends(get_db), current_user: User = Depends(get_current_user)):
    ensure_role(current_user.role, Role.admin)
    try:
        path = backup.user_backup_path(backup_file)
    except FileNotFoundError as exc:
        raise HTTPException(status_code=404, detail="Backup not found") from exc
    return FileResponse(str(path), filename=path.name, media_type="application/gzip")


@router.post("/user-backups/upload")
def upload_user_backup(file: UploadFile = File(...), db: Session = Depends(get_db), current_user: User = Depends(get_current_user)):
    ensure_role(current_user.role, Role.admin)
    item = _save_user_restore_upload(file)
    log_action(db, current_user.id, "upload_user_backup", item.get("username") or "user", item["backup_file"])
    return {"backup_file": item["backup_file"], "username": item.get("username")}


@router.post("/user-restore")
def restore_user_backup(payload: UserRestoreBackup, request: Request, db: Session = Depends(get_db), current_user: User = Depends(get_current_user)):
    ensure_role(current_user.role, Role.admin)
    try:
        result = backup.restore_user_backup(payload.backup_file, db)
    except (FileNotFoundError, ValueError, RuntimeError) as exc:
        raise HTTPException(status_code=400, detail=str(exc)) from exc
    log_action(db, current_user.id, "restore_user", result.get("username", "user"), payload.backup_file, request=request)
    return result


@router.delete("/user-backups")
def delete_user_backup(backup_file: str, request: Request, db: Session = Depends(get_db), current_user: User = Depends(get_current_user)):
    ensure_role(current_user.role, Role.admin)
    try:
        deleted = backup.delete_user_backup(backup_file)
    except FileNotFoundError as exc:
        raise HTTPException(status_code=404, detail="Backup not found") from exc
    log_action(db, current_user.id, "delete_user_backup", "user", deleted, request=request)
    return {"deleted": deleted}


@router.get("/backup-schedules", response_model=list[BackupScheduleOut])
def list_backup_schedules(db: Session = Depends(get_db), current_user: User = Depends(get_current_user)):
    ensure_role(current_user.role, Role.admin)
    return db.query(BackupSchedule).order_by(BackupSchedule.id.desc()).all()


@router.post("/backup-schedules", response_model=BackupScheduleOut)
def create_backup_schedule(payload: BackupScheduleCreate, request: Request, db: Session = Depends(get_db), current_user: User = Depends(get_current_user)):
    ensure_role(current_user.role, Role.admin)
    user_ids = [] if payload.all_users else (payload.user_ids or ([payload.user_id] if payload.user_id else []))
    user_ids = sorted({int(user_id) for user_id in user_ids if int(user_id) > 0})
    users = []
    if not payload.all_users:
        if not user_ids:
            raise HTTPException(status_code=400, detail="Select at least one user")
        users = db.query(User).filter(User.id.in_(user_ids)).all()
        found_ids = {user.id for user in users}
        missing_ids = [str(user_id) for user_id in user_ids if user_id not in found_ids]
        if missing_ids:
            raise HTTPException(status_code=404, detail=f"User not found: {', '.join(missing_ids)}")
    if payload.target_id and not db.query(SftpBackupTarget).filter(SftpBackupTarget.id == payload.target_id, SftpBackupTarget.is_active == True).first():  # noqa: E712
        raise HTTPException(status_code=404, detail="SFTP target not found")
    item = BackupSchedule(
        user_id=user_ids[0] if user_ids else None,
        user_ids=json.dumps(user_ids),
        all_users=payload.all_users,
        target_id=payload.target_id,
        schedule=payload.schedule,
        retention=payload.retention,
        is_active=payload.is_active,
        last_status="pending",
    )
    db.add(item)
    db.commit()
    db.refresh(item)
    target = "all_users" if payload.all_users else ",".join(user.username for user in users)
    log_action(db, current_user.id, "create_backup_schedule", target, payload.schedule, request=request)
    return item


@router.delete("/backup-schedules/{schedule_id}")
def delete_backup_schedule(schedule_id: int, request: Request, db: Session = Depends(get_db), current_user: User = Depends(get_current_user)):
    ensure_role(current_user.role, Role.admin)
    item = db.query(BackupSchedule).filter(BackupSchedule.id == schedule_id).first()
    if not item:
        raise HTTPException(status_code=404, detail="Backup schedule not found")
    db.delete(item)
    db.commit()
    log_action(db, current_user.id, "delete_backup_schedule", str(schedule_id), request=request)
    return {"ok": True}


@router.get("/sftp-targets", response_model=list[SftpBackupTargetOut])
def list_sftp_targets(db: Session = Depends(get_db), current_user: User = Depends(get_current_user)):
    ensure_role(current_user.role, Role.admin)
    return db.query(SftpBackupTarget).order_by(SftpBackupTarget.id.desc()).all()


@router.post("/sftp-targets", response_model=SftpBackupTargetOut)
def create_sftp_target(
    payload: SftpBackupTargetCreate,
    request: Request,
    db: Session = Depends(get_db),
    current_user: User = Depends(get_current_user),
):
    ensure_role(current_user.role, Role.admin)
    if db.query(SftpBackupTarget).filter(SftpBackupTarget.name == payload.name).first():
        raise HTTPException(status_code=409, detail="SFTP target name already exists")
    if not payload.password and not payload.private_key:
        raise HTTPException(status_code=400, detail="SFTP password or private key is required")
    target = SftpBackupTarget(
        name=payload.name,
        host=payload.host,
        port=payload.port,
        username=payload.username,
        password=encrypt(payload.password) if payload.password else None,
        private_key=encrypt(payload.private_key) if payload.private_key else None,
        remote_path=payload.remote_path,
        is_active=True,
    )
    db.add(target)
    db.commit()
    db.refresh(target)
    log_action(db, current_user.id, "create_sftp_target", target.name, request=request)
    return target


@router.delete("/sftp-targets/{target_id}")
def delete_sftp_target(
    target_id: int,
    request: Request,
    db: Session = Depends(get_db),
    current_user: User = Depends(get_current_user),
):
    ensure_role(current_user.role, Role.admin)
    target = db.query(SftpBackupTarget).filter(SftpBackupTarget.id == target_id).first()
    if not target:
        raise HTTPException(status_code=404, detail="SFTP target not found")
    name = target.name
    db.delete(target)
    db.commit()
    log_action(db, current_user.id, "delete_sftp_target", name, request=request)
    return {"ok": True}


@router.post("/backup-sftp")
def create_sftp_backup(
    payload: SftpBackupRun,
    request: Request,
    db: Session = Depends(get_db),
    current_user: User = Depends(get_current_user),
):
    ensure_role(current_user.role, Role.admin)
    website = get_owned_website(db, current_user, payload.website_id)
    target = db.query(SftpBackupTarget).filter(SftpBackupTarget.id == payload.target_id).first()
    if not target or not target.is_active:
        raise HTTPException(status_code=404, detail="SFTP target not found")
    log_action(db, current_user.id, "queue_backup_sftp", website.domain, target.name, request=request)
    return _queue_sftp_backup(current_user, website, target.id)


@router.get("/php-config")
def get_php_config(php_version: str = Query(default="8.4"), current_user: User = Depends(get_current_user)):
    ensure_role(current_user.role, Role.admin)
    try:
        return php.read_php_ini(php_version)
    except ValueError as exc:
        raise HTTPException(status_code=400, detail=str(exc)) from exc


@router.post("/php-config")
def update_php_config(payload: PhpConfigUpdate, current_user: User = Depends(get_current_user)):
    ensure_role(current_user.role, Role.admin)
    try:
        target = php.update_php_ini(payload)
    except ValueError as exc:
        raise HTTPException(status_code=400, detail=str(exc)) from exc
    except (OSError, RuntimeError) as exc:
        raise HTTPException(status_code=500, detail=str(exc)) from exc
    return {"target": target}


@router.post("/php-config/defaults")
def restore_php_config_defaults(payload: PhpConfigRestore, current_user: User = Depends(get_current_user)):
    ensure_role(current_user.role, Role.admin)
    try:
        target = php.restore_default_php_ini(payload.php_version)
        values = php.default_php_config(payload.php_version)
    except ValueError as exc:
        raise HTTPException(status_code=400, detail=str(exc)) from exc
    except (OSError, RuntimeError) as exc:
        raise HTTPException(status_code=500, detail=str(exc)) from exc
    return {"target": target, "values": values}


@router.get("/php-versions")
def get_php_versions(current_user: User = Depends(get_current_user)):
    # Every user needs this to pick a PHP version for their own site, so it is
    # not admin-only. Installing a version below still is.
    return {
        "installed": php.list_installed_php(),
        "supported": list(php.SUPPORTED_PHP_VERSIONS),
    }


@router.post("/php-versions/{php_version}/install")
def install_php_version(php_version: str, current_user: User = Depends(get_current_user)):
    ensure_role(current_user.role, Role.admin)
    try:
        result = php.install_php(php_version)
    except ValueError as exc:
        raise HTTPException(status_code=400, detail=str(exc)) from exc
    except RuntimeError as exc:
        raise HTTPException(status_code=500, detail=str(exc)) from exc
    return result


# ---------------------------------------------------------------------------
# PHP / LSPHP auto-tuning
# ---------------------------------------------------------------------------
@router.get("/php/tuning")
def get_php_tuning(
    php_version: str = Query(default="8.4"),
    current_user: User = Depends(get_current_user),
):
    """Return current OPanel Enterprise PHP config + hardware recommendation."""
    ensure_role(current_user.role, Role.admin)
    try:
        current = php.read_php_tuning(php_version)
        recommendation = php.recommend_php_config()
    except ValueError as exc:
        raise HTTPException(status_code=400, detail=str(exc)) from exc
    return {"current": current, "recommendation": recommendation}


@router.post("/php/tuning")
def apply_php_tuning_endpoint(
    php_version: str | None = Query(default=None),
    db: Session = Depends(get_db),
    current_user: User = Depends(get_current_user),
):
    """Auto-tune PHP/LSPHP for current hardware and restart OLS.

    If php_version is omitted, applies to ALL installed LSPHP versions.
    """
    ensure_role(current_user.role, Role.admin)
    try:
        result = php.apply_php_tuning(php_version)
    except ValueError as exc:
        raise HTTPException(status_code=400, detail=str(exc)) from exc
    except (OSError, RuntimeError) as exc:
        raise HTTPException(status_code=500, detail=str(exc)) from exc
    log_action(db, current_user.id, "php_tuning_apply",
               ",".join(result["applied_to"]),
               detail=f"mem={result['memory_limit']} opcache={result['opcache_memory_consumption']}M workers={result['lsapi_children']}")
    return result


@router.post("/cron")
def add_cron(payload: CronCreate, db: Session = Depends(get_db), current_user: User = Depends(get_current_user)):
    website = get_owned_website(db, current_user, payload.website_id)
    cron_user = cron.cron_user_for_website(website)
    if not website.linux_user and cron_user != "www-data":
        website.linux_user = cron_user
        db.add(website)
    try:
        line = cron.add_cron(website, payload.schedule, payload.command)
    except ValueError as exc:
        raise HTTPException(status_code=400, detail=str(exc)) from exc
    db.commit()
    log_action(db, current_user.id, "add_cron", website.domain, line)
    return {"line": line, "cron_user": cron_user}


@router.get("/cron/{website_id}")
def list_cron(website_id: int, db: Session = Depends(get_db), current_user: User = Depends(get_current_user)):
    website = get_owned_website(db, current_user, website_id)
    cron_user = cron.cron_user_for_website(website)
    return {"items": cron.list_cron_entries(website.domain, cron_user), "cron_user": cron_user}


@router.delete("/cron")
def delete_cron(payload: CronDelete, db: Session = Depends(get_db), current_user: User = Depends(get_current_user)):
    website = get_owned_website(db, current_user, payload.website_id)
    cron_user = cron.cron_user_for_website(website)
    try:
        line = cron.delete_cron(website.domain, payload.index, cron_user)
    except ValueError as exc:
        raise HTTPException(status_code=400, detail=str(exc)) from exc
    log_action(db, current_user.id, "delete_cron", website.domain, line)
    return {"deleted": line, "cron_user": cron_user}


@router.post("/wordpress")
def wordpress_action(payload: WpAction, db: Session = Depends(get_db), current_user: User = Depends(get_current_user)):
    website = get_owned_website(db, current_user, payload.website_id)
    result = wordpress.wp_update(
        str(site_users.document_root(website.root_path, website.document_root or "public_html")),
        payload.action,
        website.linux_user,
    )
    return result.__dict__


@router.post("/wordpress/{website_id}/fix-permissions")
def fix_wordpress_permissions(website_id: int, db: Session = Depends(get_db), current_user: User = Depends(get_current_user)):
    website = get_owned_website(db, current_user, website_id)
    if website.linux_user:
        runtime_php_version = website.php_version if (website.app_type or "wordpress") in {"wordpress", "php"} else None
        site_users.ensure_site_runtime(website.domain, website.root_path, runtime_php_version, website.linux_user)
    wordpress.fix_permissions(website.root_path, website.linux_user)
    log_action(db, current_user.id, "fix_permissions", website.domain, website.root_path)
    return {"message": f"Fixed permissions for {website.domain}", "root_path": website.root_path}

class WpInstallRequest(BaseModel):
    admin_user: str = "admin"
    admin_email: str = ""
    admin_password: str = ""
    title: str = ""

@router.post("/wordpress/{website_id}/install")
def install_wordpress_on_site(website_id: int, payload: WpInstallRequest, request: Request, db: Session = Depends(get_db), current_user: User = Depends(get_current_user)):
    """Install WordPress on an existing website that doesn't have it yet."""
    from pydantic import EmailStr
    website = get_owned_website(db, current_user, website_id)
    if not payload.admin_email:
        payload.admin_email = f"admin@{website.domain}"
    if not payload.admin_password or len(payload.admin_password) < 12:
        raise HTTPException(status_code=400, detail="admin_password must be at least 12 characters")
    if not payload.title:
        payload.title = website.domain

    # Check if WP already installed (wp-config.php exists)
    from pathlib import Path
    wp_config = Path(website.root_path) / (website.document_root or "public_html") / "wp-config.php"
    if wp_config.exists():
        raise HTTPException(status_code=400, detail="WordPress is already installed on this site")

    # Create database
    try:
        db_info = mariadb.create_database(website.domain)
    except RuntimeError as exc:
        raise HTTPException(status_code=400, detail=f"Could not create database: {exc}") from exc

    # Install WordPress
    try:
        wordpress.install_wordpress(
            website.domain, db_info, payload.title,
            payload.admin_user, payload.admin_password, payload.admin_email,
            website.php_version, website.linux_user, root_path=website.root_path,
        )
    except (RuntimeError, ValueError) as exc:
        mariadb.drop_database(db_info["db_name"], db_info["db_user"])
        raise HTTPException(status_code=400, detail=str(exc)) from exc

    # Update website app_type
    website.app_type = "wordpress"
    website.nginx_rewrite_mode = "front_controller"
    db.commit()

    # Rewrite vhost
    try:
        openlitespeed.rewrite_vhost(
            website.domain, website.root_path,
            app_type="wordpress", php_version=website.php_version,
            linux_user=website.linux_user,
            lsphp_socket_override=site_users.site_lsphp_socket(website.linux_user, website.root_path, website.php_version),
            document_root=website.document_root or "public_html",
            rewrite_mode="front_controller",
        )
    except (RuntimeError, ValueError):
        pass

    log_action(db, current_user.id, "install_wordpress", website.domain, request=request)
    return {"message": f"WordPress installed on {website.domain}", "admin_user": payload.admin_user, "admin_password": payload.admin_password}

@router.post("/wordpress/{website_id}/update-all")
def update_wordpress_all(website_id: int, request: Request, db: Session = Depends(get_db), current_user: User = Depends(get_current_user)):
    """Update WordPress core, plugins, and themes."""
    website = get_owned_website(db, current_user, website_id)
    doc_root = str(site_users.document_root(website.root_path, website.document_root or "public_html"))
    results = {}
    for action in ("core", "plugins", "themes"):
        try:
            result = wordpress.wp_update(doc_root, action, website.linux_user)
            results[action] = (result.stdout or result.stderr or "").strip()
        except (RuntimeError, ValueError) as exc:
            results[action] = str(exc)
    log_action(db, current_user.id, "update_wordpress_all", website.domain, request=request)
    return {"message": f"WordPress updated on {website.domain}", "results": results}


@router.get("/files/jobs")
def list_file_jobs(website_id: int | None = Query(default=None), current_user: User = Depends(get_current_user)):
    return {"jobs": _list_file_jobs(current_user, website_id)}


@router.get("/files/jobs/{job_id}")
def get_file_job(job_id: str, current_user: User = Depends(get_current_user)):
    job = _get_file_job(job_id)
    if not job:
        raise HTTPException(status_code=404, detail="File job not found")
    if job.get("user_id") != current_user.id and not is_admin_role(current_user.role):
        raise HTTPException(status_code=403, detail="Access denied")
    return _public_file_job(job)


@router.get("/files/{website_id}")
def list_files(website_id: int, path: str = Query(default=""), db: Session = Depends(get_db), current_user: User = Depends(get_current_user)):
    website = get_owned_website(db, current_user, website_id)
    return {"items": file_manager.list_files(website, path)}


@router.get("/files/{website_id}/read")
def read_file(website_id: int, path: str, db: Session = Depends(get_db), current_user: User = Depends(get_current_user)):
    website = get_owned_website(db, current_user, website_id)
    try:
        content = file_manager.read_text_file(
            website,
            path,
            allow_sensitive=is_admin_role(current_user.role),
        )
    except ValueError as exc:
        raise HTTPException(status_code=400, detail=str(exc)) from exc
    return {"content": content}


@router.get("/files/{website_id}/download")
def download_file(website_id: int, path: str, db: Session = Depends(get_db), current_user: User = Depends(get_current_user)):
    website = get_owned_website(db, current_user, website_id)
    try:
        target = file_manager.download_file_path(website, path, allow_sensitive=is_admin_role(current_user.role))
    except ValueError as exc:
        raise HTTPException(status_code=400, detail=str(exc)) from exc
    return FileResponse(str(target), filename=target.name)


@router.post("/files/mkdir")
def make_directory(payload: FileMkdir, db: Session = Depends(get_db), current_user: User = Depends(get_current_user)):
    ensure_role(current_user.role, Role.end_user)
    website = get_owned_website(db, current_user, payload.website_id)
    try:
        target = file_manager.make_directory(website, payload.path, payload.name)
    except ValueError as exc:
        raise HTTPException(status_code=400, detail=str(exc)) from exc
    log_action(db, current_user.id, "mkdir", website.domain, target)
    return {"target": target}


@router.post("/files/create")
def create_file(payload: FileCreate, db: Session = Depends(get_db), current_user: User = Depends(get_current_user)):
    ensure_role(current_user.role, Role.end_user)
    website = get_owned_website(db, current_user, payload.website_id)
    try:
        target = file_manager.create_text_file(
            website,
            payload.path,
            payload.name,
            is_admin_role(current_user.role),
            quota_check=_quota_check_for_website(db, website),
        )
    except storage_quota.StorageQuotaExceeded as exc:
        raise HTTPException(status_code=status.HTTP_413_REQUEST_ENTITY_TOO_LARGE, detail=str(exc)) from exc
    except ValueError as exc:
        raise HTTPException(status_code=400, detail=str(exc)) from exc
    log_action(db, current_user.id, "create_file", website.domain, target)
    return {"target": target}


@router.post("/files/rename")
def rename_entry(payload: FileRename, db: Session = Depends(get_db), current_user: User = Depends(get_current_user)):
    ensure_role(current_user.role, Role.end_user)
    website = get_owned_website(db, current_user, payload.website_id)
    try:
        target = file_manager.rename_entry(website, payload.path, payload.new_name, is_admin_role(current_user.role))
    except ValueError as exc:
        raise HTTPException(status_code=400, detail=str(exc)) from exc
    log_action(db, current_user.id, "rename_file", website.domain, target)
    return {"target": target}


@router.post("/files/chmod")
def chmod_entry(payload: FileChmod, db: Session = Depends(get_db), current_user: User = Depends(get_current_user)):
    ensure_role(current_user.role, Role.end_user)
    website = get_owned_website(db, current_user, payload.website_id)
    try:
        target = file_manager.chmod_entry(website, payload.path, payload.mode)
    except ValueError as exc:
        raise HTTPException(status_code=400, detail=str(exc)) from exc
    log_action(db, current_user.id, "chmod_file", website.domain, f"{target} {payload.mode}")
    return {"target": target, "mode": payload.mode}


@router.post("/files/delete")
def delete_entries(payload: FileBulkDelete, db: Session = Depends(get_db), current_user: User = Depends(get_current_user)):
    ensure_role(current_user.role, Role.end_user)
    website = get_owned_website(db, current_user, payload.website_id)
    try:
        deleted = file_manager.delete_entries(website, payload.paths, is_admin_role(current_user.role))
    except ValueError as exc:
        raise HTTPException(status_code=400, detail=str(exc)) from exc
    log_action(db, current_user.id, "delete_files", website.domain, ",".join(payload.paths[:20]))
    return {"deleted": deleted}


@router.post("/files/copy")
def copy_entries(payload: FileTransfer, db: Session = Depends(get_db), current_user: User = Depends(get_current_user)):
    ensure_role(current_user.role, Role.end_user)
    website = get_owned_website(db, current_user, payload.website_id)
    try:
        copied = file_manager.copy_entries(
            website,
            payload.paths,
            payload.destination_path,
            allow_executable=is_admin_role(current_user.role),
            allow_sensitive=is_admin_role(current_user.role),
            quota_check=_quota_check_for_website(db, website),
        )
    except storage_quota.StorageQuotaExceeded as exc:
        raise HTTPException(status_code=status.HTTP_413_REQUEST_ENTITY_TOO_LARGE, detail=str(exc)) from exc
    except ValueError as exc:
        raise HTTPException(status_code=400, detail=str(exc)) from exc
    log_action(db, current_user.id, "copy_files", website.domain, f"{','.join(payload.paths[:20])} -> {payload.destination_path}")
    return {"copied": copied}


@router.post("/files/move")
def move_entries(payload: FileTransfer, db: Session = Depends(get_db), current_user: User = Depends(get_current_user)):
    ensure_role(current_user.role, Role.end_user)
    website = get_owned_website(db, current_user, payload.website_id)
    try:
        moved = file_manager.move_entries(
            website,
            payload.paths,
            payload.destination_path,
            allow_executable=is_admin_role(current_user.role),
        )
    except ValueError as exc:
        raise HTTPException(status_code=400, detail=str(exc)) from exc
    log_action(db, current_user.id, "move_files", website.domain, f"{','.join(payload.paths[:20])} -> {payload.destination_path}")
    return {"moved": moved}


@router.post("/files/archive")
def archive_entries(payload: FileArchive, db: Session = Depends(get_db), current_user: User = Depends(get_current_user)):
    ensure_role(current_user.role, Role.end_user)
    website = get_owned_website(db, current_user, payload.website_id)
    try:
        target = file_manager.archive_entries(
            website,
            payload.base_path,
            payload.paths,
            payload.output_name,
            payload.format,
            allow_sensitive=is_admin_role(current_user.role),
            quota_check=_quota_check_for_website(db, website),
        )
    except storage_quota.StorageQuotaExceeded as exc:
        raise HTTPException(status_code=status.HTTP_413_REQUEST_ENTITY_TOO_LARGE, detail=str(exc)) from exc
    except ValueError as exc:
        raise HTTPException(status_code=400, detail=str(exc)) from exc
    log_action(db, current_user.id, "archive_files", website.domain, target)
    return {"target": target}


@router.post("/files/extract")
def extract_archive(payload: FileExtract, db: Session = Depends(get_db), current_user: User = Depends(get_current_user)):
    ensure_role(current_user.role, Role.end_user)
    website = get_owned_website(db, current_user, payload.website_id)
    # Validate archive format before queuing to catch bad archives early
    archive_file = file_manager._safe_path(website, payload.archive_path)  # noqa: SLF001
    if not archive_file.exists() or not archive_file.is_file():
        raise HTTPException(status_code=400, detail="Archive not found")
    if archive_file.is_symlink():
        raise HTTPException(status_code=400, detail="Symlinks are not allowed")
    suffix = archive_file.name.lower()
    if not (suffix.endswith(".zip") or suffix.endswith(".tar.gz") or suffix.endswith(".tgz")):
        raise HTTPException(status_code=400, detail="Only .zip, .tar.gz, and .tgz archives are supported")
    # Validate archive is readable and not corrupted
    try:
        if suffix.endswith(".zip"):
            with zipfile.ZipFile(archive_file) as _:
                pass
        else:
            with tarfile.open(archive_file, "r:gz") as _:
                pass
    except zipfile.BadZipFile:
        raise HTTPException(status_code=400, detail="Invalid or corrupted ZIP archive") from None
    except tarfile.TarError:
        raise HTTPException(status_code=400, detail="Invalid or corrupted tar archive") from None
    job = _queue_extract_job(
        current_user,
        website,
        payload.archive_path,
        payload.destination_path,
        True,
    )
    log_action(db, current_user.id, "extract_archive_queued", website.domain, payload.archive_path)
    return {**job, "message": "Extraction started in the background"}


@router.post("/files/write")
def write_file(payload: FileWrite, db: Session = Depends(get_db), current_user: User = Depends(get_current_user)):
    ensure_role(current_user.role, Role.end_user)
    website = get_owned_website(db, current_user, payload.website_id)
    try:
        target = file_manager.write_text_file(
            website,
            payload.path,
            payload.content,
            is_admin_role(current_user.role),
            quota_check=_quota_check_for_website(db, website),
        )
    except storage_quota.StorageQuotaExceeded as exc:
        raise HTTPException(status_code=status.HTTP_413_REQUEST_ENTITY_TOO_LARGE, detail=str(exc)) from exc
    except ValueError as exc:
        raise HTTPException(status_code=400, detail=str(exc)) from exc
    return {"target": target}


@router.post("/files/{website_id}/upload")
def upload_file(
    website_id: int,
    path: str = Query(default=site_users.PUBLIC_DIR),
    file: UploadFile = File(...),
    db: Session = Depends(get_db),
    current_user: User = Depends(get_current_user),
):
    ensure_role(current_user.role, Role.end_user)
    website = get_owned_website(db, current_user, website_id)
    try:
        target = file_manager.upload_file(
            website,
            path,
            file.filename or "upload.bin",
            file.file,
            is_admin_role(current_user.role),
            quota_check=_quota_check_for_website(db, website),
        )
    except storage_quota.StorageQuotaExceeded as exc:
        raise HTTPException(status_code=status.HTTP_413_REQUEST_ENTITY_TOO_LARGE, detail=str(exc)) from exc
    except ValueError as exc:
        raise HTTPException(status_code=400, detail=str(exc)) from exc
    log_action(db, current_user.id, "upload_file", website.domain, target)
    return {"target": target}


@router.delete("/files/{website_id}")
def delete_file(website_id: int, path: str, db: Session = Depends(get_db), current_user: User = Depends(get_current_user)):
    ensure_role(current_user.role, Role.end_user)
    website = get_owned_website(db, current_user, website_id)
    try:
        target = file_manager.delete_file(website, path, is_admin_role(current_user.role))
    except ValueError as exc:
        raise HTTPException(status_code=400, detail=str(exc)) from exc
    return {"deleted": target}
