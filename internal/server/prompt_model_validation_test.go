package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/modelcatalog"
)

// A stored model value is dispatched verbatim and priced by that same string,
// so the accepted set is the catalog and nothing else. The three tier words the
// product used to speak are the interesting rejection: they were valid until
// the vocabulary migration, they are what an old script or a stale client still
// sends, and accepting one would store a value the provider rejects and the
// ledger cannot cost.
func TestPromptCreate_ModelMustBeACatalogKey(t *testing.T) {
	s := newTestServer(t)

	for _, alias := range []string{"haiku", "sonnet", "opus"} {
		rec := doJSON(t, s, http.MethodPost, "/api/prompts", map[string]any{
			"name": "alias-" + alias, "body": "b", "model": alias,
		})
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("POST /api/prompts model=%q: %d, want 400 (body: %s)", alias, rec.Code, rec.Body.String())
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
			t.Fatalf("model=%q reported %d errors, want 1: %s", alias, len(resp.Errors), rec.Body.String())
		}
		item := resp.Errors[0]
		if item.Field != "model" || item.Reason != "INVALID_FIELD" {
			t.Errorf("model=%q: (reason, field) = (%q, %q), want (INVALID_FIELD, model)", alias, item.Reason, item.Field)
		}
		// The message names what a caller may pick instead — that is the whole
		// point of listing the keys rather than stating the rule abstractly.
		if !strings.Contains(item.Message, domain.ModelSonnet) {
			t.Errorf("model=%q message does not name the catalog keys: %q", alias, item.Message)
		}
	}

	// Every key the catalog offers is accepted, plus "" (inherit).
	for _, e := range modelcatalog.Entries() {
		rec := doJSON(t, s, http.MethodPost, "/api/prompts", map[string]any{
			"name": "pinned-" + e.Key, "body": "b", "model": e.Key,
		})
		if rec.Code != http.StatusCreated {
			t.Errorf("POST /api/prompts model=%q: %d, want 201 (body: %s)", e.Key, rec.Code, rec.Body.String())
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
// to the same catalog the per-step pin does. The picker offers exactly this
// set; a headless caller is the only one that can reach the rejection.
func TestTeamSettingsPatch_ModelMustBeACatalogKey(t *testing.T) {
	s := newTestServer(t)

	rec := doJSON(t, s, http.MethodPatch, teamSettingsPath("default"), map[string]any{"ai_model": "sonnet"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("PATCH team settings ai_model=sonnet: %d, want 400 (body: %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), domain.ModelSonnet) {
		t.Errorf("rejection does not name the catalog keys: %s", rec.Body.String())
	}

	resp := patchTeamSettingsOK(t, s, "default", map[string]any{"ai_model": domain.ModelHaiku})
	settings, _ := resp["team_settings"].(map[string]any)
	if settings == nil || settings["DefaultModel"] != domain.ModelHaiku {
		t.Errorf("saved team default = %#v, want %q", resp["team_settings"], domain.ModelHaiku)
	}
}
