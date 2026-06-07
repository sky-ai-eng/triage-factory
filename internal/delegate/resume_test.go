package delegate

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestResumeAfterYield_EmptyUserID guards the first gate: a resume must
// carry the responding user's identity (its writes route under that user's
// synthetic claims). No DB touched.
func TestResumeAfterYield_EmptyUserID(t *testing.T) {
	s := NewSpawner(nil, db.Stores{}, nil, nil, "", "")
	if err := s.ResumeAfterYield(runmode.LocalDefaultOrg, "any", "msg", ""); err == nil {
		t.Fatal("expected an error for an empty user id")
	}
}

// TestResumeAfterYield_ValidationGuards walks the field guards that reject a
// resume before any goroutine / subprocess work — each returns a plain error
// the respond endpoint surfaces. seedRun gives a complete row; each case
// blanks the one field under test.
func TestResumeAfterYield_ValidationGuards(t *testing.T) {
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
			database := newTakeoverTestDB(t)
			seedRun(t, database, "r-guard", "sess", "/tmp/wt")
			if _, err := database.Exec(`UPDATE runs SET status='awaiting_input' WHERE id='r-guard'`); err != nil {
				t.Fatalf("park: %v", err)
			}
			if _, err := database.Exec(tc.mutate, "r-guard"); err != nil {
				t.Fatalf("mutate: %v", err)
			}
			s := NewSpawner(database, testSpawnerStores(database), nil, nil, "m", "")

			err := s.ResumeAfterYield(runmode.LocalDefaultOrg, "r-guard", "msg", runmode.LocalDefaultUserID)
			if err == nil || !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("err = %v, want one mentioning %q", err, tc.wantSub)
			}
		})
	}
}

// TestResumeAfterYield_NotAwaitingInput pins the race guard: a run that
// reached a terminal state (e.g. a cancel won between the user's read and the
// resume) can't be woken — MarkResuming flips only from awaiting_input, so
// the resume returns ErrYieldNotResumable for the respond endpoint to map to
// 409.
func TestResumeAfterYield_NotAwaitingInput(t *testing.T) {
	database := newTakeoverTestDB(t)
	seedRun(t, database, "r-term", "sess", "/tmp/wt")
	if _, err := database.Exec(`UPDATE runs SET status='completed' WHERE id='r-term'`); err != nil {
		t.Fatalf("complete: %v", err)
	}
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "m", "")

	err := s.ResumeAfterYield(runmode.LocalDefaultOrg, "r-term", "msg", runmode.LocalDefaultUserID)
	if !errors.Is(err, ErrYieldNotResumable) {
		t.Errorf("err = %v, want ErrYieldNotResumable", err)
	}
}

// TestResumeAfterYield_InitiatesResume proves the resume entry wakes a parked
// run: it flips awaiting_input -> running and drives the run via the resume
// goroutine. Here the warm worktree is absent and no snapshot is wired, so
// the goroutine fails fast at ensureWorkspace (no subprocess spawned) and the
// run lands terminal — what matters is that it LEFT awaiting_input, i.e. the
// resume was initiated. The full "wakes as a new LiveRun resuming the
// session" path is covered by the live SDK smokes.
func TestResumeAfterYield_InitiatesResume(t *testing.T) {
	database := newTakeoverTestDB(t)
	seedRun(t, database, "r-wake", "sess-wake", "/tmp/does-not-exist-wake")
	if _, err := database.Exec(`UPDATE runs SET status='awaiting_input' WHERE id='r-wake'`); err != nil {
		t.Fatalf("park: %v", err)
	}
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6", "")

	if err := s.ResumeAfterYield(runmode.LocalDefaultOrg, "r-wake", "the answer", runmode.LocalDefaultUserID); err != nil {
		t.Fatalf("ResumeAfterYield: %v", err)
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

	// The run left awaiting_input — the resume was initiated and drove the run
	// to a terminal state (failed here, for lack of a warm worktree/snapshot).
	var status string
	if err := database.QueryRow(`SELECT status FROM runs WHERE id='r-wake'`).Scan(&status); err != nil {
		t.Fatalf("read status: %v", err)
	}
	if status == "awaiting_input" {
		t.Error("run stayed parked — resume was not initiated")
	}
}
