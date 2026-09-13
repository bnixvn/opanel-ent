"""Server address discovery and the IPv6 switch.

Detection is deliberately live rather than something recorded at install time:
an IPv6 block added to the VPS months later has to show up in the panel without
a reinstall.
"""

import ipaddress
import re
import socket
from pathlib import Path

from app.services.shell import shell

IF_INET6 = Path("/proc/net/if_inet6")
# The API runs as an unprivileged service user whose PATH may not carry the
# sbin directories, so iproute2 is looked up by absolute path as well.
IP_BINARIES = ("ip", "/sbin/ip", "/usr/sbin/ip")
IP_ADDR_LINE = re.compile(r"^\d+:\s+(?P<iface>[^:\s]+)\s+inet(?P<family>6?)\s+(?P<addr>[^/\s]+)")


def _usable(address: str) -> bool:
    """Skip the addresses that are never worth showing an admin."""
    try:
        parsed = ipaddress.ip_address(address)
    except ValueError:
        return False
    return not (
        parsed.is_loopback
        or parsed.is_link_local
        or parsed.is_multicast
        or parsed.is_unspecified
    )


def _parse_ip_output(output: str) -> tuple[list[str], list[str]]:
    ipv4: list[str] = []
    ipv6: list[str] = []
    for line in (output or "").splitlines():
        match = IP_ADDR_LINE.match(line.strip())
        if not match:
            continue
        if match.group("iface") == "lo":
            continue
        address = match.group("addr")
        if not _usable(address):
            continue
        bucket = ipv6 if match.group("family") == "6" else ipv4
        if address not in bucket:
            bucket.append(address)
    return ipv4, ipv6


def _proc_ipv6() -> list[str]:
    """Fallback for boxes without iproute2: /proc/net/if_inet6.

    Each line is a 32-char hex address, interface index, prefix length, scope
    and flags; scope 00 is global.
    """
    found: list[str] = []
    try:
        lines = IF_INET6.read_text(encoding="utf-8").splitlines()
    except OSError:
        return found
    for line in lines:
        fields = line.split()
        if len(fields) < 6 or fields[3] != "00" or fields[5] == "lo":
            continue
        raw = fields[0]
        if len(raw) != 32:
            continue
        address = ":".join(raw[index:index + 4] for index in range(0, 32, 4))
        try:
            address = str(ipaddress.ip_address(address))
        except ValueError:
            continue
        if _usable(address) and address not in found:
            found.append(address)
    return found


def _ip_output() -> str:
    for binary in IP_BINARIES:
        try:
            result = shell.run([binary, "-o", "addr", "show", "scope", "global"], check=False)
        except (OSError, RuntimeError):
            continue
        if result.returncode == 0 and result.stdout.strip():
            return result.stdout
    return ""


def _routed_source_address(family: int, probe: str) -> str:
    """Address the kernel would send from, without sending anything.

    connect() on a UDP socket only picks a route, so this works on a box with
    no iproute2 and costs no traffic.
    """
    try:
        with socket.socket(family, socket.SOCK_DGRAM) as sock:
            sock.settimeout(0.5)
            sock.connect((probe, 53))
            address = sock.getsockname()[0]
    except OSError:
        return ""
    return address if _usable(address) else ""


def detect_addresses() -> dict:
    """Global IPv4 and IPv6 addresses currently configured on the server."""
    ipv4, ipv6 = _parse_ip_output(_ip_output())
    if not ipv6:
        ipv6 = _proc_ipv6()
    if not ipv4:
        fallback = _routed_source_address(socket.AF_INET, "8.8.8.8")
        ipv4 = [fallback] if fallback else []
    return {"ipv4": ipv4, "ipv6": ipv6}


def ipv6_available() -> bool:
    return bool(detect_addresses()["ipv6"])


def apply_ipv6(enabled: bool) -> str:
    """Rebuild the web server listeners and the panel bind for *enabled*.

    The flag itself is already persisted by the caller: both this helper call
    and every later config sync read it from panel-settings.json, so the two
    can never drift apart.
    """
    result = shell.privileged(
        "panel-ipv6-set",
        helper_args=["on" if enabled else "off"],
        check=False,
        fallback=["true"],
    )
    if result.returncode != 0:
        raise RuntimeError((result.stderr or result.stdout or "Could not apply the IPv6 setting").strip())
    return (result.stdout or "").strip()


def read_stored_flag(raw: dict, key: str = "ipv6_enabled") -> bool | None:
    """The stored IPv6 choice, or None when the admin has never made one."""
    if isinstance(raw, dict) and key in raw:
        return bool(raw[key])
    return None
