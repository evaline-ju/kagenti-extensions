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
