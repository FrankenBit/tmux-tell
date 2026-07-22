#!/usr/bin/env bash
# Idempotent uninstaller for tmux-tell. Mirrors install.sh's mode split (#671):
#
# Default is a USER-SPACE uninstall — no root (matches install.sh's #636
# default): run `./uninstall.sh [--adapter=claude]` as your normal user to undo
# a user-space install (binary under ~/.local/bin, systemd template under
# ~/.config/systemd/user). Pass `--system` to undo a root install (binary under
# /usr/local/bin; requires sudo). The script:
#   - stops + disables every running tmux-tell-<adapter>-mailman@*.service unit
#     under the operator's session
#   - removes the systemd user template
#     ~/.config/systemd/user/tmux-tell-<adapter>-mailman@.service (+ the legacy
#     claude-mailman@ alias for the claude adapter)
#   - removes the tmux-tell-<adapter> binary from ${PREFIX}/bin/ (+ its
#     deprecation aliases tmux-msg-<adapter>, and claude-msg for claude)
#
# By default the SQLite data directory at ${DATADIR} is left ALONE —
# message history + the agents table survive an uninstall. Pass --purge
# to wipe the data directory too (after an interactive confirmation
# prompt when stdin is a TTY).
#
# Re-running is safe: missing units, missing files, and missing data
# directories are all no-ops with an informational log line.
#
# Refuses to run from inside the data directory — protects against the
# foot-gun of running the script from the very tree it would delete
# (the guard checks CWD against ${DATADIR}, resolved below).
#
# Companion to install.sh (#14); fulfils the uninstall AC tracked as
# #80 (filed when the M6 install issue's "Uninstall path documented"
# AC was reviewed).
set -euo pipefail

# Install mode (#671, mirrors install.sh #636). Default 0 = user-space uninstall
# (no root); --system flips to 1 for the root uninstall. PREFIX's default depends
# on it, so it is resolved AFTER arg parsing (an explicit PREFIX= env override
# wins in either mode and is captured here).
SYSTEM=0
PREFIX=${PREFIX:-}
# Which adapter to uninstall (mirrors install.sh --adapter). Selects the binary +
# unit names removed; `claude` additionally carries the older claude-msg /
# claude-mailman@ deprecation aliases.
ADAPTER=${ADAPTER:-claude}
# Default computed from the operator's user-home below (#308) when not set
# explicitly via --datadir / $DATADIR.
DATADIR=${DATADIR:-}
# Operator account (owns the systemd session the mailmen run under). Precedence
# mirrors install.sh: explicit $OPERATOR_USER, then sudo's $SUDO_USER (the
# --system case), then $USER (the default user-space case). No hardcoded
# fallback — guessing a username targets the wrong session.
OPERATOR_USER=${OPERATOR_USER:-${SUDO_USER:-${USER:-}}}
PURGE_DATA=false

usage() {
    cat <<'EOF'
Usage: ./uninstall.sh [--adapter=NAME] [--system] [--purge] [--prefix DIR] [--datadir DIR]

  --adapter=NAME    Which adapter to remove: claude (default) or codex.
  --system          Undo a root/system install (binary under /usr/local/bin;
                    requires sudo). Default is a user-space uninstall — no root,
                    binary under ~/.local/bin.
  --purge           Also delete the SQLite data directory (default-off;
                    needs an interactive confirmation when stdin is a TTY).
  --prefix DIR      Where the binary lives (default: ~/.local user-space,
                    /usr/local with --system).
  --datadir DIR     Where the SQLite DB lives (default:
                    ~/.local/share/tmux-tell under the operator's home, #308;
                    a pre-rename install may still hold ~/.local/share/tmux-msg).
  -h, --help        Show this message.

The script leaves /etc/tmux-tell/ alone (operator may have hand-
edited config there per #54). Remove that directory manually if you
also want to wipe the host config.

What --purge does NOT touch:
  - /etc/tmux-tell/       (operator-edited config; remove by hand; a
    pre-rename install may also have /etc/tmux-msg/)
  - the MCP entry in ~/.claude.json — remove with: claude mcp remove
    tmux-tell -s user  (or: claude mcp remove tmux-msg -s user on a
    pre-rename / legacy chamber)
  - loginctl enable-linger     (other services on the host may rely on it)
EOF
}

while [[ $# -gt 0 ]]; do
    case "$1" in
        --adapter=*) ADAPTER="${1#--adapter=}" ;;
        --adapter) ADAPTER="$2"; shift ;;
        --system) SYSTEM=1 ;;
        --purge) PURGE_DATA=true ;;
        --prefix) PREFIX="$2"; shift ;;
        --datadir) DATADIR="$2"; shift ;;
        -h|--help) usage; exit 0 ;;
        *) echo "uninstall.sh: unknown flag: $1" >&2; usage >&2; exit 1 ;;
    esac
    shift
done

