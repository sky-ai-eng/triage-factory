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
// assert that each successful action lands the right `deliverables` row. The
// LocalClient is the single seam the multi daemon dispatches through, so
// proving it here covers both the sandbox and local-mode paths.

// fakeJiraIssueID is the issue id startFakeJira gives the issue under key:
// "1" and the key's number, zero-padded to four digits (SKY-9 → 10009).
func fakeJiraIssueID(key string) string {
	_, n, _ := strings.Cut(key, "-")
	return "1" + strings.Repeat("0", max(0, 4-len(n))) + n
}

// fakeJiraIssueDedupKey is the dedup key of the issue deliverable for key on
// site, the fake Jira's URL the recording stores make the org's Jira site.
func fakeJiraIssueDedupKey(site, key string) string {
	return domain.ObjectKey(domain.ProviderJira, domain.DeliverableKindIssue,
		domain.JiraIssueResource(site, fakeJiraIssueID(key)), "")
}

// startFakeJira stands a minimal Jira REST backend covering the mutating verbs
// the recording tests exercise: transition (list + apply), comment (returns an
// id), assignee (assign/unassign), the user lookups an assign makes (the
// person fakeJiraUserKey names and nobody else), create (returns an id and a
// key), and the read-back of which issue a key names (fakeJiraIssueID).
// fakeJiraUserKey is the Data Center user key of the one person startFakeJira
// knows, and fakeJiraUser that person as Jira answers for them.
const (
	fakeJiraUserKey = "JIRAUSER10100"
	fakeJiraUser    = `{"key":"` + fakeJiraUserKey + `","name":"ana","displayName":"Ana Lyst","emailAddress":"ana@acme.test","active":true}`
)

