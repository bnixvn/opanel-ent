from datetime import datetime
from pathlib import Path
import secrets
import string
import subprocess
from typing import Dict

from app.core.config import settings
from app.services.shell import shell


IDENTIFIER_CHARS = set(string.ascii_lowercase + string.digits + "_")


def random_password(length: int = 24) -> str:
    alphabet = string.ascii_letters + string.digits + "!@#%^*_+-"
    return "".join(secrets.choice(alphabet) for _ in range(length))


def safe_db_identifier(domain: str, prefix: str) -> str:
    clean = "".join(ch if ch.isalnum() else "_" for ch in domain.lower())[:38]
    return f"{prefix}_{clean}"[:63]


# Names the panel must never create, drop or re-password. The panel's own SQL
# account holds GRANT ALL ON *.* WITH GRANT OPTION, so without this an ordinary
# panel user could ask for db_user="root" and take the server's root account.
RESERVED_DB_IDENTIFIERS = frozenset({
    "root",
    "mysql",
    # The panel's own account. The dash keeps it out of the identifier
    # charset already; the other spellings are the ones a customer could
    # actually submit.
    "opanel-ent",
    "opanelent",
    "opanel_ent",
    "opanel",
    "bpanel",
    "sys",
    "information_schema",
    "performance_schema",
    "debian-sys-maint",
    "mariadb",
})


def _validate_identifier(value: str) -> str:
    if not value or len(value) > 64 or any(ch not in IDENTIFIER_CHARS for ch in value):
        raise ValueError("Invalid database identifier")
    if value.lower() in RESERVED_DB_IDENTIFIERS:
        raise ValueError(f"'{value}' is reserved and cannot be used")
    return value


def _quote_sql_string(value: str) -> str:
    return "'" + value.replace("\\", "\\\\").replace("'", "''") + "'"


def _quote_identifier(value: str) -> str:
    return f"`{_validate_identifier(value)}`"


def _mysql_args(extra: list = None) -> list:
    args = ["mysql"]
    default_files = [
        Path.home() / ".my.cnf",
        Path("/opt/opanel-ent/.my.cnf"),
    ]
    defaults_file = next((path for path in default_files if path.exists()), None)
    if defaults_file:
        args.append(f"--defaults-file={defaults_file}")
    if extra:
        args.extend(extra)
    return args


def _run_sql(sql: str, *, check: bool = True):
    """Pipe SQL through stdin so secrets never appear in argv/ps output."""
    return shell.run(_mysql_args(), check=check, input=sql, sensitive=True)


def create_database(seed: str, prefix: str = "wp", db_name: str | None = None, if_not_exists: bool = True) -> Dict[str, str]:
    db_name = _validate_identifier(db_name or safe_db_identifier(seed, prefix))
    db_user = _validate_identifier(safe_db_identifier(db_name, "u"))
    db_password = random_password()
    create_clause = "CREATE DATABASE IF NOT EXISTS" if if_not_exists else "CREATE DATABASE"
    sql = (
        f"{create_clause} {_quote_identifier(db_name)} CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci;\n"
        f"CREATE USER IF NOT EXISTS {_quote_sql_string(db_user)}@'localhost' IDENTIFIED BY {_quote_sql_string(db_password)};\n"
        f"ALTER USER {_quote_sql_string(db_user)}@'localhost' IDENTIFIED BY {_quote_sql_string(db_password)};\n"
        f"GRANT ALL PRIVILEGES ON {_quote_identifier(db_name)}.* TO {_quote_sql_string(db_user)}@'localhost';\n"
        "FLUSH PRIVILEGES;\n"
    )
    _run_sql(sql)
    return {"db_name": db_name, "db_user": db_user, "db_password": db_password}


