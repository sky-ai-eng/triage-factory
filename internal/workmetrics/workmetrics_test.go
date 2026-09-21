package workmetrics

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/workitem"
	"github.com/sky-ai-eng/triage-factory/internal/db/workitemtest"
)

// collect reads every metric the reader holds, keyed by instrument name.
func collect(t *testing.T, reader *sdkmetric.ManualReader) map[string]metricdata.Metrics {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	out := map[string]metricdata.Metrics{}
	for _, scope := range rm.ScopeMetrics {
		for _, m := range scope.Metrics {
			out[m.Name] = m
		}
	}
	return out
}

// sumWith returns the counter's value at exactly the given attribute set,
// and whether such a point exists.
func sumWith(t *testing.T, ms map[string]metricdata.Metrics, name string, want ...attribute.KeyValue) (int64, bool) {
	t.Helper()
	m, ok := ms[name]
	if !ok {
		return 0, false
	}
	s, ok := m.Data.(metricdata.Sum[int64])
	if !ok {
		t.Fatalf("%s is %T, want an int64 sum", name, m.Data)
	}
	wantSet := attribute.NewSet(want...)
	for _, dp := range s.DataPoints {
		if dp.Attributes.Equals(&wantSet) {
			return dp.Value, true
		}
	}
	return 0, false
}

// gaugeWith returns the gauge's value at exactly the given attribute set.
func gaugeWith(t *testing.T, ms map[string]metricdata.Metrics, name string, want ...attribute.KeyValue) (int64, bool) {
	t.Helper()
	m, ok := ms[name]
	if !ok {
		return 0, false
	}
	g, ok := m.Data.(metricdata.Gauge[int64])
	if !ok {
		t.Fatalf("%s is %T, want an int64 gauge", name, m.Data)
	}
	wantSet := attribute.NewSet(want...)
	for _, dp := range g.DataPoints {
		if dp.Attributes.Equals(&wantSet) {
			return dp.Value, true
		}
	}
	return 0, false
}

func kindAttr(kind string) attribute.KeyValue { return attribute.String("work.kind", kind) }
func orgAttr(org string) attribute.KeyValue   { return attribute.String("org.id", org) }

