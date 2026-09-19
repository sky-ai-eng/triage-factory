package sqlite_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// sqliteRQExecutorID/sqliteRQBootEpoch are the fixed ownership identity most
// claims in this file stamp a row with.
const (
	sqliteRQExecutorID = "sqlite-test-executor"
	sqliteRQBootEpoch  = int64(1)
)

func TestConversationQueueStore_SQLite_ClaimCycle(t *testing.T) {
	conn := openSQLiteForTest(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()

	bpID, taskID, promptID := seedSqliteFiringParents(t, conn, stores, "rq-claim")

	// Empty queue: nothing claimable.
	if got, err := stores.ConversationQueue.ClaimNextConversation(ctx, sqliteRQExecutorID, sqliteRQBootEpoch, db.ClaimPlacement{}); err != nil || got != nil {
		t.Fatalf("ClaimNextConversation on empty queue = (%v, %v), want (nil, nil)", got, err)
	}

	step := fireSqliteStep(t, conn, stores, bpID, taskID, domain.Conversation{
		ID: "rq-conv-0", PromptID: promptID, Model: "claude-sonnet-4-6",
	})
	brID := step.BlueprintRunID

	// Simulate a conversation that got partway (captured a session) before being
	// re-queued by a crash reconcile, so the claim must carry session_id back
	// for runAgent's resume-on-reclaim path.
	if _, err := conn.Exec(`UPDATE conversations SET sdk_session_id = 'rq-sess' WHERE id = 'rq-conv-0'`); err != nil {
		t.Fatalf("set session_id: %v", err)
	}

	got, err := stores.ConversationQueue.ClaimNextConversation(ctx, sqliteRQExecutorID, sqliteRQBootEpoch, db.ClaimPlacement{})
	if err != nil {
		t.Fatalf("ClaimNextConversation: %v", err)
	}
	if got == nil {
		t.Fatal("ClaimNextConversation returned nil for a queued conversation")
	}
	if got.ID != "rq-conv-0" || got.BlueprintRunID != brID {
		t.Fatalf("claimed conversation = %+v", got)
	}
	// The claim writes no status: mid-flight is the absence of an outcome,
	// and the claim row itself is the ownership.
	var stored sql.NullString
	if err := conn.QueryRow(`SELECT status FROM conversations WHERE id = 'rq-conv-0'`).Scan(&stored); err != nil {
		t.Fatalf("read stored status: %v", err)
	}
	if stored.Valid {
		t.Fatalf("stored status after claim = %q, want NULL", stored.String)
	}
	if got.SessionID != "rq-sess" {
		t.Fatalf("claimed session_id = %q, want rq-sess (resume-on-reclaim plumbing)", got.SessionID)
	}
	if got.Attempts != 1 {
		t.Fatalf("attempts = %d, want 1", got.Attempts)
	}
	if got.BlueprintStepIndex == nil || *got.BlueprintStepIndex != 0 {
		t.Fatalf("step index = %v, want 0", got.BlueprintStepIndex)
	}
	// team_id rides back on the claim (TFAC-458): the dispatcher stamps
	// cfg.teamID = the claimed row's TeamID, which becomes the
	// construction-path ConversationInfo.TeamID the capture writers attribute by.
	if got.TeamID != runmode.LocalDefaultTeamID {
		t.Fatalf("claimed team_id = %q, want %q", got.TeamID, runmode.LocalDefaultTeamID)
	}

	// Claimed → no longer queued, so a second claim finds nothing.
	if got2, err := stores.ConversationQueue.ClaimNextConversation(ctx, sqliteRQExecutorID, sqliteRQBootEpoch, db.ClaimPlacement{}); err != nil || got2 != nil {
		t.Fatalf("second ClaimNextConversation = (%v, %v), want (nil, nil)", got2, err)
	}
}

func TestConversationQueueStore_SQLite_CancelRequestedNotClaimed(t *testing.T) {
	conn := openSQLiteForTest(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()
	org := runmode.LocalDefaultOrgID

	brID := stageSqliteStep(t, conn, stores, "rq-cancel").BlueprintRunID

	// Raise the sequence-cancel signal: the queued step must not be claimed.
	changed, err := stores.Blueprints.RequestRunCancelSystem(ctx, org, brID)
	if err != nil || !changed {
		t.Fatalf("RequestRunCancelSystem = (%v, %v), want (true, nil)", changed, err)
	}
	if got, err := stores.ConversationQueue.ClaimNextConversation(ctx, sqliteRQExecutorID, sqliteRQBootEpoch, db.ClaimPlacement{}); err != nil || got != nil {
		t.Fatalf("ClaimNextConversation on cancel-requested blueprint = (%v, %v), want (nil, nil)", got, err)
	}
	// Idempotent: re-requesting on an already-flagged running row reports no change.
	if changed, err := stores.Blueprints.RequestRunCancelSystem(ctx, org, brID); err != nil || changed {
		t.Fatalf("second RequestRunCancelSystem = (%v, %v), want (false, nil)", changed, err)
	}
	// The flag is readable back through GetRun.
	br, err := stores.Blueprints.GetRunSystem(ctx, org, brID)
	if err != nil || br == nil || !br.CancelRequested {
		t.Fatalf("GetRunSystem cancel_requested = %v (br=%+v, err=%v)", br != nil && br.CancelRequested, br, err)
	}
}

func TestConversationQueueStore_SQLite_RequeueAndReset(t *testing.T) {
	conn := openSQLiteForTest(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()
	org := runmode.LocalDefaultOrgID

	convID := stageSqliteStep(t, conn, stores, "rq-reset").ID

	// Claim it → running, attempts=1.
	claimed, err := stores.ConversationQueue.ClaimNextConversation(ctx, sqliteRQExecutorID, sqliteRQBootEpoch, db.ClaimPlacement{})
	if err != nil || claimed == nil {
		t.Fatalf("ClaimNextConversation: (%v, %v)", claimed, err)
	}

	// RequeueConversation puts it back to queued (attempts retained), re-claimable.
	if _, err := stores.ConversationQueue.RequeueConversation(ctx, org, convID, "transient setup error"); err != nil {
		t.Fatalf("RequeueConversation: %v", err)
	}
	reclaimed, err := stores.ConversationQueue.ClaimNextConversation(ctx, sqliteRQExecutorID, sqliteRQBootEpoch, db.ClaimPlacement{})
	if err != nil || reclaimed == nil {
		t.Fatalf("re-ClaimNextConversation: (%v, %v)", reclaimed, err)
	}
	if reclaimed.Attempts != 2 {
		t.Fatalf("attempts after requeue+reclaim = %d, want 2", reclaimed.Attempts)
	}

	// Now the conversation is 'running' again (mid-flight).
	// ResetProcessingConversations should flip it back to 'queued'.
	n, err := stores.ConversationQueue.ResetProcessingConversations(ctx, sqliteRQExecutorID, sqliteRQBootEpoch+1)
	if err != nil {
		t.Fatalf("ResetProcessingConversations: %v", err)
	}
	if n != 1 {
		t.Fatalf("ResetProcessingConversations reset %d rows, want 1", n)
	}
	afterReset, err := stores.ConversationQueue.ClaimNextConversation(ctx, sqliteRQExecutorID, sqliteRQBootEpoch, db.ClaimPlacement{})
	if err != nil || afterReset == nil {
		t.Fatalf("ClaimNextConversation after reset: (%v, %v)", afterReset, err)
	}
	if afterReset.Attempts != 3 {
		t.Fatalf("attempts after reset+reclaim = %d, want 3 (reset retains attempts)", afterReset.Attempts)
	}
}

// TestConversationQueueStore_SQLite_RequeueFromSetupPhase pins that RequeueConversation fires
// no matter which setup phase the conversation's active claim is in: setup progress
// lives on the claim, the conversation stays 'running' the whole time, so a
// workspace-setup failure mid-phase must still requeue the row and make it
// re-claimable. Coverage walks the canonical vocabulary, so a phase added in
// Go and not handled here fails rather than going untested.
func TestConversationQueueStore_SQLite_RequeueFromSetupPhase(t *testing.T) {
	for _, phase := range domain.AllClaimPhases() {
		t.Run(phase, func(t *testing.T) {
			conn := openSQLiteForTest(t)
			stores := sqlitestore.New(conn)
			ctx := context.Background()
			org := runmode.LocalDefaultOrgID

			convID := stageSqliteStep(t, conn, stores, "rq-setup-"+phase).ID
			if claimed, err := stores.ConversationQueue.ClaimNextConversation(ctx, sqliteRQExecutorID, sqliteRQBootEpoch, db.ClaimPlacement{}); err != nil || claimed == nil {
				t.Fatalf("ClaimNextConversation: (%v, %v)", claimed, err)
			}
			// Advance the claim into the setup phase the dispatcher would
			// have recorded before the workspace-setup failure fired the
			// requeue.
			if _, err := stores.Conversations.SetActiveClaimPhaseSystem(ctx, org, convID, phase); err != nil {
				t.Fatalf("SetActiveClaimPhaseSystem(%s): %v", phase, err)
			}

			if _, err := stores.ConversationQueue.RequeueConversation(ctx, org, convID, "workspace setup: boom"); err != nil {
				t.Fatalf("RequeueConversation: %v", err)
			}
			after, err := stores.Conversations.GetSystem(ctx, org, convID)
			if err != nil || after == nil {
				t.Fatalf("GetSystem after requeue: (%v, %v)", after, err)
			}
			if after.Status != "queued" {
				t.Fatalf("status after requeue from phase %q = %q, want queued (a mid-setup phase must not block the requeue)", phase, after.Status)
			}
			if reclaimed, err := stores.ConversationQueue.ClaimNextConversation(ctx, sqliteRQExecutorID, sqliteRQBootEpoch, db.ClaimPlacement{}); err != nil || reclaimed == nil {
				t.Fatalf("re-ClaimNextConversation after requeue from phase %q: (%v, %v)", phase, reclaimed, err)
			}
		})
	}
}

func TestConversationQueueStore_SQLite_ResetLeavesDormantAlone(t *testing.T) {
	conn := openSQLiteForTest(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()
	org := runmode.LocalDefaultOrgID

	bpID, taskID, promptID := seedSqliteFiringParents(t, conn, stores, "rq-dormant")
	created, err := stores.Blueprints.CreateRun(ctx, org, domain.BlueprintRun{
		ID: "rqd-br", BlueprintID: bpID, TaskID: taskID,
		TriggerType: domain.BlueprintTriggerManual, Status: domain.BlueprintRunStatusRunning,
		WorktreePath: "/tmp/wt-rqd",
	})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	brID := created.ID
	// A parked (dormant) conversation — directly insert with status open, then stamp
	// it as owned by THIS instance from an EARLIER boot (epoch 0 < the
	// sweep's epoch). Without the stamp the ownership predicate alone would
	// exclude the row and this test would pass even with 'open' dropped
	// from the status exclusion list; with it, only the status list
	// protects the row — which is exactly the invariant being pinned
	// (parked rows stay parked through a self-sweep of prior-boot orphans).
	step0 := 0
	insertConversationForTest(t, conn, domain.Conversation{
		ID: "rqd-run-0", TaskID: taskID, PromptID: promptID, Status: "open",
		Model: "m", TriggerType: "manual", BlueprintRunID: brID, BlueprintStepIndex: &step0,
	})
	insertActiveClaimForTest(t, conn, "rqd-run-0", sqliteRQExecutorID, 0)

	n, err := stores.ConversationQueue.ResetProcessingConversations(ctx, sqliteRQExecutorID, sqliteRQBootEpoch)
	if err != nil {
		t.Fatalf("ResetProcessingConversations: %v", err)
	}
	if n != 0 {
		t.Fatalf("ResetProcessingConversations reset %d rows, want 0 (dormant conversation must stay parked)", n)
	}
	// And it is not claimable (it's not 'queued').
	if got, err := stores.ConversationQueue.ClaimNextConversation(ctx, sqliteRQExecutorID, sqliteRQBootEpoch, db.ClaimPlacement{}); err != nil || got != nil {
		t.Fatalf("ClaimNextConversation = (%v, %v), want (nil, nil)", got, err)
	}
}

// TestConversationQueueStore_SQLite_ResetProcessingConversations_ScopedToOwner pins the
// TFAC-578 ownership predicate on the SQLite impl too — SQLite is N=1 (there
// is never a live sibling to protect in practice), but the predicate must
// hold identically to Postgres: a different executor_id is never swept, and
// the same executor/epoch (not yet an earlier boot) is left untouched.
func TestConversationQueueStore_SQLite_ResetProcessingConversations_ScopedToOwner(t *testing.T) {
	conn := openSQLiteForTest(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()

	stageSqliteStep(t, conn, stores, "rq-scoped")
	if _, err := stores.ConversationQueue.ClaimNextConversation(ctx, "instance-a", 3, db.ClaimPlacement{}); err != nil {
		t.Fatalf("claim: %v", err)
	}

	// A different executor_id (never happens at real N=1, but the predicate
	// must not match) leaves the row untouched.
	if n, err := stores.ConversationQueue.ResetProcessingConversations(ctx, "instance-b", 4); err != nil {
		t.Fatalf("ResetProcessingConversations (other executor): %v", err)
	} else if n != 0 {
		t.Errorf("ResetProcessingConversations for a different executor_id reset %d rows, want 0", n)
	}
	// The same executor at the SAME epoch (not an earlier boot) also leaves
	// it untouched.
	if n, err := stores.ConversationQueue.ResetProcessingConversations(ctx, "instance-a", 3); err != nil {
		t.Fatalf("ResetProcessingConversations (same epoch): %v", err)
	} else if n != 0 {
		t.Errorf("ResetProcessingConversations at the same epoch reset %d rows, want 0", n)
	}
	// The same executor at a LATER epoch (a restart) sweeps it.
	if n, err := stores.ConversationQueue.ResetProcessingConversations(ctx, "instance-a", 4); err != nil {
		t.Fatalf("ResetProcessingConversations (restart): %v", err)
	} else if n != 1 {
		t.Errorf("ResetProcessingConversations on restart reset %d rows, want 1", n)
	}
}

func TestConversationQueueStore_SQLite_SetCurrentStep(t *testing.T) {
	conn := openSQLiteForTest(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()
	org := runmode.LocalDefaultOrgID

	task := seedEntityEventTask(t, conn, "rq-step")
	insertBlueprintForTest(t, conn, "rqs-bp", "RQ Step Blueprint")
	created, err := stores.Blueprints.CreateRun(ctx, org, domain.BlueprintRun{
		ID: "rqs-br", BlueprintID: "rqs-bp", TaskID: task.ID,
		TriggerType: domain.BlueprintTriggerManual, Status: domain.BlueprintRunStatusRunning,
		WorktreePath: "/tmp/wt-rqs",
	})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	brID := created.ID
	if _, err := stores.Blueprints.SetRunCurrentStepSystem(ctx, org, brID, 3); err != nil {
		t.Fatalf("SetRunCurrentStepSystem: %v", err)
	}
	br, err := stores.Blueprints.GetRunSystem(ctx, org, brID)
	if err != nil || br == nil {
		t.Fatalf("GetRunSystem: (%v, %v)", br, err)
	}
	if br.CurrentStepIndex != 3 {
		t.Fatalf("CurrentStepIndex = %d, want 3", br.CurrentStepIndex)
	}
}

// TestConversationQueueStore_SQLite_MintStampsActorAgent pins the audit gap fix:
// the mint persists conversations.actor_agent_id, and the
// ConversationStore.Get read projection JOINs agents to surface the bot's
// display name as ActorAgentName for the "Ran as: {name}" UI. A step minted
// with no actor reads back with both fields empty (the column's nullable
// contract).
func TestConversationQueueStore_SQLite_MintStampsActorAgent(t *testing.T) {
	conn := openSQLiteForTest(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()
	org := runmode.LocalDefaultOrgID

	// One org agent backs conversations.actor_agent_id (FK) and carries the
	// display name the read JOIN denormalizes.
	agentID, err := stores.Agents.Create(ctx, org, domain.Agent{DisplayName: "Triage Bot"})
	if err != nil {
		t.Fatalf("Agents.Create: %v", err)
	}

	bpID, taskID, promptID := seedSqliteFiringParents(t, conn, stores, "rq-actor")
	withActor := fireSqliteStep(t, conn, stores, bpID, taskID, domain.Conversation{
		PromptID: promptID, ActorAgentID: agentID,
	})

	got, err := stores.Conversations.Get(ctx, org, withActor.ID)
	if err != nil || got == nil {
		t.Fatalf("Conversations.Get: (%v, %v)", got, err)
	}
	if got.ActorAgentID != agentID {
		t.Errorf("ActorAgentID = %q, want %q (the mint must persist the actor)", got.ActorAgentID, agentID)
	}
	if got.ActorAgentName != "Triage Bot" {
		t.Errorf("ActorAgentName = %q, want %q (read JOIN must denormalize agents.display_name)", got.ActorAgentName, "Triage Bot")
	}

	// A conversation with no actor reads back with both fields empty — the nullable
	// column + LEFT JOIN degrade to "" rather than erroring. A step after the
	// first is minted by the advance, which is the only door that mints one.
	noActorID := advanceSqliteStep(t, stores, withActor, domain.Conversation{PromptID: promptID}).ID
	noActor, err := stores.Conversations.Get(ctx, org, noActorID)
	if err != nil || noActor == nil {
		t.Fatalf("Conversations.Get (no actor): (%v, %v)", noActor, err)
	}
	if noActor.ActorAgentID != "" || noActor.ActorAgentName != "" {
		t.Errorf("no-actor conversation = (id %q, name %q), want both empty", noActor.ActorAgentID, noActor.ActorAgentName)
	}
}

// TestConversationQueueStore_SQLite_MintStampsTheSDKEngine is the twin of the
// Postgres assertion, and the pair is the point: the engine a delegation runs
// on is decided by the dialect, because the dialect is the mode. Read
// together they say the divergence is deliberate; either one alone could be
// mistaken for the default nobody set.
//
// The stamp is explicit here too, rather than left to the column DEFAULT, so
// that a later change to that DEFAULT is free to move without silently
// re-homing local's delegations onto another engine.
func TestConversationQueueStore_SQLite_MintStampsTheSDKEngine(t *testing.T) {
	conn := openSQLiteForTest(t)
	stores := sqlitestore.New(conn)

	convID := stageSqliteStep(t, conn, stores, "rq-engine").ID

	var runtime string
	if err := conn.QueryRow(`SELECT runtime FROM conversations WHERE id = ?`, convID).Scan(&runtime); err != nil {
		t.Fatalf("read runtime: %v", err)
	}
	if runtime != domain.ConversationRuntimeSDK {
		t.Errorf("mint runtime = %q, want %q", runtime, domain.ConversationRuntimeSDK)
	}
}

// TestConversationQueueStore_SQLite_Credentials runs the shared awaiting-credentials
// pubkey conformance suite against the SQLite impl. Each factory call opens
// a fresh in-memory DB so subtests don't share state.
func TestConversationQueueStore_SQLite_Credentials(t *testing.T) {
	dbtest.RunClaimCredentialsConformance(t, func(t *testing.T) (db.ConversationQueueStore, string, dbtest.ClaimCredentialsSeeder) {
		t.Helper()
		conn := openSQLiteForTest(t)
		stores := sqlitestore.New(conn)
		org := runmode.LocalDefaultOrgID

		seq := 0
		seed := dbtest.ClaimCredentialsSeeder{
			StageStep: func(t *testing.T) string {
				t.Helper()
				seq++
				return stageSqliteStep(t, conn, stores, fmt.Sprintf("rq-cred-%d", seq)).ID
			},
			ConversationStatus: func(t *testing.T, conversationID string) string {
				t.Helper()
				var status sql.NullString
				if err := conn.QueryRow(`SELECT status FROM conversations WHERE id = ?`, conversationID).Scan(&status); err != nil {
					t.Fatalf("read status: %v", err)
				}
				return status.String
			},
			SetActivePhase: func(t *testing.T, conversationID, phase string) {
				t.Helper()
				if _, err := conn.Exec(`
					UPDATE claims SET phase = NULLIF(?, '')
					WHERE conversation_id = ? AND released_at IS NULL
				`, phase, conversationID); err != nil {
					t.Fatalf("set active phase %q: %v", phase, err)
				}
			},
		}
		return stores.ConversationQueue, org, seed
	})
}

// TestConversationQueueStore_SQLite_FleetQueueShares runs the shared per-org queue-share
// conformance against the SQLite impl (N=1 — one local org).
func TestConversationQueueStore_SQLite_FleetQueueShares(t *testing.T) {
	dbtest.RunFleetQueueSharesConformance(t, func(t *testing.T) (db.ConversationQueueStore, string, dbtest.FleetQueueSharesSeeder) {
		t.Helper()
		conn := openSQLiteForTest(t)
		stores := sqlitestore.New(conn)
		org := runmode.LocalDefaultOrgID

		taskSeq := 0
		seed := dbtest.FleetQueueSharesSeeder{
			// One task and one blueprint_run per staged run — the real firing
			// model (one delegation = one blueprint_run on its own task), and
			// what makes several queued rows concurrently claimable: a
			// blueprint drives its current step and no other, so siblings
			// under one blueprint could never all be queued at once, and one
			// task never holds two running blueprints at all.
			StageStep: func(t *testing.T) string {
				t.Helper()
				taskSeq++
				return stageSqliteStep(t, conn, stores, fmt.Sprintf("rq-fqs-%d", taskSeq)).ID
			},
			ForceStatus: func(t *testing.T, conversationID, status string) {
				t.Helper()
				if _, err := conn.Exec(`UPDATE conversations SET status = ? WHERE id = ?`, status, conversationID); err != nil {
					t.Fatalf("force status %q: %v", status, err)
				}
			},
			SetMaxConcurrentRuns: func(t *testing.T, cap *int) {
				t.Helper()
				if _, err := conn.Exec(`
					INSERT INTO org_settings (org_id, max_concurrent_runs) VALUES (?, ?)
					ON CONFLICT(org_id) DO UPDATE SET max_concurrent_runs = excluded.max_concurrent_runs
				`, org, cap); err != nil {
					t.Fatalf("set max_concurrent_runs: %v", err)
				}
			},
		}
		return stores.ConversationQueue, org, seed
	})
}

func TestConversationQueueStore_SQLite_RejectsNonLocalOrg(t *testing.T) {
	conn := openSQLiteForTest(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()
	const bogusOrg = "11111111-1111-1111-1111-111111111111"

	if _, err := stores.ConversationQueue.RequeueConversation(ctx, bogusOrg, "r", "x"); err == nil {
		t.Errorf("RequeueConversation with non-local orgID should error")
	}
}

// TestConversationQueueStore_SQLite_QueuedAtStamps pins the queue-dwell timestamps the
// UI's queue timer reads: the mint stamps queued_at, a claim mints the claims
// row whose claimed_at Get derives, and a requeue re-stamps queued_at while
// releasing the claim — history is kept, ownership is not.
func TestConversationQueueStore_SQLite_QueuedAtStamps(t *testing.T) {
	conn := openSQLiteForTest(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()
	org := runmode.LocalDefaultOrgID

	convID := stageSqliteStep(t, conn, stores, "rq-dwell").ID

	queued, err := stores.Conversations.Get(ctx, org, convID)
	if err != nil || queued == nil {
		t.Fatalf("Get after the mint: (%v, %v)", queued, err)
	}
	if queued.QueuedAt == nil {
		t.Fatal("QueuedAt = nil after the mint; the mint must stamp queue entry")
	}
	if queued.ClaimedAt != nil {
		t.Fatalf("ClaimedAt = %v on a queued conversation, want nil", queued.ClaimedAt)
	}
	firstQueuedAt := *queued.QueuedAt

	if got, err := stores.ConversationQueue.ClaimNextConversation(ctx, sqliteRQExecutorID, sqliteRQBootEpoch, db.ClaimPlacement{}); err != nil || got == nil {
		t.Fatalf("ClaimNextConversation: (%v, %v)", got, err)
	}
	claimed, err := stores.Conversations.Get(ctx, org, convID)
	if err != nil || claimed == nil {
		t.Fatalf("Get after claim: (%v, %v)", claimed, err)
	}
	if claimed.ClaimedAt == nil {
		t.Fatal("ClaimedAt = nil after claim")
	}
	if claimed.ClaimedAt.Before(firstQueuedAt) {
		t.Fatalf("ClaimedAt %v precedes QueuedAt %v", claimed.ClaimedAt, firstQueuedAt)
	}

	if _, err := stores.ConversationQueue.RequeueConversation(ctx, org, convID, "transient setup error"); err != nil {
		t.Fatalf("RequeueConversation: %v", err)
	}
	requeued, err := stores.Conversations.Get(ctx, org, convID)
	if err != nil || requeued == nil {
		t.Fatalf("Get after requeue: (%v, %v)", requeued, err)
	}
	if requeued.QueuedAt == nil || requeued.QueuedAt.Before(firstQueuedAt) {
		t.Fatalf("QueuedAt after requeue = %v, want re-stamped at/after the first stamp %v", requeued.QueuedAt, firstQueuedAt)
	}
	// The claims model keeps engagement history: ClaimedAt stays the released
	// claim's stamp, but the row reads unowned (no active claim).
	if requeued.ClaimedAt == nil {
		t.Fatal("ClaimedAt = nil after requeue, want the released engagement's stamp retained")
	}
	if requeued.ExecutorID != "" {
		t.Fatalf("ExecutorID = %q after requeue, want empty (a queued row has no active claim)", requeued.ExecutorID)
	}
}

// TestConversationQueueStore_SQLite_ExecutorClaims runs the shared operator
// claim-projection conformance against the SQLite impl. The read is
// deployment-wide by construction; SQLite is N=1, so the arm proves the SQL
// and the scan rather than any cross-org behavior.
func TestConversationQueueStore_SQLite_ExecutorClaims(t *testing.T) {
	dbtest.RunExecutorClaimsConformance(t, func(t *testing.T) (db.ConversationQueueStore, dbtest.ExecutorClaimsSeeder) {
		t.Helper()
		conn := openSQLiteForTest(t)
		stores := sqlitestore.New(conn)
		org := runmode.LocalDefaultOrgID

		seq := 0
		seed := dbtest.ExecutorClaimsSeeder{
			OrgID: org,
			Conversation: func(t *testing.T, status, failureKind string) string {
				t.Helper()
				seq++
				conversationID := stageSqliteStep(t, conn, stores, fmt.Sprintf("rq-exclaims-%d", seq)).ID
				if _, err := conn.Exec(`
					UPDATE conversations SET status = ?, failure_kind = NULLIF(?, '') WHERE id = ?
				`, status, failureKind, conversationID); err != nil {
					t.Fatalf("force terminal state: %v", err)
				}
				return conversationID
			},
			Claim: func(t *testing.T, row dbtest.ExecutorClaimRow) string {
				t.Helper()
				claimID := uuid.New().String()
				var released any
				if row.ReleasedAt != nil {
					released = row.ReleasedAt.UTC()
				}
				var peak, cpu any
				if row.PeakMemMB != nil {
					peak = *row.PeakMemMB
				}
				if row.CPUUsec != nil {
					cpu = *row.CPUUsec
				}
				if _, err := conn.Exec(`
					INSERT INTO claims (id, org_id, conversation_id, executor_id, boot_epoch,
					                    claimed_at, released_at, outcome, peak_mem_mb, cpu_usec)
					VALUES (?, ?, ?, ?, 1, ?, ?, NULLIF(?, ''), ?, ?)
				`, claimID, org, row.ConversationID, row.ExecutorID, row.ClaimedAt.UTC(), released,
					row.Outcome, peak, cpu); err != nil {
					t.Fatalf("insert claim: %v", err)
				}
				return claimID
			},
		}
		return stores.ConversationQueue, seed
	})
}

// TestClaimPredicate_SQLite runs the shared needs-driving-predicate +
// display-ladder conformance against the SQLite impl. Each factory call opens
// a fresh in-memory DB so subtests don't share state.
func TestClaimPredicate_SQLite(t *testing.T) {
	dbtest.RunClaimPredicateConformance(t, func(t *testing.T) dbtest.ClaimPredicateHarness {
		t.Helper()
		conn := openSQLiteForTest(t)
		stores := sqlitestore.New(conn)
		ctx := context.Background()
		org := runmode.LocalDefaultOrgID

		bpID, taskID, promptID := seedSqliteFiringParents(t, conn, stores, "rq-pred")

		brID, nextStep := "", 0
		return dbtest.ClaimPredicateHarness{
			Stores: stores,
			OrgID:  org,
			UserID: runmode.LocalDefaultUserID,
			StageDelegation: func(t *testing.T, runtime string) string {
				t.Helper()
				idx := nextStep
				nextStep++
				var convID string
				if idx == 0 {
					// Step 0 is a real firing, so the fresh-mint assertions —
					// no stored status, the dialect's own runtime stamp — are
					// about the statement production mints with.
					step0 := fireSqliteStep(t, conn, stores, bpID, taskID, domain.Conversation{PromptID: promptID})
					brID, convID = step0.BlueprintRunID, step0.ID
				} else {
					// Every step after it is written directly, pointer first,
					// then the row it names. The advance door commits those
					// two together and only from the step the pointer already
					// names — and this suite rewrites the pointer by hand
					// between stagings, parking a run behind its newest step
					// and resuming past it, so a door that refuses those
					// states is a door it cannot stage through.
					if _, err := conn.Exec(`UPDATE blueprint_runs SET current_step_index = ? WHERE id = ?`, idx, brID); err != nil {
						t.Fatalf("advance current_step_index: %v", err)
					}
					convID = uuid.New().String()
					insertConversationForTest(t, conn, domain.Conversation{
						ID: convID, TaskID: taskID, PromptID: promptID, Model: "m",
						TriggerType: "manual", BlueprintRunID: brID, BlueprintStepIndex: &idx,
					})
				}
				// The dialect stamps its own runtime at mint; rewrite it so
				// one backend covers both engines. started_at is stamped in
				// the same pass, one second apart per mint: the task clause
				// picks the task's NEWEST un-ended conversation, and SQLite's
				// CURRENT_TIMESTAMP resolves to the second, so a suite that
				// staged its rows inside one tick would be asking the id
				// tiebreak — a random uuid — which one that is.
				if _, err := conn.Exec(
					`UPDATE conversations SET runtime = ?, started_at = datetime('now', ? || ' seconds') WHERE id = ?`,
					runtime, fmt.Sprintf("%+d", idx), convID,
				); err != nil {
					t.Fatalf("set runtime: %v", err)
				}
				return convID
			},
			StageUnindexed: func(t *testing.T) error {
				t.Helper()
				unindexedBp, unindexedTask, unindexedPrompt := seedSqliteFiringParents(t, conn, stores, "rq-pred-unindexed")
				_, err := tryFireSqliteStep(t, conn, stores, unindexedBp, unindexedTask, domain.Conversation{
					PromptID: unindexedPrompt,
				})
				return err
			},
			SetStoredStatus: func(t *testing.T, convID, status string) {
				t.Helper()
				var stored any
				if status != "" {
					stored = status
				}
				if _, err := conn.Exec(`UPDATE conversations SET status = ? WHERE id = ?`, stored, convID); err != nil {
					t.Fatalf("set stored status: %v", err)
				}
			},
			StoredStatus: func(t *testing.T, convID string) string {
				t.Helper()
				var status sql.NullString
				if err := conn.QueryRow(`SELECT status FROM conversations WHERE id = ?`, convID).Scan(&status); err != nil {
					t.Fatalf("read stored status: %v", err)
				}
				return status.String
			},
			SetBlueprintState: func(t *testing.T, status string, currentStepIndex int) {
				t.Helper()
				// The sequence task carries exactly one run — the firing that
				// opened it — so the task is the address here and the harness
				// keeps no run id of its own.
				if _, err := conn.Exec(
					`UPDATE blueprint_runs SET status = ?, current_step_index = ? WHERE task_id = ?`,
					status, currentStepIndex, taskID,
				); err != nil {
					t.Fatalf("set blueprint state: %v", err)
				}
			},
			InsertRow: func(t *testing.T, convID string, msg domain.Message) int64 {
				t.Helper()
				msg.ConversationID = convID
				id, err := stores.Conversations.InsertMessageSystem(ctx, org, &msg)
				if err != nil {
					t.Fatalf("insert message: %v", err)
				}
				return id
			},
			SetSeq: func(t *testing.T, msgID int64, seq float64) {
				t.Helper()
				if _, err := conn.Exec(`UPDATE messages SET seq = ? WHERE id = ?`, seq, msgID); err != nil {
					t.Fatalf("set seq: %v", err)
				}
			},
			SetParentConversation: func(t *testing.T, convID, parentConvID string) {
				t.Helper()
				if _, err := conn.Exec(
					`UPDATE conversations SET parent_conversation_id = ? WHERE id = ?`, parentConvID, convID,
				); err != nil {
					t.Fatalf("set parent_conversation_id: %v", err)
				}
			},
			CollapseClaimTimestamps: func(t *testing.T, convID string) {
				t.Helper()
				if _, err := conn.Exec(`
					UPDATE claims
					SET claimed_at = '2026-01-01 00:00:00',
					    released_at = CASE WHEN released_at IS NULL THEN NULL ELSE '2026-01-01 00:00:00' END
					WHERE conversation_id = ?`, convID); err != nil {
					t.Fatalf("collapse claim timestamps: %v", err)
				}
			},
			DisplayStatus: func(t *testing.T, convID string) string {
				t.Helper()
				got, err := stores.Conversations.Get(ctx, org, convID)
				if err != nil || got == nil {
					t.Fatalf("Conversations.Get = (%v, %v)", got, err)
				}
				return got.Status
			},
		}
	})
}

// TestConversationQueueStore_SQLite_ReconcileOrphanedConversations runs the shared boot-reconcile
// conformance against the SQLite impl. Each factory call opens a fresh
// in-memory DB, so the suite's exact healed counts are this subtest's alone.
func TestConversationQueueStore_SQLite_ReconcileOrphanedConversations(t *testing.T) {
	dbtest.RunReconcileOrphanedConversationsConformance(t, func(t *testing.T) (db.ConversationQueueStore, dbtest.ReconcileOrphanSeeder) {
		t.Helper()
		conn := openSQLiteForTest(t)
		stores := sqlitestore.New(conn)
		ctx := context.Background()
		org := runmode.LocalDefaultOrgID

		insertPromptForBlueprintTest(t, conn, domain.Prompt{ID: "rqrc-p0", Name: "Step 0", Body: "b", Source: "user"})
		insertBlueprintForTest(t, conn, "rqrc-bp", "RQ Reconcile Blueprint")
		if _, err := stores.Blueprints.ReplaceSteps(ctx, org, "rqrc-bp", []string{"rqrc-p0"}, nil); err != nil {
			t.Fatalf("ReplaceSteps: %v", err)
		}

		// Both seeders below write their rows directly. This suite's whole
		// subject is the shapes the invariants forbid — a childless run, a
		// mid-flight child under a terminal parent, a pointer naming a step
		// nothing minted — and the doors that mint a step are the ones that
		// make those shapes unreachable, so they cannot stage them.
		//
		// One task per run either way: blueprint_runs_one_active_run_per_task
		// refuses a second 'running' row on a task, and the suite stages
		// several at once.
		runTask := map[string]string{}
		nextStep := map[string]int{}
		seed := dbtest.ReconcileOrphanSeeder{
			BlueprintRun: func(t *testing.T, age time.Duration) string {
				t.Helper()
				task := seedEntityEventTask(t, conn, fmt.Sprintf("rq-recon-%d", len(runTask)))
				created, err := stores.Blueprints.CreateRun(ctx, org, domain.BlueprintRun{
					ID: uuid.New().String(), BlueprintID: "rqrc-bp", TaskID: task.ID,
					TriggerType: domain.BlueprintTriggerManual, Status: domain.BlueprintRunStatusRunning,
					WorktreePath: "/tmp/wt-rqrc",
				})
				if err != nil {
					t.Fatalf("CreateRun: %v", err)
				}
				brID := created.ID
				runTask[brID] = task.ID
				if age > 0 {
					// SQLite's own clock, in the CURRENT_TIMESTAMP shape the
					// insert paths write, so the backdate can't smuggle in a
					// timestamp format production never produces.
					if _, err := conn.Exec(`UPDATE blueprint_runs SET started_at = datetime('now', ?) WHERE id = ?`,
						fmt.Sprintf("-%d seconds", int(age.Seconds())), brID); err != nil {
						t.Fatalf("backdate started_at: %v", err)
					}
				}
				return brID
			},
			StageChild: func(t *testing.T, brID string) string {
				t.Helper()
				idx := nextStep[brID]
				nextStep[brID]++
				convID := uuid.New().String()
				insertConversationForTest(t, conn, domain.Conversation{
					ID: convID, TaskID: runTask[brID], PromptID: "rqrc-p0", Model: "m",
					TriggerType: "manual", BlueprintRunID: brID, BlueprintStepIndex: &idx,
				})
				return convID
			},
			ForceBlueprintStatus: func(t *testing.T, brID, status, abortReason string) {
				t.Helper()
				if _, err := conn.Exec(`UPDATE blueprint_runs SET status = ?, abort_reason = NULLIF(?, '') WHERE id = ?`,
					status, abortReason, brID); err != nil {
					t.Fatalf("force blueprint_run status %q: %v", status, err)
				}
			},
			SetCurrentStep: func(t *testing.T, brID string, stepIndex int) {
				t.Helper()
				if _, err := conn.Exec(`UPDATE blueprint_runs SET current_step_index = ? WHERE id = ?`, stepIndex, brID); err != nil {
					t.Fatalf("set current_step_index: %v", err)
				}
			},
			BlueprintRunState: func(t *testing.T, brID string) (string, string, bool) {
				t.Helper()
				var status string
				var reason sql.NullString
				var completedAt any
				if err := conn.QueryRow(`SELECT status, abort_reason, completed_at FROM blueprint_runs WHERE id = ?`, brID).
					Scan(&status, &reason, &completedAt); err != nil {
					t.Fatalf("read blueprint_run %s: %v", brID, err)
				}
				return status, reason.String, completedAt != nil
			},
			ConversationStatus: func(t *testing.T, convID string) string {
				t.Helper()
				var status sql.NullString
				if err := conn.QueryRow(`SELECT status FROM conversations WHERE id = ?`, convID).Scan(&status); err != nil {
					t.Fatalf("read conversation %s status: %v", convID, err)
				}
				return status.String
			},
		}
		return stores.ConversationQueue, seed
	})
}

// TestConversationQueueStore_SQLite_ReturnedRow runs the returned-row
// conformance suite against the SQLite impl.
func TestConversationQueueStore_SQLite_ReturnedRow(t *testing.T) {
	dbtest.RunConversationQueueReturnedRowConformance(t, func(t *testing.T) (db.ConversationQueueStore, db.ConversationStore, string, dbtest.ConversationQueueReturnedRowScaffold) {
		t.Helper()
		conn := openSQLiteForTest(t)
		stores := sqlitestore.New(conn)
		org := runmode.LocalDefaultOrgID

		next := 0
		scaffold := func(t *testing.T) string {
			t.Helper()
			next++
			return stageSqliteStep(t, conn, stores, fmt.Sprintf("cqrr-%d", next)).ID
		}
		return stores.ConversationQueue, stores.Conversations, org, scaffold
	})
}
