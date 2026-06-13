#!/usr/bin/env bash
# Idempotent installer for tmux-msg on alcatraz-like Linux hosts.
#
# Run as root (sudo -A ./install.sh [--adapter=claude]). The script:
#   - installs the tmux-msg-<adapter> binary to ${PREFIX}/bin/
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
# The actual mailman enablement (`systemctl --user enable --now
# tmux-msg-claude-mailman@AGENT.service`) is the operator's job — the install
# script makes no assumptions about which agents you want serviced.
#
# Re-running is safe: existing files are overwritten, the DB is never
# touched.
set -euo pipefail

PREFIX=${PREFIX:-/usr/local}

# Which adapter to install. The binary name encodes substrate+adapter
# (tmux-msg-<adapter>); `claude` is the only adapter today, but a future
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
BOOTSTRAP=1
PRUNE_ORPHANS=0
for arg in "$@"; do
    case "$arg" in
        --adapter=*) ADAPTER="${arg#--adapter=}" ;;
        --no-bootstrap) BOOTSTRAP=0 ;;
        --prune-orphans) PRUNE_ORPHANS=1 ;;
        *) echo "install.sh: unknown argument: $arg (expected --adapter=NAME | --no-bootstrap | --prune-orphans)" >&2; exit 1 ;;
    esac
done
if [[ -z "$ADAPTER" || ! -d "$(dirname "$0")/cmd/tmux-msg-${ADAPTER}" ]]; then
    echo "install.sh: no adapter 'cmd/tmux-msg-${ADAPTER}/' in this repo." >&2
    exit 1
fi
BIN_NAME="tmux-msg-${ADAPTER}"
UNIT_NAME="tmux-msg-${ADAPTER}-mailman@.service"

# Deprecation-cycle aliases (#177 / ADR-0008). Only the claude adapter carries
# the legacy claude-msg / claude-mailman names; other adapters never had them.
# Empty → no alias installed.
LEGACY_BIN=""
LEGACY_UNIT=""
if [[ "$ADAPTER" == "claude" ]]; then
    LEGACY_BIN="claude-msg"
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

if [[ $EUID -ne 0 ]]; then
    echo "install.sh: must run as root (try: sudo -A ./install.sh)" >&2
    exit 1
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
# Create bin/ owned by the operator — the build below runs as OPERATOR_USER,
# and a root-owned bin/ left from a prior install run would block its writes.
# `install -d` is idempotent and re-applies ownership on an existing dir,
# fixing a stale root-owned bin/ in place.
install -d -m 0755 -o "$OPERATOR_USER" -g "$OPERATOR_USER" bin
# Force a rebuild even if sources didn't change: `git describe` may now report
# a different tag than the last build's embedded version, and make's
# source-dependency tracking wouldn't notice. Removing the target makes the
# pattern rule fire unconditionally.
sudo -u "$OPERATOR_USER" rm -f "bin/$BIN_NAME"
sudo -u "$OPERATOR_USER" make "bin/$BIN_NAME" GO="$GO"

# 2. Install binary (root-owned, world-readable+executable).
echo "==> installing $PREFIX/bin/$BIN_NAME"
install -m 0755 -o root -g root "bin/$BIN_NAME" "$PREFIX/bin/$BIN_NAME"

# 2b. Deprecation-cycle binary alias: claude-msg → tmux-msg-claude. A relative
# symlink target keeps it valid regardless of $PREFIX. Removed at v1.0 boundary per ADR-0008 §Discretion clause extension (#177).
if [[ -n "$LEGACY_BIN" ]]; then
    echo "==> deprecation alias $PREFIX/bin/$LEGACY_BIN → $BIN_NAME (removed at v1.0 boundary)"
    ln -sfn "$BIN_NAME" "$PREFIX/bin/$LEGACY_BIN"
fi

# 3. (No data-directory step.) The DB lives under the operator's user-home
# ($XDG_DATA_HOME/tmux-msg or ~/.local/share/tmux-msg/messages.db, #308) and is
# created lazily by the binary on first open (store.Open MkdirAll's the parent).
# Nothing to create or chown at install time — the path is already owned by the
# operator by virtue of being under their home.

# 4. Install the systemd user template.
echo "==> installing systemd user template $UNIT_NAME"
install -d -m 0755 -o "$OPERATOR_USER" -g "$OPERATOR_USER" "$USER_SYSTEMD"
install -m 0644 -o "$OPERATOR_USER" -g "$OPERATOR_USER" \
    "init/$UNIT_NAME" "$USER_SYSTEMD/$UNIT_NAME"

# 4b. Deprecation-cycle systemd template alias: claude-mailman@ → the new
# template. systemd resolves a template-unit symlink, so a pre-rename
# `systemctl --user … claude-mailman@AGENT` still instantiates the renamed
# template with the same instance name. Owned by the operator. Removed at v1.0 boundary per ADR-0008 §Discretion clause extension.
if [[ -n "$LEGACY_UNIT" ]]; then
    echo "==> deprecation alias $USER_SYSTEMD/$LEGACY_UNIT → $UNIT_NAME (removed at v1.0 boundary)"
    sudo -u "$OPERATOR_USER" ln -sfn "$UNIT_NAME" "$USER_SYSTEMD/$LEGACY_UNIT"
fi

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
    echo "Next steps (--no-bootstrap chosen; run as $OPERATOR_USER, NOT as root):"
    echo "  systemctl --user daemon-reload"
    echo "  # ensure your user systemd manager runs at boot:"
    echo "  sudo loginctl enable-linger $OPERATOR_USER"
    echo "  # populate the agents table from the current tmux state:"
    echo "  $BIN_NAME discover"
    echo "  # enable a mailman per agent you want to receive messages:"
    echo "  systemctl --user enable --now ${UNIT_NAME%@.service}@surveyor.service"
    echo "  # refresh chamber MCPs against the freshly-installed binary:"
    echo "  $BIN_NAME refresh-all-mcps"
    exit 0
fi

# Bootstrap path. enable-linger is a root operation; the rest runs as
# the operator with their tmux + D-Bus session.
echo
echo "==> enabling user-manager linger for $OPERATOR_USER"
loginctl enable-linger "$OPERATOR_USER" || {
    echo "install.sh: loginctl enable-linger failed; bootstrap requires the operator's user manager to be reachable." >&2
    echo "  Re-run with --no-bootstrap if you want to handle this manually." >&2
    exit 1
}

# Drop privileges to the operator + thread through the env the bootstrap
# subcommand needs: HOME for the systemd-dir derivation and DB resolution,
# XDG_RUNTIME_DIR + DBUS_SESSION_BUS_ADDRESS for `systemctl --user`, TMUX*
# (best-effort) for the discover walker.
OPERATOR_UID=$(id -u "$OPERATOR_USER")
BOOTSTRAP_FLAGS=()
if [[ "$PRUNE_ORPHANS" -eq 1 ]]; then
    BOOTSTRAP_FLAGS+=(--prune-orphans)
fi

echo "==> running bootstrap (discover + mailman enable + orphan walk + refresh)"
sudo -u "$OPERATOR_USER" \
    --preserve-env=TMUX,TMUX_PANE,TMUX_TMPDIR \
    env \
        HOME="$OPERATOR_HOME" \
        XDG_RUNTIME_DIR="/run/user/$OPERATOR_UID" \
        DBUS_SESSION_BUS_ADDRESS="unix:path=/run/user/$OPERATOR_UID/bus" \
        "$PREFIX/bin/$BIN_NAME" bootstrap "${BOOTSTRAP_FLAGS[@]}"

echo
echo "Bootstrap complete. The bus is wired."
