package dbtest

import (
	"context"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// ConversationWorktreeStoreFactory is what a per-backend test file hands to
// RunConversationWorktreeStoreConformance. Returns:
//   - the wired ConversationWorktreeStore impl,
//   - the orgID to pass to every call,
//   - a ConversationWorktreeSeeder the harness uses to stage the run FK
//     chain (conversation_worktrees FKs to conversations; backends seed those rows
//     differently and the conformance harness shouldn't bake one
//     shape's schema into the assertions).
type ConversationWorktreeStoreFactory func(t *testing.T) (store db.ConversationWorktreeStore, orgID string, seed ConversationWorktreeSeeder)

// ConversationWorktreeSeeder is a bag of callbacks the conformance suite uses
// to stage fixture rows the ConversationWorktreeStore doesn't own.
type ConversationWorktreeSeeder struct {
	// Run inserts the entity + event + prompt + task + run FK chain
	// needed to attach a conversation_worktrees row, and returns the conversationID.
	// suffix discriminates per-subtest seeds so the unique indexes on
	// entities/runs don't collide.
	Run func(t *testing.T, suffix string) (conversationID string)

	// DeleteConversation removes the run row so the cascade-on-delete subtest
	// can verify the FK ON DELETE CASCADE.
	DeleteConversation func(t *testing.T, conversationID string)

	// Repo ensures a registry row exists for an "owner/repo" slug.
	// conversation_worktrees references the repository by that row's id, and
	// the store resolves the slug rather than creating one — on the executor
	// it holds no INSERT on repositories at all. So a fixture that reserves a
	// worktree has to bring the repository into existence first, exactly as
	// tracking does in production. Idempotent.
	Repo func(t *testing.T, slug string)
}

// insertWorktree reserves a worktree through the store, ensuring the
// repository it names has a registry row first — the production ordering
// (a repository is tracked, then a run checks it out) expressed as a fixture.
func insertWorktree(t *testing.T, store db.ConversationWorktreeStore, seed ConversationWorktreeSeeder, orgID string, w domain.ConversationWorktree) (bool, string, error) {
	t.Helper()
	seed.Repo(t, w.RepoID)
	return store.Insert(context.Background(), orgID, w)
}

// RunConversationWorktreeStoreConformance covers the ConversationWorktreeStore
// contract every backend impl must hold. System variants are NOT
// covered by parallel cases — their behavior is documented as
// identical to the non-System counterparts (the per-method
// passthrough tests were intentionally pruned across the wave so
// the conformance suite tracks contract, not pool plumbing).
func RunConversationWorktreeStoreConformance(t *testing.T, mk ConversationWorktreeStoreFactory) {
	t.Helper()
	ctx := context.Background()

	t.Run("Insert_returns_inserted_true_on_fresh_row", func(t *testing.T) {
		store, orgID, seed := mk(t)
		conversationID := seed.Run(t, "fresh")
		inserted, winning, err := insertWorktree(t, store, seed, orgID, domain.ConversationWorktree{
			ConversationID: conversationID, RepoID: "owner/repo", Path: "/tmp/wt/" + conversationID + "/owner/repo/pr-1", Ref: "pr-1",
		})
		if err != nil {
			t.Fatalf("insert: %v", err)
		}
		if !inserted {
			t.Errorf("expected inserted=true on fresh row")
		}
		if winning != "/tmp/wt/"+conversationID+"/owner/repo/pr-1" {
			t.Errorf("winningPath = %q, want fresh path", winning)
		}
	})

	t.Run("Insert_idempotent_on_conflict_returns_winning_path", func(t *testing.T) {
		store, orgID, seed := mk(t)
		conversationID := seed.Run(t, "idem")
		firstPath := "/tmp/wt/" + conversationID + "/owner/repo/pr-1"
		if _, _, err := insertWorktree(t, store, seed, orgID, domain.ConversationWorktree{
			ConversationID: conversationID, RepoID: "owner/repo", Path: firstPath, Ref: "pr-1",
		}); err != nil {
			t.Fatalf("first insert: %v", err)
		}
		// Pass a different path to confirm the conflict path reads
		// the row, not echoes the input.
		inserted, winning, err := insertWorktree(t, store, seed, orgID, domain.ConversationWorktree{
			ConversationID: conversationID, RepoID: "owner/repo", Path: "/tmp/wt/DIFFERENT/owner/repo/pr-1", Ref: "pr-1",
		})
		if err != nil {
			t.Fatalf("second insert: %v", err)
		}
		if inserted {
			t.Errorf("expected inserted=false on conflicting second insert")
		}
		if winning != firstPath {
			t.Errorf("winningPath after conflict = %q, want %q", winning, firstPath)
		}
	})

	t.Run("Insert_distinct_refs_same_repo_coexist", func(t *testing.T) {
		// The (run, repo, ref) PK lets one run hold two worktrees in one
		// repo (two PRs reviewed in one interactive run — TFAC-502). Both
		// inserts must succeed and List must return both.
		store, orgID, seed := mk(t)
		conversationID := seed.Run(t, "tworef")
		for _, w := range []domain.ConversationWorktree{
			{ConversationID: conversationID, RepoID: "owner/repo", Path: "/p/pr-1", Ref: "pr-1"},
			{ConversationID: conversationID, RepoID: "owner/repo", Path: "/p/pr-2", Ref: "pr-2"},
		} {
			inserted, _, err := insertWorktree(t, store, seed, orgID, w)
			if err != nil {
				t.Fatalf("insert ref %s: %v", w.Ref, err)
			}
			if !inserted {
				t.Errorf("ref %s: expected inserted=true (distinct ref must not conflict)", w.Ref)
			}
		}
		rows, err := store.List(ctx, orgID, conversationID)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(rows) != 2 {
			t.Fatalf("rows = %d, want 2 (one per ref)", len(rows))
		}
	})

	t.Run("GetByRepoRef_returns_row_or_nil", func(t *testing.T) {
		store, orgID, seed := mk(t)
		conversationID := seed.Run(t, "getrepo")
		if _, _, err := insertWorktree(t, store, seed, orgID, domain.ConversationWorktree{
			ConversationID: conversationID, RepoID: "owner/repo", Path: "/p1", Ref: "pr-1",
		}); err != nil {
			t.Fatalf("insert: %v", err)
		}
		got, err := store.GetByRepoRef(ctx, orgID, conversationID, "owner/repo", "pr-1")
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if got == nil {
			t.Fatal("expected row, got nil")
		}
		if got.Path != "/p1" || got.Ref != "pr-1" {
			t.Errorf("unexpected row: %+v", got)
		}
		// A different ref on the same repo is a distinct key → nil.
		missingRef, err := store.GetByRepoRef(ctx, orgID, conversationID, "owner/repo", "pr-2")
		if err != nil {
			t.Fatalf("get missing ref: %v", err)
		}
		if missingRef != nil {
			t.Errorf("expected nil for missing ref, got %+v", missingRef)
		}
		missing, err := store.GetByRepoRef(ctx, orgID, conversationID, "other/repo", "pr-1")
		if err != nil {
			t.Fatalf("get missing: %v", err)
		}
		if missing != nil {
			t.Errorf("expected nil for missing repo, got %+v", missing)
		}
	})

	t.Run("List_orders_by_created_at_then_repo_and_scopes_by_run", func(t *testing.T) {
		store, orgID, seed := mk(t)
		r1 := seed.Run(t, "list-r1")
		r2 := seed.Run(t, "list-r2")
		for _, w := range []domain.ConversationWorktree{
			{ConversationID: r1, RepoID: "owner/a", Path: "/p1", Ref: "@default"},
			{ConversationID: r1, RepoID: "owner/b", Path: "/p2", Ref: "@default"},
			{ConversationID: r2, RepoID: "owner/a", Path: "/p3", Ref: "@default"},
		} {
			if _, _, err := insertWorktree(t, store, seed, orgID, w); err != nil {
				t.Fatalf("insert %s/%s: %v", w.ConversationID, w.RepoID, err)
			}
		}
		rows, err := store.List(ctx, orgID, r1)
		if err != nil {
			t.Fatalf("list r1: %v", err)
		}
		if len(rows) != 2 {
			t.Fatalf("r1 rows = %d, want 2", len(rows))
		}
		for _, r := range rows {
			if r.ConversationID != r1 {
				t.Errorf("scope leak: r1 list contains %s", r.ConversationID)
			}
		}
	})

	t.Run("DeleteByRepoRef_idempotent_on_missing_row", func(t *testing.T) {
		store, orgID, seed := mk(t)
		conversationID := seed.Run(t, "del-repo")
		if err := store.DeleteByRepoRef(ctx, orgID, conversationID, "no/such-repo", "pr-1"); err != nil {
			t.Errorf("DeleteByRepoRef(missing) = %v, want nil", err)
		}
	})

	t.Run("DeleteByRepoRef_targets_only_the_matching_ref", func(t *testing.T) {
		store, orgID, seed := mk(t)
		conversationID := seed.Run(t, "del-ref")
		for _, w := range []domain.ConversationWorktree{
			{ConversationID: conversationID, RepoID: "owner/repo", Path: "/p1", Ref: "pr-1"},
			{ConversationID: conversationID, RepoID: "owner/repo", Path: "/p2", Ref: "pr-2"},
		} {
			if _, _, err := insertWorktree(t, store, seed, orgID, w); err != nil {
				t.Fatalf("insert ref %s: %v", w.Ref, err)
			}
		}
		if err := store.DeleteByRepoRef(ctx, orgID, conversationID, "owner/repo", "pr-1"); err != nil {
			t.Fatalf("DeleteByRepoRef: %v", err)
		}
		rows, err := store.List(ctx, orgID, conversationID)
		if err != nil {
			t.Fatalf("list after delete: %v", err)
		}
		if len(rows) != 1 || rows[0].Ref != "pr-2" {
			t.Errorf("after delete: %+v, want exactly [pr-2]", rows)
		}
	})

	t.Run("DeleteByPathSystem_removes_only_the_matching_row", func(t *testing.T) {
		store, orgID, seed := mk(t)
		conversationID := seed.Run(t, "del-path")
		if _, _, err := insertWorktree(t, store, seed, orgID, domain.ConversationWorktree{
			ConversationID: conversationID, RepoID: "owner/a", Path: "/p1", Ref: "@default",
		}); err != nil {
			t.Fatalf("insert a: %v", err)
		}
		if _, _, err := insertWorktree(t, store, seed, orgID, domain.ConversationWorktree{
			ConversationID: conversationID, RepoID: "owner/b", Path: "/p2", Ref: "@default",
		}); err != nil {
			t.Fatalf("insert b: %v", err)
		}
		if err := store.DeleteByPathSystem(ctx, orgID, conversationID, "/p1"); err != nil {
			t.Fatalf("DeleteByPathSystem: %v", err)
		}
		rows, err := store.List(ctx, orgID, conversationID)
		if err != nil {
			t.Fatalf("list after delete: %v", err)
		}
		if len(rows) != 1 || rows[0].Path != "/p2" {
			t.Errorf("after delete: %+v, want exactly [/p2]", rows)
		}
	})

	t.Run("Cascade_on_run_delete_removes_rows", func(t *testing.T) {
		store, orgID, seed := mk(t)
		conversationID := seed.Run(t, "cascade")
		if _, _, err := insertWorktree(t, store, seed, orgID, domain.ConversationWorktree{
			ConversationID: conversationID, RepoID: "owner/a", Path: "/p1", Ref: "@default",
		}); err != nil {
			t.Fatalf("insert: %v", err)
		}
		seed.DeleteConversation(t, conversationID)
		rows, err := store.List(ctx, orgID, conversationID)
		if err != nil {
			t.Fatalf("list after cascade: %v", err)
		}
		if len(rows) != 0 {
			t.Errorf("expected 0 rows after run delete cascade, got %d", len(rows))
		}
	})
}
