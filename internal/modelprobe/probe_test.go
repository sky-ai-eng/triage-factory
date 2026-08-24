package modelprobe

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/modelcatalog"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/systemllm"
)

// stubSecrets is a minimal agentproc.SecretsReader over a map keyed "org/key".
type stubSecrets map[string]string

func (s stubSecrets) Get(_ context.Context, orgID, key string) (string, error) {
	return s[orgID+"/"+key], nil
}

// fakeProvider stands in for the Anthropic Messages API. It answers either a
// streamed one-token completion or a caller-chosen error status, and records
// the request body so a test can assert what the probe actually sent — which
// is half the contract: the exact catalog key, and a cap of one token.
type fakeProvider struct {
	status  int
	errBody string

	mu       sync.Mutex
	requests int
	lastBody map[string]any
}

func (f *fakeProvider) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	f.mu.Lock()
	f.requests++
	f.lastBody = body
	f.mu.Unlock()

	if f.status != 0 && f.status != http.StatusOK {
		errBody := f.errBody
		if errBody == "" {
			errBody = `{"type":"error","error":{"type":"api_error","message":"stub failure"}}`
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(f.status)
		_, _ = w.Write([]byte(errBody))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	for _, frame := range probeStreamFrames() {
		fmt.Fprint(w, frame)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
	}
}

func (f *fakeProvider) Requests() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests
}

func (f *fakeProvider) LastBody() map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastBody
}

// probeStreamFrames is a complete Anthropic stream for a one-token answer —
// what a real provider returns to the probe's request.
func probeStreamFrames() []string {
	return []string{
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_probe\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-sonnet-5\",\"content\":[],\"stop_reason\":null,\"usage\":{\"input_tokens\":8,\"output_tokens\":0}}}\n\n",
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"p\"}}\n\n",
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n",
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"max_tokens\"},\"usage\":{\"output_tokens\":1}}\n\n",
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
	}
}

// anthropicEntry is a native universe model served by Anthropic — read from the real
// catalog so these tests exercise the ids TF actually offers.
func anthropicEntry(t *testing.T) modelcatalog.Model {
	t.Helper()
	for _, e := range modelcatalog.UniverseFor(true).Models() {
		if e.Provider == modelcatalog.ProviderAnthropic {
			return e
		}
	}
	t.Fatal("catalog offers no Anthropic model")
	return modelcatalog.Model{}
}

// probeAgainst runs one probe against a fake provider answering with status.
func probeAgainst(t *testing.T, status int, errBody string) (*fakeProvider, modelcatalog.Model, Result, error) {
	t.Helper()
	runmode.SetForTest(t, runmode.ModeMulti)
	provider := &fakeProvider{status: status, errBody: errBody}
	srv := httptest.NewServer(provider)
	t.Cleanup(srv.Close)

	secrets := stubSecrets{
		"org-1/anthropic_api_key":  "sk-ant-probe",
		"org-1/anthropic_base_url": srv.URL,
	}
	entry := anthropicEntry(t)
	res, err := New(secrets, nil, nil).Probe(t.Context(), "org-1", entry)
	return provider, entry, res, err
}

// A provider that answers is green — and the request it answered is the one
// the probe promised: the exact catalog key, capped at one token.
func TestProbe_AnsweringProviderIsGreen(t *testing.T) {
	provider, entry, res, err := probeAgainst(t, http.StatusOK, "")
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if res.Verdict != VerdictGreen {
		t.Errorf("verdict = %q, want %q", res.Verdict, VerdictGreen)
	}
	if res.Detail != "" {
		t.Errorf("green carried detail %q, want none", res.Detail)
	}
	body := provider.LastBody()
	if got := body["model"]; got != entry.Key {
		t.Errorf("requested model = %v, want the exact catalog key %q", got, entry.Key)
	}
	if got, ok := body["max_tokens"].(float64); !ok || int(got) != probeMaxTokens {
		t.Errorf("max_tokens = %v, want %d — the probe must be the cheapest valid request", body["max_tokens"], probeMaxTokens)
	}
}

