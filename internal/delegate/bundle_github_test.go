package delegate

import (
	"context"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/cmd/exec/agenthost"
	"github.com/sky-ai-eng/triage-factory/internal/credbundle"
	"github.com/sky-ai-eng/triage-factory/internal/credseal"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

func TestBundleRepoToken(t *testing.T) {
	gh := &credbundle.GitHubCreds{
		Mode: "app",
		RepoTokens: map[string]credbundle.RepoToken{
			"Acme/Widgets": {Token: "ghs_widgets", ExpiresAt: time.Unix(100, 0)},
		},
	}

	t.Run("exact repo match", func(t *testing.T) {
		tok, exp, ok := bundleRepoToken(gh, "acme", "widgets")
		if !ok || tok != "ghs_widgets" || !exp.Equal(time.Unix(100, 0)) {
			t.Fatalf("bundleRepoToken = (%q, %v, %v), want (ghs_widgets, 100, true)", tok, exp, ok)
		}
	})

	t.Run("owner-only match (no repo in view)", func(t *testing.T) {
		tok, _, ok := bundleRepoToken(gh, "ACME", "")
		if !ok || tok != "ghs_widgets" {
			t.Fatalf("bundleRepoToken owner-only = (%q, %v), want ghs_widgets/true", tok, ok)
		}
	})

	t.Run("no match, no PAT", func(t *testing.T) {
		if _, _, ok := bundleRepoToken(gh, "other", "repo"); ok {
			t.Fatal("bundleRepoToken matched an unrelated owner/repo")
		}
	})

	t.Run("PAT fallback answers for any repo", func(t *testing.T) {
		patGH := &credbundle.GitHubCreds{Mode: "pat", PAT: "ghp_borrowed"}
		tok, exp, ok := bundleRepoToken(patGH, "whoever", "whatever")
		if !ok || tok != "ghp_borrowed" || !exp.IsZero() {
			t.Fatalf("bundleRepoToken PAT fallback = (%q, %v, %v), want (ghp_borrowed, zero, true)", tok, exp, ok)
		}
	})

	t.Run("nil bundle", func(t *testing.T) {
		if _, _, ok := bundleRepoToken(nil, "a", "b"); ok {
			t.Fatal("bundleRepoToken on a nil GitHubCreds returned ok=true")
		}
	})
}

func TestResolveGHClient_UsesBundleOnExecutorRole(t *testing.T) {
	gh := &credbundle.GitHubCreds{RepoTokens: map[string]credbundle.RepoToken{"acme/widgets": {Token: "ghs_x"}}}
	ctx := credbundle.WithBundle(context.Background(), &credbundle.Bundle{GitHub: gh})

	s := NewSpawner(nil, db.Stores{}, nil, nil, "")
	if c := s.resolveGHClient(ctx, "org-1", "acme", "widgets"); c == nil {
		t.Fatal("resolveGHClient with a matching bundle returned nil")
	}
	if c := s.resolveGHClient(ctx, "org-1", "someone-else", "whatever"); c != nil {
		t.Fatal("resolveGHClient with no matching bundle entry and no PAT returned a non-nil client")
	}
}

// TestResolveGHClient_AmbiguousOwnerWithoutRepoDoesNotPickAnArbitraryToken
// pins the fix for the map-iteration-order bug: an owner with MULTIPLE
// repo-scoped tokens and no repo in view must never resolve to an
// arbitrary sibling repo's token (which would 403 whatever call it's used
// for) — only an exact repo match, or a single unambiguous candidate, may
// resolve.
func TestResolveGHClient_AmbiguousOwnerWithoutRepoDoesNotPickAnArbitraryToken(t *testing.T) {
	gh := &credbundle.GitHubCreds{RepoTokens: map[string]credbundle.RepoToken{
		"acme/widgets": {Token: "ghs_widgets"},
		"acme/gadgets": {Token: "ghs_gadgets"},
	}}
	ctx := credbundle.WithBundle(context.Background(), &credbundle.Bundle{GitHub: gh})
	s := NewSpawner(nil, db.Stores{}, nil, nil, "")

	if c := s.resolveGHClient(ctx, "org-1", "acme", ""); c != nil {
		t.Fatal("resolveGHClient with an ambiguous owner (2 repos, no repo given, no PAT) returned a non-nil client")
	}
	if got := s.resolveCloneToken(ctx, "org-1", "acme", ""); got != "" {
		t.Errorf("resolveCloneToken with an ambiguous owner = %q, want empty", got)
	}
	// A concrete repo resolves unambiguously even with a sibling present.
	if got := s.resolveCloneToken(ctx, "org-1", "acme", "gadgets"); got != "ghs_gadgets" {
		t.Errorf("resolveCloneToken(repo=gadgets) = %q, want %q", got, "ghs_gadgets")
	}
}

func TestResolveCloneToken_UsesBundleOnExecutorRole(t *testing.T) {
	gh := &credbundle.GitHubCreds{RepoTokens: map[string]credbundle.RepoToken{"acme/widgets": {Token: "ghs_clone"}}}
	ctx := credbundle.WithBundle(context.Background(), &credbundle.Bundle{GitHub: gh})

	s := NewSpawner(nil, db.Stores{}, nil, nil, "")
	if got := s.resolveCloneToken(ctx, "org-1", "acme", "widgets"); got != "ghs_clone" {
		t.Errorf("resolveCloneToken = %q, want %q", got, "ghs_clone")
	}
}

// TestGitProxyConfigFor_RereadsFreshBundleOnEachTokenSourceCall pins the
// refresh design (TFAC-614 point 5): the git proxy's TokenSource must read
// the NEWEST sealed bundle on every call, not a snapshot taken at run
// start — otherwise the brain's periodic re-mint of hour-lived GitHub
// tokens could never reach a run already past its setup gate.
func TestGitProxyConfigFor_RereadsFreshBundleOnEachTokenSourceCall(t *testing.T) {
	runmode.SetRoleForTest(t, runmode.RoleExecutor)
	database := newDelegateTestDB(t)
	seedRun(t, database, "run-refresh", "sess", "/tmp/wt")
	stores := sqlitestore.New(database)
	s := NewSpawner(database, stores, nil, nil, "")
	s.SetExecutorID("executor-1", 3)

	kp, err := credseal.GenerateKeyPair()
	if err != nil {
		t.Fatalf("generate keypair: %v", err)
	}
	s.SetSealingKey(kp)

	putBundle := func(token string) {
		t.Helper()
		bundle := &credbundle.Bundle{BootEpoch: 3, GitHub: &credbundle.GitHubCreds{
			RepoTokens: map[string]credbundle.RepoToken{"acme/widgets": {Token: token}},
		}}
		plaintext, err := bundle.Marshal()
		if err != nil {
			t.Fatalf("marshal bundle: %v", err)
		}
		sealed, err := credseal.Seal(kp.Public, plaintext)
		if err != nil {
			t.Fatalf("seal bundle: %v", err)
		}
		if err := stores.RunCredentials.Put(context.Background(), runmode.LocalDefaultOrgID, "run-refresh", "executor-1", 3, sealed); err != nil {
			t.Fatalf("put run credentials: %v", err)
		}
	}

	putBundle("ghs_first")
	info := agenthost.RunInfo{OrgID: runmode.LocalDefaultOrgID, TeamID: runmode.LocalDefaultTeamID, RunID: "run-refresh"}
	ctx := credbundle.WithBundle(context.Background(), &credbundle.Bundle{GitHub: &credbundle.GitHubCreds{RepoTokens: map[string]credbundle.RepoToken{}}})
	cfg := s.gitProxyConfigFor(ctx, info, stores)
	if cfg == nil {
		t.Fatal("gitProxyConfigFor returned nil for a run with a provisioned GitHub bundle")
	}

	tok, err := cfg.TokenSource(context.Background(), "acme", "widgets")
	if err != nil {
		t.Fatalf("TokenSource (first read): %v", err)
	}
	if tok.Value != "ghs_first" {
		t.Fatalf("TokenSource = %q, want %q", tok.Value, "ghs_first")
	}

	// The brain's refresh sweep re-mints and overwrites the bundle mid-run.
	putBundle("ghs_refreshed")

	tok, err = cfg.TokenSource(context.Background(), "acme", "widgets")
	if err != nil {
		t.Fatalf("TokenSource (after refresh): %v", err)
	}
	if tok.Value != "ghs_refreshed" {
		t.Fatalf("TokenSource after refresh = %q, want %q (must re-read, not cache the run-start snapshot)", tok.Value, "ghs_refreshed")
	}
}
