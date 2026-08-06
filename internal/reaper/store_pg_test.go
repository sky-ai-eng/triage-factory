package reaper_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/reaper"
)

// reaperFixture is one claimed run + its owning blueprint_run + the
// executor instance that (nominally) owns it, ready for a test to
// manipulate (backdate the heartbeat, bump attempts, request a cancel)
// before calling ReapDeadExecutors.
type reaperFixture struct {
	runID          string
	blueprintRunID string
	executorID     string
	orgID, userID  string
}

// seedReaperFixture mints an org, a blueprint/task/prompt chain, a running
// blueprint_run, and one claimed run under a freshly-registered executor
// instance. attempts is set via repeated claims (each ClaimNextRun mints a
// claims row — the real production mechanism, not a raw INSERT) so the
// fixture exercises the same claims count the reaper reads.
func seedReaperFixture(t *testing.T, h *pgtest.Harness, attempts int) reaperFixture {
	t.Helper()
	ctx := context.Background()
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)

	orgID, userID, _ := pgtest.SeedOrgWithUser(t, h, "reaper-"+uuid.New().String()[:8])

	teamID := reaperFirstTeamForOrg(t, h, orgID)
	blueprintID := "reaper-bp-" + uuid.New().String()[:8]
	pgtest.MustExec(t, h.AdminDB, `
		INSERT INTO blueprints (id, org_id, team_id, creator_user_id, name, source, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $1, 'user', now(), now())
	`, blueprintID, orgID, teamID, userID)

	promptID := "reaper-p-" + uuid.New().String()[:8]
	pgtest.MustExec(t, h.AdminDB, `
		INSERT INTO prompts (id, org_id, team_id, creator_user_id, name, body, source, allowed_tools, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $1, 'body', 'user', '[]'::jsonb, now(), now())
	`, promptID, orgID, teamID, userID)

	entityID := uuid.New().String()
	pgtest.MustExec(t, h.AdminDB, `
		INSERT INTO entities (id, org_id, source, source_id, kind, title, url, snapshot_json, created_at)
		VALUES ($1, $2, 'github', $3, 'pr', 'Reaper Test Entity', 'https://example/x', '{}'::jsonb, now())
	`, entityID, orgID, "reaper-test-"+entityID[:8])

	eventID := uuid.New().String()
	pgtest.MustExec(t, h.AdminDB, `
		INSERT INTO events (id, org_id, entity_id, event_type, dedup_key, metadata_json, created_at)
		VALUES ($1, $2, $3, $4, '', '{}'::jsonb, now())
	`, eventID, orgID, entityID, domain.EventGitHubPRCICheckFailed)

	taskID := uuid.New().String()
	pgtest.MustExec(t, h.AdminDB, `
		INSERT INTO tasks (id, org_id, creator_user_id, team_id, entity_id, event_type, dedup_key, primary_event_id, status, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, '', $7, 'queued', now())
	`, taskID, orgID, userID, teamID, entityID, domain.EventGitHubPRCICheckFailed, eventID)

	blueprintRunID := uuid.New().String()
	pgtest.MustExec(t, h.AdminDB, `
		INSERT INTO blueprint_runs (id, org_id, creator_user_id, blueprint_id, task_id, trigger_type, status, worktree_path, started_at, step_plan)
		VALUES ($1, $2, $3, $4, $5, 'manual', 'running', $6, now(), '[]')
	`, blueprintRunID, orgID, userID, blueprintID, taskID, "/tmp/wt-"+blueprintRunID)

	executorID := "reaper-executor-" + uuid.New().String()[:8]
	if _, err := stores.Instances.Register(ctx, executorID, domain.InstanceRoleExecutor, "v1", ""); err != nil {
		t.Fatalf("register executor: %v", err)
	}

	runID := uuid.New().String()
	step0 := 0
	if err := stores.RunQueue.EnqueueRun(ctx, orgID, domain.Conversation{
		ID: runID, TaskID: taskID, PromptID: promptID, Model: "m",
		TriggerType: "manual", CreatorUserID: userID, BlueprintRunID: blueprintRunID, BlueprintStepIndex: &step0,
	}); err != nil {
		t.Fatalf("EnqueueRun: %v", err)
	}
	for i := 0; i < attempts; i++ {
		// The claim is cross-org and takes the globally-oldest eligible
		// conversation, so a test seeding several fixtures must leave no
		// older one claimable — assert the identity rather than let a
		// mis-claim pass silently.
		got, err := stores.RunQueue.ClaimNextRun(ctx, executorID, 1, db.ClaimPlacement{})
		if err != nil || got == nil || got.ID != runID {
			t.Fatalf("ClaimNextRun (attempt %d): got=%v err=%v, want run %s", i+1, got, err, runID)
		}
		if i < attempts-1 {
			// A mid-loop requeue simulates the run bouncing back to 'queued'
			// so the NEXT claim can bump attempts again — attempts is a
			// monotonic per-row counter, not "how many times currently
			// claimed".
			if err := stores.RunQueue.RequeueRun(ctx, orgID, runID, "test churn"); err != nil {
				t.Fatalf("RequeueRun (attempt %d): %v", i+1, err)
			}
		}
	}

	return reaperFixture{runID: runID, blueprintRunID: blueprintRunID, executorID: executorID, orgID: orgID, userID: userID}
}

