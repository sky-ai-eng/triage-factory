// Deliverable-change agent feedback delivery (TFAC-493). When a human resolves a
// deliverable a run produced (approve/dismiss a draft PR, submit/dismiss a pending
// review), the agent that drafted it is told — without ever blocking the
// blueprint:
//
//   - Live run (a warm process): InjectDeliverableNote stages a note for the
//     process's next tool boundary, fire-and-forget.
//   - Terminal / paused run: nothing is persisted here. The ledger is *derived*
//     — at the next user follow-up (SendMessage), the resolved
//     deliverables since the agent's last activity are bundled into one <system-note>
//     prepended ahead of the user's message (deliverableLedgerForResume). No new
//     table: the deliverables' updated_at + terminal state vs the run's last agent
//     message is the whole ledger.
//
// The copy itself lives in internal/domain (DeliverableResolutionNote /
// DeliverableLedgerBlock), so this delivery seam and the resolve handlers render the
// same four strings.

package delegate

import (
	"context"
	"sort"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// InjectDeliverableNote stages a note describing a just-resolved deliverable for a
// run's LIVE process, if it has one, at the process's next tool boundary
// (stageNoteAtToolBoundary). Returns true iff a warm process was found and the
// stage was dispatched.
//
// Truly non-blocking: the only synchronous work is the cheap getProc liveness
// gate. The stage — a DB insert — runs on a detached goroutine off
// context.Background(), so an HTTP resolve handler never waits on it and a
// request cancel never tears it down. It never flips run status or gates the
// blueprint either — resolution is an async sidecar.
//
// A false return is NOT a lost note: a terminal/paused run has no warm process to
// tell, and the same resolution is re-derived from the deliverable row into the
// ledger on the run's next resume (deliverableLedgerForResume). A note the
// process does not reach before its engagement ends (no tool batch follows it)
// stays staged, and the next engagement's first message carries it beside the
// ledger. We deliberately do NOT wake a terminal run here ("do not restart the
// agent").
//
// The deliverable passed must carry its post-resolution State (the handlers hand us
// the flipped copy), so domain.DeliverableResolutionNote renders the right shape.
func (s *Spawner) InjectDeliverableNote(orgID, conversationID string, a domain.Deliverable) bool {
	note := domain.DeliverableResolutionNote(a)
	if note == "" {
		return false // not one of the four reported resolution states
	}
	if s.getProc(conversationID) == nil {
		return false
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), detachedWriteDeadline)
		defer cancel()
		s.stageNoteAtToolBoundary(ctx, orgID, conversationID, domain.MessageSubtypeInjectionSystemNote,
			domain.NoteProducerDeliverableResolved, note, domain.NoteProvenance{})
	}()
	return true
}

// deliverableLedgerForResume derives the bundled <system-note> block to prepend ahead
// of a resuming run's user message: every deliverable this run produced that a human
// resolved while the agent was NOT running, since the run's last agent activity.
// Returns "" (prepend nothing) when there's no pending change.
//
// Derived, not stored (TFAC-493): the watermark is the run's last non-user message
// (LastAgentActivityAtSystem) — falling back to the run's start when it has no
// agent message yet — and the entries are the run's deliverables whose updated_at is
// strictly after it AND whose terminal state is a reported human resolution. User
// messages are excluded from the watermark, so the resume message the server just
// recorded can't suppress the ledger.
//
// Against the durable deliverable state this watermark only ever OVER-includes,
// never misses — a deliberate trade (a duplicate note is harmless; a missed
// resolution leaves the agent with a stale picture). The common over-include: a
// deliverable resolved while the run was live got its note staged
// (InjectDeliverableNote), and normally the agent reads it and emits messages
// that advance the watermark past it, so it doesn't reappear here. But if no
// non-user message lands after the resolution — the textbook case being a human
// approving the FINAL deliverable of a fast-finishing run, whose next turn is just
// the conclusion — the watermark stays put and the same resolution is bundled
// again on the next resume, beside the staged note if the process never reached
// it. That second copy is expected, not a bug.
//
// The "never miss" half is conditional on the resolution actually reaching the
// deliverable row: a resolve handler whose best-effort state flip failed leaves the
// row in its pre-resolution state, so IsResolutionNoteState skips it and the
// ledger can't re-derive it (see handleDeliverableApprove step 4). That's the one
// place a note can be dropped, and it's accepted there.
//
// Any read error degrades to "" — feedback never blocks a resume.
func (s *Spawner) deliverableLedgerForResume(ctx context.Context, orgID string, conv *domain.Conversation) string {
	if s.deliverables == nil || s.conversations == nil || conv == nil {
		return ""
	}
	watermark, ok, err := s.conversations.LastAgentActivityAtSystem(ctx, orgID, conv.ID)
	if err != nil {
		delegateLog.Warn("deliverable ledger: read last-activity watermark failed", "conversation", conv.ID, "error", err)
		return ""
	}
	if !ok {
		watermark = conv.StartedAt
	}
	arts, err := s.deliverables.ListByConversationSystem(ctx, orgID, conv.ID)
	if err != nil {
		delegateLog.Warn("deliverable ledger: list deliverables failed", "conversation", conv.ID, "error", err)
		return ""
	}
	resolved := make([]domain.Deliverable, 0, len(arts))
	for _, a := range arts {
		if a.UpdatedAt.After(watermark) && domain.IsResolutionNoteState(a) {
			resolved = append(resolved, a)
		}
	}
	// Oldest→newest so the bundle reads in the order the human acted.
	sort.SliceStable(resolved, func(i, j int) bool { return resolved[i].UpdatedAt.Before(resolved[j].UpdatedAt) })
	return domain.DeliverableLedgerBlock(resolved)
}
