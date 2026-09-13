from pathlib import Path

from app.core.config import settings
from app.schemas.schemas import PhpConfigUpdate
from app.services.shell import shell

SUPPORTED_PHP_VERSIONS = ("7.4", "8.1", "8.2", "8.3", "8.4", "8.5")


def _lsphp_version(php_version: str) -> str:
    return php_version.replace(".", "")


def _lsphp_opanel_ini_path(php_version: str) -> Path:
    lsphp_ver = _lsphp_version(php_version)
    return Path(f"/usr/local/lsws/lsphp{lsphp_ver}/etc/php/{php_version}/mods-available/99-opanel-ent.ini")


def _legacy_lsphp_opanel_ini_path(php_version: str) -> Path:
    lsphp_ver = _lsphp_version(php_version)
    return Path(f"/usr/local/lsws/lsphp{lsphp_ver}/etc/php.d/99-opanel-ent.ini")


def _lsphp_litespeed_ini_path(php_version: str) -> Path:
    lsphp_ver = _lsphp_version(php_version)
    return Path(f"/usr/local/lsws/lsphp{lsphp_ver}/etc/php/{php_version}/litespeed/php.ini")


def _safe_ini_value(value: str) -> str:
    if "\n" in value or "\r" in value or "\x00" in value:
        raise ValueError("Invalid PHP ini value")
    return value


OPCACHE_PREFIX = "opcache."


def _existing_opcache_lines(php_version: str) -> list[str]:
    """Keep the tuned opcache block when this editor rewrites the ini.

    Two things write this file: the auto-tuner, which emits the whole opcache
    block, and update_php_ini, which only knows the handful of basic
    directives. Re-emitting just the basics silently discarded the tuning, so
    saving an unrelated field like memory_limit reset opcache to PHP defaults.
    """
    try:
        raw = _lsphp_opanel_ini_path(php_version).read_text(encoding="utf-8", errors="ignore")
    except OSError:
        return []
    kept = []
    for line in raw.splitlines():
        stripped = line.strip()
        if not stripped or stripped.startswith(";") or "=" not in stripped:
            continue
        key = stripped.split("=", 1)[0].strip()
        if key.startswith(OPCACHE_PREFIX) and key not in {"opcache.enable", "opcache.enable_cli"}:
            kept.append(stripped)
    return kept


def update_php_ini(payload: PhpConfigUpdate) -> str:
    php_version = payload.php_version
    if php_version not in SUPPORTED_PHP_VERSIONS:
        allowed = ", ".join(sorted(SUPPORTED_PHP_VERSIONS))
        raise ValueError(f"Unsupported PHP version. Allowed: {allowed}")
    display_errors = "On" if str(payload.display_errors).lower() in {"1", "true", "on", "yes"} else "Off"
    opcache_enable = 1 if payload.opcache_enable else 0
    content = "\n".join([
        f"display_errors = {display_errors}",
        f"memory_limit = {_safe_ini_value(payload.memory_limit)}",
        f"upload_max_filesize = {_safe_ini_value(payload.upload_max_filesize)}",
        f"post_max_size = {_safe_ini_value(payload.post_max_size)}",
        f"max_execution_time = {int(payload.max_execution_time)}",
        f"max_input_time = {int(payload.max_input_time)}",
        f"max_input_vars = {int(payload.max_input_vars)}",
        "max_file_uploads = 100",
        f"opcache.enable = {opcache_enable}",
        # enable_cli follows the same switch so wp-cli and cron runs behave
        # like web requests instead of quietly diverging.
        f"opcache.enable_cli = {opcache_enable}",
        *_existing_opcache_lines(php_version),
        "",
    ])
    target = _lsphp_opanel_ini_path(php_version)
    if settings.command_dry_run:
        return content
    shell.privileged(
        "php-config-write",
        helper_args=[php_version],
        input=content,
        fallback=[
            "bash",
            "-lc",
            "mkdir -p /usr/local/lsws/lsphp$2/etc/php/$1/mods-available && "
            "cat > /usr/local/lsws/lsphp$2/etc/php/$1/mods-available/99-opanel-ent.ini && "
            "(systemctl restart lshttpd.service || /usr/local/lsws/bin/lswsctrl restart)",
            "opanel-ent-php-config-write",
            php_version,
            _lsphp_version(php_version),
        ],
    )
    return str(target)


