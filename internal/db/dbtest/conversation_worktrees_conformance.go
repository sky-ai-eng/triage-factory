package dbtest

import (
	"context"
	"errors"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// ConversationWorktreeStoreFactory is what a per-backend test file hands to
// RunConversationWorktreeStoreConformance. Returns:
//   - the wired ConversationWorktreeStore impl,
//   - the orgID to pass to every call,
//   - a ConversationWorktreeSeeder the harness uses to stage the conversation FK
//     chain (conversation_worktrees FKs to conversations; backends seed those rows
//     differently and the conformance harness shouldn't bake one
//     shape's schema into the assertions).
type ConversationWorktreeStoreFactory func(t *testing.T) (store db.ConversationWorktreeStore, orgID string, seed ConversationWorktreeSeeder)

// ConversationWorktreeSeeder is a bag of callbacks the conformance suite uses
// to stage fixture rows the ConversationWorktreeStore doesn't own.
type ConversationWorktreeSeeder struct {
	// Conversation inserts the entity + event + prompt + task + conversation FK chain
	// needed to attach a conversation_worktrees row, and returns the conversationID.
	// suffix discriminates per-subtest seeds so the unique indexes on
	// entities/conversations don't collide.
	Conversation func(t *testing.T, suffix string) (conversationID string)

	// DeleteConversation removes the conversation row so the cascade-on-delete
	// subtest can verify the FK ON DELETE CASCADE.
	DeleteConversation func(t *testing.T, conversationID string)

	// SiblingConversation inserts a second conversation on the same task as
	// conversationID and returns its id. Nil skips the task-wide cases.
	SiblingConversation func(t *testing.T, conversationID string) (siblingID string)

	// UnrelatedConversation inserts a conversation on a different task. Nil
	// skips the task-wide cases.
	UnrelatedConversation func(t *testing.T, suffix string) (conversationID string)

	// TaskOf returns the task conversationID belongs to.
	TaskOf func(t *testing.T, conversationID string) (taskID string)

	// Claim mints a live claim on conversationID and returns its id; Release
	// releases it. Nil skips the fenced-write cases.
	Claim   func(t *testing.T, conversationID string) (claimID string)
	Release func(t *testing.T, claimID string)

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
// (a repository is tracked, then a conversation checks it out) expressed as a
// fixture.
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
		conversationID := seed.Conversation(t, "fresh")
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
		conversationID := seed.Conversation(t, "idem")
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
		// The (conversation, repo, ref) PK lets one conversation hold two
		// worktrees in one repo (two PRs reviewed in one interactive
		// conversation — TFAC-502). Both inserts must succeed and List
		// must return both.
		store, orgID, seed := mk(t)
		conversationID := seed.Conversation(t, "tworef")
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
		conversationID := seed.Conversation(t, "getrepo")
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

	t.Run("List_orders_by_created_at_then_repo_and_scopes_by_conversation", func(t *testing.T) {
		store, orgID, seed := mk(t)
		r1 := seed.Conversation(t, "list-r1")
		r2 := seed.Conversation(t, "list-r2")
		for _, w := range []domain.ConversationWorktree{
			{ConversationID: r1, RepoID: "owner/a", Path: "/p1", Ref: "default"},
			{ConversationID: r1, RepoID: "owner/b", Path: "/p2", Ref: "default"},
			{ConversationID: r2, RepoID: "owner/a", Path: "/p3", Ref: "default"},
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
		conversationID := seed.Conversation(t, "del-repo")
		if err := store.DeleteByRepoRef(ctx, orgID, conversationID, "no/such-repo", "pr-1"); err != nil {
			t.Errorf("DeleteByRepoRef(missing) = %v, want nil", err)
		}
	})

	t.Run("DeleteByRepoRef_targets_only_the_matching_ref", func(t *testing.T) {
		store, orgID, seed := mk(t)
		conversationID := seed.Conversation(t, "del-ref")
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
		conversationID := seed.Conversation(t, "del-path")
		if _, _, err := insertWorktree(t, store, seed, orgID, domain.ConversationWorktree{
			ConversationID: conversationID, RepoID: "owner/a", Path: "/p1", Ref: "default",
		}); err != nil {
			t.Fatalf("insert a: %v", err)
		}
		if _, _, err := insertWorktree(t, store, seed, orgID, domain.ConversationWorktree{
			ConversationID: conversationID, RepoID: "owner/b", Path: "/p2", Ref: "default",
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

	t.Run("ListForTaskSystem_spans_the_tasks_conversations_only", func(t *testing.T) {
		store, orgID, seed := mk(t)
		if seed.SiblingConversation == nil || seed.UnrelatedConversation == nil || seed.TaskOf == nil {
			t.Skip("backend seeds no sibling conversations")
		}
		first := seed.Conversation(t, "task-a")
		second := seed.SiblingConversation(t, first)
		other := seed.UnrelatedConversation(t, "task-b")
		for _, w := range []domain.ConversationWorktree{
			{ConversationID: first, RepoID: "owner/a", Path: "/root/owner/a/pr-1", Ref: "pr-1"},
			{ConversationID: second, RepoID: "owner/b", Path: "/root/owner/b/default", Ref: "default"},
			{ConversationID: other, RepoID: "owner/c", Path: "/other/owner/c/default", Ref: "default"},
		} {
			if _, _, err := insertWorktree(t, store, seed, orgID, w); err != nil {
				t.Fatalf("insert %s: %v", w.Path, err)
			}
		}
		rows, err := store.ListForTaskSystem(ctx, orgID, seed.TaskOf(t, first))
		if err != nil {
			t.Fatalf("ListForTaskSystem: %v", err)
		}
		got := map[string]string{}
		for _, r := range rows {
			got[r.Path] = r.ConversationID
		}
		if len(got) != 2 || got["/root/owner/a/pr-1"] != first || got["/root/owner/b/default"] != second {
			t.Errorf("ListForTaskSystem = %+v, want the two rows of the task's own conversations", rows)
		}
	})

	t.Run("RecordForClaimSystem_inserts_then_moves_the_path", func(t *testing.T) {
		store, orgID, seed := mk(t)
		if seed.Claim == nil {
			t.Skip("backend seeds no claims")
		}
		conversationID := seed.Conversation(t, "record")
		claimID := seed.Claim(t, conversationID)
		seed.Repo(t, "owner/repo")
		row := domain.ConversationWorktree{ConversationID: conversationID, RepoID: "owner/repo", Path: "/old/owner/repo/ref-main", Ref: "ref-main"}
		stored, err := store.RecordForClaimSystem(ctx, orgID, claimID, row)
		if err != nil {
			t.Fatalf("RecordForClaimSystem (insert): %v", err)
		}
		AssertWriteReturnedStoredRow(t, "RecordForClaimSystem (insert)", stored, func() (*domain.ConversationWorktree, error) {
			return store.GetByRepoRef(ctx, orgID, conversationID, "owner/repo", "ref-main")
		})

		row.Path = "/new/owner/repo/ref-main"
		stored, err = store.RecordForClaimSystem(ctx, orgID, claimID, row)
		if err != nil {
			t.Fatalf("RecordForClaimSystem (move): %v", err)
		}
		if stored.Path != row.Path {
			t.Errorf("recorded path = %q, want %q", stored.Path, row.Path)
		}
		AssertWriteReturnedStoredRow(t, "RecordForClaimSystem (move)", stored, func() (*domain.ConversationWorktree, error) {
			return store.GetByRepoRef(ctx, orgID, conversationID, "owner/repo", "ref-main")
		})
		rows, err := store.List(ctx, orgID, conversationID)
		if err != nil || len(rows) != 1 {
			t.Errorf("List after a move = %+v (err=%v), want the one row", rows, err)
		}
	})

	t.Run("RecordForClaimSystem_refused_once_the_claim_is_released", func(t *testing.T) {
		store, orgID, seed := mk(t)
		if seed.Claim == nil || seed.Release == nil {
			t.Skip("backend seeds no claims")
		}
		conversationID := seed.Conversation(t, "record-fenced")
		claimID := seed.Claim(t, conversationID)
		seed.Release(t, claimID)
		seed.Repo(t, "owner/repo")
		_, err := store.RecordForClaimSystem(ctx, orgID, claimID, domain.ConversationWorktree{
			ConversationID: conversationID, RepoID: "owner/repo", Path: "/p", Ref: "default",
		})
		if !errors.Is(err, db.ErrClaimReleased) {
			t.Fatalf("RecordForClaimSystem on a released claim = %v, want ErrClaimReleased", err)
		}
		if rows, _ := store.List(ctx, orgID, conversationID); len(rows) != 0 {
			t.Errorf("a refused record wrote %+v", rows)
		}
	})

	t.Run("Cascade_on_conversation_delete_removes_rows", func(t *testing.T) {
		store, orgID, seed := mk(t)
		conversationID := seed.Conversation(t, "cascade")
		if _, _, err := insertWorktree(t, store, seed, orgID, domain.ConversationWorktree{
			ConversationID: conversationID, RepoID: "owner/a", Path: "/p1", Ref: "default",
		}); err != nil {
			t.Fatalf("insert: %v", err)
		}
		seed.DeleteConversation(t, conversationID)
		rows, err := store.List(ctx, orgID, conversationID)
		if err != nil {
			t.Fatalf("list after cascade: %v", err)
		}
		if len(rows) != 0 {
			t.Errorf("expected 0 rows after conversation delete cascade, got %d", len(rows))
		}
	})
}
