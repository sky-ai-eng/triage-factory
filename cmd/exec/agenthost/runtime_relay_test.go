package agenthost

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// directDispatchConn wires a relayRuntime straight to a RelayServer in-memory
// (marshaling through JSON exactly as the wire would), so a test exercises the
// full relay round-trip — runtime → envelope → orchestrator op → db.Stores —
// without standing up a supervision socket.
type directDispatchConn struct{ srv *RelayServer }

func (c directDispatchConn) call(ctx context.Context, namespace, op string, args, out any) error {
	raw, err := json.Marshal(args)
	if err != nil {
		return err
	}
	res, err := c.srv.DispatchCall(ctx, namespace, op, raw)
	if err != nil {
		return err
	}
	if out == nil || len(res) == 0 {
		return nil
	}
	return json.Unmarshal(res, out)
}

func TestRelayRuntime_InsertConversationWorktreeBindsConversationIdentity(t *testing.T) {
	conn, stores, info := newCaptureStoresConn(t, true)
	repositoryID := uuid.New().String()
	if _, err := conn.Exec(`INSERT INTO repositories (id, source, host, owner, repo) VALUES (?, 'github', ?, 'octocat', 'relay')`, repositoryID, dbtest.TestGitHubHost); err != nil {
		t.Fatalf("seed repository: %v", err)
	}
	srv := NewRelayServer(stores, info, nil)
	rt := newRelayRuntime(directDispatchConn{srv: srv}, info, nil)

	inserted, _, err := rt.InsertConversationWorktree(context.Background(), domain.ConversationWorktree{
		ConversationID: "conversation-from-wire", RepositoryID: repositoryID,
		Path: "/tmp/relay", Ref: "default",
	})
	if err != nil || !inserted {
		t.Fatalf("InsertConversationWorktree relay: inserted=%v err=%v", inserted, err)
	}
	got, err := stores.ConversationWorktrees.ListSystem(context.Background(), info.OrgID, info.ConversationID)
	if err != nil {
		t.Fatalf("ListSystem: %v", err)
	}
	if len(got) != 1 || got[0].ConversationID != info.ConversationID {
		t.Fatalf("stored worktrees = %+v, want conversation %q", got, info.ConversationID)
	}
}

func (c directDispatchConn) record(namespace, op string, args any) {
	raw, _ := json.Marshal(args)
	_ = c.srv.DispatchRecord(context.Background(), namespace, op, uuid.NewString(), raw)
}

func TestRelayRuntime_RoundTripsCoreOps(t *testing.T) {
	stores, info := newCaptureStores(t, true)
	srv := NewRelayServer(stores, info, nil)
	rt := newRelayRuntime(directDispatchConn{srv: srv}, info, nil)
	ctx := context.Background()

	// A write op relays as one op (the tx stays orchestrator-side) and lands in
	// the store; the stored row — with its minted id and the org bound from the
	// orchestrator's ConversationInfo — comes back.
	art := domain.Artifact{
		Provider:   domain.ArtifactProviderGitHub,
		Kind:       domain.ArtifactKindComment,
		Target:     "octo/repo#7",
		ExternalID: "123",
		State:      domain.ArtifactStateCommentPosted,
		DedupKey:   domain.ArtifactDedupKey(domain.ArtifactProviderGitHub, domain.ArtifactKindComment, "123", ""),
	}
	stored, err := rt.UpsertArtifact(ctx, art)
	if err != nil {
		t.Fatalf("UpsertArtifact relay: %v", err)
	}
	if stored.ID == "" {
		t.Fatal("expected a stored artifact id back from the relay")
	}
	if stored.OrgID != info.OrgID {
		t.Fatalf("stored org = %q, want %q (bound from ConversationInfo, not the args)", stored.OrgID, info.OrgID)
	}

	// A read op relays and returns the just-written row.
	arts, err := rt.ListConversationArtifacts(ctx)
	if err != nil {
		t.Fatalf("ListConversationArtifacts relay: %v", err)
	}
	found := false
	for _, a := range arts {
		if a.ID == stored.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("relayed UpsertArtifact did not land in the store (got %d artifacts)", len(arts))
	}

	// A pure read op relays and returns the store's answer (team-tracks gate,
	// used by the exec-gh authorize path).
	tracks, err := rt.TeamTracksRepository(ctx, "6f1d2c3b-4a5e-4f60-8a7b-9c0d1e2f3a4b")
	if err != nil {
		t.Fatalf("TeamTracksRepository relay: %v", err)
	}
	if tracks {
		t.Fatal("expected the un-seeded repo to be untracked through the relay")
	}
}

