package delegate

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/cmd/exec/agenthost"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

type fakeCuratorTeamRepos struct {
	db.TeamGitHubReposStore
	tracked map[string]bool
	err     error
}

func (f *fakeCuratorTeamRepos) TracksRepoSystem(_ context.Context, _, owner, repo string) (bool, error) {
	if f.err != nil {
		return false, f.err
	}
	return f.tracked[owner+"/"+repo], nil
}

// TestCuratorGitAuthorizeDecision pins the curator git proxy gate: a
// repo is authorized for a fetch only when it is BOTH pinned AND team-tracked,
// with no AllowedRefs (so a push is refused). A missing store or a lookup error
// fails closed.
func TestCuratorGitAuthorizeDecision(t *testing.T) {
	ctx := context.Background()
	info := agenthost.RunInfo{OrgID: "org-1", TeamID: "team-1", RunID: "req-1"}
	pinned := []string{"acme/widgets"}

	t.Run("pinned_and_tracked_allowed_no_refs", func(t *testing.T) {
		stores := db.Stores{TeamGitHubRepos: &fakeCuratorTeamRepos{tracked: map[string]bool{"acme/widgets": true}}}
		d, err := curatorGitAuthorizeDecision(ctx, stores, info, pinned, "acme", "widgets")
		if err != nil {
			t.Fatalf("decision: %v", err)
		}
		if !d.Allowed {
			t.Error("Allowed = false, want true (pinned + tracked)")
		}
		if len(d.AllowedRefs) != 0 {
			t.Errorf("AllowedRefs = %v, want none (read-only fetch)", d.AllowedRefs)
		}
	})

	t.Run("pinned_but_untracked_denied", func(t *testing.T) {
		stores := db.Stores{TeamGitHubRepos: &fakeCuratorTeamRepos{tracked: map[string]bool{}}}
		d, err := curatorGitAuthorizeDecision(ctx, stores, info, pinned, "acme", "widgets")
		if err != nil || d.Allowed {
			t.Errorf("decision = (%+v, %v), want denied", d, err)
		}
		// Pinned-but-untracked is an admin problem, not a self-serve one.
		if d.DenyReason != "repo-not-tracked" || !strings.Contains(d.DenyMessage, "team admin") {
			t.Errorf("deny = (%q, %q), want reason repo-not-tracked + an admin hint", d.DenyReason, d.DenyMessage)
		}
	})

	t.Run("tracked_but_not_pinned_denied", func(t *testing.T) {
		stores := db.Stores{TeamGitHubRepos: &fakeCuratorTeamRepos{tracked: map[string]bool{"acme/widgets": true}}}
		d, err := curatorGitAuthorizeDecision(ctx, stores, info, []string{"acme/other"}, "acme", "widgets")
		if err != nil || d.Allowed {
			t.Errorf("decision = (%+v, %v), want denied (not pinned)", d, err)
		}
		// A curator turn can't `workspace add`; the repo is outside the project.
		if d.DenyReason != "repo-not-attached" || !strings.Contains(d.DenyMessage, "not attached to this project") {
			t.Errorf("deny = (%q, %q), want reason repo-not-attached + a project hint", d.DenyReason, d.DenyMessage)
		}
		if strings.Contains(d.DenyMessage, "workspace add") {
			t.Errorf("curator deny message %q should not suggest 'workspace add'", d.DenyMessage)
		}
	})

	t.Run("unpinned_and_untracked_reports_not_tracked", func(t *testing.T) {
		// Both conditions fail. Tracking is the deeper, admin-actionable
		// problem, so it must win — matching the exec-gh gate's ordering — and
		// the agent must NOT be told it's merely "not attached to this project".
		stores := db.Stores{TeamGitHubRepos: &fakeCuratorTeamRepos{tracked: map[string]bool{}}}
		d, err := curatorGitAuthorizeDecision(ctx, stores, info, []string{"acme/other"}, "acme", "widgets")
		if err != nil || d.Allowed {
			t.Errorf("decision = (%+v, %v), want denied", d, err)
		}
		if d.DenyReason != "repo-not-tracked" {
			t.Errorf("DenyReason = %q, want repo-not-tracked (tracking checked before pinned membership)", d.DenyReason)
		}
	})

	t.Run("nil_store_fails_closed", func(t *testing.T) {
		d, err := curatorGitAuthorizeDecision(ctx, db.Stores{}, info, pinned, "acme", "widgets")
		if err != nil || d.Allowed {
			t.Errorf("decision = (%+v, %v), want denied (no store)", d, err)
		}
	})

	t.Run("lookup_error_propagates", func(t *testing.T) {
		stores := db.Stores{TeamGitHubRepos: &fakeCuratorTeamRepos{err: errors.New("db down")}}
		if _, err := curatorGitAuthorizeDecision(ctx, stores, info, pinned, "acme", "widgets"); err == nil {
			t.Error("decision err = nil, want the propagated lookup error")
		}
	})
}

