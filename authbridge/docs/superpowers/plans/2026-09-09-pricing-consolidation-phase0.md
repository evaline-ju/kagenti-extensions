# Consolidated Model Pricing — Phase 0 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make `/v1/usage` and `abctl` report real per-request cost by having the usage aggregator consume the cost event `litellm-budget-track` already publishes, and report *coverage* so a partially-priced window is never presented as a complete total.

**Architecture:** `litellm-budget-track` already emits a settled per-request dollar figure onto the session event (`emitCost`, `plugin.go:303-309`). The aggregator ignores it and instead offers an unused `Pricer` hook. This phase promotes that event's wire shape into a new `authlib/costevent` package consumed by both producer and aggregator, adds a `PricedRequests` counter to `Counts` so coverage folds correctly across buckets, decodes the event in `foldInto`, and deletes the dead `Pricer` seam. No pricing tables, no rate resolution, no discovery — those are phases 1-7.

**Tech Stack:** Go 1.26.5, multi-module workspace (`authbridge/go.work`). Stdlib only — no new dependencies.

**Spec:** `authbridge/docs/superpowers/specs/2026-09-09-pricing-consolidation-design.md` (see its *Phasing* section, phase 0, and *What has already landed upstream*)

**Issue:** cortex #910

## Global Constraints

- **Go version:** 1.26.5 (`authbridge/go.work`). Do not raise it.
- **Module:** all code in this phase lives in `github.com/rossoctl/cortex/authbridge/authlib`. No changes to other modules — `abctl` adopts `costevent` in a later phase.
- **No new dependencies.** Stdlib `encoding/json` and `math` only.
- **Test command:** `cd authbridge/authlib && go test -race ./...` — matches CI (`.github/workflows/ci.yaml:49,67`).
- **Lint:** `make lint` from the repo root (pre-commit hooks) before each commit.
- **DCO:** every commit uses `git commit -s`. Required or CI fails.
- **Attribution:** use `Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>`. **Never** `Co-Authored-By` — a `commit-msg` hook rejects it.
- **Wire compatibility is mandatory.** The four JSON tags `cost_usd`, `source`, `daily_total_usd`, `daily_max_usd` must not change. `abctl` decodes them today (`cmd/abctl/tui/cost_event.go:18-35`) under a test that pins every field, and `abctl` is a separate module not being rebuilt in this phase.
- **Unpriced must never render as `$0.00`.** A zero cost and an unknown cost are different answers. This is existing house style — see `cost_event.go:65-68` and `usage/snapshot.go:58-61`.
- **Redirect verbose command output to a file** and report only the exit code, per the repo's context-budget rule. `export LOG_DIR=/tmp/rossoctl/tdd/pricing-phase0 && mkdir -p $LOG_DIR`.

---

## File Structure

**Create**

- `authbridge/authlib/costevent/costevent.go` — canonical wire shape of the per-request cost event, plus its decoder and the `USD → micros` conversion. Its own package (not inside `usage` or the plugin) because the producer is a plugin, the consumers are the aggregator and later `abctl`, and none of those should import each other. Small and dependency-light: it imports only `encoding/json`, `math`, and `authlib/pipeline`.
- `authbridge/authlib/costevent/costevent_test.go` — decode cases and the tag-pinning test.

**Modify**

- `authbridge/authlib/plugins/litellm_budgettrack/plugin.go:88-101,303-315` — drop the local `costEvent` struct and `source*` constants; emit `costevent.Event`.
- `authbridge/authlib/usage/usage.go:46-64` — add `Counts.PricedRequests` and fold it in `add`.
- `authbridge/authlib/usage/usage.go:399-420` — decode the cost event in `foldInto`.
- `authbridge/authlib/usage/usage.go:107-129,192-193` — delete `Pricer`, `WithPricer`, and the `pricer` field; retire the stale `TODO(cost)`.
- `authbridge/authlib/usage/snapshot.go:56-61,224` — re-derive `Priced` from `PricedRequests`.
- `authbridge/authlib/sessionapi/usage.go:46-63` — retire the stale `TODO(cost)`.
- `authbridge/docs/session-events.md` *(if it documents the wire)* and `authbridge/docs/litellm-budgettrack-plugin.md` — note the shared event package.

---

