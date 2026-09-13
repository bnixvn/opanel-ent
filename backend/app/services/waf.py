import json
import re
from datetime import datetime, timezone
from typing import Iterable
from urllib.parse import unquote

from app.models.entities import Website
from app.services.shell import CommandResult, shell


DOMAIN_RE = re.compile(r"^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?(?:\.[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?)+$")
MAX_CUSTOM_BYTES = 64 * 1024
MAX_SITE_RULE_BYTES = 160 * 1024
MAX_ACCESS_LOG_LINES = 20000
MAX_ACCESS_LOG_LIMIT = 500

ACCESS_LOG_RE = re.compile(
    r'^(?P<ip>\S+) \S+ \S+ \[(?P<time>[^\]]+)\] '
    r'"(?P<request>[^"]*)" (?P<status>\d{3}) (?P<size>\S+) '
    r'"(?P<referer>[^"]*)" "(?P<user_agent>[^"]*)"(?: (?P<extra>.*))?$'
)
MODSEC_MSG_RE = re.compile(r'(?:\[msg\s+"(?P<msg1>[^"]+)"\]|msg:\'(?P<msg2>[^\']+)\')')
MODSEC_ID_RE = re.compile(r'(?:\[id\s+"(?P<id1>\d+)"\]|id:(?P<id2>\d+))')
BLOCK_STATUSES = {401, 403, 406, 429}

