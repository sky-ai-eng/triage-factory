package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/integrations"
	"github.com/sky-ai-eng/triage-factory/internal/upstream"
)

// budgetServer answers every request with the rate-limit headers of the
// bucket the request draws on: graphql for the GraphQL endpoint, core for
// everything else. remaining[bucket] is what it reports, read per request.
func budgetServer(t *testing.T, limit int, reset time.Time, remaining map[string]*atomic.Int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bucket := bucketCore
		body := `[]`
		if strings.HasSuffix(r.URL.Path, "/graphql") {
			bucket, body = bucketGraphQL, `{"data":{}}`
		}
		w.Header().Set("X-RateLimit-Resource", bucket)
		w.Header().Set("X-RateLimit-Limit", fmt.Sprint(limit))
		w.Header().Set("X-RateLimit-Remaining", fmt.Sprint(remaining[bucket].Load()))
		w.Header().Set("X-RateLimit-Reset", fmt.Sprint(reset.Unix()))
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// gqlProbe is a GraphQL query body for tests that only need the request sent.
var gqlProbe = map[string]any{"query": "query { viewer { login } }"}

func counter(n int32) *atomic.Int32 {
	c := new(atomic.Int32)
	c.Store(n)
	return c
}

// patClient is a client made for org-1 on its PAT, the shape every recorded
// reading below comes from unless a test says otherwise.
func patClient(baseURL string) *Client {
	return NewClient(baseURL, "tok").WithOrg("org-1").WithCredential(CredentialPAT)
}

// TestRecordRateLimit_KeyedByResource: a GraphQL response and a REST response
// report different budgets, in different units, and neither overwrites the
// other, in the client's own state or in the recorder.
func TestRecordRateLimit_KeyedByResource(t *testing.T) {
	upstream.ResetBudgetsForTest(t)
	reset := time.Now().Add(time.Hour).Truncate(time.Second)
	srv := budgetServer(t, 5000, reset, map[string]*atomic.Int32{
		bucketCore:    counter(4000),
		bucketGraphQL: counter(300),
	})
	c := patClient(srv.URL)
	ctx := context.Background()

	if _, err := c.Get(ctx, "/x"); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if _, err := c.PostGraphQL(ctx, gqlProbe); err != nil {
		t.Fatalf("PostGraphQL: %v", err)
	}

	if remaining, _, ok := c.RateLimit(bucketCore); !ok || remaining != 4000 {
		t.Errorf("client core = (%d, %v), want 4000", remaining, ok)
	}
	if remaining, _, ok := c.RateLimit(bucketGraphQL); !ok || remaining != 300 {
		t.Errorf("client graphql = (%d, %v), want 300", remaining, ok)
	}
	for bucket, want := range map[string]int{bucketCore: 4000, bucketGraphQL: 300} {
		b, ok := upstream.BudgetForTest(upstream.GitHub, "org-1", CredentialPAT, bucket)
		if !ok || b.Remaining != want || b.Limit != 5000 || !b.Reset.Equal(reset) {
			t.Errorf("recorded %s = (%+v, %v), want %d of 5000 resetting %v", bucket, b, ok, want, reset)
		}
	}
}

// TestRecordRateLimit_OnlyClientsNamingACredentialReport: a reading is
// recorded under an org and a credential, so a client missing either records
// nothing — an unauthenticated call, or a run's credential-proxy client.
func TestRecordRateLimit_OnlyClientsNamingACredentialReport(t *testing.T) {
	upstream.ResetBudgetsForTest(t)
	srv := budgetServer(t, 5000, time.Now().Add(time.Hour), map[string]*atomic.Int32{bucketCore: counter(10)})
	ctx := context.Background()

	for name, c := range map[string]*Client{
		"no credential": NewClient(srv.URL, "tok").WithOrg("org-1"),
		"proxy":         NewProxyClient(srv.URL, "placeholder").WithOrg("org-1"),
		"no org":        NewClient(srv.URL, "tok").WithCredential(CredentialPAT),
	} {
		if _, err := c.Get(ctx, "/x"); err != nil {
			t.Fatalf("Get (%s): %v", name, err)
		}
	}
	for _, org := range []string{"org-1", ""} {
		for _, credential := range []string{"", CredentialPAT} {
			if b, ok := upstream.BudgetForTest(upstream.GitHub, org, credential, bucketCore); ok {
				t.Errorf("recorded %+v under org %q, credential %q", b, org, credential)
			}
		}
	}
}

// TestRecordRateLimit_MissingResourceHeader: a response without
// X-RateLimit-Resource is recorded under the bucket its request draws on, so
// an older GHES's GraphQL answer still does not overwrite core.
func TestRecordRateLimit_MissingResourceHeader(t *testing.T) {
	upstream.ResetBudgetsForTest(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", "12")
		w.Header().Set("X-RateLimit-Used", "88")
		w.Header().Set("X-RateLimit-Reset", fmt.Sprint(time.Now().Add(time.Hour).Unix()))
		_, _ = w.Write([]byte(`{"data":{}}`))
	}))
	t.Cleanup(srv.Close)
	c := patClient(srv.URL)

	if _, err := c.PostGraphQL(context.Background(), gqlProbe); err != nil {
		t.Fatalf("PostGraphQL: %v", err)
	}
	if _, _, ok := c.RateLimit(bucketCore); ok {
		t.Error("a GraphQL response was recorded as core")
	}
	b, ok := upstream.BudgetForTest(upstream.GitHub, "org-1", CredentialPAT, bucketGraphQL)
	if !ok || b.Remaining != 12 || b.Limit != 100 {
		t.Errorf("graphql = (%+v, %v), want 12 of 100 (limit derived from used + remaining)", b, ok)
	}
}

