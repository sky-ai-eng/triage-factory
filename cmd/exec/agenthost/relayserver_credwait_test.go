package agenthost

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// stubWorktrees is the reservation ledger half of db.RunWorktreeStore — the
// only method the credential wait reads. Every other method is unreachable
// from that path and panics rather than silently returning a zero value.
type stubWorktrees struct {
	rows []domain.RunWorktree
	err  error
}

func (s *stubWorktrees) ListSystem(context.Context, string, string) ([]domain.RunWorktree, error) {
	return s.rows, s.err
}

func (s *stubWorktrees) List(context.Context, string, string) ([]domain.RunWorktree, error) {
	panic("unexpected List")
}

func (s *stubWorktrees) Insert(context.Context, string, domain.RunWorktree) (bool, string, error) {
	panic("unexpected Insert")
}

func (s *stubWorktrees) InsertSystem(context.Context, string, domain.RunWorktree) (bool, string, error) {
	panic("unexpected InsertSystem")
}

func (s *stubWorktrees) GetByRepoRef(context.Context, string, string, string, string) (*domain.RunWorktree, error) {
	panic("unexpected GetByRepoRef")
}

func (s *stubWorktrees) GetByRepoRefSystem(context.Context, string, string, string, string) (*domain.RunWorktree, error) {
	panic("unexpected GetByRepoRefSystem")
}

func (s *stubWorktrees) DeleteByRepoRef(context.Context, string, string, string, string) error {
	panic("unexpected DeleteByRepoRef")
}

func (s *stubWorktrees) DeleteByRepoRefSystem(context.Context, string, string, string, string) error {
	panic("unexpected DeleteByRepoRefSystem")
}

func (s *stubWorktrees) DeleteByPathSystem(context.Context, string, string, string) error {
	panic("unexpected DeleteByPathSystem")
}

// credWaitServer builds a RelayServer whose reservation ledger holds rows and
// whose sealed bundle reports sealedAt (zero ⇒ no bundle provisioned yet).
func credWaitServer(t *testing.T, rows []domain.RunWorktree, sealedAt func() time.Time) (*RelayServer, *int32) {
	t.Helper()
	stores := db.Stores{RunWorktrees: &stubWorktrees{rows: rows}}
	srv := NewRelayServer(stores, RunInfo{OrgID: "org", RunID: "run"}, nil)
	var relayed int32
	srv.SetCredentialRefresh(&CredentialRefresh{
		SealedAt: func(context.Context) (time.Time, bool, error) {
			at := sealedAt()
			return at, !at.IsZero(), nil
		},
		Relay: func(context.Context) error {
			atomic.AddInt32(&relayed, 1)
			return nil
		},
	})
	return srv, &relayed
}

// TestAwaitCredentialsForRepo_UnwiredIsNoOp pins that the wait is inert
// wherever it isn't wired: local mode (no sealed bundles at all) and the
// curator's own RelayServer (which never serves a `workspace add`) must reach
// materialization exactly as they did before, with no store read and no delay.
func TestAwaitCredentialsForRepo_UnwiredIsNoOp(t *testing.T) {
	// Nil RunWorktrees would panic if the wait ever read the ledger.
	srv := NewRelayServer(db.Stores{}, RunInfo{OrgID: "org", RunID: "run"}, nil)
	if err := srv.awaitCredentialsForRepo(context.Background(), "sky-ai-eng", "triage-factory"); err != nil {
		t.Fatalf("unwired wait returned %v, want nil", err)
	}
}

// TestAwaitCredentialsForRepo_NoReservationDoesNotWait pins that a checkout
// with no conversation_worktrees row behind it widened nothing, so there is no
// re-seal to wait for.
func TestAwaitCredentialsForRepo_NoReservationDoesNotWait(t *testing.T) {
	srv, relayed := credWaitServer(t, nil, func() time.Time { return time.Time{} })
	if err := srv.awaitCredentialsForRepo(context.Background(), "sky-ai-eng", "triage-factory"); err != nil {
		t.Fatalf("wait with no reservation returned %v, want nil", err)
	}
	if got := atomic.LoadInt32(relayed); got != 0 {
		t.Errorf("relayed %d bundles, want 0", got)
	}
}

