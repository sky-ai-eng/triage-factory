package sqlite_test

import (
	"context"
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestArtifactStore_SQLite_RoundTrip pins that Upsert inserts a row whose
// every field reads back intact, that an empty id is generated, and that
// the empty optional fields land as SQL NULL. TFAC-455.
func TestArtifactStore_SQLite_RoundTrip(t *testing.T) {
	conn := newSQLiteForArtifactTest(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()
	runID := seedArtifactRun(t, conn)

	in := domain.Artifact{
		RunID:       runID,
		OrgID:       runmode.LocalDefaultOrgID,
		TeamID:      runmode.LocalDefaultTeamID,
		Provider:    domain.ArtifactProviderGitHub,
		Kind:        domain.ArtifactKindPullRequest,
		Target:      "octo/repo#123",
		ExternalID:  "123",
		URL:         "https://github.com/octo/repo/pull/123",
		State:       domain.ArtifactStatePROpen,
		DedupKey:    domain.ArtifactDedupKey("github", "pull_request", "octo/repo", "refs/heads/feat"),
		DetailsJSON: `{"draft":false}`,
	}
	out, err := stores.Artifacts.Upsert(ctx, runmode.LocalDefaultOrgID, in)
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if out.ID == "" {
		t.Error("expected a generated id, got empty string")
	}
	if out.CreatedAt.IsZero() || out.UpdatedAt.IsZero() {
		t.Error("expected created_at/updated_at populated")
	}
	if out.RunID != runID || out.Provider != "github" || out.Kind != "pull_request" ||
		out.Target != "octo/repo#123" || out.ExternalID != "123" ||
		out.URL != in.URL || out.State != "open" || out.DedupKey != in.DedupKey ||
		out.DetailsJSON != `{"draft":false}` {
		t.Errorf("round-trip mismatch: %+v", out)
	}

	// Empty optionals → SQL NULL.
	if _, err := stores.Artifacts.Upsert(ctx, runmode.LocalDefaultOrgID, domain.Artifact{
		OrgID:    runmode.LocalDefaultOrgID,
		TeamID:   runmode.LocalDefaultTeamID,
		Provider: domain.ArtifactProviderGit,
		Kind:     domain.ArtifactKindBranch,
		Target:   "octo/repo",
		State:    domain.ArtifactStateBranchPushed,
		DedupKey: "git:branch:octo/repo:refs/heads/x",
	}); err != nil {
		t.Fatalf("Upsert branch: %v", err)
	}
	var extID, url, details sql.NullString
	var nullRun sql.NullString
	if err := conn.QueryRow(
		`SELECT run_id, external_id, url, details_json FROM artifacts WHERE dedup_key = 'git:branch:octo/repo:refs/heads/x'`,
	).Scan(&nullRun, &extID, &url, &details); err != nil {
		t.Fatalf("read back branch: %v", err)
	}
	if nullRun.Valid || extID.Valid || url.Valid || details.Valid {
		t.Errorf("empty optionals should be NULL: run=%v ext=%v url=%v details=%v", nullRun, extID, url, details)
	}
}

// TestArtifactStore_SQLite_UpsertDedup pins that two upserts with the same
// dedup_key collapse to one row, and the second updates the mutable
// fields. TFAC-455.
func TestArtifactStore_SQLite_UpsertDedup(t *testing.T) {
	conn := newSQLiteForArtifactTest(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()
	runID := seedArtifactRun(t, conn)

	key := domain.ArtifactDedupKey("github", "pull_request", "octo/repo", "refs/heads/feat")
	first, err := stores.Artifacts.Upsert(ctx, runmode.LocalDefaultOrgID, domain.Artifact{
		RunID: runID, OrgID: runmode.LocalDefaultOrgID, TeamID: runmode.LocalDefaultTeamID,
		Provider: "github", Kind: "pull_request", Target: "octo/repo",
		State: domain.ArtifactStatePRDraft, DedupKey: key,
	})
	if err != nil {
		t.Fatalf("first Upsert: %v", err)
	}

	second, err := stores.Artifacts.Upsert(ctx, runmode.LocalDefaultOrgID, domain.Artifact{
		RunID: runID, OrgID: runmode.LocalDefaultOrgID, TeamID: runmode.LocalDefaultTeamID,
		Provider: "github", Kind: "pull_request", Target: "octo/repo#7",
		ExternalID: "7", URL: "https://github.com/octo/repo/pull/7",
		State: domain.ArtifactStatePROpen, DedupKey: key,
	})
	if err != nil {
		t.Fatalf("second Upsert: %v", err)
	}

	if first.ID != second.ID {
		t.Errorf("dedup failed: distinct ids %s vs %s", first.ID, second.ID)
	}
	var count int
	if err := conn.QueryRow(`SELECT COUNT(*) FROM artifacts WHERE dedup_key = ?`, key).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Errorf("want 1 row for dedup_key, got %d", count)
	}
	if second.State != "open" || second.ExternalID != "7" || second.Target != "octo/repo#7" {
		t.Errorf("second upsert did not update mutable fields: %+v", second)
	}
	if !second.UpdatedAt.Equal(second.CreatedAt) && second.UpdatedAt.Before(second.CreatedAt) {
		t.Errorf("updated_at %v should be >= created_at %v", second.UpdatedAt, second.CreatedAt)
	}
}

// TestArtifactStore_SQLite_PendingToReal pins the pending→real PR
// transition: a pending PR keyed on the branch ref, then the real PR keyed
// on the same ref, collapse to one row that flips pending→open with
// url/external_id filled. TFAC-455.
func TestArtifactStore_SQLite_PendingToReal(t *testing.T) {
	conn := newSQLiteForArtifactTest(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()
	runID := seedArtifactRun(t, conn)

	// The branch ref is the stable anchor across the transition.
	key := domain.ArtifactDedupKey("github", "pull_request", "octo/repo", "refs/heads/feat")

	pending, err := stores.Artifacts.Upsert(ctx, runmode.LocalDefaultOrgID, domain.Artifact{
		RunID: runID, OrgID: runmode.LocalDefaultOrgID, TeamID: runmode.LocalDefaultTeamID,
		Provider: "github", Kind: "pull_request", Target: "octo/repo",
		State: domain.ArtifactStatePRPending, DedupKey: key,
		// No external_id / url yet — the PR doesn't exist.
	})
	if err != nil {
		t.Fatalf("pending Upsert: %v", err)
	}
	if pending.State != "pending" || pending.ExternalID != "" || pending.URL != "" {
		t.Fatalf("pending row malformed: %+v", pending)
	}

	real, err := stores.Artifacts.Upsert(ctx, runmode.LocalDefaultOrgID, domain.Artifact{
		RunID: runID, OrgID: runmode.LocalDefaultOrgID, TeamID: runmode.LocalDefaultTeamID,
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
	if err := conn.QueryRow(`SELECT COUNT(*) FROM artifacts`).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Errorf("want 1 row after pending→real, got %d", count)
	}
}

// TestArtifactStore_SQLite_UpsertPreservesExternalIDAndURL pins that an
// upsert leaving external_id/url empty does NOT blank values an earlier upsert
// stored — they only ever fill in (PR number / issue key, html link). A
// reconciliation pass, or a Jira mutation whose run can't compute the browse
// URL, must not erase them. State/target still follow the latest writer.
func TestArtifactStore_SQLite_UpsertPreservesExternalIDAndURL(t *testing.T) {
	conn := newSQLiteForArtifactTest(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()
	runID := seedArtifactRun(t, conn)

	key := domain.ArtifactDedupKey("jira", "issue", "SKY-1", "")
	if _, err := stores.Artifacts.Upsert(ctx, runmode.LocalDefaultOrgID, domain.Artifact{
		RunID: runID, OrgID: runmode.LocalDefaultOrgID, TeamID: runmode.LocalDefaultTeamID,
		Provider: "jira", Kind: "issue", Target: "SKY-1",
		ExternalID: "SKY-1", URL: "https://jira.example.com/browse/SKY-1",
		State: domain.ArtifactStateIssueCreated, DedupKey: key,
	}); err != nil {
		t.Fatalf("first Upsert: %v", err)
	}

	// A later mutation that can't supply external_id/url (both empty).
	out, err := stores.Artifacts.Upsert(ctx, runmode.LocalDefaultOrgID, domain.Artifact{
		RunID: runID, OrgID: runmode.LocalDefaultOrgID, TeamID: runmode.LocalDefaultTeamID,
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
		t.Errorf("state should still follow the latest writer, got %q", out.State)
	}

	// A non-empty value still overwrites (intentional change path).
	out, err = stores.Artifacts.Upsert(ctx, runmode.LocalDefaultOrgID, domain.Artifact{
		RunID: runID, OrgID: runmode.LocalDefaultOrgID, TeamID: runmode.LocalDefaultTeamID,
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

// TestArtifactStore_SQLite_UpsertPreservesTargetOnEmpty pins that an upsert
// leaving target empty does NOT blank the resource key an earlier upsert
// stored — a GitHub comment-update/delete knows only the comment id, not the
// PR number, so it can't recompute owner/repo#N and must inherit it. A
// non-empty target still migrates (the pending→real PR path).
func TestArtifactStore_SQLite_UpsertPreservesTargetOnEmpty(t *testing.T) {
	conn := newSQLiteForArtifactTest(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()
	runID := seedArtifactRun(t, conn)

	key := domain.ArtifactDedupKey("github", "comment", "555", "")
	if _, err := stores.Artifacts.Upsert(ctx, runmode.LocalDefaultOrgID, domain.Artifact{
		RunID: runID, OrgID: runmode.LocalDefaultOrgID, TeamID: runmode.LocalDefaultTeamID,
		Provider: "github", Kind: "comment", Target: "octo/repo#7",
		ExternalID: "555", URL: "https://github.com/octo/repo/pull/7#issuecomment-555",
		State: domain.ArtifactStateCommentPosted, DedupKey: key,
	}); err != nil {
		t.Fatalf("first Upsert: %v", err)
	}

	// A comment-delete that only carries the id (empty target/url).
	out, err := stores.Artifacts.Upsert(ctx, runmode.LocalDefaultOrgID, domain.Artifact{
		RunID: runID, OrgID: runmode.LocalDefaultOrgID, TeamID: runmode.LocalDefaultTeamID,
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
	out, err = stores.Artifacts.Upsert(ctx, runmode.LocalDefaultOrgID, domain.Artifact{
		RunID: runID, OrgID: runmode.LocalDefaultOrgID, TeamID: runmode.LocalDefaultTeamID,
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

// TestArtifactStore_SQLite_ListByRunAndTeam pins the two read paths and
// their newest-first ordering + the team Limit. TFAC-455.
func TestArtifactStore_SQLite_ListByRunAndTeam(t *testing.T) {
	conn := newSQLiteForArtifactTest(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()
	runID := seedArtifactRun(t, conn)

	for i, k := range []string{"a", "b", "c"} {
		if _, err := stores.Artifacts.Upsert(ctx, runmode.LocalDefaultOrgID, domain.Artifact{
			RunID: runID, OrgID: runmode.LocalDefaultOrgID, TeamID: runmode.LocalDefaultTeamID,
			Provider: "github", Kind: "comment", Target: "octo/repo",
			State: domain.ArtifactStateCommentPosted, DedupKey: "k" + k,
		}); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}

	byRun, err := stores.Artifacts.ListByRun(ctx, runmode.LocalDefaultOrgID, runID)
	if err != nil {
		t.Fatalf("ListByRun: %v", err)
	}
	if len(byRun) != 3 {
		t.Errorf("ListByRun len = %d, want 3", len(byRun))
	}

	byTeam, err := stores.Artifacts.ListByTeam(ctx, runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, db.ArtifactListOpts{})
	if err != nil {
		t.Fatalf("ListByTeam: %v", err)
	}
	if len(byTeam) != 3 {
		t.Errorf("ListByTeam len = %d, want 3", len(byTeam))
	}

	limited, err := stores.Artifacts.ListByTeam(ctx, runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, db.ArtifactListOpts{Limit: 2})
	if err != nil {
		t.Fatalf("ListByTeam limited: %v", err)
	}
	if len(limited) != 2 {
		t.Errorf("ListByTeam(Limit:2) len = %d, want 2", len(limited))
	}
}

// TestArtifactStore_SQLite_CountByRun pins the batched per-run count the run
// list path uses for artifact_count (TFAC-465): counts keyed by run id, a run
// with no artifacts absent from the map (not 0-valued), unknown ids absent, and
// an empty runIDs a no-op returning an empty map.
func TestArtifactStore_SQLite_CountByRun(t *testing.T) {
	conn := newSQLiteForArtifactTest(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()
	runA := seedArtifactRun(t, conn)
	runB := seedArtifactRunWithID(t, conn, "88888888-8888-8888-8888-888888888888")
	runC := seedArtifactRunWithID(t, conn, "77777777-7777-7777-7777-777777777777") // no artifacts

	seed := func(runID, key string) {
		if _, err := stores.Artifacts.Upsert(ctx, runmode.LocalDefaultOrgID, domain.Artifact{
			RunID: runID, OrgID: runmode.LocalDefaultOrgID, TeamID: runmode.LocalDefaultTeamID,
			Provider: "github", Kind: "comment", Target: "octo/repo",
			State: domain.ArtifactStateCommentPosted, DedupKey: key,
		}); err != nil {
			t.Fatalf("seed %s: %v", key, err)
		}
	}
	seed(runA, "a1")
	seed(runA, "a2")
	seed(runA, "a3")
	seed(runB, "b1")

	counts, err := stores.Artifacts.CountByRun(ctx, runmode.LocalDefaultOrgID, []string{runA, runB, runC, "nonexistent"})
	if err != nil {
		t.Fatalf("CountByRun: %v", err)
	}
	if counts[runA] != 3 {
		t.Errorf("runA count = %d, want 3", counts[runA])
	}
	if counts[runB] != 1 {
		t.Errorf("runB count = %d, want 1", counts[runB])
	}
	if _, ok := counts[runC]; ok {
		t.Errorf("runC (no artifacts) should be absent from the map, got %d", counts[runC])
	}
	if _, ok := counts["nonexistent"]; ok {
		t.Error("unknown run id should be absent from the map")
	}

	// Empty runIDs is a no-op — empty, non-nil map, no query.
	empty, err := stores.Artifacts.CountByRun(ctx, runmode.LocalDefaultOrgID, nil)
	if err != nil {
		t.Fatalf("CountByRun(nil): %v", err)
	}
	if empty == nil || len(empty) != 0 {
		t.Errorf("CountByRun(nil) = %v, want empty non-nil map", empty)
	}
}

// TestArtifactStore_SQLite_ListByTeam_IncludesDetached pins the
// audit-ledger invariant: an artifact whose run was purged (run_id NULL)
// is still the team's and must come back from ListByTeam. Guards against a
// future `AND run_id IS NOT NULL` creeping into the query. TFAC-455.
func TestArtifactStore_SQLite_ListByTeam_IncludesDetached(t *testing.T) {
	conn := newSQLiteForArtifactTest(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()
	runID := seedArtifactRun(t, conn)

	art, err := stores.Artifacts.Upsert(ctx, runmode.LocalDefaultOrgID, domain.Artifact{
		RunID: runID, OrgID: runmode.LocalDefaultOrgID, TeamID: runmode.LocalDefaultTeamID,
		Provider: "git", Kind: "branch", Target: "octo/repo",
		State: domain.ArtifactStateBranchPushed, DedupKey: "git:branch:octo/repo:refs/heads/x",
	})
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	// Simulate a run purge: the FK is ON DELETE SET NULL, so deleting the
	// run detaches the artifact rather than cascading it away.
	if _, err := conn.Exec(`DELETE FROM runs WHERE id = ?`, runID); err != nil {
		t.Fatalf("purge run: %v", err)
	}
	var nullRun sql.NullString
	if err := conn.QueryRow(`SELECT run_id FROM artifacts WHERE id = ?`, art.ID).Scan(&nullRun); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if nullRun.Valid {
		t.Fatalf("run_id should be NULL after run purge, got %q", nullRun.String)
	}

	rows, err := stores.Artifacts.ListByTeam(ctx, runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, db.ArtifactListOpts{})
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

// TestArtifactStore_SQLite_DropsRunArtifacts pins that the forward
// migration retired the dead run_artifacts table and created artifacts.
func TestArtifactStore_SQLite_DropsRunArtifacts(t *testing.T) {
	conn := newSQLiteForArtifactTest(t)
	var n int
	if err := conn.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'run_artifacts'`,
	).Scan(&n); err != nil {
		t.Fatalf("query sqlite_master: %v", err)
	}
	if n != 0 {
		t.Errorf("run_artifacts still present after migration; want dropped")
	}
	if err := conn.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'artifacts'`,
	).Scan(&n); err != nil {
		t.Fatalf("query sqlite_master: %v", err)
	}
	if n != 1 {
		t.Errorf("artifacts table missing after migration")
	}
}

// TestArtifactStore_SQLite_ListNonTerminal pins the reconciler's working set
// (TFAC-464): seed one artifact in every state across kinds, then assert
// ListNonTerminalBySystem returns EXACTLY the rows domain.IsReconcilableNonTerminal
// marks — PR draft|open, review pending, branch pushed — and nothing terminal
// (merged/closed/submitted/dismissed/deleted) or unreconcilable (PR pending,
// comment, issue). Doubles as the SQL-predicate ⇔ domain-predicate drift guard.
func TestArtifactStore_SQLite_ListNonTerminal(t *testing.T) {
	conn := newSQLiteForArtifactTest(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()
	runID := seedArtifactRun(t, conn)

	// (kind, state) across the whole lifecycle space.
	seeds := []struct{ kind, state string }{
		{domain.ArtifactKindPullRequest, domain.ArtifactStatePRPending}, // unreconcilable: no node id yet
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
		key := "k" + string(rune('a'+i)) // unique dedup_key per seed
		if _, err := stores.Artifacts.Upsert(ctx, runmode.LocalDefaultOrgID, domain.Artifact{
			RunID: runID, OrgID: runmode.LocalDefaultOrgID, TeamID: runmode.LocalDefaultTeamID,
			Provider: "github", Kind: s.kind, Target: "octo/repo", State: s.state, DedupKey: key,
		}); err != nil {
			t.Fatalf("seed %s/%s: %v", s.kind, s.state, err)
		}
		if domain.IsReconcilableNonTerminal(s.kind, s.state) {
			want[key] = true
		}
	}

	got, err := stores.Artifacts.ListNonTerminalBySystem(ctx, runmode.LocalDefaultOrgID)
	if err != nil {
		t.Fatalf("ListNonTerminalBySystem: %v", err)
	}
	gotKeys := map[string]bool{}
	for _, a := range got {
		gotKeys[a.DedupKey] = true
	}
	if len(gotKeys) != len(want) {
		t.Errorf("returned %d rows, want %d: got=%v want=%v", len(gotKeys), len(want), gotKeys, want)
	}
	for k := range want {
		if !gotKeys[k] {
			t.Errorf("ListNonTerminalBySystem dropped reconcilable row %q", k)
		}
	}
	for k := range gotKeys {
		if !want[k] {
			t.Errorf("ListNonTerminalBySystem returned terminal/unreconcilable row %q (SQL predicate disagrees with domain.IsReconcilableNonTerminal)", k)
		}
	}
}

func newSQLiteForArtifactTest(t *testing.T) *sql.DB {
	t.Helper()
	conn, err := sql.Open("sqlite", ":memory:?_pragma=foreign_keys(on)")
	if err != nil {
		t.Fatalf("open in-memory db: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	conn.SetMaxOpenConns(1)
	conn.SetMaxIdleConns(1)
	if err := db.BootstrapSchemaForTest(conn); err != nil {
		t.Fatalf("bootstrap schema: %v", err)
	}
	return conn
}

// seedArtifactRun inserts a minimal run the artifacts FK (run_id →
// runs(id)) can point at. origin is set non-'blueprint' so the
// runs_origin_requires_parents CHECK doesn't demand a parent chain; the
// org/team/creator columns default to the local sentinels.
func seedArtifactRun(t *testing.T, conn *sql.DB) string {
	t.Helper()
	return seedArtifactRunWithID(t, conn, "99999999-9999-9999-9999-999999999999")
}

// seedArtifactRunWithID is seedArtifactRun parameterized on the run id, for
// tests that need more than one run (CountByRun batches across runs).
func seedArtifactRunWithID(t *testing.T, conn *sql.DB, id string) string {
	t.Helper()
	if _, err := conn.Exec(
		`INSERT INTO runs (id, origin, status) VALUES (?, 'interactive', 'running')`, id,
	); err != nil {
		t.Fatalf("seed run %s: %v", id, err)
	}
	return id
}
