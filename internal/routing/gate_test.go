package routing

import (
	"context"
	"database/sql"
	"encoding/json"
	"sort"
	"testing"
	"time"

	"github.com/google/uuid"

	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/domain/events"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/pkg/websocket"
)

// newGateDB spins up a migrated SQLite test DB with the event_handler FK
// targets seeded — the shared setup for the team↔repo gate tests.
func newGateDB(t *testing.T) *sql.DB {
	t.Helper()
	database := newTestDB(t)
	seedHandlerFKTargets(t, database)
	return database
}

// seedGateTeam inserts a team and returns its id.
func seedGateTeam(t *testing.T, database *sql.DB, slug string) string {
	t.Helper()
	id := uuid.New().String()
	if _, err := database.Exec(
		`INSERT INTO teams (id, org_id, slug, name) VALUES (?, ?, ?, ?)`,
		id, runmode.LocalDefaultOrgID, slug, slug,
	); err != nil {
		t.Fatalf("seed team %s: %v", slug, err)
	}
	return id
}

// seedMatchAllCIRule inserts an applies_to_unowned ("watch") match-all
// ci_check_failed rule for the team (empty predicate = matches every CI
// failure regardless of repo). The flag is what widens visibility beyond
// ownership (TFAC-517) — so this rule surfaces a CI failure to its team even
// for an external/ambiguous author. Without the team↔repo gate it would reach
// any repo's CI failure — that's exactly the leak the gate closes.
func seedMatchAllCIRule(t *testing.T, database *sql.DB, teamID string) {
	t.Helper()
	ruleID := uuid.New().String()
	if _, err := database.Exec(`
		INSERT INTO event_handlers
			(id, org_id, team_id, creator_user_id, kind, event_type,
			 scope_predicate_json, enabled, source, applies_to_unowned, name, default_priority, sort_order,
			 created_at, updated_at)
		VALUES (?, ?, ?, ?, 'rule', ?, NULL, 1, 'user', 1, ?, 0.7, 100, ?, ?)
	`, ruleID, runmode.LocalDefaultOrgID, teamID, runmode.LocalDefaultUserID,
		domain.EventGitHubPRCICheckFailed, "CI rule "+teamID[:8], time.Now(), time.Now()); err != nil {
		t.Fatalf("seed rule for team %s: %v", teamID, err)
	}
}

func ciEvent(t *testing.T, entityID, repo string) domain.Event {
	t.Helper()
	meta := events.GitHubPRCICheckFailedMetadata{
		Author: "aidan", CheckName: "build", Repo: repo, HeadSHA: "abc123",
	}
	metaJSON, _ := json.Marshal(meta)
	return domain.Event{
		EventType:    domain.EventGitHubPRCICheckFailed,
		EntityID:     &entityID,
		DedupKey:     "build",
		MetadataJSON: string(metaJSON),
		CreatedAt:    time.Now(),
		OrgID:        runmode.LocalDefaultOrgID,
	}
}

func gateRouter(database *sql.DB) *Router {
	st := sqlitestore.New(database)
	// Users wired so author-centric owner resolution (the PR author → team
	// ladder) runs — the gate operates on the visibility set either way, but
	// wiring it lets the CI tests assert a real owner instead of a NULL one.
	return NewRouter(testPromptStore(database), testBlueprintStore(database), testEventHandlerStore(database), nil, nil, st.Users,
		testTaskStore(database), st.Conversations, st.Entities, st.PendingFirings, st.Events,
		st.Orgs, st.Teams, st.TeamGitHubRepos, st.JiraStatusRules, nil, nil, noopScorer{}, websocket.NewHub())
}

