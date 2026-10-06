package routing

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	dbpkg "github.com/sky-ai-eng/triage-factory/internal/db"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/domain/events"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/pkg/websocket"
)

const linearTestWorkspace = "ws-acme"

// linearRouter is gateRouter with the Linear team-rules store wired.
func linearRouter(database *sql.DB) *Router {
	st := sqlitestore.New(database)
	return NewRouter(testPromptStore(database), testBlueprintStore(database), testEventHandlerStore(database), nil, nil, st.Users,
		testTaskStore(database), st.Conversations, st.Entities, st.PendingFirings, st.Events,
		st.Orgs, st.Teams, st.TeamGitHubRepos, st.JiraStatusRules, st.LinearTeamRules, nil, nil, noopScorer{}, websocket.NewHub())
}

func linearStateRef(name string) domain.LinearStateRef {
	return domain.LinearStateRef{ID: "st-" + name, Name: name}
}

// armLinearTeam arms linearTeamID for teamID with done as its done state.
func armLinearTeam(t *testing.T, database *sql.DB, teamID, linearTeamID, done string) {
	t.Helper()
	todo, prog, fin := linearStateRef("Todo"), linearStateRef("In Progress"), linearStateRef(done)
	if _, err := sqlitestore.New(database).LinearTeamRules.ReplaceForTeam(t.Context(), teamID, []domain.LinearTeamRules{{
		LinearTeamID: linearTeamID, LinearTeamKey: "K" + linearTeamID,
		PickupMembers:     []domain.LinearStateRef{todo},
		InProgressMembers: []domain.LinearStateRef{prog}, InProgressCanonical: prog,
		DoneMembers: []domain.LinearStateRef{fin}, DoneCanonical: fin,
	}}); err != nil {
		t.Fatalf("arm linear team %s for %s: %v", linearTeamID, teamID, err)
	}
}

func linearEvent(eventType, entityID, linearTeamID, assigneeUserID string) domain.Event {
	meta, _ := json.Marshal(events.LinearIssueAssignedMetadata{
		LinearIssueIdentity: events.LinearIssueIdentity{
			IssueIdentifier: "ENG-1", IssueID: "uuid-1",
			LinearTeamID: linearTeamID, LinearTeamKey: "ENG",
			AssigneeUserID: assigneeUserID,
		},
		Status: "Todo",
	})
	return domain.Event{
		EventType:    eventType,
		EntityID:     &entityID,
		MetadataJSON: string(meta),
		CreatedAt:    time.Now(),
		OrgID:        runmode.LocalDefaultOrgID,
	}
}

// erroringLinearRules fails the gate's lookup.
type erroringLinearRules struct{ dbpkg.LinearTeamRulesStore }

func (erroringLinearRules) TracksTeamSystem(context.Context, string, string) (bool, error) {
	return false, errors.New("boom: linear_team_rules read failed")
}

// TestLinearGate covers the team↔Linear-team gate: a team that tracks the
// event's Linear team passes, one that does not is dropped, and every way the
// gate cannot answer — no store, no linear_team_id, a failed read — allows,
// the gate's documented fail-open posture.
func TestLinearGate(t *testing.T) {
	database := newGateDB(t)
	teamA := runmode.LocalDefaultTeamID
	teamB := seedGateTeam(t, database, "team-b")
	armLinearTeam(t, database, teamA, "lt-eng", "Done")
	armLinearTeam(t, database, teamB, "lt-ops", "Done")
	r := linearRouter(database)
	ctx := context.Background()
	evt := linearEvent(domain.EventLinearIssueAssigned, "ent-1", "lt-eng", "")

	if !r.handlerScopeMatchesEvent(ctx, evt, domain.EventHandler{TeamID: teamA}, map[string]bool{}) {
		t.Error("a team that tracks the Linear team was dropped")
	}
	if r.handlerScopeMatchesEvent(ctx, evt, domain.EventHandler{TeamID: teamB}, map[string]bool{}) {
		t.Error("a team that does not track the Linear team passed the gate")
	}

	noTeam := evt
	noTeam.MetadataJSON = `{"issue_identifier":"ENG-1"}`
	if !r.handlerScopeMatchesEvent(ctx, noTeam, domain.EventHandler{TeamID: teamB}, map[string]bool{}) {
		t.Error("an event with no linear_team_id should fail open")
	}
	malformed := evt
	malformed.MetadataJSON = `not json`
	if !r.handlerScopeMatchesEvent(ctx, malformed, domain.EventHandler{TeamID: teamB}, map[string]bool{}) {
		t.Error("malformed metadata should fail open")
	}

	unwired := linearRouter(database)
	unwired.linearRules = nil
	if !unwired.handlerScopeMatchesEvent(ctx, evt, domain.EventHandler{TeamID: teamB}, map[string]bool{}) {
		t.Error("a nil linearRules store should skip the gate")
	}
	failing := linearRouter(database)
	failing.linearRules = erroringLinearRules{}
	if !failing.handlerScopeMatchesEvent(ctx, evt, domain.EventHandler{TeamID: teamB}, map[string]bool{}) {
		t.Error("a failed lookup should fail open")
	}
}

