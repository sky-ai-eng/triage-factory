package credbundle

import "testing"

func TestResolveRepoToken(t *testing.T) {
	gh := &GitHubCreds{
		Mode: "app",
		RepoTokens: map[string]RepoToken{
			"Acme/Widgets": {Token: "ghs_widgets"},
		},
	}

	t.Run("exact repo match reports RepoTokenApp", func(t *testing.T) {
		tok, _, source := ResolveRepoToken(gh, "acme", "widgets")
		if tok != "ghs_widgets" || source != RepoTokenApp {
			t.Fatalf("ResolveRepoToken = (%q, %v), want (ghs_widgets, RepoTokenApp)", tok, source)
		}
	})

	t.Run("owner-only match resolves only when unambiguous", func(t *testing.T) {
		tok, _, source := ResolveRepoToken(gh, "ACME", "")
		if tok != "ghs_widgets" || source != RepoTokenApp {
			t.Fatalf("ResolveRepoToken owner-only = (%q, %v), want (ghs_widgets, RepoTokenApp)", tok, source)
		}
	})

	t.Run("no match, no PAT resolves to nothing", func(t *testing.T) {
		if _, _, source := ResolveRepoToken(gh, "other", "repo"); source != RepoTokenNone {
			t.Fatalf("source = %v, want RepoTokenNone", source)
		}
	})

	t.Run("nil bundle resolves to nothing", func(t *testing.T) {
		if _, _, source := ResolveRepoToken(nil, "a", "b"); source != RepoTokenNone {
			t.Fatalf("source = %v, want RepoTokenNone", source)
		}
	})

	t.Run("PAT fallback reports RepoTokenPAT even under an app-mode bundle", func(t *testing.T) {
		mixed := &GitHubCreds{
			Mode: "app",
			PAT:  "ghp_borrowed",
			RepoTokens: map[string]RepoToken{
				"acme/other-repo": {Token: "ghs_other"},
			},
		}
		tok, _, source := ResolveRepoToken(mixed, "acme", "widgets")
		if tok != "ghp_borrowed" || source != RepoTokenPAT {
			t.Fatalf("ResolveRepoToken = (%q, %v), want (ghp_borrowed, RepoTokenPAT)", tok, source)
		}
	})

	t.Run("app-tier RepoTokens match reports RepoTokenApp even under a pat-mode bundle", func(t *testing.T) {
		mixed := &GitHubCreds{
			Mode: "pat",
			PAT:  "ghp_borrowed",
			RepoTokens: map[string]RepoToken{
				"acme/widgets": {Token: "ghs_widgets"},
			},
		}
		tok, _, source := ResolveRepoToken(mixed, "acme", "widgets")
		if tok != "ghs_widgets" || source != RepoTokenApp {
			t.Fatalf("ResolveRepoToken = (%q, %v), want (ghs_widgets, RepoTokenApp)", tok, source)
		}
	})

	t.Run("ambiguous owner without a repo falls through to the PAT, never a sibling token", func(t *testing.T) {
		ambiguous := &GitHubCreds{
			PAT: "ghp_borrowed",
			RepoTokens: map[string]RepoToken{
				"acme/widgets": {Token: "ghs_widgets"},
				"acme/gadgets": {Token: "ghs_gadgets"},
			},
		}
		tok, _, source := ResolveRepoToken(ambiguous, "acme", "")
		if tok != "ghp_borrowed" || source != RepoTokenPAT {
			t.Fatalf("ResolveRepoToken ambiguous = (%q, %v), want (ghp_borrowed, RepoTokenPAT)", tok, source)
		}
	})
}
