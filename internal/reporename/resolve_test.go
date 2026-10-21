package reporename

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
)

// fakeGitHub serves GET /repositories/{id} from byID: a full_name answers 200
// under it, and an id missing from the map answers status (404 by default). A
// 5xx carries a Retry-After past what the client waits out, so it comes back
// on the first attempt instead of after the client's transient backoff.
type fakeGitHub struct {
	byID   map[string]string
	status int
	asked  []string
}

func (g *fakeGitHub) serve(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Suffix, not equality: a non-github.com base URL is a GHES host, so
		// the client mounts the API under /api/v3.
		_, id, ok := strings.Cut(r.URL.Path, "/repositories/")
		if !ok {
			t.Errorf("unexpected request %s", r.URL.Path)
			w.WriteHeader(http.StatusTeapot)
			return
		}
		g.asked = append(g.asked, id)
		name, found := g.byID[id]
		if !found {
			status := g.status
			if status == 0 {
				status = http.StatusNotFound
			}
			if status >= 500 {
				w.Header().Set("Retry-After", "3600")
			}
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"message":"` + http.StatusText(status) + `"}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":` + id + `,"full_name":"` + name + `"}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// serverResolver hands out a PAT client against a fake GitHub, recording the
// account each client was asked for, and evicts coverage like the production
// resolver.
type serverResolver struct {
	coverageResolver
	url     string
	targets []string
}

func (r *serverResolver) ClientFor(_ context.Context, _, target string) (*ghclient.Client, error) {
	r.targets = append(r.targets, target)
	return ghclient.NewClient(r.url, "pat"), nil
}

func TestResolve(t *testing.T) {
	held := domain.RepoRef{Source: "github", Host: testHost, Owner: "octo", Repo: "api", ExternalID: "1"}

	t.Run("an answer under another name is a rename", func(t *testing.T) {
		gh := &fakeGitHub{byID: map[string]string{"1": "octo/api-legacy"}}
		resolver := &serverResolver{url: gh.serve(t).URL}
		store := &renameStore{outcomes: map[string]domain.RepoRenameOutcome{
			"1": {Renamed: true, From: "octo/api", To: "octo/api-legacy"},
		}}

		res, err := Resolve(context.Background(), store, resolver, testLog, "org-1", held)
		if err != nil || res != Renamed {
			t.Fatalf("Resolve = %v, %v; want Renamed", res, err)
		}
		want := domain.RepoRef{Source: "github", Host: testHost, Owner: "octo", Repo: "api-legacy", ExternalID: "1"}
		if len(store.attempts) != 1 || store.attempts[0] != want {
			t.Errorf("renames = %+v, want %+v", store.attempts, want)
		}
		if len(store.retires) != 0 {
			t.Errorf("retired %+v; a repository GitHub answers for is renamed, never retired", store.retires)
		}
		if !reflect.DeepEqual(resolver.forgotten, []string{"octo/api", "octo/api-legacy"}) {
			t.Errorf("evicted %v, want both slugs", resolver.forgotten)
		}
		// The client is the one for the account the name lives under.
		if !reflect.DeepEqual(resolver.targets, []string{"octo"}) {
			t.Errorf("clients asked for %v, want [octo]", resolver.targets)
		}
	})

	t.Run("a 404 retires the row", func(t *testing.T) {
		gh := &fakeGitHub{byID: map[string]string{}}
		resolver := &serverResolver{url: gh.serve(t).URL}
		store := &renameStore{}

		res, err := Resolve(context.Background(), store, resolver, testLog, "org-1", held)
		if err != nil || res != Retired {
			t.Fatalf("Resolve = %v, %v; want Retired", res, err)
		}
		if len(store.retires) != 1 || store.retires[0] != held {
			t.Errorf("retires = %+v, want the held row", store.retires)
		}
		if len(store.attempts) != 0 {
			t.Errorf("renamed %+v; there is no name to rename to", store.attempts)
		}
		if !reflect.DeepEqual(resolver.forgotten, []string{"octo/api"}) {
			t.Errorf("evicted %v, want the retired slug", resolver.forgotten)
		}
		if !reflect.DeepEqual(gh.asked, []string{"1"}) {
			t.Errorf("asked GitHub about %v, want the row's own id", gh.asked)
		}
	})

	t.Run("a retire that found the row moved keeps it", func(t *testing.T) {
		gh := &fakeGitHub{byID: map[string]string{}}
		store := &renameStore{staleRetire: true}
		res, err := Resolve(context.Background(), store, &serverResolver{url: gh.serve(t).URL}, testLog, "org-1", held)
		if err != nil || res != Kept {
			t.Fatalf("Resolve = %v, %v; want Kept", res, err)
		}
	})

	t.Run("the same name back means the row still holds it", func(t *testing.T) {
		gh := &fakeGitHub{byID: map[string]string{"1": "octo/api"}}
		store := &renameStore{outcomes: map[string]domain.RepoRenameOutcome{}}
		res, err := Resolve(context.Background(), store, &serverResolver{url: gh.serve(t).URL}, testLog, "org-1", held)
		if err != nil || res != Kept {
			t.Fatalf("Resolve = %v, %v; want Kept", res, err)
		}
		if len(store.retires) != 0 {
			t.Errorf("retired %+v", store.retires)
		}
	})

	// Every failure that is not GitHub saying the repository is not there
	// changes nothing: a retire is never inferred from an error.
	for _, status := range []int{http.StatusInternalServerError, http.StatusForbidden, http.StatusUnauthorized, http.StatusUnprocessableEntity} {
		t.Run("a "+http.StatusText(status)+" changes nothing", func(t *testing.T) {
			gh := &fakeGitHub{byID: map[string]string{}, status: status}
			store := &renameStore{}
			res, err := Resolve(context.Background(), store, &serverResolver{url: gh.serve(t).URL}, testLog, "org-1", held)
			if err == nil || res != Kept {
				t.Fatalf("Resolve = %v, %v; want Kept and the error", res, err)
			}
			if len(store.retires) != 0 || len(store.attempts) != 0 {
				t.Errorf("wrote retires=%+v renames=%+v on a failed request", store.retires, store.attempts)
			}
		})
	}

	t.Run("no credential changes nothing", func(t *testing.T) {
		store := &renameStore{}
		res, err := Resolve(context.Background(), store, plainResolver{}, testLog, "org-1", held)
		if err == nil || res != Kept || len(store.retires) != 0 {
			t.Fatalf("Resolve = %v, %v, retires %+v; want Kept, an error and no write", res, err, store.retires)
		}
	})

	t.Run("an answer for a different id changes nothing", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"id":2,"full_name":"octo/other"}`))
		}))
		t.Cleanup(srv.Close)
		store := &renameStore{}
		res, err := Resolve(context.Background(), store, &serverResolver{url: srv.URL}, testLog, "org-1", held)
		if err == nil || res != Kept || len(store.attempts) != 0 || len(store.retires) != 0 {
			t.Fatalf("Resolve = %v, %v, writes %v; want Kept, an error and no write", res, err, store.calls)
		}
	})

	t.Run("a row with no id cannot be asked about", func(t *testing.T) {
		store := &renameStore{}
		bare := held
		bare.ExternalID = ""
		if res, err := Resolve(context.Background(), store, plainResolver{}, testLog, "org-1", bare); err == nil || res != Kept {
			t.Fatalf("Resolve = %v, %v; want Kept and an error", res, err)
		}
	})
}

// The observation trigger: a name TF holds for one id, observed under another.
func TestApply_ResolvesANameObservedUnderAnotherID(t *testing.T) {
	t.Run("a deleted and recreated repository retires the stored row", func(t *testing.T) {
		gh := &fakeGitHub{byID: map[string]string{}}
		resolver := &serverResolver{url: gh.serve(t).URL}
		store := &renameStore{stored: []domain.RepoRef{
			{Source: "github", Host: testHost, Owner: "octo", Repo: "api", ExternalID: "1"},
		}}

		n, contested := Apply(context.Background(), store, resolver, testLog, "org-1", testHost,
			[]domain.RepoRef{{Owner: "octo", Repo: "api", ExternalID: "99"}})
		if n != 1 || len(contested) != 0 {
			t.Fatalf("applied = %d, contested = %v; want 1 and none", n, contested)
		}
		if len(store.retires) != 1 || store.retires[0].ExternalID != "1" {
			t.Fatalf("retires = %+v, want the stored row (id 1)", store.retires)
		}
		if !reflect.DeepEqual(gh.asked, []string{"1"}) {
			t.Errorf("asked GitHub about %v, want only the stored id", gh.asked)
		}
	})

	t.Run("a repository renamed away is renamed, not retired", func(t *testing.T) {
		// What a PAT org sees: the old name answers with the new repository's
		// id, because a reused name does not redirect. Asking by the stored id
		// finds where the old repository went.
		gh := &fakeGitHub{byID: map[string]string{"1": "octo/api-v1"}}
		store := &renameStore{
			stored:   []domain.RepoRef{{Source: "github", Host: testHost, Owner: "octo", Repo: "api", ExternalID: "1"}},
			outcomes: map[string]domain.RepoRenameOutcome{"1": {Renamed: true, From: "octo/api", To: "octo/api-v1"}},
		}

		n, _ := Apply(context.Background(), store, &serverResolver{url: gh.serve(t).URL}, testLog, "org-1", testHost,
			[]domain.RepoRef{{Owner: "octo", Repo: "api", ExternalID: "99"}})
		if n != 1 {
			t.Fatalf("applied = %d, want 1", n)
		}
		if len(store.retires) != 0 {
			t.Errorf("retired %+v; the repository still exists", store.retires)
		}
		if len(store.attempts) != 1 || store.attempts[0].Slug() != "octo/api-v1" || store.attempts[0].ExternalID != "1" {
			t.Errorf("renames = %+v, want id 1 to octo/api-v1", store.attempts)
		}
	})

	t.Run("the name is freed before a rename onto it", func(t *testing.T) {
		// octo/web was renamed to octo/api after octo/api was deleted. The
		// rename of id 2 needs the name the stored id 1 holds, so id 1 is
		// resolved first and the rename lands in the same pass.
		gh := &fakeGitHub{byID: map[string]string{}}
		store := &renameStore{
			stored: []domain.RepoRef{
				{Source: "github", Host: testHost, Owner: "octo", Repo: "api", ExternalID: "1"},
				{Source: "github", Host: testHost, Owner: "octo", Repo: "web", ExternalID: "2"},
			},
			outcomes: map[string]domain.RepoRenameOutcome{"2": {Renamed: true, From: "octo/web", To: "octo/api"}},
		}

		n, _ := Apply(context.Background(), store, &serverResolver{url: gh.serve(t).URL}, testLog, "org-1", testHost,
			[]domain.RepoRef{{Owner: "octo", Repo: "api", ExternalID: "2"}})
		if n != 2 {
			t.Fatalf("applied = %d, want 2", n)
		}
		if want := []string{"retire octo/api", "rename octo/api"}; !reflect.DeepEqual(store.calls, want) {
			t.Errorf("writes = %v, want %v", store.calls, want)
		}
	})

	t.Run("a failure to reach GitHub leaves the row, contested, and is survivable", func(t *testing.T) {
		gh := &fakeGitHub{byID: map[string]string{}, status: http.StatusBadGateway}
		store := &renameStore{stored: []domain.RepoRef{
			{Source: "github", Host: testHost, Owner: "octo", Repo: "api", ExternalID: "1"},
		}}
		n, contested := Apply(context.Background(), store, &serverResolver{url: gh.serve(t).URL}, testLog, "org-1", testHost,
			[]domain.RepoRef{{Owner: "octo", Repo: "api", ExternalID: "99"}})
		if n != 0 || len(contested) != 1 || contested[0] != "octo/api" {
			t.Errorf("applied = %d, contested = %v; want 0 and [octo/api]", n, contested)
		}
		if len(store.retires) != 0 {
			t.Errorf("retired %+v on a failed request", store.retires)
		}
	})

	t.Run("without a resolver nothing is asked", func(t *testing.T) {
		store := &renameStore{stored: []domain.RepoRef{
			{Source: "github", Host: testHost, Owner: "octo", Repo: "api", ExternalID: "1"},
		}}
		n, contested := Apply(context.Background(), store, nil, testLog, "org-1", testHost,
			[]domain.RepoRef{{Owner: "octo", Repo: "api", ExternalID: "99"}})
		if n != 0 || len(contested) != 1 || contested[0] != "octo/api" {
			t.Errorf("applied = %d, contested = %v; want 0 and [octo/api]", n, contested)
		}
		if len(store.calls) != 0 {
			t.Errorf("wrote %v with no resolver", store.calls)
		}
	})
}

// A retired row whose id GitHub answers for again is restored through the
// rename path, even under the very name it is stored under.
func TestApply_RestoresARetiredRowGitHubAnswersFor(t *testing.T) {
	store := &renameStore{
		stored:   []domain.RepoRef{{Source: "github", Host: testHost, Owner: "octo", Repo: "api", ExternalID: "1"}},
		retired:  map[string]bool{"1": true},
		outcomes: map[string]domain.RepoRenameOutcome{"1": {Restored: true, From: "octo/api", To: "octo/api"}},
	}
	if n, _ := Apply(context.Background(), store, &coverageResolver{}, testLog, "org-1", testHost,
		[]domain.RepoRef{{Owner: "octo", Repo: "api", ExternalID: "1"}}); n != 1 {
		t.Fatalf("applied = %d, want 1", n)
	}
	if len(store.attempts) != 1 || store.attempts[0].ExternalID != "1" {
		t.Errorf("attempts = %+v, want a restore of id 1", store.attempts)
	}
	// A retired row holds no name, so another id under it is not a name of
	// TF's to resolve.
	store = &renameStore{
		stored:  []domain.RepoRef{{Source: "github", Host: testHost, Owner: "octo", Repo: "api", ExternalID: "1"}},
		retired: map[string]bool{"1": true},
	}
	if n, _ := Apply(context.Background(), store, &serverResolver{url: (&fakeGitHub{}).serve(t).URL}, testLog, "org-1", testHost,
		[]domain.RepoRef{{Owner: "octo", Repo: "api", ExternalID: "99"}}); n != 0 || len(store.calls) != 0 {
		t.Errorf("applied = %d, writes %v; want nothing for a retired row's old name", n, store.calls)
	}
}

func TestReleaseName(t *testing.T) {
	observed := domain.RepoRef{Source: "github", Host: testHost, Owner: "octo", Repo: "api", ExternalID: "99"}
	for _, tc := range []struct {
		name     string
		holder   *domain.Repository
		readErr  error
		resolves bool
	}{
		{name: "nobody holds the name", holder: nil},
		{name: "the holder is the observed repository", holder: &domain.Repository{ID: "r", Host: testHost, Owner: "octo", Repo: "api", ExternalID: "99"}},
		{name: "the holder has no id", holder: &domain.Repository{ID: "r", Host: testHost, Owner: "octo", Repo: "api"}},
		{name: "the holder is another repository", holder: &domain.Repository{ID: "r", Source: "github", Host: testHost, Owner: "octo", Repo: "api", ExternalID: "1"}, resolves: true},
		{name: "the holder cannot be read", readErr: errors.New("db is having a day")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gh := &fakeGitHub{byID: map[string]string{}}
			store := &releaseStore{renameStore: renameStore{}, holder: tc.holder, readErr: tc.readErr}
			released, err := ReleaseName(context.Background(), store, &serverResolver{url: gh.serve(t).URL}, testLog, "org-1", observed)
			if (err != nil) != (tc.readErr != nil) {
				t.Fatalf("err = %v, want %v", err, tc.readErr)
			}
			if released != tc.resolves {
				t.Errorf("released = %v, want %v", released, tc.resolves)
			}
			if tc.resolves && (len(store.retires) != 1 || store.retires[0].ExternalID != "1") {
				t.Errorf("retires = %+v, want the holder", store.retires)
			}
			if !tc.resolves && len(store.calls) != 0 {
				t.Errorf("wrote %v; nothing to resolve", store.calls)
			}
		})
	}
}

// releaseStore answers the read of whatever holds a name.
type releaseStore struct {
	renameStore
	holder  *domain.Repository
	readErr error
}

func (s *releaseStore) GetByRefSystem(context.Context, string, domain.RepoRef) (*domain.Repository, error) {
	return s.holder, s.readErr
}
