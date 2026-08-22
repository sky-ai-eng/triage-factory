package modelprobe

import (
	"context"
	"fmt"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/inference"
)

// Verdict is what one probe concluded. Two of the three are stored; the third
// deliberately is not.
type Verdict string

const (
	// VerdictGreen — the model answered. Nothing else needs to be true: a
	// one-token completion that returns is proof the credential can invoke
	// this id, which is the entire question.
	VerdictGreen Verdict = "green"
	// VerdictRed — the provider ANSWERED and refused, and its answer was about
	// this credential's right to invoke this model.
	VerdictRed Verdict = "red"
	// VerdictInconclusive — nobody answered the question. A 5xx, a rate limit,
	// a timeout, a dial failure, a cancelled request. It writes NOTHING, which
	// is the whole point: one bad provider minute must never be able to block
	// a save or hide a model that works.
	VerdictInconclusive Verdict = "inconclusive"
)

// refusalStatuses are the HTTP statuses that mean "this credential may not
// invoke this model": authentication rejected, entitlement denied, and the id
// not existing for this account. A model-not-found is a refusal rather than a
// gap because the question the probe asks is not "does this model exist
// somewhere" but "can this credential invoke this exact string" — and the
// answer to that is no, permanently, until something about the account changes.
var refusalStatuses = map[int]bool{401: true, 403: true, 404: true}

// inconclusiveStatuses are the statuses that say nothing about entitlement:
// the provider is overloaded, throttling, or broken. They are listed rather
// than derived as "everything else" because they must beat the refusal markers
// below — a 503 whose body quotes an earlier AccessDeniedException is a bad
// minute, not a grant being revoked.
func inconclusiveStatus(status int) bool {
	return status == 408 || status == 409 || status == 429 || status >= 500
}

// refusalMarkers are the provider spellings of a refusal that can arrive with
// no HTTP status attached — a mid-stream error chunk, or a vendor SDK error
// rendered without one — plus the Bedrock ValidationException whose 400 says
// the id itself is not usable. Matched case-insensitively against the
// flattened provider error.
//
// Kept deliberately short. A marker that over-matches paints a working model
// red on the strength of a substring, and red is the state a user has to spend
// money to clear; an under-matching list costs an inconclusive, which is
// self-healing on the next test.
var refusalMarkers = []string{
	"accessdeniedexception",
	"not_found_error",
	"model not found",
	"the provided model identifier is invalid",
}

// detailLimit bounds the provider message a verdict carries. The string is
// whatever the upstream chose to return; it is stored on the row and published
// on every catalog read, so it is bounded here rather than left to a provider
// that answers a refusal with a page of HTML. The cut is generous enough that
// every real refusal any vendor sends survives whole.
const detailLimit = 1000

// truncate bounds a provider message and says so when it cuts, so a reader
// never mistakes a clipped message for the whole of what the provider said.
func truncate(msg string) string {
	if len(msg) <= detailLimit {
		return msg
	}
	return msg[:detailLimit] + "… (truncated)"
}

// classify sorts a probe's outcome into the three verdicts.
//
// The discipline throughout is that only an ANSWER counts. A refusal is the
// provider telling us something about this credential; anything else — even a
// definite-sounding failure — is TF failing to ask the question, and writes no
// row. The asymmetry is deliberate: an inconclusive costs a retry, while a
// wrong red costs a real request to undo and, in the meantime, tells an admin
// their credentials lack access they actually have.
//
// A rendered status settles the question when there is one, in both directions
// (a 500 stays inconclusive however its body reads), and the markers are
// consulted only for the failures that carry no status and for the 400 a
// Bedrock id error arrives as. That mirrors how the system-job breaker reads
// the same rendered text — same marker, same precedence — because the two are
// classifying the same string for different questions and disagreeing about
// what "the provider answered 503" means would be a bug in one of them.
func classify(callCtx context.Context, err error) (Verdict, string) {
	if err == nil {
		return VerdictGreen, ""
	}
	status, ok := inference.RenderedStatus(err)
	return classifyFailure(callCtx, status, ok, err.Error())
}