## Task 1: `authlib/costevent` — canonical cost-event wire shape

**Files:**
- Create: `authbridge/authlib/costevent/costevent.go`
- Test: `authbridge/authlib/costevent/costevent_test.go`

**Interfaces:**
- Consumes: `pipeline.SessionEvent` (its `Plugins map[string]json.RawMessage` field, `pipeline/session.go:108`).
- Produces: `costevent.PluginName`, `costevent.SourceGatewayHeader`, `costevent.SourceUsageFallback`, `costevent.Event{CostUSD, Source, DailyTotalUSD, DailyMaxUSD}`, `Event.Micros() int64`, `costevent.Decode(*pipeline.SessionEvent) (Event, bool)`.

- [ ] **Step 1: Write the failing test**

Create `authbridge/authlib/costevent/costevent_test.go`:

```go
package costevent

import (
	"encoding/json"
	"testing"

	"github.com/rossoctl/cortex/authbridge/authlib/pipeline"
)

// eventWith builds a SessionEvent carrying raw JSON under the cost-event key.
func eventWith(t *testing.T, key, raw string) *pipeline.SessionEvent {
	t.Helper()
	return &pipeline.SessionEvent{
		Plugins: map[string]json.RawMessage{key: json.RawMessage(raw)},
	}
}

func TestDecode(t *testing.T) {
	tests := []struct {
		name     string
		event    *pipeline.SessionEvent
		wantOK   bool
		wantCost float64
		wantSrc  string
	}{
		{
			name:     "gateway header cost",
			event:    eventWith(t, PluginName, `{"cost_usd":0.0421,"source":"gateway-header","daily_total_usd":1.5,"daily_max_usd":10}`),
			wantOK:   true,
			wantCost: 0.0421,
			wantSrc:  SourceGatewayHeader,
		},
		{
			name:     "usage fallback cost",
			event:    eventWith(t, PluginName, `{"cost_usd":0.01,"source":"usage-fallback"}`),
			wantOK:   true,
			wantCost: 0.01,
			wantSrc:  SourceUsageFallback,
		},
		{
			// A cache hit or error charges nothing and the plugin emits nothing;
			// a zero that does arrive must not read as a priced free request.
			name:   "zero cost is not priced",
			event:  eventWith(t, PluginName, `{"cost_usd":0,"source":"gateway-header"}`),
			wantOK: false,
		},
		{
			name:   "negative cost rejected",
			event:  eventWith(t, PluginName, `{"cost_usd":-1,"source":"gateway-header"}`),
			wantOK: false,
		},
		{
			name:   "malformed JSON rejected",
			event:  eventWith(t, PluginName, `{"cost_usd":`),
			wantOK: false,
		},
		{
			name:   "different plugin key ignored",
			event:  eventWith(t, "tool-prune", `{"cost_usd":5}`),
			wantOK: false,
		},
		{
			name:   "no plugins map",
			event:  &pipeline.SessionEvent{},
			wantOK: false,
		},
		{
			name:   "nil event",
			event:  nil,
			wantOK: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := Decode(tc.event)
			if ok != tc.wantOK {
				t.Fatalf("Decode ok = %v, want %v", ok, tc.wantOK)
			}
			if !tc.wantOK {
				return
			}
			if got.CostUSD != tc.wantCost {
				t.Errorf("CostUSD = %v, want %v", got.CostUSD, tc.wantCost)
			}
			if got.Source != tc.wantSrc {
				t.Errorf("Source = %q, want %q", got.Source, tc.wantSrc)
			}
		})
	}
}

// TestEventJSONTagsArePinned guards the wire format. abctl decodes these exact
// tags from a separate module that this phase does not rebuild, so a rename here
// would silently blank its COST column.
func TestEventJSONTagsArePinned(t *testing.T) {
	b, err := json.Marshal(Event{
		CostUSD:       0.25,
		Source:        SourceGatewayHeader,
		DailyTotalUSD: 3.5,
		DailyMaxUSD:   10,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	const want = `{"cost_usd":0.25,"source":"gateway-header","daily_total_usd":3.5,"daily_max_usd":10}`
	if string(b) != want {
		t.Errorf("wire format changed:\n got %s\nwant %s", b, want)
	}
}

func TestMicros(t *testing.T) {
	tests := []struct {
		name string
		usd  float64
		want int64
	}{
		{"whole dollar", 1, 1_000_000},
		{"typical request", 0.0421, 42_100},
		{"rounds to nearest", 0.0000005, 1},
		{"sub-micro rounds to zero", 0.0000001, 0},
		{"large", 1234.5678, 1_234_567_800},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := (Event{CostUSD: tc.usd}).Micros(); got != tc.want {
				t.Errorf("Micros() = %d, want %d", got, tc.want)
			}
		})
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

```bash
cd authbridge/authlib && go test ./costevent/ 2>&1 | tail -5
```

Expected: FAIL — the `costevent` package does not exist yet (`no Go files in .../costevent`).

- [ ] **Step 3: Write minimal implementation**

Create `authbridge/authlib/costevent/costevent.go`:

```go
// Package costevent is the canonical wire shape of the per-request cost that
// litellm-budget-track publishes onto a session event.
//
// It lives in its own package because the producer is a plugin while the
// consumers are the usage aggregator and abctl, and none of those should import
// each other. Before this package existed the struct was declared three times —
// once in the plugin, once in abctl, and about to be a third time in the
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
```

- [ ] **Step 4: Run test to verify it passes**

```bash
cd authbridge/authlib && go test -race ./costevent/ 2>&1 | tail -5
```

Expected: `ok  github.com/rossoctl/cortex/authbridge/authlib/costevent`

- [ ] **Step 5: Commit**

```bash
git add authbridge/authlib/costevent/
git commit -s -m "feat: Add costevent, the canonical per-request cost wire shape

The cost litellm-budget-track publishes was declared once in the plugin
and again in abctl, and the usage aggregator was about to add a third
copy. One package, one struct, one decoder.

Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>"
```

---

## Task 2: `litellm-budget-track` emits `costevent.Event`

**Files:**
- Modify: `authbridge/authlib/plugins/litellm_budgettrack/plugin.go:88-101` (delete local struct + constants), `:303-315` (`emitCost`)
- Test: `authbridge/authlib/plugins/litellm_budgettrack/plugin_test.go`

**Interfaces:**
- Consumes: `costevent.Event`, `costevent.PluginName`, `costevent.SourceGatewayHeader`, `costevent.SourceUsageFallback` from Task 1.
- Produces: no new exported API. The published JSON is byte-identical to before.

- [ ] **Step 1: Write the failing test**

Append to `authbridge/authlib/plugins/litellm_budgettrack/plugin_test.go`:

```go
// TestPluginNameMatchesCostEventKey pins the plugin name to the constant the
// aggregator and abctl look the event up by. A rename on one side only would
// make every consumer silently stop seeing costs.
func TestPluginNameMatchesCostEventKey(t *testing.T) {
	if got := (&BudgetTrack{}).Name(); got != costevent.PluginName {
		t.Errorf("Name() = %q, costevent.PluginName = %q", got, costevent.PluginName)
	}
}

// TestEmitCostPublishesCostEvent asserts the published value is a
// costevent.Event and that its JSON is unchanged from the local struct it
// replaces.
func TestEmitCostPublishesCostEvent(t *testing.T) {
	p := &BudgetTrack{}
	p.cfg.MaxBudget = 10
	pctx := &pipeline.Context{}

	p.emitCost(pctx, 0.25, costevent.SourceGatewayHeader, 3.5)

	raw, ok := pctx.Extensions.Custom[p.Name()+pipeline.PluginEventSuffix]
	if !ok {
		t.Fatalf("no event published under %q", p.Name()+pipeline.PluginEventSuffix)
	}
	ev, ok := raw.(costevent.Event)
	if !ok {
		t.Fatalf("published value is %T, want costevent.Event", raw)
	}
	if ev.CostUSD != 0.25 || ev.Source != costevent.SourceGatewayHeader ||
		ev.DailyTotalUSD != 3.5 || ev.DailyMaxUSD != 10 {
		t.Errorf("unexpected event: %+v", ev)
	}

	b, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	const want = `{"cost_usd":0.25,"source":"gateway-header","daily_total_usd":3.5,"daily_max_usd":10}`
	if string(b) != want {
		t.Errorf("wire format changed:\n got %s\nwant %s", b, want)
	}
}
```

Add these imports to the test file if absent: `encoding/json`, and `"github.com/rossoctl/cortex/authbridge/authlib/costevent"`.

- [ ] **Step 2: Run test to verify it fails**

```bash
cd authbridge/authlib && go test ./plugins/litellm_budgettrack/ -run 'TestPluginNameMatchesCostEventKey|TestEmitCostPublishesCostEvent' 2>&1 | tail -8
```

Expected: FAIL — the published value is the package-local `costEvent`, not `costevent.Event`.

- [ ] **Step 3: Write minimal implementation**

In `plugin.go`, delete the local `costEvent` struct and the two `source*` constants (currently `:88-101`), then re-point `emitCost`:

```go
func (p *BudgetTrack) emitCost(pctx *pipeline.Context, cost float64, source string, dailyTotal float64) {
	if pctx.Extensions.Custom == nil {
		pctx.Extensions.Custom = map[string]any{}
	}
	pctx.Extensions.Custom[p.Name()+pipeline.PluginEventSuffix] = costevent.Event{
		CostUSD:       cost,
		Source:        source,
		DailyTotalUSD: dailyTotal,
		DailyMaxUSD:   p.cfg.MaxBudget,
	}
}
```

Replace the two call sites' constants (`plugin.go:204`, `:255`, `:270`) with the `costevent.` forms:

```go
p.emitCost(pctx, cost, costevent.SourceGatewayHeader, total)   // header path
source := costevent.SourceGatewayHeader                         // terminal-frame path
source = costevent.SourceUsageFallback                          // when priced from usage
```

Add the import:

```go
"github.com/rossoctl/cortex/authbridge/authlib/costevent"
```

- [ ] **Step 4: Run tests to verify they pass**

```bash
cd authbridge/authlib && go test -race ./plugins/litellm_budgettrack/ > $LOG_DIR/budgettrack.log 2>&1; echo "EXIT:$?"
```

Expected: `EXIT:0`. The pre-existing tests in this package are the regression oracle — every one must still pass unchanged. If any fail, the wire shape or a constant changed; fix rather than update the expectation.

- [ ] **Step 5: Commit**

```bash
git add authbridge/authlib/plugins/litellm_budgettrack/
git commit -s -m "refactor: Emit the shared costevent.Event from litellm-budget-track

Same four JSON fields, same values, one fewer declaration of them.

Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>"
```

---

## Task 3: `Counts.PricedRequests` coverage counter

**Files:**
- Modify: `authbridge/authlib/usage/usage.go:46-64`
- Test: `authbridge/authlib/usage/usage_test.go`

**Interfaces:**
- Produces: `usage.Counts.PricedRequests int64`, summed by the existing unexported `(*Counts).add`.

- [ ] **Step 1: Write the failing test**

Append to `authbridge/authlib/usage/usage_test.go`:

```go
// TestCountsAddFoldsPricedRequests is why coverage is a counter and not a
// boolean: buckets are summed when a client asks for a coarser resolution, and
// a bool cannot express "12 of 40 requests in this window were priced".
func TestCountsAddFoldsPricedRequests(t *testing.T) {
	a := Counts{Requests: 10, CostMicros: 500, PricedRequests: 4}
	a.add(Counts{Requests: 5, CostMicros: 250, PricedRequests: 3})

	if a.Requests != 15 {
		t.Errorf("Requests = %d, want 15", a.Requests)
	}
	if a.CostMicros != 750 {
		t.Errorf("CostMicros = %d, want 750", a.CostMicros)
	}
	if a.PricedRequests != 7 {
		t.Errorf("PricedRequests = %d, want 7", a.PricedRequests)
	}
	if unpriced := a.Requests - a.PricedRequests; unpriced != 8 {
		t.Errorf("unpriced = %d, want 8", unpriced)
	}
}

// TestCountsPricedRequestsOmittedWhenZero keeps the wire quiet for deployments
// that price nothing.
func TestCountsPricedRequestsOmittedWhenZero(t *testing.T) {
	b, err := json.Marshal(Counts{Requests: 3})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(b), "pricedRequests") {
		t.Errorf("zero PricedRequests should be omitted, got %s", b)
	}
}
```

Add `encoding/json` and `strings` to the test file's imports if absent.

- [ ] **Step 2: Run test to verify it fails**

```bash
cd authbridge/authlib && go test ./usage/ -run TestCounts 2>&1 | tail -6
```

Expected: FAIL — `unknown field PricedRequests in struct literal`.

- [ ] **Step 3: Write minimal implementation**

In `usage.go`, add the field to `Counts` after `CostMicros`:

```go
	// PricedRequests counts the requests that actually produced a cost. Coverage
	// is a counter rather than a flag because buckets are summed when a client
	// asks for a coarser resolution, and because a deployment can price some of
	// its traffic and not the rest: several endpoints, rates known for some.
	// Requests-minus-PricedRequests is the gap, correct at every resolution, and
	// it is what stops a partial total being presented as a complete one.
	PricedRequests int64 `json:"pricedRequests,omitempty"`
