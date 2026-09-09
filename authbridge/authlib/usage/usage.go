// Package usage aggregates session events into fixed-width time buckets so a
// client can chart volume, errors, latency and cost over wall-clock time.
//
// It lives server-side on purpose. An aggregate built inside a client would
// start empty when that client connected, so two operators watching the same
// pod would see different histories of the same traffic — and neither would see
// anything from before they attached. The store is the only place with the whole
// picture, so the arithmetic belongs next to it.
//
// Memory is O(buckets x distinct labels), independent of event volume: mean and
// standard deviation come from running sums (count, sum, sum-of-squares) rather
// than from retained samples, and every bucket is preallocated in a fixed ring.
// Nothing here grows with traffic.
package usage

import (
	"strconv"
	"sync"
	"time"

	"github.com/rossoctl/cortex/authbridge/authlib/costevent"
	"github.com/rossoctl/cortex/authbridge/authlib/pipeline"
)

const (
	// BucketWidth is the storage resolution. Every window the API offers is a
	// whole multiple of it, and clients fold buckets together for wider views
	// rather than asking the server to pre-aggregate — one storage shape, and a
	// client is free to pick its own on-screen resolution.
	BucketWidth = time.Minute

	// NumBuckets covers the longest window offered (6h). The ring is
	// preallocated at this size for both the all-sessions aggregate and each
	// tracked session.
	NumBuckets = 360

	// MaxWindow is the longest span Snapshot will return.
	MaxWindow = NumBuckets * BucketWidth
)

// defaultMaxSessions bounds how many per-session rings are tracked. Each ring
// is NumBuckets buckets, so this is the knob that bounds worst-case memory.
// Sessions beyond the cap still land in the all-sessions aggregate; only their
// individual breakdown is dropped.
const defaultMaxSessions = 64

// Counts is the per-label tuple accumulated in each bucket.
type Counts struct {
	Requests int64 `json:"requests"`
	Errors   int64 `json:"errors,omitempty"`
	Tokens   int64 `json:"tokens,omitempty"`
	// CostMicros is millionths of a US dollar. An integer unit keeps bucket
	// addition exact and JSON round-tripping lossless, which float dollars do
	// not; a client divides by 1e6 to display. Zero when nothing here could be
	// priced, which is not the same as "this traffic was free" — the API omits
	// the field entirely in that case rather than asserting $0.
	CostMicros int64 `json:"costMicros,omitempty"`
	// PricedRequests counts the requests that actually produced a cost. Coverage
	// is a counter rather than a flag because buckets are summed when a client
	// asks for a coarser resolution, and because a deployment can price some of
	// its traffic and not the rest: several endpoints, rates known for some.
	//
	// Requests-minus-PricedRequests is the gap, correct at every resolution, and
	// it is what stops a partial total being presented as a complete one.
	PricedRequests int64 `json:"pricedRequests,omitempty"`
}

// Add accumulates o into c, field by field.
//
// Exported because consumers fold these too — abctl collapses low-volume series
// into an "(other)" band — and an unexported version left them hand-summing the
// fields in another module. That copy silently missed PricedRequests when it was
// added, under a comment explaining that every field had to be carried. One
// summation, in the same file as the struct, is the only way that stays true.
//
// Pointer receiver and mutating, matching how the aggregator accumulates on the
// hot path. For a map value, read-modify-write: `v := m[k]; v.Add(o); m[k] = v`.
func (c *Counts) Add(o Counts) {
	c.Requests += o.Requests
	c.Errors += o.Errors
	c.Tokens += o.Tokens
	c.CostMicros += o.CostMicros
	c.PricedRequests += o.PricedRequests
}

// Bucket is one BucketWidth slice of time, as served to clients.
//
// A bucket with no traffic is still emitted, with zeroed counts. That is
// deliberate: a client rendering a bar chart must be able to distinguish an idle
// minute from a minute that fell off the end of the ring, and inferring absent
// buckets from timestamps is exactly the kind of thing every client would get
// slightly differently.
type Bucket struct {
	At          time.Time `json:"at"`
	Counts                // totals across every label
	LatMeanMs   float64   `json:"latMeanMs,omitempty"`
	LatStdDevMs float64   `json:"latStdDevMs,omitempty"`
	// LatSamples is how many requests in this bucket carried a duration, which
	// is not always Requests: an unmeasured response counts as traffic but not
	// as a latency sample. Folding needs it to weight each bucket by its real
	// sample count, and a client showing a window-wide mean needs it for the
	// same reason.
	LatSamples int64 `json:"latSamples,omitempty"`
	// Series is the requested grouping, keyed by model / status / plugin name.
	// Nil when group=none.
	Series map[string]Counts `json:"series,omitempty"`
}