// TestGate_DisjointRepos_DropsUntrackingTeam is acceptance #1: two teams
// track disjoint repos; a CI failure on team B's repo creates a task
// whose visibility excludes team A — even though team A has a match-all
// CI rule. Team A's auto-fix would never fire on it because team A never
// enters the matched set.
func TestGate_DisjointRepos_DropsUntrackingTeam(t *testing.T) {
	dbh := newGateDB(t)
	st := sqlitestore.New(dbh)
	ctx := context.Background()

	teamA := runmode.LocalDefaultTeamID
	teamB := seedGateTeam(t, dbh, "team-b")

	if err := st.TeamGitHubRepos.ReplaceForTeam(ctx, runmode.LocalDefaultOrgID, teamA, []domain.TeamGitHubRepo{{Owner: "owner", Repo: "repo-a"}}); err != nil {
		t.Fatalf("teamA track: %v", err)
	}
	if err := st.TeamGitHubRepos.ReplaceForTeam(ctx, runmode.LocalDefaultOrgID, teamB, []domain.TeamGitHubRepo{{Owner: "owner", Repo: "repo-b"}}); err != nil {
		t.Fatalf("teamB track: %v", err)
	}

	// The PR author belongs to teamB (the repo-b owner), so the author-centric
	// ladder resolves the owner to teamB — the same team the gate keeps in the
	// visibility set. teamA is gated out of both.
	setReviewHost(t, dbh)
	seedUserOnTeam(t, dbh, teamB, "aidan")

	seedMatchAllCIRule(t, dbh, teamA)
	seedMatchAllCIRule(t, dbh, teamB)

	entity, _, err := st.Entities.FindOrCreate(ctx, runmode.LocalDefaultOrgID, "github", "owner/repo-b#1", "pr", "B PR", "https://example.com/b")
	if err != nil {
		t.Fatalf("create entity: %v", err)
	}

	gateRouter(dbh).HandleEvent(context.Background(), ciEvent(t, entity.ID, "owner/repo-b"))

	active, err := testTaskStore(dbh).FindActiveByEntity(ctx, runmode.LocalDefaultOrgID, entity.ID)
	if err != nil {
		t.Fatalf("list active: %v", err)
	}
	if len(active) != 1 {
		t.Fatalf("expected 1 task, got %d", len(active))
	}
	vis, err := testTaskStore(dbh).VisibilityTeams(ctx, runmode.LocalDefaultOrgID, active[0].ID)
	if err != nil {
		t.Fatalf("VisibilityTeams: %v", err)
	}
	if len(vis) != 1 || vis[0] != teamB {
		t.Fatalf("visibility = %v, want only teamB %q (teamA gated out)", vis, teamB)
	}
	if teamIDValue(&active[0]) != teamB {
		t.Errorf("owner = %q, want teamB %q", teamIDValue(&active[0]), teamB)
	}
}

// TestGate_SharedRepo_VisibleToBoth is acceptance #2: a repo both teams
// track → the event is visible to both (matched ∩ tracks).
func TestGate_SharedRepo_VisibleToBoth(t *testing.T) {
	dbh := newGateDB(t)
	st := sqlitestore.New(dbh)
	ctx := context.Background()

	teamA := runmode.LocalDefaultTeamID
	teamB := seedGateTeam(t, dbh, "team-b")
	shared := []domain.TeamGitHubRepo{{Owner: "owner", Repo: "shared"}}
	if err := st.TeamGitHubRepos.ReplaceForTeam(ctx, runmode.LocalDefaultOrgID, teamA, shared); err != nil {
		t.Fatalf("teamA track: %v", err)
	}
	if err := st.TeamGitHubRepos.ReplaceForTeam(ctx, runmode.LocalDefaultOrgID, teamB, shared); err != nil {
		t.Fatalf("teamB track: %v", err)
	}
	seedMatchAllCIRule(t, dbh, teamA)
	seedMatchAllCIRule(t, dbh, teamB)

	entity, _, err := st.Entities.FindOrCreate(ctx, runmode.LocalDefaultOrgID, "github", "owner/shared#1", "pr", "Shared PR", "https://example.com/s")
	if err != nil {
		t.Fatalf("create entity: %v", err)
	}
	gateRouter(dbh).HandleEvent(context.Background(), ciEvent(t, entity.ID, "owner/shared"))

	active, err := testTaskStore(dbh).FindActiveByEntity(ctx, runmode.LocalDefaultOrgID, entity.ID)
	if err != nil || len(active) != 1 {
		t.Fatalf("expected 1 task, got %d (err=%v)", len(active), err)
	}
	vis, err := testTaskStore(dbh).VisibilityTeams(ctx, runmode.LocalDefaultOrgID, active[0].ID)
	if err != nil {
		t.Fatalf("VisibilityTeams: %v", err)
	}
	sort.Strings(vis)
	want := []string{teamA, teamB}
	sort.Strings(want)
	if len(vis) != 2 || vis[0] != want[0] || vis[1] != want[1] {
		t.Fatalf("visibility = %v, want both teams %v", vis, want)
	}
}

