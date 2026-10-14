package agentproc

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/sidecarproto"
)

// testRecordSendPolicy shrinks the sender's waits so a test that resends a few
// times finishes in milliseconds.
var testRecordSendPolicy = recordSendPolicy{
	queueLimit:    4096,
	callTimeout:   2 * time.Second,
	resendInitial: 5 * time.Millisecond,
	resendMax:     20 * time.Millisecond,
	drainWait:     300 * time.Millisecond,
}

// fakeOrchestrator is the orchestrator end of a supervision channel, as far as
// records go: it refuses the first `refuse` delivery attempts (a database
// outage, from the sender's side), then applies every record it is given. A
// negative refuse refuses for good.
type fakeOrchestrator struct {
	mu       sync.Mutex
	refuse   int
	attempts []sidecarproto.RelayRecordBody
	applied  []sidecarproto.RelayRecordBody
	changed  chan struct{}
}

func newFakeOrchestrator(refuse int) *fakeOrchestrator {
	return &fakeOrchestrator{refuse: refuse, changed: make(chan struct{}, 1)}
}

func (o *fakeOrchestrator) Handle(_ context.Context, kind sidecarproto.Kind, body json.RawMessage) (any, error) {
	if kind != sidecarproto.KindRelayRecord {
		return nil, nil
	}
	var env sidecarproto.RelayRecordBody
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, err
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	o.attempts = append(o.attempts, env)
	if o.refuse != 0 {
		if o.refuse > 0 {
			o.refuse--
		}
		return nil, errors.New("database unavailable")
	}
	o.applied = append(o.applied, env)
	select {
	case o.changed <- struct{}{}:
	default:
	}
	return nil, nil
}

// waitApplied blocks until n records have been applied and returns them.
func (o *fakeOrchestrator) waitApplied(t *testing.T, n int) []sidecarproto.RelayRecordBody {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		o.mu.Lock()
		got := len(o.applied)
		if got >= n {
			out := append([]sidecarproto.RelayRecordBody(nil), o.applied...)
			o.mu.Unlock()
			return out
		}
		o.mu.Unlock()
		select {
		case <-o.changed:
		case <-deadline:
			t.Fatalf("only %d of %d records were applied", got, n)
		}
	}
}

func (o *fakeOrchestrator) snapshot() (attempts, applied []sidecarproto.RelayRecordBody) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]sidecarproto.RelayRecordBody(nil), o.attempts...), append([]sidecarproto.RelayRecordBody(nil), o.applied...)
}

// newSenderPair wires a record sender on a sidecar-side Conn to orch over an
// in-memory supervision channel. The sidecar end answers a drain request the
// way the sidecar's runtime does, so DrainRecords can be driven over the wire.
// It returns the sender and the orchestrator-side Conn.
func newSenderPair(t *testing.T, orch *fakeOrchestrator, policy recordSendPolicy) (*RecordSender, *sidecarproto.Conn) {
	t.Helper()
	sidecarEnd, orchEnd := net.Pipe()
	var sender *RecordSender
	sidecarConn := sidecarproto.New(sidecarEnd, sidecarproto.HandlerFunc(func(ctx context.Context, kind sidecarproto.Kind, _ json.RawMessage) (any, error) {
		if kind == sidecarproto.KindDrainRecords {
			return sender.Drain(ctx), nil
		}
		return nil, nil
	}))
	orchConn := sidecarproto.New(orchEnd, orch)
	sender = newRecordSender(sidecarConn, policy)
	t.Cleanup(func() {
		_ = sidecarConn.Close()
		_ = orchConn.Close()
		_ = sidecarEnd.Close()
		_ = orchEnd.Close()
	})
	return sender, orchConn
}

func pushRecord(sha string) RecordPushArgs {
	return RecordPushArgs{Repo: "acme/widgets", Ref: "refs/heads/feat", NewSHA: sha, Status: 200}
}

func recordSHA(t *testing.T, r sidecarproto.RelayRecordBody) string {
	t.Helper()
	var a RecordPushArgs
	if err := json.Unmarshal(r.Args, &a); err != nil {
		t.Fatalf("decode record args: %v", err)
	}
	return a.NewSHA
}

