package routing

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"testing"

	dbpkg "github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbseed"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/domain/events"
	"github.com/sky-ai-eng/triage-factory/internal/notify/kinds"
	"github.com/sky-ai-eng/triage-factory/internal/notify/notifytest"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// The ranked trigger walk, end to end: the event-time path routes the event
// and runs the walk, the score lands through the store, and the re-derive
// worker runs the same walk with the score in hand. Every scenario drives both
// halves through one router, so what the event-time walk held is what the
// post-scoring walk decides.

// walkRouter is firingRouter wired for the re-derive worker as well, with
// beforeComplete (nil for none) run at the seam between the evaluation and its
// completion.
func walkRouter(t *testing.T, database *sql.DB, stub *stubDelegator, beforeComplete func()) *Router {
	t.Helper()
	deps := firingRouterDeps(database, stub)
	deps.ReDerive = hookedReDerive{TaskReDeriveStore: sqlitestore.New(database).TaskReDerive, beforeComplete: beforeComplete}
	return newTestRouter(deps)
}

// twoAuthorTeams seeds the author aidan on two teams, both opted into
// auto-delegation, and returns them in rank order: with no rule priority
// between them, the lower team id first.
func twoAuthorTeams(t *testing.T, database *sql.DB) (first, second string) {
	t.Helper()
	seedHandlerFKTargets(t, database)
	setReviewHost(t, database)
	a := seedTeam(t, database, "team-a")
	b := seedTeam(t, database, "team-b")
	seedUserOnTeam(t, database, a, "aidan")
	seedUserOnTeam(t, database, b, "aidan")
	enableTeamAutoDelegate(t, database, a, b)
	return minStr(a, b), maxStr(a, b)
}

// seedDeferredTrigger is seedImmediateTrigger deferred to a 0.5 threshold.
func seedDeferredTrigger(t *testing.T, database *sql.DB, teamID, eventType, idSuffix string) {
	t.Helper()
	seedImmediateTrigger(t, database, teamID, eventType, idSuffix)
	deferTriggerFor(t, database, teamID, "trig-"+idSuffix)
}

// ageTrigger moves a trigger's created_at by offset, an SQLite datetime
// modifier such as "-2 hours", so a test can pin which trigger is newest.
func ageTrigger(t *testing.T, database *sql.DB, triggerID, offset string) {
	t.Helper()
	if _, err := database.Exec(`UPDATE event_handlers SET created_at = datetime('now', ?) WHERE id = ?`, offset, triggerID); err != nil {
		t.Fatalf("age trigger %s: %v", triggerID, err)
	}
}

// onlyTask returns the one active task on entityID.
func onlyTask(t *testing.T, database *sql.DB, entityID string) domain.Task {
	t.Helper()
	active, err := testTaskStore(database).FindActiveByEntity(context.Background(), runmode.LocalDefaultOrgID, entityID)
	if err != nil || len(active) != 1 {
		t.Fatalf("expected 1 task on entity %s, got %d (err=%v)", entityID, len(active), err)
	}
	return active[0]
}

// firingTriggers lists the trigger ids the firings admitted for taskID name.
func firingTriggers(t *testing.T, database *sql.DB, taskID string) []string {
	t.Helper()
	var out []string
	for _, f := range firingsForTask(t, database, taskID) {
		out = append(out, f.TriggerID)
	}
	return out
}

// requireHeld asserts the event-time walk fired nothing and left the task
// unowned and unclaimed for the post-scoring walk to decide.
func requireHeld(t *testing.T, stub *stubDelegator, task domain.Task) {
	t.Helper()
	if stub.calls != 0 || task.TeamID != nil || task.ClaimedByAgentID != "" {
		t.Fatalf("after the event: %d fire(s), owner %q, claimed by %q; want the walk held — no fire, NULL owner, unclaimed",
			stub.calls, teamIDValue(&task), task.ClaimedByAgentID)
	}
}

