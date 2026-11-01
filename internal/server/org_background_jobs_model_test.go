package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/llm/modelcatalog"
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

// The stored value is dispatched verbatim, so the save accepts only this
// deployment's universe. A local install refuses the native wire id the multi
// picker offers, and the refusal names the local universe.
func TestOrgSettingsPatch_BackgroundJobsModel_RejectsTheOtherVocabulary(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	s := newTestServer(t)
	before := orgBackgroundJobsModel(t, s)

	rec := patchOrgSettings(t, s, map[string]any{"background_jobs_model": domain.ModelSonnet})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body: %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "background_jobs_model") {
		t.Errorf("error does not name the field: %s", rec.Body.String())
	}
	// The message names this deployment's universe, because picking from it is
	// the caller's next move.
	if !strings.Contains(rec.Body.String(), domain.ModelAliasSonnet) {
		t.Errorf("error does not name the local universe: %s", rec.Body.String())
	}
	if got := orgBackgroundJobsModel(t, s); got != before {
		t.Errorf("a refused save changed the stored value to %q", got)
	}
}

// Every model the universe offers is a legitimate choice here: the background
// jobs are toolless and short-context, so the gates that narrow a delegation's
// picker do not narrow this one.
func TestOrgSettingsPatch_BackgroundJobsModel_AcceptsAnyOfferedModel(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	s := newTestServer(t)
	for _, key := range modelcatalog.UniverseFor(false).Keys() {
		patchOrgSettingsOK(t, s, map[string]any{"background_jobs_model": key})
		if got := orgBackgroundJobsModel(t, s); got != key {
			t.Errorf("background_jobs_model = %q, want %q", got, key)
		}
	}
}

// null turns the background jobs off, so it stores empty rather than being
// refused or coerced back to a default.
func TestOrgSettingsPatch_BackgroundJobsModel_NullTurnsTheJobsOff(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	s := newTestServer(t)
	patchOrgSettingsOK(t, s, map[string]any{"background_jobs_model": domain.ModelAliasOpus})
	patchOrgSettingsOK(t, s, map[string]any{"background_jobs_model": nil})
	if got := orgBackgroundJobsModel(t, s); got != "" {
		t.Errorf("background_jobs_model = %q after a null write, want empty", got)
	}
}