// TestObserver_EveryDispositionCountsWithItsAttributes drives each observer
// method once and reads the counter it should have moved, with the exact
// label set: kind and org on every one, plus the outcome, reason or verb
// where the method carries one.
func TestObserver_EveryDispositionCountsWithItsAttributes(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	c := NewCounters(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	obs := ObserveWith(c, "fixture")
	const org = "org-1"

	obs.Claimed(org, 3, 1, 2, 1)
	obs.Completed(org)
	obs.Requeued(org, workitem.OutcomeTransient)
	obs.Requeued(org, workitem.OutcomeTransient)
	obs.Parked(org, "permanent")
	obs.Parked(org, workitem.ReasonBudgetExhausted)
	obs.Deferred(org)
	obs.Cancelled(org)
	obs.LeaseLost(org, workitem.OpRenew)
	obs.Redriven(org)
	obs.Superseded(org)

	ms := collect(t, reader)
	base := []attribute.KeyValue{kindAttr("fixture"), orgAttr(org)}
	cases := []struct {
		name  string
		attrs []attribute.KeyValue
		want  int64
	}{
		{"work.claims", base, 3},
		{"work.reclaims", base, 1},
		{"work.completions", base, 1},
		{"work.requeues", append(base, attribute.String("outcome", "transient")), 2},
		{"work.parks", append(base, attribute.String("reason", "permanent")), 1},
		{"work.parks", append(base, attribute.String("reason", "budget_exhausted")), 1},
		{"work.deferrals", base, 1},
		{"work.cancellations", base, 1},
		{"work.lease_lost", append(base, attribute.String("op", "renew")), 1},
		{"work.redrives", base, 1},
		{"work.supersedes", base, 1},
	}
	for _, tc := range cases {
		got, ok := sumWith(t, ms, tc.name, tc.attrs...)
		if !ok {
			t.Errorf("%s has no point at %v", tc.name, tc.attrs)
			continue
		}
		if got != tc.want {
			t.Errorf("%s at %v = %d, want %d", tc.name, tc.attrs, got, tc.want)
		}
	}

	// The settled counts on Claimed are the round's shape, not a second
	// count: the cancellations and parks it carried moved no counter of
	// their own beyond the explicit calls above.
	if got, _ := sumWith(t, ms, "work.cancellations", base...); got != 1 {
		t.Errorf("work.cancellations = %d, want the one explicit call, not Claimed's count", got)
	}
	if got, _ := sumWith(t, ms, "work.parks", append(base, attribute.String("reason", "budget_exhausted"))...); got != 1 {
		t.Errorf("work.parks{budget_exhausted} = %d, want the one explicit call", got)
	}

	// A round that leased nothing moves nothing.
	obs.Claimed(org, 0, 0, 1, 0)
	ms = collect(t, reader)
	if got, _ := sumWith(t, ms, "work.claims", base...); got != 3 {
		t.Errorf("work.claims after an empty round = %d, want 3", got)
	}
}

// depthSource is a DepthSource over the conformance suite's fixture table in
// an in-memory SQLite, so the observer's ticks read a real Measure.
type depthSource struct {
	name string
	kind workitem.Kind
	conn workitem.DBTX
	obj  time.Duration
}

func (d depthSource) Name() string                  { return d.name }
func (d depthSource) Kind() workitem.Kind           { return d.kind }
func (d depthSource) Conn() workitem.DBTX           { return d.conn }
func (d depthSource) Objective() workitem.Objective { return workitem.Objective{OldestReadyAge: d.obj} }

func newFixtureSource(t *testing.T) (*depthSource, *sql.DB) {
	t.Helper()
	conn, err := sql.Open("sqlite", db.TestDSNMemory)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	conn.SetMaxOpenConns(1)
	t.Cleanup(func() { conn.Close() })
	for _, stmt := range workitemtest.FixtureDDL(workitem.SQLite) {
		if _, err := conn.Exec(stmt); err != nil {
			t.Fatalf("fixture DDL: %v", err)
		}
	}
	kind := workitem.Kind{
		Table: workitemtest.FixtureTable, Dialect: workitem.SQLite,
		Unique: workitem.UniqueWhileUnsettled, Strategy: workitem.SingleTx,
		Columns: []string{"payload", "frozen_col"},
	}
	return &depthSource{name: "fixture", kind: kind, conn: conn, obj: 60 * time.Second}, conn
}

func admit(t *testing.T, conn *sql.DB, kind workitem.Kind, org, key string) int64 {
	t.Helper()
	id, _, err := workitem.Admit(context.Background(), conn, kind, org, key, map[string]any{"payload": "p", "frozen_col": 0})
	if err != nil {
		t.Fatalf("admit: %v", err)
	}
	return id
}

// TestDepthObserver_RecordsDropsAndKeeps covers the depth observer's whole
// contract: gauges after one tick, a vanished org's series dropped, values
// kept across a read failure, and the objective reported per kind.
func TestDepthObserver_RecordsDropsAndKeeps(t *testing.T) {
	src, conn := newFixtureSource(t)
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	d := NewDepthObserver(provider, []DepthSource{src})

	const orgA, orgB = "org-a", "org-b"
	admit(t, conn, src.kind, orgA, "a-1")
	admit(t, conn, src.kind, orgA, "a-2")
	parkedID := admit(t, conn, src.kind, orgB, "b-1")
	res, err := workitem.Claim(context.Background(), conn, src.kind, workitem.Owner{ID: "w", Epoch: 1}, orgB, 1)
	if err != nil || len(res.Claimed) != 1 {
		t.Fatalf("claim: %+v %v", res, err)
	}
	if err := workitem.Park(context.Background(), conn, src.kind, res.Claimed[0], "stuck"); err != nil {
		t.Fatalf("park: %v", err)
	}

	// Before the first tick nothing is recorded, but the objective is
	// already there: it is declared, not measured.
	ms := collect(t, reader)
	if _, ok := gaugeWith(t, ms, "work.ready", kindAttr("fixture"), orgAttr(orgA)); ok {
		t.Error("work.ready reported before any tick")
	}
	if got, ok := gaugeWith(t, ms, "work.oldest_ready_age_objective", kindAttr("fixture")); !ok || got != 60 {
		t.Errorf("objective gauge = %d (present=%v), want 60", got, ok)
	}

	d.Tick(context.Background())
	ms = collect(t, reader)
	if got, _ := gaugeWith(t, ms, "work.ready", kindAttr("fixture"), orgAttr(orgA)); got != 2 {
		t.Errorf("work.ready{org-a} = %d, want 2", got)
	}
	if got, _ := gaugeWith(t, ms, "work.parked", kindAttr("fixture"), orgAttr(orgB)); got != 1 {
		t.Errorf("work.parked{org-b} = %d, want 1", got)
	}
	if got, ok := gaugeWith(t, ms, "work.oldest_ready_age", kindAttr("fixture"), orgAttr(orgA)); !ok || got < 0 {
		t.Errorf("work.oldest_ready_age{org-a} = %d (present=%v), want a non-negative age", got, ok)
	}
	for _, name := range []string{"work.leased", "work.deferred", "work.oldest_deferred_age"} {
		if got, ok := gaugeWith(t, ms, name, kindAttr("fixture"), orgAttr(orgA)); !ok || got != 0 {
			t.Errorf("%s{org-a} = %d (present=%v), want 0", name, got, ok)
		}
	}

	// org-b's only row settles, so its series stops rather than freezing.
	if err := workitem.Supersede(context.Background(), conn, src.kind, orgB, parkedID, "operator", 999); err != nil {
		t.Fatalf("supersede: %v", err)
	}
	d.Tick(context.Background())
	ms = collect(t, reader)
	if _, ok := gaugeWith(t, ms, "work.parked", kindAttr("fixture"), orgAttr(orgB)); ok {
		t.Error("work.parked{org-b} still reported after the org's last unsettled row settled")
	}
	if got, _ := gaugeWith(t, ms, "work.ready", kindAttr("fixture"), orgAttr(orgA)); got != 2 {
		t.Errorf("work.ready{org-a} = %d after org-b vanished, want 2", got)
	}

	// A read failure keeps the previous values rather than zeroing them or
	// dropping the org.
	if _, err := conn.Exec("DROP TABLE " + workitemtest.FixtureTable); err != nil {
		t.Fatalf("drop table: %v", err)
	}
	d.Tick(context.Background())
	ms = collect(t, reader)
	if got, ok := gaugeWith(t, ms, "work.ready", kindAttr("fixture"), orgAttr(orgA)); !ok || got != 2 {
		t.Errorf("work.ready{org-a} = %d (present=%v) after a read failure, want the previous 2 kept", got, ok)
	}
}

// TestObserve_GlobalIsNilSafeBeforeInit pins that a Kind declared at package
// init can report before telemetry.Init installs a provider: the global
// no-op provider hands back usable instruments, so nothing panics.
func TestObserve_GlobalIsNilSafeBeforeInit(t *testing.T) {
	obs := Observe("fixture")
	obs.Claimed("org", 1, 0, 0, 0)
	obs.Completed("org")
	obs.LeaseLost("org", workitem.OpComplete)
}
