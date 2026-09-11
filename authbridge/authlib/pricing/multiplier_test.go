package pricing

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The IBM LiteLLM gateways bill a uniform fraction of vendor list — measured at
// exactly 0.7600 across three models and all four tiers. That is a scalar, not a rate
// card, so it is expressed as a multiplier: one number instead of twelve, and it tracks
// upstream repricing automatically because the gateway's price is DERIVED from list.
func TestMultiplier_BundledDefaultCoversTheIBMGateways(t *testing.T) {
	tab, err := Build(nil) // no operator config whatsoever
	if err != nil {
		t.Fatal(err)
	}
	inputPerM := func(host, model string) (float64, Provenance) {
		r, p := tab.Resolve(host, model, 0)
		v, _ := r.For(TierInput)
		return v * tokensPerMillion, p
	}

	// The real gateway, with and without a port.
	for _, host := range []string{
		"ete-litellm.ai-models.vpc-int.res.ibm.com",
		"ete-litellm.ai-models.vpc-int.res.ibm.com:443",
	} {
		got, prov := inputPerM(host, "claude-opus-5")
		if want := 5.00 * 0.76; got < want-1e-9 || got > want+1e-9 {
			t.Errorf("%s opus-5 input = %.4f/Mtok, want %.4f (list x 0.76)", host, got, want)
		}
		if prov != ProvBundled {
			t.Errorf("%s provenance = %s, want bundled (rate and multiplier both shipped)", host, prov)
		}
	}

	// Direct to the vendor must stay at LIST. A global default would understate this
	// by 24% — silently, which is the failure mode the whole package exists to avoid.
	if got, _ := inputPerM("api.anthropic.com", "claude-opus-5"); got != 5.00 {
		t.Errorf("api.anthropic.com opus-5 input = %.4f/Mtok, want 5.00 (list, unscaled)", got)
	}
	// And someone else's gateway is not covered by the IBM rule.
	if got, _ := inputPerM("litellm.example.com", "claude-opus-5"); got != 5.00 {
		t.Errorf("third-party gateway = %.4f/Mtok, want 5.00 (unscaled)", got)
	}
}

// Every tier scales, not just input — the discount was measured as uniform.
func TestMultiplier_AppliesToEveryTier(t *testing.T) {
	tab, err := Build(nil)
	if err != nil {
		t.Fatal(err)
	}
	const host = "ete-litellm.ai-models.vpc-int.res.ibm.com"
	// Measured on the gateway: opus-5 3.80 / 4.75 / 0.38 / 19.00.
	for _, tc := range []struct {
		tier Tier
		want float64
	}{
		{TierInput, 3.80}, {TierCacheWrite, 4.75}, {TierCacheRead, 0.38}, {TierOutput, 19.00},
	} {
		r, _ := tab.Resolve(host, "claude-opus-5", 0)
		v, ok := r.For(tc.tier)
		if !ok {
			t.Errorf("tier %d unpriced", tc.tier)
			continue
		}
		if got := v * tokensPerMillion; got < tc.want-1e-9 || got > tc.want+1e-9 {
			t.Errorf("tier %d = %.4f/Mtok, want %.4f (measured on the gateway)", tc.tier, got, tc.want)
		}
	}
	// Haiku included, cache tiers too, per the decision to apply it uniformly.
	r, _ := tab.Resolve(host, "claude-haiku-4-5", 0)
	if v, ok := r.For(TierCacheRead); !ok || v*tokensPerMillion < 0.076-1e-9 || v*tokensPerMillion > 0.076+1e-9 {
		t.Errorf("haiku cache-read = %.5f/Mtok (ok=%v), want 0.076", v*tokensPerMillion, ok)
	}
}

// Long-context thresholds must scale as well, or a discounted gateway would be
// correct below 200k and wrong above it.
func TestMultiplier_ScalesContextThresholds(t *testing.T) {
	var r Rates
	r.Base[TierInput], r.Set[TierInput] = 3.00/tokensPerMillion, true
	r.Thresholds = []ContextThreshold{{
		AbovePromptTokens: 200_000,
		Rate:              [numTiers]float64{TierInput: 6.00 / tokensPerMillion},
		Set:               [numTiers]bool{TierInput: true},
	}}
	half := 0.5
	tab, err := NewTable([]Entry{{Host: "gw", Model: "*", Rates: r, Prov: ProvConfigured}},
		MultiplierRule{Host: "gw", Factor: half, Prov: ProvConfigured})
	if err != nil {
		t.Fatal(err)
	}
	below, _ := tab.Resolve("gw", "m", 100_000)
	above, _ := tab.Resolve("gw", "m", 300_000)
	if v, _ := below.For(TierInput); v*tokensPerMillion != 1.50 {
		t.Errorf("below threshold = %.2f/Mtok, want 1.50", v*tokensPerMillion)
	}
	if v, _ := above.For(TierInput); v*tokensPerMillion != 3.00 {
		t.Errorf("above threshold = %.2f/Mtok, want 3.00 (the premium, halved)", v*tokensPerMillion)
	}
}

