package sqlite_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

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
		ConversationID: runID,
		OrgID:          runmode.LocalDefaultOrgID,
		TeamID:         runmode.LocalDefaultTeamID,
		Provider:       domain.ArtifactProviderGitHub,
		Kind:           domain.ArtifactKindPullRequest,
		Target:         "octo/repo#123",
		ExternalID:     "123",
		URL:            "https://github.com/octo/repo/pull/123",
		State:          domain.ArtifactStatePROpen,
		DedupKey:       domain.ArtifactDedupKey("github", "pull_request", "octo/repo", "refs/heads/feat"),
		DetailsJSON:    `{"draft":false}`,
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
	if out.ConversationID != runID || out.Provider != "github" || out.Kind != "pull_request" ||
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
		`SELECT conversation_id, external_id, url, details_json FROM artifacts WHERE dedup_key = 'git:branch:octo/repo:refs/heads/x'`,
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
		ConversationID: runID, OrgID: runmode.LocalDefaultOrgID, TeamID: runmode.LocalDefaultTeamID,
		Provider: "github", Kind: "pull_request", Target: "octo/repo",
		State: domain.ArtifactStatePRDraft, DedupKey: key,
	})
	if err != nil {
		t.Fatalf("first Upsert: %v", err)
	}

	second, err := stores.Artifacts.Upsert(ctx, runmode.LocalDefaultOrgID, domain.Artifact{
		ConversationID: runID, OrgID: runmode.LocalDefaultOrgID, TeamID: runmode.LocalDefaultTeamID,
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

// TestArtifactStore_SQLite_InsertIfAbsent pins the reconciler-backstop write:
// the first insert lands (inserted=true); a second on the same dedup_key is a
// no-op (inserted=false) that NEVER overwrites the existing row's state — so a
// backstop pass can't regress a PR the observation path already recorded as, say,
// merged, back to open.
func TestArtifactStore_SQLite_InsertIfAbsent(t *testing.T) {
	conn := newSQLiteForArtifactTest(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()
	runID := seedArtifactRun(t, conn)

	key := domain.ArtifactDedupKey("github", "pull_request", "octo/repo#7", "")
	base := domain.Artifact{
		ConversationID: runID, OrgID: runmode.LocalDefaultOrgID, TeamID: runmode.LocalDefaultTeamID,
		Provider: "github", Kind: "pull_request", Target: "octo/repo#7", ExternalID: "7",
		DedupKey: key,
	}

	first := base
	first.State = domain.ArtifactStatePROpen
	inserted, err := stores.Artifacts.InsertArtifactIfAbsentSystem(ctx, runmode.LocalDefaultOrgID, first)
	if err != nil {
		t.Fatalf("first InsertArtifactIfAbsentSystem: %v", err)
	}
	if !inserted {
		t.Fatal("first insert reported not-inserted; want true")
	}

	// Advance the row out-of-band the way the observation path would.
	if _, err := conn.Exec(`UPDATE artifacts SET state = ? WHERE dedup_key = ?`, domain.ArtifactStatePRMerged, key); err != nil {
		t.Fatalf("advance state: %v", err)
	}

	second := base
	second.State = domain.ArtifactStatePROpen // a stale backstop view
	inserted, err = stores.Artifacts.InsertArtifactIfAbsentSystem(ctx, runmode.LocalDefaultOrgID, second)
	if err != nil {
		t.Fatalf("second InsertArtifactIfAbsentSystem: %v", err)
	}
	if inserted {
		t.Error("second insert reported inserted; want false (row already present)")
	}

	var count int
	var state string
	if err := conn.QueryRow(`SELECT COUNT(*), MAX(state) FROM artifacts WHERE dedup_key = ?`, key).Scan(&count, &state); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Errorf("want exactly 1 row, got %d", count)
	}
	if state != domain.ArtifactStatePRMerged {
		t.Errorf("insert-if-absent clobbered state to %q; want merged preserved", state)
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
		ConversationID: runID, OrgID: runmode.LocalDefaultOrgID, TeamID: runmode.LocalDefaultTeamID,
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
		ConversationID: runID, OrgID: runmode.LocalDefaultOrgID, TeamID: runmode.LocalDefaultTeamID,
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
		ConversationID: runID, OrgID: runmode.LocalDefaultOrgID, TeamID: runmode.LocalDefaultTeamID,
		Provider: "jira", Kind: "issue", Target: "SKY-1",
		ExternalID: "SKY-1", URL: "https://jira.example.com/browse/SKY-1",
		State: domain.ArtifactStateIssueCreated, DedupKey: key,
	}); err != nil {
		t.Fatalf("first Upsert: %v", err)
	}

	// A later mutation that can't supply external_id/url (both empty).
	out, err := stores.Artifacts.Upsert(ctx, runmode.LocalDefaultOrgID, domain.Artifact{
		ConversationID: runID, OrgID: runmode.LocalDefaultOrgID, TeamID: runmode.LocalDefaultTeamID,
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
		ConversationID: runID, OrgID: runmode.LocalDefaultOrgID, TeamID: runmode.LocalDefaultTeamID,
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
		ConversationID: runID, OrgID: runmode.LocalDefaultOrgID, TeamID: runmode.LocalDefaultTeamID,
		Provider: "github", Kind: "comment", Target: "octo/repo#7",
		ExternalID: "555", URL: "https://github.com/octo/repo/pull/7#issuecomment-555",
		State: domain.ArtifactStateCommentPosted, DedupKey: key,
	}); err != nil {
		t.Fatalf("first Upsert: %v", err)
	}

	// A comment-delete that only carries the id (empty target/url).
	out, err := stores.Artifacts.Upsert(ctx, runmode.LocalDefaultOrgID, domain.Artifact{
		ConversationID: runID, OrgID: runmode.LocalDefaultOrgID, TeamID: runmode.LocalDefaultTeamID,
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
		ConversationID: runID, OrgID: runmode.LocalDefaultOrgID, TeamID: runmode.LocalDefaultTeamID,
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
			ConversationID: runID, OrgID: runmode.LocalDefaultOrgID, TeamID: runmode.LocalDefaultTeamID,
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
			ConversationID: runID, OrgID: runmode.LocalDefaultOrgID, TeamID: runmode.LocalDefaultTeamID,
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

// TestArtifactStore_SQLite_ListByRuns pins the batched multi-run read the
// run-list path uses for pending_kind (TFAC-465): artifacts for the given runs
// come back in one slice, each carrying its RunID for grouping; a run with no
// artifacts contributes nothing and an empty runIDs is a no-op.
func TestArtifactStore_SQLite_ListByRuns(t *testing.T) {
	conn := newSQLiteForArtifactTest(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()
	runA := seedArtifactRun(t, conn)
	runB := seedArtifactRunWithID(t, conn, "88888888-8888-8888-8888-888888888888")
	runC := seedArtifactRunWithID(t, conn, "77777777-7777-7777-7777-777777777777") // no artifacts

	seed := func(runID, key string) {
		if _, err := stores.Artifacts.Upsert(ctx, runmode.LocalDefaultOrgID, domain.Artifact{
			ConversationID: runID, OrgID: runmode.LocalDefaultOrgID, TeamID: runmode.LocalDefaultTeamID,
			Provider: "github", Kind: "comment", Target: "octo/repo",
			State: domain.ArtifactStateCommentPosted, DedupKey: key,
		}); err != nil {
			t.Fatalf("seed %s: %v", key, err)
		}
	}
	seed(runA, "a1")
	seed(runA, "a2")
	seed(runB, "b1")

	arts, err := stores.Artifacts.ListByRuns(ctx, runmode.LocalDefaultOrgID, []string{runA, runB, runC})
	if err != nil {
		t.Fatalf("ListByRuns: %v", err)
	}
	byRun := map[string]int{}
	for _, a := range arts {
		if a.ConversationID == "" {
			t.Errorf("artifact %s came back without its RunID (can't group)", a.ID)
		}
		byRun[a.ConversationID]++
	}
	if byRun[runA] != 2 || byRun[runB] != 1 {
		t.Errorf("grouped counts = %v, want runA=2 runB=1", byRun)
	}
	if byRun[runC] != 0 {
		t.Errorf("runC (no artifacts) contributed %d rows, want 0", byRun[runC])
	}

	empty, err := stores.Artifacts.ListByRuns(ctx, runmode.LocalDefaultOrgID, nil)
	if err != nil {
		t.Fatalf("ListByRuns(nil): %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("ListByRuns(nil) = %v, want empty", empty)
	}
}

// TestArtifactStore_SQLite_ListByTeam_IncludesDetached pins the
// audit-ledger invariant: an artifact whose run was purged (conversation_id NULL)
// is still the team's and must come back from ListByTeam. Guards against a
// future `AND conversation_id IS NOT NULL` creeping into the query. TFAC-455.
func TestArtifactStore_SQLite_ListByTeam_IncludesDetached(t *testing.T) {
	conn := newSQLiteForArtifactTest(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()
	runID := seedArtifactRun(t, conn)

	art, err := stores.Artifacts.Upsert(ctx, runmode.LocalDefaultOrgID, domain.Artifact{
		ConversationID: runID, OrgID: runmode.LocalDefaultOrgID, TeamID: runmode.LocalDefaultTeamID,
		Provider: "git", Kind: "branch", Target: "octo/repo",
		State: domain.ArtifactStateBranchPushed, DedupKey: "git:branch:octo/repo:refs/heads/x",
	})
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	// Simulate a run purge: the FK is ON DELETE SET NULL, so deleting the
	// run detaches the artifact rather than cascading it away.
	if _, err := conn.Exec(`DELETE FROM conversations WHERE id = ?`, runID); err != nil {
		t.Fatalf("purge run: %v", err)
	}
	var nullRun sql.NullString
	if err := conn.QueryRow(`SELECT conversation_id FROM artifacts WHERE id = ?`, art.ID).Scan(&nullRun); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if nullRun.Valid {
		t.Fatalf("conversation_id should be NULL after run purge, got %q", nullRun.String)
	}

	rows, err := stores.Artifacts.ListByTeam(ctx, runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, db.ArtifactListOpts{})
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
			ConversationID: runID, OrgID: runmode.LocalDefaultOrgID, TeamID: runmode.LocalDefaultTeamID,
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

// TestArtifactStore_SQLite_ListByOrgSystem pins the bot-activity audit feed's
// org source (TFAC-483): it returns ALL states — terminal included (merged PR,
// dismissed review) — in contrast to ListNonTerminalBySystem's reconciler
// working set, newest-first, and honors the provider/kind/state/time filters and
// the limit/offset paging from ArtifactListOpts. A _time_format=sqlite conn so
// the bound Since/Until time.Time compares against created_at correctly.
func TestArtifactStore_SQLite_ListByOrgSystem(t *testing.T) {
	conn := newSQLiteForArtifactTestTimed(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()
	runID := seedArtifactRun(t, conn)

	// (provider, kind, state, created_at) across the lifecycle, including
	// terminal rows the reconciler set would hide.
	seeds := []struct {
		provider, kind, state, when string
	}{
		{domain.ArtifactProviderGit, domain.ArtifactKindBranch, domain.ArtifactStateBranchPushed, "2026-06-01 00:00:00"},
		{domain.ArtifactProviderGitHub, domain.ArtifactKindPullRequest, domain.ArtifactStatePROpen, "2026-06-02 00:00:00"},
		{domain.ArtifactProviderGitHub, domain.ArtifactKindPullRequest, domain.ArtifactStatePRMerged, "2026-06-03 00:00:00"},   // terminal
		{domain.ArtifactProviderGitHub, domain.ArtifactKindReview, domain.ArtifactStateReviewDismissed, "2026-06-04 00:00:00"}, // terminal
		{domain.ArtifactProviderJira, domain.ArtifactKindComment, domain.ArtifactStateCommentPosted, "2026-06-05 00:00:00"},
	}
	for i, s := range seeds {
		out, err := stores.Artifacts.Upsert(ctx, runmode.LocalDefaultOrgID, domain.Artifact{
			ConversationID: runID, OrgID: runmode.LocalDefaultOrgID, TeamID: runmode.LocalDefaultTeamID,
			Provider: s.provider, Kind: s.kind, Target: "octo/repo", State: s.state, DedupKey: "k" + string(rune('a'+i)),
		})
		if err != nil {
			t.Fatalf("seed %s/%s: %v", s.kind, s.state, err)
		}
		// Pin created_at so the newest-first order + the time window are deterministic.
		if _, err := conn.Exec(`UPDATE artifacts SET created_at = ? WHERE id = ?`, s.when, out.ID); err != nil {
			t.Fatalf("set created_at: %v", err)
		}
	}

	// No opts: every row, terminal included, newest-first.
	all, err := stores.Artifacts.ListByOrgSystem(ctx, runmode.LocalDefaultOrgID, db.ArtifactListOpts{})
	if err != nil {
		t.Fatalf("ListByOrgSystem: %v", err)
	}
	if len(all) != 5 {
		t.Fatalf("ListByOrgSystem returned %d rows, want all 5 (terminal included)", len(all))
	}
	states := map[string]bool{}
	for _, a := range all {
		states[a.State] = true
	}
	if !states[domain.ArtifactStatePRMerged] || !states[domain.ArtifactStateReviewDismissed] {
		t.Errorf("feed dropped terminal rows: states=%v (want merged + dismissed present)", states)
	}
	// Contrast the reconciler set: it hides exactly those terminal rows.
	nonTerminal, err := stores.Artifacts.ListNonTerminalBySystem(ctx, runmode.LocalDefaultOrgID)
	if err != nil {
		t.Fatalf("ListNonTerminalBySystem: %v", err)
	}
	if len(nonTerminal) != 2 {
		t.Errorf("ListNonTerminalBySystem returned %d, want 2 (branch pushed + PR open) — the contrast", len(nonTerminal))
	}
	// Newest-first: comment (06-05) leads, branch (06-01) trails.
	if all[0].State != domain.ArtifactStateCommentPosted || all[4].State != domain.ArtifactStateBranchPushed {
		t.Errorf("order not newest-first: first=%q last=%q", all[0].State, all[4].State)
	}

	// Filters.
	gh, err := stores.Artifacts.ListByOrgSystem(ctx, runmode.LocalDefaultOrgID, db.ArtifactListOpts{Provider: domain.ArtifactProviderGitHub})
	if err != nil {
		t.Fatalf("filter provider: %v", err)
	}
	if len(gh) != 3 {
		t.Errorf("provider=github returned %d, want 3 (PR open + PR merged + review)", len(gh))
	}
	prs, err := stores.Artifacts.ListByOrgSystem(ctx, runmode.LocalDefaultOrgID, db.ArtifactListOpts{Kind: domain.ArtifactKindPullRequest})
	if err != nil {
		t.Fatalf("filter kind: %v", err)
	}
	if len(prs) != 2 {
		t.Errorf("kind=pull_request returned %d, want 2 (open + merged)", len(prs))
	}
	merged, err := stores.Artifacts.ListByOrgSystem(ctx, runmode.LocalDefaultOrgID, db.ArtifactListOpts{State: domain.ArtifactStatePRMerged})
	if err != nil {
		t.Fatalf("filter state: %v", err)
	}
	if len(merged) != 1 || merged[0].State != domain.ArtifactStatePRMerged {
		t.Errorf("state=merged returned %+v, want exactly the merged PR", merged)
	}

	// Time window [06-03, 06-05): merged (06-03) + review (06-04), excluding the
	// open PR before it and the comment at the exclusive upper bound.
	windowed, err := stores.Artifacts.ListByOrgSystem(ctx, runmode.LocalDefaultOrgID, db.ArtifactListOpts{
		Since: time.Date(2026, 6, 3, 0, 0, 0, 0, time.UTC),
		Until: time.Date(2026, 6, 5, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("filter window: %v", err)
	}
	if len(windowed) != 2 {
		t.Fatalf("time window returned %d, want 2 (merged + review)", len(windowed))
	}
	if windowed[0].State != domain.ArtifactStateReviewDismissed || windowed[1].State != domain.ArtifactStatePRMerged {
		t.Errorf("windowed order = [%q, %q], want [dismissed, merged] (newest-first)", windowed[0].State, windowed[1].State)
	}

	// Paging: page 1 (limit 2) → comment + review; page 2 (offset 2) → merged + open.
	page1, err := stores.Artifacts.ListByOrgSystem(ctx, runmode.LocalDefaultOrgID, db.ArtifactListOpts{Limit: 2})
	if err != nil {
		t.Fatalf("page 1: %v", err)
	}
	page2, err := stores.Artifacts.ListByOrgSystem(ctx, runmode.LocalDefaultOrgID, db.ArtifactListOpts{Limit: 2, Offset: 2})
	if err != nil {
		t.Fatalf("page 2: %v", err)
	}
	if len(page1) != 2 || len(page2) != 2 {
		t.Fatalf("paging sizes = %d, %d, want 2, 2", len(page1), len(page2))
	}
	if page1[0].State != domain.ArtifactStateCommentPosted || page2[0].State != domain.ArtifactStatePRMerged {
		t.Errorf("paging boundary wrong: page1[0]=%q page2[0]=%q", page1[0].State, page2[0].State)
	}
}

// TestArtifactStore_SQLite_ListByTeam_Filters pins that the shared filter/paging
// helper applies to the team-scoped path too (not just ListByOrgSystem): a
// provider filter and an offset both narrow ListByTeam's result. TFAC-483.
func TestArtifactStore_SQLite_ListByTeam_Filters(t *testing.T) {
	conn := newSQLiteForArtifactTest(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()
	runID := seedArtifactRun(t, conn)

	seed := func(provider, kind, key string) {
		if _, err := stores.Artifacts.Upsert(ctx, runmode.LocalDefaultOrgID, domain.Artifact{
			ConversationID: runID, OrgID: runmode.LocalDefaultOrgID, TeamID: runmode.LocalDefaultTeamID,
			Provider: provider, Kind: kind, Target: "octo/repo",
			State: domain.ArtifactStateCommentPosted, DedupKey: key,
		}); err != nil {
			t.Fatalf("seed %s: %v", key, err)
		}
	}
	seed(domain.ArtifactProviderGitHub, domain.ArtifactKindComment, "g1")
	seed(domain.ArtifactProviderGitHub, domain.ArtifactKindComment, "g2")
	seed(domain.ArtifactProviderJira, domain.ArtifactKindComment, "j1")

	gh, err := stores.Artifacts.ListByTeam(ctx, runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, db.ArtifactListOpts{Provider: domain.ArtifactProviderGitHub})
	if err != nil {
		t.Fatalf("ListByTeam provider filter: %v", err)
	}
	if len(gh) != 2 {
		t.Errorf("provider=github returned %d, want 2", len(gh))
	}

	// limit 2 then offset 2 walks the 3-row team feed without overlap.
	offset, err := stores.Artifacts.ListByTeam(ctx, runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, db.ArtifactListOpts{Limit: 2, Offset: 2})
	if err != nil {
		t.Fatalf("ListByTeam offset: %v", err)
	}
	if len(offset) != 1 {
		t.Errorf("limit 2 offset 2 over 3 rows returned %d, want 1", len(offset))
	}
}

// newSQLiteForArtifactTestTimed is newSQLiteForArtifactTest with
// _time_format=sqlite, so a bound time.Time (the Since/Until filters) serializes
// to a datetime()-parseable form matching CURRENT_TIMESTAMP — the same reason
// the spend store's SQLite test uses it. Used by the time-window filter test.
func newSQLiteForArtifactTestTimed(t *testing.T) *sql.DB {
	t.Helper()
	conn, err := sql.Open("sqlite", ":memory:?_pragma=foreign_keys(on)&_time_format=sqlite")
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

// TestArtifactStore_SQLite_ListPendingReviewsByTarget pins the new-commits
// notifier's lookup (TFAC-501): only PENDING review artifacts on the exact PR
// target come back; submitted/dismissed reviews, other-PR drafts, and non-review
// kinds are excluded.
func TestArtifactStore_SQLite_ListPendingReviewsByTarget(t *testing.T) {
	conn := newSQLiteForArtifactTest(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()
	runID := seedArtifactRun(t, conn)
	org := runmode.LocalDefaultOrgID

	mk := func(kind, state, target, dedup string) {
		if _, err := stores.Artifacts.Upsert(ctx, org, domain.Artifact{
			ConversationID: runID, OrgID: org, TeamID: runmode.LocalDefaultTeamID,
			Provider: domain.ArtifactProviderGitHub, Kind: kind, State: state,
			Target: target, DedupKey: dedup,
		}); err != nil {
			t.Fatalf("seed artifact %s/%s: %v", kind, state, err)
		}
	}
	// Two pending reviews on the target PR (different runs in reality → different
	// dedup keys), plus noise that must NOT match.
	mk(domain.ArtifactKindReview, domain.ArtifactStateReviewPending, "octo/repo#7", "rev-7-a")
	mk(domain.ArtifactKindReview, domain.ArtifactStateReviewPending, "octo/repo#7", "rev-7-b")
	mk(domain.ArtifactKindReview, domain.ArtifactStateReviewSubmitted, "octo/repo#7", "rev-7-submitted")
	mk(domain.ArtifactKindReview, domain.ArtifactStateReviewDismissed, "octo/repo#7", "rev-7-dismissed")
	mk(domain.ArtifactKindReview, domain.ArtifactStateReviewPending, "octo/repo#8", "rev-8")
	mk(domain.ArtifactKindPullRequest, domain.ArtifactStatePROpen, "octo/repo#7", "pr-7")

	got, err := stores.Artifacts.ListPendingReviewsByTargetSystem(ctx, org, "octo/repo#7")
	if err != nil {
		t.Fatalf("ListPendingReviewsByTargetSystem: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 pending reviews on octo/repo#7, got %d: %+v", len(got), got)
	}
	for _, a := range got {
		if a.Kind != domain.ArtifactKindReview || a.State != domain.ArtifactStateReviewPending || a.Target != "octo/repo#7" {
			t.Errorf("unexpected artifact in result: kind=%s state=%s target=%s", a.Kind, a.State, a.Target)
		}
	}
}

// TestArtifactStore_SQLite_TransitionReviewState pins the review CAS: exactly
// one of two same-from transitions wins; the loser (and a wrong-from or
// wrong-kind row) reports false without touching anything; the stamp trio
// (external_id/url/details_json) applies preserve-on-empty in the same write.
func TestArtifactStore_SQLite_TransitionReviewState(t *testing.T) {
	conn := newSQLiteForArtifactTest(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()
	runID := seedArtifactRun(t, conn)
	org := runmode.LocalDefaultOrgID

	rev, err := stores.Artifacts.Upsert(ctx, org, domain.Artifact{
		ConversationID: runID, OrgID: org, TeamID: runmode.LocalDefaultTeamID,
		Provider: domain.ArtifactProviderGitHub, Kind: domain.ArtifactKindReview,
		State: domain.ArtifactStateReviewPending, Target: "octo/repo#7",
		DedupKey: "rev-cas", DetailsJSON: `{"number":7}`,
	})
	if err != nil {
		t.Fatalf("seed review: %v", err)
	}
	pr, err := stores.Artifacts.Upsert(ctx, org, domain.Artifact{
		ConversationID: runID, OrgID: org, TeamID: runmode.LocalDefaultTeamID,
		Provider: domain.ArtifactProviderGitHub, Kind: domain.ArtifactKindPullRequest,
		State: domain.ArtifactStateReviewPending, Target: "octo/repo#7", DedupKey: "pr-cas",
	})
	if err != nil {
		t.Fatalf("seed pr: %v", err)
	}

	// The claim: pending → submitted, no stamp yet.
	won, err := stores.Artifacts.TransitionReviewState(ctx, org, rev.ID,
		domain.ArtifactStateReviewPending, domain.ArtifactStateReviewSubmitted, "", "", "")
	if err != nil || !won {
		t.Fatalf("claim = (%v, %v), want (true, nil)", won, err)
	}
	// The losing racer: same from-state, row already moved.
	won, err = stores.Artifacts.TransitionReviewState(ctx, org, rev.ID,
		domain.ArtifactStateReviewPending, domain.ArtifactStateReviewSubmitted, "", "", "")
	if err != nil || won {
		t.Fatalf("second claim = (%v, %v), want (false, nil) — the CAS must admit one winner", won, err)
	}
	// The claim alone must not have blanked details (preserve-on-empty).
	got, _ := stores.Artifacts.Get(ctx, org, rev.ID)
	if got.State != domain.ArtifactStateReviewSubmitted || got.DetailsJSON != `{"number":7}` || got.URL != "" {
		t.Fatalf("post-claim row = state %q details %q url %q, want submitted/original details/empty url", got.State, got.DetailsJSON, got.URL)
	}

	// The stamp: submitted → submitted carrying the posted review's coordinates.
	won, err = stores.Artifacts.TransitionReviewState(ctx, org, rev.ID,
		domain.ArtifactStateReviewSubmitted, domain.ArtifactStateReviewSubmitted,
		"12345", "https://github.com/octo/repo/pull/7#pullrequestreview-12345", `{"number":7,"review_event":"COMMENT"}`)
	if err != nil || !won {
		t.Fatalf("stamp = (%v, %v), want (true, nil)", won, err)
	}
	got, _ = stores.Artifacts.Get(ctx, org, rev.ID)
	if got.ExternalID != "12345" || got.URL != "https://github.com/octo/repo/pull/7#pullrequestreview-12345" ||
		got.DetailsJSON != `{"number":7,"review_event":"COMMENT"}` {
		t.Fatalf("post-stamp row = %+v, want the posted review's coordinates", got)
	}

	// Kind guard: a non-review row never transitions, even from a matching state.
	won, err = stores.Artifacts.TransitionReviewState(ctx, org, pr.ID,
		domain.ArtifactStateReviewPending, domain.ArtifactStateReviewSubmitted, "", "", "")
	if err != nil || won {
		t.Fatalf("pr transition = (%v, %v), want (false, nil) — CAS is review-only", won, err)
	}
	// Missing row.
	won, err = stores.Artifacts.TransitionReviewState(ctx, org, "00000000-0000-0000-0000-000000000000",
		domain.ArtifactStateReviewPending, domain.ArtifactStateReviewSubmitted, "", "", "")
	if err != nil || won {
		t.Fatalf("missing-row transition = (%v, %v), want (false, nil)", won, err)
	}
}

// TestArtifactStore_SQLite_UpdateReviewDetailsIfPending pins the guarded draft
// write: it lands only while the review is still pending, and reports false —
// leaving the row untouched — once the review resolved, so a stale writer can't
// resurrect a submitted review's draft state.
func TestArtifactStore_SQLite_UpdateReviewDetailsIfPending(t *testing.T) {
	conn := newSQLiteForArtifactTest(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()
	runID := seedArtifactRun(t, conn)
	org := runmode.LocalDefaultOrgID

	rev, err := stores.Artifacts.Upsert(ctx, org, domain.Artifact{
		ConversationID: runID, OrgID: org, TeamID: runmode.LocalDefaultTeamID,
		Provider: domain.ArtifactProviderGitHub, Kind: domain.ArtifactKindReview,
		State: domain.ArtifactStateReviewPending, Target: "octo/repo#7",
		DedupKey: "rev-details", DetailsJSON: `{"v":1}`,
	})
	if err != nil {
		t.Fatalf("seed review: %v", err)
	}

	updated, err := stores.Artifacts.UpdateReviewDetailsIfPending(ctx, org, rev.ID, `{"v":2}`)
	if err != nil || !updated {
		t.Fatalf("pending update = (%v, %v), want (true, nil)", updated, err)
	}
	got, _ := stores.Artifacts.Get(ctx, org, rev.ID)
	if got.DetailsJSON != `{"v":2}` {
		t.Fatalf("details = %q, want {\"v\":2}", got.DetailsJSON)
	}

	// Resolve the review, then attempt the stale write.
	if won, err := stores.Artifacts.TransitionReviewState(ctx, org, rev.ID,
		domain.ArtifactStateReviewPending, domain.ArtifactStateReviewSubmitted, "", "", ""); err != nil || !won {
		t.Fatalf("resolve: (%v, %v)", won, err)
	}
	updated, err = stores.Artifacts.UpdateReviewDetailsIfPending(ctx, org, rev.ID, `{"v":3}`)
	if err != nil || updated {
		t.Fatalf("post-resolve update = (%v, %v), want (false, nil)", updated, err)
	}
	got, _ = stores.Artifacts.Get(ctx, org, rev.ID)
	if got.DetailsJSON != `{"v":2}` || got.State != domain.ArtifactStateReviewSubmitted {
		t.Fatalf("row after refused write = state %q details %q, want submitted/{\"v\":2} untouched", got.State, got.DetailsJSON)
	}

	// System variant is the same write in SQLite.
	if _, err := stores.Artifacts.UpdateReviewDetailsIfPendingSystem(ctx, org, rev.ID, `{"v":4}`); err != nil {
		t.Fatalf("system variant: %v", err)
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

// seedArtifactRun inserts a minimal run the artifacts FK (conversation_id →
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
		`INSERT INTO conversations (id, origin, status) VALUES (?, 'interactive', 'running')`, id,
	); err != nil {
		t.Fatalf("seed run %s: %v", id, err)
	}
	return id
}
