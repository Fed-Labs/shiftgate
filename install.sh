#!/bin/sh
set -eu

usage() {
    cat >&2 <<EOF
Usage: sh install.sh [--prefix /usr/local] [--systemd]

Environment:
  SHIFT_PREFIX           installation prefix (default: /usr/local)
  SHIFT_ALLOW_USER_PREFIX  set to 1 to permit unprivileged installation outside system prefixes
  SHIFT_INSTALL_SERVICE  set to 1 to enable the systemd agent after TLS checks
EOF
    exit 2
}

PREFIX="${SHIFT_PREFIX:-/usr/local}"
SYSTEMD_UNIT="${SHIFT_INSTALL_SERVICE:-0}"
ALLOW_USER_PREFIX="${SHIFT_ALLOW_USER_PREFIX:-0}"
while [ "$#" -gt 0 ]; do
    case "$1" in
        --prefix)
            [ "$#" -ge 2 ] || usage
            PREFIX=$2
            shift 2
            ;;
        --systemd)
            SYSTEMD_UNIT=1
            shift
            ;;
        --help|-h)
            usage
            ;;
        *)
            usage
            ;;
    esac
done
REPO_DIR="$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)"
BINDIR="$PREFIX/bin"
CONFIGDIR="/etc/shift"
STATEDIR="/var/lib/shift"

log() {
    printf '==> %s\n' "$*"
}

die() {
    printf 'install.sh: %s\n' "$*" >&2
    exit 1
}

as_root() {
    if [ "$(id -u)" -eq 0 ]; then
        "$@"
    elif [ "$ALLOW_USER_PREFIX" = "1" ]; then
        "$@"
    elif [ -n "${SUDO_UID:-}" ] && [ "$(id -u)" != "${SUDO_UID:-0}" ]; then
        "$@"
    elif command -v sudo >/dev/null 2>&1; then
        sudo "$@"
    else
        die "root privileges are required; rerun with root or install sudo"
    fi
}

install_package() {
    package=$1
    if command -v apt-get >/dev/null 2>&1; then
        as_root env DEBIAN_FRONTEND=noninteractive apt-get install -y "$package"
    elif command -v dnf >/dev/null 2>&1; then
        as_root dnf install -y "$package"
    elif command -v yum >/dev/null 2>&1; then
        as_root yum install -y "$package"
    elif command -v zypper >/dev/null 2>&1; then
        as_root zypper --non-interactive install "$package"
    elif command -v pacman >/dev/null 2>&1; then
        as_root pacman -S --needed --noconfirm "$package"
    else
        die "install $package manually, then rerun this script"
    fi
}

