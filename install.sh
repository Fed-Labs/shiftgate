#!/bin/sh
# SHIFT installer (repository checkout).
#
# Run from the repository root to build and install from sources. Piped from
# curl without a checkout present, it delegates to the release installer so
# both entry points end in the same state: binaries in /usr/local/bin, a
# local-only agent configuration, the 'shift' group, and the agent enabled as
# a systemd service (no TLS material needed until you add a remote listener).
#
# Options (flags or environment):
#   --prefix DIR     install prefix (default: /usr/local)
#   --no-service     install binaries only; no config, no systemd unit
#   --systemd        accepted for compatibility; installing the service is the
#                    default (SHIFT_INSTALL_SERVICE=1), same as the release
#                    installer
#   --url URL        release base URL, only when delegating to the release
#                    installer (default: SHIFT_RELEASE_URL or the GitHub
#                    latest-download redirect for this project)
#
# Downloads are SHA-256-verified before anything runs or installs: the
# release installer is checked against the published install.sh.sha256
# sidecar (and optionally against a SHIFT_INSTALL_SHA256 pin), and the
# release tarball is checked against its .tar.gz.sha256 sidecar.
set -eu

usage() {
    cat >&2 <<'EOF'
Usage: sh install.sh [--prefix /usr/local] [--no-service] [--url RELEASE_URL]

  --prefix DIR     installation prefix (default: /usr/local)
  --no-service     install binaries only; no config, no systemd unit
  --systemd        accepted for compatibility (the service is installed by
                   default, like the release installer)
  --url URL        release base URL when no repository checkout is present

Environment:
  SHIFT_PREFIX             installation prefix
  SHIFT_ALLOW_USER_PREFIX  set to 1 to permit unprivileged installation outside system prefixes
  SHIFT_INSTALL_SERVICE    set to 0 to skip the systemd agent (same as --no-service)
  SHIFT_RELEASE_URL        release base URL for the download fallback
  SHIFT_INSTALL_SHA256     optional pin: the expected SHA-256 of the release
                           installer fetched when no checkout is present
EOF
    exit 2
}

PREFIX="${SHIFT_PREFIX:-/usr/local}"
WANT_SERVICE="${SHIFT_INSTALL_SERVICE:-1}"
SKIP_REASON="service installation was disabled (--no-service or SHIFT_INSTALL_SERVICE=0)"
ALLOW_USER_PREFIX="${SHIFT_ALLOW_USER_PREFIX:-0}"
RELEASE_URL="${SHIFT_RELEASE_URL:-https://github.com/Fed-Labs/shiftgate/releases/latest/download}"
while [ "$#" -gt 0 ]; do
    case "$1" in
        --prefix)
            [ "$#" -ge 2 ] || usage
            PREFIX=$2
            shift 2
            ;;
        --systemd)
            WANT_SERVICE=1
            shift
            ;;
        --no-service)
            WANT_SERVICE=0
            shift
            ;;
        --url)
            [ "$#" -ge 2 ] || usage
            RELEASE_URL=$2
            shift 2
            ;;
        --help|-h)
            usage
            ;;
        *)
            usage
            ;;
    esac
done
REPO_DIR="$(CDPATH= cd -- "$(dirname -- "$0")" 2>/dev/null && pwd || pwd)"
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
    elif command -v sudo >/dev/null 2>&1; then
        sudo "$@"
    else
        die "root privileges are required; rerun with root or install sudo"
    fi
}

[ "$(uname -s)" = "Linux" ] || die "Linux is required"
[ "$(uname -m)" = "x86_64" ] || die "x86_64 is required; detected $(uname -m)"

# A checkout is trusted only when it really is this project: `curl | bash`
# resolves $0 to the shell, so REPO_DIR becomes $PWD, and an unrelated project
# sitting there must never be built or installed from.
if [ -f "$REPO_DIR/go.mod" ] && [ -f "$REPO_DIR/Makefile" ] \
    && grep -q '^module shift.dev/shift' "$REPO_DIR/go.mod" 2>/dev/null; then
    REPO_DIR="$REPO_DIR"
else
    REPO_DIR=""
fi

