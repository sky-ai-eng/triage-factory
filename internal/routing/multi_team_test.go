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
// reversal AND the author-centric visibility rule: a CI failure (author-
// centric) matching explicit user-authored rules on N teams creates ONE
// task whose visibility set is those N teams. The author here isn't a TF
// user (no identity seeded) so the owning-team ladder resolves no single
// owner — team_id stays NULL (unresolved), while the explicit watch rules
// still surface the task in both teams' queues.
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

	// Two applies_to_unowned ("watch") rules on the same event, one per team.
	// The author isn't a TF user, so visibility comes solely from the flag
	// (TFAC-517) — both teams' watch rules surface the single task. Without the
	// flag (the default) an external author would mint no task at all.
	for _, teamID := range []string{teamA, teamB} {
		ruleID := uuid.New().String()
		if _, err := database.Exec(`
			INSERT INTO event_handlers
				(id, org_id, team_id, creator_user_id, kind, event_type,
				 scope_predicate_json, enabled, source, applies_to_unowned,
				 name, default_priority, sort_order,
				 created_at, updated_at)
			VALUES (?, ?, ?, ?, 'rule', ?,
			        NULL, 1, 'user', 1,
			        ?, 0.7, 100,
			        ?, ?)
		`, ruleID, runmode.LocalDefaultOrgID, teamID, runmode.LocalDefaultUserID, domain.EventGitHubPRCICheckFailed,
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

	router := NewRouter(testPromptStore(database), testBlueprintStore(database), testEventHandlerStore(database), nil, nil, nil, testTaskStore(database), sqlitestore.New(database).Conversations, sqlitestore.New(database).Entities, sqlitestore.New(database).PendingFirings, sqlitestore.New(database).Events, sqlitestore.New(database).Orgs, sqlitestore.New(database).Teams, nil, nil, nil, nil, noopScorer{}, websocket.NewHub())

	router.HandleEvent(domain.Event{
		EventType:    domain.EventGitHubPRCICheckFailed,
		EntityID:     &entity.ID,
		DedupKey:     "build",
		MetadataJSON: string(metaJSON),
		CreatedAt:    time.Now(),
		OrgID:        runmode.LocalDefaultOrgID,
	})

	active, err := testTaskStore(database).FindActiveByEntity(t.Context(), runmode.LocalDefaultOrgID, entity.ID)
	if err != nil {
		t.Fatalf("list active tasks: %v", err)
	}
	if len(active) != 1 {
		t.Fatalf("expected exactly 1 task for the situation (team is visibility, not count), got %d", len(active))
	}

	// Owner is unresolved (NULL): author-centric routing found no single
	// owning team for a non-TF author, and explicit watch rules grant
	// visibility but never ownership. NULL is the auto-fire gate — the bot
	// never claims an unowned task.
	if active[0].TeamID != nil {
		t.Errorf("owner team_id = %v, want nil (unresolved owner for a non-TF author)", *active[0].TeamID)
	}

	// Both teams are in the visibility set.
	vis, err := testTaskStore(database).VisibilityTeams(t.Context(), runmode.LocalDefaultOrgID, active[0].ID)
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
	if err := testEventHandlerStore(database).Seed(t.Context(), runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, seedHandlerFKTargets(t, database)); err != nil {
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

	router := NewRouter(testPromptStore(database), testBlueprintStore(database), testEventHandlerStore(database), nil, nil, nil, testTaskStore(database), sqlitestore.New(database).Conversations, sqlitestore.New(database).Entities, sqlitestore.New(database).PendingFirings, sqlitestore.New(database).Events, sqlitestore.New(database).Orgs, sqlitestore.New(database).Teams, nil, nil, nil, nil, noopScorer{}, websocket.NewHub())

	router.HandleEvent(domain.Event{
		EventType:    domain.EventGitHubPRReviewRequested,
		EntityID:     &entity.ID,
		MetadataJSON: string(metaJSON),
		OccurredAt:   occurred,
		OrgID:        runmode.LocalDefaultOrgID,
	})

	active, err := testTaskStore(database).FindActiveByEntity(t.Context(), runmode.LocalDefaultOrgID, entity.ID)
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
	if err := testEventHandlerStore(database).Seed(t.Context(), runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, seedHandlerFKTargets(t, database)); err != nil {
		t.Fatalf("seed event handlers: %v", err)
	}
	setReviewHost(t, database)
	seedUserOnTeam(t, database, runmode.LocalDefaultTeamID, "aidan")

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
	router := reviewRouter(database)
	router.HandleEvent(domain.Event{
		EventType:    domain.EventGitHubPRCICheckFailed,
		EntityID:     &entity.ID,
		DedupKey:     "build",
		MetadataJSON: string(metaJSON),
		// OccurredAt deliberately zero.
		OrgID: runmode.LocalDefaultOrgID,
	})

	active, err := testTaskStore(database).FindActiveByEntity(t.Context(), runmode.LocalDefaultOrgID, entity.ID)
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
	priorEventID, err := sqlitestore.New(database).Events.Record(context.Background(), runmode.LocalDefaultOrgID, domain.Event{
		EventType:    domain.EventJiraIssueAssigned,
		EntityID:     &entity.ID,
		MetadataJSON: `{}`,
	})
	if err != nil {
		t.Fatalf("record prior event: %v", err)
	}
	if _, _, err := testTaskStore(database).FindOrCreate(t.Context(), runmode.LocalDefaultOrgID, teamA, entity.ID, domain.EventJiraIssueAssigned, "", priorEventID, 0.5); err != nil {
		t.Fatalf("create prior assigned task: %v", err)
	}

	// Both teams have an applies_to_unowned ("watch") rule matching became_atomic.
	// The assignee isn't a TF user here, so without the flag the assignee-centric
	// ladder would already mint no task — the flag makes routing surface a task so
	// this test actually exercises the became_atomic suppression (not a no-owner
	// early return).
	for _, teamID := range []string{teamA, teamB} {
		ruleID := uuid.New().String()
		if _, err := database.Exec(`
			INSERT INTO event_handlers
				(id, org_id, team_id, creator_user_id, kind, event_type,
				 scope_predicate_json, enabled, source, applies_to_unowned,
				 name, default_priority, sort_order,
				 created_at, updated_at)
			VALUES (?, ?, ?, ?, 'rule', ?,
			        NULL, 1, 'user', 1,
			        ?, 0.7, 100, ?, ?)
		`, ruleID, runmode.LocalDefaultOrgID, teamID, runmode.LocalDefaultUserID, domain.EventJiraIssueBecameAtomic,
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

	router := NewRouter(testPromptStore(database), testBlueprintStore(database), testEventHandlerStore(database), nil, nil, nil, testTaskStore(database), sqlitestore.New(database).Conversations, sqlitestore.New(database).Entities, sqlitestore.New(database).PendingFirings, sqlitestore.New(database).Events, sqlitestore.New(database).Orgs, sqlitestore.New(database).Teams, nil, nil, nil, nil, noopScorer{}, websocket.NewHub())
	router.HandleEvent(domain.Event{
		EventType:    domain.EventJiraIssueBecameAtomic,
		EntityID:     &entity.ID,
		MetadataJSON: string(atomicJSON),
		OrgID:        runmode.LocalDefaultOrgID,
	})

	active, err := testTaskStore(database).FindActiveByEntity(t.Context(), runmode.LocalDefaultOrgID, entity.ID)
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
	if err := stores.TeamAgents.AddForTeam(t.Context(), runmode.LocalDefaultOrgID, teamA, runmode.LocalDefaultAgentID); err != nil {
		t.Fatalf("add agent to team A: %v", err)
	}
	if err := stores.TeamAgents.AddForTeam(t.Context(), runmode.LocalDefaultOrgID, teamB, runmode.LocalDefaultAgentID); err != nil {
		t.Fatalf("add agent to team B: %v", err)
	}
	if err := stores.TeamAgents.SetEnabled(t.Context(), runmode.LocalDefaultOrgID, teamB, runmode.LocalDefaultAgentID, false); err != nil {
		t.Fatalf("disable agent for team B: %v", err)
	}

	// One entity, one event, ONE task — shared across both teams.
	entity, _, err := stores.Entities.FindOrCreate(context.Background(), runmode.LocalDefaultOrgID, "github", "owner/repo#gate", "pr", "Gate PR", "https://example.com/gate")
	if err != nil {
		t.Fatalf("create entity: %v", err)
	}
	createTestPrompt(t, database, domain.Prompt{ID: "p-gate", Name: "Gate", Body: "x", Source: "user"})
	eventID, err := sqlitestore.New(database).Events.Record(context.Background(), runmode.LocalDefaultOrgID, domain.Event{
		EventType:    domain.EventGitHubPRCICheckFailed,
		EntityID:     &entity.ID,
		DedupKey:     "build",
		MetadataJSON: `{"check_name":"build"}`,
	})
	if err != nil {
		t.Fatalf("record event: %v", err)
	}
	task, _, err := testTaskStore(database).FindOrCreate(t.Context(), runmode.LocalDefaultOrgID, teamA, entity.ID, domain.EventGitHubPRCICheckFailed, "build", eventID, 0.5)
	if err != nil {
		t.Fatalf("create task: %v", err)
	}

	trigger := domain.EventHandler{
		ID:                     "trigger-gate",
		Kind:                   domain.EventHandlerKindTrigger,
		BlueprintID:            "p-gate",
		TriggerType:            domain.TriggerTypeEvent,
		EventType:              domain.EventGitHubPRCICheckFailed,
		BreakerThreshold:       intPtr(4),
		MinAutonomySuitability: floatPtr(0),
		Enabled:                true,
	}
	createTriggerForTestRouting(t, database, trigger)
	// createTriggerForTestRouting remaps BlueprintID to the wrapping blueprint's
	// id in the DB, but the local struct still holds the prompt id. This test
	// calls tryAutoDelegate directly (bypassing HandleEvent's re-read from the
	// handler store), so resolve the real blueprint id onto the local trigger —
	// the spawner mints a blueprint_run whose blueprint_id FK needs it.
	if err := database.QueryRow(`SELECT blueprint_id FROM event_handlers WHERE id = ?`, "trigger-gate").Scan(&trigger.BlueprintID); err != nil {
		t.Fatalf("resolve trigger blueprint id: %v", err)
	}

	stub := &stubDelegator{db: database}
	router := NewRouter(testPromptStore(database), testBlueprintStore(database), testEventHandlerStore(database), stores.Agents, stores.TeamAgents, nil, testTaskStore(database), stores.Conversations, stores.Entities, stores.PendingFirings, stores.Events, stores.Orgs, stores.Teams, nil, nil, nil, stub, noopScorer{}, websocket.NewHub())

	// Fire the trigger as team B (bot disabled) — must be blocked — and
	// as team A (bot enabled) — must delegate. Order doesn't matter:
	// team B's gate returns before the entity gate.
	router.tryAutoDelegate(runmode.LocalDefaultOrgID, task, trigger, entity.ID, eventID, teamB)
	router.tryAutoDelegate(runmode.LocalDefaultOrgID, task, trigger, entity.ID, eventID, teamA)

	if stub.calls != 1 {
		t.Fatalf("expected exactly 1 Delegate call (team A only); got %d", stub.calls)
	}
	got, _ := testTaskStore(database).Get(t.Context(), runmode.LocalDefaultOrgID, task.ID)
	if got.ClaimedByAgentID == "" {
		t.Errorf("task: ClaimedByAgentID empty; expected agent claim after team A's successful fire")
	}
	// The claim consolidated the owning team to the acting (firing)
	// team A.
	if teamIDValue(got) != teamA {
		t.Errorf("owner team_id = %q after team A claim, want %q", teamIDValue(got), teamA)
	}
	// Resolve-once invariant: the actor frozen on the fired blueprint_run is the
	// SAME agent the task claim got stamped with. The router resolves the agent a
	// single time and feeds both the run (via DelegateOpts → blueprint_runs.actor_agent_id)
	// and the claim, so they can't drift — no second lookup, no transaction needed.
	var brActor string
	if err := database.QueryRow(`SELECT COALESCE(actor_agent_id, '') FROM blueprint_runs WHERE task_id = ?`, task.ID).Scan(&brActor); err != nil {
		t.Fatalf("read blueprint_run actor: %v", err)
	}
	if brActor == "" || brActor != got.ClaimedByAgentID {
		t.Errorf("blueprint_run actor_agent_id = %q, want it to equal the task claim %q (resolve-once)", brActor, got.ClaimedByAgentID)
	}
}

// TestHandleEvent_MultipleTeams_OneBotRun pins the exclusive-claim
// contention fix on the default (handler-team) routing path — a Jira
// issue:available (the unassigned team-pool signal), where multiple teams'
// rules legitimately fan to one shared task. When two teams both have
// auto-delegation fully enabled and an immediate trigger on the same event,
// the single shared task gets exactly ONE bot run (the first team in
// priority/id order wins the claim and becomes the owner), and the losing
// team hits the claim guard rather than leaving a queued firing that would
// drain into a duplicate run.
// (Author-centric github events and assignee-centric jira events route
// owner-only, so the cross-team contention lives on the handler-team path —
// jira:issue:available being the canonical multi-team event there.)
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
	if err := stores.TeamAgents.AddForTeam(t.Context(), runmode.LocalDefaultOrgID, teamA, runmode.LocalDefaultAgentID); err != nil {
		t.Fatalf("add agent to team A: %v", err)
	}
	if err := stores.TeamAgents.AddForTeam(t.Context(), runmode.LocalDefaultOrgID, teamB, runmode.LocalDefaultAgentID); err != nil {
		t.Fatalf("add agent to team B: %v", err)
	}

	entity, _, err := stores.Entities.FindOrCreate(context.Background(), runmode.LocalDefaultOrgID, "jira", "SKY-onerun", "issue", "One-run issue", "https://example.com/onerun")
	if err != nil {
		t.Fatalf("create entity: %v", err)
	}
	createTestPrompt(t, database, domain.Prompt{ID: "p-onerun", Name: "One-run", Body: "x", Source: "user"})
	// Team B needs its OWN prompt copy — the same-team trigger→prompt FK
	// forbids team B's trigger from binding team A's p-onerun.
	insertPromptForTeam(t, database, "p-onerun-b", teamB)

	// teamA's immediate trigger (createTriggerForTestRouting hard-codes
	// LocalDefaultTeamID = teamA). jira:issue:available is the unassigned
	// team-pool signal — it stays on the default handler-team routing path
	// (not the assignee ladder), so two teams genuinely contend for it.
	createTriggerForTestRouting(t, database, domain.EventHandler{
		ID: "trigger-A-onerun", Kind: domain.EventHandlerKindTrigger,
		BlueprintID: "p-onerun", TriggerType: domain.TriggerTypeEvent,
		EventType: domain.EventJiraIssueAvailable, BreakerThreshold: intPtr(4),
		MinAutonomySuitability: floatPtr(0), Enabled: true,
	})
	// teamB's immediate trigger (raw insert for the second team), bound to
	// team B's own blueprint copy wrapping its own prompt.
	bpOnerunB := insertBlueprintForTeam(t, database, "bp-onerun-b", "p-onerun-b", teamB)
	if _, err := database.Exec(`
		INSERT INTO event_handlers
			(id, org_id, team_id, creator_user_id, kind, event_type,
			 scope_predicate_json, enabled, source,
			 blueprint_id, breaker_threshold, min_autonomy_suitability,
			 created_at, updated_at)
		VALUES (?, ?, ?, ?, 'trigger', ?, NULL, 1, 'user', ?, 4, 0, datetime('now'), datetime('now'))
	`, "trigger-B-onerun", runmode.LocalDefaultOrgID, teamB, runmode.LocalDefaultUserID,
		domain.EventJiraIssueAvailable, bpOnerunB); err != nil {
		t.Fatalf("seed team B trigger: %v", err)
	}

	meta := events.JiraIssueAvailableMetadata{IssueKey: "SKY-onerun", Project: "SKY", Status: "To Do"}
	metaJSON, _ := json.Marshal(meta)

	stub := &stubDelegator{db: database}
	router := NewRouter(testPromptStore(database), testBlueprintStore(database), testEventHandlerStore(database), stores.Agents, stores.TeamAgents, nil, testTaskStore(database), stores.Conversations, stores.Entities, stores.PendingFirings, stores.Events, stores.Orgs, stores.Teams, nil, nil, nil, stub, noopScorer{}, websocket.NewHub())

	router.HandleEvent(domain.Event{
		EventType: domain.EventJiraIssueAvailable, EntityID: &entity.ID,
		MetadataJSON: string(metaJSON), OrgID: runmode.LocalDefaultOrgID,
	})

	active, err := testTaskStore(database).FindActiveByEntity(t.Context(), runmode.LocalDefaultOrgID, entity.ID)
	if err != nil || len(active) != 1 {
		t.Fatalf("expected 1 task, got %d (err=%v)", len(active), err)
	}
	if stub.calls != 1 {
		t.Errorf("expected exactly 1 bot run (exclusive claim), got %d", stub.calls)
	}
	if active[0].ClaimedByAgentID == "" {
		t.Error("task should be bot-claimed after the winning team fired")
	}
	if teamIDValue(&active[0]) != teamA {
		t.Errorf("owner team_id = %q, want teamA %q (first in priority/id order wins the claim)", teamIDValue(&active[0]), teamA)
	}
	// The losing team (B) must not have left a queued firing that would
	// later drain into a duplicate run.
	firings, err := stores.PendingFirings.ListForEntity(t.Context(), runmode.LocalDefaultOrgID, entity.ID)
	if err != nil {
		t.Fatalf("list firings: %v", err)
	}
	if len(firings) != 0 {
		t.Errorf("team B left %d queued firing(s) on the bot-claimed task; want 0 (would drain into a duplicate run)", len(firings))
	}
}

// TestHandleEvent_OwnerDisabled_RunAttributedToActingTeam pins, on the
// default (handler-team) routing path via a Jira issue:available (the
// unassigned team-pool signal), that when the highest-priority owner team has
// auto-delegation disabled and a lower-priority team fires the bot, the owner
// is consolidated to the acting team BEFORE the run is created — so the run
// (which inherits runs.team_id from tasks.team_id) is attributed to the team
// that acted, not the stale owner. The stub records the task's owner team at
// Delegate time.
// (Author-centric github events and assignee-centric jira events route
// owner-only, so this cross-team consolidation lives on the handler-team
// path — jira:issue:available being the canonical multi-team event there.)
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
	if err := stores.TeamAgents.AddForTeam(t.Context(), runmode.LocalDefaultOrgID, teamA, runmode.LocalDefaultAgentID); err != nil {
		t.Fatalf("add agent to team A: %v", err)
	}
	if err := stores.TeamAgents.AddForTeam(t.Context(), runmode.LocalDefaultOrgID, teamB, runmode.LocalDefaultAgentID); err != nil {
		t.Fatalf("add agent to team B: %v", err)
	}

	entity, _, err := stores.Entities.FindOrCreate(context.Background(), runmode.LocalDefaultOrgID, "jira", "SKY-attr", "issue", "Attr issue", "https://example.com/attr")
	if err != nil {
		t.Fatalf("create entity: %v", err)
	}
	createTestPrompt(t, database, domain.Prompt{ID: "p-attr", Name: "Attr", Body: "x", Source: "user"})
	// Team B's own prompt copy for its trigger (same-team FK).
	insertPromptForTeam(t, database, "p-attr-b", teamB)

	// Team A is the owner via a high-priority rule, and also has a
	// trigger (which is skipped because team A's bot is disabled).
	if _, err := database.Exec(`
		INSERT INTO event_handlers
			(id, org_id, team_id, creator_user_id, kind, event_type,
			 scope_predicate_json, enabled, source, name, default_priority, sort_order, created_at, updated_at)
		VALUES (?, ?, ?, ?, 'rule', ?, NULL, 1, 'user', 'A rule', 0.9, 100, datetime('now'), datetime('now'))
	`, "rule-A-attr", runmode.LocalDefaultOrgID, teamA, runmode.LocalDefaultUserID, domain.EventJiraIssueAvailable); err != nil {
		t.Fatalf("seed team A rule: %v", err)
	}
	createTriggerForTestRouting(t, database, domain.EventHandler{
		ID: "trigger-A-attr", Kind: domain.EventHandlerKindTrigger,
		BlueprintID: "p-attr", TriggerType: domain.TriggerTypeEvent,
		EventType: domain.EventJiraIssueAvailable, BreakerThreshold: intPtr(4),
		MinAutonomySuitability: floatPtr(0), Enabled: true,
	})
	// Team B (lower priority) also has a trigger and IS enabled.
	bpAttrB := insertBlueprintForTeam(t, database, "bp-attr-b", "p-attr-b", teamB)
	if _, err := database.Exec(`
		INSERT INTO event_handlers
			(id, org_id, team_id, creator_user_id, kind, event_type,
			 scope_predicate_json, enabled, source, blueprint_id, breaker_threshold, min_autonomy_suitability, created_at, updated_at)
		VALUES (?, ?, ?, ?, 'trigger', ?, NULL, 1, 'user', ?, 4, 0, datetime('now'), datetime('now'))
	`, "trigger-B-attr", runmode.LocalDefaultOrgID, teamB, runmode.LocalDefaultUserID,
		domain.EventJiraIssueAvailable, bpAttrB); err != nil {
		t.Fatalf("seed team B trigger: %v", err)
	}

	meta := events.JiraIssueAvailableMetadata{IssueKey: "SKY-attr", Project: "SKY", Status: "To Do"}
	metaJSON, _ := json.Marshal(meta)

	stub := &stubDelegator{db: database}
	router := NewRouter(testPromptStore(database), testBlueprintStore(database), testEventHandlerStore(database), stores.Agents, stores.TeamAgents, nil, testTaskStore(database), stores.Conversations, stores.Entities, stores.PendingFirings, stores.Events, stores.Orgs, stores.Teams, nil, nil, nil, stub, noopScorer{}, websocket.NewHub())

	router.HandleEvent(domain.Event{
		EventType: domain.EventJiraIssueAvailable, EntityID: &entity.ID,
		MetadataJSON: string(metaJSON), OrgID: runmode.LocalDefaultOrgID,
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
	active, _ := testTaskStore(database).FindActiveByEntity(t.Context(), runmode.LocalDefaultOrgID, entity.ID)
	if len(active) != 1 {
		t.Fatalf("expected 1 task, got %d", len(active))
	}
	if teamIDValue(&active[0]) != teamB {
		t.Errorf("final owner team_id = %q, want teamB %q", teamIDValue(&active[0]), teamB)
	}
	if active[0].ClaimedByAgentID == "" {
		t.Error("task should be bot-claimed by the acting team")
	}
}

// TestHandleEvent_SingleTeam_OneTask is the local N=1 happy path (the
// repo-wide-poll fix's positive case): a CI failure on the local user's own
// PR — author resolves to the one team via the owning-team ladder — creates
// exactly one task owned by that team, off the shipped (system) CI rule.
func TestHandleEvent_SingleTeam_OneTask(t *testing.T) {
	database := newTestDB(t)
	seedHandlerFKTargets(t, database)
	if err := testEventHandlerStore(database).Seed(t.Context(), runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, seedHandlerFKTargets(t, database)); err != nil {
		t.Fatalf("seed event handlers: %v", err)
	}
	// The local author is bound to the one team, so author-centric routing
	// resolves the owner to it.
	setReviewHost(t, database)
	seedUserOnTeam(t, database, runmode.LocalDefaultTeamID, "aidan")

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

	router := reviewRouter(database)

	router.HandleEvent(domain.Event{
		EventType:    domain.EventGitHubPRCICheckFailed,
		EntityID:     &entity.ID,
		DedupKey:     "build",
		MetadataJSON: string(metaJSON),
		CreatedAt:    time.Now(),
		OrgID:        runmode.LocalDefaultOrgID,
	})

	active, err := testTaskStore(database).FindActiveByEntity(t.Context(), runmode.LocalDefaultOrgID, entity.ID)
	if err != nil {
		t.Fatalf("list active tasks: %v", err)
	}
	if len(active) != 1 {
		t.Fatalf("expected exactly 1 active task with one matching rule, got %d", len(active))
	}
	if active[0].EventType != domain.EventGitHubPRCICheckFailed {
		t.Errorf("task event_type=%q, want %q", active[0].EventType, domain.EventGitHubPRCICheckFailed)
	}
	if teamIDValue(&active[0]) != runmode.LocalDefaultTeamID {
		t.Errorf("owner team_id=%q, want the one team %q", teamIDValue(&active[0]), runmode.LocalDefaultTeamID)
	}
}
