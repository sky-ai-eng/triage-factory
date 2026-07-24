package delegate

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
)

// prSkeletonServer stands in for GitHub: the PR object plus a one-page
// timeline, with a hook so a test can make either endpoint fail.
func prSkeletonServer(t *testing.T, fail map[string]int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for path, code := range fail {
			if strings.Contains(r.URL.Path, path) {
				w.WriteHeader(code)
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/pulls/18"):
			_, _ = w.Write([]byte(`{
				"title": "Add retry to the poller",
				"state": "open",
				"created_at": "2026-07-01T14:02:00Z",
				"updated_at": "2026-07-04T10:00:00Z",
				"user": {"login": "alice"},
				"head": {"ref": "fix-poller", "sha": "a1b2c3d4e5f6"},
				"base": {"ref": "main"},
				"additions": 412, "deletions": 87, "changed_files": 9
			}`))
		case strings.HasSuffix(r.URL.Path, "/timeline"):
			_, _ = w.Write([]byte(`[
				{"event": "reviewed", "state": "changes_requested",
				 "submitted_at": "2026-07-02T09:11:00Z", "user": {"login": "bob"}},
				{"event": "head_ref_force_pushed", "created_at": "2026-07-03T08:00:00Z",
				 "actor": {"login": "alice"}}
			]`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestRenderPRSkeleton(t *testing.T) {
	srv := prSkeletonServer(t, nil)
	got := renderPRSkeleton(context.Background(), ghclient.NewClient(srv.URL, "token"), "octo", "repo", 18)

	for _, want := range []string{
		"Pull request octo/repo#18",
		"review: CHANGES_REQUESTED",
		"force-push",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("rendered skeleton missing %q;\n%s", want, got)
		}
	}
}

// A GitHub failure fetching history must degrade to the context a run has
// always had, never abort the run. History is an enrichment.
func TestRenderPRSkeletonDegradesOnFailure(t *testing.T) {
	cases := map[string]map[string]int{
		"PR object unreachable": {"/pulls/": http.StatusInternalServerError},
		"PR not visible":        {"/pulls/": http.StatusNotFound},
	}
	for name, fail := range cases {
		t.Run(name, func(t *testing.T) {
			srv := prSkeletonServer(t, fail)
			if got := renderPRSkeleton(context.Background(), ghclient.NewClient(srv.URL, "t"), "octo", "repo", 18); got != "" {
				t.Errorf("want an empty skeleton on failure, got:\n%s", got)
			}
		})
	}
}

// A timeline failure is partial, not total: the header still describes the
// PR, and the block says the history is short rather than implying the PR
// has none.
func TestRenderPRSkeletonPartialTimelineStillRenders(t *testing.T) {
	srv := prSkeletonServer(t, map[string]int{"/timeline": http.StatusInternalServerError})
	got := renderPRSkeleton(context.Background(), ghclient.NewClient(srv.URL, "t"), "octo", "repo", 18)

	if !strings.Contains(got, "Pull request octo/repo#18") {
		t.Errorf("header should survive a timeline failure;\n%s", got)
	}
	if !strings.Contains(got, "oldest history is missing") {
		t.Errorf("a short fetch must be stated, not implied;\n%s", got)
	}
}

func TestRenderPRSkeletonNoClient(t *testing.T) {
	if got := renderPRSkeleton(context.Background(), nil, "octo", "repo", 18); got != "" {
		t.Errorf("want empty with no client, got %q", got)
	}
	srv := prSkeletonServer(t, nil)
	if got := renderPRSkeleton(context.Background(), ghclient.NewClient(srv.URL, "t"), "", "", 0); got != "" {
		t.Errorf("want empty with no PR coordinates, got %q", got)
	}
}
