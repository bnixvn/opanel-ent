import logging
import mimetypes
import os
from pathlib import Path

from fastapi import FastAPI, HTTPException, Depends, Request, Response, Form
from fastapi.middleware.cors import CORSMiddleware
from fastapi.middleware.gzip import GZipMiddleware
from fastapi.responses import FileResponse, JSONResponse, RedirectResponse
from fastapi.staticfiles import StaticFiles
from sqlalchemy.orm import Session

from app.api import api_tokens, auth, databases, firewall, maintenance, panel_settings as panel_settings_api, plans, provisioning, services, terminal, updates, users, waf, websites
from app.core.config import settings
from app.core.database import get_db, run_migrations
from app.core.version import APP_VERSION
from app.services import panel_settings as panel_brand_settings

run_migrations()

# Secure default umask: files get 644 (-rw-r--r--), dirs get 755 (rwxr-xr-x)
os.umask(0o022)

logger = logging.getLogger("OPanel Enterprise")

app = FastAPI(title="OPanel Enterprise API", version=APP_VERSION)

# Refuse to start in production with unsafe defaults.
if settings.app_env.lower() == "production":
    if settings.command_dry_run:
        raise RuntimeError(
            "COMMAND_DRY_RUN must be False in production. "
            "Set COMMAND_DRY_RUN=false in the environment."
        )

cors_origins = settings.cors_origins
if not cors_origins and settings.app_env != "production":
    cors_origins = ["http://localhost:5173", "http://127.0.0.1:5173"]

