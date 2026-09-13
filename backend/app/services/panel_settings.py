import calendar
import json
import os
import re
import time
import uuid
from datetime import datetime
from pathlib import Path
from tempfile import NamedTemporaryFile
from urllib.parse import urlparse

from fastapi import HTTPException, UploadFile, status

from app.core.config import settings
from app.services import network
from app.services.shell import shell


SETTINGS_DIR = Path(os.environ.get("opanel_ent_DATA_DIR", "/var/lib/opanel-ent"))
SETTINGS_FILE = SETTINGS_DIR / "panel-settings.json"
ASSETS_DIR = SETTINGS_DIR / "assets"
MAX_ASSET_SIZE = 1024 * 1024
ALLOWED_ASSET_TYPES = {
    "png": "image/png",
    "jpg": "image/jpeg",
    "jpeg": "image/jpeg",
    "webp": "image/webp",
    "ico": "image/x-icon",
}
DOMAIN_RE = re.compile(r"^(?!-)([a-z0-9-]{1,63}\.)+[a-z]{2,}$")
IPV4_RE = re.compile(r"^(\d{1,3}\.){3}\d{1,3}$")


def _read_raw() -> dict:
    try:
        if SETTINGS_FILE.exists():
            return json.loads(SETTINGS_FILE.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError):
        return {}
    return {}


def _write_raw(data: dict) -> None:
    SETTINGS_DIR.mkdir(parents=True, exist_ok=True)
    with NamedTemporaryFile("w", encoding="utf-8", dir=str(SETTINGS_DIR), delete=False) as tmp:
        json.dump(data, tmp, ensure_ascii=True, indent=2, sort_keys=True)
        tmp.write("\n")
        tmp_path = Path(tmp.name)
    tmp_path.replace(SETTINGS_FILE)


def _asset_url(filename: str | None) -> str:
    if not filename:
        return ""
    path = ASSETS_DIR / filename
    if not path.exists():
        return ""
    stat = path.stat()
    version = f"{stat.st_mtime_ns}-{stat.st_size}"
    return f"/brand-assets/{filename}?v={version}"


def _is_ipv4(host: str) -> bool:
    if not IPV4_RE.fullmatch(host):
        return False
    return all(0 <= int(part) <= 255 for part in host.split("."))


def is_domain(host: str) -> bool:
    return bool(DOMAIN_RE.fullmatch((host or "").lower()))


def default_ssl_email(host: str) -> str:
    return f"admin@{host.lower()}" if is_domain(host) else ""


def normalize_panel_hostname(value: str) -> str:
    host = (value or "").strip().lower().rstrip(".")
    if not host:
        raise ValueError("Panel hostname is required")
    if "://" in host or "/" in host or ":" in host:
        raise ValueError("Panel hostname must not include a scheme, port, or path")
    if not is_domain(host) and not _is_ipv4(host) and host != "localhost":
        raise ValueError("Panel hostname must be a domain name or IPv4 address")
    return host


def normalize_panel_port(value: int | str | None) -> int:
    try:
        port = int(value or settings.panel_port or 2222)
    except (TypeError, ValueError) as exc:
        raise ValueError("Panel port is invalid") from exc
    if port < 1 or port > 65535:
        raise ValueError("Panel port is out of range")
    return port


def normalize_panel_url(value: str) -> str:
    value = (value or "").strip()
    if not value:
        raise ValueError("Panel URL is required")
    if "://" not in value:
        value = f"http://{value}"
    parsed = urlparse(value)
    scheme = parsed.scheme.lower()
    host = (parsed.hostname or "").lower()
    if scheme not in {"http", "https"}:
        raise ValueError("Panel URL must start with http:// or https://")
    host = normalize_panel_hostname(host)
    port = normalize_panel_port(parsed.port or settings.panel_port or 2222)
    return f"{scheme}://{host}:{port}"


def panel_url_from_parts(hostname: str, port: int | str | None, scheme: str | None = None) -> str:
    safe_scheme = (scheme or "http").lower()
    if safe_scheme not in {"http", "https"}:
        raise ValueError("Panel URL scheme must be http or https")
    host = normalize_panel_hostname(hostname)
    safe_port = normalize_panel_port(port)
    return f"{safe_scheme}://{host}:{safe_port}"


def parse_panel_url(value: str) -> tuple[str, str, int]:
    normalized = normalize_panel_url(value)
    parsed = urlparse(normalized)
    return parsed.scheme, parsed.hostname or "", parsed.port or 2222


def has_panel_certificate() -> bool:
    cert_pairs = [
        (settings.panel_ssl_cert, settings.panel_ssl_key),
        ("/etc/opanel-ent/panel-fullchain.pem", "/etc/opanel-ent/panel-privkey.pem"),
    ]
    return any(bool(cert) and bool(key) and Path(cert).exists() and Path(key).exists() for cert, key in cert_pairs)


def current_settings() -> dict:
    data = _read_raw()
    app_name = (data.get("app_name") or settings.app_name or "OPanel Enterprise").strip() or "OPanel Enterprise"
    panel_url = data.get("panel_url") or settings.panel_url or ""
    panel_hostname = ""
    panel_port = settings.panel_port or 2222
    if panel_url:
        try:
            _scheme, panel_hostname, panel_port = parse_panel_url(panel_url)
        except ValueError:
            panel_hostname = ""
            panel_port = settings.panel_port or 2222
    ssl_enabled = panel_url.startswith("https://") and has_panel_certificate()
    from app.services import malware_scan

    mw = malware_scan.refresh_status()
    return {
        "app_name": app_name,
        "panel_url": panel_url,
        "panel_hostname": panel_hostname,
        "panel_port": panel_port,
        "logo_url": _asset_url(data.get("logo_filename")),
        "favicon_url": _asset_url(data.get("favicon_filename")) or "/favicon.png",
        "ssl_enabled": ssl_enabled,
        "malware_scan_enabled": mw["enabled"],
        "malware_scan_installed": mw["installed"],
        "malware_scan_active": mw["active"],
        "malware_scan_detail": mw["detail"],
    }


