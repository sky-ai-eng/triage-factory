package inference

import (
	"errors"
	"testing"
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
		if got.Provider != ProviderAnthropic || got.APIKey != "sk-ant-x" {
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
		if got.Provider != ProviderBedrock || got.APIKey != "bedrock-bearer" {
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
		if got.Provider != ProviderBedrock {
			t.Fatalf("provider = %s", got.Provider)
		}
		if got.Bedrock.AccessKey != "AKIA" || got.Bedrock.SecretKey != "secret" || got.Bedrock.SessionToken != "session" {
			t.Fatalf("bedrock config = %+v", got.Bedrock)
		}
		if got.Bedrock.Region != "us-east-1" {
			t.Errorf("an unset region must fall back to the same default the proxy path uses, got %q", got.Bedrock.Region)
		}
	})

	t.Run("anthropic wins when an org somehow has both", func(t *testing.T) {
		// The precedence is load-bearing beyond tidiness: the breaker keys and
		// the system jobs' request-model choice each mirror it, and all three
		// have to agree on which provider a mixed map means.
		got, err := ProviderCredentialsFromEnv(map[string]string{
			"ANTHROPIC_API_KEY":        "sk-ant-x",
			"AWS_BEARER_TOKEN_BEDROCK": "bedrock-bearer",
			"AWS_REGION":               "us-east-1",
		}, models)
		if err != nil {
			t.Fatal(err)
		}
		if got.Provider != ProviderAnthropic {
			t.Errorf("provider = %s, want the Anthropic key to win", got.Provider)
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

	t.Run("every branch produces an account bifrost accepts", func(t *testing.T) {
		// The mapping is only correct if what it produces validates: a Bedrock
		// entry needs its region, a non-Bedrock one needs its key, and which of
		// those applies is exactly what this function decides.
		for name, env := range map[string]map[string]string{
			"anthropic":      {"ANTHROPIC_API_KEY": "k"},
			"bedrock bearer": {"AWS_BEARER_TOKEN_BEDROCK": "b"},
			"bedrock sigv4":  {"AWS_ACCESS_KEY_ID": "a", "AWS_SECRET_ACCESS_KEY": "s"},
		} {
			creds, err := ProviderCredentialsFromEnv(env, []string{"claude-sonnet-5"})
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			if _, err := NewAccount(creds); err != nil {
				t.Errorf("%s: NewAccount rejected the mapped credentials: %v", name, err)
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
