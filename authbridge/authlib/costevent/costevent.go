// Package costevent is the canonical wire shape of the per-request cost that
// litellm-budget-track publishes onto a session event.
//
// It lives in its own package because the producer is a plugin while the
// consumers are the usage aggregator and abctl, and none of those should import
// each other. Before this package existed the struct was declared once in the
// plugin, again in abctl, and was about to be declared a third time in the
// aggregator — which is the duplication cortex #910 exists to remove.
//
// Dependency-light on purpose: the aggregator links this on every build,
// including the trimmed "lite" images that exclude the plugin entirely.
package costevent

import (
	"encoding/json"
	"math"

	"github.com/rossoctl/cortex/authbridge/authlib/pipeline"
)

// PluginName is the session-event key the cost event is published under. It must
// equal litellm_budgettrack.BudgetTrack.Name(); a test in that package asserts it.
const PluginName = "litellm-budget-track"

// Source names how the cost was arrived at.
const (
	// SourceGatewayHeader is the gateway's own post-discount figure, read from
	// x-litellm-response-cost. Authoritative.
	SourceGatewayHeader = "gateway-header"
	// SourceUsageFallback is priced from token counters because the header
	// reported 0 — which every streamed response does, so for a streaming agent
	// this is the common case, not the exception.
	SourceUsageFallback = "usage-fallback"
)

// Event is one request's settled cost. Unlike tool-prune's event, which carries
// rates and leaves the arithmetic to the consumer, this carries a finished
// figure.
type Event struct {
	CostUSD float64 `json:"cost_usd"`
	// Source is SourceGatewayHeader or SourceUsageFallback. A consumer that
	// distinguishes authoritative from modelled figures reads this.
	Source        string  `json:"source"`
	DailyTotalUSD float64 `json:"daily_total_usd"`
	DailyMaxUSD   float64 `json:"daily_max_usd"`
}

// Micros converts CostUSD to millionths of a dollar, rounded to nearest.
//
// The integer unit is what usage.Counts accumulates: it keeps bucket addition
// exact and JSON round-tripping lossless, which float dollars are not. A cost
// below half a micro rounds to 0 while still counting as priced — a real
// sub-micro charge, not an unknown one.
func (e Event) Micros() int64 { return int64(math.Round(e.CostUSD * 1e6)) }

// Decode pulls the cost event off a session event.
//
// A false return is the normal case, not an error: the plugin may not be in the
// pipeline, and a request that charged nothing (cache hit, error) emits nothing.
// A non-positive cost also returns false — "free" and "unknown" must not be
// conflated, and a consumer that treated 0 as priced would render $0.00 for
// traffic it simply cannot price.
func Decode(e *pipeline.SessionEvent) (Event, bool) {
	if e == nil || len(e.Plugins) == 0 {
		return Event{}, false
	}
	raw, ok := e.Plugins[PluginName]
	if !ok {
		return Event{}, false
	}
	var ev Event
	if err := json.Unmarshal(raw, &ev); err != nil || ev.CostUSD <= 0 {
		return Event{}, false
	}
	return ev, true
}
