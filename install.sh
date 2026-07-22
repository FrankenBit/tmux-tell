#!/usr/bin/env bash
# Idempotent installer for tmux-tell on alcatraz-like Linux hosts.
#
# Default is a USER-SPACE install — no root, no sudo (#636, the
# adopter-trust default): run `./install.sh [--adapter=claude]` as your
# normal user. It installs the tmux-tell-<adapter> binary to ${PREFIX}/bin/
# (default ${HOME}/.local/bin, owned by you) and drops the systemd user
# template into ~/.config/systemd/user/. Reverting is a plain `rm` of the
# user-space binary + template (see README → uninstall). The whole point:
# a stranger can clone + `./install.sh` to see what the tool does without
# granting root to an unfamiliar binary.
#
# Pass `--system` for the historical root install — binary root-owned
# under /usr/local/bin, in the system PATH for all users (sudo -A
# ./install.sh --system). That mode is byte-for-byte the old behavior and
# is what alcatraz's deploy chain uses. The script:
#   - installs the tmux-tell-<adapter> binary to ${PREFIX}/bin/
#   - drops the systemd user template into the operator's
#     ~/.config/systemd/user/
#
# The DB lives under the operator's user-home (#308:
# $XDG_DATA_HOME/tmux-msg or ~/.local/share/tmux-msg/messages.db) and is
# created lazily by the binary on first open — no shared-space dir to
# create or chown at install time.
#   - for the claude adapter, also drops the deprecation-cycle aliases
#     claude-msg → tmux-msg-claude and claude-mailman@ → the new template
#     (#177 / ADR-0008; removed at v1.0 boundary per ADR-0008 §Discretion clause extension)
#
# The actual mailman enablement (`systemctl --user enable
# tmux-tell-claude-mailman@AGENT.service` followed by `systemctl --user
# restart tmux-tell-claude-mailman@AGENT.service`) is the operator's job —
# the install script makes no assumptions about which agents you want
# serviced. The `bootstrap` subcommand (#349 Fix 2, run automatically
# unless `--no-bootstrap` is passed) handles this for every registered
# non-hook-context agent: `enable` then `restart` per-mailman, because
# `enable --now` is a no-op on an already-active unit and would leave
# the mailman process running the deleted pre-install inode (#410).
#
# Re-running is safe: existing files are overwritten, the DB is never
# touched.
set -euo pipefail

# Install mode (#636). Default 0 = user-space install (no root); --system flips
# to 1 for the historical root install. PREFIX's default depends on it, so the
# default is resolved AFTER arg parsing (an explicit PREFIX= env override wins in
# either mode and is captured here).
SYSTEM=0
PREFIX=${PREFIX:-}

# Which adapter to install. The binary name encodes substrate+adapter
# (tmux-tell-<adapter>); `claude` is the only adapter today, but a future
# operator picks another once codex/copilot adapters exist (#177). Accept both
# --adapter=X and an ADAPTER=X env override.
ADAPTER=${ADAPTER:-claude}
# Bootstrap orchestration flags (#349 Fix 2). Default is to run the
# substrate-honest hard-cut after the binary install so an `install.sh`
# operator gets a fully-wired bus in one invocation. `--no-bootstrap`
# preserves the historical print-next-steps behavior for operators who
# want manual control. `--prune-orphans` is passed through to the
# bootstrap subcommand to actively disable orphan mailman units (default
# is print-only).
#
# For --adapter=codex, `--agent=NAME` is required (or set TMUX_AGENT_NAME
# in the environment). It identifies which codex chamber to configure the
# codex hook blocks for (#384).
BOOTSTRAP=1
PRUNE_ORPHANS=0
# --allow-stale-mailmen (#436 / Lookout #439): demote a post-install
# restart-mailmen failure from fatal to a warning. Default 0 = fatal, so the
# deploy chain fails loud rather than greening a stale-mailman state.
ALLOW_STALE_MAILMEN=0
AGENT_NAME=${AGENT_NAME:-}
for arg in "$@"; do
    case "$arg" in
        --adapter=*) ADAPTER="${arg#--adapter=}" ;;
        --agent=*)   AGENT_NAME="${arg#--agent=}" ;;
        --no-bootstrap) BOOTSTRAP=0 ;;
        --prune-orphans) PRUNE_ORPHANS=1 ;;
        --allow-stale-mailmen) ALLOW_STALE_MAILMEN=1 ;;
        --system) SYSTEM=1 ;;
        *) echo "install.sh: unknown argument: $arg (expected --adapter=NAME | --agent=NAME | --system | --no-bootstrap | --prune-orphans | --allow-stale-mailmen)" >&2; exit 1 ;;
    esac
done

