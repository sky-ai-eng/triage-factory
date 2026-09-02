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

// reviewTestHost is the org's GitHub host. Identities are bound to it and
// org_settings.github_base_url is set to it so the router's user→teams
// resolution finds the same key the writers stored.
const reviewTestHost = "https://github.com"

// reviewRouter wires a router with the stores the review-request routing
// path consumes: Users (login→user_ids), Teams (user→teams), Orgs
// (host), and TeamGitHubGroups (github-team→TF-team).
func reviewRouter(database *sql.DB) *Router {
	st := sqlitestore.New(database)
	return NewRouter(
		testPromptStore(database), testBlueprintStore(database), testEventHandlerStore(database), nil, nil, st.Users,
		testTaskStore(database), st.Conversations, st.Entities, st.PendingFirings, st.Events,
		st.Orgs, st.Teams, nil, nil, st.TeamGitHubGroups, nil, noopScorer{}, websocket.NewHub(),
	)
}

// setReviewHost stamps the github row's base_url on org_event_sources for the
// default org.
func setReviewHost(t *testing.T, database *sql.DB) {
	t.Helper()
	if _, err := database.Exec(`
		INSERT INTO org_event_sources (org_id, kind, base_url) VALUES (?, 'github', ?)
		ON CONFLICT(org_id, kind) DO UPDATE SET base_url = excluded.base_url
	`, runmode.LocalDefaultOrgID, reviewTestHost); err != nil {
		t.Fatalf("set org github base_url: %v", err)
	}
}

