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
	var (
		a               Agent
		pane            sql.NullString
		paused          int
		aliases         string
		deliveryMode    string
		metabolismSetAt sql.NullString
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT name, pane_id, paused, updated_at, aliases, delivery_mode, backlog_epoch_id, attention_state, stuck_reason, display_name, session_id, metabolism, metabolism_set_at FROM agents WHERE name = ?`,
		name).Scan(&a.Name, &pane, &paused, &a.UpdatedAt, &aliases, &deliveryMode, &a.BacklogEpoch, &a.AttentionState, &a.StuckReason, &a.DisplayName, &a.SessionID, &a.Metabolism, &metabolismSetAt)
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

// SetSessionID persists an agent's self-discovered Claude session identity
// (#626 Phase 1b): CLAUDE_CODE_SESSION_ID, read from the registering pane's
// process tree. When non-empty it is the primary exact match key for
// session-as-addressee resolution; "" clears it (back to the name-fallback
// path). Returns ErrNotFound if no agent with that name is registered.
//
// Does not bump updated_at: like SetDisplayName, this rides the register call
// that already touched the row; it is identity-metadata, not a separate
// discovery-relevant mutation.
func (s *Store) SetSessionID(ctx context.Context, name, sessionID string) error {
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
		`SELECT name, pane_id, paused, updated_at, aliases, delivery_mode, backlog_epoch_id, attention_state, stuck_reason, display_name, session_id, metabolism, metabolism_set_at FROM agents ORDER BY name`)
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
		)
		if err := rows.Scan(&a.Name, &pane, &paused, &a.UpdatedAt, &aliases, &deliveryMode, &a.BacklogEpoch, &a.AttentionState, &a.StuckReason, &a.DisplayName, &a.SessionID, &a.Metabolism, &metabolismSetAt); err != nil {
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
	_, err := s.db.ExecContext(ctx,
		`UPDATE agents SET metabolism = '', metabolism_set_at = NULL
		 WHERE name = ? AND metabolism = ?`,
		name, MetabolismCompactPending)
	return err
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
		for _, other := range all {
			if other.Name == name {
				continue // self
			}
			if other.Name == candidate {
				return fmt.Errorf("%w: alias %q is the canonical name of agent %q",
					ErrAliasCollision, candidate, other.Name)
			}
			for _, otherAlias := range other.Aliases {
				if otherAlias == candidate {
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
