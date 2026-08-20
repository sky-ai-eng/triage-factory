package delegate

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/entitlements"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	_ "modernc.org/sqlite"
)

// newCostCapTestDB mirrors newDelegateTestDB but adds _time_format=sqlite — the
// same DSN production's db.OpenAt uses — so a bound time.Time and the stored
// started_at serialize to a datetime()-parseable form. The llm_spend view's
// SpendByCategory time filter wraps both sides in datetime(); without the
// production time format the wrapper sees modernc's default time.String()
// serialization (unparseable) and silently drops every row, which would make
// the cap read $0 and never trip.
func newCostCapTestDB(t *testing.T) *sql.DB {
	t.Helper()
	database, err := sql.Open("sqlite", db.TestDSNMemory)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	database.SetMaxOpenConns(1)
	database.SetMaxIdleConns(1)
	t.Cleanup(func() { database.Close() })
	if err := db.BootstrapSchemaForTest(database); err != nil {
		t.Fatalf("bootstrap schema: %v", err)
	}
	return database
}

// setDailyCostCap read-modify-writes the local org's daily spend cap, preserving
// the other settings fields.
func setDailyCostCap(t *testing.T, database *sql.DB, cap float64) {
	t.Helper()
	store := sqlitestore.New(database).Orgs
	set, err := store.GetSettingsSystem(context.Background(), runmode.LocalDefaultOrgID)
	if err != nil {
		t.Fatalf("get settings: %v", err)
	}
	set.MaxDailyCostUSD = cap
	if _, err := store.UpdateSettings(context.Background(), runmode.LocalDefaultOrgID, set); err != nil {
		t.Fatalf("set daily cost cap: %v", err)
	}
}

// seedSpendAt inserts a settled run at the given occurred-at instant: a
// delegation conversation plus one cost-stamped ledger row — the minimal
// llm_spend-visible shape (mirrors the SQLite spend conformance seeder:
// origin='manual' sidesteps the blueprint-parents CHECK; tokens 0). Used to
// push today's org spend above/below the cap.
func seedSpendAt(t *testing.T, database *sql.DB, cost float64, occurredAt time.Time) {
	t.Helper()
	convID := uuid.New().String()
	if _, err := database.Exec(`
		INSERT INTO conversations
			(id, org_id, team_id, creator_user_id, trigger_type, origin, model, status, started_at)
		VALUES (?, ?, ?, ?, 'manual', 'manual', 'm', 'completed', ?)
	`,
		convID, runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID,
		runmode.LocalDefaultUserID, occurredAt,
	); err != nil {
		t.Fatalf("seed spend run: %v", err)
	}
	if _, err := database.Exec(`
		INSERT INTO messages (org_id, conversation_id, role, subtype, content, cost_usd, created_at)
		VALUES (?, ?, 'assistant', '', 'work', ?, ?)
	`, runmode.LocalDefaultOrgID, convID, cost, occurredAt); err != nil {
		t.Fatalf("seed spend ledger row: %v", err)
	}
}

// seedSystemSpendAt inserts a settled system_llm_runs row — a NULL-team system
// job (scorer/classifier/profiler overhead) — at the given instant. The
// llm_spend view surfaces it with a NULL team_id, so it counts toward the ORG cap
// but never a team cap (which filters on team_id). Used to prove the per-team cap
// excludes system spend.
func seedSystemSpendAt(t *testing.T, database *sql.DB, cost float64, occurredAt time.Time) {
	t.Helper()
	if _, err := database.Exec(`
		INSERT INTO system_llm_runs
			(id, org_id, job, model, total_cost_usd,
			 input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens, started_at)
		VALUES (?, ?, 'scorer', 'm', ?, 0, 0, 0, 0, ?)
	`, uuid.New().String(), runmode.LocalDefaultOrgID, cost, occurredAt); err != nil {
		t.Fatalf("seed system spend: %v", err)
	}
}

// --- checkDailyCostCap unit tests ------------------------------------------

func TestCheckDailyCostCap_NoCap_AllowsEvenWithSpend(t *testing.T) {
	database := newCostCapTestDB(t)
	// Cap left at the seeded default (0 / NULL = no cap). Spend present.
	seedSpendAt(t, database, 999, time.Now().UTC())

	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6")
	if err := s.checkDailyCostCap(context.Background(), runmode.LocalDefaultOrgID); err != nil {
		t.Fatalf("no cap (0) must allow delegation, got: %v", err)
	}
}

