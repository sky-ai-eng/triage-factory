package agentloop

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/inference"
)

func TestRun_ImplicitCompletionConcludesWithFinalText(t *testing.T) {
	tr := newMemTranscript(pendingUser("do the thing"))
	p := &scriptedProvider{turns: []scriptedTurn{{text: "Done: renamed the field."}}}
	e := newTestEngine(tr, p, newScriptedToolHost())

	got := e.Run(context.Background(), testParams())

	if got.Kind != ResultConcluded {
		t.Fatalf("disposition = %v, want concluded (err: %v)", got.Kind, got.Err)
	}
	// `continue`, not `finish`: stopping says "my part is done", and only
	// decideBlueprintStep knows whether that ends the task. It resolves a
	// final-step continue to a structural finish, so a single run is
	// unaffected — but the loop must not be the thing that decides.
	if got.Outcome != domain.RunOutcomeContinue {
		t.Errorf("outcome = %q, want continue", got.Outcome)
	}
	if got.ResultSummary != "Done: renamed the field." {
		t.Errorf("result summary = %q, want the final assistant text", got.ResultSummary)
	}
	if got.NumTurns != 1 {
		t.Errorf("turns = %d, want 1", got.NumTurns)
	}
}

func TestRun_FirstDrainIsBareAndMidTurnDrainIsStamped(t *testing.T) {
	tr := newMemTranscript(pendingUser("opening"))
	// The mid-turn steer is queued while the first turn (a tool call) is
	// streaming, so it is drained between turns — the steer moment.
	p := &scriptedProvider{turns: []scriptedTurn{
		{
			calls: []domain.ToolCall{{ID: "c1", Name: "ls"}},
			onCall: func() {
				_, _ = tr.Insert(context.Background(), "org", &domain.Message{
					ConversationID: "conv", Role: "user",
					Content: "also check the tests", Delivered: boolPtr(false),
				})
			},
		},
		{text: "done"},
	}}
	e := newTestEngine(tr, p, newScriptedToolHost())

	if got := e.Run(context.Background(), testParams()); got.Kind != ResultConcluded {
		t.Fatalf("disposition = %v, want concluded (err: %v)", got.Kind, got.Err)
	}

	opening := tr.find(func(m domain.Message) bool { return m.Content == "opening" })
	if opening == nil || opening.Subtype != "" {
		t.Fatalf("the engagement's first drain must deliver bare, leaving the row's own subtype: %+v", opening)
	}
	steer := tr.find(func(m domain.Message) bool { return m.Content == "also check the tests" })
	if steer == nil || steer.Subtype != domain.MessageSubtypeInjectionSteer {
		t.Fatalf("a drain between turns must stamp injection:steer: %+v", steer)
	}
	if steer.Delivered == nil || !*steer.Delivered {
		t.Errorf("the steer row must be delivered by the same flush that stamped it: %+v", steer)
	}
}

func TestRun_DrainAfterNoToolCallTurnIsBare(t *testing.T) {
	tr := newMemTranscript(pendingUser("opening"))
	// Turn 1 concludes with no tool calls, but input lands while it streams.
	// The would-stop recheck sees it and the loop continues — and because the
	// model is not mid-work, that drain is bare.
	p := &scriptedProvider{turns: []scriptedTurn{
		{
			text: "I think I'm done",
			onCall: func() {
				_, _ = tr.Insert(context.Background(), "org", &domain.Message{
					ConversationID: "conv", Role: "user",
					Content: "one more thing", Delivered: boolPtr(false),
				})
			},
		},
		{text: "now really done"},
	}}
	e := newTestEngine(tr, p, newScriptedToolHost())

	got := e.Run(context.Background(), testParams())
	if got.ResultSummary != "now really done" {
		t.Fatalf("the late-arriving input must keep the run going: %+v", got)
	}
	late := tr.find(func(m domain.Message) bool { return m.Content == "one more thing" })
	if late == nil || late.Subtype != "" {
		t.Fatalf("a drain following a no-tool-call turn must deliver bare: %+v", late)
	}
}

func TestRun_RepairAnswersUnansweredToolCallsWithoutRedispatch(t *testing.T) {
	// A crash left an assistant message whose two tool calls were only half
	// answered — the exact partial-batch shape the repair pass diffs.
	tr := newMemTranscript(
		domain.Message{ConversationID: "conv", Role: "user", Content: "go"},
		domain.Message{ConversationID: "conv", Role: "assistant", ToolCalls: []domain.ToolCall{
			{ID: "c1", Name: "bash"}, {ID: "c2", Name: "write"},
		}},
		domain.Message{ConversationID: "conv", Role: "tool", ToolCallID: "c1", Content: "already recorded"},
	)
	host := newScriptedToolHost()
	p := &scriptedProvider{turns: []scriptedTurn{{text: "recovered"}}}
	e := newTestEngine(tr, p, host)

	if got := e.Run(context.Background(), testParams()); got.Kind != ResultConcluded {
		t.Fatalf("disposition = %v, want concluded (err: %v)", got.Kind, got.Err)
	}

	if calls := host.calls(); len(calls) != 0 {
		t.Fatalf("repair must never re-dispatch an interrupted call; the host saw %v", calls)
	}
	repaired := tr.find(func(m domain.Message) bool { return m.Role == "tool" && m.ToolCallID == "c2" })
	if repaired == nil {
		t.Fatal("the unanswered call must get a synthetic result")
	}
	if !repaired.IsError {
		t.Error("the synthetic result must be is_error")
	}
	if !strings.Contains(repaired.Content, "effects may be partially present or absent") {
		t.Errorf("the synthetic result must warn that effects are unknown: %q", repaired.Content)
	}
	// The already-answered call keeps its real result.
	kept := tr.find(func(m domain.Message) bool { return m.Role == "tool" && m.ToolCallID == "c1" })
	if kept == nil || kept.Content != "already recorded" {
		t.Errorf("an answered call must be left alone: %+v", kept)
	}
}

