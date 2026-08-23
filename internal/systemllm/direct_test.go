package systemllm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/inference"
	"github.com/sky-ai-eng/triage-factory/internal/modelcatalog"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// stubSecrets is a minimal agentproc.SecretsReader backed by an in-memory
// map, keyed "org/key" — enough for the credential precedence to exercise
// every provider branch without a real store.
type stubSecrets map[string]string

func (s stubSecrets) Get(_ context.Context, orgID, key string) (string, error) {
	return s[orgID+"/"+key], nil
}

// capturingHandler stands in for the Anthropic Messages API: it records the
// last request it served (path, headers, decoded body) and answers with
// either a caller-supplied error status or a streamed completion, since the
// inference client always streams.
//
// ServeHTTP runs on a goroutine spawned by the httptest server, distinct from
// the test goroutine that reads these fields back — mu guards every access so
// that's race-free rather than relying on the request/response round trip to
// provide ordering the race detector doesn't recognize.
type capturingHandler struct {
	t      *testing.T
	status int
	// text is the assistant text the streamed response carries. Ignored when
	// status is an error status.
	text string
	// errBody overrides the default error payload for an error status.
	errBody string

	mu            sync.Mutex
	lastReqHeader http.Header
	lastReqPath   string
	lastReqBody   map[string]any
	reqCount      int
}

func (h *capturingHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	var body map[string]any
	_ = json.Unmarshal(raw, &body)

	h.mu.Lock()
	h.reqCount++
	h.lastReqHeader = r.Header.Clone()
	h.lastReqPath = r.URL.Path
	h.lastReqBody = body
	h.mu.Unlock()

	if h.status != 0 && h.status != http.StatusOK {
		errBody := h.errBody
		if errBody == "" {
			errBody = `{"type":"error","error":{"type":"invalid_request_error","message":"stub error"}}`
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(h.status)
		_, _ = w.Write([]byte(errBody))
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	for _, frame := range anthropicStreamFrames(h.text) {
		fmt.Fprint(w, frame)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}
}

// Requests returns the number of requests served so far. Safe for
// concurrent use with ServeHTTP.
func (h *capturingHandler) Requests() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.reqCount
}

// LastHeader returns the most recently served request's headers. Safe for
// concurrent use with ServeHTTP.
func (h *capturingHandler) LastHeader() http.Header {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.lastReqHeader
}

// LastPath returns the most recently served request's URL path. Safe for
// concurrent use with ServeHTTP.
func (h *capturingHandler) LastPath() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.lastReqPath
}

// LastBody returns the most recently served request's decoded JSON body. Safe
// for concurrent use with ServeHTTP.
func (h *capturingHandler) LastBody() map[string]any {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.lastReqBody
}

// anthropicStreamFrames builds a complete Anthropic streaming response around
// the given (already-JSON-safe) text: one text block plus a usage block
// exercising every token category RecordDirect reads.
func anthropicStreamFrames(text string) []string {
	return []string{
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_test123\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-haiku-4-5-20251001\",\"content\":[],\"stop_reason\":null,\"usage\":{\"input_tokens\":10,\"output_tokens\":0,\"cache_creation_input_tokens\":2,\"cache_read_input_tokens\":3}}}\n\n",
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"" + text + "\"}}\n\n",
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n",
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":5}}\n\n",
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
	}
}

func completeOpts(orgID string, secrets agentproc.SecretsReader) CompleteOptions {
	return CompleteOptions{
		OrgID:        orgID,
		Job:          JobRepoProfiler,
		Message:      "combined local prompt",
		SystemPrompt: "system instructions",
		UserMessage:  "user data",
		Model:        domain.ModelHaiku,
		MaxTokens:    2048,
		Temperature:  0.1,
		TraceID:      "test-trace",
		Secrets:      secrets,
	}
}

// TestComplete_Direct_AnthropicAPIKey pins the Anthropic direct-key path:
// the resolved api key goes out as x-api-key, the configured base URL is
// honored, and the streamed response text passes through untouched.
func TestComplete_Direct_AnthropicAPIKey(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	h := &capturingHandler{t: t, text: "hello world"}
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
	if got := h.LastHeader().Get("X-Api-Key"); got != "sk-ant-test" {
		t.Errorf("x-api-key = %q, want sk-ant-test", got)
	}
	if h.LastHeader().Get("Authorization") != "" {
		t.Errorf("Authorization should be unset with no auth token configured, got %q", h.LastHeader().Get("Authorization"))
	}
}