// TestGate_EscapeHatches unit-tests handlerScopeMatchesEvent's allow
// (no-drop) paths directly, since they can't all be exercised through the
// SQLite store: a NULL-team system handler (acceptance #6) isn't even
// representable under SQLite's NOT NULL team_id, and the multi-backend
// NULL-team integration proof lives in pgtest. These are the three
// escape hatches the gate must honor regardless of tracking state.
func TestGate_EscapeHatches(t *testing.T) {
	dbh := newGateDB(t)
	st := sqlitestore.New(dbh)
	// A gate-active router whose teamRepos store tracks *nothing*, so any
	// allow result must come from an escape hatch, not from tracking.
	r := gateRouter(dbh)

	githubEvt := ciEvent(t, "entity-x", "owner/untracked")

	// (1) NULL/empty-team handler (multi-mode system/org-union row) →
	// allowed regardless of tracking. This is acceptance #6's mechanism.
	if !r.handlerScopeMatchesEvent(context.Background(), githubEvt, domain.EventHandler{TeamID: ""}, map[string]bool{}) {
		t.Error("empty-team (system) handler should skip the gate")
	}

	jiraEvt := domain.Event{
		EventType:    domain.EventJiraIssueAssigned,
		MetadataJSON: `{"project":"SKY"}`,
		OrgID:        runmode.LocalDefaultOrgID,
	}

	// (2) teamRepos + jiraRules unwired (nil) → pre-ticket behavior, never
	// drops, for either source.
	rNil := NewRouter(nil, nil, nil, nil, nil, nil, nil, st.Conversations, st.Entities, st.PendingFirings, st.Events, st.Orgs, st.Teams, nil, nil, nil, nil, noopScorer{}, nil)
	if !rNil.handlerScopeMatchesEvent(context.Background(), githubEvt, domain.EventHandler{TeamID: "some-real-team"}, map[string]bool{}) {
		t.Error("nil teamRepos store should skip the GitHub gate")
	}
	if !rNil.handlerScopeMatchesEvent(context.Background(), jiraEvt, domain.EventHandler{TeamID: "some-real-team"}, map[string]bool{}) {
		t.Error("nil jiraRules store should skip the Jira gate")
	}

	// (3) Unknown source → ungated even with a gate-active router + a real
	// team that tracks nothing.
	otherEvt := domain.Event{EventType: "system:poll:done", MetadataJSON: `{}`, OrgID: runmode.LocalDefaultOrgID}
	if !r.handlerScopeMatchesEvent(context.Background(), otherEvt, domain.EventHandler{TeamID: "some-real-team"}, map[string]bool{}) {
		t.Error("non-github/jira event should skip the scope gate")
	}

	// (4) Malformed/empty project metadata on a gate-active router →
	// fail-open (no drop), same policy as the GitHub branch.
	emptyProjEvt := domain.Event{EventType: domain.EventJiraIssueAssigned, MetadataJSON: `{}`, OrgID: runmode.LocalDefaultOrgID}
	if !r.handlerScopeMatchesEvent(context.Background(), emptyProjEvt, domain.EventHandler{TeamID: "some-real-team"}, map[string]bool{}) {
		t.Error("jira event with no project should fail open (skip the gate)")
	}
}

