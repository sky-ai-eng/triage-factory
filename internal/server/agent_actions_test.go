package server

import (
	"context"
	"net/http"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// seedConversationAction appends one external action to conversationID on the local-default
// org/team. RecordSystem mirrors how the exec choke point records a bot conversation's
// writes in production.
func seedConversationAction(t *testing.T, s *Server, conversationID string, a domain.ExternalAction) {
	t.Helper()
	a.ConversationID = conversationID
	a.OrgID = runmode.LocalDefaultOrgID
	a.TeamID = runmode.LocalDefaultTeamID
	if err := sqlitestore.New(s.db).ExternalActions.RecordSystem(context.Background(), runmode.LocalDefaultOrgID, dbtest.ActionInOrgScope(t, sqlitestore.New(s.db).Orgs, runmode.LocalDefaultOrgID, a)); err != nil {
		t.Fatalf("seed action: %v", err)
	}
}

// TestHandleAgentActions pins one conversation's action read: a review-thread
// reply and a refused merge produce no deliverable, so this list is the only
// place they appear, and another conversation's rows stay out of it.
func TestHandleAgentActions(t *testing.T) {
	s := newTestServer(t)
	conversationID := seedSteerConversation(t, s.db, "acts", dbtest.SeedConcluded)

	seedConversationAction(t, s, conversationID, domain.ExternalAction{
		Provider: domain.ProviderGitHub, Action: domain.ActionCommentPosted,
		Target: "octo/repo#42", ExternalID: "777",
		URL:        "https://github.com/octo/repo/pull/42#discussion_r777",
		Credential: domain.CredentialGitHubApp, DetailJSON: `{"in_reply_to":555}`,
		PerformedBy: domain.PerformedByAgent,
	})
	seedConversationAction(t, s, conversationID, domain.ExternalAction{
		Provider: domain.ProviderGitHub, Action: domain.ActionGHChannelWrite,
		Target: "octo/repo", Credential: domain.CredentialGitHubPAT,
		DetailJSON:  `{"method":"PUT","path":"/repos/octo/repo/pulls/42/merge","http_status":404,"attempted":"pr_merged"}`,
		PerformedBy: domain.PerformedByAgent,
	})
	// A second conversation's action, to pin the scoping.
	other := seedSteerConversation(t, s.db, "otheracts", dbtest.SeedConcluded)
	seedConversationAction(t, s, other, domain.ExternalAction{
		Provider: domain.ProviderGitHub, Action: domain.ActionBranchPushed,
		Target: "octo/repo", Credential: domain.CredentialGitHubApp,
		PerformedBy: domain.PerformedByAgent,
	})

	page := decodeList[map[string]any](t, doJSON(t, s, http.MethodPost,
		"/api/agent/conversations/"+conversationID+"/actions/list", map[string]any{}))
	got := page.Items
	if len(got) != 2 || page.Total() != 2 {
		t.Fatalf("returned %d actions (total %d), want 2/2 (the other conversation's must not appear)",
			len(got), page.Total())
	}

	byAction := map[string]map[string]any{}
	for _, m := range got {
		name, _ := m["action"].(string)
		byAction[name] = m
	}
	reply, ok := byAction[domain.ActionCommentPosted]
	if !ok {
		t.Fatalf("comment_posted missing from response: %+v", got)
	}
	if reply["target"] != "octo/repo#42" || reply["external_id"] != "777" ||
		reply["credential"] != "github_app" || reply["provider"] != "github" {
		t.Errorf("reply action fields wrong: %+v", reply)
	}

	// The fallback row's detail is its only identifying content — the whole
	// reason the wire shape embeds detail_json rather than dropping it.
	write, ok := byAction[domain.ActionGHChannelWrite]
	if !ok {
		t.Fatalf("gh_channel_write missing from response: %+v", got)
	}
	details, isObj := write["details"].(map[string]any)
	if !isObj {
		t.Fatalf("gh_channel_write details should be a parsed object, got %T: %v", write["details"], write["details"])
	}
	if details["method"] != "PUT" || details["path"] != "/repos/octo/repo/pulls/42/merge" ||
		details["http_status"] != float64(404) || details["attempted"] != "pr_merged" {
		t.Errorf("gh_channel_write details wrong: %+v", details)
	}
}

// TestHandleAgentActions_UnknownConversationNotFound pins the conversation read
// as the gate: an unknown or not-visible conversation is a 404, never an empty
// list that would confirm the id exists.
func TestHandleAgentActions_UnknownConversationNotFound(t *testing.T) {
	s := newTestServer(t)
	rec := doJSON(t, s, http.MethodPost, "/api/agent/conversations/r_ghost/actions/list", map[string]any{})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET missing conversation = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

// TestHandleAgentActions_CorruptDetails pins that a row with malformed
// detail_json is answered with `details` omitted and a 200, not a 500 from a
// failed response marshal that would hide every other row.
func TestHandleAgentActions_CorruptDetails(t *testing.T) {
	s := newTestServer(t)
	conversationID := seedSteerConversation(t, s.db, "badacts", dbtest.SeedConcluded)
	seedConversationAction(t, s, conversationID, domain.ExternalAction{
		Provider: domain.ProviderGitHub, Action: domain.ActionGHChannelWrite,
		Target: "octo/repo", Credential: domain.CredentialGitHubApp,
		PerformedBy: domain.PerformedByAgent,
	})
	if _, err := s.db.Exec(`UPDATE external_actions SET detail_json='{not valid json' WHERE conversation_id=?`, conversationID); err != nil {
		t.Fatalf("corrupt detail: %v", err)
	}

	got := decodeList[map[string]any](t, doJSON(t, s, http.MethodPost,
		"/api/agent/conversations/"+conversationID+"/actions/list", map[string]any{})).Items
	if len(got) != 1 {
		t.Fatalf("returned %d actions, want 1", len(got))
	}
	if _, present := got[0]["details"]; present {
		t.Errorf("corrupt detail should be omitted, got %v", got[0]["details"])
	}
}
