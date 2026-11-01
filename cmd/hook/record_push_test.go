package hook

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sky-ai-eng/triage-factory/cmd/exec/agenthost"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/github/githubapp"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	_ "modernc.org/sqlite"
)

// pushDetails mirrors a branch_pushed row's detail_json payload for
// assertions; the producing type is unexported in internal/domain
// (NewBranchPushAction).
type pushDetails struct {
	SHA string `json:"sha"`
	New bool   `json:"new"`
}

// pushRows returns the conversation's branch_pushed ledger rows and requires
// it to hold no deliverable: a push leaves its ledger row alone.
func pushRows(t *testing.T, stores db.Stores, conversationID string) []domain.ExternalAction {
	t.Helper()
	ctx := context.Background()
	acts, _, err := stores.ExternalActions.ListByConversation(ctx, runmode.LocalDefaultOrgID, conversationID,
		domain.ExternalActionListOpts{Action: domain.ActionBranchPushed})
	if err != nil {
		t.Fatalf("ExternalActions.ListByConversation: %v", err)
	}
	arts, _, err := stores.Deliverables.ListByConversation(ctx, runmode.LocalDefaultOrgID, conversationID, db.Unwindowed)
	if err != nil {
		t.Fatalf("Deliverables.ListByConversation: %v", err)
	}
	if len(arts) != 0 {
		t.Errorf("a push wrote %d deliverables, want none: %+v", len(arts), arts)
	}
	return acts
}

// newTestStores opens an in-memory SQLite, migrates it, and seeds one
// conversations row a ledger row's conversation_id can point at. Returns the
// stores plus the conversation id to wire into ConversationInfo.
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
	if err := db.SetLocalGitHubURLForTest(conn, "https://github.com"); err != nil {
		t.Fatalf("set github url: %v", err)
	}
	// Minimal valid conversation: event-triggered (creator_user_id NULL) + a
	// non-blueprint origin so the parent-pairing CHECKs don't demand a
	// task/prompt/blueprint chain. org_id/team_id default to the local
	// sentinels.
	if _, err := conn.Exec(`INSERT INTO conversations (id, trigger_type, creator_user_id, origin, status) VALUES ('r1', 'event', NULL, 'interactive', 'running')`); err != nil {
		t.Fatalf("seed conversation: %v", err)
	}
	return sqlitestore.New(conn), "r1"
}

// registerOctoRepo gives the registry a row for octo/repo on host, carrying
// GitHub's id for it, as the repository of a run's checkout has one: the
// recorder names the pushed repository by the row. Returns the row's id.
func registerOctoRepo(t *testing.T, stores db.Stores, host string) string {
	t.Helper()
	ctx := context.Background()
	ref := domain.RepoRef{Source: domain.RepoSourceGitHub, Host: host, Owner: "octo", Repo: "repo", ExternalID: "1"}
	row, err := stores.Repos.FindOrCreateByRefSystem(ctx, runmode.LocalDefaultOrgID, ref)
	if err != nil {
		t.Fatalf("register octo/repo on %s: %v", host, err)
	}
	if _, err := stores.Repos.FillMissingExternalIDsSystem(ctx, runmode.LocalDefaultOrgID, []domain.RepoRef{ref}); err != nil {
		t.Fatalf("record octo/repo's id on %s: %v", host, err)
	}
	return row.ID
}

