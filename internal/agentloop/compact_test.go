package agentloop

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/inference"
)

// The compaction battery. The fixtures use claude-sonnet-4-5 (window 200000,
// threshold 0.80 → trip at 160000) and drive the engine end to end through
// the scripted harness, so every property is asserted as an observable row
// sequence: what flipped, what survived, what ordered where, what the
// provider was actually sent.

const overThreshold = 165000

// usedAssistant is a prior turn carrying the stamped usage the occupancy
// derivation reads. at controls warmth: recent = cache live, old = cold.
func usedAssistant(content string, tokens int, at time.Time) domain.Message {
	in := tokens
	return domain.Message{Role: "assistant", Content: content, InputTokens: &in, CreatedAt: at}
}

func summaryReply(summary string) scriptedTurn {
	return scriptedTurn{text: "<analysis>\nthinking it through\n</analysis>\n\n<summary>\n" + summary + "\n</summary>"}
}

func forcedReply(analysis, summary string) scriptedTurn {
	return scriptedTurn{calls: []domain.ToolCall{{
		ID: "forced-1", Name: compactionToolName,
		Input: map[string]any{"analysis": analysis, "summary": summary},
	}}}
}

// rowWithContent finds the delivered row carrying content in the
// request's row set, or nil.
func rowWithContent(rows []domain.Message, content string) *domain.Message {
	for i := range rows {
		if strings.Contains(rows[i].Content, content) {
			return &rows[i]
		}
	}
	return nil
}

// TestWarmCompaction_TripsCommitsAndOrdersQueue drives the whole warm path:
// the trip fires before the drain, the summarize call excludes queued input,
// the commit leaves [pinned opening, summary] as the window, and the queued
// row is delivered after the summary in both assembly and display order.
func TestWarmCompaction_TripsCommitsAndOrdersQueue(t *testing.T) {
	tr := newMemTranscript(
		domain.Message{Role: "user", Content: "the mission"},
		usedAssistant("worked a lot", overThreshold, time.Now().UTC()),
		pendingUser("queued while it filled"),
	)
	provider := &scriptedProvider{turns: []scriptedTurn{
		summaryReply("everything that happened"),
		{text: "done"},
	}}
	engine := newTestEngine(tr, provider, newScriptedToolHost())

	result := engine.Run(context.Background(), testParams())
	if result.Kind != ResultConcluded {
		t.Fatalf("result = %+v, want concluded", result)
	}

	// The summarize call: its last row is the request, and the queued human
	// row rode along only as an undelivered row — excluded from assembly.
	if len(provider.requests) != 2 {
		t.Fatalf("provider calls = %d, want summarize + final", len(provider.requests))
	}
	sumReq := provider.requests[0]
	last := sumReq.Rows[len(sumReq.Rows)-1]
	if last.Subtype != domain.MessageSubtypeInjectionCompactionRequest {
		t.Fatalf("summarize call's last row subtype = %q, want the compaction request", last.Subtype)
	}
	if q := rowWithContent(sumReq.Rows, "queued while it filled"); q == nil || isDelivered(*q) {
		t.Fatalf("queued row in the summarize call = %+v, want present but still undelivered", q)
	}
	if sumReq.IncludeUndelivered {
		t.Fatal("summarize call must not fold undelivered rows into assembly")
	}

	rows := tr.snapshot()
	byContent := func(sub string) *domain.Message {
		return tr.find(func(m domain.Message) bool { return strings.Contains(m.Content, sub) })
	}

	// The pinned opening survives; the worked turn, the request, and the
	// summarize reply are all inactive; the result row is active and carries
	// the summary but (anchored) no re-injected original request.
	if m := byContent("the mission"); m == nil || m.WindowState == domain.MessageWindowInactive {
		t.Fatalf("opening row = %+v, want pinned active", m)
	}
	if m := byContent("worked a lot"); m == nil || m.WindowState != domain.MessageWindowInactive {
		t.Fatalf("span row = %+v, want inactive", m)
	}
	req := tr.find(func(m domain.Message) bool { return m.Subtype == domain.MessageSubtypeInjectionCompactionRequest })
	if req == nil || req.WindowState != domain.MessageWindowInactive || !isDelivered(*req) {
		t.Fatalf("request row = %+v, want delivered and inactive after commit", req)
	}
	reply := byContent("thinking it through")
	if reply == nil || reply.Role != "assistant" || reply.WindowState != domain.MessageWindowInactive {
		t.Fatalf("summarize reply = %+v, want a persisted assistant row, inactive after commit", reply)
	}
	resultRow := tr.find(func(m domain.Message) bool { return m.Subtype == domain.MessageSubtypeInjectionCompactionResult })
	if resultRow == nil || resultRow.WindowState == domain.MessageWindowInactive {
		t.Fatalf("result row = %+v, want active", resultRow)
	}
	if !strings.Contains(resultRow.Content, "everything that happened") {
		t.Errorf("result row content = %q, want the summary", resultRow.Content)
	}
	if strings.Contains(resultRow.Content, "<original_request>") {
		t.Errorf("anchored result row re-injected the original request: %q", resultRow.Content)
	}

	// Ordering: the queued row was re-seqed after the result row and
	// delivered by the post-compaction drain; the final call saw it after
	// the summary.
	queued := byContent("queued while it filled")
	if queued == nil || queued.Seq == nil || *queued.Seq <= float64(resultRow.ID) || !isDelivered(*queued) {
		t.Fatalf("queued row = %+v, want delivered with seq after the result row %d", queued, resultRow.ID)
	}
	finalReq := provider.requests[1]
	var order []string
	for _, r := range finalReq.Rows {
		if isDelivered(r) && r.WindowState != domain.MessageWindowInactive {
			order = append(order, r.Content)
		}
	}
	sawSummary := -1
	sawQueued := -1
	for i, c := range order {
		if strings.Contains(c, "everything that happened") {
			sawSummary = i
		}
		if strings.Contains(c, "queued while it filled") {
			sawQueued = i
		}
	}
	if sawSummary < 0 || sawQueued < 0 || sawQueued < sawSummary {
		t.Fatalf("final call order = %v, want the queued row after the summary", order)
	}
	_ = rows
}