func startFakeJira(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		path := r.URL.Path
		switch {
		case r.Method == http.MethodGet && strings.Contains(path, "/issue/") && !strings.Contains(path, "/transitions"):
			key := strings.ToUpper(path[strings.LastIndex(path, "/")+1:])
			project, _, _ := strings.Cut(key, "-")
			_, _ = io.WriteString(w, `{"id":"`+fakeJiraIssueID(key)+`","key":"`+key+`","fields":{"project":{"id":"id-`+project+`","key":"`+project+`"}}}`)
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
		case strings.HasSuffix(path, "/user/assignable/search"):
			_, _ = io.WriteString(w, `[`+fakeJiraUser+`]`)
		case strings.HasSuffix(path, "/user"):
			if r.URL.Query().Get("key") != fakeJiraUserKey {
				w.WriteHeader(http.StatusNotFound)
				_, _ = io.WriteString(w, `{"errorMessages":["The user does not exist"]}`)
				return
			}
			_, _ = io.WriteString(w, fakeJiraUser)
		case strings.HasSuffix(path, "/issue"):
			_, _ = io.WriteString(w, `{"id":"10001","key":"SKY-1"}`)
		default:
			_, _ = io.WriteString(w, `{}`)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// newJiraRecordingStores builds a real SQLite store bundle (so Deliverables / Tx /
// Orgs are live) with the Jira credential faked to point at jiraURL, seeds a
// conversation for the deliverables FK to anchor on, and returns the bundle + the
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
	recordGitHubURLForTest(t, conn)
	recordJiraSite(t, conn, jiraURL)

	const conversationID = "11111111-1111-1111-1111-111111111111"
	if _, err := conn.Exec(
		`INSERT INTO conversations (id, origin, status) VALUES (?, 'interactive', 'running')`, conversationID,
	); err != nil {
		t.Fatalf("seed conversation: %v", err)
	}

	stores := sqlitestore.New(conn)
	stores.Secrets = fakeJiraSecrets{pat: "org-pat"}
	info := ConversationInfo{
		OrgID:            runmode.LocalDefaultOrgID,
		TeamID:           runmode.LocalDefaultTeamID,
		ConversationID:   conversationID,
		IsEventTriggered: eventTriggered,
	}
	return conn, stores, info
}

func listConversationDeliverables(t *testing.T, stores db.Stores, conversationID string) []domain.Deliverable {
	t.Helper()
	arts, _, err := stores.Deliverables.ListByConversation(context.Background(), runmode.LocalDefaultOrgID, conversationID, db.Unwindowed)
	if err != nil {
		t.Fatalf("ListByConversation: %v", err)
	}
	return arts
}

// TestLocalClient_JiraActions_RecordDeliverables pins one row per successful
// action with the right kind/target/state/dedup_key, across both write paths.
func TestLocalClient_JiraActions_RecordDeliverables(t *testing.T) {
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
				arts := listConversationDeliverables(t, stores, info.ConversationID)
				if len(arts) != 1 {
					t.Fatalf("want 1 deliverable, got %d: %+v", len(arts), arts)
				}
				a := arts[0]
				if a.Provider != domain.ProviderJira || a.Kind != domain.DeliverableKindIssue ||
					a.Target != "SKY-1" || a.ExternalID != "10001" || a.State != domain.DeliverableStateIssueCreated ||
					a.DedupKey != fakeJiraIssueDedupKey(jira.URL, "SKY-1") {
					t.Errorf("create deliverable mismatch: %+v", a)
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
				arts := listConversationDeliverables(t, stores, info.ConversationID)
				if len(arts) != 1 {
					t.Fatalf("want 1 deliverable, got %d: %+v", len(arts), arts)
				}
				a := arts[0]
				if a.Kind != domain.DeliverableKindIssue || a.Target != "SKY-9" ||
					a.State != domain.DeliverableStateIssueUpdated || a.DedupKey != fakeJiraIssueDedupKey(jira.URL, "SKY-9") {
					t.Errorf("transition deliverable mismatch: %+v", a)
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
				arts := listConversationDeliverables(t, stores, info.ConversationID)
				if len(arts) != 1 {
					t.Fatalf("want 1 deliverable, got %d: %+v", len(arts), arts)
				}
				a := arts[0]
				if a.Kind != domain.DeliverableKindComment || a.Target != "SKY-9" ||
					a.ExternalID != "10042" || a.State != domain.DeliverableStateCommentPosted ||
					a.DedupKey != domain.ObjectKey(domain.ProviderJira, domain.DeliverableKindComment,
						domain.JiraIssueResource(jira.URL, "10009"), "10042") {
					t.Errorf("comment deliverable mismatch: %+v", a)
				}
				if !strings.Contains(a.DetailsJSON, "looks good to me") {
					t.Errorf("details_json %q did not carry the body snippet", a.DetailsJSON)
				}
			})

			t.Run("assign", func(t *testing.T) {
				jira := startFakeJira(t)
				stores, info := newJiraRecordingStores(t, jira.URL, eventTriggered)
				client := NewLocal(stores, info)

				user, err := client.JiraAssign(context.Background(), "SKY-9", fakeJiraUserKey)
				if err != nil {
					t.Fatalf("JiraAssign: %v", err)
				}
				if user.ID != fakeJiraUserKey || user.DisplayName != "Ana Lyst" {
					t.Errorf("assigned %+v, want Ana Lyst", user)
				}
				arts := listConversationDeliverables(t, stores, info.ConversationID)
				if len(arts) != 1 {
					t.Fatalf("want 1 deliverable, got %d: %+v", len(arts), arts)
				}
				a := arts[0]
				if a.Kind != domain.DeliverableKindIssue || a.Target != "SKY-9" ||
					a.State != domain.DeliverableStateIssueUpdated || a.DedupKey != fakeJiraIssueDedupKey(jira.URL, "SKY-9") {
					t.Errorf("assign deliverable mismatch: %+v", a)
				}
				for _, want := range []string{`"id":"` + fakeJiraUserKey + `"`, `"name":"Ana Lyst"`} {
					if !strings.Contains(a.DetailsJSON, want) {
						t.Errorf("details_json %q does not name the assignee (%s)", a.DetailsJSON, want)
					}
				}
				acts := listExternalActions(t, stores)
				if len(acts) != 1 || acts[0].Action != domain.ActionIssueAssigned || acts[0].Credential != domain.CredentialJiraOrg ||
					!strings.Contains(acts[0].DetailJSON, `"id":"`+fakeJiraUserKey+`"`) || !strings.Contains(acts[0].DetailJSON, `"name":"Ana Lyst"`) {
					t.Errorf("external actions = %+v, want one assign naming Ana under the org's Jira account", acts)
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
				arts := listConversationDeliverables(t, stores, info.ConversationID)
				if len(arts) != 1 {
					t.Fatalf("want 1 deliverable, got %d: %+v", len(arts), arts)
				}
				a := arts[0]
				if a.Kind != domain.DeliverableKindIssue || a.Target != "SKY-9" ||
					a.State != domain.DeliverableStateIssueUpdated || a.DedupKey != fakeJiraIssueDedupKey(jira.URL, "SKY-9") {
					t.Errorf("update deliverable mismatch: %+v", a)
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
				arts := listConversationDeliverables(t, stores, info.ConversationID)
				if len(arts) != 1 || arts[0].Kind != domain.DeliverableKindIssue ||
					arts[0].DedupKey != fakeJiraIssueDedupKey(jira.URL, "SKY-9") || arts[0].State != domain.DeliverableStateIssueUpdated {
					t.Fatalf("set-parent deliverable mismatch: %+v", arts)
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
				arts := listConversationDeliverables(t, stores, info.ConversationID)
				if len(arts) != 1 || arts[0].DedupKey != fakeJiraIssueDedupKey(jira.URL, "SKY-9") ||
					arts[0].State != domain.DeliverableStateIssueUpdated {
					t.Fatalf("set-priority deliverable mismatch: %+v", arts)
				}
				if !strings.Contains(arts[0].DetailsJSON, `"priority":"Low"`) {
					t.Errorf("details_json %q did not carry the priority", arts[0].DetailsJSON)
				}
			})
		})
	}
}

// TestLocalClient_JiraDeliverableURLs_WhenSiteConfigured pins that issue
// deliverables carry {site}/browse/<KEY> on the org's Jira site and comment
// deliverables carry the focusedCommentId deep link.
func TestLocalClient_JiraDeliverableURLs_WhenSiteConfigured(t *testing.T) {
	jira := startFakeJira(t)
	stores, info := newJiraRecordingStores(t, jira.URL, true)
	client := NewLocal(stores, info)
	ctx := context.Background()

	if err := client.JiraTransitionTo(ctx, "SKY-7", "Done"); err != nil {
		t.Fatalf("JiraTransitionTo: %v", err)
	}
	if err := client.JiraAddComment(ctx, "SKY-7", "shipping it"); err != nil {
		t.Fatalf("JiraAddComment: %v", err)
	}

	for _, a := range listConversationDeliverables(t, stores, info.ConversationID) {
		switch a.Kind {
		case domain.DeliverableKindIssue:
			if a.URL != jira.URL+"/browse/SKY-7" {
				t.Errorf("issue url = %q", a.URL)
			}
		case domain.DeliverableKindComment:
			if a.URL != jira.URL+"/browse/SKY-7?focusedCommentId=10042" {
				t.Errorf("comment url = %q", a.URL)
			}
		}
	}
}

// TestLocalClient_JiraComment_NoID_SkipsDeliverable pins that when Jira's response
// carries no comment id, the comment still posts (action succeeds) but no
// deliverable row is written — there's no stable key to dedup on.
func TestLocalClient_JiraComment_NoID_SkipsDeliverable(t *testing.T) {
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
	if arts := listConversationDeliverables(t, stores, info.ConversationID); len(arts) != 0 {
		t.Errorf("expected no deliverable when comment id is empty, got %+v", arts)
	}
}

// TestLocalClient_JiraTransitionThenComment_DedupsIssue pins the upsert
// invariant: a transition and a comment on the same issue leave exactly one
// `issue` deliverable (deduped on the issue's site and id) plus one `comment`
// deliverable.
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

	arts := listConversationDeliverables(t, stores, info.ConversationID)
	if len(arts) != 2 {
		t.Fatalf("want 2 deliverables (one issue, one comment), got %d: %+v", len(arts), arts)
	}
	var issues, comments int
	for _, a := range arts {
		switch a.Kind {
		case domain.DeliverableKindIssue:
			issues++
			if a.DedupKey != fakeJiraIssueDedupKey(jira.URL, "SKY-5") {
				t.Errorf("issue dedup_key = %q", a.DedupKey)
			}
		case domain.DeliverableKindComment:
			comments++
			if a.DedupKey != domain.ObjectKey(domain.ProviderJira, domain.DeliverableKindComment,
				domain.JiraIssueResource(jira.URL, "10005"), "10042") {
				t.Errorf("comment dedup_key = %q", a.DedupKey)
			}
		default:
			t.Errorf("unexpected deliverable kind %q", a.Kind)
		}
	}
	if issues != 1 || comments != 1 {
		t.Errorf("want 1 issue + 1 comment, got %d issue / %d comment", issues, comments)
	}
}

// erroringDeliverables fails every write so the best-effort path is exercised.
type erroringDeliverables struct {
	db.DeliverableStore
	mu    sync.Mutex
	calls int
}

func (e *erroringDeliverables) Upsert(context.Context, string, domain.Deliverable) (domain.Deliverable, error) {
	e.mu.Lock()
	e.calls++
	e.mu.Unlock()
	return domain.Deliverable{}, errors.New("boom")
}

func (e *erroringDeliverables) UpsertSystem(context.Context, string, domain.Deliverable) (domain.Deliverable, error) {
	e.mu.Lock()
	e.calls++
	e.mu.Unlock()
	return domain.Deliverable{}, errors.New("boom")
}

func (e *erroringDeliverables) callCount() int {
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
	rec := &erroringDeliverables{}
	stores.Deliverables = rec
	client := NewLocal(stores, info)

	if err := client.JiraTransitionTo(context.Background(), "SKY-9", "Done"); err != nil {
		t.Fatalf("transition must not fail when recording fails: %v", err)
	}
	if rec.callCount() == 0 {
		t.Fatal("expected the recorder to have been called (and to have failed)")
	}
}
