package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/github/githubapp"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// A registration whose name another row holds under a different GitHub id asks
// GitHub what became of that row's id, frees the name, and registers the new
// repository as a row of its own — under a team admin's claims, with RLS live.
func TestHandleRepoRegister_FreesANameGitHubGaveAway_Postgres(t *testing.T) {
	const oldID, newID = "601", "602"

	// setup: teamA tracks acme/api under GitHub id 601, and the reachable
	// mirror now lists acme/api under 602.
	setup := func(t *testing.T, byID map[string]string, status int) *repoScopeRig {
		t.Helper()
		r := newRepoScopeRig(t)
		if _, err := r.s.repos.FillMissingExternalIDsSystem(t.Context(), r.orgID, []domain.RepoRef{
			{Host: dbtest.TestGitHubHost, Owner: "acme", Repo: "api", ExternalID: oldID},
		}); err != nil {
			t.Fatalf("seed acme/api's id: %v", err)
		}
		if err := r.s.reachableRepos.ReplaceForPATSystem(t.Context(), r.orgID, dbtest.TestGitHubHost, []domain.ReachableRepository{
			{OrgID: r.orgID, Owner: "acme", Repo: "api", ExternalID: newID},
		}); err != nil {
			t.Fatalf("seed the reachable mirror: %v", err)
		}
		r.s.ghResolver = repositoriesByIDResolver{url: serveRepositoriesByID(t, byID, status)}
		return r
	}
	register := func(r *repoScopeRig, caller, githubID string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		r.s.handleRepoRegister(rec, r.req(http.MethodPost, "/api/repos", caller, map[string]any{"github_id": githubID}))
		return rec
	}

	t.Run("deleted: the holder is retired and the new repository registered", func(t *testing.T) {
		r := setup(t, map[string]string{}, http.StatusNotFound)
		oldRow := r.repoID(t, "acme", "api")

		rec := register(r, r.memberB, newID)
		if rec.Code != http.StatusOK {
			t.Fatalf("register = %d, want 200; body=%s", rec.Code, rec.Body.String())
		}
		var got repoJSON
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if got.ID == oldRow || got.Slug != "acme/api" || got.GitHubID != newID || got.RetiredAt != nil {
			t.Errorf("registered = %+v, want a new live acme/api row carrying %s", got, newID)
		}
		old, err := r.s.repos.GetSystem(t.Context(), r.orgID, oldRow)
		if err != nil || old.RetiredAt == nil || old.ExternalID != oldID {
			t.Errorf("old row = %+v, err=%v; want it retired with its id", old, err)
		}

		// teamA still tracks the retired row, and its set says so.
		req := r.req(http.MethodGet, "/api/teams/"+r.teamA+"/github-repos", r.orgOwner, nil)
		req.SetPathValue("team_id", r.teamA)
		tracked := httptest.NewRecorder()
		r.s.handleTeamReposGet(tracked, req)
		var set teamReposResponse
		if err := json.Unmarshal(tracked.Body.Bytes(), &set); err != nil {
			t.Fatalf("decode tracked set: %v; body=%s", err, tracked.Body.String())
		}
		if len(set.Repos) != 1 || set.Repos[0].ID != oldRow || set.Repos[0].RetiredAt == nil {
			t.Errorf("teamA tracked = %+v, want the retired row, marked", set.Repos)
		}
	})

	t.Run("renamed away: the holder is renamed and the new repository registered", func(t *testing.T) {
		r := setup(t, map[string]string{oldID: "acme/api-v1"}, http.StatusNotFound)
		oldRow := r.repoID(t, "acme", "api")

		if rec := register(r, r.memberB, newID); rec.Code != http.StatusOK {
			t.Fatalf("register = %d, want 200; body=%s", rec.Code, rec.Body.String())
		}
		old, err := r.s.repos.GetSystem(t.Context(), r.orgID, oldRow)
		if err != nil || old.RetiredAt != nil || old.Slug() != "acme/api-v1" {
			t.Errorf("old row = %+v, err=%v; want it live under acme/api-v1", old, err)
		}
	})

	for name, gh := range map[string]struct {
		byID   map[string]string
		status int
	}{
		// GitHub still serves 601 under acme/api, so the mirror was stale.
		"the holder still has the name": {byID: map[string]string{oldID: "acme/api"}, status: http.StatusNotFound},
		"GitHub cannot be reached":      {byID: map[string]string{}, status: http.StatusServiceUnavailable},
	} {
		t.Run(name+": 409 and nothing retired", func(t *testing.T) {
			r := setup(t, gh.byID, gh.status)
			oldRow := r.repoID(t, "acme", "api")
			before := registryCount(t, r)

			rec := register(r, r.memberB, newID)
			if rec.Code != http.StatusConflict {
				t.Fatalf("register = %d, want 409; body=%s", rec.Code, rec.Body.String())
			}
			var body struct {
				Errors []httpx.ErrorItem `json:"errors"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || len(body.Errors) != 1 {
				t.Fatalf("decode: %v; body=%s", err, rec.Body.String())
			}
			if e := body.Errors[0]; e.Reason != httpx.ReasonConflict || e.Field != "github_id" || strings.Contains(e.Message, "rename") {
				t.Errorf("error = %+v, want a CONFLICT on github_id that promises no rename", e)
			}
			if old, _ := r.s.repos.GetSystem(t.Context(), r.orgID, oldRow); old == nil || old.RetiredAt != nil || old.Slug() != "acme/api" {
				t.Errorf("old row = %+v, want it untouched", old)
			}
			if n := registryCount(t, r); n != before {
				t.Errorf("registry rows %d -> %d, want no new row", before, n)
			}
		})
	}

	t.Run("a retired row the mirror lists again is restored", func(t *testing.T) {
		r := setup(t, map[string]string{}, http.StatusNotFound)
		oldRow := r.repoID(t, "acme", "api")
		if _, err := r.s.repos.RetireSystem(t.Context(), r.orgID, domain.RepoRef{
			Host: dbtest.TestGitHubHost, Owner: "acme", Repo: "api", ExternalID: oldID,
		}); err != nil {
			t.Fatalf("RetireSystem: %v", err)
		}
		// Access to the old repository is back, under a new name.
		if err := r.s.reachableRepos.ReplaceForPATSystem(t.Context(), r.orgID, dbtest.TestGitHubHost, []domain.ReachableRepository{
			{OrgID: r.orgID, Owner: "acme", Repo: "api-archive", ExternalID: oldID},
		}); err != nil {
			t.Fatalf("seed the reachable mirror: %v", err)
		}

		rec := register(r, r.memberB, oldID)
		if rec.Code != http.StatusOK {
			t.Fatalf("register = %d, want 200; body=%s", rec.Code, rec.Body.String())
		}
		var got repoJSON
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if got.ID != oldRow || got.Slug != "acme/api-archive" || got.RetiredAt != nil {
			t.Errorf("registered = %+v, want row %s restored under acme/api-archive", got, oldRow)
		}
	})

	t.Run("a retired row is restored onto a name a live row holds once GitHub frees it", func(t *testing.T) {
		// 601 was retired and 602 registered under acme/api. Now the mirror
		// lists 601 under acme/api again and GitHub has no 602.
		r := setup(t, map[string]string{}, http.StatusNotFound)
		oldRow := r.repoID(t, "acme", "api")
		first := register(r, r.memberB, newID)
		var holder repoJSON
		if first.Code != http.StatusOK || json.Unmarshal(first.Body.Bytes(), &holder) != nil {
			t.Fatalf("register %s = %d, want 200; body=%s", newID, first.Code, first.Body.String())
		}
		newRow := holder.ID
		if err := r.s.reachableRepos.ReplaceForPATSystem(t.Context(), r.orgID, dbtest.TestGitHubHost, []domain.ReachableRepository{
			{OrgID: r.orgID, Owner: "acme", Repo: "api", ExternalID: oldID},
		}); err != nil {
			t.Fatalf("seed the reachable mirror: %v", err)
		}

		rec := register(r, r.memberB, oldID)
		if rec.Code != http.StatusOK {
			t.Fatalf("register %s = %d, want 200; body=%s", oldID, rec.Code, rec.Body.String())
		}
		var got repoJSON
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if got.ID != oldRow || got.Slug != "acme/api" || got.RetiredAt != nil {
			t.Errorf("registered = %+v, want row %s restored under acme/api", got, oldRow)
		}
		if row, err := r.s.repos.GetSystem(t.Context(), r.orgID, newRow); err != nil || row.RetiredAt == nil {
			t.Errorf("the row that held the name = %+v, err=%v; want it retired", row, err)
		}
	})
}

// serveRepositoriesByID is a GitHub whose GET /repositories/{id} answers
// byID's full_name for a known id and status for any other. A 5xx carries a
// Retry-After past what the client waits out, so it fails on the first
// attempt.
func serveRepositoriesByID(t *testing.T, byID map[string]string, status int) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, id, _ := strings.Cut(r.URL.Path, "/repositories/")
		if name, ok := byID[id]; ok {
			_, _ = w.Write([]byte(`{"id":` + id + `,"full_name":"` + name + `"}`))
			return
		}
		if status >= 500 {
			w.Header().Set("Retry-After", "3600")
		}
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// repositoriesByIDResolver hands every caller a client for one fake GitHub.
type repositoriesByIDResolver struct{ url string }

func (r repositoriesByIDResolver) ClientFor(context.Context, string, string) (*ghclient.Client, error) {
	return ghclient.NewClient(r.url, ""), nil
}

func (r repositoriesByIDResolver) ClientForInstallation(context.Context, string, domain.OrgGitHubAppInstallation) (*ghclient.Client, error) {
	return ghclient.NewClient(r.url, ""), nil
}

func (r repositoriesByIDResolver) ClientForRepo(context.Context, string, string, string) (*ghclient.Client, error) {
	return ghclient.NewClient(r.url, ""), nil
}

func (repositoriesByIDResolver) TokenFor(context.Context, string, string) (githubapp.Token, error) {
	return githubapp.Token{}, errors.New("no token in this test")
}

func (r repositoriesByIDResolver) BaseURLFor(context.Context, string) (string, error) {
	return r.url, nil
}

func (repositoriesByIDResolver) OrgIdentityFor(context.Context, string) (string, string, bool) {
	return "", "", false
}
