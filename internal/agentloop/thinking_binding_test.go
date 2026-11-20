package agentloop

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/maximhq/bifrost/core/schemas"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/llm/inference"
)

// The Anthropic API checks every thinking block it is sent back against the
// context the block was produced under: the top-level system prompt, the set
// of tools, and every message before the block. A request that replays a
// block whose context changed is refused with a 400, and retrying the same
// body fails the same way. Claude Fable 5.2 enforces this on every account;
// Fable 5.1, Opus 5.5, Sonnet 5.5 and Haiku 5.5 enforce it on accounts created
// on or after 2026-08-31. A block sent to a model other than the one that
// wrote it is dropped instead, so the rule binds only same-model replays.
//
// bindingProvider applies that rule to every request the loop makes, at the
// level TF hands to bifrost: the two system blocks, the tools, and the
// messages the client assembles from the rows. It does not cover bifrost's own
// conversion to the Anthropic wire shape.
type bindingProvider struct {
	*scriptedProvider

	mu       sync.Mutex
	call     int
	produced map[string]bindingContext
	broken   []string
}

// bindingContext is what one request carried, which is the context every
// block its response holds is bound to.
type bindingContext struct {
	call     int
	model    string
	system   string
	addendum string
	tools    string
	messages []string
}

func newBindingProvider(turns ...scriptedTurn) *bindingProvider {
	return &bindingProvider{
		scriptedProvider: &scriptedProvider{turns: turns},
		produced:         map[string]bindingContext{},
	}
}

func (p *bindingProvider) Stream(ctx context.Context, req inference.Request) (*inference.Completion, error) {
	sent, parsed, err := contextOf(req)
	if err != nil {
		return nil, err
	}

	p.mu.Lock()
	p.call++
	sent.call = p.call
	for i, m := range parsed {
		if m.ChatAssistantMessage == nil {
			continue
		}
		for _, d := range m.ReasoningDetails {
			if d.Signature == nil {
				continue
			}
			at, ok := p.produced[*d.Signature]
			if !ok || at.model != req.Model {
				continue
			}
			if why := at.changedFor(sent, i); why != "" {
				p.broken = append(p.broken, fmt.Sprintf(
					"call %d replays the block call %d produced, but %s", sent.call, at.call, why))
			}
		}
	}
	p.mu.Unlock()

	completion, err := p.scriptedProvider.Stream(ctx, req)
	if err != nil || completion == nil {
		return completion, err
	}
	if a := completion.Message.ChatAssistantMessage; a != nil {
		p.mu.Lock()
		for i := range a.ReasoningDetails {
			sig := fmt.Sprintf("sig-%d-%d", sent.call, i)
			a.ReasoningDetails[i].Signature = &sig
			p.produced[sig] = sent
		}
		p.mu.Unlock()
	}
	return completion, nil
}

// changedFor names what differs between the context a block was produced
// under and the request now replaying it at message index at, or returns ""
// when the request keeps that context intact.
func (c bindingContext) changedFor(now bindingContext, at int) string {
	switch {
	case now.system != c.system:
		return "the shared system prompt changed"
	case now.addendum != c.addendum:
		return "the conversation's system block changed"
	case now.tools != c.tools:
		return "the tools changed"
	}
	for k := 0; k < len(c.messages) && k < at; k++ {
		if now.messages[k] != c.messages[k] {
			return fmt.Sprintf("message %d changed:\n  was %s\n  now %s", k, clip(c.messages[k]), clip(now.messages[k]))
		}
	}
	switch {
	case at < len(c.messages):
		return fmt.Sprintf("it sits at message %d, and %d messages preceded it when it was produced", at, len(c.messages))
	case at > len(c.messages):
		return fmt.Sprintf("message %d was inserted ahead of it: %s", len(c.messages), clip(now.messages[len(c.messages)]))
	}
	return ""
}

