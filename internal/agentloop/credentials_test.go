package agentloop

import (
	"errors"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/inference"
)

func TestProviderCredentialsFromEnv(t *testing.T) {
	models := []string{"claude-sonnet-4-5"}

	t.Run("anthropic direct", func(t *testing.T) {
		got, err := ProviderCredentialsFromEnv(map[string]string{
			"ANTHROPIC_API_KEY":  "sk-ant-x",
			"ANTHROPIC_BASE_URL": "https://gateway.example/",
		}, models)
		if err != nil {
			t.Fatal(err)
		}
		if got.Provider != inference.ProviderAnthropic || got.APIKey != "sk-ant-x" {
			t.Fatalf("got %+v", got)
		}
		if got.BaseURL != "https://gateway.example" {
			t.Errorf("base URL must have its trailing slash trimmed: %q", got.BaseURL)
		}
		if got.Bedrock != nil {
			t.Error("a direct Anthropic key must not carry Bedrock config")
		}
	})

	t.Run("a gateway bearer rides as a header, not as the key", func(t *testing.T) {
		got, err := ProviderCredentialsFromEnv(map[string]string{
			"ANTHROPIC_API_KEY":    "sk-ant-x",
			"ANTHROPIC_AUTH_TOKEN": "gw-token",
		}, models)
		if err != nil {
			t.Fatal(err)
		}
		if got.APIKey != "sk-ant-x" {
			t.Errorf("the upstream key must survive: %q", got.APIKey)
		}
		if got.ExtraHeaders["Authorization"] != "Bearer gw-token" {
			t.Errorf("gateway bearer = %q", got.ExtraHeaders["Authorization"])
		}
	})

	t.Run("bedrock bearer", func(t *testing.T) {
		got, err := ProviderCredentialsFromEnv(map[string]string{
			"AWS_BEARER_TOKEN_BEDROCK":   "bedrock-bearer",
			"CLAUDE_CODE_USE_BEDROCK":    "1",
			"AWS_REGION":                 "eu-west-1",
			"ANTHROPIC_BEDROCK_BASE_URL": "http://10.0.0.1:9000/",
		}, models)
		if err != nil {
			t.Fatal(err)
		}
		if got.Provider != inference.ProviderBedrock || got.APIKey != "bedrock-bearer" {
			t.Fatalf("got %+v", got)
		}
		if got.Bedrock == nil || got.Bedrock.Region != "eu-west-1" {
			t.Fatalf("bedrock config = %+v", got.Bedrock)
		}
		if got.BaseURL != "http://10.0.0.1:9000" {
			t.Errorf("base URL = %q", got.BaseURL)
		}
	})

	t.Run("bedrock sigv4 triple", func(t *testing.T) {
		got, err := ProviderCredentialsFromEnv(map[string]string{
			"AWS_ACCESS_KEY_ID":     "AKIA",
			"AWS_SECRET_ACCESS_KEY": "secret",
			"AWS_SESSION_TOKEN":     "session",
		}, models)
		if err != nil {
			t.Fatal(err)
		}
		if got.Provider != inference.ProviderBedrock {
			t.Fatalf("provider = %s", got.Provider)
		}
		if got.Bedrock.AccessKey != "AKIA" || got.Bedrock.SecretKey != "secret" || got.Bedrock.SessionToken != "session" {
			t.Fatalf("bedrock config = %+v", got.Bedrock)
		}
		if got.Bedrock.Region != "us-east-1" {
			t.Errorf("an unset region must fall back to the same default the proxy path uses, got %q", got.Bedrock.Region)
		}
	})

	t.Run("a half-configured triple is not credentials", func(t *testing.T) {
		_, err := ProviderCredentialsFromEnv(map[string]string{"AWS_ACCESS_KEY_ID": "AKIA"}, models)
		if !errors.Is(err, ErrNoCredentials) {
			t.Fatalf("err = %v, want ErrNoCredentials", err)
		}
	})

	t.Run("no credentials at all", func(t *testing.T) {
		_, err := ProviderCredentialsFromEnv(map[string]string{}, models)
		if !errors.Is(err, ErrNoCredentials) {
			t.Fatalf("err = %v, want ErrNoCredentials", err)
		}
	})

	t.Run("every credential opts into dialing a private address", func(t *testing.T) {
		// A run reaches its own credential sidecar over the private veth IP the
		// orchestrator minted for it. bifrost refuses RFC 1918 by default as an
		// SSRF guard, and exempts loopback — so without this the delegate path
		// fails before a packet leaves the process, while every loopback test
		// passes. The refusal protects against an attacker-influenced base URL;
		// nothing in this map is one.
		for name, env := range map[string]map[string]string{
			"anthropic":      {"ANTHROPIC_API_KEY": "k"},
			"bedrock bearer": {"AWS_BEARER_TOKEN_BEDROCK": "b"},
			"bedrock sigv4":  {"AWS_ACCESS_KEY_ID": "a", "AWS_SECRET_ACCESS_KEY": "s"},
		} {
			creds, err := ProviderCredentialsFromEnv(env, []string{"claude-sonnet-5"})
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			if !creds.AllowPrivateNetwork {
				t.Errorf("%s: must opt into a private-IP endpoint", name)
			}
		}
	})

	t.Run("an empty whitelist is refused up front", func(t *testing.T) {
		// bifrost reads an empty whitelist as "no models", which surfaces at
		// request time as a confusing no-key-for-model error. Catch it here.
		if _, err := ProviderCredentialsFromEnv(map[string]string{"ANTHROPIC_API_KEY": "k"}, nil); err == nil {
			t.Fatal("an empty model whitelist must be refused")
		}
	})
}

func TestIsTransient(t *testing.T) {
	transient := []string{
		"inference: provider error: 429 rate limit",
		"inference: provider error: 503 service unavailable",
		"dial tcp: i/o timeout",
		"connection reset by peer",
		"overloaded_error",
	}
	for _, msg := range transient {
		if !isTransient(errors.New(msg)) {
			t.Errorf("%q must be retried", msg)
		}
	}
	permanent := []string{
		"inference: provider error: 401 invalid api key",
		"inference: provider error: model not found",
		"inference: request has no model",
	}
	for _, msg := range permanent {
		if isTransient(errors.New(msg)) {
			t.Errorf("%q must NOT burn the retry budget", msg)
		}
	}
}

// TestIsTransient_BifrostTransportFailure pins the coupling between this
// classifier and how internal/inference renders a provider error. Every
// transport failure in every bifrost provider carries the same fixed message,
// which matches no marker here — so a network blip reads as permanent and
// ends the engagement on the first attempt unless the wrapped cause and the
// status code travel with it. That rendering is the contract; this is the
// half of it that has to hold for a retryable failure to be retried.
func TestIsTransient_BifrostTransportFailure(t *testing.T) {
	rendered := "inference: provider error: failed to execute HTTP request to provider API: " +
		"dial tcp 10.42.7.1:41231: connect: connection refused (HTTP 502) " +
		"[provider_connection_failed] [endpoint: http://10.42.7.1:41231]"
	if !isTransient(errors.New(rendered)) {
		t.Fatalf("a dial failure must be retried: %q", rendered)
	}
	bare := "inference: provider error: failed to execute HTTP request to provider API"
	if isTransient(errors.New(bare)) {
		t.Fatal("this test's premise is gone: the bare message now matches a marker")
	}
}
