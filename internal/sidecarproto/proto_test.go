package sidecarproto

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// pipeConns wires two Conns over an in-memory duplex pipe, each with its own
// handler, and returns them plus a cleanup.
func pipeConns(t *testing.T, aHandler, bHandler Handler) (a, b *Conn) {
	t.Helper()
	ca, cb := net.Pipe()
	a = New(ca, aHandler)
	b = New(cb, bHandler)
	t.Cleanup(func() {
		_ = a.Close()
		_ = b.Close()
		_ = ca.Close()
		_ = cb.Close()
	})
	return a, b
}

func TestConn_CallRoundTrip(t *testing.T) {
	// b serves StartProxies by echoing an env entry derived from the request.
	server := HandlerFunc(func(_ context.Context, kind Kind, body json.RawMessage) (any, error) {
		if kind != KindStartProxies {
			t.Errorf("unexpected kind %q", kind)
		}
		var req StartProxiesBody
		if err := json.Unmarshal(body, &req); err != nil {
			return nil, err
		}
		return StartProxiesResult{Env: []string{"HOST=" + req.HostVethIP}}, nil
	})
	client, _ := pipeConns(t, nil, server)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var res StartProxiesResult
	if err := client.Call(ctx, KindStartProxies, StartProxiesBody{HostVethIP: "10.42.7.1"}, &res); err != nil {
		t.Fatalf("Call: %v", err)
	}
	if len(res.Env) != 1 || res.Env[0] != "HOST=10.42.7.1" {
		t.Fatalf("unexpected result: %+v", res)
	}
}

func TestConn_RemoteError(t *testing.T) {
	server := HandlerFunc(func(_ context.Context, _ Kind, _ json.RawMessage) (any, error) {
		return nil, errors.New("unseal failed")
	})
	client, _ := pipeConns(t, nil, server)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := client.Call(ctx, KindSealedBundle, SealedBundleBody{Sealed: []byte{1, 2, 3}}, nil)
	if err == nil {
		t.Fatal("expected remote error")
	}
	if got := err.Error(); got == "" || !contains(got, "unseal failed") {
		t.Fatalf("expected wrapped remote error, got %q", got)
	}
}

func TestConn_Bidirectional(t *testing.T) {
	// The classic sidecar shape: the orchestrator (a) asks the sidecar (b) to
	// start proxies; while serving, the sidecar relays a call back to the
	// orchestrator (the generic KindRelayCall envelope — a git-push authz
	// decision). Exercises both directions on one connection.
	authHandler := HandlerFunc(func(_ context.Context, kind Kind, body json.RawMessage) (any, error) {
		if kind != KindRelayCall {
			return nil, errors.New("wrong kind")
		}
		var env RelayCallBody
		if err := json.Unmarshal(body, &env); err != nil {
			return nil, err
		}
		if env.Namespace != "core" || env.Op != "authorize_repo" {
			return nil, errors.New("wrong relay op")
		}
		return json.RawMessage(`{"allowed":true,"allowed_refs":["refs/heads/feature"]}`), nil
	})
	var b *Conn
	sidecarHandler := HandlerFunc(func(ctx context.Context, kind Kind, _ json.RawMessage) (any, error) {
		if kind != KindStartProxies {
			return nil, errors.New("wrong kind")
		}
		// The sidecar relays a call to the orchestrator over its OWN connection.
		var dec struct {
			Allowed bool `json:"allowed"`
		}
		if err := b.Call(ctx, KindRelayCall, RelayCallBody{
			Namespace: "core", Op: "authorize_repo", Args: json.RawMessage(`{"owner":"o","repo":"r"}`),
		}, &dec); err != nil {
			return nil, err
		}
		if !dec.Allowed {
			return nil, errors.New("expected allow")
		}
		return StartProxiesResult{Env: []string{"OK=1"}}, nil
	})
	var a *Conn
	a, b = pipeConns(t, authHandler, sidecarHandler)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var res StartProxiesResult
	if err := a.Call(ctx, KindStartProxies, StartProxiesBody{HostVethIP: "10.42.1.1"}, &res); err != nil {
		t.Fatalf("Call: %v", err)
	}
	if len(res.Env) != 1 || res.Env[0] != "OK=1" {
		t.Fatalf("unexpected result: %+v", res)
	}
}

