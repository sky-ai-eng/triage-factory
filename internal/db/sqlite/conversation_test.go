package sqlite_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "modernc.org/sqlite"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestConversationStore_SQLite runs the shared conformance suite against
// the SQLite ConversationStore impl. Each subtest gets a fresh in-memory
// DB so the conversation lifecycle assertions don't bleed across.
func TestConversationStore_SQLite(t *testing.T) {
	dbtest.RunConversationStoreConformance(t, func(t *testing.T) (db.ConversationStore, string, string, dbtest.ConversationSeeder) {
		t.Helper()
		conn := newSQLiteForConversationTest(t)
		seed := newSQLiteConversationSeeder(conn)
		stores := sqlitestore.New(conn)
		return stores.Conversations, runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID, seed
	})
}

// TestConversationStore_SQLite_ReturnedRow runs the returned-row conformance
// suite against the SQLite impl.
func TestConversationStore_SQLite_ReturnedRow(t *testing.T) {
	dbtest.RunConversationReturnedRowConformance(t, func(t *testing.T) (db.ConversationStore, db.ConversationQueueStore, string, string, dbtest.ConversationSeeder) {
		t.Helper()
		conn := newSQLiteForConversationTest(t)
		seed := newSQLiteConversationSeeder(conn)
		stores := sqlitestore.New(conn)
		return stores.Conversations, stores.ConversationQueue, runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID, seed
	})
}