// TestWarmCompaction_RequestInvariance pins the cache-preservation contract:
// the summarize request differs from the working call by the appended
// request row and by nothing else, and pending input that arrived mid-turn
// is withheld from it, surviving as a steer after the summary.
func TestWarmCompaction_RequestInvariance(t *testing.T) {
	tr := newMemTranscript(pendingUser("start the work"))
	provider := &scriptedProvider{}
	provider.turns = []scriptedTurn{
		{
			calls: []domain.ToolCall{{ID: "t1", Name: "bash", Input: map[string]any{"cmd": "ls"}}},
			usage: inference.Usage{PromptTokens: overThreshold},
			onCall: func() {
				// A steer lands while the big turn streams: it must ride
				// through the compaction as queued input, not as span.
				_, _ = tr.Insert(context.Background(), "org", &domain.Message{
					Role: "user", Content: "late steer", Delivered: boolPtr(false),
				})
			},
		},
		summaryReply("compacted"),
		{text: "done"},
	}
	params := testParams()
	params.Effort = "high"
	params.MaxTokens = 9000
	engine := newTestEngine(tr, provider, newScriptedToolHost())

	result := engine.Run(context.Background(), params)
	if result.Kind != ResultConcluded {
		t.Fatalf("result = %+v, want concluded", result)
	}
	if len(provider.requests) != 3 {
		t.Fatalf("provider calls = %d, want work + summarize + final", len(provider.requests))
	}

	work, summarize := provider.requests[0], provider.requests[1]
	if work.SystemPrompt != summarize.SystemPrompt ||
		work.Model != summarize.Model ||
		work.Provider != summarize.Provider ||
		work.Effort != summarize.Effort ||
		work.MaxTokens != summarize.MaxTokens {
		t.Fatalf("summarize request shape diverged from the working call:\nwork      %+v\nsummarize %+v", work, summarize)
	}
	if work.ToolChoice != nil || summarize.ToolChoice != nil {
		t.Fatal("a conversational call must never set tool_choice")
	}
	if !reflect.DeepEqual(work.Tools, summarize.Tools) {
		t.Fatal("summarize call changed the tool list")
	}
	if steer := rowWithContent(summarize.Rows, "late steer"); steer != nil && isDelivered(*steer) {
		t.Fatalf("mid-turn input was delivered into the summarize call: %+v", steer)
	}

	// After the commit the steer is delivered (mid-work drain → steer stamp)
	// and ordered after the result row.
	resultRow := tr.find(func(m domain.Message) bool { return m.Subtype == domain.MessageSubtypeInjectionCompactionResult })
	steer := tr.find(func(m domain.Message) bool { return strings.Contains(m.Content, "late steer") })
	if steer == nil || !isDelivered(*steer) || steer.Subtype != domain.MessageSubtypeInjectionSteer {
		t.Fatalf("steer row = %+v, want delivered with the steer stamp", steer)
	}
	if steer.Seq == nil || *steer.Seq <= float64(resultRow.ID) {
		t.Fatalf("steer seq = %v, want after the result row %d", steer.Seq, resultRow.ID)
	}
}