# Resolve the PREFIX default now that the mode is known. --system installs into
# the system tree (/usr/local); the default user-space install lives under the
# invoking user's home. An explicit PREFIX= env override (captured above) wins in
# either mode.
if [[ -z "$PREFIX" ]]; then
    if [[ "$SYSTEM" -eq 1 ]]; then
        PREFIX=/usr/local
    else
        PREFIX="$HOME/.local"
    fi
fi
if [[ -z "$ADAPTER" || ! -d "$(dirname "$0")/cmd/tmux-tell-${ADAPTER}" ]]; then
    echo "install.sh: no adapter 'cmd/tmux-tell-${ADAPTER}/' in this repo." >&2
    exit 1
fi
# For codex adapter: resolve --agent from TMUX_AGENT_NAME if not passed
# explicitly, then require it (the codex bootstrap is per-agent, not a
# discover-all sweep like the claude bootstrap).
if [[ "$ADAPTER" == "codex" && "$BOOTSTRAP" -eq 1 && -z "$AGENT_NAME" ]]; then
    AGENT_NAME=${TMUX_AGENT_NAME:-}
fi
if [[ "$ADAPTER" == "codex" && "$BOOTSTRAP" -eq 1 && -z "$AGENT_NAME" ]]; then
    echo "install.sh: --agent=NAME required for --adapter=codex bootstrap (#384)" >&2
    echo "  Identifies which codex chamber to configure hook blocks for." >&2
    echo "  Pass --agent=<name> or run from a shell where TMUX_AGENT_NAME is set," >&2
    echo "  or skip automatic config with --no-bootstrap." >&2
    exit 1
fi
BIN_NAME="tmux-tell-${ADAPTER}"
UNIT_NAME="tmux-tell-${ADAPTER}-mailman@.service"

# Deprecation-cycle binary aliases (#177 / #440 Phase 3 / ADR-0008, removed at
# the v1.0 boundary). EVERY adapter now carries a tmux-msg-<adapter> alias from
# the substrate rename (so scripts / muscle-memory on the old name survive);
# claude additionally carries the older claude-msg name (#177). LEGACY_UNIT (the
# systemd template alias) stays claude-only — the unit rename is Phase 2's domain.
LEGACY_BINS=("tmux-msg-${ADAPTER}")
LEGACY_UNIT=""
if [[ "$ADAPTER" == "claude" ]]; then
    LEGACY_BINS+=("claude-msg")
    LEGACY_UNIT="claude-mailman@.service"
fi

# Resolve the operator user — the non-root account that owns the built bin/ +
# the installed systemd template, and runs the mailman daemons (the DB now lives
# under this user's home, created lazily by the binary). Precedence: an explicit
# OPERATOR_USER from the environment wins (lets you install for a target user
# without sudo, e.g. OPERATOR_USER=alice ./install.sh), then sudo's $SUDO_USER,
# then the invoking $USER. There is deliberately NO hardcoded fallback: guessing
# a username silently chowns the systemd template to the wrong (or nonexistent)
# account, contradicting the project's fail-loud ethos — and a personal username
# has no business shipping in a public installer.
OPERATOR_USER=${OPERATOR_USER:-${SUDO_USER:-${USER:-}}}
if [[ -z "$OPERATOR_USER" || "$OPERATOR_USER" == "root" ]]; then
    echo "install.sh: cannot determine the operator user (got: '${OPERATOR_USER}')." >&2
    echo "  Set OPERATOR_USER=<you> or run via sudo (which exports \$SUDO_USER)." >&2
    echo "  root is rejected: the mailman daemons must run as an unprivileged user." >&2
    exit 1
fi

if [[ "$SYSTEM" -eq 1 ]]; then
    # --system installs root-owned into the system tree; that needs root.
    if [[ $EUID -ne 0 ]]; then
        echo "install.sh: --system install must run as root (try: sudo -A ./install.sh --system)" >&2
        exit 1
    fi
else
    # The default user-space install writes only under the invoking user's home
    # ($HOME/.local/bin + ~/.config/systemd/user). Running it as root would
    # install into root's home (or chown the operator's files to root) — reject
    # rather than silently do the wrong thing. Use --system for a root install.
    if [[ $EUID -eq 0 ]]; then
        echo "install.sh: the default install is user-space and must NOT run as root." >&2
        echo "  Run it as your normal user (no sudo), or pass --system for a root/system-wide install." >&2
        exit 1
    fi
fi

# Resolve operator's home so we can install the systemd template there.
# `|| true` defangs `set -euo pipefail`: getent exits 2 when the user is not
# found, and `pipefail` propagates that — without the override the script
# would die silently on exit-2 before the friendly error below runs.
OPERATOR_HOME=$(getent passwd "$OPERATOR_USER" | cut -d: -f6 || true)
if [[ -z "$OPERATOR_HOME" ]]; then
    echo "install.sh: cannot resolve home dir for $OPERATOR_USER" >&2
    echo "  (getent passwd $OPERATOR_USER returned no entry — is the user spelled right?)" >&2
    exit 1
