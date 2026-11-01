package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/auth"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/integrations"
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

// TestAnthropicDelete_ClearsStoredKey: a bind stores the trimmed key, a blank
// PUT is a 400 naming api_key that leaves the key in place, and only DELETE
// clears it. The bind and the clear each record one audit row, and no Bedrock row.
func TestAnthropicDelete_ClearsStoredKey(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	s := newTestServer(t)
	ctx := context.Background()

	auth.SetAnthropicModelsURLForTest(t, anthropicModelsStub(t, http.StatusOK).URL)
	if rec := doJSON(t, s, http.MethodPut, llmPath("anthropic"), map[string]any{"api_key": "  sk-ant-seed  "}); rec.Code != http.StatusOK {
		t.Fatalf("seed status=%d body=%s, want 200", rec.Code, rec.Body.String())
	}
	stored, err := s.secrets.Get(ctx, runmode.LocalDefaultOrgID, "anthropic_api_key")
	if err != nil {
		t.Fatalf("Get anthropic_api_key: %v", err)
	}
	if stored != "sk-ant-seed" {
		t.Errorf("stored key = %q, want the trimmed key", stored)
	}
	if !orgHasAnthropicKey(t, s) {
		t.Fatal("seed did not set has_anthropic_api_key")
	}

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
	stored, _ = s.secrets.Get(ctx, runmode.LocalDefaultOrgID, "anthropic_api_key")
	if stored != "" {
		t.Errorf("key still stored after clear: %q", stored)
	}
	if orgHasAnthropicKey(t, s) {
		t.Error("has_anthropic_api_key=true after clearing")
	}

	rows := credAuditRows(t, s)
	findCredAudit(t, rows, domain.AccessActionCredentialSet, domain.CredentialKindAnthropicKey)
	findCredAudit(t, rows, domain.AccessActionCredentialRemoved, domain.CredentialKindAnthropicKey)
	for _, r := range rows {
		if r.Kind == domain.CredentialKindBedrock {
			t.Errorf("unexpected Bedrock row %+v: Bedrock was never configured", r)
		}
	}
}

// TestAnthropicPut_Unreachable_Returns422: a key the validator cannot confirm
// is a 422 with a message, and nothing is stored. A key the host rejects takes
// the same handler path.
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
	if firstErrorMessage(t, rec) == "" {
		t.Error("422 carried no error message")
	}
	if stored, _ := s.secrets.Get(t.Context(), runmode.LocalDefaultOrgID, "anthropic_api_key"); stored != "" {
		t.Errorf("key stored despite the failed validation: %q", stored)
	}
	if orgHasAnthropicKey(t, s) {
		t.Error("has_anthropic_api_key=true after a failed validation")
	}
}

// TestSettingsPatch_NoLLMCredentialBackDoor: the org settings PATCH has no
// field for any LLM credential, so naming one is a 400 UNKNOWN_FIELD and
// nothing is stored. The validated bind routes are the only write path.
func TestSettingsPatch_NoLLMCredentialBackDoor(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	s := newTestServer(t)

	for _, field := range []string{
		"anthropic_api_key", "aws_bearer_token_bedrock", "aws_access_key_id",
		"aws_secret_access_key", "bedrock_base_url",
	} {
		t.Run(field, func(t *testing.T) {
			rec := patchOrgSettings(t, s, map[string]any{field: "sneaky", "github_poll_interval": "5m0s"})
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("settings patch status=%d body=%s, want 400", rec.Code, rec.Body.String())
			}
			assertFirstError(t, rec, httpx.ReasonUnknownField, field)
		})
	}
	for _, k := range append([]string{"anthropic_api_key"}, integrations.BedrockKeys()...) {
		if got := mustSecret(t, s, k); got != "" {
			t.Errorf("the settings PATCH stored %s = %q", k, got)
		}
	}
	if view := getOrgBedrockView(t, s); view.Has || view.HasAnthr {
		t.Errorf("view = %+v after settings PATCHes, want no LLM credential", view)
	}
}
