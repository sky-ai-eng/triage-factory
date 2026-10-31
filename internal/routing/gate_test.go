package routing

import (
	"context"
	"database/sql"
	"encoding/json"
	"sort"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db/dbseed"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/domain/events"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
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

// gateRouter is a router with the GitHub and Jira tracking gates wired.
func gateRouter(database *sql.DB) *Router {
	return newTestRouter(gateRouterDeps(database))
}

// gateRouterDeps is gateRouter's RouterDeps, for a test that varies more.
func gateRouterDeps(database *sql.DB) RouterDeps {
	st := sqlitestore.New(database)
	deps := testRouterDeps(database)
	deps.TeamRepos, deps.JiraRules = st.TeamGitHubRepos, st.JiraStatusRules
	return deps
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

	if err := dbtest.TrackRepos(ctx, st.Repos, st.TeamGitHubRepos, runmode.LocalDefaultOrgID, teamA, dbtest.TestGitHubHost, []domain.TeamGitHubRepo{{Owner: "owner", Repo: "repo-a"}}); err != nil {
		t.Fatalf("teamA track: %v", err)
	}
	if err := dbtest.TrackRepos(ctx, st.Repos, st.TeamGitHubRepos, runmode.LocalDefaultOrgID, teamB, dbtest.TestGitHubHost, []domain.TeamGitHubRepo{{Owner: "owner", Repo: "repo-b"}}); err != nil {
		t.Fatalf("teamB track: %v", err)
	}

	// The PR author belongs to teamB (the repo-b owner), so the author-centric
	// ladder resolves the owner to teamB — the same team the gate keeps in the
	// visibility set. teamA is gated out of both.
	setReviewHost(t, dbh)
	seedUserOnTeam(t, dbh, teamB, "aidan")

	seedMatchAllCIRule(t, dbh, teamA)
	seedMatchAllCIRule(t, dbh, teamB)

	entity, _, err := st.Entities.FindOrCreate(ctx, runmode.LocalDefaultOrgID, "github", "https://github.com", "owner/repo-b#1", "", "pr", "B PR", "https://example.com/b")
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
	if err := dbtest.TrackRepos(ctx, st.Repos, st.TeamGitHubRepos, runmode.LocalDefaultOrgID, teamA, dbtest.TestGitHubHost, shared); err != nil {
		t.Fatalf("teamA track: %v", err)
	}
	if err := dbtest.TrackRepos(ctx, st.Repos, st.TeamGitHubRepos, runmode.LocalDefaultOrgID, teamB, dbtest.TestGitHubHost, shared); err != nil {
		t.Fatalf("teamB track: %v", err)
	}
	seedMatchAllCIRule(t, dbh, teamA)
	seedMatchAllCIRule(t, dbh, teamB)

	entity, _, err := st.Entities.FindOrCreate(ctx, runmode.LocalDefaultOrgID, "github", "https://github.com", "owner/shared#1", "", "pr", "Shared PR", "https://example.com/s")
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

// TestGate_MatchesTheEntitysRepositoryByID pins that the team↔repo gate reads
// the event entity's repository_id and nothing named. Team A tracks owner/old,
// which is then renamed with no key rewritten, so its pull request's key and
// the event's metadata both still spell the old name; team A still passes.
// Another entity's key spells the renamed repository but references the one
// team B tracks, and only team B passes for it.
func TestGate_MatchesTheEntitysRepositoryByID(t *testing.T) {
	dbh := newGateDB(t)
	st := sqlitestore.New(dbh)
	ctx := context.Background()

	teamA := runmode.LocalDefaultTeamID
	teamB := seedGateTeam(t, dbh, "team-b")
	if err := dbtest.TrackRepos(ctx, st.Repos, st.TeamGitHubRepos, runmode.LocalDefaultOrgID, teamA, dbtest.TestGitHubHost, []domain.TeamGitHubRepo{{Owner: "owner", Repo: "old"}}); err != nil {
		t.Fatalf("teamA track: %v", err)
	}
	if err := dbtest.TrackRepos(ctx, st.Repos, st.TeamGitHubRepos, runmode.LocalDefaultOrgID, teamB, dbtest.TestGitHubHost, []domain.TeamGitHubRepo{{Owner: "owner", Repo: "other"}}); err != nil {
		t.Fatalf("teamB track: %v", err)
	}

	renamed, _, err := st.Entities.FindOrCreate(ctx, runmode.LocalDefaultOrgID, "github", dbtest.TestGitHubHost, "owner/old#1", "", "pr", "PR", "")
	if err != nil {
		t.Fatalf("create entity: %v", err)
	}
	dbseed.RenameRepositoryOnly(t, dbh, renamed.RepositoryID, "owner", "new")

	other, err := st.Repos.GetByRefSystem(ctx, runmode.LocalDefaultOrgID, domain.RepoRef{Host: dbtest.TestGitHubHost, Owner: "owner", Repo: "other"})
	if err != nil || other == nil {
		t.Fatalf("resolve owner/other: %v, %v", other, err)
	}
	keyLies := uuid.New().String()
	if _, err := dbh.Exec(`
		INSERT INTO entities (id, source, scope, source_id, kind, title, url, state, created_at, repository_id)
		VALUES (?, 'github', ?, 'owner/new#2', 'pr', 'PR', '', 'active', ?, ?)
	`, keyLies, dbtest.TestGitHubHost, time.Now(), other.ID); err != nil {
		t.Fatalf("seed entity: %v", err)
	}

	r := gateRouter(dbh)
	for _, c := range []struct {
		name     string
		evt      domain.Event
		team     string
		wantPass bool
	}{
		{"renamed repository, its tracking team", ciEvent(t, renamed.ID, "owner/old"), teamA, true},
		{"renamed repository, another team", ciEvent(t, renamed.ID, "owner/old"), teamB, false},
		{"key spelling team A's repository, team A", ciEvent(t, keyLies, "owner/new"), teamA, false},
		{"key spelling team A's repository, the team tracking its row", ciEvent(t, keyLies, "owner/new"), teamB, true},
	} {
		if got := r.scopeFor(ctx, c.evt, c.team).open; got != c.wantPass {
			t.Errorf("%s: gate = %v, want %v", c.name, got, c.wantPass)
		}
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
	// A gate-active router whose teamRepos store tracks *nothing*, so any
	// allow result must come from an escape hatch, not from tracking.
	r := gateRouter(dbh)

	githubEvt := ciEvent(t, "entity-x", "owner/untracked")

	// (1) NULL/empty-team handler (multi-mode system/org-union row) →
	// allowed regardless of tracking. This is acceptance #6's mechanism.
	if !r.handlerScopeMatchesEvent(context.Background(), githubEvt, domain.EventHandler{TeamID: ""}, teamScopeCache{}) {
		t.Error("empty-team (system) handler should skip the gate")
	}

	jiraEvt := domain.Event{
		EventType:    domain.EventJiraIssueAssigned,
		MetadataJSON: `{"project":"SKY"}`,
		OrgID:        runmode.LocalDefaultOrgID,
	}

	// (2) teamRepos + jiraRules unwired (nil) → pre-ticket behavior, never
	// drops, for either source.
	rNil := newTestRouter(testRouterDeps(dbh)) // no tracking gate wired
	if !rNil.handlerScopeMatchesEvent(context.Background(), githubEvt, domain.EventHandler{TeamID: "some-real-team"}, teamScopeCache{}) {
		t.Error("nil teamRepos store should skip the GitHub gate")
	}
	if !rNil.handlerScopeMatchesEvent(context.Background(), jiraEvt, domain.EventHandler{TeamID: "some-real-team"}, teamScopeCache{}) {
		t.Error("nil jiraRules store should skip the Jira gate")
	}

	// (3) Unknown source → ungated even with a gate-active router + a real
	// team that tracks nothing.
	otherEvt := domain.Event{EventType: "system:poll:done", MetadataJSON: `{}`, OrgID: runmode.LocalDefaultOrgID}
	if !r.handlerScopeMatchesEvent(context.Background(), otherEvt, domain.EventHandler{TeamID: "some-real-team"}, teamScopeCache{}) {
		t.Error("non-github/jira event should skip the scope gate")
	}

	// (4) Malformed/empty project metadata on a gate-active router →
	// fail-open (no drop), same policy as the GitHub branch.
	emptyProjEvt := domain.Event{EventType: domain.EventJiraIssueAssigned, MetadataJSON: `{}`, OrgID: runmode.LocalDefaultOrgID}
	if !r.handlerScopeMatchesEvent(context.Background(), emptyProjEvt, domain.EventHandler{TeamID: "some-real-team"}, teamScopeCache{}) {
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
	trackJiraProjects(t, dbh, teamA, skyRule...)
	trackJiraProjects(t, dbh, teamB, opsRule...)

	seedMatchAllJiraAssignedRule(t, dbh, teamA)
	seedMatchAllJiraAssignedRule(t, dbh, teamB)

	// assigned routes by assignee (the owning-team ladder), so the assignee
	// must resolve to a TF team for the owner assertion below; aidan is on
	// teamA. The gate still bounds WHICH teams' handlers match (teamB tracks a
	// different project), independent of ownership.
	setJiraHost(t, dbh)
	seedJiraUserOnTeam(t, dbh, teamA, "acct-aidan", "aidan")

	entity, _, err := st.Entities.FindOrCreateJiraSystem(ctx, runmode.LocalDefaultOrgID, jiraTestHost, "SKY-1", dbseed.JiraIssueID("SKY-1"), dbseed.JiraIssueProject("SKY-1"), "An issue", "https://example.com/SKY-1")
	if err != nil {
		t.Fatalf("create entity: %v", err)
	}

	gateRouter(dbh).HandleEvent(context.Background(), jiraAssignedEvent(t, *entity, "SKY"))

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

func jiraAssignedEvent(t *testing.T, entity domain.Entity, project string) domain.Event {
	t.Helper()
	meta := events.JiraIssueAssignedMetadata{
		Assignee: "aidan", AssigneeAccountID: "acct-aidan", IssueKey: project + "-1", Project: project,
		JiraProjectID: entity.JiraProjectID, Status: "To Do",
	}
	metaJSON, _ := json.Marshal(meta)
	return domain.Event{
		EventType:    domain.EventJiraIssueAssigned,
		EntityID:     &entity.ID,
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
	if err := dbtest.TrackRepos(ctx, st.Repos, st.TeamGitHubRepos, runmode.LocalDefaultOrgID, teamA, dbtest.TestGitHubHost, []domain.TeamGitHubRepo{{Owner: "owner", Repo: "repo-a"}}); err != nil {
		t.Fatalf("teamA track: %v", err)
	}

	// The author is on BOTH teams — the founder-on-every-team shape.
	setReviewHost(t, dbh)
	uid := seedUserOnTeam(t, dbh, teamA, "aidan")
	enrollOnTeam(t, dbh, uid, teamB)

	seedMatchAllCIRule(t, dbh, teamA)
	seedMatchAllCIRule(t, dbh, teamB)

	entity, _, err := st.Entities.FindOrCreate(ctx, runmode.LocalDefaultOrgID, "github", "https://github.com", "owner/repo-a#1", "", "pr", "A PR", "https://example.com/a")
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
	if err := dbtest.TrackRepos(ctx, st.Repos, st.TeamGitHubRepos, runmode.LocalDefaultOrgID, teamA, dbtest.TestGitHubHost, shared); err != nil {
		t.Fatalf("teamA track: %v", err)
	}
	if err := dbtest.TrackRepos(ctx, st.Repos, st.TeamGitHubRepos, runmode.LocalDefaultOrgID, teamB, dbtest.TestGitHubHost, shared); err != nil {
		t.Fatalf("teamB track: %v", err)
	}

	setReviewHost(t, dbh)
	uid := seedUserOnTeam(t, dbh, teamA, "aidan")
	enrollOnTeam(t, dbh, uid, teamB)

	seedMatchAllCIRule(t, dbh, teamA)
	seedMatchAllCIRule(t, dbh, teamB)

	entity, _, err := st.Entities.FindOrCreate(ctx, runmode.LocalDefaultOrgID, "github", "https://github.com", "owner/shared#1", "", "pr", "Shared PR", "https://example.com/s")
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
	if err := dbtest.TrackRepos(ctx, st.Repos, st.TeamGitHubRepos, runmode.LocalDefaultOrgID, teamA, dbtest.TestGitHubHost, []domain.TeamGitHubRepo{{Owner: "owner", Repo: "repo"}}); err != nil {
		t.Fatalf("teamA track: %v", err)
	}
	// N=1: the local author is on the one team, so the author-centric ladder
	// resolves the owner to it.
	setReviewHost(t, dbh)
	seedUserOnTeam(t, dbh, teamA, "aidan")
	seedMatchAllCIRule(t, dbh, teamA)

	entity, _, err := st.Entities.FindOrCreate(ctx, runmode.LocalDefaultOrgID, "github", "https://github.com", "owner/repo#1", "", "pr", "PR", "https://example.com/1")
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

// TestJiraGate_KeyChangedReachesBothProjectsTeams: key_changed passes for a
// team that tracks either the project the issue moved to or the one it left,
// including when the destination is tracked by no team. No other Jira event
// reads old_jira_project_id.
func TestJiraGate_KeyChangedReachesBothProjectsTeams(t *testing.T) {
	dbh := newGateDB(t)
	ctx := context.Background()
	teamA := runmode.LocalDefaultTeamID
	teamB := seedGateTeam(t, dbh, "team-b")
	teamC := seedGateTeam(t, dbh, "team-c")
	for team, project := range map[string]string{teamA: "ENG", teamB: "OPS", teamC: "WEB"} {
		trackJiraProjects(t, dbh, team, domain.JiraProjectStatusRules{
			ProjectKey: project, PickupMembers: jiraRefs("To Do"),
			InProgressMembers: jiraRefs("In Progress"), InProgressCanonical: jiraRef("In Progress"),
			DoneMembers: jiraRefs("Done"), DoneCanonical: jiraRef("Done"),
		})
	}
	r := gateRouter(dbh)

	moved := func(eventType, from, to string) domain.Event {
		meta, _ := json.Marshal(events.JiraIssueKeyChangedMetadata{
			IssueKey: to + "-7", IssueID: "10001", Project: to, JiraProjectID: jiraProjectRow(t, dbh, to),
			OldIssueKey: from + "-1", OldProject: from, OldJiraProjectID: jiraProjectRow(t, dbh, from),
		})
		return domain.Event{EventType: eventType, EntityID: new(string), MetadataJSON: string(meta), CreatedAt: time.Now(), OrgID: runmode.LocalDefaultOrgID}
	}
	cases := []struct {
		name string
		evt  domain.Event
		want map[string]bool
	}{
		{"between tracked projects", moved(domain.EventJiraIssueKeyChanged, "ENG", "OPS"),
			map[string]bool{teamA: true, teamB: true, teamC: false}},
		{"to an untracked project", moved(domain.EventJiraIssueKeyChanged, "ENG", "SEC"),
			map[string]bool{teamA: true, teamB: false, teamC: false}},
		{"another event carrying an old project", moved(domain.EventJiraIssueStatusChanged, "ENG", "OPS"),
			map[string]bool{teamA: false, teamB: true, teamC: false}},
	}
	for _, tc := range cases {
		for team, want := range tc.want {
			if got := r.handlerScopeMatchesEvent(ctx, tc.evt, domain.EventHandler{TeamID: team}, teamScopeCache{}); got != want {
				t.Errorf("%s: gate for team %s = %v, want %v", tc.name, team, got, want)
			}
		}
	}
}

// TestJiraGate_FollowsTheProjectAcrossAKeyRename: the gate matches an event to
// the teams tracking its project by the registry row its metadata names, so an
// event spelling the project's new key passes for the team that tracked the
// project under the old one, whether or not the registry row has been
// re-keyed yet, and an event naming another project's row does not.
func TestJiraGate_FollowsTheProjectAcrossAKeyRename(t *testing.T) {
	dbh := newGateDB(t)
	ctx := context.Background()
	teamA := runmode.LocalDefaultTeamID
	teamB := seedGateTeam(t, dbh, "team-b")
	trackJiraProjects(t, dbh, teamA, domain.JiraProjectStatusRules{
		ProjectKey: "ENG", PickupMembers: jiraRefs("To Do"),
		InProgressMembers: jiraRefs("In Progress"), InProgressCanonical: jiraRef("In Progress"),
		DoneMembers: jiraRefs("Done"), DoneCanonical: jiraRef("Done"),
	})
	eng := jiraProjectRow(t, dbh, "ENG")
	r := gateRouter(dbh)
	assigned := func(key, row string) domain.Event {
		meta, _ := json.Marshal(events.JiraIssueAssignedMetadata{
			IssueKey: key + "-1", Project: key, JiraProjectID: row, Status: "To Do",
		})
		return domain.Event{EventType: domain.EventJiraIssueAssigned, EntityID: new(string), MetadataJSON: string(meta), CreatedAt: time.Now(), OrgID: runmode.LocalDefaultOrgID}
	}
	check := func(when string, evt domain.Event, want map[string]bool) {
		t.Helper()
		for team, w := range want {
			if got := r.handlerScopeMatchesEvent(ctx, evt, domain.EventHandler{TeamID: team}, teamScopeCache{}); got != w {
				t.Errorf("%s: gate for team %s = %v, want %v", when, team, got, w)
			}
		}
	}

	// The cycle that sees the rename: the issue's key is CORE-1 while the
	// registry row still holds ENG.
	check("before the registry row is re-keyed", assigned("CORE", eng), map[string]bool{teamA: true, teamB: false})
	if _, err := sqlitestore.New(dbh).JiraProjects.RefreshSystem(ctx, runmode.LocalDefaultOrgID, eng, "id-ENG", "CORE", "Core"); err != nil {
		t.Fatalf("re-key the registry row: %v", err)
	}
	check("after it is re-keyed", assigned("CORE", eng), map[string]bool{teamA: true, teamB: false})
	check("another project's row under the tracked key", assigned("CORE", jiraProjectRow(t, dbh, "OPS")), map[string]bool{teamA: false, teamB: false})
}
