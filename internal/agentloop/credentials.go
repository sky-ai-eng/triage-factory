package agentloop

import (
	"context"
	"fmt"
	"strings"

	"github.com/maximhq/bifrost/core/schemas"

	"github.com/sky-ai-eng/triage-factory/internal/inference"
)

// ProviderCredentialsFromEnv maps the resolved LLM env map — the shape the
// sealed claim bundle carries and the SDK path hands to its subprocess —
// onto inference.ProviderCredentials.
//
// This is the executor-side adapter the inference package deliberately does
// not own: that package takes credentials as plain inputs and never resolves
// or unseals anything, so the translation from "what the control plane
// sealed for this claim" to "what bifrost needs" lives here, next to the
// only consumer.
//
// The env map is the same vocabulary Claude Code documents, and the three
// modes are mutually exclusive in the order the resolver produces them:
//
//   - ANTHROPIC_API_KEY → Anthropic direct (optionally through a gateway
//     base URL, optionally with a proxy bearer).
//   - AWS_BEARER_TOKEN_BEDROCK → Bedrock with a single bearer token.
//   - AWS_ACCESS_KEY_ID + AWS_SECRET_ACCESS_KEY (+ session token) → Bedrock
//     with the SigV4 triple, which is what an STS AssumeRole mint produces
//     and why this resolves per call rather than per engagement.
//
// models is the key's whitelist and must name every model the caller will
// request: bifrost reads an empty whitelist as "no models", not "all", so an
// empty one surfaces at request time as a confusing no-key-for-model error.
func ProviderCredentialsFromEnv(env map[string]string, models []string) (inference.ProviderCredentials, error) {
	if len(models) == 0 {
		return inference.ProviderCredentials{}, fmt.Errorf("agentloop: provider credentials need a non-empty model whitelist")
	}
	get := func(k string) string { return strings.TrimSpace(env[k]) }

	if key := get("ANTHROPIC_API_KEY"); key != "" {
		creds := inference.ProviderCredentials{
			Provider: inference.ProviderAnthropic,
			APIKey:   key,
			BaseURL:  strings.TrimRight(get("ANTHROPIC_BASE_URL"), "/"),
			Models:   models,
		}
		if token := get("ANTHROPIC_AUTH_TOKEN"); token != "" {
			// A gateway in front of Anthropic authenticates the caller
			// separately from the upstream key; it rides as a header rather
			// than replacing the key.
			creds.ExtraHeaders = map[string]string{"Authorization": "Bearer " + token}
		}
		return creds, nil
	}

	region := get("AWS_REGION")
	baseURL := strings.TrimRight(get("ANTHROPIC_BEDROCK_BASE_URL"), "/")

	if bearer := get("AWS_BEARER_TOKEN_BEDROCK"); bearer != "" {
		return inference.ProviderCredentials{
			Provider: inference.ProviderBedrock,
			APIKey:   bearer,
			BaseURL:  baseURL,
			Models:   models,
			Bedrock:  &inference.BedrockCredentials{Region: orDefaultRegion(region)},
		}, nil
	}

	access, secret := get("AWS_ACCESS_KEY_ID"), get("AWS_SECRET_ACCESS_KEY")
	if access != "" && secret != "" {
		return inference.ProviderCredentials{
			Provider: inference.ProviderBedrock,
			BaseURL:  baseURL,
			Models:   models,
			Bedrock: &inference.BedrockCredentials{
				Region:       orDefaultRegion(region),
				AccessKey:    access,
				SecretKey:    secret,
				SessionToken: get("AWS_SESSION_TOKEN"),
			},
		}, nil
	}

	return inference.ProviderCredentials{}, ErrNoCredentials
}

// orDefaultRegion supplies the region the rest of the stack already defaults
// to when an org configured none. Bedrock cannot construct an endpoint
// without one, so an empty region is a hard failure inside bifrost rather
// than a soft degradation — better to use the same default the proxy path
// uses than to fail differently on the native path.
func orDefaultRegion(region string) string {
	if region != "" {
		return region
	}
	return "us-east-1"
}

// EnvCredentials is a Credentials implementation over a per-call env-map
// resolver. It rebuilds the account and client on every call, which is what
// makes an expiring STS triple safe: nothing is cached across calls that
// could outlive the credential that minted it.
//
// Cost of that choice is one bifrost Init per provider call. That is real
// but small next to the call itself, and the alternative — caching a client
// keyed on the credential — would reintroduce exactly the expiry class this
// design exists to avoid.
type EnvCredentials struct {
	// Resolve returns the LLM env map for this call. Called per call.
	Resolve func(ctx context.Context) (map[string]string, error)
	// Models is the whitelist stamped on the key.
	Models []string
	// NewClient builds the client from an account. nil uses inference.New.
	NewClient func(schemas.Account) (Provider, func(), error)
}

// ForCall satisfies Credentials.
func (c *EnvCredentials) ForCall(ctx context.Context) (schemas.ModelProvider, Provider, func(), error) {
	if c == nil || c.Resolve == nil {
		return "", nil, nil, ErrNoCredentials
	}
	env, err := c.Resolve(ctx)
	if err != nil {
		return "", nil, nil, err
	}
	creds, err := ProviderCredentialsFromEnv(env, c.Models)
	if err != nil {
		return "", nil, nil, err
	}
	account, err := inference.NewAccount(creds)
	if err != nil {
		return "", nil, nil, err
	}
	newClient := c.NewClient
	if newClient == nil {
		newClient = defaultNewClient
	}
	client, release, err := newClient(account)
	if err != nil {
		return "", nil, nil, err
	}
	return creds.Provider, client, release, nil
}

func defaultNewClient(account schemas.Account) (Provider, func(), error) {
	client, err := inference.New(account)
	if err != nil {
		return nil, nil, err
	}
	return client, client.Close, nil
}
