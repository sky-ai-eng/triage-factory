package domain

import (
	"strings"
	"time"
)

// StagedInjection is one durably-queued agent-facing injection awaiting
// delivery on a conversation's next resume/steer. It is the "terminal ledger"
// half of the artifact-sidecar
// feedback design (TFAC-493) made generic (TFAC-501): a producer that has no
// durable row to re-derive its injection from (the new-commits notifier is the first
// such producer) stages the injection here, and the spawner flushes the conversation's pending
// injections ahead of the user's text on the next ResumeWithMessage.
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
}

// StagedInjectionProducer* tag a staged injection's origin. Free-text (no DB CHECK) so a
// new producer adds a const without a migration; used for audit/debugging and a
// future per-producer flush policy, never for control flow today.
const (
	// StagedInjectionProducerPRNewCommits — the head-SHA-change notifier (TFAC-501):
	// a reviewed PR advanced under an in-progress/parked review conversation.
	StagedInjectionProducerPRNewCommits = "pr_new_commits"
)

// StagedInjectionBlock renders the bundled <system-note> block the resume path
// prepends ahead of a parked conversation's user message: every staged
// injection in the slice, one
// bullet each, in the order given (the caller passes them oldest→newest, the
// order the producers acted). Returns "" when the slice is empty, so a resume with
// nothing staged prepends nothing.
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
		"\n\nThese are informational — act on them as you resume."
	return WrapSystemNote(body)
}
