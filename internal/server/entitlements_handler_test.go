package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/auth/verify"
	"github.com/sky-ai-eng/triage-factory/internal/entitlements"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server/authz"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// entitlementsReq builds a GET of orgID's entitlements carrying a
// verified-session claim, the way withSession would seed it before the
// handler runs. Local mode's member gate admits its sentinel org with no
// database, so a Server holding only an authz Checker drives it; the
// entitlements provider is process-global, not request-scoped.
func entitlementsReq(orgID string, withClaims bool) *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/api/orgs/"+orgID+"/entitlements", nil)
	req.SetPathValue("org_id", orgID)
	if withClaims {
		req = req.WithContext(httpx.WithClaims(req.Context(), &verify.Claims{Subject: "test-user"}))
	}
	return req
}

func recordEntitlements(orgID string, withClaims bool) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	(&Server{az: authz.New(nil, nil)}).handleEntitlements(rec, entitlementsReq(orgID, withClaims))
	return rec
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

// TestEntitlements_LocalReportsTheProvider: the probe reports the checker's
// state and nothing about runmode, so an unlicensed local build reports []
// and local mode gets no entitlement pass. The multi arm of the same rule is
// TestEntitlements_MultiReadsTheNamedOrg.
func TestEntitlements_LocalReportsTheProvider(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)

	t.Run("unlicensed_is_empty", func(t *testing.T) {
		entitlements.Reset()
		t.Cleanup(entitlements.Reset)

		rec := recordEntitlements(runmode.LocalDefaultOrgID, true)
		if got := decodeFeatures(t, rec); len(got) != 0 {
			t.Fatalf("features = %v, want [] for an unlicensed local build", got)
		}
		// Wire form must be [] (a non-nil JSON array), never null.
		if body := rec.Body.String(); !jsonHasEmptyFeaturesArray(body) {
			t.Fatalf("body = %s, want a features:[] array (not null)", body)
		}
	})

	t.Run("governance_granted", func(t *testing.T) {
		entitlements.RegisterProvider(entitlements.Static(entitlements.FeatureGovernance))
		t.Cleanup(entitlements.Reset)

		got := decodeFeatures(t, recordEntitlements(runmode.LocalDefaultOrgID, true))
		if len(got) != 1 || got[0] != string(entitlements.FeatureGovernance) {
			t.Fatalf("features = %v, want [%q]", got, entitlements.FeatureGovernance)
		}
	})
}

// The route needs a credential: no claims in context → 401, never a 200 that
// would leak the deployment's feature state to an anonymous caller. An org the
// caller cannot reach is 404, as on every org-addressed route.
func TestEntitlements_RequiresSessionAndMembership(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	entitlements.Reset() // start from a known baseline, independent of test order
	entitlements.RegisterProvider(entitlements.Static(entitlements.FeatureGovernance))
	t.Cleanup(entitlements.Reset)

	if rec := recordEntitlements(runmode.LocalDefaultOrgID, false); rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 for an unauthenticated request", rec.Code)
	}
	if rec := recordEntitlements("22222222-2222-2222-2222-222222222222", true); rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for an org outside local mode's one", rec.Code)
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
