package github

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// GetRepoMeta reads GitHub's numeric repository id, and the id of the account
// that owns the repository, off a response it was already fetching for the
// clone URL and default branch. If either field stops being parsed, nothing
// fails — repo rows simply keep a NULL external_id or owner_external_id
// forever, which is exactly the state the columns are meant to leave behind.
func TestGetRepoMeta_ParsesRepositoryID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Suffix, not equality: a non-github.com base URL is a GHES host, so
		// the client mounts the API under /api/v3.
		if !strings.HasSuffix(r.URL.Path, "/repos/octo/widget") {
			t.Errorf("path = %q, want it to end in /repos/octo/widget", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{
			"id": 1296269,
			"owner": {"login": "octo", "id": 583231},
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
	if got := meta.OwnerExternalID(); got != "583231" {
		t.Errorf("OwnerExternalID() = %q, want the owner account's id as decimal text", got)
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
	if got := (RepoMeta{DefaultBranch: "main"}).OwnerExternalID(); got != "" {
		t.Errorf("OwnerExternalID() with no owner = %q, want empty", got)
	}
}

// GetRepoByID asks by the id a rename does not move, and answers under the
// repository's current name. A 404 is reported as one, distinctly from any
// other failure: it is the only answer a retire may be decided on.
func TestGetRepoByID(t *testing.T) {
	var asked []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = append(asked, r.URL.Path)
		switch {
		case strings.HasSuffix(r.URL.Path, "/repositories/1296269"):
			_, _ = w.Write([]byte(`{"id":1296269,"full_name":"octo/widget-v2","owner":{"login":"octo","id":583231}}`))
		case strings.HasSuffix(r.URL.Path, "/repositories/7"):
			w.WriteHeader(http.StatusForbidden)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	c := NewClient(srv.URL, "")

	meta, err := c.GetRepoByID(context.Background(), "1296269")
	if err != nil || meta.FullName != "octo/widget-v2" || meta.ExternalID() != "1296269" || meta.OwnerExternalID() != "583231" {
		t.Errorf("GetRepoByID = %+v, %v; want octo/widget-v2 carrying 1296269, owned by 583231", meta, err)
	}
	if _, err := c.GetRepoByID(context.Background(), "5"); !IsHTTP404(err) {
		t.Errorf("deleted repository: err = %v, want a 404", err)
	}
	if _, err := c.GetRepoByID(context.Background(), "7"); err == nil || IsHTTP404(err) {
		t.Errorf("forbidden: err = %v, want an error that is not a 404", err)
	}

	before := len(asked)
	for _, bad := range []string{"", "0", "octo/widget", "12/../users"} {
		if _, err := c.GetRepoByID(context.Background(), bad); err == nil {
			t.Errorf("GetRepoByID(%q) succeeded; want it refused", bad)
		}
	}
	if len(asked) != before {
		t.Errorf("a malformed id reached GitHub: %v", asked[before:])
	}
}
