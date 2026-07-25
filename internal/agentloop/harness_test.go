package agentloop

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/maximhq/bifrost/core/schemas"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/inference"
)

// The loop harness: an in-memory transcript, a scripted provider, and a
// scripted tool host. Together they drive the engine end to end with no
// database, no jail, and no network, which is what lets the invariants
// (repair, drain stamping, the terminate contract, the would-stop recheck)
// be asserted as observable row sequences rather than inferred from logs.

// memTranscript is an in-memory Transcript with the same semantics the store
// has: ids ascend from 1, assembly order is COALESCE(seq, id), and
// MarkDelivered flips only the named ids and stamps subtype when non-empty.
type memTranscript struct {
	mu   sync.Mutex
	rows []domain.Message
	next int
	// failInsert, when set, makes the next Insert fail — used to exercise
	// the loop's error paths.
	failInsert error
}

func newMemTranscript(seed ...domain.Message) *memTranscript {
	t := &memTranscript{next: 1}
	for _, r := range seed {
		_, _ = t.Insert(context.Background(), "org", &r)
	}
	return t
}

func (t *memTranscript) ListForAssembly(_ context.Context, _, _ string) ([]domain.Message, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]domain.Message, 0, len(t.rows))
	for _, r := range t.rows {
		if r.WindowState == domain.MessageWindowInactive {
			continue
		}
		out = append(out, r)
	}
	return out, nil
}

func (t *memTranscript) MarkDelivered(_ context.Context, _, _ string, ids []int, subtype string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	want := make(map[int]struct{}, len(ids))
	for _, id := range ids {
		want[id] = struct{}{}
	}
	for i := range t.rows {
		if _, ok := want[t.rows[i].ID]; !ok {
			continue
		}
		delivered := true
		t.rows[i].Delivered = &delivered
		if subtype != "" {
			t.rows[i].Subtype = subtype
		}
	}
	return nil
}

func (t *memTranscript) Insert(_ context.Context, _ string, msg *domain.Message) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.failInsert != nil {
		err := t.failInsert
		t.failInsert = nil
		return 0, err
	}
	row := *msg
	row.ID = t.next
	t.next++
	if row.CreatedAt.IsZero() {
		row.CreatedAt = time.Now().UTC()
	}
	t.rows = append(t.rows, row)
	msg.ID = row.ID
	return row.ID, nil
}

// snapshot returns a copy of every row, in insertion order.
func (t *memTranscript) snapshot() []domain.Message {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]domain.Message(nil), t.rows...)
}

// find returns the first row matching pred, or nil.
func (t *memTranscript) find(pred func(domain.Message) bool) *domain.Message {
	for _, r := range t.snapshot() {
		if pred(r) {
			rr := r
			return &rr
		}
	}
	return nil
}

// toolResults returns every tool row in order.
func (t *memTranscript) toolResults() []domain.Message {
	var out []domain.Message
	for _, r := range t.snapshot() {
		if r.Role == "tool" {
			out = append(out, r)
		}
	}
	return out
}

// scriptedProvider replays a fixed sequence of turns. Each turn is either a
// completion or an error; running past the end is a test bug and surfaces
// as an error rather than a hang.
type scriptedProvider struct {
	mu    sync.Mutex
	turns []scriptedTurn
	// repeat, when set, is returned for every call past the end of turns —
	// for tests about what stops an otherwise unbounded loop, where the
	// script cannot know how many turns it will take.
	repeat *scriptedTurn
	// requests records every request the engine made, so a test can assert
	// on what the model actually saw.
	requests []inference.Request
	calls    int
}

type scriptedTurn struct {
	text   string
	calls  []domain.ToolCall
	finish string
	usage  inference.Usage
	model  string
	err    error
	// onCall runs before the turn is returned, so a test can mutate the
	// transcript mid-flight (input landing while a turn streams).
	onCall func()
}

