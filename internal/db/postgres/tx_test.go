package postgres_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestSyntheticClaimsWithTx_Postgres_RunsFn_UnderClaimsAndRole pins
// the SKY-296 helper's basic contract: it opens a tx on the app pool,
// elevates to tf_app, sets request.jwt.claims from caller-supplied
// (orgID, userID), and runs fn against tx-bound stores.
//
// A store call inside fn that requires both role elevation (tf_app
// has the SELECT grants, authenticator doesn't) and a claims-set
// helper (RLS policies gate on tf.current_user_id() / current_org_id())
// would fail at either layer if the helper skipped one of them. The
// smoke test here exercises both axes by calling UsersStore.GetGitHubLogin
// inside fn — the SELECT runs through the tx, which means it must
// pass role + RLS to return without error.
func TestSyntheticClaimsWithTx_Postgres_RunsFn_UnderClaimsAndRole(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, userID := seedSyntheticClaimsOrg(t, h, "sc-runs-fn")

	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)

	called := false
	if err := stores.Tx.SyntheticClaimsWithTx(context.Background(), orgID, userID, func(tx db.TxStores) error {
		called = true
		if _, err := tx.Users.GetGitHubLogin(context.Background(), userID, "https://github.com"); err != nil {
			return fmt.Errorf("GetGitHubLogin under synthetic claims: %w", err)
		}
		return nil
	}); err != nil {
		t.Fatalf("SyntheticClaimsWithTx: %v", err)
	}
	if !called {
		t.Fatal("fn was not invoked")
	}
}

// TestSyntheticClaimsWithTx_Postgres_CrossOrgLeakage confirms that
// even when fn forges synthetic claims, the bound tx still runs under
// tf_app + RLS — a fn that queries orgB's data while the claims pair
// names orgA gets zero rows back. This is the auth-correctness pin:
// the helper doesn't accidentally bypass RLS just because the claims
// came from a non-request source.
func TestSyntheticClaimsWithTx_Postgres_CrossOrgLeakage(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)

	orgA, userA := seedSyntheticClaimsOrg(t, h, "sc-A")
	orgB, _ := seedSyntheticClaimsOrg(t, h, "sc-B")

	// Seed a repo in orgB through the admin pool so the row exists
	// regardless of claims.
	if _, err := h.AdminDB.Exec(`
		INSERT INTO repo_profiles (org_id, owner, repo, profiled_at)
		VALUES ($1, 'orgb-owner', 'orgb-repo', now())
	`, orgB); err != nil {
		t.Fatalf("seed orgB repo: %v", err)
	}

	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)

	// Run SyntheticClaimsWithTx with userA's claims (orgA). Inside
	// fn, try to read orgB's repos via the app-pool RepoStore. RLS
	// must reject the read because tf.current_org_id() resolves to
	// orgA and repo_profiles_all gates on (org_id = current_org_id()).
	if err := stores.Tx.SyntheticClaimsWithTx(context.Background(), orgA, userA, func(tx db.TxStores) error {
		got, err := tx.Repos.List(context.Background(), orgB)
		if err != nil {
			return fmt.Errorf("orgB List under orgA claims: %w", err)
		}
		if len(got) != 0 {
			t.Errorf("cross-org leakage: List(orgB) under orgA claims returned %d rows, want 0", len(got))
		}
		return nil
	}); err != nil {
		t.Fatalf("SyntheticClaimsWithTx: %v", err)
	}

	// Confirm orgB's row is visible via the admin pool, ruling out
	// "the seed didn't land" as a false negative on the assertion
	// above. Uses the SKY-296 `...System` admin variant to exercise
	// that path end-to-end.
	got, err := stores.Repos.ListSystem(context.Background(), orgB)
	if err != nil {
		t.Fatalf("ListSystem(orgB): %v", err)
	}
	if len(got) != 1 {
		t.Errorf("admin ListSystem(orgB) saw %d rows, want 1 — seed didn't land", len(got))
	}
}

// TestSyntheticClaimsWithTx_Postgres_RejectsLocalDefaultUserID pins
// the SKY-296 guardrail: passing runmode.LocalDefaultUserID is
// rejected loudly because the sentinel has no FK target in
// multi-mode users and the sentinel-shaped caller belongs on the
// admin pool, not in synthetic claims.
func TestSyntheticClaimsWithTx_Postgres_RejectsLocalDefaultUserID(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, _ := seedSyntheticClaimsOrg(t, h, "sc-reject")

	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)

	err := stores.Tx.SyntheticClaimsWithTx(context.Background(), orgID, runmode.LocalDefaultUserID, func(tx db.TxStores) error {
		t.Errorf("fn should not have run when LocalDefaultUserID was passed")
		return nil
	})
	if err == nil {
		t.Fatal("SyntheticClaimsWithTx with LocalDefaultUserID returned nil; want guardrail rejection")
	}
	if !strings.Contains(err.Error(), "LocalDefaultUserID") {
		t.Errorf("error %q does not mention the sentinel rejection", err.Error())
	}
}

