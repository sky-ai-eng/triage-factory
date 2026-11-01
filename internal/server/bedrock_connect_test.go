package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/auth"
	"github.com/sky-ai-eng/triage-factory/internal/integrations"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
	"github.com/zalando/go-keyring"
)

// orgBedrockView reads the Bedrock-facing slice of the org-settings GET —
// the presence flag plus the non-secret config the Settings form renders.
type orgBedrockView struct {
	Has      bool   `json:"has_bedrock_credentials"`
	Method   string `json:"bedrock_auth_method"`
	Region   string `json:"bedrock_region"`
	ModelID  string `json:"bedrock_model_id"`
	BaseURL  string `json:"bedrock_base_url"`
	HasAnthr bool   `json:"has_anthropic_api_key"`
}

func getOrgBedrockView(t *testing.T, s *Server) orgBedrockView {
	t.Helper()
	rec := doJSON(t, s, "GET", orgSettingsPath(), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s status=%d body=%s", orgSettingsPath(), rec.Code, rec.Body.String())
	}
	var out orgBedrockView
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode org settings: %v", err)
	}
	return out
}

// putBedrock binds one Bedrock shape through its own route.
func putBedrock(t *testing.T, s *Server, shape string, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	return doJSON(t, s, http.MethodPut, llmPath("bedrock/"+shape), body)
}

