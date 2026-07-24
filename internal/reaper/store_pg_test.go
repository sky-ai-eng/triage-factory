package reaper_test

import (
	"context"
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
		got, err := stores.RunQueue.ClaimNextRun(ctx, executorID, 1, db.ClaimPlacement{})
		if err != nil || got == nil {
			t.Fatalf("ClaimNextRun (attempt %d): got=%v err=%v", i+1, got, err)
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

	var status string
	if err := h.AdminDB.QueryRowContext(ctx,
		`SELECT status FROM conversations WHERE id = $1`, fx.runID,
	).Scan(&status); err != nil {
		t.Fatalf("read back run: %v", err)
	}
	if status != "queued" {
		t.Errorf("status = %q, want queued", status)
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
// finalize cancelled, never come back as queued work.
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
	if err := h.AdminDB.QueryRowContext(ctx, `SELECT status FROM conversations WHERE id = $1`, fx.runID).Scan(&runStatus); err != nil {
		t.Fatalf("read back run: %v", err)
	}
	if err := h.AdminDB.QueryRowContext(ctx, `SELECT status FROM blueprint_runs WHERE id = $1`, fx.blueprintRunID).Scan(&brStatus); err != nil {
		t.Fatalf("read back blueprint_run: %v", err)
	}
	if runStatus != "cancelled" || brStatus != "cancelled" {
		t.Errorf("(run=%q, blueprint_run=%q), want both cancelled", runStatus, brStatus)
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

	var status string
	if err := h.AdminDB.QueryRowContext(ctx, `SELECT status FROM conversations WHERE id = $1`, fx.runID).Scan(&status); err != nil {
		t.Fatalf("read back run: %v", err)
	}
	if status != "running" {
		t.Errorf("status = %q, want running (untouched)", status)
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

// TestHealClaimDesyncs_HealsBothStrandedShapes pins the periodic janitor: a
// terminal conversation with a dangling active claim gets the claim released
// (outcome mapped from status), an in-flight claimless run under a running
// parent is requeued, and a healthy claimed running run is untouched. These
// are the two shapes the non-atomic app-pool terminal writes can strand;
// the reaper repeating the boot-time arms bounds their lifetime to a tick.
func TestHealClaimDesyncs_HealsBothStrandedShapes(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	ctx := context.Background()

	// Crash-after-flip: the conversation committed terminal but its claim
	// release never landed.
	terminal := seedReaperFixture(t, h, 1)
	pgtest.MustExec(t, h.AdminDB, `UPDATE conversations SET status = 'completed' WHERE id = $1`, terminal.runID)

	// Rolled-back flip: the claim release committed but the conversation
	// stayed in-flight, with a stale placement stamp to clear.
	stranded := seedReaperFixture(t, h, 1)
	pgtest.MustExec(t, h.AdminDB, `
		UPDATE claims SET released_at = now(), outcome = 'failed'
		WHERE conversation_id = $1 AND released_at IS NULL
	`, stranded.runID)
	pgtest.MustExec(t, h.AdminDB, `UPDATE conversations SET preferred_executor_id = 'exec-dead' WHERE id = $1`, stranded.runID)

	// Healthy: running with a live claim.
	healthy := seedReaperFixture(t, h, 1)

	store := reaper.NewPostgresStore(h.AdminDB)
	requeued, released, err := store.HealClaimDesyncs(ctx)
	if err != nil {
		t.Fatalf("HealClaimDesyncs: %v", err)
	}
	if requeued != 1 || released != 1 {
		t.Fatalf("healed = (requeued=%d, released=%d), want (1, 1)", requeued, released)
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

	var status string
	var pref any
	if err := h.AdminDB.QueryRowContext(ctx, `
		SELECT status, preferred_executor_id FROM conversations WHERE id = $1
	`, stranded.runID).Scan(&status, &pref); err != nil {
		t.Fatalf("read stranded row: %v", err)
	}
	if status != "queued" || pref != nil {
		t.Errorf("stranded row = (status=%q, preferred=%v), want (queued, cleared)", status, pref)
	}

	var healthyStatus string
	var active int
	if err := h.AdminDB.QueryRowContext(ctx, `
		SELECT c.status, (SELECT COUNT(*) FROM claims WHERE conversation_id = c.id AND released_at IS NULL)
		FROM conversations c WHERE c.id = $1
	`, healthy.runID).Scan(&healthyStatus, &active); err != nil {
		t.Fatalf("read healthy row: %v", err)
	}
	if healthyStatus != "running" || active != 1 {
		t.Errorf("healthy row = (status=%q, active claims=%d), want (running, 1)", healthyStatus, active)
	}

	// Idempotent: a second sweep finds nothing.
	if requeued, released, err := store.HealClaimDesyncs(ctx); err != nil || requeued != 0 || released != 0 {
		t.Errorf("second sweep = (%d, %d, %v), want (0, 0, nil)", requeued, released, err)
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
