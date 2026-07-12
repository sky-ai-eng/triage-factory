package systemllm

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// stubSecrets is a minimal agentproc.SecretsReader backed by an in-memory
// map, keyed "org/key" — enough for buildDirectClient's credential
// precedence to exercise every provider branch without a real store.
type stubSecrets map[string]string

func (s stubSecrets) Get(_ context.Context, orgID, key string) (string, error) {
	return s[orgID+"/"+key], nil
}

// capturingHandler records the last request it served (method/path/header)
// alongside a caller-supplied response, so tests can assert on how
// buildDirectClient shaped the outgoing call.
type capturingHandler struct {
	t          *testing.T
	status     int
	body       string
	lastHeader http.Header
	lastPath   string
	requests   int
}

func (h *capturingHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.requests++
	h.lastHeader = r.Header.Clone()
	h.lastPath = r.URL.Path
	io.Copy(io.Discard, r.Body)
	w.Header().Set("Content-Type", "application/json")
	status := h.status
	if status == 0 {
		status = http.StatusOK
	}
	w.WriteHeader(status)
	body := h.body
	if body == "" {
		body = `{"type":"error","error":{"type":"invalid_request_error","message":"stub error"}}`
	}
	w.Write([]byte(body))
}

func completeOpts(orgID string, secrets agentproc.SecretsReader) CompleteOptions {
	return CompleteOptions{
		OrgID:        orgID,
		Job:          JobClassifier,
		Message:      "combined local prompt",
		SystemPrompt: "system instructions",
		UserMessage:  "user data",
		Model:        "haiku",
		DirectModel:  "claude-haiku-4-5-20251001",
		MaxTokens:    2048,
		Temperature:  0.1,
		TraceID:      "test-trace",
		Secrets:      secrets,
		CostFn: func(model string, in, out, cacheRead, cacheCreate int) float64 {
			return float64(in+out+cacheRead+cacheCreate) / 1000
		},
	}
}

// TestComplete_Direct_AnthropicAPIKey pins the Anthropic direct-key path:
// the resolved api key goes out as X-Api-Key, the configured base URL is
// honored, and the response text passes through untouched.
func TestComplete_Direct_AnthropicAPIKey(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	h := &capturingHandler{t: t, body: sprintfBody(`hello world`)}
	srv := httptest.NewServer(h)
	defer srv.Close()

	secrets := stubSecrets{
		"org-1/anthropic_api_key":  "sk-ant-test",
		"org-1/anthropic_base_url": srv.URL,
	}
	r := NewRecorder(nil)
	result, err := r.Complete(context.Background(), completeOpts("org-1", secrets))
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if result.Text != "hello world" {
		t.Errorf("Text = %q, want %q", result.Text, "hello world")
	}
	if got := h.lastHeader.Get("X-Api-Key"); got != "sk-ant-test" {
		t.Errorf("X-Api-Key = %q, want sk-ant-test", got)
	}
	if h.lastHeader.Get("Authorization") != "" {
		t.Errorf("Authorization should be unset with no auth token configured, got %q", h.lastHeader.Get("Authorization"))
	}
}

// TestComplete_Direct_AnthropicAuthTokenBearer pins the gateway bearer path:
// when anthropic_auth_token is configured alongside the api key, both the
// X-Api-Key header AND an Authorization: Bearer header go out — matching
// resolveCredentials, which sets both env vars together rather than
// treating them as alternatives.
func TestComplete_Direct_AnthropicAuthTokenBearer(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	h := &capturingHandler{t: t, body: sprintfBody(`auth token payload`)}
	srv := httptest.NewServer(h)
	defer srv.Close()

	secrets := stubSecrets{
		"org-1/anthropic_api_key":    "sk-ant-test",
		"org-1/anthropic_base_url":   srv.URL,
		"org-1/anthropic_auth_token": "gateway-bearer-tok",
	}
	r := NewRecorder(nil)
	if _, err := r.Complete(context.Background(), completeOpts("org-1", secrets)); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if got := h.lastHeader.Get("X-Api-Key"); got != "sk-ant-test" {
		t.Errorf("X-Api-Key = %q, want sk-ant-test", got)
	}
	if got := h.lastHeader.Get("Authorization"); got != "Bearer gateway-bearer-tok" {
		t.Errorf("Authorization = %q, want Bearer gateway-bearer-tok", got)
	}
}