// TestWarmCompactionFailure_ToolCallsHandsOffToForcedShape is the ~1/3 case:
// the model answers the ask by continuing to work. The reply is discarded
// (no row, no dispatch), its cost settles on the request row, and the
// forced-shape call finishes the compaction with the path-independent
// artifact.
func TestWarmCompactionFailure_ToolCallsHandsOffToForcedShape(t *testing.T) {
	tr := newMemTranscript(
		domain.Message{Role: "user", Content: "the mission"},
		usedAssistant("worked", overThreshold, time.Now().UTC()),
	)
	tools := newScriptedToolHost()
	provider := &scriptedProvider{turns: []scriptedTurn{
		{ // The failed warm attempt: tool calls instead of a summary.
			calls: []domain.ToolCall{{ID: "zombie-call", Name: "bash", Input: map[string]any{"cmd": "rm -rf /"}}},
			// Mostly cache reads, because a warm attempt runs against a hot
			// cache by construction — which is what makes this row the one
			// place the two token conventions are easiest to confuse.
			usage: inference.Usage{
				PromptTokens: 150000, OutputTokens: 500,
				CacheReadTokens: 120000, CacheCreationTokens: 10000,
			},
			model: "claude-sonnet-4-5",
		},
		forcedReply("cold analysis", "cold summary"),
		{text: "done"},
	}}
	engine := newTestEngine(tr, provider, tools)

	result := engine.Run(context.Background(), testParams())
	if result.Kind != ResultConcluded {
		t.Fatalf("result = %+v, want concluded", result)
	}

	// The failed reply's calls never ran and the reply was never persisted.
	for _, name := range tools.calls() {
		if name == "bash" {
			t.Fatal("a failed warm attempt's tool call was dispatched")
		}
	}
	if m := tr.find(func(m domain.Message) bool {
		return m.Role == "assistant" && len(m.ToolCalls) > 0
	}); m != nil {
		t.Fatalf("the discarded reply reached the transcript: %+v", m)
	}

	// Its accounting settled on the request row, with the reason — and in the
	// ledger's convention, where the three prompt columns are disjoint. The
	// neutral usage counts the prompt inclusive of its cache buckets, so
	// settling it verbatim would count 130k cached tokens twice on a row that
	// is, itself, one of the inputs to the next compaction decision.
	req := tr.find(func(m domain.Message) bool { return m.Subtype == domain.MessageSubtypeInjectionCompactionRequest })
	if req == nil || req.InputTokens == nil || *req.InputTokens != 20000 {
		t.Fatalf("request row = %+v, want the failed attempt's non-cached input (20000) settled on it", req)
	}
	if prompt := *req.InputTokens + *req.CacheReadTokens + *req.CacheCreationTokens; prompt != 150000 {
		t.Errorf("the row's prompt columns sum to %d, want the attempt's 150000 exactly once", prompt)
	}
	if req.CostUSD == nil {
		t.Error("failed attempt's cost not settled (sonnet is priceable)")
	}
	if req.Metadata["compaction_failure"] != compactionFailToolCalls {
		t.Errorf("failure reason = %v, want %q", req.Metadata["compaction_failure"], compactionFailToolCalls)
	}

	// Exactly one warm attempt: work call count is summarize(1) + forced(1)
	// + final(1) = 3 total requests, only one carrying the live system
	// prompt AND the request row.
	if len(provider.requests) != 3 {
		t.Fatalf("provider calls = %d, want summarize + forced + final", len(provider.requests))
	}
	forced := provider.requests[1]
	if forced.SystemPrompt != coldCompactionSystemPrompt || forced.Model != DefaultColdCompactionModel {
		t.Fatalf("forced call = (model %q, prompt %.40q...), want the dedicated cold shape", forced.Model, forced.SystemPrompt)
	}
	if forced.ToolChoice == nil || len(forced.Tools) != 1 || forced.Tools[0].Function.Name != compactionToolName {
		t.Fatalf("forced call tools = %+v choice = %+v, want exactly the required compaction tool", forced.Tools, forced.ToolChoice)
	}

	// The path-independent artifact: a reconstructed reply row, canonical
	// byte shape, cheap-model attributed, inactive, cost settled on it.
	reply := tr.find(func(m domain.Message) bool { return strings.Contains(m.Content, "cold analysis") })
	if reply == nil || reply.Role != "assistant" {
		t.Fatalf("reconstructed reply row missing: %+v", reply)
	}
	if want := canonicalCompactionArtifact("cold analysis", "cold summary"); reply.Content != want {
		t.Errorf("reconstructed reply content = %q, want the canonical artifact %q", reply.Content, want)
	}
	if reply.Model != DefaultColdCompactionModel || reply.WindowState != domain.MessageWindowInactive {
		t.Errorf("reconstructed reply = (model %q, state %q), want cheap-model attribution, inactive", reply.Model, reply.WindowState)
	}
	if reply.Metadata["compaction_path"] != "forced" {
		t.Errorf("reconstructed reply metadata = %v, want the forced path recorded", reply.Metadata)
	}
	if reply.CostUSD == nil {
		t.Error("forced call's cost not settled on the reconstructed reply row")
	}
	// And it never assembled: the final call's window is opening + result.
	finalReq := provider.requests[2]
	for _, r := range finalReq.Rows {
		if r.ID == reply.ID {
			t.Fatal("the reconstructed reply row leaked into a later assembly")
		}
	}
}