# Resolve the PREFIX default now that the mode is known (mirrors install.sh). An
# explicit PREFIX= env / --prefix override (captured above) wins in either mode.
if [[ -z "$PREFIX" ]]; then
    if [[ "$SYSTEM" -eq 1 ]]; then
        PREFIX=/usr/local
    else
        PREFIX="$HOME/.local"
    fi
fi

# Adapter-derived names (mirror install.sh). The canonical binary + unit are
# tmux-tell-<adapter>; claude additionally carries the older claude-msg binary +
# claude-mailman@ template deprecation aliases (removed at the v1.0 boundary).
BIN_NAME="tmux-tell-${ADAPTER}"
UNIT_PREFIX="tmux-tell-${ADAPTER}-mailman@"
LEGACY_BINS=("tmux-msg-${ADAPTER}")
LEGACY_UNIT=""
if [[ "$ADAPTER" == "claude" ]]; then
    LEGACY_BINS+=("claude-msg")
    LEGACY_UNIT="claude-mailman@.service"
fi

if [[ "$SYSTEM" -eq 1 ]]; then
    # --system removes the root-owned binary under /usr/local — needs root.
    if [[ $EUID -ne 0 ]]; then
        echo "uninstall.sh: --system uninstall must run as root (try: sudo -A ./uninstall.sh --system)" >&2
        exit 1
    fi
else
    # The default user-space uninstall touches only the invoking user's home.
    # Running it as root would target root's home / the wrong session — reject.
    if [[ $EUID -eq 0 ]]; then
        echo "uninstall.sh: the default uninstall is user-space and must NOT run as root." >&2
        echo "  Run it as your normal user (no sudo), or pass --system to undo a root install." >&2
        exit 1
    fi
fi

if [[ -z "$OPERATOR_USER" || "$OPERATOR_USER" == "root" ]]; then
    echo "uninstall.sh: cannot determine the operator user (got: '${OPERATOR_USER}')." >&2
    echo "  Set OPERATOR_USER=<you> or run via sudo (which exports \$SUDO_USER)." >&2
    exit 1
fi

# Resolve operator's home + uid so systemctl --user can target the right
# session, and so the default data dir resolves under user-home (#308). Same
# shape as install.sh. Resolved BEFORE the foot-gun guard because the guard
# needs the final DATADIR value.
OPERATOR_HOME=$(getent passwd "$OPERATOR_USER" | cut -d: -f6)
OPERATOR_UID=$(id -u "$OPERATOR_USER" 2>/dev/null || echo "")
if [[ -z "$OPERATOR_HOME" || -z "$OPERATOR_UID" ]]; then
    echo "uninstall.sh: cannot resolve home dir or uid for $OPERATOR_USER" >&2
    exit 1
fi
# USER_SYSTEMD may be overridden (testing / bespoke layouts); defaults to the
# operator's standard user-unit dir (mirrors install.sh).
USER_SYSTEMD="${USER_SYSTEMD:-$OPERATOR_HOME/.config/systemd/user}"

# Default data dir is the operator's user-home location (#308) unless an
# explicit --datadir / $DATADIR override was given. Honors the standard XDG
# fallback (~/.local/share); a custom $XDG_DATA_HOME install should pass
# --datadir explicitly (the operator's shell env isn't visible to this
# root-run script).
if [[ -z "$DATADIR" ]]; then
    DATADIR="$OPERATOR_HOME/.local/share/tmux-tell"
    # Lazy-rename fallback (#440): a pre-rename install keeps its data at the
    # legacy tmux-msg path until migrated — purge whichever actually exists.
    if [[ ! -d "$DATADIR" && -d "$OPERATOR_HOME/.local/share/tmux-msg" ]]; then
        DATADIR="$OPERATOR_HOME/.local/share/tmux-msg"
    fi
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

# 1. Stop + disable every running mailman unit for this adapter.
#
# `systemctl --user` needs to talk to the operator's session manager. In
# --system mode we start as root and drop to the operator with XDG_RUNTIME_DIR
# + DBUS_SESSION_BUS_ADDRESS pointed at their session bus (both are needed —
# without the bus address the stop/disable silently no-ops under `|| true`,
# orphaning the mailmen we then remove the binary+template from underneath);
# in the default user-space mode we ARE the
# operator, so we call systemctl --user directly (our own session, and
# `sudo -u $self` would needlessly demand sudo). Mirrors install.sh's #636
# privilege-drop split.
if [[ "$SYSTEM" -eq 1 ]]; then
    sysctl_user() {
        sudo -u "$OPERATOR_USER" \
            XDG_RUNTIME_DIR="/run/user/${OPERATOR_UID}" \
            DBUS_SESSION_BUS_ADDRESS="unix:path=/run/user/${OPERATOR_UID}/bus" \
            systemctl --user "$@"
    }
else
    sysctl_user() {
        systemctl --user "$@"
    }
fi

# The canonical unit is tmux-tell-<adapter>-mailman@; for claude also sweep the
# legacy claude-mailman@ instances (a pre-rename install may have enabled units
# under that name — the template is a symlink, but instance names differ).
UNIT_GLOBS=("${UNIT_PREFIX}*.service")
if [[ -n "$LEGACY_UNIT" ]]; then
    UNIT_GLOBS+=("${LEGACY_UNIT%.service}*.service")