// requireOwnedByBot asserts the task's owner consolidated onto team under the
// bot's claim.
func requireOwnedByBot(t *testing.T, database *sql.DB, taskID, team string) {
	t.Helper()
	task, err := testTaskStore(database).Get(t.Context(), runmode.LocalDefaultOrgID, taskID)
	if err != nil || task == nil {
		t.Fatalf("get task %s: %v", taskID, err)
	}
	if teamIDValue(task) != team || task.ClaimedByAgentID != runmode.LocalDefaultAgentID {
		t.Errorf("owner %q claimed by %q; want %s under the bot's claim", teamIDValue(task), task.ClaimedByAgentID, team)
	}
}

// TestRankedWalk_AuthorOnTwoTeams_AllDeferred_TopTeamFiresAfterScoring: an
// author on two teams whose triggers are all deferred gets a NULL-owned task
// at event time, and the post-scoring walk fires among the author's teams in
// rank order, exactly as the event-time walk would have: the top team fires
// and becomes the owner. The other team's trigger passed its threshold too, so
// it lost the claim and its admins are told.
func TestRankedWalk_AuthorOnTwoTeams_AllDeferred_TopTeamFiresAfterScoring(t *testing.T) {
	database := newTestDB(t)
	first, second := twoAuthorTeams(t, database)
	seedDeferredTrigger(t, database, first, domain.EventGitHubPRCICheckFailed, "ci-first")
	seedDeferredTrigger(t, database, second, domain.EventGitHubPRCICheckFailed, "ci-second")
	entityID := reviewEntity(t, database, "owner/repo#alldeferred")
	stub := &stubDelegator{db: database}
	r := walkRouter(t, database, stub, nil)
	rec := notifytest.Record(t)

	emitCI(t, r, entityID, "aidan")
	task := onlyTask(t, database, entityID)
	requireHeld(t, stub, task)

	scoreTask(t, database, task.ID, 0.9)
	drainReDeriveOnce(t, r)

	if got := firingTriggers(t, database, task.ID); !slices.Equal(got, []string{"trig-ci-first"}) {
		t.Fatalf("firings = %v, want only the top-ranked team's trig-ci-first", got)
	}
	requireOwnedByBot(t, database, task.ID, first)
	c := onlyConflict(t, rec, task.ID)
	if c.Winner.TeamID != first || c.Winner.Name == "" || c.Subject != "owner/repo#alldeferred" || c.EventType != domain.EventGitHubPRCICheckFailed {
		t.Errorf("conflict = %+v; want %s's win named, on owner/repo#alldeferred", c, first)
	}
	if len(c.Losers) != 1 || c.Losers[0] != (kinds.ConflictLoser{TeamID: second, TriggerID: "trig-ci-second"}) {
		t.Errorf("losers = %+v; want only %s, linked to trig-ci-second", c.Losers, second)
	}
}

