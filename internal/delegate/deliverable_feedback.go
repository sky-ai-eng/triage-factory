// Deliverable-change agent feedback. When a deliverable a conversation
// produced is resolved (a person approves or dismisses it, GitHub shows its
// pull request resolved, or the pull request a pending review is about ends),
// the agent that drafted it is told, without ever blocking the blueprint.
//
// Every resolution is told the same way, whatever the conversation's runtime
// or state: one note on the staged-injection queue
// (stageNoteAtToolBoundary), which each runtime reads on its own schedule.
// The note is a second write after the resolution, so a failed stage loses
// it; in return each resolution reaches the agent once, including one that
// lands while the agent is working.
//
// The copy lives in internal/domain (DeliverableResolutionNote), so every
// producer renders the same strings.

package delegate

import (
	"context"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TellDeliverableResolved stages the note that deliverable d was resolved on
// the conversation that produced it. d is the row as the resolution stored
// it: its state and the resolution in its details pick the copy
// (domain.DeliverableResolutionNote). A row that is not in a reported
// resolution state, or whose conversation was purged, stages nothing.
//
// The note is staged whatever the conversation's state. A working
// conversation reads it at its next tool boundary and a parked one at its
// next engagement. One that has finished reads it if a person resumes it,
// and until then the row does nothing: a note never wakes a conversation.
//
// It never changes the conversation's status or its blueprint's.
func (s *Spawner) TellDeliverableResolved(ctx context.Context, orgID string, d domain.Deliverable) {
	if d.ConversationID == "" {
		return
	}
	note := domain.DeliverableResolutionNote(d)
	if note == "" {
		return
	}
	s.stageNoteAtToolBoundary(ctx, orgID, d.ConversationID, domain.MessageSubtypeInjectionSystemNote,
		domain.StagedInjectionProducerDeliverableResolved, note, domain.NoteProvenance{})
}

// InjectDeliverableNote is TellDeliverableResolved for a request handler
// that has just resolved d: the stage runs on a detached goroutine off
// context.Background(), so the handler never waits on the write and a
// cancelled request does not abandon it.
func (s *Spawner) InjectDeliverableNote(orgID string, d domain.Deliverable) {
	if d.ConversationID == "" || domain.DeliverableResolutionNote(d) == "" {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), detachedWriteDeadline)
		defer cancel()
		s.TellDeliverableResolved(ctx, orgID, d)
	}()
}