fi
# USER_SYSTEMD may be overridden (testing / bespoke layouts); defaults to the
# operator's standard user-unit dir.
USER_SYSTEMD="${USER_SYSTEMD:-$OPERATOR_HOME/.config/systemd/user}"

# Mode-keyed command prefixes + ownership flags (#636). In --system mode the
# script starts as root and drops to the operator for the non-privileged steps,
# and installs the binary root-owned; these arrays expand to EXACTLY the historical
# tokens, so --system behavior is byte-for-byte unchanged. In the default
# user-space mode we already ARE the operator, so the privilege-drop is a no-op
# (sudo -u $self would still demand sudo rights) and the chown flags are dropped
# (a non-root user can't chown, and files it creates are already its own).
#
#   DROP        — drop privileges for a plain operator command (rm / make / ln / systemctl)
#   DROP_TMUX   — same, preserving the tmux env the discover/bootstrap walkers need
#   BIN_OWNER   — ownership flags for the installed binary
#   OP_OWNER    — ownership flags for operator-owned files (systemd template + dirs)
if [[ "$SYSTEM" -eq 1 ]]; then
    DROP=(sudo -u "$OPERATOR_USER")
    # The commas in --preserve-env=A,B,C are part of a single sudo argument, not
    # array-element separators (SC2054 misreads them).
    # shellcheck disable=SC2054
    DROP_TMUX=(sudo -u "$OPERATOR_USER" --preserve-env=TMUX,TMUX_PANE,TMUX_TMPDIR)
    BIN_OWNER=(-o root -g root)
    OP_OWNER=(-o "$OPERATOR_USER" -g "$OPERATOR_USER")
else
    DROP=()
    DROP_TMUX=()
    BIN_OWNER=()
    OP_OWNER=()
fi

# enable_linger keeps the operator's user manager running across logout/reboot so
# the mailman daemons survive (#636). In --system mode it is a root operation and
# stays FATAL on failure — the deploy chain requires the operator's user manager
# reachable. In the default user-space mode, enabling linger for your own user may
# need polkit auth the adopter hasn't granted, and it is NOT required to try the
# tool now (the mailman runs in the live session regardless; linger only governs
# boot persistence). So user mode fails SOFT: try, and on denial print the
# one-line sudo follow-up instead of aborting the whole install (Option B).
if [[ "$SYSTEM" -eq 1 ]]; then
    enable_linger() {
        echo
        echo "==> enabling user-manager linger for $OPERATOR_USER"
        loginctl enable-linger "$OPERATOR_USER" || {
            echo "install.sh: loginctl enable-linger failed; bootstrap requires the operator's user manager to be reachable." >&2
            echo "  Re-run with --no-bootstrap if you want to handle this manually." >&2
            exit 1
        }
    }
else
    enable_linger() {
        echo
        echo "==> enabling user-manager linger for $OPERATOR_USER (best-effort)"
        if ! loginctl enable-linger "$OPERATOR_USER" 2>/dev/null; then
            echo "install.sh: could not enable lingering for $OPERATOR_USER without privilege — continuing." >&2
            echo "  The bus works now. For the mailman daemons to survive logout/reboot, run once:" >&2
            echo "    sudo loginctl enable-linger $OPERATOR_USER" >&2
        fi
    }
fi

cd "$(dirname "$0")"

# 1. Build the adapter binary. Always rebuild — a stale bin/$BIN_NAME from a
# prior `make build` at an older tag would otherwise silently install with the
# wrong embedded version (#342). The build goes through `make` so the
# Makefile's LDFLAGS apply (-X internal/version.Version=$(git describe ...));
# pre-#342 install.sh used plain `go build` here and the binary inherited the
# source-default for version (which was a hardcoded `v0.7.0` until the
# companion fix in internal/version/version.go flipped it to `"dev"`).
echo "==> building $BIN_NAME"
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
if ! command -v make >/dev/null 2>&1; then
    echo "install.sh: make not found; install.sh requires make for ldflags-stamped builds (#342)" >&2
    exit 1
fi
# Create bin/ owned by the operator — the build below runs as the operator, and
# a root-owned bin/ left from a prior --system install run would block its
# writes. `install -d` is idempotent and re-applies ownership on an existing dir,
# fixing a stale root-owned bin/ in place. (In user-space mode we are already the
# operator: OP_OWNER is empty and the build runs directly.)
install -d -m 0755 "${OP_OWNER[@]}" bin
# Force a rebuild even if sources didn't change: `git describe` may now report
# a different tag than the last build's embedded version, and make's
# source-dependency tracking wouldn't notice. Removing the target makes the
# pattern rule fire unconditionally.
"${DROP[@]}" rm -f "bin/$BIN_NAME"
"${DROP[@]}" make "bin/$BIN_NAME" GO="$GO"

