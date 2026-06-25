package github

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// gqlCapture records the decoded GraphQL request body for assertions.
type gqlCapture struct {
	query     string
	variables map[string]any
}

// graphqlCapturing starts a test server that records the GraphQL request body
// and replies with respBody verbatim. Returns a client pointed at it plus the
// capture, which is populated once the client call returns. (graphqlURL maps a
// non-api.github.com base to "<base>/graphql"; the handler answers any path.)
func graphqlCapturing(t *testing.T, respBody string) (*Client, *gqlCapture) {
	t.Helper()
	rec := &gqlCapture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		rec.query = req.Query
		rec.variables = req.Variables
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(respBody))
	}))
	t.Cleanup(srv.Close)
	return clientAgainst(srv.URL), rec
}

// TestCreatePendingReview_OmitsEventField pins the defining property of a
// pending review: the REST create call carries commit_id and seeded comments
// but NO event field — that omission is what leaves the review PENDING instead
// of submitting it. Also asserts the GraphQL node_id is returned (the currency
// the incremental pending-review ops speak) and the multi-line comment keeps
// its start_line.
func TestCreatePendingReview_OmitsEventField(t *testing.T) {
	var (
		gotPath string
		gotBody map[string]any
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id": 555, "node_id": "PRR_node555"}`))
	}))
	t.Cleanup(srv.Close)

	start := 10
	reviewID, err := clientAgainst(srv.URL).CreatePendingReview("owner", "repo", 7, "abc123", []SubmitReviewComment{
		{Path: "main.go", Line: 12, Body: "nit"},
		{Path: "main.go", Line: 20, StartLine: &start, Body: "range"},
	})
	if err != nil {
		t.Fatalf("CreatePendingReview: %v", err)
	}
	if reviewID != "PRR_node555" {
		t.Errorf("reviewID = %q, want node_id PRR_node555", reviewID)
	}
	if gotPath != "/repos/owner/repo/pulls/7/reviews" {
		t.Errorf("path = %q, want /repos/owner/repo/pulls/7/reviews", gotPath)
	}
	if _, ok := gotBody["event"]; ok {
		t.Error("body must NOT contain an event field (that omission is what keeps the review pending)")
	}
	if got, _ := gotBody["commit_id"].(string); got != "abc123" {
		t.Errorf("commit_id = %q, want abc123", got)
	}
	comments, ok := gotBody["comments"].([]any)
	if !ok || len(comments) != 2 {
		t.Fatalf("comments = %v, want 2 seeded comments", gotBody["comments"])
	}
	second, _ := comments[1].(map[string]any)
	if _, ok := second["start_line"]; !ok {
		t.Errorf("second (multi-line) comment missing start_line; got %v", second)
	}
}

// TestCreatePendingReview_OmitsEmptyCommitAndComments confirms an empty
// commitSHA and a nil comment slice produce a clean minimal body — neither key
// is sent (GitHub rejects an empty comments array, and an empty commit_id).
func TestCreatePendingReview_OmitsEmptyCommitAndComments(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"node_id": "PRR_x"}`))
	}))
	t.Cleanup(srv.Close)

	if _, err := clientAgainst(srv.URL).CreatePendingReview("o", "r", 1, "", nil); err != nil {
		t.Fatalf("CreatePendingReview: %v", err)
	}
	if _, ok := gotBody["commit_id"]; ok {
		t.Error("commit_id should be omitted when commitSHA is empty")
	}
	if _, ok := gotBody["comments"]; ok {
		t.Error("comments should be omitted when none are provided")
	}
}

// TestCreatePendingReview_DuplicateError maps GitHub's "one pending review per
// PR" 422 to a friendly message that points the caller (B·3's collision check)
// at GetPendingReview instead of leaking the raw envelope.
func TestCreatePendingReview_DuplicateError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"message":"Unprocessable Entity","errors":[{"message":"User can only have one pending review per pull request"}]}`))
	}))
	t.Cleanup(srv.Close)

	_, err := clientAgainst(srv.URL).CreatePendingReview("o", "r", 1, "", nil)
	if err == nil {
		t.Fatal("expected error for duplicate pending review, got nil")
	}
	if !strings.Contains(err.Error(), "GetPendingReview") {
		t.Errorf("expected friendly collision message pointing at GetPendingReview, got %q", err.Error())
	}
}

