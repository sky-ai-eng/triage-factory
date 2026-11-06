package deliverables

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/auth/verify"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/github/githubapp"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server/authz"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// recordingResolver hands out clients for one stub GitHub and counts every
// request for one, so a test can show a route never resolved an upstream
// client at all.
type recordingResolver struct {
	url   string
	mu    sync.Mutex
	calls int
}

func (r *recordingResolver) client() (*ghclient.Client, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	return ghclient.NewClient(r.url, ""), nil
}

func (r *recordingResolver) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

func (r *recordingResolver) ClientFor(context.Context, string, string) (*ghclient.Client, error) {
	return r.client()
}

func (r *recordingResolver) ClientForInstallation(context.Context, string, domain.OrgGitHubAppInstallation) (*ghclient.Client, error) {
	return r.client()
}

func (r *recordingResolver) ClientForRepo(context.Context, string, string, string) (*ghclient.Client, error) {
	return r.client()
}

func (r *recordingResolver) TokenFor(context.Context, string, string) (githubapp.Token, error) {
	r.mu.Lock()
	r.calls++
	r.mu.Unlock()
	return githubapp.Token{}, errors.New("no token in this test")
}

func (r *recordingResolver) BaseURLFor(context.Context, string) (string, error) {
	return r.url, nil
}

func (r *recordingResolver) OrgIdentityFor(context.Context, string) (string, string, bool) {
	return "", "", false
}