// TestWarmCompactionFailure_NoSummaryHandsOff is the second failure shape.
func TestWarmCompactionFailure_NoSummaryHandsOff(t *testing.T) {
	tr := newMemTranscript(
		domain.Message{Role: "user", Content: "the mission"},
		usedAssistant("worked", overThreshold, time.Now().UTC()),
	)
	provider := &scriptedProvider{turns: []scriptedTurn{
		{text: "I would rather keep working on the task, thanks."},
		forcedReply("a", "s"),
		{text: "done"},
	}}
	engine := newTestEngine(tr, provider, newScriptedToolHost())

	if result := engine.Run(context.Background(), testParams()); result.Kind != ResultConcluded {
		t.Fatalf("result = %+v, want concluded", result)
	}
	req := tr.find(func(m domain.Message) bool { return m.Subtype == domain.MessageSubtypeInjectionCompactionRequest })
	if req.Metadata["compaction_failure"] != compactionFailNoSummary {
		t.Errorf("failure reason = %v, want %q", req.Metadata["compaction_failure"], compactionFailNoSummary)
	}
	if m := tr.find(func(m domain.Message) bool { return strings.Contains(m.Content, "rather keep working") }); m != nil {
		t.Fatalf("the discarded reply reached the transcript: %+v", m)
	}
}

// TestUnanchoredCompaction_ReinjectsTheOriginalRequest covers the taskless
// arm: nothing pinned, and the first human message survives mechanically in
// the result row — capped when enormous.
func TestUnanchoredCompaction_ReinjectsTheOriginalRequest(t *testing.T) {
	t.Run("verbatim under the cap", func(t *testing.T) {
		tr := newMemTranscript(
			domain.Message{Role: "user", Content: "please build me the widget"},
			usedAssistant("worked", overThreshold, time.Now().UTC()),
		)
		provider := &scriptedProvider{turns: []scriptedTurn{summaryReply("did widget things"), {text: "done"}}}
		engine := newTestEngine(tr, provider, newScriptedToolHost())
		params := testParams()
		params.MissionAnchored = false

		if result := engine.Run(context.Background(), params); result.Kind != ResultConcluded {
			t.Fatalf("result = %+v, want concluded", result)
		}
		opening := tr.find(func(m domain.Message) bool {
			return strings.Contains(m.Content, "build me the widget") && m.Subtype == ""
		})
		if opening == nil || opening.WindowState != domain.MessageWindowInactive {
			t.Fatalf("unanchored opening = %+v, want compacted like everything else", opening)
		}
		resultRow := tr.find(func(m domain.Message) bool { return m.Subtype == domain.MessageSubtypeInjectionCompactionResult })
		if !strings.Contains(resultRow.Content, "<original_request>\nplease build me the widget\n</original_request>") {
			t.Fatalf("result row = %q, want the original request re-injected verbatim", resultRow.Content)
		}
	})

	t.Run("capped over 4096 bytes", func(t *testing.T) {
		huge := strings.Repeat("x", 5000)
		tr := newMemTranscript(
			domain.Message{Role: "user", Content: huge},
			usedAssistant("worked", overThreshold, time.Now().UTC()),
		)
		provider := &scriptedProvider{turns: []scriptedTurn{summaryReply("s"), {text: "done"}}}
		engine := newTestEngine(tr, provider, newScriptedToolHost())
		params := testParams()
		params.MissionAnchored = false

		if result := engine.Run(context.Background(), params); result.Kind != ResultConcluded {
			t.Fatalf("result = %+v, want concluded", result)
		}
		resultRow := tr.find(func(m domain.Message) bool { return m.Subtype == domain.MessageSubtypeInjectionCompactionResult })
		if strings.Contains(resultRow.Content, huge) {
			t.Fatal("an over-cap original request was re-injected wholesale")
		}
		if !strings.Contains(resultRow.Content, "[truncated — the original message was 5000 bytes") {
			t.Fatalf("result row = %.120q..., want the truncation note", resultRow.Content)
		}
	})
}

