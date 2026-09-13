#!/usr/bin/env bash
set -euo pipefail

if [[ $EUID -ne 0 ]]; then
  echo "Please run this installer as root"
  exit 1
fi

if [[ ! -f /etc/os-release ]]; then
  echo "Cannot find /etc/os-release"
  exit 1
fi

source /etc/os-release
if [[ "${ID}" != "ubuntu" || "${VERSION_ID}" != "24.04" ]]; then
  echo "This installer only supports Ubuntu 24.04"
  echo "Current OS: ${PRETTY_NAME:-unknown}"
  exit 1
fi

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
BACKEND_SRC="${PROJECT_ROOT}/backend"
FRONTEND_SRC="${PROJECT_ROOT}/frontend"

# When running via bash <(curl ...), BASH_SOURCE[0] resolves to /dev/fd/N
# so PROJECT_ROOT becomes /dev and BACKEND_SRC becomes /dev/backend which
# does not exist.  Detect this and download the main branch tarball via curl
# (curl is guaranteed to be available - the user just used it to fetch us).
OPANEL_ENT_GITHUB="${OPANEL_ENT_GITHUB:-https://github.com/bnixvn/opanel-ent}"
if [[ ! -d "${BACKEND_SRC}" ]]; then
  OPANEL_ENT_CLONE_DIR="$(mktemp -d)"
  echo ""
  echo "==> Source not found locally - downloading main branch to ${OPANEL_ENT_CLONE_DIR}"
  curl -fsSL "${OPANEL_ENT_GITHUB}/archive/refs/heads/main.tar.gz" \
    | tar xz -C "${OPANEL_ENT_CLONE_DIR}" --strip-components=1
  PROJECT_ROOT="${OPANEL_ENT_CLONE_DIR}"
  SCRIPT_DIR="${PROJECT_ROOT}/installer"
  BACKEND_SRC="${PROJECT_ROOT}/backend"
  FRONTEND_SRC="${PROJECT_ROOT}/frontend"
  trap 'cd /; rm -rf "${OPANEL_ENT_CLONE_DIR}"' EXIT
fi

PANEL_URL="${PANEL_URL:-}"
PANEL_HOSTNAME="${PANEL_HOSTNAME:-}"
PANEL_DOMAIN=""
PANEL_PORT="${PANEL_PORT:-2222}"
SERVER_IP=""
ENABLE_SSL="${ENABLE_SSL:-auto}"
SSL_EMAIL="${SSL_EMAIL:-}"
NODE_MAJOR="${NODE_MAJOR:-22}"
PHP_DEFAULT="${PHP_DEFAULT:-8.4}"
PHP_VERSIONS="${PHP_VERSIONS:-8.3 8.4}"
APP_DIR="${APP_DIR:-/opt/opanel-ent}"
BACKUP_ROOT="${BACKUP_ROOT:-/var/backups/opanel-ent}"
ADMIN_PASSWORD=""

log() {
  echo ""
  echo "==> $1"
}

fail() {
  echo "ERROR: $1" >&2
  exit 1
}

# Running the installer a second time recreates the environment file and wipes
# opanel-ent.db, taking every panel user, website record and setting with it. The
# database is the one thing that cannot be rebuilt from the repo, so treat its
# presence as proof the box is already installed and refuse to continue.
if [[ -f "${APP_DIR}/backend/opanel-ent.db" ]]; then
  echo ""
  echo "ERROR: OPanel Enterprise is already installed on this server." >&2
  echo "" >&2
  echo "  Found: ${APP_DIR}/backend/opanel-ent.db" >&2
  echo "" >&2
  echo "Running the installer again would erase that database and every panel" >&2
  echo "user, website and setting recorded in it." >&2
  echo "" >&2
  echo "  To upgrade an existing install, run:  opanel-ent-update" >&2
  echo "" >&2
  echo "If you really do want a clean install, back the database up and remove" >&2
  echo "it first:" >&2
  echo "" >&2
  echo "  cp ${APP_DIR}/backend/opanel-ent.db /root/opanel-ent.db.bak" >&2
  echo "  rm ${APP_DIR}/backend/opanel-ent.db" >&2
  echo "" >&2
  exit 1
fi

host_has_global_ipv6() {
  # scope 00 in /proc/net/if_inet6 is a global address; loopback never counts.
  awk '$4 == "00" && $6 != "lo" { found = 1 } END { exit found ? 0 : 1 }' /proc/net/if_inet6 2>/dev/null
}

detect_server_ip() {
  hostname -I 2>/dev/null | awk '{print $1}' || true
}

find_sshd() {
  if command -v sshd >/dev/null 2>&1; then
    command -v sshd
    return 0
  fi
  for candidate in /usr/sbin/sshd /usr/local/sbin/sshd; do
    [[ -x "$candidate" ]] && { echo "$candidate"; return 0; }
  done
  return 1
}

validate_port() {
  [[ "$1" =~ ^[0-9]{1,5}$ ]] || fail "Invalid PANEL_PORT: $1"
  (( $1 >= 1 && $1 <= 65535 )) || fail "PANEL_PORT out of range: $1"
}

