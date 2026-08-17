package postgres_test

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TestSpendStore_Postgres runs the shared SpendStore conformance suite against
// the Postgres impl. Like the PromptStore conformance, the store is wired
// admin-on-both (pgstore.New(h.AdminDB, h.AdminDB, …)) so reads bypass RLS —
// the goal here is to prove the llm_spend view's SQL (category derivation,
// native tokens, COALESCE, the UNION shape) matches SQLite's, not to re-test
// RLS. RLS through the view gets its own dedicated test below, where it
// genuinely matters. Skips cleanly without Docker via the pgtest harness.
func TestSpendStore_Postgres(t *testing.T) {
	h := pgtest.Shared(t)

	dbtest.RunSpendStoreConformance(t, func(t *testing.T) dbtest.SpendStoreFixture {
		t.Helper()
		h.Reset(t)
		orgID, userID, teamID, agentID, projectID, nullTeamProjectID, triggerID := seedPgSpendOrg(t, h)
		// admin-on-both: bypass RLS for SQL-shape coverage (the FK + NOT NULL
		// + CHECK constraints still apply, so it's the same SQL the app pool
		// runs). RLS is exercised separately in TestSpendStore_Postgres_RLS_*.
		stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
		return dbtest.SpendStoreFixture{
			Store:     stores.Spend,
			OrgID:     orgID,
			TeamID:    teamID,
			UserID:    userID,
			AgentID:   agentID,
			TriggerID: triggerID,
			Seeder:    newPgSpendSeeder(h.AdminDB, orgID, projectID, nullTeamProjectID),
		}
	})
}

