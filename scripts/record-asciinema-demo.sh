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
# Environment notes:
#   - Trust prompt: Claude's "trust this folder?" dialog is detected and dismissed
#     automatically — no pre-trust of the working directory is required.
#   - Terminal size: asciinema is invoked with --cols 120 --rows 30 so the cast
#     dimensions are fixed regardless of the calling terminal's size; COLUMNS/LINES
#     env vars are not sufficient when asciinema is not connected to a real pty.
#
# Substrate-honest disclosure: bob's pane runs a real claude session. A ~30s
# take costs a trivial ~few-k tokens but shows the actual chamber experience
# the README sells — the gate dynamics only fire with a real Claude prompt.
#
# Idempotent: can be re-run from any prior state. The cleanup() trap resets
# all sandbox state on exit (normal or error).

set -euo pipefail
unset TMUX TMUX_PANE

# ── Configuration ────────────────────────────────────────────────────────────

readonly SESSION="observe-gate-demo"
readonly DB="/tmp/observe-gate-demo.db"
readonly MAILMAN_PID_FILE="/tmp/observe-gate-demo-mailman.pid"
readonly MAILMAN_LOG="/tmp/observe-gate-demo-mailman.log"
readonly TMUXDIR="/tmp/observe-gate-demo-tmux"

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

# Isolate the recording on a private tmux server BEFORE any cleanup runs, so the
# pre-clean kill-session and the in-script `tmux` calls all land on the sandbox
# server rather than the operator's main one — defense against the alcatraz-infra#31
# outage class (tmux crash takes the operator's chamber session with it).
#
# Why TMUX_TMPDIR not `-L <socket>`: tmux-msg-claude's mailman + discover shell
# out to plain `tmux` (no `-L` flag), but honor TMUX_TMPDIR via env-inheritance.
# Under `-L` the mailman can't find the actor pane on the isolated server
# (spawn-fail crash-loop) and discover walks the operator's default socket.
# Under TMUX_TMPDIR all three (driver / mailman / discover) coherently land on
# the private server. The `-L` form becomes viable once #288 plumbs the socket
# through; until then TMUX_TMPDIR is the working isolation lever.
export TMUX_TMPDIR="$TMUXDIR"
mkdir -p "$TMUX_TMPDIR"

# ── Cleanup (idempotent — safe to call multiple times) ────────────────────────

cleanup() {
    echo "[cleanup] tearing down sandbox..."
    # Kill the demo mailman by recorded PID only — never pkill the binary name,
    # which would also kill production mailmen on the same host.
    if [ -f "$MAILMAN_PID_FILE" ]; then
        kill "$(cat "$MAILMAN_PID_FILE")" 2>/dev/null || true
        rm -f "$MAILMAN_PID_FILE"
    fi
    # Kill ONLY the demo session first (idempotent during normal flow), then
    # nuke the private tmux server entirely — both touch only $TMUX_TMPDIR,
    # the operator's main tmux is unaffected.
    tmux kill-session -t "$SESSION" 2>/dev/null || true
    tmux kill-server 2>/dev/null || true
    rm -rf "$DB" "$MAILMAN_LOG" "$TMUXDIR"
}
trap cleanup EXIT

# ── Helpers ───────────────────────────────────────────────────────────────────

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

# wait_for_claude_ready polls bob's pane for Claude's idle ❯ prompt, dismissing
# the "trust this folder?" dialog if it appears. Replaces the hardcoded sleep 8
# to handle both already-trusted (fast) and fresh-directory (trust dialog) cases.
wait_for_claude_ready() {
    local pane="$1"
    local timeout=60
    local elapsed=0
    echo "  polling for Claude idle prompt (up to ${timeout}s)..."
    while [ $elapsed -lt $timeout ]; do
        local content
        content=$(tmux capture-pane -p -t "$pane" 2>/dev/null || true)
        # Dismiss trust/allow prompt if present (fires on first launch in untrusted dir).
        if echo "$content" | grep -qi "trust\|Do you trust\|allow.*folder"; then
            echo "  trust prompt detected — dismissing"
            tmux send-keys -t "$pane" Enter
            sleep 1
        fi
        if echo "$content" | grep -q '❯'; then
            echo "  Claude idle prompt ready (${elapsed}s elapsed)"
            return 0
        fi
        sleep 1
        elapsed=$((elapsed + 1))
    done
    echo "ERROR: Claude did not reach idle prompt within ${timeout}s" >&2
    return 1
}