// TestRecordSender_DeliversInOrder: on a healthy channel every record is
// applied once, in the order it was sent, each under a key of its own.
func TestRecordSender_DeliversInOrder(t *testing.T) {
	orch := newFakeOrchestrator(0)
	sender, _ := newSenderPair(t, orch, testRecordSendPolicy)

	shas := []string{"a1", "b2", "c3", "d4", "e5"}
	for _, sha := range shas {
		if err := sender.Send(RelayNamespaceCore, OpRecordPush, pushRecord(sha)); err != nil {
			t.Fatalf("Send: %v", err)
		}
	}
	applied := orch.waitApplied(t, len(shas))
	keys := map[string]bool{}
	for i, r := range applied {
		if got := recordSHA(t, r); got != shas[i] {
			t.Fatalf("record %d applied = %s; want %s (applied out of order)", i, got, shas[i])
		}
		if r.Key == "" || keys[r.Key] {
			t.Fatalf("record %d key %q is empty or reused", i, r.Key)
		}
		keys[r.Key] = true
	}
}

// TestRecordSender_ResendsThroughAnOutage: records sent while the orchestrator
// cannot apply them all reach it once it recovers — each applied exactly once,
// each resent under the key it was first sent with, and in the order they were
// sent: a record that needed several attempts is still applied before the ones
// queued after it, and none of those is even attempted until it is.
func TestRecordSender_ResendsThroughAnOutage(t *testing.T) {
	orch := newFakeOrchestrator(3)
	sender, _ := newSenderPair(t, orch, testRecordSendPolicy)

	shas := []string{"old", "mid", "new"}
	for _, sha := range shas {
		if err := sender.Send(RelayNamespaceCore, OpRecordPush, pushRecord(sha)); err != nil {
			t.Fatalf("Send: %v", err)
		}
	}
	applied := orch.waitApplied(t, len(shas))
	for i, r := range applied {
		if got := recordSHA(t, r); got != shas[i] {
			t.Fatalf("record %d applied = %s; want %s", i, got, shas[i])
		}
	}

	attempts, applied := orch.snapshot()
	if len(applied) != len(shas) {
		t.Fatalf("applied %d records; want each applied once (%d)", len(applied), len(shas))
	}
	// The three refused attempts were all the first record's, under one key;
	// nothing behind it was sent until it was acknowledged.
	if len(attempts) != 3+len(shas) {
		t.Fatalf("attempts = %d; want 3 refused + %d applied", len(attempts), len(shas))
	}
	for i := 0; i < 4; i++ {
		if recordSHA(t, attempts[i]) != "old" || attempts[i].Key != applied[0].Key {
			t.Fatalf("attempt %d = %s under key %s; want the first record, under its one key %s",
				i, recordSHA(t, attempts[i]), attempts[i].Key, applied[0].Key)
		}
	}
}

// TestRecordSender_DrainReportsWhatItNeverDelivered: when the run ends with
// records still unacknowledged, the drain's answer names each one by op, so
// the orchestrator can count them, and the sender stops.
func TestRecordSender_DrainReportsWhatItNeverDelivered(t *testing.T) {
	orch := newFakeOrchestrator(-1)
	sender, orchConn := newSenderPair(t, orch, testRecordSendPolicy)

	for _, sha := range []string{"a", "b"} {
		if err := sender.Send(RelayNamespaceCore, OpRecordPush, pushRecord(sha)); err != nil {
			t.Fatalf("Send: %v", err)
		}
	}
	if err := sender.Send(RelayNamespaceCore, OpRecordDenial, RecordDenialArgs{Owner: "acme", Repo: "widgets"}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	got, err := DrainRecords(context.Background(), orchConn)
	if err != nil {
		t.Fatalf("DrainRecords: %v", err)
	}
	want := []sidecarproto.UndeliveredRecords{
		{Namespace: RelayNamespaceCore, Op: OpRecordDenial, Count: 1},
		{Namespace: RelayNamespaceCore, Op: OpRecordPush, Count: 2},
	}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("undelivered = %+v; want %+v", got, want)
	}
	if err := sender.Send(RelayNamespaceCore, OpRecordPush, pushRecord("late")); err == nil {
		t.Fatal("Send after a drain succeeded; want it refused")
	}
}

