package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// --- pure: window parsing ---

func TestParseUsageWindow(t *testing.T) {
	now := time.Date(2026, 6, 26, 15, 4, 5, 0, time.UTC)
	monthStart := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

	tests := []struct {
		name      string
		q         map[string]string
		wantSince time.Time
		wantUntil time.Time
		wantErr   bool
	}{
		{"both absent → current month to now", nil, monthStart, now, false},
		{"since RFC3339", map[string]string{"since": "2026-03-01T08:30:00Z"}, time.Date(2026, 3, 1, 8, 30, 0, 0, time.UTC), time.Time{}, false},
		{"until bare date → UTC midnight", map[string]string{"until": "2026-04-01"}, time.Time{}, time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC), false},
		{"both bare dates", map[string]string{"since": "2026-03-01", "until": "2026-04-01"}, time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC), false},
		{"invalid since", map[string]string{"since": "not-a-time"}, time.Time{}, time.Time{}, true},
		{"invalid until", map[string]string{"until": "not-a-time"}, time.Time{}, time.Time{}, true},
		{"since after until", map[string]string{"since": "2026-04-01", "until": "2026-03-01"}, time.Time{}, time.Time{}, true},
		{"since equals until", map[string]string{"since": "2026-03-01", "until": "2026-03-01"}, time.Time{}, time.Time{}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			q := url.Values{}
			for k, v := range tc.q {
				q.Set(k, v)
			}
			since, until, errMsg := parseUsageWindow(q, now)
			if tc.wantErr {
				if errMsg == "" {
					t.Fatalf("want error, got since=%v until=%v", since, until)
				}
				return
			}
			if errMsg != "" {
				t.Fatalf("unexpected error: %s", errMsg)
			}
			if !since.Equal(tc.wantSince) {
				t.Errorf("since = %v, want %v", since, tc.wantSince)
			}
			if !until.Equal(tc.wantUntil) {
				t.Errorf("until = %v, want %v", until, tc.wantUntil)
			}
		})
	}
}

// --- pure: aggregation shapes ---

func usageStrPtr(s string) *string { return &s }

// usageTestRows is the fixed cross-source set the aggregation assertions read:
// two team-A runs (manual by u1, autonomous fired by trg1), a team-A curator by
// u1, a NULL-team curator by u2, and a NULL-team system row.
func usageTestRows() []domain.SpendRow {
	day := func(d int) time.Time { return time.Date(2026, 6, d, 10, 0, 0, 0, time.UTC) }
	return []domain.SpendRow{
		{Source: "run", SourceID: "r1", Category: domain.SpendCategoryManual, CreatorUserID: usageStrPtr("u1"), Model: usageStrPtr("opus"), TeamID: usageStrPtr("tA"), TotalCostUSD: 1.00, InputTokens: 100, OutputTokens: 10, CacheReadTokens: 5, CacheCreationTokens: 1, OccurredAt: day(15)},
		{Source: "run", SourceID: "r2", Category: domain.SpendCategoryAutonomous, TriggerID: usageStrPtr("trg1"), Model: usageStrPtr("haiku"), TeamID: usageStrPtr("tA"), TotalCostUSD: 0.25, InputTokens: 20, OutputTokens: 2, OccurredAt: day(16)},
		{Source: "curator", SourceID: "c1", Category: domain.SpendCategoryCurator, CreatorUserID: usageStrPtr("u1"), TeamID: usageStrPtr("tA"), TotalCostUSD: 0.50, InputTokens: 11, OccurredAt: day(15)},
		{Source: "curator", SourceID: "c2", Category: domain.SpendCategoryCurator, CreatorUserID: usageStrPtr("u2"), TotalCostUSD: 0.10, OccurredAt: day(17)},
		{Source: "system", SourceID: "s1", Category: domain.SpendCategorySystemOverhead, Model: usageStrPtr("haiku"), TotalCostUSD: 0.05, OccurredAt: day(18)},
	}
}

func TestSumSpendCost(t *testing.T) {
	if got := sumSpendCost(usageTestRows()); !floatEq(got, 1.90) {
		t.Errorf("sumSpendCost = %v, want 1.90", got)
	}
}

