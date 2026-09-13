from pathlib import Path
from urllib.parse import urlparse

from fastapi import Request

from app.core.config import settings
from app.services import panel_settings


def _request_host_without_port(request: Request) -> str:
    host = request.headers.get("x-forwarded-host") or request.headers.get("host") or ""
    host = host.split(",", 1)[0].strip()
    if host.startswith("[") and "]" in host:
        return host[1:host.index("]")]
    return host.rsplit(":", 1)[0] if ":" in host else host


def tools_base_url(request: Request) -> str:
    """Public OpenLiteSpeed URL for phpMyAdmin helper routes.

    The panel itself listens on PANEL_PORT, but phpMyAdmin is
    served by OpenLiteSpeed on the normal web ports. Keep generated URLs off :2222.
    """
    current_panel_url = panel_settings.current_settings().get("panel_url") or settings.panel_url
    parsed_panel = urlparse(current_panel_url if "://" in current_panel_url else "")
    host = settings.panel_domain or parsed_panel.hostname or _request_host_without_port(request)
    has_panel_cert = (
        bool(settings.panel_ssl_cert)
        and bool(settings.panel_ssl_key)
        and Path(settings.panel_ssl_cert).exists()
        and Path(settings.panel_ssl_key).exists()
    )
    scheme = "https" if has_panel_cert else "http"
    return f"{scheme}://{host}"
