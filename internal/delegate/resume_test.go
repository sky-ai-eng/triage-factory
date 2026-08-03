package delegate

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/paths"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/storage"
)

// TestResumeOpenRun_EmptyUserID guards the first gate: a resume must carry the
// resuming user's identity (its writes route under that user's synthetic
// claims). No DB touched.
func TestResumeOpenRun_EmptyUserID(t *testing.T) {
	s := NewSpawner(nil, db.Stores{}, nil, nil, "")
	if err := s.ResumeOpenRun(context.Background(), runmode.LocalDefaultOrgID, "any", "msg", ""); err == nil {
		t.Fatal("expected an error for an empty user id")
	}
}

// TestResumeOpenRun_ValidationGuards walks the field guards that reject a
// resume before the pending-input write / requeue flip — each returns a
// plain error the caller surfaces. seedRun gives a complete row; each case
// blanks the one field under test.
func TestResumeOpenRun_ValidationGuards(t *testing.T) {
	cases := []struct {
		name    string
		mutate  string // SQL to blank the field under test
		wantSub string
	}{
		{"no session", `UPDATE conversations SET sdk_session_id = NULL WHERE id = ?`, "session id"},
		{"no worktree", `UPDATE conversations SET worktree_path = NULL WHERE id = ?`, "worktree path"},
		{"no model", `UPDATE conversations SET model = NULL WHERE id = ?`, "model"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			database := newDelegateTestDB(t)
			seedRun(t, database, "r-guard", "sess", "/tmp/wt")
			if _, err := database.Exec(`UPDATE conversations SET status='open' WHERE id='r-guard'`); err != nil {
				t.Fatalf("open: %v", err)
			}
			if _, err := database.Exec(tc.mutate, "r-guard"); err != nil {
				t.Fatalf("mutate: %v", err)
			}
			s := NewSpawner(database, testSpawnerStores(database), nil, nil, "m")

			err := s.ResumeOpenRun(context.Background(), runmode.LocalDefaultOrgID, "r-guard", "msg", runmode.LocalDefaultUserID)
			if err == nil || !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("err = %v, want one mentioning %q", err, tc.wantSub)
			}
		})
	}
}

// TestResumeOpenRun_NotResumable pins the resumable-state guard: a failed run
// can't be woken — its workspace did not survive — so ResumeOpenRun returns
// ErrRunNotResumable for the caller to map to 409, before any DB write.
func TestResumeOpenRun_NotResumable(t *testing.T) {
	database := newDelegateTestDB(t)
	seedRun(t, database, "r-term", "sess", "/tmp/wt")
	if _, err := database.Exec(`UPDATE conversations SET status='failed' WHERE id='r-term'`); err != nil {
		t.Fatalf("fail: %v", err)
	}
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "m")

	err := s.ResumeOpenRun(context.Background(), runmode.LocalDefaultOrgID, "r-term", "msg", runmode.LocalDefaultUserID)
	if !errors.Is(err, ErrRunNotResumable) {
		t.Errorf("err = %v, want ErrRunNotResumable", err)
	}
}