DEFAULT_RULES = [
    {
        "id": "php-sensitive-files",
        "category": "PHP",
        "title": "PHP sensitive files",
        "description": "Blocks direct probes for PHP app secrets, Composer metadata, git data, and phpinfo files.",
        "rules": """SecRule REQUEST_URI "@rx (?i)(?:/\\.env(?:\\.|$)|/\\.user\\.ini(?:\\.|$)|/\\.git/|/composer\\.(?:json|lock)(?:$|[?])|/(?:phpinfo|info)\\.php(?:$|[?])|/(?:config|database|db)\\.php\\.(?:bak|old|save|txt)(?:$|[?]))" "id:1001301,phase:1,deny,status:403,log,msg:'opanel-ent blocked PHP sensitive file probe'"
""",
    },
    {
        "id": "php-path-traversal",
        "category": "PHP",
        "title": "Path traversal",
        "description": "Blocks ../ and encoded traversal, plus direct reads of /etc/passwd and /proc/self/environ, in the URL and in GET or POST arguments.",
        "rules": r"""SecRule REQUEST_URI|ARGS "@rx (?i)(?:\.\./|\.\.\\|\.\.%2f|%2e%2e%2f|%252e%252e%252f|/etc/(?:passwd|shadow|hosts)\b|/proc/self/(?:environ|cmdline)\b)" "id:1001302,phase:2,t:none,t:urlDecodeUni,t:removeComments,deny,status:403,log,msg:'opanel-ent blocked PHP path traversal'"
""",
    },
    {
        "id": "php-runtime-probes",
        "category": "PHP",
        "title": "PHP runtime probes",
        "description": "Blocks direct probes for common PHP webshell names and old PHPUnit RCE paths.",
        "rules": """SecRule REQUEST_URI "@rx (?i)(?:/(?:c99|r57|shell|cmd|wso)\\.php(?:$|[?])|/vendor/phpunit/phpunit/src/Util/PHP/eval-stdin\\.php(?:$|[?]))" "id:1001303,phase:1,deny,status:403,log,msg:'opanel-ent blocked PHP runtime probe'"
""",
    },
    {
        "id": "laravel-sensitive-files",
        "category": "Laravel",
        "title": "Laravel sensitive files",
        "description": "Blocks probes for Laravel environment files, logs, artisan, and cached PHP config.",
        "rules": """SecRule REQUEST_URI "@rx (?i)(?:/\\.env(?:\\.|$)|/artisan(?:$|[?])|/server\\.php(?:$|[?])|/storage/logs/[^?]*\\.log(?:$|[?])|/bootstrap/cache/[^?]*\\.php(?:$|[?]))" "id:1001201,phase:1,deny,status:403,log,msg:'opanel-ent blocked Laravel sensitive path'"
""",
    },
    {
        "id": "laravel-ignition-rce",
        "category": "Laravel",
        "title": "Laravel Ignition RCE probes",
        "description": "Blocks direct probes for the old Laravel Ignition execute-solution endpoint.",
        "rules": """SecRule REQUEST_URI "@rx (?i)(?:/_ignition/execute-solution(?:$|[?]))" "id:1001202,phase:1,deny,status:403,log,msg:'opanel-ent blocked Laravel Ignition RCE probe'"
""",
    },
    {
        "id": "wordpress-sensitive-files",
        "category": "WordPress",
        "title": "WordPress sensitive files",
        "description": "Blocks wp-config probes, uploads PHP execution probes, and internal WordPress PHP paths.",
        "rules": """SecRule REQUEST_URI "@rx (?i)(?:/wp-config\\.php(?:\\.|$|[?])|/wp-content/(?:uploads|cache|upgrade)/[^?]*\\.php(?:$|[?])|/wp-admin/includes/[^?]*\\.php(?:$|[?])|/wp-includes/[^?]*\\.php(?:$|[?]))" "id:1001101,phase:1,deny,status:403,log,msg:'opanel-ent blocked WordPress sensitive path'"
""",
    },
    {
        "id": "wordpress-xmlrpc-author-scan",
        "category": "WordPress",
        "title": "WordPress XML-RPC and author scans",
        "description": "Blocks XML-RPC access and ?author= enumeration scans.",
        "rules": """SecRule REQUEST_URI "@rx (?i)(?:/xmlrpc\\.php(?:$|[?]))" "id:1001102,phase:1,deny,status:403,log,msg:'opanel-ent blocked WordPress XML-RPC access'"
SecRule ARGS:author "@rx ^[0-9]+$" "id:1001103,phase:2,deny,status:403,log,msg:'opanel-ent blocked WordPress author enumeration'"
""",
    },
    {
        "id": "wordpress-install-upgrade",
        "category": "WordPress",
        "title": "WordPress installer probes",
        "description": "Blocks direct access to WordPress installation scripts after deployment.",
        "rules": """SecRule REQUEST_URI "@rx (?i)(?:/wp-admin/install\\.php(?:$|[?])|/wp-admin/setup-config\\.php(?:$|[?]))" "id:1001104,phase:1,deny,status:403,log,msg:'opanel-ent blocked WordPress installer probe'"
""",
    },
    {
        "id": "wordpress-wp2shell",
        "category": "WordPress",
        "title": "WordPress wp2shell probes",
        "description": "Blocks the wp2shell batch API paths used by the published exploit chain.",
        "rules": """SecRule REQUEST_URI "@contains /wp-json/batch/v1" "id:1000001,phase:2,deny,status:403,msg:'Block wp2shell Path'"
SecRule ARGS:rest_route "@contains /batch/v1" "id:1000002,phase:2,deny,status:403,msg:'Block wp2shell Query'"
""",
    },
    {
        "id": "generic-sensitive-files",
        "category": "Server",
        "title": "Credential and backup files",
        "description": "Blocks probes for SSH and cloud credentials, version-control data, database dumps, and editor backup copies.",
        "rules": r"""SecRule REQUEST_URI "@rx (?i)(?:/\.(?:ssh|aws|svn|hg|idea|vscode)/|/\.(?:npmrc|netrc|htpasswd|DS_Store)(?:$|[?/])|/id_rsa(?:\.pub)?(?:$|[?])|/[^?]*\.(?:sql|sqlite|sqlite3|rdb)(?:\.(?:gz|zip|bz2|tar))?(?:$|[?])|/[^?]*\.(?:bak|old|orig|save|swp|swo)(?:$|[?])|/[^?]*~(?:$|[?]))" "id:1001401,phase:1,deny,status:403,log,msg:'opanel-ent blocked credential or backup file probe'"
""",
    },
    {
        "id": "sql-injection",
        "category": "Injection",
        "title": "SQL injection",
        "description": "Blocks SQL injection in the URL and in GET or POST arguments. Comments and padding are stripped first, so UN/**/ION and UNION(SELECT(1)) are caught too.",
        "rules": r"""SecRule REQUEST_URI|ARGS "@rx (?i)(?:\bunion\b[\s(),]*(?:all[\s(),]*)?\bselect\b|\bselect\b.{0,120}?\bfrom\b[\s(]*information_schema\b|\b(?:sleep|benchmark|load_file|updatexml|extractvalue|pg_sleep)\b[\s(]*\(|\binto\s+(?:out|dump)file\b|\b(?:or|and)\s+(['\"]?)(\w+)\1\s*(?:=|<>|!=|\blike\b)\s*\1?\2\1?|\b(?:or|and)\s+\d+\s*(?:=|<>|!=|<|>)\s*\d+|;[\s(]*(?:drop|truncate|alter)\s+(?:table|database)\b)" "id:1001501,phase:2,t:none,t:urlDecodeUni,t:removeComments,t:compressWhitespace,deny,status:403,log,msg:'opanel-ent blocked SQL injection attempt'"
""",
    },
    {
        "id": "xss",
        "enabled_default": False,
        "category": "Injection",
        "title": "Cross-site scripting",
        "description": "Blocks script tags, javascript: URLs and inline event handlers in the URL and query string.",
        "rules": r"""SecRule REQUEST_URI|ARGS_GET "@rx (?i)(?:<\s*script[\s>]|<\s*/\s*script\s*>|javascript\s*:|vbscript\s*:|\bon(?:error|load|click|mouseover|focus|submit)\s*=|<\s*iframe[\s>]|<\s*svg[\s>]|document\s*\.\s*cookie)" "id:1001601,phase:2,t:none,t:urlDecodeUni,t:removeComments,deny,status:403,log,msg:'opanel-ent blocked cross-site scripting attempt'"
""",
    },
    {
        "id": "command-injection",
        "category": "Injection",
        "title": "Command injection and PHP wrappers",
        "description": "Blocks shell metacharacters followed by a command, php:// data:// file:// stream wrappers, and ${jndi:} lookups, in the URL and in GET or POST arguments.",
        "rules": r"""SecRule REQUEST_URI|ARGS "@rx (?i)(?:\b(?:php|data|expect|phar|zip|glob|file|gopher|dict)://|[;|`]\s*(?:cat|ls|id|whoami|uname|curl|wget|nc|bash|sh|python|perl|chmod)\b|&&\s*\w|\|\|\s*\w|[\n\r]\s*(?:cat|ls|id|whoami|uname|curl|wget|nc|bash|sh)\b|\$\(\s*\w|\|\s*(?:sh|bash)\b|\$\{\s*(?:jndi|env|sys|lower|upper|date)\s*:)" "id:1001701,phase:2,t:none,t:urlDecodeUni,t:removeComments,deny,status:403,log,msg:'opanel-ent blocked command injection attempt'"
""",
    },
    {
        "id": "ssrf",
        "category": "Injection",
        "title": "Server-side request forgery",
        "description": "Blocks URLs pointing at cloud metadata services, loopback and private networks, including the userinfo@ and decimal-IP forms used to disguise them.",
        "rules": r"""SecRule REQUEST_URI|ARGS "@rx (?i)(?:https?:)?//(?:[^/@\s]*@)?(?:169\.254\.169\.254|169\.254\.170\.2|metadata\.google\.internal|metadata\.azure\.com|100\.100\.200\.200|localhost(?::\d+)?(?![\w.-])|127\.\d{1,3}\.\d{1,3}\.\d{1,3}|0\.0\.0\.0|\[::1?\]|10\.\d{1,3}\.\d{1,3}\.\d{1,3}|192\.168\.\d{1,3}\.\d{1,3}|172\.(?:1[6-9]|2\d|3[01])\.\d{1,3}\.\d{1,3}|0x[0-9a-f]{8}(?![0-9a-f])|\d{8,10}(?![\d.]))" "id:1001801,phase:2,t:none,t:urlDecodeUni,t:removeComments,deny,status:403,log,msg:'opanel-ent blocked server-side request forgery attempt'"
""",
    },
    {
        "id": "ssti",
        "category": "Injection",
        "title": "Template injection",
        "description": "Blocks template expressions that carry a probe or a known gadget -- {{7*7}}, __class__ traversal, and Java EL Runtime/ProcessBuilder chains. Plain {{ }} is left alone: it is ordinary front-end syntax.",
        # No t:removeComments here. It treats # as a comment and deletes the
        # rest of the value, and OGNL uses # as syntax: ${(#a=@java.lang.
        # Runtime@getRuntime().exec(...))} was erased before the rule saw it.
        "rules": r"""SecRule REQUEST_URI|ARGS "@rx (?i)(?:\{\{\s*\d+\s*[*+\-/]\s*\d+\s*\}\}|\{\{[^}]{0,60}?__(?:class|globals|subclasses|mro|init|builtins|import)__|\{\{\s*(?:config|self|request|session|settings|cycler|joiner|namespace|lipsum|url_for)\b|\{%\s*(?:import|include|extends)\b|<%=[^%]{0,60}?(?:system|exec|eval|IO\.popen|`)|\$\{[^}]{0,120}?(?:Runtime|ProcessBuilder|getClass|forName|javax\.script)|#\{[^}]{0,60}?(?:T\(|java\.lang))" "id:1001901,phase:2,t:none,t:urlDecodeUni,deny,status:403,log,msg:'opanel-ent blocked template injection attempt'"
""",
    },
]

