package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TestBuildSnapshot_ReviewMarker pins the newest-review marker: the review's
// numeric id comes from fullDatabaseId, which GitHub encodes as a string, and
// the marker is the newest submission among the connection's last node and
// each reviewer's latest review.
func TestBuildSnapshot_ReviewMarker(t *testing.T) {
	const body = `{
	  "number": 9,
	  "repository": {"nameWithOwner": "octo/repo"},
	  "latestReviews": {"nodes": [
	    {"id": "PRR_1", "fullDatabaseId": "5499358193", "author": {"__typename": "User", "login": "alice"}, "state": "APPROVED", "submittedAt": "2026-10-01T11:00:00Z", "comments": {"totalCount": 2}},
	    {"id": "PRR_2", "fullDatabaseId": "5499358100", "author": {"__typename": "User", "login": "bob"}, "state": "COMMENTED", "submittedAt": "2026-10-01T12:00:00Z", "comments": {"totalCount": 0}}
	  ]},
	  "reviews": {"totalCount": 4, "nodes": [{"fullDatabaseId": "5499358193", "submittedAt": "2026-10-01T11:00:00Z"}]}
	}`
	var pr gqlPR
	if err := json.Unmarshal([]byte(body), &pr); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for name, snap := range map[string]domain.PRSnapshot{
		"full":      pr.toSnapshot(),
		"discovery": pr.toDiscoverySnapshot(),
	} {
		if snap.ReviewCount != 4 {
			t.Errorf("%s: ReviewCount = %d, want 4", name, snap.ReviewCount)
		}
		// bob's review was created before alice's (lower id) and submitted
		// after it: the newest submission, though not the connection's last.
		want := domain.ReviewMarker{ID: 5499358100, SubmittedAt: "2026-10-01T12:00:00Z"}
		if snap.ReviewMarker == nil || *snap.ReviewMarker != want {
			t.Errorf("%s: ReviewMarker = %+v, want %+v", name, snap.ReviewMarker, want)
		}
		if got := snap.Reviews[0]; got.DatabaseID != 5499358193 || got.CommentCount != 2 {
			t.Errorf("%s: alice's review = %+v, want database id 5499358193 and 2 comments", name, got)
		}
	}
}

// TestBuildSnapshot_ReviewMarkerWithoutReviews pins that a pull request with no
// submitted review has a zero marker, not a nil one: nil is reserved for a
// snapshot that never read reviews.
func TestBuildSnapshot_ReviewMarkerWithoutReviews(t *testing.T) {
	var pr gqlPR
	if err := json.Unmarshal([]byte(`{"number": 1, "reviews": {"totalCount": 0, "nodes": []}}`), &pr); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if m := pr.toSnapshot().ReviewMarker; m == nil || *m != (domain.ReviewMarker{}) {
		t.Errorf("ReviewMarker = %+v, want a zero marker", m)
	}
}

// TestPRBaseFields_ReadsSubmittedReviewsOnly pins that the batch's review
// count and newest review leave out the viewer's own pending review, and read
// the 64-bit id.
func TestPRBaseFields_ReadsSubmittedReviewsOnly(t *testing.T) {
	if !strings.Contains(prBaseFields, "reviews(last: 1, states: [APPROVED, CHANGES_REQUESTED, COMMENTED, DISMISSED])") {
		t.Errorf("prBaseFields does not read the last submitted review:\n%s", prBaseFields)
	}
	if strings.Contains(prBaseFields, "PENDING") {
		t.Error("prBaseFields reads pending reviews")
	}
	if strings.Contains(prBaseFields, " databaseId") {
		t.Error("prBaseFields reads a review's 32-bit databaseId")
	}
}

