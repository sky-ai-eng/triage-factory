package routing

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/domain/events"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// mergedEvent is a github:pr:merged event authored by aidan (a TF member in
// these fixtures, so the owning-team ladder resolves the riding task's owner).
func mergedEvent(t *testing.T, entityID, repo string) domain.Event {
	t.Helper()
	meta, _ := json.Marshal(events.GitHubPRMergedMetadata{
		Author: "aidan", Repo: repo, PRNumber: 1, MergedBy: "aidan", HeadSHA: "abc123",
	})
	return domain.Event{
		EventType:    domain.EventGitHubPRMerged,
		EntityID:     &entityID,
		MetadataJSON: string(meta),
		OccurredAt:   time.Now(),
		CreatedAt:    time.Now(),
		OrgID:        runmode.LocalDefaultOrgID,
	}
}

// seedMatchAllRule inserts a match-all (empty predicate) applies_to_unowned
// rule on an arbitrary event type — used to wire a triage rule on a terminating
// event so a merge can mint a riding card.
func seedMatchAllRule(t *testing.T, database *sql.DB, teamID, eventType string) {
	t.Helper()
	id := uuid.New().String()
	if _, err := database.Exec(`
		INSERT INTO event_handlers
			(id, org_id, team_id, creator_user_id, kind, event_type,
			 scope_predicate_json, enabled, source, applies_to_unowned, name, default_priority, sort_order,
			 created_at, updated_at)
		VALUES (?, ?, ?, ?, 'rule', ?, NULL, 1, 'user', 1, ?, 0.7, 100, ?, ?)
	`, id, runmode.LocalDefaultOrgID, teamID, runmode.LocalDefaultUserID,
		eventType, "rule "+eventType, time.Now(), time.Now()); err != nil {
		t.Fatalf("seed rule %s for %s: %v", eventType, teamID, err)
	}
}

// seedTerminatingEntityWithCIFailure stands up an active PR entity with one
// live ci_check_failed task (a sibling the terminating close should reap), and
// returns the router + entity id. The CI rule is the sibling's source; callers
// add whatever merged handler the case under test needs before merging.
func seedTerminatingEntityWithCIFailure(t *testing.T, database *sql.DB, stub *stubDelegator) (*Router, string) {
	t.Helper()
	seedHandlerFKTargets(t, database)
	setReviewHost(t, database)
	team := runmode.LocalDefaultTeamID
	seedUserOnTeam(t, database, team, "aidan")
	seedMatchAllCIRule(t, database, team)

	st := sqlitestore.New(database)
	ctx := context.Background()
	entity, _, err := st.Entities.FindOrCreate(ctx, runmode.LocalDefaultOrgID, "github", "owner/repo#1", "pr", "PR", "https://example.com/1")
	if err != nil {
		t.Fatalf("create entity: %v", err)
	}
	router := firingRouter(database, stub)
	router.HandleEvent(ciEvent(t, entity.ID, "owner/repo"))
	if ci, _ := testTaskStore(database).FindActiveByEntityAndType(ctx, runmode.LocalDefaultOrgID, entity.ID, domain.EventGitHubPRCICheckFailed); len(ci) != 1 {
		t.Fatalf("setup: expected 1 ci_check_failed task, got %d", len(ci))
	}
	return router, entity.ID
}

func entityState(t *testing.T, database *sql.DB, entityID string) string {
	t.Helper()
	ent, err := sqlitestore.New(database).Entities.GetSystem(context.Background(), runmode.LocalDefaultOrgID, entityID)
	if err != nil || ent == nil {
		t.Fatalf("get entity %s: %v", entityID, err)
	}
	return ent.State
}

// TestTerminating_Merged_TriggerFires_ClosesSiblings is the headline TFAC-520
// behavior: a github:pr:merged with a configured trigger closes the entity's
// in-flight tasks AND routes — minting a synthetic riding task and firing its
// blueprint — instead of short-circuiting before handler matching.
func TestTerminating_Merged_TriggerFires_ClosesSiblings(t *testing.T) {
	database := newTestDB(t)
	stub := &stubDelegator{db: database}
	router, entityID := seedTerminatingEntityWithCIFailure(t, database, stub)

	team := runmode.LocalDefaultTeamID
	enableTeamAutoDelegate(t, database, team)
	seedImmediateTrigger(t, database, team, domain.EventGitHubPRMerged, "merge")

	router.HandleEvent(mergedEvent(t, entityID, "owner/repo"))

	ctx := context.Background()
	ts := testTaskStore(database)

	// The CI failure sibling closed.
	if ci, _ := ts.FindActiveByEntityAndType(ctx, runmode.LocalDefaultOrgID, entityID, domain.EventGitHubPRCICheckFailed); len(ci) != 0 {
		t.Errorf("ci_check_failed task should close on merge, %d still active", len(ci))
	}
	// The merged trigger fired exactly once.
	if got := atomic.LoadInt64(&stub.calls); got != 1 {
		t.Errorf("merged trigger fired %d times, want 1", got)
	}
	// The riding task exists, owned by the team, on the now-closed entity.
	active, _ := ts.FindActiveByEntity(ctx, runmode.LocalDefaultOrgID, entityID)
	if len(active) != 1 || active[0].EventType != domain.EventGitHubPRMerged {
		t.Fatalf("expected 1 active merged riding task, got %d (%+v)", len(active), active)
	}
	if teamIDValue(&active[0]) != team {
		t.Errorf("riding task owner = %q, want team %q", teamIDValue(&active[0]), team)
	}
	// The entity terminated.
	if s := entityState(t, database, entityID); s != "closed" {
		t.Errorf("entity state = %q, want closed", s)
	}
}