// TestRankedWalk_AssigneeOnTwoTeams_WatcherWithNewestTrigger_MemberFires pins
// the no-steal invariant after scoring: a Jira assignee on two teams, and a
// watcher whose applies_to_unowned trigger is the newest of the three. The
// watcher sees the task, but while a member team is a candidate it is not
// one, whatever order the triggers were created in.
func TestRankedWalk_AssigneeOnTwoTeams_WatcherWithNewestTrigger_MemberFires(t *testing.T) {
	database := newTestDB(t)
	seedHandlerFKTargets(t, database)
	setJiraHost(t, database)
	a := seedTeam(t, database, "team-a")
	b := seedTeam(t, database, "team-b")
	watcher := seedTeam(t, database, "watch-team")
	seedJiraUserOnTeam(t, database, a, "acct-shared", "aidan-a")
	seedJiraUserOnTeam(t, database, b, "acct-shared", "aidan-b")
	enableTeamAutoDelegate(t, database, a, b, watcher)
	first, second := minStr(a, b), maxStr(a, b)
	seedDeferredTrigger(t, database, first, domain.EventJiraIssueAssigned, "as-first")
	seedDeferredTrigger(t, database, second, domain.EventJiraIssueAssigned, "as-second")
	seedImmediateWatchTrigger(t, database, watcher, domain.EventJiraIssueAssigned, "as-watch")
	deferTriggerFor(t, database, watcher, "trig-as-watch")
	ageTrigger(t, database, "trig-as-first", "-2 hours")
	ageTrigger(t, database, "trig-as-second", "-1 hours")

	entityID := jiraEntity(t, database, "SKY-nosteal")
	stub := &stubDelegator{db: database}
	r := walkRouter(t, database, stub, nil)
	rec := notifytest.Record(t)

	emitJiraAssigned(t, r, entityID, "acct-shared")
	task := onlyTask(t, database, entityID)
	requireHeld(t, stub, task)
	if vis := visTeamsOf(t, database, task.ID); len(vis) != 3 {
		t.Fatalf("visibility = %v, want both member teams and the watcher", vis)
	}

	scoreTask(t, database, task.ID, 0.9)
	drainReDeriveOnce(t, r)

	if got := firingTriggers(t, database, task.ID); !slices.Equal(got, []string{"trig-as-first"}) {
		t.Fatalf("firings = %v, want only the top-ranked member team's trig-as-first", got)
	}
	requireOwnedByBot(t, database, task.ID, first)
	// The watcher was never a candidate, so only the other member team lost.
	if c := onlyConflict(t, rec, task.ID); len(c.Losers) != 1 || c.Losers[0].TeamID != second {
		t.Errorf("losers = %+v; want only the assignee's other team %s", c.Losers, second)
	}
}

// TestRankedWalk_ReDeriveWinner_FollowsPriorityThenTeamID: the post-scoring
// walk ranks a pool task's teams the way the event-time walk does — the
// highest matched-rule default_priority, then the lowest team id — and the
// order the triggers were created in decides nothing. In both cases the team
// that must lose holds the newest trigger.
func TestRankedWalk_ReDeriveWinner_FollowsPriorityThenTeamID(t *testing.T) {
	for _, c := range []struct {
		name string
		// priority gives the higher team id a 0.75 rule, which outranks the
		// 0.5 a team with only triggers gets.
		priority bool
	}{
		{"priority", true},
		{"team id", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			database := newTestDB(t)
			seedHandlerFKTargets(t, database)
			a := seedTeam(t, database, "team-a")
			b := seedTeam(t, database, "team-b")
			enableTeamAutoDelegate(t, database, a, b)
			low, high := minStr(a, b), maxStr(a, b)
			seedDeferredTrigger(t, database, low, domain.EventJiraIssueAvailable, "av-low")
			seedDeferredTrigger(t, database, high, domain.EventJiraIssueAvailable, "av-high")
			winner, winnerTrigger, newest := low, "trig-av-low", "trig-av-high"
			if c.priority {
				seedSystemJiraRule(t, database, high, domain.EventJiraIssueAvailable)
				winner, winnerTrigger, newest = high, "trig-av-high", "trig-av-low"
			}
			for _, id := range []string{"trig-av-low", "trig-av-high"} {
				if id != newest {
					ageTrigger(t, database, id, "-1 hours")
				}
			}

			stores := sqlitestore.New(database)
			entity, _, err := stores.Entities.FindOrCreateJiraSystem(context.Background(), runmode.LocalDefaultOrgID, "https://jira.example.com", "SKY-rank", dbseed.JiraIssueID("SKY-rank"), dbseed.JiraIssueProject("SKY-rank"), "Rank issue", "https://example.com/rank")
			if err != nil {
				t.Fatalf("create entity: %v", err)
			}
			metaJSON, _ := json.Marshal(events.JiraIssueAvailableMetadata{IssueKey: "SKY-rank", Project: "SKY", Status: "To Do"})
			stub := &stubDelegator{db: database}
			r := walkRouter(t, database, stub, nil)
			if err := r.HandleEvent(context.Background(), domain.Event{
				EventType: domain.EventJiraIssueAvailable, EntityID: &entity.ID,
				MetadataJSON: string(metaJSON), OrgID: runmode.LocalDefaultOrgID,
			}); err != nil {
				t.Fatalf("HandleEvent: %v", err)
			}
			task := onlyTask(t, database, entity.ID)
			if stub.calls != 0 {
				t.Fatalf("%d fire(s) at event time; want the walk held for the score", stub.calls)
			}

			scoreTask(t, database, task.ID, 0.9)
			drainReDeriveOnce(t, r)

			if got := firingTriggers(t, database, task.ID); !slices.Equal(got, []string{winnerTrigger}) {
				t.Fatalf("firings = %v, want only %s", got, winnerTrigger)
			}
			requireOwnedByBot(t, database, task.ID, winner)
		})
	}
}