# 2. Install binary (root-owned under --system; user-owned in the default
#    user-space install). World-readable+executable in both.
echo "==> installing $PREFIX/bin/$BIN_NAME"
# Ensure $PREFIX/bin exists. Guard on NON-existence: `install -d` re-applies
# ownership to an existing dir, so an unconditional call would re-chown an
# already-present dir on every run — in --system mode that drifts the existing
# root:root /usr/local/bin (a system PATH dir) to the operator + breaks the
# byte-identical guarantee (Surveyor #672). When the dir is missing (a fresh
# ~/.local/bin, or a custom PREFIX), create it with the binary's ownership.
[[ -d "$PREFIX/bin" ]] || install -d -m 0755 "${BIN_OWNER[@]}" "$PREFIX/bin"
install -m 0755 "${BIN_OWNER[@]}" "bin/$BIN_NAME" "$PREFIX/bin/$BIN_NAME"

# 2b. Deprecation-cycle binary aliases: tmux-msg-<adapter> (+ claude-msg for the
# claude adapter) → the canonical tmux-tell-<adapter>. A relative symlink target
# keeps each valid regardless of $PREFIX. Removed at the v1.0 boundary per
# ADR-0008 §Discretion clause extension (#177 / #440 Phase 3).
for legacy_bin in "${LEGACY_BINS[@]}"; do
    echo "==> deprecation alias $PREFIX/bin/$legacy_bin → $BIN_NAME (removed at v1.0 boundary)"
    ln -sfn "$BIN_NAME" "$PREFIX/bin/$legacy_bin"
done

# 3. (No data-directory step.) The DB lives under the operator's user-home
# ($XDG_DATA_HOME/tmux-msg or ~/.local/share/tmux-msg/messages.db, #308) and is
# created lazily by the binary on first open (store.Open MkdirAll's the parent).
# Nothing to create or chown at install time — the path is already owned by the
# operator by virtue of being under their home.

# 4. Install the systemd user template.
#
# The shipped template hardcodes ExecStart=/usr/local/bin/$BIN_NAME (so the
# `cp init/... ~/.config/systemd/user/` manual path documented in its header
# works for a system install). For any other $PREFIX — in particular the
# user-space default $HOME/.local — that absolute path is wrong and the unit
# would fail to start (#636). Rewrite the ExecStart bindir to $PREFIX/bin at
# install time. When PREFIX=/usr/local the sed is a no-op, so the installed
# file is byte-identical to the historical --system install.
echo "==> installing systemd user template $UNIT_NAME (ExecStart → $PREFIX/bin/$BIN_NAME)"
install -d -m 0755 "${OP_OWNER[@]}" "$USER_SYSTEMD"
UNIT_TMP=$(mktemp)
sed "s|^\(ExecStart=\)/usr/local/bin/|\1$PREFIX/bin/|" "init/$UNIT_NAME" > "$UNIT_TMP"
install -m 0644 "${OP_OWNER[@]}" "$UNIT_TMP" "$USER_SYSTEMD/$UNIT_NAME"
rm -f "$UNIT_TMP"

# Independent liveness observer (#808). This is one shared fleet service, not
# one instance per adapter: whichever adapter installs last rewrites ExecStart
# to its binary, and the shared CLI resolves each registered agent's actual
# adapter before probing its unit. It stays dormant until [defaults]
# mailman-alert-to is configured.
OBSERVER_UNIT="tmux-tell-mailman-observer.service"
OBSERVER_TMP=$(mktemp)
sed -e "s|/usr/local/bin/tmux-tell-claude|$PREFIX/bin/$BIN_NAME|" \
    "init/$OBSERVER_UNIT" > "$OBSERVER_TMP"
install -m 0644 "${OP_OWNER[@]}" "$OBSERVER_TMP" "$USER_SYSTEMD/$OBSERVER_UNIT"
rm -f "$OBSERVER_TMP"

# 4b. Deprecation-cycle systemd template alias: claude-mailman@ → the new
# template. systemd resolves a template-unit symlink, so a pre-rename
# `systemctl --user … claude-mailman@AGENT` still instantiates the renamed
# template with the same instance name. Owned by the operator. Removed at v1.0 boundary per ADR-0008 §Discretion clause extension.
if [[ -n "$LEGACY_UNIT" ]]; then
    echo "==> deprecation alias $USER_SYSTEMD/$LEGACY_UNIT → $UNIT_NAME (removed at v1.0 boundary)"
    "${DROP[@]}" ln -sfn "$UNIT_NAME" "$USER_SYSTEMD/$LEGACY_UNIT"