// TestSpendStore_Postgres_RLS_ViewSecurityInvoker is the critical test: it reads
// the llm_spend view through the APP pool (tf_app) with real JWT claims, so the
// view's security_invoker=true makes the base tables' RLS apply under the
// querying user. It proves the spine doesn't leak spend across the RLS boundary:
//
//   - runs are TEAM-scoped: a teamA member sees teamA's run, not teamB's.
//   - curator is CREATOR-scoped (curator_requests_select gates on
//     creator_user_id): a user sees their own curator turns, not a peer's.
//     (The epic's "org scope" shorthand for curator is looser than the actual
//     policy; the view faithfully inherits whatever the base RLS is.)
//   - system is ORG-scoped: every org member sees the system_overhead row.
//   - a different org sees none of this org's spend.
//
// If the view were security_definer (or plain, pre-PG15 semantics), base-table
// RLS would evaluate as the view owner and every one of these would leak.
func TestSpendStore_Postgres_RLS_ViewSecurityInvoker(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)

	// orgA: alice founds it as a teamA admin; bob is a member of a sibling
	// teamB. orgB is an unrelated org (carol).
	orgA, alice, teamA := pgtest.SeedOrgWithUser(t, h, "alice")
	teamB := pgtest.SeedTeam(t, h, orgA, "teamB")
	bob := pgtest.SeedUser(t, h, "bob")
	pgtest.AddOrgMember(t, h, bob, orgA, teamB, "member", "member")
	orgB, carol, _ := pgtest.SeedOrgWithUser(t, h, "carol")

	// orgA fixtures: an agent + a team project the delegation/curator
	// conversations FK into, plus
	// a null-team org project. The RLS curator fixtures below use the default
	// (null-team) project — curator visibility is creator-scoped regardless of
	// team_id, so the snapshot value (NULL here) doesn't affect these assertions.
	agentA := uuid.New().String()
	pgtest.MustExec(t, h.AdminDB, `INSERT INTO agents (id, org_id) VALUES ($1, $2)`, agentA, orgA)
	projectA := uuid.New().String()
	pgtest.MustExec(t, h.AdminDB,
		`INSERT INTO projects (id, org_id, creator_user_id, team_id, name, visibility) VALUES ($1, $2, $3, $4, 'spend-rls', 'team')`,
		projectA, orgA, alice, teamA)
	projectAOrg := uuid.New().String()
	pgtest.MustExec(t, h.AdminDB,
		`INSERT INTO projects (id, org_id, creator_user_id, team_id, name, visibility) VALUES ($1, $2, $3, NULL, 'spend-rls-org', 'org')`,
		projectAOrg, orgA, alice)

	seeder := newPgSpendSeeder(h.AdminDB, orgA, projectA, projectAOrg)
	convA := seeder.Conversation(t, dbtest.ConversationSpendFixture{TeamID: teamA, CreatorUserID: alice, TriggerType: "manual", ActorAgentID: agentA, Model: "m", Cost: float64Ptr(1.0), Tokens: dbtest.SpendTokens{Input: 1, Output: 1, CacheRead: 1, CacheCreation: 1}, Status: "completed", StartedAt: spendTestTime})
	convB := seeder.Conversation(t, dbtest.ConversationSpendFixture{TeamID: teamB, CreatorUserID: bob, TriggerType: "manual", ActorAgentID: agentA, Model: "m", Cost: float64Ptr(2.0), Tokens: dbtest.SpendTokens{Input: 2, Output: 2, CacheRead: 2, CacheCreation: 2}, Status: "completed", StartedAt: spendTestTime})
	curatorAlice := seeder.Curator(t, dbtest.CuratorSpendFixture{CreatorUserID: alice, Cost: 0.3, Tokens: dbtest.SpendTokens{Input: 3, Output: 3, CacheRead: 3, CacheCreation: 3}, Status: "completed", CreatedAt: spendTestTime})
	curatorBob := seeder.Curator(t, dbtest.CuratorSpendFixture{CreatorUserID: bob, Cost: 0.4, Tokens: dbtest.SpendTokens{Input: 4, Output: 4, CacheRead: 4, CacheCreation: 4}, Status: "completed", CreatedAt: spendTestTime})
	systemA := seeder.System(t, dbtest.SystemSpendFixture{Job: "scorer", Model: "m", Cost: 0.05, Tokens: dbtest.SpendTokens{Input: 5, Output: 5, CacheRead: 5, CacheCreation: 5}, StartedAt: spendTestTime})

	// alice (teamA): her team's run, her curator turn, the org system row;
	// NOT teamB's run, NOT bob's curator turn.
	aliceVisible := spendVisibleIDs(t, h, alice, orgA)
	assertSpendVisible(t, "alice", aliceVisible, map[string]bool{
		convA: true, curatorAlice: true, systemA: true,
		convB: false, curatorBob: false,
	})

	// bob (teamB): mirror image — teamB's run, his curator turn, the org system
	// row; NOT teamA's run, NOT alice's curator turn.
	bobVisible := spendVisibleIDs(t, h, bob, orgA)
	assertSpendVisible(t, "bob", bobVisible, map[string]bool{
		convB: true, curatorBob: true, systemA: true,
		convA: false, curatorAlice: false,
	})

	// carol (orgB) querying her own (empty) org sees nothing...
	if got := spendVisibleIDs(t, h, carol, orgB); len(got) != 0 {
		t.Errorf("carol(orgB) saw %d spend rows, want 0 (empty org)", len(got))
	}
	// ...and even pointed at orgA's id she sees nothing: RLS pins
	// current_org_id() to orgB, so orgA's rows never match.
	crossOrg := spendVisibleIDs(t, h, carol, orgA)
	assertSpendVisible(t, "carol-cross-org", crossOrg, map[string]bool{
		convA: false, convB: false, curatorAlice: false, curatorBob: false, systemA: false,
	})
}

