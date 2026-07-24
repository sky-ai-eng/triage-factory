package agentproc

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// fakeSecrets backs the resolver tests with an in-memory map. Returns
// ("", nil) for missing keys — matches the real SecretStore.Get
// contract.
//
// errOnKey lets a single key fail mid-resolution while the rest of
// the bag stays readable. Used by the optional-field error-
// propagation tests to simulate a transient backend hiccup that
// the primary key read survives but a subsequent optional read
// trips.
type fakeSecrets struct {
	values    map[string]map[string]string // orgID → key → value
	err       error
	errOnKey  string
	errForKey error
}

func (f *fakeSecrets) Get(_ context.Context, orgID, key string) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	if f.errOnKey != "" && key == f.errOnKey {
		return "", f.errForKey
	}
	return f.values[orgID][key], nil
}

func newFakeSecrets(orgID string, kv map[string]string) *fakeSecrets {
	return &fakeSecrets{values: map[string]map[string]string{orgID: kv}}
}

func TestResolveCredentials_AnthropicConfigured(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	const orgID = "11111111-1111-1111-1111-111111111111"
	secrets := newFakeSecrets(orgID, map[string]string{
		"anthropic_api_key": "sk-ant-org1",
	})

	env, err := resolveCredentials(context.Background(), secrets, orgID, nil)
	if err != nil {
		t.Fatalf("resolveCredentials: %v", err)
	}
	if got := env["ANTHROPIC_API_KEY"]; got != "sk-ant-org1" {
		t.Errorf("ANTHROPIC_API_KEY = %q, want sk-ant-org1", got)
	}
	// Bedrock vars must NOT appear when Anthropic is configured —
	// mutually exclusive code path.
	for _, k := range []string{"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "CLAUDE_CODE_USE_BEDROCK"} {
		if _, ok := env[k]; ok {
			t.Errorf("env[%s] set when Anthropic configured; should be Anthropic-only", k)
		}
	}
}

func TestResolveCredentials_BedrockConfigured(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	const orgID = "22222222-2222-2222-2222-222222222222"
	secrets := newFakeSecrets(orgID, map[string]string{
		"aws_access_key_id":     "AKIA-test",
		"aws_secret_access_key": "secret-test",
		"aws_region":            "us-west-2",
	})

	env, err := resolveCredentials(context.Background(), secrets, orgID, nil)
	if err != nil {
		t.Fatalf("resolveCredentials: %v", err)
	}
	if env["AWS_ACCESS_KEY_ID"] != "AKIA-test" {
		t.Errorf("AWS_ACCESS_KEY_ID = %q, want AKIA-test", env["AWS_ACCESS_KEY_ID"])
	}
	if env["AWS_SECRET_ACCESS_KEY"] != "secret-test" {
		t.Errorf("AWS_SECRET_ACCESS_KEY = %q, want secret-test", env["AWS_SECRET_ACCESS_KEY"])
	}
	if env["AWS_REGION"] != "us-west-2" {
		t.Errorf("AWS_REGION = %q, want us-west-2", env["AWS_REGION"])
	}
	if env["CLAUDE_CODE_USE_BEDROCK"] != "1" {
		t.Errorf("CLAUDE_CODE_USE_BEDROCK = %q, want 1 (auto-flagged when AWS triple set)", env["CLAUDE_CODE_USE_BEDROCK"])
	}
	if _, ok := env["ANTHROPIC_API_KEY"]; ok {
		t.Errorf("ANTHROPIC_API_KEY set when only Bedrock configured")
	}
}

