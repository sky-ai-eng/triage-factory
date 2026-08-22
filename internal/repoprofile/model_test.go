package repoprofile

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/systemllm"
)

// anthropicStub stands in for the Messages API: it records the request body and
// answers with a streamed completion, since the inference client always streams.
// It is what makes "the profiler's batch call carries the org's model"
// assertable without a live call.
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

// TestProfileBatch_SendsTheOrgsModel is the multi-mode half of the knob: the
// model resolved for the cycle is the model the provider request actually
// carries. Nothing between the setting and the wire re-derives it.
func TestProfileBatch_SendsTheOrgsModel(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	stub, baseURL := newAnthropicStub(t, `[]`)

	const model = domain.ModelOpus
	batch := []repoWithDocs{{profile: domain.Repository{Owner: "o", Repo: "r"}, docs: "readme"}}
	if _, err := profileBatch(
		context.Background(), "org-1", model, batch,
		gatewaySecrets{baseURL: baseURL},
		nil, systemllm.NewRecorder(nil), nil,
	); err != nil {
		t.Fatalf("profileBatch: %v", err)
	}

	body := <-stub.body
	if got := body["model"]; got != model {
		t.Errorf("request model = %v, want the org's background-jobs model %q", got, model)
	}
}

// noModelOrgStore is an org whose background-jobs model has never been picked —
// the state a fresh multi-mode org is in before setup completes.
type noModelOrgStore struct{ db.OrgsStore }

func (noModelOrgStore) GetSettingsSystem(context.Context, string) (domain.OrgSettings, error) {
	return domain.OrgSettings{}, nil
}

// TestRunOrg_NoModelSkipsTheCycle pins the other half: an org with no usable
// background-jobs model gets a skipped cycle, not a substituted model.
//
// It skips before the doc fetches, not just before the batch call: those are
// GitHub API calls spent gathering input for a model that will not be asked, and
// the rows they would stamp profiled_at on would then sit behind the TTL
// unprofiled. A WARN names the setting so the remedy is in the log.
func TestRunOrg_NoModelSkipsTheCycle(t *testing.T) {
	logs := &captureLogs{}
	prev := repoprofileLog
	repoprofileLog = slog.New(logs)
	t.Cleanup(func() { repoprofileLog = prev })

	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("no GitHub call should be made by a cycle with no model")
	}))
	defer srv.Close()

	p := NewProfiler(fixedResolver{client: github.NewClient(srv.URL, "tok")}, nil, nil, nil, noModelOrgStore{}, nil, nil, nil)
	var batches int32
	p.batchFn = func(context.Context, string, string, []repoWithDocs, agentproc.SecretsReader) ([]repoProfileResult, error) {
		atomic.AddInt32(&batches, 1)
		return nil, nil
	}

	touched, err := p.runOrg(context.Background(), "org-1", []string{"owner/repo"}, true)
	if err != nil {
		t.Fatalf("runOrg: %v", err)
	}
	if touched {
		t.Error("touched = true; a cycle with no model persisted nothing")
	}
	if got := atomic.LoadInt32(&batches); got != 0 {
		t.Errorf("batchFn called %d times; want 0", got)
	}
	var warned bool
	for _, rec := range logs.recorded() {
		if rec.Level == slog.LevelWarn && strings.Contains(rec.Message, "skipping repo-profiling cycle") {
			warned = true
		}
	}
	if !warned {
		t.Errorf("no WARN naming the skip; records = %+v", logs.recorded())
	}
}

// captureLogs is a minimal slog.Handler that records emitted entries so a test
// can assert exactly what a code path logged.
type captureLogs struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *captureLogs) Enabled(context.Context, slog.Level) bool { return true }

func (h *captureLogs) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, r)
	return nil
}

func (h *captureLogs) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *captureLogs) WithGroup(string) slog.Handler      { return h }

func (h *captureLogs) recorded() []slog.Record {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]slog.Record(nil), h.records...)
}