// TestSpendStore_Postgres_SpendByCategorySystem_BypassesRLS pins the
// load-bearing TFAC-477 fix: SpendByCategorySystem reads the ADMIN pool, so it
// sums spend org-wide across every team, whereas the app-pool SpendByCategory
// (under a team member's claims) only sees that member's own team. The safety
// cap MUST use the System variant — Spawner.Delegate is claims-less, so an
// app-pool read there would see nothing and the cap would never trip.
//
// orgA has teamA (alice) + teamB (bob), each with one manual run. The org-wide
// System aggregate must equal the sum of both ($1 + $2 = $3); the app-pool read
// under alice must see only teamA's $1.
func TestSpendStore_Postgres_SpendByCategorySystem_BypassesRLS(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)

	orgA, alice, teamA := pgtest.SeedOrgWithUser(t, h, "spend-sys")
	teamB := pgtest.SeedTeam(t, h, orgA, "teamB")
	bob := pgtest.SeedUser(t, h, "spend-sys-bob")
	pgtest.AddOrgMember(t, h, bob, orgA, teamB, "member", "member")

	agentA := uuid.New().String()
	pgtest.MustExec(t, h.AdminDB, `INSERT INTO agents (id, org_id) VALUES ($1, $2)`, agentA, orgA)
	projectA := uuid.New().String()
	pgtest.MustExec(t, h.AdminDB,
		`INSERT INTO projects (id, org_id, creator_user_id, team_id, name, visibility) VALUES ($1, $2, $3, $4, 'spend-sys', 'team')`,
		projectA, orgA, alice, teamA)

	// Runs only (team_id set directly on each run), so the seeder's curator
	// project args (team-attributed / null-team, TFAC-476) are never exercised —
	// pass projectA for both.
	seeder := newPgSpendSeeder(h.AdminDB, orgA, projectA, projectA)
	seeder.Conversation(t, dbtest.ConversationSpendFixture{TeamID: teamA, CreatorUserID: alice, TriggerType: "manual", ActorAgentID: agentA, Model: "m", Cost: float64Ptr(1.0), Tokens: dbtest.SpendTokens{Input: 1, Output: 1, CacheRead: 1, CacheCreation: 1}, Status: "completed", StartedAt: spendTestTime})
	seeder.Conversation(t, dbtest.ConversationSpendFixture{TeamID: teamB, CreatorUserID: bob, TriggerType: "manual", ActorAgentID: agentA, Model: "m", Cost: float64Ptr(2.0), Tokens: dbtest.SpendTokens{Input: 2, Output: 2, CacheRead: 2, CacheCreation: 2}, Status: "completed", StartedAt: spendTestTime})

	// Admin-pool System read: org-wide, sees both teams → manual = $3.
	adminStore := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	sysBuckets, err := adminStore.Spend.SpendByCategorySystem(context.Background(), orgA, time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("SpendByCategorySystem: %v", err)
	}
	if got := manualBucketCost(sysBuckets); got != 3.0 {
		t.Errorf("SpendByCategorySystem manual sum = %v, want 3.0 (teamA $1 + teamB $2, org-wide)", got)
	}

	// App-pool read under alice (teamA member): RLS scopes to teamA → manual = $1.
	var appManual float64
	if err := h.WithUser(t, alice, orgA, func(tx *sql.Tx) error {
		b, e := pgstore.NewForTx(tx, pgtest.SecretKey).Spend.SpendByCategory(context.Background(), orgA, time.Time{}, time.Time{})
		if e != nil {
			return e
		}
		appManual = manualBucketCost(b)
		return nil
	}); err != nil {
		t.Fatalf("WithUser SpendByCategory: %v", err)
	}
	if appManual != 1.0 {
		t.Errorf("app-pool SpendByCategory under alice = %v, want 1.0 (teamA only; teamB excluded by RLS)", appManual)
	}
}

