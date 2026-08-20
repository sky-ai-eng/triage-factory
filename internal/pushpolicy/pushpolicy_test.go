package pushpolicy_test

import (
	"context"
	"database/sql"
	"reflect"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/pushpolicy"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

func newStores(t *testing.T) db.Stores {
	t.Helper()
	conn, err := sql.Open("sqlite", db.TestDSNMemory)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	conn.SetMaxOpenConns(1)
	conn.SetMaxIdleConns(1)
	t.Cleanup(func() { _ = conn.Close() })
	if err := db.BootstrapSchemaForTest(conn); err != nil {
		t.Fatalf("bootstrap schema: %v", err)
	}
	return sqlitestore.New(conn)
}

func setPolicy(t *testing.T, stores db.Stores, policy string) {
	t.Helper()
	ctx := context.Background()
	settings, err := stores.Teams.GetSettingsSystem(ctx, runmode.LocalDefaultTeamID)
	if err != nil {
		t.Fatalf("read team settings: %v", err)
	}
	settings.BaseBranchPushPolicy = policy
	if _, err := stores.Teams.UpdateSettings(ctx, runmode.LocalDefaultTeamID, settings); err != nil {
		t.Fatalf("write team settings: %v", err)
	}
}

// TestProtectedBranches_UnprofiledRepoStillRefusesMainAndMaster pins the
// fail-closed fallback: a repo TF has never profiled must still protect the
// universal default-branch names, since "we don't know the base branch" is
// exactly when a base-branch push must not slip through.
func TestProtectedBranches_UnprofiledRepoStillRefusesMainAndMaster(t *testing.T) {
	stores := newStores(t)
	got, err := pushpolicy.ProtectedBranches(context.Background(), stores, runmode.LocalDefaultOrgID, domain.RepoRefFromSlug("acme/never-profiled"))
	if err != nil {
		t.Fatalf("ProtectedBranches: %v", err)
	}
	if !got["main"] || !got["master"] {
		t.Errorf("unprofiled protected set = %v; want main and master protected", got)
	}
}

// TestProtectedBranches_ProfileAddsDefaultAndBase: the repository row's recorded
// default branch and the user-configured base branch join the universal set.
func TestProtectedBranches_ProfileAddsDefaultAndBase(t *testing.T) {
	stores := newStores(t)
	ctx := context.Background()
	if _, err := stores.Repos.Upsert(ctx, runmode.LocalDefaultOrgID, domain.Repository{
		Owner: "acme", Repo: "api",
		DefaultBranch: "trunk",
		CloneURL:      "https://x", ProfileText: "t",
	}); err != nil {
		t.Fatalf("seed repository: %v", err)
	}
	// The configured base branch has its own write path (it's a user choice, not
	// a profiling output), so set it the way the settings surface does.
	row, err := stores.Repos.GetByRef(ctx, runmode.LocalDefaultOrgID, domain.RepoRefFromSlug("acme/api"))
	if err != nil || row == nil {
		t.Fatalf("GetByRef: got=%v err=%v", row, err)
	}
	if _, err := stores.Repos.UpdateBaseBranch(ctx, runmode.LocalDefaultOrgID, row.ID, "develop"); err != nil {
		t.Fatalf("set base branch: %v", err)
	}
	got, err := pushpolicy.ProtectedBranches(ctx, stores, runmode.LocalDefaultOrgID, domain.RepoRefFromSlug("acme/api"))
	if err != nil {
		t.Fatalf("ProtectedBranches: %v", err)
	}
	want := map[string]bool{"main": true, "master": true, "trunk": true, "develop": true}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("protected set = %v; want %v", got, want)
	}
}

