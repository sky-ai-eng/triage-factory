package reaper_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/reaper"
)

// reaperFixture is one claimed conversation + its owning blueprint_run +
// the executor instance that (nominally) owns it, ready for a test to
// manipulate (backdate the heartbeat, bump attempts, request a cancel)
// before calling ReapDeadExecutors.
type reaperFixture struct {
	conversationID string
	blueprintRunID string
	executorID     string
	orgID, userID  string
}

// seedReaperFixture mints an org, a blueprint/task/prompt chain, a running
// blueprint_run, and one claimed conversation under a freshly-registered
// executor instance — the live claim the reaper will find.
//
// priorOutcomes is the claim history to lay down behind that live claim,
// oldest first, one released claim per entry. Every entry is produced by the
// production path that writes its outcome (each ClaimNextConversation mints a real
// claims row; no raw INSERT), because the reaper counts an EPISODE — the
// trailing run of handed-back claims — and only the real paths put the
// boundaries where production puts them:
//
//	"requeued"  — ConversationQueueStore.RequeueConversation: a hand-back, which keeps the
//	              current episode open.
//	"cancelled" — a deliberate stop and a resume: an engagement that
//	              concluded, which ENDS the episode and starts the next one
//	              at zero however many claims came before it.
func seedReaperFixture(t *testing.T, h *pgtest.Harness, priorOutcomes ...string) reaperFixture {
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

	executorID := "reaper-executor-" + uuid.New().String()[:8]
	if _, err := stores.Instances.Register(ctx, executorID, domain.InstanceRoleExecutor, "v1", ""); err != nil {
		t.Fatalf("register executor: %v", err)
	}

	// One firing, through the door production fires through: the
	// blueprint_run and its step-0 conversation commit together.
	blueprintRunID := uuid.New().String()
	conversationID := uuid.New().String()
	step0 := 0
	if _, _, _, err := stores.Blueprints.CreateRunWithFirstStepSystem(ctx, orgID, domain.BlueprintRun{
		ID: blueprintRunID, BlueprintID: blueprintID, TaskID: taskID,
		TriggerType: domain.BlueprintTriggerManual, WorktreePath: "/tmp/wt-" + blueprintRunID,
	}, db.AgentClaimStamp{}, "", domain.Conversation{
		ID: conversationID, TaskID: taskID, PromptID: promptID, Model: "m",
		TriggerType: "manual", CreatorUserID: userID, BlueprintRunID: blueprintRunID, BlueprintStepIndex: &step0,
	}); err != nil {
		t.Fatalf("CreateRunWithFirstStepSystem: %v", err)
	}
	for i := 0; i <= len(priorOutcomes); i++ {
		// The claim is cross-org and takes the globally-oldest eligible
		// conversation, so a test seeding several fixtures must leave no
		// older one claimable — assert the identity rather than let a
		// mis-claim pass silently.
		got, err := stores.ConversationQueue.ClaimNextConversation(ctx, executorID, 1, db.ClaimPlacement{}, db.DefaultClaimLease)
		if err != nil || got == nil || got.ID != conversationID {
			t.Fatalf("ClaimNextConversation (claim %d): got=%v err=%v, want conversation %s", i+1, got, err, conversationID)
		}
		if i == len(priorOutcomes) {
			break // the live claim — left held, for the reaper to find
		}
		switch outcome := priorOutcomes[i]; outcome {
		case "requeued":
			if _, err := stores.ConversationQueue.RequeueConversation(ctx, orgID, conversationID, "test churn"); err != nil {
				t.Fatalf("RequeueConversation (claim %d): %v", i+1, err)
			}
		case "cancelled":
			if ok, err := stores.Conversations.ParkOpen(ctx, orgID, conversationID, db.ParkStopped("user_cancelled", "")); err != nil || !ok {
				t.Fatalf("ParkOpen (claim %d): ok=%v err=%v", i+1, ok, err)
			}
			if ok, err := stores.Conversations.MarkQueuedForResume(ctx, orgID, conversationID); err != nil || !ok {
				t.Fatalf("MarkQueuedForResume (claim %d): ok=%v err=%v", i+1, ok, err)
			}
		default:
			t.Fatalf("seedReaperFixture: unsupported prior claim outcome %q", outcome)
		}
	}

	return reaperFixture{conversationID: conversationID, blueprintRunID: blueprintRunID, executorID: executorID, orgID: orgID, userID: userID}
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
// recovery path: a claimed conversation under a stale-heartbeat executor, its loss
// episode still under budget, is requeued (active claim released with
// outcome 'reaped', no new claim minted) rather than failed.
func TestReapDeadExecutors_RequeuesUnderAttemptBudget(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	ctx := context.Background()

	fx := seedReaperFixture(t, h)
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
		`SELECT status FROM conversations WHERE id = $1`, fx.conversationID,
	).Scan(&status); err != nil {
		t.Fatalf("read back conversation: %v", err)
	}
	if status.Valid {
		t.Errorf("status = %q, want none — releasing the claim IS the requeue", status.String)
	}
	// The dead engagement is released — a requeued row has no owner — and
	// the release carries the claim-level 'reaped' outcome.
	var active int
	if err := h.AdminDB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM claims WHERE conversation_id = $1 AND released_at IS NULL`, fx.conversationID,
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
		   FROM claims WHERE conversation_id = $1 ORDER BY claimed_at DESC LIMIT 1`, fx.conversationID,
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
// exhausted-episode path: a loss episode that reaches maxAttempts — a
// genuine crash loop, one hand-back and then the death being reaped here —
// terminal-fails the conversation with failure_kind='executor_lost' and finalizes the
// owning blueprint_run failed, instead of requeuing it forever.
func TestReapDeadExecutors_TerminalFailsExecutorLostPastAttemptBudget(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	ctx := context.Background()

	fx := seedReaperFixture(t, h, "requeued") // episode == maxAttempts below
	backdateHeartbeat(t, h, fx.executorID, time.Hour)

	store := reaper.NewPostgresStore(h.AdminDB)
	counts, err := store.ReapDeadExecutors(ctx, 30*time.Second, 2)
	if err != nil {
		t.Fatalf("ReapDeadExecutors: %v", err)
	}
	if counts.Failed != 1 || counts.Requeued != 0 || counts.Cancelled != 0 {
		t.Fatalf("counts = %+v, want {Failed:1}", counts)
	}

	var status, failureKind, endedReason string
	var ended bool
	if err := h.AdminDB.QueryRowContext(ctx,
		`SELECT status, COALESCE(failure_kind, ''), ended_at IS NOT NULL, COALESCE(ended_reason, '')
		   FROM conversations WHERE id = $1`, fx.conversationID,
	).Scan(&status, &failureKind, &ended, &endedReason); err != nil {
		t.Fatalf("read back conversation: %v", err)
	}
	if status != "failed" || failureKind != string(domain.ConversationFailureExecutorLost) {
		t.Errorf("conversation (status=%q failure_kind=%q), want (failed, executor_lost)", status, failureKind)
	}
	// The boundary rides the same statement. Without it the reaped
	// conversation stays resumable and owes its memory to nobody — and the
	// brain is the only thing left that can say so for a run whose executor
	// is gone.
	if !ended || endedReason != string(domain.EndedFailed) {
		t.Errorf("conversation boundary = (ended=%v, reason=%q), want a stamp with reason %q",
			ended, endedReason, domain.EndedFailed)
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

// TestReapDeadExecutors_RequeuesAfterEngagementsThatConcluded pins the unit
// the budget is counted in. This conversation has been stopped and resumed
// twice, so it is on its THIRD lifetime claim when its executor dies — over
// a lifetime budget of 2 before the first thing ever went wrong with it.
// Counted as an episode it is at 1, because each stop concluded an
// engagement, so the death is requeued and the blueprint keeps running.
func TestReapDeadExecutors_RequeuesAfterEngagementsThatConcluded(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	ctx := context.Background()

	fx := seedReaperFixture(t, h, "cancelled", "cancelled")
	backdateHeartbeat(t, h, fx.executorID, time.Hour)
	// Stamp the dead executor as the placement preference, so the requeue's
	// clearing of it is observable rather than vacuous.
	pgtest.MustExec(t, h.AdminDB, `UPDATE conversations SET preferred_executor_id = $2 WHERE id = $1`, fx.conversationID, fx.executorID)

	var lifetime int
	if err := h.AdminDB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM claims WHERE conversation_id = $1`, fx.conversationID,
	).Scan(&lifetime); err != nil {
		t.Fatalf("count lifetime claims: %v", err)
	}
	if lifetime != 3 {
		t.Fatalf("lifetime claims = %d, want 3 — the fixture must be over a lifetime budget of 2 for this test to mean anything", lifetime)
	}

	store := reaper.NewPostgresStore(h.AdminDB)
	counts, err := store.ReapDeadExecutors(ctx, 30*time.Second, 2)
	if err != nil {
		t.Fatalf("ReapDeadExecutors: %v", err)
	}
	if counts.Requeued != 1 || counts.Failed != 0 || counts.Cancelled != 0 {
		t.Fatalf("counts = %+v, want {Requeued:1} — a resumed conversation's first executor death is not a crash loop", counts)
	}

	var status sql.NullString
	var preferred sql.NullString
	if err := h.AdminDB.QueryRowContext(ctx,
		`SELECT status, preferred_executor_id FROM conversations WHERE id = $1`, fx.conversationID,
	).Scan(&status, &preferred); err != nil {
		t.Fatalf("read back conversation: %v", err)
	}
	if status.Valid {
		t.Errorf("status = %q, want none — releasing the claim IS the requeue", status.String)
	}
	if preferred.Valid {
		t.Errorf("preferred_executor_id = %q, want none — affinity is never carried toward a corpse", preferred.String)
	}

	var brStatus string
	if err := h.AdminDB.QueryRowContext(ctx,
		`SELECT status FROM blueprint_runs WHERE id = $1`, fx.blueprintRunID,
	).Scan(&brStatus); err != nil {
		t.Fatalf("read back blueprint_run: %v", err)
	}
	if brStatus != "running" {
		t.Errorf("blueprint_run status = %q, want running — a requeue must not abort the blueprint", brStatus)
	}
}

// TestReapDeadExecutors_TerminalFailsEpisodeBehindAConcludedEngagement is the
// other side of the same boundary: the conversation concluded an engagement
// once, but everything after it is hand-backs, and that trailing run reaches
// the budget. Episode counting must find the loop that starts mid-history —
// counting only since the last conclusion is not the same as forgiving
// everything before it.
func TestReapDeadExecutors_TerminalFailsEpisodeBehindAConcludedEngagement(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	ctx := context.Background()

	fx := seedReaperFixture(t, h, "cancelled", "requeued")
	backdateHeartbeat(t, h, fx.executorID, time.Hour)

	store := reaper.NewPostgresStore(h.AdminDB)
	counts, err := store.ReapDeadExecutors(ctx, 30*time.Second, 2)
	if err != nil {
		t.Fatalf("ReapDeadExecutors: %v", err)
	}
	if counts.Failed != 1 || counts.Requeued != 0 || counts.Cancelled != 0 {
		t.Fatalf("counts = %+v, want {Failed:1} — the trailing hand-back plus this death is a full episode", counts)
	}

	var status, failureKind, brStatus string
	if err := h.AdminDB.QueryRowContext(ctx,
		`SELECT c.status, COALESCE(c.failure_kind, ''), br.status
		   FROM conversations c JOIN blueprint_runs br ON br.id = c.blueprint_run_id
		  WHERE c.id = $1`, fx.conversationID,
	).Scan(&status, &failureKind, &brStatus); err != nil {
		t.Fatalf("read back conversation: %v", err)
	}
	if status != "failed" || failureKind != string(domain.ConversationFailureExecutorLost) || brStatus != "failed" {
		t.Errorf("(conversation=%q failure_kind=%q blueprint_run=%q), want (failed, executor_lost, failed)", status, failureKind, brStatus)
	}
}

// TestReapDeadExecutors_CancelRequestedFinalizesCancelledNotRequeued pins
// "cancel-requested rows go through the existing cancel finalization
// instead of requeue" — even a conversation still within its attempt budget must
// finalize, never come back as queued work.
//
// The split is the one every cancel path uses: the blueprint takes the
// 'cancelled' terminal, the conversation parks `open` with its workspace, and
// the claim gate refuses the parked row because its blueprint is terminal.
func TestReapDeadExecutors_CancelRequestedFinalizesCancelledNotRequeued(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	ctx := context.Background()

	fx := seedReaperFixture(t, h)
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

	var convStatus, brStatus string
	var parked bool
	if err := h.AdminDB.QueryRowContext(ctx, `SELECT status, parked_at IS NOT NULL FROM conversations WHERE id = $1`, fx.conversationID).Scan(&convStatus, &parked); err != nil {
		t.Fatalf("read back conversation: %v", err)
	}
	if err := h.AdminDB.QueryRowContext(ctx, `SELECT status FROM blueprint_runs WHERE id = $1`, fx.blueprintRunID).Scan(&brStatus); err != nil {
		t.Fatalf("read back blueprint_run: %v", err)
	}
	if convStatus != "open" || brStatus != "cancelled" {
		t.Errorf("(conversation=%q, blueprint_run=%q), want (open, cancelled)", convStatus, brStatus)
	}
	if !parked {
		t.Error("reaped conversation has no parked_at; the retention sweep keys a parked workspace off it")
	}
}

// TestReapDeadExecutors_FreshHeartbeatNeverReaped pins the negative case a
// draining executor relies on: a claimed conversation under an executor whose
// heartbeat is still fresh is left completely untouched, regardless of
// draining — the predicate is heartbeat-staleness only, so "draining is not
// death" holds without the reaper needing to know about drain at all.
func TestReapDeadExecutors_FreshHeartbeatNeverReaped(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	ctx := context.Background()

	fx := seedReaperFixture(t, h)
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
		`SELECT COUNT(*) FROM claims WHERE conversation_id = $1 AND released_at IS NULL`, fx.conversationID,
	).Scan(&active); err != nil {
		t.Fatalf("read back conversation: %v", err)
	}
	if active != 1 {
		t.Errorf("active claims = %d, want 1 (the live engagement is untouched)", active)
	}
}

// TestDeleteStaleInstances_DeletesOnlyStaleAndPreservesClaimsExecutorID pins
// the registry GC: only rows past staleAfter are deleted, and the claim
// that referenced the GC'd instance survives with its executor_id intact —
// claims.executor_id carries no FK to instances (verified separately by
// TestClaimsExecutorID_HasNoForeignKeyToInstances), so a delete here can
// never cascade into audit history.
func TestDeleteStaleInstances_DeletesOnlyStaleAndPreservesClaimsExecutorID(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	ctx := context.Background()

	fx := seedReaperFixture(t, h)
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
	if err := h.AdminDB.QueryRowContext(ctx, `SELECT executor_id FROM claims WHERE conversation_id = $1 AND released_at IS NULL`, fx.conversationID).Scan(&executorID); err != nil {
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
// claim released (outcome mapped from status), while a healthy engaged conversation
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
	terminal := seedReaperFixture(t, h)
	pgtest.MustExec(t, h.AdminDB, `UPDATE conversations SET status = 'completed' WHERE id = $1`, terminal.conversationID)

	// Healthy: mid-flight with a live claim. Seeded BEFORE the claimless
	// fixture below, because the claim is cross-org and takes the oldest
	// eligible conversation — a conversation left claimable would be picked up by the
	// next fixture's claim instead of its own.
	healthy := seedReaperFixture(t, h)

	// Mid-flight with the claim released: the ordinary claimable state now,
	// stale placement stamp and all (the next claim re-earns affinity).
	claimless := seedReaperFixture(t, h)
	pgtest.MustExec(t, h.AdminDB, `
		UPDATE claims SET released_at = now(), outcome = 'failed'
		WHERE conversation_id = $1 AND released_at IS NULL
	`, claimless.conversationID)
	pgtest.MustExec(t, h.AdminDB, `UPDATE conversations SET preferred_executor_id = 'exec-dead' WHERE id = $1`, claimless.conversationID)

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
	`, terminal.conversationID).Scan(&rel, &outcome); err != nil {
		t.Fatalf("read terminal row's claim: %v", err)
	}
	if !rel || outcome != "completed" {
		t.Errorf("terminal row's claim = (released=%v, outcome=%q), want (true, completed)", rel, outcome)
	}

	var status sql.NullString
	var pref any
	if err := h.AdminDB.QueryRowContext(ctx, `
		SELECT status, preferred_executor_id FROM conversations WHERE id = $1
	`, claimless.conversationID).Scan(&status, &pref); err != nil {
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
	`, healthy.conversationID).Scan(&healthyStatus, &active); err != nil {
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

// expireClaimLease backdates a conversation's live claim so its lease has
// lapsed. It is how a test stages the dead engagement the instance heartbeat
// cannot see: the process is alive and writing its heartbeat, but the
// goroutine that held this one conversation is not.
func expireClaimLease(t *testing.T, h *pgtest.Harness, conversationID string) {
	t.Helper()
	pgtest.MustExec(t, h.AdminDB, `
		UPDATE claims SET lease_expires_at = now() - interval '1 second'
		WHERE conversation_id = $1 AND released_at IS NULL
	`, conversationID)
}

// TestReapDeadExecutors_ReapsAnExpiredLeaseOnALiveInstance pins the second
// candidate arm. An executor renews one row every few seconds for the whole
// process, so its heartbeat cannot say whether the goroutine driving one
// conversation is alive; only that claim's own lease can. A claim past expiry
// is reaped whatever the instance says, and it is safe to reap because the
// boot-checked ordering put the holder's self-fence strictly before it.
func TestReapDeadExecutors_ReapsAnExpiredLeaseOnALiveInstance(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	ctx := context.Background()

	fx := seedReaperFixture(t, h)
	// No heartbeat backdating: the instance is registered and fresh.
	expireClaimLease(t, h, fx.conversationID)

	store := reaper.NewPostgresStore(h.AdminDB)
	counts, err := store.ReapDeadExecutors(ctx, 30*time.Second, 2)
	if err != nil {
		t.Fatalf("ReapDeadExecutors: %v", err)
	}
	if counts.Requeued != 1 || counts.Failed != 0 || counts.Cancelled != 0 {
		t.Fatalf("counts = %+v, want {Requeued:1}: an expired lease is a dead engagement whatever its host is doing", counts)
	}
	var outcome string
	if err := h.AdminDB.QueryRowContext(ctx, `
		SELECT outcome FROM claims WHERE conversation_id = $1 ORDER BY claimed_at DESC LIMIT 1
	`, fx.conversationID).Scan(&outcome); err != nil {
		t.Fatalf("read released claim: %v", err)
	}
	if outcome != "reaped" {
		t.Errorf("released claim outcome = %q, want reaped", outcome)
	}
}

// TestReapDeadExecutors_LeavesALiveLeaseOnALiveInstance is the other half:
// the arm must not widen the sweep. A healthy engagement renewing its lease
// on a heartbeating host is untouched, however long it has been running —
// claim age alone stops nothing.
func TestReapDeadExecutors_LeavesALiveLeaseOnALiveInstance(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	ctx := context.Background()

	fx := seedReaperFixture(t, h)

	store := reaper.NewPostgresStore(h.AdminDB)
	counts, err := store.ReapDeadExecutors(ctx, 30*time.Second, 2)
	if err != nil {
		t.Fatalf("ReapDeadExecutors: %v", err)
	}
	if counts != (reaper.Counts{}) {
		t.Fatalf("counts = %+v, want nothing reaped", counts)
	}
	var active int
	if err := h.AdminDB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM claims WHERE conversation_id = $1 AND released_at IS NULL`, fx.conversationID,
	).Scan(&active); err != nil {
		t.Fatalf("count active claims: %v", err)
	}
	if active != 1 {
		t.Errorf("active claims = %d, want the live engagement still holding its claim", active)
	}
}
