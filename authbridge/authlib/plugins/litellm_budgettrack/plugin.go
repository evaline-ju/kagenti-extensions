// Package litellm_budgettrack provides a pipeline plugin that tracks
// per-request cost and enforces a daily spending budget, rejecting requests
// with HTTP 429 when the budget is exceeded.
//
// Cost is resolved in two ways:
//
//   - Non-streaming responses carry the cost in a response header
//     (x-litellm-response-cost, or the pre-discount -original variant), read
//     on the terminal frame.
//
//   - Streaming responses (text/event-stream — what Claude Code's /v1/messages
//     uses) report cost 0 in the header because the total is not known when the
//     headers are sent. That zero is a placeholder, not an answer. For these the
//     plugin prices the per-tier token counts inference-parser publishes, using the
//     top-level `pricing:` section.
//
//     The plugin no longer carries rate options of its own. input_cost_per_token
//     and its three siblings were REMOVED, and a config still setting them now
//     fails to start with the field named — see docs/litellm-budgettrack-plugin.md.
//     It also requires inference-parser LATER in the chain, because the response
//     pass runs in reverse; see Capabilities.
package litellm_budgettrack

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rossoctl/cortex/authbridge/authlib/costevent"
	"github.com/rossoctl/cortex/authbridge/authlib/pipeline"
	"github.com/rossoctl/cortex/authbridge/authlib/plugins"
	"github.com/rossoctl/cortex/authbridge/authlib/pricing"
)

// Response cost headers emitted by LiteLLM.
//
// MEASURED 2026-09-11 against ete-litellm (LiteLLM 1.85.5), because an earlier version of
// this comment had the semantics backwards and a reviewer reasonably concluded from it
// that drift detection would false-positive on every Anthropic-format request:
//
//	/v1/chat/completions  both headers present, IDENTICAL values
//	/v1/messages          only "-original", same value the other path reports
//
// For 16 input + 4 output tokens of claude-opus-5 both paths reported
// 0.00013680000000000002, which is 0.76 x vendor list (16x$5 + 4x$25 per Mtok = 0.00018).
// So "-original" is the cost the gateway ACTUALLY CHARGED, not a pre-discount list price,
// and falling back to it compares like with like.
//
// "Original" refers to LiteLLM's OWN discount/margin layer, reported alongside in
// X-Litellm-Response-Cost-{Discount,Margin}-Amount: original is the figure before that
// layer is applied. This gateway runs neither (both report 0.0), so the two agree. A
// gateway that DOES configure them would see them diverge, which is why checkDrift
// checks those headers before comparing — see driftComparable.
//
// The fallback itself is load-bearing: without it, budget tracking silently records $0
// for every Anthropic-format request, which is the shape Claude Code sends.
const (
	responseCostHeader         = "X-Litellm-Response-Cost"
	responseCostOriginalHeader = "X-Litellm-Response-Cost-Original"

	// LiteLLM's own adjustment layer, non-zero only where an operator configured it.
	costDiscountAmountHeader = "X-Litellm-Response-Cost-Discount-Amount"
	costMarginAmountHeader   = "X-Litellm-Response-Cost-Margin-Amount"
)

type budgetTrackConfig struct {
	SpendFile string  `json:"spend_file" required:"true" description:"Path to the JSON spend ledger file."`
	MaxBudget float64 `json:"max_budget" required:"true" description:"Daily budget in USD."`

	// There are deliberately no rate knobs here any more.
	//
	// Four of them (input / output / cache_write / cache_read per token) used to
	// price streamed responses, duplicating both the rates in tool-prune's config
	// and the token parser in inference-parser. Rates now come from the top-level
	// `pricing:` section via authlib/pricing, which also gives them endpoint
	// scoping — the same model bills differently per gateway, and a per-plugin
	// table had no way to say so.
}

// stateKey names the per-request scratch holding the settle guard.
const stateKey = "litellm-budget-track"

// settleState records that the terminal frame already priced this request.
//
// It no longer accumulates token counts. The plugin used to run its own SSE parser
// to gather them, duplicating inference-parser frame for frame; it now reads the
// counts that parser publishes, so all that is left to remember is exactly-once.
type settleState struct {
	settled bool
}

