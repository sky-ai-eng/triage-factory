package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/modelcatalog"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/zalando/go-keyring"
)

// orgBackgroundJobsModel reads the setting back off the org settings GET.
func orgBackgroundJobsModel(t *testing.T, s *Server) string {
	t.Helper()
	rec := doJSON(t, s, http.MethodGet, orgSettingsPath(), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s: %d: %s", orgSettingsPath(), rec.Code, rec.Body.String())
	}
	var out struct {
		BackgroundJobsModel string `json:"background_jobs_model"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode org settings: %v", err)
	}
	return out.BackgroundJobsModel
}

// A local install arrives with the setting already filled in — the SQLite
// column default — because local mode has no setup step that would make someone
// choose, and the background jobs skip without one.
func TestOrgSettingsGet_LocalCarriesTheBackgroundJobsPreFill(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	s := newTestServer(t)
	if got := orgBackgroundJobsModel(t, s); got != domain.LocalBackgroundJobsModel {
		t.Errorf("background_jobs_model = %q, want the local pre-fill %q", got, domain.LocalBackgroundJobsModel)
	}
}

// The stored value is dispatched verbatim, so the accepted set is the catalog
// the picker draws from — anything else is a value nothing can invoke, and the
// save must say so rather than store it and let three background jobs fail
// quietly later.
func TestOrgSettingsPatch_BackgroundJobsModel_RejectsUnofferedModel(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	s := newTestServer(t)
	before := orgBackgroundJobsModel(t, s)

	rec := patchOrgSettings(t, s, map[string]any{"background_jobs_model": "haiku"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body: %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "background_jobs_model") {
		t.Errorf("error does not name the field: %s", rec.Body.String())
	}
	if got := orgBackgroundJobsModel(t, s); got != before {
		t.Errorf("a refused save changed the stored value to %q", got)
	}
}

// Every catalog entry is a legitimate choice here: the background jobs are
// toolless and short-context, so the gates that narrow a delegation's picker do
// not narrow this one.
func TestOrgSettingsPatch_BackgroundJobsModel_AcceptsAnyCatalogModel(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	s := newTestServer(t)
	for _, key := range modelcatalog.Keys() {
		patchOrgSettingsOK(t, s, map[string]any{"background_jobs_model": key})
		if got := orgBackgroundJobsModel(t, s); got != key {
			t.Errorf("background_jobs_model = %q, want %q", got, key)
		}
	}
}

// Clearing is a real intent — it turns the background jobs off, which is the
// only way to stop them short of unbinding the credential every other feature
// shares — so null stores empty rather than being refused or coerced back to a
// default.
func TestOrgSettingsPatch_BackgroundJobsModel_NullTurnsTheJobsOff(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	s := newTestServer(t)
	patchOrgSettingsOK(t, s, map[string]any{"background_jobs_model": domain.ModelOpus})
	patchOrgSettingsOK(t, s, map[string]any{"background_jobs_model": nil})
	if got := orgBackgroundJobsModel(t, s); got != "" {
		t.Errorf("background_jobs_model = %q after a null write, want empty", got)
	}
}

// A model whose provider the org has not connected is one its next cycle would
// refuse, so it is not worth storing. The refusal names the provider, because
// connecting it is the whole remedy — and nothing quietly picks the provider the
// org does hold.
func TestOrgSettingsPatch_BackgroundJobsModel_RefusesUnconnectedProvider(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	s := newTestServer(t)
	connectOrgProviders(t, s, true, false)
	before := orgBackgroundJobsModel(t, s)

	rec := patchOrgSettings(t, s, map[string]any{
		"background_jobs_model": modelKeyOn(t, modelcatalog.ProviderBedrock),
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body: %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), modelcatalog.ProviderDisplayName(modelcatalog.ProviderBedrock)) {
		t.Errorf("refusal does not name the provider to connect: %s", rec.Body.String())
	}
	if got := orgBackgroundJobsModel(t, s); got != before {
		t.Errorf("a refused save changed the stored value to %q", got)
	}
}
