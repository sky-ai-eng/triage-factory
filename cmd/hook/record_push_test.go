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

// pushDetails mirrors a branch artifact's details_json payload for assertions;
// the producing type is unexported in internal/domain (NewBranchArtifact).
type pushDetails struct {
	SHA string `json:"sha"`
	New bool   `json:"new"`
}

// newTestStores opens an in-memory SQLite, migrates it, and seeds one
// conversations row the artifacts FK (conversation_id) can point at. Returns the
// stores plus the run id to wire into ConversationInfo.
func newTestStores(t *testing.T) (db.Stores, string) {
	t.Helper()
	conn, err := sql.Open("sqlite", db.TestDSNMemory)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if err := db.Migrate(conn, "sqlite3"); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	// Seed the local sentinel org so org_settings (org_id → orgs FK) can be
	// written — the branch-capture gate resolves the org's GitHub base from it.
	if _, err := conn.Exec(`INSERT INTO orgs (id, slug, name) VALUES (?, 'local', 'Local')`, runmode.LocalDefaultOrgID); err != nil {
		t.Fatalf("seed org: %v", err)
	}
	// Minimal valid run: event-triggered (creator_user_id NULL) + a
	// non-blueprint origin so the parent-pairing CHECKs don't demand a
	// task/prompt/blueprint chain. org_id/team_id default to the local
	// sentinels. The artifact only needs conversation_id to satisfy its FK.
	if _, err := conn.Exec(`INSERT INTO conversations (id, trigger_type, creator_user_id, origin, status) VALUES ('r1', 'event', NULL, 'interactive', 'running')`); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	return sqlitestore.New(conn), "r1"
}

func hostFor(stores db.Stores, conversationID string, eventTriggered bool) agenthost.Client {
	return agenthost.NewLocal(stores, agenthost.ConversationInfo{
		OrgID:            runmode.LocalDefaultOrgID,
		UserID:           runmode.LocalDefaultUserID,
		TeamID:           runmode.LocalDefaultTeamID,
		ConversationID:   conversationID,
		IsEventTriggered: eventTriggered,
	})
}

