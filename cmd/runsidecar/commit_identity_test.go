package runsidecar

import (
	"context"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/credbundle"
	"github.com/sky-ai-eng/triage-factory/internal/githooks"
	"github.com/sky-ai-eng/triage-factory/internal/sidecarproto"
)

// startProxiesWith relays bundle to a fresh sidecar runtime and starts its
// proxies with no optional lane, returning the result and the jail's git
// config.
func startProxiesWith(t *testing.T, bundle *credbundle.Bundle) (sidecarproto.StartProxiesResult, map[string]string) {
	t.Helper()
	orch, rt := dialRuntime(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := orch.Call(ctx, sidecarproto.KindSealedBundle,
		sidecarproto.SealedBundleBody{Sealed: sealBundleTo(t, rt.keypair.Public, bundle)}, nil); err != nil {
		t.Fatalf("relay bundle: %v", err)
	}
	var res sidecarproto.StartProxiesResult
	if err := orch.Call(ctx, sidecarproto.KindStartProxies, sidecarproto.StartProxiesBody{HostVethIP: "127.0.0.1"}, &res); err != nil {
		t.Fatalf("start proxies: %v", err)
	}
	return res, gitConfigFromEnv(t, res.Env)
}

// TestRuntime_StartProxiesStampsTheBundleIdentity: the jail's commits are
// authored as the org identity the brain sealed, and the result reports that
// identity, account id included, for the orchestrator's co-author check.
func TestRuntime_StartProxiesStampsTheBundleIdentity(t *testing.T) {
	res, cfg := startProxiesWith(t, &credbundle.Bundle{
		LLM: map[string]string{"ANTHROPIC_API_KEY": "sk-ant-real"},
		GitHub: &credbundle.GitHubCreds{
			Mode:              credbundle.GitHubModeApp,
			BaseURL:           "https://github.com",
			IdentityName:      "acme[bot]",
			IdentityEmail:     "316798692+acme[bot]@users.noreply.github.com",
			IdentityAccountID: "316798692",
		},
	})

	if cfg["user.name"] != "acme[bot]" || cfg["user.email"] != "316798692+acme[bot]@users.noreply.github.com" {
		t.Errorf("jail git identity = (%q, %q), want the bundle's org identity (git config: %v)", cfg["user.name"], cfg["user.email"], cfg)
	}
	if res.IdentityName != "acme[bot]" || res.IdentityEmail != "316798692+acme[bot]@users.noreply.github.com" || res.IdentityAccountID != "316798692" {
		t.Errorf("reported identity = (%q, %q, %q), want the bundle's", res.IdentityName, res.IdentityEmail, res.IdentityAccountID)
	}
}

// TestRuntime_StartProxiesFallsBackWithoutAnIdentity: a bundle with no
// complete org identity — none at all, half of one, or no GitHub half —
// leaves the jail committing under the neutral fallback rather than under no
// identity, and reports no org identity, so no co-author trailer is decided
// against one.
func TestRuntime_StartProxiesFallsBackWithoutAnIdentity(t *testing.T) {
	for _, c := range []struct {
		name string
		gh   *credbundle.GitHubCreds
	}{
		{"no identity", &credbundle.GitHubCreds{Mode: credbundle.GitHubModeApp, BaseURL: "https://github.com"}},
		{"name without email", &credbundle.GitHubCreds{Mode: credbundle.GitHubModePAT, BaseURL: "https://github.com", IdentityName: "octocat"}},
		{"no github half", nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			res, cfg := startProxiesWith(t, &credbundle.Bundle{
				LLM:    map[string]string{"ANTHROPIC_API_KEY": "sk-ant-real"},
				GitHub: c.gh,
			})
			if cfg["user.name"] != githooks.FallbackIdentityName || cfg["user.email"] != githooks.FallbackIdentityEmail {
				t.Errorf("jail git identity = (%q, %q), want the fallback (%q, %q)",
					cfg["user.name"], cfg["user.email"], githooks.FallbackIdentityName, githooks.FallbackIdentityEmail)
			}
			if res.IdentityName != "" || res.IdentityEmail != "" || res.IdentityAccountID != "" {
				t.Errorf("reported identity = (%q, %q, %q), want none", res.IdentityName, res.IdentityEmail, res.IdentityAccountID)
			}
		})
	}
}