# No checkout near the script (typically `curl | bash`): delegate to the
# release installer so the one-command path works from either entry point.
# Every downloaded byte is SHA-256-verified before it runs: the installer
# against the release's published install.sh.sha256 sidecar (and, when the
# operator pins SHIFT_INSTALL_SHA256, against that pin — the sidecar alone is
# same-origin and proves integrity, not authenticity; pin the hash from a
# trusted channel when you need the stronger claim).
if [ -z "$REPO_DIR" ]; then
    log "No repository checkout found; downloading the release installer from $RELEASE_URL"
    command -v curl >/dev/null 2>&1 || command -v wget >/dev/null 2>&1 \
        || die "neither curl nor wget is available; install one and retry"
    DOWNLOAD_DIR="$(mktemp -d)"
    trap 'rm -rf "$DOWNLOAD_DIR"' EXIT
    if command -v curl >/dev/null 2>&1; then
        curl -fsSL "$RELEASE_URL/install.sh" -o "$DOWNLOAD_DIR/install.sh" \
            || die "could not download $RELEASE_URL/install.sh"
        curl -fsSL "$RELEASE_URL/install.sh.sha256" -o "$DOWNLOAD_DIR/install.sh.sha256" \
            || die "release has no install.sh.sha256 sidecar; refusing to run unverified installer bytes"
    else
        wget -q "$RELEASE_URL/install.sh" -O "$DOWNLOAD_DIR/install.sh" \
            || die "could not download $RELEASE_URL/install.sh"
        wget -q "$RELEASE_URL/install.sh.sha256" -O "$DOWNLOAD_DIR/install.sh.sha256" \
            || die "release has no install.sh.sha256 sidecar; refusing to run unverified installer bytes"
    fi
    installer_expected="$(awk '{print $1}' "$DOWNLOAD_DIR/install.sh.sha256")"
    [ -n "$installer_expected" ] || die "install.sh.sha256 sidecar is malformed"
    installer_actual="$(sha256sum "$DOWNLOAD_DIR/install.sh" | awk '{print $1}')"
    [ "$installer_actual" = "$installer_expected" ] \
        || die "installer checksum mismatch: expected $installer_expected, got $installer_actual — the download is corrupt or tampered; report this"
    if [ -n "${SHIFT_INSTALL_SHA256:-}" ]; then
        [ "$installer_actual" = "$SHIFT_INSTALL_SHA256" ] \
            || die "installer does not match the pinned SHIFT_INSTALL_SHA256; refusing to run"
        log "Installer verified against the pinned SHA-256"
    else
        log "Installer verified against the release's install.sh.sha256 sidecar"
    fi
    set -- --prefix "$PREFIX"
    if [ "$WANT_SERVICE" = "0" ]; then
        set -- "$@" --no-service
    fi
    if [ -n "${SHIFT_RELEASE_URL:-}" ] || [ "$RELEASE_URL" != "https://github.com/Fed-Labs/shiftgate/releases/latest/download" ]; then
        export SHIFT_RELEASE_URL="$RELEASE_URL"
    fi
    sh "$DOWNLOAD_DIR/install.sh" "$@"
    exit $?
fi

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
        die "Go 1.25 or newer is required: https://go.dev/dl/"
    fi
fi

GO_VERSION="$(go env GOVERSION 2>/dev/null || go version | awk '{print $3}')"
case "$GO_VERSION" in
    go1.[[:digit:]]*|go2.*) ;;
    *) die "unable to determine the Go version" ;;
esac
GO_MAJOR="${GO_VERSION#go}"
GO_MAJOR="${GO_MAJOR%%.*}"
GO_MINOR="${GO_VERSION#go${GO_MAJOR}.}"
GO_MINOR="${GO_MINOR%%.*}"
case "$GO_MAJOR$GO_MINOR" in
    *[!0-9]*) die "unable to parse the Go version: $GO_VERSION" ;;
esac
# Go 2.x and newer are accepted outright; 1.x needs a modern minor
# (go.mod declares `go 1.25.0`; older toolchains cannot build this module).
[ "$GO_MAJOR" -gt 1 ] || [ "$GO_MINOR" -ge 25 ] || die "Go 1.25 or newer is required; detected $GO_VERSION"

if ! command -v criu >/dev/null 2>&1; then
    log "Installing CRIU (the kernel's checkpoint/restore engine)"
    # Every supported distribution ships the package as 'criu';
    # install_package dispatches on the package manager.
    install_package criu
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

log "Building SHIFT from this checkout"
OLDPWD="$PWD"
cd "$REPO_DIR"
make build
mkdir -p "$HOME/.cache/shift-install"
# An integrity check of the freshly built artifacts (a corrupt or truncated
# binary fails here, before anything is installed) — not provenance: the
# source of truth is this checkout, so there is nothing external to verify
# against.
sha256sum bin/shiftgate bin/shift-agent bin/shift-control bin/shift-gc bin/shift-verify bin/shift-relay >"$HOME/.cache/shift-install/checksums.txt"
cd "$OLDPWD"

while read -r expected path; do
    actual="$(sha256sum "$path" | awk '{print $1}')"
    [ "$actual" = "$expected" ] || die "build output checksum mismatch for $path"
