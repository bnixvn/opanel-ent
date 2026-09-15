# OPanel Enterprise

Hosting control panel for **AlmaLinux 10**, built on **Apache**, **PHP-FPM**
and **MariaDB**. Three static Go binaries and an embedded web interface: no
runtime, no interpreter, nothing to keep up to date beside the panel itself.

Apache is not the fastest server available, and that is not why it is here:
it is the one **LiteSpeed Enterprise** reads the configuration of. A host
installed this way is already in the format the commercial server wants, so
buying a licence later is a switch rather than a migration. PHP comes from
Remi, which publishes **7.4 through 8.5** for EL10 — one pool per site,
running as that site's own account.

> Forked from [OPanel](https://github.com/bnixvn/opanel) at v1.6.0 and rewritten
> in Go. Nothing of the Python implementation remains here; it is in the fork
> point if anyone ever needs it.

## How it is put together

Three binaries, because the split is the security model:

| Binary | Runs as | Does |
|---|---|---|
| `opanel-api` | `opanel`, unprivileged | Serves the panel and its API. Owns the session, decides who may ask for what. |
| `opanel-agent` | `root` | Performs a fixed set of named operations over a unix socket. Never runs a command the panel composes. |
| `opanelctl` | `root`, by hand | Installs the panel, migrates the database, and is the way back in when the panel is the thing that is broken. |

The API cannot become anybody. It asks the agent for `site.provision` or
`linuxuser.set_password`, and the agent decides whether to. That is why a
stolen panel session is not a root shell: there is no action in the agent
that runs arbitrary input as root, and the list of actions is compiled in.

## Requirements

- AlmaLinux 10 (or another EL10 rebuild), freshly installed
- 2 GB RAM minimum; 4 GB if you intend to run ClamAV scans
- A domain name pointed at the server, if you want a trusted certificate on
  the panel itself — and you do, because passkeys will not work without one

If you are going on to CloudLinux, **disk is the binding constraint, not
CPU**. Measured on a converted host before a single customer file existed:

| | |
|---|---|
| CageFS skeleton | 3.4 GB |
| alt-php, all fifteen versions | 2.4 GB |
| LiteSpeed Enterprise | ~0.2 GB |
| **before any site** | **~6 GB** |

CPU is the opposite. The three CloudLinux daemons sit at 0.0% and hold about
150 MB between them; LVE is a kernel module, so it caps a customer's CPU
rather than consuming any. Size the disk for what you will sell — it is the
part that cannot be grown later without rebuilding — and the RAM and cores
against the LVE package you intend to sell, which is arithmetic you can do
from its `PMEM` and `SPEED`.

## Install

One file, on a freshly installed AlmaLinux 10, as root:

```bash
curl -fsSLO https://raw.githubusercontent.com/bnixvn/opanel-ent/main/install.sh
bash install.sh
```

That is the whole thing. `install.sh` installs a Go toolchain if the server
has none, clones this repository to `/usr/local/src/opanel-ent`, builds the
three binaries and hands over to `opanelctl install`. Flags it does not
recognise go straight through, so `bash install.sh --port 8443 --php 8.4,8.3`
does what it looks like it does.

Its own two flags: `--ref` to build a branch, tag or commit other than
`main`, and `--src` to keep the source somewhere else. Run it again to
upgrade — it fetches, rebuilds and re-runs the installer, and every step
checks whether it is already done.

If you already have the binaries — from a release, from `make build`, or from
a clone you did yourself — skip all of that:

```bash
./opanelctl install
```

Nothing else needs installing first: the installer brings its own
dependencies. Building from source is the one path that needs a toolchain,
and a freshly installed AlmaLinux 10 has none of it — which is the only
reason `install.sh` exists.

Either way it is the same installer underneath, and it is idempotent: every
step checks whether it is already done, so running it again after a failure
continues rather than starts over. It will:

- install Apache, PHP-FPM, MariaDB, valkey, nftables and the tools a terminal
  session needs (git, composer, node, unzip)
- create the `opanel` service account and the `opanel-sftp` group
- lay down systemd units, the nftables ruleset and the SFTP chroot
- enable filesystem quota on the home filesystem
- fetch WP-CLI and Composer, checking both against their published checksums
- start the agent and the API, and print the first administrator's password

Useful flags:

```bash
./opanelctl install --port 2222 --admin admin --php 8.4,8.3
./opanelctl install --skip-firewall      # leave nftables alone
```

Then open `https://<server>:2222` and sign in with the printed password.

### CloudLinux, and LiteSpeed Enterprise

The panel installs on plain AlmaLinux 10 and works there. CloudLinux is what
turns it into something you can sell shared hosting on — LVE limits, CageFS,
HardenedPHP on versions long past end of life — and LiteSpeed Enterprise is
what makes it fast. They go on in that order, and the order is not a
preference: each step is what makes the next one possible.

Run the stages in sequence. Stage 2 reboots, which is why it cannot be folded
into the others.

**1. The panel, on AlmaLinux 10.** As above. Apache, PHP-FPM from Remi, PHP
8.4 by default. Get a site working here before going further: everything
after this changes the PHP underneath a running panel, and a panel that was
not working first gives you two problems to tell apart.

**2. Convert to CloudLinux.** Their tool, not ours — CloudLinux 10 has no
installer image, only a conversion of a running AlmaLinux:

```bash
curl -O https://repo.cloudlinux.com/cloudlinux/sources/cln/cldeploy
bash cldeploy --precheck            # look before you leap
bash cldeploy -k <YOUR-LICENCE-KEY>
reboot
```

Note that `/etc/os-release` still says AlmaLinux afterwards: CloudLinux 10
runs as a subsystem rather than as its own distribution. `cldetect
--detect-os` is the honest answer, and it is what the panel asks.

**3. Bring the host the rest of the way.** One command, idempotent, safe to
re-run:

```bash
opanelctl cloudlinux setup
```

It does four things, in an order that matters:

| | |
|---|---|
| Integration | writes `/opt/cpvendor/etc/integration.ini` and the programs it names, so CloudLinux's own tools can read this panel's accounts, domains and packages |
| alt-php + PHP Selector | `dnf group install alt-php`, then `cagefsctl --setup-cl-selector` and `--force-update`. About 2 GB, a few minutes, fifteen interpreters from 5.3 to 8.5 |
| CloudLinux Manager | installed and served **inside the panel** at `/lvemanager`, behind the panel's own session, with no second login |
| Sites onto alt-php | every site's pool is rewritten against `/opt/alt`, the vhosts re-rendered onto the new sockets, and only then the old pools pruned |

`--skip-selector`, `--skip-manager` and `--stay-on-remi` each turn one off.

There is a second route for the middle two: CloudLinux ships its own
installation wizard, and it works here — it is a tab inside the Manager the
panel serves, so the panel page and the wizard reach the same place. The
command exists because it can be scripted and the wizard cannot.

After this, sites run on CloudLinux's PHP. That is the state LiteSpeed needs.

**4. LiteSpeed Enterprise.** Install it with `control panel: None`, then
switch to it from the panel's Webserver page, or:

```bash
opanelctl agent actions | grep webserver   # it is the API the page calls
```

Why stage 3 has to come first, measured on CloudLinux 10 with LiteSpeed
Enterprise 6.3.3 rather than taken from documentation:

| Vhost | What LiteSpeed does |
|---|---|
| Apache's PHP-FPM handler alone | runs the `lsphp` **it** ships with — 7.2.34 — and answers 200 |
| plus `AddHandler application/x-httpd-alt-php84` | runs alt-php 8.4.25, as the site's own user |
| handler naming a version that is not installed | 403 |

So a host that switches to LiteSpeed without stage 3 does not break loudly.
It serves every site on an interpreter from 2019 and reports success. The
panel refuses that switch and says which sites are the problem.

Every PHP vhost the panel writes carries **both** handlers: Apache's, and
inside `<IfModule LiteSpeed>` one LiteSpeed understands. Apache has no module
of that name and skips the block entirely. That is what makes switching
server a daemon starting rather than a re-render of the estate.

**Not built yet:** the offset-port arrangement that fails over to Apache
automatically when LiteSpeed stops. The configuration it needs is already on
disk — that is what the two-handler vhost is for — but the watchdog, the
health check and the hysteresis that stop it flapping are not written. Today
the switch is a button.

### A certificate for the panel

```bash
opanelctl cert issue panel.example.com you@example.com --panel
```

`--panel` points the panel's own TLS at the certificate it just obtained, and
records the name as the one the panel answers to. Until you do this the panel
serves a self-signed certificate, which browsers accept grudgingly and
passkeys refuse outright. The email is optional; without one Let's Encrypt
cannot warn you before the certificate expires.

## Build from source

Needs Go 1.26. Node 22 is only needed to change the interface: its built
output is committed, so a fresh clone builds without touching npm.

A minimal AlmaLinux 10 has neither, and no `make` either:

```bash
dnf install -y make git
curl -fsSL https://go.dev/dl/go1.26.0.linux-amd64.tar.gz | tar -C /usr/local -xz
export PATH=$PATH:/usr/local/go/bin

git clone https://github.com/bnixvn/opanel-ent.git
cd opanel-ent
make build            # all three binaries into dist/, version stamped
```

`make web` rebuilds the interface, and is the only target that wants npm.

Building by hand loses the version stamp and the panel will report `dev`:

```bash
go build ./cmd/opanel-api    # reports "dev"
make build                   # reports the tag
```

## Upgrading

```bash
systemctl stop opanel-api opanel-agent
install -m 0755 opanel-api opanel-agent opanelctl /usr/local/bin/
opanelctl db migrate
systemctl start opanel-agent opanel-api
```

Migrations are embedded in the binary and only ever move forward. The agent
repairs what it can on the way up — account home ownership, the terminal's
command list — so an upgrade is usually these four lines and nothing else.

## What it does

**Websites.** Domains and subdomains with per-site PHP version, document root,
aliases and redirects. `.htaccess` works. Each site lives in its owner's home
at `/home/<owner>/<domain>/public_html`, so the filesystem says who owns what.

**WordPress.** One-click install, plugin and theme management, and a sign-in
link that works once and expires in two minutes.

**Databases.** MariaDB databases and accounts, prefixed by owner, with
phpMyAdmin reachable through the panel session rather than a second password.

**Files.** Browse, edit, upload, compress and extract. Compressing, extracting
and saving an upload run as background jobs, so closing the tab does not stop
them.

**Terminal.** A shell on a pseudo-terminal as the account's own Linux user,
never root, with a command list an administrator can widen or turn off.

**SFTP.** The account's own login plus as many extra logins as it needs, each
with its own password and its own removal, all chrooted into the account.

**Backups.** Files and databases, scheduled or on demand, copied off the
server over SFTP or to S3. Restore and import from cPanel or DirectAdmin
archives.

**SSL.** Let's Encrypt over HTTP-01, wildcards over Cloudflare DNS, or a
certificate you bought. Renewal is automatic for the ones that can be renewed.

**Security.** nftables firewall with subscription blocklists, ModSecurity with
the OWASP Core Rule Set by category and per-site, optional ClamAV scanning
with quarantine, two-factor authentication and passkeys.

**Accounts.** Administrators, resellers and end users, with packages that cap
websites, databases and disk. Disk is enforced by the filesystem, not by the
panel asking nicely.

**CloudLinux**, when the host has been converted to it. LVE limits set per
package and per account, CloudLinux's own integration scripts answered so
their tools can read this panel, PHP Selector set up, and **CloudLinux
Manager served inside the panel** -- its own Current Usage, Users,
Statistics, Options, Packages and Selector tabs, behind the panel's session
and with no second login. The vendor's alternative is a service on a port of
its own that asks for a system password; the panel does not use it.

## Repository layout

```
install.sh         the one-file installer: toolchain, source, build, install
cmd/               the three binaries
internal/
  agent/           the root side: action registry, terminal sessions
  agent/actions/   every privileged operation, one file per subject
  httpapi/         the unprivileged side: routes, handlers, embedded web
  db/              schema, migrations, queries
  installer/       what `opanelctl install` runs
  webserver/       Apache config rendering, and the backend registry
  phpfpm/          one pool per site, owned by the site's account
  platform/        thin wrappers over the host: users, packages, quota, pty
web/               the interface's source; built output is committed
crates/            the Rust agent, mid-port and not yet serving anything
modules/servers/   WHMCS provisioning module
docs/              what is not built yet, and notes on the platform
```

## Licence

Proprietary. All rights reserved.
