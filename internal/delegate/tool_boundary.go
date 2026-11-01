package delegate

import (
	"context"
	"sync"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/domain/events"
)

// TellTaskClosing stages the note that evt resolved the conversation's task,
// delivered at its next tool boundary (stageNoteAtToolBoundary), unless the
// conversation already has the note for evt. Reports whether the
// conversation has the note now. The router tells each working conversation
// through it when a close begins to hold, and the dispatcher tells an
// engagement that starts while it holds (tellHeldClose), so both send the
// same note once. A failed read of whether it was told stages the note
// anyway: a second copy of it costs the agent less than none.
func (s *Spawner) TellTaskClosing(ctx context.Context, orgID, conversationID string, evt domain.Event) bool {
	if s.stagedInjections != nil {
		told, err := s.stagedInjections.HasNoteForEventSystem(ctx, orgID, conversationID, domain.StagedInjectionProducerTaskClosing, evt.ID)
		if err != nil {
			delegateLog.Warn("read whether the conversation has the close note failed; staging it", "conversation", conversationID, "event_id", evt.ID, "error", err)
		} else if told {
			return true
		}
	}
	metadataJSON := evt.MetadataJSON
	if current, err := events.Current(ctx, orgID, evt.EventType, metadataJSON); err == nil {
		metadataJSON = current
	}
	return s.stageNoteAtToolBoundary(ctx, orgID, conversationID, domain.StagedInjectionProducerTaskClosing,
		domain.TaskClosingInjection(evt.EventType, metadataJSON), domain.NoteProvenance{EventID: evt.ID, EventType: evt.EventType})
}

// stageNoteAtToolBoundary stages one agent-facing note for a working
// conversation and has it reach the agent at its next tool boundary: the
// moment every tool call the agent has in flight has returned. It never
// starts a turn. producer tags the row (domain.StagedInjectionProducer*),
// prov records the event the note was composed from, and body is the bare,
// already-rendered line. Reports whether the note was staged; a blank body
// stages nothing.
//
// The note is an ordinary injection:system-note row. A native loop reads it
// before its next model call, which is that boundary already, and only a
// person's input or a new-event injection keeps a finishing loop going, so a
// note that lands as the model stops waits for the next resume. A live SDK
// process has its handle armed (toolBoundary) instead, and the SDK asks for
// the note at its next tool batch. A conversation with no live process here,
// parked or concluded or owned by another executor, reads the row on its next
// resume.
func (s *Spawner) stageNoteAtToolBoundary(ctx context.Context, orgID, conversationID, producer, body string, prov domain.NoteProvenance) bool {
	if body == "" {
		return false
	}
	if _, ok := s.stageInjection(ctx, orgID, conversationID, domain.MessageSubtypeInjectionSystemNote, producer, body, prov); !ok {
		return false
	}
	if h := s.getProc(conversationID); h != nil {
		h.boundary.arm()
	}
	return true
}

// toolBoundary is whether a live SDK process owes its agent staged notes at
// the next tool batch. Arming follows the stage, so a batch that finds it
// armed finds the rows committed. It stays armed across a turn that ends
// without a tool batch, since a turn queued behind it runs under the same
// process, and goes with the process's handle.
type toolBoundary struct {
	mu    sync.Mutex
	armed bool
}

func (b *toolBoundary) arm() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.armed = true
}

// take disarms the boundary and reports whether it was armed.
func (b *toolBoundary) take() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	armed := b.armed
	b.armed = false
	return armed
}

// notesAtToolBatch answers the SDK's PostToolBatch hook for a live process:
// the conversation's staged notes, flushed and bundled, when its boundary is
// armed, and nothing otherwise, so an ordinary batch reads no rows. The
// model's next request waits on the answer and reads the bundle with the
// batch's results. The flush marks the rows delivered, so a later resume does
// not repeat them.
func (s *Spawner) notesAtToolBatch(orgID, conversationID string) string {
	h := s.getProc(conversationID)
	if h == nil || !h.boundary.take() {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), detachedWriteDeadline)
	defer cancel()
	return domain.StagedInjectionBoundaryBlock(s.flushStagedInjections(ctx, orgID, conversationID))
}
