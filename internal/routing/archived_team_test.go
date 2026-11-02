package routing

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"

	dbpkg "github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/domain/events"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// archiveTeam stamps teamID archived, the column the archive handler writes.
func archiveTeam(t *testing.T, database *sql.DB, teamID string) {
	t.Helper()
	if _, err := database.Exec(`UPDATE teams SET deleted_at = CURRENT_TIMESTAMP WHERE id = ?`, teamID); err != nil {
		t.Fatalf("archive team %s: %v", teamID, err)
	}
}

// TestArchivedTeam_TrackedRepoMintsNothingForIt: both teams track the repo and
// watch every CI failure, and team B is archived. The event's task is A's
// alone: B neither matches nor reaches the visibility set.
func TestArchivedTeam_TrackedRepoMintsNothingForIt(t *testing.T) {
	dbh := newGateDB(t)
	st := sqlitestore.New(dbh)
	ctx := context.Background()

	teamA := runmode.LocalDefaultTeamID
	teamB := seedGateTeam(t, dbh, "team-b")
	shared := []domain.TeamGitHubRepo{{Owner: "owner", Repo: "shared"}}
	for _, team := range []string{teamA, teamB} {
		if err := dbtest.TrackRepos(ctx, st.Repos, st.TeamGitHubRepos, runmode.LocalDefaultOrgID, team, dbtest.TestGitHubHost, shared); err != nil {
			t.Fatalf("track for %s: %v", team, err)
		}
		seedMatchAllCIRule(t, dbh, team)
	}
	archiveTeam(t, dbh, teamB)

	entity, _, err := st.Entities.FindOrCreate(ctx, runmode.LocalDefaultOrgID, "github", "https://github.com", "owner/shared#1", "", "pr", "Shared PR", "https://example.com/s")
	if err != nil {
		t.Fatalf("create entity: %v", err)
	}
	mustHandle(t, gateRouter(dbh), ciEvent(t, entity.ID, "owner/shared"))

	active, err := testTaskStore(dbh).FindActiveByEntity(ctx, runmode.LocalDefaultOrgID, entity.ID)
	if err != nil || len(active) != 1 {
		t.Fatalf("expected 1 task, got %d (err=%v)", len(active), err)
	}
	if vis := visTeamsOf(t, dbh, active[0].ID); len(vis) != 1 || vis[0] != teamA {
		t.Errorf("visibility = %v, want only teamA %q", vis, teamA)
	}
}

// TestArchivedTeam_IsNoGateAnswer: the team-scope gate answers that an archived
// team tracks nothing, in both postures, without consulting the tracking store
// (which here answers that every team tracks everything).
func TestArchivedTeam_IsNoGateAnswer(t *testing.T) {
	database := newTestDB(t)
	seedHandlerFKTargets(t, database)
	teamB := seedTeam(t, database, "team-b")
	archiveTeam(t, database, teamB)
	entityID := reviewEntity(t, database, "owner/repo#gate")

	r := reviewRouter(database)
	if got := r.teamScopeCached(context.Background(), ciFailedEvent(entityID, "aidan"), teamB, teamScopeCache{}); got.open || got.strict {
		t.Errorf("archived team's scope = %+v, want neither posture to pass", got)
	}
}

