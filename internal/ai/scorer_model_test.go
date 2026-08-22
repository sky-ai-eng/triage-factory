package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/systemllm"
)

// anthropicStub stands in for the Messages API: it records the request body and
// answers with a streamed completion, since the inference client always streams.
// It is what makes "the scorer's batch call carries the org's model" assertable
// without a live call.
type anthropicStub struct {
	body chan map[string]any
	text string
}

func newAnthropicStub(t *testing.T, text string) (*anthropicStub, string) {
	t.Helper()
	h := &anthropicStub{body: make(chan map[string]any, 1), text: text}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return h, srv.URL
}

func (h *anthropicStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	select {
	case h.body <- body:
	default:
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	for _, frame := range []string{
		`event: message_start` + "\n" + `data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"m","content":[],"stop_reason":null,"usage":{"input_tokens":1,"output_tokens":0}}}` + "\n\n",
		`event: content_block_start` + "\n" + `data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n",
		`event: content_block_delta` + "\n" + `data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"` + h.text + `"}}` + "\n\n",
		`event: content_block_stop` + "\n" + `data: {"type":"content_block_stop","index":0}` + "\n\n",
		`event: message_delta` + "\n" + `data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}` + "\n\n",
		`event: message_stop` + "\n" + `data: {"type":"message_stop"}` + "\n\n",
	} {
		fmt.Fprint(w, frame)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}
}

// gatewaySecrets points the org's Anthropic credential at a local stub server.
type gatewaySecrets struct{ baseURL string }

func (s gatewaySecrets) Get(_ context.Context, _, key string) (string, error) {
	switch key {
	case "anthropic_api_key":
		return "sk-ant-test", nil
	case "anthropic_base_url":
		return s.baseURL, nil
	}
	return "", nil
}

// TestScoreBatch_SendsTheOrgsModel is the multi-mode half of the knob: the model
// resolved for the cycle is the model the provider request actually carries.
// Nothing between the setting and the wire re-derives it, so a batch that scored
// on a model the org did not choose would fail here.
func TestScoreBatch_SendsTheOrgsModel(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	stub, baseURL := newAnthropicStub(t, `[]`)

	const model = domain.ModelOpus
	if _, err := scoreBatch(
		context.Background(),
		[]TaskInput{{ID: "t-1", Title: "a task"}},
		"org-1", model,
		gatewaySecrets{baseURL: baseURL},
		nil, systemllm.NewRecorder(nil), nil,
	); err != nil {
		t.Fatalf("scoreBatch: %v", err)
	}

	body := <-stub.body
	if got := body["model"]; got != model {
		t.Errorf("request model = %v, want the org's background-jobs model %q", got, model)
	}
}

// TestRun_NoModelSkipsTheCycle pins the other half: an org with no usable
// background-jobs model gets a skipped cycle, not a substituted model.
//
// Skipping means skipping cleanly — the tasks are never claimed (so nothing has
// to be reset back to pending), no error callback fires (this is a
// configuration state, not a failure of the cycle's machinery), and a WARN names
// the setting so the remedy is in the log rather than only in the code.
func TestRun_NoModelSkipsTheCycle(t *testing.T) {
	logs := &captureHandler{}
	prev := aiLog
	aiLog = slog.New(logs)
	t.Cleanup(func() { aiLog = prev })

	store := &stubScoreStore{tasks: []domain.Task{{ID: "t-1"}}}
	var errored bool
	r := NewRunner(store, nil, "org-x", nil, nil, nil, nil,
		func(context.Context, string) (string, error) {
			return "", fmt.Errorf("%w: the background jobs model setting is empty", systemllm.ErrNoModel)
		},
		RunnerCallbacks{OnError: func(string, error) { errored = true }})

	var scored int32
	r.scoreFn = func(context.Context, []TaskInput, string, string, agentproc.SecretsReader) ([]TaskScore, error) {
		atomic.AddInt32(&scored, 1)
		return nil, nil
	}

	r.run(context.Background())

	if got := atomic.LoadInt32(&scored); got != 0 {
		t.Errorf("scoreFn called %d times; want 0 — a cycle with no model must not score", got)
	}
	if errored {
		t.Error("OnError fired; an unpicked setting is a configuration state, not a cycle failure")
	}
	var warned bool
	for _, rec := range logs.recorded() {
		if rec.Level == slog.LevelWarn && strings.Contains(rec.Message, "skipping scoring cycle") {
			warned = true
		}
	}
	if !warned {
		t.Errorf("no WARN naming the skip; records = %+v", logs.recorded())
	}
}
