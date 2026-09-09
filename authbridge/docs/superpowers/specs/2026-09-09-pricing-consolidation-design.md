# Consolidated Model Pricing — Design

**Date:** 2026-09-09
**Status:** Converged design — ready for implementation plan.
**Issue:** cortex #910 — "feature: consistent model cost across the board"
**Repos touched:** `cortex/authbridge` only.

> **Terminology.** *Rate* = USD per token for one tier of one model at one
> endpoint. *Tier* = which kind of token (uncached input, cache write, cache
> read, output). *Provenance* = where a rate came from, which travels with every
> figure. *Coverage* = what fraction of requests in a window produced a cost at
> all.

## Problem

Model cost is configured and computed in several places that do not agree.

**Two plugins own independent rate tables** with incompatible shapes:

| | `toolprune` | `litellm_budgettrack` |
|---|---|---|
| Rate knobs | 12 (`plugin.go:86-115`) | 4 (`plugin.go:55-63`) |
| Per-model keys | yes, with globs | **no** — flat scalars only |
| Per-million unit | yes | no |
| Output rate | **no** | yes |
| Built-in defaults | yes (`pricing.go:43-63`) | no |
| Cache fallback | write→read→input per tier (`plugin.go:155-174`) | write→input, read→input (`plugin.go:68-82`) |

Neither table alone can price a whole request: `toolprune` has no output rate
(it prices removed *prompt* bytes), `litellm_budgettrack` has no model
dimension at all.

**Dollar arithmetic is duplicated across five sites**, three of them in the
**client**:

| Site | What it computes |
|---|---|
| `toolprune/metrics.go:214,219` | `$ saved`, `$ saved / request` |
| `litellm_budgettrack/plugin.go:191` + ledger | daily spend, budget deny |
| `cmd/abctl/tui/prune_saving.go:53-77` | saved tokens × the tier's rate |
| `cmd/abctl/tui/cost_event.go:69-80` | tier-weighted prompt cost (`promptCost`) |
| `cmd/abctl/tui/usage_render.go:332-333` | the `snap.Priced` footer |

The client recomputes only because rates reach it exclusively as event payload
(`toolprune/event.go:36-41`). Note the trend: `promptCost` is a *second*
client-side tier-weighted implementation, added while this issue was open — the
duplication is actively growing, not static.

**Token extraction is duplicated, and the pricing path uses the un-shared copy.**
`inference-parser` is the designated producer, filling `Extensions.Inference`
via `parsercommon.TokenUsage.Fill`; `sessionbudget`, `toolprune`, `cpex` and
`opa` all consume it. `litellm_budgettrack` instead runs its own
`parseFrameUsage` / `usageJSON` / `frameUsage` (`plugin.go:353-430`) with its own
cross-frame reconciliation. The two reconciliations are the same algorithm
written twice — `mergeAnthropicPromptMaxSeen` (`inferenceparser/anthropic.go:393-406`)
versus the max-per-bucket block in `OnResponseFrame` (`litellm_budgettrack/plugin.go:214-235`)
— and they already differ in one rule: output is a **cumulative assign** in
`inference-parser` (`anthropic.go:382`) and a **max-of-seen** in
`litellm_budgettrack` (`plugin.go:232-234`). Identical on a monotonic stream,
not the same rule. The budget plugin's copy also drops `Present` bits and
`Reasoning`.

**The aggregator seam is wired to nothing.** `usage.Pricer` and
`Counts.CostMicros` exist (`usage.go:107-129`); no caller supplies a Pricer, so
`/v1/usage` reports `priced:false` and `abctl` renders no cost. Two `TODO(cost)`
blocks (`usage/usage.go:114`, `sessionapi/usage.go:46`) name this issue and
propose *different* fixes — promote the rates to a shared package, versus have
the plugin that knows the true cost report it. The design below does both,
because they are complementary rather than alternatives.

