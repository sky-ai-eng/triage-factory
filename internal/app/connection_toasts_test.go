package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/ai"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/eventbus"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/github/ghbase"
	"github.com/sky-ai-eng/triage-factory/internal/githubapp"
	"github.com/sky-ai-eng/triage-factory/internal/ingest"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/toast"
	"github.com/sky-ai-eng/triage-factory/pkg/websocket"
)

// openConnectionTestStores opens a fresh in-memory local install.
func openConnectionTestStores(t *testing.T) (db.Stores, *sql.DB) {
	t.Helper()
	conn, err := sql.Open("sqlite", db.TestDSNMemory)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	conn.SetMaxOpenConns(1)
	conn.SetMaxIdleConns(1)
	if err := db.BootstrapSchemaForTest(conn); err != nil {
		t.Fatalf("bootstrap schema: %v", err)
	}
	return sqlitestore.New(conn), conn
}

// staticResolver hands every caller one client: the org's PAT, as far as the
// poll cycle can tell.
type staticResolver struct{ client *ghclient.Client }

func (r staticResolver) ClientFor(context.Context, string, string) (*ghclient.Client, error) {
	return r.client, nil
}

func (r staticResolver) ClientForRepo(context.Context, string, string, string) (*ghclient.Client, error) {
	return r.client, nil
}

func (r staticResolver) TokenFor(context.Context, string, string) (githubapp.Token, error) {
	return githubapp.Token{}, nil
}

func (r staticResolver) BaseURLFor(context.Context, string) (string, error) {
	return ghbase.DefaultBaseURL(), nil
}

func (r staticResolver) OrgIdentityFor(context.Context, string) (string, string, bool) {
	return "", "", false
}

// expectToastControl fires a toast at the org and requires the client to
// receive it, so a preceding "no message" assertion is known to have been made
// against a live socket that would have shown a toast.
func expectToastControl(t *testing.T, hub *websocket.Hub, client *hubTestClient, orgID string) {
	t.Helper()
	toast.Error(hub, orgID, "control")
	var evt struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(client.expectMessage(t, 2*time.Second), &evt); err != nil || evt.Type != "toast" {
		t.Fatalf("control toast did not arrive (type %q, err %v)", evt.Type, err)
	}
}

// TestPollFailure_NoToast: a GitHub poll cycle whose every request fails
// reaches people as the org's connection state, not as a toast. The poller is
// built the way production builds it; only the grant mirror's passes, which
// this cycle is not about, are switched off.
func TestPollFailure_NoToast(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	ghclient.SetTransientBackoffForTest(t, time.Millisecond)
	var requests atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"message":"Service Unavailable"}`))
	}))
	t.Cleanup(srv.Close)

	stores, conn := openConnectionTestStores(t)
	org := runmode.LocalDefaultOrgID
	if err := stores.TeamGitHubRepos.ReplaceForTeam(context.Background(), org, runmode.LocalDefaultTeamID, dbtest.TestGitHubHost,
		[]domain.TeamGitHubRepo{{Owner: "octo", Repo: "a"}}); err != nil {
		t.Fatalf("track repo: %v", err)
	}
	bus := eventbus.New()
	t.Cleanup(bus.Close)
	hub := websocket.NewHub()
	a := &App{
		database:   conn,
		stores:     stores,
		wsHub:      hub,
		ingestor:   ingest.New(bus, stores.EventQueue, func() {}),
		ghResolver: staticResolver{client: ghclient.NewClient(srv.URL, "pat")},
	}
	m := a.newPollerManager()
	m.ReconcileGrant, m.RefreshManagedInstallations = nil, nil

	client := dialHubTestClient(t, hub)
	waitForHubClient(t, hub)

	m.PollGitHubOnce(context.Background(), org)

	if requests.Load() == 0 {
		t.Fatal("the cycle sent no request; the test is not exercising a failure")
	}
	st, err := stores.PollReadiness.Connection(context.Background(), org, "github")
	if err != nil {
		t.Fatalf("Connection: %v", err)
	}
	if st.State != db.ConnectionDown {
		t.Errorf("state after a failed cycle = %+v, want down", st)
	}
	client.expectNoMessage(t, 300*time.Millisecond)
	expectToastControl(t, hub, client, org)
}

// failingScores fails the scoring cycle at its first real read, the way a
// database fault would, and says when it has.
type failingScores struct {
	db.ScoreStore
	failed chan struct{}
}

func (f *failingScores) ResetStaleScoring(context.Context, string) (int, error) { return 0, nil }

func (f *failingScores) UnscoredTasks(context.Context, string) ([]domain.Task, error) {
	defer close(f.failed)
	return nil, errors.New("unscored tasks: database is locked")
}

// TestScoringFailure_NoToast: a scoring cycle that aborts reaches the log and
// not the UI. The scorer runs with the callbacks production wires.
func TestScoringFailure_NoToast(t *testing.T) {
	hub := websocket.NewHub()
	a := &App{wsHub: hub}
	scores := &failingScores{failed: make(chan struct{})}
	mgr := ai.NewManager(scores, nil, nil, nil, nil, nil, nil, a.scorerCallbacks())
	t.Cleanup(mgr.Stop)

	client := dialHubTestClient(t, hub)
	waitForHubClient(t, hub)

	mgr.Trigger("org-1")
	select {
	case <-scores.failed:
	case <-time.After(5 * time.Second):
		t.Fatal("the scoring cycle never ran")
	}
	client.expectNoMessage(t, 300*time.Millisecond)
	expectToastControl(t, hub, client, "org-1")
}

// TestHandlePollCompleted_StampsEverySource: a completed poll stamps its
// source's last-poll time, for every polled source, because the team activity
// page reads them all.
func TestHandlePollCompleted_StampsEverySource(t *testing.T) {
	stores, _ := openConnectionTestStores(t)
	a := &App{stores: stores, wsHub: websocket.NewHub()}
	org := runmode.LocalDefaultOrgID

	for _, source := range []string{"github", "jira", "linear"} {
		a.handlePollCompleted(domain.Event{
			OrgID:        org,
			EventType:    domain.EventSystemPollCompleted,
			MetadataJSON: `{"source":"` + source + `","started_at":0,"entities":1}`,
		})
	}

	times, err := stores.PollReadiness.LastPollTimes(context.Background(), org)
	if err != nil {
		t.Fatalf("LastPollTimes: %v", err)
	}
	for _, source := range []string{"github", "jira", "linear"} {
		if _, ok := times[source]; !ok {
			t.Errorf("no last-poll time for %s after its completion; got %v", source, times)
		}
	}
}