// TestSyntheticClaimsWithTx_Postgres_RejectsEmptyUserID pins the
// other guardrail: an empty userID is rejected because the resulting
// JWT-claims sub would be empty and tf.current_user_id() would
// silently produce NULL — masking RLS bugs as "no rows returned"
// outcomes. Callers without a real user identity belong on the
// admin pool.
func TestSyntheticClaimsWithTx_Postgres_RejectsEmptyUserID(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, _ := seedSyntheticClaimsOrg(t, h, "sc-empty")

	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)

	err := stores.Tx.SyntheticClaimsWithTx(context.Background(), orgID, "", func(tx db.TxStores) error {
		t.Errorf("fn should not have run when userID was empty")
		return nil
	})
	if err == nil {
		t.Fatal("SyntheticClaimsWithTx with empty userID returned nil; want guardrail rejection")
	}
	if !strings.Contains(err.Error(), "userID") {
		t.Errorf("error %q does not mention the empty-userID rejection", err.Error())
	}
}

// TestSyntheticClaimsWithTx_Postgres_RollsBackOnError mirrors WithTx's
// commit-on-success / rollback-on-error semantics: a fn that returns
// an error must leave the database unchanged.
func TestSyntheticClaimsWithTx_Postgres_RollsBackOnError(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, userID := seedSyntheticClaimsOrg(t, h, "sc-rollback")

	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	sentinel := errors.New("forced rollback")

	err := stores.Tx.SyntheticClaimsWithTx(context.Background(), orgID, userID, func(tx db.TxStores) error {
		if err := tx.Repos.SetConfigured(context.Background(), orgID, []string{"rolled/back"}); err != nil {
			return err
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected sentinel error, got %v", err)
	}

	names, err := stores.Repos.ListConfiguredNamesSystem(context.Background(), orgID)
	if err != nil {
		t.Fatalf("ListConfiguredNamesSystem: %v", err)
	}
	for _, name := range names {
		if name == "rolled/back" {
			t.Errorf("rolled/back row landed after fn returned error; full list=%v", names)
		}
	}
}

// TestWithTx_Postgres_SurvivesCancelledOriginCtx pins the SKY-300
// handler-cleanup pattern: a goroutine-like cleanup that needs to
// outlive its originating request gets a cancellation-detached ctx
// via context.WithoutCancel(r.Context()), and WithTx runs to
// completion against that ctx even though the parent ctx is already
// done. The combination of WithoutCancel (value inheritance + cancel
// detachment) + WithTx (claims set from caller-supplied identity)
// is what makes post-handler cleanup safe in multi-mode: writes
// still attribute to the request user, but a client disconnect
// can't strand the cleanup mid-batch.
func TestWithTx_Postgres_SurvivesCancelledOriginCtx(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, userID := seedSyntheticClaimsOrg(t, h, "withtx-detached")

	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)

	// Build a parent ctx and cancel it immediately — the standin
	// for "request returned, r.Context() is done."
	parentCtx, parentCancel := context.WithCancel(context.Background())
	parentCancel()
	if parentCtx.Err() == nil {
		t.Fatal("parentCtx should be done after cancel")
	}

	// Detach via WithoutCancel — the cleanup ctx inherits parent's
	// values (none here in the test, but in production it carries
	// claims set by auth middleware) without inheriting the cancel.
	cleanupCtx := context.WithoutCancel(parentCtx)
	if cleanupCtx.Err() != nil {
		t.Fatal("cleanupCtx should NOT be done — WithoutCancel breaks the cancel chain")
	}

	// Inside WithTx, write a row + read it back. Both must succeed
	// despite the parent ctx being done.
	if err := stores.Tx.WithTx(cleanupCtx, orgID, userID, func(tx db.TxStores) error {
		if err := tx.Repos.SetConfigured(cleanupCtx, orgID, []string{"survives/cancel"}); err != nil {
			return fmt.Errorf("SetConfigured under detached ctx: %w", err)
		}
		return nil
	}); err != nil {
		t.Fatalf("WithTx on detached ctx: %v", err)
	}

	names, err := stores.Repos.ListConfiguredNamesSystem(context.Background(), orgID)
	if err != nil {
		t.Fatalf("ListConfiguredNamesSystem: %v", err)
	}
	if len(names) != 1 || names[0] != "survives/cancel" {
		t.Errorf("after WithTx on detached ctx: got %v, want [survives/cancel]", names)
	}
}

// seedSyntheticClaimsOrg creates a fresh org + owner user + default
// team for SyntheticClaimsWithTx tests. Mirrors seedPgEntityOrg's
// shape but with a different label so the test files don't collide
// on schema-reset interactions.
func seedSyntheticClaimsOrg(t *testing.T, h *pgtest.Harness, label string) (orgID, userID string) {
	t.Helper()
	orgID = uuid.New().String()
	userID = uuid.New().String()
	email := fmt.Sprintf("%s-%s@test.local", label, userID[:8])

	h.SeedAuthUser(t, userID, email)
	if _, err := h.AdminDB.Exec(
		`INSERT INTO users (id, display_name) VALUES ($1, $2)`,
		userID, "Synthetic Claims "+label,
	); err != nil {
		t.Fatalf("seed public.users: %v", err)
	}
	if _, err := h.AdminDB.Exec(
		`INSERT INTO orgs (id, name, slug, owner_user_id) VALUES ($1, $2, $3, $4)`,
		orgID, "SC Org "+label, "sc-"+orgID[:8], userID,
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
	return orgID, userID
}
