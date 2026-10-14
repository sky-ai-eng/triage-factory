package agentproc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime/debug"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/sky-ai-eng/triage-factory/internal/sidecarproto"
)

// RecordSender delivers a run's audit records from the sidecar to the
// orchestrator, which holds the database that turns each into a row. One per
// supervision channel, shared by everything in the sidecar that records —
// the exec verbs, the git proxy, the gh channel, the egress proxy — because the
// order records arrive in is the order they are applied in, and only a single
// sender can keep it.
//
// Each record describes an act that has already happened, so Send never waits
// on the wire: it queues the record and returns, before the act it describes
// returns to the agent, which is what makes queue order the order of the acts.
// One goroutine sends the queue: it sends the head as a KindRelayRecord Call
// and moves on only once the orchestrator acknowledges it. An error answer, an
// answer that never came, or a full record lane leaves the head where it is,
// to be sent again after a backoff, until it is acknowledged or the run ends.
// The orchestrator applies records one at a time (sidecarproto's record lane),
// so a record that took three attempts is still applied before the one queued
// after it.
//
// The queue lives in this process's memory and nowhere else. A record survives
// anything the orchestrator recovers from while this process is up — a
// database outage, an exhausted pool — but not this process's own death. A
// spool on disk would not change that: the supervision channel is the only way
// a record leaves this process, it is never re-established (the sidecar exits
// when it closes), and the cell teardown that follows deletes the per-run files
// a successor would have needed to read. Nothing would ever replay it.
//
// A record the sender gives up on is counted rather than lost silently: Drain
// reports them, by op, to the orchestrator at the end of the run.
type RecordSender struct {
	conn   recordConn
	policy recordSendPolicy

	mu    sync.Mutex
	queue []sidecarproto.RelayRecordBody
	// empty closes when the queue next becomes empty; a fresh one is made each
	// time the queue goes from empty to non-empty. Drain waits on it.
	empty chan struct{}
	// undelivered counts, by op, the records given up on so far, for Drain to
	// report.
	undelivered map[recordName]int
	stopped     bool

	// wake carries "a record was queued" to the sending goroutine; buffered
	// so Send never blocks on it.
	wake chan struct{}
	// stop closes when Drain stops the sender.
	stop chan struct{}
	// exited closes when the sending goroutine returns, however it returns —
	// so a Drain after a panic in it reports at once rather than waiting for a
	// queue nothing will empty.
	exited chan struct{}
}

// recordConn is the slice of *sidecarproto.Conn a RecordSender drives,
// declared so a test can stand in for the channel.
type recordConn interface {
	Call(ctx context.Context, kind sidecarproto.Kind, reqBody, respBody any) error
	Done() <-chan struct{}
}

// recordName is the (namespace, op) a record is counted under.
type recordName struct{ namespace, op string }

// recordSendPolicy is the sender's bounds, a value so a test can shrink them.
type recordSendPolicy struct {
	// queueLimit bounds the records held unacknowledged. One record per
	// external act, a few hundred bytes each: the bound only matters for a run
	// whose orchestrator never recovers, which would otherwise hold memory
	// without end.
	queueLimit int
	// callTimeout bounds one delivery attempt. The orchestrator bounds its own
	// application of a record, so an answer this late is an answer lost, and
	// the record is sent again.
	callTimeout time.Duration
	// resendInitial and resendMax bound the backoff between attempts on one
	// record, which doubles from the first to the second.
	resendInitial, resendMax time.Duration
	// drainWait bounds how long Drain waits for the queue to empty. It holds
	// up the run's cell teardown, so it is short.
	drainWait time.Duration
}

var defaultRecordSendPolicy = recordSendPolicy{
	queueLimit:    4096,
	callTimeout:   time.Minute,
	resendInitial: 250 * time.Millisecond,
	resendMax:     5 * time.Second,
	drainWait:     10 * time.Second,
}

