package routing

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/domain/events"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// Assignee-centric routing tests. These exercise the owning-team ladder for
// jira:issue:* events that concern whoever IS ASSIGNED the issue —
// assignment, atomic-discovery, status, priority, and comment activity — the
// Jira mirror of the author-centric GitHub routing in author_routing_test.go.
// jira:issue:available (the unassigned team-pool signal) and
// jira:issue:completed (entity-terminating) deliberately stay on handler-team
// routing and are NOT covered here. The shared helpers (seedTeam, visTeamsOf,
// testTaskStore, reviewRouter) live in review_routing_test.go.

// jiraTestHost is the org's Jira host. Identities are bound to it and
// org_event_sources' jira row's base_url is set to it so the router's
// assignee→teams resolution finds the same key the writers stored.
const jiraTestHost = "https://acme.atlassian.net"

// setJiraHost stamps the jira row's base_url on org_event_sources for the
// default org. Safe to call alongside setReviewHost — the two set different
// kinds' rows.
func setJiraHost(t *testing.T, database *sql.DB) {
	t.Helper()
	if _, err := database.Exec(`
		INSERT INTO org_event_sources (org_id, kind, base_url) VALUES (?, 'jira', ?)
		ON CONFLICT(org_id, kind) DO UPDATE SET base_url = excluded.base_url
	`, runmode.LocalDefaultOrgID, jiraTestHost); err != nil {
		t.Fatalf("set org jira base_url: %v", err)
	}
}

// seedJiraUserOnTeam creates a user, enrolls them on teamID, and binds the
// Atlassian accountID on the jira test host.
func seedJiraUserOnTeam(t *testing.T, database *sql.DB, teamID, accountID, displayName string) string {
	t.Helper()
	uid := uuid.New().String()
	if _, err := database.Exec(`INSERT INTO users (id, display_name) VALUES (?, ?)`, uid, displayName); err != nil {
		t.Fatalf("seed user %s: %v", displayName, err)
	}
	if _, err := database.Exec(`INSERT INTO memberships (user_id, team_id, role) VALUES (?, ?, 'admin')`, uid, teamID); err != nil {
		t.Fatalf("seed membership %s→%s: %v", displayName, teamID, err)
	}
	if err := sqlitestore.New(database).Users.UpsertJiraIdentity(context.Background(), uid, jiraTestHost, accountID, displayName, "pat"); err != nil {
		t.Fatalf("bind jira identity %s: %v", accountID, err)
	}
	return uid
}

// seedSystemJiraRule seeds a system-source (shipped-style) match-all rule for
// the given jira event type on teamID. Like its GitHub sibling seedSystemRule,
// a system rule gates creation but never grants visibility on its own.
func seedSystemJiraRule(t *testing.T, database *sql.DB, teamID, eventType string) {
	t.Helper()
	key := teamID[:8] + "-" + eventType[len(eventType)-6:]
	if _, err := database.Exec(`
		INSERT INTO event_handlers
			(id, org_id, team_id, creator_user_id, kind, event_type,
			 system_slug, scope_predicate_json, enabled, source, name, default_priority, sort_order,
			 created_at, updated_at)
		VALUES (?, ?, ?, NULL, 'rule', ?, ?, '{"assignee_in":[]}', 1, 'system', 'sys jira rule', 0.75, 0, ?, ?)
	`, "sysj-"+key, runmode.LocalDefaultOrgID, teamID,
		eventType, "system-jira-rule-"+key, time.Now(), time.Now()); err != nil {
		t.Fatalf("seed system jira rule (%s) for team %s: %v", eventType, teamID, err)
	}
}

// seedUserJiraRule seeds an applies_to_unowned ("watch") match-all rule for the
// given jira event type on teamID. The flag widens an assignee-centric task's
// visibility to its team even when the team isn't the owner (TFAC-517).
func seedUserJiraRule(t *testing.T, database *sql.DB, teamID, eventType string) {
	t.Helper()
	if _, err := database.Exec(`
		INSERT INTO event_handlers
			(id, org_id, team_id, creator_user_id, kind, event_type,
			 scope_predicate_json, enabled, source, applies_to_unowned, name, default_priority, sort_order,
			 created_at, updated_at)
		VALUES (?, ?, ?, ?, 'rule', ?, '{"assignee_in":[]}', 1, 'user', 1, ?, 0.7, 100, ?, ?)
	`, uuid.New().String(), runmode.LocalDefaultOrgID, teamID, runmode.LocalDefaultUserID,
		eventType, "user jira rule "+teamID[:8], time.Now(), time.Now()); err != nil {
		t.Fatalf("seed user jira rule (%s) for team %s: %v", eventType, teamID, err)
	}
}