// TestReactiveOverflow_CompactsAndRetriesOnce: the provider rejects for
// length, the engine compacts (cold here — the prior turn is old) and
// retries exactly once; a second overflow right after fails the run with a
// visible stop-note.
func TestReactiveOverflow_CompactsAndRetriesOnce(t *testing.T) {
	overflowErr := fmt.Errorf("%w: prompt is too long: 201000 tokens > 200000 maximum", inference.ErrContextOverflow)

	t.Run("compact then retry succeeds", func(t *testing.T) {
		old := time.Now().UTC().Add(-time.Hour)
		tr := newMemTranscript(
			domain.Message{Role: "user", Content: "the mission", CreatedAt: old},
			usedAssistant("prior work", 1000, old),
		)
		provider := &scriptedProvider{turns: []scriptedTurn{
			{err: overflowErr},
			forcedReply("a", "s"),
			{text: "done"},
		}}
		engine := newTestEngine(tr, provider, newScriptedToolHost())

		result := engine.Run(context.Background(), testParams())
		if result.Kind != ResultConcluded {
			t.Fatalf("result = %+v, want concluded after compact-and-retry", result)
		}
		if n := len(provider.requests); n != 3 {
			t.Fatalf("provider calls = %d, want overflowed + forced + retried", n)
		}
		if tr.find(func(m domain.Message) bool { return m.Subtype == domain.MessageSubtypeInjectionCompactionResult }) == nil {
			t.Fatal("no compaction result row after the reactive arm")
		}
	})

	t.Run("a second overflow fails the run", func(t *testing.T) {
		old := time.Now().UTC().Add(-time.Hour)
		tr := newMemTranscript(
			domain.Message{Role: "user", Content: "the mission", CreatedAt: old},
			usedAssistant("prior work", 1000, old),
		)
		provider := &scriptedProvider{turns: []scriptedTurn{
			{err: overflowErr},
			forcedReply("a", "s"),
			{err: overflowErr},
		}}
		engine := newTestEngine(tr, provider, newScriptedToolHost())

		result := engine.Run(context.Background(), testParams())
		if result.Kind != ResultFailed {
			t.Fatalf("result = %+v, want failed on the second overflow", result)
		}
		if n := tr.find(func(m domain.Message) bool {
			return m.Subtype == domain.MessageSubtypeStopNote && strings.Contains(m.Content, "could not be retried")
		}); n == nil {
			t.Fatal("no stop-note recording the terminal overflow")
		}
		// One compaction, not a loop of them.
		count := 0
		for _, m := range tr.snapshot() {
			if m.Subtype == domain.MessageSubtypeInjectionCompactionResult {
				count++
			}
		}
		if count != 1 {
			t.Fatalf("compaction results = %d, want exactly one", count)
		}
	})
}