// The refusal classes. Each one is the provider ANSWERING that this credential
// may not invoke this model, which is exactly what red means.
func TestProbe_RefusalsAreRed(t *testing.T) {
	cases := map[string]struct {
		status  int
		errBody string
	}{
		"unauthenticated":                {http.StatusUnauthorized, `{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`},
		"forbidden":                      {http.StatusForbidden, `{"type":"error","error":{"type":"permission_error","message":"AccessDeniedException"}}`},
		"model_not_found":                {http.StatusNotFound, `{"type":"error","error":{"type":"not_found_error","message":"model not found"}}`},
		"bedrock_invalid_model_on_a_400": {http.StatusBadRequest, `{"type":"error","error":{"type":"invalid_request_error","message":"The provided model identifier is invalid."}}`},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			_, _, res, err := probeAgainst(t, c.status, c.errBody)
			if err != nil {
				t.Fatalf("Probe: %v", err)
			}
			if res.Verdict != VerdictRed {
				t.Fatalf("verdict = %q, want %q (body: %s)", res.Verdict, VerdictRed, c.errBody)
			}
			if res.Detail == "" {
				t.Error("red carried no detail; an admin needs the provider's own message to know what to fix")
			}
		})
	}
}

// The failure classes. None of them says anything about entitlement, so none of
// them may be recorded — one bad provider minute must not hide a model that
// works or block a save.
func TestProbe_FailuresAreInconclusive(t *testing.T) {
	for name, status := range map[string]int{
		"server_error":    http.StatusInternalServerError,
		"bad_gateway":     http.StatusBadGateway,
		"overloaded":      http.StatusServiceUnavailable,
		"rate_limited":    http.StatusTooManyRequests,
		"request_timeout": http.StatusRequestTimeout,
	} {
		t.Run(name, func(t *testing.T) {
			_, _, res, err := probeAgainst(t, status, "")
			if err != nil {
				t.Fatalf("Probe: %v", err)
			}
			if res.Verdict != VerdictInconclusive {
				t.Errorf("HTTP %d verdict = %q, want %q", status, res.Verdict, VerdictInconclusive)
			}
		})
	}
}

// A 5xx whose body quotes a refusal is still inconclusive: the rendered status
// settles the question, so a provider echoing an earlier AccessDeniedException
// in an outage cannot revoke a model nobody revoked.
func TestProbe_StatusBeatsTheRefusalMarkers(t *testing.T) {
	_, _, res, err := probeAgainst(t, http.StatusServiceUnavailable,
		`{"type":"error","error":{"type":"overloaded_error","message":"upstream said AccessDeniedException earlier"}}`)
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if res.Verdict != VerdictInconclusive {
		t.Errorf("verdict = %q, want %q — a 503 is an outage however its body reads", res.Verdict, VerdictInconclusive)
	}
}

// An org that never connected this model's provider gets an error rather than a
// verdict: nothing was asked, so there is nothing to record, and the remedy is
// a credential rather than a different model.
func TestProbe_UnconnectedProviderIsASetupGap(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	entry := anthropicEntry(t)
	_, err := New(stubSecrets{}, nil, nil).Probe(t.Context(), "org-1", entry)
	if err == nil {
		t.Fatal("probing an org with no credentials succeeded")
	}
	if !IsSetupGap(err) {
		t.Errorf("err = %v, want a setup gap", err)
	}
}

