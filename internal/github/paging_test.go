package github

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// makeOpenPRPage returns n synthetic open-PR JSON objects for ListOpenPRs'
// per-page decoding, numbered starting at base so pages don't collide.
func makeOpenPRPage(n, base int) []map[string]any {
	out := make([]map[string]any, n)
	for i := 0; i < n; i++ {
		out[i] = map[string]any{
			"number":  base + i,
			"node_id": fmt.Sprintf("PR_%d", base+i),
			"title":   "x",
			"state":   "open",
			"user":    map[string]any{"login": "alice"},
			"head":    map[string]any{"sha": "deadbeef", "ref": "feature", "repo": map[string]any{"full_name": "octo/repo"}},
			"base":    map[string]any{"ref": "main", "repo": map[string]any{"full_name": "octo/repo"}},
		}
	}
	return out
}

// TestListOpenPRs_TruncatesAtPageCap is TFAC-571's pagination-cap acceptance:
// a repo with more open PRs than maxFetchPages*100 must truncate rather than
// paginate unbounded, and the truncation must be logged at WARN (never
// silent) — a pathological repo must not amplify call volume without bound.
func TestListOpenPRs_TruncatesAtPageCap(t *testing.T) {
	logs := &captureHandler{}
	prev := githubLog
	githubLog = slog.New(logs)
	t.Cleanup(func() { githubLog = prev })

	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page := atomic.AddInt32(&calls, 1)
		data, _ := json.Marshal(makeOpenPRPage(100, int(page)*1000))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(data)
	}))
	t.Cleanup(srv.Close)

	c := testClient(srv.URL)
	prs, _, notModified, err := c.ListOpenPRs(context.Background(), "octo", "repo", "")
	if err != nil {
		t.Fatalf("ListOpenPRs: %v", err)
	}
	if notModified {
		t.Fatal("notModified = true; want false (every page returns a full page)")
	}
	if len(prs) != maxFetchPages*100 {
		t.Errorf("got %d PRs; want %d (capped at maxFetchPages)", len(prs), maxFetchPages*100)
	}
	if got := atomic.LoadInt32(&calls); got != int32(maxFetchPages) {
		t.Errorf("made %d HTTP calls; want exactly %d (page cap)", got, maxFetchPages)
	}

	warned := false
	for _, r := range logs.recorded() {
		if strings.Contains(r.Message, "open PR listing truncated") {
			warned = true
		}
	}
	if !warned {
		t.Error("expected a truncation warning when the open-PR listing hits the page cap")
	}
}

// TestGetPRFiles_TruncationLogsWarning pins the WARN-log half of GetPRFiles'
// existing MaxPRFiles cap (see TestGetPRFiles_CapAt1000 for the count
// assertions) — truncation must be visible, not silent (TFAC-571).
func TestGetPRFiles_TruncationLogsWarning(t *testing.T) {
	logs := &captureHandler{}
	prev := githubLog
	githubLog = slog.New(logs)
	t.Cleanup(func() { githubLog = prev })

	var callCount int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		data, _ := json.Marshal(makePRFilesList(100, fmt.Sprintf("call%d", callCount)))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(data)
	}))
	t.Cleanup(srv.Close)

	c := clientAgainst(srv.URL)
	if _, err := c.GetPRFiles(context.Background(), "owner", "repo", 1); err != nil {
		t.Fatalf("GetPRFiles: %v", err)
	}

	warned := false
	for _, r := range logs.recorded() {
		if strings.Contains(r.Message, "PR files list truncated") {
			warned = true
		}
	}
	if !warned {
		t.Error("expected a truncation warning when GetPRFiles hits MaxPRFiles")
	}
}

// TestListUserRepos_TruncatesAtPageCap mirrors TestListOpenPRs_TruncatesAtPageCap
// for the uncapped-before-TFAC-571 /user/repos listing.
func TestListUserRepos_TruncatesAtPageCap(t *testing.T) {
	logs := &captureHandler{}
	prev := githubLog
	githubLog = slog.New(logs)
	t.Cleanup(func() { githubLog = prev })

	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page := atomic.AddInt32(&calls, 1)
		repos := make([]UserRepo, 100)
		for i := range repos {
			repos[i] = UserRepo{FullName: fmt.Sprintf("octo/repo-%d-%d", page, i)}
		}
		data, _ := json.Marshal(repos)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(data)
	}))
	t.Cleanup(srv.Close)

	c := clientAgainst(srv.URL)
	repos, err := c.ListUserRepos(context.Background())
	if err != nil {
		t.Fatalf("ListUserRepos: %v", err)
	}
	if len(repos) != maxFetchPages*100 {
		t.Errorf("got %d repos; want %d (capped at maxFetchPages)", len(repos), maxFetchPages*100)
	}
	if got := atomic.LoadInt32(&calls); got != int32(maxFetchPages) {
		t.Errorf("made %d HTTP calls; want exactly %d (page cap)", got, maxFetchPages)
	}

	warned := false
	for _, r := range logs.recorded() {
		if strings.Contains(r.Message, "user repos list truncated") {
			warned = true
		}
	}
	if !warned {
		t.Error("expected a truncation warning when the user-repos listing hits the page cap")
	}
}

// TestListInstallationRepos_TruncatesAtPageCap pins the installation-grant
// listing cap: an installation with more repos than maxFetchPages*100 (an
// unusually large App grant) must truncate with a logged WARN rather than
// silently under-covering the poll's tracked-repo intersection.
func TestListInstallationRepos_TruncatesAtPageCap(t *testing.T) {
	logs := &captureHandler{}
	prev := githubLog
	githubLog = slog.New(logs)
	t.Cleanup(func() { githubLog = prev })

	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page := atomic.AddInt32(&calls, 1)
		repos := make([]UserRepo, 100)
		for i := range repos {
			repos[i] = UserRepo{FullName: fmt.Sprintf("octo/repo-%d-%d", page, i)}
		}
		resp := struct {
			TotalCount   int        `json:"total_count"`
			Repositories []UserRepo `json:"repositories"`
		}{TotalCount: 5000, Repositories: repos} // far beyond the page cap — never satisfied
		data, _ := json.Marshal(resp)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(data)
	}))
	t.Cleanup(srv.Close)

	c := clientAgainst(srv.URL)
	repos, err := c.ListInstallationRepos(context.Background())
	if err != nil {
		t.Fatalf("ListInstallationRepos: %v", err)
	}
	if len(repos) != maxFetchPages*100 {
		t.Errorf("got %d repos; want %d (capped at maxFetchPages)", len(repos), maxFetchPages*100)
	}
	if got := atomic.LoadInt32(&calls); got != int32(maxFetchPages) {
		t.Errorf("made %d HTTP calls; want exactly %d (page cap)", got, maxFetchPages)
	}

	warned := false
	for _, r := range logs.recorded() {
		if strings.Contains(r.Message, "installation repos list truncated") {
			warned = true
		}
	}
	if !warned {
		t.Error("expected a truncation warning when the installation-repos listing hits the page cap")
	}
}
