package server

import (
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"github.com/zalando/go-keyring"

	"github.com/sky-ai-eng/triage-factory/internal/llm/modelcatalog"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// bindAnthropicRef records that the org holds an Anthropic credential. The
// settings REF is what the availability derivation reads — it is the record of
// what an admin bound, and the vault secret behind it is only needed by
// something that actually makes a request.
func bindAnthropicRef(t *testing.T, r *authRig, orgID string) {
	t.Helper()
	if _, err := r.h.AdminDB.Exec(`
		INSERT INTO org_settings (org_id, anthropic_api_key_ref) VALUES ($1, 'anthropic_api_key')
		ON CONFLICT (org_id) DO UPDATE SET anthropic_api_key_ref = 'anthropic_api_key'`, orgID); err != nil {
		t.Fatalf("bind anthropic ref: %v", err)
	}
}

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

// The read a member of a MULTI org gets: every native registry entry, enabled,
// with prices and capabilities joined in from the datasheet.
func TestModelsList_ServesTheJoinedCatalog(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	r := newAuthRig(t)
	alice := r.seedUser()
	org, _ := r.seedOrg(alice, "alice-catalog-org")
	bindAnthropicRef(t, r, org.String())
	resp, _ := r.driveCallback(alice)

	got := r.requestWithSid(http.MethodGet, modelsPath(org.String()), r.sidFromResp(resp))
	defer got.Body.Close()
	if got.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", got.StatusCode)
	}
	body, err := io.ReadAll(got.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	items := decodeModels(t, body).Items

	catalog := modelcatalog.Entries()
	if len(items) != len(catalog) {
		t.Fatalf("returned %d models, want %d (one per registry entry)", len(items), len(catalog))
	}
	for i, want := range catalog {
		got := items[i]
		if got.Key != want.Key {
			t.Errorf("item %d: key = %q, want %q (file order is display order)", i, got.Key, want.Key)
		}
		if got.DisplayName != want.DisplayName {
			t.Errorf("%s: display_name = %q, want %q", got.Key, got.DisplayName, want.DisplayName)
		}
		if got.Provider != want.Provider {
			t.Errorf("%s: provider = %q, want %q", got.Key, got.Provider, want.Provider)
		}
		if got.PricesPerMTok == nil {
			t.Fatalf("%s: no prices on a native row", got.Key)
		}
		if got.PricesPerMTok.Output != want.Prices.Output {
			t.Errorf("%s: output price = %v, want %v", got.Key, got.PricesPerMTok.Output, want.Prices.Output)
		}
		if got.ContextWindow != want.ContextWindow {
			t.Errorf("%s: context_window = %d, want %d", got.Key, got.ContextWindow, want.ContextWindow)
		}
		if got.SupportsPromptCaching == nil {
			t.Errorf("%s: no supports_prompt_caching on a native row", got.Key)
		} else if *got.SupportsPromptCaching != want.SupportsPromptCaching {
			t.Errorf("%s: supports_prompt_caching = %v, want %v", got.Key, *got.SupportsPromptCaching, want.SupportsPromptCaching)
		}
		if got.ProviderDisplayName != modelcatalog.ProviderDisplayName(want.Provider) {
			t.Errorf("%s: provider_display_name = %q, want %q",
				got.Key, got.ProviderDisplayName, modelcatalog.ProviderDisplayName(want.Provider))
		}
		if got.DisplayOrder != i {
			t.Errorf("%s: display_order = %d, want %d", got.Key, got.DisplayOrder, i)
		}
		// No org has expressed an enable-set yet, so every entry is enabled.
		if !got.Enabled {
			t.Errorf("%s: enabled = false, want true with no stored enable-set", got.Key)
		}
	}
}

// The read a LOCAL install gets: the Claude Code SDK's alias list, and only it.
// Every datasheet field and the availability triple are omitted from the wire,
// not zero-valued: a rendered $0.00 is a price claim and `"provider": ""` an
// access-path claim, and an alias names no provider and joins no price table.
func TestModelsList_LocalServesTheSDKAliasList(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	s := newTestServer(t)

	rec := doJSON(t, s, http.MethodGet, modelsPath(runmode.LocalDefaultOrgID), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	items := decodeModels(t, rec.Body.Bytes()).Items

	want := modelcatalog.SDKModels(modelcatalog.SDKClaudeCode)
	if len(items) != len(want) {
		t.Fatalf("returned %d models, want %d (one per SDK alias): %+v", len(items), len(want), items)
	}
	for i, w := range want {
		got := items[i]
		if got.Key != w.Key {
			t.Errorf("item %d: key = %q, want %q", i, got.Key, w.Key)
		}
		if got.DisplayName != w.DisplayName {
			t.Errorf("%s: display_name = %q, want %q", got.Key, got.DisplayName, w.DisplayName)
		}
		if got.DisplayOrder != i {
			t.Errorf("%s: display_order = %d, want %d", got.Key, got.DisplayOrder, i)
		}
		if !got.Enabled {
			t.Errorf("%s: enabled = false, want true with no stored enable-set", got.Key)
		}
		if got.Provider != "" {
			t.Errorf("%s: provider = %q, want absent — an alias names no access path", got.Key, got.Provider)
		}
		if got.PricesPerMTok != nil {
			t.Errorf("%s: prices_per_mtok = %+v, want absent — cost is harness-settled", got.Key, *got.PricesPerMTok)
		}
		if got.ContextWindow != 0 || got.SupportsPromptCaching != nil {
			t.Errorf("%s: carries datasheet facts (%d, %v), want absent", got.Key, got.ContextWindow, got.SupportsPromptCaching)
		}
		if got.ProviderDisplayName != "" {
			t.Errorf("%s: provider_display_name = %q, want absent — an alias names no access path", got.Key, got.ProviderDisplayName)
		}
		if got.Availability != "" || got.AvailabilityDetail != "" || got.AvailabilityCheckedAt != nil {
			t.Errorf("%s: carries an availability triple under system credentials, want absent", got.Key)
		}
	}

	var raw struct {
		Items []map[string]json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode raw: %v", err)
	}
	for _, item := range raw.Items {
		for _, absent := range []string{
			"provider", "prices_per_mtok", "context_window", "supports_prompt_caching",
			"availability", "availability_detail", "availability_checked_at",
		} {
			if _, present := item[absent]; present {
				t.Errorf("%s: field %q is on the wire, want it omitted", item["key"], absent)
			}
		}
		for _, required := range []string{"key", "display_name", "enabled", "display_order"} {
			if _, present := item[required]; !present {
				t.Errorf("%v: field %q is missing", item["key"], required)
			}
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
