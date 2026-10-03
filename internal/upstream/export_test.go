package upstream

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
)

// TestExportedNames_RealScrape pins the names the monitoring doc, the bundled
// dashboard and the alert rule use: the counters are pushed through a
// Prometheus exporter configured exactly as internal/telemetry configures
// production's, and the scrape is asserted line by line, so a renamed
// instrument or label fails here before it silently breaks a rule.
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

	SetMeterProviderForTest(t, provider)

	ctx := context.Background()
	Record(ctx, GitHub, "org-1", Transient)
	Record(ctx, GitHub, "org-1", Transient)
	Record(ctx, GitHub, "org-1", OK)
	Record(ctx, Jira, "", Auth)
	RecordRetry(ctx, GitHub, "org-1", Transient)
	RecordRetry(ctx, Slack, "org-2", RateLimited)

	rec := httptest.NewRecorder()
	promhttp.HandlerFor(registry, promhttp.HandlerOpts{}).ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	var lines []string
	for _, line := range strings.Split(rec.Body.String(), "\n") {
		if strings.HasPrefix(line, "tf_upstream_") {
			lines = append(lines, line)
		}
	}
	t.Logf("scrape:\n%s", strings.Join(lines, "\n"))

	want := []string{
		`tf_upstream_requests_total{org_id="org-1",outcome="transient",upstream="github"} 2`,
		`tf_upstream_requests_total{org_id="org-1",outcome="ok",upstream="github"} 1`,
		`tf_upstream_requests_total{org_id="",outcome="auth",upstream="jira"} 1`,
		`tf_upstream_retries_total{org_id="org-1",outcome="transient",upstream="github"} 1`,
		`tf_upstream_retries_total{org_id="org-2",outcome="rate_limited",upstream="slack"} 1`,
	}
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
