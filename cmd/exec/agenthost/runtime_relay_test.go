package agenthost

import (
	"context"
	"encoding/json"
	"testing"

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

func (c directDispatchConn) notify(namespace, op string, args any) {
	raw, _ := json.Marshal(args)
	c.srv.DispatchNotify(context.Background(), namespace, op, raw)
}

func TestRelayRuntime_RoundTripsCoreOps(t *testing.T) {
	stores, info := newCaptureStores(t, true)
	srv := NewRelayServer(stores, info, nil)
	rt := newRelayRuntime(directDispatchConn{srv: srv}, info, nil)
	ctx := context.Background()

	// A write op relays as one op (the tx stays orchestrator-side) and lands in
	// the store; the stored row — with its minted id and the org bound from the
	// orchestrator's RunInfo — comes back.
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
		t.Fatalf("stored org = %q, want %q (bound from RunInfo, not the args)", stored.OrgID, info.OrgID)
	}

	// A read op relays and returns the just-written row.
	arts, err := rt.ListRunArtifacts(ctx)
	if err != nil {
		t.Fatalf("ListRunArtifacts relay: %v", err)
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
	tracks, err := rt.TeamTracksRepo(ctx, "octo", "repo")
	if err != nil {
		t.Fatalf("TeamTracksRepo relay: %v", err)
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

	rt.RecordReadTouch(ctx, domain.ArtifactProviderGitHub, "octo/repo#7", "")

	ent, err := stores.Entities.GetBySource(ctx, info.OrgID, "github", "octo/repo#7")
	if err != nil || ent == nil {
		t.Fatalf("relayed read touch did not resolve-or-create the entity: ent=%v err=%v", ent, err)
	}
	if role := touchRole(t, conn, info.RunID, ent.ID); role != domain.MemoryRoleTouched {
		t.Errorf("relayed touch role = %q, want %q", role, domain.MemoryRoleTouched)
	}
}

// TestRelayServer_BindsOrgFromRunInfoNotArgs is the org-scoping guarantee: the
// relay envelope carries no org id, so the orchestrator binds identity from the
// RelayServer's own RunInfo. A sidecar runtime that believes it is a DIFFERENT
// org (a compromised or confused sidecar) still can only touch the org the
// orchestrator sealed the run to — the write lands under the SERVER's org, not
// the runtime's.
func TestRelayServer_BindsOrgFromRunInfoNotArgs(t *testing.T) {
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
		t.Fatalf("write bound the org from the sidecar's runtime (%q); it MUST bind from the orchestrator's RunInfo (%q)",
			stored.OrgID, info.OrgID)
	}
}
