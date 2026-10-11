package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
	lastKey [4]string // org, kind, host, value
}

func (f *fakeExplainer) Explain(_ context.Context, org, kind, host, val string) (placement.Plan, error) {
	f.lastKey = [4]string{org, kind, host, val}
	return f.plan, nil
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

func TestHandleFleetPlacement_ReturnsCandidateOrder(t *testing.T) {
	fe := &fakeExplainer{plan: placement.Plan{
		Enabled: true, OrgID: "org1", KeyKind: "repo", Host: "https://ghe.example.com", KeyValue: "acme/web",
		PreferredSet: []string{"exec-a"},
		Aging:        20 * time.Second, Liveness: 12 * time.Second,
		Candidates: []placement.Candidate{
			{InstanceID: "exec-a", Weight: 8, Score: 14.2, Eligible: true, Preferred: true},
			{InstanceID: "exec-b", Weight: 4, Score: 5.5, Eligible: true},
			{InstanceID: "exec-dead", Reason: "dead"},
		},
	}}
	s := &Server{az: &authz.Checker{}, placement: fe, orgs: placementOrgs{githubBase: "https://ghe.example.com/"}}

	rec := httptest.NewRecorder()
	s.handleFleetPlacement(rec, localModeReq("/api/fleet/placement?org=org1&repo=acme/web"))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var dto placementExplainDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &dto); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !dto.Enabled || dto.KeyValue != "acme/web" || dto.Host != "https://ghe.example.com" || len(dto.Candidates) != 3 {
		t.Fatalf("unexpected dto: %+v", dto)
	}
	if dto.Candidates[0].InstanceID != "exec-a" || !dto.Candidates[0].Preferred {
		t.Fatalf("top candidate should be the preferred owner: %+v", dto.Candidates)
	}
	if dto.PreferredSet[0] != "exec-a" || dto.AgingSeconds != 20 {
		t.Fatalf("preferred/aging wrong: %+v", dto)
	}
	// The handler derived the (org, repo) key it passed to the resolver, on
	// the org's current GitHub host.
	if fe.lastKey != [4]string{"org1", "repo", "https://ghe.example.com", "acme/web"} {
		t.Fatalf("resolver keyed on %v", fe.lastKey)
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