detect_ssh_ports() {
  local sshd_bin ssh_config ssh_config_files=()
  sshd_bin="$(find_sshd || true)"
  for ssh_config in /etc/ssh/sshd_config /etc/ssh/sshd_config.d/*.conf; do
    [[ -f "$ssh_config" ]] && ssh_config_files+=("$ssh_config")
  done
  {
    if [[ -n "$sshd_bin" ]]; then
      "$sshd_bin" -T 2>/dev/null | awk '$1 == "port" {print $2}'
    fi
    if [[ -n "${SSH_CONNECTION:-}" ]]; then
      awk '{print $4}' <<<"$SSH_CONNECTION"
    fi
    if (( ${#ssh_config_files[@]} > 0 )); then
      awk '
        tolower($1) == "port" && $2 ~ /^[0-9]+$/ { print $2 }
        tolower($1) == "listenaddress" {
          for (i = 2; i <= NF; i++) {
            value = $i
            gsub(/^\[/, "", value)
            gsub(/\]$/, "", value)
            if (value ~ /:[0-9]+$/) {
              sub(/^.*:/, "", value)
              print value
            }
          }
        }
      ' "${ssh_config_files[@]}" 2>/dev/null
    fi
  } | awk '$1 ~ /^[0-9]+$/ && $1 >= 1 && $1 <= 65535 {print $1}' | sort -nu
}

is_domain_name() {
  [[ "$1" =~ ^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)+$ ]]
}

need_dir() {
  [[ -d "$1" ]] || fail "Missing directory $1. Upload backend, frontend, and installer."
}

validate_sources() {
  need_dir "$BACKEND_SRC"
  need_dir "$FRONTEND_SRC"
  [[ -f "${BACKEND_SRC}/requirements.txt" ]] || fail "Missing backend/requirements.txt"
  [[ -f "${FRONTEND_SRC}/package.json" ]] || fail "Missing frontend/package.json"
}

ask_panel_url() {
  validate_port "$PANEL_PORT"
  if [[ -n "$PANEL_URL" ]]; then
    PANEL_URL="${PANEL_URL%/}"
    if [[ "$PANEL_URL" =~ ^https?:// ]]; then
      PANEL_HOSTNAME="$(echo "$PANEL_URL" | sed -E 's#^https?://([^/:]+).*#\1#')"
      parsed_port="$(echo "$PANEL_URL" | sed -nE 's#^https?://[^/:]+:([0-9]+).*#\1#p')"
    else
      PANEL_HOSTNAME="$(echo "$PANEL_URL" | sed -E 's#^([^/:]+).*#\1#')"
      parsed_port="$(echo "$PANEL_URL" | sed -nE 's#^[^/:]+:([0-9]+).*#\1#p')"
    fi
    if [[ -n "${parsed_port:-}" ]]; then
      PANEL_PORT="$parsed_port"
      validate_port "$PANEL_PORT"
    fi
  fi

  if [[ -z "$PANEL_HOSTNAME" ]]; then
    read -rp "Enter panel hostname (optional, blank = server IP): " PANEL_HOSTNAME
  fi
  PANEL_HOSTNAME="${PANEL_HOSTNAME#http://}"
  PANEL_HOSTNAME="${PANEL_HOSTNAME#https://}"
  PANEL_HOSTNAME="${PANEL_HOSTNAME%%/*}"
  if [[ "$PANEL_HOSTNAME" == *:* ]]; then
    parsed_port="${PANEL_HOSTNAME##*:}"
    PANEL_HOSTNAME="${PANEL_HOSTNAME%%:*}"
    [[ -n "$parsed_port" ]] && PANEL_PORT="$parsed_port"
    validate_port "$PANEL_PORT"
  fi
  if [[ -z "${PANEL_URL:-}" ]]; then
    read -rp "Enter panel port [${PANEL_PORT}]: " panel_port_answer
    if [[ -n "$panel_port_answer" ]]; then
      PANEL_PORT="$panel_port_answer"
      validate_port "$PANEL_PORT"
    fi
  fi

  if [[ -z "$PANEL_HOSTNAME" ]]; then
    SERVER_IP="$(detect_server_ip)"
    [[ -n "$SERVER_IP" ]] || fail "Cannot detect server IP. Set PANEL_HOSTNAME manually."
    PANEL_DOMAIN=""
    PANEL_URL="http://${SERVER_IP}:${PANEL_PORT}"
    ENABLE_SSL="no"
    return 0
  fi

  PANEL_DOMAIN="$PANEL_HOSTNAME"

  if [[ "$PANEL_DOMAIN" == "localhost" || "$PANEL_DOMAIN" == "127.0.0.1" || "$PANEL_DOMAIN" =~ ^[0-9.]+$ ]]; then
    ENABLE_SSL="no"
    PANEL_URL="http://${PANEL_DOMAIN}:${PANEL_PORT}"
    PANEL_DOMAIN=""
  elif [[ "$ENABLE_SSL" == "auto" ]]; then
    if ! is_domain_name "$PANEL_DOMAIN"; then
      fail "Invalid panel domain: $PANEL_DOMAIN"
    fi
    read -rp "Enable Let's Encrypt SSL for ${PANEL_DOMAIN}:${PANEL_PORT}? [Y/n]: " ssl_answer
    ssl_answer="${ssl_answer:-Y}"
    if [[ "$ssl_answer" =~ ^[Nn]$ ]]; then
      ENABLE_SSL="no"
      PANEL_URL="http://${PANEL_DOMAIN}:${PANEL_PORT}"
    else
      ENABLE_SSL="yes"
      PANEL_URL="https://${PANEL_DOMAIN}:${PANEL_PORT}"
    fi
  elif [[ "$ENABLE_SSL" == "yes" ]]; then
    is_domain_name "$PANEL_DOMAIN" || fail "Invalid panel domain: $PANEL_DOMAIN"
    PANEL_URL="https://${PANEL_DOMAIN}:${PANEL_PORT}"
  else
    ENABLE_SSL="no"
    PANEL_URL="http://${PANEL_DOMAIN}:${PANEL_PORT}"
  fi

  if [[ "$ENABLE_SSL" == "yes" && -z "$SSL_EMAIL" ]]; then
    read -rp "Enter email for Let's Encrypt registration: " SSL_EMAIL
    [[ -n "$SSL_EMAIL" ]] || fail "Email is required to issue SSL"
  fi
}

install_base_packages() {
  export DEBIAN_FRONTEND=noninteractive
  # Pre-seed debconf so iptables-persistent never prompts
  echo iptables-persistent iptables-persistent/autosave_v4 boolean true | debconf-set-selections
  echo iptables-persistent iptables-persistent/autosave_v6 boolean true | debconf-set-selections
  apt-get update --allow-releaseinfo-change
  apt-get install -y software-properties-common ca-certificates curl gnupg git composer mariadb-server mariadb-client redis-server openssh-server python3 python3-pip python3-venv certbot python3-certbot-dns-cloudflare tar zip unzip openssl iptables iptables-persistent ipset acl phpmyadmin zstd
  systemctl enable --now mariadb redis-server
  systemctl enable --now ssh 2>/dev/null || systemctl enable --now sshd 2>/dev/null || true
  remove_ufw_legacy
}

remove_ufw_legacy() {
  systemctl disable --now ufw >/dev/null 2>&1 || true
  command -v ufw >/dev/null 2>&1 && timeout 15 ufw --force disable >/dev/null 2>&1 || true
  DEBIAN_FRONTEND=noninteractive apt-get purge -y ufw >/dev/null 2>&1 || true
  rm -rf /etc/ufw /var/lib/ufw 2>/dev/null || true
}

install_nodejs() {
  curl -fsSL --connect-timeout 10 --max-time 180 "https://deb.nodesource.com/setup_${NODE_MAJOR}.x" | bash -
  apt-get install -y nodejs
  node - <<'NODE'
const major = Number(process.versions.node.split('.')[0]);
if (major < 20) {
  throw new Error(`Node.js 20+ is required, current: ${process.version}`);
}
console.log(`Using Node.js ${process.version}`);
NODE
  npm --version
}

install_openlitespeed() {
  export DEBIAN_FRONTEND=noninteractive
  # Add LiteSpeed repository (always re-run to ensure fresh repo config)
  curl -fsSL --connect-timeout 15 --max-time 120 https://repo.litespeed.sh | bash
  apt-get update --allow-releaseinfo-change
  # Install OLS + LSPHP versions + modsecurity
  local lsphp_packages=""
  for version in $PHP_VERSIONS; do
    local ver_no_dot="${version//./}"
    # NOTE: gd, xml, mbstring, zip, bcmath are bundled in the base lsphp package
    lsphp_packages+=" lsphp${ver_no_dot} lsphp${ver_no_dot}-common lsphp${ver_no_dot}-mysql lsphp${ver_no_dot}-sqlite3 lsphp${ver_no_dot}-curl lsphp${ver_no_dot}-opcache lsphp${ver_no_dot}-intl lsphp${ver_no_dot}-redis lsphp${ver_no_dot}-imagick"
  done
  apt-get install -y openlitespeed ${lsphp_packages} ols-modsecurity || \
    apt-get install -y openlitespeed ${lsphp_packages}
  systemctl disable --now apache2 2>/dev/null || true
  install -d -m 0755 /etc/systemd/system/lshttpd.service.d
  cat >/etc/systemd/system/lshttpd.service.d/10-opanel-ent.conf <<'UNIT'
[Service]
PIDFile=/run/openlitespeed.pid
KillMode=mixed
UNIT
  systemctl daemon-reload
  systemctl enable --now lsws || true
  # Install ionCube for each LSPHP version
  for version in $PHP_VERSIONS; do
    local ver_no_dot="${version//./}"
    install_ioncube_loader "$version" "/usr/local/lsws/lsphp${ver_no_dot}"
  done
}

install_ioncube_loader() {
  local version="$1" ioncube_target_dir="${2:-}" arch url tmp archive loader target_dir target loader_ini_dir
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
  tmp="$(mktemp -d)" || fail "Cannot create ionCube temporary directory"
  archive="${tmp}/ioncube_loaders.tar.gz"
  if ! curl -fsSL --connect-timeout 10 --max-time 300 "$url" -o "$archive"; then
    rm -rf -- "$tmp"
    fail "Failed to download ionCube Loader"
  fi
  if ! tar -xzf "$archive" -C "$tmp"; then
    rm -rf -- "$tmp"
    fail "Failed to unpack ionCube Loader"
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

  for loader_ini_dir in /etc/php/"$version"/cli/conf.d /etc/php/"$version"/fpm/conf.d; do
    [[ -d "$loader_ini_dir" ]] || continue
    printf 'zend_extension=%s\n' "$target" >"${loader_ini_dir}/00-ioncube.ini"
    chown root:root "${loader_ini_dir}/00-ioncube.ini"
    chmod 0644 "${loader_ini_dir}/00-ioncube.ini"
  done
  # Also configure for LSPHP if target dir is provided
  if [[ -n "$ioncube_target_dir" && -d "$ioncube_target_dir" ]]; then
    local lsphp_ini_dir="${ioncube_target_dir}/etc/php/${version}/mods-available"
    if [[ -d "$lsphp_ini_dir" ]]; then
      printf 'zend_extension=%s\n' "$target" >"${lsphp_ini_dir}/00-ioncube.ini"
      chown root:root "${lsphp_ini_dir}/00-ioncube.ini"
      chmod 0644 "${lsphp_ini_dir}/00-ioncube.ini"
    fi
  fi

  if command -v "php${version}" >/dev/null 2>&1; then
    if ! "php${version}" -v 2>&1 | grep -qi 'ionCube'; then
      rm -f /etc/php/"$version"/cli/conf.d/00-ioncube.ini /etc/php/"$version"/fpm/conf.d/00-ioncube.ini
      fail "ionCube Loader failed to load for PHP ${version}"
    fi
  fi
  echo "ionCube Loader enabled for PHP ${version}"
}

install_php() {
  # LSPHP packages are installed by install_openlitespeed().
  # This function configures PHP ini overrides for each LSPHP version.
  if [[ ! " ${PHP_VERSIONS} " =~ " ${PHP_DEFAULT} " ]]; then
    fail "PHP_DEFAULT=${PHP_DEFAULT} must be included in PHP_VERSIONS='${PHP_VERSIONS}'"
  fi

  for version in $PHP_VERSIONS; do
    local ver_no_dot="${version//./}"
    local lsphp_dir="/usr/local/lsws/lsphp${ver_no_dot}"
    local ini_dir="${lsphp_dir}/etc/php/${version}/mods-available"

    if [[ ! -d "$lsphp_dir" ]]; then
      echo "WARNING: LSPHP ${version} directory not found at ${lsphp_dir}, skipping ini config"
      continue
    fi

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
  done

  # Set default PHP CLI symlink
  local default_ver_no_dot="${PHP_DEFAULT//./}"
  if [[ -x "/usr/local/lsws/lsphp${default_ver_no_dot}/bin/php" ]]; then
    ln -sfn "/usr/local/lsws/lsphp${default_ver_no_dot}/bin/php" /usr/local/bin/php
  fi
  # Restart OLS to pick up PHP config changes
  systemctl restart lshttpd.service 2>/dev/null || /usr/local/lsws/bin/lswsctrl restart 2>/dev/null || true
}

# configure_fastcgi_cache removed Ã¢â‚¬â€ OLS handles caching internally via LSCache module.

write_modsec_base_conf() {
  local modsec_dir="/usr/local/lsws/conf/opanel-ent/waf"
  install -d -o root -g root -m 0755 "$modsec_dir" "${modsec_dir}/sites"
  {
    [[ -f /etc/modsecurity/modsecurity.conf ]] && echo "Include /etc/modsecurity/modsecurity.conf"
    echo "SecRuleEngine On"
    # Off here disables every phase:2 rule, not just body parsing, which left a
    # fresh install with its path-traversal, author-enumeration and wp2shell
    # rules silently dead. Must stay in step with opanel-ent-helper.sh.
    echo "SecRequestBodyAccess On"
    echo "SecRequestBodyLimit 134217728"
    echo "SecRequestBodyNoFilesLimit 1048576"
    echo "SecRequestBodyLimitAction ProcessPartial"
  } >"${modsec_dir}/opanel-ent-base.conf"
}

write_modsec_main_conf() {
  write_waf_default_rules
  write_modsec_base_conf
  local modsec_dir="/usr/local/lsws/conf/opanel-ent/waf"
  touch "${modsec_dir}/opanel-ent-custom.conf"
  {
    echo "Include ${modsec_dir}/opanel-ent-base.conf"
    echo "Include ${modsec_dir}/opanel-ent-default.conf"
    echo "Include ${modsec_dir}/opanel-ent-custom.conf"
  } >"${modsec_dir}/opanel-ent-main.conf"
}

ensure_ols_modsecurity_enabled() {
  [[ -f /usr/local/lsws/modules/mod_security.so ]] || return 1
  python3 - /usr/local/lsws/conf/httpd_config.conf <<'PY'
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

write_waf_default_rules() {
  local modsec_dir="/usr/local/lsws/conf/opanel-ent/waf"
  install -d -o root -g root -m 0755 "$modsec_dir"
  cat >"${modsec_dir}/opanel-ent-default.conf" <<'RULES'
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

install_waf_engine() {
  export DEBIAN_FRONTEND=noninteractive
  local modsec_dir="/usr/local/lsws/conf/opanel-ent/waf"
  # ols-modsecurity is installed by install_openlitespeed()
  if ! dpkg -s ols-modsecurity >/dev/null 2>&1; then
    apt-get update --allow-releaseinfo-change
    apt-get install -y ols-modsecurity modsecurity-crs libmodsecurity3 2>/dev/null || \
      apt-get install -y ols-modsecurity libmodsecurity3 2>/dev/null || \
      echo "WARNING: Could not install ols-modsecurity; WAF will not be available"
  fi
  install -d -o root -g root -m 0755 "$modsec_dir" "${modsec_dir}/sites"
  write_waf_default_rules
  touch "${modsec_dir}/opanel-ent-custom.conf"
  if [[ -f /etc/modsecurity/modsecurity.conf-recommended && ! -f /etc/modsecurity/modsecurity.conf ]]; then
    cp /etc/modsecurity/modsecurity.conf-recommended /etc/modsecurity/modsecurity.conf
  fi
  if [[ -f /etc/modsecurity/modsecurity.conf ]]; then
    sed -i -E 's/^SecRuleEngine .*/SecRuleEngine On/' /etc/modsecurity/modsecurity.conf
  fi
  write_modsec_main_conf
  ensure_ols_modsecurity_enabled
  systemctl restart lshttpd.service 2>/dev/null || /usr/local/lsws/bin/lswsctrl restart 2>/dev/null || true
}

