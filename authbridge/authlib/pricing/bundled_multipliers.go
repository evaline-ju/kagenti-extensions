package pricing

// bundledMultipliers are the gateway discounts shipped with the binary.
//
// HAND-MAINTAINED, deliberately not generated. bundled.go is derived from LiteLLM's
// public price map; these factors are measured against a live gateway, so
// `make pricing-table` must never be able to clobber them.
//
// Why ship a discount at all, when the bundled rates are vendor list: most Cortex
// installs run through one of these gateways, so list overstates them by a third. The
// alternative was every operator copying twelve rates by hand, which is both more work
// and more fragile — see MultiplierRule for why a scalar survives a repricing and a
// copied rate card does not.
//
// Why SCOPED to a host pattern rather than applied globally: a global default would
// silently understate anyone going straight to api.anthropic.com by 24%, and
// understating hides spend. Scoping means both audiences are right with no
// configuration — the gateways get the discount, everything else gets list — and an
// operator with different terms overrides with one line, which outranks this.
func bundledMultipliers() []MultiplierRule {
	return []MultiplierRule{{
		// Measured 2026-09-10 by differencing x-litellm-response-cost-original across
		// paired non-streamed calls: input, cache-write, cache-read and output, for
		// opus-5, sonnet-5 and haiku-4-5, all came to exactly 0.7600 of the
		// then-current vendor list. A uniform scalar across twelve independent
		// figures, which is what makes one number the honest representation rather
		// than a convenient approximation.
		//
		// Re-measure with the method in docs/plugin-catalog.md if the figures ever
		// look wrong; the drift check in litellm-budget-track will say so first.
		//
		// Streamed responses report a cost of 0 in that header, so this cannot be
		// re-derived from live agent traffic — which is precisely why it is shipped
		// rather than learned.
		Host:   "*.res.ibm.com",
		Factor: 0.76,
		Prov:   ProvBundled,
	}}
}