// The per-response cost event this plugin publishes lives in authlib/costevent:
// the usage aggregator and abctl both decode it, so the shape belongs where all
// three can share one declaration rather than in this package.

type spendLedger struct {
	Date       string  `json:"date"`
	TotalSpend float64 `json:"total_spend"`
	TotalCalls int     `json:"total_calls"`
}

// BudgetTrack enforces a daily spending budget based on x-litellm-response-cost.
type BudgetTrack struct {
	cfg    budgetTrackConfig
	mu     sync.Mutex
	ledger spendLedger

	// rates is the process rate table, injected before Configure. Read only
	// through costOf, which guards the nil interface.
	rates pricing.Resolver

	// drift reports when the table disagrees with the gateway's own figure.
	drift driftReporter
}

// SetPricingResolver implements pricing.ResolverConsumer.
func (p *BudgetTrack) SetPricingResolver(r pricing.Resolver) { p.rates = r }

// New creates an unconfigured BudgetTrack plugin instance.
func New() *BudgetTrack { return &BudgetTrack{} }

func init() {
	plugins.RegisterPlugin("litellm-budget-track", func() pipeline.Plugin { return New() })
}

func (p *BudgetTrack) Name() string { return "litellm-budget-track" }

func (p *BudgetTrack) Capabilities() pipeline.PluginCapabilities {
	return pipeline.PluginCapabilities{
		// ReadsBody: the plugin parses the response body (streamed usage). It
		// makes Pipeline.NeedsBody() true so the extproc (envoy-sidecar) listener
		// buffers the response body and takes its body-phase branch; without it
		// that listener dispatches a single header-only RunResponseFrame and the
		// streamed accounting silently records nothing (or double-charges if
		// Envoy is statically configured BUFFERED). The proxy listeners gate on
		// HasStreamingResponders() and are unaffected. Mirrors inference-parser.
		ReadsBody: true,
		// RequiresLater, NOT Requires — the direction is the whole point.
		//
		// This plugin prices the per-tier token counts inference-parser publishes
		// while folding response frames. The response passes walk the chain in
		// REVERSE (pipeline.RunResponseFrame), so the parser must sit at a HIGHER
		// index to fold each frame before this plugin settles the cost on the
		// terminal one. Declaring Requires would enforce the opposite order and
		// leave every streamed response unpriced, with nothing reporting that the
		// counts were missed.
		//
		// BREAKING: a pipeline listing litellm-budget-track without
		// inference-parser AFTER it now fails to build. Needs a release note.
		RequiresLater: []string{"inference-parser"},
		Description:   "Track LLM cost (response header or priced token usage) and enforce a daily budget.",
	}
}

func (p *BudgetTrack) Configure(raw json.RawMessage) error {
	// DisallowUnknownFields, matching tool-prune's Configure. Without it the four
	// rate knobs this plugin used to accept — input_cost_per_token and friends —
	// were silently dropped from an existing config: no error, no warning, no log
	// line, and the deployment switched to bundled vendor-list rates, which by this
	// change's own accounting OVERSTATES a discounted gateway. An operator would
	// see their cost figures move and have nothing pointing at the cause.
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p.cfg); err != nil {
		return fmt.Errorf("litellm-budget-track config: %w (rates moved to the top-level `pricing:` section; see docs/litellm-budgettrack-plugin.md)", err)
	}
	// Decode stops at the first JSON value, so trailing content was silently
	// accepted — a regression from json.Unmarshal, which rejected it. Switching to a
	// decoder to gain DisallowUnknownFields must not loosen this.
	if dec.More() {
		return fmt.Errorf("litellm-budget-track config: unexpected trailing content after the config object")
	}
	if p.cfg.SpendFile == "" {
		return fmt.Errorf("litellm-budget-track: spend_file is required")
	}
	if p.cfg.MaxBudget <= 0 {
		return fmt.Errorf("litellm-budget-track: max_budget must be > 0")
	}
	p.loadLedger()
	// Configure is also the hot-reload path, so drift's "already said that" state is
	// cleared here: the reload may well BE the operator acting on a drift warning.
	p.resetDrift()
	return nil
}

