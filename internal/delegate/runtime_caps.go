package delegate

import "github.com/sky-ai-eng/triage-factory/internal/domain"

// What each engine needs from the rows around it.
//
// The dispatcher forks on the runtime once; past that the differences are not
// "is this the SDK" but questions about the rows — what undelivered input MEANS,
// and what a resume rebuilds FROM. Each is a named answer rather than a boolean
// so an engine this file has not been taught lands on a value of its own: some
// callers are safe doing nothing to a conversation they do not recognise, others
// safe refusing it, and a boolean would force one polarity on both.

// inputRole says what a conversation's undelivered `messages` rows are for. The
// same rows mean opposite things per engine — the native loop's own input queue,
// or input staged for one resume dispatch — and consuming the first kind drops a
// turn while re-routing the second strands a message.
type inputRole int

const (
	// inputRoleUnknown — untaught engine. Callers leave the rows alone: not
	// touching them is recoverable, consuming or re-routing them is not.
	inputRoleUnknown inputRole = iota
	// inputRoleLiveQueue — the loop reads undelivered rows itself, so writing
	// one IS delivering it and nothing else may flip it delivered.
	inputRoleLiveQueue
	// inputRoleStagedResume — the rows are staged for a resume-by-session-id
	// and mean nothing until that dispatch reads them.
	inputRoleStagedResume
)

func undeliveredRowsFor(runtime string) inputRole {
	switch runtime {
	case domain.ConversationRuntimeNative:
		return inputRoleLiveQueue
	case domain.ConversationRuntimeSDK:
		return inputRoleStagedResume
	}
	return inputRoleUnknown
}

// resumeSource says what a waking conversation is rebuilt from. It decides two
// things that look unrelated and are one fact: which columns must be on the row
// before a wake means anything, and whether a conversation whose workspace is
// gone can be handed a fresh empty one.
type resumeSource int

const (
	// resumeSourceUnknown — untaught engine. Neither affordance extends to it: a
	// fresh workspace is only a continuation for an engine that can replay
	// itself into one.
	resumeSourceUnknown resumeSource = iota
	// resumeSourceMessages — the transcript rows ARE the resume state, replayed
	// into a fresh invocation by the loop. Needs nothing else on the row, and a
	// workspace built from nothing is a real if lossier continuation.
	resumeSourceMessages
	// resumeSourceSession — the engine re-invokes a session BY ID, in the tree
	// it ran in, on the model it started with, so all three have to be stored;
	// without the tree there is nothing to reconnect to.
	resumeSourceSession
)

func resumeSourceFor(runtime string) resumeSource {
	switch runtime {
	case domain.ConversationRuntimeNative:
		return resumeSourceMessages
	case domain.ConversationRuntimeSDK:
		return resumeSourceSession
	}
	return resumeSourceUnknown
}