fi

# 4c. Phase-2 rename migration (#440): if the legacy tmux-msg-<adapter>-mailman@
# template + active instances still exist on this host, stop+disable each active
# instance and re-enable+restart the equivalent tmux-tell-<adapter>-mailman@
# instance, then remove the legacy template. Without this step both mailmen
# would poll the same DB → #443 Obs1 dual-delivery shape. The migration is a
# one-shot: on a host with no legacy mailmen this whole block is a no-op.
OPERATOR_UID=$(id -u "$OPERATOR_USER")
LEGACY_RENAME_PREFIX="tmux-msg-${ADAPTER}-mailman@"
LEGACY_RENAME_TEMPLATE="${LEGACY_RENAME_PREFIX}.service"
LEGACY_RENAME_TEMPLATE_PATH="${USER_SYSTEMD}/${LEGACY_RENAME_TEMPLATE}"
LEGACY_RENAME_ACTIVE=$("${DROP[@]}" env \
    XDG_RUNTIME_DIR="/run/user/$OPERATOR_UID" \
    DBUS_SESSION_BUS_ADDRESS="unix:path=/run/user/$OPERATOR_UID/bus" \
    systemctl --user list-units --type=service --state=active --plain --no-legend \
    "${LEGACY_RENAME_PREFIX}*.service" 2>/dev/null \
    | awk -v p="$LEGACY_RENAME_PREFIX" '{ n=$1; sub(p,"",n); sub(/\.service$/,"",n); print n }')

if [[ -n "$LEGACY_RENAME_ACTIVE" || -f "$LEGACY_RENAME_TEMPLATE_PATH" ]]; then
    echo "==> Phase 2 rename migration: tmux-msg-${ADAPTER}-* → ${BIN_NAME}-* (#440)"
    for AGENT in $LEGACY_RENAME_ACTIVE; do
        LEGACY_FOR_AGENT="${LEGACY_RENAME_PREFIX}${AGENT}.service"
        NEW_FOR_AGENT="${BIN_NAME}-mailman@${AGENT}.service"
        echo "   - $AGENT: stop+disable $LEGACY_FOR_AGENT, enable+restart $NEW_FOR_AGENT"
        # Stop the legacy unit first so it releases the tmux pane + DB pollers
        # BEFORE the new mailman binds them. `|| true` keeps an already-stopped
        # unit from breaking the migration.
        "${DROP[@]}" env \
            XDG_RUNTIME_DIR="/run/user/$OPERATOR_UID" \
            DBUS_SESSION_BUS_ADDRESS="unix:path=/run/user/$OPERATOR_UID/bus" \
            systemctl --user stop "$LEGACY_FOR_AGENT" 2>/dev/null || true
        "${DROP[@]}" env \
            XDG_RUNTIME_DIR="/run/user/$OPERATOR_UID" \
            DBUS_SESSION_BUS_ADDRESS="unix:path=/run/user/$OPERATOR_UID/bus" \
            systemctl --user disable "$LEGACY_FOR_AGENT" 2>/dev/null || true
        "${DROP[@]}" env \
            XDG_RUNTIME_DIR="/run/user/$OPERATOR_UID" \
            DBUS_SESSION_BUS_ADDRESS="unix:path=/run/user/$OPERATOR_UID/bus" \
            systemctl --user enable "$NEW_FOR_AGENT"
        "${DROP[@]}" env \
            XDG_RUNTIME_DIR="/run/user/$OPERATOR_UID" \
            DBUS_SESSION_BUS_ADDRESS="unix:path=/run/user/$OPERATOR_UID/bus" \
            systemctl --user restart "$NEW_FOR_AGENT"
    done
    if [[ -f "$LEGACY_RENAME_TEMPLATE_PATH" ]]; then
        echo "   - removing legacy template $LEGACY_RENAME_TEMPLATE_PATH"
        "${DROP[@]}" rm -f "$LEGACY_RENAME_TEMPLATE_PATH"
        "${DROP[@]}" env \
            XDG_RUNTIME_DIR="/run/user/$OPERATOR_UID" \
            DBUS_SESSION_BUS_ADDRESS="unix:path=/run/user/$OPERATOR_UID/bus" \
            systemctl --user daemon-reload
    fi
fi

# The observer is fleet-wide and independent of any one mailman. Enable it on
# every install; repeated adapter installs simply restart the same unit onto
# the newest binary. With no [defaults] mailman-alert-to it remains live but
# dormant, polling only the small config file until activated.
echo "==> enabling independent mailman observer (#808)"
"${DROP[@]}" env \
    HOME="$OPERATOR_HOME" \
    XDG_RUNTIME_DIR="/run/user/$OPERATOR_UID" \
    DBUS_SESSION_BUS_ADDRESS="unix:path=/run/user/$OPERATOR_UID/bus" \
    systemctl --user daemon-reload