// OnRequest checks if the daily budget has been exceeded before allowing the request.
func (p *BudgetTrack) OnRequest(_ context.Context, pctx *pipeline.Context) pipeline.Action {
	p.mu.Lock()
	p.resetIfNewDay()
	spend := p.ledger.TotalSpend
	calls := p.ledger.TotalCalls
	p.mu.Unlock()

	if spend >= p.cfg.MaxBudget {
		// Record before returning — phase:"denied" events only surface when a
		// plugin recorded first, so skipping this hides the "why was I cut off?"
		// answer from the session API.
		pctx.Record(pipeline.Invocation{
			Action: pipeline.ActionDeny,
			Reason: "budget.exceeded",
			Details: map[string]string{
				// Key + precision mirror the 429 wire message and
				// costEvent.DailyTotalUSD — one ledger, one name.
				"daily_total_usd": strconv.FormatFloat(spend, 'f', 4, 64),
				"daily_max_usd":   strconv.FormatFloat(p.cfg.MaxBudget, 'f', 2, 64),
				"total_calls":     strconv.Itoa(calls),
			},
		})
		return pipeline.DenyStatus(429, "budget.exceeded",
			fmt.Sprintf("Cortex ExceededTokenBudget: daily spend $%.4f exceeds budget $%.2f. Reset at midnight UTC.", spend, p.cfg.MaxBudget))
	}
	return pipeline.Action{Type: pipeline.Continue}
}

// OnResponse handles the buffered path on listeners that do not route through
// OnResponseFrame. On the proxy listeners this plugin is a StreamingResponder,
// so pipeline.RunResponse skips it and OnResponseFrame drives accumulation
// instead; this remains for listeners that only call OnResponse.
func (p *BudgetTrack) OnResponse(_ context.Context, pctx *pipeline.Context) pipeline.Action {
	if cost, st := headerCost(pctx); st == headerPositive && cost > 0 {
		if total, ok := p.accumulate(cost); ok {
			p.emitCost(pctx, cost, costevent.SourceGatewayHeader, total, pricing.ProvAuthoritative)
		}
	}
	return pipeline.Action{Type: pipeline.Continue}
}

// OnResponseFrame observes each response frame. It parses token usage out of
// streamed SSE frames and, on the terminal frame, prices the request: the
// response-header cost when present (non-streaming), otherwise the parsed
// usage times the configured per-token rates (streaming).
func (p *BudgetTrack) OnResponseFrame(_ context.Context, pctx *pipeline.Context, frame []byte, last bool) pipeline.Action {
	if !last {
		return pipeline.Action{Type: pipeline.Continue}
	}

	// Terminal frame: settle the cost exactly once. Materialize the scratch
	// unconditionally (a header-only response never allocated it above) so the
	// guard also covers that path — a listener that dispatches last=true twice
	// (e.g. extproc header + buffered-body phases) must not double-charge.
	st := pipeline.GetState[settleState](pctx, stateKey)
	if st == nil {
		st = &settleState{}
		pipeline.SetState(pctx, stateKey, st)
	}
	if st.settled {
		return pipeline.Action{Type: pipeline.Continue}
	}
	st.settled = true

	cost, state := headerCost(pctx)
	source := costevent.SourceGatewayHeader
	// A header cost is a settled figure the gateway reported, not a rate we looked
	// up — the strongest provenance there is.
	provenance := pricing.ProvAuthoritative
	// A zero on a stream is LiteLLM's placeholder, not an answer, so it falls back
	// like an absent header. A zero on a non-streamed response IS an answer.
	streamedPlaceholder := state == headerZero && isEventStream(pctx)
	declaredFree := state == headerZero && !streamedPlaceholder
	if state != headerPositive {
		// Fall back to per-token pricing only when there is no authoritative
		// header cost: the header is absent, or this is a streamed response
		// (where LiteLLM always reports 0). A present "0" on a non-streamed
		// response is a genuine free call (cache hit / error) — charge nothing,
		// don't invent a cost from the usage block.
		if !declaredFree {
			// Price the per-tier counts inference-parser published, through the
			// process rate table scoped to this request's endpoint.
			//
			// The plugin used to run its own SSE parser to gather those counts,
			// folding the same frames inference-parser had already folded, and to
			// hold its own four rates. Both are gone: one parser, one rate table,
			// one place tokens become dollars.
			usage := pricing.UsageFromInference(pctx.Extensions.Inference)
			model := ""
			if pctx.Extensions.Inference != nil {
				model = pctx.Extensions.Inference.Model
			}
			micros, prov, priced := p.costOf(pctx.Host, model, usage)
			if priced {
				cost = float64(micros) / 1e6
				source = costevent.SourceUsageFallback
				provenance = prov
			}
		}
	}
	switch {
	case cost > 0:
		if source == costevent.SourceGatewayHeader {
			// Only a header cost is authoritative. Comparing a modelled figure against
			// itself would always agree and say nothing.
			p.checkDrift(pctx, cost)
		}
		if total, ok := p.accumulate(cost); ok {
			p.emitCost(pctx, cost, source, total, provenance)
		}
	case declaredFree:
		// The gateway reported a parsed, finite, exactly-zero cost on a NON-streamed
		// response: a genuine free call — a cache hit, or an error it declined to
		// charge for. Nothing is added to the ledger, but the event is published so
		// downstream knows this was PRICED at zero rather than unpriced. Without it
		// the aggregator finds no figure and invents a cost for a call the gateway
		// declared free.
		//
		// Deliberately NOT reached for an unusable header, nor for a stream whose
		// usage fallback failed to price. Those are unpriced, and claiming them as
		// settled zeros would count them toward coverage and drop them from the
		// unpriced list — the inverse of the bug this branch fixes.
		p.emitSettledZero(pctx)
	}
	return pipeline.Action{Type: pipeline.Continue}
}