def update_settings(
    app_name: str | None = None,
    panel_hostname: str | None = None,
    panel_port: int | None = None,
    panel_url: str | None = None,
) -> dict:
    del panel_port  # The panel port is install-time only; settings can change hostname/branding.
    data = _read_raw()
    if app_name is not None:
        value = app_name.strip()
        if not 2 <= len(value) <= 80:
            raise ValueError("Panel name must be 2-80 characters")
        data["app_name"] = value
    if (panel_hostname is not None and panel_hostname.strip()) or (panel_url is not None and panel_url.strip()):
        existing_url = data.get("panel_url") or settings.panel_url or ""
        existing_normalized = normalize_panel_url(existing_url) if existing_url else ""
        existing_scheme, existing_host, existing_port = parse_panel_url(existing_normalized) if existing_normalized else ("http", "", settings.panel_port or 2222)
        if panel_hostname is not None and panel_hostname.strip():
            normalized = panel_url_from_parts(panel_hostname, existing_port, existing_scheme)
        elif panel_url is not None and panel_url.strip():
            requested_scheme, requested_host, _requested_port = parse_panel_url(panel_url)
            normalized = panel_url_from_parts(requested_host, existing_port, requested_scheme)
        else:
            normalized = existing_normalized
        if not normalized:
            raise ValueError("Panel hostname is required")
        scheme, host, port = parse_panel_url(normalized)
        if scheme == "https" and not has_panel_certificate():
            raise ValueError("Use Install SSL before saving an HTTPS panel URL")
        if normalized != existing_normalized:
            result = shell.privileged(
                "panel-url-set",
                helper_args=[scheme, host, str(port)],
                check=False,
                fallback=["bash", "-lc", "true"],
            )
            if result.returncode != 0:
                raise RuntimeError((result.stderr or result.stdout or "Could not update panel URL").strip())
        data["panel_url"] = normalized
    _write_raw(data)
    return current_settings()


def detect_asset_type(content: bytes, filename: str) -> tuple[str, str]:
    suffix = Path(filename or "").suffix.lower().lstrip(".")
    if content.startswith(b"\x89PNG\r\n\x1a\n"):
        return "png", "image/png"
    if content.startswith(b"\xff\xd8\xff"):
        return "jpg", "image/jpeg"
    if content.startswith(b"RIFF") and content[8:12] == b"WEBP":
        return "webp", "image/webp"
    if content.startswith(b"\x00\x00\x01\x00"):
        return "ico", "image/x-icon"
    if suffix in ALLOWED_ASSET_TYPES:
        raise ValueError("Uploaded file content does not match its image type")
    raise ValueError("Only PNG, JPG, WEBP, and ICO images are supported")


async def save_asset(kind: str, upload: UploadFile) -> dict:
    if kind not in {"logo", "favicon"}:
        raise ValueError("Invalid asset kind")
    content = await upload.read(MAX_ASSET_SIZE + 1)
    if len(content) > MAX_ASSET_SIZE:
        raise ValueError("Image must be 1 MB or smaller")
    ext, _media_type = detect_asset_type(content, upload.filename or "")
    ASSETS_DIR.mkdir(parents=True, exist_ok=True)
    data = _read_raw()
    previous = data.get(f"{kind}_filename")
    if previous:
        try:
            (ASSETS_DIR / previous).unlink()
        except OSError:
            pass
    filename = f"{kind}.{ext}"
    (ASSETS_DIR / filename).write_bytes(content)
    data[f"{kind}_filename"] = filename
    _write_raw(data)
    return current_settings()


def asset_path(filename: str) -> tuple[Path, str]:
    if not re.fullmatch(r"(?:logo|favicon)\.(?:png|jpg|jpeg|webp|ico)", filename or ""):
        raise HTTPException(status_code=status.HTTP_404_NOT_FOUND, detail="Not found")
    path = ASSETS_DIR / filename
    if not path.exists():
        raise HTTPException(status_code=status.HTTP_404_NOT_FOUND, detail="Not found")
    media_type = ALLOWED_ASSET_TYPES.get(path.suffix.lower().lstrip("."), "application/octet-stream")
    return path, media_type


def install_panel_ssl(email: str | None = None, panel_hostname: str | None = None, panel_port: int | None = None, panel_url: str | None = None) -> dict:
    if panel_hostname:
        normalized = panel_url_from_parts(panel_hostname, panel_port, "http")
    elif panel_url:
        normalized = normalize_panel_url(panel_url)
    else:
        raise ValueError("Panel hostname is required")
    _scheme, host, port = parse_panel_url(normalized)
    if not is_domain(host):
        raise ValueError("Panel SSL requires a domain name, not an IP address")
    certbot_email = (email or settings.ssl_email or default_ssl_email(host)).strip()
    helper_args = [host, str(port)]
    if certbot_email:
        helper_args.append(certbot_email)
    result = shell.privileged(
        "panel-ssl-install",
        helper_args=helper_args,
        check=False,
        fallback=["bash", "-lc", "true"],
    )
    if result.returncode != 0:
        raise RuntimeError((result.stderr or result.stdout or "Could not install panel SSL").strip())
    data = _read_raw()
    data["panel_url"] = f"https://{host}:{port}"
    _write_raw(data)
    current = current_settings()
    current["message"] = result.stdout.strip() or f"Panel SSL enabled for {host}"
    return current


# --------------------------------------------------------------------------
# Optional ClamAV malware scanning
# --------------------------------------------------------------------------
import threading

