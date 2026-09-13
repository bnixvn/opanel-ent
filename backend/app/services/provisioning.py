"""Provisioning service â€” bridges WHMCS billing to OPanel Enterprise runtime.

Every public function is idempotent where the contract requires it.
"""

import hashlib
import json
import secrets
from datetime import datetime, timedelta
from pathlib import Path
from typing import Optional

from sqlalchemy.orm import Session

from app.core.config import settings
from app.core.security import hash_password
from app.models.entities import (
    ApiToken,
    DatabaseAccount,
    HostingAccount,
    HostingPlan,
    PanelSsoToken,
    ProvisioningJob,
    User,
    Website,
)
from app.services import mariadb, openlitespeed, site_users, ssl, storage_quota, wordpress
from app.services.shell import shell

# ---------------------------------------------------------------------------
# Plans
# ---------------------------------------------------------------------------

def list_plans(db: Session) -> list[HostingPlan]:
    return db.query(HostingPlan).order_by(HostingPlan.id).all()


def get_plan(db: Session, plan_id: int) -> Optional[HostingPlan]:
    return db.query(HostingPlan).filter(HostingPlan.id == plan_id).first()


def create_plan(db: Session, *, slug: str, name: str, website_limit: int = 1,
                storage_limit_mb: int = 1024, php_version: str = "8.4",
                app_type: str = "php", auto_ssl: bool = False) -> HostingPlan:
    if db.query(HostingPlan).filter(HostingPlan.slug == slug).first():
        raise ValueError(f"Plan slug '{slug}' already exists")
    plan = HostingPlan(
        slug=slug, name=name, website_limit=website_limit,
        storage_limit_mb=storage_limit_mb, php_version=php_version,
        app_type=app_type, auto_ssl=auto_ssl,
    )
    db.add(plan)
    db.commit()
    db.refresh(plan)
    return plan


def update_plan(db: Session, plan_id: int, **kwargs) -> HostingPlan:
    plan = db.query(HostingPlan).filter(HostingPlan.id == plan_id).first()
    if plan is None:
        raise ValueError(f"Plan {plan_id} not found")
    for key, value in kwargs.items():
        if value is not None and hasattr(plan, key):
            setattr(plan, key, value)
    db.commit()
    db.refresh(plan)
    return plan


def delete_plan(db: Session, plan_id: int) -> bool:
    plan = db.query(HostingPlan).filter(HostingPlan.id == plan_id).first()
    if plan is None:
        return False
    db.delete(plan)
    db.commit()
    return True


# ---------------------------------------------------------------------------
# Account lookup
# ---------------------------------------------------------------------------

def get_account(db: Session, external_id: str) -> Optional[HostingAccount]:
    return db.query(HostingAccount).filter(HostingAccount.external_id == external_id).first()


def _account_dict(db: Session, account: HostingAccount) -> dict:
    """Build the ProvisioningAccountOut dict from an HostingAccount row."""
    user = db.query(User).filter(User.id == account.user_id).first()
    plan = db.query(HostingPlan).filter(HostingPlan.id == account.plan_id).first() if account.plan_id else None
    website = db.query(Website).filter(Website.id == account.primary_website_id).first() if account.primary_website_id else None
    domain = website.domain if website else None

    # storage usage
    used_bytes = 0
    limit_bytes = None
    website_count = 0
    website_limit = 0
    if user:
        summary = storage_quota.storage_usage_summary(db, user)
        used_bytes = summary.get("storage_used_bytes", 0)
        limit_bytes = summary.get("storage_limit_bytes")
        website_count = db.query(Website).filter(Website.owner_id == user.id).count()
        website_limit = user.website_limit

    service_label_parts = []
    if domain:
        service_label_parts.append(domain)
    if plan:
        service_label_parts.append(plan.name)
    service_label = " â€” ".join(service_label_parts) if service_label_parts else None

    return {
        "external_id": account.external_id,
        "user_id": account.user_id,
        "username": user.username if user else "",
        "domain": domain,
        "status": account.status,
        "plan_id": account.plan_id,
        "package_name": plan.name if plan else None,
        "service_label": service_label,
        "storage_used_bytes": used_bytes,
        "storage_limit_bytes": limit_bytes,
        "website_count": website_count,
        "website_limit": website_limit,
        "created_at": account.created_at.isoformat() if account.created_at else None,
    }


