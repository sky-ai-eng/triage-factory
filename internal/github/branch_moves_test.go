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

// TestBranchTip_ReadsTheRefConditionally: the ref is read by its full name, a
// branch with a slash keeping it as a path separator, a 200 answers the tip
// and its ETag, and a matching ETag answers 304 with no tip.
func TestBranchTip_ReadsTheRefConditionally(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v3/repos/octo/repo/git/ref/heads/release/1.0" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if r.Header.Get("If-None-Match") == `"e1"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `"e1"`)
		_, _ = w.Write([]byte(`{"ref":"refs/heads/release/1.0","object":{"sha":"tip-1","type":"commit"}}`))
	}))
	t.Cleanup(srv.Close)
	c := NewClient(srv.URL, "tok")

	sha, etag, notModified, err := c.BranchTip(context.Background(), "octo", "repo", "release/1.0", "")
	if err != nil || notModified || sha != "tip-1" || etag != `"e1"` {
		t.Fatalf("first read = (%q, %q, %v, %v), want tip-1 with its etag", sha, etag, notModified, err)
	}
	sha, _, notModified, err = c.BranchTip(context.Background(), "octo", "repo", "release/1.0", `"e1"`)
	if err != nil || !notModified || sha != "" {
		t.Errorf("conditional read = (%q, %v, %v), want a 304", sha, notModified, err)
	}
}