func TestCheckDailyCostCap_UnderCap_Allows(t *testing.T) {
	database := newCostCapTestDB(t)
	setDailyCostCap(t, database, 10)
	seedSpendAt(t, database, 4.50, time.Now().UTC())

	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6")
	if err := s.checkDailyCostCap(context.Background(), runmode.LocalDefaultOrgID); err != nil {
		t.Fatalf("spend below cap must allow delegation, got: %v", err)
	}
}

func TestCheckDailyCostCap_AtOrOverCap_Blocks(t *testing.T) {
	for _, tc := range []struct {
		name string
		cap  float64
		// two rows so the cap sums ACROSS categories/rows, not just one.
		spendA, spendB float64
	}{
		{"exactly at cap", 10, 6, 4}, // 10 >= 10 trips (trip is >=, not >)
		{"over cap", 10, 8, 5},       // 13 >= 10
	} {
		t.Run(tc.name, func(t *testing.T) {
			database := newCostCapTestDB(t)
			setDailyCostCap(t, database, tc.cap)
			now := time.Now().UTC()
			seedSpendAt(t, database, tc.spendA, now)
			seedSpendAt(t, database, tc.spendB, now)

			s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6")
			err := s.checkDailyCostCap(context.Background(), runmode.LocalDefaultOrgID)
			if !errors.Is(err, ErrDailyCostCapReached) {
				t.Fatalf("spend at/over cap must return ErrDailyCostCapReached, got: %v", err)
			}
		})
	}
}

func TestCheckDailyCostCap_OnlyTodayCounts(t *testing.T) {
	database := newCostCapTestDB(t)
	setDailyCostCap(t, database, 10)
	// $100 spent YESTERDAY must not count toward today's UTC-day total.
	yesterday := time.Now().UTC().Add(-25 * time.Hour)
	seedSpendAt(t, database, 100, yesterday)

	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6")
	if err := s.checkDailyCostCap(context.Background(), runmode.LocalDefaultOrgID); err != nil {
		t.Fatalf("yesterday's spend must not trip today's cap, got: %v", err)
	}

	// Adding today's spend over the cap now trips it — proving the boundary is
	// the discriminator, not the presence of any spend at all.
	seedSpendAt(t, database, 11, time.Now().UTC())
	if err := s.checkDailyCostCap(context.Background(), runmode.LocalDefaultOrgID); !errors.Is(err, ErrDailyCostCapReached) {
		t.Fatalf("today's over-cap spend must trip, got: %v", err)
	}
}

func TestCheckDailyCostCap_NilStores_NoOp(t *testing.T) {
	// A partially-wired spawner (no orgs/spend stores) must treat the cap as
	// absent rather than panic — the defensive nil guard.
	s := &Spawner{}
	if err := s.checkDailyCostCap(context.Background(), runmode.LocalDefaultOrgID); err != nil {
		t.Fatalf("nil stores must no-op, got: %v", err)
	}
}

// --- fail-open on read error -----------------------------------------------

// stubOrgsSettings overrides only GetSettingsSystem (the sole OrgsStore method
// checkDailyCostCap calls). The embedded interface satisfies the rest.
type stubOrgsSettings struct {
	db.OrgsStore
	settings domain.OrgSettings
	err      error
}

func (s stubOrgsSettings) GetSettingsSystem(context.Context, string) (domain.OrgSettings, error) {
	return s.settings, s.err
}

// stubSpend overrides only SpendByCategorySystem.
type stubSpend struct {
	db.SpendStore
	buckets []domain.SpendBucket
	err     error
}

func (s stubSpend) SpendByCategorySystem(context.Context, string, time.Time, time.Time) ([]domain.SpendBucket, error) {
	return s.buckets, s.err
}

func TestCheckDailyCostCap_FailsOpenOnSettingsError(t *testing.T) {
	// Settings read fails → allow (fail open). spend is never reached, but wire
	// a non-nil one so the nil guard doesn't short-circuit first.
	s := &Spawner{
		orgs:  stubOrgsSettings{err: errors.New("settings boom")},
		spend: stubSpend{},
	}
	if err := s.checkDailyCostCap(context.Background(), runmode.LocalDefaultOrgID); err != nil {
		t.Fatalf("settings read error must fail open (allow), got: %v", err)
	}
}