### What has already landed upstream

Checked against `upstream/main` at `25e612f8`, because two of this issue's
sub-problems are partly solved and the design must not re-propose them.

**The per-request cost event exists.** `litellm_budgettrack` already publishes
`costEvent{cost_usd, source, daily_total_usd, daily_max_usd}` via `emitCost`
(`plugin.go:303-309`), from both the header path (`plugin.go:204`) and the
terminal-frame path (`plugin.go:275`), with `source` ∈ {`gateway-header`,
`usage-fallback`} (`plugin.go:88-89`). `abctl` decodes it
(`cmd/abctl/tui/cost_event.go:41-54`) under a test that pins every field.

Two consequences. First, `sessionapi/usage.go:55-61`'s claim that a real cost
"would need the cost surfaced onto the session event […] today it stays inside
the plugin" **is now stale** — it is on the event; the aggregator simply does not
read it. Second, `source` already draws the authoritative/modelled distinction
this design needs, so provenance must **extend** that field rather than rename
it: `abctl`'s decode test pins the tags, and additive is safe where a rename is
not.

**A per-tier `ok` discipline is already house style.** `promptCost`
(`cost_event.go:69-80`) returns `ok=false` when `RateSource == "none"`, under the
comment that this avoids "a $0.00 that would read as a free prompt". The `Cost`
invariant below is that rule generalised, not a new policy.

### Two grounding facts that change the shape of the fix

**1. The built-in table cannot express versions that price differently, and
that is a live error, not a hypothetical.** `pricing.go:22-31` keys by family
(`*claude-opus-*`) and states the tradeoff plainly: *"this assumes a family
bills at one rate."* Against LiteLLM's public price map today,
`claude-opus-4-1` is **$15/$75** per MTok while `claude-opus-4-5` and
`claude-opus-5` are **$5/$25** — a **3× error** between two models that both
match the same glob.

**2. The "~4x understatement" that blocked wiring the Pricer is stale.**
`usage.go:118-121` and `sessionapi/usage.go:50-53` assert the built-in
gateway-measured table understates a direct-to-Anthropic deployment by roughly
4×. Measured against current first-party list prices it is a uniform **0.76×**
— a flat 24% discount:

| Family | `toolprune` built-in | Current list | Ratio |
|---|---|---|---|
| opus | $3.80/MTok | $5.00 | 0.76 |
| sonnet | $1.52 | $2.00 | 0.76 |
| haiku | $0.76 | $1.00 | 0.76 |

The 4× figure is a fossil from when the current opus was `claude-opus-4-1` at
$15/MTok ($15 ÷ $3.80 ≈ 3.9). Opus list dropped to $5 with 4.5. The stated
objection therefore no longer holds at the stated magnitude — but the fact that
a hand-maintained table rotted silently for a whole model generation is itself
the argument for sourcing rates from data rather than from measurement.

## Goals / Non-goals

**Goals**

1. One owner of rates; one function that turns tokens into dollars.
2. Rates keyed by **(endpoint, model, tier)** — because the same model costs
   different amounts at different endpoints.
3. Distinguish models whose prices differ, including within a family.
4. Discover rates automatically where the endpoint publishes them; accept
   manual rates where it does not.
5. Prefer a real, authoritative per-request cost over a modelled one.
6. Never present an incomplete dollar total as a complete one.
7. `/v1/usage` and `abctl` stop being cost-blind.

**Non-goals**

- Deriving a gateway's negotiated discount by regression over observed
  header costs. Discovery returns the gateway's effective rates directly, which
  makes inference unnecessary.
- Modelling every tier LiteLLM can express (flex / priority / ultrafast service
  tiers). See *Tier scope*.
- Billing, invoicing, or reconciliation against a provider's books. This is
  observability and budget enforcement.
- Changing `sessionbudget`, which is token-denominated by design and has no
  rate config.

## Design decisions