func reaperFirstTeamForOrg(t *testing.T, h *pgtest.Harness, orgID string) string {
	t.Helper()
	var teamID string
	if err := h.AdminDB.QueryRow(`SELECT id FROM teams WHERE org_id = $1 ORDER BY created_at ASC LIMIT 1`, orgID).Scan(&teamID); err != nil {
		t.Fatalf("resolve team for org %s: %v", orgID, err)
	}
	return teamID
}

// backdateHeartbeat pushes an instance's last_heartbeat_at far enough into
// the past that it reads as stale under any threshold this test file uses.
func backdateHeartbeat(t *testing.T, h *pgtest.Harness, executorID string, age time.Duration) {
	t.Helper()
	pgtest.MustExec(t, h.AdminDB, `UPDATE instances SET last_heartbeat_at = now() - $2::interval WHERE id = $1`,
		executorID, age.String())
}

// TestReapDeadExecutors_RequeuesUnderAttemptBudget pins the primary
// recovery path: a claimed run under a stale-heartbeat executor, still
// within its attempt budget, is requeued (active claim released with
// outcome 'reaped', no new claim minted) rather than failed.
func TestReapDeadExecutors_RequeuesUnderAttemptBudget(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	ctx := context.Background()

	fx := seedReaperFixture(t, h, 1)
	backdateHeartbeat(t, h, fx.executorID, time.Hour)

	store := reaper.NewPostgresStore(h.AdminDB)
	counts, err := store.ReapDeadExecutors(ctx, 30*time.Second, 2)
	if err != nil {
		t.Fatalf("ReapDeadExecutors: %v", err)
	}
	if counts.Requeued != 1 || counts.Failed != 0 || counts.Cancelled != 0 {
		t.Fatalf("counts = %+v, want {Requeued:1}", counts)
	}

	var status sql.NullString
	if err := h.AdminDB.QueryRowContext(ctx,
		`SELECT status FROM conversations WHERE id = $1`, fx.runID,
	).Scan(&status); err != nil {
		t.Fatalf("read back run: %v", err)
	}
	if status.Valid {
		t.Errorf("status = %q, want none — releasing the claim IS the requeue", status.String)
	}
	// The dead engagement is released — a requeued row has no owner — and
	// the release carries the claim-level 'reaped' outcome.
	var active int
	if err := h.AdminDB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM claims WHERE conversation_id = $1 AND released_at IS NULL`, fx.runID,
	).Scan(&active); err != nil {
		t.Fatalf("count active claims: %v", err)
	}
	if active != 0 {
		t.Errorf("active claims = %d, want 0 (ownership released on requeue)", active)
	}
	var outcome string
	var total int
	if err := h.AdminDB.QueryRowContext(ctx,
		`SELECT outcome, (SELECT COUNT(*) FROM claims WHERE conversation_id = $1)
		   FROM claims WHERE conversation_id = $1 ORDER BY claimed_at DESC LIMIT 1`, fx.runID,
	).Scan(&outcome, &total); err != nil {
		t.Fatalf("read released claim: %v", err)
	}
	if outcome != "reaped" {
		t.Errorf("released claim outcome = %q, want reaped", outcome)
	}
	if total != 1 {
		t.Errorf("claims count = %d, want 1 (the reaper's requeue must not mint a claim — only a re-claim does)", total)
	}
}

// TestReapDeadExecutors_TerminalFailsExecutorLostPastAttemptBudget pins the
// attempts-exhausted path: attempts >= maxAttempts terminal-fails the run
// with failure_kind='executor_lost' and finalizes the owning blueprint_run
// failed, instead of requeuing it forever.
func TestReapDeadExecutors_TerminalFailsExecutorLostPastAttemptBudget(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	ctx := context.Background()

	fx := seedReaperFixture(t, h, 2) // attempts == maxAttempts below
	backdateHeartbeat(t, h, fx.executorID, time.Hour)

	store := reaper.NewPostgresStore(h.AdminDB)
	counts, err := store.ReapDeadExecutors(ctx, 30*time.Second, 2)
	if err != nil {
		t.Fatalf("ReapDeadExecutors: %v", err)
	}
	if counts.Failed != 1 || counts.Requeued != 0 || counts.Cancelled != 0 {
		t.Fatalf("counts = %+v, want {Failed:1}", counts)
	}

	var status, failureKind string
	if err := h.AdminDB.QueryRowContext(ctx,
		`SELECT status, COALESCE(failure_kind, '') FROM conversations WHERE id = $1`, fx.runID,
	).Scan(&status, &failureKind); err != nil {
		t.Fatalf("read back run: %v", err)
	}
	if status != "failed" || failureKind != string(domain.RunFailureExecutorLost) {
		t.Errorf("run (status=%q failure_kind=%q), want (failed, executor_lost)", status, failureKind)
	}

	var brStatus string
	if err := h.AdminDB.QueryRowContext(ctx,
		`SELECT status FROM blueprint_runs WHERE id = $1`, fx.blueprintRunID,
	).Scan(&brStatus); err != nil {
		t.Fatalf("read back blueprint_run: %v", err)
	}
	if brStatus != "failed" {
		t.Errorf("blueprint_run status = %q, want failed", brStatus)
	}
}

// TestReapDeadExecutors_CancelRequestedFinalizesCancelledNotRequeued pins
// "cancel-requested rows go through the existing cancel finalization
// instead of requeue" — even a run still within its attempt budget must
// finalize, never come back as queued work.
//
// The split is the one every cancel path uses: the blueprint takes the
// 'cancelled' terminal, the conversation parks `open` with its workspace, and
// the claim gate refuses the parked row because its blueprint is terminal.
func TestReapDeadExecutors_CancelRequestedFinalizesCancelledNotRequeued(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	ctx := context.Background()

	fx := seedReaperFixture(t, h, 1)
	backdateHeartbeat(t, h, fx.executorID, time.Hour)
	pgtest.MustExec(t, h.AdminDB, `UPDATE blueprint_runs SET cancel_requested = true WHERE id = $1`, fx.blueprintRunID)

	store := reaper.NewPostgresStore(h.AdminDB)
	counts, err := store.ReapDeadExecutors(ctx, 30*time.Second, 2)
	if err != nil {
		t.Fatalf("ReapDeadExecutors: %v", err)
	}
	if counts.Cancelled != 1 || counts.Requeued != 0 || counts.Failed != 0 {
		t.Fatalf("counts = %+v, want {Cancelled:1}", counts)
	}

	var runStatus, brStatus string
	var parked bool
	if err := h.AdminDB.QueryRowContext(ctx, `SELECT status, parked_at IS NOT NULL FROM conversations WHERE id = $1`, fx.runID).Scan(&runStatus, &parked); err != nil {
		t.Fatalf("read back run: %v", err)
	}
	if err := h.AdminDB.QueryRowContext(ctx, `SELECT status FROM blueprint_runs WHERE id = $1`, fx.blueprintRunID).Scan(&brStatus); err != nil {
		t.Fatalf("read back blueprint_run: %v", err)
	}
	if runStatus != "open" || brStatus != "cancelled" {
		t.Errorf("(run=%q, blueprint_run=%q), want (open, cancelled)", runStatus, brStatus)
	}
	if !parked {
		t.Error("reaped run has no parked_at; the retention sweep keys a parked workspace off it")
	}
}

// TestReapDeadExecutors_FreshHeartbeatNeverReaped pins the negative case a
// draining executor relies on: a claimed run under an executor whose
// heartbeat is still fresh is left completely untouched, regardless of
// draining — the predicate is heartbeat-staleness only, so "draining is not
// death" holds without the reaper needing to know about drain at all.
func TestReapDeadExecutors_FreshHeartbeatNeverReaped(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	ctx := context.Background()

	fx := seedReaperFixture(t, h, 1)
	// Simulate an operator draining this executor — still heartbeating,
	// just refusing new claims locally. The reaper must not care.
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	if _, err := stores.Instances.SetDraining(ctx, fx.executorID, true); err != nil {
		t.Fatalf("SetDraining: %v", err)
	}

	store := reaper.NewPostgresStore(h.AdminDB)
	counts, err := store.ReapDeadExecutors(ctx, 30*time.Second, 2)
	if err != nil {
		t.Fatalf("ReapDeadExecutors: %v", err)
	}
	if counts.Requeued != 0 || counts.Failed != 0 || counts.Cancelled != 0 {
		t.Fatalf("counts = %+v, want all-zero (fresh heartbeat, draining or not)", counts)
	}

	var active int
	if err := h.AdminDB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM claims WHERE conversation_id = $1 AND released_at IS NULL`, fx.runID,
	).Scan(&active); err != nil {
		t.Fatalf("read back run: %v", err)
	}
	if active != 1 {
		t.Errorf("active claims = %d, want 1 (the live engagement is untouched)", active)
	}
}

