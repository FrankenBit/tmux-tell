#!/usr/bin/env bash
# Idempotent uninstaller for tmux-msg.
#
# Run as root (sudo -A ./uninstall.sh). The script:
#   - stops + disables every running claude-mailman@*.service user unit
#     under the operator's session
#   - removes the systemd user template from
#     ~/.config/systemd/user/claude-mailman@.service
#   - removes the claude-msg binary from ${PREFIX}/bin/
#
# By default the SQLite data directory at ${DATADIR} is left ALONE —
# message history + the agents table survive an uninstall. Pass --purge
# to wipe the data directory too (after an interactive confirmation
# prompt when stdin is a TTY).
#
# Re-running is safe: missing units, missing files, and missing data
# directories are all no-ops with an informational log line.
#
# Refuses to run when CWD is the data directory or a parent of the
# binary — protects against the foot-gun of running the script from
# the very tree it would delete.
#
# Companion to install.sh (#14); fulfils the uninstall AC tracked as
# #80 (filed when the M6 install issue's "Uninstall path documented"
# AC was reviewed).
set -euo pipefail

PREFIX=${PREFIX:-/usr/local}
DATADIR=${DATADIR:-/var/lib/tmux-msg}
OPERATOR_USER=${SUDO_USER:-${USER:-alex}}
PURGE_DATA=false

usage() {
    cat <<'EOF'
Usage: sudo -A ./uninstall.sh [--purge] [--prefix DIR] [--datadir DIR]

  --purge           Also delete the SQLite data directory (default-off;
                    needs an interactive confirmation when stdin is a TTY).
  --prefix DIR      Where the binary lives (default: /usr/local).
  --datadir DIR     Where the SQLite DB lives (default: /var/lib/tmux-msg).
  -h, --help        Show this message.

The script leaves /etc/tmux-msg/ alone (operator may have hand-
edited config there per #54). Remove that directory manually if you
also want to wipe the host config.

What --purge does NOT touch:
  - /etc/tmux-msg/        (operator-edited config; remove by hand)
  - tmux-msg MCP entry in ~/.claude.json (remove with claude mcp
    remove tmux-msg -s user)
  - loginctl enable-linger     (other services on the host may rely on it)
EOF
}

while [[ $# -gt 0 ]]; do
    case "$1" in
        --purge) PURGE_DATA=true ;;
        --prefix) PREFIX="$2"; shift ;;
        --datadir) DATADIR="$2"; shift ;;
        -h|--help) usage; exit 0 ;;
        *) echo "uninstall.sh: unknown flag: $1" >&2; usage >&2; exit 1 ;;
    esac
    shift
done

if [[ $EUID -ne 0 ]]; then
    echo "uninstall.sh: must run as root (try: sudo -A ./uninstall.sh)" >&2
    exit 1
fi

# Foot-gun guard: refuse to run from inside the data directory itself.
# The `realpath` resolves symlinks so a chrooted run still trips.
CWD_REAL=$(realpath .)
DATADIR_REAL=$(realpath "$DATADIR" 2>/dev/null || echo "$DATADIR")
if [[ "$CWD_REAL" == "$DATADIR_REAL"* ]]; then
    echo "uninstall.sh: refusing to run from inside ${DATADIR_REAL}" >&2
    echo "  cd out of the data directory before running the script." >&2
    exit 1
fi

# Resolve operator's home + uid so systemctl --user can target the
# right session. Same shape as install.sh.
OPERATOR_HOME=$(getent passwd "$OPERATOR_USER" | cut -d: -f6)
OPERATOR_UID=$(id -u "$OPERATOR_USER" 2>/dev/null || echo "")
if [[ -z "$OPERATOR_HOME" || -z "$OPERATOR_UID" ]]; then
    echo "uninstall.sh: cannot resolve home dir or uid for $OPERATOR_USER" >&2
    exit 1
fi
USER_SYSTEMD="$OPERATOR_HOME/.config/systemd/user"

# 1. Stop + disable every claude-mailman@*.service user unit.
#
# `systemctl --user` needs XDG_RUNTIME_DIR set to the operator's
# session for it to talk to the right manager. machinectl or sudo -i
# would work too; the explicit env-var form is the smallest portable
# shape.
sysctl_user() {
    sudo -u "$OPERATOR_USER" \
        XDG_RUNTIME_DIR="/run/user/${OPERATOR_UID}" \
        systemctl --user "$@"
}

if sysctl_user list-units --no-legend 'claude-mailman@*.service' >/dev/null 2>&1; then
    units=$(sysctl_user list-units --no-legend --plain --state=loaded,active,failed \
        'claude-mailman@*.service' 2>/dev/null | awk '{print $1}')
    if [[ -n "$units" ]]; then
        echo "==> stopping mailman units:"
        # shellcheck disable=SC2086
        for u in $units; do
            echo "    $u"
            sysctl_user stop "$u" || true
            sysctl_user disable "$u" || true
        done
    else
        echo "==> no claude-mailman@*.service units running (skip)"
    fi
fi

# 2. Remove the systemd user template + reload the manager so it
# forgets the unit.
TEMPLATE_PATH="$USER_SYSTEMD/claude-mailman@.service"
if [[ -e "$TEMPLATE_PATH" ]]; then
    echo "==> removing $TEMPLATE_PATH"
    rm -f "$TEMPLATE_PATH"
    sysctl_user daemon-reload || true
else
    echo "==> $TEMPLATE_PATH not present (skip)"
fi

# 3. Remove the binary.
BIN_PATH="${PREFIX}/bin/claude-msg"
if [[ -e "$BIN_PATH" ]]; then
    echo "==> removing $BIN_PATH"
    rm -f "$BIN_PATH"
else
    echo "==> $BIN_PATH not present (skip)"
fi

# 4. Optional: purge the data directory. Default OFF — message history
# survives an uninstall unless --purge is passed.
if $PURGE_DATA; then
    if [[ -d "$DATADIR" ]]; then
        # Interactive confirmation when stdin is a TTY. In non-
        # interactive contexts (CI, automation) the --purge flag is
        # the operator's explicit consent — skip the prompt.
        if [[ -t 0 ]]; then
            read -r -p "Really delete ${DATADIR} and its SQLite contents? [yes/NO] " ack
            if [[ "$ack" != "yes" ]]; then
                echo "==> --purge declined; ${DATADIR} preserved"
                PURGE_DATA=false
            fi
        fi
        if $PURGE_DATA; then
            echo "==> removing ${DATADIR}"
            rm -rf "$DATADIR"
        fi
    else
        echo "==> ${DATADIR} not present (skip)"
    fi
else
    if [[ -d "$DATADIR" ]]; then
        echo "==> ${DATADIR} preserved (pass --purge to wipe it)"
    fi
fi

echo
echo "Uninstall complete."
echo
echo "Not touched (remove by hand if you also want them gone):"
echo "  /etc/tmux-msg/             — host-level config (#54)"
echo "  ~/.claude.json                  — tmux-msg MCP entry; remove with:"
echo "                                    claude mcp remove tmux-msg -s user"
echo "  loginctl enable-linger          — other services may need it"
if ! $PURGE_DATA && [[ -d "$DATADIR" ]]; then
    echo "  ${DATADIR}    — SQLite message history (re-run with --purge to wipe)"
fi