// TestCompare_MapsFilesAndCommits: a compare's file statuses read in the
// PatchStatus vocabulary with each rename's previous path, and its commits
// carry their parents in order.
func TestCompare_MapsFilesAndCommits(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v3/repos/octo/repo/compare/a...b" {
			t.Errorf("path = %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"status":"ahead","total_commits":2,
			"commits":[{"sha":"m","parents":[{"sha":"a"}]},{"sha":"b","parents":[{"sha":"m"},{"sha":"side"}]}],
			"files":[
				{"filename":"x.go","status":"added"},
				{"filename":"y.go","status":"removed"},
				{"filename":"new/z.go","status":"renamed","previous_filename":"old/z.go"},
				{"filename":"w.go","status":"modified","patch":"@@"},
				{"filename":"v.sh","status":"unchanged"}
			]}`))
	}))
	t.Cleanup(srv.Close)

	got, err := NewClient(srv.URL, "tok").Compare(context.Background(), "octo", "repo", "a", "b")
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	wantFiles := []domain.ChangedPath{
		{Path: "x.go", Change: domain.PathAdded},
		{Path: "y.go", Change: domain.PathDeleted},
		{Path: "new/z.go", Change: domain.PathRenamed, Previous: "old/z.go"},
		{Path: "w.go", Change: domain.PathModified},
		{Path: "v.sh", Change: domain.PathChanged},
	}
	if got.Status != "ahead" || got.TotalCommits != 2 || len(got.Files) != len(wantFiles) {
		t.Fatalf("compare = %+v", got)
	}
	for i, f := range wantFiles {
		if got.Files[i] != f {
			t.Errorf("file %d = %+v, want %+v", i, got.Files[i], f)
		}
	}
	if len(got.Commits) != 2 || got.Commits[1].SHA != "b" || len(got.Commits[1].Parents) != 2 || got.Commits[1].Parents[0] != "m" {
		t.Errorf("commits = %+v, want b's first parent m", got.Commits)
	}
}

// TestPRChangedPaths_PagesEachPullRequest: a pull request with more files than
// the first page is paged through node(id:) and is complete when the list
// holds all its changedFiles; one whose head moved or that was retargeted
// between pages, or whose files GitHub could not list, is incomplete; an id
// with no node is absent.
func TestPRChangedPaths_PagesEachPullRequest(t *testing.T) {
	file := func(p string) string { return `{"path":"` + p + `","changeType":"MODIFIED"}` }
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		switch {
		case strings.Contains(req.Query, "query PRChangedPaths("):
			_, _ = w.Write([]byte(`{"data":{"nodes":[
				{"id":"PR_a","headRefOid":"ha","baseRefName":"main","changedFiles":2,"files":{"pageInfo":{"hasNextPage":true,"endCursor":"c1"},"nodes":[` + file("a1") + `]}},
				{"id":"PR_b","headRefOid":"hb","baseRefName":"main","changedFiles":2,"files":{"pageInfo":{"hasNextPage":true,"endCursor":"c2"},"nodes":[` + file("b1") + `]}},
				{"id":"PR_c","headRefOid":"hc","baseRefName":"main","changedFiles":4,"files":null},
				{"id":"PR_r","headRefOid":"hr","baseRefName":"main","changedFiles":2,"files":{"pageInfo":{"hasNextPage":true,"endCursor":"c3"},"nodes":[` + file("r1") + `]}},
				null
			],"rateLimit":{"cost":1}}}`))
		case req.Variables["id"] == "PR_a":
			_, _ = w.Write([]byte(`{"data":{"node":{"headRefOid":"ha","baseRefName":"main","files":{"pageInfo":{"hasNextPage":false},"nodes":[{"path":"a2","changeType":"RENAMED"}]}},"rateLimit":{"cost":1}}}`))
		case req.Variables["id"] == "PR_r":
			_, _ = w.Write([]byte(`{"data":{"node":{"headRefOid":"hr","baseRefName":"release","files":{"pageInfo":{"hasNextPage":false},"nodes":[` + file("r2") + `]}},"rateLimit":{"cost":1}}}`))
		default:
			_, _ = w.Write([]byte(`{"data":{"node":{"headRefOid":"moved","baseRefName":"main","files":{"pageInfo":{"hasNextPage":false},"nodes":[` + file("b2") + `]}},"rateLimit":{"cost":1}}}`))
		}
	}))
	t.Cleanup(srv.Close)

	got, requests, err := NewClient(srv.URL, "tok").PRChangedPaths(context.Background(), []string{"PR_a", "PR_b", "PR_c", "PR_r", "PR_gone"})
	if err != nil {
		t.Fatalf("PRChangedPaths: %v", err)
	}
	if requests != 4 {
		t.Errorf("requests = %d, want the batch and one page for each paged pull request", requests)
	}
	if a := got["PR_a"]; !a.Complete || a.HeadSHA != "ha" || a.BaseRef != "main" || len(a.Paths) != 2 || a.Paths[1] != (domain.ChangedPath{Path: "a2", Change: domain.PathRenamed}) {
		t.Errorf("PR_a = %+v, want both pages, complete", a)
	}
	if b := got["PR_b"]; b.Complete || len(b.Paths) != 1 {
		t.Errorf("PR_b = %+v, want its first page alone, incomplete: its head moved", b)
	}
	if r := got["PR_r"]; r.Complete || len(r.Paths) != 1 {
		t.Errorf("PR_r = %+v, want its first page alone, incomplete: it was retargeted", r)
	}
	if c, ok := got["PR_c"]; !ok || c.Complete || c.ChangedFiles != 4 {
		t.Errorf("PR_c = %+v, want an incomplete read", c)
	}
	if _, ok := got["PR_gone"]; ok {
		t.Error("an id with no node has a read")
	}
}

// TestPreviousPaths_PagesTheRESTFiles: every page is read until a short one,
// and only renamed or copied files carry a previous path.
func TestPreviousPaths_PagesTheRESTFiles(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") == "1" {
			files := make([]map[string]string, 100)
			for i := range files {
				files[i] = map[string]string{"filename": "f" + string(rune('a'+i%26))}
			}
			files[3] = map[string]string{"filename": "new.go", "previous_filename": "old.go"}
			_ = json.NewEncoder(w).Encode(files)
			return
		}
		_, _ = w.Write([]byte(`[{"filename":"copy.go","previous_filename":"src.go"}]`))
	}))
	t.Cleanup(srv.Close)

	got, requests, err := NewClient(srv.URL, "tok").PreviousPaths(context.Background(), "octo", "repo", 7)
	if err != nil || requests != 2 {
		t.Fatalf("PreviousPaths = (%v, %d, %v), want two pages", got, requests, err)
	}
	if len(got) != 2 || got["new.go"] != "old.go" || got["copy.go"] != "src.go" {
		t.Errorf("previous paths = %v", got)
	}
}