```

and to `add`:

```go
	c.PricedRequests += o.PricedRequests
```

- [ ] **Step 4: Run tests to verify they pass**

```bash
cd authbridge/authlib && go test -race ./usage/ > $LOG_DIR/usage-counts.log 2>&1; echo "EXIT:$?"
```

Expected: `EXIT:0`.

- [ ] **Step 5: Commit**

```bash
git add authbridge/authlib/usage/
git commit -s -m "feat: Count priced requests per bucket

Coverage has to fold with the buckets, so it is a counter. A snapshot
boolean cannot say '12 of 40 were priced'.

Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>"
```

---

## Task 4: `foldInto` records the cost event

**Files:**
- Modify: `authbridge/authlib/usage/usage.go:399-420`
- Test: `authbridge/authlib/usage/usage_test.go`

**Interfaces:**
- Consumes: `costevent.Decode`, `Event.Micros` (Task 1); `Counts.PricedRequests` (Task 3).
- Produces: `Aggregator.Record` now populates `CostMicros` and `PricedRequests` with no `Pricer` configured.

- [ ] **Step 1: Write the failing test**

Append to `authbridge/authlib/usage/usage_test.go`:

```go
// responseEventWithCost builds the response-phase event a listener would append,
// carrying the plugin cost event under its published key.
func responseEventWithCost(t *testing.T, model string, totalTokens int, costUSD float64) *pipeline.SessionEvent {
	t.Helper()
	ev := &pipeline.SessionEvent{
		At:        time.Now(),
		Phase:     pipeline.SessionResponse,
		RequestID: "req-1",
		Host:      "litellm.corp",
		Inference: &pipeline.InferenceExtension{Model: model, TotalTokens: totalTokens},
	}
	if costUSD > 0 {
		raw, err := json.Marshal(costevent.Event{
			CostUSD: costUSD,
			Source:  costevent.SourceGatewayHeader,
		})
		if err != nil {
			t.Fatalf("marshal cost event: %v", err)
		}
		ev.Plugins = map[string]json.RawMessage{costevent.PluginName: raw}
	}
	return ev
}

