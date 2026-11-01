package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zalando/go-keyring"

	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// seedReviewDeliverableWithConversation mints a concluded conversation chain and
// a finalized review draft on it: pending, the head SHA pinned, one staged inline
// comment with a MAJOR badge, and the agent's draft snapshotted into proposed.
// The review is staged TF-side; no GitHub object exists until approval.
func seedReviewDeliverableWithConversation(t *testing.T, s *Server, suffix, owner, repo string, number int, event string) (deliverableID, conversationID, taskID string) {
	t.Helper()
	conversationID = seedSteerConversation(t, s.db, suffix, dbtest.SeedConcluded)
	taskID = fixtureUUID("t_" + suffix)
	if _, err := sqlitestore.New(s.db).TaskMemory.UpsertAgentMemory(context.Background(), runmode.LocalDefaultOrgID, conversationID, "", "agent self-report", domain.MemorySourceAgent); err != nil {
		t.Fatalf("seed agent memory: %v", err)
	}
	line := 3
	a := domain.NewReviewDeliverable(owner+"/"+repo, number, fmt.Sprintf("PR_%s_%s_%d", owner, repo, number), "headsha"+suffix, conversationID)
	a.ConversationID = conversationID
	a.OrgID = runmode.LocalDefaultOrgID
	a.TeamID = runmode.LocalDefaultTeamID
	d, _ := domain.ParseReviewDeliverableDetails(a.DetailsJSON)
	d.ReviewBody = "## Review\nlgtm"
	d.ReviewEvent = event
	// The staged comment carries the badge baked in — what the agent drafted, and
	// what the proposed snapshot on the deliverable row records.
	staged := []domain.ReviewDeliverableComment{
		{ID: "c_1", Path: "a.go", Line: &line, Body: domain.SeverityBadgeMarkdown(domain.SeverityMajor) + "nit: rename"},
	}
	d.StagedComments = staged
	d.Proposed = domain.ReviewDeliverableProposed{Body: "## Review\nlgtm", Event: event, Comments: staged}
	a.DetailsJSON = domain.MarshalReviewDeliverableDetails(d)
	stored, err := sqlitestore.New(s.db).Deliverables.UpsertSystem(context.Background(), runmode.LocalDefaultOrgID, dbtest.InOrgScope(t, sqlitestore.New(s.db).Orgs, runmode.LocalDefaultOrgID, a))
	if err != nil {
		t.Fatalf("seed review deliverable: %v", err)
	}
	return stored.ID, conversationID, taskID
}