// TestResumeOpenRun_RefusalTaxonomy pins which of three answers a parked run
// gets, and the ORDER the two structural ones are asked in. All three rows
// look identically resumable from the conversation alone, so the whole
// distinction lives in what is true around them:
//
//   - workspace reaped by the retention TTL → 410 Gone.
//   - workspace intact, blueprint cancelled → 409 concluded. A blueprint that
//     merely FINISHED is not in this set — that is the follow-up case and it
//     succeeds; called off is the one that still refuses.
//   - both                                  → the workspace answer wins,
//     because telling someone their sequence was cancelled implies there is
//     something left to cancel back into, and there isn't.
//
// The third case is not hypothetical: it is every run stopped by an older
// build. The old cancel path discarded the snapshot AND cancelled the
// blueprint, so a migrated row has both conditions and must answer 410.
func TestResumeOpenRun_RefusalTaxonomy(t *testing.T) {
	park := func(t *testing.T, database *sql.DB, id string, blueprintStatus string, keepWorkspace bool) *Spawner {
		t.Helper()
		wt := "/tmp/does-not-exist-" + id
		if keepWorkspace {
			wt = t.TempDir() // a warm worktree on disk === recoverable
		}
		seedRun(t, database, id, "sess-"+id, wt)
		if _, err := database.Exec(`UPDATE conversations SET status='open' WHERE id=?`, id); err != nil {
			t.Fatalf("park: %v", err)
		}
		if _, err := database.Exec(`UPDATE blueprint_runs SET status=? WHERE id=?`, blueprintStatus, "seedbpr-"+id); err != nil {
			t.Fatalf("set blueprint status: %v", err)
		}
		s := NewSpawner(database, testSpawnerStores(database), nil, nil, "m")
		// A blob store with nothing in it: no snapshot to rehydrate from, so a
		// run whose worktree is also gone is unrecoverable for real.
		blobs, err := storage.New()
		if err != nil {
			t.Fatalf("storage.New: %v", err)
		}
		s.SetStorage(blobs)
		return s
	}

	t.Run("workspace reaped, blueprint running", func(t *testing.T) {
		paths.SetForTest(t, t.TempDir())
		database := newDelegateTestDB(t)
		s := park(t, database, "r-reaped", "running", false)
		err := s.ResumeOpenRun(context.Background(), runmode.LocalDefaultOrgID, "r-reaped", "msg", runmode.LocalDefaultUserID)
		if !errors.Is(err, ErrWorkspaceExpired) {
			t.Errorf("err = %v, want ErrWorkspaceExpired (410)", err)
		}
	})

	t.Run("workspace intact, blueprint cancelled", func(t *testing.T) {
		paths.SetForTest(t, t.TempDir())
		database := newDelegateTestDB(t)
		s := park(t, database, "r-concluded", "cancelled", true)
		err := s.ResumeOpenRun(context.Background(), runmode.LocalDefaultOrgID, "r-concluded", "msg", runmode.LocalDefaultUserID)
		if !errors.Is(err, ErrConversationConcluded) {
			t.Errorf("err = %v, want ErrConversationConcluded (409)", err)
		}
		if st := storedStatus(t, database, "r-concluded"); st != "open" {
			t.Errorf("stored status = %q, want open — a refused wake must leave the row parked, not queued", st)
		}
	})

	t.Run("workspace intact, blueprint finished — the follow-up case", func(t *testing.T) {
		// The same shape as the refusal above, one word apart in the
		// blueprint's terminal, and the opposite answer. Finished work is
		// followed up on; called-off work is not.
		paths.SetForTest(t, t.TempDir())
		database := newDelegateTestDB(t)
		s := park(t, database, "r-followup", "completed", true)
		if err := s.ResumeOpenRun(context.Background(), runmode.LocalDefaultOrgID, "r-followup", "msg", runmode.LocalDefaultUserID); err != nil {
			t.Fatalf("ResumeOpenRun on a finished blueprint: %v", err)
		}
		if st := storedStatus(t, database, "r-followup"); st != "" {
			t.Errorf("stored status = %q, want none — the wake un-terminals the row", st)
		}
	})

	t.Run("both gone — the migrated legacy row", func(t *testing.T) {
		paths.SetForTest(t, t.TempDir())
		database := newDelegateTestDB(t)
		s := park(t, database, "r-legacy", "cancelled", false)
		err := s.ResumeOpenRun(context.Background(), runmode.LocalDefaultOrgID, "r-legacy", "msg", runmode.LocalDefaultUserID)
		if !errors.Is(err, ErrWorkspaceExpired) {
			t.Errorf("err = %v, want ErrWorkspaceExpired — the permanent answer wins over the temporary one", err)
		}
	})
}

// TestResumeOpenRun_EnqueuesRatherThanSpawning is resume-by-enqueue's core
// contract (TFAC-585, decision log #7): ResumeOpenRun records the message
// durably and flips the run's SAME row back to `queued` — no in-process
// resume goroutine, no s.cancels registration, at any point during the
// call. The message is only consumed later, by whichever executor claims
// the row.
func TestResumeOpenRun_EnqueuesRatherThanSpawning(t *testing.T) {
	database := newDelegateTestDB(t)
	seedRun(t, database, "r-wake", "sess-wake", "/tmp/does-not-exist-wake")
	if _, err := database.Exec(`UPDATE conversations SET status='open' WHERE id='r-wake'`); err != nil {
		t.Fatalf("open: %v", err)
	}
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6")

	if err := s.ResumeOpenRun(context.Background(), runmode.LocalDefaultOrgID, "r-wake", "the answer", runmode.LocalDefaultUserID); err != nil {
		t.Fatalf("ResumeOpenRun: %v", err)
	}

	if st := storedStatus(t, database, "r-wake"); st != "" {
		t.Errorf("stored status = %q, want none — resume-by-enqueue clears the park rather than writing a queue status", st)
	}

	msg, userID, ok, err := s.pendingInput.Consume(context.Background(), runmode.LocalDefaultOrgID, "r-wake")
	if err != nil {
		t.Fatalf("consume pending input: %v", err)
	}
	if !ok || msg != "the answer" || userID != runmode.LocalDefaultUserID {
		t.Errorf("pending input = (ok=%v msg=%q user=%q), want (true, %q, %q)", ok, msg, userID, "the answer", runmode.LocalDefaultUserID)
	}

	// No in-process resume goroutine — decision log #7's "no in-process
	// resume variant survives, in any mode".
	s.mu.Lock()
	_, active := s.cancels["r-wake"]
	s.mu.Unlock()
	if active {
		t.Error("ResumeOpenRun registered a cancel handle — an in-process resume goroutine must not spawn")
	}
}

