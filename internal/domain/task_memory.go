package domain

import "time"

// MemorySource names who wrote a conversation_memory row's agent_content, and
// is the only thing that tells them apart once the text is in the column:
//
//   - agent — the conversation's own agent wrote its memory file and the
//     orchestrator ingested it. The authoritative account of what was tried.
//   - generated — TF composed the memory from the transcript because the agent
//     left none. Useful, but second-hand, and overwritten the moment an agent
//     conclusion arrives for the same conversation.
//   - none — nothing was remembered. agent_content is NULL and the row exists
//     only to record that the question was settled.
//
// Stored as text and validated at the store door rather than by a CHECK, the
// `type`/`origin` pattern. The invariant the door enforces: agent_content IS
// NULL exactly when source = 'none'.
type MemorySource string

const (
	MemorySourceAgent     MemorySource = "agent"
	MemorySourceGenerated MemorySource = "generated"
	MemorySourceNone      MemorySource = "none"
)

// AllMemorySources returns the source vocabulary. Same discipline as
// AllParkReasons: SQL cannot import a Go const, so the dual-dialect conformance
// suite derives its coverage from this set and a source added here but never
// taught to a store fails on both backends.
func AllMemorySources() []MemorySource {
	return []MemorySource{MemorySourceAgent, MemorySourceGenerated, MemorySourceNone}
}

// IsMemorySource reports whether source names a memory author. Closed-world:
// the empty string and anything unrecognized are not sources, which is what
// lets the store door refuse a write rather than canonicalize one.
func IsMemorySource(source string) bool {
	switch MemorySource(source) {
	case MemorySourceAgent, MemorySourceGenerated, MemorySourceNone:
		return true
	}
	return false
}

// TaskMemory is a durable per-conversation narrative of what an agent tried on a task
// and why. The agent writes it to the one fixed path `./_tfac/memory.md` in
// its run root; the orchestrator reads it at termination and ingests it into
// the `conversation_memory` table before worktree teardown. Materialized back
// into future conversations' worktrees under `_tfac/entity-memory/` so iterations on
// the same entity can read what prior attempts tried — and so the sibling steps
// of one blueprint run can read each other's memory as their handoff.
//
// Which entities a row is reachable from is the `conversation_memory_entities`
// join, not a column here. Stored in the `conversation_memory` table with a
// denormalized `blueprint_run_id` that groups one blueprint run's files.
type TaskMemory struct {
	ID             string
	ConversationID string
	BlueprintRunID string // denormalized from the conversation; empty for a standalone (non-blueprint) conversation
	Content        string
	Source         MemorySource
	CreatedAt      time.Time

	// StepIndex and PromptName come from the producing conversation, not the
	// memory row: they are what lets a materializer name the file after the
	// work it records — "02-implement.md" rather than an opaque id. Both are
	// zero-valued when the conversation carries no step index / prompt (or the
	// reader's visibility can't see it), so every consumer must tolerate their
	// absence.
	StepIndex  *int   // 0-based blueprint step index of the producing conversation
	PromptName string // display name of the prompt that conversation ran
}

// MemoryAttemptOutcome is what one memory-generation attempt came to — the
// closed vocabulary of the `outcome` column on conversation_memory_attempts.
// NULL (the empty string here) while the attempt is still running, and left
// NULL forever by an attempt whose brain died mid-generation: an attempt with
// no outcome is not a failure, it is an attempt nobody closed out.
type MemoryAttemptOutcome string

const (
	// MemoryAttemptGenerated — the attempt produced a memory and filed it.
	MemoryAttemptGenerated MemoryAttemptOutcome = "generated"
	// MemoryAttemptEmpty — the attempt ran and concluded there was nothing
	// worth remembering. Distinct from a failure: nothing went wrong, and a
	// retry would reach the same answer over the same transcript.
	MemoryAttemptEmpty MemoryAttemptOutcome = "empty"
	// MemoryAttemptFailed — the attempt could not run to an answer. The row's
	// error_kind says what stopped it.
	MemoryAttemptFailed MemoryAttemptOutcome = "failed"
)

// AllMemoryAttemptOutcomes returns the outcome vocabulary. Same discipline as
// AllParkReasons: SQL cannot import a Go const, so the dual-dialect
// conformance suite derives its coverage from this set and an outcome added
// here but never taught to a store fails on both backends.
func AllMemoryAttemptOutcomes() []MemoryAttemptOutcome {
	return []MemoryAttemptOutcome{
		MemoryAttemptGenerated,
		MemoryAttemptEmpty,
		MemoryAttemptFailed,
	}
}