install_wp_cli() {
  if ! command -v wp >/dev/null 2>&1; then
    curl -fsSL --connect-timeout 10 --max-time 180 -o /usr/local/bin/wp https://raw.githubusercontent.com/wp-cli/builds/gh-pages/phar/wp-cli.phar
    chmod +x /usr/local/bin/wp
  fi
}

copy_sources() {
  mkdir -p "$APP_DIR" "$BACKUP_ROOT"
  rm -rf "${APP_DIR}/backend" "${APP_DIR}/frontend"
  cp -r "$BACKEND_SRC" "${APP_DIR}/backend"
  cp -r "$FRONTEND_SRC" "${APP_DIR}/frontend"
}

build_frontend() {
  cd "${APP_DIR}/frontend"
  rm -rf node_modules package-lock.json dist .vite
  npm install
  VITE_API_URL=/api npm run build
  if [[ ! -f dist/index.html ]]; then
    fail "Frontend build failed: ${APP_DIR}/frontend/dist/index.html is missing"
  fi
  # OLS (lsadm) needs to read the bundle. The frontend is public anyway.
  chmod o+rX "${APP_DIR}" "${APP_DIR}/frontend" 2>/dev/null || true
  chmod -R o+rX "${APP_DIR}/frontend/dist"
  echo "Frontend built: $(grep -oE 'index-[a-zA-Z0-9_-]+\.js' dist/index.html | head -n1 || echo 'unknown')"
}