def create_database_credentials(
    db_name: str,
    db_user: str,
    db_password: str,
    *,
    allow_existing: bool = False,
) -> Dict[str, str]:
    """Create a database and its user.

    By default a name already in use is an error. `allow_existing=True` restores
    the older re-password-on-collision behaviour and belongs only to the restore
    and import flows, which recreate accounts they already own; on the
    user-facing create path it let anyone re-password an existing SQL account.
    """
    db_name = _validate_identifier(db_name)
    db_user = _validate_identifier(db_user)
    if allow_existing:
        sql = (
            f"CREATE DATABASE IF NOT EXISTS {_quote_identifier(db_name)} CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci;\n"
            f"CREATE USER IF NOT EXISTS {_quote_sql_string(db_user)}@'localhost' IDENTIFIED BY {_quote_sql_string(db_password)};\n"
            f"ALTER USER {_quote_sql_string(db_user)}@'localhost' IDENTIFIED BY {_quote_sql_string(db_password)};\n"
        )
    else:
        sql = (
            f"CREATE DATABASE {_quote_identifier(db_name)} CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci;\n"
            f"CREATE USER {_quote_sql_string(db_user)}@'localhost' IDENTIFIED BY {_quote_sql_string(db_password)};\n"
        )
    sql += (
        f"GRANT ALL PRIVILEGES ON {_quote_identifier(db_name)}.* TO {_quote_sql_string(db_user)}@'localhost';\n"
        "FLUSH PRIVILEGES;\n"
    )
    _run_sql(sql)
    return {"db_name": db_name, "db_user": db_user, "db_password": db_password}


def identifier_in_use(db_name: str, db_user: str) -> str:
    """Say whether the name or user already exists in MariaDB itself.

    The panel's own table is not enough: a database created outside the panel,
    or a system account, is invisible there.
    """
    db_name = _validate_identifier(db_name)
    db_user = _validate_identifier(db_user)
    sql = (
        "SELECT 'db' FROM information_schema.SCHEMATA WHERE SCHEMA_NAME = "
        f"{_quote_sql_string(db_name)} UNION ALL SELECT 'user' FROM mysql.user "
        f"WHERE user = {_quote_sql_string(db_user)} AND host = 'localhost';"
    )
    result = shell.run(
        [*_mysql_args(), "-N", "-B"], check=False, input=sql, sensitive=True
    )
    if getattr(result, "returncode", 1) != 0:
        return ""
    found = {line.strip() for line in (result.stdout or "").splitlines()}
    if "db" in found:
        return "Database name already exists on this server"
    if "user" in found:
        return "Database user already exists on this server"
    return ""


def drop_database(db_name: str, db_user: str):
    sql = (
        f"DROP DATABASE IF EXISTS {_quote_identifier(db_name)};\n"
        f"DROP USER IF EXISTS {_quote_sql_string(_validate_identifier(db_user))}@'localhost';\n"
        "FLUSH PRIVILEGES;\n"
    )
    return _run_sql(sql)


def change_database_password(db_user: str, db_password: str):
    sql = (
        f"ALTER USER {_quote_sql_string(_validate_identifier(db_user))}@'localhost' "
        f"IDENTIFIED BY {_quote_sql_string(db_password)};\n"
        "FLUSH PRIVILEGES;\n"
    )
    return _run_sql(sql)


def export_database(db_name: str, output_file: str):
    args = ["mysqldump"]
    default_files = [
        Path.home() / ".my.cnf",
        Path("/opt/opanel-ent/.my.cnf"),
    ]
    defaults_file = next((path for path in default_files if path.exists()), None)
    if defaults_file:
        args.append(f"--defaults-file={defaults_file}")
    args.extend([_validate_identifier(db_name), "--result-file", output_file])
    return shell.run(args, sensitive=True)


def import_database(db_name: str, input_file: str):
    safe_name = _validate_identifier(db_name)
    sql_path = Path(input_file).resolve()
    if not sql_path.exists() or not sql_path.is_file():
        raise FileNotFoundError("SQL file not found")
    if settings.command_dry_run:
        return shell.run(["mysql", safe_name], sensitive=True)
    args = _mysql_args([safe_name])
    with sql_path.open("rb") as source:
        completed = subprocess.run(args, stdin=source, capture_output=True, check=False)
    if completed.returncode != 0:
        stderr = completed.stderr.decode("utf-8", errors="replace")
        raise RuntimeError(f"Database import failed: {stderr.strip()}")
    return completed


