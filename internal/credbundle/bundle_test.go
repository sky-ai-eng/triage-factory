package credbundle

import (
	"testing"
)

func TestMarshalUnmarshalRoundTrip(t *testing.T) {
	b := &Bundle{
		BootEpoch: 3,
		LLM:       map[string]string{"ANTHROPIC_API_KEY": "sk-ant-test"},
		GitHub: &GitHubCreds{
			Mode:          "app",
			IdentityName:  "acme[bot]",
			IdentityEmail: "acme[bot]@users.noreply.github.com",
			RepoTokens: map[string]RepoToken{
				"acme/widgets": {Token: "ghs_abc"},
			},
		},
		Jira: &JiraCreds{URL: "https://acme.atlassian.net", AuthMethod: "cloud", Email: "bot@acme.com", APIToken: "tok"},
	}
	data, err := b.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	got, err := Unmarshal(data)
	if err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got.BootEpoch != b.BootEpoch || got.LLM["ANTHROPIC_API_KEY"] != "sk-ant-test" {
		t.Fatalf("round trip mismatch: %+v", got)
	}
	if got.GitHub == nil || got.GitHub.RepoTokens["acme/widgets"].Token != "ghs_abc" {
		t.Fatalf("github round trip mismatch: %+v", got.GitHub)
	}
	if got.Jira == nil || got.Jira.APIToken != "tok" {
		t.Fatalf("jira round trip mismatch: %+v", got.Jira)
	}
}

// TestLLMExpiry_OmittedWhenZero pins that a passthrough bundle (no role-mode
// expiry) serializes byte-for-byte as before the LLMExpiryUnix field
// existed — decision 7's "bearer/access_keys bundle contents unchanged" pin.
// A role-mode bundle carries the expiry, and LLMExpiry() round-trips it.
func TestLLMExpiry_OmittedWhenZero(t *testing.T) {
	passthrough := &Bundle{BootEpoch: 1, LLM: map[string]string{"AWS_ACCESS_KEY_ID": "AKIA"}}
	data, err := passthrough.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if got := string(data); got != `{"boot_epoch":1,"llm":{"AWS_ACCESS_KEY_ID":"AKIA"}}` {
		t.Errorf("passthrough bundle changed shape (llm_expiry_unix must be omitted):\n%s", got)
	}
	if !passthrough.LLMExpiry().IsZero() {
		t.Errorf("passthrough LLMExpiry should be zero")
	}

	role := &Bundle{BootEpoch: 1, LLM: map[string]string{"AWS_ACCESS_KEY_ID": "ASIA"}, LLMExpiryUnix: 1752300000}
	rt, err := role.Marshal()
	if err != nil {
		t.Fatalf("Marshal role: %v", err)
	}
	got, err := Unmarshal(rt)
	if err != nil {
		t.Fatalf("Unmarshal role: %v", err)
	}
	if got.LLMExpiry().Unix() != 1752300000 {
		t.Errorf("role LLMExpiry round trip = %v", got.LLMExpiry())
	}
}
