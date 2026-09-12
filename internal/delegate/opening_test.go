package delegate

import (
	"context"
	"errors"
	"sort"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// collectRows is an insertRow that records what it was handed instead of
// writing it, so the composer's own output can be asserted without a store.
func collectRows(into *[]domain.Message) insertRow {
	return func(_ context.Context, msg *domain.Message) error {
		*into = append(*into, *msg)
		return nil
	}
}

func memoriesOfSize(t *testing.T, sizes ...int) []domain.TaskMemory {
	t.Helper()
	out := make([]domain.TaskMemory, 0, len(sizes))
	for i, n := range sizes {
		out = append(out, domain.TaskMemory{
			ID:      string(rune('a' + i)),
			Content: strings.Repeat("x", n),
		})
	}
	return out
}

func memoryIDs(rows []domain.Message) []string {
	var out []string
	for _, r := range rows {
		if r.Subtype != domain.MessageSubtypeInjectionMemory {
			continue
		}
		id, _ := r.Metadata["memory_id"].(string)
		out = append(out, id)
	}
	return out
}

// TestMintOpeningRows_Shape pins what a conversation opens with: one delivered
// user row per injected memory, oldest first, then the task context under its
// own subtype — and each memory row carrying its source key and nothing else,
// because every other fact about a memory is one foreign key from that id.
func TestMintOpeningRows_Shape(t *testing.T) {
	var got []domain.Message
	memories := []domain.TaskMemory{
		{ID: "m1", Content: "the oldest"},
		{ID: "m2", Content: "the newest"},
	}
	minted, err := mintOpeningRows(context.Background(), collectRows(&got), nil, memories, "conv-1", "user-1", "<task_context>ctx</task_context>")
	if err != nil {
		t.Fatalf("mintOpeningRows: %v", err)
	}
	if len(minted) != len(got) {
		t.Fatalf("returned %d rows but wrote %d; the caller must see exactly what was persisted", len(minted), len(got))
	}

	want := []struct{ subtype, content string }{
		{domain.MessageSubtypeInjectionMemory, "the oldest"},
		{domain.MessageSubtypeInjectionMemory, "the newest"},
		{domain.MessageSubtypeInjectionTaskContext, "<task_context>ctx</task_context>"},
	}
	if len(got) != len(want) {
		t.Fatalf("rows = %d, want %d: %+v", len(got), len(want), got)
	}
	for i, w := range want {
		r := got[i]
		if r.Role != "user" || r.Subtype != w.subtype || r.Content != w.content {
			t.Errorf("row %d = {%q %q %q}, want a user row {%q, %q}", i, r.Role, r.Subtype, r.Content, w.subtype, w.content)
		}
		if r.ConversationID != "conv-1" || r.UserID != "user-1" {
			t.Errorf("row %d attributed to (%q, %q), want (conv-1, user-1)", i, r.ConversationID, r.UserID)
		}
		if r.Delivered == nil || !*r.Delivered {
			t.Errorf("row %d is undelivered; an undelivered user row is the pending-input queue", i)
		}
	}

	// Exactly {memory_id} on a memory row, and nothing at all on the task
	// context — a second copy of a column one hop away is a second thing that
	// can disagree with it.
	for i := 0; i < 2; i++ {
		if len(got[i].Metadata) != 1 || got[i].Metadata["memory_id"] != memories[i].ID {
			t.Errorf("memory row %d metadata = %v, want exactly {memory_id: %q}", i, got[i].Metadata, memories[i].ID)
		}
	}
	if got[2].Metadata != nil {
		t.Errorf("task-context metadata = %v, want none", got[2].Metadata)
	}
}

// TestMintOpeningRows_Gate pins the discriminator the mint keys on. A
// conversation that has only been handed control-plane rows has not opened —
// minting nothing for it would leave the run with no task context at all — and
// one that already carries a task-context row must not be opened twice.
func TestMintOpeningRows_Gate(t *testing.T) {
	delivered := true
	for _, tc := range []struct {
		name     string
		existing []domain.Message
		wantMint bool
	}{
		{name: "an empty transcript opens", wantMint: true},
		{
			name: "a claim-time notice is not an opening",
			existing: []domain.Message{
				{Role: "user", Subtype: domain.MessageSubtypeInjectionExecutorChanged, Content: "restored", Delivered: &delivered},
			},
			wantMint: true,
		},
		{
			name: "a queued follow-up is not an opening",
			existing: []domain.Message{
				{Role: "user", Content: "and update the README"},
			},
			wantMint: true,
		},
		{
			name: "a task-context row is",
			existing: []domain.Message{
				{Role: "user", Subtype: domain.MessageSubtypeInjectionTaskContext, Content: "<task_context/>", Delivered: &delivered},
			},
			wantMint: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got []domain.Message
			minted, err := mintOpeningRows(context.Background(), collectRows(&got), tc.existing,
				[]domain.TaskMemory{{ID: "m1", Content: "prior"}}, "conv-1", "user-1", "<task_context/>")
			if err != nil {
				t.Fatalf("mintOpeningRows: %v", err)
			}
			if minted != nil && len(minted) != len(got) {
				t.Fatalf("returned %d rows, wrote %d", len(minted), len(got))
			}
			if tc.wantMint && len(got) == 0 {
				t.Error("nothing minted; this conversation has no task context")
			}
			if !tc.wantMint && len(got) != 0 {
				t.Errorf("minted %d rows onto an already-opened conversation", len(got))
			}
		})
	}
}