// TestRankedWalk_Hold_HigherDeferredPasses_LowerImmediateNeverFires: the
// top-ranked team's trigger is deferred and the other team's is immediate. The
// event-time walk holds at the top team, so the immediate trigger does not
// take the claim on arrival; the score passes the deferred trigger's
// threshold, the top team wins, and the other team's admins are told their
// trigger lost the claim.
func TestRankedWalk_Hold_HigherDeferredPasses_LowerImmediateNeverFires(t *testing.T) {
	database := newTestDB(t)
	first, second := twoAuthorTeams(t, database)
	seedDeferredTrigger(t, database, first, domain.EventGitHubPRCICheckFailed, "ci-first")
	seedImmediateTrigger(t, database, second, domain.EventGitHubPRCICheckFailed, "ci-second")
	entityID := reviewEntity(t, database, "owner/repo#holdpass")
	stub := &stubDelegator{db: database}
	r := walkRouter(t, database, stub, nil)
	rec := notifytest.Record(t)

	emitCI(t, r, entityID, "aidan")
	task := onlyTask(t, database, entityID)
	requireHeld(t, stub, task)
	rec.Inactive(t, runmode.LocalDefaultOrgID, kinds.AutomationConflict)

	scoreTask(t, database, task.ID, 0.9)
	drainReDeriveOnce(t, r)

	if got := firingTriggers(t, database, task.ID); !slices.Equal(got, []string{"trig-ci-first"}) {
		t.Fatalf("firings = %v, want only the top team's trig-ci-first", got)
	}
	if stub.calls != 0 {
		t.Errorf("%d inline fire(s); the immediate trigger must never have fired", stub.calls)
	}
	requireOwnedByBot(t, database, task.ID, first)
	c := onlyConflict(t, rec, task.ID)
	if c.Winner.TeamID != first || len(c.Losers) != 1 || c.Losers[0] != (kinds.ConflictLoser{TeamID: second, TriggerID: "trig-ci-second"}) {
		t.Errorf("conflict = %+v; want %s's win reported to %s, linked to trig-ci-second", c, first, second)
	}
}

// TestRankedWalk_Hold_HigherDeferredMisses_LowerImmediateFiresFromReDerive:
// the same hold, but the score misses the top team's threshold. The
// post-scoring walk skips it and the next team's immediate trigger fires from
// there. The skipped team lost nothing, so nobody is told.
func TestRankedWalk_Hold_HigherDeferredMisses_LowerImmediateFiresFromReDerive(t *testing.T) {
	database := newTestDB(t)
	first, second := twoAuthorTeams(t, database)
	seedDeferredTrigger(t, database, first, domain.EventGitHubPRCICheckFailed, "ci-first")
	seedImmediateTrigger(t, database, second, domain.EventGitHubPRCICheckFailed, "ci-second")
	entityID := reviewEntity(t, database, "owner/repo#holdmiss")
	stub := &stubDelegator{db: database}
	r := walkRouter(t, database, stub, nil)
	rec := notifytest.Record(t)

	emitCI(t, r, entityID, "aidan")
	task := onlyTask(t, database, entityID)
	requireHeld(t, stub, task)

	scoreTask(t, database, task.ID, 0.3)
	drainReDeriveOnce(t, r)

	if got := firingTriggers(t, database, task.ID); !slices.Equal(got, []string{"trig-ci-second"}) {
		t.Fatalf("firings = %v, want only the next team's immediate trig-ci-second", got)
	}
	requireOwnedByBot(t, database, task.ID, second)
	rec.Inactive(t, runmode.LocalDefaultOrgID, kinds.AutomationConflict)
}