// TestAllows walks the policy × dispatch-shape matrix. manual_only is the
// interesting row: it turns on WHO dispatched the run, because an
// event-triggered run's task text is externally authored.
func TestAllows(t *testing.T) {
	cases := []struct {
		policy         string
		eventTriggered bool
		want           bool
	}{
		{domain.BaseBranchPushNever, false, false},
		{domain.BaseBranchPushNever, true, false},
		{domain.BaseBranchPushManualOnly, false, true},
		{domain.BaseBranchPushManualOnly, true, false},
		{domain.BaseBranchPushAlways, false, true},
		{domain.BaseBranchPushAlways, true, true},
		// An unrecognized stored value (a rolled-back writer, a hand-edited row)
		// resolves to the strictest answer, never the most permissive.
		{"nonsense", false, false},
	}
	for _, c := range cases {
		stores := newStores(t)
		setPolicy(t, stores, c.policy)
		got, err := pushpolicy.Allows(context.Background(), stores, runmode.LocalDefaultTeamID, c.eventTriggered)
		if err != nil {
			t.Fatalf("Allows(%s, event=%v): %v", c.policy, c.eventTriggered, err)
		}
		if got != c.want {
			t.Errorf("Allows(%s, event=%v) = %v; want %v", c.policy, c.eventTriggered, got, c.want)
		}
	}
}

// TestAllows_DefaultsToNever: a team that has never touched the setting gets
// the refusing policy — the default has to be the safe one, since that's what
// every existing install silently adopts.
func TestAllows_DefaultsToNever(t *testing.T) {
	stores := newStores(t)
	got, err := pushpolicy.Allows(context.Background(), stores, runmode.LocalDefaultTeamID, false)
	if err != nil {
		t.Fatalf("Allows: %v", err)
	}
	if got {
		t.Error("a team with no explicit policy allowed a base-branch push; want refused")
	}
}

// TestAllows_UnevaluableTeamErrors: no team id / no store is an ERROR, not a
// quiet false, so each enforcement point can pick its own posture (the proxy
// gate denies, the local hook allows).
func TestAllows_UnevaluableTeamErrors(t *testing.T) {
	stores := newStores(t)
	if _, err := pushpolicy.Allows(context.Background(), stores, "", false); err == nil {
		t.Error("Allows with no team id returned nil error; want an error the caller can act on")
	}
	if _, err := pushpolicy.Allows(context.Background(), db.Stores{}, runmode.LocalDefaultTeamID, false); err == nil {
		t.Error("Allows with no team store returned nil error; want an error the caller can act on")
	}
}

// TestProtectedFor_PolicyEmptiesTheSet is the composition both enforcement
// points consume: an allowing policy leaves nothing protected, so the same
// push that is refused under the default succeeds.
func TestProtectedFor_PolicyEmptiesTheSet(t *testing.T) {
	ctx := context.Background()

	stores := newStores(t)
	strict, err := pushpolicy.ProtectedFor(ctx, stores, runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, domain.RepoRefFromSlug("acme/api"), false)
	if err != nil {
		t.Fatalf("ProtectedFor (default policy): %v", err)
	}
	if !strict["main"] {
		t.Errorf("default policy protected set = %v; want main protected", strict)
	}

	setPolicy(t, stores, domain.BaseBranchPushAlways)
	open, err := pushpolicy.ProtectedFor(ctx, stores, runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, domain.RepoRefFromSlug("acme/api"), true)
	if err != nil {
		t.Fatalf("ProtectedFor (always): %v", err)
	}
	if len(open) != 0 {
		t.Errorf("policy=always protected set = %v; want empty", open)
	}
}

// TestRefs renders the protected set in the full-ref form the git-proxy
// decision carries, sorted so a denial message reads the same way twice.
func TestRefs(t *testing.T) {
	got := pushpolicy.Refs(map[string]bool{"main": true, "develop": true})
	want := []string{"refs/heads/develop", "refs/heads/main"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Refs = %v; want %v", got, want)
	}
	if pushpolicy.Refs(nil) != nil {
		t.Error("Refs(nil) should be nil, so an allowing policy carries no protected refs")
	}
}
