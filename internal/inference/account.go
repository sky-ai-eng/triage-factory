package inference

import (
	"context"
	"fmt"

	"github.com/maximhq/bifrost/core/schemas"
)

// Provider identifiers for the surfaces TF drives. They alias bifrost's
// vocabulary so callers don't import schemas just to name a provider.
const (
	ProviderAnthropic = schemas.Anthropic
	ProviderBedrock   = schemas.Bedrock
)

// defaultConcurrency / defaultBufferSize size each provider's worker pool and
// request queue. bifrost's own defaults are 1000/5000 — far too many
// goroutines for a native loop that serializes calls per conversation — so the
// account sets modest values and lets CheckAndSetDefaults fill the rest
// (timeouts, retries) without touching them.
const (
	defaultConcurrency = 4
	defaultBufferSize  = 64
)

// ProviderCredentials is one provider's key material as plain inputs — already
// resolved by the caller (this package never seals or resolves credentials).
// Models is the per-key whitelist and MUST name every model the caller will
// request: bifrost treats an empty whitelist as "no models", not "all models",
// so an empty Models yields a confusing "no key for model" at request time.
// Use []string{"*"} only to deliberately allow every model.
type ProviderCredentials struct {
	Provider schemas.ModelProvider

	// APIKey is the Anthropic API key (or gateway bearer). Empty for a Bedrock
	// role/SigV4 config that authenticates through Bedrock instead.
	APIKey string
	// BaseURL overrides the provider endpoint (a customer gateway or a Bedrock
	// VPC endpoint). Optional.
	BaseURL string
	// ExtraHeaders are attached to every request to this provider (e.g. a
	// gateway Authorization bearer, a beta header override). Optional.
	ExtraHeaders map[string]string

	// Models is the key's model whitelist. Required — see the type doc.
	Models []string

	// Bedrock carries AWS auth for a Bedrock provider (static keys or an STS
	// AssumeRole config). Nil for a direct Anthropic key.
	Bedrock *BedrockCredentials

	// AllowPrivateNetwork lifts bifrost's refusal to dial an RFC 1918 address.
	//
	// That refusal is an SSRF guard, and it is aimed at a base URL an attacker
	// can influence — the classic "make the server fetch an internal service
	// for me". Off by default here for exactly that reason: a caller has to
	// state that its endpoint is not attacker-influenced.
	//
	// Link-local (169.254.0.0/16, fe80::/10) stays blocked either way, inside
	// bifrost, with no opt-out — so the cloud instance-metadata endpoint, which
	// is the prize this class of guard exists to protect, is out of reach
	// regardless of what this field says.
	AllowPrivateNetwork bool

	// Concurrency / BufferSize override the modest defaults for this provider's
	// worker pool and queue. Zero uses the defaults.
	Concurrency int
	BufferSize  int
}

// BedrockCredentials is the AWS auth for a Bedrock provider. Static access
// keys, a bearer, or an STS AssumeRole (RoleARN + optional ExternalID /
// RoleSessionName) are all accepted; Region is required. Only the config shape
// is validated here — a live Bedrock call is exercised by the env-gated smoke
// test.
type BedrockCredentials struct {
	Region          string
	AccessKey       string
	SecretKey       string
	SessionToken    string
	ARN             string
	RoleARN         string
	ExternalID      string
	RoleSessionName string
}

// providerEntry is one prepared provider: the keys the selector picks from and
// the config Init hands to prepareProvider.
type providerEntry struct {
	keys   []schemas.Key
	config *schemas.ProviderConfig
}

// account is the bifrost schemas.Account implementation over a fixed set of
// resolved provider credentials. It holds no dynamic resolution — the caller
// supplies everything up front, per the package's "credentials as inputs"
// contract.
type account struct {
	entries map[schemas.ModelProvider]providerEntry
}

// NewAccount builds a bifrost Account from resolved provider credentials. It
// validates each entry (a provider is required, a whitelist must be non-empty,
// a non-Bedrock provider needs an API key, a Bedrock provider needs a region)
// so a misconfiguration surfaces here rather than as an opaque request-time
// failure.
func NewAccount(creds ...ProviderCredentials) (schemas.Account, error) {
	if len(creds) == 0 {
		return nil, fmt.Errorf("inference: NewAccount requires at least one provider")
	}
	entries := make(map[schemas.ModelProvider]providerEntry, len(creds))
	for i, c := range creds {
		if c.Provider == "" {
			return nil, fmt.Errorf("inference: credentials[%d] has no provider", i)
		}
		if len(c.Models) == 0 {
			return nil, fmt.Errorf("inference: credentials[%d] (%s) has an empty model whitelist — bifrost reads empty as no models; list the model or use \"*\"", i, c.Provider)
		}
		// One entry per provider: a duplicate would otherwise silently clobber
		// the earlier key + config in the map. Reject it loudly so an accidental
		// dup is a build-time error, not a mystery at request time. (Weighted
		// multi-key per provider is a deliberate future API shape, not something
		// to infer from a repeated provider here.)
		if _, dup := entries[c.Provider]; dup {
			return nil, fmt.Errorf("inference: credentials[%d]: duplicate provider %s — supply exactly one entry per provider", i, c.Provider)
		}
		key, err := buildKey(c)
		if err != nil {
			return nil, fmt.Errorf("inference: credentials[%d] (%s): %w", i, c.Provider, err)
		}
		entries[c.Provider] = providerEntry{
			keys:   []schemas.Key{key},
			config: buildProviderConfig(c),
		}
	}
	return &account{entries: entries}, nil
}