// TestRankedWalk_NoBackgroundModel_NothingHeld: an org with no usable
// background-jobs model never scores its tasks, so a deferred trigger skips
// rather than holding the walk for a score that is not coming. The next team's
// immediate trigger fires on arrival, and the skipped team lost nothing.
func TestRankedWalk_NoBackgroundModel_NothingHeld(t *testing.T) {
	database := newTestDB(t)
	first, second := twoAuthorTeams(t, database)
	seedDeferredTrigger(t, database, first, domain.EventGitHubPRCICheckFailed, "ci-first")
	seedImmediateTrigger(t, database, second, domain.EventGitHubPRCICheckFailed, "ci-second")
	if _, err := database.Exec(`UPDATE org_settings SET background_jobs_model = '' WHERE org_id = ?`, runmode.LocalDefaultOrgID); err != nil {
		t.Fatalf("unset the background jobs model: %v", err)
	}
	entityID := reviewEntity(t, database, "owner/repo#nomodel")
	stub := &stubDelegator{db: database}
	r := walkRouter(t, database, stub, nil)
	rec := notifytest.Record(t)

	emitCI(t, r, entityID, "aidan")

	if stub.calls != 1 {
		t.Fatalf("%d fire(s) at event time, want the next team's immediate trigger", stub.calls)
	}
	task := onlyTask(t, database, entityID)
	requireOwnedByBot(t, database, task.ID, second)
	rec.Inactive(t, runmode.LocalDefaultOrgID, kinds.AutomationConflict)
}

// TestRankedWalk_BumpOnAScoredTask_DecidesDeferredAtEventTime: a bump on a
// task that already carries a score decides its deferred triggers with that
// score at event time, since no later score will come to decide them. The
// top team's deferred trigger passes and fires on the bump.
func TestRankedWalk_BumpOnAScoredTask_DecidesDeferredAtEventTime(t *testing.T) {
	database := newTestDB(t)
	first, second := twoAuthorTeams(t, database)
	seedDeferredTrigger(t, database, first, domain.EventGitHubPRCICheckFailed, "ci-first")
	seedImmediateTrigger(t, database, second, domain.EventGitHubPRCICheckFailed, "ci-second")
	entityID := reviewEntity(t, database, "owner/repo#bump")
	stub := &stubDelegator{db: database}
	r := walkRouter(t, database, stub, nil)
	rec := notifytest.Record(t)

	emitCI(t, r, entityID, "aidan")
	task := onlyTask(t, database, entityID)
	requireHeld(t, stub, task)
	scoreTask(t, database, task.ID, 0.9)

	emitCI(t, r, entityID, "aidan")

	if stub.calls != 1 {
		t.Fatalf("%d fire(s) on the bump, want the top team's deferred trigger", stub.calls)
	}
	requireOwnedByBot(t, database, task.ID, first)
	if c := onlyConflict(t, rec, task.ID); c.Winner.TeamID != first || len(c.Losers) != 1 || c.Losers[0].TeamID != second {
		t.Errorf("conflict = %+v; want %s's win reported to %s", c, first, second)
	}
}