from app.services import malware_scan as _malware_scan

MALWARE_JOBS: dict[str, dict] = {}
MALWARE_JOB_THREADS: dict[str, threading.Thread] = {}
MALWARE_JOBS_LOCK = threading.RLock()
MALWARE_JOBS_DIR = SETTINGS_DIR / "malware-scan-jobs"
MAX_MALWARE_LOG_LINES = 1000
# A job whose progress file has not moved for this long is treated as dead.
MALWARE_JOB_STALE_SECONDS = 300


def _persist_malware_enabled(enabled: bool) -> None:
    """Write the malware_scan_enabled flag to the panel settings file so it
    survives restarts. The flag is the source of truth that the API reads via
    ``settings.malware_scan_enabled`` (overridden here from persisted state)."""
    data = _read_raw()
    data["malware_scan_enabled"] = bool(enabled)
    _write_raw(data)


def malware_scan_status() -> dict:
    return _malware_scan.refresh_status()


def set_malware_scan(enabled: bool) -> dict:
    """Toggle malware scanning on/off.

    If enabling and ClamAV is not yet installed, the install runs in a
    background thread so the API call returns immediately. If disabling, the
    flag is simply turned off (clamd is left installed but idle).
    """
    enabled = bool(enabled)
    if enabled and not _malware_scan.clamav_installed():
        # Kick off install + enable in the background; mark flag intent now.
        _persist_malware_enabled(True)
        threading.Thread(target=_install_and_enable_flow, daemon=True).start()
        current = current_settings()
        current["message"] = (
            "ClamAV is being installed in the background. Scanning will activate "
            "automatically once clamav-daemon is running."
        )
        return current
    _persist_malware_enabled(enabled)
    current = current_settings()
    if enabled:
        current["message"] = "Malware scanning enabled"
    else:
        stopped = _malware_scan.stop_clamd()
        current = current_settings()
        current["message"] = (
            "Malware scanning disabled and clamav-daemon stopped"
            if stopped
            else "Malware scanning disabled, but clamav-daemon could not be stopped"
        )
    return current


def set_malware_realtime(enabled: bool) -> dict:
    """Toggle LMD real-time protection (mode 1 <-> mode 2).

    Persists the flag and (de)activates the inotify monitor. Enabling requires
    malware scanning to already be on so LMD is installed.
    """
    enabled = bool(enabled)
    data = _read_raw()
    was = bool(data.get("malware_realtime_enabled", False))
    if enabled == was:
        current = current_settings()
        current["message"] = "Real-time protection unchanged"
        return current
    if enabled and not data.get("malware_scan_enabled"):
        raise ValueError("Enable malware scanning first.")
    data["malware_realtime_enabled"] = enabled
    _write_raw(data)
    try:
        _malware_scan.set_realtime(enabled)
    except Exception as exc:  # noqa: BLE001
        data["malware_realtime_enabled"] = was
        _write_raw(data)
        raise ValueError(str(exc)) from exc
    current = current_settings()
    current["message"] = (
        "Real-time protection enabled (LMD inotify monitor on /home)"
        if enabled
        else "Real-time protection disabled -- back to scheduled scans"
    )
    return current


def add_linux_malware_detect() -> dict:
    """Install LMD on a box that already runs ClamAV but predates the feature.

    Runs the clone + install in the background so the API call returns at once;
    the Malware Scanner page reflects ``lmd_installed`` once it finishes.
    """
    status = _malware_scan.refresh_status()
    if not status.get("installed"):
        raise ValueError("Install ClamAV first, then add Linux Malware Detect.")
    if status.get("lmd_installed"):
        current = current_settings()
        current["message"] = "Linux Malware Detect is already installed"
        return current
    threading.Thread(target=_add_lmd_flow, daemon=True).start()
    current = current_settings()
    current["message"] = (
        "Linux Malware Detect is being installed in the background. It will show "
        "as active on this page once the signature download finishes."
    )
    return current


def _add_lmd_flow() -> None:
    try:
        _malware_scan.install_lmd()
    except Exception as exc:  # noqa: BLE001 - record failure, do not crash thread
        state = _malware_scan.refresh_status()
        state["detail"] = f"Linux Malware Detect install failed: {exc}"
        _malware_scan._write_status(state)


def set_malware_auto_quarantine(enabled: bool) -> dict:
    """Toggle whether a scan moves a detected file to quarantine automatically."""
    data = _read_raw()
    data["malware_auto_quarantine"] = bool(enabled)
    _write_raw(data)
    current = current_settings()
    current["message"] = (
        "Detected malware will be quarantined automatically"
        if enabled
        else "Auto-quarantine off -- scans only report threats now"
    )
    return current


def _install_and_enable_flow() -> None:
    """Background worker: install ClamAV, then leave the persisted flag on.

    The flag was already persisted as True by set_malware_scan(); this just
    performs the heavy install. Failures are captured in the status file.
    """
    try:
        _malware_scan.install_clamav()
    except Exception as exc:  # noqa: BLE001 - record failure, do not crash thread
        _malware_scan._write_status(
            {
                "installed": _malware_scan.clamav_installed(),
                "clamd_running": False,
                "enabled": True,
                "active": False,
                "socket": _malware_scan._socket_path(),
                "detail": f"ClamAV install failed: {exc}",
            }
        )


def _now_iso() -> str:
    return time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())


def _public_malware_job(job: dict) -> dict:
    return dict(job)


def _job_age_seconds(job: dict) -> float:
    stamp = job.get("updated_at") or job.get("started_at") or job.get("created_at") or ""
    try:
        return max(0.0, time.time() - calendar.timegm(time.strptime(stamp, "%Y-%m-%dT%H:%M:%SZ")))
    except (ValueError, TypeError):
        return float("inf")


