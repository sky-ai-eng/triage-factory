package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/auth/verify"
	"github.com/sky-ai-eng/triage-factory/internal/entitlements"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// governanceGrant licenses ONLY the governance feature — the grant-pattern
// mirror of ee/sso/rig_test.go's ssoLicensedGrant, used to prove the probe
// reports a feature once a checker registers it.
type governanceGrant struct{}

func (governanceGrant) Has(f entitlements.Feature) bool { return f == entitlements.FeatureGovernance }

// authedReq builds a GET /api/entitlements request carrying a verified-session
// claim in context, the way withSession would seed it before the handler runs.
// The handler reads no Server fields, so a bare &Server{} drives it without the
// auth/pg stack — the entitlements seam is process-global, not request-scoped.
func authedReq() *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/api/entitlements", nil)
	ctx := httpx.WithClaims(req.Context(), &verify.Claims{Subject: "test-user"})
	return req.WithContext(ctx)
}

func decodeFeatures(t *testing.T, rec *httptest.ResponseRecorder) []string {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	var resp struct {
		Features []string `json:"features"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v (body: %s)", err, rec.Body.String())
	}
	return resp.Features
}

// Community default (no license registered): the probe reports an EMPTY set —
// and crucially `[]`, not null, so the frontend can treat it as a set.
func TestEntitlements_CommunityDefaultIsEmpty(t *testing.T) {
	entitlements.Reset()
	t.Cleanup(entitlements.Reset)

	rec := httptest.NewRecorder()
	(&Server{}).handleEntitlements(rec, authedReq())

	if got := decodeFeatures(t, rec); len(got) != 0 {
		t.Fatalf("features = %v, want empty under the community default", got)
	}
	// Assert the wire form is [] (a non-nil JSON array), never null.
	if body := rec.Body.String(); !jsonHasEmptyFeaturesArray(body) {
		t.Fatalf("body = %s, want a features:[] array (not null)", body)
	}
}

// A registered checker that grants governance: the probe reports exactly
// ["governance"] — the licensed subset of entitlements.AllFeatures.
func TestEntitlements_ReportsGrantedFeature(t *testing.T) {
	entitlements.Register(governanceGrant{})
	t.Cleanup(entitlements.Reset)

	rec := httptest.NewRecorder()
	(&Server{}).handleEntitlements(rec, authedReq())

	got := decodeFeatures(t, rec)
	if len(got) != 1 || got[0] != string(entitlements.FeatureGovernance) {
		t.Fatalf("features = %v, want [%q]", got, entitlements.FeatureGovernance)
	}
}

// The route is authenticated-session-only: no claims in context → 401, never a
// 200 that would leak the deployment's feature state to an anonymous caller.
func TestEntitlements_RequiresSession(t *testing.T) {
	entitlements.Register(governanceGrant{})
	t.Cleanup(entitlements.Reset)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/entitlements", nil) // no WithClaims
	(&Server{}).handleEntitlements(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 for an unauthenticated request", rec.Code)
	}
}

// jsonHasEmptyFeaturesArray confirms the encoded body carries an empty JSON
// array for features (guards the make([]string, 0, …) → [] contract, since a
// nil slice would encode as null and break the frontend's set semantics).
func jsonHasEmptyFeaturesArray(body string) bool {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &raw); err != nil {
		return false
	}
	f, ok := raw["features"]
	return ok && string(f) == "[]"
}
