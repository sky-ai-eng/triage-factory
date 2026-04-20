package db

import (
	"database/sql"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// newTestDB spins up an in-memory SQLite database with the full schema + seed
// so CRUD tests run against a realistic FK graph (entities, events_catalog,
// task_rules constraints). Each test gets its own isolated DB.
func newTestDB(t *testing.T) *sql.DB {
	t.Helper()
	database, err := sql.Open("sqlite3", ":memory:?_foreign_keys=on")
	if err != nil {
		t.Fatalf("open sqlite memory: %v", err)
	}
	// Force single connection — SQLite :memory: is per-connection, so a
	// pooled second connection would get a blank database without the
	// schema from Migrate.
	database.SetMaxOpenConns(1)
	database.SetMaxIdleConns(1)
	t.Cleanup(func() { database.Close() })

	if err := Migrate(database); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	// Seed events_catalog so FK constraints on task_rules.event_type hold.
	if err := SeedEventTypes(database); err != nil {
		t.Fatalf("seed events: %v", err)
	}
	return database
}

func TestListTaskRules_Empty(t *testing.T) {
	db := newTestDB(t)
	rules, err := ListTaskRules(db)
	if err != nil {
		t.Fatalf("ListTaskRules: %v", err)
	}
	if len(rules) != 0 {
		t.Errorf("expected 0 rules on empty DB, got %d", len(rules))
	}
}

func TestSeedAndList(t *testing.T) {
	db := newTestDB(t)
	if err := SeedTaskRules(db); err != nil {
		t.Fatalf("SeedTaskRules: %v", err)
	}
	rules, err := ListTaskRules(db)
	if err != nil {
		t.Fatalf("ListTaskRules: %v", err)
	}
	if len(rules) != 6 {
		t.Errorf("expected 6 seeded rules, got %d", len(rules))
	}
	// Verify sort_order ordering.
	for i := 1; i < len(rules); i++ {
		if rules[i].SortOrder < rules[i-1].SortOrder {
			t.Errorf("rules not sorted by sort_order: %d < %d", rules[i].SortOrder, rules[i-1].SortOrder)
		}
	}
	// All seeded rules should be source=system and enabled.
	for _, r := range rules {
		if r.Source != "system" {
			t.Errorf("seeded rule %s has source=%q, want system", r.ID, r.Source)
		}
		if !r.Enabled {
			t.Errorf("seeded rule %s is disabled by default", r.ID)
		}
	}
}

func TestCreateAndGet(t *testing.T) {
	db := newTestDB(t)
	pred := `{"author_is_self":true}`
	rule := domain.TaskRule{
		ID:                 "user-rule-1",
		EventType:          "github:pr:new_commits",
		ScopePredicateJSON: &pred,
		Enabled:            true,
		Name:               "Test rule",
		DefaultPriority:    0.7,
		SortOrder:          5,
	}
	if err := CreateTaskRule(db, rule); err != nil {
		t.Fatalf("CreateTaskRule: %v", err)
	}

	got, err := GetTaskRule(db, "user-rule-1")
	if err != nil {
		t.Fatalf("GetTaskRule: %v", err)
	}
	if got == nil {
		t.Fatal("GetTaskRule returned nil, expected rule")
	}
	if got.Source != "user" {
		t.Errorf("expected source=user, got %s", got.Source)
	}
	if got.EventType != rule.EventType {
		t.Errorf("event_type mismatch: got %s, want %s", got.EventType, rule.EventType)
	}
	if got.ScopePredicateJSON == nil || *got.ScopePredicateJSON != pred {
		t.Errorf("predicate mismatch: got %v, want %q", got.ScopePredicateJSON, pred)
	}
	if got.DefaultPriority != 0.7 {
		t.Errorf("priority mismatch: got %v", got.DefaultPriority)
	}
	if got.CreatedAt.IsZero() || got.UpdatedAt.IsZero() {
		t.Error("timestamps not set")
	}
}

func TestGetTaskRule_NotFound(t *testing.T) {
	db := newTestDB(t)
	got, err := GetTaskRule(db, "does-not-exist")
	if err != nil {
		t.Fatalf("GetTaskRule: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil for missing rule, got %v", got)
	}
}

func TestUpdateTaskRule_PreservesImmutables(t *testing.T) {
	db := newTestDB(t)
	rule := domain.TaskRule{
		ID:              "user-rule-1",
		EventType:       "github:pr:new_commits",
		Enabled:         true,
		Name:            "Original",
		DefaultPriority: 0.5,
		SortOrder:       0,
	}
	if err := CreateTaskRule(db, rule); err != nil {
		t.Fatalf("CreateTaskRule: %v", err)
	}
	before, _ := GetTaskRule(db, "user-rule-1")
	originalCreatedAt := before.CreatedAt
	originalSource := before.Source

	// Wait enough for updated_at to differ.
	time.Sleep(10 * time.Millisecond)

	rule.Name = "Updated"
	rule.DefaultPriority = 0.9
	if err := UpdateTaskRule(db, rule); err != nil {
		t.Fatalf("UpdateTaskRule: %v", err)
	}

	after, _ := GetTaskRule(db, "user-rule-1")
	if after.Name != "Updated" {
		t.Errorf("name not updated: %s", after.Name)
	}
	if after.DefaultPriority != 0.9 {
		t.Errorf("priority not updated: %v", after.DefaultPriority)
	}
	if !after.CreatedAt.Equal(originalCreatedAt) {
		t.Errorf("created_at changed: %v → %v", originalCreatedAt, after.CreatedAt)
	}
	if after.Source != originalSource {
		t.Errorf("source changed: %s → %s", originalSource, after.Source)
	}
	if !after.UpdatedAt.After(before.UpdatedAt) {
		t.Errorf("updated_at did not advance: %v → %v", before.UpdatedAt, after.UpdatedAt)
	}
}

func TestSetTaskRuleEnabled(t *testing.T) {
	db := newTestDB(t)
	rule := domain.TaskRule{
		ID:              "user-rule-1",
		EventType:       "github:pr:new_commits",
		Enabled:         true,
		Name:            "Test",
		DefaultPriority: 0.5,
	}
	if err := CreateTaskRule(db, rule); err != nil {
		t.Fatalf("CreateTaskRule: %v", err)
	}

	if err := SetTaskRuleEnabled(db, "user-rule-1", false); err != nil {
		t.Fatalf("SetTaskRuleEnabled: %v", err)
	}
	got, _ := GetTaskRule(db, "user-rule-1")
	if got.Enabled {
		t.Error("rule should be disabled")
	}

	if err := SetTaskRuleEnabled(db, "user-rule-1", true); err != nil {
		t.Fatalf("SetTaskRuleEnabled: %v", err)
	}
	got, _ = GetTaskRule(db, "user-rule-1")
	if !got.Enabled {
		t.Error("rule should be re-enabled")
	}
}

func TestDeleteTaskRule(t *testing.T) {
	db := newTestDB(t)
	rule := domain.TaskRule{
		ID:              "user-rule-1",
		EventType:       "github:pr:new_commits",
		Enabled:         true,
		Name:            "Test",
		DefaultPriority: 0.5,
	}
	if err := CreateTaskRule(db, rule); err != nil {
		t.Fatalf("CreateTaskRule: %v", err)
	}

	if err := DeleteTaskRule(db, "user-rule-1"); err != nil {
		t.Fatalf("DeleteTaskRule: %v", err)
	}
	got, _ := GetTaskRule(db, "user-rule-1")
	if got != nil {
		t.Error("rule should be gone after delete")
	}
}

func TestGetEnabledRulesForEvent_FiltersDisabledAndEventType(t *testing.T) {
	db := newTestDB(t)

	a := domain.TaskRule{ID: "a", EventType: "github:pr:new_commits", Enabled: true, Name: "A", DefaultPriority: 0.5}
	b := domain.TaskRule{ID: "b", EventType: "github:pr:new_commits", Enabled: false, Name: "B (disabled)", DefaultPriority: 0.5}
	c := domain.TaskRule{ID: "c", EventType: "github:pr:ci_check_failed", Enabled: true, Name: "C (other event)", DefaultPriority: 0.5}
	for _, r := range []domain.TaskRule{a, b, c} {
		if err := CreateTaskRule(db, r); err != nil {
			t.Fatalf("CreateTaskRule %s: %v", r.ID, err)
		}
	}

	got, err := GetEnabledRulesForEvent(db, "github:pr:new_commits")
	if err != nil {
		t.Fatalf("GetEnabledRulesForEvent: %v", err)
	}
	if len(got) != 1 || got[0].ID != "a" {
		t.Errorf("expected only rule a, got %v", got)
	}
}

func TestSeedTaskRules_Idempotent(t *testing.T) {
	db := newTestDB(t)
	if err := SeedTaskRules(db); err != nil {
		t.Fatalf("first seed: %v", err)
	}
	// Modify a seeded rule to verify re-seed doesn't overwrite.
	if err := SetTaskRuleEnabled(db, "system-rule-ci-check-failed", false); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if err := SeedTaskRules(db); err != nil {
		t.Fatalf("second seed: %v", err)
	}
	got, _ := GetTaskRule(db, "system-rule-ci-check-failed")
	if got.Enabled {
		t.Error("re-seed overwrote user's disable — SeedTaskRules should use INSERT OR IGNORE")
	}
	all, _ := ListTaskRules(db)
	if len(all) != 6 {
		t.Errorf("expected 6 rules after re-seed, got %d", len(all))
	}
}