// newSQLiteForConversationTest opens an in-memory DB, bootstraps the
// schema, and seeds the local default agent + the conformance
// prompt. Returned connection is t.Cleanup-closed.
func newSQLiteForConversationTest(t *testing.T) *sql.DB {
	t.Helper()
	conn, err := sql.Open("sqlite", db.TestDSNMemory)
	if err != nil {
		t.Fatalf("open in-memory db: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	conn.SetMaxOpenConns(1)
	conn.SetMaxIdleConns(1)
	if err := db.BootstrapSchemaForTest(conn); err != nil {
		t.Fatalf("bootstrap schema: %v", err)
	}
	// agents row backs conversations.actor_agent_id and task claim stamps —
	// migration seeds the sentinel user/team but not the agent row
	// itself (production does that via BootstrapAgentForOrg).
	if _, err := conn.Exec(
		`INSERT OR IGNORE INTO agents (id, org_id, display_name) VALUES (?, ?, 'Test Bot')`,
		runmode.LocalDefaultAgentID, runmode.LocalDefaultOrgID,
	); err != nil {
		t.Fatalf("seed local agent: %v", err)
	}
	// Conformance suite's conv.PromptID points at this stable ID.
	if _, err := conn.Exec(
		`INSERT INTO prompts (id, name, body, creator_user_id, team_id) VALUES ('p_conversation_test', 'Test', 'body', ?, ?)`,
		runmode.LocalDefaultUserID, runmode.LocalDefaultTeamID,
	); err != nil {
		t.Fatalf("seed prompt: %v", err)
	}
	return conn
}

// seedSQLiteConversation inserts a conversations row directly (the seeder's
// Conversation callback + the direct tests below share it). Raw SQL keeps
// the seed independent of the store under test; the trigger_type↔creator
// CHECK is satisfied by pairing 'manual' with the sentinel user and 'event'
// with NULL.
func seedSQLiteConversation(t *testing.T, conn *sql.DB, conv domain.Conversation) string {
	t.Helper()
	id := conv.ID
	if id == "" {
		id = uuid.New().String()
	}
	trigger := conv.TriggerType
	if trigger == "" {
		trigger = "manual"
	}
	var creator any
	if trigger == "manual" {
		creator = runmode.LocalDefaultUserID
	}
	var triggerID any
	if conv.TriggerID != "" {
		triggerID = conv.TriggerID
	}
	// An empty status is the mid-flight state — SQL NULL, which the display
	// ladder reads as `queued` — not the empty string, which is no status.
	var status any
	if conv.Status != "" {
		status = conv.Status
	}
	// team_id defaults to the local sentinel team; a conversation staged for
	// a team-narrowing test names its own.
	teamID := conv.TeamID
	if teamID == "" {
		teamID = runmode.LocalDefaultTeamID
	}
	if _, err := conn.Exec(`
		INSERT INTO conversations (id, task_id, prompt_id, status, model,
		                           trigger_type, trigger_id, team_id, visibility,
		                           creator_user_id, blueprint_run_id)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, 'team', ?, ?)
	`, id, conv.TaskID, conv.PromptID, status, conv.Model,
		trigger, triggerID, teamID, creator, conv.BlueprintRunID); err != nil {
		t.Fatalf("seed conversation: %v", err)
	}
	return id
}

// newSQLiteConversationSeeder returns the FactorySeeder-style bag of
// callbacks the conformance suite drives. Raw SQL keeps the seeder
// independent of the store under test.
func newSQLiteConversationSeeder(conn *sql.DB) dbtest.ConversationSeeder {
	return dbtest.ConversationSeeder{
		Entity: func(t *testing.T, suffix string) string {
			t.Helper()
			id := uuid.New().String()
			sourceID := fmt.Sprintf("conv-%s-%s", suffix, id[:8])
			if _, err := conn.Exec(`
				INSERT INTO entities (id, source, source_id, kind, title, url, snapshot_json, created_at)
				VALUES (?, 'github', ?, 'pr', ?, ?, '{}', ?)
			`, id, sourceID, "Conformance "+suffix, "https://example/"+sourceID, time.Now().UTC()); err != nil {
				t.Fatalf("seed entity %s: %v", suffix, err)
			}
			return id
		},
		Event: func(t *testing.T, entityID, eventType string) string {
			t.Helper()
			id := uuid.New().String()
			if _, err := conn.Exec(`
				INSERT INTO events (id, entity_id, event_type, dedup_key, metadata_json, created_at)
				VALUES (?, ?, ?, '', '{}', ?)
			`, id, entityID, eventType, time.Now().UTC()); err != nil {
				t.Fatalf("seed event: %v", err)
			}
			return id
		},
		Task: func(t *testing.T, entityID, eventType, primaryEventID string) string {
			t.Helper()
			id := uuid.New().String()
			if _, err := conn.Exec(`
				INSERT INTO tasks (id, entity_id, event_type, dedup_key, primary_event_id,
				                   status, priority_score, scoring_status, created_at,
				                   team_id, visibility)
				VALUES (?, ?, ?, '', ?, 'queued', 0.5, 'pending', ?, ?, 'team')
			`, id, entityID, eventType, primaryEventID, time.Now().UTC(), runmode.LocalDefaultTeamID); err != nil {
				t.Fatalf("seed task: %v", err)
			}
			return id
		},
		Team: func(t *testing.T, slug string) string {
			t.Helper()
			id := uuid.New().String()
			if _, err := conn.Exec(
				`INSERT INTO teams (id, org_id, slug, name) VALUES (?, ?, ?, ?)`,
				id, runmode.LocalDefaultOrgID, slug+"-"+id[:8], "Conformance "+slug,
			); err != nil {
				t.Fatalf("seed team %s: %v", slug, err)
			}
			return id
		},
		Conversation: func(t *testing.T, conv domain.Conversation) string {
			return seedSQLiteConversation(t, conn, conv)
		},
		BackdateStartedAt: func(t *testing.T, conversationID string, age time.Duration) {
			t.Helper()
			// datetime('now', ...) renders the same 'YYYY-MM-DD HH:MM:SS'
			// shape CURRENT_TIMESTAMP writes, so a backdated row stays
			// comparable with the ones the column default stamped.
			if _, err := conn.Exec(
				`UPDATE conversations SET started_at = datetime('now', ?) WHERE id = ?`,
				fmt.Sprintf("-%d seconds", int64(age.Seconds())), conversationID); err != nil {
				t.Fatalf("backdate started_at of %s: %v", conversationID, err)
			}
		},
		BackdateQueuedAt: func(t *testing.T, conversationID string, age time.Duration) {
			t.Helper()
			if _, err := conn.Exec(
				`UPDATE conversations SET queued_at = datetime('now', ?) WHERE id = ?`,
				fmt.Sprintf("-%d seconds", int64(age.Seconds())), conversationID); err != nil {
				t.Fatalf("backdate queued_at of %s: %v", conversationID, err)
			}
		},
		ClaimRows: func(t *testing.T, conversationID string) []dbtest.ClaimRow {
			t.Helper()
			rows, err := conn.Query(`
				SELECT id, executor_id, boot_epoch, COALESCE(phase, ''), released_at IS NOT NULL, COALESCE(outcome, ''),
				       peak_mem_mb, cpu_usec
				FROM claims WHERE conversation_id = ? ORDER BY rowid ASC
			`, conversationID)
			if err != nil {
				t.Fatalf("read claims: %v", err)
			}
			defer rows.Close()
			var out []dbtest.ClaimRow
			for rows.Next() {
				var c dbtest.ClaimRow
				// Nullable: the measured actuals stay NULL for anything that
				// was never sandboxed, which locally is every run.
				var peak, cpu sql.NullInt64
				if err := rows.Scan(&c.ID, &c.ExecutorID, &c.BootEpoch, &c.Phase, &c.Released, &c.Outcome, &peak, &cpu); err != nil {
					t.Fatalf("scan claim: %v", err)
				}
				if peak.Valid {
					mb := int(peak.Int64)
					c.PeakMemMB = &mb
				}
				if cpu.Valid {
					usec := cpu.Int64
					c.CPUUsec = &usec
				}
				out = append(out, c)
			}
			if err := rows.Err(); err != nil {
				t.Fatalf("claims rows: %v", err)
			}
			return out
		},
		PreferredExecutor: func(t *testing.T, conversationID string) string {
			t.Helper()
			var pref sql.NullString
			if err := conn.QueryRow(
				`SELECT preferred_executor_id FROM conversations WHERE id = ?`, conversationID,
			).Scan(&pref); err != nil {
				t.Fatalf("read preferred_executor_id: %v", err)
			}
			return pref.String
		},
		CollapseClaimTimes: func(t *testing.T, conversationID string) {
			t.Helper()
			if _, err := conn.Exec(`
				UPDATE claims
				SET claimed_at = (SELECT MIN(claimed_at) FROM claims WHERE conversation_id = ?),
				    created_at = (SELECT MIN(created_at) FROM claims WHERE conversation_id = ?)
				WHERE conversation_id = ?
			`, conversationID, conversationID, conversationID); err != nil {
				t.Fatalf("collapse claim times: %v", err)
			}
		},
		Artifact: func(t *testing.T, conversationID, kind, state, detailsJSON string) string {
			t.Helper()
			id := uuid.New().String()
			if _, err := conn.Exec(`
				INSERT INTO artifacts (id, conversation_id, team_id, provider, kind, target,
				                       state, dedup_key, details_json)
				VALUES (?, ?, ?, 'github', ?, 'o/r#7', ?, ?, ?)
			`, id, conversationID, runmode.LocalDefaultTeamID, kind, state, id, detailsJSON); err != nil {
				t.Fatalf("seed artifact: %v", err)
			}
			return id
		},
		PendingPermission: func(t *testing.T, conversationID, claimID, toolCallID string) string {
			t.Helper()
			id := uuid.New().String()
			if _, err := conn.Exec(`
				INSERT INTO conversation_permissions (id, conversation_id, claim_id, tool_call_id,
				                                      tool_name, state, requested_at)
				VALUES (?, ?, ?, ?, 'Bash', 'pending', ?)
			`, id, conversationID, claimID, toolCallID, time.Now().UTC()); err != nil {
				t.Fatalf("seed permission: %v", err)
			}
			return id
		},
		StampAgentClaim: func(t *testing.T, taskID, agentID string) {
			t.Helper()
			if _, err := conn.Exec(
				`UPDATE tasks SET claimed_by_agent_id = ?, claimed_by_user_id = NULL WHERE id = ?`,
				agentID, taskID,
			); err != nil {
				t.Fatalf("stamp claim: %v", err)
			}
		},
		BlueprintRun: func(t *testing.T, taskID string) string {
			t.Helper()
			// conversations.blueprint_run_id is required by the origin CHECK —
			// a single prompt is a 1-step blueprint, so every run needs a
			// parent blueprint_run. Mint a fresh blueprint + blueprint_run per
			// call. SQLite blueprint_runs has no org_id/creator_user_id
			// columns; org_id on blueprints takes its local-sentinel DEFAULT.
			bpID := uuid.New().String()
			if _, err := conn.Exec(`
				INSERT INTO blueprints (id, name, source, team_id, creator_user_id)
				VALUES (?, 'Conformance BP', 'user', ?, ?)
			`, bpID, runmode.LocalDefaultTeamID, runmode.LocalDefaultUserID); err != nil {
				t.Fatalf("seed blueprint: %v", err)
			}
			brID := uuid.New().String()
			if _, err := conn.Exec(`
				INSERT INTO blueprint_runs (id, blueprint_id, task_id, trigger_type, status, worktree_path, step_plan)
				VALUES (?, ?, ?, 'manual', 'running', '/tmp/wt', '[]')
			`, brID, bpID, taskID); err != nil {
				t.Fatalf("seed blueprint_run: %v", err)
			}
			return brID
		},
		SetSnapshotState: func(t *testing.T, blueprintRunID, state string) {
			t.Helper()
			// The writer is a real uuid because the Postgres column is one —
			// the shared suite exists to catch exactly that kind of drift —
			// and which engagement wrote the blob is not what the eviction
			// enumeration reads, so a fixed value is enough.
			if _, err := conn.Exec(`
				INSERT INTO workspace_snapshots (org_id, blueprint_run_id, state, writer_claim_id)
				VALUES (?, ?, ?, '11111111-1111-4111-8111-111111111111')
				ON CONFLICT (org_id, blueprint_run_id) DO UPDATE SET state = excluded.state
			`, runmode.LocalDefaultOrgID, blueprintRunID, state); err != nil {
				t.Fatalf("seed workspace snapshot state: %v", err)
			}
		},
		SetBlueprintRunStatus: func(t *testing.T, blueprintRunID, status string) {
			t.Helper()
			// Raw UPDATE — must NOT cascade onto child conversations (unlike
			// BlueprintStore.MarkRunStatus), so the parked child stays parked.
			if _, err := conn.Exec(`UPDATE blueprint_runs SET status = ? WHERE id = ?`, status, blueprintRunID); err != nil {
				t.Fatalf("set blueprint_run status: %v", err)
			}
		},
		SetConversationMemory: func(t *testing.T, conversationID, entityID, content string) {
			t.Helper()
			memID := uuid.New().String()
			if content == dbtest.NullMemorySentinel {
				if _, err := conn.Exec(`
					INSERT INTO conversation_memory (id, conversation_id, entity_id, agent_content) VALUES (?, ?, ?, NULL)
				`, memID, conversationID, entityID); err != nil {
					t.Fatalf("seed null memory: %v", err)
				}
				return
			}
			if _, err := conn.Exec(`
				INSERT INTO conversation_memory (id, conversation_id, entity_id, agent_content) VALUES (?, ?, ?, ?)
			`, memID, conversationID, entityID, content); err != nil {
				t.Fatalf("seed memory: %v", err)
			}
		},
		SeedRawMessage: func(t *testing.T, conversationID, column, rawJSON string) int64 {
			t.Helper()
			if column != "reasoning" && column != "content_blocks" {
				t.Fatalf("SeedRawMessage: unsupported column %q", column)
			}
			// column is a fixed test-controlled name (not user input), so
			// string-building the column into the statement is safe here.
			res, err := conn.Exec(
				`INSERT INTO messages (conversation_id, role, subtype, content, `+column+`) VALUES (?, 'assistant', '', 'x', ?)`,
				conversationID, rawJSON,
			)
			if err != nil {
				t.Fatalf("seed raw message (%s): %v", column, err)
			}
			id, err := res.LastInsertId()
			if err != nil {
				t.Fatalf("seed raw message lastInsertId: %v", err)
			}
			return id
		},
		AgentID: runmode.LocalDefaultAgentID,
	}
}

// TestConversationStore_SQLite_AssertLocalOrg pins the local-only invariant:
// the orgID guard at every method entry refuses non-LocalDefaultOrgID.
// The conformance suite exercises the happy path; this test pins the
// SQLite-specific rejection.
func TestConversationStore_SQLite_AssertLocalOrg(t *testing.T) {
	conn := newSQLiteForConversationTest(t)
	store := sqlitestore.New(conn).Conversations
	if _, err := store.ActiveIDsForTask(t.Context(), "some-other-org", uuid.New().String()); err == nil {
		t.Error("ActiveIDsForTask accepted non-LocalDefaultOrgID without error")
	}
}

// TestConversationStore_SQLite_RuntimeDefaultsToSDK pins the
// conversations.runtime schema fact the columnar-canon epic depends on: a
// freshly seeded delegation row lands as 'sdk', not the native loop's
// 'native'. domain.Conversation has no write path for Runtime (nothing writes
// 'native' until the executor-side loop lands), so this reads the column
// directly.
func TestConversationStore_SQLite_RuntimeDefaultsToSDK(t *testing.T) {
	conn := newSQLiteForConversationTest(t)
	seed := newSQLiteConversationSeeder(conn)

	ent := seed.Entity(t, "runtime")
	ev := seed.Event(t, ent, domain.EventGitHubPROpened)
	taskID := seed.Task(t, ent, domain.EventGitHubPROpened, ev)
	conversationID := seedSQLiteConversation(t, conn, domain.Conversation{
		TaskID: taskID, PromptID: "p_conversation_test", Status: "running", Model: "m",
		BlueprintRunID: seed.BlueprintRun(t, taskID),
	})

	var runtime string
	if err := conn.QueryRow(`SELECT runtime FROM conversations WHERE id = ?`, conversationID).Scan(&runtime); err != nil {
		t.Fatalf("read runtime: %v", err)
	}
	if runtime != "sdk" {
		t.Errorf("runtime = %q, want sdk", runtime)
	}
}

// TestConversationStore_SQLite_ActiveIDsForTeamSystem pins the team-archive
// force-stop enumeration: conversations on the team in the active set
// (NOT completed/failed) are returned; terminal conversations are excluded.
// SQLite hardcodes conversations.team_id to the local sentinel, so the
// cross-team negative case lives in the Postgres tests; here we pin the status
// predicate + team scoping.
func TestConversationStore_SQLite_ActiveIDsForTeamSystem(t *testing.T) {
	conn := newSQLiteForConversationTest(t)
	seed := newSQLiteConversationSeeder(conn)
	store := sqlitestore.New(conn).Conversations
	ctx := context.Background()

	ent := seed.Entity(t, "team-active")
	ev := seed.Event(t, ent, domain.EventGitHubPROpened)
	taskID := seed.Task(t, ent, domain.EventGitHubPROpened, ev)

	mk := func(status string) string {
		return seedSQLiteConversation(t, conn, domain.Conversation{
			TaskID: taskID, PromptID: "p_conversation_test", Status: status, Model: "m",
			BlueprintRunID: seed.BlueprintRun(t, taskID),
		})
	}

	running := mk("running")
	open := mk("open")
	mk("completed")
	mk("failed")

	ids, err := store.ActiveIDsForTeamSystem(ctx, runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID)
	if err != nil {
		t.Fatalf("ActiveIDsForTeamSystem: %v", err)
	}
	got := map[string]bool{}
	for _, id := range ids {
		got[id] = true
	}
	if len(ids) != 2 || !got[running] || !got[open] {
		t.Fatalf("ActiveIDsForTeamSystem = %v; want exactly the running + open conversations (%s, %s)", ids, running, open)
	}
}

// TestConversationStore_SQLite_QueuePositionAcrossIDChunks drives List over a
// task-id set large enough to be split across statements — the only path where
// the chunking is visible, and the reason the queue's ranking is read once
// before the loop rather than derived inside each chunk's own query. Every
// chunk must see one ranking of the WHOLE queue: a rank computed per chunk
// would answer a question no chunk narrows, and would rank each chunk against
// its own snapshot.
//
// Only reachable unwindowed: a windowed read of more than one chunk is
// refused, because a window is meaningless across statements.
func TestConversationStore_SQLite_QueuePositionAcrossIDChunks(t *testing.T) {
	conn := newSQLiteForConversationTest(t)
	seed := newSQLiteConversationSeeder(conn)
	store := sqlitestore.New(conn).Conversations
	ctx := context.Background()

	// One queued conversation per task, oldest first. A conversation with no
	// stored status is mid-flight and unclaimed, which the display ladder
	// reads as `queued`; started_at is staged on the database's own clock so
	// the line's order is this fixture's.
	mk := func(suffix string, ageSeconds int) (taskID, conversationID string) {
		t.Helper()
		ent := seed.Entity(t, suffix)
		ev := seed.Event(t, ent, domain.EventGitHubPROpened)
		taskID = seed.Task(t, ent, domain.EventGitHubPROpened, ev)
		conversationID = seedSQLiteConversation(t, conn, domain.Conversation{
			TaskID: taskID, PromptID: "p_conversation_test", Model: "m",
			BlueprintRunID: seed.BlueprintRun(t, taskID),
		})
		if _, err := conn.Exec(`UPDATE conversations SET started_at = datetime('now', ?) WHERE id = ?`,
			fmt.Sprintf("-%d seconds", ageSeconds), conversationID); err != nil {
			t.Fatalf("backdate started_at: %v", err)
		}
		return taskID, conversationID
	}
	taskA, convA := mk("chunk-a", 300)
	taskB, convB := mk("chunk-b", 200)
	taskC, convC := mk("chunk-c", 100)

	// The head of the queue lands in the first chunk and the other two in the
	// second, so a per-chunk rank would restart the numbering at the chunk
	// boundary and hand convB position 1.
	taskIDs := make([]string, 0, 502)
	taskIDs = append(taskIDs, taskA)
	for len(taskIDs) < 500 {
		taskIDs = append(taskIDs, uuid.New().String())
	}
	taskIDs = append(taskIDs, taskB, taskC)

	convs, total, err := store.List(ctx, runmode.LocalDefaultOrgID,
		db.ConversationListFilter{TaskIDs: taskIDs}, db.ListOpts{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if total != 3 {
		t.Fatalf("total = %d, want 3", total)
	}
	got := map[string]int{}
	for _, c := range convs {
		if c.QueuePosition == nil {
			t.Fatalf("conversation %s is queued but carries no position", c.ID)
		}
		got[c.ID] = *c.QueuePosition
	}
	for id, want := range map[string]int{convA: 1, convB: 2, convC: 3} {
		if got[id] != want {
			t.Errorf("queue position of %s = %d, want %d", id, got[id], want)
		}
	}
}

// TestConversationStore_SQLite_PRCoherenceTargets runs the PR coherence
// conformance suite against the SQLite impl.
func TestConversationStore_SQLite_PRCoherenceTargets(t *testing.T) {
	dbtest.RunPRCoherenceTargetsConformance(t, func(t *testing.T) (db.ConversationStore, string, dbtest.ConversationSeeder, dbtest.PRCoherenceSeeder) {
		t.Helper()
		conn := newSQLiteForConversationTest(t)
		stores := sqlitestore.New(conn)
		ctx := context.Background()
		extra := dbtest.PRCoherenceSeeder{
			PendingReview: func(t *testing.T, conversationID, repo string, prNumber int) {
				t.Helper()
				a := domain.NewReviewArtifact(repo, prNumber, "head-sha", conversationID)
				a.ConversationID = conversationID
				a.OrgID, a.TeamID = runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID
				if _, err := stores.Artifacts.UpsertSystem(ctx, runmode.LocalDefaultOrgID, a); err != nil {
					t.Fatalf("seed pending review: %v", err)
				}
			},
			Worktree: func(t *testing.T, conversationID, slug, ref string) {
				t.Helper()
				if _, err := stores.Repos.GetOrCreateSystem(ctx, runmode.LocalDefaultOrgID, domain.RepoRefFromSlug(slug)); err != nil {
					t.Fatalf("seed repository %s: %v", slug, err)
				}
				if _, _, err := stores.ConversationWorktrees.InsertSystem(ctx, runmode.LocalDefaultOrgID, domain.ConversationWorktree{
					ConversationID: conversationID, RepoID: slug, Ref: ref,
					Path: "/tmp/coherence/" + conversationID + "/" + ref,
				}); err != nil {
					t.Fatalf("seed worktree: %v", err)
				}
			},
			MarkEventInjected: func(t *testing.T, taskID, eventID string) {
				t.Helper()
				if _, err := conn.Exec(`
					INSERT INTO task_events (task_id, event_id, kind, org_id) VALUES (?, ?, 'injected', ?)
				`, taskID, eventID, runmode.LocalDefaultOrgID); err != nil {
					t.Fatalf("record injected task event: %v", err)
				}
			},
			ActiveClaim: func(t *testing.T, conversationID string) {
				t.Helper()
				dbtest.SeedActiveClaim(t, conn, conversationID, "executor-coherence", 1)
			},
		}
		return stores.Conversations, runmode.LocalDefaultOrgID, newSQLiteConversationSeeder(conn), extra
	})
}

// TestConversationStore_SQLite_FenceRefusesAClaimFromAnotherOrg pins the org
// binding on this dialect's claim fence. Postgres gets it from a composite FK
// tying (conversation_id, org_id) on claims to the conversation; this schema
// has no such FK, so a claims row can name a conversation while carrying a
// different org_id. The fence must treat that row as no claim at all — a
// refusal — rather than let its conversation_id alone vouch for it.
func TestConversationStore_SQLite_FenceRefusesAClaimFromAnotherOrg(t *testing.T) {
	conn := newSQLiteForConversationTest(t)
	seed := newSQLiteConversationSeeder(conn)
	store := sqlitestore.New(conn).Conversations
	ctx := context.Background()
	org := runmode.LocalDefaultOrgID

	mintConversation := func(suffix string) string {
		ent := seed.Entity(t, suffix)
		ev := seed.Event(t, ent, domain.EventGitHubPROpened)
		taskID := seed.Task(t, ent, domain.EventGitHubPROpened, ev)
		return seed.Conversation(t, domain.Conversation{
			TaskID: taskID, PromptID: "p_conversation_test", Status: "running", Model: "m", BlueprintRunID: seed.BlueprintRun(t, taskID),
		})
	}
	mintClaim := func(conversationID, claimOrg string) string {
		id := uuid.New().String()
		if _, err := conn.Exec(`
			INSERT INTO claims (id, org_id, conversation_id, executor_id, boot_epoch)
			VALUES (?, ?, ?, 'exec', 1)
		`, id, claimOrg, conversationID); err != nil {
			t.Fatalf("seed claim under org %s: %v", claimOrg, err)
		}
		return id
	}

	// Control: a claim whose org matches its conversation's and the caller's
	// passes the fence, so the refusal below is the org binding and nothing else.
	ownConv := mintConversation("own-org")
	ownClaim := mintClaim(ownConv, org)
	if _, err := store.SetClaimPhaseSystem(ctx, org, ownConv, ownClaim, "cloning"); err != nil {
		t.Fatalf("phase write on a claim of the conversation's own org: %v", err)
	}

	otherConv := mintConversation("other-org")
	otherClaim := mintClaim(otherConv, uuid.New().String())
	if _, err := store.SetClaimPhaseSystem(ctx, org, otherConv, otherClaim, "cloning"); !errors.Is(err, db.ErrClaimReleased) {
		t.Fatalf("phase write on a claim carrying another org = %v, want ErrClaimReleased", err)
	}
	pending := false
	if _, err := store.InsertMessageForClaimSystem(ctx, org, otherClaim, &domain.Message{
		ConversationID: otherConv, Role: "assistant", Content: "zombie", Delivered: &pending,
	}); !errors.Is(err, db.ErrClaimReleased) {
		t.Fatalf("transcript write on a claim carrying another org = %v, want ErrClaimReleased", err)
	}
	if msgs, err := store.Messages(ctx, org, otherConv); err != nil || len(msgs) != 0 {
		t.Errorf("Messages = %v (err %v), want nothing written behind the refusal", msgs, err)
	}
}