// TestResolveCredentials_AnthropicWinsOverBedrock pins resolver
// precedence: when both paths are populated (malformed config that
// slipped past the admin-UI exclusivity gate), Anthropic wins. Picks
// one rather than erroring so a half-configured org can still run.
func TestResolveCredentials_AnthropicWinsOverBedrock(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	const orgID = "33333333-3333-3333-3333-333333333333"
	secrets := newFakeSecrets(orgID, map[string]string{
		"anthropic_api_key":        "sk-ant-both",
		"aws_bearer_token_bedrock": "bedrock-bearer-both",
		"aws_access_key_id":        "AKIA-both",
		"aws_secret_access_key":    "secret-both",
	})

	env, err := resolveCredentials(context.Background(), secrets, orgID, nil)
	if err != nil {
		t.Fatalf("resolveCredentials: %v", err)
	}
	if env["ANTHROPIC_API_KEY"] != "sk-ant-both" {
		t.Errorf("ANTHROPIC_API_KEY = %q, want sk-ant-both", env["ANTHROPIC_API_KEY"])
	}
	for _, k := range []string{"AWS_ACCESS_KEY_ID", "AWS_BEARER_TOKEN_BEDROCK", "CLAUDE_CODE_USE_BEDROCK"} {
		if _, ok := env[k]; ok {
			t.Errorf("env[%s] set when Anthropic configured; Anthropic should win exclusively", k)
		}
	}
}

// TestResolveCredentials_BedrockAPIKeyPath covers the Bedrock API
// key auth path (Option E in the Claude Code docs) — a single
// bearer token instead of the AWS access-key triple. The SDK reads
// AWS_BEARER_TOKEN_BEDROCK; CLAUDE_CODE_USE_BEDROCK=1 still gates
// the routing.
func TestResolveCredentials_BedrockAPIKeyPath(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	const orgID = "77777777-7777-7777-7777-777777777777"
	secrets := newFakeSecrets(orgID, map[string]string{
		"aws_bearer_token_bedrock": "bdrk-test-bearer",
		"aws_region":               "us-east-1",
		"bedrock_model_id":         "us.anthropic.claude-sonnet-4-6",
	})

	env, err := resolveCredentials(context.Background(), secrets, orgID, nil)
	if err != nil {
		t.Fatalf("resolveCredentials: %v", err)
	}
	if env["AWS_BEARER_TOKEN_BEDROCK"] != "bdrk-test-bearer" {
		t.Errorf("AWS_BEARER_TOKEN_BEDROCK = %q, want bdrk-test-bearer", env["AWS_BEARER_TOKEN_BEDROCK"])
	}
	if env["CLAUDE_CODE_USE_BEDROCK"] != "1" {
		t.Errorf("CLAUDE_CODE_USE_BEDROCK = %q, want 1", env["CLAUDE_CODE_USE_BEDROCK"])
	}
	if env["AWS_REGION"] != "us-east-1" {
		t.Errorf("AWS_REGION = %q, want us-east-1", env["AWS_REGION"])
	}
	if env["ANTHROPIC_MODEL"] != "us.anthropic.claude-sonnet-4-6" {
		t.Errorf("ANTHROPIC_MODEL = %q, want us.anthropic.claude-sonnet-4-6", env["ANTHROPIC_MODEL"])
	}
	// AWS triple must NOT appear — the bearer path is mutually
	// exclusive with the SigV4 path within the resolver.
	for _, k := range []string{"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN"} {
		if _, ok := env[k]; ok {
			t.Errorf("env[%s] set on Bedrock-bearer path; AWS triple should be unset", k)
		}
	}
}

// TestResolveCredentials_BedrockBearerWinsOverTriple — when both
// Bedrock bearer AND AWS triple are configured, bearer wins. Same
// rationale as Anthropic-over-Bedrock: pick one rather than error
// so a half-misconfigured org can still run.
func TestResolveCredentials_BedrockBearerWinsOverTriple(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	const orgID = "88888888-8888-8888-8888-888888888888"
	secrets := newFakeSecrets(orgID, map[string]string{
		"aws_bearer_token_bedrock": "bearer-wins",
		"aws_access_key_id":        "AKIA-loses",
		"aws_secret_access_key":    "secret-loses",
	})

	env, err := resolveCredentials(context.Background(), secrets, orgID, nil)
	if err != nil {
		t.Fatalf("resolveCredentials: %v", err)
	}
	if env["AWS_BEARER_TOKEN_BEDROCK"] != "bearer-wins" {
		t.Errorf("AWS_BEARER_TOKEN_BEDROCK = %q, want bearer-wins", env["AWS_BEARER_TOKEN_BEDROCK"])
	}
	if _, ok := env["AWS_ACCESS_KEY_ID"]; ok {
		t.Errorf("AWS_ACCESS_KEY_ID set; bearer path should win")
	}
}

