package server

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx/httpxtest"
)

// The org's ceiling on API-token lifetime. These tests pin the setting; the
// store tests own its enforcement, which is applied when a token is used.

// orgAPITokenMaxAge reads the setting back off the org settings GET.
func orgAPITokenMaxAge(t *testing.T, s *Server) int {
	t.Helper()
	rec := doJSON(t, s, http.MethodGet, orgSettingsPath(), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s: %d: %s", orgSettingsPath(), rec.Code, rec.Body.String())
	}
	var out struct {
		APITokenMaxAgeDays int `json:"api_token_max_age_days"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode org settings: %v", err)
	}
	return out.APITokenMaxAgeDays
}

// TestOrgSettingsPatch_APITokenMaxAge_BoundsAccepted: a fresh org is uncapped,
// and both ends of the band are legal values, not off-by-one rejections.
func TestOrgSettingsPatch_APITokenMaxAge_BoundsAccepted(t *testing.T) {
	s := newTestServer(t)
	if got := orgAPITokenMaxAge(t, s); got != 0 {
		t.Fatalf("fresh org api_token_max_age_days = %v, want 0 (uncapped by default)", got)
	}
	// Max first, so the second save lowers the cap: lowering is an ordinary
	// write, not a guarded one.
	for _, want := range []int{domain.APITokenMaxAgeDaysMax, domain.APITokenMaxAgeDaysMin} {
		patchOrgSettingsOK(t, s, map[string]any{"api_token_max_age_days": want})
		if got := orgAPITokenMaxAge(t, s); got != want {
			t.Errorf("api_token_max_age_days = %v, want %v", got, want)
		}
	}
}

// TestOrgSettingsPatch_APITokenMaxAge_OutOfBandRejected: the handler refuses an
// out-of-band value with 422 naming the field, never clamping it. 0 is refused
// because "no maximum" is spelled null.
func TestOrgSettingsPatch_APITokenMaxAge_OutOfBandRejected(t *testing.T) {
	s := newTestServer(t)
	patchOrgSettingsOK(t, s, map[string]any{"api_token_max_age_days": 30})

	for _, bad := range []int{0, -1, domain.APITokenMaxAgeDaysMax + 1} {
		rec := patchOrgSettings(t, s, map[string]any{"api_token_max_age_days": bad})
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("api_token_max_age_days=%d = %d, want 422; body=%s", bad, rec.Code, rec.Body.String())
		}
		httpxtest.AssertFirstError(t, rec, httpx.ReasonOutOfRange, "api_token_max_age_days")
		if got := orgAPITokenMaxAge(t, s); got != 30 {
			t.Errorf("a refused save changed the stored cap to %v", got)
		}
	}
}

// TestOrgSettingsPatch_APITokenMaxAge_NonIntegerRejected: a fraction, a string
// or a bool is a shape fault, 400 INVALID_FIELD, not the 422 of a bad value.
func TestOrgSettingsPatch_APITokenMaxAge_NonIntegerRejected(t *testing.T) {
	s := newTestServer(t)
	for _, bad := range []any{1.5, "30", true} {
		rec := patchOrgSettings(t, s, map[string]any{"api_token_max_age_days": bad})
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("api_token_max_age_days=%#v = %d, want 400; body=%s", bad, rec.Code, rec.Body.String())
		}
		httpxtest.AssertFirstError(t, rec, httpx.ReasonInvalidField, "api_token_max_age_days")
	}
}

// TestOrgSettingsPatch_APITokenMaxAge_OmittedFieldUntouched: a save of another
// field keeps the cap. Clearing it would un-expire every token it shortens.
func TestOrgSettingsPatch_APITokenMaxAge_OmittedFieldUntouched(t *testing.T) {
	s := newTestServer(t)
	patchOrgSettingsOK(t, s, map[string]any{"api_token_max_age_days": 14})

	patchOrgSettingsOK(t, s, map[string]any{"enabled_models": []string{domain.ModelAliasSonnet}})
	if got := orgAPITokenMaxAge(t, s); got != 14 {
		t.Errorf("omitting api_token_max_age_days cleared the cap: got %v, want 14 preserved", got)
	}
}