def _usage_dict(db: Session, account: HostingAccount) -> dict:
    user = db.query(User).filter(User.id == account.user_id).first()
    summary = storage_quota.storage_usage_summary(db, user) if user else {}
    return {
        "external_id": account.external_id,
        "storage_used_bytes": summary.get("storage_used_bytes", 0),
        "storage_limit_bytes": summary.get("storage_limit_bytes"),
        "storage_percent": summary.get("storage_percent", 0.0),
        "website_count": db.query(Website).filter(Website.owner_id == account.user_id).count() if user else 0,
        "website_limit": user.website_limit if user else 0,
    }


# ---------------------------------------------------------------------------
# Job tracking
# ---------------------------------------------------------------------------

def _start_job(db: Session, external_id: str, action: str) -> ProvisioningJob:
    job = ProvisioningJob(
        external_id=external_id,
        action=action,
        status="running",
        started_at=datetime.utcnow(),
    )
    db.add(job)
    db.commit()
    db.refresh(job)
    return job


def _finish_job(db: Session, job: ProvisioningJob, status: str, error: Optional[str] = None) -> None:
    job.status = status
    job.error = error
    job.finished_at = datetime.utcnow()
    db.commit()


# ---------------------------------------------------------------------------
# Create account (idempotent)
# ---------------------------------------------------------------------------