def dump_database_file(db_name: str, output_dir: Path) -> Path:
    safe_name = _validate_identifier(db_name)
    output_dir.mkdir(parents=True, exist_ok=True)
    target = output_dir / f"{safe_name}-{datetime.utcnow().strftime('%Y%m%d%H%M%S')}.sql"
    export_database(safe_name, str(target))
    if settings.command_dry_run and not target.exists():
        target.write_text(f"-- DRY RUN database dump for {safe_name}\n", encoding="utf-8")
    return target


# ---------------------------------------------------------------------------
# MariaDB auto-tuner
# ---------------------------------------------------------------------------
MARIADB_CONF_DIR = Path("/etc/mysql/mariadb.conf.d")
OPANEL_ENT_CONF_FILE = MARIADB_CONF_DIR / "99-opanel-ent.cnf"

# innodb_buffer_pool_size as fraction of total RAM by tier
_TIERS = [
    # (max_ram_mb, pool_fraction, max_connections, log_file_mb)
    (512,   0.25,  30,  48),
    (1024,  0.30,  50,  64),
    (2048,  0.40, 100, 128),
    (4096,  0.50, 150, 256),
    (8192,  0.60, 250, 512),
    (16384, 0.65, 400, 512),
    (32768, 0.70, 600, 1024),
    (65536, 0.75, 800, 2048),
]


def _detect_ram_mb() -> int:
    """Return total system RAM in MiB from /proc/meminfo."""
    try:
        text = Path("/proc/meminfo").read_text(encoding="utf-8")
        for line in text.splitlines():
            if line.startswith("MemTotal:"):
                return int(line.split()[1]) // 1024
    except Exception:
        pass
    return 1024  # safe fallback


def _detect_cpu_cores() -> int:
    """Return number of logical CPU cores."""
    try:
        import os
        return os.cpu_count() or 1
    except Exception:
        return 1


def _detect_is_ssd() -> bool:
    """Heuristic: check rotational flag for root disk."""
    try:
        import glob
        for dev in glob.glob("/sys/block/*/queue/rotational"):
            val = Path(dev).read_text().strip()
            if val == "0":
                return True
    except Exception:
        pass
    return False  # assume HDD if unknown


