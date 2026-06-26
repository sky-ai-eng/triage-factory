package github

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// testClient builds a Client pointed straight at an httptest server URL.
// baseURL is used verbatim (no APIBase rewrite) since we set the field
// directly — the same shortcut the download tests use.
func testClient(url string) *Client {
	return &Client{baseURL: url, pat: "test-token", http: &http.Client{Timeout: 5 * time.Second}}
}

// TestGetConditional_NotModified pins the 304 path: an If-None-Match that
// matches the server's ETag yields notModified=true with no body, and the
// header is actually sent.
func TestGetConditional_NotModified(t *testing.T) {
	var sawINM string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawINM = r.Header.Get("If-None-Match")
		if r.Header.Get("If-None-Match") == `"abc123"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `"abc123"`)
		_, _ = w.Write([]byte(`[]`))
	}))
	t.Cleanup(srv.Close)

	c := testClient(srv.URL)
	body, etag, notModified, err := c.GetConditional(context.Background(), "/x", `"abc123"`)
	if err != nil {
		t.Fatalf("GetConditional: %v", err)
	}
	if !notModified {
		t.Errorf("notModified = false; want true on matching ETag")
	}
	if body != nil || etag != "" {
		t.Errorf("304 returned body=%q etag=%q; want both empty", body, etag)
	}
	if sawINM != `"abc123"` {
		t.Errorf("If-None-Match sent = %q; want %q", sawINM, `"abc123"`)
	}
}

// TestGetConditional_OK pins the 200 path: the response ETag is surfaced for
// the caller to store and replay.
func TestGetConditional_OK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"fresh99"`)
		_, _ = w.Write([]byte(`[]`))
	}))
	t.Cleanup(srv.Close)

	c := testClient(srv.URL)
	body, etag, notModified, err := c.GetConditional(context.Background(), "/x", "")
	if err != nil {
		t.Fatalf("GetConditional: %v", err)
	}
	if notModified {
		t.Errorf("notModified = true; want false on 200")
	}
	if etag != `"fresh99"` {
		t.Errorf("etag = %q; want %q", etag, `"fresh99"`)
	}
	if string(body) != `[]` {
		t.Errorf("body = %q; want []", body)
	}
}

// TestListOpenPRs_NotModified pins the acceptance "a quiet repo returns 304
// and is skipped": one HTTP call, notModified=true, no pagination follow-up,
// no new ETag.
func TestListOpenPRs_NotModified(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusNotModified)
	}))
	t.Cleanup(srv.Close)

	c := testClient(srv.URL)
	prs, etag, notModified, err := c.ListOpenPRs(context.Background(), "octo", "repo", `"prev"`)
	if err != nil {
		t.Fatalf("ListOpenPRs: %v", err)
	}
	if !notModified || prs != nil || etag != "" {
		t.Errorf("304: got prs=%v etag=%q notModified=%v; want nil/empty/true", prs, etag, notModified)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("made %d HTTP calls on a 304; want exactly 1 (no pagination)", got)
	}
}

// TestListOpenPRs_OK pins the acceptance "a repo with a new PR returns 200
// and the PR becomes an entity": the REST PR object's node_id + fields are
// mapped into a DiscoveredPR/discovery snapshot the two-phase tracker
// consumes, and the fresh ETag is returned.
func TestListOpenPRs_OK(t *testing.T) {
	const body = `[
	  {
	    "number": 42,
	    "node_id": "PR_kwDOabc",
	    "title": "Add widget",
	    "state": "open",
	    "draft": false,
	    "html_url": "https://github.com/octo/repo/pull/42",
	    "created_at": "2026-05-20T10:00:00Z",
	    "updated_at": "2026-05-29T12:00:00Z",
	    "user": {"login": "alice"},
	    "head": {"sha": "deadbeef", "ref": "feature", "repo": {"full_name": "octo/repo"}},
	    "base": {"ref": "main", "repo": {"full_name": "octo/repo"}},
	    "requested_reviewers": [{"login": "bob"}],
	    "requested_teams": [{"slug": "platform"}],
	    "labels": [{"name": "enhancement"}]
	  }
	]`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("state"); got != "open" {
			t.Errorf("state query = %q; want open", got)
		}
		w.Header().Set("ETag", `"new-etag"`)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)

	c := testClient(srv.URL)
	prs, etag, notModified, err := c.ListOpenPRs(context.Background(), "octo", "repo", "")
	if err != nil {
		t.Fatalf("ListOpenPRs: %v", err)
	}
	if notModified {
		t.Fatalf("notModified = true; want false on 200")
	}
	if etag != `"new-etag"` {
		t.Errorf("etag = %q; want %q", etag, `"new-etag"`)
	}
	if len(prs) != 1 {
		t.Fatalf("got %d PRs; want 1", len(prs))
	}
	got := prs[0]
	if got.NodeID != "PR_kwDOabc" {
		t.Errorf("NodeID = %q; want PR_kwDOabc (must feed Phase-2 RefreshPRs)", got.NodeID)
	}
	snap := got.Snapshot
	if snap.Number != 42 || snap.Title != "Add widget" || snap.Author != "alice" {
		t.Errorf("snapshot identity wrong: %+v", snap)
	}
	if snap.Repo != "octo/repo" {
		t.Errorf("Repo = %q; want octo/repo", snap.Repo)
	}
	if snap.State != "OPEN" {
		t.Errorf("State = %q; want OPEN (REST 'open' uppercased to GraphQL form)", snap.State)
	}
	if snap.HeadSHA != "deadbeef" || snap.UpdatedAt != "2026-05-29T12:00:00Z" {
		t.Errorf("gate fields wrong: HeadSHA=%q UpdatedAt=%q", snap.HeadSHA, snap.UpdatedAt)
	}
	if snap.NodeID != "PR_kwDOabc" {
		t.Errorf("snapshot NodeID = %q; want it carried for entity refresh", snap.NodeID)
	}
	// Review requests: user login direct + team as "owner/slug".
	wantRR := map[string]bool{"bob": true, "octo/platform": true}
	if len(snap.ReviewRequests) != 2 {
		t.Fatalf("ReviewRequests = %v; want bob + octo/platform", snap.ReviewRequests)
	}
	for _, rr := range snap.ReviewRequests {
		if !wantRR[rr] {
			t.Errorf("unexpected review request %q; want one of %v", rr, wantRR)
		}
	}
	if len(snap.Labels) != 1 || snap.Labels[0] != "enhancement" {
		t.Errorf("Labels = %v; want [enhancement]", snap.Labels)
	}
	if snap.CheckRuns != nil {
		t.Errorf("CheckRuns = %v; want nil (discovery snapshot — CI filled by Phase 2)", snap.CheckRuns)
	}
}

// TestListInstallationRepos_Parses pins the GET /installation/repositories
// envelope ({total_count, repositories:[...]}) parse + single-page stop.
func TestListInstallationRepos_Parses(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		if r.URL.Path != "/installation/repositories" {
			t.Errorf("path = %q; want /installation/repositories", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"total_count": 2, "repositories": [
			{"full_name": "octo/repo"},
			{"full_name": "octo/other"}
		]}`))
	}))
	t.Cleanup(srv.Close)

	c := testClient(srv.URL)
	repos, err := c.ListInstallationRepos(context.Background())
	if err != nil {
		t.Fatalf("ListInstallationRepos: %v", err)
	}
	if len(repos) != 2 || repos[0].FullName != "octo/repo" || repos[1].FullName != "octo/other" {
		t.Fatalf("repos = %+v; want octo/repo + octo/other", repos)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("made %d calls; want 1 (total_count reached, no extra page)", got)
	}
}
