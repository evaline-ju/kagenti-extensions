package litellm_budgettrack

import (
	"fmt"
	"log/slog"
	"sync"

	"github.com/rossoctl/cortex/authbridge/authlib/pipeline"
	"github.com/rossoctl/cortex/authbridge/authlib/pricing"
)

// driftTolerance is how far the modelled cost may sit from the gateway's own figure
// before it is worth saying so.
//
// 5% absorbs rounding and the micro quantization while still catching every mistake
// that matters: a forgotten gateway discount is 32% off, a missing multiplier or a
// decimal-place slip is a factor of ten or a hundred. Anything inside 5% is not
// actionable and would train an operator to ignore the line.
const driftTolerance = 0.05

// driftReporter warns, once per endpoint and model, when the rate table disagrees with
// what the gateway actually charged.
//
// This exists because the information was already on the wire and nobody looked. A
// non-streamed response carries BOTH the gateway's settled cost and the token counts,
// so the modelled figure can be checked against the real one for free — and for months
// a gateway billing 0.76x vendor list was reported at list, with the discrepancy
// visible in every such response and no signal anywhere.
//
// Once per (endpoint, model), not per request: an agent makes thousands of calls, and a
// per-request warning would bury every other line in the log and get filtered out.
type driftReporter struct {
	mu   sync.Mutex
	seen map[string]struct{}
	log  *slog.Logger
}

func (d *driftReporter) logger() *slog.Logger {
	if d.log != nil {
		return d.log
	}
	return slog.Default()
}

// SetDriftLogger overrides the logger used for drift warnings. For tests.
func (p *BudgetTrack) SetDriftLogger(l *slog.Logger) {
	p.drift.mu.Lock()
	p.drift.log = l
	p.drift.mu.Unlock()
}

// checkDrift compares an authoritative cost against what the rate table would have
// modelled, and warns on a material divergence.
//
// Called only where an authoritative figure exists — a non-streamed response with a
// usable cost header. A streamed response reports 0 there by design, so there is
// nothing to compare and silence is correct.
//
// The ledger keeps using the authoritative figure regardless: drift is a diagnostic
// about the rate TABLE, never a reason to distrust the gateway's own number.
func (p *BudgetTrack) checkDrift(pctx *pipeline.Context, authoritative float64) {
	if authoritative <= 0 || p.rates == nil {
		return
	}
	inf := pctx.Extensions.Inference
	if inf == nil || inf.Model == "" {
		return
	}
	u := pricing.UsageFromInference(inf)
	if u == (pricing.Usage{}) {
		return
	}
	micros, prov, ok := p.costOf(pctx.Host, inf.Model, u)
	if !ok {
		return // unpriced is already reported as a coverage gap; not drift
	}
	modelled := float64(micros) / 1e6
	ratio := modelled / authoritative
	if ratio > 1-driftTolerance && ratio < 1+driftTolerance {
		return
	}

	key := pctx.Host + "\x00" + inf.Model
	p.drift.mu.Lock()
	if p.drift.seen == nil {
		p.drift.seen = map[string]struct{}{}
	}
	if _, dup := p.drift.seen[key]; dup {
		p.drift.mu.Unlock()
		return
	}
	p.drift.seen[key] = struct{}{}
	log := p.drift.logger()
	p.drift.mu.Unlock()

	direction := "over"
	if ratio < 1 {
		direction = "under"
	}
	log.Warn("pricing: the rate table disagrees with what the gateway charged",
		"endpoint", pctx.Host,
		"model", inf.Model,
		"modelled_usd", fmt.Sprintf("%.6f", modelled),
		"gateway_usd", fmt.Sprintf("%.6f", authoritative),
		"ratio", fmt.Sprintf("%.3fx", ratio),
		"effect", direction+"stating every request this table prices, including streamed ones where no gateway figure exists",
		"rates_from", prov.String(),
		"fix", "set pricing.endpoints[].multiplier for this endpoint (a fraction of list), or per-model rates; `abctl pricing --host <endpoint>` shows what is in effect")
}