// TestDeliverableWriteGate_Postgres pins the write gate on the deliverable-id
// routes under live RLS. An org admin on another team, and a viewer of the
// deliverable's own team, can both read the deliverable; every route that acts
// on it answers them 403 before resolving a GitHub client, while GET answers
// the org admin 200 and a writer of the team still gets through.
func TestDeliverableWriteGate_Postgres(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	h := pgtest.Shared(t)
	h.Reset(t)
	ctx := context.Background()

	orgID, writer, teamA := pgtest.SeedOrgWithUser(t, h, "gate-writer")
	teamB := pgtest.SeedTeam(t, h, orgID, "team-b")
	orgAdmin := pgtest.SeedUser(t, h, "gate-org-admin")
	pgtest.AddOrgMember(t, h, orgAdmin, orgID, teamB, "admin", "member")
	viewer := pgtest.SeedUser(t, h, "gate-viewer")
	pgtest.AddOrgMember(t, h, viewer, orgID, teamA, "member", "viewer")

	seed := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	if _, _, err := seed.Orgs.AdoptSourceBaseURL(ctx, orgID, domain.ProviderGitHub, dbtest.TestGitHubHost); err != nil {
		t.Fatalf("set the org's GitHub host: %v", err)
	}

	pr := domain.NewPullRequestDeliverable("octo/repo", 7, "PR_octo_repo_7", "feature/x", "main",
		"https://github.com/octo/repo/pull/7", "Proposed title", "Proposed body", true)
	pr.OrgID, pr.TeamID, pr.Scope = orgID, teamA, dbtest.TestGitHubHost
	pr.Verification = domain.DeliverableVerificationVerified
	prRow, err := seed.Deliverables.UpsertSystem(ctx, orgID, pr)
	if err != nil {
		t.Fatalf("seed PR deliverable: %v", err)
	}

	line := 3
	review := domain.NewReviewDeliverable("octo/repo", 7, "PR_octo_repo_7", "headsha", "")
	review.OrgID, review.TeamID, review.Scope = orgID, teamA, dbtest.TestGitHubHost
	details, _ := domain.ParseReviewDeliverableDetails(review.DetailsJSON)
	details.ReviewBody = "## Review\nlgtm"
	details.ReviewEvent = "COMMENT"
	details.StagedComments = []domain.ReviewDeliverableComment{{ID: "c_1", Path: "a.go", Line: &line, Body: "nit: rename"}}
	review.DetailsJSON = domain.MarshalReviewDeliverableDetails(details)
	reviewRow, err := seed.Deliverables.UpsertSystem(ctx, orgID, review)
	if err != nil {
		t.Fatalf("seed review deliverable: %v", err)
	}

	var (
		hitsMu sync.Mutex
		hits   []string
	)
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hitsMu.Lock()
		hits = append(hits, r.Method+" "+r.URL.Path)
		hitsMu.Unlock()
		if r.Method == http.MethodGet && r.URL.Path == "/api/v3/repos/octo/repo/pulls/7" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"number": 7, "node_id": "PR_octo_repo_7", "state": "open", "draft": true,
				"title": "Live title", "body": "Live body",
			})
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(gh.Close)
	upstreamHits := func() []string {
		hitsMu.Lock()
		defer hitsMu.Unlock()
		return append([]string(nil), hits...)
	}

	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	resolver := &recordingResolver{url: gh.URL}
	mux := http.NewServeMux()
	mount := func(pattern string, hf http.HandlerFunc) { mux.HandleFunc(pattern, hf) }
	New(stores.Tx, authz.New(h.AdminDB, stores.Tx), nil, resolver, nil, nil, func() string { return "" }).
		Register(httpx.Mounts{API: mount, APIMutating: mount})

	do := func(caller, method, path string, body any) *httptest.ResponseRecorder {
		t.Helper()
		var raw []byte
		if body != nil {
			raw, _ = json.Marshal(body)
		}
		req := httptest.NewRequest(method, path, bytes.NewReader(raw))
		req.Header.Set("Content-Type", "application/json")
		reqCtx := httpx.WithOrgID(req.Context(), orgID)
		reqCtx = httpx.WithClaims(reqCtx, &verify.Claims{Subject: caller})
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req.WithContext(reqCtx))
		return rec
	}

	prPath := "/api/deliverables/" + prRow.ID
	reviewPath := "/api/deliverables/" + reviewRow.ID
	writes := []struct {
		method, path string
		body         any
	}{
		{http.MethodPatch, prPath + "/pr", map[string]any{"title": "New title"}},
		{http.MethodPost, prPath + "/approve", nil},
		{http.MethodPost, prPath + "/dismiss", nil},
		{http.MethodPost, prPath + "/reject", nil},
		{http.MethodPatch, reviewPath + "/review", map[string]any{"body": "changed"}},
		{http.MethodPost, reviewPath + "/approve", nil},
		{http.MethodPost, reviewPath + "/review/refresh", nil},
		{http.MethodPatch, reviewPath + "/comments/c_1", map[string]any{"body": "rename it"}},
		{http.MethodDelete, reviewPath + "/comments/c_1", nil},
	}

	t.Run("writes_refused_403_before_github", func(t *testing.T) {
		for _, caller := range []struct{ name, id string }{{"off-team org admin", orgAdmin}, {"team viewer", viewer}} {
			for _, wr := range writes {
				rec := do(caller.id, wr.method, wr.path, wr.body)
				if rec.Code != http.StatusForbidden {
					t.Errorf("%s: %s %s = %d, want 403; body=%s", caller.name, wr.method, wr.path, rec.Code, rec.Body.String())
					continue
				}
				var env struct {
					Errors []httpx.ErrorItem `json:"errors"`
				}
				if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil || len(env.Errors) != 1 || env.Errors[0].Reason != httpx.ReasonForbidden {
					t.Errorf("%s: %s %s body = %s, want one FORBIDDEN error", caller.name, wr.method, wr.path, rec.Body.String())
				}
			}
		}
		if n := resolver.count(); n != 0 {
			t.Errorf("refused writes resolved %d GitHub clients, want 0", n)
		}
		if got := upstreamHits(); len(got) != 0 {
			t.Errorf("refused writes reached GitHub: %v", got)
		}
		var actions int
		if err := h.AdminDB.QueryRow(`SELECT COUNT(*) FROM external_actions WHERE org_id = $1`, orgID).Scan(&actions); err != nil {
			t.Fatalf("count external_actions: %v", err)
		}
		if actions != 0 {
			t.Errorf("external_actions = %d after refused writes, want 0", actions)
		}
		for _, want := range []domain.Deliverable{prRow, reviewRow} {
			got, err := seed.Deliverables.Get(ctx, orgID, want.ID)
			if err != nil || got == nil {
				t.Fatalf("re-read deliverable %s: %v, %v", want.ID, got, err)
			}
			if got.State != want.State || got.DetailsJSON != want.DetailsJSON {
				t.Errorf("deliverable %s = (%s, %s), want it unchanged (%s, %s)", want.ID, got.State, got.DetailsJSON, want.State, want.DetailsJSON)
			}
		}
	})

	t.Run("get_answers_the_org_admin", func(t *testing.T) {
		rec := do(orgAdmin, http.MethodGet, prPath, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("org admin GET PR = %d, want 200; body=%s", rec.Code, rec.Body.String())
		}
		var got struct {
			ID      string `json:"id"`
			Details struct {
				Title string `json:"title"`
			} `json:"details"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode GET: %v", err)
		}
		if got.ID != prRow.ID || got.Details.Title != "Live title" {
			t.Errorf("org admin GET PR = %+v, want %s with the live title", got, prRow.ID)
		}
		if rec := do(orgAdmin, http.MethodGet, reviewPath, nil); rec.Code != http.StatusOK {
			t.Errorf("org admin GET review = %d, want 200; body=%s", rec.Code, rec.Body.String())
		}
	})

	t.Run("a_team_writer_gets_through", func(t *testing.T) {
		rec := do(writer, http.MethodPatch, reviewPath+"/review", map[string]any{"body": "changed"})
		if rec.Code != http.StatusOK {
			t.Fatalf("writer PATCH review = %d, want 200; body=%s", rec.Code, rec.Body.String())
		}
		got, err := seed.Deliverables.Get(ctx, orgID, reviewRow.ID)
		if err != nil || got == nil {
			t.Fatalf("re-read review: %v, %v", got, err)
		}
		if d, _ := domain.ParseReviewDeliverableDetails(got.DetailsJSON); d.ReviewBody != "changed" {
			t.Errorf("review body = %q after the writer's PATCH, want %q", d.ReviewBody, "changed")
		}
	})
}
