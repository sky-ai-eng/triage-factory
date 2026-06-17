package postgres_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TestEventStore_Postgres runs the shared conformance suite against
// the Postgres EventStore impl. Wires both pools against AdminDB
// (BYPASSRLS) so the behavior tests stay independent of the auth
// path; the cross-org isolation + RLS tests below exercise the
// org_id filter and the policy directly.
func TestEventStore_Postgres(t *testing.T) {
	h := pgtest.Shared(t)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)

	dbtest.RunEventStoreConformance(t, func(t *testing.T) (db.EventStore, string, dbtest.EventStoreSeeder) {
		t.Helper()
		h.Reset(t)
		orgID := seedPgOrgForEvents(t, h)
		seed := dbtest.EventStoreSeeder{
			Entity: func(t *testing.T, suffix string) string {
				t.Helper()
				return seedPgEntityForEvents(t, h, orgID, suffix)
			},
		}
		return stores.Events, orgID, seed
	})
}

// TestEventStore_Postgres_CrossOrgLeakage pins the defense-in-depth
// org_id filter on every read + write path. RLS via events_all also
// enforces this in production; the org_id = $N clause in each query
// is the belt to RLS's suspenders that fires regardless of whether
// the admin pool bypasses RLS.
func TestEventStore_Postgres_CrossOrgLeakage(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)

	orgA := seedPgOrgForEvents(t, h)
	orgB := seedPgOrgForEvents(t, h)
	ctx := context.Background()

	entityA := seedPgEntityForEvents(t, h, orgA, "cross-org-A")
	// Record an event in orgA via the admin-pool RecordSystem path —
	// the router's SKY-305 call site uses this admin variant.
	eid := entityA
	evtID, err := stores.Events.RecordSystem(ctx, orgA, domain.Event{
		EntityID:     &eid,
		EventType:    domain.EventGitHubPRCICheckPassed,
		MetadataJSON: `{"check_name":"build"}`,
	})
	if err != nil {
		t.Fatalf("RecordSystem orgA: %v", err)
	}

	// Latest filtered by orgB must NOT see orgA's row.
	got, err := stores.Events.LatestForEntityTypeAndDedupKey(ctx, orgB, entityA, domain.EventGitHubPRCICheckPassed, "")
	if err != nil {
		t.Fatalf("Latest cross-org: %v", err)
	}
	if got != nil {
		t.Errorf("Latest under orgB returned orgA's row %s", got.ID)
	}

	// GetMetadataSystem filtered by orgB must also miss.
	meta, err := stores.Events.GetMetadataSystem(ctx, orgB, evtID)
	if err != nil {
		t.Fatalf("GetMetadataSystem cross-org: %v", err)
	}
	if meta != "" {
		t.Errorf("GetMetadataSystem under orgB returned %q; want empty", meta)
	}
}

