package tracker

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	jiraclient "github.com/sky-ai-eng/triage-factory/internal/jira"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// nonSystemEvents returns every captured event that isn't the poll-complete
// sentinel — the events a stub enrichment must NOT emit (TFAC-513 §3, Option A:
// a quiet seed mints no task-creating event for state that predates tracking).
func (p *recordingPublisher) nonSystemEvents() []domain.Event {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []domain.Event
	for _, e := range p.events {
		if e.EventType != domain.EventSystemPollCompleted {
			out = append(out, e)
		}
	}
	return out
}

// TestRefreshGitHub_EnrichesSnapshotlessStub is the §3 regression: Phase-2 used
// to `continue` past any active entity with no node_id, so a snapshot-less stub
// (created outside the poller by exec-touch FindOrCreate) was skipped forever.
// Now the stub's node_id is resolved via a REST read, its snapshot + title are
// seeded from the refresh, a terminal PR is closed — and, crucially, NO events
// are emitted (quiet seed, so wiring exec-touch live can't mint a spurious task).
func TestRefreshGitHub_EnrichesSnapshotlessStub(t *testing.T) {
	cases := []struct {
		name        string
		basicResp   string
		refreshResp string
		wantState   string
	}{
		{
			name: "open stub enriched, no events",
			basicResp: `{
				"number": 7, "node_id": "PR_node7", "title": "Stub PR",
				"state": "open", "merged": false,
				"html_url": "https://github.com/octo/repo/pull/7",
				"user": {"login": "bob"},
				"head": {"sha": "sha7", "ref": "feat"}, "base": {"ref": "main"}
			}`,
			refreshResp: `{"data":{"nodes":[
				{"id":"PR_node7","number":7,"title":"Stub PR Refreshed","author":{"login":"bob"},
				 "state":"OPEN","merged":false,"url":"https://github.com/octo/repo/pull/7",
				 "repository":{"nameWithOwner":"octo/repo"},
				 "createdAt":"2026-06-01T00:00:00Z","updatedAt":"2026-06-10T00:00:00Z"}
			]}}`,
			wantState: "active",
		},
		{
			name: "terminal stub enriched and closed, no events",
			basicResp: `{
				"number": 7, "node_id": "PR_node7", "title": "Stub PR",
				"state": "closed", "merged": true,
				"html_url": "https://github.com/octo/repo/pull/7",
				"user": {"login": "bob"},
				"head": {"sha": "sha7", "ref": "feat"}, "base": {"ref": "main"}
			}`,
			refreshResp: `{"data":{"nodes":[
				{"id":"PR_node7","number":7,"title":"Stub PR Refreshed","author":{"login":"bob"},
				 "state":"MERGED","merged":true,"url":"https://github.com/octo/repo/pull/7",
				 "repository":{"nameWithOwner":"octo/repo"},
				 "createdAt":"2026-06-01T00:00:00Z","updatedAt":"2026-06-10T00:00:00Z","mergedAt":"2026-06-10T00:00:00Z"}
			]}}`,
			wantState: "closed",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.HasSuffix(r.URL.Path, "/graphql"):
					_, _ = w.Write([]byte(tc.refreshResp))
				case strings.Contains(r.URL.Path, "/pulls/7"):
					_, _ = w.Write([]byte(tc.basicResp))
				default:
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
					http.Error(w, "unexpected", http.StatusNotFound)
				}
			}))
			t.Cleanup(srv.Close)

			ctx := context.Background()
			database := newMigratedSQLite(t)
			stores := sqlitestore.New(database)
			org := runmode.LocalDefaultOrgID

			// Seed a snapshot-less stub, exactly as exec-touch FindOrCreate would.
			stub, _, err := stores.Entities.FindOrCreate(ctx, org, "github", "octo/repo#7", "pr", "", "")
			if err != nil {
				t.Fatalf("seed stub: %v", err)
			}
			if stub.SnapshotJSON != "" {
				t.Fatalf("precondition: stub should be snapshot-less, got %q", stub.SnapshotJSON)
			}

			pub := &recordingPublisher{}
			tr := New(database, pub, stores.Tasks, stores.Entities, stores.Repos, org)
			client := ghclient.NewClient(srv.URL, "tok")

			// repos=nil → no discovery; the stub is reached only via Phase-2.
			if _, _, err := tr.RefreshGitHub(ctx, client, "", nil, nil); err != nil {
				t.Fatalf("RefreshGitHub: %v", err)
			}

			got, err := stores.Entities.GetBySource(ctx, org, "github", "octo/repo#7")
			if err != nil || got == nil {
				t.Fatalf("GetBySource: ent=%v err=%v", got, err)
			}
			if got.SnapshotJSON == "" || !strings.Contains(got.SnapshotJSON, "PR_node7") {
				t.Errorf("stub snapshot not seeded: %q", got.SnapshotJSON)
			}
			if got.Title != "Stub PR Refreshed" {
				t.Errorf("title = %q, want the refreshed title", got.Title)
			}
			if got.State != tc.wantState {
				t.Errorf("state = %q, want %q", got.State, tc.wantState)
			}
			if evts := pub.nonSystemEvents(); len(evts) != 0 {
				t.Errorf("quiet seed must emit no events, got %d: %v", len(evts), eventTypes(evts))
			}
		})
	}
}

