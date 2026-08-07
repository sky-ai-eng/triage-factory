package agentloop

import "fmt"

// The turn-end contract: what a provider's stop reason means, and what the
// transcript says when the loop will not act on it.
//
// The classification is an ALLOWLIST. Only an end-of-turn stop may conclude a
// conversation; every other reason takes a named arm. The inversion is
// load-bearing because the neutral layer does not normalize every reason it
// receives — the Anthropic map covers five of the eight stop reasons its own
// constants declare, the Bedrock map is the same shape, and both pass
// everything else through verbatim. Under a conclude-by-default rule a
// refusal, a reply the window cut in half, and a reason nobody has classified
// yet all read as "the model finished", and a blueprint advances on nothing.
//
// So the raw provider spellings sit beside the neutral ones in the same
// predicates. A fork-pin refresh that starts normalizing one of them must not
// silently reclassify it — which is why both spellings of the window wall are
// matched, and why the table test pins both.
type stopClass int

const (
	// stopEndTurn is the one class that may conclude: the model finished
	// saying what it had to say.
	stopEndTurn stopClass = iota
	// stopToolCalls is the model asking for tools — the dispatch arm.
	stopToolCalls
	// stopLength is the completion budget running out mid-generation.
	stopLength
	// stopWindowWall is the context window itself running out mid-generation:
	// input fit, input plus the cap did not.
	stopWindowWall
	// stopDecline is the model or the provider declining to produce the
	// reply at all — a refusal, a content filter, a guardrail.
	stopDecline
	// stopUnknown is every reason this build does not recognize, including
	// the ones no TF configuration can reach today (a server-tool pause, a
	// server-side compaction) — which is exactly why they need no arm of
	// their own.
	stopUnknown
)

// classifyStop maps a finish reason as it actually arrives to its arm.
func classifyStop(reason string) stopClass {
	switch reason {
	// An empty reason is a stream that ended without reporting one. Read as
	// end-of-turn: conservative, and what every reason got before this
	// classification existed. The persisted finish_reason metadata is how the
	// real distribution gets observed before anything is decided from it.
	case "", "stop":
		return stopEndTurn
	case "tool_calls":
		return stopToolCalls
	case "length", "max_tokens":
		return stopLength
	// The first is the raw Anthropic and Bedrock literal, which is what
	// arrives today. The second is the spelling a normalizing fork would most
	// plausibly land on, since normalization drops the provider-flavored
	// prefix; both name the same event, and matching both is what keeps a pin
	// refresh from turning a wall into an unknown.
	case "model_context_window_exceeded", "context_window_exceeded":
		return stopWindowWall
	// refusal is Anthropic's, guardrail_intervened is Bedrock's raw
	// passthrough, content_filter is what the Bedrock map makes of
	// content_filtered.
	case "refusal", "content_filter", "guardrail_intervened":
		return stopDecline
	default:
		return stopUnknown
	}
}

// interrupted reports a stop that ended the message before the model chose to
// end it — a limit cut it short, or the provider took it away. The classes
// differ entirely in remedy and not at all in the state they leave behind,
// which is why two rules key on this rather than on the class:
//
//   - the strict tool-argument parse is relaxed, because the stream may have
//     stopped mid-JSON and the fragments cannot form a whole document;
//   - nothing in the message is dispatched, so every call it carries is
//     answered in-band instead.
//
// A message the model finished gets neither. With nothing to blame for the
// damage, malformed arguments there are a provider bug and stay a loud
// failure. An unclassified stop counts as interrupted for the same reason it
// parks rather than concludes: a reason nobody has classified is not evidence
// that anything completed.
func (c stopClass) interrupted() bool {
	switch c {
	case stopLength, stopWindowWall, stopDecline, stopUnknown:
		return true
	default:
		return false
	}
}

// truncatedBatchNotice is the instructive result every call in a
// length-truncated assistant message receives. It names the cause and the
// remedy, because the model cannot otherwise tell a truncated argument
// object from one it wrote badly.
const truncatedBatchNotice = "This tool call was not executed: the model's response hit the output length limit, " +
	"so the tool arguments in that message may be silently truncated and are not safe to run. " +
	"Re-issue the call you intended, with smaller arguments or fewer calls in one message."

// wallCutBatchNotice is the same answer for a message the context window cut
// short. Its remedy differs: there is no smaller version of this call that
// helps, because the obstacle is the history in front of it, and the
// compaction about to run is what makes room.
const wallCutBatchNotice = "This tool call was not executed: the model's response ran out of context window " +
	"mid-message, so the tool arguments in that message may be silently truncated and are not safe to run. " +
	"The conversation is being compacted to make room; re-issue the call you intended after that."

