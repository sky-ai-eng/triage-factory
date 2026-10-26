package commitsigner

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/telemetry"
)

// meterName is the instrumentation scope every instrument here is created
// under.
const meterName = "internal/commitsigner"

// modeControlPlane is the duration histogram's mode label for a signature a
// control pod made for a sandboxed run's digest.
const modeControlPlane = "control_plane"

// durationBounds are the duration histogram's buckets, in seconds: from a
// signature made in the process that asked (a few milliseconds) to the
// request deadline, past which nothing is observed.
var durationBounds = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30}

// instruments are the signing counters and the duration histogram. signer,
// outcome, reason and mode are closed vocabularies; org_id grows with the
// orgs that sign, by at most a dozen series each.
type instruments struct {
	signatures metric.Int64Counter
	duration   metric.Float64Histogram
	swept      metric.Int64Counter
}

// newInstruments creates the instruments against a provider. A creation error
// can only be an invalid name, and the API hands back a usable no-op beside
// it, so it is logged rather than propagated.
func newInstruments(provider metric.MeterProvider) *instruments {
	m := provider.Meter(meterName)
	in := &instruments{}
	var err error
	if in.signatures, err = m.Int64Counter("commit_signatures",
		metric.WithDescription("Commit signature requests by the result the run got: signed, refused (with the reason) or unanswered.")); err != nil {
		signerLog.Warn("commit signing counter setup failed", "instrument", "commit_signatures", "error", err)
	}
	if in.duration, err = m.Float64Histogram("commit_signing.duration", metric.WithUnit("s"),
		metric.WithDescription("From a run's signature request to the signature in hand."),
		metric.WithExplicitBucketBoundaries(durationBounds...)); err != nil {
		signerLog.Warn("commit signing histogram setup failed", "instrument", "commit_signing.duration", "error", err)
	}
	if in.swept, err = m.Int64Counter("commit_signing.swept",
		metric.WithDescription("Signature requests a control pod found by its once-a-second sweep rather than by the sign_request doorbell.")); err != nil {
		signerLog.Warn("commit signing counter setup failed", "instrument", "commit_signing.swept", "error", err)
	}
	return in
}

var (
	globalOnce sync.Once
	global     atomic.Pointer[instruments]
)

// current returns the process-wide instruments, created on first use from the
// global meter provider, which delegates to whichever provider telemetry.Init
// installs.
func current() *instruments {
	globalOnce.Do(func() { global.Store(newInstruments(otel.GetMeterProvider())) })
	return global.Load()
}

// countRequest counts one request's result. reason is empty unless the result
// is a refusal.
func countRequest(signer domain.CommitSigningMode, orgID string, outcome Outcome, reason Refusal) {
	label := ""
	if outcome == OutcomeRefused {
		label = reason.metricReason()
	}
	current().signatures.Add(context.Background(), 1, metric.WithAttributes(
		telemetry.Signer(string(signer)),
		telemetry.Outcome(string(outcome)),
		telemetry.Reason(label),
		telemetry.OrgID(orgID),
	))
}

// observeDuration records how long a signature took to reach the run that
// asked for it, made where mode says.
func observeDuration(signer domain.CommitSigningMode, mode string, d time.Duration) {
	current().duration.Record(context.Background(), d.Seconds(), metric.WithAttributes(
		telemetry.Signer(string(signer)),
		telemetry.SigningMode(mode),
	))
}

// RegisterLimitGauge reports the configured budgets as tf_commit_signing_limit,
// one series per budget, so an alert rule compares two series rather than
// hard-coding a number. Called once by a process that enforces them.
func RegisterLimitGauge(provider metric.MeterProvider, b Budgets) {
	m := provider.Meter(meterName)
	limit, err := m.Int64ObservableGauge("commit_signing.limit",
		metric.WithDescription("The configured commit signing budgets: signatures per org per minute, and per claim."))
	if err != nil {
		signerLog.Error("commit signing gauge setup failed", "instrument", "commit_signing.limit", "error", err)
		return
	}
	if _, err := m.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		o.ObserveInt64(limit, int64(b.PerOrgMinute), metric.WithAttributes(telemetry.Budget("org_per_minute")))
		o.ObserveInt64(limit, int64(b.PerClaim), metric.WithAttributes(telemetry.Budget("per_claim")))
		return nil
	}, limit); err != nil {
		signerLog.Error("commit signing gauge callback registration failed", "error", err)
	}
}

