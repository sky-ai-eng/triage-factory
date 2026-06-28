package gh

import (
	"context"
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
	GetPR(ctx context.Context, owner, repo string, number int, verbose bool) (*ghclient.PRView, error)
	GetPRDiff(ctx context.Context, owner, repo string, number int, file string) (string, error)
	GetPRFiles(ctx context.Context, owner, repo string, number int) ([]ghclient.PRFile, error)
	GetCommentThread(ctx context.Context, owner, repo string, commentID, page int) (*ghclient.CommentThread, error)
	GetReviewDetail(ctx context.Context, owner, repo string, number, reviewID int, verbose bool) (*ghclient.ReviewDetail, error)
	DismissReview(ctx context.Context, owner, repo string, number, reviewID int, message string) error
	SubmitReview(ctx context.Context, owner, repo string, number int, commitSHA, event, body string, comments []ghclient.SubmitReviewComment) (int, string, error)
	CreatePR(ctx context.Context, owner, repo, head, base, title, body string, draft bool) (int, string, string, error)
	AddComment(ctx context.Context, owner, repo string, number int, body string) (int, error)
	ReplyToComment(ctx context.Context, owner, repo string, number, commentID int, body string) (int, error)
	ReactToComment(ctx context.Context, owner, repo string, commentID int, emoji string) error
	UpdateComment(ctx context.Context, owner, repo string, commentID int, body string) error
	DeleteComment(ctx context.Context, owner, repo string, commentID int) error

	// Get / DownloadArtifact are the raw transport the actions verbs lean on
	// (workflow-run + job listings and log archives have no typed method).
	// They carry no owner/repo, so the host-backed adapter folds in the repo
	// it was constructed with for the credential-tier resolution.
	Get(ctx context.Context, path string) ([]byte, error)
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

func (a hostAPIClient) GetPR(ctx context.Context, owner, repo string, number int, verbose bool) (*ghclient.PRView, error) {
	return a.host.GithubGetPR(ctx, owner, repo, number, verbose)
}

func (a hostAPIClient) GetPRDiff(ctx context.Context, owner, repo string, number int, file string) (string, error) {
	return a.host.GithubGetPRDiff(ctx, owner, repo, number, file)
}

func (a hostAPIClient) GetPRFiles(ctx context.Context, owner, repo string, number int) ([]ghclient.PRFile, error) {
	return a.host.GithubGetPRFiles(ctx, owner, repo, number)
}

func (a hostAPIClient) GetCommentThread(ctx context.Context, owner, repo string, commentID, page int) (*ghclient.CommentThread, error) {
	return a.host.GithubGetCommentThread(ctx, owner, repo, commentID, page)
}

func (a hostAPIClient) GetReviewDetail(ctx context.Context, owner, repo string, number, reviewID int, verbose bool) (*ghclient.ReviewDetail, error) {
	return a.host.GithubGetReviewDetail(ctx, owner, repo, number, reviewID, verbose)
}

func (a hostAPIClient) DismissReview(ctx context.Context, owner, repo string, number, reviewID int, message string) error {
	return a.host.GithubDismissReview(ctx, owner, repo, number, reviewID, message)
}

func (a hostAPIClient) SubmitReview(ctx context.Context, owner, repo string, number int, commitSHA, event, body string, comments []ghclient.SubmitReviewComment) (int, string, error) {
	return a.host.GithubSubmitReview(ctx, owner, repo, number, commitSHA, event, body, comments)
}

func (a hostAPIClient) CreatePR(ctx context.Context, owner, repo, head, base, title, body string, draft bool) (int, string, string, error) {
	return a.host.GithubCreatePR(ctx, owner, repo, head, base, title, body, draft)
}

func (a hostAPIClient) AddComment(ctx context.Context, owner, repo string, number int, body string) (int, error) {
	return a.host.GithubAddComment(ctx, owner, repo, number, body)
}

func (a hostAPIClient) ReplyToComment(ctx context.Context, owner, repo string, number, commentID int, body string) (int, error) {
	return a.host.GithubReplyToComment(ctx, owner, repo, number, commentID, body)
}

func (a hostAPIClient) ReactToComment(ctx context.Context, owner, repo string, commentID int, emoji string) error {
	return a.host.GithubReactToComment(ctx, owner, repo, commentID, emoji)
}

func (a hostAPIClient) UpdateComment(ctx context.Context, owner, repo string, commentID int, body string) error {
	return a.host.GithubUpdateComment(ctx, owner, repo, commentID, body)
}

func (a hostAPIClient) DeleteComment(ctx context.Context, owner, repo string, commentID int) error {
	return a.host.GithubDeleteComment(ctx, owner, repo, commentID)
}

func (a hostAPIClient) Get(ctx context.Context, path string) ([]byte, error) {
	return a.host.GithubAPIGet(ctx, a.owner, a.repo, path)
}

func (a hostAPIClient) DownloadArtifact(ctx context.Context, path string, dst io.Writer, maxBytes int64) (int64, error) {
	return a.host.GithubDownloadArtifact(ctx, a.owner, a.repo, path, dst, maxBytes)
}