"${DROP[@]}" env \
    HOME="$OPERATOR_HOME" \
    XDG_RUNTIME_DIR="/run/user/$OPERATOR_UID" \
    DBUS_SESSION_BUS_ADDRESS="unix:path=/run/user/$OPERATOR_UID/bus" \
    systemctl --user enable --now "$OBSERVER_UNIT"

echo
echo "Install complete."

# 5. Bootstrap (#349 Fix 2). Substrate-honest hard-cut: run discover +
# enable per-agent mailmen + walk orphan systemd units + refresh chamber
# MCPs as one orchestrated pass, instead of printing a manual ritual the
# operator must remember. The bootstrap subcommand handles the
# stale-DB-detect step too (offers `db migrate` from the pre-#308
# system-global path if it's the only DB present; aborts if both
# legacy + default exist). The `--no-bootstrap` escape hatch keeps the
# historical print-next-steps behavior available.
if [[ "$BOOTSTRAP" -eq 0 ]]; then
    echo
    # #436: a freshly-installed binary does NOT take effect on an already-
    # running mailman — the daemon holds the replaced inode until restarted
    # (the #393 lesson). The bootstrap path restarts mailmen per-agent; the
    # --no-bootstrap path (which the release deploy chain uses for the codex
    # adapter) must do the equivalent or the new binary stays inert. Restart
    # the adapter's running mailmen via the standalone primitive; no-op when
    # none run. Runs as the operator (systemctl --user needs their session bus).
    # (OPERATOR_UID was computed during the Phase-2 migration block above.)
    echo "==> restarting running $ADAPTER mailmen onto the new binary (#436)"
    # FATAL-BY-DEFAULT (Lookout #439 containment review): the substrate-claim
    # of this path is "the new binary is EFFECTIVE." A restart failure breaks
    # that — the binary is on disk but running mailmen hold the old inode. The
    # deploy chain calls this WITHOUT --allow-stale-mailmen, so a restart
    # failure must fail the deploy LOUD rather than green a stale-mailman state
    # (the exact green-but-incomplete shape #436 exists to kill — the smoke's
    # `--version` only proves file-on-disk, not running-process effectiveness).
    # --allow-stale-mailmen is the explicit manual-operator opt-out for
    # debug/transient-failure scenarios; it demotes the failure to a warning.
    if ! "${DROP[@]}" \
        env \
            HOME="$OPERATOR_HOME" \
            XDG_RUNTIME_DIR="/run/user/$OPERATOR_UID" \
            DBUS_SESSION_BUS_ADDRESS="unix:path=/run/user/$OPERATOR_UID/bus" \
            "$PREFIX/bin/$BIN_NAME" restart-mailmen; then
        if [[ "$ALLOW_STALE_MAILMEN" -eq 1 ]]; then
            echo "install.sh: restart-mailmen failed; --allow-stale-mailmen set → continuing. The binary is installed but some mailmen may still run the old inode (rerun '$BIN_NAME restart-mailmen' as $OPERATOR_USER to converge)." >&2
        else
            echo "install.sh: restart-mailmen FAILED — the new binary is on disk but running mailmen still hold the OLD inode, so this install is NOT effective (#436). Fix the systemctl error above and rerun, or pass --allow-stale-mailmen to proceed anyway." >&2
            exit 1
        fi
    fi
    echo

    # #797 Option A subset: after restart-mailmen succeeds, autonomously
    # fire `refresh-all-mcps` to converge chamber-side MCP subprocesses
    # against the new binary — BUT only when the operator's tmux server
    # is reachable, and NON-FATAL on failure (the safety net is doctor's
    # exit-71 idle-stale-only class in .forgejo/workflows/deploy.yml).
    #
    # Two-path substrate detail (surfaced during #797 design): refresh-all-mcps
    # is per-provider. Claude chambers enqueue via pure-DB control-macro
    # rows — needs NO tmux visibility. Codex chambers respawn via
    # `tmux respawn-pane` — REQUIRES tmux visibility. The probe below
    # (`sudo -u OP tmux list-sessions`) covers both: claude fan-out works
    # when the probe passes (probe passing == tmux server exists), codex
    # fan-out works too (same visibility surface). When the probe fails
    # (no tmux server on the operator's default socket — headless deploy
    # or first-time install), skip refresh-all-mcps and fall through to
    # the echoed manual next-steps: the idle-stale-only class then persists
    # until the operator's next login, which doctor's exit-71 surfaces as
    # a warn in the deploy workflow (not a hard fail).
    #
    # Non-fatal by design: refresh-all-mcps exits non-zero if ANY per-agent
    # fan-out failed. In runner context an operator may have codex chambers
    # whose panes have already exited (transient chamber crash), yielding
    # partial-failure. Failing the install on that would defeat the point
    # of --no-bootstrap (deploy chain proceeds; substrate follow-up is
    # doctor's job on the next post-deploy-smoke step). Warn + continue.
    if "${DROP_TMUX[@]}" \
        env HOME="$OPERATOR_HOME" \
            XDG_RUNTIME_DIR="/run/user/$OPERATOR_UID" \
            tmux list-sessions >/dev/null 2>&1; then
        echo "==> refresh-all-mcps (autonomous chamber-MCP refresh, #797)"
        if ! "${DROP_TMUX[@]}" \
            env HOME="$OPERATOR_HOME" \
                XDG_RUNTIME_DIR="/run/user/$OPERATOR_UID" \
                DBUS_SESSION_BUS_ADDRESS="unix:path=/run/user/$OPERATOR_UID/bus" \
                "$PREFIX/bin/$BIN_NAME" refresh-all-mcps; then
            echo "install.sh: refresh-all-mcps had partial failure (details above) — chamber-side MCPs may still be stale. Non-fatal: doctor's #797 exit-71 idle-stale-only class in the deploy workflow will surface residual stale MCPs as a ::warning::. Run '$BIN_NAME refresh-all-mcps' as $OPERATOR_USER from a tmux session to converge." >&2
        fi
        echo
    else
        echo "==> refresh-all-mcps SKIPPED — no reachable tmux server on $OPERATOR_USER's default socket (#797 Option A: probe failed)"
        echo "    Rely on the operator's next-login manual run OR doctor's exit-71 idle-stale-only class to surface any residual stale MCPs."
        echo
    fi

    if [[ "$ADAPTER" == "codex" ]]; then
        echo "Next steps for codex (--no-bootstrap chosen; run as $OPERATOR_USER, NOT as root):"
        echo "  # PASTE-SERVED chamber (the #360 default — codex runs a mailman like claude):"
        echo "  sudo loginctl enable-linger $OPERATOR_USER"
        echo "  $BIN_NAME discover"
        echo "  systemctl --user enable ${UNIT_NAME%@.service}@<agent>.service"
        echo "  systemctl --user restart ${UNIT_NAME%@.service}@<agent>.service"
        echo "  # OR a HOOK-CONTEXT chamber (no mailman; delivers via the UserPromptSubmit hook):"
        echo "  $BIN_NAME register --name <agent> --delivery-mode=hook-context"
        echo "  $BIN_NAME codex-install --agent=<agent>   # writes hook blocks"
        echo "  # then manually approve hooks in Codex on next launch:"
        echo "  #   UserPromptSubmit: tmux-tell-codex hook-context"
        echo "  #   SessionStart:     tmux-tell-codex hook-context"
    else
        echo "Next steps (--no-bootstrap chosen; run as $OPERATOR_USER, NOT as root):"
        echo "  systemctl --user daemon-reload"
        echo "  # ensure your user systemd manager runs at boot:"
        echo "  sudo loginctl enable-linger $OPERATOR_USER"
        echo "  # populate the agents table from the current tmux state:"
        echo "  $BIN_NAME discover"
        echo "  # enable + restart a mailman per agent you want to receive"
        echo "  # messages (restart is needed if the unit was already active —"
        echo "  # \`enable --now\` is a no-op then, leaving the deleted-inode"
        echo "  # binary running per #410):"
        echo "  systemctl --user enable ${UNIT_NAME%@.service}@surveyor.service"
        echo "  systemctl --user restart ${UNIT_NAME%@.service}@surveyor.service"
        echo "  # refresh chamber MCPs against the freshly-installed binary:"
        echo "  $BIN_NAME refresh-all-mcps"
    fi
    exit 0
