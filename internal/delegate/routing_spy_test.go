package delegate

// Tests that pin the trigger-type-driven pool routing introduced to
// close the PR #193 review-bot P2 findings (resolvePrompt visibility,
// Cancel preflight, Delegate.agentRuns.Create wrap). The
// behavior changes only become load-bearing under Postgres + RLS;
// these unit tests use recording wrappers around real SQLite stores
// to lock the routing contract so the manual-vs-event branch can't
// silently regress to "everything goes through GetSystem."
//
// The Delegate.agentRuns.Create wrap isn't tested in isolation here
// because Delegate spawns an async goroutine — testing the full
// synchronous path would require fixturing a GH client or
// restructuring the spawner. The wrap follows the same pattern as
// the IncrementUsage routing immediately above it in delegate.go;
// SQLite passes through identically for both, and the Postgres RLS
// matrix lands in D9-core's pgtest coverage.

import (
	"context"
	"database/sql"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	_ "modernc.org/sqlite"
)

// synthCall records one SyntheticClaimsWithTx invocation. The userID
// is what proves the spawner forwarded the real creator (vs falling
// back to a sentinel or skipping the wrap entirely).
type synthCall struct {
	orgID, userID string
}

// recordingTxRunner wraps a real TxRunner and counts every
// SyntheticClaimsWithTx call. WithTx is left as a pass-through
// (embedded) because none of the fixes route through it.
type recordingTxRunner struct {
	db.TxRunner
	synthCalls []synthCall
}

func (r *recordingTxRunner) SyntheticClaimsWithTx(ctx context.Context, orgID, userID string, fn func(db.TxStores) error) error {
	r.synthCalls = append(r.synthCalls, synthCall{orgID: orgID, userID: userID})
	return r.TxRunner.SyntheticClaimsWithTx(ctx, orgID, userID, fn)
}

// recordingPromptStore embeds the real store and overrides Get +
// GetSystem to count which one was reached. The non-spied methods
// pass through via the embedded interface.
type recordingPromptStore struct {
	db.PromptStore
	getCalls       int
	getSystemCalls int
}

func (r *recordingPromptStore) Get(ctx context.Context, orgID, id string) (*domain.Prompt, error) {
	r.getCalls++
	return r.PromptStore.Get(ctx, orgID, id)
}

func (r *recordingPromptStore) GetSystem(ctx context.Context, orgID, id string) (*domain.Prompt, error) {
	r.getSystemCalls++
	return r.PromptStore.GetSystem(ctx, orgID, id)
}

// newRoutingTestSpawner spins up a Spawner whose tx + prompts stores
// are wrapped in recorders. The other stores are real SQLite-backed
// so resolvePrompt / Cancel have the rows they need.
func newRoutingTestSpawner(t *testing.T) (*Spawner, *recordingTxRunner, *recordingPromptStore, *sql.DB) {
	t.Helper()
	database := newDelegateTestDB(t)
	stores := sqlitestore.New(database)
	tx := &recordingTxRunner{TxRunner: stores.Tx}
	prompts := &recordingPromptStore{PromptStore: stores.Prompts}
	stores.Tx = tx
	stores.Prompts = prompts
	s := NewSpawner(database, stores, nil, nil, "claude-sonnet-4-6")
	return s, tx, prompts, database
}

// TestResolvePrompt_ManualBranchesThroughSyntheticClaims pins the
// visibility fix for fix #4. A manual delegation must load the
// prompt via the app pool under the requesting user's synthetic
// claims so prompts_select RLS filters to prompts the user can see.
// Without this branch, a caller could supply a guessed prompt_id
// for another user's private prompt and the agent would run under it.
func TestResolvePrompt_ManualBranchesThroughSyntheticClaims(t *testing.T) {
	s, tx, prompts, database := newRoutingTestSpawner(t)

	ensureTestPrompt(t, database, domain.Prompt{ID: "p-manual", Name: "Manual prompt", Body: "x", Source: "user"})

	got, err := s.resolvePrompt(runmode.LocalDefaultOrgID, domain.Task{ID: "t"}, "p-manual", "manual", "00000000-0000-0000-0000-000000000aaa")
	if err != nil {
		t.Fatalf("resolvePrompt: %v", err)
	}
	if got == nil || got.ID != "p-manual" {
		t.Fatalf("resolvePrompt returned %+v; want p-manual", got)
	}

	if len(tx.synthCalls) != 1 {
		t.Fatalf("SyntheticClaimsWithTx called %d times; want exactly 1 (manual must route through synth claims)", len(tx.synthCalls))
	}
	if tx.synthCalls[0].userID != "00000000-0000-0000-0000-000000000aaa" {
		t.Errorf("synth call userID = %q; want the caller's creatorUserID", tx.synthCalls[0].userID)
	}
	// The actual Get inside SyntheticClaimsWithTx happens on the
	// TxStores' tx-local PromptStore (constructed inside runTx, not
	// the one we injected), so we can't assert getCalls here. The
	// observable fact that GetSystem was NOT called on the spied
	// store is the proof that the manual path didn't fall back to
	// the admin pool.
	if prompts.getSystemCalls != 0 {
		t.Errorf("PromptStore.GetSystem called %d times; want 0 (manual must NOT bypass the RLS-active app pool)", prompts.getSystemCalls)
	}
}

