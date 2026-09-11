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
	// Vendor list for opus-5 input.
	if !strings.Contains(got, "5") {
		t.Errorf("expected list rates: %s", got)
	}
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
