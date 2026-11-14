package agenthost

import (
	"context"
	"database/sql"
	"testing"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/ghcli/ghwrite"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// countRows counts the rows of table, for checks that a read created nothing.
func countRows(t *testing.T, conn *sql.DB, table string) int {
	t.Helper()
	var n int
	if err := conn.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

// seedGitHubEntity gives the org an entity for the pull request or issue key,
// carrying nodeID, and returns its id.
func seedGitHubEntity(t *testing.T, stores db.Stores, key, nodeID, kind string) string {
	t.Helper()
	ent, _, err := stores.Entities.FindOrCreateSystem(context.Background(), runmode.LocalDefaultOrgID, "github", testScope("github"), key, nodeID, kind, "", "")
	if err != nil {
		t.Fatalf("seed %s: %v", key, err)
	}
	return ent.ID
}

// TestRecordGHChannelRead_NodeIDTouchesOnlyWhatTFHolds: a read that named its
// object by node id alone touches the pull request or issue TF holds under
// that id, and a node id TF holds nothing for creates no entity and no link.
func TestRecordGHChannelRead_NodeIDTouchesOnlyWhatTFHolds(t *testing.T) {
	conn, stores, info := newCaptureStoresConn(t, true)
	ctx := context.Background()
	pr := seedGitHubEntity(t, stores, "octo/repo#7", "PR_7", domain.EntityKindPullRequest)
	issue := seedGitHubEntity(t, stores, "octo/repo#9", "I_9", domain.EntityKindIssue)

	for _, id := range []string{"PR_7", "I_9", "PR_7", "PR_unknown", "IC_unknown"} {
		RecordGHChannelRead(ctx, stores, info, ghwrite.NodeRead(id))
	}

	for name, ent := range map[string]string{"pull request": pr, "issue": issue} {
		if role := touchRole(t, conn, info.ConversationID, ent); role != domain.MemoryRoleTouched {
			t.Errorf("%s: link role = %q, want %q", name, role, domain.MemoryRoleTouched)
		}
	}
	if n := countRows(t, conn, "conversation_memory_entities"); n != 2 {
		t.Errorf("links = %d, want one per held object", n)
	}
	if n := countRows(t, conn, "entities"); n != 2 {
		t.Errorf("entities = %d, want the two seeded and none from a node id TF does not hold", n)
	}
	if acts := listExternalActions(t, stores); len(acts) != 0 {
		t.Errorf("a read left ledger rows %+v, want none", acts)
	}
}

// TestRecordedComments_AreFoundByTheirNodeID: a comment posted through each
// recorder leaves its node id on its ledger row, and a later read of the
// comment by node id, from another conversation, touches the pull request
// the comment is on.
func TestRecordedComments_AreFoundByTheirNodeID(t *testing.T) {
	ctx := context.Background()
	onPR1 := domain.GitHubIdentity{RepositoryID: testRepoID("octo/repo"), Repository: "octo/repo", NodeID: testPRNodeID(1), Kind: domain.EntityKindPullRequest}
	restPost := ghwrite.Observation{
		Method: "POST", Path: "/repos/octo/repo/issues/1/comments", Status: 201,
		ExternalID: "501", NodeID: "IC_kw501", URL: "https://github.com/octo/repo/pull/1#issuecomment-501", Identity: onPR1,
	}
	graphQLPost := graphQLWrite("addComment", testPRNodeID(1), "502", "IC_kw502", "https://github.com/octo/repo/pull/1#issuecomment-502")
	graphQLPost.Identity = onPR1
	cases := []struct {
		name   string
		nodeID string
		post   func(t *testing.T, client *LocalClient)
	}{
		{"exec conversation comment", "IC_kw777", func(t *testing.T, client *LocalClient) {
			if _, err := client.GithubAddComment(ctx, "octo", "repo", 1, "looks good to me"); err != nil {
				t.Fatalf("GithubAddComment: %v", err)
			}
		}},
		{"exec review-thread reply", "PRRC_kw777", func(t *testing.T, client *LocalClient) {
			if _, err := client.GithubReplyToComment(ctx, "octo", "repo", 1, 555, "fixed in the next push"); err != nil {
				t.Fatalf("GithubReplyToComment: %v", err)
			}
		}},
		{"gh channel REST comment", "IC_kw501", func(t *testing.T, client *LocalClient) {
			client.RecordGHChannelWrite(ctx, restPost)
		}},
		{"gh channel GraphQL comment", "IC_kw502", func(t *testing.T, client *LocalClient) {
			client.RecordGHChannelWrite(ctx, graphQLPost)
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gh := startFakeGitHubComments(t)
			conn, stores, info, client := newGithubRecordingClientConn(t, gh.URL, true)
			c.post(t, client)

			acts := listExternalActions(t, stores)
			if len(acts) != 1 || acts[0].Action != domain.ActionCommentPosted || acts[0].NodeID != c.nodeID || acts[0].EntityID == "" {
				t.Fatalf("audit rows = %+v, want one comment_posted row carrying node id %q and its entity", acts, c.nodeID)
			}

			reader := otherConversation(t, conn, info)
			RecordGHChannelRead(ctx, stores, reader, ghwrite.NodeRead(c.nodeID))
			if role := touchRole(t, conn, reader.ConversationID, acts[0].EntityID); role != domain.MemoryRoleTouched {
				t.Errorf("link role on the commented pull request = %q, want %q", role, domain.MemoryRoleTouched)
			}
		})
	}
}

// TestRecordGHChannelRead_CommentNodeIDOnlyAnEditCarries: a comment TF did not
// post has no row that carries its node id, an edit's included, so a read of
// it by node id touches nothing.
func TestRecordGHChannelRead_CommentNodeIDOnlyAnEditCarries(t *testing.T) {
	gh := startFakeGitHubComments(t)
	conn, stores, info, client := newGithubRecordingClientConn(t, gh.URL, true)
	ctx := context.Background()
	edit := graphQLWrite("updateIssueComment", "IC_kwPersons", "", "", "https://github.com/octo/repo/pull/1#issuecomment-9")
	edit.Identity = domain.GitHubIdentity{RepositoryID: testRepoID("octo/repo"), Repository: "octo/repo", NodeID: testPRNodeID(1), Kind: domain.EntityKindPullRequest}
	client.RecordGHChannelWrite(ctx, edit)
	acts := listExternalActions(t, stores)
	if len(acts) != 1 || acts[0].NodeID != "" {
		t.Fatalf("audit rows = %+v, want the edit's one row with no node id", acts)
	}

	reader := otherConversation(t, conn, info)
	RecordGHChannelRead(ctx, stores, reader, ghwrite.NodeRead("IC_kwPersons"))
	if n := countRows(t, conn, "conversation_memory_entities WHERE conversation_id = '"+reader.ConversationID+"'"); n != 0 {
		t.Errorf("links = %d, want none from a comment TF did not post", n)
	}
}

// otherConversation is a second conversation in info's org, for a read made
// apart from the conversation that posted a comment, whose own link to the
// pull request is produced.
func otherConversation(t *testing.T, conn *sql.DB, info ConversationInfo) ConversationInfo {
	t.Helper()
	id := uuid.NewString()
	if _, err := conn.Exec(`INSERT INTO conversations (id, origin, status) VALUES (?, 'interactive', 'running')`, id); err != nil {
		t.Fatalf("seed the reading conversation: %v", err)
	}
	info.ConversationID = id
	return info
}

// TestRelay_GHChannelRead_NodeIDPassesTheReadGate: in multi mode the node id
// was relayed, so a read of it links only an entity in a repository the run's
// credential reads, the gate a read by number passes, whether the node id
// names the pull request itself or a comment TF posted on it.
func TestRelay_GHChannelRead_NodeIDPassesTheReadGate(t *testing.T) {
	conn, stores, info := newCaptureStoresConn(t, true)
	runmode.SetForTest(t, runmode.ModeMulti)
	ctx := context.Background()
	trackedID := registerRepo(t, stores, "octo", "repo", "1")
	registerRepo(t, stores, "octo", "untracked", "2")
	trackRepos(t, stores, info, trackedID)
	tracked7 := seedGitHubEntity(t, stores, "octo/repo#7", "PR_7", domain.EntityKindPullRequest)
	tracked8 := seedGitHubEntity(t, stores, "octo/repo#8", "PR_8", domain.EntityKindPullRequest)
	untracked4 := seedGitHubEntity(t, stores, "octo/untracked#4", "PR_4", domain.EntityKindPullRequest)
	untracked5 := seedGitHubEntity(t, stores, "octo/untracked#5", "PR_5", domain.EntityKindPullRequest)
	for nodeID, entityID := range map[string]string{"IC_on8": tracked8, "IC_on5": untracked5} {
		act := domain.ExternalAction{
			ID: uuid.NewString(), TeamID: info.TeamID, Provider: domain.ProviderGitHub, Scope: testScope("github"),
			Action: domain.ActionCommentPosted, Target: "octo/repo#8", Credential: domain.CredentialGitHubApp,
			PerformedBy: domain.PerformedByAgent, NodeID: nodeID, EntityID: entityID,
		}
		act.SetObject(domain.ObjectKindComment, domain.GitHubCommentKey(domain.GitHubIssueComment, int64(len(entityID))))
		if err := stores.ExternalActions.RecordSystem(ctx, info.OrgID, act); err != nil {
			t.Fatalf("record the comment %s: %v", nodeID, err)
		}
	}
	srv := NewRelayServer(stores, info, nil)

	for _, id := range []string{"PR_7", "IC_on8", "PR_4", "IC_on5"} {
		relayGHRead(t, srv, ghwrite.NodeRead(id))
	}
	for name, ent := range map[string]string{"the pull request": tracked7, "the commented pull request": tracked8} {
		if role := touchRole(t, conn, info.ConversationID, ent); role != domain.MemoryRoleTouched {
			t.Errorf("%s in a repository the run's team tracks: link role = %q, want %q", name, role, domain.MemoryRoleTouched)
		}
	}
	for name, ent := range map[string]string{"the pull request": untracked4, "the commented pull request": untracked5} {
		if role := touchRole(t, conn, info.ConversationID, ent); role != "" {
			t.Errorf("%s in a repository the run's credential does not read was linked %q", name, role)
		}
	}
}