// seedLinearUserOnTeam creates a user on teamID bound to linearUserID in the
// test workspace.
func seedLinearUserOnTeam(t *testing.T, database *sql.DB, teamID, linearUserID string) string {
	t.Helper()
	uid := uuid.New().String()
	if _, err := database.Exec(`INSERT INTO users (id, display_name) VALUES (?, ?)`, uid, linearUserID); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if _, err := database.Exec(`INSERT INTO memberships (user_id, team_id, role) VALUES (?, ?, 'admin')`, uid, teamID); err != nil {
		t.Fatalf("seed membership: %v", err)
	}
	if err := sqlitestore.New(database).Users.UpsertLinearIdentity(context.Background(), uid, linearTestWorkspace, linearUserID, linearUserID, "api_key"); err != nil {
		t.Fatalf("bind linear identity: %v", err)
	}
	return uid
}

func setLinearWorkspace(t *testing.T, database *sql.DB) {
	t.Helper()
	if _, err := database.Exec(`UPDATE org_settings SET linear_workspace_id = ? WHERE org_id = ?`, linearTestWorkspace, runmode.LocalDefaultOrgID); err != nil {
		t.Fatalf("set linear workspace: %v", err)
	}
}

func seedMatchAllLinearRule(t *testing.T, database *sql.DB, teamID, eventType string) {
	t.Helper()
	if _, err := database.Exec(`
		INSERT INTO event_handlers
			(id, org_id, team_id, creator_user_id, kind, event_type,
			 scope_predicate_json, enabled, source, applies_to_unowned, name, default_priority, sort_order,
			 created_at, updated_at)
		VALUES (?, ?, ?, ?, 'rule', ?, NULL, 1, 'user', 1, ?, 0.7, 100, ?, ?)
	`, uuid.New().String(), runmode.LocalDefaultOrgID, teamID, runmode.LocalDefaultUserID,
		eventType, "Linear rule "+teamID[:8]+eventType, time.Now(), time.Now()); err != nil {
		t.Fatalf("seed linear rule for team %s: %v", teamID, err)
	}
}

