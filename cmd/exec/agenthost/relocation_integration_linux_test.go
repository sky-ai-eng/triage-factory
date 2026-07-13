//go:build linux && integration

package agenthost

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/sidecarproto"
)

// TestIntegration_RelocatedAgentHost_ExecVerbRoundTrip is the real-infra E2E for
// the relocation: it stands up the exec-verb socket server EXACTLY the way the
// capless credential sidecar does — NewServerWithRuntime over a relay runtime,
// hosted via StartWithServer on the fixed /run/tf/<runID>.sock with the sandbox
// grant — backed by a RelayServer on the other end of a live supervision
// channel. A real IPCClient (the same client the jailed agent's `exec` verbs
// use) then dials that socket and drives:
//
//  1. LookupRun — proves the relocated Server reports the run identity the
//     orchestrator handed it (AgentHostInfo), served entirely in the sidecar
//     half with no db.Stores.
//  2. TeamTracksRepo — proves a DB-backed verb RELAYS across the supervision
//     channel to the orchestrator's RelayServer, which serves it against the
//     real stores under its OWN RunInfo, and the answer round-trips back
//     through the socket.
//
// This exercises the full relocated data path — IPCClient → sidecar-hosted
// Server → relay runtime → supervision channel → RelayServer → stores → back —
// over the real /run/tf socket + grant. It is root-gated (the socket lives
// under /run/tf and is chowned to the sandbox uid), run by
// scripts/test-sandbox-linux.sh. The gVisor CONNECT mechanism (bind-mount +
// --host-uds + a jailed agent dialing /run/tf.sock) is validated separately by
// internal/sandbox's TestIntegration_AgentHostIPC_RoundTrip; together they
// cover socket relocation end to end.
func TestIntegration_RelocatedAgentHost_ExecVerbRoundTrip(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("relocation E2E needs root: it creates the socket under /run/tf and chowns it to the sandbox uid")
	}

	stores, info := newCaptureStores(t, true)

	// The orchestrator end of the supervision channel: route the relay envelope
	// to the RelayServer exactly as internal/agentproc's sidecarSupervisor does.
	relaySrv := NewRelayServer(stores, info, nil)
	orchEnd, sidecarEnd := net.Pipe()
	orchConn := sidecarproto.New(orchEnd, sidecarproto.HandlerFunc(func(ctx context.Context, kind sidecarproto.Kind, body json.RawMessage) (any, error) {
		switch kind {
		case sidecarproto.KindRelayCall:
			var env sidecarproto.RelayCallBody
			if err := json.Unmarshal(body, &env); err != nil {
				return nil, err
			}
			return relaySrv.DispatchCall(ctx, env.Namespace, env.Op, env.Args)
		case sidecarproto.KindRelayNotify:
			var env sidecarproto.RelayCallBody
			if err := json.Unmarshal(body, &env); err != nil {
				return nil, err
			}
			relaySrv.DispatchNotify(ctx, env.Namespace, env.Op, env.Args)
			return nil, nil
		default:
			return nil, nil
		}
	}))
	sidecarConn := sidecarproto.New(sidecarEnd, nil)
	t.Cleanup(func() {
		_ = orchConn.Close()
		_ = sidecarConn.Close()
		_ = orchEnd.Close()
		_ = sidecarEnd.Close()
	})

	// Host the exec-verb socket server the way the sidecar does: a relay-backed
	// Server on the real /run/tf socket with the sandbox grant.
	agentSrv := NewServerWithRuntime(NewRelayRuntime(sidecarConn, info, nil), nil)
	hd, mount, err := StartWithServer(agentSrv, info.RunID)
	if err != nil {
		t.Fatalf("StartWithServer (relocated host): %v", err)
	}
	t.Cleanup(func() { _ = hd.Close() })
	if mount.Destination != DefaultSocketPath {
		t.Fatalf("mount destination = %q, want %q", mount.Destination, DefaultSocketPath)
	}

	client := Dial(hd.SocketPath())
	t.Cleanup(func() { _ = client.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// (1) LookupRun — the relocated Server reports the run's own identity.
	got, err := client.LookupRun(ctx)
	if err != nil {
		t.Fatalf("LookupRun over the relocated socket: %v", err)
	}
	if got.RunID != info.RunID || got.OrgID != info.OrgID {
		t.Fatalf("LookupRun = %+v, want run %s / org %s", got, info.RunID, info.OrgID)
	}

	// (2) TeamTracksRepo — a DB verb relays to the orchestrator's RelayServer and
	// the answer round-trips back (un-seeded repo → not tracked, no error).
	tracks, err := client.TeamTracksRepo(ctx, "octo", "repo")
	if err != nil {
		t.Fatalf("TeamTracksRepo relay over the relocated socket: %v", err)
	}
	if tracks {
		t.Fatal("expected the un-seeded repo to be untracked through the relocated relay path")
	}
}
