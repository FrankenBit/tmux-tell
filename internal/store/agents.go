package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// UpsertAgent creates or updates an agent registry entry. If paneID is
// empty the existing pane_id (if any) is preserved — useful for `pause`
// or other operations that shouldn't touch discovery state.
//
// One-pane-one-identity (#549 Fix-2a): when paneID is non-empty, the upsert
// also SUPERSEDES any prior binding of that pane to a *different* name, so a
// pane_id is held by at most one agent row. Without this, registering a pane
// to a new name leaves the old name as a second row pointing at the same pane
// (the ON CONFLICT key is name, not pane_id); because identity resolution lists
// agents ORDER BY name and takes the first pane match, the alphabetically-prior
// stale name keeps winning — the chamber resolves to, and sends under, its old
// identity until the stale row is removed (the duplicate-pane-row drift).
func (s *Store) UpsertAgent(ctx context.Context, name, paneID string) error {
	name = CanonicalName(name)
	if name == "" {
		return errors.New("store: agent name required")
	}
	if ReservedRoutingName(name) {
		return fmt.Errorf("%w: %q", ErrReservedRoutingName, name)
	}

	if paneID == "" {
		_, err := s.db.ExecContext(ctx,
			`INSERT INTO agents (name, pane_id, updated_at)
			 VALUES (?, NULL, strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
			 ON CONFLICT(name) DO UPDATE SET
			   updated_at = excluded.updated_at`,
			name)
		return err
	}

	// Clear the pane from any OTHER row first, then upsert — atomically, so the
	// binding moves in one step and no observer sees the pane held by two names.
	// The prior holder is rebound to NULL (not deleted): it survives as a
	// dormant, pane-less registration, preserving any queued messages and
	// letting a later re-register re-bind it.
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx,
		`UPDATE agents
		 SET pane_id = NULL,
		     updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
		 WHERE pane_id = ? AND name != ?`,
		paneID, name); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO agents (name, pane_id, updated_at)
		 VALUES (?, ?, strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
		 ON CONFLICT(name) DO UPDATE SET
		   pane_id = excluded.pane_id,
		   updated_at = excluded.updated_at`,
		name, paneID); err != nil {
		return err
	}
	return tx.Commit()
}

// SetPaused updates the paused flag for an existing agent. Returns
// ErrNotFound if no agent with that name is registered.
func (s *Store) SetPaused(ctx context.Context, name string, paused bool) error {
	name = CanonicalName(name)
	p := 0
	if paused {
		p = 1
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE agents
		 SET paused = ?,
		     updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
		 WHERE name = ?`,
		p, name)
	if err != nil {
		return err
	}
	if rows, _ := res.RowsAffected(); rows == 0 {
		return fmt.Errorf("store: agent %q: %w", name, ErrNotFound)
	}
	return nil
}