def _finalize_stale_malware_job(job: dict) -> dict:
    if job.get("status") not in {"queued", "running"}:
        return job
    # Scheduled scans run in the systemd timer process, so the API has no
    # thread for them. Recent progress is what proves such a job is alive.
    if _job_age_seconds(job) < MALWARE_JOB_STALE_SECONDS:
        return job
    with MALWARE_JOBS_LOCK:
        thread = MALWARE_JOB_THREADS.get(job.get("job_id"))
        if thread and thread.is_alive():
            return job
        MALWARE_JOB_THREADS.pop(job.get("job_id"), None)
    job = dict(job)
    job.update(
        status="interrupted",
        message="Scan interrupted. Start a new scan to continue.",
        error="Scan worker is no longer running.",
        finished_at=job.get("finished_at") or _now_iso(),
        updated_at=_now_iso(),
    )
    _write_malware_job(job)
    with MALWARE_JOBS_LOCK:
        MALWARE_JOBS[job["job_id"]] = job
    return job


def _write_malware_job(job: dict) -> None:
    try:
        MALWARE_JOBS_DIR.mkdir(parents=True, exist_ok=True)
        path = MALWARE_JOBS_DIR / f"{job['job_id']}.json"
        path.write_text(json.dumps(_public_malware_job(job), indent=2) + "\n", encoding="utf-8")
    except OSError:
        pass


def _remember_malware_job(job: dict) -> dict:
    with MALWARE_JOBS_LOCK:
        MALWARE_JOBS[job["job_id"]] = job
    _write_malware_job(job)
    return _public_malware_job(job)


def _update_malware_job(job_id: str, **updates) -> None:
    with MALWARE_JOBS_LOCK:
        job = MALWARE_JOBS.get(job_id)
        if not job:
            return
        updates.setdefault("updated_at", _now_iso())
        job.update(updates)
        if len(job.get("log", [])) > MAX_MALWARE_LOG_LINES:
            job["log"] = job["log"][-MAX_MALWARE_LOG_LINES:]
        snapshot = dict(job)
    _write_malware_job(snapshot)


def _append_malware_log(job_id: str, line: str) -> None:
    with MALWARE_JOBS_LOCK:
        job = MALWARE_JOBS.get(job_id)
        if not job:
            return
        job.setdefault("log", []).append(f"{_now_iso()} {line}")
        job["updated_at"] = _now_iso()
        if len(job["log"]) > MAX_MALWARE_LOG_LINES:
            job["log"] = job["log"][-MAX_MALWARE_LOG_LINES:]
        snapshot = dict(job)
    _write_malware_job(snapshot)


def get_malware_scan_job(job_id: str) -> dict:
    with MALWARE_JOBS_LOCK:
        job = MALWARE_JOBS.get(job_id)
        if job:
            return _public_malware_job(_finalize_stale_malware_job(job))
    path = MALWARE_JOBS_DIR / f"{job_id}.json"
    try:
        if path.exists():
            return _public_malware_job(_finalize_stale_malware_job(json.loads(path.read_text(encoding="utf-8"))))
    except (OSError, json.JSONDecodeError):
        pass
    raise ValueError("Scan job not found")


def get_latest_malware_scan_job() -> dict:
    with MALWARE_JOBS_LOCK:
        jobs = list(MALWARE_JOBS.values())
    try:
        if MALWARE_JOBS_DIR.exists():
            for path in MALWARE_JOBS_DIR.glob("*.json"):
                try:
                    jobs.append(json.loads(path.read_text(encoding="utf-8")))
                except (OSError, json.JSONDecodeError):
                    continue
    except OSError:
        pass
    unique: dict[str, dict] = {}
    for job in jobs:
        job_id = job.get("job_id")
        if not job_id:
            continue
        current = unique.get(job_id)
        if not current or (job.get("updated_at") or job.get("started_at") or job.get("created_at") or "") >= (
            current.get("updated_at") or current.get("started_at") or current.get("created_at") or ""
        ):
            unique[job_id] = job
    if not unique:
        raise ValueError("Scan job not found")
    running = [job for job in unique.values() if job.get("status") in {"queued", "running"}]
    candidates = running or list(unique.values())
    return _public_malware_job(
        _finalize_stale_malware_job(max(candidates, key=lambda job: job.get("updated_at") or job.get("started_at") or job.get("created_at") or ""))
    )


def list_malware_scan_jobs(limit: int = 50) -> list[dict]:
    jobs = []
    with MALWARE_JOBS_LOCK:
        jobs.extend(MALWARE_JOBS.values())
    try:
        if MALWARE_JOBS_DIR.exists():
            for path in MALWARE_JOBS_DIR.glob("*.json"):
                try:
                    jobs.append(json.loads(path.read_text(encoding="utf-8")))
                except (OSError, json.JSONDecodeError):
                    continue
    except OSError:
        pass
    unique: dict[str, dict] = {}
    for job in jobs:
        job_id = job.get("job_id")
        if not job_id:
            continue
        current = unique.get(job_id)
        stamp = job.get("updated_at") or job.get("started_at") or job.get("created_at") or ""
        current_stamp = current.get("updated_at") or current.get("started_at") or current.get("created_at") or "" if current else ""
        if not current or stamp >= current_stamp:
            unique[job_id] = job
    sorted_jobs = sorted(
        (_finalize_stale_malware_job(job) for job in unique.values()),
        key=lambda job: job.get("updated_at") or job.get("started_at") or job.get("created_at") or "",
        reverse=True,
    )
    return [_public_malware_job(job) for job in sorted_jobs[: max(1, min(limit, 200))]]


def _select_scan_websites(website_id: int | None, db) -> list:
    from app.models.entities import Website

    if website_id is None:
        return db.query(Website).order_by(Website.domain.asc()).all()
    website = db.query(Website).filter(Website.id == website_id).first()
    if not website:
        raise ValueError("Website not found")
    return [website]


