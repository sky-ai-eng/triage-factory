package delegate

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/paths"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/worktree"
)

// evictWriterClaim is the engagement the fixtures record as having written the
// snapshot. A real uuid: the Postgres column is one, and a fixture that could
// only ever run on SQLite would be a fixture for half the product.
const evictWriterClaim = "dddddddd-4444-4444-8444-dddddddddddd"

// TestParseWorkspaceEvictAfter pins the knob's two defaults and its refusals.
// The local default is the interesting one: local mode's parked trees are on
// the user's own machine, where the warm resume is the experience being sold,
// so eviction there is opt-in rather than a window they have to widen.
func TestParseWorkspaceEvictAfter(t *testing.T) {
	for _, tc := range []struct {
		name      string
		raw       string
		local     bool
		want      time.Duration
		wantError bool
	}{
		{name: "unset_multi_defaults_to_six_hours", want: DefaultWorkspaceEvictAfterMulti},
		{name: "unset_local_is_disabled", local: true, want: 0},
		{name: "explicit_zero_disables", raw: "0", want: 0},
		{name: "explicit_zero_disables_in_multi", raw: "0", want: 0},
		{name: "local_opts_in_with_the_same_semantics", raw: "1800", local: true, want: 30 * time.Minute},
		{name: "seconds", raw: " 900 ", want: 15 * time.Minute},
		{name: "duration_string_is_not_seconds", raw: "6h", want: DefaultWorkspaceEvictAfterMulti, wantError: true},
		{name: "negative_refused", raw: "-1", want: DefaultWorkspaceEvictAfterMulti, wantError: true},
		{name: "garbage_in_local_keeps_local_default", raw: "soon", local: true, want: 0, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseWorkspaceEvictAfter(tc.raw, tc.local)
			if (err != nil) != tc.wantError {
				t.Fatalf("ParseWorkspaceEvictAfter(%q, local=%v) error = %v, wantError %v", tc.raw, tc.local, err, tc.wantError)
			}
			if got != tc.want {
				t.Errorf("ParseWorkspaceEvictAfter(%q, local=%v) = %v, want %v", tc.raw, tc.local, got, tc.want)
			}
		})
	}
}

// TestEvictIdleWorkspaces_EvictedTreeResumesByRehydrate is the acceptance round
// trip: an aged parked tree whose snapshot is written and present is reclaimed,
// and the resume that follows rebuilds it from the blob — which is the whole
// licence for deleting it. Provenance `rehydrated` is the assertion that the
// rebuild really happened rather than a warm tree quietly surviving.
func TestEvictIdleWorkspaces_EvictedTreeResumesByRehydrate(t *testing.T) {
	isolateRunNamespace(t)
	setupGitTestEnv(t)
	s, database, conversationID, _ := setupAdvanceFixture(t, "evict-roundtrip")
	wireBlobStore(t, s)
	ctx := context.Background()

	bpr := blueprintRunIDForConversation(t, database, conversationID)
	wtPath, owner, repo := setupTestWorktree(t, bpr)
	writeFile(t, filepath.Join(wtPath, "_tfac", "notes", "scratch.md"), "agent scratch")
	if err := s.snapshotWorkspace(ctx, runmode.LocalDefaultOrgID, conversationID, bpr, "", wtPath, "", domain.ConversationRuntimeNative); err != nil {
		t.Fatalf("snapshotWorkspace: %v", err)
	}
	seedSnapshotState(t, s, bpr, domain.WorkspaceSnapshotWritten)
	parkAged(t, database, conversationID, wtPath, "-2 hours")

	s.EvictIdleWorkspaces(ctx, time.Hour)

	if _, err := os.Stat(wtPath); !os.IsNotExist(err) {
		t.Fatalf("worktree %s survived the sweep (stat err %v); an idle parked tree past the TTL is what the sweep exists to reclaim", wtPath, err)
	}
	// worktree_path is deliberately left pointing at the deleted directory:
	// that is what makes ensureWorkspace's warm stat miss and fall to the blob.
	if got := worktreePathOf(t, database, conversationID); got != wtPath {
		t.Errorf("worktree_path = %q, want it left at %q — the stale path is the contract the cold path reads", got, wtPath)
	}

	conv := &domain.Conversation{ID: conversationID, WorktreePath: wtPath, BlueprintRunID: bpr}
	got, prov, err := s.ensureWorkspace(ctx, runmode.LocalDefaultOrgID, conv, gitSeed{owner: owner, repo: repo})
	if err != nil {
		t.Fatalf("ensureWorkspace after eviction: %v", err)
	}
	if prov != domain.WorkspaceProvenanceRehydrated {
		t.Errorf("provenance = %q, want rehydrated", prov)
	}
	if data, err := os.ReadFile(filepath.Join(got, "_tfac", "notes", "scratch.md")); err != nil || string(data) != "agent scratch" {
		t.Errorf("rebuilt scratch = %q (err %v), want the snapshot's content back", data, err)
	}
}

