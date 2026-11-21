package server

import (
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/entitlements"
)

// TestEntitlements_MultiReadsTheNamedOrg: in multi mode the probe answers for
// the org the path names to any of its members, reporting the provider's state
// as local mode does, and an org the caller is not in is 404.
func TestEntitlements_MultiReadsTheNamedOrg(t *testing.T) {
	r := newAuthRig(t)
	owner := r.seedUser()
	orgID, _ := r.seedOrg(owner, "entitlements-org")
	outsider := r.seedUser()
	r.seedOrg(outsider, "entitlements-other")
	path := "/api/orgs/" + orgID.String() + "/entitlements"

	features := func(sid string) []string {
		t.Helper()
		resp := r.requestWithSid(http.MethodGet, path, sid)
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body: %s)", resp.StatusCode, body)
		}
		var out struct {
			Features []string `json:"features"`
		}
		if err := json.Unmarshal(body, &out); err != nil {
			t.Fatalf("decode: %v (body: %s)", err, body)
		}
		return out.Features
	}

	entitlements.Reset()
	t.Cleanup(entitlements.Reset)
	ownerSid := r.signInAs(owner)
	if got := features(ownerSid); len(got) != 0 {
		t.Errorf("features = %v, want [] for an unlicensed build", got)
	}

	entitlements.RegisterProvider(entitlements.Static(entitlements.FeatureGovernance))
	if got := features(ownerSid); len(got) != 1 || got[0] != string(entitlements.FeatureGovernance) {
		t.Errorf("features = %v, want [%q]", got, entitlements.FeatureGovernance)
	}

	if resp := r.requestWithSid(http.MethodGet, path, r.signInAs(outsider)); resp.StatusCode != http.StatusNotFound {
		t.Errorf("outsider status = %d, want 404", resp.StatusCode)
	}
}
