package dbtest

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// SpendStoreFixture is what a per-backend factory hands the conformance suite:
// the wired read-only SpendStore plus the identifiers the seeder threads into
// the three source tables. SQLite returns the local sentinels; Postgres returns
// freshly-seeded org/team/user/agent rows. The store reads RLS-bypassed in both
// (SQLite is N=1; the Postgres factory wires admin-on-both) so this suite proves
// SQL/view *correctness* — RLS gets its own dedicated Postgres test.
type SpendStoreFixture struct {
	Store   db.SpendStore
	OrgID   string
	TeamID  string
	UserID  string
	AgentID string
	Seeder  SpendSeeder
}

// SpendStoreFactory returns a fresh fixture per call so subtests don't share
// state (mirrors PromptStoreFactory).
type SpendStoreFactory func(t *testing.T) SpendStoreFixture

// SpendTokens is the 4-token breakdown carried on every source row.
type SpendTokens struct {
	Input, Output, CacheRead, CacheCreation int
}

// RunSpendFixture stages one runs row. CreatorUserID "" → NULL (pair with
// TriggerType "event"); ActorAgentID "" → NULL; Cost nil → NULL (an in-flight
// run). The seeder writes origin='manual' to sidestep the blueprint-parents
// CHECK.
type RunSpendFixture struct {
	TeamID        string
	CreatorUserID string
	TriggerType   string // "manual" | "event"
	ActorAgentID  string
	Model         string
	Cost          *float64
	Tokens        SpendTokens
	Status        string
	StartedAt     time.Time
}

// CuratorSpendFixture stages one curator_requests row. Status "" defaults to
// "completed" in the seeder. TeamID selects which project the curator turn is
// attributed to (TFAC-476): "" → a null-team (org/private) project, so the
// snapshotted team_id is NULL; non-empty → the fixture's team-scoped project, so
// the snapshot carries that team. The per-backend seeder owns mapping this to a
// project and snapshotting team_id from it via (SELECT team_id FROM projects …),
// exactly as production does — proving project team → row team → view.
type CuratorSpendFixture struct {
	CreatorUserID string
	Cost          float64
	Tokens        SpendTokens
	Status        string
	CreatedAt     time.Time
	TeamID        string
}

// SystemSpendFixture stages one system_llm_runs row.
type SystemSpendFixture struct {
	Job       string
	Model     string
	Cost      float64
	Tokens    SpendTokens
	StartedAt time.Time
}

// SpendSeeder is the per-backend insert layer. Each closure inserts one source
// row via the backend's admin/raw connection and returns its id (== the view's
// source_id), so the suite can index results without knowing either schema.
type SpendSeeder struct {
	Run     func(t *testing.T, f RunSpendFixture) string
	Curator func(t *testing.T, f CuratorSpendFixture) string
	System  func(t *testing.T, f SystemSpendFixture) string
}

// spendEps bounds float comparisons. Postgres total_cost_usd is `real` (float4,
// ~1e-7 relative precision), so dollar values like 0.05 aren't exact; 1e-4 is
// comfortably above float4 noise and below any cost we assert.
const spendEps = 1e-4

