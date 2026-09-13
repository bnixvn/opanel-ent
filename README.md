# OPanel Enterprise

> Forked from [OPanel](https://github.com/bnixvn/opanel) at v1.6.0 on 2026-09-13.
> Everything below carries over; the two products are maintained separately from here.

Lightweight hosting management panel for Ubuntu 24.04, powered by **OpenLiteSpeed** and **LSPHP**. OPanel Enterprise helps you run WordPress and PHP websites from a single clean web UI with user ownership, quotas, backups, SSL, services, and firewall tools built in.

## Features

- Dashboard resource monitoring for CPU, RAM, disk, and network throughput
- WordPress one-click installer (default LSPHP 8.4, with 8.3 installed) with WP-CLI
- WordPress and PHP sites with managed OpenLiteSpeed vhost settings
- `.htaccess` fully supported (`allowOverride all` in all vhost templates)
- Panel users map to Linux/SFTP users; website source lives in `/home/<panel-user>/<domain>/public_html`
- Admin quick-login for creating sites as a selected user, plus one-owner assignment per website
- Website count limits and soft storage quotas per end user
- MariaDB database creation and management with phpMyAdmin SSO (60s tokens)
- **MariaDB auto-tuner** — VPS-aware InnoDB, connections, and cache sizing
- **PHP/LSPHP auto-tuner** — OPcache, LSAPI workers, memory limits tuned per VPS
- Let's Encrypt SSL via certbot (webroot mode)
- Native file manager with upload, edit, archive, and extract support
- Backups: archive site files + SQL, scheduled full-user backups, restore, upload, download
- DirectAdmin backup import — upload `.tar.zst`/`.tar.gz` archives and import sites, databases, and configs
- SFTP backup targets for off-server backup copies
- iptables + ipset firewall manager with protected panel/web/mail defaults, blocklists, and user rules
- Update controls for apt-based OS packages and OPanel Enterprise source updates
- OpenLiteSpeed ModSecurity/WAF engine with lightweight WordPress/Laravel/PHP rules, per-site toggles, and HTTP flood protection
- LSPHP config editor per version with auto-tune
- Cron job manager with whitelisted WP-CLI commands
- Role-based access: Admin / End user
- Google Authenticator compatible 2FA

## Tech stack

| Component | Technology |
|-----------|-----------|
| Backend | Python 3.12, FastAPI, SQLAlchemy, SQLite, Pydantic v2, Jinja2 |
| Frontend | React 18, Vite, lucide-react |
| Webserver | [OpenLiteSpeed](https://openlitespeed.org/) |
| PHP | LSPHP 8.4 default + 8.3 installed; 7.4, 8.1, 8.2, and 8.5 can be installed from the panel |
| Database | MariaDB |
| Cache | Redis |
| Firewall | iptables + ipset |
| SSL | Let's Encrypt via certbot (webroot) |
| WAF | OpenLiteSpeed ModSecurity |
| System | systemd, Ubuntu 24.04 LTS |

## Versioning

The `main` branch is the update/install source. Tagged releases use semantic versioning: `major.minor.patch`.

## System requirements

- Ubuntu 24.04 LTS (clean install recommended)
- Root access
- Optional: a domain pointing to the server public IP (for SSL on the panel)
- 1 vCPU / 1 GB RAM minimum, 2 vCPU / 2 GB RAM recommended

## Fresh install

Run as root on a fresh Ubuntu 24.04 server.

### Quick install (recommended)

```bash
bash <(curl -fsSL https://raw.githubusercontent.com/bnixvn/opanel-ent/main/installer/install.sh)
```

### Git clone install

```bash
set -e
apt-get update
apt-get install -y git
OPANEL_ENT_REPO=https://github.com/bnixvn/opanel-ent.git
OPANEL_ENT_REF="${OPANEL_ENT_REF:-main}"
echo "Installing OPanel Enterprise ${OPANEL_ENT_REF}"
rm -rf /tmp/opanel-ent-source
git clone --depth 1 --branch "${OPANEL_ENT_REF}" "${OPANEL_ENT_REPO}" /tmp/opanel-ent-source
cd /tmp/opanel-ent-source
trap '"'"'cd /; rm -rf /tmp/opanel-ent-source'"'"' EXIT
chmod +x installer/install.sh installer/update.sh
bash installer/install.sh
```

To pin a specific tag, set `OPANEL_ENT_REF=v1.0.0` before running the script.

### What the installer does

1. Installs base packages: git, MariaDB, Redis, OpenSSH/SFTP, Node.js 22, certbot, phpMyAdmin, WP-CLI, iptables, ipset
2. Installs **OpenLiteSpeed** + **LSPHP 8.4 and 8.3** from the LiteSpeed repository, with PHP 8.4 as the default CLI/site version
3. Copies source to `/opt/opanel-ent`, builds the frontend, sets up the Python venv
4. Creates the `opanel-ent` service account and the `admin` Linux/SFTP account
5. Creates the systemd service `opanel-ent-api`
6. Configures phpMyAdmin SSO
7. Sets up iptables firewall with `OPANEL_ENT_INPUT`/`OPANEL_ENT_USER`/`OPANEL_ENT_BLOCKLIST` chains
8. Starts the panel on the configured port
9. Optionally issues Let's Encrypt SSL for the panel domain
10. Installs `/usr/local/sbin/opanel-ent-update` for future updates

You will be prompted for:

- Panel hostname (optional; blank uses the server IP)
- Panel port (default `2222`)
- Whether to enable Let's Encrypt SSL for the panel domain
- An email for SSL registration

After install, open the panel URL printed at the end of the installer. The admin password is shown there and saved to `/root/login.txt`; store it in a password manager.

## Directory structure

| Path | Purpose |
|------|---------|
| `/opt/opanel-ent/` | Application source + frontend |
| `/var/backups/opanel-ent/` | Backup archives |
| `/home/admin/opanel-ent-backups/da/` | DirectAdmin backup import staging |
| `/var/lib/opanel-ent/` | Runtime data (firewall rules, etc.) |
| `/usr/local/lsws/conf/opanel-ent/` | OpenLiteSpeed vhost configs, SSL certs, ModSecurity rules |
| `/usr/local/lsws/lsphp83/` | LSPHP 8.3 binaries and config |
| `/usr/local/lsws/lsphp84/` | LSPHP 8.4 binaries and config (default) |
| `/etc/mysql/mariadb.conf.d/99-opanel-ent.cnf` | MariaDB auto-tuned config |
| `/home/<user>/<domain>/public_html` | Website source files |

## Project layout

```
opanel-ent/
├── backend/                    FastAPI application
│   ├── app/
│   │   ├── api/                HTTP routes
│   │   ├── core/               config, db, security, permissions, secrets
│   │   ├── models/             SQLAlchemy entities
│   │   ├── schemas/            Pydantic v2 schemas
│   │   ├── services/           openlitespeed, mariadb, php, wp, firewall, backup, etc.
│   │   ├── templates/
│   │   │   └── openlitespeed/  Jinja2 vhost templates (wordpress, php, static)
│   │   ├── main.py
│   │   └── seed.py             Seeds the first admin user
│   ├── tests/                  pytest smoke tests
│   └── requirements.txt
├── frontend/                   React + Vite SPA
│   └── src/
├── installer/
│   ├── files/                  opanel-ent-helper.sh, sudoers, bpanelctl
│   ├── install.sh              Full first-time install
│   └── update.sh               Pull from GitHub and redeploy
└── README.md
```

## SSH rescue menu

Run as root:

```bash
opanel-ent
```

Use this menu when the web panel is unavailable. It can show the saved login, show rescue status, print recent logs, restart panel services, reopen required firewall ports, reset the panel URL/port, repair panel SSL, fix runtime permissions, change the `admin` password, and update OPanel Enterprise from the `main` branch. Website and user management stays in the web panel.

## Updating

### Quick update (recommended)

```bash
bash <(curl -fsSL https://raw.githubusercontent.com/bnixvn/opanel-ent/main/installer/update.sh)
```

### With `opanel-ent-update` (installed after first setup)

```bash
opanel-ent-update
```

### Pin a specific release

```bash
opanel-ent-update --tag v1.0.0
```

Or via curl:

```bash
bash <(curl -fsSL https://raw.githubusercontent.com/bnixvn/opanel-ent/main/installer/update.sh) --tag v1.0.0
```

### Update from a branch

```bash
opanel-ent-update --branch main
```

### Manual update

```bash
cd /opt/opanel-ent
git pull
bash installer/update.sh
```

If the browser still shows the old UI, do a hard refresh (Ctrl + Shift + R) or open in incognito.

## Webserver

OPanel Enterprise uses **OpenLiteSpeed** as the webserver:

- Vhost configs stored in `/usr/local/lsws/conf/opanel-ent/vhosts/`
- Per-site vhosts are managed by OPanel Enterprise; raw custom directives are disabled
- LSPHP replaces PHP-FPM — socket at `/tmp/lshttpd/{app_name}.sock`
- LSCache built-in for WordPress sites
- ModSecurity/WAF per-site toggle with HTTP flood protection
- `.htaccess` fully supported (`allowOverride all` in all templates)
- Rewrite inheritance enabled (`rewrite { inherit 1 }`) so SSL redirects propagate to context rules

### Rewrite modes

| Mode | Use case |
|------|----------|
| `none` | No rewrite rules |
| `front_controller` | WordPress / generic PHP (index.php routing) |
| `laravel` | Laravel (public/index.php) |
| `codeigniter` | CodeIgniter |
| `seohburl` | SEO-friendly URLs |

## Firewall

OPanel Enterprise uses **iptables + ipset** for firewall management:

- **Protected ports** (22, 80, 443, 465, 587, panel port) are always allowed
- **User rules** allow/block specific ports and IPs
- **Blocklists** via ipset sets (`opanel_ent_blocklist4`, `opanel_ent_blocklist6`) with auto-update from URL lists
- All rules persist in `/var/lib/opanel-ent/firewall/rules.json`
- Chains: `OPANEL_ENT_INPUT`, `OPANEL_ENT_USER`, `OPANEL_ENT_BLOCKLIST`

## Auto-tuning

### MariaDB auto-tuner

OPanel Enterprise includes a VPS-aware MariaDB auto-tuner accessible from the panel UI or API.

**What it tunes:** InnoDB buffer pool, log file size, flush method, IO capacity, max connections, thread cache, table cache, tmp tables, sort/read/join buffers.

**How it works:** Detects total RAM, CPU cores, and SSD vs HDD, then picks from 8 tiers (512 MB to 64 GB+) to compute optimal values. Writes `/etc/mysql/mariadb.conf.d/99-opanel-ent.cnf` and restarts MariaDB.

```
GET  /databases/mariadb/tuning      # Read current config + recommendation
POST /databases/mariadb/tuning      # Apply auto-tune + restart MariaDB
```

### PHP/LSPHP auto-tuner

OPanel Enterprise includes a VPS-aware PHP auto-tuner for all installed LSPHP versions.

Fresh installs include PHP 8.4 and 8.3. PHP 8.4 is the default version, and PHP 7.4, 8.1, 8.2, and 8.5 can be installed later from the **PHP Configuration** page.

**What it tunes:** `memory_limit` (minimum/default `1024M`), OPcache (memory, max files, JIT, interned strings), LSAPI process manager (workers, idle timeout, max process time), upload limits.

**How it works:** Same hardware detection as MariaDB tuner. Tiers from 512 MB to 8 GB+. Writes `/usr/local/lsws/lsphp{ver}/etc/php.d/99-opanel-ent.ini` and restarts OpenLiteSpeed.

```
GET  /maintenance/php/tuning?php_version=8.4       # Read current + recommendation
POST /maintenance/php/tuning?php_version=8.4        # Apply to one version
POST /maintenance/php/tuning                        # Apply to ALL installed versions
```

Both auto-tuners are also accessible from the **PHP Configuration** page in the panel UI (Auto-tune button).

## Configuration

`/opt/opanel-ent/backend/.env` is generated by the installer and contains:

```ini
APP_ENV=production
SECRET_KEY=<random-32-bytes>
COMMAND_DRY_RUN=false
DATABASE_URL=sqlite:////opt/opanel-ent/backend/opanel-ent.db
REDIS_URL=redis://localhost:6379/0
RATE_LIMIT_BACKEND=redis
ALLOWED_ORIGINS=https://panel.example.com
BACKUP_ROOT=/var/backups/opanel-ent
SSL_EMAIL=admin@example.com
PANEL_URL=http://SERVER_IP:2222
PANEL_DOMAIN=
PANEL_PORT=2222
PANEL_SSL_CERT=
PANEL_SSL_KEY=
FRONTEND_DIST=/opt/opanel-ent/frontend/dist
```

When `PANEL_URL` starts with `http://`, keep `PANEL_SSL_CERT` and `PANEL_SSL_KEY` empty. The API only starts TLS on port `2222` when `PANEL_URL` starts with `https://` and the panel certificate/key exist. Use a real panel domain for panel SSL; website SSL certificates are managed separately and must not be reused for the IP-based panel URL.

The backend refuses to start in production with `COMMAND_DRY_RUN=true` or `ALLOWED_ORIGINS=*`. SECRET_KEY must be at least 32 chars in production.

## Service commands

```bash
# API logs
journalctl -u opanel-ent-api -f

# Restart the API after backend changes
systemctl restart opanel-ent-api

# OpenLiteSpeed commands
/usr/local/lsws/bin/lswsctrl restart
/usr/local/lsws/bin/lswsctrl reload

# Service status
systemctl status opanel-ent-api lsws mariadb redis-server

# SSH rescue menu
opanel-ent

# Change a cloned/template VM from old IP to the current/new IP
opanel-ent change-ip

# Change the opanel-ent admin login password
opanel-ent change-admin-password

# Make opanel-ent admin use the current root password after cloning a VPS/template
opanel-ent sync-admin-root-password
```

## Roles

| Role | Capabilities |
|------|--------------|
| `admin` | Full control: websites, users, ownership assignment, services, firewall, PHP config, auto-tuning, backups, and security settings. |
| `end_user` | Manage only websites assigned to the account, including files, databases, SSL, WordPress tools, cron, and own backups. |

## User and website ownership

- Each panel user also has a Linux user with the same normalized username.
- The panel password is synced to the Linux password so the same account can log in with chrooted SFTP, for example `admin` to `/home/admin`.
- Panel Linux users are members of `opanel-ent-sftp`; the installer adds an SSHD `Match Group opanel-ent-sftp` block for password-based SFTP access. SSH shells, TTYs and forwarding are disabled for these users.
- New websites are created under `/home/<panel-user>/<domain>/public_html`.
- If an admin creates a website without impersonating another user, the website belongs to the admin account.
- Admins can quick-login as another panel user before creating websites for that account.
- Admins can assign a website to exactly one panel user. Moving ownership also moves the site path to the new Linux user and rewrites the OpenLiteSpeed vhost configuration.
- Deleting a panel user permanently deletes all websites, files, databases, backup schedule links, cron entries, and Linux-user data owned by that user.

## Quotas

- End users have a website count limit and a storage limit in MB.
- Admin users are not storage-limited.
- Storage usage is calculated from all websites owned by the user.
- OPanel Enterprise enforces the storage limit before site creation, upload, edit, archive, extract, and ownership assignment operations.
- This is an application-level soft quota, not an OS disk quota.

## Backups

- Each website backup archives files and database into a single `.tar.gz`.
- Admins can create backups for any website; end users can back up their own sites.
- Full-user backups bundle all websites (files + databases) and the user's local MariaDB dump into a single `.tar.gz` archive.
- SFTP backup targets let you save backups off-server.
- Scheduled backups: admins schedule daily/weekly/monthly backups for all panel users; end users can manage backup targets and run own backups.

## Security model

- 2FA (Google Authenticator compatible) can be enforced at install time and at user creation.
- Login rate limiting: 5 attempts per minute, 20 per 10 minutes, 100 per hour.
- Login audit log records all sign-in and sign-out activity.
- Security session tied to the admin role only; public routes cannot trigger an access escalation.
- The backend rejects `COMMAND_DRY_RUN=true` and `ALLOWED_ORIGINS=*` in production.
- Linux users are enforced for end-user accounts and at install time.
- All credentials are hashed with bcrypt cost 12 before storage.
- Tool commands execute through an allowlisted helper (`opanel-ent-helper.sh`).
- File manager operations are isolated into the exact Linux user home folder.
- phpMyAdmin access uses a signed one-time token (60s TTL) that is verified on login and destroyed after use.
- Certbot runs in webroot mode (`certbot certonly --webroot`) so no nginx dependency is needed.
- Backup creation streams files+DB through a tar pipeline instead of copying to temp dirs.
- Backups run at low IO priority (`ionice -c 3`) and normal scheduler priority (`nice -n 10`).
- Panel data lives in SQLite at `/opt/opanel-ent/backend/opanel-ent.db`.

## License

Copyright 2026 bNix Limited.

This project is licensed under the GNU Affero General Public License v3.0 (AGPL-3.0). See the `LICENSE` file in the repository.
