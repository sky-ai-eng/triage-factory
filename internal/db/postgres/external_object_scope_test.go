package postgres_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TestExternalObjectScope_Postgres runs the shared scope conformance suite for
// artifacts and the audit ledger against the Postgres backend. Both pools are
// wired against AdminDB, so the suite exercises behavior independent of the
// auth path; the claims-carrying run below covers RLS.
func TestExternalObjectScope_Postgres(t *testing.T) {
	h := pgtest.Shared(t)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	dbtest.RunExternalObjectScopeConformance(t, func(t *testing.T) (db.Stores, string, string) {
		t.Helper()
		h.Reset(t)
		orgID, _, teamID := pgtest.SeedOrgWithUser(t, h, "scope")
		return stores, orgID, teamID
	})
}

// TestCommentArtifacts_Postgres runs the shared comment-artifact conformance
// suite against the Postgres backend, both pools wired against AdminDB like the
// scope suite above.
func TestCommentArtifacts_Postgres(t *testing.T) {
	h := pgtest.Shared(t)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	dbtest.RunCommentArtifactConformance(t, func(t *testing.T) (db.Stores, string, string) {
		t.Helper()
		h.Reset(t)
		orgID, _, teamID := pgtest.SeedOrgWithUser(t, h, "comments")
		return stores, orgID, teamID
	})
}

// TestArtifactUpsert_Postgres runs the shared artifact upsert conformance suite
// against the Postgres backend, both pools wired against AdminDB like the scope
// suite above.
func TestArtifactUpsert_Postgres(t *testing.T) {
	h := pgtest.Shared(t)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	dbtest.RunArtifactUpsertConformance(t, func(t *testing.T) (db.Stores, string, string) {
		t.Helper()
		h.Reset(t)
		orgID, _, teamID := pgtest.SeedOrgWithUser(t, h, "upsert")
		return stores, orgID, teamID
	})
}

// TestArtifactStore_Postgres_ReturnedRowUnderClaims runs the artifact upsert
// through the app pool inside a claims-carrying transaction, where the
// artifacts RLS policies apply. On a BYPASSRLS connection RETURNING hands back
// its row unconditionally; here the insert arm must pass artifacts_insert and
// the conflict arm must see the row it updates (artifacts_update and
// artifacts_select), or the write returns nothing. Both arms are checked
// against a point read under the same claims, and the scope comes back on
// each.
func TestArtifactStore_Postgres_ReturnedRowUnderClaims(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, userID, teamID := pgtest.SeedOrgWithUser(t, h, "claims")
	conversationID := seedPgArtifactConversation(t, h, orgID, teamID, userID)
	ctx := context.Background()

	if err := h.WithUser(t, userID, orgID, func(tx *sql.Tx) error {
		store := pgstore.NewForTx(tx, pgtest.SecretKey).Artifacts
		in := domain.Artifact{
			ConversationID: conversationID, TeamID: teamID,
			Provider: domain.ArtifactProviderGitHub, Kind: domain.ArtifactKindPullRequest,
			Scope: dbtest.TestOtherGitHubHost, Target: domain.PullRequestTarget("octo/api", 3), ExternalID: "3",
			URL:    dbtest.TestOtherGitHubHost + "/octo/api/pull/3",
			NodeID: "PR_kw3", State: domain.ArtifactStatePRDraft, DedupKey: domain.PullRequestDedupKey("PR_kw3"),
		}
		read := func(id string) func() (*domain.Artifact, error) {
			return func() (*domain.Artifact, error) { return store.Get(ctx, orgID, id) }
		}

		inserted, err := store.Upsert(ctx, orgID, in)
		if err != nil {
			t.Fatalf("Upsert (insert) under claims: %v", err)
		}
		dbtest.AssertWriteReturnedStoredRow(t, "Upsert (insert)", inserted, read(inserted.ID))
		if inserted.Scope != dbtest.TestOtherGitHubHost {
			t.Errorf("Upsert (insert) returned scope %q, want %q", inserted.Scope, dbtest.TestOtherGitHubHost)
		}

		in.State = domain.ArtifactStatePROpen
		updated, err := store.Upsert(ctx, orgID, in)
		if err != nil {
			t.Fatalf("Upsert (conflict) under claims: %v", err)
		}
		dbtest.AssertWriteReturnedStoredRow(t, "Upsert (conflict)", updated, read(inserted.ID))
		if updated.ID != inserted.ID || updated.State != domain.ArtifactStatePROpen || updated.Scope != dbtest.TestOtherGitHubHost {
			t.Errorf("Upsert (conflict) returned %+v, want row %s moved to open in its scope", updated, inserted.ID)
		}
		return nil
	}); err != nil {
		t.Fatalf("WithUser: %v", err)
	}
}

// TestExternalActionNodeID_Postgres runs the shared ledger node-id conformance
// suite against the Postgres backend, both pools wired against AdminDB like the
// scope suite above.
func TestExternalActionNodeID_Postgres(t *testing.T) {
	dbtest.RunExternalActionNodeIDConformance(t, pgExternalActionFixture(pgtest.Shared(t)))
}

// TestExternalActionReferences_Postgres runs the shared ledger reference
// conformance suite against the Postgres backend, on the node-id suite's
// fixture. The foreign keys that refuse a dangling reference are composite
// with org_id, so they are what holds a reference to the org's own rows.
func TestExternalActionReferences_Postgres(t *testing.T) {
	dbtest.RunExternalActionReferenceConformance(t, pgExternalActionFixture(pgtest.Shared(t)))
}

// pgExternalActionFixture builds a fresh org per subtest with a team and a
// conversation in it, both pools wired against AdminDB.
func pgExternalActionFixture(h *pgtest.Harness) dbtest.ExternalActionNodeIDFactory {
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	return func(t *testing.T) dbtest.ExternalActionNodeIDFixture {
		t.Helper()
		h.Reset(t)
		orgID, userID, teamID := pgtest.SeedOrgWithUser(t, h, "nodeid")
		return dbtest.ExternalActionNodeIDFixture{
			Stores:         stores,
			OrgID:          orgID,
			TeamID:         teamID,
			ConversationID: seedPgArtifactConversation(t, h, orgID, teamID, userID),
			BackdateAction: func(t *testing.T, actionID string, age time.Duration) {
				t.Helper()
				if _, err := h.AdminDB.Exec(
					`UPDATE external_actions SET occurred_at = now() - make_interval(secs => $2) WHERE id = $1`,
					actionID, age.Seconds()); err != nil {
					t.Fatalf("backdate occurred_at of %s: %v", actionID, err)
				}
			},
		}
	}
}
