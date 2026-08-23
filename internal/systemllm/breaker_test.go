package systemllm

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/inference"
	"github.com/sky-ai-eng/triage-factory/internal/modelcatalog"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestProviderKey pins which calls share a breaker entry. The env maps go
// through the same mapping the request does, because the key's whole job is
// to describe the endpoint that will actually be dialed — a key derived from
// the raw map instead can name a configuration the call doesn't have.
func TestProviderKey(t *testing.T) {
	// Each case names a model its credentials actually serve: the mapping
	// refuses a model whose provider the credentials don't match, so the
	// provider a case is about has to be the one its model names.
	anthropicModel := modelOn(t, modelcatalog.ProviderAnthropic)
	bedrockModel := modelOn(t, modelcatalog.ProviderBedrock)
	cases := []struct {
		name  string
		creds map[string]string
		model string
		want  string
	}{
		{
			name:  "bedrock with no configured region keys on the region the call defaults to",
			creds: map[string]string{"AWS_BEARER_TOKEN_BEDROCK": "tok"},
			model: bedrockModel,
			want:  "bedrock:us-east-1",
		},
		{
			name:  "anthropic direct, default endpoint",
			creds: map[string]string{"ANTHROPIC_API_KEY": "sk-ant-1"},
			model: anthropicModel,
			want:  "anthropic-direct:default",
		},
		{
			name:  "anthropic direct, custom base url",
			creds: map[string]string{"ANTHROPIC_API_KEY": "sk-ant-1", "ANTHROPIC_BASE_URL": "https://Gateway.Example.com/v1/"},
			model: anthropicModel,
			want:  "anthropic-direct:https://gateway.example.com/v1",
		},
		{
			name:  "two orgs on the default anthropic endpoint share one key regardless of api key",
			creds: map[string]string{"ANTHROPIC_API_KEY": "sk-ant-a-completely-different-key"},
			model: anthropicModel,
			want:  "anthropic-direct:default",
		},
		{
			name:  "bedrock bearer, region only",
			creds: map[string]string{"AWS_BEARER_TOKEN_BEDROCK": "tok", "AWS_REGION": "us-east-1"},
			model: bedrockModel,
			want:  "bedrock:us-east-1",
		},
		{
			name:  "bedrock sigv4, region only",
			creds: map[string]string{"AWS_ACCESS_KEY_ID": "AKIA", "AWS_SECRET_ACCESS_KEY": "secret", "AWS_REGION": "eu-central-1"},
			model: bedrockModel,
			want:  "bedrock:eu-central-1",
		},
		{
			name:  "bedrock with a VPC/gateway base url override gets its own key",
			creds: map[string]string{"AWS_BEARER_TOKEN_BEDROCK": "tok", "AWS_REGION": "us-east-1", "ANTHROPIC_BEDROCK_BASE_URL": "https://vpce-123.bedrock.us-east-1.vpce.amazonaws.com"},
			model: bedrockModel,
			want:  "bedrock:us-east-1@https://vpce-123.bedrock.us-east-1.vpce.amazonaws.com",
		},
		{
			name:  "anthropic direct wins over bedrock when both are somehow present",
			creds: map[string]string{"ANTHROPIC_API_KEY": "sk-ant-1", "AWS_BEARER_TOKEN_BEDROCK": "tok", "AWS_REGION": "us-east-1"},
			model: anthropicModel,
			want:  "anthropic-direct:default",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pc, err := mapDirectCreds(tc.creds, tc.model)
			if err != nil {
				t.Fatalf("mapDirectCreds: %v", err)
			}
			if got := providerKey(pc); got != tc.want {
				t.Errorf("providerKey(%v) = %q, want %q", tc.creds, got, tc.want)
			}
		})
	}

	t.Run("an unmapped env map never reaches the breaker", func(t *testing.T) {
		// There is no "unknown" key any more: credentials that name no
		// provider fail the mapping, and the call is over before there is
		// anything to open a cooldown on.
		if _, err := mapDirectCreds(map[string]string{}, anthropicModel); err == nil {
			t.Fatal("expected an error for credentials naming no provider")
		}
	})
}