// hostFor is the hook's in-process host client, reading GitHub through a fake
// that answers octo/repo's identity: the hook is the push's observer, and reads
// the repository's id with the run's credential.
func hostFor(t *testing.T, stores db.Stores, conversationID string, eventTriggered bool) agenthost.Client {
	t.Helper()
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v3/repos/octo/repo" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":1,"full_name":"octo/repo"}`)
	}))
	t.Cleanup(gh.Close)
	host := agenthost.NewLocal(stores, agenthost.ConversationInfo{
		OrgID:            runmode.LocalDefaultOrgID,
		UserID:           runmode.LocalDefaultUserID,
		TeamID:           runmode.LocalDefaultTeamID,
		ConversationID:   conversationID,
		IsEventTriggered: eventTriggered,
	})
	host.SetGitHubResolver(fakeResolver{stores: stores, apiURL: gh.URL})
	return host
}

// fakeResolver resolves every repository's client against apiURL, a fake
// GitHub, and answers the org's own GitHub base from its settings, as the real
// resolver does.
type fakeResolver struct {
	stores db.Stores
	apiURL string
}

func (f fakeResolver) ClientFor(context.Context, string, string) (*ghclient.Client, error) {
	panic("ClientFor is not used by the hook")
}

func (f fakeResolver) ClientForInstallation(context.Context, string, domain.OrgGitHubAppInstallation) (*ghclient.Client, error) {
	panic("ClientForInstallation is not used by the hook")
}

func (f fakeResolver) ClientForRepo(context.Context, string, string, string) (*ghclient.Client, error) {
	return ghclient.NewClient(f.apiURL, "org-pat"), nil
}

func (f fakeResolver) TokenFor(context.Context, string, string) (githubapp.Token, error) {
	panic("TokenFor is not used by the hook")
}

func (f fakeResolver) BaseURLFor(ctx context.Context, orgID string) (string, error) {
	return db.OrgGitHubHostSystem(ctx, f.stores.Orgs, orgID)
}

func (f fakeResolver) OrgIdentityFor(context.Context, string) (string, string, bool) {
	panic("OrgIdentityFor is not used by the hook")
}

func TestRecordPush_RecordsTheBranchPush(t *testing.T) {
	stores, conversationID := newTestStores(t)
	repoID := registerOctoRepo(t, stores, "https://github.com")
	host := hostFor(t, stores, conversationID, false)

	runRecordPush(host, []string{
		"--remote", "https://github.com/octo/repo.git",
		"--ref", "refs/heads/feature/x",
		"--sha", "abc123",
		"--new=true",
	})

	acts := pushRows(t, stores, conversationID)
	if len(acts) != 1 {
		t.Fatalf("got %d branch_pushed rows, want 1", len(acts))
	}
	a := acts[0]
	if a.Provider != domain.ProviderGitHub || a.Target != "octo/repo" || a.ExternalID != "refs/heads/feature/x" {
		t.Errorf("row = provider %q target %q external_id %q, want github, octo/repo, refs/heads/feature/x", a.Provider, a.Target, a.ExternalID)
	}
	if a.ObjectKind != domain.ObjectKindBranch || a.ObjectKey != "refs/heads/feature/x" || a.RepositoryID != repoID {
		t.Errorf("object = %q %q in repository %q, want the branch keyed on its ref in %s", a.ObjectKind, a.ObjectKey, a.RepositoryID, repoID)
	}
	if want := "https://github.com/octo/repo/tree/feature/x"; a.URL != want {
		t.Errorf("url = %q, want %q", a.URL, want)
	}
	if want := domain.BranchPushDedupKey(conversationID, repoID, "refs/heads/feature/x", "abc123"); a.DedupKey != want {
		t.Errorf("dedup_key = %q, want %q", a.DedupKey, want)
	}
	if a.ConversationID != conversationID || a.OrgID != runmode.LocalDefaultOrgID || a.TeamID != runmode.LocalDefaultTeamID {
		t.Errorf("identity not stamped: conversation=%q org=%q team=%q", a.ConversationID, a.OrgID, a.TeamID)
	}
	var d pushDetails
	if err := json.Unmarshal([]byte(a.DetailJSON), &d); err != nil {
		t.Fatalf("detail_json %q: %v", a.DetailJSON, err)
	}
	if d.SHA != "abc123" || !d.New {
		t.Errorf("details = %+v, want sha=abc123 new=true", d)
	}
}

// TestRecordPush_RepushIsItsOwnRow proves a second push of the same branch, of
// another commit, is recorded as a push of its own, and the same push observed
// twice is one row.
func TestRecordPush_RepushIsItsOwnRow(t *testing.T) {
	stores, conversationID := newTestStores(t)
	registerOctoRepo(t, stores, "https://github.com")
	host := hostFor(t, stores, conversationID, false)

	runRecordPush(host, []string{"--remote", "git@github.com:octo/repo.git", "--ref", "refs/heads/main", "--sha", "aaa", "--new=true"})
	runRecordPush(host, []string{"--remote", "git@github.com:octo/repo.git", "--ref", "refs/heads/main", "--sha", "aaa", "--new=true"})
	runRecordPush(host, []string{"--remote", "git@github.com:octo/repo.git", "--ref", "refs/heads/main", "--sha", "bbb", "--new=false"})

	acts := pushRows(t, stores, conversationID)
	if len(acts) != 2 {
		t.Fatalf("got %d branch_pushed rows, want 2 (one per commit pushed)", len(acts))
	}
	shas := map[string]bool{}
	for _, a := range acts {
		var d pushDetails
		_ = json.Unmarshal([]byte(a.DetailJSON), &d)
		shas[d.SHA] = d.New
	}
	if created, ok := shas["aaa"]; !ok || !created {
		t.Errorf("pushes = %v, want aaa recorded as creating the branch", shas)
	}
	if created, ok := shas["bbb"]; !ok || created {
		t.Errorf("pushes = %v, want bbb recorded as a push to the existing branch", shas)
	}
}

// TestRecordPush_EventTriggeredUsesSystemPool exercises the admin-pool
// branch (an auto-delegated conversation has no user identity). In SQLite
// both pools are the one connection, so success here just confirms the
// branch writes a row rather than erroring.
func TestRecordPush_EventTriggeredUsesSystemPool(t *testing.T) {
	stores, conversationID := newTestStores(t)
	registerOctoRepo(t, stores, "https://github.com")
	host := hostFor(t, stores, conversationID, true)

	runRecordPush(host, []string{"--remote", "https://github.com/octo/repo", "--ref", "refs/heads/main", "--sha", "ccc", "--new=true"})

	if acts := pushRows(t, stores, conversationID); len(acts) != 1 {
		t.Fatalf("got %d branch_pushed rows, want 1", len(acts))
	}
}

// TestRecordPush_SkipsNonBranchRef confirms a tag (or any non refs/heads ref)
// is not recorded as a branch push.
func TestRecordPush_SkipsNonBranchRef(t *testing.T) {
	stores, conversationID := newTestStores(t)
	host := hostFor(t, stores, conversationID, false)

	runRecordPush(host, []string{"--remote", "https://github.com/octo/repo", "--ref", "refs/tags/v1.0", "--sha", "ddd", "--new=true"})

	if acts := pushRows(t, stores, conversationID); len(acts) != 0 {
		t.Fatalf("got %d branch_pushed rows, want 0 (tags are not branches)", len(acts))
	}
}

// TestRecordPush_GHESHostRecordsAndAnchorsURL is the end-to-end proof for the
// local-mode GHES path: with the org's GitHub base set to a GHES host, a push to
// that host is captured (the gate accepts it) AND the recorded branch link is
// anchored to the GHES host, not github.com.
func TestRecordPush_GHESHostRecordsAndAnchorsURL(t *testing.T) {
	stores, conversationID := newTestStores(t)
	dbtest.SeedOrgSettings(t, stores.Orgs, runmode.LocalDefaultOrgID,
		domain.OrgSettings{GitHubBaseURL: "https://github.corp.example.com"})
	registerOctoRepo(t, stores, "https://github.corp.example.com")
	host := hostFor(t, stores, conversationID, false)

	runRecordPush(host, []string{
		"--remote", "https://github.corp.example.com/octo/repo.git",
		"--ref", "refs/heads/feature/x",
		"--sha", "abc123",
		"--new=true",
	})

	acts := pushRows(t, stores, conversationID)
	if len(acts) != 1 {
		t.Fatalf("got %d branch_pushed rows, want 1 (a GHES push must be captured in local mode)", len(acts))
	}
	if want := "https://github.corp.example.com/octo/repo/tree/feature/x"; acts[0].URL != want {
		t.Errorf("url = %q, want %q (anchored to the GHES host, not github.com)", acts[0].URL, want)
	}
}

// TestRecordPush_GHESOrgSkipsForeignHost proves the gate still excludes a push
// to a host that isn't the org's GitHub — here github.com from a GHES org — so
// an unrelated remote is never recorded (and never mis-anchored to the GHES
// host).
func TestRecordPush_GHESOrgSkipsForeignHost(t *testing.T) {
	stores, conversationID := newTestStores(t)
	dbtest.SeedOrgSettings(t, stores.Orgs, runmode.LocalDefaultOrgID,
		domain.OrgSettings{GitHubBaseURL: "https://github.corp.example.com"})
	host := hostFor(t, stores, conversationID, false)

	runRecordPush(host, []string{"--remote", "https://github.com/octo/repo", "--ref", "refs/heads/main", "--sha", "xyz", "--new=true"})

	if acts := pushRows(t, stores, conversationID); len(acts) != 0 {
		t.Fatalf("got %d branch_pushed rows, want 0 (github.com is not the GHES org's host)", len(acts))
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
		// Base unresolved (""): no public host is the org's.
		{"https://github.com/octo/repo", "", "", "", false},
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
