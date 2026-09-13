import json
from datetime import datetime
from pathlib import Path

from fastapi import APIRouter, Depends, File, Form, HTTPException, Query, Request, UploadFile
from jinja2 import Environment, FileSystemLoader
from sqlalchemy.exc import IntegrityError
from sqlalchemy.orm import Session
from typing import List

from app.api.deps import get_current_user
from app.core.config import settings
from app.core.database import get_db
from app.core.permissions import Role, ensure_role, is_admin_role
from app.core.secrets import decrypt, encrypt
from app.models.entities import DatabaseAccount, User, Website, WebsiteAlias
from app.schemas.schemas import AvailableCertificateOut, ReuseSslRequest, WebsiteAliasCreate, WebsiteAliasOut, WebsiteCreate, WebsiteLogOut, WebsiteNginxConfig, WebsiteNginxCustom, WebsiteOut, WebsiteUpdate, WebsiteWafUpdate, WildcardSslRequest
from app.services import file_manager, mariadb, openlitespeed, site_users, ssl, storage_quota, waf, wordpress
from app.services.audit import log_action

_PLACEHOLDER_TEMPLATE_DIR = Path(__file__).resolve().parent.parent / "templates" / "openlitespeed"

router = APIRouter(prefix="/websites", tags=["websites"])


def _command_error(result):
    return (result.stderr or result.stdout or f"Command failed with code {result.returncode}").strip()


def _cleanup_failed_site(root_path: str, linux_user: str | None, delete_files: bool = True) -> None:
    if delete_files:
        try:
            if linux_user:
                site_users.delete_site_runtime(root_path, linux_user)
            else:
                wordpress.delete_wordpress(root_path)
        except Exception:
            pass


def _delete_website_reservation(db: Session, website_id: int) -> None:
    try:
        db.rollback()
        website = db.get(Website, website_id)
        if website is not None:
            db.delete(website)
            db.commit()
    except Exception:
        db.rollback()


def _ensure_default_waf_file(domain: str) -> None:
    result = waf.sync_site_rules(domain, [rule["id"] for rule in waf.DEFAULT_RULES], "")
    if result.returncode != 0:
        raise RuntimeError(_command_error(result))


def _write_placeholder_page(domain: str, root_path: str, linux_user: str | None, php_version: str) -> None:
    placeholder = site_users.document_root(root_path) / "index.html"
    if placeholder.exists():
        return
    env = Environment(loader=FileSystemLoader(_PLACEHOLDER_TEMPLATE_DIR), autoescape=False)
    tmpl = env.get_template("placeholder.html.j2")
    placeholder_site = Website(
        domain=domain,
        owner_id=0,
        root_path=root_path,
        document_root="public_html",
        linux_user=linux_user,
        php_version=php_version,
        app_type="php",
    )
    file_manager.write_text_file(
        placeholder_site,
        "public_html/index.html",
        tmpl.render(domain=domain),
        allow_executable=True,
    )


def _rewrite_ssl_kwargs(website: Website) -> dict:
    if getattr(website, "ssl_mode", "none") == "letsencrypt" and getattr(website, "ssl_enabled", False):
        return {"ssl_enabled": True}
    if getattr(website, "ssl_mode", "none") == "reuse" and getattr(website, "ssl_reuse_name", None):
        try:
            paths = ssl.reuse_cert_paths(website.ssl_reuse_name)
        except ValueError:
            return {}
        return {
            "ssl_enabled": True,
            "ssl_cert_path": paths["cert"],
            "ssl_key_path": paths["key"],
            "ssl_ca_path": paths["ca"],
        }
    if (
        getattr(website, "ssl_mode", "none") == "manual"
        and website.ssl_cert_path
        and website.ssl_key_path
    ):
        return {
            "ssl_cert_path": website.ssl_cert_path,
            "ssl_key_path": website.ssl_key_path,
            "ssl_ca_path": website.ssl_ca_path,
        }
    return {}


def _website_rewrite_mode(website: Website) -> str:
    return getattr(website, "nginx_rewrite_mode", "none") or "none"  # DB column kept as nginx_*


def _get_authorized_website(db: Session, website_id: int, current_user: User) -> Website:
    website = db.query(Website).filter(Website.id == website_id).first()
    if not website:
        raise HTTPException(status_code=404, detail="Website not found")
    if website.owner_id != current_user.id:
        ensure_role(current_user.role, Role.admin)
    return website


def _alias_domains(website: Website) -> list[str]:
    return [
        alias.domain
        for alias in sorted(getattr(website, "aliases", []) or [], key=lambda item: item.domain)
        if getattr(alias, "mode", "alias") == "alias"
    ]


def _redirect_domains(website: Website) -> list[str]:
    return [
        alias.domain
        for alias in sorted(getattr(website, "aliases", []) or [], key=lambda item: item.domain)
        if getattr(alias, "mode", "alias") == "redirect"
    ]


def _ssl_domains(website: Website) -> list[str]:
    return [*_alias_domains(website), *_redirect_domains(website)]


def _unique_domains(domains: list[str] | tuple[str, ...]) -> list[str]:
    unique: list[str] = []
    seen: set[str] = set()
    for domain in domains:
        value = (domain or "").strip().lower()
        if value and value not in seen:
            unique.append(value)
            seen.add(value)
    return unique