| Decision | Choice | Why |
|---|---|---|
| Where rates live | New `authlib/pricing` package + top-level `pricing:` config | Sibling of `mtls` / `spiffe` / `tls_bridge`; no new config concept |
| Key | `(endpoint, model, tier)` + context threshold | The same model prices differently per endpoint; a global profile is per-deployment when the choice is per-request |
| Rate sources | authoritative → configured → discovered → bundled → none | An observed cost beats a modelled one; an explicit operator rate beats a fetched one |
| LiteLLM endpoints | Discover via `GET /model/info` | It publishes rates, resolves aliases, and honours operator overrides |
| Endpoints with no pricing API | Manual config, shipped pre-populated | Anthropic publishes no rate card; its list prices are public, so the block ships filled in rather than blank |
| Tiers | input, cache-write, cache-read, output + context thresholds | Matches what LiteLLM publishes and what long sessions actually hit |
| Coverage reporting | Counters, not a boolean | A bool cannot fold across buckets and cannot express a partial window |
| Token source | `Extensions.Inference` only | One parser; the pricing path stops using the un-shared copy |
| Injection | `pricing.ResolverConsumer`, mirroring `spiffe.ProviderConsumer` | Established precedent (`registry.go:316-368`) rather than a new mechanism |

## Verified external facts

Recorded because the design depends on them and they are checkable.

**Anthropic publishes no rate card.** `GET /v1/models` returns `id`,
`display_name`, `created_at`, `max_input_tokens`, `max_tokens`, `capabilities` —
no prices. The Usage & Cost Admin API (`/v1/organizations/cost_report`,
`/v1/organizations/usage_report/messages`) returns the organisation's **actual
historical spend**: Admin API key required (workspace keys rejected), daily
granularity for cost, ~5 minute lag. Useful for reconciliation, unusable as a
rate card, and blind to a LiteLLM→Bedrock path since it only knows spend booked
at Anthropic.

**LiteLLM publishes rates two ways.**

1. *Static public map* — `model_prices_and_context_window.json` in
   `BerriAI/litellm`: 2.3 MB, 3,853 models, no auth. Fields:
   `input_cost_per_token`, `output_cost_per_token`,
   `cache_creation_input_token_cost`, `cache_read_input_token_cost`, and
   `cache_creation_input_token_cost_above_1hr`. The endpoint dimension is
   encoded in the **key** — `claude-opus-5` $5.00, `us.anthropic.claude-opus-5`
   $5.50, `us-gov.anthropic.claude-opus-5` $6.00,
   `databricks/databricks-claude-opus-5` $5.00003.
2. *Runtime `GET /model/info`* — `proxy_server.py:14712-14721`, also registered
   at `/v1/model/info`. Auth is `Depends(user_api_key_auth)`, so **any valid
   virtual key** works, not only the master key. Credentials are stripped from
   the response (`remove_sensitive_info_from_deployment`, `proxy_server.py:14707`).
   Per deployment it returns `model_name` (the **client-facing alias**),
   `litellm_params.model` (the underlying provider model), and `model_info`
   carrying the rates.

**Discovery returns *effective* rates, not list.** `_get_proxy_model_info`
(`proxy_server.py:14673-14709`) resolves in three passes — the operator's config
`model_info`, then `litellm.get_model_info(litellm_params.model)`, then a retry
with the provider prefix stripped — and merges with `if k not in model_info`
(`proxy_server.py:14702-14704`), so **configured pricing wins over the map**. A
gateway with a negotiated discount reports the discount. This read path does not
require `store_model_in_db` (its own sample response shows `db_model: false`);
that requirement applies to the model CRUD routes.

**Why discovery is structurally necessary, not a convenience.** Both parsers
record the model from the **request** body — `inferenceparser/anthropic.go:87`
and `inferenceparser/plugin.go:104` both do `Model: req.Model`. That is the name
the *client* sent, i.e. the gateway's alias, and only the gateway knows the
alias→underlying-model mapping. The static map is keyed by LiteLLM's internal
identifier, so an alias cannot be resolved against it from the client-visible
name alone.