// TestRankedWalk_ClaimedTask_HoldsNothing: a task already claimed holds
// nothing, since no team's trigger can take the claim from its holder and the
// post-scoring pass skips a claimed task. A higher-ranked team's undecided
// trigger on a bump leaves the owner's immediate trigger to act on the event,
// here by queuing behind the owner's live conversation.
func TestRankedWalk_ClaimedTask_HoldsNothing(t *testing.T) {
	database := newTestDB(t)
	seedHandlerFKTargets(t, database)
	a := seedTeam(t, database, "team-a")
	b := seedTeam(t, database, "team-b")
	enableTeamAutoDelegate(t, database, a, b)
	owner, other := a, b
	seedImmediateTrigger(t, database, owner, domain.EventJiraIssueAvailable, "av-owner")

	entity, _, err := sqlitestore.New(database).Entities.FindOrCreateJiraSystem(context.Background(), runmode.LocalDefaultOrgID, "https://jira.example.com", "SKY-claimed", dbseed.JiraIssueID("SKY-claimed"), dbseed.JiraIssueProject("SKY-claimed"), "Claimed issue", "https://example.com/claimed")
	if err != nil {
		t.Fatalf("create entity: %v", err)
	}
	metaJSON, _ := json.Marshal(events.JiraIssueAvailableMetadata{IssueKey: "SKY-claimed", Project: "SKY", Status: "To Do"})
	emit := func(r *Router) {
		t.Helper()
		if err := r.HandleEvent(context.Background(), domain.Event{
			EventType: domain.EventJiraIssueAvailable, EntityID: &entity.ID,
			MetadataJSON: string(metaJSON), OrgID: runmode.LocalDefaultOrgID,
		}); err != nil {
			t.Fatalf("HandleEvent: %v", err)
		}
	}
	stub := &stubDelegator{db: database}
	r := walkRouter(t, database, stub, nil)
	emit(r)
	task := onlyTask(t, database, entity.ID)
	if stub.calls != 1 {
		t.Fatalf("%d fire(s) on the first event, want the owner's immediate trigger", stub.calls)
	}
	requireOwnedByBot(t, database, task.ID, owner)

	// The other team now outranks the owner, with a deferred trigger and no
	// score yet to decide it.
	seedSystemJiraRule(t, database, other, domain.EventJiraIssueAvailable)
	seedDeferredTrigger(t, database, other, domain.EventJiraIssueAvailable, "av-other")
	emit(r)

	if got := firingTriggers(t, database, task.ID); !slices.Equal(got, []string{"trig-av-owner"}) {
		t.Errorf("firings = %v, want the owner's trig-av-owner queued behind its live conversation", got)
	}
	requireOwnedByBot(t, database, task.ID, owner)
}

// TestRankedWalk_RequeueBeforeScoring_DoesNotReplanAFiredTrigger: the team's
// immediate trigger fired at event time, and a person requeued the task before
// its score landed, so the post-scoring pass finds it unclaimed. The pass
// resumes where the event-time walk would have held and decides only what
// waited for the score: the deferred trigger beside the one that fired, never
// the fired one again. Planning it again would stamp the bot's claim over the
// requeue for a firing its (event, trigger) fence then skips.
func TestRankedWalk_RequeueBeforeScoring_DoesNotReplanAFiredTrigger(t *testing.T) {
	database := newTestDB(t)
	seedHandlerFKTargets(t, database)
	setReviewHost(t, database)
	team := seedTeam(t, database, "team-a")
	seedUserOnTeam(t, database, team, "aidan")
	enableTeamAutoDelegate(t, database, team)
	seedImmediateTrigger(t, database, team, domain.EventGitHubPRCICheckFailed, "ci-now")
	seedDeferredTrigger(t, database, team, domain.EventGitHubPRCICheckFailed, "ci-later")
	entityID := reviewEntity(t, database, "owner/repo#requeued")
	stub := &stubDelegator{db: database}
	r := walkRouter(t, database, stub, nil)

	emitCI(t, r, entityID, "aidan")
	if stub.calls != 1 {
		t.Fatalf("%d fire(s) at event time, want the immediate trigger", stub.calls)
	}
	task := onlyTask(t, database, entityID)
	if ok, err := sqlitestore.New(database).Swipes.RequeueTask(t.Context(), runmode.LocalDefaultOrgID, task.ID); err != nil || !ok {
		t.Fatalf("requeue: ok=%v err=%v", ok, err)
	}

	scoreTask(t, database, task.ID, 0.9)
	drainReDeriveOnce(t, r)

	if got := firingTriggers(t, database, task.ID); !slices.Equal(got, []string{"trig-ci-later"}) {
		t.Errorf("firings = %v, want only the deferred trig-ci-later; trig-ci-now already fired for this event", got)
	}
}