# ---------------------------------------------------------------------------
# Bad bot blocking
# ---------------------------------------------------------------------------
BAD_BOT_RULE_ID = "1002001"
BAD_BOT_SETTINGS_KEY = "waf_bad_bots"
MAX_BOT_PATTERNS = 400
MAX_BOT_PATTERN_LEN = 120



def normalize_bot_patterns(raw) -> list[str]:
    """Accept a list or a newline/comma separated blob and return clean patterns.

    Entries are matched as literal, case-insensitive substrings of the
    User-Agent -- they are regex-escaped at render time. Treating them as
    literals keeps a mistyped entry from becoming a broken or catastrophically
    backtracking regex in every site's config.
    """
    if isinstance(raw, str):
        items = re.split(r"[\r\n,]+", raw)
    elif isinstance(raw, (list, tuple)):
        items = []
        for entry in raw:
            items.extend(re.split(r"[\r\n,]+", str(entry)))
    elif raw is None:
        items = []
    else:
        raise ValueError("Bot list must be a list or a string")

    cleaned: list[str] = []
    seen: set[str] = set()
    for item in items:
        pattern = item.strip()
        if not pattern or pattern.startswith("#"):
            continue
        if len(pattern) > MAX_BOT_PATTERN_LEN:
            raise ValueError(f"Bot pattern is too long (max {MAX_BOT_PATTERN_LEN}): {pattern[:40]}...")
        if '"' in pattern or "\\" in pattern:
            raise ValueError(f'Bot pattern cannot contain " or \\: {pattern}')
        if any(ord(ch) < 32 or ord(ch) == 127 for ch in pattern):
            raise ValueError("Bot pattern cannot contain control characters")
        key = pattern.lower()
        if key in seen:
            continue
        seen.add(key)
        cleaned.append(pattern)
    if len(cleaned) > MAX_BOT_PATTERNS:
        raise ValueError(f"Too many bot patterns (max {MAX_BOT_PATTERNS})")
    return cleaned


