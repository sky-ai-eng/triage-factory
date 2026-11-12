package delegate

import (
	"context"
	"errors"
	"strings"
	"sync"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/domain/events"
)

// TellTaskClosing stages the note that evt resolved the conversation's task,
// delivered at its next tool boundary (stageNoteAtToolBoundary), unless the
// conversation already has the note for evt. Reports whether the
// conversation has the note now. The router tells each working conversation
// through it when a close begins to hold, and an engagement starting, or a
// warm claim resuming, while it holds tells its own conversation
// (tellHeldClose), so both send the same note once. A failed read of whether it was told stages the note
// anyway: a second copy of it costs the agent less than none.
//
// Work a person started (personStarted) is told what happened without what
// to do about it (domain.TaskClosingNotice): what the person asked for is what
// the agent acts on. A close note still waiting for such work is withdrawn and
// the notice staged in its place, since the waiting note would reach the
// agent beside the person's message. One already delivered stays the agent's
// history.
func (s *Spawner) TellTaskClosing(ctx context.Context, orgID, conversationID string, evt domain.Event) bool {
	person := false
	if s.conversations != nil {
		conv, err := s.conversations.GetSystem(ctx, orgID, conversationID)
		if err != nil {
			delegateLog.Warn("read the conversation failed; telling it the whole close note", "conversation", conversationID, "error", err)
		} else if conv != nil {
			person = s.personStarted(ctx, orgID, conv)
		}
	}
	return s.tellTaskClosing(ctx, orgID, conversationID, person, evt)
}

