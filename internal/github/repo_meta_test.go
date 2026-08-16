package github

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// GetRepoMeta is the only place TF reads GitHub's numeric repository id, and
// it reads it off a response it was already fetching for the clone URL and
// default branch. If the field stops being parsed, nothing fails — repo rows
// simply keep a NULL external_id forever, which is exactly the state the
// column is meant to leave behind.
func TestGetRepoMeta_ParsesRepositoryID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Suffix, not equality: a non-github.com base URL is a GHES host, so
		// the client mounts the API under /api/v3.
		if !strings.HasSuffix(r.URL.Path, "/repos/octo/widget") {
			t.Errorf("path = %q, want it to end in /repos/octo/widget", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{
			"id": 1296269,
			"default_branch": "main",
			"clone_url": "https://github.com/octo/widget.git",
			"ssh_url": "git@github.com:octo/widget.git"
		}`))
	}))
	defer srv.Close()

	meta, err := NewClient(srv.URL, "").GetRepoMeta(context.Background(), "octo", "widget")
	if err != nil {
		t.Fatalf("GetRepoMeta: %v", err)
	}
	if meta.ID != 1296269 {
		t.Errorf("ID = %d, want 1296269", meta.ID)
	}
	if got := meta.ExternalID(); got != "1296269" {
		t.Errorf("ExternalID() = %q, want the decimal text repositories stores", got)
	}
	if meta.DefaultBranch != "main" || meta.CloneURL != "https://github.com/octo/widget.git" {
		t.Errorf("the fields the id rides along with regressed: %+v", meta)
	}
}

// A response with no id must produce an empty ExternalID rather than "0":
// zero is not a repository, and persisting it would key a row to an id that
// can never match a payload.
func TestRepoMetaExternalID_EmptyWhenAbsent(t *testing.T) {
	if got := (RepoMeta{DefaultBranch: "main"}).ExternalID(); got != "" {
		t.Errorf("ExternalID() with no id = %q, want empty", got)
	}
}