// TestRankedWalk_RequeueBeforeScoring_PassesOverTheTeamsAboveTheHold: the
// same requeue on a pool task, where the top team's immediate trigger fired at
// event time and the next team's trigger is deferred. The post-scoring pass
// starts at the deferred team, so the top team's fired trigger is not planned
// again.
func TestRankedWalk_RequeueBeforeScoring_PassesOverTheTeamsAboveTheHold(t *testing.T) {
	database := newTestDB(t)
	seedHandlerFKTargets(t, database)
	a := seedTeam(t, database, "team-a")
	b := seedTeam(t, database, "team-b")
	enableTeamAutoDelegate(t, database, a, b)
	low, high := minStr(a, b), maxStr(a, b)
	seedImmediateTrigger(t, database, low, domain.EventJiraIssueAvailable, "av-low")
	seedDeferredTrigger(t, database, high, domain.EventJiraIssueAvailable, "av-high")

	st := sqlitestore.New(database)
	entity, _, err := st.Entities.FindOrCreateJiraSystem(context.Background(), runmode.LocalDefaultOrgID, "https://jira.example.com", "SKY-requeue", dbseed.JiraIssueID("SKY-requeue"), dbseed.JiraIssueProject("SKY-requeue"), "Requeued issue", "https://example.com/requeue")
	if err != nil {
		t.Fatalf("create entity: %v", err)
	}
	metaJSON, _ := json.Marshal(events.JiraIssueAvailableMetadata{IssueKey: "SKY-requeue", Project: "SKY", Status: "To Do"})
	stub := &stubDelegator{db: database}
	r := walkRouter(t, database, stub, nil)
	if err := r.HandleEvent(context.Background(), domain.Event{
		EventType: domain.EventJiraIssueAvailable, EntityID: &entity.ID,
		MetadataJSON: string(metaJSON), OrgID: runmode.LocalDefaultOrgID,
	}); err != nil {
		t.Fatalf("HandleEvent: %v", err)
	}
	if stub.calls != 1 {
		t.Fatalf("%d fire(s) at event time, want the top team's immediate trigger", stub.calls)
	}
	task := onlyTask(t, database, entity.ID)
	if ok, err := st.Swipes.RequeueTask(t.Context(), runmode.LocalDefaultOrgID, task.ID); err != nil || !ok {
		t.Fatalf("requeue: ok=%v err=%v", ok, err)
	}

	scoreTask(t, database, task.ID, 0.9)
	drainReDeriveOnce(t, r)

	if got := firingTriggers(t, database, task.ID); !slices.Equal(got, []string{"trig-av-high"}) {
		t.Errorf("firings = %v, want only the deferred trig-av-high; trig-av-low already fired for this event", got)
	}
}

// failingSettingsOrgs fails the org-settings read, the one the deferral gate
// makes for the background-jobs model.
type failingSettingsOrgs struct {
	dbpkg.OrgsStore
}

func (failingSettingsOrgs) GetSettingsSystem(context.Context, string) (domain.OrgSettings, error) {
	return domain.OrgSettings{}, errors.New("org settings unreadable")
}

