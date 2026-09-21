package dbtest

import (
	"context"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/workitem"
)

// PendingFiringsGateSeeder is what RunPendingFiringsGateAgreement needs
// beyond the conformance seeder: a way to stage each conversation shape the
// live-conversation predicate distinguishes.
type PendingFiringsGateSeeder struct {
	PendingFiringsSeeder
	// SubConversation inserts a live conversation on the task whose
	// parent_conversation_id names the given conversation.
	SubConversation func(t *testing.T, taskID, promptID, parentID string) string
	// SetStatus writes a conversation's status directly, "" for NULL.
	SetStatus func(t *testing.T, conversationID, status string)
}

// RunPendingFiringsGateAgreement pins that the kind's claim filter and the
// conversation store's live-conversation read answer the same question:
// for each conversation shape, a ready firing for the task is claimed
// exactly when the store reports no live conversation.
func RunPendingFiringsGateAgreement(t *testing.T, mk func(t *testing.T) (db.PendingFiringsStore, db.ConversationStore, string, PendingFiringsGateSeeder)) {
	t.Helper()
	ctx := context.Background()
	owner := workitem.Owner{ID: "gate-agreement", Epoch: 1}

	cases := []struct {
		name  string
		stage func(t *testing.T, seed PendingFiringsGateSeeder, tup PendingFiringsTuple)
	}{
		{"live_top_level_conversation", func(t *testing.T, seed PendingFiringsGateSeeder, tup PendingFiringsTuple) {
			seed.LiveConversation(t, tup.TaskID, tup.PromptID)
		}},
		{"live_sub_conversation_only", func(t *testing.T, seed PendingFiringsGateSeeder, tup PendingFiringsTuple) {
			parent := seed.LiveConversation(t, tup.TaskID, tup.PromptID)
			seed.EndConversation(t, parent)
			seed.SubConversation(t, tup.TaskID, tup.PromptID, parent)
		}},
		{"completed_conversation", func(t *testing.T, seed PendingFiringsGateSeeder, tup PendingFiringsTuple) {
			seed.SetStatus(t, seed.LiveConversation(t, tup.TaskID, tup.PromptID), "completed")
		}},
		{"open_conversation_not_ended", func(t *testing.T, seed PendingFiringsGateSeeder, tup PendingFiringsTuple) {
			seed.SetStatus(t, seed.LiveConversation(t, tup.TaskID, tup.PromptID), "open")
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, conversations, orgID, seed := mk(t)
			tup := seed.Tuple(t)
			if _, _, err := s.Enqueue(ctx, orgID, tup.EntityID, tup.TaskID, tup.TriggerID, tup.EventID, db.AgentClaimStamp{}); err != nil {
				t.Fatalf("Enqueue: %v", err)
			}
			tc.stage(t, seed, tup)

			liveID, err := conversations.LiveConversationIDForTaskSystem(ctx, orgID, tup.TaskID)
			if err != nil {
				t.Fatalf("LiveConversationIDForTaskSystem: %v", err)
			}
			batch, err := s.Claim(ctx, owner, 10)
			if err != nil {
				t.Fatalf("Claim: %v", err)
			}
			claimed := len(batch.Firings) == 1
			if (liveID != "") == claimed {
				t.Errorf("live conversation %q and claimed=%v: the store's read and the claim filter disagree", liveID, claimed)
			}
		})
	}
}
