package projectclassify

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

	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/systemllm"
)

// anthropicStub stands in for the Messages API: it records the request body and
// answers with a streamed completion, since the inference client always streams.
// It is what makes "the classifier's vote carries the org's model" assertable
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

// TestRunVote_SendsTheOrgsModel is the multi-mode half of the knob: the model
// resolved for the cycle is the model the provider request actually carries.
// Nothing between the setting and the wire re-derives it.
func TestRunVote_SendsTheOrgsModel(t *testing.T) {
	isolateHome(t)
	runmode.SetForTest(t, runmode.ModeMulti)
	stub, baseURL := newAnthropicStub(t, `{\"score\":10,\"rationale\":\"no\"}`)

	const model = domain.ModelOpus
	r := NewRunner(nil, nil, "org-1", gatewaySecrets{baseURL: baseURL}, nil, systemllm.NewRecorder(nil), nil, fixedModel)
	if _, _, err := r.runVote(context.Background(), "org-1", votePrompt{
		SystemPrompt: "instructions", UserMessage: "the entity",
	}, model); err != nil {
		t.Fatalf("runVote: %v", err)
	}

	body := <-stub.body
	if got := body["model"]; got != model {
		t.Errorf("request model = %v, want the org's background-jobs model %q", got, model)
	}
}

// TestRun_NoModelSkipsTheCycle pins the other half: an org with no usable
// background-jobs model gets a skipped cycle, not a substituted model.
//
// Skipping leaves classified_at NULL — exactly what a fully-errored cycle
// leaves — so every entity resurfaces once a model is picked, and a WARN names
// the setting so the remedy is in the log rather than only in the code.
func TestRun_NoModelSkipsTheCycle(t *testing.T) {
	isolateHome(t)
	database := newTestDB(t)
	stores := sqlitestore.New(database)
	if _, err := stores.Projects.Create(t.Context(), runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, domain.Project{ID: "p1", Name: "P1"}); err != nil {
		t.Fatal(err)
	}
	entity, _, err := stores.Entities.FindOrCreate(context.Background(), runmode.LocalDefaultOrgID, "github", "owner/repo#1", "pr", "T", "https://x/1")
	if err != nil {
		t.Fatal(err)
	}

	logs := &captureHandler{}
	prev := classifyLog
	classifyLog = slog.New(logs)
	t.Cleanup(func() { classifyLog = prev })

	r := NewRunner(stores.Entities, stores.Projects, runmode.LocalDefaultOrgID, nil, nil, nil, nil,
		func(context.Context, string) (string, error) {
			return "", fmt.Errorf("%w: the background jobs model setting is empty", systemllm.ErrNoModel)
		})
	var votes int32
	r.stage1Fn = func(context.Context, string, votePrompt, string) (int, string, error) {
		atomic.AddInt32(&votes, 1)
		return 0, "", nil
	}

	r.run(context.Background())

	if got := atomic.LoadInt32(&votes); got != 0 {
		t.Errorf("stage1Fn called %d times; want 0 — a cycle with no model must not vote", got)
	}
	post, err := stores.Entities.ListUnclassified(context.Background(), runmode.LocalDefaultOrgID)
	if err != nil {
		t.Fatalf("ListUnclassified: %v", err)
	}
	var stillUnclassified bool
	for _, e := range post {
		if e.ID == entity.ID {
			stillUnclassified = true
		}
	}
	if !stillUnclassified {
		t.Error("entity was stamped classified by a cycle that classified nothing; it must resurface once a model is picked")
	}
	var warned bool
	for _, rec := range logs.recorded() {
		if rec.Level == slog.LevelWarn && strings.Contains(rec.Message, "skipping classification cycle") {
			warned = true
		}
	}
	if !warned {
		t.Errorf("no WARN naming the skip; records = %+v", logs.recorded())
	}
}