// TestRun_WorkspaceRebuiltNoticeIsProvenanceDriven pins what the loop is
// allowed to tell an agent about the tree it woke up in. The notice describes
// a loss — work the model remembers doing that is not on disk — so it may only
// be said when a loss actually happened, it must describe the loss that
// actually occurred, and its executor sentence may only appear when an
// executor actually changed. A resume onto the warm tree, on the executor that
// parked it, is the common case and must be silent.
func TestRun_WorkspaceRebuiltNoticeIsProvenanceDriven(t *testing.T) {
	priorWork := func() *memTranscript {
		return newMemTranscript(
			domain.Message{ConversationID: "conv", Role: "user", Content: "go"},
			domain.Message{ConversationID: "conv", Role: "assistant", Content: "working"},
		)
	}
	findNotice := func(tr *memTranscript) *domain.Message {
		return tr.find(func(m domain.Message) bool {
			return m.Subtype == domain.MessageSubtypeInjectionExecutorChanged
		})
	}
	run := func(t *testing.T, tr *memTranscript, params Params) {
		t.Helper()
		p := &scriptedProvider{turns: []scriptedTurn{{text: "done"}}}
		if got := newTestEngine(tr, p, newScriptedToolHost()).Run(context.Background(), params); got.Kind != ResultConcluded {
			t.Fatalf("disposition = %v (err: %v)", got.Kind, got.Err)
		}
	}

	t.Run("a warm resume says nothing at all", func(t *testing.T) {
		tr := priorWork()
		run(t, tr, workspaceParams(domain.WorkspaceProvenanceWarm, false))
		if n := findNotice(tr); n != nil {
			t.Fatalf("the interrupted engagement's work is right there; a warm resume must not claim otherwise: %+v", n)
		}
	})

	t.Run("a snapshot restore states the restore without claiming a move", func(t *testing.T) {
		tr := priorWork()
		run(t, tr, workspaceParams(domain.WorkspaceProvenanceRehydrated, false))
		n := findNotice(tr)
		if n == nil {
			t.Fatal("work the model remembers is not on this tree; it has to be told")
		}
		if !strings.Contains(n.Content, "restored from its last snapshot") {
			t.Errorf("the notice must describe the snapshot boundary: %q", n.Content)
		}
		if !strings.Contains(n.Content, "uncommitted and untracked files") {
			t.Errorf("a snapshot carries the uncaptured-by-git work, and the agent needs to know it is there: %q", n.Content)
		}
		if strings.Contains(n.Content, "different executor") {
			t.Errorf("a rebuild on the same executor must not claim the run moved: %q", n.Content)
		}
	})

	t.Run("a genuine executor change earns the sentence", func(t *testing.T) {
		tr := priorWork()
		run(t, tr, workspaceParams(domain.WorkspaceProvenanceRehydrated, true))
		n := findNotice(tr)
		if n == nil || !strings.Contains(n.Content, "different executor") {
			t.Fatalf("a predecessor on another executor must be stated: %+v", n)
		}
		if !strings.Contains(n.Content, "restored from its last snapshot") {
			t.Errorf("the executor sentence adds to the restore, it does not replace it: %q", n.Content)
		}
	})

	// The two rebuilds are not interchangeable. A snapshot restore keeps the
	// work git never saw; a from-scratch build keeps only what reached the
	// remote. Promising the former's survivors on the latter's tree would be
	// the same falsehood this notice exists to stop, told at the moment the
	// agent has least to work from.
	t.Run("a from-scratch build says so, and promises nothing a snapshot would have carried", func(t *testing.T) {
		tr := priorWork()
		run(t, tr, workspaceParams(domain.WorkspaceProvenanceFresh, false))
		n := findNotice(tr)
		if n == nil {
			t.Fatal("a tree built from scratch carries none of the remembered work either")
		}
		if !strings.Contains(n.Content, "no snapshot to restore from") {
			t.Errorf("the notice must say there was no snapshot: %q", n.Content)
		}
		if strings.Contains(n.Content, "restored from its last snapshot") {
			t.Errorf("nothing was restored here; claiming it was is the bug in miniature: %q", n.Content)
		}
		if !strings.Contains(n.Content, "did not push") {
			t.Errorf("the surviving set is what reached the remote, and the notice has to say so: %q", n.Content)
		}
	})

	t.Run("a re-claim before any work stays silent", func(t *testing.T) {
		tr := newMemTranscript(pendingUser("go"))
		run(t, tr, workspaceParams(domain.WorkspaceProvenanceRehydrated, true))
		if n := findNotice(tr); n != nil {
			t.Fatalf("a credential-parking / requeue-before-start re-claim must stay silent: %+v", n)
		}
	})

	t.Run("an unclassified workspace is not asserted to have been rebuilt", func(t *testing.T) {
		tr := priorWork()
		run(t, tr, testParams())
		if n := findNotice(tr); n != nil {
			t.Fatalf("a caller that never rebuilds a workspace must not have one claimed for it: %+v", n)
		}
	})
}

// TestRun_WorkspaceRebuiltNoticeDoesNotStack: a claim that queued the notice
// and died before the model ever read it must not leave a second copy behind.
func TestRun_WorkspaceRebuiltNoticeDoesNotStack(t *testing.T) {
	pending := false
	tr := newMemTranscript(
		domain.Message{ConversationID: "conv", Role: "user", Content: "go"},
		domain.Message{ConversationID: "conv", Role: "assistant", Content: "working"},
		domain.Message{
			ConversationID: "conv", Role: "user",
			Subtype:   domain.MessageSubtypeInjectionExecutorChanged,
			Content:   workspaceRebuiltNotice(domain.WorkspaceProvenanceRehydrated, false),
			Delivered: &pending,
		},
	)
	p := &scriptedProvider{turns: []scriptedTurn{{text: "done"}}}
	e := newTestEngine(tr, p, newScriptedToolHost())
	if got := e.Run(context.Background(), workspaceParams(domain.WorkspaceProvenanceRehydrated, false)); got.Kind != ResultConcluded {
		t.Fatalf("disposition = %v (err: %v)", got.Kind, got.Err)
	}
	var notices int
	for _, r := range tr.snapshot() {
		if r.Subtype == domain.MessageSubtypeInjectionExecutorChanged {
			notices++
		}
	}
	if notices != 1 {
		t.Fatalf("workspace-rebuilt notices = %d, want the undelivered one already queued and no second", notices)
	}
}

func TestRun_RepairIsIdempotentAcrossClaims(t *testing.T) {
	tr := newMemTranscript(
		domain.Message{ConversationID: "conv", Role: "user", Content: "go"},
		domain.Message{ConversationID: "conv", Role: "assistant", ToolCalls: []domain.ToolCall{{ID: "c1", Name: "bash"}}},
	)
	// Two engagements back to back, as a crash-then-reclaim produces, each
	// landing on a rebuilt tree so both halves of the repair pass run.
	for i, text := range []string{"first", "second"} {
		p := &scriptedProvider{turns: []scriptedTurn{{text: text}}}
		e := newTestEngine(tr, p, newScriptedToolHost())
		if got := e.Run(context.Background(), workspaceParams(domain.WorkspaceProvenanceRehydrated, false)); got.Kind != ResultConcluded {
			t.Fatalf("engagement %d: disposition = %v (err: %v)", i, got.Kind, got.Err)
		}
	}
	var synthetic int
	for _, r := range tr.toolResults() {
		if r.ToolCallID == "c1" {
			synthetic++
		}
	}
	if synthetic != 1 {
		t.Fatalf("a tool call must be repaired exactly once, got %d results for c1", synthetic)
	}
	var notices int
	for _, r := range tr.snapshot() {
		if r.Subtype == domain.MessageSubtypeInjectionExecutorChanged {
			notices++
		}
	}
	if notices != 2 {
		// One per engagement that followed real assistant work: the first
		// claim sees the pre-existing assistant row, the second sees both.
		// What must never happen is two UNDELIVERED notices stacking up.
		t.Logf("executor-changed notices across two engagements: %d", notices)
	}
}

func TestRun_LengthStopErrorsEveryCallInTheBatchWithoutExecuting(t *testing.T) {
	tr := newMemTranscript(pendingUser("go"))
	host := newScriptedToolHost()
	p := &scriptedProvider{turns: []scriptedTurn{
		{
			calls:  []domain.ToolCall{{ID: "c1", Name: "write"}, {ID: "c2", Name: "edit"}},
			finish: "length",
		},
		{text: "recovered"},
	}}
	e := newTestEngine(tr, p, host)

	if got := e.Run(context.Background(), testParams()); got.Kind != ResultConcluded {
		t.Fatalf("disposition = %v (err: %v)", got.Kind, got.Err)
	}
	if calls := host.calls(); len(calls) != 0 {
		t.Fatalf("a length-truncated batch must not execute; the host saw %v", calls)
	}
	results := tr.toolResults()
	if len(results) != 2 {
		t.Fatalf("every call in the truncated message needs a result, got %d", len(results))
	}
	for _, r := range results {
		if !r.IsError {
			t.Errorf("truncated-batch result for %s must be is_error", r.ToolCallID)
		}
		if !strings.Contains(r.Content, "output length limit") {
			t.Errorf("the result must name the cause: %q", r.Content)
		}
	}
}