PHP_CONFIG_KEYS = {
    "display_errors": "Off",
    "memory_limit": "1024M",
    "upload_max_filesize": "1024M",
    "post_max_size": "1024M",
    "max_execution_time": "300",
    "max_input_time": "600",
    "max_input_vars": "10000",
    "max_file_uploads": "100",
}


def default_php_config(php_version: str) -> dict:
    if php_version not in SUPPORTED_PHP_VERSIONS:
        allowed = ", ".join(sorted(SUPPORTED_PHP_VERSIONS))
        raise ValueError(f"Unsupported PHP version. Allowed: {allowed}")
    return {
        "php_version": php_version,
        "display_errors": PHP_CONFIG_KEYS["display_errors"],
        "memory_limit": PHP_CONFIG_KEYS["memory_limit"],
        "upload_max_filesize": PHP_CONFIG_KEYS["upload_max_filesize"],
        "post_max_size": PHP_CONFIG_KEYS["post_max_size"],
        "max_execution_time": int(PHP_CONFIG_KEYS["max_execution_time"]),
        "max_input_time": int(PHP_CONFIG_KEYS["max_input_time"]),
        "max_input_vars": int(PHP_CONFIG_KEYS["max_input_vars"]),
    }


def restore_default_php_ini(php_version: str) -> str:
    return update_php_ini(PhpConfigUpdate(**default_php_config(php_version)))


def read_php_ini(php_version: str) -> dict:
    if php_version not in SUPPORTED_PHP_VERSIONS:
        allowed = ", ".join(sorted(SUPPORTED_PHP_VERSIONS))
        raise ValueError(f"Unsupported PHP version. Allowed: {allowed}")
    values = dict(PHP_CONFIG_KEYS)
    values["opcache.enable"] = "1"
    for path in [
        _lsphp_litespeed_ini_path(php_version),
        _legacy_lsphp_opanel_ini_path(php_version),
        _lsphp_opanel_ini_path(php_version),
    ]:
        if not path.exists():
            continue
        for line in path.read_text(encoding="utf-8", errors="ignore").splitlines():
            line = line.strip()
            if not line or line.startswith(";") or "=" not in line:
                continue
            key, value = [part.strip() for part in line.split("=", 1)]
            if key in values:
                values[key] = value
    values["php_version"] = php_version
    values["max_execution_time"] = int(values["max_execution_time"])
    values["max_input_time"] = int(values["max_input_time"])
    values["max_input_vars"] = int(values["max_input_vars"])
    values["max_file_uploads"] = int(values["max_file_uploads"])
    raw_opcache = str(values.pop("opcache.enable", "1")).strip().lower()
    values["opcache_enable"] = raw_opcache not in {"0", "off", "false", ""}
    return values


def list_installed_php() -> list[str]:
    """List PHP versions that are currently installed (LSPHP)."""
    installed = []
    for version in SUPPORTED_PHP_VERSIONS:
        lsphp_ver = version.replace(".", "")
        lsphp_bin = Path(f"/usr/local/lsws/lsphp{lsphp_ver}/bin/lsphp")
        if lsphp_bin.exists():
            installed.append(version)
    return sorted(installed, key=lambda v: [int(x) for x in v.split(".")])


def install_php(php_version: str) -> dict:
    """Install a PHP version or repair the opanel-ent extension set via apt."""
    if php_version not in SUPPORTED_PHP_VERSIONS:
        allowed = ", ".join(sorted(SUPPORTED_PHP_VERSIONS))
        raise ValueError(f"Unsupported PHP version. Allowed: {allowed}")

    lsphp_ver = php_version.replace(".", "")
    already_installed = Path(f"/usr/local/lsws/lsphp{lsphp_ver}/bin/lsphp").exists()

    if settings.command_dry_run:
        action = "repair" if already_installed else "install"
        return {"status": "dry_run", "message": f"Would {action} lsphp{lsphp_ver} and opanel-ent extensions"}

    result = shell.privileged(
        "php-install",
        helper_args=[php_version],
        fallback=[
            "apt-get",
            "install",
            "-y",
            f"lsphp{lsphp_ver}",
            f"lsphp{lsphp_ver}-common",
            f"lsphp{lsphp_ver}-mysql",
            f"lsphp{lsphp_ver}-sqlite3",
            f"lsphp{lsphp_ver}-curl",
            f"lsphp{lsphp_ver}-opcache",
            f"lsphp{lsphp_ver}-intl",
            f"lsphp{lsphp_ver}-redis",
            f"lsphp{lsphp_ver}-imagick",
            # NOTE: gd, xml, mbstring, zip, bcmath are bundled in the base lsphp package
        ],
    )
    return {"status": "ensured" if already_installed else "installed", "version": php_version, "output": result.stdout}