// emitSettledZero publishes a zero cost the gateway actually reported, as distinct
// from the absence of any figure.
func (p *BudgetTrack) emitSettledZero(pctx *pipeline.Context) {
	if pctx.Extensions.Custom == nil {
		pctx.Extensions.Custom = map[string]any{}
	}
	p.mu.Lock()
	total := p.ledger.TotalSpend
	p.mu.Unlock()
	pctx.Extensions.Custom[p.Name()+pipeline.PluginEventSuffix] = costevent.Event{
		CostUSD:       0,
		Source:        costevent.SourceGatewayHeader,
		DailyTotalUSD: total,
		DailyMaxUSD:   p.cfg.MaxBudget,
		Provenance:    pricing.ProvAuthoritative.String(),
		Settled:       true,
	}
}

// accumulate adds one priced call to today's ledger and persists it,
// returning the post-add TotalSpend and whether the cost was recorded.
// A non-finite or non-positive cost is ignored (added=false): NaN/±Inf
// would poison TotalSpend (making the budget check meaningless) and
// break the JSON marshal, so this is the single chokepoint that
// guarantees the ledger only ever holds finite money.
func (p *BudgetTrack) accumulate(cost float64) (dailyTotal float64, added bool) {
	if cost <= 0 || math.IsNaN(cost) || math.IsInf(cost, 0) {
		return 0, false
	}
	p.mu.Lock()
	p.resetIfNewDay()
	p.ledger.TotalSpend += cost
	p.ledger.TotalCalls++
	total := p.ledger.TotalSpend
	p.saveLedger()
	p.mu.Unlock()
	return total, true
}

// emitCost writes the costEvent to pctx.Extensions.Custom; the listener
// forwards it to SessionEvent.Plugins under the plugin name.
func (p *BudgetTrack) emitCost(pctx *pipeline.Context, cost float64, source string, dailyTotal float64, prov pricing.Provenance) {
	if pctx.Extensions.Custom == nil {
		pctx.Extensions.Custom = map[string]any{}
	}
	pctx.Extensions.Custom[p.Name()+pipeline.PluginEventSuffix] = costevent.Event{
		CostUSD:       cost,
		Source:        source,
		DailyTotalUSD: dailyTotal,
		DailyMaxUSD:   p.cfg.MaxBudget,
		Provenance:    prov.String(),
		Settled:       true,
	}
}

