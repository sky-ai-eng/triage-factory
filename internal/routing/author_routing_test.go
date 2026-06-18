package routing

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/domain/events"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/pkg/websocket"
)

// Author-centric routing tests (SKY-372). These exercise the owning-team
// ladder for github:pr:* events that concern the PR's owner — CI failures,
// conflicts, review feedback, new commits — as opposed to review_requested
// (the reviewer axis, covered in review_routing_test.go). The shared helpers
// (setReviewHost, seedTeam, seedUserOnTeam, reviewEntity, visTeamsOf,
// seedMatchAllCIRule) live in review_routing_test.go / gate_test.go.

// emitCI fires a ci_check_failed (author-centric) event for entityID authored
// by author.
func emitCI(router *Router, entityID, author string) {
	meta := events.GitHubPRCICheckFailedMetadata{
		Author: author, CheckName: "build", Repo: "owner/repo", HeadSHA: "abc123",
	}
	metaJSON, _ := json.Marshal(meta)
	router.HandleEvent(domain.Event{
		EventType:    domain.EventGitHubPRCICheckFailed,
		EntityID:     &entityID,
		DedupKey:     "build",
		MetadataJSON: string(metaJSON),
		OccurredAt:   time.Now(),
		OrgID:        runmode.LocalDefaultOrgID,
	})
}

// seedSystemRule seeds a system-source (shipped-style) match-all rule for the
// given event type on teamID. A system rule gates creation but — unlike a
// user-authored rule — never grants visibility on its own, so on its own it
// can't surface a task for a non-owner. The id/slug embed the event so several
// types can be seeded per team.
func seedSystemRule(t *testing.T, database *sql.DB, teamID, eventType string) {
	t.Helper()
	key := teamID[:8] + "-" + eventType[len(eventType)-6:]
	if _, err := database.Exec(`
		INSERT INTO event_handlers
			(id, org_id, team_id, creator_user_id, kind, event_type,
			 system_slug, scope_predicate_json, enabled, source, name, default_priority, sort_order,
			 created_at, updated_at)
		VALUES (?, ?, ?, NULL, 'rule', ?, ?, '{"author_in":[]}', 1, 'system', 'sys rule', 0.75, 0, ?, ?)
	`, "sys-"+key, runmode.LocalDefaultOrgID, teamID,
		eventType, "system-rule-"+key, time.Now(), time.Now()); err != nil {
		t.Fatalf("seed system rule (%s) for team %s: %v", eventType, teamID, err)
	}
}

// seedSystemCIRule is the ci_check_failed convenience wrapper around
// seedSystemRule.
func seedSystemCIRule(t *testing.T, database *sql.DB, teamID string) {
	t.Helper()
	seedSystemRule(t, database, teamID, domain.EventGitHubPRCICheckFailed)
}

// TestAuthorCentric_StrangerPR_NoTask is the headline bug fix (local N=1):
// repo-wide polling surfaces a CI failure on a PR authored by someone who
// isn't a TF user. With only a system rule (no explicit watch) and no
// resolvable owner, the event is recorded but mints NO task.
func TestAuthorCentric_StrangerPR_NoTask(t *testing.T) {
	database := newTestDB(t)
	seedHandlerFKTargets(t, database)
	setReviewHost(t, database)
	seedUserOnTeam(t, database, runmode.LocalDefaultTeamID, "aidan") // the local user
	seedSystemCIRule(t, database, runmode.LocalDefaultTeamID)

	entityID := reviewEntity(t, database, "owner/repo#stranger")
	emitCI(reviewRouter(database), entityID, "stranger") // not a TF user

	active, err := testTaskStore(database).FindActiveByEntity(context.Background(), runmode.LocalDefaultOrgID, entityID)
	if err != nil {
		t.Fatalf("list tasks: %v", err)
	}
	if len(active) != 0 {
		t.Fatalf("expected no task for a stranger's PR (over-spawn fix), got %d", len(active))
	}
}