// TestResolveCredentials_BedrockEndpointOverride pins the
// bedrock_base_url secret surfacing as ANTHROPIC_BEDROCK_BASE_URL on
// BOTH Bedrock auth paths. This is how orgs on a VPC interface
// endpoint (PrivateLink), GovCloud, or the China partition reach
// Bedrock — those hostnames don't follow the public regional formula,
// so the endpoint must be explicit config. In local direct-spawn mode
// the env var routes the SDK straight at the endpoint; in multi mode
// proxyConfigFromCreds consumes it as the proxy upstream.
func TestResolveCredentials_BedrockEndpointOverride(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	cases := map[string]map[string]string{
		"triple": {
			"aws_access_key_id":     "AKIA-test",
			"aws_secret_access_key": "secret-test",
			"aws_region":            "us-gov-west-1",
			"bedrock_base_url":      "https://vpce-0abc123-xyz.bedrock-runtime.us-gov-west-1.vpce.amazonaws.com",
		},
		"bearer": {
			"aws_bearer_token_bedrock": "bdrk-test",
			"bedrock_base_url":         "https://vpce-0abc123-xyz.bedrock-runtime.us-gov-west-1.vpce.amazonaws.com",
		},
	}
	for name, kv := range cases {
		t.Run(name, func(t *testing.T) {
			const orgID = "99999999-9999-9999-9999-999999999999"
			env, err := resolveCredentials(context.Background(), newFakeSecrets(orgID, kv), orgID, nil)
			if err != nil {
				t.Fatalf("resolveCredentials: %v", err)
			}
			if got := env["ANTHROPIC_BEDROCK_BASE_URL"]; got != kv["bedrock_base_url"] {
				t.Errorf("ANTHROPIC_BEDROCK_BASE_URL = %q, want the configured override %q", got, kv["bedrock_base_url"])
			}
		})
	}

	// Unset override → env var absent (SDK / proxy fall back to the
	// regional formula).
	const orgID = "99999999-9999-9999-9999-999999999999"
	env, err := resolveCredentials(context.Background(), newFakeSecrets(orgID, map[string]string{
		"aws_access_key_id":     "AKIA-test",
		"aws_secret_access_key": "secret-test",
	}), orgID, nil)
	if err != nil {
		t.Fatalf("resolveCredentials (no override): %v", err)
	}
	if got, ok := env["ANTHROPIC_BEDROCK_BASE_URL"]; ok {
		t.Errorf("ANTHROPIC_BEDROCK_BASE_URL = %q with no override configured; want absent", got)
	}
}

func TestResolveCredentials_PartialBedrockIsNotConfigured(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	const orgID = "44444444-4444-4444-4444-444444444444"
	// Access key set, secret key missing — malformed config. Treat
	// as not-configured rather than half-injecting (which would
	// surface as an AWS-SDK error inside the Node subprocess, much
	// harder to debug than a typed Go error at the call site).
	secrets := newFakeSecrets(orgID, map[string]string{
		"aws_access_key_id": "AKIA-partial",
	})

	_, err := resolveCredentials(context.Background(), secrets, orgID, nil)
	if !errors.Is(err, ErrNoCredentialsConfigured) {
		t.Fatalf("err = %v, want ErrNoCredentialsConfigured wrap", err)
	}
}

func TestResolveCredentials_EmptyOrgIDLocalMode(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)

	env, err := resolveCredentials(context.Background(), nil, "", nil)
	if err != nil {
		t.Fatalf("resolveCredentials(local, empty): %v", err)
	}
	if len(env) != 0 {
		t.Errorf("env = %v, want empty (subscription fallback)", env)
	}
}

