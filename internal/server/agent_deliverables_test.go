package server

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// seedConversationDeliverable stamps a deliverable onto conversationID on the local-default org/team
// and returns the stored row (with its generated id). UpsertSystem mirrors how
// the exec choke point records deliverables in production.
func seedConversationDeliverable(t *testing.T, s *Server, conversationID string, a domain.Deliverable) domain.Deliverable {
	t.Helper()
	a.ConversationID = conversationID
	a.OrgID = runmode.LocalDefaultOrgID
	a.TeamID = runmode.LocalDefaultTeamID
	stored, err := sqlitestore.New(s.db).Deliverables.UpsertSystem(context.Background(), runmode.LocalDefaultOrgID, dbtest.InOrgScope(t, sqlitestore.New(s.db).Orgs, runmode.LocalDefaultOrgID, a))
	if err != nil {
		t.Fatalf("seed deliverable: %v", err)
	}
	return stored
}

// TestHandleAgentDeliverables pins the conversation-scoped deliverable read
// (TFAC-465): every deliverable a conversation produced, across kinds, projected
// with its stable coordinates and details parsed — an object for a PR, null
// for a detail-less review.
func TestHandleAgentDeliverables(t *testing.T) {
	s := newTestServer(t)
	conversationID := seedSteerConversation(t, s.db, "arts", dbtest.SeedConcluded)

	pr := seedConversationDeliverable(t, s, conversationID, domain.NewPullRequestDeliverable(
		"octo/repo", 42, "PR_node", "feature/x", "main",
		"https://github.com/octo/repo/pull/42", "Add thing", "Body.", true))
	review := seedConversationDeliverable(t, s, conversationID, domain.Deliverable{
		Provider: domain.ProviderGitHub, Kind: domain.DeliverableKindReview,
		Target: "octo/repo#42", ExternalID: "555",
		URL:      "https://github.com/octo/repo/pull/42#pullrequestreview-555",
		State:    domain.DeliverableStateReviewSubmitted,
		DedupKey: domain.ReviewDedupKey("PR_node", conversationID),
	})

	page := decodeList[map[string]any](t, doJSON(t, s, http.MethodPost,
		"/api/agent/conversations/"+conversationID+"/deliverables/list", map[string]any{}))
	got := page.Items
	if len(got) != 2 || page.Total() != 2 {
		t.Fatalf("returned %d deliverables (total %d), want 2/2", len(got), page.Total())
	}
	byID := map[string]map[string]any{}
	for _, m := range got {
		id, _ := m["id"].(string)
		byID[id] = m
	}

	prJSON, ok := byID[pr.ID]
	if !ok {
		t.Fatalf("PR deliverable %s missing from response", pr.ID)
	}
	if prJSON["kind"] != "pull_request" || prJSON["provider"] != "github" ||
		prJSON["state"] != "draft" || prJSON["target"] != "octo/repo#42" ||
		prJSON["external_id"] != "42" || prJSON["url"] != "https://github.com/octo/repo/pull/42" {
		t.Errorf("PR deliverable fields wrong: %+v", prJSON)
	}
	if _, isObj := prJSON["details"].(map[string]any); !isObj {
		t.Errorf("PR details should be a parsed object, got %T: %v", prJSON["details"], prJSON["details"])
	}
	if ca, _ := prJSON["created_at"].(string); ca == "" {
		t.Errorf("PR created_at missing: %v", prJSON["created_at"])
	}

	reviewJSON, ok := byID[review.ID]
	if !ok {
		t.Fatalf("review deliverable %s missing from response", review.ID)
	}
	if reviewJSON["kind"] != "review" {
		t.Errorf("review kind = %v", reviewJSON["kind"])
	}
	// A detail-less deliverable serializes details as null — key present, value nil.
	if v, present := reviewJSON["details"]; !present || v != nil {
		t.Errorf("review details = %v (present=%v), want null", v, present)
	}
}

