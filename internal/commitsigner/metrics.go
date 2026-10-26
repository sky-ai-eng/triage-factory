package commitsigner

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/sky-ai-eng/triage-factory/internal/db"
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
	pushes     metric.Int64Counter
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
	if in.pushes, err = m.Int64Counter("commit_signing.pushes",
		metric.WithDescription("Pushes from runs whose org resolves to a commit signer, by whether the pushed tip was signed.")); err != nil {
		signerLog.Warn("commit signing counter setup failed", "instrument", "commit_signing.pushes", "error", err)
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

// The checks tf_commit_signer_checks_total counts a result under.
const (
	// CheckVerify is the check an org admin runs with Verify: is the
	// signer's key on its account, and does the commit that broke it verify
	// now.
	CheckVerify = "verify"
	// CheckPeriodic is the poll cycle's check of the signer's key on its
	// account.
	CheckPeriodic = "periodic"
	// CheckPostPush is the poll cycle's read of a pushed commit's
	// verification.
	CheckPostPush = "post_push"
)

// The check results that are not a verification reason GitHub gave.
const (
	// CheckResultKeyMissing: the key was not on the account.
	CheckResultKeyMissing = domain.CommitSignerReasonKeyMissing
	// CheckResultError: GitHub could not be read.
	CheckResultError = "error"
	// checkResultOther stands in for any verification reason not named in
	// checkResults, so a reason GitHub adds cannot mint series.
	checkResultOther = "other"
)

// checkResults are the verification reasons a check result keeps as they are.
var checkResults = map[string]bool{
	domain.CommitVerificationValid:      true,
	domain.CommitVerificationUnknownKey: true,
	"no_user":                           true,
	"unverified_email":                  true,
	CheckResultKeyMissing:               true,
	CheckResultError:                    true,
}

// resultLabel is result as a check result's label value: other for a reason
// outside the ones the metrics name, so a reason GitHub adds cannot mint
// series.
func resultLabel(result string) string {
	if !checkResults[result] {
		return checkResultOther
	}
	return result
}

// checkStats owns the health-check counter. Package-level because a metric
// instrument is process-global by nature, and swapped atomically so a test can
// back it with a manual reader.
var checkStats atomic.Pointer[stats]

func init() {
	checkStats.Store(newStats(otel.GetMeterProvider()))
}

type stats struct {
	checks metric.Int64Counter
}

// newStats builds the instrument against mp — production passes the global
// provider, which telemetry.Init installs. An instrument-creation error can
// only be a programmer error, and the API hands back a usable no-op alongside
// it, so it is logged rather than propagated.
func newStats(mp metric.MeterProvider) *stats {
	counter, err := mp.Meter("internal/commitsigner").Int64Counter("commit_signer.checks",
		metric.WithDescription("Commit signer health checks, by check and result."))
	if err != nil {
		signerLog.Warn("commit signer check counter setup failed", "error", err)
	}
	return &stats{checks: counter}
}

// CountCheck counts one run of check that ended in result: GitHub's
// verification reason, CheckResultKeyMissing or CheckResultError. A reason
// outside the ones the metric names is counted as other. The periodic check
// counts valid for a key it found.
func CountCheck(ctx context.Context, check, result string) {
	s := checkStats.Load()
	if s == nil || s.checks == nil {
		return
	}
	s.checks.Add(ctx, 1, metric.WithAttributes(
		attribute.String("check", check), attribute.String("result", resultLabel(result))))
}

// HealthSource is the read the signer gauges sample.
// db.OrgCommitSignerStore satisfies it.
type HealthSource interface {
	ListHealthSystem(ctx context.Context) ([]db.SignerHealth, error)
}

// signerStatuses are the statuses tf_commit_signers reports, each on every
// sample, so a status no signer holds reads as zero rather than as no series.
var signerStatuses = []domain.CommitSignerStatus{domain.CommitSignerPending, domain.CommitSignerActive, domain.CommitSignerBroken}

// SignerGauges samples every org signer's status on a ticker and reports it at
// scrape time as tf_commit_signers{status}, the count of signers in each
// status, and tf_commit_signer_broken{org_id, reason}, 1 for each broken
// signer with the reason it broke for.
//
// It runs on the background brain, one process at a time, beside the checks
// that move the statuses, so each org's signer is reported once.
type SignerGauges struct {
	src HealthSource

	mu      sync.Mutex
	health  []db.SignerHealth
	sampled bool
	reg     metric.Registration
}

// NewSignerGauges creates the gauges against a provider and registers the
// callback that reports them. A provider that cannot build an instrument
// leaves them unregistered, and the ticks still run.
func NewSignerGauges(provider metric.MeterProvider, src HealthSource) *SignerGauges {
	g := &SignerGauges{src: src}
	m := provider.Meter(meterName)
	signers, err := m.Int64ObservableGauge("commit_signers",
		metric.WithDescription("Org commit signers by status: pending, active or broken."))
	if err != nil {
		signerLog.Error("commit signer gauge setup failed", "instrument", "commit_signers", "error", err)
		return g
	}
	broken, err := m.Int64ObservableGauge("commit_signer.broken",
		metric.WithDescription("1 for each broken org commit signer, with the reason it broke for."))
	if err != nil {
		signerLog.Error("commit signer gauge setup failed", "instrument", "commit_signer.broken", "error", err)
		return g
	}
	reg, err := m.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		g.mu.Lock()
		defer g.mu.Unlock()
		// Nothing before the first successful sample: zero signers this
		// process has not counted would read as an org with none.
		if !g.sampled {
			return nil
		}
		counts := map[domain.CommitSignerStatus]int64{}
		for _, h := range g.health {
			counts[h.Status]++
			if h.Status == domain.CommitSignerBroken {
				o.ObserveInt64(broken, 1, metric.WithAttributes(telemetry.OrgID(h.OrgID), telemetry.Reason(resultLabel(h.Reason))))
			}
		}
		for _, status := range signerStatuses {
			o.ObserveInt64(signers, counts[status], metric.WithAttributes(attribute.String("status", string(status))))
		}
		return nil
	}, signers, broken)
	if err != nil {
		signerLog.Error("commit signer gauge callback registration failed", "error", err)
		return g
	}
	g.reg = reg
	return g
}

