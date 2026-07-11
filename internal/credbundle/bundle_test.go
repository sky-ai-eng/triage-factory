package credbundle

import (
	"context"
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

func TestContextRoundTrip(t *testing.T) {
	ctx := context.Background()
	if _, ok := FromContext(ctx); ok {
		t.Fatal("FromContext on a bare context returned ok=true")
	}
	b := &Bundle{BootEpoch: 1, LLM: map[string]string{"ANTHROPIC_API_KEY": "x"}}
	ctx = WithBundle(ctx, b)
	got, ok := FromContext(ctx)
	if !ok {
		t.Fatal("FromContext after WithBundle returned ok=false")
	}
	if got != b {
		t.Fatalf("FromContext returned a different bundle: %+v", got)
	}
}

func TestFromContextNilBundleIsNotOK(t *testing.T) {
	ctx := WithBundle(context.Background(), nil)
	if _, ok := FromContext(ctx); ok {
		t.Fatal("FromContext with a nil bundle stored returned ok=true")
	}
}