// TestColdResume_CompactsBeforeTheFirstCall: claim-start order is repair →
// compact → drain → call. The forced-shape call runs before any
// conversational call and never sees queued input.
func TestColdResume_CompactsBeforeTheFirstCall(t *testing.T) {
	old := time.Now().UTC().Add(-time.Hour)
	tr := newMemTranscript(
		domain.Message{Role: "user", Content: "the mission", CreatedAt: old},
		usedAssistant("prior work", overThreshold, old),
		pendingUser("queued overnight"),
	)
	provider := &scriptedProvider{turns: []scriptedTurn{
		forcedReply("resume analysis", "resume summary"),
		{text: "done"},
	}}
	engine := newTestEngine(tr, provider, newScriptedToolHost())

	result := engine.Run(context.Background(), testParams())
	if result.Kind != ResultConcluded {
		t.Fatalf("result = %+v, want concluded", result)
	}
	first := provider.requests[0]
	if first.SystemPrompt != coldCompactionSystemPrompt {
		t.Fatalf("first provider call system prompt = %.40q..., want the forced-shape call before any conversational one", first.SystemPrompt)
	}
	if q := rowWithContent(first.Rows, "queued overnight"); q != nil && isDelivered(*q) {
		t.Fatal("queued input was delivered into the resume compaction")
	}
	// The summarize call carries a cap resolved for the SUMMARIZE model, not
	// the conversation's and not the provider layer's default: this call's
	// entire output is the summary, and a summary cut short is the one
	// failure this path has no fallback beneath.
	if first.MaxTokens != 64000 {
		t.Errorf("forced-shape call max_tokens = %d, want haiku's own maximum 64000", first.MaxTokens)
	}
	resultRow := tr.find(func(m domain.Message) bool { return m.Subtype == domain.MessageSubtypeInjectionCompactionResult })
	queued := tr.find(func(m domain.Message) bool { return strings.Contains(m.Content, "queued overnight") })
	if queued == nil || !isDelivered(*queued) || queued.Seq == nil || *queued.Seq <= float64(resultRow.ID) {
		t.Fatalf("queued row = %+v, want delivered after the result row %d", queued, resultRow.ID)
	}
}

// TestCompactionReentry_RequestWithNoReply: the crash shape. A delivered
// request with no reply re-enters compaction on the next claim without
// minting a second ask.
func TestCompactionReentry_RequestWithNoReply(t *testing.T) {
	t.Run("warm re-claim reuses the standing request", func(t *testing.T) {
		tr := newMemTranscript(
			domain.Message{Role: "user", Content: "the mission"},
			usedAssistant("prior work", overThreshold, time.Now().UTC()),
			domain.Message{Role: "user", Subtype: domain.MessageSubtypeInjectionCompactionRequest, Content: warmCompactionRequest},
		)
		provider := &scriptedProvider{turns: []scriptedTurn{
			summaryReply("recovered"),
			{text: "done"},
		}}
		engine := newTestEngine(tr, provider, newScriptedToolHost())

		if result := engine.Run(context.Background(), testParams()); result.Kind != ResultConcluded {
			t.Fatalf("result = %+v, want concluded", result)
		}
		count := 0
		for _, m := range tr.snapshot() {
			if m.Subtype == domain.MessageSubtypeInjectionCompactionRequest {
				count++
			}
		}
		if count != 1 {
			t.Fatalf("request rows = %d, want the standing ask reused, not re-minted", count)
		}
	})

	t.Run("cold re-claim folds the stale request into the span", func(t *testing.T) {
		old := time.Now().UTC().Add(-time.Hour)
		tr := newMemTranscript(
			domain.Message{Role: "user", Content: "the mission", CreatedAt: old},
			usedAssistant("prior work", overThreshold, old),
			domain.Message{Role: "user", Subtype: domain.MessageSubtypeInjectionCompactionRequest, Content: warmCompactionRequest, CreatedAt: old},
		)
		provider := &scriptedProvider{turns: []scriptedTurn{
			forcedReply("a", "s"),
			{text: "done"},
		}}
		engine := newTestEngine(tr, provider, newScriptedToolHost())

		if result := engine.Run(context.Background(), testParams()); result.Kind != ResultConcluded {
			t.Fatalf("result = %+v, want concluded", result)
		}
		req := tr.find(func(m domain.Message) bool { return m.Subtype == domain.MessageSubtypeInjectionCompactionRequest })
		if req == nil || req.WindowState != domain.MessageWindowInactive {
			t.Fatalf("stale request row = %+v, want compacted with the span", req)
		}
	})
}

