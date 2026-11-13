package agenthost

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
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

// TestRelay_RecordGHRead_RepositoryOutsideTheRunIsNotTouched: in multi mode the
// ids were read by the run's sidecar and relayed, so a read links only an
// object in a repository the run works in, the rule a relayed write's link
// follows. A read of anything else creates no entity and no link.
func TestRelay_RecordGHRead_RepositoryOutsideTheRunIsNotTouched(t *testing.T) {
	conn, stores, info := newCaptureStoresConn(t, true)
	runmode.SetForTest(t, runmode.ModeMulti)
	ctx := context.Background()
	ownID := registerRepo(t, stores, "octo", "repo", "1")
	registerRepo(t, stores, "octo", "other", "2")
	if _, _, err := stores.ConversationWorktrees.InsertSystem(ctx, info.OrgID, domain.ConversationWorktree{
		ConversationID: info.ConversationID, RepositoryID: ownID, Ref: "refs/heads/main", Path: "/runs/octo/repo/main",
	}); err != nil {
		t.Fatalf("seed the run's worktree: %v", err)
	}
	srv := NewRelayServer(stores, info, nil)

	relayGHRead(t, srv, agentproc.RecordGHReadArgs{
		Owner: "octo", Repo: "repo", Number: 7,
		RepositoryID: "1", RepositoryName: "octo/repo", ObjectNodeID: "PR_7", ObjectKind: domain.EntityKindPullRequest,
	})
	relayGHRead(t, srv, agentproc.RecordGHReadArgs{
		Owner: "octo", Repo: "other", Number: 4,
		RepositoryID: "2", RepositoryName: "octo/other", ObjectNodeID: "PR_other4", ObjectKind: domain.EntityKindPullRequest,
	})

	own, err := stores.Entities.GetBySourceSystem(ctx, info.OrgID, "github", testScope("github"), "octo/repo#7")
	if err != nil || own == nil {
		t.Fatalf("a read in the run's repository resolved no entity: ent=%v err=%v", own, err)
	}
	if role := touchRole(t, conn, info.ConversationID, own.ID); role != domain.MemoryRoleTouched {
		t.Errorf("link role in the run's repository = %q, want %q", role, domain.MemoryRoleTouched)
	}
	if e, err := stores.Entities.GetBySourceSystem(ctx, info.OrgID, "github", testScope("github"), "octo/other#4"); err != nil || e != nil {
		t.Errorf("octo/other#4 = %+v, %v; want no entity for a repository the run does not work in", e, err)
	}
}
