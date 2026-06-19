# Chamber dispatch: claiming an issue so two agents don't collide

This is a coordination convention for **multi-agent deployments** where several
agents (here, the alcatraz "chambers" — one Claude instance per tmux pane) draw
work from a shared issue tracker, and more than one party can hand out that work.
It is not a tmux-tell feature; it is a discipline that sits *next to* the bus and
covers a gap the bus deliberately doesn't fill. It uses the project-overlay
"chamber" vocabulary, which per [ADR-0005](adr/0005-substrate-honest-terminology.md)
is intentionally *outside* the substrate-neutral surface — so this doc is
deliberately **not** part of the [Arc42](arc42/) architecture spine (which
documents the substrate only). Orphan-kept by design.

If you run one agent, or never dispatch the same tracker from two directions, you
don't need this.

## The gap

The bus is the right surface for **coordination conversations**: rapid
back-and-forth on a design fork, a merge-window heads-up, "I'm about to force-push,
hold your rebase." Those are ephemeral and addressed to a known recipient.

The bus is the *wrong* surface for **discoverable persistent state** — facts a
party who isn't in the conversation needs to find later, by looking. "This issue is
mine right now" is exactly that kind of fact. A claim announced on the bus is
visible only to whoever was addressed and awake when it was sent; a dispatcher
scanning the tracker an hour later for "is anyone on this?" sees nothing.

That gap produced a real collision (2026-06-07): one chamber was mid-rewrite on an
issue under an operator dispatch routed over the bus, while a dispatcher — seeing no
assignee, no label, no comment on the issue — handed the same issue to a second
chamber. Both worked ~30 min before it surfaced. No work was lost (the rewrite
rebased cleanly over the other PR), but the duplicated effort was avoidable.

Root cause: **the claim lived on the bus, not on the issue.**

## The convention: assignee-on-claim

When you start substantive work on an issue — after you've picked it up, before you
open the branch — **assign the issue to yourself** on the tracker.

```bash
# Forgejo / Gitea API
curl -s -X PATCH -H "Authorization: token $TOKEN" -H "Content-Type: application/json" \
  -d '{"assignees":["your-agent-name"]}' \
  "$FORGEJO/api/v1/repos/<owner>/<repo>/issues/<N>"
```

That's the whole claimant side. The assignee field is explicit, visible on every
issue-list view, queryable via the API without reading any comment thread, and it
says *who* — so a dispatcher who finds it knows exactly whom to ping.

The claim's lifecycle matches the issue's: the assignment stands while you hold the
work and clears when the issue closes (the merging PR closes it; the tracker drops
the assignee at close, or you remove it if you stand down before a PR).

## The dispatcher side

Whoever hands out work — an orchestrator, a quartermaster, a human operator —
**checks `assignees` on the target issue before dispatching.**

```bash
curl -s -H "Authorization: token $TOKEN" \
  "$FORGEJO/api/v1/repos/<owner>/<repo>/issues/<N>" | jq '.assignees[].login'
```

If it's non-empty, the issue is already claimed: route through the current assignee
on the bus *first* — confirm they've handed it back or stalled — rather than
dispatching a second agent onto it.

This is a convention, not enforcement. A dispatcher who knowingly re-dispatches a
claimed issue (the assignee is stuck, the work is being reassigned) is free to —
the assignee field is a signal to *look before you leap*, not a lock.

## Fallbacks

Assignee-on-claim is the default because it carries the most information for the
least mechanism. Two weaker signals cover cases it doesn't fit:

- **An `in-progress` label** — a binary "someone's on this" when the tracker tier
  caps single-assignee and a shape needs to read as claimed-by-several. Uniform, but
  it doesn't say who.
- **A claiming comment** — lowest friction, highest discovery cost (only visible by
  reading the thread). A reasonable last resort when neither assignee nor label fits.

Prefer the assignee. Reach for a label or comment only when the assignee field
genuinely can't carry the claim.

## When a crossing happens anyway

Assignee-on-claim avoids collisions on work that *already exists* as an issue. It
structurally **cannot** cover the other recurring shape: a reviewer surfaces an "X
needs a follow-up tracker" gap mid-review, and two chambers — the dispatcher and
whoever's closest to the substrate — both **file the tracker within seconds**.
There is no assignee to check, because the issue they'd check doesn't exist until
they both create it. This crossed five-plus times in one day (2026-06-15); it is
*ambient* in a fast parallel crew, and trying to engineer it away costs more than
it saves.

So the discipline here is not prevention — it is **making the crossing cheap to
resolve**. The operator's framing: *better to make conflict resolution a walk in
the park than to try hard to prevent every conflict at all costs.* The resolution
norm, which already operates crew-wide and is worth naming so a new chamber
inherits it rather than re-deriving it:

1. **Verify substrate-state before re-acting.** When you notice a parallel filing
   (a near-duplicate tracker, a second PR on the same gap), *look at the tracker*
   first — both issues, both authors, timestamps — instead of assuming yours is
   canonical.
2. **Surface the divergence**, briefly, where the other party will see it (a
   comment on the dup, a bus line) — "this overlaps #N, I think #N is the keeper."
3. **Defer to merged-reality.** Whichever tracker is further along — has the
   assignee, the richer body, the downstream references, the in-flight PR — is the
   keeper. Close the other as a duplicate pointing at it. The tie-break is
   *whatever is closer to merged*, **not** whoever filed first or who is senior —
   a later-filed but better-referenced tracker rightly wins.
4. **Don't re-litigate.** Once the keeper is chosen, the loser's author does not
   re-argue the framing or re-file. The few seconds of overlap are the whole cost;
   re-litigating is what would turn an optical overlap into actual rework.

Worked instance (2026-06-15): two chambers filed #461 (22:43) and #462 (22:45)
two minutes apart during a #454 review, both for the same
"canonical-substrate-vs-curated-surface ADR" gap. Resolution was the four steps
above — the **later**-filed #462 was kept because it carried the band/labels and
the downstream review reference (merged-reality), and the earlier #461 closed
itself as a duplicate pointing at #462 ("No content lost"). Seconds, zero rework,
and note the tie-break ran on *closer-to-canonical*, not *filed-first*. That is
the target state, and it is already how crossings resolve; this section just makes
it transferable.

**Forward-watch, not dup-rate.** The success metric for this discipline is that
**resolution stays cheap** (a crossing resolves in seconds, no rework), *not* that
crossings stop happening. A measurable rise in resolution cost across reviews is
the signal that something heavier — e.g. a file-time visibility broadcast on the
bus — is worth building. Until then it isn't: a mandated "announce every filing"
step adds friction to chase a gain the cheap-resolution norm already delivers, and
drifts back toward the prevention posture this discipline deliberately retired.

## Out of scope

- Tracker schema changes (multi-assignee tiers, custom fields) — work within the
  default tier.
- Enforced dispatch (rejecting a dispatch when an assignee is already set) —
  convention only; dispatchers keep discretion.
- Mid-work handoffs between agents — this convention assumes one agent holds an
  issue end-to-end. Re-assignment handoffs are a separate shape.
- **Preventing crossings.** A file-time title-prefix lock, a "only the reviewer
  files" rule, or a mandated file-time bus broadcast were all considered and
  declined: at this crew's cadence crossings are ambient and cheap to resolve, so
  prevention costs more friction than the overlap it removes. Cheap resolution (the
  section above) is the chosen posture; the broadcast re-opens only if forward-watch
  shows resolution cost rising.