// TestJiraGate_DisjointProjects_DropsUntrackingTeam is a
// router-gate acceptance test: a jira:issue:assigned event on project "SKY" →
// team A (tracks SKY via jira_project_status_rules) fires; team B (tracks
// a different project) is dropped from the task's visibility.
func TestJiraGate_DisjointProjects_DropsUntrackingTeam(t *testing.T) {
	dbh := newGateDB(t)
	st := sqlitestore.New(dbh)
	ctx := context.Background()

	teamA := runmode.LocalDefaultTeamID
	teamB := seedGateTeam(t, dbh, "team-b")

	skyRule := []domain.JiraProjectStatusRules{{
		ProjectKey: "SKY", PickupMembers: jiraRefs("To Do"),
		InProgressMembers: jiraRefs("In Progress"), InProgressCanonical: jiraRef("In Progress"),
		DoneMembers: jiraRefs("Done"), DoneCanonical: jiraRef("Done"),
	}}
	opsRule := []domain.JiraProjectStatusRules{{
		ProjectKey: "OPS", PickupMembers: jiraRefs("To Do"),
		InProgressMembers: jiraRefs("In Progress"), InProgressCanonical: jiraRef("In Progress"),
		DoneMembers: jiraRefs("Done"), DoneCanonical: jiraRef("Done"),
	}}
	if err := st.JiraStatusRules.ReplaceForTeam(ctx, teamA, skyRule); err != nil {
		t.Fatalf("teamA track SKY: %v", err)
	}
	if err := st.JiraStatusRules.ReplaceForTeam(ctx, teamB, opsRule); err != nil {
		t.Fatalf("teamB track OPS: %v", err)
	}

	seedMatchAllJiraAssignedRule(t, dbh, teamA)
	seedMatchAllJiraAssignedRule(t, dbh, teamB)

	// assigned routes by assignee (the owning-team ladder), so the assignee
	// must resolve to a TF team for the owner assertion below; aidan is on
	// teamA. The gate still bounds WHICH teams' handlers match (teamB tracks a
	// different project), independent of ownership.
	setJiraHost(t, dbh)
	seedJiraUserOnTeam(t, dbh, teamA, "acct-aidan", "aidan")

	entity, _, err := st.Entities.FindOrCreate(ctx, runmode.LocalDefaultOrgID, "jira", "SKY-1", "issue", "An issue", "https://example.com/SKY-1")
	if err != nil {
		t.Fatalf("create entity: %v", err)
	}

	gateRouter(dbh).HandleEvent(context.Background(), jiraAssignedEvent(t, entity.ID, "SKY"))

	active, err := testTaskStore(dbh).FindActiveByEntity(ctx, runmode.LocalDefaultOrgID, entity.ID)
	if err != nil {
		t.Fatalf("list active: %v", err)
	}
	if len(active) != 1 {
		t.Fatalf("expected 1 task, got %d", len(active))
	}
	vis, err := testTaskStore(dbh).VisibilityTeams(ctx, runmode.LocalDefaultOrgID, active[0].ID)
	if err != nil {
		t.Fatalf("VisibilityTeams: %v", err)
	}
	if len(vis) != 1 || vis[0] != teamA {
		t.Fatalf("visibility = %v, want only teamA %q (teamB gated out)", vis, teamA)
	}
	if teamIDValue(&active[0]) != teamA {
		t.Errorf("owner = %q, want teamA %q", teamIDValue(&active[0]), teamA)
	}
}

// seedMatchAllJiraAssignedRule inserts an applies_to_unowned ("watch")
// match-all jira:issue:assigned rule for the team (empty predicate = matches
// every assignment regardless of project). The flag widens visibility beyond
// ownership (TFAC-517). Without the team↔project gate it would reach any
// project's events — the leak the gate closes.
func seedMatchAllJiraAssignedRule(t *testing.T, database *sql.DB, teamID string) {
	t.Helper()
	ruleID := uuid.New().String()
	if _, err := database.Exec(`
		INSERT INTO event_handlers
			(id, org_id, team_id, creator_user_id, kind, event_type,
			 scope_predicate_json, enabled, source, applies_to_unowned, name, default_priority, sort_order,
			 created_at, updated_at)
		VALUES (?, ?, ?, ?, 'rule', ?, NULL, 1, 'user', 1, ?, 0.7, 100, ?, ?)
	`, ruleID, runmode.LocalDefaultOrgID, teamID, runmode.LocalDefaultUserID,
		domain.EventJiraIssueAssigned, "Jira rule "+teamID[:8], time.Now(), time.Now()); err != nil {
		t.Fatalf("seed jira rule for team %s: %v", teamID, err)
	}
}

func jiraAssignedEvent(t *testing.T, entityID, project string) domain.Event {
	t.Helper()
	meta := events.JiraIssueAssignedMetadata{
		Assignee: "aidan", AssigneeAccountID: "acct-aidan", IssueKey: project + "-1", Project: project, Status: "To Do",
	}
	metaJSON, _ := json.Marshal(meta)
	return domain.Event{
		EventType:    domain.EventJiraIssueAssigned,
		EntityID:     &entityID,
		MetadataJSON: string(metaJSON),
		CreatedAt:    time.Now(),
		OrgID:        runmode.LocalDefaultOrgID,
	}
}