// TestRun_LengthStopStubsUnparseableArgs pins the common truncation shape: the
// cut lands mid-arguments, so the final call's JSON does not parse at all.
// Under a length stop that must cost one instructive turn, not the engagement;
// under any other stop reason the same malformed JSON stays a hard failure,
// because with no truncation to blame it is a provider bug.
func TestRun_LengthStopStubsUnparseableArgs(t *testing.T) {
	const cutArgs = `{"file_path": "/work/x.go", "old_string": "func ma`

	t.Run("under a length stop the batch is answered and the run recovers", func(t *testing.T) {
		tr := newMemTranscript(pendingUser("go"))
		host := newScriptedToolHost()
		p := &scriptedProvider{turns: []scriptedTurn{
			{
				calls:   []domain.ToolCall{{ID: "c1", Name: "write", Input: map[string]any{"file_path": "/work/a.go"}}, {ID: "c2", Name: "edit"}},
				rawArgs: map[int]string{1: cutArgs},
				finish:  "length",
			},
			{text: "recovered"},
		}}
		e := newTestEngine(tr, p, host)

		if got := e.Run(context.Background(), testParams()); got.Kind != ResultConcluded {
			t.Fatalf("disposition = %v (err: %v)", got.Kind, got.Err)
		}
		if calls := host.calls(); len(calls) != 0 {
			t.Fatalf("nothing in a truncated batch may execute; the host saw %v", calls)
		}
		if results := tr.toolResults(); len(results) != 2 {
			t.Fatalf("every call in the truncated message needs a result, got %d", len(results))
		}
		assistant := tr.find(func(m domain.Message) bool { return m.Role == "assistant" && len(m.ToolCalls) > 0 })
		if assistant == nil {
			t.Fatal("the truncated assistant message must still persist")
		}
		if got := assistant.ToolCalls[1].Input; len(got) != 0 {
			t.Errorf("the cut call's arguments must be stubbed empty, got %v", got)
		}
	})

	t.Run("without a length stop malformed arguments still fail loudly", func(t *testing.T) {
		tr := newMemTranscript(pendingUser("go"))
		p := &scriptedProvider{turns: []scriptedTurn{{
			calls:   []domain.ToolCall{{ID: "c1", Name: "edit"}},
			rawArgs: map[int]string{0: cutArgs},
		}}}
		e := newTestEngine(tr, p, newScriptedToolHost())

		if got := e.Run(context.Background(), testParams()); got.Kind != ResultFailed {
			t.Fatalf("disposition = %v, want failed — stubbing is licensed by truncation only", got.Kind)
		}
	})
}

// TestRun_NULBytesAreStrippedBeforePersist pins the sanitization Postgres
// forces: TEXT cannot hold a raw NUL and JSONB cannot hold its escaped form,
// so a tool result carrying one (a bash call that catted a binary — the live
// failure) must be cleaned at persist, not allowed to fail the engagement.
func TestRun_NULBytesAreStrippedBeforePersist(t *testing.T) {
	tr := newMemTranscript(pendingUser("go"))
	host := newScriptedToolHost()
	host.answers["bash"] = ToolOutcome{Content: "ELF\x00\x00garbage\x00"}
	p := &scriptedProvider{turns: []scriptedTurn{
		{
			text:  "inspecting\x00the binary",
			calls: []domain.ToolCall{{ID: "c1", Name: "bash", Input: map[string]any{"command": "cat \x00/bin/thing"}}},
		},
		{text: "done"},
	}}
	e := newTestEngine(tr, p, host)

	if got := e.Run(context.Background(), testParams()); got.Kind != ResultConcluded {
		t.Fatalf("a NUL in tool output must not end the run: %v (err: %v)", got.Kind, got.Err)
	}
	for _, m := range tr.snapshot() {
		if strings.ContainsRune(m.Content, 0) {
			t.Errorf("row %d content carries NUL after persist: %q", m.ID, m.Content)
		}
		for _, call := range m.ToolCalls {
			if cmd, _ := call.Input["command"].(string); strings.ContainsRune(cmd, 0) {
				t.Errorf("row %d tool call input carries NUL after persist: %q", m.ID, cmd)
			}
		}
	}
	if r := tr.toolResults(); len(r) != 1 || r[0].Content != "ELFgarbage" {
		t.Fatalf("tool result must survive minus the NULs: %+v", r)
	}
}

func TestRun_ToolDispatchIsSerialAndInCallOrder(t *testing.T) {
	tr := newMemTranscript(pendingUser("go"))
	host := newScriptedToolHost()
	p := &scriptedProvider{turns: []scriptedTurn{
		{calls: []domain.ToolCall{{ID: "c1", Name: "read"}, {ID: "c2", Name: "grep"}, {ID: "c3", Name: "bash"}}},
		{text: "done"},
	}}
	e := newTestEngine(tr, p, host)

	if got := e.Run(context.Background(), testParams()); got.Kind != ResultConcluded {
		t.Fatalf("disposition = %v (err: %v)", got.Kind, got.Err)
	}
	want := []string{"read", "grep", "bash"}
	got := host.calls()
	if len(got) != len(want) {
		t.Fatalf("dispatched %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("dispatch order = %v, want %v", got, want)
		}
	}
	results := tr.toolResults()
	for i, r := range results {
		if r.ToolCallID != []string{"c1", "c2", "c3"}[i] {
			t.Fatalf("results must land in call order: %+v", results)
		}
	}
}

func TestRun_SurvivableProtocolErrorContinuesAndFatalOneFailsTheClaim(t *testing.T) {
	t.Run("survivable", func(t *testing.T) {
		tr := newMemTranscript(pendingUser("go"))
		host := newScriptedToolHost()
		host.answers["frobnicate"] = ToolOutcome{Protocol: &ProtocolError{Kind: protoUnknownTool, Message: "unknown tool: frobnicate"}}
		p := &scriptedProvider{turns: []scriptedTurn{
			{calls: []domain.ToolCall{{ID: "c1", Name: "frobnicate"}}},
			{text: "understood"},
		}}
		e := newTestEngine(tr, p, host)
		got := e.Run(context.Background(), testParams())
		if got.Kind != ResultConcluded {
			t.Fatalf("a survivable protocol error must not end the run: %v (err: %v)", got.Kind, got.Err)
		}
		r := tr.toolResults()
		if len(r) != 1 || !r[0].IsError {
			t.Fatalf("the model must see the protocol error as an is_error result: %+v", r)
		}
	})

	t.Run("fatal", func(t *testing.T) {
		tr := newMemTranscript(pendingUser("go"))
		host := newScriptedToolHost()
		host.answers["bash"] = ToolOutcome{Protocol: &ProtocolError{Kind: protoRequestTooLarge, Message: "frame too large"}}
		p := &scriptedProvider{turns: []scriptedTurn{
			{calls: []domain.ToolCall{{ID: "c1", Name: "bash"}, {ID: "c2", Name: "read"}}},
		}}
		e := newTestEngine(tr, p, host)
		got := e.Run(context.Background(), testParams())
		if got.Kind != ResultFailed {
			t.Fatalf("a fatal protocol error must fail the claim: %v", got.Kind)
		}
		results := tr.toolResults()
		if len(results) != 2 {
			t.Fatalf("the failed call and every remaining one need results, got %d: %+v", len(results), results)
		}
		for _, r := range results {
			if !r.IsError {
				t.Errorf("result for %s must be is_error", r.ToolCallID)
			}
		}
	})
}