// TestReviewDeliverableGet_SeverityRoundTrip pins the read: GET returns the deliverable
// with its TF-side staged comments — no GitHub call — each with its severity
// parsed back out of the body (the chip) and the clean body shown.
func TestReviewDeliverableGet_SeverityRoundTrip(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)

	artID, _, _ := seedReviewDeliverableWithConversation(t, srv, "rget", "acme", "api", 7, "COMMENT")
	rec := doJSON(t, srv, http.MethodGet, "/api/deliverables/"+artID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("get = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var out struct {
		Kind       string `json:"kind"`
		ExternalID string `json:"external_id"`
		Details    struct {
			ReviewBody  string `json:"review_body"`
			ReviewEvent string `json:"review_event"`
			Comments    []struct {
				Body     string `json:"body"`
				Severity string `json:"severity"`
			} `json:"comments"`
		} `json:"details"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// The discriminator names the payload's shape, and a draft has no GitHub
	// review id yet.
	if out.Kind != domain.DeliverableKindReview {
		t.Errorf("kind = %q, want review", out.Kind)
	}
	if out.ExternalID != "" || out.Details.ReviewEvent != "COMMENT" {
		t.Errorf("review id/event = %q/%q, want empty/COMMENT", out.ExternalID, out.Details.ReviewEvent)
	}
	if len(out.Details.Comments) != 1 {
		t.Fatalf("len(comments) = %d, want 1", len(out.Details.Comments))
	}
	if out.Details.Comments[0].Severity != domain.SeverityMajor {
		t.Errorf("comment severity = %q, want MAJOR (parsed from the body badge)", out.Details.Comments[0].Severity)
	}
	if out.Details.Comments[0].Body != "nit: rename" {
		t.Errorf("comment body = %q, want the clean body with the badge stripped", out.Details.Comments[0].Body)
	}
}

// TestReviewDeliverableApprove: approval submits the staged event, body, footer
// and comments in one POST, marks the deliverable submitted with the review's id
// and URL, and leaves the conversation and its memory alone. Keep the payload
// checks in step with cmd/exec/agenthost TestFinalizeReviewDraft_PostureDecidesPosting.
func TestReviewDeliverableApprove(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)
	var submitBody map[string]any
	mux := newAppAPIMux()
	mux.HandleFunc("POST /api/v3/repos/{owner}/{repo}/pulls/{number}/reviews", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&submitBody)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 12345})
	})
	stub := httptest.NewServer(mux)
	t.Cleanup(stub.Close)
	seedApp(t, srv, stub, acmeInstall())

	artID, conversationID, _ := seedReviewDeliverableWithConversation(t, srv, "rappr", "acme", "api", 7, "COMMENT")
	rec := doJSON(t, srv, http.MethodPost, "/api/deliverables/"+artID+"/approve", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("approve = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	// The submit reached GitHub atomically: staged event, body + footer, commit
	// pin, and the staged comment all in one POST.
	if submitBody == nil {
		t.Fatal("approve must call SubmitReview")
	}
	if submitBody["event"] != "COMMENT" {
		t.Errorf("submit event = %v, want COMMENT", submitBody["event"])
	}
	if submitBody["commit_id"] != "headsharappr" {
		t.Errorf("submit commit_id = %v, want the pinned head SHA", submitBody["commit_id"])
	}
	// The footer is the same static disclosure the PR create verb appends: the
	// AI line, and nothing that moves after posting. This server runs in local
	// mode, where the run's address is the operator's own browser and so is
	// never published.
	got, _ := submitBody["body"].(string)
	if !strings.HasPrefix(got, "## Review") || !strings.Contains(got, "Triage Factory") {
		t.Errorf("submit body = %q, want staged body + agentmeta footer", got)
	}
	if strings.Contains(got, "View the run") {
		t.Errorf("submit body = %q, want no run link in local mode", got)
	}
	for _, banned := range []string{"Time:", "Cost:", "Model:"} {
		if strings.Contains(got, banned) {
			t.Errorf("submit body carries %q; the footer discloses, it does not meter: %q", banned, got)
		}
	}
	comments, _ := submitBody["comments"].([]any)
	if len(comments) != 1 {
		t.Fatalf("submit comments = %v, want the one staged comment", submitBody["comments"])
	}

	art := getDeliverable(t, srv, artID)
	if art.State != domain.DeliverableStateReviewSubmitted {
		t.Errorf("deliverable state = %q, want submitted", art.State)
	}
	if art.ExternalID != "12345" {
		t.Errorf("deliverable ExternalID = %q, want the submitted review id 12345", art.ExternalID)
	}
	if !strings.Contains(art.URL, "pullrequestreview-12345") {
		t.Errorf("deliverable URL = %q, want a review anchor", art.URL)
	}
	// The deep link must be anchored to the org's own GitHub host (seedApp sets
	// GitHubBaseURL to the stub), not a hardcoded github.com — the review URL is
	// composed TF-side, so an org on GHES/GHEC would otherwise get a dead link.
	if !strings.HasPrefix(art.URL, stub.URL+"/") || strings.Contains(art.URL, "github.com") {
		t.Errorf("deliverable URL = %q, want the org GitHub host %q, not hardcoded github.com", art.URL, stub.URL)
	}
	if !storedConcluded(t, srv.db, conversationID) {
		t.Error("conversation no longer concluded")
	}
	// Submitting a review writes no memory: the conversation's row is the
	// agent's own account of what it tried, and a verdict about a deliverable is
	// not that.
	assertAgentMemoryUntouched(t, srv, conversationID)
}

// TestReviewDeliverableApprove_NonPending_409 pins the state guard: a stale/double
// approve on an already-submitted review returns 409 and makes no GitHub call.
func TestReviewDeliverableApprove_NonPending_409(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)
	var submitHit bool
	mux := newAppAPIMux()
	mux.HandleFunc("POST /api/v3/repos/{owner}/{repo}/pulls/{number}/reviews", func(w http.ResponseWriter, r *http.Request) {
		submitHit = true
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 1})
	})
	stub := httptest.NewServer(mux)
	t.Cleanup(stub.Close)
	seedApp(t, srv, stub, acmeInstall())

	artID, _, _ := seedReviewDeliverableWithConversation(t, srv, "r409", "acme", "api", 7, "COMMENT")
	// Flip it to submitted out-of-band.
	art := getDeliverable(t, srv, artID)
	art.State = domain.DeliverableStateReviewSubmitted
	if _, err := sqlitestore.New(srv.db).Deliverables.UpsertSystem(context.Background(), runmode.LocalDefaultOrgID, dbtest.InOrgScope(t, sqlitestore.New(srv.db).Orgs, runmode.LocalDefaultOrgID, *art)); err != nil {
		t.Fatalf("flip submitted: %v", err)
	}
	rec := doJSON(t, srv, http.MethodPost, "/api/deliverables/"+artID+"/approve", nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("approve = %d, want 409 on a non-pending review; body=%s", rec.Code, rec.Body.String())
	}
	if submitHit {
		t.Error("a 409 approve must not touch GitHub")
	}
}

// TestReviewDeliverableApprove_SubmitsPostClaimContent: an edit landing between
// the approve click and the claim is what reaches GitHub, because the payload is
// rebuilt from the row after the claim. The racing PATCH runs from the repo
// probe (GET /repos), which approve makes before taking the claim.
func TestReviewDeliverableApprove_SubmitsPostClaimContent(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)

	var artID string
	var raced bool
	var submitBody map[string]any
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v3/app/installations/{id}/access_tokens", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token":      "ghs_" + r.PathValue("id"),
			"expires_at": time.Now().Add(time.Hour).UTC(),
		})
	})
	mux.HandleFunc("GET /api/v3/repos/{owner}/{repo}", func(w http.ResponseWriter, r *http.Request) {
		if !raced && artID != "" {
			raced = true
			rec := doJSON(t, srv, http.MethodPatch, "/api/deliverables/"+artID+"/review", map[string]any{"body": "EDITED just before the claim"})
			if rec.Code != http.StatusOK {
				t.Errorf("racing PATCH = %d, want 200 (the claim hasn't been taken yet); body=%s", rec.Code, rec.Body.String())
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"clone_url": "https://example.test/acme/api.git", "default_branch": "main",
		})
	})
	mux.HandleFunc("POST /api/v3/repos/{owner}/{repo}/pulls/{number}/reviews", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&submitBody)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 31337})
	})
	stub := httptest.NewServer(mux)
	t.Cleanup(stub.Close)
	seedApp(t, srv, stub, acmeInstall())

	artID, _, _ = seedReviewDeliverableWithConversation(t, srv, "rrace", "acme", "api", 7, "COMMENT")
	rec := doJSON(t, srv, http.MethodPost, "/api/deliverables/"+artID+"/approve", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("approve = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !raced {
		t.Fatal("the coverage-probe hook never fired — the racing PATCH was not exercised")
	}
	got, _ := submitBody["body"].(string)
	if !strings.HasPrefix(got, "EDITED just before the claim") {
		t.Errorf("submitted body = %q, want the post-edit content (approve must re-read after the claim)", got)
	}
}

// TestReviewDeliverableApprove_GitHubFailure_ReleasesClaim: approve claims the
// review before submitting so the post is at most once; a GitHub failure
// releases the claim (pending, no id or URL), and the retry then submits.
func TestReviewDeliverableApprove_GitHubFailure_ReleasesClaim(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)
	fail := true
	mux := newAppAPIMux()
	mux.HandleFunc("POST /api/v3/repos/{owner}/{repo}/pulls/{number}/reviews", func(w http.ResponseWriter, r *http.Request) {
		if fail {
			fail = false
			http.Error(w, `{"message":"boom"}`, http.StatusBadGateway)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 777})
	})
	stub := httptest.NewServer(mux)
	t.Cleanup(stub.Close)
	seedApp(t, srv, stub, acmeInstall())

	artID, _, _ := seedReviewDeliverableWithConversation(t, srv, "rrel", "acme", "api", 7, "COMMENT")

	rec := doJSON(t, srv, http.MethodPost, "/api/deliverables/"+artID+"/approve", nil)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("failed approve = %d, want 502; body=%s", rec.Code, rec.Body.String())
	}
	art := getDeliverable(t, srv, artID)
	if art.State != domain.DeliverableStateReviewPending || art.ExternalID != "" || art.URL != "" {
		t.Fatalf("after failed submit: state=%q ext=%q url=%q, want the claim released (pending, no coordinates)", art.State, art.ExternalID, art.URL)
	}

	rec = doJSON(t, srv, http.MethodPost, "/api/deliverables/"+artID+"/approve", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("retry approve = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	art = getDeliverable(t, srv, artID)
	if art.State != domain.DeliverableStateReviewSubmitted || art.ExternalID != "777" || !strings.Contains(art.URL, "pullrequestreview-777") {
		t.Fatalf("after retry: state=%q ext=%q url=%q, want submitted with the posted review's coordinates", art.State, art.ExternalID, art.URL)
	}
}

// TestReviewDeliverableGet_Submitted_URLNoFreshness: a submitted review reads
// back with its posted coordinates and no GitHub call, since freshness against
// the live head means nothing once posted; commits_since_finalize stays null.
func TestReviewDeliverableGet_Submitted_URLNoFreshness(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)
	mux := newAppAPIMux()
	// The approve itself reads the live head to reconcile before posting; only
	// the read AFTER the submit is forbidden.
	var submitted atomic.Bool
	mux.HandleFunc("GET /api/v3/repos/{owner}/{repo}/pulls/{number}", func(w http.ResponseWriter, r *http.Request) {
		if submitted.Load() {
			t.Error("a submitted review's GET must not fetch the live PR head")
			http.Error(w, "unexpected", http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"number": 7,
			"base":   map[string]any{"ref": "main", "sha": "base1"},
			"head":   map[string]any{"sha": "headsharsurl"},
		})
	})
	mux.HandleFunc("POST /api/v3/repos/{owner}/{repo}/pulls/{number}/reviews", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 555})
	})
	stub := httptest.NewServer(mux)
	t.Cleanup(stub.Close)
	seedApp(t, srv, stub, acmeInstall())

	artID, _, _ := seedReviewDeliverableWithConversation(t, srv, "rsurl", "acme", "api", 7, "COMMENT")
	if rec := doJSON(t, srv, http.MethodPost, "/api/deliverables/"+artID+"/approve", nil); rec.Code != http.StatusOK {
		t.Fatalf("approve = %d; body=%s", rec.Code, rec.Body.String())
	}
	submitted.Store(true)

	rec := doJSON(t, srv, http.MethodGet, "/api/deliverables/"+artID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("get = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var out struct {
		State      string `json:"state"`
		URL        string `json:"url"`
		ExternalID string `json:"external_id"`
		Details    struct {
			CommitsSinceFinalize *int `json:"commits_since_finalize"`
		} `json:"details"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.State != domain.DeliverableStateReviewSubmitted || out.ExternalID != "555" ||
		!strings.Contains(out.URL, "pullrequestreview-555") {
		t.Errorf("get = state %q id %q url %q, want the posted review's coordinates", out.State, out.ExternalID, out.URL)
	}
	if out.Details.CommitsSinceFinalize != nil {
		t.Errorf("commits_since_finalize = %v, want null for a resolved review", *out.Details.CommitsSinceFinalize)
	}
}

// TestReviewDeliverableApprove_NilLineComment_422 pins that a staged comment with no
// line anchor can't be submitted: approve 422s before any GitHub call rather than
// silently dropping it.
func TestReviewDeliverableApprove_NilLineComment_422(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)
	var submitHit bool
	mux := newAppAPIMux()
	mux.HandleFunc("POST /api/v3/repos/{owner}/{repo}/pulls/{number}/reviews", func(w http.ResponseWriter, r *http.Request) {
		submitHit = true
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 1})
	})
	stub := httptest.NewServer(mux)
	t.Cleanup(stub.Close)
	seedApp(t, srv, stub, acmeInstall())

	artID, _, _ := seedReviewDeliverableWithConversation(t, srv, "rnil", "acme", "api", 7, "COMMENT")
	// Null out the staged comment's line anchor.
	art := getDeliverable(t, srv, artID)
	d, _ := domain.ParseReviewDeliverableDetails(art.DetailsJSON)
	d.StagedComments[0].Line = nil
	art.DetailsJSON = domain.MarshalReviewDeliverableDetails(d)
	if _, err := sqlitestore.New(srv.db).Deliverables.UpsertSystem(context.Background(), runmode.LocalDefaultOrgID, dbtest.InOrgScope(t, sqlitestore.New(srv.db).Orgs, runmode.LocalDefaultOrgID, *art)); err != nil {
		t.Fatalf("null line: %v", err)
	}
	rec := doJSON(t, srv, http.MethodPost, "/api/deliverables/"+artID+"/approve", nil)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("approve = %d, want 422 on an unpositioned comment; body=%s", rec.Code, rec.Body.String())
	}
	if submitHit {
		t.Error("a 422 must not reach GitHub")
	}
	if getDeliverable(t, srv, artID).State != domain.DeliverableStateReviewPending {
		t.Error("deliverable must stay pending after a 422")
	}
}