// TestEventStore_Postgres_RLS_AppPoolRequiresClaims pins the
// events_all policy on the app pool: a Record without
// request.jwt.claims set is refused (tf.current_org_id() returns NULL
// → policy predicate fails), and the same Record inside a claims-set
// tx for the matching org succeeds while a tx claims-set for a
// different org is denied.
func TestEventStore_Postgres_RLS_AppPoolRequiresClaims(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	ctx := context.Background()

	orgA := seedPgOrgForEvents(t, h)
	orgB := seedPgOrgForEvents(t, h)
	aliceA := mustOwnerUserForOrg(t, h, orgA)
	bobB := mustOwnerUserForOrg(t, h, orgB)
	entityA := seedPgEntityForEvents(t, h, orgA, "rls-app")
	eid := entityA

	// Subtest 1: without claims (app pool, no WithUser), Record is
	// refused — tf.current_org_id() is NULL inside a fresh tf_app
	// connection so the events_all USING/WITH CHECK predicate fails.
	t.Run("no_claims_refuses_record", func(t *testing.T) {
		// Construct a Store wired only against AppDB so Record routes
		// through the RLS-active pool with no claims set.
		stores := pgstore.New(h.AppDB, h.AppDB, pgtest.SecretKey)
		_, err := stores.Events.Record(ctx, orgA, domain.Event{
			EntityID:  &eid,
			EventType: domain.EventGitHubPROpened,
		})
		if err == nil {
			t.Fatal("Record without JWT claims should have been refused by RLS")
		}
		// Postgres surfaces the row-level-security violation as code
		// 42501 (insufficient_privilege). Pin the code so a noisy
		// error message change doesn't silently weaken the test.
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
			t.Errorf("expected RLS violation (42501), got %v", err)
		}
	})

	// Subtest 2: inside WithUser(aliceA, orgA, ...) — claims match the
	// row's org — Record succeeds and the row is visible to the same
	// claims principal.
	t.Run("matching_org_claims_succeeds", func(t *testing.T) {
		err := h.WithUser(t, aliceA, orgA, func(tx *sql.Tx) error {
			txStores := pgstore.NewForTx(tx, pgtest.SecretKey)
			id, err := txStores.Events.Record(ctx, orgA, domain.Event{
				EntityID:  &eid,
				EventType: domain.EventGitHubPRMerged,
			})
			if err != nil {
				return fmt.Errorf("Record: %w", err)
			}
			got, err := txStores.Events.LatestForEntityTypeAndDedupKey(ctx, orgA, entityA, domain.EventGitHubPRMerged, "")
			if err != nil {
				return fmt.Errorf("Latest: %w", err)
			}
			if got == nil || got.ID != id {
				return fmt.Errorf("Latest = %v, want id %s", got, id)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("matching claims: %v", err)
		}
	})

	// Subtest 3: inside WithUser(bobB, orgB, ...) — claims point at a
	// different org — Record into orgA's events row is refused.
	t.Run("cross_org_claims_denied", func(t *testing.T) {
		err := h.WithUser(t, bobB, orgB, func(tx *sql.Tx) error {
			txStores := pgstore.NewForTx(tx, pgtest.SecretKey)
			_, err := txStores.Events.Record(ctx, orgA, domain.Event{
				EntityID:  &eid,
				EventType: domain.EventGitHubPRClosed,
			})
			return err
		})
		if err == nil {
			t.Fatal("Record into mismatched org should be refused by RLS")
		}
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code != "42501" {
			t.Errorf("expected RLS violation (42501), got code %s: %v", pgErr.Code, err)
		}
	})
}

// seedPgOrgForEvents stages (user, org, org_membership, team) so
// events.org_id FK satisfies and the entity rows the conformance
// suite seeds against this org pass the entities_all RLS predicate
// when accessed under the org's owner.
func seedPgOrgForEvents(t *testing.T, h *pgtest.Harness) string {
	t.Helper()
	orgID := uuid.New().String()
	userID := uuid.New().String()
	email := fmt.Sprintf("events-conf-%s@test.local", userID[:8])

	h.SeedAuthUser(t, userID, email)
	if _, err := h.AdminDB.Exec(
		`INSERT INTO users (id, display_name) VALUES ($1, $2)`,
		userID, "Events Conformance User",
	); err != nil {
		t.Fatalf("seed users: %v", err)
	}
	if _, err := h.AdminDB.Exec(
		`INSERT INTO orgs (id, name, slug, owner_user_id) VALUES ($1, $2, $3, $4)`,
		orgID, "Events Org "+orgID[:8], "events-"+orgID[:8], userID,
	); err != nil {
		t.Fatalf("seed org: %v", err)
	}
	if _, err := h.AdminDB.Exec(
		`INSERT INTO org_memberships (org_id, user_id, role) VALUES ($1, $2, 'owner')`,
		orgID, userID,
	); err != nil {
		t.Fatalf("seed org_membership: %v", err)
	}
	seedPgDefaultTeam(t, h, orgID, userID)
	return orgID
}

// seedPgEntityForEvents inserts an active GitHub PR entity in the
// given org. Direct INSERT (not EntityStore) so the conformance
// fixture path is schema-coupled and short — matches the SwipeStore
// / RepoStore seed pattern.
func seedPgEntityForEvents(t *testing.T, h *pgtest.Harness, orgID, suffix string) string {
	t.Helper()
	id := uuid.New().String()
	now := time.Now().UTC()
	sourceID := fmt.Sprintf("events-conf-%s-%s-%d", orgID[:8], suffix, now.UnixNano())
	if _, err := h.AdminDB.Exec(`
		INSERT INTO entities (id, org_id, source, source_id, kind, title, url, snapshot_json, created_at, state)
		VALUES ($1, $2, 'github', $3, 'pr', 'Events Conformance', 'https://example/x', '{}'::jsonb, $4, 'active')
	`, id, orgID, sourceID, now); err != nil {
		t.Fatalf("seed entity: %v", err)
	}
	return id
}
