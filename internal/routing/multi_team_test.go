package routing

import (
	"context"
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

// TestHandleEvent_MultipleTeams_OneTask pins the core of the task-model
// reversal: when one event matches rules belonging to N different
// teams, the router creates ONE task — not N. The teams are recorded
// as a visibility set (task_teams), and the owning team_id is the
// deterministic pick among them.
func TestHandleEvent_MultipleTeams_OneTask(t *testing.T) {
	database := newTestDB(t)
	seedHandlerFKTargets(t, database)

	// Seed a second team alongside LocalDefaultTeamID.
	teamA := runmode.LocalDefaultTeamID
	teamB := uuid.New().String()
	if _, err := database.Exec(
		`INSERT INTO teams (id, org_id, slug, name) VALUES (?, ?, ?, ?)`,
		teamB, runmode.LocalDefaultOrgID, "team-b", "Team B",
	); err != nil {
		t.Fatalf("seed team B: %v", err)
	}

	entity, _, err := sqlitestore.New(database).Entities.FindOrCreate(context.Background(), runmode.LocalDefaultOrgID, "github", "owner/repo#multi", "pr", "Multi-team PR", "https://example.com/multi")
	if err != nil {
		t.Fatalf("create entity: %v", err)
	}

	// Two user-source rules on the same event, one per team. Both
	// match (empty predicate = match all), so the router records both
	// teams in the visibility set of the single task.
	for _, teamID := range []string{teamA, teamB} {
		ruleID := uuid.New().String()
		if _, err := database.Exec(`
			INSERT INTO event_handlers
				(id, org_id, team_id, creator_user_id, kind, event_type,
				 scope_predicate_json, enabled, source,
				 name, default_priority, sort_order,
				 created_at, updated_at)
			VALUES (?, ?, ?, ?, 'rule', ?,
			        NULL, 1, 'user',
			        ?, 0.7, 100,
			        ?, ?)
		`, ruleID, runmode.LocalDefaultOrg, teamID, runmode.LocalDefaultUserID, domain.EventGitHubPRCICheckFailed,
			"CI rule "+teamID[:8], time.Now(), time.Now()); err != nil {
			t.Fatalf("seed rule for team %s: %v", teamID, err)
		}
	}

	meta := events.GitHubPRCICheckFailedMetadata{
		Author:    "aidan",
		CheckName: "build",
		Repo:      "owner/repo",
		HeadSHA:   "abc123",
	}
	metaJSON, _ := json.Marshal(meta)

	router := NewRouter(testPromptStore(database), testEventHandlerStore(database), nil, nil, nil, testTaskStore(database), sqlitestore.New(database).AgentRuns, sqlitestore.New(database).Entities, sqlitestore.New(database).PendingFirings, sqlitestore.New(database).Events, sqlitestore.New(database).Orgs, sqlitestore.New(database).Teams, nil, nil, nil, nil, noopScorer{}, websocket.NewHub())

	router.HandleEvent(domain.Event{
		EventType:    domain.EventGitHubPRCICheckFailed,
		EntityID:     &entity.ID,
		DedupKey:     "build",
		MetadataJSON: string(metaJSON),
		CreatedAt:    time.Now(),
		OrgID:        runmode.LocalDefaultOrg,
	})

	active, err := testTaskStore(database).FindActiveByEntity(t.Context(), runmode.LocalDefaultOrg, entity.ID)
	if err != nil {
		t.Fatalf("list active tasks: %v", err)
	}
	if len(active) != 1 {
		t.Fatalf("expected exactly 1 task for the situation (team is visibility, not count), got %d", len(active))
	}

	// Owner is the deterministic pick — lowest team id on a priority
	// tie. teamA (LocalDefaultTeamID, all-zero prefix) sorts below the
	// random teamB.
	if active[0].TeamID != teamA {
		t.Errorf("owner team_id = %q, want teamA %q (lowest-id tie-break)", active[0].TeamID, teamA)
	}

	// Both teams are in the visibility set.
	vis, err := testTaskStore(database).VisibilityTeams(t.Context(), runmode.LocalDefaultOrg, active[0].ID)
	if err != nil {
		t.Fatalf("VisibilityTeams: %v", err)
	}
	sort.Strings(vis)
	want := []string{teamA, teamB}
	sort.Strings(want)
	if len(vis) != 2 || vis[0] != want[0] || vis[1] != want[1] {
		t.Errorf("visibility set = %v, want both teams %v", vis, want)
	}
}

// TestHandleEvent_BackfillCreatedAt_PreservesOccurredAt pins that when
// an event arrives with a non-zero OccurredAt (e.g. the tracker's
// review-requested backfill stamped with the PR's CreatedAt), the
// task's created_at reflects when the event happened, not when the
// router processed it. Without this the queue ordering would treat a
// week-old review request as "just discovered."
func TestHandleEvent_BackfillCreatedAt_PreservesOccurredAt(t *testing.T) {
	database := newTestDB(t)
	seedHandlerFKTargets(t, database)
	if err := testEventHandlerStore(database).Seed(t.Context(), runmode.LocalDefaultOrg, runmode.LocalDefaultTeamID, seedHandlerFKTargets(t, database)); err != nil {
		t.Fatalf("seed event handlers: %v", err)
	}

	entity, _, err := sqlitestore.New(database).Entities.FindOrCreate(context.Background(), runmode.LocalDefaultOrgID, "github", "owner/repo#backfill", "pr", "Stale PR", "https://example.com/backfill")
	if err != nil {
		t.Fatalf("create entity: %v", err)
	}

	// 14 days ago — typical "PR has been awaiting your review for
	// weeks" backfill case.
	occurred := time.Now().Add(-14 * 24 * time.Hour).Truncate(time.Second)

	meta := events.GitHubPRReviewRequestedMetadata{
		Author:   "alice",
		Repo:     "owner/repo",
		PRNumber: 7,
		HeadSHA:  "abc123",
	}
	metaJSON, _ := json.Marshal(meta)

	router := NewRouter(testPromptStore(database), testEventHandlerStore(database), nil, nil, nil, testTaskStore(database), sqlitestore.New(database).AgentRuns, sqlitestore.New(database).Entities, sqlitestore.New(database).PendingFirings, sqlitestore.New(database).Events, sqlitestore.New(database).Orgs, sqlitestore.New(database).Teams, nil, nil, nil, nil, noopScorer{}, websocket.NewHub())

	router.HandleEvent(domain.Event{
		EventType:    domain.EventGitHubPRReviewRequested,
		EntityID:     &entity.ID,
		MetadataJSON: string(metaJSON),
		OccurredAt:   occurred,
		OrgID:        runmode.LocalDefaultOrg,
	})

	active, err := testTaskStore(database).FindActiveByEntity(t.Context(), runmode.LocalDefaultOrg, entity.ID)
	if err != nil || len(active) != 1 {
		t.Fatalf("expected 1 active task, got %d (err=%v)", len(active), err)
	}
	if !active[0].CreatedAt.Equal(occurred) {
		t.Errorf("task.CreatedAt = %v, want OccurredAt %v (backfill timestamp regression)", active[0].CreatedAt, occurred)
	}
}

// TestHandleEvent_NoOccurredAt_FallsBackToNow ensures the OccurredAt
// path doesn't accidentally land zero times on poll-detected events
// that legitimately omit the field. Sanity-check companion to the
// backfill test above.
func TestHandleEvent_NoOccurredAt_FallsBackToNow(t *testing.T) {
	database := newTestDB(t)
	seedHandlerFKTargets(t, database)
	if err := testEventHandlerStore(database).Seed(t.Context(), runmode.LocalDefaultOrg, runmode.LocalDefaultTeamID, seedHandlerFKTargets(t, database)); err != nil {
		t.Fatalf("seed event handlers: %v", err)
	}

	entity, _, err := sqlitestore.New(database).Entities.FindOrCreate(context.Background(), runmode.LocalDefaultOrgID, "github", "owner/repo#now", "pr", "Now PR", "https://example.com/now")
	if err != nil {
		t.Fatalf("create entity: %v", err)
	}

	meta := events.GitHubPRCICheckFailedMetadata{
		Author:    "aidan",
		CheckName: "build",
		Repo:      "owner/repo",
		HeadSHA:   "abc123",
	}
	metaJSON, _ := json.Marshal(meta)

	before := time.Now()
	router := NewRouter(testPromptStore(database), testEventHandlerStore(database), nil, nil, nil, testTaskStore(database), sqlitestore.New(database).AgentRuns, sqlitestore.New(database).Entities, sqlitestore.New(database).PendingFirings, sqlitestore.New(database).Events, sqlitestore.New(database).Orgs, sqlitestore.New(database).Teams, nil, nil, nil, nil, noopScorer{}, websocket.NewHub())
	router.HandleEvent(domain.Event{
		EventType:    domain.EventGitHubPRCICheckFailed,
		EntityID:     &entity.ID,
		DedupKey:     "build",
		MetadataJSON: string(metaJSON),
		// OccurredAt deliberately zero.
		OrgID: runmode.LocalDefaultOrg,
	})

	active, err := testTaskStore(database).FindActiveByEntity(t.Context(), runmode.LocalDefaultOrg, entity.ID)
	if err != nil || len(active) != 1 {
		t.Fatalf("expected 1 active task, got %d (err=%v)", len(active), err)
	}
	if active[0].CreatedAt.Before(before) {
		t.Errorf("task.CreatedAt = %v predates router invocation (%v); want time.Now() fallback", active[0].CreatedAt, before)
	}
}

// TestHandleEvent_BecameAtomic_Suppressed pins that became_atomic does
// not create a duplicate card when the entity already has an active
// task. With one task per situation the suppression is per-task: any
// active task on the entity blocks the belated became_atomic card,
// regardless of which team's rule matched.
func TestHandleEvent_BecameAtomic_Suppressed(t *testing.T) {
	database := newTestDB(t)
	seedHandlerFKTargets(t, database)

	teamA := runmode.LocalDefaultTeamID
	teamB := uuid.New().String()
	if _, err := database.Exec(
		`INSERT INTO teams (id, org_id, slug, name) VALUES (?, ?, ?, ?)`,
		teamB, runmode.LocalDefaultOrgID, "team-b-atomic", "Team B Atomic",
	); err != nil {
		t.Fatalf("seed team B: %v", err)
	}

	entity, _, err := sqlitestore.New(database).Entities.FindOrCreate(context.Background(), runmode.LocalDefaultOrgID, "jira", "SKY-700", "issue", "Cross-team atomic", "https://jira.example.com/browse/SKY-700")
	if err != nil {
		t.Fatalf("create entity: %v", err)
	}

	// Pre-seed: an assigned task already exists on this entity.
	priorEventID, err := sqlitestore.New(database).Events.Record(context.Background(), runmode.LocalDefaultOrg, domain.Event{
		EventType:    domain.EventJiraIssueAssigned,
		EntityID:     &entity.ID,
		MetadataJSON: `{}`,
	})
	if err != nil {
		t.Fatalf("record prior event: %v", err)
	}
	if _, _, err := testTaskStore(database).FindOrCreate(t.Context(), runmode.LocalDefaultOrg, teamA, entity.ID, domain.EventJiraIssueAssigned, "", priorEventID, 0.5); err != nil {
		t.Fatalf("create prior assigned task: %v", err)
	}

	// Both teams have a rule matching became_atomic.
	for _, teamID := range []string{teamA, teamB} {
		ruleID := uuid.New().String()
		if _, err := database.Exec(`
			INSERT INTO event_handlers
				(id, org_id, team_id, creator_user_id, kind, event_type,
				 scope_predicate_json, enabled, source,
				 name, default_priority, sort_order,
				 created_at, updated_at)
			VALUES (?, ?, ?, ?, 'rule', ?,
			        NULL, 1, 'user',
			        ?, 0.7, 100, ?, ?)
		`, ruleID, runmode.LocalDefaultOrg, teamID, runmode.LocalDefaultUserID, domain.EventJiraIssueBecameAtomic,
			"became_atomic "+teamID[:8], time.Now(), time.Now()); err != nil {
			t.Fatalf("seed rule for team %s: %v", teamID, err)
		}
	}

	atomicMeta := events.JiraIssueBecameAtomicMetadata{
		Assignee:          "aidan",
		AssigneeAccountID: "557058:abc-aidan",
		IssueKey:          "SKY-700",
		Project:           "SKY",
	}
	atomicJSON, _ := json.Marshal(atomicMeta)

	router := NewRouter(testPromptStore(database), testEventHandlerStore(database), nil, nil, nil, testTaskStore(database), sqlitestore.New(database).AgentRuns, sqlitestore.New(database).Entities, sqlitestore.New(database).PendingFirings, sqlitestore.New(database).Events, sqlitestore.New(database).Orgs, sqlitestore.New(database).Teams, nil, nil, nil, nil, noopScorer{}, websocket.NewHub())
	router.HandleEvent(domain.Event{
		EventType:    domain.EventJiraIssueBecameAtomic,
		EntityID:     &entity.ID,
		MetadataJSON: string(atomicJSON),
		OrgID:        runmode.LocalDefaultOrg,
	})

	active, err := testTaskStore(database).FindActiveByEntity(t.Context(), runmode.LocalDefaultOrg, entity.ID)
	if err != nil {
		t.Fatalf("list active tasks: %v", err)
	}
	// Still exactly the pre-existing assigned task: became_atomic is
	// suppressed because an active task already exists.
	if len(active) != 1 {
		t.Fatalf("expected 1 active task (assigned kept, became_atomic suppressed), got %d", len(active))
	}
	if active[0].EventType != domain.EventJiraIssueAssigned {
		t.Errorf("surviving task event_type = %q, want %q", active[0].EventType, domain.EventJiraIssueAssigned)
	}
}

// TestTryAutoDelegate_PerTeamBotGate pins that tryAutoDelegate's
// team_agents gate reads the FIRING team's row — the team whose
// trigger routed the bot here — not the task's owner team. In a
// two-team org where team B disabled the bot, firing the trigger as
// team A delegates while firing it as team B is blocked, even though
// both act on the same single task.
func TestTryAutoDelegate_PerTeamBotGate(t *testing.T) {
	database := newTestDB(t)

	teamA := runmode.LocalDefaultTeamID
	teamB := uuid.New().String()
	if _, err := database.Exec(
		`INSERT INTO teams (id, org_id, slug, name) VALUES (?, ?, ?, ?)`,
		teamB, runmode.LocalDefaultOrgID, "team-b-bot-gate", "Team B Gate",
	); err != nil {
		t.Fatalf("seed team B: %v", err)
	}

	stores := sqlitestore.New(database)
	if _, err := database.Exec(
		`INSERT INTO agents (id, org_id, display_name) VALUES (?, ?, 'Test Bot')`,
		runmode.LocalDefaultAgentID, runmode.LocalDefaultOrgID,
	); err != nil {
		t.Fatalf("seed agent: %v", err)
	}
	if err := stores.TeamAgents.AddForTeam(t.Context(), runmode.LocalDefaultOrg, teamA, runmode.LocalDefaultAgentID); err != nil {
		t.Fatalf("add agent to team A: %v", err)
	}
	if err := stores.TeamAgents.AddForTeam(t.Context(), runmode.LocalDefaultOrg, teamB, runmode.LocalDefaultAgentID); err != nil {
		t.Fatalf("add agent to team B: %v", err)
	}
	if err := stores.TeamAgents.SetEnabled(t.Context(), runmode.LocalDefaultOrg, teamB, runmode.LocalDefaultAgentID, false); err != nil {
		t.Fatalf("disable agent for team B: %v", err)
	}

	// One entity, one event, ONE task — shared across both teams.
	entity, _, err := stores.Entities.FindOrCreate(context.Background(), runmode.LocalDefaultOrgID, "github", "owner/repo#gate", "pr", "Gate PR", "https://example.com/gate")
	if err != nil {
		t.Fatalf("create entity: %v", err)
	}
	createTestPrompt(t, database, domain.Prompt{ID: "p-gate", Name: "Gate", Body: "x", Source: "user"})
	eventID, err := sqlitestore.New(database).Events.Record(context.Background(), runmode.LocalDefaultOrg, domain.Event{
		EventType:    domain.EventGitHubPRCICheckFailed,
		EntityID:     &entity.ID,
		DedupKey:     "build",
		MetadataJSON: `{"check_name":"build"}`,
	})
	if err != nil {
		t.Fatalf("record event: %v", err)
	}
	task, _, err := testTaskStore(database).FindOrCreate(t.Context(), runmode.LocalDefaultOrg, teamA, entity.ID, domain.EventGitHubPRCICheckFailed, "build", eventID, 0.5)
	if err != nil {
		t.Fatalf("create task: %v", err)
	}

	trigger := domain.EventHandler{
		ID:                     "trigger-gate",
		Kind:                   domain.EventHandlerKindTrigger,
		PromptID:               "p-gate",
		TriggerType:            domain.TriggerTypeEvent,
		EventType:              domain.EventGitHubPRCICheckFailed,
		BreakerThreshold:       intPtr(4),
		MinAutonomySuitability: floatPtr(0),
		Enabled:                true,
	}
	createTriggerForTestRouting(t, database, trigger)

	stub := &stubDelegator{db: database}
	router := NewRouter(testPromptStore(database), testEventHandlerStore(database), stores.Agents, stores.TeamAgents, nil, testTaskStore(database), stores.AgentRuns, stores.Entities, stores.PendingFirings, stores.Events, stores.Orgs, stores.Teams, nil, nil, nil, stub, noopScorer{}, websocket.NewHub())

	// Fire the trigger as team B (bot disabled) — must be blocked — and
	// as team A (bot enabled) — must delegate. Order doesn't matter:
	// team B's gate returns before the entity gate.
	router.tryAutoDelegate(runmode.LocalDefaultOrg, task, trigger, entity.ID, eventID, teamB)
	router.tryAutoDelegate(runmode.LocalDefaultOrg, task, trigger, entity.ID, eventID, teamA)

	if stub.calls != 1 {
		t.Fatalf("expected exactly 1 Delegate call (team A only); got %d", stub.calls)
	}
	got, _ := testTaskStore(database).Get(t.Context(), runmode.LocalDefaultOrg, task.ID)
	if got.ClaimedByAgentID == "" {
		t.Errorf("task: ClaimedByAgentID empty; expected agent claim after team A's successful fire")
	}
	// The claim consolidated the owning team to the acting (firing)
	// team A.
	if got.TeamID != teamA {
		t.Errorf("owner team_id = %q after team A claim, want %q", got.TeamID, teamA)
	}
}

// TestHandleEvent_MultipleTeams_OneBotRun pins the exclusive-claim
// contention fix: when two teams both have auto-delegation fully
// enabled and an immediate trigger on the same event, the single shared
// task gets exactly ONE bot run (the first team in priority/id order
// wins the claim and becomes the owner), and the losing team does NOT
// leave a queued firing behind that would drain into a duplicate run.
func TestHandleEvent_MultipleTeams_OneBotRun(t *testing.T) {
	database := newTestDB(t)
	seedHandlerFKTargets(t, database)
	stores := sqlitestore.New(database)

	teamA := runmode.LocalDefaultTeamID
	teamB := "00000000-0000-0000-0000-0000000000b1"
	if _, err := database.Exec(`INSERT INTO teams (id, org_id, slug, name) VALUES (?, ?, 'team-b-onerun', 'Team B One-Run')`, teamB, runmode.LocalDefaultOrgID); err != nil {
		t.Fatalf("seed team B: %v", err)
	}
	// Both teams opt fully into auto-delegation.
	if _, err := database.Exec(`INSERT OR IGNORE INTO team_settings (team_id, auto_delegate_enabled) VALUES (?, 1)`, teamA); err != nil {
		t.Fatalf("seed team A settings: %v", err)
	}
	if _, err := database.Exec(`INSERT INTO team_settings (team_id, auto_delegate_enabled) VALUES (?, 1)`, teamB); err != nil {
		t.Fatalf("seed team B settings: %v", err)
	}
	if _, err := database.Exec(`INSERT OR IGNORE INTO agents (id, org_id, display_name) VALUES (?, ?, 'Test Bot')`, runmode.LocalDefaultAgentID, runmode.LocalDefaultOrgID); err != nil {
		t.Fatalf("seed agent: %v", err)
	}
	if err := stores.TeamAgents.AddForTeam(t.Context(), runmode.LocalDefaultOrg, teamA, runmode.LocalDefaultAgentID); err != nil {
		t.Fatalf("add agent to team A: %v", err)
	}
	if err := stores.TeamAgents.AddForTeam(t.Context(), runmode.LocalDefaultOrg, teamB, runmode.LocalDefaultAgentID); err != nil {
		t.Fatalf("add agent to team B: %v", err)
	}

	entity, _, err := stores.Entities.FindOrCreate(context.Background(), runmode.LocalDefaultOrgID, "github", "owner/repo#onerun", "pr", "One-run PR", "https://example.com/onerun")
	if err != nil {
		t.Fatalf("create entity: %v", err)
	}
	createTestPrompt(t, database, domain.Prompt{ID: "p-onerun", Name: "One-run", Body: "x", Source: "user"})
	// Team B needs its OWN prompt copy — the same-team trigger→prompt FK
	// forbids team B's trigger from binding team A's p-onerun (SKY-380).
	insertPromptForTeam(t, database, "p-onerun-b", teamB)

	// teamA's immediate trigger (createTriggerForTestRouting hard-codes
	// LocalDefaultTeamID = teamA).
	createTriggerForTestRouting(t, database, domain.EventHandler{
		ID: "trigger-A-onerun", Kind: domain.EventHandlerKindTrigger,
		PromptID: "p-onerun", TriggerType: domain.TriggerTypeEvent,
		EventType: domain.EventGitHubPRCICheckFailed, BreakerThreshold: intPtr(4),
		MinAutonomySuitability: floatPtr(0), Enabled: true,
	})
	// teamB's immediate trigger (raw insert for the second team), bound to
	// team B's own prompt copy.
	if _, err := database.Exec(`
		INSERT INTO event_handlers
			(id, org_id, team_id, creator_user_id, kind, event_type,
			 scope_predicate_json, enabled, source,
			 prompt_id, breaker_threshold, min_autonomy_suitability,
			 created_at, updated_at)
		VALUES (?, ?, ?, ?, 'trigger', ?, NULL, 1, 'user', ?, 4, 0, datetime('now'), datetime('now'))
	`, "trigger-B-onerun", runmode.LocalDefaultOrg, teamB, runmode.LocalDefaultUserID,
		domain.EventGitHubPRCICheckFailed, "p-onerun-b"); err != nil {
		t.Fatalf("seed team B trigger: %v", err)
	}

	meta := events.GitHubPRCICheckFailedMetadata{Author: "aidan", CheckName: "build", Repo: "owner/repo"}
	metaJSON, _ := json.Marshal(meta)

	stub := &stubDelegator{db: database}
	router := NewRouter(testPromptStore(database), testEventHandlerStore(database), stores.Agents, stores.TeamAgents, nil, testTaskStore(database), stores.AgentRuns, stores.Entities, stores.PendingFirings, stores.Events, stores.Orgs, stores.Teams, nil, nil, nil, stub, noopScorer{}, websocket.NewHub())

	router.HandleEvent(domain.Event{
		EventType: domain.EventGitHubPRCICheckFailed, EntityID: &entity.ID,
		DedupKey: "build", MetadataJSON: string(metaJSON), OrgID: runmode.LocalDefaultOrg,
	})

	active, err := testTaskStore(database).FindActiveByEntity(t.Context(), runmode.LocalDefaultOrg, entity.ID)
	if err != nil || len(active) != 1 {
		t.Fatalf("expected 1 task, got %d (err=%v)", len(active), err)
	}
	if stub.calls != 1 {
		t.Errorf("expected exactly 1 bot run (exclusive claim), got %d", stub.calls)
	}
	if active[0].ClaimedByAgentID == "" {
		t.Error("task should be bot-claimed after the winning team fired")
	}
	if active[0].TeamID != teamA {
		t.Errorf("owner team_id = %q, want teamA %q (first in priority/id order wins the claim)", active[0].TeamID, teamA)
	}
	// The losing team (B) must not have left a queued firing that would
	// later drain into a duplicate run.
	firings, err := stores.PendingFirings.ListForEntity(t.Context(), runmode.LocalDefaultOrg, entity.ID)
	if err != nil {
		t.Fatalf("list firings: %v", err)
	}
	if len(firings) != 0 {
		t.Errorf("team B left %d queued firing(s) on the bot-claimed task; want 0 (would drain into a duplicate run)", len(firings))
	}
}

// TestHandleEvent_OwnerDisabled_RunAttributedToActingTeam pins that
// when the highest-priority owner team has auto-delegation disabled and
// a lower-priority team fires the bot, the owner is consolidated to the
// acting team BEFORE the run is created — so the run (which inherits
// runs.team_id from tasks.team_id) is attributed to the team that
// acted, not the stale owner. The stub records the task's owner team at
// Delegate time.
func TestHandleEvent_OwnerDisabled_RunAttributedToActingTeam(t *testing.T) {
	database := newTestDB(t)
	seedHandlerFKTargets(t, database)
	stores := sqlitestore.New(database)

	teamA := runmode.LocalDefaultTeamID
	teamB := "00000000-0000-0000-0000-0000000000c1"
	if _, err := database.Exec(`INSERT INTO teams (id, org_id, slug, name) VALUES (?, ?, 'team-b-attr', 'Team B Attr')`, teamB, runmode.LocalDefaultOrgID); err != nil {
		t.Fatalf("seed team B: %v", err)
	}
	// Owner team A's bot is DISABLED; team B's is enabled.
	if _, err := database.Exec(`INSERT OR REPLACE INTO team_settings (team_id, auto_delegate_enabled) VALUES (?, 0)`, teamA); err != nil {
		t.Fatalf("disable team A auto-delegate: %v", err)
	}
	if _, err := database.Exec(`INSERT INTO team_settings (team_id, auto_delegate_enabled) VALUES (?, 1)`, teamB); err != nil {
		t.Fatalf("enable team B auto-delegate: %v", err)
	}
	if _, err := database.Exec(`INSERT OR IGNORE INTO agents (id, org_id, display_name) VALUES (?, ?, 'Test Bot')`, runmode.LocalDefaultAgentID, runmode.LocalDefaultOrgID); err != nil {
		t.Fatalf("seed agent: %v", err)
	}
	if err := stores.TeamAgents.AddForTeam(t.Context(), runmode.LocalDefaultOrg, teamA, runmode.LocalDefaultAgentID); err != nil {
		t.Fatalf("add agent to team A: %v", err)
	}
	if err := stores.TeamAgents.AddForTeam(t.Context(), runmode.LocalDefaultOrg, teamB, runmode.LocalDefaultAgentID); err != nil {
		t.Fatalf("add agent to team B: %v", err)
	}

	entity, _, err := stores.Entities.FindOrCreate(context.Background(), runmode.LocalDefaultOrgID, "github", "owner/repo#attr", "pr", "Attr PR", "https://example.com/attr")
	if err != nil {
		t.Fatalf("create entity: %v", err)
	}
	createTestPrompt(t, database, domain.Prompt{ID: "p-attr", Name: "Attr", Body: "x", Source: "user"})
	// Team B's own prompt copy for its trigger (same-team FK, SKY-380).
	insertPromptForTeam(t, database, "p-attr-b", teamB)

	// Team A is the owner via a high-priority rule, and also has a
	// trigger (which is skipped because team A's bot is disabled).
	if _, err := database.Exec(`
		INSERT INTO event_handlers
			(id, org_id, team_id, creator_user_id, kind, event_type,
			 scope_predicate_json, enabled, source, name, default_priority, sort_order, created_at, updated_at)
		VALUES (?, ?, ?, ?, 'rule', ?, NULL, 1, 'user', 'A rule', 0.9, 100, datetime('now'), datetime('now'))
	`, "rule-A-attr", runmode.LocalDefaultOrg, teamA, runmode.LocalDefaultUserID, domain.EventGitHubPRCICheckFailed); err != nil {
		t.Fatalf("seed team A rule: %v", err)
	}
	createTriggerForTestRouting(t, database, domain.EventHandler{
		ID: "trigger-A-attr", Kind: domain.EventHandlerKindTrigger,
		PromptID: "p-attr", TriggerType: domain.TriggerTypeEvent,
		EventType: domain.EventGitHubPRCICheckFailed, BreakerThreshold: intPtr(4),
		MinAutonomySuitability: floatPtr(0), Enabled: true,
	})
	// Team B (lower priority) also has a trigger and IS enabled.
	if _, err := database.Exec(`
		INSERT INTO event_handlers
			(id, org_id, team_id, creator_user_id, kind, event_type,
			 scope_predicate_json, enabled, source, prompt_id, breaker_threshold, min_autonomy_suitability, created_at, updated_at)
		VALUES (?, ?, ?, ?, 'trigger', ?, NULL, 1, 'user', ?, 4, 0, datetime('now'), datetime('now'))
	`, "trigger-B-attr", runmode.LocalDefaultOrg, teamB, runmode.LocalDefaultUserID,
		domain.EventGitHubPRCICheckFailed, "p-attr-b"); err != nil {
		t.Fatalf("seed team B trigger: %v", err)
	}

	meta := events.GitHubPRCICheckFailedMetadata{Author: "aidan", CheckName: "build", Repo: "owner/repo"}
	metaJSON, _ := json.Marshal(meta)

	stub := &stubDelegator{db: database}
	router := NewRouter(testPromptStore(database), testEventHandlerStore(database), stores.Agents, stores.TeamAgents, nil, testTaskStore(database), stores.AgentRuns, stores.Entities, stores.PendingFirings, stores.Events, stores.Orgs, stores.Teams, nil, nil, nil, stub, noopScorer{}, websocket.NewHub())

	router.HandleEvent(domain.Event{
		EventType: domain.EventGitHubPRCICheckFailed, EntityID: &entity.ID,
		DedupKey: "build", MetadataJSON: string(metaJSON), OrgID: runmode.LocalDefaultOrg,
	})

	if stub.calls != 1 {
		t.Fatalf("expected exactly 1 bot run (team B only; team A disabled), got %d", stub.calls)
	}
	// The owner must have been consolidated to team B BEFORE the run was
	// created — the run inherits its team from the task at insert time.
	stub.mu.Lock()
	fired := stub.lastTaskTeamID
	stub.mu.Unlock()
	if fired != teamB {
		t.Errorf("run fired with task owner team %q, want teamB %q (owner must consolidate before the run is created)", fired, teamB)
	}
	active, _ := testTaskStore(database).FindActiveByEntity(t.Context(), runmode.LocalDefaultOrg, entity.ID)
	if len(active) != 1 {
		t.Fatalf("expected 1 task, got %d", len(active))
	}
	if active[0].TeamID != teamB {
		t.Errorf("final owner team_id = %q, want teamB %q", active[0].TeamID, teamB)
	}
	if active[0].ClaimedByAgentID == "" {
		t.Error("task should be bot-claimed by the acting team")
	}
}

// TestHandleEvent_SingleTeam_OneTask is the regression baseline: with
// only one matching rule, exactly one task gets created — the
// local-mode + single-team-rule path.
func TestHandleEvent_SingleTeam_OneTask(t *testing.T) {
	database := newTestDB(t)
	seedHandlerFKTargets(t, database)
	if err := testEventHandlerStore(database).Seed(t.Context(), runmode.LocalDefaultOrg, runmode.LocalDefaultTeamID, seedHandlerFKTargets(t, database)); err != nil {
		t.Fatalf("seed event handlers: %v", err)
	}

	entity, _, err := sqlitestore.New(database).Entities.FindOrCreate(context.Background(), runmode.LocalDefaultOrgID, "github", "owner/repo#single", "pr", "Single team PR", "https://example.com/single")
	if err != nil {
		t.Fatalf("create entity: %v", err)
	}

	meta := events.GitHubPRCICheckFailedMetadata{
		Author:    "aidan",
		CheckName: "build",
		Repo:      "owner/repo",
		HeadSHA:   "abc123",
	}
	metaJSON, _ := json.Marshal(meta)

	router := NewRouter(testPromptStore(database), testEventHandlerStore(database), nil, nil, nil, testTaskStore(database), sqlitestore.New(database).AgentRuns, sqlitestore.New(database).Entities, sqlitestore.New(database).PendingFirings, sqlitestore.New(database).Events, sqlitestore.New(database).Orgs, sqlitestore.New(database).Teams, nil, nil, nil, nil, noopScorer{}, websocket.NewHub())

	router.HandleEvent(domain.Event{
		EventType:    domain.EventGitHubPRCICheckFailed,
		EntityID:     &entity.ID,
		DedupKey:     "build",
		MetadataJSON: string(metaJSON),
		CreatedAt:    time.Now(),
		OrgID:        runmode.LocalDefaultOrg,
	})

	active, err := testTaskStore(database).FindActiveByEntity(t.Context(), runmode.LocalDefaultOrg, entity.ID)
	if err != nil {
		t.Fatalf("list active tasks: %v", err)
	}
	if len(active) != 1 {
		t.Errorf("expected exactly 1 active task with one matching rule, got %d", len(active))
	}
	if len(active) >= 1 && active[0].EventType != domain.EventGitHubPRCICheckFailed {
		t.Errorf("task event_type=%q, want %q", active[0].EventType, domain.EventGitHubPRCICheckFailed)
	}
}
