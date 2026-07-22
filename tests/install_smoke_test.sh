#!/usr/bin/env bash
# Smoke tests for install.sh (#349 Fix 2).
#
# Scope: exercises the parts of install.sh that exit BEFORE the EUID
# check fires — syntax, flag-parsing, adapter-validation — so the test
# can run without root + without sudo + without a real systemd-user
# session.
#
# What this test does NOT cover: the actual binary install, systemd
# unit drop, bootstrap path. Those need a tmux + systemd fixture and
# land as a separate integration suite if/when that fixture lives.
# The bootstrap subcommand's logic is unit-tested in
# internal/cli/bootstrap_test.go.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
INSTALL_SH="$SCRIPT_DIR/../install.sh"
FAILED=0

pass() { printf "  PASS: %s\n" "$1"; }
fail() { printf "  FAIL: %s\n" "$1" >&2; FAILED=$((FAILED + 1)); }

# 1. Bash syntax check.
if bash -n "$INSTALL_SH"; then
    pass "install.sh parses cleanly"
else
    fail "bash -n install.sh"
fi

# 2. Unknown flag exits non-zero with the expected error.
# Run in a subshell with a non-existent adapter so we hit the flag
# parser before the adapter check would fire.
output=$(bash "$INSTALL_SH" --bogus-flag 2>&1 || true)
if echo "$output" | grep -q "unknown argument"; then
    pass "unknown flag rejected"
else
    fail "unknown flag did not print 'unknown argument'; got: $output"
fi

# 3. Missing adapter directory surfaces a clear error (covers adapter
# resolution before EUID).
output=$(bash "$INSTALL_SH" --adapter=does-not-exist 2>&1 || true)
if echo "$output" | grep -q "no adapter"; then
    pass "missing adapter directory rejected"
else
    fail "missing adapter did not print 'no adapter'; got: $output"
fi

# 4. Sanity that the new flags surface in the help/usage text — guards
# against an accidental flag-removal regression.
if grep -q -- '--no-bootstrap' "$INSTALL_SH"; then
    pass "--no-bootstrap flag present in install.sh"
else
    fail "--no-bootstrap flag missing from install.sh"
fi

if grep -q -- '--prune-orphans' "$INSTALL_SH"; then
    pass "--prune-orphans flag present in install.sh"
else
    fail "--prune-orphans flag missing from install.sh"
fi

# 5. The bootstrap dispatch must thread DBUS_SESSION_BUS_ADDRESS +
# XDG_RUNTIME_DIR through to the operator-side `bootstrap` subcommand
# call — those are the env vars `systemctl --user` requires (#356).
if grep -q "DBUS_SESSION_BUS_ADDRESS" "$INSTALL_SH" && \
   grep -q "XDG_RUNTIME_DIR" "$INSTALL_SH"; then
    pass "DBUS + XDG vars threaded to operator-side bootstrap"
else
    fail "DBUS_SESSION_BUS_ADDRESS / XDG_RUNTIME_DIR missing from bootstrap dispatch"
fi

# 6. The bootstrap step invokes the binary's `bootstrap` subcommand.
if grep -q '"\$PREFIX/bin/\$BIN_NAME" bootstrap' "$INSTALL_SH"; then
    pass "install.sh invokes bootstrap subcommand"
else
    fail "install.sh does not invoke bootstrap subcommand"
fi

# 7. The observer must be RESTARTED, not just `enable --now`d (#828). A
# redeploy replaces the binary inode on disk, but `enable --now` is a no-op
# on an already-running unit, so the observer would keep the deleted inode and
# trip doctor exit 69 (mailman-stale) on the next deploy. Guards against a
# regression back to `enable --now "$OBSERVER_UNIT"` as the sole treatment.
if grep -q 'systemctl --user restart "\$OBSERVER_UNIT"' "$INSTALL_SH"; then
    pass "install.sh restarts the observer onto the fresh binary (#828)"
else
    fail "install.sh does not restart \$OBSERVER_UNIT — a redeploy would leave the observer on the stale inode (#828)"
fi

if [[ $FAILED -gt 0 ]]; then
    echo
    echo "$FAILED smoke test(s) failed."
    exit 1
fi

echo
echo "All install.sh smoke tests passed."