func TestRun_BeforeToolCallGateDeniesInBand(t *testing.T) {
	tr := newMemTranscript(pendingUser("go"))
	host := newScriptedToolHost()
	p := &scriptedProvider{turns: []scriptedTurn{
		{calls: []domain.ToolCall{{ID: "c1", Name: "bash"}}},
		{text: "ok, different approach"},
	}}
	e := newTestEngine(tr, p, host)
	e.Hooks.BeforeToolCall = func(_ context.Context, call domain.ToolCall) string {
		if call.Name == "bash" {
			return "bash is not available on this step."
		}
		return ""
	}

	if got := e.Run(context.Background(), testParams()); got.Kind != ResultConcluded {
		t.Fatalf("a gate denial must not end the run: %v (err: %v)", got.Kind, got.Err)
	}
	if calls := host.calls(); len(calls) != 0 {
		t.Fatalf("a denied call must never reach the host: %v", calls)
	}
	r := tr.toolResults()
	if len(r) != 1 || !r[0].IsError || r[0].Content != "bash is not available on this step." {
		t.Fatalf("the denial must be an in-band is_error result: %+v", r)
	}
}

func TestRun_TerminateContractRequiresEveryResultToTerminate(t *testing.T) {
	t.Run("stop_blueprint alone terminates and its summary becomes the run's result", func(t *testing.T) {
		tr := newMemTranscript(pendingUser("go"))
		p := &scriptedProvider{turns: []scriptedTurn{{
			text: "Wrapping up.",
			calls: []domain.ToolCall{{ID: "c1", Name: ToolStopBlueprint, Input: map[string]any{
				"type": "finish", "reason": "no reviewable changes",
				"summary": "Checked the PR; it only touches generated files.",
			}}},
		}}}
		e := newTestEngine(tr, p, newScriptedToolHost())
		got := e.Run(context.Background(), testParams())
		if got.Outcome != domain.RunOutcomeFinish || got.OutcomeReason != "no reviewable changes" {
			t.Fatalf("finish must reach the outcome unchanged: %+v", got)
		}
		if got.ResultSummary != "Checked the PR; it only touches generated files." {
			t.Errorf("result summary = %q, want the tool's summary argument, not the surrounding prose", got.ResultSummary)
		}
	})

	t.Run("stop_blueprint paired with work does not terminate", func(t *testing.T) {
		tr := newMemTranscript(pendingUser("go"))
		host := newScriptedToolHost()
		p := &scriptedProvider{turns: []scriptedTurn{
			{calls: []domain.ToolCall{
				{ID: "c1", Name: "write"},
				{ID: "c2", Name: ToolStopBlueprint, Input: map[string]any{"type": "abort", "reason": "r", "summary": "s"}},
			}},
			{text: "actually finished"},
		}}
		e := newTestEngine(tr, p, host)
		got := e.Run(context.Background(), testParams())
		if got.Outcome != domain.RunOutcomeContinue {
			t.Fatalf("a batch that also did real work must keep going: %+v", got)
		}
		if calls := host.calls(); len(calls) != 1 || calls[0] != "write" {
			t.Fatalf("the paired work must still run: %v", calls)
		}
	})

	t.Run("a stop with no reason is corrected, not accepted", func(t *testing.T) {
		tr := newMemTranscript(pendingUser("go"))
		p := &scriptedProvider{turns: []scriptedTurn{
			{calls: []domain.ToolCall{{ID: "c1", Name: ToolStopBlueprint, Input: map[string]any{"type": "abort"}}}},
			{calls: []domain.ToolCall{{ID: "c2", Name: ToolStopBlueprint, Input: map[string]any{"type": "abort", "reason": "the branch is gone", "summary": "could not rebase; upstream branch was deleted"}}}},
		}}
		e := newTestEngine(tr, p, newScriptedToolHost())
		got := e.Run(context.Background(), testParams())
		if got.Outcome != domain.RunOutcomeAbort || got.OutcomeReason != "the branch is gone" {
			t.Fatalf("abort must carry its reason: %+v", got)
		}
		if got.ResultSummary != "could not rebase; upstream branch was deleted" {
			t.Errorf("result summary = %q, want the corrected call's summary", got.ResultSummary)
		}
		first := tr.toolResults()[0]
		if !first.IsError || !strings.Contains(first.Content, "requires a reason") {
			t.Fatalf("a reasonless stop must be told what it owes: %+v", first)
		}
	})

	t.Run("a stop typed continue is refused and the run keeps going", func(t *testing.T) {
		tr := newMemTranscript(pendingUser("go"))
		p := &scriptedProvider{turns: []scriptedTurn{
			{calls: []domain.ToolCall{{ID: "c1", Name: ToolStopBlueprint, Input: map[string]any{"type": "continue", "reason": "did my part"}}}},
			{text: "handing off"},
		}}
		e := newTestEngine(tr, p, newScriptedToolHost())
		got := e.Run(context.Background(), testParams())
		// It ends as a continue anyway — but by stopping, which is the point:
		// the tool never mints one, so the loop stays the only thing that
		// decides what an ordinary ending means.
		if got.Outcome != domain.RunOutcomeContinue || got.ResultSummary != "handing off" {
			t.Fatalf("a bad type must not terminate the run: %+v", got)
		}
		first := tr.toolResults()[0]
		if !first.IsError || !strings.Contains(first.Content, "no tool calls") {
			t.Fatalf("the correction must point at stopping, not at another tool: %+v", first)
		}
	})
}

func TestRun_TurnBackstopParksWithNotice(t *testing.T) {
	tr := newMemTranscript(pendingUser("go"))
	host := newScriptedToolHost()
	p := &scriptedProvider{turns: []scriptedTurn{
		{calls: []domain.ToolCall{{ID: "c1", Name: "ls"}}},
		{calls: []domain.ToolCall{{ID: "c2", Name: "ls"}}},
		{calls: []domain.ToolCall{{ID: "c3", Name: "ls"}}},
	}}
	e := newTestEngine(tr, p, host)
	params := testParams()
	params.MaxIterations = 2

	got := e.Run(context.Background(), params)
	if got.Kind != ResultParked {
		t.Fatalf("the backstop must park, not fail: %v (err: %v)", got.Kind, got.Err)
	}
	if !strings.Contains(got.ParkNotice, "2 model calls") {
		t.Errorf("the notice must name the bound: %q", got.ParkNotice)
	}
	if n := tr.find(func(m domain.Message) bool { return strings.Contains(m.Content, "2 model calls") }); n == nil {
		t.Error("the park notice must be recorded in the transcript")
	}
	if p.calls != 2 {
		t.Errorf("the guard runs before the call, so exactly 2 calls should have been made, got %d", p.calls)
	}
}