// TestLinearAssigned_RoutesToAssigneesTrackingTeam: a linear:issue:assigned
// event routes through the assignee ladder. The assignee is bound in the org's
// workspace to a member of team A, which tracks the issue's Linear team, so A
// owns the task; team B tracks another Linear team and is gated out even
// though its match-all rule matched.
func TestLinearAssigned_RoutesToAssigneesTrackingTeam(t *testing.T) {
	database := newGateDB(t)
	st := sqlitestore.New(database)
	ctx := context.Background()
	teamA := runmode.LocalDefaultTeamID
	teamB := seedGateTeam(t, database, "team-b")
	armLinearTeam(t, database, teamA, "lt-eng", "Done")
	armLinearTeam(t, database, teamB, "lt-ops", "Done")
	seedMatchAllLinearRule(t, database, teamA, domain.EventLinearIssueAssigned)
	seedMatchAllLinearRule(t, database, teamB, domain.EventLinearIssueAssigned)
	setLinearWorkspace(t, database)
	seedLinearUserOnTeam(t, database, teamA, "lu-alice")

	entity, _, err := st.Entities.FindOrCreate(ctx, runmode.LocalDefaultOrgID, "linear", "ENG-1", "issue", "An issue", "")
	if err != nil {
		t.Fatal(err)
	}
	linearRouter(database).HandleEvent(ctx, linearEvent(domain.EventLinearIssueAssigned, entity.ID, "lt-eng", "lu-alice"))

	active, err := testTaskStore(database).FindActiveByEntity(ctx, runmode.LocalDefaultOrgID, entity.ID)
	if err != nil || len(active) != 1 {
		t.Fatalf("active tasks = %v err=%v, want one", active, err)
	}
	if owner := teamIDValue(&active[0]); owner != teamA {
		t.Errorf("owner = %q, want team A %q (the assignee's tracking team)", owner, teamA)
	}
	vis, err := testTaskStore(database).VisibilityTeams(ctx, runmode.LocalDefaultOrgID, active[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(vis) != 1 || vis[0] != teamA {
		t.Errorf("visibility = %v, want team A alone", vis)
	}
}

// reassignFixture is two teams that both track Linear team lt-eng, each with a
// member bound in the org's workspace and a system rule on linear:issue:
// assigned.
func reassignFixture(t *testing.T) (database *sql.DB, router *Router, teamA, teamB, entityID string) {
	t.Helper()
	database = newGateDB(t)
	teamA = seedTeam(t, database, "team-a")
	teamB = seedTeam(t, database, "team-b")
	armLinearTeam(t, database, teamA, "lt-eng", "Done")
	armLinearTeam(t, database, teamB, "lt-eng", "Done")
	setLinearWorkspace(t, database)
	seedLinearUserOnTeam(t, database, teamA, "lu-alice")
	seedLinearUserOnTeam(t, database, teamB, "lu-bob")
	seedSystemJiraRule(t, database, teamA, domain.EventLinearIssueAssigned)
	seedSystemJiraRule(t, database, teamB, domain.EventLinearIssueAssigned)
	e, _, err := sqlitestore.New(database).Entities.FindOrCreate(context.Background(), runmode.LocalDefaultOrgID, "linear", "ENG-9", "issue", "An issue", "")
	if err != nil {
		t.Fatal(err)
	}
	return database, linearRouter(database), teamA, teamB, e.ID
}

func activeOfType(t *testing.T, database *sql.DB, entityID, eventType string) []domain.Task {
	t.Helper()
	tasks, err := testTaskStore(database).FindActiveByEntityAndType(context.Background(), runmode.LocalDefaultOrgID, entityID, eventType)
	if err != nil {
		t.Fatalf("list active %s tasks: %v", eventType, err)
	}
	return tasks
}

// TestLinearReassign_PriorOwnerClosesNewOwnerMinted: an issue assigned to a
// member of team A and then to a member of team B retires A's task, and the
// new assignment mints one owned by B. Without the close, the ladder's prior-
// task tier would anchor the second event on A's still-active task and bump it.
func TestLinearReassign_PriorOwnerClosesNewOwnerMinted(t *testing.T) {
	database, router, teamA, teamB, entityID := reassignFixture(t)
	ctx := context.Background()

	router.HandleEvent(ctx, linearEvent(domain.EventLinearIssueAssigned, entityID, "lt-eng", "lu-alice"))
	first := activeOfType(t, database, entityID, domain.EventLinearIssueAssigned)
	if len(first) != 1 || teamIDValue(&first[0]) != teamA {
		t.Fatalf("after the first assignment: %d tasks, want one owned by team A", len(first))
	}

	router.HandleEvent(ctx, linearEvent(domain.EventLinearIssueAssigned, entityID, "lt-eng", "lu-bob"))
	active := activeOfType(t, database, entityID, domain.EventLinearIssueAssigned)
	if len(active) != 1 {
		t.Fatalf("active assigned tasks after reassignment = %d, want 1", len(active))
	}
	if teamIDValue(&active[0]) != teamB {
		t.Errorf("owner = %q, want team B %q (the new assignee's team)", teamIDValue(&active[0]), teamB)
	}
	if status, reason := taskCloseReason(t, database, first[0].ID); status != "done" || reason != "auto_closed_by_event" {
		t.Errorf("team A's task = (%s, %s), want closed by the reassignment", status, reason)
	}
}

// TestLinearReassign_SameMemberNoChurn: a re-emitted assignment to the same
// member leaves that member's task in place rather than closing and minting
// it again, which would discard its claim and any run on it.
func TestLinearReassign_SameMemberNoChurn(t *testing.T) {
	database, router, _, _, entityID := reassignFixture(t)
	ctx := context.Background()

	router.HandleEvent(ctx, linearEvent(domain.EventLinearIssueAssigned, entityID, "lt-eng", "lu-alice"))
	first := activeOfType(t, database, entityID, domain.EventLinearIssueAssigned)
	router.HandleEvent(ctx, linearEvent(domain.EventLinearIssueAssigned, entityID, "lt-eng", "lu-alice"))
	second := activeOfType(t, database, entityID, domain.EventLinearIssueAssigned)
	if len(first) != 1 || len(second) != 1 || second[0].ID != first[0].ID {
		t.Errorf("tasks before %v, after %v: want the one task preserved", first, second)
	}
}

// TestLinearReassign_AssignClosesAvailablePoolTask: once the issue is
// assigned, its unassigned-pool task retires even when its owner is the new
// assignee's team, leaving the assigned task alone.
func TestLinearReassign_AssignClosesAvailablePoolTask(t *testing.T) {
	database, router, teamA, _, entityID := reassignFixture(t)
	ctx := context.Background()
	seedUserJiraRule(t, database, teamA, domain.EventLinearIssueAvailable)

	router.HandleEvent(ctx, linearEvent(domain.EventLinearIssueAvailable, entityID, "lt-eng", ""))
	if avail := activeOfType(t, database, entityID, domain.EventLinearIssueAvailable); len(avail) != 1 {
		t.Fatalf("setup: available tasks = %d, want 1", len(avail))
	}

	router.HandleEvent(ctx, linearEvent(domain.EventLinearIssueAssigned, entityID, "lt-eng", "lu-alice"))
	if avail := activeOfType(t, database, entityID, domain.EventLinearIssueAvailable); len(avail) != 0 {
		t.Errorf("available pool task should retire on assignment, still %d active", len(avail))
	}
	all, err := testTaskStore(database).FindActiveByEntity(ctx, runmode.LocalDefaultOrgID, entityID)
	if err != nil || len(all) != 1 || all[0].EventType != domain.EventLinearIssueAssigned || teamIDValue(&all[0]) != teamA {
		t.Errorf("active tasks = %v err=%v, want only the assigned task owned by team A", all, err)
	}
}

// TestLinearAssigneeTeams_WorkspaceScoped: the assignee's Linear user id is
// looked up in the org's own workspace and nowhere else; with no workspace
// bound, nobody resolves.
func TestLinearAssigneeTeams_WorkspaceScoped(t *testing.T) {
	database := newGateDB(t)
	teamA := runmode.LocalDefaultTeamID
	seedLinearUserOnTeam(t, database, teamA, "lu-alice")
	r := linearRouter(database)
	evt := linearEvent(domain.EventLinearIssueAssigned, "ent-1", "lt-eng", "lu-alice")

	teams, err := r.assigneeTeams(t.Context(), runmode.LocalDefaultOrgID, evt)
	if err != nil || len(teams) != 0 {
		t.Fatalf("teams with no workspace bound = %v err=%v, want none", teams, err)
	}
	setLinearWorkspace(t, database)
	teams, err = r.assigneeTeams(t.Context(), runmode.LocalDefaultOrgID, evt)
	if err != nil || len(teams) != 1 || teams[0] != teamA {
		t.Fatalf("teams = %v err=%v, want [%s]", teams, err, teamA)
	}
	other := linearEvent(domain.EventLinearIssueAssigned, "ent-1", "lt-eng", "lu-stranger")
	if teams, err := r.assigneeTeams(t.Context(), runmode.LocalDefaultOrgID, other); err != nil || len(teams) != 0 {
		t.Errorf("teams for an unbound Linear user = %v err=%v, want none", teams, err)
	}
}

// TestLinearUnreachable_ClosesEntityAndTasks: unreachable terminates the
// entity and closes its tasks, as Jira's does.
func TestLinearUnreachable_ClosesEntityAndTasks(t *testing.T) {
	database := newGateDB(t)
	ctx := context.Background()
	entityID, taskIDs := seedDivergentEntity(t, database, "linear", "ENG-5",
		`{"id":"uuid-5","identifier":"ENG-5","team_id":"lt-eng","state":{"id":"st-Todo","name":"Todo"}}`,
		domain.EventLinearIssueAssigned)

	linearRouter(database).HandleEvent(ctx, linearEvent(domain.EventLinearIssueUnreachable, entityID, "lt-eng", ""))

	e, err := sqlitestore.New(database).Entities.Get(ctx, runmode.LocalDefaultOrgID, entityID)
	if err != nil || e == nil || e.State != "closed" {
		t.Fatalf("entity = %+v err=%v, want closed", e, err)
	}
	if status, reason := taskCloseReason(t, database, taskIDs[0]); status != "done" || reason != "entity_closed" {
		t.Errorf("task = (%s, %s), want (done, entity_closed)", status, reason)
	}
}

// TestTerminalChecker_LinearDoneIsPerTeam: a Linear entity counts as stranded
// only when its state is done in its own Linear team; the flat union the store
// narrows on would otherwise count one done only in another team.
func TestTerminalChecker_LinearDoneIsPerTeam(t *testing.T) {
	database := newTestDB(t)
	r := linearRouter(database)
	armLinearTeam(t, database, runmode.LocalDefaultTeamID, "lt-eng", "Shipped")
	teamB := seedGateTeam(t, database, "team-b")
	armLinearTeam(t, database, teamB, "lt-ops", "Done")

	shipped := `{"identifier":"%s","team_id":"%s","state":{"id":"st-Shipped","name":"Shipped"}}`
	seedDivergentEntity(t, database, "linear", "ENG-7", fmt.Sprintf(shipped, "ENG-7", "lt-eng"), domain.EventLinearIssueAssigned)
	seedDivergentEntity(t, database, "linear", "OPS-7", fmt.Sprintf(shipped, "OPS-7", "lt-ops"), domain.EventLinearIssueAssigned)

	if a, _, _ := r.checkOrgTerminalInvariant(context.Background(), runmode.LocalDefaultOrgID); a != 1 {
		t.Errorf("count = %d, want 1 — only the ENG issue is in its own team's done state", a)
	}
}