// TestTerminating_Merged_RuleCreatesCard_NoFire: a triage rule (no
// auto-delegation) on merge mints a triage card and closes siblings, but fires
// nothing.
func TestTerminating_Merged_RuleCreatesCard_NoFire(t *testing.T) {
	database := newTestDB(t)
	stub := &stubDelegator{db: database}
	router, entityID := seedTerminatingEntityWithCIFailure(t, database, stub)

	seedMatchAllRule(t, database, runmode.LocalDefaultTeamID, domain.EventGitHubPRMerged)

	router.HandleEvent(mergedEvent(t, entityID, "owner/repo"))

	ctx := context.Background()
	ts := testTaskStore(database)

	if got := atomic.LoadInt64(&stub.calls); got != 0 {
		t.Errorf("a rule must not fire automation, fired %d times", got)
	}
	active, _ := ts.FindActiveByEntity(ctx, runmode.LocalDefaultOrgID, entityID)
	if len(active) != 1 || active[0].EventType != domain.EventGitHubPRMerged {
		t.Fatalf("expected 1 active merged triage card, got %d (%+v)", len(active), active)
	}
	if s := entityState(t, database, entityID); s != "closed" {
		t.Errorf("entity state = %q, want closed", s)
	}
}

// TestTerminating_Merged_NoHandler_ClosesAndTerminates_NoCard: with nothing
// configured on merge, the close + terminate still happen, but no riding task
// is created (same user-visible behavior as before TFAC-520).
func TestTerminating_Merged_NoHandler_ClosesAndTerminates_NoCard(t *testing.T) {
	database := newTestDB(t)
	stub := &stubDelegator{db: database}
	router, entityID := seedTerminatingEntityWithCIFailure(t, database, stub)

	router.HandleEvent(mergedEvent(t, entityID, "owner/repo"))

	ctx := context.Background()
	active, _ := testTaskStore(database).FindActiveByEntity(ctx, runmode.LocalDefaultOrgID, entityID)
	if len(active) != 0 {
		t.Errorf("no handler on merge → no riding task, got %d active (%+v)", len(active), active)
	}
	if got := atomic.LoadInt64(&stub.calls); got != 0 {
		t.Errorf("nothing should fire, fired %d times", got)
	}
	if s := entityState(t, database, entityID); s != "closed" {
		t.Errorf("entity state = %q, want closed", s)
	}
}

// TestTerminating_StragglerOnClosedEntity_Dropped: once the entity is closed, a
// later non-terminating event neither reopens it nor mints a task — the gate
// drops it.
func TestTerminating_StragglerOnClosedEntity_Dropped(t *testing.T) {
	database := newTestDB(t)
	stub := &stubDelegator{db: database}
	router, entityID := seedTerminatingEntityWithCIFailure(t, database, stub)

	router.HandleEvent(mergedEvent(t, entityID, "owner/repo"))
	// A straggler CI failure arrives after the merge.
	router.HandleEvent(ciEvent(t, entityID, "owner/repo"))

	ctx := context.Background()
	if ci, _ := testTaskStore(database).FindActiveByEntityAndType(ctx, runmode.LocalDefaultOrgID, entityID, domain.EventGitHubPRCICheckFailed); len(ci) != 0 {
		t.Errorf("straggler on a closed entity must not mint a task, got %d", len(ci))
	}
	if s := entityState(t, database, entityID); s != "closed" {
		t.Errorf("entity state = %q, want closed (straggler must not reopen)", s)
	}
}

// TestTerminalCloseSet_CoversTaskTypes is the drift guard: every github:pr:* /
// jira:issue:* event type that can mint a task must be either in the
// terminating close set or be a terminating event itself — otherwise a
// merge/close/complete would silently leak that type's tasks.
func TestTerminalCloseSet_CoversTaskTypes(t *testing.T) {
	covers := func(prefix string, closeSet []string, terminators ...string) {
		covered := map[string]bool{}
		for _, et := range closeSet {
			covered[et] = true
		}
		for _, et := range terminators {
			covered[et] = true
		}
		for _, et := range domain.AllEventTypes() {
			if strings.HasPrefix(et.ID, prefix) && !covered[et.ID] {
				t.Errorf("%q is neither in the terminal close set nor a terminating event — terminating would leak its tasks", et.ID)
			}
		}
	}
	covers("github:pr:", githubPRTerminalCloseTypes(), domain.EventGitHubPRMerged, domain.EventGitHubPRClosed)
	covers("jira:issue:", jiraIssueTerminalCloseTypes(), domain.EventJiraIssueCompleted)
}
