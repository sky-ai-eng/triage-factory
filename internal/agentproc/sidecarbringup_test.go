package agentproc

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/sky-ai-eng/triage-factory/internal/credbundle"
	"github.com/sky-ai-eng/triage-factory/internal/credseal"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/sidecarproto"
)

// fakeLaunchedSidecar is a sandbox.LaunchedSidecar backed by one end of an
// in-memory pipe; the test drives the other end as the sidecar.
type fakeLaunchedSidecar struct{ conn net.Conn }

func (f *fakeLaunchedSidecar) Supervision() net.Conn { return f.conn }
func (f *fakeLaunchedSidecar) Close() error          { return f.conn.Close() }

// scriptedSidecar drives the sidecar side of the supervision channel with the
// real protocol + real sealed-box crypto: it announces a per-run key, unseals
// what the orchestrator relays, and answers StartProxies. It optionally calls
// the orchestrator back to authorize a repo, exercising the reverse direction.
type scriptedSidecar struct {
	kp          *credseal.KeyPair
	conn        *sidecarproto.Conn
	gotBundle   chan *credbundle.Bundle
	startEnv    []string
	authorizeCB func(t *testing.T)
}

func (s *scriptedSidecar) Handle(_ context.Context, kind sidecarproto.Kind, body json.RawMessage) (any, error) {
	switch kind {
	case sidecarproto.KindSealedBundle:
		var msg sidecarproto.SealedBundleBody
		if err := json.Unmarshal(body, &msg); err != nil {
			return nil, err
		}
		plaintext, err := s.kp.Open(msg.Sealed)
		if err != nil {
			return nil, err
		}
		b, err := credbundle.Unmarshal(plaintext)
		if err != nil {
			return nil, err
		}
		s.gotBundle <- b
		return nil, nil
	case sidecarproto.KindStartProxies:
		return sidecarproto.StartProxiesResult{Env: s.startEnv}, nil
	default:
		return nil, nil
	}
}