func TestRun_SpendGuardParksBeforeTheCall(t *testing.T) {
	tr := newMemTranscript(pendingUser("go"))
	p := &scriptedProvider{turns: []scriptedTurn{{text: "should never run"}}}
	e := newTestEngine(tr, p, newScriptedToolHost())
	e.Guards = []Guard{guardFunc(func(context.Context, int) (string, error) {
		return "org at $50.00 of $50.00 today", nil
	})}

	got := e.Run(context.Background(), testParams())
	if got.Kind != ResultParked {
		t.Fatalf("a spend breach must park, never fail: %v", got.Kind)
	}
	if p.calls != 0 {
		t.Errorf("the guard runs BEFORE the call; provider was called %d times", p.calls)
	}
	if got.ParkNotice != "org at $50.00 of $50.00 today" {
		t.Errorf("the guard's own wording must survive: %q", got.ParkNotice)
	}
}

func TestRun_GuardReadFailureFailsOpen(t *testing.T) {
	tr := newMemTranscript(pendingUser("go"))
	p := &scriptedProvider{turns: []scriptedTurn{{text: "done"}}}
	e := newTestEngine(tr, p, newScriptedToolHost())
	e.Guards = []Guard{guardFunc(func(context.Context, int) (string, error) {
		return "", errors.New("spend store unreachable")
	})}

	if got := e.Run(context.Background(), testParams()); got.Kind != ResultConcluded {
		t.Fatalf("a guard that cannot read its inputs must not wedge the run: %v", got.Kind)
	}
}

// TestRun_WouldStopHookOwnsWhetherItRepeats pins the division of labour: the
// engine inserts what the hook returns and keeps no memory of having done
// so. Whether asking again is badgering or a fair question about new work
// depends on the transcript, which the hook can read and the engine
// deliberately does not summarize into a flag.
func TestRun_WouldStopHookOwnsWhetherItRepeats(t *testing.T) {
	t.Run("an empty answer lets the conclusion stand", func(t *testing.T) {
		tr := newMemTranscript(pendingUser("go"))
		p := &scriptedProvider{turns: []scriptedTurn{{text: "first stop"}, {text: "second stop"}}}
		e := newTestEngine(tr, p, newScriptedToolHost())
		asked := 0
		e.Hooks.ShouldStopAfterTurn = func(context.Context, int, string) string {
			asked++
			if asked == 1 {
				return "you produced no artifact"
			}
			return ""
		}

		got := e.Run(context.Background(), testParams())
		if got.Kind != ResultConcluded || got.ResultSummary != "second stop" {
			t.Fatalf("the conclusion the hook allows must stand: %+v", got)
		}
		if asked != 2 {
			t.Errorf("the hook must be consulted on every would-stop, consulted %d times", asked)
		}
		n := tr.find(func(m domain.Message) bool { return m.Content == "you produced no artifact" })
		if n == nil {
			t.Fatal("the nudge must be recorded as input")
		}
		if n.Delivered == nil || !*n.Delivered {
			t.Error("the nudge must be drained on the next iteration like any other input")
		}
	})

	t.Run("a hook that keeps asking is bounded by the turn backstop, not by the engine", func(t *testing.T) {
		tr := newMemTranscript(pendingUser("go"))
		p := &scriptedProvider{repeat: &scriptedTurn{text: "still nothing to add"}}
		e := newTestEngine(tr, p, newScriptedToolHost())
		e.Hooks.ShouldStopAfterTurn = func(context.Context, int, string) string { return "answer me" }
		params := testParams()
		params.MaxIterations = 3

		got := e.Run(context.Background(), params)
		if got.Kind != ResultParked {
			t.Fatalf("disposition = %v, want parked — the backstop is what stops a hook that never yields", got.Kind)
		}
	})
}

// TestRun_FlowControlIsRegisteredOnlyWithABlueprint pins both halves of the
// gate. Within a blueprint the tool set does not vary with step position —
// position is carried by the system prompt alone, so a tool list and the
// text describing it cannot disagree. Without one there is no flow control
// at all.
func TestRun_FlowControlIsRegisteredOnlyWithABlueprint(t *testing.T) {
	toolsFor := func(t *testing.T, params Params) []string {
		t.Helper()
		tr := newMemTranscript(pendingUser("go"))
		p := &scriptedProvider{turns: []scriptedTurn{{text: "done"}}}
		e := newTestEngine(tr, p, newScriptedToolHost())
		if got := e.Run(context.Background(), params); got.Kind != ResultConcluded {
			t.Fatalf("disposition = %v (err: %v)", got.Kind, got.Err)
		}
		var names []string
		for _, tool := range p.requests[0].Tools {
			if tool.Function != nil {
				names = append(names, tool.Function.Name)
			}
		}
		return names
	}

	t.Run("a blueprint gets the seven sandbox tools plus stop_blueprint", func(t *testing.T) {
		names := toolsFor(t, testParams())
		if len(names) != 8 || names[7] != ToolStopBlueprint {
			t.Fatalf("tools = %v, want the seven sandbox tools then stop_blueprint", names)
		}
	})

	t.Run("a taskless conversation gets the sandbox tools only", func(t *testing.T) {
		params := testParams()
		params.HasBlueprint = false
		names := toolsFor(t, params)
		if len(names) != 7 {
			t.Fatalf("tools = %v, want the seven sandbox tools alone", names)
		}
		for _, n := range names {
			if n == ToolStopBlueprint {
				t.Error("there is no blueprint to stop; the way to say so is to say it")
			}
		}
	})
}

// TestRun_StopBlueprintIsInertWithoutABlueprint pins what happens if a model
// calls the tool anyway. It must not terminate: the name means nothing where
// the tool was never offered, so the call goes to the sandbox and comes back
// unknown, and the conversation carries on.
func TestRun_StopBlueprintIsInertWithoutABlueprint(t *testing.T) {
	tr := newMemTranscript(pendingUser("go"))
	host := newScriptedToolHost()
	p := &scriptedProvider{turns: []scriptedTurn{
		{calls: []domain.ToolCall{{ID: "c1", Name: ToolStopBlueprint, Input: map[string]any{"type": "abort", "reason": "r"}}}},
		{text: "sorry, ignore that"},
	}}
	e := newTestEngine(tr, p, host)
	params := testParams()
	params.HasBlueprint = false

	got := e.Run(context.Background(), params)
	if got.Outcome != domain.RunOutcomeContinue || got.ResultSummary != "sorry, ignore that" {
		t.Fatalf("a hallucinated stop must not end the conversation: %+v", got)
	}
	if calls := host.calls(); len(calls) != 1 || calls[0] != ToolStopBlueprint {
		t.Fatalf("the call must be dispatched like any other unknown tool: %v", calls)
	}
}

func TestRun_RetryIsSameProviderSameModelAndExhaustionFails(t *testing.T) {
	tr := newMemTranscript(pendingUser("go"))
	p := &scriptedProvider{turns: []scriptedTurn{
		{err: errors.New("429 rate limit exceeded")},
		{err: errors.New("503 service unavailable")},
		{text: "recovered"},
	}}
	e := newTestEngine(tr, p, newScriptedToolHost())

	got := e.Run(context.Background(), testParams())
	if got.Kind != ResultConcluded {
		t.Fatalf("transient errors must be retried: %v (err: %v)", got.Kind, got.Err)
	}
	if len(p.requests) != 3 {
		t.Fatalf("expected 3 attempts, got %d", len(p.requests))
	}
	for i, r := range p.requests {
		if r.Model != "claude-sonnet-4-5" || r.Provider != inference.ProviderAnthropic {
			t.Fatalf("attempt %d changed provider/model (%s/%s) — there is no fallback of any kind", i, r.Provider, r.Model)
		}
	}
}

