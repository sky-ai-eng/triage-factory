package workmetrics

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/otlptranslator"
	otelprom "go.opentelemetry.io/otel/exporters/prometheus"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"

	"github.com/sky-ai-eng/triage-factory/internal/db/workitem"
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
	for _, w := range want {
		found := false
		for _, line := range lines {
			if line == w {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("scrape lacks %q", w)
		}
	}
}
