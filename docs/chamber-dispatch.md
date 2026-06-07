# Chamber dispatch: claiming an issue so two agents don't collide

This is a coordination convention for **multi-agent deployments** where several
agents (here, the alcatraz "chambers" — one Claude instance per tmux pane) draw
work from a shared issue tracker, and more than one party can hand out that work.
It is not a tmux-msg feature; it is a discipline that sits *next to* the bus and
covers a gap the bus deliberately doesn't fill.

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

## Out of scope

- Tracker schema changes (multi-assignee tiers, custom fields) — work within the
  default tier.
- Enforced dispatch (rejecting a dispatch when an assignee is already set) —
  convention only; dispatchers keep discretion.
- Mid-work handoffs between agents — this convention assumes one agent holds an
  issue end-to-end. Re-assignment handoffs are a separate shape.