// TestEvictIdleWorkspaces_RemovesThroughThePrivilegedSeam: in multi mode the
// tree is owned by the sandbox identity that ran in it, so only the cap-broker
// can unlink it. An eviction that reached for os.RemoveAll would pass on a
// laptop and fail on every executor, so the seam itself is the assertion.
func TestEvictIdleWorkspaces_RemovesThroughThePrivilegedSeam(t *testing.T) {
	isolateRunNamespace(t)
	s, database, conversationID, _ := setupAdvanceFixture(t, "evict-seam")
	wireBlobStore(t, s)
	bpr, wtPath := seedEvictableTree(t, s, database, conversationID)

	type removal struct{ path, rootKey string }
	var removals []removal
	restoreRemoveSeam(t, func(path, rootKey string) error {
		removals = append(removals, removal{path, rootKey})
		return os.RemoveAll(path)
	})

	s.EvictIdleWorkspaces(context.Background(), time.Hour)

	if len(removals) != 1 || removals[0].path != wtPath || removals[0].rootKey != bpr {
		t.Fatalf("removals = %+v, want exactly one through the privileged seam at %s keyed by %s", removals, wtPath, bpr)
	}
}

// TestEvictIdleWorkspaces_RefusesWithoutAVerifiedSnapshot walks the four ways a
// snapshot can fail to be a second copy of the work. Each one leaves the tree
// as the only copy, so each one must leave it on disk.
func TestEvictIdleWorkspaces_RefusesWithoutAVerifiedSnapshot(t *testing.T) {
	for _, tc := range []struct {
		name  string
		state string
		// blob says whether the object is actually in the store, which is the
		// case a state row alone cannot answer.
		blob bool
	}{
		{name: "state_pending", state: domain.WorkspaceSnapshotPending, blob: true},
		{name: "state_failed", state: domain.WorkspaceSnapshotFailed, blob: true},
		{name: "state_absent", state: "", blob: true},
		{name: "written_but_blob_gone", state: domain.WorkspaceSnapshotWritten, blob: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateRunNamespace(t)
			s, database, conversationID, _ := setupAdvanceFixture(t, "evict-"+tc.name)
			wireBlobStore(t, s)
			ctx := context.Background()

			bpr := blueprintRunIDForConversation(t, database, conversationID)
			wtPath := makeRunTree(t, bpr)
			if tc.blob {
				putTestSnapshot(t, s, bpr)
			}
			if tc.state != "" {
				seedSnapshotState(t, s, bpr, tc.state)
			}
			parkAged(t, database, conversationID, wtPath, "-2 hours")

			s.EvictIdleWorkspaces(ctx, time.Hour)

			if _, err := os.Stat(wtPath); err != nil {
				t.Fatalf("worktree evicted with snapshot state %q / blob present=%v; the tree was the only copy of the agent's work", tc.state, tc.blob)
			}
		})
	}
}

// TestEvictIdleWorkspaces_RefusesWhileASiblingStepIsClaimed: a blueprint's steps
// share one tree, so a step parked past the TTL says nothing about whether
// anyone is using the directory — the live sibling is working in it.
func TestEvictIdleWorkspaces_RefusesWhileASiblingStepIsClaimed(t *testing.T) {
	isolateRunNamespace(t)
	s, database, conversationID, taskID := setupAdvanceFixture(t, "evict-sibling")
	wireBlobStore(t, s)
	ctx := context.Background()

	bpr, wtPath := seedEvictableTree(t, s, database, conversationID)
	addStepConversation(t, database, bpr, taskID, "step-live", 1, "running")
	if err := s.conversations.SetExecutorSystem(ctx, runmode.LocalDefaultOrgID, "step-live", "exec-live", 1); err != nil {
		t.Fatalf("mint sibling claim: %v", err)
	}

	s.EvictIdleWorkspaces(ctx, time.Hour)

	if _, err := os.Stat(wtPath); err != nil {
		t.Fatalf("shared tree %s evicted under a live sibling step: %v", wtPath, err)
	}

	// Release the sibling and settle it, and the same sweep reclaims the tree —
	// the live claim was the only thing holding it.
	if err := s.conversations.SetExecutorSystem(ctx, runmode.LocalDefaultOrgID, "step-live", "", 0); err != nil {
		t.Fatalf("release sibling claim: %v", err)
	}
	if _, err := database.Exec(`UPDATE conversations SET status='completed', completed_at=datetime('now','-2 hours') WHERE id='step-live'`); err != nil {
		t.Fatalf("settle sibling: %v", err)
	}
	s.EvictIdleWorkspaces(ctx, time.Hour)
	if _, err := os.Stat(wtPath); !os.IsNotExist(err) {
		t.Fatalf("tree %s survived once every step was at rest and unclaimed (stat err %v)", wtPath, err)
	}
}