// TestResumeOpenRun_CompletedAbortReopensBlueprintAtomically: a
// completed+abort run's blueprint (already terminal 'aborted') is reopened
// to 'running' in the SAME tx as the requeue flip — required for
// ClaimNextRun to ever claim the row (it only claims under a 'running'
// blueprint_run).
func TestResumeOpenRun_CompletedAbortReopensBlueprintAtomically(t *testing.T) {
	database := newDelegateTestDB(t)
	seedRun(t, database, "r-ab", "sess-ab", "/tmp/wt-ab")
	if _, err := database.Exec(`UPDATE conversations SET status='completed', outcome='abort' WHERE id='r-ab'`); err != nil {
		t.Fatalf("completed+abort: %v", err)
	}
	bpr := blueprintRunIDForRun(t, database, "r-ab")
	if _, err := database.Exec(`UPDATE blueprint_runs SET status='aborted' WHERE id=?`, bpr); err != nil {
		t.Fatalf("abort blueprint_run: %v", err)
	}
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "m")

	if err := s.ResumeOpenRun(context.Background(), runmode.LocalDefaultOrgID, "r-ab", "pick it back up", runmode.LocalDefaultUserID); err != nil {
		t.Fatalf("ResumeOpenRun: %v", err)
	}

	var bpStatus string
	if err := database.QueryRow(`SELECT status FROM blueprint_runs WHERE id=?`, bpr).Scan(&bpStatus); err != nil {
		t.Fatalf("read blueprint_run status: %v", err)
	}
	if bpStatus != "running" {
		t.Errorf("blueprint_run status = %q, want running (reopened so ClaimNextRun can claim the resumed row)", bpStatus)
	}
}

// claimAndDispatch claims the globally-oldest queued run (mirroring the
// dispatcher's own ClaimNextRun call) and drives it synchronously through
// dispatchClaimedRun — the same entry a real drainRunQueue goroutine would
// call, minus the goroutine wrapper, so resume-claim tests don't need to
// poll for completion.
func claimAndDispatch(t *testing.T, s *Spawner, database *sql.DB) {
	t.Helper()
	ctx := context.Background()
	run, err := s.runQueue.ClaimNextRun(ctx, "test-executor", 1, db.ClaimPlacement{})
	if err != nil {
		t.Fatalf("claim next run: %v", err)
	}
	if run == nil {
		t.Fatal("claim next run: nothing claimable (is the row queued under a running blueprint_run?)")
	}
	run.OrgID = runmode.LocalDefaultOrgID
	s.dispatchClaimedRun(ctx, run)
}

// TestDispatchResumeClaim_DeliversRecordedInput proves the delivery half of
// resume-by-enqueue end to end: after ResumeOpenRun enqueues, claiming and
// dispatching the row consumes the pending input exactly once (a second
// consume finds nothing) and drives the resume — failing fast here for
// lack of a warm worktree/snapshot (no subprocess), landing the run
// terminal. What matters is that delivery was attempted off the ordinary
// claim path, not an in-process goroutine.
func TestDispatchResumeClaim_DeliversRecordedInput(t *testing.T) {
	paths.SetForTest(t, t.TempDir())
	database := newDelegateTestDB(t)
	seedRun(t, database, "r-deliver", "sess-deliver", "/tmp/does-not-exist-deliver")
	if _, err := database.Exec(`UPDATE conversations SET status='open' WHERE id='r-deliver'`); err != nil {
		t.Fatalf("open: %v", err)
	}
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6")

	if err := s.ResumeOpenRun(context.Background(), runmode.LocalDefaultOrgID, "r-deliver", "the answer", runmode.LocalDefaultUserID); err != nil {
		t.Fatalf("ResumeOpenRun: %v", err)
	}

	claimAndDispatch(t, s, database)

	// The pending input was consumed exactly once.
	if _, _, ok, err := s.pendingInput.Consume(context.Background(), runmode.LocalDefaultOrgID, "r-deliver"); err != nil || ok {
		t.Errorf("pending input still present after dispatch: ok=%v err=%v", ok, err)
	}

	// The run reached an outcome — delivery was attempted (and failed fast
	// here for lack of a warm worktree/snapshot, landing terminal).
	st := storedStatus(t, database, "r-deliver")
	if st == "" || st == "open" {
		t.Errorf("stored status = %q, want a terminal status (delivery was not attempted)", st)
	}

	// No cancel handle survives dispatch (deferred cleanup ran).
	s.mu.Lock()
	_, active := s.cancels["r-deliver"]
	s.mu.Unlock()
	if active {
		t.Error("cancel handle leaked past dispatchResumeClaim's return")
	}
}

