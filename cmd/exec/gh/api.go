package gh

import (
	"context"
	"errors"
	"io"

	"github.com/sky-ai-eng/triage-factory/cmd/exec/agenthost"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
)

// ghAPI is the GitHub-API surface the pr/actions subcommands consume. It is
// deliberately the exact method set of *github.Client, so a real client still
// satisfies it (the package tests drive the helpers against an httptest
// backend directly) while production routes the same calls through the host
// agenthost daemon, which holds the credential the sandbox must not.
//
// Property B: in multi mode the agent process never builds a *github.Client
// of its own — it only ever holds a hostAPIClient whose every method is an
// RPC to the host. The host resolves the org's App-installation-or-PAT
// credential (github.Resolver.ClientForRepo) and makes the call. In local
// mode the same surface is backed by an in-process LocalClient that builds
// the client on the user's own machine; either way no token resides in a jail.
type ghAPI interface {
	GetPR(owner, repo string, number int, verbose bool) (*ghclient.PRView, error)
	GetPRDiff(owner, repo string, number int, file string) (string, error)
	GetPRFiles(owner, repo string, number int) ([]ghclient.PRFile, error)
	GetCommentThread(owner, repo string, commentID, page int) (*ghclient.CommentThread, error)
	GetReviewDetail(owner, repo string, number, reviewID int, verbose bool) (*ghclient.ReviewDetail, error)
	DismissReview(owner, repo string, number, reviewID int, message string) error
	SubmitReview(owner, repo string, number int, commitSHA, event, body string, comments []ghclient.SubmitReviewComment) (int, string, error)
	CreatePR(owner, repo, head, base, title, body string, draft bool) (int, string, string, error)
	AddComment(owner, repo string, number int, body string) (int, error)
	ReplyToComment(owner, repo string, number, commentID int, body string) (int, error)
	ReactToComment(owner, repo string, commentID int, emoji string) error
	UpdateComment(owner, repo string, commentID int, body string) error
	DeleteComment(owner, repo string, commentID int) error

	// Pending-review surface (TFAC-469): the GitHub-native preview backing.
	// Signatures mirror *github.Client exactly (so a real client still
	// satisfies ghAPI); the host-backed adapter routes them through the
	// agenthost daemon, where CreatePendingReview also runs the start-review
	// collision check (the real client does a raw create — the check is a
	// host-only concern keyed on the resolved credential identity).
	// AddPendingReviewComment carries no owner/repo (it keys off the review
	// node id), so the host adapter folds in the repo it was constructed with,
	// like Get/DownloadArtifact below.
	CreatePendingReview(owner, repo string, number int, commitSHA string, comments []ghclient.SubmitReviewComment) (string, error)
	AddPendingReviewComment(reviewID string, comment ghclient.SubmitReviewComment) (string, error)
	GetPendingReview(owner, repo string, number int) (string, []ghclient.PendingReviewComment, error)

	// Get / DownloadArtifact are the raw transport the actions verbs lean on
	// (workflow-run + job listings and log archives have no typed method).
	// They carry no owner/repo, so the host-backed adapter folds in the repo
	// it was constructed with for the credential-tier resolution.
	Get(path string) ([]byte, error)
	DownloadArtifact(ctx context.Context, path string, dst io.Writer, maxBytes int64) (int64, error)
}

// hostAPIClient adapts an agenthost.Client to the ghAPI surface. The PR
// methods carry owner/repo explicitly and forward them; the raw Get /
// DownloadArtifact paths have no owner/repo in their signature, so the
// adapter folds in the owner/repo it was constructed with (resolved once at
// the top of an actions command) for the host's per-repo credential
// resolution.
type hostAPIClient struct {
	host        agenthost.Client
	owner, repo string
}

// newHostAPI builds the adapter. owner/repo are used only by the raw
// Get/DownloadArtifact paths; the typed PR methods pass their own, so the PR
// dispatch can construct it with empty owner/repo.
func newHostAPI(host agenthost.Client, owner, repo string) hostAPIClient {
	return hostAPIClient{host: host, owner: owner, repo: repo}
}

func (a hostAPIClient) GetPR(owner, repo string, number int, verbose bool) (*ghclient.PRView, error) {
	return a.host.GithubGetPR(context.Background(), owner, repo, number, verbose)
}

func (a hostAPIClient) GetPRDiff(owner, repo string, number int, file string) (string, error) {
	return a.host.GithubGetPRDiff(context.Background(), owner, repo, number, file)
}

