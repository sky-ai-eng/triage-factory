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

// The recording tests drive the LocalClient's host-routed Jira mutations
// against a fake Jira backend with a *real* SQLite store bundle, and assert
// that each successful action lands the right external_actions row and no
// deliverable. The
// LocalClient is the single seam the multi daemon dispatches through, so
// proving it here covers both the sandbox and local-mode paths.

// fakeJiraIssueID is the issue id startFakeJira gives the issue under key:
// "1" and the key's number, zero-padded to four digits (SKY-9 → 10009).
func fakeJiraIssueID(key string) string {
	_, n, _ := strings.Cut(key, "-")
	return "1" + strings.Repeat("0", max(0, 4-len(n))) + n
}

// fakeJiraIssueObjectKey is the object key of the issue under key on site, the
// fake Jira's URL the recording stores make the org's Jira site.
func fakeJiraIssueObjectKey(site, key string) string {
	return domain.ObjectKey(domain.ProviderJira, domain.ObjectKindIssue,
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

// onlyJiraAction is the one ledger row a Jira write recorded, failing the test
// when there is not exactly one, or when the write left a deliverable.
func onlyJiraAction(t *testing.T, stores db.Stores, info ConversationInfo) domain.ExternalAction {
	t.Helper()
	if arts := listConversationDeliverables(t, stores, info.ConversationID); len(arts) != 0 {
		t.Errorf("the write left deliverables %+v, want none", arts)
	}
	acts := listExternalActions(t, stores)
	if len(acts) != 1 {
		t.Fatalf("want 1 action, got %d: %+v", len(acts), acts)
	}
	a := acts[0]
	if a.Provider != domain.ProviderJira || a.Credential != domain.CredentialJiraOrg ||
		a.ConversationID != info.ConversationID || a.TeamID != runmode.LocalDefaultTeamID {
		t.Errorf("action attribution mismatch: %+v", a)
	}
	return a
}

// TestLocalClient_JiraActions_RecordTheirLedgerRows pins one ledger row per
// successful action with the right action, target and object, and no
// deliverable, across both write paths.
func TestLocalClient_JiraActions_RecordTheirLedgerRows(t *testing.T) {
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
				a := onlyJiraAction(t, stores, info)
				if a.Action != domain.ActionIssueCreated || a.Target != "SKY-1" || a.ExternalID != "10001" ||
					a.ObjectKind != domain.ObjectKindIssue || a.ObjectKey != fakeJiraIssueObjectKey(jira.URL, "SKY-1") {
					t.Errorf("create action mismatch: %+v", a)
				}
			})

			t.Run("transition", func(t *testing.T) {
				jira := startFakeJira(t)
				stores, info := newJiraRecordingStores(t, jira.URL, eventTriggered)
				client := NewLocal(stores, info)

				if err := client.JiraTransitionTo(context.Background(), "SKY-9", "Done"); err != nil {
					t.Fatalf("JiraTransitionTo: %v", err)
				}
				a := onlyJiraAction(t, stores, info)
				if a.Action != domain.ActionIssueTransitioned || a.Target != "SKY-9" || a.ToState != "Done" ||
					a.ObjectKind != domain.ObjectKindIssue || a.ObjectKey != fakeJiraIssueObjectKey(jira.URL, "SKY-9") {
					t.Errorf("transition action mismatch: %+v", a)
				}
				if !strings.Contains(a.DetailJSON, `"status":"Done"`) {
					t.Errorf("detail_json %q did not carry the new status", a.DetailJSON)
				}
			})

			t.Run("comment", func(t *testing.T) {
				jira := startFakeJira(t)
				stores, info := newJiraRecordingStores(t, jira.URL, eventTriggered)
				client := NewLocal(stores, info)

				if err := client.JiraAddComment(context.Background(), "SKY-9", "looks good to me"); err != nil {
					t.Fatalf("JiraAddComment: %v", err)
				}
				a := onlyJiraAction(t, stores, info)
				if a.Action != domain.ActionIssueCommentPosted || a.Target != "SKY-9" || a.ExternalID != "10042" ||
					a.ObjectKind != domain.ObjectKindComment ||
					a.ObjectKey != domain.ObjectKey(domain.ProviderJira, domain.ObjectKindComment,
						domain.JiraIssueResource(jira.URL, "10009"), "10042") {
					t.Errorf("comment action mismatch: %+v", a)
				}
				if !strings.Contains(a.DetailJSON, "looks good to me") {
					t.Errorf("detail_json %q did not carry the body snippet", a.DetailJSON)
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
				a := onlyJiraAction(t, stores, info)
				if a.Action != domain.ActionIssueAssigned || a.Target != "SKY-9" ||
					a.ObjectKind != domain.ObjectKindIssue || a.ObjectKey != fakeJiraIssueObjectKey(jira.URL, "SKY-9") {
					t.Errorf("assign action mismatch: %+v", a)
				}
				for _, want := range []string{`"id":"` + fakeJiraUserKey + `"`, `"name":"Ana Lyst"`} {
					if !strings.Contains(a.DetailJSON, want) {
						t.Errorf("detail_json %q does not name the assignee (%s)", a.DetailJSON, want)
					}
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
				a := onlyJiraAction(t, stores, info)
				if a.Action != domain.ActionIssueUpdated || a.Target != "SKY-9" ||
					a.ObjectKind != domain.ObjectKindIssue || a.ObjectKey != fakeJiraIssueObjectKey(jira.URL, "SKY-9") {
					t.Errorf("update action mismatch: %+v", a)
				}
				if !strings.Contains(a.DetailJSON, `"summary"`) || !strings.Contains(a.DetailJSON, `"priority"`) {
					t.Errorf("detail_json %q did not list the updated fields", a.DetailJSON)
				}
			})

			t.Run("set-parent", func(t *testing.T) {
				jira := startFakeJira(t)
				stores, info := newJiraRecordingStores(t, jira.URL, eventTriggered)
				client := NewLocal(stores, info)

				if err := client.JiraSetParent(context.Background(), "SKY-9", "SKY-1"); err != nil {
					t.Fatalf("JiraSetParent: %v", err)
				}
				a := onlyJiraAction(t, stores, info)
				if a.Action != domain.ActionIssueUpdated || a.ObjectKey != fakeJiraIssueObjectKey(jira.URL, "SKY-9") {
					t.Fatalf("set-parent action mismatch: %+v", a)
				}
				if !strings.Contains(a.DetailJSON, `"parent":"SKY-1"`) {
					t.Errorf("detail_json %q did not carry the parent", a.DetailJSON)
				}
			})

			t.Run("set-priority", func(t *testing.T) {
				jira := startFakeJira(t)
				stores, info := newJiraRecordingStores(t, jira.URL, eventTriggered)
				client := NewLocal(stores, info)

				if err := client.JiraSetPriority(context.Background(), "SKY-9", "Low"); err != nil {
					t.Fatalf("JiraSetPriority: %v", err)
				}
				a := onlyJiraAction(t, stores, info)
				if a.Action != domain.ActionIssueUpdated || a.ObjectKey != fakeJiraIssueObjectKey(jira.URL, "SKY-9") {
					t.Fatalf("set-priority action mismatch: %+v", a)
				}
				if !strings.Contains(a.DetailJSON, `"priority":"Low"`) {
					t.Errorf("detail_json %q did not carry the priority", a.DetailJSON)
				}
			})
		})
	}
}

