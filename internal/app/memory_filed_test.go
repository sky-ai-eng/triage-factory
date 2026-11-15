package app

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/delegate"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/pgnotify/wakebus"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// claimCountingQueue is the queue a running dispatcher reads, answering every
// pass with nothing to do and recording each claim attempt. A claim attempt is
// the only thing a dispatcher wake produces that is visible from outside the
// delegate package.
//
// The embedded nil interface is deliberate: a store method the loop reaches
// that is not answered here panics with its name.
type claimCountingQueue struct {
	db.ConversationQueueStore
	claims chan struct{}
}

func (q *claimCountingQueue) ClaimNextConversation(context.Context, string, int64, db.ClaimPlacement, time.Duration) (*domain.Conversation, error) {
	q.claims <- struct{}{}
	return nil, nil
}

func (q *claimCountingQueue) CheckOrphanedStepsSystem(context.Context) (db.OrphanedStepCheck, error) {
	return db.OrphanedStepCheck{}, nil
}

func (q *claimCountingQueue) TakeOverExpiredClaimsSystem(context.Context, string, int64, int) ([]db.ClaimRef, error) {
	return nil, nil
}

func (q *claimCountingQueue) SettleUnclaimedStopsSystem(context.Context) ([]db.SettledStop, error) {
	return nil, nil
}

func waitClaim(t *testing.T, q *claimCountingQueue, why string) {
	t.Helper()
	select {
	case <-q.claims:
	case <-time.After(2 * time.Second):
		t.Fatal(why)
	}
}

// Where the brain's process also runs the dispatcher, a filed memory row wakes
// that dispatcher directly. The scan interval is one the test never waits out,
// so the claim attempt after the boot drain can only be the wake's.
func TestApp_MemoryFiled_DispatcherIsWokenInProcess(t *testing.T) {
	queue := &claimCountingQueue{claims: make(chan struct{}, 8)}
	a := &App{
		plan:    subsystemPlan{role: runmode.RoleAll, brain: true, dispatcher: true},
		spawner: delegate.NewSpawner(nil, db.Stores{ConversationQueue: queue}, nil, nil, ""),
	}
	a.buildMemoryProvisioner()

	ctx, stop := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		a.spawner.RunDispatcher(ctx, time.Hour)
	}()
	t.Cleanup(func() {
		stop()
		<-done
	})

	waitClaim(t, queue, "the dispatcher never ran its boot drain")
	select {
	case <-queue.claims:
		t.Fatal("the dispatcher claimed again with no wake and no scan tick")
	case <-time.After(100 * time.Millisecond):
	}

	a.memoryFiled("org-1")
	waitClaim(t, queue, "a filed memory row did not wake the dispatcher")
}

// notifyRecorder is a database/sql driver that records every statement
// executed through it, standing in for the Postgres admin pool a control pod
// publishes over. Its statements return fail when it holds one.
type notifyRecorder struct {
	mu    sync.Mutex
	execs []recordedExec
	fail  error
}

type recordedExec struct {
	query string
	args  []driver.NamedValue
}

func (r *notifyRecorder) Connect(context.Context) (driver.Conn, error) { return notifyConn{r}, nil }
func (r *notifyRecorder) Driver() driver.Driver                        { return r }
func (r *notifyRecorder) Open(string) (driver.Conn, error)             { return notifyConn{r}, nil }

func (r *notifyRecorder) recorded() []recordedExec {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]recordedExec(nil), r.execs...)
}

type notifyConn struct{ r *notifyRecorder }

func (c notifyConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("notifyRecorder: prepared statements are not recorded")
}
func (c notifyConn) Close() error { return nil }
func (c notifyConn) Begin() (driver.Tx, error) {
	return nil, errors.New("notifyRecorder: transactions are not recorded")
}

func (c notifyConn) ExecContext(_ context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	c.r.mu.Lock()
	defer c.r.mu.Unlock()
	c.r.execs = append(c.r.execs, recordedExec{query: query, args: args})
	if c.r.fail != nil {
		return nil, c.r.fail
	}
	return driver.RowsAffected(0), nil
}

// newControlPodApp builds an App planned as a control pod: the brain with no
// dispatcher. It holds no spawner, so an in-process wake would panic instead
// of passing.
func newControlPodApp(t *testing.T, recorder *notifyRecorder) *App {
	t.Helper()
	database := sql.OpenDB(recorder)
	t.Cleanup(func() { database.Close() })
	a := &App{
		plan:     subsystemPlan{role: runmode.RoleControl, brain: true},
		database: database,
	}
	a.buildMemoryProvisioner()
	return a
}

// A control pod has no dispatcher to wake, so a filed memory row is published
// on tf_wake for the executors': one message, of kind memory, naming the org.
func TestApp_MemoryFiled_ControlPodPublishesOneMemoryWake(t *testing.T) {
	recorder := &notifyRecorder{}
	a := newControlPodApp(t, recorder)

	a.memoryFiled("org-1")

	execs := recorder.recorded()
	if len(execs) != 1 {
		t.Fatalf("statements executed = %d, want one publish", len(execs))
	}
	exec := execs[0]
	if !strings.Contains(exec.query, "pg_notify") || len(exec.args) != 2 {
		t.Fatalf("executed %q with %d args, want a pg_notify of a channel and a payload", exec.query, len(exec.args))
	}
	if channel, _ := exec.args[0].Value.(string); channel != wakebus.Channel {
		t.Errorf("published on %q, want %q", channel, wakebus.Channel)
	}
	payload, _ := exec.args[1].Value.(string)
	var got wakebus.Message
	if err := json.Unmarshal([]byte(payload), &got); err != nil {
		t.Fatalf("wake payload %q: %v", payload, err)
	}
	if want := (wakebus.Message{Kind: wakebus.KindMemory, OrgID: "org-1"}); got != want {
		t.Errorf("published %+v, want %+v", got, want)
	}
}

// The wake is best-effort: a publish that fails is dropped, and the queued
// conversation is claimed on the dispatcher's next scan instead.
func TestApp_MemoryFiled_ControlPodDropsAFailedPublish(t *testing.T) {
	recorder := &notifyRecorder{fail: errors.New("the pool is closed")}
	a := newControlPodApp(t, recorder)

	a.memoryFiled("org-1")

	if got := len(recorder.recorded()); got != 1 {
		t.Errorf("statements executed = %d, want the one publish and no retry", got)
	}
}