// contextOf renders a request the way the client assembles it, with the
// moving cache breakpoint left off: cache_control markers may move freely
// without invalidating a block.
func contextOf(req inference.Request) (bindingContext, []schemas.ChatMessage, error) {
	msgs, err := inference.RowsToMessages(req.Rows, inference.AssemblyOptions{
		IncludeUndelivered: req.IncludeUndelivered,
		NoCacheBreakpoint:  true,
	})
	if err != nil {
		return bindingContext{}, nil, err
	}
	encoded := make([]string, len(msgs))
	for i, m := range msgs {
		b, err := json.Marshal(m)
		if err != nil {
			return bindingContext{}, nil, err
		}
		encoded[i] = string(b)
	}
	// The API binds the tools as a name-sorted set, so their order is free.
	// Rendered here and not by canonicalTools, so a fault in what the
	// fingerprint covers shows up as a block this provider reports.
	tools := append([]schemas.ChatTool(nil), req.Tools...)
	sort.SliceStable(tools, func(i, j int) bool { return toolName(tools[i]) < toolName(tools[j]) })
	toolsJSON, err := json.Marshal(tools)
	if err != nil {
		return bindingContext{}, nil, err
	}
	return bindingContext{
		model:    req.Model,
		system:   req.SystemPrompt,
		addendum: req.SystemAddendum,
		tools:    string(toolsJSON),
		messages: encoded,
	}, msgs, nil
}

func toolName(t schemas.ChatTool) string {
	if t.Function != nil {
		return t.Function.Name
	}
	return ""
}

func clip(s string) string {
	if len(s) > 160 {
		return s[:160] + "…"
	}
	return s
}

func (p *bindingProvider) assertBound(t *testing.T) {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, b := range p.broken {
		t.Error(b)
	}
}

// thinking gives a scripted turn the chain of thought a thinking model
// always returns, so every response carries a block to bind.
func thinking(turn scriptedTurn) scriptedTurn {
	turn.reasoning = "reasoning"
	return turn
}

func fableParams() Params {
	p := testParams()
	p.Model = "claude-fable-5-2"
	p.SystemAddendum = "conversation block"
	return p
}

func overThresholdFor(t *testing.T, model string) int {
	t.Helper()
	window, ok := inference.ModelWindow(model)
	if !ok {
		t.Fatalf("%s carries no datasheet window", model)
	}
	return int(DefaultCompactionThreshold*float64(window)) + 5000
}

// ageRows moves every row's creation time back past the provider cache TTL,
// which is what a conversation parked overnight looks like to the next claim.
func (t *memTranscript) ageRows(by time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for i := range t.rows {
		t.rows[i].CreatedAt = t.rows[i].CreatedAt.Add(-by)
	}
}

func runEngagement(t *testing.T, tr *memTranscript, p Provider, host ToolHost, params Params) {
	t.Helper()
	if got := newTestEngine(tr, p, host).Run(context.Background(), params); got.Kind != ResultConcluded {
		t.Fatalf("disposition = %v, want concluded (err: %v)", got.Kind, got.Err)
	}
}

func TestThinkingBinding_ATurnLoopReplaysItsBlocksUnchanged(t *testing.T) {
	tr := newMemTranscript(pendingUser("go"))
	p := newBindingProvider(
		thinking(scriptedTurn{calls: []domain.ToolCall{{ID: "c1", Name: "bash"}}}),
		thinking(scriptedTurn{calls: []domain.ToolCall{{ID: "c2", Name: "bash"}, {ID: "c3", Name: "bash"}}}),
		thinking(scriptedTurn{text: "done"}),
	)
	runEngagement(t, tr, p, newScriptedToolHost(), fableParams())
	p.assertBound(t)
}

func TestThinkingBinding_AFollowUpThatLandsWhileAToolRuns(t *testing.T) {
	tr := newMemTranscript(pendingUser("go"))
	host := &racingToolHost{scriptedToolHost: newScriptedToolHost(), transcript: tr,
		arrive: pendingUser("also update the changelog")}
	p := newBindingProvider(
		thinking(scriptedTurn{calls: []domain.ToolCall{{ID: "c1", Name: "bash"}}}),
		thinking(scriptedTurn{text: "done"}),
	)
	runEngagement(t, tr, p, host, fableParams())
	p.assertBound(t)
}

