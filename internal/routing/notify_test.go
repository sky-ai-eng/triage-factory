package routing

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/domain/events"
	"github.com/sky-ai-eng/triage-factory/internal/notify/notifytest"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/pkg/websocket"
)

// seedNotifyRule inserts an enabled, match-all notify rule on eventType for
// teamID and returns its id.
func seedNotifyRule(t *testing.T, database *sql.DB, teamID, eventType, audience string) string {
	t.Helper()
	id := uuid.New().String()
	if _, err := database.Exec(`
		INSERT INTO event_handlers
			(id, org_id, team_id, creator_user_id, kind, event_type, enabled, source, name, notify_audience, created_at, updated_at)
		VALUES (?, ?, ?, ?, 'notify', ?, 1, 'user', ?, ?, ?, ?)
	`, id, runmode.LocalDefaultOrgID, teamID, runmode.LocalDefaultUserID, eventType, "notify "+eventType, audience, time.Now(), time.Now()); err != nil {
		t.Fatalf("seed notify rule: %v", err)
	}
	return id
}

// notifyFixture is a gate DB whose local team tracks owner/repo, with a PR
// entity there, and a router with every tracking store wired.
func notifyFixture(t *testing.T) (*sql.DB, *Router, string) {
	t.Helper()
	database := newGateDB(t)
	st := sqlitestore.New(database)
	ctx := context.Background()
	if err := dbtest.TrackRepos(ctx, st.Repos, st.TeamGitHubRepos, runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, dbtest.TestGitHubHost,
		[]domain.TeamGitHubRepo{{Owner: "owner", Repo: "repo"}}); err != nil {
		t.Fatalf("track repo: %v", err)
	}
	entity, _, err := st.Entities.FindOrCreate(ctx, runmode.LocalDefaultOrgID, "github", dbtest.TestGitHubHost, "owner/repo#7", "", "pr", "Fix it", "https://github.com/owner/repo/pull/7")
	if err != nil {
		t.Fatalf("create entity: %v", err)
	}
	return database, gateRouter(database), entity.ID
}

func githubEvent(entityID, eventType, dedupKey string, meta any) domain.Event {
	raw, _ := json.Marshal(meta)
	return domain.Event{
		OrgID: runmode.LocalDefaultOrgID, EntityID: &entityID, EventType: eventType,
		DedupKey: dedupKey, MetadataJSON: string(raw), CreatedAt: time.Now(),
	}
}

// A notify rule on a type a typed close relation closes raises a condition
// naming the rule, with no task when no rule or trigger matched; the relation
// that closes the task closes the notification, on the same gate: a pass with
// a check still failing leaves it, a green suite resolves it.
func TestNotify_ConditionRaisedAndResolvedByItsCloseRelation(t *testing.T) {
	rec := notifytest.Record(t)
	database, r, entityID := notifyFixture(t)
	ctx := context.Background()
	ruleID := seedNotifyRule(t, database, runmode.LocalDefaultTeamID, domain.EventGitHubPRCICheckFailed, domain.NotifyAudienceCreator)

	if err := r.HandleEvent(ctx, ciEvent(t, entityID, "owner/repo")); err != nil {
		t.Fatalf("route ci_check_failed: %v", err)
	}
	got := rec.Events(runmode.LocalDefaultOrgID)
	if len(got) != 1 || !got[0].Condition || got[0].EntityID != entityID || got[0].EventType != domain.EventGitHubPRCICheckFailed ||
		got[0].DedupKey != "build" || len(got[0].Raise.Rules) != 1 || got[0].Raise.Rules[0].HandlerID != ruleID ||
		got[0].Raise.Rules[0].CreatorUserID != runmode.LocalDefaultUserID || got[0].Raise.Source != "github" {
		t.Fatalf("raised %+v; want one condition about the entity naming the rule and its creator", got)
	}
	if n := activeTaskCount(t, database, entityID); n != 0 {
		t.Errorf("active tasks = %d; want none, a notify rule makes no task", n)
	}

	passed := events.GitHubPRCICheckPassedMetadata{Author: "aidan", CheckName: "lint", Conclusion: "success", Repo: "owner/repo"}
	seedPRCheckRuns(t, database, entityID,
		domain.CheckRun{ID: 1, Name: "build", Status: "completed", Conclusion: "failure"},
		domain.CheckRun{ID: 2, Name: "lint", Status: "completed", Conclusion: "success"})
	if err := r.HandleEvent(ctx, githubEvent(entityID, domain.EventGitHubPRCICheckPassed, "", passed)); err != nil {
		t.Fatalf("route a sibling's pass: %v", err)
	}
	if got := rec.Events(runmode.LocalDefaultOrgID); len(got) != 1 {
		t.Fatalf("after a pass with build still failing: %+v; want the condition open", got)
	}

	seedPRCheckRuns(t, database, entityID,
		domain.CheckRun{ID: 3, Name: "build", Status: "completed", Conclusion: "success"},
		domain.CheckRun{ID: 2, Name: "lint", Status: "completed", Conclusion: "success"})
	if err := r.HandleEvent(ctx, githubEvent(entityID, domain.EventGitHubPRCICheckPassed, "", passed)); err != nil {
		t.Fatalf("route the recovering pass: %v", err)
	}
	if got := rec.Events(runmode.LocalDefaultOrgID); len(got) != 0 {
		t.Errorf("after a green suite: %+v; want the condition resolved", got)
	}
}

