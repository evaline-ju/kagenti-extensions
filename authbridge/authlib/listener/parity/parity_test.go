// Package parity runs the same fixture through both the extproc and
// HTTP-proxy listeners and asserts the resulting session event is
// identical, catching silent drift between the two deployment shapes.
package parity

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"testing"

	"github.com/rossoctl/cortex/authbridge/authlib/config"
	"github.com/rossoctl/cortex/authbridge/authlib/pipeline"
)

// listenerRun pairs a driver with a name so failure messages point at
// the listener that drifted.
type listenerRun struct {
	name string
	run  func(*testing.T, fixture, pipeline.SessionPhase) *observation
}

var inboundListeners = []listenerRun{
	{name: "extproc", run: runExtproc},
	{name: "reverseproxy", run: runReverseProxy},
}

var outboundListeners = []listenerRun{
	{name: "extproc", run: runExtproc},
	{name: "forwardproxy", run: runForwardProxy},
}

// TestParity_DenyOnRequest: pctx.Record before Reject must produce the
// same phase:"denied" event on both listeners.
func TestParity_DenyOnRequest(t *testing.T) {
	f := fixture{
		name:      "deny-on-request",
		direction: pipeline.Inbound,
		entries: []config.PluginEntry{spyEntry(spyPluginA, spyConfig{
			DenyOnRequest: true,
			DenyStatus:    429,
			DenyReason:    "spy.denied",
			DenyDetails:   map[string]string{"cutoff_reason": "quota"},
		})},
		method: "GET",
		path:   "/parity/deny",
	}
	assertParity(t, f, pipeline.SessionDenied, inboundListeners)
}

// TestParity_ResponseEventEmission: an OnResponse emit to Extensions.
// Custom must surface identically on SessionEvent.Plugins from both
// listeners.
func TestParity_ResponseEventEmission(t *testing.T) {
	f := fixture{
		name:      "priced-response-headers-only",
		direction: pipeline.Inbound,
		entries: []config.PluginEntry{spyEntry(spyPluginA, spyConfig{
			EmitOnResponse: true,
			ResponseEvent:  &spyEvent{Marker: "priced", Count: 3},
		})},
		method:         "GET",
		path:           "/parity/priced",
		upstreamStatus: 200,
		upstreamBody:   []byte(`{"reply":"ok"}`),
	}
	assertParity(t, f, pipeline.SessionResponse, inboundListeners)
}

// TestParity_OutboundDenyOnRequest: same deny-on-request contract on the
// outbound side — extproc and forwardproxy must record the same
// phase:"denied" event when a plugin rejects at OnRequest.
func TestParity_OutboundDenyOnRequest(t *testing.T) {
	f := fixture{
		name:      "outbound-deny-on-request",
		direction: pipeline.Outbound,
		entries: []config.PluginEntry{spyEntry(spyPluginA, spyConfig{
			DenyOnRequest: true,
			DenyStatus:    403,
			DenyReason:    "spy.blocked",
			DenyDetails:   map[string]string{"cutoff_reason": "egress-policy"},
		})},
		method: "GET",
		path:   "/parity/egress",
	}
	assertParity(t, f, pipeline.SessionDenied, outboundListeners)
}

// TestParity_ReadsBodyBufferedJSON: with ReadsBody set, both listeners
// present the plugin with the same request and response body bytes,
// despite extproc's two-phase handshake vs. the proxies' in-process
// buffering.
func TestParity_ReadsBodyBufferedJSON(t *testing.T) {
	f := fixture{
		name:      "reads-body-buffered-json",
		direction: pipeline.Inbound,
		entries: []config.PluginEntry{spyEntry(spyPluginAStreaming, spyConfig{
			ReadsBody:            true,
			RecordRequestBody:    true,
			RecordResponseFrames: true,
		})},
		method:         "POST",
		path:           "/parity/echo",
		reqBody:        []byte(`{"prompt":"hello"}`),
		upstreamStatus: 200,
		upstreamBody:   []byte(`{"reply":"ok"}`),
	}
	assertParity(t, f, pipeline.SessionResponse, inboundListeners)
}

