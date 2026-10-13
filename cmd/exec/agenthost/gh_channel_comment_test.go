package agenthost

import (
	"context"
	"strconv"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/ghwrite"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// graphQLWrite is a successful GraphQL comment write as the injector reports
// it: the mutation and the node id its request named, plus whatever ids and
// link the response gave.
func graphQLWrite(mutation, subject, externalID, nodeID, url string) ghwrite.Observation {
	return ghwrite.Observation{
		Method: "POST", Path: "/graphql", Status: 200,
		ExternalID: externalID, NodeID: nodeID, URL: url,
		GraphQL: &ghwrite.GraphQLFacts{Fields: []string{mutation}, Subject: subject},
	}
}

// onlyArtifact returns the conversation's one artifact, failing on any other
// count.
func onlyArtifact(t *testing.T, stores db.Stores, conversationID string) domain.Artifact {
	t.Helper()
	arts := listConversationArtifacts(t, stores, conversationID)
	if len(arts) != 1 {
		t.Fatalf("want 1 artifact, got %d: %+v", len(arts), arts)
	}
	return arts[0]
}

// postedByTF reports whether the posted-by-TF lookup names a's key.
func postedByTF(t *testing.T, stores db.Stores, a domain.Artifact) bool {
	t.Helper()
	got, err := stores.Artifacts.PostedByTFDedupKeysSystem(context.Background(), runmode.LocalDefaultOrgID, a.Scope, []string{a.DedupKey})
	if err != nil {
		t.Fatalf("PostedByTFDedupKeysSystem: %v", err)
	}
	return got[a.DedupKey]
}

// TestGHChannel_ThreadReplyRecordsCommentArtifact pins the reply acceptance for
// both raw transports: a reply through `gh api` REST and one through
// `gh api graphql` each leave a `comment` artifact in the review comments'
// numbering, carrying the reply's number and node id and posted by TF.
func TestGHChannel_ThreadReplyRecordsCommentArtifact(t *testing.T) {
	const replyURL = "https://github.com/octo/repo/pull/42#discussion_r8"
	cases := []struct {
		name        string
		obs         ghwrite.Observation
		wantDetails string
	}{
		{
			name: "REST",
			obs: ghwrite.Observation{
				Method: "POST", Path: "/repos/octo/repo/pulls/42/comments/5/replies", Status: 201,
				ExternalID: "8", NodeID: "PRRC_kwReply", URL: replyURL,
			},
			wantDetails: `{"in_reply_to":5}`,
		},
		{
			// The request names the thread by node id, which is not a comment,
			// so the details carry no in_reply_to.
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

				a := onlyArtifact(t, stores, info.ConversationID)
				if a.Provider != domain.ArtifactProviderGitHub || a.Kind != domain.ArtifactKindComment ||
					a.DedupKey != domain.GitHubCommentDedupKey(domain.GitHubReviewComment, 8) ||
					a.ExternalID != "8" || a.NodeID != "PRRC_kwReply" || a.Target != "octo/repo#42" ||
					a.URL != replyURL || a.State != domain.ArtifactStateCommentPosted || !a.PostedByTF ||
					a.DetailsJSON != tc.wantDetails {
					t.Errorf("reply artifact = %+v", a)
				}
				if acts := listExternalActions(t, stores); len(acts) != 1 || acts[0].Action != domain.ActionCommentPosted {
					t.Errorf("audit rows = %+v, want the one comment_posted row beside the artifact", acts)
				}
			})
		}
	}
}

// TestGHChannel_CommentLifecycleByNodeID pins the edit and delete acceptance:
// a comment posted through the channel, then edited and deleted through
// GraphQL mutations that name it by node id alone, is one artifact that ends
// deleted and still posted by TF. The conversation comment is `gh pr comment`'s
// shape — its number off the url's anchor, its node id read back by the
// injector — and the inline comment is a thread reply.
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
			posted := onlyArtifact(t, stores, info.ConversationID)
			number := strconv.FormatInt(tc.number, 10)
			if posted.ExternalID != number || posted.NodeID != tc.nodeID || !posted.PostedByTF {
				t.Fatalf("posted artifact = %+v, want both ids and posted by TF", posted)
			}

			// Edit and delete name the comment by node id only, and their
			// responses return no number.
			client.RecordGHChannelWrite(ctx, graphQLWrite(tc.edit, tc.nodeID, "", "", ""))
			edited := onlyArtifact(t, stores, info.ConversationID)
			if edited.ID != posted.ID || edited.State != domain.ArtifactStateCommentPosted {
				t.Errorf("after the edit = %+v, want row %s still posted", edited, posted.ID)
			}

			client.RecordGHChannelWrite(ctx, graphQLWrite(tc.delete, tc.nodeID, "", "", ""))
			deleted := onlyArtifact(t, stores, info.ConversationID)
			if deleted.ID != posted.ID || deleted.State != domain.ArtifactStateCommentDeleted ||
				deleted.DedupKey != domain.GitHubCommentDedupKey(tc.space, tc.number) ||
				deleted.ExternalID != number || deleted.NodeID != tc.nodeID || deleted.Target != "octo/repo#42" {
				t.Errorf("after the delete = %+v, want row %s deleted with its coordinates kept", deleted, posted.ID)
			}
			if !postedByTF(t, stores, deleted) {
				t.Errorf("posted-by-TF lookup left out %s, want the comment TF posted named", deleted.DedupKey)
			}
			if acts := listExternalActions(t, stores); len(acts) != 3 {
				t.Errorf("audit rows = %d, want one per write: %+v", len(acts), acts)
			}
		})
	}
}

