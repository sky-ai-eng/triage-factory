package server

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/auth"
	"github.com/sky-ai-eng/triage-factory/internal/integrations"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/zalando/go-keyring"
)

// orgBedrockView reads the Bedrock-facing slice of GET /api/settings/org —
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
	rec := doJSON(t, s, "GET", "/api/settings/org", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/settings/org status=%d body=%s", rec.Code, rec.Body.String())
	}
	var out orgBedrockView
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode org settings: %v", err)
	}
	return out
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

// TestBedrockConnect_Bearer_StoresAndSetsRef drives the Bedrock API-key
// happy path: the token lands in the vault under aws_bearer_token_bedrock,
// the non-secret config is stored alongside, BedrockCredentialsRef flips
// the presence flag, and GET echoes the config for the form.
func TestBedrockConnect_Bearer_StoresAndSetsRef(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	s := newTestServer(t)

	rec := doJSON(t, s, "POST", "/api/bedrock/connect", map[string]any{
		"auth_method":  "bearer",
		"bearer_token": "  bdrk-token-1  ",
		"region":       "us-west-2",
		"model_id":     "us.anthropic.claude-haiku-4-5",
		"base_url":     "https://vpce-0abc.bedrock-runtime.us-west-2.vpce.amazonaws.com/",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s, want 200", rec.Code, rec.Body.String())
	}

	if got := mustSecret(t, s, integrations.KeyAWSBearerTokenBedrock); got != "bdrk-token-1" {
		t.Errorf("stored bearer = %q, want the trimmed token", got)
	}
	if got := mustSecret(t, s, integrations.KeyAWSRegion); got != "us-west-2" {
		t.Errorf("stored region = %q, want us-west-2", got)
	}
	if got := mustSecret(t, s, integrations.KeyBedrockModelID); got != "us.anthropic.claude-haiku-4-5" {
		t.Errorf("stored model = %q", got)
	}
	// Trailing slash stripped before storage — the proxy upstream
	// validation rejects a path component.
	if got := mustSecret(t, s, integrations.KeyBedrockBaseURL); got != "https://vpce-0abc.bedrock-runtime.us-west-2.vpce.amazonaws.com" {
		t.Errorf("stored base URL = %q, want the trailing slash stripped", got)
	}

	view := getOrgBedrockView(t, s)
	if !view.Has || view.Method != "bearer" {
		t.Errorf("view = %+v, want has_bedrock_credentials + method bearer", view)
	}
	if view.Region != "us-west-2" || view.ModelID != "us.anthropic.claude-haiku-4-5" {
		t.Errorf("GET did not echo the non-secret config: %+v", view)
	}
}

// TestBedrockConnect_AccessKeys_StoresTriple drives the IAM path: the full
// triple (with session token) lands in the vault and the method marker
// reads back access_keys.
func TestBedrockConnect_AccessKeys_StoresTriple(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	s := newTestServer(t)

	rec := doJSON(t, s, "POST", "/api/bedrock/connect", map[string]any{
		"auth_method":       "access_keys",
		"access_key_id":     "AKIAEXAMPLE",
		"secret_access_key": "secret-example",
		"session_token":     "session-example",
		"region":            "us-gov-west-1",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s, want 200", rec.Code, rec.Body.String())
	}

	if got := mustSecret(t, s, integrations.KeyAWSAccessKeyID); got != "AKIAEXAMPLE" {
		t.Errorf("stored access key = %q", got)
	}
	if got := mustSecret(t, s, integrations.KeyAWSSecretAccessKey); got != "secret-example" {
		t.Errorf("stored secret key = %q", got)
	}
	if got := mustSecret(t, s, integrations.KeyAWSSessionToken); got != "session-example" {
		t.Errorf("stored session token = %q", got)
	}
	view := getOrgBedrockView(t, s)
	if !view.Has || view.Method != "access_keys" {
		t.Errorf("view = %+v, want has_bedrock_credentials + method access_keys", view)
	}
	if view.Region != "us-gov-west-1" {
		t.Errorf("region = %q, want us-gov-west-1 (GovCloud regions must pass validation)", view.Region)
	}
}

// TestBedrockConnect_KeepCurrentOnBlankSecrets pins the "leave blank to
// keep current" convention: re-posting the same auth method with every
// secret field blank keeps the stored credential and applies only the
// non-secret config changes.
func TestBedrockConnect_KeepCurrentOnBlankSecrets(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	s := newTestServer(t)

	seed := doJSON(t, s, "POST", "/api/bedrock/connect", map[string]any{
		"auth_method":       "access_keys",
		"access_key_id":     "AKIAKEEP",
		"secret_access_key": "secret-keep",
		"session_token":     "session-keep",
		"region":            "us-east-1",
		"model_id":          "old-model",
	})
	if seed.Code != http.StatusOK {
		t.Fatalf("seed status=%d body=%s", seed.Code, seed.Body.String())
	}

	rec := doJSON(t, s, "POST", "/api/bedrock/connect", map[string]any{
		"auth_method": "access_keys",
		"region":      "eu-central-1",
		"model_id":    "new-model",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("keep-current status=%d body=%s, want 200", rec.Code, rec.Body.String())
	}

	// Secrets untouched, including the session token.
	if got := mustSecret(t, s, integrations.KeyAWSAccessKeyID); got != "AKIAKEEP" {
		t.Errorf("access key = %q, want kept", got)
	}
	if got := mustSecret(t, s, integrations.KeyAWSSecretAccessKey); got != "secret-keep" {
		t.Errorf("secret key = %q, want kept", got)
	}
	if got := mustSecret(t, s, integrations.KeyAWSSessionToken); got != "session-keep" {
		t.Errorf("session token = %q, want kept", got)
	}
	// Config applied literally, including the blank base_url (unset).
	view := getOrgBedrockView(t, s)
	if view.Region != "eu-central-1" || view.ModelID != "new-model" || view.BaseURL != "" {
		t.Errorf("config not applied: %+v", view)
	}
}

// TestBedrockConnect_ReplaceClearsSessionToken pins the replace semantics:
// submitting a new key pair with a blank session token means "no session
// token" (long-lived keys), not "keep the old one" — a stale token would
// 403 every AWS call.
func TestBedrockConnect_ReplaceClearsSessionToken(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	s := newTestServer(t)

	seed := doJSON(t, s, "POST", "/api/bedrock/connect", map[string]any{
		"auth_method":       "access_keys",
		"access_key_id":     "AKIAOLD",
		"secret_access_key": "secret-old",
		"session_token":     "session-old",
		"region":            "us-east-1",
	})
	if seed.Code != http.StatusOK {
		t.Fatalf("seed status=%d body=%s", seed.Code, seed.Body.String())
	}

	rec := doJSON(t, s, "POST", "/api/bedrock/connect", map[string]any{
		"auth_method":       "access_keys",
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
		t.Errorf("session token = %q, want cleared on replace with blank token", got)
	}
}

// TestBedrockConnect_MethodSwitchClearsOtherMethod pins that switching auth
// methods removes the previous method's secret material — no stale
// credentials linger in the vault.
func TestBedrockConnect_MethodSwitchClearsOtherMethod(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	s := newTestServer(t)

	if rec := doJSON(t, s, "POST", "/api/bedrock/connect", map[string]any{
		"auth_method": "bearer", "bearer_token": "bdrk-1", "region": "us-east-1",
	}); rec.Code != http.StatusOK {
		t.Fatalf("bearer seed: %d %s", rec.Code, rec.Body.String())
	}
	if rec := doJSON(t, s, "POST", "/api/bedrock/connect", map[string]any{
		"auth_method": "access_keys", "access_key_id": "AKIA2", "secret_access_key": "sec2", "region": "us-east-1",
	}); rec.Code != http.StatusOK {
		t.Fatalf("switch to access_keys: %d %s", rec.Code, rec.Body.String())
	}
	if got := mustSecret(t, s, integrations.KeyAWSBearerTokenBedrock); got != "" {
		t.Errorf("bearer token = %q after switching to access_keys, want cleared", got)
	}
	if view := getOrgBedrockView(t, s); view.Method != "access_keys" {
		t.Errorf("method = %q, want access_keys", view.Method)
	}

	// And back: the triple must go when a bearer replaces it.
	if rec := doJSON(t, s, "POST", "/api/bedrock/connect", map[string]any{
		"auth_method": "bearer", "bearer_token": "bdrk-2", "region": "us-east-1",
	}); rec.Code != http.StatusOK {
		t.Fatalf("switch back to bearer: %d %s", rec.Code, rec.Body.String())
	}
	for _, k := range []string{integrations.KeyAWSAccessKeyID, integrations.KeyAWSSecretAccessKey, integrations.KeyAWSSessionToken} {
		if got := mustSecret(t, s, k); got != "" {
			t.Errorf("%s = %q after switching to bearer, want cleared", k, got)
		}
	}
}

// TestBedrockConnect_Clear pins the explicit clear: auth_method "none"
// removes every Bedrock key and the ref, flipping the presence flag off.
func TestBedrockConnect_Clear(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	s := newTestServer(t)

	if rec := doJSON(t, s, "POST", "/api/bedrock/connect", map[string]any{
		"auth_method": "bearer", "bearer_token": "bdrk-1", "region": "us-east-1", "model_id": "m",
	}); rec.Code != http.StatusOK {
		t.Fatalf("seed: %d %s", rec.Code, rec.Body.String())
	}

	rec := doJSON(t, s, "POST", "/api/bedrock/connect", map[string]any{"auth_method": "none"})
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
}

// TestBedrockConnect_ExclusivityWithAnthropic pins the provider switch in
// both directions: connecting Bedrock clears a stored Anthropic key (which
// would otherwise win in the resolver and silently ignore Bedrock), and
// connecting Anthropic clears the Bedrock set. The "system credentials"
// clear (empty Anthropic key) wipes Bedrock too.
func TestBedrockConnect_ExclusivityWithAnthropic(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	s := newTestServer(t)

	auth.SetAnthropicModelsURLForTest(t, anthropicModelsStub(t, http.StatusOK).URL)

	// Anthropic first, then Bedrock: the Anthropic key must go.
	if rec := doJSON(t, s, "POST", "/api/anthropic/connect", map[string]any{"api_key": "sk-ant-first"}); rec.Code != http.StatusOK {
		t.Fatalf("anthropic seed: %d %s", rec.Code, rec.Body.String())
	}
	if rec := doJSON(t, s, "POST", "/api/bedrock/connect", map[string]any{
		"auth_method": "bearer", "bearer_token": "bdrk-1", "region": "us-east-1",
	}); rec.Code != http.StatusOK {
		t.Fatalf("bedrock connect: %d %s", rec.Code, rec.Body.String())
	}
	if got := mustSecret(t, s, "anthropic_api_key"); got != "" {
		t.Errorf("anthropic key = %q after Bedrock connect, want cleared (resolver would prefer it)", got)
	}
	view := getOrgBedrockView(t, s)
	if view.HasAnthr || !view.Has {
		t.Errorf("view = %+v, want bedrock-only after the switch", view)
	}

	// Bedrock then Anthropic: the Bedrock set must go.
	if rec := doJSON(t, s, "POST", "/api/anthropic/connect", map[string]any{"api_key": "sk-ant-back"}); rec.Code != http.StatusOK {
		t.Fatalf("anthropic reconnect: %d %s", rec.Code, rec.Body.String())
	}
	if got := mustSecret(t, s, integrations.KeyAWSBearerTokenBedrock); got != "" {
		t.Errorf("bedrock bearer = %q after Anthropic connect, want cleared", got)
	}
	view = getOrgBedrockView(t, s)
	if !view.HasAnthr || view.Has {
		t.Errorf("view = %+v, want anthropic-only after switching back", view)
	}

	// "System credentials" (empty Anthropic key) clears Bedrock too.
	if rec := doJSON(t, s, "POST", "/api/bedrock/connect", map[string]any{
		"auth_method": "bearer", "bearer_token": "bdrk-2", "region": "us-east-1",
	}); rec.Code != http.StatusOK {
		t.Fatalf("bedrock reseed: %d %s", rec.Code, rec.Body.String())
	}
	if rec := doJSON(t, s, "POST", "/api/anthropic/connect", map[string]any{"api_key": ""}); rec.Code != http.StatusOK {
		t.Fatalf("system-creds clear: %d %s", rec.Code, rec.Body.String())
	}
	view = getOrgBedrockView(t, s)
	if view.Has || view.HasAnthr {
		t.Errorf("view = %+v after system-creds clear, want no provider configured", view)
	}
}

// TestBedrockConnect_ValidationErrors pins the 422 shapes: bad method,
// missing/malformed region, malformed endpoint override, partial key pair,
// and blank secrets with nothing stored to keep.
func TestBedrockConnect_ValidationErrors(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	s := newTestServer(t)

	cases := []struct {
		name string
		body map[string]any
	}{
		{"unknown_method", map[string]any{"auth_method": "iam", "region": "us-east-1"}},
		{"missing_region", map[string]any{"auth_method": "bearer", "bearer_token": "b"}},
		{"malformed_region", map[string]any{"auth_method": "bearer", "bearer_token": "b", "region": "US EAST"}},
		{"base_url_with_path", map[string]any{"auth_method": "bearer", "bearer_token": "b", "region": "us-east-1", "base_url": "https://x.example.com/v1"}},
		{"base_url_http", map[string]any{"auth_method": "bearer", "bearer_token": "b", "region": "us-east-1", "base_url": "http://x.example.com"}},
		{"base_url_no_scheme", map[string]any{"auth_method": "bearer", "bearer_token": "b", "region": "us-east-1", "base_url": "x.example.com"}},
		{"partial_pair_id_only", map[string]any{"auth_method": "access_keys", "access_key_id": "AKIA", "region": "us-east-1"}},
		{"partial_pair_secret_only", map[string]any{"auth_method": "access_keys", "secret_access_key": "s", "region": "us-east-1"}},
		{"blank_bearer_nothing_stored", map[string]any{"auth_method": "bearer", "region": "us-east-1"}},
		{"blank_keys_nothing_stored", map[string]any{"auth_method": "access_keys", "region": "us-east-1"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := doJSON(t, s, "POST", "/api/bedrock/connect", c.body)
			if rec.Code != http.StatusUnprocessableEntity {
				t.Errorf("status=%d body=%s, want 422", rec.Code, rec.Body.String())
			}
		})
	}
	// Nothing was stored by any of the rejected requests.
	if view := getOrgBedrockView(t, s); view.Has {
		t.Errorf("view = %+v, want unconfigured after only-rejected requests", view)
	}
}

// TestBedrockConnect_MethodSwitchRequiresSecrets pins that keep-current
// does not cross auth methods: blank secrets with a DIFFERENT method
// stored is a 422, not a silent half-switch.
func TestBedrockConnect_MethodSwitchRequiresSecrets(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	s := newTestServer(t)

	if rec := doJSON(t, s, "POST", "/api/bedrock/connect", map[string]any{
		"auth_method": "bearer", "bearer_token": "bdrk-1", "region": "us-east-1",
	}); rec.Code != http.StatusOK {
		t.Fatalf("seed: %d %s", rec.Code, rec.Body.String())
	}
	rec := doJSON(t, s, "POST", "/api/bedrock/connect", map[string]any{
		"auth_method": "access_keys", "region": "us-east-1",
	})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status=%d body=%s, want 422 (no stored pair to keep)", rec.Code, rec.Body.String())
	}
	// The stored bearer config is untouched by the rejected switch.
	if view := getOrgBedrockView(t, s); view.Method != "bearer" {
		t.Errorf("method = %q after rejected switch, want bearer", view.Method)
	}
}

// TestBedrockConnect_BulkPostNoBackDoor pins that the bulk org-settings
// POST is not a write path for any Bedrock field — the validated connect
// endpoint is the only door, mirroring the Anthropic rule.
func TestBedrockConnect_BulkPostNoBackDoor(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	s := newTestServer(t)

	rec := doJSON(t, s, "POST", "/api/settings/org", map[string]any{
		"github_base_url":          "https://github.com",
		"aws_bearer_token_bedrock": "bdrk-sneaky",
		"aws_access_key_id":        "AKIA-sneaky",
		"aws_secret_access_key":    "secret-sneaky",
		"bedrock_base_url":         "https://sneaky.example.com",
		"github_poll_interval":     "5m0s",
		"jira_poll_interval":       "5m0s",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("bulk settings status=%d body=%s, want 200", rec.Code, rec.Body.String())
	}
	for _, k := range integrations.BedrockKeys() {
		if got := mustSecret(t, s, k); got != "" {
			t.Errorf("bulk POST stored %s = %q (back door)", k, got)
		}
	}
	if view := getOrgBedrockView(t, s); view.Has {
		t.Error("has_bedrock_credentials=true after a bulk POST (back door)")
	}
}