// tellTaskClosing is TellTaskClosing with whether a person started the
// conversation's work already known.
func (s *Spawner) tellTaskClosing(ctx context.Context, orgID, conversationID string, person bool, evt domain.Event) bool {
	if s.stagedInjections != nil {
		if person {
			if err := s.stagedInjections.WithdrawPendingSystem(ctx, orgID, conversationID, domain.StagedInjectionProducerTaskClosing); err != nil {
				delegateLog.Warn("withdraw the waiting close note failed; the agent may read it beside the person's message", "conversation", conversationID, "error", err)
			}
		}
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
	body := domain.TaskClosingInjection(evt.EventType, metadataJSON)
	if person {
		body = domain.TaskClosingNotice(evt.EventType, metadataJSON)
	}
	return s.stageNoteAtToolBoundary(ctx, orgID, conversationID, domain.MessageSubtypeInjectionSystemNote,
		domain.StagedInjectionProducerTaskClosing, body, domain.NoteProvenance{EventID: evt.ID, EventType: evt.EventType})
}

// stageNoteAtToolBoundary stages one agent-facing note for a working
// conversation and has it reach the agent at its next tool boundary: the
// moment every tool call the agent has in flight has returned. subtype is
// what the note is for (domain.MessageSubtypeInjectionEvent for a new-event
// injection, domain.MessageSubtypeInjectionSystemNote for every other note),
// producer tags the row (domain.StagedInjectionProducer*), prov records the
// event the note was composed from, and body is the bare, already-rendered
// line. Reports whether the note was staged; a blank body stages nothing.
//
// A native loop reads the row before its next model call, which is that
// boundary already. A live SDK process has its handle armed (toolBoundary)
// instead, and the SDK asks for the note at its next tool batch. In both, a
// turn the model ends goes on only for a person's input or a new-event
// injection, and a turn that ends on stop_blueprint goes on for neither, so a
// note that lands as the turn ends waits for the next resume. A
// conversation with no live process here, parked or concluded or owned by
// another executor, reads the row on its next resume.
func (s *Spawner) stageNoteAtToolBoundary(ctx context.Context, orgID, conversationID, subtype, producer, body string, prov domain.NoteProvenance) bool {
	if body == "" {
		return false
	}
	if _, ok := s.stageInjection(ctx, orgID, conversationID, subtype, producer, body, prov); !ok {
		return false
	}
	if h := s.getProc(conversationID); h != nil {
		h.boundary.arm()
	}
	return true
}

// toolBoundary is whether a live SDK process owes its agent waiting input at
// the next tool batch: staged notes or a person's messages. Arming follows
// the write, so a batch that finds it armed finds the rows committed. It
// stays armed across a turn that ends without a tool batch, since the driver
// may start another turn under the same process, and goes with the process's
// handle.
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

// notesAtToolBatch answers the SDK's PostToolBatch hook for a live process
// when its boundary is armed, and hands nothing otherwise, so an ordinary
// batch reads no rows. The answer is the conversation's staged notes,
// bundled, followed by each person's message waiting for the process in the
// steer envelope (domain.SteerOpen), which tells the model the message
// arrived while it was working. The model's next request waits on the answer
// and reads it with the batch's results.
//
// Both are flushed, marked delivered, before the answer goes out, so a later
// resume does not repeat them; a person's messages only under claimID, the
// engagement driving the process. An answer that never reached the model (an
// interrupt gave the batch up first, say) is undone: the rows go back to the
// queue and the boundary re-arms, so the next batch, the next turn, or the
// next resume carries them.
func (s *Spawner) notesAtToolBatch(orgID, conversationID, claimID string) agentproc.ToolBatchNotes {
	h := s.getProc(conversationID)
	if h == nil || !h.boundary.take() {
		return agentproc.ToolBatchNotes{}
	}
	ctx, cancel := context.WithTimeout(context.Background(), detachedWriteDeadline)
	defer cancel()
	notes := s.flushStagedInjections(ctx, orgID, conversationID)
	var messages []domain.Message
	if rows, ok := s.waitingRows(ctx, orgID, conversationID); ok {
		messages, _ = s.takeWaitingMessages(ctx, orgID, conversationID, claimID, rows)
	}
	if len(notes) == 0 && len(messages) == 0 {
		return agentproc.ToolBatchNotes{}
	}
	var parts []string
	if len(notes) > 0 {
		parts = append(parts, domain.StagedInjectionBoundaryBlock(notes))
	}
	for _, m := range messages {
		parts = append(parts, domain.SteerOpen+m.Content+domain.SteerClose)
	}
	return agentproc.ToolBatchNotes{
		Text: strings.Join(parts, db.PendingInputSeparator),
		Undo: func() {
			s.restoreStagedInjections(orgID, conversationID, notes)
			s.requeueMessages(orgID, conversationID, claimID, messages)
			if h := s.getProc(conversationID); h != nil {
				h.boundary.arm()
			}
		},
	}
}

// waitingRows reads the conversation's transcript, in assembly order, for the
// input waiting for its live process. ok is false when the read failed, and
// the input waits for the next read.
func (s *Spawner) waitingRows(ctx context.Context, orgID, conversationID string) (rows []domain.Message, ok bool) {
	if s.conversations == nil {
		return nil, false
	}
	rows, err := s.conversations.ListForAssemblySystem(ctx, orgID, conversationID)
	if err != nil {
		delegateLog.Warn("read the input waiting for a live process failed; it waits for the next read", "conversation", conversationID, "error", err)
		return nil, false
	}
	return rows, true
}

// takeWaitingMessages flushes the people's messages among rows (waitingRows):
// the conversation's pending input, in assembly order, marked delivered under
// claimID so only the engagement driving the process takes them. fenced
// reports that the claim fence refused the flush, which means a successor owns
// the conversation and the messages are its to deliver. Any other failure
// takes nothing, and the messages wait for the next read.
func (s *Spawner) takeWaitingMessages(ctx context.Context, orgID, conversationID, claimID string, rows []domain.Message) (messages []domain.Message, fenced bool) {
	var ids []int
	for _, r := range rows {
		if waitingMessage(r) {
			messages = append(messages, r)
			ids = append(ids, r.ID)
		}
	}
	if len(ids) == 0 {
		return nil, false
	}
	if err := s.conversations.MarkDeliveredForClaimSystem(ctx, orgID, conversationID, claimID, ids, ""); err != nil {
		if errors.Is(err, db.ErrClaimReleased) {
			delegateLog.Error("claim fence refused delivering a person's messages to the live process — a successor owns this conversation",
				"conversation", conversationID, "claim_id", claimID, "org_id", orgID)
			return nil, true
		}
		delegateLog.Warn("mark the messages waiting for a live process delivered failed; they wait for the next read", "conversation", conversationID, "error", err)
		return nil, false
	}
	return messages, false
}

// requeueMessages puts flushed messages back on the conversation's pending
// input, for a delivery that never reached the model. Detached from any
// caller's context, since it runs as the delivery is given up.
func (s *Spawner) requeueMessages(orgID, conversationID, claimID string, messages []domain.Message) {
	if s.conversations == nil || len(messages) == 0 {
		return
	}
	ids := make([]int, len(messages))
	for i, m := range messages {
		ids[i] = m.ID
	}
	ctx, cancel := context.WithTimeout(context.Background(), detachedWriteDeadline)
	defer cancel()
	if err := s.conversations.RequeueInputForClaimSystem(ctx, orgID, conversationID, claimID, ids); err != nil {
		delegateLog.Warn("returning undelivered messages to the queue failed; the transcript shows them delivered though the agent did not read them",
			"conversation", conversationID, "messages", len(ids), "error", err)
	}
}

// waitingMessage reports whether r is a person's message the conversation has
// not delivered yet: its pending input, in the queue's own shape
// (pendingUserInput).
func waitingMessage(r domain.Message) bool {
	return pendingRow(r) && r.Subtype == ""
}

// pendingRow reports whether r is an undelivered user row: input of any kind
// still waiting for the agent.
func pendingRow(r domain.Message) bool {
	return r.Role == "user" && r.Delivered != nil && !*r.Delivered
}
