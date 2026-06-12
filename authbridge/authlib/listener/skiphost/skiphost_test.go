package skiphost

import "testing"

func TestNew_EmptyList(t *testing.T) {
	m, err := New(nil)
	if err != nil {
		t.Fatalf("New(nil) err = %v", err)
	}
	if m.Match("any-host") {
		t.Error("empty matcher matched a host; should match nothing")
	}
}

func TestNew_RejectsEmptyPattern(t *testing.T) {
	if _, err := New([]string{""}); err == nil {
		t.Error("New([\"\"]) returned nil error; empty pattern must be rejected at boot")
	}
}

func TestNew_RejectsInvalidPattern(t *testing.T) {
	if _, err := New([]string{"["}); err == nil {
		t.Error("New([\"[\"]) returned nil error; malformed glob must surface at boot")
	}
}

func TestMatch_NilMatcher(t *testing.T) {
	var m *Matcher
	if m.Match("host") {
		t.Error("nil matcher matched; zero value must be safe and match nothing")
	}
}

func TestMatch_EmptyHost(t *testing.T) {
	m, _ := New([]string{"*"})
	if m.Match("") {
		t.Error("matcher matched empty host; empty host must never match (defensive against unset Host header)")
	}
}

func TestMatch_StripsPort(t *testing.T) {
	m, _ := New([]string{"otel-collector.kagenti-system.svc.cluster.local"})
	if !m.Match("otel-collector.kagenti-system.svc.cluster.local:8335") {
		t.Error("port-stripping failed: pattern without :port should match host with :port")
	}
}

func TestMatch_GlobSingleLabel(t *testing.T) {
	// `*` with `.` separator matches a single DNS label, not multi-label.
	m, _ := New([]string{"otel-collector*"})
	cases := []struct {
		host string
		want bool
	}{
		{"otel-collector", true},
		{"otel-collector-v2", true},
		{"otel-collector.kagenti-system.svc.cluster.local", false}, // separator stops at .
		{"foo-otel-collector", false},
	}
	for _, tc := range cases {
		if got := m.Match(tc.host); got != tc.want {
			t.Errorf("Match(%q) = %v, want %v", tc.host, got, tc.want)
		}
	}
}

func TestMatch_GlobLeadingWildcard(t *testing.T) {
	// `*.svc.cluster.local` → single-label prefix on a fixed suffix.
	m, _ := New([]string{"*.kagenti-system.svc.cluster.local"})
	if !m.Match("otel-collector.kagenti-system.svc.cluster.local") {
		t.Error("leading-* should match a single-label prefix on the FQDN")
	}
	if m.Match("a.b.kagenti-system.svc.cluster.local") {
		t.Error("leading-* must NOT match a two-label prefix (separator semantics)")
	}
}

func TestMatch_MultiplePatterns_FirstMatchWins(t *testing.T) {
	m, _ := New([]string{"never-matches", "otel-*", "another"})
	if !m.Match("otel-collector") {
		t.Error("matcher with multiple patterns should match if any pattern matches")
	}
}

func TestMatch_NoMatch(t *testing.T) {
	m, _ := New([]string{"otel-collector*", "*.metrics.local"})
	if m.Match("github-tool-mcp") {
		t.Error("Match returned true for an unrelated host")
	}
}
