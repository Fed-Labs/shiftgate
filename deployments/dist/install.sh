#!/bin/sh
# SHIFT one-command installer.
#
#   curl -fsSL <release-url>/install.sh | bash
#
# Downloads the release tarball, verifies its SHA-256, installs the release
# binaries (CLI, agent, control plane, plus the GC/verify/relay tools when
# the tarball carries them), installs CRIU when the distro offers it, writes
# a local-only agent configuration, and enables the systemd service. A
# local-only agent (no remote_listen) needs no TLS material; peers are added
# later by editing /etc/shift/agent.json — docs/installation.md describes the
# TLS files a remote listener requires.
#
# Options (flags or environment):
#   --url URL        release base URL (default: SHIFT_RELEASE_URL or the
#                    GitHub latest-download redirect for this project)
#   --prefix DIR     install prefix (default: /usr/local)
#   --no-service     install binaries only; no config, no systemd unit
set -eu

RELEASE_URL="${SHIFT_RELEASE_URL:-https://github.com/Fed-Labs/shiftgate/releases/latest/download}"
PREFIX="${SHIFT_PREFIX:-/usr/local}"
WANT_SERVICE=1

usage() {
    cat >&2 <<'EOF'
Usage: sh install.sh [--url RELEASE_URL] [--prefix /usr/local] [--no-service]
Environment: SHIFT_RELEASE_URL, SHIFT_PREFIX
EOF
    exit 2
}

while [ "$#" -gt 0 ]; do
    case "$1" in
        --url) [ "$#" -ge 2 ] || usage; RELEASE_URL=$2; shift 2 ;;
        --prefix) [ "$#" -ge 2 ] || usage; PREFIX=$2; shift 2 ;;
        --no-service) WANT_SERVICE=0; shift ;;
        --help|-h) usage ;;
        *) usage ;;
    esac
done

log() { printf '==> %s\n' "$*"; }
die() { printf 'install.sh: %s\n' "$*" >&2; exit 1; }

have_root() { [ "$(id -u)" -eq 0 ]; }

[ "$(uname -s)" = "Linux" ] || die "Linux is required"
[ "$(uname -m)" = "x86_64" ] || die "x86_64 is required; detected $(uname -m)"

command -v curl >/dev/null 2>&1 || command -v wget >/dev/null 2>&1 \
    || die "neither curl nor wget is available; install one and retry"

fetch() {
    if command -v curl >/dev/null 2>&1; then
        curl -fsSL "$1" -o "$2"
    else
        wget -q "$1" -O "$2"
    fi
}

WORKDIR="$(mktemp -d)"
trap 'rm -rf "$WORKDIR"' EXIT

log "Downloading SHIFT from $RELEASE_URL"
TARBALL="$WORKDIR/shiftgate-linux-amd64.tar.gz"
fetch "$RELEASE_URL/shiftgate-linux-amd64.tar.gz" "$TARBALL"
fetch "$RELEASE_URL/shiftgate-linux-amd64.tar.gz.sha256" "$TARBALL.sha256" \
    || die "release has no SHA-256 sidecar; refusing to install unverified bytes"

expected="$(awk '{print $1}' "$TARBALL.sha256")"
[ -n "$expected" ] || die "SHA-256 sidecar is malformed"
actual="$(sha256sum "$TARBALL" | awk '{print $1}')"
[ "$actual" = "$expected" ] \
    || die "checksum mismatch: expected $expected, got $actual — the download is corrupt or the release is inconsistent; report this"

mkdir -p "$WORKDIR/extract"
tar -xzf "$TARBALL" -C "$WORKDIR/extract"
for name in shiftgate shift-agent shift-control; do
    [ -f "$WORKDIR/extract/$name" ] || die "release tarball is missing the $name binary"
done