func TestResolveCredentials_EmptyOrgIDMultiMode(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)

	_, err := resolveCredentials(context.Background(), nil, "", nil)
	if !errors.Is(err, ErrNoCredentialsConfigured) {
		t.Fatalf("err = %v, want ErrNoCredentialsConfigured wrap (multi mode refuses empty orgID)", err)
	}
}

func TestResolveCredentials_MultiModeOrgWithNoKey(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	const orgID = "55555555-5555-5555-5555-555555555555"
	secrets := newFakeSecrets(orgID, map[string]string{}) // empty vault row

	_, err := resolveCredentials(context.Background(), secrets, orgID, nil)
	if !errors.Is(err, ErrNoCredentialsConfigured) {
		t.Fatalf("err = %v, want ErrNoCredentialsConfigured wrap", err)
	}
}

// TestResolveCredentials_LocalModeOrgWithKey pins the local-mode +
// per-org key path: the resolver returns the key so mergeEnv strips
// credential vars from os.Environ and uses the keychain value. This
// is the seam that lets the future per-org credential UI work
// identically in local-as-N=1 and multi mode.
func TestResolveCredentials_LocalModeOrgWithKey(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	secrets := newFakeSecrets(runmode.LocalDefaultOrgID, map[string]string{
		"anthropic_api_key": "sk-ant-local-configured",
	})

	env, err := resolveCredentials(context.Background(), secrets, runmode.LocalDefaultOrgID, nil)
	if err != nil {
		t.Fatalf("resolveCredentials: %v", err)
	}
	if env["ANTHROPIC_API_KEY"] != "sk-ant-local-configured" {
		t.Errorf("ANTHROPIC_API_KEY = %q, want sk-ant-local-configured", env["ANTHROPIC_API_KEY"])
	}
}

func TestResolveCredentials_SecretReadErrorPropagates(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	const orgID = "66666666-6666-6666-6666-666666666666"
	want := errors.New("vault unavailable")
	secrets := &fakeSecrets{err: want}

	_, err := resolveCredentials(context.Background(), secrets, orgID, nil)
	if err == nil {
		t.Fatal("err = nil, want propagated secret-read failure")
	}
	if !errors.Is(err, want) {
		t.Errorf("err = %v, want errors.Is(_, want)", err)
	}
	if errors.Is(err, ErrNoCredentialsConfigured) {
		t.Errorf("err mis-classified as ErrNoCredentialsConfigured; want raw vault failure (caller may retry)")
	}
}

// TestMergeEnv_StripsParentCredsWhenResolved is the load-bearing
// parent-env-leak guard. A misconfigured operator-level
// ANTHROPIC_API_KEY at the systemd-unit level would otherwise win
// over the resolver's per-org key via last-entry-wins semantics on
// some platforms; this test pins that the parent-env credential
// keys are filtered out before the resolved creds are appended.
func TestMergeEnv_StripsParentCredsWhenResolved(t *testing.T) {
	parent := []string{
		"PATH=/usr/bin:/bin",
		"HOME=/home/test",
		"ANTHROPIC_API_KEY=parent-leak",
		"AWS_ACCESS_KEY_ID=parent-aws-leak",
		"AWS_PROFILE=operator-account",          // newly added to the leak guard
		"CLAUDE_CODE_USE_VERTEX=1",              // alternate-provider toggle
		"ANTHROPIC_BEDROCK_BASE_URL=https://op", // endpoint hijack
		"NPM_CONFIG_CACHE=/cache",
	}
	extra := []string{"TRIAGE_FACTORY_CONVERSATION_ID=run-123"}
	creds := map[string]string{"ANTHROPIC_API_KEY": "sk-ant-org-resolved"}

	merged := mergeEnv(parent, extra, creds)

	for _, leak := range []string{
		"ANTHROPIC_API_KEY=parent-leak",
		"AWS_ACCESS_KEY_ID=parent-aws-leak",
		"AWS_PROFILE=operator-account",
		"CLAUDE_CODE_USE_VERTEX=1",
		"ANTHROPIC_BEDROCK_BASE_URL=https://op",
	} {
		if containsKey(merged, leak) {
			t.Errorf("parent leak survived merge: %q — leak guard broken", leak)
		}
	}
	if !containsKey(merged, "ANTHROPIC_API_KEY=sk-ant-org-resolved") {
		t.Errorf("resolved ANTHROPIC_API_KEY missing from merged env: %v", merged)
	}
	// Non-credential parent vars must pass through (PATH, NPM cache,
	// etc — the SDK + Node toolchain depend on these).
	for _, want := range []string{"PATH=/usr/bin:/bin", "HOME=/home/test", "NPM_CONFIG_CACHE=/cache"} {
		if !containsKey(merged, want) {
			t.Errorf("merged env missing %q (non-credential parent var should pass through): %v", want, merged)
		}
	}
	// ExtraEnv must pass through.
	if !containsKey(merged, "TRIAGE_FACTORY_CONVERSATION_ID=run-123") {
		t.Errorf("ExtraEnv var missing from merged env: %v", merged)
	}
}