// TestReviewDeliverableCommentUpdate_RebakesBadge pins that a staged-comment edit
// re-bakes the comment's existing severity onto the human's clean body, with no
// GitHub call.
func TestReviewDeliverableCommentUpdate_RebakesBadge(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)

	artID, _, _ := seedReviewDeliverableWithConversation(t, srv, "rcu", "acme", "api", 7, "COMMENT")
	rec := doJSON(t, srv, http.MethodPatch, "/api/deliverables/"+artID+"/comments/c_1", map[string]any{"body": "rename to fooBar"})
	if rec.Code != http.StatusOK {
		t.Fatalf("comment update = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	d, _ := domain.ParseReviewDeliverableDetails(getDeliverable(t, srv, artID).DetailsJSON)
	if len(d.StagedComments) != 1 {
		t.Fatalf("staged comments = %d, want 1", len(d.StagedComments))
	}
	// The MAJOR badge (recovered from the staged comment) must be re-baked on, and
	// the human's clean prose preserved.
	got := d.StagedComments[0].Body
	if !strings.HasPrefix(got, domain.SeverityBadgeMarkdown(domain.SeverityMajor)) {
		t.Errorf("updated body = %q, want the MAJOR badge re-baked in", got)
	}
	if !strings.HasSuffix(got, "rename to fooBar") {
		t.Errorf("updated body = %q, want the human's clean prose preserved", got)
	}
}

// TestReviewDeliverableCommentUpdate_NotFound_404 pins that an edit targeting a
// comment id not in the staged set is rejected with 404.
func TestReviewDeliverableCommentUpdate_NotFound_404(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)

	artID, _, _ := seedReviewDeliverableWithConversation(t, srv, "rcw", "acme", "api", 7, "COMMENT")
	rec := doJSON(t, srv, http.MethodPatch, "/api/deliverables/"+artID+"/comments/c_missing", map[string]any{"body": "x"})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("update = %d, want 404 for a comment not in the staged set; body=%s", rec.Code, rec.Body.String())
	}
}