// buildKey maps one credentials entry onto a bifrost Key, wiring the Bedrock
// sub-config when present.
func buildKey(c ProviderCredentials) (schemas.Key, error) {
	key := schemas.Key{
		ID:     "tf-" + string(c.Provider),
		Name:   string(c.Provider),
		Value:  plainSecret(c.APIKey),
		Models: schemas.WhiteList(c.Models),
		Weight: 1,
	}

	if c.Bedrock != nil {
		if c.Bedrock.Region == "" {
			return schemas.Key{}, fmt.Errorf("bedrock credentials require a region")
		}
		key.BedrockKeyConfig = &schemas.BedrockKeyConfig{
			AccessKey:       plainSecret(c.Bedrock.AccessKey),
			SecretKey:       plainSecret(c.Bedrock.SecretKey),
			SessionToken:    optionalSecret(c.Bedrock.SessionToken),
			Region:          optionalSecret(c.Bedrock.Region),
			ARN:             optionalSecret(c.Bedrock.ARN),
			RoleARN:         optionalSecret(c.Bedrock.RoleARN),
			ExternalID:      optionalSecret(c.Bedrock.ExternalID),
			RoleSessionName: optionalSecret(c.Bedrock.RoleSessionName),
		}
		return key, nil
	}

	if c.APIKey == "" {
		return schemas.Key{}, fmt.Errorf("a non-Bedrock provider requires an API key")
	}
	return key, nil
}

// buildProviderConfig sizes the worker pool modestly and lets bifrost fill the
// remaining network defaults (timeouts, retries) via CheckAndSetDefaults.
func buildProviderConfig(c ProviderCredentials) *schemas.ProviderConfig {
	concurrency := c.Concurrency
	if concurrency <= 0 {
		concurrency = defaultConcurrency
	}
	bufSize := c.BufferSize
	if bufSize <= 0 {
		bufSize = defaultBufferSize
	}
	cfg := &schemas.ProviderConfig{
		NetworkConfig: schemas.NetworkConfig{
			BaseURL:             c.BaseURL,
			ExtraHeaders:        c.ExtraHeaders,
			AllowPrivateNetwork: c.AllowPrivateNetwork,
		},
		ConcurrencyAndBufferSize: schemas.ConcurrencyAndBufferSize{
			Concurrency: concurrency,
			BufferSize:  bufSize,
		},
	}
	cfg.CheckAndSetDefaults()
	return cfg
}

func plainSecret(v string) schemas.SecretVar {
	return schemas.SecretVar{Val: v, SecretType: schemas.SecretTypePlainText}
}

func optionalSecret(v string) *schemas.SecretVar {
	if v == "" {
		return nil
	}
	s := plainSecret(v)
	return &s
}

// GetConfiguredProviders reports which providers the account can serve.
func (a *account) GetConfiguredProviders() ([]schemas.ModelProvider, error) {
	out := make([]schemas.ModelProvider, 0, len(a.entries))
	for p := range a.entries {
		out = append(out, p)
	}
	return out, nil
}

// GetKeysForProvider returns the keys bifrost's selector picks from for a
// provider. The context is unused — the credentials are already resolved.
func (a *account) GetKeysForProvider(_ context.Context, providerKey schemas.ModelProvider) ([]schemas.Key, error) {
	entry, ok := a.entries[providerKey]
	if !ok {
		return nil, fmt.Errorf("inference: no keys configured for provider %s", providerKey)
	}
	return entry.keys, nil
}

// GetConfigForProvider returns the prepared provider config.
func (a *account) GetConfigForProvider(providerKey schemas.ModelProvider) (*schemas.ProviderConfig, error) {
	entry, ok := a.entries[providerKey]
	if !ok {
		return nil, fmt.Errorf("inference: no config for provider %s", providerKey)
	}
	return entry.config, nil
}