func TestThinkingBinding_AFollowUpThatLandsWhileATurnStreams(t *testing.T) {
	tr := newMemTranscript(pendingUser("go"))
	p := newBindingProvider(
		thinking(scriptedTurn{
			calls: []domain.ToolCall{{ID: "c1", Name: "bash"}},
			onCall: func() {
				_, _ = tr.Insert(context.Background(), "org", &domain.Message{
					ConversationID: "conv", Role: "user", Content: "also check the tests", Delivered: boolPtr(false),
				})
			},
		}),
		thinking(scriptedTurn{text: "done"}),
	)
	runEngagement(t, tr, p, newScriptedToolHost(), fableParams())
	p.assertBound(t)
}

// The final turn's input reaches the model through the would-stop recheck and
// a bare drain, not the drain between tool calls.
func TestThinkingBinding_AFollowUpThatLandsWhileTheFinalTurnStreams(t *testing.T) {
	tr := newMemTranscript(pendingUser("go"))
	p := newBindingProvider(
		thinking(scriptedTurn{
			text: "done",
			onCall: func() {
				_, _ = tr.Insert(context.Background(), "org", &domain.Message{
					ConversationID: "conv", Role: "user", Content: "one more thing", Delivered: boolPtr(false),
				})
			},
		}),
		thinking(scriptedTurn{text: "done again"}),
	)
	runEngagement(t, tr, p, newScriptedToolHost(), fableParams())
	if len(p.requests) != 2 {
		t.Fatalf("staging failed: %d calls, want the follow-up to keep the run going for a second", len(p.requests))
	}
	p.assertBound(t)
}

func TestThinkingBinding_ASystemNoteThatLandsWhileATurnStreams(t *testing.T) {
	tr := newMemTranscript(pendingUser("go"))
	p := newBindingProvider(
		thinking(scriptedTurn{
			calls: []domain.ToolCall{{ID: "c1", Name: "bash"}},
			onCall: func() {
				_, _ = tr.Insert(context.Background(), "org", &domain.Message{
					ConversationID: "conv", Role: "user", Subtype: domain.MessageSubtypeInjectionSystemNote,
					Content: "the task was closed", Delivered: boolPtr(false),
				})
			},
		}),
		thinking(scriptedTurn{text: "done"}),
	)
	runEngagement(t, tr, p, newScriptedToolHost(), fableParams())
	p.assertBound(t)
}

func TestThinkingBinding_AResumeWithTheSameSystemText(t *testing.T) {
	tr := newMemTranscript(pendingUser("go"))
	p := newBindingProvider(
		thinking(scriptedTurn{calls: []domain.ToolCall{{ID: "c1", Name: "bash"}}}),
		thinking(scriptedTurn{text: "done"}),
		thinking(scriptedTurn{calls: []domain.ToolCall{{ID: "c2", Name: "bash"}}}),
		thinking(scriptedTurn{text: "done again"}),
	)
	runEngagement(t, tr, p, newScriptedToolHost(), fableParams())
	tr.ageRows(time.Hour)
	_, _ = tr.Insert(context.Background(), "org", &domain.Message{
		ConversationID: "conv", Role: "user", Content: "one more thing", Delivered: boolPtr(false),
	})
	runEngagement(t, tr, p, newScriptedToolHost(), fableParams())
	if got := replayedBlocks(t, p.requests[2]); got != 2 {
		t.Errorf("the resume's first call replays %d blocks, want both the earlier engagement produced", got)
	}
	p.assertBound(t)
}

