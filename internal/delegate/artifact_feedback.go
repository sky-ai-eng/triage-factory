// Artifact-change agent feedback delivery (TFAC-493). When a human resolves an
// artifact a run produced (approve/dismiss a draft PR, submit/dismiss a pending
// review), the agent that drafted it is told — without ever blocking the
// blueprint:
//
//   - Live run (a warm, steerable process): InjectArtifactNote steers a single
//     <system-note> into it immediately, fire-and-forget.
//   - Terminal / paused run: nothing is persisted here. The ledger is *derived*
//     — at the next user follow-up (SendMessage → ResumeOpenRun), the resolved
//     artifacts since the agent's last activity are bundled into one <system-note>
//     prepended ahead of the user's message (artifactLedgerForResume). No new
//     table: the artifacts' updated_at + terminal state vs the run's last agent
//     message is the whole ledger.
//
// The copy itself lives in internal/domain (ArtifactResolutionNote /
// ArtifactLedgerBlock), so this delivery seam and the resolve handlers render the
// same four strings.

package delegate

import (
	"context"
	"sort"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// InjectArtifactNote delivers a single fire-and-forget <system-note> describing a
// just-resolved artifact to a run's LIVE process, if it has one. Returns true iff
// a warm process was found and delivery was dispatched.
//
// Truly non-blocking: the only synchronous work is the cheap getProc liveness
// gate. The actual delivery — a DB insert (the transcript row) and a write into
// the live process's stdin, either of which can block under contention — runs on
// a detached goroutine off context.Background(), so an HTTP resolve handler never
// waits on it and a request cancel never tears it down. It never flips run status
// or gates the blueprint either — resolution is an async sidecar (TFAC-379 #2).
//
// A false return is NOT a lost note: a terminal/paused run has no warm process to
// steer, and the same resolution is re-derived from the artifact row into the
// ledger on the run's next resume (artifactLedgerForResume). A true return is
// optimistic — the process found here may close before the goroutine's steer
// lands, in which case the resolution likewise resurfaces via that ledger. We
// deliberately do NOT wake a terminal run here (TFAC-493: "do not restart the
// agent").
//
// The artifact passed must carry its post-resolution State (the handlers hand us
// the flipped copy), so domain.ArtifactResolutionNote renders the right shape.
func (s *Spawner) InjectArtifactNote(orgID, runID string, a domain.Artifact) bool {
	note := domain.ArtifactResolutionNote(a)
	if note == "" {
		return false // not one of the four reported resolution states
	}
	if s.getProc(runID) == nil {
		return false
	}
	s.deliverInjectionLive(orgID, runID, domain.WrapSystemNote(note))
	return true
}

// deliverInjectionLive records a wrapped <system-note> as a transcript row and steers
// it into the run's warm process, on a DETACHED goroutine. The caller must have
// already confirmed the run is live (getProc != nil) — this is the shared live-
// delivery core of both the artifact-change note (InjectArtifactNote) and the
// generic staged-injection queue (StageOrDeliverInjection).
//
// Detached so the caller (an HTTP resolve handler or an eventbus subscriber)
// returns immediately. Record + broadcast first so the live transcript shows the
// out-of-band action as a user message (the shape the agent receives it as), then
// steer it into the process. All best-effort: a failed record must not stop the
// steer, and a steer that races the process closing is tolerated — the artifact
// note re-derives via the ledger on resume, the staged injection re-fires on the next
// head change.
func (s *Spawner) deliverInjectionLive(orgID, runID, wrapped string) {
	go func() {
		s.recordInjectedNote(orgID, runID, wrapped)
		if err := s.Steer(context.Background(), runID, wrapped); err != nil {
			delegateLog.Warn("deliver injection live: steer failed", "run", runID, "error", err)
		}
	}()
}

// recordInjectedNote persists a live-injected <system-note> as a user-role
// run_messages row and broadcasts it, so the run transcript and any watching UI
// show the human action inline. Admin pool (no JWT claims on this path) +
// best-effort: a failure is logged, never fatal — the steer above is what the
// agent actually consumes. Recorded as role='user' (not agent activity), so it
// never advances the artifact-change ledger watermark.
//
// Subtype is "system_note", not "text": the transcript renders user rows by Role
// regardless of subtype today (so this shows as an operator line), but tagging it
// distinctly from a human steer (server's message endpoint records those as
// subtype="text") leaves the UI a handle to style it / drop the reply affordance
// later without a schema change.
func (s *Spawner) recordInjectedNote(orgID, runID, content string) {
	if s.agentRuns == nil {
		return
	}
	msg := &domain.Message{ConversationID: runID, Role: "user", Subtype: "system_note", Content: content}
	id, err := s.agentRuns.InsertMessageSystem(context.Background(), orgID, msg)
	if err != nil {
		delegateLog.Warn("record injected artifact note failed", "run", runID, "error", err)
		return
	}
	msg.ID = int(id)
	s.broadcastMessage(orgID, runID, msg)
}

// artifactLedgerForResume derives the bundled <system-note> block to prepend ahead
// of a resuming run's user message: every artifact this run produced that a human
// resolved while the agent was NOT running, since the run's last agent activity.
// Returns "" (prepend nothing) when there's no pending change.
//
// Derived, not stored (TFAC-493): the watermark is the run's last non-user message
// (LastAgentActivityAtSystem) — falling back to the run's start when it has no
// agent message yet — and the entries are the run's artifacts whose updated_at is
// strictly after it AND whose terminal state is a reported human resolution. User
// messages are excluded from the watermark, so the resume message the server just
// recorded can't suppress the ledger.
//
// Against the durable artifact state this watermark only ever OVER-includes,
// never misses — a deliberate trade (a duplicate note is harmless; a missed
// resolution leaves the agent with a stale picture). The common over-include: an
// artifact resolved while the run was live got its note steered in
// (InjectArtifactNote), and normally the agent then emits a turn whose messages
// advance the watermark past it, so it doesn't reappear here. But if no non-user
// message lands after the steer — the textbook case being a human approving the
// FINAL artifact of a fast-finishing run, whose next turn is just the conclusion
// — the watermark stays put and the same note is bundled again on the next
// resume. That second copy is expected, not a bug.
//
// The "never miss" half is conditional on the resolution actually reaching the
// artifact row: a resolve handler whose best-effort state flip failed leaves the
// row in its pre-resolution state, so IsResolutionNoteState skips it and the
// ledger can't re-derive it (see handleArtifactApprove step 4). That's the one
// place a note can be dropped, and it's accepted there.
//
// Any read error degrades to "" — feedback never blocks a resume.
func (s *Spawner) artifactLedgerForResume(ctx context.Context, orgID string, run *domain.Conversation) string {
	if s.artifacts == nil || s.agentRuns == nil || run == nil {
		return ""
	}
	watermark, ok, err := s.agentRuns.LastAgentActivityAtSystem(ctx, orgID, run.ID)
	if err != nil {
		delegateLog.Warn("artifact ledger: read last-activity watermark failed", "run", run.ID, "error", err)
		return ""
	}
	if !ok {
		watermark = run.StartedAt
	}
	arts, err := s.artifacts.ListByRunSystem(ctx, orgID, run.ID)
	if err != nil {
		delegateLog.Warn("artifact ledger: list artifacts failed", "run", run.ID, "error", err)
		return ""
	}
	resolved := make([]domain.Artifact, 0, len(arts))
	for _, a := range arts {
		if a.UpdatedAt.After(watermark) && domain.IsResolutionNoteState(a) {
			resolved = append(resolved, a)
		}
	}
	// Oldest→newest so the bundle reads in the order the human acted.
	sort.SliceStable(resolved, func(i, j int) bool { return resolved[i].UpdatedAt.Before(resolved[j].UpdatedAt) })
	return domain.ArtifactLedgerBlock(resolved)
}