[ "$(uname -s)" = "Linux" ] || die "Linux is required"
[ "$(uname -m)" = "x86_64" ] || die "x86_64 is required; detected $(uname -m)"
[ -f "$REPO_DIR/go.mod" ] && [ -f "$REPO_DIR/Makefile" ] || die "run this script from the SHIFT repository root"
[ "$ALLOW_USER_PREFIX" = "1" ] && [ "$(id -u)" -ne 0 ] && case "$PREFIX" in
    /usr|/usr/*|/bin|/sbin|/etc|/var|/var/*) die "refusing a system prefix without root privileges" ;;
esac
command -v git >/dev/null 2>&1 || install_package git
command -v curl >/dev/null 2>&1 || command -v wget >/dev/null 2>&1 || install_package curl
command -v tar >/dev/null 2>&1 || install_package tar

if ! command -v go >/dev/null 2>&1; then
    log "Installing Go"
    if command -v apt-get >/dev/null 2>&1 && apt-cache show golang-go >/dev/null 2>&1; then
        install_package golang-go
    elif command -v dnf >/dev/null 2>&1; then
        install_package golang
    elif command -v yum >/dev/null 2>&1; then
        install_package golang
    elif command -v zypper >/dev/null 2>&1; then
        install_package go
    elif command -v pacman >/dev/null 2>&1; then
        install_package go
    else
        die "Go 1.24 or newer is required: https://go.dev/dl/"
    fi
fi

GO_VERSION="$(go env GOVERSION 2>/dev/null || go version | awk '{print $3}')"
case "$GO_VERSION" in
    go1.[[:digit:]]*|go2.*) ;;
    *) die "unable to determine the Go version" ;;
esac
GO_MINOR="${GO_VERSION#go1.}"
[ "$GO_VERSION" = "go2.0" ] || [ "${GO_MINOR%%.*}" -ge 24 ] || die "Go 1.24 or newer is required; detected $GO_VERSION"

if ! command -v criu >/dev/null 2>&1; then
    log "Installing CRIU"
    case "$(uname -s)-$(. /etc/os-release 2>/dev/null && print "${ID:-unknown}")" in
        Linux-debian|Linux-ubuntu) install_package criu ;;
        Linux-fedora) install_package criu ;;
        Linux-centos|Linux-rhel) install_package criu ;;
        Linux-opensuse*|Linux-sles) install_package criu ;;
        Linux-arch) install_package criu ;;
        *) install_package criu ;;
    esac
fi

for package in make gcc libc-dev; do
    case "$package" in
        make)
            command -v make >/dev/null 2>&1 || install_package make
            ;;
        gcc)
            command -v cc >/dev/null 2>&1 || command -v gcc >/dev/null 2>&1 || install_package gcc
            ;;
        libc-dev)
            if command -v dpkg-query >/dev/null 2>&1; then
                dpkg-query -W -f='${Status}' libc6-dev 2>/dev/null | grep -q '^install ok installed' || install_package libc6-dev
            elif command -v rpm >/dev/null 2>&1; then
                rpm -q glibc-devel >/dev/null 2>&1 || install_package glibc-devel
            fi
            ;;
    esac
done

if ! criu check >/dev/null 2>&1; then
    cat >&2 <<'EOF'
warning: `criu check` failed. Binaries can still be installed, but checkpoint and
restore will fail until the required kernel capabilities are enabled.
EOF
fi

log "Building SHIFT from verified repository sources"
OLDPWD="$PWD"
cd "$REPO_DIR"
make build
mkdir -p "$HOME/.cache/shift-install"
sha256sum bin/shiftgate bin/shift-agent bin/shift-control >"$HOME/.cache/shift-install/checksums.txt"
cd "$OLDPWD"

while read -r expected path; do
    actual="$(sha256sum "$path" | awk '{print $1}')"
    [ "$actual" = "$expected" ] || die "build output checksum mismatch for $path"
done <"$HOME/.cache/shift-install/checksums.txt"

log "Installing binaries to $BINDIR"
as_root mkdir -p "$BINDIR"
as_root install -m 0755 "$REPO_DIR/bin/shiftgate" "$BINDIR/shiftgate"
as_root install -m 0755 "$REPO_DIR/bin/shift-agent" "$BINDIR/shift-agent"
as_root install -m 0755 "$REPO_DIR/bin/shift-control" "$BINDIR/shift-control"

rollback_binaries() {
    for name in shiftgate shift-agent shift-control; do
        if [ -e "$BINDIR/$name.pre-shift" ]; then
            as_root mv "$BINDIR/$name.pre-shift" "$BINDIR/$name"
        else
            as_root rm -f "$BINDIR/$name"
        fi
    done
}

if [ "$SYSTEMD_UNIT" = "1" ]; then
    log "Installing systemd service configuration"
    [ -d /run/systemd/system ] || die "--systemd requires a running systemd system"
    as_root mkdir -p "$CONFIGDIR/tls" "$STATEDIR" /run/shift
    as_root chmod 0750 "$STATEDIR"
    as_root chmod 0755 /run/shift
    # The agent hands its Unix socket to the 'shift' group so the CLI can
    # reach it without root; create the group if it does not exist yet.
    if ! getent group shift >/dev/null 2>&1; then
        as_root groupadd --system shift >/dev/null 2>&1 \
            || as_root addgroup --system shift >/dev/null 2>&1 \
            || log "WARNING: could not create group 'shift'; only root can reach the agent socket"
    fi
    if [ ! -f "$CONFIGDIR/agent.json" ]; then
        as_root install -m 0640 "$REPO_DIR/deployments/systemd/agent.json.example" "$CONFIGDIR/agent.json"
        config_installed=1
    fi
    if [ ! -f "$CONFIGDIR/tls/agent-cert.pem" ] || [ ! -f "$CONFIGDIR/tls/agent-key.pem" ] || [ ! -f "$CONFIGDIR/tls/agents-ca.pem" ]; then
        as_root rm -rf "$CONFIGDIR/agent.json.new" "$STATEDIR/identity.partial" 2>/dev/null || true
        if [ "${config_installed:-0}" = "1" ]; then
            as_root rm -f "$CONFIGDIR/agent.json"
        fi
        rollback_binaries
        die "remote peer TLS files referenced by the example config are missing"
    fi
    as_root install -m 0644 "$REPO_DIR/deployments/systemd/shift-agent.service" /etc/systemd/system/shift-agent.service
    # enable --now starts an inactive unit but never restarts a running one —
    # an upgrade must replace the running agent, or the old binary keeps
    # serving the local API until the next reboot.
    service_started() {
        if as_root systemctl is-active --quiet shift-agent.service; then
            as_root systemctl restart shift-agent.service
        else
            as_root systemctl enable --now shift-agent.service
        fi
    }
    if service_started; then
        log "Agent service enabled"
    else
        as_root systemctl disable --failed shift-agent.service >/dev/null 2>&1 || true
        [ -e /etc/systemd/system/shift-agent.service.pre-shift ] && as_root mv /etc/systemd/system/shift-agent.service.pre-shift /etc/systemd/system/shift-agent.service
        rollback_binaries
        die "systemd service activation failed; previous binaries restored"
    fi
else
    log "Skipping systemd installation (set SHIFT_INSTALL_SERVICE=1 to enable)"
fi

rm -f "$HOME/.cache/shift-install/checksums.txt"
log "Installation complete"
printf '%s\n' \
    "  CLI:   $BINDIR/shiftgate" \
    "  Check: $BINDIR/shiftgate doctor" \
    "  Agent: systemd requires TLS configuration; see docs/installation.md"