// TestAuthorCentric_LocalUserPR_OneTeam is the positive of the bug fix: the
// same system rule, but the PR is the local user's own — the author resolves
// to the one team, so a single task lands there.
func TestAuthorCentric_LocalUserPR_OneTeam(t *testing.T) {
	database := newTestDB(t)
	seedHandlerFKTargets(t, database)
	setReviewHost(t, database)
	seedUserOnTeam(t, database, runmode.LocalDefaultTeamID, "aidan")
	seedSystemCIRule(t, database, runmode.LocalDefaultTeamID)

	entityID := reviewEntity(t, database, "owner/repo#mine")
	emitCI(reviewRouter(database), entityID, "aidan")

	active, err := testTaskStore(database).FindActiveByEntity(context.Background(), runmode.LocalDefaultOrgID, entityID)
	if err != nil || len(active) != 1 {
		t.Fatalf("expected 1 task on the local user's PR, got %d (err=%v)", len(active), err)
	}
	if teamIDValue(&active[0]) != runmode.LocalDefaultTeamID {
		t.Errorf("owner = %q, want the one team %q", teamIDValue(&active[0]), runmode.LocalDefaultTeamID)
	}
}

// TestAuthorCentric_AuthorOnOneTeam_OwnedByThatTeam: in a two-team org the
// author belongs to team A only (no project/override/prior task), so a CI
// failure is owned by A and never surfaces to team B. A system rule on B does
// not pull B in.
func TestAuthorCentric_AuthorOnOneTeam_OwnedByThatTeam(t *testing.T) {
	database := newTestDB(t)
	seedHandlerFKTargets(t, database)
	setReviewHost(t, database)

	teamA := seedTeam(t, database, "team-a")
	teamB := seedTeam(t, database, "team-b")
	seedUserOnTeam(t, database, teamA, "aidan")
	// Both teams run a system CI rule (gates creation; no visibility grant).
	seedSystemCIRule(t, database, teamA)
	seedSystemCIRule(t, database, teamB)

	entityID := reviewEntity(t, database, "owner/repo#aonly")
	emitCI(reviewRouter(database), entityID, "aidan")

	active, err := testTaskStore(database).FindActiveByEntity(context.Background(), runmode.LocalDefaultOrgID, entityID)
	if err != nil || len(active) != 1 {
		t.Fatalf("expected 1 task, got %d (err=%v)", len(active), err)
	}
	if teamIDValue(&active[0]) != teamA {
		t.Errorf("owner = %q, want teamA %q", teamIDValue(&active[0]), teamA)
	}
	if vis := visTeamsOf(t, database, active[0].ID); len(vis) != 1 || vis[0] != teamA {
		t.Errorf("visibility = %v, want [%s] (team B never sees it)", vis, teamA)
	}
}

// TestAuthorCentric_AuthorOnTwoTeams_NullOwnerVisibleBoth: the author maps to
// two teams with no structural signal, so the router can't pick a single
// owner — team_id is NULL and the task is visible to both teams' queues.
func TestAuthorCentric_AuthorOnTwoTeams_NullOwnerVisibleBoth(t *testing.T) {
	database := newTestDB(t)
	seedHandlerFKTargets(t, database)
	setReviewHost(t, database)

	teamA := seedTeam(t, database, "team-a")
	teamB := seedTeam(t, database, "team-b")
	// One login bound to two TF users, one per team — the tier-4 union > 1.
	seedUserOnTeam(t, database, teamA, "aidan")
	seedUserOnTeam(t, database, teamB, "aidan")
	seedSystemCIRule(t, database, teamA)
	seedSystemCIRule(t, database, teamB)

	entityID := reviewEntity(t, database, "owner/repo#both")
	emitCI(reviewRouter(database), entityID, "aidan")

	active, err := testTaskStore(database).FindActiveByEntity(context.Background(), runmode.LocalDefaultOrgID, entityID)
	if err != nil || len(active) != 1 {
		t.Fatalf("expected 1 task, got %d (err=%v)", len(active), err)
	}
	if active[0].TeamID != nil {
		t.Errorf("owner = %v, want nil (unresolved owner: author on two teams)", *active[0].TeamID)
	}
	vis := visTeamsOf(t, database, active[0].ID)
	if len(vis) != 2 || vis[0] != minStr(teamA, teamB) || vis[1] != maxStr(teamA, teamB) {
		t.Errorf("visibility = %v, want both teams (%s, %s)", vis, teamA, teamB)
	}
}

