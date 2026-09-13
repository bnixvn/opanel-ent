import ipaddress
import json
import re
from pathlib import Path
from typing import Optional

from app.core.config import settings
from app.services.shell import CommandResult, shell

# ---------------------------------------------------------------------------
# Constants
# ---------------------------------------------------------------------------
PORT_RE = re.compile(r"^[0-9]{1,5}$")
PROTOCOLS = {"tcp", "udp"}
# Ports the helper opens for every install (see iptables_add_default_allowances).
DEFAULT_PROTECTED_PORTS = {22, 25, 80, 443, 465, 587}
LEGACY_NUMBERED_RULE_RE = re.compile(r"^\[\s*(\d+)\]\s+(.+?)\s{2,}(ALLOW|DENY|REJECT|LIMIT)\s+(IN|OUT)\s+(.+)$", re.I)
IPTABLES_NUMBERED_RULE_RE = re.compile(r"^(?:\[\s*)?(\d+)(?:\])?\s+(\S+)\s+(\S+)\s+--\s+(\S+)\s+(\S+)\s*(.*)$", re.I)
PORT_SPEC_RE = re.compile(r"^(\d{1,5})/(tcp|udp)(?:\s+\(v6\))?$", re.I)
ZONE_COMMENT_RE = re.compile(r"(?:opanel-ent|bpanel):(PanelZone|UserZone)", re.I)

CHAIN_INPUT = "OPANEL_ENT_INPUT"
CHAIN_USER = "OPANEL_ENT_USER"
CHAIN_BLOCKLIST = "OPANEL_ENT_BLOCKLIST"

RULES_FILE = Path("/var/lib/opanel-ent/firewall/rules.json")
BLOCKLIST_URLS_FILE = Path("/var/lib/opanel-ent/firewall-blocklists.urls")

IPSET_V4 = "opanel_ent_blocklist4"
IPSET_V6 = "opanel_ent_blocklist6"
IPSET_V4_NEW = "opanel_ent_blocklist4_new"
IPSET_V6_NEW = "opanel_ent_blocklist6_new"

PANEL_ZONE = "PanelZone"
USER_ZONE = "UserZone"


def _validate_protocol(protocol: str) -> str:
    value = (protocol or "tcp").strip().lower()
    if value not in PROTOCOLS:
        raise ValueError("Protocol must be tcp or udp")
    return value


def _validate_port(port: str | int) -> str:
    value = str(port).strip()
    if not PORT_RE.match(value):
        raise ValueError("Port must be a number from 1 to 65535")
    number = int(value)
    if number < 1 or number > 65535:
        raise ValueError("Port must be a number from 1 to 65535")
    return value


def _validate_network(network: str) -> str:
    value = network.strip()
    try:
        parsed = ipaddress.ip_network(value, strict=False)
    except ValueError as exc:
        raise ValueError("IP must be a valid IPv4/IPv6 address or CIDR network") from exc
    return str(parsed)


def _strip_inline_comment(value: str) -> str:
    return re.sub(r"\s+#.*$", "", value).strip()


def _zone_from_values(*values: str) -> str | None:
    joined = " ".join(values)
    match = ZONE_COMMENT_RE.search(joined)
    if not match:
        return None
    return PANEL_ZONE if match.group(1).lower() == PANEL_ZONE.lower() else USER_ZONE


def _classify_rule_zone(action: str, rule_type: str, port: str, comment_zone: str | None, protected_ports: set[int]) -> tuple[str, bool]:
    if comment_zone:
        return comment_zone, comment_zone == PANEL_ZONE
    is_protected = False
    if rule_type == "port" and action == "ALLOW" and port:
        try:
            is_protected = int(port) in protected_ports
        except ValueError:
            pass
    return (PANEL_ZONE if is_protected else USER_ZONE), is_protected


def _display_source(value: str) -> str:
    value = (value or "").strip()
    if not value or value in {"0.0.0.0/0", "::/0", "0/0"}:
        return "Anywhere"
    return value


