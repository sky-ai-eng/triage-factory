package postgres_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TestConversationStore_Postgres runs the shared conformance suite
// against the Postgres ConversationStore impl. Each subtest gets a
// fresh org + team + user + prompt + agent seed; the suite drives
// every method through its happy and edge paths.
func TestConversationStore_Postgres(t *testing.T) {
	h := pgtest.Shared(t)
	// Wire both pools against AdminDB so the conversation lifecycle
	// statements run without a JWT-claims tx. Production wiring
	// uses the app pool, but the conformance suite is about
	// behavior, not auth; the cross-org leakage test below
	// exercises the org_id defense-in-depth filter directly.
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)

	dbtest.RunConversationStoreConformance(t, func(t *testing.T) (db.ConversationStore, string, string, dbtest.ConversationSeeder) {
		t.Helper()
		h.Reset(t)
		orgID, userID, agentID := seedPgConversationOrg(t, h)
		promptID := seedPgConversationPrompt(t, h, orgID, userID)
		seeder := newPgConversationSeeder(h.AdminDB, orgID, userID, agentID, promptID)
		return stores.Conversations, orgID, userID, seeder
	})
}

// TestConversationStore_Postgres_ReturnedRow runs the returned-row
// conformance suite against the admin pool — see
// TestConversationStore_Postgres_ReturnedRow_AppPool for the RLS-under-claims
// wiring that exercises the RETURNING visibility property directly.
func TestConversationStore_Postgres_ReturnedRow(t *testing.T) {
	h := pgtest.Shared(t)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)

	dbtest.RunConversationReturnedRowConformance(t, func(t *testing.T) (db.ConversationStore, db.ConversationQueueStore, string, string, dbtest.ConversationSeeder) {
		t.Helper()
		h.Reset(t)
		orgID, userID, agentID := seedPgConversationOrg(t, h)
		promptID := seedPgConversationPrompt(t, h, orgID, userID)
		seeder := newPgConversationSeeder(h.AdminDB, orgID, userID, agentID, promptID)
		return stores.Conversations, stores.ConversationQueue, orgID, userID, seeder
	})
}

// seedPgConversationOrg builds the auth.user + public.user + org +
// org_membership + default team + agent graph the ConversationStore
// needs. Mirrors seedPgOrgUserAgent from tasks_test.go.
func seedPgConversationOrg(t *testing.T, h *pgtest.Harness) (orgID, userID, agentID string) {
	t.Helper()
	orgID = uuid.New().String()
	userID = uuid.New().String()
	agentID = uuid.New().String()
	email := fmt.Sprintf("conv-%s@test.local", userID[:8])

	h.SeedAuthUser(t, userID, email)
	if _, err := h.AdminDB.Exec(
		`INSERT INTO users (id, display_name) VALUES ($1, $2)`,
		userID, "Conversation Conformance User",
	); err != nil {
		t.Fatalf("seed public.users: %v", err)
	}
	if _, err := h.AdminDB.Exec(
		`INSERT INTO orgs (id, name, slug, owner_user_id) VALUES ($1, $2, $3, $4)`,
		orgID, "Conversation Org "+orgID[:8], "ar-"+orgID[:8], userID,
	); err != nil {
		t.Fatalf("seed org: %v", err)
	}
	if _, err := h.AdminDB.Exec(
		`INSERT INTO org_memberships (org_id, user_id, role) VALUES ($1, $2, 'owner')`,
		orgID, userID,
	); err != nil {
		t.Fatalf("seed org_membership: %v", err)
	}
	seedPgDefaultTeam(t, h, orgID, userID)
	if _, err := h.AdminDB.Exec(
		`INSERT INTO agents (id, org_id, display_name) VALUES ($1, $2, 'Conformance Bot')`,
		agentID, orgID,
	); err != nil {
		t.Fatalf("seed agent: %v", err)
	}
	return orgID, userID, agentID
}

// seedPgConversationPrompt inserts a user-source prompt the conformance
// suite's conversations FK into. Stable id `p_conversation_test` matches the
// constant the shared harness expects.
func seedPgConversationPrompt(t *testing.T, h *pgtest.Harness, orgID, userID string) string {
	t.Helper()
	teamID := firstTeamForOrg(t, h, orgID)
	if _, err := h.AdminDB.Exec(`
		INSERT INTO prompts (id, org_id, creator_user_id, team_id, name, body, source, allowed_tools, created_at, updated_at)
		VALUES ('p_conversation_test', $1, $2, $3, 'Conversation Test', 'body', 'user', '', now(), now())
	`, orgID, userID, teamID); err != nil {
		t.Fatalf("seed prompt: %v", err)
	}
	return "p_conversation_test"
}