// TestCreatePendingReview_MissingNodeID guards the contract that a successful
// create must yield a node_id — without it none of the GraphQL ops can address
// the review, so an empty one is an error, not a silent "".
func TestCreatePendingReview_MissingNodeID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id": 1}`))
	}))
	t.Cleanup(srv.Close)

	if _, err := clientAgainst(srv.URL).CreatePendingReview("o", "r", 1, "", nil); err == nil {
		t.Fatal("expected error when response has no node_id")
	}
}

// TestAddPendingReviewComment_SingleLine asserts the GraphQL mutation targets
// addPullRequestReviewThread with the review id, path, and line, anchored on
// the RIGHT side, and returns the new comment's node id. A single-line comment
// must NOT send a startLine variable.
func TestAddPendingReviewComment_SingleLine(t *testing.T) {
	resp := `{"data":{"addPullRequestReviewThread":{"thread":{"comments":{"nodes":[{"id":"PRRC_new"}]}}}}}`
	c, rec := graphqlCapturing(t, resp)

	id, err := c.AddPendingReviewComment("PRR_1", SubmitReviewComment{Path: "a.go", Line: 5, Body: "hi"})
	if err != nil {
		t.Fatalf("AddPendingReviewComment: %v", err)
	}
	if id != "PRRC_new" {
		t.Errorf("comment id = %q, want PRRC_new", id)
	}
	if rec.variables["reviewId"] != "PRR_1" {
		t.Errorf("reviewId var = %v, want PRR_1", rec.variables["reviewId"])
	}
	if rec.variables["path"] != "a.go" {
		t.Errorf("path var = %v, want a.go", rec.variables["path"])
	}
	if rec.variables["line"] != float64(5) {
		t.Errorf("line var = %v, want 5", rec.variables["line"])
	}
	if _, ok := rec.variables["startLine"]; ok {
		t.Error("single-line comment should not send a startLine variable")
	}
	if !strings.Contains(rec.query, "addPullRequestReviewThread") {
		t.Errorf("query should call addPullRequestReviewThread; got %q", rec.query)
	}
	if !strings.Contains(rec.query, "side: RIGHT") {
		t.Errorf("query should anchor on the RIGHT side; got %q", rec.query)
	}
}

// TestAddPendingReviewComment_MultiLine asserts a non-nil StartLine adds the
// startLine variable and startSide to the mutation (a multi-line range).
func TestAddPendingReviewComment_MultiLine(t *testing.T) {
	resp := `{"data":{"addPullRequestReviewThread":{"thread":{"comments":{"nodes":[{"id":"PRRC_ml"}]}}}}}`
	c, rec := graphqlCapturing(t, resp)

	start := 3
	id, err := c.AddPendingReviewComment("PRR_1", SubmitReviewComment{Path: "a.go", Line: 8, StartLine: &start, Body: "range"})
	if err != nil {
		t.Fatalf("AddPendingReviewComment: %v", err)
	}
	if id != "PRRC_ml" {
		t.Errorf("comment id = %q, want PRRC_ml", id)
	}
	if rec.variables["startLine"] != float64(3) {
		t.Errorf("startLine var = %v, want 3", rec.variables["startLine"])
	}
	if !strings.Contains(rec.query, "startSide: RIGHT") {
		t.Errorf("multi-line query should set startSide; got %q", rec.query)
	}
}

// TestAddPendingReviewComment_EmptyReviewID guards the empty-id precondition —
// it must fail fast, before any network call.
func TestAddPendingReviewComment_EmptyReviewID(t *testing.T) {
	if _, err := clientAgainst("http://invalid.invalid").AddPendingReviewComment("", SubmitReviewComment{Path: "a.go", Line: 1, Body: "x"}); err == nil {
		t.Fatal("expected error for empty review id")
	}
}

// TestAddPendingReviewComment_Guards pins the fail-fast preconditions for the
// required line-thread fields, so a missing path/body/line yields a pointed
// error before any network call rather than a vague GraphQL rejection.
func TestAddPendingReviewComment_Guards(t *testing.T) {
	c := clientAgainst("http://invalid.invalid")
	cases := map[string]SubmitReviewComment{
		"empty path": {Path: "", Line: 1, Body: "x"},
		"empty body": {Path: "a.go", Line: 1, Body: ""},
		"zero line":  {Path: "a.go", Line: 0, Body: "x"},
		"neg line":   {Path: "a.go", Line: -3, Body: "x"},
	}
	for name, cm := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := c.AddPendingReviewComment("PRR_1", cm); err == nil {
				t.Fatalf("expected error for %s", name)
			}
		})
	}
}