def _display_protocol(value: str, extra: str = "") -> str:
    value = (value or "").strip().lower()
    if value == "6":
        return "tcp"
    if value == "17":
        return "udp"
    if value in PROTOCOLS:
        return value
    extra_value = (extra or "").lower()
    if re.search(r"\btcp\b", extra_value):
        return "tcp"
    if re.search(r"\budp\b", extra_value):
        return "udp"
    return value or "all"


def _port_list_from_extra(extra: str) -> list[str]:
    ports: list[str] = []

    def add(token: str) -> None:
        token = token.strip()
        if re.match(r"^\d{1,5}(?::\d{1,5})?$", token) and token not in ports:
            ports.append(token)

    for match in re.finditer(r"\bdpt:(\d{1,5})\b", extra or "", re.I):
        add(match.group(1))
    for match in re.finditer(r"\bdpts:([0-9:]+)\b", extra or "", re.I):
        add(match.group(1))
    for match in re.finditer(r"\bmultiport\s+dports\s+([0-9:,]+)", extra or "", re.I):
        for token in match.group(1).split(","):
            add(token)
    return ports


def _ports_from_to_field(to_value: str) -> tuple[list[str], str]:
    match = re.match(r"^([0-9:,]+)/(tcp|udp)$", (to_value or "").strip(), re.I)
    if not match:
        return [], ""
    return [token for token in match.group(1).split(",") if token], match.group(2).lower()


def open_ports_from_rules(rules: list[dict]) -> list[dict]:
    ports: list[dict] = []
    seen: set[tuple[str, str, str]] = set()
    for rule in rules:
        if (rule.get("action") or "").upper() != "ALLOW":
            continue
        rule_ports, protocol = _ports_from_to_field(str(rule.get("to") or ""))
        if not rule_ports:
            continue
        zone = rule.get("zone") or USER_ZONE
        for port in rule_ports:
            key = (port, protocol, zone)
            if key in seen:
                continue
            seen.add(key)
            ports.append({
                "port": port,
                "protocol": protocol,
                "zone": zone,
                "protected": bool(rule.get("protected")),
                "rule": rule.get("number") or rule.get("id"),
                "source": _display_source(str(rule.get("from") or "")),
            })
    return sorted(ports, key=lambda item: (
        item["protocol"],
        int(item["port"].split(":", 1)[0]) if item["port"].split(":", 1)[0].isdigit() else 999999,
        item["port"],
        item["zone"],
    ))


def _parse_numbered_output(output: str) -> list[dict]:
    protected_ports = set(DEFAULT_PROTECTED_PORTS)
    try:
        protected_ports.add(int(settings.panel_port or 2222))
    except (TypeError, ValueError):
        pass

    result: list[dict] = []
    for raw_line in (output or "").splitlines():
        line = raw_line.strip()
        if not line:
            continue

        legacy_match = LEGACY_NUMBERED_RULE_RE.match(line)
        if legacy_match:
            number = int(legacy_match.group(1))
            to_field = _strip_inline_comment(legacy_match.group(2))
            action = legacy_match.group(3).upper()
            direction = legacy_match.group(4).upper()
            from_field = _strip_inline_comment(legacy_match.group(5))
            port_match = PORT_SPEC_RE.match(to_field)
            if port_match:
                port, protocol = port_match.group(1), port_match.group(2).lower()
                rule_type = "port"
                display_to = f"{port}/{protocol}"
            else:
                port, protocol = "", "tcp"
                rule_type = "ip"
                display_to = "Anywhere" if to_field.lower() == "anywhere" else to_field
            comment_zone = _zone_from_values(line)
            zone, is_protected = _classify_rule_zone(action, rule_type, port, comment_zone, protected_ports)
            result.append({
                "id": number,
                "number": number,
                "to": display_to,
                "action": action,
                "direction": direction,
                "from": _display_source(from_field),
                "zone": zone,
                "protected": is_protected,
            })
            continue

        iptables_match = IPTABLES_NUMBERED_RULE_RE.match(line)
        if iptables_match:
            number = int(iptables_match.group(1))
            target = iptables_match.group(2).upper()
            source = _strip_inline_comment(iptables_match.group(4))
            extra = iptables_match.group(6)
            protocol = _display_protocol(iptables_match.group(3), extra)
            ports = _port_list_from_extra(extra)
            port = ",".join(ports)
            rule_type = "port" if port else "ip"
            if target == "ACCEPT":
                action = "ALLOW"
            elif target in {"DROP", "REJECT"}:
                action = "DENY"
            elif target == "LIMIT":
                action = "LIMIT"
            else:
                action = target
            comment_zone = _zone_from_values(line, extra)
            zone, is_protected = _classify_rule_zone(action, rule_type, port, comment_zone, protected_ports)
            result.append({
                "id": number,
                "number": number,
                "to": f"{port}/{protocol}" if port else "Anywhere",
                "action": action,
                "direction": "IN",
                "from": _display_source(source),
                "zone": zone,
                "protected": is_protected,
            })

    return result


