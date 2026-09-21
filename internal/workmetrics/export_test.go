package workmetrics

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/otlptranslator"
	otelprom "go.opentelemetry.io/otel/exporters/prometheus"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/sky-ai-eng/triage-factory/internal/db/workitem"
	"github.com/sky-ai-eng/triage-factory/internal/db/workitemtest"
)

// TestExportedNames_RealScrape pins the names the monitoring doc promises:
// every instrument here is pushed through a Prometheus exporter configured
// exactly as internal/telemetry configures production's — the tf namespace,
// no scope info, underscore escaping with unit and counter suffixes — and the
// scrape is asserted line by line. The doc's tables and alert rules are
// copied from what this prints, so a renamed instrument or a changed unit
// fails here before it silently breaks a rule.
func TestExportedNames_RealScrape(t *testing.T) {
	registry := prometheus.NewRegistry()
	exporter, err := otelprom.New(
		otelprom.WithRegisterer(registry),
		otelprom.WithNamespace("tf"),
		otelprom.WithoutScopeInfo(),
		otelprom.WithTranslationStrategy(otlptranslator.UnderscoreEscapingWithSuffixes),
	)
	if err != nil {
		t.Fatalf("exporter: %v", err)
	}
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(exporter))

	c := NewCounters(provider)
	obs := ObserveWith(c, "event_queue")
	const org = "org-1"
	obs.Claimed(org, 1, 1, 0, 0)
	obs.Completed(org)
	obs.Requeued(org, workitem.OutcomeTransient)
	obs.Parked(org, "transient")
	obs.Deferred(org)
	obs.Cancelled(org)
	obs.LeaseLost(org, workitem.OpRenew)
	obs.Redriven(org)
	obs.Superseded(org)

	src, conn := newFixtureSource(t)
	src.name = "event_queue"
	admit(t, conn, src.kind, org, "r1")
	d := NewDepthObserver(provider, []DepthSource{src})
	d.Tick(context.Background())

	rec := httptest.NewRecorder()
	promhttp.HandlerFor(registry, promhttp.HandlerOpts{}).ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	var lines []string
	for _, line := range strings.Split(rec.Body.String(), "\n") {
		if strings.HasPrefix(line, "tf_work_") {
			lines = append(lines, line)
		}
	}
	t.Logf("scrape:\n%s", strings.Join(lines, "\n"))

	want := []string{
		`tf_work_claims_total{org_id="org-1",work_kind="event_queue"} 1`,
		`tf_work_reclaims_total{org_id="org-1",work_kind="event_queue"} 1`,
		`tf_work_completions_total{org_id="org-1",work_kind="event_queue"} 1`,
		`tf_work_requeues_total{org_id="org-1",outcome="transient",work_kind="event_queue"} 1`,
		`tf_work_parks_total{org_id="org-1",reason="transient",work_kind="event_queue"} 1`,
		`tf_work_deferrals_total{org_id="org-1",work_kind="event_queue"} 1`,
		`tf_work_cancellations_total{org_id="org-1",work_kind="event_queue"} 1`,
		`tf_work_lease_lost_total{op="renew",org_id="org-1",work_kind="event_queue"} 1`,
		`tf_work_redrives_total{org_id="org-1",work_kind="event_queue"} 1`,
		`tf_work_supersedes_total{org_id="org-1",work_kind="event_queue"} 1`,
		`tf_work_ready{org_id="org-1",work_kind="event_queue"} 1`,
		`tf_work_leased{org_id="org-1",work_kind="event_queue"} 0`,
		`tf_work_parked{org_id="org-1",work_kind="event_queue"} 0`,
		`tf_work_deferred{org_id="org-1",work_kind="event_queue"} 0`,
		`tf_work_oldest_ready_age_seconds{org_id="org-1",work_kind="event_queue"} 0`,
		`tf_work_oldest_deferred_age_seconds{org_id="org-1",work_kind="event_queue"} 0`,
		`tf_work_oldest_ready_age_objective_seconds{work_kind="event_queue"} 60`,
	}
	// Exact set equality, both directions: a missing line is a renamed
	// instrument, an extra one is a duplicate series.
	got := map[string]int{}
	for _, line := range lines {
		got[line]++
	}
	for _, w := range want {
		switch got[w] {
		case 0:
			t.Errorf("scrape lacks %q", w)
		case 1:
		default:
			t.Errorf("scrape carries %q %d times", w, got[w])
		}
		delete(got, w)
	}
	for line := range got {
		t.Errorf("scrape carries unexpected %q", line)
	}
}

