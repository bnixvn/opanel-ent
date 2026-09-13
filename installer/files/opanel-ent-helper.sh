#!/usr/bin/env bash
# /usr/local/sbin/opanel-ent-helper
#
# Root-privileged trampoline for the OPanel Enterprise API daemon.
# This is the ONLY code that runs as root for the daemon.
# Installed by install.sh as root:root mode 0750, callable only by user
# 'opanel-ent' through sudo (see /etc/sudoers.d/opanel-ent).
#
# Every operation here is the trust boundary. Validate aggressively.

set -euo pipefail

if [[ "${SUDO_USER:-}" != "opanel-ent" ]]; then
  echo "opanel-ent-helper must be invoked by user 'opanel-ent' via sudo" >&2
  exit 2
fi

# Reset PATH so an attacker cannot ship a shadow binary in opanel-ent's PATH.
PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
export PATH

ALLOWED_SERVICES=(lsws mariadb redis-server opanel-ent-api)
ALLOWED_ACTIONS=(start stop restart reload status is-active is-enabled)
HOME_ROOT="/home"
OLS_HTTPD_CONF="/usr/local/lsws/conf/httpd_config.conf"
OLS_VHOSTS_DIR="/usr/local/lsws/conf/opanel-ent/vhosts"
PHP_CONF_DIRS=(/usr/local/lsws/lsphp{83,84}/etc/php.d)
opanel_ent_SITES_GROUP="opanel-ent-sites"
opanel_ent_SFTP_GROUP="opanel-ent-sftp"
APP_DIR="/opt/opanel-ent"
ENV_FILE="${APP_DIR}/backend/.env"
DEFAULT_PANEL_PORT="2222"
SOURCE_DIR="/opt/opanel-ent-source"
UPDATE_SCRIPT="/usr/local/sbin/opanel-ent-update"
opanel_ent_DATA_DIR="/var/lib/opanel-ent"
FIREWALL_BLOCKLIST_URLS="${opanel_ent_DATA_DIR}/firewall-blocklists.urls"
FIREWALL_BLOCKLIST_WORK="${opanel_ent_DATA_DIR}/firewall-blocklists.current"
BLOCKLIST_DIR="${opanel_ent_DATA_DIR}/firewall"
BLOCKLIST_IPSET_V4="opanel_ent_blocklist4"
BLOCKLIST_IPSET_V6="opanel_ent_blocklist6"
OLS_CUSTOM_DIR="/usr/local/lsws/conf/opanel-ent/custom"
LSPHP_DEFAULT_WORKER_MB=128
LSPHP_DEFAULT_REQUEST_TERMINATE_TIMEOUT=300
MARIADB_TUNING_CONF="/etc/mysql/mariadb.conf.d/90-opanel-ent-tuning.cnf"

deny() { echo "opanel-ent-helper: $*" >&2; exit 1; }

restart_openlitespeed() {
  ensure_lshttpd_runtime_dir
  if systemctl cat lshttpd.service >/dev/null 2>&1; then
    systemctl restart lshttpd.service
  else
    /usr/local/lsws/bin/lswsctrl restart
  fi
}

ensure_opanel_data_dir() {
  install -d -o opanel-ent -g opanel-ent -m 0750 "$opanel_ent_DATA_DIR"
}

ensure_ols_conf_dir_writable() {
  ensure_sites_group
  install -d -o root -g root -m 0755 "$BLOCKLIST_DIR"
  install -d -o www-data -g "$opanel_ent_SITES_GROUP" -m 2775 /var/log/openlitespeed
  ensure_lshttpd_runtime_dir
  chmod g+s /var/log/openlitespeed 2>/dev/null || true
  if getent group opanel-ent >/dev/null 2>&1; then
    install -d -o root -g opanel-ent -m 2775 "$OLS_VHOSTS_DIR"
    install -d -o root -g opanel-ent -m 2775 "$OLS_CUSTOM_DIR"
    chmod g+s "$OLS_VHOSTS_DIR" 2>/dev/null || true
    chmod g+s "$OLS_CUSTOM_DIR" 2>/dev/null || true
  else
    install -d -o root -g root -m 0755 "$OLS_VHOSTS_DIR"
    install -d -o root -g root -m 0755 "$OLS_CUSTOM_DIR"
  fi
}

ensure_lshttpd_runtime_dir() {
  ensure_sites_group
  install -d -o www-data -g "$opanel_ent_SITES_GROUP" -m 2775 /tmp/lshttpd /tmp/lshttpd/swap
  chmod g+s /tmp/lshttpd 2>/dev/null || true
  if [[ -d /tmp/lshttpd/swap ]]; then
    chown -R www-data:"$opanel_ent_SITES_GROUP" /tmp/lshttpd/swap 2>/dev/null || true
    find /tmp/lshttpd/swap -type d -exec chmod 2775 {} + 2>/dev/null || true
    find /tmp/lshttpd/swap -type f -exec chmod 0664 {} + 2>/dev/null || true
  fi
  chown www-data:"$opanel_ent_SITES_GROUP" /tmp/lshttpd/lsphp*.sock /tmp/lshttpd/lsphp*.sock.pid 2>/dev/null || true
  chmod 0664 /tmp/lshttpd/lsphp*.sock.pid 2>/dev/null || true
}

fix_phpmyadmin_permissions() {
  local file
  for file in \
    /etc/phpmyadmin/conf.d/opanel-ent-signon.php \
    /etc/phpmyadmin/config.inc.php \
    /usr/share/phpmyadmin/opanel-ent-signon.php; do
    [[ -e "$file" ]] || continue
    chgrp "$opanel_ent_SITES_GROUP" "$file" 2>/dev/null || true
    chmod 0644 "$file" 2>/dev/null || true
  done
  for file in \
    /etc/phpmyadmin/config-db.php \
    /var/lib/phpmyadmin/blowfish_secret.inc.php; do
    [[ -e "$file" ]] || continue
    chgrp "$opanel_ent_SITES_GROUP" "$file" 2>/dev/null || true
    chmod 0640 "$file" 2>/dev/null || true
  done
}

ols_disable_conflicting_apache() {
  if systemctl list-unit-files apache2.service >/dev/null 2>&1; then
    systemctl disable --now apache2 >/dev/null 2>&1 || true
  fi
}

ensure_ols_modsecurity_enabled() {
  [[ -f /usr/local/lsws/modules/mod_security.so ]] || return 1
  python3 - "$OLS_HTTPD_CONF" <<'PY'
import pathlib
import re
import sys

conf = pathlib.Path(sys.argv[1])
if conf.exists():
    text = conf.read_text(encoding="utf-8", errors="replace").replace("\r\n", "\n")
else:
    text = ""
text = re.sub(r"(?ms)^# OPANEL_ENT managed ModSecurity BEGIN\n.*?^# OPANEL_ENT managed ModSecurity END\n?", "", text)
block = (
    "# OPANEL_ENT managed ModSecurity BEGIN\n"
    "module mod_security {\n"
    "    modsecurity             on\n"
    "    ls_enabled              1\n"
    "}\n"
    "# OPANEL_ENT managed ModSecurity END\n\n"
)
marker = "# OPanel Enterprise managed vhosts BEGIN"
pos = text.find(marker)
if pos >= 0:
    text = text[:pos] + block + text[pos:]
else:
    text = text.rstrip() + "\n\n" + block
conf.write_text(text, encoding="utf-8")
PY
}

ols_sync_main_config() {
  ensure_ols_conf_dir_writable
  install -d -o root -g root -m 0755 /usr/local/lsws/conf/opanel-ent
  ols_disable_conflicting_apache
  ensure_ols_modsecurity_enabled >/dev/null 2>&1 || true
  python3 - "$OLS_HTTPD_CONF" "$OLS_VHOSTS_DIR" "$ENV_FILE" <<'PY'
import pathlib
import re
import sys

conf = pathlib.Path(sys.argv[1])
vhosts_dir = pathlib.Path(sys.argv[2])
env_file = pathlib.Path(sys.argv[3])
tools_conf_name = "00-opanel-ent-tools.conf"
tools_conf = vhosts_dir / tools_conf_name
domain_re = re.compile(r"^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?(?:\.[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?)+$")
ipv4_re = re.compile(r"^(?:25[0-5]|2[0-4][0-9]|1?[0-9]?[0-9])(?:\.(?:25[0-5]|2[0-4][0-9]|1?[0-9]?[0-9])){3}$")
settings_file = pathlib.Path("/var/lib/opanel-ent/panel-settings.json")


def host_has_global_ipv6() -> bool:
    """A global (scope 0) address on something other than loopback."""
    try:
        for line in pathlib.Path("/proc/net/if_inet6").read_text(encoding="utf-8").splitlines():
            fields = line.split()
            if len(fields) >= 6 and fields[3] == "00" and fields[5] != "lo":
                return True
    except OSError:
        return False
    return False


def ipv6_enabled() -> bool:
    """Panel setting wins, but the address still has to exist.

    A listener on an address family the kernel no longer has stops
    OpenLiteSpeed from starting at all, which would take every site down over
    a setting nobody re-read, so a stale "on" flag simply produces no IPv6
    listener.
    """
    if not host_has_global_ipv6():
        return False
    try:
        import json

        stored = json.loads(settings_file.read_text(encoding="utf-8"))
        if isinstance(stored, dict) and "ipv6_enabled" in stored:
            return bool(stored["ipv6_enabled"])
    except (OSError, ValueError):
        pass
    # Never asked: a box that has IPv6 serves it from the first boot.
    return True


def env_get(key: str) -> str:
    if not env_file.is_file():
        return ""
    prefix = f"{key}="
    for line in env_file.read_text(encoding="utf-8", errors="replace").splitlines():
        if line.startswith(prefix):
            return line[len(prefix):].strip()
    return ""

def normalized_host(value: str) -> str:
    value = value.strip().lower()
    if not value:
        return ""
    value = re.sub(r"^https?://", "", value)
    value = value.split("/", 1)[0]
    if ":" in value:
        value = value.rsplit(":", 1)[0]
    if domain_re.fullmatch(value) or ipv4_re.fullmatch(value):
        return value
    return ""

if conf.exists():
    text = conf.read_text(encoding="utf-8", errors="replace")
else:
    text = ""
text = text.replace("\r\n", "\n")
if re.search(r"(?m)^\s*user\s+", text):
    text = re.sub(r"(?m)^\s*user\s+.*$", "user                             www-data", text, count=1)
else:
    text = "user                             www-data\n" + text
if re.search(r"(?m)^\s*group\s+", text):
    text = re.sub(r"(?m)^\s*group\s+.*$", "group                            opanel-ent-sites", text, count=1)
else:
    text = "group                            opanel-ent-sites\n" + text

def remove_named_block(source: str, directive: str, name: str) -> str:
    pattern = re.compile(rf"(?ms)^[ \t]*{re.escape(directive)}[ \t]+{re.escape(name)}[ \t]*\{{.*?^[ \t]*\}}[ \t]*\n?")
    return pattern.sub("", source)

for block_name in ("opanel_ent_http", "opanel_ent_https", "opanel_ent_http6", "opanel_ent_https6"):
    text = remove_named_block(text, "listener", block_name)
text = re.sub(r"(?ms)^# OPanel Enterprise managed vhosts BEGIN\n.*?^# OPanel Enterprise managed vhosts END\n?", "", text)

sites = []
if vhosts_dir.is_dir():
    for vhost_file in sorted(vhosts_dir.glob("*/vhost.conf")):
        domain = vhost_file.parent.name.lower()
        if not domain_re.fullmatch(domain):
            continue
        content = vhost_file.read_text(encoding="utf-8", errors="replace")
        hosts = [domain]
        for match in re.finditer(r"(?m)^\s*vh(?:Domain|Aliases)\s+(.+?)\s*$", content):
            for host in re.split(r"[,\s]+", match.group(1).strip()):
                host = host.strip().lower()
                if domain_re.fullmatch(host) and host not in hosts:
                    hosts.append(host)
        cert_match = re.search(r"(?m)^\s*certFile\s+(.+?)\s*$", content)
        key_match = re.search(r"(?m)^\s*keyFile\s+(.+?)\s*$", content)
        has_ssl = False
        if cert_match and key_match:
            cert_path = pathlib.Path(cert_match.group(1).strip())
            key_path = pathlib.Path(key_match.group(1).strip())
            has_ssl = cert_path.is_file() and key_path.is_file()
        sites.append((domain, hosts, has_ssl))

panel_hosts = []
for raw_host in (env_get("PANEL_DOMAIN"), env_get("PANEL_URL")):
    host = normalized_host(raw_host)
    if host and host not in panel_hosts:
        panel_hosts.append(host)
site_hosts = {host for _domain, hosts, _has_ssl in sites for host in hosts}
tools_hosts = [host for host in panel_hosts if host not in site_hosts]
include_tools_vhost = tools_conf.is_file() and bool(tools_hosts)

managed = ["# OPanel Enterprise managed vhosts BEGIN"]
if include_tools_vhost:
    managed.extend([
        "virtualHost opanel_ent_tools {",
        "    vhRoot                   conf/opanel-ent/vhosts/",
        "    allowSymbolLink          1",
        "    enableScript             1",
        "    restrained               1",
        f"    configFile               conf/opanel-ent/vhosts/{tools_conf_name}",
        "}",
        "",
    ])
for domain, _hosts, _has_ssl in sites:
    managed.extend([
        f"virtualHost {domain} {{",
        f"    vhRoot                   conf/opanel-ent/vhosts/{domain}/",
        "    allowSymbolLink          1",
        "    enableScript             1",
        "    restrained               1",
        "    setUIDMode               2",
        f"    configFile               conf/opanel-ent/vhosts/{domain}/vhost.conf",
        "}",
        "",
    ])

# Read panel TLS cert from tools vhost for the HTTPS listener default
_tools_cert = pathlib.Path("/etc/ssl/certs/ssl-cert-snakeoil.pem")
_tools_key = pathlib.Path("/etc/ssl/private/ssl-cert-snakeoil.key")
if tools_conf.is_file():
    _tc = tools_conf.read_text(encoding="utf-8", errors="replace")
    _cm = re.search(r"(?m)^\s*certFile\s+(.+?)\s*$", _tc)
    _km = re.search(r"(?m)^\s*keyFile\s+(.+?)\s*$", _tc)
    if _cm and _km:
        _cp = pathlib.Path(_cm.group(1).strip())
        _kp = pathlib.Path(_km.group(1).strip())
        if _cp.is_file() and _kp.is_file():
            _tools_cert, _tools_key = _cp, _kp

def listener_block(name: str, address: str, secure: bool, include_ssl_sites: bool) -> list[str]:
    lines = [f"listener {name} {{", f"    address                  {address}", f"    secure                   {1 if secure else 0}"]
    if secure:
        lines.extend([
            f"    keyFile                 {_tools_key}",
            f"    certFile                {_tools_cert}",
            "    certChain               1",
            "    enableSpdy              16",
            "    enableQuic              1",
        ])
    if include_tools_vhost:
        for host in tools_hosts:
            lines.append(f"    map                      opanel_ent_tools {host}")
    for domain, hosts, has_ssl in sites:
        if include_ssl_sites and not has_ssl:
            continue
        lines.append(f"    map                      {domain} {', '.join(hosts)}")
    lines.append("}")
    lines.append("")
    return lines

managed.extend(listener_block("opanel_ent_http", "*:80", False, False))
managed.extend(listener_block("opanel_ent_https", "*:443", True, True))
if ipv6_enabled():
    # "*" is IPv4-only in OpenLiteSpeed; [ANY] is the IPv6 wildcard. Sites are
    # mapped into both, so every vhost answers on either protocol.
    managed.extend(listener_block("opanel_ent_http6", "[ANY]:80", False, False))
    managed.extend(listener_block("opanel_ent_https6", "[ANY]:443", True, True))
managed.append("# OPanel Enterprise managed vhosts END")
managed.append("")

new_text = text.rstrip() + "\n\n" + "\n".join(managed)
conf.parent.mkdir(parents=True, exist_ok=True)
if conf.exists() and conf.read_text(encoding="utf-8", errors="replace") == new_text:
    raise SystemExit(0)
backup = conf.with_suffix(conf.suffix + ".opanel-ent.bak")
if conf.exists():
    backup.write_text(conf.read_text(encoding="utf-8", errors="replace"), encoding="utf-8")
conf.write_text(new_text, encoding="utf-8")
PY
}

file_has_nul() {
  local path="$1"
  python3 - "$path" <<'PY'
import sys

with open(sys.argv[1], "rb") as handle:
    data = handle.read()
sys.exit(0 if b"\0" in data else 1)
PY
}

env_get() {
  local key="$1"
  [[ -f "$ENV_FILE" ]] || return 0
  awk -F= -v key="$key" '$1 == key { sub(/^[^=]*=/, ""); print; exit }' "$ENV_FILE"
}

env_set() {
  local key="$1" value="$2" escaped
  [[ -f "$ENV_FILE" ]] || deny "$ENV_FILE not found"
  escaped="$(printf '%s' "$value" | sed -e 's/[&|]/\\&/g')"
  if grep -q "^${key}=" "$ENV_FILE"; then
    sed -i "s|^${key}=.*|${key}=${escaped}|" "$ENV_FILE"
  else
    printf '%s=%s\n' "$key" "$value" >>"$ENV_FILE"
  fi
}

detect_ip() {
  hostname -I 2>/dev/null | awk '{print $1}' || true
}

is_ipv4() {
  local value="$1" part
  local -a parts
  [[ "$value" =~ ^([0-9]{1,3}\.){3}[0-9]{1,3}$ ]] || return 1
  IFS=. read -r -a parts <<<"$value"
  for part in "${parts[@]}"; do
    (( 10#$part >= 0 && 10#$part <= 255 )) || return 1
  done
}

is_domain() {
  ! is_ipv4 "$1" && [[ "$1" =~ ^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)+$ ]]
}

panel_url_scheme() {
  local url
  url="$(env_get PANEL_URL)"
  case "$url" in
    https://*) echo "https" ;;
    *) echo "http" ;;
  esac
}

panel_tls_enabled() {
  local cert key
  [[ "$(panel_url_scheme)" == "https" ]] || return 1
  cert="$(env_get PANEL_SSL_CERT)"; key="$(env_get PANEL_SSL_KEY)"
  [[ -n "$cert" && -n "$key" && -f "$cert" && -f "$key" ]]
}

require_panel_scheme() {
  [[ "$1" == "http" || "$1" == "https" ]] || deny "invalid panel scheme: $1"
}

require_panel_host() {
  local host="$1"
  if is_domain "$host" || is_ipv4 "$host" || [[ "$host" == "localhost" ]]; then
    return 0
  fi
  deny "invalid panel host: $host"
}

set_outbound_ipv4_preference() {
  # Serving IPv6 says nothing about being able to reach the internet over it.
  # Plenty of VPS images hand out an address with no working route, and many
  # providers block outbound 25/587 on IPv6 -- while glibc starts preferring
  # IPv6 the moment an address exists, so mail to any relay with an AAAA
  # record begins to fail. Pinning outbound to IPv4 leaves delivery exactly as
  # it was before the switch; inbound still answers on both families.
  local mode="$1" tmp
  [[ -f /etc/gai.conf ]] || : >/etc/gai.conf
  tmp="$(mktemp /tmp/opanel-ent-gai.XXXXXX)" || return 0
  awk '$0 == "# OPanel Enterprise BEGIN" { skip = 1 } skip == 0 { print } $0 == "# OPanel Enterprise END" { skip = 0 }'     /etc/gai.conf >"$tmp" 2>/dev/null || cp /etc/gai.conf "$tmp"
  if [[ "$mode" == "on" ]]; then
    printf '%s\n' "# OPanel Enterprise BEGIN" "precedence ::ffff:0:0/96  100" "# OPanel Enterprise END" >>"$tmp"
  fi
  install -o root -g root -m 0644 "$tmp" /etc/gai.conf
  rm -f "$tmp"
}

allow_panel_port() {
  local port="$1"
  iptables_panel_allow_port "$port"
}

iptables_panel_allow_port() {
  local port="$1" binary
  require_port "$port"
  iptables_ensure_opanel_chains
  # Remove any existing opanel-ent panel-zone rules for this port first
  iptables_panel_delete_port_rules "$port" "opanel-ent:PanelZone"
  # Both families: a port opened only for IPv4 is a closed port to every
  # visitor whose DNS answer was an AAAA record.
  for binary in iptables ip6tables; do
    "$binary" -I OPANEL_ENT_INPUT 1 -p tcp --dport "$port" -j ACCEPT -m comment --comment "opanel-ent:PanelZone" 2>/dev/null \
      || "$binary" -I OPANEL_ENT_INPUT 1 -p tcp --dport "$port" -j ACCEPT 2>/dev/null \
      || true
  done
}

firewall_persist_rules() {
  install -d -o root -g root -m 0755 /etc/iptables
  iptables-save >/etc/iptables/rules.v4 2>/dev/null || true
  ip6tables-save >/etc/iptables/rules.v6 2>/dev/null || true
}

iptables_refresh_standard_ports() {
  # SSH, web, and the three mail ports. Re-applied whenever IPv6 is switched
  # on, because a chain built before the box had IPv6 only ever got the IPv4
  # half -- which reads to a visitor arriving over IPv6 as a closed port.
  local port p
  port="$(env_get PANEL_PORT)"
  port="${port:-$DEFAULT_PANEL_PORT}"
  for p in 22 25 80 443 465 587 "$port"; do
    iptables_panel_allow_port "$p" 2>/dev/null || true
  done
}

iptables_panel_delete_port_rules() {
  local port="$1" comment="${2:-}" binary num line_nums
  # Both families, so re-applying a port cannot leave a duplicate IPv6 rule
  # behind and closing one really closes it.
  for binary in iptables ip6tables; do
    line_nums="$("$binary" -L OPANEL_ENT_INPUT -n --line-numbers 2>/dev/null \
      | awk -v port="$port" -v comment="$comment" '
          $0 ~ ("tcp dpt:" port "([^0-9]|$)") {
            if (comment == "" || $0 ~ comment) {
              gsub(/[^0-9]/, "", $1)
              if ($1 != "") print $1
            }
          }
        ' | sort -rn)"
    for num in $line_nums; do
      [[ -n "$num" ]] || continue
      "$binary" -D OPANEL_ENT_INPUT "$num" 2>/dev/null || true
    done
  done
}

iptables_panel_delete_commented_rules() {
  local comment="$1"
  local line_nums
  line_nums="$(iptables -L OPANEL_ENT_INPUT -n --line-numbers 2>/dev/null \
    | awk -v comment="$comment" '
        $0 ~ comment {
          gsub(/[^0-9]/, "", $1)
          if ($1 != "") print $1
        }
      ' | sort -rn)"
  local num
  for num in $line_nums; do
    [[ -n "$num" ]] || continue
    iptables -D OPANEL_ENT_INPUT "$num" 2>/dev/null || true
  done
}

iptables_ensure_opanel_chains() {
  iptables -N OPANEL_ENT_INPUT 2>/dev/null || true
  iptables -N OPANEL_ENT_USER 2>/dev/null || true
  iptables -N OPANEL_ENT_BLOCKLIST 2>/dev/null || true
  ip6tables -N OPANEL_ENT_INPUT 2>/dev/null || true
  ip6tables -N OPANEL_ENT_USER 2>/dev/null || true
  ip6tables -N OPANEL_ENT_BLOCKLIST 2>/dev/null || true
}

iptables_flush_managed_chains() {
  iptables_ensure_opanel_chains
  iptables -F OPANEL_ENT_INPUT 2>/dev/null || true
  iptables -F OPANEL_ENT_USER 2>/dev/null || true
  iptables -F OPANEL_ENT_BLOCKLIST 2>/dev/null || true
  ip6tables -F OPANEL_ENT_INPUT 2>/dev/null || true
  ip6tables -F OPANEL_ENT_USER 2>/dev/null || true
  ip6tables -F OPANEL_ENT_BLOCKLIST 2>/dev/null || true
}

iptables_insert_managed_jumps() {
  # Order matters: OPANEL_ENT_INPUT accepts the default ports from any source, and
  # an ACCEPT inside a user chain ends the traversal. With it ahead of
  # OPANEL_ENT_USER an admin's "block this IP" never ran for ports 22/80/443/the
  # panel -- which is every port an attacker uses. Admin rules go first.
  iptables -C INPUT -j OPANEL_ENT_BLOCKLIST 2>/dev/null || iptables -I INPUT 1 -j OPANEL_ENT_BLOCKLIST
  iptables -C INPUT -j OPANEL_ENT_USER 2>/dev/null || iptables -I INPUT 2 -j OPANEL_ENT_USER
  iptables -C INPUT -j OPANEL_ENT_INPUT 2>/dev/null || iptables -I INPUT 3 -j OPANEL_ENT_INPUT
  ip6tables -C INPUT -j OPANEL_ENT_BLOCKLIST 2>/dev/null || ip6tables -I INPUT 1 -j OPANEL_ENT_BLOCKLIST
  ip6tables -C INPUT -j OPANEL_ENT_USER 2>/dev/null || ip6tables -I INPUT 2 -j OPANEL_ENT_USER
  ip6tables -C INPUT -j OPANEL_ENT_INPUT 2>/dev/null || ip6tables -I INPUT 3 -j OPANEL_ENT_INPUT
}

iptables_reorder_managed_jumps() {
  # Existing installs already have the jumps in the old order; -C above would
  # find them and leave it. Drop and re-add so the fix reaches them too.
  local binary
  for binary in iptables ip6tables; do
    "$binary" -D INPUT -j OPANEL_ENT_BLOCKLIST 2>/dev/null || true
    "$binary" -D INPUT -j OPANEL_ENT_USER 2>/dev/null || true
    "$binary" -D INPUT -j OPANEL_ENT_INPUT 2>/dev/null || true
  done
  iptables_insert_managed_jumps
}

