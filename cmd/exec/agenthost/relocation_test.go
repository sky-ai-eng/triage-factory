package agenthost

import (
	"context"
	"encoding/json"
	"net"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/sidecarproto"
)

// TestRelocation_RelayRuntimeOverRealConn exercises the exact data path the
// relocated sidecar uses: a relayRuntime built with NewRelayRuntime over a live
// sidecarproto.Conn, talking to a RelayServer wired as the orchestrator serves
// the relay envelope (the same routing internal/agentproc's supervisor does).
// It proves a DB effect the sidecar-hosted exec verbs perform round-trips over
// the real wire protocol (JSON frames, Call/response correlation) and lands in
// the orchestrator's stores under the orchestrator's identity — no db.Stores or
// secret store ever in the sidecar half.
func TestRelocation_RelayRuntimeOverRealConn(t *testing.T) {
	stores, info := newCaptureStores(t, true)
	srv := NewRelayServer(stores, info, nil)

	orchEnd, sidecarEnd := net.Pipe()
	// The orchestrator end routes the relay envelope to the RelayServer, exactly
	// as agentproc's sidecarSupervisor does for a real supervision channel.
	orchHandler := sidecarproto.HandlerFunc(func(ctx context.Context, kind sidecarproto.Kind, body json.RawMessage) (any, error) {
		switch kind {
		case sidecarproto.KindRelayCall:
			var env sidecarproto.RelayCallBody
			if err := json.Unmarshal(body, &env); err != nil {
				return nil, err
			}
			return srv.DispatchCall(ctx, env.Namespace, env.Op, env.Args)
		case sidecarproto.KindRelayNotify:
			var env sidecarproto.RelayCallBody
			if err := json.Unmarshal(body, &env); err != nil {
				return nil, err
			}
			srv.DispatchNotify(ctx, env.Namespace, env.Op, env.Args)
			return nil, nil
		default:
			return nil, nil
		}
	})
	orchConn := sidecarproto.New(orchEnd, orchHandler)
	sidecarConn := sidecarproto.New(sidecarEnd, nil)
	t.Cleanup(func() {
		_ = orchConn.Close()
		_ = sidecarConn.Close()
		_ = orchEnd.Close()
		_ = sidecarEnd.Close()
	})

	// The sidecar's runtime — the real exported constructor, over the real conn.
	rt := NewRelayRuntime(sidecarConn, info, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	// A read op relays over the wire and returns the orchestrator's answer.
	tracks, err := rt.TeamTracksRepo(ctx, "octo", "repo")
	if err != nil {
		t.Fatalf("TeamTracksRepo over wire: %v", err)
	}
	if tracks {
		t.Fatal("expected the un-seeded repo to be untracked")
	}

	// A write op (upsert_artifact) relays over the wire and lands in the store.
	stored, err := rt.UpsertArtifact(ctx, domain.Artifact{
		Provider:   domain.ArtifactProviderGitHub,
		Kind:       domain.ArtifactKindComment,
		Target:     "octo/repo#5",
		ExternalID: "5",
		State:      domain.ArtifactStateCommentPosted,
		DedupKey:   domain.ArtifactDedupKey(domain.ArtifactProviderGitHub, domain.ArtifactKindComment, "5", ""),
	})
	if err != nil {
		t.Fatalf("UpsertArtifact over wire: %v", err)
	}
	if stored.ID == "" {
		t.Fatal("expected a stored artifact id back over the wire")
	}
	if stored.OrgID != info.OrgID {
		t.Fatalf("stored org = %q, want %q (bound orchestrator-side)", stored.OrgID, info.OrgID)
	}

	arts, err := rt.ListRunArtifacts(ctx)
	if err != nil {
		t.Fatalf("ListRunArtifacts over wire: %v", err)
	}
	found := false
	for _, a := range arts {
		if a.ID == stored.ID {
			found = true
		}
	}
	if !found {
		t.Fatal("the relayed write did not land in the orchestrator's store")
	}
}
