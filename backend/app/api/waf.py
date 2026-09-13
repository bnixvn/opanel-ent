from fastapi import APIRouter, Depends, HTTPException, Query
from pydantic import BaseModel, Field
from sqlalchemy.orm import Session

from app.api.deps import get_current_user
from app.core.database import get_db
from app.core.permissions import Role, ensure_role, is_admin_role
from app.models.entities import User, Website
from app.services import openlitespeed, waf

router = APIRouter(prefix="/waf", tags=["waf"])


class WafCustomRulesUpdate(BaseModel):
    content: str = ""


class WebsiteWafRulesUpdate(BaseModel):
    enabled_rule_ids: list[str] = Field(default_factory=list)
    custom_rules: str = ""
    # Omitted -> leave this site's bot settings as they are.
    bot_blocking_enabled: bool | None = None
    bot_extra: list[str] | str | None = None


class BadBotListUpdate(BaseModel):
    """The server-wide list. Accepts a list or a newline/comma separated blob."""
    patterns: list[str] | str = Field(default_factory=list)


def _require_admin(current_user: User) -> None:
    ensure_role(current_user.role, Role.admin)


def _is_admin(current_user: User) -> bool:
    # is_admin_role, not a string compare: it also maps the legacy super_admin.
    return is_admin_role(current_user.role)


def _website_or_404(db: Session, website_id: int) -> Website:
    website = db.query(Website).filter(Website.id == website_id).first()
    if not website:
        raise HTTPException(status_code=404, detail="Website not found")
    return website


def _authorized_website(db: Session, website_id: int, current_user: User) -> Website:
    """A site the caller may configure: their own, or anything for an admin."""
    website = _website_or_404(db, website_id)
    if website.owner_id != current_user.id:
        ensure_role(current_user.role, Role.admin)
    return website


def _readable_domains(db: Session, current_user: User) -> list[str]:
    """The domains whose logs this caller may read. Also the allow-list that
    stops a `domain=` parameter reaching someone else's site."""
    query = db.query(Website)
    if not _is_admin(current_user):
        query = query.filter(Website.owner_id == current_user.id)
    return [website.domain for website in query.order_by(Website.domain.asc()).all()]


@router.get("/status")
def get_waf_status(current_user: User = Depends(get_current_user)):
    _require_admin(current_user)
    return waf.status().__dict__


@router.get("/rules")
def get_waf_rules(current_user: User = Depends(get_current_user)):
    # Anyone may read the rule catalogue -- it is what the per-site checkboxes
    # are drawn from. The engine status and the server-wide rule file stay
    # admin-only: they describe the machine, not the caller's sites.
    definitions = waf.default_rule_definitions()
    if not _is_admin(current_user):
        return {"default_rule_definitions": definitions}
    return {
        "status": waf.status().__dict__,
        "default_rules": waf.default_rules().stdout,
        "default_rule_definitions": definitions,
        "custom_rules": waf.custom_rules().stdout,
    }


@router.get("/access-logs")
def get_waf_access_logs(
    domain: str = "",
    verdict: str = Query(default="", pattern="^(|allow|block)$"),
    q: str = "",
    limit: int = Query(default=50, ge=1, le=500),
    offset: int = Query(default=0, ge=0),
    lines: int = Query(default=5000, ge=1, le=20000),
    db: Session = Depends(get_db),
    current_user: User = Depends(get_current_user),
):
    domains = _readable_domains(db, current_user)
    try:
        return waf.access_log_report(domains, domain=domain, verdict=verdict, query=q, limit=limit, offset=offset, lines=lines)
    except (RuntimeError, ValueError) as exc:
        raise HTTPException(status_code=400, detail=str(exc)) from exc


@router.delete("/access-logs")
def clear_waf_access_logs(
    domain: str = "",
    db: Session = Depends(get_db),
    current_user: User = Depends(get_current_user),
):
    domains = _readable_domains(db, current_user)
    try:
        result = waf.clear_access_logs(domains, domain=domain)
    except ValueError as exc:
        raise HTTPException(status_code=400, detail=str(exc)) from exc
    if result.returncode != 0:
        raise HTTPException(status_code=400, detail=(result.stderr or result.stdout or "Could not clear WAF access logs").strip())
    return {**result.__dict__, "message": (result.stdout or "WAF access logs cleared.").strip()}


