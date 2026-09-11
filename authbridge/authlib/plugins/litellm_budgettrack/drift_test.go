package litellm_budgettrack

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/rossoctl/cortex/authbridge/authlib/pipeline"
	"github.com/rossoctl/cortex/authbridge/authlib/pricing"
)

// A non-streamed response carries BOTH the gateway's own settled cost and the token
// counts, so the modelled figure can be checked against the real one for free. Nothing
// did that, which is how a gateway billing 0.76x vendor list was reported at list for
// months with no signal.
func TestDrift_WarnsWhenModelledDivergesFromAuthoritative(t *testing.T) {
	// Rates deliberately 2x what the gateway will report, so the modelled figure is
	// double the authoritative one.
	p := configurePriced(t, 100, map[pricing.Tier]float64{
		pricing.TierInput:  2e-6,
		pricing.TierOutput: 2e-6,
	})
	var buf bytes.Buffer
	p.SetDriftLogger(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))

	// 1000 in + 1000 out at 2e-6 = 0.004 modelled; gateway says 0.002.
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	h.Set(responseCostHeader, "0.002")
	pctx := &pipeline.Context{Host: "gw.internal", ResponseHeaders: h}
	pricedInference(pctx, 1000, 0, 0, 1000)
	p.OnResponseFrame(context.Background(), pctx, nil, true)

	got := buf.String()
	if got == "" {
		t.Fatal("no drift warning for a 2x divergence")
	}
	for _, want := range []string{"gw.internal", "claude-opus-5", "pricing"} {
		if !strings.Contains(got, want) {
			t.Errorf("warning omits %q: %s", want, got)
		}
	}
	// The ledger must still use the AUTHORITATIVE figure — drift is a diagnostic, not
	// a reason to distrust the gateway's own number.
	if p.ledger.TotalSpend != 0.002 {
		t.Errorf("TotalSpend = %v, want the gateway's 0.002", p.ledger.TotalSpend)
	}
}

func TestDrift_SilentWhenTheyAgree(t *testing.T) {
	p := configurePriced(t, 100, map[pricing.Tier]float64{
		pricing.TierInput:  1e-6,
		pricing.TierOutput: 1e-6,
	})
	var buf bytes.Buffer
	p.SetDriftLogger(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))

	h := http.Header{}
	h.Set("Content-Type", "application/json")
	h.Set(responseCostHeader, "0.002") // 1000+1000 at 1e-6 = exactly 0.002
	pctx := &pipeline.Context{Host: "gw.internal", ResponseHeaders: h}
	pricedInference(pctx, 1000, 0, 0, 1000)
	p.OnResponseFrame(context.Background(), pctx, nil, true)

	if got := buf.String(); got != "" {
		t.Errorf("warned on agreement: %s", got)
	}
}

// Warned once per (endpoint, model), not per request: an agent makes thousands of calls
// and a per-request warning would bury every other line in the log.
func TestDrift_WarnsOncePerEndpointModel(t *testing.T) {
	p := configurePriced(t, 100, map[pricing.Tier]float64{
		pricing.TierInput:  2e-6,
		pricing.TierOutput: 2e-6,
	})
	var buf bytes.Buffer
	p.SetDriftLogger(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))

	for i := 0; i < 5; i++ {
		h := http.Header{}
		h.Set("Content-Type", "application/json")
		h.Set(responseCostHeader, "0.002")
		pctx := &pipeline.Context{Host: "gw.internal", ResponseHeaders: h}
		pricedInference(pctx, 1000, 0, 0, 1000)
		p.OnResponseFrame(context.Background(), pctx, nil, true)
	}
	if n := strings.Count(buf.String(), "gw.internal"); n != 1 {
		t.Errorf("warned %d times, want exactly 1 per endpoint/model", n)
	}
}

// A streamed response has no authoritative figure (the header is 0 by design), so there
// is nothing to compare and no warning to give.
func TestDrift_SilentOnStreamedResponses(t *testing.T) {
	p := configurePriced(t, 100, map[pricing.Tier]float64{
		pricing.TierInput:  2e-6,
		pricing.TierOutput: 2e-6,
	})
	var buf bytes.Buffer
	p.SetDriftLogger(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))

	h := http.Header{}
	h.Set("Content-Type", "text/event-stream")
	h.Set(responseCostHeader, "0")
	pctx := &pipeline.Context{Host: "gw.internal", ResponseHeaders: h}
	pricedInference(pctx, 1000, 0, 0, 1000)
	p.OnResponseFrame(context.Background(), pctx, nil, true)

	if got := buf.String(); got != "" {
		t.Errorf("warned on a streamed response, which has no authoritative cost: %s", got)
	}
}

// The dedup key is (endpoint, model), and BOTH halves come off the request: Host is a
// client-supplied header and the model is a body field. An unbounded map keyed on those
// grows for as long as a caller varies them, in a long-lived sidecar, for a diagnostic.
func TestDrift_SeenMapIsBounded(t *testing.T) {
	p := configurePriced(t, 1e9, map[pricing.Tier]float64{
		pricing.TierInput:  2e-6,
		pricing.TierOutput: 2e-6,
	})
	var buf bytes.Buffer
	p.SetDriftLogger(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))

	for i := 0; i < maxDriftKeys*3; i++ {
		h := http.Header{}
		h.Set("Content-Type", "application/json")
		h.Set(responseCostHeader, "0.002")
		pctx := &pipeline.Context{Host: fmt.Sprintf("gw-%d.internal", i), ResponseHeaders: h}
		pricedInference(pctx, 1000, 0, 0, 1000)
		p.OnResponseFrame(context.Background(), pctx, nil, true)
	}

	p.drift.mu.Lock()
	n := len(p.drift.seen)
	p.drift.mu.Unlock()
	if n > maxDriftKeys {
		t.Errorf("seen holds %d keys, cap is %d", n, maxDriftKeys)
	}
	// And it must say it stopped, exactly once — silently going quiet would read as
	// "the drift was fixed".
	if c := strings.Count(buf.String(), "no longer reporting"); c != 1 {
		t.Errorf("cap notice appeared %d times, want exactly 1", c)
	}
}
