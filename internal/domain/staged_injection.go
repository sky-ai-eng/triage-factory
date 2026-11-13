package domain

import (
	"slices"
	"strings"
	"time"
)

// StagedInjection is one durably-queued agent-facing injection awaiting
// delivery to a conversation's agent. Every producer of a note (the PR
// coherence feed, a task's close, a task's new assignee, a deliverable's
// resolution) stages it here, and each runtime reads the conversation's
// pending injections on its own schedule: a native loop before its next
// model call, a live SDK process at its next tool batch, and an SDK
// engagement in its first message.
//
// Producer-agnostic by construction: Body is the already-rendered, bare injection
// line (no <system-note> wrapper — the flush bundles and wraps), and Producer is
// a free-text tag kept only for audit/observability (StagedInjectionProducer*). The
// queue does not interpret either, so a new consumer adds a producer tag and a
// body string without touching the store.
type StagedInjection struct {
	ID             string `json:"id"`
	OrgID          string `json:"org_id"`
	ConversationID string `json:"conversation_id"`
	// Subtype is the message subtype the row is stored under, and says what
	// the note is for: MessageSubtypeInjectionEvent for a new-event
	// injection, MessageSubtypeInjectionSystemNote for every other note. The
	// store refuses any other value.
	Subtype   string    `json:"subtype"`
	Producer  string    `json:"producer"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"created_at"`
	// Provenance names the event this note was composed from, for a reader
	// that wants to say why the agent was told something. Zero for a note
	// with no originating event.
	Provenance NoteProvenance `json:"provenance,omitzero"`
}

// NoteProvenance names what an agent-facing note was composed from: the event
// behind it, for a close note the close it tells of, and for an assignment
// note the task and who took it on. It is
// recorded on the note's message row rather than derived from the body, so a
// transcript view or an audit can resolve a note back to its cause without
// re-parsing prose that was written for an agent to read.
//
// Deliberately not a foreign key. The transcript is the durable record of what
// the agent was told, and that stays true whether or not the event row still
// exists — an FK would force a purge of events to either block or take
// transcript rows with it. An id that no longer resolves is the better
// failure, the same trade deliverables make by outliving the run that produced
// them.
//
// The zero value is valid and means "no originating event": a human resolving
// a deliverable produces a note nothing external caused.
type NoteProvenance struct {
	EventID   string `json:"event_id,omitempty"`
	EventType string `json:"event_type,omitempty"`
	// TaskClose is the close of the conversation's task a close note tells
	// of (TaskCloseKey), or the held close (TaskCloseHoldKey). A person's
	// close has no event, so this, not EventID, is what says a conversation
	// has been told.
	TaskClose string `json:"task_close,omitempty"`
	// AssignedTask is the task an assignment note (TaskAssignedNote) tells
	// of, by id, and Assignee is who the note says holds it (AgentAssignee,
	// UserAssignee). A note is true while the task is open under that
	// assignee, which is what retires it once it is not
	// (StagedInjectionStore.RetireStaleAssignmentNotesSystem).
	AssignedTask string `json:"assigned_task,omitempty"`
	Assignee     string `json:"assignee,omitempty"`
}

// AgentAssignee and UserAssignee spell who holds a task in an assignment
// note's provenance. Both dialects' retire compares the stored value with the
// task's claim columns, so the spelling is the stored rows' as well.
func AgentAssignee(agentID string) string { return "agent:" + agentID }

// UserAssignee is AgentAssignee for a person.
func UserAssignee(userID string) string { return "user:" + userID }

// Empty reports whether nothing was recorded.
func (p NoteProvenance) Empty() bool {
	return p.EventID == "" && p.EventType == "" && p.TaskClose == "" && p.AssignedTask == "" && p.Assignee == ""
}

// NoteMetadata builds the message-row metadata an agent-facing note carries.
// Both delivery paths compose it here — the live steer writes it straight onto
// the message row, the staged queue marshals it into the row it inserts — so a
// note's provenance reads the same whether the agent was awake when it landed.
func NoteMetadata(producer string, prov NoteProvenance) map[string]any {
	meta := map[string]any{"producer": producer}
	if prov.EventID != "" {
		meta["event_id"] = prov.EventID
	}
	if prov.EventType != "" {
		meta["event_type"] = prov.EventType
	}
	if prov.TaskClose != "" {
		meta["task_close"] = prov.TaskClose
	}
	if prov.AssignedTask != "" {
		meta["assigned_task"] = prov.AssignedTask
	}
	if prov.Assignee != "" {
		meta["assignee"] = prov.Assignee
	}
	return meta
}