app.add_middleware(
    CORSMiddleware,
    allow_origins=cors_origins,
    allow_credentials=True,
    allow_methods=["GET", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"],
    allow_headers=["Authorization", "Content-Type", "X-CSRF-Token"],
)

# Compress responses (the ~1.8 MB JS bundle drops to ~490 KB, JSON lists shrink
# too). Matters most for admins reaching the panel over a slow/long-haul link.
app.add_middleware(GZipMiddleware, minimum_size=700, compresslevel=6)


def _is_potentially_trustworthy_origin(request) -> bool:
    host = (request.url.hostname or "").lower()
    return request.url.scheme == "https" or host in {"localhost", "127.0.0.1", "::1"}


@app.exception_handler(Exception)
async def unhandled_exception_handler(request, exc):
    logger.exception("Unhandled request error: %s %s", request.method, request.url.path)
    if settings.app_env.lower() == "production":
        return JSONResponse(status_code=500, content={"detail": "Internal server error"})
    return JSONResponse(status_code=500, content={"detail": str(exc)})


@app.middleware("http")
async def security_headers(request, call_next):
    response = await call_next(request)
    response.headers.setdefault("X-Content-Type-Options", "nosniff")
    if _is_potentially_trustworthy_origin(request):
        response.headers.setdefault("Cross-Origin-Opener-Policy", "same-origin")
        response.headers.setdefault("Cross-Origin-Resource-Policy", "same-origin")
    response.headers.setdefault("X-Permitted-Cross-Domain-Policies", "none")
    response.headers.setdefault("Referrer-Policy", "strict-origin-when-cross-origin")
    response.headers.setdefault(
        "Permissions-Policy",
        "accelerometer=(), autoplay=(), camera=(), display-capture=(), encrypted-media=(), "
        "fullscreen=(), geolocation=(), gyroscope=(), magnetometer=(), microphone=(), midi=(), "
        "payment=(), usb=()",
    )
    response.headers.setdefault(
        "Content-Security-Policy",
        "default-src 'self'; "
        "script-src 'self'; "
        "style-src 'self' 'unsafe-inline'; "
        "img-src 'self' data:; "
        "font-src 'self' data:; "
        "connect-src 'self' ws: wss:; "
        "base-uri 'self'; "
        "form-action 'self'",
    )
    if settings.app_env.lower() == "production" and request.url.scheme == "https":
        response.headers.setdefault(
            "Strict-Transport-Security", "max-age=31536000; includeSubDomains"
        )
    return response


app.include_router(auth.router, prefix="/api")
app.include_router(users.router, prefix="/api")
app.include_router(websites.router, prefix="/api")
app.include_router(databases.router, prefix="/api")
app.include_router(firewall.router, prefix="/api")
app.include_router(services.router, prefix="/api")
app.include_router(updates.router, prefix="/api")
app.include_router(waf.router, prefix="/api")
app.include_router(maintenance.router, prefix="/api")
app.include_router(panel_settings_api.router, prefix="/api")
app.include_router(terminal.router, prefix="/api")
app.include_router(provisioning.router, prefix="/api")
app.include_router(api_tokens.router, prefix="/api")
app.include_router(plans.router, prefix="/api")


@app.get("/api/health")
def health():
    return {
        "status": "ok",
        "name": panel_brand_settings.current_settings().get("app_name") or "OPanel Enterprise",
        "version": APP_VERSION,
    }


frontend_dist = Path(settings.frontend_dist)
assets_dir = frontend_dist / "assets"
# The bundle ships self-hosted Lexend woff2 files under /assets (the panel's
# CSP is font-src 'self' data:, so a font CDN is not an option). Not every
# host has .woff2 in its mime database, and StaticFiles would then serve it
# as text/plain, so register it explicitly.
mimetypes.add_type("font/woff2", ".woff2")
mimetypes.add_type("font/woff", ".woff")


class _ImmutableStaticFiles(StaticFiles):
    """Vite emits content-hashed filenames under /assets, so a cache entry is
    valid forever — tell the browser not to revalidate on every page load."""

    async def get_response(self, path, scope):
        response = await super().get_response(path, scope)
        if response.status_code < 400:
            response.headers["Cache-Control"] = "public, max-age=31536000, immutable"
        return response


if assets_dir.exists():
    app.mount("/assets", _ImmutableStaticFiles(directory=str(assets_dir)), name="assets")


@app.get("/favicon.png", include_in_schema=False)
def favicon():
    custom = panel_brand_settings.current_settings().get("favicon_url") or ""
    if custom.startswith("/brand-assets/"):
        filename = custom.split("/brand-assets/", 1)[1].split("?", 1)[0]
        path, media_type = panel_brand_settings.asset_path(filename)
        return FileResponse(
            path,
            media_type=media_type,
            headers={"Cache-Control": "no-cache, must-revalidate"},
        )
    path = frontend_dist / "favicon.png"
    if path.exists():
        return FileResponse(
            path,
            media_type="image/png",
            headers={"Cache-Control": "no-cache, must-revalidate"},
        )
    raise HTTPException(status_code=404, detail="Not found")


@app.get("/brand-assets/{filename}", include_in_schema=False)
def brand_asset(filename: str):
    path, media_type = panel_brand_settings.asset_path(filename)
    return FileResponse(
        path,
        media_type=media_type,
        headers={"Cache-Control": "no-cache, must-revalidate"},
    )


@app.get("/sso", include_in_schema=False)
def sso_intermediary():
    """Serve an intermediary page that reads the SSO token from the URL
    fragment (#) and POSTs it to /sso.

    Using the fragment keeps the token out of server access logs and proxy
    logs because browsers never send the fragment in HTTP requests.

    The auto-submit script is loaded from /sso.js (same-origin) rather than
    inlined here: the panel's CSP sends script-src 'self' with no
    'unsafe-inline', so an inline <script> block is silently blocked by the
    browser and the token is never submitted (the login just appears to
    hang, then falls through to the normal login page).
    """
    from fastapi.responses import HTMLResponse

    return HTMLResponse(
        """<!DOCTYPE html>
<html><head><meta charset="utf-8"><title>OPanel Enterprise SSO</title></head>
<body style="display:flex;justify-content:center;align-items:center;height:100vh;font-family:sans-serif">
<p>Logging in…</p>
<form id="sso-form" method="POST" action="/sso">
<input type="hidden" name="token" id="sso-token">
</form>
<script src="/sso.js"></script>
</body></html>""",
        headers={"Cache-Control": "no-store, no-cache, must-revalidate"},
    )


@app.get("/sso.js", include_in_schema=False)
def sso_intermediary_script():
    """Same-origin auto-submit script for the /sso intermediary page.

    Served as a real script-src 'self' resource (see sso_intermediary())
    rather than inlined, so the CSP doesn't block it from running.
    """
    from fastapi.responses import Response

    return Response(
        content="""(function(){
  var hash = window.location.hash.replace(/^#/, "");
  if (!hash) { document.body.innerHTML = "<p>Invalid SSO link.</p>"; return; }
  document.getElementById("sso-token").value = hash;
  document.getElementById("sso-form").submit();
})();
""",
        media_type="application/javascript",
        headers={"Cache-Control": "no-store, no-cache, must-revalidate"},
    )


@app.post("/sso", include_in_schema=False)
def sso_login_post(request: Request, response: Response, token: str = Form(...), db: Session = Depends(get_db)):
    """Consume a one-time SSO token submitted via POST body."""
    from app.api.auth import _issue_login_session
    from app.models.entities import User
    from app.services.provisioning import consume_sso_token

    user_id = consume_sso_token(db, token)
    if user_id is None:
        raise HTTPException(status_code=401, detail="Invalid or expired SSO token")
    user = db.query(User).filter(User.id == user_id, User.is_active.is_(True)).first()
    if user is None:
        raise HTTPException(status_code=401, detail="User not found or inactive")
    redirect = RedirectResponse(url="/", status_code=302)
    _issue_login_session(redirect, request, user)
    return redirect


@app.get("/sso/{token}", include_in_schema=False)
def sso_login(token: str, request: Request, response: Response, db: Session = Depends(get_db)):
    """Consume a one-time SSO token from WHMCS bpanel and create a session.

    Deprecated: kept for backward compatibility. New SSO links use the
    fragment-based /sso endpoint to keep the token out of server logs.
    """
    from app.api.auth import _issue_login_session
    from app.models.entities import User
    from app.services.provisioning import consume_sso_token

    user_id = consume_sso_token(db, token)
    if user_id is None:
        raise HTTPException(status_code=401, detail="Invalid or expired SSO token")
    user = db.query(User).filter(User.id == user_id, User.is_active.is_(True)).first()
    if user is None:
        raise HTTPException(status_code=401, detail="User not found or inactive")
    redirect = RedirectResponse(url="/", status_code=302)
    _issue_login_session(redirect, request, user)
    return redirect

@app.get("/{full_path:path}", include_in_schema=False)
def serve_spa(full_path: str):
    """Serve the built React app directly from FastAPI on the panel port."""
    if full_path.startswith("api/"):
        raise HTTPException(status_code=404, detail="Not found")
    requested = (frontend_dist / full_path).resolve()
    try:
        requested.relative_to(frontend_dist.resolve())
    except ValueError:
        raise HTTPException(status_code=404, detail="Not found")
    if requested.is_file():
        return FileResponse(requested)
    if full_path.startswith("assets/"):
        raise HTTPException(status_code=404, detail="Not found")
    index = frontend_dist / "index.html"
    if index.exists():
        return FileResponse(index)
    return {"detail": "Frontend build not found", "path": str(index)}