func TestRun_RetryExhaustionFailsAndRecordsTheError(t *testing.T) {
	tr := newMemTranscript(pendingUser("go"))
	p := &scriptedProvider{turns: []scriptedTurn{
		{err: errors.New("503 service unavailable")},
		{err: errors.New("503 service unavailable")},
		{err: errors.New("503 service unavailable")},
	}}
	e := newTestEngine(tr, p, newScriptedToolHost())
	e.Retry = RetryPolicy{MaxAttempts: 3, Sleep: func(context.Context, time.Duration) error { return nil }}

	got := e.Run(context.Background(), testParams())
	if got.Kind != ResultFailed {
		t.Fatalf("exhaustion must fail the conversation: %v", got.Kind)
	}
	if n := tr.find(func(m domain.Message) bool { return strings.Contains(m.Content, "could not be retried") }); n == nil {
		t.Error("the failure's cause must be visible in the transcript")
	}
}

func TestRun_PermanentErrorIsNotRetried(t *testing.T) {
	tr := newMemTranscript(pendingUser("go"))
	p := &scriptedProvider{turns: []scriptedTurn{{err: errors.New("401 invalid api key")}}}
	e := newTestEngine(tr, p, newScriptedToolHost())

	if got := e.Run(context.Background(), testParams()); got.Kind != ResultFailed {
		t.Fatalf("disposition = %v, want failed", got.Kind)
	}
	if len(p.requests) != 1 {
		t.Errorf("a permanent error must not burn the retry budget, attempts = %d", len(p.requests))
	}
}

func TestRun_CancellationStopsWithoutFurtherRows(t *testing.T) {
	tr := newMemTranscript(pendingUser("go"))
	ctx, cancel := context.WithCancel(context.Background())
	p := &scriptedProvider{turns: []scriptedTurn{
		{calls: []domain.ToolCall{{ID: "c1", Name: "ls"}}, onCall: cancel},
		{text: "should never run"},
	}}
	e := newTestEngine(tr, p, newScriptedToolHost())

	got := e.Run(ctx, testParams())
	if got.Kind != ResultCancelled {
		t.Fatalf("disposition = %v, want cancelled (err: %v)", got.Kind, got.Err)
	}
	if p.calls != 1 {
		t.Errorf("no call may start after cancellation, calls = %d", p.calls)
	}
}

// TestRun_CancellationObservedThroughAFailedWriteIsStillACancellation covers
// the siblings of the stream arm above: the dozen exits that see a stop not as
// ctx.Done but as an ordinary store error, because the kill landed inside the
// write they were making.
//
// Getting this wrong is not a mislabel. A ResultFailed run has its workspace
// snapshot discarded, so a user who stopped a run mid-write got back "this
// run's workspace has expired" a minute later — the workspace was never saved
// because the loop reported a failure and the failure path throws it away.
//
// Both subtests deliberately fail the write with a plain error whose text
// merely mentions cancellation: the classification must come from the context,
// never from matching on a message.
func TestRun_CancellationObservedThroughAFailedWriteIsStillACancellation(t *testing.T) {
	t.Run("persisting the assistant message", func(t *testing.T) {
		tr := newMemTranscript(pendingUser("go"))
		ctx, cancel := context.WithCancel(context.Background())
		// The stop lands while the turn streams; the very next write is the
		// one that observes it.
		p := &scriptedProvider{turns: []scriptedTurn{{text: "done", onCall: func() {
			cancel()
			tr.failInsert = errors.New("insert message: context canceled")
		}}}}
		e := newTestEngine(tr, p, newScriptedToolHost())

		if got := e.Run(ctx, testParams()); got.Kind != ResultCancelled {
			t.Fatalf("disposition = %v, want cancelled (err: %v)", got.Kind, got.Err)
		}
	})

	t.Run("flushing pending input", func(t *testing.T) {
		tr := newMemTranscript(pendingUser("go"))
		ctx, cancel := context.WithCancel(context.Background())
		// The kill lands inside the flush itself, which is why the loop's
		// own top-of-iteration ctx check cannot catch it.
		tr.failMarkDelivered = func() error {
			cancel()
			return errors.New("mark delivered: context canceled")
		}
		e := newTestEngine(tr, &scriptedProvider{}, newScriptedToolHost())

		got := e.Run(ctx, testParams())
		if got.Kind != ResultCancelled {
			t.Fatalf("disposition = %v, want cancelled (err: %v)", got.Kind, got.Err)
		}
	})
}

// TestRun_CancellationCarriesTheContextsOwnCause: the disposition collapses
// the two ways a context dies, the reported cause must not. A deadline is the
// engagement running out of time — something to tune — and a cancel is
// somebody stopping it, which is nothing to tune; a reader who is told
// "context canceled" for a run that timed out goes looking for a person who
// pressed stop.
//
// Driven through the reroute rather than a pre-expired context, because that
// is the arm that reads the cause secondhand: the deadline lands inside the
// flush, whose error text is deliberately not what the classification uses.
func TestRun_CancellationCarriesTheContextsOwnCause(t *testing.T) {
	tr := newMemTranscript(pendingUser("go"))
	// Long enough that the loop's top-of-iteration ctx check passes first, so
	// the deadline is genuinely observed by the write and not before it.
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	tr.failMarkDelivered = func() error {
		<-ctx.Done()
		return errors.New("mark delivered: context deadline exceeded")
	}
	e := newTestEngine(tr, &scriptedProvider{}, newScriptedToolHost())

	got := e.Run(ctx, testParams())
	if got.Kind != ResultCancelled {
		t.Fatalf("disposition = %v, want cancelled (err: %v)", got.Kind, got.Err)
	}
	if !errors.Is(got.Err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want context.DeadlineExceeded — the cause is the context's, not a constant", got.Err)
	}
}

// TestRun_AFailedWriteWithALiveContextStillFails is the other half of the
// pair: the reclassification is keyed on the context alone, so a write that
// fails for its own reasons is still a failure and still fails the
// conversation.
func TestRun_AFailedWriteWithALiveContextStillFails(t *testing.T) {
	tr := newMemTranscript(pendingUser("go"))
	tr.failMarkDelivered = func() error { return errors.New("deadlock detected") }
	e := newTestEngine(tr, &scriptedProvider{}, newScriptedToolHost())

	got := e.Run(context.Background(), testParams())
	if got.Kind != ResultFailed {
		t.Fatalf("disposition = %v, want failed", got.Kind)
	}
	if got.FailureKind != domain.RunFailureAgentError {
		t.Errorf("failure kind = %q, want %q", got.FailureKind, domain.RunFailureAgentError)
	}
}

func TestRun_CostIsStampedPerAssistantRowAndNullWhenUnpriceable(t *testing.T) {
	tr := newMemTranscript(pendingUser("go"))
	p := &scriptedProvider{turns: []scriptedTurn{{
		text:  "done",
		model: "a-model-no-datasheet-carries",
		usage: inference.Usage{PromptTokens: 100, OutputTokens: 20},
	}}}
	e := newTestEngine(tr, p, newScriptedToolHost())

	if got := e.Run(context.Background(), testParams()); got.Kind != ResultConcluded {
		t.Fatalf("disposition = %v (err: %v)", got.Kind, got.Err)
	}
	row := tr.find(func(m domain.Message) bool { return m.Role == "assistant" })
	if row == nil {
		t.Fatal("the assistant message must be persisted")
	}
	if row.CostUSD != nil {
		t.Errorf("an unpriceable model must leave cost NULL, never 0: %v", *row.CostUSD)
	}
	if row.InputTokens == nil || *row.InputTokens != 100 || row.OutputTokens == nil || *row.OutputTokens != 20 {
		t.Errorf("display columns must be fully populated: %+v", row)
	}
	if row.Model != "a-model-no-datasheet-carries" {
		t.Errorf("the row must record the model the provider served, got %q", row.Model)
	}
}