// TestDispatchResumeClaim_WorkspaceFailureFinalizesBlueprint is the
// resume-by-enqueue counterpart of the retired in-process goroutine's
// early-exit regression test: a resume claim that fails at ensureWorkspace
// must finalize the blueprint and discard its snapshot — not strand the
// blueprint_run `running` with a blob the reaper will never see. A snapshot
// is seeded (garbage content) so the enqueue-time recoverability pre-flight
// passes but the actual rehydrate fails at claim — the realistic
// enqueue-then-workspace-lost race, not an already-expired workspace (which
// ResumeOpenRun now refuses up front — see the sibling test).
func TestDispatchResumeClaim_WorkspaceFailureFinalizesBlueprint(t *testing.T) {
	paths.SetForTest(t, t.TempDir())
	s, database, run, _ := setupAdvanceFixture(t, "open-strand")
	bpr := blueprintRunIDForRun(t, database, run)
	wireBlobStore(t, s)
	putTestSnapshot(t, s, bpr) // garbage blob: passes Exists, fails rehydrate
	if _, err := database.Exec(`UPDATE conversations SET status='open', worktree_path='/tmp/does-not-exist-open-strand' WHERE id=?`, run); err != nil {
		t.Fatalf("park open: %v", err)
	}

	if err := s.ResumeOpenRun(context.Background(), runmode.LocalDefaultOrgID, run, "carry on", runmode.LocalDefaultUserID); err != nil {
		t.Fatalf("ResumeOpenRun: %v", err)
	}
	claimAndDispatch(t, s, database)

	var bpStatus string
	if err := database.QueryRow(`SELECT status FROM blueprint_runs WHERE id=?`, bpr).Scan(&bpStatus); err != nil {
		t.Fatalf("read blueprint_run status: %v", err)
	}
	if bpStatus == "running" {
		t.Errorf("blueprint_run stranded 'running' after a failed resume claim; want finalized")
	}
	assertSnapshotPresent(t, s, bpr, false) // discarded, not orphaned
}

// TestResumeOpenRun_ExpiredWorkspaceRefusedAtEnqueue pins the workspace-expiry
// contract: a wake whose workspace is gone for good (no warm worktree, no
// durable snapshot) is refused with ErrWorkspaceExpired at enqueue, with NO
// side effect — the run stays resumable, no pending-input row is recorded, and
// the blueprint is left running. Without the pre-flight the flip succeeds and
// the run is destroyed at claim time (a generic failRun) instead of the caller
// seeing a clean 410.
func TestResumeOpenRun_ExpiredWorkspaceRefusedAtEnqueue(t *testing.T) {
	paths.SetForTest(t, t.TempDir())
	s, database, run, _ := setupAdvanceFixture(t, "expired")
	bpr := blueprintRunIDForRun(t, database, run)
	wireBlobStore(t, s) // storage present but empty: no snapshot to recover from
	if _, err := database.Exec(`UPDATE conversations SET status='open', worktree_path='/tmp/does-not-exist-expired' WHERE id=?`, run); err != nil {
		t.Fatalf("park open: %v", err)
	}

	err := s.ResumeOpenRun(context.Background(), runmode.LocalDefaultOrgID, run, "carry on", runmode.LocalDefaultUserID)
	if !errors.Is(err, ErrWorkspaceExpired) {
		t.Fatalf("ResumeOpenRun err = %v, want ErrWorkspaceExpired", err)
	}

	var status string
	if err := database.QueryRow(`SELECT status FROM conversations WHERE id=?`, run).Scan(&status); err != nil {
		t.Fatalf("read run status: %v", err)
	}
	if status != "open" {
		t.Errorf("run status = %q after refused wake; want unchanged 'open'", status)
	}
	var pending int
	if err := database.QueryRow(`SELECT count(*) FROM messages WHERE conversation_id=? AND delivered=0`, run).Scan(&pending); err != nil {
		t.Fatalf("count pending input: %v", err)
	}
	if pending != 0 {
		t.Errorf("undelivered message rows = %d after refused wake; want 0 (no side effect)", pending)
	}
	var bpStatus string
	if err := database.QueryRow(`SELECT status FROM blueprint_runs WHERE id=?`, bpr).Scan(&bpStatus); err != nil {
		t.Fatalf("read blueprint_run status: %v", err)
	}
	if bpStatus != "running" {
		t.Errorf("blueprint_run status = %q after refused wake; want untouched 'running'", bpStatus)
	}
}