// TestPRReviewsBefore pages a pull request's submitted reviews from the end
// backwards: the first read sends no cursor and the second the page's start
// cursor; each review carries its id, state, REST-spelled author, submission
// time, comment count and the comments' ids and files.
func TestPRReviewsBefore(t *testing.T) {
	var befores []any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
		}
		if !strings.Contains(req.Query, "author { __typename login }") {
			t.Errorf("query reads an author without __typename:\n%s", req.Query)
		}
		if req.Variables["id"] != "PR_node" {
			t.Errorf("id = %v, want PR_node", req.Variables["id"])
		}
		befores = append(befores, req.Variables["before"])
		if req.Variables["before"] == nil {
			_, _ = w.Write([]byte(`{"data":{"node":{"reviews":{
			  "totalCount": 3,
			  "pageInfo": {"hasPreviousPage": true, "startCursor": "CUR_2"},
			  "nodes": [
			    {"fullDatabaseId": "6000000002", "state": "COMMENTED", "submittedAt": "2026-10-02T10:00:00Z",
			     "author": {"__typename": "Bot", "login": "copilot-pull-request-reviewer"},
			     "comments": {"totalCount": 2, "nodes": [{"fullDatabaseId": "4300000001", "path": "a.go"}, {"fullDatabaseId": "4300000002", "path": "b.go"}]}},
			    {"fullDatabaseId": "6000000003", "state": "COMMENTED", "submittedAt": "2026-10-02T11:00:00Z",
			     "author": {"__typename": "User", "login": "alice"},
			     "comments": {"totalCount": 1, "nodes": [{"fullDatabaseId": "4300000003", "path": "a.go"}]}}
			  ]}}}}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":{"node":{"reviews":{
		  "totalCount": 3,
		  "pageInfo": {"hasPreviousPage": false, "startCursor": "CUR_1"},
		  "nodes": [
		    {"fullDatabaseId": "6000000001", "state": "PENDING", "submittedAt": null, "author": {"login": "me"}, "comments": {"totalCount": 0, "nodes": []}},
		    {"fullDatabaseId": "6000000000", "state": "APPROVED", "submittedAt": "2026-10-01T10:00:00Z", "author": {"login": "bob"}, "comments": {"totalCount": 0, "nodes": []}}
		  ]}}}}`))
	}))
	t.Cleanup(srv.Close)
	client := NewClient(srv.URL, "tok")
	ctx := context.Background()

	last, err := client.PRReviewsBefore(ctx, "PR_node", "")
	if err != nil {
		t.Fatalf("PRReviewsBefore(last page): %v", err)
	}
	if last.TotalCount != 3 || !last.HasPrevious || last.Before != "CUR_2" || len(last.Reviews) != 2 {
		t.Fatalf("last page = %+v", last)
	}
	bot := last.Reviews[0]
	if bot.ID != 6000000002 || bot.State != "COMMENTED" || bot.Author != "copilot-pull-request-reviewer[bot]" ||
		bot.SubmittedAt != "2026-10-02T10:00:00Z" || bot.CommentCount != 2 {
		t.Errorf("bot review = %+v", bot)
	}
	if len(bot.Comments) != 2 || bot.Comments[1] != (PRReviewComment{ID: 4300000002, Path: "b.go"}) {
		t.Errorf("bot review comments = %+v", bot.Comments)
	}

	first, err := client.PRReviewsBefore(ctx, "PR_node", last.Before)
	if err != nil {
		t.Fatalf("PRReviewsBefore(CUR_2): %v", err)
	}
	if first.HasPrevious || len(first.Reviews) != 1 || first.Reviews[0].ID != 6000000000 {
		t.Errorf("first page = %+v, want bob's review alone (the pending one skipped)", first)
	}
	if len(befores) != 2 || befores[0] != nil || befores[1] != "CUR_2" {
		t.Errorf("before cursors sent = %v, want [nil CUR_2]", befores)
	}
}

// TestPRReviewsBefore_MissingPullRequest pins that a node GitHub cannot
// resolve is an error, not an empty page that would read as "no new reviews".
func TestPRReviewsBefore_MissingPullRequest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":{"node":null}}`))
	}))
	t.Cleanup(srv.Close)
	if _, err := NewClient(srv.URL, "tok").PRReviewsBefore(context.Background(), "PR_gone", ""); err == nil {
		t.Error("PRReviewsBefore on a missing pull request returned no error")
	}
}

// TestGQLBigInt_Decode pins that an id decodes from GitHub's string encoding
// and from a JSON number, and that null or an unparseable value decodes as
// zero without failing the response around it.
func TestGQLBigInt_Decode(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want gqlBigInt
	}{
		{`"5499358193"`, 5499358193},
		{`5499358193`, 5499358193},
		{`null`, 0},
		{`"not-a-number"`, 0},
	} {
		var got struct {
			ID gqlBigInt `json:"id"`
		}
		got.ID = 7
		if err := json.Unmarshal([]byte(`{"id": `+tc.in+`}`), &got); err != nil {
			t.Errorf("decode %s: %v", tc.in, err)
			continue
		}
		if got.ID != tc.want {
			t.Errorf("decode %s = %d, want %d", tc.in, got.ID, tc.want)
		}
	}
}