def _reserved_hostnames(db: Session, exclude_website_id: int | None = None, exclude_alias_id: int | None = None) -> set[str]:
    names: set[str] = set()
    for website in db.query(Website).all():
        if exclude_website_id is not None and website.id == exclude_website_id:
            continue
        domain = (website.domain or "").strip().lower()
        if not domain:
            continue
        names.add(domain)
        names.add(f"www.{domain}")
    for alias in db.query(WebsiteAlias).all():
        if exclude_alias_id is not None and alias.id == exclude_alias_id:
            continue
        domain = (alias.domain or "").strip().lower()
        if domain:
            names.add(domain)
    return names


def _hostname_conflicts(db: Session, domain: str, *, exclude_website_id: int | None = None, exclude_alias_id: int | None = None) -> bool:
    safe_domain = (domain or "").strip().lower()
    if not safe_domain:
        return True
    return bool({safe_domain, f"www.{safe_domain}"} & _reserved_hostnames(db, exclude_website_id=exclude_website_id, exclude_alias_id=exclude_alias_id))


def _rewrite_website_vhost(website: Website, **overrides) -> str:
    app_type = overrides.pop("app_type", website.app_type or "wordpress")
    php_version = overrides.pop("php_version", website.php_version)
    root_path = overrides.pop("root_path", website.root_path)
    linux_user = overrides.pop("linux_user", website.linux_user)
    runtime_php_version = php_version if app_type in {"wordpress", "php"} else None
    if "lsphp_socket_override" in overrides:
        lsphp_socket_override = overrides.pop("lsphp_socket_override")
    elif runtime_php_version:
        lsphp_socket_override = site_users.site_lsphp_socket(linux_user, root_path, runtime_php_version)
    else:
        lsphp_socket_override = None
    rewrite_kwargs = {
        "app_type": app_type,
        "php_version": php_version,
        "custom_directives": "",
        "linux_user": linux_user,
        "lsphp_socket_override": lsphp_socket_override,
        "waf_enabled": overrides.pop("waf_enabled", website.waf_enabled),
        "document_root": overrides.pop("document_root", website.document_root or "public_html"),
        "rewrite_mode": overrides.pop("rewrite_mode", _website_rewrite_mode(website)),
        "aliases": overrides.pop("aliases", _alias_domains(website)),
        "redirects": overrides.pop("redirects", _redirect_domains(website)),
    }
    if overrides.pop("include_ssl", True):
        rewrite_kwargs.update(_rewrite_ssl_kwargs(website))
    rewrite_kwargs.update(overrides)
    return openlitespeed.rewrite_vhost(
        website.domain,
        root_path,
        **rewrite_kwargs,
    )


def _has_live_certificate(website: Website) -> bool:
    domain = website.domain
    if getattr(website, "ssl_mode", "none") == "reuse" and getattr(website, "ssl_reuse_name", None):
        try:
            paths = ssl.reuse_cert_paths(website.ssl_reuse_name)
            return Path(paths["cert"]).is_file() and Path(paths["key"]).is_file()
        except (ValueError, OSError):
            return False
    if (
        getattr(website, "ssl_mode", "none") == "manual"
        and website.ssl_cert_path
        and website.ssl_key_path
    ):
        try:
            return Path(website.ssl_cert_path).is_file() and Path(website.ssl_key_path).is_file()
        except OSError:
            return False
    live_dir = Path("/etc/letsencrypt/live") / domain
    try:
        return (live_dir / "fullchain.pem").is_file() and (live_dir / "privkey.pem").is_file()
    except OSError:
        return False


def _sync_live_ssl_flags(db: Session, websites: list[Website]) -> list[Website]:
    changed = False
    for website in websites:
        if not website.ssl_enabled and _has_live_certificate(website):
            website.ssl_enabled = True
            changed = True
    if changed:
        db.commit()
        for website in websites:
            db.refresh(website)
    return websites


def _read_ssl_input(upload: UploadFile | None, text: str | None, label: str, required: bool = True) -> bytes:
    if upload is not None and upload.filename:
        return ssl.read_ssl_part(upload, label=label, required=required)
    return ssl.read_ssl_part(text or "", label=label, required=required)


