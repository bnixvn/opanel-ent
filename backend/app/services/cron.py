import re
import shlex
from pathlib import Path
from urllib.parse import urlsplit

from app.models.entities import Website
from app.services import site_users
from app.services.shell import shell

CRON_FIELD_RE = r"(?:\*|\d{1,2})(?:[-/,](?:\*|\d{1,2}))*"
DOMAIN_GREP_RE = re.compile(r"^[a-z0-9.\-]{3,253}$")
ALLOWED_COMMAND_PREFIXES = (
    ("wp", "cron", "event", "run", "--due-now"),
    ("wp", "core", "update"),
    ("wp", "plugin", "update", "--all"),
    ("wp", "theme", "update", "--all"),
)
ALLOWED_PHP_OPTIONS = {"-q"}

# URL fetchers. Every accepted option is listed explicitly: an allowlist is the
# only safe shape here because both tools can be told to read their options
# from a file (`curl -K`, `wget -e`), which would turn any denylist into a
# bypass. Anything not named below is rejected.
FETCH_COMMANDS = ("wget", "curl")
FETCH_BOOL_FLAGS = {
    "wget": {"-q", "--quiet", "-nv", "--no-verbose", "--spider", "--no-check-certificate", "--no-cache"},
    "curl": {"-s", "--silent", "-S", "--show-error", "-L", "--location", "-k", "--insecure", "-f", "--fail", "-I", "--head"},
}
FETCH_VALUE_FLAGS = {
    "wget": {"-t", "--tries", "-T", "--timeout", "--connect-timeout", "--read-timeout", "-U", "--user-agent", "--header", "--post-data"},
    "curl": {"-m", "--max-time", "--connect-timeout", "-A", "--user-agent", "-H", "--header", "-d", "--data", "-X", "--request", "--retry"},
}
FETCH_OUTPUT_FLAGS = {"wget": {"-O", "--output-document"}, "curl": {"-o", "--output"}}
SAFE_OUTPUT_TARGETS = {"-", "/dev/null"}
FETCH_REDIRECT = ">/dev/null 2>&1"
# A trailing `>/dev/null 2>&1` (or any mix of redirects) pasted from a tutorial.
# shlex would turn these into literal argv entries, so they are stripped before
# validation and re-attached afterwards as a real shell redirect.
TRAILING_REDIRECT_RE = re.compile(r"\s*(?:[12]?>>?\s*\S+|[12]>&[12])(?:\s*(?:[12]?>>?\s*\S+|[12]>&[12]))*\s*$")


def _validate_schedule(schedule: str) -> str:
    fields = schedule.split()
    if len(fields) != 5 or not all(re.fullmatch(CRON_FIELD_RE, field) for field in fields):
        raise ValueError("Invalid cron schedule")
    return " ".join(fields)


def _validate_domain(domain: str) -> str:
    value = (domain or "").lower()
    if not DOMAIN_GREP_RE.fullmatch(value):
        raise ValueError("Invalid domain")
    return value


def _validate_php_command(args: list[str], document_root: str | Path) -> str:
    option_count = 1 if len(args) > 1 and args[1] in ALLOWED_PHP_OPTIONS else 0
    script_index = 1 + option_count
    if len(args) <= script_index or args[script_index].startswith("-"):
        raise ValueError("PHP cron commands must run a .php file; only the -q option is allowed")

    script = Path(args[script_index])
    if script.suffix.lower() != ".php":
        raise ValueError("PHP cron commands must run a .php file")

    safe_root = Path(document_root).resolve(strict=False)
    candidate = (script if script.is_absolute() else safe_root / script).resolve(strict=False)
    try:
        candidate.relative_to(safe_root)
    except ValueError as exc:
        raise ValueError("PHP cron scripts must be inside this website's public_html directory") from exc

    return " ".join(shlex.quote(arg) for arg in args)


def _validate_fetch_command(args: list[str], document_root: str | Path) -> str:
    program = args[0]
    bool_flags = FETCH_BOOL_FLAGS[program]
    value_flags = FETCH_VALUE_FLAGS[program]
    output_flags = FETCH_OUTPUT_FLAGS[program]
    safe_root = Path(document_root).resolve(strict=False)

    urls: list[str] = []
    index = 1
    while index < len(args):
        arg = args[index]
        if arg in bool_flags:
            index += 1
            continue
        if arg in value_flags or arg in output_flags:
            if index + 1 >= len(args):
                raise ValueError(f"{arg} needs a value (separate the option and its value with a space)")
            value = args[index + 1]
            if arg in output_flags and value not in SAFE_OUTPUT_TARGETS:
                candidate = (Path(value) if Path(value).is_absolute() else safe_root / value).resolve(strict=False)
                try:
                    candidate.relative_to(safe_root)
                except ValueError as exc:
                    raise ValueError(
                        "Cron downloads may only write to -, /dev/null, or a path inside this website's public_html"
                    ) from exc
            index += 2
            continue
        if arg.startswith("-"):
            raise ValueError(f"Option {arg} is not allowed in cron commands")
        urls.append(arg)
        index += 1

    if len(urls) != 1:
        raise ValueError("Provide exactly one http:// or https:// URL")
    parsed = urlsplit(urls[0])
    if parsed.scheme not in {"http", "https"} or not parsed.hostname:
        raise ValueError("Cron URLs must start with http:// or https://")

    quoted = " ".join(shlex.quote(arg) for arg in args)
    return f"{quoted} {FETCH_REDIRECT}"