// TestHandleAgentDeliverables_CorruptDetails pins that a row with malformed
// details_json degrades to `details: null` with a 200 (the json.Valid guard),
// never a 500 from a failed response marshal.
func TestHandleAgentDeliverables_CorruptDetails(t *testing.T) {
	s := newTestServer(t)
	conversationID := seedSteerConversation(t, s.db, "corrupt", dbtest.SeedConcluded)
	art := seedConversationDeliverable(t, s, conversationID, domain.Deliverable{
		Provider: "github", Kind: "review", Target: "octo/repo#1",
		State: domain.DeliverableStateReviewSubmitted, DedupKey: "corrupt1",
	})
	if _, err := s.db.Exec(`UPDATE deliverables SET details_json='{not valid json' WHERE id=?`, art.ID); err != nil {
		t.Fatalf("corrupt details: %v", err)
	}

	rec := doJSON(t, s, http.MethodPost, "/api/agent/conversations/"+conversationID+"/deliverables/list", map[string]any{})
	if rec.Code != http.StatusOK {
		t.Fatalf("POST = %d, want 200 (corrupt details must not 500); body=%s", rec.Code, rec.Body.String())
	}
	got := decodeList[map[string]any](t, rec).Items
	if len(got) != 1 {
		t.Fatalf("returned %d deliverables, want 1", len(got))
	}
	if v, present := got[0]["details"]; !present || v != nil {
		t.Errorf("corrupt details = %v (present=%v), want null", v, present)
	}
}

// TestHandleAgentDeliverables_Empty pins that a conversation with no deliverables returns an
// empty items array, never null — the board card can render a 0 count
// without special-casing.
func TestHandleAgentDeliverables_Empty(t *testing.T) {
	s := newTestServer(t)
	conversationID := seedSteerConversation(t, s.db, "noarts", dbtest.SeedConcluded)
	rec := doJSON(t, s, http.MethodPost, "/api/agent/conversations/"+conversationID+"/deliverables/list", map[string]any{})
	page := decodeList[map[string]any](t, rec)
	if len(page.Items) != 0 || page.Total() != 0 {
		t.Errorf("empty conversation deliverables = %+v (total %d), want an empty page", page.Items, page.Total())
	}
	if !strings.Contains(rec.Body.String(), `"items":[]`) {
		t.Errorf("body = %s, want an empty array (never null)", rec.Body.String())
	}
}

// TestHandleAgentDeliverables_Pages pins the list contract on the route: a
// page holds page_size rows newest first, its token continues where it
// stopped, total_count is every row on each page, and page_size 0 counts.
func TestHandleAgentDeliverables_Pages(t *testing.T) {
	s := newTestServer(t)
	conversationID := seedSteerConversation(t, s.db, "pages", dbtest.SeedConcluded)
	var ids []string
	for i := 0; i < 3; i++ {
		d := seedConversationDeliverable(t, s, conversationID, domain.Deliverable{
			Provider: domain.ProviderGitHub, Kind: domain.DeliverableKindPullRequest,
			Target: "octo/repo#" + strconv.Itoa(100+i), State: domain.DeliverableStatePROpen,
			DedupKey: domain.PullRequestDedupKey("PR_" + strconv.Itoa(100+i)),
		})
		ids = append(ids, d.ID)
	}
	// Equal created_at values would leave the order to the id tiebreaker;
	// distinct ones make newest-first the thing under test.
	for i, id := range ids {
		if _, err := s.db.Exec(`UPDATE deliverables SET created_at = ? WHERE id = ?`,
			time.Date(2026, 1, 1, 0, i, 0, 0, time.UTC), id); err != nil {
			t.Fatalf("stamp created_at: %v", err)
		}
	}

	path := "/api/agent/conversations/" + conversationID + "/deliverables/list"
	first := decodeList[map[string]any](t, doJSON(t, s, http.MethodPost, path, map[string]any{"page_size": 2}))
	if len(first.Items) != 2 || first.Total() != 3 || first.NextPageToken == "" {
		t.Fatalf("first page = %d rows, total %d, token %q; want 2 rows of 3 and a token",
			len(first.Items), first.Total(), first.NextPageToken)
	}
	second := decodeList[map[string]any](t, doJSON(t, s, http.MethodPost, path,
		map[string]any{"page_size": 2, "page_token": first.NextPageToken}))
	if len(second.Items) != 1 || second.Total() != 3 || second.NextPageToken != "" {
		t.Fatalf("second page = %d rows, total %d, token %q; want the last row and no token",
			len(second.Items), second.Total(), second.NextPageToken)
	}
	var got []string
	for _, m := range append(first.Items, second.Items...) {
		id, _ := m["id"].(string)
		got = append(got, id)
	}
	if want := []string{ids[2], ids[1], ids[0]}; !slices.Equal(got, want) {
		t.Errorf("paged ids = %v, want newest first %v", got, want)
	}

	count := decodeList[map[string]any](t, doJSON(t, s, http.MethodPost, path, map[string]any{"page_size": 0}))
	if len(count.Items) != 0 || count.Total() != 3 {
		t.Errorf("count-only = %d rows, total %d; want no rows and 3", len(count.Items), count.Total())
	}
}