@router.post("", response_model=WebsiteOut)
def create_website(payload: WebsiteCreate, request: Request, db: Session = Depends(get_db), current_user: User = Depends(get_current_user)):
    """Create a website. If install_wordpress is False, only creates the domain
    folder + OLS vhost (no DB, no WordPress files)."""
    requested_owner_id = payload.owner_id
    if requested_owner_id is not None and requested_owner_id != current_user.id:
        ensure_role(current_user.role, Role.admin)
    if _hostname_conflicts(db, payload.domain):
        raise HTTPException(status_code=409, detail="Domain already exists")

    if requested_owner_id is not None:
        owner = db.query(User).filter(User.id == requested_owner_id).first()
        if not owner:
            raise HTTPException(status_code=404, detail="Owner not found")
    else:
        owner = current_user

    owner_id = owner.id
    current_count = db.query(Website).filter(Website.owner_id == owner_id).count()
    if not is_admin_role(owner.role) and current_count >= owner.website_limit:
        raise HTTPException(status_code=403, detail="Website limit reached")

    install_wp = payload.install_wordpress and payload.app_type == "wordpress"
    app_type_value = "wordpress" if install_wp else ("php" if payload.app_type == "wordpress" else payload.app_type)
    create_estimate_bytes = storage_quota.WORDPRESS_SITE_ESTIMATE_BYTES if install_wp else storage_quota.STATIC_SITE_ESTIMATE_BYTES
    try:
        storage_quota.enforce_user_storage_quota(db, owner, incoming_bytes=create_estimate_bytes)
    except storage_quota.StorageQuotaExceeded as exc:
        raise HTTPException(status_code=413, detail=str(exc)) from exc

    linux_user = site_users.linux_user_for_panel_username(owner.username)
    root_path = site_users.site_root_for_panel_user(owner.username, payload.domain)
    if install_wp and (not payload.admin_email or not payload.admin_password):
        raise HTTPException(status_code=400, detail="admin_email and admin_password are required when install_wordpress is true")

    website = Website(
        domain=payload.domain,
        owner_id=owner_id,
        root_path=root_path,
        document_root="public_html",
        linux_user=linux_user,
        php_version=payload.php_version,
        app_type=app_type_value,
        nginx_rewrite_mode="front_controller" if app_type_value == "wordpress" else "none",
        status="provisioning",
    )
    db.add(website)
    try:
        db.commit()
    except IntegrityError as exc:
        db.rollback()
        raise HTTPException(status_code=409, detail="Domain already exists") from exc
    db.refresh(website)

    if install_wp:
        try:
            db_info = mariadb.create_database(payload.domain)
        except RuntimeError as exc:
            _delete_website_reservation(db, website.id)
            raise HTTPException(status_code=400, detail=f"Could not create MariaDB database: {exc}") from exc
        try:
            linux_user = site_users.ensure_site_runtime(payload.domain, root_path, payload.php_version, linux_user)
            root_path = wordpress.install_wordpress(
                payload.domain,
                db_info,
                payload.title,
                payload.admin_user,
                payload.admin_password,
                str(payload.admin_email),
                payload.php_version,
                linux_user,
                root_path=root_path,
            )
        except (RuntimeError, ValueError) as exc:
            mariadb.drop_database(db_info["db_name"], db_info["db_user"])
            _cleanup_failed_site(root_path, linux_user)
            _delete_website_reservation(db, website.id)
            raise HTTPException(status_code=400, detail=str(exc)) from exc
        try:
            _ensure_default_waf_file(payload.domain)
            openlitespeed.rewrite_vhost(
                payload.domain,
                root_path,
                app_type="wordpress",
                php_version=payload.php_version,
                linux_user=linux_user,
                lsphp_socket_override=site_users.site_lsphp_socket(linux_user, root_path, payload.php_version),
                document_root="public_html",
                rewrite_mode="front_controller",
            )
        except (RuntimeError, ValueError) as exc:
            mariadb.drop_database(db_info["db_name"], db_info["db_user"])
            _cleanup_failed_site(root_path, linux_user)
            _delete_website_reservation(db, website.id)
            raise HTTPException(status_code=400, detail=str(exc)) from exc
    else:
        runtime_php_version = payload.php_version if app_type_value in {"wordpress", "php"} else None
        try:
            linux_user = site_users.ensure_site_runtime(payload.domain, root_path, runtime_php_version, linux_user)
            # Just create the public_html/ folder skeleton and write a vhost.
            public = site_users.document_root(root_path)
            if not settings.command_dry_run:
                _write_placeholder_page(payload.domain, root_path, linux_user, payload.php_version)
                site_users.fix_site_path(str(public), linux_user)
            _ensure_default_waf_file(payload.domain)
            openlitespeed.rewrite_vhost(
                payload.domain,
                root_path,
                app_type=app_type_value,
                php_version=payload.php_version,
                linux_user=linux_user,
                lsphp_socket_override=site_users.site_lsphp_socket(linux_user, root_path, runtime_php_version),
                document_root="public_html",
                rewrite_mode="none",
            )
        except (RuntimeError, ValueError, OSError) as exc:
            _cleanup_failed_site(root_path, linux_user)
            _delete_website_reservation(db, website.id)
            raise HTTPException(status_code=400, detail=str(exc)) from exc
        db_info = None

    website.root_path = root_path
    website.linux_user = linux_user
    website.status = "active"
    db.commit()
    db.refresh(website)
    if db_info:
        # Store password encrypted; phpMyAdmin SSO decrypts on demand.
        db.add(DatabaseAccount(
            owner_id=owner_id,
            website_id=website.id,
            db_name=db_info["db_name"],
            db_user=db_info["db_user"],
            db_password=encrypt(db_info["db_password"]),
        ))
        db.commit()
    log_action(
        db,
        current_user.id,
        "create_wordpress" if install_wp else "create_site",
        payload.domain,
        request=request,
    )
    return website


@router.post("/wordpress", response_model=WebsiteOut)
def create_wordpress(payload: WebsiteCreate, request: Request, db: Session = Depends(get_db), current_user: User = Depends(get_current_user)):
    """Legacy endpoint for backwards compatibility. Forces install_wordpress=True."""
    payload = payload.model_copy(update={"install_wordpress": True, "app_type": "wordpress"})
    return create_website(payload, request, db, current_user)


@router.get("", response_model=List[WebsiteOut])
def list_websites(db: Session = Depends(get_db), current_user: User = Depends(get_current_user)):
    if is_admin_role(current_user.role):
        websites = db.query(Website).order_by(Website.id.desc()).all()
    else:
        websites = db.query(Website).filter(Website.owner_id == current_user.id).order_by(Website.id.desc()).all()
    return _sync_live_ssl_flags(db, websites)