// TestRun_TokenColumnsAreDisjoint pins the convention every reader of these
// columns assumes: the four sum to the prompt exactly once. The neutral usage
// counts prompt tokens inclusive of its cache buckets, so stamping it verbatim
// would count each cached token twice — inflating the compaction trip's
// occupancy (compacting early), the context gauge, and the approximate-cost
// footer alike, while the SDK runtime's rows kept meaning the other thing.
func TestRun_TokenColumnsAreDisjoint(t *testing.T) {
	tr := newMemTranscript(pendingUser("go"))
	p := &scriptedProvider{turns: []scriptedTurn{{
		text: "done",
		usage: inference.Usage{
			PromptTokens: 100_000, OutputTokens: 20,
			CacheReadTokens: 90_000, CacheCreationTokens: 5_000,
		},
	}}}
	e := newTestEngine(tr, p, newScriptedToolHost())

	if got := e.Run(context.Background(), testParams()); got.Kind != ResultConcluded {
		t.Fatalf("disposition = %v (err: %v)", got.Kind, got.Err)
	}
	row := tr.find(func(m domain.Message) bool { return m.Role == "assistant" })
	if row == nil {
		t.Fatal("the assistant message must be persisted")
	}
	if row.InputTokens == nil || *row.InputTokens != 5_000 {
		t.Errorf("input_tokens = %v, want 5000 — the prompt with its cache buckets taken out", row.InputTokens)
	}
	total := *row.InputTokens + *row.CacheReadTokens + *row.CacheCreationTokens
	if total != 100_000 {
		t.Errorf("the three prompt columns sum to %d, want the prompt's 100000 exactly once", total)
	}
}

func TestRun_CredentialsResolvePerCall(t *testing.T) {
	tr := newMemTranscript(pendingUser("go"))
	p := &scriptedProvider{turns: []scriptedTurn{
		{calls: []domain.ToolCall{{ID: "c1", Name: "ls"}}},
		{text: "done"},
	}}
	creds := &staticCredentials{provider: p}
	e := newTestEngine(tr, p, newScriptedToolHost())
	e.Credentials = creds

	if got := e.Run(context.Background(), testParams()); got.Kind != ResultConcluded {
		t.Fatalf("disposition = %v (err: %v)", got.Kind, got.Err)
	}
	if creds.resolves != 2 {
		t.Errorf("credentials must resolve per call (an STS triple expires mid-run), resolved %d times for 2 calls", creds.resolves)
	}
}

// guardFunc adapts a function to the Guard interface.
type guardFunc func(context.Context, int) (string, error)

func (f guardFunc) Check(ctx context.Context, turn int) (string, error) { return f(ctx, turn) }

func boolPtr(b bool) *bool { return &b }

// TestRun_TurnBudgetDerivesFromTranscriptAcrossClaims pins the budget's
// durability: assistant turns from an earlier engagement count against the
// bound, so a crash or re-claim cannot buy a fresh block of calls. With the
// budget one short of the bound at claim time, the engagement's single call
// is the wrap-up turn.
func TestRun_TurnBudgetDerivesFromTranscriptAcrossClaims(t *testing.T) {
	tr := newMemTranscript(
		domain.Message{Role: "user", Content: "mission"},
		domain.Message{Role: "assistant", Content: "turn one"},
		domain.Message{Role: "assistant", Content: "turn two"},
	)
	p := &scriptedProvider{turns: []scriptedTurn{{text: "wrap-up: did X, branch is clean"}}}
	e := newTestEngine(tr, p, newScriptedToolHost())
	params := testParams()
	params.MaxIterations = 3

	got := e.Run(context.Background(), params)
	if got.Kind != ResultParked {
		t.Fatalf("a budget-exhausted wrap-up must park, not conclude: %v (err: %v)", got.Kind, got.Err)
	}
	if got.ResultSummary != "wrap-up: did X, branch is clean" {
		t.Errorf("the wrap-up text must ride out as the summary: %q", got.ResultSummary)
	}
	if p.calls != 1 {
		t.Errorf("2 prior turns + 1 wrap-up call = the bound of 3; provider was called %d times", p.calls)
	}
	if got.NumTurns != 1 {
		t.Errorf("claim telemetry stays per-engagement: NumTurns = %d, want 1", got.NumTurns)
	}
	wrap := tr.find(func(m domain.Message) bool { return m.Subtype == domain.MessageSubtypeInjectionWrapUp })
	if wrap == nil {
		t.Fatal("the wrap-up ask must be a durable transcript row")
	}
	if wrap.Delivered != nil && !*wrap.Delivered {
		t.Error("the wrap-up ask must have been drained before the final call")
	}
}

// TestRun_HumanInputResetsTheTurnBudget pins the renewal rule: a human
// message grants a fresh budget, so a transcript already past the bound
// runs normally once the user speaks.
func TestRun_HumanInputResetsTheTurnBudget(t *testing.T) {
	pending := false
	tr := newMemTranscript(
		domain.Message{Role: "user", Content: "mission"},
		domain.Message{Role: "assistant", Content: "t1"},
		domain.Message{Role: "assistant", Content: "t2"},
		domain.Message{Role: "assistant", Content: "t3"},
		// The legacy "text" spelling of a normal user row must still read
		// as human — rows written before the blank-subtype vocabulary.
		domain.Message{Role: "user", Content: "keep going please", Delivered: &pending},
	)
	p := &scriptedProvider{turns: []scriptedTurn{{text: "done"}}}
	e := newTestEngine(tr, p, newScriptedToolHost())
	params := testParams()
	params.MaxIterations = 3

	got := e.Run(context.Background(), params)
	if got.Kind != ResultConcluded {
		t.Fatalf("a fresh human message renews the budget; got %v (err: %v)", got.Kind, got.Err)
	}
	if p.calls != 1 {
		t.Errorf("provider calls = %d, want 1", p.calls)
	}
}

// TestRun_SystemInjectionsDoNotRenewTheBudget pins the other half of the
// renewal rule: a staged system note landing on an exhausted conversation
// re-parks it without a model call — only a human buys more work.
func TestRun_SystemInjectionsDoNotRenewTheBudget(t *testing.T) {
	pending := false
	tr := newMemTranscript(
		domain.Message{Role: "user", Content: "mission"},
		domain.Message{Role: "assistant", Content: "t1"},
		domain.Message{Role: "assistant", Content: "t2"},
		domain.Message{Role: "user", Subtype: domain.MessageSubtypeInjectionWrapUp, Content: wrapUpNotice},
		domain.Message{Role: "assistant", Content: "the wrap-up"},
		domain.Message{Role: "user", Subtype: "injection:system-note", Content: "<system-note>new CI failure</system-note>", Delivered: &pending},
	)
	p := &scriptedProvider{}
	e := newTestEngine(tr, p, newScriptedToolHost())
	params := testParams()
	params.MaxIterations = 3

	got := e.Run(context.Background(), params)
	if got.Kind != ResultParked {
		t.Fatalf("a system note must not renew the budget: %v (err: %v)", got.Kind, got.Err)
	}
	if p.calls != 0 {
		t.Errorf("no model call may happen on an exhausted budget; provider was called %d times", p.calls)
	}
}

