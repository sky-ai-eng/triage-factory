package agentloop

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/inference"
)

// The turn-end allowlist battery. Every case drives the engine end to end
// through the scripted harness and asserts the disposition plus the rows it
// left, because the defect this replaced was invisible in the disposition
// alone: a run that "completed" having produced nothing.

// the raw literal both Anthropic and Bedrock send.
const wallStop = "model_context_window_exceeded"

// TestClassifyStop pins the taxonomy against the strings that actually arrive.
// The window wall matters most: bifrost's Anthropic map covers five of its own
// eight stop-reason constants and passes the rest through verbatim, so the raw
// literal is what a run sees today — and a pin refresh that starts normalizing
// it must not silently reclassify it into a different arm.
func TestClassifyStop(t *testing.T) {
	cases := []struct {
		reason string
		want   stopClass
	}{
		{"stop", stopEndTurn},                       // bifrost: end_turn and stop_sequence both
		{"", stopEndTurn},                           // stream reported none
		{"tool_calls", stopToolCalls},               // bifrost: tool_use
		{"length", stopLength},                      // bifrost: max_tokens
		{"max_tokens", stopLength},                  // raw, if a provider map ever misses it
		{wallStop, stopWindowWall},                  // raw Anthropic + Bedrock passthrough
		{"context_window_exceeded", stopWindowWall}, // a future normalized spelling
		{"refusal", stopDecline},                    // raw Anthropic passthrough
		{"content_filter", stopDecline},             // bifrost: bedrock content_filtered
		{"guardrail_intervened", stopDecline},       // raw Bedrock passthrough
		{"pause_turn", stopUnknown},                 // raw; unreachable, TF uses no server tools
		{"compaction", stopUnknown},                 // mapped by bifrost, never requested by TF
		{"something_nobody_has_shipped", stopUnknown},
	}
	for _, tc := range cases {
		if got := classifyStop(tc.reason); got != tc.want {
			t.Errorf("classifyStop(%q) = %v, want %v", tc.reason, got, tc.want)
		}
	}

	// The two derived predicates read off the same table, so a reason added
	// to one arm can never disagree with them.
	if !isLengthStop("max_tokens") || isLengthStop(wallStop) {
		t.Error("isLengthStop must cover the length arm and nothing else")
	}
	for _, cut := range []string{"length", "max_tokens", wallStop, "context_window_exceeded"} {
		if !isCutStop(cut) {
			t.Errorf("isCutStop(%q) = false; both cuts damage a message the same way", cut)
		}
	}
	for _, whole := range []string{"stop", "", "tool_calls", "refusal"} {
		if isCutStop(whole) {
			t.Errorf("isCutStop(%q) = true; nothing cut this message short", whole)
		}
	}
}

// TestRun_EndTurnStopStillConcludesExactlyAsBefore is the behavior-preserving
// half of the inversion: the healthy path is untouched. A no-tool-call reply
// under an ordinary stop concludes with its text as the summary, and a
// tool_calls stop dispatches.
func TestRun_EndTurnStopStillConcludesExactlyAsBefore(t *testing.T) {
	tr := newMemTranscript(pendingUser("go"))
	host := newScriptedToolHost()
	p := &scriptedProvider{turns: []scriptedTurn{
		{calls: []domain.ToolCall{{ID: "c1", Name: "bash"}}, finish: "tool_calls"},
		{text: "All done.", finish: "stop"},
	}}
	e := newTestEngine(tr, p, host)

	got := e.Run(context.Background(), testParams())

	if got.Kind != ResultConcluded {
		t.Fatalf("disposition = %v (err: %v), want concluded", got.Kind, got.Err)
	}
	if got.Outcome != domain.RunOutcomeContinue || got.ResultSummary != "All done." {
		t.Errorf("result = %+v, want outcome continue with the final text as the summary", got)
	}
	if calls := host.calls(); len(calls) != 1 || calls[0] != "bash" {
		t.Errorf("tool host saw %v, want the one dispatched call", calls)
	}
}

