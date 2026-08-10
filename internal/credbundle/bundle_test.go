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

// TestGitHubCredsCredential pins the audit-log name each tier reports. The
// bundle is the only thing on an executor that knows which one a run spends, so
// this mapping is what keeps that side's rows from all claiming the App.
func TestGitHubCredsCredential(t *testing.T) {
	for _, tc := range []struct {
		name string
		gh   *GitHubCreds
		want string
	}{
		{"app", &GitHubCreds{Mode: GitHubModeApp}, "github_app"},
		{"pat", &GitHubCreds{Mode: GitHubModePAT}, "github_pat"},
		// A run with no GitHub credential at all, and one whose mode never got
		// written, both read as the App — the value this column has always had.
		{"absent", nil, "github_app"},
		{"unset", &GitHubCreds{}, "github_app"},
	} {
		if got := tc.gh.Credential(); got != tc.want {
			t.Errorf("%s: Credential() = %q, want %q", tc.name, got, tc.want)
		}
	}
}
