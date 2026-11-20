package agentloop

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/maximhq/bifrost/core/schemas"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/llm/inference"
)

func usageOf(promptTokens int) inference.Usage { return inference.Usage{PromptTokens: promptTokens} }

// reclaimParams is fableParams for a re-claim whose composition moved: the
// saved blocks it sends, the blocks it composed, and the update between them.
func reclaimParams() Params {
	p := fableParams()
	p.NewSandbox = true
	p.Current = &SystemBlocks{Prompt: "system (next release)", Addendum: "conversation block (recomposed)"}
	p.SystemUpdate = &SystemUpdate{
		Content:  "The branch naming convention changed.",
		Metadata: map[string]any{domain.MessageMetadataSystemParts: map[string]any{"shared": []any{}}},
	}
	return p
}

func (t *memTranscript) systemUpdates() []domain.Message {
	var out []domain.Message
	for _, r := range t.snapshot() {
		if r.Role == "system" {
			out = append(out, r)
		}
	}
	return out
}

func oneMoreThing(tr *memTranscript) {
	_, _ = tr.Insert(context.Background(), "org", &domain.Message{
		ConversationID: "conv", Role: "user", Content: "one more thing", Delivered: boolPtr(false),
	})
}

// TestThinkingBinding_AResumeWithASystemUpdate is the re-claim the update is
// for: the blocks moved since the conversation's first claim, and every
// earlier thinking block is still replayed under the blocks it was produced
// under, with the change as a system message after the claim's input.
func TestThinkingBinding_AResumeWithASystemUpdate(t *testing.T) {
	tr := newMemTranscript(pendingUser("go"))
	p := newBindingProvider(
		thinking(scriptedTurn{calls: []domain.ToolCall{{ID: "c1", Name: "bash"}}}),
		thinking(scriptedTurn{text: "done"}),
		thinking(scriptedTurn{calls: []domain.ToolCall{{ID: "c2", Name: "bash"}}}),
		thinking(scriptedTurn{text: "done again"}),
	)
	runEngagement(t, tr, p, newScriptedToolHost(), fableParams())
	tr.ageRows(time.Hour)
	oneMoreThing(tr)
	runEngagement(t, tr, p, newScriptedToolHost(), reclaimParams())

	p.assertBound(t)
	if got := replayedBlocks(t, p.requests[2]); got != 2 {
		t.Errorf("the re-claim's first call replays %d blocks, want both the first claim produced", got)
	}
	for _, call := range []int{2, 3} {
		req := p.requests[call]
		if req.SystemPrompt != "system" || req.SystemAddendum != "conversation block" {
			t.Errorf("call %d sends system blocks (%q, %q), want the ones the first claim sent", call+1, req.SystemPrompt, req.SystemAddendum)
		}
	}

	updates := tr.systemUpdates()
	if len(updates) != 1 {
		t.Fatalf("system rows = %d, want the one update", len(updates))
	}
	if updates[0].Subtype != domain.MessageSubtypeSystemUpdate || updates[0].Content != "The branch naming convention changed." {
		t.Errorf("update row = %+v", updates[0])
	}
	if _, ok := updates[0].Metadata[domain.MessageMetadataSystemParts]; !ok {
		t.Error("the update row does not record the parts it was made from")
	}

	_, msgs, err := contextOf(p.requests[2])
	if err != nil {
		t.Fatal(err)
	}
	last := msgs[len(msgs)-1]
	if last.Role != schemas.ChatMessageRoleSystem {
		t.Fatalf("the re-claim's first call ends with a %s message, want the update", last.Role)
	}
	if prev := msgs[len(msgs)-2]; prev.Role != schemas.ChatMessageRoleUser {
		t.Errorf("the update follows a %s message, want the claim's input", prev.Role)
	}
}

