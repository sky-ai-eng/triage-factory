package delegate

import (
	"context"
	"errors"
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