// TestArchivedTeam_StructuralOwnerFallsThrough: the entity's owning_team_id
// names archived team B, so tier 1 has no answer and the author's team A owns
// the task.
func TestArchivedTeam_StructuralOwnerFallsThrough(t *testing.T) {
	database := newTestDB(t)
	seedHandlerFKTargets(t, database)
	setReviewHost(t, database)

	teamA := seedTeam(t, database, "author-team")
	teamB := seedTeam(t, database, "override-team")
	seedUserOnTeam(t, database, teamA, "aidan")
	seedSystemCIRule(t, database, teamA)
	seedSystemCIRule(t, database, teamB)

	entityID := reviewEntity(t, database, "owner/repo#archived-override")
	if stamped, err := sqlitestore.New(database).Entities.StampOwningTeamIfUnsetSystem(
		context.Background(), runmode.LocalDefaultOrgID, entityID, teamB); err != nil || !stamped {
		t.Fatalf("stamp owning team: stamped=%v err=%v", stamped, err)
	}
	archiveTeam(t, database, teamB)

	emitCI(t, reviewRouter(database), entityID, "aidan")

	active, err := testTaskStore(database).FindActiveByEntity(context.Background(), runmode.LocalDefaultOrgID, entityID)
	if err != nil || len(active) != 1 {
		t.Fatalf("expected 1 task, got %d (err=%v)", len(active), err)
	}
	if teamIDValue(&active[0]) != teamA {
		t.Errorf("owner = %q, want the author's team %q (the archived structural owner is no answer)", teamIDValue(&active[0]), teamA)
	}
	if vis := visTeamsOf(t, database, active[0].ID); len(vis) != 1 || vis[0] != teamA {
		t.Errorf("visibility = %v, want only %q", vis, teamA)
	}
}

// TestArchivedTeam_PriorTaskOwnerFallsThrough: the entity's prior owned task is
// archived team B's, so tier 2 has no answer and the next event goes to its
// author's team A rather than anchoring to B.
func TestArchivedTeam_PriorTaskOwnerFallsThrough(t *testing.T) {
	database := newTestDB(t)
	seedHandlerFKTargets(t, database)
	setReviewHost(t, database)

	teamA := seedTeam(t, database, "team-a")
	teamB := seedTeam(t, database, "team-b")
	seedUserOnTeam(t, database, teamA, "aidan")
	seedUserOnTeam(t, database, teamB, "bob")
	for _, team := range []string{teamA, teamB} {
		seedSystemCIRule(t, database, team)
		seedSystemRule(t, database, team, domain.EventGitHubPRConflicts)
	}

	entityID := reviewEntity(t, database, "owner/repo#archived-anchor")
	router := reviewRouter(database)
	emitCI(t, router, entityID, "bob")
	archiveTeam(t, database, teamB)

	conflictMeta, _ := json.Marshal(events.GitHubPRConflictsMetadata{Author: "aidan", Repo: "owner/repo"})
	mustHandle(t, router, domain.Event{
		EventType:    domain.EventGitHubPRConflicts,
		EntityID:     &entityID,
		MetadataJSON: string(conflictMeta),
		OrgID:        runmode.LocalDefaultOrgID,
	})

	conflicts, err := testTaskStore(database).FindActiveByEntityAndType(context.Background(), runmode.LocalDefaultOrgID, entityID, domain.EventGitHubPRConflicts)
	if err != nil || len(conflicts) != 1 {
		t.Fatalf("expected 1 conflicts task, got %d (err=%v)", len(conflicts), err)
	}
	if teamIDValue(&conflicts[0]) != teamA {
		t.Errorf("conflicts owner = %q, want teamA %q (an archived team's task anchors nothing)", teamIDValue(&conflicts[0]), teamA)
	}
}

// TestArchivedTeam_MultiTeamAuthorGetsTheActiveTeam: the author belongs to A
// and archived B, and both track the repo. The owner is A alone, not an
// ambiguous A/B pair.
func TestArchivedTeam_MultiTeamAuthorGetsTheActiveTeam(t *testing.T) {
	database := newTestDB(t)
	seedHandlerFKTargets(t, database)
	setReviewHost(t, database)

	teamA := seedTeam(t, database, "team-a")
	teamB := seedTeam(t, database, "team-b")
	uid := seedUserOnTeam(t, database, teamA, "aidan")
	enrollOnTeam(t, database, uid, teamB)
	seedSystemCIRule(t, database, teamA)
	seedSystemCIRule(t, database, teamB)
	archiveTeam(t, database, teamB)

	entityID := reviewEntity(t, database, "owner/repo#archived-author")
	emitCI(t, reviewRouter(database), entityID, "aidan")

	active, err := testTaskStore(database).FindActiveByEntity(context.Background(), runmode.LocalDefaultOrgID, entityID)
	if err != nil || len(active) != 1 {
		t.Fatalf("expected 1 task, got %d (err=%v)", len(active), err)
	}
	if teamIDValue(&active[0]) != teamA {
		t.Errorf("owner = %q, want teamA %q", teamIDValue(&active[0]), teamA)
	}
	if vis := visTeamsOf(t, database, active[0].ID); len(vis) != 1 || vis[0] != teamA {
		t.Errorf("visibility = %v, want only %q", vis, teamA)
	}
}