// RunSpendStoreConformance runs the dialect-agnostic assertion suite against any
// db.SpendStore impl. Covers the contract TFAC-472 specifies: one row per source
// row, correct category derivation (manual / autonomous / curator /
// system_overhead), native + correct cost & token breakdown for all three
// sources, settled-spend semantics (terminal non-completed rows carry tokens;
// in-flight rows read 0), SpendByCategory sums, and the ListSpend filters.
func RunSpendStoreConformance(t *testing.T, factory SpendStoreFactory) {
	t.Helper()

	// Fixed historical timestamps (truncated to whole seconds so the SQLite
	// text round-trip can't drop sub-second precision and break Equal).
	t1 := time.Date(2026, 6, 20, 10, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 6, 20, 11, 0, 0, 0, time.UTC)
	t3 := time.Date(2026, 6, 20, 12, 0, 0, 0, time.UTC)
	t4 := time.Date(2026, 6, 20, 13, 0, 0, 0, time.UTC)

	t.Run("OneRowPerSource_Derivations", func(t *testing.T) {
		fx := factory(t)
		ctx := context.Background()

		manualID := fx.Seeder.Run(t, RunSpendFixture{
			TeamID: fx.TeamID, CreatorUserID: fx.UserID, TriggerType: "manual",
			ActorAgentID: fx.AgentID, Model: "claude-opus-4-8", Cost: spendCostPtr(1.50),
			Tokens: SpendTokens{100, 200, 300, 400}, Status: "completed", StartedAt: t1,
		})
		eventID := fx.Seeder.Run(t, RunSpendFixture{
			TeamID: fx.TeamID, CreatorUserID: "", TriggerType: "event",
			ActorAgentID: fx.AgentID, Model: "claude-haiku-4-5", Cost: spendCostPtr(0.25),
			Tokens: SpendTokens{10, 20, 30, 40}, Status: "completed", StartedAt: t2,
		})
		// Curator on a TEAM-visible project → team_id snapshotted from the project.
		curatorID := fx.Seeder.Curator(t, CuratorSpendFixture{
			CreatorUserID: fx.UserID, Cost: 0.75, TeamID: fx.TeamID,
			Tokens: SpendTokens{11, 22, 33, 44}, Status: "completed", CreatedAt: t3,
		})
		// Curator on a null-team (org/private) project → team_id NULL (TFAC-476).
		orgCuratorID := fx.Seeder.Curator(t, CuratorSpendFixture{
			CreatorUserID: fx.UserID, Cost: 0.10,
			Tokens: SpendTokens{1, 1, 1, 1}, Status: "completed", CreatedAt: t3,
		})
		systemID := fx.Seeder.System(t, SystemSpendFixture{
			Job: "scorer", Model: "claude-haiku-4-5", Cost: 0.05,
			Tokens: SpendTokens{1, 2, 3, 4}, StartedAt: t4,
		})

		rows, err := fx.Store.ListSpend(ctx, fx.OrgID, domain.SpendFilter{})
		if err != nil {
			t.Fatalf("ListSpend: %v", err)
		}
		byID := indexSpend(t, rows)
		if len(rows) != 5 {
			t.Fatalf("ListSpend returned %d rows, want 5 (one per seeded source row)", len(rows))
		}

		// Manual run → category 'manual', full passthrough.
		mr := byID[manualID]
		assertEq(t, "manual.source", mr.Source, domain.SpendSourceRun)
		assertEq(t, "manual.category", mr.Category, domain.SpendCategoryManual)
		assertNilStr(t, "manual.subtype", mr.Subtype)
		assertPtrEq(t, "manual.team_id", mr.TeamID, fx.TeamID)
		assertPtrEq(t, "manual.creator_user_id", mr.CreatorUserID, fx.UserID)
		assertPtrEq(t, "manual.actor_agent_id", mr.ActorAgentID, fx.AgentID)
		assertPtrEq(t, "manual.model", mr.Model, "claude-opus-4-8")
		assertCost(t, "manual.cost", mr.TotalCostUSD, 1.50)
		assertTokens(t, "manual", mr, SpendTokens{100, 200, 300, 400})
		if !mr.OccurredAt.Truncate(time.Second).Equal(t1) {
			t.Errorf("manual.occurred_at = %s, want %s (runs.started_at)", mr.OccurredAt, t1)
		}

		// Event run → category 'autonomous'; creator NULL (the CHECK pairs
		// trigger_type='event' with a NULL creator), agent still set.
		er := byID[eventID]
		assertEq(t, "event.category", er.Category, domain.SpendCategoryAutonomous)
		assertNilStr(t, "event.creator_user_id", er.CreatorUserID)
		assertPtrEq(t, "event.actor_agent_id", er.ActorAgentID, fx.AgentID)
		assertPtrEq(t, "event.team_id", er.TeamID, fx.TeamID)
		assertCost(t, "event.cost", er.TotalCostUSD, 0.25)

		// Curator on a team project → category 'curator'; team_id snapshotted from
		// the project (TFAC-476); no model, no agent.
		cr := byID[curatorID]
		assertEq(t, "curator.source", cr.Source, domain.SpendSourceCurator)
		assertEq(t, "curator.category", cr.Category, domain.SpendCategoryCurator)
		assertPtrEq(t, "curator.team_id", cr.TeamID, fx.TeamID)
		assertNilStr(t, "curator.subtype", cr.Subtype)
		assertNilStr(t, "curator.model", cr.Model)
		assertNilStr(t, "curator.actor_agent_id", cr.ActorAgentID)
		assertPtrEq(t, "curator.creator_user_id", cr.CreatorUserID, fx.UserID)
		assertCost(t, "curator.cost", cr.TotalCostUSD, 0.75)
		assertTokens(t, "curator", cr, SpendTokens{11, 22, 33, 44})
		if !cr.OccurredAt.Truncate(time.Second).Equal(t3) {
			t.Errorf("curator.occurred_at = %s, want %s (curator_requests.created_at)", cr.OccurredAt, t3)
		}

		// Curator on a null-team (org/private) project → team_id NULL: still
		// creator-visible, just absent from team dashboards (TFAC-476).
		ocr := byID[orgCuratorID]
		assertEq(t, "org-curator.category", ocr.Category, domain.SpendCategoryCurator)
		assertNilStr(t, "org-curator.team_id", ocr.TeamID)
		assertPtrEq(t, "org-curator.creator_user_id", ocr.CreatorUserID, fx.UserID)

		// System → category 'system_overhead'; job carried as subtype; no team,
		// no creator, no agent; model present.
		sr := byID[systemID]
		assertEq(t, "system.source", sr.Source, domain.SpendSourceSystem)
		assertEq(t, "system.category", sr.Category, domain.SpendCategorySystemOverhead)
		assertPtrEq(t, "system.subtype", sr.Subtype, "scorer")
		assertNilStr(t, "system.team_id", sr.TeamID)
		assertNilStr(t, "system.creator_user_id", sr.CreatorUserID)
		assertNilStr(t, "system.actor_agent_id", sr.ActorAgentID)
		assertPtrEq(t, "system.model", sr.Model, "claude-haiku-4-5")
		assertCost(t, "system.cost", sr.TotalCostUSD, 0.05)
		assertTokens(t, "system", sr, SpendTokens{1, 2, 3, 4})
	})

	// Terminal-spend completeness: TFAC-473 rolls the token breakdown up on
	// EVERY terminal write — cancel and infra-failure included, not just clean
	// completion. The view reads columns, so a cancelled/failed run or curator
	// turn surfaces its real token spend. Locks that guarantee at the read layer.
	t.Run("TerminalSpend_NonCompletedRowsCarryTokens", func(t *testing.T) {
		fx := factory(t)
		ctx := context.Background()

		cancelledRun := fx.Seeder.Run(t, RunSpendFixture{
			TeamID: fx.TeamID, CreatorUserID: fx.UserID, TriggerType: "manual",
			Model: "m", Cost: spendCostPtr(0.40),
			Tokens: SpendTokens{50, 60, 70, 80}, Status: "cancelled", StartedAt: t1,
		})
		failedRun := fx.Seeder.Run(t, RunSpendFixture{
			TeamID: fx.TeamID, CreatorUserID: fx.UserID, TriggerType: "manual",
			Model: "m", Cost: spendCostPtr(0.10),
			Tokens: SpendTokens{5, 6, 7, 8}, Status: "failed", StartedAt: t2,
		})
		cancelledCurator := fx.Seeder.Curator(t, CuratorSpendFixture{
			CreatorUserID: fx.UserID, Cost: 0.30,
			Tokens: SpendTokens{9, 8, 7, 6}, Status: "cancelled", CreatedAt: t3,
		})

		rows, err := fx.Store.ListSpend(ctx, fx.OrgID, domain.SpendFilter{})
		if err != nil {
			t.Fatalf("ListSpend: %v", err)
		}
		byID := indexSpend(t, rows)
		assertTokens(t, "cancelled-run", byID[cancelledRun], SpendTokens{50, 60, 70, 80})
		assertCost(t, "cancelled-run.cost", byID[cancelledRun].TotalCostUSD, 0.40)
		assertTokens(t, "failed-run", byID[failedRun], SpendTokens{5, 6, 7, 8})
		assertTokens(t, "cancelled-curator", byID[cancelledCurator], SpendTokens{9, 8, 7, 6})
	})

	// In-flight (non-terminal) rows: cost is NULL → COALESCE(…,0) → 0, and the
	// token columns are still 0 (no terminal roll-up yet). Settled-spend
	// semantics — consistent across cost and tokens.
	t.Run("InFlightRows_ReadZero", func(t *testing.T) {
		fx := factory(t)
		ctx := context.Background()

		inflight := fx.Seeder.Run(t, RunSpendFixture{
			TeamID: fx.TeamID, CreatorUserID: fx.UserID, TriggerType: "manual",
			Model: "m", Cost: nil, // NULL cost — the in-flight shape
			Tokens: SpendTokens{0, 0, 0, 0}, Status: "running", StartedAt: t1,
		})

		rows, err := fx.Store.ListSpend(ctx, fx.OrgID, domain.SpendFilter{})
		if err != nil {
			t.Fatalf("ListSpend: %v", err)
		}
		r := indexSpend(t, rows)[inflight]
		if r.TotalCostUSD != 0 {
			t.Errorf("in-flight cost = %v, want 0 (COALESCE of NULL total_cost_usd)", r.TotalCostUSD)
		}
		assertTokens(t, "in-flight", r, SpendTokens{0, 0, 0, 0})
	})

	t.Run("SpendByCategory_SumsAcrossUnion", func(t *testing.T) {
		fx := factory(t)
		ctx := context.Background()

		fx.Seeder.Run(t, RunSpendFixture{TeamID: fx.TeamID, CreatorUserID: fx.UserID, TriggerType: "manual", Model: "m", Cost: spendCostPtr(1.00), Tokens: SpendTokens{100, 10, 1, 0}, Status: "completed", StartedAt: t1})
		fx.Seeder.Run(t, RunSpendFixture{TeamID: fx.TeamID, CreatorUserID: fx.UserID, TriggerType: "manual", Model: "m", Cost: spendCostPtr(2.00), Tokens: SpendTokens{200, 20, 2, 0}, Status: "completed", StartedAt: t2})
		fx.Seeder.Run(t, RunSpendFixture{TeamID: fx.TeamID, CreatorUserID: "", TriggerType: "event", Model: "m", Cost: spendCostPtr(0.50), Tokens: SpendTokens{5, 5, 5, 5}, Status: "completed", StartedAt: t2})
		fx.Seeder.Curator(t, CuratorSpendFixture{CreatorUserID: fx.UserID, Cost: 0.30, Tokens: SpendTokens{3, 3, 3, 3}, Status: "completed", CreatedAt: t3})
		fx.Seeder.Curator(t, CuratorSpendFixture{CreatorUserID: fx.UserID, Cost: 0.70, Tokens: SpendTokens{7, 7, 7, 7}, Status: "completed", CreatedAt: t3})
		fx.Seeder.System(t, SystemSpendFixture{Job: "classifier", Model: "m", Cost: 0.05, Tokens: SpendTokens{1, 1, 1, 1}, StartedAt: t4})

		// All-time (zero since + zero until → both bounds dropped).
		allBuckets, err := fx.Store.SpendByCategory(ctx, fx.OrgID, time.Time{}, time.Time{})
		byCat := spendByCat(t, allBuckets, err)
		assertCost(t, "manual.sum", byCat[domain.SpendCategoryManual].TotalCostUSD, 3.00)
		assertEq(t, "manual.input", byCat[domain.SpendCategoryManual].InputTokens, 300)
		assertEq(t, "manual.output", byCat[domain.SpendCategoryManual].OutputTokens, 30)
		assertEq(t, "manual.cache_read", byCat[domain.SpendCategoryManual].CacheReadTokens, 3)
		assertCost(t, "autonomous.sum", byCat[domain.SpendCategoryAutonomous].TotalCostUSD, 0.50)
		assertCost(t, "curator.sum", byCat[domain.SpendCategoryCurator].TotalCostUSD, 1.00)
		assertEq(t, "curator.input", byCat[domain.SpendCategoryCurator].InputTokens, 10)
		assertCost(t, "system.sum", byCat[domain.SpendCategorySystemOverhead].TotalCostUSD, 0.05)
		assertEq(t, "system.cache_creation", byCat[domain.SpendCategorySystemOverhead].CacheCreationTokens, 1)

		// Closed window [t2, t4) — the billing-period shape. Excludes the t1
		// manual run (since lower bound) and the t4 system row (until upper
		// bound, exclusive); keeps the t2 runs + t3 curator turns.
		winBuckets, err := fx.Store.SpendByCategory(ctx, fx.OrgID, t2, t4)
		win := spendByCat(t, winBuckets, err)
		assertCost(t, "win.manual", win[domain.SpendCategoryManual].TotalCostUSD, 2.00) // only the t2 run, not t1
		assertCost(t, "win.autonomous", win[domain.SpendCategoryAutonomous].TotalCostUSD, 0.50)
		assertCost(t, "win.curator", win[domain.SpendCategoryCurator].TotalCostUSD, 1.00)
		if _, ok := win[domain.SpendCategorySystemOverhead]; ok {
			t.Errorf("window [t2,t4) included a system_overhead bucket; the t4 row should be excluded by the exclusive until bound")
		}
	})

	// TeamID filter narrows to one team's spend — two-sided: it INCLUDES
	// team-scoped runs AND team-attributed curator (its project carries the team,
	// TFAC-476), and EXCLUDES null-team curator + system (NULL team_id) so the team
	// dashboard sees its own work, not org-level overhead or another project's.
	t.Run("ListSpend_TeamFilter", func(t *testing.T) {
		fx := factory(t)
		ctx := context.Background()

		runID := fx.Seeder.Run(t, RunSpendFixture{TeamID: fx.TeamID, CreatorUserID: fx.UserID, TriggerType: "manual", Model: "m", Cost: spendCostPtr(1.00), Tokens: SpendTokens{1, 1, 1, 1}, Status: "completed", StartedAt: t1})
		// Team-attributed curator (project carries fx.TeamID) → included.
		teamCuratorID := fx.Seeder.Curator(t, CuratorSpendFixture{CreatorUserID: fx.UserID, Cost: 0.5, TeamID: fx.TeamID, Tokens: SpendTokens{1, 1, 1, 1}, Status: "completed", CreatedAt: t2})
		// Null-team curator (org/private project) + system → NULL team_id, excluded.
		fx.Seeder.Curator(t, CuratorSpendFixture{CreatorUserID: fx.UserID, Cost: 0.5, Tokens: SpendTokens{1, 1, 1, 1}, Status: "completed", CreatedAt: t2})
		fx.Seeder.System(t, SystemSpendFixture{Job: "scorer", Model: "m", Cost: 0.05, Tokens: SpendTokens{1, 1, 1, 1}, StartedAt: t3})

		team := fx.TeamID
		got, err := fx.Store.ListSpend(ctx, fx.OrgID, domain.SpendFilter{TeamID: &team})
		if err != nil {
			t.Fatalf("ListSpend team filter: %v", err)
		}
		gotIDs := map[string]bool{}
		for _, r := range got {
			gotIDs[r.SourceID] = true
		}
		if len(got) != 2 || !gotIDs[runID] || !gotIDs[teamCuratorID] {
			t.Fatalf("TeamID filter = %d rows %v, want exactly the run %s + team-attributed curator %s (null-team curator/system excluded)", len(got), gotIDs, runID, teamCuratorID)
		}

		other := "00000000-0000-0000-0000-0000000009ff"
		none, err := fx.Store.ListSpend(ctx, fx.OrgID, domain.SpendFilter{TeamID: &other})
		if err != nil {
			t.Fatalf("ListSpend other-team filter: %v", err)
		}
		if len(none) != 0 {
			t.Fatalf("TeamID=other returned %d rows, want 0", len(none))
		}
	})

	t.Run("ListSpend_CategoryFilter", func(t *testing.T) {
		fx := factory(t)
		ctx := context.Background()

		fx.Seeder.Run(t, RunSpendFixture{TeamID: fx.TeamID, CreatorUserID: fx.UserID, TriggerType: "manual", Model: "m", Cost: spendCostPtr(1.00), Tokens: SpendTokens{1, 1, 1, 1}, Status: "completed", StartedAt: t1})
		curatorID := fx.Seeder.Curator(t, CuratorSpendFixture{CreatorUserID: fx.UserID, Cost: 0.5, Tokens: SpendTokens{1, 1, 1, 1}, Status: "completed", CreatedAt: t2})
		fx.Seeder.System(t, SystemSpendFixture{Job: "scorer", Model: "m", Cost: 0.05, Tokens: SpendTokens{1, 1, 1, 1}, StartedAt: t3})

		cat := domain.SpendCategoryCurator
		got, err := fx.Store.ListSpend(ctx, fx.OrgID, domain.SpendFilter{Category: &cat})
		if err != nil {
			t.Fatalf("ListSpend category filter: %v", err)
		}
		if len(got) != 1 || got[0].SourceID != curatorID {
			t.Fatalf("Category=curator = %d rows, want exactly the curator row %s", len(got), curatorID)
		}
	})

	t.Run("ListSpend_TimeWindow_And_Limit", func(t *testing.T) {
		fx := factory(t)
		ctx := context.Background()

		fx.Seeder.System(t, SystemSpendFixture{Job: "scorer", Model: "m", Cost: 0.01, Tokens: SpendTokens{1, 1, 1, 1}, StartedAt: t1})
		fx.Seeder.System(t, SystemSpendFixture{Job: "scorer", Model: "m", Cost: 0.02, Tokens: SpendTokens{1, 1, 1, 1}, StartedAt: t3})
		fx.Seeder.System(t, SystemSpendFixture{Job: "scorer", Model: "m", Cost: 0.03, Tokens: SpendTokens{1, 1, 1, 1}, StartedAt: t4})

		// Since t3 → the t3 + t4 rows (>=), not t1.
		since, err := fx.Store.ListSpend(ctx, fx.OrgID, domain.SpendFilter{Since: t3})
		if err != nil {
			t.Fatalf("ListSpend since: %v", err)
		}
		if len(since) != 2 {
			t.Fatalf("Since=t3 = %d rows, want 2 (t3 + t4)", len(since))
		}
		// Newest-first ordering.
		if !since[0].OccurredAt.After(since[1].OccurredAt) {
			t.Errorf("rows not ordered occurred_at DESC: [0]=%s [1]=%s", since[0].OccurredAt, since[1].OccurredAt)
		}

		// Until t3 → only the t1 row (< t3).
		until, err := fx.Store.ListSpend(ctx, fx.OrgID, domain.SpendFilter{Until: t3})
		if err != nil {
			t.Fatalf("ListSpend until: %v", err)
		}
		if len(until) != 1 {
			t.Fatalf("Until=t3 = %d rows, want 1 (only t1, the < bound is exclusive)", len(until))
		}

		// Limit caps the page.
		limited, err := fx.Store.ListSpend(ctx, fx.OrgID, domain.SpendFilter{Limit: 1})
		if err != nil {
			t.Fatalf("ListSpend limit: %v", err)
		}
		if len(limited) != 1 {
			t.Fatalf("Limit=1 = %d rows, want 1", len(limited))
		}
	})
}

