package dbtest

import (
	"context"
	"errors"
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
