# Roadmap

What is not built yet. Everything else that used to be written down here has
been built, and the code is a better description of it than a plan was.

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

## CloudLinux

Stage B, and a substantial piece of work rather than a feature: CageFS, LVE
resource limits, MySQL Governor, and alt-php in place of lsphp. The seams are
already in place — `phpmgr.Provider` and `webserver.Backend` are interfaces
with one implementation each, and `distro.Detect` already reports whether
CloudLinux is present — so this is filling in second implementations rather
than restructuring anything.

`docs/bringup-el10.md` records what was verified about the platform when the
EL10 port was done, and is the starting point for the same exercise on
CloudLinux.
