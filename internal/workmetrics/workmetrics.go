// Package workmetrics turns the work-item package's dispositions into
// OpenTelemetry metrics: one counter per disposition, keyed by kind and org,
// and per-kind depth gauges read from the table on a ticker.
//
// The workitem package reports every disposition it commits to a
// Kind.Observer and emits nothing itself; this package is the observer. A
// kind installs it where its Kind is declared (workkinds), so the store that
// adopts the table needs no wiring, and the depth observer runs once, on
// whichever process holds the background brain, over every registered kind.
package workmetrics

import (
	"context"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"

	"github.com/sky-ai-eng/triage-factory/internal/db/workitem"
	"github.com/sky-ai-eng/triage-factory/internal/logging"
	"github.com/sky-ai-eng/triage-factory/internal/telemetry"
)

var log = logging.Component("workmetrics")

// meterName is the instrumentation scope every instrument here is created
// under.
const meterName = "internal/workmetrics"

// Counters is the disposition counters, created once per provider. Every
// label value is a closed vocabulary: kind names, org ids, typed outcomes,
// the park reason constants, the verb names.
type Counters struct {
	claims        metric.Int64Counter
	reclaims      metric.Int64Counter
	completions   metric.Int64Counter
	requeues      metric.Int64Counter
	parks         metric.Int64Counter
	deferrals     metric.Int64Counter
	cancellations metric.Int64Counter
	leaseLost     metric.Int64Counter
	redrives      metric.Int64Counter
	supersedes    metric.Int64Counter
}

// NewCounters creates the counter set against a provider — the global one in
// production, a manual-reader one in tests. An instrument-creation error can
// only be a programmer error (an invalid name), and the API hands back a
// usable no-op beside it, so it is logged rather than propagated.
func NewCounters(provider metric.MeterProvider) *Counters {
	m := provider.Meter(meterName)
	c := &Counters{}
	mk := func(dst *metric.Int64Counter, name, desc string) {
		inst, err := m.Int64Counter(name, metric.WithDescription(desc))
		if err != nil {
			log.Warn("work counter setup failed", "instrument", name, "error", err)
		}
		*dst = inst
	}
	mk(&c.claims, "work.claims", "Work items leased, by kind and org.")
	mk(&c.reclaims, "work.reclaims", "Work items leased from an expired lease: interrupted units replayed.")
	mk(&c.completions, "work.completions", "Work items driven to done.")
	mk(&c.requeues, "work.requeues", "Failed attempts returned to ready, by typed outcome.")
	mk(&c.parks, "work.parks", "Work items taken out of circulation for an operator, by reason.")
	mk(&c.deferrals, "work.deferrals", "Attempts refunded for expected waiting.")
	mk(&c.cancellations, "work.cancellations", "Cancellation requests settled, at claim or by a holder.")
	mk(&c.leaseLost, "work.lease_lost", "Holder writes refused because the lease was lost, by verb.")
	mk(&c.redrives, "work.redrives", "Parked work items an operator returned to ready.")
	mk(&c.supersedes, "work.supersedes", "Parked work items an operator settled in favour of a replacement.")
	return c
}

var (
	globalOnce     sync.Once
	globalCounters *Counters
)

// global returns the process-wide counter set, created on first use from the
// global meter provider. The global provider delegates to whichever provider
// telemetry.Init installs later, so a kind declared at package init still
// reports to the real exporter.
func global() *Counters {
	globalOnce.Do(func() { globalCounters = NewCounters(otel.GetMeterProvider()) })
	return globalCounters
}

// Observe returns the workitem.Observer for one kind, reporting to the
// process-wide counters. It is what a Kind declaration installs.
func Observe(kind string) workitem.Observer {
	return &observer{kind: kind, counters: global}
}

// ObserveWith is Observe against a caller's counter set, for a test reading
// through a manual reader.
func ObserveWith(c *Counters, kind string) workitem.Observer {
	return &observer{kind: kind, counters: func() *Counters { return c }}
}

