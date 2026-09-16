package events

import "github.com/sky-ai-eng/triage-factory/internal/domain"

// Body update metadata identifies source revisions without copying complete
// bodies into the append-only event log. Author/assignee identify ownership,
// not the editor: snapshot polling does not establish who made the edit.
type GitHubPRBodyUpdatedMetadata struct {
	Author           string   `json:"author"`
	Repo             string   `json:"repo"`
	PRNumber         int      `json:"pr_number"`
	IsDraft          bool     `json:"is_draft"`
	HeadSHA          string   `json:"head_sha"`
	Labels           []string `json:"labels"`
	Title            string   `json:"title"`
	PreviousBodyHash string   `json:"previous_body_hash"`
	BodyHash         string   `json:"body_hash"`
}

type GitHubPRBodyUpdatedPredicate struct {
	AuthorIn []string `json:"author_in,omitempty" doc:"Match PR authors by GitHub login."`
	Author   *string  `json:"author,omitempty"`
	Repo     *string  `json:"repo,omitempty"`
	IsDraft  *bool    `json:"is_draft,omitempty"`
	HasLabel *string  `json:"has_label,omitempty"`
}

func (p GitHubPRBodyUpdatedPredicate) Matches(m GitHubPRBodyUpdatedMetadata) bool {
	return stringInSliceFold(p.AuthorIn, m.Author) &&
		strEq(p.Author, m.Author) && strEq(p.Repo, m.Repo) &&
		boolEq(p.IsDraft, m.IsDraft) && hasLabel(p.HasLabel, m.Labels)
}

type JiraIssueBodyUpdatedMetadata struct {
	Assignee          string   `json:"assignee"`
	AssigneeAccountID string   `json:"assignee_account_id"`
	IssueKey          string   `json:"issue_key"`
	Project           string   `json:"project"`
	IssueType         string   `json:"issue_type"`
	Summary           string   `json:"summary"`
	Labels            []string `json:"labels"`
	PreviousBodyHash  string   `json:"previous_body_hash"`
	BodyHash          string   `json:"body_hash"`
}

type JiraIssueBodyUpdatedPredicate struct {
	AssigneeIn []string `json:"assignee_in,omitempty" doc:"Match assignees by Atlassian account ID."`
	Project    *string  `json:"project,omitempty"`
	IssueType  *string  `json:"issue_type,omitempty"`
	HasLabel   *string  `json:"has_label,omitempty"`
}

func (p JiraIssueBodyUpdatedPredicate) Matches(m JiraIssueBodyUpdatedMetadata) bool {
	return stringInSliceFold(p.AssigneeIn, m.AssigneeAccountID) &&
		strEq(p.Project, m.Project) && strEq(p.IssueType, m.IssueType) &&
		hasLabel(p.HasLabel, m.Labels)
}

func init() {
	Register(NewSchema[GitHubPRBodyUpdatedMetadata, GitHubPRBodyUpdatedPredicate](domain.EventGitHubPRBodyUpdated, OwnershipOwned))
	Register(NewSchema[JiraIssueBodyUpdatedMetadata, JiraIssueBodyUpdatedPredicate](domain.EventJiraIssueBodyUpdated, OwnershipOwned))
}