func TestCheckDailyCostCap_FailsOpenOnSpendError(t *testing.T) {
	// A real cap is configured (so we get past the no-cap short-circuit), but the
	// spend read fails → allow (fail open). A read failure must not wedge all
	// delegation.
	s := &Spawner{
		orgs:  stubOrgsSettings{settings: domain.OrgSettings{MaxDailyCostUSD: 10}},
		spend: stubSpend{err: errors.New("spend boom")},
	}
	if err := s.checkDailyCostCap(context.Background(), runmode.LocalDefaultOrgID); err != nil {
		t.Fatalf("spend read error must fail open (allow), got: %v", err)
	}
}

// --- Delegate end-to-end ----------------------------------------------------

// delegatableFixture stands up a 1-step blueprint on a fresh GitHub task, ready
// for a manual Delegate. Returns the task and the blueprint id to fire.
func delegatableFixture(t *testing.T, database *sql.DB, suffix string) (domain.Task, string) {
	t.Helper()
	ctx := context.Background()
	org := runmode.LocalDefaultOrgID
	stores := sqlitestore.New(database)

	entity, _, err := stores.Entities.FindOrCreate(ctx, org, "github", "owner/repo#"+suffix, "pr", "T", "https://x/"+suffix)
	if err != nil {
		t.Fatalf("entity: %v", err)
	}
	eventID, err := stores.Events.Record(ctx, org, domain.Event{
		EventType: domain.EventGitHubPRCICheckFailed, EntityID: &entity.ID, MetadataJSON: `{}`,
	})
	if err != nil {
		t.Fatalf("event: %v", err)
	}
	task, _, err := stores.Tasks.FindOrCreate(ctx, org, runmode.LocalDefaultTeamID, entity.ID, domain.EventGitHubPRCICheckFailed, suffix, eventID, 0.5)
	if err != nil {
		t.Fatalf("task: %v", err)
	}

	bpID := "capbp-" + suffix
	if _, err := stores.Blueprints.Create(ctx, org, runmode.LocalDefaultTeamID, domain.Blueprint{
		ID: bpID, Name: bpID, Source: "user", TeamID: runmode.LocalDefaultTeamID,
	}); err != nil {
		t.Fatalf("blueprint: %v", err)
	}
	pid := "capp-" + suffix
	ensureTestPrompt(t, database, domain.Prompt{ID: pid, Name: pid, Body: "b", Source: "user"})
	if _, err := stores.Blueprints.ReplaceSteps(ctx, org, bpID, []string{pid}, nil); err != nil {
		t.Fatalf("ReplaceSteps: %v", err)
	}
	return *task, bpID
}

func countBlueprintRuns(t *testing.T, database *sql.DB, taskID string) int {
	t.Helper()
	var n int
	if err := database.QueryRow(`SELECT COUNT(*) FROM blueprint_runs WHERE task_id = ?`, taskID).Scan(&n); err != nil {
		t.Fatalf("count blueprint_runs: %v", err)
	}
	return n
}

// TestDelegate_DailyCostCap_BlocksAndMintsNoBlueprintRun is the core acceptance
// test: with the cap tripped, a manual Delegate returns ErrDailyCostCapReached
// and creates NO blueprint_run row — the choke point refuses the spawn before
// any DB write.
func TestDelegate_DailyCostCap_BlocksAndMintsNoBlueprintRun(t *testing.T) {
	database := newCostCapTestDB(t)
	task, bpID := delegatableFixture(t, database, "blocked")
	setDailyCostCap(t, database, 10)
	seedSpendAt(t, database, 12, time.Now().UTC()) // over cap

	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6")
	conversationID, err := s.Delegate(task, DelegateOpts{
		OrgID:               runmode.LocalDefaultOrgID,
		ExplicitBlueprintID: bpID,
		TriggerType:         "manual",
		CreatorUserID:       runmode.LocalDefaultUserID,
	})
	if !errors.Is(err, ErrDailyCostCapReached) {
		t.Fatalf("Delegate over cap must return ErrDailyCostCapReached, got conversationID=%q err=%v", conversationID, err)
	}
	if conversationID != "" {
		t.Errorf("blocked Delegate must return empty conversationID, got %q", conversationID)
	}
	if n := countBlueprintRuns(t, database, task.ID); n != 0 {
		t.Errorf("blocked Delegate minted %d blueprint_run rows; want 0 (no DB write past the cap)", n)
	}
}