// TestSpendStore_Postgres_ListSpendSystem_BypassesRLS pins the TFAC-478
// cross-RLS read: ListSpendSystem (admin pool) returns rows across every team,
// whereas the app-pool ListSpend under a team member's claims sees only their
// own team. The role-gated team / org usage endpoints depend on this — an org
// admin inspecting a team they don't belong to would otherwise see nothing.
func TestSpendStore_Postgres_ListSpendSystem_BypassesRLS(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)

	orgA, alice, teamA := pgtest.SeedOrgWithUser(t, h, "spend-ls-sys")
	teamB := pgtest.SeedTeam(t, h, orgA, "teamB")
	bob := pgtest.SeedUser(t, h, "spend-ls-bob")
	pgtest.AddOrgMember(t, h, bob, orgA, teamB, "member", "member")

	agentA := uuid.New().String()
	pgtest.MustExec(t, h.AdminDB, `INSERT INTO agents (id, org_id) VALUES ($1, $2)`, agentA, orgA)
	projectA := uuid.New().String()
	pgtest.MustExec(t, h.AdminDB,
		`INSERT INTO projects (id, org_id, creator_user_id, team_id, name, visibility) VALUES ($1, $2, $3, $4, 'spend-ls', 'team')`,
		projectA, orgA, alice, teamA)

	// Runs only (team_id set directly), so the curator project args are unused.
	seeder := newPgSpendSeeder(h.AdminDB, orgA, projectA, projectA)
	convA := seeder.Conversation(t, dbtest.ConversationSpendFixture{TeamID: teamA, CreatorUserID: alice, TriggerType: "manual", ActorAgentID: agentA, Model: "m", Cost: float64Ptr(1.0), Tokens: dbtest.SpendTokens{Input: 1, Output: 1, CacheRead: 1, CacheCreation: 1}, Status: "completed", StartedAt: spendTestTime})
	convB := seeder.Conversation(t, dbtest.ConversationSpendFixture{TeamID: teamB, CreatorUserID: bob, TriggerType: "manual", ActorAgentID: agentA, Model: "m", Cost: float64Ptr(2.0), Tokens: dbtest.SpendTokens{Input: 2, Output: 2, CacheRead: 2, CacheCreation: 2}, Status: "completed", StartedAt: spendTestTime})

	// Admin-pool System read: org-wide, sees BOTH teams' runs.
	adminStore := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	sysRows, err := adminStore.Spend.ListSpendSystem(context.Background(), orgA, domain.SpendFilter{})
	if err != nil {
		t.Fatalf("ListSpendSystem: %v", err)
	}
	sysIDs := map[string]bool{}
	for _, r := range sysRows {
		sysIDs[r.SourceID] = true
	}
	if !sysIDs[convA] || !sysIDs[convB] {
		t.Errorf("ListSpendSystem saw %v, want both runA %s + runB %s (org-wide)", sysIDs, convA, convB)
	}

	// App-pool ListSpend under alice (teamA member): RLS scopes to teamA → only runA.
	aliceVisible := spendVisibleIDs(t, h, alice, orgA)
	if !aliceVisible[convA] || aliceVisible[convB] {
		t.Errorf("app-pool ListSpend under alice saw %v, want runA %s only (runB excluded by RLS)", aliceVisible, convA)
	}
}

// manualBucketCost returns the 'manual' category bucket's total cost, or 0 if
// absent.
func manualBucketCost(buckets []domain.SpendBucket) float64 {
	for _, b := range buckets {
		if b.Category == domain.SpendCategoryManual {
			return b.TotalCostUSD
		}
	}
	return 0
}

// TestSpendView_Postgres_IsSecurityInvoker is the regression guard for the
// load-bearing storage option: if a future schema edit drops
// security_invoker=true (or flips the view to security_definer), base-table RLS
// would evaluate as the view owner and the RLS test above would pass while
// production leaked. Pin the catalog fact directly.
func TestSpendView_Postgres_IsSecurityInvoker(t *testing.T) {
	h := pgtest.Shared(t)

	var ok bool
	if err := h.AdminDB.QueryRow(`
		SELECT EXISTS (
			SELECT 1 FROM pg_class
			WHERE relname = 'llm_spend' AND relkind = 'v'
			  AND 'security_invoker=true' = ANY(reloptions)
		)
	`).Scan(&ok); err != nil {
		t.Fatalf("probe pg_class.reloptions: %v", err)
	}
	if !ok {
		t.Fatal("llm_spend is not security_invoker=true — base-table RLS would evaluate as the view owner, leaking cross-team/cross-org spend")
	}
}

// spendTestTime is a fixed terminal timestamp for the RLS fixtures (the RLS test
// asserts visibility, not time windowing, so one shared instant is fine).
var spendTestTime = time.Date(2026, 6, 20, 10, 0, 0, 0, time.UTC)

