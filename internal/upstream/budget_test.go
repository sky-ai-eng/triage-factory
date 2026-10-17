package upstream

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
)

// TestRecordBudget_KeyedByCredentialAndBucket pins the registry's key: the
// latest reading per (upstream, org, credential, bucket). Two installations
// in one org, and two buckets of one installation, never overwrite each
// other.
func TestRecordBudget_KeyedByCredentialAndBucket(t *testing.T) {
	ResetBudgetsForTest(t)
	reset := time.Now().Add(time.Hour)
	RecordBudget(GitHub, "org-1", "installation:1", "core", Budget{Remaining: 4000, Limit: 5000, Reset: reset})
	RecordBudget(GitHub, "org-1", "installation:2", "core", Budget{Remaining: 12, Limit: 5000, Reset: reset})
	RecordBudget(GitHub, "org-1", "installation:1", "graphql", Budget{Remaining: 10, Limit: 5000, Reset: reset})
	RecordBudget(GitHub, "org-1", "installation:1", "core", Budget{Remaining: 3999, Limit: 5000, Reset: reset})

	for _, c := range []struct {
		org, credential, bucket string
		want                    int
	}{
		{"org-1", "installation:1", "core", 3999},
		{"org-1", "installation:2", "core", 12},
		{"org-1", "installation:1", "graphql", 10},
	} {
		if b, ok := BudgetForTest(GitHub, c.org, c.credential, c.bucket); !ok || b.Remaining != c.want {
			t.Errorf("%s %s %s = (%+v, %v), want %d", c.org, c.credential, c.bucket, b, ok, c.want)
		}
	}
	if b, ok := BudgetForTest(GitHub, "org-2", "installation:1", "core"); ok {
		t.Errorf("org-2 read org-1's reading %+v", b)
	}
	if b, ok := BudgetForTest(Linear, "org-1", "installation:1", "core"); ok {
		t.Errorf("linear read github's reading %+v", b)
	}
}

// TestBudgetRegistry_WarnsOncePerWindow pins when the low-budget WARN fires:
// the first reading below 20% in a window, again once that window's reset has
// passed, and again after the bucket climbs back above the threshold. A
// reading that does not state its size never warns.
func TestBudgetRegistry_WarnsOncePerWindow(t *testing.T) {
	r := newBudgetRegistry()
	k := budgetKey{GitHub, "org-1", "pat", "core"}
	now := time.Unix(1_000_000, 0)
	window1 := now.Add(time.Hour)
	window2 := now.Add(2 * time.Hour)
	reading := func(remaining int, reset time.Time) Budget {
		return Budget{Remaining: remaining, Limit: 5000, Reset: reset}
	}

	steps := []struct {
		name string
		b    Budget
		at   time.Time
		warn bool
	}{
		{"healthy", reading(4000, window1), now, false},
		{"first drop below 20%", reading(999, window1), now, true},
		{"still low, same window", reading(500, window1), now.Add(time.Minute), false},
		{"still low, after the window's reset", reading(400, window2), window1, true},
		{"back above", reading(4999, window2), window1.Add(time.Minute), false},
		{"low again, same window", reading(10, window2), window1.Add(2 * time.Minute), true},
		{"no limit stated", Budget{Remaining: 0, Reset: window2}, window1.Add(3 * time.Minute), false},
		{"low after an unsized reading", reading(10, window2), window1.Add(4 * time.Minute), true},
	}
	for _, s := range steps {
		if got := r.record(k, s.b, s.at); got != s.warn {
			t.Errorf("%s: warn = %v, want %v", s.name, got, s.warn)
		}
	}
}

// TestBudgetRegistry_DropsStaleEntries pins eviction: record's sweep drops an
// entry once its reading is past its reset and its WARN, if any, is for a
// window that has ended. A reading with no reset never goes stale, and neither
// does a WARN fired for one, since only a reading back above the threshold
// clears it.
func TestBudgetRegistry_DropsStaleEntries(t *testing.T) {
	r := newBudgetRegistry()
	t0 := time.Unix(1_000_000, 0)
	key := func(credential string) budgetKey { return budgetKey{GitHub, "org-1", credential, "core"} }

	r.record(key("installation:1"), Budget{Remaining: 4000, Limit: 5000, Reset: t0.Add(time.Minute)}, t0)
	r.record(key("installation:2"), Budget{Remaining: 10, Limit: 5000, Reset: t0.Add(time.Minute)}, t0) // warned
	r.record(key("installation:3"), Budget{Remaining: 4000, Limit: 5000}, t0)                           // no reset
	r.record(key("installation:4"), Budget{Remaining: 10, Limit: 5000}, t0)                             // warned, no reset
	// A later reading of installation:4 states a reset, which then passes; the
	// WARN it carries came from a reading with none, so it stays.
	r.record(key("installation:4"), Budget{Remaining: 9, Limit: 5000, Reset: t0.Add(time.Minute)}, t0)

	// Any record past sweepEvery sweeps the whole registry.
	r.record(key("installation:5"), Budget{Remaining: 4000, Limit: 5000, Reset: t0.Add(time.Hour)}, t0.Add(2*time.Minute))

	for credential, want := range map[string]bool{
		"installation:1": false,
		"installation:2": false,
		"installation:3": true,
		"installation:4": true,
		"installation:5": true,
	} {
		if _, got := r.get(key(credential)); got != want {
			t.Errorf("%s kept = %v, want %v", credential, got, want)
		}
	}
}