// bucket is the internal accumulator. It keeps all three groupings at once so
// the group= parameter is a read-time choice: an operator cycling groupings in a
// TUI sees the same history from each angle, instead of each grouping only
// having data from the moment it was first selected.
type bucket struct {
	start time.Time // truncated to BucketWidth; zero means never written
	Counts
	latSum   float64 // milliseconds
	latSumSq float64 // milliseconds squared, for stddev
	// latN counts only the requests that actually carried a duration. Dividing
	// latSum by Requests instead reports a mean diluted by every unmeasured
	// response: one 2s response plus one unmeasured one reported 1s, not 2s.
	latN     int64
	byMethod map[string]Counts
	byStatus map[string]Counts
	byPlugin map[string]Counts
}

// eventCost is one event's settled cost, decoded once per Record and passed to
// each ring's foldInto.
//
// Cost is read from the figure litellm-budget-track settles per response and
// publishes on the session event (see authlib/costevent): it prefers the
// gateway's own post-discount cost header and falls back to pricing the usage
// block. So the number here is a plugin's measurement, not a rate table's guess,
// and this package holds no rates of its own — deployment-specific pricing is
// not something authlib should assert.
//
// Traffic the plugin did not price contributes no cost and is visible as the gap
// between Counts.PricedRequests and Counts.Requests. Modelled rates for that
// traffic arrive with the pricing resolver; see
// docs/superpowers/specs/2026-09-09-pricing-consolidation-design.md.
type eventCost struct {
	micros int64
	// priced is 1 when a cost was found and 0 otherwise, so it sums into
	// Counts.PricedRequests as a coverage count rather than needing a separate
	// branch at every accumulation site.
	priced int64
}

// decodeEventCost reads the cost event off e, if any. Zero value means unpriced,
// which is not the same as a zero cost — see Counts.PricedRequests.
func decodeEventCost(e *pipeline.SessionEvent) eventCost {
	ce, ok := costevent.Decode(e)
	if !ok {
		return eventCost{}
	}
	return eventCost{micros: ce.Micros(), priced: 1}
}

// Aggregator is a fixed ring of per-minute buckets. Safe for concurrent use.
//
// Expiry is implicit: a slot is indexed by minutes-since-epoch modulo
// NumBuckets, so a write whose timestamp does not match the slot's recorded
// start has landed on a stale bucket from a previous lap and resets it. No
// sweeper goroutine, no cleanup path. The tradeoff is that a large backwards
// clock jump can reset buckets that were still current; for observability
// counters that is acceptable, and it is preferable to a timer that has to be
// stopped on shutdown.
type Aggregator struct {
	mu       sync.RWMutex
	all      []bucket
	sessions map[string]*sessionRing
	maxSess  int
	now      func() time.Time

	// pending holds request-phase plugin names awaiting their response event,
	// keyed by RequestID. See Record.
	pending map[string]*pendingRequest
}

// pendingRequest is the request half of one turn: the plugins that ran before the
// response existed, held until the response arrives so they can be attributed to
// the same bucket with the same tokens and latency.
type pendingRequest struct {
	plugins []string
	at      time.Time // for expiry when no response ever arrives
}

// maxPendingRequests bounds the pending map. A request whose response never
// arrives — client disconnect, upstream hang, a proxy restart mid-turn — would
// otherwise leak an entry per turn forever.
//
// Generous relative to real in-flight concurrency for one sidecar, so eviction is
// a backstop rather than something the steady state relies on.
const maxPendingRequests = 4096

// pendingTTL bounds how long a request half waits for its response. Matched to the
// streaming read timeout in the forward proxy: a turn quiet for longer than that
// has been abandoned by the listener too, so its plugins will never be paired.
const pendingTTL = 5 * time.Minute