// TestRefreshJira_EnrichesSnapshotlessStub is the §3 Jira side: a snapshot-less
// stub self-heals through the existing key-batch refresh, but — unlike before —
// Phase-3 seeds it WITHOUT diffing, so DiffJiraSnapshots' prev.Key=="" first-
// discovery branch never fires. The stub gets its snapshot + title (and is
// closed when terminal) with NO assigned/available/completed event, so wiring
// exec-touch live can't mint a spurious task off a touched issue.
func TestRefreshJira_EnrichesSnapshotlessStub(t *testing.T) {
	cases := []struct {
		name      string
		status    string
		projects  JiraRules
		wantState string
	}{
		{
			name:      "non-terminal stub enriched, no events",
			status:    "In Progress",
			projects:  nil, // no done-members configured → never terminal
			wantState: "active",
		},
		{
			name:      "terminal stub enriched and closed, no events",
			status:    "Done",
			projects:  JiraRules{{Key: "SKY", DoneMembers: []string{"Done"}}},
			wantState: "closed",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			searchResp := `{"issues":[
				{"key":"SKY-1","fields":{
					"summary":"Touched issue","status":{"name":"` + tc.status + `"},
					"assignee":{"displayName":"Alice","accountId":"acc-1"},
					"created":"2026-06-01T00:00:00.000+0000","updated":"2026-06-10T00:00:00.000+0000"
				}}
			]}`
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if strings.HasSuffix(r.URL.Path, "/search") {
					_, _ = w.Write([]byte(searchResp))
					return
				}
				_, _ = w.Write([]byte(`{}`))
			}))
			t.Cleanup(srv.Close)

			ctx := context.Background()
			database := newMigratedSQLite(t)
			stores := sqlitestore.New(database)
			org := runmode.LocalDefaultOrgID

			stub, _, err := stores.Entities.FindOrCreate(ctx, org, "jira", "SKY-1", "issue", "", "")
			if err != nil {
				t.Fatalf("seed stub: %v", err)
			}
			if stub.SnapshotJSON != "" {
				t.Fatalf("precondition: stub should be snapshot-less, got %q", stub.SnapshotJSON)
			}

			pub := &recordingPublisher{}
			tr := New(database, pub, stores.Tasks, stores.Entities, stores.Repos, org)
			client := jiraclient.NewClient(jiraclient.DataCenterPAT(srv.URL, "pat"))

			if _, err := tr.RefreshJira(client, srv.URL, tc.projects); err != nil {
				t.Fatalf("RefreshJira: %v", err)
			}

			got, err := stores.Entities.GetBySource(ctx, org, "jira", "SKY-1")
			if err != nil || got == nil {
				t.Fatalf("GetBySource: ent=%v err=%v", got, err)
			}
			if got.SnapshotJSON == "" || !strings.Contains(got.SnapshotJSON, tc.status) {
				t.Errorf("stub snapshot not seeded: %q", got.SnapshotJSON)
			}
			if got.Title != "Touched issue" {
				t.Errorf("title = %q, want the seeded summary", got.Title)
			}
			if got.State != tc.wantState {
				t.Errorf("state = %q, want %q", got.State, tc.wantState)
			}
			if evts := pub.nonSystemEvents(); len(evts) != 0 {
				t.Errorf("quiet seed must emit no events, got %d: %v", len(evts), eventTypes(evts))
			}
		})
	}
}