// TestGHChannel_DeleteByNodeIDFindsTheVerbsRow pins the two doors meeting on
// one row: a comment the exec verb posted, which stored the node id from its
// REST response, is retired by a GraphQL delete naming only that node id.
func TestGHChannel_DeleteByNodeIDFindsTheVerbsRow(t *testing.T) {
	gh := startFakeGitHubComments(t)
	stores, info, client := newGithubRecordingClient(t, gh.URL, true)
	ctx := context.Background()

	if _, err := client.GithubAddComment(ctx, "octo", "repo", 1, "posted through the verb"); err != nil {
		t.Fatalf("GithubAddComment: %v", err)
	}
	client.RecordGHChannelWrite(ctx, graphQLWrite("deleteIssueComment", "IC_kw777", "", "", ""))

	a := onlyArtifact(t, stores, info.ConversationID)
	if a.DedupKey != "github:comment:777" || a.State != domain.ArtifactStateCommentDeleted || !a.PostedByTF {
		t.Errorf("artifact = %+v, want the verb's row retired", a)
	}
}

// TestGHChannel_PersonsCommentIsNotPostedByTF pins that an agent editing a
// person's comment through the channel leaves a row recording the edit, and
// that the posted-by-TF lookup does not return it.
func TestGHChannel_PersonsCommentIsNotPostedByTF(t *testing.T) {
	stores, info, client := newGithubRecordingClient(t, "https://github.com", true)
	client.RecordGHChannelWrite(context.Background(), ghwrite.Observation{
		Method: "PATCH", Path: "/repos/octo/repo/issues/comments/99", Status: 200,
	})

	a := onlyArtifact(t, stores, info.ConversationID)
	if a.DedupKey != "github:comment:99" || a.ExternalID != "99" || a.PostedByTF || a.Target != "" {
		t.Errorf("artifact = %+v, want a row for comment 99, not posted by TF and with no repo-level target", a)
	}
	if postedByTF(t, stores, a) {
		t.Error("posted-by-TF lookup named a person's comment the run edited")
	}
}

// TestGHChannel_NoCommentArtifact pins the writes that leave only their audit
// row: a refused comment write, a node id no row carries, a reaction to a
// comment, and a non-comment write.
func TestGHChannel_NoCommentArtifact(t *testing.T) {
	cases := []struct {
		name string
		obs  ghwrite.Observation
	}{
		{"refused REST create", ghwrite.Observation{Method: "POST", Path: "/repos/octo/repo/issues/1/comments", Status: 404}},
		{"errored GraphQL create", func() ghwrite.Observation {
			o := graphQLWrite("addComment", "PR_kw", "7", "IC_kw", "https://github.com/octo/repo/pull/1#issuecomment-7")
			o.Errored = true
			return o
		}()},
		{"delete of a node id no row carries", graphQLWrite("deleteIssueComment", "IC_kwUnknown", "", "", "")},
		{"delete naming nothing", graphQLWrite("deleteIssueComment", "", "", "", "")},
		{"reaction to a comment", ghwrite.Observation{Method: "POST", Path: "/repos/octo/repo/issues/comments/5/reactions", Status: 201}},
		{"merge", ghwrite.Observation{Method: "PUT", Path: "/repos/octo/repo/pulls/7/merge", Status: 200}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stores, info, client := newGithubRecordingClient(t, "https://github.com", true)
			client.RecordGHChannelWrite(context.Background(), tc.obs)
			if arts := listConversationArtifacts(t, stores, info.ConversationID); len(arts) != 0 {
				t.Errorf("artifacts = %+v, want none", arts)
			}
			if acts := listExternalActions(t, stores); len(acts) != 1 {
				t.Errorf("audit rows = %+v, want the write's one row", acts)
			}
		})
	}
}