// providerErr builds an error in the shape internal/inference renders a
// provider failure into — bifrost's own message, the wrapped cause, and the
// status marker the classifier keys on. Written out rather than referencing
// the renderer so a change to that rendering fails here loudly instead of
// silently reclassifying every failure.
func providerErr(status int, detail string) error {
	return fmt.Errorf("inference: provider error: %s (HTTP %d) [provider_error] [endpoint: https://api.anthropic.com]", detail, status)
}

// TestIsTransientFailure pins the classification that decides what trips
// the breaker: overloaded/rate-limited/5xx and transport failures do; a
// caller-cancelled ctx, permanent 4xx client errors, and a context overflow
// don't.
func TestIsTransientFailure(t *testing.T) {
	bg := context.Background()
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()

	cases := []struct {
		name string
		ctx  context.Context
		err  error
		want bool
	}{
		{"nil error", bg, nil, false},
		{"cancelled ctx never trips the breaker, even for an overload", cancelled, providerErr(529, "overloaded"), false},
		{"529 overloaded", bg, providerErr(529, "overloaded_error"), true},
		{"500 internal error", bg, providerErr(500, "internal server error"), true},
		{"503 service unavailable", bg, providerErr(503, "service unavailable"), true},
		{"429 rate limited", bg, providerErr(429, "rate_limit_error"), true},
		{"408 request timeout", bg, providerErr(408, "request timeout"), true},
		{"409 conflict", bg, providerErr(409, "conflict"), true},
		{"400 bad request is permanent, not transient", bg, providerErr(400, "invalid_request_error"), false},
		{"401 unauthorized is permanent, not transient", bg, providerErr(401, "authentication_error"), false},
		{"404 not found is permanent, not transient", bg, providerErr(404, "model not found"), false},
		{
			name: "a transport failure that never got a response is transient",
			ctx:  bg,
			err:  errors.New("inference: provider error: failed to execute HTTP request to provider API: dial tcp 10.42.7.1:443: connect: connection refused"),
			want: true,
		},
		{
			name: "a rendered status settles it: a 400 quoting a transport phrase stays permanent",
			ctx:  bg,
			err:  providerErr(400, "invalid_request_error: your prompt mentioned a connection reset"),
			want: false,
		},
		{
			name: "a context overflow is a deterministic rejection, not provider health",
			ctx:  bg,
			err:  fmt.Errorf("%w: prompt is too long: 429000 tokens > 200000 maximum (HTTP 400)", inference.ErrContextOverflow),
			want: false,
		},
		{
			name: "an unclassified error is NOT transient",
			ctx:  bg,
			err:  errors.New("unexpected end of JSON input"),
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isTransientFailure(tc.ctx, tc.err); got != tc.want {
				t.Errorf("isTransientFailure(...) = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestIsTransientFailure_MatchesInferenceRendering pins the coupling between
// this classifier and how internal/inference actually renders a status: the
// marker is the whole contract, and a change to it would otherwise turn every
// overload into an unclassified (permanent) failure with no test failing.
func TestIsTransientFailure_MatchesInferenceRendering(t *testing.T) {
	rendered := renderedStatusFixture(t, 529)
	if !isTransientFailure(context.Background(), errors.New(rendered)) {
		t.Fatalf("inference now renders a 529 as %q, which this classifier no longer recognizes", rendered)
	}
	if isTransientFailure(context.Background(), errors.New(renderedStatusFixture(t, 401))) {
		t.Fatal("a 401 rendered the same way must stay permanent")
	}
}

// renderedStatusFixture drives a real inference call against a stub provider
// answering the given status and returns the error text it produced — the
// actual rendering, not this package's belief about it.
func renderedStatusFixture(t *testing.T, status int) string {
	t.Helper()
	srv := httptest.NewServer(&capturingHandler{t: t, status: status})
	defer srv.Close()

	const model = "claude-haiku-4-5-20251001"
	pc, err := mapDirectCreds(map[string]string{"ANTHROPIC_API_KEY": "k", "ANTHROPIC_BASE_URL": srv.URL}, model)
	if err != nil {
		t.Fatalf("mapDirectCreds: %v", err)
	}
	client, release, err := newDirectClient(pc)
	if err != nil {
		t.Fatalf("newDirectClient: %v", err)
	}
	defer release()

	_, callErr := client.Stream(context.Background(), inference.Request{
		Provider:     pc.Provider,
		Model:        model,
		SystemPrompt: "system instructions",
		Rows:         []domain.Message{{Role: "user", Content: "user data"}},
	})
	if callErr == nil {
		t.Fatalf("expected an error for a %d response", status)
	}
	return callErr.Error()
}

func TestProviderBreaker_CheckAndRecord(t *testing.T) {
	b := newProviderBreaker()

	if err := b.check("p"); err != nil {
		t.Fatalf("check on an empty registry = %v, want nil", err)
	}

	b.recordResult("p", true)
	var backoffErr *ErrProviderBackoff
	if err := b.check("p"); !errors.As(err, &backoffErr) {
		t.Fatalf("check after a transient failure = %v, want *ErrProviderBackoff", err)
	}
	if backoffErr.Provider != "p" {
		t.Errorf("Provider = %q, want %q", backoffErr.Provider, "p")
	}
	if !backoffErr.ResumeAt.After(time.Now()) {
		t.Errorf("ResumeAt = %v, want a future time", backoffErr.ResumeAt)
	}

	if err := b.check("other"); err != nil {
		t.Errorf(`check("other") = %v, want nil — breaker state must not leak across providers`, err)
	}

	b.recordResult("p", false)
	if err := b.check("p"); err != nil {
		t.Errorf("check after a success = %v, want nil — a success must clear the cooldown", err)
	}
}

func TestProviderBreaker_EscalatesAndCaps(t *testing.T) {
	b := newProviderBreaker()
	var prev time.Duration
	for i := 0; i < providerBreakerMaxDoublings+3; i++ {
		b.recordResult("p", true)
		var backoffErr *ErrProviderBackoff
		if err := b.check("p"); !errors.As(err, &backoffErr) {
			t.Fatalf("iteration %d: check = %v, want *ErrProviderBackoff", i, err)
		}
		delay := time.Until(backoffErr.ResumeAt)
		if delay > providerBreakerMaxDelay+time.Second {
			t.Errorf("iteration %d: delay = %v, want capped at %v", i, delay, providerBreakerMaxDelay)
		}
		// Tolerance covers the scheduling jitter between recordResult's
		// internal time.Now() and this check's — once capped, successive
		// delays are each recomputed as "now + max", so back-to-back
		// iterations legitimately differ by a few microseconds even though
		// neither is a real decrease in backoff.
		if i > 0 && delay+100*time.Millisecond < prev {
			t.Errorf("iteration %d: delay %v shrank below the previous %v, want non-decreasing until the cap", i, delay, prev)
		}
		prev = delay
	}

	// The raw failures counter must itself stay bounded across an extended
	// outage, not just the delay derived from it — an unbounded counter
	// would be a misleading number for anyone who later logs or exposes it
	// directly.
	if got, want := b.state["p"].failures, providerBreakerMaxDoublings+1; got != want {
		t.Errorf("failures = %d after %d consecutive failures, want capped at %d", got, providerBreakerMaxDoublings+3, want)
	}
}

func TestProviderBreaker_NilIsSafeNoOp(t *testing.T) {
	var b *providerBreaker
	if err := b.check("p"); err != nil {
		t.Errorf("check on a nil breaker = %v, want nil", err)
	}
	b.recordResult("p", true) // must not panic
}

// TestComplete_Direct_ProviderBreakerShortCircuitsRepeatedOverload is the
// end-to-end regression guard for the boot-time overload storm: a 529
// response trips the breaker, and a second call against the same provider
// short-circuits without ever reaching the network.
func TestComplete_Direct_ProviderBreakerShortCircuitsRepeatedOverload(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	h := &capturingHandler{t: t, status: 529, errBody: `{"type":"error","error":{"type":"overloaded_error","message":"overloaded"}}`}
	srv := httptest.NewServer(h)
	defer srv.Close()

	secrets := stubSecrets{
		"org-1/anthropic_api_key":  "sk-ant-test",
		"org-1/anthropic_base_url": srv.URL,
	}
	r := NewRecorder(nil)

	_, err := r.Complete(context.Background(), completeOpts("org-1", secrets))
	if err == nil {
		t.Fatal("expected an error for a 529 response")
	}
	if IsProviderBackoff(err) {
		t.Fatal("the first call is a genuine attempt against the stub server, not a breaker short-circuit")
	}
	afterFirst := h.Requests()
	if afterFirst == 0 {
		t.Fatal("expected at least one real request against the stub server")
	}

	_, err2 := r.Complete(context.Background(), completeOpts("org-1", secrets))
	if !IsProviderBackoff(err2) {
		t.Fatalf("second Complete's err = %v, want ErrProviderBackoff (the breaker should have tripped)", err2)
	}
	if h.Requests() != afterFirst {
		t.Errorf("requests = %d after the second Complete, want unchanged at %d — the breaker should short-circuit without a network call", h.Requests(), afterFirst)
	}
}

// TestComplete_Direct_ProviderBreakerDoesNotLeakAcrossProviders pins the
// keying rule: two orgs configured against different Anthropic-direct base
// URLs are different upstream fleets, so one tripping its breaker must not
// gate the other.
func TestComplete_Direct_ProviderBreakerDoesNotLeakAcrossProviders(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	overloaded := &capturingHandler{t: t, status: 529, errBody: `{"type":"error","error":{"type":"overloaded_error","message":"overloaded"}}`}
	overloadedSrv := httptest.NewServer(overloaded)
	defer overloadedSrv.Close()

	healthy := &capturingHandler{t: t, text: "still healthy"}
	healthySrv := httptest.NewServer(healthy)
	defer healthySrv.Close()

	r := NewRecorder(nil)

	secrets1 := stubSecrets{
		"org-1/anthropic_api_key":  "sk-ant-test-1",
		"org-1/anthropic_base_url": overloadedSrv.URL,
	}
	secrets2 := stubSecrets{
		"org-2/anthropic_api_key":  "sk-ant-test-2",
		"org-2/anthropic_base_url": healthySrv.URL,
	}

	if _, err := r.Complete(context.Background(), completeOpts("org-1", secrets1)); err == nil {
		t.Fatal("expected an error for org-1's 529 response")
	}

	result, err := r.Complete(context.Background(), completeOpts("org-2", secrets2))
	if err != nil {
		t.Fatalf("org-2 Complete: %v (a healthy, unrelated provider must not be gated by org-1's cooldown)", err)
	}
	if result.Text != "still healthy" {
		t.Errorf("Text = %q", result.Text)
	}
	if healthy.Requests() == 0 {
		t.Error("expected org-2's healthy provider to receive a real request")
	}
}

// TestComplete_Direct_TerminalErrorDoesNotTripBreaker extends the existing
// single-call 400 pin: a permanent client error must never trip the
// breaker, however many times it recurs.
func TestComplete_Direct_TerminalErrorDoesNotTripBreaker(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	h := &capturingHandler{t: t, status: http.StatusBadRequest, errBody: `{"type":"error","error":{"type":"invalid_request_error","message":"bad request"}}`}
	srv := httptest.NewServer(h)
	defer srv.Close()

	secrets := stubSecrets{
		"org-1/anthropic_api_key":  "sk-ant-test",
		"org-1/anthropic_base_url": srv.URL,
	}
	r := NewRecorder(nil)

	if _, err := r.Complete(context.Background(), completeOpts("org-1", secrets)); err == nil {
		t.Fatal("expected an error for a 400 response")
	}
	if _, err := r.Complete(context.Background(), completeOpts("org-1", secrets)); err == nil {
		t.Fatal("expected an error for a 400 response")
	}
	if h.Requests() != 2 {
		t.Errorf("requests = %d, want 2 — a permanent 4xx must never trip the breaker", h.Requests())
	}
}
