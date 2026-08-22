package delegate

import "github.com/sky-ai-eng/triage-factory/internal/domain"

// What the two engines need from the rows around them.
//
// The dispatcher forks on the runtime once, and after that the differences are
// not "is this the SDK" but questions about the rows: what undelivered input
// MEANS, and what a resume rebuilds FROM. Asking them by engine name at each
// site is how six call sites came to spell the same two facts six ways, each
// with its own implied answer for an engine nobody has taught yet.
//
// So each is a named answer rather than a boolean, and the unknown case is a
// value with a name. That is what lets every caller pick the safe arm for
// itself: some sites are safe when they do nothing to an unrecognized
// conversation, others are safe when they refuse it loudly, and a boolean would
// force one polarity on both. An engine added without teaching this file gets
// the zero value everywhere and, at worst, an error naming the place to teach.

// inputRole says what a conversation's undelivered `messages` rows are for.
//
// The same rows carry opposite meanings per engine, which is the whole reason
// this is asked: to the native loop they are the ordinary input queue it drains
// on its own next iteration, and to the SDK they are staged resume input read by
// exactly one thing, the resume dispatch. Consuming the first kind silently
// drops a turn; routing the second kind anywhere else strands a message.
type inputRole int

const (
	// inputRoleUnknown — an engine this file has not been taught. Callers must
	// treat it as "leave the rows alone": not touching them is recoverable,
	// consuming or re-routing them is not.
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

// resumeSource says what a waking conversation is rebuilt from.
//
// It decides two things that look unrelated and are the same fact: which
// columns must be present on the row before a wake means anything, and whether
// a conversation whose workspace is gone can be handed a fresh empty one.
type resumeSource int

const (
	// resumeSourceUnknown — an engine this file has not been taught. Callers
	// must not extend either affordance to it: the stored-state requirements
	// are the SDK's own, and a fresh workspace is only a continuation for an
	// engine that can replay itself into one.
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
