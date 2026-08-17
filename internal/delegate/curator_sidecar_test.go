package delegate

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"sync"
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
	info := agenthost.ConversationInfo{OrgID: "org-1", TeamID: "team-1", ConversationID: "req-1"}
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

// seedCuratorTurn mints a curator conversation on a fresh project plus one
// ACTIVE claim homed to executorID — the shape a homed turn has when the
// executor brings its sidecar up. Raw SQL because curator conversations are
// minted by the curator engine, not any delegate-side store.
func seedCuratorTurn(t *testing.T, database *sql.DB, stores db.Stores, executorID string, bootEpoch int64) (conversationID string) {
	t.Helper()
	projectID, err := stores.Projects.Create(context.Background(), runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, domain.Project{Name: "homed"})
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	conversationID = "conv-curator-turn"
	if _, err := database.Exec(`
		INSERT INTO conversations (id, org_id, type, creator_user_id, team_id, visibility, trigger_type, origin, status, project_id)
		VALUES (?, ?, 'curator', ?, ?, 'private', 'manual', 'curator', NULL, ?)
	`, conversationID, runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID, runmode.LocalDefaultTeamID, projectID); err != nil {
		t.Fatalf("seed curator conversation: %v", err)
	}
	if _, err := database.Exec(`
		INSERT INTO claims (id, org_id, conversation_id, executor_id, boot_epoch)
		VALUES ('claim-curator-turn', ?, ?, ?, ?)
	`, runmode.LocalDefaultOrgID, conversationID, executorID, bootEpoch); err != nil {
		t.Fatalf("seed active claim: %v", err)
	}
	return conversationID
}

// fakeRunCredentials is an in-memory db.ClaimCredentialsStore standing in for
// the Postgres-only claim_credentials channel (the SQLite impl refuses every
// call), so the provision loop's poll half is exercisable here.
type fakeRunCredentials struct {
	mu         sync.Mutex
	executorID string
	sealed     []byte
	bootEpoch  int64
	sealedAt   time.Time
	ok         bool
}

func (f *fakeRunCredentials) Put(_ context.Context, _, _, executorID string, bootEpoch int64, sealed []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.executorID, f.sealed, f.bootEpoch, f.ok = executorID, sealed, bootEpoch, true
	// The real store stamps created_at on every write (created_at =
	// EXCLUDED.created_at), which is what the workspace-add freshness gate
	// compares against a reservation.
	f.sealedAt = time.Now()
	return nil
}

func (f *fakeRunCredentials) Get(context.Context, string, string) (db.SealedBundle, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return db.SealedBundle{
		ExecutorID: f.executorID,
		BootEpoch:  f.bootEpoch,
		Sealed:     f.sealed,
		SealedAt:   f.sealedAt,
	}, f.ok, nil
}

// TestCuratorSidecarProvisionFor pins the curator provision fn's handshake:
// it publishes the sidecar's pubkey onto the conversation's active claim,
// then polls the sealed-bundle channel and returns the OPAQUE sealed bytes
// the brain wrote (sealed to this executor's boot epoch) — the exact "pubkey
// publish → poll → opaque relay" loop the run path's sidecarProvisionFor runs.
func TestCuratorSidecarProvisionFor(t *testing.T) {
	database := newDelegateTestDB(t)
	stores := sqlitestore.New(database)
	ctx := context.Background()
	org := runmode.LocalDefaultOrgID

	conversationID := seedCuratorTurn(t, database, stores, "home-exec", 5)

	s := NewSpawner(database, stores, nil, nil, "")
	s.SetExecutorID("home-exec", 5)
	s.SetAwaitingCredentialsTimeout(3*time.Second, 5*time.Millisecond)
	creds := &fakeRunCredentials{}
	s.runCredentials = creds

	fn := s.curatorSidecarProvisionFor(org, conversationID)

	sealed := []byte("sealed-curator-bundle")
	// The brain seals + writes the bundle shortly after the executor publishes
	// its key. Race it with the provision poll.
	go func() {
		time.Sleep(20 * time.Millisecond)
		_ = creds.Put(ctx, org, conversationID, "home-exec", 5, sealed)
	}()

	gotSealed, gotEpoch, err := fn(ctx, "sidecar-pubkey-b64")
	if err != nil {
		t.Fatalf("provision fn: %v", err)
	}
	if string(gotSealed) != string(sealed) || gotEpoch != 5 {
		t.Errorf("provision fn = (%q, %d), want (%q, 5)", gotSealed, gotEpoch, sealed)
	}

	// The pubkey was published onto the active claim so the brain could seal
	// to it.
	info, ok, err := stores.Curator.GetTurnProvisionInfoSystem(ctx, org, conversationID)
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

	conversationID := seedCuratorTurn(t, database, stores, "home-exec", 5)

	s := NewSpawner(database, stores, nil, nil, "")
	s.SetExecutorID("home-exec", 5)
	s.SetAwaitingCredentialsTimeout(80*time.Millisecond, 5*time.Millisecond)
	s.runCredentials = &fakeRunCredentials{}

	fn := s.curatorSidecarProvisionFor(org, conversationID)
	if _, _, err := fn(ctx, "sidecar-pubkey-b64"); err == nil {
		t.Fatal("provision fn returned nil error, want a timeout (brain never provisioned)")
	}
}