@router.get("/{website_id}/aliases", response_model=List[WebsiteAliasOut])
def list_website_aliases(website_id: int, db: Session = Depends(get_db), current_user: User = Depends(get_current_user)):
    website = _get_authorized_website(db, website_id, current_user)
    return sorted(website.aliases or [], key=lambda alias: alias.domain)


@router.post("/{website_id}/aliases", response_model=WebsiteAliasOut)
def create_website_alias(
    website_id: int,
    payload: WebsiteAliasCreate,
    request: Request,
    db: Session = Depends(get_db),
    current_user: User = Depends(get_current_user),
):
    website = _get_authorized_website(db, website_id, current_user)
    if payload.domain == website.domain or _hostname_conflicts(db, payload.domain):
        raise HTTPException(status_code=409, detail="Domain alias already exists")
    alias = WebsiteAlias(website_id=website.id, domain=payload.domain, mode=payload.mode)
    db.add(alias)
    db.flush()
    try:
        aliases = _alias_domains(website)
        redirects = _redirect_domains(website)
        if payload.mode == "alias" and payload.domain not in aliases:
            aliases.append(payload.domain)
        if payload.mode == "redirect" and payload.domain not in redirects:
            redirects.append(payload.domain)
        aliases = _unique_domains(aliases)
        redirects = _unique_domains(redirects)
        _rewrite_website_vhost(website, aliases=aliases, redirects=redirects)
        if getattr(website, "ssl_mode", "none") == "letsencrypt":
            result = ssl.issue_ssl(website.domain, [*aliases, *redirects])
            if result.returncode != 0:
                raise RuntimeError(_command_error(result))
    except (RuntimeError, ValueError, FileNotFoundError) as exc:
        db.rollback()
        raise HTTPException(status_code=400, detail=f"Cannot write webserver config: {exc}") from exc
    db.commit()
    db.refresh(alias)
    log_action(db, current_user.id, "create_website_alias", website.domain, payload.domain, request=request)
    return alias


@router.delete("/{website_id}/aliases/{alias_id}")
def delete_website_alias(
    website_id: int,
    alias_id: int,
    request: Request,
    db: Session = Depends(get_db),
    current_user: User = Depends(get_current_user),
):
    website = _get_authorized_website(db, website_id, current_user)
    alias = db.query(WebsiteAlias).filter(
        WebsiteAlias.id == alias_id,
        WebsiteAlias.website_id == website.id,
    ).first()
    if not alias:
        raise HTTPException(status_code=404, detail="Alias not found")
    domain = alias.domain
    db.delete(alias)
    db.flush()
    try:
        aliases = [item for item in _alias_domains(website) if item != domain]
        redirects = [item for item in _redirect_domains(website) if item != domain]
        _rewrite_website_vhost(website, aliases=aliases, redirects=redirects)
    except (RuntimeError, ValueError, FileNotFoundError) as exc:
        db.rollback()
        raise HTTPException(status_code=400, detail=f"Cannot write webserver config: {exc}") from exc
    db.commit()
    log_action(db, current_user.id, "delete_website_alias", website.domain, domain, request=request)
    return {"ok": True}


