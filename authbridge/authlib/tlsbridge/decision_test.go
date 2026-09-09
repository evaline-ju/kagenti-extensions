package tlsbridge

import (
	"testing"
	"time"
)

// mustDecision fails the test on an invalid pattern list, so each case reads as
// the single call it was before NewDecision grew an error return.
func mustDecision(t *testing.T, o DecisionOpts) *Decision {
	t.Helper()
	d, err := NewDecision(o)
	if err != nil {
		t.Fatalf("NewDecision(%+v): %v", o, err)
	}
	return d
}

func TestDecision_Classify(t *testing.T) {
	d := mustDecision(t, DecisionOpts{
		Ports:     map[int]bool{443: true, 8443: true},
		SkipHosts: []string{"pinned.example.com"},
	})
	tlsHello := []byte{0x16, 0x03, 0x01, 0x00, 0x05} // handshake, TLS1.0 record, len
	cases := []struct {
		name   string
		host   string
		port   int
		first  []byte
		expect Verdict
		reason string
	}{
		{"happy https", "api.example.com", 443, tlsHello, Terminate, ""},
		{"happy 8443", "api.example.com", 8443, tlsHello, Terminate, ""},
		{"non-tls first byte", "api.example.com", 443, []byte("GET / "), Passthrough, "non-tls"},
		{"unlisted port", "api.example.com", 9999, tlsHello, Passthrough, "port"},
		{"skip-listed host", "pinned.example.com", 443, tlsHello, Passthrough, "skip"},
		{"short record (<5 bytes)", "api.example.com", 443, []byte{0x16, 0x03}, Passthrough, "non-tls"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v, reason := d.Classify(tc.host, tc.port, tc.first)
			if v != tc.expect || reason != tc.reason {
				t.Errorf("got (%v,%q), want (%v,%q)", v, reason, tc.expect, tc.reason)
			}
		})
	}
}

func TestDecision_DefaultPortsWhenNil(t *testing.T) {
	d := mustDecision(t, DecisionOpts{}) // nil Ports -> {443,8443}
	tlsHello := []byte{0x16, 0x03, 0x01, 0x00, 0x05}
	if v, reason := d.Classify("api.example.com", 443, tlsHello); v != Terminate || reason != "" {
		t.Errorf("port 443: got (%v,%q), want (%v,%q)", v, reason, Terminate, "")
	}
	if v, reason := d.Classify("api.example.com", 80, tlsHello); v != Passthrough || reason != "port" {
		t.Errorf("port 80: got (%v,%q), want (%v,%q)", v, reason, Passthrough, "port")
	}
}

func TestSkipSet_AutoSkip(t *testing.T) {
	s := NewSkipSet()
	if s.Contains("h") {
		t.Fatal("empty set should not contain h")
	}
	s.Add("h")
	if !s.Contains("h") {
		t.Error("Add then Contains failed")
	}
}

func TestSkipSet_TTLExpires(t *testing.T) {
	s := NewSkipSet()
	s.ttl = 20 * time.Millisecond // same-package test can tighten the TTL
	s.Add("h")
	if !s.Contains("h") {
		t.Fatal("should contain immediately after Add")
	}
	time.Sleep(40 * time.Millisecond)
	if s.Contains("h") {
		t.Error("entry should have expired (self-healing re-attempt)")
	}
}

func TestSkipSet_Bounded(t *testing.T) {
	s := NewSkipSet()
	s.max = 2 // cap small; a flood of distinct SNIs must not grow it unbounded
	s.Add("a")
	s.Add("b")
	s.Add("c")
	if len(s.m) > 2 {
		t.Errorf("SkipSet grew past max: len=%d, want <=2", len(s.m))
	}
}

func TestDecision_HandlesPort(t *testing.T) {
	// Default set when Ports is nil.
	d := mustDecision(t, DecisionOpts{})
	if !d.HandlesPort(443) || !d.HandlesPort(8443) {
		t.Error("default set must handle 443 and 8443")
	}
	if d.HandlesPort(9443) {
		t.Error("default set must not handle 9443")
	}
	// Custom set replaces the default.
	c := mustDecision(t, DecisionOpts{Ports: map[int]bool{9443: true}})
	if !c.HandlesPort(9443) {
		t.Error("custom set must handle 9443")
	}
	if c.HandlesPort(443) {
		t.Error("custom set must not handle 443 (it replaces, not augments, the default)")
	}
}