// TestAddPendingReviewComment_NoCommentReturned treats a thread payload with no
// comment node as an error — the caller needs the comment id for later edits.
func TestAddPendingReviewComment_NoCommentReturned(t *testing.T) {
	resp := `{"data":{"addPullRequestReviewThread":{"thread":{"comments":{"nodes":[]}}}}}`
	c, _ := graphqlCapturing(t, resp)
	if _, err := c.AddPendingReviewComment("PRR_1", SubmitReviewComment{Path: "a.go", Line: 1, Body: "x"}); err == nil {
		t.Fatal("expected error when no comment id is returned")
	}
}

// TestUpdatePendingReviewComment asserts the body edit goes through
// updatePullRequestReviewComment keyed on the comment node id via the
// pullRequestReviewCommentId input field.
func TestUpdatePendingReviewComment(t *testing.T) {
	resp := `{"data":{"updatePullRequestReviewComment":{"pullRequestReviewComment":{"id":"PRRC_1"}}}}`
	c, rec := graphqlCapturing(t, resp)

	if err := c.UpdatePendingReviewComment("PRRC_1", "edited"); err != nil {
		t.Fatalf("UpdatePendingReviewComment: %v", err)
	}
	if rec.variables["id"] != "PRRC_1" {
		t.Errorf("id var = %v, want PRRC_1", rec.variables["id"])
	}
	if rec.variables["body"] != "edited" {
		t.Errorf("body var = %v, want edited", rec.variables["body"])
	}
	if !strings.Contains(rec.query, "pullRequestReviewCommentId") {
		t.Errorf("update should use the pullRequestReviewCommentId input field; got %q", rec.query)
	}
}

func TestUpdatePendingReviewComment_EmptyID(t *testing.T) {
	if err := clientAgainst("http://invalid.invalid").UpdatePendingReviewComment("", "x"); err == nil {
		t.Fatal("expected error for empty comment id")
	}
}

// TestDeletePendingReviewComment asserts delete keys on the comment node id and
// uses the bare `id` input field — GitHub's schema is asymmetric, so this must
// NOT use pullRequestReviewCommentId like the update mutation does.
func TestDeletePendingReviewComment(t *testing.T) {
	resp := `{"data":{"deletePullRequestReviewComment":{"pullRequestReviewComment":{"id":"PRRC_1"}}}}`
	c, rec := graphqlCapturing(t, resp)

	if err := c.DeletePendingReviewComment("PRRC_1"); err != nil {
		t.Fatalf("DeletePendingReviewComment: %v", err)
	}
	if rec.variables["id"] != "PRRC_1" {
		t.Errorf("id var = %v, want PRRC_1", rec.variables["id"])
	}
	if !strings.Contains(rec.query, "deletePullRequestReviewComment(input: {id: $id})") {
		t.Errorf("delete should use the bare id input field; got %q", rec.query)
	}
}

func TestDeletePendingReviewComment_EmptyID(t *testing.T) {
	if err := clientAgainst("http://invalid.invalid").DeletePendingReviewComment(""); err == nil {
		t.Fatal("expected error for empty comment id")
	}
}

// TestDeletePendingReview pins the two-step delete: GraphQL resolves the review
// node id to its numeric databaseId, then a REST DELETE keyed on that database id
// (NOT the node id) drops the pending review. GraphQL has no delete-review
// mutation, so REST is the only path and it needs the numeric id.
func TestDeletePendingReview(t *testing.T) {
	var (
		gqlVars    map[string]any
		deleteHit  bool
		deletePath string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/graphql") {
			var req struct {
				Variables map[string]any `json:"variables"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			gqlVars = req.Variables
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":{"node":{"databaseId":98765}}}`))
			return
		}
		if r.Method == http.MethodDelete {
			deleteHit = true
			deletePath = r.URL.Path
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)

	if err := clientAgainst(srv.URL).DeletePendingReview("owner", "repo", 7, "PRR_node1"); err != nil {
		t.Fatalf("DeletePendingReview: %v", err)
	}
	if gqlVars["id"] != "PRR_node1" {
		t.Errorf("graphql node id var = %v, want PRR_node1", gqlVars["id"])
	}
	if !deleteHit {
		t.Fatal("expected a REST DELETE call after resolving the database id")
	}
	if deletePath != "/repos/owner/repo/pulls/7/reviews/98765" {
		t.Errorf("delete path = %q, want /repos/owner/repo/pulls/7/reviews/98765 (numeric database id)", deletePath)
	}
}