// seedPgSpendOrg bootstraps an org + user + default team + agent + project for
// the conformance fixture, all via the admin pool. Returns the ids the seeder
// and the suite thread through the three source tables.
func seedPgSpendOrg(t *testing.T, h *pgtest.Harness) (orgID, userID, teamID, agentID, projectID, nullTeamProjectID, triggerID string) {
	t.Helper()
	orgID, userID, teamID = pgtest.SeedOrgWithUser(t, h, "spend-"+uuid.NewString()[:8])
	agentID = uuid.New().String()
	pgtest.MustExec(t, h.AdminDB, `INSERT INTO agents (id, org_id) VALUES ($1, $2)`, agentID, orgID)
	projectID = uuid.New().String()
	pgtest.MustExec(t, h.AdminDB,
		`INSERT INTO projects (id, org_id, creator_user_id, team_id, name, visibility) VALUES ($1, $2, $3, $4, 'spend-test', 'team')`,
		projectID, orgID, userID, teamID)
	// A null-team, org-visibility project for the curator null-team case (TFAC-476):
	// team_id NULL is valid for visibility <> 'team'.
	nullTeamProjectID = uuid.New().String()
	pgtest.MustExec(t, h.AdminDB,
		`INSERT INTO projects (id, org_id, creator_user_id, team_id, name, visibility) VALUES ($1, $2, $3, NULL, 'spend-test-org', 'org')`,
		nullTeamProjectID, orgID, userID)
	// A blueprint + trigger event_handler so an autonomous run can carry a
	// non-NULL trigger_id (conversations.trigger_id FK → event_handlers;
	// the trigger's same-team FK → blueprints). The view passes
	// conversations.trigger_id straight through (TFAC-478).
	blueprintID := uuid.New().String()
	pgtest.MustExec(t, h.AdminDB,
		`INSERT INTO blueprints (id, org_id, team_id, creator_user_id, name) VALUES ($1, $2, $3, $4, 'spend-bp')`,
		blueprintID, orgID, teamID, userID)
	triggerID = uuid.New().String()
	pgtest.MustExec(t, h.AdminDB,
		`INSERT INTO event_handlers (id, org_id, team_id, creator_user_id, kind, event_type, blueprint_id, breaker_threshold, min_autonomy_suitability)
		 VALUES ($1, $2, $3, $4, 'trigger', 'github:pr:ci_check_failed', $5, 3, 0.5)`,
		triggerID, orgID, teamID, userID, blueprintID)
	return
}