// TestRun_WindowWallCompactsAndNeverConcludes covers the wall in all three
// shapes it can arrive in. In each: the turn is not a conclusion, no tool call
// from it runs, and the conversation compacts and carries on — the wall is the
// window being genuinely full, which is the reactive arm's event arriving as a
// 200 instead of a rejection.
func TestRun_WindowWallCompactsAndNeverConcludes(t *testing.T) {
	// A prior turn old enough that its cache is dead, so the fixtures whose
	// cut row is unreplayable (and therefore invisible to the warmth read)
	// resolve to the forced-shape path deterministically.
	old := time.Now().UTC().Add(-time.Hour)

	t.Run("cut mid-sentence", func(t *testing.T) {
		tr := newMemTranscript(
			domain.Message{Role: "user", Content: "the mission"},
			pendingUser("review it"),
		)
		host := newScriptedToolHost()
		p := &scriptedProvider{turns: []scriptedTurn{
			{text: "I read the diff and the first problem is", finish: wallStop},
			summaryReply("what happened before the window filled"),
			{text: "Posted the review."},
		}}
		e := newTestEngine(tr, p, host)

		got := e.Run(context.Background(), testParams())

		if got.Kind != ResultConcluded || got.ResultSummary != "Posted the review." {
			t.Fatalf("result = %+v, want the run to compact and conclude on real work", got)
		}
		cut := tr.find(func(m domain.Message) bool { return strings.HasPrefix(m.Content, "I read the diff") })
		if cut == nil {
			t.Fatal("the wall-cut turn must persist — its tokens and dollars are real")
		}
		if reason, _ := cut.Metadata["finish_reason"].(string); reason != wallStop {
			t.Errorf("finish_reason = %q, want %q — a wall hit and a cap hit must be distinguishable", reason, wallStop)
		}
		if cut.WindowState != domain.MessageWindowInactive {
			t.Errorf("window_state = %q, want inactive: the compaction commit supersedes the cut turn "+
				"along with the rest of the span", cut.WindowState)
		}
		if tr.find(func(m domain.Message) bool {
			return m.Subtype == domain.MessageSubtypeInjectionCompactionResult
		}) == nil {
			t.Fatal("a wall stop must trigger compaction; no result row was written")
		}
		if tr.find(func(m domain.Message) bool {
			return m.Subtype == domain.MessageSubtypeInjectionOutputLimit
		}) != nil {
			t.Error("the wall is not the output cap: escalating the budget makes the wall arrive sooner, " +
				"so the length arm's notice must not fire")
		}
	})

	t.Run("cut mid-arguments", func(t *testing.T) {
		tr := newMemTranscript(
			domain.Message{Role: "user", Content: "the mission"},
			pendingUser("go"),
		)
		host := newScriptedToolHost()
		p := &scriptedProvider{turns: []scriptedTurn{
			{
				calls: []domain.ToolCall{{ID: "c1", Name: "write"}, {ID: "c2", Name: "edit"}},
				// The tail of the second call's JSON never arrived.
				rawArgs: map[int]string{1: `{"path":"internal/ag`},
				finish:  wallStop,
			},
			summaryReply("what happened"),
			{text: "done"},
		}}
		e := newTestEngine(tr, p, host)

		got := e.Run(context.Background(), testParams())

		if got.Kind != ResultConcluded {
			t.Fatalf("disposition = %v (err: %v), want concluded", got.Kind, got.Err)
		}
		if calls := host.calls(); len(calls) != 0 {
			t.Fatalf("a wall-cut tool call must never execute; the host saw %v", calls)
		}
		results := tr.toolResults()
		if len(results) != 2 {
			t.Fatalf("tool results = %d, want every call in the cut message answered", len(results))
		}
		for _, r := range results {
			if !r.IsError {
				t.Errorf("result for %s must be is_error", r.ToolCallID)
			}
			if !strings.Contains(r.Content, "context window") {
				t.Errorf("the result must name the cause it actually had: %q", r.Content)
			}
		}
		if tr.find(func(m domain.Message) bool {
			return m.Subtype == domain.MessageSubtypeInjectionCompactionResult
		}) == nil {
			t.Fatal("a wall stop must trigger compaction; no result row was written")
		}

		// The reason the answers are written before the compaction rather
		// than after: the summarize call reads the cut message, and an
		// unanswered tool_use is rejected on the wire.
		summarize := p.requests[1]
		answered := map[string]bool{}
		for _, row := range summarize.Rows {
			if row.Role == "tool" {
				answered[row.ToolCallID] = true
			}
		}
		for _, row := range summarize.Rows {
			for _, call := range row.ToolCalls {
				if !answered[call.ID] {
					t.Errorf("the summarize call assembled tool_use %q with no result — an illegal transcript", call.ID)
				}
			}
		}
	})

	t.Run("cut before anything came out", func(t *testing.T) {
		tr := newMemTranscript(
			domain.Message{Role: "user", Content: "the mission", CreatedAt: old},
			usedAssistant("prior work", 1000, old),
			pendingUser("go"),
		)
		p := &scriptedProvider{turns: []scriptedTurn{
			{reasoning: strings.Repeat("deliberating. ", 200), finish: wallStop},
			forcedReply("analysis", "what happened"),
			{text: "done"},
		}}
		e := newTestEngine(tr, p, newScriptedToolHost())

		got := e.Run(context.Background(), testParams())

		if got.Kind != ResultConcluded || got.ResultSummary != "done" {
			t.Fatalf("result = %+v, want the run to compact and carry on", got)
		}
		cut := tr.find(func(m domain.Message) bool { return m.Role == "assistant" && len(m.Reasoning) > 0 })
		if cut == nil {
			t.Fatal("the wall-cut turn must persist")
		}
		if cut.WindowState != domain.MessageWindowInactive {
			t.Errorf("window_state = %q, want inactive: an assistant row whose only content is thinking "+
				"replays as an empty message, and the summarize call would be the first to read it", cut.WindowState)
		}
		if tr.find(func(m domain.Message) bool {
			return m.Subtype == domain.MessageSubtypeInjectionCompactionResult
		}) == nil {
			t.Fatal("a wall stop must trigger compaction; no result row was written")
		}
	})
}