// TestMintOpeningRows_StopsAtTheFirstInsertFailure: the rows go in one at a
// time and the first refusal is the answer. A fenced engagement must not write
// half an opening onto a conversation its successor owns.
func TestMintOpeningRows_StopsAtTheFirstInsertFailure(t *testing.T) {
	boom := errors.New("claim released")
	var wrote int
	insert := func(context.Context, *domain.Message) error {
		wrote++
		if wrote == 2 {
			return boom
		}
		return nil
	}
	_, err := mintOpeningRows(context.Background(), insert, nil,
		memoriesOfSize(t, 10, 10, 10), "conv-1", "user-1", "<task_context/>")
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the insert's own failure", err)
	}
	if wrote != 2 {
		t.Errorf("insert called %d times, want 2 — the refusal stops the mint", wrote)
	}
}

// TestSelectInjectedMemories_Budget pins the count-and-size cap: the newest
// that fit, contiguous, whole rows only, handed back oldest-first.
//
// Contiguous is the load-bearing part. Skipping an oversized memory for a
// smaller older one would make the envelope's "these are the most recent" a
// lie, so the walk stops at the first that does not fit rather than looking
// past it.
func TestSelectInjectedMemories_Budget(t *testing.T) {
	small := 1 << 10 // 1 KiB — six of these are under the byte cap
	for _, tc := range []struct {
		name  string
		sizes []int
		want  []string // memory ids, in mint order
	}{
		{name: "nothing to inject"},
		{
			name:  "everything fits",
			sizes: []int{small, small, small},
			want:  []string{"a", "b", "c"},
		},
		{
			name:  "a sixth memory is dropped, and it is the oldest that goes",
			sizes: []int{small, small, small, small, small, small},
			want:  []string{"b", "c", "d", "e", "f"},
		},
		{
			name:  "the byte cap cuts at the first that does not fit",
			sizes: []int{small, 30 << 10, 30 << 10},
			want:  []string{"c"},
		},
		{
			name:  "a newest memory over the cap on its own is not injected",
			sizes: []int{small, maxInjectedMemoryBytes + 1},
			want:  nil,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := selectInjectedMemories(memoriesOfSize(t, tc.sizes...))
			var ids []string
			size := 0
			for _, m := range got {
				ids = append(ids, m.ID)
				size += len(m.Content)
			}
			if strings.Join(ids, ",") != strings.Join(tc.want, ",") {
				t.Errorf("selected %v, want %v", ids, tc.want)
			}
			if len(got) > maxInjectedMemories || size > maxInjectedMemoryBytes {
				t.Errorf("selected %d rows / %d bytes, over the %d / %d budget", len(got), size, maxInjectedMemories, maxInjectedMemoryBytes)
			}
		})
	}
}

