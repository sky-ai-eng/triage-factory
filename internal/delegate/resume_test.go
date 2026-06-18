package delegate

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/paths"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestResumeOpenRun_EmptyUserID guards the first gate: a resume must carry the
// resuming user's identity (its writes route under that user's synthetic
// claims). No DB touched.
func TestResumeOpenRun_EmptyUserID(t *testing.T) {
	s := NewSpawner(nil, db.Stores{}, nil, nil, "")
	if err := s.ResumeOpenRun(runmode.LocalDefaultOrgID, "any", "msg", ""); err == nil {
		t.Fatal("expected an error for an empty user id")
	}
}

// TestResumeOpenRun_ValidationGuards walks the field guards that reject a
// resume before any goroutine / subprocess work — each returns a plain error
// the caller surfaces. seedRun gives a complete row; each case blanks the one
// field under test.
func TestResumeOpenRun_ValidationGuards(t *testing.T) {
	cases := []struct {
		name    string
		mutate  string // SQL to blank the field under test
		wantSub string
	}{
		{"no session", `UPDATE runs SET session_id = NULL WHERE id = ?`, "session id"},
		{"no worktree", `UPDATE runs SET worktree_path = NULL WHERE id = ?`, "worktree path"},
		{"no model", `UPDATE runs SET model = NULL WHERE id = ?`, "model"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			database := newDelegateTestDB(t)
			seedRun(t, database, "r-guard", "sess", "/tmp/wt")
			if _, err := database.Exec(`UPDATE runs SET status='open' WHERE id='r-guard'`); err != nil {
				t.Fatalf("open: %v", err)
			}
			if _, err := database.Exec(tc.mutate, "r-guard"); err != nil {
				t.Fatalf("mutate: %v", err)
			}
			s := NewSpawner(database, testSpawnerStores(database), nil, nil, "m")

			err := s.ResumeOpenRun(runmode.LocalDefaultOrgID, "r-guard", "msg", runmode.LocalDefaultUserID)
			if err == nil || !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("err = %v, want one mentioning %q", err, tc.wantSub)
			}
		})
	}
}

// TestResumeOpenRun_NotResumable pins the resumable-state guard: a finish
// terminal (completed without an abort outcome) can't be woken — ResumeOpenRun
// returns ErrRunNotResumable for the caller to map to 409, before any workspace
// pre-flight runs.
func TestResumeOpenRun_NotResumable(t *testing.T) {
	database := newDelegateTestDB(t)
	seedRun(t, database, "r-term", "sess", "/tmp/wt")
	if _, err := database.Exec(`UPDATE runs SET status='completed', outcome='finish' WHERE id='r-term'`); err != nil {
		t.Fatalf("complete: %v", err)
	}
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "m")

	err := s.ResumeOpenRun(runmode.LocalDefaultOrgID, "r-term", "msg", runmode.LocalDefaultUserID)
	if !errors.Is(err, ErrRunNotResumable) {
		t.Errorf("err = %v, want ErrRunNotResumable", err)
	}
}

// TestResumeOpenRun_ExpiredWorkspace pins the retention-expiry semantics: a
// resumable run whose warm worktree is gone AND whose snapshot was reaped can't
// rebuild a workspace, so the resume returns ErrWorkspaceExpired and — crucially
// — leaves the run's status UNCHANGED (no flip), so the user sees a clear
// "expired" error rather than the run silently failing.
func TestResumeOpenRun_ExpiredWorkspace(t *testing.T) {
	paths.SetForTest(t, t.TempDir())
	database := newDelegateTestDB(t)
	seedRun(t, database, "r-exp", "sess-exp", "/tmp/does-not-exist-exp")
	if _, err := database.Exec(`UPDATE runs SET status='open' WHERE id='r-exp'`); err != nil {
		t.Fatalf("open: %v", err)
	}
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6")
	wireBlobStore(t, s) // store wired, but no snapshot for this run → expired

	err := s.ResumeOpenRun(runmode.LocalDefaultOrgID, "r-exp", "go on", runmode.LocalDefaultUserID)
	if !errors.Is(err, ErrWorkspaceExpired) {
		t.Fatalf("err = %v, want ErrWorkspaceExpired", err)
	}
	var status string
	if err := database.QueryRow(`SELECT status FROM runs WHERE id='r-exp'`).Scan(&status); err != nil {
		t.Fatalf("read status: %v", err)
	}
	if status != "open" {
		t.Errorf("status = %q, want open unchanged (status must not change at expiry)", status)
	}
}

