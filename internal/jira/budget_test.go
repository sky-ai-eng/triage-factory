package jira

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/sky-ai-eng/triage-factory/internal/telemetry"
	"github.com/sky-ai-eng/triage-factory/internal/upstream"
)

// reply is one scripted response: a status (200 when zero), its headers, and
// its body ({} when empty).
type reply struct {
	status int
	header map[string]string
	body   string
}

// scriptedServer answers each request, whatever its path, with the next
// reply, and a bare 200 once they run out.
func scriptedServer(t *testing.T, replies ...reply) *httptest.Server {
	t.Helper()
	var n int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		i := int(atomic.AddInt32(&n, 1)) - 1
		w.Header().Set("Content-Type", "application/json")
		if i >= len(replies) {
			_, _ = w.Write([]byte(`{}`))
			return
		}
		for k, v := range replies[i].header {
			w.Header().Set(k, v)
		}
		if replies[i].status != 0 {
			w.WriteHeader(replies[i].status)
		}
		body := replies[i].body
		if body == "" {
			body = `{}`
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// The three credentials whose readings are recorded, as ForSystem and
// ForUser build them, each against base.
func cloudTokenClient(base string) *Client {
	return NewClient(CloudAPIToken(base, "bot@acme.com", "tok")).
		WithOrg("org-1").WithCredential(AuthMethodCloudAPIToken)
}

func cloudOAuthClient(base string) *Client {
	cfg := CloudOAuth("cloud-9", "acc")
	cfg.BaseURL = base
	return NewClient(cfg).WithOrg("org-1").WithCredential(AuthMethodCloudOAuth)
}

func dcPATClient(base string) *Client {
	return NewClient(DataCenterPAT(base, "pat")).WithOrg("org-1").WithCredential(AuthMethodDCPAT)
}

// myself is a GET of the myself endpoint through c.
func myself(c *Client) error {
	_, err := c.get(context.Background(), c.apiURL("myself"))
	return err
}

// collectMetrics reads every metric the provider holds, by instrument name.
func collectMetrics(t *testing.T, reader *sdkmetric.ManualReader) map[string]metricdata.Aggregation {
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

func meterForTest(t *testing.T) *sdkmetric.ManualReader {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	upstream.SetMeterProviderForTest(t, sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	return reader
}

// refusals reads the refusal counter by bucket, failing t for a refusal
// counted under any other upstream, org or credential than (jira, org-1,
// credential).
func refusals(t *testing.T, reader *sdkmetric.ManualReader, credential AuthMethod) map[string]int64 {
	t.Helper()
	out := map[string]int64{}
	sum, ok := collectMetrics(t, reader)["upstream.rate_limit.refused"].(metricdata.Sum[int64])
	if !ok {
		return out
	}
	for _, dp := range sum.DataPoints {
		bucket, _ := dp.Attributes.Value(telemetry.Bucket("").Key)
		want := attribute.NewSet(
			telemetry.Upstream("jira"), telemetry.OrgID("org-1"),
			telemetry.Credential(string(credential)), telemetry.Bucket(bucket.AsString()),
		)
		if !dp.Attributes.Equals(&want) {
			t.Errorf("refusal counted under %v, want (jira, org-1, %s, <bucket>)",
				dp.Attributes.Encoded(attribute.DefaultEncoder()), credential)
			continue
		}
		out[bucket.AsString()] += dp.Value
	}
	return out
}

// withinWindow reports whether reset is window after a time in [before, after].
func withinWindow(reset, before, after time.Time, window time.Duration) bool {
	return !reset.Before(before.Add(window)) && !reset.After(after.Add(window))
}

// TestRecordBudgets_CloudBurst: X-RateLimit-Limit and -Remaining on a Cloud
// response are the endpoint's per-second allowance, recorded under the
// endpoint's burst bucket and kept for one second. An API token is not
// subject to the hourly quota, so a response that says nothing of it records
// no quota reading.
func TestRecordBudgets_CloudBurst(t *testing.T) {
	upstream.ResetBudgetsForTest(t)
	srv := scriptedServer(t, reply{header: map[string]string{
		"X-RateLimit-Limit":     "100",
		"X-RateLimit-Remaining": "90",
	}})

	before := time.Now()
	if err := myself(cloudTokenClient(srv.URL)); err != nil {
		t.Fatalf("get: %v", err)
	}
	after := time.Now()

	b, ok := upstream.BudgetForTest(upstream.Jira, "org-1", "cloud_api_token", "burst:myself")
	if !ok || b.Remaining != 90 || b.Limit != 100 || !withinWindow(b.Reset, before, after, time.Second) || !b.Quiet {
		t.Errorf("burst:myself = (%+v, %v), want 90 of 100 resetting a second after the response, quiet", b, ok)
	}
	if b, ok := upstream.BudgetForTest(upstream.Jira, "org-1", "cloud_api_token", "quota"); ok {
		t.Errorf("recorded a quota reading %+v from a response that states none", b)
	}
}

// TestRecordBudgets_CloudBurstPerEndpoint: Atlassian sets the burst
// allowance per endpoint, so one credential's responses from two endpoints
// record two buckets, each with its own limit, and neither overwrites the
// other. The limits are a probe's, for one API token.
func TestRecordBudgets_CloudBurstPerEndpoint(t *testing.T) {
	upstream.ResetBudgetsForTest(t)
	srv := scriptedServer(t,
		reply{header: map[string]string{"X-RateLimit-Limit": "350", "X-RateLimit-Remaining": "349"}, body: `[]`},
		reply{header: map[string]string{"X-RateLimit-Limit": "200", "X-RateLimit-Remaining": "199"}},
	)
	c := cloudTokenClient(srv.URL)
	ctx := context.Background()

	if _, err := c.ListPriorities(ctx); err != nil {
		t.Fatalf("list priorities: %v", err)
	}
	if _, err := c.SearchIssues(ctx, "project = SKY", nil, 10); err != nil {
		t.Fatalf("search: %v", err)
	}
	for bucket, limit := range map[string]int{"burst:priority": 350, "burst:search/jql": 200} {
		b, ok := upstream.BudgetForTest(upstream.Jira, "org-1", "cloud_api_token", bucket)
		if !ok || b.Limit != limit || b.Remaining != limit-1 {
			t.Errorf("%s = (%+v, %v), want %d of %d", bucket, b, ok, limit-1, limit)
		}
	}
	if readings, _ := upstream.ChangedReadings(0); len(readings) != 2 {
		t.Errorf("readings = %+v, want the two endpoints' burst buckets alone", readings)
	}
}

// TestRecordBudgets_CloudBurstUnderTheTemplate: a request naming an issue
// records its endpoint's burst under the path template, never the issue key.
func TestRecordBudgets_CloudBurstUnderTheTemplate(t *testing.T) {
	upstream.ResetBudgetsForTest(t)
	srv := scriptedServer(t, reply{header: map[string]string{"X-RateLimit-Limit": "350", "X-RateLimit-Remaining": "340"}})

	if _, err := cloudTokenClient(srv.URL).GetTransitions(context.Background(), "ABC-123"); err != nil {
		t.Fatalf("get transitions: %v", err)
	}
	b, ok := upstream.BudgetForTest(upstream.Jira, "org-1", "cloud_api_token", "burst:issue/{}/transitions")
	if !ok || b.Remaining != 340 || b.Limit != 350 {
		t.Errorf("burst:issue/{}/transitions = (%+v, %v), want 340 of 350", b, ok)
	}
	readings, _ := upstream.ChangedReadings(0)
	for _, r := range readings {
		if strings.Contains(r.Bucket, "ABC-123") {
			t.Errorf("recorded a bucket naming the issue key: %q", r.Bucket)
		}
	}
}

// TestRecordBudgets_CloudQuotaNearLimit: X-RateLimit-NearLimit is the quota's
// only signal on a normal response, recorded as near_limit and kept for the
// quota's hour. A Cloud OAuth credential is metered by the quota, so its
// response without the header records the quota as not near; an API token's
// records it only when the header is there.
func TestRecordBudgets_CloudQuotaNearLimit(t *testing.T) {
	upstream.ResetBudgetsForTest(t)
	srv := scriptedServer(t,
		reply{header: map[string]string{"X-RateLimit-NearLimit": "true"}},
		reply{header: map[string]string{"X-RateLimit-NearLimit": "true"}},
		reply{},
	)

	before := time.Now()
	if err := myself(cloudTokenClient(srv.URL)); err != nil {
		t.Fatalf("get: %v", err)
	}
	if err := myself(cloudOAuthClient(srv.URL)); err != nil {
		t.Fatalf("get: %v", err)
	}
	after := time.Now()
	for _, credential := range []string{"cloud_api_token", "cloud_oauth"} {
		b, ok := upstream.BudgetForTest(upstream.Jira, "org-1", credential, "quota")
		near, known := b.NearLimit()
		if !ok || !near || !known || !withinWindow(b.Reset, before, after, time.Hour) {
			t.Errorf("%s quota = (%+v, %v), want near, kept for an hour", credential, b, ok)
		}
	}

	if err := myself(cloudOAuthClient(srv.URL)); err != nil {
		t.Fatalf("get: %v", err)
	}
	b, ok := upstream.BudgetForTest(upstream.Jira, "org-1", "cloud_oauth", "quota")
	if near, known := b.NearLimit(); !ok || near || !known {
		t.Errorf("cloud_oauth quota after a response without the header = (%+v, %v), want not near", b, ok)
	}
}

// TestRecordBudgets_Cloud429Quota: a 429 for the quota records it as near,
// with the reset X-RateLimit-Reset states, and is counted under its reason. A
// Retry-After past the inline cap is not waited on.
func TestRecordBudgets_Cloud429Quota(t *testing.T) {
	upstream.ResetBudgetsForTest(t)
	reader := meterForTest(t)
	reset := time.Now().Add(40 * time.Minute).UTC().Truncate(time.Second)
	srv := scriptedServer(t, reply{status: http.StatusTooManyRequests, header: map[string]string{
		"RateLimit-Reason":  reasonQuotaTenant,
		"X-RateLimit-Reset": reset.Format(time.RFC3339),
		"Retry-After":       "2400",
	}})

	if err := myself(cloudOAuthClient(srv.URL)); err == nil {
		t.Fatal("get succeeded on a 429 past the wait cap")
	}
	b, ok := upstream.BudgetForTest(upstream.Jira, "org-1", "cloud_oauth", "quota")
	if near, _ := b.NearLimit(); !ok || !near || !b.Reset.Equal(reset) {
		t.Errorf("quota = (%+v, %v), want near, resetting at %v", b, ok, reset)
	}
	if got := refusals(t, reader, AuthMethodCloudOAuth); got[reasonQuotaTenant] != 1 || len(got) != 1 {
		t.Errorf("refusals = %v, want one under %s", got, reasonQuotaTenant)
	}
}

// TestRecordBudgets_Cloud429Burst: a 429 for burst is the endpoint's, so it
// records the endpoint's burst reading with the reset the refusal states and
// is counted under that bucket, and its wait is a wait on a limit. The headers
// are a probe's. Its X-RateLimit-Reset has minute precision and had already
// passed when the refusal arrived, so the reset is the Retry-After's.
func TestRecordBudgets_Cloud429Burst(t *testing.T) {
	upstream.ResetBudgetsForTest(t)
	reader := meterForTest(t)
	srv := scriptedServer(t, reply{status: http.StatusTooManyRequests, header: map[string]string{
		"RateLimit-Reason":      reasonBurst,
		"Retry-After":           "1",
		"X-RateLimit-Reset":     "2026-10-19T23:07Z",
		"X-RateLimit-Remaining": "0",
		"X-RateLimit-Limit":     "350",
	}})

	before := time.Now()
	if _, err := cloudTokenClient(srv.URL).GetTransitions(context.Background(), "ABC-123"); err != nil {
		t.Fatalf("get transitions: %v", err)
	}
	const bucket = "burst:issue/{}/transitions"
	b, ok := upstream.BudgetForTest(upstream.Jira, "org-1", "cloud_api_token", bucket)
	if !ok || b.Remaining != 0 || b.Limit != 350 || b.Reset.Before(before.Add(time.Second)) || !b.Reset.Before(before.Add(2*time.Second)) {
		t.Errorf("%s = (%+v, %v), want 0 of 350 resetting a second after the refusal", bucket, b, ok)
	}
	if got := refusals(t, reader, AuthMethodCloudAPIToken); got[bucket] != 1 || len(got) != 1 {
		t.Errorf("refusals = %v, want one under %s", got, bucket)
	}
	sum, ok := collectMetrics(t, reader)["upstream.rate_limit.wait"].(metricdata.Sum[float64])
	if !ok || len(sum.DataPoints) != 1 {
		t.Fatalf("upstream.rate_limit.wait = %#v, want one series", sum)
	}
	want := attribute.NewSet(telemetry.Upstream("jira"), telemetry.OrgID("org-1"))
	if dp := sum.DataPoints[0]; !dp.Attributes.Equals(&want) || dp.Value <= 0 {
		t.Errorf("wait = %v under %v, want a positive wait under (jira, org-1)", dp.Value, dp.Attributes.Encoded(attribute.DefaultEncoder()))
	}
}

// TestRecordBudgets_UnrecognizedReasonsCountAsOther: a reason outside the
// documented set, or none, is counted as other rather than as a label of its
// own.
func TestRecordBudgets_UnrecognizedReasonsCountAsOther(t *testing.T) {
	upstream.ResetBudgetsForTest(t)
	reader := meterForTest(t)
	srv := scriptedServer(t,
		reply{status: http.StatusTooManyRequests, header: map[string]string{"RateLimit-Reason": "jira-cost-based", "Retry-After": "3600"}},
		reply{status: http.StatusTooManyRequests, header: map[string]string{"Retry-After": "3600"}},
		reply{status: http.StatusTooManyRequests, header: map[string]string{"RateLimit-Reason": reasonPerIssueWrite, "Retry-After": "3600"}},
	)
	c := cloudTokenClient(srv.URL)
	for range 3 {
		if err := myself(c); err == nil {
			t.Fatal("get succeeded on a 429 past the wait cap")
		}
	}
	if got := refusals(t, reader, AuthMethodCloudAPIToken); got[reasonOther] != 2 || got[reasonPerIssueWrite] != 1 || len(got) != 2 {
		t.Errorf("refusals = %v, want 2 under other and 1 under %s", got, reasonPerIssueWrite)
	}
}

// TestRecordBudgets_DataCenterTokenBucket: Data Center's token bucket is one
// requests reading, resetting when the bucket is full again, at least one
// interval out. Without the fill rate or interval the reset is unknown.
func TestRecordBudgets_DataCenterTokenBucket(t *testing.T) {
	bucket := func(remaining string) reply {
		return reply{header: map[string]string{
			"X-RateLimit-Limit":            "500",
			"X-RateLimit-Remaining":        remaining,
			"X-RateLimit-FillRate":         "10",
			"X-RateLimit-Interval-Seconds": "60",
		}}
	}
	for _, c := range []struct {
		name      string
		reply     reply
		remaining int
		toFull    time.Duration
	}{
		// 50 missing at 10 an interval: 5 intervals of a minute.
		{"draining", bucket("450"), 450, 5 * time.Minute},
		// 51 missing: a sixth, partial fill still has to land.
		{"partial fill", bucket("449"), 449, 6 * time.Minute},
		{"full", bucket("500"), 500, time.Minute},
		{"no fill rate", reply{header: map[string]string{"X-RateLimit-Limit": "500", "X-RateLimit-Remaining": "450"}}, 450, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			upstream.ResetBudgetsForTest(t)
			srv := scriptedServer(t, c.reply)
			before := time.Now()
			if err := myself(dcPATClient(srv.URL)); err != nil {
				t.Fatalf("get: %v", err)
			}
			after := time.Now()
			b, ok := upstream.BudgetForTest(upstream.Jira, "org-1", "dc_pat", "requests")
			if !ok || b.Remaining != c.remaining || b.Limit != 500 || b.Quiet {
				t.Fatalf("requests = (%+v, %v), want %d of 500, not quiet", b, ok, c.remaining)
			}
			if c.toFull == 0 {
				if !b.Reset.IsZero() {
					t.Errorf("reset = %v, want unknown", b.Reset)
				}
			} else if !withinWindow(b.Reset, before, after, c.toFull) {
				t.Errorf("reset = %v, want %v after the response", b.Reset, c.toFull)
			}
		})
	}
}

// TestRecordBudgets_DataCenterLimitingOff: an instance with rate limiting off
// sends no headers and records nothing. A 429 is still counted, under the
// requests bucket, since Data Center states no reason.
func TestRecordBudgets_DataCenterLimitingOff(t *testing.T) {
	upstream.ResetBudgetsForTest(t)
	reader := meterForTest(t)
	srv := scriptedServer(t, reply{}, reply{status: http.StatusTooManyRequests, header: map[string]string{"Retry-After": "3600"}})

	c := dcPATClient(srv.URL)
	if err := myself(c); err != nil {
		t.Fatalf("get: %v", err)
	}
	if b, ok := upstream.BudgetForTest(upstream.Jira, "org-1", "dc_pat", "requests"); ok {
		t.Errorf("recorded %+v from a response without headers", b)
	}
	if err := myself(c); err == nil {
		t.Fatal("get succeeded on a 429 past the wait cap")
	}
	if got := refusals(t, reader, AuthMethodDCPAT); got[bucketRequests] != 1 || len(got) != 1 {
		t.Errorf("refusals = %v, want one under requests", got)
	}
}

// TestRecordBudgets_UnnamedClientsRecordNothing: a client with no credential
// (a user's own token or PAT), no org (validation) or a proxy placeholder
// reports no reading and counts no refusal.
func TestRecordBudgets_UnnamedClientsRecordNothing(t *testing.T) {
	upstream.ResetBudgetsForTest(t)
	reader := meterForTest(t)
	limited := reply{status: http.StatusTooManyRequests, header: map[string]string{
		"RateLimit-Reason":      reasonBurst,
		"X-RateLimit-NearLimit": "true",
		"X-RateLimit-Limit":     "100",
		"X-RateLimit-Remaining": "0",
		"Retry-After":           "3600",
	}}
	srv := scriptedServer(t, limited, limited, limited)
	for name, c := range map[string]*Client{
		"user token": NewClient(CloudAPIToken(srv.URL, "u@acme.com", "tok")).WithOrg("org-1"),
		"no org":     NewClient(CloudAPIToken(srv.URL, "u@acme.com", "tok")).WithCredential(AuthMethodCloudAPIToken),
		"proxy":      NewClient(ProxyPlaceholder(srv.URL, "placeholder", DeploymentCloud)).WithOrg("org-1"),
	} {
		if err := myself(c); err == nil {
			t.Fatalf("%s: get succeeded on a 429 past the wait cap", name)
		}
	}
	if readings, _ := upstream.ChangedReadings(0); len(readings) > 0 {
		t.Errorf("recorded %+v for unnamed clients", readings)
	}
	if sum, ok := collectMetrics(t, reader)["upstream.rate_limit.refused"].(metricdata.Sum[int64]); ok && len(sum.DataPoints) > 0 {
		t.Errorf("counted refusals %+v for unnamed clients", sum.DataPoints)
	}
}

func TestPathTemplate(t *testing.T) {
	for format, want := range map[string]string{
		"priority":                                  "priority",
		"issue/%s/transitions":                      "issue/{}/transitions",
		"issue/%s?fields=assignee,status":           "issue/{}",
		"issue/%s/comment?startAt=%d&maxResults=%d": "issue/{}/comment",
		"project/search?%s":                         "project/search",
	} {
		if got := pathTemplate(format); got != want {
			t.Errorf("pathTemplate(%q) = %q, want %q", format, got, want)
		}
	}
}

func TestParseReset(t *testing.T) {
	for v, want := range map[string]time.Time{
		"2026-10-18T13:21:07Z":          time.Date(2026, 10, 18, 13, 21, 7, 0, time.UTC),
		"2026-10-18T13:21Z":             time.Date(2026, 10, 18, 13, 21, 0, 0, time.UTC),
		"2026-10-18T15:21:07.5+02:00":   time.Date(2026, 10, 18, 13, 21, 7, 500_000_000, time.UTC),
		" 2026-10-18T13:21:07.000Z ":    time.Date(2026, 10, 18, 13, 21, 7, 0, time.UTC),
		"1760793667":                    {},
		"Sat, 18 Oct 2026 13:21:07 GMT": {},
		"":                              {},
	} {
		got, ok := parseReset(v)
		if ok != !want.IsZero() || !got.Equal(want) {
			t.Errorf("parseReset(%q) = (%v, %v), want %v", v, got, ok, want)
		}
	}
}