// TestComplete_Direct_AnthropicAuthTokenBearer pins the gateway bearer path:
// when anthropic_auth_token is configured alongside the api key, both the
// x-api-key header AND an Authorization: Bearer header go out — matching
// resolveCredentials, which sets both env vars together rather than
// treating them as alternatives.
func TestComplete_Direct_AnthropicAuthTokenBearer(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	h := &capturingHandler{t: t, text: "auth token payload"}
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
	if got := h.LastHeader().Get("X-Api-Key"); got != "sk-ant-test" {
		t.Errorf("x-api-key = %q, want sk-ant-test", got)
	}
	if got := h.LastHeader().Get("Authorization"); got != "Bearer gateway-bearer-tok" {
		t.Errorf("Authorization = %q, want Bearer gateway-bearer-tok", got)
	}
}

// TestComplete_Direct_RequestShape pins what a system job actually sends: the
// org's background-jobs model, the prompt split (instructions as the system
// prompt, data as the single user turn), the token cap, and — the one
// easily-dropped field — the caller's temperature.
func TestComplete_Direct_RequestShape(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	h := &capturingHandler{t: t, text: "ok"}
	srv := httptest.NewServer(h)
	defer srv.Close()

	secrets := stubSecrets{
		"org-1/anthropic_api_key":  "sk-ant-test",
		"org-1/anthropic_base_url": srv.URL,
	}
	if _, err := NewRecorder(nil).Complete(context.Background(), completeOpts("org-1", secrets)); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	body := h.LastBody()
	if got := body["model"]; got != domain.ModelHaiku {
		t.Errorf("model = %v, want the org's background-jobs model", got)
	}
	if got, ok := body["temperature"].(float64); !ok || got != 0.1 {
		t.Errorf("temperature = %v (present=%v), want the caller's 0.1", body["temperature"], ok)
	}
	if got, ok := body["max_tokens"].(float64); !ok || int(got) != 2048 {
		t.Errorf("max_tokens = %v, want 2048", body["max_tokens"])
	}
	if !strings.Contains(fmt.Sprint(body["system"]), "system instructions") {
		t.Errorf("system = %v, want the SystemPrompt", body["system"])
	}
	msgs, _ := body["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("messages = %v, want exactly the one synthetic user row", body["messages"])
	}
	msg, _ := msgs[0].(map[string]any)
	if msg["role"] != "user" || !strings.Contains(fmt.Sprint(msg["content"]), "user data") {
		t.Errorf("user message = %v, want the UserMessage as a user turn", msg)
	}
	if body["tools"] != nil || body["tool_choice"] != nil {
		t.Errorf("a system job is toolless: tools = %v, tool_choice = %v", body["tools"], body["tool_choice"])
	}
}

// TestComplete_Direct_NoCacheBreakpointOnTheData pins that the one thing a
// system job never sends again — this call's own data — is not marked as a
// cache write. The system prefix repeats across calls of the same job and
// keeps its breakpoint; the tail does not repeat, so a breakpoint there is a
// write premium on tokens nothing will ever read back.
func TestComplete_Direct_NoCacheBreakpointOnTheData(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	h := &capturingHandler{t: t, text: "ok"}
	srv := httptest.NewServer(h)
	defer srv.Close()

	secrets := stubSecrets{
		"org-1/anthropic_api_key":  "sk-ant-test",
		"org-1/anthropic_base_url": srv.URL,
	}
	if _, err := NewRecorder(nil).Complete(context.Background(), completeOpts("org-1", secrets)); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	body := h.LastBody()
	if !strings.Contains(fmt.Sprint(body["system"]), "cache_control") {
		t.Errorf("system = %v, want the repeated prefix cached", body["system"])
	}
	if got := fmt.Sprint(body["messages"]); strings.Contains(got, "cache_control") {
		t.Errorf("messages = %v, want no cache breakpoint on the per-call data", got)
	}
}