// newPgConversationSeeder builds the FactorySeeder-style callbacks the
// conformance harness uses to stage non-conversation fixture rows. INSERTs
// carry org_id explicitly so the cross-org leakage test below can
// reuse the same seeder for two orgs in parallel.
func newPgConversationSeeder(conn *sql.DB, orgID, userID, agentID, promptID string) dbtest.ConversationSeeder {
	_ = promptID // referenced via the conformance suite's constant
	return dbtest.ConversationSeeder{
		Entity: func(t *testing.T, suffix string) string {
			t.Helper()
			id := uuid.New().String()
			sourceID := fmt.Sprintf("conv-%s-%s", suffix, id[:8])
			if _, err := conn.Exec(`
				INSERT INTO entities (id, org_id, source, source_id, kind, title, url, snapshot_json, created_at)
				VALUES ($1, $2, 'github', $3, 'pr', $4, $5, '{}'::jsonb, $6)
			`, id, orgID, sourceID, "Conformance "+suffix, "https://example/"+sourceID, time.Now().UTC()); err != nil {
				t.Fatalf("seed entity %s: %v", suffix, err)
			}
			return id
		},
		Event: func(t *testing.T, entityID, eventType string) string {
			t.Helper()
			id := uuid.New().String()
			if _, err := conn.Exec(`
				INSERT INTO events (id, org_id, entity_id, event_type, dedup_key, metadata_json, created_at)
				VALUES ($1, $2, $3, $4, '', '{}'::jsonb, $5)
			`, id, orgID, entityID, eventType, time.Now().UTC()); err != nil {
				t.Fatalf("seed event %s: %v", eventType, err)
			}
			return id
		},
		Task: func(t *testing.T, entityID, eventType, primaryEventID string) string {
			t.Helper()
			id := uuid.New().String()
			if _, err := conn.Exec(`
				INSERT INTO tasks (id, org_id, creator_user_id, team_id, visibility, entity_id, event_type, dedup_key, primary_event_id,
				                   status, scoring_status, priority_score, created_at)
				VALUES ($1, $2, $3,
				        (SELECT id FROM teams WHERE org_id = $2 ORDER BY created_at ASC LIMIT 1),
				        'team', $4, $5, '', $6, 'queued', 'pending', 0.5, $7)
			`, id, orgID, userID, entityID, eventType, primaryEventID, time.Now().UTC()); err != nil {
				t.Fatalf("seed task: %v", err)
			}
			return id
		},
		Team: func(t *testing.T, slug string) string {
			t.Helper()
			id := uuid.New().String()
			// A membership row too: conversations.team_id is FK-checked here,
			// and a team the seeding user belongs to keeps the fixture
			// representative of the rows RLS would admit.
			if _, err := conn.Exec(
				`INSERT INTO teams (id, org_id, slug, name) VALUES ($1, $2, $3, $4)`,
				id, orgID, slug+"-"+id[:8], "Conformance "+slug,
			); err != nil {
				t.Fatalf("seed team %s: %v", slug, err)
			}
			if _, err := conn.Exec(
				`INSERT INTO memberships (user_id, team_id, role) VALUES ($1, $2, 'member')`,
				userID, id,
			); err != nil {
				t.Fatalf("seed team membership %s: %v", slug, err)
			}
			return id
		},
		Conversation: func(t *testing.T, conv domain.Conversation) string {
			t.Helper()
			if conv.CreatorUserID == "" && conv.TriggerType != "event" {
				conv.CreatorUserID = userID
			}
			return seedPgConversation(t, conn, orgID, conv)
		},
		ClaimRows: func(t *testing.T, conversationID string) []dbtest.ClaimRow {
			t.Helper()
			rows, err := conn.Query(`
				SELECT id::text, executor_id, boot_epoch, COALESCE(phase, ''), released_at IS NOT NULL, COALESCE(outcome, ''),
				       peak_mem_mb, cpu_usec
				FROM claims WHERE conversation_id = $1 ORDER BY claimed_at ASC, created_at ASC
			`, conversationID)
			if err != nil {
				t.Fatalf("read claims: %v", err)
			}
			defer rows.Close()
			var out []dbtest.ClaimRow
			for rows.Next() {
				var c dbtest.ClaimRow
				// Nullable: the measured actuals stay NULL for anything that
				// was never sandboxed.
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
				`SELECT preferred_executor_id FROM conversations WHERE id = $1`, conversationID,
			).Scan(&pref); err != nil {
				t.Fatalf("read preferred_executor_id: %v", err)
			}
			return pref.String
		},
		CollapseClaimTimes: func(t *testing.T, conversationID string) {
			t.Helper()
			if _, err := conn.Exec(`
				UPDATE claims SET claimed_at = m.at, created_at = m.at
				FROM (SELECT MIN(claimed_at) AS at FROM claims WHERE conversation_id = $1) m
				WHERE conversation_id = $1
			`, conversationID); err != nil {
				t.Fatalf("collapse claim times: %v", err)
			}
		},
		Artifact: func(t *testing.T, conversationID, kind, state, detailsJSON string) string {
			t.Helper()
			id := uuid.New().String()
			if _, err := conn.Exec(`
				INSERT INTO artifacts (id, org_id, conversation_id, team_id, provider, kind, target,
				                       state, dedup_key, details_json)
				VALUES ($1, $2, $3,
				        (SELECT id FROM teams WHERE org_id = $2 ORDER BY created_at ASC LIMIT 1),
				        'github', $4, 'o/r#7', $5, $6, $7)
			`, id, orgID, conversationID, kind, state, id, detailsJSON); err != nil {
				t.Fatalf("seed artifact: %v", err)
			}
			return id
		},
		PendingPermission: func(t *testing.T, conversationID, claimID, toolCallID string) string {
			t.Helper()
			id := uuid.New().String()
			if _, err := conn.Exec(`
				INSERT INTO conversation_permissions (id, org_id, conversation_id, claim_id, tool_call_id,
				                                      tool_name, state, requested_at)
				VALUES ($1, $2, $3, $4, $5, 'Bash', 'pending', $6)
			`, id, orgID, conversationID, claimID, toolCallID, time.Now().UTC()); err != nil {
				t.Fatalf("seed permission: %v", err)
			}
			return id
		},
		StampAgentClaim: func(t *testing.T, taskID, agent string) {
			t.Helper()
			if _, err := conn.Exec(
				`UPDATE tasks SET claimed_by_agent_id = $1::uuid, claimed_by_user_id = NULL WHERE id = $2 AND org_id = $3`,
				agent, taskID, orgID,
			); err != nil {
				t.Fatalf("stamp claim: %v", err)
			}
		},
		BlueprintRun: func(t *testing.T, taskID string) string {
			t.Helper()
			// The origin CHECK requires blueprint_run_id — every run needs a parent
			// blueprint_run. Mint a fresh blueprint + blueprint_run per
			// call. Postgres requires org_id on both; blueprint_runs with
			// trigger_type='manual' also needs a non-NULL creator_user_id
			// (blueprint_runs_creator_matches_trigger_type CHECK). team_id
			// resolves from the org's sole seeded team, mirroring the other
			// seeders.
			bpID := uuid.New().String()
			if _, err := conn.Exec(`
				INSERT INTO blueprints (id, org_id, creator_user_id, team_id, name, source)
				VALUES ($1, $2, $3,
				        (SELECT id FROM teams WHERE org_id = $2 ORDER BY created_at ASC LIMIT 1),
				        'Conformance BP', 'user')
			`, bpID, orgID, userID); err != nil {
				t.Fatalf("seed blueprint: %v", err)
			}
			brID := uuid.New().String()
			if _, err := conn.Exec(`
				INSERT INTO blueprint_runs (id, org_id, creator_user_id, blueprint_id, task_id, trigger_type, status, worktree_path, step_plan)
				VALUES ($1, $2, $3, $4, $5, 'manual', 'running', '/tmp/wt', '[]')
			`, brID, orgID, userID, bpID, taskID); err != nil {
				t.Fatalf("seed blueprint_run: %v", err)
			}
			return brID
		},
		SetSnapshotState: func(t *testing.T, blueprintRunID, state string) {
			t.Helper()
			// Which engagement wrote the blob is not what the eviction
			// enumeration reads, so a fixed writer is enough — it just has to
			// be a real uuid for the column.
			if _, err := conn.Exec(`
				INSERT INTO workspace_snapshots (org_id, blueprint_run_id, state, writer_claim_id)
				VALUES ($1, $2, $3, '11111111-1111-4111-8111-111111111111')
				ON CONFLICT (org_id, blueprint_run_id) DO UPDATE SET state = excluded.state
			`, orgID, blueprintRunID, state); err != nil {
				t.Fatalf("seed workspace snapshot state: %v", err)
			}
		},
		SetBlueprintRunStatus: func(t *testing.T, blueprintRunID, status string) {
			t.Helper()
			// Raw UPDATE — must NOT cascade onto child conversations (unlike
			// BlueprintStore.MarkRunStatus), so the parked child stays parked.
			if _, err := conn.Exec(`UPDATE blueprint_runs SET status = $1 WHERE id = $2`, status, blueprintRunID); err != nil {
				t.Fatalf("set blueprint_run status: %v", err)
			}
		},
		SetConversationMemory: func(t *testing.T, conversationID, entityID, content string) {
			t.Helper()
			memID := uuid.New().String()
			if content == dbtest.NullMemorySentinel {
				if _, err := conn.Exec(`
					INSERT INTO conversation_memory (id, org_id, conversation_id, entity_id, agent_content) VALUES ($1, $2, $3, $4, NULL)
				`, memID, orgID, conversationID, entityID); err != nil {
					t.Fatalf("seed null memory: %v", err)
				}
				return
			}
			if _, err := conn.Exec(`
				INSERT INTO conversation_memory (id, org_id, conversation_id, entity_id, agent_content) VALUES ($1, $2, $3, $4, $5)
			`, memID, orgID, conversationID, entityID, content); err != nil {
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
			// rawJSON must be syntactically valid JSON — jsonb enforces that
			// at the storage layer — but can be any shape, including one
			// that fails to unmarshal into the target Go slice.
			var id int64
			if err := conn.QueryRow(
				`INSERT INTO messages (org_id, conversation_id, role, subtype, content, `+column+`)
				 VALUES ($1, $2, 'assistant', '', 'x', $3::jsonb) RETURNING id`,
				orgID, conversationID, rawJSON,
			).Scan(&id); err != nil {
				t.Fatalf("seed raw message (%s): %v", column, err)
			}
			return id
		},
		AgentID: agentID,
	}
}

// TestConversationStore_Postgres_CrossOrgLeakage pins the defense-in-
// depth guarantee: even with the org_id filter as the only line of
// defense (AdminDB bypasses RLS), org A's queries can't see org B's
// conversations. In production the RLS policies add a second layer; this
// test validates the WHERE-clause filter on its own so a regression
// there can't silently rely on RLS to compensate.
func TestConversationStore_Postgres_CrossOrgLeakage(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)

	orgA, userA, agentA := seedPgConversationOrg(t, h)
	orgB, userB, agentB := seedPgConversationOrg(t, h)
	_ = agentA
	_ = agentB
	seedPgConversationPromptIn(t, h, "p_xleak_A", orgA, userA)
	seedPgConversationPromptIn(t, h, "p_xleak_B", orgB, userB)

	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	ctx := context.Background()

	// Seed an entity + task + conversation in each org via the AdminDB so
	// the FK chain is satisfied.
	mkChain := func(t *testing.T, orgID, userID, promptID, conversationID string) (taskID string) {
		t.Helper()
		entityID := uuid.New().String()
		eventID := uuid.New().String()
		taskID = uuid.New().String()
		if _, err := h.AdminDB.Exec(`
			INSERT INTO entities (id, org_id, source, source_id, kind, title, url, snapshot_json, created_at)
			VALUES ($1, $2, 'github', $3, 'pr', 'Cross-org test', '', '{}'::jsonb, now())
		`, entityID, orgID, "xleak-"+orgID[:8]); err != nil {
			t.Fatalf("entity: %v", err)
		}
		if _, err := h.AdminDB.Exec(`
			INSERT INTO events (id, org_id, entity_id, event_type, dedup_key, metadata_json, created_at)
			VALUES ($1, $2, $3, 'github:pr:opened', '', '{}'::jsonb, now())
		`, eventID, orgID, entityID); err != nil {
			t.Fatalf("event: %v", err)
		}
		if _, err := h.AdminDB.Exec(`
			INSERT INTO tasks (id, org_id, creator_user_id, team_id, visibility, entity_id, event_type, dedup_key, primary_event_id, status, scoring_status, priority_score)
			VALUES ($1, $2, $3,
			        (SELECT id FROM teams WHERE org_id = $2 ORDER BY created_at ASC LIMIT 1),
			        'team', $4, 'github:pr:opened', '', $5, 'queued', 'pending', 0.5)
		`, taskID, orgID, userID, entityID, eventID); err != nil {
			t.Fatalf("task: %v", err)
		}
		seedPgConversation(t, h.AdminDB, orgID, domain.Conversation{
			ID: conversationID, TaskID: taskID, PromptID: promptID, Status: "running", Model: "m",
			CreatorUserID:  userID,
			BlueprintRunID: seedPgBlueprintRun(t, h, orgID, userID, taskID),
		})
		return taskID
	}
	convA := uuid.New().String()
	convB := uuid.New().String()
	taskA := mkChain(t, orgA, userA, "p_xleak_A", convA)
	_ = mkChain(t, orgB, userB, "p_xleak_B", convB)

	// Org A's view must NOT see B's conversation.
	if got, err := stores.Conversations.Get(ctx, orgA, convB); err != nil {
		t.Fatalf("Get cross-org: %v", err)
	} else if got != nil {
		t.Errorf("orgA Get returned orgB conversation %s; defense-in-depth filter leaked", convB)
	}
	if got, err := stores.Conversations.Get(ctx, orgB, convA); err != nil {
		t.Fatalf("Get cross-org reverse: %v", err)
	} else if got != nil {
		t.Errorf("orgB Get returned orgA conversation %s", convA)
	}

	// ListForTask scoped to orgB looking at orgA's task must
	// return nothing.
	if convs, err := stores.Conversations.ListForTask(ctx, orgB, taskA); err != nil {
		t.Fatalf("ListForTask cross-org: %v", err)
	} else if len(convs) != 0 {
		t.Errorf("orgB ListForTask(orgA task) returned %d conversations; want 0", len(convs))
	}
}

