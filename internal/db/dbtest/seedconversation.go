package dbtest

import (
	"database/sql"
	"testing"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// SeedConversation inserts a conversations row directly via raw SQLite SQL so
// test fixtures outside internal/db can seed an agent run in any state.
// Production mints conversation rows through RunQueueStore.EnqueueRun (there
// is no store-level Create), but most tests only need a row to hang
// messages / artifacts / claims off — this helper is that fixture door.
//
// The column list mirrors the conversations DDL. Constraint-driven defaults:
// type is always 'delegation'; a manual trigger gets the local sentinel
// creator when none is supplied while an event trigger forces NULL (the
// creator/trigger_type CHECK); origin is 'blueprint' only when the fixture
// carries a blueprint_run_id (the origin CHECK requires the blueprint
// parents), 'interactive' otherwise; status falls back to NULL — the
// mid-flight state, which is what an unconcluded conversation carries now
// that "queued" and "running" are derived from the claim table rather than
// stored (see SeedActiveClaim).
func SeedConversation(tb testing.TB, database *sql.DB, run domain.Conversation) {
	tb.Helper()

	orgID := run.OrgID
	if orgID == "" {
		orgID = runmode.LocalDefaultOrgID
	}
	teamID := run.TeamID
	if teamID == "" {
		teamID = runmode.LocalDefaultTeamID
	}
	triggerType := run.TriggerType
	if triggerType == "" {
		triggerType = "manual"
	}
	var creator any
	if triggerType == "manual" {
		if run.CreatorUserID != "" {
			creator = run.CreatorUserID
		} else {
			creator = runmode.LocalDefaultUserID
		}
	}
	origin := "interactive"
	if run.BlueprintRunID != "" {
		origin = "blueprint"
	}
	var status any
	if run.Status != "" {
		status = run.Status
	}
	var startedAt any
	if !run.StartedAt.IsZero() {
		startedAt = run.StartedAt
	}
	var stepIdx any
	if run.BlueprintStepIndex != nil {
		stepIdx = *run.BlueprintStepIndex
	}

	if _, err := database.Exec(`
		INSERT INTO conversations (
			id, org_id, type, creator_user_id, team_id, visibility, task_id,
			prompt_id, trigger_id, trigger_type, origin, runtime, status, model,
			sdk_session_id, worktree_path, result_summary, outcome,
			outcome_reason, failure_kind, park_reason, started_at, completed_at,
			parked_at,
			actor_agent_id, blueprint_run_id, blueprint_step_index,
			triggering_event_id, queued_at, preferred_executor_id)
		VALUES (?, ?, 'delegation', ?, ?, 'team', ?, ?, ?, ?, ?, 'sdk', ?, ?,
		        ?, ?, ?, ?, ?, ?, ?, COALESCE(?, CURRENT_TIMESTAMP), ?, NULL,
		        ?, ?, ?, ?, ?, ?)
	`,
		run.ID, orgID, creator, teamID, nullIfEmpty(run.TaskID),
		nullIfEmpty(run.PromptID), nullIfEmpty(run.TriggerID), triggerType,
		origin, status, nullIfEmpty(run.Model),
		nullIfEmpty(run.SessionID), nullIfEmpty(run.WorktreePath),
		nullIfEmpty(run.ResultSummary), nullIfEmpty(run.Outcome),
		nullIfEmpty(run.OutcomeReason), nullIfEmpty(string(run.FailureKind)),
		nullIfEmpty(string(run.ParkReason)), startedAt, run.CompletedAt,
		nullIfEmpty(run.ActorAgentID), nullIfEmpty(run.BlueprintRunID),
		stepIdx, nullIfEmpty(run.TriggeringEventID), run.QueuedAt,
		nullIfEmpty(run.PreferredExecutorID)); err != nil {
		tb.Fatalf("SeedConversation %s: %v", run.ID, err)
	}

	// The fixture's accounting fields translate into the rows the read
	// projections derive them from: cost/tokens become one settled ledger
	// message, duration/turns a released claim's telemetry. A fixture with
	// none of them seeds neither row.
	if run.TotalCostUSD != nil || run.InputTokens != 0 || run.OutputTokens != 0 ||
		run.CacheReadTokens != 0 || run.CacheCreationTokens != 0 {
		var cost any
		if run.TotalCostUSD != nil {
			cost = *run.TotalCostUSD
		}
		if _, err := database.Exec(`
			INSERT INTO messages (org_id, conversation_id, role, subtype, content, cost_usd,
			                      input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens,
			                      created_at)
			VALUES (?, ?, 'assistant', '', 'seeded work', ?, ?, ?, ?, ?, COALESCE(?, CURRENT_TIMESTAMP))
		`, orgID, run.ID, cost, run.InputTokens, run.OutputTokens,
			run.CacheReadTokens, run.CacheCreationTokens, startedAt); err != nil {
			tb.Fatalf("SeedConversation %s ledger row: %v", run.ID, err)
		}
	}
	if run.DurationMs != nil || run.NumTurns != nil {
		var duration, turns any
		if run.DurationMs != nil {
			duration = *run.DurationMs
		}
		if run.NumTurns != nil {
			turns = *run.NumTurns
		}
		if _, err := database.Exec(`
			INSERT INTO claims (id, org_id, conversation_id, executor_id, boot_epoch,
			                    claimed_at, released_at, outcome, duration_ms, num_turns)
			VALUES (?, ?, ?, 'seed-exec', 0, COALESCE(?, CURRENT_TIMESTAMP),
			        COALESCE(?, CURRENT_TIMESTAMP), 'completed', ?, ?)
		`, uuid.New().String(), orgID, run.ID, startedAt, startedAt, duration, turns); err != nil {
			tb.Fatalf("SeedConversation %s telemetry claim: %v", run.ID, err)
		}
	}
}

// SeedActiveClaim inserts a live claim (released_at NULL) for the given
// conversation and returns the claim id. Execution ownership moved off the
// conversation row onto claims, so a fixture that used to stamp
// executor_id/boot_epoch/claimed_at columns seeds one of these instead. The
// partial unique index allows at most one active claim per conversation —
// callers seeding a second engagement must release the first.
func SeedActiveClaim(tb testing.TB, database *sql.DB, conversationID, executorID string, bootEpoch int64) string {
	tb.Helper()
	id := uuid.New().String()
	if _, err := database.Exec(`
		INSERT INTO claims (id, org_id, conversation_id, executor_id, boot_epoch)
		VALUES (?, ?, ?, ?, ?)
	`, id, runmode.LocalDefaultOrgID, conversationID, executorID, bootEpoch); err != nil {
		tb.Fatalf("SeedActiveClaim %s: %v", conversationID, err)
	}
	return id
}

// nullIfEmpty maps "" to SQL NULL for nullable / FK columns, mirroring the
// store implementations' convention.
func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
