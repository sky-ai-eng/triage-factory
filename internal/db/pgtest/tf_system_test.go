package pgtest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/wsbackplane"
	"github.com/sky-ai-eng/triage-factory/pkg/websocket"
)

// TestTfSystem_RoleShape pins the pg_roles attributes tf_system must
// carry: BYPASSRLS (it reproduces today's admin-pool RLS-bypass
// semantics) and NOINHERIT (mirrors tf_app — no ambient privilege via
// role membership). rolcanlogin is NOT asserted false here: the baseline
// ships tf_system NOLOGIN, but boot() above ALTERs it to LOGIN so
// SystemDB can connect, the same test-only accommodation authPassword
// makes for tf_app/authenticator.
func TestTfSystem_RoleShape(t *testing.T) {
	h := Shared(t)

	var inherit, bypassRLS bool
	err := h.AdminDB.QueryRow(`
		SELECT rolinherit, rolbypassrls
		  FROM pg_roles WHERE rolname = 'tf_system'
	`).Scan(&inherit, &bypassRLS)
	if err != nil {
		t.Fatalf("query tf_system: %v", err)
	}
	if inherit {
		t.Errorf("tf_system.rolinherit = true, want false (NOINHERIT)")
	}
	if !bypassRLS {
		t.Errorf("tf_system.rolbypassrls = false, want true (BYPASSRLS)")
	}
}

// TestTfSystem_DDLDenied pins that tf_system's migration path is
// SELECT-only against goose_db_version. Any DDL — even a throwaway table
// in the test's own transaction — must be rejected.
func TestTfSystem_DDLDenied(t *testing.T) {
	h := Shared(t)

	_, err := h.SystemDB.Exec(`CREATE TABLE tf_system_ddl_probe (id int)`)
	if err == nil {
		h.AdminDB.Exec(`DROP TABLE IF EXISTS tf_system_ddl_probe`)
		t.Fatalf("tf_system CREATE TABLE succeeded, want permission denied")
	}
	assertPgCode(t, err, "42501", "tf_system CREATE TABLE")
}

// TestTfSystem_OffSurfaceReadDenied pins that tables entirely outside the
// executor's enumerated surface must be unreachable, even for a plain
// SELECT that would return zero rows: sso_connections (SSO config is a
// control-plane-only concern) and org_secrets (an executor never holds
// TF_SECRET_ENCRYPTION_KEY at all as of TFAC-614 — its per-run credential
// material arrives pre-resolved via sealed run_credentials bundles
// instead, so the org_secrets grant this ticket originally shipped is
// dead weight and was removed; this pins that it stays removed).
func TestTfSystem_OffSurfaceReadDenied(t *testing.T) {
	h := Shared(t)

	for _, table := range []string{"sso_connections", "org_secrets"} {
		var n int
		err := h.SystemDB.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n)
		if err == nil {
			t.Errorf("tf_system SELECT %s succeeded, want permission denied", table)
			continue
		}
		assertPgCode(t, err, "42501", "tf_system SELECT "+table)
	}
}

// TestTfSystem_CrossOrgSystemReadSucceeds pins that BYPASSRLS is REQUIRED
// semantics for a granted table, not accidental — a *System read for one
// org must succeed when called with a different org's context bound
// (there is no session/claims concept on the admin pool at all; the
// caller-supplied orgID is trusted, exactly like today's supabase_admin
// behavior). Uses Tasks.GetSystem as the vehicle — any granted table with
// an org-scoped *System(ctx, orgID, id) read would do; tasks is simplest
// to seed.
func TestTfSystem_CrossOrgSystemReadSucceeds(t *testing.T) {
	h := Shared(t)
	h.Reset(t)

	orgA, userA, _ := SeedOrgWithUser(t, h, "cross-org-a")
	orgB, _, _ := SeedOrgWithUser(t, h, "cross-org-b")

	entityA := seedEntity(t, h, orgA, "github", "octo/cross-org#1")
	taskA := seedTask(t, h, orgA, userA, entityA, "github:pr:opened")

	stores := pgstore.New(h.SystemDB, h.AppDB, SecretKey)
	ctx := context.Background()

	// Read orgA's task while correctly scoped to orgA — BYPASSRLS + a
	// trusted orgID argument means this must resolve regardless of the
	// admin pool having no session claims at all (there is nothing to
	// "act as orgB" here; the point is that no claims context is needed).
	task, err := stores.Tasks.GetSystem(ctx, orgA, taskA)
	if err != nil {
		t.Fatalf("GetSystem(orgA, taskA) as tf_system: %v", err)
	}
	if task == nil {
		t.Fatalf("GetSystem(orgA, taskA) returned nil, want the seeded task")
	}

	// And the same id under orgB's id resolves to nothing — proves the
	// read is scoped by the argument, not a blanket cross-org leak.
	taskUnderB, err := stores.Tasks.GetSystem(ctx, orgB, taskA)
	if err != nil {
		t.Fatalf("GetSystem(orgB, taskA) as tf_system: %v", err)
	}
	if taskUnderB != nil {
		t.Errorf("GetSystem(orgB, taskA) = %+v, want nil (no such row under orgB)", taskUnderB)
	}
}

