package server

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// The conversation body a client reads off GET, list, message, and stop is one
// set of snake_case keys, the conditional ones included.
func TestConversationResponseKeysAreSnakeCase(t *testing.T) {
	at := time.Unix(1, 0).UTC()
	cost, ms, turns, position, step := 0.5, 10, 2, 1, 0
	conv := &domain.Conversation{
		ID:                  "c1",
		TaskID:              "t1",
		PromptID:            "p1",
		Status:              domain.StatusOpen,
		Model:               "m",
		StartedAt:           at,
		QueuedAt:            &at,
		ClaimedAt:           &at,
		CompletedAt:         &at,
		NextAttemptAt:       &at,
		QueuePosition:       &position,
		TotalCostUSD:        &cost,
		DurationMs:          &ms,
		NumTurns:            &turns,
		ParkReason:          domain.ParkReasonUserCancelled,
		EndedAt:             &at,
		EndedReason:         domain.EndedDelegated,
		StopRequestedAt:     &at,
		ClaimLastActivityAt: &at,
		ClaimCurrentOp:      "op",
		WorktreePath:        "/w",
		ResultSummary:       "done",
		Outcome:             "continue",
		OutcomeReason:       "why",
		FailureKind:         "kind",
		SessionID:           "s1",
		MemoryMissing:       true,
		HasRun:              true,
		Type:                "delegation",
		Runtime:             domain.ConversationRuntimeNative,
		TriggerType:         "manual",
		TriggerID:           "tr1",
		ActorAgentID:        "a1",
		ActorAgentName:      "Agent",
		BlueprintRunID:      "br1",
		BlueprintStepIndex:  &step,
	}
	// deliverableCount 0 emits the approval keys, and a non-empty current
	// action emits current_action, so every conditional key is present.
	out := conversationResponse(conv, 0, nil, map[string]int{"pull_request": 1}, 1,
		db.RunProgress{StepCount: 1, Status: domain.BlueprintRunStatusRunning}, "Editing x")
	for _, key := range []string{"current_action", "queue_position", "ended_at", "stop_requested_at", "next_attempt_at", "claim_last_activity_at", "claim_current_op", "has_unresolved_deliverables"} {
		if _, ok := out[key]; !ok {
			t.Fatalf("fixture leaves %q unset, so the body under test is not the full one", key)
		}
	}
	b, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var wire map[string]any
	if err := json.Unmarshal(b, &wire); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for key := range wire {
		if key != strings.ToLower(key) {
			t.Errorf("key %q holds an uppercase letter", key)
		}
	}
}