// TestDeleteStaleInstances_DeletesOnlyStaleAndPreservesRunsExecutorID pins
// the registry GC: only rows past staleAfter are deleted, and the claim
// that referenced the GC'd instance survives with its executor_id intact —
// claims.executor_id carries no FK to instances (verified separately by
// TestClaimsExecutorID_HasNoForeignKeyToInstances), so a delete here can
// never cascade into audit history.
func TestDeleteStaleInstances_DeletesOnlyStaleAndPreservesRunsExecutorID(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	ctx := context.Background()

	fx := seedReaperFixture(t, h, 1)
	backdateHeartbeat(t, h, fx.executorID, 8*24*time.Hour) // 8 days stale

	freshID := "reaper-fresh-" + uuid.New().String()[:8]
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	if _, err := stores.Instances.Register(ctx, freshID, domain.InstanceRoleExecutor, "v1", ""); err != nil {
		t.Fatalf("register fresh instance: %v", err)
	}

	store := reaper.NewPostgresStore(h.AdminDB)
	n, err := store.DeleteStaleInstances(ctx, 7*24*time.Hour)
	if err != nil {
		t.Fatalf("DeleteStaleInstances: %v", err)
	}
	if n != 1 {
		t.Fatalf("deleted = %d, want 1", n)
	}

	if got, err := stores.Instances.Get(ctx, fx.executorID); err != nil || got != nil {
		t.Fatalf("stale instance still present after GC: got=%v err=%v", got, err)
	}
	if got, err := stores.Instances.Get(ctx, freshID); err != nil || got == nil {
		t.Fatalf("fresh instance was deleted by GC: got=%v err=%v", got, err)
	}

	var executorID string
	if err := h.AdminDB.QueryRowContext(ctx, `SELECT executor_id FROM claims WHERE conversation_id = $1 AND released_at IS NULL`, fx.runID).Scan(&executorID); err != nil {
		t.Fatalf("read back claim after GC'd its executor: %v", err)
	}
	if executorID != fx.executorID {
		t.Errorf("claims.executor_id = %q after GC, want it to survive unchanged as %q", executorID, fx.executorID)
	}

	// A GC'd id that comes back alive simply re-registers at boot_epoch 1
	// (the epoch counter has no memory of the deleted row) — harmless per
	// the ticket's decision log.
	epoch, err := stores.Instances.Register(ctx, fx.executorID, domain.InstanceRoleExecutor, "v2", "")
	if err != nil {
		t.Fatalf("re-register a GC'd id: %v", err)
	}
	if epoch != 1 {
		t.Errorf("re-registered epoch = %d, want 1 (fresh row, no memory of the deleted one)", epoch)
	}
}

