package server

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// The org's ceiling on API-token lifetime, on the surface that already carries
// the org's other policy caps. What the tests below pin is the setting, not the
// enforcement: effective expiry is min(stored expires_at, created_at + cap)
// computed at USE, and the store tests own the property that tightening the cap
// stops an over-age token authenticating.

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

// TestOrgSettingsPatch_APITokenMaxAge_RoundTrip: a value in the band reflects on
// the next GET, and null clears it back to "no maximum".
func TestOrgSettingsPatch_APITokenMaxAge_RoundTrip(t *testing.T) {
	s := newTestServer(t)

	if got := orgAPITokenMaxAge(t, s); got != 0 {
		t.Fatalf("fresh org api_token_max_age_days = %v, want 0 (uncapped by default)", got)
	}

	patchOrgSettingsOK(t, s, map[string]any{"api_token_max_age_days": 30})
	if got := orgAPITokenMaxAge(t, s); got != 30 {
		t.Errorf("after setting 30, GET api_token_max_age_days = %v, want 30", got)
	}

	// Lowering is an ordinary write, not a guarded one: the cap is applied at
	// use, so this immediately shortens every existing token in the org, and
	// that is what the control is for.
	patchOrgSettingsOK(t, s, map[string]any{"api_token_max_age_days": 7})
	if got := orgAPITokenMaxAge(t, s); got != 7 {
		t.Errorf("after lowering to 7, GET api_token_max_age_days = %v, want 7", got)
	}

	patchOrgSettingsOK(t, s, map[string]any{"api_token_max_age_days": nil})
	if got := orgAPITokenMaxAge(t, s); got != 0 {
		t.Errorf("after null, GET api_token_max_age_days = %v, want 0 (cleared)", got)
	}
}

// TestOrgSettingsPatch_APITokenMaxAge_BoundsAccepted: both ends of the band are
// legal values, not off-by-one rejections.
func TestOrgSettingsPatch_APITokenMaxAge_BoundsAccepted(t *testing.T) {
	s := newTestServer(t)
	for _, want := range []int{domain.APITokenMaxAgeDaysMin, domain.APITokenMaxAgeDaysMax} {
		patchOrgSettingsOK(t, s, map[string]any{"api_token_max_age_days": want})
		if got := orgAPITokenMaxAge(t, s); got != want {
			t.Errorf("api_token_max_age_days = %v, want %v", got, want)
		}
	}
}

// TestOrgSettingsPatch_APITokenMaxAge_OutOfBandRejected pins the range check the
// handler owns. The Postgres column carries the same CHECK, but the DB is the
// backstop and not the gate: a CHECK violation is a 500 naming no field, and the
// SQLite twin has no CHECK at all. Never clamped — a caller who asked for 400
// days did not ask for 365.
//
// 0 is refused rather than read as "no maximum": that has exactly one spelling
// here, and it is null. A zero-day cap would also expire every token the moment
// it was minted.
func TestOrgSettingsPatch_APITokenMaxAge_OutOfBandRejected(t *testing.T) {
	s := newTestServer(t)
	patchOrgSettingsOK(t, s, map[string]any{"api_token_max_age_days": 30})

	for _, bad := range []int{0, -1, domain.APITokenMaxAgeDaysMax + 1} {
		rec := patchOrgSettings(t, s, map[string]any{"api_token_max_age_days": bad})
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("api_token_max_age_days=%d = %d, want 422; body=%s", bad, rec.Code, rec.Body.String())
		}
		assertFirstError(t, rec, httpx.ReasonOutOfRange, "api_token_max_age_days")
		if got := orgAPITokenMaxAge(t, s); got != 30 {
			t.Errorf("a refused save changed the stored cap to %v", got)
		}
	}
}

// TestOrgSettingsPatch_APITokenMaxAge_NonIntegerRejected: the column is an
// integer, so a fraction or a string is a shape fault — 400 INVALID_FIELD, the
// split this route draws between a body it cannot read and a value it will not
// store.
func TestOrgSettingsPatch_APITokenMaxAge_NonIntegerRejected(t *testing.T) {
	s := newTestServer(t)
	for _, bad := range []any{1.5, "30", true} {
		rec := patchOrgSettings(t, s, map[string]any{"api_token_max_age_days": bad})
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("api_token_max_age_days=%#v = %d, want 400; body=%s", bad, rec.Code, rec.Body.String())
		}
		assertFirstError(t, rec, httpx.ReasonInvalidField, "api_token_max_age_days")
	}
}

// TestOrgSettingsPatch_APITokenMaxAge_OmittedFieldUntouched pins the absent-
// means-keep half: a save touching another section must not clear the org's
// token policy — which, since the cap is applied at use, would silently
// un-expire every token the policy was shortening.
func TestOrgSettingsPatch_APITokenMaxAge_OmittedFieldUntouched(t *testing.T) {
	s := newTestServer(t)
	patchOrgSettingsOK(t, s, map[string]any{"api_token_max_age_days": 14})

	patchOrgSettingsOK(t, s, map[string]any{"enabled_models": []string{domain.ModelAliasSonnet}})
	if got := orgAPITokenMaxAge(t, s); got != 14 {
		t.Errorf("omitting api_token_max_age_days cleared the cap: got %v, want 14 preserved", got)
	}
}