**The endpoint dimension already exists on the wire.** `pctx.Host`
(`pipeline/context.go:108`) at request time, and `SessionEvent.Host`
(`pipeline/session.go:126-131`), whose doc already states the intent: *"for
outbound events it's the target service, which is the useful case — a session
with many outbound calls can be attributed to the tool / LLM / target each
landed on."*

## Tier scope

LiteLLM's `ModelInfoBase` (`litellm/types/utils.py:232-269`) models far more
tiers than authbridge does: `cache_creation_input_token_cost_above_1hr`,
`input_cost_per_token_above_200k_tokens`,
`cache_read_input_token_cost_above_200k_tokens` / `_above_272k` / `_above_512k`,
plus flex / priority / ultrafast service-tier variants.

**In scope:** the four base tiers, each optionally overridden above a context
threshold. Long-context premiums are not exotic for 1M-context Opus running an
agent — crossing 200k prompt tokens is what a long session *is*, and today's
three flat tiers price that traffic at the base rate.

**Out of scope:** service-tier variants (flex / priority / ultrafast) and the
1-hour-TTL cache-write premium. Both are real, neither is on a Claude path we
run today, and unpopulated tiers are untested tiers. Discovery ignores those
fields; adding one later is a field on `Rates` plus a threshold row, not a
reshape.

## Architecture

### New package `authlib/pricing/`

Single owner of rates and of the arithmetic.

```go
// Tier names which kind of token is being priced.
type Tier int
const (TierInput Tier = iota; TierCacheWrite; TierCacheRead; TierOutput)

// Rates is one (endpoint, model) pair's rates, in USD per token.
// Thresholds override Base above a prompt-token count, highest match winning.
type Rates struct {
    Base       [4]float64
    Thresholds []ContextThreshold // {AbovePromptTokens int; Rate [4]float64; Set [4]bool}
    Set        [4]bool            // which tiers actually have a rate
}

// Provenance travels with every figure so no consumer has to guess.
type Provenance int
const (ProvNone Provenance = iota; ProvBundled; ProvDiscovered; ProvConfigured; ProvAuthoritative)

type Resolver interface {
    Resolve(endpoint, model string, promptTotal int) (Rates, Provenance)
}

// Cost is the ONE place tokens become dollars.
func Cost(r Rates, u parsercommon.TokenUsage) (micros int64, ok bool)
```

**`Cost` invariant — a request is priced only if every tier that carried tokens
had a rate.** Otherwise `ok` is false and the request counts as *unpriced*,
never as under-priced. This is not new policy; it is `toolprune`'s existing rule
promoted. `rateFor` (`toolprune/plugin.go:155-174`) already returns a bool for
exactly this reason, documented there: a model configured with only
`cache_read_cost_per_token`

> *"used to resolve as 'priced' and then return 0 for a cache-write request —
> pricing it at zero while still counting toward the priced denominator, so the
> saving silently vanished with no `requests unpriced` row to show it had."*

Integer micros (millionths of a dollar) because `Counts.CostMicros` is already
that unit (`usage/usage.go:51-56`) and it keeps bucket addition exact.

### Resolution order

Per `(endpoint, model, tier)`, first hit wins:

1. **`ProvAuthoritative`** — a real observed cost for this request
   (`X-Litellm-Response-Cost`, or its `-Original` variant). Not a rate; a
   settled figure. Bypasses `Cost` entirely.
2. **`ProvConfigured`** — an explicit `pricing.endpoints[].models` entry. An
   override is an override, so it outranks a fetched value.
3. **`ProvDiscovered`** — a cached `GET /model/info` answer for this endpoint.
4. **`ProvBundled`** — the shipped slice of the public map.
5. **`ProvNone`** — no rate. The request is unpriced.