// TestReviewDeliverableCommentDelete pins delete on the staged set (found → 200,
// removed; missing → 404), with no GitHub call.
func TestReviewDeliverableCommentDelete(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)

	artID, _, _ := seedReviewDeliverableWithConversation(t, srv, "rcd", "acme", "api", 7, "COMMENT")

	rec := doJSON(t, srv, http.MethodDelete, "/api/deliverables/"+artID+"/comments/c_1", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if d, _ := domain.ParseReviewDeliverableDetails(getDeliverable(t, srv, artID).DetailsJSON); len(d.StagedComments) != 0 {
		t.Errorf("staged comments = %d after delete, want 0", len(d.StagedComments))
	}

	rec = doJSON(t, srv, http.MethodDelete, "/api/deliverables/"+artID+"/comments/c_missing", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("delete = %d, want 404 for a comment not in the staged set; body=%s", rec.Code, rec.Body.String())
	}
}

// TestReviewDeliverableUpdate_InvalidEvent_400: an invalid or empty review_event
// is rejected at PATCH time and the stored one is left unchanged, since an empty
// event would un-park the conversation.
func TestReviewDeliverableUpdate_InvalidEvent_400(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)
	artID, _, _ := seedReviewDeliverableWithConversation(t, srv, "rie", "acme", "api", 7, "COMMENT")
	for _, bad := range []string{"", "approve", "LGTM"} {
		rec := doJSON(t, srv, http.MethodPatch, "/api/deliverables/"+artID+"/review", map[string]any{"event": bad})
		if rec.Code != http.StatusBadRequest {
			t.Errorf("PATCH review_event=%q = %d, want 400", bad, rec.Code)
		}
	}
	d, _ := domain.ParseReviewDeliverableDetails(getDeliverable(t, srv, artID).DetailsJSON)
	if d.ReviewEvent != "COMMENT" {
		t.Errorf("review_event = %q after rejected PATCHes, want COMMENT (unchanged)", d.ReviewEvent)
	}
}