case "$PREFIX" in
    /usr|/usr/*|/bin|/sbin|/etc|/var|/var/*)
        have_root || command -v sudo >/dev/null 2>&1 \
            || die "installing under $PREFIX needs root; rerun with sudo or pass --prefix \$HOME/.local"
        NEEDS_ROOT=1
        ;;
    *) NEEDS_ROOT=0 ;;
esac

# as_root escalates only when the install location actually needs it; a
# user-owned prefix installs without ever asking for a password.
if [ "$NEEDS_ROOT" = "1" ]; then
    as_root() {
        if have_root; then
            "$@"
        else
            sudo "$@"
        fi
    }
else
    as_root() { "$@"; }
fi

BINDIR="$PREFIX/bin"
log "Installing binaries to $BINDIR"
as_root mkdir -p "$BINDIR"

# From here on the script replaces system state. Any exit that is not a
# committed install — a failed step under set -e as much as a deliberate
# die — must leave the machine as it found it: restore what was replaced
# (.pre-shift), remove what this run added (installed.txt). The same
# aside/rollback contract as the checkout installer (install.sh), so both
# entry points end in the same state or none at all.
: >"$WORKDIR/installed.txt"
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
    if [ "$committed" != "1" ]; then
        printf '==> aborted before committing; previous installation restored\n' >&2
    fi
    rm -rf "$WORKDIR"
}
trap restore_prior_state EXIT

for name in shiftgate shift-agent shift-control; do
    [ -f "$WORKDIR/extract/$name" ] || die "release tarball is missing the $name binary"
    if [ -e "$BINDIR/$name" ]; then
        as_root mv "$BINDIR/$name" "$BINDIR/$name.pre-shift"
    fi
    as_root install -m 0755 "$WORKDIR/extract/$name" "$BINDIR/$name"
    printf '%s\n' "$name" >>"$WORKDIR/installed.txt"
done
# The maintenance tools ship alongside the core three; older tarballs
# predate them, so install each only when present. shift-gc reclaims chunk
# store disk, shift-verify runs the backup-restore drill, shift-relay serves
# relay-mediated migrations (see docs/relay.md).
for name in shift-gc shift-verify shift-relay; do
    if [ -f "$WORKDIR/extract/$name" ]; then
        if [ -e "$BINDIR/$name" ]; then
            as_root mv "$BINDIR/$name" "$BINDIR/$name.pre-shift"
        fi
        as_root install -m 0755 "$WORKDIR/extract/$name" "$BINDIR/$name"
        printf '%s\n' "$name" >>"$WORKDIR/installed.txt"
    fi
done

# The CLI was named `shift` up to v0.1.0 — a name every POSIX shell shadows
# with its own builtin, so the installed binary never ran. Remove that stale
# copy when it is ours, so `which shift` stops hiding the problem.
if [ -x "$BINDIR/shift" ] && [ ! -e "$BINDIR/shift.pre-shiftgate" ]; then
    if "$BINDIR/shift" version 2>/dev/null | head -1 | grep -q '^SHIFT'; then
        log "Renaming the old 'shift' CLI aside to 'shift.pre-shiftgate' (v0.1.0 shadowed by the shell builtin); it is 'shiftgate' now"
        as_root mv "$BINDIR/shift" "$BINDIR/shift.pre-shiftgate"
    fi
fi

installed_version="$("$BINDIR/shiftgate" version 2>/dev/null || echo "SHIFT unknown")"
log "Installed $installed_version"

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
        log "WARNING: install $package with your package manager, then run 'shiftgate doctor'"
        return 1
    fi
}

if ! command -v criu >/dev/null 2>&1; then
    log "Installing CRIU (the kernel's checkpoint/restore engine)"
    install_package criu || true
else
    log "CRIU $(criu --version | awk 'NR==1 {print $2}') already present"
fi

if [ "$WANT_SERVICE" = "1" ] && [ "$(id -u)" -ne 0 ] && ! command -v sudo >/dev/null 2>&1; then
    log "No root access: skipping the system service (binaries are installed)."
    log "Run 'shift-agent --state-dir ~/shift-state --listen unix://\$HOME/shift-state/agent.sock &'"
    log "and 'shiftgate --agent unix://\$HOME/shift-state/agent.sock doctor' to use it unprivileged."
    WANT_SERVICE=0
fi
if [ "$PREFIX" != "/usr/local" ]; then
    [ "$WANT_SERVICE" = "1" ] && log "Custom prefix: skipping the systemd unit (it expects /usr/local/bin)."
    WANT_SERVICE=0
fi

if [ "$WANT_SERVICE" = "1" ]; then
    log "Installing the agent as a systemd service"
    [ -d /run/systemd/system ] || die "the systemd service requires a running systemd system"
    as_root mkdir -p /etc/shift /var/lib/shift /run/shift
    as_root chmod 0750 /var/lib/shift

    # The agent hands its Unix socket to the 'shift' group so the CLI works
    # unprivileged. Create the group and enroll the installing user.
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
    if [ ! -f /etc/shift/agent.json ]; then
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
        as_root install -m 0600 "$WORKDIR/agent.json" /etc/shift/agent.json
        as_root chown root:root /etc/shift/agent.json
    fi
    # TLS is only required when the config actually references it: a
    # remote_listen deployment names certificate/key/CA paths, and every
    # one of them must exist before the agent may start.
    missing_tls=""
    if grep -q '"remote_listen"' /etc/shift/agent.json 2>/dev/null; then
        for tls_path in $(sed -n 's/.*"\(certificate_file\|private_key_file\|client_ca_file\|peer_ca_file\)"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\2/p' /etc/shift/agent.json); do
            [ -f "$tls_path" ] || missing_tls="$missing_tls $tls_path"
        done
    fi
    if [ -n "$missing_tls" ]; then
        die "the agent config references remote-peer TLS files that are missing:$missing_tls
       Either install certificates at those paths (see docs/installation.md,
       'Accepting migrations from other machines') or remove remote_listen and
       the tls block from /etc/shift/agent.json for a local-only agent — the
       local Unix socket API needs no TLS material."
    fi
    # The unit is embedded because this installer runs with no repository
    # checkout — keep it in sync with deployments/systemd/shift-agent.service.
    cat >"$WORKDIR/shift-agent.service" <<'EOF'
[Unit]
Description=SHIFT privileged workload mobility agent
Documentation=https://github.com/Fed-Labs/shiftgate
After=local-fs.target network-online.target
Wants=network-online.target
# A unit that fails on every start must reach the failed state instead of
# restarting forever: with RestartSec=5s the default start-limit window
# (10s / burst 5) is never reached, so tune the window to match.
StartLimitIntervalSec=60
StartLimitBurst=5

[Service]
Type=simple
ExecStart=/usr/local/bin/shift-agent --config /etc/shift/agent.json
EnvironmentFile=-/etc/shift/agent.env
Restart=on-failure
RestartSec=5s
User=root
Group=root
UMask=0077
StateDirectory=shift
RuntimeDirectory=shift
# The agent checkpoints and restores workloads rooted anywhere on the
# filesystem, so the sandbox keeps only the OS directories read-only. It
# must not hide /home or mount the whole tree read-only (ProtectSystem=strict,
# ProtectHome) — that fails every restore whose workload root lives outside
# /var/lib/shift — and it must allow AF_NETLINK, which CRIU needs to
# enumerate network state; without it CRIU's kernel-feature detection fails
# before any dump can start.
NoNewPrivileges=true
ProtectSystem=full
RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6 AF_NETLINK
CapabilityBoundingSet=CAP_CHOWN CAP_DAC_OVERRIDE CAP_FOWNER CAP_KILL CAP_NET_ADMIN CAP_SETGID CAP_SETUID CAP_SYS_ADMIN CAP_SYS_CHROOT CAP_SYS_PTRACE
AmbientCapabilities=CAP_CHOWN CAP_DAC_OVERRIDE CAP_FOWNER CAP_KILL CAP_NET_ADMIN CAP_SETGID CAP_SETUID CAP_SYS_ADMIN CAP_SYS_CHROOT CAP_SYS_PTRACE
LimitNOFILE=1048576

[Install]
WantedBy=multi-user.target
EOF
    if [ -e /etc/systemd/system/shift-agent.service ]; then
        as_root mv /etc/systemd/system/shift-agent.service /etc/systemd/system/shift-agent.service.pre-shift
    fi
    as_root install -m 0644 "$WORKDIR/shift-agent.service" /etc/systemd/system/shift-agent.service
    unit_installed=1
    as_root systemctl daemon-reload
    # enable --now starts an inactive unit but never restarts a running one —
    # an upgrade must replace the running agent, or the old binary keeps
    # serving the local API until the next reboot.
    if as_root systemctl is-active --quiet shift-agent.service; then
        if as_root systemctl restart shift-agent.service; then
            log "Agent service restarted on the new binary"
        else
            log "WARNING: service failed to restart; check 'journalctl -u shift-agent'"
        fi
    else
        if as_root systemctl enable --now shift-agent.service; then
            log "Agent service enabled and started"
        else
            log "WARNING: service failed to start; check 'journalctl -u shift-agent'"
        fi
    fi
fi

# The install committed: the aside copies are no longer needed. Clear them
# first, then drop the rollback marker and stand the abort trap down in one
# step, so no abort can observe a state where the markers are gone but the
# aside copies still exist.
for name in shiftgate shift-agent shift-control shift-gc shift-verify shift-relay; do
    if [ -e "$BINDIR/$name.pre-shift" ]; then
        as_root rm -f "$BINDIR/$name.pre-shift"
    fi
done
if [ -e /etc/systemd/system/shift-agent.service.pre-shift ]; then
    as_root rm -f /etc/systemd/system/shift-agent.service.pre-shift
fi
rm -f "$WORKDIR/installed.txt"
committed=1
log "Installation complete"

if [ "${ADDED_TO_GROUP:-0}" = "1" ]; then
    relogin_note="You were added to the 'shift' group — log out and back in once"
    relogin_note="$relogin_note (or run 'newgrp shift') so 'shiftgate' can reach the agent."
else
    relogin_note=""
fi

printf '%s\n' \
    "" \
    "$installed_version installed." \
    "${relogin_note:+$relogin_note}" \
    "" \
    "Next steps:" \
    "  shiftgate doctor                                # verify the machine can checkpoint" \
    "  mkdir -p ~/demo" \
    "  shiftgate workload create demo --path ~/demo -- python3 -m http.server 8080" \
    "  shiftgate workload start demo" \
    "  shiftgate checkpoint create demo                # take an encrypted checkpoint" \
    "  shiftgate checkpoint list                       # find the checkpoint id" \
    "  shiftgate restore CHECKPOINT_ID                 # bring it back" \
    ""


