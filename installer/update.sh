#!/usr/bin/env bash
# Update opanel-ent from GitHub.
#
# By default this script downloads the newest stable release zip to a temporary
# directory, syncs the source into /opt/opanel-ent, rebuilds the frontend, refreshes
# helper scripts, restarts the API, and reloads nginx for customer vhosts. A git
# checkout is only used for --branch or --skip-pull development workflows.
#
# Usage:
#   sudo bash installer/update.sh
#   sudo bash installer/update.sh --branch main
#   sudo bash installer/update.sh --tag v1.0.4
#   sudo bash installer/update.sh --help

set -euo pipefail

log()  { echo ""; echo "==> $1"; }
fail() { echo "ERROR: $1" >&2; exit 1; }

usage() {
  cat <<'USAGE'
Usage:
  sudo bash installer/update.sh
  sudo bash installer/update.sh --tag v1.0.4
  sudo bash installer/update.sh --branch main

Options:
  --release          Update to the newest matching release tag.
  --tag TAG          Pin an exact release tag.
  --branch NAME      Update from a remote branch.
  --channel MODE     Set update mode: release, tag, or branch.
  --remote NAME      Git remote to fetch from or create on clone (default: origin).
  --skip-pull        Sync the current SOURCE_DIR without fetching/checking out.
  --refresh-sites    Force the per-site refresh (re-harden files, rewrite every
                     vhost/WAF file). It is skipped automatically when nothing
                     site-facing changed since the last update.
  --app-dir DIR      Production deployment dir (default: /opt/opanel-ent).
  -h, --help         Show this help.

Environment:
  APP_DIR, SOURCE_DIR, REPO_URL, GIT_REMOTE, UPDATE_CHANNEL, BRANCH,
  RELEASE_TAG, RELEASE_PATTERN, RELEASE_ZIP_URL, SKIP_PULL, FORCE_SITE_REFRESH.

Notes:
  The branch channel is the default and pulls GitHub main. The release channel
  selects the newest matching release tag and downloads a zip archive. Use
  --tag or UPDATE_CHANNEL=tag with RELEASE_TAG to pin a tag.
  Use RELEASE_ZIP_URL with {tag} only for non-GitHub archive URLs.
USAGE
}

require_arg() {
  local opt="$1" value="${2-}"
  [[ -n "$value" && "$value" != --* ]] || fail "$opt requires a value"
}

for arg in "$@"; do
  if [[ "$arg" == "-h" || "$arg" == "--help" ]]; then
    usage
    exit 0
  fi
done

if [[ $EUID -ne 0 ]]; then
  echo "Please run as root"
  exit 1
fi

if [[ -z "${opanel_ent_UPDATE_STABLE_COPY:-}" ]]; then
  _original_script="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/$(basename "${BASH_SOURCE[0]}")"
  _stable_copy="$(mktemp /tmp/opanel-ent-update.XXXXXX.sh)"
  cp "$_original_script" "$_stable_copy"
  chmod 0700 "$_stable_copy"
  opanel_ent_UPDATE_STABLE_COPY="$_stable_copy" \
    opanel_ent_UPDATE_ORIGINAL_SCRIPT="$_original_script" \
    exec /bin/bash "$_stable_copy" "$@"
fi

cleanup_stable_copy() {
  rm -f "${opanel_ent_UPDATE_STABLE_COPY:-}" 2>/dev/null || true
}
trap cleanup_stable_copy EXIT

# --- Config ----------------------------------------------------------------
APP_DIR="${APP_DIR:-/opt/opanel-ent}"                 # Production deployment dir
DEFAULT_SOURCE_DIR="/opt/opanel-ent-source"           # Dev/branch checkout dir only

# Resolve where THIS script lives. If it's inside a real git checkout we use
# that. Otherwise we fall back to /opt/opanel-ent-source so users running the
# script from the deploy dir still get a usable workflow.
_SCRIPT_SOURCE="${opanel_ent_UPDATE_ORIGINAL_SCRIPT:-${BASH_SOURCE[0]}}"
_SCRIPT_DIR="$(cd "$(dirname "$_SCRIPT_SOURCE")/.." && pwd)"
if [[ -d "$_SCRIPT_DIR/.git" ]]; then
  SOURCE_DIR="${SOURCE_DIR:-$_SCRIPT_DIR}"
else
  SOURCE_DIR="${SOURCE_DIR:-$DEFAULT_SOURCE_DIR}"
fi

REPO_URL="${REPO_URL:-https://github.com/bnixvn/opanel-ent.git}"
GIT_REMOTE="${GIT_REMOTE-origin}"                 # remote name in the local checkout
UPDATE_CHANNEL="${UPDATE_CHANNEL-branch}"         # branch, release, or tag
BRANCH="${BRANCH-main}"                           # used when UPDATE_CHANNEL=branch
RELEASE_TAG="${RELEASE_TAG-}"                     # used when UPDATE_CHANNEL=tag
RELEASE_PATTERN="${RELEASE_PATTERN:-v[0-9]*.[0-9]*.[0-9]*}"
RELEASE_ZIP_URL="${RELEASE_ZIP_URL:-}"             # optional archive URL template with {tag}
SKIP_PULL="${SKIP_PULL:-false}"
FORCE_SITE_REFRESH="${FORCE_SITE_REFRESH:-false}"
UPDATE_STATE_FILE="${UPDATE_STATE_FILE:-/var/lib/opanel-ent/update-status.json}"
RELEASE_WORK_DIR=""

while [[ $# -gt 0 ]]; do
  case "$1" in
    --channel) require_arg "$1" "${2-}"; UPDATE_CHANNEL="$2"; shift 2 ;;
    --release) UPDATE_CHANNEL="release"; shift ;;
    --branch) require_arg "$1" "${2-}"; UPDATE_CHANNEL="branch"; BRANCH="$2"; shift 2 ;;
    --tag) require_arg "$1" "${2-}"; UPDATE_CHANNEL="tag"; RELEASE_TAG="$2"; shift 2 ;;
    --remote) require_arg "$1" "${2-}"; GIT_REMOTE="$2"; shift 2 ;;
    --skip-pull) SKIP_PULL="true"; shift ;;
    --refresh-sites) FORCE_SITE_REFRESH="true"; shift ;;
    --app-dir) require_arg "$1" "${2-}"; APP_DIR="$2"; shift 2 ;;
    -h|--help)
      usage
      exit 0 ;;
    *) fail "Unknown arg: $1" ;;
  esac
done

# --- Validate config --------------------------------------------------------
require_git_ref_name() {
  local kind="$1" value="$2"
  [[ -n "$value" ]] || fail "$kind cannot be empty"
  [[ "$value" != -* ]] || fail "$kind must not start with '-'"
  case "$kind" in
    BRANCH)
      git check-ref-format --branch "$value" >/dev/null 2>&1 \
        || fail "BRANCH has invalid git ref characters: $value"
      ;;
    RELEASE_TAG)
      git check-ref-format "refs/tags/$value" >/dev/null 2>&1 \
        || fail "RELEASE_TAG has invalid git ref characters: $value"
      ;;
  esac
}

[[ -n "$REPO_URL" ]] || fail "REPO_URL cannot be empty"
[[ -n "$GIT_REMOTE" ]] || fail "GIT_REMOTE cannot be empty"
[[ "$GIT_REMOTE" != -* ]] || fail "GIT_REMOTE must not start with '-'"
if ! [[ "$GIT_REMOTE" =~ ^[A-Za-z0-9._-]+$ ]]; then
  fail "GIT_REMOTE must match [A-Za-z0-9._-]+ (got: $GIT_REMOTE)"
fi
if [[ "$UPDATE_CHANNEL" != "release" && "$UPDATE_CHANNEL" != "branch" && "$UPDATE_CHANNEL" != "tag" ]]; then
  fail "UPDATE_CHANNEL must be release|branch|tag (got: $UPDATE_CHANNEL)"
fi
[[ "$SKIP_PULL" == "true" || "$SKIP_PULL" == "false" ]] || fail "SKIP_PULL must be true|false"
if [[ "$UPDATE_CHANNEL" == "branch" ]]; then
  require_git_ref_name BRANCH "$BRANCH"
fi
if [[ "$UPDATE_CHANNEL" == "tag" ]]; then
  require_git_ref_name RELEASE_TAG "$RELEASE_TAG"
fi
if [[ "$UPDATE_CHANNEL" == "release" && -n "$RELEASE_TAG" ]]; then
  echo "INFO: ignoring RELEASE_TAG in release channel; use --tag to pin ${RELEASE_TAG}."
fi

env_get() {
  local file="$APP_DIR/backend/.env" key="$1"
  [[ -f "$file" ]] || return 0
  awk -F= -v key="$key" '$1 == key { sub(/^[^=]*=/, ""); print; exit }' "$file"
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

is_domain_name() {
  local value="$1"
  ! is_ipv4 "$value" && [[ "$value" =~ ^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)+$ ]]
}

env_set() {
  local file="$APP_DIR/backend/.env" key="$1" value="$2" escaped
  [[ -f "$file" ]] || return 0
  escaped="$(printf '%s' "$value" | sed -e 's/[&|]/\\&/g')"
  if grep -q "^${key}=" "$file"; then
    sed -i "s|^${key}=.*|${key}=${escaped}|" "$file"
  else
    printf '%s=%s\n' "$key" "$value" >>"$file"
  fi
}

