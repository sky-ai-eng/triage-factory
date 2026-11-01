package agenthost

import (
	"context"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/ghcli/ghwrite"
)

// graphQLWrite is a successful GraphQL comment write as the injector reports
// it for gh's form of the request, whose input is bound to the node id it
// names: the mutation and that node id, plus whatever ids and link the
// response gave.
func graphQLWrite(mutation, subject, externalID, nodeID, url string) ghwrite.Observation {
	return ghwrite.Observation{
		Method: "POST", Path: "/graphql", Status: 200,
		ExternalID: externalID, NodeID: nodeID, URL: url,
		GraphQL: &ghwrite.GraphQLFacts{Fields: []string{mutation}, Subject: subject, SubjectBound: true},
	}
}

// TestGHChannel_ThreadReplyNamesTheComment pins the reply acceptance for both
// raw transports: a reply through `gh api` REST and one through
// `gh api graphql` each leave one comment_posted row naming the reply in the
// review comments' numbering, and no deliverable.
func TestGHChannel_ThreadReplyNamesTheComment(t *testing.T) {
	const replyURL = "https://github.com/octo/repo/pull/42#discussion_r8"
	cases := []struct {
		name string
		obs  ghwrite.Observation
	}{
		{
			name: "REST",
			obs: ghwrite.Observation{
				Method: "POST", Path: "/repos/octo/repo/pulls/42/comments/5/replies", Status: 201,
				ExternalID: "8", NodeID: "PRRC_kwReply", URL: replyURL,
			},
		},
		{
			name: "GraphQL",
			obs:  graphQLWrite("addPullRequestReviewThreadReply", "PRRT_kwThread", "8", "PRRC_kwReply", replyURL),
		},
	}
	for _, tc := range cases {
		for _, eventTriggered := range []bool{true, false} {
			mode := "manual"
			if eventTriggered {
				mode = "event-triggered"
			}
			t.Run(tc.name+"/"+mode, func(t *testing.T) {
				stores, info, client := newGithubRecordingClient(t, "https://github.com", eventTriggered)
				client.RecordGHChannelWrite(context.Background(), tc.obs)

				if arts := listConversationDeliverables(t, stores, info.ConversationID); len(arts) != 0 {
					t.Errorf("deliverables = %+v, want none", arts)
				}
				acts := listExternalActions(t, stores)
				if len(acts) != 1 || acts[0].Action != domain.ActionCommentPosted ||
					acts[0].ObjectKind != domain.ObjectKindComment ||
					acts[0].ObjectKey != domain.GitHubCommentKey(domain.GitHubReviewComment, 8) {
					t.Errorf("audit rows = %+v, want the one comment_posted row naming the reply", acts)
				}
			})
		}
	}
}