Within a level, an **exact model key beats a glob**, and longer globs beat
shorter ones — `toolprune`'s existing rule (`plugin.go:190-209`, `pricing.go:80-85`)
promoted unchanged. Endpoint matching uses host globs with the port stripped,
reusing the established idiom (`sparc/collect.go:148-156`).

### Discovery

Per-endpoint, opt-in, and failure-tolerant.

- On `Init`, and every `refresh` interval, `GET {endpoint}/model/info` with the
  configured key. Index `model_name` → `Rates` built from `model_info`.
- A fetch failure or timeout **never fails a request**: the previous snapshot is
  retained, and if there is none, resolution falls through to
  `ProvBundled`/`ProvNone`. Same fail-soft posture as the config reloader, which
  keeps a stale bundle rather than deleting it.
- Both `model_name` and `litellm_params.model` are indexed, so traffic naming
  either the alias or the underlying model resolves.
- Status (last success, model count, last error) is exposed on the existing
  diagnostic listener alongside `/reload/status`, so "why is my traffic
  unpriced" is answerable without a log dive.

### Injection

Plugins take no construction arguments and build dependencies from local config
inside `Configure` (`plugins/registry.go:15-20`). The documented exception is
the framework SPIFFE provider, injected via `BuildWithSPIFFE` +
`spiffe.ProviderConsumer` **before** `Configure` runs so config code can use it
(`registry.go:316-368`), with `SPIFFEConsumerPlugins()` (`registry.go:96-107`)
probing for consumers the binary forgot to provision.

Pricing follows that pattern exactly: `pricing.ResolverConsumer` with
`SetPricingResolver`, injected before `Configure`, nil-tolerant, with an
equivalent consumer probe.

**One in-passing cleanup.** A second injected service would give us
`BuildWithSPIFFEAndPricing`, and a third would be worse. Introduce:

```go
type Deps struct {
    SPIFFE  *spiffe.Provider
    Pricing pricing.Resolver
}
func BuildWithDeps(entries []config.PluginEntry, d Deps, opts ...pipeline.Option) (*pipeline.Pipeline, error)
```

`Build` and `BuildWithSPIFFE` become thin wrappers, so no caller or test
changes. This is the only refactor outside the feature's own surface and it
exists to stop a combinatorial explosion the feature would otherwise start.

The resolver is constructed once at binary composition and shared by the
pipeline builder *and* the usage aggregator — the aggregator needs it too, and a
per-plugin instance would mean N discovery pollers per sidecar.

### Plugin changes

**`toolprune`** — delete `pricing.go`'s `defaultPatterns` and its glob machinery,
all 12 rate knobs, `modelRates`, `normalize`, `rateFor`, `set`, `ratesFor`, and
the `rateSource` enum. Consume the resolver. It keeps publishing resolved rates
on its event: the counterfactual genuinely needs them, because it prices tokens
that were never sent and therefore never appear in any authoritative cost.

Its `$ saved` metric keeps its existing caveats and its `requests unpriced` row
(`metrics.go:215-234`), now fed by resolver provenance rather than a local enum.
The `usedDefaultRates` note (`metrics.go:190-201`) becomes provenance-driven.
Its "understates list pricing" wording stays accurate — at 0.76× the built-in
table does understate list — but the accompanying source comment's claim that
the figure is *"several times low"* for anyone paying vendor list must go: that
was true against $15/MTok opus and is now a 24% gap, not a multiple.

**`litellm_budgettrack`** — delete `parseFrameUsage`, `usageJSON`, `frameUsage`,
the max-per-bucket reconciliation, and all four rate knobs. Read
`Extensions.Inference`. Keep `max_budget`, keep header authority, keep the
exactly-once settle guard (`plugin.go:240-252`) — a listener that dispatches
`last=true` twice must still not double-charge.