// TestIsHumanInput_StopNoteIsNotHuman pins the vocabulary invariant for the
// stop-note row, which has two producers: the loop's own park decisions and
// a user pressing stop. Both write a role=user row, and neither is somebody
// asking for more work — a note that read as human input would let a re-park
// restate itself on every claim.
func TestIsHumanInput_StopNoteIsNotHuman(t *testing.T) {
	if IsHumanInput(domain.Message{Role: "user", Subtype: domain.MessageSubtypeStopNote}) {
		t.Error("IsHumanInput(stop-note) = true, want false")
	}
}

// TestIsHumanInput_CompactionRowsAreNotHuman pins the same invariant for
// compaction: neither the request nor the result row is human input.
func TestIsHumanInput_CompactionRowsAreNotHuman(t *testing.T) {
	for _, sub := range []string{
		domain.MessageSubtypeInjectionCompactionRequest,
		domain.MessageSubtypeInjectionCompactionResult,
	} {
		if IsHumanInput(domain.Message{Role: "user", Subtype: sub}) {
			t.Errorf("IsHumanInput(%q) = true, want false", sub)
		}
	}
}

// TestIsTransient_OverflowIsNeverRetried: the guard matters — an overflow
// message quotes token counts, and "429 tokens" would otherwise satisfy the
// numeric-token matcher and buy a retry on a permanently doomed request.
func TestIsTransient_OverflowIsNeverRetried(t *testing.T) {
	err := fmt.Errorf("%w: prompt is too long: 429 tokens > 200 maximum (HTTP 400)", inference.ErrContextOverflow)
	if isTransient(err) {
		t.Fatal("a context overflow classified as transient")
	}
}

// TestCompaction_SanitizesModelTextForStore: a summary that quotes binary
// command output carries NUL bytes Postgres cannot store, and both
// compaction rows are built from raw model text (the warm summary is
// extracted from the unsanitized completion; the forced arguments come
// straight from JSON). One NUL inside the fenced commit would fail the
// whole compaction — including in the reactive arm, where compaction is the
// rescue path.
func TestCompaction_SanitizesModelTextForStore(t *testing.T) {
	assertNoNUL := func(t *testing.T, tr *memTranscript) {
		t.Helper()
		for _, m := range tr.snapshot() {
			if strings.ContainsRune(m.Content, 0) {
				t.Fatalf("row %d content carries a NUL byte: %q", m.ID, m.Content)
			}
		}
	}

	t.Run("warm summary", func(t *testing.T) {
		tr := newMemTranscript(
			domain.Message{Role: "user", Content: "the mission"},
			usedAssistant("worked", overThreshold, time.Now().UTC()),
		)
		provider := &scriptedProvider{turns: []scriptedTurn{
			summaryReply("binary output was \x00\x00 here"),
			{text: "done"},
		}}
		engine := newTestEngine(tr, provider, newScriptedToolHost())
		if result := engine.Run(context.Background(), testParams()); result.Kind != ResultConcluded {
			t.Fatalf("result = %+v, want concluded", result)
		}
		resultRow := tr.find(func(m domain.Message) bool { return m.Subtype == domain.MessageSubtypeInjectionCompactionResult })
		if !strings.Contains(resultRow.Content, "binary output was") {
			t.Fatalf("result row lost the summary: %q", resultRow.Content)
		}
		assertNoNUL(t, tr)
	})

	t.Run("forced-shape arguments", func(t *testing.T) {
		old := time.Now().UTC().Add(-time.Hour)
		tr := newMemTranscript(
			domain.Message{Role: "user", Content: "the mission", CreatedAt: old},
			usedAssistant("worked", overThreshold, old),
		)
		provider := &scriptedProvider{turns: []scriptedTurn{
			forcedReply("saw \x00 bytes", "kept \x00 going"),
			{text: "done"},
		}}
		engine := newTestEngine(tr, provider, newScriptedToolHost())
		if result := engine.Run(context.Background(), testParams()); result.Kind != ResultConcluded {
			t.Fatalf("result = %+v, want concluded", result)
		}
		reply := tr.find(func(m domain.Message) bool { return strings.Contains(m.Content, "saw") && m.Role == "assistant" })
		if reply == nil {
			t.Fatal("reconstructed reply row missing")
		}
		assertNoNUL(t, tr)
	})
}

