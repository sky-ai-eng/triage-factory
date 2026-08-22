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

// clearedList reads the `cleared` enumeration off a bind response — the
// provider material this write removed, which used to happen silently.
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

// TestBedrockBearerPut_StoresAndSetsRef drives the Bedrock API-key
// happy path: the token lands in the vault under aws_bearer_token_bedrock,
// the non-secret config is stored alongside, BedrockCredentialsRef flips
// the presence flag, and GET echoes the config for the form.
func TestBedrockBearerPut_StoresAndSetsRef(t *testing.T) {
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
		t.Fatalf("status=%d body=%s, want 200", rec.Code, rec.Body.String())
	}
	if got := clearedList(t, rec); len(got) != 0 {
		t.Errorf("cleared = %v on a fresh org, want empty", got)
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

// TestBedrockAccessKeysPut_StoresTriple drives the IAM path: the full
// triple (with session token) lands in the vault and the method marker
// reads back access_keys.
func TestBedrockAccessKeysPut_StoresTriple(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	s := newTestServer(t)

	rec := putBedrock(t, s, "access-keys", map[string]any{
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

// TestBedrockPut_BlankSecretIsRefusedNotKept is the negative space of the
// "leave blank to keep current" convention this surface used to speak. A blank
// secret meant "keep" here and "destroy every LLM credential" on the Anthropic
// sibling, and nothing on the wire told the two apart. Each shape's bind now
// REPLACES the credential, so a blank secret is a 400 naming the field and the
// stored credential is left exactly as it was.
func TestBedrockPut_BlankSecretIsRefusedNotKept(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	s := newTestServer(t)

	seed := putBedrock(t, s, "access-keys", map[string]any{
		"access_key_id":     "AKIAKEEP",
		"secret_access_key": "secret-keep",
		"session_token":     "session-keep",
		"region":            "us-east-1",
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
	// carried. A refused write writes nothing.
	if got := mustSecret(t, s, integrations.KeyAWSAccessKeyID); got != "AKIAKEEP" {
		t.Errorf("access key = %q, want untouched by the refused write", got)
	}
	if got := mustSecret(t, s, integrations.KeyAWSSessionToken); got != "session-keep" {
		t.Errorf("session token = %q, want untouched by the refused write", got)
	}
	if view := getOrgBedrockView(t, s); view.Region != "us-east-1" || view.ModelID != "old-model" {
		t.Errorf("config moved on a refused write: %+v", view)
	}
}

// TestBedrockAccessKeysPut_ReplaceClearsSessionToken pins the replace
// semantics: submitting a new key pair with no session token means "no session
// token" (long-lived keys), not "keep the old one" — a stale token would 403
// every AWS call.
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

// TestBedrockPut_ShapeSwitchClearsOtherShape pins that binding one shape
// removes the previous shape's secret material — no stale credentials linger in
// the vault — and that the response SAYS which stored material it removed.
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

// TestBedrockDelete pins the explicit unbind: it removes every Bedrock key and
// the ref, flipping the presence flag off, and it is idempotent.
func TestBedrockDelete(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	s := newTestServer(t)

	if rec := putBedrock(t, s, "bearer", map[string]any{
		"bearer_token": "bdrk-1", "region": "us-east-1", "model_id": "m",
	}); rec.Code != http.StatusOK {
		t.Fatalf("seed: %d %s", rec.Code, rec.Body.String())
	}

	rec := doJSON(t, s, http.MethodDelete, llmPath("bedrock"), nil)
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

// TestLLMCredentials_BothProvidersCoexist pins that the two providers are
// independent: binding one does not disturb the other's stored material in
// either direction, both secrets survive, and each disconnect removes only its
// own. An org running some models on Anthropic and others on Bedrock needs both
// live at once — which run uses which is decided by the run's model, not by
// which credential was bound last.
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

// A second Bedrock shape still replaces the first: the two are one credential
// wearing different clothes, and the resolver detects role mode by a stored ARN
// — so a leftover shape is material that would be used instead of the one just
// bound. The `cleared` list names it.
func TestBedrockPut_ReplacingAShapeClearsThePrevious(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	s := newTestServer(t)

	if rec := putBedrock(t, s, "bearer", map[string]any{"bearer_token": "bdrk-1", "region": "us-east-1"}); rec.Code != http.StatusOK {
		t.Fatalf("bedrock bearer bind: %d %s", rec.Code, rec.Body.String())
	}
	rec := putBedrock(t, s, "access-keys", map[string]any{
		"access_key_id": "AKIA1", "secret_access_key": "sk", "region": "us-east-1",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("bedrock access-keys bind: %d %s", rec.Code, rec.Body.String())
	}
	if got := clearedList(t, rec); len(got) != 1 || got[0] != "bedrock:bearer" {
		t.Errorf("cleared = %v, want [bedrock:bearer]", got)
	}
	if got := mustSecret(t, s, integrations.KeyAWSBearerTokenBedrock); got != "" {
		t.Errorf("bedrock bearer = %q after binding the access-key pair, want cleared", got)
	}
}

// TestBedrockPut_ValidationErrors pins the rejections. Everything here is a
// shape fault — a missing required field, a value that doesn't parse as what it
// claims to be — so everything is a 400, which is also the point of the split:
// each route's required set is fixed, so "which fields does this need" is never
// a function of what else was sent.
func TestBedrockPut_ValidationErrors(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	s := newTestServer(t)

	cases := []struct {
		name  string
		shape string
		body  map[string]any
		field string
	}{
		{"missing_region", "bearer", map[string]any{"bearer_token": "b"}, "region"},
		{"malformed_region", "bearer", map[string]any{"bearer_token": "b", "region": "US EAST"}, "region"},
		{"base_url_with_path", "bearer", map[string]any{"bearer_token": "b", "region": "us-east-1", "base_url": "https://x.example.com/v1"}, "base_url"},
		{"base_url_http", "bearer", map[string]any{"bearer_token": "b", "region": "us-east-1", "base_url": "http://x.example.com"}, "base_url"},
		{"base_url_no_scheme", "bearer", map[string]any{"bearer_token": "b", "region": "us-east-1", "base_url": "x.example.com"}, "base_url"},
		{"missing_bearer", "bearer", map[string]any{"region": "us-east-1"}, "bearer_token"},
		{"partial_pair_id_only", "access-keys", map[string]any{"access_key_id": "AKIA", "region": "us-east-1"}, "secret_access_key"},
		{"partial_pair_secret_only", "access-keys", map[string]any{"secret_access_key": "s", "region": "us-east-1"}, "access_key_id"},
		{"missing_pair", "access-keys", map[string]any{"region": "us-east-1"}, "access_key_id"},
		// The wrong shape's field is not merely ignored — the strict decode
		// against THIS shape's struct rejects it by name, which is the property
		// the one-route-with-a-discriminator body could not have.
		{"cross_shape_field", "bearer", map[string]any{"bearer_token": "b", "region": "us-east-1", "access_key_id": "AKIA"}, "access_key_id"},
		{"role_arn_on_bearer", "bearer", map[string]any{"bearer_token": "b", "region": "us-east-1", "role_arn": "arn:aws:iam::123456789012:role/x"}, "role_arn"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := putBedrock(t, s, c.shape, c.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s, want 400", rec.Code, rec.Body.String())
			}
			if got := errorFields(t, rec); !containsString(got, c.field) {
				t.Errorf("error fields = %v, want one naming %q; body=%s", got, c.field, rec.Body.String())
			}
		})
	}
	// Nothing was stored by any of the rejected requests.
	if view := getOrgBedrockView(t, s); view.Has {
		t.Errorf("view = %+v, want unconfigured after only-rejected requests", view)
	}
}

// TestBedrockPut_ReportsEveryBadField pins the accumulator: a body with two
// faults reports two, rather than the first one hit.
func TestBedrockPut_ReportsEveryBadField(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	s := newTestServer(t)

	rec := putBedrock(t, s, "access-keys", map[string]any{"region": "not a region"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s, want 400", rec.Code, rec.Body.String())
	}
	fields := errorFields(t, rec)
	for _, want := range []string{"region", "access_key_id", "secret_access_key"} {
		if !containsString(fields, want) {
			t.Errorf("error fields = %v, want one naming %q", fields, want)
		}
	}
}

// TestBedrockPut_SettingsPatchNoBackDoor pins that the org-settings PATCH is
// not a write path for any Bedrock field — the validated bind routes are the
// only door, mirroring the Anthropic rule. Strict decoding rejects the attempt
// outright rather than ignoring the fields.
func TestBedrockPut_SettingsPatchNoBackDoor(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	s := newTestServer(t)

	rec := patchOrgSettings(t, s, map[string]any{
		"github_base_url":          "https://github.com",
		"aws_bearer_token_bedrock": "bdrk-sneaky",
		"aws_access_key_id":        "AKIA-sneaky",
		"aws_secret_access_key":    "secret-sneaky",
		"bedrock_base_url":         "https://sneaky.example.com",
		"github_poll_interval":     "5m0s",
		"jira_poll_interval":       "5m0s",
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("settings patch status=%d body=%s, want 400", rec.Code, rec.Body.String())
	}
	for _, k := range integrations.BedrockKeys() {
		if got := mustSecret(t, s, k); got != "" {
			t.Errorf("the settings PATCH stored %s = %q (back door)", k, got)
		}
	}
	if view := getOrgBedrockView(t, s); view.Has {
		t.Error("has_bedrock_credentials=true after a settings PATCH (back door)")
	}
}
