package server

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/zalando/go-keyring"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// orgLLMAuthMethod reads the setting back off the org settings GET — the shape
// the setup picker and the Settings source toggle both hydrate from.
func orgLLMAuthMethod(t *testing.T, s *Server) string {
	t.Helper()
	rec := doJSON(t, s, http.MethodGet, orgSettingsPath(), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s: %d: %s", orgSettingsPath(), rec.Code, rec.Body.String())
	}
	var out struct {
		LLMAuthMethod string `json:"llm_auth_method"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode org settings: %v", err)
	}
	return out.LLMAuthMethod
}

// bindAnthropicRefWithMethod stores the Anthropic ref the way the bind route
// does — the ref AND the credential source — without the live key validation
// that route performs.
func bindAnthropicRefWithMethod(t *testing.T, s *Server) {
	t.Helper()
	if _, err := s.allStores.Orgs.SetAnthropicKeyRef(t.Context(), runmode.LocalDefaultOrgID, secretKeyAnthropicAPIKey); err != nil {
		t.Fatalf("bind anthropic: %v", err)
	}
}

// The selection round-trips as a stored fact rather than being derived from
// what is bound: an org that brings its own key but has not bound one yet is a
// different state from one running on the host's.
func TestOrgSettingsPatch_LLMAuthMethod_RoundTrips(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	s := newTestServer(t)

	patchOrgSettingsOK(t, s, map[string]any{"llm_auth_method": domain.LLMAuthBYOK})
	if got := orgLLMAuthMethod(t, s); got != domain.LLMAuthBYOK {
		t.Fatalf("llm_auth_method = %q after selecting byok, want %q", got, domain.LLMAuthBYOK)
	}
	patchOrgSettingsOK(t, s, map[string]any{"llm_auth_method": domain.LLMAuthSystem})
	if got := orgLLMAuthMethod(t, s); got != domain.LLMAuthSystem {
		t.Errorf("llm_auth_method = %q after selecting system, want %q", got, domain.LLMAuthSystem)
	}
}

// The accepted set is the two credential sources. A third spelling is a value
// nothing resolves, so the save says so rather than storing it and letting the
// read guess.
func TestOrgSettingsPatch_LLMAuthMethod_RejectsAnythingElse(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	s := newTestServer(t)
	before := orgLLMAuthMethod(t, s)

	for _, bad := range []any{"ambient", "BYOK", "", nil} {
		rec := patchOrgSettings(t, s, map[string]any{"llm_auth_method": bad})
		if rec.Code != http.StatusBadRequest {
			t.Errorf("PATCH llm_auth_method=%v = %d, want 400 (body: %s)", bad, rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "llm_auth_method") {
			t.Errorf("PATCH llm_auth_method=%v: error does not name the field: %s", bad, rec.Body.String())
		}
	}
	if got := orgLLMAuthMethod(t, s); got != before {
		t.Errorf("llm_auth_method = %q after the refusals, want %q unchanged", got, before)
	}
}

// A local org starts on the host's credentials and binding a key moves it onto
// its own. Moving back while a key is bound is refused with 422 naming the key
// to disconnect, and succeeds once nothing is bound.
func TestOrgSettingsPatch_LLMAuthMethod_RefusesSystemWhileCredentialsBound(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	s := newTestServer(t)
	if got := orgLLMAuthMethod(t, s); got != domain.LLMAuthSystem {
		t.Fatalf("llm_auth_method = %q before the bind, want the local default %q", got, domain.LLMAuthSystem)
	}
	bindAnthropicRefWithMethod(t, s)

	rec := patchOrgSettings(t, s, map[string]any{"llm_auth_method": domain.LLMAuthSystem})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("PATCH = %d, want 422 (body: %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "llm_auth_method") {
		t.Errorf("error does not name the field: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Anthropic") {
		t.Errorf("error does not name the credential to disconnect: %s", rec.Body.String())
	}
	if got := orgLLMAuthMethod(t, s); got != domain.LLMAuthBYOK {
		t.Errorf("llm_auth_method = %q after the refusal, want %q unchanged", got, domain.LLMAuthBYOK)
	}
	// And it is the bound material that refuses, not the value: removing it
	// makes the same write succeed.
	if _, err := s.allStores.Orgs.SetAnthropicKeyRef(t.Context(), runmode.LocalDefaultOrgID, ""); err != nil {
		t.Fatalf("unbind anthropic: %v", err)
	}
	patchOrgSettingsOK(t, s, map[string]any{"llm_auth_method": domain.LLMAuthSystem})
	if got := orgLLMAuthMethod(t, s); got != domain.LLMAuthSystem {
		t.Errorf("llm_auth_method = %q once nothing is bound, want %q", got, domain.LLMAuthSystem)
	}
}

// A save that re-sends the method it already has is not refused for a state it
// did not create: the check fires on a change, not on the value.
func TestOrgSettingsPatch_LLMAuthMethod_UnchangedValueIsNotRechecked(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	s := newTestServer(t)

	// An org on host credentials that acquired a ref some other way: the
	// combination the PATCH refuses to CREATE, re-sent rather than requested.
	// The ref write sets the org's own credentials, so the method is put back
	// to the host's by a bulk save, which can write the method but not the ref.
	set, err := s.allStores.Orgs.SetAnthropicKeyRef(t.Context(), runmode.LocalDefaultOrgID, secretKeyAnthropicAPIKey)
	if err != nil {
		t.Fatalf("seed ref: %v", err)
	}
	set.LLMAuthMethod = domain.LLMAuthSystem
	if _, err := s.allStores.Orgs.UpdateSettingsVersioned(t.Context(), runmode.LocalDefaultOrgID, set, set.Version); err != nil {
		t.Fatalf("seed settings: %v", err)
	}

	patchOrgSettingsOK(t, s, map[string]any{"llm_auth_method": domain.LLMAuthSystem})
}

// Multi mode has no host credentials to lend, so a freshly provisioned org
// stores and reads "byok", and a PATCH to "system" is refused with 400 naming
// the field.
func TestOrgSettings_MultiRefusesHostCredentials(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	r := newAuthRig(t)

	founder := r.seedUser()
	orgA, _ := r.seedOrg(founder, "acme")
	sid := r.signInAs(founder)

	// Verbatim the statement provisioning runs: naming only the foreign key is
	// what lets every other column come from its DEFAULT, so this is the row
	// shape a real multi org is born with.
	if _, err := r.h.AdminDB.Exec(
		`INSERT INTO org_settings (org_id) VALUES ($1) ON CONFLICT (org_id) DO NOTHING`, orgA,
	); err != nil {
		t.Fatalf("seed org settings: %v", err)
	}

	storedMethod := func() string {
		t.Helper()
		var method string
		if err := r.h.AdminDB.QueryRow(
			`SELECT llm_auth_method FROM org_settings WHERE org_id = $1`, orgA,
		).Scan(&method); err != nil {
			t.Fatalf("read stored auth method: %v", err)
		}
		return method
	}
	if got := storedMethod(); got != domain.LLMAuthBYOK {
		t.Fatalf("a freshly provisioned org stores %q; want %q", got, domain.LLMAuthBYOK)
	}

	path := "/api/orgs/" + orgA.String() + "/settings"
	resp := r.requestWithSid(http.MethodGet, path, sid)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: %d: %s", path, resp.StatusCode, body)
	}
	var cur struct {
		Version       int    `json:"version"`
		LLMAuthMethod string `json:"llm_auth_method"`
	}
	if err := json.Unmarshal(body, &cur); err != nil {
		t.Fatalf("decode settings: %v", err)
	}
	if cur.LLMAuthMethod != domain.LLMAuthBYOK {
		t.Errorf("GET llm_auth_method = %q, want %q", cur.LLMAuthMethod, domain.LLMAuthBYOK)
	}

	out := r.postJSONWithSid(http.MethodPatch, path, sid, map[string]any{
		"version":         cur.Version,
		"llm_auth_method": domain.LLMAuthSystem,
	})
	refused, _ := io.ReadAll(out.Body)
	out.Body.Close()
	if out.StatusCode != http.StatusBadRequest {
		t.Errorf("PATCH llm_auth_method=system = %d, want 400 (body: %s)", out.StatusCode, refused)
	}
	if !strings.Contains(string(refused), "llm_auth_method") {
		t.Errorf("error does not name the field: %s", refused)
	}
	if got := storedMethod(); got != domain.LLMAuthBYOK {
		t.Errorf("after the refusal the column holds %q; want %q", got, domain.LLMAuthBYOK)
	}
}