// TestThinkingBinding_AResumeAfterTheSystemTextChanged is the checker's own
// control: a claim that composes either system block anew is the edit the API
// refuses, so the provider must report it. It is also the shape a release that
// changes a framework block gives every parked conversation.
func TestThinkingBinding_AResumeAfterTheSystemTextChanged(t *testing.T) {
	for name, change := range map[string]func(*Params){
		"the shared system prompt":        func(p *Params) { p.SystemPrompt += " (next release)" },
		"the conversation's system block": func(p *Params) { p.SystemAddendum += " (recomposed)" },
	} {
		t.Run(name, func(t *testing.T) {
			tr := newMemTranscript(pendingUser("go"))
			p := newBindingProvider(
				thinking(scriptedTurn{calls: []domain.ToolCall{{ID: "c1", Name: "bash"}}}),
				thinking(scriptedTurn{text: "done"}),
				thinking(scriptedTurn{text: "done again"}),
			)
			runEngagement(t, tr, p, newScriptedToolHost(), fableParams())
			_, _ = tr.Insert(context.Background(), "org", &domain.Message{
				ConversationID: "conv", Role: "user", Content: "one more thing", Delivered: boolPtr(false),
			})
			params := fableParams()
			change(&params)
			runEngagement(t, tr, p, newScriptedToolHost(), params)
			if len(p.broken) == 0 {
				t.Fatal("a resume that changed a system block replayed its earlier blocks unreported")
			}
		})
	}
}

// A resume under other tools is what a release that changes a tool definition
// gives every parked conversation, and what a claim that offers a different
// set gives one. The earlier blocks are bound to the earlier tools, so the
// resume must not replay them, and must replay the ones it produces itself.
func TestThinkingBinding_AResumeAfterTheToolsChanged(t *testing.T) {
	tr := newMemTranscript(pendingUser("go"))
	p := newBindingProvider(
		thinking(scriptedTurn{calls: []domain.ToolCall{{ID: "c1", Name: "bash"}}}),
		thinking(scriptedTurn{text: "done"}),
		thinking(scriptedTurn{calls: []domain.ToolCall{{ID: "c2", Name: "bash"}}}),
		thinking(scriptedTurn{text: "done again"}),
	)
	runEngagement(t, tr, p, newScriptedToolHost(), fableParams())
	tr.ageRows(time.Hour)
	_, _ = tr.Insert(context.Background(), "org", &domain.Message{
		ConversationID: "conv", Role: "user", Content: "one more thing", Delivered: boolPtr(false),
	})
	runEngagement(t, tr, p, newScriptedToolHost(), otherToolsParams())

	if toolsFingerprint(p.requests[1].Tools) == toolsFingerprint(p.requests[2].Tools) {
		t.Fatal("staging failed: the resume carries the tools the first engagement did")
	}
	for call, want := range []int{0, 1, 0, 1} {
		if got := replayedBlocks(t, p.requests[call]); got != want {
			t.Errorf("call %d replays %d blocks, want %d", call+1, got, want)
		}
	}
	p.assertBound(t)
}

// A conversation that goes back to tools it held before, as one does when a
// deploy is rolled back or its claims alternate between two releases, does
// not get the blocks from its first time under them back: the rows between
// were produced without those blocks in front of them.
func TestThinkingBinding_AResumeUnderToolsTheConversationHeldBefore(t *testing.T) {
	tr := newMemTranscript(pendingUser("go"))
	p := newBindingProvider(
		thinking(scriptedTurn{text: "first"}),
		thinking(scriptedTurn{text: "second"}),
		thinking(scriptedTurn{calls: []domain.ToolCall{{ID: "c1", Name: "bash"}}}),
		thinking(scriptedTurn{text: "third"}),
	)
	for _, params := range []Params{fableParams(), otherToolsParams(), fableParams()} {
		_, _ = tr.Insert(context.Background(), "org", &domain.Message{
			ConversationID: "conv", Role: "user", Content: "next", Delivered: boolPtr(false),
		})
		runEngagement(t, tr, p, newScriptedToolHost(), params)
	}
	for call, want := range []int{0, 0, 0, 1} {
		if got := replayedBlocks(t, p.requests[call]); got != want {
			t.Errorf("call %d replays %d blocks, want %d", call+1, got, want)
		}
	}
	p.assertBound(t)
}

