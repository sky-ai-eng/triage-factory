package routing

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/domain/events"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// otherGitHubHost is a GitHub deployment other than the org's current one
// (dbtest.TestGitHubHost, the host an org with no base URL resolves to).
const otherGitHubHost = "https://ghe.example.com"

// unreachableEvent is the github:pr:unreachable the tracker emits for a pull
// request polled from host after the org's GitHub host moved off it.
func unreachableEvent(t *testing.T, entityID, host, repo string) domain.Event {
	t.Helper()
	meta, _ := json.Marshal(events.GitHubPRUnreachableMetadata{
		Author: "stranger", Repo: repo, PRNumber: 1, HeadSHA: "abc123",
		Host: host, Reason: events.GitHubUnreachableScopeChanged,
	})
	return domain.Event{
		EventType:    domain.EventGitHubPRUnreachable,
		EntityID:     &entityID,
		MetadataJSON: string(meta),
		CreatedAt:    time.Now(),
		OrgID:        runmode.LocalDefaultOrgID,
	}
}

// seedPRWithTasks creates an active pull request entity on host with one open
// task per event type, and returns the entity id and the task ids by type.
func seedPRWithTasks(t *testing.T, database *sql.DB, host, sourceID string, taskTypes ...string) (string, map[string]string) {
	t.Helper()
	ctx := t.Context()
	st := sqlitestore.New(database)
	entity, _, err := st.Entities.FindOrCreate(ctx, runmode.LocalDefaultOrgID, "github", host, sourceID, "", "pr", sourceID, "")
	if err != nil {
		t.Fatalf("create entity %s: %v", sourceID, err)
	}
	tasks := map[string]string{}
	for _, eventType := range taskTypes {
		evtID, err := st.Events.RecordSystem(ctx, runmode.LocalDefaultOrgID, domain.Event{
			OrgID: runmode.LocalDefaultOrgID, EntityID: &entity.ID, EventType: eventType, MetadataJSON: "{}",
		})
		if err != nil {
			t.Fatalf("record event %s: %v", eventType, err)
		}
		task, _, err := testTaskStore(database).FindOrCreateAtSystem(ctx, runmode.LocalDefaultOrgID,
			runmode.LocalDefaultTeamID, entity.ID, eventType, "", evtID, 0.5, time.Now())
		if err != nil {
			t.Fatalf("create task %s: %v", eventType, err)
		}
		tasks[eventType] = task.ID
	}
	return entity.ID, tasks
}

// TestGitHubUnreachable_ClosesEntityAndTasks: github:pr:unreachable closes the
// entity and its open tasks the way github:pr:closed does. The one difference
// is the type each spares: closed spares the merged and closed riders a
// terminating close leaves standing, unreachable spares only its own, so a task
// riding an earlier close on a PR TF can no longer observe closes too.
func TestGitHubUnreachable_ClosesEntityAndTasks(t *testing.T) {
	closedEvent := func(t *testing.T, entityID string) domain.Event {
		meta, _ := json.Marshal(events.GitHubPRClosedMetadata{Author: "stranger", Repo: "owner/repo", PRNumber: 1})
		return domain.Event{
			EventType: domain.EventGitHubPRClosed, EntityID: &entityID, MetadataJSON: string(meta),
			OccurredAt: time.Now(), CreatedAt: time.Now(), OrgID: runmode.LocalDefaultOrgID,
		}
	}
	cases := []struct {
		name         string
		host         string
		event        func(t *testing.T, entityID string) domain.Event
		closesARider bool
	}{
		{
			name:         "closed",
			host:         dbtest.TestGitHubHost,
			event:        closedEvent,
			closesARider: false,
		},
		{
			name: "unreachable",
			// The pull request is on the host the org has left.
			host: otherGitHubHost,
			event: func(t *testing.T, entityID string) domain.Event {
				return unreachableEvent(t, entityID, otherGitHubHost, "owner/repo")
			},
			closesARider: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			database := newGateDB(t)
			ctx := context.Background()
			live := []string{domain.EventGitHubPRCICheckFailed, domain.EventGitHubPRNewCommits, domain.EventGitHubPRReviewRequested}
			entityID, taskIDs := seedPRWithTasks(t, database, tc.host, "owner/repo#1",
				append(live, domain.EventGitHubPRClosed)...)

			gateRouter(database).HandleEvent(ctx, tc.event(t, entityID))

			e, err := sqlitestore.New(database).Entities.Get(ctx, runmode.LocalDefaultOrgID, entityID)
			if err != nil || e == nil || e.State != "closed" {
				t.Fatalf("entity = %+v err=%v, want closed", e, err)
			}
			for _, et := range live {
				if status, reason := taskCloseReason(t, database, taskIDs[et]); status != "done" || reason != "entity_closed" {
					t.Errorf("%s task = (%s, %s), want (done, entity_closed)", et, status, reason)
				}
			}
			status, _ := taskCloseReason(t, database, taskIDs[domain.EventGitHubPRClosed])
			if closed := status == "done"; closed != tc.closesARider {
				t.Errorf("rider of an earlier close is %s; closed=%v, want %v", status, closed, tc.closesARider)
			}
		})
	}
}