// IsMemoryAttemptOutcome reports whether outcome names a completed attempt's
// answer. Closed-world, like IsParkReason: the empty string is NOT an outcome
// — it is the running (or abandoned) attempt's absence of one, which is why
// the store door refuses it rather than storing it as a verdict.
func IsMemoryAttemptOutcome(outcome string) bool {
	switch MemoryAttemptOutcome(outcome) {
	case MemoryAttemptGenerated, MemoryAttemptEmpty, MemoryAttemptFailed:
		return true
	}
	return false
}

// MemoryAttemptErrorKind is why a failed attempt failed — the closed
// vocabulary of the `error_kind` column. Set exactly when the outcome is
// MemoryAttemptFailed, so the pair is a biconditional the store door enforces
// rather than a convention each writer has to remember.
type MemoryAttemptErrorKind string

const (
	// MemoryAttemptErrNoModel — the org's background-jobs model is unset, not
	// offered by this build, or served by a provider the org has not
	// connected. Nothing was bought and nothing will be until a person picks
	// a model.
	MemoryAttemptErrNoModel MemoryAttemptErrorKind = "no_model"
	// MemoryAttemptErrProviderBackoff — the provider asked us to slow down.
	// Its own kind because it is the one failure a later attempt is expected
	// to walk straight past.
	MemoryAttemptErrProviderBackoff MemoryAttemptErrorKind = "provider_backoff"
	// MemoryAttemptErrProviderError — the provider answered, and the answer
	// was an error.
	MemoryAttemptErrProviderError MemoryAttemptErrorKind = "provider_error"
	// MemoryAttemptErrTimeout — the call did not answer inside the budget.
	MemoryAttemptErrTimeout MemoryAttemptErrorKind = "timeout"
	// MemoryAttemptErrOther — anything else. A catch-all rather than a
	// growing enum: the kinds above exist because a reader does something
	// different with each, and a kind nobody acts on differently earns
	// nothing over the error message beside it.
	MemoryAttemptErrOther MemoryAttemptErrorKind = "other"
)

// AllMemoryAttemptErrorKinds returns the error_kind vocabulary, under the same
// rule as AllMemoryAttemptOutcomes.
func AllMemoryAttemptErrorKinds() []MemoryAttemptErrorKind {
	return []MemoryAttemptErrorKind{
		MemoryAttemptErrNoModel,
		MemoryAttemptErrProviderBackoff,
		MemoryAttemptErrProviderError,
		MemoryAttemptErrTimeout,
		MemoryAttemptErrOther,
	}
}

// IsMemoryAttemptErrorKind reports whether kind names a failure. Closed-world:
// the empty string is NOT a kind — it is what every non-failed attempt
// carries.
func IsMemoryAttemptErrorKind(kind string) bool {
	switch MemoryAttemptErrorKind(kind) {
	case MemoryAttemptErrNoModel, MemoryAttemptErrProviderBackoff,
		MemoryAttemptErrProviderError, MemoryAttemptErrTimeout, MemoryAttemptErrOther:
		return true
	}
	return false
}

// MemoryAttempt is one try at generating a memory for a conversation that
// ended owing one — the conversation_memory_attempts ledger. It exists
// because the memory row alone cannot distinguish "nothing was owed" from
// "something was owed and generation failed", and a reader who cannot tell
// those apart either retries forever or gives up silently.
//
// One row per attempt, never updated in place except to close it out:
// BeginAttempt writes the started row and CompleteAttempt stamps the verdict
// under a `completed_at IS NULL` CAS, so an attempt is closed out exactly
// once and a second closer is told so rather than overwriting the first.
//
// The window counters record how much of the transcript the attempt actually
// fed the model — WindowRowsTotal is what the conversation had, and
// WindowRowsSent what fit the budget — so a thin memory can be read as a
// truncated window rather than as a model that had nothing to say.
type MemoryAttempt struct {
	ID             string
	OrgID          string
	ConversationID string
	StartedAt      time.Time
	// CompletedAt is nil while the attempt runs, and stays nil for an attempt
	// whose brain died before it could close out. No sweeper rewrites it: a
	// row that never completed is the honest record of a brain that stopped.
	CompletedAt *time.Time
	// Outcome is empty exactly when CompletedAt is nil.
	Outcome MemoryAttemptOutcome
	// ErrorKind is set exactly when Outcome is MemoryAttemptFailed.
	ErrorKind MemoryAttemptErrorKind
	// ErrorMessage is TF's own wording of the failure, never an upstream
	// response body: the row is read by a person looking at a task, and a
	// provider's JSON is neither theirs to read nor ours to republish.
	ErrorMessage string
	// SystemLLMRunID links the attempt to its row in the system_llm_runs
	// spend ledger, or is empty when no ledger row landed — that insert is
	// best-effort, so the link is nullable and a dangling id stores as NULL
	// rather than failing the attempt's own record.
	SystemLLMRunID  string
	WindowRowsTotal int
	WindowRowsSent  int
}