// TestConversationStore_Postgres_CrossOrgRLSDenied pins the production
// RLS layer for conversations. Where
// TestConversationStore_Postgres_CrossOrgLeakage
// above wires both pools against AdminDB to prove the defense-in-depth
// WHERE-clause filter is intact, this test runs the store through the
// app pool under tf_app with real JWT claims so the actual
// conversations_select / conversations_insert policies are exercised. Same-org reads
// succeed; cross-org reads are silently filtered (USING); cross-org
// Create raises 42501 from conversations_insert WITH CHECK.
func TestConversationStore_Postgres_CrossOrgRLSDenied(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)

	orgA, alice, _ := seedPgConversationOrg(t, h)
	orgB, bob, _ := seedPgConversationOrg(t, h)
	seedPgConversationPromptIn(t, h, "p_rls_A", orgA, alice)
	seedPgConversationPromptIn(t, h, "p_rls_B", orgB, bob)

	// Seed entity + event + task + conversation in orgA via admin so the row
	// exists. Whether bob (claims orgB) can see/mutate it is the
	// question.
	entityA := uuid.New().String()
	eventA := uuid.New().String()
	taskA := uuid.New().String()
	convA := uuid.New().String()
	if _, err := h.AdminDB.Exec(`
		INSERT INTO entities (id, org_id, source, source_id, kind, title, url, snapshot_json, created_at)
		VALUES ($1, $2, 'github', $3, 'pr', 'RLS Cross-org', '', '{}'::jsonb, now())
	`, entityA, orgA, "rls-cross-"+orgA[:8]); err != nil {
		t.Fatalf("entity: %v", err)
	}
	if _, err := h.AdminDB.Exec(`
		INSERT INTO events (id, org_id, entity_id, event_type, dedup_key, metadata_json, created_at)
		VALUES ($1, $2, $3, 'github:pr:ci_check_failed', '', '{}'::jsonb, now())
	`, eventA, orgA, entityA); err != nil {
		t.Fatalf("event: %v", err)
	}
	if _, err := h.AdminDB.Exec(`
		INSERT INTO tasks (id, org_id, creator_user_id, team_id, visibility, entity_id, event_type, dedup_key, primary_event_id, status, scoring_status, priority_score)
		VALUES ($1, $2, $3,
		        (SELECT id FROM teams WHERE org_id = $2 ORDER BY created_at ASC LIMIT 1),
		        'team', $4, 'github:pr:ci_check_failed', '', $5, 'queued', 'pending', 0.5)
	`, taskA, orgA, alice, entityA, eventA); err != nil {
		t.Fatalf("task: %v", err)
	}
	blueprintRunA := seedPgBlueprintRun(t, h, orgA, alice, taskA)
	if _, err := h.AdminDB.Exec(`
		INSERT INTO conversations (id, org_id, task_id, team_id, prompt_id, status, model, creator_user_id, trigger_type, blueprint_run_id)
		VALUES ($1, $2, $3,
		        (SELECT id FROM teams WHERE org_id = $2 ORDER BY created_at ASC LIMIT 1),
		        'p_rls_A', 'running', 'm', $4, 'manual', $5)
	`, convA, orgA, taskA, alice, blueprintRunA); err != nil {
		t.Fatalf("seed conversation: %v", err)
	}
	ctx := context.Background()

	t.Run("same_org_user_can_read", func(t *testing.T) {
		err := h.WithUser(t, alice, orgA, func(tx *sql.Tx) error {
			conv, err := pgstore.NewForTx(tx, pgtest.SecretKey).Conversations.Get(ctx, orgA, convA)
			if err != nil {
				return fmt.Errorf("Get: %w", err)
			}
			if conv == nil {
				t.Errorf("alice Get(orgA, runA) returned nil; same-org RLS USING filter wrongly excluded the row")
			}
			return nil
		})
		if err != nil {
			t.Fatalf("alice path: %v", err)
		}
	})

	t.Run("cross_org_read_filtered", func(t *testing.T) {
		err := h.WithUser(t, bob, orgB, func(tx *sql.Tx) error {
			conv, err := pgstore.NewForTx(tx, pgtest.SecretKey).Conversations.Get(ctx, orgA, convA)
			if err != nil {
				return fmt.Errorf("Get: %w", err)
			}
			if conv != nil {
				t.Errorf("bob Get(orgA, runA) returned %+v; RLS USING filter leaked orgA's conversation to orgB", conv)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("bob read path: %v", err)
		}
	})

	t.Run("cross_org_write_filtered", func(t *testing.T) {
		// The insert path moved to the admin-pool conversation queue (nothing
		// mints conversations under tf_app), so the write arm to pin is
		// an UPDATE: bob's lifecycle write against orgA's conversation must be
		// filtered by the USING clause — no rows touched. Under the
		// returned-row standard that is now db.ErrNoSuchConversation (the
		// RETURNING clause finds nothing to hand back) rather than a silent
		// no-op — the miss is reported, not swallowed.
		var werr error
		err := h.WithUser(t, bob, orgB, func(tx *sql.Tx) error {
			_, werr = pgstore.NewForTx(tx, pgtest.SecretKey).Conversations.SetWorktreePath(ctx, orgA, convA, "/tmp/bob-was-here")
			return nil
		})
		if err != nil {
			t.Fatalf("cross-org SetWorktreePath: %v", err)
		}
		if !errors.Is(werr, db.ErrNoSuchConversation) {
			t.Fatalf("cross-org SetWorktreePath = %v, want db.ErrNoSuchConversation (RLS USING filter hides the row)", werr)
		}
		var wt sql.NullString
		if err := h.AdminDB.QueryRow(`SELECT worktree_path FROM conversations WHERE id = $1`, convA).Scan(&wt); err != nil {
			t.Fatalf("read back: %v", err)
		}
		if wt.Valid && wt.String == "/tmp/bob-was-here" {
			t.Errorf("cross-org UPDATE landed; RLS USING filter leaked orgA's conversation to orgB")
		}
	})
}

// TestConversationStore_Postgres_LifecycleWrites_UnderSyntheticClaims
// pins the routing the delegate spawner uses for manual-conversation
// bookkeeping: lifecycle writes (Complete, ParkOpen,
// MarkQueuedForResume) wrapped in SyntheticClaimsWithTx must pass RLS under
// tf_app and land the expected status. Mirrors the spawner's per-call-site
// branch:
//
//	if triggerType == "manual" {
//	    s.tx.SyntheticClaimsWithTx(...) // this path
//	} else {
//	    s.conversations.XxxSystem(...)
//	}
//
// The admin-pool System variants are already pgtested via the
// conformance suite. This test specifically exercises the app-pool
// arm under realistic RLS — the only way the manual-routing path
// can succeed in multi-mode without the tx wrap is if the
// row's creator_user_id matches tf.current_user_id() under the
// claims set by WithTx.
func TestConversationStore_Postgres_LifecycleWrites_UnderSyntheticClaims(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, userID, _ := seedPgConversationOrg(t, h)
	seedPgConversationPromptIn(t, h, "p_lc_test", orgID, userID)

	// FK chain on admin (same pattern as
	// TestConversationStore_Postgres_CrossOrgRLSDenied).
	entityID := uuid.New().String()
	eventID := uuid.New().String()
	taskID := uuid.New().String()
	if _, err := h.AdminDB.Exec(`
		INSERT INTO entities (id, org_id, source, source_id, kind, title, url, snapshot_json, created_at)
		VALUES ($1, $2, 'github', $3, 'pr', 'LC Test', '', '{}'::jsonb, now())
	`, entityID, orgID, "lc-"+orgID[:8]); err != nil {
		t.Fatalf("entity: %v", err)
	}
	if _, err := h.AdminDB.Exec(`
		INSERT INTO events (id, org_id, entity_id, event_type, dedup_key, metadata_json, created_at)
		VALUES ($1, $2, $3, 'github:pr:ci_check_failed', '', '{}'::jsonb, now())
	`, eventID, orgID, entityID); err != nil {
		t.Fatalf("event: %v", err)
	}
	if _, err := h.AdminDB.Exec(`
		INSERT INTO tasks (id, org_id, creator_user_id, team_id, visibility, entity_id, event_type, dedup_key, primary_event_id, status, scoring_status, priority_score)
		VALUES ($1, $2, $3,
		        (SELECT id FROM teams WHERE org_id = $2 ORDER BY created_at ASC LIMIT 1),
		        'team', $4, 'github:pr:ci_check_failed', '', $5, 'queued', 'pending', 0.5)
	`, taskID, orgID, userID, entityID, eventID); err != nil {
		t.Fatalf("task: %v", err)
	}

	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	ctx := context.Background()

	// Seed a manual conversation row owned by userID — the queue's
	// EnqueueConversation does this in production before any lifecycle
	// write runs.
	lcBlueprintRun := seedPgBlueprintRun(t, h, orgID, userID, taskID)
	conversationID := seedPgConversation(t, h.AdminDB, orgID, domain.Conversation{
		TaskID: taskID, PromptID: "p_lc_test", Status: "running", Model: "m",
		TriggerType: "manual", CreatorUserID: userID,
		BlueprintRunID: lcBlueprintRun,
	})

	// Drive each lifecycle write through SyntheticClaimsWithTx — the
	// shape the spawner uses for every manual-conversation bookkeeping point.

	// ParkOpen (park) then MarkQueuedForResume (resume-by-enqueue) — the
	// open→queued CAS the resume path drives under the user's claims.
	var parked, requeued bool
	if err := stores.Tx.SyntheticClaimsWithTx(ctx, orgID, userID, func(tx db.TxStores) error {
		p, mErr := tx.Conversations.ParkOpen(ctx, orgID, conversationID, db.ParkIdle())
		parked = p
		if mErr != nil {
			return mErr
		}
		r, mErr := tx.Conversations.MarkQueuedForResume(ctx, orgID, conversationID)
		requeued = r
		return mErr
	}); err != nil {
		t.Fatalf("ParkOpen/MarkQueuedForResume under synth claims: %v", err)
	}
	if !parked || !requeued {
		t.Errorf("park/requeue = (%v, %v), want (true, true)", parked, requeued)
	}
	// Back to running for the terminal writes below (the claim path is
	// admin-side; here we only need the status precondition).
	if _, err := h.AdminDB.Exec(`UPDATE conversations SET status = NULL WHERE id = $1`, conversationID); err != nil {
		t.Fatalf("reset status to running: %v", err)
	}

	// Complete twice — two invocation cycles, each going live first (the
	// claim mint) so its streamed row is claim-stamped under RLS, then
	// settling its own cost lump on that row and stamping its telemetry
	// onto the claim it releases; the derived totals then ADD across the
	// cycles.
	settle := func(cost float64, durationMs, numTurns int, resultSummary, outcome string) {
		t.Helper()
		if _, err := stores.Conversations.SetExecutorSystem(ctx, orgID, conversationID, "exec-lc", 1); err != nil {
			t.Fatalf("SetExecutorSystem: %v", err)
		}
		if err := stores.Tx.SyntheticClaimsWithTx(ctx, orgID, userID, func(tx db.TxStores) error {
			_, mErr := tx.Conversations.InsertMessage(ctx, orgID, &domain.Message{
				ConversationID: conversationID, Role: "assistant", Content: "work",
			})
			return mErr
		}); err != nil {
			t.Fatalf("InsertMessage under synth claims: %v", err)
		}
		if err := stores.Tx.SyntheticClaimsWithTx(ctx, orgID, userID, func(tx db.TxStores) error {
			_, err := tx.Conversations.Complete(ctx, orgID, conversationID, "completed", cost, durationMs, numTurns, resultSummary, outcome, "", "")
			return err
		}); err != nil {
			t.Fatalf("Complete under synth claims: %v", err)
		}
	}
	settle(0.5, 1500, 3, "", "")
	settle(0.25, 500, 2, "ok", "finish")

	// Verify through the derived projection: row landed in completed, the
	// totals reflect both cycles, creator stayed the original user.
	got, err := stores.Conversations.GetSystem(ctx, orgID, conversationID)
	if err != nil || got == nil {
		t.Fatalf("GetSystem: err=%v got=%v", err, got)
	}
	if got.Status != "completed" {
		t.Errorf("status = %q, want completed", got.Status)
	}
	if got.TotalCostUSD == nil || *got.TotalCostUSD != 0.75 {
		t.Errorf("total_cost_usd = %v, want 0.75 (0.5 lump + 0.25 lump)", got.TotalCostUSD)
	}
	if got.DurationMs == nil || *got.DurationMs != 2000 {
		t.Errorf("duration_ms = %v, want 2000 (1500 + 500 across claims)", got.DurationMs)
	}
	if got.NumTurns == nil || *got.NumTurns != 5 {
		t.Errorf("num_turns = %v, want 5 (3 + 2 across claims)", got.NumTurns)
	}
	// A terminal writes no park reason — it did not park, and the model's own
	// stop reason is a per-turn fact on the messages rows.
	if got.ParkReason != "" {
		t.Errorf("park_reason = %q after two terminals, want empty", got.ParkReason)
	}
	if got.CreatorUserID != userID {
		t.Errorf("creator_user_id = %v, want %s", got.CreatorUserID, userID)
	}

	// MarkFailedIfActive on a terminal row is a no-op (guarded
	// transition). Verifies the System variant's guard fires even
	// though we never wrapped in claims for this call (spawner uses
	// it goroutine-internally with no user identity).
	failed, err := stores.Conversations.MarkFailedIfActiveSystem(ctx, orgID, conversationID, "")
	if err != nil {
		t.Fatalf("MarkFailedIfActiveSystem: %v", err)
	}
	if failed {
		t.Errorf("MarkFailedIfActiveSystem on terminal row: flipped=true, want false (guard)")
	}
}

// seedPgConversation inserts a conversations row directly — the test
// fixture stand-in for the queue's EnqueueConversation mint, staging rows in
// arbitrary status. The trigger_type↔creator CHECK is satisfied by the
// caller: manual rows carry CreatorUserID, event rows leave it empty.
func seedPgConversation(t *testing.T, conn *sql.DB, orgID string, conv domain.Conversation) string {
	t.Helper()
	id := conv.ID
	if id == "" {
		id = uuid.New().String()
	}
	trigger := conv.TriggerType
	if trigger == "" {
		trigger = "manual"
	}
	var stepIdx any
	if conv.BlueprintStepIndex != nil {
		stepIdx = *conv.BlueprintStepIndex
	}
	var creator any
	if conv.CreatorUserID != "" {
		creator = conv.CreatorUserID
	}
	// team_id defaults to the org's first team; a conversation staged for a
	// team-narrowing test names its own.
	if _, err := conn.Exec(`
		INSERT INTO conversations (id, org_id, task_id, team_id, prompt_id, status, model,
		                           trigger_type, trigger_id, visibility, creator_user_id,
		                           blueprint_run_id, blueprint_step_index)
		VALUES ($1, $2, $3,
		        COALESCE(NULLIF($12, '')::uuid,
		                 (SELECT id FROM teams WHERE org_id = $2 ORDER BY created_at ASC LIMIT 1)),
		        $4, $5, $6, $7, NULLIF($8, '')::uuid, 'team', $9, $10, $11)
	`, id, orgID, conv.TaskID, conv.PromptID, conv.Status, conv.Model,
		trigger, conv.TriggerID, creator, conv.BlueprintRunID, stepIdx, conv.TeamID); err != nil {
		t.Fatalf("seed conversation %s: %v", id, err)
	}
	return id
}

// seedPgBlueprintRun mints a blueprint + blueprint_run pointed at the
// given task so a standalone conversations insert can satisfy the origin
// CHECK's blueprint_run_id FK (→ blueprint_runs(id)). Mirrors the
// conformance seeder's BlueprintRun, but exposed as a plain helper for
// the RLS/cross-org tests that seed conversations outside the conformance
// suite. Postgres requires org_id on both rows; trigger_type='manual' also
// requires a non-NULL creator_user_id
// (blueprint_runs_creator_matches_trigger_type CHECK).
func seedPgBlueprintRun(t *testing.T, h *pgtest.Harness, orgID, userID, taskID string) string {
	t.Helper()
	bpID := uuid.New().String()
	if _, err := h.AdminDB.Exec(`
		INSERT INTO blueprints (id, org_id, creator_user_id, team_id, name, source)
		VALUES ($1, $2, $3,
		        (SELECT id FROM teams WHERE org_id = $2 ORDER BY created_at ASC LIMIT 1),
		        'Conformance BP', 'user')
	`, bpID, orgID, userID); err != nil {
		t.Fatalf("seed blueprint: %v", err)
	}
	brID := uuid.New().String()
	if _, err := h.AdminDB.Exec(`
		INSERT INTO blueprint_runs (id, org_id, creator_user_id, blueprint_id, task_id, trigger_type, status, worktree_path, step_plan)
		VALUES ($1, $2, $3, $4, $5, 'manual', 'running', '/tmp/wt', '[]')
	`, brID, orgID, userID, bpID, taskID); err != nil {
		t.Fatalf("seed blueprint_run: %v", err)
	}
	return brID
}

// TestConversationStore_Postgres_RuntimeDefaultsToSDK pins the
// conversations.runtime schema fact the columnar-canon epic depends on: a
// freshly seeded delegation row lands as 'sdk', not the native loop's
// 'native'. domain.Conversation has no write path for Runtime (nothing writes
// 'native' until the executor-side loop lands), so this reads the column
// directly.
func TestConversationStore_Postgres_RuntimeDefaultsToSDK(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, userID, _ := seedPgConversationOrg(t, h)
	seedPgConversationPromptIn(t, h, "p_runtime_test", orgID, userID)

	entityID := uuid.New().String()
	eventID := uuid.New().String()
	taskID := uuid.New().String()
	if _, err := h.AdminDB.Exec(`
		INSERT INTO entities (id, org_id, source, source_id, kind, title, url, snapshot_json, created_at)
		VALUES ($1, $2, 'github', $3, 'pr', 'Runtime Test', '', '{}'::jsonb, now())
	`, entityID, orgID, "runtime-"+orgID[:8]); err != nil {
		t.Fatalf("entity: %v", err)
	}
	if _, err := h.AdminDB.Exec(`
		INSERT INTO events (id, org_id, entity_id, event_type, dedup_key, metadata_json, created_at)
		VALUES ($1, $2, $3, 'github:pr:opened', '', '{}'::jsonb, now())
	`, eventID, orgID, entityID); err != nil {
		t.Fatalf("event: %v", err)
	}
	if _, err := h.AdminDB.Exec(`
		INSERT INTO tasks (id, org_id, creator_user_id, team_id, visibility, entity_id, event_type, dedup_key, primary_event_id, status, scoring_status, priority_score)
		VALUES ($1, $2, $3,
		        (SELECT id FROM teams WHERE org_id = $2 ORDER BY created_at ASC LIMIT 1),
		        'team', $4, 'github:pr:opened', '', $5, 'queued', 'pending', 0.5)
	`, taskID, orgID, userID, entityID, eventID); err != nil {
		t.Fatalf("task: %v", err)
	}

	conversationID := seedPgConversation(t, h.AdminDB, orgID, domain.Conversation{
		TaskID: taskID, PromptID: "p_runtime_test", Status: "running", Model: "m",
		CreatorUserID:  userID,
		BlueprintRunID: seedPgBlueprintRun(t, h, orgID, userID, taskID),
	})

	var runtime string
	if err := h.AdminDB.QueryRow(`SELECT runtime FROM conversations WHERE id = $1`, conversationID).Scan(&runtime); err != nil {
		t.Fatalf("read runtime: %v", err)
	}
	if runtime != "sdk" {
		t.Errorf("runtime = %q, want sdk", runtime)
	}
}

// seedPgConversationPromptIn is a small variant that inserts a prompt
// with an explicit id. Used by cross-org leakage which needs two
// prompts in two orgs with distinct ids.
func seedPgConversationPromptIn(t *testing.T, h *pgtest.Harness, id, orgID, userID string) {
	t.Helper()
	teamID := firstTeamForOrg(t, h, orgID)
	if _, err := h.AdminDB.Exec(`
		INSERT INTO prompts (id, org_id, creator_user_id, team_id, name, body, source, allowed_tools, created_at, updated_at)
		VALUES ($1, $2, $3, $4, 'X-leak Test', 'body', 'user', '', now(), now())
	`, id, orgID, userID, teamID); err != nil {
		t.Fatalf("seed prompt %s: %v", id, err)
	}
}

// TestConversationStore_Postgres_HandOffGuardHoldsForANonCreator puts the
// hand-off guard under the pool it actually runs on, as the principal it is
// most likely to be wrong for.
//
// blueprint_runs_select is creator-scoped for manual runs, so a teammate who
// may legitimately update a team-visible conversation sees NO row for its
// blueprint. Written as a correlated subquery, the guard reads that emptiness
// as "no blueprint is running" and lets the flip through — reopening the
// window for everyone except the person who started the sequence, which is
// the shape a conformance suite wired to the admin pool cannot see.
//
// Both directions are asserted, because a guard that refuses everyone would
// pass the first half and silently break teammate follow-up.
func TestConversationStore_Postgres_HandOffGuardHoldsForANonCreator(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	ctx := context.Background()

	orgID, creator, teamID := pgtest.SeedOrgWithUser(t, h, "creator")
	teammate := seedPgMember(t, h, orgID, "teammate", "member")
	pgtest.MustExec(t, h.AdminDB,
		`INSERT INTO memberships (user_id, team_id, role) VALUES ($1, $2, 'member')`, teammate, teamID)
	seedPgConversationPromptIn(t, h, "p_handoff_rls", orgID, creator)

	entityID, eventID, taskID := uuid.New().String(), uuid.New().String(), uuid.New().String()
	pgtest.MustExec(t, h.AdminDB, `
		INSERT INTO entities (id, org_id, source, source_id, kind, title, url, snapshot_json, created_at)
		VALUES ($1, $2, 'github', $3, 'pr', 'Handoff RLS', '', '{}'::jsonb, now())
	`, entityID, orgID, "handoff-"+orgID[:8])
	pgtest.MustExec(t, h.AdminDB, `
		INSERT INTO events (id, org_id, entity_id, event_type, dedup_key, metadata_json, created_at)
		VALUES ($1, $2, $3, 'github:pr:ci_check_failed', '', '{}'::jsonb, now())
	`, eventID, orgID, entityID)
	pgtest.MustExec(t, h.AdminDB, `
		INSERT INTO tasks (id, org_id, creator_user_id, team_id, visibility, entity_id, event_type, dedup_key,
		                   primary_event_id, status, scoring_status, priority_score)
		VALUES ($1, $2, $3, $4, 'team', $5, 'github:pr:ci_check_failed', '', $6, 'queued', 'pending', 0.5)
	`, taskID, orgID, creator, teamID, entityID, eventID)

	// The creator's manual blueprint, still running, and its concluded step:
	// the hand-off window, exactly as the reactor would find it.
	brID := seedPgBlueprintRun(t, h, orgID, creator, taskID)
	stepIdx := 0
	convID := seedPgConversation(t, h.AdminDB, orgID, domain.Conversation{
		TaskID: taskID, PromptID: "p_handoff_rls", Status: "completed", Model: "m",
		TriggerType: "manual", CreatorUserID: creator,
		BlueprintRunID: brID, BlueprintStepIndex: &stepIdx,
	})

	// The precondition this test rests on: the teammate genuinely cannot see
	// the blueprint row. Without it the assertions below would pass for the
	// wrong reason.
	if err := h.WithUser(t, teammate, orgID, func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM blueprint_runs WHERE id = $1`, brID).Scan(&n); err != nil {
			return err
		}
		if n != 0 {
			t.Errorf("the teammate can see %d blueprint_runs rows; this test needs the creator-scoped policy to hide it", n)
		}
		return nil
	}); err != nil {
		t.Fatalf("read blueprint_runs as the teammate: %v", err)
	}

	// The wake, as the teammate, in the window. Refused — the guard reads the
	// blueprint's state whoever is asking.
	var flipped bool
	if err := stores.Tx.SyntheticClaimsWithTx(ctx, orgID, teammate, func(tx db.TxStores) error {
		f, mErr := tx.Conversations.MarkQueuedForResume(ctx, orgID, convID)
		flipped = f
		return mErr
	}); err != nil {
		t.Fatalf("MarkQueuedForResume as a non-creator: %v", err)
	}
	if flipped {
		t.Error("a non-creator's wake flipped a concluded step of a running blueprint; the guard fell open on a row RLS hides")
	}
	var status string
	if err := h.AdminDB.QueryRow(`SELECT status FROM conversations WHERE id = $1`, convID).Scan(&status); err != nil {
		t.Fatalf("read status: %v", err)
	}
	if status != "completed" {
		t.Errorf("status = %q, want completed — a refused CAS writes nothing", status)
	}

	// Once the blueprint stops, the same teammate's follow-up lands. The guard
	// is about the sequence's state, never about who can see it.
	pgtest.MustExec(t, h.AdminDB, `UPDATE blueprint_runs SET status = 'completed' WHERE id = $1`, brID)
	if err := stores.Tx.SyntheticClaimsWithTx(ctx, orgID, teammate, func(tx db.TxStores) error {
		f, mErr := tx.Conversations.MarkQueuedForResume(ctx, orgID, convID)
		flipped = f
		return mErr
	}); err != nil {
		t.Fatalf("MarkQueuedForResume after the blueprint settled: %v", err)
	}
	if !flipped {
		t.Error("the teammate's follow-up was refused after the blueprint finished; the guard must not fail closed on an invisible row")
	}
}

// TestConversationStore_Postgres_ResumeStampsTheWarmExecutorForANonCreator
// pins the resume flip's affinity stamp against the trap its sibling guard
// fell into: this statement runs on the app pool, so a subquery over a
// creator-scoped table would read empty for a teammate and the flip would
// silently stamp NULL — sending every teammate follow-up to a cold executor
// while the warm tree sat idle, and doing it invisibly, since NULL is also
// the legitimate answer for a conversation nothing ever drove.
//
// claims is not creator-scoped — its policy composes through the
// conversation, so anyone who may update the row can read its claims — which
// is what makes a plain correlated subquery the right shape here. The
// precondition is asserted first, because that is the fact the subquery rests
// on rather than something the assertion below would reveal on its own.
func TestConversationStore_Postgres_ResumeStampsTheWarmExecutorForANonCreator(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	ctx := context.Background()

	orgID, creator, teamID := pgtest.SeedOrgWithUser(t, h, "creator")
	teammate := seedPgMember(t, h, orgID, "teammate", "member")
	pgtest.MustExec(t, h.AdminDB,
		`INSERT INTO memberships (user_id, team_id, role) VALUES ($1, $2, 'member')`, teammate, teamID)
	seedPgConversationPromptIn(t, h, "p_resume_affinity_rls", orgID, creator)

	entityID, eventID, taskID := uuid.New().String(), uuid.New().String(), uuid.New().String()
	pgtest.MustExec(t, h.AdminDB, `
		INSERT INTO entities (id, org_id, source, source_id, kind, title, url, snapshot_json, created_at)
		VALUES ($1, $2, 'github', $3, 'pr', 'Resume Affinity RLS', '', '{}'::jsonb, now())
	`, entityID, orgID, "resume-affinity-"+orgID[:8])
	pgtest.MustExec(t, h.AdminDB, `
		INSERT INTO events (id, org_id, entity_id, event_type, dedup_key, metadata_json, created_at)
		VALUES ($1, $2, $3, 'github:pr:ci_check_failed', '', '{}'::jsonb, now())
	`, eventID, orgID, entityID)
	pgtest.MustExec(t, h.AdminDB, `
		INSERT INTO tasks (id, org_id, creator_user_id, team_id, visibility, entity_id, event_type, dedup_key,
		                   primary_event_id, status, scoring_status, priority_score)
		VALUES ($1, $2, $3, $4, 'team', $5, 'github:pr:ci_check_failed', '', $6, 'queued', 'pending', 0.5)
	`, taskID, orgID, creator, teamID, entityID, eventID)

	// The creator's team-visible conversation, driven by one engagement and
	// then stopped: a warm tree on exec-warm and a released claim naming it.
	brID := seedPgBlueprintRun(t, h, orgID, creator, taskID)
	stepIdx := 0
	convID := seedPgConversation(t, h.AdminDB, orgID, domain.Conversation{
		TaskID: taskID, PromptID: "p_resume_affinity_rls", Status: "running", Model: "m",
		TriggerType: "manual", CreatorUserID: creator,
		BlueprintRunID: brID, BlueprintStepIndex: &stepIdx,
	})
	if _, err := stores.Conversations.SetExecutorSystem(ctx, orgID, convID, "exec-warm", 1); err != nil {
		t.Fatalf("mint the engagement: %v", err)
	}
	if ok, err := stores.Conversations.ParkOpenSystem(ctx, orgID, convID, db.ParkStopped(domain.ParkReasonUserCancelled, "")); err != nil || !ok {
		t.Fatalf("park: ok=%v err=%v", ok, err)
	}

	// The precondition the subquery rests on: the teammate really can read
	// the claim, unlike the blueprint row its sibling guard has to route past.
	if err := h.WithUser(t, teammate, orgID, func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM claims WHERE conversation_id = $1`, convID).Scan(&n); err != nil {
			return err
		}
		if n != 1 {
			t.Errorf("the teammate sees %d claims rows, want 1; claims visibility must compose through the conversation", n)
		}
		return nil
	}); err != nil {
		t.Fatalf("read claims as the teammate: %v", err)
	}

	// The teammate's follow-up wakes it — and routes it back to the warm tree.
	var flipped bool
	if err := stores.Tx.SyntheticClaimsWithTx(ctx, orgID, teammate, func(tx db.TxStores) error {
		f, mErr := tx.Conversations.MarkQueuedForResume(ctx, orgID, convID)
		flipped = f
		return mErr
	}); err != nil {
		t.Fatalf("MarkQueuedForResume as a non-creator: %v", err)
	}
	if !flipped {
		t.Fatal("the teammate's follow-up was refused on a team-visible parked conversation")
	}
	var preferred sql.NullString
	if err := h.AdminDB.QueryRow(
		`SELECT preferred_executor_id FROM conversations WHERE id = $1`, convID).Scan(&preferred); err != nil {
		t.Fatalf("read preferred_executor_id: %v", err)
	}
	if preferred.String != "exec-warm" {
		t.Errorf("preferred_executor_id = %q, want exec-warm — a teammate's resume must chase the same warm tree the creator's would", preferred.String)
	}
}