// TestDelegate_UnderCap_Proceeds is the inverse: with spend below the cap, the
// same manual Delegate runs to completion and mints its blueprint_run.
func TestDelegate_UnderCap_Proceeds(t *testing.T) {
	database := newCostCapTestDB(t)
	task, bpID := delegatableFixture(t, database, "allowed")
	setDailyCostCap(t, database, 100)
	seedSpendAt(t, database, 5, time.Now().UTC()) // well under cap

	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6")
	conversationID, err := s.Delegate(task, DelegateOpts{
		OrgID:               runmode.LocalDefaultOrgID,
		ExplicitBlueprintID: bpID,
		TriggerType:         "manual",
		CreatorUserID:       runmode.LocalDefaultUserID,
	})
	if err != nil {
		t.Fatalf("Delegate under cap must proceed, got err=%v", err)
	}
	if conversationID == "" {
		t.Error("successful Delegate must return a blueprint_run id")
	}
	if n := countBlueprintRuns(t, database, task.ID); n != 1 {
		t.Errorf("under-cap Delegate minted %d blueprint_run rows; want 1", n)
	}
}

// TestDelegate_NoCap_Proceeds proves the default (cap unset / 0) is unaffected:
// even with spend on the books, delegation runs normally.
func TestDelegate_NoCap_Proceeds(t *testing.T) {
	database := newCostCapTestDB(t)
	task, bpID := delegatableFixture(t, database, "nocap")
	seedSpendAt(t, database, 9999, time.Now().UTC()) // huge spend, but no cap set

	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6")
	conversationID, err := s.Delegate(task, DelegateOpts{
		OrgID:               runmode.LocalDefaultOrgID,
		ExplicitBlueprintID: bpID,
		TriggerType:         "manual",
		CreatorUserID:       runmode.LocalDefaultUserID,
	})
	if err != nil {
		t.Fatalf("Delegate with no cap must proceed, got err=%v", err)
	}
	if conversationID == "" || countBlueprintRuns(t, database, task.ID) != 1 {
		t.Errorf("no-cap Delegate should mint a blueprint_run; conversationID=%q", conversationID)
	}
}

// --- per-team daily cost cap (TFAC-482) ----------------------------------

// licenseGovernance registers a per-org provider granting ONLY
// FeatureGovernance — the EE entitlement the per-team cap gates on — for
// every org, and resets on cleanup.
func licenseGovernance(t *testing.T) {
	t.Helper()
	entitlements.RegisterProvider(entitlements.Static(entitlements.FeatureGovernance))
	t.Cleanup(entitlements.Reset)
}

// setTeamDailyCostCap sets the local team's per-team daily spend cap via the
// org-admin write path (SetDailyCostCapSystem), mirroring setDailyCostCap for the
// org tier.
func setTeamDailyCostCap(t *testing.T, database *sql.DB, cap float64) {
	t.Helper()
	store := sqlitestore.New(database).Teams
	if _, err := store.SetDailyCostCapSystem(context.Background(), runmode.LocalDefaultTeamID, cap); err != nil {
		t.Fatalf("set team daily cost cap: %v", err)
	}
}

// stubTeamsSettings overrides only GetSettingsSystem (the sole TeamsStore method
// checkTeamDailyCostCap calls). The embedded interface satisfies the rest.
type stubTeamsSettings struct {
	db.TeamsStore
	settings domain.TeamSettings
	err      error
}

func (s stubTeamsSettings) GetSettingsSystem(context.Context, string) (domain.TeamSettings, error) {
	return s.settings, s.err
}

// SpendByCategorySystemForTeam extends the existing stubSpend so it can drive the
// per-team cap path's fail-open test (and any wiring that reaches it).
func (s stubSpend) SpendByCategorySystemForTeam(context.Context, string, string, time.Time, time.Time) ([]domain.SpendBucket, error) {
	return s.buckets, s.err
}

// --- checkTeamDailyCostCap unit tests --------------------------------------

