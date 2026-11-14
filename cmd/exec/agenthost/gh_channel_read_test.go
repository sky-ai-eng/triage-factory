package agenthost

import (
	"context"
	"database/sql"
	"encoding/json"
	"strconv"
	"testing"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/ghcli/ghwrite"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// ghRead is one addressed read as the injector reports it: the spelling the
// request used, and GitHub's answer for it.
func ghRead(owner, repo string, number int, repositoryID, nodeID, kind string) ghwrite.ReadObservation {
	return ghwrite.ReadObservation{
		ObjectRef: ghwrite.ObjectRef{Owner: owner, Repo: repo, Number: number},
		Identity:  domain.GitHubIdentity{RepositoryID: repositoryID, Repository: owner + "/" + repo, NodeID: nodeID, Kind: kind},
	}
}

// TestRecordGHChannelRead_TouchesTheEntity: a pull request or issue read
// through the real gh is the run's touched entity, as one read through an exec
// verb is. The read mints the entity when TF has none, of the kind GitHub
// answered, and a second read of it changes nothing.
func TestRecordGHChannelRead_TouchesTheEntity(t *testing.T) {
	conn, stores, info := newCaptureStoresConn(t, true)
	ctx := context.Background()

	RecordGHChannelRead(ctx, stores, info, ghRead("octo", "repo", 7, "1", "PR_7", domain.EntityKindPullRequest))
	RecordGHChannelRead(ctx, stores, info, ghRead("octo", "repo", 7, "1", "PR_7", domain.EntityKindPullRequest))
	RecordGHChannelRead(ctx, stores, info, ghRead("octo", "repo", 9, "1", "I_9", domain.EntityKindIssue))

	for key, kind := range map[string]string{"octo/repo#7": domain.EntityKindPullRequest, "octo/repo#9": domain.EntityKindIssue} {
		ent, err := stores.Entities.GetBySource(ctx, info.OrgID, "github", testScope("github"), key)
		if err != nil || ent == nil {
			t.Fatalf("%s: the read did not resolve-or-create the entity: ent=%v err=%v", key, ent, err)
		}
		if ent.Kind != kind {
			t.Errorf("%s: entity kind = %q, want %q", key, ent.Kind, kind)
		}
		if role := touchRole(t, conn, info.ConversationID, ent.ID); role != domain.MemoryRoleTouched {
			t.Errorf("%s: link role = %q, want %q", key, role, domain.MemoryRoleTouched)
		}
	}
	var links int
	if err := conn.QueryRow(`SELECT COUNT(*) FROM conversation_memory_entities WHERE conversation_id = ?`, info.ConversationID).Scan(&links); err != nil {
		t.Fatalf("count links: %v", err)
	}
	if links != 2 {
		t.Errorf("links = %d, want one per object read", links)
	}
	if acts := listExternalActions(t, stores); len(acts) != 0 {
		t.Errorf("a read left ledger rows %+v, want none: a read is no act", acts)
	}
}

// TestRecordGHChannelRead_ResolvesByIdentityNotSpelling: the entity is the one
// GitHub's node id names, whatever name the request spelled its repository by,
// and an observation with no node id touches nothing.
func TestRecordGHChannelRead_ResolvesByIdentityNotSpelling(t *testing.T) {
	conn, stores, info := newCaptureStoresConn(t, true)
	ctx := context.Background()
	existing, _, err := stores.Entities.FindOrCreateSystem(ctx, info.OrgID, "github", testScope("github"), "octo/repo#7", "PR_7", domain.EntityKindPullRequest, "", "")
	if err != nil {
		t.Fatalf("seed the pull request: %v", err)
	}

	// The request spelled the repository's former name.
	read := ghRead("octo", "old-name", 7, "1", "PR_7", domain.EntityKindPullRequest)
	read.Identity.Repository = "octo/repo"
	RecordGHChannelRead(ctx, stores, info, read)
	if role := touchRole(t, conn, info.ConversationID, existing.ID); role != domain.MemoryRoleTouched {
		t.Errorf("link role on the entity carrying the node id = %q, want %q", role, domain.MemoryRoleTouched)
	}
	if e, err := stores.Entities.GetBySourceSystem(ctx, info.OrgID, "github", testScope("github"), "octo/old-name#7"); err != nil || e != nil {
		t.Errorf("octo/old-name#7 = %+v, %v; want no second entity under the spelling", e, err)
	}

	RecordGHChannelRead(ctx, stores, info, ghRead("octo", "repo", 8, "1", "", ""))
	if e, err := stores.Entities.GetBySourceSystem(ctx, info.OrgID, "github", testScope("github"), "octo/repo#8"); err != nil || e != nil {
		t.Errorf("octo/repo#8 = %+v, %v; want nothing from a read with no node id", e, err)
	}
}

// TestRecordGHChannelRead_DoesNotLowerAProducedLink: a read of an object the
// run already changed leaves its link as produced.
func TestRecordGHChannelRead_DoesNotLowerAProducedLink(t *testing.T) {
	conn, stores, info := newCaptureStoresConn(t, true)
	ctx := context.Background()
	ent, _, err := stores.Entities.FindOrCreateSystem(ctx, info.OrgID, "github", testScope("github"), "octo/repo#7", "PR_7", domain.EntityKindPullRequest, "", "")
	if err != nil {
		t.Fatalf("seed the pull request: %v", err)
	}
	if err := stores.TaskMemory.RecordEntityTouchSystem(ctx, info.OrgID, info.ConversationID, ent.ID, domain.MemoryRoleProduced); err != nil {
		t.Fatalf("seed the produced link: %v", err)
	}

	RecordGHChannelRead(ctx, stores, info, ghRead("octo", "repo", 7, "1", "PR_7", domain.EntityKindPullRequest))
	if role := touchRole(t, conn, info.ConversationID, ent.ID); role != domain.MemoryRoleProduced {
		t.Errorf("link role = %q, want it left at %q", role, domain.MemoryRoleProduced)
	}
}

// relayGHRead applies one record_gh_read record to srv the way the supervision
// channel delivers it.
func relayGHRead(t *testing.T, srv *RelayServer, args agentproc.RecordGHReadArgs) {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshal record_gh_read: %v", err)
	}
	if err := srv.DispatchRecord(context.Background(), agentproc.RelayNamespaceCore, agentproc.OpRecordGHRead, uuid.NewString(), raw); err != nil {
		t.Fatalf("dispatch record_gh_read: %v", err)
	}
}