# ---------------------------------------------------------------------------
# PHP / LSPHP auto-tuner
# ---------------------------------------------------------------------------
# Hardware detection helpers imported from mariadb (shared with MariaDB tuner)
from app.services.mariadb import _detect_ram_mb, _detect_cpu_cores, _detect_is_ssd

# (max_ram_mb, memory_limit, opcache_mem_mb, opcache_files, interned_strings_mb,
#  lsapi_children, lsapi_max_idle, lsapi_max_idle_children, lsapi_max_process_time)
_PHP_TIERS = [
    # Tiny VPS (≤512 MB)
    (512,  "1024M",   32,  2000,   8,   5,  60,   3,  300),
    # Small VPS (≤1 GB)
    (1024, "1024M",   64,  4000,  16,  10,  60,   5,  300),
    # Medium VPS (≤2 GB)
    (2048, "1024M",  128,  8000,  32,  20, 120,  10,  600),
    # Large VPS (≤4 GB)
    (4096, "1024M",  256, 12000,  64,  40, 120,  20,  600),
    # XLarge VPS (≤8 GB)
    (8192, "2048M",  512, 16000, 128,  60, 180,  30,  900),
    # XXLarge VPS (>8 GB)
    (999999, "4096M", 512, 20000, 128, 80, 180,  40,  900),
]


