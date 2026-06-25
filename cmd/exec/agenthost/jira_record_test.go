package agenthost

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// The TFAC-459 recording tests drive the LocalClient's host-routed Jira
// mutations against a fake Jira backend with a *real* SQLite store bundle, and
// assert that each successful action lands the right `artifacts` row. The
// LocalClient is the single seam the multi daemon dispatches through, so
// proving it here covers both the sandbox and local-mode paths.

// startFakeJira stands a minimal Jira REST backend covering the mutating verbs
// the recording tests exercise: transition (list + apply), comment (returns an
// id), assignee (self/unassign), and create (returns a key).
func startFakeJira(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		path := r.URL.Path
		switch {
		case strings.HasSuffix(path, "/myself"):
			_, _ = io.WriteString(w, `{"name":"bot","accountId":""}`)
		case strings.Contains(path, "/transitions"):
			if r.Method == http.MethodGet {
				_, _ = io.WriteString(w, `{"transitions":[{"id":"31","name":"Done","to":{"id":"5","name":"Done"}}]}`)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		case strings.HasSuffix(path, "/comment"):
			_, _ = io.WriteString(w, `{"id":"10042"}`)
		case strings.HasSuffix(path, "/assignee"):
			w.WriteHeader(http.StatusNoContent)
		case strings.HasSuffix(path, "/issue"):
			_, _ = io.WriteString(w, `{"key":"SKY-1"}`)
		default:
			_, _ = io.WriteString(w, `{}`)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// newJiraRecordingStores builds a real SQLite store bundle (so Artifacts / Tx /
// Orgs are live) with the Jira credential faked to point at jiraURL, seeds a
// run for the artifacts FK to anchor on, and returns the bundle + the run
// identity. eventTriggered picks the write path withWrite routes through:
// admin pool (no user) when true, a synthetic-claims tx (manual run) when
// false — both are exercised across the tests below.
func newJiraRecordingStores(t *testing.T, jiraURL string, eventTriggered bool) (db.Stores, RunInfo) {
	t.Helper()
	conn, err := sql.Open("sqlite", ":memory:?_pragma=foreign_keys(on)")
	if err != nil {
		t.Fatalf("open in-memory db: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	conn.SetMaxOpenConns(1)
	conn.SetMaxIdleConns(1)
	if err := db.BootstrapSchemaForTest(conn); err != nil {
		t.Fatalf("bootstrap schema: %v", err)
	}

	const runID = "11111111-1111-1111-1111-111111111111"
	if _, err := conn.Exec(
		`INSERT INTO runs (id, origin, status) VALUES (?, 'interactive', 'running')`, runID,
	); err != nil {
		t.Fatalf("seed run: %v", err)
	}

	stores := sqlitestore.New(conn)
	stores.Secrets = fakeJiraSecrets{url: jiraURL, pat: "org-pat"}
	info := RunInfo{
		OrgID:            runmode.LocalDefaultOrgID,
		TeamID:           runmode.LocalDefaultTeamID,
		RunID:            runID,
		IsEventTriggered: eventTriggered,
	}
	return stores, info
}

func listRunArtifacts(t *testing.T, stores db.Stores, runID string) []domain.Artifact {
	t.Helper()
	arts, err := stores.Artifacts.ListByRun(context.Background(), runmode.LocalDefaultOrgID, runID)
	if err != nil {
		t.Fatalf("ListByRun: %v", err)
	}
	return arts
}

// TestLocalClient_JiraActions_RecordArtifacts pins one row per successful
// action with the right kind/target/state/dedup_key, across both write paths.
func TestLocalClient_JiraActions_RecordArtifacts(t *testing.T) {
	for _, eventTriggered := range []bool{true, false} {
		name := "manual"
		if eventTriggered {
			name = "event-triggered"
		}
		t.Run(name, func(t *testing.T) {
			t.Run("create", func(t *testing.T) {
				jira := startFakeJira(t)
				stores, info := newJiraRecordingStores(t, jira.URL, eventTriggered)
				client := NewLocal(stores, info)

				key, err := client.JiraCreateIssue(context.Background(), "SKY", "Task", "do a thing", "", "", "")
				if err != nil {
					t.Fatalf("JiraCreateIssue: %v", err)
				}
				if key != "SKY-1" {
					t.Fatalf("created key = %q, want SKY-1", key)
				}
				arts := listRunArtifacts(t, stores, info.RunID)
				if len(arts) != 1 {
					t.Fatalf("want 1 artifact, got %d: %+v", len(arts), arts)
				}
				a := arts[0]
				if a.Provider != domain.ArtifactProviderJira || a.Kind != domain.ArtifactKindIssue ||
					a.Target != "SKY-1" || a.ExternalID != "SKY-1" || a.State != domain.ArtifactStateIssueCreated ||
					a.DedupKey != "jira:issue:SKY-1" {
					t.Errorf("create artifact mismatch: %+v", a)
				}
				if a.RunID != info.RunID || a.TeamID != runmode.LocalDefaultTeamID {
					t.Errorf("attribution mismatch: run=%q team=%q", a.RunID, a.TeamID)
				}
			})

			t.Run("transition", func(t *testing.T) {
				jira := startFakeJira(t)
				stores, info := newJiraRecordingStores(t, jira.URL, eventTriggered)
				client := NewLocal(stores, info)

				if err := client.JiraTransitionTo(context.Background(), "SKY-9", "Done"); err != nil {
					t.Fatalf("JiraTransitionTo: %v", err)
				}
				arts := listRunArtifacts(t, stores, info.RunID)
				if len(arts) != 1 {
					t.Fatalf("want 1 artifact, got %d: %+v", len(arts), arts)
				}
				a := arts[0]
				if a.Kind != domain.ArtifactKindIssue || a.Target != "SKY-9" ||
					a.State != domain.ArtifactStateIssueUpdated || a.DedupKey != "jira:issue:SKY-9" {
					t.Errorf("transition artifact mismatch: %+v", a)
				}
				if !strings.Contains(a.DetailsJSON, `"status":"Done"`) {
					t.Errorf("details_json %q did not carry the new status", a.DetailsJSON)
				}
			})

			t.Run("comment", func(t *testing.T) {
				jira := startFakeJira(t)
				stores, info := newJiraRecordingStores(t, jira.URL, eventTriggered)
				client := NewLocal(stores, info)

				if err := client.JiraAddComment(context.Background(), "SKY-9", "looks good to me"); err != nil {
					t.Fatalf("JiraAddComment: %v", err)
				}
				arts := listRunArtifacts(t, stores, info.RunID)
				if len(arts) != 1 {
					t.Fatalf("want 1 artifact, got %d: %+v", len(arts), arts)
				}
				a := arts[0]
				if a.Kind != domain.ArtifactKindComment || a.Target != "SKY-9" ||
					a.ExternalID != "10042" || a.State != domain.ArtifactStateCommentPosted ||
					a.DedupKey != "jira:comment:10042" {
					t.Errorf("comment artifact mismatch: %+v", a)
				}
				if !strings.Contains(a.DetailsJSON, "looks good to me") {
					t.Errorf("details_json %q did not carry the body snippet", a.DetailsJSON)
				}
			})

			t.Run("assign", func(t *testing.T) {
				jira := startFakeJira(t)
				stores, info := newJiraRecordingStores(t, jira.URL, eventTriggered)
				client := NewLocal(stores, info)

				if err := client.JiraAssignToSelf(context.Background(), "SKY-9"); err != nil {
					t.Fatalf("JiraAssignToSelf: %v", err)
				}
				arts := listRunArtifacts(t, stores, info.RunID)
				if len(arts) != 1 {
					t.Fatalf("want 1 artifact, got %d: %+v", len(arts), arts)
				}
				a := arts[0]
				if a.Kind != domain.ArtifactKindIssue || a.Target != "SKY-9" ||
					a.State != domain.ArtifactStateIssueUpdated || a.DedupKey != "jira:issue:SKY-9" {
					t.Errorf("assign artifact mismatch: %+v", a)
				}
				if !strings.Contains(a.DetailsJSON, `"assignee":"self"`) {
					t.Errorf("details_json %q did not carry the assignee", a.DetailsJSON)
				}
			})
		})
	}
}

// TestLocalClient_JiraTransitionThenComment_DedupsIssue pins the upsert
// invariant: a transition and a comment on the same issue leave exactly one
// `issue` artifact (deduped on jira:issue:<KEY>) plus one `comment` artifact.
func TestLocalClient_JiraTransitionThenComment_DedupsIssue(t *testing.T) {
	jira := startFakeJira(t)
	stores, info := newJiraRecordingStores(t, jira.URL, true)
	client := NewLocal(stores, info)
	ctx := context.Background()

	if err := client.JiraTransitionTo(ctx, "SKY-5", "Done"); err != nil {
		t.Fatalf("JiraTransitionTo: %v", err)
	}
	if err := client.JiraAddComment(ctx, "SKY-5", "done and dusted"); err != nil {
		t.Fatalf("JiraAddComment: %v", err)
	}

	arts := listRunArtifacts(t, stores, info.RunID)
	if len(arts) != 2 {
		t.Fatalf("want 2 artifacts (one issue, one comment), got %d: %+v", len(arts), arts)
	}
	var issues, comments int
	for _, a := range arts {
		switch a.Kind {
		case domain.ArtifactKindIssue:
			issues++
			if a.DedupKey != "jira:issue:SKY-5" {
				t.Errorf("issue dedup_key = %q", a.DedupKey)
			}
		case domain.ArtifactKindComment:
			comments++
			if a.DedupKey != "jira:comment:10042" {
				t.Errorf("comment dedup_key = %q", a.DedupKey)
			}
		default:
			t.Errorf("unexpected artifact kind %q", a.Kind)
		}
	}
	if issues != 1 || comments != 1 {
		t.Errorf("want 1 issue + 1 comment, got %d issue / %d comment", issues, comments)
	}
}

// erroringArtifacts fails every write so the best-effort path is exercised.
type erroringArtifacts struct {
	db.ArtifactStore
	mu    sync.Mutex
	calls int
}

func (e *erroringArtifacts) Upsert(context.Context, string, domain.Artifact) (domain.Artifact, error) {
	e.mu.Lock()
	e.calls++
	e.mu.Unlock()
	return domain.Artifact{}, errors.New("boom")
}

func (e *erroringArtifacts) UpsertSystem(context.Context, string, domain.Artifact) (domain.Artifact, error) {
	e.mu.Lock()
	e.calls++
	e.mu.Unlock()
	return domain.Artifact{}, errors.New("boom")
}

func (e *erroringArtifacts) callCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.calls
}

// TestLocalClient_JiraRecordingFailure_DoesNotFailAction pins the best-effort
// contract: a store write that errors is swallowed (logged), and the Jira
// action — which already took effect upstream — still returns success.
func TestLocalClient_JiraRecordingFailure_DoesNotFailAction(t *testing.T) {
	jira := startFakeJira(t)
	stores, info := newJiraRecordingStores(t, jira.URL, true)
	rec := &erroringArtifacts{}
	stores.Artifacts = rec
	client := NewLocal(stores, info)

	if err := client.JiraTransitionTo(context.Background(), "SKY-9", "Done"); err != nil {
		t.Fatalf("transition must not fail when recording fails: %v", err)
	}
	if rec.callCount() == 0 {
		t.Fatal("expected the recorder to have been called (and to have failed)")
	}
}