// A type no typed close relation closes posts a notice; a terminating close
// resolves the entity's open conditions and leaves notices to expire.
func TestNotify_NoticeAndTerminatingClose(t *testing.T) {
	rec := notifytest.Record(t)
	database, r, entityID := notifyFixture(t)
	ctx := context.Background()
	seedNotifyRule(t, database, runmode.LocalDefaultTeamID, domain.EventGitHubPRCICheckFailed, domain.NotifyAudienceTeam)
	seedNotifyRule(t, database, runmode.LocalDefaultTeamID, domain.EventGitHubPRReviewCommented, domain.NotifyAudienceCreator)

	if err := r.HandleEvent(ctx, ciEvent(t, entityID, "owner/repo")); err != nil {
		t.Fatalf("route ci_check_failed: %v", err)
	}
	commented := events.GitHubPRReviewCommentedMetadata{Author: "aidan", Reviewer: "bob", ReviewID: 9, Repo: "owner/repo"}
	if err := r.HandleEvent(ctx, githubEvent(entityID, domain.EventGitHubPRReviewCommented, "", commented)); err != nil {
		t.Fatalf("route review_commented: %v", err)
	}
	got := rec.Events(runmode.LocalDefaultOrgID)
	if len(got) != 2 || got[0].EventType != domain.EventGitHubPRCICheckFailed || !got[0].Condition ||
		got[1].EventType != domain.EventGitHubPRReviewCommented || got[1].Condition {
		t.Fatalf("raised %+v; want the CI condition and the review notice", got)
	}

	merged := events.GitHubPRMergedMetadata{Author: "aidan", Repo: "owner/repo", PRNumber: 7, MergedBy: "aidan", HeadSHA: "abc"}
	if err := r.HandleEvent(ctx, githubEvent(entityID, domain.EventGitHubPRMerged, "", merged)); err != nil {
		t.Fatalf("route pr_merged: %v", err)
	}
	if got := rec.Events(runmode.LocalDefaultOrgID); len(got) != 1 || got[0].EventType != domain.EventGitHubPRReviewCommented {
		t.Errorf("after the merge: %+v; want only the notice left", got)
	}
}

// The notify gate fails closed where the task gate fails open. A rule whose
// team does not track the entity's repository tells nobody; with no tracking
// store wired, nothing can be shown, so a notify rule tells nobody while a
// rule on the same event still makes its task.
func TestNotify_GateFailsClosed(t *testing.T) {
	rec := notifytest.Record(t)
	database, r, _ := notifyFixture(t)
	ctx := context.Background()
	st := sqlitestore.New(database)
	seedNotifyRule(t, database, runmode.LocalDefaultTeamID, domain.EventGitHubPRCICheckFailed, domain.NotifyAudienceCreator)

	untracked, _, err := st.Entities.FindOrCreate(ctx, runmode.LocalDefaultOrgID, "github", dbtest.TestGitHubHost, "other/repo#1", "", "pr", "x", "https://github.com/other/repo/pull/1")
	if err != nil {
		t.Fatalf("create entity: %v", err)
	}
	if err := r.HandleEvent(ctx, ciEvent(t, untracked.ID, "other/repo")); err != nil {
		t.Fatalf("route: %v", err)
	}
	if got := rec.Events(runmode.LocalDefaultOrgID); len(got) != 0 {
		t.Fatalf("raised %+v for a repository the team does not track; want nothing", got)
	}

	seedMatchAllCIRule(t, database, runmode.LocalDefaultTeamID)
	r.teamRepos = nil
	if err := r.HandleEvent(ctx, ciEvent(t, untracked.ID, "other/repo")); err != nil {
		t.Fatalf("route with no tracking store: %v", err)
	}
	if got := rec.Events(runmode.LocalDefaultOrgID); len(got) != 0 {
		t.Errorf("raised %+v with no tracking store; want nothing", got)
	}
	if n := activeTaskCount(t, database, untracked.ID); n != 1 {
		t.Errorf("active tasks = %d; want the rule's task, whose gate fails open", n)
	}
}