def global_bad_bots() -> list[str]:
    """The server-wide bot list, applied to every site that has bot blocking on."""
    from app.services import panel_settings

    try:
        return normalize_bot_patterns(panel_settings._read_raw().get(BAD_BOT_SETTINGS_KEY) or [])
    except (ValueError, OSError):
        return []


def set_global_bad_bots(patterns) -> list[str]:
    from app.services import panel_settings

    cleaned = normalize_bot_patterns(patterns)
    data = panel_settings._read_raw()
    data[BAD_BOT_SETTINGS_KEY] = cleaned
    panel_settings._write_raw(data)
    return cleaned


def website_bot_settings(website: Website) -> dict:
    return {
        "bot_blocking_enabled": bool(getattr(website, "waf_bot_enabled", True)),
        "bot_extra": normalize_bot_patterns(getattr(website, "waf_bot_extra", "") or ""),
    }


def render_bad_bot_rules(patterns: Iterable[str]) -> str:
    """One rule: deny when the User-Agent contains any listed pattern.

    Exactly what the admin listed is blocked, nothing more and nothing less --
    to stop blocking something, take it off the list.
    """
    bad = [re.escape(p) for p in normalize_bot_patterns(list(patterns))]
    if not bad:
        return ""
    return (
        f'SecRule REQUEST_HEADERS:User-Agent "@rx (?i)({"|".join(bad)})" '
        f'"id:{BAD_BOT_RULE_ID},phase:1,deny,status:403,log,'
        "msg:'opanel-ent blocked bad bot'\""
    )


LEGACY_RULE_ID_MAP = {
    "general-sensitive-files": "php-sensitive-files",
    "general-path-traversal": "php-path-traversal",
    "general-command-injection": "php-runtime-probes",
    "general-sqli": None,
    "general-xss": None,
}

RULE_REASON_BY_ID = {
    "1001101": "Block WordPress sensitive path",
    "1001102": "Block WordPress XML-RPC",
    "1001103": "Block WordPress author enumeration",
    "1001104": "Block WordPress installer probe",
    "1000001": "Block wp2shell Path",
    "1000002": "Block wp2shell Query",
    "1001201": "Block Laravel sensitive path",
    "1001202": "Block Laravel Ignition RCE probe",
    "1001301": "Block PHP sensitive file probe",
    "1001302": "Block PHP path traversal",
    "1001303": "Block PHP runtime probe",
}

PATH_REASON_RULES = [
    (re.compile(r"(?i)(?:^|/)xmlrpc\.php(?:$|[?])"), "Block WordPress XML-RPC"),
    (re.compile(r"(?i)(?:^|/)wp-config\.php(?:\.|$|[?])"), "Block WordPress sensitive path"),
    (re.compile(r"(?i)(?:^|/)wp-admin/(?:install|setup-config)\.php(?:$|[?])"), "Block WordPress installer probe"),
    (re.compile(r"(?i)(?:^|/)wp-json/batch/v1(?:$|[?])"), "Block wp2shell Path"),
    (re.compile(r"(?i)(?:[?&]rest_route=/batch/v1(?:$|&))"), "Block wp2shell Query"),
    (re.compile(r"(?i)(?:[?&]author=\d+(?:$|&))"), "Block WordPress author enumeration"),
    (re.compile(r"(?i)(?:^|/)\.env(?:\.|$|[?])|(?:^|/)\.user\.ini(?:\.|$)|(?:^|/)\.git/"), "Block PHP sensitive file probe"),
    (re.compile(r"(?i)(?:\.\./|\.\.\\|%2e%2e%2f|%252e%252e%252f)"), "Block PHP path traversal"),
    (re.compile(r"(?i)(?:^|/)(?:c99|r57|shell|cmd|wso)\.php(?:$|[?])|/vendor/phpunit/phpunit/src/Util/PHP/eval-stdin\.php"), "Block PHP runtime probe"),
    (re.compile(r"(?i)(?:^|/)artisan(?:$|[?])|(?:^|/)server\.php(?:$|[?])|/storage/logs/[^?]*\.log(?:$|[?])"), "Block Laravel sensitive path"),
    (re.compile(r"(?i)(?:^|/)_ignition/execute-solution(?:$|[?])"), "Block Laravel Ignition RCE probe"),
]


