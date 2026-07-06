#!/bin/sh
# AlphaMintX watcher-host installer (beta-ops-tooling.md DM-1..DM-5).
# Installs ONLY the deadman receiver. Run as root from the repo checkout
# root on the WATCHER HOST — a machine separate from the beta VM:
#
#   sudo sh deploy/install-deadman.sh
#
# Idempotent. The env file is NOT created here — see docs/RUNBOOK.md §11.3.
#
# Re-running over a RUNNING unit fails with ETXTBSY on the Go binary:
# `systemctl stop alphamintx-deadman` first.
# Deliberately NOT part of deploy/install.sh: deadman is a separate trust
# domain (DM-1) and co-locating it with the control-plane voids BP-2 item 4.
set -eu

if [ "$(id -u)" -ne 0 ]; then
    echo "install-deadman.sh must run as root" >&2
    exit 1
fi
if [ ! -f docs/specs/beta-ops-tooling.md ]; then
    echo "run install-deadman.sh from the repo checkout root" >&2
    exit 1
fi
if ! command -v go >/dev/null 2>&1; then
    echo "install-deadman.sh: required tool not found: go" >&2
    exit 1
fi
# Refuse to install next to the control-plane: same host = same failure
# domain = no dead-man. This is a guard, not a suggestion.
if [ -f /etc/systemd/system/alphamintx-controlplane.service ]; then
    echo "install-deadman.sh: this host has the control-plane unit installed." >&2
    echo "deadman MUST run on a separate watcher host (DM-1 / BP-2 item 4)." >&2
    exit 1
fi
# Same dubious-ownership guard as install.sh (root building a user checkout).
if git -C . rev-parse --is-inside-work-tree >/dev/null 2>&1; then
    if ! git -C . status >/dev/null 2>&1; then
        git config --system --add safe.directory "$PWD"
    fi
fi

# --- system user (system account, no login shell) ------------------------
if ! getent passwd alphamintx-deadman >/dev/null; then
    useradd --system --home-dir /var/lib/alphamintx-deadman --no-create-home \
        --shell /usr/sbin/nologin alphamintx-deadman
    echo "created system user alphamintx-deadman"
fi

# --- directories ----------------------------------------------------------
mkdir -p /opt/alphamintx-deadman/bin
mkdir -p /var/lib/alphamintx-deadman
chown alphamintx-deadman:alphamintx-deadman /var/lib/alphamintx-deadman
chmod 0700 /var/lib/alphamintx-deadman
mkdir -p /etc/alphamintx

# --- binary (built artifact, never `go run`) -------------------------------
echo "building deadman..."
(
    cd control-plane
    go build -o /opt/alphamintx-deadman/bin/deadman ./cmd/deadman
)

# --- systemd unit -----------------------------------------------------------
install -m 0644 deploy/systemd/alphamintx-deadman.service /etc/systemd/system/
systemctl daemon-reload

echo ""
echo "install complete. Next steps (docs/RUNBOOK.md §11.3):"
echo "  1. Create /etc/alphamintx/deadman.env (0600 root:root):"
echo "       DEADMAN_BEARER=<generated secret>"
echo "       DEADMAN_ALARM_URL=<optional alarm POST target>"
echo "  2. Check -heartbeat-hours in the unit matches the notifier's"
echo "     heartbeat_hours, then: systemctl enable --now alphamintx-deadman"
echo "  3. Day-0 drill (DM-4): DEADMAN_BEARER from the env file, then"
echo "       /opt/alphamintx-deadman/bin/deadman -selftest -target http://127.0.0.1:9190"