// A reassignment resolves the old assignment's notification: a notification
// has no owning team for the member-aware skip to keep it on.
func TestNotify_ReassignKeepsNoNotification(t *testing.T) {
	keep := keepReassign(domain.EventJiraIssueAvailable)
	newOwners := map[string]struct{}{"team-a": {}}
	if !keep(domain.Event{}, newOwners, closeTarget{eventType: domain.EventJiraIssueAssigned}) {
		t.Error("an assigned notification is kept; want it resolved")
	}
	if keep(domain.Event{}, newOwners, closeTarget{eventType: domain.EventJiraIssueAssigned, ownerTeam: "team-a"}) {
		t.Error("an assigned task still owned by the new assignee's team is closed; want it kept")
	}
}

// A notify rule raises once the event has routed. A failure in the task phase
// replays the event; raising before it would tell people once per attempt.
func TestNotify_RaisesOnlyOnceRoutingSucceeds(t *testing.T) {
	rec := notifytest.Record(t)
	database, r, entityID := notifyFixture(t)
	ctx := context.Background()
	seedNotifyRule(t, database, runmode.LocalDefaultTeamID, domain.EventGitHubPRCICheckFailed, domain.NotifyAudienceCreator)
	seedMatchAllCIRule(t, database, runmode.LocalDefaultTeamID)

	st := sqlitestore.New(database)
	failing := NewRouter(testPromptStore(database), testBlueprintStore(database), testEventHandlerStore(database), nil, nil, st.Users,
		erroringTaskStore{testTaskStore(database)}, st.Conversations, st.Entities, st.PendingFirings, st.Events, st.ExternalActions,
		st.Orgs, st.Teams, st.TeamGitHubRepos, st.JiraStatusRules, nil, nil, nil, noopScorer{}, websocket.NewHub())
	if err := failing.HandleEvent(ctx, ciEvent(t, entityID, "owner/repo")); err == nil {
		t.Fatal("route with a failing task store = nil; want the upsert's error")
	}
	if got := rec.Events(runmode.LocalDefaultOrgID); len(got) != 0 {
		t.Fatalf("raised %+v on an attempt that failed; want nothing until the replay", got)
	}

	if err := r.HandleEvent(ctx, ciEvent(t, entityID, "owner/repo")); err != nil {
		t.Fatalf("replay: %v", err)
	}
	if got := rec.Events(runmode.LocalDefaultOrgID); len(got) != 1 || got[0].Seen != 1 {
		t.Errorf("after the replay: %+v; want one raise", got)
	}
}

// A close relation with only notifications to resolve has no routing
// obligation: a prepare that fails leaves them open and the event routes.
func TestNotify_NotificationOnlyCloseNeverFailsRouting(t *testing.T) {
	rec := notifytest.Record(t)
	database, r, entityID := notifyFixture(t)
	ctx := context.Background()
	seedNotifyRule(t, database, runmode.LocalDefaultTeamID, domain.EventGitHubPRCICheckFailed, domain.NotifyAudienceCreator)
	if err := r.HandleEvent(ctx, ciEvent(t, entityID, "owner/repo")); err != nil {
		t.Fatalf("route ci_check_failed: %v", err)
	}

	prev := closeRelations
	t.Cleanup(func() { closeRelations = prev })
	closeRelations = []closeRelation{{
		onEvents: []string{domain.EventGitHubPRCICheckPassed},
		closes:   []string{domain.EventGitHubPRCICheckFailed},
		prepare: func(context.Context, *Router, string, domain.Event, string) (closeContext, bool, error) {
			return nil, false, errors.New("snapshot unreadable")
		},
	}}
	passed := events.GitHubPRCICheckPassedMetadata{Author: "aidan", CheckName: "build", Conclusion: "success", Repo: "owner/repo"}
	if _, err := r.runCloses(ctx, runmode.LocalDefaultOrgID, githubEvent(entityID, domain.EventGitHubPRCICheckPassed, "", passed), entityID); err != nil {
		t.Errorf("runCloses = %v; want nil, no task waits on this close", err)
	}
	if got := rec.Events(runmode.LocalDefaultOrgID); len(got) != 1 {
		t.Errorf("after a failed prepare: %+v; want the condition left open", got)
	}
}

// The sources the routing gate decides for itself are exactly the ones a
// notify rule may be written on: a source the create route admits but the gate
// cannot decide would match nobody, and one the gate decides but the route
// refuses would have a strict answer nothing reads.
func TestNotify_CoreGateCoversNotifyRuleSources(t *testing.T) {
	r := &Router{}
	for _, et := range domain.AllEventTypes() {
		_, err := r.teamScopeVerdict(context.Background(), domain.Event{EventType: et.ID}, "team")
		if gated := !errors.Is(err, errNoCoreGate); gated != domain.NotifyRuleSource(et.ID) {
			t.Errorf("%s: core gate = %v, NotifyRuleSource = %v", et.ID, gated, domain.NotifyRuleSource(et.ID))
		}
	}
}