// TestWaitOnLimit_HonorsContext: the wait ends with the caller's ctx, like
// Sleep, and a non-positive wait returns at once.
func TestWaitOnLimit_HonorsContext(t *testing.T) {
	if err := WaitOnLimit(context.Background(), GitHub, "org-1", 0); err != nil {
		t.Errorf("zero wait: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	if err := WaitOnLimit(ctx, GitHub, "org-1", time.Minute); err == nil {
		t.Error("a cancelled wait returned nil")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("a cancelled wait took %v", elapsed)
	}
}

// TestBudgetMetrics_RealScrape pins the names and labels the monitoring doc
// uses for the rate-limit gauges and the wait counter, through an exporter
// configured as production's is (see TestExportedNames_RealScrape), and that
// a reading past its reset is not reported.
func TestBudgetMetrics_RealScrape(t *testing.T) {
	ResetBudgetsForTest(t)
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
	SetMeterProviderForTest(t, sdkmetric.NewMeterProvider(sdkmetric.WithReader(exporter)))

	RecordBudget(GitHub, "org-1", "installation:42", "graphql", Budget{Remaining: 1250, Limit: 5000, Reset: time.Now().Add(time.Hour)})
	// No limit or reset stated: remaining is all it reports.
	RecordBudget(GitHub, "org-2", "pat", "core", Budget{Remaining: 7})
	// Its window has reset since, so the bucket has refilled: not reported.
	RecordBudget(GitHub, "org-3", "pat", "core", Budget{Remaining: 3, Limit: 5000, Reset: time.Now().Add(-time.Minute)})
	if err := WaitOnLimit(context.Background(), GitHub, "org-1", time.Millisecond); err != nil {
		t.Fatalf("WaitOnLimit: %v", err)
	}

	rec := httptest.NewRecorder()
	promhttp.HandlerFor(registry, promhttp.HandlerOpts{}).ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	var lines []string
	for _, line := range strings.Split(rec.Body.String(), "\n") {
		if strings.HasPrefix(line, "tf_upstream_rate_limit_") {
			lines = append(lines, line)
		}
	}
	t.Logf("scrape:\n%s", strings.Join(lines, "\n"))

	// Exact lines where the value is fixed; a prefix where it depends on the
	// clock.
	exact := []string{
		`tf_upstream_rate_limit_remaining{bucket="graphql",credential="installation:42",org_id="org-1",upstream="github"} 1250`,
		`tf_upstream_rate_limit_limit{bucket="graphql",credential="installation:42",org_id="org-1",upstream="github"} 5000`,
		`tf_upstream_rate_limit_remaining_ratio{bucket="graphql",credential="installation:42",org_id="org-1",upstream="github"} 0.25`,
		`tf_upstream_rate_limit_remaining{bucket="core",credential="pat",org_id="org-2",upstream="github"} 7`,
	}
	prefixed := []string{
		`tf_upstream_rate_limit_until_reset_seconds{bucket="graphql",credential="installation:42",org_id="org-1",upstream="github"} `,
		`tf_upstream_rate_limit_wait_seconds_total{org_id="org-1",upstream="github"} `,
	}
	seen := map[string]bool{}
	for _, line := range lines {
		matched := false
		for _, w := range exact {
			if line == w {
				matched, seen[w] = true, true
			}
		}
		for _, p := range prefixed {
			if strings.HasPrefix(line, p) {
				matched, seen[p] = true, true
			}
		}
		if !matched {
			t.Errorf("scrape carries unexpected %q", line)
		}
	}
	for _, w := range append(exact, prefixed...) {
		if !seen[w] {
			t.Errorf("scrape lacks %q", w)
		}
	}
	if b, ok := BudgetForTest(GitHub, "org-3", "pat", "core"); ok {
		t.Errorf("the scrape kept the expired reading %+v; observe drops what it skips", b)
	}
}
