package modelprobe

import (
	"context"
	"strings"

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
// callCtx is the ctx the REQUEST ran under, not the caller's. It is derived
// from the caller's, so a non-nil Err covers both endings that are TF's own
// clock rather than the provider's answer: the caller navigating away
// mid-sweep, and the per-probe timeout expiring. Checking the caller's ctx
// instead would miss the second — the outer one is still healthy when a probe
// times out — and there is no error value to fall back on, because
// internal/inference flattens every failure to a plain string and no ctx
// sentinel survives for errors.Is to match.
func classify(callCtx context.Context, err error) (Verdict, string) {
	if err == nil {
		return VerdictGreen, ""
	}
	if callCtx.Err() != nil {
		return VerdictInconclusive, truncate(err.Error())
	}
	if status, ok := inference.RenderedStatus(err); ok {
		switch {
		case refusalStatuses[status]:
			return VerdictRed, truncate(err.Error())
		case inconclusiveStatus(status):
			return VerdictInconclusive, truncate(err.Error())
		}
	}
	lower := strings.ToLower(err.Error())
	for _, marker := range refusalMarkers {
		if strings.Contains(lower, marker) {
			return VerdictRed, truncate(err.Error())
		}
	}
	// Everything unrecognized is inconclusive. That includes a malformed
	// request TF itself produced, which is the case this default exists for: a
	// bug on our side must not be recorded as the org's model being
	// unavailable.
	return VerdictInconclusive, truncate(err.Error())
}