def _validate_command(command: str, document_root: str | Path) -> str:
    args = shlex.split(TRAILING_REDIRECT_RE.sub("", command))
    if not args:
        raise ValueError("Cron command is required")
    if args[0] == "php":
        return _validate_php_command(args, document_root)
    if args[0] in FETCH_COMMANDS:
        return _validate_fetch_command(args, document_root)

    normalized = [arg for arg in args if arg != "--allow-root"]
    if not any(tuple(normalized[:len(prefix)]) == prefix for prefix in ALLOWED_COMMAND_PREFIXES):
        raise ValueError("Only safe WP-CLI maintenance commands or PHP scripts inside this website are allowed")
    return " ".join(shlex.quote(arg) for arg in [*normalized, "--allow-root"])


def cron_user_for_website(website: Website) -> str:
    if website.linux_user:
        return site_users.validate_linux_user(website.linux_user)
    try:
        parts = Path(website.root_path).resolve().relative_to(site_users.HOME_ROOT.resolve()).parts
    except ValueError:
        return "www-data"
    if parts:
        try:
            return site_users.validate_linux_user(parts[0])
        except ValueError:
            return "www-data"
    return "www-data"


def _parse_cron_line(index: int, line: str) -> dict:
    parts = line.split(maxsplit=5)
    schedule = " ".join(parts[:5]) if len(parts) >= 5 else ""
    command = parts[5] if len(parts) >= 6 else ""
    command = re.sub(r"\s+#\s*opanelent:[^\s]+\s*$", "", command, flags=re.IGNORECASE).strip()
    if command.startswith("cd ") and " && " in command:
        command = command.split(" && ", 1)[1].strip()
    # The command is shown back verbatim. Rewriting it for display made users
    # think their input had been swallowed, so they re-added the same job.
    command = command.replace(" --allow-root", "").strip()
    return {"index": index, "schedule": schedule, "command": command, "line": line}


def add_cron(website: Website, schedule: str, command: str) -> str:
    safe_schedule = _validate_schedule(schedule)
    document_root = site_users.document_root(website.root_path)
    safe_command = _validate_command(command, document_root)
    safe_domain = _validate_domain(website.domain)
    marker = f"# OPanelEnt:{safe_domain}"
    line = f"{safe_schedule} cd {shlex.quote(str(document_root))} && {safe_command} {marker}"
    cron_user = cron_user_for_website(website)
    if cron_user != "www-data":
        runtime_php_version = website.php_version if (website.app_type or "wordpress") in {"wordpress", "php"} else None
        site_users.ensure_site_runtime(website.domain, website.root_path, runtime_php_version, cron_user)
    existing = list_cron_all(cron_user)
    new_content = existing.rstrip() + ("\n" if existing.strip() else "") + line + "\n"
    shell.privileged(
        "cron-write",
        helper_args=[cron_user],
        input=new_content,
        fallback=["bash", "-lc", "crontab -"],
    )
    return line


def list_cron_all(cron_user: str = "www-data") -> str:
    if cron_user != "www-data":
        site_users.validate_linux_user(cron_user)
    result = shell.privileged(
        "cron-list",
        helper_args=[cron_user],
        check=False,
        fallback=["bash", "-lc", "crontab -l 2>/dev/null || true"],
    )
    return result.stdout or ""


def list_cron(domain: str, cron_user: str = "www-data") -> str:
    safe_domain = _validate_domain(domain)
    marker = f"opanelent:{safe_domain}"
    # add_cron writes the marker as "# OPanelEnt:<domain>", so match case-insensitively.
    # It has to stay one token with no space: the reader matches the whole
    # marker, and a display-style name would silently hide every job.
    # A case-sensitive test against the lowercase marker never matched, which left
    # every cron job invisible in the panel although it was written to the crontab.
    return "\n".join(line for line in list_cron_all(cron_user).splitlines() if marker in line.lower())


def list_cron_entries(domain: str, cron_user: str = "www-data") -> list[dict]:
    return [_parse_cron_line(index, line) for index, line in enumerate(list_cron(domain, cron_user).splitlines())]


def delete_cron(domain: str, index: int, cron_user: str = "www-data") -> str:
    safe_domain = _validate_domain(domain)
    matching = list_cron(safe_domain, cron_user).splitlines()
    if index < 0 or index >= len(matching):
        raise ValueError("Cron not found")
    target = matching[index]
    full = list_cron_all(cron_user).splitlines()
    new_lines = [line for line in full if line.strip() != target.strip()]
    new_content = "\n".join(new_lines) + ("\n" if new_lines else "")
    shell.privileged(
        "cron-write",
        helper_args=[cron_user],
        input=new_content,
        fallback=["bash", "-lc", "crontab -"],
    )
    return target