// TestThinkingBinding_ACompactionMovesTheCallsOntoTheNewestBlocks: once a
// compaction commits, no row a thinking block rides on is sent again, so the
// engagement saves the blocks its claim composed and every later call sends
// them, while the compaction's own call still sends the old ones.
func TestThinkingBinding_ACompactionMovesTheCallsOntoTheNewestBlocks(t *testing.T) {
	tr := newMemTranscript(pendingUser("go"))
	full := overThresholdFor(t, fableParams().Model)
	p := newBindingProvider(
		thinking(scriptedTurn{text: "done"}),
		thinking(scriptedTurn{calls: []domain.ToolCall{{ID: "c1", Name: "bash"}}, usage: usageOf(full)}),
		thinking(summaryReply("everything that happened")),
		thinking(scriptedTurn{calls: []domain.ToolCall{{ID: "c2", Name: "bash"}}}),
		thinking(scriptedTurn{text: "done again"}),
	)
	runEngagement(t, tr, p, newScriptedToolHost(), fableParams())
	oneMoreThing(tr)

	var saved []SystemBlocks
	engine := newTestEngine(tr, p, newScriptedToolHost())
	engine.Hooks.SaveSystemBlocks = func(_ context.Context, blocks SystemBlocks) error {
		saved = append(saved, blocks)
		return nil
	}
	params := reclaimParams()
	result := engine.Run(context.Background(), params)
	if result.Kind != ResultConcluded {
		t.Fatalf("disposition = %v, want concluded (err: %v)", result.Kind, result.Err)
	}
	if tr.find(func(m domain.Message) bool { return m.Subtype == domain.MessageSubtypeInjectionCompactionResult }) == nil {
		t.Fatal("staging failed: the engagement never compacted")
	}

	p.assertBound(t)
	if len(saved) != 1 || saved[0] != *params.Current {
		t.Errorf("saved blocks = %+v, want the claim's composition once", saved)
	}
	for call, want := range map[int]string{1: "system", 2: "system", 3: "system (next release)", 4: "system (next release)"} {
		if got := p.requests[call].SystemPrompt; got != want {
			t.Errorf("call %d sends shared prompt %q, want %q", call+1, got, want)
		}
	}
	if got := p.requests[3].SystemAddendum; got != params.Current.Addendum {
		t.Errorf("the first call after the compaction sends block 2 %q, want the claim's", got)
	}
	if result.SystemPrompt != params.Current.Prompt || result.SystemAddendum != params.Current.Addendum {
		t.Errorf("result system blocks = (%q, %q), want the claim's composition", result.SystemPrompt, result.SystemAddendum)
	}
}

// TestSystemUpdate_AFailedSaveKeepsTheBlocksTheEngagementWasSending: the next
// claim reads the saved blocks back, so an engagement whose save failed must
// go on sending what is saved.
func TestSystemUpdate_AFailedSaveKeepsTheBlocksTheEngagementWasSending(t *testing.T) {
	tr := newMemTranscript(pendingUser("go"))
	full := overThresholdFor(t, fableParams().Model)
	p := newBindingProvider(
		thinking(scriptedTurn{calls: []domain.ToolCall{{ID: "c1", Name: "bash"}}, usage: usageOf(full)}),
		thinking(summaryReply("everything that happened")),
		thinking(scriptedTurn{text: "done"}),
	)
	engine := newTestEngine(tr, p, newScriptedToolHost())
	engine.Hooks.SaveSystemBlocks = func(context.Context, SystemBlocks) error { return errors.New("connection reset") }
	result := engine.Run(context.Background(), reclaimParams())
	if result.Kind != ResultConcluded {
		t.Fatalf("disposition = %v, want concluded (err: %v)", result.Kind, result.Err)
	}
	if got := p.requests[2].SystemPrompt; got != "system" {
		t.Errorf("the call after a compaction whose save failed sends %q, want the saved prompt", got)
	}
	if result.SystemPrompt != "system" {
		t.Errorf("result.SystemPrompt = %q, want the saved prompt", result.SystemPrompt)
	}
	p.assertBound(t)
}

// TestSystemUpdate_AResumeCompactionCarriesTheChangeItself: a compaction that
// commits before the claim's first drain leaves nothing earlier to bind, so the
// claim's blocks are sent from the first call and no update is written.
func TestSystemUpdate_AResumeCompactionCarriesTheChangeItself(t *testing.T) {
	tr := newMemTranscript(pendingUser("go"))
	p := newBindingProvider(
		thinking(scriptedTurn{text: "done", usage: usageOf(overThresholdFor(t, fableParams().Model))}),
		forcedReply("resume analysis", "resume summary"),
		thinking(scriptedTurn{text: "done again"}),
	)
	runEngagement(t, tr, p, newScriptedToolHost(), fableParams())
	tr.ageRows(time.Hour)
	oneMoreThing(tr)

	var saved int
	engine := newTestEngine(tr, p, newScriptedToolHost())
	engine.Hooks.SaveSystemBlocks = func(context.Context, SystemBlocks) error { saved++; return nil }
	params := reclaimParams()
	if result := engine.Run(context.Background(), params); result.Kind != ResultConcluded {
		t.Fatalf("disposition = %v, want concluded (err: %v)", result.Kind, result.Err)
	}
	if p.requests[1].SystemPrompt != coldCompactionSystemPrompt {
		t.Fatal("staging failed: the re-claim did not compact on resume")
	}
	if saved != 1 {
		t.Errorf("saves = %d, want one", saved)
	}
	if got := p.requests[2]; got.SystemPrompt != params.Current.Prompt || got.SystemAddendum != params.Current.Addendum {
		t.Errorf("the first working call sends (%q, %q), want the claim's composition", got.SystemPrompt, got.SystemAddendum)
	}
	if n := len(tr.systemUpdates()); n != 0 {
		t.Errorf("system rows = %d, want none: the blocks carry the change", n)
	}
	p.assertBound(t)
}