env_set_default() {
  local file="$APP_DIR/backend/.env" key="$1" value="$2"
  if ! grep -q "^${key}=" "$file"; then
    printf '%s=%s\n' "$key" "$value" >>"$file"
  fi
}

ensure_git_remote() {
  if git remote get-url "$GIT_REMOTE" >/dev/null 2>&1; then
    return 0
  fi
  log "Adding git remote ${GIT_REMOTE} -> ${REPO_URL}"
  git remote add "$GIT_REMOTE" "$REPO_URL"
}

latest_release_tag() {
  git ls-remote --tags --refs "$REPO_URL" "refs/tags/${RELEASE_PATTERN}" \
    | awk '{ sub("refs/tags/", "", $2); print $2 }' \
    | sort -V \
    | tail -n 1
}

release_archive_url() {
  local tag="$1" repo="$REPO_URL" base
  if [[ -n "$RELEASE_ZIP_URL" ]]; then
    printf '%s\n' "${RELEASE_ZIP_URL//\{tag\}/$tag}"
    return 0
  fi
  case "$repo" in
    https://github.com/*)
      base="${repo%.git}"
      ;;
    git@github.com:*)
      base="https://github.com/${repo#git@github.com:}"
      base="${base%.git}"
      ;;
    *)
      fail "Cannot derive release zip URL from REPO_URL. Set RELEASE_ZIP_URL or use --branch."
      ;;
  esac
  printf '%s/archive/refs/tags/%s.zip\n' "$base" "$tag"
}

download_release_source() {
  local tag="$1" archive extract_dir archive_url
  RELEASE_WORK_DIR="$(mktemp -d /tmp/opanel-ent-release-update.XXXXXX)"
  archive="${RELEASE_WORK_DIR}/opanel-ent-release.zip"
  extract_dir="${RELEASE_WORK_DIR}/extract"
  archive_url="$(release_archive_url "$tag")"
  log "Downloading release ${tag}"
  curl -fL --connect-timeout 10 --max-time 300 "$archive_url" -o "$archive"
  mkdir -p "$extract_dir"
  unzip -q "$archive" -d "$extract_dir"
  SOURCE_DIR="$(find "$extract_dir" -mindepth 1 -maxdepth 1 -type d | head -n 1)"
  [[ -n "$SOURCE_DIR" && -d "$SOURCE_DIR/backend" && -d "$SOURCE_DIR/frontend" ]] \
    || fail "Release archive does not contain backend/frontend source"
}

reset_worktree_to_ref() {
  local ref="$1" branch_name="${2-}"
  if [[ -n "$branch_name" ]]; then
    git checkout -f -B "$branch_name" "$ref"
  else
    git checkout -f --detach "$ref"
  fi
  git reset --hard "$ref"
}

current_panel_version() {
  if [[ -f "$APP_DIR/VERSION" ]]; then
    tr -d '[:space:]' <"$APP_DIR/VERSION"
    return 0
  fi
  sed -nE 's/^APP_VERSION = "([^"]+)"/\1/p' "$APP_DIR/backend/app/core/version.py" 2>/dev/null | head -n 1
}

write_update_state() {
  local status="$1" ref="${2:-}" message="${3:-}" now current latest
  now="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  current="$(current_panel_version)"
  latest="${ref#v}"
  [[ "$latest" == "$ref" ]] && latest=""
  install -d -m 0750 "$(dirname "$UPDATE_STATE_FILE")"
  python3 - "$UPDATE_STATE_FILE" "$status" "$ref" "$latest" "$current" "$message" "$now" <<'PY'
import json
import sys
from pathlib import Path

path = Path(sys.argv[1])
status, ref, latest, current, message, now = sys.argv[2:8]
try:
    state = json.loads(path.read_text(encoding="utf-8")) if path.exists() else {}
except Exception:
    state = {}

if current:
    state["current_version"] = current
if latest:
    state["latest_tag"] = ref
    state["latest_version"] = latest
state["last_update_status"] = status
if ref:
    state["last_update_ref"] = ref
if message:
    state["last_update_message"] = message
if status in {"checking", "updating"}:
    state["last_update_started_at"] = now
elif status in {"completed", "failed"}:
    state["last_update_finished_at"] = now
path.write_text(json.dumps(state, indent=2, sort_keys=True) + "\n", encoding="utf-8")
PY
  if id -u opanel-ent >/dev/null 2>&1; then
    chown opanel-ent:opanel-ent "$UPDATE_STATE_FILE" 2>/dev/null || true
  fi
  chmod 0640 "$UPDATE_STATE_FILE" 2>/dev/null || true
}

# Write progress markers (percent / phase / message) into the update state file
# without touching the existing status/version fields. Safe to call at any time.
update_progress() {
  local percent="$1" phase="$2" message="${3:-}"
  install -d -m 0750 "$(dirname "$UPDATE_STATE_FILE")"
  python3 - "$UPDATE_STATE_FILE" "$percent" "$phase" "$message" <<'PY'
import json
import sys
from pathlib import Path

path = Path(sys.argv[1])
percent, phase, message = sys.argv[2:5]
try:
    state = json.loads(path.read_text(encoding="utf-8")) if path.exists() else {}
except Exception:
    state = {}
try:
    state["progress_percent"] = int(percent)
except (ValueError, TypeError):
    state["progress_percent"] = 0
if phase:
    state["progress_phase"] = phase
if message:
    state["progress_message"] = message
path.write_text(json.dumps(state, indent=2, sort_keys=True) + "\n", encoding="utf-8")
PY
  if id -u opanel-ent >/dev/null 2>&1; then
    chown opanel-ent:opanel-ent "$UPDATE_STATE_FILE" 2>/dev/null || true
  fi
  chmod 0640 "$UPDATE_STATE_FILE" 2>/dev/null || true
}

UPDATE_REF=""
cleanup_release_work_dir() {
  if [[ -n "${RELEASE_WORK_DIR:-}" && -d "$RELEASE_WORK_DIR" ]]; then
    rm -rf -- "$RELEASE_WORK_DIR"
  fi
}

finish_update_script() {
  local rc=$?
  if [[ $rc -ne 0 ]]; then
    local cur_pct=0
    if [[ -f "$UPDATE_STATE_FILE" ]]; then
      cur_pct="$(python3 - "$UPDATE_STATE_FILE" <<'PY' 2>/dev/null || true
import json, sys
try:
    print(json.load(open(sys.argv[1])).get("progress_percent", 0))
except Exception:
    print(0)
PY
)"
    fi
    update_progress "${cur_pct:-0}" "failed" "Update failed with exit code ${rc}" || true
    write_update_state "failed" "${UPDATE_REF:-}" "Update failed with exit code ${rc}" || true
  fi
  cleanup_release_work_dir
  cleanup_stable_copy
}
trap finish_update_script EXIT

# Trap ERR so any failing command records a failed progress marker. With
# `set -e` the EXIT trap above performs the actual state write; this is a
# secondary safety net for non-fatal paths that call `|| true`/return codes.
trap 'update_progress "${progress_percent:-0}" "failed" "Update failed at phase: ${progress_phase:-unknown}" 2>/dev/null || true' ERR

iptables_panel_allow_port() {
  local port="$1"
  [[ "$port" =~ ^[0-9]+$ ]] || return 0
  if [[ -x /usr/local/sbin/opanel-ent-helper ]] && id -u opanel-ent >/dev/null 2>&1; then
    sudo -u opanel-ent env HOME="$APP_DIR" sudo -n /usr/local/sbin/opanel-ent-helper iptables-panel-allow-port "$port" >/dev/null 2>&1 || true
    return 0
  fi
  iptables -N OPANEL_ENT_INPUT 2>/dev/null || true
  while iptables -D OPANEL_ENT_INPUT -p tcp --dport "$port" -j ACCEPT 2>/dev/null; do :; done
  iptables -I OPANEL_ENT_INPUT 1 -p tcp --dport "$port" -j ACCEPT -m comment --comment "opanel-ent:PanelZone" 2>/dev/null \
    || iptables -I OPANEL_ENT_INPUT 1 -p tcp --dport "$port" -j ACCEPT 2>/dev/null \
    || true
}

host_has_global_ipv6() {
  # scope 00 in /proc/net/if_inet6 is a global address; loopback never counts.
  awk '$4 == "00" && $6 != "lo" { found = 1 } END { exit found ? 0 : 1 }' /proc/net/if_inet6 2>/dev/null
}

detect_server_ip() {
  hostname -I 2>/dev/null | awk '{print $1}' || true
}

remove_filebrowser_runtime() {
  systemctl disable --now filebrowser 2>/dev/null || true
  rm -f /etc/systemd/system/filebrowser.service
  rm -rf /etc/systemd/system/filebrowser.service.d
  rm -rf /etc/filebrowser /var/lib/filebrowser
  rm -f /usr/local/bin/filebrowser
  sed -i '/^FILEBROWSER_PORT=/d' "$APP_DIR/backend/.env" 2>/dev/null || true
  systemctl daemon-reload
}

