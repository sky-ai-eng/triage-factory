package domain

import (
	"strings"
	"time"
)

// StagedInjection is one durably-queued agent-facing injection awaiting
// delivery on a conversation's next resume/steer. It is the "terminal ledger"
// half of the artifact-sidecar feedback design (TFAC-493) made generic
// (TFAC-501): a producer that has no durable row to re-derive its injection
// from (the PR coherence feed is such a producer) stages the
// injection here, and the spawner flushes the conversation's pending injections
// ahead of the user's text on the next ResumeWithMessage.
//
// Producer-agnostic by construction: Body is the already-rendered, bare injection
// line (no <system-note> wrapper — the flush bundles and wraps), and Producer is
// a free-text tag kept only for audit/observability (StagedInjectionProducer*). The
// queue does not interpret either, so a new consumer adds a producer tag and a
// body string without touching the store.
type StagedInjection struct {
	ID             string    `json:"id"`
	OrgID          string    `json:"org_id"`
	ConversationID string    `json:"conversation_id"`
	Producer       string    `json:"producer"`
	Body           string    `json:"body"`
	CreatedAt      time.Time `json:"created_at"`
	// Provenance names the event this note was composed from, for a reader
	// that wants to say why the agent was told something. Zero for a note
	// with no originating event.
	Provenance NoteProvenance `json:"provenance,omitzero"`
}

// NoteProvenance names the event an agent-facing note was composed from. It is
// recorded on the note's message row rather than derived from the body, so a
// transcript view or an audit can resolve a note back to its cause without
// re-parsing prose that was written for an agent to read.
//
// Deliberately not a foreign key. The transcript is the durable record of what
// the agent was told, and that stays true whether or not the event row still
// exists — an FK would force a purge of events to either block or take
// transcript rows with it. An id that no longer resolves is the better
// failure, the same trade artifacts make by outliving the run that produced
// them.
//
// The zero value is valid and means "no originating event": a human resolving
// an artifact produces a note nothing external caused.
type NoteProvenance struct {
	EventID   string `json:"event_id,omitempty"`
	EventType string `json:"event_type,omitempty"`
}

// Empty reports whether no originating event was recorded.
func (p NoteProvenance) Empty() bool { return p.EventID == "" && p.EventType == "" }

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
	return meta
}

// PRCoherenceTarget is one delegation conversation that is working on a PR
// entity or has materialized that PR's checkout/head branch. Active is derived
// from an unreleased claim; Status and Outcome are the stored conversation
// lifecycle fields used to decide whether a non-live note can ever flush.
type PRCoherenceTarget struct {
	ConversationID string
	TaskID         string
	Status         string
	Outcome        string
	Active         bool
}

// StagedInjectionProducer* tag a staged injection's origin. Free-text (no DB CHECK) so a
// new producer adds a const without a migration; used for audit/debugging and a
// future per-producer flush policy, never for control flow today.
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
)

// NoteProducerArtifactResolved tags the note telling an agent that a human
// resolved an artifact it produced. It shares the producer vocabulary above
// but never reaches the staged queue — the resume path re-derives that note
// from artifact state instead (artifactLedgerForResume) — so it appears only
// on a live-delivered note's message row.
const NoteProducerArtifactResolved = "artifact_resolved"

// StagedInjectionBlock renders the bundled <system-note> block the resume path
// prepends ahead of a parked conversation's user message: every staged
// injection in the slice, one bullet each, in the order given (the caller
// passes them oldest→newest, the order the producers acted). Returns "" when
// the slice is empty, so a resume with nothing staged prepends nothing.
//
// The lead-in frames these as out-of-band updates that landed while the agent was
// not running — the same shape ArtifactLedgerBlock uses for the artifact-change
// ledger, so the two prepended blocks read consistently when a resume carries
// both.
func StagedInjectionBlock(injections []StagedInjection) string {
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
	body := "While you were not running, these updates landed on work you have open:\n\n" +
		strings.Join(lines, "\n") +
		"\n\nThese are informational — act only if they affect your work."
	return WrapSystemNote(body)
}
