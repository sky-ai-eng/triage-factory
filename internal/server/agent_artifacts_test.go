package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// seedConversationArtifact stamps an artifact onto conversationID on the local-default org/team
// and returns the stored row (with its generated id). UpsertSystem mirrors how
// the exec choke point records artifacts in production.
func seedConversationArtifact(t *testing.T, s *Server, conversationID string, a domain.Artifact) domain.Artifact {
	t.Helper()
	a.ConversationID = conversationID
	a.OrgID = runmode.LocalDefaultOrgID
	a.TeamID = runmode.LocalDefaultTeamID
	stored, err := sqlitestore.New(s.db).Artifacts.UpsertSystem(context.Background(), runmode.LocalDefaultOrgID, a)
	if err != nil {
		t.Fatalf("seed artifact: %v", err)
	}
	return stored
}

// TestHandleAgentArtifacts pins the conversation-scoped artifact read
// (TFAC-465): every artifact a conversation produced, across kinds, projected
// with its stable coordinates and details parsed — an object for a PR, null
// for a detail-less comment.
func TestHandleAgentArtifacts(t *testing.T) {
	s := newTestServer(t)
	conversationID := seedSteerConversation(t, s.db, "arts", "completed")

	pr := seedConversationArtifact(t, s, conversationID, domain.NewPullRequestArtifact(
		"octo/repo", 42, "PR_node", "feature/x", "main",
		"https://github.com/octo/repo/pull/42", "Add thing", "Body.", true))
	comment := seedConversationArtifact(t, s, conversationID, domain.Artifact{
		Provider: domain.ArtifactProviderGitHub, Kind: domain.ArtifactKindComment,
		Target: "octo/repo#42", ExternalID: "555",
		URL:      "https://github.com/octo/repo/pull/42#issuecomment-555",
		State:    domain.ArtifactStateCommentPosted,
		DedupKey: domain.ArtifactDedupKey("github", "comment", "555", ""),
	})

	rec := doJSON(t, s, http.MethodGet, "/api/agent/conversations/"+conversationID+"/artifacts", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET artifacts = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var got []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v; body=%s", err, rec.Body.String())
	}
	if len(got) != 2 {
		t.Fatalf("returned %d artifacts, want 2", len(got))
	}
	byID := map[string]map[string]any{}
	for _, m := range got {
		id, _ := m["id"].(string)
		byID[id] = m
	}

	prJSON, ok := byID[pr.ID]
	if !ok {
		t.Fatalf("PR artifact %s missing from response", pr.ID)
	}
	if prJSON["kind"] != "pull_request" || prJSON["provider"] != "github" ||
		prJSON["state"] != "draft" || prJSON["target"] != "octo/repo#42" ||
		prJSON["external_id"] != "42" || prJSON["url"] != "https://github.com/octo/repo/pull/42" {
		t.Errorf("PR artifact fields wrong: %+v", prJSON)
	}
	if _, isObj := prJSON["details"].(map[string]any); !isObj {
		t.Errorf("PR details should be a parsed object, got %T: %v", prJSON["details"], prJSON["details"])
	}
	if ca, _ := prJSON["created_at"].(string); ca == "" {
		t.Errorf("PR created_at missing: %v", prJSON["created_at"])
	}

	commentJSON, ok := byID[comment.ID]
	if !ok {
		t.Fatalf("comment artifact %s missing from response", comment.ID)
	}
	if commentJSON["kind"] != "comment" {
		t.Errorf("comment kind = %v", commentJSON["kind"])
	}
	// A detail-less artifact serializes details as null — key present, value nil.
	if v, present := commentJSON["details"]; !present || v != nil {
		t.Errorf("comment details = %v (present=%v), want null", v, present)
	}
}

