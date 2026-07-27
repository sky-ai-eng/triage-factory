package delegate

import (
	"context"
	"fmt"

	"github.com/sky-ai-eng/triage-factory/internal/agentloop"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// nativeTranscript adapts the conversations store to the native loop's
// Transcript surface. It is the runSink of this runtime: every row the loop
// writes lands through here and fans out to the websocket, so a native
// conversation streams to the UI in the same shapes an SDK one does.
//
// Pool routing mirrors runSink exactly — a manual run's writes go through
// synthetic claims so they pass RLS under tf_app, an event run's through the
// admin pool. The loop is a long-lived goroutine, so each write is its own
// short transaction rather than one that spans the engagement.
type nativeTranscript struct {
	spawner       *Spawner
	orgID         string
	conversation  string
	triggerType   string
	creatorUserID string
}

var _ agentloop.Transcript = (*nativeTranscript)(nil)

func newNativeTranscript(s *Spawner, orgID, conversationID, triggerType, creatorUserID string) *nativeTranscript {
	return &nativeTranscript{
		spawner:       s,
		orgID:         orgID,
		conversation:  conversationID,
		triggerType:   triggerType,
		creatorUserID: creatorUserID,
	}
}

// ListForAssembly reads the assembly window. The admin pool is the right
// door for both trigger types: it is a system read on a detached goroutine,
// and org_id + conversation_id are bound as defense in depth regardless.
func (t *nativeTranscript) ListForAssembly(ctx context.Context, orgID, conversationID string) ([]domain.Message, error) {
	return t.spawner.agentRuns.ListForAssemblySystem(ctx, orgID, conversationID)
}

func (t *nativeTranscript) MarkDelivered(ctx context.Context, orgID, conversationID string, ids []int, subtype string) error {
	return t.spawner.agentRuns.MarkDeliveredSystem(ctx, orgID, conversationID, ids, subtype)
}

// Insert appends a row and broadcasts it. Claim attribution is stamped
// server-side by the store (the conversation's active claim), so the loop
// never has to know which claim it is.
func (t *nativeTranscript) Insert(ctx context.Context, orgID string, msg *domain.Message) (int, error) {
	var id int64
	if t.triggerType == "manual" {
		if err := t.spawner.tx.SyntheticClaimsWithTx(ctx, orgID, t.creatorUserID, func(ts db.TxStores) error {
			i, ierr := ts.Conversations.InsertMessage(ctx, orgID, msg)
			if ierr != nil {
				return ierr
			}
			id = i
			return nil
		}); err != nil {
			return 0, fmt.Errorf("insert message: %w", err)
		}
	} else {
		i, ierr := t.spawner.agentRuns.InsertMessageSystem(ctx, orgID, msg)
		if ierr != nil {
			return 0, fmt.Errorf("insert message: %w", ierr)
		}
		id = i
	}
	msg.ID = int(id)
	// An undelivered row is pending input, not transcript: broadcasting it
	// would render a message the model has not been shown yet.
	if msg.Delivered == nil || *msg.Delivered {
		t.spawner.broadcastMessage(orgID, t.conversation, msg)
	}
	return int(id), nil
}

// spendGuard is the native loop's pre-call spend arm. It reuses the
// admission gate's own checks — the same org and team daily caps, read off
// the same messages-backed ledger, with the same fail-open behavior and the
// same user-visible wording — so a run that would have been refused at
// delegation is stopped mid-engagement by the identical rule rather than a
// parallel one that could drift from it.
//
// The difference is what a breach does. At admission it refuses the spawn;
// here it parks the conversation `open` with a notice, because there is
// already work in progress and a conversation is never failed for spending
// money it was allowed to spend.
type spendGuard struct {
	spawner *Spawner
	orgID   string
	teamID  string
}

var _ agentloop.Guard = (*spendGuard)(nil)

func (g *spendGuard) Check(ctx context.Context, _ int) (string, error) {
	if err := g.spawner.checkDailyCostCap(ctx, g.orgID); err != nil {
		return spendParkNotice(err), nil
	}
	if g.teamID != "" {
		if err := g.spawner.checkTeamDailyCostCap(ctx, g.orgID, g.teamID); err != nil {
			return spendParkNotice(err), nil
		}
	}
	return "", nil
}

// spendParkNotice renders the cap error as the transcript notice. The cap
// error already carries the scope and the today-vs-cap figures, which is
// exactly what a reader needs; this adds only what the native mapping
// contributes — that the work is paused, not lost.
func spendParkNotice(err error) string {
	return "<system-note>\nThis run is paused: " + err.Error() + ". " +
		"No work has been lost — it resumes on the next message, or once the cap resets or is raised.\n</system-note>"
}
