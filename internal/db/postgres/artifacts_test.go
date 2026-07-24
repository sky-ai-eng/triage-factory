package postgres_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

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
		ConversationID: runID,
		OrgID:          orgID,
		TeamID:         teamID,
		Provider:       domain.ArtifactProviderGitHub,
		Kind:           domain.ArtifactKindPullRequest,
		Target:         "octo/repo#123",
		ExternalID:     "123",
		URL:            "https://github.com/octo/repo/pull/123",
		State:          domain.ArtifactStatePROpen,
		DedupKey:       domain.ArtifactDedupKey("github", "pull_request", "octo/repo", "refs/heads/feat"),
		DetailsJSON:    `{"draft":false}`,
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
	if out.ConversationID != runID || out.TeamID != teamID || out.Provider != "github" ||
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
		ConversationID: runID, OrgID: orgID, TeamID: teamID,
		Provider: "github", Kind: "pull_request", Target: "octo/repo",
		State: domain.ArtifactStatePRDraft, DedupKey: key,
	})
	if err != nil {
		t.Fatalf("first Upsert: %v", err)
	}
	second, err := stores.Artifacts.Upsert(ctx, orgID, domain.Artifact{
		ConversationID: runID, OrgID: orgID, TeamID: teamID,
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
		ConversationID: runID, OrgID: orgID, TeamID: teamID,
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
		ConversationID: runID, OrgID: orgID, TeamID: teamID,
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
		INSERT INTO artifacts (org_id, conversation_id, team_id, provider, kind, target, state, dedup_key)
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

	// conversation_id left empty (NULL) so this isolates artifacts_insert, not the
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

// TestArtifactStore_Postgres_ListByTeam_IncludesDetached pins the
// audit-ledger invariant: an artifact whose run was purged (conversation_id NULL)
// is still the team's and must come back from ListByTeam. Guards against a
// future `AND conversation_id IS NOT NULL` creeping into the query. TFAC-455.
func TestArtifactStore_Postgres_ListByTeam_IncludesDetached(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, userID, teamID := pgtest.SeedOrgWithUser(t, h, "alice")
	runID := seedPgArtifactRun(t, h, orgID, teamID, userID)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	ctx := context.Background()

	art, err := stores.Artifacts.Upsert(ctx, orgID, domain.Artifact{
		ConversationID: runID, OrgID: orgID, TeamID: teamID,
		Provider: "git", Kind: "branch", Target: "octo/repo",
		State: domain.ArtifactStateBranchPushed, DedupKey: "git:branch:octo/repo:refs/heads/x",
	})
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	// Simulate a run purge: the FK is ON DELETE SET NULL, so deleting the
	// run detaches the artifact rather than cascading it away.
	if _, err := h.AdminDB.Exec(`DELETE FROM conversations WHERE id = $1`, runID); err != nil {
		t.Fatalf("purge run: %v", err)
	}
	var nullRun sql.NullString
	if err := h.AdminDB.QueryRow(`SELECT conversation_id FROM artifacts WHERE id = $1`, art.ID).Scan(&nullRun); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if nullRun.Valid {
		t.Fatalf("conversation_id should be NULL after run purge, got %q", nullRun.String)
	}

	rows, err := stores.Artifacts.ListByTeam(ctx, orgID, teamID, db.ArtifactListOpts{})
	if err != nil {
		t.Fatalf("ListByTeam: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != art.ID {
		t.Errorf("ListByTeam dropped the detached artifact: %+v", rows)
	}
	if rows[0].ConversationID != "" {
		t.Errorf("detached row RunID = %q, want empty", rows[0].ConversationID)
	}
}

// TestArtifactStore_Postgres_UpsertPreservesExternalIDAndURL pins that an
// upsert leaving external_id/url empty does NOT blank values an earlier upsert
// stored. They are the backing object's stable coordinates and only fill in;
// a reconciliation pass or a URL-less mutation must not erase them. State
// still follows the latest writer.
func TestArtifactStore_Postgres_UpsertPreservesExternalIDAndURL(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, userID, teamID := pgtest.SeedOrgWithUser(t, h, "alice")
	runID := seedPgArtifactRun(t, h, orgID, teamID, userID)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	ctx := context.Background()

	key := domain.ArtifactDedupKey("jira", "issue", "SKY-1", "")
	if _, err := stores.Artifacts.Upsert(ctx, orgID, domain.Artifact{
		ConversationID: runID, OrgID: orgID, TeamID: teamID,
		Provider: "jira", Kind: "issue", Target: "SKY-1",
		ExternalID: "SKY-1", URL: "https://jira.example.com/browse/SKY-1",
		State: domain.ArtifactStateIssueCreated, DedupKey: key,
	}); err != nil {
		t.Fatalf("first Upsert: %v", err)
	}

	out, err := stores.Artifacts.Upsert(ctx, orgID, domain.Artifact{
		ConversationID: runID, OrgID: orgID, TeamID: teamID,
		Provider: "jira", Kind: "issue", Target: "SKY-1",
		State: domain.ArtifactStateIssueUpdated, DedupKey: key,
	})
	if err != nil {
		t.Fatalf("second Upsert: %v", err)
	}
	if out.ExternalID != "SKY-1" || out.URL != "https://jira.example.com/browse/SKY-1" {
		t.Errorf("external_id/url were blanked by an empty upsert: ext=%q url=%q", out.ExternalID, out.URL)
	}
	if out.State != domain.ArtifactStateIssueUpdated {
		t.Errorf("state should follow the latest writer, got %q", out.State)
	}

	// A non-empty value still overwrites.
	out, err = stores.Artifacts.Upsert(ctx, orgID, domain.Artifact{
		ConversationID: runID, OrgID: orgID, TeamID: teamID,
		Provider: "jira", Kind: "issue", Target: "SKY-1",
		ExternalID: "SKY-1", URL: "https://jira.example.com/browse/SKY-1?focusedId=9",
		State: domain.ArtifactStateIssueUpdated, DedupKey: key,
	})
	if err != nil {
		t.Fatalf("third Upsert: %v", err)
	}
	if out.URL != "https://jira.example.com/browse/SKY-1?focusedId=9" {
		t.Errorf("non-empty url did not overwrite: %q", out.URL)
	}
}

// TestArtifactStore_Postgres_UpsertPreservesTargetOnEmpty pins that an upsert
// leaving target empty does NOT blank the resource key an earlier upsert
// stored — a GitHub comment-update/delete knows only the comment id, not the
// PR number, so it can't recompute owner/repo#N and must inherit it. A
// non-empty target still migrates (the pending→real PR path).
func TestArtifactStore_Postgres_UpsertPreservesTargetOnEmpty(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, userID, teamID := pgtest.SeedOrgWithUser(t, h, "alice")
	runID := seedPgArtifactRun(t, h, orgID, teamID, userID)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	ctx := context.Background()

	key := domain.ArtifactDedupKey("github", "comment", "555", "")
	if _, err := stores.Artifacts.Upsert(ctx, orgID, domain.Artifact{
		ConversationID: runID, OrgID: orgID, TeamID: teamID,
		Provider: "github", Kind: "comment", Target: "octo/repo#7",
		ExternalID: "555", URL: "https://github.com/octo/repo/pull/7#issuecomment-555",
		State: domain.ArtifactStateCommentPosted, DedupKey: key,
	}); err != nil {
		t.Fatalf("first Upsert: %v", err)
	}

	// A comment-delete that only carries the id (empty target/url).
	out, err := stores.Artifacts.Upsert(ctx, orgID, domain.Artifact{
		ConversationID: runID, OrgID: orgID, TeamID: teamID,
		Provider: "github", Kind: "comment", ExternalID: "555",
		State: domain.ArtifactStateCommentDeleted, DedupKey: key,
	})
	if err != nil {
		t.Fatalf("second Upsert: %v", err)
	}
	if out.Target != "octo/repo#7" || out.URL != "https://github.com/octo/repo/pull/7#issuecomment-555" {
		t.Errorf("empty upsert blanked stable coordinates: target=%q url=%q", out.Target, out.URL)
	}
	if out.State != domain.ArtifactStateCommentDeleted {
		t.Errorf("state should follow the latest writer, got %q", out.State)
	}

	// A non-empty target still overwrites (migration path).
	out, err = stores.Artifacts.Upsert(ctx, orgID, domain.Artifact{
		ConversationID: runID, OrgID: orgID, TeamID: teamID,
		Provider: "github", Kind: "comment", Target: "octo/repo#8",
		State: domain.ArtifactStateCommentPosted, DedupKey: key,
	})
	if err != nil {
		t.Fatalf("third Upsert: %v", err)
	}
	if out.Target != "octo/repo#8" {
		t.Errorf("non-empty target did not overwrite: %q", out.Target)
	}
}

// TestArtifactStore_Postgres_UpsertSystem_BypassesRLS pins the admin-pool
// write path the event-triggered exec choke point depends on (TFAC-459): a
// run with no creator user has no JWT-claims context, so its artifact insert
// is unreachable through tf_app (artifacts_insert demands a team-writing
// user). UpsertSystem routes to the admin pool and lands the row; the app-pool
// Upsert on the same bare (claims-less) connection is rejected, proving the
// System variant is doing real pool-routing work, not riding ambient access.
func TestArtifactStore_Postgres_UpsertSystem_BypassesRLS(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, _, teamID := pgtest.SeedOrgWithUser(t, h, "alice")

	// app = AppDB (authenticator, RLS-active, no claims set here); admin =
	// AdminDB (BYPASSRLS). This is the production pool split.
	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	ctx := context.Background()

	// conversation_id left empty (NULL) so this isolates the write-policy/pool routing
	// from the runs FK.
	art := domain.Artifact{
		OrgID: orgID, TeamID: teamID,
		Provider: domain.ArtifactProviderJira, Kind: domain.ArtifactKindIssue,
		Target: "SKY-1", ExternalID: "SKY-1", State: domain.ArtifactStateIssueCreated,
		DedupKey: domain.ArtifactDedupKey("jira", "issue", "SKY-1", ""),
	}

	// The app-pool write with no claims context fails — exactly why the
	// event-triggered writer needs the admin path.
	if _, err := stores.Artifacts.Upsert(ctx, orgID, art); err == nil {
		t.Fatal("expected the claims-less app-pool Upsert to be rejected")
	}

	// UpsertSystem (admin pool) lands the row.
	out, err := stores.Artifacts.UpsertSystem(ctx, orgID, art)
	if err != nil {
		t.Fatalf("UpsertSystem: %v", err)
	}
	if out.ID == "" || out.Target != "SKY-1" || out.State != domain.ArtifactStateIssueCreated {
		t.Errorf("UpsertSystem row malformed: %+v", out)
	}

	var count int
	if err := h.AdminDB.QueryRow(
		`SELECT COUNT(*) FROM artifacts WHERE team_id = $1 AND dedup_key = 'jira:issue:SKY-1'`, teamID,
	).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Errorf("UpsertSystem did not land the row: count = %d, want 1", count)
	}
}

// TestArtifactStore_Postgres_ListNonTerminalBySystem pins the reconciler's
// org-wide admin read (TFAC-464): on the admin pool it returns EXACTLY the
// reconcilable non-terminal rows for the org (PR draft|open, review pending,
// branch pushed — domain.IsReconcilableNonTerminal), excludes terminal /
// unreconcilable states, and never crosses org boundaries.
func TestArtifactStore_Postgres_ListNonTerminalBySystem(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgA, _, teamA := pgtest.SeedOrgWithUser(t, h, "alice")
	orgB, _, teamB := pgtest.SeedOrgWithUser(t, h, "bob")

	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	ctx := context.Background()

	seeds := []struct{ kind, state string }{
		{domain.ArtifactKindPullRequest, domain.ArtifactStatePRPending},
		{domain.ArtifactKindPullRequest, domain.ArtifactStatePRDraft},
		{domain.ArtifactKindPullRequest, domain.ArtifactStatePROpen},
		{domain.ArtifactKindPullRequest, domain.ArtifactStatePRMerged},
		{domain.ArtifactKindPullRequest, domain.ArtifactStatePRClosed},
		{domain.ArtifactKindReview, domain.ArtifactStateReviewPending},
		{domain.ArtifactKindReview, domain.ArtifactStateReviewSubmitted},
		{domain.ArtifactKindReview, domain.ArtifactStateReviewDismissed},
		{domain.ArtifactKindBranch, domain.ArtifactStateBranchPushed},
		{domain.ArtifactKindBranch, domain.ArtifactStateBranchDeleted},
		{domain.ArtifactKindComment, domain.ArtifactStateCommentPosted},
		{domain.ArtifactKindIssue, domain.ArtifactStateIssueCreated},
	}
	want := map[string]bool{}
	for i, s := range seeds {
		key := fmt.Sprintf("a%d", i)
		if _, err := stores.Artifacts.UpsertSystem(ctx, orgA, domain.Artifact{
			OrgID: orgA, TeamID: teamA, Provider: "github", Kind: s.kind, Target: "octo/repo", State: s.state, DedupKey: key,
		}); err != nil {
			t.Fatalf("seed %s/%s: %v", s.kind, s.state, err)
		}
		if domain.IsReconcilableNonTerminal(s.kind, s.state) {
			want[key] = true
		}
	}
	// Org B owns a non-terminal artifact that must never surface in org A's list.
	if _, err := stores.Artifacts.UpsertSystem(ctx, orgB, domain.Artifact{
		OrgID: orgB, TeamID: teamB, Provider: "github", Kind: domain.ArtifactKindPullRequest,
		Target: "octo/repo", State: domain.ArtifactStatePROpen, DedupKey: "b0",
	}); err != nil {
		t.Fatalf("seed org B: %v", err)
	}

	got, err := stores.Artifacts.ListNonTerminalBySystem(ctx, orgA)
	if err != nil {
		t.Fatalf("ListNonTerminalBySystem: %v", err)
	}
	gotKeys := map[string]bool{}
	for _, a := range got {
		if a.OrgID != orgA {
			t.Errorf("cross-org leak: row %q belongs to org %q, not %q", a.DedupKey, a.OrgID, orgA)
		}
		gotKeys[a.DedupKey] = true
	}
	if len(gotKeys) != len(want) {
		t.Errorf("returned %d rows, want %d: got=%v want=%v", len(gotKeys), len(want), gotKeys, want)
	}
	for k := range want {
		if !gotKeys[k] {
			t.Errorf("dropped reconcilable row %q", k)
		}
	}
	for k := range gotKeys {
		if !want[k] {
			t.Errorf("returned terminal/unreconcilable row %q (SQL predicate disagrees with domain.IsReconcilableNonTerminal)", k)
		}
	}
}

// TestArtifactStore_Postgres_CountByRun pins the batched per-run count
// (TFAC-465) on the admin pool: counts keyed by run id across a multi-run
// batch, a run with no artifacts absent from the map, and an empty runIDs a
// no-op. Doubles as the proof that the []string→ANY($2) bind and GROUP BY work
// against the uuid conversation_id column.
func TestArtifactStore_Postgres_CountByRun(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, userID, teamID := pgtest.SeedOrgWithUser(t, h, "alice")
	runA := seedPgArtifactRun(t, h, orgID, teamID, userID)
	runB := seedPgArtifactRun(t, h, orgID, teamID, userID)
	runC := seedPgArtifactRun(t, h, orgID, teamID, userID) // no artifacts
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	ctx := context.Background()

	seed := func(runID, key string) {
		if _, err := stores.Artifacts.Upsert(ctx, orgID, domain.Artifact{
			ConversationID: runID, OrgID: orgID, TeamID: teamID,
			Provider: "github", Kind: "comment", Target: "octo/repo",
			State: domain.ArtifactStateCommentPosted, DedupKey: key,
		}); err != nil {
			t.Fatalf("seed %s: %v", key, err)
		}
	}
	seed(runA, "a1")
	seed(runA, "a2")
	seed(runB, "b1")

	counts, err := stores.Artifacts.CountByRun(ctx, orgID, []string{runA, runB, runC})
	if err != nil {
		t.Fatalf("CountByRun: %v", err)
	}
	if counts[runA] != 2 {
		t.Errorf("runA count = %d, want 2", counts[runA])
	}
	if counts[runB] != 1 {
		t.Errorf("runB count = %d, want 1", counts[runB])
	}
	if _, ok := counts[runC]; ok {
		t.Errorf("runC (no artifacts) should be absent, got %d", counts[runC])
	}

	empty, err := stores.Artifacts.CountByRun(ctx, orgID, nil)
	if err != nil {
		t.Fatalf("CountByRun(nil): %v", err)
	}
	if empty == nil || len(empty) != 0 {
		t.Errorf("CountByRun(nil) = %v, want empty non-nil map", empty)
	}
}

// TestArtifactStore_Postgres_CountByRun_TeamScoped pins that CountByRun runs on
// the RLS-active app pool: a same-org member of a different team counts zero for
// a run whose artifacts belong to another team — the per-card count can't leak
// across the tenancy boundary. Mirrors the ListByTeam RLS test. TFAC-465.
func TestArtifactStore_Postgres_CountByRun_TeamScoped(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgA, alice, teamA := pgtest.SeedOrgWithUser(t, h, "alice")
	carol := pgtest.SeedUser(t, h, "carol")
	teamB := pgtest.SeedTeam(t, h, orgA, "team-b")
	pgtest.AddOrgMember(t, h, carol, orgA, teamB, "member", "member")
	runID := seedPgArtifactRun(t, h, orgA, teamA, alice)

	// Two teamA-scoped artifacts on the run (admin insert bypasses RLS for setup).
	for _, key := range []string{"github:comment:octo/repo:1", "github:comment:octo/repo:2"} {
		if _, err := h.AdminDB.Exec(`
			INSERT INTO artifacts (org_id, conversation_id, team_id, provider, kind, target, state, dedup_key)
			VALUES ($1, $2, $3, 'github', 'comment', 'octo/repo', 'posted', $4)
		`, orgA, runID, teamA, key); err != nil {
			t.Fatalf("seed artifact: %v", err)
		}
	}
	ctx := context.Background()

	// Alice (teamA) counts both.
	if err := h.WithUser(t, alice, orgA, func(tx *sql.Tx) error {
		counts, err := pgstore.NewForTx(tx, pgtest.SecretKey).Artifacts.CountByRun(ctx, orgA, []string{runID})
		if err != nil {
			return err
		}
		if counts[runID] != 2 {
			t.Errorf("alice counted %d, want 2", counts[runID])
		}
		return nil
	}); err != nil {
		t.Fatalf("alice path: %v", err)
	}

	// Carol (teamB, same org, different team) counts zero — artifacts_select
	// gates on user_in_team(team_id), so the rows are invisible to her.
	if err := h.WithUser(t, carol, orgA, func(tx *sql.Tx) error {
		counts, err := pgstore.NewForTx(tx, pgtest.SecretKey).Artifacts.CountByRun(ctx, orgA, []string{runID})
		if err != nil {
			return err
		}
		if _, ok := counts[runID]; ok {
			t.Errorf("carol (different team) counted %d — RLS leaked across teams", counts[runID])
		}
		return nil
	}); err != nil {
		t.Fatalf("carol path: %v", err)
	}
}

// TestArtifactStore_Postgres_ListByRuns pins the batched multi-run read on the
// admin pool (TFAC-465): artifacts for the given runs come back in one slice,
// each carrying its RunID for grouping; a run with no artifacts contributes
// nothing and an empty runIDs is a no-op. Doubles as proof the []string→ANY
// uuid[] bind works for the list variant.
func TestArtifactStore_Postgres_ListByRuns(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, userID, teamID := pgtest.SeedOrgWithUser(t, h, "alice")
	runA := seedPgArtifactRun(t, h, orgID, teamID, userID)
	runB := seedPgArtifactRun(t, h, orgID, teamID, userID)
	runC := seedPgArtifactRun(t, h, orgID, teamID, userID) // no artifacts
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	ctx := context.Background()

	seed := func(runID, key string) {
		if _, err := stores.Artifacts.Upsert(ctx, orgID, domain.Artifact{
			ConversationID: runID, OrgID: orgID, TeamID: teamID,
			Provider: "github", Kind: "comment", Target: "octo/repo",
			State: domain.ArtifactStateCommentPosted, DedupKey: key,
		}); err != nil {
			t.Fatalf("seed %s: %v", key, err)
		}
	}
	seed(runA, "a1")
	seed(runA, "a2")
	seed(runB, "b1")

	arts, err := stores.Artifacts.ListByRuns(ctx, orgID, []string{runA, runB, runC})
	if err != nil {
		t.Fatalf("ListByRuns: %v", err)
	}
	byRun := map[string]int{}
	for _, a := range arts {
		if a.ConversationID == "" {
			t.Errorf("artifact %s came back without its RunID", a.ID)
		}
		byRun[a.ConversationID]++
	}
	if byRun[runA] != 2 || byRun[runB] != 1 {
		t.Errorf("grouped counts = %v, want runA=2 runB=1", byRun)
	}
	if _, ok := byRun[runC]; ok {
		t.Errorf("runC (no artifacts) contributed %d rows, want 0", byRun[runC])
	}

	empty, err := stores.Artifacts.ListByRuns(ctx, orgID, nil)
	if err != nil {
		t.Fatalf("ListByRuns(nil): %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("ListByRuns(nil) = %v, want empty", empty)
	}
}

// TestArtifactStore_Postgres_ListByRuns_TeamScoped pins that ListByRuns runs on
// the RLS-active app pool — symmetric to CountByRun_TeamScoped, the
// defense-in-depth scoping guarantee at the store layer. A same-org member of a
// different team sees no artifacts for a run whose artifacts belong to another
// team, even passing the run id directly. ListByRuns feeds the pending_kind /
// pending_artifact_id overlay discriminator, so a leak here would mis-surface
// another team's approval card. TFAC-465.
func TestArtifactStore_Postgres_ListByRuns_TeamScoped(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgA, alice, teamA := pgtest.SeedOrgWithUser(t, h, "alice")
	carol := pgtest.SeedUser(t, h, "carol")
	teamB := pgtest.SeedTeam(t, h, orgA, "team-b")
	pgtest.AddOrgMember(t, h, carol, orgA, teamB, "member", "member")
	runID := seedPgArtifactRun(t, h, orgA, teamA, alice)

	// Two teamA-scoped artifacts on the run (admin insert bypasses RLS for setup).
	for _, key := range []string{"github:comment:octo/repo:1", "github:comment:octo/repo:2"} {
		if _, err := h.AdminDB.Exec(`
			INSERT INTO artifacts (org_id, conversation_id, team_id, provider, kind, target, state, dedup_key)
			VALUES ($1, $2, $3, 'github', 'comment', 'octo/repo', 'posted', $4)
		`, orgA, runID, teamA, key); err != nil {
			t.Fatalf("seed artifact: %v", err)
		}
	}
	ctx := context.Background()

	// Alice (teamA) sees both.
	if err := h.WithUser(t, alice, orgA, func(tx *sql.Tx) error {
		arts, err := pgstore.NewForTx(tx, pgtest.SecretKey).Artifacts.ListByRuns(ctx, orgA, []string{runID})
		if err != nil {
			return err
		}
		if len(arts) != 2 {
			t.Errorf("alice saw %d artifacts, want 2", len(arts))
		}
		return nil
	}); err != nil {
		t.Fatalf("alice path: %v", err)
	}

	// Carol (teamB, same org, different team) sees zero — artifacts_select gates
	// on user_in_team(team_id), so the rows are invisible to her.
	if err := h.WithUser(t, carol, orgA, func(tx *sql.Tx) error {
		arts, err := pgstore.NewForTx(tx, pgtest.SecretKey).Artifacts.ListByRuns(ctx, orgA, []string{runID})
		if err != nil {
			return err
		}
		if len(arts) != 0 {
			t.Errorf("carol (different team) saw %d artifacts — RLS leaked across teams", len(arts))
		}
		return nil
	}); err != nil {
		t.Fatalf("carol path: %v", err)
	}
}

// TestArtifactStore_Postgres_ListByOrgSystem pins the bot-activity audit feed's
// org source (TFAC-483): on the admin pool it returns the org's artifacts across
// EVERY team (cross-team), ALL states — terminal included (merged PR, dismissed
// review), in contrast to ListNonTerminalBySystem — never crosses org
// boundaries, and honors the provider/kind/state/time filters + limit/offset
// paging from ArtifactListOpts.
func TestArtifactStore_Postgres_ListByOrgSystem(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgA, _, teamA := pgtest.SeedOrgWithUser(t, h, "alice")
	teamB := pgtest.SeedTeam(t, h, orgA, "team-b")
	orgB, _, teamBOther := pgtest.SeedOrgWithUser(t, h, "bob")
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	ctx := context.Background()

	seedArt := func(orgID, teamID, provider, kind, state, key, createdAt string) {
		if _, err := h.AdminDB.Exec(`
			INSERT INTO artifacts (org_id, team_id, provider, kind, target, state, dedup_key, created_at)
			VALUES ($1, $2, $3, $4, 'octo/repo', $5, $6, $7)
		`, orgID, teamID, provider, kind, state, key, createdAt); err != nil {
			t.Fatalf("seed %s: %v", key, err)
		}
	}
	// teamA + teamB rows across the lifecycle (terminal included), plus an orgB
	// row that must never leak into orgA's feed.
	seedArt(orgA, teamA, domain.ArtifactProviderGit, domain.ArtifactKindBranch, domain.ArtifactStateBranchPushed, "a-branch", "2026-06-01T00:00:00Z")
	seedArt(orgA, teamB, domain.ArtifactProviderGitHub, domain.ArtifactKindPullRequest, domain.ArtifactStatePROpen, "b-pr-open", "2026-06-02T00:00:00Z")
	seedArt(orgA, teamA, domain.ArtifactProviderGitHub, domain.ArtifactKindPullRequest, domain.ArtifactStatePRMerged, "a-pr-merged", "2026-06-03T00:00:00Z") // terminal
	seedArt(orgA, teamB, domain.ArtifactProviderGitHub, domain.ArtifactKindReview, domain.ArtifactStateReviewDismissed, "b-review", "2026-06-04T00:00:00Z")  // terminal
	seedArt(orgA, teamA, domain.ArtifactProviderJira, domain.ArtifactKindComment, domain.ArtifactStateCommentPosted, "a-comment", "2026-06-05T00:00:00Z")
	seedArt(orgB, teamBOther, domain.ArtifactProviderGitHub, domain.ArtifactKindPullRequest, domain.ArtifactStatePROpen, "other-org", "2026-06-06T00:00:00Z")

	// No opts: every orgA row across both teams, terminal included, newest-first.
	all, err := stores.Artifacts.ListByOrgSystem(ctx, orgA, db.ArtifactListOpts{})
	if err != nil {
		t.Fatalf("ListByOrgSystem: %v", err)
	}
	if len(all) != 5 {
		t.Fatalf("returned %d rows, want 5 (3 teamA + 2 teamB, no orgB)", len(all))
	}
	teamsSeen := map[string]bool{}
	for _, a := range all {
		if a.OrgID != orgA {
			t.Errorf("cross-org leak: row %q belongs to org %q", a.DedupKey, a.OrgID)
		}
		teamsSeen[a.TeamID] = true
	}
	if !teamsSeen[teamA] || !teamsSeen[teamB] {
		t.Errorf("feed is not cross-team: saw teams %v, want both %s and %s", teamsSeen, teamA, teamB)
	}
	if all[0].DedupKey != "a-comment" || all[4].DedupKey != "a-branch" {
		t.Errorf("order not newest-first: first=%q last=%q", all[0].DedupKey, all[4].DedupKey)
	}

	// Contrast the reconciler set: terminal rows (merged PR, dismissed review)
	// drop out, leaving branch pushed + PR open = 2.
	nonTerminal, err := stores.Artifacts.ListNonTerminalBySystem(ctx, orgA)
	if err != nil {
		t.Fatalf("ListNonTerminalBySystem: %v", err)
	}
	if len(nonTerminal) != 2 {
		t.Errorf("ListNonTerminalBySystem returned %d, want 2 (the terminal-hiding contrast)", len(nonTerminal))
	}

	// Filters.
	gh, err := stores.Artifacts.ListByOrgSystem(ctx, orgA, db.ArtifactListOpts{Provider: domain.ArtifactProviderGitHub})
	if err != nil {
		t.Fatalf("filter provider: %v", err)
	}
	if len(gh) != 3 {
		t.Errorf("provider=github returned %d, want 3", len(gh))
	}
	prs, err := stores.Artifacts.ListByOrgSystem(ctx, orgA, db.ArtifactListOpts{Kind: domain.ArtifactKindPullRequest})
	if err != nil {
		t.Fatalf("filter kind: %v", err)
	}
	if len(prs) != 2 {
		t.Errorf("kind=pull_request returned %d, want 2", len(prs))
	}
	open, err := stores.Artifacts.ListByOrgSystem(ctx, orgA, db.ArtifactListOpts{State: domain.ArtifactStatePROpen})
	if err != nil {
		t.Fatalf("filter state: %v", err)
	}
	if len(open) != 1 || open[0].DedupKey != "b-pr-open" {
		t.Errorf("state=open returned %+v, want exactly the teamB open PR", open)
	}

	// Time window [06-03, 06-05): merged (06-03) + review (06-04), newest-first.
	windowed, err := stores.Artifacts.ListByOrgSystem(ctx, orgA, db.ArtifactListOpts{
		Since: time.Date(2026, 6, 3, 0, 0, 0, 0, time.UTC),
		Until: time.Date(2026, 6, 5, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("filter window: %v", err)
	}
	if len(windowed) != 2 || windowed[0].DedupKey != "b-review" || windowed[1].DedupKey != "a-pr-merged" {
		t.Errorf("time window = %+v, want [b-review, a-pr-merged]", dedupKeys(windowed))
	}

	// Paging: page 1 (limit 2) → comment + review; page 2 (offset 2) → merged + open.
	page1, err := stores.Artifacts.ListByOrgSystem(ctx, orgA, db.ArtifactListOpts{Limit: 2})
	if err != nil {
		t.Fatalf("page 1: %v", err)
	}
	page2, err := stores.Artifacts.ListByOrgSystem(ctx, orgA, db.ArtifactListOpts{Limit: 2, Offset: 2})
	if err != nil {
		t.Fatalf("page 2: %v", err)
	}
	if len(page1) != 2 || len(page2) != 2 || page1[0].DedupKey != "a-comment" || page2[0].DedupKey != "a-pr-merged" {
		t.Errorf("paging wrong: page1=%v page2=%v", dedupKeys(page1), dedupKeys(page2))
	}
}

func dedupKeys(arts []domain.Artifact) []string {
	out := make([]string, len(arts))
	for i, a := range arts {
		out[i] = a.DedupKey
	}
	return out
}

// TestArtifactStore_Postgres_ListPendingReviewsByTarget pins the new-commits
// notifier's lookup (TFAC-501): only PENDING review artifacts on the exact PR
// target come back, org-scoped, on the admin pool.
func TestArtifactStore_Postgres_ListPendingReviewsByTarget(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, userID, teamID := pgtest.SeedOrgWithUser(t, h, "alice")
	runID := seedPgArtifactRun(t, h, orgID, teamID, userID)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	ctx := context.Background()

	mk := func(kind, state, target, dedup string) {
		if _, err := stores.Artifacts.Upsert(ctx, orgID, domain.Artifact{
			ConversationID: runID, OrgID: orgID, TeamID: teamID,
			Provider: domain.ArtifactProviderGitHub, Kind: kind, State: state,
			Target: target, DedupKey: dedup,
		}); err != nil {
			t.Fatalf("seed artifact %s/%s: %v", kind, state, err)
		}
	}
	mk(domain.ArtifactKindReview, domain.ArtifactStateReviewPending, "octo/repo#7", "rev-7-a")
	mk(domain.ArtifactKindReview, domain.ArtifactStateReviewPending, "octo/repo#7", "rev-7-b")
	mk(domain.ArtifactKindReview, domain.ArtifactStateReviewSubmitted, "octo/repo#7", "rev-7-submitted")
	mk(domain.ArtifactKindReview, domain.ArtifactStateReviewPending, "octo/repo#8", "rev-8")
	mk(domain.ArtifactKindPullRequest, domain.ArtifactStatePROpen, "octo/repo#7", "pr-7")

	got, err := stores.Artifacts.ListPendingReviewsByTargetSystem(ctx, orgID, "octo/repo#7")
	if err != nil {
		t.Fatalf("ListPendingReviewsByTargetSystem: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 pending reviews on octo/repo#7, got %d: %+v", len(got), got)
	}
	for _, a := range got {
		if a.Kind != domain.ArtifactKindReview || a.State != domain.ArtifactStateReviewPending || a.Target != "octo/repo#7" {
			t.Errorf("unexpected artifact: kind=%s state=%s target=%s", a.Kind, a.State, a.Target)
		}
	}
}

// TestArtifactStore_Postgres_TransitionReviewState pins the review CAS: exactly
// one of two same-from transitions wins; the loser (and a wrong-kind row)
// reports false without touching anything; the stamp trio applies
// preserve-on-empty in the same write.
func TestArtifactStore_Postgres_TransitionReviewState(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, userID, teamID := pgtest.SeedOrgWithUser(t, h, "alice")
	runID := seedPgArtifactRun(t, h, orgID, teamID, userID)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	ctx := context.Background()

	rev, err := stores.Artifacts.Upsert(ctx, orgID, domain.Artifact{
		ConversationID: runID, OrgID: orgID, TeamID: teamID,
		Provider: domain.ArtifactProviderGitHub, Kind: domain.ArtifactKindReview,
		State: domain.ArtifactStateReviewPending, Target: "octo/repo#7",
		DedupKey: "rev-cas", DetailsJSON: `{"number":7}`,
	})
	if err != nil {
		t.Fatalf("seed review: %v", err)
	}
	pr, err := stores.Artifacts.Upsert(ctx, orgID, domain.Artifact{
		ConversationID: runID, OrgID: orgID, TeamID: teamID,
		Provider: domain.ArtifactProviderGitHub, Kind: domain.ArtifactKindPullRequest,
		State: domain.ArtifactStateReviewPending, Target: "octo/repo#7", DedupKey: "pr-cas",
	})
	if err != nil {
		t.Fatalf("seed pr: %v", err)
	}

	won, err := stores.Artifacts.TransitionReviewState(ctx, orgID, rev.ID,
		domain.ArtifactStateReviewPending, domain.ArtifactStateReviewSubmitted, "", "", "")
	if err != nil || !won {
		t.Fatalf("claim = (%v, %v), want (true, nil)", won, err)
	}
	won, err = stores.Artifacts.TransitionReviewState(ctx, orgID, rev.ID,
		domain.ArtifactStateReviewPending, domain.ArtifactStateReviewSubmitted, "", "", "")
	if err != nil || won {
		t.Fatalf("second claim = (%v, %v), want (false, nil) — the CAS must admit one winner", won, err)
	}
	got, _ := stores.Artifacts.Get(ctx, orgID, rev.ID)
	if got.State != domain.ArtifactStateReviewSubmitted || got.DetailsJSON != `{"number":7}` || got.URL != "" {
		t.Fatalf("post-claim row = state %q details %q url %q, want submitted/original details/empty url", got.State, got.DetailsJSON, got.URL)
	}

	won, err = stores.Artifacts.TransitionReviewState(ctx, orgID, rev.ID,
		domain.ArtifactStateReviewSubmitted, domain.ArtifactStateReviewSubmitted,
		"12345", "https://github.com/octo/repo/pull/7#pullrequestreview-12345", `{"number":7,"review_event":"COMMENT"}`)
	if err != nil || !won {
		t.Fatalf("stamp = (%v, %v), want (true, nil)", won, err)
	}
	got, _ = stores.Artifacts.Get(ctx, orgID, rev.ID)
	if got.ExternalID != "12345" || got.URL != "https://github.com/octo/repo/pull/7#pullrequestreview-12345" ||
		got.DetailsJSON != `{"number":7,"review_event":"COMMENT"}` {
		t.Fatalf("post-stamp row = %+v, want the posted review's coordinates", got)
	}

	won, err = stores.Artifacts.TransitionReviewState(ctx, orgID, pr.ID,
		domain.ArtifactStateReviewPending, domain.ArtifactStateReviewSubmitted, "", "", "")
	if err != nil || won {
		t.Fatalf("pr transition = (%v, %v), want (false, nil) — CAS is review-only", won, err)
	}
}

// TestArtifactStore_Postgres_UpdateReviewDetailsIfPending pins the guarded
// draft write: it lands only while the review is still pending and refuses —
// leaving the row untouched — once the review resolved.
func TestArtifactStore_Postgres_UpdateReviewDetailsIfPending(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, userID, teamID := pgtest.SeedOrgWithUser(t, h, "alice")
	runID := seedPgArtifactRun(t, h, orgID, teamID, userID)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	ctx := context.Background()

	rev, err := stores.Artifacts.Upsert(ctx, orgID, domain.Artifact{
		ConversationID: runID, OrgID: orgID, TeamID: teamID,
		Provider: domain.ArtifactProviderGitHub, Kind: domain.ArtifactKindReview,
		State: domain.ArtifactStateReviewPending, Target: "octo/repo#7",
		DedupKey: "rev-details", DetailsJSON: `{"v":1}`,
	})
	if err != nil {
		t.Fatalf("seed review: %v", err)
	}

	updated, err := stores.Artifacts.UpdateReviewDetailsIfPendingSystem(ctx, orgID, rev.ID, `{"v":2}`)
	if err != nil || !updated {
		t.Fatalf("pending update = (%v, %v), want (true, nil)", updated, err)
	}
	got, _ := stores.Artifacts.Get(ctx, orgID, rev.ID)
	if got.DetailsJSON != `{"v":2}` {
		t.Fatalf("details = %q, want {\"v\":2}", got.DetailsJSON)
	}

	if won, err := stores.Artifacts.TransitionReviewState(ctx, orgID, rev.ID,
		domain.ArtifactStateReviewPending, domain.ArtifactStateReviewSubmitted, "", "", ""); err != nil || !won {
		t.Fatalf("resolve: (%v, %v)", won, err)
	}
	updated, err = stores.Artifacts.UpdateReviewDetailsIfPendingSystem(ctx, orgID, rev.ID, `{"v":3}`)
	if err != nil || updated {
		t.Fatalf("post-resolve update = (%v, %v), want (false, nil)", updated, err)
	}
	got, _ = stores.Artifacts.Get(ctx, orgID, rev.ID)
	if got.DetailsJSON != `{"v":2}` || got.State != domain.ArtifactStateReviewSubmitted {
		t.Fatalf("row after refused write = state %q details %q, want submitted/{\"v\":2} untouched", got.State, got.DetailsJSON)
	}
}

// seedPgArtifactRun mints a minimal run the artifacts.conversation_id FK can point
// at. origin is non-'blueprint' so runs_origin_requires_parents doesn't
// demand a parent chain; trigger_type='manual' needs a non-NULL creator.
func seedPgArtifactRun(t *testing.T, h *pgtest.Harness, orgID, teamID, userID string) string {
	t.Helper()
	id := uuid.New().String()
	if _, err := h.AdminDB.Exec(`
		INSERT INTO conversations (id, org_id, team_id, creator_user_id, trigger_type, origin, status, visibility)
		VALUES ($1, $2, $3, $4, 'manual', 'interactive', 'running', 'team')
	`, id, orgID, teamID, userID); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	return id
}
