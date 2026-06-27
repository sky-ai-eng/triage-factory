package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/auth/verify"
	"github.com/sky-ai-eng/triage-factory/internal/entitlements"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
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

// Local mode is fully source-available and free, so the probe reports EVERY
// gated feature — even with no license registered. This is the "buying EE in
// local changes nothing" contract: local is already fully entitled.
func TestEntitlements_LocalModeFullyEntitled(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	entitlements.Reset() // no checker registered — local must still report all
	t.Cleanup(entitlements.Reset)

	got := decodeFeatures(t, recordEntitlements())
	if len(got) != len(entitlements.AllFeatures) {
		t.Fatalf("features = %v, want all %d gated features in local mode", got, len(entitlements.AllFeatures))
	}
	for _, f := range entitlements.AllFeatures {
		if !featuresContain(got, string(f)) {
			t.Fatalf("features = %v, missing %q — local must report every feature", got, f)
		}
	}
}

// Multi mode, no license (community default): the probe reports an EMPTY set —
// and crucially `[]`, not null, so the frontend can treat it as a set.
func TestEntitlements_MultiCommunityDefaultIsEmpty(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	entitlements.Reset()
	t.Cleanup(entitlements.Reset)

	rec := recordEntitlements()
	if got := decodeFeatures(t, rec); len(got) != 0 {
		t.Fatalf("features = %v, want empty under the multi-mode community default", got)
	}
	if body := rec.Body.String(); !jsonHasEmptyFeaturesArray(body) {
		t.Fatalf("body = %s, want a features:[] array (not null)", body)
	}
}

// Multi mode with a checker that grants governance: the probe reports exactly
// ["governance"] — the licensed subset of entitlements.AllFeatures.
func TestEntitlements_MultiReportsGrantedFeature(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	entitlements.Register(governanceGrant{})
	t.Cleanup(entitlements.Reset)

	got := decodeFeatures(t, recordEntitlements())
	if len(got) != 1 || got[0] != string(entitlements.FeatureGovernance) {
		t.Fatalf("features = %v, want [%q]", got, entitlements.FeatureGovernance)
	}
}

// The route is authenticated-session-only, in any mode: no claims in context →
// 401, never a 200 that would leak the deployment's feature state anonymously.
func TestEntitlements_RequiresSession(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	entitlements.Register(governanceGrant{})
	t.Cleanup(entitlements.Reset)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/entitlements", nil) // no WithClaims
	(&Server{}).handleEntitlements(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 for an unauthenticated request", rec.Code)
	}
}

// recordEntitlements drives the handler with an authenticated request and
// returns the recorder.
func recordEntitlements() *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	(&Server{}).handleEntitlements(rec, authedReq())
	return rec
}

func featuresContain(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
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