// TestEvictIdleWorkspaces_RefusesWithinTheIdleWindow: the TTL is the whole
// distinction between a warm cache and a stale one.
func TestEvictIdleWorkspaces_RefusesWithinTheIdleWindow(t *testing.T) {
	isolateRunNamespace(t)
	s, database, conversationID, _ := setupAdvanceFixture(t, "evict-ttl")
	wireBlobStore(t, s)

	_, wtPath := seedEvictableTree(t, s, database, conversationID)
	// Parked two hours ago, evicted after six: still warm.
	s.EvictIdleWorkspaces(context.Background(), 6*time.Hour)

	if _, err := os.Stat(wtPath); err != nil {
		t.Fatalf("tree %s evicted inside the idle window: %v", wtPath, err)
	}
}

// TestEvictIdleWorkspaces_RefusesAPathOutsideTheRunNamespace: worktree_path is
// a value some other process wrote, and eviction hands it to a privileged
// removal. A path that is not one of our ephemeral run trees is refused rather
// than deleted on the strength of a database column.
func TestEvictIdleWorkspaces_RefusesAPathOutsideTheRunNamespace(t *testing.T) {
	isolateRunNamespace(t)
	s, database, conversationID, _ := setupAdvanceFixture(t, "evict-foreign")
	wireBlobStore(t, s)
	ctx := context.Background()

	bpr := blueprintRunIDForConversation(t, database, conversationID)
	foreign := filepath.Join(t.TempDir(), "someones-checkout")
	if err := os.MkdirAll(foreign, 0o755); err != nil {
		t.Fatalf("mkdir foreign tree: %v", err)
	}
	putTestSnapshot(t, s, bpr)
	seedSnapshotState(t, s, bpr, domain.WorkspaceSnapshotWritten)
	parkAged(t, database, conversationID, foreign, "-2 hours")

	var removed []string
	restoreRemoveSeam(t, func(path, _ string) error {
		removed = append(removed, path)
		return os.RemoveAll(path)
	})

	s.EvictIdleWorkspaces(ctx, time.Hour)

	if len(removed) != 0 {
		t.Fatalf("evictor removed %v; a recorded path outside the run namespace is not ours to delete", removed)
	}
	if _, err := os.Stat(foreign); err != nil {
		t.Fatalf("foreign directory %s was deleted: %v", foreign, err)
	}
}

// TestEvictIdleWorkspaces_RefusesAnotherKeysRunTree: a recorded path can be a
// perfectly valid run tree and still belong to a different snapshot key. Every
// gate the sweep passed — nothing claimed, snapshot written, blob present — is
// evidence about THIS key and says nothing about that directory, whose own key
// may have a live engagement in it right now. So the path is refused rather
// than reasoned about.
func TestEvictIdleWorkspaces_RefusesAnotherKeysRunTree(t *testing.T) {
	isolateRunNamespace(t)
	s, database, conversationID, _ := setupAdvanceFixture(t, "evict-wrongkey")
	wireBlobStore(t, s)
	ctx := context.Background()

	bpr := blueprintRunIDForConversation(t, database, conversationID)
	// A run tree in the right namespace, under someone else's key.
	otherKey := "0f8f1b1c-0000-4000-8000-00000000beef"
	otherTree := makeRunTree(t, otherKey)

	putTestSnapshot(t, s, bpr)
	seedSnapshotState(t, s, bpr, domain.WorkspaceSnapshotWritten)
	parkAged(t, database, conversationID, otherTree, "-2 hours")

	var removed []string
	restoreRemoveSeam(t, func(path, _ string) error {
		removed = append(removed, path)
		return os.RemoveAll(path)
	})

	s.EvictIdleWorkspaces(ctx, time.Hour)

	if len(removed) != 0 {
		t.Fatalf("evictor removed %v; the recorded path is %s's tree, not %s's, so nothing this sweep checked applies to it", removed, otherKey, bpr)
	}
	if _, err := os.Stat(otherTree); err != nil {
		t.Fatalf("another key's workspace %s was deleted: %v", otherTree, err)
	}
}

