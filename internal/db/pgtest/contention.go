package pgtest

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// contentionSampleInterval is how often a statement in flight is asked who
// else is live. Long enough that the ordinary sub-10ms truncate never takes a
// sample at all, short enough to land inside the one-second wait Postgres
// gives a lock before it declares a deadlock.
const contentionSampleInterval = 250 * time.Millisecond

// busySettle is how long AssertNoBusyBackends waits before it starts looking,
// and busyProbeWindow how long it looks for. The settle is for the one thing a
// correct cleanup still leaves behind: a statement the client cancelled on its
// way out, whose backend goes on running until the cancel request reaches it.
// The window is because of what it is looking for — see busyDuring.
const (
	busySettle      = 100 * time.Millisecond
	busyProbeWindow = 300 * time.Millisecond
)

// busyBackend is one session other than the caller's own that is mid-statement
// or holding a transaction open — the only two states in which a session holds
// a lock another one can lose to.
type busyBackend struct {
	pid       int
	state     string
	waitEvent string
	xactAge   float64
	query     string
}

func (b busyBackend) String() string {
	wait := b.waitEvent
	if wait == "" {
		wait = "nothing"
	}
	return fmt.Sprintf("pid %d: %s, waiting on %s, %.1fs into its transaction — %s",
		b.pid, b.state, wait, b.xactAge, b.query)
}

// busyBackends lists them. An idle session is excluded because it holds no
// table lock: Postgres releases those at commit, so a connection sitting in a
// pool between statements can block nothing.
func busyBackends(ctx context.Context, db *sql.DB) ([]busyBackend, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT pid,
		       state,
		       COALESCE(wait_event_type || ':' || wait_event, ''),
		       COALESCE(EXTRACT(EPOCH FROM now() - xact_start), 0),
		       COALESCE(NULLIF(left(query, 400), ''), '(no statement recorded)')
		FROM pg_stat_activity
		WHERE datname = current_database()
		  AND pid <> pg_backend_pid()
		  AND backend_type = 'client backend'
		  AND state IN ('active', 'idle in transaction', 'idle in transaction (aborted)')
		ORDER BY xact_start NULLS LAST, pid
	`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []busyBackend
	for rows.Next() {
		var b busyBackend
		if err := rows.Scan(&b.pid, &b.state, &b.waitEvent, &b.xactAge, &b.query); err != nil {
			return nil, err
		}
		b.query = strings.Join(strings.Fields(b.query), " ")
		out = append(out, b)
	}
	return out, rows.Err()
}

// describeBusyBackends renders them for an error or a failure message. Empty
// for an empty list, so a caller can concatenate it unconditionally.
func describeBusyBackends(backends []busyBackend) string {
	if len(backends) == 0 {
		return ""
	}
	var b strings.Builder
	for _, backend := range backends {
		b.WriteString("\n\t")
		b.WriteString(backend.String())
	}
	return b.String()
}

// watchContention samples the other live sessions while a statement is in
// flight, returning a stop function that yields the last non-empty sample.
//
// Sampling has to happen during the statement, not after it fails: Postgres
// breaks a deadlock by cancelling one side, and the side that won has usually
// committed and gone idle by the time the loser's error surfaces — so a
// snapshot taken on the error path routinely finds nobody at all.
func watchContention(db *sql.DB) (stop func() []busyBackend) {
	done := make(chan struct{})
	var (
		mu   sync.Mutex
		last []busyBackend
		wg   sync.WaitGroup
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(contentionSampleInterval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				got, err := busyBackends(ctx, db)
				cancel()
				if err != nil || len(got) == 0 {
					continue
				}
				mu.Lock()
				last = got
				mu.Unlock()
			}
		}
	}()
	return func() []busyBackend {
		close(done)
		wg.Wait()
		mu.Lock()
		defer mu.Unlock()
		return last
	}
}

// busyDuring samples across window and returns every session it caught busy at
// least once, most recent observation per pid.
//
// A window rather than a moment, and one that never stops early. A goroutine
// still working after its test is visible only while it is mid-statement — it
// sits idle between them — so a single sample sees it or misses it by luck,
// and a loop that stopped at the first quiet sample would read exactly those
// gaps as an all-clear. Watching right through the window and keeping
// everything seen improves the odds against that, and costs a fixture whose
// goroutines really are joined nothing but the window itself. What it cannot
// do is see a goroutine that has already finished — which is why this is a
// net, and the joins are the guarantee.
func busyDuring(ctx context.Context, db *sql.DB, window time.Duration) ([]busyBackend, error) {
	seen := map[int]busyBackend{}
	deadline := time.Now().Add(window)
	for {
		got, err := busyBackends(ctx, db)
		if err != nil {
			return nil, err
		}
		for _, b := range got {
			seen[b.pid] = b
		}
		if time.Now().After(deadline) {
			break
		}
		select {
		case <-time.After(window / 10):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	out := make([]busyBackend, 0, len(seen))
	for _, b := range seen {
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].pid < out[j].pid })
	return out, nil
}

// AssertNoBusyBackends fails t if any session other than this one is still
// running a statement or holding a transaction open.
//
// Call it from a fixture's t.Cleanup, after joining whatever the fixture
// started. A test's own work is finished by then, so anything still live is a
// goroutine that outlived the test that created it — and the next test opens
// with Reset, whose TRUNCATE takes ACCESS EXCLUSIVE on every table in turn and
// deadlocks against exactly that. Catching it here names the test that leaked
// it; catching it at the next Reset names only the test that paid for it.
//
// It is a net, not a proof: it sees a goroutine only while that goroutine is
// talking to the server (see busyDuring), so a leak that finishes quickly
// slips past it. That is the right way round — the leak worth catching is the
// slow one on a loaded machine, which is the same leak that is still going
// when the next test truncates. It backs up the joins a fixture does; it does
// not replace them.
func (h *Harness) AssertNoBusyBackends(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	time.Sleep(busySettle)
	busy, err := busyDuring(ctx, h.AdminDB, busyProbeWindow)
	if err != nil {
		t.Errorf("AssertNoBusyBackends: read pg_stat_activity: %v", err)
		return
	}
	if len(busy) == 0 {
		return
	}
	t.Errorf("%d database session(s) still live now the test has returned; the next test's Reset will deadlock against them:%s",
		len(busy), describeBusyBackends(busy))
}
