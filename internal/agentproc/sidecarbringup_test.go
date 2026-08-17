package agentproc

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/credbundle"
	"github.com/sky-ai-eng/triage-factory/internal/credseal"
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
	notifies  chan sidecarproto.RelayCallBody
}

func (f *fakeRelayDispatcher) DispatchCall(_ context.Context, ns, op string, args json.RawMessage) (json.RawMessage, error) {
	f.calls <- sidecarproto.RelayCallBody{Namespace: ns, Op: op, Args: args}
	return f.callReply, nil
}

func (f *fakeRelayDispatcher) DispatchNotify(_ context.Context, ns, op string, args json.RawMessage) {
	f.notifies <- sidecarproto.RelayCallBody{Namespace: ns, Op: op, Args: args}
}

func bringUpWithDispatcher(t *testing.T, relay RelayDispatcher) *scriptedSidecar {
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
	return sc
}

func TestBringUpSidecar_RelayCallRoutesToDispatcher(t *testing.T) {
	relay := &fakeRelayDispatcher{
		callReply: json.RawMessage(`{"allowed":true,"allowed_refs":["refs/heads/feature"]}`),
		calls:     make(chan sidecarproto.RelayCallBody, 1),
	}
	sc := bringUpWithDispatcher(t, relay)

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

func TestBringUpSidecar_RelayNotifyRoutesToDispatcher(t *testing.T) {
	relay := &fakeRelayDispatcher{notifies: make(chan sidecarproto.RelayCallBody, 1)}
	sc := bringUpWithDispatcher(t, relay)

	// The sidecar's git proxy reports a completed push (a fire-and-forget
	// Notify); the supervisor must route it to the dispatcher without awaiting
	// a reply.
	if err := NotifyRelay(sc.conn, RelayNamespaceCore, OpRecordPush,
		RecordPushArgs{Repo: "acme/widgets", Ref: "refs/heads/feature", NewSHA: "deadbeef", Created: true, Status: 200}); err != nil {
		t.Fatalf("relay notify: %v", err)
	}
	select {
	case got := <-relay.notifies:
		if got.Namespace != RelayNamespaceCore || got.Op != OpRecordPush {
			t.Fatalf("dispatcher saw wrong op: %s/%s", got.Namespace, got.Op)
		}
		var a RecordPushArgs
		if err := json.Unmarshal(got.Args, &a); err != nil {
			t.Fatalf("unmarshal relayed args: %v", err)
		}
		if a.Repo != "acme/widgets" || a.NewSHA != "deadbeef" || a.Status != 200 {
			t.Fatalf("dispatcher saw wrong args: %+v", a)
		}
	case <-time.After(time.Second):
		t.Fatal("push notify was never routed to the dispatcher")
	}
}
