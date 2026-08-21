// Generic staged-injection queue. The producer-agnostic "deliver live or stage
// for the next resume" seam, generalized from the artifact-change feedback:
//
//   - Live run (a warm, steerable process): the note is wrapped and steered in
//     immediately, fire-and-forget (deliverInjectionLive).
//   - Terminal / paused run: the bare injection is persisted to the durable
//     staged-injection queue (db.StagedInjectionStore, backed by undelivered
//     messages rows) and flushed — bundled into one <system-note> ahead of the
//     user's message — on the next resume (stagedInjectionsForResume, wired
//     into SendMessage).
//
// Unlike the artifact-change ledger (which DERIVES its terminal injections from
// the artifact rows, so it needs no queue), a producer here has no durable row
// of its own to re-derive from — the PR coherence feed is such a producer.
// That's why this half is a real durable queue, not a derivation.

package delegate

import (
	"context"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// stageOrDeliverInjection routes one agent-facing note for a conversation by
// its live state. It is the local-arm primitive StageOrDeliverAdditiveEvent is
// built on:
//
//   - live (a warm process): the note is wrapped and steered in immediately,
//     fire-and-forget; delivered=true.
//   - not live (parked / terminal-resumable / terminal): the bare body is
//     appended to the durable queue for the next resume to flush; staged=true.
//
// producer tags the row for audit (domain.StagedInjectionProducer*), prov
// records the event the note was composed from, and body is the bare,
// already-rendered line — the live path wraps one note, the flush bundles and
// wraps the block. A blank body is a no-op.
//
// Never flips conversation status or wakes a terminal run: this is an async
// sidecar, so a note for a terminal-but-resumable conversation waits in the
// queue until the user's next follow-up, and one for a conversation that can
// never resume simply never reaches the agent. The caller gates that — see
// HandlePRCoherence, which only stages for live-or-resumable conversations.
//
// stagedID is the durable row's id when staged=true (empty otherwise), which
// the additive gate needs in order to delete the row when its post-stage
// resumability recheck decides against delivery.
//
// delivered=false with staged=false is a drop: the live gate raced a closing
// process and the durable append then failed (no store wired, or AppendSystem
// errored). Nothing re-delivers it, so a producer whose news does not recur
// must not treat a drop as tolerable — the coherence feed can, because its
// next transition carries a fresh note.
func (s *Spawner) stageOrDeliverInjection(orgID, conversationID, producer, body string, prov domain.NoteProvenance) (delivered, staged bool, stagedID string) {
	if body == "" {
		return false, false, ""
	}
	if s.getProc(conversationID) != nil {
		s.deliverInjectionLive(orgID, conversationID, domain.WrapSystemNote(body), producer, prov)
		return true, false, ""
	}
	if s.stagedInjections == nil {
		delegateLog.Warn("stage injection dropped: no staged-injection store wired", "conversation", conversationID, "producer", producer)
		return false, false, ""
	}
	stored, err := s.stagedInjections.AppendSystem(context.Background(), orgID, domain.StagedInjection{
		ConversationID: conversationID,
		Producer:       producer,
		Body:           body,
		Provenance:     prov,
	})
	if err != nil {
		delegateLog.Warn("stage injection: append failed (dropped for good; only a later transition from a self-renewing producer might stage fresh news)", "conversation", conversationID, "producer", producer, "error", err)
		return false, false, ""
	}
	return false, true, stored.ID
}

// stagedInjectionsForResume flushes a resuming run's durable staged-injection queue and
// returns the bundled <system-note> block to prepend ahead of its user message —
// every injection staged while the run was not running (TFAC-501). Returns ""
// (prepend nothing) when nothing is staged or on any read error: feedback never
// blocks a resume.
//
// The flush is destructive (DELETE … RETURNING), so an injection is delivered exactly
// once even if two resumes race. This is the durable sibling of
// artifactLedgerForResume — SendMessage prepends both, so a resume that has both
// a resolved artifact and a staged injection carries two <system-note> blocks ahead of
// the user's text.
func (s *Spawner) stagedInjectionsForResume(ctx context.Context, orgID, conversationID string) string {
	if s.stagedInjections == nil {
		return ""
	}
	injections, err := s.stagedInjections.FlushPendingSystem(ctx, orgID, conversationID)
	if err != nil {
		delegateLog.Warn("staged-injection flush failed; resuming without the block", "conversation", conversationID, "error", err)
		return ""
	}
	return domain.StagedInjectionBlock(injections)
}