func TestRecordPricesFromCostEvent(t *testing.T) {
	a := New()
	a.Record("sess-1", responseEventWithCost(t, "claude-opus-5", 1000, 0.0421))

	snap := a.Snapshot(10*BucketWidth, BucketWidth, "", GroupNone)
	if snap.Totals.Requests != 1 {
		t.Fatalf("Requests = %d, want 1", snap.Totals.Requests)
	}
	if snap.Totals.CostMicros != 42_100 {
		t.Errorf("CostMicros = %d, want 42100", snap.Totals.CostMicros)
	}
	if snap.Totals.PricedRequests != 1 {
		t.Errorf("PricedRequests = %d, want 1", snap.Totals.PricedRequests)
	}
}

// A request with no cost event still counts, and still contributes tokens — it
// just contributes no dollars. This is the partial-coverage case.
func TestRecordMixedCoverage(t *testing.T) {
	a := New()
	a.Record("sess-1", responseEventWithCost(t, "claude-opus-5", 1000, 0.02))
	a.Record("sess-1", responseEventWithCost(t, "some-other-model", 500, 0))

	snap := a.Snapshot(10*BucketWidth, BucketWidth, "", GroupNone)
	if snap.Totals.Requests != 2 {
		t.Fatalf("Requests = %d, want 2", snap.Totals.Requests)
	}
	if snap.Totals.PricedRequests != 1 {
		t.Errorf("PricedRequests = %d, want 1", snap.Totals.PricedRequests)
	}
	if snap.Totals.CostMicros != 20_000 {
		t.Errorf("CostMicros = %d, want 20000 (only the priced request)", snap.Totals.CostMicros)
	}
	if snap.Totals.Tokens != 1500 {
		t.Errorf("Tokens = %d, want 1500 (both requests)", snap.Totals.Tokens)
	}
}