// TestComplete_Direct_RecordsSuccessRow pins the system_llm_runs row a
// successful direct call produces: the provider's usage verbatim, a cost
// priced from the vendored datasheet, the response id as the idempotency
// key, and the single-turn/no-error shape.
func TestComplete_Direct_RecordsSuccessRow(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	h := &capturingHandler{t: t, text: "ok"}
	srv := httptest.NewServer(h)
	defer srv.Close()

	secrets := stubSecrets{
		"org-1/anthropic_api_key":  "sk-ant-test",
		"org-1/anthropic_base_url": srv.URL,
	}
	fs := &fakeStore{}
	opts := completeOpts("org-1", secrets)
	opts.Metadata = map[string]any{"batch_size": 10}
	if _, err := NewRecorder(fs).Complete(context.Background(), opts); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	if len(fs.rows) != 1 {
		t.Fatalf("recorded %d rows, want 1", len(fs.rows))
	}
	got := fs.rows[0]
	if got.OrgID != "org-1" || got.Job != JobRepoProfiler || got.Model != domain.ModelHaiku {
		t.Errorf("context fields wrong: %+v", got)
	}
	// The fixture reports 10 input + 3 cache-read + 2 cache-write; the neutral
	// layer folds the cache buckets into its prompt count (15), and the row
	// records the four disjoint counts the column has always meant.
	if got.InputTokens != 10 || got.OutputTokens != 5 || got.CacheReadTokens != 3 || got.CacheCreationTokens != 2 {
		t.Errorf("token fields = %+v, want 10/5/3/2 as disjoint counts", got)
	}
	// Cost, by contrast, prices the inclusive usage — the formula subtracts
	// the cache buckets back out at their own rates.
	wantCost, ok := inference.CostForUsage(opts.Model, inference.Usage{
		PromptTokens: 15, OutputTokens: 5, CacheReadTokens: 3, CacheCreationTokens: 2,
	})
	if !ok {
		t.Fatalf("the system-job model %q must be priceable from the vendored datasheet", opts.Model)
	}
	if got.TotalCostUSD != wantCost {
		t.Errorf("TotalCostUSD = %v, want %v", got.TotalCostUSD, wantCost)
	}
	if got.TraceID != "msg_test123" {
		t.Errorf("TraceID = %q, want the provider's message id", got.TraceID)
	}
	if got.NumTurns != 1 || got.IsError {
		t.Errorf("NumTurns = %d, IsError = %v, want a single successful turn", got.NumTurns, got.IsError)
	}
	if got.MetadataJSON != `{"batch_size":10}` {
		t.Errorf("MetadataJSON = %q", got.MetadataJSON)
	}
	if got.DurationMs < 0 || got.CompletedAt.Before(got.StartedAt) {
		t.Errorf("timing fields wrong: %+v", got)
	}
}

// TestComplete_Direct_RecordsErrorRow pins the other half: a failed call
// still lands a row, flagged is_error, with no invented usage or cost and no
// idempotency key (there was no response to key on).
func TestComplete_Direct_RecordsErrorRow(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	h := &capturingHandler{t: t, status: http.StatusBadRequest}
	srv := httptest.NewServer(h)
	defer srv.Close()

	secrets := stubSecrets{
		"org-1/anthropic_api_key":  "sk-ant-test",
		"org-1/anthropic_base_url": srv.URL,
	}
	fs := &fakeStore{}
	if _, err := NewRecorder(fs).Complete(context.Background(), completeOpts("org-1", secrets)); err == nil {
		t.Fatal("expected an error for a 400 response")
	}

	if len(fs.rows) != 1 {
		t.Fatalf("recorded %d rows, want 1 — a failed call is still accounted for", len(fs.rows))
	}
	got := fs.rows[0]
	if !got.IsError {
		t.Error("IsError = false, want the row flagged")
	}
	if got.InputTokens != 0 || got.OutputTokens != 0 || got.TotalCostUSD != 0 {
		t.Errorf("usage/cost = %+v, want zeros for a call that produced no response", got)
	}
	if got.TraceID != "" {
		t.Errorf("TraceID = %q, want empty — no response, no idempotency key", got.TraceID)
	}
	if got.Model != domain.ModelHaiku || got.Job != JobRepoProfiler {
		t.Errorf("context fields wrong: %+v", got)
	}
}