// TestRelayRuntime_RecordReadTouch_RoundTrips proves the sidecar path for an
// addressed read's touch: the capless runtime relays RecordReadTouch to the
// orchestrator, which resolves-or-creates the entity and persists the durable
// touch row against its own stores — so a read touch is not lost when the
// verb parser runs in the jail (where no db.Stores exists).
func TestRelayRuntime_RecordReadTouch_RoundTrips(t *testing.T) {
	conn, stores, info := newCaptureStoresConn(t, true)
	srv := NewRelayServer(stores, info, nil)
	rt := newRelayRuntime(directDispatchConn{srv: srv}, info, nil)
	ctx := context.Background()

	rt.RecordReadTouch(ctx, domain.ArtifactProviderGitHub, "octo/repo#7", "", domain.EntityContainer{}, "")

	ent, err := stores.Entities.GetBySource(ctx, info.OrgID, "github", "https://github.com", "octo/repo#7")
	if err != nil || ent == nil {
		t.Fatalf("relayed read touch did not resolve-or-create the entity: ent=%v err=%v", ent, err)
	}
	if role := touchRole(t, conn, info.ConversationID, ent.ID); role != domain.MemoryRoleTouched {
		t.Errorf("relayed touch role = %q, want %q", role, domain.MemoryRoleTouched)
	}
}

// TestRelayRuntime_LinearTouchCarriesTheTeam: the Linear team rides both
// relayed touches — a read's, and a write's — so a Linear entity the
// orchestrator creates for the sidecar carries it. A Linear touch that names no
// team creates nothing: a Linear entity is created only with its team.
func TestRelayRuntime_LinearTouchCarriesTheTeam(t *testing.T) {
	conn, stores, info := newCaptureStoresConn(t, true)
	if _, err := conn.Exec(`UPDATE org_settings SET linear_workspace_id = ? WHERE org_id = ?`, testScope("linear"), info.OrgID); err != nil {
		t.Fatalf("record linear workspace: %v", err)
	}
	srv := NewRelayServer(stores, info, nil)
	rt := newRelayRuntime(directDispatchConn{srv: srv}, info, nil)
	ctx := context.Background()

	rt.RecordReadTouch(ctx, domain.ArtifactProviderLinear, "ENG-1", "uuid-read", domain.EntityContainer{LinearTeamID: "team-eng"}, "")
	rt.Record(ctx, &domain.Artifact{
		Provider: domain.ArtifactProviderLinear, Kind: domain.ArtifactKindIssue,
		Target: "OPS-2", ExternalID: "uuid-write", State: domain.ArtifactStateIssueCreated,
		DedupKey: domain.ArtifactDedupKey(domain.ArtifactProviderLinear, domain.ArtifactKindIssue, "uuid-write", ""),
	}, &domain.ExternalAction{
		Provider: domain.ArtifactProviderLinear, Action: domain.ActionIssueCreated,
		Target: "OPS-2", ExternalID: "uuid-write", Credential: domain.CredentialLinearOrg,
	}, domain.EntityContainer{LinearTeamID: "team-ops"})
	rt.RecordReadTouch(ctx, domain.ArtifactProviderLinear, "ENG-3", "uuid-teamless", domain.EntityContainer{}, "")

	for uuid, team := range map[string]string{"uuid-read": "team-eng", "uuid-write": "team-ops"} {
		ent, err := stores.Entities.GetByExternalIDSystem(ctx, info.OrgID, domain.ArtifactProviderLinear, testScope("linear"), uuid)
		if err != nil || ent == nil || ent.LinearTeamID != team {
			t.Errorf("entity for %s = %+v err=%v, want one in %s", uuid, ent, err, team)
		}
	}
	if ent, err := stores.Entities.GetByExternalIDSystem(ctx, info.OrgID, domain.ArtifactProviderLinear, testScope("linear"), "uuid-teamless"); err != nil || ent != nil {
		t.Errorf("a teamless Linear touch created %+v (err=%v), want nothing", ent, err)
	}
}