// TestGate_HostIsPartOfTheRepoMatch: a slug names a different repository on
// another GitHub host, so the team↔repo gate matches on (host, owner, repo).
// Team A tracks acme/api on the org's current host and team B tracks acme/api on
// another; both have a match-all watch rule. An event about the other host's
// acme/api reaches team B only, and one about the current host's reaches team A
// only. The host comes from the event's metadata when it carries one
// (github:pr:unreachable), else from the scope of the entity it is about.
func TestGate_HostIsPartOfTheRepoMatch(t *testing.T) {
	cases := []struct {
		name     string
		host     string
		rule     string
		event    func(t *testing.T, entityID, host string) domain.Event
		wantTeam string // "a" or "b"
	}{
		{
			name: "unreachable on the other host, from its metadata",
			host: otherGitHubHost, rule: domain.EventGitHubPRUnreachable,
			event: func(t *testing.T, entityID, host string) domain.Event {
				return unreachableEvent(t, entityID, host, "acme/api")
			},
			wantTeam: "b",
		},
		{
			name: "unreachable on the current host, from its metadata",
			host: dbtest.TestGitHubHost, rule: domain.EventGitHubPRUnreachable,
			event: func(t *testing.T, entityID, host string) domain.Event {
				return unreachableEvent(t, entityID, host, "acme/api")
			},
			wantTeam: "a",
		},
		{
			name: "ci failure on the other host, from the entity's scope",
			host: otherGitHubHost, rule: domain.EventGitHubPRCICheckFailed,
			event: func(t *testing.T, entityID, _ string) domain.Event {
				return ciEvent(t, entityID, "acme/api")
			},
			wantTeam: "b",
		},
		{
			name: "ci failure on the current host, from the entity's scope",
			host: dbtest.TestGitHubHost, rule: domain.EventGitHubPRCICheckFailed,
			event: func(t *testing.T, entityID, _ string) domain.Event {
				return ciEvent(t, entityID, "acme/api")
			},
			wantTeam: "a",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			database := newGateDB(t)
			st := sqlitestore.New(database)
			ctx := context.Background()
			teams := map[string]string{"a": runmode.LocalDefaultTeamID, "b": seedGateTeam(t, database, "team-b")}
			acmeAPI := []domain.TeamGitHubRepo{{Owner: "acme", Repo: "api"}}
			if err := st.TeamGitHubRepos.ReplaceForTeam(ctx, runmode.LocalDefaultOrgID, teams["a"], dbtest.TestGitHubHost, acmeAPI); err != nil {
				t.Fatalf("team a track: %v", err)
			}
			if err := st.TeamGitHubRepos.ReplaceForTeam(ctx, runmode.LocalDefaultOrgID, teams["b"], otherGitHubHost, acmeAPI); err != nil {
				t.Fatalf("team b track: %v", err)
			}
			seedMatchAllRule(t, database, teams["a"], tc.rule)
			seedMatchAllRule(t, database, teams["b"], tc.rule)

			entity, _, err := st.Entities.FindOrCreate(ctx, runmode.LocalDefaultOrgID, "github", tc.host, "acme/api#1", "", "pr", "PR", "")
			if err != nil {
				t.Fatalf("create entity: %v", err)
			}
			gateRouter(database).HandleEvent(ctx, tc.event(t, entity.ID, tc.host))

			var taskID string
			if err := database.QueryRow(`SELECT id FROM tasks WHERE entity_id = ? AND event_type = ?`, entity.ID, tc.rule).Scan(&taskID); err != nil {
				t.Fatalf("read %s task: %v", tc.rule, err)
			}
			vis, err := testTaskStore(database).VisibilityTeams(ctx, runmode.LocalDefaultOrgID, taskID)
			if err != nil {
				t.Fatalf("VisibilityTeams: %v", err)
			}
			if want := teams[tc.wantTeam]; len(vis) != 1 || vis[0] != want {
				t.Errorf("visibility = %v, want only team %s (%s)", vis, tc.wantTeam, want)
			}
		})
	}
}

// TestGitHubEventHost_Precedence pins where the gate reads an event's GitHub
// host from: the metadata's "host" when the event carries one, else the scope
// of the entity it is about, else the org's current host.
func TestGitHubEventHost_Precedence(t *testing.T) {
	database := newGateDB(t)
	st := sqlitestore.New(database)
	ctx := context.Background()
	r := gateRouter(database)
	onOther, _, err := st.Entities.FindOrCreate(ctx, runmode.LocalDefaultOrgID, "github", otherGitHubHost, "acme/api#1", "", "pr", "PR", "")
	if err != nil {
		t.Fatalf("create entity: %v", err)
	}

	for _, tc := range []struct {
		name string
		evt  domain.Event
		want string
	}{
		{"metadata host", unreachableEvent(t, onOther.ID, "https://third.example.com", "acme/api"), "https://third.example.com"},
		{"entity scope", ciEvent(t, onOther.ID, "acme/api"), otherGitHubHost},
		{"org's current host", domain.Event{EventType: domain.EventGitHubPRCICheckFailed, MetadataJSON: `{"repo":"acme/api"}`, OrgID: runmode.LocalDefaultOrgID}, dbtest.TestGitHubHost},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := r.githubEventHost(ctx, tc.evt)
			if err != nil {
				t.Fatalf("githubEventHost: %v", err)
			}
			if got != tc.want {
				t.Errorf("host = %q, want %q", got, tc.want)
			}
		})
	}
}