func TestDeletePendingReview_EmptyID(t *testing.T) {
	if err := clientAgainst("http://invalid.invalid").DeletePendingReview("o", "r", 1, ""); err == nil {
		t.Fatal("expected error for empty review id")
	}
}

// TestDeletePendingReview_NoDatabaseID guards the "already gone" case: a node
// lookup that resolves nothing must error rather than issue a DELETE against
// review id 0.
func TestDeletePendingReview_NoDatabaseID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			t.Errorf("must not DELETE when no database id resolved; hit %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"node":{}}}`))
	}))
	t.Cleanup(srv.Close)

	if err := clientAgainst(srv.URL).DeletePendingReview("o", "r", 1, "PRR_gone"); err == nil {
		t.Fatal("expected error when the review node has no database id")
	}
}

// TestSubmitExistingReview asserts the submit goes through
// submitPullRequestReview with the review node id, event, and body — the
// "approve the preview" step that flips a pending review public.
func TestSubmitExistingReview(t *testing.T) {
	resp := `{"data":{"submitPullRequestReview":{"pullRequestReview":{"id":"PRR_1","state":"APPROVED"}}}}`
	c, rec := graphqlCapturing(t, resp)

	if err := c.SubmitExistingReview("o", "r", "PRR_1", "APPROVE", "lgtm"); err != nil {
		t.Fatalf("SubmitExistingReview: %v", err)
	}
	if rec.variables["reviewId"] != "PRR_1" {
		t.Errorf("reviewId var = %v, want PRR_1", rec.variables["reviewId"])
	}
	if rec.variables["event"] != "APPROVE" {
		t.Errorf("event var = %v, want APPROVE", rec.variables["event"])
	}
	if rec.variables["body"] != "lgtm" {
		t.Errorf("body var = %v, want lgtm", rec.variables["body"])
	}
	if !strings.Contains(rec.query, "submitPullRequestReview") {
		t.Errorf("query should call submitPullRequestReview; got %q", rec.query)
	}
}

// TestSubmitExistingReview_Guards covers the fail-fast preconditions.
func TestSubmitExistingReview_Guards(t *testing.T) {
	c := clientAgainst("http://invalid.invalid")
	if err := c.SubmitExistingReview("o", "r", "", "APPROVE", ""); err == nil {
		t.Error("expected error for empty review id")
	}
	if err := c.SubmitExistingReview("o", "r", "PRR_1", "", ""); err == nil {
		t.Error("expected error for empty event")
	}
}

// TestSubmitExistingReview_EmptyBodyPassedThrough pins that an empty body (the
// documented "optional body" case — submit with no top-level message) is sent
// as "" rather than dropped, since the mutation's $body is a nullable String.
func TestSubmitExistingReview_EmptyBodyPassedThrough(t *testing.T) {
	resp := `{"data":{"submitPullRequestReview":{"pullRequestReview":{"id":"PRR_1","state":"COMMENTED"}}}}`
	c, rec := graphqlCapturing(t, resp)

	if err := c.SubmitExistingReview("o", "r", "PRR_1", "COMMENT", ""); err != nil {
		t.Fatalf("SubmitExistingReview: %v", err)
	}
	body, ok := rec.variables["body"]
	if !ok {
		t.Fatal("body variable should be present (sent as empty string), not omitted")
	}
	if body != "" {
		t.Errorf("body var = %v, want empty string", body)
	}
}

