package poller

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/otlptranslator"
	otelprom "go.opentelemetry.io/otel/exporters/prometheus"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"

	dbpkg "github.com/sky-ai-eng/triage-factory/internal/db"
)

// fakeConnectionSource is the store read as the connection observer sees it.
type fakeConnectionSource struct {
	statuses []dbpkg.ConnectionStatus
	err      error
}

func (f *fakeConnectionSource) ListConnectionStatuses(context.Context) ([]dbpkg.ConnectionStatus, error) {
	return f.statuses, f.err
}

// scrapeUpstreamUp pushes the observer's gauge through a Prometheus exporter
// configured exactly as internal/telemetry configures production's and returns
// the tf_upstream_up lines, so the name the docs, the dashboard and the alert
// rule use is the name asserted on.
func scrapeUpstreamUp(t *testing.T, registry *prometheus.Registry) []string {
	t.Helper()
	rec := httptest.NewRecorder()
	promhttp.HandlerFor(registry, promhttp.HandlerOpts{}).ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	var lines []string
	for _, line := range strings.Split(rec.Body.String(), "\n") {
		if strings.HasPrefix(line, "tf_upstream_up") {
			lines = append(lines, line)
		}
	}
	return lines
}

func newScrapedProvider(t *testing.T) (*sdkmetric.MeterProvider, *prometheus.Registry) {
	t.Helper()
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
	return sdkmetric.NewMeterProvider(sdkmetric.WithReader(exporter)), registry
}

func assertLines(t *testing.T, got, want []string) {
	t.Helper()
	seen := map[string]int{}
	for _, line := range got {
		seen[line]++
	}
	for _, w := range want {
		if seen[w] != 1 {
			t.Errorf("scrape carries %q %d times, want once\nscrape:\n%s", w, seen[w], strings.Join(got, "\n"))
		}
		delete(seen, w)
	}
	for line := range seen {
		t.Errorf("scrape carries unexpected %q", line)
	}
}

// TestConnectionObserver_ExportsTheStoredState: a down org reports 0, an up
// org 1, and an org whose state is unknown has no series at all.
func TestConnectionObserver_ExportsTheStoredState(t *testing.T) {
	provider, registry := newScrapedProvider(t)
	src := &fakeConnectionSource{statuses: []dbpkg.ConnectionStatus{
		{OrgID: "org-1", Source: "github", State: dbpkg.ConnectionDown, FailureClass: "transient"},
		{OrgID: "org-1", Source: "jira", State: dbpkg.ConnectionUp},
		{OrgID: "org-2", Source: "github", State: dbpkg.ConnectionUnknown},
	}}
	c := NewConnectionObserver(provider, src)
	defer c.Close()

	if lines := scrapeUpstreamUp(t, registry); len(lines) != 0 {
		t.Errorf("reported before any read: %v", lines)
	}

	c.Tick(context.Background())
	assertLines(t, scrapeUpstreamUp(t, registry), []string{
		`tf_upstream_up{org_id="org-1",upstream="github"} 0`,
		`tf_upstream_up{org_id="org-1",upstream="jira"} 1`,
	})
}

// TestConnectionObserver_DropsVanishedOrgsAndKeepsValuesOnAFailedRead: each
// read replaces the last, so an org it no longer lists stops reporting; a read
// that fails keeps the previous values rather than reporting nothing.
func TestConnectionObserver_DropsVanishedOrgsAndKeepsValuesOnAFailedRead(t *testing.T) {
	provider, registry := newScrapedProvider(t)
	src := &fakeConnectionSource{statuses: []dbpkg.ConnectionStatus{
		{OrgID: "org-1", Source: "github", State: dbpkg.ConnectionDown},
		{OrgID: "org-2", Source: "github", State: dbpkg.ConnectionUp},
	}}
	c := NewConnectionObserver(provider, src)
	defer c.Close()
	c.Tick(context.Background())

	src.statuses = src.statuses[1:]
	c.Tick(context.Background())
	assertLines(t, scrapeUpstreamUp(t, registry), []string{`tf_upstream_up{org_id="org-2",upstream="github"} 1`})

	src.statuses, src.err = nil, errors.New("database unreachable")
	c.Tick(context.Background())
	assertLines(t, scrapeUpstreamUp(t, registry), []string{`tf_upstream_up{org_id="org-2",upstream="github"} 1`})
}

// TestConnectionObserver_CloseStopsReporting is the lease-flap case: a
// demoted brain's observer must stop reporting before the next one starts, or
// the two would report the same series.
func TestConnectionObserver_CloseStopsReporting(t *testing.T) {
	provider, registry := newScrapedProvider(t)
	src := &fakeConnectionSource{statuses: []dbpkg.ConnectionStatus{
		{OrgID: "org-1", Source: "github", State: dbpkg.ConnectionDown},
	}}
	c := NewConnectionObserver(provider, src)
	c.Tick(context.Background())
	c.Close()
	if lines := scrapeUpstreamUp(t, registry); len(lines) != 0 {
		t.Errorf("a closed observer still reports: %v", lines)
	}
}
