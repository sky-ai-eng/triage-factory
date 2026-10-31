package agenthost

import (
	"context"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/ghwrite"
	jiraclient "github.com/sky-ai-eng/triage-factory/internal/jira"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestRecorders_NameTheObjectALandedChangeChanged pins that a row recorded for
// a landed change names the object it changed, and that domain.ObjectID
// renders an identity for it, while a refusal names none. Each case makes one
// write through the recorder that ships it and returns the stores it recorded
// into and the identity the row must carry ("" for none).
func TestRecorders_NameTheObjectALandedChangeChanged(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name   string
		action string
		record func(t *testing.T) (db.Stores, string)
	}{
		{
			name:   "branch push",
			action: domain.ActionBranchPushed,
			record: func(t *testing.T) (db.Stores, string) {
				stores, info := newCaptureStores(t, false)
				repoID := registerRepo(t, stores, "octo", "repo", "1")
				branch, _ := domain.NewBranchDeliverable("octo/repo", "refs/heads/feat", "abc123", true)
				if _, err := NewLocal(stores, info).UpsertDeliverable(ctx, branch); err != nil {
					t.Fatalf("UpsertDeliverable: %v", err)
				}
				return stores, "branch|" + repoID + "|refs/heads/feat"
			},
		},
		{
			name:   "pull request create",
			action: domain.ActionPRCreated,
			record: func(t *testing.T) (db.Stores, string) {
				gh, _ := startFakeGitHubPRCreate(t)
				stores, _, client := newGithubRecordingClient(t, gh.URL, true)
				if _, _, _, err := client.GithubCreatePR(ctx, "octo", "repo", "feature/x", "main", "Fix", "Body", true); err != nil {
					t.Fatalf("GithubCreatePR: %v", err)
				}
				pr, err := stores.Entities.GetBySourceSystem(ctx, runmode.LocalDefaultOrgID, "github", testScope("github"), "octo/repo#42")
				if err != nil || pr == nil {
					t.Fatalf("the pull request's entity: %v, %v", pr, err)
				}
				return stores, "pull_request|" + pr.ID
			},
		},
		{
			name:   "gh channel review-thread reply",
			action: domain.ActionCommentPosted,
			record: func(t *testing.T) (db.Stores, string) {
				gh := startFakeGitHubComments(t)
				stores, _, client := newGithubRecordingClient(t, gh.URL, true)
				client.RecordGHChannelWrite(ctx, ghwrite.Observation{
					Method:     "POST",
					Path:       "/repos/octo/repo/pulls/1/comments/555/replies",
					Status:     201,
					ExternalID: "777",
					URL:        gh.URL + "/octo/repo/pull/1#discussion_r777",
				})
				return stores, "comment|" + testScope("github") + "|" + domain.GitHubCommentKey(domain.GitHubReviewComment, 777)
			},
		},
		{
			// The transition's row recorded alone: its issue's entity comes
			// from the row's own object key.
			name:   "Jira transition with no deliverable",
			action: domain.ActionIssueTransitioned,
			record: func(t *testing.T) (db.Stores, string) {
				stores, info := newCaptureStores(t, true)
				site := testScope("jira")
				key := domain.IssueObjectKey(domain.ProviderJira, site, "10042")
				act := jiraAction(domain.Deliverable{
					Kind:       domain.DeliverableKindIssue,
					Target:     "SKY-42",
					ExternalID: "10042",
					DedupKey:   key,
				}, domain.ActionIssueTransitioned, "", "Done")
				recordExternalWrite(ctx, stores, info, nil, act,
					jiraContainer(jiraclient.Project{ID: "10000", Key: "SKY", Name: "Sky"}))

				if got := listConversationDeliverables(t, stores, info.ConversationID); len(got) != 0 {
					t.Fatalf("the action was recorded with deliverables %+v", got)
				}
				issue, err := stores.Entities.GetByExternalIDSystem(ctx, runmode.LocalDefaultOrgID, "jira", site, "10042")
				if err != nil || issue == nil {
					t.Fatalf("the issue's entity: %v, %v", issue, err)
				}
				for _, a := range listExternalActions(t, stores) {
					if a.EntityID != issue.ID {
						t.Errorf("the transition links entity %q, want the issue's %s", a.EntityID, issue.ID)
					}
				}
				return stores, "issue|" + site + "|" + key
			},
		},
		{
			name:   "Linear comment",
			action: domain.ActionIssueCommentPosted,
			record: func(t *testing.T) (db.Stores, string) {
				_, srv := startFakeLinear(t)
				lc, stores, _ := linearProxyClient(t, srv.URL, true)
				if _, err := lc.LinearAddComment(ctx, "ENG-7", "on it"); err != nil {
					t.Fatalf("LinearAddComment: %v", err)
				}
				key := domain.ObjectKey(domain.ProviderLinear, domain.DeliverableKindComment, fakeLinearIssueUUID, fakeLinearCommentID)
				return stores, "comment|linear-workspace-uuid|" + key
			},
		},
		{
			name:   "git denial",
			action: domain.ActionGitDenied,
			record: func(t *testing.T) (db.Stores, string) {
				stores, info := newCaptureStores(t, true)
				NewLocal(stores, info).RecordGitDenied(ctx, "octo", "repo", "refs/heads/main", "push", "off_ref")
				return stores, ""
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			stores, want := c.record(t)
			var rows []domain.ExternalAction
			for _, a := range listExternalActions(t, stores) {
				if a.Action == c.action {
					rows = append(rows, a)
				}
			}
			if len(rows) != 1 {
				t.Fatalf("want one %s row, got %+v", c.action, rows)
			}
			got, ok := domain.ObjectID(rows[0])
			if want == "" {
				if ok || rows[0].ObjectKind != "" || rows[0].ObjectKey != "" {
					t.Errorf("the row names object (%q, %q) = %q, want none", rows[0].ObjectKind, rows[0].ObjectKey, got)
				}
				return
			}
			if !ok || got != want {
				t.Errorf("ObjectID = (%q, %v), want %q (row %+v)", got, ok, want, rows[0])
			}
		})
	}
}