// TestResolvePrompt_EventStaysOnAdminPool pins the other half of
// fix #4: the router is a system actor with no user identity, so
// event-triggered runs must keep loading via the admin pool. If
// they were forced through SyntheticClaimsWithTx with an empty
// userID, the FK check in multi-mode would fail.
func TestResolvePrompt_EventStaysOnAdminPool(t *testing.T) {
	s, tx, prompts, database := newRoutingTestSpawner(t)

	ensureTestPrompt(t, database, domain.Prompt{ID: "p-event", Name: "Event prompt", Body: "x", Source: "system"})

	// Event-triggered runs carry creatorUserID="" — the router has
	// no user. The routing must NOT depend on creatorUserID being set.
	got, err := s.resolvePrompt(runmode.LocalDefaultOrgID, domain.Task{ID: "t"}, "p-event", "event", "")
	if err != nil {
		t.Fatalf("resolvePrompt: %v", err)
	}
	if got == nil || got.ID != "p-event" {
		t.Fatalf("resolvePrompt returned %+v; want p-event", got)
	}

	if len(tx.synthCalls) != 0 {
		t.Errorf("SyntheticClaimsWithTx called %d times; want 0 (event must stay on admin pool)", len(tx.synthCalls))
	}
	if prompts.getSystemCalls != 1 {
		t.Errorf("PromptStore.GetSystem called %d times; want 1 (event must reach admin pool)", prompts.getSystemCalls)
	}
}

// TestStop_UserInitiated_PreflightUsesSyntheticClaims pins the
// active-goroutine gate for user-initiated cancels. The cancels map
// inside Spawner is keyed only by runID, so without an org-scoped
// preflight a cross-org caller who learns an active runID could fire
// the goroutine's cancel() and tear down a live run — the goroutine
// would then write the terminal row under its own captured cfg.orgID
// and the attacker would be invisible to the audit trail. This test
// proves the user-initiated path goes through SyntheticClaimsWithTx
// before any cancels-map access, with the caller's userID for RLS.
func TestStop_UserInitiated_PreflightUsesSyntheticClaims(t *testing.T) {
	const runID = "run-cancel-spy"
	const callerID = "00000000-0000-0000-0000-000000000ddd"

	database := newDelegateTestDB(t)
	seedRun(t, database, runID, "session-x", "/tmp/some-wt")

	stores := sqlitestore.New(database)
	tx := &recordingTxRunner{TxRunner: stores.Tx}
	stores.Tx = tx
	s := NewSpawner(database, stores, nil, nil, "")

	// No goroutine registered in s.cancels — Stop will fall through
	// to the DB path. The preflight runs first; the assertion is the
	// synth call.
	_ = s.Stop(runmode.LocalDefaultOrgID, runID, callerID)

	if len(tx.synthCalls) < 1 {
		t.Fatalf("SyntheticClaimsWithTx called %d times; want at least 1 (preflight must route through synth claims)", len(tx.synthCalls))
	}
	if tx.synthCalls[0].userID != callerID {
		t.Errorf("preflight synth call userID = %q; want the calling user %q (preflight must check the caller's RLS scope, not a sentinel)", tx.synthCalls[0].userID, callerID)
	}
}

// TestStop_SystemInitiated_PreflightSkipsSynthClaims pins the other
// side of the gate: router-driven cancels (DrainTask rollback,
// task-close cleanup) pass userID="" because they're system actors
// with no user identity to project. Those must still scope by orgID
// but go through the admin pool, not synth claims — otherwise the
// router's no-user-context path would FK-fail in multi-mode.
func TestStop_SystemInitiated_PreflightSkipsSynthClaims(t *testing.T) {
	const runID = "run-cancel-system-spy"

	database := newDelegateTestDB(t)
	seedRun(t, database, runID, "session-x", "/tmp/some-wt")

	stores := sqlitestore.New(database)
	tx := &recordingTxRunner{TxRunner: stores.Tx}
	stores.Tx = tx
	s := NewSpawner(database, stores, nil, nil, "")

	_ = s.Stop(runmode.LocalDefaultOrgID, runID, "")

	if len(tx.synthCalls) != 0 {
		t.Errorf("SyntheticClaimsWithTx called %d times; want 0 for a system-initiated stop (must use admin pool, not synth claims)", len(tx.synthCalls))
	}
}

// Compile-time confirmation that the embedded-interface trick gives
// us full PromptStore + TxRunner satisfaction. If db adds a new
// method without a default impl, these break at compile time and
// flag the spy as needing an update.
var (
	_ db.PromptStore = (*recordingPromptStore)(nil)
	_ db.TxRunner    = (*recordingTxRunner)(nil)
)

// avoid unused-import warning when runmode isn't referenced directly
// (the constants are used implicitly via seedRun helpers).
var _ = runmode.LocalDefaultOrgID