// classifyFailure sorts one attempt that did NOT succeed, given whatever the
// transport managed to say about it: the provider's HTTP status when there is
// one (hasStatus false means nobody reported a number, not status 0) and the
// flattened message.
//
// Both probe paths land here, which is the point: the direct call reads a
// status off the rendered provider error, the SDK subprocess reads it off the
// terminal result event's api_error_status, and the two arrive at the same
// verdict for the same number. A refusal is a refusal whichever transport
// carried the answer, so the status sets and the marker list are not
// duplicated per path.
//
// callCtx is the ctx the ATTEMPT ran under, not the caller's. It is derived
// from the caller's, so a non-nil Err covers both endings that are TF's own
// clock rather than the provider's answer: the caller navigating away
// mid-sweep, and the per-probe timeout expiring. Checking the caller's ctx
// instead would miss the second — the outer one is still healthy when a probe
// times out. It is checked before the status because a transport can report
// one on the way down from a cancellation, and TF's own clock ending the
// attempt is not the provider answering.
func classifyFailure(callCtx context.Context, status int, hasStatus bool, message string) (Verdict, string) {
	if callCtx.Err() != nil {
		return VerdictInconclusive, truncate(message)
	}
	if hasStatus {
		switch {
		case refusalStatuses[status]:
			return VerdictRed, truncate(message)
		case inconclusiveStatus(status):
			return VerdictInconclusive, truncate(message)
		}
	}
	lower := strings.ToLower(message)
	for _, marker := range refusalMarkers {
		if strings.Contains(lower, marker) {
			return VerdictRed, truncate(message)
		}
	}
	// Everything unrecognized is inconclusive. That includes a malformed
	// request TF itself produced, which is the case this default exists for: a
	// bug on our side must not be recorded as the org's model being
	// unavailable.
	return VerdictInconclusive, truncate(message)
}

// classifySDK sorts the outcome of a probe spent through the agent runtime.
//
// The runtime reports a provider refusal as a completed invocation carrying an
// error, not as a process failure: Run returns a nil error and a terminal
// result whose IsError is set and whose APIErrorStatus is the status the
// provider answered with. So a non-nil error here means the runtime itself
// never got an answer — it failed to start, its stream broke, the subprocess
// died, or the deadline ended it — which is inconclusive for the same reason a
// dial failure is on the direct path.
//
// Two fields are deliberately not consulted. Subtype reads "success" on an
// authentication failure, so it distinguishes nothing. And the runtime's own
// prose contradicts the status it ships beside — a 403 arrives described as a
// temporary network issue — which is exactly why the status is what decides
// and why sdkErrorDetail puts the number in front of the message.
func classifySDK(callCtx context.Context, outcome *agentproc.Outcome, err error) (Verdict, string) {
	if err != nil {
		return classifyFailure(callCtx, 0, false, sdkFailureMessage(outcome, err))
	}
	if outcome == nil || outcome.Result == nil {
		return VerdictInconclusive, "the agent runtime exited without a terminal result event"
	}
	res := outcome.Result
	if !res.IsError {
		return VerdictGreen, ""
	}
	// An error with no status is an invocation that failed short of the
	// provider answering — a turn limit, a runtime fault. hasStatus is false
	// for it, so it falls to the markers and then to inconclusive, which is
	// what "the question was not answered" is worth.
	return classifyFailure(callCtx, res.APIErrorStatus, res.APIErrorStatus != 0, sdkErrorDetail(res))
}

// sdkErrorDetail is what an admin reads for a refusal the runtime carried.
//
// The status leads because the message behind it cannot be trusted to describe
// it: the runtime renders a 403 as "Authentication error · This may be a
// temporary network issue, please try again", which tells someone to retry
// their way out of a permanent entitlement problem. The message is kept whole
// after it — it is still the only vendor-specific thing in the answer.
func sdkErrorDetail(res *agentproc.Result) string {
	msg := strings.TrimSpace(res.Result)
	if msg == "" {
		msg = "the agent runtime reported an error with no message"
	}
	if res.APIErrorStatus == 0 {
		return msg
	}
	return fmt.Sprintf("HTTP %d: %s", res.APIErrorStatus, msg)
}

// sdkFailureMessage carries the runtime's stderr alongside the error, because
// on this path it is the only account of what happened: the error itself names
// a failure class ("agent runtime exited with error: exit status 1") and the
// diagnostics are all on the other stream. Nothing is stored for the verdict
// this feeds — inconclusive writes no row — so it exists purely to be read
// once by the admin who pressed test.
func sdkFailureMessage(outcome *agentproc.Outcome, err error) string {
	stderr := ""
	if outcome != nil {
		stderr = strings.TrimSpace(outcome.Stderr)
	}
	if stderr == "" {
		return err.Error()
	}
	return fmt.Sprintf("%s (stderr: %s)", err.Error(), stderr)
}
