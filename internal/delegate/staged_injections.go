// Generic staged-injection queue, generalized from the deliverable-change
// feedback. A note is persisted to the durable staged-injection queue
// (db.StagedInjectionStore, backed by undelivered messages rows) as the bare
// injection, under the subtype that says what it is for:
// injection:event for a new-event injection, injection:system-note for every
// other note. A native loop reads the rows itself before its next model call.
// An SDK conversation has them flushed — bundled into one <system-note> with
// its first message — when its next engagement starts
// (stagedInjectionsForEngagement, read by a resume's prepends and a launch's
// first turn); a new-event injection for a live SDK process is steered in
// instead (deliverInjectionLive).
//
// Unlike the deliverable-change ledger (which DERIVES its terminal injections from
// the deliverable rows, so it needs no queue), a producer here has no durable row
// of its own to re-derive from — the PR coherence feed is such a producer.
// That's why this half is a real durable queue, not a derivation.

package delegate

import (
	"context"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// stageOrDeliverInjection routes one agent-facing note for an SDK
// conversation by its live state. It is the local arm
// StageOrDeliverAdditiveEvent falls back on for an SDK conversation:
//
//   - live (a warm process): the note is wrapped and steered in immediately,
//     fire-and-forget; delivered=true.
//   - not live (parked / terminal-resumable / terminal): the bare body is
//     staged under subtype for the next resume to flush; staged=true.
//
// producer tags the row for audit (domain.StagedInjectionProducer*), prov
// records the event the note was composed from, and body is the bare,
// already-rendered line — the live path wraps one note, the flush bundles and
// wraps the block. A blank body is a no-op.
//
// Never flips conversation status or wakes a terminal run: a note for a
// terminal-but-resumable conversation waits in the queue until the user's
// next follow-up, and one for a conversation that can never resume simply
// never reaches the agent. The caller gates that.
//
// stagedID is the durable row's id when staged=true (empty otherwise), which
// the additive gate needs in order to delete the row when its post-stage
// recheck decides against delivery.
//
// delivered=false with staged=false is a drop: the live gate raced a closing
// process and the durable append then failed (see stageInjection).
func (s *Spawner) stageOrDeliverInjection(ctx context.Context, orgID, conversationID, subtype, producer, body string, prov domain.NoteProvenance) (delivered, staged bool, stagedID string) {
	if body == "" {
		return false, false, ""
	}
	if s.getProc(conversationID) != nil {
		s.deliverInjectionLive(orgID, conversationID, domain.WrapSystemNote(body), producer, prov)
		return true, false, ""
	}
	stagedID, staged = s.stageInjection(ctx, orgID, conversationID, subtype, producer, body, prov)
	return false, staged, stagedID
}

// stageInjection appends one note to the durable queue under subtype and
// returns the row's id. ok=false is a drop: no store is wired, or the append
// failed. Nothing re-delivers it, so a producer whose news does not recur
// must not treat a drop as tolerable — the coherence feed can, because its
// next transition carries a fresh note.
func (s *Spawner) stageInjection(ctx context.Context, orgID, conversationID, subtype, producer, body string, prov domain.NoteProvenance) (stagedID string, ok bool) {
	if s.stagedInjections == nil {
		delegateLog.Warn("stage injection dropped: no staged-injection store wired", "conversation", conversationID, "producer", producer)
		return "", false
	}
	stored, err := s.stagedInjections.AppendSystem(ctx, orgID, domain.StagedInjection{
		ConversationID: conversationID,
		Subtype:        subtype,
		Producer:       producer,
		Body:           body,
		Provenance:     prov,
	})
	if err != nil {
		delegateLog.Warn("stage injection: append failed (dropped for good; only a later transition from a self-renewing producer might stage fresh news)", "conversation", conversationID, "producer", producer, "error", err)
		return "", false
	}
	return stored.ID, true
}

// tellHeldClose tells a conversation an engagement is starting that its
// task's close is held (TellTaskClosing). Every runtime reads staged notes
// before its agent's first model call of an engagement, so the agent learns
// the task is resolved before it does anything. The note is a transcript row
// rather than a line in the conversation's system block, which is fixed for
// the conversation's life: the hold can be cancelled after this engagement,
// and a row says what was true when it was sent.
//
// Work a person started (a conversation a person delegated, or an engagement
// delivering a person's message) is told what happened without what to do
// about it (TellTaskClosing). A task held for a person's deliverable is
// engaged almost only that way, by the person asking for changes before they
// resolve it. Best effort: a failed read or stage logs, and the engagement
// runs without the note.
func (s *Spawner) tellHeldClose(ctx context.Context, orgID string, conv *domain.Conversation, taskID string) {
	if s.tasks == nil || s.events == nil {
		return
	}
	hold, err := s.tasks.CloseDeferralForTaskSystem(ctx, orgID, taskID)
	if err != nil {
		delegateLog.Warn("read the task's held close failed; the engagement starts without the close note", "task_id", taskID, "error", err)
		return
	}
	if hold == nil {
		return
	}
	evt, err := s.events.GetSystem(ctx, orgID, hold.EventID)
	if err != nil || evt == nil {
		delegateLog.Warn("read the held close's event failed; the engagement starts without the close note", "task_id", taskID, "event_id", hold.EventID, "error", err)
		return
	}
	s.tellTaskClosing(ctx, orgID, conv.ID, s.personStarted(ctx, orgID, conv), *evt)
}

// personStarted reports whether a person started the engagement a claim on
// conv begins: they delegated the conversation, which every step of its
// blueprint inherits, or a message of theirs is waiting for it. A failed read
// of the waiting input answers false, and the engagement is told.
func (s *Spawner) personStarted(ctx context.Context, orgID string, conv *domain.Conversation) bool {
	if conv.CreatorUserID != "" {
		return true
	}
	if s.pendingInput == nil {
		return false
	}
	_, userID, ok, err := s.pendingInput.Peek(ctx, orgID, conv.ID)
	if err != nil {
		delegateLog.Warn("read the conversation's waiting input failed; telling it of the held close", "conversation", conv.ID, "error", err)
		return false
	}
	return ok && userID != ""
}

// stagedInjectionsForEngagement flushes the conversation's durable
// staged-injection queue and returns the bundled <system-note> block an SDK
// engagement sends with its first message: every injection staged while the
// run was not running. A resume puts it ahead of the person's message, and a
// launch ahead of its continuation note or after its opening
// (composeLaunchTurn). Returns "" (nothing to send) when nothing is staged or
// on any read error: feedback never blocks an engagement.
//
// This is the durable sibling of deliverableLedgerForResume — SendMessage
// prepends both, so a resume that has both a resolved deliverable and a staged
// injection carries two <system-note> blocks ahead of the user's text.
func (s *Spawner) stagedInjectionsForEngagement(ctx context.Context, orgID, conversationID string) string {
	return domain.StagedInjectionBlock(s.flushStagedInjections(ctx, orgID, conversationID))
}

// flushStagedInjections claims every pending staged injection on the
// conversation, oldest first, or none when nothing is staged or on any read
// error. The flush marks the rows delivered in the statement that returns
// them, so an injection is delivered exactly once even if two readers race.
func (s *Spawner) flushStagedInjections(ctx context.Context, orgID, conversationID string) []domain.StagedInjection {
	if s.stagedInjections == nil {
		return nil
	}
	injections, err := s.stagedInjections.FlushPendingSystem(ctx, orgID, conversationID)
	if err != nil {
		delegateLog.Warn("staged-injection flush failed; delivering without the block", "conversation", conversationID, "error", err)
		return nil
	}
	return injections
}