// TestComplete_Direct_BedrockBearer pins the Bedrock API-key path: the
// bearer token goes out as an Authorization header and the request is
// rewritten onto the Bedrock invoke path.
func TestComplete_Direct_BedrockBearer(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	h := &capturingHandler{t: t, body: sprintfBody(`bedrock bearer ok`)}
	srv := httptest.NewServer(h)
	defer srv.Close()

	secrets := stubSecrets{
		"org-1/aws_bearer_token_bedrock": "bedrock-bearer-tok",
		"org-1/aws_region":               "us-east-1",
		"org-1/bedrock_base_url":         srv.URL,
	}
	r := NewRecorder(nil)
	result, err := r.Complete(context.Background(), completeOpts("org-1", secrets))
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if result.Text != "bedrock bearer ok" {
		t.Errorf("Text = %q", result.Text)
	}
	if got := h.lastHeader.Get("Authorization"); got != "Bearer bedrock-bearer-tok" {
		t.Errorf("Authorization = %q, want Bearer bedrock-bearer-tok", got)
	}
	if !strings.Contains(h.lastPath, "/invoke") {
		t.Errorf("path = %q, want a Bedrock /model/.../invoke path", h.lastPath)
	}
}

// TestComplete_Direct_BedrockSigV4WithSessionToken pins the Bedrock
// access-key-triple path, including a session token — required because
// Bedrock STS session creds ship this release. The signed request must
// carry an AWS4-HMAC-SHA256 Authorization header and an
// X-Amz-Security-Token header equal to the session token (SigV4 signs the
// session token in, it doesn't just drop it).
func TestComplete_Direct_BedrockSigV4WithSessionToken(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	h := &capturingHandler{t: t, body: sprintfBody(`sigv4 ok`)}
	srv := httptest.NewServer(h)
	defer srv.Close()

	secrets := stubSecrets{
		"org-1/aws_access_key_id":     "AKIAEXAMPLE",
		"org-1/aws_secret_access_key": "secretexample",
		"org-1/aws_session_token":     "sessiontoken123",
		"org-1/aws_region":            "us-east-1",
		"org-1/bedrock_base_url":      srv.URL,
	}
	r := NewRecorder(nil)
	result, err := r.Complete(context.Background(), completeOpts("org-1", secrets))
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if result.Text != "sigv4 ok" {
		t.Errorf("Text = %q", result.Text)
	}
	if got := h.lastHeader.Get("Authorization"); !strings.HasPrefix(got, "AWS4-HMAC-SHA256") {
		t.Errorf("Authorization = %q, want an AWS4-HMAC-SHA256 SigV4 signature", got)
	}
	if got := h.lastHeader.Get("X-Amz-Security-Token"); got != "sessiontoken123" {
		t.Errorf("X-Amz-Security-Token = %q, want sessiontoken123", got)
	}
}

// TestComplete_Direct_BedrockModelDefaultAndOverride pins decision 5's
// Bedrock model mapping: bedrock_model_id wins when set, otherwise Complete
// falls back to the pinned Haiku inference-profile default.
func TestComplete_Direct_BedrockModelDefaultAndOverride(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)

	t.Run("default", func(t *testing.T) {
		h := &capturingHandler{t: t, body: sprintfBody(`ok`)}
		srv := httptest.NewServer(h)
		defer srv.Close()
		secrets := stubSecrets{
			"org-1/aws_bearer_token_bedrock": "tok",
			"org-1/aws_region":               "us-east-1",
			"org-1/bedrock_base_url":         srv.URL,
		}
		r := NewRecorder(nil)
		if _, err := r.Complete(context.Background(), completeOpts("org-1", secrets)); err != nil {
			t.Fatalf("Complete: %v", err)
		}
		if !strings.Contains(h.lastPath, defaultBedrockHaikuModel) {
			t.Errorf("path = %q, want it to reference the default model %q", h.lastPath, defaultBedrockHaikuModel)
		}
	})

	t.Run("override", func(t *testing.T) {
		h := &capturingHandler{t: t, body: sprintfBody(`ok`)}
		srv := httptest.NewServer(h)
		defer srv.Close()
		secrets := stubSecrets{
			"org-1/aws_bearer_token_bedrock": "tok",
			"org-1/aws_region":               "us-east-1",
			"org-1/bedrock_base_url":         srv.URL,
			"org-1/bedrock_model_id":         "custom.inference.profile",
		}
		r := NewRecorder(nil)
		if _, err := r.Complete(context.Background(), completeOpts("org-1", secrets)); err != nil {
			t.Fatalf("Complete: %v", err)
		}
		if !strings.Contains(h.lastPath, "custom.inference.profile") {
			t.Errorf("path = %q, want it to reference the overridden model", h.lastPath)
		}
	})
}