// TestTfSystem_ExecutorSurfaceConformance is the authoritative source of
// truth for tf_system's grant list — more authoritative than the pg
// baseline's own comments, which can drift. It points the postgres
// store's admin half at SystemDB (tf_system) and exercises the
// enumerated executor surface — claim, heartbeat, messages/artifacts/
// worktrees/memory/external-actions, cross-pod signals, the
// blueprint-run reactor, and the schema assert. Any SQLSTATE 42501 here
// means a missing grant: add it to the tf_system section of the pg
// baseline while that file is still pre-release and freely editable: a
// new migration granting the table once it is not.
func TestTfSystem_ExecutorSurfaceConformance(t *testing.T) {
	h := Shared(t)
	h.Reset(t)
	ctx := context.Background()

	orgID, userID, teamID := SeedOrgWithUser(t, h, "executor-org")
	entityID := seedEntity(t, h, orgID, "github", "octo/repo#1")
	taskID := seedTask(t, h, orgID, userID, entityID, "github:pr:opened")
	promptID := seedPrompt(t, h, orgID, userID, "step-prompt")
	blueprintRunID := seedBlueprintRun(t, h, orgID, userID, taskID)

	var blueprintID string
	if err := h.AdminDB.QueryRow(`SELECT blueprint_id FROM blueprint_runs WHERE id = $1`, blueprintRunID).Scan(&blueprintID); err != nil {
		t.Fatalf("read seeded blueprint_id: %v", err)
	}

	// Store bundle whose "admin" half is tf_system — exactly the shape a
	// TF_ROLE=executor process wires in internal/app/stores.go.
	stores := pgstore.New(h.SystemDB, h.AppDB, SecretKey)

	const executorID = "conformance-executor"

	t.Run("run_queue_enqueue_and_claim", func(t *testing.T) {
		runID := newUUID(t, h)
		if err := stores.RunQueue.EnqueueRun(ctx, orgID, domain.AgentRun{
			ID:             runID,
			TaskID:         taskID,
			PromptID:       promptID,
			Model:          "test-model",
			TriggerType:    "manual",
			CreatorUserID:  userID,
			BlueprintRunID: blueprintRunID,
		}); err != nil {
			t.Fatalf("RunQueue.EnqueueRun (dispatcher's own step enqueue): %v", err)
		}

		claimed, err := stores.RunQueue.ClaimNextRun(ctx, executorID, 1, db.ClaimPlacement{})
		if err != nil {
			t.Fatalf("RunQueue.ClaimNextRun: %v", err)
		}
		if claimed == nil || claimed.ID != runID {
			t.Fatalf("ClaimNextRun = %+v, want the just-enqueued run", claimed)
		}

		if err := stores.AgentRuns.SetExecutorSystem(ctx, orgID, runID, executorID, 1); err != nil {
			t.Errorf("AgentRuns.SetExecutorSystem: %v", err)
		}
		if _, err := stores.AgentRuns.MarkOpenSystem(ctx, orgID, runID); err != nil {
			t.Errorf("AgentRuns.MarkOpenSystem: %v", err)
		}
		if _, err := stores.AgentRuns.InsertMessageSystem(ctx, orgID, &domain.AgentMessage{
			RunID: runID, Role: "assistant", Content: "hello", Subtype: "text",
		}); err != nil {
			t.Errorf("AgentRuns.InsertMessageSystem: %v", err)
		}
		if _, err := stores.AgentRuns.GetSystem(ctx, orgID, runID); err != nil {
			t.Errorf("AgentRuns.GetSystem: %v", err)
		}
		if err := stores.AgentRuns.SetWorktreePathSystem(ctx, orgID, runID, "/tmp/conformance-wt"); err != nil {
			t.Errorf("AgentRuns.SetWorktreePathSystem: %v", err)
		}
		if err := stores.AgentRuns.CompleteSystem(ctx, orgID, runID, "completed", 0.01, 1000, 3,
			"", "did the thing", "completed", "", ""); err != nil {
			t.Errorf("AgentRuns.CompleteSystem: %v", err)
		}

		if _, err := stores.RunQueue.ResetProcessingRuns(ctx, executorID, 1); err != nil {
			t.Errorf("RunQueue.ResetProcessingRuns: %v", err)
		}
	})

	t.Run("run_worktrees", func(t *testing.T) {
		runID := seedQueuedRun(t, h, stores, ctx, orgID, taskID, promptID, blueprintRunID)
		if inserted, _, err := stores.RunWorktrees.InsertSystem(ctx, orgID, domain.RunWorktree{
			RunID: runID, RepoID: "octo/repo", Path: "/tmp/conformance-wt", Ref: "main",
		}); err != nil || !inserted {
			t.Errorf("RunWorktrees.InsertSystem: inserted=%v err=%v", inserted, err)
		}
		if _, err := stores.RunWorktrees.ListSystem(ctx, orgID, runID); err != nil {
			t.Errorf("RunWorktrees.ListSystem: %v", err)
		}
		if err := stores.RunWorktrees.DeleteByRepoRefSystem(ctx, orgID, runID, "octo/repo", "main"); err != nil {
			t.Errorf("RunWorktrees.DeleteByRepoRefSystem: %v", err)
		}
	})

	t.Run("artifacts", func(t *testing.T) {
		runID := seedQueuedRun(t, h, stores, ctx, orgID, taskID, promptID, blueprintRunID)
		art, err := stores.Artifacts.UpsertSystem(ctx, orgID, domain.Artifact{
			RunID: runID, TeamID: teamID, Provider: "github", Kind: "pull_request",
			Target: "octo/repo#1", State: "open", DedupKey: "conformance-" + runID,
		})
		if err != nil {
			t.Fatalf("Artifacts.UpsertSystem: %v", err)
		}
		if _, err := stores.Artifacts.ListByRunSystem(ctx, orgID, runID); err != nil {
			t.Errorf("Artifacts.ListByRunSystem: %v", err)
		}
		_ = art
	})

	t.Run("run_memory", func(t *testing.T) {
		runID := seedQueuedRun(t, h, stores, ctx, orgID, taskID, promptID, blueprintRunID)
		if err := stores.TaskMemory.UpsertAgentMemorySystem(ctx, orgID, runID, entityID, blueprintRunID, "agent narrative"); err != nil {
			t.Errorf("TaskMemory.UpsertAgentMemorySystem: %v", err)
		}
		if err := stores.TaskMemory.RecordEntityTouchSystem(ctx, orgID, runID, entityID, domain.MemoryRolePrimary); err != nil {
			t.Errorf("TaskMemory.RecordEntityTouchSystem: %v", err)
		}
		if _, err := stores.TaskMemory.GetMemoriesForEntitySystem(ctx, orgID, entityID, teamID); err != nil {
			t.Errorf("TaskMemory.GetMemoriesForEntitySystem: %v", err)
		}
		if _, err := stores.TaskMemory.CountMemoriesForEntitySystem(ctx, orgID, entityID, teamID); err != nil {
			t.Errorf("TaskMemory.CountMemoriesForEntitySystem: %v", err)
		}
	})

	t.Run("external_actions", func(t *testing.T) {
		runID := seedQueuedRun(t, h, stores, ctx, orgID, taskID, promptID, blueprintRunID)
		if err := stores.ExternalActions.RecordSystem(ctx, orgID, domain.ExternalAction{
			TeamID: teamID, Provider: "jira", Action: "status_transition", Target: "SKY-1",
			RunID: runID, Credential: "org", ToState: "In Progress",
		}); err != nil {
			t.Errorf("ExternalActions.RecordSystem: %v", err)
		}
	})

	t.Run("entities_touched_resolution", func(t *testing.T) {
		ent, _, err := stores.Entities.FindOrCreateSystem(ctx, orgID, "github", "octo/repo#999", "pr", "conformance pr", "https://example.test/pr")
		if err != nil {
			t.Fatalf("Entities.FindOrCreateSystem: %v", err)
		}
		if _, err := stores.Entities.GetSystem(ctx, orgID, ent.ID); err != nil {
			t.Errorf("Entities.GetSystem: %v", err)
		}
		if _, err := stores.Entities.UpdateSnapshotCASSystem(ctx, orgID, ent.ID, `{"k":"v"}`, ent.PollSeq); err != nil {
			t.Errorf("Entities.UpdateSnapshotCASSystem: %v", err)
		}
	})

	t.Run("task_events_and_status", func(t *testing.T) {
		evtID := seedEventForEntity(t, h, orgID, entityID, "github:pr:opened")
		if err := stores.Tasks.RecordEventSystem(ctx, orgID, taskID, evtID, "injected"); err != nil {
			t.Errorf("Tasks.RecordEventSystem: %v", err)
		}
		if err := stores.Tasks.SetStatusSystem(ctx, orgID, taskID, "in_review"); err != nil {
			t.Errorf("Tasks.SetStatusSystem: %v", err)
		}
		if err := stores.Tasks.CloseSystem(ctx, orgID, taskID, "run_completed", ""); err != nil {
			t.Errorf("Tasks.CloseSystem: %v", err)
		}
		if _, err := stores.Tasks.GetSystem(ctx, orgID, taskID); err != nil {
			t.Errorf("Tasks.GetSystem: %v", err)
		}
	})

	t.Run("staged_injections", func(t *testing.T) {
		runID := seedQueuedRun(t, h, stores, ctx, orgID, taskID, promptID, blueprintRunID)
		if err := stores.StagedInjections.AppendSystem(ctx, orgID, &domain.StagedInjection{
			RunID: runID, Producer: domain.StagedInjectionProducerPRNewCommits, Body: "new commits landed",
		}); err != nil {
			t.Fatalf("StagedInjections.AppendSystem: %v", err)
		}
		if _, err := stores.StagedInjections.FlushPendingSystem(ctx, orgID, runID); err != nil {
			t.Errorf("StagedInjections.FlushPendingSystem: %v", err)
		}
	})

	t.Run("pending_firings", func(t *testing.T) {
		evtID := seedEventForEntity(t, h, orgID, entityID, "github:pr:opened")
		triggerID := seedTrigger(t, h, orgID, userID, teamID, blueprintID)
		if _, err := stores.PendingFirings.Enqueue(ctx, orgID, userID, entityID, taskID, triggerID, evtID); err != nil {
			t.Errorf("PendingFirings.Enqueue (inject signal gone-compensation): %v", err)
		}
	})

	t.Run("run_signals", func(t *testing.T) {
		runID := seedQueuedRun(t, h, stores, ctx, orgID, taskID, promptID, blueprintRunID)
		var sigID int64
		if err := h.AdminDB.QueryRowContext(ctx, `
			INSERT INTO run_signals (org_id, run_id, kind, target) VALUES ($1, $2, 'interrupt', $3) RETURNING id
		`, orgID, runID, executorID).Scan(&sigID); err != nil {
			t.Fatalf("seed run_signals: %v", err)
		}
		sigs, err := stores.RunSignals.ListUnackedForTarget(ctx, executorID)
		if err != nil {
			t.Fatalf("RunSignals.ListUnackedForTarget: %v", err)
		}
		if len(sigs) == 0 {
			t.Fatalf("ListUnackedForTarget returned no rows, want the seeded signal")
		}
		if err := stores.RunSignals.Ack(ctx, sigID, "ok"); err != nil {
			t.Errorf("RunSignals.Ack: %v", err)
		}
	})

	t.Run("run_pending_input", func(t *testing.T) {
		runID := seedQueuedRun(t, h, stores, ctx, orgID, taskID, promptID, blueprintRunID)
		MustExec(t, h.AdminDB, `
			INSERT INTO run_pending_input (run_id, org_id, user_id, message) VALUES ($1, $2, $3, 'resume this')
		`, runID, orgID, userID)
		msg, _, ok, err := stores.RunPendingInput.Peek(ctx, orgID, runID)
		if err != nil || !ok || msg == "" {
			t.Fatalf("RunPendingInput.Peek: msg=%q ok=%v err=%v", msg, ok, err)
		}
		if _, _, ok, err := stores.RunPendingInput.Consume(ctx, orgID, runID); err != nil || !ok {
			t.Errorf("RunPendingInput.Consume: ok=%v err=%v", ok, err)
		}
	})

	t.Run("instances", func(t *testing.T) {
		epoch, err := stores.Instances.Register(ctx, executorID, "executor", "test-build", "test-pubkey")
		if err != nil {
			t.Fatalf("Instances.Register: %v", err)
		}
		if _, _, err := stores.Instances.Heartbeat(ctx, executorID, epoch, domain.InstanceHeartbeat{}); err != nil {
			t.Errorf("Instances.Heartbeat: %v", err)
		}
		if _, err := stores.Instances.Get(ctx, executorID); err != nil {
			t.Errorf("Instances.Get: %v", err)
		}
	})

	t.Run("blueprint_run_reactor", func(t *testing.T) {
		if _, err := stores.Blueprints.GetSystem(ctx, orgID, blueprintID); err != nil {
			t.Errorf("Blueprints.GetSystem: %v", err)
		}
		if _, err := stores.Blueprints.ListStepsSystem(ctx, orgID, blueprintID); err != nil {
			t.Errorf("Blueprints.ListStepsSystem: %v", err)
		}
		if _, err := stores.Blueprints.GetRunSystem(ctx, orgID, blueprintRunID); err != nil {
			t.Errorf("Blueprints.GetRunSystem: %v", err)
		}
		if _, err := stores.Blueprints.ActiveRunForTaskSystem(ctx, orgID, taskID); err != nil {
			t.Errorf("Blueprints.ActiveRunForTaskSystem: %v", err)
		}
		if _, err := stores.Blueprints.RunsForBlueprintSystem(ctx, orgID, blueprintRunID); err != nil {
			t.Errorf("Blueprints.RunsForBlueprintSystem: %v", err)
		}
		if _, err := stores.Blueprints.ActiveStepRunIDsSystem(ctx, orgID, blueprintRunID); err != nil {
			t.Errorf("Blueprints.ActiveStepRunIDsSystem: %v", err)
		}
		if err := stores.Blueprints.SetRunCurrentStepSystem(ctx, orgID, blueprintRunID, 1); err != nil {
			t.Errorf("Blueprints.SetRunCurrentStepSystem: %v", err)
		}
		if err := stores.Blueprints.SetRunWorktreePathSystem(ctx, orgID, blueprintRunID, "/tmp/conformance-shared-wt"); err != nil {
			t.Errorf("Blueprints.SetRunWorktreePathSystem: %v", err)
		}
		if _, err := stores.Blueprints.RequestRunCancelSystem(ctx, orgID, blueprintRunID); err != nil {
			t.Errorf("Blueprints.RequestRunCancelSystem: %v", err)
		}
		if _, err := stores.Blueprints.MarkRunStatusSystem(ctx, orgID, blueprintRunID, domain.BlueprintRunStatusCompleted, "", nil); err != nil {
			t.Errorf("Blueprints.MarkRunStatusSystem: %v", err)
		}
	})

	t.Run("prompts_and_agents", func(t *testing.T) {
		if _, err := stores.Prompts.GetSystem(ctx, orgID, promptID); err != nil {
			t.Errorf("Prompts.GetSystem: %v", err)
		}
		if err := stores.Prompts.IncrementUsageSystem(ctx, orgID, promptID); err != nil {
			t.Errorf("Prompts.IncrementUsageSystem: %v", err)
		}
		if _, err := stores.Agents.GetForOrgSystem(ctx, orgID); err != nil {
			t.Errorf("Agents.GetForOrgSystem: %v", err)
		}
	})

	t.Run("reference_reads", func(t *testing.T) {
		if _, err := stores.Orgs.GetSettingsSystem(ctx, orgID); err != nil {
			t.Errorf("Orgs.GetSettingsSystem: %v", err)
		}
		if _, err := stores.Teams.GetSettingsSystem(ctx, teamID); err != nil {
			t.Errorf("Teams.GetSettingsSystem: %v", err)
		}
		if _, err := stores.Teams.GetSystem(ctx, orgID, teamID); err != nil {
			t.Errorf("Teams.GetSystem: %v", err)
		}
		if _, err := stores.Users.GetGitHubLoginSystem(ctx, userID, "github.com"); err != nil {
			t.Errorf("Users.GetGitHubLoginSystem: %v", err)
		}
		if _, err := stores.TeamGitHubRepos.TracksRepoSystem(ctx, teamID, "octo", "repo"); err != nil {
			t.Errorf("TeamGitHubRepos.TracksRepoSystem: %v", err)
		}
		if _, err := stores.GitHubApps.GetForOrgSystem(ctx, orgID); err != nil {
			t.Errorf("GitHubApps.GetForOrgSystem: %v", err)
		}
		if _, err := stores.GitHubApps.ListInstallationsForOrgSystem(ctx, orgID); err != nil {
			t.Errorf("GitHubApps.ListInstallationsForOrgSystem: %v", err)
		}
		if _, err := stores.JiraStatusRules.ListForTeamSystem(ctx, teamID); err != nil {
			t.Errorf("JiraStatusRules.ListForTeamSystem: %v", err)
		}
		if _, err := stores.Spend.SpendByCategorySystem(ctx, orgID, time.Now().Add(-24*time.Hour), time.Time{}); err != nil {
			t.Errorf("Spend.SpendByCategorySystem (llm_spend security_invoker view): %v", err)
		}
		evtID := seedEventForEntity(t, h, orgID, entityID, "github:pr:closed")
		if _, err := stores.Events.GetMetadataSystem(ctx, orgID, evtID); err != nil {
			t.Errorf("Events.GetMetadataSystem: %v", err)
		}
	})

	t.Run("repo_profiles", func(t *testing.T) {
		repoID := seedRepoProfile(t, h, orgID)
		if _, err := stores.Repos.GetSystem(ctx, orgID, repoID); err != nil {
			t.Errorf("Repos.GetSystem: %v", err)
		}
		if err := stores.Repos.UpdateCloneStatusSystem(ctx, orgID, "octo", "conformance-repo", "ok", "", ""); err != nil {
			t.Errorf("Repos.UpdateCloneStatusSystem: %v", err)
		}
	})

	t.Run("run_credentials", func(t *testing.T) {
		runID := seedQueuedRun(t, h, stores, ctx, orgID, taskID, promptID, blueprintRunID)
		// The brain seals + writes bundles on its own pool (supabase_admin,
		// never tf_system); seed directly via AdminDB so this subtest
		// isolates the executor's read side — RunCredentials.Get, which the
		// awaiting-credentials wait polls.
		MustExec(t, h.AdminDB, `
			INSERT INTO run_credentials (run_id, org_id, executor_id, boot_epoch, sealed)
			VALUES ($1, $2, $3, 1, $4)
		`, runID, orgID, executorID, []byte("sealed-bytes"))
		_, _, _, ok, err := stores.RunCredentials.Get(ctx, orgID, runID)
		if err != nil {
			t.Fatalf("RunCredentials.Get: %v", err)
		}
		if !ok {
			t.Errorf("RunCredentials.Get: ok=false, want the seeded bundle")
		}
	})

	t.Run("curator_homing", func(t *testing.T) {
		// Seed a project + two turns homed to this executor: a 'queued' turn
		// (the claim loop's ListQueuedRequestsForHomeSystem must see it) and a
		// 'running' turn that already streamed a token-bearing message (the
		// ownership-scoped sweep's roll-up must read curator_messages).
		projectID := newUUID(t, h)
		MustExec(t, h.AdminDB, `
			INSERT INTO projects (id, org_id, creator_user_id, team_id, name, created_at, updated_at)
			VALUES ($1, $2, $3, $4, 'homing-conformance', now(), now())
		`, projectID, orgID, userID, teamID)

		queuedID := newUUID(t, h)
		runningID := newUUID(t, h)
		MustExec(t, h.AdminDB, `
			INSERT INTO curator_requests (id, org_id, project_id, creator_user_id, team_id, status, user_input, home_instance_id)
			VALUES ($1, $2, $3, $4, $5, 'queued', 'q', $6),
			       ($7, $2, $3, $4, $5, 'running', 'r', $6)
		`, queuedID, orgID, projectID, userID, teamID, executorID, runningID)
		MustExec(t, h.AdminDB, `
			INSERT INTO curator_messages (org_id, creator_user_id, request_id, role, input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens)
			VALUES ($1, $2, $3, 'assistant', 11, 22, 33, 44)
		`, orgID, userID, runningID)

		// List (SELECT curator_requests) — the claim loop's homed scan.
		homed, err := stores.Curator.ListQueuedRequestsForHomeSystem(ctx, executorID)
		if err != nil {
			t.Fatalf("Curator.ListQueuedRequestsForHomeSystem: %v", err)
		}
		if len(homed) != 1 || homed[0].ID != queuedID {
			t.Fatalf("ListQueuedRequestsForHomeSystem = %+v, want the one queued turn", homed)
		}

		// Sweep (UPDATE curator_requests + SELECT curator_messages for the
		// token roll-up) — a missing curator_messages grant fails 42501 here.
		n, err := stores.Curator.CancelStrandedRequestsForHomeSystem(ctx, executorID, "process restarted")
		if err != nil {
			t.Fatalf("Curator.CancelStrandedRequestsForHomeSystem: %v", err)
		}
		if n != 2 {
			t.Fatalf("CancelStrandedRequestsForHomeSystem flipped %d turns, want 2", n)
		}
		// The running turn's streamed tokens rolled up (not stranded at 0) —
		// the whole point of matching every other terminal write (TFAC-473).
		var in, out, cr, cc int
		if err := h.AdminDB.QueryRowContext(ctx,
			`SELECT input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens FROM curator_requests WHERE id = $1`, runningID,
		).Scan(&in, &out, &cr, &cc); err != nil {
			t.Fatalf("read back running turn: %v", err)
		}
		if in != 11 || out != 22 || cr != 33 || cc != 44 {
			t.Errorf("token roll-up = (%d,%d,%d,%d), want (11,22,33,44) — a stranded turn must report its streamed spend", in, out, cr, cc)
		}
	})

	t.Run("ws_backplane", func(t *testing.T) {
		b := wsbackplane.New(h.SystemDB, "", executorID, websocket.NewHub())
		if present := b.PresentFor(ctx, orgID, "some-run-id"); present {
			t.Errorf("PresentFor = true on an empty hub + no presence rows, want false")
		}
		_ = b

		// ws_outbox: publish + read-back + TTL reap, matching the exact
		// statements wsbackplane.publishViaOutbox / fetchOutbox / the outbox
		// reaper run (those specific methods are package-private; the SQL
		// text here is copied verbatim from internal/wsbackplane/publish.go
		// and reaper.go so this is still a direct grant check, not a
		// behavioral reimplementation).
		var outboxID int64
		if err := h.SystemDB.QueryRowContext(ctx,
			`INSERT INTO ws_outbox (payload) VALUES ($1) RETURNING id`, []byte(`{"type":"probe"}`),
		).Scan(&outboxID); err != nil {
			t.Fatalf("tf_system INSERT ws_outbox: %v", err)
		}
		var raw []byte
		if err := h.SystemDB.QueryRowContext(ctx,
			`SELECT payload FROM ws_outbox WHERE id = $1`, outboxID,
		).Scan(&raw); err != nil {
			t.Errorf("tf_system SELECT ws_outbox: %v", err)
		}
		if _, err := h.SystemDB.ExecContext(ctx,
			`DELETE FROM ws_outbox WHERE created_at < now() - make_interval(secs => $1)`, 0,
		); err != nil {
			t.Errorf("tf_system DELETE ws_outbox (TTL reaper): %v", err)
		}

		// ws_presence: seed a row via AdminDB (executors never INSERT
		// presence), then confirm tf_system can read it via PresentFor and
		// the presence reaper's DELETE.
		MustExec(t, h.AdminDB, `
			INSERT INTO ws_presence (user_id, conn_id, org_id, instance_id, viewing, visible, last_seen)
			VALUES ($1, 'conn-1', $2, 'control-pod-1', $3, true, now())
		`, userID, orgID, "run:some-run-id")
		if present := b.PresentFor(ctx, orgID, "some-run-id"); !present {
			t.Errorf("PresentFor = false after seeding a live ws_presence row, want true")
		}
		if _, err := h.SystemDB.ExecContext(ctx,
			`DELETE FROM ws_presence WHERE last_seen < now() - make_interval(secs => $1)`, 0,
		); err != nil {
			t.Errorf("tf_system DELETE ws_presence (presence reaper): %v", err)
		}
	})

	t.Run("schema_assert", func(t *testing.T) {
		runmode.SetRoleForTest(t, runmode.RoleExecutor)
		if err := db.Migrate(h.SystemDB, "postgres"); err != nil {
			t.Errorf("db.Migrate as tf_system under TF_ROLE=executor (schema-compatibility assert): %v", err)
		}
	})
}

