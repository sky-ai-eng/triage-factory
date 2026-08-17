package server

import (
	"context"
	"net/http"
	"strings"
	"testing"

	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// seedRunAction appends one external action to conversationID on the local-default
// org/team. RecordSystem mirrors how the exec choke point records a bot run's
// writes in production.
func seedRunAction(t *testing.T, s *Server, conversationID string, a domain.ExternalAction) {
	t.Helper()
	a.ConversationID = conversationID
	a.OrgID = runmode.LocalDefaultOrgID
	a.TeamID = runmode.LocalDefaultTeamID
	if err := sqlitestore.New(s.db).ExternalActions.RecordSystem(context.Background(), runmode.LocalDefaultOrgID, a); err != nil {
		t.Fatalf("seed action: %v", err)
	}
}

// TestHandleAgentActions pins the run-scoped action read — the half of "what did
// this run do" that the artifact list structurally cannot answer. The two rows
// here are exactly that case: a review-thread reply and a refused merge both
// produce no artifact, so before this endpoint a run whose only external acts
// were these read as a run that did nothing.
func TestHandleAgentActions(t *testing.T) {
	s := newTestServer(t)
	conversationID := seedSteerRun(t, s.db, "acts", "completed")

	seedRunAction(t, s, conversationID, domain.ExternalAction{
		Provider: domain.ArtifactProviderGitHub, Action: domain.ActionCommentPosted,
		Target: "octo/repo#42", ExternalID: "777",
		URL:        "https://github.com/octo/repo/pull/42#discussion_r777",
		Credential: domain.CredentialGitHubApp, DetailJSON: `{"in_reply_to":555}`,
	})
	seedRunAction(t, s, conversationID, domain.ExternalAction{
		Provider: domain.ArtifactProviderGitHub, Action: domain.ActionGHChannelWrite,
		Target: "octo/repo", Credential: domain.CredentialGitHubPAT,
		DetailJSON: `{"method":"PUT","path":"/repos/octo/repo/pulls/42/merge","http_status":404,"attempted":"pr_merged"}`,
	})
	// A second run's action, to pin the scoping.
	other := seedSteerRun(t, s.db, "otheracts", "completed")
	seedRunAction(t, s, other, domain.ExternalAction{
		Provider: domain.ArtifactProviderGitHub, Action: domain.ActionBranchPushed,
		Target: "octo/repo", Credential: domain.CredentialGitHubApp,
	})

	page := decodeList[map[string]any](t, doJSON(t, s, http.MethodPost,
		"/api/agent/conversations/"+conversationID+"/actions/list", map[string]any{}))
	got := page.Items
	if len(got) != 2 || page.Total() != 2 {
		t.Fatalf("returned %d actions (total %d), want 2/2 (the other run's must not appear)",
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

// TestHandleAgentActions_Empty pins that a run that touched nothing outside the
// box answers with an empty items array rather than null — the run view renders
// "no external actions yet" off it, which is itself an answer.
func TestHandleAgentActions_Empty(t *testing.T) {
	s := newTestServer(t)
	conversationID := seedSteerRun(t, s.db, "noacts", "completed")
	rec := doJSON(t, s, http.MethodPost, "/api/agent/conversations/"+conversationID+"/actions/list", map[string]any{})
	page := decodeList[map[string]any](t, rec)
	if len(page.Items) != 0 || page.Total() != 0 {
		t.Errorf("empty run actions = %+v (total %d), want an empty page", page.Items, page.Total())
	}
	if !strings.Contains(rec.Body.String(), `"items":[]`) {
		t.Errorf("body = %s, want an empty array (never null)", rec.Body.String())
	}
}

// TestHandleAgentActions_RunNotFound pins that the run read is the authorization
// gate here exactly as it is for artifacts: an unknown — or not-visible — run is
// a 404, never an empty list that would confirm the id exists.
func TestHandleAgentActions_RunNotFound(t *testing.T) {
	s := newTestServer(t)
	rec := doJSON(t, s, http.MethodPost, "/api/agent/conversations/r_ghost/actions/list", map[string]any{})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET missing run = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

// TestHandleAgentActions_CorruptDetails pins that a row with malformed
// detail_json degrades to an omitted `details` with a 200, never a 500 from a
// failed response marshal — an audit surface that 500s on one bad row shows
// nothing at all.
func TestHandleAgentActions_CorruptDetails(t *testing.T) {
	s := newTestServer(t)
	conversationID := seedSteerRun(t, s.db, "badacts", "completed")
	seedRunAction(t, s, conversationID, domain.ExternalAction{
		Provider: domain.ArtifactProviderGitHub, Action: domain.ActionGHChannelWrite,
		Target: "octo/repo", Credential: domain.CredentialGitHubApp,
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