done <"$HOME/.cache/shift-install/checksums.txt"

log "Installing binaries to $BINDIR"
as_root mkdir -p "$BINDIR"
WORKDIR="$HOME/.cache/shift-install"
: >"$WORKDIR/installed.txt"

# From here on the script is replacing system state. Any exit that is not a
# committed install — a failed step under set -e as much as a deliberate
# die — must leave the machine as it found it: restore what was replaced
# (.pre-shift), remove what this run added (installed.txt), put back the
# service unit only if this run replaced it.
rollback_binaries() {
    if [ -f "$WORKDIR/installed.txt" ]; then
        while read -r name; do
            if [ -e "$BINDIR/$name.pre-shift" ]; then
                as_root mv "$BINDIR/$name.pre-shift" "$BINDIR/$name"
            else
                as_root rm -f "$BINDIR/$name"
            fi
        done <"$WORKDIR/installed.txt"
    fi
    rm -f "$WORKDIR/installed.txt"
}

committed=0
unit_installed=0
restore_prior_state() {
    if [ "$committed" = "1" ]; then
        return 0
    fi
    if [ "$unit_installed" = "1" ]; then
        if [ -e /etc/systemd/system/shift-agent.service.pre-shift ]; then
            as_root mv /etc/systemd/system/shift-agent.service.pre-shift /etc/systemd/system/shift-agent.service
        else
            as_root rm -f /etc/systemd/system/shift-agent.service
        fi
    fi
    rollback_binaries
    rm -f "$HOME/.cache/shift-install/checksums.txt"
    printf '==> aborted before committing; previous installation restored\n' >&2
}
trap restore_prior_state EXIT

for name in shiftgate shift-agent shift-control shift-gc shift-verify shift-relay; do
    if [ -e "$BINDIR/$name" ]; then
        as_root mv "$BINDIR/$name" "$BINDIR/$name.pre-shift"
    fi
    as_root install -m 0755 "$REPO_DIR/bin/$name" "$BINDIR/$name"
    printf '%s\n' "$name" >>"$WORKDIR/installed.txt"
done

# The same terminal state as the release installer: a 'shift' group the agent
# socket is handed to (so the CLI works unprivileged), a local-only
# agent.json (no remote listener, so no TLS material), and the service
# enabled. TLS is only required when the installed config references it.
if [ "$WANT_SERVICE" = "1" ]; then
    if [ "$PREFIX" != "/usr/local" ]; then
        log "Custom prefix: skipping the systemd unit (it expects /usr/local/bin)."
        SKIP_REASON="a custom --prefix was given (the systemd unit expects /usr/local/bin); rerun without --prefix to get the service"
        WANT_SERVICE=0
    elif [ "$(id -u)" -ne 0 ] && ! command -v sudo >/dev/null 2>&1; then
        log "No root access: skipping the system service (binaries are installed)."
        log "Run 'shift-agent --state-dir ~/shift-state --listen unix://\$HOME/shift-state/agent.sock &'"
        log "and 'shiftgate --agent unix://\$HOME/shift-state/agent.sock doctor' to use it unprivileged."
        SKIP_REASON="root is required for the systemd unit and sudo is unavailable; rerun with root to get the service"
        WANT_SERVICE=0
    fi
fi