func spendCostPtr(f float64) *float64 { return &f }

func indexSpend(t *testing.T, rows []domain.SpendRow) map[string]domain.SpendRow {
	t.Helper()
	m := make(map[string]domain.SpendRow, len(rows))
	for _, r := range rows {
		m[r.SourceID] = r
	}
	return m
}

// spendByCat fails on error and indexes SpendByCategory buckets by category.
func spendByCat(t *testing.T, buckets []domain.SpendBucket, err error) map[string]domain.SpendBucket {
	t.Helper()
	if err != nil {
		t.Fatalf("SpendByCategory: %v", err)
	}
	m := make(map[string]domain.SpendBucket, len(buckets))
	for _, b := range buckets {
		m[b.Category] = b
	}
	return m
}

func assertEq[T comparable](t *testing.T, label string, got, want T) {
	t.Helper()
	if got != want {
		t.Errorf("%s = %v, want %v", label, got, want)
	}
}

func assertCost(t *testing.T, label string, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > spendEps {
		t.Errorf("%s = %v, want %v (±%v)", label, got, want, spendEps)
	}
}

func assertTokens(t *testing.T, label string, r domain.SpendRow, want SpendTokens) {
	t.Helper()
	if r.InputTokens != want.Input || r.OutputTokens != want.Output ||
		r.CacheReadTokens != want.CacheRead || r.CacheCreationTokens != want.CacheCreation {
		t.Errorf("%s tokens = (in=%d out=%d cr=%d cc=%d), want (in=%d out=%d cr=%d cc=%d)",
			label, r.InputTokens, r.OutputTokens, r.CacheReadTokens, r.CacheCreationTokens,
			want.Input, want.Output, want.CacheRead, want.CacheCreation)
	}
}

func assertPtrEq(t *testing.T, label string, got *string, want string) {
	t.Helper()
	if got == nil {
		t.Errorf("%s = nil, want %q", label, want)
		return
	}
	if *got != want {
		t.Errorf("%s = %q, want %q", label, *got, want)
	}
}

func assertNilStr(t *testing.T, label string, got *string) {
	t.Helper()
	if got != nil {
		t.Errorf("%s = %q, want nil", label, *got)
	}
}