def create_account(
    db: Session,
    *,
    external_id: str,
    username: str,
    password: str,
    domain: Optional[str],
    package_id: int,
    php_version: str,
    app_type: str,
    install_wordpress: bool,
    enable_ssl: bool,
) -> tuple[HostingAccount, bool]:
    """Create hosting account. Returns (account, is_new).

    Idempotent: if external_id already exists, returns existing account.
    """
    existing = get_account(db, external_id)
    if existing is not None and existing.status != "terminated":
        # Idempotent: a still-active account already exists, return it without
        # re-provisioning (otherwise we would recreate the website/domain).
        return existing, False

    if existing is not None and existing.status == "terminated":
        # WHMCS sent CreateAccount for a service that was previously terminated.
        # Re-provision from scratch: drop the stale account row (and its user,
        # since username/email are unique) and any leftover websites so the
        # normal create path below can rebuild everything cleanly.
        stale_user = (
            db.query(User).filter(User.id == existing.user_id).first()
            if existing.user_id
            else None
        )
        for website in (
            db.query(Website).filter(Website.owner_id == stale_user.id).all()
            if stale_user
            else []
        ):
            db.delete(website)
        if stale_user is not None:
            db.delete(stale_user)
        db.delete(existing)
        db.commit()

    job = _start_job(db, external_id, "create")

    plan = get_plan(db, package_id)
    if plan is None:
        _finish_job(db, job, "failed", f"Plan {package_id} not found or inactive")
        raise ValueError(f"Hosting plan {package_id} not found or inactive")

    # 1. Create panel user
    try:
        linux_user = site_users.ensure_panel_user(username, password)
    except (ValueError, RuntimeError) as exc:
        _finish_job(db, job, "failed", f"Failed to create panel user: {exc}")
        raise

    # 2. Create OPanel Enterprise user record
    user = User(
        username=username,
        email=f"{username}@opanel-ent.local",
        hashed_password=hash_password(password),
        role="end_user",
        is_active=True,
        website_limit=plan.website_limit,
        storage_limit_mb=plan.storage_limit_mb,
    )
    db.add(user)
    db.commit()
    db.refresh(user)

    # 3. Create website if domain provided
    website = None
    if domain:
        root_path = site_users.site_root_for_panel_user(username, domain)
        # root_path lives under the panel user's home (/home/<paneluser>/<domain>),
        # exactly like the canonical website-create path in websites.py. Pass the
        # panel linux_user so ensure_site_runtime asserts ownership against the
        # real owner of that path (the same account that owns the home).
        try:
            site_linux_user = site_users.ensure_site_runtime(domain, root_path, php_version if app_type in {"php", "wordpress"} else None, linux_user)
        except (ValueError, RuntimeError) as exc:
            user.is_active = False
            db.commit()
            _finish_job(db, job, "failed", f"Failed to create site runtime: {exc}")
            raise

        website = Website(
            domain=domain,
            owner_id=user.id,
            root_path=root_path,
            document_root="public_html",
            linux_user=site_linux_user,
            php_version=php_version,
            app_type=app_type if not install_wordpress else "wordpress",
            nginx_rewrite_mode="front_controller" if (app_type == "wordpress" or install_wordpress) else "none",
            status="provisioning",
        )
        db.add(website)
        db.commit()
        db.refresh(website)

        # 3a. Install WordPress if requested
        if install_wordpress:
            try:
                db_info = mariadb.create_database(domain)
            except RuntimeError as exc:
                _cleanup_failed(db, user, website)
                _finish_job(db, job, "failed", f"MariaDB creation failed: {exc}")
                raise ValueError(f"Could not create database: {exc}") from exc

            try:
                wordpress.install_wordpress(
                    domain,
                    db_info,
                    f"{domain} WordPress",
                    "admin",
                    password,
                    f"admin@{domain}",
                    php_version,
                    site_linux_user,
                    root_path=root_path,
                )
            except (RuntimeError, ValueError) as exc:
                mariadb.drop_database(db_info["db_name"], db_info["db_user"])
                _cleanup_failed(db, user, website)
                _finish_job(db, job, "failed", f"WordPress install failed: {exc}")
                raise
        else:
            # Write placeholder page
            try:
                placeholder = site_users.document_root(root_path)
                if not settings.command_dry_run:
                    placeholder.mkdir(parents=True, exist_ok=True)
                    site_users.fix_site_path(str(placeholder), site_linux_user)
            except (OSError, RuntimeError):
                pass

        # 4. Write vhost
        try:
            openlitespeed.rewrite_vhost(
                domain,
                root_path,
                app_type=website.app_type,
                php_version=php_version,
                linux_user=site_linux_user,
                lsphp_socket_override=site_users.site_lsphp_socket(site_linux_user, root_path, php_version if website.app_type in {"php", "wordpress"} else None),
                document_root="public_html",
                rewrite_mode=website.nginx_rewrite_mode,
            )
        except (RuntimeError, ValueError) as exc:
            _cleanup_failed(db, user, website)
            _finish_job(db, job, "failed", f"Vhost creation failed: {exc}")
            raise ValueError(f"Vhost creation failed: {exc}") from exc

        # 5. SSL
        if enable_ssl and domain:
            try:
                ssl.issue_ssl(domain)
            except Exception:
                pass  # non-fatal

        website.status = "active"
        db.commit()

    # 6. Create hosting account record
    account = HostingAccount(
        external_id=external_id,
        user_id=user.id,
        primary_website_id=website.id if website else None,
        plan_id=plan.id,
        status="active",
    )
    db.add(account)
    _finish_job(db, job, "completed")
    db.commit()
    db.refresh(account)
    return account, True


def _cleanup_failed(db: Session, user: User, website: Optional[Website]) -> None:
    """Best-effort rollback of a failed provisioning."""
    try:
        if website:
            openlitespeed.remove_vhost(website.domain)
            wordpress.delete_wordpress(website.root_path)
            db.delete(website)
        user.is_active = False
        db.commit()
    except Exception:
        db.rollback()


# ---------------------------------------------------------------------------
# Suspend / Unsuspend
# ---------------------------------------------------------------------------

