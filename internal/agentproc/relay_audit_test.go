package agentproc

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/sidecarproto"
)

// relayPeer is the orchestrator end of a supervision channel: it records every
// notify it is handed so a test can assert on what actually crossed the wire.
type relayPeer struct {
	conn   *sidecarproto.Conn
	frames chan RecordRelayDropArgs
}

func (p *relayPeer) Handle(_ context.Context, kind sidecarproto.Kind, body json.RawMessage) (any, error) {
	if kind != sidecarproto.KindRelayNotify {
		return nil, nil
	}
	var env sidecarproto.RelayCallBody
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, err
	}
	if env.Namespace != RelayNamespaceCore || env.Op != OpRecordRelayDrop {
		return nil, nil
	}
	var args RecordRelayDropArgs
	if err := json.Unmarshal(env.Args, &args); err != nil {
		return nil, err
	}
	p.frames <- args
	return nil, nil
}

// newRelayPeer wires a sidecar-side Conn to a peer that captures drop reports.
func newRelayPeer(t *testing.T) (*sidecarproto.Conn, *relayPeer) {
	t.Helper()
	sidecarEnd, orchEnd := net.Pipe()
	peer := &relayPeer{frames: make(chan RecordRelayDropArgs, 4)}
	peer.conn = sidecarproto.New(orchEnd, peer)
	sidecar := sidecarproto.New(sidecarEnd, nil)
	t.Cleanup(func() {
		_ = sidecar.Close()
		_ = peer.conn.Close()
		_ = sidecarEnd.Close()
		_ = orchEnd.Close()
	})
	return sidecar, peer
}

// unmarshalable is a payload json.Marshal always rejects — the forced send
// failure that leaves the channel itself healthy, which is exactly the half
// NotifyRelayAudit exists to make countable.
type unmarshalable struct {
	Bad float64 `json:"bad"`
}

// TestNotifyRelayAudit_ReportsADropItCanStillSend: a record that never
// serializes is lost, and the loss is reported over the (healthy) channel
// naming the op that lost it — so the orchestrator counts a hole rather than
// inferring one from a warn line.
func TestNotifyRelayAudit_ReportsADropItCanStillSend(t *testing.T) {
	conn, peer := newRelayPeer(t)

	sent := make(chan error, 1)
	// net.Pipe is unbuffered: the send blocks until the peer's read loop takes
	// the frame, so it cannot run on the asserting goroutine.
	go func() {
		sent <- NotifyRelayAudit(conn, RelayNamespaceCore, OpRecordGHWrite, unmarshalable{Bad: math.NaN()})
	}()

	select {
	case err := <-sent:
		if err == nil {
			t.Fatal("NotifyRelayAudit: want the send error surfaced to the caller for its log line")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("NotifyRelayAudit never returned")
	}

	select {
	case got := <-peer.frames:
		if got.Namespace != RelayNamespaceCore || got.Op != OpRecordGHWrite {
			t.Fatalf("drop report named %s/%s; want %s/%s", got.Namespace, got.Op, RelayNamespaceCore, OpRecordGHWrite)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no drop report crossed the wire")
	}
}

// TestNotifyRelayAudit_HealthySendReportsNothing: the ordinary path sends the
// record and nothing else — a drop report on a successful relay would make the
// counter fire on every audit row.
func TestNotifyRelayAudit_HealthySendReportsNothing(t *testing.T) {
	conn, peer := newRelayPeer(t)

	sent := make(chan error, 1)
	go func() {
		sent <- NotifyRelayAudit(conn, RelayNamespaceCore, OpRecordGHWrite,
			RecordGHWriteArgs{Method: "PATCH", Path: "/repos/acme/widgets/pulls/7", Status: 200})
	}()
	select {
	case err := <-sent:
		if err != nil {
			t.Fatalf("NotifyRelayAudit: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("NotifyRelayAudit never returned")
	}

	select {
	case got := <-peer.frames:
		t.Fatalf("healthy send reported a drop: %+v", got)
	case <-time.After(200 * time.Millisecond):
	}
}

// TestNotifyRelayAudit_DeadChannelIsTheNamedResidual: once the channel is gone
// the report has no way out either, so the caller gets the error and the
// counter never learns. This is the accepted residual — the teardown race —
// and it is pinned here so it stays a known shape rather than a surprise.
func TestNotifyRelayAudit_DeadChannelIsTheNamedResidual(t *testing.T) {
	conn, peer := newRelayPeer(t)
	_ = conn.Close()

	err := NotifyRelayAudit(conn, RelayNamespaceCore, OpRecordPush, RecordPushArgs{Repo: "acme/widgets"})
	if !errors.Is(err, sidecarproto.ErrClosed) {
		t.Fatalf("NotifyRelayAudit on a closed conn = %v; want ErrClosed", err)
	}
	select {
	case got := <-peer.frames:
		t.Fatalf("a closed channel somehow carried a drop report: %+v", got)
	case <-time.After(200 * time.Millisecond):
	}
}