// seedQueuedRun mints a fresh runs row via the tf_system-backed
// RunQueue.EnqueueRun (the same INSERT the dispatcher's reactor performs
// for every subsequent blueprint step), so each subtest below gets its
// own run without re-running the claim subtest's side effects.
func seedQueuedRun(t *testing.T, h *Harness, stores db.Stores, ctx context.Context, orgID, taskID, promptID, blueprintRunID string) string {
	t.Helper()
	runID := newUUID(t, h)
	if err := stores.RunQueue.EnqueueRun(ctx, orgID, domain.AgentRun{
		ID: runID, TaskID: taskID, PromptID: promptID, Model: "test-model",
		TriggerType: "manual", CreatorUserID: "", BlueprintRunID: blueprintRunID,
	}); err != nil {
		t.Fatalf("seedQueuedRun EnqueueRun: %v", err)
	}
	return runID
}

func newUUID(t *testing.T, h *Harness) string {
	t.Helper()
	var id string
	if err := h.AdminDB.QueryRow(`SELECT gen_random_uuid()`).Scan(&id); err != nil {
		t.Fatalf("gen uuid: %v", err)
	}
	return id
}

func seedEventForEntity(t *testing.T, h *Harness, orgID, entityID, eventType string) string {
	t.Helper()
	var id string
	if err := h.AdminDB.QueryRow(`
		INSERT INTO events (org_id, entity_id, event_type) VALUES ($1, $2, $3) RETURNING id
	`, orgID, entityID, eventType).Scan(&id); err != nil {
		t.Fatalf("seed event: %v", err)
	}
	return id
}