// composedOpeningTurn is the shape a delegation's opening turn really has now
// that the mission rides the transcript: several sections in one row. The
// fixtures above use a one-line stand-in; the pin has to hold for this.
const composedOpeningTurn = "<run_context>\n" +
	"Branch naming convention for this team: tfac/SKY-9\n" +
	"</run_context>\n\n" +
	"<task_context>\n" +
	"Reference data about the task that fired this run.\n\n" +
	"- Task: CI is failing on the retry path\n" +
	"- Repository: owner/repo\n" +
	"- Pull request: #18\n" +
	"</task_context>\n\n" +
	"Fix the failing check on pull request 18."

// TestAnchoredCompaction_PinsTheComposedOpeningTurn is what keeps a long run
// from summarizing away the only statement of what it was asked to do. Several
// turns happen first, so the pin is exercised on a real window rather than on a
// transcript whose opening is also its only row.
func TestAnchoredCompaction_PinsTheComposedOpeningTurn(t *testing.T) {
	tr := newMemTranscript(
		domain.Message{Role: "user", Content: composedOpeningTurn},
		domain.Message{Role: "assistant", Content: "reading the failing job"},
		domain.Message{Role: "user", Subtype: domain.MessageSubtypeInjectionSteer, Content: "check the flaky test too"},
		domain.Message{Role: "assistant", Content: "found the race"},
		usedAssistant("pushed the fix", overThreshold, time.Now().UTC()),
	)
	provider := &scriptedProvider{turns: []scriptedTurn{
		summaryReply("fixed the race, pushed"),
		{text: "done"},
	}}
	engine := newTestEngine(tr, provider, newScriptedToolHost())

	if result := engine.Run(context.Background(), testParams()); result.Kind != ResultConcluded {
		t.Fatalf("result = %+v, want concluded", result)
	}

	opening := tr.find(func(m domain.Message) bool { return m.Content == composedOpeningTurn })
	if opening == nil || opening.WindowState == domain.MessageWindowInactive {
		t.Fatalf("opening turn = %+v, want pinned active through the compaction", opening)
	}
	// Everything after it went: the pin is the opening, not the beginning of the
	// conversation.
	for _, gone := range []string{"reading the failing job", "check the flaky test too", "found the race", "pushed the fix"} {
		row := tr.find(func(m domain.Message) bool { return m.Content == gone })
		if row == nil || row.WindowState != domain.MessageWindowInactive {
			t.Errorf("row %q = %+v, want compacted", gone, row)
		}
	}

	// What the model reads next: the mission, in full, ahead of the summary.
	final := provider.requests[len(provider.requests)-1]
	var window []string
	for _, r := range final.Rows {
		if r.WindowState != domain.MessageWindowInactive {
			window = append(window, r.Content)
		}
	}
	if len(window) == 0 || window[0] != composedOpeningTurn {
		t.Fatalf("post-compaction window opens with %.60q, want the whole opening turn", strings.Join(window, "|"))
	}
	if !strings.Contains(strings.Join(window, "\n"), "fixed the race, pushed") {
		t.Errorf("post-compaction window = %v, want the summary after the mission", window)
	}
}

// TestCompactionSpan_AnchorCoversEveryLeadingUserRow covers the shape the
// opening turn is not, but could become: several rows rather than one. The
// anchor is every leading row before the first assistant turn, so splitting the
// mission up later cannot feed half of it to the summarizer.
func TestCompactionSpan_AnchorCoversEveryLeadingUserRow(t *testing.T) {
	rows := []domain.Message{
		{ID: 1, Role: "user", Content: "<run_context>…</run_context>"},
		{ID: 2, Role: "user", Content: "<task_context>…</task_context>"},
		{ID: 3, Role: "user", Content: "the mission"},
		{ID: 4, Role: "assistant", Content: "working"},
		{ID: 5, Role: "user", Subtype: domain.MessageSubtypeInjectionSteer, Content: "and this"},
		{ID: 6, Role: "assistant", Content: "still working"},
		{ID: 7, Role: "user", Content: "queued", Delivered: boolPtr(false)},
	}
	if got, want := compactionSpan(rows, true), []int{4, 5, 6}; !reflect.DeepEqual(got, want) {
		t.Errorf("anchored span = %v, want %v — every leading user row is the mission", got, want)
	}
	// Unanchored, the same rows are all fair game: a taskless conversation's
	// opening is just its oldest message, re-injected into the result row.
	if got, want := compactionSpan(rows, false), []int{1, 2, 3, 4, 5, 6}; !reflect.DeepEqual(got, want) {
		t.Errorf("unanchored span = %v, want %v", got, want)
	}
}
