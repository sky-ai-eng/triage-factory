package postgres_test

import (
	"database/sql"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TestRateLimitReadingStore_Postgres_Conformance runs the shared contract
// against the Postgres impl, with both pools on the admin connection as every
// store suite wires them. The RLS read is covered separately below.
func TestRateLimitReadingStore_Postgres_Conformance(t *testing.T) {
	dbtest.RunRateLimitReadingStoreConformance(t, func(t *testing.T) (db.RateLimitReadingStore, string) {
		h := pgtest.Shared(t)
		h.Reset(t)
		orgID, _, _ := pgtest.SeedOrgWithUser(t, h, "ratelimits")
		return pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey).RateLimitReadings, orgID
	})
}

// TestRateLimitReadingStore_Postgres_MemberReadIsOrgScoped pins the read the
// route serves: through the app pool under a member's claims, the member sees
// their own org's readings and none of another org's, which the flusher wrote
// to the same table on the admin pool.
func TestRateLimitReadingStore_Postgres_MemberReadIsOrgScoped(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgA, userA, _ := pgtest.SeedOrgWithUser(t, h, "alice")
	orgB, _, _ := pgtest.SeedOrgWithUser(t, h, "bob")

	now := time.Now().UTC().Truncate(time.Second)
	remaining, limit := 10, 5000
	reset := now.Add(time.Hour)
	reading := func(orgID string) domain.RateLimitReading {
		return domain.RateLimitReading{
			OrgID: orgID, Upstream: "github", Credential: "pat", Bucket: "core",
			Remaining: &remaining, Limit: &limit, ResetAt: &reset, ObservedAt: now,
		}
	}
	admin := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey).RateLimitReadings
	if err := admin.UpsertSystem(t.Context(), []domain.RateLimitReading{reading(orgA), reading(orgB)}); err != nil {
		t.Fatalf("UpsertSystem: %v", err)
	}

	if err := h.WithUser(t, userA, orgA, func(tx *sql.Tx) error {
		store := pgstore.NewForTx(tx, pgtest.SecretKey).RateLimitReadings
		got, err := store.List(t.Context(), orgA, now)
		if err != nil {
			t.Fatalf("List own org: %v", err)
		}
		if len(got) != 1 || got[0].OrgID != orgA {
			t.Errorf("member's List of their own org = %+v, want orgA's one reading", got)
		}
		other, err := store.List(t.Context(), orgB, now)
		if err != nil {
			t.Fatalf("List other org: %v", err)
		}
		if len(other) != 0 {
			t.Errorf("member of orgA read orgB's readings %+v", other)
		}
		return nil
	}); err != nil {
		t.Fatalf("WithUser: %v", err)
	}
}