// TestSelectInjectedMemories_SkipsEmptyRows: a conversation that ended with
// nothing to remember files a row with no content, and it must neither be
// injected nor spend one of the five slots — the envelope would otherwise
// promise a prior conversation and deliver a blank block.
func TestSelectInjectedMemories_SkipsEmptyRows(t *testing.T) {
	memories := []domain.TaskMemory{
		{ID: "kept-1", Content: "something"},
		{ID: "empty", Source: domain.MemorySourceNone},
		{ID: "kept-2", Content: "something else"},
	}
	got := selectInjectedMemories(memories)
	if len(got) != 2 || got[0].ID != "kept-1" || got[1].ID != "kept-2" {
		t.Errorf("selected %+v, want the two non-empty memories oldest-first", got)
	}
}

// TestMintOpeningRows_MintsOldestFirstUnderTheBudget is the budget and the row
// order together: what the model reads is the newest five, in the order they
// were recorded, so a cumulative memory contract reads forwards.
func TestMintOpeningRows_MintsOldestFirstUnderTheBudget(t *testing.T) {
	var got []domain.Message
	if _, err := mintOpeningRows(context.Background(), collectRows(&got), nil,
		memoriesOfSize(t, 1, 1, 1, 1, 1, 1), "conv-1", "user-1", "<task_context/>"); err != nil {
		t.Fatalf("mintOpeningRows: %v", err)
	}
	if ids := memoryIDs(got); strings.Join(ids, ",") != "b,c,d,e,f" {
		t.Errorf("memory rows = %v, want the newest five minted oldest-first", ids)
	}
	if last := got[len(got)-1]; last.Subtype != domain.MessageSubtypeInjectionTaskContext {
		t.Errorf("last row subtype = %q, want the task context to close the opening", last.Subtype)
	}
}