iptables_add_default_allowances() {
  local port p
  port="$(env_get PANEL_PORT)"
  port="${port:-$DEFAULT_PANEL_PORT}"
  for p in 22 25 80 443 465 587 "$port"; do
    require_port "$p"
    iptables -A OPANEL_ENT_INPUT -p tcp --dport "$p" -j ACCEPT -m comment --comment "opanel-ent:PanelZone" 2>/dev/null \
      || iptables -A OPANEL_ENT_INPUT -p tcp --dport "$p" -j ACCEPT 2>/dev/null \
      || true
    ip6tables -A OPANEL_ENT_INPUT -p tcp --dport "$p" -j ACCEPT -m comment --comment "opanel-ent:PanelZone" 2>/dev/null \
      || ip6tables -A OPANEL_ENT_INPUT -p tcp --dport "$p" -j ACCEPT 2>/dev/null \
      || true
  done
  iptables -A OPANEL_ENT_INPUT -m state --state ESTABLISHED,RELATED -j ACCEPT 2>/dev/null || true
  iptables -A OPANEL_ENT_INPUT -i lo -j ACCEPT 2>/dev/null || true
  iptables -A OPANEL_ENT_INPUT -p icmp -j ACCEPT 2>/dev/null || true
  ip6tables -A OPANEL_ENT_INPUT -m state --state ESTABLISHED,RELATED -j ACCEPT 2>/dev/null || true
  ip6tables -A OPANEL_ENT_INPUT -i lo -j ACCEPT 2>/dev/null || true
  ip6tables -A OPANEL_ENT_INPUT -p ipv6-icmp -j ACCEPT 2>/dev/null || true
}