// TestDepthObserver_CloseStopsReporting is the lease-flap case: a stopped
// observer's callback must not keep reporting under a new observer's series,
// or an org that vanished from the live one would never drop.
func TestDepthObserver_CloseStopsReporting(t *testing.T) {
	src, conn := newFixtureSource(t)
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))

	admit(t, conn, src.kind, "org-a", "a-1")
	old := NewDepthObserver(provider, []DepthSource{src})
	old.Tick(context.Background())
	if got, ok := gaugeWith(t, collect(t, reader), "work.ready", kindAttr("fixture"), orgAttr("org-a")); !ok || got != 1 {
		t.Fatalf("work.ready{org-a} = %d (present=%v) before the flap, want 1", got, ok)
	}

	// Demotion: the old observer's Run returns and closes it. Re-acquisition
	// starts a new one, which measures a table where org-a has settled.
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { old.Run(ctx, time.Hour); close(done) }()
	cancel()
	<-done

	if _, err := conn.Exec("UPDATE " + workitemtest.FixtureTable + " SET status = 'done'"); err != nil {
		t.Fatalf("settle: %v", err)
	}
	fresh := NewDepthObserver(provider, []DepthSource{src})
	defer fresh.Close()
	fresh.Tick(context.Background())

	ms := collect(t, reader)
	if _, ok := gaugeWith(t, ms, "work.ready", kindAttr("fixture"), orgAttr("org-a")); ok {
		t.Error("work.ready{org-a} still reported after the observer that saw it was closed")
	}
	// The objective is reported exactly once, by the live observer.
	m := ms["work.oldest_ready_age_objective"]
	g, _ := m.Data.(metricdata.Gauge[int64])
	if len(g.DataPoints) != 1 {
		t.Errorf("objective gauge has %d points after a flap, want 1", len(g.DataPoints))
	}
}

// TestDepthObserver_CloseWhileRunningStopsReporting is the same-pod flap as
// the brain drives it: demotion closes the observer before its goroutine has
// noticed the cancellation, and re-acquisition registers a new one while the
// old goroutine is still on its way out. The old callback must already be
// gone when Close returns, so the two never report the same series together.
func TestDepthObserver_CloseWhileRunningStopsReporting(t *testing.T) {
	src, conn := newFixtureSource(t)
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))

	admit(t, conn, src.kind, "org-a", "a-1")
	old := NewDepthObserver(provider, []DepthSource{src})
	old.Tick(context.Background())

	// The old goroutine keeps running: nothing cancels it until the end.
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { old.Run(ctx, time.Hour); close(done) }()
	old.Close()

	fresh := NewDepthObserver(provider, []DepthSource{src})
	defer fresh.Close()
	fresh.Tick(context.Background())

	ms := collect(t, reader)
	m := ms["work.oldest_ready_age_objective"]
	g, _ := m.Data.(metricdata.Gauge[int64])
	if len(g.DataPoints) != 1 {
		t.Errorf("objective gauge has %d points with the old observer closed but still running, want 1", len(g.DataPoints))
	}
	m = ms["work.ready"]
	g, _ = m.Data.(metricdata.Gauge[int64])
	if len(g.DataPoints) != 1 {
		t.Errorf("work.ready has %d points with the old observer closed but still running, want 1", len(g.DataPoints))
	}

	cancel()
	<-done
}