// TestCuratorSidecarProvisionFor pins the curator provision fn's handshake:
// it publishes the sidecar's pubkey onto the turn, then polls the
// curator_turn_credentials channel and returns the OPAQUE sealed bytes the brain
// wrote (sealed to this executor's boot epoch) — the exact "pubkey publish →
// poll → opaque relay" loop the run path's sidecarProvisionFor runs.
func TestCuratorSidecarProvisionFor(t *testing.T) {
	database := newDelegateTestDB(t)
	stores := sqlitestore.New(database)
	ctx := context.Background()
	org := runmode.LocalDefaultOrgID

	projectID, err := stores.Projects.Create(ctx, org, runmode.LocalDefaultTeamID, domain.Project{Name: "homed"})
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	requestID, err := stores.Curator.CreateRequest(ctx, org, projectID, runmode.LocalDefaultUserID, "home-exec", "hi")
	if err != nil {
		t.Fatalf("CreateRequest: %v", err)
	}

	s := NewSpawner(database, stores, nil, nil, "")
	s.SetExecutorID("home-exec", 5)
	s.SetAwaitingCredentialsTimeout(3*time.Second, 5*time.Millisecond)

	fn := s.curatorSidecarProvisionFor(org, requestID)

	sealed := []byte("sealed-curator-bundle")
	// The brain seals + writes the bundle shortly after the executor publishes
	// its key. Race it with the provision poll.
	go func() {
		time.Sleep(20 * time.Millisecond)
		_ = stores.CuratorTurnCredentials.Put(ctx, org, requestID, "home-exec", 5, sealed)
	}()

	gotSealed, gotEpoch, err := fn(ctx, "sidecar-pubkey-b64")
	if err != nil {
		t.Fatalf("provision fn: %v", err)
	}
	if string(gotSealed) != string(sealed) || gotEpoch != 5 {
		t.Errorf("provision fn = (%q, %d), want (%q, 5)", gotSealed, gotEpoch, sealed)
	}

	// The pubkey was published onto the turn so the brain could seal to it.
	info, ok, err := stores.Curator.GetTurnProvisionInfoSystem(ctx, org, requestID)
	if err != nil || !ok {
		t.Fatalf("GetTurnProvisionInfoSystem: ok=%v err=%v", ok, err)
	}
	if info.CredPubKey != "sidecar-pubkey-b64" {
		t.Errorf("published CredPubKey = %q, want sidecar-pubkey-b64", info.CredPubKey)
	}
}

// TestCuratorSidecarProvisionFor_TimesOut pins that a turn the brain never
// provisions surfaces a timeout (which fails the sidecar bring-up), rather than
// blocking forever.
func TestCuratorSidecarProvisionFor_TimesOut(t *testing.T) {
	database := newDelegateTestDB(t)
	stores := sqlitestore.New(database)
	ctx := context.Background()
	org := runmode.LocalDefaultOrgID

	projectID, err := stores.Projects.Create(ctx, org, runmode.LocalDefaultTeamID, domain.Project{Name: "homed"})
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	requestID, err := stores.Curator.CreateRequest(ctx, org, projectID, runmode.LocalDefaultUserID, "home-exec", "hi")
	if err != nil {
		t.Fatalf("CreateRequest: %v", err)
	}

	s := NewSpawner(database, stores, nil, nil, "")
	s.SetExecutorID("home-exec", 5)
	s.SetAwaitingCredentialsTimeout(80*time.Millisecond, 5*time.Millisecond)

	fn := s.curatorSidecarProvisionFor(org, requestID)
	if _, _, err := fn(ctx, "sidecar-pubkey-b64"); err == nil {
		t.Fatal("provision fn returned nil error, want a timeout (brain never provisioned)")
	}
}