// Tokens are unaffected by pricing: a request with no cost event is not a
// request with no traffic.
func TestRecordUnpricedContributesNoCost(t *testing.T) {
	a := New()
	a.Record("sess-1", responseEventWithCost(t, "claude-opus-5", 700, 0))

	snap := a.Snapshot(10*BucketWidth, BucketWidth, "", GroupNone)
	if snap.Totals.CostMicros != 0 {
		t.Errorf("CostMicros = %d, want 0", snap.Totals.CostMicros)
	}
	if snap.Totals.PricedRequests != 0 {
		t.Errorf("PricedRequests = %d, want 0", snap.Totals.PricedRequests)
	}
	if snap.Totals.Tokens != 700 {
		t.Errorf("Tokens = %d, want 700", snap.Totals.Tokens)
	}
}
```

Add imports if absent: `encoding/json`, `time`, `"github.com/rossoctl/cortex/authbridge/authlib/costevent"`, `"github.com/rossoctl/cortex/authbridge/authlib/pipeline"`.

- [ ] **Step 2: Run test to verify it fails**

```bash
cd authbridge/authlib && go test ./usage/ -run TestRecord 2>&1 | tail -10
```

Expected: FAIL — `CostMicros = 0, want 42100`. Nothing populates cost yet.

- [ ] **Step 3: Write minimal implementation**

In `foldInto`, replace the tokens/cost block (`usage.go:405-415`) with:

```go
	var tokens int64
	var model string
	if e.Inference != nil {
		tokens = int64(e.Inference.TotalTokens)
		model = e.Inference.Model
	}

	// Cost comes from the plugin that already settled it, not from a rate table
	// here: litellm-budget-track prefers the gateway's own post-discount figure
	// and falls back to pricing the usage block, and it publishes the result on
	// the event. Absent means unpriced, which is not the same as free — see
	// Counts.PricedRequests.
	var cost, priced int64
	if ce, ok := costevent.Decode(e); ok {
		cost = ce.Micros()
		priced = 1
	}

	one := Counts{Requests: 1, Tokens: tokens, CostMicros: cost, PricedRequests: priced}