// TestAuthorCentric_ProjectOwned_OwnerIsProjectTeam: an entity attached to a
// team-visibility project routes to the project's team regardless of who
// authored the PR (tier 2 beats the author tier).
func TestAuthorCentric_ProjectOwned_OwnerIsProjectTeam(t *testing.T) {
	database := newTestDB(t)
	seedHandlerFKTargets(t, database)
	setReviewHost(t, database)

	teamProject := seedTeam(t, database, "project-team")
	teamAuthor := seedTeam(t, database, "author-team")
	seedUserOnTeam(t, database, teamAuthor, "aidan")
	seedSystemCIRule(t, database, teamProject)
	seedSystemCIRule(t, database, teamAuthor)

	// A team-visibility project owned by teamProject, with an entity attached.
	// Raw insert (not Projects.Create) because the SQLite store pins every
	// project to the local team — we need a distinct team here to prove tier 2
	// beats the author tier.
	st := sqlitestore.New(database)
	projectID := "proj-" + teamProject[:8]
	if _, err := database.Exec(`
		INSERT INTO projects (id, name, pinned_repos, org_id, team_id, creator_user_id, visibility, created_at, updated_at)
		VALUES (?, 'Proj', '[]', ?, ?, ?, 'team', datetime('now'), datetime('now'))
	`, projectID, runmode.LocalDefaultOrgID, teamProject, runmode.LocalDefaultUserID); err != nil {
		t.Fatalf("create project: %v", err)
	}
	entityID := reviewEntity(t, database, "owner/repo#proj")
	if err := st.Entities.AssignProject(context.Background(), runmode.LocalDefaultOrgID, entityID, &projectID, "test"); err != nil {
		t.Fatalf("assign project: %v", err)
	}

	emitCI(reviewRouter(database), entityID, "aidan") // author on teamAuthor

	active, err := testTaskStore(database).FindActiveByEntity(context.Background(), runmode.LocalDefaultOrgID, entityID)
	if err != nil || len(active) != 1 {
		t.Fatalf("expected 1 task, got %d (err=%v)", len(active), err)
	}
	if teamIDValue(&active[0]) != teamProject {
		t.Errorf("owner = %q, want the project's team %q (not the author's %q)", teamIDValue(&active[0]), teamProject, teamAuthor)
	}
}

// TestAuthorCentric_OverrideOwningTeam: entities.owning_team_id pins the owner
// (tier 1) ahead of project, prior-task, and author tiers.
func TestAuthorCentric_OverrideOwningTeam(t *testing.T) {
	database := newTestDB(t)
	seedHandlerFKTargets(t, database)
	setReviewHost(t, database)

	teamOverride := seedTeam(t, database, "override-team")
	teamAuthor := seedTeam(t, database, "author-team")
	seedUserOnTeam(t, database, teamAuthor, "aidan")
	seedSystemCIRule(t, database, teamOverride)
	seedSystemCIRule(t, database, teamAuthor)

	entityID := reviewEntity(t, database, "owner/repo#override")
	if _, err := database.Exec(`UPDATE entities SET owning_team_id = ? WHERE id = ?`, teamOverride, entityID); err != nil {
		t.Fatalf("set owning_team_id: %v", err)
	}

	emitCI(reviewRouter(database), entityID, "aidan")

	active, err := testTaskStore(database).FindActiveByEntity(context.Background(), runmode.LocalDefaultOrgID, entityID)
	if err != nil || len(active) != 1 {
		t.Fatalf("expected 1 task, got %d (err=%v)", len(active), err)
	}
	if teamIDValue(&active[0]) != teamOverride {
		t.Errorf("owner = %q, want the override team %q", teamIDValue(&active[0]), teamOverride)
	}
}