def _rule_ids() -> set[str]:
    return {rule["id"] for rule in DEFAULT_RULES}


def _validate_domain(domain: str) -> str:
    value = (domain or "").strip().lower()
    if not DOMAIN_RE.fullmatch(value):
        raise ValueError("Invalid domain")
    return value


def _validate_custom_rules(content: str) -> str:
    value = content or ""
    if "\x00" in value:
        raise ValueError("WAF rules cannot contain NUL bytes")
    if len(value.encode("utf-8")) > MAX_CUSTOM_BYTES:
        raise ValueError("WAF custom rules must be 64 KB or smaller")
    return value.replace("\r\n", "\n").strip()


def _validate_access_log_options(limit: int, offset: int, lines: int) -> tuple[int, int, int]:
    try:
        safe_limit = int(limit)
        safe_offset = int(offset)
        safe_lines = int(lines)
    except (TypeError, ValueError) as exc:
        raise ValueError("Access log options must be numbers") from exc
    safe_limit = max(1, min(MAX_ACCESS_LOG_LIMIT, safe_limit))
    safe_offset = max(0, safe_offset)
    safe_lines = max(1, min(MAX_ACCESS_LOG_LINES, safe_lines))
    return safe_limit, safe_offset, safe_lines


def _request_parts(request: str) -> tuple[str, str, str]:
    parts = (request or "").split()
    if len(parts) >= 3:
        return parts[0], parts[1], parts[2]
    if len(parts) == 2:
        return parts[0], parts[1], ""
    if len(parts) == 1:
        return "", parts[0], ""
    return "", "", ""


def _parse_access_time(value: str) -> tuple[str, str]:
    try:
        parsed = datetime.strptime(value, "%d/%b/%Y:%H:%M:%S %z")
    except ValueError:
        return "", value
    return parsed.astimezone(timezone.utc).isoformat(), parsed.strftime("%b %d, %I:%M:%S %p")


def _parse_log_payload(stdout: str) -> tuple[dict[str, str], str]:
    paths: dict[str, str] = {}
    content_lines = []
    for line in (stdout or "").splitlines():
        if line.startswith("opanel_ent_LOG_PATH="):
            _, _, rest = line.partition("=")
            domain, _, path = rest.partition("\t")
            if domain and path:
                paths[domain] = path
            continue
        content_lines.append(line)
    return paths, "\n".join(content_lines)


def _parse_modsec_reasons(log_text: str) -> dict[tuple[str, str], str]:
    reasons: dict[tuple[str, str], str] = {}
    for line in (log_text or "").splitlines():
        if "mod_security" not in line.lower() and "modsecurity" not in line.lower() and "[id " not in line and "id:" not in line:
            continue
        request_match = re.search(r'(?:REQUEST_URI|uri|URI|request)[:= ]+"?([^"\s]+)', line, re.IGNORECASE)
        ip_match = re.search(r'\[client\s+(?P<ip>[^\]:]+)', line)
        if not request_match:
            continue
        raw_path = request_match.group(1)
        rule_id_match = MODSEC_ID_RE.search(line)
        msg_match = MODSEC_MSG_RE.search(line)
        rule_id = ""
        if rule_id_match:
            rule_id = rule_id_match.group("id1") or rule_id_match.group("id2") or ""
        message = ""
        if msg_match:
            message = msg_match.group("msg1") or msg_match.group("msg2") or ""
        reason = RULE_REASON_BY_ID.get(rule_id) or message
        if not reason:
            continue
        key_ip = ip_match.group("ip") if ip_match else ""
        reasons[(key_ip, raw_path)] = reason
        reasons[("", raw_path)] = reason
    return reasons


def _infer_block_reason(path: str, status: int, modsec_reasons: dict[tuple[str, str], str], ip: str = "") -> str:
    if status not in BLOCK_STATUSES:
        return "Allowed"
    for candidate in (path, unquote(path or "")):
        reason = modsec_reasons.get((ip, candidate)) or modsec_reasons.get(("", candidate))
        if reason:
            return reason
        for pattern, label in PATH_REASON_RULES:
            if pattern.search(candidate or ""):
                return label
    if status == 429:
        return "Rate limited"
    return "Blocked"


