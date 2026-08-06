package inference

import (
	"errors"
	"fmt"
	"strings"
)

// ErrNoCredentials is returned when an env map names no provider at all —
// surfaced rather than papered over, since a provider call cannot fall back to
// anything.
var ErrNoCredentials = errors.New("inference: no LLM credentials available")

// ProviderCredentialsFromEnv maps a resolved LLM env map — the shape the sealed
// claim bundle carries, the shape the SDK path hands to its subprocess, and the
// shape the system jobs' own resolver returns — onto ProviderCredentials.
//
// It lives here because env-map→credentials is this package's own vocabulary:
// credentials arrive as plain inputs, and this is the translation from the one
// dialect every caller already speaks into them. What it deliberately does not
// do is resolve or unseal anything — the caller has already done that, and
// which resolver it used is none of this function's business.
//
// The env map is the same vocabulary Claude Code documents, and the three modes
// are mutually exclusive in the order the resolvers produce them:
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
// allowPrivateNetwork lifts bifrost's refusal to dial an RFC 1918 address for
// every credential this adapter produces.
//
// The refusal is an SSRF guard against a base URL an attacker can influence.
// Nothing in this env map is: it is either the address of a run's own
// credential sidecar — a private veth IP the orchestrator minted seconds
// earlier, on the far side of which the real key is attached — or an
// operator-configured gateway from the org's secrets. The agent, which is the
// one component deliberately fed hostile text, cannot reach either. So the
// guard blocks the architecture and stops nothing.
//
// It is scoped to this adapter rather than defaulted on in NewAccount, so the
// claim above stays next to the map it is a claim about. Link-local stays
// blocked inside bifrost regardless, which is the instance-metadata case that
// actually matters.
const allowPrivateNetwork = true

func ProviderCredentialsFromEnv(env map[string]string, models []string) (ProviderCredentials, error) {
	if len(models) == 0 {
		return ProviderCredentials{}, fmt.Errorf("inference: provider credentials need a non-empty model whitelist")
	}
	get := func(k string) string { return strings.TrimSpace(env[k]) }

	if key := get("ANTHROPIC_API_KEY"); key != "" {
		creds := ProviderCredentials{
			Provider:            ProviderAnthropic,
			APIKey:              key,
			BaseURL:             strings.TrimRight(get("ANTHROPIC_BASE_URL"), "/"),
			Models:              models,
			AllowPrivateNetwork: allowPrivateNetwork,
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
		return ProviderCredentials{
			Provider:            ProviderBedrock,
			APIKey:              bearer,
			BaseURL:             baseURL,
			Models:              models,
			AllowPrivateNetwork: allowPrivateNetwork,
			Bedrock:             &BedrockCredentials{Region: orDefaultRegion(region)},
		}, nil
	}

	access, secret := get("AWS_ACCESS_KEY_ID"), get("AWS_SECRET_ACCESS_KEY")
	if access != "" && secret != "" {
		return ProviderCredentials{
			Provider:            ProviderBedrock,
			BaseURL:             baseURL,
			Models:              models,
			AllowPrivateNetwork: allowPrivateNetwork,
			Bedrock: &BedrockCredentials{
				Region:       orDefaultRegion(region),
				AccessKey:    access,
				SecretKey:    secret,
				SessionToken: get("AWS_SESSION_TOKEN"),
			},
		}, nil
	}

	return ProviderCredentials{}, ErrNoCredentials
}

// orDefaultRegion supplies the region the rest of the stack already defaults
// to when an org configured none. Bedrock cannot construct an endpoint
// without one, so an empty region is a hard failure inside bifrost rather
// than a soft degradation — better to use the same default the proxy path
// uses than to fail differently here.
func orDefaultRegion(region string) string {
	if region != "" {
		return region
	}
	return "us-east-1"
}