func (a hostAPIClient) GetPRFiles(owner, repo string, number int) ([]ghclient.PRFile, error) {
	return a.host.GithubGetPRFiles(context.Background(), owner, repo, number)
}

func (a hostAPIClient) GetCommentThread(owner, repo string, commentID, page int) (*ghclient.CommentThread, error) {
	return a.host.GithubGetCommentThread(context.Background(), owner, repo, commentID, page)
}

func (a hostAPIClient) GetReviewDetail(owner, repo string, number, reviewID int, verbose bool) (*ghclient.ReviewDetail, error) {
	return a.host.GithubGetReviewDetail(context.Background(), owner, repo, number, reviewID, verbose)
}

func (a hostAPIClient) DismissReview(owner, repo string, number, reviewID int, message string) error {
	return a.host.GithubDismissReview(context.Background(), owner, repo, number, reviewID, message)
}

func (a hostAPIClient) SubmitReview(owner, repo string, number int, commitSHA, event, body string, comments []ghclient.SubmitReviewComment) (int, string, error) {
	return a.host.GithubSubmitReview(context.Background(), owner, repo, number, commitSHA, event, body, comments)
}

func (a hostAPIClient) CreatePR(owner, repo, head, base, title, body string, draft bool) (int, string, string, error) {
	return a.host.GithubCreatePR(context.Background(), owner, repo, head, base, title, body, draft)
}

func (a hostAPIClient) CreatePendingReview(owner, repo string, number int, commitSHA string, comments []ghclient.SubmitReviewComment) (string, error) {
	return a.host.GithubCreatePendingReview(context.Background(), owner, repo, number, commitSHA, comments)
}

// AddPendingReviewComment carries no owner/repo (it keys off the review node
// id, matching *github.Client), so — like Get/DownloadArtifact — the adapter
// folds in the owner/repo it was constructed with for the host's per-repo
// credential resolution. The shared PR-dispatch adapter is built unscoped
// (newHostAPI(host, "", "")), so a caller of this op must reconstruct a
// repo-scoped adapter (as the actions dispatch does); the guard turns the
// otherwise-confusing "GitHub not configured for /" into an actionable error if
// that's missed.
func (a hostAPIClient) AddPendingReviewComment(reviewID string, comment ghclient.SubmitReviewComment) (string, error) {
	if a.owner == "" || a.repo == "" {
		return "", errors.New("hostAPIClient.AddPendingReviewComment needs a repo-scoped adapter: construct newHostAPI(host, owner, repo) (this op keys off the review node id and has no owner/repo to forward for credential resolution)")
	}
	return a.host.GithubAddPendingReviewComment(context.Background(), a.owner, a.repo, reviewID, comment.Path, comment.Body, comment.Line, comment.StartLine)
}

func (a hostAPIClient) GetPendingReview(owner, repo string, number int) (string, []ghclient.PendingReviewComment, error) {
	return a.host.GithubGetPendingReview(context.Background(), owner, repo, number)
}

func (a hostAPIClient) AddComment(owner, repo string, number int, body string) (int, error) {
	return a.host.GithubAddComment(context.Background(), owner, repo, number, body)
}

func (a hostAPIClient) ReplyToComment(owner, repo string, number, commentID int, body string) (int, error) {
	return a.host.GithubReplyToComment(context.Background(), owner, repo, number, commentID, body)
}

func (a hostAPIClient) ReactToComment(owner, repo string, commentID int, emoji string) error {
	return a.host.GithubReactToComment(context.Background(), owner, repo, commentID, emoji)
}

func (a hostAPIClient) UpdateComment(owner, repo string, commentID int, body string) error {
	return a.host.GithubUpdateComment(context.Background(), owner, repo, commentID, body)
}

func (a hostAPIClient) DeleteComment(owner, repo string, commentID int) error {
	return a.host.GithubDeleteComment(context.Background(), owner, repo, commentID)
}

func (a hostAPIClient) Get(path string) ([]byte, error) {
	return a.host.GithubAPIGet(context.Background(), a.owner, a.repo, path)
}

func (a hostAPIClient) DownloadArtifact(ctx context.Context, path string, dst io.Writer, maxBytes int64) (int64, error) {
	return a.host.GithubDownloadArtifact(ctx, a.owner, a.repo, path, dst, maxBytes)
}
