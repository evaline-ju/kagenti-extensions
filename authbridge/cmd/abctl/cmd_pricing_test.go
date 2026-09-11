package main

import (
	"bytes"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rossoctl/cortex/authbridge/authlib/pricing"
)

// End to end through the real handler: a real Registry, the real JSON, the real
// renderer. A canned fixture would let the wire shape drift from the producer, which is
// the failure this whole area keeps having.
func realStatServer(t *testing.T) string {
	t.Helper()
	tab, err := pricing.Build(nil) // bundled rates + the shipped gateway discount
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(pricing.NewRegistry(tab).Handler())
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestRunPricing_HostViewShowsTheDiscountApplied(t *testing.T) {
	var out, errb bytes.Buffer
	code := runPricing([]string{
		"--stats-url", realStatServer(t),
		"--host", "ete-litellm.ai-models.vpc-int.res.ibm.com",
	}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, errb.String())
	}
	got := out.String()
	t.Logf("\n%s", got)

	// The measured gateway rates for opus-5: 3.8 in, 19 out.
	for _, want := range []string{"claude-opus-5", "3.8", "19", "multiplier 0.76", "ete-litellm"} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q", want)
		}
	}
	// It must say the figures are scaled, or a reader compares them against a
	// published price list and concludes the tool is broken.
	if !strings.Contains(got, "vendor list scaled") {
		t.Errorf("output does not explain that rates are scaled: %s", got)
	}
	// The bundled table gives sonnet-4-5 a 200k long-context tier, and the host view
	// resolves at prompt size 0 — so its row MUST carry the marker and the footnote
	// must give the breakpoint. Quoting a below-threshold rate as if it were the only
	// rate is the exact failure this package exists to remove.
	if row := modelRow(t, got, "claude-sonnet-4-5"); !strings.Contains(row, "*") {
		t.Errorf("sonnet-4-5 row %q is not marked as having long-context tiers", row)
	}
	if !strings.Contains(got, "long-context rates apply above 200,000 prompt tokens") {
		t.Errorf("no long-context footnote with a breakpoint: %s", got)
	}
	// A model without a tier must not be marked.
	if row := modelRow(t, got, "claude-opus-5"); strings.Contains(row, "*") {
		t.Errorf("opus-5 row %q is marked, but it has no long-context tier", row)
	}
}

func TestRunPricing_UnscaledEndpointSaysSo(t *testing.T) {
	var out, errb bytes.Buffer
	if code := runPricing([]string{"--stats-url", realStatServer(t), "--host", "api.anthropic.com"}, &out, &errb); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	got := out.String()
	if !strings.Contains(got, "no gateway discount") {
		t.Errorf("expected the no-discount note: %s", got)
	}
	// UNSCALED vendor list for opus-5: 5 in, 25 out. Asserted as a whole row, because
	// "5" alone also matches the model name "claude-opus-5" and so proves nothing — and
	// specifically NOT the discounted 3.8/19, which is what a multiplier leaking onto
	// api.anthropic.com would print here.
	row := modelRow(t, got, "claude-opus-5")
	for _, want := range []string{"5", "25"} {
		if !strings.Contains(row, want) {
			t.Errorf("opus-5 row %q missing list rate %q", row, want)
		}
	}
	for _, unwanted := range []string{"3.8", "19"} {
		if strings.Contains(row, unwanted) {
			t.Errorf("opus-5 row %q carries the discounted rate %q on an undiscounted endpoint", row, unwanted)
		}
	}
}

// modelRow returns one model's line, so a rate assertion is scoped to that model rather
// than matching any digit anywhere in the table.
func modelRow(t *testing.T, out, model string) string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		// Exact first field, not Contains: "claude-opus-5" is a prefix of
		// "claude-opus-5-1", and a substring match would silently accept the wrong row.
		if f := strings.Fields(line); len(f) > 0 && f[0] == model {
			return line
		}
	}
	t.Fatalf("no %s row in:\n%s", model, out)
	return ""
}

func TestRunPricing_TableViewListsRowsAndDiscounts(t *testing.T) {
	var out, errb bytes.Buffer
	if code := runPricing([]string{"--stats-url", realStatServer(t)}, &out, &errb); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	got := out.String()
	for _, want := range []string{"Pricing table", "generated from litellm", "Gateway discounts", "res.ibm.com", "UNSCALED"} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q", want)
		}
	}
}

// A tier with no rate must render "-", never 0.00: pricing.Cost refuses to price a
// request that used such a tier, so a zero would misrepresent a coverage gap as free.
func TestRunPricing_UnsetTierRendersAsAbsent(t *testing.T) {
	if got := rate(0); got != "-" {
		t.Errorf("rate(0) = %q, want %q", got, "-")
	}
	if got := rate(3.8); got != "3.8" {
		t.Errorf("rate(3.8) = %q", got)
	}
}

func TestRunPricing_ProxyDownIsActionable(t *testing.T) {
	var out, errb bytes.Buffer
	code := runPricing([]string{"--stats-url", "http://127.0.0.1:1"}, &out, &errb)
	if code == 0 {
		t.Fatal("expected a non-zero exit when the proxy is unreachable")
	}
	if !strings.Contains(errb.String(), "abctl service status") {
		t.Errorf("error does not tell the operator what to check: %s", errb.String())
	}
}