// TestRefreshGitHub_StubExitsSeedModeAndDiffsNextCycle closes the loop on the
// quiet seed: once cycle 1 seeds the node_id, the entity is no longer a stub, so
// cycle 2 takes the NORMAL diff path — GetPRBasic is not called again, and a real
// state change (draft → ready) emits its event as usual. This proves the seed is
// a one-time bootstrap, not a permanent "swallow all events" mode.
func TestRefreshGitHub_StubExitsSeedModeAndDiffsNextCycle(t *testing.T) {
	const basicResp = `{
		"number": 7, "node_id": "PR_node7", "title": "Stub PR",
		"state": "open", "merged": false,
		"html_url": "https://github.com/octo/repo/pull/7",
		"user": {"login": "bob"},
		"head": {"sha": "sha7", "ref": "feat"}, "base": {"ref": "main"}
	}`
	// Cycle 1 seeds a draft snapshot; cycle 2 returns the same PR marked ready.
	refreshResp := func(draft string) string {
		return `{"data":{"nodes":[
			{"id":"PR_node7","number":7,"title":"Stub PR","author":{"login":"bob"},
			 "state":"OPEN","merged":false,"isDraft":` + draft + `,
			 "url":"https://github.com/octo/repo/pull/7","repository":{"nameWithOwner":"octo/repo"},
			 "createdAt":"2026-06-01T00:00:00Z","updatedAt":"2026-06-10T00:00:00Z"}
		]}}`
	}

	var graphqlCalls, basicCalls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/graphql"):
			// Call 1 (cycle-1 seed) → draft; call 2 (cycle-2 diff) → ready.
			if atomic.AddInt32(&graphqlCalls, 1) == 1 {
				_, _ = w.Write([]byte(refreshResp("true")))
			} else {
				_, _ = w.Write([]byte(refreshResp("false")))
			}
		case strings.Contains(r.URL.Path, "/pulls/7"):
			atomic.AddInt32(&basicCalls, 1)
			_, _ = w.Write([]byte(basicResp))
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected", http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	ctx := context.Background()
	database := newMigratedSQLite(t)
	stores := sqlitestore.New(database)
	org := runmode.LocalDefaultOrgID

	if _, _, err := stores.Entities.FindOrCreate(ctx, org, "github", "octo/repo#7", "pr", "", ""); err != nil {
		t.Fatalf("seed stub: %v", err)
	}

	pub := &recordingPublisher{}
	tr := New(database, pub, stores.Tasks, stores.Entities, stores.Repos, org)
	client := ghclient.NewClient(srv.URL, "tok")

	// Cycle 1: quiet seed — no events, and the snapshot now carries the node_id.
	if _, _, err := tr.RefreshGitHub(ctx, client, "", nil, nil); err != nil {
		t.Fatalf("RefreshGitHub cycle 1: %v", err)
	}
	if evts := pub.nonSystemEvents(); len(evts) != 0 {
		t.Fatalf("cycle 1 (seed) must emit no events, got %v", eventTypes(evts))
	}
	if seeded, _ := stores.Entities.GetBySource(ctx, org, "github", "octo/repo#7"); seeded == nil || !strings.Contains(seeded.SnapshotJSON, "PR_node7") {
		t.Fatalf("cycle 1 did not seed the node_id: %+v", seeded)
	}

	// Cycle 2: the entity has a node_id now, so it takes the normal diff path —
	// no second GetPRBasic, and the draft→ready transition emits its event.
	if _, _, err := tr.RefreshGitHub(ctx, client, "", nil, nil); err != nil {
		t.Fatalf("RefreshGitHub cycle 2: %v", err)
	}
	if evts := pub.nonSystemEvents(); len(evts) != 1 || evts[0].EventType != domain.EventGitHubPRReadyForReview {
		t.Errorf("cycle 2 should diff normally and emit [ready_for_review], got %v", eventTypes(evts))
	}
	if got := atomic.LoadInt32(&basicCalls); got != 1 {
		t.Errorf("GetPRBasic calls = %d, want 1 (only the cycle-1 stub resolve; cycle 2 must not re-fetch)", got)
	}
}
