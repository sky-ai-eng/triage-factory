package gh

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/cmd/exec/agenthost"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// reviewAnchorHost answers the reads add-review-comment makes — the review
// draft's recorded PR and the run's checkouts — and records what the verb
// stages. It embeds agenthost.Client (nil) so it satisfies the interface; a
// verb reaching any other method panics, failing the test loudly.
type reviewAnchorHost struct {
	agenthost.Client
	checkouts   fakeCheckouts
	owner, repo string
	number      int

	gotOwner, gotRepo, gotAnchor string
}

func (h *reviewAnchorHost) LookupConversation(context.Context) (agenthost.ConversationInfo, error) {
	return agenthost.ConversationInfo{ConversationID: "conv-1", OrgID: "org-1"}, nil
}

func (h *reviewAnchorHost) ReviewDraftTarget(context.Context, string) (string, string, int, error) {
	return h.owner, h.repo, h.number, nil
}

func (h *reviewAnchorHost) ListConversationWorktrees(ctx context.Context) ([]domain.ConversationWorktree, error) {
	return h.checkouts.ListConversationWorktrees(ctx)
}

func (h *reviewAnchorHost) WorkspaceRoots(ctx context.Context) (string, string, error) {
	return h.checkouts.WorkspaceRoots(ctx)
}

func (h *reviewAnchorHost) GithubAddPendingReviewComment(_ context.Context, owner, repo, _, _, _ string, _ int, _ *int, commitSHA string) (string, error) {
	h.gotOwner, h.gotRepo, h.gotAnchor = owner, repo, commitSHA
	return "comment-1", nil
}

// gitCheckoutCommitted makes dir a checkout with one commit unique to it (its
// own marker file) and an origin, returning its HEAD. Distinct content keeps
// the HEADs distinct, so an anchor names exactly one checkout.
func gitCheckoutCommitted(t *testing.T, dir, ownerRepo string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(cmd.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run("init", "-q")
	if err := os.WriteFile(filepath.Join(dir, "marker"), []byte(dir), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", ".")
	run("commit", "-q", "-m", "marker")
	run("remote", "add", "origin", "https://github.com/"+ownerRepo+".git")
	return run("rev-parse", "HEAD")
}

// TestPRAddReviewComment_AnchorsToTheReviewedPRCheckout drives
// add-review-comment in a run tree laid out as the sandbox sees it: registry
// paths recorded in host view under a different root, two PR checkouts of the
// same repo, and a checkout of an unrelated repo. Wherever the agent stands,
// the comment goes to the review's repo and anchors to the HEAD of that PR's
// own checkout. A review whose PR the run holds no checkout of sends an empty
// anchor, so the host anchors to the live head, the frame `pr diff` used.
func TestPRAddReviewComment_AnchorsToTheReviewedPRCheckout(t *testing.T) {
	isolateGit(t)
	agentRoot := t.TempDir()
	const hostRoot = "/host/runs/task-1"

	pr42 := filepath.Join(agentRoot, "owner", "repo", "pr-42")
	pr4 := filepath.Join(agentRoot, "owner", "repo", "pr-4")
	other := filepath.Join(agentRoot, "other", "lib", "default")
	pr42Head := gitCheckoutCommitted(t, pr42, "owner/repo")
	gitCheckoutCommitted(t, pr4, "owner/repo")
	gitCheckoutCommitted(t, other, "other/lib")

	checkouts := fakeCheckouts{
		hostRoot:  hostRoot,
		agentRoot: agentRoot,
		rows: []domain.ConversationWorktree{
			{RepoID: "owner/repo", Ref: "pr-4", Path: hostRoot + "/owner/repo/pr-4"},
			{RepoID: "owner/repo", Ref: "pr-42", Path: hostRoot + "/owner/repo/pr-42"},
			{RepoID: "other/lib", Ref: "default", Path: hostRoot + "/other/lib/default"},
		},
	}

	for _, tc := range []struct {
		name       string
		number     int
		wd         string
		wantAnchor string
	}{
		{"from another repo's checkout", 42, other, pr42Head},
		{"from another PR's checkout of the same repo", 42, pr4, pr42Head},
		{"from the run root", 42, agentRoot, pr42Head},
		{"no checkout of the reviewed PR", 99, pr4, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Chdir(tc.wd)
			host := &reviewAnchorHost{checkouts: checkouts, owner: "owner", repo: "repo", number: tc.number}
			captureStdout(t, func() {
				prAddReviewComment(context.Background(), host, []string{"rv-1", "--file", "a.go", "--line", "3", "--body", "nit"})
			})
			if host.gotOwner != "owner" || host.gotRepo != "repo" {
				t.Errorf("staged on %s/%s, want the review's owner/repo", host.gotOwner, host.gotRepo)
			}
			if host.gotAnchor != tc.wantAnchor {
				t.Errorf("anchor = %q, want %q", host.gotAnchor, tc.wantAnchor)
			}
		})
	}
}

// TestReviewCommentTarget_RepoFlag pins that an explicit --repo has to agree
// with the review it is commenting on (case-insensitively, like every repo
// comparison here) rather than silently redirecting the comment.
func TestReviewCommentTarget_RepoFlag(t *testing.T) {
	host := &reviewAnchorHost{owner: "owner", repo: "repo", number: 42}

	owner, repo, _, err := reviewCommentTarget(context.Background(), host, "rv-1", []string{"--repo", "Owner/Repo"})
	if err != nil || owner != "owner" || repo != "repo" {
		t.Errorf("matching --repo: got (%s/%s, %v), want owner/repo", owner, repo, err)
	}

	_, _, _, err = reviewCommentTarget(context.Background(), host, "rv-1", []string{"--repo", "other/lib"})
	if err == nil || !strings.Contains(err.Error(), "owner/repo#42") {
		t.Errorf("mismatched --repo: want an error naming the review's PR, got %v", err)
	}

	if _, _, _, err := reviewCommentTarget(context.Background(), host, "rv-1", []string{"--repo"}); err == nil {
		t.Error("--repo with no value should be refused")
	}
}
