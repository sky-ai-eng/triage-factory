// The staged-injection queue. A note for an agent is persisted to the durable
// queue (db.StagedInjectionStore, backed by undelivered messages rows) as the
// bare injection, under the subtype that says what it is for:
// injection:event for a new-event injection, injection:system-note for every
// other note. A native loop reads the rows itself before its next model call.
// An SDK conversation has them flushed — bundled into one <system-note> with
// its first message — when its next engagement starts
// (stagedInjectionsForEngagement, read by a resume's prepends and a launch's
// first turn), and a live SDK process has them handed to it at its next tool
// batch (notesAtToolBatch), or as its next turn when one of them is a
// new-event injection and the turn ends first.
//
// The queue is the only place a note waits: no producer keeps a second record
// to deliver from, so a note whose stage fails is lost, and one staged is
// delivered once.

package delegate

import (
	"context"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/domain/events"
)

// stageOrDeliverInjection routes one agent-facing note for an SDK
// conversation by its live state. It is the local arm
// StageOrDeliverAdditiveEvent falls back on for an SDK conversation:
//
//   - live (a warm process): the note is staged and the process's tool
//     boundary armed (stageNoteAtToolBoundary); delivered=true.
//   - not live (parked / terminal-resumable / terminal): the bare body is
//     staged under subtype for the next resume to flush; staged=true.
//
// producer tags the row for audit (domain.StagedInjectionProducer*), prov
// records the event the note was composed from, and body is the bare,
// already-rendered line, which the flush bundles and wraps. A blank body is a
// no-op.
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
// delivered=false with staged=false is a drop: the durable append failed (see
// stageInjection).
func (s *Spawner) stageOrDeliverInjection(ctx context.Context, orgID, conversationID, subtype, producer, body string, prov domain.NoteProvenance) (delivered, staged bool, stagedID string) {
	if body == "" {
		return false, false, ""
	}
	if s.getProc(conversationID) != nil {
		return s.stageNoteAtToolBoundary(ctx, orgID, conversationID, subtype, producer, body, prov), false, ""
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

// tellHeldClose tells a conversation whose turn is about to start that its
// task's close is held (TellTaskClosing): at the start of an engagement, and
// before a turn a warm claim resumes, which no claim precedes. Every runtime
// reads staged notes before its agent's next model call, so the agent learns
// the task is resolved before it does anything. The note is a transcript row
// rather than a line in the conversation's system block, which is fixed for
// the conversation's life: the hold can be cancelled after this turn, and a
// row says what was true when it was sent.
//
// Work a person drives (a conversation a person delegated, or one a person has
// written to) is told what happened without what to do about it
// (personStarted). A close holds only for a person's deliverable, so a held
// task is engaged almost only that way, by the person asking for changes
// before they resolve it. Best effort: a failed read or stage logs, and the
// turn runs without the note.
func (s *Spawner) tellHeldClose(ctx context.Context, orgID string, conv *domain.Conversation, taskID string) {
	if s.tasks == nil || s.events == nil {
		return
	}
	// TODO(TFAC-1392): an engagement that starts on a task already closed is
	// told nothing here, so a blueprint step minted after the close, and a
	// conversation resumed after it, run without knowing the task is resolved.
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

// handOffQueuedFirings hands a conversation whose engagement is starting the
// events queued on its task as pending firings, each as the note an event
// folded into a running conversation is (domain.AdditiveEventInjection). It
// applies while the conversation's blueprint is still running, to the firings
// queued since that run started: an event that reached the task when no
// conversation was live to take it (between two steps, or while a crash left
// the run between them) queued a firing the claim filter holds until the run
// ends, and the blueprint's events go to the blueprint. Every runtime reads
// staged notes before its agent's first model call of an engagement, so the
// step reads them before it does anything.
//
// The firing is marked with the note and stays queued. This engagement may
// never reach its agent (a bring-up that fails until the claim budget is
// spent, a stop), and the firing is the event's only durable record until
// something has read it. The firing worker settles it once the run ends: as
// injected when the note was read, and otherwise as the ordinary firing it
// then is again.
//
// Three kinds of firing are left as they are. One queued before the run
// started waits for a run of its own, as it did when this run was fired ahead
// of it. One for the event the run itself fired from has nothing to tell the
// run. And nothing is handed over on a task that is closed or snoozed: an
// event queued behind one of those is answered by it, which is the firing
// worker's skip to make.
//
// One event queues a firing per matched trigger; its note is staged once and
// every such firing names it. The note and the mark commit together per
// firing (HandOffToConversation). Best effort: a firing this pass fails on
// stays unmarked, for a later engagement under the run or for the firing
// worker once the run ends.
func (s *Spawner) handOffQueuedFirings(ctx context.Context, orgID string, br *domain.BlueprintRun, conv *domain.Conversation, task *domain.Task) {
	if s.pendingFirings == nil || s.events == nil || br == nil || br.Status != domain.BlueprintRunStatusRunning {
		return
	}
	if task.Status == "done" || task.Status == "dismissed" || task.Status == "snoozed" {
		return
	}
	taskID := task.ID
	firings, err := s.pendingFirings.ListReadyForTask(ctx, orgID, taskID)
	if err != nil {
		delegateLog.Warn("read the task's queued firings failed; the engagement starts without their events", "task_id", taskID, "conversation", conv.ID, "error", err)
		return
	}
	if len(firings) == 0 {
		return
	}
	// The note each event is staged as, by event id: the ones this pass
	// stages, and the ones firings already name. A firing an earlier pass
	// failed on shares the note that pass staged for its event.
	noteFor := map[string]string{}
	for _, f := range firings {
		if f.HandedNoteID != "" {
			noteFor[f.TriggeringEventID] = f.HandedNoteID
		}
	}
	for _, f := range firings {
		if f.HandedNoteID != "" || f.CreatedAt.Before(br.StartedAt) || f.TriggeringEventID == br.TriggeringEventID {
			continue
		}
		var note *domain.StagedInjection
		shared := noteFor[f.TriggeringEventID]
		if shared == "" {
			evt, err := s.events.GetSystem(ctx, orgID, f.TriggeringEventID)
			if err != nil || evt == nil {
				delegateLog.Warn("read a queued firing's event failed; the firing is not handed over", "task_id", taskID, "firing_id", f.ID, "event_id", f.TriggeringEventID, "error", err)
				continue
			}
			// A failed current view hands the agent the metadata as recorded.
			metadataJSON := evt.MetadataJSON
			if current, err := events.Current(ctx, orgID, evt.EventType, metadataJSON); err == nil {
				metadataJSON = current
			}
			note = &domain.StagedInjection{
				ConversationID: conv.ID,
				Subtype:        domain.MessageSubtypeInjectionEvent,
				Producer:       evt.EventType,
				Body:           domain.AdditiveEventInjection(evt.EventType, metadataJSON),
				Provenance:     domain.NoteProvenance{EventID: evt.ID, EventType: evt.EventType},
			}
		}
		noteID, handed, err := s.pendingFirings.HandOffToConversation(ctx, orgID, f.ID, note, shared)
		if err != nil {
			delegateLog.Warn("hand a queued firing to the starting engagement failed; the firing is not handed over", "task_id", taskID, "firing_id", f.ID, "conversation", conv.ID, "error", err)
			continue
		}
		if !handed {
			continue
		}
		noteFor[f.TriggeringEventID] = noteID
		delegateLog.Info("handed a queued firing's event to the starting engagement", "task_id", taskID, "firing_id", f.ID, "event_id", f.TriggeringEventID, "conversation", conv.ID, "blueprint_run", br.ID)
	}
}

// personStarted reports whether a person drives the work on conv: they
// delegated the conversation, which every step of its blueprint inherits, or
// they have written to it. Only a person's message wakes a conversation that
// has run (staged notes ride whatever engagement runs next), so once one has
// written, every engagement after is theirs, and the answer holds before
// their message is delivered and after. A failed read answers false, and the
// engagement is told to wrap up.
func (s *Spawner) personStarted(ctx context.Context, orgID string, conv *domain.Conversation) bool {
	if conv.CreatorUserID != "" {
		return true
	}
	if s.conversations == nil {
		return false
	}
	wrote, err := s.conversations.HasPersonMessageSystem(ctx, orgID, conv.ID)
	if err != nil {
		delegateLog.Warn("read whether a person wrote to the conversation failed; telling it of the held close", "conversation", conv.ID, "error", err)
		return false
	}
	return wrote
}

// stagedInjectionsForEngagement flushes the conversation's durable
// staged-injection queue for an SDK engagement's first message: every
// injection staged while the run was not running. A resume puts the block
// ahead of the person's message, and a launch ahead of its continuation note
// or after its opening (composeLaunchTurn). It returns the bundled
// <system-note> block, "" when nothing is staged or on any read error
// (feedback never blocks an engagement), and the opening-unsent callback
// (agentproc.RunOptions.OnOpeningUnsent) that returns the flushed injections
// to the queue if the message never reaches the agent, nil when nothing was
// flushed.
func (s *Spawner) stagedInjectionsForEngagement(ctx context.Context, orgID, conversationID string) (string, func()) {
	injections := s.flushStagedInjections(ctx, orgID, conversationID)
	if len(injections) == 0 {
		return "", nil
	}
	return domain.StagedInjectionBlock(injections), func() { s.restoreStagedInjections(orgID, conversationID, injections) }
}

// flushStagedInjections claims every pending staged injection on the
// conversation, oldest first, or none when nothing is staged or on any read
// error. The flush marks the rows delivered in the statement that returns
// them, so an injection is delivered exactly once even if two readers race;
// a caller that learns its flush never reached the agent hands them back
// (restoreStagedInjections).
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

// restoreStagedInjections returns flushed injections to the queue, so the
// next flush delivers them: what was to carry them to the agent never did.
// Detached from any caller's context, since it runs as the engagement that
// flushed them fails or ends.
func (s *Spawner) restoreStagedInjections(orgID, conversationID string, injections []domain.StagedInjection) {
	if s.stagedInjections == nil || len(injections) == 0 {
		return
	}
	ids := make([]string, len(injections))
	for i, inj := range injections {
		ids[i] = inj.ID
	}
	ctx, cancel := context.WithTimeout(context.Background(), detachedWriteDeadline)
	defer cancel()
	if err := s.stagedInjections.RestorePendingSystem(ctx, orgID, conversationID, ids); err != nil {
		delegateLog.Warn("returning undelivered notes to the queue failed; the agent does not read them", "conversation", conversationID, "notes", len(ids), "error", err)
	}
}