// TestDefaultPassthrough_CoversTheToolsThatCannotBeConfigured is the point of the
// default list: `gh` has no CA option at all and Go on macOS honours no CA
// environment variable, so intercepting these hosts breaks them with no fix
// available to the user. Every host below appeared in a real proxy log as
// reason=handshake-fail.
func TestDefaultPassthrough_CoversTheToolsThatCannotBeConfigured(t *testing.T) {
	d := mustDecision(t, DecisionOpts{}) // nil SkipHosts -> defaults
	tlsHello := []byte{0x16, 0x03, 0x01, 0x00, 0x05}
	for _, host := range []string{
		"api.github.com", "github.com", "raw.githubusercontent.com",
		"codeload.github.com", "uploads.github.com", "cafe.github.com",
		"ghcr.io",
		"proxy.golang.org", "sum.golang.org", "google.golang.org", "golang.org",
		"go.googlesource.com", "go.opentelemetry.io", "go.yaml.in", "gopkg.in",
		"pypi.org", "files.pythonhosted.org", "registry.npmjs.org", "crates.io",
		// proxy.golang.org redirects module zips here; without it `go mod download`
		// still failed on one host after every other Go host was skipped.
		"storage.googleapis.com",
	} {
		if v, reason := d.Classify(host, 443, tlsHello); v != Passthrough || reason != "skip" {
			t.Errorf("%s: got (%v,%q), want (Passthrough,\"skip\")", host, v, reason)
		}
	}
	// Ports are still honoured ahead of the host check.
	if v, reason := d.Classify("api.github.com", 9999, tlsHello); reason != "port" {
		t.Errorf("port gate should win: got (%v,%q)", v, reason)
	}
	// The matcher strips the port, so host:port forms skip too.
	if v, _ := d.Classify("api.github.com:443", 443, tlsHello); v != Passthrough {
		t.Error("host:port form should still match the skip list")
	}
}

// TestDefaultPassthrough_NeverSkipsInferenceOrToolEndpoints is the guard that
// matters most. A default that quietly stopped bridging an LLM endpoint would
// remove the parsing and the tool-prune savings — the whole reason the bridge
// exists — with no error anywhere to notice it by.
//
// generativelanguage.googleapis.com is listed explicitly: it is why the Go module
// hosts are enumerated per-family instead of as a blanket *.googleapis.com.
func TestDefaultPassthrough_NeverSkipsInferenceOrToolEndpoints(t *testing.T) {
	d := mustDecision(t, DecisionOpts{})
	tlsHello := []byte{0x16, 0x03, 0x01, 0x00, 0x05}
	for _, host := range []string{
		"api.anthropic.com",
		"api.openai.com",
		"generativelanguage.googleapis.com",
		"us-central1-aiplatform.googleapis.com",
		"ete-litellm.ai-models.vpc-int.res.ibm.com",
		"github-tool-mcp",
		"github-tool-mcp.team1.svc.cluster.local",
		"weather-agent.team1.svc.cluster.local",
	} {
		if v, reason := d.Classify(host, 443, tlsHello); v != Terminate {
			t.Errorf("%s must stay bridged, got (%v,%q)", host, v, reason)
		}
	}
}

// TestDecisionOpts_ExplicitEmptyDisablesDefaults: nil means "unset, use the
// defaults" and an empty non-nil slice means "skip nothing". YAML distinguishes an
// absent key from `passthrough_hosts: []`, which is what lets an operator bridge
// everything without needing a second config field to turn defaults off.
func TestDecisionOpts_ExplicitEmptyDisablesDefaults(t *testing.T) {
	tlsHello := []byte{0x16, 0x03, 0x01, 0x00, 0x05}

	def := mustDecision(t, DecisionOpts{SkipHosts: nil})
	if v, _ := def.Classify("api.github.com", 443, tlsHello); v != Passthrough {
		t.Error("nil SkipHosts should apply the defaults")
	}
	none := mustDecision(t, DecisionOpts{SkipHosts: []string{}})
	if v, _ := none.Classify("api.github.com", 443, tlsHello); v != Terminate {
		t.Error("explicit empty SkipHosts should skip nothing, not fall back to defaults")
	}
	// An explicit list replaces the defaults rather than adding to them.
	own := mustDecision(t, DecisionOpts{SkipHosts: []string{"pinned.example.com"}})
	if v, _ := own.Classify("api.github.com", 443, tlsHello); v != Terminate {
		t.Error("an explicit list should replace the defaults")
	}
	if v, _ := own.Classify("pinned.example.com", 443, tlsHello); v != Passthrough {
		t.Error("an explicit list should still be honoured")
	}
}

// TestNewDecision_RejectsUnusablePatterns: a pattern that cannot match is a typo,
// and ignoring it presents as "the bridge broke my tool" with nothing connecting
// the symptom to the cause. Match-all is rejected for a different reason — it
// would disable interception wholesale while looking like a narrowing.
func TestNewDecision_RejectsUnusablePatterns(t *testing.T) {
	for _, bad := range [][]string{
		{"*"},                  // match-all
		{"**"},                 // match-all, other spelling
		{"api.github.com:443"}, // port-bearing: Match strips ports, so it could never fire
		{""},                   // empty
		{"github.com", "  "},   // one good, one blank
	} {
		if _, err := NewDecision(DecisionOpts{SkipHosts: bad}); err == nil {
			t.Errorf("NewDecision(%q) should have failed", bad)
		}
	}
	// The shipped defaults must themselves compile — a bad entry here would be
	// a fatal boot error for every user.
	if _, err := NewDecision(DecisionOpts{SkipHosts: DefaultPassthroughHosts}); err != nil {
		t.Fatalf("DefaultPassthroughHosts does not compile: %v", err)
	}
}