// TestAwaitCredentialsForRepo_RepeatAddDoesNotWait pins the idempotent re-add:
// the reservation is old, so any seal since then already postdates it and the
// wait clears on its first read.
func TestAwaitCredentialsForRepo_RepeatAddDoesNotWait(t *testing.T) {
	reserved := time.Now().Add(-time.Hour)
	rows := []domain.RunWorktree{{RepoID: "sky-ai-eng/triage-factory", Ref: "@default", CreatedAt: reserved}}
	srv, relayed := credWaitServer(t, rows, func() time.Time { return reserved.Add(time.Minute) })

	start := time.Now()
	if err := srv.awaitCredentialsForRepo(context.Background(), "sky-ai-eng", "triage-factory"); err != nil {
		t.Fatalf("wait on an already-fresh bundle returned %v, want nil", err)
	}
	if elapsed := time.Since(start); elapsed > workspaceCredPollInterval {
		t.Errorf("wait took %s; an already-fresh bundle must clear on the first read", elapsed)
	}
	if got := atomic.LoadInt32(relayed); got != 1 {
		t.Errorf("relayed %d bundles, want exactly 1 push to the sidecar", got)
	}
}

// TestAwaitCredentialsForRepo_WaitsForASealAfterTheReservation is the bug this
// exists for: the run's bundle predates the reservation (it was sealed for the
// narrower repo set), so the wait must hold until the brain's re-seal lands and
// only then hand it to the sidecar. Freshness comes from the timestamp alone —
// the wait never sees, and cannot see, what the bundle contains.
func TestAwaitCredentialsForRepo_WaitsForASealAfterTheReservation(t *testing.T) {
	reserved := time.Now()
	rows := []domain.RunWorktree{{RepoID: "sky-ai-eng/triage-factory", Ref: "@default", CreatedAt: reserved}}

	// Starts stale (sealed before the reservation); the brain's re-seal lands
	// mid-wait.
	var resealed atomic.Bool
	srv, relayed := credWaitServer(t, rows, func() time.Time {
		if resealed.Load() {
			return reserved.Add(time.Second)
		}
		return reserved.Add(-time.Minute)
	})
	go func() {
		time.Sleep(2 * workspaceCredPollInterval)
		resealed.Store(true)
	}()

	if err := srv.awaitCredentialsForRepo(context.Background(), "sky-ai-eng", "triage-factory"); err != nil {
		t.Fatalf("wait returned %v, want nil once the re-seal landed", err)
	}
	if got := atomic.LoadInt32(relayed); got != 1 {
		t.Errorf("relayed %d bundles, want exactly 1 push to the sidecar", got)
	}
}

// TestAwaitCredentialsForRepo_NewestReservationWins pins that a run holding
// several checkouts of one repo waits out the LATEST of them — an older row for
// the same repo must not let a pre-widening bundle through.
func TestAwaitCredentialsForRepo_NewestReservationWins(t *testing.T) {
	old := time.Now().Add(-time.Hour)
	fresh := time.Now()
	rows := []domain.RunWorktree{
		{RepoID: "sky-ai-eng/triage-factory", Ref: "@default", CreatedAt: old},
		{RepoID: "sky-ai-eng/triage-factory", Ref: "pr-42", CreatedAt: fresh},
		{RepoID: "sky-ai-eng/other", Ref: "@default", CreatedAt: fresh.Add(time.Hour)},
	}
	// Sealed between the two reservations for this repo: newer than the old
	// row, older than the one the checkout answers.
	srv, _ := credWaitServer(t, rows, func() time.Time { return old.Add(time.Minute) })

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	err := srv.awaitCredentialsForRepo(ctx, "sky-ai-eng", "triage-factory")
	if err == nil {
		t.Fatal("wait cleared on a bundle older than the newest reservation for the repo")
	}
}