// drainAnswerSlack is how much longer the orchestrator waits for the drain's
// answer than the sidecar waits for its queue to empty.
const drainAnswerSlack = 2 * time.Second

var (
	errRecordQueueFull = errors.New("agentproc: audit record queue full")
	errRecordsDrained  = errors.New("agentproc: audit records already drained")
)

// NewRecordSender starts the sender for one supervision channel. It stops when
// the channel closes or Drain is called.
func NewRecordSender(conn *sidecarproto.Conn) *RecordSender {
	return newRecordSender(conn, defaultRecordSendPolicy)
}

func newRecordSender(conn recordConn, policy recordSendPolicy) *RecordSender {
	s := &RecordSender{
		conn:        conn,
		policy:      policy,
		undelivered: map[recordName]int{},
		wake:        make(chan struct{}, 1),
		stop:        make(chan struct{}),
		exited:      make(chan struct{}),
	}
	// Recovers — see run.
	go s.run()
	return s
}

// Send queues one audit record and returns without waiting on the wire. The
// record gets its key here, once, so every attempt to deliver it carries the
// same one.
//
// A non-nil error means the record will never be delivered: its args did not
// marshal, the queue is full, or the sender has been drained. In the first two
// cases it has been counted for Drain to report; after a drain there is no one
// left to report to. The error is the caller's to log — the act the record
// describes already happened, and nothing here may fail it.
func (s *RecordSender) Send(namespace, op string, args any) error {
	raw, err := json.Marshal(args)
	name := recordName{namespace: namespace, op: op}
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case s.stopped:
		return errRecordsDrained
	case err != nil:
		s.undelivered[name]++
		return fmt.Errorf("agentproc: marshal audit record %s/%s: %w", namespace, op, err)
	case len(s.queue) >= s.policy.queueLimit:
		s.undelivered[name]++
		return errRecordQueueFull
	}
	if len(s.queue) == 0 {
		s.empty = make(chan struct{})
	}
	s.queue = append(s.queue, sidecarproto.RelayRecordBody{
		Namespace: namespace, Op: op, Key: uuid.NewString(), Args: raw,
	})
	select {
	case s.wake <- struct{}{}:
	default:
	}
	return nil
}