// TestRelay_RecordGHRead_TouchesTheEntity proves the sandbox path: the
// sidecar's injector relays the read as record_gh_read, and the orchestrator
// resolves the entity from the ids it carries and records the touch against
// its own stores.
func TestRelay_RecordGHRead_TouchesTheEntity(t *testing.T) {
	conn, stores, info := newCaptureStoresConn(t, true)
	srv := NewRelayServer(stores, info, nil)
	ctx := context.Background()

	relayGHRead(t, srv, agentproc.RecordGHReadArgs{
		Owner: "octo", Repo: "repo", Number: 7,
		RepositoryID: "1", RepositoryName: "octo/repo", ObjectNodeID: "PR_7", ObjectKind: domain.EntityKindPullRequest,
	})

	ent, err := stores.Entities.GetBySource(ctx, info.OrgID, "github", testScope("github"), "octo/repo#7")
	if err != nil || ent == nil {
		t.Fatalf("relayed read did not resolve-or-create the entity: ent=%v err=%v", ent, err)
	}
	if role := touchRole(t, conn, info.ConversationID, ent.ID); role != domain.MemoryRoleTouched {
		t.Errorf("relayed link role = %q, want %q", role, domain.MemoryRoleTouched)
	}
}

// trackRepos makes ids the tracked repositories of the run's team.
func trackRepos(t *testing.T, stores db.Stores, info ConversationInfo, ids ...string) {
	t.Helper()
	if err := stores.TeamGitHubRepos.ReplaceForTeam(context.Background(), info.OrgID, info.TeamID, testScope("github"), ids); err != nil {
		t.Fatalf("track repositories: %v", err)
	}
}

