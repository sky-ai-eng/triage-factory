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
// the run was live and the note was handed to the process.
//
// A false return is NOT a lost note: a terminal/paused run has no warm process to
// steer, and the same resolution is re-derived from the artifact row into the
// ledger on the run's next resume (artifactLedgerForResume). It never blocks the
// caller, flips run status, or gates the blueprint — resolution is an async
// sidecar (TFAC-379 locked decision #2).
//
// The artifact passed must carry its post-resolution State (the handlers hand us
// the flipped copy), so domain.ArtifactResolutionNote renders the right shape.
func (s *Spawner) InjectArtifactNote(ctx context.Context, orgID, runID string, a domain.Artifact) bool {
	note := domain.ArtifactResolutionNote(a)
	if note == "" {
		return false // not one of the four reported resolution states
	}
	// getProc is the liveness gate; nil means no warm process → defer to the
	// derived ledger on the next resume. We deliberately do NOT wake a terminal
	// run here (TFAC-493: "do not restart the agent").
	if s.getProc(runID) == nil {
		return false
	}
	wrapped := domain.WrapSystemNote(note)
	// Record + broadcast so the live transcript shows the human action as a user
	// message (the shape the agent receives it as), then steer it into the warm
	// process. All best-effort: a failed record/broadcast must not stop delivery,
	// and a steer that races the process closing just means the ledger picks it up
	// on the next resume.
	s.recordInjectedNote(orgID, runID, wrapped)
	if err := s.Steer(ctx, runID, wrapped); err != nil {
		delegateLog.Warn("inject artifact note: steer failed (will surface via the ledger on resume)", "run", runID, "error", err)
		return false
	}
	return true
}

// recordInjectedNote persists a live-injected <system-note> as a user-role
// run_messages row and broadcasts it, so the run transcript and any watching UI
// show the human action inline. Admin pool (no JWT claims on this path) +
// best-effort: a failure is logged, never fatal — the steer above is what the
// agent actually consumes. Recorded as role='user' (not agent activity), so it
// never advances the artifact-change ledger watermark.
func (s *Spawner) recordInjectedNote(orgID, runID, content string) {
	if s.agentRuns == nil {
		return
	}
	msg := &domain.AgentMessage{RunID: runID, Role: "user", Subtype: "text", Content: content}
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
// recorded can't suppress the ledger; the agent's own messages advance the
// watermark past anything delivered live, so a live-injected note doesn't reappear
// here. Any read error degrades to "" — feedback never blocks a resume.
func (s *Spawner) artifactLedgerForResume(ctx context.Context, orgID string, run *domain.AgentRun) string {
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