func (p *scriptedProvider) Stream(_ context.Context, req inference.Request) (*inference.Completion, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.requests = append(p.requests, req)
	if p.calls >= len(p.turns) && p.repeat == nil {
		return nil, fmt.Errorf("scripted provider ran out of turns after %d calls", p.calls)
	}
	var turn scriptedTurn
	if p.calls < len(p.turns) {
		turn = p.turns[p.calls]
	} else {
		turn = *p.repeat
	}
	p.calls++
	if turn.onCall != nil {
		turn.onCall()
	}
	if turn.err != nil {
		return nil, turn.err
	}

	msg := schemas.ChatMessage{Role: schemas.ChatMessageRoleAssistant}
	if turn.text != "" {
		text := turn.text
		msg.Content = &schemas.ChatMessageContent{ContentStr: &text}
	}
	if len(turn.calls) > 0 {
		toolCalls := make([]schemas.ChatAssistantMessageToolCall, len(turn.calls))
		for i, c := range turn.calls {
			id, name := c.ID, c.Name
			args := "{}"
			if len(c.Input) > 0 {
				args = mustJSON(c.Input)
			}
			fn := "function"
			toolCalls[i] = schemas.ChatAssistantMessageToolCall{
				Index:    uint16(i),
				Type:     &fn,
				ID:       &id,
				Function: schemas.ChatAssistantMessageToolCallFunction{Name: &name, Arguments: args},
			}
		}
		msg.ChatAssistantMessage = &schemas.ChatAssistantMessage{ToolCalls: toolCalls}
	}
	finish := turn.finish
	if finish == "" {
		if len(turn.calls) > 0 {
			finish = "tool_calls"
		} else {
			finish = "stop"
		}
	}
	return &inference.Completion{
		Message:      msg,
		Usage:        turn.usage,
		FinishReason: finish,
		Model:        turn.model,
	}, nil
}

// staticCredentials hands the same provider to every call.
type staticCredentials struct {
	provider Provider
	// resolves counts ForCall invocations, so a test can assert the
	// per-call (not per-engagement) resolution contract.
	resolves int
	err      error
}

func (c *staticCredentials) ForCall(context.Context) (schemas.ModelProvider, Provider, func(), error) {
	c.resolves++
	if c.err != nil {
		return "", nil, nil, c.err
	}
	return inference.ProviderAnthropic, c.provider, nil, nil
}

// scriptedToolHost answers by tool name, recording the calls it saw.
type scriptedToolHost struct {
	mu       sync.Mutex
	answers  map[string]ToolOutcome
	errs     map[string]error
	observed []string
	closed   bool
}

func newScriptedToolHost() *scriptedToolHost {
	return &scriptedToolHost{answers: map[string]ToolOutcome{}, errs: map[string]error{}}
}

func (h *scriptedToolHost) Call(name string, _ map[string]any) (ToolOutcome, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.observed = append(h.observed, name)
	if err, ok := h.errs[name]; ok {
		return ToolOutcome{}, err
	}
	if out, ok := h.answers[name]; ok {
		return out, nil
	}
	return ToolOutcome{Content: "ok:" + name}, nil
}

func (h *scriptedToolHost) Close() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.closed = true
	return nil
}

func (h *scriptedToolHost) calls() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.observed...)
}

// newTestEngine wires an engine whose retry never sleeps, so a retry test
// runs at memory speed.
func newTestEngine(t *memTranscript, p Provider, tools ToolHost) *Engine {
	return &Engine{
		Transcript:  t,
		Credentials: &staticCredentials{provider: p},
		Tools:       tools,
		Retry:       RetryPolicy{Sleep: func(context.Context, time.Duration) error { return nil }},
	}
}

func testSpec() Spec {
	return Spec{
		OrgID:          "org",
		ConversationID: "conv",
		Model:          "claude-sonnet-4-5",
		SystemPrompt:   "system",
	}
}

func pendingUser(content string) domain.Message {
	pending := false
	return domain.Message{Role: "user", Subtype: "text", Content: content, Delivered: &pending}
}

func mustJSON(v map[string]any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}