// TestLocalClient_JiraActionURLs_WhenSiteConfigured pins that issue rows carry
// {site}/browse/<KEY> on the org's Jira site and comment rows carry the
// focusedCommentId deep link.
func TestLocalClient_JiraActionURLs_WhenSiteConfigured(t *testing.T) {
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

	acts := listExternalActions(t, stores)
	if len(acts) != 2 {
		t.Fatalf("want 2 actions, got %d: %+v", len(acts), acts)
	}
	for _, a := range acts {
		switch a.ObjectKind {
		case domain.ObjectKindIssue:
			if a.URL != jira.URL+"/browse/SKY-7" {
				t.Errorf("issue url = %q", a.URL)
			}
		case domain.ObjectKindComment:
			if a.URL != jira.URL+"/browse/SKY-7?focusedCommentId=10042" {
				t.Errorf("comment url = %q", a.URL)
			}
		default:
			t.Errorf("unexpected object kind %q on %+v", a.ObjectKind, a)
		}
	}
}

// TestLocalClient_JiraComment_NoID_RecordsTheRowNamingNoObject pins that when
// Jira's response carries no comment id, the comment still posts (action
// succeeds), and its ledger row is recorded naming no object, since there's
// no stable key to name the comment by, and counted as an unnamed comment.
func TestLocalClient_JiraComment_NoID_RecordsTheRowNamingNoObject(t *testing.T) {
	jira := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Comment POST returns a body with no id field.
		_, _ = io.WriteString(w, `{}`)
	}))
	t.Cleanup(jira.Close)
	counted := unnamedComments(t)

	stores, info := newJiraRecordingStores(t, jira.URL, true)
	client := NewLocal(stores, info)

	if err := client.JiraAddComment(context.Background(), "SKY-9", "no id back"); err != nil {
		t.Fatalf("JiraAddComment must succeed even when the id is missing: %v", err)
	}
	a := onlyJiraAction(t, stores, info)
	if a.Action != domain.ActionIssueCommentPosted || a.ObjectKind != "" || a.ObjectKey != "" {
		t.Errorf("comment action = %+v, want issue_comment_posted naming no object", a)
	}
	if want := jira.URL + "/browse/SKY-9"; a.URL != want {
		t.Errorf("url = %q, want the issue's link %q: there is no comment id to focus", a.URL, want)
	}
	if got := counted(); got["jira/issue_comment_posted"] != 1 || len(got) != 1 {
		t.Errorf("unnamed comments = %v, want one jira/issue_comment_posted", got)
	}
}

