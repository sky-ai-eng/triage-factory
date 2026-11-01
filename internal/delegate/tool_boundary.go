package delegate

import (
	"context"
	"sync"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// StageNoteAtToolBoundary stages one agent-facing note for a working
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
// process has its handle armed (toolBoundary) instead, since the SDK starts
// a message sent in mid-turn only as a turn of its own after the current
// one. A conversation with no live process here, parked or concluded or
// owned by another executor, reads the row on its next resume.
func (s *Spawner) StageNoteAtToolBoundary(ctx context.Context, orgID, conversationID, producer, body string, prov domain.NoteProvenance) bool {
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

// toolBoundary delivers a live SDK process's staged notes at its next tool
// boundary once armed. The activity sink reports every boundary it reads
// (reached); arm makes the next one deliver. It stays armed across a turn
// that ends without reaching one, since a turn queued behind it runs under
// the same process, and goes with the process's handle.
type toolBoundary struct {
	mu    sync.Mutex
	armed bool
	// deliver flushes the conversation's staged notes into the process
	// (deliverNotesAtBoundary). It runs off the reader goroutine, which must
	// not block on a database read or a write into the process.
	deliver func()
}

func (b *toolBoundary) arm() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.armed = true
}

// reached reports a tool boundary, from the reader goroutine.
func (b *toolBoundary) reached() {
	b.mu.Lock()
	fire := b.armed && b.deliver != nil
	b.armed = false
	b.mu.Unlock()
	if fire {
		go b.deliver()
	}
}

// boundaryProc is what a boundary delivery needs of the live process.
type boundaryProc interface {
	Send(ctx context.Context, text string) error
	Interrupt(ctx context.Context) error
}

// deliverNotesAtBoundary flushes the conversation's staged notes, sends them
// into the process as one bundle, and interrupts the turn. The send goes
// first: an interrupted turn with a message queued behind it starts that
// message instead of parking (driveLiveConversation), and the interrupt
// cancels only the model request that started after the tool results, which
// is redone with the notes in front of it. The flush marks the rows
// delivered, so a later resume does not repeat them.
//
// A pause, never a stop: a stopped conversation parks, and a parked one is
// not working, so a held close would apply before anything resumed it.
func (s *Spawner) deliverNotesAtBoundary(orgID, conversationID string, proc boundaryProc) {
	ctx, cancel := context.WithTimeout(context.Background(), detachedWriteDeadline)
	defer cancel()
	block := domain.StagedInjectionBoundaryBlock(s.flushStagedInjections(ctx, orgID, conversationID))
	if block == "" {
		return
	}
	if err := proc.Send(ctx, block); err != nil {
		delegateLog.Warn("tool boundary: send of staged notes failed; they were flushed and are lost", "conversation", conversationID, "error", err)
		return
	}
	s.activityFor(conversationID).touch()
	if err := proc.Interrupt(ctx); err != nil {
		// The notes are queued in the process, and run as the next turn
		// unless this one concludes.
		delegateLog.Warn("tool boundary: interrupt failed", "conversation", conversationID, "error", err)
	}
}