// enrollOnTeam adds an existing user to an additional team, modeling one
// person who belongs to multiple teams (the founder auto-joined to every team
// they create). seedUserOnTeam mints a fresh user per call; this reuses one.
func enrollOnTeam(t *testing.T, database *sql.DB, userID, teamID string) {
	t.Helper()
	if _, err := database.Exec(
		`INSERT INTO memberships (user_id, team_id, role) VALUES (?, ?, 'admin')`,
		userID, teamID,
	); err != nil {
		t.Fatalf("enroll %s→%s: %v", userID, teamID, err)
	}
}

// TestGate_MultiTeamAuthor_UntrackedTeamGatedFromOwnerLadder is the regression
// guard for the queued-column team-filter leak. A PR author belongs to two
// teams: A (tracks the repo) and B (unconfigured, tracks nothing). The event is
// on A's repo. The task must be owned by and visible to A ONLY.
//
// Before the owner ladder's tier-3 identity resolution was tracking-gated, the
// author's membership alone pulled B into the set: ownership resolved ambiguous
// (NULL owner) and task_teams became {A, B}, so filtering the board to B still
// surfaced A's queued work — the card was visible to a team that could never
// claim or act on the repo. B's own match-all watch rule is already gated out at
// match time (B tracks nothing), so the author ladder is the only path B could
// have entered — which is exactly what this asserts is now closed.
func TestGate_MultiTeamAuthor_UntrackedTeamGatedFromOwnerLadder(t *testing.T) {
	dbh := newGateDB(t)
	st := sqlitestore.New(dbh)
	ctx := context.Background()

	teamA := runmode.LocalDefaultTeamID
	teamB := seedGateTeam(t, dbh, "team-b")

	// A tracks repo-a; B (unconfigured) tracks nothing.
	if err := st.TeamGitHubRepos.ReplaceForTeam(ctx, runmode.LocalDefaultOrgID, teamA, []domain.TeamGitHubRepo{{Owner: "owner", Repo: "repo-a"}}); err != nil {
		t.Fatalf("teamA track: %v", err)
	}

	// The author is on BOTH teams — the founder-on-every-team shape.
	setReviewHost(t, dbh)
	uid := seedUserOnTeam(t, dbh, teamA, "aidan")
	enrollOnTeam(t, dbh, uid, teamB)

	seedMatchAllCIRule(t, dbh, teamA)
	seedMatchAllCIRule(t, dbh, teamB)

	entity, _, err := st.Entities.FindOrCreate(ctx, runmode.LocalDefaultOrgID, "github", "owner/repo-a#1", "pr", "A PR", "https://example.com/a")
	if err != nil {
		t.Fatalf("create entity: %v", err)
	}
	gateRouter(dbh).HandleEvent(context.Background(), ciEvent(t, entity.ID, "owner/repo-a"))

	active, err := testTaskStore(dbh).FindActiveByEntity(ctx, runmode.LocalDefaultOrgID, entity.ID)
	if err != nil || len(active) != 1 {
		t.Fatalf("expected 1 task, got %d (err=%v)", len(active), err)
	}
	vis, err := testTaskStore(dbh).VisibilityTeams(ctx, runmode.LocalDefaultOrgID, active[0].ID)
	if err != nil {
		t.Fatalf("VisibilityTeams: %v", err)
	}
	if len(vis) != 1 || vis[0] != teamA {
		t.Fatalf("visibility = %v, want only teamA %q (teamB must be gated out of the owner ladder)", vis, teamA)
	}
	if teamIDValue(&active[0]) != teamA {
		t.Errorf("owner = %q, want teamA %q (a NULL owner means teamB leaked in as an ambiguous co-owner)", teamIDValue(&active[0]), teamA)
	}
}