def start_scan_job(website_id: int | None, db) -> dict:
    """Start a background malware scan for one website or all websites."""
    websites = _select_scan_websites(website_id, db)
    if not websites:
        raise ValueError("No websites found")
    targets = []
    for website in websites:
        root_path = website.root_path
        if not root_path or not Path(root_path).is_dir():
            raise ValueError(f"Website root not found: {root_path}")
        targets.append({"id": website.id, "domain": website.domain, "root_path": root_path})

    job_id = uuid.uuid4().hex
    job = {
        "job_id": job_id,
        "status": "queued",
        "scope": "all" if website_id is None else "website",
        "website_id": website_id,
        "domains": [target["domain"] for target in targets],
        "message": "Queued",
        "progress_percent": 0,
        "total_files": 0,
        "scanned": 0,
        "infected": 0,
        "errors": 0,
        "skipped": 0,
        "threats": [],
        "log": [],
        "created_at": _now_iso(),
        "started_at": "",
        "finished_at": "",
        "updated_at": _now_iso(),
    }
    _remember_malware_job(job)
    thread = threading.Thread(target=_run_scan_job, args=(job_id, targets), daemon=True)
    with MALWARE_JOBS_LOCK:
        MALWARE_JOB_THREADS[job_id] = thread
    thread.start()
    return _public_malware_job(job)


def _run_scan_job(job_id: str, targets: list[dict]) -> None:
    _update_malware_job(job_id, status="running", started_at=_now_iso(), message="Preparing scan")
    try:
        if not _malware_scan.clamd_running():
            raise RuntimeError("ClamAV daemon is not running. Enable malware scanning first.")
        target_files: list[tuple[dict, str]] = []
        skipped = 0
        for target in targets:
            files, skipped_lines = _malware_scan.collect_regular_files(target["root_path"])
            skipped += len(skipped_lines)
            _append_malware_log(job_id, f"Prepared {target['domain']}: {len(files)} files")
            for line in skipped_lines:
                _append_malware_log(job_id, line)
            target_files.extend((target, file_path) for file_path in files)
        total = len(target_files)
        _update_malware_job(job_id, total_files=total, skipped=skipped, message=f"Scanning 0/{total} files")

        threats = []
        errors = 0
        scanned = 0
        for target, file_path in target_files:
            result = _malware_scan.scan_file_with_clamdscan(file_path)
            scanned += 1
            status = result["status"]
            if status == "infected":
                _append_malware_log(job_id, f"INFECTED {target['domain']} {file_path}: {result['signature']}")
                quarantined = _maybe_quarantine_threat(job_id, file_path, result["signature"], target["domain"])
                threats.append({
                    "path": file_path, "signature": result["signature"],
                    "domain": target["domain"], "quarantined": quarantined,
                })
            elif status == "error":
                errors += 1
                _append_malware_log(job_id, f"ERROR {target['domain']} {file_path}: {result['detail']}")
            elif status == "skipped":
                skipped += 1
                _append_malware_log(job_id, f"SKIP {target['domain']} {file_path}: {result['detail']}")

            percent = int((scanned / total) * 100) if total else 100
            _update_malware_job(
                job_id,
                scanned=scanned,
                infected=len(threats),
                errors=errors,
                skipped=skipped,
                threats=threats,
                progress_percent=percent,
                message=f"Scanning {scanned}/{total} files",
            )

        status = "infected" if threats else "done"
        _update_malware_job(
            job_id,
            status=status,
            progress_percent=100,
            scanned=scanned,
            infected=len(threats),
            errors=errors,
            skipped=skipped,
            threats=threats,
            message=f"Scan complete: {scanned} files, {len(threats)} threats, {errors} errors",
            finished_at=_now_iso(),
        )
        _append_malware_log(job_id, "Scan finished")
    except Exception as exc:  # noqa: BLE001 - scan jobs should report errors, not crash the API
        _update_malware_job(
            job_id,
            status="error",
            message="Scan failed",
            error=str(exc),
            finished_at=_now_iso(),
        )
        _append_malware_log(job_id, f"ERROR scan failed: {exc}")
    finally:
        with MALWARE_JOBS_LOCK:
            MALWARE_JOB_THREADS.pop(job_id, None)


# ---------------------------------------------------------------------------
# Server addresses / IPv6
# ---------------------------------------------------------------------------
IPV6_SETTING_KEY = "ipv6_enabled"


def network_status() -> dict:
    """Addresses the server answers on, plus the state of the IPv6 switch."""
    addresses = network.detect_addresses()
    stored = network.read_stored_flag(_read_raw(), IPV6_SETTING_KEY)
    available = bool(addresses["ipv6"])
    return {
        "ipv4": addresses["ipv4"],
        "ipv6": addresses["ipv6"],
        "ipv6_available": available,
        # Never asked: follow the server. A box installed with IPv6 serves it
        # from day one, and a block added later is picked up on the next read
        # instead of waiting for a reinstall.
        "ipv6_enabled": available if stored is None else stored,
        "ipv6_configured": stored is not None,
        "message": "",
    }


def set_ipv6(enabled: bool) -> dict:
    enabled = bool(enabled)
    if enabled and not network.ipv6_available():
        raise ValueError("This server has no global IPv6 address")
    data = _read_raw()
    previous = data.get(IPV6_SETTING_KEY)
    data[IPV6_SETTING_KEY] = enabled
    _write_raw(data)
    try:
        message = network.apply_ipv6(enabled)
    except RuntimeError:
        # The listeners are what actually serve IPv6; if they could not be
        # rebuilt, the stored flag must not claim otherwise.
        data = _read_raw()
        if previous is None:
            data.pop(IPV6_SETTING_KEY, None)
        else:
            data[IPV6_SETTING_KEY] = previous
        _write_raw(data)
        raise
    status = network_status()
    status["message"] = message or (
        "IPv6 enabled. Sites and the panel now answer on IPv6."
        if enabled
        else "IPv6 disabled. Sites and the panel answer on IPv4 only."
    )
    return status


