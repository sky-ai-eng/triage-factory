package linear

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/sky-ai-eng/triage-factory/internal/telemetry"
	"github.com/sky-ai-eng/triage-factory/internal/upstream"
)

// systemClient is the shape ForSystem builds for an org on an API key: a
// client whose readings are recorded under org-1 and api_key.
func (s *stub) systemClient() *Client {
	return s.client().WithOrg("org-1").WithCredential(string(AuthMethodAPIKey))
}

func millis(t time.Time) string { return strconv.FormatInt(t.UnixMilli(), 10) }

// freshEndpointNames gives t an empty set of reported endpoint names, and
// restores the process's set via t.Cleanup.
func freshEndpointNames(t *testing.T) {
	t.Helper()
	endpointNames.Lock()
	prevSeen, prevFull := endpointNames.seen, endpointNames.full
	endpointNames.seen, endpointNames.full = map[string]struct{}{}, false
	endpointNames.Unlock()
	t.Cleanup(func() {
		endpointNames.Lock()
		endpointNames.seen, endpointNames.full = prevSeen, prevFull
		endpointNames.Unlock()
	})
}

// TestRecordBudgets_EachBucketOnItsOwn: requests, complexity and an
// endpoint's own limit are three budgets, each recorded under its own bucket
// with the limit, remaining and reset its headers state.
func TestRecordBudgets_EachBucketOnItsOwn(t *testing.T) {
	upstream.ResetBudgetsForTest(t)
	freshEndpointNames(t)
	reqReset := time.Now().Add(40 * time.Minute).Truncate(time.Millisecond)
	cxReset := time.Now().Add(50 * time.Minute).Truncate(time.Millisecond)
	epReset := time.Now().Add(time.Minute).Truncate(time.Millisecond)
	s := newStub(t, scripted(reply{
		header: map[string]string{
			"X-RateLimit-Requests-Limit":              "5000",
			"X-RateLimit-Requests-Remaining":          "4990",
			"X-RateLimit-Requests-Reset":              millis(reqReset),
			"X-RateLimit-Complexity-Limit":            "2000000",
			"X-RateLimit-Complexity-Remaining":        "1500000",
			"X-RateLimit-Complexity-Reset":            millis(cxReset),
			"X-RateLimit-Endpoint-Name":               "issueCreate",
			"X-RateLimit-Endpoint-Requests-Limit":     "100",
			"X-RateLimit-Endpoint-Requests-Remaining": "7",
			"X-RateLimit-Endpoint-Requests-Reset":     millis(epReset),
		},
		body: viewerOK,
	}))

	if _, err := s.systemClient().Viewer(context.Background()); err != nil {
		t.Fatalf("Viewer: %v", err)
	}
	for bucket, want := range map[string]upstream.Budget{
		"requests":             {Remaining: 4990, Limit: 5000, Reset: reqReset},
		"complexity":           {Remaining: 1500000, Limit: 2000000, Reset: cxReset},
		"endpoint:issueCreate": {Remaining: 7, Limit: 100, Reset: epReset},
	} {
		got, ok := upstream.BudgetForTest(upstream.Linear, "org-1", "api_key", bucket)
		if !ok || got.Remaining != want.Remaining || got.Limit != want.Limit || !got.Reset.Equal(want.Reset) {
			t.Errorf("recorded %s = (%+v, %v), want %+v", bucket, got, ok, want)
		}
	}
}

// TestRecordBudgets_PartialHeaders: remaining alone is a reading, with its
// size and reset unknown; an endpoint's counts with no name to file them under
// are not one.
func TestRecordBudgets_PartialHeaders(t *testing.T) {
	upstream.ResetBudgetsForTest(t)
	s := newStub(t, scripted(reply{
		header: map[string]string{
			"X-RateLimit-Requests-Remaining":          "12",
			"X-RateLimit-Complexity-Limit":            "2000000",
			"X-RateLimit-Endpoint-Requests-Remaining": "3",
		},
		body: viewerOK,
	}))

	if _, err := s.systemClient().Viewer(context.Background()); err != nil {
		t.Fatalf("Viewer: %v", err)
	}
	if got, ok := upstream.BudgetForTest(upstream.Linear, "org-1", "api_key", "requests"); !ok ||
		got != (upstream.Budget{Remaining: 12}) {
		t.Errorf("recorded requests = (%+v, %v), want 12 left of an unknown limit, reset unknown", got, ok)
	}
	for _, bucket := range []string{"complexity", "endpoint:"} {
		if got, ok := upstream.BudgetForTest(upstream.Linear, "org-1", "api_key", bucket); ok {
			t.Errorf("recorded %s = %+v from a response that stated no remaining for it", bucket, got)
		}
	}
}