# wait_for_delivery polls the mailman log for a "delivered id=" line written after
# the pane-paste succeeds. Falls back with a warning after timeout so the recording
# still stops rather than hanging.
wait_for_delivery() {
    local timeout=20
    local i=0
    echo "  polling for delivery confirmation (up to ${timeout}s)..."
    while [ $i -lt $((timeout * 2)) ]; do
        if grep -q "delivered id=" "$MAILMAN_LOG" 2>/dev/null; then
            echo "  delivery confirmed"
            return 0
        fi
        sleep 0.5
        i=$((i + 1))
    done
    echo "WARNING: delivery not confirmed within ${timeout}s — stopping anyway" >&2
}

# ── Phase 1: pre-clean any prior state ───────────────────────────────────────

echo "[1/6] pre-clean prior sandbox state"
rm -f "$CAST"
cleanup

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

# Propagate demo DB path to alice's shell. CLAUDE_MSG_DB is not in tmux's
# update-environment, so the calling-process export doesn't reach pane shells
# when the tmux server is already running (which it is on alcatraz). tmux setenv
# also doesn't help — it sets server-state at spawn-time; shells already running
# don't see it retroactively. Pre-exporting in the pane's own shell is the only
# reliable path. Runs before asciinema starts so it's invisible in the cast.
tmux send-keys -t "$ALICE_PANE" "export CLAUDE_MSG_DB=${DB}" Enter
sleep 0.2

# Launch real Claude Code in bob's pane. The observe-gate classifier requires the
# ❯ sentinel (internal/tmuxio/state.go AgentState branch); a non-Claude shell pane
# classifies as StateUnknown and the gate never shows the hold dynamics.
tmux send-keys -t "$BOB_PANE" 'claude' Enter
wait_for_claude_ready "$BOB_PANE"

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
# --cols/--rows force the cast dimensions regardless of the calling terminal size;
# without them, asciinema uses the pty size (defaults to 80×24 when not a real tty).
# --overwrite prevents silent abort when a prior .cast exists at the output path.
asciinema rec \
    --title "tmux-msg observe-gate" \
    --idle-time-limit=2 \
    --cols 120 \
    --rows 30 \
    --overwrite \
    --command "tmux attach -t $SESSION" \
    "$CAST" &
ASCIINEMA_PID=$!

# Wait for asciinema's tmux attach to connect before starting the take. A fixed
# sleep is not reliable: if the attach hasn't connected yet, keystrokes land in
# the calling shell rather than in the recording.
echo "  waiting for asciinema attach to connect..."
until tmux list-clients -t "$SESSION" 2>/dev/null | grep -q .; do
    sleep 0.2
done
echo "  attach ready"

# ── Phase 5: time-driven take ─────────────────────────────────────────────────

echo "[5/6] take begin — typing prompt in bob @ $(date +%H:%M:%S)"

# Start the typing in the background so we can fire the bus send mid-keystroke.
type_prompt "$PROMPT_TEXT" "$BOB_PANE" "$TYPING_DELAY" &
TYPING_PID=$!

# Let the typist establish rhythm and get visible characters into the pane before
# firing the send. This ensures the gate classifies bob as StateAwaitingOperator
# (cursor past the ❯ sentinel) when the message arrives, triggering the 📫 hold.
sleep "$PRE_SEND_DELAY"

# Send from alice's pane — type the command visibly so the viewer sees the send
# originating from alice's side (editorial intent: Herald's recipe Step 5 wants
# the send visible from alice). Uses send-keys -l for the full command so quoting
# and spaces pass through literally without tmux key-chord interpretation.
echo "  alice sends @ $(date +%H:%M:%S)"
tmux send-keys -t "$ALICE_PANE" -l "tmux-msg-claude send --to bob \"${MSG_BODY}\""
sleep 0.3
tmux send-keys -t "$ALICE_PANE" Enter

# Keep typing for a beat after the send so the viewer sees the 📫 hold during
# active keystroke activity — the visual proof that the gate is waiting for a
# pause, not just the end of a character stream.
sleep "$POST_SEND_TYPING"

# Pause the typing — the gate now waits for the input-stale-threshold.
kill "$TYPING_PID" 2>/dev/null || true
wait "$TYPING_PID" 2>/dev/null || true

echo "  typing paused — waiting for gate (${STALE_THRESHOLD}s) then delivery confirmation"
sleep "$STALE_THRESHOLD"

# Poll for actual delivery rather than sleeping a guessed total. The old fixed
# STALE_THRESHOLD+POST_LAND_WAIT arithmetic could race the mailman paste and leak
# keystrokes into the operator's terminal after the recording stopped.
wait_for_delivery
sleep "$POST_LAND_WAIT"

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