# ---------------------------------------------------------------------------
# Scheduled scans
# ---------------------------------------------------------------------------
# Two independent schedules, each with a fixed scope:
#   "system" -> "/"     the whole VPS (clamdscan + a prune list, always full)
#   "web"    -> "/home"  every website's files (LMD, incremental between full runs)
# scan_root is derived from the scope and never taken from the client.
MALWARE_SCHEDULE_KEY = "malware_scan_schedule"          # legacy single schedule
MALWARE_SCHEDULES_KEY = "malware_scan_schedules"        # {scope: schedule}
MALWARE_SCHEDULE_STATE_FILE = SETTINGS_DIR / "malware-scan-schedule-state.json"
MALWARE_SCHEDULE_FREQUENCIES = ("hourly", "daily", "weekly", "monthly")
MALWARE_SCAN_SCOPES = {"system": "/", "web": "/home"}
MALWARE_SCHEDULE_DEFAULTS = {
    "enabled": False,
    "frequency": "weekly",
    "hour": 3,
    "minute": 0,
    "weekday": 0,
    "day": 1,
    "scan_root": "/",
}
MALWARE_SCHEDULE_SCOPE_DEFAULTS = {
    "system": {"enabled": False, "frequency": "weekly", "weekday": 0, "hour": 2, "minute": 0, "day": 1},
    "web": {"enabled": False, "frequency": "daily", "hour": 3, "minute": 30, "weekday": 0, "day": 1},
}


def _clamp_int(value, low: int, high: int, default: int) -> int:
    try:
        number = int(value)
    except (TypeError, ValueError):
        return default
    return max(low, min(high, number))


def _scope_scan_root(scope: str) -> str:
    try:
        return MALWARE_SCAN_SCOPES[scope]
    except KeyError:
        raise ValueError(f"Unknown scan scope: {scope}") from None


def normalize_malware_schedule(raw, scope: str | None = None) -> dict:
    """Coerce stored or posted schedule fields into a complete, valid schedule.

    When *scope* is given, scan_root is forced to that scope's fixed path and
    any scan_root in *raw* is ignored.
    """
    data = raw if isinstance(raw, dict) else {}
    frequency = str(data.get("frequency") or MALWARE_SCHEDULE_DEFAULTS["frequency"]).strip().lower()
    if frequency not in MALWARE_SCHEDULE_FREQUENCIES:
        frequency = MALWARE_SCHEDULE_DEFAULTS["frequency"]
    if scope is not None:
        scan_root = _scope_scan_root(scope)
    else:
        scan_root = str(data.get("scan_root") or "/").strip() or "/"
        if not scan_root.startswith("/"):
            raise ValueError("Scan path must be absolute")
        if len(scan_root) > 1:
            scan_root = scan_root.rstrip("/") or "/"
    return {
        "enabled": bool(data.get("enabled", MALWARE_SCHEDULE_DEFAULTS["enabled"])),
        "frequency": frequency,
        "hour": _clamp_int(data.get("hour"), 0, 23, MALWARE_SCHEDULE_DEFAULTS["hour"]),
        "minute": _clamp_int(data.get("minute"), 0, 59, MALWARE_SCHEDULE_DEFAULTS["minute"]),
        # 0 = Sunday, matching the weekday numbering used in the panel UI.
        "weekday": _clamp_int(data.get("weekday"), 0, 6, MALWARE_SCHEDULE_DEFAULTS["weekday"]),
        # Capped at 28 so a monthly schedule fires in February too.
        "day": _clamp_int(data.get("day"), 1, 28, MALWARE_SCHEDULE_DEFAULTS["day"]),
        "scan_root": scan_root,
    }


def _legacy_schedule_scope(legacy: dict) -> str:
    """Which of the two scopes an old single schedule maps onto."""
    root = str((legacy or {}).get("scan_root") or "/").rstrip("/") or "/"
    return "web" if root.startswith("/home") else "system"


def _read_malware_schedule_state() -> dict:
    try:
        if MALWARE_SCHEDULE_STATE_FILE.exists():
            state = json.loads(MALWARE_SCHEDULE_STATE_FILE.read_text(encoding="utf-8"))
            return state if isinstance(state, dict) else {}
    except (OSError, json.JSONDecodeError):
        return {}
    return {}


def _write_malware_schedule_state(state: dict) -> None:
    try:
        SETTINGS_DIR.mkdir(parents=True, exist_ok=True)
        with NamedTemporaryFile("w", encoding="utf-8", dir=str(SETTINGS_DIR), delete=False) as tmp:
            print(json.dumps(state, ensure_ascii=True, indent=2, sort_keys=True), file=tmp)
            tmp_path = Path(tmp.name)
        tmp_path.replace(MALWARE_SCHEDULE_STATE_FILE)
    except OSError:
        pass


def write_malware_schedule_state(**updates) -> dict:
    """Record top-level scheduler state (currently just the last full-scan
    baseline that the incremental/full decision reads).

    Kept out of panel-settings.json on purpose: the scheduler process writes
    this after every run, and a read-modify-write race against the settings API
    would otherwise be able to drop an unrelated setting.
    """
    state = _read_malware_schedule_state()
    state.update(updates)
    _write_malware_schedule_state(state)
    return state


def write_malware_schedule_scope_state(scope: str, **updates) -> dict:
    """Record the outcome of one scope's last scheduled run."""
    state = _read_malware_schedule_state()
    entry = state.get(scope)
    entry = dict(entry) if isinstance(entry, dict) else {}
    entry.update(updates)
    state[scope] = entry
    _write_malware_schedule_state(state)
    return state