// TestAuthorCentric_PriorTaskAnchorsOwner (tier 3): once an entity has an
// owned author-centric task, a later author-centric event anchors to that same
// team even when the second event's author maps elsewhere.
func TestAuthorCentric_PriorTaskAnchorsOwner(t *testing.T) {
	database := newTestDB(t)
	seedHandlerFKTargets(t, database)
	setReviewHost(t, database)

	teamA := seedTeam(t, database, "team-a")
	teamB := seedTeam(t, database, "team-b")
	seedUserOnTeam(t, database, teamA, "aidan")
	seedUserOnTeam(t, database, teamB, "bob")
	seedSystemCIRule(t, database, teamA)
	seedSystemCIRule(t, database, teamB)
	seedSystemRule(t, database, teamA, domain.EventGitHubPRConflicts)
	seedSystemRule(t, database, teamB, domain.EventGitHubPRConflicts)

	entityID := reviewEntity(t, database, "owner/repo#anchor")
	router := reviewRouter(database)

	// First: a CI failure from aidan (team A) establishes A as the owner.
	emitCI(router, entityID, "aidan")

	// Then: a conflicts event from bob (team B). Tier 3 (prior owned
	// author-centric task) anchors it to A, not bob's team B.
	conflictMeta, _ := json.Marshal(events.GitHubPRConflictsMetadata{Author: "bob", Repo: "owner/repo"})
	router.HandleEvent(domain.Event{
		EventType:    domain.EventGitHubPRConflicts,
		EntityID:     &entityID,
		MetadataJSON: string(conflictMeta),
		OrgID:        runmode.LocalDefaultOrgID,
	})

	conflictTasks, err := testTaskStore(database).FindActiveByEntityAndType(context.Background(), runmode.LocalDefaultOrgID, entityID, domain.EventGitHubPRConflicts)
	if err != nil || len(conflictTasks) != 1 {
		t.Fatalf("expected 1 conflicts task, got %d (err=%v)", len(conflictTasks), err)
	}
	if teamIDValue(&conflictTasks[0]) != teamA {
		t.Errorf("conflicts owner = %q, want teamA %q (prior-task anchor, not bob's team)", teamIDValue(&conflictTasks[0]), teamA)
	}
}

// TestAuthorCentric_ReviewFirstTrap_CIFallsToAuthor is the review-first
// regression guard: a review_requested task on the reviewer's team must NOT
// anchor a later CI failure (tier 3 excludes review tasks), which then falls to
// the author tier.
func TestAuthorCentric_ReviewFirstTrap_CIFallsToAuthor(t *testing.T) {
	database := newTestDB(t)
	seedHandlerFKTargets(t, database)
	setReviewHost(t, database)

	teamAuthor := seedTeam(t, database, "author-team")
	teamReviewer := seedTeam(t, database, "reviewer-team")
	seedUserOnTeam(t, database, teamAuthor, "aidan")    // PR author
	seedUserOnTeam(t, database, teamReviewer, "rachel") // reviewer
	seedReviewRule(t, database, runmode.LocalDefaultTeamID)
	seedSystemCIRule(t, database, teamAuthor)
	seedSystemCIRule(t, database, teamReviewer)

	entityID := reviewEntity(t, database, "owner/repo#reviewfirst")
	router := reviewRouter(database)

	// Review requested from rachel first → a review task on the reviewer's team.
	emitReviewRequested(router, entityID, "user:rachel", events.GitHubPRReviewRequestedMetadata{
		Author: "aidan", Repo: "owner/repo", PRNumber: 1, RequestedLogin: "rachel",
	})
	// Then CI fails. It must NOT route to the reviewer's team — review tasks
	// don't confer entity ownership — so it falls to the author (teamAuthor).
	emitCI(router, entityID, "aidan")

	ci, err := testTaskStore(database).FindActiveByEntityAndType(context.Background(), runmode.LocalDefaultOrgID, entityID, domain.EventGitHubPRCICheckFailed)
	if err != nil || len(ci) != 1 {
		t.Fatalf("expected 1 CI task, got %d (err=%v)", len(ci), err)
	}
	if teamIDValue(&ci[0]) != teamAuthor {
		t.Errorf("CI owner = %q, want author's team %q (must not inherit the reviewer's team)", teamIDValue(&ci[0]), teamAuthor)
	}
}

// TestAuthorCentric_ExternalAuthor_WatchRuleSurfaces: an external/dependabot
// author resolves to no team, but an explicit user-authored watch rule on team
// C still surfaces the task (owner NULL, C in the visibility set) — the opt-in
// that brings external CI onto a team's board.
func TestAuthorCentric_ExternalAuthor_WatchRuleSurfaces(t *testing.T) {
	database := newTestDB(t)
	seedHandlerFKTargets(t, database)
	setReviewHost(t, database)

	teamC := seedTeam(t, database, "watch-team")
	seedMatchAllCIRule(t, database, teamC) // user-authored watch rule

	entityID := reviewEntity(t, database, "owner/repo#dependabot")
	emitCI(reviewRouter(database), entityID, "dependabot[bot]") // not a TF user

	active, err := testTaskStore(database).FindActiveByEntity(context.Background(), runmode.LocalDefaultOrgID, entityID)
	if err != nil || len(active) != 1 {
		t.Fatalf("expected 1 task from the watch rule, got %d (err=%v)", len(active), err)
	}
	if active[0].TeamID != nil {
		t.Errorf("owner = %v, want nil (external author has no owning team)", *active[0].TeamID)
	}
	if vis := visTeamsOf(t, database, active[0].ID); len(vis) != 1 || vis[0] != teamC {
		t.Errorf("visibility = %v, want [%s] (the watching team)", vis, teamC)
	}
}