// TestReviewDeliverableDismiss: dismissing a staged review is a field write,
// PATCH {state:"dismissed"}, because nothing happens on GitHub; it flips the
// deliverable to dismissed and leaves the conversation alone.
func TestReviewDeliverableDismiss(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)

	artID, conversationID, _ := seedReviewDeliverableWithConversation(t, srv, "rdis", "acme", "api", 7, "COMMENT")
	rec := doJSON(t, srv, http.MethodPatch, "/api/deliverables/"+artID+"/review", map[string]any{"state": "dismissed"})
	if rec.Code != http.StatusOK {
		t.Fatalf("dismiss = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got := getDeliverable(t, srv, artID).State; got != domain.DeliverableStateReviewDismissed {
		t.Errorf("deliverable state = %q, want dismissed", got)
	}
	if !storedConcluded(t, srv.db, conversationID) {
		t.Error("conversation no longer concluded (dismiss must not flip conversation lifecycle)")
	}
}

// TestReviewDeliverableDismiss_NonPending_409: once a review is submitted, both
// a dismiss and a content edit are 409, and the staged body is unchanged.
func TestReviewDeliverableDismiss_NonPending_409(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)
	artID, _, _ := seedReviewDeliverableWithConversation(t, srv, "rdis409", "acme", "api", 7, "COMMENT")
	execSQL(t, srv.db, `UPDATE deliverables SET state = ? WHERE id = ?`, domain.DeliverableStateReviewSubmitted, artID)
	for _, body := range []map[string]any{{"state": "dismissed"}, {"body": "edit"}} {
		rec := doJSON(t, srv, http.MethodPatch, "/api/deliverables/"+artID+"/review", body)
		if rec.Code != http.StatusConflict {
			t.Errorf("PATCH %v on a submitted review = %d, want 409; body=%s", body, rec.Code, rec.Body.String())
		}
	}
	if d, _ := domain.ParseReviewDeliverableDetails(getDeliverable(t, srv, artID).DetailsJSON); d.ReviewBody != "## Review\nlgtm" {
		t.Errorf("review_body = %q after the rejected PATCHes, want unchanged", d.ReviewBody)
	}
}

