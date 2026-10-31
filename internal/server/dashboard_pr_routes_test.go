package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
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

// fakePRGitHub answers the status reads and the draft mutation for one pull
// request and 404s every other path, recording each request as
// "METHOD path" without the API prefix. duringMutation, when set, runs
// while the draft mutation is in flight.
type fakePRGitHub struct {
	url            string
	mu             sync.Mutex
	paths          []string
	duringMutation func()
}

func newFakePRGitHub(t *testing.T, owner, repo, number string) *fakePRGitHub {
	t.Helper()
	f := &fakePRGitHub{}
	pull := "/repos/" + owner + "/" + repo + "/pulls/" + number
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Any host but github.com is GitHub Enterprise: REST under /api/v3,
		// GraphQL at /api/graphql.
		req := r.Method + " " + strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, "/api/v3"), "/api")
		f.mu.Lock()
		f.paths = append(f.paths, req)
		during := f.duringMutation
		f.mu.Unlock()
		switch req {
		case "GET " + pull:
			_, _ = w.Write([]byte(`{"node_id":"PR_node","mergeable":true,"mergeable_state":"clean","head":{"sha":""}}`))
		case "GET " + pull + "/reviews":
			_, _ = w.Write([]byte(`[]`))
		case "POST /graphql":
			if during != nil {
				during()
			}
			_, _ = w.Write([]byte(`{"data":{"convertPullRequestToDraft":{"pullRequest":{"isDraft":true}}}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	f.url = srv.URL
	return f
}

func (f *fakePRGitHub) asked() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.paths)
}

// prRoutes serves the per-PR routes against gh, with the local org and user
// on every request as the session middleware would put them.
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

// dashboardPRFixture seeds acme/old#7 authored by the local user's GitHub
// login, then renames the registry row to acme/new without touching the
// entity's key, as a GitHub rename leaves it until the next poll.
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

func TestDashboardPRRoutes_ReachGitHubUnderTheRegistryName(t *testing.T) {
	s := newTestServer(t)
	pr := dashboardPRFixture(t, s)
	gh := newFakePRGitHub(t, "acme", "new", "7")
	h := prRoutes(s, gh)

	rec := servePR(t, h, http.MethodGet, "/api/dashboard/prs/"+pr.ID+"/status", "")
	var status struct {
		MergeableState string `json:"mergeable_state"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &status); err != nil || rec.Code != http.StatusOK || status.MergeableState != "clean" {
		t.Fatalf("status = %d %s, want 200 with the fake's answer for acme/new#7", rec.Code, rec.Body.String())
	}

	rec = servePR(t, h, http.MethodPatch, "/api/dashboard/prs/"+pr.ID, `{"draft":true}`)
	var row domain.PRSummaryRow
	if err := json.Unmarshal(rec.Body.Bytes(), &row); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("PATCH = %d %s, want 200", rec.Code, rec.Body.String())
	}
	if row.EntityID != pr.ID || row.Number != 7 || !row.Draft {
		t.Errorf("answered %+v, want the pull request's row with draft set", row)
	}
	if asked := gh.asked(); !slices.Contains(asked, "GET /repos/acme/new/pulls/7") || !slices.Contains(asked, "POST /graphql") {
		t.Errorf("GitHub was not asked about acme/new#7; asked %v", asked)
	}
	if got := readPRSnapshot(t, s, pr.ID); !got.IsDraft {
		t.Error("the entity's snapshot was not patched to draft")
	}
	// The drag is recorded as a change to the pull request, by its entity.
	acts, _, err := sqlitestore.New(s.db).ExternalActions.ListByOrgSystem(context.Background(), runmode.LocalDefaultOrgID,
		domain.ExternalActionListOpts{Action: domain.ActionPRConvertedToDraft})
	if err != nil {
		t.Fatalf("list actions: %v", err)
	}
	if len(acts) != 1 || acts[0].ObjectKind != domain.ObjectKindPullRequest || acts[0].EntityID != pr.ID {
		t.Errorf("pr_converted_to_draft rows = %+v, want one naming the pull request %s", acts, pr.ID)
	}
}