@router.patch("/{website_id}", response_model=WebsiteOut)
def update_website(website_id: int, payload: WebsiteUpdate, db: Session = Depends(get_db), current_user: User = Depends(get_current_user)):
    website = db.query(Website).filter(Website.id == website_id).first()
    if not website:
        raise HTTPException(status_code=404, detail="Website not found")
    if website.owner_id != current_user.id:
        ensure_role(current_user.role, Role.admin)
    if payload.php_version is not None:
        try:
            runtime_php_version = payload.php_version if (website.app_type or "wordpress") in {"wordpress", "php"} else None
            if website.linux_user and runtime_php_version:
                site_users.ensure_site_runtime(website.domain, website.root_path, payload.php_version, website.linux_user)
            result = waf.sync_website_rules(website)
            if result.returncode != 0:
                raise RuntimeError(_command_error(result))
            app_type = website.app_type or "wordpress"
            _rewrite_website_vhost(
                website,
                app_type=app_type,
                php_version=payload.php_version,
            )
        except (RuntimeError, ValueError) as exc:
                raise HTTPException(status_code=400, detail=f"Cannot write vhost config: {exc}") from exc
        website.php_version = payload.php_version
    if payload.app_type is not None and payload.app_type != (website.app_type or "wordpress"):
        try:
            next_app_type = payload.app_type
            runtime_php_version = website.php_version if next_app_type in {"wordpress", "php"} else None
            if website.linux_user and runtime_php_version:
                site_users.ensure_site_runtime(
                    website.domain,
                    website.root_path,
                    website.php_version,
                    website.linux_user,
                )
            result = waf.sync_website_rules(website)
            if result.returncode != 0:
                raise RuntimeError(_command_error(result))
            next_rewrite_mode = (
                "front_controller" if next_app_type == "wordpress"
                else "none" if next_app_type == "static"
                else payload.nginx_rewrite_mode or _website_rewrite_mode(website)
            )
            _rewrite_website_vhost(
                website,
                app_type=next_app_type,
                php_version=website.php_version,
                rewrite_mode=next_rewrite_mode,
            )
        except (RuntimeError, ValueError, OSError) as exc:
            raise HTTPException(status_code=400, detail=f"Cannot change website mode: {exc}") from exc
        website.app_type = next_app_type
        website.nginx_rewrite_mode = next_rewrite_mode
    if payload.status is not None:
        # Suspension is an admin decision; an owner could otherwise flip their
        # own site back to active.
        ensure_role(current_user.role, Role.admin)
        website.status = payload.status
    if payload.document_root is not None and payload.document_root != (website.document_root or "public_html"):
        try:
            next_document_root = site_users.validate_document_root(payload.document_root)
            site_users.ensure_document_root(website.root_path, next_document_root, website.linux_user)
            app_type = website.app_type or "wordpress"
            runtime_php_version = website.php_version if app_type in {"wordpress", "php"} else None
            result = waf.sync_website_rules(website)
            if result.returncode != 0:
                raise RuntimeError(_command_error(result))
            _rewrite_website_vhost(
                website,
                app_type=app_type,
                php_version=website.php_version,
                document_root=next_document_root,
            )
        except (RuntimeError, ValueError, OSError) as exc:
            raise HTTPException(status_code=400, detail=f"Cannot change document root: {exc}") from exc
        website.document_root = next_document_root
    if payload.owner_id is not None:
        ensure_role(current_user.role, Role.admin)
        owner = db.query(User).filter(User.id == payload.owner_id).first()
        if not owner:
            raise HTTPException(status_code=404, detail="Owner not found")
        assigned_count = db.query(Website).filter(Website.owner_id == owner.id, Website.id != website.id).count()
        if not is_admin_role(owner.role) and assigned_count >= owner.website_limit:
            raise HTTPException(status_code=403, detail="Website limit reached")
        if payload.owner_id != website.owner_id:
            try:
                storage_quota.enforce_user_storage_quota(
                    db,
                    owner,
                    incoming_bytes=storage_quota.website_storage_used_bytes(website),
                )
                new_linux_user = site_users.linux_user_for_panel_username(owner.username)
                new_root_path = site_users.site_root_for_panel_user(owner.username, website.domain)
                runtime_php_version = website.php_version if (website.app_type or "wordpress") in {"wordpress", "php"} else None
                site_users.move_site_runtime(website.root_path, new_root_path, new_linux_user, runtime_php_version)
                result = waf.sync_website_rules(website)
                if result.returncode != 0:
                    raise RuntimeError(_command_error(result))
                _rewrite_website_vhost(
                    website,
                    root_path=new_root_path,
                    linux_user=new_linux_user,
                    app_type=website.app_type or "wordpress",
                    php_version=website.php_version,
                )
                website.root_path = new_root_path
                website.linux_user = new_linux_user
            except storage_quota.StorageQuotaExceeded as exc:
                raise HTTPException(status_code=413, detail=str(exc)) from exc
            except (RuntimeError, ValueError) as exc:
                raise HTTPException(status_code=400, detail=str(exc)) from exc
        website.owner_id = payload.owner_id
    if payload.nginx_custom is not None:
        raise HTTPException(status_code=400, detail="Custom OpenLiteSpeed directives are disabled. Use managed website settings.")
    if payload.nginx_rewrite_mode is not None and payload.nginx_rewrite_mode != _website_rewrite_mode(website):
        app_type = website.app_type or "wordpress"
        next_rewrite_mode = "front_controller" if app_type == "wordpress" else "none" if app_type == "static" else payload.nginx_rewrite_mode
        runtime_php_version = website.php_version if app_type in {"wordpress", "php"} else None
        try:
            result = waf.sync_website_rules(website)
            if result.returncode != 0:
                raise RuntimeError(_command_error(result))
            _rewrite_website_vhost(
                website,
                app_type=app_type,
                php_version=website.php_version,
                rewrite_mode=next_rewrite_mode,
            )
        except (RuntimeError, ValueError, OSError, FileNotFoundError) as exc:
            raise HTTPException(status_code=400, detail=f"Cannot change webserver rewrite: {exc}") from exc
        website.nginx_rewrite_mode = next_rewrite_mode
        website.nginx_config_mode = "managed"
    if payload.waf_enabled is not None:
        ensure_role(current_user.role, Role.admin)
        try:
            result = waf.sync_website_rules(website)
            if result.returncode != 0:
                raise RuntimeError(_command_error(result))
            openlitespeed.update_waf_block(website.domain, payload.waf_enabled)
        except (RuntimeError, ValueError, FileNotFoundError) as exc:
            raise HTTPException(status_code=400, detail=str(exc)) from exc
        website.waf_enabled = payload.waf_enabled
    db.commit()
    db.refresh(website)
    log_action(db, current_user.id, "update_website", website.domain)
    return website


@router.get("/{website_id}/nginx-custom", response_model=WebsiteNginxCustom)
def get_website_webserver_custom(website_id: int, db: Session = Depends(get_db), current_user: User = Depends(get_current_user)):
    website = db.query(Website).filter(Website.id == website_id).first()
    if not website:
        raise HTTPException(status_code=404, detail="Website not found")
    if website.owner_id != current_user.id:
        ensure_role(current_user.role, Role.admin)
    return WebsiteNginxCustom(nginx_custom=website.nginx_custom or "")


@router.get("/{website_id}/nginx-config", response_model=WebsiteNginxConfig)
def get_website_webserver_config(website_id: int, db: Session = Depends(get_db), current_user: User = Depends(get_current_user)):
    ensure_role(current_user.role, Role.admin)
    website = db.query(Website).filter(Website.id == website_id).first()
    if not website:
        raise HTTPException(status_code=404, detail="Website not found")
    try:
        return WebsiteNginxConfig(nginx_config=openlitespeed.read_vhost_config(website.domain))
    except (FileNotFoundError, ValueError) as exc:
        raise HTTPException(status_code=404, detail=str(exc)) from exc