// TestRunWorkspaceEvictor_DisabledKnobNeverSweeps is the local-mode default:
// with the knob unset the sweep does not start at all, so a parked tree that is
// evictable in every other respect stays on the user's disk.
func TestRunWorkspaceEvictor_DisabledKnobNeverSweeps(t *testing.T) {
	isolateRunNamespace(t)
	s, database, conversationID, _ := setupAdvanceFixture(t, "evict-off")
	wireBlobStore(t, s)

	_, wtPath := seedEvictableTree(t, s, database, conversationID)

	// Returns rather than blocking — a disabled sweep starts no ticker, so
	// calling it on this goroutine is the assertion that it never runs.
	disabled, err := ParseWorkspaceEvictAfter("", true)
	if err != nil {
		t.Fatalf("ParseWorkspaceEvictAfter: %v", err)
	}
	s.RunWorkspaceEvictor(context.Background(), time.Millisecond, disabled)

	if _, err := os.Stat(wtPath); err != nil {
		t.Fatalf("tree %s evicted with the knob unset in local mode: %v", wtPath, err)
	}
}

// --- helpers ---------------------------------------------------------------

// isolateRunNamespace points $TMPDIR (and so worktree.RunRoot) and the state
// root at this test's own directories, so a sweep that walks the run namespace
// can only ever see this test's trees.
func isolateRunNamespace(t *testing.T) {
	t.Helper()
	paths.SetForTest(t, t.TempDir())
	t.Setenv("TMPDIR", t.TempDir())
}

// restoreRemoveSeam swaps the privileged removal seam for the duration of the
// test.
func restoreRemoveSeam(t *testing.T, fn func(path, rootKey string) error) {
	t.Helper()
	prev := removeWorkspaceTree
	removeWorkspaceTree = fn
	t.Cleanup(func() { removeWorkspaceTree = prev })
}

// seedEvictableTree stages the fully evictable shape — a real directory under
// the run namespace, a present blob, a `written` state row, and a park two
// hours old — so each test above varies exactly one thing away from it.
func seedEvictableTree(t *testing.T, s *Spawner, database *sql.DB, conversationID string) (blueprintRunID, wtPath string) {
	t.Helper()
	blueprintRunID = blueprintRunIDForConversation(t, database, conversationID)
	wtPath = makeRunTree(t, blueprintRunID)
	putTestSnapshot(t, s, blueprintRunID)
	seedSnapshotState(t, s, blueprintRunID, domain.WorkspaceSnapshotWritten)
	parkAged(t, database, conversationID, wtPath, "-2 hours")
	return blueprintRunID, wtPath
}

// makeRunTree creates a directory where a run tree for keyID would live, with
// something in it, and returns its path.
func makeRunTree(t *testing.T, keyID string) string {
	t.Helper()
	wtPath := worktree.RunRoot(keyID)
	if err := os.MkdirAll(filepath.Join(wtPath, "_tfac"), 0o755); err != nil {
		t.Fatalf("mkdir run tree: %v", err)
	}
	writeFile(t, filepath.Join(wtPath, "_tfac", "notes.md"), "work in progress")
	return wtPath
}

// seedSnapshotState drives the real lifecycle store to leave the key in state:
// begin, then finish written/failed. `pending` is just the begin with no
// finish, which is what an in-flight persist actually looks like.
func seedSnapshotState(t *testing.T, s *Spawner, keyID, state string) {
	t.Helper()
	ctx := context.Background()
	if err := s.workspaceSnapshots.BeginSnapshotSystem(ctx, runmode.LocalDefaultOrgID, keyID, evictWriterClaim); err != nil {
		t.Fatalf("BeginSnapshotSystem: %v", err)
	}
	if state == domain.WorkspaceSnapshotPending {
		return
	}
	if _, err := s.workspaceSnapshots.FinishSnapshotSystem(ctx, runmode.LocalDefaultOrgID, keyID, evictWriterClaim, state == domain.WorkspaceSnapshotWritten); err != nil {
		t.Fatalf("FinishSnapshotSystem: %v", err)
	}
}

// parkAged parks the conversation `open` with its worktree path recorded and
// its park backdated by the given SQLite modifier.
func parkAged(t *testing.T, database *sql.DB, conversationID, wtPath, age string) {
	t.Helper()
	if _, err := database.Exec(
		`UPDATE conversations SET status='open', worktree_path=?, parked_at=datetime('now',?), completed_at=NULL WHERE id=?`,
		wtPath, age, conversationID,
	); err != nil {
		t.Fatalf("park conversation: %v", err)
	}
}

func worktreePathOf(t *testing.T, database *sql.DB, conversationID string) string {
	t.Helper()
	var path sql.NullString
	if err := database.QueryRow(`SELECT worktree_path FROM conversations WHERE id=?`, conversationID).Scan(&path); err != nil {
		t.Fatalf("read worktree_path: %v", err)
	}
	return strings.TrimSpace(path.String)
}