```

Add the import:

```go
"github.com/rossoctl/cortex/authbridge/authlib/costevent"
```

- [ ] **Step 4: Run tests to verify they pass**

```bash
cd authbridge/authlib && go test -race ./usage/ ./sessionapi/ > $LOG_DIR/usage-fold.log 2>&1; echo "EXIT:$?"
```

Expected: `EXIT:0`. Every pre-existing test in both packages must pass unchanged.

- [ ] **Step 5: Commit**

```bash
git add authbridge/authlib/usage/
git commit -s -m "feat: Record per-request cost in the usage aggregator

The figure was already on the session event; the aggregator just never
read it. /v1/usage reports real dollars now, plus how many of the
requests in the window contributed one.

Closes the TODO(cost) in usage.go and sessionapi/usage.go.

Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>"
```

---

## Task 5: Derive `Priced`, delete the dead `Pricer` seam

**Files:**
- Modify: `authbridge/authlib/usage/usage.go:107-129` (delete `Pricer`), `:145` (`pricer` field), `:192-193` (`WithPricer`), `authbridge/authlib/usage/snapshot.go:56-61,224`, `authbridge/authlib/sessionapi/usage.go:46-63`
- Test: `authbridge/authlib/usage/snapshot_test.go`

**Interfaces:**
- Consumes: `Counts.PricedRequests` (Task 3), populated by `foldInto` (Task 4).
- Produces: `Snapshot.Priced` now means "at least one request in this window was priced". `usage.Pricer`, `usage.WithPricer` and the `Aggregator.pricer` field no longer exist.

- [ ] **Step 1: Write the failing test**

Append to `authbridge/authlib/usage/snapshot_test.go`:

```go
// Priced is derived from what actually happened, not from whether a hook was
// installed. A window that priced nothing must report false so a client renders
// "cost unavailable" rather than $0.00.
func TestSnapshotPricedDerivedFromCoverage(t *testing.T) {
	t.Run("nothing priced", func(t *testing.T) {
		a := New()
		a.Record("s", responseEventWithCost(t, "m", 100, 0))
		snap := a.Snapshot(10*BucketWidth, BucketWidth, "", GroupNone)
		if snap.Priced {
			t.Error("Priced = true, want false when no request was priced")
		}
	})

	t.Run("something priced", func(t *testing.T) {
		a := New()
		a.Record("s", responseEventWithCost(t, "m", 100, 0.01))
		snap := a.Snapshot(10*BucketWidth, BucketWidth, "", GroupNone)
		if !snap.Priced {
			t.Error("Priced = false, want true when a request was priced")
		}
	})

	t.Run("partially priced still reports priced", func(t *testing.T) {
		a := New()
		a.Record("s", responseEventWithCost(t, "m", 100, 0.01))
		a.Record("s", responseEventWithCost(t, "n", 100, 0))
		snap := a.Snapshot(10*BucketWidth, BucketWidth, "", GroupNone)
		if !snap.Priced {
			t.Error("Priced = false, want true")
		}
		if snap.Totals.PricedRequests != 1 || snap.Totals.Requests != 2 {
			t.Errorf("coverage = %d/%d, want 1/2",
				snap.Totals.PricedRequests, snap.Totals.Requests)
		}
	})
}
```

- [ ] **Step 2: Run test to verify it fails**

```bash
cd authbridge/authlib && go test ./usage/ -run TestSnapshotPricedDerived 2>&1 | tail -8
```

Expected: FAIL on the "something priced" subtest — `Priced` is still `a.pricer != nil`, which is always `nil`.

- [ ] **Step 3: Write minimal implementation**

`Priced` is currently set in the `Snapshot{...}` literal at `snapshot.go:224`, which runs *before* the loop that accumulates `out.Totals`. It cannot be derived there. **Delete the line from the literal:**

```go
		Priced:        a.pricer != nil,   // <- delete this line