Its cost event stays; only `source` changes, additively. Today
`usage-fallback` means "priced from the plugin's own rates" and collapses every
modelled case into one label. It becomes the rate's provenance —
`configured` / `discovered` / `bundled` — with `gateway-header` unchanged and
`usage-fallback` retained as the value when provenance is unavailable, so
`abctl`'s pinning decode test keeps passing and older consumers keep parsing.

It declares `Requires: ["inference-parser"]` so a pipeline missing the parser
fails at startup with a named error rather than silently pricing zero — exactly
what `validateRelationships` (`registry.go:370-448`) exists for. This is the one
change that can break a working deployment: anyone running
`litellm-budget-track` without `inference-parser` goes from working to a startup
error. That is deliberate — the alternative is a budget that silently never
triggers — and it belongs in the release note.

### Aggregator and wire format

`foldInto` currently collapses four tiers into `e.Inference.TotalTokens` and
hands that to a scalar `Pricer` (`usage/usage.go:405-412`), which structurally
cannot price cache-read at 0.1× input. Replace with:

1. If the event carries a `litellm-budget-track` cost event, use its `cost_usd`
   and its `source` as provenance. This is a decode of data already on the wire
   (`cost_event.go:41-54` proves it decodes cleanly) — the cheapest correct
   answer available, and the one the stale TODO was waiting for.
2. Else resolve `(e.Host, e.Inference.Model, promptTotal)` and call `Cost` with
   the full `TokenUsage`.
3. Else the request is unpriced.

Step 1 alone closes both `TODO(cost)` blocks for any pipeline running
`litellm-budget-track`, independently of everything else in this design. It is
therefore sequenced first among the aggregator work.

`Pricer` and `WithPricer` are deleted rather than re-signatured — no caller has
ever supplied one, so there is nothing to migrate.

**Coverage counters replace the `Priced` boolean.** `Snapshot.Priced`
(`usage/snapshot.go:58-61`) means "a Pricer was configured", which is wrong in
two ways once rates are per-endpoint. Mixed traffic is the normal case for a
laptop hitting several endpoints: `true` would present a total that silently
omits unpriced traffic, and `false` would let one stray endpoint erase cost
display for everything. It also **cannot be folded** — `Counts.add` sums fields
(`usage.go:63`) and the aggregator folds buckets to any requested `resolution`,
so a per-snapshot bool cannot express "12 of 40 requests in this bucket were
priced."

```go
type Counts struct {
    Requests       int64
    CostMicros     int64
    PricedRequests int64 // NEW — requests that contributed a cost
    ...
}
```

Unpriced is `Requests - PricedRequests`, correct at every resolution. Plus a
snapshot-level `UnpricedBy map[string]int64` keyed `endpoint|model`, capped and
truncated the way `byMethod` already is via `addLabel` / `truncateLabel`, so an
operator sees which pricing block is missing. `Priced` stays on the wire as a
derived convenience (`PricedRequests > 0`); its meaning shifts from "a Pricer
exists" to "something actually priced", which is strictly more truthful and not
a shape change.

This directly precedents on `toolprune`, which already reports coverage
alongside its total under the comment *"An incomplete pricing table is a gap in
the dollar total, so name it"* (`metrics.go:225`).

### `abctl`

`savedTokensAndCost` (`prune_saving.go:53-77`) and `promptCost`
(`cost_event.go:69-80`) both stop computing dollars and render what the event
carries. `promptCost`'s `ok=false`-on-`none` behaviour — show tokens, omit the
`$` — is the model for the unpriced case everywhere.

**One existing decision this reverses, deliberately.** `cost_event.go:25-27`
decodes `Source` but pointedly does *not* render it, reasoning that the COST
column shows modelled and gateway-stamped figures identically because "the
request-side cost […] is modelled too. Marking one and not the other would be the
inconsistency." That reasoning is sound *today*. Once request-side cost also
carries provenance, the asymmetry it guards against no longer exists, so
rendering provenance on both becomes the consistent choice. The reversal is
conditional on that, and the phase that renders it must land after the phase that
gives the request side provenance.

