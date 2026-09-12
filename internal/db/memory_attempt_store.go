package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// ErrNoSuchMemoryAttempt means a close-out named no OPEN attempt in this
// org — either the id matches nothing, or the attempt it names was already
// completed. Deliberately one error for both: the CAS exists so an attempt
// is closed out exactly once, and a second closer needs to stop either way
// rather than learn which of the two happened.
var ErrNoSuchMemoryAttempt = errors.New("db: no open memory attempt with that id in this org")

// MemoryAttemptStore owns the conversation_memory_attempts table — the ledger
// of tries at generating a memory for a conversation that ended owing one.
// Without it a missing memory row is two situations wearing one face: nothing
// was owed, or something was owed and generation failed. A reader who cannot
// tell those apart either retries forever or gives up silently.
//
// Admin-pool-only in Postgres, hence the `...System` suffix on every
// method: the writer is the brain's memory provisioner, a background
// goroutine with no JWT-claims context, and the org-scoped RLS policy gates
// the app-pool reads (tf_app holds SELECT alone). org_id is bound by
// argument on every call as defense in depth. SQLite is N=1 and unscoped.
//
// The provisioner is a sibling change and is not wired yet, so today the
// only callers are the conformance suites. The ledger lands ahead of its
// writer deliberately, so that work and the read over these rows can
// proceed in parallel.
type MemoryAttemptStore interface {
	// BeginAttemptSystem opens an attempt on conversationID: one INSERT with
	// started_at = now and every verdict column NULL. Returns the stored row,
	// off RETURNING on the insert itself — the id and started_at are the
	// store's, not the caller's, and the caller needs the id to close it out.
	BeginAttemptSystem(ctx context.Context, orgID, conversationID string) (domain.MemoryAttempt, error)

	// CompleteAttemptSystem closes out the attempt attemptID names, under a
	// `completed_at IS NULL` CAS so an attempt is closed exactly once: a miss
	// — no such id in this org, or one already completed — is
	// ErrNoSuchMemoryAttempt, not a silent overwrite of the first verdict.
	//
	// The vocabularies are validated at the door, and the pair is a
	// biconditional: outcome must name a real outcome (never the empty string
	// — that is what a running attempt carries), errKind must be set exactly
	// when the outcome is failed, and errMsg is refused on any other outcome.
	// The store is the only enforcement — the columns carry no CHECK — so a
	// bad value is an error rather than a row nobody can read back.
	//
	// systemLLMRunID is bound through a subselect against system_llm_runs, so
	// an id whose ledger row never landed (that insert is best-effort) stores
	// NULL instead of violating the FK. Empty means "no ledger row" and stores
	// NULL too.
	//
	// Returns the stored row, off RETURNING on the update itself.
	CompleteAttemptSystem(ctx context.Context, orgID, attemptID string,
		outcome domain.MemoryAttemptOutcome, errKind domain.MemoryAttemptErrorKind,
		errMsg, systemLLMRunID string, rowsTotal, rowsSent int) (domain.MemoryAttempt, error)

	// NewestAttemptForConversationSystem returns the conversation's most
	// recent attempt by started_at, or (nil, nil) when it has never been
	// attempted — an answer, not an error, and the common case for every
	// conversation that concluded through the gate with its own memory.
	NewestAttemptForConversationSystem(ctx context.Context, orgID, conversationID string) (*domain.MemoryAttempt, error)
}

// ValidateMemoryAttemptVerdict is the door CompleteAttemptSystem's arguments
// pass through in both dialects: the closed vocabularies plus the failed ⇔
// error_kind biconditional. Shared rather than restated per backend so the two
// impls cannot drift on what they accept.
func ValidateMemoryAttemptVerdict(outcome domain.MemoryAttemptOutcome, errKind domain.MemoryAttemptErrorKind, errMsg string) error {
	if !domain.IsMemoryAttemptOutcome(string(outcome)) {
		return fmt.Errorf("db: memory attempt outcome %q is not one of %v", outcome, domain.AllMemoryAttemptOutcomes())
	}
	if outcome == domain.MemoryAttemptFailed {
		if !domain.IsMemoryAttemptErrorKind(string(errKind)) {
			return fmt.Errorf("db: a failed memory attempt needs an error_kind from %v, got %q", domain.AllMemoryAttemptErrorKinds(), errKind)
		}
		return nil
	}
	if errKind != "" {
		return fmt.Errorf("db: memory attempt outcome %q must carry no error_kind, got %q", outcome, errKind)
	}
	if errMsg != "" {
		return fmt.Errorf("db: memory attempt outcome %q must carry no error_message", outcome)
	}
	return nil
}

// ScanMemoryAttempt reads one conversation_memory_attempts row off an
// already-prepared scanner. Shared rather than duplicated per dialect because
// nothing in it is dialect-specific: both backends COALESCE the four nullable
// text columns to the empty string in their own projection, so what reaches
// here is the same eleven values in the same order. The projections themselves
// stay beside their SQL — Postgres casts its uuid columns to text, SQLite has
// no cast to make.
//
// CompletedAt is the one pointer: nil is "still running, or a brain that died
// before closing out", which is a different thing from any timestamp.
func ScanMemoryAttempt(scan func(...any) error) (domain.MemoryAttempt, error) {
	var a domain.MemoryAttempt
	var completedAt sql.NullTime
	err := scan(&a.ID, &a.OrgID, &a.ConversationID, &a.StartedAt, &completedAt,
		&a.Outcome, &a.ErrorKind, &a.ErrorMessage,
		&a.SystemLLMRunID, &a.WindowRowsTotal, &a.WindowRowsSent)
	if err != nil {
		return domain.MemoryAttempt{}, err
	}
	if completedAt.Valid {
		t := completedAt.Time.UTC()
		a.CompletedAt = &t
	}
	return a, nil
}