```

and set it after the accumulation loop, immediately before the `fold` call at `snapshot.go:244`:

```go
	// Derived after the loop: Totals is only complete once every bucket has been
	// added. Placed before fold because fold rewrites Buckets, not Totals.
	out.Priced = out.Totals.PricedRequests > 0

	// Fold last: totals are summed from the raw buckets above and are unaffected
	// by grouping width, so a client's summary line agrees with its chart no
	// matter which resolution it asked for.
	out.Buckets = fold(out.Buckets, resolution)
	return out
```

Replace the `Priced` doc comment (`snapshot.go:58-61`):

```go
	// Priced reports whether ANY request in this window produced a cost. False
	// means CostMicros is absent everywhere and a client must say "cost
	// unavailable" rather than display $0.00 — which would read as "this traffic
	// was free".
	//
	// True does NOT mean every request was priced. Compare Totals.PricedRequests
	// against Totals.Requests: rates are per-endpoint, so a deployment talking to
	// several endpoints can price some traffic and not the rest, and the total
	// then covers only the priced subset. A client showing a dollar figure from a
	// partial window must say so.
	Priced bool `json:"priced"`
```

Delete the `Pricer` type and its `TODO(cost)` block (`usage.go:107-129`), the `pricer Pricer` field (`usage.go:145`), and `WithPricer` (`usage.go:192-193`).

Replace the stale `TODO(cost)` in `sessionapi/usage.go:46-63` with:

```go
// Cost is populated from the per-request figure litellm-budget-track publishes on
// the session event: it prefers the gateway's own post-discount cost header and
// falls back to pricing the usage block, so the number here is the plugin's, not
// a rate table's. Requests the plugin did not price contribute no cost and are
// counted in the gap between totals.pricedRequests and totals.requests; a client
// rendering a dollar figure from a window where those differ must present it as
// partial. priced:false means nothing was priced at all — render "cost
// unavailable", not $0.00.
//
// Modelled rates for traffic that has no cost event (no litellm-budget-track in
// the pipeline) arrive with the pricing resolver — see
// docs/superpowers/specs/2026-09-09-pricing-consolidation-design.md.
```

- [ ] **Step 4: Run the full module test suite**

```bash
cd authbridge/authlib && go test -race ./... > $LOG_DIR/authlib-full.log 2>&1; echo "EXIT:$?"
```

Expected: `EXIT:0`. If a test referenced `WithPricer`, delete that test — it was asserting on a seam that never had a producer.

Then confirm the other modules still build, since `go.work` links them:

```bash
cd authbridge && go build ./... > $LOG_DIR/build-all.log 2>&1; echo "EXIT:$?"
cd cmd/abctl && go build ./... > $LOG_DIR/build-abctl.log 2>&1; echo "EXIT:$?"
```

Expected: `EXIT:0` for both. `abctl` keeps its own copy of the event struct in this phase, so it must be unaffected.

- [ ] **Step 5: Lint and commit**

```bash
make lint > $LOG_DIR/lint.log 2>&1; echo "EXIT:$?"
git add authbridge/authlib/usage/ authbridge/authlib/sessionapi/
git commit -s -m "refactor: Derive Priced from coverage, delete the unused Pricer seam