// sessionRing is one session's buckets plus the last time it was written.
//
// lastSeen exists because the store evicts and expires sessions without telling
// the aggregator. Without reclamation, a pod that churns through session ids
// fills the map to the cap and then refuses every new session forever:
// /v1/sessions would list a live session while /v1/usage?session=<id> returned
// zeroed buckets, which looks exactly like "this session did nothing". Matching
// the store's own cap only delays that. Reusing the coldest ring instead bounds
// memory AND keeps the newest sessions answerable, which is what an operator is
// looking at.
type sessionRing struct {
	buckets  []bucket
	lastSeen time.Time
}

// Option configures an Aggregator.
type Option func(*Aggregator)

// WithClock overrides time.Now, for deterministic tests.
func WithClock(now func() time.Time) Option { return func(a *Aggregator) { a.now = now } }

// WithMaxSessions bounds the number of per-session rings retained. Each ring is
// NumBuckets buckets, so this is the knob that bounds worst-case memory.
//
// n == 0 means track NO per-session rings: /v1/usage?session=... returns zeroed
// buckets, while the all-sessions aggregate keeps counting everything. That is
// the reading the name implies, and the safe one if this is ever wired to
// operator config — a 0 in a ConfigMap should not silently mean "unlimited".
// Pass a negative n to leave the default in place.
func WithMaxSessions(n int) Option {
	return func(a *Aggregator) {
		if n >= 0 {
			a.maxSess = n
		}
	}
}

// New returns an empty Aggregator.
func New(opts ...Option) *Aggregator {
	a := &Aggregator{
		all:      make([]bucket, NumBuckets),
		sessions: make(map[string]*sessionRing),
		pending:  make(map[string]*pendingRequest),
		maxSess:  defaultMaxSessions,
		now:      time.Now,
	}
	for _, o := range opts {
		o(a)
	}
	return a
}