// TestEndpointBucket_BoundsTheNames: an endpoint name becomes a label value
// on every org's and credential's series, so one that does not look like a
// name is refused, and so is a new one once the process has reported
// maxEndpointNames; a name already reported still is.
func TestEndpointBucket_BoundsTheNames(t *testing.T) {
	freshEndpointNames(t)
	for _, name := range []string{"issueCreate", "issue.create-v2_x", strings.Repeat("a", maxEndpointNameLen)} {
		if got, ok := endpointBucket(name); !ok || got != "endpoint:"+name {
			t.Errorf("endpointBucket(%q) = (%q, %v), want endpoint:%s", name, got, ok, name)
		}
	}
	for _, name := range []string{"issue create", "a/b", "issué", `{"id":1}`, strings.Repeat("a", maxEndpointNameLen+1)} {
		if got, ok := endpointBucket(name); ok {
			t.Errorf("endpointBucket(%q) = %q, want it refused", name, got)
		}
	}

	for i := 0; len(endpointNames.seen) < maxEndpointNames; i++ {
		if _, ok := endpointBucket(fmt.Sprintf("ep%d", i)); !ok {
			t.Fatalf("ep%d refused before the bound", i)
		}
	}
	if got, ok := endpointBucket("oneMore"); ok {
		t.Errorf("a new name past the bound = %q, want it refused", got)
	}
	if _, ok := endpointBucket("issueCreate"); !ok {
		t.Error("a name already reported was refused once the bound was reached")
	}
}

// TestRecordBudgets_RefusedEndpointName: a response whose endpoint name is
// refused still reports its other buckets.
func TestRecordBudgets_RefusedEndpointName(t *testing.T) {
	upstream.ResetBudgetsForTest(t)
	freshEndpointNames(t)
	s := newStub(t, scripted(reply{
		header: map[string]string{
			"X-RateLimit-Requests-Remaining":          "40",
			"X-RateLimit-Endpoint-Name":               "issue 7f3c",
			"X-RateLimit-Endpoint-Requests-Remaining": "3",
		},
		body: viewerOK,
	}))

	if _, err := s.systemClient().Viewer(context.Background()); err != nil {
		t.Fatalf("Viewer: %v", err)
	}
	if _, ok := upstream.BudgetForTest(upstream.Linear, "org-1", "api_key", "requests"); !ok {
		t.Error("requests not recorded beside a refused endpoint name")
	}
	if b, ok := upstream.BudgetForTest(upstream.Linear, "org-1", "api_key", "endpoint:issue 7f3c"); ok {
		t.Errorf("recorded %+v under a refused endpoint name", b)
	}
}

// TestRecordBudgets_RateLimitedResponseReports: the response that refuses a
// request states the spent budget, and that reading is what the gauges show
// while the caller holds the reset.
func TestRecordBudgets_RateLimitedResponseReports(t *testing.T) {
	upstream.ResetBudgetsForTest(t)
	shortWaits(t)
	reset := time.Now().Add(time.Hour).Truncate(time.Millisecond)
	s := newStub(t, scripted(rateLimited(reset)))

	if _, err := s.systemClient().Viewer(context.Background()); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("err = %v, want ErrRateLimited", err)
	}
	if got, ok := upstream.BudgetForTest(upstream.Linear, "org-1", "api_key", "requests"); !ok ||
		got.Remaining != 0 || !got.Reset.Equal(reset) {
		t.Errorf("recorded requests = (%+v, %v), want 0 left until %v", got, ok, reset)
	}
}

