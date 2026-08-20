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
	jiraclient "github.com/sky-ai-eng/triage-factory/internal/jira"
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
// conversation for the artifacts FK to anchor on, and returns the bundle + the
// conversation identity. eventTriggered picks the write path withWrite routes
// through: admin pool (no user) when true, a synthetic-claims tx (manual
// conversation) when false — both are exercised across the tests below.
func newJiraRecordingStores(t *testing.T, jiraURL string, eventTriggered bool) (db.Stores, ConversationInfo) {
	_, stores, info := newJiraRecordingStoresConn(t, jiraURL, eventTriggered)
	return stores, info
}

// newJiraRecordingStoresConn is newJiraRecordingStores plus the raw *sql.DB, for
// touch tests that read conversation_memory_entities directly.
func newJiraRecordingStoresConn(t *testing.T, jiraURL string, eventTriggered bool) (*sql.DB, db.Stores, ConversationInfo) {
	t.Helper()
	conn, err := sql.Open("sqlite", db.TestDSNMemory)
	if err != nil {
		t.Fatalf("open in-memory db: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	conn.SetMaxOpenConns(1)
	conn.SetMaxIdleConns(1)
	if err := db.BootstrapSchemaForTest(conn); err != nil {
		t.Fatalf("bootstrap schema: %v", err)
	}

	const conversationID = "11111111-1111-1111-1111-111111111111"
	if _, err := conn.Exec(
		`INSERT INTO conversations (id, origin, status) VALUES (?, 'interactive', 'running')`, conversationID,
	); err != nil {
		t.Fatalf("seed conversation: %v", err)
	}

	stores := sqlitestore.New(conn)
	stores.Secrets = fakeJiraSecrets{url: jiraURL, pat: "org-pat"}
	info := ConversationInfo{
		OrgID:            runmode.LocalDefaultOrgID,
		TeamID:           runmode.LocalDefaultTeamID,
		ConversationID:   conversationID,
		IsEventTriggered: eventTriggered,
	}
	return conn, stores, info
}

func listConversationArtifacts(t *testing.T, stores db.Stores, conversationID string) []domain.Artifact {
	t.Helper()
	arts, err := stores.Artifacts.ListByConversation(context.Background(), runmode.LocalDefaultOrgID, conversationID)
	if err != nil {
		t.Fatalf("ListByConversation: %v", err)
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
				arts := listConversationArtifacts(t, stores, info.ConversationID)
				if len(arts) != 1 {
					t.Fatalf("want 1 artifact, got %d: %+v", len(arts), arts)
				}
				a := arts[0]
				if a.Provider != domain.ArtifactProviderJira || a.Kind != domain.ArtifactKindIssue ||
					a.Target != "SKY-1" || a.ExternalID != "SKY-1" || a.State != domain.ArtifactStateIssueCreated ||
					a.DedupKey != "jira:issue:SKY-1" {
					t.Errorf("create artifact mismatch: %+v", a)
				}
				if a.ConversationID != info.ConversationID || a.TeamID != runmode.LocalDefaultTeamID {
					t.Errorf("attribution mismatch: conversation=%q team=%q", a.ConversationID, a.TeamID)
				}
			})

			t.Run("transition", func(t *testing.T) {
				jira := startFakeJira(t)
				stores, info := newJiraRecordingStores(t, jira.URL, eventTriggered)
				client := NewLocal(stores, info)

				if err := client.JiraTransitionTo(context.Background(), "SKY-9", "Done"); err != nil {
					t.Fatalf("JiraTransitionTo: %v", err)
				}
				arts := listConversationArtifacts(t, stores, info.ConversationID)
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
				arts := listConversationArtifacts(t, stores, info.ConversationID)
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
				arts := listConversationArtifacts(t, stores, info.ConversationID)
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

			t.Run("update", func(t *testing.T) {
				jira := startFakeJira(t)
				stores, info := newJiraRecordingStores(t, jira.URL, eventTriggered)
				client := NewLocal(stores, info)

				summary := "rewritten"
				priority := "High"
				if err := client.JiraUpdateIssue(context.Background(), "SKY-9", jiraclient.UpdateIssueFields{
					Summary: &summary, Priority: &priority,
				}); err != nil {
					t.Fatalf("JiraUpdateIssue: %v", err)
				}
				arts := listConversationArtifacts(t, stores, info.ConversationID)
				if len(arts) != 1 {
					t.Fatalf("want 1 artifact, got %d: %+v", len(arts), arts)
				}
				a := arts[0]
				if a.Kind != domain.ArtifactKindIssue || a.Target != "SKY-9" ||
					a.State != domain.ArtifactStateIssueUpdated || a.DedupKey != "jira:issue:SKY-9" {
					t.Errorf("update artifact mismatch: %+v", a)
				}
				if !strings.Contains(a.DetailsJSON, `"summary"`) || !strings.Contains(a.DetailsJSON, `"priority"`) {
					t.Errorf("details_json %q did not list the updated fields", a.DetailsJSON)
				}
			})

			t.Run("set-parent", func(t *testing.T) {
				jira := startFakeJira(t)
				stores, info := newJiraRecordingStores(t, jira.URL, eventTriggered)
				client := NewLocal(stores, info)

				if err := client.JiraSetParent(context.Background(), "SKY-9", "SKY-1"); err != nil {
					t.Fatalf("JiraSetParent: %v", err)
				}
				arts := listConversationArtifacts(t, stores, info.ConversationID)
				if len(arts) != 1 || arts[0].Kind != domain.ArtifactKindIssue ||
					arts[0].DedupKey != "jira:issue:SKY-9" || arts[0].State != domain.ArtifactStateIssueUpdated {
					t.Fatalf("set-parent artifact mismatch: %+v", arts)
				}
				if !strings.Contains(arts[0].DetailsJSON, `"parent":"SKY-1"`) {
					t.Errorf("details_json %q did not carry the parent", arts[0].DetailsJSON)
				}
			})

			t.Run("set-priority", func(t *testing.T) {
				jira := startFakeJira(t)
				stores, info := newJiraRecordingStores(t, jira.URL, eventTriggered)
				client := NewLocal(stores, info)

				if err := client.JiraSetPriority(context.Background(), "SKY-9", "Low"); err != nil {
					t.Fatalf("JiraSetPriority: %v", err)
				}
				arts := listConversationArtifacts(t, stores, info.ConversationID)
				if len(arts) != 1 || arts[0].DedupKey != "jira:issue:SKY-9" ||
					arts[0].State != domain.ArtifactStateIssueUpdated {
					t.Fatalf("set-priority artifact mismatch: %+v", arts)
				}
				if !strings.Contains(arts[0].DetailsJSON, `"priority":"Low"`) {
					t.Errorf("details_json %q did not carry the priority", arts[0].DetailsJSON)
				}
			})
		})
	}
}