// TestArchivedTeam_RequestedGitHubTeamSkipsIt: a review request names a GitHub
// team mapped to TF teams A and archived B. The task is visible to A alone.
func TestArchivedTeam_RequestedGitHubTeamSkipsIt(t *testing.T) {
	database := newTestDB(t)
	seedHandlerFKTargets(t, database)
	setReviewHost(t, database)

	teamA := seedTeam(t, database, "backend-a")
	teamB := seedTeam(t, database, "backend-b")
	for _, team := range []string{teamA, teamB} {
		if err := sqlitestore.New(database).TeamGitHubGroups.SetForTeam(context.Background(), team, dbtest.TestGitHubHost, []domain.TeamGitHubGroup{
			{GitHubTeamID: "11", GitHubOrgID: "9", OrgLogin: "acme", TeamSlug: "backend"},
		}); err != nil {
			t.Fatalf("map github team for %s: %v", team, err)
		}
	}
	seedReviewRule(t, database, teamA)
	archiveTeam(t, database, teamB)

	entityID := reviewEntity(t, database, "owner/repo#archived-team-review")
	emitReviewRequested(t, reviewRouter(database), entityID, "team:acme/backend", events.GitHubPRReviewRequestedMetadata{
		Author: "carol", Repo: "owner/repo", PRNumber: 3, RequestedTeam: "acme/backend",
	})

	active, err := testTaskStore(database).FindActiveByEntity(context.Background(), runmode.LocalDefaultOrgID, entityID)
	if err != nil || len(active) != 1 {
		t.Fatalf("expected 1 task, got %d (err=%v)", len(active), err)
	}
	if vis := visTeamsOf(t, database, active[0].ID); len(vis) != 1 || vis[0] != teamA {
		t.Errorf("visibility = %v, want only %q", vis, teamA)
	}
}

// TestArchivedTeam_RegisteredSourceOwnerIsNoAnswer: a registered source's owner
// hook names archived team B (as Slack's does for a channel whose primary
// tracker was archived). B owns nothing: the task is unowned and reaches A
// only through A's watch rule.
func TestArchivedTeam_RegisteredSourceOwnerIsNoAnswer(t *testing.T) {
	database := newTestDB(t)
	seedHandlerFKTargets(t, database)
	seedFakeEventCatalog(t, database)

	teamA := seedTeam(t, database, "team-a")
	teamB := seedTeam(t, database, "team-b")
	t.Cleanup(ResetSources)
	RegisterSource("fake", fakeSourceHooks(teamB, true))
	if _, err := database.Exec(`
		INSERT INTO event_handlers
			(id, org_id, team_id, creator_user_id, kind, event_type,
			 scope_predicate_json, enabled, source, applies_to_unowned, name, default_priority, sort_order,
			 created_at, updated_at)
		VALUES (?, ?, ?, ?, 'rule', ?, NULL, 1, 'user', 1, 'fake watch', 0.6, 100, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)
	`, "fake-watch-"+teamA[:8], runmode.LocalDefaultOrgID, teamA, runmode.LocalDefaultUserID, fakeEventType); err != nil {
		t.Fatalf("seed watch rule: %v", err)
	}
	archiveTeam(t, database, teamB)

	entityID := fakeEntity(t, database, "fake/thing#archived-owner")
	mustHandle(t, fakeSourceRouter(database), fakeEvent(entityID))

	active, err := testTaskStore(database).FindActiveByEntity(context.Background(), runmode.LocalDefaultOrgID, entityID)
	if err != nil || len(active) != 1 {
		t.Fatalf("expected 1 task, got %d (err=%v)", len(active), err)
	}
	if owner := teamIDValue(&active[0]); owner != "" {
		t.Errorf("owner = %q, want none (the hook's team is archived)", owner)
	}
	if vis := visTeamsOf(t, database, active[0].ID); len(vis) != 1 || vis[0] != teamA {
		t.Errorf("visibility = %v, want only the watcher %q", vis, teamA)
	}
}