setup_panel_user() {
  if ! getent group opanel-ent-sites >/dev/null; then
    groupadd --system opanel-ent-sites
  fi
  if ! getent group opanel-ent-sftp >/dev/null; then
    groupadd --system opanel-ent-sftp
  fi
  if ! id -u opanel-ent >/dev/null 2>&1; then
    useradd --system --home-dir "$APP_DIR" --shell /usr/sbin/nologin --user-group opanel-ent
  fi
  usermod -aG www-data opanel-ent || true
  usermod -aG opanel-ent-sites opanel-ent || true
  usermod -aG opanel-ent-sites www-data || true

  # Allow opanel-ent to write into OLS vhost directory.
  install -d -o root -g opanel-ent -m 2775 /usr/local/lsws/conf/opanel-ent/vhosts
  install -d -o root -g opanel-ent -m 2775 /usr/local/lsws/conf/opanel-ent/custom
  install -d -o www-data -g opanel-ent-sites -m 2775 /var/log/openlitespeed
  # setgid so new files inherit the opanel-ent group; allows future writes.
  chmod g+s /usr/local/lsws/conf/opanel-ent/vhosts || true
  chmod g+s /usr/local/lsws/conf/opanel-ent/custom 2>/dev/null || true
  chmod g+s /var/log/openlitespeed 2>/dev/null || true

  # Make the panel data dirs writable by opanel-ent.
  install -d -o opanel-ent -g opanel-ent -m 0750 "$APP_DIR"
  install -d -o opanel-ent -g opanel-ent -m 0750 "$BACKUP_ROOT"
  install -d -o opanel-ent -g opanel-ent -m 0750 /home/admin/opanel-ent-backups/da

  # MariaDB: create an admin user that opanel-ent can use without password
  # (auth via a defaults-file in ~opanel-ent/.my.cnf, mode 0600).
  local mariadb_password
  mariadb_password="$(openssl rand -base64 32 | tr -d '/+=' | cut -c1-32)"
  mariadb -e "
    CREATE USER IF NOT EXISTS 'opanel-ent'@'localhost' IDENTIFIED BY '${mariadb_password}';
    ALTER USER 'opanel-ent'@'localhost' IDENTIFIED BY '${mariadb_password}';
    GRANT ALL PRIVILEGES ON *.* TO 'opanel-ent'@'localhost' WITH GRANT OPTION;
    FLUSH PRIVILEGES;
  " 2>/dev/null || mysql -e "
    CREATE USER IF NOT EXISTS 'opanel-ent'@'localhost' IDENTIFIED BY '${mariadb_password}';
    ALTER USER 'opanel-ent'@'localhost' IDENTIFIED BY '${mariadb_password}';
    GRANT ALL PRIVILEGES ON *.* TO 'opanel-ent'@'localhost' WITH GRANT OPTION;
    FLUSH PRIVILEGES;
  "

  cat >"${APP_DIR}/.my.cnf" <<MYCNF
[client]
user=opanel-ent
password="${mariadb_password}"
host=localhost

[mysqldump]
user=opanel-ent
password="${mariadb_password}"
host=localhost
MYCNF
  chown opanel-ent:opanel-ent "${APP_DIR}/.my.cnf"
  chmod 0600 "${APP_DIR}/.my.cnf"
}

setup_sftp_access() {
  local sshd_config="/etc/ssh/sshd_config" backup
  getent group opanel-ent-sftp >/dev/null || groupadd --system opanel-ent-sftp
  install -d -o root -g root -m 0755 /run/sshd
  rm -f /etc/ssh/sshd_config.d/99-opanel-ent-sftp.conf 2>/dev/null || true
  touch "$sshd_config"
  backup="${sshd_config}.opanel-ent.bak"
  cp "$sshd_config" "$backup"
  sed -i '/^# BEGIN opanel-ent SFTP USERS$/,/^# END opanel-ent SFTP USERS$/d' "$sshd_config"
  cat >>"$sshd_config" <<'SSHD'
# BEGIN opanel-ent SFTP USERS
# Allow opanel-ent Linux users to log in with SFTP using their panel password.
# SSH shells are intentionally disabled; /home/%u is a root-owned chroot.
Match Group opanel-ent-sftp
    PasswordAuthentication yes
    ChrootDirectory /home/%u
    ForceCommand internal-sftp -d /
    PermitTTY no
    X11Forwarding no
    AllowTcpForwarding no
    PermitTunnel no
# END opanel-ent SFTP USERS
SSHD
  if ! sshd -t; then
    cp "$backup" "$sshd_config"
    fail "Invalid SSHD configuration for opanel-ent SFTP users"
  fi
  systemctl reload ssh 2>/dev/null || systemctl reload sshd 2>/dev/null || true
}

install_privileged_helper() {
  install -m 0750 -o root -g opanel-ent "${SCRIPT_DIR}/files/opanel-ent-helper.sh" /usr/local/sbin/opanel-ent-helper
  sed -i "s#^APP_DIR=\"/opt/opanel-ent\"#APP_DIR=\"${APP_DIR}\"#" /usr/local/sbin/opanel-ent-helper
  install -m 0755 -o root -g root "${SCRIPT_DIR}/update.sh" /usr/local/sbin/opanel-ent-update
  install -m 0440 -o root -g root "${SCRIPT_DIR}/files/opanel-ent-sudoers" /etc/sudoers.d/opanel-ent
  sed -i 's/\r$//' /etc/sudoers.d/opanel-ent
  visudo -c -f /etc/sudoers.d/opanel-ent >/dev/null
  if [[ -f "${PROJECT_ROOT}/change_IP.sh" ]]; then
    install -m 0755 -o root -g root "${PROJECT_ROOT}/change_IP.sh" /usr/local/sbin/opanel-ent-change-ip
  fi
}

install_panel_cli() {
  install -m 0755 -o root -g root "${SCRIPT_DIR}/files/opanel-ent-ctl" /usr/local/sbin/opanel-ent
  ln -sfn /usr/local/sbin/opanel-ent /usr/local/sbin/opanel-ent-ctl
  sed -i "s#APP_DIR=\"\${APP_DIR:-/opt/opanel-ent}\"#APP_DIR=\"\${APP_DIR:-${APP_DIR}}\"#" /usr/local/sbin/opanel-ent /usr/local/sbin/opanel-ent-ctl 2>/dev/null || true
}

