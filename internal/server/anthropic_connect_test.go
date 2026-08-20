package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/auth"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
	"github.com/zalando/go-keyring"
)

// anthropicModelsStub stands up a host whose every response carries the given
// status, standing in for Anthropic's GET /v1/models key check. The connect
// handler's validator (auth.ValidateAnthropicAPIKey) is pointed at it via
// auth.SetAnthropicModelsURLForTest.
func anthropicModelsStub(t *testing.T, status int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// llmPath addresses one of the org's LLM credential resources.
func llmPath(suffix string) string {
	return "/api/orgs/" + runmode.LocalDefaultOrgID + "/llm/" + suffix
}

// orgHasAnthropicKey reads the org-settings presence flag — the same
// has_anthropic_api_key signal the "Configured" UI reads.
func orgHasAnthropicKey(t *testing.T, s *Server) bool {
	t.Helper()
	rec := doJSON(t, s, "GET", orgSettingsPath(), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s status=%d body=%s", orgSettingsPath(), rec.Code, rec.Body.String())
	}
	var out struct {
		HasAnthropicAPIKey bool `json:"has_anthropic_api_key"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode org settings: %v", err)
	}
	return out.HasAnthropicAPIKey
}

// TestAnthropicPut_ValidKey_StoresAndSetsRef drives the BYOK happy path: a
// key that validates (stub 200) is stored in the vault under anthropic_api_key,
// AnthropicAPIKeyRef is set (so has_anthropic_api_key flips true), and the
// resolver would find it.
func TestAnthropicPut_ValidKey_StoresAndSetsRef(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	s := newTestServer(t)
	ctx := context.Background()

	auth.SetAnthropicModelsURLForTest(t, anthropicModelsStub(t, http.StatusOK).URL)

	rec := doJSON(t, s, http.MethodPut, llmPath("anthropic"), map[string]any{"api_key": "  sk-ant-valid  "})
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s, want 200", rec.Code, rec.Body.String())
	}

	// Stored under the canonical key — trimmed of the surrounding whitespace.
	stored, err := s.secrets.Get(ctx, runmode.LocalDefaultOrgID, "anthropic_api_key")
	if err != nil {
		t.Fatalf("Get anthropic_api_key: %v", err)
	}
	if stored != "sk-ant-valid" {
		t.Errorf("stored key = %q, want the trimmed key", stored)
	}
	if !orgHasAnthropicKey(t, s) {
		t.Error("has_anthropic_api_key=false after storing a valid key")
	}
}

// TestAnthropicPut_BadKey_Returns422_NothingStored pins the rejection path:
// a key the host rejects (stub 401) is a 422, the message is surfaced, and no
// secret nor ref is written.
func TestAnthropicPut_BadKey_Returns422_NothingStored(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	s := newTestServer(t)
	ctx := context.Background()

	auth.SetAnthropicModelsURLForTest(t, anthropicModelsStub(t, http.StatusUnauthorized).URL)

	rec := doJSON(t, s, http.MethodPut, llmPath("anthropic"), map[string]any{"api_key": "sk-ant-bad"})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status=%d body=%s, want 422", rec.Code, rec.Body.String())
	}
	if firstErrorMessage(t, rec) == "" {
		t.Error("422 carried no error message")
	}

	stored, _ := s.secrets.Get(ctx, runmode.LocalDefaultOrgID, "anthropic_api_key")
	if stored != "" {
		t.Errorf("key stored despite rejection: %q", stored)
	}
	if orgHasAnthropicKey(t, s) {
		t.Error("has_anthropic_api_key=true after a rejected key")
	}
}

// TestAnthropicDelete_ClearsStoredKey drives the "use system credentials" path.
// It is a DELETE, not a blank PUT: a blank secret used to mean "clear the key
// AND every Bedrock secret", the opposite of what the same blank meant on the
// Bedrock route, and neither credential had a delete of its own. The blank-key
// arm is now a 400 naming the field (asserted below), so the destructive intent
// has exactly one spelling.
func TestAnthropicDelete_ClearsStoredKey(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	s := newTestServer(t)
	ctx := context.Background()

	// Seed a stored key via the validated path (stub 200).
	auth.SetAnthropicModelsURLForTest(t, anthropicModelsStub(t, http.StatusOK).URL)
	if rec := doJSON(t, s, http.MethodPut, llmPath("anthropic"), map[string]any{"api_key": "sk-ant-seed"}); rec.Code != http.StatusOK {
		t.Fatalf("seed status=%d body=%s, want 200", rec.Code, rec.Body.String())
	}
	if !orgHasAnthropicKey(t, s) {
		t.Fatal("seed did not set has_anthropic_api_key")
	}

	// A blank key is refused rather than silently destructive.
	blank := doJSON(t, s, http.MethodPut, llmPath("anthropic"), map[string]any{"api_key": "   "})
	if blank.Code != http.StatusBadRequest {
		t.Fatalf("blank key status=%d body=%s, want 400", blank.Code, blank.Body.String())
	}
	assertFirstError(t, blank, httpx.ReasonMissingField, "api_key")
	if !orgHasAnthropicKey(t, s) {
		t.Fatal("the refused blank PUT cleared the stored key")
	}

	// The DELETE clears it — no validator round-trip.
	rec := doJSON(t, s, http.MethodDelete, llmPath("anthropic"), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("clear status=%d body=%s, want 200", rec.Code, rec.Body.String())
	}
	stored, _ := s.secrets.Get(ctx, runmode.LocalDefaultOrgID, "anthropic_api_key")
	if stored != "" {
		t.Errorf("key still stored after clear: %q", stored)
	}
	if orgHasAnthropicKey(t, s) {
		t.Error("has_anthropic_api_key=true after clearing")
	}
}

// TestAnthropicPut_Unreachable_Returns422 pins that a transport failure
// (stub closed) surfaces as a 422 on the bind surface (mirroring
// handleJiraConnect's single-422 shape) and stores nothing.
func TestAnthropicPut_Unreachable_Returns422(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	s := newTestServer(t)

	// Stand a stub up to claim a port, then close it so dials are refused.
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := dead.URL
	dead.Close()
	auth.SetAnthropicModelsURLForTest(t, deadURL)

	rec := doJSON(t, s, http.MethodPut, llmPath("anthropic"), map[string]any{"api_key": "sk-ant-x"})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status=%d body=%s, want 422", rec.Code, rec.Body.String())
	}
}

// TestAnthropicPut_SettingsPatchNoBackDoor pins that the org-settings PATCH
// is NOT a write path for the key: an anthropic_api_key in that body is
// REJECTED (strict decoding — the field doesn't exist on the schema) rather
// than silently ignored, so the only way to set it is the validated resource
// and a client that tries the back door is told so.
func TestAnthropicPut_SettingsPatchNoBackDoor(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	s := newTestServer(t)
	ctx := context.Background()

	rec := patchOrgSettings(t, s, map[string]any{
		"github_base_url":      "https://github.com",
		"anthropic_api_key":    "sk-ant-sneaky",
		"github_poll_interval": "5m0s",
		"jira_poll_interval":   "5m0s",
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("settings patch status=%d body=%s, want 400", rec.Code, rec.Body.String())
	}
	assertFirstError(t, rec, httpx.ReasonUnknownField, "anthropic_api_key")
	stored, _ := s.secrets.Get(ctx, runmode.LocalDefaultOrgID, "anthropic_api_key")
	if stored != "" {
		t.Errorf("the settings PATCH stored the key (back door): %q", stored)
	}
	if orgHasAnthropicKey(t, s) {
		t.Error("has_anthropic_api_key=true after a settings PATCH (back door)")
	}
}
