package upstream

import (
	"context"
	"sync"
	"sync/atomic"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"

	"github.com/sky-ai-eng/triage-factory/internal/logging"
	"github.com/sky-ai-eng/triage-factory/internal/telemetry"
)

var log = logging.Component("upstream")

// meterName is the instrumentation scope every instrument here is created
// under.
const meterName = "internal/upstream"

// instruments is the counter pair, created once per provider. upstream and
// outcome are closed vocabularies (Name, Class); org_id is not, so the series
// count grows with the number of orgs, by at most 30 per counter for each
// (6 names × 5 classes). That per-org breakdown is what the metrics are for.
type instruments struct {
	requests metric.Int64Counter
	retries  metric.Int64Counter
}

// newInstruments creates the counters against a provider. An
// instrument-creation error can only be a programmer error (an invalid name),
// and the API hands back a usable no-op beside it, so it is logged rather
// than propagated.
func newInstruments(provider metric.MeterProvider) *instruments {
	m := provider.Meter(meterName)
	in := &instruments{}
	mk := func(dst *metric.Int64Counter, name, desc string) {
		c, err := m.Int64Counter(name, metric.WithDescription(desc))
		if err != nil {
			log.Warn("upstream counter setup failed", "instrument", name, "error", err)
		}
		*dst = c
	}
	mk(&in.requests, "upstream.requests", "HTTP attempts against an upstream system, by outcome; a retried call counts once per attempt.")
	mk(&in.retries, "upstream.retries", "Decisions to retry a request, by the outcome that caused the retry.")
	return in
}

var (
	globalOnce sync.Once
	global     atomic.Pointer[instruments]
)

// current returns the process-wide counters, created on first use from the
// global meter provider. The global provider delegates to whichever provider
// telemetry.Init installs later, so a client used before Init still reports
// to the real exporter.
func current() *instruments {
	globalOnce.Do(func() { global.Store(newInstruments(otel.GetMeterProvider())) })
	return global.Load()
}

func attrs(name Name, orgID string, c Class) metric.AddOption {
	return metric.WithAttributes(
		telemetry.Upstream(string(name)),
		telemetry.Outcome(string(c)),
		telemetry.OrgID(orgID),
	)
}

// Record counts one HTTP attempt against name, made for orgID, that ended in
// c. orgID is empty for a request made for no org. If ctx carries a Tally,
// the attempt is added to it as well, and if it carries a progress report
// (WithProgress), that is called.
func Record(ctx context.Context, name Name, orgID string, c Class) {
	current().requests.Add(context.Background(), 1, attrs(name, orgID, c))
	if t := tallyFrom(ctx); t != nil {
		t.add(c)
	}
	if report := progressFrom(ctx); report != nil {
		report()
	}
}

type progressKey struct{}

// ReportProgress calls the progress report on ctx (WithProgress), if any, for
// a request that ended, answered or not, and that its client does not count
// with Record.
func ReportProgress(ctx context.Context) {
	if report := progressFrom(ctx); report != nil {
		report()
	}
}

// WithProgress returns a context under which every attempt Record counts
// also calls report, whatever its outcome. It is for a caller whose liveness
// is the requests it completes rather than how long its work takes: a poll
// cycle against a slow host is alive for as long as its requests keep
// finishing, however long the whole cycle runs. report must be cheap and safe
// for concurrent use.
func WithProgress(ctx context.Context, report func()) context.Context {
	return context.WithValue(ctx, progressKey{}, report)
}

func progressFrom(ctx context.Context) func() {
	if ctx == nil {
		return nil
	}
	report, _ := ctx.Value(progressKey{}).(func())
	return report
}

// RecordRetry counts one decision to retry a request against name; c is the
// class of the attempt that caused it.
func RecordRetry(ctx context.Context, name Name, orgID string, c Class) {
	current().retries.Add(context.Background(), 1, attrs(name, orgID, c))
}

// Tally is a count of the outcomes Record saw under one context, for a caller
// (a poll cycle) that needs to know what its own requests met rather than
// what the whole process did. It is safe for concurrent use, because a cycle
// fans its requests out across goroutines.
type Tally struct {
	mu       sync.Mutex
	attempts int
	byClass  map[Class]int
}

type tallyKey struct{}

// WithTally returns a context whose Record calls also count into the returned
// Tally. A Tally already on ctx is replaced for the derived context, so a
// nested scope counts only its own attempts.
func WithTally(ctx context.Context) (context.Context, *Tally) {
	t := &Tally{byClass: map[Class]int{}}
	return context.WithValue(ctx, tallyKey{}, t), t
}

func tallyFrom(ctx context.Context) *Tally {
	if ctx == nil {
		return nil
	}
	t, _ := ctx.Value(tallyKey{}).(*Tally)
	return t
}

func (t *Tally) add(c Class) {
	t.mu.Lock()
	t.attempts++
	t.byClass[c]++
	t.mu.Unlock()
}

// Attempts is the number of attempts recorded, of every class.
func (t *Tally) Attempts() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.attempts
}

// Count is the number of attempts recorded with class c.
func (t *Tally) Count(c Class) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.byClass[c]
}
