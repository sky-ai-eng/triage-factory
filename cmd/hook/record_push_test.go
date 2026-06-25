package hook

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"

	"github.com/sky-ai-eng/triage-factory/cmd/exec/agenthost"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	_ "modernc.org/sqlite"
)

// newTestStores opens an in-memory SQLite, migrates it, and seeds one runs
// row the artifacts FK (run_id) can point at. Returns the stores plus the
// run id to wire into RunInfo.
func newTestStores(t *testing.T) (db.Stores, string) {
	t.Helper()
	conn, err := sql.Open("sqlite", ":memory:?_pragma=foreign_keys(on)")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if err := db.Migrate(conn, "sqlite3"); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	// Minimal valid run: event-triggered (creator_user_id NULL) + a
	// non-blueprint origin so the parent-pairing CHECKs don't demand a
	// task/prompt/blueprint chain. org_id/team_id default to the local
	// sentinels. The artifact only needs run_id to satisfy its FK.
	if _, err := conn.Exec(`INSERT INTO runs (id, trigger_type, creator_user_id, origin) VALUES ('r1', 'event', NULL, 'interactive')`); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	return sqlitestore.New(conn), "r1"
}

func hostFor(stores db.Stores, runID string, eventTriggered bool) agenthost.Client {
	return agenthost.NewLocal(stores, agenthost.RunInfo{
		OrgID:            runmode.LocalDefaultOrgID,
		UserID:           runmode.LocalDefaultUserID,
		TeamID:           runmode.LocalDefaultTeamID,
		RunID:            runID,
		IsEventTriggered: eventTriggered,
	})
}

func TestRecordPush_UpsertsBranchArtifact(t *testing.T) {
	stores, runID := newTestStores(t)
	host := hostFor(stores, runID, false)

	runRecordPush(host, []string{
		"--remote", "https://github.com/octo/repo.git",
		"--ref", "refs/heads/feature/x",
		"--sha", "abc123",
		"--new=true",
	})

	arts, err := stores.Artifacts.ListByRun(context.Background(), runmode.LocalDefaultOrgID, runID)
	if err != nil {
		t.Fatalf("ListByRun: %v", err)
	}
	if len(arts) != 1 {
		t.Fatalf("got %d artifacts, want 1", len(arts))
	}
	a := arts[0]
	if a.Provider != domain.ArtifactProviderGit || a.Kind != domain.ArtifactKindBranch {
		t.Errorf("provider/kind = %q/%q, want git/branch", a.Provider, a.Kind)
	}
	if a.Target != "octo/repo" {
		t.Errorf("target = %q, want octo/repo", a.Target)
	}
	if a.ExternalID != "refs/heads/feature/x" {
		t.Errorf("external_id = %q, want refs/heads/feature/x", a.ExternalID)
	}
	if want := "https://github.com/octo/repo/tree/feature/x"; a.URL != want {
		t.Errorf("url = %q, want %q", a.URL, want)
	}
	if a.State != domain.ArtifactStateBranchPushed {
		t.Errorf("state = %q, want pushed", a.State)
	}
	if want := "git:branch:octo/repo:refs/heads/feature/x"; a.DedupKey != want {
		t.Errorf("dedup_key = %q, want %q", a.DedupKey, want)
	}
	if a.RunID != runID || a.OrgID != runmode.LocalDefaultOrgID || a.TeamID != runmode.LocalDefaultTeamID {
		t.Errorf("identity not stamped: run=%q org=%q team=%q", a.RunID, a.OrgID, a.TeamID)
	}
	var d branchDetails
	if err := json.Unmarshal([]byte(a.DetailsJSON), &d); err != nil {
		t.Fatalf("details_json %q: %v", a.DetailsJSON, err)
	}
	if d.SHA != "abc123" || !d.New {
		t.Errorf("details = %+v, want sha=abc123 new=true", d)
	}
}

// TestRecordPush_RepushUpsertsOneRow proves a second push of the same branch
// lands on the one deduped row, updating the volatile payload (sha, new)
// rather than minting a second artifact.
func TestRecordPush_RepushUpsertsOneRow(t *testing.T) {
	stores, runID := newTestStores(t)
	host := hostFor(stores, runID, false)

	runRecordPush(host, []string{"--remote", "git@github.com:octo/repo.git", "--ref", "refs/heads/main", "--sha", "aaa", "--new=true"})
	runRecordPush(host, []string{"--remote", "git@github.com:octo/repo.git", "--ref", "refs/heads/main", "--sha", "bbb", "--new=false"})

	arts, err := stores.Artifacts.ListByRun(context.Background(), runmode.LocalDefaultOrgID, runID)
	if err != nil {
		t.Fatalf("ListByRun: %v", err)
	}
	if len(arts) != 1 {
		t.Fatalf("got %d artifacts, want 1 (re-push must upsert)", len(arts))
	}
	var d branchDetails
	_ = json.Unmarshal([]byte(arts[0].DetailsJSON), &d)
	if d.SHA != "bbb" || d.New {
		t.Errorf("details after re-push = %+v, want sha=bbb new=false", d)
	}
}