func TestSpendByCategory(t *testing.T) {
	got := spendByCategory(usageTestRows())
	// Sorted by category: autonomous, curator, manual, system_overhead.
	want := []usageCategoryBucket{
		{Category: domain.SpendCategoryAutonomous, Cost: 0.25, InputTokens: 20, OutputTokens: 2},
		{Category: domain.SpendCategoryCurator, Cost: 0.60, InputTokens: 11},
		{Category: domain.SpendCategoryManual, Cost: 1.00, InputTokens: 100, OutputTokens: 10, CacheReadTokens: 5, CacheCreationTokens: 1},
		{Category: domain.SpendCategorySystemOverhead, Cost: 0.05},
	}
	if len(got) != len(want) {
		t.Fatalf("byCategory = %d buckets, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i].Category != want[i].Category || !floatEq(got[i].Cost, want[i].Cost) ||
			got[i].InputTokens != want[i].InputTokens || got[i].OutputTokens != want[i].OutputTokens ||
			got[i].CacheReadTokens != want[i].CacheReadTokens || got[i].CacheCreationTokens != want[i].CacheCreationTokens {
			t.Errorf("byCategory[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestSpendByModel_SkipsNilModel(t *testing.T) {
	got := spendByModel(usageTestRows())
	// curator rows (NULL model) excluded; haiku = autonomous 0.25 + system 0.05.
	// Sorted cost-desc: opus (1.00), haiku (0.30).
	want := []usageModelBucket{{Model: "opus", Cost: 1.00}, {Model: "haiku", Cost: 0.30}}
	if len(got) != len(want) {
		t.Fatalf("byModel = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i].Model != want[i].Model || !floatEq(got[i].Cost, want[i].Cost) {
			t.Errorf("byModel[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// TestSpendModelBreakdowns_SkipSyntheticModel covers the rows the runtime
// composes itself (an API-error or interrupt line): they name no model, so the
// per-model breakdowns drop them exactly like a NULL-model curator row. Their
// dollars are real and still land in the period total and in by_day.
func TestSpendModelBreakdowns_SkipSyntheticModel(t *testing.T) {
	day := time.Date(2026, 6, 15, 10, 0, 0, 0, time.UTC)
	rows := []domain.SpendRow{
		{Source: "run", SourceID: "r1", Category: domain.SpendCategoryManual, Model: usageStrPtr("opus"), TotalCostUSD: 1.00, OccurredAt: day},
		{Source: "run", SourceID: "r2", Category: domain.SpendCategoryManual, Model: usageStrPtr(domain.ModelSynthetic), TotalCostUSD: 0.40, OccurredAt: day},
	}

	byModel := spendByModel(rows)
	if len(byModel) != 1 || byModel[0].Model != "opus" || !floatEq(byModel[0].Cost, 1.00) {
		t.Errorf("byModel = %+v, want opus only", byModel)
	}

	byDayModel := spendByDayModel(rows)
	if len(byDayModel) != 1 || byDayModel[0].Model != "opus" || !floatEq(byDayModel[0].Cost, 1.00) {
		t.Errorf("byDayModel = %+v, want the opus cell only", byDayModel)
	}

	if got := spendByDay(rows); len(got) != 1 || !floatEq(got[0].Cost, 1.40) {
		t.Errorf("byDay = %+v, want one 1.40 day (synthetic spend still counts in the total)", got)
	}
	if got := sumSpendCost(rows); !floatEq(got, 1.40) {
		t.Errorf("sumSpendCost = %v, want 1.40", got)
	}
}

func TestSpendByDay(t *testing.T) {
	got := spendByDay(usageTestRows())
	want := []usageDayBucket{
		{Date: "2026-06-15", Cost: 1.50}, // r1 + c1
		{Date: "2026-06-16", Cost: 0.25}, // r2
		{Date: "2026-06-17", Cost: 0.10}, // c2
		{Date: "2026-06-18", Cost: 0.05}, // s1
	}
	if len(got) != len(want) {
		t.Fatalf("byDay = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i].Date != want[i].Date || !floatEq(got[i].Cost, want[i].Cost) {
			t.Errorf("byDay[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestSpendByDayModel_SkipsNilModelSortedByDateThenModel(t *testing.T) {
	got := spendByDayModel(usageTestRows())
	// Curator rows (NULL model) excluded — c1 (06-15) and c2 (06-17) drop out.
	// Sorted by date, then model: r1 opus (06-15), r2 haiku (06-16), s1 haiku (06-18).
	want := []usageDayModelBucket{
		{Date: "2026-06-15", Model: "opus", Cost: 1.00},
		{Date: "2026-06-16", Model: "haiku", Cost: 0.25},
		{Date: "2026-06-18", Model: "haiku", Cost: 0.05},
	}
	if len(got) != len(want) {
		t.Fatalf("byDayModel = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i].Date != want[i].Date || got[i].Model != want[i].Model ||
			!floatEq(got[i].Cost, want[i].Cost) {
			t.Errorf("byDayModel[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestSpendByUser_ManualAndCuratorOnly(t *testing.T) {
	profiles := map[string]userProfile{
		"u1": {name: "Alice", avatar: "https://ex/a.png"},
		"u2": {name: "Bob"}, // no avatar → AvatarURL omitted
	}
	got := spendByUser(usageTestRows(), profiles)
	// Autonomous (NULL creator) + system (NULL creator) excluded. Sorted cost-desc.
	want := []usageUserBucket{
		{UserID: "u1", DisplayName: "Alice", AvatarURL: "https://ex/a.png", Cost: 1.50}, // r1 + c1
		{UserID: "u2", DisplayName: "Bob", Cost: 0.10},                                  // c2
	}
	if len(got) != len(want) {
		t.Fatalf("byUser = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("byUser[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestSpendByRule_AutonomousOnly(t *testing.T) {
	names := map[string]string{"trg1": "CI Fix"}
	got := spendByRule(usageTestRows(), names)
	if len(got) != 1 {
		t.Fatalf("byRule = %+v, want 1 bucket (only the autonomous row)", got)
	}
	if got[0].TriggerID != "trg1" || got[0].RuleName != "CI Fix" || !floatEq(got[0].Cost, 0.25) {
		t.Errorf("byRule[0] = %+v, want {trg1, CI Fix, 0.25}", got[0])
	}
}

func TestSpendByTeam_ExcludesNullTeam(t *testing.T) {
	names := map[string]string{"tA": "Team A"}
	got := spendByTeam(usageTestRows(), names)
	// c2 (NULL team) + s1 (NULL team) excluded; tA = r1 + r2 + c1.
	if len(got) != 1 {
		t.Fatalf("byTeam = %+v, want 1 bucket", got)
	}
	if got[0].TeamID != "tA" || got[0].TeamName != "Team A" || !floatEq(got[0].Cost, 1.75) {
		t.Errorf("byTeam[0] = %+v, want {tA, Team A, 1.75}", got[0])
	}
}

func TestSpendOrgLevel_NullTeamRowsByCategory(t *testing.T) {
	got := spendOrgLevel(usageTestRows())
	// NULL-team rows: c2 (curator 0.10) + s1 (system_overhead 0.05), by category.
	want := []usageOrgLevelBucket{
		{Category: domain.SpendCategoryCurator, Cost: 0.10},
		{Category: domain.SpendCategorySystemOverhead, Cost: 0.05},
	}
	if len(got) != len(want) {
		t.Fatalf("orgLevel = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i].Category != want[i].Category || !floatEq(got[i].Cost, want[i].Cost) {
			t.Errorf("orgLevel[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// --- local-mode end-to-end (SQLite, no Docker) ---

// TestUsageEndpoints_Local drives the three endpoints through the real router +
// withSession middleware against an in-memory SQLite (local mode), so the gates
// short-circuit to the single implicit admin and we exercise the full read +
// aggregation + JSON path. The role-gate 403s are multi-mode-only and covered
// by the Postgres rig.
func TestUsageEndpoints_Local(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newUsageTestServer(t)
	_, blueprintName := seedUsageLocal(t, s)

	t.Run("me_own_spend_only", func(t *testing.T) {
		var resp usageMeResponse
		doUsage(t, s, "/api/usage/me?since=2000-01-01", &resp)
		// Only the local user's manual run (1.00) + their two curator turns
		// (0.50 + 0.10); the autonomous run + system row carry a NULL creator.
		if !floatEq(resp.TotalCostUSD, 1.60) {
			t.Errorf("/me total = %v, want 1.60", resp.TotalCostUSD)
		}
		cats := categoryCosts(resp.ByCategory)
		if !floatEq(cats[domain.SpendCategoryManual], 1.00) || !floatEq(cats[domain.SpendCategoryCurator], 0.60) {
			t.Errorf("/me by_category = %+v, want manual 1.00 + curator 0.60", resp.ByCategory)
		}
		if _, ok := cats[domain.SpendCategoryAutonomous]; ok {
			t.Errorf("/me by_category included autonomous (NULL creator should be excluded): %+v", resp.ByCategory)
		}
		// by_model: only the manual run's model (curator NULL model skipped).
		if len(resp.ByModel) != 1 || resp.ByModel[0].Model != "claude-opus-4-8" {
			t.Errorf("/me by_model = %+v, want only claude-opus-4-8", resp.ByModel)
		}
		assertByDaySumsToTotal(t, "/me", resp.ByDay, resp.TotalCostUSD)
	})

	t.Run("team_full_breakdown", func(t *testing.T) {
		var resp usageTeamResponse
		doUsage(t, s, "/api/usage/teams/"+runmode.LocalDefaultTeamID+"?since=2000-01-01", &resp)
		if resp.TeamID != runmode.LocalDefaultTeamID {
			t.Errorf("/teams team_id = %q, want %q", resp.TeamID, runmode.LocalDefaultTeamID)
		}
		// team rows: manual 1.00 + autonomous 0.25 + team curator 0.50 (NULL-team
		// curator + system excluded by the team filter).
		if !floatEq(resp.TotalCostUSD, 1.75) {
			t.Errorf("/teams total = %v, want 1.75", resp.TotalCostUSD)
		}
		// by_user: the local user's manual + team curator (autonomous NULL creator excluded).
		if len(resp.ByUser) != 1 || resp.ByUser[0].UserID != runmode.LocalDefaultUserID || !floatEq(resp.ByUser[0].Cost, 1.50) {
			t.Errorf("/teams by_user = %+v, want the local user at 1.50", resp.ByUser)
		}
		// by_rule: the autonomous run's trigger, labeled with the blueprint name
		// (triggers carry a NULL name, so we fall back to the blueprint).
		if len(resp.ByRule) != 1 || resp.ByRule[0].RuleName != blueprintName || !floatEq(resp.ByRule[0].Cost, 0.25) {
			t.Errorf("/teams by_rule = %+v, want one rule %q at 0.25", resp.ByRule, blueprintName)
		}
		cats := categoryCosts(resp.ByCategory)
		if !floatEq(cats[domain.SpendCategoryManual], 1.00) || !floatEq(cats[domain.SpendCategoryAutonomous], 0.25) || !floatEq(cats[domain.SpendCategoryCurator], 0.50) {
			t.Errorf("/teams by_category = %+v", resp.ByCategory)
		}
		assertByDaySumsToTotal(t, "/teams", resp.ByDay, resp.TotalCostUSD)
	})

	t.Run("org_rollup", func(t *testing.T) {
		var resp usageOrgResponse
		doUsage(t, s, "/api/usage/org?since=2000-01-01", &resp)
		if !floatEq(resp.TotalCostUSD, 1.90) {
			t.Errorf("/org total = %v, want 1.90", resp.TotalCostUSD)
		}
		// by_team: the one local team carries the team-attributed rows (1.75).
		if len(resp.ByTeam) != 1 || resp.ByTeam[0].TeamID != runmode.LocalDefaultTeamID || !floatEq(resp.ByTeam[0].Cost, 1.75) {
			t.Errorf("/org by_team = %+v, want the local team at 1.75", resp.ByTeam)
		}
		if resp.ByTeam[0].TeamName == "" {
			t.Errorf("/org by_team team_name empty; GetSystem name resolution failed: %+v", resp.ByTeam)
		}
		// by_user (org-wide): the local user's manual run (1.00) + both curator
		// turns (0.50 + 0.10) = 1.60; the autonomous run + system carry no creator.
		if len(resp.ByUser) != 1 || resp.ByUser[0].UserID != runmode.LocalDefaultUserID || !floatEq(resp.ByUser[0].Cost, 1.60) {
			t.Errorf("/org by_user = %+v, want the local user at 1.60", resp.ByUser)
		}
		// org_level: the NULL-team rows — curator 0.10 + system_overhead 0.05.
		ol := map[string]float64{}
		for _, b := range resp.OrgLevel {
			ol[b.Category] = b.Cost
		}
		if !floatEq(ol[domain.SpendCategoryCurator], 0.10) || !floatEq(ol[domain.SpendCategorySystemOverhead], 0.05) {
			t.Errorf("/org org_level = %+v, want curator 0.10 + system_overhead 0.05", resp.OrgLevel)
		}
		// by_rule: local mode (N=1) carries it on the org rollup too — the
		// autonomous run's trigger, labeled by its blueprint (0.25).
		if len(resp.ByRule) != 1 || resp.ByRule[0].RuleName != blueprintName || !floatEq(resp.ByRule[0].Cost, 0.25) {
			t.Errorf("/org by_rule = %+v, want one rule %q at 0.25 (local mode)", resp.ByRule, blueprintName)
		}
		assertByDaySumsToTotal(t, "/org", resp.ByDay, resp.TotalCostUSD)
	})

	t.Run("invalid_window_400", func(t *testing.T) {
		rec := doJSON(t, s, "GET", "/api/usage/me?since=not-a-date", nil)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("/me?since=not-a-date = %d, want 400; body=%s", rec.Code, rec.Body.String())
		}
	})

	t.Run("inverted_window_400", func(t *testing.T) {
		// since >= until is a non-positive window — reject rather than return an
		// empty 200 that masks the client bug.
		rec := doJSON(t, s, "GET", "/api/usage/me?since=2026-06-30&until=2026-06-01", nil)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("/me with since>until = %d, want 400; body=%s", rec.Code, rec.Body.String())
		}
	})
}

// --- local e2e helpers ---

// doUsage performs a GET against the server's mux and decodes a 200 JSON body
// into out.
func doUsage(t *testing.T, s *Server, path string, out any) {
	t.Helper()
	rec := doJSON(t, s, "GET", path, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200; body=%s", path, rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), out); err != nil {
		t.Fatalf("decode %s body %q: %v", path, rec.Body.String(), err)
	}
}

func categoryCosts(buckets []usageCategoryBucket) map[string]float64 {
	m := make(map[string]float64, len(buckets))
	for _, b := range buckets {
		m[b.Category] = b.Cost
	}
	return m
}

func assertByDaySumsToTotal(t *testing.T, label string, days []usageDayBucket, total float64) {
	t.Helper()
	if len(days) == 0 {
		t.Errorf("%s by_day is empty", label)
		return
	}
	var sum float64
	for _, d := range days {
		sum += d.Cost
	}
	if !floatEq(sum, total) {
		t.Errorf("%s by_day sums to %v, want %v (the total)", label, sum, total)
	}
}

func floatEq(a, b float64) bool {
	d := a - b
	return d < 1e-9 && d > -1e-9
}

// newUsageTestServer mirrors newTestServer but opens SQLite with the production
// _time_format=sqlite DSN (the same db.OpenAt uses), so a time.Time window bind
// wrapped in datetime() round-trips against stored datetimes. The bare
// newTestServer DSN serializes a bound time.Time to an unparseable form, which
// would silently drop every spend row from the windowed reads.
func newUsageTestServer(t *testing.T) *Server {
	t.Helper()
	database, err := sql.Open("sqlite", ":memory:?_pragma=foreign_keys(on)&_time_format=sqlite")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	database.SetMaxOpenConns(1)
	database.SetMaxIdleConns(1)
	t.Cleanup(func() { database.Close() })
	if err := db.BootstrapSchemaForTest(database); err != nil {
		t.Fatalf("bootstrap schema: %v", err)
	}
	if _, err := database.Exec(
		`INSERT OR IGNORE INTO agents (id, org_id, display_name) VALUES (?, ?, 'Test Bot')`,
		runmode.LocalDefaultAgentID, runmode.LocalDefaultOrgID,
	); err != nil {
		t.Fatalf("seed local agent: %v", err)
	}
	return New(database, sqlitestore.New(database))
}

// seedUsageLocal seeds a cross-source spend fixture on the local sentinel
// org/team/user: a manual run + an autonomous run (fired by a seeded trigger
// whose blueprint is named) + a team-attributed curator turn + a NULL-team
// curator turn + a system job. Returns the trigger id and its blueprint name.
func seedUsageLocal(t *testing.T, s *Server) (triggerID, blueprintName string) {
	t.Helper()
	ctx := context.Background()
	org := runmode.LocalDefaultOrgID
	team := runmode.LocalDefaultTeamID
	user := runmode.LocalDefaultUserID
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := s.db.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("seed exec failed: %v\nSQL: %s", err, q)
		}
	}

	blueprintName = "CI Fix"
	const bpID = "bp-usage"
	exec(`INSERT INTO blueprints (id, name, org_id, team_id, creator_user_id) VALUES (?, ?, ?, ?, ?)`,
		bpID, blueprintName, org, team, user)
	triggerID = "trg-usage"
	exec(`INSERT INTO event_handlers
			(id, org_id, team_id, creator_user_id, kind, event_type, blueprint_id, breaker_threshold, min_autonomy_suitability)
		 VALUES (?, ?, ?, ?, 'trigger', 'github:pr:ci_check_failed', ?, 3, 0.5)`,
		triggerID, org, team, user, bpID)

	// Projects back the curator conversations (conversations.project_id FK).
	exec(`INSERT INTO projects (id, name, team_id, org_id, creator_user_id, visibility) VALUES ('p-team', 'team-proj', ?, ?, ?, 'team')`, team, org, user)
	exec(`INSERT INTO projects (id, name, team_id, org_id, creator_user_id, visibility) VALUES ('p-org', 'org-proj', NULL, ?, ?, 'org')`, org, user)

	// SQLite canonical datetime text ('YYYY-MM-DD HH:MM:SS') so both the view's
	// datetime() filter and the _time_format=sqlite scan round-trip cleanly.
	const t1 = "2026-06-15 10:00:00"
	const t2 = "2026-06-16 10:00:00"

	// Spend is the messages ledger now: each conversation gets ONE
	// cost-stamped assistant row carrying the model/tokens the view reads.
	// Manual run by the user (team-scoped).
	exec(`INSERT INTO conversations
			(id, org_id, team_id, creator_user_id, trigger_type, origin, model, status, started_at)
		 VALUES ('r-manual', ?, ?, ?, 'manual', 'manual', 'claude-opus-4-8', 'completed', ?)`,
		org, team, user, t1)
	exec(`INSERT INTO messages
			(org_id, conversation_id, role, subtype, content, model, cost_usd,
			 input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens, created_at)
		 VALUES (?, 'r-manual', 'assistant', '', 'work', 'claude-opus-4-8', 1.00, 100, 10, 5, 1, ?)`,
		org, t1)
	// Autonomous run fired by the trigger (NULL creator, team-scoped).
	exec(`INSERT INTO conversations
			(id, org_id, team_id, creator_user_id, trigger_type, origin, trigger_id, model, status, started_at)
		 VALUES ('r-auto', ?, ?, NULL, 'event', 'manual', ?, 'claude-haiku-4-5', 'completed', ?)`,
		org, team, triggerID, t2)
	exec(`INSERT INTO messages
			(org_id, conversation_id, role, subtype, content, model, cost_usd,
			 input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens, created_at)
		 VALUES (?, 'r-auto', 'assistant', '', 'work', 'claude-haiku-4-5', 0.25, 20, 2, 0, 0, ?)`,
		org, t2)
	// Curator turns: a curator conversation plus one cost-stamped ledger row
	// (curator rows carry no model on the wire, so model stays NULL).
	// Team-attributed curator turn by the user.
	exec(`INSERT INTO conversations
			(id, org_id, type, creator_user_id, team_id, visibility, trigger_type, origin, status, project_id, started_at)
		 VALUES ('conv-cur-team', ?, 'curator', ?, ?, 'private', 'manual', 'curator', NULL, 'p-team', ?)`,
		org, user, team, t1)
	exec(`INSERT INTO messages
			(org_id, conversation_id, role, subtype, content, cost_usd,
			 input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens, created_at)
		 VALUES (?, 'conv-cur-team', 'assistant', '', 'ack', 0.50, 11, 1, 0, 0, ?)`,
		org, t1)
	// NULL-team curator turn by the user (org-visibility project).
	exec(`INSERT INTO conversations
			(id, org_id, type, creator_user_id, team_id, visibility, trigger_type, origin, status, project_id, started_at)
		 VALUES ('conv-cur-org', ?, 'curator', ?, NULL, 'private', 'manual', 'curator', NULL, 'p-org', ?)`,
		org, user, t2)
	exec(`INSERT INTO messages
			(org_id, conversation_id, role, subtype, content, cost_usd,
			 input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens, created_at)
		 VALUES (?, 'conv-cur-org', 'assistant', '', 'ack', 0.10, 1, 1, 0, 0, ?)`,
		org, t2)
	// System job (org-level, NULL team).
	exec(`INSERT INTO system_llm_runs
			(id, org_id, job, model, total_cost_usd, input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens, started_at)
		 VALUES ('sys-1', ?, 'scorer', 'claude-haiku-4-5', 0.05, 2, 1, 0, 0, ?)`,
		org, t2)
	return triggerID, blueprintName
}
