package postgres_test

import (
	"context"
	"database/sql"
	"testing"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TestArtifactStore_Postgres_RoundTrip drives Upsert + ListByRun against
// the AdminDB-wired store (RLS bypassed — behavior, not auth). Pins that
// every field round-trips and an empty id is server-generated. TFAC-455.
func TestArtifactStore_Postgres_RoundTrip(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, userID, teamID := pgtest.SeedOrgWithUser(t, h, "alice")
	runID := seedPgArtifactRun(t, h, orgID, teamID, userID)

	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	ctx := context.Background()

	in := domain.Artifact{
		RunID:       runID,
		OrgID:       orgID,
		TeamID:      teamID,
		Provider:    domain.ArtifactProviderGitHub,
		Kind:        domain.ArtifactKindPullRequest,
		Target:      "octo/repo#123",
		ExternalID:  "123",
		URL:         "https://github.com/octo/repo/pull/123",
		State:       domain.ArtifactStatePROpen,
		DedupKey:    domain.ArtifactDedupKey("github", "pull_request", "octo/repo", "refs/heads/feat"),
		DetailsJSON: `{"draft":false}`,
	}
	out, err := stores.Artifacts.Upsert(ctx, orgID, in)
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if out.ID == "" {
		t.Error("expected server-generated id")
	}
	if out.CreatedAt.IsZero() || out.UpdatedAt.IsZero() {
		t.Error("expected timestamps populated")
	}
	if out.RunID != runID || out.TeamID != teamID || out.Provider != "github" ||
		out.Kind != "pull_request" || out.Target != "octo/repo#123" || out.ExternalID != "123" ||
		out.URL != in.URL || out.State != "open" || out.DedupKey != in.DedupKey ||
		out.DetailsJSON != `{"draft":false}` {
		t.Errorf("round-trip mismatch: %+v", out)
	}

	got, err := stores.Artifacts.ListByRun(ctx, orgID, runID)
	if err != nil {
		t.Fatalf("ListByRun: %v", err)
	}
	if len(got) != 1 || got[0].ID != out.ID {
		t.Errorf("ListByRun = %+v, want the one upserted row", got)
	}
}

// TestArtifactStore_Postgres_UpsertDedup pins that the same dedup_key
// collapses to one row and the second upsert updates the mutable fields.
func TestArtifactStore_Postgres_UpsertDedup(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, userID, teamID := pgtest.SeedOrgWithUser(t, h, "alice")
	runID := seedPgArtifactRun(t, h, orgID, teamID, userID)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	ctx := context.Background()

	key := domain.ArtifactDedupKey("github", "pull_request", "octo/repo", "refs/heads/feat")
	first, err := stores.Artifacts.Upsert(ctx, orgID, domain.Artifact{
		RunID: runID, OrgID: orgID, TeamID: teamID,
		Provider: "github", Kind: "pull_request", Target: "octo/repo",
		State: domain.ArtifactStatePRDraft, DedupKey: key,
	})
	if err != nil {
		t.Fatalf("first Upsert: %v", err)
	}
	second, err := stores.Artifacts.Upsert(ctx, orgID, domain.Artifact{
		RunID: runID, OrgID: orgID, TeamID: teamID,
		Provider: "github", Kind: "pull_request", Target: "octo/repo#7",
		ExternalID: "7", URL: "https://github.com/octo/repo/pull/7",
		State: domain.ArtifactStatePROpen, DedupKey: key,
	})
	if err != nil {
		t.Fatalf("second Upsert: %v", err)
	}
	if first.ID != second.ID {
		t.Errorf("dedup failed: %s vs %s", first.ID, second.ID)
	}
	var count int
	if err := h.AdminDB.QueryRow(`SELECT COUNT(*) FROM artifacts WHERE dedup_key = $1`, key).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Errorf("want 1 row, got %d", count)
	}
	if second.State != "open" || second.ExternalID != "7" || second.Target != "octo/repo#7" {
		t.Errorf("second upsert did not update fields: %+v", second)
	}
	if second.UpdatedAt.Before(second.CreatedAt) {
		t.Errorf("updated_at %v before created_at %v", second.UpdatedAt, second.CreatedAt)
	}
}

