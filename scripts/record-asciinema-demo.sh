#!/usr/bin/env bash
# scripts/record-asciinema-demo.sh
#
# Unattended driver for the observe-gate asciinema demo (#216 / #273).
# See docs/asciinema-capture.md for the recipe this script implements —
# the recipe is the spec (with the editorial rationale); this script is
# the executable form.
#
# Usage:
#   ./scripts/record-asciinema-demo.sh
#
# Overrides (env vars):
#   CAST          output path for the .cast  (default: docs/asciinema/observe-gate.cast)
#   TYPING_DELAY  per-character delay in seconds (default: 0.15 — human pace)
#   PRE_SEND_DELAY   seconds to type before firing the bus send (default: 2.0)
#   POST_SEND_TYPING seconds to keep typing after the send fires (default: 2.0)
#   POST_LAND_WAIT   seconds to let the message visually settle after delivery (default: 2)
#
# Prerequisites:
#   - asciinema >= 2.4 (apt install asciinema)
#   - tmux-msg-claude >= v0.13.0 on PATH
#   - tmux on PATH
#   - A claude binary on PATH (real Claude Code — the observe-gate classifier
#     requires the ❯ sentinel; a generic shell pane classifies as StateUnknown
#     and never shows the gate hold dynamics)
#
# Substrate-honest disclosure: bob's pane runs a real claude session. A ~30s
# take costs a trivial ~few-k tokens but shows the actual chamber experience
# the README sells — the gate dynamics only fire with a real Claude prompt.
#
# Idempotent: can be re-run from any prior state. The cleanup() trap resets
# all sandbox state on exit (normal or error).

set -euo pipefail

# ── Configuration ────────────────────────────────────────────────────────────

readonly SESSION="observe-gate-demo"
readonly DB="/tmp/observe-gate-demo.db"
readonly MAILMAN_PID_FILE="/tmp/observe-gate-demo-mailman.pid"
readonly MAILMAN_LOG="/tmp/observe-gate-demo-mailman.log"

# CAST: respect env override; default to the canonical path under the repo root.
CAST="${CAST:-$(git rev-parse --show-toplevel 2>/dev/null || pwd)/docs/asciinema/observe-gate.cast}"
readonly CAST

# Editorial choices settled in Herald's #216 PR #263 editorial pass.
readonly MSG_BODY="the API changed, look at what I just pushed"
readonly PROMPT_TEXT="is the auth header still set on the client"

# Stale threshold must match the mailman flag below. The gate fires ~STALE_THRESHOLD
# seconds after the operator stops typing; production default is 2min, which is
# too long for a ~15s clip.
readonly STALE_THRESHOLD=3

# Timing knobs — defaults calibrated for alcatraz. Adjust if the take reads rushed
# or the 📫 doesn't have time to settle before the gate fires.
TYPING_DELAY="${TYPING_DELAY:-0.15}"       # per-character; mimics deliberate human pace
PRE_SEND_DELAY="${PRE_SEND_DELAY:-2.0}"    # type this long before firing the send
POST_SEND_TYPING="${POST_SEND_TYPING:-2.0}" # keep typing this long after send fires
POST_LAND_WAIT="${POST_LAND_WAIT:-2}"      # seconds for message to visually settle
readonly TYPING_DELAY PRE_SEND_DELAY POST_SEND_TYPING POST_LAND_WAIT

# ── Cleanup (idempotent — safe to call multiple times) ────────────────────────

cleanup() {
    echo "[cleanup] tearing down sandbox..."
    # Kill the demo mailman by recorded PID only — never pkill the binary name,
    # which would also kill production mailmen on the same host.
    if [ -f "$MAILMAN_PID_FILE" ]; then
        kill "$(cat "$MAILMAN_PID_FILE")" 2>/dev/null || true
        rm -f "$MAILMAN_PID_FILE"
    fi
    # Kill ONLY the demo session — the operator's crew session lives on the same
    # default socket and must survive.
    tmux kill-session -t "$SESSION" 2>/dev/null || true
    rm -f "$DB" "$MAILMAN_LOG"
}
trap cleanup EXIT

# ── Phase 1: pre-clean any prior state ───────────────────────────────────────

echo "[1/6] pre-clean prior sandbox state"
cleanup
# Re-arm trap after the manual cleanup call (trap resets on EXIT, but we want it
# active for the rest of the script too).
trap cleanup EXIT

# ── Phase 2: sandbox env + tmux session ──────────────────────────────────────

echo "[2/6] sandbox env + tmux session"
export COLUMNS=120 LINES=30
export CLAUDE_MSG_DB="$DB"

tmux new-session -d -s "$SESSION" -x 120 -y 30
# Split into left (alice/sender) and right (bob/recipient) panes.
tmux split-window -h -t "$SESSION"

ALICE_PANE=$(tmux list-panes -t "$SESSION" -F '#{pane_id} #{pane_index}' | awk '$2==1 {print $1}')
BOB_PANE=$(  tmux list-panes -t "$SESSION" -F '#{pane_id} #{pane_index}' | awk '$2==2 {print $1}')
echo "  alice=$ALICE_PANE  bob=$BOB_PANE"

