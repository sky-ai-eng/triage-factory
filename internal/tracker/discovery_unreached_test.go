package tracker

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	jiraclient "github.com/sky-ai-eng/triage-factory/internal/jira"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/upstream"
)

// TestDiscoveryUnreached pins when a discovery pass reports that it reached
// nothing: every call failed, and at least one failed because the connection
// did. A call that succeeded, a pass that sent nothing, and failures that are
// all answers about their own request are not that.
func TestDiscoveryUnreached(t *testing.T) {
	transient := &ghclient.HTTPError{StatusCode: http.StatusServiceUnavailable, Class: upstream.Transient}
	auth := &ghclient.HTTPError{StatusCode: http.StatusUnauthorized, Class: upstream.Auth}
	missing := &ghclient.HTTPError{StatusCode: http.StatusNotFound, Class: upstream.Rejected}
	local := errors.New("read pulls poll state: database is locked")

	for _, tc := range []struct {
		name      string
		errs      []error
		wantClass upstream.Class // "" means no error
	}{
		{name: "nothing sent"},
		{name: "every call succeeded", errs: []error{nil, nil}},
		{name: "one call succeeded", errs: []error{transient, nil, auth}},
		{name: "every call an answer about its own repo", errs: []error{missing, missing}},
		{name: "local failures say nothing about the connection", errs: []error{local, local}},
		{name: "every call failed in transit", errs: []error{transient, transient}, wantClass: upstream.Transient},
		{name: "a 404 beside an outage", errs: []error{missing, transient}, wantClass: upstream.Transient},
		{name: "auth is preferred", errs: []error{transient, auth, transient}, wantClass: upstream.Auth},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := discoveryUnreached("github", tc.errs)
			if tc.wantClass == "" {
				if err != nil {
					t.Errorf("discoveryUnreached = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatal("discoveryUnreached = nil, want an error")
			}
			if class, ok := upstream.ClassOf(err); !ok || class != tc.wantClass {
				t.Errorf("class of %v = %q (ok=%v), want %q — the wrapped failure must keep its class", err, class, ok, tc.wantClass)
			}
		})
	}
}