// costOf prices usage through the injected rate table.
//
// The nil guard is on the INTERFACE, which is the trap: an un-injected plugin holds
// a nil interface and calling a method on it panics, where a nil *pricing.Registry
// would have been safe. tool-prune hit exactly this and its fail-open masked the
// panic as silently-disabled pruning.
func (p *BudgetTrack) costOf(host, model string, u pricing.Usage) (micros int64, prov pricing.Provenance, ok bool) {
	if p.rates == nil {
		return 0, pricing.ProvNone, false
	}
	rates, prov := p.rates.Resolve(host, model, u.PromptTotal())
	if prov == pricing.ProvNone {
		return 0, pricing.ProvNone, false
	}
	micros, ok = pricing.Cost(rates, u)
	if !ok {
		return 0, pricing.ProvNone, false
	}
	return micros, prov, true
}

// headerCostState says what the gateway's cost header actually told us. A bool
// could not carry this, and collapsing these cases caused a real defect: every
// state below except headerPositive returned (0, true), so "the gateway declared
// this call free" was indistinguishable from "the header was garbage" and from
// "this is a stream, where LiteLLM always stamps 0 as a placeholder". Publishing a
// settled zero for all of them counted unpriced traffic as priced.
type headerCostState int

const (
	// headerAbsent: no cost header at all. Price from token usage.
	headerAbsent headerCostState = iota
	// headerUnusable: a header was present but could not be believed — unparseable,
	// negative, NaN or Inf. NOT a declaration of anything, so it must not suppress
	// the usage fallback and must never publish a settled zero.
	headerUnusable
	// headerZero: present, parsed, finite and exactly zero. On a NON-streamed
	// response this is the gateway saying the call was free — a cache hit, or an
	// error it declined to charge for. On a streamed response it means nothing:
	// LiteLLM stamps 0 there by design because the total is unknown when headers
	// are sent.
	headerZero
	// headerPositive: a usable figure.
	headerPositive
)

// headerCost returns the cost the gateway reported and what kind of answer it was.
func headerCost(pctx *pipeline.Context) (cost float64, state headerCostState) {
	costStr := pctx.ResponseHeaders.Get(responseCostHeader)
	if costStr == "" {
		// Anthropic /v1/messages (and newer LiteLLM) omit the bare header.
		costStr = pctx.ResponseHeaders.Get(responseCostOriginalHeader)
	}
	if costStr == "" {
		return 0, headerAbsent
	}
	c, err := strconv.ParseFloat(costStr, 64)
	if err != nil || math.IsNaN(c) || math.IsInf(c, 0) || c < 0 {
		return 0, headerUnusable
	}
	if c == 0 {
		return 0, headerZero
	}
	return c, headerPositive
}

// isEventStream reports whether the response is a text/event-stream (SSE) — the
// streamed shape where LiteLLM reports cost 0 in the header, so usage-based
// pricing is the intended fallback.
func isEventStream(pctx *pipeline.Context) bool {
	ct := pctx.ResponseHeaders.Get("Content-Type")
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = ct[:i]
	}
	return strings.EqualFold(strings.TrimSpace(ct), "text/event-stream")
}

func (p *BudgetTrack) todayUTC() string {
	return time.Now().UTC().Format("2006-01-02")
}

func (p *BudgetTrack) resetIfNewDay() {
	today := p.todayUTC()
	if p.ledger.Date != today {
		p.ledger = spendLedger{Date: today}
	}
}

func (p *BudgetTrack) loadLedger() {
	data, err := os.ReadFile(p.cfg.SpendFile)
	if err != nil {
		p.ledger = spendLedger{Date: p.todayUTC()}
		return
	}
	var l spendLedger
	if json.Unmarshal(data, &l) != nil || l.Date != p.todayUTC() {
		p.ledger = spendLedger{Date: p.todayUTC()}
		return
	}
	p.ledger = l
}

func (p *BudgetTrack) saveLedger() {
	data, err := json.MarshalIndent(p.ledger, "", "  ")
	if err != nil {
		// Never overwrite a good ledger with a failed marshal (e.g. a
		// non-finite TotalSpend that slipped through). accumulate already
		// rejects non-finite costs; this is the belt-and-suspenders guard.
		return
	}
	_ = os.WriteFile(p.cfg.SpendFile, data, 0644)
}

var (
	_ pipeline.Plugin             = (*BudgetTrack)(nil)
	_ pipeline.Configurable       = (*BudgetTrack)(nil)
	_ pipeline.StreamingResponder = (*BudgetTrack)(nil)
)
