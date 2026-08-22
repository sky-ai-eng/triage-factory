package modelprobe

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
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

// sdkResult builds the terminal event shape classifySDK reads.
func sdkResult(isError bool, status int, text string) *agentproc.Outcome {
	return &agentproc.Outcome{Result: &agentproc.Result{
		IsError: isError,
		// Always "success" — the runtime sets it on refusals too, which is
		// exactly why nothing here reads it.
		Subtype:        "success",
		APIErrorStatus: status,
		Result:         text,
	}}
}

// The status on the terminal event decides, and the message beside it does not
// get a vote in either direction. Both halves matter: the runtime describes a
// permanent 403 as a temporary network issue, and a transient 500 can carry a
// body quoting a refusal it retried past.
func TestClassifySDK_TheStatusDecides(t *testing.T) {
	for name, tc := range map[string]struct {
		status int
		text   string
		want   Verdict
	}{
		"401":                        {401, "Authentication error", VerdictRed},
		"403 described as transient": {403, "Authentication error · This may be a temporary network issue, please try again", VerdictRed},
		"404":                        {404, "model not found", VerdictRed},
		"429":                        {429, "rate limited", VerdictInconclusive},
		"500":                        {500, "API Error: 500 Internal server error", VerdictInconclusive},
		"503 quoting a refusal":      {503, "upstream said AccessDeniedException", VerdictInconclusive},
	} {
		t.Run(name, func(t *testing.T) {
			verdict, detail := classifySDK(context.Background(), sdkResult(true, tc.status, tc.text), nil)
			if verdict != tc.want {
				t.Errorf("verdict = %q, want %q", verdict, tc.want)
			}
			if detail == "" {
				t.Error("no detail on a non-green verdict")
			}
		})
	}
}

// A run the provider answered is green, whatever else the event carries.
func TestClassifySDK_CleanResultIsGreen(t *testing.T) {
	verdict, detail := classifySDK(context.Background(), sdkResult(false, 0, "pong"), nil)
	if verdict != VerdictGreen || detail != "" {
		t.Errorf("classifySDK = (%q, %q), want green with no detail", verdict, detail)
	}
}

// Every way the runtime can fail short of the provider answering is
// inconclusive, because none of them says anything about this credential's
// right to invoke this model. A turn limit is the one worth naming: it carries
// is_error like a refusal does and no status at all.
func TestClassifySDK_NoAnswerIsInconclusive(t *testing.T) {
	for name, tc := range map[string]struct {
		outcome *agentproc.Outcome
		err     error
	}{
		"a turn limit":        {sdkResult(true, 0, "reached max turns"), nil},
		"no terminal event":   {&agentproc.Outcome{}, nil},
		"a nil outcome":       {nil, errors.New("start agent runtime: exec: \"node\": not found")},
		"the runtime crashed": {&agentproc.Outcome{Stderr: "node: symbol lookup error"}, errors.New("agent runtime exited with error: exit status 1")},
	} {
		t.Run(name, func(t *testing.T) {
			if verdict, _ := classifySDK(context.Background(), tc.outcome, tc.err); verdict != VerdictInconclusive {
				t.Errorf("verdict = %q, want %q", verdict, VerdictInconclusive)
			}
		})
	}
}

// TF's own clock running out beats a status the runtime reported on the way
// down, the same short-circuit the direct path takes and for the same reason: a
// wrong red costs a real request to undo.
func TestClassifySDK_ExpiredCtxBeatsARefusalStatus(t *testing.T) {
	expired, cancel := context.WithCancel(context.Background())
	cancel()

	if verdict, _ := classifySDK(expired, sdkResult(true, 403, "Authentication error"), nil); verdict != VerdictInconclusive {
		t.Errorf("verdict = %q, want %q", verdict, VerdictInconclusive)
	}
}

// The stored detail leads with the number, because the message behind it tells
// an admin to retry their way out of a permanent entitlement problem.
func TestSDKErrorDetail_LeadsWithTheStatus(t *testing.T) {
	got := sdkErrorDetail(&agentproc.Result{APIErrorStatus: 403, Result: "Authentication error · This may be a temporary network issue"})
	if !strings.HasPrefix(got, "HTTP 403: ") {
		t.Errorf("detail = %q, want it to lead with the status", got)
	}
	if !strings.Contains(got, "Authentication error") {
		t.Errorf("detail = %q, want the runtime's own message kept", got)
	}
}

// A runtime failure carries its stderr, because the error alone names only a
// failure class and the diagnostics are all on the other stream.
func TestSDKFailureMessage_CarriesStderr(t *testing.T) {
	got := sdkFailureMessage(&agentproc.Outcome{Stderr: "Error: Cannot find module 'sdk'"},
		errors.New("agent runtime exited with error: exit status 1"))
	if !strings.Contains(got, "exit status 1") || !strings.Contains(got, "Cannot find module") {
		t.Errorf("message = %q, want both the error and its stderr", got)
	}
}
