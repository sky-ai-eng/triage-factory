package dbtest

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// PendingPRStoreFactory is what a per-backend test file hands to
// RunPendingPRStoreConformance. Returns:
//   - the wired PendingPRStore impl,
//   - the orgID to pass to every call,
//   - a PendingPRSeeder for fixtures the harness can't create through
//     the store itself (a runs row that pending_prs.run_id can FK to,
//     plus a setSubmittedAtNull escape hatch for testing the
//     ClearSubmitted retry path without going through the
//     real time-sensitive MarkSubmitted/Clear cycle).
type PendingPRStoreFactory func(t *testing.T) (
	store db.PendingPRStore,
	orgID string,
	seed PendingPRSeeder,
)

// PendingPRSeeder bags the raw-SQL helpers backend tests provide.
type PendingPRSeeder struct {
	// Run inserts a runs row and returns its id. pending_prs.run_id
	// is NOT NULL with a UNIQUE constraint + ON DELETE CASCADE FK to
	// runs(id); every conformance test that creates a pending PR
	// needs a real run row to point at.
	Run func(t *testing.T) string
}

// RunPendingPRStoreConformance covers the pending-PR contract every
// backend impl must hold:
//
//   - Create snapshots OriginalTitle / OriginalBody at insert time
//     (no separate Lock/Update needed first). Empty body collapses
//     to NULL → nil pointer on read.
//   - Create with a duplicate run_id violates the UNIQUE constraint.
//   - Get / ByRunID round-trip. Get returns (nil, nil) on miss; same
//     for ByRunID.
//   - UpdateTitleBody preserves originals via COALESCE; gates on
//     submitted_at IS NULL, returning ErrPendingPRSubmitted otherwise.
//   - UpdateTitleBody with a bogus id returns a typed "not found"
//     error distinct from ErrPendingPRSubmitted.
//   - Lock first call sets locked=true; second call returns
//     ErrPendingPRAlreadyQueued and does not mutate title/body.
//   - Lock with a bogus id is "not found", not "already queued".
//   - MarkSubmitted first call wins; second returns
//     ErrPendingPRSubmitInFlight; bogus id is "not found".
//   - ClearSubmitted releases the guard so MarkSubmitted wins again.
//   - Delete tears down the row; DeleteByRunID is idempotent (no
//     error on a run with no PR).
func RunPendingPRStoreConformance(t *testing.T, mk PendingPRStoreFactory) {
	t.Helper()
	ctx := context.Background()

	t.Run("Create_then_Get_round_trips", func(t *testing.T) {
		s, orgID, seed := mk(t)
		runID := seed.Run(t)
		id := uuid.New().String()
		if err := s.Create(ctx, orgID, domain.PendingPR{
			ID: id, RunID: runID,
			Owner: "sky-ai-eng", Repo: "triage-factory",
			HeadBranch: "feature/SKY-1", HeadSHA: "abc123", BaseBranch: "main",
			Title: "Agent draft title", Body: "Agent draft body",
			Draft: true,
		}); err != nil {
			t.Fatalf("Create: %v", err)
		}
		got, err := s.Get(ctx, orgID, id)
		if err != nil || got == nil {
			t.Fatalf("Get: got=%v err=%v", got, err)
		}
		if got.Title != "Agent draft title" || got.Body != "Agent draft body" {
			t.Errorf("round-trip mismatch: %+v", got)
		}
		if !got.Draft {
			t.Errorf("Draft should round-trip true, got false")
		}
		if got.Locked {
			t.Errorf("Locked should be false on fresh row, got true")
		}
		if got.SubmittedAt != nil {
			t.Errorf("SubmittedAt should be nil on fresh row, got %v", got.SubmittedAt)
		}
	})

	t.Run("Create_snapshots_originals_at_insert", func(t *testing.T) {
		// At-queue-time snapshot: even before Lock runs, the row
		// should already have OriginalTitle / OriginalBody captured.
		// Matters because human edits via PATCH can land before the
		// agent calls Lock, and the human-feedback diff needs a
		// stable baseline.
		s, orgID, seed := mk(t)
		runID := seed.Run(t)
		id := uuid.New().String()
		if err := s.Create(ctx, orgID, domain.PendingPR{
			ID: id, RunID: runID,
			Owner: "owner", Repo: "repo",
			HeadBranch: "feature/SKY-1", HeadSHA: "abc123", BaseBranch: "main",
			Title: "Agent draft title", Body: "Agent draft body",
		}); err != nil {
			t.Fatalf("Create: %v", err)
		}

		got, _ := s.Get(ctx, orgID, id)
		if got.OriginalTitle == nil || *got.OriginalTitle != "Agent draft title" {
			t.Errorf("OriginalTitle = %v, want pointer to %q", ptrOrNil(got.OriginalTitle), "Agent draft title")
		}
		if got.OriginalBody == nil || *got.OriginalBody != "Agent draft body" {
			t.Errorf("OriginalBody = %v, want pointer to %q", ptrOrNil(got.OriginalBody), "Agent draft body")
		}
	})

	t.Run("Create_empty_body_yields_nil_original_body", func(t *testing.T) {
		// nullIfEmpty in the SQLite impl + NULLIF in the Postgres
		// impl collapse "" → SQL NULL on insert. The original_body
		// column also gets NULL because Create snapshots the same
		// value into both. Distinguishes "no body captured" (nil)
		// from a future "captured snapshot of empty body" — same
		// pointer-vs-nil contract domain.PendingPR documents.
		s, orgID, seed := mk(t)
		runID := seed.Run(t)
		id := uuid.New().String()
		if err := s.Create(ctx, orgID, domain.PendingPR{
			ID: id, RunID: runID,
			Owner: "o", Repo: "r",
			HeadBranch: "h", HeadSHA: "s", BaseBranch: "main",
			Title: "T", Body: "",
		}); err != nil {
			t.Fatalf("Create: %v", err)
		}
		got, _ := s.Get(ctx, orgID, id)
		if got.OriginalBody != nil {
			t.Errorf("empty body should produce nil OriginalBody, got %v", ptrOrNil(got.OriginalBody))
		}
		if got.Body != "" {
			t.Errorf("Body should be empty string on read, got %q", got.Body)
		}
	})

	t.Run("Create_one_pending_pr_per_run_app_guard", func(t *testing.T) {
		// One pending PR per run is now an APP-LAYER guard, not a DB
		// constraint: the UNIQUE(run_id) was dropped at this baseline
		// (so a future multi-repo run can queue several PRs), but the store
		// still blocks a second PR per run for now and surfaces it as the
		// clean ErrPendingPRAlreadyQueued — not a raw SQL constraint error.
		s, orgID, seed := mk(t)
		runID := seed.Run(t)
		base := domain.PendingPR{
			RunID: runID, Owner: "o", Repo: "r",
			HeadBranch: "h", HeadSHA: "abc", BaseBranch: "main",
			Title: "T1",
		}
		first := base
		first.ID = uuid.New().String()
		if err := s.Create(ctx, orgID, first); err != nil {
			t.Fatalf("first Create: %v", err)
		}
		second := base
		second.ID = uuid.New().String() // distinct PR id, same run_id
		err := s.Create(ctx, orgID, second)
		if !errors.Is(err, db.ErrPendingPRAlreadyQueued) {
			t.Errorf("second Create for same run: got %v, want ErrPendingPRAlreadyQueued (app-layer one-per-run guard)", err)
		}
	})

	t.Run("Get_returns_nil_on_miss", func(t *testing.T) {
		s, orgID, _ := mk(t)
		got, err := s.Get(ctx, orgID, uuid.New().String())
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got != nil {
			t.Errorf("Get on missing id should be nil, got %+v", got)
		}
	})

	t.Run("ByRunID_returns_nil_on_miss", func(t *testing.T) {
		s, orgID, _ := mk(t)
		got, err := s.ByRunID(ctx, orgID, uuid.New().String())
		if err != nil {
			t.Fatalf("ByRunID: %v", err)
		}
		if got != nil {
			t.Errorf("ByRunID on missing run should be nil, got %+v", got)
		}
	})

	t.Run("ByRunID_finds_and_projects", func(t *testing.T) {
		s, orgID, seed := mk(t)
		runID := seed.Run(t)
		mustCreatePendingPR(ctx, t, s, orgID, runID, uuid.New().String())
		got, err := s.ByRunID(ctx, orgID, runID)
		if err != nil || got == nil {
			t.Fatalf("ByRunID: got=%v err=%v", got, err)
		}
		if got.RunID != runID {
			t.Errorf("RunID = %q, want %q", got.RunID, runID)
		}
	})

	t.Run("UpdateTitleBody_preserves_originals", func(t *testing.T) {
		// Human-edit path. The agent's snapshot is frozen; subsequent
		// user edits move the visible title/body forward but
		// originals stay put.
		s, orgID, seed := mk(t)
		runID := seed.Run(t)
		id := uuid.New().String()
		if err := s.Create(ctx, orgID, domain.PendingPR{
			ID: id, RunID: runID,
			Owner: "o", Repo: "r",
			HeadBranch: "h", HeadSHA: "abc", BaseBranch: "main",
			Title: "Agent draft", Body: "Agent body",
		}); err != nil {
			t.Fatalf("Create: %v", err)
		}
		if err := s.UpdateTitleBody(ctx, orgID, id, "Human edit", "Human body"); err != nil {
			t.Fatalf("UpdateTitleBody: %v", err)
		}
		got, _ := s.Get(ctx, orgID, id)
		if got.Title != "Human edit" || got.Body != "Human body" {
			t.Errorf("title/body = %q/%q, want Human edit/Human body", got.Title, got.Body)
		}
		if got.OriginalTitle == nil || *got.OriginalTitle != "Agent draft" {
			t.Errorf("OriginalTitle = %v, want frozen at %q", ptrOrNil(got.OriginalTitle), "Agent draft")
		}
		if got.OriginalBody == nil || *got.OriginalBody != "Agent body" {
			t.Errorf("OriginalBody = %v, want frozen at %q", ptrOrNil(got.OriginalBody), "Agent body")
		}
	})

	t.Run("UpdateTitleBody_after_submit_returns_submitted", func(t *testing.T) {
		// `submitted_at IS NULL` gate: once MarkSubmitted has
		// claimed the row, PATCH must surface ErrPendingPRSubmitted
		// so the handler can render the right 409.
		s, orgID, seed := mk(t)
		runID := seed.Run(t)
		id := uuid.New().String()
		mustCreatePendingPR(ctx, t, s, orgID, runID, id)
		if err := s.MarkSubmitted(ctx, orgID, id); err != nil {
			t.Fatalf("MarkSubmitted: %v", err)
		}
		err := s.UpdateTitleBody(ctx, orgID, id, "late edit", "late body")
		if !errors.Is(err, db.ErrPendingPRSubmitted) {
			t.Errorf("UpdateTitleBody after submit: err = %v, want ErrPendingPRSubmitted", err)
		}
	})

	t.Run("UpdateTitleBody_bogus_id_not_found", func(t *testing.T) {
		s, orgID, _ := mk(t)
		err := s.UpdateTitleBody(ctx, orgID, uuid.New().String(), "T", "B")
		if errors.Is(err, db.ErrPendingPRSubmitted) {
			t.Errorf("bogus id should not return ErrPendingPRSubmitted, got %v", err)
		}
		if err == nil || !strings.Contains(err.Error(), "not found") {
			t.Errorf("bogus id: err = %v, want 'not found'", err)
		}
	})

	t.Run("Lock_first_call_sets_locked", func(t *testing.T) {
		s, orgID, seed := mk(t)
		runID := seed.Run(t)
		id := uuid.New().String()
		mustCreatePendingPR(ctx, t, s, orgID, runID, id)
		if err := s.Lock(ctx, orgID, id, "T-locked", "B-locked"); err != nil {
			t.Fatalf("Lock: %v", err)
		}
		got, _ := s.Get(ctx, orgID, id)
		if !got.Locked {
			t.Errorf("Locked = false after Lock, want true")
		}
		if got.Title != "T-locked" || got.Body != "B-locked" {
			t.Errorf("title/body = %q/%q, want T-locked/B-locked", got.Title, got.Body)
		}
	})

	t.Run("Lock_second_call_returns_already_queued", func(t *testing.T) {
		// SKY-212 anti-retry: the agent retries `pr create` (didn't
		// see the response, ambiguous tool result). Second Lock must
		// return the typed sentinel so the CLI can render a clean
		// "already queued" message rather than a generic SQL error.
		// Crucially the second call must NOT mutate title/body.
		s, orgID, seed := mk(t)
		runID := seed.Run(t)
		id := uuid.New().String()
		mustCreatePendingPR(ctx, t, s, orgID, runID, id)
		if err := s.Lock(ctx, orgID, id, "T", "B"); err != nil {
			t.Fatalf("first Lock: %v", err)
		}
		err := s.Lock(ctx, orgID, id, "T-2nd", "B-2nd")
		if !errors.Is(err, db.ErrPendingPRAlreadyQueued) {
			t.Errorf("second Lock: err = %v, want ErrPendingPRAlreadyQueued", err)
		}
		got, _ := s.Get(ctx, orgID, id)
		if got.Title != "T" || got.Body != "B" {
			t.Errorf("rejected second Lock mutated title/body: %q/%q", got.Title, got.Body)
		}
	})

	t.Run("Lock_bogus_id_distinct_from_already_queued", func(t *testing.T) {
		// A non-existent id should NOT get the SKY-212 "already
		// queued" message — that would mislead the agent into
		// thinking it was a retry. Surface a "not found" error.
		s, orgID, _ := mk(t)
		err := s.Lock(ctx, orgID, uuid.New().String(), "T", "B")
		if errors.Is(err, db.ErrPendingPRAlreadyQueued) {
			t.Errorf("bogus id got ErrPendingPRAlreadyQueued; want a not-found error")
		}
		if err == nil || !strings.Contains(err.Error(), "not found") {
			t.Errorf("bogus id: err = %v, want 'not found'", err)
		}
	})

	t.Run("MarkSubmitted_first_call_wins", func(t *testing.T) {
		s, orgID, seed := mk(t)
		runID := seed.Run(t)
		id := uuid.New().String()
		mustCreatePendingPR(ctx, t, s, orgID, runID, id)

		if err := s.MarkSubmitted(ctx, orgID, id); err != nil {
			t.Fatalf("first MarkSubmitted: %v", err)
		}
		// The row should now have a non-nil SubmittedAt.
		got, _ := s.Get(ctx, orgID, id)
		if got.SubmittedAt == nil {
			t.Errorf("SubmittedAt should be set after MarkSubmitted")
		}
	})

	t.Run("MarkSubmitted_second_call_in_flight", func(t *testing.T) {
		// Concurrent-submit guard: two browser tabs click "Open PR"
		// simultaneously; only one should proceed to CreatePR.
		s, orgID, seed := mk(t)
		runID := seed.Run(t)
		id := uuid.New().String()
		mustCreatePendingPR(ctx, t, s, orgID, runID, id)
		if err := s.MarkSubmitted(ctx, orgID, id); err != nil {
			t.Fatalf("first MarkSubmitted: %v", err)
		}
		err := s.MarkSubmitted(ctx, orgID, id)
		if !errors.Is(err, db.ErrPendingPRSubmitInFlight) {
			t.Errorf("second MarkSubmitted: err = %v, want ErrPendingPRSubmitInFlight", err)
		}
	})

	t.Run("MarkSubmitted_bogus_id_distinct_from_in_flight", func(t *testing.T) {
		s, orgID, _ := mk(t)
		err := s.MarkSubmitted(ctx, orgID, uuid.New().String())
		if errors.Is(err, db.ErrPendingPRSubmitInFlight) {
			t.Errorf("bogus id should not return ErrPendingPRSubmitInFlight, got %v", err)
		}
		if err == nil || !strings.Contains(err.Error(), "not found") {
			t.Errorf("bogus id: err = %v, want 'not found'", err)
		}
	})

	t.Run("ClearSubmitted_allows_retry", func(t *testing.T) {
		// On submit failure the server clears the guard so the user
		// can retry. After clear, MarkSubmitted wins again.
		s, orgID, seed := mk(t)
		runID := seed.Run(t)
		id := uuid.New().String()
		mustCreatePendingPR(ctx, t, s, orgID, runID, id)
		if err := s.MarkSubmitted(ctx, orgID, id); err != nil {
			t.Fatalf("first MarkSubmitted: %v", err)
		}
		if err := s.ClearSubmitted(ctx, orgID, id); err != nil {
			t.Fatalf("ClearSubmitted: %v", err)
		}
		if err := s.MarkSubmitted(ctx, orgID, id); err != nil {
			t.Fatalf("retry MarkSubmitted (Clear should have released the guard): %v", err)
		}
	})

	t.Run("Delete_removes_row", func(t *testing.T) {
		s, orgID, seed := mk(t)
		runID := seed.Run(t)
		id := uuid.New().String()
		mustCreatePendingPR(ctx, t, s, orgID, runID, id)
		if err := s.Delete(ctx, orgID, id); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		got, _ := s.Get(ctx, orgID, id)
		if got != nil {
			t.Errorf("row survived Delete: %+v", got)
		}
	})

	t.Run("Delete_bogus_id_not_found", func(t *testing.T) {
		s, orgID, _ := mk(t)
		err := s.Delete(ctx, orgID, uuid.New().String())
		if err == nil || !strings.Contains(err.Error(), "not found") {
			t.Errorf("Delete bogus: err = %v, want 'not found'", err)
		}
	})

	t.Run("DeleteByRunID_no_op_when_none", func(t *testing.T) {
		// Idempotent — used by cleanupPendingApprovalRun, which
		// calls both ReviewStore.DeleteByRunID and
		// PendingPRStore.DeleteByRunID regardless of which side-table
		// actually has a row.
		s, orgID, seed := mk(t)
		runID := seed.Run(t)
		if err := s.DeleteByRunID(ctx, orgID, runID); err != nil {
			t.Errorf("DeleteByRunID on run with no PR should be no-op, got %v", err)
		}
	})

	t.Run("DeleteByRunID_removes_row", func(t *testing.T) {
		s, orgID, seed := mk(t)
		runID := seed.Run(t)
		id := uuid.New().String()
		mustCreatePendingPR(ctx, t, s, orgID, runID, id)
		if err := s.DeleteByRunID(ctx, orgID, runID); err != nil {
			t.Fatalf("DeleteByRunID: %v", err)
		}
		got, _ := s.ByRunID(ctx, orgID, runID)
		if got != nil {
			t.Errorf("row survived DeleteByRunID: %+v", got)
		}
	})
}

// mustCreatePendingPR is a thin wrapper around PendingPRStore.Create
// that fails the test on insert error. Most subtests only care about
// the row existing, not its full field surface, so this collapses
// the boilerplate.
func mustCreatePendingPR(ctx context.Context, t *testing.T, s db.PendingPRStore, orgID, runID, id string) {
	t.Helper()
	if err := s.Create(ctx, orgID, domain.PendingPR{
		ID: id, RunID: runID,
		Owner: "o", Repo: "r",
		HeadBranch: "h", HeadSHA: "s", BaseBranch: "main",
		Title: "T", Body: "B",
	}); err != nil {
		t.Fatalf("mustCreatePendingPR: %v", err)
	}
}