@router.put("/{website_id}/nginx-config", response_model=WebsiteOut)
def set_website_webserver_config(website_id: int, payload: WebsiteNginxConfig, request: Request, db: Session = Depends(get_db), current_user: User = Depends(get_current_user)):
    raise HTTPException(
        status_code=405,
        detail="The main vhost is managed by opanel-ent. Use managed website settings.",
    )


@router.post("/{website_id}/nginx-config/reset", response_model=WebsiteOut)
def reset_website_webserver_config(website_id: int, request: Request, db: Session = Depends(get_db), current_user: User = Depends(get_current_user)):
    ensure_role(current_user.role, Role.admin)
    website = db.query(Website).filter(Website.id == website_id).first()
    if not website:
        raise HTTPException(status_code=404, detail="Website not found")
    try:
        result = waf.sync_website_rules(website)
        if result.returncode != 0:
            raise RuntimeError(_command_error(result))
        _rewrite_website_vhost(
            website,
            custom_directives="",
        )
    except (RuntimeError, ValueError, FileNotFoundError) as exc:
        raise HTTPException(status_code=400, detail=str(exc)) from exc
    website.nginx_custom = ""
    website.nginx_config_mode = "managed"
    db.commit()
    db.refresh(website)
    log_action(db, current_user.id, "reset_webserver_config", website.domain, request=request)
    return website


@router.patch("/{website_id}/waf", response_model=WebsiteOut)
def set_website_waf(website_id: int, payload: WebsiteWafUpdate, request: Request, db: Session = Depends(get_db), current_user: User = Depends(get_current_user)):
    # A site owner runs their own WAF, including turning it off -- the blast
    # radius of that is their site and nothing else.
    website = _get_authorized_website(db, website_id, current_user)
    try:
        result = waf.sync_website_rules(website)
        if result.returncode != 0:
            raise RuntimeError(_command_error(result))
        openlitespeed.update_waf_block(website.domain, payload.waf_enabled)
    except (RuntimeError, ValueError, FileNotFoundError) as exc:
        raise HTTPException(status_code=400, detail=str(exc)) from exc
    website.waf_enabled = payload.waf_enabled
    db.commit()
    db.refresh(website)
    log_action(db, current_user.id, "update_waf", website.domain, "enabled" if payload.waf_enabled else "disabled", request=request)
    return website


@router.get("/{website_id}/logs", response_model=WebsiteLogOut)
def get_website_log(
    website_id: int,
    kind: str = Query(default="access", pattern="^(access|error)$"),
    lines: int = Query(default=200, ge=1, le=5000),
    db: Session = Depends(get_db),
    current_user: User = Depends(get_current_user),
):
    website = db.query(Website).filter(Website.id == website_id).first()
    if not website:
        raise HTTPException(status_code=404, detail="Website not found")
    if website.owner_id != current_user.id:
        ensure_role(current_user.role, Role.admin)
    try:
        return openlitespeed.read_site_log(website.domain, kind, lines)
    except (RuntimeError, ValueError) as exc:
        raise HTTPException(status_code=400, detail=str(exc)) from exc


@router.put("/{website_id}/nginx-custom", response_model=WebsiteOut)
def set_website_webserver_custom(website_id: int, payload: WebsiteNginxCustom, request: Request, db: Session = Depends(get_db), current_user: User = Depends(get_current_user)):
    website = db.query(Website).filter(Website.id == website_id).first()
    if not website:
        raise HTTPException(status_code=404, detail="Website not found")
    if website.owner_id != current_user.id:
        ensure_role(current_user.role, Role.admin)
    raise HTTPException(status_code=405, detail="Custom OpenLiteSpeed directives are disabled. Use managed website settings.")


@router.delete("/{website_id}")
def delete_website(website_id: int, request: Request, delete_files: bool = True, delete_database: bool = True, db: Session = Depends(get_db), current_user: User = Depends(get_current_user)):
    website = _get_authorized_website(db, website_id, current_user)
    db_item = db.query(DatabaseAccount).filter(DatabaseAccount.website_id == website.id).first()
    if delete_database and db_item:
        mariadb.drop_database(db_item.db_name, db_item.db_user)
    db.query(WebsiteAlias).filter(WebsiteAlias.website_id == website.id).delete(synchronize_session=False)
    try:
        openlitespeed.remove_vhost(website.domain)
    except (RuntimeError, ValueError) as exc:
        raise HTTPException(status_code=400, detail=f"Cannot delete webserver config: {exc}") from exc
    if getattr(website, "ssl_wildcard", False) and getattr(website, "ssl_mode", "none") == "letsencrypt":
        try:
            ssl.remove_wildcard_ssl(website.domain)
        except Exception:  # noqa: BLE001
            pass
    if delete_files:
        if website.linux_user:
            site_users.delete_site_runtime(website.root_path, website.linux_user)
        else:
            wordpress.delete_wordpress(website.root_path)
    if db_item:
        db.delete(db_item)
    db.delete(website)
    db.commit()
    log_action(db, current_user.id, "delete_website", website.domain, request=request)
    return {"ok": True}


