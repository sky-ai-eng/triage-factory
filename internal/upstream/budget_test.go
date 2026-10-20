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

// TestBudgetRegistry_WarnsOnNearLimit: a reading that states only whether its
// bucket is near its limit warns as a low count does, on the first near
// reading in a window, again once that window's reset has passed, and again
// after a reading that is not near.
func TestBudgetRegistry_WarnsOnNearLimit(t *testing.T) {
	r := newBudgetRegistry()
	k := budgetKey{Jira, "org-1", "cloud_oauth", "quota"}
	now := time.Unix(1_000_000, 0)
	window1 := now.Add(time.Hour)
	window2 := now.Add(2 * time.Hour)

	steps := []struct {
		name string
		b    Budget
		at   time.Time
		warn bool
	}{
		{"not near", NearLimitReading(false, window1), now, false},
		{"near", NearLimitReading(true, window1), now, true},
		{"still near, same window", NearLimitReading(true, window1), now.Add(time.Minute), false},
		{"still near, after the window's reset", NearLimitReading(true, window2), window1, true},
		{"not near again", NearLimitReading(false, window2), window1.Add(time.Minute), false},
		{"near again, same window", NearLimitReading(true, window2), window1.Add(2 * time.Minute), true},
	}
	for _, s := range steps {
		if got := r.record(k, s.b, s.at); got != s.warn {
			t.Errorf("%s: warn = %v, want %v", s.name, got, s.warn)
		}
	}
}

// TestBudgetRegistry_QuietNeverWarns: a Quiet reading never warns, however
// often its short window resets while it stays low, and does not hold off the
// WARN for a reading that is not Quiet.
func TestBudgetRegistry_QuietNeverWarns(t *testing.T) {
	r := newBudgetRegistry()
	k := budgetKey{Jira, "org-1", "cloud_api_token", "burst"}
	now := time.Unix(1_000_000, 0)
	for i := range 5 {
		at := now.Add(time.Duration(i) * time.Second)
		if r.record(k, Budget{Remaining: 2, Limit: 100, Reset: at.Add(time.Second), Quiet: true}, at) {
			t.Errorf("second %d: a quiet reading warned", i)
		}
	}
	at := now.Add(5 * time.Second)
	if !r.record(k, Budget{Remaining: 2, Limit: 100, Reset: at.Add(time.Second)}, at) {
		t.Error("a low reading that is not quiet did not warn after quiet ones")
	}
}