// A row that records no tools fingerprint is one a binary that records none
// wrote, and that binary replays its reasoning, so this one does too.
func TestThinkingBinding_AResumeOverRowsThatRecordNoTools(t *testing.T) {
	tr := newMemTranscript(pendingUser("go"))
	p := newBindingProvider(
		thinking(scriptedTurn{text: "done"}),
		thinking(scriptedTurn{calls: []domain.ToolCall{{ID: "c1", Name: "bash"}}}),
		thinking(scriptedTurn{text: "done again"}),
	)
	runEngagement(t, tr, p, newScriptedToolHost(), fableParams())
	tr.forgetToolsFingerprints()
	_, _ = tr.Insert(context.Background(), "org", &domain.Message{
		ConversationID: "conv", Role: "user", Content: "one more thing", Delivered: boolPtr(false),
	})
	runEngagement(t, tr, p, newScriptedToolHost(), fableParams())
	for call, want := range []int{0, 1, 2} {
		if got := replayedBlocks(t, p.requests[call]); got != want {
			t.Errorf("call %d replays %d blocks, want %d", call+1, got, want)
		}
	}
	p.assertBound(t)
}

// A row that records no fingerprint goes out without its reasoning once a
// later row puts it behind the cut.
func TestThinkingBinding_AToolChangeAfterRowsThatRecordNoTools(t *testing.T) {
	tr := newMemTranscript(pendingUser("go"))
	p := newBindingProvider(
		thinking(scriptedTurn{text: "first"}),
		thinking(scriptedTurn{text: "second"}),
		thinking(scriptedTurn{text: "third"}),
	)
	runEngagement(t, tr, p, newScriptedToolHost(), fableParams())
	tr.forgetToolsFingerprints()
	for _, params := range []Params{fableParams(), otherToolsParams()} {
		_, _ = tr.Insert(context.Background(), "org", &domain.Message{
			ConversationID: "conv", Role: "user", Content: "next", Delivered: boolPtr(false),
		})
		runEngagement(t, tr, p, newScriptedToolHost(), params)
	}
	for call, want := range []int{0, 1, 0} {
		if got := replayedBlocks(t, p.requests[call]); got != want {
			t.Errorf("call %d replays %d blocks, want %d", call+1, got, want)
		}
	}
	p.assertBound(t)
}

// forgetToolsFingerprints leaves every row as a binary that records no
// fingerprint would have written it.
func (t *memTranscript) forgetToolsFingerprints() {
	t.mu.Lock()
	defer t.mu.Unlock()
	for i := range t.rows {
		delete(t.rows[i].Metadata, domain.MessageMetadataToolsFingerprint)
	}
}

func TestToolsFingerprint(t *testing.T) {
	tool := func(name, description string, required ...string) schemas.ChatTool {
		return schemas.ChatTool{
			Type: schemas.ChatToolTypeFunction,
			Function: &schemas.ChatToolFunction{
				Name:        name,
				Description: &description,
				Parameters:  &schemas.ToolFunctionParameters{Type: "object", Required: required},
			},
		}
	}
	base := toolsFingerprint([]schemas.ChatTool{tool("bash", "run a command", "command"), tool("read", "read a file", "path")})
	if base == "" {
		t.Fatal("no fingerprint for a list that renders")
	}
	if got := toolsFingerprint([]schemas.ChatTool{tool("read", "read a file", "path"), tool("bash", "run a command", "command")}); got != base {
		t.Error("the same tools in another order have another fingerprint; the API binds them as a set")
	}
	if bifrostVersion() == "" {
		t.Error("the binary reports no bifrost version, so the fingerprint cannot tell one from another")
	}
	tools := []schemas.ChatTool{tool("bash", "run a command", "command")}
	if fingerprintTools(tools, "v1.0.0") == fingerprintTools(tools, "v1.0.1") {
		t.Error("the same tools under another bifrost version have the same fingerprint")
	}
	for name, changed := range map[string][]schemas.ChatTool{
		"a description":   {tool("bash", "run one command", "command"), tool("read", "read a file", "path")},
		"an input schema": {tool("bash", "run a command", "command", "timeout"), tool("read", "read a file", "path")},
		"a name":          {tool("shell", "run a command", "command"), tool("read", "read a file", "path")},
		"a removed tool":  {tool("bash", "run a command", "command")},
		"an added tool":   {tool("bash", "run a command", "command"), tool("read", "read a file", "path"), tool("write", "write a file", "path")},
	} {
		if toolsFingerprint(changed) == base {
			t.Errorf("%s changed and the fingerprint did not", name)
		}
	}
}