// TestParity_ReadsBodySSE: an SSE upstream yields the same accumulated
// frame bytes and exactly one terminal frame on every listener. Frame
// counts differ (extproc buffered, proxies streamed) — that's fine and
// intentionally not asserted.
func TestParity_ReadsBodySSE(t *testing.T) {
	sse := []byte(
		"data: {\"type\":\"message_start\"}\n\n" +
			"data: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":7}}\n\n" +
			"data: {\"type\":\"message_stop\"}\n\n" +
			"data: [DONE]\n\n",
	)
	f := fixture{
		name:      "reads-body-sse",
		direction: pipeline.Inbound,
		entries: []config.PluginEntry{spyEntry(spyPluginAStreaming, spyConfig{
			ReadsBody:            true,
			RecordResponseFrames: true,
		})},
		method:              "POST",
		path:                "/parity/sse",
		upstreamStatus:      200,
		upstreamBody:        sse,
		upstreamContentType: "text/event-stream",
	}
	assertParity(t, f, pipeline.SessionResponse, inboundListeners)
}

// TestParity_InboundRequestBodyOverflow: a body exceeding both
// listeners' 1 MiB cap must be rejected before the pipeline runs, with
// the same wire status. Forwardproxy's 10 MiB cap is a documented
// divergence; outbound-overflow parity belongs to a follow-up.
func TestParity_InboundRequestBodyOverflow(t *testing.T) {
	big := make([]byte, (1<<20)+1) // 1 MiB + 1 byte — one over the cap
	for i := range big {
		big[i] = 'x'
	}
	f := fixture{
		name:      "inbound-request-body-overflow",
		direction: pipeline.Inbound,
		entries: []config.PluginEntry{spyEntry(spyPluginA, spyConfig{
			ReadsBody:         true,
			RecordRequestBody: true, // spy would record IF it got called; it must not
		})},
		method:  "POST",
		path:    "/parity/big",
		reqBody: big,
		// upstreamStatus deliberately 0: the listener must reject
		// before ever reaching an upstream.
	}
	assertParity(t, f, pipeline.SessionRequest, inboundListeners)
}

// TestParity_RequiresLaterOrderingRejected: each listener's
// construction must reject a RequiresLater violation (dependency at a
// LOWER index than the plugin naming it; contract requires HIGHER).
func TestParity_RequiresLaterOrderingRejected(t *testing.T) {
	entriesWrong := []config.PluginEntry{
		spyEntry(spyPluginB, spyConfig{}),
		spyEntry(spyPluginA, spyConfig{RequiresLater: []string{spyPluginB}}),
	}
	entriesOK := []config.PluginEntry{
		spyEntry(spyPluginA, spyConfig{RequiresLater: []string{spyPluginB}}),
		spyEntry(spyPluginB, spyConfig{}),
	}

	builders := []struct {
		name string
		fn   func([]config.PluginEntry) error
	}{
		{"extproc", tryBuildExtproc},
		{"reverseproxy", tryBuildReverseProxy},
		{"forwardproxy", tryBuildForwardProxy},
	}
	for _, b := range builders {
		t.Run(b.name, func(t *testing.T) {
			if err := b.fn(entriesWrong); err == nil {
				t.Errorf("%s accepted a wrong-order pipeline; RequiresLater is not enforced", b.name)
			}
			if err := b.fn(entriesOK); err != nil {
				t.Errorf("%s rejected a well-ordered pipeline: %v", b.name, err)
			}
		})
	}
}

// assertParity runs the fixture through every listener and fails on
// any observation mismatch. Partial presence (one listener records, the
// others don't) is itself drift and reported at the parent-test level.
func assertParity(t *testing.T, f fixture, wantPhase pipeline.SessionPhase, listeners []listenerRun) {
	t.Helper()

	// A single-listener call would pass vacuously with nothing to compare.
	if len(listeners) < 2 {
		t.Fatalf("assertParity: fixture %q was given %d listener(s); need at least 2", f.name, len(listeners))
	}

	type namedObs struct {
		listener string
		observed *observation
	}
	got := make([]namedObs, 0, len(listeners))
	for _, l := range listeners {
		t.Run(f.name+"/"+l.name, func(t *testing.T) {
			obs := l.run(t, f, wantPhase)
			if obs == nil {
				t.Fatalf("listener %q produced no matching event for fixture %q (phase=%v)", l.name, f.name, wantPhase)
			}
			got = append(got, namedObs{listener: l.name, observed: obs})
		})
	}

	// Partial-presence drift: some listeners produced an event, others
	// didn't. Report at the parent level so a failing subtest doesn't
	// mask the parity gap.
	if len(got) > 0 && len(got) < len(listeners) {
		present := make([]string, 0, len(got))
		for _, g := range got {
			present = append(present, g.listener)
		}
		t.Errorf("parity presence drift on fixture %q: only these listeners produced an event: %v", f.name, present)
	}
	if len(got) < 2 {
		return // one or more legs failed in the subtest; presence drift already reported.
	}

	// Pairwise compare against the first listener. All observations must
	// agree; a diff names both sides so operators see which drifted.
	base := got[0]
	for _, other := range got[1:] {
		if diff := observationDiff(base.observed, other.observed); diff != "" {
			t.Errorf("parity drift on fixture %q between %s and %s:\n%s",
				f.name, base.listener, other.listener, diff)
		}
	}
}