def _panel_port() -> int:
    try:
        return int(settings.panel_port or 2222)
    except (TypeError, ValueError):
        return 2222


# ---------------------------------------------------------------------------
# Persistent rule store
# ---------------------------------------------------------------------------
def _ensure_rules_dir() -> None:
    shell.privileged(
        "iptables-rules-store-ensure",
        check=False,
        fallback=["bash", "-lc", "mkdir -p /var/lib/opanel-ent/firewall"],
    )
    RULES_FILE.parent.mkdir(parents=True, exist_ok=True)


def _read_rules() -> list[dict]:
    if not RULES_FILE.exists():
        return []
    try:
        data = json.loads(RULES_FILE.read_text(encoding="utf-8"))
        return data if isinstance(data, list) else []
    except (OSError, json.JSONDecodeError):
        return []


def _write_rules(rules: list[dict]) -> None:
    _ensure_rules_dir()
    RULES_FILE.write_text(
        json.dumps(rules, indent=2, ensure_ascii=True) + "\n",
        encoding="utf-8",
    )


# ---------------------------------------------------------------------------
# iptables helpers
# ---------------------------------------------------------------------------
def _iptables(*args: str, check: bool = True) -> CommandResult:
    return shell.privileged(
        "iptables-run",
        helper_args=list(args),
        check=check,
        fallback=["iptables"] + list(args),
    )


def _ip6tables(*args: str, check: bool = True) -> CommandResult:
    return shell.privileged(
        "ip6tables-run",
        helper_args=list(args),
        check=check,
        fallback=["ip6tables"] + list(args),
    )


def _ipset(*args: str, check: bool = True) -> CommandResult:
    return shell.privileged(
        "ipset-run",
        helper_args=list(args),
        check=check,
        fallback=["ipset"] + list(args),
    )


# ---------------------------------------------------------------------------
# Status & rule parsing
# ---------------------------------------------------------------------------
def status() -> CommandResult:
    return shell.privileged(
        "iptables-status",
        check=False,
        fallback=[
            "bash", "-lc",
            (
                "echo '=== IPv4 ==='; "
                "iptables -L OPANEL_ENT_INPUT -n --line-numbers 2>/dev/null || echo 'Chain not found'; "
                "echo; echo '=== IPv6 ==='; "
                "ip6tables -L OPANEL_ENT_INPUT -n --line-numbers 2>/dev/null || echo 'Chain not found'"
            ),
        ],
    )


def is_enabled() -> bool:
    result = shell.privileged(
        "iptables-check-enabled",
        check=False,
        fallback=[
            "bash", "-lc",
            "iptables -L OPANEL_ENT_INPUT -n >/dev/null 2>&1 && echo yes || echo no",
        ],
    )
    return "yes" in (result.stdout or "").lower()