// TestMintOpeningRows_OpensAheadOfWhatIsAlreadyThere is the ordering guarantee
// the whole change rests on. A conversation can collect rows before its
// engagement ever reaches the mint — a person types while it waits for an
// executor, a bring-up dies and leaves a stop note — and those rows already
// hold assembly positions the opening's own inserts land after.
//
// Without the hoist the model reads "and update the README" before the block
// that says which pull request that is.
func TestMintOpeningRows_OpensAheadOfWhatIsAlreadyThere(t *testing.T) {
	queued := domain.Message{ID: 7, Role: "user", Content: "and update the README"}
	stopNote := domain.Message{ID: 8, Role: "user", Subtype: domain.MessageSubtypeStopNote, Content: "the runtime would not start"}
	existing := []domain.Message{queued, stopNote}

	var got []domain.Message
	if _, err := mintOpeningRows(context.Background(), collectRows(&got), existing,
		memoriesOfSize(t, 10, 10), "conv-1", "user-1", "<task_context/>"); err != nil {
		t.Fatalf("mintOpeningRows: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("minted %d rows, want two memories and the task context", len(got))
	}

	// Every opening row sorts ahead of the rows already there, and they keep
	// their own order among themselves.
	prev := 0.0
	for i, r := range got {
		if r.Seq == nil {
			t.Fatalf("row %d carries no seq; appended to a non-empty transcript it assembles last", i)
		}
		if *r.Seq >= float64(queued.ID) {
			t.Errorf("row %d seq = %v, want below the earliest existing key %d", i, *r.Seq, queued.ID)
		}
		if i > 0 && *r.Seq <= prev {
			t.Errorf("row %d seq = %v, not after row %d's %v", i, *r.Seq, i-1, prev)
		}
		prev = *r.Seq
	}

	// And the assembly order the loop would read is the opening, then what was
	// waiting — the point of the exercise.
	assembled := append(append([]domain.Message{}, got...), existing...)
	sort.SliceStable(assembled, func(i, j int) bool {
		return assemblyKey(assembled[i]) < assemblyKey(assembled[j])
	})
	var order []string
	for _, r := range assembled {
		order = append(order, r.Subtype+"/"+r.Content)
	}
	want := []string{
		domain.MessageSubtypeInjectionMemory + "/" + strings.Repeat("x", 10),
		domain.MessageSubtypeInjectionMemory + "/" + strings.Repeat("x", 10),
		domain.MessageSubtypeInjectionTaskContext + "/<task_context/>",
		"/and update the README",
		domain.MessageSubtypeStopNote + "/the runtime would not start",
	}
	if strings.Join(order, "|") != strings.Join(want, "|") {
		t.Errorf("assembly order =\n  %v\nwant\n  %v", order, want)
	}
}

// TestMintOpeningRows_LeavesArrivalOrderAloneOnAFreshTranscript: the hoist is
// for the exception, not the rule. A conversation with nothing on it has no
// row to sort ahead of, and stamping one anyway would put a fractional key on
// the overwhelmingly common opening for no reason.
func TestMintOpeningRows_LeavesArrivalOrderAloneOnAFreshTranscript(t *testing.T) {
	var got []domain.Message
	if _, err := mintOpeningRows(context.Background(), collectRows(&got), nil,
		memoriesOfSize(t, 10), "conv-1", "user-1", "<task_context/>"); err != nil {
		t.Fatalf("mintOpeningRows: %v", err)
	}
	for i, r := range got {
		if r.Seq != nil {
			t.Errorf("row %d carries seq %v; arrival order is already its position", i, *r.Seq)
		}
	}
}

// TestMintOpeningRows_HoistsBelowAnExistingSeq: the earliest row is the one
// with the lowest assembly key, which is not the lowest id once anything has
// been sequenced — a compaction places rows by seq, and reading rows[0] or
// min(id) instead would mint the opening into the middle of the transcript.
func TestMintOpeningRows_HoistsBelowAnExistingSeq(t *testing.T) {
	sequenced := 2.5
	existing := []domain.Message{
		{ID: 40, Role: "user", Content: "arrived late, placed early", Seq: &sequenced},
		{ID: 9, Role: "user", Content: "lower id, later position"},
	}
	var got []domain.Message
	if _, err := mintOpeningRows(context.Background(), collectRows(&got), existing,
		nil, "conv-1", "user-1", "<task_context/>"); err != nil {
		t.Fatalf("mintOpeningRows: %v", err)
	}
	if len(got) != 1 || got[0].Seq == nil || *got[0].Seq >= sequenced {
		t.Errorf("task-context seq = %v, want below the earliest assembly key %v", got[0].Seq, sequenced)
	}
}

// TestConversationHasWork is the disposition's question, which is not the
// mint's. Only native mints an opening today, so a driven SDK conversation has
// to be recognised by the turn its model took — reading it as fresh fails a
// blueprint over a runtime that would not restart.
func TestConversationHasWork(t *testing.T) {
	for _, tc := range []struct {
		name string
		rows []domain.Message
		want bool
	}{
		{name: "nothing at all"},
		{
			name: "a queued follow-up is not work",
			rows: []domain.Message{{Role: "user", Content: "and update the README"}},
		},
		{
			name: "nor a stop note from a bring-up that died",
			rows: []domain.Message{{Role: "user", Subtype: domain.MessageSubtypeStopNote, Content: "would not start"}},
		},
		{
			name: "nor a claim-time notice",
			rows: []domain.Message{{Role: "user", Subtype: domain.MessageSubtypeInjectionExecutorChanged, Content: "restored"}},
		},
		{
			name: "a minted opening is",
			rows: []domain.Message{{Role: "user", Subtype: domain.MessageSubtypeInjectionTaskContext, Content: "<task_context/>"}},
			want: true,
		},
		{
			name: "and so is a turn on a transcript with no opening — the SDK's shape",
			rows: []domain.Message{
				{Role: "user", Content: "the human's opening ask"},
				{Role: "assistant", Content: "looking at the failing check now"},
			},
			want: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := conversationHasWork(tc.rows); got != tc.want {
				t.Errorf("conversationHasWork = %v, want %v", got, tc.want)
			}
		})
	}
}

// assemblyKey is COALESCE(seq, id) — the order a transcript read hands rows
// back in, reproduced here so a test can sort a set it built itself.
func assemblyKey(m domain.Message) float64 {
	if m.Seq != nil {
		return *m.Seq
	}
	return float64(m.ID)
}
