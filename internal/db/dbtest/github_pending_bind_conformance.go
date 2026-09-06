package dbtest

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// GitHubPendingBindBackend is what a per-backend test file hands to
// RunGitHubPendingBindConformance: the store, an org the records may hang off
// (the FK is real in both dialects), and a user id to attribute them to.
type GitHubPendingBindBackend struct {
	Store  db.GitHubPendingBindStore
	OrgID  string
	UserID string
}

// GitHubPendingBindFactory returns a fresh, isolated backend per subtest so
// records don't leak between them.
type GitHubPendingBindFactory func(t *testing.T) GitHubPendingBindBackend

// RunGitHubPendingBindConformance is the shared assertion suite for the
// pending-bind record, in both dialects.
//
// The record is a security control, so the suite is written as the properties
// the ceremony leans on rather than as coverage of two methods:
//
//   - What comes back is what the callback needs. The consume hands back the
//     org and the initiating user, because those are the facts the callback
//     cannot get from anywhere else — GitHub's redirect names neither.
//   - Single use, under concurrency. Exactly one of two callbacks racing on one
//     record may proceed. This is why consumption is a conditional update and
//     not a read followed by a write, and it is the assertion that would fail
//     if someone "simplified" it back.
//   - Expiry is enforced by the store, not by the caller. A record past its
//     expires_at is unspendable even though its row is still there.
//   - Absent, expired and already-consumed are one answer. The caller refuses
//     identically for all three, so the store hands back one nil rather than
//     three distinguishable outcomes an unauthenticated caller could probe.
func RunGitHubPendingBindConformance(t *testing.T, mk GitHubPendingBindFactory) {
	t.Helper()
	ctx := context.Background()

	// create writes one record with the given lifetime relative to now.
	create := func(t *testing.T, be GitHubPendingBindBackend, hash string, ttl time.Duration) domain.GitHubPendingBind {
		t.Helper()
		now := time.Now().UTC()
		stored, err := be.Store.CreateSystem(ctx, domain.GitHubPendingBind{
			NonceHash: hash,
			OrgID:     be.OrgID,
			UserID:    be.UserID,
			CreatedAt: now,
			ExpiresAt: now.Add(ttl),
		})
		if err != nil {
			t.Fatalf("CreateSystem(%q): %v", hash, err)
		}
		return stored
	}

	t.Run("CreateReturnsTheStoredRow", func(t *testing.T) {
		be := mk(t)
		stored := create(t, be, "hash-create", db.GitHubPendingBindTTL)

		if stored.NonceHash != "hash-create" {
			t.Errorf("NonceHash = %q, want %q", stored.NonceHash, "hash-create")
		}
		if stored.OrgID != be.OrgID {
			t.Errorf("OrgID = %q, want %q", stored.OrgID, be.OrgID)
		}
		if stored.UserID != be.UserID {
			t.Errorf("UserID = %q, want %q", stored.UserID, be.UserID)
		}
		if stored.CreatedAt.IsZero() || stored.ExpiresAt.IsZero() {
			t.Errorf("timestamps not returned: created=%v expires=%v", stored.CreatedAt, stored.ExpiresAt)
		}
		if !stored.ConsumedAt.IsZero() {
			t.Errorf("ConsumedAt = %v on a fresh record, want zero", stored.ConsumedAt)
		}
	})

	t.Run("ConsumeReturnsTheOrgAndUserTheCallbackNeeds", func(t *testing.T) {
		be := mk(t)
		create(t, be, "hash-consume", db.GitHubPendingBindTTL)

		got, err := be.Store.ConsumeSystem(ctx, "hash-consume", time.Now().UTC())
		if err != nil {
			t.Fatalf("ConsumeSystem: %v", err)
		}
		if got == nil {
			t.Fatal("ConsumeSystem returned nil for a live record")
		}
		if got.OrgID != be.OrgID || got.UserID != be.UserID {
			t.Errorf("consumed record = (org %q, user %q), want (%q, %q)",
				got.OrgID, got.UserID, be.OrgID, be.UserID)
		}
		if got.ConsumedAt.IsZero() {
			t.Error("ConsumedAt is zero on the returned row; the marker must be stamped by the same statement")
		}
	})

	t.Run("ConsumeReturnsTheLegAndTheAccountTheCeremonyWasStartedFor", func(t *testing.T) {
		// The callback learns which return it is waiting for and which
		// account it is about from this row alone — neither rides a URL — so
		// the consume has to hand both back exactly.
		be := mk(t)
		now := time.Now().UTC()
		for _, rec := range []domain.GitHubPendingBind{
			{NonceHash: "hash-authorize", Leg: domain.GitHubBindLegAuthorize, AccountLogin: "Acme-Corp"},
			{NonceHash: "hash-install", Leg: domain.GitHubBindLegInstall, AccountLogin: "acme-corp"},
		} {
			rec.OrgID, rec.UserID = be.OrgID, be.UserID
			rec.CreatedAt, rec.ExpiresAt = now, now.Add(db.GitHubPendingBindTTL)
			if _, err := be.Store.CreateSystem(ctx, rec); err != nil {
				t.Fatalf("CreateSystem(%s): %v", rec.NonceHash, err)
			}
			got, err := be.Store.ConsumeSystem(ctx, rec.NonceHash, now)
			if err != nil || got == nil {
				t.Fatalf("ConsumeSystem(%s) = %v, %v", rec.NonceHash, got, err)
			}
			if got.Leg != rec.Leg || got.AccountLogin != rec.AccountLogin {
				t.Errorf("consumed %s = (leg %q, account %q), want (%q, %q) back verbatim",
					rec.NonceHash, got.Leg, got.AccountLogin, rec.Leg, rec.AccountLogin)
			}
		}
	})

	t.Run("SecondConsumeFindsNothing", func(t *testing.T) {
		be := mk(t)
		create(t, be, "hash-once", db.GitHubPendingBindTTL)

		first, err := be.Store.ConsumeSystem(ctx, "hash-once", time.Now().UTC())
		if err != nil || first == nil {
			t.Fatalf("first ConsumeSystem = (%v, %v), want a record", first, err)
		}
		second, err := be.Store.ConsumeSystem(ctx, "hash-once", time.Now().UTC())
		if err != nil {
			t.Fatalf("second ConsumeSystem: %v", err)
		}
		if second != nil {
			t.Errorf("second ConsumeSystem returned %+v, want nil — the record is single-use", second)
		}
	})

	t.Run("ConcurrentConsumesElectExactlyOneWinner", func(t *testing.T) {
		be := mk(t)
		create(t, be, "hash-race", db.GitHubPendingBindTTL)

		// Two callbacks on one record, released together. A read-then-write
		// consume passes the sequential subtest above and fails here, which is
		// the point of running it.
		const racers = 2
		var (
			start sync.WaitGroup
			done  sync.WaitGroup
			mu    sync.Mutex
			wins  int
			errs  []error
		)
		start.Add(1)
		for i := 0; i < racers; i++ {
			done.Add(1)
			go func() {
				defer done.Done()
				start.Wait()
				got, err := be.Store.ConsumeSystem(ctx, "hash-race", time.Now().UTC())
				mu.Lock()
				defer mu.Unlock()
				if err != nil {
					errs = append(errs, err)
					return
				}
				if got != nil {
					wins++
				}
			}()
		}
		start.Done()
		done.Wait()

		for _, err := range errs {
			t.Errorf("ConsumeSystem under contention: %v", err)
		}
		if wins != 1 {
			t.Errorf("%d of %d concurrent consumes succeeded, want exactly 1", wins, racers)
		}
	})

	t.Run("ExpiredRecordIsUnspendable", func(t *testing.T) {
		be := mk(t)
		// Already expired when written: the store, not the caller, is what
		// refuses it.
		create(t, be, "hash-expired", -time.Minute)

		got, err := be.Store.ConsumeSystem(ctx, "hash-expired", time.Now().UTC())
		if err != nil {
			t.Fatalf("ConsumeSystem: %v", err)
		}
		if got != nil {
			t.Errorf("ConsumeSystem returned %+v for an expired record, want nil", got)
		}
	})

	t.Run("UnknownNonceIsTheSameNilAsAnExpiredOne", func(t *testing.T) {
		be := mk(t)
		got, err := be.Store.ConsumeSystem(ctx, "hash-never-existed", time.Now().UTC())
		if err != nil {
			t.Fatalf("ConsumeSystem: %v", err)
		}
		if got != nil {
			t.Errorf("ConsumeSystem returned %+v for an unknown nonce, want nil", got)
		}
	})

	t.Run("ConsumeIsScopedToItsOwnNonce", func(t *testing.T) {
		be := mk(t)
		create(t, be, "hash-a", db.GitHubPendingBindTTL)
		create(t, be, "hash-b", db.GitHubPendingBindTTL)

		if _, err := be.Store.ConsumeSystem(ctx, "hash-a", time.Now().UTC()); err != nil {
			t.Fatalf("ConsumeSystem(hash-a): %v", err)
		}
		// Spending one ceremony must not spend another the same admin started
		// in a second tab.
		got, err := be.Store.ConsumeSystem(ctx, "hash-b", time.Now().UTC())
		if err != nil {
			t.Fatalf("ConsumeSystem(hash-b): %v", err)
		}
		if got == nil {
			t.Error("consuming one record spent another; the update must key on nonce_hash alone")
		}
	})

	t.Run("PruneSweepsRecordsLongPastExpiry", func(t *testing.T) {
		be := mk(t)
		// Old enough for the prune's grace period, so the sweep the next
		// consume piggybacks on must remove it.
		create(t, be, "hash-ancient", -(db.GitHubPendingBindPruneAge + time.Hour))
		create(t, be, "hash-live", db.GitHubPendingBindTTL)

		if _, err := be.Store.ConsumeSystem(ctx, "hash-live", time.Now().UTC()); err != nil {
			t.Fatalf("ConsumeSystem(hash-live): %v", err)
		}
		// The pruned row is unspendable either way — it was already expired —
		// so what is asserted is that re-creating its nonce is possible, which
		// only holds once the row is gone (nonce_hash is the primary key).
		if _, err := be.Store.CreateSystem(ctx, domain.GitHubPendingBind{
			NonceHash: "hash-ancient",
			OrgID:     be.OrgID,
			UserID:    be.UserID,
			CreatedAt: time.Now().UTC(),
			ExpiresAt: time.Now().UTC().Add(db.GitHubPendingBindTTL),
		}); err != nil {
			t.Errorf("re-creating a pruned nonce failed, so the prune did not sweep it: %v", err)
		}
	})
}
