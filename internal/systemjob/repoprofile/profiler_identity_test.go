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
			_, _ = w.Write([]byte(`{"id":1296269,"owner":{"login":"octo","id":583231},"default_branch":"main","clone_url":"https://x/octo/with-id.git"}`))
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
	if withID.OwnerExternalID != "583231" {
		t.Errorf("OwnerExternalID = %q, want 583231 — the owner id GitHub sent on the same response", withID.OwnerExternalID)
	}
	if withID.Source != domain.RepoSourceGitHub {
		t.Errorf("Source = %q, want %q", withID.Source, domain.RepoSourceGitHub)
	}

	noID, ok := got["octo/no-id"]
	if !ok {
		t.Fatal("octo/no-id was not upserted")
	}
	if noID.ExternalID != "" || noID.OwnerExternalID != "" {
		t.Errorf("ids = (%q, %q), want both empty — an absent id is not the id 0", noID.ExternalID, noID.OwnerExternalID)
	}
}

// The profiler holds the repository's registry row when it resolves the
// GitHub client, so it names the owner account by the id the row records as
// well as by the login: a rename of the owner account that the row's name has
// not caught up with still finds the installation on that account.
func TestProfiler_ResolvesTheClientByTheRowsOwnerID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/contents/") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{"id":1296269,"full_name":"octo/api","owner":{"login":"octo-co","id":583231},"default_branch":"main"}`))
	}))
	defer srv.Close()

	repos := &ownedRepositoryStore{fetchRepositoryStore: fetchRepositoryStore{names: []string{"octo/api"}}}
	resolver := &accountRecordingResolver{fixedResolver: fixedResolver{client: github.NewClient(srv.URL, "tok")}}
	p := NewProfiler(resolver, nil, nil, repos, oneOrgStore{}, nil, nil, nil)
	if err := p.Run(context.Background(), true /* force: skip the TTL read */); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if want := []string{"583231 octo"}; len(resolver.asked) != 1 || resolver.asked[0] != want[0] {
		t.Errorf("client asked for %v; want %v", resolver.asked, want)
	}
}

// ownedRepositoryStore is fetchRepositoryStore with rows that record their
// owner's account id.
type ownedRepositoryStore struct {
	fetchRepositoryStore
}

func (s *ownedRepositoryStore) GetByRefSystem(_ context.Context, _ string, ref domain.RepoRef) (*domain.Repository, error) {
	return &domain.Repository{ID: "repo-id-" + ref.Repo, Host: ref.Host, Owner: ref.Owner, Repo: ref.Repo, ExternalID: "1296269", OwnerExternalID: "583231"}, nil
}

// accountRecordingResolver is fixedResolver with the account-id extension,
// recording each account it was asked for as "<id> <login>".
type accountRecordingResolver struct {
	fixedResolver
	asked []string
}

func (r *accountRecordingResolver) ClientForAccount(_ context.Context, _, accountID, login string) (*github.Client, error) {
	r.asked = append(r.asked, accountID+" "+login)
	return r.client, nil
}

var _ github.AccountClientResolver = (*accountRecordingResolver)(nil)