// TestRun_SecondWindowWallFailsLegibly: compaction already ran, and the very
// next call hit the wall again — so there is no smaller history to retry
// against. Fail with the reason in the transcript rather than compacting in a
// circle.
func TestRun_SecondWindowWallFailsLegibly(t *testing.T) {
	tr := newMemTranscript(
		domain.Message{Role: "user", Content: "the mission"},
		pendingUser("go"),
	)
	p := &scriptedProvider{turns: []scriptedTurn{
		{text: "starting", finish: wallStop},
		summaryReply("what happened"),
		{text: "starting again", finish: wallStop},
	}}
	e := newTestEngine(tr, p, newScriptedToolHost())

	got := e.Run(context.Background(), testParams())

	if got.Kind != ResultFailed {
		t.Fatalf("disposition = %v, want failed: %+v", got.Kind, got)
	}
	if got.Outcome != "" {
		t.Errorf("outcome = %q — a wall stop must never resolve to a terminal outcome", got.Outcome)
	}
	if len(p.requests) != 3 {
		t.Fatalf("provider calls = %d, want cut + summarize + cut, and no fourth", len(p.requests))
	}
	compactions := 0
	for _, r := range tr.snapshot() {
		if r.Subtype == domain.MessageSubtypeInjectionCompactionResult {
			compactions++
		}
	}
	if compactions != 1 {
		t.Errorf("compaction results = %d, want exactly one — the second wall fails instead of compacting again", compactions)
	}
	if tr.find(func(m domain.Message) bool {
		return m.Subtype == domain.MessageSubtypeStopNote && m.Content == windowWallFailureNotice
	}) == nil {
		t.Fatal("the transcript must record why the run stopped")
	}
}

