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

// changedSystem is a claim's composition that differs from fableParams' frozen
// text, as a release or a team setting would make it.
func changedSystem() *domain.SystemText {
	return &domain.SystemText{
		Prompt: "system (next release)",
		Block:  "conversation block (recomposed)",
		Parts:  []domain.SystemPart{{Key: "run_context/branch", Name: "the branch naming convention", Text: "Branch naming convention for this team: fix/{slug}"}},
	}
}

const systemUpdateText = "<instructions_update>the branch naming convention changed</instructions_update>"

// A re-claim whose composition changed keeps sending the text the
// conversation was frozen with, and tells it what changed in a system row
// after the claim's input, so every block it replays keeps its context.
func TestThinkingBinding_AResumeWithAChangedSystemTextAppendsAnUpdate(t *testing.T) {
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
	params.SystemUpdate = systemUpdateText
	params.CurrentSystem = changedSystem()
	runEngagement(t, tr, p, newScriptedToolHost(), params)
	p.assertBound(t)

	resumed := p.requests[2]
	if resumed.SystemPrompt != fableParams().SystemPrompt || resumed.SystemAddendum != fableParams().SystemAddendum {
		t.Fatalf("the resume sent system %q / %q, want the frozen text", resumed.SystemPrompt, resumed.SystemAddendum)
	}
	last := resumed.Rows[len(resumed.Rows)-1]
	if last.Role != "system" || last.Content != systemUpdateText || last.Subtype != domain.MessageSubtypeInjectionSystemUpdate {
		t.Fatalf("the resume's last row = %s %q %q, want the system update after the follow-up", last.Role, last.Subtype, last.Content)
	}
	if !isDelivered(last) {
		t.Errorf("the update reached the request undelivered")
	}
	if parts, ok := last.Metadata[domain.SystemUpdateMetadataKey].([]domain.SystemPart); !ok || len(parts) != 1 {
		t.Errorf("the update carries %#v as its parts, want this claim's composition", last.Metadata[domain.SystemUpdateMetadataKey])
	}
}

// A re-claim whose composition matches what the conversation last heard adds
// no row: the driver passes no update.
func TestThinkingBinding_AResumeWithNoUpdateAddsNoSystemRow(t *testing.T) {
	tr := newMemTranscript(pendingUser("go"))
	p := newBindingProvider(thinking(scriptedTurn{text: "done"}), thinking(scriptedTurn{text: "done again"}))
	runEngagement(t, tr, p, newScriptedToolHost(), fableParams())
	_, _ = tr.Insert(context.Background(), "org", &domain.Message{
		ConversationID: "conv", Role: "user", Content: "one more thing", Delivered: boolPtr(false),
	})
	params := fableParams()
	params.CurrentSystem = changedSystem()
	runEngagement(t, tr, p, newScriptedToolHost(), params)
	if tr.find(func(m domain.Message) bool { return m.Role == "system" }) != nil {
		t.Fatal("a claim with no update wrote a system row")
	}
	p.assertBound(t)
}

// A compaction replaces the history with a summary, so no block from before
// it is sent again and the system text may change there: the engagement adopts
// this claim's composition, and the transcript records it in the same commit.
func TestThinkingBinding_CompactionAdoptsTheCurrentSystemText(t *testing.T) {
	tr := newMemTranscript(pendingUser("go"))
	full := overThresholdFor(t, fableParams().Model)
	p := newBindingProvider(
		thinking(scriptedTurn{calls: []domain.ToolCall{{ID: "c1", Name: "bash"}}, usage: inference.Usage{PromptTokens: full}}),
		thinking(summaryReply("everything that happened")),
		thinking(scriptedTurn{calls: []domain.ToolCall{{ID: "c2", Name: "bash"}}}),
		thinking(scriptedTurn{text: "done"}),
	)
	params := fableParams()
	params.CurrentSystem = changedSystem()
	runEngagement(t, tr, p, newScriptedToolHost(), params)
	p.assertBound(t)

	if tr.system == nil || tr.system.Prompt != params.CurrentSystem.Prompt || tr.system.Block != params.CurrentSystem.Block {
		t.Fatalf("the compaction recorded system text %+v, want this claim's composition", tr.system)
	}
	// The summarize call rides the conversation's cache, so it is the frozen
	// text; every call after the commit is the new one.
	for i, req := range p.requests {
		want := fableParams().SystemPrompt
		if i >= 2 {
			want = params.CurrentSystem.Prompt
		}
		if req.SystemPrompt != want {
			t.Errorf("call %d sent system %q, want %q", i+1, req.SystemPrompt, want)
		}
	}
}

// A resume that compacts before its first call has already made this claim's
// composition the conversation's text, so there is nothing left to tell.
func TestThinkingBinding_AResumeCompactionLeavesNoUpdate(t *testing.T) {
	tr := newMemTranscript(pendingUser("go"))
	p := newBindingProvider(
		thinking(scriptedTurn{text: "done", usage: inference.Usage{PromptTokens: overThresholdFor(t, fableParams().Model)}}),
		forcedReply("resume analysis", "resume summary"),
		thinking(scriptedTurn{text: "done again"}),
	)
	runEngagement(t, tr, p, newScriptedToolHost(), fableParams())
	tr.ageRows(time.Hour)
	_, _ = tr.Insert(context.Background(), "org", &domain.Message{
		ConversationID: "conv", Role: "user", Content: "one more thing", Delivered: boolPtr(false),
	})
	params := fableParams()
	params.SystemUpdate = systemUpdateText
	params.CurrentSystem = changedSystem()
	runEngagement(t, tr, p, newScriptedToolHost(), params)
	p.assertBound(t)

	if p.requests[1].SystemPrompt != coldCompactionSystemPrompt {
		t.Fatalf("staging failed: the resume's first call has system prompt %.40q…, want the forced-shape call", p.requests[1].SystemPrompt)
	}
	if tr.find(func(m domain.Message) bool { return m.Role == "system" }) != nil {
		t.Error("a resume that compacted first still appended the update")
	}
	if got := p.requests[2].SystemPrompt; got != params.CurrentSystem.Prompt {
		t.Errorf("the call after the resume compaction sent system %q, want this claim's composition", got)
	}
}