func TestCheckTeamDailyCostCap_AtOrOverCap_Blocks(t *testing.T) {
	database := newCostCapTestDB(t)
	licenseGovernance(t)
	setTeamDailyCostCap(t, database, 10)
	// Two team-attributed rows so the cap sums across rows, like the org test.
	now := time.Now().UTC()
	seedSpendAt(t, database, 6, now)
	seedSpendAt(t, database, 5, now) // 11 >= 10 trips

	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6")
	err := s.checkTeamDailyCostCap(context.Background(), runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID)
	if !errors.Is(err, ErrDailyCostCapReached) {
		t.Fatalf("team spend at/over cap must return ErrDailyCostCapReached, got: %v", err)
	}
}

func TestCheckTeamDailyCostCap_UnderCap_Allows(t *testing.T) {
	database := newCostCapTestDB(t)
	licenseGovernance(t)
	setTeamDailyCostCap(t, database, 10)
	seedSpendAt(t, database, 4.50, time.Now().UTC())

	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6")
	if err := s.checkTeamDailyCostCap(context.Background(), runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID); err != nil {
		t.Fatalf("team spend below cap must allow, got: %v", err)
	}
}

func TestCheckTeamDailyCostCap_NoCap_Allows(t *testing.T) {
	database := newCostCapTestDB(t)
	licenseGovernance(t)
	// No team cap set (0 / NULL = no cap), spend present.
	seedSpendAt(t, database, 999, time.Now().UTC())

	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6")
	if err := s.checkTeamDailyCostCap(context.Background(), runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID); err != nil {
		t.Fatalf("no team cap (0) must allow, got: %v", err)
	}
}

func TestCheckTeamDailyCostCap_EntitlementOff_Dormant(t *testing.T) {
	database := newCostCapTestDB(t)
	// Governance NOT licensed (default community checker) → per-team cap dormant.
	setTeamDailyCostCap(t, database, 10)
	seedSpendAt(t, database, 50, time.Now().UTC()) // way over, but dormant

	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6")
	if err := s.checkTeamDailyCostCap(context.Background(), runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID); err != nil {
		t.Fatalf("unlicensed governance must make the per-team cap dormant, got: %v", err)
	}
}

func TestCheckTeamDailyCostCap_OnlyTeamRowsCount(t *testing.T) {
	// A team cap sums team_id rows only — system overhead + non-team curator
	// (NULL team_id) never count. seedSpendAt writes a team row; seedSystemSpendAt
	// writes a NULL-team system row that must be ignored.
	database := newCostCapTestDB(t)
	licenseGovernance(t)
	setTeamDailyCostCap(t, database, 10)
	now := time.Now().UTC()
	seedSystemSpendAt(t, database, 100, now) // NULL team → excluded from the team cap
	seedSpendAt(t, database, 4, now)         // team row → 4 < 10, under cap

	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6")
	if err := s.checkTeamDailyCostCap(context.Background(), runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID); err != nil {
		t.Fatalf("system spend must not count toward the team cap, got: %v", err)
	}
}

func TestCheckTeamDailyCostCap_NilStores_NoOp(t *testing.T) {
	licenseGovernance(t) // past the entitlement gate, into the nil-store guard
	s := &Spawner{}
	if err := s.checkTeamDailyCostCap(context.Background(), runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID); err != nil {
		t.Fatalf("nil stores must no-op, got: %v", err)
	}
}

func TestCheckTeamDailyCostCap_FailsOpenOnSettingsError(t *testing.T) {
	licenseGovernance(t)
	s := &Spawner{
		teams: stubTeamsSettings{err: errors.New("settings boom")},
		spend: stubSpend{},
	}
	if err := s.checkTeamDailyCostCap(context.Background(), runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID); err != nil {
		t.Fatalf("team settings read error must fail open (allow), got: %v", err)
	}
}

func TestCheckTeamDailyCostCap_FailsOpenOnSpendError(t *testing.T) {
	licenseGovernance(t)
	s := &Spawner{
		teams: stubTeamsSettings{settings: domain.TeamSettings{MaxDailyCostUSD: 10}},
		spend: stubSpend{err: errors.New("spend boom")},
	}
	if err := s.checkTeamDailyCostCap(context.Background(), runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID); err != nil {
		t.Fatalf("team spend read error must fail open (allow), got: %v", err)
	}
}

// --- Delegate end-to-end (team cap) ----------------------------------------

