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
// said is left of it for the credential that made the request.
//
// TODO(TFAC-1111): a near_limit reading, for a bucket whose upstream says only
// that it is close (Jira Cloud's hourly quota), with the low-budget WARN keyed
// on it as well.
type Budget struct {
	Remaining int
	// Limit is the bucket's size, 0 when the upstream did not say.
	Limit int
	// Reset is when the bucket refills, the zero Time when the upstream did
	// not say. A reading is current until then and is not reported after it:
	// by then the bucket has refilled, whatever it said was left.
	Reset time.Time
}

// Share is the fraction of the bucket left. ok is false when the upstream did
// not say the bucket's size.
func (b Budget) Share() (share float64, ok bool) {
	if b.Limit <= 0 {
		return 0, false
	}
	return float64(b.Remaining) / float64(b.Limit), true
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
// The reading is logged at WARN when its bucket is first seen below
// lowBudgetShare in a window. It is logged again once the warned reading's
// reset has passed, or after a reading back above the threshold.
func RecordBudget(name Name, orgID, credential, bucket string, b Budget) {
	// The gauges that report the registry are created with the counters.
	current()
	if liveBudgets.Load().record(budgetKey{name, orgID, credential, bucket}, b, time.Now()) {
		log.Warn("rate-limit budget low",
			"upstream", name, "org", orgID, "credential", credential, "bucket", bucket,
			"remaining", b.Remaining, "limit", b.Limit, "reset", b.Reset)
	}
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
	// it was last at or above lowBudgetShare; warnedUntil is the reset of the
	// reading it fired for, past which a low reading is a new window's.
	warned      bool
	warnedUntil time.Time
}

type budgetRegistry struct {
	mu    sync.Mutex
	byKey map[budgetKey]*budgetEntry
}

func newBudgetRegistry() *budgetRegistry {
	return &budgetRegistry{byKey: map[budgetKey]*budgetEntry{}}
}

// record stores b under k and reports whether it warrants the low-budget WARN.
func (r *budgetRegistry) record(k budgetKey, b Budget, now time.Time) (warn bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.byKey[k]
	if !ok {
		e = &budgetEntry{}
		r.byKey[k] = e
	}
	e.Budget = b
	share, known := b.Share()
	switch {
	case !known || share >= lowBudgetShare:
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

// observe reports every current reading through the budget gauges. A reading
// that did not say its bucket's size reports no limit or ratio, and one that
// did not say its reset reports no time until it.
func (r *budgetRegistry) observe(o metric.Observer, g budgetGauges, now time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for k, e := range r.byKey {
		if !e.current(now) {
			continue
		}
		attrs := metric.WithAttributes(
			telemetry.Upstream(string(k.name)),
			telemetry.OrgID(k.orgID),
			telemetry.Credential(k.credential),
			telemetry.Bucket(k.bucket),
		)
		o.ObserveInt64(g.remaining, int64(e.Remaining), attrs)
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
	}, g.remaining, g.limit, g.ratio, g.untilReset); err != nil {
		log.Warn("rate-limit gauge callback registration failed", "error", err)
	}
}