// SetPausedAll flips the paused flag for every registered agent in one
// transaction. Returns the number of rows touched.
func (s *Store) SetPausedAll(ctx context.Context, paused bool) (int64, error) {
	p := 0
	if paused {
		p = 1
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE agents
		 SET paused = ?,
		     updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')`,
		p)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// GetAgent returns the agent by name, or ErrNotFound.
func (s *Store) GetAgent(ctx context.Context, name string) (*Agent, error) {
	name = CanonicalName(name)
	var (
		a               Agent
		pane            sql.NullString
		paused          int
		aliases         string
		deliveryMode    string
		metabolismSetAt sql.NullString
		lastCompactAt   sql.NullString
		compactCounted  sql.NullString
		autoRestart     int
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT name, pane_id, paused, updated_at, aliases, delivery_mode, backlog_epoch_id, attention_state, stuck_reason, display_name, session_id, metabolism, metabolism_set_at, respawn_after_shrinks, respawn_shrink_count, last_self_compact_at, self_compact_counted_at, relaunch_cmd, auto_restart, provider FROM agents WHERE name = ?`,
		name).Scan(&a.Name, &pane, &paused, &a.UpdatedAt, &aliases, &deliveryMode, &a.BacklogEpoch, &a.AttentionState, &a.StuckReason, &a.DisplayName, &a.SessionID, &a.Metabolism, &metabolismSetAt, &a.RespawnAfterShrinks, &a.RespawnShrinkCount, &lastCompactAt, &compactCounted, &a.RelaunchCmd, &autoRestart, &a.Provider)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	} else if err != nil {
		return nil, err
	}
	if pane.Valid {
		a.PaneID = pane.String
	}
	a.Paused = paused != 0
	a.Aliases = decodeAliases(aliases)
	a.DeliveryMode = deliveryMode
	if metabolismSetAt.Valid {
		a.MetabolismSetAt = metabolismSetAt.String
	}
	if lastCompactAt.Valid {
		a.LastSelfCompactAt = lastCompactAt.String
	}
	if compactCounted.Valid {
		a.SelfCompactCountedAt = compactCounted.String
	}
	a.AutoRestart = autoRestart != 0
	return &a, nil
}

// SetDisplayName persists an agent's chamber-asserted display name (#556).
// Called by set_pane_name alongside the tmux title-set so agents listings +
// status outputs can show the case-preserved name. Returns ErrNotFound if no
// agent with that name is registered.
//
// Does not bump updated_at: like the attention-state / stuck setters, this is
// a presentational signal from the chamber, not a discovery-relevant change.
func (s *Store) SetDisplayName(ctx context.Context, name, displayName string) error {
	name = CanonicalName(name)
	res, err := s.db.ExecContext(ctx,
		`UPDATE agents SET display_name = ? WHERE name = ?`,
		displayName, name)
	if err != nil {
		return err
	}
	if rows, _ := res.RowsAffected(); rows == 0 {
		return fmt.Errorf("store: agent %q: %w", name, ErrNotFound)
	}
	return nil
}

// SetSessionID persists an agent's intrinsic session identity (#626 Phase 1b):
// the wrapper-injected TMUX_TELL_SESSION_ID UUID (#643), passed at register or
// read from the registering pane's process tree. When non-empty it is the
// primary exact match key for
// session-as-addressee resolution; "" clears it (back to the name-fallback
// path). Returns ErrNotFound if no agent with that name is registered.
//
// Does not bump updated_at: like SetDisplayName, this rides the register call
// that already touched the row; it is identity-metadata, not a separate
// discovery-relevant mutation.
func (s *Store) SetSessionID(ctx context.Context, name, sessionID string) error {
	name = CanonicalName(name)
	res, err := s.db.ExecContext(ctx,
		`UPDATE agents SET session_id = ? WHERE name = ?`,
		sessionID, name)
	if err != nil {
		return err
	}
	if rows, _ := res.RowsAffected(); rows == 0 {
		return fmt.Errorf("store: agent %q: %w", name, ErrNotFound)
	}
	return nil
}

// SetDeliveryMode updates the delivery_mode for an existing agent.
// Returns ErrNotFound if no agent with that name is registered.
// Validates against ValidDeliveryMode — invalid modes are rejected.
func (s *Store) SetDeliveryMode(ctx context.Context, name, mode string) error {
	name = CanonicalName(name)
	if !ValidDeliveryMode(mode) {
		return fmt.Errorf("store: invalid delivery_mode %q (want %q or %q)",
			mode, DeliveryModePasteAndEnter, DeliveryModeMailboxOnly)
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE agents
		 SET delivery_mode = ?,
		     updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
		 WHERE name = ?`,
		mode, name)
	if err != nil {
		return err
	}
	if rows, _ := res.RowsAffected(); rows == 0 {
		return fmt.Errorf("store: agent %q: %w", name, ErrNotFound)
	}
	return nil
}

// SetBacklogEpoch stamps the #204 claim-floor for an agent: the highest
// message id the mailman should treat as pre-existing backlog and skip on
// claim. Called by the register handler when a (re)registering agent has a
// queued backlog the don't-flood policy decided not to paste all at once.
// Returns ErrNotFound if no agent with that name is registered.
//
// The floor only ever advances in practice (new arrivals always get higher
// ids than any earlier floor), but this setter writes whatever the caller
// computed — monotonicity is the register handler's policy, not the store's.
// Does not bump updated_at: the epoch is internal delivery bookkeeping, not a
// discovery-relevant change, and the register flow's UpsertAgent already
// touched the row microseconds earlier.
func (s *Store) SetBacklogEpoch(ctx context.Context, name string, floor int64) error {
	name = CanonicalName(name)
	res, err := s.db.ExecContext(ctx,
		`UPDATE agents SET backlog_epoch_id = ? WHERE name = ?`,
		floor, name)
	if err != nil {
		return err
	}
	if rows, _ := res.RowsAffected(); rows == 0 {
		return fmt.Errorf("store: agent %q: %w", name, ErrNotFound)
	}
	return nil
}

// ListAgents returns every registered agent, ordered by name ASC.
func (s *Store) ListAgents(ctx context.Context) ([]Agent, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT name, pane_id, paused, updated_at, aliases, delivery_mode, backlog_epoch_id, attention_state, stuck_reason, display_name, session_id, metabolism, metabolism_set_at, respawn_after_shrinks, respawn_shrink_count, last_self_compact_at, self_compact_counted_at, relaunch_cmd, auto_restart, provider FROM agents ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck // best-effort close

	var out []Agent
	for rows.Next() {
		var (
			a               Agent
			pane            sql.NullString
			paused          int
			aliases         string
			deliveryMode    string
			metabolismSetAt sql.NullString
			lastCompactAt   sql.NullString
			compactCounted  sql.NullString
			autoRestart     int
		)
		if err := rows.Scan(&a.Name, &pane, &paused, &a.UpdatedAt, &aliases, &deliveryMode, &a.BacklogEpoch, &a.AttentionState, &a.StuckReason, &a.DisplayName, &a.SessionID, &a.Metabolism, &metabolismSetAt, &a.RespawnAfterShrinks, &a.RespawnShrinkCount, &lastCompactAt, &compactCounted, &a.RelaunchCmd, &autoRestart, &a.Provider); err != nil {
			return nil, err
		}
		if pane.Valid {
			a.PaneID = pane.String
		}
		a.Paused = paused != 0
		a.Aliases = decodeAliases(aliases)
		a.DeliveryMode = deliveryMode
		if metabolismSetAt.Valid {
			a.MetabolismSetAt = metabolismSetAt.String
		}
		if lastCompactAt.Valid {
			a.LastSelfCompactAt = lastCompactAt.String
		}
		if compactCounted.Valid {
			a.SelfCompactCountedAt = compactCounted.String
		}
		a.AutoRestart = autoRestart != 0
		out = append(out, a)
	}
	return out, rows.Err()
}

// SetAttentionState updates an agent's attention_state (#224). The state is
// validated against ValidAttentionState before writing — invalid values
// are rejected with a descriptive error. Returns ErrNotFound if no agent
// with that name is registered.
//
// Does not bump updated_at: attention transitions are operational
// signals from the chamber, not discovery-relevant changes. (The
// chamber-attention-signal mechanism is for operator visibility; the
// agents-table updated_at carries a different semantic.)
func (s *Store) SetAttentionState(ctx context.Context, name, state string) error {
	name = CanonicalName(name)
	if !ValidAttentionState(state) {
		return fmt.Errorf("store: invalid attention_state %q (want %q, %q, or %q)",
			state, AttentionStateIdle, AttentionStateBusy, AttentionStateAwaitingOperator)
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE agents SET attention_state = ? WHERE name = ?`,
		state, name)
	if err != nil {
		return err
	}
	if rows, _ := res.RowsAffected(); rows == 0 {
		return fmt.Errorf("store: agent %q: %w", name, ErrNotFound)
	}
	return nil
}

// SetMetabolism records a chamber's self-reported metabolism (#621): one of
// MetabolismWarming / MetabolismSaturating / MetabolismCompactPending, or "" to
// clear the self-report. Validated against ValidMetabolism (empty is valid — the
// clear path). A non-empty value stamps metabolism_set_at = now so consumers can
// discount a stale self-report; clearing nulls the stamp, keeping the invariant
// metabolism == "" ⟺ metabolism_set_at IS NULL. Returns ErrNotFound if no agent
// with that name is registered.
//
// Does not bump updated_at: like SetAttentionState, this is an operational
// self-signal from the chamber, not a discovery-relevant change.
//
// The store does not enforce WHO calls this — self-only is enforced at the
// surface (set_metabolism resolves the caller's own identity and exposes no
// target parameter; see internal/cli/set_metabolism.go). A third-party write
// would clobber the target's real signal, the failure #621 AC#2 guards against.
func (s *Store) SetMetabolism(ctx context.Context, name, value string) error {
	name = CanonicalName(name)
	if !ValidMetabolism(value) {
		return fmt.Errorf("store: invalid metabolism %q (want %q, %q, %q, or empty)",
			value, MetabolismWarming, MetabolismSaturating, MetabolismCompactPending)
	}
	var res sql.Result
	var err error
	if value == "" {
		res, err = s.db.ExecContext(ctx,
			`UPDATE agents SET metabolism = '', metabolism_set_at = NULL WHERE name = ?`,
			name)
	} else {
		res, err = s.db.ExecContext(ctx,
			`UPDATE agents SET metabolism = ?, metabolism_set_at = ? WHERE name = ?`,
			value, time.Now().UTC().Format(sqliteTimeFormat), name)
	}
	if err != nil {
		return err
	}
	if rows, _ := res.RowsAffected(); rows == 0 {
		return fmt.Errorf("store: agent %q: %w", name, ErrNotFound)
	}
	return nil
}

// ClearMetabolismIfPending clears a chamber's metabolism ONLY when it is
// currently MetabolismCompactPending (#621). Called from the mailman's
// self-observation path once it observes the chamber actually at-rest-in-
// compaction: observed-truth now carries the ground state, so the
// compact-pending self-report has done its job and is cleared to avoid lingering
// stale after the chamber resumes. The WHERE-guard makes it a no-op against any
// other value (a warming/saturating self-report is NOT clobbered) and against a
// missing agent — so, unlike SetMetabolism, zero rows affected is not an error.
func (s *Store) ClearMetabolismIfPending(ctx context.Context, name string) error {
	name = CanonicalName(name)
	_, err := s.db.ExecContext(ctx,
		`UPDATE agents SET metabolism = '', metabolism_set_at = NULL
		 WHERE name = ? AND metabolism = ?`,
		name, MetabolismCompactPending)
	return err
}

// SetRespawnAfterShrinks sets the #285 per-chamber respawn threshold N: the
// mailman respawns the chamber's process after N counted context-shrink events.
// n must be >= 0; 0 disables respawn for this chamber (the default). Returns
// ErrNotFound if no agent with that name is registered.
//
// Does not bump updated_at: an operational tunable, not a discovery-relevant
// change (mirrors SetMetabolism / SetAttentionState).
func (s *Store) SetRespawnAfterShrinks(ctx context.Context, name string, n int) error {
	name = CanonicalName(name)
	if n < 0 {
		return fmt.Errorf("store: respawn-after-shrinks must be >= 0, got %d", n)
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE agents SET respawn_after_shrinks = ? WHERE name = ?`,
		n, name)
	if err != nil {
		return err
	}
	if rows, _ := res.RowsAffected(); rows == 0 {
		return fmt.Errorf("store: agent %q: %w", name, ErrNotFound)
	}
	return nil
}

// SetRelaunchCmd persists the #285/#730 relaunch command — the exact string the
// mailman send-keys into a post-exit bare shell to restart the chamber. An empty
// cmd is allowed (clears/leaves the primitive unconfigured, so a threshold/
// co-trigger fire logs+skips rather than stranding a bare shell). Returns
// ErrNotFound if no agent with that name is registered.
//
// Does not bump updated_at: an operational tunable, not a discovery-relevant
// change (mirrors SetRespawnAfterShrinks).
func (s *Store) SetRelaunchCmd(ctx context.Context, name, cmd string) error {
	name = CanonicalName(name)
	res, err := s.db.ExecContext(ctx,
		`UPDATE agents SET relaunch_cmd = ? WHERE name = ?`,
		cmd, name)
	if err != nil {
		return err
	}
	if rows, _ := res.RowsAffected(); rows == 0 {
		return fmt.Errorf("store: agent %q: %w", name, ErrNotFound)
	}
	return nil
}

// SetAutoRestart persists the #730 per-chamber co-trigger flag: when true, a
// tmux-tell-triggered /compact that leads to a chamber exit is auto-relaunched
// via the registered relaunch_cmd. Returns ErrNotFound if no agent with that
// name is registered.
//
// Does not bump updated_at: an operational tunable, not a discovery-relevant
// change (mirrors SetRespawnAfterShrinks).
func (s *Store) SetAutoRestart(ctx context.Context, name string, on bool) error {
	name = CanonicalName(name)
	v := 0
	if on {
		v = 1
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE agents SET auto_restart = ? WHERE name = ?`,
		v, name)
	if err != nil {
		return err
	}
	if rows, _ := res.RowsAffected(); rows == 0 {
		return fmt.Errorf("store: agent %q: %w", name, ErrNotFound)
	}
	return nil
}

// IncrementRespawnShrinkCount increments the #285 shrink counter for an agent
// and returns the new count. Called by the mailman when it delivers a counted
// context-shrink event (a bus-delivered clear in PR1). Returns ErrNotFound if
// no agent with that name is registered.
//
// The increment + read-back are two statements rather than one RETURNING (the
// codebase's SQLite idiom is ExecContext + Select). That is race-free here: the
// per-agent mailman loop is single-flight (one delivery at a time) and is the
// SOLE writer of this counter — both increment and ResetRespawnShrinkCount run
// on that one goroutine, so no concurrent mutation can interleave. Does not bump
// updated_at: internal delivery bookkeeping.
func (s *Store) IncrementRespawnShrinkCount(ctx context.Context, name string) (int, error) {
	name = CanonicalName(name)
	res, err := s.db.ExecContext(ctx,
		`UPDATE agents SET respawn_shrink_count = respawn_shrink_count + 1 WHERE name = ?`,
		name)
	if err != nil {
		return 0, err
	}
	if rows, _ := res.RowsAffected(); rows == 0 {
		return 0, fmt.Errorf("store: agent %q: %w", name, ErrNotFound)
	}
	var count int
	if err := s.db.QueryRowContext(ctx,
		`SELECT respawn_shrink_count FROM agents WHERE name = ?`, name).Scan(&count); err != nil {
		return 0, err
	}
	return count, nil
}

// ResetRespawnShrinkCount clears the #285 shrink counter back to 0 — called
// after a respawn fires (the counter cycle restarts) and safe to call when the
// count is already 0. A missing agent is a no-op, not an error: this is
// best-effort bookkeeping bracketing the respawn pathway.
func (s *Store) ResetRespawnShrinkCount(ctx context.Context, name string) error {
	name = CanonicalName(name)
	_, err := s.db.ExecContext(ctx,
		`UPDATE agents SET respawn_shrink_count = 0 WHERE name = ?`,
		name)
	return err
}

// SetSelfCompactSignal stamps the #285 PR2 self-compact signal for an agent:
// last_self_compact_at = now (sqliteTimeFormat UTC). Invoked by the adapter's
// post-compaction hook via the note-compact helper — it records THAT a
// self-/compact happened; the mailman counts it on its next self-observation via
// CountSelfCompactIfNew. Returns ErrNotFound if no agent with that name is
// registered (a hook wired for an unregistered agent must fail loud, not silently
// no-op).
//
// This is the ONE column the hook writes. It is a blind overwrite (always "now"),
// never a read-modify-write, so it is safe for the hook process to write
// concurrently with the mailman's reads: SQLite WAL serializes the write, and the
// mailman only ever READS last_self_compact_at (it writes self_compact_counted_at +
// respawn_shrink_count, which the hook never touches). That separation is what
// keeps the mailman the sole writer of the counter family — PR1's race-freedom
// invariant — even though a second process (the hook) now participates.
//
// Does not bump updated_at: an operational delivery signal, not discovery-relevant.
func (s *Store) SetSelfCompactSignal(ctx context.Context, name string) error {
	return s.setSelfCompactSignalAt(ctx, name, time.Now())
}

// setSelfCompactSignalAt is the timestamp-injectable core of SetSelfCompactSignal.
// Production stamps time.Now(); tests pass explicit instants so the edge-detection
// ordering (CountSelfCompactIfNew's `>` watermark guard) is deterministic without
// sleeping to cross the millisecond-precision sqliteTimeFormat boundary.
func (s *Store) setSelfCompactSignalAt(ctx context.Context, name string, at time.Time) error {
	name = CanonicalName(name)
	res, err := s.db.ExecContext(ctx,
		`UPDATE agents SET last_self_compact_at = ? WHERE name = ?`,
		at.UTC().Format(sqliteTimeFormat), name)
	if err != nil {
		return err
	}
	if rows, _ := res.RowsAffected(); rows == 0 {
		return fmt.Errorf("store: agent %q: %w", name, ErrNotFound)
	}
	return nil
}

// CountSelfCompactIfNew edge-detects a fresh self-compact signal and, if the
// current last_self_compact_at is newer than the mailman's watermark
// (self_compact_counted_at), counts ONE shrink toward respawn_shrink_count and
// advances the watermark — atomically, in a single UPDATE. Returns (counted=true,
// the new count) when it counted, or (false, 0) when nothing was new (or the agent
// is missing). Called by the mailman on its self-observation cadence; a self-compact
// is chamber-driven (no bus delivery), so this is the counting path the inline
// clear path (PR1) can't serve.
//
// Atomicity + race-freedom: the increment and the watermark-advance are ONE
// statement, so a mailman crash between them is impossible — the signal is either
// fully counted or not at all (never double-counted, never half-counted). The
// mailman is the sole writer of both columns this touches; the hook only writes
// last_self_compact_at (read here). Lexical `>` on last_self_compact_at vs
// self_compact_counted_at is chronological because both are the same fixed-width
// sqliteTimeFormat UTC stamp (the watermark is copied verbatim from the signal).
//
// Burst semantics (documented, accepted): if two self-/compacts land between two
// mailman observations, last_self_compact_at reflects only the LATEST, so this
// counts ONE, not two. Compactions are turns/minutes apart, and an under-count in a
// rapid burst merely delays a respawn by one shrink event — the next compaction
// re-triggers. Exact per-compaction counting would require a hook-side
// read-modify-write, reintroducing the very cross-process race this design avoids.
func (s *Store) CountSelfCompactIfNew(ctx context.Context, name string) (counted bool, newCount int, err error) {
	name = CanonicalName(name)
	res, err := s.db.ExecContext(ctx,
		`UPDATE agents
		    SET respawn_shrink_count = respawn_shrink_count + 1,
		        self_compact_counted_at = last_self_compact_at
		  WHERE name = ?
		    AND last_self_compact_at IS NOT NULL
		    AND last_self_compact_at != ''
		    AND (self_compact_counted_at IS NULL
		         OR self_compact_counted_at = ''
		         OR last_self_compact_at > self_compact_counted_at)`,
		name)
	if err != nil {
		return false, 0, err
	}
	if rows, _ := res.RowsAffected(); rows == 0 {
		// Nothing new (or the agent is missing). Not an error: the mailman polls
		// this every eligible iteration, so a no-op is the common case.
		return false, 0, nil
	}
	if err := s.db.QueryRowContext(ctx,
		`SELECT respawn_shrink_count FROM agents WHERE name = ?`, name).Scan(&newCount); err != nil {
		return false, 0, err
	}
	return true, newCount, nil
}

// SetStuck parks an agent's mailman with the given non-empty reason (#291).
// The mailman's loop reads stuck_reason every iteration and, once it is
// non-empty, stops probing tmux for this agent entirely — the load-bearing
// property that prevents a persistent pane-probe failure from driving the
// retry storm that wedged the tmux server (2026-06-10 17:54). Reason must be
// non-empty (use ClearStuck to un-park). Returns ErrNotFound if no agent with
// that name is registered.
//
// Does not bump updated_at: like the attention-state setter, this is an
// operational delivery signal, not a discovery-relevant change.
func (s *Store) SetStuck(ctx context.Context, name, reason string) error {
	if reason == "" {
		return fmt.Errorf("store: SetStuck requires a non-empty reason (use ClearStuck to un-park)")
	}
	return s.setStuckReason(ctx, name, reason)
}

// ClearStuck un-parks an agent's mailman by resetting stuck_reason to the
// empty (healthy) default (#291). Called by `register --force` when the
// operator fixes the registration; the mailman resumes normal delivery on
// its next loop iteration. Idempotent — clearing an already-healthy agent is
// a no-op write. Returns ErrNotFound if no agent with that name is registered.
func (s *Store) ClearStuck(ctx context.Context, name string) error {
	return s.setStuckReason(ctx, name, "")
}

func (s *Store) setStuckReason(ctx context.Context, name, reason string) error {
	name = CanonicalName(name)
	res, err := s.db.ExecContext(ctx,
		`UPDATE agents SET stuck_reason = ? WHERE name = ?`,
		reason, name)
	if err != nil {
		return err
	}
	if rows, _ := res.RowsAffected(); rows == 0 {
		return fmt.Errorf("store: agent %q: %w", name, ErrNotFound)
	}
	return nil
}

// ErrAliasCollision is returned by SetAliases/AddAlias when the
// requested alias is already claimed by another canonical agent (as
// that agent's name or as one of its aliases). Surveyor #38-Q(a)
// review: catch collisions at registration time so the resolver
// never has to choose between two canonicals at delivery time.
var ErrAliasCollision = errors.New("store: alias collides with an existing canonical agent")

// ErrReservedRoutingName is returned by UpsertAgent / SetAliases /
// AddAlias when a caller tries to register or alias one of the
// substrate's reserved routing primitives (see ReservedRoutingName).
// Substrate-honest fail-loud: a real chamber registering as a
// routing primitive would shadow the resolver (#228).
var ErrReservedRoutingName = errors.New("store: name is reserved as a routing primitive")

// SetAliases replaces the alias list for an agent. Empty slice removes
// all aliases. Returns ErrNotFound if no agent with that name exists,
// or ErrAliasCollision if any requested alias is already claimed by
// another agent.
func (s *Store) SetAliases(ctx context.Context, name string, aliases []string) error {
	name = CanonicalName(name)
	if err := s.checkAliasCollisions(ctx, name, aliases); err != nil {
		return err
	}
	encoded, err := encodeAliases(aliases)
	if err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE agents
		 SET aliases = ?,
		     updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
		 WHERE name = ?`,
		encoded, name)
	if err != nil {
		return err
	}
	if rows, _ := res.RowsAffected(); rows == 0 {
		return fmt.Errorf("store: agent %q: %w", name, ErrNotFound)
	}
	return nil
}

// AddAlias appends an alias to the agent's list (idempotent — duplicate
// aliases on the SAME agent are silently ignored). Returns ErrNotFound
// for missing agents, ErrAliasCollision if another canonical agent
// already claims the alias.
func (s *Store) AddAlias(ctx context.Context, name, alias string) error {
	a, err := s.GetAgent(ctx, name)
	if err != nil {
		return err
	}
	for _, existing := range a.Aliases {
		if existing == alias {
			return nil
		}
	}
	return s.SetAliases(ctx, name, append(a.Aliases, alias))
}

// checkAliasCollisions returns ErrAliasCollision if any of `aliases`
// collides with another agent's canonical name OR with one of another
// agent's aliases. Self-collisions (the agent's own name/aliases) are
// allowed — that's just rebinding.
func (s *Store) checkAliasCollisions(ctx context.Context, name string, aliases []string) error {
	if len(aliases) == 0 {
		return nil
	}
	name = CanonicalName(name)
	for _, candidate := range aliases {
		if ReservedRoutingName(candidate) {
			return fmt.Errorf("%w: alias %q", ErrReservedRoutingName, candidate)
		}
	}
	all, err := s.ListAgents(ctx)
	if err != nil {
		return err
	}
	for _, candidate := range aliases {
		// Compare on the canonical routing key (#721): agent names are stored
		// canonical, so an alias "Quartermaster" must still be caught as
		// colliding with the canonical name "quartermaster" — otherwise
		// canonicalizing names would open the very shadow this guard prevents.
		canonCand := CanonicalName(candidate)
		for _, other := range all {
			if other.Name == name {
				continue // self
			}
			if other.Name == canonCand {
				return fmt.Errorf("%w: alias %q is the canonical name of agent %q",
					ErrAliasCollision, candidate, other.Name)
			}
			for _, otherAlias := range other.Aliases {
				if CanonicalName(otherAlias) == canonCand {
					return fmt.Errorf("%w: alias %q is already an alias of agent %q",
						ErrAliasCollision, candidate, other.Name)
				}
			}
		}
	}
	return nil
}

func decodeAliases(raw string) []string {
	if raw == "" || raw == "[]" {
		return nil
	}
	var out []string
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		// Corrupted aliases column shouldn't break callers; treat as
		// empty so the rest of the row is usable.
		return nil
	}
	return out
}

func encodeAliases(aliases []string) (string, error) {
	if len(aliases) == 0 {
		return "[]", nil
	}
	b, err := json.Marshal(aliases)
	if err != nil {
		return "", err
	}
	return string(b), nil
}
