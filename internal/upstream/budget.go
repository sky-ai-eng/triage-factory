package upstream

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/metric"

	"github.com/sky-ai-eng/triage-factory/internal/telemetry"
)

// lowBudgetShare is the share of a bucket left below which RecordBudget logs
// a WARN, once per window.
const lowBudgetShare = 0.2

// Budget is one reading of a rate-limit bucket: what an upstream's response
// said is left of it for the credential that made the request, or, for an
// upstream whose responses state nothing of it, what TF counted itself
// spending of it (Used), or, for one whose responses state no count, whether
// it said the bucket is close to its limit (NearLimitReading).
type Budget struct {
	Remaining int
	// Limit is the bucket's size, 0 when the upstream did not say.
	Limit int
	// Reset is when the bucket refills, the zero Time when the upstream did
	// not say. A reading is current until then and is not reported after it:
	// by then the bucket has refilled, whatever it said was left.
	Reset time.Time
	// Used is how many calls this process counted against the bucket in its
	// window, on a reading TF counted itself: Slack sends no budget, so its
	// client counts its own calls to each method and reads Remaining off the
	// limit Slack publishes for it. Such a reading is recorded for a call just
	// made, so Used is at least 1 on it, and 0 on a reading an upstream
	// stated. A counted reading with no published limit has Used alone.
	Used int
	// Quiet keeps the reading out of the low-budget WARN. It is for a bucket
	// whose window is so short that the WARN, which re-arms once a window has
	// reset, would fire about once a window for as long as the bucket stays
	// low: Jira Cloud's per-second burst allowance. A refusal from such a
	// bucket is still counted and a wait on it logged.
	Quiet bool
	// nearOnly marks a NearLimitReading, and near is what it said.
	nearOnly bool
	near     bool
}

// NearLimitReading is a reading of a bucket whose upstream states no count
// for it, only whether it is close to its limit: Jira Cloud's hourly points
// quota, which a response marks with X-RateLimit-NearLimit: true once less
// than 20% of it is left. It reports no remaining or limit. reset is when the
// reading stops describing the bucket, the zero Time when nothing says.
func NearLimitReading(near bool, reset time.Time) Budget {
	return Budget{Reset: reset, nearOnly: true, near: near}
}

// NearLimit reports whether the bucket is close to its limit: what the
// upstream said, on a NearLimitReading, and otherwise whether less than
// lowBudgetShare of it is left. ok is false for a reading that states neither,
// one that did not say its bucket's size.
func (b Budget) NearLimit() (near, ok bool) {
	if b.nearOnly {
		return b.near, true
	}
	share, ok := b.Share()
	return ok && share < lowBudgetShare, ok
}

// Share is the fraction of the bucket left. ok is false when the upstream did
// not say the bucket's size.
func (b Budget) Share() (share float64, ok bool) {
	if b.Limit <= 0 {
		return 0, false
	}
	return float64(b.Remaining) / float64(b.Limit), true
}

// statesRemaining reports whether b says what is left of its bucket. A
// reading an upstream stated does, unless it states only whether the bucket
// is near its limit; a counted one does only against a published limit.
func (b Budget) statesRemaining() bool {
	return !b.nearOnly && (b.Used == 0 || b.Limit > 0)
}

// current reports whether b still describes its bucket at now.
func (b Budget) current(now time.Time) bool {
	return b.Reset.IsZero() || now.Before(b.Reset)
}

// RecordBudget keeps b as the latest reading of bucket in the budget name
// meters for credential, used by orgID, replacing the one before it.
//
// An upstream meters the credential that made a request, and an org can hold
// several (one GitHub App installation per GitHub org it connects), so
// credential is part of the key. It is an opaque id the upstream's client
// chooses, never a token, a login or a user id, because it is a metric label.
// A bucket is one of the budgets the upstream meters separately for that
// credential, often in different units (GitHub's core counts requests and its
// graphql counts points), so buckets are never combined. An upstream's
// headers state its own count of what is left, so the latest reading from any
// caller is accurate and nothing is summed, here or across pods.
//
// The reading is logged at WARN when its bucket is first seen near its limit
// (Budget.NearLimit) in a window, unless it is Quiet. It is logged again once
// the warned reading's reset has passed, or after a reading that is not near.
func RecordBudget(name Name, orgID, credential, bucket string, b Budget) {
	// The gauges that report the registry are created with the counters.
	current()
	if !liveBudgets.Load().record(budgetKey{name, orgID, credential, bucket}, b, time.Now()) {
		return
	}
	if b.nearOnly {
		log.Warn("rate-limit budget low",
			"upstream", name, "org", orgID, "credential", credential, "bucket", bucket,
			"near_limit", true, "reset", b.Reset)
		return
	}
	log.Warn("rate-limit budget low",
		"upstream", name, "org", orgID, "credential", credential, "bucket", bucket,
		"remaining", b.Remaining, "limit", b.Limit, "reset", b.Reset)
}

