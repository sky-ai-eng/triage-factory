package delegate

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/cmd/exec/agenthost"
	"github.com/sky-ai-eng/triage-factory/internal/commitsign"
	"github.com/sky-ai-eng/triage-factory/internal/commitsigner"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/githooks"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/paths"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// signingSpawner is a local spawner over a fresh database whose org has a
// signer, active or pending, named `"Acme"` on its profile.
func signingSpawner(t *testing.T, active bool) (*Spawner, db.Stores, string) {
	t.Helper()
	runmode.SetForTest(t, runmode.ModeLocal)
	paths.SetForTest(t, t.TempDir())
	stores := sqlitestore.New(newDelegateTestDB(t))
	key, err := commitsign.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	public, err := commitsign.PublicKey(key)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := stores.OrgCommitSigners.Create(ctx, domain.OrgCommitSigner{
		OrgID: runmode.LocalDefaultOrgID, Host: "https://github.com", GitHubUserID: 583231,
		Login: "octocat", Name: `"Acme"`, PublicKey: public,
	}); err != nil {
		t.Fatalf("Create signer: %v", err)
	}
	if active {
		if _, err := stores.OrgCommitSigners.Activate(ctx, runmode.LocalDefaultOrgID, public); err != nil {
			t.Fatalf("Activate: %v", err)
		}
	}
	s := NewSpawner(nil, stores, ghclient.NewClient("https://fallback", "tok"), nil, "m")
	s.SetRunCredentialResolvers(&fakeResolver{orgLogin: "acme-bot"}, nil, nil)
	s.SetStores(stores)
	s.SetExecutorID("executor-1", 7)
	return s, stores, public
}

// An org with a signer commits as the signer, under the name git writes for
// its profile name, whatever the signer's status; only an active one signs.
func TestResolveCommitIdentity_Signer(t *testing.T) {
	for _, active := range []bool{true, false} {
		s, _, public := signingSpawner(t, active)
		id, signer := s.resolveCommitIdentity(context.Background(), runmode.LocalDefaultOrgID, "event", "")
		if id.Name != "Acme" || id.Email != "583231+octocat@users.noreply.github.com" {
			t.Errorf("active=%v: identity = %+v, want Acme <583231+octocat@users.noreply.github.com>", active, id)
		}
		wantMode := domain.CommitSigningNone
		if active {
			wantMode = domain.CommitSigningOrg
		}
		if signer.Mode != wantMode || signer.AccountID != "583231" {
			t.Errorf("active=%v: signer = %+v, want mode %s for 583231", active, signer, wantMode)
		}
		if active && signer.PublicKey != public {
			t.Errorf("signer key = %q, want %q", signer.PublicKey, public)
		}
	}
}

func TestPrepareLocalCommitSigning(t *testing.T) {
	s, stores, public := signingSpawner(t, true)
	ctx := context.Background()
	if _, err := stores.OrgCommitSigners.StagePendingKey(ctx, runmode.LocalDefaultOrgID, "ssh-ed25519 AAAApending"); err != nil {
		t.Fatalf("StagePendingKey: %v", err)
	}
	_, signer := s.resolveCommitIdentity(ctx, runmode.LocalDefaultOrgID, "event", "")

	c := s.prepareLocalCommitSigning(ctx, runmode.LocalDefaultOrgID, "conv-1", "claim-1", signer)
	if c == nil {
		t.Fatal("an active signer prepared no signing")
	}
	path := paths.AllowedSignersPath("claim-1")
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the allowed-signers file: %v", err)
	}
	want := `583231+octocat@users.noreply.github.com namespaces="git" ` + public + "\n" +
		`583231+octocat@users.noreply.github.com namespaces="git" ssh-ed25519 AAAApending` + "\n"
	if string(content) != want {
		t.Errorf("allowed signers =\n%s\nwant\n%s", content, want)
	}

	pairs := c.configPairs()
	wantPairs := githooks.SigningConfigPairs(githooks.HostDir(), public, path)
	if len(pairs) != len(wantPairs) {
		t.Fatalf("config pairs = %v, want %v", pairs, wantPairs)
	}
	for i := range wantPairs {
		if pairs[i] != wantPairs[i] {
			t.Errorf("config pair %d = %v, want %v", i, pairs[i], wantPairs[i])
		}
	}

	env := c.env()
	if len(env) != 1 || !strings.HasPrefix(env[0], agenthost.CommitSigningEnvVar+"=") {
		t.Fatalf("env = %v", env)
	}
	var handed agenthost.LocalCommitSigning
	if err := json.Unmarshal([]byte(strings.TrimPrefix(env[0], agenthost.CommitSigningEnvVar+"=")), &handed); err != nil {
		t.Fatalf("decode the env hand-off: %v", err)
	}
	if handed.ClaimID != "claim-1" || handed.BootEpoch != 7 || handed.Signer.Name != "Acme" ||
		handed.Signer.Mode != domain.CommitSigningOrg || handed.Budgets.PerClaim != commitsigner.DefaultMaxSignaturesPerClaim ||
		handed.AllowedSignersFile != path {
		t.Errorf("handed off %+v", handed)
	}
	if d := c.daemon(); d == nil || *d != handed {
		t.Errorf("the daemon signs with %+v, the CLI with %+v", d, handed)
	}
	if c.allowedSignersPath() != path {
		t.Errorf("the sandbox binds %q, want %q", c.allowedSignersPath(), path)
	}

	c.Close()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("Close left the allowed-signers file: %v", err)
	}
}