// TestLocalClient_JiraArtifactURLs_WhenSiteConfigured pins that with the org's
// Jira site URL set, issue artifacts carry {site}/browse/<KEY> and comment
// artifacts carry the focusedCommentId deep link.
func TestLocalClient_JiraArtifactURLs_WhenSiteConfigured(t *testing.T) {
	jira := startFakeJira(t)
	stores, info := newJiraRecordingStores(t, jira.URL, true)
	if _, err := stores.Orgs.UpdateSettings(context.Background(), runmode.LocalDefaultOrgID, domain.OrgSettings{
		JiraBaseURL: "https://acme.atlassian.net",
	}); err != nil {
		t.Fatalf("set site URL: %v", err)
	}
	client := NewLocal(stores, info)
	ctx := context.Background()

	if err := client.JiraTransitionTo(ctx, "SKY-7", "Done"); err != nil {
		t.Fatalf("JiraTransitionTo: %v", err)
	}
	if err := client.JiraAddComment(ctx, "SKY-7", "shipping it"); err != nil {
		t.Fatalf("JiraAddComment: %v", err)
	}

	for _, a := range listConversationArtifacts(t, stores, info.ConversationID) {
		switch a.Kind {
		case domain.ArtifactKindIssue:
			if a.URL != "https://acme.atlassian.net/browse/SKY-7" {
				t.Errorf("issue url = %q", a.URL)
			}
		case domain.ArtifactKindComment:
			if a.URL != "https://acme.atlassian.net/browse/SKY-7?focusedCommentId=10042" {
				t.Errorf("comment url = %q", a.URL)
			}
		}
	}
}

// TestLocalClient_JiraComment_NoID_SkipsArtifact pins that when Jira's response
// carries no comment id, the comment still posts (action succeeds) but no
// artifact row is written — there's no stable key to dedup on.
func TestLocalClient_JiraComment_NoID_SkipsArtifact(t *testing.T) {
	jira := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Comment POST returns a body with no id field.
		_, _ = io.WriteString(w, `{}`)
	}))
	t.Cleanup(jira.Close)

	stores, info := newJiraRecordingStores(t, jira.URL, true)
	client := NewLocal(stores, info)

	if err := client.JiraAddComment(context.Background(), "SKY-9", "no id back"); err != nil {
		t.Fatalf("JiraAddComment must succeed even when the id is missing: %v", err)
	}
	if arts := listConversationArtifacts(t, stores, info.ConversationID); len(arts) != 0 {
		t.Errorf("expected no artifact when comment id is empty, got %+v", arts)
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

	arts := listConversationArtifacts(t, stores, info.ConversationID)
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
