#!/bin/sh
# AlphaMintX install script (deploy-and-survive.md DS-11). Idempotent; run
# as root from the repo checkout root on the VM:
#
#   sudo sh deploy/install.sh
#
# Builds and installs the control-plane binaries, the agent-plane venv, and
# the web standalone build; creates the alphamintx system user, the state
# and backup directories, and installs the systemd units. Env files are NOT
# created here — see docs/RUNBOOK.md §10.
#
# Re-running over RUNNING units fails with ETXTBSY on the Go binaries:
# stop the units first (RUNBOOK §10.6 upgrade order).
set -eu

if [ "$(id -u)" -ne 0 ]; then
    echo "install.sh must run as root" >&2
    exit 1
fi
if [ ! -f docs/specs/deploy-and-survive.md ]; then
    echo "run install.sh from the repo checkout root" >&2
    exit 1
fi
for tool in go pnpm node python3; do
    if ! command -v "$tool" >/dev/null 2>&1; then
        echo "install.sh: required tool not found: $tool" >&2
        exit 1
    fi
done
if [ ! -x /usr/bin/node ]; then
    echo "install.sh: /usr/bin/node not found — the web unit's ExecStart requires it" >&2
    exit 1
fi
# Root builds from a user-owned checkout: without this, git's dubious-
# ownership guard aborts `go build -buildvcs=auto` (and -buildvcs=false
# would erase the DS-12 vcs.revision stamp).
if git -C . rev-parse --is-inside-work-tree >/dev/null 2>&1; then
    if ! git -C . status >/dev/null 2>&1; then
        git config --system --add safe.directory "$PWD"
    fi
fi

# --- system user (system account, no login shell) ------------------------
if ! getent passwd alphamintx >/dev/null; then
    useradd --system --home-dir /var/lib/alphamintx --no-create-home \
        --shell /usr/sbin/nologin alphamintx
    echo "created system user alphamintx"
fi

# --- directories ----------------------------------------------------------
mkdir -p /opt/alphamintx/bin
mkdir -p /var/lib/alphamintx /var/backups/alphamintx
chown alphamintx:alphamintx /var/lib/alphamintx /var/backups/alphamintx
chmod 0700 /var/lib/alphamintx /var/backups/alphamintx
mkdir -p /etc/alphamintx

# --- control-plane binaries (DS-11: built artifacts, never `go run`) ------
echo "building control-plane binaries..."
(
    cd control-plane
    go build -o /opt/alphamintx/bin/controlplane ./cmd/controlplane
    go build -o /opt/alphamintx/bin/backupverify ./cmd/backupverify
)

# --- agent-plane venv ------------------------------------------------------
echo "installing agent-plane venv..."
mkdir -p /opt/alphamintx/agent-plane
python3 -m venv /opt/alphamintx/agent-plane/.venv
/opt/alphamintx/agent-plane/.venv/bin/pip install --quiet --upgrade pip
/opt/alphamintx/agent-plane/.venv/bin/pip install --quiet ./agent-plane

# --- web standalone build (DS-11c) -----------------------------------------
# CONTROLPLANE_API_BASE_URL and NEXT_PUBLIC_READ_TOKEN are BUILD-time
# inputs: export them before running this script, and use `sudo -E` so
# they survive sudo (RUNBOOK §10).
echo "building web standalone..."
(
    cd web
    pnpm install --frozen-lockfile
    pnpm run build
)
# Next.js standalone layout: server.js at the standalone root; .next/static
# and public/ are NOT copied by `next build` and must ship alongside.
rm -rf /opt/alphamintx/web
mkdir -p /opt/alphamintx/web
cp -a web/.next/standalone/. /opt/alphamintx/web/
mkdir -p /opt/alphamintx/web/.next
cp -a web/.next/static /opt/alphamintx/web/.next/static
if [ -d web/public ]; then
    cp -a web/public /opt/alphamintx/web/public
fi
if [ ! -f /opt/alphamintx/web/server.js ]; then
    echo "install.sh: /opt/alphamintx/web/server.js missing after standalone copy" >&2
    echo "(outputFileTracingRoot mis-inference? check web/.next/standalone layout)" >&2
    exit 1
fi

# --- systemd units ----------------------------------------------------------
install -m 0644 deploy/systemd/alphamintx-controlplane.service \
    deploy/systemd/alphamintx-scheduler@.service \
    deploy/systemd/alphamintx-web.service \
    /etc/systemd/system/
systemctl daemon-reload

echo ""
echo "install complete. Next steps (docs/RUNBOOK.md §10):"
echo "  1. Create env files (0600 root:root):"
echo "       /etc/alphamintx/controlplane.env"
echo "       /etc/alphamintx/scheduler-<strategy-id>.env  (one per strategy)"
echo "       /etc/alphamintx/web.env"
echo "  2. Enable and start:"
echo "       systemctl enable --now alphamintx-controlplane"
echo "       systemctl enable --now alphamintx-scheduler@<strategy-id>"
echo "       systemctl enable --now alphamintx-web"
echo "  3. Verify: /opt/alphamintx/bin/controlplane --version;"
echo "     curl -sS http://localhost:8080/health; systemctl status ..."