validate_privileged_helper() {
  sudo -u opanel-ent env HOME="$APP_DIR" sudo -n /usr/local/sbin/opanel-ent-helper wp --info >/dev/null
  # Seed /etc/opanel-ent/certs before the panel first starts: a self-signed default
  # so HTTPS works out of the box, plus a directory per Let's Encrypt domain so
  # any site with SSL can reach the panel under its own certificate.
  sudo -u opanel-ent env HOME="$APP_DIR" sudo -n /usr/local/sbin/opanel-ent-helper panel-cert-sync >/dev/null || \
    echo "  (warning: could not seed the panel certificate store)"
}

setup_backend() {
  cd "${APP_DIR}/backend"
  python3 -m venv .venv
  source .venv/bin/activate
  pip install --upgrade pip
  pip install -r requirements.txt

  ADMIN_PASSWORD="${opanel_ent_ADMIN_PASSWORD:-$(openssl rand -base64 24 | tr -d '\n')}"

  # A box that ships with IPv6 serves it from the first boot; the panel
  # settings page can turn it off later.
  panel_bind_host="0.0.0.0"
  host_has_global_ipv6 && panel_bind_host="::"

  cat > .env <<ENV
APP_ENV=production
SECRET_KEY=$(openssl rand -hex 32)
COMMAND_DRY_RUN=false
DATABASE_URL=sqlite:///${APP_DIR}/backend/opanel-ent.db
REDIS_URL=redis://localhost:6379/0
RATE_LIMIT_BACKEND=redis
ALLOWED_ORIGINS=${PANEL_URL}
BACKUP_ROOT=${BACKUP_ROOT}
SSL_EMAIL=${SSL_EMAIL}
PANEL_URL=${PANEL_URL}
PANEL_DOMAIN=${PANEL_DOMAIN}
PANEL_PORT=${PANEL_PORT}
PANEL_BIND_HOST=${panel_bind_host}
PANEL_SSL_CERT=
PANEL_SSL_KEY=
FRONTEND_DIST=${APP_DIR}/frontend/dist
PMA_SIGNON_SECRET=$(openssl rand -hex 32)
ENV

  # Lock down the env file: contains SECRET_KEY and ALLOWED_ORIGINS.
  chmod 0640 "${APP_DIR}/backend/.env"

  # Make panel files writable before seed creates the SQLite DB and admin Linux user.
  chown -R opanel-ent:opanel-ent "${APP_DIR}/backend"
  chown -R opanel-ent:opanel-ent "${APP_DIR}/frontend" 2>/dev/null || true

  sudo -u opanel-ent env HOME="$APP_DIR" opanel_ent_USE_HELPER=true opanel_ent_ADMIN_PASSWORD="$ADMIN_PASSWORD" \
    "${APP_DIR}/backend/.venv/bin/python" -m app.seed
  deactivate || true
}

wait_for_backend() {
  # The panel serves HTTPS with a self-signed certificate out of the box, so a
  # plain-HTTP probe fails on a fresh IP-only install even though the API is up.
  for _ in {1..30}; do
    if curl -kfsS --connect-timeout 2 --max-time 5 "https://127.0.0.1:${PANEL_PORT}/api/health" >/dev/null 2>&1 \
       || curl -fsS --connect-timeout 2 --max-time 5 "http://127.0.0.1:${PANEL_PORT}/api/health" >/dev/null 2>&1; then
      return 0
    fi
    sleep 1
  done
  journalctl -u opanel-ent-api -n 80 --no-pager || true
  fail "opanel-ent-api did not respond at http://127.0.0.1:${PANEL_PORT}/api/health"
}

setup_systemd() {
  cat >/usr/local/sbin/opanel-ent-api-start <<STARTER
#!/usr/bin/env bash
# Trusted forwarders: only the local reverse proxy (127.0.0.1) is allowed to set
# X-Forwarded-For / X-Forwarded-Proto. Anything else (direct hits on
# the configured panel port) cannot spoof the audit log IP or the login rate-limit key.
set -euo pipefail
cd ${APP_DIR}/backend
# app.server serves HTTPS by default and selects a certificate per SNI
# hostname from /etc/opanel-ent/certs, so any site with SSL can reach the panel on
# this port. It degrades to the self-signed default, then to plain HTTP, rather
# than refusing to start -- the panel is how the box gets repaired.
exec ${APP_DIR}/backend/.venv/bin/python -m app.server
STARTER
  chmod 0755 /usr/local/sbin/opanel-ent-api-start

  cat >/etc/systemd/system/opanel-ent-api.service <<SERVICE
[Unit]
Description=OPanel Enterprise API
After=network.target mariadb.service

[Service]
Type=exec
User=opanel-ent
Group=opanel-ent
SupplementaryGroups=www-data opanel-ent-sites
WorkingDirectory=${APP_DIR}/backend
EnvironmentFile=${APP_DIR}/backend/.env
Environment=HOME=${APP_DIR}
Environment=opanel_ent_USE_HELPER=true
ExecStart=/usr/local/sbin/opanel-ent-api-start
Restart=always
RestartSec=3

# Hardening. These settings must not block the sudo helper; privileged work is
# restricted by /usr/local/sbin/opanel-ent-helper and /etc/sudoers.d/opanel-ent.
NoNewPrivileges=false
ProtectSystem=false
ProtectHome=false
ReadWritePaths=${APP_DIR} /home ${BACKUP_ROOT} /usr/local/lsws/conf/opanel-ent /tmp /var/lib/opanel-ent
PrivateTmp=true
PrivateDevices=true
ProtectKernelTunables=true
ProtectKernelModules=true
ProtectKernelLogs=true
ProtectControlGroups=true
ProtectClock=true
ProtectHostname=true
ProtectProc=invisible
RestrictNamespaces=true
RestrictRealtime=true
RestrictSUIDSGID=false
LockPersonality=true
MemoryDenyWriteExecute=false
SystemCallArchitectures=native
RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6 AF_NETLINK
CapabilityBoundingSet=~

[Install]
WantedBy=multi-user.target
SERVICE

  install -d -o opanel-ent -g opanel-ent -m 0750 /var/lib/opanel-ent
  cat >/etc/systemd/system/opanel-ent-backup-scheduler.service <<SERVICE
[Unit]
Description=opanel-ent scheduled backup runner
After=network.target mariadb.service

[Service]
Type=oneshot
User=opanel-ent
Group=opanel-ent
SupplementaryGroups=www-data opanel-ent-sites
WorkingDirectory=${APP_DIR}/backend
EnvironmentFile=${APP_DIR}/backend/.env
Environment=HOME=${APP_DIR}
Environment=opanel_ent_USE_HELPER=true
ExecStart=${APP_DIR}/backend/.venv/bin/python -m app.services.backup_scheduler
NoNewPrivileges=false
ProtectSystem=false
ProtectHome=false
ReadWritePaths=${APP_DIR} /home ${BACKUP_ROOT} /usr/local/lsws/conf/opanel-ent /tmp /var/lib/opanel-ent
PrivateTmp=true

[Install]
WantedBy=multi-user.target
SERVICE

  cat >/etc/systemd/system/opanel-ent-backup-scheduler.timer <<'SERVICE'
[Unit]
Description=Run opanel-ent scheduled backups every minute

[Timer]
OnBootSec=90s
OnUnitActiveSec=60s
AccuracySec=15s
Persistent=true

[Install]
WantedBy=timers.target
SERVICE


  cat >/etc/systemd/system/opanel-ent-malware-scheduler.service <<SERVICE
[Unit]
Description=opanel-ent scheduled malware scan runner
After=network.target clamav-daemon.service

[Service]
Type=oneshot
User=opanel-ent
Group=opanel-ent
SupplementaryGroups=www-data opanel-ent-sites
WorkingDirectory=${APP_DIR}/backend
EnvironmentFile=${APP_DIR}/backend/.env
Environment=HOME=${APP_DIR}
Environment=opanel_ent_USE_HELPER=true
ExecStart=${APP_DIR}/backend/.venv/bin/python -m app.services.malware_scheduler
# A full-filesystem scan is not a one-minute job; let it run to the end instead
# of being killed while the next timer tick waits.
TimeoutStartSec=infinity
NoNewPrivileges=false
ProtectSystem=false
ProtectHome=false
ReadWritePaths=${APP_DIR} /var/lib/opanel-ent /tmp
PrivateTmp=false

[Install]
WantedBy=multi-user.target
SERVICE

  cat >/etc/systemd/system/opanel-ent-malware-scheduler.timer <<'SERVICE'
[Unit]
Description=Check for a due opanel-ent malware scan every minute

[Timer]
OnBootSec=120s
OnUnitActiveSec=60s
AccuracySec=15s
Persistent=true

[Install]
WantedBy=timers.target
SERVICE
  systemctl daemon-reload
  systemctl enable --now opanel-ent-api
  systemctl enable --now opanel-ent-backup-scheduler.timer
  systemctl enable --now opanel-ent-malware-scheduler.timer
  if id -u opanel-ent >/dev/null 2>&1; then
    sudo -u opanel-ent env HOME="$APP_DIR" sudo -n /usr/local/sbin/opanel-ent-helper certbot-auto-renew-install >/dev/null 2>&1 || true
    sudo -u opanel-ent env HOME="$APP_DIR" sudo -n /usr/local/sbin/opanel-ent-helper blocklist-timer-install >/dev/null 2>&1 || true
  fi
  wait_for_backend
}