def suspend_account(db: Session, external_id: str, reason: str = "Suspended by WHMCS") -> HostingAccount:
    account = get_account(db, external_id)
    if account is None:
        raise ValueError(f"Account {external_id} not found")
    if account.status == "suspended":
        return account

    job = _start_job(db, external_id, "suspend")
    user = db.query(User).filter(User.id == account.user_id).first()
    if not user:
        _finish_job(db, job, "failed", "User not found")
        raise ValueError("User not found")

    # Disable panel user. Bumping token_version is what actually ends the
    # sessions already open -- without it a suspended account keeps full use of
    # the panel until its JWT expires.
    user.is_active = False
    user.token_version = (user.token_version or 0) + 1
    db.commit()

    # Lock Linux user
    linux_user = site_users.linux_user_for_panel_username(user.username)
    try:
        shell.privileged("panel-user-lock", helper_args=[linux_user], check=False)
    except Exception:
        pass

    # Disable vhost
    website = db.query(Website).filter(Website.id == account.primary_website_id).first() if account.primary_website_id else None
    if website:
        try:
            openlitespeed.suspend_vhost(website.domain)
        except Exception:
            pass

    account.status = "suspended"
    account.updated_at = datetime.utcnow()
    _finish_job(db, job, "completed")
    db.commit()
    db.refresh(account)
    return account


def unsuspend_account(db: Session, external_id: str) -> HostingAccount:
    account = get_account(db, external_id)
    if account is None:
        raise ValueError(f"Account {external_id} not found")
    if account.status == "active":
        return account

    job = _start_job(db, external_id, "unsuspend")
    user = db.query(User).filter(User.id == account.user_id).first()
    if not user:
        _finish_job(db, job, "failed", "User not found")
        raise ValueError("User not found")

    # Unlock Linux user
    linux_user = site_users.linux_user_for_panel_username(user.username)
    try:
        shell.privileged("panel-user-unlock", helper_args=[linux_user], check=False)
    except Exception:
        pass

    # Enable panel user
    user.is_active = True
    db.commit()

    # Restore vhost
    website = db.query(Website).filter(Website.id == account.primary_website_id).first() if account.primary_website_id else None
    if website:
        try:
            openlitespeed.restore_vhost(website.domain)
        except Exception:
            pass

    account.status = "active"
    account.updated_at = datetime.utcnow()
    _finish_job(db, job, "completed")
    db.commit()
    db.refresh(account)
    return account


# ---------------------------------------------------------------------------
# Terminate
# ---------------------------------------------------------------------------

def terminate_account(db: Session, external_id: str) -> bool:
    account = get_account(db, external_id)
    if account is None:
        raise ValueError(f"Account {external_id} not found")

    job = _start_job(db, external_id, "terminate")
    user = db.query(User).filter(User.id == account.user_id).first()
    websites = db.query(Website).filter(Website.owner_id == account.user_id).all() if user else []

    for website in websites:
        # Full teardown: drop the site database, remove files, and the vhost.
        db_acc = db.query(DatabaseAccount).filter(DatabaseAccount.website_id == website.id).first()
        if db_acc:
            mariadb.drop_database(db_acc.db_name, db_acc.db_user)
            db.delete(db_acc)
        openlitespeed.remove_vhost(website.domain)
        wordpress.delete_wordpress(website.root_path)
        db.delete(website)

    if user:
        # Remove the Linux/SFTP user and its home directory entirely.
        site_users.delete_panel_user(user.username)
        db.delete(user)
        db.commit()

    account.status = "terminated"
    account.updated_at = datetime.utcnow()
    _finish_job(db, job, "completed")
    db.commit()
    return True


# ---------------------------------------------------------------------------
# Change password
# ---------------------------------------------------------------------------

def change_password(db: Session, external_id: str, password: str) -> bool:
    account = get_account(db, external_id)
    if account is None:
        raise ValueError(f"Account {external_id} not found")

    job = _start_job(db, external_id, "change_password")
    user = db.query(User).filter(User.id == account.user_id).first()
    if not user:
        _finish_job(db, job, "failed", "User not found")
        raise ValueError("User not found")

    # Update panel password, and cut the sessions signed with the old one.
    user.hashed_password = hash_password(password)
    user.token_version = (user.token_version or 0) + 1
    db.commit()

    # Update Linux/SFTP password
    linux_user = site_users.linux_user_for_panel_username(user.username)
    site_users.set_panel_user_password(linux_user, password)

    _finish_job(db, job, "completed")
    db.commit()
    return True