// TestRecordRateLimit_UnknownLimit: a response stating neither its limit nor
// what was used leaves the size unknown rather than reading as a full bucket.
func TestRecordRateLimit_UnknownLimit(t *testing.T) {
	upstream.ResetBudgetsForTest(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", "12")
		w.Header().Set("X-RateLimit-Reset", fmt.Sprint(time.Now().Add(time.Hour).Unix()))
		_, _ = w.Write([]byte(`[]`))
	}))
	t.Cleanup(srv.Close)

	if _, err := patClient(srv.URL).Get(context.Background(), "/x"); err != nil {
		t.Fatalf("Get: %v", err)
	}
	b, ok := upstream.BudgetForTest(upstream.GitHub, "org-1", CredentialPAT, bucketCore)
	if !ok || b.Remaining != 12 || b.Limit != 0 {
		t.Errorf("core = (%+v, %v), want 12 left of an unknown limit", b, ok)
	}
}

// TestPreflight_SpentGraphQLDoesNotHoldREST: a client that has seen its
// graphql budget spent refuses the next GraphQL query before sending it, and
// still sends a REST call, which draws on core.
func TestPreflight_SpentGraphQLDoesNotHoldREST(t *testing.T) {
	upstream.ResetBudgetsForTest(t)
	var sent atomic.Int32
	inner := budgetServer(t, 5000, time.Now().Add(time.Hour), map[string]*atomic.Int32{
		bucketCore:    counter(4000),
		bucketGraphQL: counter(0),
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sent.Add(1)
		inner.Config.Handler.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	c := patClient(srv.URL)
	ctx := context.Background()

	if _, err := c.PostGraphQL(ctx, gqlProbe); err != nil {
		t.Fatalf("seed PostGraphQL: %v", err)
	}
	start := time.Now()
	if _, err := c.Get(ctx, "/x"); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("REST call took %v; a spent graphql budget must not hold it", elapsed)
	}
	_, err := c.PostGraphQL(ctx, gqlProbe)
	var rl *ErrRateLimited
	if !errors.As(err, &rl) {
		t.Fatalf("second PostGraphQL err = %v, want *ErrRateLimited from the pre-flight", err)
	}
	if got := sent.Load(); got != 2 {
		t.Errorf("requests sent = %d, want 2 (the second GraphQL query is refused before sending)", got)
	}
}

// TestSecondaryLimit_CountsAsRateLimitedOnly: a secondary limit carries no
// budget of its own, only a 429 or 403 with Retry-After, so it counts as a
// rate-limited attempt and leaves no reading behind.
func TestSecondaryLimit_CountsAsRateLimitedOnly(t *testing.T) {
	upstream.ResetBudgetsForTest(t)
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"message":"You have exceeded a secondary rate limit."}`))
			return
		}
		_, _ = w.Write([]byte(`[]`))
	}))
	t.Cleanup(srv.Close)

	ctx, tally := upstream.WithTally(context.Background())
	if _, err := patClient(srv.URL).Get(ctx, "/x"); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if tally.Count(upstream.RateLimited) != 1 || tally.Count(upstream.OK) != 1 {
		t.Errorf("tally = %d rate_limited, %d ok; want 1, 1", tally.Count(upstream.RateLimited), tally.Count(upstream.OK))
	}
	if b, ok := upstream.BudgetForTest(upstream.GitHub, "org-1", CredentialPAT, bucketCore); ok {
		t.Errorf("a secondary limit left a reading: %+v", b)
	}
}

// TestResolverClients_ReportPerInstallation pins the case the credential key
// exists for: one org, two GitHub accounts, one App installation on each.
// GitHub meters each installation separately, so a client the resolver mints
// for each account reports under its own installation, and neither reading
// overwrites the other.
func TestResolverClients_ReportPerInstallation(t *testing.T) {
	upstream.ResetBudgetsForTest(t)
	remainingFor := map[string]string{"Bearer ghs_456": "4000", "Bearer ghs_789": "12"}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/app/installations/", func(w http.ResponseWriter, r *http.Request) {
		id, _, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/api/v3/app/installations/"), "/")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"token": "ghs_" + id, "expires_at": time.Now().Add(time.Hour).UTC()})
	})
	mux.HandleFunc("/api/v3/probe", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Resource", bucketCore)
		w.Header().Set("X-RateLimit-Limit", "5000")
		w.Header().Set("X-RateLimit-Remaining", remainingFor[r.Header.Get("Authorization")])
		w.Header().Set("X-RateLimit-Reset", fmt.Sprint(time.Now().Add(time.Hour).Unix()))
		_, _ = w.Write([]byte(`[]`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	r := newTestResolver(
		&fakeSecrets{vals: map[string]string{"pem": testPEM(t)}},
		&fakeApps{app: activeApp(), insts: []domain.OrgGitHubAppInstallation{
			installOnAccount("456", "1234", "acme"),
			installOnAccount("789", "5678", "globex"),
		}},
		&fakeOrgs{base: srv.URL},
		&fakeAgents{},
		nil,
	)
	for _, owner := range []string{"acme", "globex"} {
		client, err := r.ClientFor(context.Background(), "org-1", owner)
		if err != nil {
			t.Fatalf("ClientFor(%s): %v", owner, err)
		}
		if _, err := client.Get(context.Background(), "/probe"); err != nil {
			t.Fatalf("probe as %s: %v", owner, err)
		}
	}

	for installationID, want := range map[string]int{"456": 4000, "789": 12} {
		b, ok := upstream.BudgetForTest(upstream.GitHub, "org-1", InstallationCredential(installationID), bucketCore)
		if !ok || b.Remaining != want {
			t.Errorf("installation %s = (%+v, %v), want %d left", installationID, b, ok, want)
		}
	}
}

// TestResolverClients_PATReportsUnderPAT: the PAT tier's client reports its
// readings under the org's PAT.
func TestResolverClients_PATReportsUnderPAT(t *testing.T) {
	upstream.ResetBudgetsForTest(t)
	srv := budgetServer(t, 5000, time.Now().Add(time.Hour), map[string]*atomic.Int32{bucketCore: counter(4321)})
	r := newTestResolver(
		&fakeSecrets{vals: map[string]string{integrations.KeyGitHubPAT: "ghp_test"}},
		&fakeApps{app: nil}, // no registered App — PAT tier
		&fakeOrgs{base: srv.URL},
		&fakeAgents{},
		nil,
	)
	client, err := r.ClientFor(context.Background(), "org-1", "acme")
	if err != nil {
		t.Fatalf("ClientFor: %v", err)
	}
	if _, err := client.Get(context.Background(), "/probe"); err != nil {
		t.Fatalf("probe: %v", err)
	}
	if b, ok := upstream.BudgetForTest(upstream.GitHub, "org-1", CredentialPAT, bucketCore); !ok || b.Remaining != 4321 {
		t.Errorf("pat core = (%+v, %v), want 4321 left", b, ok)
	}
}
