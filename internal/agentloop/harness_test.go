package agentloop

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
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
	// failInsertAt, when non-zero, fails that Insert and lets the ones before
	// it land — counting from the first write after the seed. The crash
	// mid-way through a sequence of writes no transaction spans: a repair that
	// answered one call and died before the next, most of all.
	failInsertAt int
	inserts      int
	// failMarkDelivered, when set, fails the next MarkDelivered. A function
	// rather than an error so a test can land something else inside the
	// write it stands in for — a context kill, most of all, which is how a
	// stop is really observed by a flush that was already in flight.
	failMarkDelivered func() error
}

func newMemTranscript(seed ...domain.Message) *memTranscript {
	t := &memTranscript{next: 1}
	for _, r := range seed {
		_, _ = t.Insert(context.Background(), "org", &r)
	}
	t.inserts = 0 // the seed is the fixture, not a write under test
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
	// The store orders by COALESCE(seq, id); a re-seqed row must sort where
	// its fraction puts it, not where it was inserted.
	sort.SliceStable(out, func(i, j int) bool { return assemblyKey(out[i]) < assemblyKey(out[j]) })
	return out, nil
}

func (t *memTranscript) MarkDelivered(_ context.Context, _, _ string, ids []int, subtype string) error {
	if t.failMarkDelivered != nil {
		fail := t.failMarkDelivered
		t.failMarkDelivered = nil
		if err := fail(); err != nil {
			return err
		}
	}
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
	t.inserts++
	if t.failInsertAt == t.inserts {
		t.failInsertAt = 0
		return 0, fmt.Errorf("insert %d failed", t.inserts)
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

// Compact mirrors the store op: insert the optional reply row (forced
// inactive), insert the result row, flip the span, re-seq undelivered rows
// to fractions after the result row in their existing relative order.
func (t *memTranscript) Compact(_ context.Context, _, conversationID string, replyRow, resultRow *domain.Message, inactiveIDs []int) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	insert := func(msg *domain.Message) {
		row := *msg
		row.ID = t.next
		t.next++
		if row.CreatedAt.IsZero() {
			row.CreatedAt = time.Now().UTC()
		}
		row.ConversationID = conversationID
		t.rows = append(t.rows, row)
		msg.ID = row.ID
	}
	if replyRow != nil {
		replyRow.WindowState = domain.MessageWindowInactive
		insert(replyRow)
	}
	insert(resultRow)

	want := make(map[int]struct{}, len(inactiveIDs))
	for _, id := range inactiveIDs {
		want[id] = struct{}{}
	}
	for i := range t.rows {
		if _, ok := want[t.rows[i].ID]; ok {
			t.rows[i].WindowState = domain.MessageWindowInactive
		}
	}

	var queued []int
	for i := range t.rows {
		r := t.rows[i]
		delivered := r.Delivered == nil || *r.Delivered
		key := float64(r.ID)
		if r.Seq != nil {
			key = *r.Seq
		}
		if !delivered && key < float64(resultRow.ID) {
			queued = append(queued, i)
		}
	}
	for n, i := range queued {
		seq := float64(resultRow.ID) + float64(n+1)/float64(len(queued)+1)
		t.rows[i].Seq = &seq
	}
	return nil
}

// SettleCompactionRequest records the settlement so tests can assert the
// failed attempt's accounting landed on the request row.
func (t *memTranscript) SettleCompactionRequest(_ context.Context, _, _ string, requestID, inputTokens, outputTokens, cacheReadTokens, cacheCreationTokens int, costUSD *float64, reason string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	for i := range t.rows {
		if t.rows[i].ID != requestID {
			continue
		}
		in, out, cr, cc := inputTokens, outputTokens, cacheReadTokens, cacheCreationTokens
		t.rows[i].InputTokens = &in
		t.rows[i].OutputTokens = &out
		t.rows[i].CacheReadTokens = &cr
		t.rows[i].CacheCreationTokens = &cc
		t.rows[i].CostUSD = costUSD
		if t.rows[i].Metadata == nil {
			t.rows[i].Metadata = map[string]any{}
		}
		t.rows[i].Metadata["compaction_failure"] = reason
		return nil
	}
	return fmt.Errorf("settle: no row %d", requestID)
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
	text string
	// reasoning is the turn's chain of thought. Set alone (no text, no
	// calls) it is the shape a thinking model returns when the output cap
	// ran out before it wrote anything: reasoning is all there is, and the
	// provider strips it on replay.
	reasoning string
	calls     []domain.ToolCall
	finish    string
	// noFinishReason makes the turn report no stop reason at all — the stream
	// anomaly. It needs a field of its own because an unset `finish` already
	// means "the script didn't say", which the harness fills in below.
	noFinishReason bool
	usage          inference.Usage
	model          string
	err            error
	// rawArgs overrides the rendered argument JSON for the call at that
	// index — for truncation tests, where the wire carries a fragment no
	// Input map can express.
	rawArgs map[int]string
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
			if raw, ok := turn.rawArgs[i]; ok {
				args = raw
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
	if turn.reasoning != "" {
		if msg.ChatAssistantMessage == nil {
			msg.ChatAssistantMessage = &schemas.ChatAssistantMessage{}
		}
		text, sig := turn.reasoning, "c2ln"
		msg.ReasoningDetails = []schemas.ChatReasoningDetails{{
			Index: 0, Type: "reasoning.text", Text: &text, Signature: &sig,
		}}
	}
	finish := turn.finish
	if finish == "" && !turn.noFinishReason {
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

// scriptedToolHost answers by tool name, recording the calls it saw — names
// in order, and the args each was sent with.
type scriptedToolHost struct {
	mu       sync.Mutex
	answers  map[string]ToolOutcome
	errs     map[string]error
	observed []string
	args     []map[string]any
	closed   bool
}

func newScriptedToolHost() *scriptedToolHost {
	return &scriptedToolHost{answers: map[string]ToolOutcome{}, errs: map[string]error{}}
}

func (h *scriptedToolHost) Call(name string, args map[string]any) (ToolOutcome, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.observed = append(h.observed, name)
	h.args = append(h.args, args)
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

// argsFor returns the args of the first call to `name`, and whether it
// happened at all.
func (h *scriptedToolHost) argsFor(name string) (map[string]any, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for i, seen := range h.observed {
		if seen == name {
			return h.args[i], true
		}
	}
	return nil, false
}

// racingToolHost lands a row in the transcript while the first tool call is
// executing — the window where a person's follow-up takes the id between a
// call and its answer. Once, so a multi-turn script races exactly one dispatch.
type racingToolHost struct {
	*scriptedToolHost
	transcript *memTranscript
	arrive     domain.Message
	landed     bool
}

func (h *racingToolHost) Call(name string, input map[string]any) (ToolOutcome, error) {
	if !h.landed {
		h.landed = true
		row := h.arrive
		row.ConversationID = "conv"
		_, _ = h.transcript.Insert(context.Background(), "org", &row)
	}
	return h.scriptedToolHost.Call(name, input)
}

// recordingLogger captures what the engine logged. Needed where the log IS
// the deliverable: a shape the loop deliberately tolerates leaves no row
// behind, so the warn is the only evidence it was noticed at all.
type recordingLogger struct {
	mu    sync.Mutex
	warns []string
	infos []string
}

func (l *recordingLogger) Warn(msg string, _ ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.warns = append(l.warns, msg)
}

func (l *recordingLogger) Info(msg string, _ ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.infos = append(l.infos, msg)
}

func (l *recordingLogger) warned(substr string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, w := range l.warns {
		if strings.Contains(w, substr) {
			return true
		}
	}
	return false
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

// testParams is a delegation-shaped engagement: a conversation executing a
// blueprint, which is the only shape the loop drives today.
func testParams() Params {
	return Params{
		OrgID:          "org",
		ConversationID: "conv",
		Model:          "claude-sonnet-4-5",
		SystemPrompt:   "system",
		HasBlueprint:   true,
		// A delegation opens with a minted mission; compaction pins it.
		MissionAnchored: true,
	}
}

// workspaceParams is testParams for an engagement whose run tree the claim
// path has classified — the input the claim-time notice turns on.
func workspaceParams(prov domain.WorkspaceProvenance, executorChanged bool) Params {
	p := testParams()
	p.Workspace = prov
	p.ExecutorChanged = executorChanged
	return p
}

func pendingUser(content string) domain.Message {
	pending := false
	return domain.Message{Role: "user", Content: content, Delivered: &pending}
}

func mustJSON(v map[string]any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// assertToolResultsAreAdjacent applies the adjacency rule to a request as the
// model will see it. Every tool_use id an assistant message carries must be
// answered within the run of tool messages that immediately follows it — the
// exact condition behind Anthropic's "tool_use ids were found without
// tool_result blocks immediately after".
func assertToolResultsAreAdjacent(t *testing.T, req inference.Request) {
	t.Helper()
	msgs, err := inference.RowsToMessages(req.Rows, inference.AssemblyOptions{})
	if err != nil {
		t.Fatalf("assemble request: %v", err)
	}
	for i, m := range msgs {
		if m.Role != schemas.ChatMessageRoleAssistant || m.ChatAssistantMessage == nil {
			continue
		}
		unanswered := map[string]struct{}{}
		for _, call := range m.ToolCalls {
			if call.ID != nil && *call.ID != "" {
				unanswered[*call.ID] = struct{}{}
			}
		}
		if len(unanswered) == 0 {
			continue
		}
		for j := i + 1; j < len(msgs) && msgs[j].Role == schemas.ChatMessageRoleTool; j++ {
			if msgs[j].ChatToolMessage == nil || msgs[j].ToolCallID == nil {
				continue
			}
			delete(unanswered, *msgs[j].ToolCallID)
		}
		if len(unanswered) > 0 {
			var ids []string
			for id := range unanswered {
				ids = append(ids, id)
			}
			t.Fatalf("assistant message %d's calls %v are not answered in the messages immediately after it; assembled: %s",
				i, ids, describeAssembly(msgs))
		}
	}
}

// describeAssembly renders an assembled request as a role sequence, so a
// failure names the shape that broke rather than dumping every message.
func describeAssembly(msgs []schemas.ChatMessage) string {
	parts := make([]string, 0, len(msgs))
	for _, m := range msgs {
		switch {
		case m.ChatAssistantMessage != nil && len(m.ToolCalls) > 0:
			var ids []string
			for _, c := range m.ToolCalls {
				if c.ID != nil {
					ids = append(ids, *c.ID)
				}
			}
			parts = append(parts, fmt.Sprintf("assistant(tool_use:%s)", strings.Join(ids, ",")))
		case m.ChatToolMessage != nil && m.ToolCallID != nil:
			parts = append(parts, fmt.Sprintf("tool(%s)", *m.ToolCallID))
		default:
			parts = append(parts, string(m.Role))
		}
	}
	return strings.Join(parts, " → ")
}
