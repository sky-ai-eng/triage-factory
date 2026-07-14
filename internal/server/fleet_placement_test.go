package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/auth/verify"
	"github.com/sky-ai-eng/triage-factory/internal/placement"
	"github.com/sky-ai-eng/triage-factory/internal/server/authz"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

type fakeExplainer struct {
	plan    placement.Plan
	lastKey [3]string // org, kind, value
}

func (f *fakeExplainer) Explain(_ context.Context, org, kind, val string) (placement.Plan, error) {
	f.lastKey = [3]string{org, kind, val}
	return f.plan, nil
}

// localModeReq builds a GET request carrying local-mode claims so
// RequireOrgAdminRole short-circuits to authorized (local mode) without a DB.
func localModeReq(target string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, target, nil)
	return r.WithContext(httpx.WithClaims(r.Context(), &verify.Claims{Subject: "u1"}))
}

func TestHandleFleetPlacement_ReturnsCandidateOrder(t *testing.T) {
	fe := &fakeExplainer{plan: placement.Plan{
		Enabled: true, OrgID: "org1", KeyKind: "repo", KeyValue: "acme/web",
		PreferredSet: []string{"exec-a"},
		Aging:        20 * time.Second, Liveness: 12 * time.Second,
		Candidates: []placement.Candidate{
			{InstanceID: "exec-a", Weight: 8, Score: 14.2, Eligible: true, Preferred: true},
			{InstanceID: "exec-b", Weight: 4, Score: 5.5, Eligible: true},
			{InstanceID: "exec-dead", Reason: "dead"},
		},
	}}
	s := &Server{az: &authz.Checker{}, placement: fe}

	rec := httptest.NewRecorder()
	s.handleFleetPlacement(rec, localModeReq("/api/fleet/placement?org=org1&repo=acme/web"))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var dto placementExplainDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &dto); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !dto.Enabled || dto.KeyValue != "acme/web" || len(dto.Candidates) != 3 {
		t.Fatalf("unexpected dto: %+v", dto)
	}
	if dto.Candidates[0].InstanceID != "exec-a" || !dto.Candidates[0].Preferred {
		t.Fatalf("top candidate should be the preferred owner: %+v", dto.Candidates)
	}
	if dto.PreferredSet[0] != "exec-a" || dto.AgingSeconds != 20 {
		t.Fatalf("preferred/aging wrong: %+v", dto)
	}
	// The handler derived the (org, repo) key it passed to the resolver.
	if fe.lastKey != [3]string{"org1", "repo", "acme/web"} {
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

func TestHandleFleetPlacement_UnwiredResolver503(t *testing.T) {
	s := &Server{az: &authz.Checker{}} // placement nil
	rec := httptest.NewRecorder()
	s.handleFleetPlacement(rec, localModeReq("/api/fleet/placement?org=org1&repo=acme/web"))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("nil resolver should 503, got %d", rec.Code)
	}
}
