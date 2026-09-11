package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
)

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

// TestInTx_CanceledCtxSurfacesAsCanceled pins the disconnect contract on the
// store-internal transaction helper: a ctx that dies inside fn surfaces as
// context.Canceled on the branch where the stdlib's rollback goroutine beat
// the commit. See dbtest.WaitTxDone for why the body waits that race out
// instead of running it.
func TestInTx_CanceledCtxSurfacesAsCanceled(t *testing.T) {
	conn, err := sql.Open("sqlite", db.TestDSNMemory)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	err = inTx(ctx, conn, func(q queryer) error {
		if _, err := q.ExecContext(ctx, `SELECT 1`); err != nil {
			return err
		}
		cancel()
		return dbtest.WaitTxDone(t, func(live context.Context) error {
			_, err := q.ExecContext(live, `SELECT 1`)
			return err
		})
	})
	if err == nil {
		t.Fatal("inTx committed under a canceled ctx")
	}
	if !errors.Is(err, sql.ErrTxDone) {
		t.Fatalf("test did not reach the ErrTxDone branch it exists to pin; got %v", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("errors.Is(err, context.Canceled) = false; got %v", err)
	}
}
