#!/usr/bin/env bash
#
# OPanel Enterprise -- one-file installer.
#
# Put this file on a freshly installed AlmaLinux 10 server and run it as root:
#
#   bash install.sh
#
# It brings its own toolchain, builds the three binaries from source and hands
# over to `opanelctl install`, which does the actual work of installing the
# panel. Everything here is idempotent: run it again after a failure, or to
# upgrade, and it continues rather than starting over.
#
# Any flag it does not recognise is passed through to `opanelctl install`, so
#
#   bash install.sh --port 8443 --php 8.4,8.3
#
# does what you would expect.

set -euo pipefail

REPO_URL="${OPANEL_REPO:-https://github.com/bnixvn/opanel-ent.git}"
REF="main"
SRC_DIR="/usr/local/src/opanel-ent"

# Pinned, with the checksums published by go.dev. The distro's own golang is
# used when it is new enough; this is the fallback, and a fallback that
# downloads a toolchain over the network has to verify what it got.
GO_VERSION="1.26.0"
GO_SHA256_amd64="aac1b08a0fb0c4e0a7c1555beb7b59180b05dfc5a3d62e40e9de90cd42f88235"
GO_SHA256_arm64="bd03b743eb6eb4193ea3c3fd3956546bf0e3ca5b7076c8226334afe6b75704cd"

# Flags meant for this script rather than for opanelctl.
PASS_THROUGH=()
while [ $# -gt 0 ]; do
	case "$1" in
	--ref)
		REF="${2:?--ref needs a branch, tag or commit}"
		shift 2
		;;
	--ref=*)
		REF="${1#*=}"
		shift
		;;
	--src)
		SRC_DIR="${2:?--src needs a directory}"
		shift 2
		;;
	--src=*)
		SRC_DIR="${1#*=}"
		shift
		;;
	-h | --help)
		sed -n '3,20p' "$0" | sed 's/^# \{0,1\}//'
		echo
		echo "Flags for this script:"
		echo "  --ref <ref>   branch, tag or commit to build (default: main)"
		echo "  --src <dir>   where to keep the source (default: $SRC_DIR)"
		echo
		echo "Everything else is passed to 'opanelctl install'; see --help there."
		exit 0
		;;
	*)
		PASS_THROUGH+=("$1")
		shift
		;;
	esac
done

step() { printf '\n==> %s\n' "$*"; }
note() { printf '    %s\n' "$*"; }
die() {
	printf '\nopanel install: %s\n' "$*" >&2
	exit 1
}

# ---------------------------------------------------------------- the host

[ "$(id -u)" -eq 0 ] || die "run this as root."

[ -r /etc/os-release ] || die "no /etc/os-release; this does not look like a Linux distribution I know."
# shellcheck disable=SC1091
. /etc/os-release
OS_MAJOR="${VERSION_ID%%.*}"
case "${ID}:${OS_MAJOR}" in
almalinux:10 | rocky:10 | rhel:10 | centos:10 | cloudlinux:10) ;;
*)
	# ID_LIKE catches the rebuilds that do not name themselves above.
	case " ${ID_LIKE:-} " in
	*" rhel "* | *" fedora "*)
		[ "$OS_MAJOR" = "10" ] || die "OPanel needs an EL10 distribution; this is ${PRETTY_NAME:-$ID $VERSION_ID}."
		;;
	*)
		die "OPanel needs AlmaLinux 10 or another EL10 rebuild; this is ${PRETTY_NAME:-$ID $VERSION_ID}."
		;;
	esac
	;;
esac

ARCH="$(uname -m)"
case "$ARCH" in
x86_64) GOARCH=amd64 ;;
aarch64) GOARCH=arm64 ;;
*) die "no build for $ARCH; OPanel runs on x86_64 and aarch64." ;;
esac

MEM_MB=$(awk '/^MemTotal:/ {print int($2/1024)}' /proc/meminfo)
if [ "$MEM_MB" -lt 1800 ]; then
	die "this server has ${MEM_MB} MB of RAM. OPanel needs 2 GB, and the build alone will not fit."
fi

step "OPanel Enterprise on ${PRETTY_NAME:-$ID $VERSION_ID} ($ARCH, ${MEM_MB} MB RAM)"

# ---------------------------------------------------------------- toolchain

step "Build tools"
dnf install -y -q git make tar gzip >/dev/null
note "git, make, tar"

# The distro's Go if it is new enough, upstream's if it is not. A minor
# version behind what go.mod asks for does not build at all, so this is
# checked rather than assumed.
go_is_new_enough() {
	command -v "$1" >/dev/null 2>&1 || return 1
	local have
	have="$("$1" version 2>/dev/null | awk '{print $3}' | sed 's/^go//')"
	[ -n "$have" ] || return 1
	[ "$(printf '%s\n%s\n' "$GO_VERSION" "$have" | sort -V | head -1)" = "$GO_VERSION" ]
}