// PRCoherenceTarget is one delegation conversation that is working on a PR
// entity or has materialized that PR's checkout/head branch. Active is derived
// from an unreleased claim; Status, Concluded and Outcome are the stored
// conversation lifecycle fields used to decide whether a non-live note can ever
// flush. Concluded is Conversation.Concluded read off the stored row.
type PRCoherenceTarget struct {
	ConversationID string
	TaskID         string
	Status         string
	Concluded      bool
	Outcome        string
	Active         bool
}

// StagedInjectionSubtypes is every subtype a staged injection may be stored
// under: the set the store writes, flushes and withdraws.
var StagedInjectionSubtypes = []string{MessageSubtypeInjectionEvent, MessageSubtypeInjectionSystemNote}

// IsStagedInjectionSubtype reports whether subtype is one a staged injection
// may be stored under.
func IsStagedInjectionSubtype(subtype string) bool {
	return slices.Contains(StagedInjectionSubtypes, subtype)
}

// StagedInjectionProducer* tag a staged injection's origin. Free-text (no DB CHECK) so a
// new producer adds a const without a migration; used for audit/debugging, and
// the close notes' tag is what their withdrawal matches, so renaming one
// strands the rows already staged under the old value.
const (
	// StagedInjectionProducerPRNewCommits tags a head-SHA-change note. Nothing
	// writes it — the PR coherence feed carries head changes under its own tag
	// — but staged rows persist across an upgrade, so a queue being read today
	// may still hold one.
	StagedInjectionProducerPRNewCommits = "pr_new_commits"
	// StagedInjectionProducerPRCoherence is the entity-level PR state feed.
	// The event type remains in the rendered body; one producer tag keeps the
	// queue vocabulary independent of the four event kinds it carries.
	StagedInjectionProducerPRCoherence = "pr_coherence"
	// StagedInjectionProducerTaskClosing is the note telling a conversation
	// that its task closed (TaskClosedNote), or that its close is held for a
	// person (TaskCloseHeldNote). A conversation is told once per close and
	// once per hold (StagedInjectionStore.HasNoteForTaskCloseSystem). A
	// reopen of the task, and a hold cleared without its close, withdraw the
	// notes still waiting (TaskStore.Bump).
	StagedInjectionProducerTaskClosing = "task_closing"
	// StagedInjectionProducerDeliverableResolved is the note telling an agent
	// that a deliverable it produced was resolved (DeliverableResolutionNote).
	// Stored message rows carry the value, so it keeps its spelling.
	StagedInjectionProducerDeliverableResolved = "artifact_resolved"
	// StagedInjectionProducerTaskAssigned is the note telling a conversation
	// that someone took on another task its work overlaps (TaskAssignedNote).
	// One is staged per new assignee of the task, and it carries the task and
	// the assignee in its provenance. A note still waiting when its task
	// closes, changes hands, or is told of again is retired before a turn
	// reads it (StagedInjectionStore.RetireStaleAssignmentNotesSystem), which
	// matches on this tag.
	StagedInjectionProducerTaskAssigned = "task_assigned"
	// StagedInjectionProducerSiblingWork is the note listing the work that
	// overlaps a conversation (SiblingWorkNote), staged when a follow-up
	// wakes a conversation that had concluded: nothing told it of an
	// assignment while it sat, and its <run_context> is not rebuilt.
	StagedInjectionProducerSiblingWork = "sibling_work"
)

// StagedInjectionBlock renders the bundled <system-note> block the resume path
// prepends ahead of a parked conversation's user message: every staged
// injection in the slice, one bullet each, in the order given (the caller
// passes them oldest→newest, the order the producers acted). Returns "" when
// the slice is empty, so a resume with nothing staged prepends nothing.
//
// The lead-in frames these as out-of-band updates that landed while the agent was
// not running.
func StagedInjectionBlock(injections []StagedInjection) string {
	return stagedInjectionBlock("While you were not running, these updates landed on work you have open:", injections)
}

// StagedInjectionBoundaryBlock is StagedInjectionBlock for notes delivered
// into a run that is still working, at a tool boundary, where they landed
// while it worked rather than while it was away.
func StagedInjectionBoundaryBlock(injections []StagedInjection) string {
	return stagedInjectionBlock("While you were working, these updates landed on work you have open:", injections)
}

func stagedInjectionBlock(leadIn string, injections []StagedInjection) string {
	lines := make([]string, 0, len(injections))
	for i := range injections {
		if injections[i].Body == "" {
			continue
		}
		lines = append(lines, "- "+injections[i].Body)
	}
	if len(lines) == 0 {
		return ""
	}
	body := leadIn + "\n\n" +
		strings.Join(lines, "\n") +
		"\n\nEach update says what happened. Where one also says what to do about it, follow that; otherwise act on it only if it affects your work."
	return WrapSystemNote(body)
}
