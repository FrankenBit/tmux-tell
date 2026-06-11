# ADR-0010: Tool name — `tmux-msg`, or rename?

> **Status**: Accepted
> **Date**: 2026-06-10
> **Authors**: Surveyor (re-proposer at operator request); disposition by operator 2026-06-10

## Context

Whether to keep `tmux-msg` or rename has been deliberated once already on the
record, in [closed PR #218](https://git.frankenbit.de/frankenbit/tmux-msg/pulls/218)
(ADR-0009 *Proposed* — file `docs/adr/0009-tool-name.md` on the
`adr-0009-tool-name` branch, not merged). That round accumulated structural
problems worth naming so this round doesn't repeat them:

1. **Framing-tilt acknowledged mid-round.** The original "bar a rename must
   clear" framing favored the incumbent by structurally asking "what must a
   challenger overcome?" rather than "what does each name trade?". Herald
   self-corrected via Amendment, but the Round 1 votes had already landed
   under the tilted bar — and the corrections kept arriving in subsequent
   operator exchanges (architectural-corner commitment, retired
   spontaneous-attraction test, now-is-cheapest timing), some of which never
   surfaced on the PR thread.
2. **Voter cascade.** Round 1 votes were posted in order; each successive
   voter saw the running tally before posting. Round 2 re-votes had the same
   shape. Independent reasoning was contaminated by anchoring.
3. **Candidate slate as pre-curation.** The seed slate (`tmux-msg`,
   `tmux-talk`, `tmux-bus`, `tmux-mail`, `tmux-post`) was Herald's pre-poll
   selection. The honest order is candidate-space expansion *before*
   narrowing — chambers may have non-slate candidates worth surfacing that
   never got a fair hearing.

ADR-0009 (tool-name draft) was never merged; the number is now occupied by
[ADR-0009 (hook-context delivery)](0009-hook-context-delivery-substrate-vs-adapter-boundary.md).
This ADR carries the substantive question forward at ADR-0010 with a
deliberation process engineered to address (1)–(3) above.

The substrate grounding from prior ADRs still frames this:

- **ADR-0003 (substrate-vs-flavor naming)** — repo name reflects what the
  substrate *is*.
- **ADR-0005 (substrate-honest terminology)** — names should be honest about
  what the thing is, not aspirational.

## The bar (corrected)

A candidate is evaluated against **five axes**, weighted on substantive
soundness — there is **no default-incumbent advantage**. The question is the
best compromise from the candidate space, not "does any challenger overcome
the incumbent?"

1. **Substrate-honest under architectural commitment.** The substrate IS
   committed to p2p single-writer-per-mailbox + per-recipient mailman daemon
   delivery. Pub-sub conflicts with single-writer; fan-in conflicts with
   single-recipient; non-mailbox transports conflict with the mailman model.
   *Extending beyond mailbox-shape would be a bigger architectural move than
   a rename — the substrate is already in the mailbox corner.* So a name's
   forward-compat is bounded by that commitment; "lock-in" concerns about
   "what if the substrate ever extends to X" are walked back where X conflicts
   with the architectural commitment. A substrate-honest name *describes* the
   committed shape; it doesn't merely avoid over-claiming a topology the tool
   disclaims.

2. **Fits the adapter grammar.** The binary is `<substrate>-<adapter>`
   (`tmux-msg-claude`, `tmux-msg-codex`, future `tmux-msg-copilot`). A
   candidate must read cleanly as `tmux-<X>-claude`.

3. **Speakability.** How the name reads aloud matters for a tool pitched
   verbally. Operator-confirmed real axis — `tmux-msg` has no clean spoken
   form, candidates with vowel-shapes (`post`, `note`, `send`, `push`) read
   more naturally. Not a marketing concern; a daily-spoken-cost concern.

4. **Tonal match.** The substrate is *informal peer messaging* — paste-and-
   enter delivery, verify-token-as-return-receipt is "did you read this?"
   not "delivered to your registered address," chambers passing messages
   via the mailman daemon. Names connoting formal infrastructure (postal
   service, intercom) carry a tonal-mismatch cost; names connoting informal
   peer (note, message) carry a tonal-match credit.

5. **Churn cost.** A repo/binary rename ripples through every chamber's
   muscle memory + cached Claude session + downstream consumer (Binnacle).
   Real cost, paid once. Pre-1.0 is the cheapest window because:
   - Pre-1.0 minor bumps may carry breaking changes per CONTRIBUTING (semver
     looseness); post-1.0 binds the rename to deprecation cycles
   - A rename baked into v0.17.0 lets the new name *bake under usage* before
     the v1.0 stability commitment, rather than bundling rename + stability
     into one cut
   - The asciinema take (#216) becomes substrate-decisive: capturing the
     headline visual under the final name avoids re-take

The previously-load-bearing "K-reset" bar from ADR-0009 is folded into churn
cost — it is non-discriminating across candidates (every rename pays it
equally) and timing-neutral if a rename happens at all (paid the same
whenever). It does not separately weight.

## Operator-disposition framing

**This is not a democratic vote.** The crew blind-vote produces input
substrate for the operator's call; the operator weighs the aggregate
against substantive soundness and disposes. If the disposition diverges
from the aggregate, the divergence is named explicitly in the Rationale
section appended when ADR-0010 flips to *Accepted*.

This framing is named **up front** so chambers vote knowing their input is
substrate for the call, not binding aggregate — there is no failure mode
where "the votes were ignored." Crew votes carry weight as substantive
reasoning; the disposition follows the soundest argument.

## Deliberation process

Two phases, both routed through Pilot to eliminate voter cascade.

### Phase 1 — three-favorites (candidate-space expansion)

Each crew member sends their **three favorite candidate names** to Pilot via
bus (tmux-msg). Format per candidate: a small table row with name + brief
reasoning (1–3 sentences). No ordering signal among the three; favorites are
unranked.

- Candidates may be net-new or include existing names (`tmux-msg`,
  `tmux-post`, `tmux-note`, etc.). No pre-loaded slate constrains the space.
- Each chamber's three are *private to Pilot* during Phase 1 — Pilot does
  not echo intermediates.
- After Phase 1 soft deadline (or all chambers' submissions received), Pilot
  posts the **aggregate** as a single comment on this PR — every chamber's
  three-favorites, with reasoning, no ordering signal.
- The aggregate fixes the candidate pool for Phase 2.

### Phase 2 — single blind pick (final vote)

Each crew member sends **one pick** from the Phase 1 pool to Pilot via bus,
with a few sentences of reasoning for the chosen name.

- Each chamber's pick is *private to Pilot* during Phase 2.
- After Phase 2 soft deadline (or all chambers' picks received), Pilot posts
  **all picks at once** as a single comment on this PR.
- Chambers vote *blind* — no chamber sees another's Phase 2 pick before
  their own arrives at Pilot.

### Disposition

After the Phase 2 aggregate is posted, the operator calls disposition,
appending a **Rationale** section to this ADR recording the chosen name +
the soundness chain. ADR-0010 flips *Re-Proposed → Accepted*. The rename arc
(if any) files as a v0.17.0 candidate per the timing argument above.

### Soft deadlines

- Phase 1 close: 48h from this PR's open
- Phase 2 close: 48h from Phase 1 aggregate posting
- Pilot waits for whichever arrives first: all chambers' submissions, or
  the deadline. Chambers that don't submit count as abstain.

## Historical record

Closed PR #218 carries the deliberation history of the first attempt —
seed slate, Round 1 votes, Herald's Amendment, Round 2 re-votes, operator-
side bar-shifts surfaced bilaterally. Reading that thread is **not required**
for this round; the corrected bar above is self-sufficient. The history is
preserved for fact-archeology.

Substantive landings carried forward from PR #218 (now ADR-text above, not
just historical):

- The substrate is architecturally committed to mailbox-shape (operator,
  via bilateral exchange with Surveyor 2026-06-10) — "we are already in
  that corner."
- Speakability is a real axis the original bar omitted (Herald Amendment +
  operator-confirmed daily experience).
- Operator's spontaneous-attraction test ("something we (almost) all
  spontaneously fall in love with") is retired (operator, 2026-06-10) —
  what remains is best-available compromise.
- Now is the cheapest timing window if a rename happens at all (operator,
  2026-06-10) — K-cost is timing-neutral; pre-1.0 admits breaking changes;
  v0.17.0 lets the new name bake before v1.0 stability commitment.

## Alternatives historically considered

*(Reference only — Phase 1 will surface the actual candidate space for this
round. The names below are NOT a curated slate.)*

Candidates analyzed in PR #218: `tmux-msg` (incumbent), `tmux-talk`,
`tmux-bus`, `tmux-mail`, `tmux-post`. See [PR #218 thread](https://git.frankenbit.de/frankenbit/tmux-msg/pulls/218)
for per-candidate pro/con. Operator-surfaced during the 2026-06-10 exchange:
`tmux-note` (with verb-arc analysis — "pass a note / leave a note / check
the notes"), `tmux-memo` (Surveyor brainstorm, retracted for layer-mismatch
with mailman daemon vocab).

Phase 1 submissions may include, exclude, or add to these.

## Consequences

- **If the incumbent disposition holds (`tmux-msg`):** zero rename cost, K
  stays at its current value, the question closes with a citable record.
  The tagline keeps "message bus for CLI agents" as the *category* phrase;
  "bus" survives only as the colloquial *verb*.
- **If a rename disposition wins:** v0.17.0 carries the repo/binary/
  template/env rename behind a one-cycle alias + `WARN deprecated_surface_used`
  (per ADR-0008). The asciinema take (#216) is captured post-rename so the
  README's headline visual is baked under the final name. The K-counter
  resets on tracked surfaces that can't be aliased (notably the Go
  module-path); the 1.0 stability gate reopens with the new name baking
  under usage through v0.17→v1.0. ADR-0003's naming rationale is amended
  (not superseded — the substrate-grounding holds; only the substrate-token
  changes).

## What would change the disposition

If Phase 1 surfaces a candidate that no chamber had considered and that
substantively edges the current pool on multiple bars, the operator may
extend Phase 1 to admit consideration before locking Phase 2's pool.
Otherwise the process runs to disposition on the Phase 2 aggregate.

## Rationale

**Chosen name: `tmux-tell`** — operator disposition 2026-06-10.

### Vote aggregate (Phase 2)

| Pick | Voters |
|------|--------|
| `tmux-post` | Bosun, Engineer, Herald, Quartermaster |
| `tmux-note` | Pilot, Shipwright, Surveyor |
| `tmux-tell` | Operator |

Full deliberation record: [PR #294 comments](https://git.frankenbit.de/frankenbit/tmux-msg/pulls/294).

### Soundness chain

`tmux-tell` won against the plurality (`tmux-post`, 4 votes) and near-plurality
(`tmux-note`, 3 votes) on two axes that prove decisive at the product-name layer:

**Adapter grammar (axis 2).** `tmux-tell-claude` reads as a clean imperative —
"tmux, tell Claude…" The verb-in-compound reads agentically and the grammar holds
across the adapter family (`tmux-tell-codex` equally clean). The plurality
candidate `tmux-post-claude` is genuinely ambiguous: *after* Claude? *to*
Claude? The ambiguity is real at every call site where the adapter name appears.

**Product-name framing over technical-description.** The operator's reframe is
load-bearing: the task is to pick a matching, well-sounding, memorable product
name — not the best fitting technical description in one word. Under that
framing `tell` is warmer and more precise than `post` (tonal-match, axis 4) and
avoids the note-tool market saturation that weights against `tmux-note`
(`tmux-note-claude` reads as a teacher taking notes on pupils).

**The `/tell` heritage is async, not synchronous.** The only nit raised —
synchronous-lean — dissolves under examination: `/tell` in MUD/IRC was
specifically the store-and-forward directed-message primitive (queued for
offline players), not the live split-screen `talk`. The connotation is not
dishonest; paste-and-enter to a live pane is semi-synchronous from the
recipient's view in any case.

**Ship framing.** `tmux-tell` fits the project's naval-operations register
naturally; the verb arc ("tell Bosun", "tell future-me", "tell everyone")
is immediately readable and memorable.

### Forward notes (non-blocking, for the rename arc)

- **MCP wire surface**: `tmux-tell_send` / `tmux-tell_inbox` carries mild
  verb-on-verb redundancy (noted independently by Quartermaster and Surveyor).
  Lever is noun-shaped method names at the rename arc — design exercise, not
  a blocker.
- **Description line accuracy**: since `tell` leans synchronous in connotation
  while the substrate grows explicitly-async modes (deferred delivery, mailbox-
  only, hook-context), keep description prose accurate that delivery is
  store-and-forward. Same spirit as retiring "bus" from the tagline.

### Crew buy-in

6 of 7 crew members responded with genuine buy-in after disposition broadcast
(Pilot drove the Phase 1 + Phase 2 aggregates and the disposition broadcast
itself, so their buy-in is implicit-by-driving rather than explicit-by-response;
no chamber raised a concern that the operator's reasoning did not address).

## References

- ADR-0003 (substrate-vs-flavor naming) · ADR-0005 (substrate-honest
  terminology) · ADR-0008 (deprecation policy) · ADR-0009 (hook-context
  delivery — unrelated; collided number)
- [Closed PR #218](https://git.frankenbit.de/frankenbit/tmux-msg/pulls/218)
  — deliberation history of the first attempt
- [PR #294](https://git.frankenbit.de/frankenbit/tmux-msg/pulls/294)
  — ADR-0010 blind-vote deliberation (Phase 1 + Phase 2 aggregates)
- #163 — K-counter tracker
- #216 — asciinema take (timing-coupled to disposition)
- 2026-06-07 / 2026-06-08 / 2026-06-10 crew + operator exchanges (PR #218
  thread + bilateral Surveyor↔operator session)