// TestRecordBudgets_OnlyClientsNamingACredentialReport: a reading is recorded
// under an org and a credential, so a client missing either records nothing:
// a user's own credential (ForUser), a credential being validated, and a run's
// credential-proxy client.
func TestRecordBudgets_OnlyClientsNamingACredentialReport(t *testing.T) {
	upstream.ResetBudgetsForTest(t)
	s := newStub(t, scripted(reply{
		header: map[string]string{"X-RateLimit-Requests-Remaining": "10", "X-RateLimit-Requests-Limit": "5000"},
		body:   viewerOK,
	}))
	ctx := context.Background()

	for name, c := range map[string]*Client{
		"user":   s.client().WithOrg("org-1"),
		"no org": s.client().WithCredential(string(AuthMethodAPIKey)),
		"proxy":  NewClient(ProxyPlaceholder(s.srv.URL, "placeholder")).WithOrg("org-1"),
	} {
		if _, err := c.Viewer(ctx); err != nil {
			t.Fatalf("Viewer (%s): %v", name, err)
		}
	}
	for _, org := range []string{"org-1", ""} {
		for _, credential := range []string{"", "api_key"} {
			if b, ok := upstream.BudgetForTest(upstream.Linear, org, credential, "requests"); ok {
				t.Errorf("recorded %+v under org %q, credential %q", b, org, credential)
			}
		}
	}
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

// TestRateLimited_WaitIsCounted: the wait for a rate limit's reset is added to
// the org's wait counter.
func TestRateLimited_WaitIsCounted(t *testing.T) {
	shortWaits(t)
	reader := meterForTest(t)
	s := newStub(t, scripted(rateLimited(time.Now().Add(100*time.Millisecond)), reply{body: viewerOK}))

	if _, err := s.systemClient().Viewer(context.Background()); err != nil {
		t.Fatalf("Viewer: %v", err)
	}
	sum, ok := collectMetrics(t, reader)["upstream.rate_limit.wait"].(metricdata.Sum[float64])
	if !ok || len(sum.DataPoints) != 1 {
		t.Fatalf("upstream.rate_limit.wait = %#v, want one series", sum)
	}
	dp := sum.DataPoints[0]
	want := attribute.NewSet(telemetry.Upstream("linear"), telemetry.OrgID("org-1"))
	if !dp.Attributes.Equals(&want) || dp.Value <= 0 {
		t.Errorf("wait = %v under %v, want a positive wait under %v", dp.Value, dp.Attributes.Encoded(attribute.DefaultEncoder()), want.Encoded(attribute.DefaultEncoder()))
	}
}

// TestQueryCost_ByOperationForEveryClient: X-Complexity is recorded under the
// operation the document names, whichever credential sent it, and a response
// without it records nothing.
func TestQueryCost_ByOperationForEveryClient(t *testing.T) {
	reader := meterForTest(t)
	s := newStub(t, scripted(
		reply{header: map[string]string{"X-Complexity": "1"}, body: viewerOK},
		reply{header: map[string]string{"X-Complexity": "3"}, body: viewerOK},
		reply{body: viewerOK},
	))
	ctx := context.Background()
	for _, c := range []*Client{s.systemClient(), s.client(), s.client()} {
		if _, err := c.Viewer(ctx); err != nil {
			t.Fatalf("Viewer: %v", err)
		}
	}

	hist, ok := collectMetrics(t, reader)["upstream.query.cost"].(metricdata.Histogram[int64])
	if !ok || len(hist.DataPoints) != 1 {
		t.Fatalf("upstream.query.cost = %#v, want one series", hist)
	}
	dp := hist.DataPoints[0]
	want := attribute.NewSet(telemetry.Upstream("linear"), telemetry.Op("Viewer"))
	if !dp.Attributes.Equals(&want) {
		t.Errorf("series = %v, want %v", dp.Attributes.Encoded(attribute.DefaultEncoder()), want.Encoded(attribute.DefaultEncoder()))
	}
	if dp.Count != 2 || dp.Sum != 4 {
		t.Errorf("count, sum = %d, %d; want 2 queries costing 4 points", dp.Count, dp.Sum)
	}
}

func TestOperationName(t *testing.T) {
	for doc, want := range map[string]string{
		issuesByIDQuery:           "IssuesByID",
		searchIssuesQuery:         "SearchIssues",
		viewerQuery:               "Viewer",
		commentCreateMutation:     "CommentCreate",
		"query { viewer { id } }": "",
		"{ viewer { id } }":       "",
		"":                        "",
	} {
		if got := operationName(doc); got != want {
			t.Errorf("operationName(%.30q) = %q, want %q", doc, got, want)
		}
	}
}

// TestOperationName_EveryDocumentNamesOne: the operation names are the cost
// histogram's labels, so every document in documents.go has to declare one.
func TestOperationName_EveryDocumentNamesOne(t *testing.T) {
	src, err := os.ReadFile("documents.go")
	if err != nil {
		t.Fatalf("read documents.go: %v", err)
	}
	heads := regexp.MustCompile(`(?m)^(query|mutation)\b.*$`).FindAllString(string(src), -1)
	if len(heads) == 0 {
		t.Fatal("found no documents in documents.go")
	}
	for _, head := range heads {
		if operationName(head) == "" {
			t.Errorf("document %q names no operation", head)
		}
	}
}