def _schedule_scope_state(state: dict, scope: str) -> dict:
    entry = state.get(scope)
    return entry if isinstance(entry, dict) else {}


def malware_schedules() -> dict:
    """Return both scan schedules, keyed by scope, each with its last-run info."""
    raw = _read_raw()
    stored = raw.get(MALWARE_SCHEDULES_KEY)
    legacy = raw.get(MALWARE_SCHEDULE_KEY)
    legacy_scope = _legacy_schedule_scope(legacy) if isinstance(legacy, dict) else None
    state = _read_malware_schedule_state()
    out: dict[str, dict] = {}
    for scope in MALWARE_SCAN_SCOPES:
        base = dict(MALWARE_SCHEDULE_SCOPE_DEFAULTS[scope])
        if isinstance(stored, dict) and isinstance(stored.get(scope), dict):
            base.update(stored[scope])
        elif legacy_scope == scope and isinstance(legacy, dict):
            base.update({k: v for k, v in legacy.items() if k != "scan_root"})
        schedule = normalize_malware_schedule(base, scope=scope)
        schedule["scope"] = scope
        sk = _schedule_scope_state(state, scope)
        schedule.update(
            last_run_at=sk.get("last_run_at") or "",
            last_status=sk.get("last_status") or "",
            last_message=sk.get("last_message") or "",
            last_job_id=sk.get("last_job_id") or "",
        )
        out[scope] = schedule
    return out


def set_malware_schedules(payload) -> dict:
    """Persist one or both schedules. Only the scopes present in *payload* are
    touched; scan_root is always forced from the scope."""
    data = payload if isinstance(payload, dict) else {}
    raw = _read_raw()
    stored = raw.get(MALWARE_SCHEDULES_KEY)
    stored = dict(stored) if isinstance(stored, dict) else {}
    for scope in MALWARE_SCAN_SCOPES:
        entry = data.get(scope)
        if isinstance(entry, dict):
            stored[scope] = normalize_malware_schedule(entry, scope=scope)
    raw[MALWARE_SCHEDULES_KEY] = stored
    raw.pop(MALWARE_SCHEDULE_KEY, None)  # legacy single schedule folded in
    _write_raw(raw)
    return malware_schedules()


# A full-filesystem scan walks hundreds of thousands of files. Writing the job
# file on every one of them would cost more IO than the scan itself, so
# progress is flushed on a file/second budget instead.
SYSTEM_SCAN_FLUSH_FILES = 250
SYSTEM_SCAN_FLUSH_SECONDS = 3.0
SYSTEM_SCAN_MAX_ERROR_LINES = 100
# With LMD installed, scans default to incremental (files changed in the last few
# days). A full scan still runs the first time and at least this often after.
FULL_SCAN_INTERVAL_DAYS = 7


def _resolve_scan_mode(requested: str) -> str:
    """auto -> full/incremental. Only LMD does incremental; without it, always full."""
    if requested in ("full", "incremental"):
        return requested
    status = _malware_scan._lmd_status()
    if not status.get("lmd_installed"):
        return "full"
    last_full = _read_malware_schedule_state().get("last_full_scan_at") or ""
    if not last_full:
        return "full"
    try:
        when = datetime.fromisoformat(last_full.replace("Z", "+00:00"))
        age_days = (datetime.now(when.tzinfo) - when).total_seconds() / 86400
    except ValueError:
        return "full"
    return "full" if age_days >= FULL_SCAN_INTERVAL_DAYS else "incremental"


def _new_malware_job(scope: str, **fields) -> dict:
    job = {
        "job_id": uuid.uuid4().hex,
        "status": "queued",
        "scope": scope,
        "website_id": None,
        "domains": [],
        "scan_root": "",
        "scan_mode": "",
        "trigger": "manual",
        "message": "Queued",
        "progress_percent": 0,
        "total_files": 0,
        "scanned": 0,
        "infected": 0,
        "errors": 0,
        "skipped": 0,
        "threats": [],
        "log": [],
        "error": "",
        "created_at": _now_iso(),
        "started_at": "",
        "finished_at": "",
        "updated_at": _now_iso(),
    }
    job.update(fields)
    return job


def _auto_quarantine_enabled() -> bool:
    return bool(_read_raw().get("malware_auto_quarantine", settings.malware_auto_quarantine))


_QUARANTINE_ELIGIBLE_PREFIXES = ("/home/", "/tmp/", "/var/tmp/", "/dev/shm/", "/var/www/")


def _maybe_quarantine_threat(job_id: str, path: str, signature: str, domain: str = "") -> bool:
    """Move a detected malware file to quarantine. Returns True when it was."""
    if not _auto_quarantine_enabled():
        return False
    if not any(path.startswith(p) for p in _QUARANTINE_ELIGIBLE_PREFIXES):
        return False
    try:
        _malware_scan.quarantine_file(path, signature)
        _append_malware_log(job_id, f"QUARANTINED {(domain + ' ') if domain else ''}{path}")
        return True
    except Exception as exc:  # noqa: BLE001 - a failed move must not abort the scan
        _append_malware_log(job_id, f"QUARANTINE FAILED {path}: {exc}")
        return False