func TestMultiplier_ConfiguredOverridesBundled(t *testing.T) {
	var c Config
	src := `
endpoints:
  - hosts: ["ete-litellm.ai-models.vpc-int.res.ibm.com"]
    multiplier: 0.50
`
	if err := yaml.Unmarshal([]byte(src), &c); err != nil {
		t.Fatal(err)
	}
	tab, err := Build(&c)
	if err != nil {
		t.Fatal(err)
	}
	// An exact host beats the bundled *.res.ibm.com glob, and a configured multiplier
	// outranks a bundled one — an operator whose deal changed says so once.
	r, p := tab.Resolve("ete-litellm.ai-models.vpc-int.res.ibm.com", "claude-opus-5", 0)
	v, _ := r.For(TierInput)
	if got := v * tokensPerMillion; got != 2.50 {
		t.Errorf("input = %.2f/Mtok, want 2.50 (list x 0.50)", got)
	}
	if p != ProvConfigured {
		t.Errorf("provenance = %s, want configured — the operator supplied the multiplier", p)
	}
}

func TestMultiplier_Validation(t *testing.T) {
	for name, src := range map[string]string{
		// 76 instead of 0.76 inflates every figure 100x and looks plausible in YAML.
		"typo: whole number":         "endpoints:\n  - hosts: [gw]\n    multiplier: 76\n",
		"zero would free everything": "endpoints:\n  - hosts: [gw]\n    multiplier: 0\n",
		"negative":                   "endpoints:\n  - hosts: [gw]\n    multiplier: -1\n",
	} {
		t.Run(name, func(t *testing.T) {
			var c Config
			if err := yaml.Unmarshal([]byte(src), &c); err != nil {
				t.Fatal(err)
			}
			_, err := Build(&c)
			if err == nil {
				t.Fatalf("accepted %s", name)
			}
			if !strings.Contains(err.Error(), "multiplier") {
				t.Errorf("error %q does not name the field", err)
			}
		})
	}
	// 1.0 is legal and means "no discount", which is worth being able to state.
	var c Config
	if err := yaml.Unmarshal([]byte("endpoints:\n  - hosts: [gw]\n    multiplier: 1.0\n"), &c); err != nil {
		t.Fatal(err)
	}
	if _, err := Build(&c); err != nil {
		t.Errorf("rejected multiplier 1.0: %v", err)
	}
}

// A multiplier-only endpoint needs no models block: it scales what already resolves.
func TestMultiplier_EndpointNeedsNoModels(t *testing.T) {
	var c Config
	if err := yaml.Unmarshal([]byte("endpoints:\n  - hosts: [gw]\n    multiplier: 0.5\n"), &c); err != nil {
		t.Fatal(err)
	}
	if _, err := Build(&c); err != nil {
		t.Fatalf("rejected a multiplier-only endpoint: %v", err)
	}
}

// Two behaviours the docs now promise, both surprising enough that they must be pinned:
// a configured catch-all outranks a shipped host-specific rule (provenance decides before
// specificity), and an explicit 1.0 is how an operator drops a shipped discount.
func TestMultiplier_ConfiguredCatchAllOutranksShippedSpecific(t *testing.T) {
	one := 1.0
	nine := 0.9
	shipped := "ete-litellm.ai-models.vpc-int.res.ibm.com"

	// Control: shipped rule alone gives 0.76.
	base := mustBuild(t, nil)
	if f, prov := base.multiplierFor(shipped); f != 0.76 || prov != ProvBundled {
		t.Fatalf("shipped rule = %v/%v, want 0.76/bundled", f, prov)
	}

	t.Run("catch-all replaces it", func(t *testing.T) {
		tab := mustBuild(t, &Config{Endpoints: []EndpointConfig{{Hosts: []string{"*"}, Multiplier: &nine}}})
		f, prov := tab.multiplierFor(shipped)
		if f != 0.9 || prov != ProvConfigured {
			t.Errorf("multiplierFor(%s) = %v/%v, want 0.9/configured — a configured catch-all must outrank the shipped specific rule", shipped, f, prov)
		}
	})

	t.Run("explicit 1.0 drops the shipped discount", func(t *testing.T) {
		tab := mustBuild(t, &Config{Endpoints: []EndpointConfig{{Hosts: []string{shipped}, Multiplier: &one}}})
		if f, _ := tab.multiplierFor(shipped); f != 1 {
			t.Errorf("multiplierFor = %v, want 1 — an explicit 1.0 must cancel the shipped factor", f)
		}
		// And the rates must come back at list, not scaled.
		rates, prov := tab.Resolve(shipped, "claude-opus-5", 0)
		if got := perM(rates, TierInput); got != 5 {
			t.Errorf("input = %v/Mtok, want the unscaled 5 (prov %v)", got, prov)
		}
	})
}