// RecordRefused counts one response from name that a rate limit refused,
// made for orgID with credential, under the bucket that refused it. It is for
// an upstream whose refusal says which bucket it was: Slack meters each method
// separately, so a 429 from one is that method's. The response is also
// counted by Record under RateLimited; this count is what says which bucket
// it came from, so a refusal against a bucket whose counted reading still
// shows headroom says the published limit counted against is wrong.
func RecordRefused(name Name, orgID, credential, bucket string) {
	current().refused.Add(context.Background(), 1, metric.WithAttributes(
		telemetry.Upstream(string(name)),
		telemetry.OrgID(orgID),
		telemetry.Credential(credential),
		telemetry.Bucket(bucket),
	))
}

// WaitOnLimit waits d out as Sleep does, for a request to name made for orgID
// that a rate limit holds back. It logs the wait at WARN, which is the signal
// local mode has, since it serves no metrics unless TF_METRICS_ADDR is set,
// and adds the time actually waited to upstream.rate_limit.wait.
func WaitOnLimit(ctx context.Context, name Name, orgID string, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	log.Warn("waiting on a rate limit", "upstream", name, "org", orgID, "wait", d)
	start := time.Now()
	err := Sleep(ctx, d)
	current().waited.Add(context.Background(), time.Since(start).Seconds(),
		metric.WithAttributes(telemetry.Upstream(string(name)), telemetry.OrgID(orgID)))
	return err
}

// liveBudgets is the process-wide registry: in-memory, so a restart loses it
// and the next response repopulates it. A pointer only so a test can swap in
// an empty one (ResetBudgetsForTest).
//
// TODO(TFAC-1108): write each pod's changed readings to a shared table, serve
// an org's readings from it at GET /api/orgs/{org_id}/integrations/rate-limits,
// and show them on each integration's settings card.
var liveBudgets atomic.Pointer[budgetRegistry]

func init() { liveBudgets.Store(newBudgetRegistry()) }

type budgetKey struct {
	name       Name
	orgID      string
	credential string
	bucket     string
}

type budgetEntry struct {
	Budget
	// warned is whether the low-budget WARN has fired for this bucket since
	// a reading last said it was not near its limit; warnedUntil is the reset
	// of the reading it fired for, past which a low reading is a new window's.
	warned      bool
	warnedUntil time.Time
}

// stale reports whether e can be dropped at now: its reading is past its
// reset, and any low-budget WARN it carries is for a window that has ended.
// A WARN fired for a reading with no reset clears only on a reading back above
// the threshold, so an entry holding one is kept.
func (e *budgetEntry) stale(now time.Time) bool {
	if e.current(now) {
		return false
	}
	return !e.warned || (!e.warnedUntil.IsZero() && !now.Before(e.warnedUntil))
}

// sweepEvery bounds how often record walks the registry for stale entries.
// observe drops them on every scrape; this is what drops them in a process
// whose gauges are never scraped (local mode without TF_METRICS_ADDR),
// without walking the map on every response.
const sweepEvery = time.Minute

type budgetRegistry struct {
	mu    sync.Mutex
	byKey map[budgetKey]*budgetEntry
	// swept is when record last walked byKey for stale entries.
	swept time.Time
}

func newBudgetRegistry() *budgetRegistry {
	return &budgetRegistry{byKey: map[budgetKey]*budgetEntry{}}
}

// dropStale deletes every stale entry. Callers hold r.mu.
func (r *budgetRegistry) dropStale(now time.Time) {
	for k, e := range r.byKey {
		if e.stale(now) {
			delete(r.byKey, k)
		}
	}
}

// record stores b under k and reports whether it warrants the low-budget WARN.
func (r *budgetRegistry) record(k budgetKey, b Budget, now time.Time) (warn bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if now.Sub(r.swept) >= sweepEvery {
		r.dropStale(now)
		r.swept = now
	}
	e, ok := r.byKey[k]
	if !ok {
		e = &budgetEntry{}
		r.byKey[k] = e
	}
	e.Budget = b
	switch near, known := b.NearLimit(); {
	case b.Quiet || !known || !near:
		e.warned = false
	case !e.warned || (!e.warnedUntil.IsZero() && !now.Before(e.warnedUntil)):
		e.warned, e.warnedUntil, warn = true, b.Reset, true
	}
	return warn
}

