package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/auth/verify"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbseed"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// The dashboard's per-PR routes address a pull request by the entity id its
// list row served, and reach GitHub under the repository's current registry
// name, whatever the entity's key still spells.

// fakePRGitHub is a GitHub that knows one pull request, owner/repo#number,
// answers the status reads and the draft mutation for it, and 404s every other
// path. It records the paths it was asked for, without the API prefix.
type fakePRGitHub struct {
	url string

	mu    sync.Mutex
	paths []string
}

func newFakePRGitHub(t *testing.T, owner, repo, number string) *fakePRGitHub {
	t.Helper()
	f := &fakePRGitHub{}
	pull := "/repos/" + owner + "/" + repo + "/pulls/" + number
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The client treats any host but github.com as GitHub Enterprise:
		// REST under /api/v3, GraphQL at /api/graphql.
		path := strings.TrimPrefix(r.URL.Path, "/api/v3")
		path = strings.TrimPrefix(path, "/api")
		f.mu.Lock()
		f.paths = append(f.paths, r.Method+" "+path)
		f.mu.Unlock()
		switch {
		case r.Method == http.MethodGet && path == pull:
			_, _ = w.Write([]byte(`{"node_id":"PR_node","mergeable":true,"mergeable_state":"clean","head":{"sha":""}}`))
		case r.Method == http.MethodGet && path == pull+"/reviews":
			_, _ = w.Write([]byte(`[]`))
		case r.Method == http.MethodPost && path == "/graphql":
			_, _ = w.Write([]byte(`{"data":{"convertPullRequestToDraft":{"pullRequest":{"isDraft":true}}}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	f.url = srv.URL
	return f
}

func (f *fakePRGitHub) asked(method, path string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, p := range f.paths {
		if p == method+" "+path {
			return true
		}
	}
	return false
}

// prRoutes serves the per-PR routes from a dashboard handler whose GitHub is
// gh, with the local org and user on every request — what the session
// middleware puts there for the server's own mux, whose handler holds the
// server's resolver.
func prRoutes(s *Server, gh *fakePRGitHub) http.Handler {
	dh := &dashboardHandler{tx: s.tx, ghResolver: repositoriesByIDResolver{url: gh.url}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/dashboard/prs/{entity_id}/status", dh.handleDashboardPRStatus)
	mux.HandleFunc("PATCH /api/dashboard/prs/{entity_id}", dh.handleDashboardPRUpdate)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := httpx.WithOrgID(r.Context(), runmode.LocalDefaultOrgID)
		ctx = httpx.WithClaims(ctx, &verify.Claims{Subject: runmode.LocalDefaultUserID})
		mux.ServeHTTP(w, r.WithContext(ctx))
	})
}

func servePR(t *testing.T, h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// dashboardPRFixture binds the local user's GitHub login and seeds pull
// request #7 in acme/old authored under it, then renames the registry row to
// acme/new without touching the entity's key, as GitHub renaming the
// repository leaves it until the next poll rewrites the key.
func dashboardPRFixture(t *testing.T, s *Server) *domain.Entity {
	t.Helper()
	host := dashboardTestHost(t, s, runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID)
	if err := s.users.UpsertGitHubIdentity(context.Background(), runmode.LocalDefaultUserID, host, "octocat", "", "", "connect_oauth"); err != nil {
		t.Fatalf("bind identity: %v", err)
	}
	pr := seedPRSnapshot(t, s, "acme", "old", 7, domain.PRSnapshot{
		Number: 7, Repo: "acme/old", Author: "octocat", State: "OPEN", Title: "Mine",
	})
	dbseed.RenameRepositoryOnly(t, s.db, pr.RepositoryID, "acme", "new")
	return pr
}

func TestDashboardPRStatus_ReachesGitHubUnderTheRegistryName(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)
	pr := dashboardPRFixture(t, s)
	gh := newFakePRGitHub(t, "acme", "new", "7")

	rec := servePR(t, prRoutes(s, gh), http.MethodGet, "/api/dashboard/prs/"+pr.ID+"/status", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	var status struct {
		MergeableState string `json:"mergeable_state"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &status); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if status.MergeableState != "clean" {
		t.Errorf("mergeable_state = %q, want the fake GitHub's answer for acme/new#7", status.MergeableState)
	}
	if !gh.asked(http.MethodGet, "/repos/acme/new/pulls/7") {
		t.Errorf("GitHub was not asked for acme/new#7; asked %v", gh.paths)
	}
}

func TestDashboardPRRoutes_RefuseAPullRequestOutsideTheViewersList(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)
	dashboardPRFixture(t, s)
	strangers := seedPRSnapshot(t, s, "acme", "new", 8, domain.PRSnapshot{
		Number: 8, Repo: "acme/new", Author: "stranger", State: "OPEN",
	})
	gh := newFakePRGitHub(t, "acme", "new", "8")
	h := prRoutes(s, gh)

	for _, id := range []string{strangers.ID, uuid.NewString(), "acme-new-8"} {
		if rec := servePR(t, h, http.MethodGet, "/api/dashboard/prs/"+id+"/status", ""); rec.Code != http.StatusNotFound {
			t.Errorf("status of %q = %d, want 404 (body=%s)", id, rec.Code, rec.Body.String())
		}
		if rec := servePR(t, h, http.MethodPatch, "/api/dashboard/prs/"+id, `{"draft":true}`); rec.Code != http.StatusNotFound {
			t.Errorf("PATCH of %q = %d, want 404 (body=%s)", id, rec.Code, rec.Body.String())
		}
	}
	if len(gh.paths) != 0 {
		t.Errorf("GitHub was asked %v for pull requests the viewer cannot address", gh.paths)
	}
}

func TestDashboardPRRoutes_RefuseWhatGitHubCannotBeAskedByName(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)
	pr := dashboardPRFixture(t, s)
	gh := newFakePRGitHub(t, "acme", "new", "7")
	h := prRoutes(s, gh)

	t.Run("a retired repository", func(t *testing.T) {
		if _, err := s.db.Exec(`UPDATE repositories SET retired_at = CURRENT_TIMESTAMP WHERE id = ?`, pr.RepositoryID); err != nil {
			t.Fatalf("retire repository: %v", err)
		}
		t.Cleanup(func() {
			if _, err := s.db.Exec(`UPDATE repositories SET retired_at = NULL WHERE id = ?`, pr.RepositoryID); err != nil {
				t.Fatalf("restore repository: %v", err)
			}
		})
		if rec := servePR(t, h, http.MethodGet, "/api/dashboard/prs/"+pr.ID+"/status", ""); rec.Code != http.StatusConflict {
			t.Errorf("status = %d, want 409 (body=%s)", rec.Code, rec.Body.String())
		}
		if rec := servePR(t, h, http.MethodPatch, "/api/dashboard/prs/"+pr.ID, `{"draft":true}`); rec.Code != http.StatusConflict {
			t.Errorf("PATCH = %d, want 409 (body=%s)", rec.Code, rec.Body.String())
		}
	})

	t.Run("a pull request from another GitHub host", func(t *testing.T) {
		entities := sqlitestore.New(s.db).Entities
		other, _, err := entities.FindOrCreate(context.Background(), runmode.LocalDefaultOrgID, "github", "https://ghe.example.com", "acme/new#9", "", "pr", "Elsewhere", "")
		if err != nil {
			t.Fatalf("seed entity: %v", err)
		}
		if ok, err := entities.UpdateSnapshotCASSystem(context.Background(), runmode.LocalDefaultOrgID, other.ID,
			`{"number":9,"repo":"acme/new","author":"octocat","state":"OPEN"}`, other.PollSeq); err != nil || !ok {
			t.Fatalf("seed snapshot: ok=%v err=%v", ok, err)
		}
		if rec := servePR(t, h, http.MethodGet, "/api/dashboard/prs/"+other.ID+"/status", ""); rec.Code != http.StatusConflict {
			t.Errorf("status = %d, want 409 (body=%s)", rec.Code, rec.Body.String())
		}
	})

	if len(gh.paths) != 0 {
		t.Errorf("GitHub was asked %v for pull requests it cannot be asked about by name", gh.paths)
	}
}