// TestRecordDirectCall_PricesOnTheRequestedModel pins that the ledger charges
// what actually ran: the model the request carried is both the row's model and
// the pricing key, for every provider. The knob is a catalog key and the catalog
// only offers keys the datasheet prices, so the two can't come apart.
func TestRecordDirectCall_PricesOnTheRequestedModel(t *testing.T) {
	usage := inference.Usage{PromptTokens: 1000, OutputTokens: 500, CacheReadTokens: 100, CacheCreationTokens: 50}

	for _, model := range modelcatalog.Keys() {
		t.Run(model, func(t *testing.T) {
			wantCost, ok := inference.CostForUsage(model, usage)
			if !ok {
				t.Fatalf("catalog model %q must be priceable from the vendored datasheet", model)
			}
			fs := &fakeStore{}
			opts := completeOpts("org-1", nil)
			opts.Model = model
			NewRecorder(fs).recordDirectCall(context.Background(), opts, time.Now().UTC(), 42,
				&inference.Completion{ID: "msg_x", Usage: usage}, nil)

			if len(fs.rows) != 1 {
				t.Fatalf("recorded %d rows, want 1", len(fs.rows))
			}
			got := fs.rows[0]
			if got.TotalCostUSD != wantCost {
				t.Errorf("TotalCostUSD = %v, want %v", got.TotalCostUSD, wantCost)
			}
			if got.Model != model {
				t.Errorf("Model = %q, want the model the request carried", got.Model)
			}
		})
	}
}

// TestDirectUsageFrom pins the projection between the two token conventions:
// the neutral layer's prompt count includes the cache buckets, the row's
// columns are disjoint. Getting this backwards double-counts every cached
// token in the accounting table without failing anything else.
func TestDirectUsageFrom(t *testing.T) {
	got := DirectUsageFrom(inference.Usage{
		PromptTokens: 1000, OutputTokens: 200, CacheReadTokens: 300, CacheCreationTokens: 100,
	})
	want := DirectUsage{InputTokens: 600, OutputTokens: 200, CacheReadTokens: 300, CacheCreationTokens: 100}
	if got != want {
		t.Errorf("DirectUsageFrom = %+v, want %+v", got, want)
	}

	// A provider that already reports prompt tokens exclusively (or a
	// malformed payload) must not produce a negative count.
	if got := DirectUsageFrom(inference.Usage{PromptTokens: 10, CacheReadTokens: 400}); got.InputTokens != 0 {
		t.Errorf("InputTokens = %d, want 0 rather than a negative count", got.InputTokens)
	}
}

// TestMapDirectCreds pins the two things the credential mapping decides for a
// system job: the key it builds whitelists the model the request will carry
// (bifrost reads an empty whitelist as "no models", so a missing one fails at
// request time with a confusing no-key-for-model error), and credentials for a
// provider other than the one the catalog says serves the model are refused
// rather than used — resolution already picked the provider from the model, so
// a mismatch is a bug, and buying the request anyway is the substitution the
// no-fallback rule forbids.
func TestMapDirectCreds(t *testing.T) {
	anthropicKey := map[string]string{"ANTHROPIC_API_KEY": "k"}
	bedrockBearer := map[string]string{"AWS_BEARER_TOKEN_BEDROCK": "t", "AWS_REGION": "us-east-1"}
	bedrockSigV4 := map[string]string{"AWS_ACCESS_KEY_ID": "a", "AWS_SECRET_ACCESS_KEY": "s", "AWS_REGION": "us-east-1"}

	t.Run("every catalog model maps to the provider that serves it", func(t *testing.T) {
		for _, e := range modelcatalog.Entries() {
			var creds map[string]string
			switch e.Provider {
			case modelcatalog.ProviderAnthropic:
				creds = anthropicKey
			case modelcatalog.ProviderBedrock:
				creds = bedrockBearer
			default:
				t.Fatalf("%s: no credential shape for provider %q", e.Key, e.Provider)
			}
			pc, err := mapDirectCreds(creds, e.Key)
			if err != nil {
				t.Errorf("%s: %v", e.Key, err)
				continue
			}
			if string(pc.Provider) != e.Provider {
				t.Errorf("%s: provider = %q, want %q", e.Key, pc.Provider, e.Provider)
			}
			if !slices.Contains(pc.Models, e.Key) {
				t.Errorf("%s: key whitelist %v does not carry the requested model", e.Key, pc.Models)
			}
		}
	})

	t.Run("a provider mismatch is refused", func(t *testing.T) {
		anthropicModel := modelOn(t, modelcatalog.ProviderAnthropic)
		bedrockModel := modelOn(t, modelcatalog.ProviderBedrock)
		for _, tc := range []struct {
			name  string
			creds map[string]string
			model string
		}{
			{"bedrock model, anthropic credentials", anthropicKey, bedrockModel},
			{"anthropic model, bedrock bearer", bedrockBearer, anthropicModel},
			{"anthropic model, bedrock sigv4", bedrockSigV4, anthropicModel},
		} {
			t.Run(tc.name, func(t *testing.T) {
				if _, err := mapDirectCreds(tc.creds, tc.model); err == nil {
					t.Fatal("expected a refusal, got credentials")
				}
			})
		}
	})
}