// seedTrigger inserts a minimal trigger-kind event_handlers row so
// pending_firings' trigger_id FK resolves.
func seedTrigger(t *testing.T, h *Harness, orgID, creatorID, teamID, blueprintID string) string {
	t.Helper()
	var id string
	if err := h.AdminDB.QueryRow(`
		INSERT INTO event_handlers (org_id, creator_user_id, team_id, kind, blueprint_id, event_type, breaker_threshold, min_autonomy_suitability)
		VALUES ($1, $2, $3, 'trigger', $4, 'github:pr:opened', 4, 0.0) RETURNING id
	`, orgID, creatorID, teamID, blueprintID).Scan(&id); err != nil {
		t.Fatalf("seed trigger: %v", err)
	}
	return id
}

func seedRepoProfile(t *testing.T, h *Harness, orgID string) string {
	t.Helper()
	MustExec(t, h.AdminDB, `
		INSERT INTO repo_profiles (org_id, owner, repo)
		VALUES ($1, 'octo', 'conformance-repo')
	`, orgID)
	return "octo/conformance-repo"
}

// assertPgCode is defined in baseline_test.go (same package); referenced
// here across files. errors/pgconn imports below keep this file
// self-contained if that helper ever moves.
var _ = errors.As
var _ = (*pgconn.PgError)(nil)