// TestArtifactStore_Postgres_PendingToReal pins the pending→real PR
// transition: keyed on the branch ref, the real PR upserts onto the same
// row (pending→open, url/external_id filled).
func TestArtifactStore_Postgres_PendingToReal(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, userID, teamID := pgtest.SeedOrgWithUser(t, h, "alice")
	runID := seedPgArtifactRun(t, h, orgID, teamID, userID)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	ctx := context.Background()

	key := domain.ArtifactDedupKey("github", "pull_request", "octo/repo", "refs/heads/feat")
	pending, err := stores.Artifacts.Upsert(ctx, orgID, domain.Artifact{
		RunID: runID, OrgID: orgID, TeamID: teamID,
		Provider: "github", Kind: "pull_request", Target: "octo/repo",
		State: domain.ArtifactStatePRPending, DedupKey: key,
	})
	if err != nil {
		t.Fatalf("pending Upsert: %v", err)
	}
	if pending.State != "pending" || pending.ExternalID != "" || pending.URL != "" {
		t.Fatalf("pending row malformed: %+v", pending)
	}
	real, err := stores.Artifacts.Upsert(ctx, orgID, domain.Artifact{
		RunID: runID, OrgID: orgID, TeamID: teamID,
		Provider: "github", Kind: "pull_request", Target: "octo/repo#42",
		ExternalID: "42", URL: "https://github.com/octo/repo/pull/42",
		State: domain.ArtifactStatePROpen, DedupKey: key,
	})
	if err != nil {
		t.Fatalf("real Upsert: %v", err)
	}
	if real.ID != pending.ID {
		t.Errorf("pending→real minted a new row: %s vs %s", pending.ID, real.ID)
	}
	if real.State != "open" || real.ExternalID != "42" || real.URL == "" {
		t.Errorf("real row did not transition: %+v", real)
	}
	var count int
	if err := h.AdminDB.QueryRow(`SELECT COUNT(*) FROM artifacts`).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Errorf("want 1 row after pending→real, got %d", count)
	}
}

