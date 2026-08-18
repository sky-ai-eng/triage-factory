package dbtest

import (
	"database/sql"
	"testing"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// SeedConversation inserts a conversations row directly via raw SQLite SQL so
// test fixtures outside internal/db can seed a conversation in any state.
// Production mints conversation rows through ConversationQueueStore.EnqueueConversation (there
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
func SeedConversation(tb testing.TB, database *sql.DB, conv domain.Conversation) {
	tb.Helper()

	orgID := conv.OrgID
	if orgID == "" {
		orgID = runmode.LocalDefaultOrgID
	}
	teamID := conv.TeamID
	if teamID == "" {
		teamID = runmode.LocalDefaultTeamID
	}
	triggerType := conv.TriggerType
	if triggerType == "" {
		triggerType = "manual"
	}
	var creator any
	if triggerType == "manual" {
		if conv.CreatorUserID != "" {
			creator = conv.CreatorUserID
		} else {
			creator = runmode.LocalDefaultUserID
		}
	}
	origin := "interactive"
	if conv.BlueprintRunID != "" {
		origin = "blueprint"
	}
	var status any
	if conv.Status != "" {
		status = conv.Status
	}
	var startedAt any
	if !conv.StartedAt.IsZero() {
		startedAt = conv.StartedAt
	}
	var stepIdx any
	if conv.BlueprintStepIndex != nil {
		stepIdx = *conv.BlueprintStepIndex
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
		conv.ID, orgID, creator, teamID, nullIfEmpty(conv.TaskID),
		nullIfEmpty(conv.PromptID), nullIfEmpty(conv.TriggerID), triggerType,
		origin, status, nullIfEmpty(conv.Model),
		nullIfEmpty(conv.SessionID), nullIfEmpty(conv.WorktreePath),
		nullIfEmpty(conv.ResultSummary), nullIfEmpty(conv.Outcome),
		nullIfEmpty(conv.OutcomeReason), nullIfEmpty(string(conv.FailureKind)),
		nullIfEmpty(string(conv.ParkReason)), startedAt, conv.CompletedAt,
		nullIfEmpty(conv.ActorAgentID), nullIfEmpty(conv.BlueprintRunID),
		stepIdx, nullIfEmpty(conv.TriggeringEventID), conv.QueuedAt,
		nullIfEmpty(conv.PreferredExecutorID)); err != nil {
		tb.Fatalf("SeedConversation %s: %v", conv.ID, err)
	}

	// The fixture's accounting fields translate into the rows the read
	// projections derive them from: cost/tokens become one settled ledger
	// message, duration/turns a released claim's telemetry. A fixture with
	// none of them seeds neither row.
	if conv.TotalCostUSD != nil || conv.InputTokens != 0 || conv.OutputTokens != 0 ||
		conv.CacheReadTokens != 0 || conv.CacheCreationTokens != 0 {
		var cost any
		if conv.TotalCostUSD != nil {
			cost = *conv.TotalCostUSD
		}
		if _, err := database.Exec(`
			INSERT INTO messages (org_id, conversation_id, role, subtype, content, cost_usd,
			                      input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens,
			                      created_at)
			VALUES (?, ?, 'assistant', '', 'seeded work', ?, ?, ?, ?, ?, COALESCE(?, CURRENT_TIMESTAMP))
		`, orgID, conv.ID, cost, conv.InputTokens, conv.OutputTokens,
			conv.CacheReadTokens, conv.CacheCreationTokens, startedAt); err != nil {
			tb.Fatalf("SeedConversation %s ledger row: %v", conv.ID, err)
		}
	}
	if conv.DurationMs != nil || conv.NumTurns != nil {
		var duration, turns any
		if conv.DurationMs != nil {
			duration = *conv.DurationMs
		}
		if conv.NumTurns != nil {
			turns = *conv.NumTurns
		}
		if _, err := database.Exec(`
			INSERT INTO claims (id, org_id, conversation_id, executor_id, boot_epoch,
			                    claimed_at, released_at, outcome, duration_ms, num_turns)
			VALUES (?, ?, ?, 'seed-exec', 0, COALESCE(?, CURRENT_TIMESTAMP),
			        COALESCE(?, CURRENT_TIMESTAMP), 'completed', ?, ?)
		`, uuid.New().String(), orgID, conv.ID, startedAt, startedAt, duration, turns); err != nil {
			tb.Fatalf("SeedConversation %s telemetry claim: %v", conv.ID, err)
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
