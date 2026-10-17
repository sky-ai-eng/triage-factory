package dbtest

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
)

// OperatorStoreFactory hands the conformance suite a wired store, plus the
// login-identity seeder IsOperatorUserSystem resolves through (nil on a backend
// without login identities). operators itself has no FK chain (email is the
// PK), so nothing else needs seeding.
type OperatorStoreFactory func(t *testing.T) (db.OperatorStore, *LoginIdentitySeeder)

// RunOperatorStoreConformance covers the contract every backend must hold: Add
// is case-folding and idempotent; Remove reports matched then false; List is
// email-ordered and normalized; IsOperatorUserSystem matches a principal only
// through a verified identity holding an operator address, case-insensitively,
// and a backend without login identities answers false for everyone.
func RunOperatorStoreConformance(t *testing.T, mk OperatorStoreFactory) {
	t.Helper()
	ctx := context.Background()

	emails := func(t *testing.T, store db.OperatorStore) []string {
		t.Helper()
		list, err := store.List(ctx)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		out := make([]string, len(list))
		for i, op := range list {
			out[i] = op.Email
		}
		return out
	}

	t.Run("add_normalizes_email", func(t *testing.T) {
		store, _ := mk(t)
		if err := store.Add(ctx, "  Ada@Example.com ", "root"); err != nil {
			t.Fatalf("add: %v", err)
		}
		if got := emails(t, store); len(got) != 1 || got[0] != "ada@example.com" {
			t.Fatalf("operators = %v, want [ada@example.com]", got)
		}
	})

	t.Run("add_is_idempotent", func(t *testing.T) {
		store, _ := mk(t)
		if err := store.Add(ctx, "dup@example.com", "a"); err != nil {
			t.Fatalf("add 1: %v", err)
		}
		if err := store.Add(ctx, "DUP@example.com", "b"); err != nil {
			t.Fatalf("re-add must not error: %v", err)
		}
		if got := emails(t, store); len(got) != 1 {
			t.Fatalf("idempotent add should leave one row, got %v", got)
		}
	})

	t.Run("remove_reports_matched_then_false", func(t *testing.T) {
		store, _ := mk(t)
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
		if got := emails(t, store); len(got) != 0 {
			t.Fatalf("removed operator still listed: %v", got)
		}
	})

	t.Run("list_is_ordered_and_normalized", func(t *testing.T) {
		store, _ := mk(t)
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

	t.Run("is_operator_user_system", func(t *testing.T) {
		store, seed := mk(t)
		if err := store.Add(ctx, "ops@example.com", "root"); err != nil {
			t.Fatalf("add: %v", err)
		}
		isOp := func(t *testing.T, userID string) bool {
			t.Helper()
			ok, err := store.IsOperatorUserSystem(ctx, userID)
			if err != nil {
				t.Fatalf("IsOperatorUserSystem(%s): %v", userID, err)
			}
			return ok
		}

		if seed == nil {
			// No login identities on this backend: nobody resolves to an
			// operator through one, whatever the operators table holds.
			if isOp(t, uuid.NewString()) {
				t.Fatal("a backend without login identities must answer false")
			}
			return
		}

		verified := seed.Principal(t)
		seed.Identity(t, verified, "Ops@Example.COM", true)
		if !isOp(t, verified) {
			t.Error("a principal holding the operator address verified (in another case) must be an operator")
		}

		unverified := seed.Principal(t)
		seed.Identity(t, unverified, "ops@example.com", false)
		if isOp(t, unverified) {
			t.Error("an unverified identity holding the operator address must not make its principal an operator")
		}

		bystander := seed.Principal(t)
		seed.Identity(t, bystander, "someone@example.com", true)
		if isOp(t, bystander) {
			t.Error("a principal with no operator address must not be an operator")
		}

		if isOp(t, uuid.NewString()) {
			t.Error("an unknown principal must not be an operator")
		}

		if _, err := store.Remove(ctx, "ops@example.com"); err != nil {
			t.Fatalf("remove: %v", err)
		}
		if isOp(t, verified) {
			t.Error("removing the operator address must revoke the match")
		}
	})
}