// modelOn returns an offered model served by provider.
func modelOn(t *testing.T, provider string) string {
	t.Helper()
	for _, e := range modelcatalog.Entries() {
		if e.Provider == provider {
			return e.Key
		}
	}
	t.Fatalf("catalog offers no model on %s", provider)
	return ""
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

// TestComplete_Direct_ResolverReturnedNothing pins the other empty-map route
// in: a resolver that answers with no provider material must fail before any
// request, as the credentials class it is rather than as an opaque provider
// rejection.
func TestComplete_Direct_ResolverReturnedNothing(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	opts := completeOpts("org-1", stubSecrets{})
	opts.LLMResolver = func(context.Context, string, string) (map[string]string, error) {
		return map[string]string{}, nil
	}
	_, err := NewRecorder(nil).Complete(context.Background(), opts)
	if !errors.Is(err, inference.ErrNoCredentials) {
		t.Fatalf("err = %v, want ErrNoCredentials", err)
	}
}

// TestResolveDirectCreds_BedrockOnlyOrg is the case the pinned-Haiku system
// jobs could not serve: an org that holds only AWS material. Its knob names a
// Bedrock catalog key, and that key is what selects the provider — so
// resolution returns Bedrock material and the mapping whitelists the very model
// the request will carry, with nothing anywhere re-deriving a spelling.
func TestResolveDirectCreds_BedrockOnlyOrg(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	bedrockModel := modelOn(t, modelcatalog.ProviderBedrock)
	secrets := stubSecrets{
		"org-1/aws_bearer_token_bedrock": "bedrock-tok",
		"org-1/aws_region":               "eu-central-1",
	}

	opts := completeOpts("org-1", secrets)
	opts.Model = bedrockModel
	creds, err := resolveDirectCreds(context.Background(), opts)
	if err != nil {
		t.Fatalf("resolveDirectCreds: %v", err)
	}
	pc, err := mapDirectCreds(creds, opts.Model)
	if err != nil {
		t.Fatalf("mapDirectCreds: %v", err)
	}
	if pc.Provider != inference.ProviderBedrock {
		t.Errorf("provider = %q, want bedrock", pc.Provider)
	}
	if pc.Bedrock == nil || pc.Bedrock.Region != "eu-central-1" {
		t.Errorf("bedrock credentials = %+v, want the org's region", pc.Bedrock)
	}
	if !slices.Contains(pc.Models, bedrockModel) {
		t.Errorf("key whitelist %v does not carry the requested model %q", pc.Models, bedrockModel)
	}
}

// TestResolveDirectCreds_ModelWithoutItsProviderIsRefused is the same org from
// the other side: it picked an Anthropic model and holds no Anthropic
// credential. Nothing substitutes the Bedrock material it does hold — the call
// is refused, naming the provider to connect.
func TestResolveDirectCreds_ModelWithoutItsProviderIsRefused(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	secrets := stubSecrets{
		"org-1/aws_bearer_token_bedrock": "bedrock-tok",
		"org-1/aws_region":               "eu-central-1",
	}

	opts := completeOpts("org-1", secrets)
	opts.Model = modelOn(t, modelcatalog.ProviderAnthropic)
	if _, err := resolveDirectCreds(context.Background(), opts); !errors.Is(err, agentproc.ErrProviderNotConfigured) {
		t.Fatalf("err = %v, want ErrProviderNotConfigured", err)
	}
}

// TestComplete_Direct_MissingSystemOrUserMessage pins the fast-fail guard
// for a caller that forgot to populate the multi-mode prompt split: no
// network call happens (the stub server fails the test if hit), and the
// error names the actual problem instead of surfacing whatever opaque
// rejection the provider would give an empty system/user turn.
func TestComplete_Direct_MissingSystemOrUserMessage(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("no request should be sent when SystemPrompt/UserMessage is missing")
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

// TestComplete_MissingModel pins the fast-fail guard for a caller whose org has
// no background-jobs model resolved: no request may go out — the stub server
// fails the test if hit — and the refusal is ErrNoModel in BOTH modes, because
// an empty --model is a model nobody chose just as much as an empty request
// model is.
func TestComplete_MissingModel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("no request should be sent when the model is missing")
	}))
	defer srv.Close()

	secrets := stubSecrets{
		"org-1/anthropic_api_key":  "sk-ant-test",
		"org-1/anthropic_base_url": srv.URL,
	}
	for _, mode := range []runmode.Mode{runmode.ModeMulti, runmode.ModeLocal} {
		t.Run(string(mode), func(t *testing.T) {
			runmode.SetForTest(t, mode)
			orig := runLocal
			runLocal = func(context.Context, agentproc.RunOptions, agentproc.Sink) (*agentproc.Outcome, error) {
				t.Error("no subprocess should be spawned when the model is missing")
				return nil, nil
			}
			t.Cleanup(func() { runLocal = orig })

			opts := completeOpts("org-1", secrets)
			opts.Model = ""
			_, err := NewRecorder(nil).Complete(context.Background(), opts)
			if !errors.Is(err, ErrNoModel) {
				t.Fatalf("err = %v, want ErrNoModel", err)
			}
		})
	}
}