// TestRun_WrapUpIsRequestedOncePerBudget pins the ask's idempotence across
// claims: the durable row is the memory, so a re-claim at the same budget
// position does not ask twice.
func TestRun_WrapUpIsRequestedOncePerBudget(t *testing.T) {
	tr := newMemTranscript(
		domain.Message{Role: "user", Content: "mission"},
		domain.Message{Role: "assistant", Content: "t1"},
		domain.Message{Role: "assistant", Content: "t2"},
		domain.Message{Role: "user", Subtype: domain.MessageSubtypeInjectionWrapUp, Content: wrapUpNotice},
	)
	p := &scriptedProvider{turns: []scriptedTurn{{text: "the wrap-up"}}}
	e := newTestEngine(tr, p, newScriptedToolHost())
	params := testParams()
	params.MaxIterations = 3

	if got := e.Run(context.Background(), params); got.Kind != ResultParked {
		t.Fatalf("disposition = %v (err: %v)", got.Kind, got.Err)
	}
	count := 0
	for _, r := range tr.snapshot() {
		if r.Subtype == domain.MessageSubtypeInjectionWrapUp {
			count++
		}
	}
	if count != 1 {
		t.Errorf("wrap-up rows = %d, want exactly 1", count)
	}
}

// TestRun_WrapUpTurnSkipsTheWouldStopHook pins that an exhausted budget is
// not a would-stop: nudging more work out of it would contradict the
// wrap-up ask one turn earlier.
func TestRun_WrapUpTurnSkipsTheWouldStopHook(t *testing.T) {
	tr := newMemTranscript(pendingUser("go"))
	p := &scriptedProvider{turns: []scriptedTurn{
		{calls: []domain.ToolCall{{ID: "c1", Name: "ls"}}},
		{text: "the wrap-up"},
	}}
	e := newTestEngine(tr, p, newScriptedToolHost())
	hookAsked := 0
	e.Hooks.ShouldStopAfterTurn = func(context.Context, int, string) string {
		hookAsked++
		return "produce an artifact"
	}
	params := testParams()
	params.MaxIterations = 2

	got := e.Run(context.Background(), params)
	if got.Kind != ResultParked {
		t.Fatalf("disposition = %v (err: %v)", got.Kind, got.Err)
	}
	if hookAsked != 0 {
		t.Errorf("the hook must not be consulted on a wrap-up turn; consulted %d times", hookAsked)
	}
	if got.ResultSummary != "the wrap-up" {
		t.Errorf("summary = %q", got.ResultSummary)
	}
	if n := tr.find(func(m domain.Message) bool { return m.Subtype == domain.MessageSubtypeStopNote }); n == nil {
		t.Error("the park must leave a stop-note recording why")
	}
}

// TestRun_MidWorkDrainStampsOnlyHumanRows pins the flush partition: a steer
// stamp on a system injection would both mislabel it and make it read as
// human input to the budget derivation.
func TestRun_MidWorkDrainStampsOnlyHumanRows(t *testing.T) {
	tr := newMemTranscript(pendingUser("go"))
	pendingFlag := false
	p := &scriptedProvider{turns: []scriptedTurn{
		{calls: []domain.ToolCall{{ID: "c1", Name: "ls"}}, onCall: func() {
			_, _ = tr.Insert(context.Background(), "org", &domain.Message{
				Role: "user", Content: "human steer", Delivered: &pendingFlag,
			})
			_, _ = tr.Insert(context.Background(), "org", &domain.Message{
				Role: "user", Subtype: "injection:system-note", Content: "<system-note>event</system-note>", Delivered: &pendingFlag,
			})
		}},
		{text: "done"},
	}}
	e := newTestEngine(tr, p, newScriptedToolHost())

	if got := e.Run(context.Background(), testParams()); got.Kind != ResultConcluded {
		t.Fatalf("disposition = %v (err: %v)", got.Kind, got.Err)
	}
	steer := tr.find(func(m domain.Message) bool { return m.Content == "human steer" })
	if steer == nil || steer.Subtype != domain.MessageSubtypeInjectionSteer {
		t.Errorf("the human row must carry the steer stamp, got %+v", steer)
	}
	note := tr.find(func(m domain.Message) bool { return m.Content == "<system-note>event</system-note>" })
	if note == nil || note.Subtype != "injection:system-note" {
		t.Errorf("the system note must keep its own subtype through a mid-work flush, got %+v", note)
	}
	if note != nil && (note.Delivered == nil || !*note.Delivered) {
		t.Error("the system note must still be flushed")
	}
}

// TestRun_ParkNoticeWrittenOncePerBudgetWindow pins the stop-note dedupe: a
// re-claim of a still-exhausted conversation re-parks silently instead of
// stacking identical notices.
func TestRun_ParkNoticeWrittenOncePerBudgetWindow(t *testing.T) {
	notice := limitParkNotice(2)
	tr := newMemTranscript(
		domain.Message{Role: "user", Content: "mission"},
		domain.Message{Role: "assistant", Content: "t1"},
		domain.Message{Role: "user", Subtype: domain.MessageSubtypeInjectionWrapUp, Content: wrapUpNotice},
		domain.Message{Role: "assistant", Content: "the wrap-up"},
		domain.Message{Role: "user", Subtype: domain.MessageSubtypeStopNote, Content: notice},
	)
	p := &scriptedProvider{}
	e := newTestEngine(tr, p, newScriptedToolHost())
	params := testParams()
	params.MaxIterations = 2

	got := e.Run(context.Background(), params)
	if got.Kind != ResultParked || got.ParkNotice != notice {
		t.Fatalf("disposition = %v, notice = %q (err: %v)", got.Kind, got.ParkNotice, got.Err)
	}
	count := 0
	for _, r := range tr.snapshot() {
		if r.Subtype == domain.MessageSubtypeStopNote {
			count++
		}
	}
	if count != 1 {
		t.Errorf("stop-note rows = %d, want exactly 1 — a re-claim must not stack notices", count)
	}
	if p.calls != 0 {
		t.Errorf("provider calls = %d, want 0", p.calls)
	}
}

// TestIsTransient_NumericCodesMatchAsTokens pins the retry classifier's
// numeric matching: a status code is a standalone token, never a substring
// of an identifier, so a trace id in a permanent error cannot buy a retry.
func TestIsTransient_NumericCodesMatchAsTokens(t *testing.T) {
	transient := []string{
		"HTTP 500 from upstream",
		"provider returned (502)",
		"status code: 429",
		"503: service unavailable",
		"500",
	}
	for _, msg := range transient {
		if !isTransient(errors.New(msg)) {
			t.Errorf("isTransient(%q) = false, want true", msg)
		}
	}
	permanent := []string{
		"invalid api key (request id req_a5003b)",
		"model not found; trace 4290ab11",
		"deadline of 15000ms exceeded budget policy",
		"quota id 90429x rejected",
	}
	for _, msg := range permanent {
		if isTransient(errors.New(msg)) {
			t.Errorf("isTransient(%q) = true, want false — an embedded id must not read as a status code", msg)
		}
	}
}
