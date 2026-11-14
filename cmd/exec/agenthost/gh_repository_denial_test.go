package agenthost

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbseed"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// A relayed refusal of the gh channel's repository gate leaves the git_denied
// row the exec verbs' repository gate writes, with the reason that gate would
// give, decided from this side's tracking: a repository the team tracks is not
// materialized, any other is not tracked, and one the gate could not settle is
// an authorize error. The repository is found by GitHub's id, so a name the
// registry does not hold yet still finds it.
func TestRelayServer_RecordsAGHRepositoryDenial(t *testing.T) {
	ctx := context.Background()
	stores, conn := newTestDB(t)
	org, team := runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID
	host, err := db.OrgGitHubHostSystem(ctx, stores.Orgs, org)
	if err != nil {
		t.Fatalf("org host: %v", err)
	}
	tracked := dbseed.GitHubRepository(t, conn, org, host, "acme", "tracked")
	if _, err := conn.Exec(`UPDATE repositories SET external_id = '77' WHERE id = ?`, tracked); err != nil {
		t.Fatalf("record GitHub's id: %v", err)
	}
	dbseed.GitHubRepository(t, conn, org, host, "acme", "untracked")
	if err := stores.TeamGitHubRepos.ReplaceForTeam(ctx, org, team, host, []string{tracked}); err != nil {
		t.Fatalf("track acme/tracked: %v", err)
	}
	const conversationID = "33333333-3333-3333-3333-333333333333"
	dbtest.SeedConversation(t, conn, domain.Conversation{ID: conversationID})
	s := NewRelayServer(stores, ConversationInfo{OrgID: org, TeamID: team, ConversationID: conversationID}, nil)

	cases := []struct {
		args       agentproc.RecordGHRepositoryDenialArgs
		wantTarget string
		wantReason string
	}{
		{agentproc.RecordGHRepositoryDenialArgs{Repository: "acme/tracked", Path: "/repos/acme/tracked"},
			"acme/tracked", "repo-not-materialized"},
		{agentproc.RecordGHRepositoryDenialArgs{Repository: "acme/renamed", RepositoryID: "77", Path: "/api/graphql"},
			"acme/renamed", "repo-not-materialized"},
		{agentproc.RecordGHRepositoryDenialArgs{Repository: "acme/untracked", RepositoryID: "88", Path: "/repos/acme/untracked"},
			"acme/untracked", "repo-not-tracked"},
		{agentproc.RecordGHRepositoryDenialArgs{Repository: "elsewhere/repo", Path: "/repos/elsewhere/repo"},
			"elsewhere/repo", "repo-not-tracked"},
		{agentproc.RecordGHRepositoryDenialArgs{Repository: "acme/tracked", Unresolved: true, Path: "/repos/acme/tracked"},
			"acme/tracked", "authorize-error"},
		{agentproc.RecordGHRepositoryDenialArgs{Unresolved: true, Path: "/api/graphql"},
			"/api/graphql", "authorize-error"},
	}
	for _, tc := range cases {
		args, err := json.Marshal(tc.args)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if err := relayRecord(t, s, agentproc.RelayNamespaceCore, agentproc.OpRecordGHRepositoryDenial, args); err != nil {
			t.Fatalf("relay %+v: %v", tc.args, err)
		}
	}

	var got []domain.ExternalAction
	for _, a := range listExternalActions(t, stores) {
		if a.Action == domain.ActionGitDenied {
			got = append(got, a)
		}
	}
	if len(got) != len(cases) {
		t.Fatalf("git_denied rows = %d, want %d: %+v", len(got), len(cases), got)
	}
	for _, tc := range cases {
		found := false
		for _, row := range got {
			var detail map[string]string
			if err := json.Unmarshal([]byte(row.DetailJSON), &detail); err != nil {
				t.Fatalf("detail %q: %v", row.DetailJSON, err)
			}
			if row.Target == tc.wantTarget && detail["reason"] == tc.wantReason && detail["op"] == "gh" &&
				row.ConversationID == conversationID {
				found = true
			}
		}
		if !found {
			t.Errorf("no git_denied row for %s with reason %s under op gh: %+v", tc.wantTarget, tc.wantReason, got)
		}
	}
}