// TestArtifactStore_Postgres_RLS_TeamScoped pins the production RLS layer:
// an artifact scoped to teamA is visible to teamA members (alice, bob) and
// invisible to a same-org member of a different team (carol). Mirrors the
// runs team-visibility scoping. TFAC-455.
func TestArtifactStore_Postgres_RLS_TeamScoped(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)

	orgA, alice, teamA := pgtest.SeedOrgWithUser(t, h, "alice")
	bob := pgtest.SeedUser(t, h, "bob")
	carol := pgtest.SeedUser(t, h, "carol")
	pgtest.AddOrgMember(t, h, bob, orgA, teamA, "member", "member")
	teamB := pgtest.SeedTeam(t, h, orgA, "team-b")
	pgtest.AddOrgMember(t, h, carol, orgA, teamB, "member", "member")

	runID := seedPgArtifactRun(t, h, orgA, teamA, alice)
	// Seed a teamA-scoped artifact via admin (bypass RLS for setup).
	if _, err := h.AdminDB.Exec(`
		INSERT INTO artifacts (org_id, run_id, team_id, provider, kind, target, state, dedup_key)
		VALUES ($1, $2, $3, 'github', 'pull_request', 'octo/repo', 'open', 'github:pull_request:octo/repo#1')
	`, orgA, runID, teamA); err != nil {
		t.Fatalf("seed artifact: %v", err)
	}
	ctx := context.Background()

	// Alice (teamA) and bob (teamA) see it.
	for _, u := range []struct {
		name, id string
	}{{"alice", alice}, {"bob", bob}} {
		err := h.WithUser(t, u.id, orgA, func(tx *sql.Tx) error {
			rows, err := pgstore.NewForTx(tx, pgtest.SecretKey).Artifacts.ListByTeam(ctx, orgA, teamA, db.ArtifactListOpts{})
			if err != nil {
				return err
			}
			if len(rows) != 1 {
				t.Errorf("%s saw %d teamA artifacts, want 1", u.name, len(rows))
			}
			return nil
		})
		if err != nil {
			t.Fatalf("%s path: %v", u.name, err)
		}
	}

	// Carol (teamB, same org, different team) sees zero — artifacts_select
	// gates on user_in_team(team_id).
	err := h.WithUser(t, carol, orgA, func(tx *sql.Tx) error {
		rows, err := pgstore.NewForTx(tx, pgtest.SecretKey).Artifacts.ListByTeam(ctx, orgA, teamA, db.ArtifactListOpts{})
		if err != nil {
			return err
		}
		if len(rows) != 0 {
			t.Errorf("carol (different team) saw %d teamA artifacts — RLS leaked across teams", len(rows))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("carol path: %v", err)
	}
}

// TestArtifactStore_Postgres_RLS_WritePath exercises the artifacts_insert
// policy under tf_app (the read test bypasses it via AdminDB at setup): a
// teamA member with write access can Upsert a teamA artifact, but a member
// of a different team is rejected by the WITH CHECK
// (user_can_write_team(team_id)). Without this the write policy could be
// misconfigured and the read-only suite would still pass. TFAC-455.
func TestArtifactStore_Postgres_RLS_WritePath(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)

	orgA, alice, teamA := pgtest.SeedOrgWithUser(t, h, "alice")
	carol := pgtest.SeedUser(t, h, "carol")
	teamB := pgtest.SeedTeam(t, h, orgA, "team-b")
	pgtest.AddOrgMember(t, h, carol, orgA, teamB, "member", "member")
	ctx := context.Background()

	// run_id left empty (NULL) so this isolates artifacts_insert, not the
	// runs FK/RLS interplay.
	mkArtifact := func(key string) domain.Artifact {
		return domain.Artifact{
			OrgID: orgA, TeamID: teamA,
			Provider: "github", Kind: "pull_request", Target: "octo/repo",
			State: domain.ArtifactStatePROpen, DedupKey: key,
		}
	}

	// Alice (teamA, founder → can write) succeeds through the app pool.
	if err := h.WithUser(t, alice, orgA, func(tx *sql.Tx) error {
		_, err := pgstore.NewForTx(tx, pgtest.SecretKey).Artifacts.Upsert(ctx, orgA, mkArtifact("github:pull_request:octo/repo:refs/heads/a"))
		return err
	}); err != nil {
		t.Fatalf("alice (teamA writer) Upsert under RLS: %v", err)
	}
	var landed int
	if err := h.AdminDB.QueryRow(`SELECT COUNT(*) FROM artifacts WHERE team_id = $1`, teamA).Scan(&landed); err != nil {
		t.Fatalf("count: %v", err)
	}
	if landed != 1 {
		t.Errorf("alice's artifact didn't land: count = %d, want 1", landed)
	}

	// Carol (teamB) writing a teamA-scoped artifact is rejected by the
	// artifacts_insert WITH CHECK.
	err := h.WithUser(t, carol, orgA, func(tx *sql.Tx) error {
		_, e := pgstore.NewForTx(tx, pgtest.SecretKey).Artifacts.Upsert(ctx, orgA, mkArtifact("github:pull_request:octo/repo:refs/heads/b"))
		return e
	})
	pgtest.AssertRLSViolation(t, err)
}