// A model served by a provider the org has not connected is refused by name,
// never quietly probed with the other provider's credentials — the same rule
// dispatch holds.
func TestProbe_WrongProviderIsNotSubstituted(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	var bedrock modelcatalog.Model
	for _, e := range modelcatalog.UniverseFor(true).Models() {
		if e.Provider == modelcatalog.ProviderBedrock {
			bedrock = e
			break
		}
	}
	if bedrock.Key == "" {
		t.Skip("catalog offers no Bedrock model")
	}
	secrets := stubSecrets{"org-1/anthropic_api_key": "sk-ant-probe"}
	_, err := New(secrets, nil, nil).Probe(t.Context(), "org-1", bedrock)
	if !errors.Is(err, agentproc.ErrProviderNotConfigured) {
		t.Fatalf("err = %v, want ErrProviderNotConfigured", err)
	}
	if !strings.Contains(err.Error(), bedrock.Key) {
		t.Errorf("refusal %q does not name the model", err)
	}
}

// A caller who walked away mid-probe gets an inconclusive, not whatever the
// transport reported on the way down. The alternative is a cancelled request
// recording a verdict about a model nobody heard back on.
func TestProbe_CancelledCallerIsInconclusive(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	blocked := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-blocked
	}))
	t.Cleanup(func() { close(blocked); srv.Close() })

	secrets := stubSecrets{
		"org-1/anthropic_api_key":  "sk-ant-probe",
		"org-1/anthropic_base_url": srv.URL,
	}
	ctx, cancel := context.WithCancel(t.Context())
	go cancel()
	res, err := New(secrets, nil, nil).Probe(ctx, "org-1", anthropicEntry(t))
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if res.Verdict != VerdictInconclusive {
		t.Errorf("verdict = %q, want %q", res.Verdict, VerdictInconclusive)
	}
}

// recordingStore captures the system_llm_runs rows a probe lands.
type recordingStore struct {
	mu   sync.Mutex
	rows []domain.SystemLLMRun
}

func (s *recordingStore) Record(_ context.Context, row domain.SystemLLMRun) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rows = append(s.rows, row)
	return nil
}

func (s *recordingStore) Rows() []domain.SystemLLMRun {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]domain.SystemLLMRun(nil), s.rows...)
}

// A probe is a real, billed request, so it lands in the same ledger every other
// call TF makes for itself does — under job=probe, which the spend view rolls
// up as system_overhead. An unrecorded probe would be the one charge on the
// org's bill with nothing in the product to explain it.
func TestProbe_RecordsItsSpend(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	srv := httptest.NewServer(&fakeProvider{})
	t.Cleanup(srv.Close)

	secrets := stubSecrets{
		"org-1/anthropic_api_key":  "sk-ant-probe",
		"org-1/anthropic_base_url": srv.URL,
	}
	store := &recordingStore{}
	entry := anthropicEntry(t)
	if _, err := New(secrets, nil, systemllm.NewRecorder(store)).Probe(t.Context(), "org-1", entry); err != nil {
		t.Fatalf("Probe: %v", err)
	}

	rows := store.Rows()
	if len(rows) != 1 {
		t.Fatalf("recorded %d ledger rows, want 1", len(rows))
	}
	row := rows[0]
	if row.Job != systemllm.JobProbe {
		t.Errorf("job = %q, want %q", row.Job, systemllm.JobProbe)
	}
	if row.OrgID != "org-1" {
		t.Errorf("org = %q, want org-1", row.OrgID)
	}
	// The model is the exact key probed, which is also the pricing key — the
	// catalog join guarantees every offered entry has a price, so a probe can
	// never record the silent $0 an unpriced id would.
	if row.Model != entry.Key {
		t.Errorf("model = %q, want %q", row.Model, entry.Key)
	}
	if row.TotalCostUSD <= 0 {
		t.Errorf("total_cost_usd = %v, want a real stamp for %d in / %d out tokens",
			row.TotalCostUSD, row.InputTokens, row.OutputTokens)
	}
	if row.InputTokens == 0 || row.OutputTokens == 0 {
		t.Errorf("tokens = %d in / %d out, want the provider's own counts", row.InputTokens, row.OutputTokens)
	}
	if row.IsError {
		t.Error("a successful probe recorded is_error")
	}
}

