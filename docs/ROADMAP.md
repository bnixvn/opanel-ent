# Roadmap

What is not built yet. Everything else that used to be written down here has
been built, and the code is a better description of it than a plan was.

## The agreed sequence

Four stages, in this order, because each one is what makes the next one
possible rather than merely convenient.

**1. Apache and PHP-FPM.** Where the panel is now. Default PHP 8.4, one pool
per site running as that site's own account, versions 7.4 to 8.5 from Remi.
This stage has to be solid before the next one starts: everything after it
changes the PHP underneath a working panel, and a panel that was not working
first gives you two problems to tell apart.

**2. CloudLinux.** Done. Converted from AlmaLinux with `cldeploy`, which
reboots partway, so the installer for it is two steps. The panel now drives
LVE limits per package and per user, answers CloudLinux's own integration
scripts, serves CloudLinux Manager behind the panel session, and sets up PHP
Selector. MySQL Governor is still untouched.

The Manager is worth a note because the wrong choice here is easy to make and
looks like it works. CloudLinux ships two ways to serve it, and
`run_service = 1` -- the obvious one -- starts a service that authenticates
with PAM: a second login, for a system account, inside a page the panel has
already signed somebody in for. The other way is what the `lvemanager_config`
section is for: the panel serves the files and answers "who is asking?"
through `ui_user_info`, with `vendor.php` refusing anything that cannot prove
it came through the panel. Two things their documentation gets wrong for
CloudLinux 10: the domain field is `userDomain`, not `defaultDomain`, and
`post_modify_admin.py create` takes `--name`, not `--username`.

**3. alt-php in place of PHP-FPM.** CloudLinux's PHP, with the version picker
in the panel pointing at it. Apache runs it through `mod_lsapi`. This is a
provider swap, which is what `phpmgr.Provider` exists for: a site stores the
string "8.4" and never a path, so changing where PHP comes from is a
re-render of every vhost rather than a data migration.

**4. LiteSpeed Enterprise, with the offset switch.** Only here, and only
because of stage 3.

A correction first, because an earlier version of this file got it wrong and
the code repeated the mistake in a message shown to operators. It said
LiteSpeed serves the `.php` source when it meets Apache's PHP-FPM handler.
It does not. Measured on CloudLinux 10 with LiteSpeed Enterprise 6.3.3, four
configurations of the same vhost:

| Vhost | What LiteSpeed does |
|---|---|
| `SetHandler proxy:unix:` alone | Runs the `lsphp` it ships with — **7.2.34** — and answers 200 |
| plus `AddHandler application/x-httpd-alt-php84` | Runs alt-php 8.4.25, ini `/opt/alt/php84/etc/php.ini`, as the site's uid |
| handler naming an uninstalled version | **403**. Fails closed |
| handler naming a different installed version | Runs that version — the handler decides |

So the hazard is not a source leak; it is a site written for 8.4 silently
running on an interpreter from 2019, returning 200 for every request. Less
alarming, equally worth refusing, and a completely different thing to tell
an operator. `canServePHP` now says the true one.

The mechanism stage 4 is built on: `<IfModule LiteSpeed>` is evaluated by
LiteSpeed and skipped by Apache — Apache has no module of that name — and
`httpd -t` accepts a file containing one. Every PHP vhost therefore carries
both handlers, so switching server starts a daemon instead of re-rendering
the estate. That is what makes an automatic failover possible: the
configuration for the server that is *not* running is already on disk and
already correct.

Still outstanding, and the reason stage 3 is not finished: Apache runs
**Remi** 8.4 and LiteSpeed runs **alt-php** 8.4. Same version, different
builds, different extension sets and different `php.ini`. A failover would
change what a site's PHP can do. One provider has to win, and it has to be
alt-php, because LiteSpeed cannot be given the Remi one.

## The offset switch, and failing over

For stage 4. LiteSpeed's own arrangement, read from
`/usr/local/lsws/admin/misc/cp_switch_ws.sh` rather than from documentation:

- `<apachePortOffset>` in `httpd_config.xml` is the setting.
- Switching *to* LiteSpeed sets the offset to 0 and stops Apache outright,
  which is what the panel's switch already does. The offset exists for the
  other arrangement: Apache alive on shifted ports while LiteSpeed holds 80.
- `bin/wswatch.sh` is the vendor's watchdog. It polls the pid file every two
  seconds and restarts LiteSpeed after ten seconds down. It restarts
  LiteSpeed; it does not fail over to anything.
- LiteSpeed falls back to Apache by itself when its licence cannot be
  verified, unless `admin/tmp/.stay_with_lsws` exists.

What the panel should add on top:

- Apache warm on 8080 and 8443, LiteSpeed on 80 and 443.
- A health check that is an **HTTP request**, not `systemctl is-active`. A
  LiteSpeed that is up and serving nothing reports active, which was measured
  here and is exactly the failure a watchdog exists for.
- Three consecutive failures before failing over, and a much longer quiet
  period before failing back. A flip-flop between servers takes a site down
  harder than the fault that started it.
- The flip itself as one nftables ruleset swap redirecting 80 to 8080 and 443
  to 8443. Milliseconds, no reload of either server, no configuration
  rewritten — the panel already owns `table inet opanel`.
- An override, so a deliberate choice is never undone by the watchdog, and an
  audit entry for every flip.

## Panel self-update

The only feature from the original specification still outstanding.

- **Source**: a GitHub release or tag of this repository.
- **Flow**: check for a newer release, fetch the tarball, verify its checksum,
  unpack to a temporary directory, run migrations, replace the binaries,
  restart the agent and then the API.
- **The hard part**: a running binary replacing itself. Keep the previous
  version at `/usr/local/bin/*.prev`, and if a health check fails within
  thirty seconds of the restart, put it back. An update that half-succeeds on
  a server somebody is hosting customers on is worse than one that never ran.
- Both a button and a schedule.

Until this exists, upgrading is the four commands in the README, which work
and are not going away — an automatic update is a convenience on top of them,
not a replacement.

## CloudLinux, in detail

Stage 2 and 3 above, and a substantial piece of work rather than a feature:
CageFS, LVE resource limits, MySQL Governor, and alt-php in place of
PHP-FPM. The seams are already in place — `phpmgr.Provider` and
`webserver.Backend` are interfaces, and `distro.Detect` already reports
whether CloudLinux is present — so this is filling in second implementations
rather than restructuring anything.

`docs/bringup-el10.md` records what was verified about the platform when the
EL10 port was done, and is the starting point for the same exercise on
CloudLinux.