@router.post("/{website_id}/fix-nginx-security")
def fix_webserver_security(website_id: int, db: Session = Depends(get_db), current_user: User = Depends(get_current_user)):
    website = db.query(Website).filter(Website.id == website_id).first()
    if not website:
        raise HTTPException(status_code=404, detail="Website not found")
    if website.owner_id != current_user.id:
        ensure_role(current_user.role, Role.admin)
    result = waf.sync_website_rules(website)
    if result.returncode != 0:
        raise HTTPException(status_code=400, detail=_command_error(result))
    target = _rewrite_website_vhost(website)
    log_action(db, current_user.id, "fix_webserver_security", website.domain)
    return {"message": f"Rewrote webserver security template for {website.domain}", "path": target}


@router.post("/{website_id}/ssl", response_model=WebsiteOut)
def enable_ssl(website_id: int, db: Session = Depends(get_db), current_user: User = Depends(get_current_user)):
    website = db.query(Website).filter(Website.id == website_id).first()
    if not website:
        raise HTTPException(status_code=404, detail="Website not found")
    if website.owner_id != current_user.id:
        ensure_role(current_user.role, Role.admin)
    previous_manual_paths = (website.ssl_cert_path, website.ssl_key_path, website.ssl_ca_path)
    previous_snapshot = ssl.snapshot_manual_ssl_domain(website.domain)
    try:
        _rewrite_website_vhost(
            website,
            preserve_existing_ssl=False,
            include_ssl=False,
        )
    except (RuntimeError, ValueError) as exc:
        raise HTTPException(status_code=400, detail=f"Cannot prepare vhost config for Let's Encrypt: {exc}") from exc
    result = ssl.issue_ssl(website.domain, _ssl_domains(website))
    if result.returncode != 0:
        if getattr(website, "ssl_mode", "none") == "manual":
            ssl.restore_manual_ssl(previous_snapshot)
        # The vhost above was rewritten without SSL so certbot could answer on
        # port 80. Put it back whatever the previous mode was: leaving a site
        # that already had HTTPS without a certificate drops it out of the 443
        # listener, and browsers holding HSTS then refuse plain HTTP too, so a
        # failed renewal would take the site off the air entirely.
        try:
            _rewrite_website_vhost(website)
        except (RuntimeError, ValueError):
            pass
        raise HTTPException(status_code=500, detail=_command_error(result))
    website.ssl_enabled = True
    website.ssl_mode = "letsencrypt"
    website.ssl_wildcard = False
    website.ssl_reuse_name = None
    website.ssl_updated_at = datetime.utcnow()
    website.ssl_cert_path = None
    website.ssl_key_path = None
    website.ssl_ca_path = None
    ssl.remove_manual_ssl_files(*previous_manual_paths)
    try:
        _rewrite_website_vhost(website)
    except (RuntimeError, ValueError) as exc:
        raise HTTPException(status_code=400, detail=f"Cannot enable SSL in webserver config: {exc}") from exc
    db.commit()
    db.refresh(website)
    log_action(db, current_user.id, "enable_ssl", website.domain)
    return website


@router.post("/{website_id}/ssl/manual", response_model=WebsiteOut)
async def install_manual_ssl(
    website_id: int,
    request: Request,
    certificate: UploadFile | None = File(default=None),
    private_key: UploadFile | None = File(default=None),
    ca_bundle: UploadFile | None = File(default=None),
    certificate_text: str | None = Form(default=None),
    private_key_text: str | None = Form(default=None),
    ca_bundle_text: str | None = Form(default=None),
    db: Session = Depends(get_db),
    current_user: User = Depends(get_current_user),
):
    website = db.query(Website).filter(Website.id == website_id).first()
    if not website:
        raise HTTPException(status_code=404, detail="Website not found")
    if website.owner_id != current_user.id:
        ensure_role(current_user.role, Role.admin)

    try:
        cert_raw = _read_ssl_input(certificate, certificate_text, "certificate")
        key_raw = _read_ssl_input(private_key, private_key_text, "private_key")
        ca_raw = _read_ssl_input(ca_bundle, ca_bundle_text, "ca_bundle", required=False)
    except ValueError as exc:
        raise HTTPException(status_code=400, detail=str(exc)) from exc

    try:
        previous_snapshot = ssl.snapshot_manual_ssl_domain(website.domain)
        written = ssl.install_manual_ssl(website.domain, cert_raw, key_raw, ca_raw, aliases=_ssl_domains(website))
        website.ssl_enabled = True
        website.ssl_mode = "manual"
        website.ssl_wildcard = False
        website.ssl_reuse_name = None
        website.ssl_cert_path = written["cert"]
        website.ssl_key_path = written["key"]
        website.ssl_ca_path = written["ca"]
        website.ssl_updated_at = datetime.utcnow()
        result = waf.sync_website_rules(website)
        if result.returncode != 0:
            raise RuntimeError(_command_error(result))
        _rewrite_website_vhost(website)
    except (RuntimeError, ValueError) as exc:
        ssl.restore_manual_ssl(previous_snapshot)
        raise HTTPException(status_code=400, detail=str(exc)) from exc
    except Exception as exc:
        ssl.restore_manual_ssl(previous_snapshot)
        raise HTTPException(status_code=400, detail=str(exc)) from exc
    db.commit()
    db.refresh(website)
    log_action(db, current_user.id, "install_manual_ssl", website.domain, request=request)
    return website