// PendingSource is the read the queue observer samples.
// db.CommitSignatureStore satisfies it.
type PendingSource interface {
	PendingRequestsSystem(ctx context.Context) (count int, oldestAge time.Duration, err error)
}

// QueueObserver samples the signing queue on a ticker and reports it at
// scrape time as tf_commit_signing_requests_pending and
// tf_commit_signing_oldest_pending_age_seconds.
//
// It runs on the background brain, one pod at a time, rather than inside the
// signers: a queue every signer has stopped draining is the thing it reports,
// so the signers must not be what reports it.
type QueueObserver struct {
	src PendingSource

	mu      sync.Mutex
	count   int
	oldest  time.Duration
	sampled bool
	reg     metric.Registration
}

// NewQueueObserver creates the gauges against a provider and registers the
// callback that reports them. A provider that cannot build an instrument
// leaves them unregistered, and the ticks still run.
func NewQueueObserver(provider metric.MeterProvider, src PendingSource) *QueueObserver {
	q := &QueueObserver{src: src}
	m := provider.Meter(meterName)
	pending, err := m.Int64ObservableGauge("commit_signing.requests_pending",
		metric.WithDescription("Commit signature requests no control pod has claimed. Zero is the steady state."))
	if err != nil {
		signerLog.Error("commit signing gauge setup failed", "instrument", "commit_signing.requests_pending", "error", err)
		return q
	}
	oldest, err := m.Int64ObservableGauge("commit_signing.oldest_pending_age", metric.WithUnit("s"),
		metric.WithDescription("Seconds the oldest unclaimed commit signature request has waited."))
	if err != nil {
		signerLog.Error("commit signing gauge setup failed", "instrument", "commit_signing.oldest_pending_age", "error", err)
		return q
	}
	reg, err := m.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		q.mu.Lock()
		defer q.mu.Unlock()
		// Nothing before the first successful sample: a zero this process
		// has not measured would read as a healthy queue.
		if q.sampled {
			o.ObserveInt64(pending, int64(q.count))
			o.ObserveInt64(oldest, int64(q.oldest.Seconds()))
		}
		return nil
	}, pending, oldest)
	if err != nil {
		signerLog.Error("commit signing gauge callback registration failed", "error", err)
		return q
	}
	q.reg = reg
	return q
}

// Tick samples once. A failed read keeps the previous values: a zero the
// sample did not establish is worse than a stale number.
func (q *QueueObserver) Tick(ctx context.Context) {
	count, oldest, err := q.src.PendingRequestsSystem(ctx)
	if err != nil {
		signerLog.Error("commit signing queue measure failed; keeping the previous values", "error", err)
		return
	}
	q.mu.Lock()
	q.count, q.oldest, q.sampled = count, oldest, true
	q.mu.Unlock()
}

// Close unregisters the gauges, so an observer the brain stopped on demotion
// stops reporting. Safe to call more than once.
func (q *QueueObserver) Close() {
	q.mu.Lock()
	reg := q.reg
	q.reg = nil
	q.mu.Unlock()
	if reg != nil {
		if err := reg.Unregister(); err != nil {
			signerLog.Error("commit signing gauge callback unregister failed", "error", err)
		}
	}
}

// Run ticks until ctx is cancelled, then unregisters the gauges.
func (q *QueueObserver) Run(ctx context.Context, interval time.Duration) {
	defer q.Close()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			q.Tick(ctx)
		}
	}
}