# ---------------------------------------------------------------------------
# Change package
# ---------------------------------------------------------------------------

def change_package(db: Session, external_id: str, package_id: int) -> HostingAccount:
    account = get_account(db, external_id)
    if account is None:
        raise ValueError(f"Account {external_id} not found")

    plan = get_plan(db, package_id)
    if plan is None:
        raise ValueError(f"Plan {package_id} not found or inactive")

    job = _start_job(db, external_id, "change_package")
    user = db.query(User).filter(User.id == account.user_id).first()
    if not user:
        _finish_job(db, job, "failed", "User not found")
        raise ValueError("User not found")

    # Update user limits from plan
    user.website_limit = plan.website_limit
    user.storage_limit_mb = plan.storage_limit_mb
    account.plan_id = plan.id
    account.updated_at = datetime.utcnow()

    _finish_job(db, job, "completed")
    db.commit()
    db.refresh(account)
    return account


# ---------------------------------------------------------------------------
# SSO login
# ---------------------------------------------------------------------------

def create_sso_login(db: Session, external_id: str, panel_url: str) -> dict:
    account = get_account(db, external_id)
    if account is None:
        raise ValueError(f"Account {external_id} not found")

    user = db.query(User).filter(User.id == account.user_id).first()
    if not user:
        raise ValueError("User not found")

    # Generate one-time token. The WHMCS bpanel module bakes this URL into a
    # rendered page (client area / admin service page) and waits for the
    # user to click it later — it is not fetched at click time — so the
    # window has to survive normal page-load-to-click time, not just
    # network latency. The token is still single-use and 256 bits of
    # entropy, so 5 minutes stays effectively as safe as the previous 60s
    # while giving real users enough time to actually click the link.
    raw_token = secrets.token_urlsafe(32)
    token_hash = hashlib.sha256(raw_token.encode("utf-8")).hexdigest()
    expires_at = datetime.utcnow() + timedelta(seconds=300)

    sso = PanelSsoToken(
        user_id=user.id,
        token_hash=token_hash,
        expires_at=expires_at,
    )
    db.add(sso)
    db.commit()

    base_url = panel_url.rstrip("/")
    return {
        "login_url": f"{base_url}/sso#{raw_token}",
        "expires_at": expires_at.isoformat(),
    }


def consume_sso_token(db: Session, raw_token: str) -> Optional[int]:
    """Validate and consume an SSO token. Returns user_id or None."""
    token_hash = hashlib.sha256(raw_token.encode("utf-8")).hexdigest()
    sso = db.query(PanelSsoToken).filter(
        PanelSsoToken.token_hash == token_hash,
        PanelSsoToken.used_at.is_(None),
    ).first()
    if sso is None:
        return None
    if sso.expires_at < datetime.utcnow():
        return None
    sso.used_at = datetime.utcnow()
    db.commit()
    return sso.user_id


# ---------------------------------------------------------------------------
# API token helpers
# ---------------------------------------------------------------------------

def create_api_token(db: Session, name: str, scopes: list[str], expires_days: int = 365, ip_allowlist: str = "") -> tuple[str, ApiToken]:
    """Create an API token. Returns (raw_token, token_row)."""
    raw_token = secrets.token_urlsafe(48)
    token_hash = hashlib.sha256(raw_token.encode("utf-8")).hexdigest()
    token = ApiToken(
        name=name,
        token_hash=token_hash,
        scopes=json.dumps(scopes),
        expires_at=datetime.utcnow() + timedelta(days=expires_days),
        ip_allowlist=ip_allowlist or None,
    )
    db.add(token)
    db.commit()
    db.refresh(token)
    return raw_token, token


def list_api_tokens(db: Session) -> list[ApiToken]:
    return db.query(ApiToken).filter(ApiToken.revoked_at.is_(None)).order_by(ApiToken.id).all()


def delete_api_token(db: Session, token_id: int) -> bool:
    token = db.query(ApiToken).filter(ApiToken.id == token_id).first()
    if token is None:
        return False
    db.delete(token)
    db.commit()
    return True
