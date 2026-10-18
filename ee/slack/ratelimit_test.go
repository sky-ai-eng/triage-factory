package slack

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"regexp"
	"sort"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/sky-ai-eng/triage-factory/internal/telemetry"
	"github.com/sky-ai-eng/triage-factory/internal/upstream"
)

// freshSlackCalls gives t an empty call count and an empty rate-limit
// registry, restoring both after it.
func freshSlackCalls(t *testing.T) {
	t.Helper()
	upstream.ResetBudgetsForTest(t)
	prev := slackCalls
	slackCalls = newSlackCallWindow()
	t.Cleanup(func() { slackCalls = prev })
}

// slackMeterForTest points the upstream instruments at a reader t collects
// from.
func slackMeterForTest(t *testing.T) *sdkmetric.ManualReader {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	upstream.SetMeterProviderForTest(t, sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	return reader
}

// collectSlackMetrics reads every metric reader holds, by instrument name.
func collectSlackMetrics(t *testing.T, reader *sdkmetric.ManualReader) map[string]metricdata.Aggregation {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	out := map[string]metricdata.Aggregation{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			out[m.Name] = m.Data
		}
	}
	return out
}

func okSlackAPI(w http.ResponseWriter, _ *http.Request) {
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

// TestSlackCallWindow_CountsTheLastMinute: a call counts under its key until a
// minute after it was made, and a key with no call in the window is dropped
// by the next sweep.
func TestSlackCallWindow_CountsTheLastMinute(t *testing.T) {
	w := newSlackCallWindow()
	update := slackCallKey{"org-1", "bot:T1:A1", "chat.update"}
	info := slackCallKey{"org-1", "bot:T1:A1", "users.info"}
	t0 := time.Unix(1_000_000, 0)
	for _, s := range []struct {
		name string
		key  slackCallKey
		at   time.Duration
		want int
	}{
		{"first", update, 0, 1},
		{"second", update, 10 * time.Second, 2},
		{"another method", info, 20 * time.Second, 1},
		{"third", update, 30 * time.Second, 3},
		{"a minute after the first, which has left", update, time.Minute, 3},
		{"the second has left too", update, 75 * time.Second, 3},
		{"after a quiet minute", update, 3 * time.Minute, 1},
	} {
		if got := w.add(s.key, t0.Add(s.at)); got != s.want {
			t.Errorf("%s: count = %d, want %d", s.name, got, s.want)
		}
	}
	w.mu.Lock()
	_, kept := w.byKey[info]
	w.mu.Unlock()
	if kept {
		t.Error("users.info, idle since 20s, survived the sweep at 3m")
	}
}

// TestDoSlackJSON_ReportsTheMethodsBudget: every call is counted against its
// method, and the method's budget for the caller's credential is what was
// used and what is left of its published limit, until a minute after the
// latest call.
func TestDoSlackJSON_ReportsTheMethodsBudget(t *testing.T) {
	freshSlackCalls(t)
	srv := withFakeSlackAPI(t, okSlackAPI)
	ctx := context.Background()
	caller := slackBotCaller("org-1", "T1", "A1")

	start := time.Now()
	for i := 0; i < 3; i++ {
		if err := slackChatUpdate(ctx, srv.Client(), caller, "xoxb-test", slackMessageParams{Channel: "C1", ThreadTS: "1.0", Text: "x"}); err != nil {
			t.Fatalf("chat.update: %v", err)
		}
	}
	if _, err := slackUsersInfo(ctx, srv.Client(), caller, "xoxb-test", "U1"); err != nil {
		t.Fatalf("users.info: %v", err)
	}

	for _, c := range []struct {
		method                 string
		used, limit, remaining int
	}{
		{"chat.update", 3, slackTier3, slackTier3 - 3},
		{"users.info", 1, slackTier4, slackTier4 - 1},
	} {
		b, ok := upstream.BudgetForTest(upstream.Slack, "org-1", "bot:T1:A1", c.method)
		if !ok {
			t.Errorf("%s: no reading", c.method)
			continue
		}
		if b.Used != c.used || b.Limit != c.limit || b.Remaining != c.remaining {
			t.Errorf("%s = used %d, limit %d, remaining %d; want %d, %d, %d",
				c.method, b.Used, b.Limit, b.Remaining, c.used, c.limit, c.remaining)
		}
		if b.Reset.Before(start.Add(slackWindow)) || b.Reset.After(time.Now().Add(slackWindow)) {
			t.Errorf("%s: reset %v, want a minute after the latest call", c.method, b.Reset)
		}
	}
}

// TestDoSlackJSON_NoPublishedLimit_ReportsTheCountAlone: a method Slack
// publishes no per-minute figure for, and one missing from the table, report
// what was used and no limit.
func TestDoSlackJSON_NoPublishedLimit_ReportsTheCountAlone(t *testing.T) {
	freshSlackCalls(t)
	srv := withFakeSlackAPI(t, okSlackAPI)
	ctx := context.Background()
	caller := slackBotCaller("org-1", "T1", "A1")

	if _, err := slackChatPostMessage(ctx, srv.Client(), caller, "xoxb-test", slackMessageParams{Channel: "C1", Text: "x"}); err != nil {
		t.Fatalf("chat.postMessage: %v", err)
	}
	var out struct{}
	if err := doSlackJSON(ctx, srv.Client(), caller, newSlackTestRequest(t, ctx), &out); err != nil {
		t.Fatalf("some.method: %v", err)
	}

	for _, method := range []string{"chat.postMessage", "some.method"} {
		b, ok := upstream.BudgetForTest(upstream.Slack, "org-1", "bot:T1:A1", method)
		if !ok || b.Used != 1 || b.Limit != 0 || b.Remaining != 0 {
			t.Errorf("%s = (%+v, %v); want used 1 and no limit", method, b, ok)
		}
	}
}

// TestDoSlackJSON_CountsOnlyAnsweredCallsOfANamedCredential: a caller with no
// credential (the connect handler's checks) or no org counts nothing, and
// neither does a call that met no response.
func TestDoSlackJSON_CountsOnlyAnsweredCallsOfANamedCredential(t *testing.T) {
	freshSlackCalls(t)
	srv := withFakeSlackAPI(t, okSlackAPI)
	ctx := context.Background()
	var out struct{}
	for _, caller := range []slackCaller{{orgID: "org-1"}, {credential: "bot:T1:A1"}} {
		if err := doSlackJSON(ctx, srv.Client(), caller, newSlackTestRequest(t, ctx), &out); err != nil {
			t.Fatalf("doSlackJSON(%+v): %v", caller, err)
		}
	}
	client := srv.Client()
	srv.Close()
	if err := doSlackJSON(ctx, client, slackBotCaller("org-1", "T1", "A1"), newSlackTestRequest(t, ctx), &out); err == nil {
		t.Fatal("doSlackJSON against a closed server returned nil")
	}

	slackCalls.mu.Lock()
	counted := len(slackCalls.byKey)
	slackCalls.mu.Unlock()
	if counted != 0 {
		t.Errorf("counted %d keys, want none", counted)
	}
	for _, k := range []struct{ org, credential string }{{"org-1", ""}, {"", "bot:T1:A1"}, {"org-1", "bot:T1:A1"}} {
		if b, ok := upstream.BudgetForTest(upstream.Slack, k.org, k.credential, "some.method"); ok {
			t.Errorf("recorded %+v under org %q, credential %q", b, k.org, k.credential)
		}
	}
}

// TestDoSlackJSON_429_CountsTheCallAndTheRefusal: a refused attempt is a call
// Slack answered, so the 429 and the retry both count toward the method's
// budget, and the refusal is counted as the method's.
func TestDoSlackJSON_429_CountsTheCallAndTheRefusal(t *testing.T) {
	freshSlackCalls(t)
	reader := slackMeterForTest(t)
	recordSlackWaits(t)
	var hits int32
	srv := withFakeSlackAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&hits, 1) == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		okSlackAPI(w, r)
	})

	if err := slackReactionsAdd(context.Background(), srv.Client(), slackBotCaller("org-1", "T1", "A1"), "xoxb-test", "C1", "1.0", "eyes"); err != nil {
		t.Fatalf("reactions.add: %v", err)
	}

	if b, ok := upstream.BudgetForTest(upstream.Slack, "org-1", "bot:T1:A1", "reactions.add"); !ok || b.Used != 2 || b.Remaining != slackTier3-2 {
		t.Errorf("reactions.add = (%+v, %v); want 2 used of %d", b, ok, slackTier3)
	}
	sum, ok := collectSlackMetrics(t, reader)["upstream.rate_limit.refused"].(metricdata.Sum[int64])
	if !ok || len(sum.DataPoints) != 1 {
		t.Fatalf("upstream.rate_limit.refused = %#v, want one series", sum)
	}
	dp := sum.DataPoints[0]
	want := attribute.NewSet(
		telemetry.Upstream("slack"), telemetry.OrgID("org-1"),
		telemetry.Credential("bot:T1:A1"), telemetry.Bucket("reactions.add"),
	)
	if !dp.Attributes.Equals(&want) || dp.Value != 1 {
		t.Errorf("refused = %d under %v, want 1 under %v", dp.Value,
			dp.Attributes.Encoded(attribute.DefaultEncoder()), want.Encoded(attribute.DefaultEncoder()))
	}
}