// TestArtifactStore_Postgres_UpsertSystem_BypassesRLS pins the
// event-triggered capture path (TFAC-460): a run with no user identity
// (auto-delegation) writes through UpsertSystem on the admin pool, which
// must land a team-scoped artifact WITHOUT any JWT claims — exactly where
// the app-pool Upsert is rejected for lack of a current org/user.
func TestArtifactStore_Postgres_UpsertSystem_BypassesRLS(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)

	orgA, _, teamA := pgtest.SeedOrgWithUser(t, h, "alice")
	ctx := context.Background()

	// Dual-pool store: Upsert → AppDB (RLS-active), UpsertSystem → AdminDB
	// (BYPASSRLS), the production wiring.
	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)

	art := domain.Artifact{
		OrgID: orgA, TeamID: teamA,
		Provider: "git", Kind: "branch", Target: "octo/repo",
		State: domain.ArtifactStateBranchPushed, DedupKey: "git:branch:octo/repo:refs/heads/x",
	}

	// The app-pool Upsert with no claims set is rejected — there is no
	// current org/user for the artifacts_insert WITH CHECK to satisfy. This
	// is the condition an event-triggered run is in.
	if _, err := stores.Artifacts.Upsert(ctx, orgA, art); err == nil {
		t.Fatal("app-pool Upsert with no claims unexpectedly succeeded")
	}

	// UpsertSystem on the admin pool lands the row regardless.
	if _, err := stores.Artifacts.UpsertSystem(ctx, orgA, art); err != nil {
		t.Fatalf("UpsertSystem under no claims: %v", err)
	}
	var landed int
	if err := h.AdminDB.QueryRow(`SELECT COUNT(*) FROM artifacts WHERE team_id = $1 AND dedup_key = $2`, teamA, art.DedupKey).Scan(&landed); err != nil {
		t.Fatalf("count: %v", err)
	}
	if landed != 1 {
		t.Errorf("UpsertSystem artifact didn't land: count = %d, want 1", landed)
	}
}

// TestArtifactStore_Postgres_ListByTeam_IncludesDetached pins the
// audit-ledger invariant: an artifact whose run was purged (run_id NULL)
// is still the team's and must come back from ListByTeam. Guards against a
// future `AND run_id IS NOT NULL` creeping into the query. TFAC-455.
func TestArtifactStore_Postgres_ListByTeam_IncludesDetached(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, userID, teamID := pgtest.SeedOrgWithUser(t, h, "alice")
	runID := seedPgArtifactRun(t, h, orgID, teamID, userID)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	ctx := context.Background()

	art, err := stores.Artifacts.Upsert(ctx, orgID, domain.Artifact{
		RunID: runID, OrgID: orgID, TeamID: teamID,
		Provider: "git", Kind: "branch", Target: "octo/repo",
		State: domain.ArtifactStateBranchPushed, DedupKey: "git:branch:octo/repo:refs/heads/x",
	})
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	// Simulate a run purge: the FK is ON DELETE SET NULL, so deleting the
	// run detaches the artifact rather than cascading it away.
	if _, err := h.AdminDB.Exec(`DELETE FROM runs WHERE id = $1`, runID); err != nil {
		t.Fatalf("purge run: %v", err)
	}
	var nullRun sql.NullString
	if err := h.AdminDB.QueryRow(`SELECT run_id FROM artifacts WHERE id = $1`, art.ID).Scan(&nullRun); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if nullRun.Valid {
		t.Fatalf("run_id should be NULL after run purge, got %q", nullRun.String)
	}

	rows, err := stores.Artifacts.ListByTeam(ctx, orgID, teamID, db.ArtifactListOpts{})
	if err != nil {
		t.Fatalf("ListByTeam: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != art.ID {
		t.Errorf("ListByTeam dropped the detached artifact: %+v", rows)
	}
	if rows[0].RunID != "" {
		t.Errorf("detached row RunID = %q, want empty", rows[0].RunID)
	}
}

// seedPgArtifactRun mints a minimal run the artifacts.run_id FK can point
// at. origin is non-'blueprint' so runs_origin_requires_parents doesn't
// demand a parent chain; trigger_type='manual' needs a non-NULL creator.
func seedPgArtifactRun(t *testing.T, h *pgtest.Harness, orgID, teamID, userID string) string {
	t.Helper()
	id := uuid.New().String()
	if _, err := h.AdminDB.Exec(`
		INSERT INTO runs (id, org_id, team_id, creator_user_id, trigger_type, origin, status, visibility)
		VALUES ($1, $2, $3, $4, 'manual', 'interactive', 'running', 'team')
	`, id, orgID, teamID, userID); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	return id
}
