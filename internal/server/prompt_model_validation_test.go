package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/modelcatalog"
)

// A stored model value is dispatched verbatim by the runtime that will send it,
// so the accepted set is this deployment's universe and nothing else. These
// tests run local, where that universe is the Claude Code SDK's alias list — so
// the interesting rejection is the NATIVE wire id: it is what the multi picker
// offers, what an older local build stored, and what a stale client still
// sends. Handing one to the subprocess pins a version nobody asked for.
func TestPromptCreate_ModelMustBeOfferedByTheUniverse(t *testing.T) {
	s := newTestServer(t)

	for _, wrongVocabulary := range []string{domain.ModelHaiku, domain.ModelSonnet, domain.ModelOpus} {
		rec := doJSON(t, s, http.MethodPost, "/api/prompts", map[string]any{
			"name": "native-" + wrongVocabulary, "body": "b", "model": wrongVocabulary,
		})
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("POST /api/prompts model=%q: %d, want 400 (body: %s)", wrongVocabulary, rec.Code, rec.Body.String())
		}
		var resp struct {
			Errors []struct {
				Reason  string `json:"reason"`
				Message string `json:"message"`
				Field   string `json:"field"`
			} `json:"errors"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode error body: %v", err)
		}
		if len(resp.Errors) != 1 {
			t.Fatalf("model=%q reported %d errors, want 1: %s", wrongVocabulary, len(resp.Errors), rec.Body.String())
		}
		item := resp.Errors[0]
		if item.Field != "model" || item.Reason != "INVALID_FIELD" {
			t.Errorf("model=%q: (reason, field) = (%q, %q), want (INVALID_FIELD, model)", wrongVocabulary, item.Reason, item.Field)
		}
		// The message names what a caller may pick instead — that is the whole
		// point of listing the universe rather than stating the rule abstractly.
		if !strings.Contains(item.Message, domain.ModelAliasSonnet) {
			t.Errorf("model=%q message does not name the local universe: %q", wrongVocabulary, item.Message)
		}
	}

	// Every model the universe offers is accepted, plus "" (inherit).
	for _, key := range modelcatalog.UniverseFor(false).Keys() {
		rec := doJSON(t, s, http.MethodPost, "/api/prompts", map[string]any{
			"name": "pinned-" + key, "body": "b", "model": key,
		})
		if rec.Code != http.StatusCreated {
			t.Errorf("POST /api/prompts model=%q: %d, want 201 (body: %s)", key, rec.Code, rec.Body.String())
		}
	}
	rec := doJSON(t, s, http.MethodPost, "/api/prompts", map[string]any{
		"name": "inherits", "body": "b", "model": "",
	})
	if rec.Code != http.StatusCreated {
		t.Errorf("POST /api/prompts with an unset model: %d, want 201 (body: %s)", rec.Code, rec.Body.String())
	}
}

// The team default is the value every unset step falls back to, so it answers
// to the same universe the per-step pin does. The picker offers exactly this
// set; a headless caller is the only one that can reach the rejection.
func TestTeamSettingsPatch_ModelMustBeOfferedByTheUniverse(t *testing.T) {
	s := newTestServer(t)

	rec := doJSON(t, s, http.MethodPatch, teamSettingsPath("default"), map[string]any{"ai_model": domain.ModelSonnet})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("PATCH team settings ai_model=%s: %d, want 400 (body: %s)", domain.ModelSonnet, rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), domain.ModelAliasSonnet) {
		t.Errorf("rejection does not name the local universe: %s", rec.Body.String())
	}

	resp := patchTeamSettingsOK(t, s, "default", map[string]any{"ai_model": domain.ModelAliasHaiku})
	settings, _ := resp["team_settings"].(map[string]any)
	if settings == nil || settings["DefaultModel"] != domain.ModelAliasHaiku {
		t.Errorf("saved team default = %#v, want %q", resp["team_settings"], domain.ModelAliasHaiku)
	}
}