// seedTeam inserts a team in the default org and returns its id.
func seedTeam(t *testing.T, database *sql.DB, slug string) string {
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

// seedUserOnTeam creates a user, enrolls them on teamID, and binds login on
// the review test host.
func seedUserOnTeam(t *testing.T, database *sql.DB, teamID, login string) string {
	t.Helper()
	uid := uuid.New().String()
	if _, err := database.Exec(`INSERT INTO users (id, display_name) VALUES (?, ?)`, uid, login); err != nil {
		t.Fatalf("seed user %s: %v", login, err)
	}
	if _, err := database.Exec(`INSERT INTO memberships (user_id, team_id, role) VALUES (?, ?, 'admin')`, uid, teamID); err != nil {
		t.Fatalf("seed membership %s→%s: %v", login, teamID, err)
	}
	if err := sqlitestore.New(database).Users.UpsertGitHubIdentity(context.Background(), uid, reviewTestHost, login, "", "", "pat"); err != nil {
		t.Fatalf("bind identity %s: %v", login, err)
	}
	return uid
}

// seedReviewRule seeds a match-all review_requested rule owned by teamID so
// task creation is gated open. The rule's team gates creation + supplies
// priority but does NOT determine visibility.
func seedReviewRule(t *testing.T, database *sql.DB, teamID string) {
	t.Helper()
	id := uuid.New().String()
	if _, err := database.Exec(`
		INSERT INTO event_handlers
			(id, org_id, team_id, creator_user_id, kind, event_type,
			 scope_predicate_json, enabled, source, name, default_priority, sort_order,
			 created_at, updated_at)
		VALUES (?, ?, ?, ?, 'rule', ?, NULL, 1, 'user', ?, 0.6, 100, ?, ?)
	`, id, runmode.LocalDefaultOrgID, teamID, runmode.LocalDefaultUserID,
		domain.EventGitHubPRReviewRequested, "review rule "+teamID[:8], time.Now(), time.Now()); err != nil {
		t.Fatalf("seed review rule for team %s: %v", teamID, err)
	}
}

func reviewEntity(t *testing.T, database *sql.DB, sourceID string) string {
	t.Helper()
	e, _, err := sqlitestore.New(database).Entities.FindOrCreate(context.Background(), runmode.LocalDefaultOrgID, "github", sourceID, "pr", "Review PR", "https://example.com/"+sourceID)
	if err != nil {
		t.Fatalf("create entity: %v", err)
	}
	return e.ID
}

func emitReviewRequested(router *Router, entityID, dedupKey string, meta events.GitHubPRReviewRequestedMetadata) {
	metaJSON, _ := json.Marshal(meta)
	router.HandleEvent(context.Background(), domain.Event{
		EventType:    domain.EventGitHubPRReviewRequested,
		EntityID:     &entityID,
		DedupKey:     dedupKey,
		MetadataJSON: string(metaJSON),
		OccurredAt:   time.Now(),
		OrgID:        runmode.LocalDefaultOrgID,
	})
}

func visTeamsOf(t *testing.T, database *sql.DB, taskID string) []string {
	t.Helper()
	vis, err := testTaskStore(database).VisibilityTeams(context.Background(), runmode.LocalDefaultOrgID, taskID)
	if err != nil {
		t.Fatalf("VisibilityTeams: %v", err)
	}
	sort.Strings(vis)
	return vis
}

// TestReviewRequested_UserRoutesToRequestedUsersTeams is the acceptance core
// for individual requests: a review_requested for user A routes the task's
// visibility to A's team(s) — resolved from the requested identity, NOT from
// the matched rule's team. Post-TFAC-514 the task is created NULL-owned (no
// trigger here to fire and consolidate); the owner is no longer stamped at
// creation.
func TestReviewRequested_UserRoutesToRequestedUsersTeams(t *testing.T) {
	database := newTestDB(t)
	seedHandlerFKTargets(t, database)
	setReviewHost(t, database)

	teamA := runmode.LocalDefaultTeamID // rule lives here
	teamReviewer := seedTeam(t, database, "reviewer-team")
	seedUserOnTeam(t, database, teamReviewer, "alice")
	seedReviewRule(t, database, teamA) // match-all rule on a DIFFERENT team

	entityID := reviewEntity(t, database, "owner/repo#user")
	router := reviewRouter(database)
	emitReviewRequested(router, entityID, "user:alice", events.GitHubPRReviewRequestedMetadata{
		Author: "carol", Repo: "owner/repo", PRNumber: 1, RequestedLogin: "alice",
	})

	active, err := testTaskStore(database).FindActiveByEntity(context.Background(), runmode.LocalDefaultOrgID, entityID)
	if err != nil || len(active) != 1 {
		t.Fatalf("expected 1 task, got %d (err=%v)", len(active), err)
	}
	if active[0].TeamID != nil {
		t.Errorf("owner team_id = %v, want nil (NULL at creation; no trigger fired to consolidate)", *active[0].TeamID)
	}
	if vis := visTeamsOf(t, database, active[0].ID); len(vis) != 1 || vis[0] != teamReviewer {
		t.Errorf("visibility = %v, want [%s] (the requested user's team only)", vis, teamReviewer)
	}
}

// TestReviewRequested_EmptyOrgHost_DefaultsToGitHubCom guards the common
// github.com case where org_settings.github_base_url is unset: the OAuth
// login-claim binds identities to "https://github.com" literally, so the
// resolver must default an empty host to the deployment default or the reverse lookup
// misses and the task is dropped.
func TestReviewRequested_EmptyOrgHost_DefaultsToGitHubCom(t *testing.T) {
	database := newTestDB(t)
	seedHandlerFKTargets(t, database)
	// Deliberately NOT calling setReviewHost — github_base_url stays empty.

	teamReviewer := seedTeam(t, database, "reviewer-team")
	seedUserOnTeam(t, database, teamReviewer, "alice") // bound under https://github.com
	seedReviewRule(t, database, runmode.LocalDefaultTeamID)

	entityID := reviewEntity(t, database, "owner/repo#emptyhost")
	router := reviewRouter(database)
	emitReviewRequested(router, entityID, "user:alice", events.GitHubPRReviewRequestedMetadata{
		Author: "carol", Repo: "owner/repo", PRNumber: 9, RequestedLogin: "alice",
	})

	active, err := testTaskStore(database).FindActiveByEntity(context.Background(), runmode.LocalDefaultOrgID, entityID)
	if err != nil || len(active) != 1 {
		t.Fatalf("expected 1 task even with empty org github_base_url, got %d (err=%v)", len(active), err)
	}
	if vis := visTeamsOf(t, database, active[0].ID); len(vis) != 1 || vis[0] != teamReviewer {
		t.Errorf("visibility = %v, want [%s] (resolved via github.com default)", vis, teamReviewer)
	}
}

// TestReviewRequested_SharedLoginUnionsTeams is the regression guard for the
// set-valued reverse lookup (acceptance #4): a login bound to two TF users on
// the org's host makes the review task visible to the union of both users'
// teams.
func TestReviewRequested_SharedLoginUnionsTeams(t *testing.T) {
	database := newTestDB(t)
	seedHandlerFKTargets(t, database)
	setReviewHost(t, database)

	teamA := runmode.LocalDefaultTeamID
	teamB := seedTeam(t, database, "team-b")
	// Two distinct TF users share the login "sharedbot" on the host.
	seedUserOnTeam(t, database, teamA, "sharedbot")
	seedUserOnTeam(t, database, teamB, "sharedbot")
	seedReviewRule(t, database, teamA)

	entityID := reviewEntity(t, database, "owner/repo#shared")
	router := reviewRouter(database)
	emitReviewRequested(router, entityID, "user:sharedbot", events.GitHubPRReviewRequestedMetadata{
		Author: "carol", Repo: "owner/repo", PRNumber: 2, RequestedLogin: "sharedbot",
	})

	active, err := testTaskStore(database).FindActiveByEntity(context.Background(), runmode.LocalDefaultOrgID, entityID)
	if err != nil || len(active) != 1 {
		t.Fatalf("expected 1 task, got %d (err=%v)", len(active), err)
	}
	vis := visTeamsOf(t, database, active[0].ID)
	want := []string{teamA, teamB}
	sort.Strings(want)
	if len(vis) != 2 || vis[0] != want[0] || vis[1] != want[1] {
		t.Errorf("visibility = %v, want union of both users' teams %v", vis, want)
	}
}

// TestReviewRequested_TeamRoutesToMappedTFTeams: a github-team review request
// routes to the TF team(s) the github-team→TF-team mapping returns.
func TestReviewRequested_TeamRoutesToMappedTFTeams(t *testing.T) {
	database := newTestDB(t)
	seedHandlerFKTargets(t, database)
	setReviewHost(t, database)

	teamA := runmode.LocalDefaultTeamID
	teamBackend := seedTeam(t, database, "backend-tf")
	if err := sqlitestore.New(database).TeamGitHubGroups.SetForTeam(context.Background(), teamBackend, []domain.TeamGitHubGroup{
		{OrgLogin: "acme", TeamSlug: "backend"},
	}); err != nil {
		t.Fatalf("map github team: %v", err)
	}
	seedReviewRule(t, database, teamA)

	entityID := reviewEntity(t, database, "owner/repo#team")
	router := reviewRouter(database)
	emitReviewRequested(router, entityID, "team:acme/backend", events.GitHubPRReviewRequestedMetadata{
		Author: "carol", Repo: "owner/repo", PRNumber: 3, RequestedTeam: "acme/backend",
	})

	active, err := testTaskStore(database).FindActiveByEntity(context.Background(), runmode.LocalDefaultOrgID, entityID)
	if err != nil || len(active) != 1 {
		t.Fatalf("expected 1 task, got %d (err=%v)", len(active), err)
	}
	if vis := visTeamsOf(t, database, active[0].ID); len(vis) != 1 || vis[0] != teamBackend {
		t.Errorf("visibility = %v, want [%s] (the mapped TF team)", vis, teamBackend)
	}
}

// TestReviewRequested_PerReviewerDistinctTasks is acceptance #1: a PR requesting
// review from user A, user B, and a github team yields three tasks with distinct
// dedup_keys — A reviewing does not consolidate B's.
func TestReviewRequested_PerReviewerDistinctTasks(t *testing.T) {
	database := newTestDB(t)
	seedHandlerFKTargets(t, database)
	setReviewHost(t, database)

	teamA := runmode.LocalDefaultTeamID
	teamAlice := seedTeam(t, database, "alice-team")
	teamBob := seedTeam(t, database, "bob-team")
	teamBackend := seedTeam(t, database, "backend-tf")
	seedUserOnTeam(t, database, teamAlice, "alice")
	seedUserOnTeam(t, database, teamBob, "bob")
	if err := sqlitestore.New(database).TeamGitHubGroups.SetForTeam(context.Background(), teamBackend, []domain.TeamGitHubGroup{
		{OrgLogin: "acme", TeamSlug: "backend"},
	}); err != nil {
		t.Fatalf("map github team: %v", err)
	}
	seedReviewRule(t, database, teamA)

	entityID := reviewEntity(t, database, "owner/repo#multi")
	router := reviewRouter(database)
	emitReviewRequested(router, entityID, "user:alice", events.GitHubPRReviewRequestedMetadata{
		Author: "carol", Repo: "owner/repo", PRNumber: 4, RequestedLogin: "alice"})
	emitReviewRequested(router, entityID, "user:bob", events.GitHubPRReviewRequestedMetadata{
		Author: "carol", Repo: "owner/repo", PRNumber: 4, RequestedLogin: "bob"})
	emitReviewRequested(router, entityID, "team:acme/backend", events.GitHubPRReviewRequestedMetadata{
		Author: "carol", Repo: "owner/repo", PRNumber: 4, RequestedTeam: "acme/backend"})

	active, err := testTaskStore(database).FindActiveByEntity(context.Background(), runmode.LocalDefaultOrgID, entityID)
	if err != nil {
		t.Fatalf("list tasks: %v", err)
	}
	if len(active) != 3 {
		t.Fatalf("expected 3 distinct per-reviewer tasks, got %d", len(active))
	}
	// Post-TFAC-514 each per-reviewer task is NULL-owned at creation (no trigger
	// here to fire and consolidate). Routing is asserted via the per-task
	// visibility set — the reviewer's team(s) — keyed by dedup_key.
	vis := map[string]string{} // dedup_key → sole visible team
	for _, task := range active {
		if task.TeamID != nil {
			t.Errorf("%s task owner = %v, want nil (NULL at creation)", task.DedupKey, *task.TeamID)
		}
		vt := visTeamsOf(t, database, task.ID)
		if len(vt) != 1 {
			t.Errorf("%s task visibility = %v, want exactly one team", task.DedupKey, vt)
			continue
		}
		vis[task.DedupKey] = vt[0]
	}
	if vis["user:alice"] != teamAlice {
		t.Errorf("alice task visible to %q, want %q", vis["user:alice"], teamAlice)
	}
	if vis["user:bob"] != teamBob {
		t.Errorf("bob task visible to %q, want %q", vis["user:bob"], teamBob)
	}
	if vis["team:acme/backend"] != teamBackend {
		t.Errorf("team task visible to %q, want %q", vis["team:acme/backend"], teamBackend)
	}
}

// TestReviewRequested_UnknownIdentity_NoTask is acceptance #3 at the router
// layer: a requested reviewer that resolves to no TF team (no identity row /
// no mapping) records the event but creates no task.
func TestReviewRequested_UnknownIdentity_NoTask(t *testing.T) {
	database := newTestDB(t)
	seedHandlerFKTargets(t, database)
	setReviewHost(t, database)
	seedReviewRule(t, database, runmode.LocalDefaultTeamID)

	entityID := reviewEntity(t, database, "owner/repo#unknown")
	router := reviewRouter(database)
	emitReviewRequested(router, entityID, "user:ghost", events.GitHubPRReviewRequestedMetadata{
		Author: "carol", Repo: "owner/repo", PRNumber: 5, RequestedLogin: "ghost",
	})

	active, err := testTaskStore(database).FindActiveByEntity(context.Background(), runmode.LocalDefaultOrgID, entityID)
	if err != nil {
		t.Fatalf("list tasks: %v", err)
	}
	if len(active) != 0 {
		t.Fatalf("expected no task for an unmapped requested reviewer, got %d", len(active))
	}
}

// TestReviewByReviewer_ClosesOnlyTheirTask pins the parity-correct close: when
// reviewer A submits a review (any typed review_* event), only A's per-reviewer
// review_requested task closes — B's stays open. Driven off the typed review
// event, so it works for any reviewer in any mode.
func TestReviewByReviewer_ClosesOnlyTheirTask(t *testing.T) {
	database := newTestDB(t)
	seedHandlerFKTargets(t, database)
	setReviewHost(t, database)

	teamAlice := seedTeam(t, database, "alice-team")
	teamBob := seedTeam(t, database, "bob-team")
	seedUserOnTeam(t, database, teamAlice, "alice")
	seedUserOnTeam(t, database, teamBob, "bob")
	seedReviewRule(t, database, runmode.LocalDefaultTeamID)

	entityID := reviewEntity(t, database, "owner/repo#submit")
	router := reviewRouter(database)
	emitReviewRequested(router, entityID, "user:alice", events.GitHubPRReviewRequestedMetadata{
		Author: "carol", Repo: "owner/repo", PRNumber: 7, RequestedLogin: "alice"})
	emitReviewRequested(router, entityID, "user:bob", events.GitHubPRReviewRequestedMetadata{
		Author: "carol", Repo: "owner/repo", PRNumber: 7, RequestedLogin: "bob"})

	// Alice reviews (approves).
	subMeta, _ := json.Marshal(events.GitHubPRReviewApprovedMetadata{
		Author: "carol", Reviewer: "alice", Repo: "owner/repo", PRNumber: 7,
	})
	router.HandleEvent(context.Background(), domain.Event{
		EventType:    domain.EventGitHubPRReviewApproved,
		EntityID:     &entityID,
		MetadataJSON: string(subMeta),
		OrgID:        runmode.LocalDefaultOrgID,
	})

	active, err := testTaskStore(database).FindActiveByEntity(context.Background(), runmode.LocalDefaultOrgID, entityID)
	if err != nil {
		t.Fatalf("list tasks: %v", err)
	}
	if len(active) != 1 {
		t.Fatalf("expected 1 surviving task (bob's), got %d", len(active))
	}
	if active[0].DedupKey != "user:bob" {
		t.Errorf("surviving task dedup_key = %q, want user:bob (alice's was closed by her review)", active[0].DedupKey)
	}
}

// TestReviewRequestRemoved_ClosesOnlyMatchingReviewer pins the per-identity
// removal: dropping reviewer A closes A's task but leaves B's open (close by
// dedup_key).
func TestReviewRequestRemoved_ClosesOnlyMatchingReviewer(t *testing.T) {
	database := newTestDB(t)
	seedHandlerFKTargets(t, database)
	setReviewHost(t, database)

	teamAlice := seedTeam(t, database, "alice-team")
	teamBob := seedTeam(t, database, "bob-team")
	seedUserOnTeam(t, database, teamAlice, "alice")
	seedUserOnTeam(t, database, teamBob, "bob")
	seedReviewRule(t, database, runmode.LocalDefaultTeamID)

	entityID := reviewEntity(t, database, "owner/repo#remove")
	router := reviewRouter(database)
	emitReviewRequested(router, entityID, "user:alice", events.GitHubPRReviewRequestedMetadata{
		Author: "carol", Repo: "owner/repo", PRNumber: 6, RequestedLogin: "alice"})
	emitReviewRequested(router, entityID, "user:bob", events.GitHubPRReviewRequestedMetadata{
		Author: "carol", Repo: "owner/repo", PRNumber: 6, RequestedLogin: "bob"})

	// Drop alice's request only.
	removedMeta, _ := json.Marshal(events.GitHubPRReviewRequestRemovedMetadata{
		Author: "carol", Repo: "owner/repo", PRNumber: 6, RequestedLogin: "alice",
	})
	router.HandleEvent(context.Background(), domain.Event{
		EventType:    domain.EventGitHubPRReviewRequestRemoved,
		EntityID:     &entityID,
		DedupKey:     "user:alice",
		MetadataJSON: string(removedMeta),
		OrgID:        runmode.LocalDefaultOrgID,
	})

	active, err := testTaskStore(database).FindActiveByEntity(context.Background(), runmode.LocalDefaultOrgID, entityID)
	if err != nil {
		t.Fatalf("list tasks: %v", err)
	}
	if len(active) != 1 {
		t.Fatalf("expected 1 surviving task (bob's), got %d", len(active))
	}
	if active[0].DedupKey != "user:bob" {
		t.Errorf("surviving task dedup_key = %q, want user:bob (alice's was closed)", active[0].DedupKey)
	}
}

// TestReviewRequested_MultiTeamReviewer_FiresDeterministically pins the
// TFAC-514 review_requested change: a reviewer on two teams, each with an
// enabled pr-review trigger, yields a NULL-owned task visible to both teams that
// fires deterministically — exactly one team's trigger runs (orderedTeams is the
// reviewer teams sorted by id, so the lowest-id team wins the exclusive claim)
// and the fire consolidates ownership onto it.
func TestReviewRequested_MultiTeamReviewer_FiresDeterministically(t *testing.T) {
	database := newTestDB(t)
	seedHandlerFKTargets(t, database)
	setReviewHost(t, database)

	teamA := seedTeam(t, database, "rev-a")
	teamB := seedTeam(t, database, "rev-b")
	// One login bound to two TF users, one per team → reviewer maps to both.
	seedUserOnTeam(t, database, teamA, "alice")
	seedUserOnTeam(t, database, teamB, "alice")
	enableTeamAutoDelegate(t, database, teamA, teamB)
	seedImmediateTrigger(t, database, teamA, domain.EventGitHubPRReviewRequested, "rev-a")
	seedImmediateTrigger(t, database, teamB, domain.EventGitHubPRReviewRequested, "rev-b")

	entityID := reviewEntity(t, database, "owner/repo#multirev")
	stub := &stubDelegator{db: database}
	router := firingRouter(database, stub)
	emitReviewRequested(router, entityID, "user:alice", events.GitHubPRReviewRequestedMetadata{
		Author: "carol", Repo: "owner/repo", PRNumber: 1, RequestedLogin: "alice",
	})

	active, err := testTaskStore(database).FindActiveByEntity(context.Background(), runmode.LocalDefaultOrgID, entityID)
	if err != nil || len(active) != 1 {
		t.Fatalf("expected 1 task, got %d (err=%v)", len(active), err)
	}
	if stub.calls != 1 {
		t.Fatalf("expected exactly 1 fire (deterministic), got %d", stub.calls)
	}
	want := minStr(teamA, teamB) // reqTeams sorted by id; lowest fires first and wins
	if teamIDValue(&active[0]) != want {
		t.Errorf("owner = %q after fire, want the deterministic team %q", teamIDValue(&active[0]), want)
	}
	if active[0].ClaimedByAgentID == "" {
		t.Error("task should be bot-claimed by the firing team")
	}
	if vis := visTeamsOf(t, database, active[0].ID); len(vis) != 2 {
		t.Errorf("visibility = %v, want both reviewer teams", vis)
	}
}

// TestReviewRequested_SingleTeamReviewer_FiresAndOwns pins the unchanged end
// state for the single-team reviewer post-TFAC-514: created NULL-owned, but the
// pr-review trigger fires and consolidates the owner onto the reviewer's one
// team — no observable change from the pre-change stamp-then-fire behavior.
func TestReviewRequested_SingleTeamReviewer_FiresAndOwns(t *testing.T) {
	database := newTestDB(t)
	seedHandlerFKTargets(t, database)
	setReviewHost(t, database)

	teamReviewer := seedTeam(t, database, "rev-solo")
	seedUserOnTeam(t, database, teamReviewer, "alice")
	enableTeamAutoDelegate(t, database, teamReviewer)
	seedImmediateTrigger(t, database, teamReviewer, domain.EventGitHubPRReviewRequested, "rev-solo")

	entityID := reviewEntity(t, database, "owner/repo#solorev")
	stub := &stubDelegator{db: database}
	router := firingRouter(database, stub)
	emitReviewRequested(router, entityID, "user:alice", events.GitHubPRReviewRequestedMetadata{
		Author: "carol", Repo: "owner/repo", PRNumber: 2, RequestedLogin: "alice",
	})

	active, err := testTaskStore(database).FindActiveByEntity(context.Background(), runmode.LocalDefaultOrgID, entityID)
	if err != nil || len(active) != 1 {
		t.Fatalf("expected 1 task, got %d (err=%v)", len(active), err)
	}
	if stub.calls != 1 {
		t.Fatalf("expected exactly 1 fire, got %d", stub.calls)
	}
	if teamIDValue(&active[0]) != teamReviewer {
		t.Errorf("owner = %q after fire, want the reviewer's one team %q (unchanged end state)", teamIDValue(&active[0]), teamReviewer)
	}
	if active[0].ClaimedByAgentID == "" {
		t.Error("task should be bot-claimed after the reviewer's team fired")
	}
}
