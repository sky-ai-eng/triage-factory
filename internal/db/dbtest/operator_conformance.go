package dbtest

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// OperatorStoreFactory hands the conformance suite a wired store, plus the
// login-identity seeder Add and IsOperatorUserSystem resolve an address
// through (nil on a backend without login identities).
type OperatorStoreFactory func(t *testing.T) (db.OperatorStore, *LoginIdentitySeeder)

// RunOperatorStoreConformance covers the contract every backend must hold: Add
// is case-folding, returns the stored row, and on a re-add keeps the row's
// provenance; Remove reports matched then false; List is email-ordered and
// normalized. Where login identities exist: Add binds a grant to the principal
// holding the address verified, a re-add binds a pending grant and never
// re-binds a bound one, a bound grant admits its principal wherever the
// address goes and nobody else, and a pending grant matches a principal
// through a verified identity holding the address, case-insensitively. A
// backend without login identities binds nothing and answers false for a
// principal no grant is bound to.
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
	// stored is the point read the returned-row assertion compares against:
	// the store has no read by email, so it is List filtered to one row.
	stored := func(store db.OperatorStore, email string) func() (*domain.Operator, error) {
		return func() (*domain.Operator, error) {
			list, err := store.List(ctx)
			if err != nil {
				return nil, err
			}
			for _, op := range list {
				if op.Email == email {
					return &op, nil
				}
			}
			return nil, nil
		}
	}
	add := func(t *testing.T, store db.OperatorStore, email string) domain.Operator {
		t.Helper()
		op, err := store.Add(ctx, email, "root")
		if err != nil {
			t.Fatalf("add %s: %v", email, err)
		}
		return op
	}
	isOp := func(t *testing.T, store db.OperatorStore, userID string) bool {
		t.Helper()
		ok, err := store.IsOperatorUserSystem(ctx, userID)
		if err != nil {
			t.Fatalf("IsOperatorUserSystem(%s): %v", userID, err)
		}
		return ok
	}

	t.Run("add_normalizes_email", func(t *testing.T) {
		store, _ := mk(t)
		if _, err := store.Add(ctx, "  Ada@Example.com ", "root"); err != nil {
			t.Fatalf("add: %v", err)
		}
		if got := emails(t, store); len(got) != 1 || got[0] != "ada@example.com" {
			t.Fatalf("operators = %v, want [ada@example.com]", got)
		}
	})

	t.Run("add_returns_the_stored_row", func(t *testing.T) {
		store, _ := mk(t)
		op, err := store.Add(ctx, "Row@Example.com", "root")
		if err != nil {
			t.Fatalf("add: %v", err)
		}
		if op.Email != "row@example.com" || op.AddedBy != "root" || op.AddedAt.IsZero() {
			t.Fatalf("returned row = %+v, want the normalized email, its provenance and a stamp", op)
		}
		AssertWriteReturnedStoredRow(t, "Add", op, stored(store, "row@example.com"))
	})

	t.Run("re_add_keeps_provenance", func(t *testing.T) {
		store, _ := mk(t)
		first, err := store.Add(ctx, "dup@example.com", "a")
		if err != nil {
			t.Fatalf("add 1: %v", err)
		}
		again, err := store.Add(ctx, "DUP@example.com", "b")
		if err != nil {
			t.Fatalf("re-add must not error: %v", err)
		}
		if got := emails(t, store); len(got) != 1 {
			t.Fatalf("a re-add should leave one row, got %v", got)
		}
		if again.AddedBy != "a" || !again.AddedAt.Equal(first.AddedAt) {
			t.Errorf("re-add returned %+v, want the first add's provenance (%+v)", again, first)
		}
		AssertWriteReturnedStoredRow(t, "Add (re-add)", again, stored(store, "dup@example.com"))
	})

	t.Run("remove_reports_matched_then_false", func(t *testing.T) {
		store, _ := mk(t)
		add(t, store, "gone@example.com")
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
		add(t, store, "Zoe@Example.com")
		add(t, store, "amy@example.com")
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

	t.Run("without_login_identities_nothing_binds", func(t *testing.T) {
		store, seed := mk(t)
		if seed != nil {
			t.Skip("this backend has login identities; the subtests below cover it")
		}
		if op := add(t, store, "ops@example.com"); !op.Pending() {
			t.Errorf("Add bound %+v on a backend with no login identities to resolve the address", op)
		}
		if isOp(t, store, uuid.NewString()) {
			t.Error("a principal no grant is bound to must not be an operator")
		}
	})

	withIdentities := func(t *testing.T) (db.OperatorStore, *LoginIdentitySeeder) {
		t.Helper()
		store, seed := mk(t)
		if seed == nil {
			t.Skip("no login identities on this backend")
		}
		return store, seed
	}

	t.Run("add_binds_the_verified_holder", func(t *testing.T) {
		store, seed := withIdentities(t)
		holder := seed.Principal(t)
		seed.Identity(t, holder, "Ops@Example.COM", true)
		other := seed.Principal(t)
		seed.Identity(t, other, "ops@example.com", false)

		op := add(t, store, "ops@example.com")
		if op.UserID != holder {
			t.Fatalf("Add bound %q, want the principal holding the address verified (%s)", op.UserID, holder)
		}
		AssertWriteReturnedStoredRow(t, "Add (bound)", op, stored(store, "ops@example.com"))
	})

	t.Run("re_add_binds_a_pending_grant_and_never_rebinds", func(t *testing.T) {
		store, seed := withIdentities(t)
		unverified := seed.Principal(t)
		seed.Identity(t, unverified, "ops@example.com", false)
		if op := add(t, store, "ops@example.com"); !op.Pending() {
			t.Fatalf("first add bound %+v with no identity holding the address verified", op)
		}
		first := seed.Principal(t)
		seed.Identity(t, first, "ops@example.com", true)
		if op := add(t, store, "ops@example.com"); op.UserID != first {
			t.Fatalf("re-add left %+v, want the pending grant bound to %s", op, first)
		}

		// The address moves to another principal; re-adding it must not move
		// the grant with it.
		seed.Readdress(t, first, "ops@example.com", "first@example.com")
		second := seed.Principal(t)
		seed.Identity(t, second, "ops@example.com", true)
		if op := add(t, store, "ops@example.com"); op.UserID != first {
			t.Fatalf("re-add moved the grant to %q, want it kept on %s", op.UserID, first)
		}
	})

	t.Run("bound_grant_follows_the_principal_not_the_address", func(t *testing.T) {
		store, seed := withIdentities(t)
		operator := seed.Principal(t)
		seed.Identity(t, operator, "ops@example.com", true)
		if op := add(t, store, "ops@example.com"); op.UserID != operator {
			t.Fatalf("Add bound %q, want %s", op.UserID, operator)
		}

		// The operator's identity provider now asserts another address, and a
		// new person verifies the operator address.
		seed.Readdress(t, operator, "ops@example.com", "former-ops@example.com")
		successor := seed.Principal(t)
		seed.Identity(t, successor, "ops@example.com", true)

		if !isOp(t, store, operator) {
			t.Error("the principal the grant is bound to must stay an operator after its address changes")
		}
		if isOp(t, store, successor) {
			t.Error("a principal that verified the address after the grant was bound must not be an operator")
		}

		if _, err := store.Remove(ctx, "ops@example.com"); err != nil {
			t.Fatalf("remove: %v", err)
		}
		if isOp(t, store, operator) {
			t.Error("removing the grant must revoke its principal")
		}
	})

	t.Run("pending_grant_matches_by_verified_address", func(t *testing.T) {
		store, seed := withIdentities(t)
		if op := add(t, store, "ops@example.com"); !op.Pending() {
			t.Fatalf("Add bound %+v with no identity holding the address", op)
		}

		verified := seed.Principal(t)
		seed.Identity(t, verified, "Ops@Example.COM", true)
		if !isOp(t, store, verified) {
			t.Error("a principal holding a pending grant's address verified (in another case) must be an operator")
		}

		unverified := seed.Principal(t)
		seed.Identity(t, unverified, "ops@example.com", false)
		if isOp(t, store, unverified) {
			t.Error("an unverified identity holding the operator address must not make its principal an operator")
		}

		bystander := seed.Principal(t)
		seed.Identity(t, bystander, "someone@example.com", true)
		if isOp(t, store, bystander) {
			t.Error("a principal with no operator address must not be an operator")
		}

		if isOp(t, store, uuid.NewString()) {
			t.Error("an unknown principal must not be an operator")
		}

		if _, err := store.Remove(ctx, "ops@example.com"); err != nil {
			t.Fatalf("remove: %v", err)
		}
		if isOp(t, store, verified) {
			t.Error("removing the operator address must revoke the match")
		}
	})
}