// TestRelayRuntime_JiraTouchCarriesTheProject: the Jira project rides both
// relayed touches, a read's and a write's, so a Jira entity the orchestrator
// creates for the sidecar references its project's registry row, registered
// on the org's site when it had none. A Jira touch that names no project
// creates nothing: a Jira entity is created only with its project.
func TestRelayRuntime_JiraTouchCarriesTheProject(t *testing.T) {
	_, stores, info := newCaptureStoresConn(t, true)
	srv := NewRelayServer(stores, info, nil)
	rt := newRelayRuntime(directDispatchConn{srv: srv}, info, nil)
	ctx := context.Background()
	site := testScope("jira")
	eng := domain.EntityContainer{JiraProject: domain.JiraProjectRef{ID: "10100", Key: "ENG", Name: "Engineering"}}
	ops := domain.EntityContainer{JiraProject: domain.JiraProjectRef{ID: "10200", Key: "OPS"}}

	rt.RecordReadTouch(ctx, domain.ArtifactProviderJira, "ENG-1", "20001", eng, "")
	resource := domain.JiraIssueResource(site, "20002")
	rt.Record(ctx, &domain.Artifact{
		Provider: domain.ArtifactProviderJira, Kind: domain.ArtifactKindIssue,
		Target: "OPS-2", ExternalID: "20002", State: domain.ArtifactStateIssueCreated,
		DedupKey: domain.ArtifactDedupKey(domain.ArtifactProviderJira, domain.ArtifactKindIssue, resource, ""),
	}, &domain.ExternalAction{
		Provider: domain.ArtifactProviderJira, Action: domain.ActionIssueCreated,
		Target: "OPS-2", Credential: domain.CredentialJiraOrg,
	}, ops)
	rt.RecordReadTouch(ctx, domain.ArtifactProviderJira, "ENG-3", "20003", domain.EntityContainer{}, "")

	for issueID, want := range map[string]domain.JiraProjectRef{"20001": eng.JiraProject, "20002": ops.JiraProject} {
		ent, err := stores.Entities.GetByExternalIDSystem(ctx, info.OrgID, domain.ArtifactProviderJira, site, issueID)
		if err != nil || ent == nil {
			t.Fatalf("entity for %s = %+v err=%v, want one", issueID, ent, err)
		}
		project, err := stores.JiraProjects.GetSystem(ctx, info.OrgID, ent.JiraProjectID)
		if err != nil || project == nil || project.Site != site || project.ExternalID != want.ID || project.Key != want.Key {
			t.Errorf("project of %s = %+v err=%v, want %s under %s on %s", issueID, project, err, want.Key, want.ID, site)
		}
	}
	if ent, err := stores.Entities.GetByExternalIDSystem(ctx, info.OrgID, domain.ArtifactProviderJira, site, "20003"); err != nil || ent != nil {
		t.Errorf("a projectless Jira touch created %+v (err=%v), want nothing", ent, err)
	}
}

// TestRelayServer_BindsOrgFromConversationInfoNotArgs is the org-scoping guarantee: the
// relay envelope carries no org id, so the orchestrator binds identity from the
// RelayServer's own ConversationInfo. A sidecar runtime that believes it is a DIFFERENT
// org (a compromised or confused sidecar) still can only touch the org the
// orchestrator sealed the run to — the write lands under the SERVER's org, not
// the runtime's.
func TestRelayServer_BindsOrgFromConversationInfoNotArgs(t *testing.T) {
	stores, info := newCaptureStores(t, true)
	srv := NewRelayServer(stores, info, nil)

	// A runtime that thinks it is some other org, pointed at THIS run's server.
	bogus := info
	bogus.OrgID = "attacker-controlled-org"
	rt := newRelayRuntime(directDispatchConn{srv: srv}, bogus, nil)

	stored, err := rt.UpsertArtifact(context.Background(), domain.Artifact{
		Provider:   domain.ArtifactProviderGitHub,
		Kind:       domain.ArtifactKindComment,
		Target:     "octo/repo#9",
		ExternalID: "9",
		State:      domain.ArtifactStateCommentPosted,
		DedupKey:   domain.ArtifactDedupKey(domain.ArtifactProviderGitHub, domain.ArtifactKindComment, "9", ""),
	})
	if err != nil {
		t.Fatalf("UpsertArtifact relay: %v", err)
	}
	if stored.OrgID != info.OrgID {
		t.Fatalf("write bound the org from the sidecar's runtime (%q); it MUST bind from the orchestrator's ConversationInfo (%q)",
			stored.OrgID, info.OrgID)
	}
}
