package curator

import (
	"context"
	"errors"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// A curator turn is an executor engagement on the same claims model, so the
// same fence applies to it. What this pins is the reaction: a refused
// transcript write kills the turn and marks it fenced, and the dispatch loop
// reads that flag before its cancellation branch — otherwise the turn would
// go on to release a claim and revert context rows that belong to the
// successor engagement now driving the conversation.
//
// The refusal is injected rather than staged against a released claim: the
// fence is a Postgres property (asserted in internal/db/postgres) and the
// local store does not refuse anything, which is precisely why the reaction
// needs its own coverage here.

type fencedConversationStore struct {
	db.ConversationStore
}

func (fencedConversationStore) InsertMessageForClaimSystem(context.Context, string, string, *domain.Message) (int64, error) {
	return 0, db.ErrClaimReleased
}

func TestTurnSink_FenceTripKillsTheTurnAndRecordsIt(t *testing.T) {
	killed := false
	c := &Curator{stores: db.Stores{Conversations: fencedConversationStore{}}}
	sink := newTurnSink(c, "proj-1", "conv-1", "req-1", "claim-1", "org-1", "user-1", func() { killed = true })

	err := sink.OnMessage(&domain.Message{Role: "assistant", Content: "hello"})
	if !errors.Is(err, db.ErrClaimReleased) {
		t.Fatalf("OnMessage = %v, want the fence trip surfaced to the runtime", err)
	}
	if !sink.fenceTripped() {
		t.Error("sink did not record the fence trip; the dispatch would release the successor's claim")
	}
	if !killed {
		t.Error("sink did not kill the turn after being fenced out")
	}
}