// TestConversationStore_Postgres_PRCoherenceTargets runs the PR coherence
// conformance suite against the Postgres impl.
func TestConversationStore_Postgres_PRCoherenceTargets(t *testing.T) {
	h := pgtest.Shared(t)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)

	dbtest.RunPRCoherenceTargetsConformance(t, func(t *testing.T) (db.ConversationStore, string, dbtest.ConversationSeeder, dbtest.PRCoherenceSeeder) {
		t.Helper()
		h.Reset(t)
		orgID, userID, agentID := seedPgConversationOrg(t, h)
		promptID := seedPgConversationPrompt(t, h, orgID, userID)
		teamID := firstTeamForOrg(t, h, orgID)
		ctx := context.Background()
		extra := dbtest.PRCoherenceSeeder{
			PendingReview: func(t *testing.T, conversationID, repo string, prNumber int) {
				t.Helper()
				a := domain.NewReviewArtifact(repo, prNumber, "head-sha", conversationID)
				a.ConversationID = conversationID
				a.OrgID, a.TeamID = orgID, teamID
				if _, err := stores.Artifacts.UpsertSystem(ctx, orgID, a); err != nil {
					t.Fatalf("seed pending review: %v", err)
				}
			},
			Worktree: func(t *testing.T, conversationID, slug, ref string) {
				t.Helper()
				if _, err := stores.Repos.GetOrCreateSystem(ctx, orgID, domain.RepoRefFromSlug(slug)); err != nil {
					t.Fatalf("seed repository %s: %v", slug, err)
				}
				if _, _, err := stores.ConversationWorktrees.InsertSystem(ctx, orgID, domain.ConversationWorktree{
					ConversationID: conversationID, RepoID: slug, Ref: ref,
					Path: "/tmp/coherence/" + conversationID + "/" + ref,
				}); err != nil {
					t.Fatalf("seed worktree: %v", err)
				}
			},
			MarkEventInjected: func(t *testing.T, taskID, eventID string) {
				t.Helper()
				if _, err := h.AdminDB.Exec(`
					INSERT INTO task_events (task_id, event_id, kind, org_id) VALUES ($1, $2, 'injected', $3)
				`, taskID, eventID, orgID); err != nil {
					t.Fatalf("record injected task event: %v", err)
				}
			},
			ActiveClaim: func(t *testing.T, conversationID string) {
				t.Helper()
				seedPgActiveClaim(t, h, orgID, conversationID, "executor-coherence", 1)
			},
		}
		seeder := newPgConversationSeeder(h.AdminDB, orgID, userID, agentID, promptID)
		return stores.Conversations, orgID, seeder, extra
	})
}
