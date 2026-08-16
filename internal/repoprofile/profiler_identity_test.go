package repoprofile

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/github"
)

// The profiler is where TF learns a repository's provider id: the
// /repos/{owner}/{repo} response it already fetches for the clone URL and
// default branch carries it, so recording it costs no request. A repo whose
// response omits the id (an old GHES, a mocked upstream) must persist an empty
// one rather than "0", because the store treats empty as "learned nothing" and
// leaves whatever id the row already had — while "0" would overwrite a real id
// with a value no payload can ever match.
func TestProfiler_CarriesRepositoryIdentityOntoTheRow(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/api/v3/repos/")
		if strings.Contains(path, "/contents/") {
			w.WriteHeader(http.StatusNotFound) // docs-less: straight to the upsert
			return
		}
		switch {
		case strings.HasPrefix(path, "octo/with-id"):
			_, _ = w.Write([]byte(`{"id":1296269,"default_branch":"main","clone_url":"https://x/octo/with-id.git"}`))
		case strings.HasPrefix(path, "octo/no-id"):
			_, _ = w.Write([]byte(`{"default_branch":"main","clone_url":"https://x/octo/no-id.git"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	repos := &fetchRepositoryStore{names: []string{"octo/with-id", "octo/no-id"}}
	p := NewProfiler(
		fixedResolver{client: github.NewClient(srv.URL, "tok")},
		nil, nil, repos, oneOrgStore{}, nil, nil, nil,
	)
	if err := p.Run(context.Background(), true /* force: skip the TTL read */); err != nil {
		t.Fatalf("Run: %v", err)
	}

	got := repos.upsertedByID()

	withID, ok := got["octo/with-id"]
	if !ok {
		t.Fatal("octo/with-id was not upserted")
	}
	if withID.ExternalID != "1296269" {
		t.Errorf("ExternalID = %q, want 1296269 — the id GitHub sent on the meta response", withID.ExternalID)
	}
	if withID.Source != domain.RepoSourceGitHub {
		t.Errorf("Source = %q, want %q", withID.Source, domain.RepoSourceGitHub)
	}

	noID, ok := got["octo/no-id"]
	if !ok {
		t.Fatal("octo/no-id was not upserted")
	}
	if noID.ExternalID != "" {
		t.Errorf("ExternalID = %q, want empty — an absent id is not the id 0", noID.ExternalID)
	}
}