func TestDelegate_TeamDailyCostCap_BlocksAndMintsNoBlueprintRun(t *testing.T) {
	database := newCostCapTestDB(t)
	licenseGovernance(t)
	task, bpID := delegatableFixture(t, database, "teamblocked")
	// No org cap; only the team cap is tripped — proving the team check is what
	// blocks (the org check runs first and passes).
	setTeamDailyCostCap(t, database, 10)
	seedSpendAt(t, database, 12, time.Now().UTC()) // team over cap

	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6")
	conversationID, err := s.Delegate(task, DelegateOpts{
		OrgID:               runmode.LocalDefaultOrgID,
		ExplicitBlueprintID: bpID,
		TriggerType:         "manual",
		CreatorUserID:       runmode.LocalDefaultUserID,
	})
	if !errors.Is(err, ErrDailyCostCapReached) {
		t.Fatalf("Delegate over team cap must return ErrDailyCostCapReached, got conversationID=%q err=%v", conversationID, err)
	}
	if conversationID != "" {
		t.Errorf("blocked Delegate must return empty conversationID, got %q", conversationID)
	}
	if n := countBlueprintRuns(t, database, task.ID); n != 0 {
		t.Errorf("team-cap-blocked Delegate minted %d blueprint_run rows; want 0", n)
	}
}

func TestDelegate_TeamUnderCap_Proceeds(t *testing.T) {
	database := newCostCapTestDB(t)
	licenseGovernance(t)
	task, bpID := delegatableFixture(t, database, "teamallowed")
	setTeamDailyCostCap(t, database, 100)
	seedSpendAt(t, database, 5, time.Now().UTC()) // well under

	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6")
	conversationID, err := s.Delegate(task, DelegateOpts{
		OrgID:               runmode.LocalDefaultOrgID,
		ExplicitBlueprintID: bpID,
		TriggerType:         "manual",
		CreatorUserID:       runmode.LocalDefaultUserID,
	})
	if err != nil {
		t.Fatalf("Delegate under team cap must proceed, got err=%v", err)
	}
	if conversationID == "" || countBlueprintRuns(t, database, task.ID) != 1 {
		t.Errorf("under-team-cap Delegate should mint one blueprint_run; conversationID=%q", conversationID)
	}
}

func TestDelegate_TeamCap_EntitlementOff_Ignored(t *testing.T) {
	database := newCostCapTestDB(t)
	// Governance NOT licensed → the team cap is dormant; the org cap (unset here)
	// is the only safety net, so an over-team-cap spawn proceeds.
	task, bpID := delegatableFixture(t, database, "teamdormant")
	setTeamDailyCostCap(t, database, 10)
	seedSpendAt(t, database, 50, time.Now().UTC()) // over the (dormant) team cap

	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6")
	conversationID, err := s.Delegate(task, DelegateOpts{
		OrgID:               runmode.LocalDefaultOrgID,
		ExplicitBlueprintID: bpID,
		TriggerType:         "manual",
		CreatorUserID:       runmode.LocalDefaultUserID,
	})
	if err != nil {
		t.Fatalf("unlicensed governance → team cap dormant → Delegate must proceed, got err=%v", err)
	}
	if conversationID == "" || countBlueprintRuns(t, database, task.ID) != 1 {
		t.Errorf("dormant-team-cap Delegate should mint one blueprint_run; conversationID=%q", conversationID)
	}
}

func TestDelegate_TeamCap_UnownedTaskSkipped(t *testing.T) {
	database := newCostCapTestDB(t)
	licenseGovernance(t)
	task, bpID := delegatableFixture(t, database, "teamunowned")
	task.TeamID = nil // unowned task: no team to cap → the team check is skipped
	// A tripped cap on the local team that the unowned task is NOT on; it must not
	// block the spawn (the Delegate call site skips the check for teamID == "").
	setTeamDailyCostCap(t, database, 10)
	seedSpendAt(t, database, 50, time.Now().UTC())

	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6")
	conversationID, err := s.Delegate(task, DelegateOpts{
		OrgID:               runmode.LocalDefaultOrgID,
		ExplicitBlueprintID: bpID,
		TriggerType:         "manual",
		CreatorUserID:       runmode.LocalDefaultUserID,
	})
	if err != nil {
		t.Fatalf("unowned task (teamID \"\") must skip the team cap and proceed, got err=%v", err)
	}
	if conversationID == "" || countBlueprintRuns(t, database, task.ID) != 1 {
		t.Errorf("unowned-task Delegate should mint one blueprint_run; conversationID=%q", conversationID)
	}
}
