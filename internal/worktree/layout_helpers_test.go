package worktree

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// createPRCheckout builds a PR checkout the way a PR run's setup does: a run
// root keyed by rootKey, and the checkout beneath it, namespaced by rootKey.
func createPRCheckout(ctx context.Context, owner, repo, upstreamCloneURL, headCloneURL, headBranch string, prNumber int, rootKey string, opts ...CloneOption) (string, error) {
	root, err := MakeRunRoot(rootKey)
	if err != nil {
		return "", err
	}
	return CreateForPRInRoot(ctx, testRepo(owner, repo), upstreamCloneURL, headCloneURL, headBranch, prNumber, rootKey, root, opts...)
}

// createBranchCheckout builds a prescribed-branch checkout beneath a run root
// keyed by rootKey.
func createBranchCheckout(ctx context.Context, owner, repo, cloneURL, baseBranch, featureBranch, rootKey string, opts ...CloneOption) (string, error) {
	root, err := MakeRunRoot(rootKey)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Join(root, owner), 0o755); err != nil {
		return "", err
	}
	return createBranchWorktreeAt(ctx, testRepo(owner, repo), cloneURL, baseBranch, featureBranch, filepath.Join(root, owner, repo), resolveCloneOptions(opts).auth)
}

// mustRunRoot makes rootKey's run root or fails the test.
func mustRunRoot(t *testing.T, rootKey string) string {
	t.Helper()
	root, err := MakeRunRoot(rootKey)
	if err != nil {
		t.Fatalf("MakeRunRoot(%q): %v", rootKey, err)
	}
	return root
}

// restoreDelta stages delta's members the way a capture hands them over and
// rebuilds the checkout under root through RestoreCheckout, returning the
// restored checkout's path. slug is the checkout's conversation_worktrees ref.
func restoreDelta(t *testing.T, root, rootKey, owner, repo, slug, cloneURL string, auth CloneAuth, delta *GitDelta, pr *PRCheckout) string {
	t.Helper()
	got, err := restoreDeltaErr(t, root, rootKey, owner, repo, slug, cloneURL, auth, delta, pr)
	if err != nil {
		t.Fatalf("RestoreCheckout: %v", err)
	}
	return got.Path
}

func restoreDeltaErr(t *testing.T, root, rootKey, owner, repo, slug, cloneURL string, auth CloneAuth, delta *GitDelta, pr *PRCheckout) (RestoredCheckout, error) {
	t.Helper()
	staging := t.TempDir()
	r := CheckoutRestore{
		RepositoryID: testRepo(owner, repo).ID, Owner: owner, Repo: repo, CloneURL: cloneURL, Auth: auth,
		Root: root, Slug: slug, RootKey: rootKey, PR: pr,
		Head: delta.Head, Branch: delta.Branch,
	}
	if len(delta.Bundle) > 0 {
		r.BundlePath = filepath.Join(staging, "bundle")
		if err := os.WriteFile(r.BundlePath, delta.Bundle, 0o600); err != nil {
			t.Fatalf("stage bundle: %v", err)
		}
	}
	if len(delta.Patch) > 0 {
		r.PatchPath = filepath.Join(staging, "patch")
		if err := os.WriteFile(r.PatchPath, delta.Patch, 0o600); err != nil {
			t.Fatalf("stage patch: %v", err)
		}
	}
	return RestoreCheckout(context.Background(), r)
}