// TestComplete_Direct_BedrockCostAccounting_PricesOnPinnedModel is the
// regression guard for a Bedrock call recording $0 cost: the resolved
// request model is a Bedrock inference-profile id/ARN (the regional
// default, or an org's bedrock_model_id override) that has no entry in
// ai.pricing's table, so cost math must key off opts.DirectModel (the
// pinned Anthropic model id) instead.
//
// Can't use the real ai.CalculateCostUSD here — internal/ai imports
// internal/systemllm, and Go treats that as a genuine import cycle even for
// an internal (package systemllm) test file, not just production code. The
// stub below mirrors its exact contract instead: 0 for any model string it
// doesn't recognize, matching the account of the real bug (a resolved
// Bedrock model has no pricing-table entry → silently $0). Before the fix,
// CostFn was called with the resolved request model, so this stub would
// have returned 0 for both cases below; after the fix it's called with
// opts.DirectModel, matching the pinned constant.
func TestComplete_Direct_BedrockCostAccounting_PricesOnPinnedModel(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	const sentinelCost = 1.23

	run := func(t *testing.T, secrets stubSecrets, wantModelSubstr string) domain.SystemLLMRun {
		t.Helper()
		h := &capturingHandler{t: t, body: sprintfBody(`ok`)}
		srv := httptest.NewServer(h)
		defer srv.Close()
		secrets["org-1/bedrock_base_url"] = srv.URL

		fs := &fakeStore{}
		r := NewRecorder(fs)
		opts := completeOpts("org-1", secrets)
		pinnedModel := opts.DirectModel
		opts.CostFn = func(model string, _, _, _, _ int) float64 {
			if model != pinnedModel {
				return 0 // mirrors ai.CalculateCostUSD: unknown model → 0
			}
			return sentinelCost
		}
		if _, err := r.Complete(context.Background(), opts); err != nil {
			t.Fatalf("Complete: %v", err)
		}
		if !strings.Contains(h.lastPath, wantModelSubstr) {
			t.Errorf("path = %q, want it to reference %q", h.lastPath, wantModelSubstr)
		}
		if len(fs.rows) != 1 {
			t.Fatalf("recorded %d rows, want 1", len(fs.rows))
		}
		return fs.rows[0]
	}

	t.Run("default model", func(t *testing.T) {
		got := run(t, stubSecrets{
			"org-1/aws_bearer_token_bedrock": "tok",
			"org-1/aws_region":               "us-east-1",
		}, defaultBedrockHaikuModel)
		if got.TotalCostUSD != sentinelCost {
			t.Errorf("TotalCostUSD = %v, want %v (CostFn must be keyed on DirectModel, not the resolved Bedrock model %q)", got.TotalCostUSD, sentinelCost, defaultBedrockHaikuModel)
		}
		if got.Model != defaultBedrockHaikuModel {
			t.Errorf("Model = %q, want the resolved Bedrock model %q recorded for observability", got.Model, defaultBedrockHaikuModel)
		}
	})

	t.Run("bedrock_model_id override", func(t *testing.T) {
		got := run(t, stubSecrets{
			"org-1/aws_bearer_token_bedrock": "tok",
			"org-1/aws_region":               "us-east-1",
			"org-1/bedrock_model_id":         "custom.inference.profile",
		}, "custom.inference.profile")
		if got.TotalCostUSD != sentinelCost {
			t.Errorf("TotalCostUSD = %v, want %v (CostFn must be keyed on DirectModel, not the org's bedrock_model_id override)", got.TotalCostUSD, sentinelCost)
		}
		if got.Model != "custom.inference.profile" {
			t.Errorf("Model = %q, want the overridden Bedrock model recorded for observability", got.Model)
		}
	})
}

// TestComplete_Direct_NoCredentialsConfigured pins error propagation for an
// org with nothing in vault: ResolveCredentialsForBundle's
// ErrNoCredentialsConfigured must surface, unwrapped, to the caller.
func TestComplete_Direct_NoCredentialsConfigured(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	r := NewRecorder(nil)
	_, err := r.Complete(context.Background(), completeOpts("org-1", stubSecrets{}))
	if !errors.Is(err, agentproc.ErrNoCredentialsConfigured) {
		t.Fatalf("err = %v, want ErrNoCredentialsConfigured", err)
	}
}

// TestComplete_Direct_MissingSystemOrUserMessage pins the fast-fail guard
// for a caller that forgot to populate the multi-mode prompt split: no
// network call happens (the stub server fails the test if hit), and the
// error names the actual problem instead of surfacing whatever opaque
// rejection the SDK would give an empty system/user turn.
func TestComplete_Direct_MissingSystemOrUserMessage(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("no request should be sent when SystemPrompt/UserMessage is missing")
	}))
	defer srv.Close()

	secrets := stubSecrets{
		"org-1/anthropic_api_key":  "sk-ant-test",
		"org-1/anthropic_base_url": srv.URL,
	}

	t.Run("missing system prompt", func(t *testing.T) {
		opts := completeOpts("org-1", secrets)
		opts.SystemPrompt = ""
		if _, err := NewRecorder(nil).Complete(context.Background(), opts); err == nil {
			t.Fatal("expected an error when SystemPrompt is empty")
		}
	})

	t.Run("missing user message", func(t *testing.T) {
		opts := completeOpts("org-1", secrets)
		opts.UserMessage = ""
		if _, err := NewRecorder(nil).Complete(context.Background(), opts); err == nil {
			t.Fatal("expected an error when UserMessage is empty")
		}
	})
}