ADDED_TO_GROUP=0
if [ "$WANT_SERVICE" = "1" ]; then
    log "Installing the agent as a systemd service"
    [ -d /run/systemd/system ] || die "the systemd service requires a running systemd system"
    as_root mkdir -p "$CONFIGDIR/tls" "$STATEDIR" /run/shift
    as_root chmod 0750 "$STATEDIR"
    as_root chmod 0755 /run/shift
    # The agent hands its Unix socket to the 'shift' group so the CLI can
    # reach it without root; create the group if it does not exist yet and
    # enroll the installing user.
    if getent group shift >/dev/null 2>&1; then
        log "Group 'shift' already exists"
    elif as_root groupadd --system shift >/dev/null 2>&1 || as_root addgroup --system shift >/dev/null 2>&1; then
        log "Created system group 'shift'"
    else
        log "WARNING: could not create group 'shift'; only root can reach the agent socket"
        log "         create it manually (groupadd --system shift) and restart shift-agent"
    fi
    # The shift-control / shift-relay units run as user 'shift'
    # (deployments/systemd/*.service). Create it here so the documented unit
    # install path never starts with a nonexistent user.
    if getent passwd shift >/dev/null 2>&1; then
        log "User 'shift' already exists"
    elif as_root useradd --system --home-dir /var/lib/shift --shell /usr/sbin/nologin shift >/dev/null 2>&1 \
        || as_root adduser --system --group --home /var/lib/shift --no-create-home --shell /usr/sbin/nologin shift >/dev/null 2>&1; then
        log "Created system user 'shift' (for shift-control / shift-relay services)"
    else
        log "WARNING: could not create user 'shift'; create it before enabling shift-control/shift-relay:"
        log "         useradd --system --home-dir /var/lib/shift --shell /usr/sbin/nologin shift"
    fi
    install_user="${SUDO_USER:-$(id -un 2>/dev/null || true)}"
    if [ -n "$install_user" ] && [ "$install_user" != "root" ]; then
        if as_root usermod -aG shift "$install_user" >/dev/null 2>&1 \
            || as_root addgroup "$install_user" shift >/dev/null 2>&1; then
            ADDED_TO_GROUP=1
        else
            log "WARNING: could not add '$install_user' to group 'shift'"
        fi
    fi
    config_installed=0
    if [ ! -f "$CONFIGDIR/agent.json" ]; then
        # Local-only defaults: a Unix-socket API and no remote listener, so
        # no TLS material exists to provision. Adding peers later means
        # setting remote_listen and the tls block — see docs/installation.md.
        cat >"$WORKDIR/agent.json" <<'EOF'
{
  "version": 1,
  "state_dir": "/var/lib/shift",
  "listen": "unix:///run/shift/agent.sock",
  "log_level": "info"
}
EOF
        # Mode 0600 is not optional: the agent's loader refuses any config
        # readable by group or other (chmod 600), so anything else crash-loops.
        as_root install -m 0600 "$WORKDIR/agent.json" "$CONFIGDIR/agent.json"
        as_root chown root:root "$CONFIGDIR/agent.json"
        config_installed=1
    fi
    # TLS is only required when the config actually references it: a
    # remote_listen deployment names certificate/key/CA paths, and every
    # one of them must exist before the agent may start.
    missing_tls=""
    if grep -q '"remote_listen"' "$CONFIGDIR/agent.json" 2>/dev/null; then
        for tls_path in $(sed -n 's/.*"\(certificate_file\|private_key_file\|client_ca_file\|peer_ca_file\)"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\2/p' "$CONFIGDIR/agent.json"); do
            [ -f "$tls_path" ] || missing_tls="$missing_tls $tls_path"
        done
    fi
    if [ -n "$missing_tls" ]; then
        if [ "$config_installed" = "1" ]; then
            as_root rm -f "$CONFIGDIR/agent.json"
        fi
        die "the agent config references remote-peer TLS files that are missing:$missing_tls
       Either install certificates at those paths (see docs/installation.md,
       'Accepting migrations from other machines') or remove remote_listen and
       the tls block from $CONFIGDIR/agent.json for a local-only agent — the
       local Unix socket API needs no TLS material."
    fi
    if [ -e /etc/systemd/system/shift-agent.service ]; then
        as_root mv /etc/systemd/system/shift-agent.service /etc/systemd/system/shift-agent.service.pre-shift
    fi
    as_root install -m 0644 "$REPO_DIR/deployments/systemd/shift-agent.service" /etc/systemd/system/shift-agent.service
    unit_installed=1
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
        die "systemd service activation failed; previous installation restored"
    fi
else
    log "Skipping systemd installation: $SKIP_REASON"
fi

rm -f "$HOME/.cache/shift-install/checksums.txt"
# The install committed: the aside copies it replaced are no longer needed.
# Clear them first, then drop the rollback marker and stand the abort trap
# down in one step, so no abort can observe a state where the markers are
# gone but the aside copies still exist.
for name in shiftgate shift-agent shift-control shift-gc shift-verify shift-relay; do
    if [ -e "$BINDIR/$name.pre-shift" ]; then
        as_root rm -f "$BINDIR/$name.pre-shift"
    fi
done
if [ -e /etc/systemd/system/shift-agent.service.pre-shift ]; then
    as_root rm -f /etc/systemd/system/shift-agent.service.pre-shift
fi
rm -f "$HOME/.cache/shift-install/installed.txt"
committed=1
log "Installation complete"

if [ "$ADDED_TO_GROUP" = "1" ]; then
    relogin_note="You were added to the 'shift' group — log out and back in once"
    relogin_note="$relogin_note (or run 'newgrp shift') so 'shiftgate' can reach the agent."
else
    relogin_note=""
fi

printf '%s\n' \
    "  CLI:   $BINDIR/shiftgate" \
    "  Check: $BINDIR/shiftgate doctor" \
    "${relogin_note:+  $relogin_note}" \
    "  Agent: local-only by default (no TLS material); add remote_listen + the" \
    "         tls block to $CONFIGDIR/agent.json to accept migrations — see docs/installation.md"