// TestMergeEnv_PreservesParentEnvWhenNoResolvedCreds covers the
// local-mode subscription path: when the resolver returns an empty
// map (no per-org credentials configured), the parent's env passes
// through unchanged so existing single-user installs that rely on
// process-environment ANTHROPIC_API_KEY or the Claude Code
// subscription's own auth path continue to work.
func TestMergeEnv_PreservesParentEnvWhenNoResolvedCreds(t *testing.T) {
	parent := []string{
		"PATH=/usr/bin",
		"ANTHROPIC_API_KEY=user-shell-key",
	}
	merged := mergeEnv(parent, nil, map[string]string{})

	if !containsKey(merged, "ANTHROPIC_API_KEY=user-shell-key") {
		t.Errorf("parent ANTHROPIC_API_KEY stripped despite no resolver creds: %v", merged)
	}
}

func TestFilterEnv_HandlesValuesContainingEquals(t *testing.T) {
	// env values often contain '=' (PATH, structured tokens). Filter
	// must split on the *first* '=' only, never strip a var whose
	// value happens to start with a forbidden key.
	env := []string{
		"PATH=/bin:/usr/bin",
		"ANTHROPIC_API_KEY=secret",
		"DEBUG_HINT=value with=equals inside",
		"SOMETHING_LIKE_ANTHROPIC_API_KEY=not-actually-a-key",
	}
	out := filterEnv(env, []string{"ANTHROPIC_API_KEY"})

	if containsKey(out, "ANTHROPIC_API_KEY=secret") {
		t.Errorf("target key not removed")
	}
	for _, want := range []string{
		"PATH=/bin:/usr/bin",
		"DEBUG_HINT=value with=equals inside",
		"SOMETHING_LIKE_ANTHROPIC_API_KEY=not-actually-a-key",
	} {
		if !containsKey(out, want) {
			t.Errorf("non-matching entry stripped: missing %q in %v", want, out)
		}
	}
}

// containsKey reports whether env contains the exact line want
// (KEY=VALUE form). Used by the merge tests instead of substring
// search because env entries are exact-match strings.
func containsKey(env []string, want string) bool {
	for _, e := range env {
		if e == want {
			return true
		}
	}
	return false
}

// TestResolveCredentials_OptionalAnthropicFieldErrorsPropagate
// pins that a backend hiccup mid-resolution surfaces as a real
// error instead of being silently dropped. Without this, an
// admin-UI write that succeeded for the primary key but failed
// for the gateway base URL would resolve to "Anthropic configured,
// no gateway" — and the run would route directly to api.anthropic.com
// bypassing the org's required proxy, an exfil risk on top of the
// obvious misconfiguration signal lost.
func TestResolveCredentials_OptionalAnthropicFieldErrorsPropagate(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	const orgID = "99999999-9999-9999-9999-999999999999"
	secrets := &fakeSecrets{
		values: map[string]map[string]string{
			orgID: {"anthropic_api_key": "sk-ant-primary-ok"},
		},
		errOnKey:  "anthropic_base_url",
		errForKey: errors.New("vault transient"),
	}

	_, err := resolveCredentials(context.Background(), secrets, orgID, nil)
	if err == nil {
		t.Fatal("optional field error swallowed; resolver should propagate")
	}
	if !strings.Contains(err.Error(), "anthropic_base_url") {
		t.Errorf("err = %v; want it to name anthropic_base_url so debugging is obvious", err)
	}
}

