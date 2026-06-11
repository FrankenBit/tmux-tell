# ADR-0012: Session rename on bus-mediated clear (application of ADR-0011's three-fence test)

> **Status**: Accepted
> **Date**: 2026-06-11 (proposed); 2026-06-11 (accepted — operator ratification via Quartermaster, #306)
> **Authors**: Engineer (ADR draft), operator (ratified the three-fence framing 2026-06-11), Quartermaster (design framing + ratification routing), operator + Quartermaster (design framing — #286 provenance, 2026-06-09 design conversation), Surveyor (framework-quality review on PR #306)

## Context

The **bus-mediated clear** primitive (today Bosun + Quartermaster → Pilot only)
spawns a fresh chamber session each time it fires. The new session inherits the
chamber's canonical name ("Pilot"), so the chamber's session storage accumulates
many JSONLs all named "Pilot" (17 by 2026-06-09). The costs, surfaced by the
alcatraz outage diagnosis:

1. **`claude --resume <chamber>` is ambiguous** — it prompts among N same-named
   sessions, so pane-restore after a tmux restart is non-deterministic.
2. **No forensic queryability** — "what was Pilot doing for #266?" needs a manual
   scan of every JSONL.
3. **Unbounded storage growth** — sessions accumulate until manual cleanup.

The operator's structural fix (2026-06-09): every bus-mediated clear must name
*what task the cleared session was working on*, and the **outgoing** session is
renamed to that task identity *before* the new session takes over. Result:
`claude --resume Pilot` matches exactly one (the current, canonically-named)
session; history is searchable by task; growth is bounded *with intent*.

That makes tmux-msg **touch session storage** — a new substrate responsibility
beyond message delivery. ADR-0011 established the test for exactly this kind of
outward scope-expansion. This ADR is its **second application** (n=2), which is
also what promotes the three fences from respawn's local justification to an
extracted project pattern (mirroring ADR-0003's structurally-distinct-case logic).

## Decision

**Admit session-rename-on-clear under ADR-0011's three-fence test, and accept the
session-storage-touching responsibility it entails.**

The clear flow gains a **required** target-task parameter; the rename runs as a
thin step *before* the existing `/clear` paste:

```
bus-mediated clear-with-task arrives at chamber
  → rename the current session to the task identity      ← THIS ADR / #286
  → /clear the chamber (existing behavior)
  → (if ADR-0011's respawn counter tripped: respawn)     ← ADR-0011 / #285
  → a fresh, canonically-named session takes over
```

### How rename satisfies ADR-0011's fences

1. **Trigger observable, not invented.** Rename fires *only* as part of a
   bus-mediated clear — an event the substrate already originates. There is no
   "rename this session" event.
2. **Carriage already in the mailman's hands.** Bus-mediated clear is already
   in-substrate; rename is a thin pre-step before the existing clear-paste, not a
   new subsystem.
3. **No standalone lever.** There is **no** `rename` / `rename-session`
   subcommand and **no** `rename_chamber_session` MCP tool. Renaming happens
   *only* inside clear-with-task. The bus does not become a session-storage
   manager beyond what clear-with-task already requires.

### What this ADR commits to vs. leaves open

This ADR commits to: (a) the rename is admissible under the fences; (b) the clear
API gains a **required** task parameter (`--for-task <string>`, or structured
`--project + --issue`), validated — empty rejected, the chamber's canonical name
rejected, name-collisions warned-not-failed; (c) clear-without-task enters a
**deprecation cycle** — soft-warn now, hard-error two minor cycles later, per
ADR-0008; (d) tmux-msg accepting a session-storage-touching responsibility as a
recorded, bounded expansion.

This ADR deliberately does **NOT** fix the rename *mechanism*. How a Claude Code
session is named — a start-time CLI flag, a JSONL metadata field, derivation from
the first prompt, or a separate index file — is **an open research item**, the
first implementation task of #286. The fences hold regardless of which mechanism
is true; the ADR is the boundary, the probe is the build. **Honesty caveat with
its close:** if the probe finds rename requires *file surgery* (editing or moving
JSONLs in `~/.claude/projects/...`), that is a strictly larger substrate
responsibility than setting a name at spawn time — the implementation PR must
re-confirm against fence 2 (is editing session-storage files still "carriage the
mailman owns", or has the expansion outgrown the fence?) and record the finding
here as an amendment before building on it.

## Alternatives considered

- **Keep clear param optional / default to canonical name.** Rejected: an optional
  task param leaves the 17-identical-Pilots failure mode in place for every caller
  that omits it; the fix only works if naming is *required* (fail-loud when
  absent, after the deprecation cycle).
- **A standalone `rename --session <id> --to <name>` lever.** Rejected by fence 3:
  a free-standing rename verb makes tmux-msg a session-storage manager and invites
  "rename/move/delete any session" scope. Coupling rename to clear keeps it a
  carriage pre-step.
- **Archive/cleanup sweep instead of rename-on-clear.** Rejected as the *primary*
  fix: a periodic sweep (alcatraz-infra#34 did a one-time archive) treats the
  symptom after the fact and still leaves resume ambiguous between sweeps.
  Rename-at-source is deterministic. (A forensic rename of the already-archived
  JSONLs remains a reasonable *follow-up*, out of scope here.)
- **Solve it outside tmux-msg (operator script renames sessions).** Rejected: the
  task identity is known *only* at clear time, and clear is a tmux-msg primitive —
  an external script would have to shadow the bus's clear surface and race it. The
  knowledge and the trigger both live in the bus already (fences 1–2).

## Consequences

### Cleaner

- **Deterministic resume + forensic queryability + bounded growth**, all three,
  from one required parameter — `claude --resume Pilot` is unambiguous, sessions
  are `grep`-able by task, and storage grows with intent rather than as N copies
  of one name.
- **The three-fence test earns its second clean application**, promoting it from
  respawn-local to an extracted pattern; future scope-expansion proposals now have
  two worked instances to calibrate against.

### Harder

- **tmux-msg now touches session storage.** Even at its lightest (set a name at
  spawn), this is a responsibility beyond message delivery; at its heaviest (file
  surgery) it is a meaningful expansion that must be re-checked against fence 2 at
  implementation time. The fence is what bounds it; the responsibility is real
  either way.
- **A forward-incompatible API change.** Required `--for-task` breaks existing
  clear-without-task callers; the ADR-0008 two-minor deprecation cycle (soft-warn
  → hard-error) is the migration cost, plus a CHANGELOG entry at each end of the
  cycle.
- **Validation surface.** Empty-rejected, canonical-name-rejected, collision-warned
  is three rules the clear path must enforce and test — modest, but it is new
  policy the primitive did not carry before.

## What would change the decision

- **The research probe finds no rename mechanism** that satisfies the fences — e.g.
  renaming demands unsupported file surgery that pushes past fence 2. Then the
  decision narrows to "require the task param for forensic logging" *without* the
  storage-side rename, or the feature is deferred until Claude Code exposes a
  supported naming surface.
- **Claude Code ships first-class session naming** (a supported `--session-name`
  or equivalent). The rename step then becomes a thin pass-through and the
  file-surgery concern evaporates — a simplification, not a retraction.
- **A standalone-rename need surfaces** that cannot be expressed as part of clear
  (the same fence-1/3 trigger as ADR-0011's parallel watch). That would reopen
  whether tmux-msg should own an explicit session-management verb.

## References

- #286 (this feature — clear-with-task + outgoing-session rename; carries the
  research-probe AC and the API/validation/deprecation contract)
- ADR-0011 (the three-fence scope-expansion test this ADR applies — the first
  application, chamber respawn; #285)
- #285 (post-compact respawn — shares the bus-mediated-clear trigger surface; the
  two features compose as one design unit: rename → /clear → optional respawn)
- #227 (deferred-delivery primitive — rename runs *before* the deferred-delivery
  window opens)
- ADR-0003 (substrate-vs-flavor naming — the structurally-distinct-case
  promotion-threshold this ADR's n=2 instantiates)
- ADR-0008 (deprecation policy — governs the clear-without-task removal cycle)
- alcatraz-infra#34 (one-time archive of 16 Pilot JSONLs — the cleanup this
  feature obviates going forward)
