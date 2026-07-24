package inference

import (
	"context"
	"testing"

	"github.com/maximhq/bifrost/core/schemas"
)

func TestNewAccount_Validation(t *testing.T) {
	cases := map[string][]ProviderCredentials{
		"no providers":         {},
		"empty provider":       {{APIKey: "k", Models: []string{"m"}}},
		"empty whitelist":      {{Provider: ProviderAnthropic, APIKey: "k"}},
		"anthropic no api key": {{Provider: ProviderAnthropic, Models: []string{"m"}}},
		"bedrock no region":    {{Provider: ProviderBedrock, Models: []string{"m"}, Bedrock: &BedrockCredentials{}}},
	}
	for name, creds := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := NewAccount(creds...); err == nil {
				t.Fatalf("expected an error for %q", name)
			}
		})
	}
}

func TestNewAccount_AnthropicKeyMaterial(t *testing.T) {
	acct, err := NewAccount(ProviderCredentials{
		Provider: ProviderAnthropic,
		APIKey:   "sk-test-key",
		BaseURL:  "https://gw.example/anthropic",
		Models:   []string{"claude-sonnet-4-5", "claude-opus-4-1"},
	})
	if err != nil {
		t.Fatal(err)
	}

	providers, err := acct.GetConfiguredProviders()
	if err != nil || len(providers) != 1 || providers[0] != ProviderAnthropic {
		t.Fatalf("configured providers: %v (%v)", providers, err)
	}

	keys, err := acct.GetKeysForProvider(context.Background(), ProviderAnthropic)
	if err != nil || len(keys) != 1 {
		t.Fatalf("keys: %v (%v)", keys, err)
	}
	if got := keys[0].Value.GetValue(); got != "sk-test-key" {
		t.Fatalf("key value = %q, want sk-test-key", got)
	}
	// The whitelist must name the model exactly (empty would mean "no models").
	if !keys[0].Models.IsAllowed("claude-sonnet-4-5") {
		t.Fatal("whitelist must allow the listed model")
	}
	if keys[0].Models.IsAllowed("gpt-4o") {
		t.Fatal("whitelist must not allow an unlisted model")
	}

	cfg, err := acct.GetConfigForProvider(ProviderAnthropic)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.NetworkConfig.BaseURL != "https://gw.example/anthropic" {
		t.Fatalf("base url not carried: %q", cfg.NetworkConfig.BaseURL)
	}
	// The worker pool must be modest, not bifrost's 1000 default.
	if cfg.ConcurrencyAndBufferSize.Concurrency != defaultConcurrency {
		t.Fatalf("concurrency = %d, want %d", cfg.ConcurrencyAndBufferSize.Concurrency, defaultConcurrency)
	}
	// CheckAndSetDefaults must still have filled the network defaults.
	if cfg.NetworkConfig.DefaultRequestTimeoutInSeconds <= 0 {
		t.Fatal("network defaults were not filled")
	}
}

func TestNewAccount_BedrockSTSConfig(t *testing.T) {
	acct, err := NewAccount(ProviderCredentials{
		Provider: ProviderBedrock,
		Models:   []string{"anthropic.claude-sonnet-4-20250514-v1:0"},
		Bedrock: &BedrockCredentials{
			Region:          "us-east-1",
			RoleARN:         "arn:aws:iam::123456789012:role/tf-bedrock",
			RoleSessionName: "triage-factory",
			ExternalID:      "tf-ext",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	keys, err := acct.GetKeysForProvider(context.Background(), ProviderBedrock)
	if err != nil || len(keys) != 1 {
		t.Fatalf("keys: %v (%v)", keys, err)
	}
	bk := keys[0].BedrockKeyConfig
	if bk == nil {
		t.Fatal("bedrock key config not carried")
	}
	if bk.Region == nil || bk.Region.GetValue() != "us-east-1" {
		t.Fatalf("region not carried: %+v", bk.Region)
	}
	if bk.RoleARN == nil || bk.RoleARN.GetValue() != "arn:aws:iam::123456789012:role/tf-bedrock" {
		t.Fatalf("role arn not carried: %+v", bk.RoleARN)
	}
}

// TestNew_InitAcceptsAccounts exercises the embedded bifrost Init path offline
// for both an Anthropic key and a Bedrock STS config — the ticket's "Bedrock
// STS config accepted at Init" acceptance, minus the (separately env-gated)
// live call. Init prepares providers and starts workers; Close shuts them down.
func TestNew_InitAcceptsAccounts(t *testing.T) {
	acct, err := NewAccount(
		ProviderCredentials{Provider: ProviderAnthropic, APIKey: "sk-test", Models: []string{"claude-sonnet-5"}},
		ProviderCredentials{
			Provider: ProviderBedrock,
			Models:   []string{"anthropic.claude-sonnet-4-20250514-v1:0"},
			Bedrock:  &BedrockCredentials{Region: "us-east-1", RoleARN: "arn:aws:iam::1:role/r"},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	client, err := New(acct)
	if err != nil {
		t.Fatalf("New (bifrost Init) must accept the accounts: %v", err)
	}
	client.Close()
}

var _ schemas.Account = (*account)(nil)
