package dbtest

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// RunExternalActionReferenceConformance covers the ledger's references to
// what an act was about: entity_id and repository_id are written as the
// writer resolved them through both write pools and read back by every list,
// an act about nothing leaves both empty, and a reference to a row the org
// does not hold is refused rather than stored. It runs on the node-id suite's
// fixture: a fresh org with a team and a conversation.
func RunExternalActionReferenceConformance(t *testing.T, mk ExternalActionNodeIDFactory) {
	t.Helper()
	ctx := context.Background()
	action := func(f ExternalActionNodeIDFixture, action, target string) domain.ExternalAction {
		return domain.ExternalAction{
			ID:             uuid.NewString(),
			TeamID:         f.TeamID,
			Provider:       domain.ProviderGitHub,
			Scope:          TestGitHubHost,
			Action:         action,
			Target:         target,
			ConversationID: f.ConversationID,
			Credential:     domain.CredentialGitHubApp,
		}
	}

	t.Run("References_round_trip_through_both_pools_and_every_list", func(t *testing.T) {
		f := mk(t)
		entity, _, err := f.Stores.Entities.FindOrCreateSystem(ctx, f.OrgID, "github", TestGitHubHost, "octo/api#7", "PR_ref7", "pr", "A PR", "")
		if err != nil {
			t.Fatalf("seed entity: %v", err)
		}
		repo, err := f.Stores.Repos.UpsertSystem(ctx, f.OrgID, domain.Repository{Host: TestGitHubHost, Owner: "octo", Repo: "web"})
		if err != nil {
			t.Fatalf("seed repository: %v", err)
		}

		aboutEntity := action(f, domain.ActionCommentPosted, "octo/api#7")
		aboutEntity.EntityID = entity.ID
		aboutRepo := action(f, domain.ActionBranchPushed, "octo/web")
		aboutRepo.RepositoryID = repo.ID
		aboutNothing := action(f, domain.ActionGraphQLWrite, "/graphql")
		if err := f.Stores.ExternalActions.Record(ctx, f.OrgID, aboutEntity); err != nil {
			t.Fatalf("Record: %v", err)
		}
		if err := f.Stores.ExternalActions.RecordSystem(ctx, f.OrgID, aboutRepo); err != nil {
			t.Fatalf("RecordSystem: %v", err)
		}
		if err := f.Stores.ExternalActions.RecordSystem(ctx, f.OrgID, aboutNothing); err != nil {
			t.Fatalf("RecordSystem about nothing: %v", err)
		}
		want := map[string][2]string{
			aboutEntity.ID:  {entity.ID, ""},
			aboutRepo.ID:    {"", repo.ID},
			aboutNothing.ID: {"", ""},
		}

		ea, opts := f.Stores.ExternalActions, domain.ExternalActionListOpts{}
		byOrg, _, errOrg := ea.ListByOrgSystem(ctx, f.OrgID, opts)
		byTeam, _, errTeam := ea.ListByTeam(ctx, f.OrgID, f.TeamID, opts)
		byConversation, _, errConversation := ea.ListByConversation(ctx, f.OrgID, f.ConversationID, opts)
		for _, read := range []struct {
			name string
			got  []domain.ExternalAction
			err  error
		}{{"ListByOrgSystem", byOrg, errOrg}, {"ListByTeam", byTeam, errTeam}, {"ListByConversation", byConversation, errConversation}} {
			if read.err != nil || len(read.got) != len(want) {
				t.Fatalf("%s = %d rows, %v; want %d", read.name, len(read.got), read.err, len(want))
			}
			for _, a := range read.got {
				if refs, ok := want[a.ID]; !ok || a.EntityID != refs[0] || a.RepositoryID != refs[1] {
					t.Errorf("%s row %s = (entity %q, repository %q), want %q", read.name, a.ID, a.EntityID, a.RepositoryID, refs)
				}
			}
		}
	})

	t.Run("A_reference_to_a_row_the_org_does_not_hold_is_refused", func(t *testing.T) {
		f := mk(t)
		ghostEntity := action(f, domain.ActionCommentPosted, "octo/api#8")
		ghostEntity.EntityID = uuid.NewString()
		ghostRepo := action(f, domain.ActionBranchPushed, "octo/gone")
		ghostRepo.RepositoryID = uuid.NewString()
		for _, a := range []domain.ExternalAction{ghostEntity, ghostRepo} {
			if err := f.Stores.ExternalActions.RecordSystem(ctx, f.OrgID, a); err == nil {
				t.Errorf("a row about (entity %q, repository %q), neither of which exists, was recorded", a.EntityID, a.RepositoryID)
			}
		}
		if rows, _, err := f.Stores.ExternalActions.ListByOrgSystem(ctx, f.OrgID, domain.ExternalActionListOpts{}); err != nil || len(rows) != 0 {
			t.Errorf("refused writes left rows: %+v, %v", rows, err)
		}
	})
}