// Tick samples once. A failed read keeps the previous sample.
func (g *SignerGauges) Tick(ctx context.Context) {
	health, err := g.src.ListHealthSystem(ctx)
	if err != nil {
		signerLog.Error("commit signer status read failed; keeping the previous values", "error", err)
		return
	}
	g.mu.Lock()
	g.health, g.sampled = health, true
	g.mu.Unlock()
}

// Close unregisters the gauges, so gauges the brain stopped on demotion stop
// reporting. Safe to call more than once.
func (g *SignerGauges) Close() {
	g.mu.Lock()
	reg := g.reg
	g.reg = nil
	g.mu.Unlock()
	if reg != nil {
		if err := reg.Unregister(); err != nil {
			signerLog.Error("commit signer gauge callback unregister failed", "error", err)
		}
	}
}

// Run samples now and then on every tick until ctx is cancelled, then
// unregisters the gauges.
func (g *SignerGauges) Run(ctx context.Context, interval time.Duration) {
	defer g.Close()
	g.Tick(ctx)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			g.Tick(ctx)
		}
	}
}

// TestT is the minimal slice of *testing.T that SetMeterProviderForTest needs,
// so the standard library's testing package stays out of production builds.
type TestT interface {
	Helper()
	Cleanup(func())
}

// SetMeterProviderForTest points the package's counters and histogram at
// provider for the duration of t and restores the previous ones via t.Cleanup.
// It lives in a non-_test.go file so the packages that run checks and record
// pushes can read what their tests recorded. It swaps process globals, so
// tests that call it must not run in parallel with each other.
func SetMeterProviderForTest(t TestT, provider metric.MeterProvider) {
	t.Helper()
	prev := checkStats.Swap(newStats(provider))
	current()
	prevInstruments := global.Swap(newInstruments(provider))
	t.Cleanup(func() {
		checkStats.Store(prev)
		global.Store(prevInstruments)
	})
}