The usage footer (`usage_render.go:330-333`) gains three states:

| Coverage | Render |
|---|---|
| none | `COST unavailable` |
| partial | `COST $1.8421 (42/57 priced)` |
| full | `COST $1.8421` + provenance label |

## Config

```yaml
pricing:
  endpoints:
    # A LiteLLM gateway: ask it what it charges.
    - hosts: ["litellm-a.corp", "*.litellm.internal"]
      discover:
        key_ref: { env: LITELLM_KEY }   # or file:
        refresh: 15m

    # No pricing API: manual rates. Ships pre-populated from public data.
    - hosts: ["api.anthropic.com"]
      models:
        "claude-opus-5":
          input_cost_per_million: 5.00
          cache_write_cost_per_million: 6.25
          cache_read_cost_per_million: 0.50
          output_cost_per_million: 25.00
        "claude-opus-4-1":
          input_cost_per_million: 15.00
          cache_write_cost_per_million: 18.75
          cache_read_cost_per_million: 1.50
          output_cost_per_million: 75.00
```

- Rates accept `*_per_million` (the published unit) or `*_per_token`; setting
  both for one tier is a config error, as `toolprune` already enforces
  (`plugin.go:139-150`).
- `above_prompt_tokens:` blocks nest under a model for context thresholds.
- Absent `pricing:` means bundled-only resolution — never a hard failure.
- Hot reload: `pricing:` is not on the reloader's refusal list (`mode`,
  `listener`, `session` — `reloader.go:309-318`), so it reloads by pipeline
  rebuild. The resolver is shared with the aggregator, which outlives a
  rebuild, so it is swapped in place rather than reconstructed.

### The bundled slice

A generated Go file holding Anthropic-family entries extracted from the public
map, with the upstream commit SHA recorded in a constant. A `make` target
regenerates it. This replaces three hand-measured family globs with data that
already distinguishes `claude-opus-4-1` from `claude-opus-5`, and turns "refresh
the rates" from a measurement exercise into a scripted diff.

## Testing & success criteria

**Unit**

- Resolution precedence table: every provenance level, exact-beats-glob,
  longest-glob-wins, endpoint scoping, port stripping.
- Context-threshold crossing, including a request exactly at the boundary.
- `Cost` invariant: a tier carrying tokens with no rate yields `ok == false`,
  not a partial sum. Direct regression cover for the `rateFor` hazard.
- Coverage folding: `Counts.add` and `fold` keep `PricedRequests` consistent
  when buckets merge at coarser resolution.

**Integration**

- A discovery fake serving a real-shaped `/model/info` payload: alias
  resolution, config-override precedence, fetch failure retaining the previous
  snapshot, and cold-start failure falling through to bundled.
- **SSE equivalence test (the key regression guard).** A canned Anthropic
  streaming body, including the `?beta=true` shape that defers cache counts to
  `message_delta`, must price identically before and after
  `litellm_budgettrack` drops its own parser. This is the riskiest change in the
  set and the only one where a silent divergence would be invisible.
- Golden test pinning the bundled slice to its recorded upstream commit, so the
  table cannot rot silently the way the 4× comment did.
- `/v1/usage` end to end: mixed priced and unpriced traffic reports a partial
  total with correct counters and names the unpriced pairs.

**Success criteria**

1. Exactly one rate table, one `Cost` function, one token parser.
2. `claude-opus-4-1` and `claude-opus-5` price 3× apart, from bundled data, with
   no operator config.
3. A laptop hitting two LiteLLM gateways and `api.anthropic.com` prices all
   three correctly — discovery for the gateways, manual for Anthropic.
4. `/v1/usage` and `abctl` show cost, labelled with provenance, and say so when
   coverage is partial.