func TestPrepareLocalCommitSigning_RunsThatDoNotSign(t *testing.T) {
	t.Run("a pending signer", func(t *testing.T) {
		s, _, _ := signingSpawner(t, false)
		_, signer := s.resolveCommitIdentity(context.Background(), runmode.LocalDefaultOrgID, "event", "")
		if c := s.prepareLocalCommitSigning(context.Background(), runmode.LocalDefaultOrgID, "conv-1", "claim-1", signer); c != nil {
			t.Error("a pending signer prepared signing")
		}
	})
	t.Run("git older than 2.34", func(t *testing.T) {
		s, _, _ := signingSpawner(t, true)
		_, signer := s.resolveCommitIdentity(context.Background(), runmode.LocalDefaultOrgID, "event", "")
		bin := t.TempDir()
		if err := os.WriteFile(filepath.Join(bin, "git"), []byte("#!/bin/sh\necho 'git version 2.30.2'\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PATH", bin)
		if c := s.prepareLocalCommitSigning(context.Background(), runmode.LocalDefaultOrgID, "conv-1", "claim-1", signer); c != nil {
			t.Error("git 2.30 prepared signing")
		}
		if _, err := os.Stat(paths.AllowedSignersPath("claim-1")); !os.IsNotExist(err) {
			t.Errorf("an unsigned run wrote an allowed-signers file: %v", err)
		}
	})
	t.Run("no claim id", func(t *testing.T) {
		s, _, _ := signingSpawner(t, true)
		_, signer := s.resolveCommitIdentity(context.Background(), runmode.LocalDefaultOrgID, "event", "")
		if c := s.prepareLocalCommitSigning(context.Background(), runmode.LocalDefaultOrgID, "conv-1", "", signer); c != nil {
			t.Error("an engagement with no claim id prepared signing")
		}
		if entries, err := os.ReadDir(paths.AllowedSignersDir()); err == nil && len(entries) > 0 {
			t.Errorf("an engagement with no claim id wrote %d allowed-signers files", len(entries))
		}
	})
	t.Run("multi mode", func(t *testing.T) {
		s, _, _ := signingSpawner(t, true)
		_, signer := s.resolveCommitIdentity(context.Background(), runmode.LocalDefaultOrgID, "event", "")
		runmode.SetForTest(t, runmode.ModeMulti)
		if c := s.prepareLocalCommitSigning(context.Background(), runmode.LocalDefaultOrgID, "conv-1", "claim-1", signer); c != nil {
			t.Error("multi mode prepared local signing")
		}
	})
	t.Run("nil is inert", func(t *testing.T) {
		var c *localCommitSigning
		if c.configPairs() != nil || c.env() != nil || c.daemon() != nil || c.allowedSignersPath() != "" {
			t.Error("a run that does not sign got signing config")
		}
		c.Close()
	})
}