// TestReviewDeliverablePatch_StateVocabulary pins what the state field accepts:
// "dismissed" and nothing else, and never alongside a content edit. Submitting
// is still POST /approve — a real GitHub write, not a field.
func TestReviewDeliverablePatch_StateVocabulary(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)
	artID, _, _ := seedReviewDeliverableWithConversation(t, srv, "rstate", "acme", "api", 7, "COMMENT")

	for name, body := range map[string]map[string]any{
		"submitting is not a field write": {"state": "submitted"},
		"unknown state":                   {"state": "banana"},
		"dismiss plus an edit":            {"state": "dismissed", "body": "one more thought"},
		"no fields at all":                {},
	} {
		t.Run(name, func(t *testing.T) {
			rec := doJSON(t, srv, http.MethodPatch, "/api/deliverables/"+artID+"/review", body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
			}
		})
	}
	if got := getDeliverable(t, srv, artID).State; got != domain.DeliverableStateReviewPending {
		t.Errorf("state = %q after the refused patches, want pending (unchanged)", got)
	}
}

// TestReviewDeliverableCommentUpdate_RejectsSeverityInBody: a comment body that
// carries its own severity badge is refused with 422 rather than stripped, and
// the stored severity is unchanged.
func TestReviewDeliverableCommentUpdate_RejectsSeverityInBody(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)
	artID, _, _ := seedReviewDeliverableWithConversation(t, srv, "rsev", "acme", "api", 7, "COMMENT")

	rec := doJSON(t, srv, http.MethodPatch, "/api/deliverables/"+artID+"/comments/c_1", map[string]any{
		"body": domain.SeverityBadgeMarkdown(domain.SeverityMinor) + "rename to fooBar",
	})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("comment update with an embedded badge = %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
	d, _ := domain.ParseReviewDeliverableDetails(getDeliverable(t, srv, artID).DetailsJSON)
	if sev, _ := domain.ParseSeverityBadge(d.StagedComments[0].Body); sev != domain.SeverityMajor {
		t.Errorf("severity = %q after the refused edit, want MAJOR (unchanged)", sev)
	}
}