func TestConn_Notify(t *testing.T) {
	got := make(chan HelloBody, 1)
	server := HandlerFunc(func(_ context.Context, kind Kind, body json.RawMessage) (any, error) {
		if kind == KindHello {
			var h HelloBody
			_ = json.Unmarshal(body, &h)
			got <- h
		}
		return nil, nil
	})
	client, _ := pipeConns(t, nil, server)

	if err := client.Notify(KindHello, HelloBody{PubKey: "abc"}); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	select {
	case h := <-got:
		if h.PubKey != "abc" {
			t.Fatalf("unexpected hello: %+v", h)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("notify not delivered")
	}
}

func TestConn_CallAfterCloseFails(t *testing.T) {
	client, _ := pipeConns(t, nil, nil)
	_ = client.Close()
	err := client.Call(context.Background(), KindStartProxies, StartProxiesBody{}, nil)
	if !errors.Is(err, ErrClosed) {
		t.Fatalf("expected ErrClosed, got %v", err)
	}
}

// TestConn_CallInterruptedByCloseFails covers the in-flight case: the conn
// shuts down while a Call is blocked waiting for its reply. shutdown() closes
// the pending channel, and the Call must report that as a failure — never read
// the zero-value Frame off the closed channel as an empty success.
func TestConn_CallInterruptedByCloseFails(t *testing.T) {
	// The server never answers, so the Call blocks on its pending channel until
	// we tear the connection down underneath it.
	blocked := HandlerFunc(func(ctx context.Context, _ Kind, _ json.RawMessage) (any, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})
	client, _ := pipeConns(t, nil, blocked)

	errc := make(chan error, 1)
	go func() {
		errc <- client.Call(context.Background(), KindStartProxies, StartProxiesBody{HostVethIP: "10.42.1.1"}, nil)
	}()

	// Give the Call time to register its pending entry and block, then shut the
	// conn down out from under it.
	time.Sleep(50 * time.Millisecond)
	_ = client.Close()

	select {
	case err := <-errc:
		if err == nil {
			t.Fatal("Call returned nil on a mid-flight shutdown — a closed pending channel must not read as success")
		}
		if !errors.Is(err, ErrClosed) {
			t.Fatalf("expected ErrClosed, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Call did not return after the conn was closed mid-flight")
	}
}

// panicPayload is the giveaway a panic value would carry across the wire if
// either guard formatted it into an error. Panic values are arbitrary runtime
// state — a formatted request body, a struct that happened to hold a credential
// — and the peer's error string is logged and persisted, so the tests below
// assert this string is nowhere in what the caller receives.
const panicPayload = "token-abc123-must-not-cross-the-wire"

// TestConn_PanickingHandler_AnswersAndKeepsServing pins all three halves of the
// dispatch guard. Containment: the panic doesn't take the process (without the
// recover this test kills the binary, which is what it would do to the sidecar
// and every proxy in it). Correctness: the caller gets an error response
// promptly instead of blocking to its own deadline — a panicking handler used
// to turn an immediate failure into a timeout. Discretion: the response names
// the failure without carrying the panic value.
func TestConn_PanickingHandler_AnswersAndKeepsServing(t *testing.T) {
	server := HandlerFunc(func(_ context.Context, kind Kind, _ json.RawMessage) (any, error) {
		if kind == KindSealedBundle {
			panic("handler exploded: " + panicPayload)
		}
		return StartProxiesResult{Env: []string{"OK=1"}}, nil
	})
	client, _ := pipeConns(t, nil, server)

	// A deadline far longer than the answer should take, so a timeout here
	// means the caller was left blocking rather than answered.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	answered := make(chan error, 1)
	go func() {
		answered <- client.Call(ctx, KindSealedBundle, SealedBundleBody{Sealed: []byte{1, 2, 3}}, nil)
	}()
	select {
	case err := <-answered:
		if err == nil {
			t.Fatal("expected the panicking handler to answer with an error")
		}
		if !contains(err.Error(), "panicked") {
			t.Fatalf("error = %q, want it to name the internal failure", err)
		}
		if contains(err.Error(), panicPayload) {
			t.Fatalf("the panic value crossed the wire in %q — a response is an error string the peer logs and persists", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("caller was left blocking on a request whose handler panicked")
	}

	// The connection is still usable: one dead request is one dead request.
	var res StartProxiesResult
	if err := client.Call(ctx, KindStartProxies, StartProxiesBody{HostVethIP: "10.42.1.1"}, &res); err != nil {
		t.Fatalf("next call after the panic: %v", err)
	}
	if len(res.Env) != 1 || res.Env[0] != "OK=1" {
		t.Fatalf("unexpected result after the panic: %+v", res)
	}
}

// panickingStream is a duplex stream whose Read panics — the shape of a
// decoder blowing up on an inbound frame. The read blocks until the first
// Write, so the panic lands after a Call has registered its pending entry and
// the test is exercising the in-flight case rather than a race.
type panickingStream struct {
	release   chan struct{}
	once      sync.Once
	firstRead chan struct{}
}

func newPanickingStream() *panickingStream {
	return &panickingStream{release: make(chan struct{}), firstRead: make(chan struct{}, 1)}
}

func (p *panickingStream) Read([]byte) (int, error) {
	select {
	case p.firstRead <- struct{}{}:
	default:
	}
	<-p.release
	panic("reader exploded: " + panicPayload)
}

func (p *panickingStream) Write(b []byte) (int, error) {
	p.once.Do(func() { close(p.release) })
	return len(b), nil
}

func (p *panickingStream) Close() error { return nil }

// TestConn_PanickingReadLoop_CollapsesConnWithoutLeakingTheValue covers the
// other raw goroutine. A panic in the reader must collapse the connection
// rather than the process — every in-flight Call fails with the shutdown cause,
// which is an outcome callers already handle — and that cause must not carry
// the panic value, because unlike a response it is handed to EVERY pending Call
// at once.
func TestConn_PanickingReadLoop_CollapsesConnWithoutLeakingTheValue(t *testing.T) {
	stream := newPanickingStream()
	c := New(stream, nil)
	t.Cleanup(func() { _ = c.Close() })

	// Wait for the read loop to be parked in Read, so the Call below is the
	// in-flight case: its own Write is what releases the panic.
	select {
	case <-stream.firstRead:
	case <-time.After(2 * time.Second):
		t.Fatal("read loop never reached its first Read")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := c.Call(ctx, KindStartProxies, StartProxiesBody{HostVethIP: "10.42.1.1"}, nil)
	if err == nil {
		t.Fatal("Call returned nil after the read loop panicked")
	}
	if !errors.Is(err, ErrClosed) {
		t.Fatalf("error = %v, want it to wrap ErrClosed (the defined collapse outcome)", err)
	}
	if contains(err.Error(), panicPayload) {
		t.Fatalf("the panic value reached a pending Call in %q — this cause fans out to every caller at once", err)
	}

	// The conn is properly collapsed, not merely wedged: Done is closed and
	// subsequent Calls fail fast instead of blocking on a dead reader.
	select {
	case <-c.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("Done was not closed after the read loop panicked")
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// recordCall sends one KindRelayRecord frame as a Call, the way a sidecar's
// record sender does, and reports its outcome on the returned channel.
func recordCall(c *Conn, op string) <-chan error {
	done := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		done <- c.Call(ctx, KindRelayRecord, RelayRecordBody{Namespace: "core", Op: op, Key: op}, nil)
	}()
	return done
}

// recordOp decodes the op a record frame carries.
func recordOp(t *testing.T, body json.RawMessage) string {
	t.Helper()
	var env RelayRecordBody
	if err := json.Unmarshal(body, &env); err != nil {
		t.Errorf("decode record: %v", err)
	}
	return env.Op
}

// TestConn_RecordsAreServedOneAtATimeInOrder: a second record is not handed to
// the handler until the first has returned, whatever their arrival gap — which
// is what keeps a comment's delete from being applied before its post.
func TestConn_RecordsAreServedOneAtATimeInOrder(t *testing.T) {
	started := make(chan string, 4)
	release := make(chan struct{})
	server := HandlerFunc(func(_ context.Context, kind Kind, body json.RawMessage) (any, error) {
		if kind != KindRelayRecord {
			return nil, nil
		}
		op := recordOp(t, body)
		started <- op
		if op == "first" {
			<-release
		}
		return nil, nil
	})
	client, _ := pipeConns(t, nil, server)

	first := recordCall(client, "first")
	if got := <-started; got != "first" {
		t.Fatalf("first record served = %q", got)
	}
	second := recordCall(client, "second")
	select {
	case got := <-started:
		t.Fatalf("record %q was served while the first was still being served", got)
	case <-time.After(200 * time.Millisecond):
	}

	close(release)
	if err := <-first; err != nil {
		t.Fatalf("first record: %v", err)
	}
	select {
	case got := <-started:
		if got != "second" {
			t.Fatalf("second record served = %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the second record was never served")
	}
	if err := <-second; err != nil {
		t.Fatalf("second record: %v", err)
	}
}

// TestConn_ASlowRecordDoesNotHoldCalls: a record being served blocks only the
// records behind it. A Call on the same connection is still served, and this
// side's own Calls still get their answers.
func TestConn_ASlowRecordDoesNotHoldCalls(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	recordStarted := make(chan struct{})
	server := HandlerFunc(func(_ context.Context, kind Kind, _ json.RawMessage) (any, error) {
		switch kind {
		case KindRelayRecord:
			close(recordStarted)
			<-release
			return nil, nil
		case KindStartProxies:
			return StartProxiesResult{Env: []string{"OK=1"}}, nil
		}
		return nil, nil
	})
	client, _ := pipeConns(t, nil, server)

	_ = recordCall(client, "slow")
	<-recordStarted

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var res StartProxiesResult
	if err := client.Call(ctx, KindStartProxies, StartProxiesBody{}, &res); err != nil {
		t.Fatalf("Call behind a slow record: %v", err)
	}
	if len(res.Env) != 1 || res.Env[0] != "OK=1" {
		t.Fatalf("unexpected result: %+v", res)
	}
}

// TestConn_AFullRecordLaneAnswersAtOnce: once the lane is full, a further
// record is refused immediately with ErrRecordLaneFull rather than queued
// behind the reader, so the sender backs off and resends instead of waiting.
func TestConn_AFullRecordLaneAnswersAtOnce(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	started := make(chan struct{}, 1)
	server := HandlerFunc(func(_ context.Context, kind Kind, _ json.RawMessage) (any, error) {
		if kind == KindRelayRecord {
			select {
			case started <- struct{}{}:
			default:
			}
			<-release
		}
		return nil, nil
	})
	client, server2 := pipeConns(t, nil, server)

	_ = recordCall(client, "being-served")
	<-started
	// Fill the lane behind the record being served.
	for i := 0; i < recordLaneLimit; i++ {
		_ = recordCall(client, "queued")
	}
	deadline := time.Now().Add(2 * time.Second)
	for len(server2.records) < recordLaneLimit {
		if time.Now().After(deadline) {
			t.Fatalf("lane holds %d records; want %d", len(server2.records), recordLaneLimit)
		}
		time.Sleep(5 * time.Millisecond)
	}

	select {
	case err := <-recordCall(client, "overflow"):
		if err == nil || !strings.Contains(err.Error(), ErrRecordLaneFull.Error()) {
			t.Fatalf("overflowing record = %v; want %v", err, ErrRecordLaneFull)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a record arriving at a full lane was not answered")
	}
}

// testFrameLimit is the frame limit the size tests build their Conns with:
// small enough to cross quickly, and many read buffers long.
const testFrameLimit = 64 << 10

// readBufferSize is the size of the bufio.Reader New reads through
// (bufio.NewReader's default), which is how far past the limit a read may go
// before an oversized frame is refused.
const readBufferSize = 4096

// countingStream is a duplex stream that counts the bytes read from it, so a
// test can show how much of an oversized frame the reader took in.
type countingStream struct {
	net.Conn
	read atomic.Int64
}

func (s *countingStream) Read(p []byte) (int, error) {
	n, err := s.Conn.Read(p)
	s.read.Add(int64(n))
	return n, err
}

// helloFrame is a hello frame exactly n bytes long, its newline not counted,
// padded out through the public key.
func helloFrame(t *testing.T, n int) []byte {
	t.Helper()
	const prefix, suffix = `{"kind":"hello","body":{"pubkey":"`, `"}}`
	pad := n - len(prefix) - len(suffix)
	if pad < 0 {
		t.Fatalf("a hello frame cannot be %d bytes", n)
	}
	return []byte(prefix + strings.Repeat("k", pad) + suffix)
}

// limitedConn builds a Conn with testFrameLimit over one end of a pipe, and
// returns the Conn, the counted stream under it, and the raw other end the
// test writes frames to.
func limitedConn(t *testing.T, handler Handler) (*Conn, *countingStream, net.Conn) {
	t.Helper()
	ours, peer := net.Pipe()
	stream := &countingStream{Conn: ours}
	c := New(stream, handler, WithReadLimit(testFrameLimit))
	t.Cleanup(func() {
		_ = c.Close()
		_ = ours.Close()
		_ = peer.Close()
	})
	return c, stream, peer
}

// writeRaw writes b to the peer end in the background. A write the reader
// stops consuming blocks until the test's cleanup closes the pipe.
func writeRaw(peer net.Conn, b []byte) {
	go func() { _, _ = peer.Write(b) }()
}

// awaitOversized waits for c to shut down and checks that it shut down for an
// oversized frame, having read at most the limit plus one buffer of it.
func awaitOversized(t *testing.T, c *Conn, stream *countingStream) {
	t.Helper()
	select {
	case <-c.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("the conn stayed open after a frame over the limit")
	}
	if err := c.Err(); !errors.Is(err, ErrFrameTooLarge) || !errors.Is(err, ErrClosed) {
		t.Fatalf("shutdown cause = %v; want it to wrap ErrFrameTooLarge and ErrClosed", err)
	}
	if got, most := stream.read.Load(), int64(testFrameLimit+readBufferSize); got > most {
		t.Fatalf("read %d bytes of the oversized frame; at most %d (the limit plus one buffer) should have been read", got, most)
	}
}

func TestConn_AFrameAtTheLimitIsDelivered(t *testing.T) {
	got := make(chan int, 1)
	handler := HandlerFunc(func(_ context.Context, kind Kind, body json.RawMessage) (any, error) {
		var h HelloBody
		if err := json.Unmarshal(body, &h); err != nil {
			t.Errorf("decode hello: %v", err)
		}
		got <- len(h.PubKey)
		return nil, nil
	})
	c, _, peer := limitedConn(t, handler)

	frame := helloFrame(t, testFrameLimit)
	writeRaw(peer, append(frame, '\n'))
	select {
	case n := <-got:
		if want := len(frame) - len(`{"kind":"hello","body":{"pubkey":""}}`); n != want {
			t.Fatalf("delivered a %d-byte key; want %d", n, want)
		}
	case <-c.Done():
		t.Fatalf("a frame at the limit closed the conn: %v", c.Err())
	case <-time.After(2 * time.Second):
		t.Fatal("a frame at the limit was never delivered")
	}
}

// TestConn_AFrameOverTheLimitClosesTheConn: one byte over is refused before the
// reader holds it, and the Call this side has open fails with the shutdown
// cause rather than waiting for an answer that can no longer arrive.
func TestConn_AFrameOverTheLimitClosesTheConn(t *testing.T) {
	c, stream, peer := limitedConn(t, nil)

	// The peer reads this side's request off the wire, so the Call is in
	// flight, then answers with a frame one byte over the limit.
	oversized := append(helloFrame(t, testFrameLimit+1), '\n')
	go func() {
		if _, err := bufio.NewReader(peer).ReadBytes('\n'); err != nil {
			return
		}
		_, _ = peer.Write(oversized)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := c.Call(ctx, KindStartProxies, StartProxiesBody{HostVethIP: "10.42.1.1"}, nil)
	if !errors.Is(err, ErrFrameTooLarge) || !errors.Is(err, ErrClosed) {
		t.Fatalf("in-flight Call = %v; want the shutdown cause, wrapping ErrFrameTooLarge and ErrClosed", err)
	}
	awaitOversized(t, c, stream)
}

// TestConn_AFrameThatNeverEndsIsRefused: a peer that never sends the newline
// is refused once the limit is passed, not read until it stops.
func TestConn_AFrameThatNeverEndsIsRefused(t *testing.T) {
	c, stream, peer := limitedConn(t, nil)
	writeRaw(peer, []byte(strings.Repeat("x", 4*testFrameLimit)))
	awaitOversized(t, c, stream)
}

// TestConn_ACallOverTheWriteLimitIsNotSent: a frame the peer would refuse
// fails on this end, before any of it is written, and the conn stays open for
// the next Call.
func TestConn_ACallOverTheWriteLimitIsNotSent(t *testing.T) {
	ours, peer := net.Pipe()
	peerStream := &countingStream{Conn: peer}
	served := make(chan Kind, 4)
	server := New(peerStream, HandlerFunc(func(_ context.Context, kind Kind, _ json.RawMessage) (any, error) {
		served <- kind
		return StartProxiesResult{Env: []string{"OK=1"}}, nil
	}))
	client := New(ours, nil, WithWriteLimit(testFrameLimit))
	t.Cleanup(func() {
		_ = client.Close()
		_ = server.Close()
		_ = ours.Close()
		_ = peer.Close()
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := client.Call(ctx, KindStartProxies, StartProxiesBody{HostVethIP: strings.Repeat("x", testFrameLimit)}, nil)
	if !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("Call over the write limit = %v; want ErrFrameTooLarge", err)
	}
	if errors.Is(err, ErrClosed) {
		t.Fatalf("Call over the write limit = %v; a refused frame must not read as a closed conn", err)
	}

	var res StartProxiesResult
	if err := client.Call(ctx, KindStartProxies, StartProxiesBody{HostVethIP: "10.42.1.1"}, &res); err != nil {
		t.Fatalf("the next Call after the refused one: %v", err)
	}
	if len(served) != 1 {
		t.Fatalf("the peer served %d requests; want 1, the refused frame never sent", len(served))
	}
	if n := peerStream.read.Load(); n > 1024 {
		t.Fatalf("the peer read %d bytes; only the small Call should have crossed", n)
	}
}

// TestConn_AResponseOverTheWriteLimitIsAnsweredWithTheRefusal: a handler whose
// answer is too large to send still answers, with the refusal, so the caller
// is not left waiting out its deadline.
func TestConn_AResponseOverTheWriteLimitIsAnsweredWithTheRefusal(t *testing.T) {
	ca, cb := net.Pipe()
	client := New(ca, nil)
	server := New(cb, HandlerFunc(func(context.Context, Kind, json.RawMessage) (any, error) {
		return StartProxiesResult{Env: []string{strings.Repeat("x", testFrameLimit)}}, nil
	}), WithWriteLimit(testFrameLimit))
	t.Cleanup(func() {
		_ = client.Close()
		_ = server.Close()
		_ = ca.Close()
		_ = cb.Close()
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	answered := make(chan error, 1)
	go func() { answered <- client.Call(ctx, KindStartProxies, StartProxiesBody{}, nil) }()
	select {
	case err := <-answered:
		if err == nil || !strings.Contains(err.Error(), ErrFrameTooLarge.Error()) {
			t.Fatalf("Call = %v; want the remote refusal naming the size limit", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the caller was left waiting on an answer too large to send")
	}
	if err := server.Err(); err != nil {
		t.Fatalf("the serving end closed: %v", err)
	}
}
