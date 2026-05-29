#!/usr/bin/env bash
# Idempotent installer for cli-semaphore on alcatraz-like Linux hosts.
#
# Run as root (sudo -A ./install.sh). The script:
#   - installs the claude-msg binary to ${PREFIX}/bin/
#   - creates ${DATADIR} owned by the operator user
#   - drops the systemd user template into the operator's
#     ~/.config/systemd/user/
#
# The actual mailman enablement (`systemctl --user enable --now
# claude-mailman@AGENT.service`) is the operator's job — the install
# script makes no assumptions about which agents you want serviced.
#
# Re-running is safe: existing files are overwritten, the DB is never
# touched.
set -euo pipefail

PREFIX=${PREFIX:-/usr/local}
DATADIR=${DATADIR:-/var/lib/cli-semaphore}
OPERATOR_USER=${SUDO_USER:-${USER:-alex}}

if [[ $EUID -ne 0 ]]; then
    echo "install.sh: must run as root (try: sudo -A ./install.sh)" >&2
    exit 1
fi

# Resolve operator's home so we can install the systemd template there.
OPERATOR_HOME=$(getent passwd "$OPERATOR_USER" | cut -d: -f6)
if [[ -z "$OPERATOR_HOME" ]]; then
    echo "install.sh: cannot resolve home dir for $OPERATOR_USER" >&2
    exit 1
fi
USER_SYSTEMD="$OPERATOR_HOME/.config/systemd/user"

cd "$(dirname "$0")"

# 1. Build if the binary isn't already in bin/.
if [[ ! -x bin/claude-msg ]]; then
    echo "==> building claude-msg"
    GO=${GO:-go}
    if ! command -v "$GO" >/dev/null 2>&1; then
        # Common alternate Go install path on alcatraz.
        if [[ -x /usr/local/go/bin/go ]]; then
            GO=/usr/local/go/bin/go
        else
            echo "install.sh: go not found in PATH; set GO=/path/to/go" >&2
            exit 1
        fi
    fi
    mkdir -p bin
    sudo -u "$OPERATOR_USER" "$GO" build -o bin/claude-msg ./cmd/claude-msg
fi

# 2. Install binary (root-owned, world-readable+executable).
echo "==> installing $PREFIX/bin/claude-msg"
install -m 0755 -o root -g root bin/claude-msg "$PREFIX/bin/claude-msg"

# 3. Create the data directory.
if [[ ! -d "$DATADIR" ]]; then
    echo "==> creating $DATADIR (owner: $OPERATOR_USER)"
    install -d -m 0755 -o "$OPERATOR_USER" -g "$OPERATOR_USER" "$DATADIR"
else
    echo "==> $DATADIR already exists (left alone)"
fi

# 4. Install the systemd user template.
echo "==> installing systemd user template"
install -d -m 0755 -o "$OPERATOR_USER" -g "$OPERATOR_USER" "$USER_SYSTEMD"
install -m 0644 -o "$OPERATOR_USER" -g "$OPERATOR_USER" \
    init/claude-mailman@.service "$USER_SYSTEMD/claude-mailman@.service"

echo
echo "Install complete."
echo
echo "Next steps (run as $OPERATOR_USER, NOT as root):"
echo "  systemctl --user daemon-reload"
echo "  # ensure your user systemd manager runs at boot:"
echo "  sudo loginctl enable-linger $OPERATOR_USER"
echo "  # populate the agents table from the current tmux state:"
echo "  claude-msg discover"
echo "  # enable a mailman per agent you want to receive messages:"
echo "  systemctl --user enable --now claude-mailman@surveyor.service"
