package domain

import "strconv"

// GitHubCommentSpace names which of GitHub's two comment numberings a comment
// belongs to. A pull request's or issue's conversation comment is addressed at
// /issues/comments/{id}, and a comment on a diff line (a review comment, a
// reply in its thread) at /pulls/comments/{id}. GitHub numbers the two
// separately and does not promise the numbers never collide, so a comment is
// identified by its space and its number together.
type GitHubCommentSpace string

const (
	// GitHubIssueComment is a conversation comment on a pull request or issue.
	GitHubIssueComment GitHubCommentSpace = "issue"
	// GitHubReviewComment is a comment in a review thread on a pull request's
	// diff, a reply included.
	GitHubReviewComment GitHubCommentSpace = "review"
)

// GitHubCommentDedupKey is the key a GitHub comment's `comment` artifact is
// recorded under, from its space and its number: github:comment:<id> for a
// conversation comment, which is the shape stored rows carry, and
// github:comment:review:<id> for a review comment. "" for a space outside the
// two or a number that is not positive, since neither names a comment.
func GitHubCommentDedupKey(space GitHubCommentSpace, id int64) string {
	if id <= 0 {
		return ""
	}
	n := strconv.FormatInt(id, 10)
	switch space {
	case GitHubIssueComment:
		return ArtifactDedupKey(ArtifactProviderGitHub, ArtifactKindComment, n, "")
	case GitHubReviewComment:
		return ArtifactDedupKey(ArtifactProviderGitHub, ArtifactKindComment, string(GitHubReviewComment), n)
	}
	return ""
}