// TestHealClaimDesyncs_ReleasesTerminalDanglingClaims pins the periodic
// janitor: a terminal conversation with a dangling active claim gets the
// claim released (outcome mapped from status), while a healthy engaged run
// and a mid-flight claimless one are both untouched. That last shape used to
// be the janitor's second arm — under the derived model a released claim on
// a mid-flight conversation IS the requeue, so there is nothing left to heal
// about it.
func TestHealClaimDesyncs_ReleasesTerminalDanglingClaims(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	ctx := context.Background()

	// Crash-after-flip: the conversation committed terminal but its claim
	// release never landed.
	terminal := seedReaperFixture(t, h, 1)
	pgtest.MustExec(t, h.AdminDB, `UPDATE conversations SET status = 'completed' WHERE id = $1`, terminal.runID)

	// Healthy: mid-flight with a live claim. Seeded BEFORE the claimless
	// fixture below, because the claim is cross-org and takes the oldest
	// eligible conversation — a run left claimable would be picked up by the
	// next fixture's claim instead of its own.
	healthy := seedReaperFixture(t, h, 1)

	// Mid-flight with the claim released: the ordinary claimable state now,
	// stale placement stamp and all (the next claim re-earns affinity).
	claimless := seedReaperFixture(t, h, 1)
	pgtest.MustExec(t, h.AdminDB, `
		UPDATE claims SET released_at = now(), outcome = 'failed'
		WHERE conversation_id = $1 AND released_at IS NULL
	`, claimless.runID)
	pgtest.MustExec(t, h.AdminDB, `UPDATE conversations SET preferred_executor_id = 'exec-dead' WHERE id = $1`, claimless.runID)

	store := reaper.NewPostgresStore(h.AdminDB)
	released, err := store.HealClaimDesyncs(ctx)
	if err != nil {
		t.Fatalf("HealClaimDesyncs: %v", err)
	}
	if released != 1 {
		t.Fatalf("released = %d, want 1", released)
	}

	var rel bool
	var outcome string
	if err := h.AdminDB.QueryRowContext(ctx, `
		SELECT released_at IS NOT NULL, COALESCE(outcome, '') FROM claims
		WHERE conversation_id = $1 ORDER BY claimed_at DESC LIMIT 1
	`, terminal.runID).Scan(&rel, &outcome); err != nil {
		t.Fatalf("read terminal row's claim: %v", err)
	}
	if !rel || outcome != "completed" {
		t.Errorf("terminal row's claim = (released=%v, outcome=%q), want (true, completed)", rel, outcome)
	}

	var status sql.NullString
	var pref any
	if err := h.AdminDB.QueryRowContext(ctx, `
		SELECT status, preferred_executor_id FROM conversations WHERE id = $1
	`, claimless.runID).Scan(&status, &pref); err != nil {
		t.Fatalf("read claimless row: %v", err)
	}
	if status.Valid {
		t.Errorf("claimless row status = %q, want none (already claimable)", status.String)
	}

	var healthyStatus sql.NullString
	var active int
	if err := h.AdminDB.QueryRowContext(ctx, `
		SELECT c.status, (SELECT COUNT(*) FROM claims WHERE conversation_id = c.id AND released_at IS NULL)
		FROM conversations c WHERE c.id = $1
	`, healthy.runID).Scan(&healthyStatus, &active); err != nil {
		t.Fatalf("read healthy row: %v", err)
	}
	if healthyStatus.Valid || active != 1 {
		t.Errorf("healthy row = (status=%q, active claims=%d), want (none, 1)", healthyStatus.String, active)
	}

	// Idempotent: a second sweep finds nothing.
	if released, err := store.HealClaimDesyncs(ctx); err != nil || released != 0 {
		t.Errorf("second sweep = (%d, %v), want (0, nil)", released, err)
	}
}

// TestClaimsExecutorID_HasNoForeignKeyToInstances pins the schema invariant
// the GC's safety argument depends on: claims.executor_id is a plain text
// column with no FK constraint into instances, so deleting an instances row
// can never cascade into (or be blocked by) claims.
func TestClaimsExecutorID_HasNoForeignKeyToInstances(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)

	var count int
	if err := h.AdminDB.QueryRow(`
		SELECT COUNT(*)
		FROM information_schema.table_constraints tc
		JOIN information_schema.key_column_usage kcu
		  ON tc.constraint_name = kcu.constraint_name AND tc.table_schema = kcu.table_schema
		WHERE tc.table_name = 'claims' AND kcu.column_name = 'executor_id' AND tc.constraint_type = 'FOREIGN KEY'
	`).Scan(&count); err != nil {
		t.Fatalf("query constraints: %v", err)
	}
	if count != 0 {
		t.Fatalf("claims.executor_id has %d foreign key constraint(s), want 0 — a FK here would make GC deletes cascade into (or be blocked by) audit history", count)
	}
}