// A probe that does not come back with a completion still lands in the ledger,
// flagged is_error and stamped at zero — which is the TRUE cost, since a
// request rejected at the door consumes nothing. The row is what lets an
// operator see probes failing at all; without it a provider refusing every
// model would leave no trace anywhere but the availability table.
func TestProbe_RecordsARefusedCall(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	srv := httptest.NewServer(&fakeProvider{
		status:  http.StatusForbidden,
		errBody: `{"type":"error","error":{"type":"permission_error","message":"AccessDeniedException"}}`,
	})
	t.Cleanup(srv.Close)

	secrets := stubSecrets{
		"org-1/anthropic_api_key":  "sk-ant-probe",
		"org-1/anthropic_base_url": srv.URL,
	}
	store := &recordingStore{}
	res, err := New(secrets, nil, systemllm.NewRecorder(store)).Probe(t.Context(), "org-1", anthropicEntry(t))
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if res.Verdict != VerdictRed {
		t.Fatalf("verdict = %q, want %q", res.Verdict, VerdictRed)
	}
	rows := store.Rows()
	if len(rows) != 1 || !rows[0].IsError {
		t.Fatalf("ledger rows = %+v, want one flagged is_error", rows)
	}
	if rows[0].TotalCostUSD != 0 || rows[0].InputTokens != 0 || rows[0].OutputTokens != 0 {
		t.Errorf("refused probe recorded %v USD / %d in / %d out, want zeros — the provider rejected it before inference",
			rows[0].TotalCostUSD, rows[0].InputTokens, rows[0].OutputTokens)
	}
	if rows[0].Model != anthropicEntry(t).Key {
		t.Errorf("model = %q, want the probed key even on a failure", rows[0].Model)
	}
}

// The same holds for a probe nobody answered: an attempt that reached no
// verdict is still an attempt, and its row says so at zero rather than being
// dropped. This is the ledger's half of "an inconclusive writes nothing" — it
// writes no AVAILABILITY row; the attempt itself is still accounted for.
func TestProbe_RecordsAnInconclusiveCall(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	srv := httptest.NewServer(&fakeProvider{status: http.StatusInternalServerError})
	t.Cleanup(srv.Close)

	secrets := stubSecrets{
		"org-1/anthropic_api_key":  "sk-ant-probe",
		"org-1/anthropic_base_url": srv.URL,
	}
	store := &recordingStore{}
	res, err := New(secrets, nil, systemllm.NewRecorder(store)).Probe(t.Context(), "org-1", anthropicEntry(t))
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if res.Verdict != VerdictInconclusive {
		t.Fatalf("verdict = %q, want %q", res.Verdict, VerdictInconclusive)
	}
	rows := store.Rows()
	if len(rows) != 1 || !rows[0].IsError || rows[0].TotalCostUSD != 0 {
		t.Errorf("ledger rows = %+v, want one is_error row stamped at zero", rows)
	}
}

// A provider that answers a refusal with a wall of text does not get to put a
// wall of text on the row, which is stored and published on every catalog read.
func TestProbe_DetailIsBounded(t *testing.T) {
	_, _, res, err := probeAgainst(t, http.StatusForbidden,
		`{"type":"error","error":{"type":"permission_error","message":"AccessDeniedException `+strings.Repeat("x", 4000)+`"}}`)
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if res.Verdict != VerdictRed {
		t.Fatalf("verdict = %q, want %q", res.Verdict, VerdictRed)
	}
	if len(res.Detail) > detailLimit+len("… (truncated)") {
		t.Errorf("detail is %d bytes, want it bounded near %d", len(res.Detail), detailLimit)
	}
	if !strings.HasSuffix(res.Detail, "… (truncated)") {
		t.Error("a clipped detail does not say it was clipped")
	}
}