// newPgSpendSeeder owns the raw INSERT shape for each source table on the admin
// pool. origin='manual' satisfies conversations_origin_requires_parents without a
// blueprint graph; empty CreatorUserID/ActorAgentID and a nil Cost map to SQL
// NULL (uuid columns reject empty strings; the in-flight run carries NULL cost).
func newPgSpendSeeder(conn *sql.DB, orgID, teamProjectID, nullTeamProjectID string) dbtest.SpendSeeder {
	seedLedgerRow := func(t *testing.T, convID, model string, cost any, tok dbtest.SpendTokens, at time.Time) string {
		t.Helper()
		var id int64
		if err := conn.QueryRow(`
			INSERT INTO messages
				(org_id, conversation_id, role, subtype, content, model,
				 input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens,
				 cost_usd, created_at)
			VALUES ($1, $2, 'assistant', '', 'work', NULLIF($3, ''),
			        $4, $5, $6, $7, $8, $9)
			RETURNING id
		`, orgID, convID, model,
			tok.Input, tok.Output, tok.CacheRead, tok.CacheCreation, cost, at).Scan(&id); err != nil {
			t.Fatalf("seed ledger row: %v", err)
		}
		return strconv.FormatInt(id, 10)
	}
	return dbtest.SpendSeeder{
		Conversation: func(t *testing.T, f dbtest.ConversationSpendFixture) string {
			t.Helper()
			id := uuid.New().String()
			if _, err := conn.Exec(`
				INSERT INTO conversations
					(id, org_id, team_id, creator_user_id, trigger_type, origin, actor_agent_id, trigger_id, model, status, started_at)
				VALUES ($1, $2, $3, $4, $5, 'manual', $6, $7, $8, $9, $10)
			`,
				id, orgID, f.TeamID, pgUUIDArg(f.CreatorUserID), f.TriggerType,
				pgUUIDArg(f.ActorAgentID), pgUUIDArg(f.TriggerID), f.Model, f.Status, f.StartedAt,
			); err != nil {
				t.Fatalf("seed run: %v", err)
			}
			return seedLedgerRow(t, id, f.Model, pgCostArg(f.Cost), f.Tokens, f.StartedAt)
		},
		Curator: func(t *testing.T, f dbtest.CuratorSpendFixture) string {
			t.Helper()
			// One curator conversation carrying the (team, creator)
			// attribution snapshot, one cost-stamped ledger row. Non-empty
			// TeamID → the team-scoped project (snapshot carries its team);
			// empty → the null-team project (snapshot is NULL), captured via
			// the same (SELECT team_id FROM projects WHERE id = ...) subquery
			// production uses, so this proves project team → conversation
			// team → view.
			projID := nullTeamProjectID
			if f.TeamID != "" {
				projID = teamProjectID
			}
			convID := uuid.New().String()
			if _, err := conn.Exec(`
				INSERT INTO conversations
					(id, org_id, type, creator_user_id, team_id, visibility,
					 trigger_type, origin, runtime, status, project_id, started_at)
				VALUES ($1, $2, 'curator',
				        COALESCE(NULLIF($3, '')::uuid, (SELECT owner_user_id FROM orgs WHERE id = $2)),
				        (SELECT team_id FROM projects WHERE id = $4),
				        'private', 'manual', 'curator', 'sdk', NULL, $4, $5)
			`, convID, orgID, f.CreatorUserID, projID, f.CreatedAt); err != nil {
				t.Fatalf("seed curator conversation: %v", err)
			}
			// The curator arm's model deliberately stays NULL — the wire
			// contract never exposed a curator model.
			return seedLedgerRow(t, convID, "", f.Cost, f.Tokens, f.CreatedAt)
		},
		System: func(t *testing.T, f dbtest.SystemSpendFixture) string {
			t.Helper()
			id := uuid.New().String()
			if _, err := conn.Exec(`
				INSERT INTO system_llm_runs
					(id, org_id, job, model, total_cost_usd,
					 input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens, started_at)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
			`,
				id, orgID, f.Job, f.Model, f.Cost,
				f.Tokens.Input, f.Tokens.Output, f.Tokens.CacheRead, f.Tokens.CacheCreation, f.StartedAt,
			); err != nil {
				t.Fatalf("seed system_llm_run: %v", err)
			}
			return id
		},
	}
}

// spendVisibleIDs reads the llm_spend view through the app pool under (userID,
// orgID) claims — exercising the view's security_invoker RLS — and returns the
// set of source_ids the caller can see.
func spendVisibleIDs(t *testing.T, h *pgtest.Harness, userID, orgID string) map[string]bool {
	t.Helper()
	got := map[string]bool{}
	if err := h.WithUser(t, userID, orgID, func(tx *sql.Tx) error {
		rows, err := pgstore.NewForTx(tx, pgtest.SecretKey).Spend.ListSpend(context.Background(), orgID, domain.SpendFilter{})
		if err != nil {
			return fmt.Errorf("ListSpend: %w", err)
		}
		for _, r := range rows {
			got[r.SourceID] = true
		}
		return nil
	}); err != nil {
		t.Fatalf("WithUser(%s, %s): %v", userID, orgID, err)
	}
	return got
}

// assertVisible checks each expected source_id's presence/absence in got.
func assertSpendVisible(t *testing.T, who string, got map[string]bool, want map[string]bool) {
	t.Helper()
	for id, shouldSee := range want {
		if got[id] != shouldSee {
			t.Errorf("%s visibility of %s = %v, want %v", who, id, got[id], shouldSee)
		}
	}
}

func float64Ptr(f float64) *float64 { return &f }

// pgUUIDArg maps "" → nil so an empty id lands as SQL NULL (uuid columns reject
// an empty string); a non-empty value passes through (it must be a valid uuid).
func pgUUIDArg(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func pgCostArg(c *float64) any {
	if c == nil {
		return nil
	}
	return *c
}
