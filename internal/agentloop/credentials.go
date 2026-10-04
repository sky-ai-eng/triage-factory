package agentloop

import (
	"context"

	"github.com/maximhq/bifrost/core/schemas"

	"github.com/sky-ai-eng/triage-factory/internal/inference"
)

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
	creds, err := inference.ProviderCredentialsFromEnv(env, c.Models)
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
	return creds.Provider, client, closeInBackground(release), nil
}

// closeInBackground is the release ForCall returns. Closing a client waits
// for every request it still has in flight, and after a cancelled call that
// can be a request to a provider that took it and then went silent, which the
// client keeps reading until its own request timeout, minutes after the
// cancel. Nothing else holds the client, so nothing needs the close to have
// finished, and after a call that ran to its end it has nothing in flight and
// closes at once.
func closeInBackground(release func()) func() {
	if release == nil {
		return nil
	}
	return func() { go release() }
}

func defaultNewClient(account schemas.Account) (Provider, func(), error) {
	client, err := inference.New(account)
	if err != nil {
		return nil, nil, err
	}
	return client, client.Close, nil
}
