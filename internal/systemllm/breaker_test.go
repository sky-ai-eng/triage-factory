package systemllm

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestProviderKey pins buildDirectClient's branch precedence and the
// base-URL/region rules that decide which calls share a breaker entry.
func TestProviderKey(t *testing.T) {
	cases := []struct {
		name  string
		creds map[string]string
		want  string
	}{
		{
			name:  "anthropic direct, default endpoint",
			creds: map[string]string{"ANTHROPIC_API_KEY": "sk-ant-1"},
			want:  "anthropic-direct:default",
		},
		{
			name:  "anthropic direct, custom base url",
			creds: map[string]string{"ANTHROPIC_API_KEY": "sk-ant-1", "ANTHROPIC_BASE_URL": "https://Gateway.Example.com/v1/"},
			want:  "anthropic-direct:https://gateway.example.com/v1",
		},
		{
			name:  "two orgs on the default anthropic endpoint share one key regardless of api key",
			creds: map[string]string{"ANTHROPIC_API_KEY": "sk-ant-a-completely-different-key"},
			want:  "anthropic-direct:default",
		},
		{
			name:  "bedrock bearer, region only",
			creds: map[string]string{"AWS_BEARER_TOKEN_BEDROCK": "tok", "AWS_REGION": "us-east-1"},
			want:  "bedrock:us-east-1",
		},
		{
			name:  "bedrock sigv4, region only",
			creds: map[string]string{"AWS_ACCESS_KEY_ID": "AKIA", "AWS_SECRET_ACCESS_KEY": "secret", "AWS_REGION": "eu-central-1"},
			want:  "bedrock:eu-central-1",
		},
		{
			name:  "bedrock with a VPC/gateway base url override gets its own key",
			creds: map[string]string{"AWS_BEARER_TOKEN_BEDROCK": "tok", "AWS_REGION": "us-east-1", "ANTHROPIC_BEDROCK_BASE_URL": "https://vpce-123.bedrock.us-east-1.vpce.amazonaws.com"},
			want:  "bedrock:us-east-1@https://vpce-123.bedrock.us-east-1.vpce.amazonaws.com",
		},
		{
			name:  "anthropic direct wins over bedrock when both are somehow present",
			creds: map[string]string{"ANTHROPIC_API_KEY": "sk-ant-1", "AWS_BEARER_TOKEN_BEDROCK": "tok", "AWS_REGION": "us-east-1"},
			want:  "anthropic-direct:default",
		},
		{
			name:  "no recognized credentials",
			creds: map[string]string{},
			want:  "unknown",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := providerKey(tc.creds); got != tc.want {
				t.Errorf("providerKey(%v) = %q, want %q", tc.creds, got, tc.want)
			}
		})
	}
}

// TestIsTransientFailure pins the classification that decides what trips
// the breaker: overloaded/rate-limited/5xx and unstructured transport
// failures do; a caller-cancelled ctx and permanent 4xx client errors don't.
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
		{"cancelled ctx never trips the breaker, even for an overload", cancelled, &anthropic.Error{StatusCode: 529}, false},
		{"529 overloaded", bg, &anthropic.Error{StatusCode: 529}, true},
		{"500 internal error", bg, &anthropic.Error{StatusCode: 500}, true},
		{"429 rate limited", bg, &anthropic.Error{StatusCode: 429}, true},
		{"408 request timeout", bg, &anthropic.Error{StatusCode: 408}, true},
		{"409 conflict", bg, &anthropic.Error{StatusCode: 409}, true},
		{"400 bad request is permanent, not transient", bg, &anthropic.Error{StatusCode: 400}, false},
		{"401 unauthorized is permanent, not transient", bg, &anthropic.Error{StatusCode: 401}, false},
		{"404 not found is permanent, not transient", bg, &anthropic.Error{StatusCode: 404}, false},
		{
			name: "a net.Error-shaped transport failure (dial/DNS/TLS/timeout) is transient",
			ctx:  bg,
			err:  &url.Error{Op: "Post", URL: "https://api.anthropic.com/v1/messages", Err: errors.New("connection refused")},
			want: true,
		},
		{
			name: "an unstructured local/SDK error with no net.Error shape is NOT transient",
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
	h := &capturingHandler{t: t, status: 529, body: `{"type":"error","error":{"type":"overloaded_error","message":"overloaded"}}`}
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
	overloaded := &capturingHandler{t: t, status: 529, body: `{"type":"error","error":{"type":"overloaded_error","message":"overloaded"}}`}
	overloadedSrv := httptest.NewServer(overloaded)
	defer overloadedSrv.Close()

	healthy := &capturingHandler{t: t, body: sprintfBody(`still healthy`)}
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
	h := &capturingHandler{t: t, status: http.StatusBadRequest, body: `{"type":"error","error":{"type":"invalid_request_error","message":"bad request"}}`}
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