// interruptedCallNotice answers a tool call in a message the provider ended
// before the loop could run it — a decline, or a stop nobody has classified.
// It promises less than the two above because less is known: the call did not
// run, and whether re-issuing it is even the right move depends on what a
// person makes of the park it arrived with.
const interruptedCallNotice = "This tool call was not executed: the provider ended the reply that contained it " +
	"before the call could run, and this run is paused for a person to look at. Nothing from this call happened, " +
	"and nothing it would have changed was changed."

// maxEmptyLengthStops bounds consecutive turns cut at the output limit that
// produced nothing. Two: the first buys a notice and a larger cap, and if the
// larger cap produces nothing either, a third identical call is not a
// different experiment.
const maxEmptyLengthStops = 2

// maxWindowWallStops bounds consecutive turns cut at the context window wall.
// Two, for the same reason and a sharper one: the first compacts, and a wall
// on the very next call means the compacted history still leaves no room to
// answer in. There is no third, smaller history to try.
const maxWindowWallStops = 2

// outputLimitNotice tells the model what the transcript cannot show it — the
// turn it just took is not in the window, because the whole cap went to
// reasoning and nothing replayable came out. Without this the model sees its
// own message vanish and has no account of why.
const outputLimitNotice = "<system-note>\n" +
	"Your last reply reached the output token limit while still reasoning, before it produced any text " +
	"or any tool call, so nothing from it was recorded and no work happened. That turn is not in this " +
	"conversation — this note is what took its place.\n" +
	"Continue from where you were, and get to the action sooner: reason more briefly and take the next " +
	"concrete step in this reply rather than planning the whole remainder first.\n" +
	"</system-note>"

// outputLimitFailureNotice is the transcript's record of the engagement
// ending on the second such turn. It names the cap, because the number is
// what an operator needs to decide whether to raise a budget or shorten the
// work.
func outputLimitFailureNotice(maxTokens int) string {
	return fmt.Sprintf("<system-note>\nThis run stopped: two consecutive model replies reached the %d-token output "+
		"limit while still reasoning, producing no text and no tool call either time. The work so far is intact — "+
		"send a message to pick it back up.\n</system-note>", maxTokens)
}

// windowWallFailureNotice records the engagement ending on a second wall.
// Unlike the output limit there is no number worth naming: the cap is not the
// obstacle and raising it makes the wall arrive sooner, so what an operator
// needs to know is that compaction already ran and did not help.
const windowWallFailureNotice = "<system-note>\nThis run stopped: the model ran out of context window mid-reply twice " +
	"in a row, the second time on the call immediately after this conversation was compacted — so there is no " +
	"smaller history left to retry against. The work so far is intact — send a message to pick it back up.\n" +
	"</system-note>"

// declineParkNotice records a provider declining to produce the reply.
//
// There is deliberately no automatic retry beneath this: the same request
// against a refusal is refused again, and whether the ask should change is a
// person's call, not a loop's. So the run parks where a human can see it and
// the next message wakes it.
func declineParkNotice(reason string) string {
	return fmt.Sprintf("<system-note>\nThis run paused: the provider stopped the last reply because %s "+
		"(stop reason %q). It was not retried — an identical request would be declined again. The work so far is "+
		"intact; send a message to pick it back up.\n</system-note>", declineCause(reason), reason)
}

// declineCause says which decline it was in the words a reader thinks in. The
// three are not interchangeable to whoever has to decide what to do next: a
// model declining, a filter on the output, and an account-level guardrail
// have different remedies and different people to talk to.
func declineCause(reason string) string {
	switch reason {
	case "refusal":
		return "the model declined to produce it"
	case "content_filter":
		return "a content filter blocked it"
	case "guardrail_intervened":
		return "a provider guardrail blocked it"
	default:
		return "the provider blocked it"
	}
}

// unrecognizedStopParkNotice names the raw string, which is the whole point of
// it: the reason is unrecognized here, so the transcript has to carry the
// thing itself for whoever classifies it next.
func unrecognizedStopParkNotice(reason string) string {
	return fmt.Sprintf("<system-note>\nThis run paused: the provider ended the last reply with a stop reason this "+
		"build does not recognize (%q), so it was not read as a finished turn. The work so far is intact; "+
		"send a message to pick it back up.\n</system-note>", reason)
}

// emptyToolBatchParkNotice covers the provider contradicting itself: it
// reported stopping to call a tool and then called none. Nothing ran, and a
// turn that asked for nothing is not a finished turn either.
const emptyToolBatchParkNotice = "<system-note>\nThis run paused: the provider reported that the last reply stopped " +
	"to call a tool, and the reply contained no tool call. Nothing was run, and nothing was concluded from it. " +
	"The work so far is intact; send a message to pick it back up.\n</system-note>"
