package store

import (
	"fmt"
	"sort"
	"strings"
)

// Priority weights for per-message scheduling (#449). Stored as an INTEGER on
// messages.priority so the scheduler's arithmetic (Strategy-B aging) and any
// ORDER BY stay integer-clean. The 10-spacing leaves room to insert tiers
// (e.g. an "urgent" at 40) without a re-migration. Default is PriorityNormal.
const (
	PriorityLow    = 10
	PriorityNormal = 20
	PriorityHigh   = 30
)

var priorityByName = map[string]int{
	"low":    PriorityLow,
	"normal": PriorityNormal,
	"high":   PriorityHigh,
}
var nameByPriority = map[int]string{
	PriorityLow:    "low",
	PriorityNormal: "normal",
	PriorityHigh:   "high",
}

// ParsePriority maps a level name to its weight. Empty → normal (the default,
// so senders never have to think about priority for routine traffic). Unknown
// names are rejected so a typo fails loud rather than silently degrading.
func ParsePriority(name string) (int, error) {
	if strings.TrimSpace(name) == "" {
		return PriorityNormal, nil
	}
	if w, ok := priorityByName[strings.ToLower(strings.TrimSpace(name))]; ok {
		return w, nil
	}
	return 0, fmt.Errorf("store: unknown priority %q (want low | normal | high)", name)
}

// PriorityName renders a weight as its level name for display; an unrecognized
// weight (a future tier on an older binary) renders as the integer.
func PriorityName(weight int) string {
	if n, ok := nameByPriority[weight]; ok {
		return n
	}
	return fmt.Sprintf("%d", weight)
}

// SchedulerStrategy selects how ClaimNext weights sender-channels against each
// other (#449). Within a channel (one from_agent → to_agent substream) order is
// always FIFO; the strategy only decides which channel's head fires next.
type SchedulerStrategy int

const (
	// StrategyMaxPriority (the default) weights a channel by its highest-priority
	// queued message. Under uniform priority every channel ties → the tiebreak
	// (lowest head id) yields plain global FIFO, so no-priority traffic behaves
	// exactly as before; a buried high-priority message still lifts its whole
	// channel above a normal-only one (anti-starvation) regardless of its depth.
	StrategyMaxPriority SchedulerStrategy = iota
	// StrategyAged weights a channel by max(priority * (1 + position)) — it adds
	// depth-aging among same-priority channels, at the cost of favoring the
	// longest backlog under uniform priority (so it is NOT global-FIFO by
	// default). Opt-in via --priority-strategy=aged.
	StrategyAged
)

// ParseStrategy maps a config name to a SchedulerStrategy. Empty → the default
// (max-priority). "max"/"max-priority" → A; "aged"/"aged-priority" → B.
func ParseStrategy(name string) (SchedulerStrategy, error) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "", "max", "max-priority":
		return StrategyMaxPriority, nil
	case "aged", "aged-priority":
		return StrategyAged, nil
	default:
		return 0, fmt.Errorf("store: unknown priority strategy %q (want max | aged)", name)
	}
}

// claimCandidate is a queued message's scheduling-relevant fields, as read by
// ClaimNext before it picks one to deliver.
type claimCandidate struct {
	ID        int64
	FromAgent string
	Priority  int
}

// selectScheduled picks which queued message ClaimNext delivers next, preserving
// within-channel (per-sender) FIFO while weighting across channels by priority.
// It returns the chosen message id; ok is false only for an empty candidate set.
//
// A channel is a from_agent substream within the recipient's queue (Reading-1,
// #449). The chosen message is always a channel's HEAD (its lowest id), so
// within-channel FIFO is never violated — priority only changes WHICH channel's
// head goes next. Channel weight is per the strategy; ties break to the lowest
// head id (global FIFO when weights are uniform). candidates need not be sorted;
// the function groups + orders internally.
func selectScheduled(candidates []claimCandidate, strategy SchedulerStrategy) (int64, bool) {
	if len(candidates) == 0 {
		return 0, false
	}
	// Stable order: by from_agent, then id — so each channel is contiguous and
	// its first element is the head.
	cs := append([]claimCandidate(nil), candidates...)
	sort.SliceStable(cs, func(i, j int) bool {
		if cs[i].FromAgent != cs[j].FromAgent {
			return cs[i].FromAgent < cs[j].FromAgent
		}
		return cs[i].ID < cs[j].ID
	})

	bestWeight := -1
	var bestHead int64
	found := false
	for i := 0; i < len(cs); {
		j := i
		head := cs[i].ID
		weight := 0
		pos := 0
		for j < len(cs) && cs[j].FromAgent == cs[i].FromAgent {
			var w int
			switch strategy {
			case StrategyAged:
				w = cs[j].Priority * (1 + pos)
			default: // StrategyMaxPriority
				w = cs[j].Priority
			}
			if w > weight {
				weight = w
			}
			pos++
			j++
		}
		if !found || weight > bestWeight || (weight == bestWeight && head < bestHead) {
			bestWeight, bestHead, found = weight, head, true
		}
		i = j
	}
	return bestHead, found
}