// TestRun_DeclineParksForAHuman: a refusal, a content filter and a guardrail
// are the same disposition — park `open` with a notice naming the reason, no
// automatic retry, because an identical request is declined identically and
// only a person can change the ask.
func TestRun_DeclineParksForAHuman(t *testing.T) {
	cases := []struct {
		reason string
		says   string
	}{
		{"refusal", "the model declined to produce it"},
		{"content_filter", "a content filter blocked it"},
		{"guardrail_intervened", "a provider guardrail blocked it"},
	}
	for _, tc := range cases {
		t.Run(tc.reason, func(t *testing.T) {
			tr := newMemTranscript(pendingUser("go"))
			p := &scriptedProvider{turns: []scriptedTurn{{text: "I can't help with that.", finish: tc.reason}}}
			e := newTestEngine(tr, p, newScriptedToolHost())

			got := e.Run(context.Background(), testParams())

			if got.Kind != ResultParked {
				t.Fatalf("disposition = %v (err: %v), want parked", got.Kind, got.Err)
			}
			if len(p.requests) != 1 {
				t.Errorf("provider calls = %d, want exactly one — a refusal is not retried", len(p.requests))
			}
			notice := tr.find(func(m domain.Message) bool { return m.Subtype == domain.MessageSubtypeStopNote })
			if notice == nil {
				t.Fatal("the transcript must record why the run paused")
			}
			if !strings.Contains(notice.Content, tc.says) || !strings.Contains(notice.Content, tc.reason) {
				t.Errorf("notice = %q, want it to name the stop reason in plain language and verbatim", notice.Content)
			}
			if got.ParkNotice != notice.Content {
				t.Errorf("ParkNotice = %q, want the row the loop wrote", got.ParkNotice)
			}
		})
	}
}

// TestRun_DeclineParkIsWokenByAFollowUp: the park is the ordinary
// needs-driving park, so the next user message picks the conversation back up.
//
// The wordless subtest is the one that can silently break the promise. A
// decline that returns no content leaves an assistant row with nothing on it,
// and an empty assistant message is rejected on the wire — so if that row
// stayed in the window, the follow-up call that is supposed to wake the
// conversation would be the call that bricks it.
func TestRun_DeclineParkIsWokenByAFollowUp(t *testing.T) {
	cases := map[string]scriptedTurn{
		"with a spoken refusal": {text: "I can't help with that.", finish: "refusal"},
		"with nothing at all":   {finish: "refusal"},
	}
	for name, declined := range cases {
		t.Run(name, func(t *testing.T) {
			tr := newMemTranscript(pendingUser("go"))
			p := &scriptedProvider{turns: []scriptedTurn{declined, {text: "Done."}}}
			e := newTestEngine(tr, p, newScriptedToolHost())

			if got := e.Run(context.Background(), testParams()); got.Kind != ResultParked {
				t.Fatalf("first engagement = %v, want parked", got.Kind)
			}

			follow := pendingUser("try it a different way")
			if _, err := tr.Insert(context.Background(), "org", &follow); err != nil {
				t.Fatalf("insert follow-up: %v", err)
			}

			got := e.Run(context.Background(), testParams())
			if got.Kind != ResultConcluded || got.ResultSummary != "Done." {
				t.Fatalf("second engagement = %+v, want the follow-up to wake the conversation", got)
			}
			for _, row := range p.requests[1].Rows {
				if row.Role == "assistant" && unreplayableTurn(row) {
					t.Errorf("the waking call assembled an assistant row with nothing on it (id %d); "+
						"the provider rejects that message", row.ID)
				}
			}
		})
	}
}

// TestRun_UnrecognizedStopParksNamingTheRawString: never conclude on a reason
// nobody has classified, and never spin on one either. The raw string goes in
// the notice because it is the only thing that tells the next person what
// arrived.
func TestRun_UnrecognizedStopParksNamingTheRawString(t *testing.T) {
	// pause_turn and compaction are real reasons TF cannot reach today; the
	// third stands in for whatever a provider ships next.
	for _, reason := range []string{"pause_turn", "compaction", "supernova"} {
		t.Run(reason, func(t *testing.T) {
			tr := newMemTranscript(pendingUser("go"))
			p := &scriptedProvider{turns: []scriptedTurn{{text: "partial", finish: reason}}}
			log := &recordingLogger{}
			e := newTestEngine(tr, p, newScriptedToolHost())
			e.Log = log

			got := e.Run(context.Background(), testParams())

			if got.Kind != ResultParked {
				t.Fatalf("disposition = %v (err: %v), want parked", got.Kind, got.Err)
			}
			if len(p.requests) != 1 {
				t.Errorf("provider calls = %d, want exactly one — an unknown stop must not spin", len(p.requests))
			}
			notice := tr.find(func(m domain.Message) bool { return m.Subtype == domain.MessageSubtypeStopNote })
			if notice == nil || !strings.Contains(notice.Content, reason) {
				t.Fatalf("notice = %+v, want it to name %q verbatim", notice, reason)
			}
			if !log.warned("unrecognized finish reason") {
				t.Errorf("an unclassified stop reason must be logged; warns = %v", log.warns)
			}
		})
	}
}