func TestBringUpSidecar_HandshakeAndProxyEnv(t *testing.T) {
	kp, err := credseal.GenerateKeyPair()
	if err != nil {
		t.Fatalf("keypair: %v", err)
	}
	orchEnd, sidecarEnd := net.Pipe()
	sc := &scriptedSidecar{
		kp:        kp,
		gotBundle: make(chan *credbundle.Bundle, 1),
		startEnv:  []string{"ANTHROPIC_BASE_URL=http://10.42.9.1:5000", "ANTHROPIC_API_KEY=sk-ant-placeholder"},
	}
	sc.conn = sidecarproto.New(sidecarEnd, sc)
	// The sidecar announces its key first, exactly like the real runtime.
	// Fire it concurrently: net.Pipe is unbuffered, so the write blocks until
	// bringUpRunSidecar (below) constructs the orchestrator Conn and starts
	// reading — a real broker socket is buffered and does not.
	go func() {
		_ = sc.conn.Notify(sidecarproto.KindHello, sidecarproto.HelloBody{PubKey: base64.StdEncoding.EncodeToString(kp.Public[:])})
	}()
	t.Cleanup(func() { _ = sc.conn.Close(); _ = sidecarEnd.Close() })

	// The orchestrator's provision closure stands in for the brain: it seals a
	// bundle to whatever public key the sidecar announced.
	provision := func(_ context.Context, pubKeyB64 string) ([]byte, int64, error) {
		pubBytes, err := base64.StdEncoding.DecodeString(pubKeyB64)
		if err != nil {
			return nil, 0, err
		}
		var pub [32]byte
		copy(pub[:], pubBytes)
		plaintext, err := (&credbundle.Bundle{BootEpoch: 3, LLM: map[string]string{"ANTHROPIC_API_KEY": "sk-ant-REAL"}}).Marshal()
		if err != nil {
			return nil, 0, err
		}
		sealed, err := credseal.Seal(pub, plaintext)
		return sealed, 3, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	res, conn, err := BringUpRunSidecar(ctx, &fakeLaunchedSidecar{conn: orchEnd}, provision, SidecarBringUpParams{HostVethIP: "10.42.9.1"})
	if err != nil {
		t.Fatalf("BringUpRunSidecar: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	// The env the sidecar's proxies produced is returned verbatim.
	if len(res.Env) != 2 || res.Env[0] != "ANTHROPIC_BASE_URL=http://10.42.9.1:5000" {
		t.Fatalf("unexpected proxy env: %v", res.Env)
	}
	// The sidecar received — and could open — the relayed bundle.
	select {
	case b := <-sc.gotBundle:
		if b.LLM["ANTHROPIC_API_KEY"] != "sk-ant-REAL" {
			t.Fatalf("sidecar unsealed wrong material: %+v", b.LLM)
		}
		if b.BootEpoch != 3 {
			t.Fatalf("boot epoch not relayed: %d", b.BootEpoch)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("sidecar never received the relayed bundle")
	}
}

// fakeRelayDispatcher records the ops the supervisor routes to it and replies
// to a Call with a fixed body — the unit seam for testing that the supervisor
// parses the relay envelope and dispatches (namespace, op, args) correctly.
// The dispatcher's own behavior (git gate, DB ops) is tested where it lives
// (agenthost.RelayServer), which this package can't import without a cycle.
type fakeRelayDispatcher struct {
	callReply json.RawMessage
	calls     chan sidecarproto.RelayCallBody
	records   chan sidecarproto.RelayRecordBody
}

func (f *fakeRelayDispatcher) DispatchCall(_ context.Context, ns, op string, args json.RawMessage) (json.RawMessage, error) {
	f.calls <- sidecarproto.RelayCallBody{Namespace: ns, Op: op, Args: args}
	return f.callReply, nil
}

func (f *fakeRelayDispatcher) DispatchRecord(_ context.Context, ns, op, key string, args json.RawMessage) error {
	f.records <- sidecarproto.RelayRecordBody{Namespace: ns, Op: op, Key: key, Args: args}
	return nil
}

// bringUpWithDispatcher brings a scripted sidecar up against relay and returns
// the sidecar and the orchestrator's end of the supervision channel.
func bringUpWithDispatcher(t *testing.T, relay RelayDispatcher) (*scriptedSidecar, *sidecarproto.Conn) {
	t.Helper()
	kp, _ := credseal.GenerateKeyPair()
	orchEnd, sidecarEnd := net.Pipe()

	sc := &scriptedSidecar{kp: kp, gotBundle: make(chan *credbundle.Bundle, 1), startEnv: nil}
	sc.conn = sidecarproto.New(sidecarEnd, sc)
	go func() {
		_ = sc.conn.Notify(sidecarproto.KindHello, sidecarproto.HelloBody{PubKey: base64.StdEncoding.EncodeToString(kp.Public[:])})
	}()
	t.Cleanup(func() { _ = sc.conn.Close(); _ = sidecarEnd.Close() })

	provision := func(_ context.Context, pubKeyB64 string) ([]byte, int64, error) {
		pubBytes, _ := base64.StdEncoding.DecodeString(pubKeyB64)
		var pub [32]byte
		copy(pub[:], pubBytes)
		pt, _ := (&credbundle.Bundle{}).Marshal()
		sealed, err := credseal.Seal(pub, pt)
		return sealed, 0, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, conn, err := BringUpRunSidecar(ctx, &fakeLaunchedSidecar{conn: orchEnd}, provision, SidecarBringUpParams{HostVethIP: "10.42.9.1", Relay: relay})
	if err != nil {
		t.Fatalf("BringUpRunSidecar: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return sc, conn
}

func TestBringUpSidecar_RelayCallRoutesToDispatcher(t *testing.T) {
	relay := &fakeRelayDispatcher{
		callReply: json.RawMessage(`{"allowed":true,"allowed_refs":["refs/heads/feature"]}`),
		calls:     make(chan sidecarproto.RelayCallBody, 1),
	}
	sc, _ := bringUpWithDispatcher(t, relay)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	// The sidecar relays a request/response op; the supervisor must parse the
	// envelope, dispatch (namespace, op, args), and relay the reply back.
	var dec AuthorizeRepoReply
	if err := CallRelay(ctx, sc.conn, RelayNamespaceCore, OpAuthorizeRepo,
		AuthorizeRepoArgs{Owner: "acme", Repo: "widgets"}, &dec); err != nil {
		t.Fatalf("relay call: %v", err)
	}
	if !dec.Allowed || len(dec.AllowedRefs) != 1 || dec.AllowedRefs[0] != "refs/heads/feature" {
		t.Fatalf("unexpected decision: %+v", dec)
	}
	select {
	case got := <-relay.calls:
		if got.Namespace != RelayNamespaceCore || got.Op != OpAuthorizeRepo {
			t.Fatalf("dispatcher saw wrong op: %s/%s", got.Namespace, got.Op)
		}
		var a AuthorizeRepoArgs
		if err := json.Unmarshal(got.Args, &a); err != nil {
			t.Fatalf("unmarshal relayed args: %v", err)
		}
		if a.Owner != "acme" || a.Repo != "widgets" {
			t.Fatalf("dispatcher saw wrong args: %+v", a)
		}
	case <-time.After(time.Second):
		t.Fatal("dispatcher was never consulted")
	}
}

func TestBringUpSidecar_RelayRecordRoutesToDispatcher(t *testing.T) {
	relay := &fakeRelayDispatcher{records: make(chan sidecarproto.RelayRecordBody, 1)}
	sc, _ := bringUpWithDispatcher(t, relay)

	// The sidecar's git proxy records a completed push through the run's
	// record sender; the supervisor must route it, key included, to the
	// dispatcher.
	sender := NewRecordSender(sc.conn)
	if err := sender.Send(RelayNamespaceCore, OpRecordPush,
		RecordPushArgs{Repo: "acme/widgets", Ref: "refs/heads/feature", NewSHA: "deadbeef", Created: true, Status: 200}); err != nil {
		t.Fatalf("send record: %v", err)
	}
	select {
	case got := <-relay.records:
		if got.Namespace != RelayNamespaceCore || got.Op != OpRecordPush {
			t.Fatalf("dispatcher saw wrong op: %s/%s", got.Namespace, got.Op)
		}
		if got.Key == "" {
			t.Fatal("dispatcher saw a record with no key")
		}
		var a RecordPushArgs
		if err := json.Unmarshal(got.Args, &a); err != nil {
			t.Fatalf("unmarshal relayed args: %v", err)
		}
		if a.Repo != "acme/widgets" || a.NewSHA != "deadbeef" || a.Status != 200 {
			t.Fatalf("dispatcher saw wrong args: %+v", a)
		}
	case <-time.After(time.Second):
		t.Fatal("push record was never routed to the dispatcher")
	}
}

// largestReviewDraftDetails is the details of the largest review draft
// sidecarFrameLimit is set to fit: finalized, with 100 comments of English
// prose (code punctuation included), each at GitHub's 65,536-character
// maximum, every one held twice — once staged and once in the proposed
// snapshot.
func largestReviewDraftDetails() string {
	const unit = "This change moves the `Foo` call ahead of the lock, so a caller that \"holds\" it <never> waits & retries.\n"
	body := strings.Repeat(unit, 65536/len(unit)+1)[:65536]
	line := 10
	comments := make([]domain.ReviewArtifactComment, 100)
	for i := range comments {
		comments[i] = domain.ReviewArtifactComment{
			ID: fmt.Sprintf("c%04d", i), Path: "internal/some/package/file_name.go",
			Line: &line, Body: body, CommitSHA: strings.Repeat("a", 40),
		}
	}
	return domain.MarshalReviewArtifactDetails(domain.ReviewArtifactDetails{
		Number: 1311, HeadSHA: strings.Repeat("b", 40),
		FinalizedHeadSHA: strings.Repeat("b", 40), FinalizedBaseSHA: strings.Repeat("c", 40),
		ReviewBody: body, ReviewEvent: "COMMENT", StagedComments: comments,
		Proposed: domain.ReviewArtifactProposed{Body: body, Event: "COMMENT", Comments: comments},
	})
}

// reviewDetailsArgs is the wire shape of agenthost's update_review_details_if_pending
// op, which agentproc cannot import.
type reviewDetailsArgs struct {
	ArtifactID  string `json:"artifact_id"`
	DetailsJSON string `json:"details_json"`
}

// TestBringUpSidecar_TheLargestLegitimateFrameGetsThrough: the review-draft
// write sidecarFrameLimit is measured against crosses the orchestrator's end
// of a real bring-up's channel and reaches the dispatcher whole.
func TestBringUpSidecar_TheLargestLegitimateFrameGetsThrough(t *testing.T) {
	relay := &fakeRelayDispatcher{callReply: json.RawMessage(`{"updated":true}`), calls: make(chan sidecarproto.RelayCallBody, 1)}
	sc, conn := bringUpWithDispatcher(t, relay)

	args := reviewDetailsArgs{ArtifactID: "art-1", DetailsJSON: largestReviewDraftDetails()}
	// Measured the way the Conn writes it, so this checks the frame is the
	// size the limit was set for and not a smaller stand-in.
	rawArgs, _ := json.Marshal(args)
	body, _ := json.Marshal(sidecarproto.RelayCallBody{Namespace: RelayNamespaceCore, Op: "update_review_details_if_pending", Args: rawArgs})
	var frame bytes.Buffer
	_ = json.NewEncoder(&frame).Encode(sidecarproto.Frame{Kind: sidecarproto.KindRelayCall, ID: 1, Body: body})
	if size := frame.Len() - 1; size < 15<<20 || size > sidecarFrameLimit {
		t.Fatalf("the review-draft frame is %d bytes; the test needs one between 15 MiB and the %d-byte limit", size, sidecarFrameLimit)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var out struct {
		Updated bool `json:"updated"`
	}
	if err := CallRelay(ctx, sc.conn, RelayNamespaceCore, "update_review_details_if_pending", args, &out); err != nil {
		t.Fatalf("relay the review draft: %v", err)
	}
	if !out.Updated {
		t.Fatal("the dispatcher's answer did not come back")
	}
	got := <-relay.calls
	var gotArgs reviewDetailsArgs
	if err := json.Unmarshal(got.Args, &gotArgs); err != nil {
		t.Fatalf("decode relayed args: %v", err)
	}
	if gotArgs.DetailsJSON != args.DetailsJSON {
		t.Fatalf("the dispatcher saw %d bytes of details; the sidecar sent %d", len(gotArgs.DetailsJSON), len(args.DetailsJSON))
	}
	if err := conn.Err(); err != nil {
		t.Fatalf("the channel closed: %v", err)
	}
}

// TestBringUpSidecar_AFrameOverTheLimitClosesTheChannelAndIsCounted: once the
// run is up, a frame over sidecarFrameLimit closes the orchestrator's end of
// the channel, and the refusal is counted.
func TestBringUpSidecar_AFrameOverTheLimitClosesTheChannelAndIsCounted(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	prev := oversizedFrames.Swap(newOversizedFrameStats(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))))
	t.Cleanup(func() { oversizedFrames.Store(prev) })

	sc, conn := bringUpWithDispatcher(t, &fakeRelayDispatcher{})

	// The write cannot finish once the orchestrator stops reading; the
	// cleanup's close of the sidecar's end releases it.
	go func() {
		_ = sc.conn.Notify(sidecarproto.KindHello, sidecarproto.HelloBody{PubKey: strings.Repeat("k", sidecarFrameLimit)})
	}()
	select {
	case <-conn.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("the channel stayed open after a frame over the limit")
	}
	if err := conn.Err(); !errors.Is(err, sidecarproto.ErrFrameTooLarge) {
		t.Fatalf("channel closed with %v; want ErrFrameTooLarge", err)
	}

	// Counted by the goroutine watching the channel, so wait for it.
	deadline := time.Now().Add(2 * time.Second)
	for oversizedFrameCount(t, reader) != 1 {
		if time.Now().After(deadline) {
			t.Fatalf("sidecar.frames.oversized = %d; want 1", oversizedFrameCount(t, reader))
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// oversizedFrameCount reads sidecar.frames.oversized off reader.
func oversizedFrameCount(t *testing.T, reader *sdkmetric.ManualReader) int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect metrics: %v", err)
	}
	var n int64
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "sidecar.frames.oversized" {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("sidecar.frames.oversized is %T, want an int64 sum", m.Data)
			}
			for _, dp := range sum.DataPoints {
				n += dp.Value
			}
		}
	}
	return n
}