// TestGHChannel_CommentLifecycleByNodeID pins a comment posted through the
// channel, then edited and deleted through GraphQL mutations that name it by
// node id alone: each write leaves its own row, the post's naming the comment,
// and the edit's and the delete's naming no object, since a node id is not
// what a comment is named by.
func TestGHChannel_CommentLifecycleByNodeID(t *testing.T) {
	cases := []struct {
		name         string
		post         ghwrite.Observation
		edit, delete string
		space        domain.GitHubCommentSpace
		number       int64
		nodeID       string
	}{
		{
			name:  "conversation comment",
			post:  graphQLWrite("addComment", "PR_kwSubject", "7", "IC_kwComment", "https://github.com/octo/repo/pull/42#issuecomment-7"),
			edit:  "updateIssueComment",
			space: domain.GitHubIssueComment, number: 7, nodeID: "IC_kwComment",
			delete: "deleteIssueComment",
		},
		{
			name:  "inline comment",
			post:  graphQLWrite("addPullRequestReviewThreadReply", "PRRT_kwThread", "8", "PRRC_kwReply", "https://github.com/octo/repo/pull/42#discussion_r8"),
			edit:  "updatePullRequestReviewComment",
			space: domain.GitHubReviewComment, number: 8, nodeID: "PRRC_kwReply",
			delete: "deletePullRequestReviewComment",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stores, info, client := newGithubRecordingClient(t, "https://github.com", true)
			ctx := context.Background()

			client.RecordGHChannelWrite(ctx, tc.post)
			client.RecordGHChannelWrite(ctx, graphQLWrite(tc.edit, tc.nodeID, "", "", ""))
			client.RecordGHChannelWrite(ctx, graphQLWrite(tc.delete, tc.nodeID, "", "", ""))

			if arts := listConversationDeliverables(t, stores, info.ConversationID); len(arts) != 0 {
				t.Errorf("deliverables = %+v, want none", arts)
			}
			acts := listExternalActions(t, stores)
			if len(acts) != 3 {
				t.Fatalf("audit rows = %d, want one per write: %+v", len(acts), acts)
			}
			named := 0
			for _, a := range acts {
				switch {
				case a.ObjectKind == domain.ObjectKindComment && a.ObjectKey == domain.GitHubCommentKey(tc.space, tc.number):
					named++
				case a.ObjectKind != "":
					t.Errorf("row %s names %q %q, want the comment or nothing", a.Action, a.ObjectKind, a.ObjectKey)
				}
			}
			if named != 1 {
				t.Errorf("rows naming the comment = %d, want the post's alone: %+v", named, acts)
			}
		})
	}
}

// TestGHChannel_WritesLeaveNoDeliverable pins that every write through the
// channel leaves its one audit row and no deliverable: a comment post, a
// refused comment write, an edit or a delete of any comment, a reaction to a
// comment, and a non-comment write.
func TestGHChannel_WritesLeaveNoDeliverable(t *testing.T) {
	cases := []struct {
		name string
		obs  ghwrite.Observation
	}{
		{"REST create", ghwrite.Observation{Method: "POST", Path: "/repos/octo/repo/issues/1/comments", Status: 201, ExternalID: "7"}},
		{"refused REST create", ghwrite.Observation{Method: "POST", Path: "/repos/octo/repo/issues/1/comments", Status: 404}},
		{"errored GraphQL create", func() ghwrite.Observation {
			o := graphQLWrite("addComment", "PR_kw", "7", "IC_kw", "https://github.com/octo/repo/pull/1#issuecomment-7")
			o.Errored = true
			return o
		}()},
		{"REST edit of a person's comment", ghwrite.Observation{Method: "PATCH", Path: "/repos/octo/repo/issues/comments/99", Status: 200}},
		{"GraphQL edit of a person's comment", graphQLWrite("updateIssueComment", "IC_kwPerson", "99", "IC_kwPerson", "")},
		{"REST delete of a person's comment", ghwrite.Observation{Method: "DELETE", Path: "/repos/octo/repo/issues/comments/99", Status: 204}},
		{"REST delete of a person's review comment", ghwrite.Observation{Method: "DELETE", Path: "/repos/octo/repo/pulls/comments/99", Status: 204}},
		{"delete naming nothing", graphQLWrite("deleteIssueComment", "", "", "", "")},
		{"reaction to a comment", ghwrite.Observation{Method: "POST", Path: "/repos/octo/repo/issues/comments/5/reactions", Status: 201}},
		{"merge", ghwrite.Observation{Method: "PUT", Path: "/repos/octo/repo/pulls/7/merge", Status: 200}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stores, info, client := newGithubRecordingClient(t, "https://github.com", true)
			client.RecordGHChannelWrite(context.Background(), tc.obs)
			if arts := listConversationDeliverables(t, stores, info.ConversationID); len(arts) != 0 {
				t.Errorf("deliverables = %+v, want none", arts)
			}
			if acts := listExternalActions(t, stores); len(acts) != 1 {
				t.Errorf("audit rows = %+v, want the write's one row", acts)
			}
		})
	}
}
