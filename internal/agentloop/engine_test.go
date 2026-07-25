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

	got := e.Run(context.Background(), testSpec())

	if got.Disposition != DispositionConcluded {
		t.Fatalf("disposition = %v, want concluded (err: %v)", got.Disposition, got.Err)
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
					ConversationID: "conv", Role: "user", Subtype: "text",
					Content: "also check the tests", Delivered: boolPtr(false),
				})
			},
		},
		{text: "done"},
	}}
	e := newTestEngine(tr, p, newScriptedToolHost())

	if got := e.Run(context.Background(), testSpec()); got.Disposition != DispositionConcluded {
		t.Fatalf("disposition = %v, want concluded (err: %v)", got.Disposition, got.Err)
	}

	opening := tr.find(func(m domain.Message) bool { return m.Content == "opening" })
	if opening == nil || opening.Subtype != "text" {
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
					ConversationID: "conv", Role: "user", Subtype: "text",
					Content: "one more thing", Delivered: boolPtr(false),
				})
			},
		},
		{text: "now really done"},
	}}
	e := newTestEngine(tr, p, newScriptedToolHost())

	got := e.Run(context.Background(), testSpec())
	if got.ResultSummary != "now really done" {
		t.Fatalf("the late-arriving input must keep the run going: %+v", got)
	}
	late := tr.find(func(m domain.Message) bool { return m.Content == "one more thing" })
	if late == nil || late.Subtype != "text" {
		t.Fatalf("a drain following a no-tool-call turn must deliver bare: %+v", late)
	}
}