// TestHandleAgentDeliverables_UnknownConversationNotFound pins the 404 for an
// unknown (or not-visible) conversation — the conversation read is the
// authorization gate, so a missing conversation is indistinguishable from a
// cross-team one.
func TestHandleAgentDeliverables_UnknownConversationNotFound(t *testing.T) {
	s := newTestServer(t)
	rec := doJSON(t, s, http.MethodPost, "/api/agent/conversations/r_ghost/deliverables/list", map[string]any{})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("POST missing conversation = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

// TestConversationResponse_HasUnresolved pins that a completed conversation with an
// unresolved draft PR reports the derived approval signal
// (has_unresolved_deliverables + unresolved_pr_count) on the single-conversation read.
func TestConversationResponse_HasUnresolved(t *testing.T) {
	s := newTestServer(t)
	conversationID := seedSteerConversation(t, s.db, "park", dbtest.SeedConcluded)
	seedConversationDeliverable(t, s, conversationID, domain.NewPullRequestDeliverable(
		"octo/repo", 7, "PR_node", "feature/x", "main",
		"https://github.com/octo/repo/pull/7", "T", "B", true))
	seedConversationDeliverable(t, s, conversationID, domain.Deliverable{
		Provider: "github", Kind: "review", Target: "octo/repo#7",
		State: domain.DeliverableStateReviewSubmitted, DedupKey: "pk1",
	})

	rec := doJSON(t, s, http.MethodGet, "/api/agent/conversations/"+conversationID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET conversation = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if m["has_unresolved_deliverables"] != true {
		t.Errorf("has_unresolved_deliverables = %v, want true", m["has_unresolved_deliverables"])
	}
	if m["unresolved_pr_count"] != float64(1) {
		t.Errorf("unresolved_pr_count = %v, want 1", m["unresolved_pr_count"])
	}
}

// TestConversationResponse_HasUnresolved_List pins that the derived approval
// signal propagates through the conversation-LIST endpoint — the batched
// ListByConversations path, distinct from the single-conversation path. Guards
// the list endpoint against a silent has_unresolved_deliverables regression.
func TestConversationResponse_HasUnresolved_List(t *testing.T) {
	s := newTestServer(t)
	conversationID := seedSteerConversation(t, s.db, "pklist", dbtest.SeedConcluded)
	seedConversationDeliverable(t, s, conversationID, domain.NewPullRequestDeliverable(
		"octo/repo", 9, "PR_node", "feature/x", "main",
		"https://github.com/octo/repo/pull/9", "T", "B", true))

	convs := listConversations(t, s, fixtureUUID("t_pklist"))
	if len(convs) != 1 {
		t.Fatalf("listed %d conversations, want 1", len(convs))
	}
	if got := convs[0]["has_unresolved_deliverables"]; got != true {
		t.Errorf("has_unresolved_deliverables = %v, want true", got)
	}
	if got := convs[0]["unresolved_pr_count"]; got != float64(1) {
		t.Errorf("unresolved_pr_count = %v, want 1", got)
	}
}