// relayedPullRead relays a read of the pull request numbered number in
// owner/repo, whose repository GitHub calls repositoryID, and reports whether
// the run now has a touched link to its entity.
func relayedPullRead(t *testing.T, conn *sql.DB, stores db.Stores, info ConversationInfo, srv *RelayServer, owner, repo string, number int, repositoryID string) bool {
	t.Helper()
	slug := owner + "/" + repo
	relayGHRead(t, srv, agentproc.RecordGHReadArgs{
		Owner: owner, Repo: repo, Number: number,
		RepositoryID: repositoryID, RepositoryName: slug,
		ObjectNodeID: "PR_" + repo + strconv.Itoa(number), ObjectKind: domain.EntityKindPullRequest,
	})
	ent, err := stores.Entities.GetBySourceSystem(context.Background(), info.OrgID, "github", testScope("github"), domain.PullRequestTarget(slug, number))
	if err != nil {
		t.Fatalf("read the entity of %s#%d: %v", slug, number, err)
	}
	return ent != nil && touchRole(t, conn, info.ConversationID, ent.ID) == domain.MemoryRoleTouched
}

// TestRelay_RecordGHRead_AnchoredRunTouchesOnlyItsRepositories: in multi mode
// the ids were read by the run's sidecar and relayed, so a read links only an
// object the run's credential could have read. A run that works in a
// repository holds a credential for the repositories it works in and no
// other, so a read relayed for another repository, one its team tracks
// included, creates no entity and no link.
func TestRelay_RecordGHRead_AnchoredRunTouchesOnlyItsRepositories(t *testing.T) {
	conn, stores, info := newCaptureStoresConn(t, true)
	runmode.SetForTest(t, runmode.ModeMulti)
	ctx := context.Background()
	ownID := registerRepo(t, stores, "octo", "repo", "1")
	otherID := registerRepo(t, stores, "octo", "other", "2")
	trackRepos(t, stores, info, ownID, otherID)
	if _, _, err := stores.ConversationWorktrees.InsertSystem(ctx, info.OrgID, domain.ConversationWorktree{
		ConversationID: info.ConversationID, RepositoryID: ownID, Ref: "refs/heads/main", Path: "/runs/octo/repo/main",
	}); err != nil {
		t.Fatalf("seed the run's worktree: %v", err)
	}
	srv := NewRelayServer(stores, info, nil)

	if !relayedPullRead(t, conn, stores, info, srv, "octo", "repo", 7, "1") {
		t.Error("a read in the repository the run works in left no touched link")
	}
	if relayedPullRead(t, conn, stores, info, srv, "octo", "other", 4, "2") {
		t.Error("a read relayed for a tracked repository the run's credential does not reach was linked")
	}
	if e, err := stores.Entities.GetBySourceSystem(ctx, info.OrgID, "github", testScope("github"), "octo/other#4"); err != nil || e != nil {
		t.Errorf("octo/other#4 = %+v, %v; want no entity", e, err)
	}
}