// The handler resolves the entity before the GitHub round trip, and a poll
// can commit a newer snapshot during it. Setting is_draft must keep that
// snapshot: putting the older copy back would undo the poll's transitions,
// and the next poll would emit them again.
func TestDashboardPRUpdate_KeepsASnapshotAPollCommitsDuringTheMutation(t *testing.T) {
	s := newTestServer(t)
	pr := dashboardPRFixture(t, s)
	gh := newFakePRGitHub(t, "acme", "new", "7")
	ctx, org := context.Background(), runmode.LocalDefaultOrgID
	gh.mu.Lock()
	gh.duringMutation = func() {
		cur, err := s.allStores.Entities.Get(ctx, org, pr.ID)
		if err != nil || cur == nil {
			t.Errorf("read during the mutation: %v", err)
			return
		}
		snap := readPRSnapshot(t, s, pr.ID)
		snap.HeadSHA, snap.Title = "newsha", "Mine v2"
		data, _ := json.Marshal(snap)
		if ok, err := s.allStores.Entities.UpdateSnapshotCASSystem(ctx, org, pr.ID, string(data), cur.PollSeq); err != nil || !ok {
			t.Errorf("poll write during the mutation: ok=%v err=%v", ok, err)
		}
	}
	gh.mu.Unlock()

	rec := servePR(t, prRoutes(s, gh), http.MethodPatch, "/api/dashboard/prs/"+pr.ID, `{"draft":true}`)
	var row domain.PRSummaryRow
	if err := json.Unmarshal(rec.Body.Bytes(), &row); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("PATCH = %d %s, want 200", rec.Code, rec.Body.String())
	}
	if got := readPRSnapshot(t, s, pr.ID); !got.IsDraft || got.HeadSHA != "newsha" || got.Title != "Mine v2" {
		t.Errorf("stored snapshot = %+v, want the poll's snapshot with is_draft set", got)
	}
	if !row.Draft || row.Title != "Mine v2" {
		t.Errorf("answered %+v, want the stored row with draft set", row)
	}
}

func TestDashboardPRRoutes_RefuseAPullRequestOutsideTheViewersList(t *testing.T) {
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
	if asked := gh.asked(); len(asked) != 0 {
		t.Errorf("GitHub was asked %v for pull requests the viewer cannot address", asked)
	}
}

// A retired repository and a pull request polled from another GitHub host are
// in the viewer's list, but GitHub cannot be asked about either by name.
func TestDashboardPRRoutes_RefuseWhatGitHubCannotBeAskedByName(t *testing.T) {
	s := newTestServer(t)
	pr := dashboardPRFixture(t, s)
	gh := newFakePRGitHub(t, "acme", "new", "7")
	h := prRoutes(s, gh)

	execSQL(t, s.db, `UPDATE repositories SET retired_at = CURRENT_TIMESTAMP WHERE id = ?`, pr.RepositoryID)
	if rec := servePR(t, h, http.MethodGet, "/api/dashboard/prs/"+pr.ID+"/status", ""); rec.Code != http.StatusConflict {
		t.Errorf("status of a retired repository's PR = %d, want 409 (body=%s)", rec.Code, rec.Body.String())
	}
	if rec := servePR(t, h, http.MethodPatch, "/api/dashboard/prs/"+pr.ID, `{"draft":true}`); rec.Code != http.StatusConflict {
		t.Errorf("PATCH of a retired repository's PR = %d, want 409 (body=%s)", rec.Code, rec.Body.String())
	}

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
		t.Errorf("status of another host's PR = %d, want 409 (body=%s)", rec.Code, rec.Body.String())
	}

	if asked := gh.asked(); len(asked) != 0 {
		t.Errorf("GitHub was asked %v for pull requests it cannot be asked about by name", asked)
	}
}

func TestDashboardPRUpdate_RefusesABodyThatWritesNothingValid(t *testing.T) {
	s := newTestServer(t)
	pr := dashboardPRFixture(t, s)
	gh := newFakePRGitHub(t, "acme", "new", "7")
	h := prRoutes(s, gh)

	for _, tc := range []struct{ body, reason, field string }{
		{`{}`, httpx.ReasonMissingField, "draft"},
		{`{"draft":null}`, httpx.ReasonInvalidField, "draft"},
		{`{"draft":"yes"}`, httpx.ReasonInvalidField, "draft"},
		{`{"draft":true,"title":"x"}`, httpx.ReasonUnknownField, "title"},
	} {
		assertOneFault(t, servePR(t, h, http.MethodPatch, "/api/dashboard/prs/"+pr.ID, tc.body), http.StatusBadRequest, tc.reason, tc.field)
	}
	if asked := gh.asked(); len(asked) != 0 {
		t.Errorf("GitHub was asked %v by a request that wrote nothing", asked)
	}
}

// The server's own mux answers under the entity id path, and a list row hands
// back the id the routes take.
func TestDashboardPRRoutes_RegisteredOnTheServer(t *testing.T) {
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
		t.Error("POST .../draft answers; the draft write is the PATCH")
	}
}