// TestLocalClient_JiraTransitionThenComment_NamesTheIssueAndTheComment pins
// that a transition and a comment on the same issue leave two ledger rows, one
// naming the issue by its site and id and one naming the comment, and no
// deliverable.
func TestLocalClient_JiraTransitionThenComment_NamesTheIssueAndTheComment(t *testing.T) {
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

	if arts := listConversationDeliverables(t, stores, info.ConversationID); len(arts) != 0 {
		t.Errorf("deliverables = %+v, want none", arts)
	}
	want := map[string]string{
		domain.ObjectKindIssue: fakeJiraIssueObjectKey(jira.URL, "SKY-5"),
		domain.ObjectKindComment: domain.ObjectKey(domain.ProviderJira, domain.ObjectKindComment,
			domain.JiraIssueResource(jira.URL, "10005"), "10042"),
	}
	acts := listExternalActions(t, stores)
	if len(acts) != 2 {
		t.Fatalf("want 2 actions, got %d: %+v", len(acts), acts)
	}
	for _, a := range acts {
		if key, ok := want[a.ObjectKind]; !ok || a.ObjectKey != key {
			t.Errorf("action %s names %q %q, want %q", a.Action, a.ObjectKind, a.ObjectKey, key)
		}
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

func (e *erroringDeliverables) InsertDeliverableIfAbsent(context.Context, string, domain.Deliverable) (bool, error) {
	e.mu.Lock()
	e.calls++
	e.mu.Unlock()
	return false, errors.New("boom")
}

func (e *erroringDeliverables) InsertDeliverableIfAbsentSystem(context.Context, string, domain.Deliverable) (bool, error) {
	e.mu.Lock()
	e.calls++
	e.mu.Unlock()
	return false, errors.New("boom")
}

func (e *erroringDeliverables) callCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.calls
}

// erroringActions fails every ledger append so the best-effort path of a write
// that records only its ledger row is exercised.
type erroringActions struct {
	db.ExternalActionStore
	mu    sync.Mutex
	calls int
}

func (e *erroringActions) Record(context.Context, string, domain.ExternalAction) error {
	e.mu.Lock()
	e.calls++
	e.mu.Unlock()
	return errors.New("boom")
}

func (e *erroringActions) RecordSystem(context.Context, string, domain.ExternalAction) error {
	e.mu.Lock()
	e.calls++
	e.mu.Unlock()
	return errors.New("boom")
}

func (e *erroringActions) callCount() int {
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
	rec := &erroringActions{}
	stores.ExternalActions = rec
	client := NewLocal(stores, info)

	if err := client.JiraTransitionTo(context.Background(), "SKY-9", "Done"); err != nil {
		t.Fatalf("transition must not fail when recording fails: %v", err)
	}
	if rec.callCount() == 0 {
		t.Fatal("expected the recorder to have been called (and to have failed)")
	}
}