def parse_numbered_rules(output: str) -> list[dict]:
    """Parse persistent rules into unified list with zone/protected metadata."""
    rules = _read_rules()
    parsed_output = _parse_numbered_output(output)
    if parsed_output:
        return parsed_output
    result = []
    protected_ports = set(DEFAULT_PROTECTED_PORTS)
    try:
        protected_ports.add(int(settings.panel_port or 2222))
    except (TypeError, ValueError):
        pass

    for rule in rules:
        rule_id = rule.get("id", 0)
        action = (rule.get("action") or "allow").upper()
        network = rule.get("network", "")
        port = rule.get("port", "")
        protocol = rule.get("protocol", "tcp")
        rule_type = rule.get("type", "ip")

        is_protected = False
        if rule_type == "port" and action == "ALLOW" and port:
            try:
                if int(port) in protected_ports:
                    is_protected = True
            except ValueError:
                pass

        zone = PANEL_ZONE if is_protected else USER_ZONE

        if rule_type == "port":
            display_to = f"{port}/{protocol}"
            display_from = "Anywhere"
        else:
            display_to = "Anywhere"
            if port:
                display_to = f"{port}/{protocol}"
            display_from = network or "Anywhere"

        result.append({
            "id": rule_id,
            "number": rule_id,
            "to": display_to,
            "action": action,
            "direction": "IN",
            "from": display_from,
            "zone": zone,
            "protected": is_protected,
        })

    return result


# ---------------------------------------------------------------------------
# Enable / Disable / Reload
# ---------------------------------------------------------------------------
def enable() -> CommandResult:
    """Enable OPanel Enterprise firewall: create chains, set defaults, restore user rules."""
    port = _panel_port()
    script = (
        "#!/usr/bin/env bash\n"
        "set -euo pipefail\n"
        # Create chains
        f"iptables -N {CHAIN_INPUT} 2>/dev/null || true\n"
        f"iptables -N {CHAIN_USER} 2>/dev/null || true\n"
        f"iptables -N {CHAIN_BLOCKLIST} 2>/dev/null || true\n"
        f"ip6tables -N {CHAIN_INPUT} 2>/dev/null || true\n"
        f"ip6tables -N {CHAIN_USER} 2>/dev/null || true\n"
        f"ip6tables -N {CHAIN_BLOCKLIST} 2>/dev/null || true\n"
        # Flush for idempotency
        f"iptables -F {CHAIN_INPUT}\n"
        f"iptables -F {CHAIN_USER}\n"
        f"iptables -F {CHAIN_BLOCKLIST}\n"
        f"ip6tables -F {CHAIN_INPUT}\n"
        f"ip6tables -F {CHAIN_USER}\n"
        f"ip6tables -F {CHAIN_BLOCKLIST}\n"
        # Insert into INPUT
        f"iptables -C INPUT -j {CHAIN_INPUT} 2>/dev/null || iptables -I INPUT 1 -j {CHAIN_INPUT}\n"
        f"ip6tables -C INPUT -j {CHAIN_INPUT} 2>/dev/null || ip6tables -I INPUT 1 -j {CHAIN_INPUT}\n"
        # Established + loopback
        f"iptables -A {CHAIN_INPUT} -m state --state ESTABLISHED,RELATED -j ACCEPT\n"
        f"iptables -A {CHAIN_INPUT} -i lo -j ACCEPT\n"
        f"ip6tables -A {CHAIN_INPUT} -m state --state ESTABLISHED,RELATED -j ACCEPT\n"
        f"ip6tables -A {CHAIN_INPUT} -i lo -j ACCEPT\n"
        # Default port allowances
        f"for p in 22 80 443 {port} 465 587; do\n"
        f"  iptables -A {CHAIN_INPUT} -p tcp --dport $p -j ACCEPT\n"
        f"  ip6tables -A {CHAIN_INPUT} -p tcp --dport $p -j ACCEPT\n"
        "done\n"
        # Blocklist chain (ipset)
        f"iptables -A {CHAIN_BLOCKLIST} -m set --match-set {IPSET_V4} src -j DROP 2>/dev/null || true\n"
        f"ip6tables -A {CHAIN_BLOCKLIST} -m set --match-set {IPSET_V6} src -j DROP 2>/dev/null || true\n"
        # Blocklist and admin rules are consulted before the default port
        # allowances, which accept from any source and would otherwise end the
        # traversal before a block could apply.
        f"iptables -I {CHAIN_INPUT} 1 -j {CHAIN_USER}\n"
        f"iptables -I {CHAIN_INPUT} 1 -j {CHAIN_BLOCKLIST}\n"
        f"ip6tables -I {CHAIN_INPUT} 1 -j {CHAIN_USER}\n"
        f"ip6tables -I {CHAIN_INPUT} 1 -j {CHAIN_BLOCKLIST}\n"
        # Default policy accept (don't lock out)
        "iptables -P INPUT ACCEPT\n"
        "ip6tables -P INPUT ACCEPT\n"
        "echo 'OPanel Enterprise firewall enabled'\n"
    )
    result = shell.privileged("iptables-enable", check=False, fallback=["bash", "-lc", script])
    _restore_user_rules()
    # Snapshot after the stored rules are back, not before: persisting inside
    # iptables-enable captured an empty user chain, so a reboot dropped every
    # admin rule while the panel still listed them.
    shell.privileged("iptables-persist", check=False, fallback=["true"])
    return result