func TestDashboardPRUpdate_SetsDraftAndAnswersTheRow(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)
	pr := dashboardPRFixture(t, s)
	gh := newFakePRGitHub(t, "acme", "new", "7")

	rec := servePR(t, prRoutes(s, gh), http.MethodPatch, "/api/dashboard/prs/"+pr.ID, `{"draft":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	var row domain.PRSummaryRow
	if err := json.Unmarshal(rec.Body.Bytes(), &row); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if row.EntityID != pr.ID || row.Number != 7 || !row.Draft {
		t.Errorf("answered %+v, want the pull request's row with draft set", row)
	}
	if !gh.asked(http.MethodGet, "/repos/acme/new/pulls/7") || !gh.asked(http.MethodPost, "/graphql") {
		t.Errorf("GitHub was not asked to convert acme/new#7; asked %v", gh.paths)
	}
	if got := readPRSnapshot(t, s, pr.ID); !got.IsDraft {
		t.Error("the entity's snapshot was not patched to draft")
	}
}

func TestDashboardPRUpdate_RefusesABodyThatWritesNothingValid(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)
	pr := dashboardPRFixture(t, s)
	gh := newFakePRGitHub(t, "acme", "new", "7")
	h := prRoutes(s, gh)

	for _, tc := range []struct {
		body, reason string
	}{
		{`{}`, httpx.ReasonMissingField},
		{`{"draft":null}`, httpx.ReasonInvalidField},
		{`{"draft":"yes"}`, httpx.ReasonInvalidField},
	} {
		rec := servePR(t, h, http.MethodPatch, "/api/dashboard/prs/"+pr.ID, tc.body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("PATCH %s = %d, want 400 (body=%s)", tc.body, rec.Code, rec.Body.String())
			continue
		}
		assertFirstError(t, rec, tc.reason, "draft")
	}
	if rec := servePR(t, h, http.MethodPatch, "/api/dashboard/prs/"+pr.ID, `{"draft":true,"title":"x"}`); rec.Code != http.StatusBadRequest {
		t.Errorf("PATCH with an unknown field = %d, want 400", rec.Code)
	}
	if len(gh.paths) != 0 {
		t.Errorf("GitHub was asked %v by a request that wrote nothing", gh.paths)
	}
}

// TestDashboardPRRoutes_RegisteredOnTheServer drives the server's own mux: the
// routes answer under the entity id path, and a list row hands back the id
// they take.
func TestDashboardPRRoutes_RegisteredOnTheServer(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)
	pr := dashboardPRFixture(t, s)

	out := decodeList[domain.PRSummaryRow](t, doJSON(t, s, "POST", "/api/dashboard/prs/list", map[string]any{}))
	if len(out.Items) != 1 || out.Items[0].EntityID != pr.ID {
		t.Fatalf("list = %+v, want the one pull request carrying entity id %s", out.Items, pr.ID)
	}

	if rec := doJSON(t, s, http.MethodGet, "/api/dashboard/prs/"+uuid.NewString()+"/status", nil); rec.Code != http.StatusNotFound {
		t.Errorf("status of an unknown id = %d, want 404 (body=%s)", rec.Code, rec.Body.String())
	}
	if rec := doJSON(t, s, http.MethodPatch, "/api/dashboard/prs/"+pr.ID, map[string]any{}); rec.Code != http.StatusBadRequest {
		t.Errorf("PATCH {} = %d, want 400 (body=%s)", rec.Code, rec.Body.String())
	}
	if rec := doJSON(t, s, http.MethodPost, "/api/dashboard/prs/"+pr.ID+"/draft", map[string]any{"draft": true}); rec.Code == http.StatusOK {
		t.Error("the draft verb route still answers")
	}
}