@router.get("/websites/{website_id}")
def get_website_waf(website_id: int, db: Session = Depends(get_db), current_user: User = Depends(get_current_user)):
    website = _authorized_website(db, website_id, current_user)
    return waf.site_config(website)


@router.put("/websites/{website_id}")
def save_website_waf(payload: WebsiteWafRulesUpdate, website_id: int, db: Session = Depends(get_db), current_user: User = Depends(get_current_user)):
    website = _authorized_website(db, website_id, current_user)
    # Custom rules are raw ModSecurity config written into a file the web
    # server includes. ModSecurity actions can run programs, and a rule that
    # fails to parse can stop the server coming back up, so a site owner picks
    # from the rule catalogue and never writes directives.
    custom_rules = payload.custom_rules
    if not _is_admin(current_user):
        stored = waf.website_custom_rules(website)
        if (custom_rules or "").strip() != stored.strip():
            raise HTTPException(
                status_code=403,
                detail="Custom WAF rules can only be changed by an administrator.",
            )
        custom_rules = stored
    try:
        result = waf.save_website_config(
            website,
            payload.enabled_rule_ids,
            custom_rules,
            bot_blocking_enabled=payload.bot_blocking_enabled,
            bot_extra=payload.bot_extra,
        )
    except ValueError as exc:
        raise HTTPException(status_code=400, detail=str(exc)) from exc
    if result.returncode != 0:
        raise HTTPException(status_code=400, detail=(result.stderr or result.stdout or "Could not save WAF rules").strip())
    db.add(website)
    db.commit()
    db.refresh(website)
    if website.waf_enabled:
        try:
            openlitespeed.update_waf_block(website.domain, True)
        except (RuntimeError, ValueError, FileNotFoundError) as exc:
            raise HTTPException(status_code=400, detail=str(exc)) from exc
    data = waf.site_config(website)
    data["message"] = "Website WAF rules saved."
    return data


@router.get("/bad-bots")
def get_bad_bots(current_user: User = Depends(get_current_user)):
    return {
        "patterns": waf.global_bad_bots(),
        "max_patterns": waf.MAX_BOT_PATTERNS,
    }


@router.put("/bad-bots")
def save_bad_bots(
    payload: BadBotListUpdate,
    db: Session = Depends(get_db),
    current_user: User = Depends(get_current_user),
):
    """Save the server-wide list and push it to every site in one pass.

    Without the re-sync the new list would sit in panel-settings.json and not
    reach a single vhost until each site happened to be saved individually.
    """
    _require_admin(current_user)
    try:
        patterns = waf.set_global_bad_bots(payload.patterns)
    except ValueError as exc:
        raise HTTPException(status_code=400, detail=str(exc)) from exc
    outcome = waf.resync_all_websites(db)
    message = f"Bad bot list saved ({len(patterns)} patterns) and applied to {outcome['total']} website(s)."
    if outcome["failed"]:
        message += " Could not update: " + ", ".join(outcome["failed"])
    return {
        "patterns": patterns,
        "applied_to": outcome["total"],
        "failed": outcome["failed"],
        "message": message,
    }


@router.put("/rules/custom")
def save_waf_custom_rules(payload: WafCustomRulesUpdate, current_user: User = Depends(get_current_user)):
    _require_admin(current_user)
    try:
        result = waf.save_custom_rules(payload.content)
    except ValueError as exc:
        raise HTTPException(status_code=400, detail=str(exc)) from exc
    if result.returncode != 0:
        raise HTTPException(status_code=400, detail=(result.stderr or result.stdout or "Could not save WAF rules").strip())
    return result.__dict__


@router.post("/install")
def install_waf(current_user: User = Depends(get_current_user)):
    _require_admin(current_user)
    return waf.install_engine().__dict__


@router.post("/update-rules")
def update_waf_rules(current_user: User = Depends(get_current_user)):
    _require_admin(current_user)
    return waf.update_rules().__dict__