func TestRun_RepairAnswersUnansweredToolCallsWithoutRedispatch(t *testing.T) {
	// A crash left an assistant message whose two tool calls were only half
	// answered — the exact partial-batch shape the repair pass diffs.
	tr := newMemTranscript(
		domain.Message{ConversationID: "conv", Role: "user", Subtype: "text", Content: "go"},
		domain.Message{ConversationID: "conv", Role: "assistant", Subtype: "tool_use", ToolCalls: []domain.ToolCall{
			{ID: "c1", Name: "bash"}, {ID: "c2", Name: "write"},
		}},
		domain.Message{ConversationID: "conv", Role: "tool", Subtype: "tool", ToolCallID: "c1", Content: "already recorded"},
	)
	host := newScriptedToolHost()
	p := &scriptedProvider{turns: []scriptedTurn{{text: "recovered"}}}
	e := newTestEngine(tr, p, host)

	if got := e.Run(context.Background(), testSpec()); got.Disposition != DispositionConcluded {
		t.Fatalf("disposition = %v, want concluded (err: %v)", got.Disposition, got.Err)
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

func TestRun_ExecutorChangedNoticeGatedOnPriorAssistantWork(t *testing.T) {
	t.Run("prior work queues the notice", func(t *testing.T) {
		tr := newMemTranscript(
			domain.Message{ConversationID: "conv", Role: "user", Subtype: "text", Content: "go"},
			domain.Message{ConversationID: "conv", Role: "assistant", Subtype: "text", Content: "working"},
		)
		p := &scriptedProvider{turns: []scriptedTurn{{text: "done"}}}
		e := newTestEngine(tr, p, newScriptedToolHost())
		if got := e.Run(context.Background(), testSpec()); got.Disposition != DispositionConcluded {
			t.Fatalf("disposition = %v (err: %v)", got.Disposition, got.Err)
		}
		notice := tr.find(func(m domain.Message) bool {
			return m.Subtype == domain.MessageSubtypeInjectionExecutorChanged
		})
		if notice == nil {
			t.Fatal("a conversation with prior assistant work must be told the workspace was restored")
		}
		if !strings.Contains(notice.Content, "restored from its last snapshot") {
			t.Errorf("the notice must describe the snapshot boundary: %q", notice.Content)
		}
	})

	t.Run("a re-claim before any work stays silent", func(t *testing.T) {
		tr := newMemTranscript(pendingUser("go"))
		p := &scriptedProvider{turns: []scriptedTurn{{text: "done"}}}
		e := newTestEngine(tr, p, newScriptedToolHost())
		if got := e.Run(context.Background(), testSpec()); got.Disposition != DispositionConcluded {
			t.Fatalf("disposition = %v (err: %v)", got.Disposition, got.Err)
		}
		if n := tr.find(func(m domain.Message) bool {
			return m.Subtype == domain.MessageSubtypeInjectionExecutorChanged
		}); n != nil {
			t.Fatalf("a credential-parking / requeue-before-start re-claim must stay silent: %+v", n)
		}
	})
}

func TestRun_RepairIsIdempotentAcrossClaims(t *testing.T) {
	tr := newMemTranscript(
		domain.Message{ConversationID: "conv", Role: "user", Subtype: "text", Content: "go"},
		domain.Message{ConversationID: "conv", Role: "assistant", Subtype: "tool_use", ToolCalls: []domain.ToolCall{{ID: "c1", Name: "bash"}}},
	)
	// Two engagements back to back, as a crash-then-reclaim produces.
	for i, text := range []string{"first", "second"} {
		p := &scriptedProvider{turns: []scriptedTurn{{text: text}}}
		e := newTestEngine(tr, p, newScriptedToolHost())
		if got := e.Run(context.Background(), testSpec()); got.Disposition != DispositionConcluded {
			t.Fatalf("engagement %d: disposition = %v (err: %v)", i, got.Disposition, got.Err)
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

	if got := e.Run(context.Background(), testSpec()); got.Disposition != DispositionConcluded {
		t.Fatalf("disposition = %v (err: %v)", got.Disposition, got.Err)
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

func TestRun_ToolDispatchIsSerialAndInCallOrder(t *testing.T) {
	tr := newMemTranscript(pendingUser("go"))
	host := newScriptedToolHost()
	p := &scriptedProvider{turns: []scriptedTurn{
		{calls: []domain.ToolCall{{ID: "c1", Name: "read"}, {ID: "c2", Name: "grep"}, {ID: "c3", Name: "bash"}}},
		{text: "done"},
	}}
	e := newTestEngine(tr, p, host)

	if got := e.Run(context.Background(), testSpec()); got.Disposition != DispositionConcluded {
		t.Fatalf("disposition = %v (err: %v)", got.Disposition, got.Err)
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
		got := e.Run(context.Background(), testSpec())
		if got.Disposition != DispositionConcluded {
			t.Fatalf("a survivable protocol error must not end the run: %v (err: %v)", got.Disposition, got.Err)
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
		got := e.Run(context.Background(), testSpec())
		if got.Disposition != DispositionFailed {
			t.Fatalf("a fatal protocol error must fail the claim: %v", got.Disposition)
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

	if got := e.Run(context.Background(), testSpec()); got.Disposition != DispositionConcluded {
		t.Fatalf("a gate denial must not end the run: %v (err: %v)", got.Disposition, got.Err)
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
	t.Run("stop_run alone terminates and takes the turn's prose as its summary", func(t *testing.T) {
		tr := newMemTranscript(pendingUser("go"))
		p := &scriptedProvider{turns: []scriptedTurn{{
			text:  "Nothing to review here — the PR only touches generated files.",
			calls: []domain.ToolCall{{ID: "c1", Name: ToolStopRun, Input: map[string]any{"type": "finish", "reason": "no reviewable changes"}}},
		}}}
		e := newTestEngine(tr, p, newScriptedToolHost())
		got := e.Run(context.Background(), testSpec())
		if got.Outcome != domain.RunOutcomeFinish || got.OutcomeReason != "no reviewable changes" {
			t.Fatalf("finish must reach the outcome unchanged: %+v", got)
		}
		if got.ResultSummary != "Nothing to review here — the PR only touches generated files." {
			t.Errorf("result summary = %q, want the accompanying prose — stop_run carries no summary of its own", got.ResultSummary)
		}
	})

	t.Run("stop_run paired with work does not terminate", func(t *testing.T) {
		tr := newMemTranscript(pendingUser("go"))
		host := newScriptedToolHost()
		p := &scriptedProvider{turns: []scriptedTurn{
			{calls: []domain.ToolCall{
				{ID: "c1", Name: "write"},
				{ID: "c2", Name: ToolStopRun, Input: map[string]any{"type": "abort", "reason": "r"}},
			}},
			{text: "actually finished"},
		}}
		e := newTestEngine(tr, p, host)
		got := e.Run(context.Background(), testSpec())
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
			{calls: []domain.ToolCall{{ID: "c1", Name: ToolStopRun, Input: map[string]any{"type": "abort"}}}},
			{calls: []domain.ToolCall{{ID: "c2", Name: ToolStopRun, Input: map[string]any{"type": "abort", "reason": "the branch is gone"}}}},
		}}
		e := newTestEngine(tr, p, newScriptedToolHost())
		got := e.Run(context.Background(), testSpec())
		if got.Outcome != domain.RunOutcomeAbort || got.OutcomeReason != "the branch is gone" {
			t.Fatalf("abort must carry its reason: %+v", got)
		}
		first := tr.toolResults()[0]
		if !first.IsError || !strings.Contains(first.Content, "requires a reason") {
			t.Fatalf("a reasonless stop must be told what it owes: %+v", first)
		}
	})

	t.Run("a stop typed continue is refused and the run keeps going", func(t *testing.T) {
		tr := newMemTranscript(pendingUser("go"))
		p := &scriptedProvider{turns: []scriptedTurn{
			{calls: []domain.ToolCall{{ID: "c1", Name: ToolStopRun, Input: map[string]any{"type": "continue", "reason": "did my part"}}}},
			{text: "handing off"},
		}}
		e := newTestEngine(tr, p, newScriptedToolHost())
		got := e.Run(context.Background(), testSpec())
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
	spec := testSpec()
	spec.MaxIterations = 2

	got := e.Run(context.Background(), spec)
	if got.Disposition != DispositionParked {
		t.Fatalf("the backstop must park, not fail: %v (err: %v)", got.Disposition, got.Err)
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

	got := e.Run(context.Background(), testSpec())
	if got.Disposition != DispositionParked {
		t.Fatalf("a spend breach must park, never fail: %v", got.Disposition)
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

	if got := e.Run(context.Background(), testSpec()); got.Disposition != DispositionConcluded {
		t.Fatalf("a guard that cannot read its inputs must not wedge the run: %v", got.Disposition)
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

		got := e.Run(context.Background(), testSpec())
		if got.Disposition != DispositionConcluded || got.ResultSummary != "second stop" {
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
		spec := testSpec()
		spec.MaxIterations = 3

		got := e.Run(context.Background(), spec)
		if got.Disposition != DispositionParked {
			t.Fatalf("disposition = %v, want parked — the backstop is what stops a hook that never yields", got.Disposition)
		}
	})
}

// TestRun_ToolSetDoesNotVaryWithStepPosition pins that flow control is
// registered identically everywhere. Position is carried by the system
// prompt alone, so a tool list and the text describing it cannot disagree.
func TestRun_ToolSetDoesNotVaryWithStepPosition(t *testing.T) {
	tr := newMemTranscript(pendingUser("go"))
	p := &scriptedProvider{turns: []scriptedTurn{{text: "done"}}}
	e := newTestEngine(tr, p, newScriptedToolHost())

	if got := e.Run(context.Background(), testSpec()); got.Disposition != DispositionConcluded {
		t.Fatalf("disposition = %v (err: %v)", got.Disposition, got.Err)
	}
	var flow []string
	for _, tool := range p.requests[0].Tools {
		if tool.Function != nil && tool.Function.Name == ToolStopRun {
			flow = append(flow, tool.Function.Name)
		}
	}
	if len(flow) != 1 {
		t.Fatalf("flow-control tools = %v, want exactly stop_run", flow)
	}
	// The seven in-jail tools are always registered alongside.
	if len(p.requests[0].Tools) != 8 {
		t.Errorf("tool count = %d, want the seven sandbox tools plus stop_run", len(p.requests[0].Tools))
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

	got := e.Run(context.Background(), testSpec())
	if got.Disposition != DispositionConcluded {
		t.Fatalf("transient errors must be retried: %v (err: %v)", got.Disposition, got.Err)
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

	got := e.Run(context.Background(), testSpec())
	if got.Disposition != DispositionFailed {
		t.Fatalf("exhaustion must fail the conversation: %v", got.Disposition)
	}
	if n := tr.find(func(m domain.Message) bool { return strings.Contains(m.Content, "could not be retried") }); n == nil {
		t.Error("the failure's cause must be visible in the transcript")
	}
}

func TestRun_PermanentErrorIsNotRetried(t *testing.T) {
	tr := newMemTranscript(pendingUser("go"))
	p := &scriptedProvider{turns: []scriptedTurn{{err: errors.New("401 invalid api key")}}}
	e := newTestEngine(tr, p, newScriptedToolHost())

	if got := e.Run(context.Background(), testSpec()); got.Disposition != DispositionFailed {
		t.Fatalf("disposition = %v, want failed", got.Disposition)
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

	got := e.Run(ctx, testSpec())
	if got.Disposition != DispositionCancelled {
		t.Fatalf("disposition = %v, want cancelled (err: %v)", got.Disposition, got.Err)
	}
	if p.calls != 1 {
		t.Errorf("no call may start after cancellation, calls = %d", p.calls)
	}
}

func TestRun_CostIsStampedPerAssistantRowAndNullWhenUnpriceable(t *testing.T) {
	tr := newMemTranscript(pendingUser("go"))
	p := &scriptedProvider{turns: []scriptedTurn{{
		text:  "done",
		model: "a-model-no-datasheet-carries",
		usage: inference.Usage{InputTokens: 100, OutputTokens: 20},
	}}}
	e := newTestEngine(tr, p, newScriptedToolHost())

	if got := e.Run(context.Background(), testSpec()); got.Disposition != DispositionConcluded {
		t.Fatalf("disposition = %v (err: %v)", got.Disposition, got.Err)
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

func TestRun_CredentialsResolvePerCall(t *testing.T) {
	tr := newMemTranscript(pendingUser("go"))
	p := &scriptedProvider{turns: []scriptedTurn{
		{calls: []domain.ToolCall{{ID: "c1", Name: "ls"}}},
		{text: "done"},
	}}
	creds := &staticCredentials{provider: p}
	e := newTestEngine(tr, p, newScriptedToolHost())
	e.Credentials = creds

	if got := e.Run(context.Background(), testSpec()); got.Disposition != DispositionConcluded {
		t.Fatalf("disposition = %v (err: %v)", got.Disposition, got.Err)
	}
	if creds.resolves != 2 {
		t.Errorf("credentials must resolve per call (an STS triple expires mid-run), resolved %d times for 2 calls", creds.resolves)
	}
}

// guardFunc adapts a function to the Guard interface.
type guardFunc func(context.Context, int) (string, error)

func (f guardFunc) Check(ctx context.Context, turn int) (string, error) { return f(ctx, turn) }

func boolPtr(b bool) *bool { return &b }