// Record folds one event into the aggregate.
//
// Counts and timings come from the response event only: a request event carries no
// status, no duration and no usage, so counting it as traffic would double every
// request and pull the latency mean toward zero. Denials (phase "denied") are
// counted as errors — they are requests that happened and failed, and omitting
// them would make an authentication outage look like a traffic drop.
//
// A request event is not ignored, though. The listener splits plugin invocations
// by phase — the request event carries InvocationPhaseRequest, the response event
// InvocationPhaseResponse — so a plugin that only acts on the request appears in
// neither the response event nor, previously, the by-plugin breakdown. context-guru
// and tool-prune are exactly that shape (WritesRequestBody with a stub OnResponse),
// so `by plugin` silently meant "plugins that ran on the response".
//
// Request-phase plugin names are therefore held by RequestID — the field that
// exists for this pairing — and merged when the paired response arrives, so each
// gets the response's own tokens and latency and one turn counts once. Pairing on
// the id rather than positionally matters because a client can have several
// requests in flight at a time.
func (a *Aggregator) Record(sessionID string, e *pipeline.SessionEvent) {
	if e == nil {
		return
	}

	// A request event with nothing to hold is the common case — Invocations is nil
	// on any plain proxied request — and this runs synchronously inside
	// Store.Append on the request hot path. Checked before the lock so that path
	// stays lock-free: neither guard touches aggregator state, so there is nothing
	// to protect.
	if e.Phase == pipeline.SessionRequest && (e.RequestID == "" || e.Invocations == nil) {
		return
	}

	at := e.At
	if at.IsZero() {
		at = a.now()
	}

	// Decoded before the lock, for the same reason the guards above are: it
	// touches no aggregator state, and foldInto runs up to twice per event (the
	// all-sessions ring and this session's ring), which would otherwise unmarshal
	// the same JSON twice while holding mu.
	//
	// Skipped for request events, which return below without ever reaching
	// foldInto. The cost is only ever published on the response pass, so this
	// would find nothing anyway — but not calling it at all beats calling it and
	// relying on that.
	var ec eventCost
	if e.Phase != pipeline.SessionRequest {
		ec = decodeEventCost(e)
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	if e.Phase == pipeline.SessionRequest {
		a.holdRequestPluginsLocked(e, at)
		return
	}
	if e.Phase != pipeline.SessionResponse && e.Phase != pipeline.SessionDenied {
		return
	}

	// Claim the request half, if it is still waiting.
	var requestPlugins []string
	if e.RequestID != "" {
		if p, ok := a.pending[e.RequestID]; ok {
			requestPlugins = p.plugins
			delete(a.pending, e.RequestID)
		}
	}

	t := at.Truncate(BucketWidth)
	a.foldInto(a.all, t, e, requestPlugins, ec)

	if ring, ok := a.sessions[sessionID]; ok {
		ring.lastSeen = at
		a.foldInto(ring.buckets, t, e, requestPlugins, ec)
		return
	}
	// maxSess == 0 means no per-session rings at all — see WithMaxSessions. The
	// event still counts toward the all-sessions total above.
	if a.maxSess == 0 {
		return
	}
	// At the cap, reclaim the least-recently-written ring rather than refuse the
	// new session. The store expires and evicts sessions without notifying us, so
	// the coldest ring is very likely one the store has already dropped; refusing
	// instead would make every session after the first maxSess unanswerable for
	// the life of the process.
	if len(a.sessions) >= a.maxSess {
		a.evictColdestLocked()
	}
	ring := &sessionRing{buckets: make([]bucket, NumBuckets), lastSeen: at}
	a.sessions[sessionID] = ring
	a.foldInto(ring.buckets, t, e, requestPlugins, ec)
}

// holdRequestPluginsLocked stashes a request event's plugin names until its
// response arrives. Caller holds mu.
//
// Nothing is counted here — no requests, no tokens, no latency. The request event
// contributes only the LABELS its plugins need in order to be attributed later.
func (a *Aggregator) holdRequestPluginsLocked(e *pipeline.SessionEvent, at time.Time) {
	if e.RequestID == "" || e.Invocations == nil {
		// Without an id there is nothing to pair against, and pairing positionally
		// misattributes as soon as two requests are in flight — the reason
		// SessionEvent carries RequestID at all.
		//
		// Record checks the same two conditions before taking the lock, so this is
		// normally unreachable; kept so the helper is correct on its own terms
		// rather than relying on its only caller.
		return
	}
	names := invocationPlugins(e.Invocations)
	if len(names) == 0 {
		return
	}
	// Sweep before inserting so a burst of abandoned turns cannot push the map
	// past its bound between sweeps.
	if len(a.pending) >= maxPendingRequests {
		a.expirePendingLocked(at)
	}
	if len(a.pending) >= maxPendingRequests {
		// Still full of live requests: drop this one's labels rather than grow
		// without bound. The response will still be counted, just without its
		// request-phase plugins — losing a label is preferable to unbounded memory
		// on the synchronous append path.
		return
	}
	a.pending[e.RequestID] = &pendingRequest{plugins: names, at: at}
}

// expirePendingLocked drops request halves whose response never arrived. Caller
// holds mu.
func (a *Aggregator) expirePendingLocked(now time.Time) {
	for id, p := range a.pending {
		if now.Sub(p.at) > pendingTTL {
			delete(a.pending, id)
		}
	}
}

// invocationPlugins returns the distinct plugin names in an Invocations set.
//
// Deduped because one plugin can append several invocations to a single pass, and
// counting it twice would inflate its share of a stacked bar.
func invocationPlugins(inv *pipeline.Invocations) []string {
	if inv == nil {
		return nil
	}
	seen := make(map[string]bool, len(inv.Outbound)+len(inv.Inbound))
	var out []string
	for _, list := range [][]pipeline.Invocation{inv.Outbound, inv.Inbound} {
		for _, iv := range list {
			if iv.Plugin == "" || seen[iv.Plugin] {
				continue
			}
			seen[iv.Plugin] = true
			out = append(out, iv.Plugin)
		}
	}
	return out
}

// evictColdestLocked drops the ring with the oldest lastSeen. Caller holds mu.
//
// Linear scan rather than a heap: maxSess is a few dozen, this runs only when the
// map is full and a genuinely new session arrives, and a heap would need
// maintaining on every write instead.
func (a *Aggregator) evictColdestLocked() {
	var coldestID string
	var coldest time.Time
	for id, r := range a.sessions {
		if coldestID == "" || r.lastSeen.Before(coldest) {
			coldestID, coldest = id, r.lastSeen
		}
	}
	if coldestID != "" {
		delete(a.sessions, coldestID)
	}
}

func (a *Aggregator) foldInto(ring []bucket, t time.Time, e *pipeline.SessionEvent, requestPlugins []string, ec eventCost) {
	b := &ring[slot(t)]
	if !b.start.Equal(t) {
		*b = bucket{start: t} // stale lap: reset rather than accumulate onto old data
	}

	var tokens int64
	var model string
	if e.Inference != nil {
		tokens = int64(e.Inference.TotalTokens)
		model = e.Inference.Model
	}

	one := Counts{Requests: 1, Tokens: tokens, CostMicros: ec.micros, PricedRequests: ec.priced}
	if e.StatusCode >= 400 || e.Phase == pipeline.SessionDenied {
		one.Errors = 1
	}
	b.Counts.Add(one)

	// Latency: only from events that actually carry one. A zero duration is
	// "not measured", not "instant", and folding it in would drag the mean down.
	if ms := float64(e.Duration.Milliseconds()); ms > 0 {
		b.latSum += ms
		b.latSumSq += ms * ms
		b.latN++
	}

	if model != "" {
		addLabel(&b.byMethod, truncateLabel(model), one)
	}
	if e.StatusCode > 0 {
		addLabel(&b.byStatus, strconv.Itoa(e.StatusCode), one)
	} else if e.Phase == pipeline.SessionDenied {
		addLabel(&b.byStatus, "denied", one)
	}
	// Per-plugin attribution counts the request once per plugin that ran, so
	// these sub-totals intentionally sum to more than Requests when several
	// plugins touched one message. Tokens are attributed whole to each plugin
	// for the same reason: there is no defensible way to split one response's
	// usage between the plugins that observed it.
	//
	// CostMicros and PricedRequests are duplicated the same way, and both are
	// exposed under group=plugin. Summing cost across the per-plugin series
	// therefore over-reports dollars by the number of plugins that touched each
	// turn — a worse error than over-reporting tokens, because it reads as spend.
	// Use Totals for any dollar figure; the per-plugin values answer "what did
	// traffic this plugin saw cost", not "what did this plugin cost".
	// Invocations is a POINTER and is nil whenever no plugin appended a record —
	// which is the common case for a plain proxied response. Dereferencing it
	// unguarded panics inside Store.Append, i.e. on the request hot path.
	// Response-phase plugins from this event, plus the request-phase plugins held
	// from its paired request event. Deduped across the two halves: a plugin that
	// ran in both phases is still one plugin that touched one turn.
	seen := make(map[string]bool, 4)
	for _, name := range invocationPlugins(e.Invocations) {
		seen[name] = true
		addLabel(&b.byPlugin, name, one)
	}
	for _, name := range requestPlugins {
		if seen[name] {
			continue
		}
		addLabel(&b.byPlugin, name, one)
	}
}

// maxLabelsPerBucket caps distinct keys in one bucket's grouping map.
//
// The model name comes off the request body, so its cardinality is set by
// whatever the client sends, not by anything this process controls. Unbounded, a
// caller varying the model string every request would add a retained map entry
// and a retained string per request, in a ring that only frees a slot when it is
// reused a full lap later — 6h at one minute. That is memory growth driven from
// off-host, on the synchronous session-append path.
//
// 64 is well above any real deployment: a pod talks to a handful of models, a
// few status codes and its own plugin list. Past the cap, further keys fold into
// overflowLabel so the totals stay correct and the excess is visible rather than
// silently dropped.
const maxLabelsPerBucket = 64

// overflowLabel collects everything past maxLabelsPerBucket. Named rather than
// dropped so a chart's segments still sum to the bucket total, and so an
// operator can see that cardinality was capped instead of wondering why a model
// is missing.
const overflowLabel = "(other)"

// maxLabelLen bounds one retained label. The model name is request-controlled, so
// without this a caller could park a megabyte of string in a bucket that lives
// for a full ring lap. Long enough for any real model id, including provider
// prefixes and dated suffixes.
const maxLabelLen = 96

func truncateLabel(s string) string {
	if len(s) <= maxLabelLen {
		return s
	}
	return s[:maxLabelLen]
}

func addLabel(m *map[string]Counts, key string, c Counts) {
	if *m == nil {
		*m = make(map[string]Counts, 4)
	}
	// Reserve the last slot for overflowLabel: switching to it only once the map
	// is already full would make the overflow key itself the (cap+1)th entry.
	if _, ok := (*m)[key]; !ok && len(*m) >= maxLabelsPerBucket-1 {
		key = overflowLabel
	}
	cur := (*m)[key]
	cur.Add(c)
	(*m)[key] = cur
}

func slot(t time.Time) int {
	s := int(t.Unix()/int64(BucketWidth/time.Second)) % NumBuckets
	if s < 0 {
		s += NumBuckets
	}
	return s
}