// TestRun_ToolCallsStopWithNoCallsParks: the provider contradicted itself. It
// has nothing to dispatch and it is not a finished turn either, so it takes
// the same park as any other stop the loop will not act on.
func TestRun_ToolCallsStopWithNoCallsParks(t *testing.T) {
	tr := newMemTranscript(pendingUser("go"))
	p := &scriptedProvider{turns: []scriptedTurn{{text: "about to call something", finish: "tool_calls"}}}
	host := newScriptedToolHost()
	e := newTestEngine(tr, p, host)

	got := e.Run(context.Background(), testParams())

	if got.Kind != ResultParked {
		t.Fatalf("disposition = %v (err: %v), want parked", got.Kind, got.Err)
	}
	if len(host.calls()) != 0 {
		t.Errorf("nothing was asked for; the host saw %v", host.calls())
	}
	if tr.find(func(m domain.Message) bool {
		return m.Subtype == domain.MessageSubtypeStopNote && m.Content == emptyToolBatchParkNotice
	}) == nil {
		t.Fatal("the transcript must record why the run paused")
	}
}

// TestRun_EmptyFinishReasonConcludesWithAWarning: a stream that never reported
// a reason is read as an ordinary stop — the conservative choice, and what
// every reason got before the allowlist existed. The warn is what makes the
// real distribution observable, since the persisted metadata deliberately
// stores nothing for an absent reason.
func TestRun_EmptyFinishReasonConcludesWithAWarning(t *testing.T) {
	tr := newMemTranscript(pendingUser("go"))
	p := &scriptedProvider{turns: []scriptedTurn{{text: "Finished.", noFinishReason: true}}}
	log := &recordingLogger{}
	e := newTestEngine(tr, p, newScriptedToolHost())
	e.Log = log

	got := e.Run(context.Background(), testParams())

	if got.Kind != ResultConcluded || got.ResultSummary != "Finished." {
		t.Fatalf("result = %+v, want the turn read as an ordinary conclusion", got)
	}
	if !log.warned("no finish reason") {
		t.Errorf("the anomaly must be logged; warns = %v", log.warns)
	}
	row := tr.find(func(m domain.Message) bool { return m.Content == "Finished." })
	if row == nil {
		t.Fatal("the concluding turn must persist")
	}
	if _, present := row.Metadata["finish_reason"]; present {
		t.Errorf("metadata = %v, want no finish_reason key: \"not reported\" and \"unknown\" are the same thing "+
			"and storing \"\" would make them look different", row.Metadata)
	}
}

// TestWindowWallIsReachableWhileCompactionIsHealthy is the reachability
// fixture. The wall is not a pathology of a broken compactor — it sits in the
// band between the proactive trip and the window, which any Haiku-class
// context can occupy: 0.80 of a 200k window is 160k, and 160k plus the model's
// own 64k output cap overdraws the window by 24k. Every occupancy in that band
// leaves compaction correctly idle and the wall genuinely reachable.
func TestWindowWallIsReachableWhileCompactionIsHealthy(t *testing.T) {
	const model = "claude-haiku-4-5"
	window, ok := inference.ModelWindow(model)
	if !ok {
		t.Fatalf("%s is missing from the pricing snapshot", model)
	}
	cap := inference.MaxOutputTokens(inference.ProviderAnthropic, model)

	params := testParams()
	params.Model = model
	// Inside the gap: under the trip point, over the wall.
	occupied := int(DefaultCompactionThreshold*float64(window)) - 10000

	e := &Engine{}
	if e.compactionDue(params, []domain.Message{usedAssistant("work", occupied, time.Now().UTC())}) {
		t.Fatalf("compaction tripped at %d of %d — the fixture must sit below the trip", occupied, window)
	}
	if occupied+cap <= window {
		t.Fatalf("input %d + cap %d does not overdraw the %d window; the wall is unreachable in this fixture "+
			"and the arm it exercises would be dead code", occupied, cap, window)
	}
}