// observer is one kind's workitem.Observer. The counter set is resolved per
// call rather than captured, so a Kind declared before telemetry.Init still
// finds the instruments on its first disposition.
type observer struct {
	kind     string
	counters func() *Counters
}

func (o *observer) attrs(orgID string, extra ...metric.AddOption) []metric.AddOption {
	return append([]metric.AddOption{metric.WithAttributes(telemetry.WorkKind(o.kind), telemetry.OrgID(orgID))}, extra...)
}

func (o *observer) Claimed(orgID string, leased, reclaimed, _, _ int) {
	c := o.counters()
	if leased > 0 {
		c.claims.Add(context.Background(), int64(leased), o.attrs(orgID)...)
	}
	if reclaimed > 0 {
		c.reclaims.Add(context.Background(), int64(reclaimed), o.attrs(orgID)...)
	}
}

func (o *observer) Completed(orgID string) {
	o.counters().completions.Add(context.Background(), 1, o.attrs(orgID)...)
}

func (o *observer) Requeued(orgID string, outcome workitem.Outcome) {
	o.counters().requeues.Add(context.Background(), 1,
		o.attrs(orgID, metric.WithAttributes(telemetry.Outcome(string(outcome))))...)
}

func (o *observer) Parked(orgID, reason string) {
	o.counters().parks.Add(context.Background(), 1,
		o.attrs(orgID, metric.WithAttributes(telemetry.Reason(reason)))...)
}

func (o *observer) Deferred(orgID string) {
	o.counters().deferrals.Add(context.Background(), 1, o.attrs(orgID)...)
}

func (o *observer) Cancelled(orgID string) {
	o.counters().cancellations.Add(context.Background(), 1, o.attrs(orgID)...)
}

func (o *observer) LeaseLost(orgID, op string) {
	o.counters().leaseLost.Add(context.Background(), 1,
		o.attrs(orgID, metric.WithAttributes(telemetry.Op(op)))...)
}

func (o *observer) Redriven(orgID string) {
	o.counters().redrives.Add(context.Background(), 1, o.attrs(orgID)...)
}

func (o *observer) Superseded(orgID string) {
	o.counters().supersedes.Add(context.Background(), 1, o.attrs(orgID)...)
}

// DefaultDepthInterval is how often the depth observer measures every kind.
// Thirty seconds is fine for depths whose objective is a minute, and it keeps
// the aggregate off the scrape path: a scrape must not run a query over a
// production table, and two scrapers must not double the load.
const DefaultDepthInterval = 30 * time.Second

// DepthObserver measures every registered kind on a ticker and reports the
// result through observable gauges at scrape time. Observable instruments are
// read on collection, not on write, so each tick records into per-kind maps
// and the callback reports whatever is current.
type DepthObserver struct {
	kinds []DepthSource

	mu     sync.Mutex
	depths map[string]map[string]workitem.Depths // kind → org → depths
	reg    metric.Registration
}

// DepthSource is what the depth observer needs of a registered kind: the
// subset of db.WorkKindHandle it reads, which every handle satisfies. The
// narrower interface is what keeps this package off the store bundle: a Kind
// declaration installs Observe, and a Kind is declared below internal/db.
type DepthSource interface {
	Name() string
	Kind() workitem.Kind
	Conn() workitem.DBTX
	Objective() workitem.Objective
}