def _restore_user_rules() -> None:
    """Re-apply all persistent rules to the live OPANEL_ENT_USER chain."""
    for rule in _read_rules():
        _apply_rule_to_iptables(rule)


def disable() -> CommandResult:
    """Disable OPanel Enterprise firewall: remove chains from INPUT, flush and delete."""
    script = (
        "#!/usr/bin/env bash\n"
        "set -euo pipefail\n"
        f"iptables -D INPUT -j {CHAIN_INPUT} 2>/dev/null || true\n"
        f"ip6tables -D INPUT -j {CHAIN_INPUT} 2>/dev/null || true\n"
        f"for chain in {CHAIN_INPUT} {CHAIN_USER} {CHAIN_BLOCKLIST}; do\n"
        '  iptables -F "$chain" 2>/dev/null || true\n'
        '  iptables -X "$chain" 2>/dev/null || true\n'
        '  ip6tables -F "$chain" 2>/dev/null || true\n'
        '  ip6tables -X "$chain" 2>/dev/null || true\n'
        "done\n"
        "iptables -P INPUT ACCEPT\n"
        "ip6tables -P INPUT ACCEPT\n"
        "echo 'OPanel Enterprise firewall disabled'\n"
    )
    return shell.privileged("iptables-disable", check=False, fallback=["bash", "-lc", script])


def reload() -> CommandResult:
    """Reload: disable then re-enable with current rules."""
    disable()
    return enable()


# ---------------------------------------------------------------------------
# User rules (declarative, stored in JSON)
# ---------------------------------------------------------------------------
def _next_rule_id(rules: list[dict]) -> int:
    if not rules:
        return 1
    return max(r.get("id", 0) for r in rules) + 1


def _apply_rule_to_iptables(rule: dict) -> None:
    action = rule.get("action", "allow").upper()
    network = rule.get("network", "")
    port = rule.get("port", "")
    protocol = rule.get("protocol", "tcp")
    if not network and rule.get("type") == "ip":
        return
    is_v6 = network and ":" in network
    target = "ACCEPT" if action == "ALLOW" else "DROP"

    if rule.get("type") == "port":
        _iptables("-A", CHAIN_USER, "-p", protocol, "--dport", str(port), "-j", "ACCEPT", check=False)
        _ip6tables("-A", CHAIN_USER, "-p", protocol, "--dport", str(port), "-j", "ACCEPT", check=False)
    elif network:
        cmd_fn = _ip6tables if is_v6 else _iptables
        argv = ["-A", CHAIN_USER]
        if port:
            argv += ["-p", protocol, "--dport", str(port)]
        argv += ["-s", network, "-j", target]
        cmd_fn(*argv, check=False)