run_managed_iptables_command() {
  local binary="$1" op="${2:-}" chain="${3:-}" proto="" port="" network="" target=""
  local -a argv
  shift || true
  [[ $# -ge 2 ]] || deny "usage: ${binary}-run <-A|-D> OPANEL_ENT_USER ..."
  op="$1"; chain="$2"; shift 2
  [[ "$op" == "-A" || "$op" == "-D" ]] || deny "unsupported ${binary} operation: $op"
  [[ "$chain" == "OPANEL_ENT_USER" ]] || deny "unsupported ${binary} chain: $chain"
  while [[ $# -gt 0 ]]; do
    case "$1" in
      -p)
        [[ $# -ge 2 ]] || deny "missing protocol"
        proto="$2"; require_proto "$proto"; shift 2
        ;;
      --dport)
        [[ $# -ge 2 ]] || deny "missing port"
        port="$2"; require_port "$port"; shift 2
        ;;
      -s)
        [[ $# -ge 2 ]] || deny "missing source network"
        network="$2"; require_ip_or_cidr "$network"; shift 2
        ;;
      -j)
        [[ $# -ge 2 ]] || deny "missing target"
        target="$2"
        [[ "$target" == "ACCEPT" || "$target" == "DROP" ]] || deny "unsupported target: $target"
        shift 2
        ;;
      *)
        deny "unsupported ${binary} argument: $1"
        ;;
    esac
  done
  [[ -n "$target" ]] || deny "missing target"
  if [[ -n "$port" && -z "$proto" ]]; then
    deny "port rule requires protocol"
  fi
  if [[ "$binary" == "ip6tables" && -n "$network" && "$network" != *:* ]]; then
    return 0
  fi
  if [[ "$binary" == "iptables" && -n "$network" && "$network" == *:* ]]; then
    return 0
  fi
  iptables_ensure_opanel_chains
  argv=("$binary" "$op" "$chain")
  [[ -n "$proto" ]] && argv+=("-p" "$proto")
  [[ -n "$port" ]] && argv+=("--dport" "$port")
  [[ -n "$network" ]] && argv+=("-s" "$network")
  argv+=("-j" "$target")
  "${argv[@]}"
  # Without this the rule lives only in the running kernel: the on-disk snapshot
  # is taken elsewhere and never refreshed, so every admin rule vanished on the
  # next reboot while the panel kept listing it.
  firewall_persist_rules 2>/dev/null || true
}

run_managed_ipset_command() {
  [[ $# -ge 1 ]] || deny "usage: ipset-run <create|flush|add|destroy|list> ..."
  case "$1" in
    create)
      [[ $# -ge 3 ]] || deny "usage: ipset-run create <set> <type> ..."
      [[ "$2" == "$BLOCKLIST_IPSET_V4" || "$2" == "$BLOCKLIST_IPSET_V6" ]] || deny "unsupported ipset: $2"
      exec ipset "$@"
      ;;
    flush|destroy|list)
      [[ $# -eq 2 ]] || deny "usage: ipset-run $1 <set>"
      [[ "$2" == "$BLOCKLIST_IPSET_V4" || "$2" == "$BLOCKLIST_IPSET_V6" ]] || deny "unsupported ipset: $2"
      exec ipset "$@"
      ;;
    add)
      [[ $# -ge 3 ]] || deny "usage: ipset-run add <set> <network> ..."
      [[ "$2" == "$BLOCKLIST_IPSET_V4" || "$2" == "$BLOCKLIST_IPSET_V6" ]] || deny "unsupported ipset: $2"
      require_ip_or_cidr "$3"
      exec ipset "$@"
      ;;
    *)
      deny "unsupported ipset operation: $1"
      ;;
  esac
}

require_time_hhmm() {
  local value="$1" hour minute
  [[ "$value" =~ ^[0-9]{2}:[0-9]{2}$ ]] || deny "invalid time: $value"
  hour="${value%%:*}"; minute="${value##*:}"
  (( 10#$hour >= 0 && 10#$hour <= 23 )) || deny "invalid hour: $hour"
  (( 10#$minute >= 0 && 10#$minute <= 59 )) || deny "invalid minute: $minute"
}

schedule_panel_restart() {
  local unit
  systemctl daemon-reload || true
  if command -v systemd-run >/dev/null 2>&1; then
    unit="opanel-ent-api-delayed-restart-$(date +%s)"
    systemd-run --unit="$unit" --on-active=2s /bin/systemctl restart opanel-ent-api >/dev/null 2>&1 || true
  else
    (sleep 2; systemctl restart opanel-ent-api >/dev/null 2>&1 || true) >/dev/null 2>&1 &
  fi
}

refresh_tools_ols() {
  local port domain host api_scheme tools_scheme pma_secure php_version panel_cert panel_key default_ver_no_dot lsphp_sock
  port="$(env_get PANEL_PORT)"; port="${port:-$DEFAULT_PANEL_PORT}"
  domain="$(env_get PANEL_DOMAIN)"; host="${domain:-$(detect_ip)}"
  panel_cert="$(env_get PANEL_SSL_CERT)"; panel_key="$(env_get PANEL_SSL_KEY)"
  php_version="${PHP_DEFAULT:-8.4}"
  default_ver_no_dot="${php_version//./}"
  lsphp_sock="/tmp/lshttpd/lsphp${default_ver_no_dot}.sock"
  api_scheme="http"; tools_scheme="http"; pma_secure="false"
  if panel_tls_enabled; then
    api_scheme="https"; tools_scheme="https"; pma_secure="true"
  fi
  ensure_ols_conf_dir_writable
  # The IP blocklist is not part of the web configuration and has its own
  # opanel-ent-firewall-blocklist.timer plus a blocklist-apply command. Re-applying
  # it from here made every caller -- issuing SSL, refreshing a vhost -- pay for
  # a full reload of the set, which is minutes once the list reaches six figures.
  cat >"${OLS_VHOSTS_DIR}/00-opanel-ent-tools.conf" <<OLS_VHOST
docRoot                   /usr/share/phpmyadmin/
vhDomain                  ${host}
enableIpGeo               0
allowSymbolLink           1

context /.well-known/acme-challenge/ {
  type                    static
  location                /var/www/opanel-ent-acme/.well-known/acme-challenge/
  allowBrowse             1
  addDefaultCharset       off
}

context / {
  type                    null
  location                /usr/share/phpmyadmin/
  allowBrowse             1
}

extprocessor lsphp${default_ver_no_dot} {
  type                    lsapi
  address                 uds://${lsphp_sock}
  maxConns                10
  env                     PHP_LSAPI_CHILDREN=10
  initTimeout             60
  retryTimeout            0
  persistConn             1
  pcKeepAliveTimeout      1
  respBuffer              0
  autoStart               1
  path                    /usr/local/lsws/lsphp${default_ver_no_dot}/bin/lsphp
  backlog                 100
  instances               1
  extUser                 www-data
  extGroup                www-data
  runOnStartUp            1
}

scripthandler {
  add                     lsapi:lsphp${default_ver_no_dot} php
}

rewrite  {
  enable                  1
  rules                   rewriteRule ^/phpmyadmin/(.*)$ /\$1 [L]
}

vhssl  {
  keyFile                 ${panel_key:-/dev/null}
  certFile                ${panel_cert:-/dev/null}
}

phpIniOverride  {
  php_value               include_path .:/usr/share/php
  php_value               upload_max_filesize 1024M
  php_value               post_max_size 1024M
  php_value               memory_limit 512M
  php_value               max_execution_time 300
  php_value               max_input_time 600
}
OLS_VHOST
  # Replace phpMyAdmin symlinks pointing outside docRoot with actual files
  # so OLS can serve static assets (CSS, JS) without symlink restrictions
  if command -v python3 >/dev/null 2>&1; then
    python3 -c "
import pathlib, shutil
phpmyadmin = pathlib.Path('/usr/share/phpmyadmin')
for link in phpmyadmin.rglob('*'):
    if link.is_symlink():
        target = link.resolve()
        if target.exists() and not str(target).startswith(str(phpmyadmin)):
            link.unlink()
            if target.is_dir():
                shutil.copytree(str(target), str(link), symlinks=False)
            else:
                shutil.copy2(str(target), str(link))
" 2>/dev/null || true
  fi
  sed -i -E "/api\/databases\/phpmyadmin-sso/s#'[^']+/api/databases/phpmyadmin-sso/'#'${api_scheme}://127.0.0.1:${port}/api/databases/phpmyadmin-sso/'#" /usr/share/phpmyadmin/opanel-ent-signon.php 2>/dev/null || true
  sed -i -E "s#('secure' => )(true|false)#\1${pma_secure}#" /etc/phpmyadmin/conf.d/opanel-ent-signon.php /usr/share/phpmyadmin/opanel-ent-signon.php 2>/dev/null || true
  [[ -n "$host" ]] && sed -i -E "/PmaAbsoluteUri/s#'https?://[^']+/phpmyadmin/'#'${tools_scheme}://${host}/phpmyadmin/'#" /etc/phpmyadmin/conf.d/opanel-ent-signon.php 2>/dev/null || true
  local pma_signon_secret
  pma_signon_secret="$(env_get PMA_SIGNON_SECRET)"
  [[ -n "$pma_signon_secret" ]] && sed -i -E "s#(X-OPanel Enterprise-Signon-Secret: )[^']*#\1${pma_signon_secret}#" /usr/share/phpmyadmin/opanel-ent-signon.php 2>/dev/null || true
  # OLS PHP runs as www-data:opanel-ent-sites â€“ group is opanel-ent-sites (not www-data),
  # so group-readable (640) won't work.  Must be world-readable (644).
  chmod 644 /etc/phpmyadmin/conf.d/opanel-ent-signon.php 2>/dev/null || true
  fix_phpmyadmin_permissions
  ols_sync_main_config
  restart_openlitespeed 2>/dev/null || true
}

configure_unattended_upgrades() {
  local enabled="$1" mode="$2" reboot="$3" origins
  [[ "$enabled" == "on" || "$enabled" == "off" ]] || deny "enabled must be on/off"
  [[ "$mode" == "security" || "$mode" == "all" ]] || deny "mode must be security/all"
  [[ "$reboot" == "on" || "$reboot" == "off" ]] || deny "auto reboot must be on/off"

  DEBIAN_FRONTEND=noninteractive apt-get update --allow-releaseinfo-change
  DEBIAN_FRONTEND=noninteractive apt-get install -y unattended-upgrades apt-listchanges

  if [[ "$enabled" == "off" ]]; then
    cat >/etc/apt/apt.conf.d/20auto-upgrades <<'APT'
APT::Periodic::Update-Package-Lists "0";
APT::Periodic::Unattended-Upgrade "0";
APT
    systemctl disable --now unattended-upgrades.service 2>/dev/null || true
    echo "OS auto updates disabled"
    return 0
  fi

  origins='        "${distro_id}:${distro_codename}-security";'
  if [[ "$mode" == "all" ]]; then
    origins='        "${distro_id}:${distro_codename}";
        "${distro_id}:${distro_codename}-updates";
        "${distro_id}:${distro_codename}-security";'
  fi

  cat >/etc/apt/apt.conf.d/20auto-upgrades <<'APT'
APT::Periodic::Update-Package-Lists "1";
APT::Periodic::Unattended-Upgrade "1";
APT::Periodic::AutocleanInterval "7";
APT
  cat >/etc/apt/apt.conf.d/51opanel-unattended-upgrades <<APT
Unattended-Upgrade::Allowed-Origins {
${origins}
};
Unattended-Upgrade::Remove-Unused-Dependencies "true";
Unattended-Upgrade::Automatic-Reboot "$([[ "$reboot" == "on" ]] && echo true || echo false)";
Unattended-Upgrade::Automatic-Reboot-Time "03:00";
APT
  systemctl enable --now unattended-upgrades.service 2>/dev/null || true
  echo "OS auto updates enabled (${mode}, reboot=${reboot})"
}

run_os_update_now() {
  export DEBIAN_FRONTEND=noninteractive APT_LISTCHANGES_FRONTEND=none
  apt-get update --allow-releaseinfo-change
  apt-get \
    -o Dpkg::Options::=--force-confdef \
    -o Dpkg::Options::=--force-confold \
    upgrade -y
}

run_os_update() {
  local unit="opanel-ent-os-update"
  if systemctl is-active --quiet "${unit}.service"; then
    echo "OS update is already running: ${unit}.service"
    return 0
  fi
  if command -v systemd-run >/dev/null 2>&1; then
    systemd-run \
      --unit="$unit" \
      --collect \
      --description="Update OS packages for opanel-ent" \
      /bin/bash -lc 'export DEBIAN_FRONTEND=noninteractive APT_LISTCHANGES_FRONTEND=none; apt-get update --allow-releaseinfo-change; apt-get -o Dpkg::Options::=--force-confdef -o Dpkg::Options::=--force-confold upgrade -y'
    echo "OS update started: ${unit}.service"
    echo "Check progress: journalctl -u ${unit}.service -f"
    return 0
  fi
  nohup /bin/bash -lc 'export DEBIAN_FRONTEND=noninteractive APT_LISTCHANGES_FRONTEND=none; apt-get update --allow-releaseinfo-change; apt-get -o Dpkg::Options::=--force-confdef -o Dpkg::Options::=--force-confold upgrade -y' \
    >/var/log/opanel-ent-os-update.log 2>&1 &
  echo "OS update started in background. Log: /var/log/opanel-ent-os-update.log"
}

write_panel_auto_update_timer() {
  local enabled="$1" time_value="$2"
  [[ "$enabled" == "on" || "$enabled" == "off" ]] || deny "enabled must be on/off"
  require_time_hhmm "$time_value"
  if [[ "$enabled" == "off" ]]; then
    systemctl disable --now opanel-ent-auto-update.timer 2>/dev/null || true
    rm -f /etc/systemd/system/opanel-ent-auto-update.service /etc/systemd/system/opanel-ent-auto-update.timer
    systemctl daemon-reload
    echo "Panel auto update disabled"
    return 0
  fi
  [[ -f "$UPDATE_SCRIPT" ]] || deny "missing $UPDATE_SCRIPT"
  cat >/etc/systemd/system/opanel-ent-auto-update.service <<SERVICE
[Unit]
Description=Update opanel-ent from GitHub
After=network-online.target
Wants=network-online.target

[Service]
Type=oneshot
Environment=SOURCE_DIR=${SOURCE_DIR}
Environment=APP_DIR=${APP_DIR}
Environment=REPO_URL=${REPO_URL:-https://github.com/bnixvn/opanel-ent.git}
Environment=GIT_REMOTE=${GIT_REMOTE:-origin}
Environment=UPDATE_CHANNEL=${UPDATE_CHANNEL:-branch}
Environment=BRANCH=${BRANCH:-main}
Environment=RELEASE_TAG=${RELEASE_TAG:-}
Environment=RELEASE_PATTERN=${RELEASE_PATTERN:-v[0-9]*.[0-9]*.[0-9]*}
Environment=SKIP_PULL=${SKIP_PULL:-false}
ExecStart=/bin/bash ${UPDATE_SCRIPT}
SERVICE
  cat >/etc/systemd/system/opanel-ent-auto-update.timer <<TIMER
[Unit]
Description=Run opanel-ent auto update daily

[Timer]
OnCalendar=*-*-* ${time_value}:00
Persistent=true
RandomizedDelaySec=15m

[Install]
WantedBy=timers.target
TIMER
  systemctl daemon-reload
  systemctl enable --now opanel-ent-auto-update.timer
  echo "Panel auto update enabled at ${time_value}"
}

run_panel_update() {
  [[ -f "$UPDATE_SCRIPT" ]] || deny "missing $UPDATE_SCRIPT"
  local unit="opanel-ent-panel-update"
  if systemctl is-active --quiet "${unit}.service"; then
    echo "Panel update is already running: ${unit}.service"
    return 0
  fi
  if command -v systemd-run >/dev/null 2>&1; then
    systemd-run \
      --unit="$unit" \
      --collect \
      --description="Update opanel-ent from GitHub" \
      --property="Environment=SOURCE_DIR=${SOURCE_DIR}" \
      --property="Environment=APP_DIR=${APP_DIR}" \
      --property="Environment=REPO_URL=${REPO_URL:-https://github.com/bnixvn/opanel-ent.git}" \
      --property="Environment=GIT_REMOTE=${GIT_REMOTE:-origin}" \
      --property="Environment=UPDATE_CHANNEL=${UPDATE_CHANNEL:-branch}" \
      --property="Environment=BRANCH=${BRANCH:-main}" \
      --property="Environment=RELEASE_TAG=${RELEASE_TAG:-}" \
      --property="Environment=RELEASE_PATTERN=${RELEASE_PATTERN:-v[0-9]*.[0-9]*.[0-9]*}" \
      --property="Environment=SKIP_PULL=${SKIP_PULL:-false}" \
      /bin/bash "$UPDATE_SCRIPT"
    echo "Panel update started: ${unit}.service"
    echo "Check progress: journalctl -u ${unit}.service -f"
    return 0
  fi
  nohup env \
    SOURCE_DIR="$SOURCE_DIR" \
    APP_DIR="$APP_DIR" \
    REPO_URL="${REPO_URL:-https://github.com/bnixvn/opanel-ent.git}" \
    GIT_REMOTE="${GIT_REMOTE:-origin}" \
    UPDATE_CHANNEL="${UPDATE_CHANNEL:-branch}" \
    BRANCH="${BRANCH:-main}" \
    RELEASE_TAG="${RELEASE_TAG:-}" \
    RELEASE_PATTERN="${RELEASE_PATTERN:-v[0-9]*.[0-9]*.[0-9]*}" \
    SKIP_PULL="${SKIP_PULL:-false}" \
    /bin/bash "$UPDATE_SCRIPT" \
    >/var/log/opanel-ent-panel-update.log 2>&1 &
  echo "Panel update started in background. Log: /var/log/opanel-ent-panel-update.log"
}

write_modsec_base_conf() {
  install -d -o root -g root -m 0755 /usr/local/lsws/conf/opanel-ent/waf /usr/local/lsws/conf/opanel-ent/waf/sites
  {
    [[ -f /etc/modsecurity/modsecurity.conf ]] && echo "Include /etc/modsecurity/modsecurity.conf"
    echo "SecRuleEngine On"
    # libmodsecurity3 skips the whole of phase 2 when body access is off, so
    # `SecRequestBodyAccess Off` silently disabled every phase:2 rule -- not
    # just the body ones. The wp2shell block, the ?rest_route smuggling block
    # and author enumeration are all phase:2 and never fired on any site.
    #
    # The limits keep large media/plugin uploads working, and must come after
    # the distro modsecurity.conf include above, which ships a 13 MB limit with
    # SecRequestBodyLimitAction Reject: only the first 1 MB of non-file content
    # is buffered, and a body over the total limit is inspected as far as it
    # goes instead of being rejected outright.
    echo "SecRequestBodyAccess On"
    echo "SecRequestBodyLimit 134217728"
    echo "SecRequestBodyNoFilesLimit 1048576"
    echo "SecRequestBodyLimitAction ProcessPartial"
  } >/usr/local/lsws/conf/opanel-ent/waf/opanel-ent-base.conf
}

write_modsec_main_conf() {
  write_waf_default_rules
  write_modsec_base_conf
  touch /usr/local/lsws/conf/opanel-ent/waf/opanel-ent-custom.conf
  {
    echo "Include /usr/local/lsws/conf/opanel-ent/waf/opanel-ent-base.conf"
    echo "Include /usr/local/lsws/conf/opanel-ent/waf/opanel-ent-default.conf"
    echo "Include /usr/local/lsws/conf/opanel-ent/waf/opanel-ent-custom.conf"
  } >/usr/local/lsws/conf/opanel-ent/waf/opanel-ent-main.conf
}

ensure_firewall_rule_store() {
  install -d -o opanel-ent -g opanel-ent -m 0750 "$opanel_ent_DATA_DIR/firewall"
  if [[ -f "$opanel_ent_DATA_DIR/firewall/rules.json" ]]; then
    chown opanel-ent:opanel-ent "$opanel_ent_DATA_DIR/firewall/rules.json" 2>/dev/null || true
    chmod 0640 "$opanel_ent_DATA_DIR/firewall/rules.json" 2>/dev/null || true
  fi
}

write_waf_default_rules() {
  install -d -o root -g root -m 0755 /usr/local/lsws/conf/opanel-ent/waf
  cat >/usr/local/lsws/conf/opanel-ent/waf/opanel-ent-default.conf <<'RULES'
# OPanel Enterprise default WAF rules: lightweight WordPress, Laravel, and PHP probes only.
SecRule REQUEST_URI "@rx (?i)(?:/\.env(?:\.|$)|/\.user\.ini(?:\.|$)|/\.git/|/composer\.(?:json|lock)(?:$|[?])|/(?:phpinfo|info)\.php(?:$|[?])|/(?:config|database|db)\.php\.(?:bak|old|save|txt)(?:$|[?]))" "id:1001301,phase:1,deny,status:403,log,msg:'opanel-ent blocked PHP sensitive file probe'"
SecRule REQUEST_URI|ARGS "@rx (?i)(?:\.\./|\.\.\\|%2e%2e%2f|%252e%252e%252f)" "id:1001302,phase:2,deny,status:403,log,msg:'opanel-ent blocked PHP path traversal'"
SecRule REQUEST_URI "@rx (?i)(?:/(?:c99|r57|shell|cmd|wso)\.php(?:$|[?])|/vendor/phpunit/phpunit/src/Util/PHP/eval-stdin\.php(?:$|[?]))" "id:1001303,phase:1,deny,status:403,log,msg:'opanel-ent blocked PHP runtime probe'"
SecRule REQUEST_URI "@rx (?i)(?:/\.env(?:\.|$)|/artisan(?:$|[?])|/server\.php(?:$|[?])|/storage/logs/[^?]*\.log(?:$|[?])|/bootstrap/cache/[^?]*\.php(?:$|[?]))" "id:1001201,phase:1,deny,status:403,log,msg:'opanel-ent blocked Laravel sensitive path'"
SecRule REQUEST_URI "@rx (?i)(?:/_ignition/execute-solution(?:$|[?]))" "id:1001202,phase:1,deny,status:403,log,msg:'opanel-ent blocked Laravel Ignition RCE probe'"
SecRule REQUEST_URI "@rx (?i)(?:/wp-config\.php(?:\.|$|[?])|/wp-content/(?:uploads|cache|upgrade)/[^?]*\.php(?:$|[?])|/wp-admin/includes/[^?]*\.php(?:$|[?])|/wp-includes/[^?]*\.php(?:$|[?]))" "id:1001101,phase:1,deny,status:403,log,msg:'opanel-ent blocked WordPress sensitive path'"
SecRule REQUEST_URI "@rx (?i)(?:/xmlrpc\.php(?:$|[?]))" "id:1001102,phase:1,deny,status:403,log,msg:'opanel-ent blocked WordPress XML-RPC access'"
SecRule ARGS:author "@rx ^[0-9]+$" "id:1001103,phase:2,deny,status:403,log,msg:'opanel-ent blocked WordPress author enumeration'"
SecRule REQUEST_URI "@rx (?i)(?:/wp-admin/install\.php(?:$|[?])|/wp-admin/setup-config\.php(?:$|[?]))" "id:1001104,phase:1,deny,status:403,log,msg:'opanel-ent blocked WordPress installer probe'"
RULES
}

save_waf_custom_rules() {
  install -d -o root -g root -m 0755 /usr/local/lsws/conf/opanel-ent/waf
  write_waf_default_rules
  local tmp
  tmp="$(mktemp)"
  cat >"$tmp"
  if file_has_nul "$tmp"; then
    rm -f "$tmp"
    deny "WAF rules cannot contain NUL bytes"
  fi
  if [[ $(wc -c <"$tmp") -gt 65536 ]]; then
    rm -f "$tmp"
    deny "WAF custom rules must be 64 KB or smaller"
  fi
  install -m 0644 -o root -g root "$tmp" /usr/local/lsws/conf/opanel-ent/waf/opanel-ent-custom.conf
  rm -f "$tmp"
  write_modsec_main_conf
  restart_openlitespeed
  echo "WAF custom rules saved"
}

save_waf_site_rules() {
  local domain="$1" defer="${2:-}" tmp target backup=""
  require_domain "$domain"
  [[ "$defer" == "" || "$defer" == "defer" ]] || deny "invalid waf-site-save mode: $defer"
  install -d -o root -g root -m 0755 /usr/local/lsws/conf/opanel-ent/waf /usr/local/lsws/conf/opanel-ent/waf/sites
  write_modsec_base_conf
  tmp="$(mktemp)"
  cat >"$tmp"
  if file_has_nul "$tmp"; then
    rm -f "$tmp"
    deny "WAF rules cannot contain NUL bytes"
  fi
  if [[ $(wc -c <"$tmp") -gt 163840 ]]; then
    rm -f "$tmp"
    deny "WAF site rules must be 160 KB or smaller"
  fi
  target="/usr/local/lsws/conf/opanel-ent/waf/sites/${domain}.conf"
  # Nothing to do (and no reason to restart OLS) when the rendered rules are
  # byte-identical to what is already on disk -- the common case on a bulk
  # refresh where the rule set has not changed.
  if [[ -f "$target" ]] && cmp -s "$tmp" "$target"; then
    rm -f "$tmp"
    echo "WAF site rules unchanged: ${domain}"
    return 0
  fi
  if [[ -f "$target" ]]; then
    backup="${target}.bak.$(date +%s)"
    cp "$target" "$backup"
  fi
  install -m 0644 -o root -g root "$tmp" "$target"
  rm -f "$tmp"
  # "defer" skips the reload so a bulk caller can restart OLS once at the end.
  [[ "$defer" == "defer" ]] || restart_openlitespeed
  rm -f "$backup" 2>/dev/null || true
  echo "WAF site rules saved: ${domain}"
}

install_waf_engine() {
  export DEBIAN_FRONTEND=noninteractive
  if ! dpkg -s ols-modsecurity >/dev/null 2>&1; then
    apt-get update --allow-releaseinfo-change
    apt-get install -y ols-modsecurity modsecurity-crs libmodsecurity3 2>/dev/null || \
      apt-get install -y ols-modsecurity libmodsecurity3 2>/dev/null || \
      deny "Could not install ols-modsecurity"
  elif ! dpkg -s modsecurity-crs >/dev/null 2>&1; then
    apt-get update --allow-releaseinfo-change
    apt-get install -y modsecurity-crs libmodsecurity3 2>/dev/null || true
  fi
  [[ -f /usr/local/lsws/modules/mod_security.so ]] || deny "OpenLiteSpeed mod_security.so is missing"
  install -d -o root -g root -m 0755 /usr/local/lsws/conf/opanel-ent/waf /usr/local/lsws/conf/opanel-ent/waf/sites
  write_waf_default_rules
  touch /usr/local/lsws/conf/opanel-ent/waf/opanel-ent-custom.conf
  if [[ -f /etc/modsecurity/modsecurity.conf-recommended && ! -f /etc/modsecurity/modsecurity.conf ]]; then
    cp /etc/modsecurity/modsecurity.conf-recommended /etc/modsecurity/modsecurity.conf
  fi
  if [[ -f /etc/modsecurity/modsecurity.conf ]]; then
    sed -i -E 's/^SecRuleEngine .*/SecRuleEngine On/' /etc/modsecurity/modsecurity.conf
  fi
  write_modsec_main_conf
  ensure_ols_modsecurity_enabled
  ols_sync_main_config
  restart_openlitespeed
  echo "WAF engine installed with opanel-ent lightweight WordPress/Laravel/PHP rules."
}

install_clamav_engine() {
  export DEBIAN_FRONTEND=noninteractive
  apt-get update --allow-releaseinfo-change
  if ! dpkg -s clamav clamav-daemon >/dev/null 2>&1; then
    apt-get install -y clamav clamav-daemon
  fi
  # Ensure the daemon socket directory exists and the service is enabled.
  install -d -o clamav -g clamav -m 0755 /run/clamav 2>/dev/null || true
  systemctl enable --now clamav-daemon
  # Triggers an initial signature database refresh in the background.
  freshclam >/dev/null 2>&1 || true
  echo "ClamAV installed and clamav-daemon enabled."
  # Layer Linux Malware Detect on top -- its web-focused signature set catches
  # the PHP shells / injections ClamAV's general signatures miss, and it uses the
  # resident clamd as its scan engine (both signature sets, one fast scanner).
  install_lmd_engine || echo "NOTE: Linux Malware Detect not installed; ClamAV scanning still works."
}

# github.com/rfxn/linux-malware-detect. Pinned tag; update deliberately.
LMD_GIT_TAG="v2.0.1"
LMD_DIR="/usr/local/maldetect"
LMD_CONF="${LMD_DIR}/conf.maldet"

lmd_installed() { command -v maldet >/dev/null 2>&1 && [[ -f "$LMD_CONF" ]]; }

install_lmd_engine() {
  export DEBIAN_FRONTEND=noninteractive
  command -v clamdscan >/dev/null 2>&1 || { echo "LMD needs ClamAV first"; return 1; }
  if ! lmd_installed; then
    apt-get install -y git ca-certificates inotify-tools >/dev/null 2>&1 || true
    local tmp
    tmp="$(mktemp -d)" || return 1
    if ! git clone --depth 1 --branch "$LMD_GIT_TAG" \
        https://github.com/rfxn/linux-malware-detect.git "$tmp/lmd" >/dev/null 2>&1; then
      rm -rf -- "$tmp"; echo "failed to clone linux-malware-detect@${LMD_GIT_TAG}"; return 1
    fi
    ( cd "$tmp/lmd" && ./install.sh ) >/dev/null 2>&1 || { rm -rf -- "$tmp"; return 1; }
    rm -rf -- "$tmp"
  fi
  lmd_installed || { echo "maldet not on PATH after install"; return 1; }
  configure_lmd
  # LMD ships its own daily cron; opanel-ent drives the schedule instead.
  rm -f /etc/cron.d/maldet /etc/cron.daily/maldet 2>/dev/null || true
  ( maldet -u >/dev/null 2>&1; maldet -d >/dev/null 2>&1 ) &
  echo "Linux Malware Detect ${LMD_GIT_TAG} installed (engine: clamd)."
}

# Opanel's opinionated conf.maldet: use clamd, never auto-quarantine or suspend a
# user (a false positive on a legit plugin would take a live site down -- hits
# are surfaced in the panel and the admin decides), keep signatures current.
set_lmd_conf() {
  local k="$1" v="$2"
  if grep -qE "^${k}=" "$LMD_CONF"; then
    sed -i "s|^${k}=.*|${k}=\"${v}\"|" "$LMD_CONF"
  else
    printf '%s="%s"\n' "$k" "$v" >>"$LMD_CONF"
  fi
}

configure_lmd() {
  [[ -f "$LMD_CONF" ]] || return 0
  set_lmd_conf scan_clamscan 1
  set_lmd_conf clamav_scan 1
  set_lmd_conf autoupdate_signatures 1
  set_lmd_conf autoupdate_version 1
  set_lmd_conf cron_daily_scan 0
  set_lmd_conf quarantine_hits 0
  set_lmd_conf quarantine_clean 0
  set_lmd_conf quarantine_suspend_user 0
  set_lmd_conf email_alert 0
  set_lmd_conf scan_ignore_root 1
  set_lmd_conf scan_max_filesize 2048k
  set_lmd_conf scan_tmpdir /var/tmp
}

LMD_MONITOR_UNIT="/etc/systemd/system/opanel-ent-maldet-monitor.service"

enable_lmd_monitor() {
  lmd_installed || deny "Linux Malware Detect is not installed"
  # inotify watches for every file under /home -- a busy shared host has a lot.
  local wf=/etc/sysctl.d/60-opanel-ent-inotify.conf
  printf 'fs.inotify.max_user_watches=1048576\nfs.inotify.max_user_instances=1024\n' >"$wf"
  sysctl -p "$wf" >/dev/null 2>&1 || true
  cat >"$LMD_MONITOR_UNIT" <<'UNIT'
[Unit]
Description=OPanel Enterprise Linux Malware Detect real-time monitor
After=clamav-daemon.service
Wants=clamav-daemon.service

[Service]
# `maldet --monitor` stays in the foreground for as long as it watches, so it
# is a simple service, not a oneshot. As a oneshot systemd waited for it to
# exit: first killing it at the 120s timeout on a busy host and marking the
# unit failed, then -- with the timeout lifted -- sitting in "activating"
# forever, which is_active reports as not running. Either way the panel's
# real-time status was wrong while the watcher itself was fine.
Type=simple
# maldet refuses to start when another monitor is already running, and one
# outlives the unit easily -- a killed or crashed start leaves the watcher
# behind, after which every restart exits 1 with "existing monitor process
# detected". Clear any stale one first; the leading - keeps a clean start from
# failing when there is nothing to stop.
ExecStartPre=-/usr/local/sbin/maldet --monitor stop
ExecStart=/usr/local/sbin/maldet --monitor /home
ExecStop=/usr/local/sbin/maldet --monitor stop
Restart=on-failure
RestartSec=30

[Install]
WantedBy=multi-user.target
UNIT
  systemctl daemon-reload
  systemctl enable --now opanel-ent-maldet-monitor.service
  echo "LMD real-time monitor enabled for /home"
}

disable_lmd_monitor() {
  systemctl disable --now opanel-ent-maldet-monitor.service >/dev/null 2>&1 || true
  maldet --monitor stop >/dev/null 2>&1 || true
  rm -f "$LMD_MONITOR_UNIT"
  systemctl daemon-reload
  echo "LMD real-time monitor disabled"
}

update_malware_signatures() {
  freshclam >/dev/null 2>&1 || true
  if lmd_installed; then
    maldet -u >/dev/null 2>&1 || true
    maldet -d >/dev/null 2>&1 || true
  fi
  echo "malware signatures updated"
}

# --- Quarantine -------------------------------------------------------------
# opanel-ent keeps its own per-file quarantine (LMD's quarantine_hits is left off so
# a false positive never takes a live site down silently). A quarantined file is
# moved to a root-only store with its origin, owner and mode recorded, so it can
# be restored byte-for-byte or dropped for good.
QUARANTINE_DIR="${opanel_ent_DATA_DIR}/quarantine"

quarantine_dispatch() {
  install -d -o root -g root -m 0700 "$QUARANTINE_DIR" "$QUARANTINE_DIR/store"
  [[ -f "$QUARANTINE_DIR/index.json" ]] || { printf '[]' > "$QUARANTINE_DIR/index.json"; chmod 0600 "$QUARANTINE_DIR/index.json"; }
  python3 - "$QUARANTINE_DIR" "$@" <<'PY'
import hashlib, json, os, secrets, stat as stat_mod, sys, time

qdir = sys.argv[1]
store = os.path.join(qdir, "store")
index_path = os.path.join(qdir, "index.json")
action = sys.argv[2] if len(sys.argv) > 2 else "list"
args = sys.argv[3:]

# Malware lands in web content and world-writable spool dirs -- quarantine is
# limited to those. System paths are handled over SSH, not from the panel.
SAFE_PREFIXES = ("/home/", "/tmp/", "/var/tmp/", "/dev/shm/", "/var/www/")


def die(msg):
    sys.stderr.write("opanel-ent-helper: " + msg + "\n")
    sys.exit(1)


def load():
    try:
        data = json.load(open(index_path, encoding="utf-8"))
        return data if isinstance(data, list) else []
    except Exception:
        return []


def save(entries):
    tmp = index_path + ".tmp"
    with open(tmp, "w", encoding="utf-8") as fh:
        json.dump(entries, fh, indent=2, sort_keys=True)
    os.chmod(tmp, 0o600)
    os.replace(tmp, index_path)


def check_path(p):
    if not p or not p.startswith("/") or "\x00" in p or "\n" in p:
        die("bad path")
    n = os.path.normpath(p)
    if ".." in n.split("/"):
        die("path traversal not allowed")
    if not any(n.startswith(pre) for pre in SAFE_PREFIXES):
        die("not a quarantine-eligible location: " + n)
    return n


def parent_fd(path):
    return os.open(os.path.dirname(path) or "/", os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)


if action == "list":
    entries = load()
    changed = False
    for e in list(entries):
        if not os.path.isfile(os.path.join(store, e["id"] + ".bin")):
            entries.remove(e)
            changed = True
    if changed:
        save(entries)
    json.dump(entries, sys.stdout, indent=2, sort_keys=True)
    sys.stdout.write("\n")

elif action == "add":
    if not args:
        die("usage: malware-quarantine add <path> [signature]")
    src = check_path(args[0])
    signature = (args[1] if len(args) > 1 else "")[:120]
    name = os.path.basename(src)
    if not name or name in (".", ".."):
        die("bad file name")
    pfd = parent_fd(src)
    try:
        st = os.stat(name, dir_fd=pfd, follow_symlinks=False)
        if not stat_mod.S_ISREG(st.st_mode):
            die("not a regular file: " + src)
        if st.st_size > 512 * 1024 * 1024:
            die("file too large to quarantine (>512MB): " + src)
        fd = os.open(name, os.O_RDONLY | os.O_NOFOLLOW, dir_fd=pfd)
        try:
            chunks = []
            while True:
                buf = os.read(fd, 1 << 20)
                if not buf:
                    break
                chunks.append(buf)
            data = b"".join(chunks)
        finally:
            os.close(fd)
        qid = secrets.token_hex(16)
        blob = os.path.join(store, qid + ".bin")
        wfd = os.open(blob, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
        with os.fdopen(wfd, "wb") as out:
            out.write(data)
        entry = {
            "id": qid,
            "original_path": src,
            "signature": signature,
            "size": st.st_size,
            "uid": st.st_uid,
            "gid": st.st_gid,
            "mode": stat_mod.S_IMODE(st.st_mode),
            "sha256": hashlib.sha256(data).hexdigest(),
            "quarantined_at": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
        }
        # Record it before removing the original, so a crash never loses the file.
        entries = load()
        entries.insert(0, entry)
        save(entries)
        os.unlink(name, dir_fd=pfd)
    finally:
        os.close(pfd)
    json.dump(entry, sys.stdout, indent=2, sort_keys=True)
    sys.stdout.write("\n")

elif action == "restore":
    if not args:
        die("usage: malware-quarantine restore <id>")
    qid = args[0]
    if not (len(qid) == 32 and all(c in "0123456789abcdef" for c in qid)):
        die("bad id")
    entries = load()
    entry = next((e for e in entries if e["id"] == qid), None)
    if entry is None:
        die("no such quarantine entry: " + qid)
    dest = check_path(entry["original_path"])
    blob = os.path.join(store, qid + ".bin")
    if not os.path.isfile(blob):
        die("quarantined blob is missing")
    with open(blob, "rb") as fh:
        data = fh.read()
    pfd = parent_fd(dest)
    try:
        name = os.path.basename(dest)
        try:
            os.stat(name, dir_fd=pfd, follow_symlinks=False)
            die("a file already exists at the original path; not overwriting: " + dest)
        except FileNotFoundError:
            pass
        wfd = os.open(name, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW,
                      int(entry.get("mode", 0o644)), dir_fd=pfd)
        try:
            mv = memoryview(data)
            while mv:
                mv = mv[os.write(wfd, mv):]
            os.fchown(wfd, int(entry.get("uid", 0)), int(entry.get("gid", 0)))
            os.fchmod(wfd, int(entry.get("mode", 0o644)))
        finally:
            os.close(wfd)
    finally:
        os.close(pfd)
    os.unlink(blob)
    save([e for e in entries if e["id"] != qid])
    sys.stdout.write("restored " + dest + "\n")

elif action == "drop":
    if not args:
        die("usage: malware-quarantine drop <id>")
    qid = args[0]
    if not (len(qid) == 32 and all(c in "0123456789abcdef" for c in qid)):
        die("bad id")
    entries = load()
    if not any(e["id"] == qid for e in entries):
        die("no such quarantine entry: " + qid)
    blob = os.path.join(store, qid + ".bin")
    if os.path.isfile(blob):
        os.unlink(blob)
    save([e for e in entries if e["id"] != qid])
    sys.stdout.write("deleted quarantine entry " + qid + "\n")

else:
    die("unknown quarantine action: " + action)
PY
}

# Directories a full-system scan must not walk into: kernel/device trees and
# live process state (not real files), the signature database (clamd's own
# working set), and the panel's backup archives, which are multi-GB tarballs of
# files the scan already covers in place.
CLAMAV_SCAN_PRUNE_PATHS=(/proc /sys /dev /run /var/lib/clamav /var/backups/opanel-ent /snap)

# Incremental scans look back this many days of file changes; full scans run at
# least this often regardless of what the panel asks for.
LMD_INCREMENTAL_DAYS=7

run_malware_lmd_scan() {
  local scan_root="$1" mode="${2:-full}" root tmp scanid report total hits total_pre
  systemctl is-active --quiet clamav-daemon 2>/dev/null || deny "clamav-daemon is not running"
  root="$scan_root"
  [[ "$root" == "/" ]] && root=/home
  [[ -d "$root" ]] || deny "scan path is not a directory: $root"

  # A quick pre-count gives the panel a progress denominator; maldet's own
  # streamed output keeps the pipe alive while it runs.
  total_pre="$( { find "$root" -type f 2>/dev/null || true; } | wc -l | tr -d '[:space:]')"
  echo "opanel-ent-scan-total ${total_pre:-0}"
  echo "opanel-ent-scan-mode ${mode}"

  tmp="$(mktemp /tmp/opanel-ent-maldet.XXXXXX)" || deny "cannot create scan temp file"
  # A RETURN trap is not scoped to the function that sets it: it stays armed and
  # fires again when the *caller* returns, by which point this local is gone and
  # `set -u` kills the helper. run_clamav_system_scan calls this function, so
  # that fired on every LMD scan. Clear the trap as it runs, and read the path
  # defensively so a stray firing is a harmless no-op.
  trap 'rm -f "${tmp:-}"; trap - RETURN' RETURN
  if [[ "$mode" == "incremental" ]]; then
    { stdbuf -oL maldet -r "$root" "$LMD_INCREMENTAL_DAYS" 2>&1 || true; } | tee "$tmp" || true
  else
    { stdbuf -oL maldet -r "$root" 2>&1 || true; } | tee "$tmp" || true
  fi

  scanid="$(grep -oE '[0-9]{6}-[0-9]{4}\.[0-9]+' "$tmp" | tail -n1)"
  if [[ -z "$scanid" ]]; then
    echo "opanel-ent-scan-scanned ${total_pre:-0}"
    return 0
  fi
  report="$(maldet --report "$scanid" 2>/dev/null)"
  total="$(printf '%s\n' "$report" | sed -nE 's/^TOTAL FILES:[[:space:]]*([0-9]+).*/\1/p' | head -n1)"
  hits="$(printf '%s\n' "$report" | sed -nE 's/^TOTAL HITS:[[:space:]]*([0-9]+).*/\1/p' | head -n1)"

  # FILE HIT LIST rows:  {HEX}php.base64.v23eb9 : /home/x/public_html/a.php
  printf '%s\n' "$report" | sed -nE 's|^(\{[^}]*\}[^:]*) : (/.+)$|\2: \1 FOUND|p'

  echo "opanel-ent-scan-scanned ${total:-${total_pre:-0}}"
  echo "opanel-ent-scan-report ${scanid}"
  [[ "${hits:-0}" -eq 0 ]] || return 1
  return 0
}

run_clamav_system_scan() {
  local scan_root="$1" mode="${2:-full}" list total prune=() path rc=0
  # LMD (LMD + ClamAV signatures, clamd as the engine, incremental support)
  # drives the website-tree scan. A whole-server scan also has to walk /usr,
  # /var, /etc and friends, which maldet -r is not built for, so "/" stays on
  # clamdscan with the prune list below.
  if lmd_installed && [[ "$scan_root" != "/" ]]; then
    run_malware_lmd_scan "$scan_root" "$mode" || rc=$?
    return "$rc"
  fi
  command -v clamdscan >/dev/null 2>&1 || deny "clamdscan is not installed"
  systemctl is-active --quiet clamav-daemon 2>/dev/null || deny "clamav-daemon is not running"

  for path in "${CLAMAV_SCAN_PRUNE_PATHS[@]}"; do
    prune+=(-path "$path" -o)
  done

  list="$(mktemp /tmp/opanel-ent-clamav-list.XXXXXX)" || deny "cannot create scan list"
  trap 'rm -f "${list:-}"; trap - RETURN' RETURN
  find "$scan_root" \( "${prune[@]}" -false \) -prune -o -type f -print >"$list" 2>/dev/null || true
  total="$(wc -l <"$list" | tr -d "[:space:]")"
  # The panel reads this first line to size its progress bar; clamdscan itself
  # never reports a total.
  echo "opanel-ent-scan-total ${total:-0}"
  [[ "${total:-0}" -gt 0 ]] || { echo "opanel-ent-scan-empty"; return 0; }

  # stdbuf keeps the per-file lines flowing so progress updates while the scan
  # runs instead of arriving in one block at the end.
  stdbuf -oL clamdscan --fdpass --stdout --file-list="$list"
}

ensure_litespeed_php74_repo() {
  install -d -o root -g root -m 0755 /etc/apt/preferences.d /etc/apt/sources.list.d
  cat >/etc/apt/sources.list.d/opanel-ent-lsphp74-jammy.list <<'EOF'
deb http://hk.archive.ubuntu.com/ubuntu jammy main universe multiverse restricted
deb http://hk.archive.ubuntu.com/ubuntu jammy-updates main universe multiverse restricted
deb http://security.ubuntu.com/ubuntu jammy-security main universe multiverse restricted
deb [trusted=yes] http://rpms.litespeedtech.com/debian/ jammy main
EOF
  cat >/etc/apt/preferences.d/opanel-ent-lsphp74-jammy.pref <<'EOF'
Package: *
Pin: release n=jammy
Pin-Priority: 50

Package: lsphp74*
Pin: release n=jammy
Pin-Priority: 990

Package: libicu70 mime-support libmagickcore-6.q16-6 libmagickwand-6.q16-6 libtiff5
Pin: release n=jammy*
Pin-Priority: 990
EOF
  apt-get update --allow-releaseinfo-change
}

install_php_version() {
  local version="$1" lsphp_ver ini_dir
  export DEBIAN_FRONTEND=noninteractive
  require_php_version "$version"
  lsphp_ver="${version//./}"
  if [[ -x "/usr/local/lsws/lsphp${lsphp_ver}/bin/lsphp" ]]; then
    echo "LSPHP $version is already installed; ensuring opanel-ent extension set..."
  fi
  if [[ "$version" == "7.4" ]]; then
    ensure_litespeed_php74_repo
  elif ! apt-cache show "lsphp${lsphp_ver}" >/dev/null 2>&1; then
    echo "Refreshing LiteSpeed package metadata for LSPHP $version..."
    curl -fsSL --connect-timeout 15 --max-time 120 https://repo.litespeed.sh | bash
    apt-get update --allow-releaseinfo-change
  fi
  echo "Installing LSPHP $version..."
  local packages=(
    "lsphp${lsphp_ver}"
    "lsphp${lsphp_ver}-common"
    "lsphp${lsphp_ver}-mysql"
    "lsphp${lsphp_ver}-sqlite3"
    "lsphp${lsphp_ver}-curl"
    "lsphp${lsphp_ver}-opcache"
    "lsphp${lsphp_ver}-intl"
    "lsphp${lsphp_ver}-redis"
    "lsphp${lsphp_ver}-imagick"
  )
  local available_packages=() missing_packages=() package
  for package in "${packages[@]}"; do
    if apt-cache show "$package" >/dev/null 2>&1; then
      available_packages+=("$package")
    else
      missing_packages+=("$package")
    fi
  done
  if [[ ${#missing_packages[@]} -gt 0 ]]; then
    echo "Skipping PHP packages not available in repo: ${missing_packages[*]}"
  fi
  [[ ${#available_packages[@]} -gt 0 ]] || deny "No LSPHP package found for PHP ${version}"
  apt-get install -y "${available_packages[@]}" || { echo "Failed to install LSPHP $version"; return 1; }
  ini_dir="/usr/local/lsws/lsphp${lsphp_ver}/etc/php/${version}/mods-available"
  install -d -o root -g root -m 0755 "$ini_dir"
  cat >"${ini_dir}/99-opanel-ent.ini" <<INI
upload_max_filesize = 1024M
post_max_size = 1024M
memory_limit = 1024M
max_execution_time = 300
max_input_time = 600
max_input_vars = 10000
max_file_uploads = 100
INI
  chown root:root "${ini_dir}/99-opanel-ent.ini"
  chmod 0644 "${ini_dir}/99-opanel-ent.ini"
  install_ioncube_loader "$version"
  # Enable and start OLS (which manages lsphp)
  restart_openlitespeed 2>/dev/null || true
  echo "LSPHP $version installed successfully"
}

install_ioncube_loader() {
  local version="$1" arch url tmp archive loader target_dir target loader_ini_dir
  require_php_version "$version"
  arch="$(dpkg --print-architecture 2>/dev/null || uname -m)"
  case "$arch" in
    amd64|x86_64)
      url="https://downloads.ioncube.com/loader_downloads/ioncube_loaders_lin_x86-64.tar.gz"
      ;;
    *)
      echo "Skipping ionCube Loader: unsupported architecture ${arch}"
      return 0
      ;;
  esac

  apt-get install -y ca-certificates curl tar >/dev/null
  tmp="$(mktemp -d)" || deny "cannot create ionCube temporary directory"
  archive="${tmp}/ioncube_loaders.tar.gz"
  if ! curl -fsSL --connect-timeout 10 --max-time 300 "$url" -o "$archive"; then
    rm -rf -- "$tmp"
    deny "failed to download ionCube Loader"
  fi
  if ! tar -xzf "$archive" -C "$tmp"; then
    rm -rf -- "$tmp"
    deny "failed to unpack ionCube Loader"
  fi
  loader="${tmp}/ioncube/ioncube_loader_lin_${version}.so"
  if [[ ! -f "$loader" ]]; then
    rm -rf -- "$tmp"
    echo "Skipping ionCube Loader: no loader found for PHP ${version}"
    return 0
  fi

  target_dir="/usr/local/ioncube"
  target="${target_dir}/ioncube_loader_lin_${version}.so"
  install -d -o root -g root -m 0755 "$target_dir"
  install -m 0644 -o root -g root "$loader" "$target"
  rm -rf -- "$tmp"

  for loader_ini_dir in /etc/php/"$version"/cli/conf.d /usr/local/lsws/lsphp${version//./}/etc/php/"$version"/mods-available; do
    [[ -d "$loader_ini_dir" ]] || continue
    printf 'zend_extension=%s\n' "$target" >"${loader_ini_dir}/00-ioncube.ini"
    chown root:root "${loader_ini_dir}/00-ioncube.ini"
    chmod 0644 "${loader_ini_dir}/00-ioncube.ini"
  done

  if command -v "php${version}" >/dev/null 2>&1; then
    if ! "php${version}" -v 2>&1 | grep -qi 'ionCube'; then
      rm -f /etc/php/"$version"/cli/conf.d/00-ioncube.ini /usr/local/lsws/lsphp${version//./}/etc/php/"$version"/mods-available/00-ioncube.ini
      deny "ionCube Loader failed to load for PHP ${version}"
    fi
  fi
  echo "ionCube Loader enabled for PHP ${version}"
}

validate_php_config_file() {
  local file="$1" line key value
  while IFS= read -r line || [[ -n "$line" ]]; do
    line="${line#"${line%%[![:space:]]*}"}"
    line="${line%"${line##*[![:space:]]}"}"
    [[ -z "$line" || "$line" == \;* ]] && continue
    case "$line" in *$'\r'*) deny "PHP config contains a carriage return" ;; esac
    [[ "$line" == *"="* ]] || deny "invalid PHP config line: $line"
    key="$(printf '%s' "${line%%=*}" | xargs)"
    value="$(printf '%s' "${line#*=}" | xargs)"
    case "$key" in
      display_errors)
        [[ "$value" == "On" || "$value" == "Off" ]] || deny "invalid display_errors value"
        ;;
      memory_limit|upload_max_filesize|post_max_size)
        [[ "$value" =~ ^[0-9]{1,6}[KMG]?$ ]] || deny "invalid PHP size value for $key"
        ;;
      max_execution_time|max_input_time)
        [[ "$value" =~ ^[0-9]{1,4}$ ]] || deny "invalid integer value for $key"
        (( 10#$value >= 1 && 10#$value <= 3600 )) || deny "$key out of range"
        ;;
      max_input_vars|max_file_uploads)
        [[ "$value" =~ ^[0-9]{1,7}$ ]] || deny "invalid integer value for $key"
        (( 10#$value >= 1 && 10#$value <= 1000000 )) || deny "$key out of range"
        ;;
      opcache.enable|opcache.enable_cli|opcache.validate_timestamps|opcache.save_comments)
        [[ "$value" == "0" || "$value" == "1" ]] || deny "invalid boolean value for $key"
        ;;
      opcache.memory_consumption|opcache.interned_strings_buffer|opcache.max_accelerated_files|opcache.revalidate_freq|lsapi_children|lsapi_max_idle|lsapi_max_idle_children|lsapi_max_process_time)
        [[ "$value" =~ ^[0-9]{1,7}$ ]] || deny "invalid integer value for $key"
        ;;
      opcache.jit)
        [[ "$value" =~ ^[A-Za-z0-9_-]{0,32}$ ]] || deny "invalid opcache.jit value"
        ;;
      opcache.jit_buffer_size)
        [[ "$value" =~ ^[0-9]{1,6}M?$ ]] || deny "invalid opcache.jit_buffer_size value"
        ;;
      *)
        deny "unsupported PHP config directive: $key"
        ;;
    esac
  done <"$file"
}

write_php_config() {
  local version="$1" conf_dir target tmp size
  require_php_version "$version"
  conf_dir="/usr/local/lsws/lsphp${version//./}/etc/php/${version}/mods-available"
  target="${conf_dir}/99-opanel-ent.ini"
  install -d -o root -g root -m 0755 "$conf_dir"
  tmp="$(mktemp "${conf_dir}/.99-opanel-ent.ini.XXXXXX")" || deny "cannot create temporary PHP config"
  if ! cat >"$tmp"; then
    rm -f -- "$tmp"
    deny "failed to read PHP config"
  fi
  size="$(wc -c <"$tmp" | tr -d '[:space:]')"
  if (( size <= 0 || size > 8192 )); then
    rm -f -- "$tmp"
    deny "PHP config size out of range"
  fi
  validate_php_config_file "$tmp"
  chown root:root "$tmp"
  chmod 0644 "$tmp"
  mv -f -- "$tmp" "$target"
  restart_openlitespeed
  echo "PHP ${version} config updated: ${target}"
}

waf_status() {
  echo "ModSecurity module:"
  if /usr/local/lsws/bin/lswsctrl status 2>&1 | grep -qi modsecurity || [[ -d /usr/local/lsws/conf/opanel-ent/waf ]]; then
    echo "  installed"
  else
    echo "  not installed"
  fi
  echo "Rules file:"
  [[ -f /usr/local/lsws/conf/opanel-ent/waf/opanel-ent-main.conf ]] && echo "  /usr/local/lsws/conf/opanel-ent/waf/opanel-ent-main.conf" || echo "  missing"
  echo "Default rules:"
  [[ -f /usr/local/lsws/conf/opanel-ent/waf/opanel-ent-default.conf ]] && echo "  /usr/local/lsws/conf/opanel-ent/waf/opanel-ent-default.conf" || echo "  missing"
  echo "Custom rules:"
  [[ -f /usr/local/lsws/conf/opanel-ent/waf/opanel-ent-custom.conf ]] && echo "  /usr/local/lsws/conf/opanel-ent/waf/opanel-ent-custom.conf" || echo "  missing"
  echo "Managed profile:"
  echo "  opanel-ent built-in lightweight WordPress/Laravel/PHP rules"
  echo "Timers:"
  systemctl list-timers opanel-ent-auto-update.timer apt-daily-upgrade.timer --no-pager 2>/dev/null || true
}

audit_log() {
  local quoted="" arg
  for arg in "$@"; do
    printf -v quoted '%s %q' "$quoted" "$arg"
  done
  if command -v logger >/dev/null 2>&1; then
    logger -t opanel-ent-helper -- "cmd=${cmd:-unknown}${quoted}"
  fi
}

run_ip_rule() {
  local action="$1" network="$2" port="${3:-}" protocol="${4:-tcp}"
  require_ip_or_cidr "$network"
  case "$action" in
    allow|deny) ;;
    *) deny "invalid firewall action: $action" ;;
  esac
  local target
  if [[ "$action" == "allow" ]]; then
    target="ACCEPT"
  else
    target="DROP"
  fi
  if [[ -z "$port" ]]; then
    iptables -A OPANEL_ENT_USER -s "$network" -j "$target" -m comment --comment "opanel-ent:UserZone" 2>/dev/null \
      || iptables -A OPANEL_ENT_USER -s "$network" -j "$target" 2>/dev/null \
      || true
    return 0
  fi
  require_port "$port"; require_proto "$protocol"
  iptables -A OPANEL_ENT_USER -s "$network" -p "$protocol" --dport "$port" -j "$target" -m comment --comment "opanel-ent:UserZone" 2>/dev/null \
    || iptables -A OPANEL_ENT_USER -s "$network" -p "$protocol" --dport "$port" -j "$target" 2>/dev/null \
    || true
}

require_url() {
  local value="$1"
  [[ "$value" =~ ^https?://[^[:space:]]+$ ]] || deny "invalid URL: $value"
}

firewall_blocklist_urls() {
  ensure_opanel_data_dir
  touch "$FIREWALL_BLOCKLIST_URLS"
  sed '/^[[:space:]]*$/d' "$FIREWALL_BLOCKLIST_URLS" | sort -u
}

firewall_blocklist_write_timer() {
  cat >/etc/systemd/system/opanel-ent-firewall-blocklist.service <<SERVICE
[Unit]
Description=Refresh opanel-ent IP blocklists
After=network-online.target
Wants=network-online.target

[Service]
Type=oneshot
Environment=SUDO_USER=opanel-ent
ExecStart=/usr/local/sbin/opanel-ent-helper blocklist-run
SERVICE
  cat >/etc/systemd/system/opanel-ent-firewall-blocklist.timer <<TIMER
[Unit]
Description=Refresh opanel-ent IP blocklists daily

[Timer]
OnCalendar=*-*-* 01:00:00
Persistent=true

[Install]
WantedBy=timers.target
TIMER
  systemctl daemon-reload
  systemctl enable --now opanel-ent-firewall-blocklist.timer >/dev/null 2>&1 || true
}

firewall_blocklist_apply() {
  local v4_count=0 v6_count=0 v4_max=65536 v6_max=65536
  local v4_new="${BLOCKLIST_IPSET_V4}_new" v6_new="${BLOCKLIST_IPSET_V6}_new"
  local v4_target="$v4_new" v6_target="$v6_new"
  ensure_ols_conf_dir_writable
  install -d -o root -g root -m 0755 "$BLOCKLIST_DIR"
  iptables -N OPANEL_ENT_BLOCKLIST 2>/dev/null || true
  ip6tables -N OPANEL_ENT_BLOCKLIST 2>/dev/null || true
  iptables -C INPUT -j OPANEL_ENT_BLOCKLIST 2>/dev/null || iptables -I INPUT 1 -j OPANEL_ENT_BLOCKLIST 2>/dev/null || true
  ip6tables -C INPUT -j OPANEL_ENT_BLOCKLIST 2>/dev/null || ip6tables -I INPUT 1 -j OPANEL_ENT_BLOCKLIST 2>/dev/null || true
  if [[ -s "${BLOCKLIST_DIR}/blocklist.set" ]]; then
    v4_count="$(awk 'NF && $0 !~ /^[[:space:]]*#/ && index($0, ":") == 0 { count++ } END { print count + 0 }' "${BLOCKLIST_DIR}/blocklist.set")"
    v6_count="$(awk 'NF && $0 !~ /^[[:space:]]*#/ && index($0, ":") > 0 { count++ } END { print count + 0 }' "${BLOCKLIST_DIR}/blocklist.set")"
    v4_max=$(( v4_count + v4_count / 4 + 1024 ))
    v6_max=$(( v6_count + v6_count / 4 + 1024 ))
    (( v4_max < 65536 )) && v4_max=65536
    (( v6_max < 65536 )) && v6_max=65536
  fi
  ipset create "$BLOCKLIST_IPSET_V4" hash:net family inet hashsize 32768 maxelem "$v4_max" -exist 2>/dev/null || true
  ipset create "$BLOCKLIST_IPSET_V6" hash:net family inet6 hashsize 32768 maxelem "$v6_max" -exist 2>/dev/null || true
  ipset destroy "$v4_new" 2>/dev/null || true
  ipset destroy "$v6_new" 2>/dev/null || true
  ipset create "$v4_new" hash:net family inet hashsize 32768 maxelem "$v4_max" 2>/dev/null || true
  ipset create "$v6_new" hash:net family inet6 hashsize 32768 maxelem "$v6_max" 2>/dev/null || true
  if ! ipset list "$v4_new" >/dev/null 2>&1; then
    v4_target="$BLOCKLIST_IPSET_V4"
    ipset flush "$v4_target" 2>/dev/null || true
  fi
  if ! ipset list "$v6_new" >/dev/null 2>&1; then
    v6_target="$BLOCKLIST_IPSET_V6"
    ipset flush "$v6_target" 2>/dev/null || true
  fi
  if [[ -s "${BLOCKLIST_DIR}/blocklist.set" ]]; then
    # A per-line `ipset add` loop forks one process per entry; with a
    # 100k+ line blocklist that turns a few seconds of work into minutes,
    # and this function can run several times in one "Update panel now"
    # pass. `ipset restore` loads the whole batch in a single process.
    awk -v v4="$v4_target" -v v6="$v6_target" '
      NF && $0 !~ /^[[:space:]]*#/ {
        if (index($0, ":") > 0) print "add " v6 " " $0
        else print "add " v4 " " $0
      }
    ' "${BLOCKLIST_DIR}/blocklist.set" | ipset restore -exist 2>/dev/null || true
  fi
  if [[ "$v4_target" == "$v4_new" ]]; then
    ipset swap "$v4_new" "$BLOCKLIST_IPSET_V4" 2>/dev/null || true
    ipset destroy "$v4_new" 2>/dev/null || true
  fi
  if [[ "$v6_target" == "$v6_new" ]]; then
    ipset swap "$v6_new" "$BLOCKLIST_IPSET_V6" 2>/dev/null || true
    ipset destroy "$v6_new" 2>/dev/null || true
  fi
  if ! iptables -C OPANEL_ENT_BLOCKLIST -m set --match-set "$BLOCKLIST_IPSET_V4" src -j DROP 2>/dev/null; then
    iptables -I OPANEL_ENT_BLOCKLIST 1 -m set --match-set "$BLOCKLIST_IPSET_V4" src -j DROP 2>/dev/null || true
  fi
  if ! ip6tables -C OPANEL_ENT_BLOCKLIST -m set --match-set "$BLOCKLIST_IPSET_V6" src -j DROP 2>/dev/null; then
    ip6tables -I OPANEL_ENT_BLOCKLIST 1 -m set --match-set "$BLOCKLIST_IPSET_V6" src -j DROP 2>/dev/null || true
  fi
  ensure_firewall_rule_store >/dev/null 2>&1 || true
}

firewall_blocklist_status() {
  ensure_opanel_data_dir
  touch "$FIREWALL_BLOCKLIST_URLS"
  echo "URLs:"
  if [[ -s "$FIREWALL_BLOCKLIST_URLS" ]]; then
    firewall_blocklist_urls | sed 's/^/  /'
  else
    echo "  (none)"
  fi
  echo ""
  echo "Engine:"
  echo "  ipset"
  echo "Rules file:"
  [[ -f "${BLOCKLIST_DIR}/blocklist.set" ]] && echo "  ${BLOCKLIST_DIR}/blocklist.set" || echo "  missing"
  echo ""
  echo "ipset:"
  local total4 total6
  total4="$(ipset list "$BLOCKLIST_IPSET_V4" 2>/dev/null | grep -Ec '^[0-9]' || true)"
  total6="$(ipset list "$BLOCKLIST_IPSET_V6" 2>/dev/null | grep -Ec '^[0-9a-fA-F:]+/' || true)"
  echo "  ${BLOCKLIST_IPSET_V4}: ${total4:-0} network(s)"
  echo "  ${BLOCKLIST_IPSET_V6}: ${total6:-0} network(s)"
  echo ""
  echo "Timer:"
  systemctl is-enabled opanel-ent-firewall-blocklist.timer 2>/dev/null || true
  systemctl list-timers opanel-ent-firewall-blocklist.timer --no-pager 2>/dev/null || true
}

firewall_blocklist_clear_rules() {
  ipset flush "$BLOCKLIST_IPSET_V4" 2>/dev/null || true
  ipset flush "$BLOCKLIST_IPSET_V6" 2>/dev/null || true
}

firewall_blocklist_run() {
  ensure_opanel_data_dir
  touch "$FIREWALL_BLOCKLIST_URLS"
  local tmp fetched rules_tmp count url old_work old_rules
  tmp="$(mktemp)"
  fetched="$(mktemp)"
  rules_tmp="$(mktemp)"
  old_work="$(mktemp)"
  old_rules="$(mktemp)"
  [[ -f "$FIREWALL_BLOCKLIST_WORK" ]] && cp "$FIREWALL_BLOCKLIST_WORK" "$old_work" || true
  [[ -f "${BLOCKLIST_DIR}/blocklist.set" ]] && cp "${BLOCKLIST_DIR}/blocklist.set" "$old_rules" || true
  local fetch_failures=0
  while IFS= read -r url; do
    [[ -n "$url" ]] || continue
    require_url "$url"
    if ! curl -fsSL --connect-timeout 10 --max-time 30 "$url" >>"$fetched"; then
      echo "WARNING: could not fetch $url" >&2
      fetch_failures=$((fetch_failures + 1))
    fi
    printf '\n' >>"$fetched"
  done < <(firewall_blocklist_urls)
  python3 - "$fetched" "$tmp" "$rules_tmp" <<'PY'
import ipaddress
import re
import sys

seen = set()
networks = []
for raw in open(sys.argv[1], encoding="utf-8", errors="ignore"):
    line = re.split(r"[\s#;,]+", raw.strip(), 1)[0]
    if not line:
        continue
    try:
        value = str(ipaddress.ip_network(line, strict=False))
    except ValueError:
        continue
    network = ipaddress.ip_network(value, strict=False)
    if (
        network.is_loopback
        or network.is_private
        or network.is_link_local
        or network.is_multicast
        or network.is_reserved
        or network.is_unspecified
    ):
        continue
    if value not in seen:
        seen.add(value)
        networks.append(value)

with open(sys.argv[2], "w", encoding="utf-8") as handle:
    for value in networks:
        handle.write(value + "\n")

with open(sys.argv[3], "w", encoding="utf-8") as handle:
    handle.write("# Managed by opanel-ent. Generated from URL IP blocklists.\n")
    handle.write("# Loaded into opanel_ent_blocklist4/opanel_ent_blocklist6 ipsets.\n")
    for value in networks:
        handle.write(value + "\n")
PY
  # A transient DNS or network failure used to overwrite the stored list with
  # an empty one and flush the ipset, so one bad night wiped every blocked
  # network until the next successful run. Keep what we have instead.
  local new_count old_count
  new_count="$(sed '/^[[:space:]]*$/d' "$tmp" | wc -l | tr -d '[:space:]')"
  old_count="$(sed '/^[[:space:]]*$/d' "$old_work" 2>/dev/null | wc -l | tr -d '[:space:]')"
  if (( fetch_failures > 0 )) && (( new_count * 2 < old_count )); then
    rm -f "$tmp" "$fetched" "$rules_tmp" "$old_work" "$old_rules"
    deny "blocklist refresh aborted: ${fetch_failures} source(s) unreachable and the result (${new_count}) is far below the stored list (${old_count}); keeping the existing blocklist"
  fi
  install -d -o root -g root -m 0755 "$BLOCKLIST_DIR"
  install -m 0644 -o root -g root "$rules_tmp" "${BLOCKLIST_DIR}/blocklist.set"
  install -m 0644 -o root -g root "$tmp" "$FIREWALL_BLOCKLIST_WORK"
  firewall_blocklist_apply
  count="$(sed '/^[[:space:]]*$/d' "$FIREWALL_BLOCKLIST_WORK" | wc -l | tr -d '[:space:]')"
  firewall_blocklist_write_timer
  rm -f "$tmp" "$fetched" "$rules_tmp" "$old_work" "$old_rules"
  echo "Blocklist refreshed: ${count} network(s)"
}

firewall_blocklist_add_url() {
  local url="$1"
  require_url "$url"
  ensure_opanel_data_dir
  touch "$FIREWALL_BLOCKLIST_URLS"
  if ! grep -Fxq -- "$url" "$FIREWALL_BLOCKLIST_URLS"; then
    printf '%s\n' "$url" >>"$FIREWALL_BLOCKLIST_URLS"
  fi
  sort -u -o "$FIREWALL_BLOCKLIST_URLS" "$FIREWALL_BLOCKLIST_URLS"
  firewall_blocklist_write_timer
  echo "Blocklist URL added"
}

firewall_blocklist_delete_url() {
  local url="$1"
  require_url "$url"
  ensure_opanel_data_dir
  touch "$FIREWALL_BLOCKLIST_URLS"
  grep -Fxv -- "$url" "$FIREWALL_BLOCKLIST_URLS" >"${FIREWALL_BLOCKLIST_URLS}.tmp" || true
  mv -f "${FIREWALL_BLOCKLIST_URLS}.tmp" "$FIREWALL_BLOCKLIST_URLS"
  firewall_blocklist_write_timer
  echo "Blocklist URL removed"
}

write_ssl_auto_renew_timer() {
  cat >/etc/systemd/system/opanel-ent-ssl-auto-renew.service <<SERVICE
[Unit]
Description=Renew opanel-ent SSL certificates that expire within 10 days
After=network-online.target
Wants=network-online.target

[Service]
Type=oneshot
Environment=SUDO_USER=opanel-ent
ExecStart=/usr/local/sbin/opanel-ent-helper certbot-renew-soon 10
SERVICE
  cat >/etc/systemd/system/opanel-ent-ssl-auto-renew.timer <<TIMER
[Unit]
Description=Check opanel-ent SSL certificates daily

[Timer]
OnCalendar=*-*-* 01:30:00
Persistent=true

[Install]
WantedBy=timers.target
TIMER
  systemctl daemon-reload
  systemctl enable --now opanel-ent-ssl-auto-renew.timer >/dev/null 2>&1 || true
}

copy_panel_live_certificate() {
  local domain="$1"
  [[ -n "$domain" ]] || return 0
  is_domain "$domain" || return 0
  # /etc/opanel-ent/panel-*.pem is the panel's OWN certificate -- it backs the panel
  # port and the phpMyAdmin tools vhost, which are served for the panel hostname
  # only. A website's "Install SSL" (certbot-issue <site-domain>) must never
  # overwrite it, or phpMyAdmin SSO breaks with an SNI cert mismatch. Only copy
  # when the issued domain IS the panel's hostname.
  local panel_host
  panel_host="$(env_get PANEL_DOMAIN)"
  if [[ -z "$panel_host" ]]; then
    panel_host="$(env_get PANEL_URL)"
    panel_host="${panel_host#*://}"; panel_host="${panel_host%%/*}"; panel_host="${panel_host%%:*}"
  fi
  [[ -n "$panel_host" && "$domain" == "$panel_host" ]] || return 0
  [[ "$(panel_url_scheme)" == "https" ]] || return 0
  [[ -f "/etc/letsencrypt/live/${domain}/fullchain.pem" && -f "/etc/letsencrypt/live/${domain}/privkey.pem" ]] || return 0
  install -d -o root -g opanel-ent -m 0750 /etc/opanel-ent
  install -m 0640 -o root -g opanel-ent "/etc/letsencrypt/live/${domain}/fullchain.pem" /etc/opanel-ent/panel-fullchain.pem
  install -m 0640 -o root -g opanel-ent "/etc/letsencrypt/live/${domain}/privkey.pem" /etc/opanel-ent/panel-privkey.pem
  if [[ -f "$ENV_FILE" ]]; then
    env_set PANEL_SSL_CERT "/etc/opanel-ent/panel-fullchain.pem"
    env_set PANEL_SSL_KEY "/etc/opanel-ent/panel-privkey.pem"
  fi
}

# ---- panel certificate store -----------------------------------------------
# The panel serves HTTPS itself and picks a certificate per SNI hostname, so it
# needs every site certificate in a directory it can read. It runs as `opanel-ent`
# and /etc/letsencrypt is root-only (0700), so certificates are mirrored here as
# root:opanel-ent 0640 -- the same arrangement panel-ssl-install already used for the
# single panel certificate, just one directory per domain.
PANEL_CERT_STORE="/etc/opanel-ent/certs"

panel_cert_store_put() {
  local name="$1" cert="$2" key="$3"
  [[ -f "$cert" && -f "$key" ]] || return 0
  install -d -o root -g opanel-ent -m 0750 "$PANEL_CERT_STORE"
  install -d -o root -g opanel-ent -m 0750 "${PANEL_CERT_STORE}/${name}"
  install -m 0640 -o root -g opanel-ent "$cert" "${PANEL_CERT_STORE}/${name}/fullchain.pem"
  install -m 0640 -o root -g opanel-ent "$key" "${PANEL_CERT_STORE}/${name}/privkey.pem"
}

# A self-signed fallback so the panel is never served over plain HTTP. Browsers
# will warn on it, which is correct: it is a placeholder until a real
# certificate exists, not a substitute for one.
panel_self_signed_ensure() {
  local dir="${PANEL_CERT_STORE}/_default"
  local cert="${dir}/fullchain.pem" key="${dir}/privkey.pem"
  if [[ -f "$cert" && -f "$key" ]] && openssl x509 -checkend 2592000 -noout -in "$cert" >/dev/null 2>&1; then
    return 0
  fi
  local cn tmp
  cn="$(env_get PANEL_DOMAIN)"
  [[ -n "$cn" ]] || cn="$(hostname -f 2>/dev/null || hostname)"
  [[ -n "$cn" ]] || cn="opanel-ent.local"
  tmp="$(mktemp -d /tmp/opanel-ent-selfsigned.XXXXXX)"
  if openssl req -x509 -newkey rsa:2048 -nodes -days 3650        -keyout "${tmp}/privkey.pem" -out "${tmp}/fullchain.pem"        -subj "/CN=${cn}" -addext "subjectAltName=DNS:${cn}" >/dev/null 2>&1; then
    panel_cert_store_put "_default" "${tmp}/fullchain.pem" "${tmp}/privkey.pem"
    echo "Generated self-signed panel certificate for ${cn}"
  else
    echo "WARNING: could not generate a self-signed panel certificate" >&2
  fi
  rm -rf "$tmp"
}

# Mirror every Let's Encrypt certificate into the store and drop entries whose
# certificate is gone, so a site that lost its SSL stops being offered a stale one.
panel_cert_store_sync() {
  local live name
  panel_self_signed_ensure
  install -d -o root -g opanel-ent -m 0750 "$PANEL_CERT_STORE"
  for live in /etc/letsencrypt/live/*/; do
    [[ -d "$live" ]] || continue
    name="$(basename "$live")"
    is_domain "$name" || continue
    panel_cert_store_put "$name" "${live}fullchain.pem" "${live}privkey.pem"
  done
  for live in "${PANEL_CERT_STORE}"/*/; do
    [[ -d "$live" ]] || continue
    name="$(basename "$live")"
    [[ "$name" == "_default" ]] && continue
    if [[ ! -f "/etc/letsencrypt/live/${name}/fullchain.pem" ]]; then
      rm -rf "${PANEL_CERT_STORE:?}/${name}"
    fi
  done
}

install_manual_ssl() {
  local domain="$1" base tmpdir
  require_domain "$domain"
  base="/usr/local/lsws/conf/opanel-ent/ssl/sites/${domain}"
  tmpdir="$(mktemp -d /tmp/opanel-ent-manual-ssl.XXXXXX)"
  trap 'rm -rf "${tmpdir:-}"; trap - RETURN' RETURN
  local payload_file="$tmpdir/payload.json"
  cat >"$payload_file"
  python3 - "$tmpdir" "$payload_file" <<'PY'
import json
import pathlib
import sys

tmpdir = pathlib.Path(sys.argv[1])
payload_file = pathlib.Path(sys.argv[2])
data = json.loads(payload_file.read_text(encoding="utf-8"))
parts = {
    "cert.crt": data.get("certificate", ""),
    "privkey.key": data.get("private_key", ""),
}
ca_bundle = data.get("ca_bundle", "")
if ca_bundle:
    parts["ca.crt"] = ca_bundle
for name, content in parts.items():
    if not content or "\x00" in content:
        raise SystemExit(f"invalid {name}")
    (tmpdir / name).write_text(content, encoding="utf-8")
PY
  install -d -o root -g opanel-ent -m 0750 "$base"
  install -m 0640 -o root -g opanel-ent "$tmpdir/cert.crt" "$base/cert.crt"
  install -m 0640 -o root -g opanel-ent "$tmpdir/privkey.key" "$base/privkey.key"
  if [[ -f "$tmpdir/ca.crt" ]]; then
    install -m 0640 -o root -g opanel-ent "$tmpdir/ca.crt" "$base/ca.crt"
    cat "$tmpdir/cert.crt" "$tmpdir/ca.crt" >"$tmpdir/fullchain.crt"
    install -m 0640 -o root -g opanel-ent "$tmpdir/fullchain.crt" "$base/fullchain.crt"
  else
    rm -f "$base/ca.crt"
    install -m 0640 -o root -g opanel-ent "$tmpdir/cert.crt" "$base/fullchain.crt"
  fi
  echo "Manual SSL installed for ${domain}"
}

remove_manual_ssl() {
  local domain="$1" base
  require_domain "$domain"
  base="/usr/local/lsws/conf/opanel-ent/ssl/sites/${domain}"
  rm -f "$base/cert.crt" "$base/privkey.key" "$base/ca.crt" "$base/fullchain.crt"
  rmdir "$base" 2>/dev/null || true
  echo "Manual SSL removed for ${domain}"
}

renew_ssl_soon() {
  local days="${1:-10}" seconds cert cert_name checked=0 renewed=0 panel_domain
  [[ "$days" =~ ^[0-9]+$ && "$days" -ge 1 && "$days" -le 30 ]] || deny "usage: certbot-renew-soon [1-30 days]"
  write_ssl_auto_renew_timer
  if ! command -v certbot >/dev/null 2>&1; then
    echo "certbot is not installed"
    return 0
  fi
  seconds=$((days * 86400))
  shopt -s nullglob
  for cert in /etc/letsencrypt/live/*/cert.pem; do
    [[ -f "$cert" ]] || continue
    cert_name="$(basename "$(dirname "$cert")")"
    [[ "$cert_name" == "README" ]] && continue
    checked=$((checked + 1))
    if ! openssl x509 -checkend "$seconds" -noout -in "$cert" >/dev/null 2>&1; then
      echo "Renewing certificate: ${cert_name}"
      if certbot renew --cert-name "$cert_name" --quiet --force-renewal \
        --deploy-hook "systemctl restart lshttpd.service || /usr/local/lsws/bin/lswsctrl restart || true; systemctl restart opanel-ent-api || true"; then
        renewed=$((renewed + 1))
      else
        echo "WARNING: could not renew ${cert_name}" >&2
      fi
    fi
  done
  shopt -u nullglob
  panel_domain="$(env_get PANEL_DOMAIN)"
  copy_panel_live_certificate "$panel_domain"
  panel_cert_store_sync
  if [[ "$renewed" -gt 0 ]]; then
    restart_openlitespeed >/dev/null 2>&1 || true
    systemctl restart opanel-ent-api >/dev/null 2>&1 || true
  fi
  echo "SSL auto-renew checked ${checked} certificate(s); renewed ${renewed} certificate(s) within ${days} day(s)."
}

# Cloudflare DNS-01 plugin — installed on demand so a box that never issues a
# wildcard cert stays lean.
ACME_DNS_DIR="/etc/opanel-ent/acme-dns"

ensure_certbot_dns_cloudflare() {
  if certbot plugins 2>/dev/null | grep -q 'dns-cloudflare'; then
    return 0
  fi
  DEBIAN_FRONTEND=noninteractive apt-get install -y python3-certbot-dns-cloudflare >/dev/null 2>&1 || true
  certbot plugins 2>/dev/null | grep -q 'dns-cloudflare' \
    || deny "certbot-dns-cloudflare plugin is not available (install python3-certbot-dns-cloudflare)"
}

# Issue / renew a wildcard cert for <domain> + *.<domain> via Cloudflare DNS-01.
# stdin is a scoped Cloudflare API Token (Zone:DNS:Edit for the zone) -- NOT the
# Global API Key. It is written as `dns_cloudflare_api_token` in a root-only
# credentials file that certbot re-reads on every renewal.
issue_cloudflare_wildcard() {
  local domain="$1" email="${2:-}" token creds
  require_domain "$domain"
  if [[ -n "$email" ]]; then
    require_email "$email"
  fi
  token="$(cat)"
  token="${token%$'\n'}"
  [[ -n "$token" ]] || deny "empty Cloudflare API token"
  [[ "$token" =~ ^[A-Za-z0-9_-]{20,200}$ ]] || deny "malformed Cloudflare API token"
  ensure_certbot_dns_cloudflare
  install -d -o root -g root -m 0700 "$ACME_DNS_DIR"
  creds="${ACME_DNS_DIR}/${domain}.ini"
  local tmp
  tmp="$(mktemp "${ACME_DNS_DIR}/.${domain}.XXXXXX")"
  printf 'dns_cloudflare_api_token = %s\n' "$token" >"$tmp"
  install -o root -g root -m 0600 "$tmp" "$creds"
  rm -f "$tmp"
  local args=(certonly --dns-cloudflare
    --dns-cloudflare-credentials "$creds"
    --dns-cloudflare-propagation-seconds 30
    --cert-name "$domain" --non-interactive --agree-tos --expand
    --deploy-hook "systemctl restart lshttpd.service 2>/dev/null || /usr/local/lsws/bin/lswsctrl restart 2>/dev/null || true; systemctl restart opanel-ent-api 2>/dev/null || true"
    -d "$domain" -d "*.${domain}")
  if [[ -n "$email" ]]; then
    args+=(--email "$email")
  else
    args+=(--register-unsafely-without-email)
  fi
  certbot "${args[@]}"
  restart_openlitespeed
  copy_panel_live_certificate "$domain"
  panel_cert_store_sync
  echo "Wildcard SSL certificate issued for ${domain} (and *.${domain})"
}

remove_cloudflare_wildcard() {
  local domain="$1"
  require_domain "$domain"
  certbot delete --cert-name "$domain" --non-interactive 2>/dev/null || true
  rm -f "${ACME_DNS_DIR}/${domain}.ini"
  panel_cert_store_sync
  echo "Wildcard SSL certificate removed for ${domain}"
}

is_in() {
  local needle="$1"; shift
  local x
  for x in "$@"; do [[ "$x" == "$needle" ]] && return 0; done
  return 1
}

is_allowed_service() {
  local service="$1" php_version=""
  if is_in "$service" "${ALLOWED_SERVICES[@]}"; then
    return 0
  fi
  return 1
}

require_safe_path() {
  local prefix="$1" path="$2"
  # Reject path traversal components, newlines, and empty input. Bash strings
  # cannot carry NUL bytes, so there is no separate NUL pattern here.
  # Note: we cannot use `*..*` as a glob because that would also reject
  # legitimate filenames that just happen to contain a dot adjacent to a dot
  # via Bash's pattern matching quirks; instead we match the `..` only when
  # it actually forms a path component.
  case "$path" in
    *$'\n'*) deny "unsafe path: $path" ;;
    "") deny "empty path" ;;
    "..") deny "path traversal not allowed" ;;
    "../"*|*"/.."|*"/../"*) deny "path traversal not allowed" ;;
  esac
  local resolved
  resolved=$(readlink -m "$path") || deny "cannot resolve $path"
  case "$resolved/" in
    "$prefix"/*) ;;
    *) deny "path outside $prefix: $resolved" ;;
  esac
  echo "$resolved"
}

require_domain() {
  local d="$1"
  [[ "$d" =~ ^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)+$ ]] \
    || deny "invalid domain: $d"
}

require_email() {
  local e="$1"
  [[ "$e" =~ ^[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}$ ]] \
    || deny "invalid email: $e"
}

require_port() {
  [[ "$1" =~ ^[0-9]{1,5}$ ]] || deny "invalid port: $1"
  (( $1 >= 1 && $1 <= 65535 )) || deny "port out of range: $1"
}

require_tail_lines() {
  [[ "$1" =~ ^[0-9]{1,4}$ ]] || deny "invalid log line count: $1"
  (( $1 >= 1 && $1 <= 5000 )) || deny "log line count out of range: $1"
}

require_proto() {
  [[ "$1" == "tcp" || "$1" == "udp" ]] || deny "invalid protocol: $1"
}

require_php_version() {
  [[ "$1" =~ ^(7\.4|8\.1|8\.2|8\.3|8\.4|8\.5)$ ]] || deny "invalid PHP version: $1"
}

require_linux_user() {
  [[ "$1" =~ ^[a-z_][a-z0-9_-]{2,31}$ ]] || deny "invalid panel Linux user: $1"
  case "$1" in
    root|daemon|bin|sys|sync|games|man|lp|mail|news|uucp|proxy|www-data|backup|list|irc|_apt|nobody|opanel-ent|opanel-ent-sites|opanel-ent-sftp|mysql|redis|nobody)
      deny "reserved panel Linux user: $1" ;;
  esac
}

require_site_domain_segment() {
  [[ "$1" =~ ^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)+$ ]] \
    || deny "invalid site domain path segment: $1"
}

read_site_log() {
  local domain="$1" kind="$2" lines="$3" resolved php_log php_resolved any=0
  require_domain "$domain"
  [[ "$kind" == "access" || "$kind" == "error" ]] || deny "invalid log kind: $kind"
  require_tail_lines "$lines"
  resolved=$(readlink -m "/var/log/openlitespeed/${domain}.${kind}.log") || deny "cannot resolve log path"
  case "$resolved" in
    /var/log/openlitespeed/*) ;;
    *) deny "log path outside /var/log/openlitespeed: $resolved" ;;
  esac

  # The Error tab shows PHP's own error_log first (that is where application
  # errors land), then the OpenLiteSpeed server error log.
  if [[ "$kind" == "error" ]]; then
    php_resolved=$(readlink -m "/var/log/openlitespeed/${domain}/php_error.log") || deny "cannot resolve log path"
    case "$php_resolved" in
      /var/log/openlitespeed/*) ;;
      *) deny "log path outside /var/log/openlitespeed: $php_resolved" ;;
    esac
    echo "opanel_ent_LOG_PATH=${php_resolved} + ${resolved}" >&2
    if [[ -s "$php_resolved" ]]; then
      any=1
      echo "===== PHP error log (${php_resolved}) ====="
      tail -n "$lines" -- "$php_resolved"
      echo
    fi
    if [[ -s "$resolved" ]]; then
      any=1
      echo "===== OpenLiteSpeed error log (${resolved}) ====="
      tail -n "$lines" -- "$resolved"
    fi
    [[ "$any" == 1 ]] || echo "opanel_ent_LOG_MISSING=1" >&2
    return 0
  fi

  echo "opanel_ent_LOG_PATH=$resolved" >&2
  if [[ ! -f "$resolved" ]]; then
    echo "opanel_ent_LOG_MISSING=1" >&2
    return 0
  fi
  tail -n "$lines" -- "$resolved"
}

read_waf_access_logs() {
  local lines="$1" domain path resolved
  shift
  require_tail_lines "$lines"
  [[ $# -ge 1 ]] || deny "usage: waf-access-log-read <lines> <domain>..."
  for domain in "$@"; do
    require_domain "$domain"
    path="/var/log/openlitespeed/${domain}.access.log"
    resolved=$(readlink -m "$path") || deny "cannot resolve log path"
    case "$resolved" in
      /var/log/openlitespeed/*) ;;
      *) deny "log path outside /var/log/openlitespeed: $resolved" ;;
    esac
    printf 'opanel_ent_LOG_PATH=%s\t%s\n' "$domain" "$resolved"
    if [[ -f "$resolved" ]]; then
      tail -n "$lines" -- "$resolved" | sed "s/^/${domain}\t/"
    fi
  done
}

clear_waf_access_logs() {
  local domain path resolved
  [[ $# -ge 1 ]] || deny "usage: waf-access-log-clear <domain>..."
  for domain in "$@"; do
    require_domain "$domain"
    path="/var/log/openlitespeed/${domain}.access.log"
    resolved=$(readlink -m "$path") || deny "cannot resolve log path"
    case "$resolved" in
      /var/log/openlitespeed/*) ;;
      *) deny "log path outside /var/log/openlitespeed: $resolved" ;;
    esac
    if [[ -f "$resolved" ]]; then
      : >"$resolved"
    fi
  done
  echo "WAF access logs cleared"
}

require_managed_path() {
  local path="$1" user="${2:-}"
  local resolved first_part relative domain_part
  resolved=$(require_safe_path "$HOME_ROOT" "$path")
  if [[ -n "$user" ]]; then
    require_linux_user "$user"
    case "$resolved/" in
      "$HOME_ROOT/$user/"*)
        relative="${resolved#${HOME_ROOT}/${user}/}"
        domain_part="${relative%%/*}"
        require_site_domain_segment "$domain_part"
        ;;
      *) deny "path is not owned by panel Linux user $user: $resolved" ;;
    esac
  else
    case "$resolved/" in
      "$HOME_ROOT"/*/*)
        first_part="${resolved#${HOME_ROOT}/}"
        first_part="${first_part%%/*}"
        require_linux_user "$first_part"
        relative="${resolved#${HOME_ROOT}/${first_part}/}"
        domain_part="${relative%%/*}"
        require_site_domain_segment "$domain_part"
        ;;
      *) deny "path outside managed site roots: $resolved" ;;
    esac
  fi
  echo "$resolved"
}

require_bound_managed_path() {
  local user="$1" root="$2" path="$3"
  local normalized_root normalized target target_relative root_relative
  require_linux_user "$user"
  case "$root" in
    *$'\n'*) deny "unsafe root: $root" ;;
    "") deny "empty root" ;;
    "..") deny "root traversal not allowed" ;;
    "../"*|*"/.."|*"/../"*) deny "root traversal not allowed" ;;
  esac
  [[ "$root" == /* ]] || deny "root must be absolute: $root"
  normalized_root=$(python3 -c 'import os, sys; print(os.path.normpath(sys.argv[1]))' "$root") || deny "cannot normalize $root"
  case "$normalized_root/" in
    "$HOME_ROOT/$user/"*) ;;
    *) deny "root is not owned by panel Linux user $user: $normalized_root" ;;
  esac
  root_relative="${normalized_root#${HOME_ROOT}/${user}/}"
  require_site_domain_segment "${root_relative%%/*}"
  [[ "$root_relative" == */* ]] && deny "site root must be a direct domain path: $normalized_root"

  case "$path" in
    *$'\n'*) deny "unsafe path: $path" ;;
    "") deny "empty path" ;;
    "..") deny "path traversal not allowed" ;;
    "../"*|*"/.."|*"/../"*) deny "path traversal not allowed" ;;
  esac
  [[ "$path" == /* ]] || deny "path must be absolute: $path"
  normalized=$(python3 -c 'import os, sys; print(os.path.normpath(sys.argv[1]))' "$path") || deny "cannot normalize $path"
  case "$normalized/" in
    "$normalized_root"|"$normalized_root/"*) ;;
    *) deny "path outside expected site root: $normalized" ;;
  esac
  target="$normalized"
  target_relative="${target#${HOME_ROOT}/${user}/}"
  [[ "$target" == "$normalized_root" || "$target_relative" == */* ]] || deny "refusing to operate on a panel user home"
  echo "$target"
}

delete_no_follow() {
  local user="$1" root="$2" target="$3"
  python3 - "$user" "$root" "$target" <<'PY'
import os
import stat
import sys

user, root, target = sys.argv[1:4]
base = f"/home/{user}"
root = os.path.normpath(root)
target = os.path.normpath(target)

if os.path.dirname(root) != base:
    raise SystemExit("invalid site root")
if target != root and not target.startswith(root + os.sep):
    raise SystemExit("target outside site root")

rel = os.path.relpath(target, base)
if rel.startswith("..") or rel == ".":
    raise SystemExit("target outside site root")

def open_child(parent_fd, name):
    return os.open(name, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW, dir_fd=parent_fd)

base_fd = os.open(base, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
try:
    parent_fd = base_fd
    close_parent = False
    parts = rel.split(os.sep)
    for part in parts[:-1]:
        next_fd = open_child(parent_fd, part)
        if close_parent:
            os.close(parent_fd)
        parent_fd = next_fd
        close_parent = True

    leaf = parts[-1]

    def remove_entry(dir_fd, name):
        st = os.lstat(name, dir_fd=dir_fd)
        if stat.S_ISDIR(st.st_mode):
            child_fd = open_child(dir_fd, name)
            try:
                for entry in os.listdir(child_fd):
                    remove_entry(child_fd, entry)
            finally:
                os.close(child_fd)
            os.rmdir(name, dir_fd=dir_fd)
        else:
            os.unlink(name, dir_fd=dir_fd)

    remove_entry(parent_fd, leaf)
finally:
    try:
        if 'parent_fd' in locals() and parent_fd != base_fd:
            os.close(parent_fd)
    finally:
        os.close(base_fd)
PY
}

require_terminal_cwd() {
  local path="$1" user="$2" resolved
  require_linux_user "$user"
  resolved=$(require_safe_path "$HOME_ROOT" "$path")
  case "$resolved" in
    "$HOME_ROOT/$user"|"$HOME_ROOT/$user"/*) ;;
    *) deny "terminal cwd is not owned by panel Linux user $user: $resolved" ;;
  esac
  [[ -d "$resolved" ]] || deny "terminal cwd is not a directory: $resolved"
  echo "$resolved"
}

require_terminal_path_args() {
  local user="$1" cwd="$2" arg resolved
  shift 2
  require_linux_user "$user"
  for arg in "$@"; do
    case "$arg" in
      ""|"-"*|"--") continue ;;
      *$'\n'*|".."|"../"*|*"/.."|*"/../"*) deny "terminal path argument escapes user home: $arg" ;;
    esac
    if [[ "$arg" = /* ]]; then
      resolved=$(readlink -m -- "$arg") || deny "cannot resolve terminal path: $arg"
    else
      resolved=$(readlink -m -- "$cwd/$arg") || deny "cannot resolve terminal path: $arg"
    fi
    case "$resolved/" in
      "$HOME_ROOT/$user/"*) ;;
      *) deny "terminal path argument is outside panel user home: $arg" ;;
    esac
  done
}

_block_internal_url() {
  local url="$1" host port resolved
  host="${url#*://}"
  host="${host%%/*}"
  host="${host%%\?*}"
  port="${host##*:}"
  if [[ "$port" == "$host" ]]; then port=""; fi
  host="${host%%:*}"
  host="${host#[@]}"
  [[ -z "$host" ]] && return
  case "$host" in
    169.254.*|metadata.google.internal)
      deny "terminal URL points to a cloud metadata endpoint: $url"
      ;;
    0.0.0.0|[::])
      deny "terminal URL points to an unspecified address: $url"
      ;;
  esac
  if command -v getent >/dev/null 2>&1; then
    while IFS= read -r resolved; do
      case "$resolved" in
        169.254.*|fe80:*|fc00:*|fd00:*) deny "terminal URL resolves to a link-local or private address: $url" ;;
      esac
    done < <(getent ahosts "$host" 2>/dev/null | awk '{print $1}' | sort -u)
  fi
}

require_terminal_download_args() {
  local user="$1" cwd="$2" arg value expect_output=0
  shift 2
  for arg in "$@"; do
    case "${arg,,}" in
      file://*) deny "terminal URL argument uses local file scheme: $arg" ;;
    esac
    if (( expect_output )); then
      require_terminal_path_args "$user" "$cwd" "$arg"
      expect_output=0
      continue
    fi
    case "$arg" in
      --output=*|--output-document=*|-O=*)
        value="${arg#*=}"
        require_terminal_path_args "$user" "$cwd" "$value"
        ;;
      -o|-O|--output|--output-document)
        expect_output=1
        ;;
      http://*|https://*|ftp://*|ftps://*|sftp://*)
        _block_internal_url "$arg"
        ;;
      -*|"")
        ;;
      *)
        require_terminal_path_args "$user" "$cwd" "$arg"
        ;;
    esac
  done
  (( expect_output == 0 )) || deny "terminal download output path is missing"
}

ensure_sites_group() {
  getent group "$opanel_ent_SITES_GROUP" >/dev/null || groupadd --system "$opanel_ent_SITES_GROUP"
  usermod -aG "$opanel_ent_SITES_GROUP" opanel-ent 2>/dev/null || true
  usermod -aG "$opanel_ent_SITES_GROUP" www-data 2>/dev/null || true
  ensure_site_log_rotation
}

# OLS rotates its own access/error logs (rollingSize/keepDays in the vhost), but
# the per-site php_error.log is written by PHP's error_log() and needs help.
ensure_site_log_rotation() {
  [[ -f /etc/logrotate.d/opanel-ent-sites ]] && return 0
  command -v logrotate >/dev/null 2>&1 || return 0
  cat >/etc/logrotate.d/opanel-ent-sites <<'EOF'
/var/log/openlitespeed/*/php_error.log {
    weekly
    rotate 8
    missingok
    notifempty
    compress
    delaycompress
    copytruncate
}
EOF
  chmod 0644 /etc/logrotate.d/opanel-ent-sites
}

ensure_sftp_group() {
  getent group "$opanel_ent_SFTP_GROUP" >/dev/null || groupadd --system "$opanel_ent_SFTP_GROUP"
}

clear_path_acl() {
  local target="$1"
  if command -v setfacl >/dev/null 2>&1; then
    setfacl -b -k "$target" 2>/dev/null || true
  fi
}

harden_site_dir() {
  local target="$1" user="$2"
  chown "$user:$user" "$target"
  clear_path_acl "$target"
  chmod 0755 "$target"
  chmod a-s "$target" 2>/dev/null || true
  chmod -t "$target" 2>/dev/null || true
}

harden_site_file() {
  local target="$1" user="$2"
  chown "$user:$user" "$target"
  clear_path_acl "$target"
  chmod 0644 "$target"
  chmod a-s "$target" 2>/dev/null || true
  chmod -t "$target" 2>/dev/null || true
}

harden_site_dir_path() {
  local root="$1" target="$2" user="$3" relative current part
  ensure_sites_group
  require_linux_user "$user"
  root=$(readlink -m "$root") || deny "cannot resolve $root"
  target=$(readlink -m "$target") || deny "cannot resolve $target"
  case "$target" in
    "$root"|"$root"/*) ;;
    *) deny "directory path outside site root: $target" ;;
  esac
  [[ -d "$target" ]] || deny "site directory does not exist: $target"
  harden_site_dir "$root" "$user"
  [[ "$target" == "$root" ]] && return 0
  relative="${target#${root}/}"
  current="$root"
  IFS='/' read -r -a root_parts <<< "$relative"
  for part in "${root_parts[@]}"; do
    current="$current/$part"
    [[ -d "$current" ]] || deny "site directory does not exist: $current"
    harden_site_dir "$current" "$user"
  done
}

ensure_panel_user_home() {
  local user="$1" home_dir="$HOME_ROOT/$1"
  ensure_sites_group
  ensure_sftp_group
  require_linux_user "$user"
  getent group "$user" >/dev/null || groupadd "$user"
  usermod -aG "$user" www-data 2>/dev/null || true
  chown root:root "$HOME_ROOT"
  chmod 0711 "$HOME_ROOT"
  chmod a-s "$HOME_ROOT" 2>/dev/null || true
  chmod -t "$HOME_ROOT" 2>/dev/null || true
  if ! id -u "$user" >/dev/null 2>&1; then
    useradd --create-home --home-dir "$home_dir" --shell /usr/sbin/nologin --gid "$user" "$user"
  fi
  usermod --home "$home_dir" --shell /usr/sbin/nologin --gid "$user" "$user" 2>/dev/null || true
  usermod -aG "$opanel_ent_SFTP_GROUP" "$user" 2>/dev/null || true
  mkdir -p "$home_dir"
  chown "root:$user" "$home_dir"
  chmod 0751 "$home_dir"
  chmod a-s "$home_dir" 2>/dev/null || true
  chmod -t "$home_dir" 2>/dev/null || true
  clear_path_acl "$home_dir"
}

set_panel_user_password() {
  local user="$1" password
  require_linux_user "$user"
  id -u "$user" >/dev/null 2>&1 || deny "panel Linux user does not exist: $user"
  password="$(cat)"
  password="${password%$'\n'}"
  [[ ${#password} -ge 12 && ${#password} -le 72 ]] || deny "password must be 12-72 characters"
  case "$password" in
    *:*|*$'\r'*|*$'\n'*) deny "password cannot contain ':', carriage returns or newlines" ;;
  esac
  printf '%s:%s\n' "$user" "$password" | chpasswd
  passwd -u "$user" >/dev/null 2>&1 || true
}

delete_panel_user_runtime() {
  local user="$1"
  require_linux_user "$user"
  for dir in /usr/local/lsws/lsphp*/etc/php.d; do
    [[ -d "$dir" ]] || continue
    for pool_file in "$dir"/opanel-ent-${user}.conf "$dir"/opanel-ent-${user}-*.conf; do
      [[ -f "$pool_file" ]] || continue
      rm -f "$pool_file"
    done
  done
  restart_openlitespeed 2>/dev/null || true
  crontab -r -u "$user" 2>/dev/null || true
  pkill -u "$user" 2>/dev/null || true
  userdel "$user" 2>/dev/null || true
  groupdel "$user" 2>/dev/null || true
  rm -rf "$HOME_ROOT/$user" 2>/dev/null || true
  rm -rf "/var/lib/php/sessions/$user" 2>/dev/null || true
  rm -rf "/var/lib/php/uploads/$user" 2>/dev/null || true
}

site_php_pool_glob() {
  local user="$1" target="$2"
  require_linux_user "$user"
  target=$(readlink -m "$target") || deny "cannot resolve $target"
  local site_hash
  site_hash="$(printf '%s' "$target" | sha256sum | awk '{print substr($1, 1, 12)}')"
  printf 'opanel-ent-%s-%s-*' "$user" "$site_hash"
}

positive_int_or_default() {
  local value="${1:-}" default="$2" min="${3:-1}" max="${4:-}"
  if [[ ! "$value" =~ ^[0-9]+$ ]]; then
    value="$default"
  fi
  if (( value < min )); then
    value="$min"
  fi
  if [[ -n "$max" ]] && (( value > max )); then
    value="$max"
  fi
  printf '%s\n' "$value"
}

php_fpm_tuning_value() {
  local key="$1" default="$2" value=""
  if [[ -v "$key" ]]; then
    value="${!key}"
  fi
  if [[ -z "$value" ]]; then
    value="$(env_get "$key" 2>/dev/null || true)"
  fi
  printf '%s\n' "${value:-$default}"
}

php_fpm_total_memory_mb() {
  local total
  total="$(awk '/^MemTotal:/ { print int($2 / 1024); exit }' /proc/meminfo 2>/dev/null || true)"
  positive_int_or_default "$total" 1024 256 1048576
}

php_fpm_cpu_count() {
  local count
  count="$(nproc 2>/dev/null || getconf _NPROCESSORS_ONLN 2>/dev/null || echo 1)"
  positive_int_or_default "$count" 1 1 256
}

php_fpm_pool_count() {
  local current_pool="${1:-}" count=0 pool_file
  shopt -s nullglob
  for pool_file in /usr/local/lsws/lsphp*/etc/php.d/opanel-ent-*.conf; do
    [[ -f "$pool_file" ]] || continue
    count=$((count + 1))
  done
  shopt -u nullglob
  if [[ -n "$current_pool" && ! -f "$current_pool" ]]; then
    count=$((count + 1))
  fi
  if (( count < 1 )); then
    count=1
  fi
  printf '%s\n' "$count"
}

php_fpm_reserved_memory_mb() {
  local total="$1" reserve
  if (( total <= 1024 )); then
    reserve=$((total * 45 / 100))
    (( reserve >= 448 )) || reserve=448
  elif (( total <= 2048 )); then
    reserve=$((total * 35 / 100))
    (( reserve >= 640 )) || reserve=640
  elif (( total <= 4096 )); then
    reserve=$((total * 30 / 100))
    (( reserve >= 896 )) || reserve=896
  elif (( total <= 8192 )); then
    reserve=$((total * 25 / 100))
    (( reserve >= 1280 )) || reserve=1280
  else
    reserve=$((total * 20 / 100))
    (( reserve >= 2048 )) || reserve=2048
  fi
  if (( reserve > total - 128 )); then
    reserve=$((total - 128))
  fi
  if (( reserve < 128 )); then
    reserve=128
  fi
  printf '%s\n' "$reserve"
}

calculate_php_fpm_pool_tuning() {
  local current_pool="${1:-}" total_mb reserve_mb php_budget_mb cpu_count pool_count worker_mb
  local global_children pool_children cpu_cap profile_cap forced_children idle_default requests_default
  local active_pool_divisor pool_floor
  total_mb="$(php_fpm_total_memory_mb)"
  cpu_count="$(php_fpm_cpu_count)"
  pool_count="$(php_fpm_pool_count "$current_pool")"
  worker_mb="$(positive_int_or_default "$(php_fpm_tuning_value opanel_ent_PHP_FPM_WORKER_MB "$LSPHP_DEFAULT_WORKER_MB")" "$LSPHP_DEFAULT_WORKER_MB" 32 1024)"
  reserve_mb="$(php_fpm_reserved_memory_mb "$total_mb")"
  php_budget_mb=$((total_mb - reserve_mb))
  if (( php_budget_mb < worker_mb )); then
    php_budget_mb="$worker_mb"
  fi

  global_children=$((php_budget_mb / worker_mb))
  (( global_children >= 1 )) || global_children=1

  active_pool_divisor=1
  while (( active_pool_divisor * active_pool_divisor < pool_count )); do
    active_pool_divisor=$((active_pool_divisor + 1))
  done
  pool_children=$((global_children / active_pool_divisor))
  (( pool_children >= 1 )) || pool_children=1

  cpu_cap=$((cpu_count * 4))
  if (( total_mb >= 3072 )); then
    cpu_cap=$((cpu_count * 6))
  fi
  if (( total_mb >= 8192 )); then
    cpu_cap=$((cpu_count * 8))
  fi
  (( cpu_cap >= 2 )) || cpu_cap=2
  (( cpu_cap <= 96 )) || cpu_cap=96

  if (( total_mb <= 1024 )); then
    pool_floor=1
    profile_cap=4
    idle_default=10
    requests_default=300
  elif (( total_mb <= 2048 )); then
    pool_floor=2
    profile_cap=8
    idle_default=15
    requests_default=400
  elif (( total_mb <= 4096 )); then
    pool_floor=3
    profile_cap=14
    idle_default=20
    requests_default=500
  elif (( total_mb <= 8192 )); then
    pool_floor=4
    profile_cap=24
    idle_default=30
    requests_default=750
  else
    pool_floor=6
    profile_cap=48
    idle_default=45
    requests_default=1000
  fi

  (( pool_children >= pool_floor )) || pool_children="$pool_floor"
  (( pool_children <= cpu_cap )) || pool_children="$cpu_cap"
  (( pool_children <= profile_cap )) || pool_children="$profile_cap"
  forced_children="$(php_fpm_tuning_value opanel_ent_PHP_FPM_MAX_CHILDREN "")"
  if [[ -n "$forced_children" ]]; then
    pool_children="$(positive_int_or_default "$forced_children" "$pool_children" 1 512)"
  fi

  PHP_FPM_PM_MODE="ondemand"
  PHP_FPM_MAX_CHILDREN="$pool_children"
  PHP_FPM_PROCESS_IDLE_TIMEOUT="$(positive_int_or_default "$(php_fpm_tuning_value opanel_ent_PHP_FPM_IDLE_TIMEOUT "$idle_default")" "$idle_default" 5 300)"
  PHP_FPM_MAX_REQUESTS="$(positive_int_or_default "$(php_fpm_tuning_value opanel_ent_PHP_FPM_MAX_REQUESTS "$requests_default")" "$requests_default" 50 10000)"
  PHP_FPM_REQUEST_TERMINATE_TIMEOUT="$(positive_int_or_default "$(php_fpm_tuning_value opanel_ent_PHP_FPM_REQUEST_TERMINATE_TIMEOUT "$LSPHP_DEFAULT_REQUEST_TERMINATE_TIMEOUT")" "$LSPHP_DEFAULT_REQUEST_TERMINATE_TIMEOUT" 30 3600)"
}

php_fpm_set_directive() {
  local file="$1" key="$2" value="$3" key_re
  key_re="${key//./\\.}"
  if grep -Eq "^[;[:space:]]*${key_re}[[:space:]]*=" "$file"; then
    sed -i -E "s|^[;[:space:]]*${key_re}[[:space:]]*=.*|${key} = ${value}|" "$file"
  else
    printf '%s = %s\n' "$key" "$value" >>"$file"
  fi
}

apply_php_fpm_tuning_to_pool_file() {
  local pool_file="$1"
  php_fpm_set_directive "$pool_file" "pm" "$PHP_FPM_PM_MODE"
  php_fpm_set_directive "$pool_file" "pm.max_children" "$PHP_FPM_MAX_CHILDREN"
  php_fpm_set_directive "$pool_file" "pm.process_idle_timeout" "${PHP_FPM_PROCESS_IDLE_TIMEOUT}s"
  php_fpm_set_directive "$pool_file" "pm.max_requests" "$PHP_FPM_MAX_REQUESTS"
  php_fpm_set_directive "$pool_file" "request_terminate_timeout" "${PHP_FPM_REQUEST_TERMINATE_TIMEOUT}s"
}

retune_php_fpm_pools() {
  local pool_file php_version pool_user count=0
  shopt -s nullglob
  for pool_file in /usr/local/lsws/lsphp*/etc/php.d/opanel-ent-*.conf; do
    [[ -f "$pool_file" ]] || continue
    calculate_php_fpm_pool_tuning "$pool_file"
    apply_php_fpm_tuning_to_pool_file "$pool_file"
    pool_user="$(awk -F= '/^[[:space:]]*user[[:space:]]*=/ { gsub(/^[[:space:]]+|[[:space:]]+$/, "", $2); print $2; exit }' "$pool_file")"
    if [[ -n "$pool_user" ]]; then
      ensure_php_runtime_dirs "$pool_user"
      usermod -aG "$pool_user" www-data 2>/dev/null || true
    fi
    count=$((count + 1))
  done
  shopt -u nullglob
  restart_openlitespeed 2>/dev/null || true
  echo "Retuned ${count} opanel-ent PHP-FPM pool(s)."
}

mariadb_tuning_value() {
  local key="$1" default="$2" value=""
  if [[ -v "$key" ]]; then
    value="${!key}"
  fi
  if [[ -z "$value" ]]; then
    value="$(env_get "$key" 2>/dev/null || true)"
  fi
  printf '%s\n' "${value:-$default}"
}

mariadb_megabytes() {
  local value="${1:-}" default="$2" number unit
  if [[ "$value" =~ ^([0-9]+)([KkMmGg]?)$ ]]; then
    number="${BASH_REMATCH[1]}"
    unit="${BASH_REMATCH[2]}"
    case "$unit" in
      [Kk]) printf '%s\n' $(((number + 1023) / 1024)) ;;
      [Gg]) printf '%s\n' $((number * 1024)) ;;
      *) printf '%s\n' "$number" ;;
    esac
    return 0
  fi
  printf '%s\n' "$default"
}

calculate_mariadb_tuning() {
  local total_mb cpu_count buffer_default buffer_mb log_file_mb tmp_mb max_connections thread_cache
  local table_open_cache open_files_limit packet_mb io_capacity
  total_mb="$(php_fpm_total_memory_mb)"
  cpu_count="$(php_fpm_cpu_count)"

  if (( total_mb <= 1024 )); then
    buffer_default=$((total_mb * 22 / 100))
    max_connections=35
    thread_cache=16
    table_open_cache=512
    tmp_mb=32
    packet_mb=64
  elif (( total_mb <= 2048 )); then
    buffer_default=$((total_mb * 25 / 100))
    max_connections=50
    thread_cache=24
    table_open_cache=512
    tmp_mb=48
    packet_mb=64
  elif (( total_mb <= 4096 )); then
    buffer_default=$((total_mb * 28 / 100))
    max_connections=80
    thread_cache=32
    table_open_cache=1024
    tmp_mb=64
    packet_mb=96
  elif (( total_mb <= 8192 )); then
    buffer_default=$((total_mb * 32 / 100))
    max_connections=120
    thread_cache=48
    table_open_cache=1024
    tmp_mb=96
    packet_mb=128
  else
    buffer_default=$((total_mb * 36 / 100))
    max_connections=180
    thread_cache=64
    table_open_cache=2048
    tmp_mb=128
    packet_mb=128
  fi

  (( buffer_default >= 128 )) || buffer_default=128
  (( buffer_default <= total_mb * 45 / 100 )) || buffer_default=$((total_mb * 45 / 100))
  buffer_mb="$(mariadb_megabytes "$(mariadb_tuning_value opanel_ent_MARIADB_BUFFER_POOL_SIZE "${buffer_default}M")" "$buffer_default")"
  buffer_mb="$(positive_int_or_default "$buffer_mb" "$buffer_default" 128 "$((total_mb * 60 / 100))")"

  max_connections="$(positive_int_or_default "$(mariadb_tuning_value opanel_ent_MARIADB_MAX_CONNECTIONS "$max_connections")" "$max_connections" 20 1000)"
  thread_cache="$(positive_int_or_default "$(mariadb_tuning_value opanel_ent_MARIADB_THREAD_CACHE_SIZE "$thread_cache")" "$thread_cache" 8 256)"
  table_open_cache="$(positive_int_or_default "$(mariadb_tuning_value opanel_ent_MARIADB_TABLE_OPEN_CACHE "$table_open_cache")" "$table_open_cache" 256 65535)"
  tmp_mb="$(mariadb_megabytes "$(mariadb_tuning_value opanel_ent_MARIADB_TMP_TABLE_SIZE "${tmp_mb}M")" "$tmp_mb")"
  tmp_mb="$(positive_int_or_default "$tmp_mb" 64 16 512)"
  packet_mb="$(mariadb_megabytes "$(mariadb_tuning_value opanel_ent_MARIADB_MAX_ALLOWED_PACKET "${packet_mb}M")" "$packet_mb")"
  packet_mb="$(positive_int_or_default "$packet_mb" 64 16 512)"
  log_file_mb=$((buffer_mb / 4))
  log_file_mb="$(positive_int_or_default "$(mariadb_megabytes "$(mariadb_tuning_value opanel_ent_MARIADB_LOG_FILE_SIZE "${log_file_mb}M")" "$log_file_mb")" "$log_file_mb" 64 1024)"
  io_capacity=$((cpu_count * 200))
  io_capacity="$(positive_int_or_default "$(mariadb_tuning_value opanel_ent_MARIADB_IO_CAPACITY "$io_capacity")" "$io_capacity" 200 4000)"
  open_files_limit=$((table_open_cache * 2 + max_connections + 512))
  open_files_limit="$(positive_int_or_default "$(mariadb_tuning_value opanel_ent_MARIADB_OPEN_FILES_LIMIT "$open_files_limit")" "$open_files_limit" 2048 200000)"

  MARIADB_INNODB_BUFFER_POOL_SIZE="${buffer_mb}M"
  MARIADB_INNODB_LOG_FILE_SIZE="${log_file_mb}M"
  MARIADB_MAX_CONNECTIONS="$max_connections"
  MARIADB_THREAD_CACHE_SIZE="$thread_cache"
  MARIADB_TABLE_OPEN_CACHE="$table_open_cache"
  MARIADB_TMP_TABLE_SIZE="${tmp_mb}M"
  MARIADB_MAX_ALLOWED_PACKET="${packet_mb}M"
  MARIADB_INNODB_IO_CAPACITY="$io_capacity"
  MARIADB_OPEN_FILES_LIMIT="$open_files_limit"
}

write_mariadb_tuning() {
  calculate_mariadb_tuning
  install -d -o root -g root -m 0755 "$(dirname "$MARIADB_TUNING_CONF")"
  cat >"$MARIADB_TUNING_CONF" <<MYSQL
# OPanel Enterprise auto-tunes MariaDB for small and medium VPS plans.
# Optional overrides in ${ENV_FILE}: opanel_ent_MARIADB_BUFFER_POOL_SIZE,
# OPanel_MARIADB_MAX_CONNECTIONS, opanel_ent_MARIADB_THREAD_CACHE_SIZE,
# OPanel_MARIADB_TABLE_OPEN_CACHE, opanel_ent_MARIADB_TMP_TABLE_SIZE,
# OPanel_MARIADB_MAX_ALLOWED_PACKET, opanel_ent_MARIADB_LOG_FILE_SIZE,
# OPanel_MARIADB_IO_CAPACITY, opanel_ent_MARIADB_OPEN_FILES_LIMIT.
[mysqld]
innodb_buffer_pool_size = ${MARIADB_INNODB_BUFFER_POOL_SIZE}
innodb_log_file_size = ${MARIADB_INNODB_LOG_FILE_SIZE}
innodb_flush_log_at_trx_commit = 2
innodb_flush_method = O_DIRECT
innodb_io_capacity = ${MARIADB_INNODB_IO_CAPACITY}
max_connections = ${MARIADB_MAX_CONNECTIONS}
thread_cache_size = ${MARIADB_THREAD_CACHE_SIZE}
table_open_cache = ${MARIADB_TABLE_OPEN_CACHE}
tmp_table_size = ${MARIADB_TMP_TABLE_SIZE}
max_heap_table_size = ${MARIADB_TMP_TABLE_SIZE}
max_allowed_packet = ${MARIADB_MAX_ALLOWED_PACKET}
skip_name_resolve = 1
slow_query_log = 1
slow_query_log_file = /var/log/mysql/opanel-ent-slow.log
long_query_time = 2

[server]
open_files_limit = ${MARIADB_OPEN_FILES_LIMIT}
MYSQL
}

ensure_mariadb_slow_log() {
  local log_dir="/var/log/mysql" log_file="/var/log/mysql/opanel-ent-slow.log" log_group="mysql"
  getent group adm >/dev/null 2>&1 && log_group="adm"
  install -d -o mysql -g "$log_group" -m 0750 "$log_dir"
  touch "$log_file"
  chown mysql:"$log_group" "$log_file"
  chmod 0640 "$log_file"
}

retune_mariadb() {
  write_mariadb_tuning
  ensure_mariadb_slow_log
  mariadbd --help --verbose >/dev/null
  systemctl restart mariadb
  echo "Retuned MariaDB: innodb_buffer_pool_size=${MARIADB_INNODB_BUFFER_POOL_SIZE}, max_connections=${MARIADB_MAX_CONNECTIONS}, table_open_cache=${MARIADB_TABLE_OPEN_CACHE}."
}

delete_site_php_pools() {
  local user="$1" target="$2" glob
  glob="$(site_php_pool_glob "$user" "$target")"
  for dir in /etc/php/*/fpm/pool.d; do
    [[ -d "$dir" ]] || continue
    for pool_file in "$dir"/$glob.conf; do
      [[ -f "$pool_file" ]] || continue
      rm -f "$pool_file"
      local php_version
      php_version="$(echo "$dir" | awk -F/ '{print $4}')"
      systemctl reload "php${php_version}-fpm" 2>/dev/null || true
    done
  done
  for dir in /usr/local/lsws/lsphp*/etc/php.d; do
    [[ -d "$dir" ]] || continue
    for pool_file in "$dir"/$glob.conf; do
      [[ -f "$pool_file" ]] || continue
      rm -f "$pool_file"
    done
  done
  restart_openlitespeed 2>/dev/null || true
}

ensure_php_pool() {
  local user="$1" target="$2" php_version="$3"
  [[ "$php_version" != "none" ]] || return 0
  require_linux_user "$user"
  require_php_version "$php_version"
  target=$(readlink -m "$target") || deny "cannot resolve $target"
  local pool_suffix="lsphp${php_version//./}"
  local lsphp_version="${php_version//./}"
  local site_hash
  site_hash="$(printf '%s' "$target" | sha256sum | awk '{print substr($1, 1, 12)}')"
  local pool_name="opanel-ent-${user}-${site_hash}-${pool_suffix}"
  local lsphp_dir="/usr/local/lsws/lsphp${lsphp_version}"
  [[ -x "${lsphp_dir}/bin/lsphp" ]] || deny "LSPHP ${php_version} is not installed"
  local pool_dir="${lsphp_dir}/etc/php.d"
  local pool_file="${pool_dir}/${pool_name}.conf"
  # Per-user dirs for sessions/uploads. Sharing /tmp across pools lets one
  # site read another's session files (mode 0600 helps but only inside the
  # same uid; uploads land world-writable on tmpfs). Using 0700 dirs owned
  # by the pool's Linux user contains the data inside the site's trust
  # boundary.
  local sess_dir="/var/lib/php/sessions/${user}"
  local upload_dir="/var/lib/php/uploads/${user}"
  ensure_php_runtime_dirs "$user"
  install -d -o root -g root -m 0755 "$pool_dir"
  calculate_php_fpm_pool_tuning "$pool_file"
  local pool_tmp
  pool_tmp="$(mktemp)"
  cat >"$pool_tmp" <<POOL
; opanel-ent auto-tunes these values from RAM, CPU and managed pool count.
; Optional overrides: opanel_ent_PHP_FPM_WORKER_MB, opanel_ent_PHP_FPM_MAX_CHILDREN,
; opanel_ent_PHP_FPM_IDLE_TIMEOUT, opanel_ent_PHP_FPM_MAX_REQUESTS,
; opanel_ent_PHP_FPM_REQUEST_TERMINATE_TIMEOUT.
; OLS starts this site as an LSAPI external app from the vhost config.
user = ${user}
group = ${user}
LSAPI_CHILDREN = ${PHP_FPM_MAX_CHILDREN}
LSAPI_MAX_IDLE = ${PHP_FPM_PROCESS_IDLE_TIMEOUT}
LSAPI_MAX_REQS = ${PHP_FPM_MAX_REQUESTS}
LSAPI_MAX_PROCESS_TIME = ${PHP_FPM_REQUEST_TERMINATE_TIMEOUT}
open_basedir = ${target}:${sess_dir}:${upload_dir}:/usr/share/php
upload_tmp_dir = ${upload_dir}
session.save_path = ${sess_dir}
POOL
  # Skip the OLS restart when the pool config is byte-identical to what is
  # already deployed -- a bulk site refresh writes the same file back for every
  # site and would otherwise restart OLS once per site for no reason.
  if [[ -f "$pool_file" ]] && cmp -s "$pool_tmp" "$pool_file"; then
    rm -f "$pool_tmp"
    return 0
  fi
  install -m 0644 -o root -g root "$pool_tmp" "$pool_file"
  rm -f "$pool_tmp"
  restart_openlitespeed 2>/dev/null || true
}

ensure_php_runtime_dirs() {
  local user="$1"
  local sess_dir="/var/lib/php/sessions/${user}"
  local upload_dir="/var/lib/php/uploads/${user}"
  ensure_sites_group
  require_linux_user "$user"
  install -d -o www-data -g "$opanel_ent_SITES_GROUP" -m 2775 /tmp/lshttpd
  chmod g+s /tmp/lshttpd 2>/dev/null || true
  install -d -o "$user" -g "$user" -m 0700 "$sess_dir"
  install -d -o "$user" -g "$user" -m 0700 "$upload_dir"
  chmod g-s "$upload_dir" 2>/dev/null || true
}

fix_site_tree() {
  local target="$1" user="$2"
  ensure_sites_group
  require_linux_user "$user"
  chown -R "$user:$user" "$target"
  if [[ -d "$target" ]]; then
    if command -v setfacl >/dev/null 2>&1; then
      setfacl -Rb "$target" 2>/dev/null || true
      find "$target" -type d -exec setfacl -k {} + 2>/dev/null || true
    fi
    find "$target" -type d -exec chmod 755 {} +
    find "$target" -type d -exec chmod a-s {} + 2>/dev/null || true
    find "$target" -type d -exec chmod -t {} + 2>/dev/null || true
    find "$target" -type f -exec chmod 644 {} +
  else
    harden_site_file "$target" "$user"
  fi
}

require_ip_or_cidr() {
  # Loose check; we trust iptables to do the final parsing.
  [[ "$1" =~ ^[0-9a-fA-F.:/]+$ ]] || deny "invalid IP/CIDR: $1"
}

cmd="${1:-}"
shift || true
audit_log "$@"

case "$cmd" in

  # ---- systemctl --------------------------------------------------------
  systemctl)
    [[ $# -ge 2 ]] || deny "usage: systemctl <service> <action>"
    service="$1"; action="$2"
    is_allowed_service "$service" || deny "service not allowed: $service"
    is_in "$action" "${ALLOWED_ACTIONS[@]}" || deny "action not allowed: $action"
    if [[ "$action" == "stop" && ( "$service" == "opanel-ent-api" || "$service" == "redis-server" ) ]]; then
      deny "refusing to stop panel-critical service: $service"
    fi
    exec systemctl "$action" "$service"
    ;;

  daemon-reload)
    exec systemctl daemon-reload
    ;;

  # ---- web server (OpenLiteSpeed) --------------------------------------
  ols-test|nginx-test)
    restart_openlitespeed >/dev/null 2>&1 \
      || deny "OpenLiteSpeed configuration test failed"
    echo "OpenLiteSpeed configuration OK"
    ;;

  ols-reload|nginx-reload)
    ols_sync_main_config
    restart_openlitespeed
    ;;
  ols-sync-main)
    ols_sync_main_config
    restart_openlitespeed 2>/dev/null || true
    echo "OpenLiteSpeed main config synced"
    ;;

  panel-ipv6-set)
    [[ $# -eq 1 ]] || deny "usage: panel-ipv6-set <on|off>"
    case "$1" in
      on)  panel_bind="::" ;;
      off) panel_bind="0.0.0.0" ;;
      *) deny "usage: panel-ipv6-set <on|off>" ;;
    esac
    # Write the panel bind before touching the web server: if the OLS sync
    # fails, the admin must still get a panel back on the address family they
    # just asked for.
    env_set PANEL_BIND_HOST "$panel_bind"
    set_outbound_ipv4_preference "$1"
    if [[ "$1" == "on" ]]; then
      iptables_ensure_opanel_chains
      iptables_refresh_standard_ports
      firewall_persist_rules 2>/dev/null || true
    fi
    # The listeners follow the flag the panel already wrote to
    # panel-settings.json.
    ols_sync_main_config || true
    restart_openlitespeed 2>/dev/null || true
    schedule_panel_restart
    echo "IPv6 $1 (panel bind ${panel_bind})"
    ;;

  refresh-tools)
    refresh_tools_ols
    echo "Tools vhost (phpMyAdmin) config refreshed"
    ;;
  ols-custom-write|nginx-custom-write)
    [[ $# -eq 1 ]] || deny "usage: ols-custom-write <domain>"
    domain="$1"
    require_domain "$domain"
    ensure_ols_conf_dir_writable
    target="${OLS_CUSTOM_DIR}/${domain}.conf"
    tmp="${target}.tmp.$$"
    cat >"$tmp"
    if file_has_nul "$tmp"; then
      rm -f "$tmp"
      deny "custom OLS include contains NUL byte"
    fi
    install -m 0664 -o root -g opanel-ent "$tmp" "$target"
    rm -f "$tmp"
    ;;
  ols-custom-delete|nginx-custom-delete)
    [[ $# -eq 1 ]] || deny "usage: ols-custom-delete <domain>"
    domain="$1"
    require_domain "$domain"
    rm -f "${OLS_CUSTOM_DIR}/${domain}.conf"
    ;;

  fastcgi-cache-clear)
    # OLS does not use nginx-style FastCGI caching; no-op for compatibility.
    ;;

  # ---- updates ----------------------------------------------------------
  updates-status)
    echo "opanel-ent release status:"
    if [[ -f "${opanel_ent_DATA_DIR}/update-status.json" ]]; then
      cat "${opanel_ent_DATA_DIR}/update-status.json"
    else
      echo "No update status file found."
    fi
    echo ""
    echo "APT upgradable packages:"
    apt list --upgradable 2>/dev/null | sed -n '1,60p' || true
    echo ""
    echo "Unattended upgrades:"
    systemctl is-enabled unattended-upgrades.service 2>/dev/null || true
    systemctl is-active unattended-upgrades.service 2>/dev/null || true
    echo ""
    echo "Panel auto update timer:"
    systemctl is-enabled opanel-ent-auto-update.timer 2>/dev/null || true
    systemctl list-timers opanel-ent-auto-update.timer apt-daily-upgrade.timer --no-pager 2>/dev/null || true
    echo ""
    echo "OS update service:"
    systemctl is-active opanel-ent-os-update.service 2>/dev/null | sed 's/^inactive$/idle/' || true
    journalctl -u opanel-ent-os-update.service -n 16 --no-pager 2>/dev/null | grep -v "Failed to open /run/systemd/transient" || true
    echo ""
    echo "Panel update service:"
    systemctl is-active opanel-ent-panel-update.service 2>/dev/null | sed 's/^inactive$/idle/' || true
    journalctl -u opanel-ent-panel-update.service -n 16 --no-pager 2>/dev/null | grep -v "Failed to open /run/systemd/transient" || true
    echo ""
    echo "Panel update log:"
    if command -v journalctl >/dev/null 2>&1 && systemctl cat opanel-ent-panel-update.service >/dev/null 2>&1; then
      journalctl -u opanel-ent-panel-update.service -n 60 --no-pager 2>/dev/null | grep -v "Failed to open /run/systemd/transient" || true
    fi
    if [[ ! -s /dev/stdin ]]; then :; fi
    if [[ -f /var/log/opanel-ent-panel-update.log ]]; then
      echo "--- /var/log/opanel-ent-panel-update.log (tail) ---"
      tail -n 60 /var/log/opanel-ent-panel-update.log 2>/dev/null || true
    fi
    ;;

  updates-os-run)
    run_os_update
    ;;

  updates-os-auto)
    [[ $# -eq 3 ]] || deny "usage: updates-os-auto <on|off> <security|all> <on|off>"
    configure_unattended_upgrades "$1" "$2" "$3"
    ;;

  updates-panel-run)
    run_panel_update
    ;;

  updates-panel-auto)
    [[ $# -eq 2 ]] || deny "usage: updates-panel-auto <on|off> <HH:MM>"
    write_panel_auto_update_timer "$1" "$2"
    ;;

  # ---- WAF --------------------------------------------------------------
  waf-status)
    waf_status
    ;;

  waf-install)
    install_waf_engine
    ;;

  ols-vhost-write|ols-vhost-write-defer)
    [[ $# -ge 1 ]] || deny "usage: ols-vhost-write <domain> [hostname ...]"
    safe_domain="$1"
    require_domain "$safe_domain"
    shift
    for hostname in "$@"; do
      require_domain "$hostname"
    done
    vhost_conf="$OLS_VHOSTS_DIR/$safe_domain/vhost.conf"
    vhost_tmp="$(mktemp)"
    cat >"$vhost_tmp"
    # A bulk refresh re-renders every vhost with identical output; when the file
    # is byte-identical and already in place there is nothing to sync or restart.
    if [[ -f "$vhost_conf" ]] && cmp -s "$vhost_tmp" "$vhost_conf"; then
      rm -f "$vhost_tmp"
      echo "vhost unchanged: ${safe_domain}"
      exit 0
    fi
    # The site's PHP runs as its own Linux user and cannot write the
    # OLS-owned <domain>.error.log, so PHP's error_log points at a per-domain
    # directory the site user owns. Create it here (the vhost that references it
    # is being written right now).
    vhost_site_user="$(sed -nE 's#^[[:space:]]*docRoot[[:space:]]+/home/([^/]+)/.*#\1#p' "$vhost_tmp" | head -1)"
    if [[ -n "$vhost_site_user" ]] && id "$vhost_site_user" >/dev/null 2>&1; then
      [[ -d /var/log/openlitespeed ]] || install -d -o www-data -g opanel-ent-sites -m 2775 /var/log/openlitespeed
      install -d -o "$vhost_site_user" -g "$vhost_site_user" -m 0750 "/var/log/openlitespeed/$safe_domain"
    fi
    install -d -o root -g opanel-ent -m 2775 "$OLS_VHOSTS_DIR/$safe_domain"
    install -m 0644 -o root -g opanel-ent "$vhost_tmp" "$vhost_conf"
    rm -f "$vhost_tmp"
    chown -R root:opanel-ent "$OLS_VHOSTS_DIR/$safe_domain"
    chmod 2775 "$OLS_VHOSTS_DIR/$safe_domain"
    chmod 0644 "$vhost_conf"
    # The -defer variant only stages the vhost file; the caller (e.g. a bulk
    # DirectAdmin import writing dozens of vhosts) is responsible for a single
    # `ols-sync-main` afterwards instead of one OLS restart per vhost.
    if [[ "$cmd" == "ols-vhost-write" ]]; then
      ols_sync_main_config
      restart_openlitespeed 2>/dev/null || true
    fi
    ;;

  ols-vhost-delete)
    [[ $# -eq 1 ]] || deny "usage: ols-vhost-delete <domain>"
    safe_domain="$1"
    require_domain "$safe_domain"
    rm -rf "$OLS_VHOSTS_DIR/$safe_domain"
    # The site's WAF rules are written per vhost and referenced by nothing else,
    # so they go with it. Leaving them behind accumulated orphaned rule files --
    # 42 of them on a box that had deleted that many sites.
    rm -f "/usr/local/lsws/conf/opanel-ent/waf/sites/${safe_domain}.conf"
    ols_sync_main_config
    restart_openlitespeed 2>/dev/null || true
    ;;

  # ---- ClamAV malware scanning (optional) -------------------------------
  clamav-install)
    install_clamav_engine
    ;;

  clamav-status)
    if command -v clamd >/dev/null 2>&1 || command -v clamscan >/dev/null 2>&1; then
      installed=1
    else
      installed=0
    fi
    if systemctl is-active --quiet clamav-daemon 2>/dev/null; then
      running=1
    else
      running=0
    fi
    if lmd_installed; then lmd=1; else lmd=0; fi
    lmd_ver="$( [[ -f /usr/local/maldetect/VERSION ]] && tr -d '[:space:]' </usr/local/maldetect/VERSION || echo '' )"
    if systemctl is-active --quiet opanel-ent-maldet-monitor.service 2>/dev/null; then monitor=1; else monitor=0; fi
    echo "installed=${installed} running=${running} lmd=${lmd} lmd_version=${lmd_ver} monitor=${monitor}"
    ;;

  maldet-ensure)
    # Add LMD to a box that already runs ClamAV (called from opanel-ent-update).
    command -v clamdscan >/dev/null 2>&1 || deny "ClamAV is not installed"
    if lmd_installed; then
      configure_lmd
      echo "Linux Malware Detect already present"
    else
      install_lmd_engine
    fi
    ;;

  maldet-monitor)
    [[ $# -eq 1 ]] || deny "usage: maldet-monitor <enable|disable>"
    case "$1" in
      enable) enable_lmd_monitor ;;
      disable) disable_lmd_monitor ;;
      *) deny "usage: maldet-monitor <enable|disable>" ;;
    esac
    ;;

  malware-sigs-update)
    update_malware_signatures
    ;;

  malware-quarantine)
    # <list> | <add PATH [SIG]> | <restore ID> | <drop ID>
    [[ $# -ge 1 && $# -le 3 ]] || deny "usage: malware-quarantine <list|add|restore|drop> [args]"
    quarantine_dispatch "$@"
    ;;

  clamav-start)
    install -d -o clamav -g clamav -m 0755 /run/clamav 2>/dev/null || true
    systemctl enable --now clamav-daemon
    echo "clamav-daemon started"
    ;;

  clamav-stop)
    systemctl disable --now clamav-daemon 2>/dev/null || systemctl stop clamav-daemon
    echo "clamav-daemon stopped"
    ;;

  clamav-scan-system)
    [[ $# -le 2 ]] || deny "usage: clamav-scan-system [path] [full|incremental]"
    scan_root="${1:-/}"
    scan_mode="${2:-full}"
    [[ "$scan_mode" == "full" || "$scan_mode" == "incremental" ]] || deny "invalid scan mode: $scan_mode"
    # Anchored allowlist: absolute, and no whitespace, quotes or shell
    # metacharacters that could survive into a log line or a filename glob.
    [[ "$scan_root" =~ ^/[A-Za-z0-9._/-]*$ ]] || deny "unsafe scan path"
    case "$scan_root" in
      ".."|"../"*|*"/.."|*"/../"*) deny "path traversal not allowed" ;;
    esac
    scan_root="$(readlink -m "$scan_root")" || deny "cannot resolve $scan_root"
    [[ -d "$scan_root" ]] || deny "scan path is not a directory: $scan_root"
    run_clamav_system_scan "$scan_root" "$scan_mode"
    ;;

  waf-update)
    write_modsec_main_conf
    restart_openlitespeed
    echo "opanel-ent lightweight WAF rules refreshed"
    ;;

  waf-default-rules)
    write_waf_default_rules
    exec cat /usr/local/lsws/conf/opanel-ent/waf/opanel-ent-default.conf
    ;;

  waf-custom-rules)
    touch /usr/local/lsws/conf/opanel-ent/waf/opanel-ent-custom.conf
    exec cat /usr/local/lsws/conf/opanel-ent/waf/opanel-ent-custom.conf
    ;;

  waf-custom-save)
    save_waf_custom_rules
    ;;
  waf-site-rules)
    [[ $# -eq 1 ]] || deny "usage: waf-site-rules <domain>"
    require_domain "$1"
    exec cat "/usr/local/lsws/conf/opanel-ent/waf/sites/${1}.conf"
    ;;
  waf-site-save)
    [[ $# -eq 1 ]] || deny "usage: waf-site-save <domain>"
    save_waf_site_rules "$1"
    ;;
  waf-site-save-defer)
    [[ $# -eq 1 ]] || deny "usage: waf-site-save-defer <domain>"
    save_waf_site_rules "$1" defer
    ;;
  # ---- PHP installation --------------------------------------------------
  php-install)
    [[ $# -eq 1 ]] || deny "usage: php-install <version>"
    install_php_version "$1"
    ;;

  php-config-write)
    [[ $# -eq 1 ]] || deny "usage: php-config-write <version>"
    write_php_config "$1"
    ;;

  php-fpm-retune)
    [[ $# -eq 0 ]] || deny "usage: php-fpm-retune"
    retune_php_fpm_pools
    ;;

  mariadb-retune)
    [[ $# -eq 0 ]] || deny "usage: mariadb-retune"
    retune_mariadb
    ;;

  # ---- panel runtime ----------------------------------------------------
  panel-url-set)
    [[ $# -eq 3 ]] || deny "usage: panel-url-set <http|https> <host> <port>"
    scheme="$1"; host="$2"; port="$3"
    require_panel_scheme "$scheme"
    require_panel_host "$host"
    require_port "$port"
    env_set PANEL_PORT "$port"
    env_set PANEL_URL "${scheme}://${host}:${port}"
    env_set ALLOWED_ORIGINS "${scheme}://${host}:${port}"
    if is_domain "$host"; then
      env_set PANEL_DOMAIN "$host"
    else
      env_set PANEL_DOMAIN ""
    fi
    if [[ "$scheme" == "http" ]]; then
      env_set PANEL_SSL_CERT ""
      env_set PANEL_SSL_KEY ""
    fi
    allow_panel_port "$port"
    refresh_tools_ols
    schedule_panel_restart
    echo "Panel URL: ${scheme}://${host}:${port}"
    ;;

  panel-ssl-install)
    [[ $# -ge 2 && $# -le 3 ]] || deny "usage: panel-ssl-install <domain> <port> [email]"
    domain="$1"; port="$2"; email="${3:-}"
    require_domain "$domain"
    require_port "$port"
    env_set PANEL_DOMAIN "$domain"
    env_set PANEL_PORT "$port"
    install -d -o root -g opanel-ent -m 0755 /var/www/opanel-ent-acme/.well-known/acme-challenge
    refresh_tools_ols
    certbot_args=(certonly --webroot -w /var/www/opanel-ent-acme
      --cert-name "$domain" \
      --agree-tos \
      --non-interactive \
      --deploy-hook "install -d -o root -g opanel-ent -m 0750 /etc/opanel-ent && install -m 0640 -o root -g opanel-ent /etc/letsencrypt/live/${domain}/fullchain.pem /etc/opanel-ent/panel-fullchain.pem && install -m 0640 -o root -g opanel-ent /etc/letsencrypt/live/${domain}/privkey.pem /etc/opanel-ent/panel-privkey.pem")
    if [[ -n "$email" ]]; then
      require_email "$email"
      certbot_args+=(--email "$email")
    else
      certbot_args+=(--register-unsafely-without-email)
    fi
    certbot_args+=(--expand -d "$domain")
    certbot "${certbot_args[@]}"
    env_set PANEL_URL "https://${domain}:${port}"
    env_set ALLOWED_ORIGINS "https://${domain}:${port}"
    copy_panel_live_certificate "$domain"
    panel_cert_store_sync
    if [[ -n "$email" ]]; then
      env_set SSL_EMAIL "$email"
    fi
    allow_panel_port "$port"
    refresh_tools_ols
    schedule_panel_restart
    echo "Panel SSL enabled: https://${domain}:${port}"
    ;;

  panel-cert-sync)
    [[ $# -eq 0 ]] || deny "usage: panel-cert-sync"
    panel_cert_store_sync
    echo "Panel certificate store synced"
    ;;

  # Regenerate only the self-signed default. The panel calls this at start-up
  # when no certificate loads, so it must stay cheap: a full store sync copies
  # every Let's Encrypt certificate on the box, which is 180 files on a busy
  # server and not what a panel trying to come up needs to wait for.
  panel-cert-selfsigned)
    [[ $# -eq 0 ]] || deny "usage: panel-cert-selfsigned"
    install -d -o root -g opanel-ent -m 0750 "$PANEL_CERT_STORE"
    rm -f "${PANEL_CERT_STORE}/_default/fullchain.pem" "${PANEL_CERT_STORE}/_default/privkey.pem"
    panel_self_signed_ensure
    ;;

  # ---- certbot ----------------------------------------------------------
  certbot-issue)
    [[ $# -ge 1 ]] || deny "usage: certbot-issue <domain> [alias-domain ...] [email]"
    domain="$1"; shift
    email=""
    domains=("$domain")
    require_domain "$domain"
    while [[ $# -gt 0 ]]; do
      if [[ "$1" == *@* ]]; then
        [[ $# -eq 1 ]] || deny "email must be the final certbot-issue argument"
        email="$1"
        shift
        break
      fi
      require_domain "$1"
      domains+=("$1")
      shift
    done
    install -d -o root -g opanel-ent -m 0755 /var/www/opanel-ent-acme/.well-known/acme-challenge
    # Ensure OLS serves ACME challenges via the webroot
    refresh_tools_ols
    args=(certonly --webroot -w /var/www/opanel-ent-acme --cert-name "$domain" --non-interactive --agree-tos --expand)
    for cert_domain in "${domains[@]}"; do
      args+=(-d "$cert_domain")
    done
    if [[ -n "$email" ]]; then
      require_email "$email"
      args+=(--email "$email")
    else
      args+=(--register-unsafely-without-email)
    fi
    certbot "${args[@]}"
    restart_openlitespeed
    copy_panel_live_certificate "$domain"
    panel_cert_store_sync
    echo "SSL certificate issued for ${domain}"
    ;;

  certbot-renew)
    certbot renew --quiet
    panel_cert_store_sync
    schedule_panel_restart
    ;;
  certbot-renew-soon)
    [[ $# -le 1 ]] || deny "usage: certbot-renew-soon [days]"
    renew_ssl_soon "${1:-10}"
    ;;
  certbot-auto-renew-install)
    write_ssl_auto_renew_timer
    echo "SSL auto-renew timer installed"
    ;;
  certbot-dns-cloudflare)
    [[ $# -ge 1 && $# -le 2 ]] || deny "usage: certbot-dns-cloudflare <domain> [email]   (token on stdin)"
    issue_cloudflare_wildcard "$1" "${2:-}"
    ;;
  certbot-dns-cloudflare-remove)
    [[ $# -eq 1 ]] || deny "usage: certbot-dns-cloudflare-remove <domain>"
    remove_cloudflare_wildcard "$1"
    ;;
  manual-ssl-install)
    [[ $# -eq 1 ]] || deny "usage: manual-ssl-install <domain>"
    install_manual_ssl "$1"
    ;;
  manual-ssl-remove)
    [[ $# -eq 1 ]] || deny "usage: manual-ssl-remove <domain>"
    remove_manual_ssl "$1"
    ;;

  # ---- firewall (iptables) ---------------------------------------------
  iptables-status)
    echo "Chains: OPANEL_ENT_INPUT, OPANEL_ENT_USER, OPANEL_ENT_BLOCKLIST"
    echo ""
    echo "=== OPANEL_ENT_INPUT ==="
    iptables -L OPANEL_ENT_INPUT -n --line-numbers 2>/dev/null || echo "  (chain not found)"
    echo ""
    echo "=== OPANEL_ENT_USER ==="
    iptables -L OPANEL_ENT_USER -n --line-numbers 2>/dev/null || echo "  (chain not found)"
    echo ""
    echo "=== OPANEL_ENT_BLOCKLIST ==="
    iptables -L OPANEL_ENT_BLOCKLIST -n --line-numbers 2>/dev/null || echo "  (chain not found)"
    echo ""
    echo "=== ipsets ==="
    total="$(ipset list "$BLOCKLIST_IPSET_V4" 2>/dev/null | grep -Ec '^[0-9]' || true)"
    echo "  ${BLOCKLIST_IPSET_V4}: ${total:-0} network(s)"
    total="$(ipset list "$BLOCKLIST_IPSET_V6" 2>/dev/null | grep -Ec '^[0-9a-fA-F:]+/' || true)"
    echo "  ${BLOCKLIST_IPSET_V6}: ${total:-0} network(s)"
    ;;
  iptables-check-enabled)
    if iptables -C INPUT -j OPANEL_ENT_BLOCKLIST 2>/dev/null && \
       iptables -C INPUT -j OPANEL_ENT_INPUT 2>/dev/null && \
       iptables -C INPUT -j OPANEL_ENT_USER 2>/dev/null; then
      echo yes
    else
      echo no
    fi
    ;;
  iptables-rules-store-ensure)
    ensure_firewall_rule_store
    echo "opanel-ent firewall rule store ready"
    ;;
  iptables-enable)
    iptables -P INPUT ACCEPT 2>/dev/null || true
    ip6tables -P INPUT ACCEPT 2>/dev/null || true
    iptables_flush_managed_chains
    iptables_reorder_managed_jumps
    iptables_add_default_allowances
    firewall_blocklist_apply 2>/dev/null || true
    echo "opanel-ent iptables chains enabled"
    ;;
  iptables-persist)
    [[ $# -eq 0 ]] || deny "usage: iptables-persist"
    firewall_persist_rules
    echo "opanel-ent firewall rules persisted"
    ;;
  iptables-disable)
    # Remove chain references from INPUT (rules inside chains are preserved)
    iptables -D INPUT -j OPANEL_ENT_BLOCKLIST 2>/dev/null || true
    iptables -D INPUT -j OPANEL_ENT_INPUT 2>/dev/null || true
    iptables -D INPUT -j OPANEL_ENT_USER 2>/dev/null || true
    ip6tables -D INPUT -j OPANEL_ENT_BLOCKLIST 2>/dev/null || true
    ip6tables -D INPUT -j OPANEL_ENT_INPUT 2>/dev/null || true
    ip6tables -D INPUT -j OPANEL_ENT_USER 2>/dev/null || true
    iptables -P INPUT ACCEPT 2>/dev/null || true
    ip6tables -P INPUT ACCEPT 2>/dev/null || true
    firewall_persist_rules
    echo "opanel-ent iptables chains disconnected from INPUT"
    ;;
  iptables-reload)
    firewall_blocklist_apply 2>/dev/null || true
    firewall_persist_rules
    echo "opanel-ent iptables rules reloaded"
    ;;
  iptables-run)
    run_managed_iptables_command iptables "$@"
    ;;
  ip6tables-run)
    run_managed_iptables_command ip6tables "$@"
    ;;
  ipset-run)
    run_managed_ipset_command "$@"
    ;;
  iptables-allow-port)
    [[ $# -eq 2 ]] || deny "usage: iptables-allow-port <port> <proto>"
    require_port "$1"; require_proto "$2"
    iptables -A OPANEL_ENT_USER -p "$2" --dport "$1" -j ACCEPT -m comment --comment "opanel-ent:UserZone" 2>/dev/null \
      || iptables -A OPANEL_ENT_USER -p "$2" --dport "$1" -j ACCEPT 2>/dev/null \
      || true
    ;;
  iptables-panel-allow-port)
    [[ $# -eq 1 ]] || deny "usage: iptables-panel-allow-port <port>"
    allow_panel_port "$1"
    ;;
  iptables-allow-ip)
    [[ $# -ge 1 && $# -le 3 ]] || deny "usage: iptables-allow-ip <ip> [port] [proto]"
    run_ip_rule allow "$1" "${2:-}" "${3:-tcp}"
    ;;
  iptables-deny-ip)
    [[ $# -ge 1 && $# -le 3 ]] || deny "usage: iptables-deny-ip <ip> [port] [proto]"
    run_ip_rule deny "$1" "${2:-}" "${3:-tcp}"
    ;;
  iptables-delete)
    [[ $# -eq 1 && "$1" =~ ^[0-9]+$ ]] || deny "usage: iptables-delete <rule-number>"
    iptables -D OPANEL_ENT_USER "$1" 2>/dev/null \
      || iptables -D OPANEL_ENT_INPUT "$1" 2>/dev/null \
      || deny "could not delete rule $1"
    echo "Rule $1 deleted"
    ;;

  # ---- firewall blocklist -----------------------------------------------
  firewall-blocklist-status|iptables-blocklist-status|blocklist-status|nginx-blocklist-status)
    firewall_blocklist_status
    ;;
  firewall-blocklist-timer-install|iptables-blocklist-timer-install|blocklist-timer-install|nginx-blocklist-timer-install)
    firewall_blocklist_write_timer
    echo "Blocklist timer installed"
    ;;
  firewall-blocklist-add|iptables-blocklist-add|blocklist-add|nginx-blocklist-add)
    [[ $# -eq 1 ]] || deny "usage: firewall-blocklist-add <url>"
    firewall_blocklist_add_url "$1"
    ;;
  firewall-blocklist-delete|iptables-blocklist-delete|blocklist-delete|nginx-blocklist-delete)
    [[ $# -eq 1 ]] || deny "usage: firewall-blocklist-delete <url>"
    firewall_blocklist_delete_url "$1"
    ;;
  firewall-blocklist-run|iptables-blocklist-run|blocklist-run|nginx-blocklist-run)
    [[ $# -eq 0 ]] || deny "usage: firewall-blocklist-run"
    firewall_blocklist_run
    ;;

  # ---- filesystem -------------------------------------------------------
  chown-www)
    deny "chown-www has been removed: site files belong to the site's own Linux user, use fix-permissions"
    ;;

  fix-permissions)
    [[ $# -ge 1 && $# -le 2 ]] || deny "usage: fix-permissions <path> [site-user]"
    target=$(require_managed_path "$1" "${2:-}")
    site_user="${2:-}"
    if [[ -z "$site_user" ]]; then
      # Every managed site lives at /home/<site-user>/<domain>, so the owning
      # user is the first path segment. Deriving it beats the old behaviour of
      # falling back to www-data, which handed one shared account ownership of
      # the tree and broke isolation between sites.
      site_user="${target#${HOME_ROOT}/}"
      site_user="${site_user%%/*}"
    fi
    require_linux_user "$site_user"
    fix_site_tree "$target" "$site_user"
    ;;

  site-path-fix)
    [[ $# -eq 2 ]] || deny "usage: site-path-fix <path> <site-user>"
    target=$(require_managed_path "$1" "$2")
    fix_site_tree "$target" "$2"
    ;;

  site-document-root-ensure)
    [[ $# -eq 3 ]] || deny "usage: site-document-root-ensure <site-user> <site-root> <relative-path>"
    user="$1"; root_arg="$2"; rel_arg="$3"
    ensure_sites_group
    require_linux_user "$user"
    root_target=$(require_managed_path "$root_arg" "$user")
    [[ "$rel_arg" =~ ^[A-Za-z0-9._-]+(/[A-Za-z0-9._-]+)*$ ]] || deny "unsafe relative path: $rel_arg"
    case "$rel_arg" in
      ""|"/"|/*|*$'\n'*|"."|".."|"./"*|"../"*|*"/."|*"/.."|*"/./"*|*"/../"*) deny "unsafe relative path: $rel_arg" ;;
    esac
    target=$(require_safe_path "$root_target" "$root_target/$rel_arg")
    mkdir -p -- "$target"
    harden_site_dir_path "$root_target" "$target" "$user"
    ;;

  site-file-write)
    [[ $# -eq 3 || $# -eq 4 ]] || deny "usage: site-file-write <site-user> <site-root> <relative-path> [0644|0640]"
    user="$1"; root_arg="$2"; rel_arg="$3"; mode_arg="${4:-0644}"
    require_linux_user "$user"
    [[ "$mode_arg" == "0644" || "$mode_arg" == "0640" ]] || deny "invalid file mode: $mode_arg"
    root_target=$(require_managed_path "$root_arg" "$user")
    case "$rel_arg" in
      ""|"/"|/*|*$'\n'*|".."|"../"*|*"/.."|*"/../"*) deny "unsafe relative path: $rel_arg" ;;
    esac
    target=$(require_safe_path "$root_target" "$root_target/$rel_arg")
    [[ -d "$target" ]] && deny "cannot write a directory: $target"
    [[ -L "$target" ]] && deny "refusing to write through a symlink: $target"
    parent=$(dirname -- "$target")
    runuser -u "$user" -- mkdir -p -- "$parent"
    harden_site_dir_path "$root_target" "$parent" "$user"
    existing_mode=""
    if [[ -e "$target" ]]; then
      existing_mode=$(stat -c '%a' -- "$target")
    fi
    base=$(basename -- "$target")
    tmp="$parent/.${base}.opanel-ent-write-$$"
    rm -f -- "$tmp"
    cat >"$tmp"
    chown "$user:$user" "$tmp"
    chmod "$mode_arg" "$tmp"
    mv -f -- "$tmp" "$target"
    ;;

  site-file-install)
    [[ $# -eq 4 ]] || deny "usage: site-file-install <site-user> <site-root> <relative-path> <staged-path>"
    user="$1"; root_arg="$2"; rel_arg="$3"; staged_arg="$4"
    require_linux_user "$user"
    root_target=$(require_managed_path "$root_arg" "$user")
    case "$rel_arg" in
      ""|"/"|/*|*$'\n'*|".."|"../"*|*"/.."|*"/../"*) deny "unsafe relative path: $rel_arg" ;;
    esac
    target=$(require_safe_path "$root_target" "$root_target/$rel_arg")
    [[ ! -L "$target" ]] || deny "refusing to write through a symlink: $target"
    [[ "$staged_arg" == /tmp/opanel-ent-upload-* ]] || deny "invalid staged upload path"
    [[ ! -L "$staged_arg" ]] || deny "staged upload cannot be a symlink"
    staged=$(readlink -e -- "$staged_arg") || deny "staged upload not found"
    [[ "$staged" == /tmp/opanel-ent-upload-* && -f "$staged" ]] || deny "invalid staged upload"
    [[ "$(stat -c '%U' -- "$staged")" == "opanel-ent" ]] || deny "staged upload must be owned by opanel-ent"
    parent=$(dirname -- "$target")
    runuser -u "$user" -- mkdir -p -- "$parent"
    harden_site_dir_path "$root_target" "$parent" "$user"
    base=$(basename -- "$target")
    tmp="$parent/.${base}.opanel-ent-install-$$"
    rm -f -- "$tmp"
    install -o "$user" -g "$user" -m 0644 -- "$staged" "$tmp"
    mv -f -- "$tmp" "$target"
    rm -f -- "$staged"
    ;;

  site-file-delete)
    [[ $# -eq 3 ]] || deny "usage: site-file-delete <site-user> <site-root> <relative-path>"
    user="$1"; root_arg="$2"; rel_arg="$3"
    require_linux_user "$user"
    root_target=$(require_managed_path "$root_arg" "$user")
    case "$rel_arg" in
      ""|"/"|/*|".."|"../"*|*"/.."|*"/../"*) deny "unsafe relative path: $rel_arg" ;;
    esac
    target=$(require_safe_path "$root_target" "$root_target/$rel_arg")
    [[ ! -L "$target" ]] || deny "refusing to delete through a symlink: $target"
    rm -f -- "$target"
    ;;

  site-backup-restore)
    [[ $# -eq 5 ]] || deny "usage: site-backup-restore <site-user> <site-root> <backup-path> <max-items> <max-bytes>"
    user="$1"; root_arg="$2"; archive_arg="$3"; max_items="$4"; max_bytes="$5"
    require_linux_user "$user"
    [[ "$max_items" =~ ^[0-9]+$ && "$max_bytes" =~ ^[0-9]+$ ]] || deny "invalid restore limits"
    root_target=$(require_managed_path "$root_arg" "$user")
    backup_root="$(env_get BACKUP_ROOT)"
    [[ -n "$backup_root" ]] || backup_root="/var/backups/opanel-ent"
    backup_root=$(readlink -m "$backup_root") || deny "cannot resolve backup root"
    archive_target=$(require_safe_path "$backup_root" "$archive_arg")
    [[ -f "$archive_target" && ! -L "$archive_target" ]] || deny "backup archive not found"
    python3 - "$archive_target" "$root_target" "$max_items" "$max_bytes" <<'PY'
import os
import shutil
import sys
import tarfile

archive_path, destination, max_items, max_bytes = sys.argv[1:]
max_items = int(max_items)
max_bytes = int(max_bytes)
destination = os.path.realpath(destination)


def normalized_name(name, has_site_prefix):
    if "\x00" in name:
        raise ValueError("backup contains an unsafe path")
    name = name.replace("\\", "/")
    if name.startswith("database/"):
        return None
    if has_site_prefix:
        if name == "site" or not name.startswith("site/"):
            return None
        name = name[len("site/"):]
    parts = [part for part in name.split("/") if part not in ("", ".")]
    if not parts or any(part == ".." for part in parts) or name.startswith("/"):
        raise ValueError("backup contains an unsafe path")
    if ":" in parts[0]:
        raise ValueError("backup contains an absolute path")
    return os.path.join(*parts)


with tarfile.open(archive_path, "r:gz") as archive:
    members = archive.getmembers()
    has_site_prefix = any(member.name == "site" or member.name.startswith("site/") for member in members)
    selected = []
    total = 0
    for member in members:
        name = normalized_name(member.name, has_site_prefix)
        if name is None:
            continue
        if member.issym() or member.islnk() or member.isdev() or not (member.isdir() or member.isfile()):
            raise ValueError("backup links and special files are not allowed")
        target = os.path.abspath(os.path.join(destination, name))
        resolved = os.path.realpath(target)
        if os.path.commonpath((destination, resolved)) != destination:
            raise ValueError("backup path escapes website root")
        selected.append((member, target))
        total += member.size if member.isfile() else 0
        if max_items and len(selected) > max_items:
            raise ValueError("backup has too many files")
        if max_bytes and total > max_bytes:
            raise ValueError("backup is too large")

    for member, target in selected:
        if member.isdir():
            if os.path.islink(target):
                raise ValueError("backup destination contains an unsafe symlink")
            if os.path.lexists(target) and not os.path.isdir(target):
                os.unlink(target)
            os.makedirs(target, exist_ok=True)
            continue
        parent = os.path.dirname(target)
        os.makedirs(parent, exist_ok=True)
        if os.path.islink(target):
            raise ValueError("backup destination contains an unsafe symlink")
        if os.path.isdir(target):
            shutil.rmtree(target)
        source = archive.extractfile(member)
        if source is None:
            raise ValueError("backup entry cannot be read")
        descriptor = os.open(target, os.O_WRONLY | os.O_CREAT | os.O_TRUNC | os.O_NOFOLLOW, 0o600)
        with source, os.fdopen(descriptor, "wb") as output:
            shutil.copyfileobj(source, output, length=1024 * 1024)
PY
    fix_site_tree "$root_target" "$user"
    ;;

  site-import-copy)
    # Copies an already-extracted directory tree (built by an unprivileged
    # opanel-ent-api staging step, e.g. DirectAdmin backup import) into a site's
    # document root. The site directory is created chown'd to the site's own
    # Linux user (see site-document-root-ensure), so opanel-ent-api itself has no
    # write access to it — this subcommand is the privileged trampoline that
    # places the files, mirroring how site-backup-restore places a raw
    # archive's contents.
    [[ $# -eq 4 ]] || deny "usage: site-import-copy <site-user> <site-root> <relative-path> <staging-source-dir>"
    user="$1"; root_arg="$2"; rel_arg="$3"; source_arg="$4"
    require_linux_user "$user"
    root_target=$(require_managed_path "$root_arg" "$user")
    case "$rel_arg" in
      ""|"/"|/*|*$'\n'*|"."|".."|"./"*|"../"*|*"/."|*"/.."|*"/./"*|*"/../"*) deny "unsafe relative path: $rel_arg" ;;
    esac
    target=$(require_safe_path "$root_target" "$root_target/$rel_arg")
    mkdir -p -- "$target"
    source_target=$(readlink -m "$source_arg") || deny "cannot resolve staging source"
    case "$source_target" in
      /tmp/opanel-ent-da-import-*) ;;
      *) deny "staging source outside expected da-import temp dir: $source_target" ;;
    esac
    [[ -d "$source_target" && ! -L "$source_target" ]] || deny "staging source not found"
    python3 - "$source_target" "$target" <<'PY'
import os
import shutil
import sys

source, destination = sys.argv[1:3]
source = os.path.realpath(source)
destination = os.path.realpath(destination)


def _contained(path):
    return os.path.commonpath((destination, path)) == destination


for root, dirs, files in os.walk(source):
    dirs[:] = [d for d in dirs if not os.path.islink(os.path.join(root, d))]
    rel = os.path.relpath(root, source)
    target_dir = destination if rel == "." else os.path.abspath(os.path.join(destination, rel))
    if not _contained(target_dir):
        raise ValueError("import path escapes website root")
    if os.path.islink(target_dir):
        raise ValueError("import destination contains an unsafe symlink")
    os.makedirs(target_dir, exist_ok=True)
    for name in files:
        src = os.path.join(root, name)
        if os.path.islink(src):
            continue
        dst = os.path.abspath(os.path.join(target_dir, name))
        if not _contained(dst):
            raise ValueError("import path escapes website root")
        if os.path.islink(dst):
            raise ValueError("import destination contains an unsafe symlink")
        if os.path.isdir(dst):
            shutil.rmtree(dst)
        descriptor = os.open(dst, os.O_WRONLY | os.O_CREAT | os.O_TRUNC | os.O_NOFOLLOW, 0o600)
        with open(src, "rb") as handle, os.fdopen(descriptor, "wb") as output:
            shutil.copyfileobj(handle, output, length=1024 * 1024)
PY
    fix_site_tree "$root_target" "$user"
    ;;

  site-archive-extract)
    [[ $# -eq 7 ]] || deny "usage: site-archive-extract <site-user> <site-root> <archive-path> <destination-path> <zip|tar.gz> <max-items> <max-bytes>"
    user="$1"; root_arg="$2"; archive_rel="$3"; destination_rel="$4"; archive_kind="$5"
    max_items="$6"; max_bytes="$7"
    require_linux_user "$user"
    [[ "$archive_kind" == "zip" || "$archive_kind" == "tar.gz" ]] || deny "unsupported archive type"
    [[ "$max_items" =~ ^[0-9]+$ && "$max_bytes" =~ ^[0-9]+$ ]] || deny "invalid archive limits"
    root_target=$(require_managed_path "$root_arg" "$user")
    archive_target=$(require_safe_path "$root_target" "$root_target/$archive_rel")
    destination_target=$(require_safe_path "$root_target" "$root_target/$destination_rel")
    [[ -f "$archive_target" && ! -L "$archive_target" ]] || deny "archive not found"
    [[ -d "$destination_target" && ! -L "$destination_target" ]] || deny "archive destination not found"
    tmp_archive=$(mktemp "/tmp/opanel-ent-extract-XXXXXX")
    trap 'rm -f -- "$tmp_archive"' EXIT
    install -o "$user" -g "$user" -m 0600 -- "$archive_target" "$tmp_archive"
    runuser -u "$user" -- python3 - "$tmp_archive" "$archive_kind" "$destination_target" "$max_items" "$max_bytes" "$archive_target" <<'PY'
import os
import shutil
import stat
import sys
import tarfile
import zipfile

archive_path, archive_kind, destination = sys.argv[1:4]
max_items, max_bytes = int(sys.argv[4]), int(sys.argv[5])
source_archive = os.path.realpath(sys.argv[6])
destination = os.path.realpath(destination)

# Symlinks, hardlinks and device nodes are never recreated from an archive --
# they are the classic write-through-a-link escalation vector. Rather than
# rejecting the whole archive when it contains one (a stock WordPress export
# ships e.g. Query Monitor's wp-content/db.php symlink), skip those entries and
# extract everything else. Count them so the extraction can say what it dropped.
skipped_specials = 0


def safe_target(name):
    """Normalize backslash paths and resolve to a safe absolute path."""
    if "\x00" in name:
        raise ValueError("archive contains an unsafe path")
    normalized = name.replace("\\", "/")
    if normalized.startswith("/") or ":" in normalized.split("/", 1)[0]:
        raise ValueError("archive contains an absolute path")
    parts = [part for part in normalized.split("/") if part not in ("", ".")]
    if not parts or any(part == ".." for part in parts):
        raise ValueError("archive contains an unsafe path")
    target = os.path.abspath(os.path.join(destination, *parts))
    resolved = os.path.realpath(target)
    if os.path.commonpath((destination, resolved)) != destination:
        raise ValueError("archive path escapes destination")
    return target, resolved


def zip_implied_dirs(infos):
    implied = set()
    for info in infos:
        parts = [part for part in info.filename.replace("\\", "/").split("/") if part not in ("", ".")]
        for index in range(1, len(parts)):
            implied.add("/".join(parts[:index]))
    return implied


def _is_dir_entry(info, implied_dirs):
    """Return True if a ZipInfo represents a directory."""
    normalized = info.filename.replace("\\", "/")
    if info.is_dir() or normalized.endswith("/"):
        return True
    mode = (info.external_attr >> 16) & 0o170000
    if stat.S_ISDIR(mode) and info.file_size == 0:
        return True
    if info.file_size == 0 and normalized.rstrip("/") in implied_dirs:
        return True
    return False


def is_source_archive(resolved):
    return resolved == source_archive


def ensure_regular_target(target):
    if os.path.islink(target):
        raise ValueError("refusing to overwrite a symlink")
    if os.path.isdir(target):
        raise ValueError("archive file conflicts with an existing directory")


def ensure_directory_target(target):
    if os.path.islink(target):
        raise ValueError("refusing to overwrite a symlink")
    if os.path.exists(target) and not os.path.isdir(target):
        try:
            if os.path.getsize(target) == 0:
                return
        except OSError:
            pass
        raise ValueError("archive directory conflicts with an existing file")


def validate_zip():
    global skipped_specials
    count = 0
    total = 0
    with zipfile.ZipFile(archive_path) as archive:
        infos = archive.infolist()
        implied_dirs = zip_implied_dirs(infos)
        for info in infos:
            count += 1
            if max_items and count > max_items:
                raise ValueError("archive has too many files")
            target, resolved = safe_target(info.filename)
            mode = (info.external_attr >> 16) & 0o170000
            if stat.S_ISLNK(mode):
                skipped_specials += 1
                continue
            if is_source_archive(resolved):
                continue
            if _is_dir_entry(info, implied_dirs):
                ensure_directory_target(target)
                continue
            ensure_regular_target(target)
            total += info.file_size
            if max_bytes and total > max_bytes:
                raise ValueError("archive is too large")


def extract_zip():
    with zipfile.ZipFile(archive_path) as archive:
        infos = archive.infolist()
        implied_dirs = zip_implied_dirs(infos)
        for info in infos:
            if stat.S_ISLNK((info.external_attr >> 16) & 0o170000):
                continue
            target, resolved = safe_target(info.filename)
            if is_source_archive(resolved):
                continue
            if _is_dir_entry(info, implied_dirs):
                if os.path.exists(target) and not os.path.isdir(target):
                    os.unlink(target)
                os.makedirs(target, exist_ok=True)
                continue
            os.makedirs(os.path.dirname(target), exist_ok=True)
            try:
                with archive.open(info) as src, open(target, "wb") as dst:
                    shutil.copyfileobj(src, dst, length=1024 * 1024)
            except RuntimeError as exc:
                raise ValueError("archive entry cannot be extracted") from exc


def validate_tar():
    global skipped_specials
    count = 0
    total = 0
    with tarfile.open(archive_path, "r:gz") as archive:
        for member in archive:
            count += 1
            if max_items and count > max_items:
                raise ValueError("archive has too many files")
            if member.issym() or member.islnk() or member.isdev() or member.isfifo():
                skipped_specials += 1
                continue
            target, resolved = safe_target(member.name)
            if is_source_archive(resolved):
                continue
            if not member.isdir() and not member.isfile():
                skipped_specials += 1
                continue
            if member.isdir():
                ensure_directory_target(target)
                continue
            ensure_regular_target(target)
            total += member.size
            if max_bytes and total > max_bytes:
                raise ValueError("archive is too large")


def extract_tar():
    with tarfile.open(archive_path, "r:gz") as archive:
        for member in archive:
            if member.issym() or member.islnk() or member.isdev() or member.isfifo():
                continue
            target, resolved = safe_target(member.name)
            if is_source_archive(resolved):
                continue
            if member.isdir():
                os.makedirs(target, exist_ok=True)
                continue
            if not member.isfile():
                continue
            source = archive.extractfile(member)
            if source is None:
                continue
            os.makedirs(os.path.dirname(target), exist_ok=True)
            with source, open(target, "wb") as dst:
                shutil.copyfileobj(source, dst, length=1024 * 1024)


if archive_kind == "zip":
    validate_zip()
    extract_zip()
else:
    validate_tar()
    extract_tar()

if skipped_specials:
    sys.stderr.write(
        "opanel-ent: skipped %d symlink/special entr%s (not extracted for safety)\n"
        % (skipped_specials, "y" if skipped_specials == 1 else "ies")
    )
PY
    # The archive may contain an entry with its own filename. Restore the
    # original source archive after extraction so it cannot overwrite itself.
    install -o "$user" -g "$user" -m 0644 -- "$tmp_archive" "$archive_target"
    fix_site_tree "$destination_target" "$user"
    rm -f -- "$tmp_archive"
    trap - EXIT
    ;;

  panel-user-ensure)
    [[ $# -eq 1 ]] || deny "usage: panel-user-ensure <panel-user>"
    ensure_panel_user_home "$1"
    ;;

  panel-user-password)
    [[ $# -eq 1 ]] || deny "usage: panel-user-password <panel-user>"
    set_panel_user_password "$1"
    ;;

  panel-user-delete)
    [[ $# -eq 1 ]] || deny "usage: panel-user-delete <panel-user>"
    delete_panel_user_runtime "$1"
    ;;

  site-runtime-ensure)
    [[ $# -eq 3 ]] || deny "usage: site-runtime-ensure <site-user> <path> <php-version|none>"
    user="$1"; path="$2"; php_version="$3"
    require_linux_user "$user"
    target=$(require_managed_path "$path" "$user")
    ensure_panel_user_home "$user"
    if [[ -d "$target/public" && ! -e "$target/public_html" ]]; then
      mv "$target/public" "$target/public_html"
    elif [[ -d "$target/public" && -d "$target/public_html" && -z "$(find "$target/public_html" -mindepth 1 -maxdepth 1 -print -quit)" ]]; then
      rmdir "$target/public_html"
      mv "$target/public" "$target/public_html"
    fi
    mkdir -p "$target/public_html"
    harden_site_dir_path "$target" "$target/public_html" "$user"
    fix_site_tree "$target" "$user"
    ensure_php_pool "$user" "$target" "$php_version"
    ;;

  site-runtime-move)
    [[ $# -eq 4 ]] || deny "usage: site-runtime-move <site-user> <old-path> <new-path> <php-version|none>"
    user="$1"; old_path="$2"; new_path="$3"; php_version="$4"
    require_linux_user "$user"
    old_target=$(require_managed_path "$old_path")
    new_target=$(require_managed_path "$new_path" "$user")
    old_user="${old_target#${HOME_ROOT}/}"
    old_user="${old_user%%/*}"
    ensure_panel_user_home "$user"
    if [[ "$old_target" != "$new_target" ]]; then
      [[ ! -e "$new_target" ]] || deny "target path already exists: $new_target"
      delete_site_php_pools "$old_user" "$old_target"
      mkdir -p "$(dirname "$new_target")"
      mv "$old_target" "$new_target"
    fi
    if [[ -d "$new_target/public" && ! -e "$new_target/public_html" ]]; then
      mv "$new_target/public" "$new_target/public_html"
    fi
    mkdir -p "$new_target/public_html"
    harden_site_dir_path "$new_target" "$new_target/public_html" "$user"
    fix_site_tree "$new_target" "$user"
    ensure_php_pool "$user" "$new_target" "$php_version"
    ;;

  site-runtime-delete)
    [[ $# -eq 2 ]] || deny "usage: site-runtime-delete <site-user> <path>"
    user="$1"; path="$2"
    require_linux_user "$user"
    target=$(require_managed_path "$path" "$user")
    delete_site_php_pools "$user" "$target"
    exec rm -rf "$target"
    ;;

  rm-site)
    [[ $# -eq 3 ]] || deny "usage: rm-site <site-user> <site-root> <path>"
    user="$1"; root="$2"; path="$3"
    target=$(require_bound_managed_path "$user" "$root" "$path")
    delete_no_follow "$user" "$root" "$target"
    ;;

  mkdir-site)
    [[ $# -eq 1 ]] || deny "usage: mkdir-site <path>"
    target=$(require_managed_path "$1")
    install -d -o www-data -g www-data -m 0750 "$target"
    install -d -o www-data -g www-data -m 0750 "$target/public_html"
    ;;

  site-log-read)
    [[ $# -eq 3 ]] || deny "usage: site-log-read <domain> <access|error> <lines>"
    read_site_log "$1" "$2" "$3"
    ;;

  waf-access-log-read)
    [[ $# -ge 2 ]] || deny "usage: waf-access-log-read <lines> <domain>..."
    read_waf_access_logs "$@"
    ;;

  waf-access-log-clear)
    [[ $# -ge 1 ]] || deny "usage: waf-access-log-clear <domain>..."
    clear_waf_access_logs "$@"
    ;;

  # ---- WP-CLI as www-data ----------------------------------------------
  # pcre.jit=0 + opcache.jit=disable: the ionCube loader sets a user opcode
  # handler, which the opcache JIT refuses ("JIT is incompatible with third
  # party extensions..."). Turning JIT off up front keeps wp-cli output clean.
  wp)
    [[ $# -ge 1 ]] || deny "usage: wp <args...>"
    exec runuser -u www-data -- env HOME=/var/www WP_CLI_PHP_ARGS='-d pcre.jit=0 -d opcache.jit=disable' php -d pcre.jit=0 -d opcache.jit=disable /usr/local/bin/wp "$@"
    ;;

  wp-site)
    [[ $# -ge 2 ]] || deny "usage: wp-site <site-user> <args...>"
    user="$1"; shift
    require_linux_user "$user"
    exec runuser -u "$user" -- env HOME="$HOME_ROOT/$user" WP_CLI_PHP_ARGS='-d pcre.jit=0 -d opcache.jit=disable' php -d pcre.jit=0 -d opcache.jit=disable /usr/local/bin/wp "$@"
    ;;

  # ---- crontab managed for www-data ------------------------------------
  cron-list)
    user="${1:-www-data}"
    if [[ "$user" != "www-data" ]]; then require_linux_user "$user"; fi
    exec runuser -u "$user" -- crontab -l 2>/dev/null
    ;;
  cron-write)
    # crontab content is fed via stdin
    user="${1:-www-data}"
    if [[ "$user" != "www-data" ]]; then require_linux_user "$user"; fi
    exec runuser -u "$user" -- crontab -
    ;;

  # ---- service status (read-only, no privilege change needed but useful)
  service-status)
    [[ $# -eq 1 ]] || deny "usage: service-status <service>"
    is_allowed_service "$1" || deny "service not allowed: $1"
    exec systemctl status "$1" --no-pager
    ;;

  # ---- terminal command execution as panel Linux user ------------------
  terminal-exec)
    # Execute a whitelisted command as the panel Linux user
    # Args: <site-user> <cwd> [--php-version=<version>] <command> [args...]
    [[ $# -ge 3 ]] || deny "usage: terminal-exec <site-user> <cwd> [--php-version=<version>] <command> [args...]"
    user="$1"; cwd_arg="$2"; shift 2
    php_version=""
    if [[ "${1:-}" == --php-version=* ]]; then
      php_version="${1#--php-version=}"
      require_php_version "$php_version"
      shift
    fi
    [[ $# -ge 1 ]] || deny "usage: terminal-exec <site-user> <cwd> [--php-version=<version>] <command> [args...]"
    cmd="$1"; shift
    require_linux_user "$user"
    id -u "$user" >/dev/null 2>&1 || deny "panel Linux user does not exist: $user"
    target=$(require_terminal_cwd "$cwd_arg" "$user")

    install -d -o "$user" -g "$user" -m 0700 "$HOME_ROOT/$user/.composer" "$HOME_ROOT/$user/.npm"
    # Validate cwd exists immediately before cd to avoid TOCTOU
    [[ -d "$target" ]] || deny "working directory does not exist: $target"
    cd "$target" || deny "failed to change to working directory: $target"
    umask 027
    terminal_env=(
      "HOME=$HOME_ROOT/$user"
      "COMPOSER_HOME=$HOME_ROOT/$user/.composer"
      "npm_config_cache=$HOME_ROOT/$user/.npm"
      "PATH=/usr/local/bin:/usr/bin:/bin"
    )
    php_bin="php"
    if [[ -n "$php_version" ]]; then
      lsphp_php_bin="/usr/local/lsws/lsphp${php_version//./}/bin/php"
      if [[ -x "$lsphp_php_bin" ]]; then
        php_bin="$lsphp_php_bin"
      else
        deny "PHP CLI is not installed: lsphp${php_version//./}"
      fi
    fi

    # Whitelist of allowed commands for terminal access
    case "$cmd" in
      php)
        exec runuser -u "$user" -- env "${terminal_env[@]}" "$php_bin" "$@"
        ;;
      composer)
        composer_bin="$(command -v composer || true)"
        [[ -n "$composer_bin" ]] || deny "composer not found"
        exec runuser -u "$user" -- env "${terminal_env[@]}" "$php_bin" "$composer_bin" "$@"
        ;;
      phpunit)
        phpunit_bin="$(command -v phpunit || true)"
        [[ -n "$phpunit_bin" ]] || deny "phpunit not found"
        exec runuser -u "$user" -- env "${terminal_env[@]}" "$php_bin" "$phpunit_bin" "$@"
        ;;
      node|npm|npx|yarn|git)
        exec runuser -u "$user" -- env "${terminal_env[@]}" "$cmd" "$@"
        ;;
      ls|cat|mkdir|rm|cp|mv|chmod|chown|grep|find|tar|zip|unzip|diff|head|tail|less|du|df)
        require_terminal_path_args "$user" "$target" "$@"
        exec runuser -u "$user" -- env "${terminal_env[@]}" "$cmd" "$@"
        ;;
      pwd|echo|touch|date|whoami|which|clear)
        if [[ "$cmd" == "touch" ]]; then
          require_terminal_path_args "$user" "$target" "$@"
        fi
        exec runuser -u "$user" -- env "${terminal_env[@]}" "$cmd" "$@"
        ;;
      curl|wget)
        require_terminal_download_args "$user" "$target" "$@"
        exec runuser -u "$user" -- env "${terminal_env[@]}" "$cmd" "$@"
        ;;
      artisan)
        # artisan is a PHP script, executed via php
        [[ -f artisan ]] || deny "artisan not found in $target"
        exec runuser -u "$user" -- env "${terminal_env[@]}" "$php_bin" artisan "$@"
        ;;
      wp)
        # WP-CLI as the site user with the selected PHP version. JIT off (pcre
        # + opcache) -- the ionCube loader's user opcode handler is incompatible
        # with the opcache JIT and otherwise prints a warning on every command.
        [[ -x /usr/local/bin/wp ]] || deny "wp-cli not found"
        exec runuser -u "$user" -- env "${terminal_env[@]}" WP_CLI_PHP_ARGS='-d pcre.jit=0 -d opcache.jit=disable' "$php_bin" -d pcre.jit=0 -d opcache.jit=disable /usr/local/bin/wp "$@"
        ;;
      *)
        echo "Command not allowed: $cmd" >&2
        echo "Allowed commands: php, composer, artisan, wp, node, npm, npx, yarn, git, phpunit, ls, cat, mkdir, rm, cp, mv, chmod, chown, pwd, echo, touch, grep, find, tar, zip, unzip, curl, wget, diff, head, tail, less, du, df, date, whoami, which, clear" >&2
        exit 126
        ;;
    esac
    ;;

  *)
    deny "unknown command: $cmd"
    ;;
esac
