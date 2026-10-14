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

// TestBuildSnapshot_CommentMarker pins the newest-comment marker: the
// comment's numeric id comes from fullDatabaseId, which GitHub encodes as a
// string, and both snapshot forms carry it beside the count.
func TestBuildSnapshot_CommentMarker(t *testing.T) {
	const body = `{
	  "number": 9,
	  "repository": {"nameWithOwner": "octo/repo"},
	  "comments": {"totalCount": 12, "nodes": [{"fullDatabaseId": "3300000042", "createdAt": "2026-10-01T11:00:00Z"}]}
	}`
	var pr gqlPR
	if err := json.Unmarshal([]byte(body), &pr); err != nil {
		t.Fatalf("decode: %v", err)
	}
	want := domain.CommentMarker{ID: 3300000042, CreatedAt: "2026-10-01T11:00:00Z"}
	for name, snap := range map[string]domain.PRSnapshot{
		"full":      pr.toSnapshot(),
		"discovery": pr.toDiscoverySnapshot(),
	} {
		if snap.CommentCount != 12 {
			t.Errorf("%s: CommentCount = %d, want 12", name, snap.CommentCount)
		}
		if snap.CommentMarker == nil || *snap.CommentMarker != want {
			t.Errorf("%s: CommentMarker = %+v, want %+v", name, snap.CommentMarker, want)
		}
	}
}

// TestBuildSnapshot_CommentMarkerWithoutComments pins that a pull request with
// no comment has a zero marker, not a nil one: nil is reserved for a snapshot
// that never read comments.
func TestBuildSnapshot_CommentMarkerWithoutComments(t *testing.T) {
	var pr gqlPR
	if err := json.Unmarshal([]byte(`{"number": 1, "comments": {"totalCount": 0, "nodes": []}}`), &pr); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if m := pr.toSnapshot().CommentMarker; m == nil || *m != (domain.CommentMarker{}) {
		t.Errorf("CommentMarker = %+v, want a zero marker", m)
	}
}

// TestPRFragments_ReadNewestComment pins that both fragments read the newest
// comment with its 64-bit id, beside the count.
func TestPRFragments_ReadNewestComment(t *testing.T) {
	for name, fragment := range map[string]string{"discovery": prDiscoveryFragment, "full": prFullFragment} {
		if !strings.Contains(fragment, "comments(last: 1) {\n\t\ttotalCount\n\t\tnodes { fullDatabaseId createdAt }") {
			t.Errorf("%s fragment does not read the newest comment:\n%s", name, fragment)
		}
	}
}

// TestPRCommentsBefore pages a pull request's conversation comments from the
// end backwards: the first read sends no cursor and the second the page's
// start cursor; each comment carries its id, REST-spelled author and creation
// time.
func TestPRCommentsBefore(t *testing.T) {
	var befores []any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
		}
		if !strings.Contains(req.Query, "author { __typename login }") || !strings.Contains(req.Query, "fullDatabaseId") {
			t.Errorf("query does not read the author's type or the 64-bit id:\n%s", req.Query)
		}
		if req.Variables["id"] != "PR_node" {
			t.Errorf("id = %v, want PR_node", req.Variables["id"])
		}
		befores = append(befores, req.Variables["before"])
		if req.Variables["before"] == nil {
			_, _ = w.Write([]byte(`{"data":{"node":{"comments":{
			  "totalCount": 3,
			  "pageInfo": {"hasPreviousPage": true, "startCursor": "CUR_2"},
			  "nodes": [
			    {"fullDatabaseId": "3300000002", "createdAt": "2026-10-02T10:00:00Z", "author": {"__typename": "Bot", "login": "dependabot"}},
			    {"fullDatabaseId": "3300000003", "createdAt": "2026-10-02T11:00:00Z", "author": {"__typename": "User", "login": "alice"}}
			  ]}}}}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":{"node":{"comments":{
		  "totalCount": 3,
		  "pageInfo": {"hasPreviousPage": false, "startCursor": "CUR_1"},
		  "nodes": [{"fullDatabaseId": "3300000001", "createdAt": "2026-10-01T10:00:00Z", "author": null}]}}}}`))
	}))
	t.Cleanup(srv.Close)
	client := NewClient(srv.URL, "tok")
	ctx := context.Background()

	last, err := client.PRCommentsBefore(ctx, "PR_node", "")
	if err != nil {
		t.Fatalf("PRCommentsBefore(last page): %v", err)
	}
	if last.TotalCount != 3 || !last.HasPrevious || last.Before != "CUR_2" || len(last.Comments) != 2 {
		t.Fatalf("last page = %+v", last)
	}
	if got := last.Comments[0]; got != (PRComment{ID: 3300000002, Author: "dependabot[bot]", CreatedAt: "2026-10-02T10:00:00Z"}) {
		t.Errorf("bot comment = %+v", got)
	}
	if got := last.Comments[1]; got != (PRComment{ID: 3300000003, Author: "alice", CreatedAt: "2026-10-02T11:00:00Z"}) {
		t.Errorf("alice's comment = %+v", got)
	}

	first, err := client.PRCommentsBefore(ctx, "PR_node", last.Before)
	if err != nil {
		t.Fatalf("PRCommentsBefore(CUR_2): %v", err)
	}
	if first.HasPrevious || len(first.Comments) != 1 || first.Comments[0] != (PRComment{ID: 3300000001, CreatedAt: "2026-10-01T10:00:00Z"}) {
		t.Errorf("first page = %+v, want the authorless comment alone", first)
	}
	if len(befores) != 2 || befores[0] != nil || befores[1] != "CUR_2" {
		t.Errorf("before cursors sent = %v, want [nil CUR_2]", befores)
	}
}

// TestPRCommentsBefore_MissingPullRequest pins that a node GitHub cannot
// resolve is an error, not an empty page that would read as "no new comments".
func TestPRCommentsBefore_MissingPullRequest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":{"node":null}}`))
	}))
	t.Cleanup(srv.Close)
	if _, err := NewClient(srv.URL, "tok").PRCommentsBefore(context.Background(), "PR_gone", ""); err == nil {
		t.Error("PRCommentsBefore on a missing pull request returned no error")
	}
}