def parse_access_log_line(line: str, domain: str, modsec_reasons: dict[tuple[str, str], str] | None = None) -> dict | None:
    match = ACCESS_LOG_RE.match(line.strip())
    if not match:
        return None
    method, path, protocol = _request_parts(match.group("request"))
    try:
        status = int(match.group("status"))
    except (TypeError, ValueError):
        status = 0
    iso_time, display_time = _parse_access_time(match.group("time"))
    modsec_reasons = modsec_reasons or {}
    verdict = "block" if status in BLOCK_STATUSES else "allow"
    return {
        "domain": domain,
        "site": domain,
        "verdict": verdict,
        "timestamp": iso_time,
        "time": display_time,
        "ip": match.group("ip"),
        "method": method,
        "path": path,
        "protocol": protocol,
        "status": status,
        "bytes": 0 if match.group("size") == "-" else match.group("size"),
        "duration_ms": 0,
        "reason": _infer_block_reason(path, status, modsec_reasons, match.group("ip")),
        "user_agent": match.group("user_agent") or "",
        "referer": "" if match.group("referer") == "-" else match.group("referer"),
    }


def _entry_matches(entry: dict, domain: str, verdict: str, query: str) -> bool:
    if domain and entry.get("domain") != domain:
        return False
    if verdict and entry.get("verdict") != verdict:
        return False
    if not query:
        return True
    haystack = " ".join(
        str(entry.get(key, ""))
        for key in ("domain", "verdict", "ip", "method", "path", "status", "reason", "user_agent", "referer")
    ).lower()
    return query.lower() in haystack


def _domains_for_log(website_domains: Iterable[str], domain: str = "") -> list[str]:
    allowed = []
    for item in website_domains:
        safe = _validate_domain(str(item))
        if safe not in allowed:
            allowed.append(safe)
    if domain:
        safe_domain = _validate_domain(domain)
        if safe_domain not in allowed:
            raise ValueError("Domain is not managed by opanel-ent")
        return [safe_domain]
    return allowed


# The rule ids that existed while selections were stored as a flat list of
# enabled ids. Anything outside this set post-dates that format, so a legacy
# list not mentioning it means "did not exist yet", never "turned off".
HISTORICAL_RULE_IDS = frozenset({
    "php-sensitive-files",
    "php-path-traversal",
    "php-runtime-probes",
    "laravel-sensitive-files",
    "laravel-ignition-rce",
    "wordpress-sensitive-files",
    "wordpress-xmlrpc-author-scan",
    "wordpress-install-upgrade",
    "wordpress-wp2shell",
})


def _clean_ids(items, valid: set[str]) -> set[str]:
    return {
        mapped
        for item in items or []
        for mapped in [LEGACY_RULE_ID_MAP.get(str(item), str(item))]
        if mapped in valid
    }


def selection_payload(enabled_rule_ids: Iterable[str]) -> dict:
    """What gets stored: which defaults were switched off, and which opt-in
    groups were switched on. Recording the deltas rather than the full list is
    what lets a rule group added later arrive at its own default."""
    valid = _rule_ids()
    selected = _clean_ids(enabled_rule_ids, valid)
    defaults = _default_enabled_ids()
    return {
        "disabled": sorted(defaults - selected),
        "enabled": sorted(selected - defaults),
    }


def _parse_enabled_rule_ids(value: str | None) -> set[str]:
    valid = _rule_ids()
    defaults = _default_enabled_ids()
    if not value:
        return set(defaults)
    try:
        raw = json.loads(value)
    except (TypeError, ValueError):
        return set(defaults)

    if isinstance(raw, dict):
        disabled = _clean_ids(raw.get("disabled"), valid)
        enabled = _clean_ids(raw.get("enabled"), valid)
        return (defaults - disabled) | enabled

    if isinstance(raw, list):
        # Legacy format. Only the groups that existed back then can have been
        # deselected; everything newer takes its own default.
        listed = _clean_ids(raw, valid)
        disabled = (HISTORICAL_RULE_IDS & valid) - listed
        enabled = listed - defaults
        return (defaults - disabled) | enabled

    return set(defaults)


def validate_enabled_rule_ids(rule_ids: Iterable[str]) -> list[str]:
    valid = _rule_ids()
    selected = []
    for rule_id in rule_ids:
        value = LEGACY_RULE_ID_MAP.get(str(rule_id), str(rule_id))
        if value is None:
            continue
        if value not in valid:
            raise ValueError(f"Unknown WAF rule: {value}")
        if value not in selected:
            selected.append(value)
    return selected


def _default_enabled_ids() -> set[str]:
    """Rule groups a site gets when its selection has never been saved."""
    return {rule["id"] for rule in DEFAULT_RULES if rule.get("enabled_default", True)}


def default_rule_definitions() -> list[dict]:
    return [
        {
            "id": rule["id"],
            "category": rule["category"],
            "title": rule["title"],
            "description": rule["description"],
            "enabled_default": bool(rule.get("enabled_default", True)),
        }
        for rule in DEFAULT_RULES
    ]


def site_rules_file(domain: str) -> str:
    safe_domain = _validate_domain(domain)
    return f"/usr/local/lsws/conf/opanel-ent/waf/sites/{safe_domain}.conf"