// otherToolsParams is fableParams for a claim that offers the conversation a
// different set of tools.
func otherToolsParams() Params {
	p := fableParams()
	p.HasBlueprint = !p.HasBlueprint
	return p
}

// replayedBlocks counts the thinking blocks a request sends back.
func replayedBlocks(t *testing.T, req inference.Request) int {
	t.Helper()
	_, msgs, err := contextOf(req)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, m := range msgs {
		if m.ChatAssistantMessage != nil {
			n += len(m.ReasoningDetails)
		}
	}
	return n
}

func TestThinkingBinding_WarmCompactionMidWork(t *testing.T) {
	tr := newMemTranscript(pendingUser("go"))
	full := overThresholdFor(t, fableParams().Model)
	summary := summaryReply("everything that happened")
	p := newBindingProvider(
		thinking(scriptedTurn{calls: []domain.ToolCall{{ID: "c1", Name: "bash"}}, usage: inference.Usage{PromptTokens: full}}),
		thinking(summary),
		thinking(scriptedTurn{calls: []domain.ToolCall{{ID: "c2", Name: "bash"}}}),
		thinking(scriptedTurn{text: "done"}),
	)
	runEngagement(t, tr, p, newScriptedToolHost(), fableParams())
	if tr.find(func(m domain.Message) bool { return m.Subtype == domain.MessageSubtypeInjectionCompactionResult }) == nil {
		t.Fatal("staging failed: the engagement never compacted")
	}
	p.assertBound(t)
}

// TestThinkingBinding_ColdCompactionOnResume covers the forced-shape call,
// which sends the transcript under its own system prompt and tool. That is an
// edit for any block it replays on the model that wrote it, and a dropped
// block for any other model.
func TestThinkingBinding_ColdCompactionOnResume(t *testing.T) {
	for _, model := range []string{"claude-fable-5-2", domain.ModelHaiku} {
		t.Run(model, func(t *testing.T) {
			params := fableParams()
			params.Model = model
			tr := newMemTranscript(pendingUser("go"))
			p := newBindingProvider(
				thinking(scriptedTurn{calls: []domain.ToolCall{{ID: "c1", Name: "bash"}}}),
				thinking(scriptedTurn{text: "done", usage: inference.Usage{PromptTokens: overThresholdFor(t, model)}}),
				forcedReply("resume analysis", "resume summary"),
				thinking(scriptedTurn{text: "done again"}),
			)
			runEngagement(t, tr, p, newScriptedToolHost(), params)
			tr.ageRows(time.Hour)
			_, _ = tr.Insert(context.Background(), "org", &domain.Message{
				ConversationID: "conv", Role: "user", Content: "one more thing", Delivered: boolPtr(false),
			})
			runEngagement(t, tr, p, newScriptedToolHost(), params)
			if got := p.requests[2].SystemPrompt; got != coldCompactionSystemPrompt {
				t.Fatalf("staging failed: the resume's first call has system prompt %.40q…, want the forced-shape call", got)
			}
			p.assertBound(t)
		})
	}
}
