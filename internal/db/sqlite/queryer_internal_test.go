package sqlite

import "testing"

// TestChunkIDs pins the IN-list chunker used by the batched run-list reads:
// empty → nil, an under-cap slice → one chunk aliasing the input, and an
// over-cap slice → consecutive chunks of inListChunkSize with a smaller tail,
// covering every element exactly once in order.
func TestChunkIDs(t *testing.T) {
	if got := chunkIDs(nil); got != nil {
		t.Errorf("chunkIDs(nil) = %v, want nil", got)
	}
	if got := chunkIDs([]string{}); got != nil {
		t.Errorf("chunkIDs(empty) = %v, want nil", got)
	}

	small := []string{"a", "b", "c"}
	chunks := chunkIDs(small)
	if len(chunks) != 1 || len(chunks[0]) != 3 {
		t.Fatalf("under-cap: got %d chunks (%v), want 1 of len 3", len(chunks), chunks)
	}

	// Exactly the cap → still one chunk.
	exact := make([]string, inListChunkSize)
	for i := range exact {
		exact[i] = "x"
	}
	if got := chunkIDs(exact); len(got) != 1 || len(got[0]) != inListChunkSize {
		t.Fatalf("exact-cap: got %d chunks, want 1 of len %d", len(got), inListChunkSize)
	}

	// Over the cap → ceil(n/cap) chunks, last one the remainder, total preserved.
	n := inListChunkSize*2 + 7
	big := make([]string, n)
	for i := range big {
		big[i] = string(rune('a' + i%26))
	}
	got := chunkIDs(big)
	if len(got) != 3 {
		t.Fatalf("over-cap: got %d chunks, want 3", len(got))
	}
	total := 0
	for i, c := range got {
		total += len(c)
		if i < len(got)-1 && len(c) != inListChunkSize {
			t.Errorf("chunk %d len = %d, want %d", i, len(c), inListChunkSize)
		}
	}
	if len(got[2]) != 7 {
		t.Errorf("tail chunk len = %d, want 7", len(got[2]))
	}
	if total != n {
		t.Errorf("covered %d ids, want %d", total, n)
	}
}