def render_site_rules(
    domain: str,
    enabled_rule_ids: Iterable[str],
    custom_rules: str = "",
    *,
    bot_blocking: bool = True,
    bot_extra: Iterable[str] = (),
    global_bots: Iterable[str] | None = None,
) -> str:
    safe_domain = _validate_domain(domain)
    enabled = set(validate_enabled_rule_ids(enabled_rule_ids))
    custom = _validate_custom_rules(custom_rules)
    chunks = [
        f"# OPanel Enterprise WAF rules for {safe_domain}",
        "Include /usr/local/lsws/conf/opanel-ent/waf/opanel-ent-base.conf",
        "",
        "# OPanel Enterprise selected default rules",
    ]
    for rule in DEFAULT_RULES:
        if rule["id"] not in enabled:
            continue
        chunks.append(f"# {rule['category']} - {rule['title']} ({rule['id']})")
        chunks.append(rule["rules"].strip())
        if rule.get("exceptions"):
            chunks.append(rule["exceptions"].strip())

    # Server-wide bot list plus whatever this site adds. An empty list renders
    # nothing at all, so the feature is inert until an admin fills it in.
    if bot_blocking:
        bots = list(global_bots if global_bots is not None else global_bad_bots())
        bots.extend(bot_extra or ())
        bot_block = render_bad_bot_rules(bots)
        if bot_block:
            chunks.extend(["", "# OPanel Enterprise bad bot blocking", bot_block])

    chunks.extend(["", "# OPanel Enterprise custom rules"])
    if custom:
        chunks.append(custom)
    content = "\n".join(chunks).strip() + "\n"
    if len(content.encode("utf-8")) > MAX_SITE_RULE_BYTES:
        raise ValueError("WAF site rules are too large")
    return content


def website_enabled_rule_ids(website: Website) -> set[str]:
    return _parse_enabled_rule_ids(getattr(website, "waf_default_rules", ""))


def website_custom_rules(website: Website) -> str:
    return _validate_custom_rules(getattr(website, "waf_custom_rules", "") or "")


def sync_site_rules(
    domain: str,
    enabled_rule_ids: Iterable[str],
    custom_rules: str = "",
    *,
    defer_reload: bool = False,
    bot_blocking: bool = True,
    bot_extra: Iterable[str] = (),
    global_bots: Iterable[str] | None = None,
) -> CommandResult:
    safe_domain = _validate_domain(domain)
    content = render_site_rules(
        safe_domain, enabled_rule_ids, custom_rules,
        bot_blocking=bot_blocking, bot_extra=bot_extra,
        global_bots=global_bots,
    )
    # defer_reload writes the site rules file without restarting OpenLiteSpeed;
    # a bulk caller must trigger one reload afterwards (see da_import).
    return shell.privileged(
        "waf-site-save-defer" if defer_reload else "waf-site-save",
        helper_args=[safe_domain],
        check=False,
        input=content,
        fallback=["bash", "-lc", "cat >/tmp/opanel-ent-waf-site.conf && echo WAF site rules saved"],
    )


def sync_website_rules(
    website: Website,
    *,
    defer_reload: bool = False,
    global_bots: Iterable[str] | None = None,
) -> CommandResult:
    bots = website_bot_settings(website)
    return sync_site_rules(
        website.domain,
        website_enabled_rule_ids(website),
        website_custom_rules(website),
        defer_reload=defer_reload,
        bot_blocking=bots["bot_blocking_enabled"],
        bot_extra=bots["bot_extra"],
        global_bots=global_bots,
    )


def resync_all_websites(db) -> dict:
    """Re-render every site's rules -- what makes an edit to the global bot list
    actually reach the sites. One OLS reload at the end, not one per site."""
    from app.services import openlitespeed as webserver

    bots = global_bad_bots()
    websites = db.query(Website).all()
    failed: list[str] = []
    for website in websites:
        try:
            result = sync_website_rules(website, defer_reload=True, global_bots=bots)
            if result.returncode != 0:
                failed.append(website.domain)
        except (ValueError, RuntimeError):
            failed.append(website.domain)
    webserver.reload_service()
    return {"total": len(websites), "failed": failed}


def site_config(website: Website) -> dict:
    from app.services import openlitespeed as webserver

    enabled = website_enabled_rule_ids(website)
    return {
        "website_id": website.id,
        "domain": website.domain,
        "waf_enabled": bool(website.waf_enabled),
        "rules_file": site_rules_file(website.domain),
        "default_rules": [
            {
                **rule,
                "enabled": rule["id"] in enabled,
            }
            for rule in default_rule_definitions()
        ],
        "enabled_rule_ids": [rule["id"] for rule in DEFAULT_RULES if rule["id"] in enabled],
        "custom_rules": website_custom_rules(website),
        **website_bot_settings(website),
        "global_bad_bots": global_bad_bots(),
    }