// TestGate_MultiTeamAuthor_BothTrack_StaysAmbiguous guards the opposite edge:
// when BOTH of the author's teams track the repo, the task is genuinely shared —
// visible to both, NULL owner (the first member to claim consolidates). The
// tracking gate must narrow ownership to reality without collapsing a
// legitimately ambiguous owner.
func TestGate_MultiTeamAuthor_BothTrack_StaysAmbiguous(t *testing.T) {
	dbh := newGateDB(t)
	st := sqlitestore.New(dbh)
	ctx := context.Background()

	teamA := runmode.LocalDefaultTeamID
	teamB := seedGateTeam(t, dbh, "team-b")
	shared := []domain.TeamGitHubRepo{{Owner: "owner", Repo: "shared"}}
	if err := st.TeamGitHubRepos.ReplaceForTeam(ctx, runmode.LocalDefaultOrgID, teamA, shared); err != nil {
		t.Fatalf("teamA track: %v", err)
	}
	if err := st.TeamGitHubRepos.ReplaceForTeam(ctx, runmode.LocalDefaultOrgID, teamB, shared); err != nil {
		t.Fatalf("teamB track: %v", err)
	}

	setReviewHost(t, dbh)
	uid := seedUserOnTeam(t, dbh, teamA, "aidan")
	enrollOnTeam(t, dbh, uid, teamB)

	seedMatchAllCIRule(t, dbh, teamA)
	seedMatchAllCIRule(t, dbh, teamB)

	entity, _, err := st.Entities.FindOrCreate(ctx, runmode.LocalDefaultOrgID, "github", "owner/shared#1", "pr", "Shared PR", "https://example.com/s")
	if err != nil {
		t.Fatalf("create entity: %v", err)
	}
	gateRouter(dbh).HandleEvent(context.Background(), ciEvent(t, entity.ID, "owner/shared"))

	active, err := testTaskStore(dbh).FindActiveByEntity(ctx, runmode.LocalDefaultOrgID, entity.ID)
	if err != nil || len(active) != 1 {
		t.Fatalf("expected 1 task, got %d (err=%v)", len(active), err)
	}
	vis := visTeamsOf(t, dbh, active[0].ID)
	want := []string{teamA, teamB}
	sort.Strings(want)
	if len(vis) != 2 || vis[0] != want[0] || vis[1] != want[1] {
		t.Fatalf("visibility = %v, want both teams %v (a legitimately ambiguous owner must be preserved)", vis, want)
	}
	if teamIDValue(&active[0]) != "" {
		t.Errorf("owner = %q, want NULL (both teams track the repo → ambiguous membership, consolidated on first claim)", teamIDValue(&active[0]))
	}
}

// TestGate_LocalN1_NoOp is acceptance #7: in local N=1 the single default
// team tracks every configured repo (because repositories is its union),
// so the gate never drops anything — identical behavior to pre-ticket.
func TestGate_LocalN1_NoOp(t *testing.T) {
	dbh := newGateDB(t)
	st := sqlitestore.New(dbh)
	ctx := context.Background()

	teamA := runmode.LocalDefaultTeamID
	if err := st.TeamGitHubRepos.ReplaceForTeam(ctx, runmode.LocalDefaultOrgID, teamA, []domain.TeamGitHubRepo{{Owner: "owner", Repo: "repo"}}); err != nil {
		t.Fatalf("teamA track: %v", err)
	}
	// N=1: the local author is on the one team, so the author-centric ladder
	// resolves the owner to it.
	setReviewHost(t, dbh)
	seedUserOnTeam(t, dbh, teamA, "aidan")
	seedMatchAllCIRule(t, dbh, teamA)

	entity, _, err := st.Entities.FindOrCreate(ctx, runmode.LocalDefaultOrgID, "github", "owner/repo#1", "pr", "PR", "https://example.com/1")
	if err != nil {
		t.Fatalf("create entity: %v", err)
	}
	gateRouter(dbh).HandleEvent(context.Background(), ciEvent(t, entity.ID, "owner/repo"))

	active, err := testTaskStore(dbh).FindActiveByEntity(ctx, runmode.LocalDefaultOrgID, entity.ID)
	if err != nil || len(active) != 1 {
		t.Fatalf("expected 1 task, got %d (err=%v)", len(active), err)
	}
	if teamIDValue(&active[0]) != teamA {
		t.Errorf("owner = %q, want teamA %q", teamIDValue(&active[0]), teamA)
	}
}