// TestAuthorCentric_NullOwner_NoAutoFire pins the auto-fire gate: an
// unresolved-owner task (author on two teams) does not auto-delegate even with
// an enabled trigger — the bot never claims an unowned task. Resolution waits
// for a human claim.
func TestAuthorCentric_NullOwner_NoAutoFire(t *testing.T) {
	database := newTestDB(t)
	seedHandlerFKTargets(t, database)
	setReviewHost(t, database)
	st := sqlitestore.New(database)

	teamA := seedTeam(t, database, "team-a")
	teamB := seedTeam(t, database, "team-b")
	seedUserOnTeam(t, database, teamA, "aidan")
	seedUserOnTeam(t, database, teamB, "aidan") // two teams → NULL owner

	// Both teams fully opt into auto-delegation and have an immediate trigger.
	if _, err := database.Exec(`INSERT INTO team_settings (team_id, auto_delegate_enabled) VALUES (?, 1), (?, 1)`, teamA, teamB); err != nil {
		t.Fatalf("seed team settings: %v", err)
	}
	if _, err := database.Exec(`INSERT OR IGNORE INTO agents (id, org_id, display_name) VALUES (?, ?, 'Bot')`, runmode.LocalDefaultAgentID, runmode.LocalDefaultOrgID); err != nil {
		t.Fatalf("seed agent: %v", err)
	}
	for _, tm := range []string{teamA, teamB} {
		if err := st.TeamAgents.AddForTeam(context.Background(), runmode.LocalDefaultOrgID, tm, runmode.LocalDefaultAgentID); err != nil {
			t.Fatalf("add agent to %s: %v", tm, err)
		}
		pid := "p-nf-" + tm[:8]
		insertPromptForTeam(t, database, pid, tm)
		bp := insertBlueprintForTeam(t, database, "bp-nf-"+tm[:8], pid, tm)
		if _, err := database.Exec(`
			INSERT INTO event_handlers
				(id, org_id, team_id, creator_user_id, kind, event_type,
				 scope_predicate_json, enabled, source, blueprint_id, breaker_threshold, min_autonomy_suitability, created_at, updated_at)
			VALUES (?, ?, ?, ?, 'trigger', ?, NULL, 1, 'user', ?, 4, 0, datetime('now'), datetime('now'))
		`, "trig-nf-"+tm[:8], runmode.LocalDefaultOrgID, tm, runmode.LocalDefaultUserID,
			domain.EventGitHubPRCICheckFailed, bp); err != nil {
			t.Fatalf("seed trigger for %s: %v", tm, err)
		}
	}

	entityID := reviewEntity(t, database, "owner/repo#nofire")
	stub := &stubDelegator{db: database}
	router := NewRouter(testPromptStore(database), testBlueprintStore(database), testEventHandlerStore(database),
		st.Agents, st.TeamAgents, st.Users, testTaskStore(database), st.AgentRuns, st.Entities, st.PendingFirings, st.Events,
		st.Orgs, st.Teams, nil, nil, nil, stub, noopScorer{}, websocket.NewHub())

	emitCI(router, entityID, "aidan")

	active, err := testTaskStore(database).FindActiveByEntity(context.Background(), runmode.LocalDefaultOrgID, entityID)
	if err != nil || len(active) != 1 {
		t.Fatalf("expected 1 task, got %d (err=%v)", len(active), err)
	}
	if active[0].TeamID != nil {
		t.Errorf("owner = %v, want nil (unresolved)", *active[0].TeamID)
	}
	if active[0].ClaimedByAgentID != "" {
		t.Errorf("task was bot-claimed (%q); an unowned task must not auto-fire", active[0].ClaimedByAgentID)
	}
	if stub.calls != 0 {
		t.Errorf("Delegate called %d times; an unowned task must not auto-fire", stub.calls)
	}
}

func minStr(a, b string) string {
	if a < b {
		return a
	}
	return b
}

func maxStr(a, b string) string {
	if a < b {
		return b
	}
	return a
}
