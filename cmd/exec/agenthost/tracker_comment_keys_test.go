package agenthost

import (
	"context"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/ghcli/ghwrite"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestRecordedComments_AreFoundUnderTheTrackersKeys pins what the tracker's
// check for TF's own comments rests on: a comment posted through each recorder
// leaves a ledger row that PresentPostedCommentKeysSystem finds under the
// scope the poll reads the org at (domain.EntityScope over its settings) and
// the key the tracker spells from the comment's id and its issue's. Each case
// posts one comment and returns the stores it recorded into and that key,
// given the scope.
func TestRecordedComments_AreFoundUnderTheTrackersKeys(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name   string
		source string
		post   func(t *testing.T) (db.Stores, func(scope string) string)
	}{
		{
			name:   "github conversation comment",
			source: "github",
			post: func(t *testing.T) (db.Stores, func(string) string) {
				gh := startFakeGitHubComments(t)
				stores, _, client := newGithubRecordingClient(t, gh.URL, true)
				id, err := client.GithubAddComment(ctx, "octo", "repo", 1, "looks good to me")
				if err != nil {
					t.Fatalf("GithubAddComment: %v", err)
				}
				return stores, func(string) string { return domain.GitHubCommentKey(domain.GitHubIssueComment, int64(id)) }
			},
		},
		{
			name:   "github review-thread reply",
			source: "github",
			post: func(t *testing.T) (db.Stores, func(string) string) {
				gh := startFakeGitHubComments(t)
				stores, _, client := newGithubRecordingClient(t, gh.URL, true)
				id, err := client.GithubReplyToComment(ctx, "octo", "repo", 1, 555, "fixed in the next push")
				if err != nil {
					t.Fatalf("GithubReplyToComment: %v", err)
				}
				return stores, func(string) string { return domain.GitHubCommentKey(domain.GitHubReviewComment, int64(id)) }
			},
		},
		{
			name:   "gh channel review-thread reply",
			source: "github",
			post: func(t *testing.T) (db.Stores, func(string) string) {
				gh := startFakeGitHubComments(t)
				stores, _, client := newGithubRecordingClient(t, gh.URL, true)
				client.RecordGHChannelWrite(ctx, ghwrite.Observation{
					Method:     "POST",
					Path:       "/repos/octo/repo/pulls/1/comments/555/replies",
					Status:     201,
					ExternalID: "777",
					URL:        gh.URL + "/octo/repo/pull/1#discussion_r777",
				})
				return stores, func(string) string { return domain.GitHubCommentKey(domain.GitHubReviewComment, 777) }
			},
		},
		{
			name:   "jira comment",
			source: "jira",
			post: func(t *testing.T) (db.Stores, func(string) string) {
				jira := startFakeJira(t)
				stores, info := newJiraRecordingStores(t, jira.URL, true)
				if err := NewLocal(stores, info).JiraAddComment(ctx, "SKY-5", "done and dusted"); err != nil {
					t.Fatalf("JiraAddComment: %v", err)
				}
				return stores, func(scope string) string {
					return domain.ObjectKey(domain.ProviderJira, domain.ObjectKindComment,
						domain.EntityResource(domain.ProviderJira, scope, "10005"), "10042")
				}
			},
		},
		{
			name:   "linear comment",
			source: "linear",
			post: func(t *testing.T) (db.Stores, func(string) string) {
				_, srv := startFakeLinear(t)
				client, stores, _ := linearProxyClient(t, srv.URL, true)
				if _, err := client.LinearAddComment(ctx, "ENG-7", "on it"); err != nil {
					t.Fatalf("LinearAddComment: %v", err)
				}
				return stores, func(string) string {
					return domain.ObjectKey(domain.ProviderLinear, domain.ObjectKindComment, fakeLinearIssueUUID, fakeLinearCommentID)
				}
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			stores, keyFor := c.post(t)
			settings, err := stores.Orgs.GetSettingsSystem(ctx, runmode.LocalDefaultOrgID)
			if err != nil {
				t.Fatalf("GetSettingsSystem: %v", err)
			}
			scope := domain.EntityScope(c.source, settings)
			key := keyFor(scope)
			present, err := stores.ExternalActions.PresentPostedCommentKeysSystem(ctx, runmode.LocalDefaultOrgID, scope, []string{key})
			if err != nil {
				t.Fatalf("PresentPostedCommentKeysSystem: %v", err)
			}
			if !present[key] {
				t.Errorf("no posted comment under scope %q and key %q; ledger holds %+v", scope, key, listExternalActions(t, stores))
			}
		})
	}
}