GO=""
for candidate in go /usr/local/go/bin/go /usr/lib/golang/bin/go; do
	if go_is_new_enough "$candidate"; then
		GO="$(command -v "$candidate" || echo "$candidate")"
		break
	fi
done

if [ -z "$GO" ]; then
	# Ask what the repository has before installing it: a golang package a
	# minor version too old cannot build this, and installing it anyway
	# would leave a few hundred megabytes of package nothing will use.
	repo_go="$(dnf -q info golang 2>/dev/null | awk '/^Version/ {print $3; exit}')"
	if [ -n "$repo_go" ] &&
		[ "$(printf '%s\n%s\n' "$GO_VERSION" "$repo_go" | sort -V | head -1)" = "$GO_VERSION" ] &&
		dnf install -y -q golang >/dev/null 2>&1 && go_is_new_enough go; then
		GO="$(command -v go)"
		note "go $("$GO" version | awk '{print $3}') from the distribution"
	else
		tarball="go${GO_VERSION}.linux-${GOARCH}.tar.gz"
		want="GO_SHA256_${GOARCH}"
		note "downloading ${tarball}"
		curl -fsSL -o "/tmp/$tarball" "https://go.dev/dl/${tarball}"
		echo "${!want}  /tmp/$tarball" | sha256sum -c - >/dev/null ||
			die "the Go tarball did not match its published checksum. Nothing was installed."
		rm -rf /usr/local/go
		tar -C /usr/local -xzf "/tmp/$tarball"
		rm -f "/tmp/$tarball"
		GO="/usr/local/go/bin/go"
		note "go ${GO_VERSION} in /usr/local/go"
	fi
else
	note "go $("$GO" version | awk '{print $3}') already here"
fi
export PATH="$PATH:$(dirname "$GO"):/usr/local/go/bin"

# ---------------------------------------------------------------- source

# Running from inside a checkout uses it, so an operator who cloned the
# repository themselves does not end up with a second copy under /usr/local.
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
if [ -f "$HERE/go.mod" ] && grep -q '^module github.com/bnixvn/opanel-ent$' "$HERE/go.mod"; then
	SRC_DIR="$HERE"
	step "Source: this checkout ($SRC_DIR)"
else
	step "Source: $SRC_DIR (ref $REF)"
	if [ -d "$SRC_DIR/.git" ]; then
		git -C "$SRC_DIR" remote set-url origin "$REPO_URL"
		git -C "$SRC_DIR" fetch --tags --prune origin
		# Hard reset rather than pull: this directory belongs to the
		# installer, and a merge conflict here would strand an upgrade
		# halfway with no obvious way out.
		git -C "$SRC_DIR" checkout -q --detach "origin/$REF" 2>/dev/null ||
			git -C "$SRC_DIR" checkout -q --detach "$REF"
		note "updated to $(git -C "$SRC_DIR" rev-parse --short HEAD)"
	else
		mkdir -p "$(dirname "$SRC_DIR")"
		git clone -q "$REPO_URL" "$SRC_DIR"
		git -C "$SRC_DIR" checkout -q --detach "origin/$REF" 2>/dev/null ||
			git -C "$SRC_DIR" checkout -q --detach "$REF"
		note "cloned at $(git -C "$SRC_DIR" rev-parse --short HEAD)"
	fi
fi

# ---------------------------------------------------------------- build

step "Building"
# One compiler process per 1.5 GB. Go defaults to one per core, and three
# binaries built four-wide on a 2 GB VPS is how the OOM killer gets involved
# in an installation.
JOBS=$((MEM_MB / 1500))
[ "$JOBS" -lt 1 ] && JOBS=1
CORES="$(nproc)"
[ "$JOBS" -gt "$CORES" ] && JOBS="$CORES"
note "$JOBS compile job(s)"

cd "$SRC_DIR"
# The web interface is committed already built, so npm is not needed and is
# not installed. See the note in .gitignore.
GOFLAGS="-p=$JOBS" GOMAXPROCS="$JOBS" make build GO="$GO" >/dev/null
note "opanel-api, opanel-agent, opanelctl -> $SRC_DIR/dist"
./dist/opanelctl version

# ---------------------------------------------------------------- install

step "Installing the panel"
# opanelctl copies the three binaries out of its own directory into
# /usr/local/bin, writes the systemd units, and starts them.
exec ./dist/opanelctl install "${PASS_THROUGH[@]+"${PASS_THROUGH[@]}"}"
