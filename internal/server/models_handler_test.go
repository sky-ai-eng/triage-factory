package server

import (
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"github.com/zalando/go-keyring"

	"github.com/sky-ai-eng/triage-factory/internal/modelcatalog"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// modelsPath addresses one org's catalog.
func modelsPath(orgID string) string { return "/api/orgs/" + orgID + "/models" }

// decodeModels reads the response body as the catalog contract.
func decodeModels(t *testing.T, body []byte) modelCatalogResponse {
	t.Helper()
	var resp modelCatalogResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode catalog: %v (body: %s)", err, body)
	}
	return resp
}

// The read an org member gets: every catalog entry, enabled, with prices and
// capabilities joined in from the datasheet.
func TestModelsList_ServesTheJoinedCatalog(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	s := newTestServer(t)

	rec := doJSON(t, s, http.MethodGet, modelsPath(runmode.LocalDefaultOrgID), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	items := decodeModels(t, rec.Body.Bytes()).Items

	catalog := modelcatalog.Entries()
	if len(items) != len(catalog) {
		t.Fatalf("returned %d models, want %d (one per catalog entry)", len(items), len(catalog))
	}
	for i, want := range catalog {
		got := items[i]
		if got.Key != want.Key {
			t.Errorf("item %d: key = %q, want %q (catalog order is display order)", i, got.Key, want.Key)
		}
		if got.DisplayName != want.DisplayName {
			t.Errorf("%s: display_name = %q, want %q", got.Key, got.DisplayName, want.DisplayName)
		}
		if got.Provider != want.Provider {
			t.Errorf("%s: provider = %q, want %q", got.Key, got.Provider, want.Provider)
		}
		if got.PricesPerMTok.Output != want.Prices.Output {
			t.Errorf("%s: output price = %v, want %v", got.Key, got.PricesPerMTok.Output, want.Prices.Output)
		}
		if got.ContextWindow != want.ContextWindow {
			t.Errorf("%s: context_window = %d, want %d", got.Key, got.ContextWindow, want.ContextWindow)
		}
		if got.SupportsPromptCaching != want.SupportsPromptCaching {
			t.Errorf("%s: supports_prompt_caching = %v, want %v", got.Key, got.SupportsPromptCaching, want.SupportsPromptCaching)
		}
		if got.DisplayOrder != i {
			t.Errorf("%s: display_order = %d, want %d", got.Key, got.DisplayOrder, i)
		}
		// No org has expressed an enable-set yet, so every entry is enabled;
		// and nothing probes yet, so nothing is more than assumed available.
		if !got.Enabled {
			t.Errorf("%s: enabled = false, want true with no stored enable-set", got.Key)
		}
		if got.Availability != modelAvailabilityAssumed {
			t.Errorf("%s: availability = %q, want %q", got.Key, got.Availability, modelAvailabilityAssumed)
		}
	}
}

// A path id that cannot name an org names nothing the caller may learn about.
func TestModelsList_MalformedOrgIs404(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	s := newTestServer(t)

	rec := doJSON(t, s, http.MethodGet, modelsPath("not-a-uuid"), nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

// The org in the path is the subject, and membership of THAT org is the gate: a
// member reads it, and a non-member is told the org does not exist rather than
// that they may not see it.
func TestModelsList_OrgScoping(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	r := newAuthRig(t)

	alice := r.seedUser()
	orgA, _ := r.seedOrg(alice, "alice-models-org")
	bob := r.seedUser()
	orgB, _ := r.seedOrg(bob, "bob-models-org")

	resp, _ := r.driveCallback(alice)
	sid := r.sidFromResp(resp)

	own := r.requestWithSid(http.MethodGet, modelsPath(orgA.String()), sid)
	if own.StatusCode != http.StatusOK {
		t.Fatalf("member GET own org = %d, want 200", own.StatusCode)
	}
	body, err := io.ReadAll(own.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	own.Body.Close()
	if got, want := len(decodeModels(t, body).Items), len(modelcatalog.Entries()); got != want {
		t.Errorf("member read returned %d models, want %d", got, want)
	}

	other := r.requestWithSid(http.MethodGet, modelsPath(orgB.String()), sid)
	other.Body.Close()
	if other.StatusCode != http.StatusNotFound {
		t.Errorf("non-member GET = %d, want 404 (not 403 — don't leak existence)", other.StatusCode)
	}
}

// One contract, two implementations. The catalog ships in the binary, so both
// modes answer with the same rows in the same order carrying the same facts —
// and the single field they differ on is `availability`, which exists to carry
// exactly this difference as DATA. A client reads the field; it never asks
// which mode it is talking to.
//
// Local says "assumed" because it has nothing to probe with: its runs go
// through the SDK subprocess, possibly on a Claude Code subscription with no
// API key. Multi says "unverified" until a probe concludes something.
func TestModelsList_AcrossModes_DiffersOnlyOnAvailability(t *testing.T) {
	multi := func() []modelCatalogRow {
		runmode.SetForTest(t, runmode.ModeMulti)
		r := newAuthRig(t)
		alice := r.seedUser()
		orgA, _ := r.seedOrg(alice, "alice-parity-org")
		resp, _ := r.driveCallback(alice)
		got := r.requestWithSid(http.MethodGet, modelsPath(orgA.String()), r.sidFromResp(resp))
		defer got.Body.Close()
		if got.StatusCode != http.StatusOK {
			t.Fatalf("multi-mode read = %d, want 200", got.StatusCode)
		}
		body, err := io.ReadAll(got.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		return decodeModels(t, body).Items
	}()

	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	rec := doJSON(t, newTestServer(t), http.MethodGet, modelsPath(runmode.LocalDefaultOrgID), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("local-mode read = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	local := decodeModels(t, rec.Body.Bytes()).Items

	if len(local) != len(multi) {
		t.Fatalf("local returned %d models, multi %d — the catalog is the same file in both", len(local), len(multi))
	}
	for i := range local {
		if local[i].Availability != modelAvailabilityAssumed {
			t.Errorf("%s: local availability = %q, want %q", local[i].Key, local[i].Availability, modelAvailabilityAssumed)
		}
		if multi[i].Availability != modelAvailabilityUnverified {
			t.Errorf("%s: multi availability = %q, want %q with nothing probed", multi[i].Key, multi[i].Availability, modelAvailabilityUnverified)
		}
		// Neither mode reports a detail or a check time for a state no probe
		// produced.
		if local[i].AvailabilityDetail != "" || local[i].AvailabilityCheckedAt != nil ||
			multi[i].AvailabilityDetail != "" || multi[i].AvailabilityCheckedAt != nil {
			t.Errorf("%s: an unprobed row carries a detail or a timestamp", local[i].Key)
		}
		// Everything else is the same fact about the same build.
		a, b := local[i], multi[i]
		a.Availability, b.Availability = "", ""
		if a != b {
			t.Errorf("modes disagree on more than availability:\n local: %+v\n multi: %+v", a, b)
		}
	}
}