fi
for glob in "${UNIT_GLOBS[@]}"; do
    if sysctl_user list-units --no-legend "$glob" >/dev/null 2>&1; then
        units=$(sysctl_user list-units --no-legend --plain --state=loaded,active,failed \
            "$glob" 2>/dev/null | awk '{print $1}')
        if [[ -n "$units" ]]; then
            echo "==> stopping mailman units ($glob):"
            # shellcheck disable=SC2086
            for u in $units; do
                echo "    $u"
                sysctl_user stop "$u" || true
                sysctl_user disable "$u" || true
            done
        else
            echo "==> no $glob units running (skip)"
        fi
    fi
done

# The central observer is shared by adapters, but its ExecStart points at the
# adapter binary that installed it most recently. If that binary is being
# removed while the sibling remains, repoint + restart instead of silently
# dropping fleet observation for the surviving adapter.
OBSERVER_UNIT="tmux-tell-mailman-observer.service"
OBSERVER_PATH="$USER_SYSTEMD/$OBSERVER_UNIT"
if [[ -f "$OBSERVER_PATH" ]] && grep -Fq "${PREFIX}/bin/${BIN_NAME} observe-mailmen" "$OBSERVER_PATH"; then
    if [[ "$ADAPTER" == "claude" ]]; then
        SIBLING_BIN="tmux-tell-codex"
    else
        SIBLING_BIN="tmux-tell-claude"
    fi
    if [[ -x "${PREFIX}/bin/${SIBLING_BIN}" ]]; then
        echo "==> repointing shared mailman observer to surviving $SIBLING_BIN"
        OBSERVER_TMP=$(mktemp)
        sed "s|${PREFIX}/bin/${BIN_NAME} observe-mailmen|${PREFIX}/bin/${SIBLING_BIN} observe-mailmen|" \
            "$OBSERVER_PATH" > "$OBSERVER_TMP"
        if [[ "$SYSTEM" -eq 1 ]]; then
            install -o "$OPERATOR_USER" -g "$OPERATOR_USER" -m 0644 "$OBSERVER_TMP" "$OBSERVER_PATH"
        else
            install -m 0644 "$OBSERVER_TMP" "$OBSERVER_PATH"
        fi
        rm -f "$OBSERVER_TMP"
        sysctl_user daemon-reload
        sysctl_user restart "$OBSERVER_UNIT"
    else
        echo "==> stopping shared mailman observer (no sibling adapter remains)"
        sysctl_user disable --now "$OBSERVER_UNIT" || true
        rm -f "$OBSERVER_PATH"
    fi
fi

# 2. Remove the systemd user template(s) + reload the manager so it forgets them.
# The canonical template is tmux-tell-<adapter>-mailman@.service; claude also has
# the legacy claude-mailman@.service symlink alias. A `-L` check catches the
# alias even after the canonical target it points at is removed (dangling).
TEMPLATES=("$USER_SYSTEMD/${UNIT_PREFIX}.service")
if [[ -n "$LEGACY_UNIT" ]]; then
    TEMPLATES+=("$USER_SYSTEMD/$LEGACY_UNIT")
fi
removed_template=0
for tpl in "${TEMPLATES[@]}"; do
    if [[ -e "$tpl" || -L "$tpl" ]]; then
        echo "==> removing $tpl"
        rm -f "$tpl"
        removed_template=1
    else
        echo "==> $tpl not present (skip)"
    fi
done
if [[ "$removed_template" -eq 1 || ! -e "$OBSERVER_PATH" ]]; then
    sysctl_user daemon-reload || true
fi

# 3. Remove the binary + its deprecation aliases. Canonical is
# tmux-tell-<adapter>; aliases are tmux-msg-<adapter> (+ claude-msg for claude),
# each a symlink — the `-L` check removes a dangling alias too.
BINS=("${PREFIX}/bin/${BIN_NAME}")
for legacy in "${LEGACY_BINS[@]}"; do
    BINS+=("${PREFIX}/bin/${legacy}")
done
for bin in "${BINS[@]}"; do
    if [[ -e "$bin" || -L "$bin" ]]; then
        echo "==> removing $bin"
        rm -f "$bin"
    else
        echo "==> $bin not present (skip)"
    fi
done

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
echo "  /etc/tmux-tell/            — host-level config (#54; pre-rename: /etc/tmux-msg/)"
echo "  ~/.claude.json                  — MCP entry; remove with:"
echo "                                    claude mcp remove tmux-tell -s user"
echo "                                    (or 'tmux-msg' on a pre-rename / legacy chamber)"
echo "  loginctl enable-linger          — other services may need it"
if ! $PURGE_DATA && [[ -d "$DATADIR" ]]; then
    echo "  ${DATADIR}    — SQLite message history (re-run with --purge to wipe)"
fi