// TestHandleAgentArtifacts_CorruptDetails pins that a row with malformed
// details_json degrades to `details: null` with a 200 (the json.Valid guard),
// never a 500 from a failed response marshal.
func TestHandleAgentArtifacts_CorruptDetails(t *testing.T) {
	s := newTestServer(t)
	conversationID := seedSteerConversation(t, s.db, "corrupt", "completed")
	art := seedConversationArtifact(t, s, conversationID, domain.Artifact{
		Provider: "github", Kind: "comment", Target: "octo/repo#1",
		State: domain.ArtifactStateCommentPosted, DedupKey: "corrupt1",
	})
	if _, err := s.db.Exec(`UPDATE artifacts SET details_json='{not valid json' WHERE id=?`, art.ID); err != nil {
		t.Fatalf("corrupt details: %v", err)
	}

	rec := doJSON(t, s, http.MethodGet, "/api/agent/conversations/"+conversationID+"/artifacts", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET = %d, want 200 (corrupt details must not 500); body=%s", rec.Code, rec.Body.String())
	}
	var got []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v; body=%s", err, rec.Body.String())
	}
	if len(got) != 1 {
		t.Fatalf("returned %d artifacts, want 1", len(got))
	}
	if v, present := got[0]["details"]; !present || v != nil {
		t.Errorf("corrupt details = %v (present=%v), want null", v, present)
	}
}