// TestCancelStrandedCuratorTurns_CancelsDeadHomeOnly pins the curator homing
// recovery (spec §6.3): the active claim of a turn homed to a dead (missing
// or stale-heartbeat) executor is released with outcome 'cancelled' so it
// stops showing in-flight, while a claim homed to a live executor is left
// untouched. Recovery is retire-only — the user's next turn re-homes to a
// live executor. A queued turn is an unowned undelivered message with no
// claim, so it needs no reaping and is untouched by construction.
func TestCancelStrandedCuratorTurns_CancelsDeadHomeOnly(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	ctx := context.Background()

	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	orgID, userID, _ := pgtest.SeedOrgWithUser(t, h, "curhome-"+uuid.New().String()[:8])
	teamID := reaperFirstTeamForOrg(t, h, orgID)

	projectID := uuid.New().String()
	pgtest.MustExec(t, h.AdminDB, `
		INSERT INTO projects (id, org_id, creator_user_id, team_id, name, created_at, updated_at)
		VALUES ($1, $2, $3, $4, 'curator-home-proj', now(), now())
	`, projectID, orgID, userID, teamID)

	// One live executor, one dead (stale-heartbeat) executor.
	liveExec := "live-" + uuid.New().String()[:8]
	deadExec := "dead-" + uuid.New().String()[:8]
	for _, id := range []string{liveExec, deadExec} {
		if _, err := stores.Instances.Register(ctx, id, domain.InstanceRoleExecutor, "v1", ""); err != nil {
			t.Fatalf("register %s: %v", id, err)
		}
	}
	backdateHeartbeat(t, h, deadExec, time.Hour)

	// Two curator conversations, each mid-turn: one active claim homed to the
	// dead executor (stranded) and one homed to the live executor (survives).
	insertTurn := func(home string) (convID, claimID string) {
		convID = uuid.New().String()
		pgtest.MustExec(t, h.AdminDB, `
			INSERT INTO conversations (id, org_id, type, creator_user_id, team_id, visibility, trigger_type, origin, status, project_id)
			VALUES ($1, $2, 'curator', $3, $4, 'private', 'manual', 'curator', NULL, $5)
		`, convID, orgID, userID, teamID, projectID)
		claimID = uuid.New().String()
		pgtest.MustExec(t, h.AdminDB, `
			INSERT INTO claims (id, org_id, conversation_id, executor_id, boot_epoch)
			VALUES ($1, $2, $3, $4, 1)
		`, claimID, orgID, convID, home)
		return convID, claimID
	}
	strandedConv, strandedClaim := insertTurn(deadExec)
	_, survivorClaim := insertTurn(liveExec)

	// The stranded turn already streamed a token-bearing message before its
	// home died — the ledger keeps it; the sweep is an outcome/error release
	// only and must not disturb the row.
	pgtest.MustExec(t, h.AdminDB, `
		INSERT INTO messages (org_id, conversation_id, user_id, claim_id, role, input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens)
		VALUES ($1, $2, $3, $4, 'assistant', 7, 13, 5, 9)
	`, orgID, strandedConv, userID, strandedClaim)

	store := reaper.NewPostgresStore(h.AdminDB)
	n, err := store.CancelStrandedCuratorTurns(ctx, 30*time.Second)
	if err != nil {
		t.Fatalf("CancelStrandedCuratorTurns: %v", err)
	}
	if n != 1 {
		t.Fatalf("cancelled %d turns, want 1 (only the dead-home claim)", n)
	}
	// The stranded claim is released cancelled; the streamed tokens still
	// read through the ledger.
	var released bool
	var outcome string
	if err := h.AdminDB.QueryRowContext(ctx,
		`SELECT released_at IS NOT NULL, COALESCE(outcome, '') FROM claims WHERE id = $1`, strandedClaim,
	).Scan(&released, &outcome); err != nil {
		t.Fatalf("read back stranded claim: %v", err)
	}
	if !released || outcome != "cancelled" {
		t.Errorf("stranded claim = (released=%v outcome=%q), want (true, cancelled)", released, outcome)
	}
	var in, out, cr, cc int
	if err := h.AdminDB.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(input_tokens), 0), COALESCE(SUM(output_tokens), 0),
		        COALESCE(SUM(cache_read_tokens), 0), COALESCE(SUM(cache_creation_tokens), 0)
		   FROM messages WHERE claim_id = $1`, strandedClaim,
	).Scan(&in, &out, &cr, &cc); err != nil {
		t.Fatalf("read back stranded ledger: %v", err)
	}
	if in != 7 || out != 13 || cr != 5 || cc != 9 {
		t.Errorf("stranded-turn ledger tokens = (%d,%d,%d,%d), want (7,13,5,9)", in, out, cr, cc)
	}

	// The live-home claim is untouched (still active, no outcome).
	if err := h.AdminDB.QueryRowContext(ctx,
		`SELECT released_at IS NOT NULL, COALESCE(outcome, '') FROM claims WHERE id = $1`, survivorClaim,
	).Scan(&released, &outcome); err != nil {
		t.Fatalf("read back survivor claim: %v", err)
	}
	if released || outcome != "" {
		t.Errorf("live-home claim = (released=%v outcome=%q), want (false, \"\") — the sweep must ignore live homes", released, outcome)
	}
}

// orphanFixture is the mint→enqueue crash shape plus everything needed to
// re-fire the same task afterwards: an org with a blueprint / prompt / task /
// trigger chain, and a mint helper that produces event-triggered blueprint_runs
// against it. The runs it mints carry no child conversation, which is the whole
// point — that absence is what makes the row invisible to every other reaper arm.
type orphanFixture struct {
	orgID, userID, teamID string
	taskID, blueprintID   string
	promptID, triggerID   string
	entityID              string
	nextStep              int
}

func seedOrphanFixture(t *testing.T, h *pgtest.Harness) *orphanFixture {
	t.Helper()
	orgID, userID, _ := pgtest.SeedOrgWithUser(t, h, "orphan-"+uuid.New().String()[:8])
	teamID := reaperFirstTeamForOrg(t, h, orgID)

	blueprintID := "orphan-bp-" + uuid.New().String()[:8]
	pgtest.MustExec(t, h.AdminDB, `
		INSERT INTO blueprints (id, org_id, team_id, creator_user_id, name, source, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $1, 'user', now(), now())
	`, blueprintID, orgID, teamID, userID)

	promptID := "orphan-p-" + uuid.New().String()[:8]
	pgtest.MustExec(t, h.AdminDB, `
		INSERT INTO prompts (id, org_id, team_id, creator_user_id, name, body, source, allowed_tools, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $1, 'body', 'user', '[]'::jsonb, now(), now())
	`, promptID, orgID, teamID, userID)

	entityID := uuid.New().String()
	pgtest.MustExec(t, h.AdminDB, `
		INSERT INTO entities (id, org_id, source, source_id, kind, title, url, snapshot_json, created_at)
		VALUES ($1, $2, 'github', $3, 'pr', 'Orphan Test Entity', 'https://example/x', '{}'::jsonb, now())
	`, entityID, orgID, "orphan-test-"+entityID[:8])

	fx := &orphanFixture{
		orgID: orgID, userID: userID, teamID: teamID,
		blueprintID: blueprintID, promptID: promptID, entityID: entityID,
	}

	taskID := uuid.New().String()
	pgtest.MustExec(t, h.AdminDB, `
		INSERT INTO tasks (id, org_id, creator_user_id, team_id, entity_id, event_type, dedup_key, primary_event_id, status, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, '', $7, 'queued', now())
	`, taskID, orgID, userID, teamID, entityID, domain.EventGitHubPRCICheckFailed, fx.newEvent(t, h))
	fx.taskID = taskID

	triggerID := uuid.New().String()
	pgtest.MustExec(t, h.AdminDB, `
		INSERT INTO event_handlers (id, org_id, team_id, creator_user_id, kind, event_type, enabled, source, blueprint_id, breaker_threshold, min_autonomy_suitability, created_at, updated_at)
		VALUES ($1, $2, $3, $4, 'trigger', $5, true, 'user', $6, 4, 0, now(), now())
	`, triggerID, orgID, teamID, userID, domain.EventGitHubPRCICheckFailed, blueprintID)
	fx.triggerID = triggerID

	return fx
}

// newEvent records one more detection on the fixture's entity — a distinct
// (triggering_event_id, trigger_id) pair per firing, so the replay fence never
// stands in for the one-active index the tests here are about.
func (fx *orphanFixture) newEvent(t *testing.T, h *pgtest.Harness) string {
	t.Helper()
	eventID := uuid.New().String()
	pgtest.MustExec(t, h.AdminDB, `
		INSERT INTO events (id, org_id, entity_id, event_type, dedup_key, metadata_json, created_at)
		VALUES ($1, $2, $3, $4, '', '{}'::jsonb, now())
	`, eventID, fx.orgID, fx.entityID, domain.EventGitHubPRCICheckFailed)
	return eventID
}

// firing is the BlueprintRun an auto-delegation would mint for this task: an
// event-triggered run against a fresh event, which is what holds
// blueprint_runs_one_active_auto_run_per_task.
func (fx *orphanFixture) firing(t *testing.T, h *pgtest.Harness) domain.BlueprintRun {
	t.Helper()
	return domain.BlueprintRun{
		ID:                uuid.New().String(),
		BlueprintID:       fx.blueprintID,
		TaskID:            fx.taskID,
		TriggerType:       domain.BlueprintTriggerEvent,
		TriggerID:         fx.triggerID,
		TriggeringEventID: fx.newEvent(t, h),
		Status:            domain.BlueprintRunStatusRunning,
		WorktreePath:      "/tmp/wt-orphan",
		StepPlan:          []domain.BlueprintPlanStep{{StepIndex: 0, PromptID: fx.promptID, PromptName: "p", PromptBody: "b"}},
	}
}

// mintOrphan commits one childless 'running' blueprint_run — exactly what the
// firing path leaves behind when it dies between its two commits — backdated by
// age so the test controls which side of the grace it falls on.
func (fx *orphanFixture) mintOrphan(t *testing.T, h *pgtest.Harness, age time.Duration) string {
	t.Helper()
	ctx := context.Background()
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	br := fx.firing(t, h)
	inserted, err := stores.Blueprints.CreateRunIfNotFiredSystem(ctx, fx.orgID, br)
	if err != nil || !inserted {
		t.Fatalf("CreateRunIfNotFiredSystem = (%v, %v), want (true, nil)", inserted, err)
	}
	if age > 0 {
		pgtest.MustExec(t, h.AdminDB,
			`UPDATE blueprint_runs SET started_at = now() - $2::interval WHERE id = $1`, br.ID, age.String())
	}
	return br.ID
}

// enqueueChild stages one mid-flight step conversation under brID — the second
// commit the crash window skips, and the presence that makes a run ordinary.
func (fx *orphanFixture) enqueueChild(t *testing.T, h *pgtest.Harness, brID string) string {
	t.Helper()
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	idx := fx.nextStep
	fx.nextStep++
	convID := uuid.New().String()
	if err := stores.RunQueue.EnqueueRun(context.Background(), fx.orgID, domain.Conversation{
		ID: convID, TaskID: fx.taskID, PromptID: fx.promptID, Model: "m",
		TriggerType: "event", BlueprintRunID: brID, BlueprintStepIndex: &idx,
	}); err != nil {
		t.Fatalf("EnqueueRun: %v", err)
	}
	return convID
}

func orphanBlueprintState(t *testing.T, h *pgtest.Harness, brID string) (status, abortReason string, completedAtSet bool) {
	t.Helper()
	var reason sql.NullString
	if err := h.AdminDB.QueryRow(
		`SELECT status, abort_reason, completed_at IS NOT NULL FROM blueprint_runs WHERE id = $1`, brID,
	).Scan(&status, &reason, &completedAtSet); err != nil {
		t.Fatalf("read blueprint_run %s: %v", brID, err)
	}
	return status, reason.String, completedAtSet
}

// TestFailBlueprintRunsOrphanedAtMint_SweepsChildlessRunPastGrace pins the arm
// itself: a 'running' blueprint_run with no child conversation, older than the
// grace, takes the terminal that frees its task — and nothing else in the
// deployment can reach that row, since every other arm joins through
// conversations and this one has none.
func TestFailBlueprintRunsOrphanedAtMint_SweepsChildlessRunPastGrace(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	ctx := context.Background()

	fx := seedOrphanFixture(t, h)
	brID := fx.mintOrphan(t, h, domain.BlueprintOrphanedAtMintGrace+time.Minute)

	store := reaper.NewPostgresStore(h.AdminDB)
	n, err := store.FailBlueprintRunsOrphanedAtMint(ctx, domain.BlueprintOrphanedAtMintGrace)
	if err != nil {
		t.Fatalf("FailBlueprintRunsOrphanedAtMint: %v", err)
	}
	if n != 1 {
		t.Fatalf("swept %d runs, want 1", n)
	}
	status, reason, completed := orphanBlueprintState(t, h, brID)
	if status != string(domain.BlueprintRunStatusFailed) {
		t.Errorf("status = %q, want failed", status)
	}
	if reason != domain.BlueprintAbortOrphanedAtMint {
		t.Errorf("abort_reason = %q, want %q", reason, domain.BlueprintAbortOrphanedAtMint)
	}
	if !completed {
		t.Error("completed_at is NULL on a terminal blueprint_run")
	}
	// The replay fence survives on the failed row, so an at-least-once
	// redelivery of the SAME event stays the no-op it always was — re-firing is
	// the pending firing's job, as a fresh firing.
	var fenceHeld bool
	if err := h.AdminDB.QueryRow(
		`SELECT triggering_event_id IS NOT NULL AND trigger_id IS NOT NULL FROM blueprint_runs WHERE id = $1`, brID,
	).Scan(&fenceHeld); err != nil {
		t.Fatalf("read fence columns: %v", err)
	}
	if !fenceHeld {
		t.Error("the failed run dropped its (triggering_event_id, trigger_id) fence")
	}

	// Idempotent: the row is terminal, so a second sweep finds nothing.
	if n2, err := store.FailBlueprintRunsOrphanedAtMint(ctx, domain.BlueprintOrphanedAtMintGrace); err != nil || n2 != 0 {
		t.Errorf("second sweep = (%d, %v), want (0, nil)", n2, err)
	}
}

// TestFailBlueprintRunsOrphanedAtMint_NegativeSpace pins the three ways this
// arm could misfire, each of which would kill live or already-settled work.
func TestFailBlueprintRunsOrphanedAtMint_NegativeSpace(t *testing.T) {
	h := pgtest.Shared(t)
	ctx := context.Background()
	grace := domain.BlueprintOrphanedAtMintGrace

	t.Run("run_with_a_child_at_any_age", func(t *testing.T) {
		// A child is proof the mint completed. After that, age is just how long
		// the work has been going — failing it would kill a live agent mid-step.
		h.Reset(t)
		fx := seedOrphanFixture(t, h)
		brID := fx.mintOrphan(t, h, 30*24*time.Hour)
		fx.enqueueChild(t, h, brID)

		store := reaper.NewPostgresStore(h.AdminDB)
		n, err := store.FailBlueprintRunsOrphanedAtMint(ctx, grace)
		if err != nil {
			t.Fatalf("FailBlueprintRunsOrphanedAtMint: %v", err)
		}
		if n != 0 {
			t.Fatalf("swept %d runs, want 0 (a run with a child is not an orphan)", n)
		}
		if status, _, _ := orphanBlueprintState(t, h, brID); status != string(domain.BlueprintRunStatusRunning) {
			t.Errorf("status = %q, want running", status)
		}
	})

	t.Run("childless_run_inside_the_grace", func(t *testing.T) {
		// The window this arm recovers from is also the window a healthy mint
		// passes through, so the grace is the only thing keeping the sweep off a
		// live Delegate.
		h.Reset(t)
		fx := seedOrphanFixture(t, h)
		brID := fx.mintOrphan(t, h, 0)

		store := reaper.NewPostgresStore(h.AdminDB)
		n, err := store.FailBlueprintRunsOrphanedAtMint(ctx, grace)
		if err != nil {
			t.Fatalf("FailBlueprintRunsOrphanedAtMint: %v", err)
		}
		if n != 0 {
			t.Fatalf("swept %d runs, want 0 (a mint still inside the grace)", n)
		}
		if status, _, _ := orphanBlueprintState(t, h, brID); status != string(domain.BlueprintRunStatusRunning) {
			t.Errorf("status = %q, want running", status)
		}
	})

	t.Run("terminal_childless_run_keeps_its_verdict", func(t *testing.T) {
		// A terminal is a settled account of what happened; the arm may only
		// claim a run still reading 'running'.
		h.Reset(t)
		fx := seedOrphanFixture(t, h)
		brID := fx.mintOrphan(t, h, grace+time.Minute)
		pgtest.MustExec(t, h.AdminDB,
			`UPDATE blueprint_runs SET status = 'cancelled', abort_reason = 'user_cancelled', completed_at = now() WHERE id = $1`, brID)

		store := reaper.NewPostgresStore(h.AdminDB)
		n, err := store.FailBlueprintRunsOrphanedAtMint(ctx, grace)
		if err != nil {
			t.Fatalf("FailBlueprintRunsOrphanedAtMint: %v", err)
		}
		if n != 0 {
			t.Fatalf("swept %d runs, want 0 (terminal runs are settled)", n)
		}
		status, reason, _ := orphanBlueprintState(t, h, brID)
		if status != string(domain.BlueprintRunStatusCancelled) || reason != "user_cancelled" {
			t.Errorf("terminal run = (%q, %q), want (cancelled, user_cancelled)", status, reason)
		}
	})
}

// TestFailBlueprintRunsOrphanedAtMint_UnblocksTheOneActiveIndex is the
// end-to-end point of the arm, at the level where the damage actually happened.
//
// The orphan holds blueprint_runs_one_active_auto_run_per_task against its task
// while the router's busy gate — which reads CONVERSATIONS — sees an idle task.
// So every drain of the task's pending firing re-fires, hits the index, comes
// back task-busy, and is released to pending again: a livelock at the sweeper's
// interval with no breaker. This test reproduces exactly that refusal, sweeps,
// and then re-fires the same firing to a fully-minted run with a real child —
// which is what the queued pending firing does on its next drain.
func TestFailBlueprintRunsOrphanedAtMint_UnblocksTheOneActiveIndex(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	ctx := context.Background()
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)

	fx := seedOrphanFixture(t, h)
	orphanID := fx.mintOrphan(t, h, domain.BlueprintOrphanedAtMintGrace+time.Minute)

	// The livelock: a NEW (event, trigger) firing on the same task — a fresh
	// intent, not a replay — is refused by the index the orphan is holding.
	blocked := fx.firing(t, h)
	if _, err := stores.Blueprints.CreateRunIfNotFiredSystem(ctx, fx.orgID, blocked); !errors.Is(err, db.ErrTaskBusyActiveAutoRun) {
		t.Fatalf("re-fire under the orphan = %v, want ErrTaskBusyActiveAutoRun", err)
	}

	store := reaper.NewPostgresStore(h.AdminDB)
	if n, err := store.FailBlueprintRunsOrphanedAtMint(ctx, domain.BlueprintOrphanedAtMintGrace); err != nil || n != 1 {
		t.Fatalf("FailBlueprintRunsOrphanedAtMint = (%d, %v), want (1, nil)", n, err)
	}

	// The same firing now lands, and its step conversation with it: the task is
	// working again, on a run that has an owner.
	fresh := fx.firing(t, h)
	inserted, err := stores.Blueprints.CreateRunIfNotFiredSystem(ctx, fx.orgID, fresh)
	if err != nil || !inserted {
		t.Fatalf("re-fire after sweep = (%v, %v), want (true, nil) — the index is still held", inserted, err)
	}
	childID := fx.enqueueChild(t, h, fresh.ID)

	var childCount int
	if err := h.AdminDB.QueryRow(
		`SELECT count(*) FROM conversations WHERE blueprint_run_id = $1`, fresh.ID,
	).Scan(&childCount); err != nil {
		t.Fatalf("count fresh run children: %v", err)
	}
	if childCount != 1 {
		t.Errorf("fresh run has %d children, want 1 (%s)", childCount, childID)
	}
	if status, _, _ := orphanBlueprintState(t, h, orphanID); status != string(domain.BlueprintRunStatusFailed) {
		t.Errorf("orphan status = %q, want failed", status)
	}
	// And the sweep is not hungry: the fresh run has a child, so it is never a
	// candidate no matter how long it runs.
	pgtest.MustExec(t, h.AdminDB,
		`UPDATE blueprint_runs SET started_at = now() - '1 day'::interval WHERE id = $1`, fresh.ID)
	if n, err := store.FailBlueprintRunsOrphanedAtMint(ctx, domain.BlueprintOrphanedAtMintGrace); err != nil || n != 0 {
		t.Errorf("sweep after re-fire = (%d, %v), want (0, nil) — the replacement run must survive", n, err)
	}
}
