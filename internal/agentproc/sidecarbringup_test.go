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
	"github.com/sky-ai-eng/triage-factory/internal/gitproxy"
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
	// bringUpSidecar (below) constructs the orchestrator Conn and starts
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

func TestBringUpSidecar_AuthorizeCallbackRoutesToGate(t *testing.T) {
	kp, _ := credseal.GenerateKeyPair()
	orchEnd, sidecarEnd := net.Pipe()

	authorized := make(chan [2]string, 1)
	git := &GitProxyConfig{
		Authorize: func(_ context.Context, owner, repo string) (gitproxy.Decision, error) {
			authorized <- [2]string{owner, repo}
			return gitproxy.Decision{Allowed: true, AllowedRefs: []string{"refs/heads/feature"}}, nil
		},
	}

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
	_, conn, err := BringUpRunSidecar(ctx, &fakeLaunchedSidecar{conn: orchEnd}, provision, SidecarBringUpParams{HostVethIP: "10.42.9.1", Git: git})
	if err != nil {
		t.Fatalf("BringUpRunSidecar: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	// The sidecar calls the orchestrator back to authorize a push; the
	// supervisor must route it to the DB-backed gate and relay the decision.
	var dec sidecarproto.AuthorizeRepoResult
	if err := sc.conn.Call(ctx, sidecarproto.KindAuthorizeRepo, sidecarproto.AuthorizeRepoBody{Owner: "acme", Repo: "widgets"}, &dec); err != nil {
		t.Fatalf("authorize callback: %v", err)
	}
	if !dec.Allowed || len(dec.AllowedRefs) != 1 || dec.AllowedRefs[0] != "refs/heads/feature" {
		t.Fatalf("unexpected decision: %+v", dec)
	}
	select {
	case got := <-authorized:
		if got[0] != "acme" || got[1] != "widgets" {
			t.Fatalf("gate saw wrong repo: %v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("authorize gate was never consulted")
	}
}

func TestBringUpSidecar_RecordPushRelaysToRecorder(t *testing.T) {
	kp, _ := credseal.GenerateKeyPair()
	orchEnd, sidecarEnd := net.Pipe()

	recorded := make(chan gitproxy.PushedRef, 1)
	git := &GitProxyConfig{
		RecordPush: func(_ context.Context, push gitproxy.PushedRef) {
			recorded <- push
		},
	}

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
	_, conn, err := BringUpRunSidecar(ctx, &fakeLaunchedSidecar{conn: orchEnd}, provision, SidecarBringUpParams{HostVethIP: "10.42.9.1", Git: git})
	if err != nil {
		t.Fatalf("BringUpRunSidecar: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	// The sidecar's git proxy reports a completed push (a Notify); the
	// supervisor must reshape it back into a PushedRef and hand it to the
	// orchestrator-side recorder — the sole push-capture path on the executor.
	if err := sc.conn.Notify(sidecarproto.KindRecordPush, sidecarproto.RecordPushBody{
		Repo:    "acme/widgets",
		Ref:     "refs/heads/feature",
		NewSHA:  "deadbeef",
		Created: true,
		Status:  200,
	}); err != nil {
		t.Fatalf("record-push notify: %v", err)
	}
	select {
	case got := <-recorded:
		want := gitproxy.PushedRef{Repo: "acme/widgets", Ref: "refs/heads/feature", NewSHA: "deadbeef", Created: true, Status: 200}
		if got != want {
			t.Fatalf("recorder saw %+v, want %+v", got, want)
		}
	case <-time.After(time.Second):
		t.Fatal("push was never relayed to the recorder")
	}
}