func jiraEntity(t *testing.T, database *sql.DB, sourceID string) string {
	t.Helper()
	e, _, err := sqlitestore.New(database).Entities.FindOrCreate(context.Background(), runmode.LocalDefaultOrgID, "jira", sourceID, "issue", "Jira issue", "https://example.com/"+sourceID)
	if err != nil {
		t.Fatalf("create jira entity: %v", err)
	}
	return e.ID
}

// emitJiraAssigned fires a jira:issue:assigned event for entityID assigned to
// the Atlassian accountID.
func emitJiraAssigned(router *Router, entityID, accountID string) {
	meta := events.JiraIssueAssignedMetadata{
		Assignee: "Display " + accountID, AssigneeAccountID: accountID,
		IssueKey: "SKY-1", Project: "SKY", Status: "To Do",
	}
	metaJSON, _ := json.Marshal(meta)
	router.HandleEvent(context.Background(), domain.Event{
		EventType:    domain.EventJiraIssueAssigned,
		EntityID:     &entityID,
		MetadataJSON: string(metaJSON),
		OccurredAt:   time.Now(),
		OrgID:        runmode.LocalDefaultOrgID,
	})
}

// TestAssigneeCentric_NonMemberAssignee_NoTask is the headline bug fix (local
// N=1): org-wide polling surfaces an assignment to someone who isn't a TF
// user. With only a system rule (no explicit watch) and no resolvable owner,
// the event is recorded but mints NO task. (Was: a task for any assignee.)
func TestAssigneeCentric_NonMemberAssignee_NoTask(t *testing.T) {
	database := newTestDB(t)
	seedHandlerFKTargets(t, database)
	setJiraHost(t, database)
	seedJiraUserOnTeam(t, database, runmode.LocalDefaultTeamID, "acct-aidan", "aidan") // the local user
	seedSystemJiraRule(t, database, runmode.LocalDefaultTeamID, domain.EventJiraIssueAssigned)

	entityID := jiraEntity(t, database, "SKY-stranger")
	emitJiraAssigned(reviewRouter(database), entityID, "acct-stranger") // not a TF user

	active, err := testTaskStore(database).FindActiveByEntity(context.Background(), runmode.LocalDefaultOrgID, entityID)
	if err != nil {
		t.Fatalf("list tasks: %v", err)
	}
	if len(active) != 0 {
		t.Fatalf("expected no task for a non-member assignee (over-spawn fix), got %d", len(active))
	}
}

// TestAssigneeCentric_LocalUserAssignee_OneTeam is the positive of the bug
// fix: the same system rule, but the issue is assigned to the local user — the
// assignee resolves to the one team, so a single task lands there.
func TestAssigneeCentric_LocalUserAssignee_OneTeam(t *testing.T) {
	database := newTestDB(t)
	seedHandlerFKTargets(t, database)
	setJiraHost(t, database)
	seedJiraUserOnTeam(t, database, runmode.LocalDefaultTeamID, "acct-aidan", "aidan")
	seedSystemJiraRule(t, database, runmode.LocalDefaultTeamID, domain.EventJiraIssueAssigned)

	entityID := jiraEntity(t, database, "SKY-mine")
	emitJiraAssigned(reviewRouter(database), entityID, "acct-aidan")

	active, err := testTaskStore(database).FindActiveByEntity(context.Background(), runmode.LocalDefaultOrgID, entityID)
	if err != nil || len(active) != 1 {
		t.Fatalf("expected 1 task on the local user's issue, got %d (err=%v)", len(active), err)
	}
	if teamIDValue(&active[0]) != runmode.LocalDefaultTeamID {
		t.Errorf("owner = %q, want the one team %q", teamIDValue(&active[0]), runmode.LocalDefaultTeamID)
	}
}

