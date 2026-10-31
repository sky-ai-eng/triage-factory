package inference

import (
	"reflect"
	"testing"

	"github.com/maximhq/bifrost/core/schemas"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// packed renders a message list as a comparable shape: role plus the text of
// each content block, so a test can assert the exact packing rather than
// poking at pointer fields.
func packed(msgs []schemas.ChatMessage) []string {
	out := make([]string, 0, len(msgs))
	for _, m := range msgs {
		s := string(m.Role) + ":"
		switch {
		case m.Content == nil:
			s += "<nil>"
		case m.Content.ContentStr != nil:
			s += *m.Content.ContentStr
		default:
			for i, b := range m.Content.ContentBlocks {
				if i > 0 {
					s += "|"
				}
				if b.Text != nil {
					s += *b.Text
				} else {
					s += "<" + string(b.Type) + ">"
				}
			}
		}
		out = append(out, s)
	}
	return out
}

func TestConsolidateAdjacentUsers(t *testing.T) {
	tests := []struct {
		name string
		rows []domain.Message
		want []string
	}{
		{
			name: "a lone user row keeps its string form",
			rows: []domain.Message{{ID: 1, Role: "user", Content: "hello"}},
			want: []string{"user:hello"},
		},
		{
			name: "a run of adjacent user rows packs into one message",
			rows: []domain.Message{
				{ID: 1, Role: "user", Content: "first"},
				{ID: 2, Role: "user", Content: "second"},
				{ID: 3, Role: "user", Content: "third"},
			},
			want: []string{"user:first|second|third"},
		},
		{
			name: "runs are bounded by non-user messages",
			rows: []domain.Message{
				{ID: 1, Role: "user", Content: "a"},
				{ID: 2, Role: "user", Content: "b"},
				{ID: 3, Role: "assistant", Content: "reply"},
				{ID: 4, Role: "user", Content: "c"},
			},
			want: []string{"user:a|b", "assistant:reply", "user:c"},
		},
		{
			name: "tool rows are left to bifrost's own grouping",
			rows: []domain.Message{
				{ID: 1, Role: "assistant", Content: "calling", ToolCalls: []domain.ToolCall{{ID: "t1", Name: "ls"}}},
				{ID: 2, Role: "tool", ToolCallID: "t1", Content: "out"},
				{ID: 3, Role: "tool", ToolCallID: "t2", Content: "out2"},
			},
			want: []string{"assistant:calling", "tool:out", "tool:out2"},
		},
		{
			name: "an empty user row contributes nothing to the pack",
			rows: []domain.Message{
				{ID: 1, Role: "user", Content: "a"},
				{ID: 2, Role: "user", Content: ""},
				{ID: 3, Role: "user", Content: "b"},
			},
			want: []string{"user:a|b"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			msgs, err := RowsToMessages(tc.rows, AssemblyOptions{NoCacheBreakpoint: true})
			if err != nil {
				t.Fatal(err)
			}
			if got := packed(msgs); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("packed = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestConsolidateAdjacentUsers_IsDeterministic pins the property two
// executors depend on: the same rows always assemble to the same request.
func TestConsolidateAdjacentUsers_IsDeterministic(t *testing.T) {
	rows := []domain.Message{
		{ID: 1, Role: "user", Content: "a"},
		{ID: 2, Role: "user", Content: "b"},
		{ID: 3, Role: "assistant", Content: "r"},
		{ID: 4, Role: "user", Content: "c"},
		{ID: 5, Role: "user", Content: "d"},
	}
	first, err := RowsToMessages(rows, AssemblyOptions{NoCacheBreakpoint: true})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		again, err := RowsToMessages(rows, AssemblyOptions{NoCacheBreakpoint: true})
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(packed(first), packed(again)) {
			t.Fatalf("assembly is not deterministic: %v then %v", packed(first), packed(again))
		}
	}
}

// TestConsolidateAdjacentUsers_IsAppendStable pins the property prompt
// caching depends on: appending a row never changes the packed form of
// anything before the last user run.
func TestConsolidateAdjacentUsers_IsAppendStable(t *testing.T) {
	base := []domain.Message{
		{ID: 1, Role: "user", Content: "a"},
		{ID: 2, Role: "assistant", Content: "r1"},
		{ID: 3, Role: "user", Content: "b"},
		{ID: 4, Role: "assistant", Content: "r2"},
		{ID: 5, Role: "user", Content: "c"},
	}
	before, err := RowsToMessages(base, AssemblyOptions{NoCacheBreakpoint: true})
	if err != nil {
		t.Fatal(err)
	}

	grown := append(append([]domain.Message(nil), base...), domain.Message{ID: 6, Role: "user", Content: "d"})
	after, err := RowsToMessages(grown, AssemblyOptions{NoCacheBreakpoint: true})
	if err != nil {
		t.Fatal(err)
	}

	beforePacked, afterPacked := packed(before), packed(after)
	if len(afterPacked) != len(beforePacked) {
		t.Fatalf("appending to the final user run must not add a message: %v then %v", beforePacked, afterPacked)
	}
	// Everything up to the last user run is byte-identical.
	for i := 0; i < len(beforePacked)-1; i++ {
		if beforePacked[i] != afterPacked[i] {
			t.Fatalf("message %d changed on append (%q -> %q): the cached prefix would be invalidated",
				i, beforePacked[i], afterPacked[i])
		}
	}
	if afterPacked[len(afterPacked)-1] != "user:c|d" {
		t.Fatalf("the final user run must grow: %q", afterPacked[len(afterPacked)-1])
	}
}

// TestConsolidateRunsBeforeCacheBreakpoint pins the ordering the loop
// depends on: the moving breakpoint lands on the merged message's last
// block, not on a block a later merge would move.
func TestConsolidateRunsBeforeCacheBreakpoint(t *testing.T) {
	rows := []domain.Message{
		{ID: 1, Role: "assistant", Content: "r"},
		{ID: 2, Role: "user", Content: "a"},
		{ID: 3, Role: "user", Content: "b"},
	}
	msgs, err := RowsToMessages(rows, AssemblyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	last := msgs[len(msgs)-1]
	blocks := last.Content.ContentBlocks
	if len(blocks) != 2 {
		t.Fatalf("the final message must be the merged pair: %+v", blocks)
	}
	if blocks[0].CacheControl != nil {
		t.Error("the breakpoint must not land on an interior block of the merged message")
	}
	if blocks[1].CacheControl == nil {
		t.Fatal("the breakpoint must land on the merged message's last block")
	}
}

func TestWrapSteer_WrapsOnlySteerRows(t *testing.T) {
	rows := []domain.Message{
		{ID: 1, Role: "user", Content: "plain"},
		{ID: 2, Role: "assistant", Content: "r"},
		{ID: 3, Role: "user", Subtype: domain.MessageSubtypeInjectionSteer, Content: "also check the tests"},
	}
	msgs, err := RowsToMessages(rows, AssemblyOptions{NoCacheBreakpoint: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := *msgs[0].Content.ContentStr; got != "plain" {
		t.Errorf("an ordinary user row must not be wrapped: %q", got)
	}
	steer := *msgs[2].Content.ContentStr
	if !containsAll(steer, "<steer", "keep going", "also check the tests", "</steer>") {
		t.Errorf("a steer row must be wrapped in the keep-working envelope: %q", steer)
	}
}

// TestWrapSteer_IsDrivenOnlyByTheRow pins assembly purity: the wrapping is a
// function of the row's own columns, so the same rows wrap identically no
// matter which process assembles them.
func TestWrapSteer_IsDrivenOnlyByTheRow(t *testing.T) {
	row := domain.Message{ID: 1, Role: "user", Subtype: domain.MessageSubtypeInjectionSteer, Content: "x"}
	a, err := rowToMessage(row)
	if err != nil {
		t.Fatal(err)
	}
	b, err := rowToMessage(row)
	if err != nil {
		t.Fatal(err)
	}
	if *a.Content.ContentStr != *b.Content.ContentStr {
		t.Fatal("wrapping must be a pure function of the row")
	}
}

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		found := false
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// memoryRow is one injected prior memory: a system-authored user row whose
// content is the memory text and nothing around it, the framing being the
// envelope assembly adds.
func memoryRow(id int, content string) domain.Message {
	return domain.Message{ID: id, Role: "user", Subtype: domain.MessageSubtypeInjectionMemory, Content: content}
}

func taskContextRow(id int) domain.Message {
	return domain.Message{
		ID: id, Role: "user", Subtype: domain.MessageSubtypeInjectionTaskContext,
		Content: "<task_context>\nPR #18\n</task_context>",
	}
}

// TestWrapMemoryRuns_BracketsEachMaximalRun pins the envelope's shape: one
// opening block before the first row of a run and one closing block after the
// last, K counting the rows in that run, and a run bounded by anything that
// is not a memory row.
func TestWrapMemoryRuns_BracketsEachMaximalRun(t *testing.T) {
	tests := []struct {
		name string
		rows []domain.Message
		want []string
	}{
		{
			name: "a run of one is opened and closed on the same message",
			rows: []domain.Message{memoryRow(1, "m1")},
			want: []string{"user:" + domain.MemoryEnvelopeOpen(1) + "|m1|" + domain.MemoryEnvelopeClose},
		},
		{
			name: "K is the length of the run",
			rows: []domain.Message{memoryRow(1, "m1"), memoryRow(2, "m2"), memoryRow(3, "m3")},
			want: []string{"user:" + domain.MemoryEnvelopeOpen(3) + "|m1|m2|m3|" + domain.MemoryEnvelopeClose},
		},
		{
			name: "a transcript with no memory row gets no envelope",
			rows: []domain.Message{{ID: 1, Role: "user", Content: "plain"}, taskContextRow(2)},
			want: []string{"user:plain|<task_context>\nPR #18\n</task_context>"},
		},
		{
			name: "each run is bracketed on its own and counts only its own rows",
			rows: []domain.Message{
				memoryRow(1, "m1"),
				{ID: 2, Role: "assistant", Content: "working"},
				memoryRow(3, "m2"),
				memoryRow(4, "m3"),
			},
			want: []string{
				"user:" + domain.MemoryEnvelopeOpen(1) + "|m1|" + domain.MemoryEnvelopeClose,
				"assistant:working",
				"user:" + domain.MemoryEnvelopeOpen(2) + "|m2|m3|" + domain.MemoryEnvelopeClose,
			},
		},
		{
			name: "a plain user row beside a run stays outside the envelope",
			rows: []domain.Message{
				memoryRow(1, "m1"),
				{ID: 2, Role: "user", Content: "typed"},
			},
			want: []string{"user:" + domain.MemoryEnvelopeOpen(1) + "|m1|" + domain.MemoryEnvelopeClose + "|typed"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			msgs, err := RowsToMessages(tc.rows, AssemblyOptions{NoCacheBreakpoint: true})
			if err != nil {
				t.Fatal(err)
			}
			if got := packed(msgs); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("packed = %#v, want %#v", got, tc.want)
			}
		})
	}
}

// TestWrapMemoryRuns_ElidedRowKeepsItsPlace pins that elision does not break
// a run: the row is still one of the K and renders its stub inside the
// envelope, so the note stays a faithful record of what was injected.
func TestWrapMemoryRuns_ElidedRowKeepsItsPlace(t *testing.T) {
	rows := []domain.Message{
		memoryRow(1, "m1"),
		func() domain.Message {
			r := memoryRow(2, "m2")
			r.WindowState = domain.MessageWindowElided
			return r
		}(),
		memoryRow(3, "m3"),
	}
	msgs, err := RowsToMessages(rows, AssemblyOptions{NoCacheBreakpoint: true})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"user:" + domain.MemoryEnvelopeOpen(3) + "|m1|" + elidedStubText + "|m3|" + domain.MemoryEnvelopeClose}
	if got := packed(msgs); !reflect.DeepEqual(got, want) {
		t.Fatalf("packed = %#v, want %#v", got, want)
	}
}

// TestWrapMemoryRuns_NoRunOnceEveryRowIsInactive pins the compaction case:
// the envelope is a property of the surviving run, so once compaction has
// taken the memories out of the window there is no run and no note claiming
// memories the model cannot see.
func TestWrapMemoryRuns_NoRunOnceEveryRowIsInactive(t *testing.T) {
	inactive := func(id int, content string) domain.Message {
		r := memoryRow(id, content)
		r.WindowState = domain.MessageWindowInactive
		return r
	}
	rows := []domain.Message{inactive(1, "m1"), inactive(2, "m2"), taskContextRow(3)}
	msgs, err := RowsToMessages(rows, AssemblyOptions{NoCacheBreakpoint: true})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"user:<task_context>\nPR #18\n</task_context>"}
	if got := packed(msgs); !reflect.DeepEqual(got, want) {
		t.Fatalf("packed = %#v, want %#v", got, want)
	}
}

// TestWrapMemoryRuns_IsDrivenOnlyByTheRows is wrapSteer's purity discipline
// one level up: the envelope is a function of the rows, so two executors
// assembling the same rows build the same bytes.
func TestWrapMemoryRuns_IsDrivenOnlyByTheRows(t *testing.T) {
	rows := []domain.Message{memoryRow(1, "m1"), memoryRow(2, "m2"), taskContextRow(3)}
	first, err := RowsToMessages(rows, AssemblyOptions{NoCacheBreakpoint: true})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		again, err := RowsToMessages(rows, AssemblyOptions{NoCacheBreakpoint: true})
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(packed(first), packed(again)) {
			t.Fatalf("the envelope must be a pure function of the rows: %#v then %#v", packed(first), packed(again))
		}
	}
}

// TestOpeningRows_AssembleToOneTurnWithTheBreakpointOnTheTaskContext is the
// wire shape of a new conversation's opening turn: one user message whose
// blocks are the envelope open, the memories in order, the close, and the
// task context — with the moving breakpoint on that last block.
func TestOpeningRows_AssembleToOneTurnWithTheBreakpointOnTheTaskContext(t *testing.T) {
	rows := []domain.Message{memoryRow(1, "m1"), memoryRow(2, "m2"), taskContextRow(3)}
	msgs, err := RowsToMessages(rows, AssemblyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 {
		t.Fatalf("the opening rows must assemble to one user turn, got %d: %#v", len(msgs), packed(msgs))
	}
	blocks := msgs[0].Content.ContentBlocks
	wantTexts := []string{
		domain.MemoryEnvelopeOpen(2),
		"m1",
		"m2",
		domain.MemoryEnvelopeClose,
		"<task_context>\nPR #18\n</task_context>",
	}
	if len(blocks) != len(wantTexts) {
		t.Fatalf("block count = %d, want %d: %#v", len(blocks), len(wantTexts), packed(msgs))
	}
	for i, want := range wantTexts {
		if blocks[i].Text == nil || *blocks[i].Text != want {
			t.Errorf("block %d = %q, want %q", i, derefStr(blocks[i].Text), want)
		}
	}
	for i := 0; i < len(blocks)-1; i++ {
		if blocks[i].CacheControl != nil {
			t.Errorf("block %d must not carry the moving breakpoint", i)
		}
	}
	if blocks[len(blocks)-1].CacheControl == nil {
		t.Fatal("the moving breakpoint must land on the task-context block")
	}
}