// TestComplete_Direct_TerminalErrorNotRetried pins 4xx handling: a
// bad-request response is terminal and must surface as an error after
// exactly one request — no retry storm on a caller/prompt bug.
func TestComplete_Direct_TerminalErrorNotRetried(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	h := &capturingHandler{t: t, status: http.StatusBadRequest, errBody: `{"type":"error","error":{"type":"invalid_request_error","message":"bad request"}}`}
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
	if h.Requests() != 1 {
		t.Errorf("requests = %d, want exactly 1 (400 is terminal, not retried)", h.Requests())
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

// TestComplete_Direct_LLMResolverUsed pins the TFAC-616 seam in the direct
// path: when CompleteOptions.LLMResolver is wired, completeDirect resolves
// the env map through it (minting for a role-mode Bedrock org) instead of the
// raw secret store. Here the raw secrets are EMPTY — only the resolver
// supplies credentials, so a fall-through to the raw path would fail.
func TestComplete_Direct_LLMResolverUsed(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	h := &capturingHandler{t: t, text: "resolver ok"}
	srv := httptest.NewServer(h)
	defer srv.Close()

	opts := completeOpts("org-1", stubSecrets{}) // empty raw secrets
	called := false
	opts.LLMResolver = func(_ context.Context, orgID, _ string) (map[string]string, error) {
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
	if got := h.LastHeader().Get("X-Api-Key"); got != "sk-ant-minted" {
		t.Errorf("x-api-key = %q, want the resolver-supplied key", got)
	}
}

// TestCompletionText pins the text extraction: a plain string passes through,
// and a block-shaped message concatenates its text blocks in order while
// ignoring anything that isn't text.
func TestCompletionText(t *testing.T) {
	if got := completionText(nil); got != "" {
		t.Errorf("nil completion = %q, want empty", got)
	}
	if got := completionText(&inference.Completion{}); got != "" {
		t.Errorf("contentless completion = %q, want empty", got)
	}

	rows, err := inference.RowsToMessages([]domain.Message{
		{Role: "assistant", Content: "part one", ContentBlocks: []domain.ContentBlock{
			{Type: domain.ContentBlockImage, ImageURL: &domain.ContentImageURL{URL: "data:image/png;base64,eA=="}},
		}},
	}, inference.AssemblyOptions{NoCacheBreakpoint: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := completionText(&inference.Completion{Message: rows[0]}); got != "part one" {
		t.Errorf("block-shaped completion = %q, want the text blocks only", got)
	}
}