def _remove_rule_from_iptables(rule: dict) -> None:
    action = rule.get("action", "allow").upper()
    network = rule.get("network", "")
    port = rule.get("port", "")
    protocol = rule.get("protocol", "tcp")
    if not network and rule.get("type") == "ip":
        return
    is_v6 = network and ":" in network
    target = "ACCEPT" if action == "ALLOW" else "DROP"

    if rule.get("type") == "port":
        _iptables("-D", CHAIN_USER, "-p", protocol, "--dport", str(port), "-j", "ACCEPT", check=False)
        _ip6tables("-D", CHAIN_USER, "-p", protocol, "--dport", str(port), "-j", "ACCEPT", check=False)
    elif network:
        cmd_fn = _ip6tables if is_v6 else _iptables
        argv = ["-D", CHAIN_USER]
        if port:
            argv += ["-p", protocol, "--dport", str(port)]
        argv += ["-s", network, "-j", target]
        cmd_fn(*argv, check=False)


def default_allowed_ports() -> set[int]:
    """Ports every install already accepts, panel port included."""
    ports = set(DEFAULT_PROTECTED_PORTS)
    try:
        ports.add(int(settings.panel_port or 2222))
    except (TypeError, ValueError):
        pass
    return ports


def allow_port(port: str | int, protocol: str = "tcp") -> CommandResult:
    clean_port = _validate_port(port)
    clean_protocol = _validate_protocol(protocol)

    # A port the default zone already accepts needs no user rule. Adding one
    # anyway leaves two rules for the same port -- the panel then lists it
    # twice and the admin cannot tell which one is theirs.
    if clean_protocol == "tcp" and clean_port.isdigit() and int(clean_port) in default_allowed_ports():
        return CommandResult(
            command=f"allow port {clean_port}/{clean_protocol}",
            returncode=0,
            stdout=f"Port {clean_port} is already open by default",
            stderr="",
        )

    rules = _read_rules()
    for existing in rules:
        if (
            existing.get("type") == "port"
            and str(existing.get("port")) == clean_port
            and existing.get("protocol") == clean_protocol
        ):
            return CommandResult(
                command=f"allow port {clean_port}/{clean_protocol}",
                returncode=0,
                stdout=f"Port {clean_port}/{clean_protocol} is already allowed",
                stderr="",
            )

    rule = {
        "id": _next_rule_id(rules),
        "action": "allow",
        "type": "port",
        "port": clean_port,
        "protocol": clean_protocol,
        "network": "",
    }
    rules.append(rule)
    _write_rules(rules)
    _iptables("-A", CHAIN_USER, "-p", clean_protocol, "--dport", clean_port, "-j", "ACCEPT", check=False)
    _ip6tables("-A", CHAIN_USER, "-p", clean_protocol, "--dport", clean_port, "-j", "ACCEPT", check=False)
    return CommandResult(command=f"allow port {clean_port}/{clean_protocol}", returncode=0, stdout="Port allowed", stderr="")


def allow_ip(network: str, port: Optional[str | int] = None, protocol: str = "tcp") -> CommandResult:
    clean_network = _validate_network(network)
    clean_protocol = _validate_protocol(protocol)
    rules = _read_rules()
    rule = {
        "id": _next_rule_id(rules),
        "action": "allow",
        "type": "ip",
        "network": clean_network,
        "protocol": clean_protocol,
    }
    if port:
        rule["port"] = _validate_port(port)
    rules.append(rule)
    _write_rules(rules)
    _apply_rule_to_iptables(rule)
    return CommandResult(command=f"allow ip {clean_network}", returncode=0, stdout="IP allowed", stderr="")


def block_ip(network: str, port: Optional[str | int] = None, protocol: str = "tcp") -> CommandResult:
    clean_network = _validate_network(network)
    clean_protocol = _validate_protocol(protocol)
    rules = _read_rules()
    rule = {
        "id": _next_rule_id(rules),
        "action": "deny",
        "type": "ip",
        "network": clean_network,
        "protocol": clean_protocol,
    }
    if port:
        rule["port"] = _validate_port(port)
    rules.append(rule)
    _write_rules(rules)
    _apply_rule_to_iptables(rule)
    return CommandResult(command=f"block ip {clean_network}", returncode=0, stdout="IP blocked", stderr="")