// TestAssigneeCentric_AssigneeOnOneTeam_OwnedByThatTeam: in a two-team org the
// assignee belongs to team A only, so an assignment is owned by A and never
// surfaces to team B. A system rule on B does not pull B in.
func TestAssigneeCentric_AssigneeOnOneTeam_OwnedByThatTeam(t *testing.T) {
	database := newTestDB(t)
	seedHandlerFKTargets(t, database)
	setJiraHost(t, database)

	teamA := seedTeam(t, database, "team-a")
	teamB := seedTeam(t, database, "team-b")
	seedJiraUserOnTeam(t, database, teamA, "acct-aidan", "aidan")
	seedSystemJiraRule(t, database, teamA, domain.EventJiraIssueAssigned)
	seedSystemJiraRule(t, database, teamB, domain.EventJiraIssueAssigned)

	entityID := jiraEntity(t, database, "SKY-aonly")
	emitJiraAssigned(reviewRouter(database), entityID, "acct-aidan")

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

// TestAssigneeCentric_AssigneeOnTwoTeams_NullOwnerVisibleBoth: one Atlassian
// account bound to two TF users on two teams → the tier-3 union is > 1, so the
// router can't pick a single owner: team_id is NULL and the task is visible to
// both teams.
func TestAssigneeCentric_AssigneeOnTwoTeams_NullOwnerVisibleBoth(t *testing.T) {
	database := newTestDB(t)
	seedHandlerFKTargets(t, database)
	setJiraHost(t, database)

	teamA := seedTeam(t, database, "team-a")
	teamB := seedTeam(t, database, "team-b")
	// One account bound to two TF users, one per team — the tier-3 union > 1.
	seedJiraUserOnTeam(t, database, teamA, "acct-shared", "aidan-a")
	seedJiraUserOnTeam(t, database, teamB, "acct-shared", "aidan-b")
	seedSystemJiraRule(t, database, teamA, domain.EventJiraIssueAssigned)
	seedSystemJiraRule(t, database, teamB, domain.EventJiraIssueAssigned)

	entityID := jiraEntity(t, database, "SKY-both")
	emitJiraAssigned(reviewRouter(database), entityID, "acct-shared")

	active, err := testTaskStore(database).FindActiveByEntity(context.Background(), runmode.LocalDefaultOrgID, entityID)
	if err != nil || len(active) != 1 {
		t.Fatalf("expected 1 task, got %d (err=%v)", len(active), err)
	}
	if active[0].TeamID != nil {
		t.Errorf("owner = %v, want nil (unresolved owner: assignee on two teams)", *active[0].TeamID)
	}
	vis := visTeamsOf(t, database, active[0].ID)
	if len(vis) != 2 || vis[0] != minStr(teamA, teamB) || vis[1] != maxStr(teamA, teamB) {
		t.Errorf("visibility = %v, want both teams (%s, %s)", vis, teamA, teamB)
	}
}

// TestAssigneeCentric_NonAssignedTypes_RouteByAssignee: became_atomic,
// status_changed, priority_changed, and commented all route by the assignee
// identically to assigned — each owned by the assignee's one team. (Covers
// every non-assigned member of assigneeCentricJiraEventTypes, so a typo
// dropping a type from the set fails here.)
func TestAssigneeCentric_NonAssignedTypes_RouteByAssignee(t *testing.T) {
	for _, tc := range []struct {
		name      string
		eventType string
		dedup     string
		metaJSON  func(accountID string) string
	}{
		{
			name:      "became_atomic",
			eventType: domain.EventJiraIssueBecameAtomic,
			metaJSON: func(a string) string {
				b, _ := json.Marshal(events.JiraIssueBecameAtomicMetadata{Assignee: "d", AssigneeAccountID: a, IssueKey: "SKY-1", Project: "SKY"})
				return string(b)
			},
		},
		{
			name:      "status_changed",
			eventType: domain.EventJiraIssueStatusChanged,
			dedup:     "In Progress",
			metaJSON: func(a string) string {
				b, _ := json.Marshal(events.JiraIssueStatusChangedMetadata{Assignee: "d", AssigneeAccountID: a, IssueKey: "SKY-1", Project: "SKY", NewStatus: "In Progress"})
				return string(b)
			},
		},
		{
			name:      "priority_changed",
			eventType: domain.EventJiraIssuePriorityChanged,
			dedup:     "High",
			metaJSON: func(a string) string {
				b, _ := json.Marshal(events.JiraIssuePriorityChangedMetadata{Assignee: "d", AssigneeAccountID: a, IssueKey: "SKY-1", Project: "SKY", NewPriority: "High"})
				return string(b)
			},
		},
		{
			name:      "commented",
			eventType: domain.EventJiraIssueCommented,
			metaJSON: func(a string) string {
				b, _ := json.Marshal(events.JiraIssueCommentedMetadata{Assignee: "d", AssigneeAccountID: a, IssueKey: "SKY-1", Project: "SKY"})
				return string(b)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			database := newTestDB(t)
			seedHandlerFKTargets(t, database)
			setJiraHost(t, database)

			teamA := seedTeam(t, database, "team-a")
			teamB := seedTeam(t, database, "team-b")
			seedJiraUserOnTeam(t, database, teamA, "acct-aidan", "aidan")
			seedSystemJiraRule(t, database, teamA, tc.eventType)
			seedSystemJiraRule(t, database, teamB, tc.eventType)

			entityID := jiraEntity(t, database, "SKY-"+tc.name)
			reviewRouter(database).HandleEvent(context.Background(), domain.Event{
				EventType:    tc.eventType,
				EntityID:     &entityID,
				DedupKey:     tc.dedup,
				MetadataJSON: tc.metaJSON("acct-aidan"),
				OccurredAt:   time.Now(),
				OrgID:        runmode.LocalDefaultOrgID,
			})

			active, err := testTaskStore(database).FindActiveByEntity(context.Background(), runmode.LocalDefaultOrgID, entityID)
			if err != nil || len(active) != 1 {
				t.Fatalf("expected 1 task, got %d (err=%v)", len(active), err)
			}
			if teamIDValue(&active[0]) != teamA {
				t.Errorf("owner = %q, want teamA %q (routed by assignee)", teamIDValue(&active[0]), teamA)
			}
			if vis := visTeamsOf(t, database, active[0].ID); len(vis) != 1 || vis[0] != teamA {
				t.Errorf("visibility = %v, want [%s] (team B never sees it)", vis, teamA)
			}
		})
	}
}

// TestAssigneeCentric_ExternalAssignee_WatchRuleSurfaces: a non-member
// assignee resolves to no team, but an explicit user-authored watch rule on
// team C still surfaces the task (owner NULL, C in the visibility set) — the
// opt-in that brings external work onto a team's board.
func TestAssigneeCentric_ExternalAssignee_WatchRuleSurfaces(t *testing.T) {
	database := newTestDB(t)
	seedHandlerFKTargets(t, database)
	setJiraHost(t, database)

	teamC := seedTeam(t, database, "watch-team")
	seedUserJiraRule(t, database, teamC, domain.EventJiraIssueAssigned) // user-authored watch rule

	entityID := jiraEntity(t, database, "SKY-external")
	emitJiraAssigned(reviewRouter(database), entityID, "acct-outsider") // not a TF user

	active, err := testTaskStore(database).FindActiveByEntity(context.Background(), runmode.LocalDefaultOrgID, entityID)
	if err != nil || len(active) != 1 {
		t.Fatalf("expected 1 task from the watch rule, got %d (err=%v)", len(active), err)
	}
	if active[0].TeamID != nil {
		t.Errorf("owner = %v, want nil (external assignee has no owning team)", *active[0].TeamID)
	}
	if vis := visTeamsOf(t, database, active[0].ID); len(vis) != 1 || vis[0] != teamC {
		t.Errorf("visibility = %v, want [%s] (the watching team)", vis, teamC)
	}
}

// TestAssigneeCentric_CommenterNotARoutingDriver: a comment on a non-member's
// issue where a TF member is the COMMENTER mints no task — the commenter is
// not a routing driver; only the assignee is. (Deliberate, mirrors GitHub
// ignoring reviewer/commenter for author-centric routing.)
func TestAssigneeCentric_CommenterNotARoutingDriver(t *testing.T) {
	database := newTestDB(t)
	seedHandlerFKTargets(t, database)
	setJiraHost(t, database)

	teamA := seedTeam(t, database, "team-a")
	seedJiraUserOnTeam(t, database, teamA, "acct-aidan", "aidan") // a TF member…
	seedSystemJiraRule(t, database, teamA, domain.EventJiraIssueCommented)

	entityID := jiraEntity(t, database, "SKY-comment")
	// …who comments on an issue assigned to a non-member. The assignee drives
	// routing, so the member-commenter does not get a task.
	meta := events.JiraIssueCommentedMetadata{
		Assignee: "Outsider", AssigneeAccountID: "acct-outsider",
		Commenter: "aidan", CommenterAccountID: "acct-aidan",
		IssueKey: "SKY-1", Project: "SKY",
	}
	metaJSON, _ := json.Marshal(meta)
	reviewRouter(database).HandleEvent(context.Background(), domain.Event{
		EventType:    domain.EventJiraIssueCommented,
		EntityID:     &entityID,
		MetadataJSON: string(metaJSON),
		OccurredAt:   time.Now(),
		OrgID:        runmode.LocalDefaultOrgID,
	})

	active, err := testTaskStore(database).FindActiveByEntity(context.Background(), runmode.LocalDefaultOrgID, entityID)
	if err != nil {
		t.Fatalf("list tasks: %v", err)
	}
	if len(active) != 0 {
		t.Fatalf("expected no task (commenter is not a routing driver), got %d", len(active))
	}
}

// TestAssigneeCentric_OverrideOwningTeam: entities.owning_team_id pins the
// owner (tier 1) ahead of the assignee tier.
func TestAssigneeCentric_OverrideOwningTeam(t *testing.T) {
	database := newTestDB(t)
	seedHandlerFKTargets(t, database)
	setJiraHost(t, database)

	teamOverride := seedTeam(t, database, "override-team")
	teamAssignee := seedTeam(t, database, "assignee-team")
	seedJiraUserOnTeam(t, database, teamAssignee, "acct-aidan", "aidan")
	seedSystemJiraRule(t, database, teamOverride, domain.EventJiraIssueAssigned)
	seedSystemJiraRule(t, database, teamAssignee, domain.EventJiraIssueAssigned)

	entityID := jiraEntity(t, database, "SKY-override")
	if _, err := database.Exec(`UPDATE entities SET owning_team_id = ? WHERE id = ?`, teamOverride, entityID); err != nil {
		t.Fatalf("set owning_team_id: %v", err)
	}

	emitJiraAssigned(reviewRouter(database), entityID, "acct-aidan")

	active, err := testTaskStore(database).FindActiveByEntity(context.Background(), runmode.LocalDefaultOrgID, entityID)
	if err != nil || len(active) != 1 {
		t.Fatalf("expected 1 task, got %d (err=%v)", len(active), err)
	}
	if teamIDValue(&active[0]) != teamOverride {
		t.Errorf("owner = %q, want the override team %q (not the assignee's %q)", teamIDValue(&active[0]), teamOverride, teamAssignee)
	}
}

// TestAssigneeCentric_PriorTaskAnchorsOwner (tier 2): once an entity has an
// owned assignee-centric task, a later assignee-centric event whose assignee
// maps elsewhere anchors to the same team.
func TestAssigneeCentric_PriorTaskAnchorsOwner(t *testing.T) {
	database := newTestDB(t)
	seedHandlerFKTargets(t, database)
	setJiraHost(t, database)

	teamA := seedTeam(t, database, "team-a")
	teamB := seedTeam(t, database, "team-b")
	seedJiraUserOnTeam(t, database, teamA, "acct-aidan", "aidan")
	seedJiraUserOnTeam(t, database, teamB, "acct-bob", "bob")
	seedSystemJiraRule(t, database, teamA, domain.EventJiraIssueAssigned)
	seedSystemJiraRule(t, database, teamB, domain.EventJiraIssueAssigned)
	seedSystemJiraRule(t, database, teamA, domain.EventJiraIssueStatusChanged)
	seedSystemJiraRule(t, database, teamB, domain.EventJiraIssueStatusChanged)

	entityID := jiraEntity(t, database, "SKY-anchor")
	router := reviewRouter(database)

	// First: an assignment to aidan (team A) establishes A as the owner.
	emitJiraAssigned(router, entityID, "acct-aidan")

	// Then: a status_changed whose assignee is bob (team B). Tier 2 (prior
	// owned assignee-centric task) anchors it to A, not bob's team B.
	statusMeta, _ := json.Marshal(events.JiraIssueStatusChangedMetadata{
		Assignee: "bob", AssigneeAccountID: "acct-bob", IssueKey: "SKY-anchor", Project: "SKY", NewStatus: "In Progress",
	})
	router.HandleEvent(context.Background(), domain.Event{
		EventType:    domain.EventJiraIssueStatusChanged,
		EntityID:     &entityID,
		DedupKey:     "In Progress",
		MetadataJSON: string(statusMeta),
		OccurredAt:   time.Now(),
		OrgID:        runmode.LocalDefaultOrgID,
	})

	statusTasks, err := testTaskStore(database).FindActiveByEntityAndType(context.Background(), runmode.LocalDefaultOrgID, entityID, domain.EventJiraIssueStatusChanged)
	if err != nil || len(statusTasks) != 1 {
		t.Fatalf("expected 1 status_changed task, got %d (err=%v)", len(statusTasks), err)
	}
	if teamIDValue(&statusTasks[0]) != teamA {
		t.Errorf("status owner = %q, want teamA %q (prior-task anchor, not bob's team)", teamIDValue(&statusTasks[0]), teamA)
	}
}

// TestAssigneeCentric_Available_StillCreatesTeamPoolTask is the regression
// guard for the critical carve-out: jira:issue:available is unassigned, so it
// must NOT route through the assignee ladder (which would always drop it).
// It stays on handler-team routing and still mints the team-pool task.
func TestAssigneeCentric_Available_StillCreatesTeamPoolTask(t *testing.T) {
	database := newTestDB(t)
	seedHandlerFKTargets(t, database)
	setJiraHost(t, database)

	teamA := seedTeam(t, database, "team-a")
	seedUserJiraRule(t, database, teamA, domain.EventJiraIssueAvailable)

	entityID := jiraEntity(t, database, "SKY-pool")
	meta := events.JiraIssueAvailableMetadata{IssueKey: "SKY-pool", Project: "SKY", Status: "To Do"}
	metaJSON, _ := json.Marshal(meta)
	reviewRouter(database).HandleEvent(context.Background(), domain.Event{
		EventType:    domain.EventJiraIssueAvailable,
		EntityID:     &entityID,
		MetadataJSON: string(metaJSON),
		OccurredAt:   time.Now(),
		OrgID:        runmode.LocalDefaultOrgID,
	})

	active, err := testTaskStore(database).FindActiveByEntity(context.Background(), runmode.LocalDefaultOrgID, entityID)
	if err != nil || len(active) != 1 {
		t.Fatalf("expected 1 team-pool task from available, got %d (err=%v)", len(active), err)
	}
	if teamIDValue(&active[0]) != teamA {
		t.Errorf("owner = %q, want teamA %q (handler-team routing, unchanged)", teamIDValue(&active[0]), teamA)
	}
}

// TestAssigneeCentric_ReassignToMember_PriorOwnerCloses_NewOwnerMinted: an
// issue assigned to member A, then reassigned to member B (different team).
// The reassignment close-check retires A's task and the new assigned event
// mints a fresh task owned by B's team.
func TestAssigneeCentric_ReassignToMember_PriorOwnerCloses_NewOwnerMinted(t *testing.T) {
	database := newTestDB(t)
	seedHandlerFKTargets(t, database)
	setJiraHost(t, database)

	teamA := seedTeam(t, database, "team-a")
	teamB := seedTeam(t, database, "team-b")
	seedJiraUserOnTeam(t, database, teamA, "acct-aidan", "aidan")
	seedJiraUserOnTeam(t, database, teamB, "acct-bob", "bob")
	seedSystemJiraRule(t, database, teamA, domain.EventJiraIssueAssigned)
	seedSystemJiraRule(t, database, teamB, domain.EventJiraIssueAssigned)

	entityID := jiraEntity(t, database, "SKY-reassign")
	router := reviewRouter(database)

	emitJiraAssigned(router, entityID, "acct-aidan")
	emitJiraAssigned(router, entityID, "acct-bob")

	active, err := testTaskStore(database).FindActiveByEntityAndType(context.Background(), runmode.LocalDefaultOrgID, entityID, domain.EventJiraIssueAssigned)
	if err != nil || len(active) != 1 {
		t.Fatalf("expected exactly 1 active assigned task after reassignment, got %d (err=%v)", len(active), err)
	}
	if teamIDValue(&active[0]) != teamB {
		t.Errorf("owner = %q, want teamB %q (the new assignee's team)", teamIDValue(&active[0]), teamB)
	}
}

// TestAssigneeCentric_AssignClosesAvailablePoolTask: an unassigned issue with
// an open jira:issue:available pool task owned by the tracking team is then
// assigned to a MEMBER of that same team. The pool task must retire (the issue
// is no longer unclaimed) even though its owner coincides with the new
// assignee's team — leaving exactly the new assigned task, not two cards.
func TestAssigneeCentric_AssignClosesAvailablePoolTask(t *testing.T) {
	database := newTestDB(t)
	seedHandlerFKTargets(t, database)
	setJiraHost(t, database)

	teamA := seedTeam(t, database, "team-a")
	seedJiraUserOnTeam(t, database, teamA, "acct-aidan", "aidan")
	seedUserJiraRule(t, database, teamA, domain.EventJiraIssueAvailable)
	seedSystemJiraRule(t, database, teamA, domain.EventJiraIssueAssigned)

	entityID := jiraEntity(t, database, "SKY-poolclaim")
	router := reviewRouter(database)

	// The pool task lands first (owned by teamA, the tracking team).
	availMeta, _ := json.Marshal(events.JiraIssueAvailableMetadata{IssueKey: "SKY-poolclaim", Project: "SKY", Status: "To Do"})
	router.HandleEvent(context.Background(), domain.Event{
		EventType:    domain.EventJiraIssueAvailable,
		EntityID:     &entityID,
		MetadataJSON: string(availMeta),
		OccurredAt:   time.Now(),
		OrgID:        runmode.LocalDefaultOrgID,
	})
	if avail, _ := testTaskStore(database).FindActiveByEntityAndType(context.Background(), runmode.LocalDefaultOrgID, entityID, domain.EventJiraIssueAvailable); len(avail) != 1 {
		t.Fatalf("setup: expected 1 available pool task, got %d", len(avail))
	}

	// Now assigned to aidan, a member of teamA (the pool task's owner).
	emitJiraAssigned(router, entityID, "acct-aidan")

	avail, err := testTaskStore(database).FindActiveByEntityAndType(context.Background(), runmode.LocalDefaultOrgID, entityID, domain.EventJiraIssueAvailable)
	if err != nil {
		t.Fatalf("list available tasks: %v", err)
	}
	if len(avail) != 0 {
		t.Errorf("available pool task should retire on assignment, still %d active", len(avail))
	}
	all, err := testTaskStore(database).FindActiveByEntity(context.Background(), runmode.LocalDefaultOrgID, entityID)
	if err != nil || len(all) != 1 {
		t.Fatalf("expected exactly 1 active task (the assigned one), got %d (err=%v)", len(all), err)
	}
	if all[0].EventType != domain.EventJiraIssueAssigned || teamIDValue(&all[0]) != teamA {
		t.Errorf("surviving task = (%s, owner %q), want (assigned, %q)", all[0].EventType, teamIDValue(&all[0]), teamA)
	}
}

// TestAssigneeCentric_ReassignSameMember_NoChurn: a re-emit of jira:issue:
// assigned for the SAME member must not close-and-recreate that member's own
// task (which would discard its claim / in-flight run) — the member-aware
// close-check leaves a task still owned by the new assignee open.
func TestAssigneeCentric_ReassignSameMember_NoChurn(t *testing.T) {
	database := newTestDB(t)
	seedHandlerFKTargets(t, database)
	setJiraHost(t, database)

	teamA := seedTeam(t, database, "team-a")
	seedJiraUserOnTeam(t, database, teamA, "acct-aidan", "aidan")
	seedSystemJiraRule(t, database, teamA, domain.EventJiraIssueAssigned)

	entityID := jiraEntity(t, database, "SKY-rechurn")
	router := reviewRouter(database)

	emitJiraAssigned(router, entityID, "acct-aidan")
	first, err := testTaskStore(database).FindActiveByEntityAndType(context.Background(), runmode.LocalDefaultOrgID, entityID, domain.EventJiraIssueAssigned)
	if err != nil || len(first) != 1 {
		t.Fatalf("expected 1 task after first assign, got %d (err=%v)", len(first), err)
	}

	emitJiraAssigned(router, entityID, "acct-aidan") // re-emit, same member

	second, err := testTaskStore(database).FindActiveByEntityAndType(context.Background(), runmode.LocalDefaultOrgID, entityID, domain.EventJiraIssueAssigned)
	if err != nil || len(second) != 1 {
		t.Fatalf("expected still 1 task after re-emit, got %d (err=%v)", len(second), err)
	}
	if second[0].ID != first[0].ID {
		t.Errorf("task was recreated (id %s → %s) on a same-member re-emit; want the original preserved", first[0].ID, second[0].ID)
	}
}