// clearedList reads the `cleared` enumeration off a bind response: the
// provider material this write removed.
func clearedList(t *testing.T, rec *httptest.ResponseRecorder) []string {
	t.Helper()
	var out struct {
		Cleared []string `json:"cleared"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode bind response: %v", err)
	}
	if out.Cleared == nil {
		t.Fatalf("response omitted `cleared` entirely: %s", rec.Body.String())
	}
	return out.Cleared
}

// mustSecret reads a vault key, failing the test on a backend error.
func mustSecret(t *testing.T, s *Server, key string) string {
	t.Helper()
	v, err := s.secrets.Get(context.Background(), runmode.LocalDefaultOrgID, key)
	if err != nil {
		t.Fatalf("Get %s: %v", key, err)
	}
	return v
}

// TestBedrockPut_BlankSecretIsRefusedNotKept: each shape's bind replaces the
// credential, so a blank secret is a 400 naming the field and leaves the stored
// credential and its config exactly as they were.
func TestBedrockPut_BlankSecretIsRefusedNotKept(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	s := newTestServer(t)

	seed := putBedrock(t, s, "access-keys", map[string]any{
		"access_key_id":     "AKIAKEEP",
		"secret_access_key": "secret-keep",
		"session_token":     "session-keep",
		"region":            "us-gov-west-1", // GovCloud regions pass validation
		"model_id":          "old-model",
	})
	if seed.Code != http.StatusOK {
		t.Fatalf("seed status=%d body=%s", seed.Code, seed.Body.String())
	}

	rec := putBedrock(t, s, "access-keys", map[string]any{
		"region":   "eu-central-1",
		"model_id": "new-model",
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("blank-secret status=%d body=%s, want 400", rec.Code, rec.Body.String())
	}
	assertFirstError(t, rec, httpx.ReasonMissingField, "access_key_id")

	// Nothing moved — not the secrets, and not the config the request also
	// carried.
	for k, want := range map[string]string{
		integrations.KeyAWSAccessKeyID:     "AKIAKEEP",
		integrations.KeyAWSSecretAccessKey: "secret-keep",
		integrations.KeyAWSSessionToken:    "session-keep",
	} {
		if got := mustSecret(t, s, k); got != want {
			t.Errorf("%s = %q, want %q untouched by the refused write", k, got, want)
		}
	}
	if view := getOrgBedrockView(t, s); !view.Has || view.Method != "access_keys" || view.Region != "us-gov-west-1" || view.ModelID != "old-model" {
		t.Errorf("view = %+v, want the seeded access_keys config untouched by the refused write", view)
	}
}

// TestBedrockAccessKeysPut_ReplaceClearsSessionToken: a new key pair with no
// session token means long-lived keys, not "keep the old token" — a stale
// token would 403 every AWS call.
func TestBedrockAccessKeysPut_ReplaceClearsSessionToken(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	s := newTestServer(t)

	seed := putBedrock(t, s, "access-keys", map[string]any{
		"access_key_id":     "AKIAOLD",
		"secret_access_key": "secret-old",
		"session_token":     "session-old",
		"region":            "us-east-1",
	})
	if seed.Code != http.StatusOK {
		t.Fatalf("seed status=%d body=%s", seed.Code, seed.Body.String())
	}

	rec := putBedrock(t, s, "access-keys", map[string]any{
		"access_key_id":     "AKIANEW",
		"secret_access_key": "secret-new",
		"region":            "us-east-1",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("replace status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := mustSecret(t, s, integrations.KeyAWSAccessKeyID); got != "AKIANEW" {
		t.Errorf("access key = %q, want replaced", got)
	}
	if got := mustSecret(t, s, integrations.KeyAWSSessionToken); got != "" {
		t.Errorf("session token = %q, want cleared on replace with no token", got)
	}
}

// TestBedrockPut_ShapeSwitchClearsOtherShape: binding one Bedrock shape removes
// the previous shape's material in both directions, and the response's `cleared`
// names it. The resolver picks a shape by which keys are stored, so a leftover
// would be used instead of the shape just bound.
func TestBedrockPut_ShapeSwitchClearsOtherShape(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	s := newTestServer(t)

	if rec := putBedrock(t, s, "bearer", map[string]any{
		"bearer_token": "bdrk-1", "region": "us-east-1",
	}); rec.Code != http.StatusOK {
		t.Fatalf("bearer seed: %d %s", rec.Code, rec.Body.String())
	}
	rec := putBedrock(t, s, "access-keys", map[string]any{
		"access_key_id": "AKIA2", "secret_access_key": "sec2", "region": "us-east-1",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("switch to access keys: %d %s", rec.Code, rec.Body.String())
	}
	if got := clearedList(t, rec); len(got) != 1 || got[0] != "bedrock:bearer" {
		t.Errorf("cleared = %v, want [bedrock:bearer]", got)
	}
	if got := mustSecret(t, s, integrations.KeyAWSBearerTokenBedrock); got != "" {
		t.Errorf("bearer token = %q after switching to access keys, want cleared", got)
	}
	if view := getOrgBedrockView(t, s); view.Method != "access_keys" {
		t.Errorf("method = %q, want access_keys", view.Method)
	}

	// And back: the triple must go when a bearer replaces it.
	if rec := putBedrock(t, s, "bearer", map[string]any{
		"bearer_token": "bdrk-2", "region": "us-east-1",
	}); rec.Code != http.StatusOK {
		t.Fatalf("switch back to bearer: %d %s", rec.Code, rec.Body.String())
	}
	for _, k := range []string{integrations.KeyAWSAccessKeyID, integrations.KeyAWSSecretAccessKey, integrations.KeyAWSSessionToken} {
		if got := mustSecret(t, s, k); got != "" {
			t.Errorf("%s = %q after switching to bearer, want cleared", k, got)
		}
	}
}

// TestBedrockDelete: a bearer bind stores the trimmed token and its config and
// GET echoes the config; the unbind removes every Bedrock key and the ref, and
// repeating it is a success.
func TestBedrockDelete(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	s := newTestServer(t)

	rec := putBedrock(t, s, "bearer", map[string]any{
		"bearer_token": "  bdrk-token-1  ",
		"region":       "us-west-2",
		"model_id":     "us.anthropic.claude-haiku-4-5",
		"base_url":     "https://vpce-0abc.bedrock-runtime.us-west-2.vpce.amazonaws.com/",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("seed: %d %s", rec.Code, rec.Body.String())
	}
	if got := clearedList(t, rec); len(got) != 0 {
		t.Errorf("cleared = %v on a fresh org, want empty", got)
	}
	for k, want := range map[string]string{
		integrations.KeyAWSBearerTokenBedrock: "bdrk-token-1",
		integrations.KeyAWSRegion:             "us-west-2",
		integrations.KeyBedrockModelID:        "us.anthropic.claude-haiku-4-5",
		// The trailing slash is stripped: the proxy's upstream validation
		// rejects a path component.
		integrations.KeyBedrockBaseURL: "https://vpce-0abc.bedrock-runtime.us-west-2.vpce.amazonaws.com",
	} {
		if got := mustSecret(t, s, k); got != want {
			t.Errorf("stored %s = %q, want %q", k, got, want)
		}
	}
	if view := getOrgBedrockView(t, s); !view.Has || view.Method != "bearer" || view.Region != "us-west-2" || view.ModelID != "us.anthropic.claude-haiku-4-5" {
		t.Errorf("view = %+v, want bearer with the stored region and model echoed", view)
	}

	rec = doJSON(t, s, http.MethodDelete, llmPath("bedrock"), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("clear status=%d body=%s", rec.Code, rec.Body.String())
	}
	for _, k := range integrations.BedrockKeys() {
		if got := mustSecret(t, s, k); got != "" {
			t.Errorf("%s = %q after clear, want empty", k, got)
		}
	}
	if view := getOrgBedrockView(t, s); view.Has || view.Method != "" {
		t.Errorf("view = %+v after clear, want unconfigured", view)
	}
	// Idempotent: removing nothing is a success, not a 404.
	if again := doJSON(t, s, http.MethodDelete, llmPath("bedrock"), nil); again.Code != http.StatusOK {
		t.Errorf("second delete = %d, want 200 (idempotent)", again.Code)
	}
}

// TestLLMCredentials_BothProvidersCoexist: Anthropic and Bedrock credentials
// are independent. Binding either leaves the other's material in place, and
// each DELETE removes only its own.
func TestLLMCredentials_BothProvidersCoexist(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	s := newTestServer(t)

	auth.SetAnthropicModelsURLForTest(t, anthropicModelsStub(t, http.StatusOK).URL)

	// Anthropic first, then Bedrock: the Anthropic key stays, and the bind says
	// it removed nothing.
	if rec := doJSON(t, s, http.MethodPut, llmPath("anthropic"), map[string]any{"api_key": "sk-ant-first"}); rec.Code != http.StatusOK {
		t.Fatalf("anthropic seed: %d %s", rec.Code, rec.Body.String())
	}
	rec := putBedrock(t, s, "bearer", map[string]any{"bearer_token": "bdrk-1", "region": "us-east-1"})
	if rec.Code != http.StatusOK {
		t.Fatalf("bedrock bind: %d %s", rec.Code, rec.Body.String())
	}
	if got := clearedList(t, rec); len(got) != 0 {
		t.Errorf("cleared = %v, want [] (binding Bedrock removes no Anthropic material)", got)
	}
	if got := mustSecret(t, s, "anthropic_api_key"); got != "sk-ant-first" {
		t.Errorf("anthropic key = %q after the Bedrock bind, want it untouched", got)
	}
	if got := mustSecret(t, s, integrations.KeyAWSBearerTokenBedrock); got != "bdrk-1" {
		t.Errorf("bedrock bearer = %q, want bdrk-1", got)
	}
	view := getOrgBedrockView(t, s)
	if !view.HasAnthr || !view.Has {
		t.Errorf("view = %+v, want BOTH providers configured", view)
	}

	// Rotating the Anthropic key leaves Bedrock alone, the other direction of
	// the same rule.
	back := doJSON(t, s, http.MethodPut, llmPath("anthropic"), map[string]any{"api_key": "sk-ant-back"})
	if back.Code != http.StatusOK {
		t.Fatalf("anthropic rebind: %d %s", back.Code, back.Body.String())
	}
	if got := clearedList(t, back); len(got) != 0 {
		t.Errorf("cleared = %v, want [] (binding Anthropic removes no Bedrock material)", got)
	}
	if got := mustSecret(t, s, integrations.KeyAWSBearerTokenBedrock); got != "bdrk-1" {
		t.Errorf("bedrock bearer = %q after the Anthropic bind, want it untouched", got)
	}
	view = getOrgBedrockView(t, s)
	if !view.HasAnthr || !view.Has {
		t.Errorf("view = %+v, want both still configured", view)
	}

	// Each disconnect removes its own provider and only its own.
	if rec := doJSON(t, s, http.MethodDelete, llmPath("anthropic"), nil); rec.Code != http.StatusOK {
		t.Fatalf("anthropic delete: %d %s", rec.Code, rec.Body.String())
	}
	view = getOrgBedrockView(t, s)
	if view.HasAnthr || !view.Has {
		t.Errorf("view = %+v after disconnecting Anthropic, want Bedrock still configured", view)
	}
	if rec := doJSON(t, s, http.MethodDelete, llmPath("bedrock"), nil); rec.Code != http.StatusOK {
		t.Fatalf("bedrock delete: %d %s", rec.Code, rec.Body.String())
	}
	view = getOrgBedrockView(t, s)
	if view.Has || view.HasAnthr {
		t.Errorf("view = %+v after both deletes, want no provider configured", view)
	}
}

// TestBedrockPut_ValidationErrors: every shape fault is a 400 naming each
// failing field, and each route's strict decode rejects another shape's field
// by name. Nothing a rejected request carried is stored.
func TestBedrockPut_ValidationErrors(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	s := newTestServer(t)

	cases := []struct {
		name   string
		shape  string
		body   map[string]any
		fields []string
	}{
		{"missing_region", "bearer", map[string]any{"bearer_token": "b"}, []string{"region"}},
		{"malformed_region", "bearer", map[string]any{"bearer_token": "b", "region": "US EAST"}, []string{"region"}},
		{"base_url_with_path", "bearer", map[string]any{"bearer_token": "b", "region": "us-east-1", "base_url": "https://x.example.com/v1"}, []string{"base_url"}},
		{"base_url_http", "bearer", map[string]any{"bearer_token": "b", "region": "us-east-1", "base_url": "http://x.example.com"}, []string{"base_url"}},
		{"base_url_no_scheme", "bearer", map[string]any{"bearer_token": "b", "region": "us-east-1", "base_url": "x.example.com"}, []string{"base_url"}},
		{"missing_bearer", "bearer", map[string]any{"region": "us-east-1"}, []string{"bearer_token"}},
		{"partial_pair_id_only", "access-keys", map[string]any{"access_key_id": "AKIA", "region": "us-east-1"}, []string{"secret_access_key"}},
		{"partial_pair_secret_only", "access-keys", map[string]any{"secret_access_key": "s", "region": "us-east-1"}, []string{"access_key_id"}},
		{"missing_pair", "access-keys", map[string]any{"region": "us-east-1"}, []string{"access_key_id"}},
		// Several faults in one body are all reported, not the first one hit.
		{"every_fault_reported", "access-keys", map[string]any{"region": "not a region"}, []string{"region", "access_key_id", "secret_access_key"}},
		{"cross_shape_field", "bearer", map[string]any{"bearer_token": "b", "region": "us-east-1", "access_key_id": "AKIA"}, []string{"access_key_id"}},
		{"role_arn_on_bearer", "bearer", map[string]any{"bearer_token": "b", "region": "us-east-1", "role_arn": "arn:aws:iam::123456789012:role/x"}, []string{"role_arn"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := putBedrock(t, s, c.shape, c.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s, want 400", rec.Code, rec.Body.String())
			}
			got := errorFields(t, rec)
			for _, want := range c.fields {
				if !containsString(got, want) {
					t.Errorf("error fields = %v, want one naming %q; body=%s", got, want, rec.Body.String())
				}
			}
		})
	}
	if view := getOrgBedrockView(t, s); view.Has {
		t.Errorf("view = %+v, want unconfigured after only-rejected requests", view)
	}
}
