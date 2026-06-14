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

// TestForSystem_CloudKeysMatchIntegrations is the Cloud sibling of the test
// above: it closes the same drift gap for the Cloud credential keys
// (jira.keyJiraEmail/keyJiraAPIToken/keyJiraAuthMethod hand-copied from
// integrations.KeyJira*). Storing the Cloud credential under the integrations
// keys + marker and resolving through ForSystem proves the resolver reads them
// under the same names integrations writes — a silent rename of either side
// would make ForSystem return ErrNoJiraSystemCredential with no compile error.
func TestForSystem_CloudKeysMatchIntegrations(t *testing.T) {
	secrets := keyedSecrets{vals: map[string]string{
		integrations.KeyJiraURL:        "https://acme.atlassian.net",
		integrations.KeyJiraEmail:      "bot@acme.com",
		integrations.KeyJiraAPIToken:   "tok",
		integrations.KeyJiraAuthMethod: string(jira.AuthMethodCloudAPIToken),
	}}
	c, err := jira.NewResolver(secrets, stubOrgs{}).ForSystem(context.Background(), "org-1")
	if err != nil {
		t.Fatalf("ForSystem with integrations-keyed cloud secrets: %v\n"+
			"jira.keyJira{Email,APIToken,AuthMethod} have drifted from integrations.KeyJira*", err)
	}
	if c == nil {
		t.Fatal("ForSystem returned a nil client")
	}
}

// TestCanonicalHostMatchesNormalizeJiraHost closes the second drift gap the
// import cycle creates: db.NormalizeJiraHost (which keys every
// user_jira_identities row) can't call jira.CanonicalHost (internal/jira
// imports internal/db, so the reverse would cycle), so it reimplements the
// trailing-slash + whitespace trim by hand. NormalizeJiraHost's doc reasons
// in prose that the two agree on CanonicalHost's success path; this pins it in
// code. A silent change to either trim would route a captured identity under a
// host the read side never looks up, with no compile error. This external test
// package sits outside the cycle, so it can hold both sides together.
func TestCanonicalHostMatchesNormalizeJiraHost(t *testing.T) {
	// Inputs CanonicalHost accepts (real http(s) origin): the normalized host
	// it returns must equal what NormalizeJiraHost keys the row under.
	valid := []string{
		"https://jira.example.com",
		"https://jira.example.com/",
		"https://jira.example.com///",
		"  https://jira.example.com  ",
		" https://site.atlassian.net/ ",
		"http://localhost:8080/",
	}
	for _, in := range valid {
		canon, ok := jira.CanonicalHost(in)
		if !ok {
			t.Errorf("CanonicalHost(%q) ok=false; expected a valid origin", in)
			continue
		}
		if norm := db.NormalizeJiraHost(in); canon != norm {
			t.Errorf("host key drift for %q: CanonicalHost=%q, NormalizeJiraHost=%q (writers and readers would disagree)", in, canon, norm)
		}
	}

	// Inputs CanonicalHost rejects (no real origin): the credential is never
	// stored, so there's no identity row to key — agreement is moot, but the
	// normalizer must not panic and returns its best-effort trim.
	invalid := []string{"", "   ", "/", "not a url"}
	for _, in := range invalid {
		if _, ok := jira.CanonicalHost(in); ok {
			t.Errorf("CanonicalHost(%q) ok=true; expected rejection", in)
		}
		_ = db.NormalizeJiraHost(in) // must not panic
	}
}
