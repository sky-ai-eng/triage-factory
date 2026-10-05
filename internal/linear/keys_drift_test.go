package linear_test

// An external test package, because internal/integrations imports
// internal/linear (through internal/auth's ValidateLinear). That cycle is why
// the resolver keeps hand-copied key names; from out here both sides can be
// held together and compared.

import (
	"context"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/integrations"
	"github.com/sky-ai-eng/triage-factory/internal/linear"
)

// keyedSecrets answers GetSystem from a map keyed by the integrations names.
type keyedSecrets struct {
	db.SecretStore
	vals map[string]string
}

func (k keyedSecrets) GetSystem(_ context.Context, _ string, key string) (string, error) {
	return k.vals[key], nil
}

// stubOrgs is never read: the system credential does not consult org
// settings.
type stubOrgs struct{ db.OrgsStore }

// TestResolver_KeysMatchIntegrations stores the org credential under the
// names integrations writes and resolves it through the resolver, which reads
// its own copies. A rename on either side would make every org resolve as
// unconfigured with no compile error.
func TestResolver_KeysMatchIntegrations(t *testing.T) {
	secrets := keyedSecrets{vals: map[string]string{
		integrations.KeyLinearAuthMethod: string(linear.AuthMethodAPIKey),
		integrations.KeyLinearAPIKey:     "lin_api_org",
	}}
	cred, err := linear.NewResolver(secrets, stubOrgs{}).ResolveSystemCredential(context.Background(), "org-1")
	if err != nil {
		t.Fatalf("ResolveSystemCredential with integrations-keyed secrets: %v\n"+
			"linear.keyLinear{APIKey,AuthMethod} have drifted from integrations.KeyLinear*", err)
	}
	if cred.APIKey != "lin_api_org" {
		t.Errorf("api key = %q, want the one stored under integrations.KeyLinearAPIKey", cred.APIKey)
	}
}