write_tools_vhost_config() {
  local api_scheme="http" tools_scheme="http" pma_secure="false"
  if [[ "${PANEL_URL:-}" == https://* && -n "${PANEL_SSL_CERT:-}" && -n "${PANEL_SSL_KEY:-}" && -f "${PANEL_SSL_CERT}" && -f "${PANEL_SSL_KEY}" ]]; then
    api_scheme="https"
    tools_scheme="https"
    pma_secure="true"
  fi

  local default_ver_no_dot="${PHP_DEFAULT//./}"
  local lsphp_sock="/tmp/lshttpd/lsphp${default_ver_no_dot}.sock"
  local vhosts_dir="/usr/local/lsws/conf/opanel-ent/vhosts"
  install -d -o root -g opanel-ent -m 2775 "$vhosts_dir"

  cat >"${vhosts_dir}/00-opanel-ent-tools.conf" <<OLS
# OPanel Enterprise tools vhost (phpMyAdmin)
docRoot                   /usr/share/phpmyadmin/
vhDomain                  *
vhAliases                 *

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
  rules                   rewriteRule ^/phpmyadmin/(.*)\$ /\$1 [L]
}

vhssl  {
  keyFile                 ${PANEL_SSL_KEY:-/dev/null}
  certFile                ${PANEL_SSL_CERT:-/dev/null}
}
OLS
  chown root:opanel-ent "${vhosts_dir}/00-opanel-ent-tools.conf"
  chmod 0664 "${vhosts_dir}/00-opanel-ent-tools.conf"

  local host
  host="${PANEL_DOMAIN:-$SERVER_IP}"
  [[ -n "$host" ]] || host="$(detect_server_ip)"
  sed -i -E "/api\/databases\/phpmyadmin-sso/s#'[^']+/api/databases/phpmyadmin-sso/'#'${api_scheme}://127.0.0.1:${PANEL_PORT}/api/databases/phpmyadmin-sso/'#" /usr/share/phpmyadmin/opanel-ent-signon.php 2>/dev/null || true
  sed -i -E "s#('secure' => )(true|false)#\1${pma_secure}#" /etc/phpmyadmin/conf.d/opanel-ent-signon.php /usr/share/phpmyadmin/opanel-ent-signon.php 2>/dev/null || true
  sed -i -E "/PmaAbsoluteUri/s#'https?://[^']+/phpmyadmin/'#'${tools_scheme}://${host}/phpmyadmin/'#" /etc/phpmyadmin/conf.d/opanel-ent-signon.php 2>/dev/null || true
  if id -u opanel-ent >/dev/null 2>&1 && [[ -x /usr/local/sbin/opanel-ent-helper ]]; then
    sudo -u opanel-ent env HOME="$APP_DIR" sudo -n /usr/local/sbin/opanel-ent-helper ols-sync-main >/dev/null 2>&1 || true
  fi
  fix_phpmyadmin_permissions
}


fix_phpmyadmin_permissions() {
  local file
  for file in \
    /etc/phpmyadmin/conf.d/opanel-ent-signon.php \
    /etc/phpmyadmin/config.inc.php \
    /usr/share/phpmyadmin/opanel-ent-signon.php; do
    [[ -e "$file" ]] || continue
    chgrp opanel-ent-sites "$file" 2>/dev/null || true
    chmod 0644 "$file" 2>/dev/null || true
  done
  for file in \
    /etc/phpmyadmin/config-db.php \
    /var/lib/phpmyadmin/blowfish_secret.inc.php; do
    [[ -e "$file" ]] || continue
    chgrp opanel-ent-sites "$file" 2>/dev/null || true
    chmod 0640 "$file" 2>/dev/null || true
  done
}


setup_phpmyadmin_sso() {
  # phpMyAdmin must be installed first
  if [[ ! -d /usr/share/phpmyadmin ]]; then
    log "phpMyAdmin not found Ã¢â‚¬â€ skipping SSO setup"
    return 0
  fi

  local blowfish_secret
  blowfish_secret="$(openssl rand -hex 32)"
  local pma_host pma_scheme pma_secure
  pma_host="${PANEL_DOMAIN:-$SERVER_IP}"
  [[ -n "$pma_host" ]] || pma_host="$(detect_server_ip)"
  pma_scheme="http"
  pma_secure="false"
  if [[ "$ENABLE_SSL" == "yes" ]]; then
    pma_scheme="https"
    pma_secure="true"
  fi

  mkdir -p /etc/phpmyadmin/conf.d
  cat >/etc/phpmyadmin/conf.d/opanel-ent-signon.php <<PHP
<?php
\$cfg['blowfish_secret'] = '${blowfish_secret}';
\$i = 1;
\$cfg['Servers'][\$i]['auth_type'] = 'signon';
\$cfg['Servers'][\$i]['SignonSession'] = 'opanelPmaSignon';
\$cfg['Servers'][\$i]['SignonCookieParams'] = [
    'lifetime' => 0,
    'path' => '/',
    'domain' => '',
    'secure' => ${pma_secure},
    'httponly' => true,
    'samesite' => 'Lax',
];
\$cfg['Servers'][\$i]['SignonURL'] = '/phpmyadmin/opanel-ent-signon.php';
\$cfg['Servers'][\$i]['host'] = 'localhost';
\$cfg['Servers'][\$i]['AllowNoPassword'] = false;
\$cfg['Servers'][\$i]['only_db'] = '';
\$cfg['SessionSavePath'] = '/var/lib/php/sessions';
\$cfg['PmaAbsoluteUri'] = '${pma_scheme}://${pma_host}/phpmyadmin/';
PHP

  cat >/usr/share/phpmyadmin/opanel-ent-signon.php <<'PHP'
<?php
declare(strict_types=1);

session_save_path('/var/lib/php/sessions');
ini_set('session.use_cookies', 'true');
session_set_cookie_params([
    'lifetime' => 0,
    'path' => '/',
    'domain' => '',
    'secure' => __opanel_PMA_COOKIE_SECURE__,
    'httponly' => true,
    'samesite' => 'Lax',
]);
session_name('opanelPmaSignon');
if (!session_start()) {
    http_response_code(500);
    exit('Cannot start signon session');
}

$token = $_GET['opanel_ent_sso'] ?? '';
if (!preg_match('/^[A-Za-z0-9_-]{20,}$/', $token)) {
    http_response_code(403);
    exit('Invalid token');
}

$apiUrl = '__opanel_API_BASE__' . rawurlencode($token);
$ch = curl_init($apiUrl);
curl_setopt_array($ch, [
    CURLOPT_RETURNTRANSFER => true,
    CURLOPT_TIMEOUT => 5,
    CURLOPT_SSL_VERIFYPEER => false,
    CURLOPT_SSL_VERIFYHOST => false,
    CURLOPT_HTTPHEADER => ['Accept: application/json', 'X-OPanel Enterprise-Signon-Secret: __opanel_PMA_SIGNON_SECRET__'],
]);
$response = curl_exec($ch);
$status = curl_getinfo($ch, CURLINFO_HTTP_CODE);
curl_close($ch);

if ($status !== 200 || !$response) {
    http_response_code(403);
    exit('Expired token');
}

$data = json_decode($response, true);
if (!is_array($data) || empty($data['db_user']) || empty($data['db_password'])) {
    http_response_code(403);
    exit('Invalid signon data');
}

session_regenerate_id(true);
$_SESSION = [];
$_SESSION['PMA_single_signon_user'] = $data['db_user'];
$_SESSION['PMA_single_signon_password'] = $data['db_password'];
$_SESSION['PMA_single_signon_host'] = 'localhost';
$_SESSION['PMA_single_signon_port'] = '';
$_SESSION['PMA_single_signon_cfgupdate'] = [
    'only_db' => $data['db_name'] ?? '',
];
$_SESSION['PMA_single_signon_HMAC_secret'] = bin2hex(random_bytes(16));
session_write_close();

header('Cache-Control: no-store, no-cache, must-revalidate, max-age=0');
header('Pragma: no-cache');
header('Location: /phpmyadmin/index.php?server=1');
exit;
PHP

  local api_scheme="http"
  if [[ "$ENABLE_SSL" == "yes" ]]; then
    api_scheme="https"
  fi
  sed -i "s#__opanel_API_BASE__#${api_scheme}://127.0.0.1:${PANEL_PORT}/api/databases/phpmyadmin-sso/#" /usr/share/phpmyadmin/opanel-ent-signon.php
  sed -i "s#__opanel_PMA_COOKIE_SECURE__#${pma_secure}#" /usr/share/phpmyadmin/opanel-ent-signon.php
  local pma_signon_secret="$(grep -oP "^PMA_SIGNON_SECRET=\K.*" "${APP_DIR}/backend/.env" 2>/dev/null || true)"
  sed -i "s#__opanel_PMA_SIGNON_SECRET__#${pma_signon_secret}#" /usr/share/phpmyadmin/opanel-ent-signon.php

  chown root:opanel-ent-sites /etc/phpmyadmin/conf.d/opanel-ent-signon.php 2>/dev/null || chown root:www-data /etc/phpmyadmin/conf.d/opanel-ent-signon.php
  chmod 644 /etc/phpmyadmin/conf.d/opanel-ent-signon.php
  chmod 644 /usr/share/phpmyadmin/opanel-ent-signon.php
  fix_phpmyadmin_permissions
}

setup_openlitespeed() {
  local ols_conf="/usr/local/lsws/conf"
  local opanel_ent_conf="/usr/local/lsws/conf/opanel-ent"
  install -d -o root -g opanel-ent -m 2775 "${opanel_ent_conf}/vhosts"
  install -d -o root -g root -m 0755 "${opanel_ent_conf}/ssl/sites"
  install -d -o root -g root -m 0755 "${opanel_ent_conf}/modsec/sites"
  install -d -o root -g root -m 0755 "${opanel_ent_conf}/custom"

  # Configure main OLS listener for port 80/443
  write_tools_vhost_config
  systemctl restart lshttpd.service 2>/dev/null || /usr/local/lsws/bin/lswsctrl restart 2>/dev/null || true
}

setup_firewall() {
  local default_port seen_ssh_ports ssh_port
  fw() {
    timeout 15 "$@" 2>/dev/null
  }

  # Keep INPUT permissive during install. The panel manages OPANEL_* chains and
  # user rules; setting DROP here can lock out SSH if a distro has unusual SSH
  # socket/config state.
  fw iptables -P INPUT ACCEPT || true
  fw ip6tables -P INPUT ACCEPT || true
  fw iptables -P OUTPUT ACCEPT || true
  fw ip6tables -P OUTPUT ACCEPT || true

  fw iptables -N OPANEL_ENT_INPUT || fw iptables -F OPANEL_ENT_INPUT || true
  fw iptables -N OPANEL_ENT_USER || fw iptables -F OPANEL_ENT_USER || true
  fw iptables -N OPANEL_ENT_BLOCKLIST || fw iptables -F OPANEL_ENT_BLOCKLIST || true
  fw ip6tables -N OPANEL_ENT_INPUT || fw ip6tables -F OPANEL_ENT_INPUT || true
  fw ip6tables -N OPANEL_ENT_USER || fw ip6tables -F OPANEL_ENT_USER || true
  fw ip6tables -N OPANEL_ENT_BLOCKLIST || fw ip6tables -F OPANEL_ENT_BLOCKLIST || true

  fw ipset create opanel_ent_blocklist4 hash:net family inet -exist || true
  fw ipset create opanel_ent_blocklist6 hash:net family inet6 -exist || true

  fw iptables -C INPUT -j OPANEL_ENT_BLOCKLIST || fw iptables -I INPUT 1 -j OPANEL_ENT_BLOCKLIST || true
  fw iptables -C INPUT -j OPANEL_ENT_INPUT || fw iptables -I INPUT 2 -j OPANEL_ENT_INPUT || true
  fw iptables -C INPUT -j OPANEL_ENT_USER || fw iptables -I INPUT 3 -j OPANEL_ENT_USER || true
  fw ip6tables -C INPUT -j OPANEL_ENT_BLOCKLIST || fw ip6tables -I INPUT 1 -j OPANEL_ENT_BLOCKLIST || true
  fw ip6tables -C INPUT -j OPANEL_ENT_INPUT || fw ip6tables -I INPUT 2 -j OPANEL_ENT_INPUT || true
  fw ip6tables -C INPUT -j OPANEL_ENT_USER || fw ip6tables -I INPUT 3 -j OPANEL_ENT_USER || true

  fw iptables -A OPANEL_ENT_INPUT -m state --state ESTABLISHED,RELATED -j ACCEPT || true
  fw iptables -A OPANEL_ENT_INPUT -i lo -j ACCEPT || true
  fw ip6tables -A OPANEL_ENT_INPUT -m state --state ESTABLISHED,RELATED -j ACCEPT || true
  fw ip6tables -A OPANEL_ENT_INPUT -i lo -j ACCEPT || true

  fw iptables -A OPANEL_ENT_BLOCKLIST -m set --match-set opanel_ent_blocklist4 src -j DROP || true
  fw ip6tables -A OPANEL_ENT_BLOCKLIST -m set --match-set opanel_ent_blocklist6 src -j DROP || true

  fw iptables -A OPANEL_ENT_INPUT -p tcp --dport 22 -j ACCEPT || true
  fw ip6tables -A OPANEL_ENT_INPUT -p tcp --dport 22 -j ACCEPT || true
  seen_ssh_ports="$(detect_ssh_ports)"
  while read -r ssh_port; do
    [[ -n "$ssh_port" ]] || continue
    [[ "$ssh_port" == "22" ]] && continue
    fw iptables -A OPANEL_ENT_INPUT -p tcp --dport "$ssh_port" -j ACCEPT || true
    fw ip6tables -A OPANEL_ENT_INPUT -p tcp --dport "$ssh_port" -j ACCEPT || true
  done <<<"$seen_ssh_ports" || true

  fw iptables -A OPANEL_ENT_INPUT -p tcp --dport 80 -j ACCEPT || true
  fw iptables -A OPANEL_ENT_INPUT -p tcp --dport 443 -j ACCEPT || true
  fw ip6tables -A OPANEL_ENT_INPUT -p tcp --dport 80 -j ACCEPT || true
  fw ip6tables -A OPANEL_ENT_INPUT -p tcp --dport 443 -j ACCEPT || true

  for default_port in 465 587 "${PANEL_PORT}"; do
    [[ "$default_port" =~ ^[0-9]+$ ]] || continue
    fw iptables -A OPANEL_ENT_INPUT -p tcp --dport "$default_port" -j ACCEPT || true
    fw ip6tables -A OPANEL_ENT_INPUT -p tcp --dport "$default_port" -j ACCEPT || true
  done

  fw iptables -A OPANEL_ENT_INPUT -p icmp -j ACCEPT || true
  fw ip6tables -A OPANEL_ENT_INPUT -p ipv6-icmp -j ACCEPT || true

  install -d -o opanel-ent -g opanel-ent -m 0750 /var/lib/opanel-ent/firewall
  install -d -o root -g root -m 0755 /etc/iptables
  timeout 15 iptables-save >/etc/iptables/rules.v4 2>/dev/null || true
  timeout 15 ip6tables-save >/etc/iptables/rules.v6 2>/dev/null || true
  systemctl enable netfilter-persistent >/dev/null 2>&1 || true
  systemctl enable opanel-ent-firewall-blocklist.timer >/dev/null 2>&1 || true
  remove_ufw_legacy
}

setup_ssl() {
  if [[ "$ENABLE_SSL" != "yes" ]]; then
    return 0
  fi

  sudo -u opanel-ent env HOME="$APP_DIR" sudo -n /usr/local/sbin/opanel-ent-helper panel-ssl-install "$PANEL_DOMAIN" "$PANEL_PORT" "$SSL_EMAIL"
  PANEL_SSL_CERT=/etc/opanel-ent/panel-fullchain.pem
  PANEL_SSL_KEY=/etc/opanel-ent/panel-privkey.pem
  for _ in {1..20}; do
    if curl -kfsS --connect-timeout 2 --max-time 5 "https://127.0.0.1:${PANEL_PORT}/api/health" >/dev/null 2>&1; then
      return 0
    fi
    sleep 1
  done
  journalctl -u opanel-ent-api -n 80 --no-pager || true
  fail "opanel-ent-api did not respond after enabling panel SSL"
}

print_summary() {
  echo ""
  echo "=================================================="
  echo "Panel URL: ${PANEL_URL}"
  echo "User: admin"
  echo "Password: ${ADMIN_PASSWORD}"
  echo "=================================================="
}

write_login_info() {
  local tmp
  (
    if command -v flock >/dev/null 2>&1; then
      flock -x 9
    fi
    tmp="$(mktemp /root/login.txt.XXXXXX)"
    chmod 600 "$tmp"
    cat >"$tmp" <<INFO
Panel URL: ${PANEL_URL}
User: admin
Password: ${ADMIN_PASSWORD}
INFO
    mv -f "$tmp" /root/login.txt
  ) 9>/root/.opanel-ent-login.lock
  chmod 600 /root/login.txt
}

source_version() {
  if [[ -f "${PROJECT_ROOT}/VERSION" ]]; then
    tr -d '[:space:]' <"${PROJECT_ROOT}/VERSION"
    return 0
  fi
  sed -nE 's/^APP_VERSION = "([^"]+)"/\1/p' "${PROJECT_ROOT}/backend/app/core/version.py" 2>/dev/null | head -n 1
}

write_update_state() {
  local version now
  version="$(source_version)"
  version="${version:-1.0.4}"
  now="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  install -d -o opanel-ent -g opanel-ent -m 0750 /var/lib/opanel-ent
  cat >/var/lib/opanel-ent/update-status.json <<STATE
{
  "current_version": "${version}",
  "latest_tag": "v${version}",
  "latest_version": "${version}",
  "last_checked_at": "${now}",
  "last_update_finished_at": "${now}",
  "last_update_ref": "v${version}",
  "last_update_status": "installed"
}
STATE
  chown opanel-ent:opanel-ent /var/lib/opanel-ent/update-status.json 2>/dev/null || true
  chmod 0640 /var/lib/opanel-ent/update-status.json
}

cleanup_release_source() {
  [[ "${CLEAN_RELEASE_SOURCE:-true}" == "true" ]] || return 0
  [[ "$PROJECT_ROOT" == "/opt/opanel-ent-source" ]] || return 0
  [[ ! -d "${PROJECT_ROOT}/.git" ]] || return 0
  log "Removing release source from ${PROJECT_ROOT}"
  cd /
  rm -rf "$PROJECT_ROOT" /tmp/opanel-ent-release /tmp/opanel-ent-release.zip
}

main() {
  validate_sources
  ask_panel_url

  log "Installing base packages"
  install_base_packages

  log "Installing OpenLiteSpeed and LSPHP"
  install_openlitespeed

  log "Installing Node.js ${NODE_MAJOR} from NodeSource"
  install_nodejs

  log "Configuring PHP ${PHP_VERSIONS} for LSPHP"
  install_php

  log "Installing ModSecurity WAF engine"
  if ! install_waf_engine; then
    echo "WARNING: WAF engine installation failed; continuing without ModSecurity."
  fi

  log "Installing WP-CLI"
  install_wp_cli

  log "Copying source to ${APP_DIR}"
  copy_sources

  log "Building frontend"
  build_frontend

  log "Creating opanel-ent system user, MariaDB credentials and filesystem ACLs"
  setup_panel_user

  log "Configuring SFTP access for panel users"
  setup_sftp_access

  log "Installing privileged helper and sudoers rule"
  install_privileged_helper

  log "Installing SSH maintenance menu"
  install_panel_cli

  log "Validating privileged helper"
  validate_privileged_helper

  log "Configuring backend"
  setup_backend

  log "Creating systemd service (hardened, runs as opanel-ent user)"
  setup_systemd

  log "Configuring phpMyAdmin SSO"
  setup_phpmyadmin_sso

  log "Preparing OpenLiteSpeed for customer websites"
  setup_openlitespeed

  log "Configuring firewall"
  setup_firewall

  log "Configuring SSL"
  setup_ssl

  write_login_info
  write_update_state

  print_summary
  cleanup_release_source
}

main "$@"