// Drain waits for the queue to empty, up to the policy's drain wait, then stops
// the sender and returns every record it gave up on, by op. Called once, when
// the orchestrator asks at the end of the run; a record sent after it is
// refused.
//
// A record whose delivery was in flight when the wait ran out is reported as
// undelivered even if its acknowledgement arrives a moment later, so at the
// edge this can count one record lost that landed.
func (s *RecordSender) Drain(ctx context.Context) sidecarproto.DrainRecordsResult {
	ctx, cancel := context.WithTimeout(ctx, s.policy.drainWait)
	defer cancel()
	s.mu.Lock()
	pending, empty := len(s.queue) > 0, s.empty
	s.mu.Unlock()
	if pending {
		select {
		case <-empty:
		case <-ctx.Done():
		case <-s.conn.Done():
		case <-s.exited:
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.stopped {
		s.stopped = true
		close(s.stop)
	}
	for _, r := range s.queue {
		s.undelivered[recordName{namespace: r.Namespace, op: r.Op}]++
	}
	s.queue = nil
	var res sidecarproto.DrainRecordsResult
	for name, n := range s.undelivered {
		res.Undelivered = append(res.Undelivered, sidecarproto.UndeliveredRecords{
			Namespace: name.namespace, Op: name.op, Count: n,
		})
	}
	sort.Slice(res.Undelivered, func(i, j int) bool {
		a, b := res.Undelivered[i], res.Undelivered[j]
		if a.Namespace != b.Namespace {
			return a.Namespace < b.Namespace
		}
		return a.Op < b.Op
	})
	s.undelivered = map[recordName]int{}
	return res
}

// run is the sender's only goroutine: it delivers the queue head, waits for
// the acknowledgement, and only then takes the next record. It recovers from a
// panic because it runs in the sidecar (see cmd/runsidecar's goroutine rule);
// a panic ends delivery, and the records still queued are lost with it.
func (s *RecordSender) run() {
	defer close(s.exited)
	defer func() {
		if rec := recover(); rec != nil {
			agentprocLog.Error("audit record sender panicked; queued records will not be delivered",
				"panic", fmt.Sprint(rec), "stack", string(debug.Stack()))
		}
	}()
	delay := s.policy.resendInitial
	for {
		head, ok := s.next()
		if !ok {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), s.policy.callTimeout)
		err := s.conn.Call(ctx, sidecarproto.KindRelayRecord, head, nil)
		cancel()
		if err == nil {
			s.acknowledged(head.Key)
			delay = s.policy.resendInitial
			continue
		}
		agentprocLog.Warn("audit record not acknowledged; it will be sent again",
			"namespace", head.Namespace, "op", head.Op, "retry_in", delay, "error", err)
		t := time.NewTimer(delay)
		select {
		case <-t.C:
		case <-s.stop:
			t.Stop()
			return
		case <-s.conn.Done():
			t.Stop()
			s.logLost()
			return
		}
		delay = min(2*delay, s.policy.resendMax)
	}
}

// next blocks until the queue has a head and returns it, leaving it queued
// until it is acknowledged. False once the sender is drained or the channel is
// gone.
func (s *RecordSender) next() (sidecarproto.RelayRecordBody, bool) {
	for {
		s.mu.Lock()
		if s.stopped {
			s.mu.Unlock()
			return sidecarproto.RelayRecordBody{}, false
		}
		if len(s.queue) > 0 {
			head := s.queue[0]
			s.mu.Unlock()
			return head, true
		}
		s.mu.Unlock()
		select {
		case <-s.wake:
		case <-s.stop:
			return sidecarproto.RelayRecordBody{}, false
		case <-s.conn.Done():
			s.logLost()
			return sidecarproto.RelayRecordBody{}, false
		}
	}
}

// acknowledged removes the head the orchestrator just acknowledged. The key
// check covers a drain that emptied the queue while the delivery was in
// flight.
func (s *RecordSender) acknowledged(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.queue) == 0 || s.queue[0].Key != key {
		return
	}
	s.queue[0] = sidecarproto.RelayRecordBody{}
	s.queue = s.queue[1:]
	if len(s.queue) == 0 {
		close(s.empty)
	}
}

// logLost names what the sender still held when the channel closed before any
// drain. Nothing can be counted then — the orchestrator is on the far side of
// the closed channel — so this log line is all these records get.
func (s *RecordSender) logLost() {
	s.mu.Lock()
	queued := len(s.queue)
	given := 0
	for _, n := range s.undelivered {
		given += n
	}
	s.mu.Unlock()
	if queued+given > 0 {
		agentprocLog.Warn("supervision channel closed with audit records undelivered",
			"queued", queued, "given_up", given)
	}
}

// DrainRecords asks the run's sidecar to finish delivering its audit records
// and stop sending, and returns the records it never delivered, by op. The
// orchestrator calls it once, as the run's cell is torn down and before the
// supervision channel closes. The sidecar bounds its own wait, and the answer
// is awaited a little longer than that. An error means no answer came — the
// channel was already gone — and nothing can be said about what the sidecar
// still held.
func DrainRecords(ctx context.Context, conn *sidecarproto.Conn) ([]sidecarproto.UndeliveredRecords, error) {
	ctx, cancel := context.WithTimeout(ctx, defaultRecordSendPolicy.drainWait+drainAnswerSlack)
	defer cancel()
	var res sidecarproto.DrainRecordsResult
	if err := conn.Call(ctx, sidecarproto.KindDrainRecords, nil, &res); err != nil {
		return nil, err
	}
	return res.Undelivered, nil
}
