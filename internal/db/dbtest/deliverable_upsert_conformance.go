package dbtest

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// RunDeliverableUpsertConformance covers the rules DeliverableStore's upsert applies
// to every deliverable kind on a conflict. details_json is preserve-on-empty: a
// write that passes no details keeps the ones the row holds, and a write that
// passes details replaces them. The rule is store-wide, so it is checked for
// each kind, built by the constructors its writers use, through both Upsert
// and UpsertSystem. entity_id is preserve-on-empty the same way. A kind other
// than a pull request or a review is refused through every write door. The
// factory is ExternalObjectScopeFactory's: a fresh, empty org and a team per
// call.
func RunDeliverableUpsertConformance(t *testing.T, mk ExternalObjectScopeFactory) {
	t.Helper()
	ctx := context.Background()

	kinds := []struct {
		name string
		a    domain.Deliverable
		// replaced is a different, non-empty details payload for the same row.
		replaced string
	}{
		{
			name:     "pull_request",
			a:        domain.NewPullRequestDeliverable("octo/api", 7, "PR_kw7", "feat", "main", "https://github.com/octo/api/pull/7", "Title", "Body", true),
			replaced: `{"node_id":"PR_kw7","head_branch":"feat","base":"main"}`,
		},
		{
			name:     "review",
			a:        domain.NewReviewDeliverable("octo/api", 7, "PR_kw7", "abc123", ""),
			replaced: `{"head_sha":"def456"}`,
		},
	}

	t.Run("Other_kinds_are_refused", func(t *testing.T) {
		s, orgID, teamID := mk(t)
		for _, kind := range []string{"branch", "issue", "comment", "message", ""} {
			a := domain.Deliverable{
				TeamID: teamID, Provider: domain.ProviderGitHub, Kind: kind, Scope: TestGitHubHost,
				Target: "octo/api", ExternalID: "refs/heads/feat", State: "pushed",
				DedupKey: "git:branch:repo-octo-api:refs/heads/feat",
			}
			if _, err := s.Deliverables.Upsert(ctx, orgID, a); !errors.Is(err, db.ErrDeliverableKind) {
				t.Errorf("Upsert(kind %q): err = %v, want ErrDeliverableKind", kind, err)
			}
			if _, err := s.Deliverables.UpsertSystem(ctx, orgID, a); !errors.Is(err, db.ErrDeliverableKind) {
				t.Errorf("UpsertSystem(kind %q): err = %v, want ErrDeliverableKind", kind, err)
			}
			if _, err := s.Deliverables.InsertDeliverableIfAbsentSystem(ctx, orgID, a); !errors.Is(err, db.ErrDeliverableKind) {
				t.Errorf("InsertDeliverableIfAbsentSystem(kind %q): err = %v, want ErrDeliverableKind", kind, err)
			}
			if _, err := s.Deliverables.InsertDeliverableIfAbsent(ctx, orgID, a); !errors.Is(err, db.ErrDeliverableKind) {
				t.Errorf("InsertDeliverableIfAbsent(kind %q): err = %v, want ErrDeliverableKind", kind, err)
			}
		}
	})

	// Insert-if-absent inserts a row once and, on a conflict, changes nothing:
	// a second write of the same key in another state leaves the first row's
	// state as it was. A row with no scope is refused and leaves nothing.
	for _, door := range []struct {
		name   string
		insert func(s db.DeliverableStore, orgID string, a domain.Deliverable) (bool, error)
	}{
		{"InsertDeliverableIfAbsent", func(s db.DeliverableStore, orgID string, a domain.Deliverable) (bool, error) {
			return s.InsertDeliverableIfAbsent(ctx, orgID, a)
		}},
		{"InsertDeliverableIfAbsentSystem", func(s db.DeliverableStore, orgID string, a domain.Deliverable) (bool, error) {
			return s.InsertDeliverableIfAbsentSystem(ctx, orgID, a)
		}},
	} {
		t.Run("Insert_if_absent/"+door.name, func(t *testing.T) {
			s, orgID, teamID := mk(t)
			pr := func(draft bool) domain.Deliverable {
				a := domain.NewPullRequestDeliverable("octo/api", 7, "PR_kw7", "feat", "main", "https://github.com/octo/api/pull/7", "Title", "Body", draft)
				a.TeamID = teamID
				a.Scope = TestGitHubHost
				return a
			}
			unscoped := pr(true)
			unscoped.Scope = ""
			if _, err := door.insert(s.Deliverables, orgID, unscoped); !errors.Is(err, db.ErrExternalObjectScopeRequired) {
				t.Errorf("%s with no scope: err = %v, want ErrExternalObjectScopeRequired", door.name, err)
			}
			if inserted, err := door.insert(s.Deliverables, orgID, pr(true)); err != nil || !inserted {
				t.Fatalf("%s (first) = %v, %v; want an insert", door.name, inserted, err)
			}
			if inserted, err := door.insert(s.Deliverables, orgID, pr(false)); err != nil || inserted {
				t.Errorf("%s (conflict) = %v, %v; want no insert", door.name, inserted, err)
			}
			rows, err := s.Deliverables.ListNonTerminalBySystem(ctx, orgID)
			if err != nil {
				t.Fatalf("ListNonTerminalBySystem: %v", err)
			}
			if len(rows) != 1 || rows[0].State != domain.DeliverableStatePRDraft {
				t.Errorf("rows after a conflicting insert = %+v, want the one row, still draft", rows)
			}
		})
	}

	// A row is inserted verified unless its writer says pending, an upsert
	// never moves it, and a settle moves a pending row once: to verified with
	// its details replaced, or to foreign and closed.
	t.Run("Verification_is_settled_once", func(t *testing.T) {
		s, orgID, teamID := mk(t)
		pr := func(number int, verification string) domain.Deliverable {
			a := domain.NewPullRequestDeliverable("octo/api", number, fmt.Sprintf("PR_v%d", number), "feat", "main", "", "Title", "Body", true)
			a.TeamID, a.Scope, a.Verification = teamID, TestGitHubHost, verification
			return a
		}
		get := func(id string) domain.Deliverable {
			t.Helper()
			got, err := s.Deliverables.Get(ctx, orgID, id)
			if err != nil || got == nil {
				t.Fatalf("Get %s = (%+v, %v)", id, got, err)
			}
			return *got
		}
		byKey := func(dedupKey string) domain.Deliverable {
			t.Helper()
			rows, err := s.Deliverables.ListNonTerminalBySystem(ctx, orgID)
			if err != nil {
				t.Fatalf("ListNonTerminalBySystem: %v", err)
			}
			for _, r := range rows {
				if r.DedupKey == dedupKey {
					return r
				}
			}
			t.Fatalf("no row keyed %s", dedupKey)
			return domain.Deliverable{}
		}

		plain, err := s.Deliverables.UpsertSystem(ctx, orgID, pr(1, ""))
		if err != nil || plain.Verification != domain.DeliverableVerificationVerified {
			t.Fatalf("UpsertSystem with no verification = (%q, %v), want verified", plain.Verification, err)
		}
		if _, err := s.Deliverables.InsertDeliverableIfAbsentSystem(ctx, orgID, pr(9, "maybe")); !errors.Is(err, db.ErrDeliverableVerification) {
			t.Errorf("insert with verification %q: err = %v, want ErrDeliverableVerification", "maybe", err)
		}

		mine, theirs := pr(2, domain.DeliverableVerificationPending), pr(3, domain.DeliverableVerificationPending)
		for _, a := range []domain.Deliverable{mine, theirs} {
			if inserted, err := s.Deliverables.InsertDeliverableIfAbsentSystem(ctx, orgID, a); err != nil || !inserted {
				t.Fatalf("insert pending %s = (%v, %v)", a.DedupKey, inserted, err)
			}
		}
		mineRow, theirsRow := byKey(mine.DedupKey), byKey(theirs.DedupKey)
		if mineRow.Verification != domain.DeliverableVerificationPending {
			t.Fatalf("inserted verification = %q, want pending", mineRow.Verification)
		}
		again := mine
		again.Verification = domain.DeliverableVerificationVerified
		if up, err := s.Deliverables.UpsertSystem(ctx, orgID, again); err != nil || up.Verification != domain.DeliverableVerificationPending {
			t.Errorf("upsert over a pending row = (%q, %v), want it left pending", up.Verification, err)
		}

		if _, err := s.Deliverables.SettleVerificationSystem(ctx, orgID, mineRow.ID, domain.DeliverableVerificationPending, ""); !errors.Is(err, db.ErrDeliverableVerification) {
			t.Errorf("settle to pending: err = %v, want ErrDeliverableVerification", err)
		}
		const details = `{"node_id":"PR_v2","head_branch":"from-github","base":"main"}`
		if moved, err := s.Deliverables.SettleVerificationSystem(ctx, orgID, mineRow.ID, domain.DeliverableVerificationVerified, details); err != nil || !moved {
			t.Fatalf("settle verified = (%v, %v), want (true, nil)", moved, err)
		}
		if got := get(mineRow.ID); got.Verification != domain.DeliverableVerificationVerified || got.State != domain.DeliverableStatePRDraft || got.DetailsJSON != details {
			t.Errorf("verified row = (%q, %q, %s), want verified, still draft, details replaced", got.Verification, got.State, got.DetailsJSON)
		}
		if moved, err := s.Deliverables.SettleVerificationSystem(ctx, orgID, mineRow.ID, domain.DeliverableVerificationForeign, ""); err != nil || moved {
			t.Errorf("a second settle = (%v, %v), want (false, nil)", moved, err)
		}
		if moved, err := s.Deliverables.SettleVerificationSystem(ctx, orgID, plain.ID, domain.DeliverableVerificationForeign, ""); err != nil || moved {
			t.Errorf("settle of a row inserted verified = (%v, %v), want (false, nil)", moved, err)
		}

		if moved, err := s.Deliverables.SettleVerificationSystem(ctx, orgID, theirsRow.ID, domain.DeliverableVerificationForeign, ""); err != nil || !moved {
			t.Fatalf("settle foreign = (%v, %v), want (true, nil)", moved, err)
		}
		if got := get(theirsRow.ID); got.Verification != domain.DeliverableVerificationForeign || got.State != domain.DeliverableStatePRClosed || got.DetailsJSON != theirsRow.DetailsJSON {
			t.Errorf("foreign row = (%q, %q, %s), want foreign, closed, details kept", got.Verification, got.State, got.DetailsJSON)
		}
		if moved, err := s.Deliverables.SettleVerificationSystem(ctx, orgID, "not-a-deliverable-id", domain.DeliverableVerificationVerified, ""); err != nil || moved {
			t.Errorf("settle of an unknown id = (%v, %v), want (false, nil)", moved, err)
		}
	})

	for _, door := range []struct {
		name   string
		upsert func(s db.DeliverableStore, orgID string, a domain.Deliverable) (domain.Deliverable, error)
	}{
		{"Upsert", func(s db.DeliverableStore, orgID string, a domain.Deliverable) (domain.Deliverable, error) {
			return s.Upsert(ctx, orgID, a)
		}},
		{"UpsertSystem", func(s db.DeliverableStore, orgID string, a domain.Deliverable) (domain.Deliverable, error) {
			return s.UpsertSystem(ctx, orgID, a)
		}},
	} {
		t.Run("Entity_is_preserved_on_empty/"+door.name, func(t *testing.T) {
			s, orgID, teamID := mk(t)
			entity, _, err := s.Entities.FindOrCreateSystem(ctx, orgID, domain.ProviderGitHub, TestGitHubHost, "octo/api#7", "PR_kw7", "pr", "t", "")
			if err != nil {
				t.Fatalf("seed entity: %v", err)
			}
			in := domain.NewPullRequestDeliverable("octo/api", 7, "PR_kw7", "feat", "main", "", "Title", "Body", true)
			in.TeamID, in.Scope, in.EntityID = teamID, TestGitHubHost, entity.ID
			first, err := door.upsert(s.Deliverables, orgID, in)
			if err != nil || first.EntityID != entity.ID {
				t.Fatalf("%s (linked) = entity %q, %v; want %s", door.name, first.EntityID, err, entity.ID)
			}
			AssertWriteReturnedStoredRow(t, door.name+" (linked)", first, func() (*domain.Deliverable, error) {
				return s.Deliverables.Get(ctx, orgID, first.ID)
			})
			// A later writer that resolved no entity does not unlink the row.
			bare := in
			bare.EntityID = ""
			kept, err := door.upsert(s.Deliverables, orgID, bare)
			if err != nil || kept.ID != first.ID || kept.EntityID != entity.ID {
				t.Errorf("%s (unlinked) = entity %q on %s, %v; want %s kept on %s", door.name, kept.EntityID, kept.ID, err, entity.ID, first.ID)
			}
		})
		for _, k := range kinds {
			t.Run("Details_are_preserved_on_empty/"+door.name+"/"+k.name, func(t *testing.T) {
				s, orgID, teamID := mk(t)
				in := k.a
				in.TeamID, in.Scope = teamID, TestGitHubHost
				if in.DetailsJSON == "" {
					t.Fatalf("fixture %s carries no details; the case needs some to preserve", k.name)
				}
				first, err := door.upsert(s.Deliverables, orgID, in)
				if err != nil {
					t.Fatalf("%s (insert): %v", door.name, err)
				}

				// A later write that knows nothing of the details: they stay.
				bare := in
				bare.DetailsJSON = ""
				kept, err := door.upsert(s.Deliverables, orgID, bare)
				if err != nil {
					t.Fatalf("%s (no details): %v", door.name, err)
				}
				if kept.ID != first.ID || kept.DetailsJSON != first.DetailsJSON {
					t.Errorf("row after a write with no details = %q on %s, want %q kept on %s",
						kept.DetailsJSON, kept.ID, first.DetailsJSON, first.ID)
				}
				AssertWriteReturnedStoredRow(t, door.name+" (no details)", kept, func() (*domain.Deliverable, error) {
					return s.Deliverables.Get(ctx, orgID, first.ID)
				})

				// A write that carries details replaces them.
				next := in
				next.DetailsJSON = k.replaced
				replaced, err := door.upsert(s.Deliverables, orgID, next)
				if err != nil {
					t.Fatalf("%s (new details): %v", door.name, err)
				}
				if replaced.ID != first.ID || replaced.DetailsJSON != k.replaced {
					t.Errorf("row after a write with details = %q, want %q", replaced.DetailsJSON, k.replaced)
				}
				AssertWriteReturnedStoredRow(t, door.name+" (new details)", replaced, func() (*domain.Deliverable, error) {
					return s.Deliverables.Get(ctx, orgID, first.ID)
				})
			})
		}
	}
}