// TestResumeOpenRun_InitiatesResume proves the resume entry wakes an open run:
// it flips open -> running and drives the run via the resume goroutine. The warm
// worktree is absent but a durable snapshot is present (so the workspace
// pre-flight passes); the goroutine then fails fast at the cold rehydrate of
// this stub blob (no subprocess spawned) and the run lands terminal — what
// matters is that it LEFT `open`, i.e. the resume was initiated. The full
// "wakes as a new LiveRun resuming the session" path is covered by the live SDK
// smokes.
func TestResumeOpenRun_InitiatesResume(t *testing.T) {
	paths.SetForTest(t, t.TempDir())
	database := newDelegateTestDB(t)
	seedRun(t, database, "r-wake", "sess-wake", "/tmp/does-not-exist-wake")
	if _, err := database.Exec(`UPDATE runs SET status='open' WHERE id='r-wake'`); err != nil {
		t.Fatalf("open: %v", err)
	}
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6")
	wireBlobStore(t, s)
	putTestSnapshot(t, s, blueprintRunIDForRun(t, database, "r-wake"))

	if err := s.ResumeOpenRun(runmode.LocalDefaultOrgID, "r-wake", "the answer", runmode.LocalDefaultUserID); err != nil {
		t.Fatalf("ResumeOpenRun: %v", err)
	}

	// Join the resume goroutine deterministically: it registers s.cancels on
	// entry and deletes it in its terminal defer (after all DB writes), so
	// waiting for that entry to clear avoids racing the in-memory DB close.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		_, active := s.cancels["r-wake"]
		s.mu.Unlock()
		if !active {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	s.mu.Lock()
	_, stillActive := s.cancels["r-wake"]
	s.mu.Unlock()
	if stillActive {
		t.Fatal("resume goroutine did not finish in time")
	}

	// The run left `open` — the resume was initiated and drove the run to a
	// terminal state (failed here, for lack of a warm worktree/snapshot).
	var status string
	if err := database.QueryRow(`SELECT status FROM runs WHERE id='r-wake'`).Scan(&status); err != nil {
		t.Fatalf("read status: %v", err)
	}
	if status == "open" {
		t.Error("run stayed open — resume was not initiated")
	}
}

// TestResumeOpenRun_EarlyExitFinalizesBlueprint is the regression for the
// pending_approval strand: a resume that exits early (here a cold-rehydrate
// failure; a mid-resume cancel shares the same non-disposed defer) must finalize
// the blueprint and discard its snapshot — not strand the blueprint_run
// `running` with a blob the reaper will never see (the run is no longer in a
// resumable state). pending_approval is the case that regressed: it doesn't
// re-open a blueprint, so the old abort-only defer never fired for it.
func TestResumeOpenRun_EarlyExitFinalizesBlueprint(t *testing.T) {
	paths.SetForTest(t, t.TempDir())
	s, database, run, _ := setupAdvanceFixture(t, "pa-strand")
	bpr := blueprintRunIDForRun(t, database, run)
	wireBlobStore(t, s)
	// A snapshot passes the workspace pre-flight; the stub blob then fails the
	// cold rehydrate (no subprocess), driving the early failure exit.
	putTestSnapshot(t, s, bpr)
	if _, err := database.Exec(`UPDATE runs SET status='pending_approval', worktree_path='/tmp/does-not-exist-pa-strand' WHERE id=?`, run); err != nil {
		t.Fatalf("park pending_approval: %v", err)
	}

	if err := s.ResumeOpenRun(runmode.LocalDefaultOrgID, run, "carry on", runmode.LocalDefaultUserID); err != nil {
		t.Fatalf("ResumeOpenRun: %v", err)
	}
	awaitResumeGoroutine(t, s, run)

	var bpStatus string
	if err := database.QueryRow(`SELECT status FROM blueprint_runs WHERE id=?`, bpr).Scan(&bpStatus); err != nil {
		t.Fatalf("read blueprint_run status: %v", err)
	}
	if bpStatus == "running" {
		t.Errorf("blueprint_run stranded 'running' after a failed pending_approval resume; want finalized")
	}
	assertSnapshotPresent(t, s, bpr, false) // discarded, not orphaned
}
