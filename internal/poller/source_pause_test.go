package poller

import (
	"context"
	"database/sql"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/eventbus"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// spanRecorder installs a span recorder on the OTel global, ONCE for the
// package. Once is the whole point: this package's tracer is a package variable
// resolved through the global delegate, and that delegate binds to the first
// real provider installed — a second SetTracerProvider leaves the already-bound
// tracer pointing at the first recorder, so per-test recorders would silently
// capture nothing.
var (
	spanRecorderOnce sync.Once
	spanRecorder     *tracetest.SpanRecorder
)

func recordSpans(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()
	spanRecorderOnce.Do(func() {
		spanRecorder = tracetest.NewSpanRecorder()
		otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spanRecorder)))
	})
	return spanRecorder
}

// spanOutcome returns the outcome attribute of the MOST RECENT span named name.
// Most recent because the recorder spans the package: tests run sequentially,
// so the last one recorded is this test's.
func spanOutcome(t *testing.T, recorder *tracetest.SpanRecorder, name string) string {
	t.Helper()
	ended := recorder.Ended()
	for i := len(ended) - 1; i >= 0; i-- {
		if ended[i].Name() != name {
			continue
		}
		for _, a := range ended[i].Attributes() {
			if a.Key == "outcome" {
				return a.Value.AsString()
			}
		}
		return ""
	}
	t.Fatalf("span %q was never recorded", name)
	return ""
}

// pauseSource records an org admin's disable for kind, the way the PATCH route
// does.
func pauseSource(t *testing.T, database *sql.DB, kind string) {
	t.Helper()
	if _, err := sqlitestore.New(database).OrgEventSources.SetDisabled(
		context.Background(), runmode.LocalDefaultOrgID, kind, true, runmode.LocalDefaultUserID); err != nil {
		t.Fatalf("pause %s: %v", kind, err)
	}
}

// TestRunGitHubCycleForOrg_SkipsPausedSource: an org admin turned GitHub off,
// so the cycle spends no API budget on it. The skip is an optimization, not the
// enforcement point — the router drops a paused source's events at the single
// funnel every producer crosses — so its whole observable contract is that the
// cycle did nothing and said why.
func TestRunGitHubCycleForOrg_SkipsPausedSource(t *testing.T) {
	recorder := recordSpans(t)
	runmode.SetForTest(t, runmode.ModeMulti)
	srv := pollerTestServer(t)
	ctx := context.Background()
	database := newMigratedSQLiteForPoller(t)
	stores := sqlitestore.New(database)
	org := runmode.LocalDefaultOrgID
	trackRepos(t, stores, org, []string{"octo/repo"})
	pauseSource(t, database, "github")

	reconciled := 0
	m := &Manager{
		database: database, pub: busPublisher{bus: newTestBus(t)},
		tasks: stores.Tasks, entities: stores.Entities, repos: stores.Repos,
		orgs: stores.Orgs, users: stores.Users,
		apps: &fakeInstallsStore{}, resolver: &fakeResolver{client: ghclient.NewClient(srv.URL, "pat")},
		EventSources:   stores.OrgEventSources,
		ReconcileGrant: func(context.Context, string) error { reconciled++; return nil },
	}
	m.runGitHubCycleForOrg(ctx, org)

	if got := spanOutcome(t, recorder, "poll.github.org"); got != "disabled" {
		t.Errorf("span outcome = %q, want %q", got, "disabled")
	}
	if reconciled != 0 {
		t.Errorf("grant reconcile ran %d times on a paused source, want 0 — the skip is ahead of every call", reconciled)
	}
	ents, err := stores.Entities.ListActiveSystem(ctx, org, "github")
	if err != nil {
		t.Fatalf("ListActiveSystem: %v", err)
	}
	if len(ents) != 0 {
		t.Errorf("paused cycle discovered %d entities, want 0", len(ents))
	}
}

// TestRunGitHubCycleForOrg_PausingJiraDoesNotSkipGitHub is the negative
// control: the pause names one source, and each cycle reads only its own.
func TestRunGitHubCycleForOrg_PausingJiraDoesNotSkipGitHub(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	srv := pollerTestServer(t)
	ctx := context.Background()
	database := newMigratedSQLiteForPoller(t)
	stores := sqlitestore.New(database)
	org := runmode.LocalDefaultOrgID
	trackRepos(t, stores, org, []string{"octo/repo"})
	pauseSource(t, database, "jira")

	m := &Manager{
		database: database, pub: busPublisher{bus: newTestBus(t)},
		tasks: stores.Tasks, entities: stores.Entities, repos: stores.Repos,
		orgs: stores.Orgs, users: stores.Users,
		apps: &fakeInstallsStore{}, resolver: &fakeResolver{client: ghclient.NewClient(srv.URL, "pat")},
		EventSources: stores.OrgEventSources,
	}
	m.runGitHubCycleForOrg(ctx, org)

	ents, err := stores.Entities.ListActiveSystem(ctx, org, "github")
	if err != nil {
		t.Fatalf("ListActiveSystem: %v", err)
	}
	if len(ents) != 1 {
		t.Errorf("pausing jira cost the github cycle: got %d entities, want 1", len(ents))
	}
}

// TestRunJiraCycleForOrg_SkipsPausedSource is the Jira twin. The resolver is
// nil on purpose: reaching it at all would be the failure this pins, so the
// skip is the only thing standing between this test and a nil dereference.
func TestRunJiraCycleForOrg_SkipsPausedSource(t *testing.T) {
	recorder := recordSpans(t)
	ctx := context.Background()
	database := newMigratedSQLiteForPoller(t)
	stores := sqlitestore.New(database)
	org := runmode.LocalDefaultOrgID
	pauseSource(t, database, "jira")

	m := &Manager{
		database: database, pub: busPublisher{bus: newTestBus(t)},
		tasks: stores.Tasks, entities: stores.Entities, repos: stores.Repos,
		orgs: stores.Orgs, users: stores.Users, secrets: stores.Secrets,
		jiraRules:    stores.JiraStatusRules,
		EventSources: stores.OrgEventSources,
	}
	m.runJiraCycleForOrg(ctx, nil, org, time.Now())

	if got := spanOutcome(t, recorder, "poll.jira.org"); got != "disabled" {
		t.Errorf("span outcome = %q, want %q", got, "disabled")
	}
}

// newTestBus builds a bus that closes with the test.
func newTestBus(t *testing.T) *eventbus.Bus {
	t.Helper()
	bus := eventbus.New()
	t.Cleanup(bus.Close)
	return bus
}
