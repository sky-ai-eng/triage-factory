package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/auth/verify"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/placement"
	"github.com/sky-ai-eng/triage-factory/internal/server/authz"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

type fakeExplainer struct {
	plan    placement.Plan
	lastKey [2]string // org, repository id
}

func (f *fakeExplainer) Explain(_ context.Context, org, repositoryID string) (placement.Plan, error) {
	f.lastKey = [2]string{org, repositoryID}
	return f.plan, nil
}

// placementRepos answers the registry read with the rows it holds, keyed by
// host and case-folded name, the way GetByRefSystem matches.
type placementRepos struct {
	db.RepositoryStore
	rows []domain.Repository
}

func (p placementRepos) GetByRefSystem(_ context.Context, _ string, ref domain.RepoRef) (*domain.Repository, error) {
	for _, r := range p.rows {
		if r.Host == ref.Host && strings.EqualFold(r.Slug(), ref.Slug()) {
			return &r, nil
		}
	}
	return nil, nil
}

// placementOrgs answers the settings read with a fixed GitHub base URL — the
// org's current host the explainer keys the override on.
type placementOrgs struct {
	db.OrgsStore
	githubBase string
}

func (o placementOrgs) GetSettingsSystem(context.Context, string) (domain.OrgSettings, error) {
	return domain.OrgSettings{GitHubBaseURL: o.githubBase}, nil
}

// localModeReq builds a GET request carrying local-mode claims so
// RequireOrgAdminRole short-circuits to authorized (local mode) without a DB.
func localModeReq(target string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, target, nil)
	return r.WithContext(httpx.WithClaims(r.Context(), &verify.Claims{Subject: "u1"}))
}

// fleetRepos is the registry the handler tests resolve ?repo= against: acme/web
// on the org's host, and the same name on another host.
var fleetRepos = placementRepos{rows: []domain.Repository{
	{ID: "repo-web", Host: "https://ghe.example.com", Owner: "acme", Repo: "Web"},
	{ID: "repo-web-dotcom", Host: "https://github.com", Owner: "acme", Repo: "web"},
}}

func TestHandleFleetPlacement_ReturnsCandidateOrder(t *testing.T) {
	fe := &fakeExplainer{plan: placement.Plan{
		Enabled: true, OrgID: "org1", RepositoryID: "repo-web",
		PreferredSet: []string{"exec-a"},
		Aging:        20 * time.Second, Liveness: 12 * time.Second,
		Candidates: []placement.Candidate{
			{InstanceID: "exec-a", Weight: 8, Score: 14.2, Eligible: true, Preferred: true},
			{InstanceID: "exec-b", Weight: 4, Score: 5.5, Eligible: true},
			{InstanceID: "exec-dead", Reason: "dead"},
		},
	}}
	s := &Server{az: &authz.Checker{}, placement: fe, orgs: placementOrgs{githubBase: "https://ghe.example.com/"}, repos: fleetRepos}

	rec := httptest.NewRecorder()
	s.handleFleetPlacement(rec, localModeReq("/api/fleet/placement?org=org1&repo=acme/web"))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var dto placementExplainDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &dto); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !dto.Enabled || dto.KeyKind != "repo" || dto.RepositoryID != "repo-web" || len(dto.Candidates) != 3 {
		t.Fatalf("unexpected dto: %+v", dto)
	}
	// The repository's current name and host, off its registry row.
	if dto.Repo != "acme/Web" || dto.Host != "https://ghe.example.com" {
		t.Fatalf("dto names %s on %s, want the registry row's acme/Web on https://ghe.example.com", dto.Repo, dto.Host)
	}
	if dto.Candidates[0].InstanceID != "exec-a" || !dto.Candidates[0].Preferred {
		t.Fatalf("top candidate should be the preferred owner: %+v", dto.Candidates)
	}
	if dto.PreferredSet[0] != "exec-a" || dto.AgingSeconds != 20 {
		t.Fatalf("preferred/aging wrong: %+v", dto)
	}
	// The handler resolved the name on the org's current GitHub host to its
	// registry row, and keyed the resolver on the row's id.
	if fe.lastKey != [2]string{"org1", "repo-web"} {
		t.Fatalf("resolver keyed on %v", fe.lastKey)
	}
}

func TestHandleFleetPlacement_UnregisteredRepoIsNotFound(t *testing.T) {
	fe := &fakeExplainer{}
	s := &Server{az: &authz.Checker{}, placement: fe, orgs: placementOrgs{githubBase: "https://ghe.example.com"}, repos: fleetRepos}
	rec := httptest.NewRecorder()
	s.handleFleetPlacement(rec, localModeReq("/api/fleet/placement?org=org1&repo=acme/api"))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("a name no registry row answers to should 404, got %d: %s", rec.Code, rec.Body.String())
	}
	if fe.lastKey != [2]string{} {
		t.Fatalf("the resolver was consulted for %v with no registry row", fe.lastKey)
	}
}

func TestHandleFleetPlacement_RejectsAMalformedRepo(t *testing.T) {
	for _, repo := range []string{"acme", "acme/", "/web", "acme/web/extra"} {
		s := &Server{az: &authz.Checker{}, placement: &fakeExplainer{}, repos: fleetRepos}
		rec := httptest.NewRecorder()
		s.handleFleetPlacement(rec, localModeReq("/api/fleet/placement?org=org1&repo="+repo))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("repo=%q: status %d, want 400", repo, rec.Code)
		}
	}
}

func TestHandleFleetPlacement_RequiresRepoParam(t *testing.T) {
	s := &Server{az: &authz.Checker{}, placement: &fakeExplainer{}}
	rec := httptest.NewRecorder()
	s.handleFleetPlacement(rec, localModeReq("/api/fleet/placement?org=org1"))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing repo should 400, got %d", rec.Code)
	}
}

func TestHandleFleetPlacement_RequiresOrgParam(t *testing.T) {
	s := &Server{az: &authz.Checker{}, placement: &fakeExplainer{}}
	rec := httptest.NewRecorder()
	s.handleFleetPlacement(rec, localModeReq("/api/fleet/placement?repo=acme/web"))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing org should 400, got %d", rec.Code)
	}
}

func TestHandleFleetPlacement_RejectsBadKind(t *testing.T) {
	s := &Server{az: &authz.Checker{}, placement: &fakeExplainer{}}
	rec := httptest.NewRecorder()
	s.handleFleetPlacement(rec, localModeReq("/api/fleet/placement?org=org1&repo=acme/web&kind=bogus"))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad kind should 400, got %d", rec.Code)
	}
}

func TestHandleFleetPlacement_UnwiredResolverNotConfigured(t *testing.T) {
	s := &Server{az: &authz.Checker{}} // placement nil
	rec := httptest.NewRecorder()
	s.handleFleetPlacement(rec, localModeReq("/api/fleet/placement?org=org1&repo=acme/web"))
	if rec.Code != http.StatusConflict {
		t.Fatalf("nil resolver should 409 NOT_CONFIGURED, got %d", rec.Code)
	}
}