// TestResolveCredentials_OptionalBedrockFieldErrorsPropagate is the
// Bedrock-bearer-path mirror of the Anthropic case above. Pins the
// fix for Copilot finding 3 on PR #213.
func TestResolveCredentials_OptionalBedrockFieldErrorsPropagate(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	const orgID = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	secrets := &fakeSecrets{
		values: map[string]map[string]string{
			orgID: {
				"aws_access_key_id":     "AKIA-ok",
				"aws_secret_access_key": "secret-ok",
			},
		},
		errOnKey:  "aws_session_token",
		errForKey: errors.New("vault transient"),
	}

	_, err := resolveCredentials(context.Background(), secrets, orgID, nil)
	if err == nil {
		t.Fatal("optional Bedrock field error swallowed; resolver should propagate")
	}
	if !strings.Contains(err.Error(), "aws_session_token") {
		t.Errorf("err = %v; want it to name aws_session_token", err)
	}
}

// TestCredentialEnvKeysWellFormed catches accidental duplication
// or casing drift in the leak-guard list — drift here weakens the
// multi-mode safety net silently. Doesn't enforce alphabetical
// order (the list is grouped by category for readability), just
// no-dupes + upper-case.
func TestCredentialEnvKeysWellFormed(t *testing.T) {
	seen := make(map[string]struct{}, len(credentialEnvKeys))
	for _, k := range credentialEnvKeys {
		if _, dup := seen[k]; dup {
			t.Errorf("duplicate entry in credentialEnvKeys: %q", k)
		}
		seen[k] = struct{}{}
	}
	// Don't enforce alphabetical, but flag obvious typos by
	// requiring upper-case (env vars are conventionally upper).
	for _, k := range credentialEnvKeys {
		if strings.ToUpper(k) != k {
			t.Errorf("credential env key %q is not upper-case", k)
		}
	}
}

// TestResolveCredentials_LLMResolverPreferredOverRawPath pins the TFAC-616
// seam: when RunOptions.LLMResolver is wired (brain-side / all-local), it
// owns resolution — the built-in raw-secret path is not consulted, so a
// role-mode org (no raw key) resolves via the minting resolver.
func TestResolveCredentials_LLMResolverPreferredOverRawPath(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	const orgID = "22222222-2222-2222-2222-222222222222"
	// A bag with NO anthropic/bedrock key — the raw path would error here.
	secrets := newFakeSecrets(orgID, map[string]string{"aws_role_arn": "arn:aws:iam::1:role/r"})
	resolver := func(_ context.Context, gotOrg string) (map[string]string, error) {
		if gotOrg != orgID {
			t.Errorf("resolver got org %q, want %q", gotOrg, orgID)
		}
		return map[string]string{"AWS_ACCESS_KEY_ID": "ASIA-minted", "CLAUDE_CODE_USE_BEDROCK": "1"}, nil
	}
	env, err := resolveCredentials(context.Background(), secrets, orgID, resolver)
	if err != nil {
		t.Fatalf("resolveCredentials with resolver: %v", err)
	}
	if env["AWS_ACCESS_KEY_ID"] != "ASIA-minted" {
		t.Errorf("resolver output not used: %v", env)
	}
}

// The executor never calls resolveCredentials (agentproc.Run launches into the
// prebuilt network and holds no credential), so the former "bundle wins over
// the resolver" test is gone with the branch it covered — the orchestrator can
// no longer read a credential from ctx at all (TFAC-631).
