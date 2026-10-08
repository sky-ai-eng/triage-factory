package postgres_test

import (
	"database/sql"
	"testing"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
)

// TestEntityKey_Postgres runs the key half of the entity identity model with
// both pools on AdminDB, isolating the store's own behavior.
func TestEntityKey_Postgres(t *testing.T) {
	h := pgtest.Shared(t)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	dbtest.RunEntityKeyConformance(t, func(t *testing.T) (db.EntityStore, string) {
		t.Helper()
		h.Reset(t)
		orgID, _ := seedPgEntityOrg(t, h)
		return stores.Entities, orgID
	})
}

// TestEntityKey_Postgres_Claims runs the same suite inside one transaction
// carrying a member's claims, so every statement passes RLS: the insert arm and
// its read-back, the key lookup, and the occupied refusal — which must leave
// the transaction usable, since a raised violation would abort it.
func TestEntityKey_Postgres_Claims(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, userID := seedPgEntityOrg(t, h)
	if err := h.WithUser(t, userID, orgID, func(tx *sql.Tx) error {
		store := pgstore.NewForTx(tx, pgtest.SecretKey).Entities
		dbtest.RunEntityKeyConformance(t, func(t *testing.T) (db.EntityStore, string) {
			t.Helper()
			return store, orgID
		})
		return nil
	}); err != nil {
		t.Fatalf("WithUser: %v", err)
	}
}

// TestEntityIdentity_Postgres runs the identity writes against the Postgres
// backend. They are admin-pool by design, so both pools are AdminDB.
func TestEntityIdentity_Postgres(t *testing.T) {
	h := pgtest.Shared(t)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	dbtest.RunEntityIdentityConformance(t, func(t *testing.T) (db.Stores, string, dbtest.EntityIdentitySeeder) {
		t.Helper()
		h.Reset(t)
		orgID, userID, teamID := pgtest.SeedOrgWithUser(t, h, "identity")
		seed := dbtest.EntityIdentitySeeder{
			TeamID: teamID,
			Task: func(t *testing.T, entityID, suffix string) string {
				t.Helper()
				const eventType = "linear:issue:assigned"
				eventID := uuid.New().String()
				if _, err := h.AdminDB.Exec(`
					INSERT INTO events (id, org_id, entity_id, event_type, dedup_key, metadata_json)
					VALUES ($1, $2, $3, $4, $5, '{}'::jsonb)
				`, eventID, orgID, entityID, eventType, suffix); err != nil {
					t.Fatalf("seed event: %v", err)
				}
				taskID := uuid.New().String()
				if _, err := h.AdminDB.Exec(`
					INSERT INTO tasks (id, org_id, creator_user_id, team_id, visibility, entity_id,
					                   event_type, dedup_key, primary_event_id, status, scoring_status)
					VALUES ($1, $2, $3, $4, 'team', $5, $6, $7, $8, 'queued', 'pending')
				`, taskID, orgID, userID, teamID, entityID, eventType, suffix, eventID); err != nil {
					t.Fatalf("seed task: %v", err)
				}
				return taskID
			},
			RawActionURL: func(t *testing.T, dedupKey string) (string, string) {
				t.Helper()
				var u, cur string
				if err := h.AdminDB.QueryRow(
					`SELECT COALESCE(url, ''), COALESCE(current_url, '') FROM external_actions WHERE org_id = $1 AND dedup_key = $2`,
					orgID, dedupKey,
				).Scan(&u, &cur); err != nil {
					t.Fatalf("read action row %q: %v", dedupKey, err)
				}
				return u, cur
			},
			// The harness database is shared across tests, so the forced id is
			// cleared and the index rebuilt when the subtest ends.
			ForceExternalID: func(t *testing.T, entityID, externalID string) {
				t.Helper()
				if _, err := h.AdminDB.Exec(`DROP INDEX public.entities_identity`); err != nil {
					t.Fatalf("drop identity index: %v", err)
				}
				t.Cleanup(func() {
					if _, err := h.AdminDB.Exec(`UPDATE entities SET external_id = NULL WHERE id = $1`, entityID); err != nil {
						t.Errorf("clear forced external id: %v", err)
					}
					if _, err := h.AdminDB.Exec(`CREATE UNIQUE INDEX entities_identity ON public.entities USING btree (org_id, source, scope, external_id) WHERE (external_id IS NOT NULL)`); err != nil {
						t.Errorf("restore identity index: %v", err)
					}
				})
				if _, err := h.AdminDB.Exec(`UPDATE entities SET external_id = $1 WHERE id = $2`, externalID, entityID); err != nil {
					t.Fatalf("force external id: %v", err)
				}
			},
		}
		return stores, orgID, seed
	})
}