fi

# Bootstrap path. enable-linger is a root operation needed by BOTH adapters:
# since #360 codex IS paste-capable with systemd mailman daemons (e.g. Lookout),
# so a paste-served codex chamber needs linger + a mailman exactly like claude.
# Only a hook-context codex chamber delivers via the UserPromptSubmit hook and
# runs no mailman. The codex branch below branches on the agent's CURRENT
# delivery_mode and does NOT force hook-context (#438). The rest runs as the
# operator (tmux + D-Bus). (OPERATOR_UID computed in the Phase-2 migration block above.)

if [[ "$ADAPTER" == "codex" ]]; then
    # Codex bootstrap is per-agent (#384) and paste-aware (#438): enable-linger
    # so a paste-served codex mailman persists at boot (same as claude), then
    # discover + branch on the agent's CURRENT delivery_mode. NEVER force-flip it
    # — the pre-#438 path always ran codex-install, whose step 2 flipped a
    # paste-served chamber (Lookout) back to hook-context, re-creating the
    # #443 Obs1 stale-hook duplicate-delivery.
    enable_linger

    # Discover (populates $AGENT_NAME; a fresh agent defaults to paste-and-enter
    # per #360) + read its delivery_mode via whoami's MODE line (no jq). Runs as
    # the operator: discover needs TMUX*, whoami reads the same DB via HOME.
    echo "==> discovering codex panes + resolving delivery_mode for '$AGENT_NAME'"
    "${DROP_TMUX[@]}" \
        env HOME="$OPERATOR_HOME" \
            "$PREFIX/bin/$BIN_NAME" discover >/dev/null
    CODEX_MODE=$("${DROP[@]}" \
        env HOME="$OPERATOR_HOME" \
            "$PREFIX/bin/$BIN_NAME" whoami --as "$AGENT_NAME" --format=text \
        | awk -F'\t' '$1 == "MODE" { print $2 }')

    if [[ -z "$CODEX_MODE" ]]; then
        echo "install.sh: agent '$AGENT_NAME' not found after discover — ensure the codex pane is in the current tmux session with TMUX_AGENT_NAME=$AGENT_NAME set, or run '$BIN_NAME register --name $AGENT_NAME --delivery-mode=...' first." >&2
        exit 1
    fi

    case "$CODEX_MODE" in
    hook-context)
        # Deliberate hook-context chamber: write hook blocks. The mode
        # is already hook-context so codex-install's step 2 is a no-op (no flip).
        # No mailman — hook-context delivers via the UserPromptSubmit hook.
        echo "==> '$AGENT_NAME' is hook-context → codex-install (hook config)"
        "${DROP_TMUX[@]}" \
            env HOME="$OPERATOR_HOME" \
                "$PREFIX/bin/$BIN_NAME" codex-install \
                    --agent="$AGENT_NAME"
        ;;
    paste-and-enter)
        # Paste-served chamber (the #360 default): enable + restart its mailman,
        # exactly like claude (#410's enable-then-restart so an already-running
        # mailman picks up the freshly-installed inode). delivery_mode preserved;
        # no hook blocks written (writing them would re-create the #443 Obs1
        # stale-hook condition). MCP-env wiring for a fresh paste-served codex
        # chamber is tracked separately (#453).
        MAILMAN_UNIT="${UNIT_NAME%@.service}@${AGENT_NAME}.service"
        echo "==> '$AGENT_NAME' is paste-and-enter → enable + restart $MAILMAN_UNIT (mode preserved)"
        "${DROP[@]}" \
            env HOME="$OPERATOR_HOME" \
                XDG_RUNTIME_DIR="/run/user/$OPERATOR_UID" \
                DBUS_SESSION_BUS_ADDRESS="unix:path=/run/user/$OPERATOR_UID/bus" \
                systemctl --user enable "$MAILMAN_UNIT"
        "${DROP[@]}" \
            env HOME="$OPERATOR_HOME" \
                XDG_RUNTIME_DIR="/run/user/$OPERATOR_UID" \
                DBUS_SESSION_BUS_ADDRESS="unix:path=/run/user/$OPERATOR_UID/bus" \
                systemctl --user restart "$MAILMAN_UNIT"
        ;;
    *)
        # mailbox-only (operator polls the inbox; register defaults start_mailman
        # =false) or any future mode: NO mailman + NO hooks. Enabling a mailman
        # here would contradict the mailbox-only contract (Surveyor #455 nit).
        # The agent is registered (discover ran); nothing else to wire.
        echo "==> '$AGENT_NAME' is $CODEX_MODE → no mailman / no hooks (operator-polled); nothing to wire"
        ;;
    esac

    echo
    echo "Codex bootstrap complete."
else
    enable_linger

    # Drop privileges to the operator + thread through the env the bootstrap
    # subcommand needs: HOME for the systemd-dir derivation and DB resolution,
    # XDG_RUNTIME_DIR + DBUS_SESSION_BUS_ADDRESS for `systemctl --user`, TMUX*
    # (best-effort) for the discover walker.
    BOOTSTRAP_FLAGS=()
    if [[ "$PRUNE_ORPHANS" -eq 1 ]]; then
        BOOTSTRAP_FLAGS+=(--prune-orphans)
    fi

    echo "==> running bootstrap (discover + mailman enable + orphan walk + refresh)"
    "${DROP_TMUX[@]}" \
        env \
            HOME="$OPERATOR_HOME" \
            XDG_RUNTIME_DIR="/run/user/$OPERATOR_UID" \
            DBUS_SESSION_BUS_ADDRESS="unix:path=/run/user/$OPERATOR_UID/bus" \
            "$PREFIX/bin/$BIN_NAME" bootstrap "${BOOTSTRAP_FLAGS[@]}"

    echo
    echo "Bootstrap complete. The bus is wired."
fi