// TestGetPendingReview_Found returns the viewer's pending review id and its
// inline comments, mapping line/startLine and the per-comment node ids the sync
// needs for edit/delete. The third comment pins the nullable-anchor contract: a
// null line maps to a nil Line pointer (an outdated/unpositioned comment),
// distinct from a real line and never silently collapsed to 0.
func TestGetPendingReview_Found(t *testing.T) {
	resp := `{"data":{"repository":{"pullRequest":{"reviews":{"nodes":[
		{"id":"PRR_mine","viewerDidAuthor":true,"comments":{"nodes":[
			{"id":"PRRC_1","path":"a.go","line":5,"startLine":null,"body":"one"},
			{"id":"PRRC_2","path":"b.go","line":20,"startLine":15,"body":"two"},
			{"id":"PRRC_3","path":"c.go","line":null,"startLine":null,"body":"outdated"}
		]}}
	]}}}}}`
	c, rec := graphqlCapturing(t, resp)

	id, comments, err := c.GetPendingReview("owner", "repo", 7)
	if err != nil {
		t.Fatalf("GetPendingReview: %v", err)
	}
	if id != "PRR_mine" {
		t.Errorf("reviewID = %q, want PRR_mine", id)
	}
	if rec.variables["number"] != float64(7) {
		t.Errorf("number var = %v, want 7", rec.variables["number"])
	}
	if len(comments) != 3 {
		t.Fatalf("comments = %+v, want 3", comments)
	}
	if comments[0].ID != "PRRC_1" || comments[0].Path != "a.go" || comments[0].Line == nil || *comments[0].Line != 5 {
		t.Errorf("comment[0] = %+v, want id PRRC_1 path a.go line 5", comments[0])
	}
	if comments[0].StartLine != nil {
		t.Errorf("comment[0].StartLine = %v, want nil (single-line)", *comments[0].StartLine)
	}
	if comments[1].StartLine == nil || *comments[1].StartLine != 15 {
		t.Errorf("comment[1].StartLine = %v, want 15", comments[1].StartLine)
	}
	// Null line → nil pointer, not 0: the outdated/unpositioned case stays
	// distinguishable from a comment that really anchors at line 0.
	if comments[2].Line != nil {
		t.Errorf("comment[2].Line = %v, want nil for a null (unpositioned) line", *comments[2].Line)
	}
}

// TestGetPendingReview_None treats every "nothing for this viewer" shape as a
// normal empty result ("", nil, nil), never an error — that is the signal B·3's
// collision check uses to decide to create a fresh pending review.
func TestGetPendingReview_None(t *testing.T) {
	cases := map[string]string{
		"no pending reviews":  `{"data":{"repository":{"pullRequest":{"reviews":{"nodes":[]}}}}}`,
		"not viewer-authored": `{"data":{"repository":{"pullRequest":{"reviews":{"nodes":[{"id":"PRR_other","viewerDidAuthor":false,"comments":{"nodes":[]}}]}}}}}`,
		"null pull request":   `{"data":{"repository":{"pullRequest":null}}}`,
		"null repository":     `{"data":{"repository":null}}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			c, _ := graphqlCapturing(t, body)
			id, comments, err := c.GetPendingReview("o", "r", 1)
			if err != nil {
				t.Fatalf("GetPendingReview: %v", err)
			}
			if id != "" || comments != nil {
				t.Errorf("want empty result; got id=%q comments=%+v", id, comments)
			}
		})
	}
}

// TestGetPendingReview_GraphQLError confirms a genuine GraphQL failure (null
// data + errors[]) propagates rather than being swallowed as "no review".
func TestGetPendingReview_GraphQLError(t *testing.T) {
	body := `{"data":null,"errors":[{"type":"NOT_FOUND","message":"Could not resolve to a Repository"}]}`
	c, _ := graphqlCapturing(t, body)
	if _, _, err := c.GetPendingReview("o", "r", 1); err == nil {
		t.Fatal("expected error to propagate from GraphQL errors[]")
	}
}

// TestGetPendingReview_PartialError covers the subtler shape PostGraphQL passes
// through: usable (non-null) data alongside errors[] — here a FORBIDDEN scoped
// to the reviews field, leaving empty nodes under a non-null pullRequest. The
// error must propagate, NOT read as "no pending review", so B·3's collision
// check can't create a duplicate on top of an access failure.
func TestGetPendingReview_PartialError(t *testing.T) {
	body := `{"data":{"repository":{"pullRequest":{"reviews":{"nodes":[]}}}},"errors":[{"type":"FORBIDDEN","message":"Resource not accessible by integration"}]}`
	c, _ := graphqlCapturing(t, body)
	id, comments, err := c.GetPendingReview("o", "r", 1)
	if err == nil {
		t.Fatalf("expected error to propagate from a partial GraphQL error; got id=%q comments=%+v", id, comments)
	}
	if !strings.Contains(err.Error(), "FORBIDDEN") {
		t.Errorf("error should surface the GraphQL error type; got %q", err.Error())
	}
}