@router.post("/{website_id}/ssl/wildcard", response_model=WebsiteOut)
def enable_wildcard_ssl(
    website_id: int,
    payload: WildcardSslRequest,
    request: Request,
    db: Session = Depends(get_db),
    current_user: User = Depends(get_current_user),
):
    """Issue a Let's Encrypt cert covering the domain and *.domain via the
    Cloudflare DNS-01 challenge. The API token is stored encrypted for renewal."""
    website = db.query(Website).filter(Website.id == website_id).first()
    if not website:
        raise HTTPException(status_code=404, detail="Website not found")
    if website.owner_id != current_user.id:
        ensure_role(current_user.role, Role.admin)

    token = payload.api_token or (decrypt(website.ssl_dns_api_token) if website.ssl_dns_api_token else "")
    if not token:
        raise HTTPException(status_code=400, detail="A Cloudflare API token is required to issue a wildcard certificate")

    previous_manual_paths = (website.ssl_cert_path, website.ssl_key_path, website.ssl_ca_path)
    previous_snapshot = ssl.snapshot_manual_ssl_domain(website.domain)
    try:
        _rewrite_website_vhost(website, preserve_existing_ssl=False, include_ssl=False)
    except (RuntimeError, ValueError) as exc:
        raise HTTPException(status_code=400, detail=f"Cannot prepare vhost config for the DNS challenge: {exc}") from exc

    email = payload.email or settings.ssl_email or None
    result = ssl.issue_wildcard_ssl(website.domain, token, email=email)
    if result.returncode != 0:
        if getattr(website, "ssl_mode", "none") == "manual":
            ssl.restore_manual_ssl(previous_snapshot)
        try:
            _rewrite_website_vhost(website)
        except (RuntimeError, ValueError):
            pass
        raise HTTPException(status_code=500, detail=_command_error(result))

    website.ssl_enabled = True
    website.ssl_mode = "letsencrypt"
    website.ssl_wildcard = True
    website.ssl_reuse_name = None
    website.ssl_dns_provider = payload.provider
    website.ssl_dns_api_token = encrypt(token)
    website.ssl_updated_at = datetime.utcnow()
    website.ssl_cert_path = None
    website.ssl_key_path = None
    website.ssl_ca_path = None
    ssl.remove_manual_ssl_files(*previous_manual_paths)
    try:
        _rewrite_website_vhost(website)
    except (RuntimeError, ValueError) as exc:
        raise HTTPException(status_code=400, detail=f"Cannot enable SSL in webserver config: {exc}") from exc
    db.commit()
    db.refresh(website)
    log_action(db, current_user.id, "enable_wildcard_ssl", website.domain, request=request)
    return website


@router.get("/{website_id}/available-certificates", response_model=List[AvailableCertificateOut])
def website_available_certificates(website_id: int, db: Session = Depends(get_db), current_user: User = Depends(get_current_user)):
    website = _get_authorized_website(db, website_id, current_user)
    ssl.refresh_cert_store()
    return ssl.list_available_certificates(website.domain)


@router.get("/available-certificates", response_model=List[AvailableCertificateOut])
def available_certificates_for_domain(
    domain: str = Query(..., min_length=3, max_length=255),
    db: Session = Depends(get_db),
    current_user: User = Depends(get_current_user),
):
    """Certificates on the box that cover ``domain`` — used by the create-website
    form before the site exists. Only covering certs are returned so one tenant
    cannot enumerate another's certificate names."""
    safe_domain = (domain or "").strip().lower()
    ssl.refresh_cert_store()
    return [c for c in ssl.list_available_certificates(safe_domain) if c["covers_domain"]]


@router.post("/{website_id}/ssl/reuse", response_model=WebsiteOut)
def reuse_existing_ssl(
    website_id: int,
    payload: ReuseSslRequest,
    request: Request,
    db: Session = Depends(get_db),
    current_user: User = Depends(get_current_user),
):
    """Serve this website with a certificate already installed on the box
    (a Let's Encrypt cert — including a wildcard — or a manually installed one)."""
    website = db.query(Website).filter(Website.id == website_id).first()
    if not website:
        raise HTTPException(status_code=404, detail="Website not found")
    if website.owner_id != current_user.id:
        ensure_role(current_user.role, Role.admin)

    usable, reason = ssl.reuse_cert_covers(payload.name, website.domain)
    if not usable:
        raise HTTPException(status_code=400, detail=reason)

    previous_manual_paths = (website.ssl_cert_path, website.ssl_key_path, website.ssl_ca_path)
    is_wildcard = any(
        entry["name"] == payload.name and entry["is_wildcard"]
        for entry in ssl.list_available_certificates(website.domain)
    )
    website.ssl_enabled = True
    website.ssl_mode = "reuse"
    website.ssl_reuse_name = payload.name
    website.ssl_wildcard = is_wildcard
    website.ssl_updated_at = datetime.utcnow()
    website.ssl_cert_path = None
    website.ssl_key_path = None
    website.ssl_ca_path = None
    try:
        _rewrite_website_vhost(website)
    except (RuntimeError, ValueError, FileNotFoundError) as exc:
        raise HTTPException(status_code=400, detail=f"Cannot apply the certificate: {exc}") from exc
    ssl.remove_manual_ssl_files(*previous_manual_paths)
    db.commit()
    db.refresh(website)
    log_action(db, current_user.id, "reuse_ssl", website.domain, payload.name, request=request)
    return website