// TestComplete_Direct_MissingDirectModel pins the fast-fail guard for the
// Anthropic direct-API branch: a caller that forgot to set DirectModel must
// not send a request with an empty model — the stub server fails the test
// if hit.
func TestComplete_Direct_MissingDirectModel(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("no request should be sent when DirectModel is missing")
	}))
	defer srv.Close()

	secrets := stubSecrets{
		"org-1/anthropic_api_key":  "sk-ant-test",
		"org-1/anthropic_base_url": srv.URL,
	}
	opts := completeOpts("org-1", secrets)
	opts.DirectModel = ""
	if _, err := NewRecorder(nil).Complete(context.Background(), opts); err == nil {
		t.Fatal("expected an error when DirectModel is empty")
	}
}

// TestComplete_Direct_TerminalErrorNotRetried pins 4xx handling: a
// bad-request response is terminal (per the SDK's own retry policy) and
// must surface as an error after exactly one request — no retry storm on a
// caller/prompt bug.
func TestComplete_Direct_TerminalErrorNotRetried(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	h := &capturingHandler{t: t, status: http.StatusBadRequest, body: `{"type":"error","error":{"type":"invalid_request_error","message":"bad request"}}`}
	srv := httptest.NewServer(h)
	defer srv.Close()

	secrets := stubSecrets{
		"org-1/anthropic_api_key":  "sk-ant-test",
		"org-1/anthropic_base_url": srv.URL,
	}
	r := NewRecorder(nil)
	_, err := r.Complete(context.Background(), completeOpts("org-1", secrets))
	if err == nil {
		t.Fatal("expected an error for a 400 response")
	}
	if h.requests != 1 {
		t.Errorf("requests = %d, want exactly 1 (400 is terminal, not retried)", h.requests)
	}
}

// TestComplete_Direct_ContextCancelAbortsPromptly pins ctx cancellation:
// Complete must return promptly with the context error rather than hanging
// or retrying, when the caller's ctx is cancelled mid-flight.
func TestComplete_Direct_ContextCancelAbortsPromptly(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	unblock := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-unblock // never released within the test's timeout budget
	}))
	defer func() {
		close(unblock)
		srv.Close()
	}()

	secrets := stubSecrets{
		"org-1/anthropic_api_key":  "sk-ant-test",
		"org-1/anthropic_base_url": srv.URL,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	r := NewRecorder(nil)
	start := time.Now()
	_, err := r.Complete(ctx, completeOpts("org-1", secrets))
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected a context-deadline error")
	}
	if elapsed > 5*time.Second {
		t.Errorf("Complete took %v to abort; want prompt cancellation", elapsed)
	}
}

// sprintfBody builds a minimal-but-complete Anthropic Messages API response
// — one text content block plus a usage block exercising every token
// category RecordDirect reads — around the given (already-JSON-safe) text.
func sprintfBody(text string) string {
	return `{"id":"msg_test123","type":"message","role":"assistant","model":"claude-haiku-4-5-20251001","content":[{"type":"text","text":"` + text + `"}],"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":5,"cache_creation_input_tokens":2,"cache_read_input_tokens":3}}`
}

// TestComplete_Direct_LLMResolverUsed pins the TFAC-616 seam in the direct
// path: when CompleteOptions.LLMResolver is wired, completeDirect resolves
// the env map through it (minting for a role-mode Bedrock org) instead of the
// raw secret store. Here the raw secrets are EMPTY — only the resolver
// supplies credentials, so a fall-through to the raw path would 500.
func TestComplete_Direct_LLMResolverUsed(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	h := &capturingHandler{t: t, body: sprintfBody(`resolver ok`)}
	srv := httptest.NewServer(h)
	defer srv.Close()

	opts := completeOpts("org-1", stubSecrets{}) // empty raw secrets
	called := false
	opts.LLMResolver = func(_ context.Context, orgID string) (map[string]string, error) {
		called = true
		if orgID != "org-1" {
			t.Errorf("resolver got org %q", orgID)
		}
		return map[string]string{"ANTHROPIC_API_KEY": "sk-ant-minted", "ANTHROPIC_BASE_URL": srv.URL}, nil
	}
	r := NewRecorder(nil)
	result, err := r.Complete(context.Background(), opts)
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if !called {
		t.Fatal("LLMResolver was not consulted")
	}
	if result.Text != "resolver ok" {
		t.Errorf("Text = %q", result.Text)
	}
	if got := h.lastHeader.Get("X-Api-Key"); got != "sk-ant-minted" {
		t.Errorf("X-Api-Key = %q, want the resolver-supplied key", got)
	}
}