// failingTeamReads is a Teams store whose by-id read is down.
type failingTeamReads struct{ dbpkg.TeamsStore }

func (failingTeamReads) GetSystem(context.Context, string, string) (*domain.Team, error) {
	return nil, errors.New("teams store down")
}

// TestArchivedTeam_FailedTeamReadIsALadderError: tier 1 names a team and the
// read of whether it is archived fails. The event errors, to be retried, and
// no task is minted on whatever the next tier would have answered.
func TestArchivedTeam_FailedTeamReadIsALadderError(t *testing.T) {
	database := newTestDB(t)
	seedHandlerFKTargets(t, database)
	setReviewHost(t, database)

	teamA := seedTeam(t, database, "owner-team")
	teamB := seedTeam(t, database, "author-team")
	seedUserOnTeam(t, database, teamB, "aidan")
	seedSystemCIRule(t, database, teamA)
	seedSystemCIRule(t, database, teamB)

	entityID := reviewEntity(t, database, "owner/repo#team-read-down")
	if stamped, err := sqlitestore.New(database).Entities.StampOwningTeamIfUnsetSystem(
		context.Background(), runmode.LocalDefaultOrgID, entityID, teamA); err != nil || !stamped {
		t.Fatalf("stamp owning team: stamped=%v err=%v", stamped, err)
	}

	deps := testRouterDeps(database)
	deps.Teams = failingTeamReads{deps.Teams}
	if err := newTestRouter(deps).HandleEvent(t.Context(), ciFailedEvent(entityID, "aidan")); err == nil {
		t.Fatal("HandleEvent succeeded; want the failed team read to fail the event")
	}
	active, err := testTaskStore(database).FindActiveByEntity(context.Background(), runmode.LocalDefaultOrgID, entityID)
	if err != nil {
		t.Fatalf("list active: %v", err)
	}
	if len(active) != 0 {
		t.Errorf("tasks = %d, want 0 (a failed tier must not fall through to the author's team)", len(active))
	}
}

// TestFiringWorker_ArchivedTeamsTrigger: a firing queued before its trigger's
// team was archived is skipped as team_archived, and no run starts.
func TestFiringWorker_ArchivedTeamsTrigger(t *testing.T) {
	database := newTestDB(t)
	entityID, taskID, triggerID, eventID := setupDrainScenario(t, database)
	enqueueFiring(t, database, entityID, taskID, triggerID, eventID)
	archiveTeam(t, database, runmode.LocalDefaultTeamID)

	stub := &stubDelegator{db: database}
	drainOnce(t, drainRouter(database, sqlitestore.New(database).PendingFirings, stub))

	rows := firingsFor(t, database, entityID)
	if len(rows) != 1 {
		t.Fatalf("expected 1 firing row, got %d", len(rows))
	}
	requireSkipped(t, rows[0], domain.PendingFiringSkipTeamArchived)
	if n := atomic.LoadInt64(&stub.calls); n != 0 {
		t.Errorf("Delegate called %d times, want 0", n)
	}
	var conversations int
	if err := database.QueryRow(`SELECT COUNT(*) FROM conversations WHERE task_id = ?`, taskID).Scan(&conversations); err != nil {
		t.Fatalf("count conversations: %v", err)
	}
	if conversations != 0 {
		t.Errorf("conversations on the task = %d, want 0", conversations)
	}
}