// stubRunSDK swaps the agent-runtime seam for the duration of one test.
func stubRunSDK(t *testing.T, fn func(context.Context, agentproc.RunOptions, agentproc.Sink) (*agentproc.Outcome, error)) {
	t.Helper()
	prev := runSDK
	runSDK = fn
	t.Cleanup(func() { runSDK = prev })
}

// Local mode spends its probe through the agent runtime, and hands it the
// invocation a local run would make: the exact catalog key, the one-token
// prompt, a single turn, no tool allowlist, and the org's own credential seams
// — which are nil here, exactly as they are for a local run, because the
// subprocess authenticates from the host's environment.
//
// Resolution is the runtime's to do. A probe that assembled its own request
// would be testing a credential path no local run takes, and would have nothing
// at all to assemble for the subscription case this exists to answer.
func TestProbe_LocalMode_InvokesTheRuntimeAsARunWould(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	entry := anthropicEntry(t)

	var got agentproc.RunOptions
	stubRunSDK(t, func(_ context.Context, opts agentproc.RunOptions, _ agentproc.Sink) (*agentproc.Outcome, error) {
		got = opts
		return &agentproc.Outcome{Result: &agentproc.Result{Result: "pong"}}, nil
	})

	res, err := New(nil, nil, nil).Probe(t.Context(), "org-1", entry)
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if res.Verdict != VerdictGreen {
		t.Errorf("verdict = %q, want %q", res.Verdict, VerdictGreen)
	}
	if got.Model != entry.Key {
		t.Errorf("model = %q, want the catalog key %q", got.Model, entry.Key)
	}
	if got.Message != probeMessage {
		t.Errorf("message = %q, want %q", got.Message, probeMessage)
	}
	if got.MaxTurns != probeMaxTurns {
		t.Errorf("max turns = %d, want %d", got.MaxTurns, probeMaxTurns)
	}
	if got.AllowedTools != "" {
		t.Errorf("allowed tools = %q, want none — a probe is one API call and nothing else", got.AllowedTools)
	}
	if got.OrgID != "org-1" {
		t.Errorf("org = %q, want org-1", got.OrgID)
	}
	if got.Secrets != nil || got.LLMResolver != nil {
		t.Error("the probe supplied credential seams a local run does not have")
	}
}

// A refusal the runtime carried is red, and the stored detail names the status
// — the runtime's own prose calls a 403 a temporary network issue.
func TestProbe_LocalMode_RefusalIsRed(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	stubRunSDK(t, func(context.Context, agentproc.RunOptions, agentproc.Sink) (*agentproc.Outcome, error) {
		return &agentproc.Outcome{Result: &agentproc.Result{
			IsError:        true,
			Subtype:        "success",
			APIErrorStatus: 403,
			Result:         "Authentication error · This may be a temporary network issue, please try again",
		}}, nil
	})

	res, err := New(nil, nil, nil).Probe(t.Context(), "org-1", anthropicEntry(t))
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if res.Verdict != VerdictRed {
		t.Fatalf("verdict = %q, want %q", res.Verdict, VerdictRed)
	}
	if !strings.Contains(res.Detail, "403") {
		t.Errorf("detail = %q, want the status in it", res.Detail)
	}
}

// A credential the runtime could not assemble is a setup gap on this transport
// too — an error rather than a verdict, so the caller reports a thing to go
// configure instead of recording the model as unavailable.
func TestProbe_LocalMode_SetupGapIsAnErrorNotAVerdict(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	stubRunSDK(t, func(context.Context, agentproc.RunOptions, agentproc.Sink) (*agentproc.Outcome, error) {
		return nil, fmt.Errorf("agent runtime: %w", agentproc.ErrNoCredentialsConfigured)
	})

	res, err := New(nil, nil, nil).Probe(t.Context(), "org-1", anthropicEntry(t))
	if err == nil {
		t.Fatalf("Probe returned verdict %q, want a setup-gap error", res.Verdict)
	}
	if !IsSetupGap(err) {
		t.Errorf("IsSetupGap(%v) = false, want true", err)
	}
}
