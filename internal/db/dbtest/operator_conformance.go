package dbtest

import (
	"context"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
)

// OperatorStoreFactory hands the conformance suite a wired store. operators has
// no FK chain (email is the PK), so there is nothing to seed.
type OperatorStoreFactory func(t *testing.T) db.OperatorStore

// RunOperatorStoreConformance covers the contract every backend must hold:
// absence reads false; Add then IsOperator is true and case-insensitive; Add is
// idempotent; Remove reports matched then false; List is email-ordered and
// normalized.
func RunOperatorStoreConformance(t *testing.T, mk OperatorStoreFactory) {
	t.Helper()
	ctx := context.Background()

	t.Run("absent_is_operator_false", func(t *testing.T) {
		store := mk(t)
		ok, err := store.IsOperator(ctx, "nobody@example.com")
		if err != nil {
			t.Fatalf("is-operator: %v", err)
		}
		if ok {
			t.Fatalf("absent email must not be an operator")
		}
	})

	t.Run("add_then_is_operator_case_insensitive", func(t *testing.T) {
		store := mk(t)
		if err := store.Add(ctx, "Ada@Example.com", "root"); err != nil {
			t.Fatalf("add: %v", err)
		}
		// Query with different casing + surrounding whitespace.
		ok, err := store.IsOperator(ctx, "  ADA@example.COM ")
		if err != nil || !ok {
			t.Fatalf("case-insensitive lookup failed: ok=%v err=%v", ok, err)
		}
	})

	t.Run("add_is_idempotent", func(t *testing.T) {
		store := mk(t)
		if err := store.Add(ctx, "dup@example.com", "a"); err != nil {
			t.Fatalf("add 1: %v", err)
		}
		if err := store.Add(ctx, "DUP@example.com", "b"); err != nil {
			t.Fatalf("re-add must not error: %v", err)
		}
		list, err := store.List(ctx)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(list) != 1 {
			t.Fatalf("idempotent add should leave one row, got %d: %+v", len(list), list)
		}
	})

	t.Run("remove_reports_matched_then_false", func(t *testing.T) {
		store := mk(t)
		_ = store.Add(ctx, "gone@example.com", "")
		removed, err := store.Remove(ctx, "GONE@example.com")
		if err != nil || !removed {
			t.Fatalf("first remove: removed=%v err=%v", removed, err)
		}
		removed, err = store.Remove(ctx, "gone@example.com")
		if err != nil {
			t.Fatalf("second remove err: %v", err)
		}
		if removed {
			t.Fatalf("second remove should report matched=false")
		}
		ok, _ := store.IsOperator(ctx, "gone@example.com")
		if ok {
			t.Fatalf("removed operator must not be an operator")
		}
	})

	t.Run("list_is_ordered_and_normalized", func(t *testing.T) {
		store := mk(t)
		_ = store.Add(ctx, "Zoe@Example.com", "")
		_ = store.Add(ctx, "amy@example.com", "root")
		list, err := store.List(ctx)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(list) != 2 {
			t.Fatalf("expected 2, got %d", len(list))
		}
		if list[0].Email != "amy@example.com" || list[1].Email != "zoe@example.com" {
			t.Fatalf("emails should be lower-cased and ordered: %+v", list)
		}
		if list[1].AddedAt.IsZero() {
			t.Fatalf("added_at not stamped")
		}
	})
}
