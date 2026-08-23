package worktree

import (
	"context"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// startAuthedGitServer serves reposRoot over TLS via git-http-backend, gated by
// HTTP Basic auth that must match wantUser/wantToken — the
// "x-access-token:<token>" pair CloneAuth injects as a host-scoped extraHeader.
// It is the stand-in for a private GitHub host: an anonymous request gets a 401
// with a Basic challenge (which, prompts disabled, makes git fail fast), and an
// authenticated one is proxied to git's smart-HTTP backend. TLS because
// CloneAuthFor only injects for https:// URLs; the caller sets GIT_SSL_NO_VERIFY
// so git accepts the self-signed cert. Skips the test when git-http-backend
// isn't on disk (no partial-clone server to exercise).
func startAuthedGitServer(t *testing.T, reposRoot, wantUser, wantToken string) string {
	t.Helper()
	execPathOut, err := exec.Command("git", "--exec-path").Output()
	if err != nil {
		t.Fatalf("git --exec-path: %v", err)
	}
	backend := filepath.Join(strings.TrimSpace(string(execPathOut)), "git-http-backend")
	if _, err := os.Stat(backend); err != nil {
		t.Skipf("git-http-backend not available (%v); skipping blobless-promisor HTTP integration test", err)
	}

	handler := &cgi.Handler{
		Path: backend,
		Env: []string{
			"GIT_PROJECT_ROOT=" + reposRoot,
			"GIT_HTTP_EXPORT_ALL=1",
		},
	}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok || user != wantUser || pass != wantToken {
			w.Header().Set("WWW-Authenticate", `Basic realm="git"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// seedBloblessUpstream creates a bare repo at reposRoot/<repo>.git carrying one
// commit with a real file (so a --filter=blob:none clone actually defers a blob
// and the checkout must lazily fetch it), enables the partial-clone server
// capabilities the promisor fetch needs, and returns the upstream's on-disk
// path plus the committed file's content. extraRefs are additional fully-
// qualified refs (e.g. "refs/pull/7/head") to point at the same tip — used by
// the PR path, which fetches via refs/pull/<n>/head.
func seedBloblessUpstream(t *testing.T, reposRoot, repo string, extraRefs ...string) (upstreamPath, content string) {
	t.Helper()
	upstreamPath = filepath.Join(reposRoot, repo+".git")
	runGit(t, "init", "--bare", "--initial-branch=main", upstreamPath)
	// Partial-clone server caps: advertise the filter, and allow fetching objects
	// by OID so the deferred-blob lazy fetch resolves.
	runGit(t, "-C", upstreamPath, "config", "uploadpack.allowFilter", "true")
	runGit(t, "-C", upstreamPath, "config", "uploadpack.allowAnySHA1InWant", "true")

	work := filepath.Join(t.TempDir(), "seed-"+repo)
	runGit(t, "init", "-b", "main", work)
	runGit(t, "-C", work, "config", "user.email", "test@example.com")
	runGit(t, "-C", work, "config", "user.name", "Test")
	content = "blobless-promisor-fetch-canary-" + repo + "\n"
	if err := os.WriteFile(filepath.Join(work, "canary.txt"), []byte(content), 0644); err != nil {
		t.Fatalf("write canary: %v", err)
	}
	runGit(t, "-C", work, "add", "canary.txt")
	runGit(t, "-C", work, "commit", "-m", "add canary blob")
	runGit(t, "-C", work, "remote", "add", "origin", upstreamPath)
	runGit(t, "-C", work, "push", "origin", "main")
	for _, ref := range extraRefs {
		runGit(t, "-C", work, "push", upstreamPath, "HEAD:"+ref)
	}
	return upstreamPath, content
}

func runGit(t *testing.T, args ...string) {
	t.Helper()
	if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
}

// TestGitBaseEnv_ForcesPromptDisabledOverInherited locks in the prompt-hardening
// guarantee against an inherited GIT_TERMINAL_PROMPT: even when the parent
// process preset =1, gitBaseEnv() must yield exactly one GIT_TERMINAL_PROMPT
// entry set to 0, so a credential-less git op can never re-acquire an
// interactive prompt — independent of how the exec layer resolves duplicate
// env keys.
func TestGitBaseEnv_ForcesPromptDisabledOverInherited(t *testing.T) {
	t.Setenv("GIT_TERMINAL_PROMPT", "1")
	var vals []string
	for _, kv := range gitBaseEnv() {
		if v, ok := strings.CutPrefix(kv, "GIT_TERMINAL_PROMPT="); ok {
			vals = append(vals, v)
		}
	}
	if len(vals) != 1 || vals[0] != "0" {
		t.Errorf("GIT_TERMINAL_PROMPT entries = %v, want exactly [\"0\"]", vals)
	}
}

// TestGitBaseEnv_StripsInheritedAskpass guards the editor-terminal hang: an
// inherited GIT_ASKPASS/SSH_ASKPASS (Cursor / VS Code export one) drives git to
// a GUI credential prompt regardless of GIT_TERMINAL_PROMPT=0, so a failed-auth
// fetch hangs instead of fast-failing. gitBaseEnv must drop them from the child
// env so the fast-fail guarantee actually holds.
func TestGitBaseEnv_StripsInheritedAskpass(t *testing.T) {
	t.Setenv("GIT_ASKPASS", "/Applications/SomeEditor/askpass.sh")
	t.Setenv("SSH_ASKPASS", "/usr/bin/ssh-askpass")
	for _, kv := range gitBaseEnv() {
		if strings.HasPrefix(kv, "GIT_ASKPASS=") || strings.HasPrefix(kv, "SSH_ASKPASS=") {
			t.Errorf("gitBaseEnv leaked an askpass helper: %q (git would prompt and hang instead of fast-failing)", kv)
		}
	}
}

// TestCheckoutOpsNeverUseUnauthWrapper is the static regression guard for
// TFAC-401: the blob-materializing host-side ops (`git worktree add`, `git reset
// --hard`) must always route through gitRunCtxAuth so the blobless bare's lazy
// promisor fetch carries the credential. If any of them regresses to the
// no-auth gitRunCtx wrapper, a private-repo checkout would break again with
// "could not read Username". The scan keys on the op's argument literals on the
// same line as the call (how every site in these files is written); note
// `gitRunCtx(` does not match `gitRunCtxAuth(` because the next char is `A`.
//
// Limitation: a call wrapped so the op literal lands on a different line from
// `gitRunCtx(` would slip through. That can't happen for these short calls under
// gofmt (they stay single-line), so a line-scoped scan is sufficient here.
func TestCheckoutOpsNeverUseUnauthWrapper(t *testing.T) {
	files := []string{
		"worktree_pr.go",
		"worktree_branch.go",
		"snapshot.go",
	}
	materializingOps := []string{`"worktree", "add"`, `"reset", "--hard"`}
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		for i, line := range strings.Split(string(data), "\n") {
			if !strings.Contains(line, "gitRunCtx(") {
				continue // gitRunCtxAuth( and non-git lines are fine
			}
			for _, op := range materializingOps {
				if strings.Contains(line, op) {
					t.Errorf("%s:%d invokes a blob-materializing op (%s) via the no-auth gitRunCtx wrapper; use gitRunCtxAuth so the blobless promisor fetch authenticates:\n\t%s",
						f, i+1, op, strings.TrimSpace(line))
				}
			}
		}
	}
}

// TestWorktreeAdd_BloblessPromisorFetch_AuthAndFastFail is the core seam
// regression for TFAC-401. Against a real blobless bare whose origin promisor
// remote requires HTTPS auth, it drives `git worktree add` through the exact
// helper the host-side checkout sites use (gitRunCtxAuth) and asserts both
// halves of the fix:
//
//   - WITHOUT auth (inert CloneAuth, the old gitRunCtx behavior): the lazy fetch
//     of the deferred blob is anonymous, hits a 401, and — with prompts disabled
//     (Part 1) — fails FAST with "terminal prompts disabled" rather than hanging
//     on /dev/tty. That this fails at all also proves the blob really was
//     deferred by the blobless clone.
//   - WITH auth (Part 2): the host-scoped extraHeader rides the subprocess env
//     into the promisor fetch, the blob materializes, and the checkout's
//     canary.txt has the upstream content.
func TestWorktreeAdd_BloblessPromisorFetch_AuthAndFastFail(t *testing.T) {
	withTestHome(t)
	t.Setenv("GIT_SSL_NO_VERIFY", "true") // accept the httptest self-signed cert

	reposRoot := t.TempDir()
	const token = "ghs_blobless_token"
	_, content := seedBloblessUpstream(t, reposRoot, "repo")
	base := startAuthedGitServer(t, reposRoot, "x-access-token", token)
	cloneURL := base + "/repo.git"

	// Blobless-clone the bare WITH auth so the bare exists but defers the blob.
	auth := CloneAuthFor(cloneURL, token)
	bareDir, err := EnsureBareClone(context.Background(), "acme", "repo", cloneURL, WithCloneAuth(auth))
	if err != nil {
		t.Fatalf("EnsureBareClone (blobless, authed): %v", err)
	}

	// Negative: an inert auth (what gitRunCtx passed before the fix) can't
	// authenticate the promisor fetch and must fail fast, not hang.
	noAuthWT := runDir("run-noauth")
	err = gitRunCtxAuth(context.Background(), bareDir, CloneAuth{}, "worktree", "add", noAuthWT, "main")
	if err == nil {
		t.Fatal("worktree add WITHOUT auth unexpectedly succeeded — the blobless promisor fetch should have failed for lack of credentials")
	}
	if !strings.Contains(err.Error(), "terminal prompts disabled") {
		t.Errorf("no-auth worktree add error should fail fast with %q, got: %v", "terminal prompts disabled", err)
	}
	_ = RemoveAt(noAuthWT, "run-noauth")

	// Positive: the same op WITH auth materializes the deferred blob.
	authWT := runDir("run-auth")
	if err := gitRunCtxAuth(context.Background(), bareDir, auth, "worktree", "add", authWT, "main"); err != nil {
		t.Fatalf("worktree add WITH auth failed — the promisor fetch should have authenticated: %v", err)
	}
	t.Cleanup(func() { _ = RemoveAt(authWT, "run-auth") })

	got, err := os.ReadFile(filepath.Join(authWT, "canary.txt"))
	if err != nil {
		t.Fatalf("read canary from authed checkout: %v", err)
	}
	if string(got) != content {
		t.Errorf("canary.txt = %q, want %q (deferred blob not materialized by the authed lazy fetch)", got, content)
	}
}

// TestCreateForBranch_BloblessPrivateRepo is the branch-path acceptance test:
// the full CreateForBranch flow (authed blobless clone → fetch base → `git
// worktree add -b`) against a private repo must complete setup and materialize
// the working tree. Pre-fix, the unauth `git worktree add` would fail the lazy
// promisor fetch with "could not read Username".
func TestCreateForBranch_BloblessPrivateRepo(t *testing.T) {
	withTestHome(t)
	t.Setenv("GIT_SSL_NO_VERIFY", "true")

	reposRoot := t.TempDir()
	const token = "ghs_branch_token"
	_, content := seedBloblessUpstream(t, reposRoot, "repo")
	cloneURL := startAuthedGitServer(t, reposRoot, "x-access-token", token) + "/repo.git"

	wtPath, err := CreateForBranch(context.Background(), "acme", "repo", cloneURL, "main", "aa/feature", "branch-run",
		WithCloneAuth(CloneAuthFor(cloneURL, token)))
	if err != nil {
		t.Fatalf("CreateForBranch on a private blobless repo: %v", err)
	}
	t.Cleanup(func() { _ = RemoveAt(wtPath, "branch-run") })

	got, err := os.ReadFile(filepath.Join(wtPath, "canary.txt"))
	if err != nil {
		t.Fatalf("read canary from branch worktree: %v", err)
	}
	if string(got) != content {
		t.Errorf("canary.txt = %q, want %q", got, content)
	}
}

// TestCreateForPR_BloblessPrivateRepo is the PR-path acceptance test: the full
// CreateForPR flow (authed blobless clone → fetch refs/pull/<n>/head → `git
// worktree add`) against a private repo must complete and materialize the
// working tree.
func TestCreateForPR_BloblessPrivateRepo(t *testing.T) {
	withTestHome(t)
	t.Setenv("GIT_SSL_NO_VERIFY", "true")

	reposRoot := t.TempDir()
	const token = "ghs_pr_token"
	_, content := seedBloblessUpstream(t, reposRoot, "repo", "refs/pull/7/head")
	cloneURL := startAuthedGitServer(t, reposRoot, "x-access-token", token) + "/repo.git"

	// Own-repo PR: head URL == upstream URL.
	wtPath, err := CreateForPR(context.Background(), "acme", "repo", cloneURL, cloneURL, "feature-branch", 7, "pr-run",
		WithCloneAuth(CloneAuthFor(cloneURL, token)))
	if err != nil {
		t.Fatalf("CreateForPR on a private blobless repo: %v", err)
	}
	t.Cleanup(func() { _ = RemoveAt(wtPath, "pr-run") })

	got, err := os.ReadFile(filepath.Join(wtPath, "canary.txt"))
	if err != nil {
		t.Fatalf("read canary from PR worktree: %v", err)
	}
	if string(got) != content {
		t.Errorf("canary.txt = %q, want %q", got, content)
	}
}

// TestRestoreWorkspaceGit_BloblessPrivateRepo is the resume-path acceptance
// test for a fresh executor: the bare is absent, so RestoreWorkspaceGit must
// (1) re-clone it authed via EnsureBareClone — the unauth seed was the Part-3
// bug — and (2) `git worktree add` authed so the blobless checkout's lazy
// promisor fetch succeeds. Asserts the rebuilt worktree carries the file.
func TestRestoreWorkspaceGit_BloblessPrivateRepo(t *testing.T) {
	withTestHome(t)
	t.Setenv("GIT_SSL_NO_VERIFY", "true")

	reposRoot := t.TempDir()
	const token = "ghs_restore_token"
	upstreamPath, content := seedBloblessUpstream(t, reposRoot, "repo")
	cloneURL := startAuthedGitServer(t, reposRoot, "x-access-token", token) + "/repo.git"

	// The snapshot delta records the branch + its tip; read the tip straight
	// from the upstream (no local bare yet — this is the fresh-executor case).
	tipOut, err := exec.Command("git", "-C", upstreamPath, "rev-parse", "refs/heads/main").Output()
	if err != nil {
		t.Fatalf("rev-parse upstream main: %v", err)
	}
	delta := &GitDelta{Branch: "main", Head: strings.TrimSpace(string(tipOut))}

	wtDir := runDir("restore-run")
	if err := RestoreWorkspaceGit(context.Background(), "acme", "repo", wtDir, delta, cloneURL, CloneAuthFor(cloneURL, token)); err != nil {
		t.Fatalf("RestoreWorkspaceGit on a private blobless repo (fresh executor): %v", err)
	}
	t.Cleanup(func() { _ = RemoveAt(wtDir, "restore-run") })

	got, err := os.ReadFile(filepath.Join(wtDir, "canary.txt"))
	if err != nil {
		t.Fatalf("read canary from restored worktree: %v", err)
	}
	if string(got) != content {
		t.Errorf("canary.txt = %q, want %q", got, content)
	}
}