// TestRefreshJira_EveryQueryFailingIsAnError: when every discovery query gets
// a 503, the cycle reached nothing. RefreshJira says so instead of carrying on
// to an empty refresh, and publishes no poll completion — the sentinel that
// marks Jira ready in poll_readiness. The control half of the test is the same
// cycle against a Jira that answers, with no entities either: that one does
// publish the completion, so its absence above is the error's doing.
func TestRefreshJira_EveryQueryFailingIsAnError(t *testing.T) {
	jiraclient.SetRetryBackoffForTest(t, time.Millisecond)
	var failing atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if failing.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"errorMessages":["Service Unavailable"]}`))
			return
		}
		_, _ = w.Write([]byte(`{"issues":[],"total":0}`))
	}))
	t.Cleanup(srv.Close)

	database := newMigratedSQLite(t)
	stores := sqlitestore.New(database)
	org := runmode.LocalDefaultOrgID
	client := jiraclient.NewClient(jiraclient.DataCenterPAT(srv.URL, "pat"))
	rules := JiraRules{{Key: "PROJ", PickupMembers: jiraRefs("To Do"), DoneMembers: jiraRefs("Done")}}
	completions := func(pub *recordingPublisher) int {
		pub.mu.Lock()
		defer pub.mu.Unlock()
		n := 0
		for _, e := range pub.events {
			if e.EventType == domain.EventSystemPollCompleted {
				n++
			}
		}
		return n
	}

	failing.Store(true)
	pub := &recordingPublisher{}
	_, err := New(database, pub, stores.Tasks, stores.Entities, stores.Repos, stores.EventQueue, org).
		RefreshJira(context.Background(), client, srv.URL, rules)
	if err == nil {
		t.Fatal("RefreshJira returned nil with every discovery query failing")
	}
	if class, ok := upstream.ClassOf(err); !ok || class != upstream.Transient {
		t.Errorf("class of %v = %q (ok=%v), want transient", err, class, ok)
	}
	if n := completions(pub); n != 0 {
		t.Errorf("a cycle that reached nothing published %d poll completions, want 0", n)
	}

	failing.Store(false)
	pub = &recordingPublisher{}
	if _, err := New(database, pub, stores.Tasks, stores.Entities, stores.Repos, stores.EventQueue, org).
		RefreshJira(context.Background(), client, srv.URL, rules); err != nil {
		t.Fatalf("RefreshJira against a Jira that answers: %v", err)
	}
	if n := completions(pub); n != 1 {
		t.Errorf("a cycle Jira answered published %d poll completions, want 1", n)
	}
}

// TestRefreshJira_EveryQueryRateLimitedIsAnError: when Jira answers every
// discovery query with a 429 whose Retry-After is longer than the client
// waits, the cycle fetched nothing. That is not a completed poll: RefreshJira
// returns the rate limit, keeping its class, and publishes no completion. A
// rate-limited query is not one Jira rejected, so no workflow is read to
// salvage it.
func TestRefreshJira_EveryQueryRateLimitedIsAnError(t *testing.T) {
	jiraclient.SetRetryBackoffForTest(t, time.Millisecond)
	var calls, workflowReads atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if strings.HasSuffix(r.URL.Path, "/statuses") {
			workflowReads.Add(1)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "120")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"errorMessages":["Rate limit exceeded."]}`))
	}))
	t.Cleanup(srv.Close)

	database := newMigratedSQLite(t)
	stores := sqlitestore.New(database)
	client := jiraclient.NewClient(jiraclient.DataCenterPAT(srv.URL, "pat"))
	rules := JiraRules{
		{Key: "PROJ", PickupMembers: jiraRefs("To Do"), DoneMembers: jiraRefs("Done")},
		{Key: "OPS", PickupMembers: jiraRefs("To Do"), DoneMembers: jiraRefs("Done")},
	}
	pub := &recordingPublisher{}

	_, err := New(database, pub, stores.Tasks, stores.Entities, stores.Repos, stores.EventQueue, runmode.LocalDefaultOrgID).
		RefreshJira(context.Background(), client, srv.URL, rules)
	if err == nil {
		t.Fatal("RefreshJira returned nil with every discovery query rate-limited")
	}
	if class, ok := upstream.ClassOf(err); !ok || class != upstream.RateLimited {
		t.Errorf("class of %v = %q (ok=%v), want rate_limited", err, class, ok)
	}
	pub.mu.Lock()
	defer pub.mu.Unlock()
	for _, e := range pub.events {
		if e.EventType == domain.EventSystemPollCompleted {
			t.Error("a cycle that fetched nothing published a poll completion")
		}
	}
	if calls.Load() == 0 {
		t.Error("no discovery query reached the server")
	}
	if n := workflowReads.Load(); n != 0 {
		t.Errorf("read a workflow %d times to salvage a rate-limited query", n)
	}
}

// TestDiscoveryRateLimited pins when a discovery pass that the connection did
// not fail still fetched nothing: no call succeeded and at least one was
// rate-limited.
func TestDiscoveryRateLimited(t *testing.T) {
	limited := &jiraclient.StatusError{Status: http.StatusTooManyRequests, Class: upstream.RateLimited}
	rejected := &jiraclient.StatusError{Status: http.StatusBadRequest, Class: upstream.Rejected}
	for _, tc := range []struct {
		name string
		errs []error
		want bool
	}{
		{name: "nothing sent"},
		{name: "one call succeeded", errs: []error{limited, nil}},
		{name: "every call rejected", errs: []error{rejected, rejected}},
		{name: "every call rate-limited", errs: []error{limited, limited}, want: true},
		{name: "rate-limited beside a rejection", errs: []error{rejected, limited}, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := discoveryRateLimited("jira", tc.errs)
			if (err != nil) != tc.want {
				t.Fatalf("discoveryRateLimited = %v, want error: %v", err, tc.want)
			}
			if err != nil {
				if class, _ := upstream.ClassOf(err); class != upstream.RateLimited {
					t.Errorf("class of %v = %q, want rate_limited", err, class)
				}
			}
		})
	}
}