func TestRecordPush_UpsertsBranchArtifact(t *testing.T) {
	stores, conversationID := newTestStores(t)
	host := hostFor(stores, conversationID, false)

	runRecordPush(host, []string{
		"--remote", "https://github.com/octo/repo.git",
		"--ref", "refs/heads/feature/x",
		"--sha", "abc123",
		"--new=true",
	})

	arts, err := stores.Artifacts.ListByConversation(context.Background(), runmode.LocalDefaultOrgID, conversationID)
	if err != nil {
		t.Fatalf("ListByConversation: %v", err)
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
	if a.ConversationID != conversationID || a.OrgID != runmode.LocalDefaultOrgID || a.TeamID != runmode.LocalDefaultTeamID {
		t.Errorf("identity not stamped: run=%q org=%q team=%q", a.ConversationID, a.OrgID, a.TeamID)
	}
	var d pushDetails
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
	stores, conversationID := newTestStores(t)
	host := hostFor(stores, conversationID, false)

	runRecordPush(host, []string{"--remote", "git@github.com:octo/repo.git", "--ref", "refs/heads/main", "--sha", "aaa", "--new=true"})
	runRecordPush(host, []string{"--remote", "git@github.com:octo/repo.git", "--ref", "refs/heads/main", "--sha", "bbb", "--new=false"})

	arts, err := stores.Artifacts.ListByConversation(context.Background(), runmode.LocalDefaultOrgID, conversationID)
	if err != nil {
		t.Fatalf("ListByConversation: %v", err)
	}
	if len(arts) != 1 {
		t.Fatalf("got %d artifacts, want 1 (re-push must upsert)", len(arts))
	}
	var d pushDetails
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
	stores, conversationID := newTestStores(t)
	host := hostFor(stores, conversationID, true)

	runRecordPush(host, []string{"--remote", "https://github.com/octo/repo", "--ref", "refs/heads/main", "--sha", "ccc", "--new=true"})

	arts, err := stores.Artifacts.ListByConversation(context.Background(), runmode.LocalDefaultOrgID, conversationID)
	if err != nil {
		t.Fatalf("ListByConversation: %v", err)
	}
	if len(arts) != 1 {
		t.Fatalf("got %d artifacts, want 1", len(arts))
	}
}

// TestRecordPush_SkipsNonBranchRef confirms a tag (or any non refs/heads ref)
// is not recorded as a branch artifact.
func TestRecordPush_SkipsNonBranchRef(t *testing.T) {
	stores, conversationID := newTestStores(t)
	host := hostFor(stores, conversationID, false)

	runRecordPush(host, []string{"--remote", "https://github.com/octo/repo", "--ref", "refs/tags/v1.0", "--sha", "ddd", "--new=true"})

	arts, err := stores.Artifacts.ListByConversation(context.Background(), runmode.LocalDefaultOrgID, conversationID)
	if err != nil {
		t.Fatalf("ListByConversation: %v", err)
	}
	if len(arts) != 0 {
		t.Fatalf("got %d artifacts, want 0 (tags are not branches)", len(arts))
	}
}

// TestRecordPush_GHESHostRecordsAndAnchorsURL is the end-to-end proof for the
// local-mode GHES path: with the org's GitHub base set to a GHES host, a push to
// that host is captured (the gate accepts it) AND the recorded branch link is
// anchored to the GHES host, not github.com. This is the scenario the earlier
// github.com-only gate silently dropped before an artifact was ever written.
func TestRecordPush_GHESHostRecordsAndAnchorsURL(t *testing.T) {
	stores, conversationID := newTestStores(t)
	if err := stores.Orgs.UpdateSettings(context.Background(), runmode.LocalDefaultOrgID,
		domain.OrgSettings{GitHubBaseURL: "https://github.corp.example.com"}); err != nil {
		t.Fatalf("seed org github base: %v", err)
	}
	host := hostFor(stores, conversationID, false)

	runRecordPush(host, []string{
		"--remote", "https://github.corp.example.com/octo/repo.git",
		"--ref", "refs/heads/feature/x",
		"--sha", "abc123",
		"--new=true",
	})

	arts, err := stores.Artifacts.ListByConversation(context.Background(), runmode.LocalDefaultOrgID, conversationID)
	if err != nil {
		t.Fatalf("ListByConversation: %v", err)
	}
	if len(arts) != 1 {
		t.Fatalf("got %d artifacts, want 1 (a GHES push must be captured in local mode)", len(arts))
	}
	if want := "https://github.corp.example.com/octo/repo/tree/feature/x"; arts[0].URL != want {
		t.Errorf("url = %q, want %q (anchored to the GHES host, not github.com)", arts[0].URL, want)
	}
}

// TestRecordPush_GHESOrgSkipsForeignHost proves the gate still excludes a push
// to a host that isn't the org's GitHub — here github.com from a GHES org — so
// an unrelated remote is never recorded (and never mis-anchored to the GHES
// host).
func TestRecordPush_GHESOrgSkipsForeignHost(t *testing.T) {
	stores, conversationID := newTestStores(t)
	if err := stores.Orgs.UpdateSettings(context.Background(), runmode.LocalDefaultOrgID,
		domain.OrgSettings{GitHubBaseURL: "https://github.corp.example.com"}); err != nil {
		t.Fatalf("seed org github base: %v", err)
	}
	host := hostFor(stores, conversationID, false)

	runRecordPush(host, []string{"--remote", "https://github.com/octo/repo", "--ref", "refs/heads/main", "--sha", "xyz", "--new=true"})

	arts, err := stores.Artifacts.ListByConversation(context.Background(), runmode.LocalDefaultOrgID, conversationID)
	if err != nil {
		t.Fatalf("ListByConversation: %v", err)
	}
	if len(arts) != 0 {
		t.Fatalf("got %d artifacts, want 0 (github.com is not the GHES org's host)", len(arts))
	}
}

func TestParseRemoteOwnerRepo(t *testing.T) {
	const ghes = "github.corp.example.com"
	cases := []struct {
		remote   string
		baseHost string
		owner    string
		repo     string
		ok       bool
	}{
		// A github.com org: its own host parses in every remote shape.
		{"https://github.com/octo/repo.git", "github.com", "octo", "repo", true},
		{"https://github.com/octo/repo", "github.com", "octo", "repo", true},
		{"git@github.com:octo/repo.git", "github.com", "octo", "repo", true},
		{"git@github.com:octo/repo", "github.com", "octo", "repo", true},
		{"ssh://git@github.com/octo/repo.git", "github.com", "octo", "repo", true},
		// A GHES org: its own host is accepted (URL- and SCP-style, case-
		// insensitive), which is the whole point of this change.
		{"https://github.corp.example.com/octo/repo.git", ghes, "octo", "repo", true},
		{"git@github.corp.example.com:octo/repo.git", ghes, "octo", "repo", true},
		{"https://GitHub.Corp.Example.Com/octo/repo", ghes, "octo", "repo", true},
		// ...but github.com is NOT the GHES org's host, so a push there is not
		// recorded (it isn't the org's repo, and would be mis-anchored).
		{"https://github.com/octo/repo", ghes, "", "", false},
		// Sandbox git-proxy veth (loopback/private) is trusted regardless of base.
		{"http://10.42.0.1:38573/octo/repo", "github.com", "octo", "repo", true},
		{"http://127.0.0.1:9000/octo/repo", ghes, "octo", "repo", true},
		// Base unresolved ("") falls back to a github.com-only gate.
		{"https://github.com/octo/repo", "", "octo", "repo", true},
		{"https://github.corp.example.com/octo/repo", "", "", "", false},
		// Unparseable / unsupported shapes -> best-effort skip.
		{"", "github.com", "", "", false},
		{"https://github.com/octo", "github.com", "", "", false},
		// A non-org public host is rejected even when the path is exactly
		// owner/repo — the recorded link would be wrong for it.
		{"https://gitlab.com/group/repo.git", "github.com", "", "", false},
		{"git@gitlab.com:group/repo.git", ghes, "", "", false},
		{"https://gitlab.com/group/subgroup/repo.git", "github.com", "", "", false},
	}
	for _, c := range cases {
		owner, repo, ok := parseRemoteOwnerRepo(c.remote, c.baseHost)
		if ok != c.ok || owner != c.owner || repo != c.repo {
			t.Errorf("parseRemoteOwnerRepo(%q, base=%q) = (%q,%q,%v), want (%q,%q,%v)",
				c.remote, c.baseHost, owner, repo, ok, c.owner, c.repo, c.ok)
		}
	}
}