# Launch real Claude Code in bob's pane. The observe-gate classifier requires the
# ❯ sentinel (internal/tmuxio/state.go AgentState branch); a non-Claude shell pane
# classifies as StateUnknown and the gate never shows the hold dynamics.
tmux send-keys -t "$BOB_PANE" 'claude' Enter
echo "  waiting 8s for Claude to reach its first idle prompt..."
sleep 8

# ── Phase 3: register agents + start bob's mailman ───────────────────────────

echo "[3/6] register agents + start bob's mailman"
tmux-msg-claude register --name alice --pane "$ALICE_PANE" --start-mailman=false >/dev/null
tmux-msg-claude register --name bob   --pane "$BOB_PANE"   --start-mailman=false >/dev/null

# Demo-tuned cadence (recipe §Step 3): 3s stale-threshold + 500ms–2s poll so the
# gate fires within the ~15s clip. Production defaults (2min stale, 3–15s poll)
# would leave the viewer staring at a frozen frame.
nohup tmux-msg-claude serve --agent bob \
    --drift-soft-fail \
    --input-stale-threshold "${STALE_THRESHOLD}s" \
    --poll-interval-min 500ms \
    --poll-interval-max 2s \
    > "$MAILMAN_LOG" 2>&1 &
echo "$!" > "$MAILMAN_PID_FILE"

sleep 1
echo "  mailman log (first 3 lines):"
head -3 "$MAILMAN_LOG" 2>/dev/null | sed 's/^/    /'

# ── Phase 4: start asciinema recording ───────────────────────────────────────

echo "[4/6] start asciinema recording"
mkdir -p "$(dirname "$CAST")"

# --command attaches to the tmux session inside the recording shell. When we kill
# the tmux session later, the attach client exits, asciinema's child exits, and
# the .cast is written. Recording starts OUTSIDE tmux so both panes + the divider
# + status bar are captured; recording inside a pane would capture only that PTY.
asciinema rec \
    --title "tmux-msg observe-gate" \
    --idle-time-limit=2 \
    --command "tmux attach -t $SESSION" \
    "$CAST" &
ASCIINEMA_PID=$!

# Give asciinema and the attach a moment to settle before we start the take.
sleep 2

# ── Phase 5: time-driven take ─────────────────────────────────────────────────

echo "[5/6] take begin — typing prompt in bob @ $(date +%H:%M:%S)"

# type_prompt sends text to a pane one character at a time with a human-pace delay.
# -l (literal) is critical: without it, spaces and punctuation are interpreted as
# tmux key chord prefixes (e.g. a space becomes a raw chord, apostrophes may trip
# the shell). -l forces byte-for-byte passthrough.
type_prompt() {
    local text="$1" pane="$2" delay="$3"
    local i char
    for (( i=0; i<${#text}; i++ )); do
        char="${text:i:1}"
        tmux send-keys -t "$pane" -l "$char"
        sleep "$delay"
    done
}

# Start the typing in the background so we can fire the bus send mid-keystroke.
type_prompt "$PROMPT_TEXT" "$BOB_PANE" "$TYPING_DELAY" &
TYPING_PID=$!

# Let the typist establish rhythm and get visible characters into the pane before
# firing the send. This ensures the gate classifies bob as StateAwaitingOperator
# (cursor past the ❯ sentinel) when the message arrives, triggering the 📫 hold.
sleep "$PRE_SEND_DELAY"

echo "  firing bus send @ $(date +%H:%M:%S)"
tmux-msg-claude send --from alice --to bob "$MSG_BODY" >/dev/null

# Keep typing for a beat after the send so the viewer sees the 📫 hold during
# active keystroke activity — the visual proof that the gate is waiting for a
# pause, not just the end of a character stream.
sleep "$POST_SEND_TYPING"

# Pause the typing — the gate now waits for the input-stale-threshold.
kill "$TYPING_PID" 2>/dev/null || true
wait "$TYPING_PID" 2>/dev/null || true

echo "  typing paused — waiting for stale-threshold (${STALE_THRESHOLD}s) + paste + settle"
# Gate fires within STALE_THRESHOLD seconds of the last keystroke; add POST_LAND_WAIT
# so the message text is fully visible in the pane before the recording stops.
sleep $(( STALE_THRESHOLD + POST_LAND_WAIT ))

# ── Phase 6: stop the recording ──────────────────────────────────────────────

echo "[6/6] stop recording @ $(date +%H:%M:%S)"
# Killing the session causes the attached client (inside asciinema's --command) to
# exit, which causes asciinema to write the .cast and exit.
tmux kill-session -t "$SESSION" 2>/dev/null || true
wait "$ASCIINEMA_PID" 2>/dev/null || true

echo ""
echo "[done] cast written to $CAST"
echo "       size: $(wc -c < "$CAST") bytes"
echo ""
echo "Verify the take:"
echo "  asciinema play $CAST"
echo ""
echo "Checklist (recipe §verify-the-take):"
echo "  [ ] 📫 indicator visibly holds during typing"
echo "  [ ] message body appears only AFTER the typing pause"
echo "  [ ] total length ≈ 15–30s"
echo "  [ ] terminal size 120×30"
echo ""
echo "If the take needs a retake, re-run this script — it pre-cleans sandbox state."

# trap fires cleanup on EXIT
