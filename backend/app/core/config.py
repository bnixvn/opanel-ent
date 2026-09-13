from pydantic import Field, field_validator
from pydantic_settings import BaseSettings


DEFAULT_SECRET_KEY = "change-this-secret-key"


class Settings(BaseSettings):
    app_name: str = "OPanel Enterprise"
    app_env: str = "development"
    secret_key: str = DEFAULT_SECRET_KEY
    access_token_expire_minutes: int = 120  # was 720; shorter window if a token is stolen
    database_url: str = "sqlite:///./opanel-ent.db"
    command_dry_run: bool = True
    allowed_origins: str = Field(default="")
    backup_root: str = "/var/backups/opanel-ent"
    da_backup_dir: str = "/home/admin/opanel-ent-backups/da"
    # Legacy field kept for migration; use webserver_vhosts_dir instead
    nginx_sites_available: str = "/usr/local/lsws/conf/opanel-ent/vhosts"
    # OpenLiteSpeed paths
    webserver_vhosts_dir: str = "/usr/local/lsws/conf/opanel-ent/vhosts"
    ols_conf_root: str = "/usr/local/lsws/conf/opanel-ent"
    ols_bin: str = "/usr/local/lsws/bin/lswsctrl"
    default_php_version: str = "8.4"
    ssl_email: str = ""
    redis_url: str = "redis://localhost:6379/0"
    rate_limit_backend: str = "redis"
    panel_url: str = ""
    panel_domain: str = ""
    panel_port: int = 2222
    # Address the API binds. "::" serves IPv6 and IPv4 from one socket;
    # the installer writes it, so the model has to know the name or start-up
    # fails on an unexpected env var.
    panel_bind_host: str = "0.0.0.0"
    panel_ssl_cert: str = ""
    panel_ssl_key: str = ""
    frontend_dist: str = "/opt/opanel-ent/frontend/dist"
    totp_issuer: str = "OPanel Enterprise"
    github_token: str = ""
    # Malware scanning is OPTIONAL and OFF by default. It only becomes active
    # after an admin enables it in the panel, which triggers an on-demand
    # install of clamav-daemon. Leaving this False keeps opanel-ent lightweight.
    malware_scan_enabled: bool = False
    clamav_socket_path: str = "/run/clamav/clamd.sock"
    # Real-time protection: an LMD inotify monitor on /home. Off by default --
    # it costs RAM per watched file and is a deliberate opt-in from the panel.
    malware_realtime_enabled: bool = False
    # When True, uploaded files are scanned in memory before being accepted.
    malware_scan_on_upload: bool = True
    # When True, a file a scan flags as malware is moved to quarantine
    # automatically. It stops being served at once; restore it from the
    # Quarantine list if it was a false positive.
    malware_auto_quarantine: bool = True
    # When true, ``app.core.secrets.decrypt`` refuses to read legacy plaintext
    # values (the deprecated migration grace path). Production should leave
    # this True so any unmigrated row surfaces as a hard error instead of
    # silently leaking through. Set STRICT_DECRYPT=false during a one-shot
    # migration window only.
    strict_decrypt: bool = True
    pma_signon_secret: str = ""

    @field_validator("secret_key")
    @classmethod
    def validate_secret_key(cls, value: str, info):
        app_env = (info.data.get("app_env") or "development").lower()
        if app_env == "production" and (value == DEFAULT_SECRET_KEY or len(value) < 32):
            raise ValueError("SECRET_KEY must be changed to a strong random value in production")
        return value

    @field_validator("allowed_origins")
    @classmethod
    def validate_allowed_origins(cls, value: str, info):
        app_env = (info.data.get("app_env") or "development").lower()
        normalized = [o.strip() for o in (value or "").split(",") if o.strip()]
        if app_env == "production":
            if "*" in normalized:
                raise ValueError("ALLOWED_ORIGINS cannot be '*' in production with credentials enabled")
            for origin in normalized:
                if not origin.startswith(("http://", "https://")):
                    raise ValueError(f"ALLOWED_ORIGINS entry must include scheme: {origin}")
        return value

    @field_validator("rate_limit_backend")
    @classmethod
    def validate_rate_limit_backend(cls, value: str, info):
        backend = (value or "memory").strip().lower()
        if backend not in {"memory", "redis"}:
            raise ValueError("RATE_LIMIT_BACKEND must be 'memory' or 'redis'")
        app_env = (info.data.get("app_env") or "development").lower()
        if app_env == "production" and backend != "redis":
            raise ValueError("RATE_LIMIT_BACKEND=redis is required in production")
        return backend

    @property
    def cors_origins(self) -> list[str]:
        origins = []
        for origin in self.allowed_origins.split(","):
            origin = origin.strip().rstrip("/")
            if not origin or origin == "*":
                continue
            origins.append(origin)
        return origins

    class Config:
        env_file = ".env"


settings = Settings()
