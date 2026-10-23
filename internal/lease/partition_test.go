package lease

import (
	"context"
	"fmt"
	"io"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/logging"
)

// hungRenewalStore wins one acquisition and then gets no renewal through.
// EnsureRow answers after ensureLatency, the winning TryAcquire commits when
// it is called and answers after acquireLatency, and each Renew blocks until
// its context expires, the way a pgx call on a silently black-holed
// connection does.
type hungRenewalStore struct {
	failedAcquires int // TryAcquire calls that find the lease fresh before the one that wins
	ensureLatency  time.Duration
	acquireLatency time.Duration

	mu         sync.Mutex
	acquires   int
	commitTime time.Time
}

func (s *hungRenewalStore) EnsureRow(context.Context, string) error {
	time.Sleep(s.ensureLatency)
	return nil
}

func (s *hungRenewalStore) TryAcquire(context.Context, string, string, time.Duration) (int64, bool, error) {
	s.mu.Lock()
	s.acquires++
	won := s.acquires == s.failedAcquires+1
	if won {
		s.commitTime = time.Now()
	}
	s.mu.Unlock()
	if !won {
		return 0, false, nil
	}
	time.Sleep(s.acquireLatency)
	return 1, true, nil
}

func (s *hungRenewalStore) Renew(ctx context.Context, _, _ string, _ int64) (bool, error) {
	<-ctx.Done()
	return false, ctx.Err()
}

func (s *hungRenewalStore) Read(context.Context, string) (string, int64, error) {
	return "pod-b", 0, nil
}

func (s *hungRenewalStore) committed() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.commitTime
}

// TestManager_PartitionedHolderDemotesBeforeTakeover runs the real Run loop
// against a holder whose every renewal hangs until its store-call timeout, and
// asserts that it demotes before a standby may acquire: commit time + TTL,
// since the row's renewed_at is the winning TryAcquire's commit. The bubble's
// fake clock drives the tickers and the store-call timeouts, so each case
// costs no wall time. The sweep over renew interval, failed acquire attempts
// and the latencies of EnsureRow and the winning TryAcquire moves the
// acquire commit across both tickers' phases, and the TryAcquire latency
// separates its commit from its return. When both tickers are ready, Run's
// select picks one at random, as it does in production. The configurations
// are the default knobs, a generous TTL, and, for a range of demote
// deadlines, the smallest TTL the margin check accepts.
func TestManager_PartitionedHolderDemotesBeforeTakeover(t *testing.T) {
	restore := logging.SetOutput(io.Discard)
	defer restore()

	type knobs struct{ ttl, demote time.Duration }
	// A 40s TTL makes the store-call timeout (5s) the larger term.
	configs := []knobs{{DefaultTTL, DefaultDemoteDeadline}, {40 * time.Second, DefaultDemoteDeadline}}
	for _, demote := range []time.Duration{1, 2, 3, 4, 5, 7, 10, 15, 30} {
		demote *= time.Second
		ttl := demote + time.Second
		for demoteMarginError(demote, ttl) != nil {
			ttl += time.Second
		}
		configs = append(configs, knobs{ttl, demote})
	}

	for _, k := range configs {
		timeout := storeCallTimeout(k.ttl, k.demote)
		latencies := []time.Duration{0, timeout / 2, timeout - 100*time.Millisecond}
		for renew := time.Second; renew <= 5*time.Second; renew += time.Second {
			for _, latency := range latencies {
				for _, ensure := range latencies {
					for failed := 0; failed <= 3; failed++ {
						store := hungRenewalStore{failedAcquires: failed, ensureLatency: ensure, acquireLatency: latency}
						name := fmt.Sprintf("ttl=%s/demote=%s/renew=%s/acquire=%s/ensure=%s/failed=%d", k.ttl, k.demote, renew, latency, ensure, failed)
						t.Run(name, func(t *testing.T) {
							synctest.Test(t, func(t *testing.T) {
								assertDemotesBeforeTakeover(t, &store, renew, k.ttl, k.demote)
							})
						})
					}
				}
			}
		}
	}
}

func assertDemotesBeforeTakeover(t *testing.T, store *hungRenewalStore, renew, ttl, demote time.Duration) {
	// A Run that never demotes on its own is shut down well past the TTL, so
	// the shutdown demotion lands late and fails the assertion below.
	horizon := time.Duration(store.failedAcquires+1)*renew + 3*ttl
	ctx, cancel := context.WithTimeout(context.Background(), horizon)
	defer cancel()

	var demotedAt time.Time
	var reason string
	m, err := NewManager(store, "x", "pod-a", renew, ttl, demote, nil, nil, func(r string) {
		demotedAt, reason = time.Now(), r
		cancel()
	})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		m.Run(ctx)
		close(done)
	}()
	<-done

	commit := store.committed()
	if commit.IsZero() {
		t.Fatal("the store never committed an acquisition")
	}
	if demotedAt.IsZero() {
		t.Fatal("the holder never demoted")
	}
	eligible := commit.Add(ttl)
	if !demotedAt.Before(eligible) {
		t.Fatalf("demoted %s after the acquire commit (reason %q); a standby may acquire %s after it", demotedAt.Sub(commit), reason, ttl)
	}
	if demotedAt.Sub(commit) < demote {
		t.Fatalf("demoted %s after the acquire commit, before the %s demote deadline elapsed", demotedAt.Sub(commit), demote)
	}
}