5. `litellm_budgettrack`'s ledger and `/v1/usage` agree on the same request.

## Implementation surface

**New:** `authlib/pricing/` (rates, resolver, `Cost`, host matching, discovery
client, generated bundled slice) · `pricing.ResolverConsumer` · `Deps` +
`BuildWithDeps`.

**Deleted:** `toolprune/pricing.go` defaults and glob machinery · 12 `toolprune`
rate knobs · `toolprune`'s `modelRates` / `normalize` / `rateFor` / `set` /
`ratesFor` / `rateSource` · `litellm_budgettrack`'s `parseFrameUsage` / `usageJSON` /
`frameUsage` / reconciliation / 4 rate knobs · `usage.Pricer` / `WithPricer` ·
client-side arithmetic in `prune_saving.go`.

**Changed:** `config.Config` (+`Pricing`) · `usage.Counts` (+`PricedRequests`) ·
`usage.Snapshot` (+`UnpricedBy`, `Priced` re-derived) · `usage.foldInto` ·
`litellm_budgettrack`'s `costEvent.source` values (additive) · `toolprune` and
`litellm_budgettrack` configure/price paths · `abctl` `prune_saving.go`,
`cost_event.go`, `usage_render.go` · `plugin-catalog.md`,
`tool-prune-plugin.md`, `litellm-budgettrack-plugin.md`,
`laptop-token-savings.md` · the stale `TODO(cost)` blocks and the stale
"several times low" comment.

**Net:** ~17 rate knobs → one `pricing:` section · 3 hand-measured globs → a
generated slice · 2 token parsers → 1 · 5 arithmetic sites → 1.

## Phasing (for the implementation plan)

0. **Aggregator consumes the existing cost event** — `foldInto` decodes
   `litellm-budget-track`'s `costEvent`, populates `CostMicros` and the coverage
   counters, deletes `Pricer` / `WithPricer`, closes both `TODO(cost)` blocks.
   **Independently shippable and independently valuable**: it needs none of the
   packages below, and it makes `/v1/usage` and `abctl` show real cost for any
   pipeline already running `litellm-budget-track`. Sequenced first so the
   longest-standing gap closes in the smallest diff.
1. **`authlib/pricing` standalone** — types, resolution, `Cost`, host matching,
   bundled slice + generator + golden test. No consumers. Fully unit-tested in
   isolation.
2. **Injection** — `Deps` / `BuildWithDeps`, `ResolverConsumer`, consumer probe.
   Behaviour-neutral.
3. **`toolprune` migration** — swap to the resolver, delete its table. Existing
   `$ saved` tests are the oracle; the bundled slice changes some expected
   figures, which the diff must state per model rather than bulk-update.
4. **`litellm_budgettrack` migration** — SSE equivalence test written *first*,
   then drop the parser and rate knobs, add `Requires`, refine `source` into
   provenance.
5. **Aggregator resolver fallback** — pricing for requests with no cost event
   (no `litellm-budget-track` in the pipeline), plus `UnpricedBy`.
6. **`abctl`** — render provenance on both sides and the three coverage states.
   Must follow phase 3, which is what makes rendering provenance consistent
   rather than asymmetric (see the `cost_event.go:25-27` note above).
7. **Discovery** — the client, refresh loop, fail-soft, status endpoint. Last
   because every prior phase is useful without it, and it is the only phase with
   an outbound dependency and a credential.

## Open questions

None blocking. Two to settle during implementation:

- `/model/info` versus `/model_group/info` as the discovery target. Both expose
  the client-facing name; `/model_group/info` (`proxy_server.py:15006`) is
  documented as the end-user surface. Phase 7 checks which is more stable across
  LiteLLM versions and may index both.
- Whether the bundled slice covers non-Anthropic families. Anthropic-only keeps
  it small and matches current traffic; the generator makes widening a one-line
  filter change.
