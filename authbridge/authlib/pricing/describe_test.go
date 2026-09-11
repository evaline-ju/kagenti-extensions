package pricing

import (
	"encoding/json"
	"strings"
	"testing"
)

// Describe answers "what rates is this process actually using", which no config file
// can: the config shows what the operator wrote, not what the binary contributed.
func TestDescribe_ShowsBundledRowsAndMultipliers(t *testing.T) {
	reg := NewRegistry(mustBuild(t, nil))
	d := reg.Describe()

	if len(d.Rows) == 0 {
		t.Fatal("no rows described")
	}
	var sawOpus5 bool
	for _, r := range d.Rows {
		if r.Model == "claude-opus-5" {
			sawOpus5 = true
			if r.InputPerMillion != 5.00 {
				t.Errorf("opus-5 input = %v, want 5.00 (list, undscaled in the raw table)", r.InputPerMillion)
			}
			if r.Provenance != "bundled" {
				t.Errorf("opus-5 provenance = %q, want bundled", r.Provenance)
			}
		}
	}
	if !sawOpus5 {
		t.Error("claude-opus-5 missing from the described table")
	}

	// The shipped gateway discount has to be visible, or an operator cannot tell why
	// their figures differ from vendor list.
	var sawIBM bool
	for _, m := range d.Multipliers {
		if strings.Contains(m.Host, "res.ibm.com") {
			sawIBM = true
			if m.Factor != 0.76 || m.Provenance != "bundled" {
				t.Errorf("ibm multiplier = %+v, want 0.76 bundled", m)
			}
		}
	}
	if !sawIBM {
		t.Error("the shipped *.res.ibm.com multiplier is not described")
	}
}

// The question an operator actually asks is per-endpoint: what will THIS gateway charge?
// That needs resolution, including the multiplier, which a raw table dump cannot show.
func TestDescribe_EffectiveForHostAppliesTheMultiplier(t *testing.T) {
	reg := NewRegistry(mustBuild(t, nil))

	eff := reg.EffectiveFor("ete-litellm.ai-models.vpc-int.res.ibm.com")
	if eff.Multiplier != 0.76 {
		t.Errorf("Multiplier = %v, want 0.76", eff.Multiplier)
	}
	if eff.MultiplierFrom != "bundled" {
		t.Errorf("MultiplierFrom = %q, want bundled", eff.MultiplierFrom)
	}
	byModel := map[string]EffectiveRates{}
	for _, m := range eff.Models {
		byModel[m.Model] = m
	}
	// Measured on that gateway: 3.80 / 4.75 / 0.38 / 19.00.
	got, ok := byModel["claude-opus-5"]
	if !ok {
		t.Fatalf("claude-opus-5 absent; got %d models", len(eff.Models))
	}
	for name, pair := range map[string][2]float64{
		"input":       {got.InputPerMillion, 3.80},
		"cache-write": {got.CacheWritePerMillion, 4.75},
		"cache-read":  {got.CacheReadPerMillion, 0.38},
		"output":      {got.OutputPerMillion, 19.00},
	} {
		if pair[0] < pair[1]-1e-9 || pair[0] > pair[1]+1e-9 {
			t.Errorf("opus-5 %s = %v, want %v (list scaled by the gateway discount)", name, pair[0], pair[1])
		}
	}

	// The vendor endpoint is unscaled, which is the whole reason the rule is scoped.
	direct := reg.EffectiveFor("api.anthropic.com")
	if direct.Multiplier != 1 {
		t.Errorf("api.anthropic.com multiplier = %v, want 1", direct.Multiplier)
	}
}

func TestDescribe_JSONRoundTrips(t *testing.T) {
	// This is served over HTTP, so it has to survive encoding.
	reg := NewRegistry(mustBuild(t, nil))
	for name, v := range map[string]any{"table": reg.Describe(), "effective": reg.EffectiveFor("x.res.ibm.com")} {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(b) < 10 {
			t.Errorf("%s encoded to %q", name, b)
		}
	}
}

func TestDescribe_NilRegistryIsEmptyNotAPanic(t *testing.T) {
	var reg *Registry
	if d := reg.Describe(); len(d.Rows) != 0 || len(d.Multipliers) != 0 {
		t.Errorf("nil registry described %+v", d)
	}
	if e := reg.EffectiveFor("h"); e.Multiplier != 1 || len(e.Models) != 0 {
		t.Errorf("nil registry effective = %+v", e)
	}
}

func mustBuild(t *testing.T, c *Config) *Table {
	t.Helper()
	tab, err := Build(c)
	if err != nil {
		t.Fatal(err)
	}
	return tab
}