def start_system_scan_job(scan_root: str = "/", trigger: str = "manual", background: bool = True,
                          mode: str = "auto") -> dict:
    """Scan the whole server, not just website roots.

    Website scans only ever look at document roots, so malware parked in
    /tmp, /root, a user's home outside public_html or a system directory stays
    invisible. This walks the filesystem as root through the helper.
    """
    scan_root = (scan_root or "/").strip() or "/"
    if not scan_root.startswith("/"):
        raise ValueError("Scan path must be absolute")
    # A whole-VPS scan runs through clamdscan, which has no incremental mode.
    resolved = "full" if scan_root == "/" else _resolve_scan_mode(mode)
    job = _new_malware_job("system", scan_root=scan_root, trigger=trigger, scan_mode=resolved,
                           message=f"Queued {resolved} system scan")
    _remember_malware_job(job)
    job_id = job["job_id"]
    if not background:
        _run_system_scan_job(job_id, scan_root, resolved)
        return get_malware_scan_job(job_id)
    thread = threading.Thread(target=_run_system_scan_job, args=(job_id, scan_root, resolved), daemon=True)
    with MALWARE_JOBS_LOCK:
        MALWARE_JOB_THREADS[job_id] = thread
    thread.start()
    return _public_malware_job(job)


def _run_system_scan_job(job_id: str, scan_root: str, mode: str = "full") -> None:
    _update_malware_job(job_id, status="running", started_at=_now_iso(),
                        message=f"Starting {mode} scan of {scan_root}")
    total = 0
    scanned = 0
    errors = 0
    error_lines = 0
    threats: list[dict] = []
    unparsed_tail: list[str] = []
    try:
        process = _malware_scan.system_scan_process(scan_root, mode)
        last_flush = time.monotonic()

        def flush(message: str) -> None:
            _update_malware_job(
                job_id,
                total_files=total,
                scanned=scanned,
                infected=len(threats),
                errors=errors,
                threats=threats,
                progress_percent=int((scanned / total) * 100) if total else 0,
                message=message,
            )

        for raw_line in process.stdout or []:
            line = raw_line.rstrip()
            if line.startswith(_malware_scan.SYSTEM_SCAN_TOTAL_PREFIX):
                try:
                    total = int(line[len(_malware_scan.SYSTEM_SCAN_TOTAL_PREFIX):].strip() or 0)
                except ValueError:
                    total = 0
                _append_malware_log(job_id, f"Found {total} files to scan under {scan_root}")
                flush(f"Scanning 0/{total} files")
                continue
            if line.startswith(_malware_scan.SYSTEM_SCAN_SCANNED_PREFIX):
                # LMD reports one total instead of a line per file.
                try:
                    scanned = int(line[len(_malware_scan.SYSTEM_SCAN_SCANNED_PREFIX):].strip() or 0)
                except ValueError:
                    pass
                continue
            if line.startswith(_malware_scan.SYSTEM_SCAN_MODE_PREFIX) or line.startswith(_malware_scan.SYSTEM_SCAN_REPORT_PREFIX):
                _append_malware_log(job_id, line.strip())
                continue
            result = _malware_scan.parse_clamdscan_line(line)
            if result is None:
                if line.strip():
                    _append_malware_log(job_id, line.strip())
                    unparsed_tail = [*unparsed_tail[-2:], line.strip()]
                continue
            scanned += 1
            if result["status"] == "infected":
                _append_malware_log(job_id, f"INFECTED {result['path']}: {result['signature']}")
                quarantined = _maybe_quarantine_threat(job_id, result["path"], result["signature"])
                threats.append({
                    "path": result["path"], "signature": result["signature"],
                    "domain": "", "quarantined": quarantined,
                })
            elif result["status"] == "error":
                errors += 1
                # Unreadable special files are common and uninteresting; keep a
                # sample in the log and just count the rest.
                if error_lines < SYSTEM_SCAN_MAX_ERROR_LINES:
                    error_lines += 1
                    _append_malware_log(job_id, f"ERROR {result['path']}: {result['detail']}")
            now = time.monotonic()
            if scanned % SYSTEM_SCAN_FLUSH_FILES == 0 or now - last_flush >= SYSTEM_SCAN_FLUSH_SECONDS:
                last_flush = now
                flush(f"Scanning {scanned}/{total} files" if total else f"Scanned {scanned} files")

        returncode = process.wait()
        # clamdscan exits 1 when it finds something and the helper exits 1 when
        # it refuses to start, so the exit code alone cannot tell those apart --
        # a run that scanned nothing and found nothing is the failure.
        if returncode != 0 and scanned == 0 and not threats:
            detail = "; ".join(unparsed_tail) or "no output"
            raise RuntimeError(f"Scan command failed (exit {returncode}): {detail}")
        status = "infected" if threats else "done"
        if mode == "full":
            write_malware_schedule_state(last_full_scan_at=_now_iso())
        _update_malware_job(
            job_id,
            status=status,
            progress_percent=100,
            total_files=total,
            scanned=scanned,
            infected=len(threats),
            errors=errors,
            threats=threats,
            message=f"{mode.title()} scan complete: {scanned} files, {len(threats)} threats, {errors} errors",
            finished_at=_now_iso(),
        )
        _append_malware_log(job_id, "Scan finished")
    except Exception as exc:  # noqa: BLE001 - scan jobs report errors, they do not crash the API
        _update_malware_job(
            job_id,
            status="error",
            message="Scan failed",
            error=str(exc),
            scanned=scanned,
            infected=len(threats),
            errors=errors,
            threats=threats,
            finished_at=_now_iso(),
        )
        _append_malware_log(job_id, f"ERROR scan failed: {exc}")
    finally:
        with MALWARE_JOBS_LOCK:
            MALWARE_JOB_THREADS.pop(job_id, None)


def run_scan(website_id: int, db) -> dict:
    """Trigger an on-demand malware scan of a website's root directory."""
    from app.models.entities import Website

    website = db.query(Website).filter(Website.id == website_id).first()
    if not website:
        raise ValueError("Website not found")
    root_path = website.root_path
    if not root_path or not Path(root_path).is_dir():
        raise ValueError(f"Website root not found: {root_path}")
    return _malware_scan.scan_directory(root_path)
