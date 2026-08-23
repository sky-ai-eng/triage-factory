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

// Author-centric routing tests. These exercise the owning-team
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
	router.HandleEvent(context.Background(), domain.Event{
		EventType:    domain.EventGitHubPRCICheckFailed,
		EntityID:     &entityID,
		DedupKey:     "build",
		MetadataJSON: string(metaJSON),
		OccurredAt:   time.Now(),
		OrgID:        runmode.LocalDefaultOrgID,
	})
}

// seedSystemRule seeds a system-source (shipped-style) match-all rule for the
// given event type on teamID. It ships applies_to_unowned=false (the default),
// so it gates creation but — unlike an applies_to_unowned ("watch") rule —
// never grants visibility on its own, so on its own it can't surface a task for
// a non-owner. The id/slug embed the event so several types can be seeded per
// team.
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
// author belongs to team A only (no override/prior task), so a CI
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
	// One login bound to two TF users, one per team — the tier-3 union > 1.
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

// TestAuthorCentric_OverrideOwningTeam: entities.owning_team_id pins the owner
// (tier 1) ahead of the prior-task and author tiers.
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
	// Through the real writer, not a raw UPDATE: this is the exact pairing the
	// bot-opened-PR path relies on — the exec funnel stamps the commissioning
	// team, and routing reads it back as tier 1 — so the two halves are proven
	// against each other rather than against a hand-written column poke.
	stamped, err := sqlitestore.New(database).Entities.StampOwningTeamIfUnsetSystem(
		context.Background(), runmode.LocalDefaultOrgID, entityID, teamOverride)
	if err != nil || !stamped {
		t.Fatalf("stamp owning team: stamped=%v err=%v", stamped, err)
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

// TestAuthorCentric_PriorTaskAnchorsOwner (tier 2): once an entity has an
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

	// Then: a conflicts event from bob (team B). Tier 2 (prior owned
	// author-centric task) anchors it to A, not bob's team B.
	conflictMeta, _ := json.Marshal(events.GitHubPRConflictsMetadata{Author: "bob", Repo: "owner/repo"})
	router.HandleEvent(context.Background(), domain.Event{
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
// anchor a later CI failure (tier 2 excludes review tasks), which then falls to
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

// TestAuthorCentric_ExternalAuthor_WatchRuleSurfaces is the flag-ON case
// (TFAC-517): an external/dependabot author resolves to no team, but a rule with
// applies_to_unowned=true on team C still surfaces the task (owner NULL, C in
// the visibility set) — the explicit opt-in that brings external CI onto a
// team's board. The flag-OFF mirror (no task) is
// TestAuthorCentric_ExternalAuthor_FlagOff_NoTask.
func TestAuthorCentric_ExternalAuthor_WatchRuleSurfaces(t *testing.T) {
	database := newTestDB(t)
	seedHandlerFKTargets(t, database)
	setReviewHost(t, database)

	teamC := seedTeam(t, database, "watch-team")
	seedMatchAllCIRule(t, database, teamC) // applies_to_unowned=true watch rule

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

// seedFlagOffCIRule inserts a user-source match-all ci_check_failed rule with
// applies_to_unowned=false (the TFAC-517 default) on teamID. Unlike
// seedMatchAllCIRule (flag on), it contributes visibility ONLY when the
// owning-team ladder resolves teamID as owner — it never broadens to unowned
// entities. The "user" source proves the broadening is gated on the flag, not
// on provenance (the heuristic TFAC-517 removed).
func seedFlagOffCIRule(t *testing.T, database *sql.DB, teamID string) {
	t.Helper()
	if _, err := database.Exec(`
		INSERT INTO event_handlers
			(id, org_id, team_id, creator_user_id, kind, event_type,
			 scope_predicate_json, enabled, source, applies_to_unowned, name, default_priority, sort_order,
			 created_at, updated_at)
		VALUES (?, ?, ?, ?, 'rule', ?, NULL, 1, 'user', 0, ?, 0.7, 100, ?, ?)
	`, "flagoff-"+teamID[:8], runmode.LocalDefaultOrgID, teamID, runmode.LocalDefaultUserID,
		domain.EventGitHubPRCICheckFailed, "flag-off CI rule "+teamID[:8], time.Now(), time.Now()); err != nil {
		t.Fatalf("seed flag-off CI rule for team %s: %v", teamID, err)
	}
}

// TestAuthorCentric_ExternalAuthor_FlagOff_NoTask is the headline default-flip
// (TFAC-517): the SAME match-all user rule as the watch test, but with
// applies_to_unowned=false (the new default). An external author resolves to no
// team and a flag-off rule grants no visibility on its own, so the event is
// recorded but mints NO task for the would-be watcher. This is the behavior the
// flag restores: a rule no longer broadens by accident.
func TestAuthorCentric_ExternalAuthor_FlagOff_NoTask(t *testing.T) {
	database := newTestDB(t)
	seedHandlerFKTargets(t, database)
	setReviewHost(t, database)

	teamC := seedTeam(t, database, "watch-team")
	seedFlagOffCIRule(t, database, teamC) // user rule, applies_to_unowned=false

	entityID := reviewEntity(t, database, "owner/repo#dependabot-off")
	emitCI(reviewRouter(database), entityID, "dependabot[bot]") // not a TF user

	active, err := testTaskStore(database).FindActiveByEntity(context.Background(), runmode.LocalDefaultOrgID, entityID)
	if err != nil {
		t.Fatalf("list tasks: %v", err)
	}
	if len(active) != 0 {
		t.Fatalf("expected no task (flag off → no broadening), got %d", len(active))
	}
}

// TestAuthorCentric_OwnedEntity_FlagOff_VisibleToTeam is the owned-entity half
// of the default-flip (TFAC-517): with the flag OFF, a rule still surfaces a
// task to its team when the team OWNS the entity — visibility rides ownership,
// not the flag. The author is on teamA (the owner); a flag-off rule on teamA
// surfaces the task, while a flag-off rule on the non-owning teamB does NOT pull
// teamB into visibility (proving flag-off never broadens).
func TestAuthorCentric_OwnedEntity_FlagOff_VisibleToTeam(t *testing.T) {
	database := newTestDB(t)
	seedHandlerFKTargets(t, database)
	setReviewHost(t, database)

	teamA := seedTeam(t, database, "team-a")
	teamB := seedTeam(t, database, "team-b")
	seedUserOnTeam(t, database, teamA, "aidan") // author owns via teamA
	seedFlagOffCIRule(t, database, teamA)       // owner's rule, flag off
	seedFlagOffCIRule(t, database, teamB)       // non-owner's rule, flag off → no broadening

	entityID := reviewEntity(t, database, "owner/repo#owned-flagoff")
	emitCI(reviewRouter(database), entityID, "aidan")

	active, err := testTaskStore(database).FindActiveByEntity(context.Background(), runmode.LocalDefaultOrgID, entityID)
	if err != nil || len(active) != 1 {
		t.Fatalf("expected 1 task on the owned entity, got %d (err=%v)", len(active), err)
	}
	if teamIDValue(&active[0]) != teamA {
		t.Errorf("owner = %q, want teamA %q", teamIDValue(&active[0]), teamA)
	}
	if vis := visTeamsOf(t, database, active[0].ID); len(vis) != 1 || vis[0] != teamA {
		t.Errorf("visibility = %v, want [%s] only (teamB's flag-off rule must not broaden)", vis, teamA)
	}
}

// TestAuthorCentric_TwoTeams_FiresDeterministically is the TFAC-514 headline
// repro: an author on two teams, each with an enabled ci-fix trigger, no prior
// owner. The task is created NULL-owned but — unlike before — fires
// deterministically rather than not at all: exactly one team's trigger runs
// (the deterministic order winner: equal priority → lowest team id), and the
// fire consolidates the NULL owner onto that team.
func TestAuthorCentric_TwoTeams_FiresDeterministically(t *testing.T) {
	database := newTestDB(t)
	seedHandlerFKTargets(t, database)
	setReviewHost(t, database)

	teamA := seedTeam(t, database, "team-a")
	teamB := seedTeam(t, database, "team-b")
	seedUserOnTeam(t, database, teamA, "aidan")
	seedUserOnTeam(t, database, teamB, "aidan") // author on two teams → ambiguous owner

	// Both teams fully opt into auto-delegation and have an immediate ci-fix
	// trigger. No rules → both tie at the 0.5 trigger-default priority, so the
	// deterministic firing order falls to lowest team id.
	enableTeamAutoDelegate(t, database, teamA, teamB)
	seedImmediateTrigger(t, database, teamA, domain.EventGitHubPRCICheckFailed, "ci-a")
	seedImmediateTrigger(t, database, teamB, domain.EventGitHubPRCICheckFailed, "ci-b")

	entityID := reviewEntity(t, database, "owner/repo#fires")
	stub := &stubDelegator{db: database}
	router := firingRouter(database, stub)

	emitCI(router, entityID, "aidan")

	active, err := testTaskStore(database).FindActiveByEntity(context.Background(), runmode.LocalDefaultOrgID, entityID)
	if err != nil || len(active) != 1 {
		t.Fatalf("expected 1 task, got %d (err=%v)", len(active), err)
	}
	// Exactly one team's blueprint ran (the exclusive claim CAS serializes the
	// two firing teams) and the owner consolidated to the deterministic winner.
	if stub.calls != 1 {
		t.Fatalf("expected exactly 1 fire (deterministic), got %d", stub.calls)
	}
	want := minStr(teamA, teamB)
	if teamIDValue(&active[0]) != want {
		t.Errorf("owner = %q after fire, want the deterministic winner %q (priority tie → lowest team id)", teamIDValue(&active[0]), want)
	}
	if active[0].ClaimedByAgentID == "" {
		t.Error("task should be bot-claimed by the firing team")
	}
	// Still visible to both teams — the loser keeps its queue visibility.
	if vis := visTeamsOf(t, database, active[0].ID); len(vis) != 2 {
		t.Errorf("visibility = %v, want both teams", vis)
	}
}

// TestAuthorCentric_ConsolidatedOwnerAnchorsNextEvent pins that tier-2 anchoring
// still works after the TFAC-514 deterministic fire: a multi-team author's first
// event consolidates the owner onto the firing team, and a second author-centric
// event on the same entity resolves to that same team via tier 2 (the prior
// owned author-centric task), not back to the ambiguous author tier.
func TestAuthorCentric_ConsolidatedOwnerAnchorsNextEvent(t *testing.T) {
	database := newTestDB(t)
	seedHandlerFKTargets(t, database)
	setReviewHost(t, database)

	teamA := seedTeam(t, database, "team-a")
	teamB := seedTeam(t, database, "team-b")
	seedUserOnTeam(t, database, teamA, "aidan")
	seedUserOnTeam(t, database, teamB, "aidan") // author on two teams → ambiguous owner
	enableTeamAutoDelegate(t, database, teamA, teamB)
	seedImmediateTrigger(t, database, teamA, domain.EventGitHubPRCICheckFailed, "ci-a")
	seedImmediateTrigger(t, database, teamB, domain.EventGitHubPRCICheckFailed, "ci-b")
	// A conflicts rule on each team gates creation of the second task (no
	// trigger → it won't fire, we only assert its owner).
	seedSystemRule(t, database, teamA, domain.EventGitHubPRConflicts)
	seedSystemRule(t, database, teamB, domain.EventGitHubPRConflicts)

	entityID := reviewEntity(t, database, "owner/repo#anchorfire")
	stub := &stubDelegator{db: database}
	router := firingRouter(database, stub)

	// First CI failure: ambiguous owner → deterministic team fires and
	// consolidates ownership onto itself.
	emitCI(router, entityID, "aidan")
	ci, err := testTaskStore(database).FindActiveByEntityAndType(context.Background(), runmode.LocalDefaultOrgID, entityID, domain.EventGitHubPRCICheckFailed)
	if err != nil || len(ci) != 1 {
		t.Fatalf("expected 1 CI task, got %d (err=%v)", len(ci), err)
	}
	winner := teamIDValue(&ci[0])
	if winner == "" {
		t.Fatalf("owner not consolidated after fire")
	}

	// Second author-centric event (conflicts): tier 2 anchors it to the
	// consolidated owner, even though the author still maps to both teams.
	conflictMeta, _ := json.Marshal(events.GitHubPRConflictsMetadata{Author: "aidan", Repo: "owner/repo"})
	router.HandleEvent(context.Background(), domain.Event{
		EventType:    domain.EventGitHubPRConflicts,
		EntityID:     &entityID,
		MetadataJSON: string(conflictMeta),
		OrgID:        runmode.LocalDefaultOrgID,
	})
	conflicts, err := testTaskStore(database).FindActiveByEntityAndType(context.Background(), runmode.LocalDefaultOrgID, entityID, domain.EventGitHubPRConflicts)
	if err != nil || len(conflicts) != 1 {
		t.Fatalf("expected 1 conflicts task, got %d (err=%v)", len(conflicts), err)
	}
	if teamIDValue(&conflicts[0]) != winner {
		t.Errorf("conflicts owner = %q, want the consolidated team %q (tier-2 anchor)", teamIDValue(&conflicts[0]), winner)
	}
}

// TestOwnerLadderRouting_AmbiguousOwner_OrdersByPriorityThenID is the unit-level
// guard for the TFAC-514 change: an ambiguous owner (identity on multiple teams)
// yields a NULL ownerTeam at creation but a populated, deterministically-ordered
// orderedTeams — max matched-rule DefaultPriority desc, then lowest team id —
// so a multi-team-ambiguous event fires deterministically instead of not at all.
func TestOwnerLadderRouting_AmbiguousOwner_OrdersByPriorityThenID(t *testing.T) {
	hi, lo := floatPtr(0.9), floatPtr(0.5)
	// team-z carries the higher-priority rule but the lexically later id;
	// team-a the lower priority but earlier id. Priority must beat id order.
	rules := []domain.EventHandler{
		{TeamID: "team-z", Source: domain.EventHandlerSourceUser, DefaultPriority: hi},
		{TeamID: "team-a", Source: domain.EventHandlerSourceUser, DefaultPriority: lo},
	}
	vis, owner, ordered, _, ok := ownerLadderRouting("", []string{"team-a", "team-z"}, rules, nil)
	if !ok {
		t.Fatal("ownerLadderRouting ok=false, want a routable task")
	}
	if owner != "" {
		t.Errorf("ownerTeam = %q, want \"\" (NULL at creation for an ambiguous owner)", owner)
	}
	if len(vis) != 2 {
		t.Errorf("visibleTeams = %v, want both teams", vis)
	}
	if len(ordered) != 2 || ordered[0] != "team-z" || ordered[1] != "team-a" {
		t.Errorf("orderedTeams = %v, want [team-z team-a] (priority desc beats team-id asc)", ordered)
	}

	// Priority tie → lowest team id first.
	tie := []domain.EventHandler{
		{TeamID: "team-z", Source: domain.EventHandlerSourceUser, DefaultPriority: lo},
		{TeamID: "team-a", Source: domain.EventHandlerSourceUser, DefaultPriority: lo},
	}
	_, _, tieOrdered, _, _ := ownerLadderRouting("", []string{"team-a", "team-z"}, tie, nil)
	if len(tieOrdered) != 2 || tieOrdered[0] != "team-a" || tieOrdered[1] != "team-z" {
		t.Errorf("tie orderedTeams = %v, want [team-a team-z] (equal priority → lowest team id)", tieOrdered)
	}

	// Resolved (unambiguous) owner: orderedTeams is exactly the owner — watcher
	// teams get visibility but never firing rights.
	_, _, resolvedOrdered, _, _ := ownerLadderRouting("team-a", []string{"team-a"}, rules, nil)
	if len(resolvedOrdered) != 1 || resolvedOrdered[0] != "team-a" {
		t.Errorf("resolved-owner orderedTeams = %v, want [team-a] only", resolvedOrdered)
	}
}

// TestOwnerLadderRouting_ExternalAuthor_WatchersFireRanked is the unit-level
// guard for the TFAC-517 external branch: when no member owns the entity
// (ownerSet empty — a truly external author), the opted-in watcher teams become
// the firing candidates, ranked the same way (priority desc, then lowest team
// id). The task is still created NULL-owned; the first watcher to win the claim
// consolidates ownership. This is the branch that supersedes 514's "external
// never fires."
func TestOwnerLadderRouting_ExternalAuthor_WatchersFireRanked(t *testing.T) {
	hi, lo := floatPtr(0.9), floatPtr(0.5)
	// Two opted-in watcher teams, no member owner (ownerSet empty). team-w1
	// carries the higher priority but the later-or-equal id, so priority orders.
	watchers := []domain.EventHandler{
		{TeamID: "team-w2", AppliesToUnowned: true, DefaultPriority: lo},
		{TeamID: "team-w1", AppliesToUnowned: true, DefaultPriority: hi},
	}
	vis, owner, ordered, _, ok := ownerLadderRouting("", nil, watchers, nil)
	if !ok {
		t.Fatal("ownerLadderRouting ok=false, want a routable task (the watchers surface it)")
	}
	if owner != "" {
		t.Errorf("ownerTeam = %q, want \"\" (NULL at creation; the fire consolidates ownership)", owner)
	}
	if len(vis) != 2 {
		t.Errorf("visibleTeams = %v, want both watcher teams", vis)
	}
	if len(ordered) != 2 || ordered[0] != "team-w1" || ordered[1] != "team-w2" {
		t.Errorf("orderedTeams = %v, want [team-w1 team-w2] (watchers fire, ranked by priority desc)", ordered)
	}

	// A flag-off rule contributes neither visibility nor firing for an external
	// author: ownerSet empty + no opted-in watcher → no task at all.
	flagOff := []domain.EventHandler{{TeamID: "team-x", DefaultPriority: hi}}
	if _, _, _, _, ok2 := ownerLadderRouting("", nil, flagOff, nil); ok2 {
		t.Error("ownerLadderRouting ok=true for an external author with only a flag-off rule; want no task")
	}

	// Per-handler reach (TFAC-517 option 1): the opt-in can live on a TRIGGER,
	// not just a rule — a team configures orphan reach + auto-delegation entirely
	// on the prompts page. A flag-on trigger (in matchedTriggers, no rule) makes
	// the team a watcher → visible AND in the firing order. Triggers carry no
	// DefaultPriority, so the team ranks at the 0.5 fallback.
	trigOnly := []domain.EventHandler{
		{TeamID: "team-trig", Kind: domain.EventHandlerKindTrigger, AppliesToUnowned: true},
	}
	tvis, _, tordered, _, tok := ownerLadderRouting("", nil, nil, trigOnly)
	if !tok {
		t.Fatal("ownerLadderRouting ok=false for a trigger-only watcher; want a routable task")
	}
	if len(tvis) != 1 || tvis[0] != "team-trig" {
		t.Errorf("visibleTeams = %v, want [team-trig] (the flag-on trigger's team)", tvis)
	}
	if len(tordered) != 1 || tordered[0] != "team-trig" {
		t.Errorf("orderedTeams = %v, want [team-trig] (a flag-on trigger fires on orphans)", tordered)
	}

	// A flag-off trigger (no rule) does NOT reach an orphan — preserves the
	// over-spawn fix: a plain match-all trigger must not fire on every PR.
	trigOff := []domain.EventHandler{
		{TeamID: "team-trig", Kind: domain.EventHandlerKindTrigger},
	}
	if _, _, _, _, ok3 := ownerLadderRouting("", nil, nil, trigOff); ok3 {
		t.Error("ownerLadderRouting ok=true for an external author with only a flag-off trigger; want no task")
	}
}

// TestAuthorCentric_ExternalAuthor_WatchTriggerFires pins the TFAC-517 firing
// behavior for an opted-in watcher: an external/dependabot PR is owned by no
// member, so the team that explicitly opted into reaching unowned entities
// (applies_to_unowned rule) AND has a configured, enabled auto-delegation may
// now fire on it — the deliberate, eyes-open behavior that supersedes the
// TFAC-514 blanket "external never fires." The fire consolidates ownership onto
// the watcher (delegation.go SetOwnerTeamSystem). The no-steal invariant — a
// watcher never beats a MEMBER team — is unaffected and covered by
// TestAuthorCentric_TwoTeams_WatcherDoesNotStealOwnership.
func TestAuthorCentric_ExternalAuthor_WatchTriggerFires(t *testing.T) {
	database := newTestDB(t)
	seedHandlerFKTargets(t, database)
	setReviewHost(t, database)

	teamC := seedTeam(t, database, "watch-team")
	seedMatchAllCIRule(t, database, teamC) // applies_to_unowned watch rule → reach
	enableTeamAutoDelegate(t, database, teamC)
	seedImmediateTrigger(t, database, teamC, domain.EventGitHubPRCICheckFailed, "ci-c")

	entityID := reviewEntity(t, database, "owner/repo#extwatch")
	stub := &stubDelegator{db: database}
	router := firingRouter(database, stub)
	emitCI(router, entityID, "dependabot[bot]") // external — no member owns it

	active, err := testTaskStore(database).FindActiveByEntity(context.Background(), runmode.LocalDefaultOrgID, entityID)
	if err != nil || len(active) != 1 {
		t.Fatalf("expected 1 task from the watch rule, got %d (err=%v)", len(active), err)
	}
	// The opted-in watcher fired and consolidated ownership onto itself: nobody
	// else owns the orphan PR and team C accepted the risk eyes-open.
	if stub.calls != 1 {
		t.Fatalf("expected exactly 1 fire (opted-in watcher acts on the orphan PR), got %d", stub.calls)
	}
	if teamIDValue(&active[0]) != teamC {
		t.Errorf("owner = %q after fire, want the watcher team %q (the fire consolidates ownership)", teamIDValue(&active[0]), teamC)
	}
	if active[0].ClaimedByAgentID == "" {
		t.Error("task should be bot-claimed by the firing watcher team")
	}
	if vis := visTeamsOf(t, database, active[0].ID); len(vis) != 1 || vis[0] != teamC {
		t.Errorf("visibility = %v, want [%s] (the watching team)", vis, teamC)
	}
}

// TestAuthorCentric_ExternalAuthor_WatchTriggerOnly_Fires is the per-handler
// reach path (TFAC-517 option 1): team C opts into orphan reach with a flag-on
// TRIGGER and NO companion task rule — the "configure it all on the prompts
// page" experience. On an external/dependabot PR (no member owner) the trigger
// both surfaces the task and fires, consolidating ownership onto C. Proves the
// reach flag is honored on triggers, not just rules.
func TestAuthorCentric_ExternalAuthor_WatchTriggerOnly_Fires(t *testing.T) {
	database := newTestDB(t)
	seedHandlerFKTargets(t, database)
	setReviewHost(t, database)

	teamC := seedTeam(t, database, "watch-team")
	enableTeamAutoDelegate(t, database, teamC)
	// Flag-on trigger, NO rule — the trigger alone is the team's opt-in.
	seedImmediateWatchTrigger(t, database, teamC, domain.EventGitHubPRCICheckFailed, "ci-c")

	entityID := reviewEntity(t, database, "owner/repo#trigonly")
	stub := &stubDelegator{db: database}
	router := firingRouter(database, stub)
	emitCI(router, entityID, "dependabot[bot]") // external — no member owns it

	active, err := testTaskStore(database).FindActiveByEntity(context.Background(), runmode.LocalDefaultOrgID, entityID)
	if err != nil || len(active) != 1 {
		t.Fatalf("expected 1 task from the watch trigger, got %d (err=%v)", len(active), err)
	}
	if stub.calls != 1 {
		t.Fatalf("expected exactly 1 fire (the flag-on trigger acts on the orphan PR), got %d", stub.calls)
	}
	if teamIDValue(&active[0]) != teamC {
		t.Errorf("owner = %q after fire, want the watcher team %q (fire consolidates ownership)", teamIDValue(&active[0]), teamC)
	}
	if active[0].ClaimedByAgentID == "" {
		t.Error("task should be bot-claimed by the firing watcher team")
	}
	if vis := visTeamsOf(t, database, active[0].ID); len(vis) != 1 || vis[0] != teamC {
		t.Errorf("visibility = %v, want [%s] (the watching team)", vis, teamC)
	}
}

// TestAuthorCentric_ExternalAuthor_PlainTriggerNoReach guards the over-spawn
// fix under option 1: a team with a plain (flag-off) trigger and no watch
// handler must NOT reach an external PR — no task, no fire. Only the explicit
// applies_to_unowned opt-in grants orphan reach.
func TestAuthorCentric_ExternalAuthor_PlainTriggerNoReach(t *testing.T) {
	database := newTestDB(t)
	seedHandlerFKTargets(t, database)
	setReviewHost(t, database)

	teamC := seedTeam(t, database, "no-reach-team")
	enableTeamAutoDelegate(t, database, teamC)
	seedImmediateTrigger(t, database, teamC, domain.EventGitHubPRCICheckFailed, "ci-c") // flag OFF

	entityID := reviewEntity(t, database, "owner/repo#plaintrig")
	stub := &stubDelegator{db: database}
	router := firingRouter(database, stub)
	emitCI(router, entityID, "dependabot[bot]") // external, not a TF user

	active, err := testTaskStore(database).FindActiveByEntity(context.Background(), runmode.LocalDefaultOrgID, entityID)
	if err != nil {
		t.Fatalf("list tasks: %v", err)
	}
	if len(active) != 0 {
		t.Fatalf("expected no task (plain trigger has no orphan reach), got %d", len(active))
	}
	if stub.calls != 0 {
		t.Errorf("Delegate called %d times; a flag-off trigger must not fire on an external PR", stub.calls)
	}
}

// TestAuthorCentric_TwoTeams_WatcherDoesNotStealOwnership pins that firing in the
// ambiguous (multi-team author) case is scoped to the IDENTITY's teams, never a
// watcher. A pure watcher team with the highest-priority rule + an enabled
// trigger must not win the exclusive claim and consolidate itself as owner of a
// PR authored by a member of other teams.
func TestAuthorCentric_TwoTeams_WatcherDoesNotStealOwnership(t *testing.T) {
	database := newTestDB(t)
	seedHandlerFKTargets(t, database)
	setReviewHost(t, database)

	teamA := seedTeam(t, database, "team-a")
	teamB := seedTeam(t, database, "team-b")
	teamC := seedTeam(t, database, "watch-team")
	seedUserOnTeam(t, database, teamA, "aidan")
	seedUserOnTeam(t, database, teamB, "aidan") // author on A and B → ambiguous

	// teamC is a pure watcher: a high-priority applies_to_unowned rule (would
	// sort FIRST if it were in the firing order) plus an enabled trigger.
	seedMatchAllCIRule(t, database, teamC) // priority 0.7, applies_to_unowned=true
	enableTeamAutoDelegate(t, database, teamA, teamB, teamC)
	seedImmediateTrigger(t, database, teamA, domain.EventGitHubPRCICheckFailed, "ci-a")
	seedImmediateTrigger(t, database, teamB, domain.EventGitHubPRCICheckFailed, "ci-b")
	seedImmediateTrigger(t, database, teamC, domain.EventGitHubPRCICheckFailed, "ci-c")

	entityID := reviewEntity(t, database, "owner/repo#nosteal")
	stub := &stubDelegator{db: database}
	router := firingRouter(database, stub)
	emitCI(router, entityID, "aidan")

	active, err := testTaskStore(database).FindActiveByEntity(context.Background(), runmode.LocalDefaultOrgID, entityID)
	if err != nil || len(active) != 1 {
		t.Fatalf("expected 1 task, got %d (err=%v)", len(active), err)
	}
	if stub.calls != 1 {
		t.Fatalf("expected exactly 1 fire, got %d", stub.calls)
	}
	// The winner is one of the author's teams (lowest team id), NOT the watcher,
	// even though the watcher's rule outranks them on priority.
	owner := teamIDValue(&active[0])
	if owner == teamC {
		t.Fatalf("watcher team C stole ownership; firing must be scoped to the author's teams")
	}
	if owner != minStr(teamA, teamB) {
		t.Errorf("owner = %q, want the deterministic author team %q", owner, minStr(teamA, teamB))
	}
	// C still sees the card (visibility), it just can't fire on it.
	if vis := visTeamsOf(t, database, active[0].ID); len(vis) != 3 {
		t.Errorf("visibility = %v, want all three teams (A, B, watcher C)", vis)
	}
}

// firingRouter wires a router with both the identity-routing stores (Users,
// Teams, Orgs, TeamGitHubGroups) and the delegation stores (Agents, TeamAgents)
// plus a stub delegator, for tests that exercise the auto-fire +
// owner-consolidation path end to end.
func firingRouter(database *sql.DB, stub *stubDelegator) *Router {
	st := sqlitestore.New(database)
	return NewRouter(testPromptStore(database), testBlueprintStore(database), testEventHandlerStore(database),
		st.Agents, st.TeamAgents, st.Users, testTaskStore(database), st.Conversations, st.Entities, st.PendingFirings, st.Events,
		st.Orgs, st.Teams, nil, nil, st.TeamGitHubGroups, stub, noopScorer{}, websocket.NewHub())
}

// enableTeamAutoDelegate opts each team fully into auto-delegation: a
// team_settings row with the flag on plus the local agent attached to the team.
func enableTeamAutoDelegate(t *testing.T, database *sql.DB, teams ...string) {
	t.Helper()
	st := sqlitestore.New(database)
	if _, err := database.Exec(`INSERT OR IGNORE INTO agents (id, org_id, display_name) VALUES (?, ?, 'Bot')`,
		runmode.LocalDefaultAgentID, runmode.LocalDefaultOrgID); err != nil {
		t.Fatalf("seed agent: %v", err)
	}
	for _, tm := range teams {
		if _, err := database.Exec(`INSERT OR IGNORE INTO team_settings (team_id, auto_delegate_enabled) VALUES (?, 1)`, tm); err != nil {
			t.Fatalf("seed team settings %s: %v", tm, err)
		}
		if err := st.TeamAgents.AddForTeam(context.Background(), runmode.LocalDefaultOrgID, tm, runmode.LocalDefaultAgentID); err != nil {
			t.Fatalf("add agent to %s: %v", tm, err)
		}
	}
}

// seedImmediateTrigger seeds an enabled event trigger (min_autonomy_suitability
// 0 → fires immediately, no post-scoring deferral) for eventType on teamID,
// backed by its own team-owned prompt+blueprint (the same-team FK). idSuffix
// disambiguates several triggers within one test.
func seedImmediateTrigger(t *testing.T, database *sql.DB, teamID, eventType, idSuffix string) {
	t.Helper()
	pid := "p-" + idSuffix
	insertPromptForTeam(t, database, pid, teamID)
	bp := insertBlueprintForTeam(t, database, "bp-"+idSuffix, pid, teamID)
	if _, err := database.Exec(`
		INSERT INTO event_handlers
			(id, org_id, team_id, creator_user_id, kind, event_type,
			 scope_predicate_json, enabled, source, blueprint_id, breaker_threshold, min_autonomy_suitability, created_at, updated_at)
		VALUES (?, ?, ?, ?, 'trigger', ?, NULL, 1, 'user', ?, 4, 0, datetime('now'), datetime('now'))
	`, "trig-"+idSuffix, runmode.LocalDefaultOrgID, teamID, runmode.LocalDefaultUserID, eventType, bp); err != nil {
		t.Fatalf("seed trigger %s for %s: %v", idSuffix, teamID, err)
	}
}

// seedImmediateWatchTrigger is seedImmediateTrigger with applies_to_unowned=1 —
// a trigger that opts its team into reaching unowned entities (TFAC-517 option
// 1). With no companion rule, this alone makes the team a watcher that fires on
// orphan PRs, the "configure it all on the prompts page" path.
func seedImmediateWatchTrigger(t *testing.T, database *sql.DB, teamID, eventType, idSuffix string) {
	t.Helper()
	pid := "p-" + idSuffix
	insertPromptForTeam(t, database, pid, teamID)
	bp := insertBlueprintForTeam(t, database, "bp-"+idSuffix, pid, teamID)
	if _, err := database.Exec(`
		INSERT INTO event_handlers
			(id, org_id, team_id, creator_user_id, kind, event_type,
			 scope_predicate_json, enabled, source, applies_to_unowned, blueprint_id, breaker_threshold, min_autonomy_suitability, created_at, updated_at)
		VALUES (?, ?, ?, ?, 'trigger', ?, NULL, 1, 'user', 1, ?, 4, 0, datetime('now'), datetime('now'))
	`, "trig-"+idSuffix, runmode.LocalDefaultOrgID, teamID, runmode.LocalDefaultUserID, eventType, bp); err != nil {
		t.Fatalf("seed watch trigger %s for %s: %v", idSuffix, teamID, err)
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
