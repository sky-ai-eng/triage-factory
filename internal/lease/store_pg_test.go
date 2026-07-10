package lease_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	"github.com/sky-ai-eng/triage-factory/internal/lease"
)

// TestPostgresStore_BootstrapRace pins the pre-made decision verbatim:
// concurrent EnsureRow calls (simulating two pods racing to seed the row
// on first boot) converge on exactly one row, with no error from the
// INSERT ... ON CONFLICT DO NOTHING race itself.
func TestPostgresStore_BootstrapRace(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	store := lease.NewPostgresStore(h.AdminDB)
	ctx := context.Background()

	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := store.EnsureRow(ctx, lease.BackgroundBrainName); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent EnsureRow: %v", err)
	}

	var count int
	if err := h.AdminDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM leases WHERE name = $1`, lease.BackgroundBrainName).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("row count = %d, want exactly 1", count)
	}
}

// TestPostgresStore_AcquireRenewMutualExclusion pins the acquire/renew SQL
// semantics against a real Postgres: a fresh acquisition mints term=1 and
// blocks a second acquirer while renewed; an explicit Renew loss (someone
// else already took over) reports renewed=false without erroring.
func TestPostgresStore_AcquireRenewMutualExclusion(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	store := lease.NewPostgresStore(h.AdminDB)
	ctx := context.Background()
	const name = lease.BackgroundBrainName
	const ttl = 200 * time.Millisecond

	if err := store.EnsureRow(ctx, name); err != nil {
		t.Fatal(err)
	}

	term, acquired, err := store.TryAcquire(ctx, name, "pod-a", ttl)
	if err != nil {
		t.Fatal(err)
	}
	if !acquired || term != 1 {
		t.Fatalf("first acquire: acquired=%v term=%d, want true/1", acquired, term)
	}

	// A second pod can't acquire while pod-a's lease is fresh.
	if _, acquired, err := store.TryAcquire(ctx, name, "pod-b", ttl); err != nil || acquired {
		t.Fatalf("second acquire while fresh: acquired=%v err=%v, want false/nil", acquired, err)
	}

	// pod-a renews successfully.
	if renewed, err := store.Renew(ctx, name, "pod-a", term); err != nil || !renewed {
		t.Fatalf("pod-a renew: renewed=%v err=%v, want true/nil", renewed, err)
	}

	// Once the TTL elapses without a renewal, pod-b can take over — and
	// pod-a's subsequent Renew against its now-stale term reports false.
	time.Sleep(ttl + 50*time.Millisecond)
	term2, acquired, err := store.TryAcquire(ctx, name, "pod-b", ttl)
	if err != nil || !acquired || term2 != 2 {
		t.Fatalf("pod-b takeover: acquired=%v term=%d err=%v, want true/2/nil", acquired, term2, err)
	}
	if renewed, err := store.Renew(ctx, name, "pod-a", term); err != nil || renewed {
		t.Fatalf("stale pod-a renew: renewed=%v err=%v, want false/nil", renewed, err)
	}

	holderID, readTerm, err := store.Read(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	if holderID != "pod-b" || readTerm != 2 {
		t.Fatalf("Read = (%q, %d), want (pod-b, 2)", holderID, readTerm)
	}
}

// TestPostgresStore_ReadUnknownName pins the "no row yet" contract: Read
// returns zero values with no error, not sql.ErrNoRows leaking out.
func TestPostgresStore_ReadUnknownName(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	store := lease.NewPostgresStore(h.AdminDB)
	holderID, term, err := store.Read(context.Background(), "no-such-lease")
	if err != nil {
		t.Fatal(err)
	}
	if holderID != "" || term != 0 {
		t.Fatalf("Read(unknown) = (%q, %d), want zero values", holderID, term)
	}
}
