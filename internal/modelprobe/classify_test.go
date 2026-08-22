package modelprobe

import (
	"context"
	"errors"
	"testing"
)

// errFlattenedTimeout is what a probe timeout actually looks like by the time it
// reaches classify: internal/inference renders every provider failure into a
// plain string, so the ctx sentinel does NOT survive in the error chain. This
// is the premise the ctx check exists for, asserted rather than assumed —
// if the neutral layer ever started wrapping, the structured check below would
// stop being the only thing standing between a timeout and a misread.
var errFlattenedTimeout = errors.New("inference: provider error: context deadline exceeded")

func TestClassify_TheTimeoutSentinelDoesNotSurviveFlattening(t *testing.T) {
	if errors.Is(errFlattenedTimeout, context.DeadlineExceeded) {
		t.Fatal("the flattened provider error matches the ctx sentinel; classify could rely on errors.Is after all")
	}
}

// An expired request ctx is TF's own clock running out, not the provider
// answering — whatever string came back on the way down. The ctx is the
// REQUEST's, derived from the caller's, so this one check covers both the
// caller abandoning a sweep and the per-probe timeout expiring.
func TestClassify_ExpiredRequestCtxIsInconclusive(t *testing.T) {
	expired, cancel := context.WithCancel(context.Background())
	cancel()

	for name, err := range map[string]error{
		"a flattened deadline": errFlattenedTimeout,
		// The case that makes this a real check rather than a formality: a
		// probe that ran out of time against a provider whose last words
		// looked like a refusal must not be recorded as one. Nothing else in
		// classify would stop it — the marker scan below would call it red.
		"a refusal-shaped body": errors.New("inference: provider error: AccessDeniedException"),
	} {
		t.Run(name, func(t *testing.T) {
			if verdict, _ := classify(expired, err); verdict != VerdictInconclusive {
				t.Errorf("verdict = %q, want %q", verdict, VerdictInconclusive)
			}
		})
	}
}

// With the request ctx still live, an unrecognized failure is inconclusive by
// the default arm. Pinned so the outcome of a timeout is right for two
// independent reasons, and neither is load-bearing alone.
func TestClassify_UnrecognizedFailureIsInconclusive(t *testing.T) {
	if verdict, _ := classify(context.Background(), errFlattenedTimeout); verdict != VerdictInconclusive {
		t.Errorf("verdict = %q, want %q", verdict, VerdictInconclusive)
	}
}

// A live ctx does not soften a real refusal — the ctx check is a short-circuit
// for our own clock, not a blanket amnesty.
func TestClassify_LiveCtxStillReadsARefusal(t *testing.T) {
	err := errors.New("inference: provider error: permission denied (HTTP 403)")
	if verdict, detail := classify(context.Background(), err); verdict != VerdictRed || detail == "" {
		t.Errorf("classify = (%q, %q), want red with a detail", verdict, detail)
	}
}