func (r *budgetRegistry) get(k budgetKey) (Budget, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.byKey[k]
	if !ok {
		return Budget{}, false
	}
	return e.Budget, true
}

// observe reports every current reading through the budget gauges, and drops
// the stale entries it meets. A reading that did not say its bucket's size
// reports no limit or ratio, one that did not say its reset reports no time
// until it, only a counted reading reports what was used (statesRemaining
// says when it reports what is left), and only a NearLimitReading reports
// near_limit.
func (r *budgetRegistry) observe(o metric.Observer, g budgetGauges, now time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for k, e := range r.byKey {
		if e.stale(now) {
			delete(r.byKey, k)
			continue
		}
		if !e.current(now) {
			continue
		}
		attrs := metric.WithAttributes(
			telemetry.Upstream(string(k.name)),
			telemetry.OrgID(k.orgID),
			telemetry.Credential(k.credential),
			telemetry.Bucket(k.bucket),
		)
		if e.statesRemaining() {
			o.ObserveInt64(g.remaining, int64(e.Remaining), attrs)
		}
		if e.Used > 0 {
			o.ObserveInt64(g.used, int64(e.Used), attrs)
		}
		if e.nearOnly {
			var near int64
			if e.near {
				near = 1
			}
			o.ObserveInt64(g.nearLimit, near, attrs)
		}
		if share, ok := e.Share(); ok {
			o.ObserveInt64(g.limit, int64(e.Limit), attrs)
			o.ObserveFloat64(g.ratio, share, attrs)
		}
		if !e.Reset.IsZero() {
			o.ObserveFloat64(g.untilReset, e.Reset.Sub(now).Seconds(), attrs)
		}
	}
}

// budgetGauges are the observable gauges the registry is reported through.
type budgetGauges struct {
	remaining  metric.Int64ObservableGauge
	used       metric.Int64ObservableGauge
	nearLimit  metric.Int64ObservableGauge
	limit      metric.Int64ObservableGauge
	ratio      metric.Float64ObservableGauge
	untilReset metric.Float64ObservableGauge
}

// registerBudgetGauges creates the budget gauges against m and the callback
// that reports the live registry through them. Errors can only be programmer
// errors (an invalid name), so they are logged and the gauges report nothing.
func registerBudgetGauges(m metric.Meter) {
	var (
		g    budgetGauges
		errs []error
	)
	keep := func(err error) {
		if err != nil {
			errs = append(errs, err)
		}
	}
	var err error
	g.remaining, err = m.Int64ObservableGauge("upstream.rate_limit.remaining",
		metric.WithDescription("What the upstream's latest response said is left of a rate-limit bucket for the credential."))
	keep(err)
	g.used, err = m.Int64ObservableGauge("upstream.rate_limit.used",
		metric.WithDescription("Calls this process counted against a rate-limit bucket in its window, for an upstream whose responses state no budget."))
	keep(err)
	g.nearLimit, err = m.Int64ObservableGauge("upstream.rate_limit.near_limit",
		metric.WithDescription("1 when the upstream's latest response said a rate-limit bucket is close to its limit, 0 when it said it is not, for an upstream that states no count for the bucket."))
	keep(err)
	g.limit, err = m.Int64ObservableGauge("upstream.rate_limit.limit",
		metric.WithDescription("The rate-limit bucket's size, as the upstream's latest response stated it."))
	keep(err)
	g.ratio, err = m.Float64ObservableGauge("upstream.rate_limit.remaining_ratio",
		metric.WithDescription("The fraction of the rate-limit bucket left, as of the upstream's latest response."))
	keep(err)
	g.untilReset, err = m.Float64ObservableGauge("upstream.rate_limit.until_reset", metric.WithUnit("s"),
		metric.WithDescription("Time until the rate-limit bucket refills, per the upstream's latest response."))
	keep(err)
	if len(errs) > 0 {
		log.Warn("rate-limit gauge setup failed", "errors", errs)
		return
	}
	if _, err := m.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		liveBudgets.Load().observe(o, g, time.Now())
		return nil
	}, g.remaining, g.used, g.nearLimit, g.limit, g.ratio, g.untilReset); err != nil {
		log.Warn("rate-limit gauge callback registration failed", "error", err)
	}
}