// TestBudget_NearLimit: a near-limit reading says what the upstream said and
// nothing of what is left; a counted reading is near below 20% of a stated
// size, and says neither without one.
func TestBudget_NearLimit(t *testing.T) {
	for _, c := range []struct {
		name               string
		b                  Budget
		near, ok, stateRem bool
	}{
		{"stated near", NearLimitReading(true, time.Time{}), true, true, false},
		{"stated not near", NearLimitReading(false, time.Time{}), false, true, false},
		{"19% left", Budget{Remaining: 19, Limit: 100}, true, true, true},
		{"20% left", Budget{Remaining: 20, Limit: 100}, false, true, true},
		{"no size", Budget{Remaining: 0}, false, false, true},
	} {
		near, ok := c.b.NearLimit()
		if near != c.near || ok != c.ok {
			t.Errorf("%s: NearLimit = (%v, %v), want (%v, %v)", c.name, near, ok, c.near, c.ok)
		}
		if got := c.b.statesRemaining(); got != c.stateRem {
			t.Errorf("%s: statesRemaining = %v, want %v", c.name, got, c.stateRem)
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

// TestBudgetRegistry_ChangedReadings pins what a flusher is handed: each
// bucket recorded after its mark, once, with its latest reading, and the mark
// to pass next time. A reading past its reset is not handed over, and a mark
// that did not advance (a failed write) hands the same readings back.
func TestBudgetRegistry_ChangedReadings(t *testing.T) {
	r := newBudgetRegistry()
	now := time.Unix(1_000_000, 0)
	reset := now.Add(time.Hour)
	core := budgetKey{GitHub, "org-1", "installation:1", "core"}
	graphql := budgetKey{GitHub, "org-1", "installation:1", "graphql"}

	r.record(core, Budget{Remaining: 4000, Limit: 5000, Reset: reset}, now)
	r.record(graphql, Budget{Remaining: 10, Limit: 5000, Reset: reset}, now)
	r.record(core, Budget{Remaining: 3999, Limit: 5000, Reset: reset}, now.Add(time.Second))

	got, mark := r.changed(0, now.Add(time.Second))
	if len(got) != 2 {
		t.Fatalf("changed(0) = %d readings, want 2 (one per bucket): %+v", len(got), got)
	}
	for _, rd := range got {
		if rd.Bucket == "core" && (rd.Remaining == nil || *rd.Remaining != 3999) {
			t.Errorf("core reading = %+v, want the latest (3999)", rd)
		}
	}
	if again, _ := r.changed(0, now.Add(time.Second)); len(again) != 2 {
		t.Errorf("changed(0) after a write that did not advance the mark = %d readings, want 2", len(again))
	}

	if none, same := r.changed(mark, now.Add(time.Second)); len(none) != 0 || same != mark {
		t.Errorf("changed(mark) with nothing recorded since = (%+v, %d), want (none, %d)", none, same, mark)
	}

	r.record(graphql, Budget{Remaining: 9, Limit: 5000, Reset: reset}, now.Add(2*time.Second))
	got, next := r.changed(mark, now.Add(2*time.Second))
	if len(got) != 1 || got[0].Bucket != "graphql" || next <= mark {
		t.Errorf("changed(mark) after one record = (%+v, %d), want graphql alone and a later mark than %d", got, next, mark)
	}

	if past, _ := r.changed(0, reset); len(past) != 0 {
		t.Errorf("changed(0) at the reset = %+v, want none: the buckets have refilled", past)
	}
}

// TestBudgetEntry_Reading pins which fields a stored reading states: what
// the upstream said, and nil for what it did not, never a zero standing in
// for "not stated".
func TestBudgetEntry_Reading(t *testing.T) {
	observed := time.Unix(1_000_000, 0)
	reset := observed.Add(time.Minute)
	k := budgetKey{Jira, "org-1", "cloud_oauth", "quota"}

	stated := (&budgetEntry{Budget: Budget{Remaining: 0, Limit: 350, Reset: reset}, observed: observed}).reading(k)
	if stated.Remaining == nil || *stated.Remaining != 0 || stated.Limit == nil || *stated.Limit != 350 ||
		stated.Used != nil || stated.NearLimit != nil || stated.ResetAt == nil || !stated.ResetAt.Equal(reset) {
		t.Errorf("stated reading = %+v, want remaining 0 (not nil), limit 350, reset, no used or near_limit", stated)
	}
	if !stated.ObservedAt.Equal(observed) || stated.Upstream != "jira" || stated.Credential != "cloud_oauth" || stated.Bucket != "quota" {
		t.Errorf("stated reading key/observed = %+v", stated)
	}

	near := (&budgetEntry{Budget: NearLimitReading(true, time.Time{}), observed: observed}).reading(k)
	if near.NearLimit == nil || !*near.NearLimit || near.Remaining != nil || near.Limit != nil || near.ResetAt != nil {
		t.Errorf("near-limit reading = %+v, want near_limit true and nothing else stated", near)
	}

	counted := (&budgetEntry{Budget: Budget{Used: 3}, observed: observed}).reading(k)
	if counted.Used == nil || *counted.Used != 3 || counted.Remaining != nil {
		t.Errorf("counted reading with no published limit = %+v, want used 3 and no remaining", counted)
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
// uses for the rate-limit gauges, the refusal counter and the wait counter,
// through an exporter configured as production's is (see
// TestExportedNames_RealScrape), that a reading past its reset is not
// reported, that a counted reading reports what was used, and what is left
// only against a published limit, and that a near-limit reading reports
// near_limit and nothing of what is left.
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
	// Counted readings: one against a published limit, one with none.
	RecordBudget(Slack, "org-4", "bot:T1:A1", "chat.update", Budget{Used: 12, Remaining: 38, Limit: 50, Reset: time.Now().Add(time.Minute)})
	RecordBudget(Slack, "org-4", "bot:T1:A1", "chat.postMessage", Budget{Used: 3, Reset: time.Now().Add(time.Minute)})
	RecordRefused(Slack, "org-4", "bot:T1:A1", "chat.update")
	// Near-limit readings: near_limit and the reset, never a remaining.
	RecordBudget(Jira, "org-5", "cloud_oauth", "quota", NearLimitReading(true, time.Now().Add(time.Hour)))
	RecordBudget(Jira, "org-6", "cloud_oauth", "quota", NearLimitReading(false, time.Now().Add(time.Hour)))
	RecordRefused(Jira, "org-5", "cloud_oauth", "jira-quota-global-based")
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
		`tf_upstream_rate_limit_used{bucket="chat.update",credential="bot:T1:A1",org_id="org-4",upstream="slack"} 12`,
		`tf_upstream_rate_limit_remaining{bucket="chat.update",credential="bot:T1:A1",org_id="org-4",upstream="slack"} 38`,
		`tf_upstream_rate_limit_limit{bucket="chat.update",credential="bot:T1:A1",org_id="org-4",upstream="slack"} 50`,
		`tf_upstream_rate_limit_remaining_ratio{bucket="chat.update",credential="bot:T1:A1",org_id="org-4",upstream="slack"} 0.76`,
		`tf_upstream_rate_limit_used{bucket="chat.postMessage",credential="bot:T1:A1",org_id="org-4",upstream="slack"} 3`,
		`tf_upstream_rate_limit_refused_total{bucket="chat.update",credential="bot:T1:A1",org_id="org-4",upstream="slack"} 1`,
		`tf_upstream_rate_limit_near_limit{bucket="quota",credential="cloud_oauth",org_id="org-5",upstream="jira"} 1`,
		`tf_upstream_rate_limit_near_limit{bucket="quota",credential="cloud_oauth",org_id="org-6",upstream="jira"} 0`,
		`tf_upstream_rate_limit_refused_total{bucket="jira-quota-global-based",credential="cloud_oauth",org_id="org-5",upstream="jira"} 1`,
	}
	prefixed := []string{
		`tf_upstream_rate_limit_until_reset_seconds{bucket="graphql",credential="installation:42",org_id="org-1",upstream="github"} `,
		`tf_upstream_rate_limit_until_reset_seconds{bucket="chat.update",credential="bot:T1:A1",org_id="org-4",upstream="slack"} `,
		`tf_upstream_rate_limit_until_reset_seconds{bucket="chat.postMessage",credential="bot:T1:A1",org_id="org-4",upstream="slack"} `,
		`tf_upstream_rate_limit_until_reset_seconds{bucket="quota",credential="cloud_oauth",org_id="org-5",upstream="jira"} `,
		`tf_upstream_rate_limit_until_reset_seconds{bucket="quota",credential="cloud_oauth",org_id="org-6",upstream="jira"} `,
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
