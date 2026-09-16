package routing

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	jiraclient "github.com/sky-ai-eng/triage-factory/internal/jira"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/tracker"
)

// No live bus delivery: routing must recover the poller's committed outbox.
type bodyTestPublisher struct{}

func (bodyTestPublisher) Publish(context.Context, domain.Event)            {}
func (bodyTestPublisher) PublishPreEnqueued(context.Context, domain.Event) {}

func TestBodyUpdated_PollToRule(t *testing.T) {
	for _, source := range []string{"github", "jira"} {
		t.Run(source, func(t *testing.T) {
			ctx := t.Context()
			database := newTestDB(t)
			seedHandlerFKTargets(t, database)
			st := sqlitestore.New(database)
			org, team := runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID
			eventType, sourceID, kind := domain.EventGitHubPRBodyUpdated, "owner/repo#7", "pr"
			if source == "github" {
				setReviewHost(t, database)
				seedUserOnTeam(t, database, team, "author")
			} else {
				eventType, sourceID, kind = domain.EventJiraIssueBodyUpdated, "PROJ-7", "issue"
				setJiraHost(t, database)
				seedJiraUserOnTeam(t, database, team, "account-7", "Assignee")
			}
			entity, _, err := st.Entities.FindOrCreate(ctx, org, source, sourceID, kind, "", "")
			if err != nil {
				t.Fatal(err)
			}
			var mu sync.Mutex
			body := "Original body"
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				mu.Lock()
				raw, _ := json.Marshal(body)
				mu.Unlock()
				switch {
				case source == "github" && strings.HasSuffix(req.URL.Path, "/graphql"):
					_, _ = fmt.Fprintf(w, `{"data":{"nodes":[{"id":"PR_7","number":7,"title":"Body routing","body":%s,"author":{"login":"author"},"state":"OPEN","repository":{"nameWithOwner":"owner/repo"},"labels":{"nodes":[{"name":"attention"}]},"updatedAt":"2026-09-15T12:00:00Z"}]}}`, raw)
				case source == "github" && strings.HasSuffix(req.URL.Path, "/pulls/7"):
					_, _ = w.Write([]byte(`{"number":7,"node_id":"PR_7","state":"open"}`))
				case source == "jira" && strings.HasSuffix(req.URL.Path, "/search"):
					var query struct {
						JQL string `json:"jql"`
					}
					if err := json.NewDecoder(req.Body).Decode(&query); err != nil {
						t.Error(err)
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					if !strings.Contains(query.JQL, "key IN") {
						_, _ = w.Write([]byte(`{"issues":[],"total":0}`))
						return
					}
					_, _ = fmt.Fprintf(w, `{"issues":[{"key":"PROJ-7","fields":{"summary":"Body routing","description":%s,"status":{"name":"Open"},"assignee":{"accountId":"account-7","displayName":"Assignee"},"labels":["attention"],"updated":"2026-09-15T12:00:00.000+0000"}}],"total":1}`, raw)
				default:
					t.Errorf("unexpected provider request: %s", req.URL.Path)
					http.NotFound(w, req)
				}
			}))
			t.Cleanup(srv.Close)
			tr := tracker.New(database, bodyTestPublisher{}, st.Tasks, st.Entities, st.Repos, st.EventQueue, org)
			gh := ghclient.NewClient(srv.URL, "test-token")
			jira := jiraclient.NewClient(jiraclient.DataCenterPAT(srv.URL, "test-token"))
			poll := func(next string) {
				t.Helper()
				mu.Lock()
				body = next
				mu.Unlock()
				var err error
				if source == "github" {
					_, _, err = tr.RefreshGitHub(ctx, gh, "", nil, nil)
				} else {
					_, err = tr.RefreshJira(ctx, jira, srv.URL, tracker.JiraRules{{Key: "PROJ"}})
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			check := func(events, tasks int) {
				t.Helper()
				if got := countRows(t, database, `SELECT COUNT(*) FROM events`); got != events {
					t.Fatalf("recorded events = %d, want %d", got, events)
				}
				if got := countRows(t, database, `SELECT COUNT(*) FROM tasks`); got != tasks {
					t.Fatalf("tasks = %d, want %d", got, tasks)
				}
			}
			poll(body)
			if got := countRows(t, database, `SELECT COUNT(*) FROM event_queue`); got != 0 {
				t.Fatalf("baseline queued %d events", got)
			}
			poll("Edit before a rule exists")
			if got := countRows(t, database, `SELECT COUNT(*) FROM event_queue WHERE status='pending'`); got != 1 {
				t.Fatalf("pending body events = %d, want 1", got)
			}
			// A fresh router discovers the edit without an in-memory signal.
			router := reviewRouter(database)
			router.SetEventQueue(st.EventQueue)
			router.drainEventQueue(ctx)
			check(1, 0)
			if _, err := database.Exec(`INSERT INTO event_handlers
				(id, org_id, team_id, creator_user_id, kind, event_type, scope_predicate_json,
				 enabled, source, name, default_priority, sort_order, created_at, updated_at)
				VALUES ('body-rule', ?, ?, ?, 'rule', ?, '{"has_label":"other"}',
				 1, 'user', 'Body edits', 0.6, 100, ?, ?)`, org, team, runmode.LocalDefaultUserID, eventType, time.Now(), time.Now()); err != nil {
				t.Fatal(err)
			}
			poll("Edit with a mismatching predicate")
			router.drainEventQueue(ctx)
			check(2, 0)
			if _, err := database.Exec(`UPDATE event_handlers SET scope_predicate_json='{"has_label":"attention"}' WHERE id='body-rule'`); err != nil {
				t.Fatal(err)
			}
			router.drainEventQueue(ctx)
			check(2, 0) // Enabling a match does not replay historical edits.
			poll("Edit matching the rule")
			router.drainEventQueue(ctx)
			check(3, 1)
			active, err := st.Tasks.FindActiveByEntity(ctx, org, entity.ID)
			if err != nil || len(active) != 1 || active[0].EventType != eventType {
				t.Fatalf("body task = %v, err=%v", active, err)
			}
			if visible := visTeamsOf(t, database, active[0].ID); len(visible) != 1 || visible[0] != team {
				t.Fatalf("body task visibility = %v, want owner team", visible)
			}
			poll("Edit matching the rule")
			router.drainEventQueue(ctx)
			router.drainEventQueue(ctx)
			check(3, 1)
			if got := countRows(t, database, `SELECT COUNT(*) FROM event_queue WHERE status='done'`); got != 3 {
				t.Fatalf("completed queue rows = %d, want 3", got)
			}
		})
	}
}