// TestAwaitCredentialsForRepo_TimesOutNamingTheRepo pins the brain-unreachable
// shape: the op fails before the clone starts (never a 502 mid-clone), and the
// message names the repo and says a retry will work — which it does, because
// our doorbell's provision lands regardless.
func TestAwaitCredentialsForRepo_TimesOutNamingTheRepo(t *testing.T) {
	if workspaceCredWaitTimeout > 15*time.Second {
		t.Fatalf("workspaceCredWaitTimeout = %s; the op must fail inside 15s, well within the executor's awaiting-credentials deadline", workspaceCredWaitTimeout)
	}
	reserved := time.Now()
	rows := []domain.RunWorktree{{RepoID: "sky-ai-eng/triage-factory", Ref: "@default", CreatedAt: reserved}}
	// The brain never provisions: no bundle at all.
	srv, relayed := credWaitServer(t, rows, func() time.Time { return time.Time{} })

	// Bounded by the caller's own deadline so the test doesn't sit out the
	// full production window; the timeout branch is the same one either way.
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	err := srv.awaitCredentialsForRepo(ctx, "sky-ai-eng", "triage-factory")
	if err == nil {
		t.Fatal("wait with no bundle returned nil; want a timeout naming the repo")
	}
	if !strings.Contains(err.Error(), "sky-ai-eng/triage-factory") {
		t.Errorf("error = %v; want it to name the repo the agent asked for", err)
	}
	if !strings.Contains(err.Error(), "retry") {
		t.Errorf("error = %v; want it to tell the agent a retry will work", err)
	}
	if got := atomic.LoadInt32(relayed); got != 0 {
		t.Errorf("relayed %d bundles on a timeout, want 0", got)
	}
}

// TestAwaitCredentialsForRepo_RelayFailureFailsTheOp pins that a bundle that
// reached the database but not the sidecar is still a failure: the sidecar is
// what holds the token the clone authenticates with.
func TestAwaitCredentialsForRepo_RelayFailureFailsTheOp(t *testing.T) {
	reserved := time.Now()
	rows := []domain.RunWorktree{{RepoID: "sky-ai-eng/triage-factory", Ref: "@default", CreatedAt: reserved}}
	stores := db.Stores{RunWorktrees: &stubWorktrees{rows: rows}}
	srv := NewRelayServer(stores, RunInfo{OrgID: "org", RunID: "run"}, nil)
	srv.SetCredentialRefresh(&CredentialRefresh{
		SealedAt: func(context.Context) (time.Time, bool, error) { return reserved.Add(time.Second), true, nil },
		Relay:    func(context.Context) error { return errors.New("sidecar supervision channel is gone") },
	})

	err := srv.awaitCredentialsForRepo(context.Background(), "sky-ai-eng", "triage-factory")
	if err == nil || !strings.Contains(err.Error(), "sidecar") {
		t.Fatalf("error = %v; want the relay failure surfaced rather than a clone attempted with the old bundle", err)
	}
}

// TestRepoReservedAt_MatchesCaseInsensitively pins the spelling seam: the
// reservation records the agent's casing while the checkout op carries the repo
// profile's, so a case difference must not read as "no reservation" and skip
// the wait entirely.
func TestRepoReservedAt_MatchesCaseInsensitively(t *testing.T) {
	reserved := time.Now()
	rows := []domain.RunWorktree{{RepoID: "Sky-AI-Eng/Triage-Factory", Ref: "@default", CreatedAt: reserved}}
	srv, _ := credWaitServer(t, rows, func() time.Time { return time.Time{} })

	at, ok, err := srv.repoReservedAt(context.Background(), "sky-ai-eng/triage-factory")
	if err != nil || !ok {
		t.Fatalf("repoReservedAt: ok=%v err=%v, want the differently-cased row matched", ok, err)
	}
	if !at.Equal(reserved) {
		t.Errorf("repoReservedAt = %s, want %s", at, reserved)
	}
}