// TestRecordSender_DrainWaitsForTheQueue: a drain that arrives while records
// are still being retried waits for them, so an outage that ends inside the
// drain's wait loses nothing.
func TestRecordSender_DrainWaitsForTheQueue(t *testing.T) {
	orch := newFakeOrchestrator(4)
	sender, orchConn := newSenderPair(t, orch, testRecordSendPolicy)

	if err := sender.Send(RelayNamespaceCore, OpRecordPush, pushRecord("a")); err != nil {
		t.Fatalf("Send: %v", err)
	}
	got, err := DrainRecords(context.Background(), orchConn)
	if err != nil {
		t.Fatalf("DrainRecords: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("undelivered = %+v; want none: the outage ended inside the drain's wait", got)
	}
	if _, applied := orch.snapshot(); len(applied) != 1 {
		t.Fatalf("applied %d records; want 1", len(applied))
	}
}

// unmarshalable is a payload json.Marshal always rejects.
type unmarshalable struct {
	Bad float64 `json:"bad"`
}

// TestRecordSender_CountsWhatNeverEnteredTheQueue: a record whose args do not
// marshal, and one that finds the queue full, are given up on at once — and
// still reported by the drain, so they are counted rather than only logged.
func TestRecordSender_CountsWhatNeverEnteredTheQueue(t *testing.T) {
	policy := testRecordSendPolicy
	policy.queueLimit = 1
	orch := newFakeOrchestrator(-1)
	sender, orchConn := newSenderPair(t, orch, policy)

	if err := sender.Send(RelayNamespaceCore, OpRecordGHWrite, unmarshalable{Bad: math.NaN()}); err == nil {
		t.Fatal("Send of an unmarshalable record succeeded")
	}
	if err := sender.Send(RelayNamespaceCore, OpRecordPush, pushRecord("held")); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if err := sender.Send(RelayNamespaceCore, OpRecordEgressDenial, RecordEgressDenialArgs{Target: "x:443"}); !errors.Is(err, errRecordQueueFull) {
		t.Fatalf("Send into a full queue = %v; want %v", err, errRecordQueueFull)
	}

	got, err := DrainRecords(context.Background(), orchConn)
	if err != nil {
		t.Fatalf("DrainRecords: %v", err)
	}
	counts := map[string]int{}
	for _, u := range got {
		counts[u.Op] += u.Count
	}
	want := map[string]int{OpRecordGHWrite: 1, OpRecordPush: 1, OpRecordEgressDenial: 1}
	if len(counts) != len(want) {
		t.Fatalf("undelivered = %+v; want %v", got, want)
	}
	for op, n := range want {
		if counts[op] != n {
			t.Fatalf("undelivered = %+v; want %v", got, want)
		}
	}
}

// panickingConn is a channel whose every call panics, standing in for a bug in
// the sending goroutine.
type panickingConn struct{ done chan struct{} }

func (c panickingConn) Call(context.Context, sidecarproto.Kind, any, any) error {
	panic("sender bug")
}

func (c panickingConn) Done() <-chan struct{} { return c.done }

// TestRecordSender_DrainDoesNotWaitOnADeadSender: once the sending goroutine
// is gone, nothing will empty the queue, so a drain reports what it holds at
// once instead of waiting out its bound.
func TestRecordSender_DrainDoesNotWaitOnADeadSender(t *testing.T) {
	policy := testRecordSendPolicy
	policy.drainWait = 5 * time.Second
	sender := newRecordSender(panickingConn{done: make(chan struct{})}, policy)

	if err := sender.Send(RelayNamespaceCore, OpRecordPush, pushRecord("a")); err != nil {
		t.Fatalf("Send: %v", err)
	}
	select {
	case <-sender.exited:
	case <-time.After(2 * time.Second):
		t.Fatal("the sending goroutine did not exit after panicking")
	}

	start := time.Now()
	res := sender.Drain(context.Background())
	if took := time.Since(start); took > time.Second {
		t.Fatalf("Drain took %v; want it to return at once on a dead sender", took)
	}
	if len(res.Undelivered) != 1 || res.Undelivered[0].Op != OpRecordPush || res.Undelivered[0].Count != 1 {
		t.Fatalf("undelivered = %+v; want the one queued push", res.Undelivered)
	}
}