// TestHandleAgentArtifacts_Empty pins that a conversation with no artifacts returns an
// empty JSON array (200 []), never null — the board card can render a 0 count
// without special-casing.
func TestHandleAgentArtifacts_Empty(t *testing.T) {
	s := newTestServer(t)
	conversationID := seedSteerConversation(t, s.db, "noarts", "completed")
	rec := doJSON(t, s, http.MethodGet, "/api/agent/conversations/"+conversationID+"/artifacts", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if body := strings.TrimSpace(rec.Body.String()); body != "[]" {
		t.Errorf("empty conversation artifacts body = %q, want []", body)
	}
}

// TestHandleAgentArtifacts_UnknownConversationNotFound pins the 404 for an
// unknown (or not-visible) conversation — the conversation read is the
// authorization gate, so a missing conversation is indistinguishable from a
// cross-team one.
func TestHandleAgentArtifacts_UnknownConversationNotFound(t *testing.T) {
	s := newTestServer(t)
	rec := doJSON(t, s, http.MethodGet, "/api/agent/conversations/r_ghost/artifacts", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET missing conversation = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

// TestConversationResponse_ArtifactCount pins artifact_count on the single-conversation response
// (GET /api/agent/conversations/{id}).
func TestConversationResponse_ArtifactCount(t *testing.T) {
	s := newTestServer(t)
	conversationID := seedSteerConversation(t, s.db, "cnt", "completed")
	seedConversationArtifact(t, s, conversationID, domain.Artifact{
		Provider: "github", Kind: "comment", Target: "octo/repo",
		State: domain.ArtifactStateCommentPosted, DedupKey: "c1",
	})
	seedConversationArtifact(t, s, conversationID, domain.Artifact{
		Provider: "github", Kind: "comment", Target: "octo/repo",
		State: domain.ArtifactStateCommentPosted, DedupKey: "c2",
	})

	rec := doJSON(t, s, http.MethodGet, "/api/agent/conversations/"+conversationID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET conversation = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if m["artifact_count"] != float64(2) {
		t.Errorf("artifact_count = %v, want 2", m["artifact_count"])
	}
}

// TestConversationResponse_ArtifactCount_Unresolved pins that a completed conversation with an
// unresolved draft PR reports the right artifact_count (from CountByConversation) and the
// derived approval signal (has_unresolved_artifacts + unresolved_pr_count) — the
// successor to the legacy pending_kind overlay.
func TestConversationResponse_ArtifactCount_Unresolved(t *testing.T) {
	s := newTestServer(t)
	conversationID := seedSteerConversation(t, s.db, "park", "completed")
	seedConversationArtifact(t, s, conversationID, domain.NewPullRequestArtifact(
		"octo/repo", 7, "PR_node", "feature/x", "main",
		"https://github.com/octo/repo/pull/7", "T", "B", true))
	seedConversationArtifact(t, s, conversationID, domain.Artifact{
		Provider: "github", Kind: "comment", Target: "octo/repo#7",
		State: domain.ArtifactStateCommentPosted, DedupKey: "pk1",
	})

	rec := doJSON(t, s, http.MethodGet, "/api/agent/conversations/"+conversationID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET conversation = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if m["artifact_count"] != float64(2) {
		t.Errorf("artifact_count = %v, want 2", m["artifact_count"])
	}
	if m["has_unresolved_artifacts"] != true {
		t.Errorf("has_unresolved_artifacts = %v, want true", m["has_unresolved_artifacts"])
	}
	if m["unresolved_pr_count"] != float64(1) {
		t.Errorf("unresolved_pr_count = %v, want 1", m["unresolved_pr_count"])
	}
}

// TestConversationResponse_HasUnresolved_List pins that the derived approval
// signal propagates through the conversation-LIST endpoint — the batched
// ListByConversations path, distinct from the single-conversation path. Guards
// the list endpoint against a silent has_unresolved_artifacts regression.
func TestConversationResponse_HasUnresolved_List(t *testing.T) {
	s := newTestServer(t)
	conversationID := seedSteerConversation(t, s.db, "pklist", "completed")
	seedConversationArtifact(t, s, conversationID, domain.NewPullRequestArtifact(
		"octo/repo", 9, "PR_node", "feature/x", "main",
		"https://github.com/octo/repo/pull/9", "T", "B", true))

	convs := listConversations(t, s, fixtureUUID("t_pklist"))
	if len(convs) != 1 {
		t.Fatalf("listed %d conversations, want 1", len(convs))
	}
	if got := convs[0]["has_unresolved_artifacts"]; got != true {
		t.Errorf("has_unresolved_artifacts = %v, want true", got)
	}
	if got := convs[0]["unresolved_pr_count"]; got != float64(1) {
		t.Errorf("unresolved_pr_count = %v, want 1", got)
	}
}

// TestConversationResponse_ArtifactCount_List pins that the batched count flows
// through the conversation-list path (POST /api/agent/conversations/list): two
// conversations on one task get their own correct counts from the single
// CountByConversation batch.
func TestConversationResponse_ArtifactCount_List(t *testing.T) {
	s := newTestServer(t)
	// seedSteerConversation mints task t_lst with conversation r_lst and prompt
	// p_lst; add a second conversation on the same task so the list path counts
	// more than one conversation.
	conv1 := seedSteerConversation(t, s.db, "lst", "completed")
	taskID := fixtureUUID("t_lst")
	conv2 := fixtureUUID("r_lst2")
	brID := seedBlueprintRunSQLite(t, s.db, taskID)
	execSQL(t, s.db, `INSERT INTO conversations (id, task_id, prompt_id, status, trigger_type, blueprint_run_id, blueprint_step_index) VALUES (?, ?, ?, 'completed', 'manual', ?, 0)`, conv2, taskID, fixtureUUID("p_lst"), brID)

	mkComment := func(key string) domain.Artifact {
		return domain.Artifact{
			Provider: "github", Kind: "comment", Target: "octo/repo",
			State: domain.ArtifactStateCommentPosted, DedupKey: key,
		}
	}
	seedConversationArtifact(t, s, conv1, mkComment("l1"))
	seedConversationArtifact(t, s, conv1, mkComment("l2"))
	seedConversationArtifact(t, s, conv2, mkComment("l3"))

	convs := listConversations(t, s, taskID)
	if len(convs) != 2 {
		t.Fatalf("listed %d conversations, want 2", len(convs))
	}
	want := map[string]float64{conv1: 2, conv2: 1}
	for _, m := range convs {
		id, _ := m["ID"].(string)
		if got := m["artifact_count"]; got != want[id] {
			t.Errorf("conversation %s artifact_count = %v, want %v", id, got, want[id])
		}
	}
}