def save_website_config(
    website: Website,
    enabled_rule_ids: Iterable[str],
    custom_rules: str,
    *,
    bot_blocking_enabled: bool | None = None,
    bot_extra=None,
) -> CommandResult:
    selected = validate_enabled_rule_ids(enabled_rule_ids)
    custom = _validate_custom_rules(custom_rules)
    website.waf_default_rules = json.dumps(selection_payload(selected), ensure_ascii=True)
    website.waf_custom_rules = custom
    if bot_blocking_enabled is not None:
        website.waf_bot_enabled = bool(bot_blocking_enabled)
    if bot_extra is not None:
        website.waf_bot_extra = "\n".join(normalize_bot_patterns(bot_extra))
    bots = website_bot_settings(website)
    return sync_site_rules(
        website.domain, selected, custom,
        bot_blocking=bots["bot_blocking_enabled"],
        bot_extra=bots["bot_extra"],
    )


def access_log_report(
    website_domains: Iterable[str],
    domain: str = "",
    verdict: str = "",
    query: str = "",
    limit: int = 50,
    offset: int = 0,
    lines: int = 5000,
) -> dict:
    if verdict not in {"", "allow", "block"}:
        raise ValueError("Verdict must be allow, block, or empty")
    safe_limit, safe_offset, safe_lines = _validate_access_log_options(limit, offset, lines)
    domains = _domains_for_log(website_domains, domain)
    paths: dict[str, str] = {}
    if not domains:
        return {
            "entries": [],
            "total": 0,
            "limit": safe_limit,
            "offset": safe_offset,
            "domains": [],
            "paths": {},
            "lines": safe_lines,
        }
    result = shell.privileged(
        "waf-access-log-read",
        helper_args=[str(safe_lines), *domains],
        check=False,
        fallback=["bash", "-lc", "lines=\"$1\"; shift; for domain in \"$@\"; do path=\"/var/log/openlitespeed/${domain}.access.log\"; echo \"opanel_ent_LOG_PATH=${domain}\t${path}\"; [ -f \"$path\" ] && tail -n \"$lines\" -- \"$path\" | sed \"s/^/${domain}\\t/\"; done", "opanel-ent-waf-access-log-read", str(safe_lines), *domains],
    )
    if result.returncode != 0:
        raise RuntimeError((result.stderr or result.stdout or "Cannot read WAF access logs").strip())
    paths, payload = _parse_log_payload(result.stdout)
    modsec_reasons = _parse_modsec_reasons(result.stderr or "")
    entries = []
    for raw_line in payload.splitlines():
        line_domain, sep, line = raw_line.partition("\t")
        if not sep or line_domain not in domains:
            continue
        entry = parse_access_log_line(line, line_domain, modsec_reasons)
        if entry and _entry_matches(entry, domain, verdict, query.strip()):
            entries.append(entry)
    entries.sort(key=lambda item: item.get("timestamp") or "", reverse=True)
    total = len(entries)
    return {
        "entries": entries[safe_offset:safe_offset + safe_limit],
        "total": total,
        "limit": safe_limit,
        "offset": safe_offset,
        "domains": domains,
        "paths": paths,
        "lines": safe_lines,
    }


def clear_access_logs(website_domains: Iterable[str], domain: str = "") -> CommandResult:
    domains = _domains_for_log(website_domains, domain)
    if not domains:
        return CommandResult("waf-access-log-clear", 0, "No WAF access logs to clear.", "")
    return shell.privileged(
        "waf-access-log-clear",
        helper_args=domains,
        check=False,
        fallback=["bash", "-lc", "for domain in \"$@\"; do path=\"/var/log/openlitespeed/${domain}.access.log\"; [ -f \"$path\" ] && : >\"$path\"; done; echo WAF access logs cleared", "opanel-ent-waf-access-log-clear", *domains],
    )


def status():
    return shell.privileged(
        "waf-status",
        check=False,
        fallback=["bash", "-lc", "test -f /usr/local/lsws/conf/opanel-ent/waf/opanel-ent-base.conf && echo installed || echo not-installed"],
    )


def install_engine():
    return shell.privileged(
        "waf-install",
        check=False,
        fallback=["bash", "-lc", "apt-get update && apt-get install -y ols-modsecurity"],
    )


def update_rules():
    return shell.privileged(
        "waf-update",
        check=False,
        fallback=["bash", "-lc", "echo no WAF updater found"],
    )


def default_rules():
    return shell.privileged(
        "waf-default-rules",
        check=False,
        fallback=["bash", "-lc", "cat /usr/local/lsws/conf/opanel-ent/waf/opanel-ent-default.conf 2>/dev/null || true"],
    )


def custom_rules():
    return shell.privileged(
        "waf-custom-rules",
        check=False,
        fallback=["bash", "-lc", "cat /usr/local/lsws/conf/opanel-ent/waf/opanel-ent-custom.conf 2>/dev/null || true"],
    )


def save_custom_rules(content: str):
    return shell.privileged(
        "waf-custom-save",
        check=False,
        input=_validate_custom_rules(content),
        fallback=["bash", "-lc", "cat >/tmp/opanel-ent-waf-custom.conf && echo WAF custom rules saved"],
    )