write_tools_nginx_config() {
  local panel_port panel_url panel_domain panel_cert panel_key php_version server_ip host api_scheme tools_scheme pma_secure ssl_block
  panel_port="$(env_get PANEL_PORT)"; panel_port="${panel_port:-2222}"
  panel_url="$(env_get PANEL_URL)"
  panel_domain="$(env_get PANEL_DOMAIN)"
  panel_cert="$(env_get PANEL_SSL_CERT)"
  panel_key="$(env_get PANEL_SSL_KEY)"
  php_version="${PHP_DEFAULT:-8.4}"
  server_ip="$(detect_server_ip)"
  host="${panel_domain:-$server_ip}"
  api_scheme="http"; tools_scheme="http"; pma_secure="false"; ssl_block=""
  if [[ "$panel_url" == https://* && -n "$panel_cert" && -n "$panel_key" && -f "$panel_cert" && -f "$panel_key" ]]; then
    api_scheme="https"; tools_scheme="https"; pma_secure="true"
    printf -v ssl_block '\n    listen 443 ssl http2 default_server;\n    ssl_certificate %s;\n    ssl_certificate_key %s;' "$panel_cert" "$panel_key"
  fi
  cat >/etc/nginx/conf.d/00-opanel-ent-tools.conf <<NGINX
server {
    listen 80 default_server;${ssl_block}
    server_name _;
    client_max_body_size 1100M;

    location = /phpmyadmin { return 301 /phpmyadmin/; }
    location /phpmyadmin/ { alias /usr/share/phpmyadmin/; index index.php; try_files \$uri \$uri/ =404; }
    location ~ ^/phpmyadmin/(.+\.php)$ {
        alias /usr/share/phpmyadmin/\$1;
        include fastcgi_params;
        fastcgi_param SCRIPT_FILENAME /usr/share/phpmyadmin/\$1;
        fastcgi_param SCRIPT_NAME /phpmyadmin/\$1;
        fastcgi_pass unix:/run/php/php${php_version}-fpm.sock;
        fastcgi_read_timeout 300;
    }
}
NGINX
  mkdir -p /etc/phpmyadmin/conf.d
  sed -i -E "/api\/databases\/phpmyadmin-sso/s#'[^']+/api/databases/phpmyadmin-sso/'#'${api_scheme}://127.0.0.1:${panel_port}/api/databases/phpmyadmin-sso/'#" /usr/share/phpmyadmin/opanel-ent-signon.php 2>/dev/null || true
  sed -i -E "s#('secure' => )(true|false)#\1${pma_secure}#" /etc/phpmyadmin/conf.d/opanel-ent-signon.php /usr/share/phpmyadmin/opanel-ent-signon.php 2>/dev/null || true
  [[ -n "$host" ]] && sed -i -E "/PmaAbsoluteUri/s#'https?://[^']+/phpmyadmin/'#'${tools_scheme}://${host}/phpmyadmin/'#" /etc/phpmyadmin/conf.d/opanel-ent-signon.php 2>/dev/null || true
}

configure_fastcgi_cache() {
  install -d -o www-data -g www-data -m 0755 /var/cache/nginx/opanel-ent-fastcgi
  find /var/cache/nginx/opanel-ent-fastcgi -mindepth 1 -delete
  cat >/etc/nginx/conf.d/00-opanel-ent-fastcgi-cache.conf <<'NGINX'
fastcgi_cache_path /var/cache/nginx/opanel-ent-fastcgi levels=1:2 keys_zone=opanel_ent_FASTCGI:32m inactive=30m max_size=256m use_temp_path=off;
fastcgi_cache_key "$scheme$request_method$host$request_uri";
NGINX
}

migrate_nginx_wordpress_csp_worker_src() {
  python3 - <<'PY'
from pathlib import Path

roots = [Path("/etc/nginx/conf.d"), Path("/etc/nginx/sites-enabled")]
needle = "worker-src 'self' blob:;"
anchor = "frame-src 'self' https: blob:;"

for root in roots:
    if not root.exists():
        continue
    for path in sorted(root.glob("*.conf")):
        try:
            text = path.read_text(encoding="utf-8")
        except UnicodeDecodeError:
            text = path.read_text(encoding="latin-1")
        if "Content-Security-Policy" not in text or needle in text:
            continue
        if anchor in text:
            new_text = text.replace(anchor, f"{anchor} {needle}")
        else:
            new_text = text.replace("object-src", f"{needle} object-src")
        if new_text != text:
            path.write_text(new_text, encoding="utf-8")
            print(f"Updated CSP worker-src in {path}")
PY
}

migrate_drop_iframe_blocking() {
  python3 - <<'PY'
import re
from pathlib import Path

roots = [
    Path("/etc/nginx/conf.d"),
    Path("/etc/nginx/sites-enabled"),
    Path("/etc/nginx/opanel-ent/custom"),
    Path("/usr/local/lsws/conf/opanel-ent/vhosts"),
]
XFO_LINE = re.compile(r'(?im)^[ \t]*(?:add_header[ \t]+)?"?X-Frame-Options"?[ \t:].*\r?\n')
FRAME_ANCESTORS = re.compile(r'(?i)frame-ancestors[^;"\n]*(?:;[ \t]*)?')

for root in roots:
    if not root.exists():
        continue
    for path in sorted(root.rglob("*.conf")):
        try:
            text = path.read_text(encoding="utf-8")
        except (UnicodeDecodeError, OSError):
            continue
        new_text = FRAME_ANCESTORS.sub("", XFO_LINE.sub("", text))
        if new_text != text:
            path.write_text(new_text, encoding="utf-8")
            print(f"Removed iframe blocking from {path}")
PY
  if command -v nginx >/dev/null 2>&1 && nginx -t >/dev/null 2>&1; then
    systemctl reload nginx >/dev/null 2>&1 || true
  fi
}

ensure_terminal_tools() {
  local missing=()
  command -v composer >/dev/null 2>&1 || missing+=(composer)
  command -v zip >/dev/null 2>&1 || missing+=(zip)
  command -v unzip >/dev/null 2>&1 || missing+=(unzip)
  command -v zstd >/dev/null 2>&1 || missing+=(zstd)
  if [[ ${#missing[@]} -gt 0 ]]; then
    log "Installing terminal/file-manager tools: ${missing[*]}"
    DEBIAN_FRONTEND=noninteractive apt-get update --allow-releaseinfo-change
    DEBIAN_FRONTEND=noninteractive apt-get install -y "${missing[@]}"
  fi
}

ensure_dns_ssl_plugin() {
  # Wildcard SSL via Cloudflare DNS-01. The helper also installs this on
  # demand, but doing it here keeps the first issuance fast.
  command -v certbot >/dev/null 2>&1 || return 0
  if ! certbot plugins 2>/dev/null | grep -q 'dns-cloudflare'; then
    log "Installing certbot Cloudflare DNS plugin"
    DEBIAN_FRONTEND=noninteractive apt-get install -y python3-certbot-dns-cloudflare >/dev/null 2>&1 || true
  fi
}

remove_ufw_legacy() {
  systemctl disable --now ufw >/dev/null 2>&1 || true
  command -v ufw >/dev/null 2>&1 && timeout 15 ufw --force disable >/dev/null 2>&1 || true
  DEBIAN_FRONTEND=noninteractive apt-get purge -y ufw >/dev/null 2>&1 || true
  rm -rf /etc/ufw /var/lib/ufw 2>/dev/null || true
}

# Bump when the recursive per-site hardening below changes. Runs that already
# stamped the current version skip the (expensive) recursive chown/chmod walk and
# only re-apply the cheap home-dir and php-dir perms. FORCE_SITE_REFRESH=true
# re-runs it regardless.
HARDEN_SITES_VERSION=2

harden_existing_panel_users() {
  local user home_dir site_dir
  getent group opanel-ent-sftp >/dev/null || return 0
  chown root:root /home
  chmod 0711 /home
  chmod a-s /home 2>/dev/null || true
  chmod -t /home 2>/dev/null || true
  local harden_marker="/var/lib/opanel-ent/harden-sites.version"
  local do_recursive=1
  if [[ "$FORCE_SITE_REFRESH" != "true" \
        && "$(cat "$harden_marker" 2>/dev/null || true)" == "$HARDEN_SITES_VERSION" ]]; then
    do_recursive=0
    log "Panel-user site permissions already at v${HARDEN_SITES_VERSION} -- skipping the recursive re-harden"
  fi
  while IFS= read -r user; do
    [[ -n "$user" ]] || continue
    case "$user" in
      root|daemon|bin|sys|sync|games|man|lp|mail|news|uucp|proxy|www-data|backup|list|irc|_apt|nobody|opanel-ent|opanel-ent-sites|opanel-ent-sftp|mysql|redis|nginx)
        continue ;;
    esac
    id -u "$user" >/dev/null 2>&1 || continue
    getent group "$user" >/dev/null || groupadd "$user" 2>/dev/null || true
    usermod -aG "$user" www-data 2>/dev/null || true
    home_dir="/home/$user"
    usermod --home "$home_dir" --shell /usr/sbin/nologin --gid "$user" "$user" 2>/dev/null || true
    mkdir -p "$home_dir"
    chown "root:$user" "$home_dir"
    chmod 0751 "$home_dir"
    chmod a-s "$home_dir" 2>/dev/null || true
    chmod -t "$home_dir" 2>/dev/null || true
    if command -v setfacl >/dev/null 2>&1; then
      setfacl -b -k "$home_dir" 2>/dev/null || true
    fi
    if [[ "$do_recursive" == 1 ]]; then
      find "$home_dir" -mindepth 1 -maxdepth 1 -type d -print0 2>/dev/null | while IFS= read -r -d '' site_dir; do
        [[ "$(basename "$site_dir")" =~ ^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)+$ ]] || continue
        chown -R "$user:$user" "$site_dir" 2>/dev/null || true
        if command -v setfacl >/dev/null 2>&1; then
          setfacl -Rb "$site_dir" 2>/dev/null || true
          find "$site_dir" -type d -exec setfacl -k {} + 2>/dev/null || true
        fi
        find "$site_dir" -type d -exec chmod 755 {} + 2>/dev/null || true
        find "$site_dir" -type d -exec chmod a-s {} + 2>/dev/null || true
        find "$site_dir" -type d -exec chmod -t {} + 2>/dev/null || true
        find "$site_dir" -type f -exec chmod 644 {} + 2>/dev/null || true
      done
    fi
    if [[ -d "/var/lib/php/uploads/$user" ]]; then
      chown "$user:$user" "/var/lib/php/uploads/$user" 2>/dev/null || true
      chmod 0700 "/var/lib/php/uploads/$user" 2>/dev/null || true
      chmod g-s "/var/lib/php/uploads/$user" 2>/dev/null || true
    fi
  done < <(getent group opanel-ent-sftp | awk -F: '{gsub(",", "\n", $4); print $4}')
  if [[ "$do_recursive" == 1 ]]; then
    install -d -o opanel-ent -g opanel-ent -m 0750 /var/lib/opanel-ent 2>/dev/null || mkdir -p /var/lib/opanel-ent
    printf '%s\n' "$HARDEN_SITES_VERSION" > "$harden_marker" 2>/dev/null || true
  fi
}

install_panel_runtime() {
  local env_file="$APP_DIR/backend/.env"
  [[ -f "$env_file" ]] || return 0
  local panel_port panel_url panel_domain server_ip sshd_config sshd_backup
  panel_port="$(env_get PANEL_PORT)"
  panel_port="${panel_port:-2222}"
  server_ip="$(detect_server_ip)"
  panel_url="$(env_get PANEL_URL)"
  panel_url="${panel_url:-http://${server_ip:-127.0.0.1}:${panel_port}}"

  env_set_default PANEL_PORT "$panel_port"
  env_set_default PANEL_URL "$panel_url"
  env_set_default PANEL_DOMAIN ""
  env_set_default PANEL_SSL_CERT ""
  env_set_default PANEL_SSL_KEY ""
  env_set_default FRONTEND_DIST "$APP_DIR/frontend/dist"
  env_set_default REDIS_URL "redis://localhost:6379/0"
  env_set_default RATE_LIMIT_BACKEND "redis"
  if [[ -z "$(env_get ALLOWED_ORIGINS)" ]]; then
    env_set_default ALLOWED_ORIGINS "$panel_url"
  fi
  # Outside the block above: boxes installed before this key existed already
  # have ALLOWED_ORIGINS, so nesting it there meant they never got a secret and
  # phpMyAdmin single sign-on stayed broken for good.
  env_set_default PMA_SIGNON_SECRET "$(openssl rand -hex 32)"
  panel_domain="$(env_get PANEL_DOMAIN)"
  if [[ -n "$panel_domain" ]] && ! is_domain_name "$panel_domain"; then
    env_set PANEL_DOMAIN ""
  fi
  if [[ "$panel_url" != https://* ]]; then
    env_set PANEL_SSL_CERT ""
    env_set PANEL_SSL_KEY ""
  fi

  # Auto-issue Let's Encrypt cert if domain is set but cert files are missing
  if [[ "$panel_url" == https://* && -n "$panel_domain" ]]; then
    _pcert="$(env_get PANEL_SSL_CERT)"; _pkey="$(env_get PANEL_SSL_KEY)"
    if [[ -z "$_pcert" || -z "$_pkey" || ! -f "$_pcert" || ! -f "$_pkey" ]]; then
      log "SSL cert missing for $panel_domain â€” requesting Let's Encrypt certificate"
      sudo -u opanel-ent env HOME="$APP_DIR" sudo -n /usr/local/sbin/opanel-ent-helper panel-ssl-install "$panel_domain" "$panel_port" "$(env_get SSL_EMAIL)" || \
        echo "WARNING: certbot failed; panel will use self-signed cert"
    fi
  fi

  remove_filebrowser_runtime

  getent group opanel-ent-sites >/dev/null || groupadd --system opanel-ent-sites
  getent group opanel-ent-sftp >/dev/null || groupadd --system opanel-ent-sftp
  if ! command -v setfacl >/dev/null 2>&1 && command -v apt-get >/dev/null 2>&1; then
    DEBIAN_FRONTEND=noninteractive apt-get update --allow-releaseinfo-change
    DEBIAN_FRONTEND=noninteractive apt-get install -y acl
  fi
  usermod -aG opanel-ent-sites opanel-ent 2>/dev/null || true
  usermod -aG opanel-ent-sites www-data 2>/dev/null || true
  harden_existing_panel_users
  install -d -o root -g opanel-ent -m 2775 /etc/nginx/conf.d
  chmod g+s /etc/nginx/conf.d 2>/dev/null || true
  install -d -o root -g opanel-ent -m 2775 /etc/nginx/opanel-ent/custom
  chmod g+s /etc/nginx/opanel-ent/custom 2>/dev/null || true
  install -d -o opanel-ent -g opanel-ent -m 0750 /var/lib/opanel-ent
  if command -v sshd >/dev/null 2>&1; then
    sshd_config="/etc/ssh/sshd_config"
    sshd_backup="${sshd_config}.opanel-ent.bak"
    install -d -o root -g root -m 0755 /run/sshd
    rm -f /etc/ssh/sshd_config.d/99-opanel-ent-sftp.conf 2>/dev/null || true
    touch "$sshd_config"
    cp "$sshd_config" "$sshd_backup"
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
    if sshd -t; then
      systemctl reload ssh 2>/dev/null || systemctl reload sshd 2>/dev/null || true
    else
      cp "$sshd_backup" "$sshd_config"
      echo "WARNING: invalid SSHD configuration; skipped opanel-ent SFTP password block"
    fi
  fi

  cat >/usr/local/sbin/opanel-ent-api-start <<STARTER
#!/usr/bin/env bash
# Trusted forwarders: only a local reverse proxy (127.0.0.1) is allowed to set
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
  mkdir -p /etc/systemd/system/opanel-ent-api.service.d
  cat >/etc/systemd/system/opanel-ent-api.service.d/20-panel-port.conf <<SERVICE
[Service]
WorkingDirectory=${APP_DIR}/backend
EnvironmentFile=
EnvironmentFile=${APP_DIR}/backend/.env
Environment=HOME=${APP_DIR}
ExecStart=
ExecStart=/usr/local/sbin/opanel-ent-api-start
SupplementaryGroups=www-data opanel-ent-sites
ProtectHome=false
ReadWritePaths=
ReadWritePaths=${APP_DIR} /home /var/backups/opanel-ent /etc/nginx/conf.d /etc/nginx/opanel-ent/custom /tmp /var/lib/opanel-ent
SERVICE
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
ReadWritePaths=/home /var/backups/opanel-ent /etc/nginx/conf.d /etc/nginx/opanel-ent/custom /tmp /var/lib/opanel-ent ${APP_DIR}
PrivateTmp=true

[Install]
WantedBy=multi-user.target
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
  systemctl daemon-reload
  for default_port in 80 443 465 587 "${panel_port}"; do
    iptables_panel_allow_port "$default_port"
  done
  install -d -o root -g root -m 0755 /etc/iptables
  iptables-save >/etc/iptables/rules.v4 2>/dev/null || true
  rm -f /etc/nginx/sites-enabled/default /etc/nginx/conf.d/default.conf 2>/dev/null || true
  rm -f /etc/nginx/sites-enabled/opanel-ent.conf /etc/nginx/sites-available/opanel-ent.conf 2>/dev/null || true
  # Refresh tools vhost (phpMyAdmin) via OLS helper â€” handles phpmyadmin
  # signon config, phpIniOverride, SSL, and OLS main config sync.
  # Must invoke as 'opanel-ent' user (helper enforces caller identity).
  sudo -u opanel-ent env HOME="$APP_DIR" sudo -n /usr/local/sbin/opanel-ent-helper refresh-tools 2>/dev/null || write_tools_nginx_config
}

panel_healthcheck() {
  local port url
  port="$(env_get PANEL_PORT)"
  port="${port:-2222}"
  url="http://127.0.0.1:${port}/api/health"
  if [[ "$(env_get PANEL_URL)" == https://* ]]; then
    url="https://127.0.0.1:${port}/api/health"
  fi
  curl -kfsS --connect-timeout 2 --max-time 5 "$url" >/dev/null 2>&1
}

refresh_opanel_mariadb_grants() {
  local defaults_file="$APP_DIR/.my.cnf"
  local mysql_bin
  mysql_bin="$(command -v mariadb || command -v mysql || true)"
  [[ -n "$mysql_bin" ]] || return 0
  local password
  password=""
  if [[ -f "$defaults_file" ]]; then
    password="$(awk -F= '
      /^\[client\]/ { in_client=1; next }
      /^\[/ { in_client=0 }
      in_client && $1 == "password" {
        value=$0; sub(/^[^=]*=/, "", value); gsub(/^"|"$/, "", value); print value; exit
      }
    ' "$defaults_file")"
  fi
  if [[ -z "$password" ]]; then
    password="$(openssl rand -base64 32 | tr -d '/+=' | cut -c1-32)"
  fi
  "$mysql_bin" <<SQL
CREATE USER IF NOT EXISTS 'opanel-ent'@'localhost' IDENTIFIED BY '${password}';
ALTER USER 'opanel-ent'@'localhost' IDENTIFIED BY '${password}';
GRANT ALL PRIVILEGES ON *.* TO 'opanel-ent'@'localhost' WITH GRANT OPTION;
FLUSH PRIVILEGES;
SQL
  cat >"$defaults_file" <<MYCNF
[client]
user=opanel-ent
password="${password}"
host=localhost

[mysqldump]
user=opanel-ent
password="${password}"
host=localhost
MYCNF
  chown opanel-ent:opanel-ent "$defaults_file"
  chmod 0600 "$defaults_file"
}

ensure_panel_runtime_ownership() {
  id -u opanel-ent >/dev/null 2>&1 || return 0
  [[ -d "$APP_DIR/backend" ]] && chown -R opanel-ent:opanel-ent "$APP_DIR/backend" 2>/dev/null || true
  [[ -d "$APP_DIR/frontend" ]] && chown -R opanel-ent:opanel-ent "$APP_DIR/frontend" 2>/dev/null || true
  [[ -f "$APP_DIR/.my.cnf" ]] && chown opanel-ent:opanel-ent "$APP_DIR/.my.cnf" 2>/dev/null || true
  [[ -f "$APP_DIR/.my.cnf" ]] && chmod 0600 "$APP_DIR/.my.cnf" 2>/dev/null || true
  [[ -d /var/lib/opanel-ent ]] && chown opanel-ent:opanel-ent /var/lib/opanel-ent 2>/dev/null || true
  [[ -d /var/lib/opanel-ent/firewall ]] && chown -R opanel-ent:opanel-ent /var/lib/opanel-ent/firewall 2>/dev/null || true
  [[ -d /var/lib/opanel-ent/assets ]] && chown -R opanel-ent:opanel-ent /var/lib/opanel-ent/assets 2>/dev/null || true
  [[ -f "$APP_DIR/backend/.env" ]] && chmod 0640 "$APP_DIR/backend/.env"
}

# --- Snapshot the SQLite DB before doing anything ---------------------------
backup_db() {
  local db_path="$APP_DIR/backend/opanel-ent.db"
  if [[ ! -f "$db_path" ]]; then
    return 0
  fi
  local snap_dir="${BACKUP_ROOT:-/var/backups/opanel-ent}/db-snapshots"
  install -d -m 0750 "$snap_dir"
  if id -u opanel-ent >/dev/null 2>&1; then
    chown opanel-ent:opanel-ent "$snap_dir" 2>/dev/null || true
  fi
  local stamp
  stamp=$(date -u +%Y%m%d-%H%M%S)
  local snap="$snap_dir/opanel-ent-$stamp.db"
  if command -v sqlite3 >/dev/null 2>&1; then
    sqlite3 "$db_path" ".backup '$snap'"
  else
    cp -a "$db_path" "$snap"
  fi
  echo "DB snapshot saved: $snap"
  # Keep the 10 most recent snapshots.
  ls -1t "$snap_dir"/opanel-ent-*.db 2>/dev/null | tail -n +11 | xargs -r rm -f
}

log "Backing up SQLite DB before update"
backup_db
write_update_state "checking" "" "Checking for opanel-ent releases"
update_progress 5 "checking" "Backing up SQLite DB before update"

# --- Fetch source -----------------------------------------------------------
if [[ "$SKIP_PULL" == "true" ]]; then
  if [[ "$(readlink -f "$SOURCE_DIR")" == "$(readlink -f "$APP_DIR")" ]]; then
    fail "SOURCE_DIR ($SOURCE_DIR) and APP_DIR ($APP_DIR) must be different."
  fi
  UPDATE_REF="local:${SOURCE_DIR}"
  write_update_state "updating" "$UPDATE_REF" "Syncing opanel-ent from ${SOURCE_DIR}"
else
  case "$UPDATE_CHANNEL" in
    release)
      log "Checking latest release from ${REPO_URL}"
      RELEASE_TAG="$(latest_release_tag)"
      [[ -n "$RELEASE_TAG" ]] || fail "No release tags found matching $RELEASE_PATTERN"
      UPDATE_REF="$RELEASE_TAG"
      write_update_state "updating" "$UPDATE_REF" "Updating opanel-ent from ${UPDATE_REF}"
      download_release_source "$RELEASE_TAG"
      echo "Release: ${RELEASE_TAG}"
      ;;
    tag)
      [[ -n "$RELEASE_TAG" ]] || fail "--tag requires a release tag"
      UPDATE_REF="$RELEASE_TAG"
      write_update_state "updating" "$UPDATE_REF" "Updating opanel-ent from ${UPDATE_REF}"
      update_progress 15 "fetching" "Downloading release ${RELEASE_TAG}"
      download_release_source "$RELEASE_TAG"
      echo "Release: ${RELEASE_TAG}"
      ;;
    branch)
      if [[ "$(readlink -f "$SOURCE_DIR")" == "$(readlink -f "$APP_DIR")" ]]; then
        fail "SOURCE_DIR ($SOURCE_DIR) and APP_DIR ($APP_DIR) must be different."
      fi
      if [[ ! -d "$SOURCE_DIR/.git" ]]; then
        if [[ -e "$SOURCE_DIR" && -n "$(ls -A "$SOURCE_DIR" 2>/dev/null)" ]]; then
          source_backup="${SOURCE_DIR}.release-archive-$(date -u +%Y%m%d-%H%M%S)"
          log "Archiving non-git release source to ${source_backup}"
          cd /
          mv "$SOURCE_DIR" "$source_backup"
        fi
        log "Cloning ${REPO_URL} to ${SOURCE_DIR} with remote ${GIT_REMOTE}"
        git clone -o "$GIT_REMOTE" "$REPO_URL" "$SOURCE_DIR"
      fi
      cd "$SOURCE_DIR"
      ensure_git_remote
      remote_branch="${GIT_REMOTE}/${BRANCH}"
      log "Pulling latest from ${remote_branch}"
      update_progress 15 "fetching" "Pulling latest from ${remote_branch}"
      git fetch --prune "$GIT_REMOTE" "+refs/heads/${BRANCH}:refs/remotes/${GIT_REMOTE}/${BRANCH}" --tags
      UPDATE_REF="$remote_branch"
      write_update_state "updating" "$UPDATE_REF" "Updating opanel-ent from ${UPDATE_REF}"
      reset_worktree_to_ref "$remote_branch" "$BRANCH"
      echo "HEAD: $(git rev-parse --short HEAD) - $(git log -1 --pretty=%s)"
      ;;
    *)
      fail "Unsupported UPDATE_CHANNEL: $UPDATE_CHANNEL"
      ;;
  esac
fi

# --- Validate ---------------------------------------------------------------
[[ -d "$SOURCE_DIR/backend"  ]] || fail "Missing $SOURCE_DIR/backend"
[[ -d "$SOURCE_DIR/frontend" ]] || fail "Missing $SOURCE_DIR/frontend"

# --- Sync code into APP_DIR -------------------------------------------------
log "Syncing source to $APP_DIR"
mkdir -p "$APP_DIR"

if command -v rsync >/dev/null 2>&1; then
  # --filter='protect ...' keeps the destination file even when --delete
  # would otherwise remove it because the source side doesn't have it. We use
  # this for runtime artefacts that the installer creates: .env, .venv,
  # OPanel Enterprise.db, .my.cnf.
  rsync -a --delete \
    --filter='protect /.env' \
    --filter='protect /.venv' \
    --filter='protect /.venv/**' \
    --filter='protect /opanel-ent.db' \
    --filter='protect /.my.cnf' \
    --exclude '__pycache__/' \
    --exclude '*.pyc' \
    "$SOURCE_DIR/backend/" "$APP_DIR/backend/"
  rsync -a --delete \
    --filter='protect /node_modules' \
    --filter='protect /node_modules/**' \
    --filter='protect /dist' \
    --filter='protect /dist/**' \
    --filter='protect /.vite' \
    --filter='protect /.vite/**' \
    "$SOURCE_DIR/frontend/" "$APP_DIR/frontend/"
else
  cp -r "$SOURCE_DIR/backend/."  "$APP_DIR/backend/"
  cp -r "$SOURCE_DIR/frontend/." "$APP_DIR/frontend/"
fi
if [[ -f "$SOURCE_DIR/VERSION" ]]; then
  install -m 0644 "$SOURCE_DIR/VERSION" "$APP_DIR/VERSION"
fi
ensure_panel_runtime_ownership

# Defensive: if .env still doesn't exist (e.g. fresh deploy syncing on top of
# nothing), leave a clear error message.
if [[ ! -f "$APP_DIR/backend/.env" ]]; then
  fail "$APP_DIR/backend/.env is missing. Run installer/install.sh first or restore .env from backup."
fi
log "Installing direct panel runtime"
update_progress 25 "syncing" "Syncing source into ${APP_DIR}"
install_panel_runtime

# Ensure DA backup import directory exists
if id -u opanel-ent >/dev/null 2>&1; then
  log "Ensuring DirectAdmin backup import directory"
  install -d -o opanel-ent -g opanel-ent -m 0750 /home/admin/opanel-ent-backups/da
fi
log "Configuring legacy FastCGI cache compatibility"
configure_fastcgi_cache
ensure_terminal_tools
ensure_dns_ssl_plugin
venv_needs_recreate=false
if [[ ! -x "$APP_DIR/backend/.venv/bin/uvicorn" ]]; then
  venv_needs_recreate=true
elif ! head -n1 "$APP_DIR/backend/.venv/bin/uvicorn" 2>/dev/null | grep -Fq "$APP_DIR/backend/.venv"; then
  venv_needs_recreate=true
fi
if [[ "$venv_needs_recreate" == "true" ]]; then
  log "Recreating Python virtualenv (missing or stale path)"
  rm -rf "$APP_DIR/backend/.venv"
  python3 -m venv "$APP_DIR/backend/.venv"
fi

# --- Refresh helper + sudoers (idempotent) ---------------------------------
if [[ -f "$SOURCE_DIR/installer/files/opanel-ent-helper.sh" ]]; then
  log "Refreshing /usr/local/sbin/opanel-ent-helper and /etc/sudoers.d/opanel-ent"
  update_progress 40 "runtime" "Refreshing panel helper and runtime"
  if id -u opanel-ent >/dev/null 2>&1; then
    install -m 0750 -o root -g opanel-ent "$SOURCE_DIR/installer/files/opanel-ent-helper.sh" /usr/local/sbin/opanel-ent-helper
    sed -i "s#^APP_DIR=\"/opt/opanel-ent\"#APP_DIR=\"${APP_DIR}\"#" /usr/local/sbin/opanel-ent-helper
    install -m 0440 -o root -g root  "$SOURCE_DIR/installer/files/opanel-ent-sudoers"   /etc/sudoers.d/opanel-ent
    sed -i 's/\r$//' /etc/sudoers.d/opanel-ent
    visudo -c -f /etc/sudoers.d/opanel-ent >/dev/null
    sudo -u opanel-ent env HOME="$APP_DIR" sudo -n /usr/local/sbin/opanel-ent-helper wp --info >/dev/null
    sudo -u opanel-ent env HOME="$APP_DIR" sudo -n /usr/local/sbin/opanel-ent-helper php-fpm-retune >/dev/null || \
      echo "  (warning: could not retune existing PHP-FPM pools; site refresh will retry later)"
    sudo -u opanel-ent env HOME="$APP_DIR" sudo -n /usr/local/sbin/opanel-ent-helper mariadb-retune >/dev/null || \
      echo "  (warning: could not retune MariaDB; update will continue with existing settings)"
    sudo -u opanel-ent env HOME="$APP_DIR" sudo -n /usr/local/sbin/opanel-ent-helper refresh-tools >/dev/null || \
      echo "  (warning: could not refresh phpMyAdmin tools vhost; update will retry later)"
    # Populates /etc/opanel-ent/certs: a self-signed default plus one directory per
    # Let's Encrypt domain, so the panel can serve HTTPS immediately and pick a
    # matching certificate for whichever hostname a browser asks for.
    sudo -u opanel-ent env HOME="$APP_DIR" sudo -n /usr/local/sbin/opanel-ent-helper panel-cert-sync >/dev/null || \
      echo "  (warning: could not sync the panel certificate store; panel may stay on HTTP)"
  else
    echo "  (opanel-ent user not found; skipping helper refresh - run install.sh first)"
  fi
fi

# --- Panel serves HTTPS by default -----------------------------------------
# There is always a certificate now (self-signed at worst), so an http:// panel
# URL only means "configured before this existed". Flip it; app.server still
# falls back to plain HTTP by itself if no certificate can be loaded at all.
panel_url_now="$(env_get PANEL_URL)"
if [[ "$panel_url_now" == http://* ]]; then
  panel_url_https="https://${panel_url_now#http://}"
  env_set PANEL_URL "$panel_url_https"
  env_set ALLOWED_ORIGINS "$panel_url_https"
  log "Panel now serves HTTPS: ${panel_url_https}"
  echo "  (the old http:// address will no longer connect -- use https://)"
fi

# --- Repair OPcache timestamp validation ------------------------------------
# Installs tuned before this fix shipped opcache.validate_timestamps = 0, which
# leaves edited files invisible until every LSAPI worker for the pool exits.
# Only that directive is touched; a full re-tune would discard whatever else
# the admin has changed. If the directive is missing entirely it is added,
# rather than left to PHP's built-in default.
opcache_repaired=0
for _ini in /usr/local/lsws/lsphp*/etc/php/*/mods-available/99-opanel-ent.ini; do
  [[ -f "$_ini" ]] || continue
  _before="$(md5sum "$_ini" | cut -d" " -f1)"
  if grep -qE '^[[:space:]]*opcache\.validate_timestamps[[:space:]]*=[[:space:]]*0[[:space:]]*$' "$_ini"; then
    sed -i -E 's/^[[:space:]]*opcache\.validate_timestamps[[:space:]]*=[[:space:]]*0[[:space:]]*$/opcache.validate_timestamps = 1/' "$_ini"
  elif grep -qE '^[[:space:]]*opcache\.enable[[:space:]]*=' "$_ini" && ! grep -qE '^[[:space:]]*opcache\.validate_timestamps[[:space:]]*=' "$_ini"; then
    sed -i -E '/^[[:space:]]*opcache\.enable[[:space:]]*=/a opcache.validate_timestamps = 1' "$_ini"
  fi
  _after="$(md5sum "$_ini" | cut -d" " -f1)"
  [[ "$_before" != "$_after" ]] && opcache_repaired=$((opcache_repaired + 1))
done
if [[ "$opcache_repaired" -gt 0 ]]; then
  log "Repaired OPcache validate_timestamps in ${opcache_repaired} php.ini file(s)"
  systemctl restart lshttpd.service 2>/dev/null || /usr/local/lsws/bin/lswsctrl restart 2>/dev/null || true
fi

log "Removing legacy UFW firewall package"
remove_ufw_legacy
rm -f /usr/local/sbin/opanel-ent-rescue-ufw-blocklist

if [[ -f "$SOURCE_DIR/change_IP.sh" ]]; then
  log "Refreshing panel IP change command"
  install -m 0755 -o root -g root "$SOURCE_DIR/change_IP.sh" /usr/local/sbin/opanel-ent-change-ip
fi

if [[ -f "$SOURCE_DIR/installer/update.sh" ]]; then
  log "Refreshing panel update command"
  install -m 0755 -o root -g root "$SOURCE_DIR/installer/update.sh" /usr/local/sbin/opanel-ent-update
fi

if [[ -f "$SOURCE_DIR/installer/files/opanel-ent-ctl" ]]; then
  log "Refreshing SSH menu command: opanel-ent"
  install -m 0755 -o root -g root "$SOURCE_DIR/installer/files/opanel-ent-ctl" /usr/local/sbin/opanel-ent
  ln -sfn /usr/local/sbin/opanel-ent /usr/local/sbin/opanel-ent-ctl
  sed -i "s#APP_DIR=\"\${APP_DIR:-/opt/opanel-ent}\"#APP_DIR=\"\${APP_DIR:-${APP_DIR}}\"#" /usr/local/sbin/opanel-ent /usr/local/sbin/opanel-ent-ctl 2>/dev/null || true
fi

log "Ensuring OpenLiteSpeed ModSecurity WAF engine is installed"
if id -u opanel-ent >/dev/null 2>&1; then
  sudo -u opanel-ent env HOME="$APP_DIR" sudo -n /usr/local/sbin/opanel-ent-helper waf-install || \
    echo "WARNING: WAF engine installation failed; continuing without ModSecurity."
  sudo -u opanel-ent env HOME="$APP_DIR" sudo -n /usr/local/sbin/opanel-ent-helper certbot-auto-renew-install >/dev/null 2>&1 || true
  sudo -u opanel-ent env HOME="$APP_DIR" sudo -n /usr/local/sbin/opanel-ent-helper blocklist-timer-install >/dev/null 2>&1 || true
  sudo -u opanel-ent env HOME="$APP_DIR" sudo -n /usr/local/sbin/opanel-ent-helper blocklist-run >/dev/null 2>&1 || true
  sudo -u opanel-ent env HOME="$APP_DIR" sudo -n /usr/local/sbin/opanel-ent-helper ols-sync-main >/dev/null 2>&1 || true
  for php_ver in 8.3 8.4; do
    if [[ -f "/usr/local/lsws/lsphp${php_ver//./}/etc/php.d/99-opanel-ent.ini" ]]; then
      sudo -u opanel-ent env HOME="$APP_DIR" sudo -n /usr/local/sbin/opanel-ent-helper php-config-write "$php_ver" \
        <"/usr/local/lsws/lsphp${php_ver//./}/etc/php.d/99-opanel-ent.ini" >/dev/null || true
    fi
  done
else
  echo "  (opanel-ent user not found; skipping WAF install - run install.sh first)"
fi

# --- Restore ownership so opanel-ent user can read/write the deploy ------------
if id -u opanel-ent >/dev/null 2>&1; then
  ensure_panel_runtime_ownership
fi

# --- Backend ---------------------------------------------------------------
log "Updating backend dependencies"
update_progress 55 "backend" "Updating backend dependencies"
cd "$APP_DIR/backend"
if [[ ! -d .venv ]]; then
  python3 -m venv .venv
fi
# shellcheck disable=SC1091
source .venv/bin/activate
pip install --upgrade pip
pip install -r requirements.txt

log "Refreshing MariaDB grants"
refresh_opanel_mariadb_grants

# Linux Malware Detect is layered on ClamAV, but only for boxes that already run
# ClamAV -- a fresh install without malware scanning enabled is left untouched.
if dpkg -s clamav-daemon >/dev/null 2>&1 \
   && grep -qE '"malware_scan_enabled"[[:space:]]*:[[:space:]]*true' /var/lib/opanel-ent/panel-settings.json 2>/dev/null \
   && ! command -v maldet >/dev/null 2>&1; then
  log "Adding Linux Malware Detect to the malware scanner"
  # opanel-ent-helper only accepts calls made *as the opanel-ent user* via sudo -- the
  # same wrapper every other helper call in this script uses.
  sudo -u opanel-ent env HOME="$APP_DIR" sudo -n /usr/local/sbin/opanel-ent-helper maldet-ensure >/dev/null 2>&1 \
    || echo "  (Linux Malware Detect install skipped; ClamAV scanning still works)"
fi

# The real-time monitor unit is only written when someone enables the feature,
# so boxes that turned it on under an older build kept a Type=oneshot unit --
# which systemd either killed at the start timeout or left in "activating"
# forever. Either way the watcher was not running while the panel said it was.
# Rewrite and restart it here for boxes that have the feature enabled.
if [[ -f /etc/systemd/system/opanel-ent-maldet-monitor.service ]]    && ! grep -q '^ExecStartPre=' /etc/systemd/system/opanel-ent-maldet-monitor.service    && grep -qE '"malware_realtime_enabled"[[:space:]]*:[[:space:]]*true' /var/lib/opanel-ent/panel-settings.json 2>/dev/null; then
  log "Repairing the real-time malware monitor service"
  systemctl stop opanel-ent-maldet-monitor.service >/dev/null 2>&1 || true
  systemctl reset-failed opanel-ent-maldet-monitor.service >/dev/null 2>&1 || true
  sudo -u opanel-ent env HOME="$APP_DIR" sudo -n /usr/local/sbin/opanel-ent-helper maldet-monitor enable >/dev/null 2>&1     || echo "  (could not restart the real-time monitor; enable it again from the panel)"
fi

# Chain order fix: OPANEL_ENT_INPUT accepts the default ports from any source and
# used to be consulted before OPANEL_ENT_USER, so an admin's "block this IP" never
# ran for 22/80/443/the panel port. Existing boxes keep the old jump order --
# the -C check inside the helper finds it and leaves it -- so re-run the enable
# path here, which drops and re-adds the jumps in the right order.
if command -v iptables >/dev/null 2>&1    && iptables -L INPUT -n --line-numbers 2>/dev/null       | awk '/OPANEL_ENT_INPUT/{i=NR} /OPANEL_ENT_USER/{u=NR} END{exit !(i && u && i < u)}'; then
  log "Reordering firewall chains so IP blocks take effect"
  sudo -u opanel-ent env HOME="$APP_DIR" sudo -n /usr/local/sbin/opanel-ent-helper iptables-enable >/dev/null 2>&1     || echo "  (could not reorder firewall chains; run Reload on the Firewall page)"
fi

# Deleting a website removed its vhost but left its WAF rules file behind, so
# boxes accumulate one orphan per site ever deleted. The helper drops the file
# with the vhost from now on; clear out what earlier deletes left.
WAF_SITES_DIR="/usr/local/lsws/conf/opanel-ent/waf/sites"
OLS_VHOSTS_DIR_CLEAN="/usr/local/lsws/conf/opanel-ent/vhosts"
if [[ -d "$WAF_SITES_DIR" && -d "$OLS_VHOSTS_DIR_CLEAN" ]]; then
  orphans=0
  for rules_file in "$WAF_SITES_DIR"/*.conf; do
    [[ -f "$rules_file" ]] || continue
    rules_domain="$(basename "$rules_file" .conf)"
    # Only ever remove a file whose vhost is provably gone.
    if [[ -n "$rules_domain" && ! -d "$OLS_VHOSTS_DIR_CLEAN/$rules_domain" ]]; then
      rm -f "$rules_file"
      orphans=$((orphans + 1))
    fi
  done
  [[ "$orphans" -gt 0 ]] && log "Removed $orphans orphaned WAF rule file(s) from deleted sites"
fi

update_progress 62 "backend" "Checking the database schema"
MIGRATE_RUNNER="python"
id -u opanel-ent >/dev/null 2>&1 && MIGRATE_RUNNER="sudo -u opanel-ent $APP_DIR/backend/.venv/bin/python"
# Almost every update ships no new migration. `alembic upgrade head` still opens
# the DB, reads alembic_version and walks the script tree every time -- skip that
# when the DB is already stamped at the head revision.
SCHEMA_AT_HEAD="$($MIGRATE_RUNNER - <<'PY' 2>/dev/null || echo no
try:
    from alembic.script import ScriptDirectory
    from alembic.runtime.migration import MigrationContext
    from app.core.database import engine, _alembic_config
    heads = set(ScriptDirectory.from_config(_alembic_config()).get_heads())
    with engine.connect() as conn:
        current = set(MigrationContext.configure(conn).get_current_heads())
    print("yes" if current and current == heads else "no")
except Exception:
    print("no")
PY
)"
if [[ "$(printf '%s' "$SCHEMA_AT_HEAD" | tr -cd 'a-z' | tail -c3)" == "yes" ]]; then
  log "Database schema already at head -- skipping migrations"
  update_progress 65 "backend" "Database schema up to date"
else
  log "Running database migrations"
  update_progress 65 "backend" "Running database migrations"
  # Run migrations as the opanel-ent user so the SQLite file ownership stays correct.
  $MIGRATE_RUNNER -c "from app.core.database import run_migrations; run_migrations()"
fi
systemctl enable --now opanel-ent-backup-scheduler.timer >/dev/null 2>&1 || true
systemctl enable --now opanel-ent-malware-scheduler.timer >/dev/null 2>&1 || true

log "Refreshing managed site config"
# The per-site refresh has two costs, gated separately:
#  1. Rewriting every vhost + WAF file (cheap: render + `cmp -s` skip, one
#     deferred `ols-sync-main` at the end). Runs whenever the code that shapes a
#     site's generated config changes -- keyed on SITE_FP_FILE.
#  2. Re-hardening every site's file TREE (recursive chown + setfacl + 4 `find`
#     walks per site -- this is what makes an update take ~50 min on an 80-site
#     box). Only needed when the ownership/permission scheme itself changes:
#     gated on SITE_HARDEN_VERSION, or forced by --refresh-sites / a provisioning
#     site. A plain backend or malware-scanner change no longer triggers it.
SITE_FP_FILE="/var/lib/opanel-ent/site-refresh.fingerprint"
SITE_HARDEN_VERSION=1
SITE_HARDEN_MARKER="/var/lib/opanel-ent/site-harden.version"
# harden_existing_panel_users v2 applies the identical tree scheme, so a box
# already stamped there needs no first-time re-harden from this loop.
if [[ ! -f "$SITE_HARDEN_MARKER" && "$(cat /var/lib/opanel-ent/harden-sites.version 2>/dev/null || true)" == "2" ]]; then
  install -d -o opanel-ent -g opanel-ent -m 0750 /var/lib/opanel-ent 2>/dev/null || true
  printf '%s\n' "$SITE_HARDEN_VERSION" > "$SITE_HARDEN_MARKER" 2>/dev/null || true
fi
site_refresh_fingerprint() {
  sha256sum \
    /usr/local/sbin/opanel-ent-helper \
    "$APP_DIR/backend/app/services/openlitespeed.py" \
    "$APP_DIR/backend/app/services/waf.py" \
    "$APP_DIR/backend/app/services/site_users.py" \
    "$APP_DIR/backend/app/api/websites.py" \
    2>/dev/null | sha256sum | awk '{print $1}'
}
_site_refresh_progress() {
  # Translate `opanel-ent-site-progress <cur> <total>` markers into progress ticks;
  # pass every other line through to the update log.
  local line cur tot
  while IFS= read -r line; do
    if [[ "$line" == "opanel-ent-site-progress "* ]]; then
      cur="${line#opanel-ent-site-progress }"; tot="${cur##* }"; cur="${cur%% *}"
      if [[ "$cur" =~ ^[0-9]+$ && "$tot" =~ ^[0-9]+$ && "$tot" -gt 0 ]]; then
        update_progress "$(( 66 + cur * 12 / tot ))" "backend" "Refreshing sites (${cur}/${tot})"
      fi
    elif [[ -n "$line" ]]; then
      printf '%s\n' "$line"
    fi
  done
}
if id -u opanel-ent >/dev/null 2>&1; then
  NEW_SITE_FP="$(site_refresh_fingerprint)"
  OLD_SITE_FP="$(cat "$SITE_FP_FILE" 2>/dev/null || true)"
  # Cheap DB-only normalisation always runs; it also tells us whether any site
  # still needs provisioning (forces the full refresh regardless of fingerprint).
  NEEDS_REFRESH="$(sudo -u opanel-ent env HOME="$APP_DIR" "$APP_DIR/backend/.venv/bin/python" - <<'PY' || echo 1
import sys
from app.core.database import SessionLocal
from app.models.entities import Website
with SessionLocal() as db:
    n = db.query(Website).filter(Website.nginx_config_mode != "managed").update(
        {"nginx_config_mode": "managed"}, synchronize_session=False)
    if n:
        db.commit()
        print(f"normalised nginx_config_mode for {n} site(s)", file=sys.stderr)
    pending = db.query(Website).filter(Website.status == "provisioning").count()
print(1 if pending else 0)
PY
)"
  NEEDS_REFRESH="$(printf '%s' "$NEEDS_REFRESH" | tail -n1 | tr -cd '01')"
  SITE_REHARDEN=0
  if [[ "$FORCE_SITE_REFRESH" == "true" \
        || "${NEEDS_REFRESH:-1}" == "1" \
        || "$(cat "$SITE_HARDEN_MARKER" 2>/dev/null || true)" != "$SITE_HARDEN_VERSION" ]]; then
    SITE_REHARDEN=1
  fi
  if [[ "$FORCE_SITE_REFRESH" != "true" && "$SITE_REHARDEN" == "0" \
        && -n "$NEW_SITE_FP" && "$NEW_SITE_FP" == "$OLD_SITE_FP" ]]; then
    log "Site config + permissions unchanged since last update -- skipping the per-site refresh (--refresh-sites to override)"
  else
    if [[ "$SITE_REHARDEN" == "1" ]]; then
      log "Re-applying per-site file permissions + rewriting vhost/WAF config"
    else
      log "Rewriting per-site vhost + WAF config (file permissions unchanged)"
    fi
    sudo -u opanel-ent env HOME="$APP_DIR" opanel_ent_USE_HELPER=true SITE_REHARDEN="$SITE_REHARDEN" \
      "$APP_DIR/backend/.venv/bin/python" - <<'PY' | _site_refresh_progress
import os
from app.core.database import SessionLocal
from app.models.entities import Website
from app.services import site_users, waf
# _rewrite_website_vhost resolves SSL from ssl_mode (letsencrypt / reuse / manual)
# and carries aliases, redirects and WAF settings. Calling
# openlitespeed.rewrite_vhost by hand here dropped ssl_mode="reuse" -- it fell
# back to /etc/letsencrypt/live/<own-domain>/ (which does not exist) and broke
# HTTPS for every reuse-mode site on the next update.
from app.api.websites import _rewrite_website_vhost

reharden = os.environ.get("SITE_REHARDEN") == "1"
with SessionLocal() as db:
    websites = db.query(Website).all()
    total = len(websites)
    for i, website in enumerate(websites, 1):
        try:
            if reharden and website.linux_user:
                runtime_php_version = website.php_version if (website.app_type or "wordpress") in {"wordpress", "php"} else None
                # The recursive chown/chmod tree fix. Skipped unless the scheme
                # changed (SITE_HARDEN_VERSION) or --refresh-sites was passed.
                site_users.ensure_site_runtime(website.domain, website.root_path, runtime_php_version, website.linux_user)
                site_users.ensure_document_root(
                    website.root_path,
                    getattr(website, "document_root", "public_html") or "public_html",
                    website.linux_user,
                )
            result = waf.sync_website_rules(website, defer_reload=True)
            if result.returncode != 0:
                print(f"WARNING: could not refresh WAF rules for {website.domain}: {result.stderr or result.stdout}")
            _rewrite_website_vhost(website, defer_reload=True)
        except Exception as exc:
            print(f"WARNING: could not refresh {website.domain}: {exc}")
        if i % 5 == 0 or i == total:
            print(f"opanel-ent-site-progress {i} {total}", flush=True)
PY
    printf '%s\n' "$NEW_SITE_FP" > "$SITE_FP_FILE" 2>/dev/null || true
    [[ "$SITE_REHARDEN" == "1" ]] && printf '%s\n' "$SITE_HARDEN_VERSION" > "$SITE_HARDEN_MARKER" 2>/dev/null || true
  fi
fi

log "Compiling backend modules"
python -m py_compile \
  app/main.py \
  app/api/auth.py \
  app/api/users.py \
  app/api/websites.py \
  app/api/databases.py \
  app/api/maintenance.py \
  app/api/firewall.py \
  app/api/services.py \
  app/api/updates.py \
  app/api/waf.py \
  app/api/panel_settings.py \
  app/api/terminal.py \
  app/services/firewall.py \
  app/services/nginx.py \
  app/services/network.py \
  app/services/panel_urls.py \
  app/services/panel_settings.py \
  app/services/updates.py \
  app/services/waf.py \
  app/services/mariadb.py \
  app/services/wordpress.py \
  app/services/file_manager.py \
  app/services/backup.py \
  app/services/backup_scheduler.py \
  app/services/malware_scheduler.py \
  app/services/da_import.py \
  app/services/storage_quota.py \
  app/services/site_users.py \
  app/services/cron.py \
  app/services/php.py \
  app/schemas/schemas.py \
  app/seed.py
deactivate

log "Restarting opanel-ent-api"
mkdir -p /etc/systemd/system/opanel-ent-api.service.d
cat >/etc/systemd/system/opanel-ent-api.service.d/10-opanel-ent-helper.conf <<'SERVICE'
[Service]
NoNewPrivileges=false
ProtectSystem=false
RestrictSUIDSGID=false
CapabilityBoundingSet=~
SystemCallFilter=
RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6 AF_NETLINK
SERVICE
systemctl daemon-reload
systemctl restart opanel-ent-api

# --- Frontend --------------------------------------------------------------
log "Building frontend (clean rebuild)"
update_progress 80 "frontend" "Building frontend"
cd "$APP_DIR/frontend"

# Check Node.js availability
if ! command -v node >/dev/null 2>&1; then
  log "WARNING: Node.js not found, installing..."
  DEBIAN_FRONTEND=noninteractive apt-get update --allow-releaseinfo-change
  DEBIAN_FRONTEND=noninteractive apt-get install -y nodejs npm
fi
echo "Node version: $(node --version)"
echo "npm version: $(npm --version)"

rm -rf dist .vite node_modules/.vite

if [[ ! -d node_modules ]] || [[ package.json -nt node_modules ]]; then
  log "Installing npm dependencies..."
  rm -rf node_modules package-lock.json
  npm install
fi
log "Building frontend with VITE_API_URL=/api..."
VITE_API_URL=/api npm run build 2>&1 || { log "BUILD FAILED"; exit 1; }

[[ -f dist/index.html ]] || fail "Frontend build failed: dist/index.html missing"
HASHED=$(grep -oE 'index-[a-zA-Z0-9_-]+\.js' dist/index.html | head -n1 || true)
echo "Built bundle: ${HASHED:-unknown}"

# Make sure nginx (www-data) can read the freshly built bundle.
chmod o+rX "$APP_DIR" "$APP_DIR/frontend" 2>/dev/null || true
chmod -R o+rX "$APP_DIR/frontend/dist"

# The API mounts /assets only when the built directory exists at process start.
# Restart once more after the clean frontend rebuild so newly hashed JS/CSS
# files are served as static assets instead of falling through to index.html.
log "Restarting opanel-ent-api after frontend build"
systemctl restart opanel-ent-api

# --- Reload OpenLiteSpeed --------------------------------------------------
log "Reloading OpenLiteSpeed"
update_progress 92 "restarting" "Restarting services and reloading OpenLiteSpeed"
migrate_nginx_wordpress_csp_worker_src
migrate_drop_iframe_blocking

# --- Serve IPv6 when the box has it ----------------------------------------
# Only fills in a bind host that was never set: an admin who turned IPv6 off in
# the panel keeps it off.
if grep -q '^PANEL_BIND_HOST=::' "$APP_DIR/backend/.env" 2>/dev/null && ! host_has_global_ipv6; then
  # The address family went away (kernel flag, rebuilt VPS) but the bind did
  # not. Left alone the API cannot listen at all, and the panel is what the
  # admin would use to undo it.
  sed -i 's|^PANEL_BIND_HOST=.*|PANEL_BIND_HOST=0.0.0.0|' "$APP_DIR/backend/.env"
  log "IPv6 is gone from this host; panel bind reset to 0.0.0.0"
fi

if ! grep -q '^PANEL_BIND_HOST=' "$APP_DIR/backend/.env" 2>/dev/null; then
  if host_has_global_ipv6; then
    printf 'PANEL_BIND_HOST=%s\n' "::" >>"$APP_DIR/backend/.env"
    log "Detected global IPv6; panel will listen on IPv6 as well"
  else
    printf 'PANEL_BIND_HOST=%s\n' "0.0.0.0" >>"$APP_DIR/backend/.env"
  fi
fi

install -d -m 0755 /etc/systemd/system/lshttpd.service.d
printf '%s\n' '[Service]' 'PIDFile=/run/openlitespeed.pid' 'KillMode=mixed' \
  >/etc/systemd/system/lshttpd.service.d/10-opanel-ent.conf
systemctl daemon-reload
if id -u opanel-ent >/dev/null 2>&1; then
  sudo -u opanel-ent env HOME="$APP_DIR" sudo -n /usr/local/sbin/opanel-ent-helper ols-sync-main >/dev/null 2>&1 || true
fi
systemctl restart lshttpd.service || /usr/local/lsws/bin/lswsctrl restart

# --- Health check ----------------------------------------------------------
log "Health check"
update_progress 98 "healthcheck" "Running API health check"
for _ in {1..20}; do
  if panel_healthcheck; then
    echo "API is healthy."
    echo ""
    echo "Update completed."
    echo "If the browser still shows the old UI, hard refresh (Ctrl + Shift + R)."
    write_update_state "completed" "${UPDATE_REF:-}" "Update completed"
    update_progress 100 "completed" "Update completed"
    exit 0
  fi
  sleep 1
done

echo "API did not respond. Check logs:"
echo "  journalctl -u opanel-ent-api -n 100 --no-pager"
write_update_state "failed" "${UPDATE_REF:-}" "API health check failed after update"
update_progress 0 "failed" "API health check failed after update"
exit 1
