// Package skiphost matches an outbound destination Host against an
// operator-configured pattern list to decide whether the listener should
// short-circuit — bypassing the plugin pipeline and session recording —
// and forward the request as a transparent proxy.
//
// Matcher semantics intentionally mirror authlib/routing: gobwas/glob with
// `.` as the separator so "*.svc.cluster.local" matches a single label and
// "service-*" matches anything starting with "service-". Port is stripped
// before matching so operators write patterns against the hostname alone
// regardless of which port the upstream listens on.
//
// The package is a leaf — no dependencies inside authlib — so both
// listener implementations (extproc, forwardproxy) can import it without
// risking an import cycle, and tests can exercise the matcher in
// isolation from the listener machinery.
package skiphost

import (
	"fmt"
	"net"

	"github.com/gobwas/glob"
)

// Matcher answers "does this host match any configured skip pattern?".
// A nil Matcher matches nothing (zero value is safe to call).
type Matcher struct {
	patterns []compiled
}

type compiled struct {
	raw  string
	glob glob.Glob
}

// New compiles a skip-host matcher from raw glob patterns. Returns an
// error identifying the first invalid pattern so misconfigurations
// surface at startup rather than at first request. An empty input is
// valid and yields a Matcher that matches nothing.
func New(patterns []string) (*Matcher, error) {
	if len(patterns) == 0 {
		return &Matcher{}, nil
	}
	out := make([]compiled, 0, len(patterns))
	for _, p := range patterns {
		if p == "" {
			return nil, fmt.Errorf("skiphost: empty pattern in skip_hosts list")
		}
		g, err := glob.Compile(p, '.')
		if err != nil {
			return nil, fmt.Errorf("skiphost: invalid pattern %q: %w", p, err)
		}
		out = append(out, compiled{raw: p, glob: g})
	}
	return &Matcher{patterns: out}, nil
}

// Match reports whether host matches any configured pattern. Strips
// the port (everything from the first colon) before comparing so
// operators can write "otel-collector.kagenti-system.svc.cluster.local"
// without worrying which port the upstream listens on. Returns false
// for the nil Matcher and for the empty host.
func (m *Matcher) Match(host string) bool {
	if m == nil || len(m.patterns) == 0 || host == "" {
		return false
	}
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	for _, p := range m.patterns {
		if p.glob.Match(host) {
			return true
		}
	}
	return false
}