def recommend_mariadb_config() -> dict:
    """Compute recommended MariaDB settings from current hardware."""
    ram_mb = _detect_ram_mb()
    cores = _detect_cpu_cores()
    is_ssd = _detect_is_ssd()

    # Pick tier
    pool_frac, max_conn, log_mb = 0.25, 30, 48
    for tier_max, frac, conn, log in _TIERS:
        if ram_mb <= tier_max:
            pool_frac, max_conn, log_mb = frac, conn, log
            break
    else:
        pool_frac, max_conn, log_mb = 0.75, 800, 2048

    pool_mb = max(64, int(ram_mb * pool_frac))
    # Round to nearest 64 MB for cleaner values
    pool_mb = (pool_mb // 64) * 64

    log_buffer_mb = min(64, max(8, pool_mb // 16))
    sort_buffer = "2M" if ram_mb >= 4096 else "1M"
    read_rnd = "1M" if ram_mb >= 2048 else "512K"
    io_capacity = 2000 if is_ssd else 200
    io_capacity_max = io_capacity * 2
    flush_method = "O_DIRECT" if is_ssd else "fsync"
    thread_cache = min(128, max(8, cores * 8))
    tmp_table_mb = max(16, min(256, ram_mb // 16))

    return {
        "ram_mb": ram_mb,
        "cores": cores,
        "is_ssd": is_ssd,
        "innodb_buffer_pool_size": f"{pool_mb}M",
        "innodb_log_file_size": f"{log_mb}M",
        "innodb_log_buffer_size": f"{log_buffer_mb}M",
        "innodb_flush_log_at_trx_commit": 2,
        "innodb_flush_method": flush_method,
        "innodb_io_capacity": io_capacity,
        "innodb_io_capacity_max": io_capacity_max,
        "innodb_file_per_table": "ON",
        "innodb_open_files": 2000,
        "max_connections": max_conn,
        "thread_cache_size": thread_cache,
        "table_open_cache": 2000,
        "tmp_table_size": f"{tmp_table_mb}M",
        "max_heap_table_size": f"{tmp_table_mb}M",
        "sort_buffer_size": sort_buffer,
        "read_rnd_buffer_size": read_rnd,
        "join_buffer_size": "2M" if ram_mb >= 4096 else "1M",
        "key_buffer_size": "32M",
    }


def render_mariadb_conf(cfg: dict | None = None) -> str:
    """Render a MariaDB .cnf snippet from recommended settings."""
    if cfg is None:
        cfg = recommend_mariadb_config()
    lines = [
        "# -------------------------------------------------------",
        "# OPanel Enterprise auto-tuned MariaDB settings",
        "# Generated by opanel-ent — do not edit manually.",
        f"# Detected: {cfg['ram_mb']} MB RAM, {cfg['cores']} CPU cores",
        f"#          {'SSD' if cfg['is_ssd'] else 'HDD'} storage",
        "# -------------------------------------------------------",
        "[server]",
        "",
        "# InnoDB",
        "innodb_buffer_pool_size        = " + cfg["innodb_buffer_pool_size"],
        "innodb_log_file_size           = " + cfg["innodb_log_file_size"],
        "innodb_log_buffer_size         = " + cfg["innodb_log_buffer_size"],
        "innodb_flush_log_at_trx_commit = " + str(cfg["innodb_flush_log_at_trx_commit"]),
        "innodb_flush_method            = " + cfg["innodb_flush_method"],
        "innodb_io_capacity             = " + str(cfg["innodb_io_capacity"]),
        "innodb_io_capacity_max         = " + str(cfg["innodb_io_capacity_max"]),
        "innodb_file_per_table          = " + cfg["innodb_file_per_table"],
        "innodb_open_files              = " + str(cfg["innodb_open_files"]),
        "",
        "# Connections & threads",
        "max_connections                = " + str(cfg["max_connections"]),
        "thread_cache_size              = " + str(cfg["thread_cache_size"]),
        "",
        "# Table & buffer tuning",
        "table_open_cache               = " + str(cfg["table_open_cache"]),
        "tmp_table_size                 = " + cfg["tmp_table_size"],
        "max_heap_table_size            = " + cfg["max_heap_table_size"],
        "sort_buffer_size               = " + cfg["sort_buffer_size"],
        "read_rnd_buffer_size           = " + cfg["read_rnd_buffer_size"],
        "join_buffer_size               = " + cfg["join_buffer_size"],
        "key_buffer_size                = " + cfg["key_buffer_size"],
        "",
    ]
    return "\n".join(lines)


def apply_mariadb_tuning() -> dict:
    """Write auto-tuned config and restart MariaDB."""
    cfg = recommend_mariadb_config()
    content = render_mariadb_conf(cfg)
    OPANEL_ENT_CONF_FILE.parent.mkdir(parents=True, exist_ok=True)
    OPANEL_ENT_CONF_FILE.write_text(content, encoding="utf-8")
    # Restart MariaDB to apply
    result = shell.privileged("systemctl", helper_args=["mariadb", "restart"], check=False,
                              fallback=["systemctl", "restart", "mariadb"])
    cfg["restart_returncode"] = result.returncode
    cfg["conf_path"] = str(OPANEL_ENT_CONF_FILE)
    return cfg


def read_mariadb_tuning() -> dict | None:
    """Read current OPanel Enterprise-generated MariaDB config if it exists."""
    if not OPANEL_ENT_CONF_FILE.exists():
        return None
    return {
        "conf_path": str(OPANEL_ENT_CONF_FILE),
        "content": OPANEL_ENT_CONF_FILE.read_text(encoding="utf-8"),
        "recommendation": recommend_mariadb_config(),
    }
