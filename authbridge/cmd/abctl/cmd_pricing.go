package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// runPricing renders the rates the running proxy is actually using.
//
// This exists because no config file can answer the question. `pricing:` shows what the
// operator wrote; the figures a request is charged come from that PLUS a rate table
// compiled into the binary PLUS any shipped gateway discount. Storing the table in the
// config instead would freeze every install at the rates current on its install date,
// silently, because that file is written once and never refreshed — which is the exact
// staleness the pricing work was done to remove.
//
// So: not stored, inspectable. And with --host it applies host scoping, specificity,
// provenance precedence and the multiplier together, which is not something a reader
// can carry out by eye over a rate table.
func runPricing(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("abctl pricing", flag.ContinueOnError)
	fs.SetOutput(stderr)
	host := fs.String("host", "", "show the rates this endpoint is actually charged, discount applied")
	asJSON := fs.Bool("json", false, "emit the raw JSON from the proxy")
	statsURL := fs.String("stats-url", defaultCortexStatsURL, "base URL of the proxy's stat server")
	fs.Usage = func() {
		fmt.Fprint(stderr, `abctl pricing — show the model rates Cortex is using

Usage:
  abctl pricing                     every row in the table, unscaled
  abctl pricing --host <gateway>    what that endpoint is charged, discount applied
  abctl pricing --json              raw JSON

The rates come from a table built into the binary (vendor list, refreshed per release),
your `+"`pricing:`"+` config, and any gateway discount Cortex ships. --host is the useful
form: it resolves all three the way a request would.

Flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}

	u := strings.TrimSuffix(*statsURL, "/") + "/pricing/table"
	if *host != "" {
		// Escaped, not concatenated: a host is operator-supplied and a "&" or "#" in
		// it would silently truncate the parameter, so the proxy would answer for the
		// wrong endpoint — a wrong answer presented as a right one.
		u += "?" + url.Values{"host": {*host}}.Encode()
	}
	body, err := fetchPricing(u)
	if err != nil {
		fmt.Fprintf(stderr, "abctl pricing: %v\n", err)
		fmt.Fprintf(stderr, "  is Cortex running? `abctl service status`\n")
		return 1
	}
	if *asJSON {
		fmt.Fprintln(stdout, string(body))
		return 0
	}
	if *host != "" {
		return renderEffective(body, stdout, stderr)
	}
	return renderTable(body, stdout, stderr)
}

func fetchPricing(url string) ([]byte, error) {
	c := &http.Client{Timeout: 10 * time.Second}
	resp, err := c.Get(url)
	if err != nil {
		return nil, fmt.Errorf("cannot reach the stat server at %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("%s returned 404 — this proxy predates the pricing endpoint", url)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s returned HTTP %d", url, resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 8<<20))
}

// effective mirrors pricing.Effective. Declared here rather than imported so abctl
// keeps decoding a wire shape rather than linking the pricing package's internals; the
// field names are what the JSON contract pins.
type effective struct {
	Host           string  `json:"host"`
	Multiplier     float64 `json:"multiplier"`
	MultiplierFrom string  `json:"multiplierFrom"`
	Models         []struct {
		Model      string  `json:"model"`
		Provenance string  `json:"provenance"`
		Unpriced   bool    `json:"unpriced"`
		LongCtx    int     `json:"longContextAbove"`
		In         float64 `json:"inputPerMillion"`
		CW         float64 `json:"cacheWritePerMillion"`
		CR         float64 `json:"cacheReadPerMillion"`
		Out        float64 `json:"outputPerMillion"`
	} `json:"models"`
}

func renderEffective(body []byte, stdout, stderr io.Writer) int {
	var e effective
	if err := json.Unmarshal(body, &e); err != nil {
		fmt.Fprintf(stderr, "abctl pricing: unreadable response: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "Rates in effect for %s\n\n", e.Host)
	fmt.Fprintf(stdout, "  %-30s %9s %9s %9s %9s  %s\n", "model", "input", "cache-wr", "cache-rd", "output", "provenance")
	fmt.Fprintf(stdout, "  %-30s %9s %9s %9s %9s\n", "", "$/Mtok", "$/Mtok", "$/Mtok", "$/Mtok")
	var breakpoints []int
	for _, m := range e.Models {
		if m.Unpriced {
			fmt.Fprintf(stdout, "  %-30s %9s %9s %9s %9s  %s\n", m.Model, "-", "-", "-", "-", "UNPRICED")
			continue
		}
		name := m.Model
		// Marked, because the figures on this row are the BELOW-threshold ones. An
		// unmarked row would quote a long session a rate it is not charged, which is
		// the failure the pricing work exists to remove — it must not come back in
		// the tool built to inspect it.
		if m.LongCtx > 0 {
			name += " *"
			breakpoints = append(breakpoints, m.LongCtx)
		}
		fmt.Fprintf(stdout, "  %-30s %9s %9s %9s %9s  %s\n", name,
			rate(m.In), rate(m.CW), rate(m.CR), rate(m.Out), m.Provenance)
	}
	if e.Multiplier != 1 {
		fmt.Fprintf(stdout, "\n  multiplier %.4g applied, from the %s rules — rates above are vendor list scaled by it\n",
			e.Multiplier, e.MultiplierFrom)
	} else {
		fmt.Fprintf(stdout, "\n  no gateway discount applies to this endpoint; rates above are vendor list\n")
	}
	if len(breakpoints) > 0 {
		fmt.Fprintf(stdout, "  * long-context rates apply above %s prompt tokens — the figures above are the\n"+
			"    below-threshold ones, so a longer request costs MORE than shown\n", breakpointList(breakpoints))
	}
	return 0
}

// rate formats a per-million figure, or "-" for a tier with no rate. A tier with no
// rate is not free: pricing.Cost refuses to price a request that used one, so showing
// 0.00 would misrepresent a coverage gap as a price.
func rate(v float64) string {
	if v == 0 {
		return "-"
	}
	return fmt.Sprintf("%.4g", v)
}

// thresholdRow is one long-context override on a raw row.
//
// Rendered rather than dropped: the raw view already warns that its rates are unscaled,
// and staying silent about a second tier would leave the same gap in the other direction
// — a figure presented as the whole answer when a longer request is charged more.
type thresholdRow struct {
	Above int     `json:"abovePromptTokens"`
	In    float64 `json:"inputPerMillion"`
	CW    float64 `json:"cacheWritePerMillion"`
	CR    float64 `json:"cacheReadPerMillion"`
	Out   float64 `json:"outputPerMillion"`
}

type describeBody struct {
	UpstreamCommit string `json:"upstreamCommit"`
	Rows           []struct {
		Host       string         `json:"host"`
		Model      string         `json:"model"`
		Provenance string         `json:"provenance"`
		In         float64        `json:"inputPerMillion"`
		CW         float64        `json:"cacheWritePerMillion"`
		CR         float64        `json:"cacheReadPerMillion"`
		Out        float64        `json:"outputPerMillion"`
		Thresholds []thresholdRow `json:"thresholds"`
	} `json:"rows"`
	Multipliers []struct {
		Host       string  `json:"host"`
		Factor     float64 `json:"factor"`
		Provenance string  `json:"provenance"`
	} `json:"multipliers"`
}

func renderTable(body []byte, stdout, stderr io.Writer) int {
	var d describeBody
	if err := json.Unmarshal(body, &d); err != nil {
		fmt.Fprintf(stderr, "abctl pricing: unreadable response: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "Pricing table (%d rows)\n", len(d.Rows))
	if d.UpstreamCommit != "" {
		fmt.Fprintf(stdout, "  bundled rates generated from litellm %s\n", short(d.UpstreamCommit))
	}
	fmt.Fprintf(stdout, "\n  %-22s %-30s %9s %9s %9s %9s  %s\n",
		"endpoint", "model", "input", "cache-wr", "cache-rd", "output", "from")
	for _, r := range d.Rows {
		h := r.Host
		if h == "" || h == "*" {
			h = "(any)"
		}
		fmt.Fprintf(stdout, "  %-22s %-30s %9s %9s %9s %9s  %s\n", h, r.Model,
			rate(r.In), rate(r.CW), rate(r.CR), rate(r.Out), r.Provenance)
		// Overrides on a continuation line, so the row above is not silently the
		// below-threshold half of a two-tier answer. A tier the override leaves unset
		// renders "-", meaning "inherits the row above", which is what At() does.
		for _, th := range r.Thresholds {
			fmt.Fprintf(stdout, "  %-22s   %-28s %9s %9s %9s %9s\n", "",
				"above "+commas(th.Above)+" tok:",
				rate(th.In), rate(th.CW), rate(th.CR), rate(th.Out))
		}
	}
	if len(d.Multipliers) > 0 {
		fmt.Fprintf(stdout, "\nGateway discounts\n\n  %-30s %8s  %s\n", "endpoint", "factor", "from")
		for _, m := range d.Multipliers {
			h := m.Host
			if h == "" || h == "*" {
				h = "(any)"
			}
			fmt.Fprintf(stdout, "  %-30s %8.4g  %s\n", h, m.Factor, m.Provenance)
		}
		fmt.Fprintf(stdout, "\n  Rates above are UNSCALED. Use --host <endpoint> to see what one is charged.\n")
	}
	return 0
}

func short(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

// breakpointList renders the distinct long-context breakpoints, largest last.
//
// Distinct, because four models sharing one 200k threshold should read "200,000", not
// the same number four times.
func breakpointList(ns []int) string {
	sort.Ints(ns)
	var out []string
	for i, n := range ns {
		if i > 0 && n == ns[i-1] {
			continue
		}
		out = append(out, commas(n))
	}
	return strings.Join(out, "/")
}

// commas groups a token count for reading: 200000 is hard to size at a glance, 200,000
// is not, and these numbers only ever appear in prose meant for a human.
func commas(n int) string {
	s := strconv.Itoa(n)
	if len(s) <= 3 {
		return s
	}
	var b strings.Builder
	lead := len(s) % 3
	if lead > 0 {
		b.WriteString(s[:lead])
	}
	for i := lead; i < len(s); i += 3 {
		if b.Len() > 0 {
			b.WriteByte(',')
		}
		b.WriteString(s[i : i+3])
	}
	return b.String()
}