// observationDiff returns the first field-level disagreement, or ""
// when both agree on every parity-comparable field.
func observationDiff(a, b *observation) string {
	if a.PipelineRan != b.PipelineRan {
		return fmt.Sprintf("PipelineRan: %v vs %v", a.PipelineRan, b.PipelineRan)
	}
	// WireStatus is meaningful only when the pipeline refused pre-run;
	// on the success path extproc has no HTTP transport to report from.
	// Session-event fields cover parity for the run-and-recorded case.
	if !a.PipelineRan {
		if a.WireStatus != b.WireStatus {
			return fmt.Sprintf("WireStatus: %d vs %d", a.WireStatus, b.WireStatus)
		}
		return ""
	}
	if a.Phase != b.Phase {
		return "Phase: " + a.Phase + " vs " + b.Phase
	}
	if a.StatusCode != b.StatusCode {
		return fmt.Sprintf("StatusCode: %d vs %d", a.StatusCode, b.StatusCode)
	}
	if !reflect.DeepEqual(a.Error, b.Error) {
		return "Error: " + jsonPretty(a.Error) + " vs " + jsonPretty(b.Error)
	}
	if !invocationsEqual(a.Invocations, b.Invocations) {
		return "Invocations differ:\n  a=" + jsonPretty(a.Invocations) + "\n  b=" + jsonPretty(b.Invocations)
	}
	if !reflect.DeepEqual(a.PluginKeys, b.PluginKeys) {
		return "PluginKeys: " + jsonPretty(a.PluginKeys) + " vs " + jsonPretty(b.PluginKeys)
	}
	// Compare per-plugin JSON payloads structurally to tolerate
	// whitespace and map-order differences between listeners.
	keys := make([]string, 0, len(a.PluginEventJSON))
	for k := range a.PluginEventJSON {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if !jsonEqual(a.PluginEventJSON[k], b.PluginEventJSON[k]) {
			return "PluginEventJSON[" + k + "]: " + a.PluginEventJSON[k] + " vs " + b.PluginEventJSON[k]
		}
	}
	return ""
}

// invocationsEqual compares two slices as sets so multi-plugin
// fixtures tolerate independent-gate ordering.
func invocationsEqual(a, b []invocationSummary) bool {
	if len(a) != len(b) {
		return false
	}
	byKey := func(s []invocationSummary) {
		sort.Slice(s, func(i, j int) bool {
			if s[i].Plugin != s[j].Plugin {
				return s[i].Plugin < s[j].Plugin
			}
			if s[i].Action != s[j].Action {
				return s[i].Action < s[j].Action
			}
			return s[i].Reason < s[j].Reason
		})
	}
	as := append([]invocationSummary(nil), a...)
	bs := append([]invocationSummary(nil), b...)
	byKey(as)
	byKey(bs)
	return reflect.DeepEqual(as, bs)
}

func jsonPretty(v any) string {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return "<unmarshalable>"
	}
	return string(b)
}

// jsonEqual compares two raw JSON strings structurally so map-order
// and whitespace differences between listeners register as equal.
func jsonEqual(a, b string) bool {
	var av, bv any
	if err := json.Unmarshal([]byte(a), &av); err != nil {
		return false
	}
	if err := json.Unmarshal([]byte(b), &bv); err != nil {
		return false
	}
	return reflect.DeepEqual(av, bv)
}