// TestRecordPush_EventTriggeredUsesSystemPool exercises the admin-pool
// branch (an auto-delegated run has no user identity). In SQLite both pools
// are the one connection, so success here just confirms the branch writes a
// row rather than erroring.
func TestRecordPush_EventTriggeredUsesSystemPool(t *testing.T) {
	stores, runID := newTestStores(t)
	host := hostFor(stores, runID, true)

	runRecordPush(host, []string{"--remote", "https://github.com/octo/repo", "--ref", "refs/heads/main", "--sha", "ccc", "--new=true"})

	arts, err := stores.Artifacts.ListByRun(context.Background(), runmode.LocalDefaultOrgID, runID)
	if err != nil {
		t.Fatalf("ListByRun: %v", err)
	}
	if len(arts) != 1 {
		t.Fatalf("got %d artifacts, want 1", len(arts))
	}
}

// TestRecordPush_SkipsNonBranchRef confirms a tag (or any non refs/heads ref)
// is not recorded as a branch artifact.
func TestRecordPush_SkipsNonBranchRef(t *testing.T) {
	stores, runID := newTestStores(t)
	host := hostFor(stores, runID, false)

	runRecordPush(host, []string{"--remote", "https://github.com/octo/repo", "--ref", "refs/tags/v1.0", "--sha", "ddd", "--new=true"})

	arts, err := stores.Artifacts.ListByRun(context.Background(), runmode.LocalDefaultOrgID, runID)
	if err != nil {
		t.Fatalf("ListByRun: %v", err)
	}
	if len(arts) != 0 {
		t.Fatalf("got %d artifacts, want 0 (tags are not branches)", len(arts))
	}
}

func TestBranchWebURL_EscapesSegments(t *testing.T) {
	cases := []struct {
		branch string
		want   string
	}{
		{"main", "https://github.com/octo/repo/tree/main"},
		// `/` separators are preserved as path separators.
		{"feature/x", "https://github.com/octo/repo/tree/feature/x"},
		// `#`, space, and `%` are escaped so they don't break the link.
		{"feature/#123", "https://github.com/octo/repo/tree/feature/%23123"},
		{"wip/a b", "https://github.com/octo/repo/tree/wip/a%20b"},
		{"odd/100%done", "https://github.com/octo/repo/tree/odd/100%25done"},
	}
	for _, c := range cases {
		if got := branchWebURL("octo", "repo", c.branch); got != c.want {
			t.Errorf("branchWebURL(octo, repo, %q) = %q, want %q", c.branch, got, c.want)
		}
	}
}

func TestParseRemoteOwnerRepo(t *testing.T) {
	cases := []struct {
		remote string
		owner  string
		repo   string
		ok     bool
	}{
		{"https://github.com/octo/repo.git", "octo", "repo", true},
		{"https://github.com/octo/repo", "octo", "repo", true},
		{"git@github.com:octo/repo.git", "octo", "repo", true},
		{"git@github.com:octo/repo", "octo", "repo", true},
		{"ssh://git@github.com/octo/repo.git", "octo", "repo", true},
		// Sandbox git-proxy rewrites the remote to its veth address; the
		// owner/repo path still parses cleanly.
		{"http://10.42.0.1:38573/octo/repo", "octo", "repo", true},
		{"http://127.0.0.1:9000/octo/repo", "octo", "repo", true},
		// Unparseable / unsupported shapes -> best-effort skip.
		{"", "", "", false},
		{"https://github.com/octo", "", "", false},
		// Non-GitHub public hosts are rejected even when the path is exactly
		// owner/repo — the github.com web URL would be wrong for them.
		{"https://gitlab.com/group/repo.git", "", "", false},
		{"git@gitlab.com:group/repo.git", "", "", false},
		{"https://gitlab.com/group/subgroup/repo.git", "", "", false},
	}
	for _, c := range cases {
		owner, repo, ok := parseRemoteOwnerRepo(c.remote)
		if ok != c.ok || owner != c.owner || repo != c.repo {
			t.Errorf("parseRemoteOwnerRepo(%q) = (%q,%q,%v), want (%q,%q,%v)",
				c.remote, owner, repo, ok, c.owner, c.repo, c.ok)
		}
	}
}