// TestSlackWait_IsAWaitOnALimit: a 429's wait is logged and counted as a wait
// on a rate limit, under Slack and the caller's org.
func TestSlackWait_IsAWaitOnALimit(t *testing.T) {
	reader := slackMeterForTest(t)
	if err := slackWait(context.Background(), "org-1", time.Millisecond); err != nil {
		t.Fatalf("slackWait: %v", err)
	}
	sum, ok := collectSlackMetrics(t, reader)["upstream.rate_limit.wait"].(metricdata.Sum[float64])
	if !ok || len(sum.DataPoints) != 1 {
		t.Fatalf("upstream.rate_limit.wait = %#v, want one series", sum)
	}
	dp := sum.DataPoints[0]
	want := attribute.NewSet(telemetry.Upstream("slack"), telemetry.OrgID("org-1"))
	if !dp.Attributes.Equals(&want) || dp.Value <= 0 {
		t.Errorf("wait = %v under %v, want a positive wait under %v", dp.Value,
			dp.Attributes.Encoded(attribute.DefaultEncoder()), want.Encoded(attribute.DefaultEncoder()))
	}
}

// TestSlackMethodLimits_CoversEveryMethodCalled: the table names every Web API
// method api.go calls, and no other. Every method is a literal there, either
// after slackAPIBase ("/users.info?user=") or passed whole
// ("chat.postMessage").
func TestSlackMethodLimits_CoversEveryMethodCalled(t *testing.T) {
	src, err := os.ReadFile("api.go")
	if err != nil {
		t.Fatalf("read api.go: %v", err)
	}
	called := map[string]bool{}
	for _, m := range regexp.MustCompile(`"/?([a-z]+(?:\.[a-zA-Z]+)+)[?"]`).FindAllStringSubmatch(string(src), -1) {
		called[m[1]] = true
	}
	if len(called) == 0 {
		t.Fatal("found no methods in api.go")
	}
	var missing, extra []string
	for m := range called {
		if _, ok := slackMethodLimits[m]; !ok {
			missing = append(missing, m)
		}
	}
	for m := range slackMethodLimits {
		if !called[m] {
			extra = append(extra, m)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	if len(missing) > 0 {
		t.Errorf("methods api.go calls with no entry in slackMethodLimits: %v", missing)
	}
	if len(extra) > 0 {
		t.Errorf("slackMethodLimits entries api.go never calls: %v", extra)
	}
}

// TestSlackCallers_NameTheirCredential pins the credential ids, which are
// metric labels: a bot token is its (workspace, app) install, an app-level
// token its app.
func TestSlackCallers_NameTheirCredential(t *testing.T) {
	if got := slackBotCaller("org-1", "T1", "A1"); got != (slackCaller{orgID: "org-1", credential: "bot:T1:A1"}) {
		t.Errorf("slackBotCaller = %+v", got)
	}
	if got := slackAppCaller("org-1", "A1"); got != (slackCaller{orgID: "org-1", credential: "app:A1"}) {
		t.Errorf("slackAppCaller = %+v", got)
	}
}