// NewDepthObserver creates the gauges against a provider and registers the
// callback that reports them. As with the counters, a provider that cannot
// build an instrument leaves the gauges unregistered and the ticks still run.
//
// The objective is a gauge per kind rather than a number in a runbook, so an
// alert rule compares two series instead of hard-coding one.
func NewDepthObserver(provider metric.MeterProvider, kinds []DepthSource) *DepthObserver {
	d := &DepthObserver{kinds: kinds, depths: map[string]map[string]workitem.Depths{}}
	m := provider.Meter(meterName)

	var (
		insts []metric.Observable
		fail  bool
	)
	gauge := func(name, desc string, unit string) metric.Int64ObservableGauge {
		opts := []metric.Int64ObservableGaugeOption{metric.WithDescription(desc)}
		if unit != "" {
			opts = append(opts, metric.WithUnit(unit))
		}
		g, err := m.Int64ObservableGauge(name, opts...)
		if err != nil {
			log.Error("work depth gauge setup failed", "instrument", name, "error", err)
			fail = true
		}
		insts = append(insts, g)
		return g
	}
	ready := gauge("work.ready", "Work items ready to claim now.", "")
	leased := gauge("work.leased", "Work items a holder is driving.", "")
	parked := gauge("work.parked", "Work items that will not run without a person.", "")
	deferred := gauge("work.deferred", "Ready work items whose retry time has not come.", "")
	readyAge := gauge("work.oldest_ready_age", "Age of the oldest ready work item, from its original enqueue.", "s")
	deferredAge := gauge("work.oldest_deferred_age", "Age of the oldest deferred work item, from its original enqueue.", "s")
	objective := gauge("work.oldest_ready_age_objective", "The kind's declared bound on oldest ready age.", "s")
	if fail {
		return d
	}

	reg, err := m.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		d.mu.Lock()
		defer d.mu.Unlock()
		for _, k := range d.kinds {
			kindAttr := metric.WithAttributes(telemetry.WorkKind(k.Name()))
			o.ObserveInt64(objective, int64(k.Objective().OldestReadyAge.Seconds()), kindAttr)
			for org, depths := range d.depths[k.Name()] {
				attrs := metric.WithAttributes(telemetry.WorkKind(k.Name()), telemetry.OrgID(org))
				o.ObserveInt64(ready, int64(depths.Ready), attrs)
				o.ObserveInt64(leased, int64(depths.Leased), attrs)
				o.ObserveInt64(parked, int64(depths.Parked), attrs)
				o.ObserveInt64(deferred, int64(depths.Deferred), attrs)
				o.ObserveInt64(readyAge, int64(depths.OldestReadyAge.Seconds()), attrs)
				o.ObserveInt64(deferredAge, int64(depths.OldestDeferredAge.Seconds()), attrs)
			}
		}
		return nil
	}, insts...)
	if err != nil {
		log.Error("work depth gauge callback registration failed", "error", err)
		return d
	}
	d.reg = reg
	return d
}

// Tick measures every kind once. An org absent from a kind's latest result is
// dropped, so a removed org's series stops rather than freezing at its last
// value; a read failure leaves that kind's previous values in place and logs,
// because a zero the pass did not establish is worse than a stale number.
func (d *DepthObserver) Tick(ctx context.Context) {
	for _, k := range d.kinds {
		if ctx.Err() != nil {
			return
		}
		byOrg, err := workitem.MeasureByOrg(ctx, k.Conn(), k.Kind())
		if err != nil {
			log.Error("work depth measure failed; keeping the previous values", "kind", k.Name(), "error", err)
			continue
		}
		d.mu.Lock()
		d.depths[k.Name()] = byOrg
		d.mu.Unlock()
	}
}

// Run ticks until ctx is cancelled. The first measure happens on the first
// tick rather than at start, so a freshly promoted brain does not run every
// kind's aggregate inside the promotion callback.
func (d *DepthObserver) Run(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			d.Tick(ctx)
		}
	}
}

// RunDepthObserver builds the depth observer on the global meter provider and
// runs it until ctx is cancelled. Started from the background brain, so in
// multi mode the lease holder reports and standbys do not; local mode runs it
// directly.
func RunDepthObserver(ctx context.Context, kinds []DepthSource, interval time.Duration) {
	NewDepthObserver(otel.GetMeterProvider(), kinds).Run(ctx, interval)
}