def recommend_php_config() -> dict:
    """Compute recommended PHP/LSPHP settings from current hardware."""
    ram_mb = _detect_ram_mb()
    cores = _detect_cpu_cores()
    is_ssd = _detect_is_ssd()

    # Pick tier
    (memory_limit, opcache_mem, opcache_files, interned_buf,
     lsapi_children, lsapi_idle, lsapi_idle_children, lsapi_max_proc) = (
        "1024M", 64, 4000, 16, 10, 60, 5, 300
    )
    for tier in _PHP_TIERS:
        if ram_mb <= tier[0]:
            memory_limit = tier[1]
            opcache_mem = tier[2]
            opcache_files = tier[3]
            interned_buf = tier[4]
            lsapi_children = tier[5]
            lsapi_idle = tier[6]
            lsapi_idle_children = tier[7]
            lsapi_max_proc = tier[8]
            break

    # Scale LSAPI children by CPU cores (min from tier, max from cores)
    lsapi_children = max(lsapi_children, cores * 2)

    # Upload limits scale with memory
    upload_mb = max(64, min(1024, ram_mb // 4))
    post_mb = upload_mb + max(32, upload_mb // 4)

    # OPcache staleness. revalidate_freq is how long a cached script may be
    # served without re-checking its mtime; 0 means check on every request.
    # It is NOT an on/off switch -- that is validate_timestamps, which must stay
    # on for shared hosting. With it off, an edited file is never noticed: the
    # old opcode stays until every LSAPI worker for that pool exits. On a quiet
    # site the workers idle out within LSAPI_MAX_IDLE and the edit appears to
    # take effect, so the breakage only shows up once a site has real traffic.
    revalidate = 0

    return {
        "ram_mb": ram_mb,
        "cores": cores,
        "is_ssd": is_ssd,
        # PHP core
        "memory_limit": memory_limit,
        "max_execution_time": 300,
        "max_input_time": 600,
        "max_input_vars": 10000,
        "upload_max_filesize": f"{upload_mb}M",
        "post_max_size": f"{post_mb}M",
        "display_errors": "Off",
        # OPcache
        "opcache_enable": 1,
        "opcache_memory_consumption": opcache_mem,
        "opcache_interned_strings_buffer": interned_buf,
        "opcache_max_accelerated_files": opcache_files,
        "opcache_revalidate_freq": revalidate,
        "opcache_validate_timestamps": 1,
        "opcache_save_comments": 1,
        "opcache_jit": 1255,
        "opcache_jit_buffer_size": max(16, min(256, ram_mb // 16)),
        # LSAPI process manager
        "lsapi_children": lsapi_children,
        "lsapi_max_idle": lsapi_idle,
        "lsapi_max_idle_children": lsapi_idle_children,
        "lsapi_max_process_time": lsapi_max_proc,
    }


def render_php_ini(cfg: dict | None = None) -> str:
    """Render a PHP .ini snippet from recommended settings."""
    if cfg is None:
        cfg = recommend_php_config()
    lines = [
        "; -------------------------------------------------------",
        "; OPanel Enterprise auto-tuned PHP/LSPHP settings",
        "; Generated by opanel-ent — do not edit manually.",
        f"; Detected: {cfg['ram_mb']} MB RAM, {cfg['cores']} CPU cores",
        f";          {'SSD' if cfg['is_ssd'] else 'HDD'} storage",
        "; -------------------------------------------------------",
        "",
        "; Core",
        f"memory_limit          = {cfg['memory_limit']}",
        f"max_execution_time    = {cfg['max_execution_time']}",
        f"max_input_time        = {cfg['max_input_time']}",
        f"max_input_vars        = {cfg['max_input_vars']}",
        "max_file_uploads      = 100",
        f"upload_max_filesize   = {cfg['upload_max_filesize']}",
        f"post_max_size         = {cfg['post_max_size']}",
        f"display_errors        = {cfg['display_errors']}",
        "",
        "; OPcache",
        f"opcache.enable        = {cfg['opcache_enable']}",
        f"opcache.enable_cli    = {cfg['opcache_enable']}",
        f"opcache.memory_consumption      = {cfg['opcache_memory_consumption']}",
        f"opcache.interned_strings_buffer = {cfg['opcache_interned_strings_buffer']}",
        f"opcache.max_accelerated_files    = {cfg['opcache_max_accelerated_files']}",
        f"opcache.revalidate_freq  = {cfg['opcache_revalidate_freq']}",
        f"opcache.validate_timestamps = {cfg['opcache_validate_timestamps']}",
        f"opcache.save_comments   = {cfg['opcache_save_comments']}",
        f"opcache.jit              = {cfg['opcache_jit']}",
        f"opcache.jit_buffer_size  = {cfg['opcache_jit_buffer_size']}M",
        "",
        "; LSAPI process manager",
        f"lsapi_children             = {cfg['lsapi_children']}",
        f"lsapi_max_idle             = {cfg['lsapi_max_idle']}",
        f"lsapi_max_idle_children    = {cfg['lsapi_max_idle_children']}",
        f"lsapi_max_process_time     = {cfg['lsapi_max_process_time']}",
        "",
    ]
    return "\n".join(lines)


def current_opcache_enabled(php_version: str) -> bool:
    """Read the OPcache switch currently written for *php_version*."""
    try:
        return bool(read_php_ini(php_version)["opcache_enable"])
    except (ValueError, OSError, KeyError):
        return True


def apply_php_tuning(php_version: str | None = None) -> dict:
    """Write auto-tuned PHP config and restart OLS.

    If *php_version* is None, applies to ALL installed LSPHP versions.
    Returns the recommendation dict with an extra ``applied_to`` list.
    """
    cfg = recommend_php_config()

    targets: list[str] = []
    versions = [php_version] if php_version else list_installed_php()

    for ver in versions:
        # The tuner recommends OPcache on, but a version the operator switched
        # off must stay off: otherwise applying the recommendation silently
        # flips the switch back behind their back.
        ver_cfg = dict(cfg)
        ver_cfg["opcache_enable"] = 1 if current_opcache_enabled(ver) else 0
        content = render_php_ini(ver_cfg)
        target = _lsphp_opanel_ini_path(ver)
        if settings.command_dry_run:
            targets.append(str(target))
            continue
        shell.privileged(
            "php-config-write",
            helper_args=[ver],
            input=content,
            fallback=[
                "bash",
                "-lc",
                "mkdir -p /usr/local/lsws/lsphp$2/etc/php/$1/mods-available && "
                "cat > /usr/local/lsws/lsphp$2/etc/php/$1/mods-available/99-opanel-ent.ini && "
                "(systemctl restart lshttpd.service || /usr/local/lsws/bin/lswsctrl restart)",
                "opanel-ent-php-tuning-write",
                ver,
                _lsphp_version(ver),
            ],
        )
        targets.append(str(target))

    cfg["restart_returncode"] = 0

    cfg["applied_to"] = targets
    return cfg


def read_php_tuning(php_version: str) -> dict | None:
    """Read current OPanel Enterprise-generated PHP config + recommendation."""
    target = _lsphp_opanel_ini_path(php_version)
    if not target.exists():
        target = _legacy_lsphp_opanel_ini_path(php_version)
    if not target.exists():
        return None
    return {
        "php_version": php_version,
        "ini_path": str(target),
        "content": target.read_text(encoding="utf-8"),
        "recommendation": recommend_php_config(),
    }