// TestRankedWalk_ModelReadFailingAfterTheWinner_IsNotRetried: once a team's
// trigger has won, a failed model read for a lower team's deferred trigger can
// change nothing — the lower team lost the claim whatever its score — so the
// event is not sent back to be replayed over it.
func TestRankedWalk_ModelReadFailingAfterTheWinner_IsNotRetried(t *testing.T) {
	database := newTestDB(t)
	seedHandlerFKTargets(t, database)
	a := seedTeam(t, database, "team-a")
	b := seedTeam(t, database, "team-b")
	enableTeamAutoDelegate(t, database, a, b)
	low, high := minStr(a, b), maxStr(a, b)
	seedImmediateTrigger(t, database, low, domain.EventJiraIssueAvailable, "av-low")
	seedDeferredTrigger(t, database, high, domain.EventJiraIssueAvailable, "av-high")

	st := sqlitestore.New(database)
	entity, _, err := st.Entities.FindOrCreateJiraSystem(context.Background(), runmode.LocalDefaultOrgID, "https://jira.example.com", "SKY-modelerr", dbseed.JiraIssueID("SKY-modelerr"), dbseed.JiraIssueProject("SKY-modelerr"), "Model error issue", "https://example.com/modelerr")
	if err != nil {
		t.Fatalf("create entity: %v", err)
	}
	metaJSON, _ := json.Marshal(events.JiraIssueAvailableMetadata{IssueKey: "SKY-modelerr", Project: "SKY", Status: "To Do"})
	stub := &stubDelegator{db: database}
	r := firingRouter(database, stub)
	r.orgs = failingSettingsOrgs{OrgsStore: st.Orgs}

	err = r.HandleEvent(context.Background(), domain.Event{
		EventType: domain.EventJiraIssueAvailable, EntityID: &entity.ID,
		MetadataJSON: string(metaJSON), OrgID: runmode.LocalDefaultOrgID,
	})
	if err != nil {
		t.Errorf("HandleEvent = %v; a read that cannot change the outcome must not ask for a replay", err)
	}
	if stub.calls != 1 {
		t.Errorf("%d fire(s), want the top team's immediate trigger", stub.calls)
	}
}

// TestRankedWalk_ScoreMovedRollback_PostsNoConflict: the post-scoring walk
// decided a conflict, and a newer score landed before its completion locked
// the row. The completion commits nothing, so nothing the conflict rests on
// landed and no notice is posted; the next evaluation decides it again and
// posts once its firing is admitted.
func TestRankedWalk_ScoreMovedRollback_PostsNoConflict(t *testing.T) {
	database := newTestDB(t)
	first, second := twoAuthorTeams(t, database)
	seedDeferredTrigger(t, database, first, domain.EventGitHubPRCICheckFailed, "ci-first")
	seedImmediateTrigger(t, database, second, domain.EventGitHubPRCICheckFailed, "ci-second")
	entityID := reviewEntity(t, database, "owner/repo#moved")
	stub := &stubDelegator{db: database}
	var taskID string
	landed := false
	r := walkRouter(t, database, stub, func() {
		if landed {
			return
		}
		landed = true
		scoreTask(t, database, taskID, 0.95)
	})
	rec := notifytest.Record(t)

	emitCI(t, r, entityID, "aidan")
	taskID = onlyTask(t, database, entityID).ID
	scoreTask(t, database, taskID, 0.9)

	drainReDeriveOnce(t, r)
	if !landed {
		t.Fatal("the newer score never landed; the hook did not run before the completion")
	}
	requireNoFirings(t, database, taskID)
	rec.Inactive(t, runmode.LocalDefaultOrgID, kinds.AutomationConflict)

	drainReDeriveOnce(t, r)
	if got := firingTriggers(t, database, taskID); !slices.Equal(got, []string{"trig-ci-first"}) {
		t.Fatalf("firings = %v, want trig-ci-first from the re-evaluation", got)
	}
	if c := onlyConflict(t, rec, taskID); c.Winner.TeamID != first || len(c.Losers) != 1 || c.Losers[0].TeamID != second {
		t.Errorf("conflict = %+v; want %s's win reported to %s", c, first, second)
	}
}