// TestReviewDeliverableGet_MalformedDetails_500 pins that a corrupt details_json
// surfaces as a 500, not a misleadingly empty review overlay the user might act on.
func TestReviewDeliverableGet_MalformedDetails_500(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)

	artID, _, _ := seedReviewDeliverableWithConversation(t, srv, "rbad", "acme", "api", 7, "COMMENT")
	// Corrupt the details_json out-of-band.
	execSQL(t, srv.db, `UPDATE deliverables SET details_json = ? WHERE id = ?`, `{not valid json`, artID)
	rec := doJSON(t, srv, http.MethodGet, "/api/deliverables/"+artID, nil)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("get with corrupt details = %d, want 500; body=%s", rec.Code, rec.Body.String())
	}
}

// TestReviewDeliverableApprove_EmptyReviewRefused pins the same rule the agent's
// finalize applies, at the human's door: a COMMENT review that lost its last
// inline comment and has no summary is refused with 422 before GitHub is
// called, and the claim is released so the draft is editable again.
func TestReviewDeliverableApprove_EmptyReviewRefused(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)
	mux := newAppAPIMux()
	mux.HandleFunc("POST /api/v3/repos/{owner}/{repo}/pulls/{number}/reviews", func(w http.ResponseWriter, r *http.Request) {
		t.Error("an empty review must not reach GitHub")
		http.Error(w, "unexpected", http.StatusInternalServerError)
	})
	stub := httptest.NewServer(mux)
	t.Cleanup(stub.Close)
	seedApp(t, srv, stub, acmeInstall())

	artID, _, _ := seedReviewDeliverableWithConversation(t, srv, "rempty", "acme", "api", 7, "COMMENT")
	art := getDeliverable(t, srv, artID)
	d, _ := domain.ParseReviewDeliverableDetails(art.DetailsJSON)
	d.ReviewBody = ""
	d.StagedComments = nil
	art.DetailsJSON = domain.MarshalReviewDeliverableDetails(d)
	if _, err := sqlitestore.New(srv.db).Deliverables.UpsertSystem(context.Background(), runmode.LocalDefaultOrgID, dbtest.InOrgScope(t, sqlitestore.New(srv.db).Orgs, runmode.LocalDefaultOrgID, *art)); err != nil {
		t.Fatalf("seed: %v", err)
	}

	rec := doJSON(t, srv, http.MethodPost, "/api/deliverables/"+artID+"/approve", nil)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("approve = %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
	assertFirstError(t, rec, httpx.ReasonInvalidField, "")
	if got := getDeliverable(t, srv, artID); got.State != domain.DeliverableStateReviewPending {
		t.Errorf("state after refused approve = %q, want pending (claim released)", got.State)
	}
}