Priced meant 'a Pricer was installed' and no caller ever installed one,
so it was always false. It now reports whether anything was actually
priced, and Totals.PricedRequests says how much of the window that
covers.

Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>"
```

---

## Task 6: Document the wire change

**Files:**
- Modify: `authbridge/docs/litellm-budgettrack-plugin.md`, `authbridge/CLAUDE.md:453-510` (the Session Events API section, `/v1/usage` row)

**Interfaces:**
- Consumes: nothing. Documentation only.
- Produces: nothing.

- [ ] **Step 1: Find the surfaces that document the wire**

```bash
cd authbridge && grep -rn "priced\|costMicros\|cost_usd" docs/ CLAUDE.md | grep -v superpowers
```

Record the hits — those are the places that now describe stale behaviour.

- [ ] **Step 2: Update each hit**

For every occurrence, apply these facts:

- `costMicros` is populated whenever `litellm-budget-track` ran and priced the request.
- `pricedRequests` is new on `Counts`; `requests - pricedRequests` is the unpriced gap.
- `priced` now means "at least one request was priced", not "a pricer is configured".
- The event shape lives in `authlib/costevent` and is shared by the plugin and the aggregator.

In `docs/litellm-budgettrack-plugin.md`, add to the session-event section:

```markdown
The event's wire shape is defined once in `authlib/costevent` (`costevent.Event`)
and consumed by the usage aggregator, so `/v1/usage` reports the same figure this
plugin enforces its budget against.
```

- [ ] **Step 3: Verify no stale statement remains**

```bash
cd authbridge && grep -rn "Pricer\|WithPricer" docs/ CLAUDE.md | grep -v superpowers || echo "clean"
```

Expected: `clean`.

- [ ] **Step 4: Commit**

```bash
git add authbridge/docs/ authbridge/CLAUDE.md
git commit -s -m "docs: Record that /v1/usage now reports cost and coverage

Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>"
```

---

## Definition of done

- [ ] `cd authbridge/authlib && go test -race ./...` passes.
- [ ] `cd authbridge && go build ./...` and `cd authbridge/cmd/abctl && go build ./...` both succeed.
- [ ] `make lint` passes.
- [ ] `grep -rn "TODO(cost)" authbridge/` returns nothing.
- [ ] `grep -rn "Pricer" authbridge/authlib/` returns nothing.
- [ ] The four `cost_usd` / `source` / `daily_total_usd` / `daily_max_usd` JSON tags are unchanged, pinned by a test in `costevent` and another in `litellm_budgettrack`.
- [ ] Every commit carries `Signed-off-by` and `Assisted-By`, and none carries `Co-Authored-By`.

## Not in this phase

Phases 1-7 of the spec, which get their own plan: the `authlib/pricing` package, `Deps`/`BuildWithDeps` injection, the `toolprune` and `litellm_budgettrack` rate-config migrations, dropping `parseFrameUsage`, `UnpricedBy`, the `abctl` provenance and coverage rendering, and `/model/info` discovery.

Deliberately deferred detail: after this phase, a request is priced only when
`litellm-budget-track` is in the pipeline. Traffic without it stays unpriced and
is visible as the gap between `pricedRequests` and `requests` — which is the
honest report, and what phase 5's resolver fallback closes.