def delete_rule(rule_id: int) -> CommandResult:
    if rule_id < 1:
        raise ValueError("Rule ID must be greater than 0")
    rules = _read_rules()
    target = next((r for r in rules if r.get("id") == rule_id), None)
    if not target:
        raise ValueError(f"Rule {rule_id} not found")
    # Check if protected
    rule_type = target.get("type", "ip")
    action = (target.get("action") or "").upper()
    port = target.get("port", "")
    protected_ports = set(DEFAULT_PROTECTED_PORTS)
    try:
        protected_ports.add(int(settings.panel_port or 2222))
    except (TypeError, ValueError):
        pass
    if rule_type == "port" and action == "ALLOW" and port:
        # int() has to be guarded, but the refusal must not be: raising inside
        # the try meant the except below swallowed it and the default ports were
        # deletable after all.
        try:
            port_number = int(port)
        except (ValueError, TypeError):
            port_number = None
        if port_number is not None and port_number in protected_ports:
            raise ValueError("Default panel, mail, web, and SSH firewall rules cannot be deleted")
    _remove_rule_from_iptables(target)
    rules = [r for r in rules if r.get("id") != rule_id]
    _write_rules(rules)
    return CommandResult(command=f"delete rule {rule_id}", returncode=0, stdout=f"Rule {rule_id} deleted", stderr="")


def list_rules() -> list[dict]:
    return _read_rules()


# ---------------------------------------------------------------------------
# Blocklist (ipset + iptables)
# ---------------------------------------------------------------------------
def blocklists() -> CommandResult:
    return shell.privileged(
        "iptables-blocklist-status",
        check=False,
        fallback=[
            "bash", "-lc",
            (
                f"echo 'URLs:'; cat {BLOCKLIST_URLS_FILE} 2>/dev/null || true; "
                "echo; echo 'Engine:'; echo '  iptables+ipset'"
            ),
        ],
    )


def add_blocklist_url(url: str) -> CommandResult:
    url = (url or "").strip()
    if not url.startswith(("http://", "https://")):
        raise ValueError("URL must start with http:// or https://")
    return shell.privileged(
        "iptables-blocklist-add",
        helper_args=[url],
        check=False,
        fallback=[
            "bash", "-lc",
            (
                f"mkdir -p {BLOCKLIST_URLS_FILE.parent} && "
                f"touch {BLOCKLIST_URLS_FILE} && "
                "(grep -Fxq -- \"$1\" \"$2\" || printf '%s\\n' \"$1\" >>\"$2\") && "
                "sort -u -o \"$2\" \"$2\" && "
                "echo 'Blocklist URL added'"
            ),
            "bash", url, str(BLOCKLIST_URLS_FILE),
        ],
    )


def delete_blocklist_url(url: str) -> CommandResult:
    url = (url or "").strip()
    if not url.startswith(("http://", "https://")):
        raise ValueError("URL must start with http:// or https://")
    return shell.privileged(
        "iptables-blocklist-delete",
        helper_args=[url],
        check=False,
        fallback=[
            "bash", "-lc",
            (
                f"mkdir -p {BLOCKLIST_URLS_FILE.parent} && "
                f"touch {BLOCKLIST_URLS_FILE} && "
                "grep -Fxv -- \"$1\" \"$2\" >\"$2.tmp\" || true && "
                "mv -f \"$2.tmp\" \"$2\" && "
                "echo 'Blocklist URL removed'"
            ),
            "bash", url, str(BLOCKLIST_URLS_FILE),
        ],
    )


def update_blocklists() -> CommandResult:
    return shell.privileged(
        "iptables-blocklist-run",
        check=False,
        fallback=["bash", "-lc", "echo 'Blocklist update triggered'"],
    )