// TestRelay_RecordGHRead_UnanchoredRunTouchesWhatItsTeamTracks: a run with no
// GitHub anchor, one a Jira issue or a chat message started, works in no
// repository, and its credential reads every repository its team tracks. A
// read it relays is linked in any of them and in no other. What the run may
// file an act under is unchanged: with no repository it works in, a relayed
// write stays unlinked.
func TestRelay_RecordGHRead_UnanchoredRunTouchesWhatItsTeamTracks(t *testing.T) {
	conn, stores, info := newCaptureStoresConn(t, true)
	runmode.SetForTest(t, runmode.ModeMulti)
	ctx := context.Background()
	trackedID := registerRepo(t, stores, "octo", "repo", "1")
	registerRepo(t, stores, "octo", "untracked", "2")
	trackRepos(t, stores, info, trackedID)
	srv := NewRelayServer(stores, info, nil)

	if !relayedPullRead(t, conn, stores, info, srv, "octo", "repo", 7, "1") {
		t.Error("a read in a repository the run's team tracks left no touched link")
	}
	if relayedPullRead(t, conn, stores, info, srv, "octo", "untracked", 4, "2") {
		t.Error("a read relayed for a repository the run's team does not track was linked")
	}

	// The exec verbs' relayed read touch passes the same gate.
	rt := newRelayRuntime(directDispatchConn{srv: srv}, info, nil)
	rt.RecordReadTouch(ctx, domain.ProviderGitHub, "octo/repo#8", "", domain.EntityContainer{},
		domain.GitHubIdentity{RepositoryID: "1", Repository: "octo/repo", NodeID: "PR_repo8", Kind: domain.EntityKindPullRequest}, "")
	if e, err := stores.Entities.GetBySourceSystem(ctx, info.OrgID, "github", testScope("github"), "octo/repo#8"); err != nil || e == nil {
		t.Errorf("octo/repo#8 = %+v, %v; want the entity a relayed exec read touched", e, err)
	}

	act := domain.ExternalAction{Provider: domain.ProviderGitHub, Action: domain.ActionLabelAdded, Target: "octo/repo#7", Credential: domain.CredentialGitHubApp}
	recordExternalWrite(ctx, stores, srv.info, nil, &act, domain.EntityContainer{},
		domain.GitHubIdentity{RepositoryID: "1", Repository: "octo/repo", NodeID: "PR_repo7", Kind: domain.EntityKindPullRequest})
	acts := listExternalActions(t, stores)
	if len(acts) != 1 || acts[0].EntityID != "" || acts[0].RepositoryID != "" {
		t.Errorf("a relayed write in a repository the run does not work in = %+v, want it recorded unlinked", acts)
	}
}

// TestRelay_RecordGHRead_ReadsACheckoutOfItsTasksOtherConversations: a task's
// conversations share one run tree, and the run's credential covers the
// repositories any of them checked out. A read in one of those is linked,
// though this conversation holds no worktree row for it.
func TestRelay_RecordGHRead_ReadsACheckoutOfItsTasksOtherConversations(t *testing.T) {
	conn, stores, info := newCaptureStoresConn(t, true)
	runmode.SetForTest(t, runmode.ModeMulti)
	ctx := context.Background()
	ownID := registerRepo(t, stores, "octo", "repo", "1")
	siblingID := registerRepo(t, stores, "octo", "other", "2")
	elsewhereID := registerRepo(t, stores, "octo", "elsewhere", "3")
	trackRepos(t, stores, info, ownID, siblingID, elsewhereID)

	const sibling = "44444444-4444-4444-4444-444444444444"
	for _, stmt := range []struct {
		query string
		args  []any
	}{
		{`INSERT INTO entities (id, source, source_id, kind, scope, repository_id) VALUES ('ent-1', 'github', 'octo/repo#1', 'pr', ?, ?)`, []any{testScope("github"), ownID}},
		{`INSERT INTO events (id, event_type, entity_id) VALUES ('evt-1', 'github:pr:ci_check_failed', 'ent-1')`, nil},
		{`INSERT INTO tasks (id, entity_id, event_type, primary_event_id) VALUES ('task-1', 'ent-1', 'github:pr:ci_check_failed', 'evt-1')`, nil},
		{`UPDATE conversations SET task_id = 'task-1' WHERE id = ?`, []any{info.ConversationID}},
		{`INSERT INTO conversations (id, origin, status, task_id) VALUES (?, 'interactive', 'running', 'task-1')`, []any{sibling}},
	} {
		if _, err := conn.Exec(stmt.query, stmt.args...); err != nil {
			t.Fatalf("seed (%s): %v", stmt.query, err)
		}
	}
	if _, _, err := stores.ConversationWorktrees.InsertSystem(ctx, info.OrgID, domain.ConversationWorktree{
		ConversationID: sibling, RepositoryID: siblingID, Ref: "refs/heads/main", Path: "/runs/octo/other/main",
	}); err != nil {
		t.Fatalf("seed the other conversation's worktree: %v", err)
	}
	srv := NewRelayServer(stores, info, nil)

	if !relayedPullRead(t, conn, stores, info, srv, "octo", "other", 4, "2") {
		t.Error("a read in a repository another conversation of the task checked out left no touched link")
	}
	if relayedPullRead(t, conn, stores, info, srv, "octo", "elsewhere", 5, "3") {
		t.Error("a read relayed for a tracked repository no conversation of the task works in was linked")
	}
}
