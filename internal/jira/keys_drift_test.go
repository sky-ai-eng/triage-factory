package jira_test

// External test package (jira_test) on purpose: it imports internal/integrations,
// which transitively imports internal/jira (integrations → auth → jira). That's
// the exact cycle that forces jira.keyJiraURL/keyJiraPAT to be hand-copied from
// integrations.KeyJiraURL/KeyJiraPAT instead of imported. An external test
// package sits outside that cycle, so it can hold both sides together and prove
// they still agree.

import (
	"context"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/integrations"
	"github.com/sky-ai-eng/triage-factory/internal/jira"
)

// keyedSecrets is a SecretStore stub whose GetSystem reads from a map keyed by
// the integrations.Key* constants — so a value stored under integrations' public
// keys is exactly what ForSystem looks up via its own (unexported,
// cycle-dodging) keyJiraURL/keyJiraPAT.
type keyedSecrets struct {
	db.SecretStore
	vals map[string]string
}

func (k keyedSecrets) GetSystem(_ context.Context, _ string, key string) (string, error) {
	return k.vals[key], nil
}

// stubOrgs satisfies the resolver's OrgsStore dependency; ForSystem never reads
// it (only ForUser does), so the embedded nil is never dereferenced.
type stubOrgs struct{ db.OrgsStore }

// TestForSystem_KeysMatchIntegrations closes the drift gap the import cycle
// creates: jira.keyJiraURL/keyJiraPAT are hand-copied from
// integrations.KeyJiraURL/KeyJiraPAT. A silent rename of either side would make
// ForSystem read under the wrong key and always return ErrNoJiraSystemCredential
// with no compile error — storing under the integrations keys and resolving
// through ForSystem proves the two stay in lockstep.
func TestForSystem_KeysMatchIntegrations(t *testing.T) {
	secrets := keyedSecrets{vals: map[string]string{
		integrations.KeyJiraURL: "https://jira.example.com",
		integrations.KeyJiraPAT: "tok",
	}}
	c, err := jira.NewResolver(secrets, stubOrgs{}).ForSystem(context.Background(), "org-1")
	if err != nil {
		t.Fatalf("ForSystem with integrations-keyed secrets: %v\n"+
			"jira.keyJiraURL/keyJiraPAT have drifted from integrations.KeyJiraURL/KeyJiraPAT", err)
	}
	if c == nil {
		t.Fatal("ForSystem returned a nil client")
	}
}
