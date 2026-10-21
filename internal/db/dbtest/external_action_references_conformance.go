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
			Provider:       domain.ArtifactProviderGitHub,
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

		check := func(read string, got []domain.ExternalAction) {
			t.Helper()
			if len(got) != len(want) {
				t.Fatalf("%s returned %d rows, want %d", read, len(got), len(want))
			}
			for _, a := range got {
				if refs, ok := want[a.ID]; !ok || a.EntityID != refs[0] || a.RepositoryID != refs[1] {
					t.Errorf("%s row %s = (entity %q, repository %q), want %q", read, a.ID, a.EntityID, a.RepositoryID, refs)
				}
			}
		}
		byOrg, _, err := f.Stores.ExternalActions.ListByOrgSystem(ctx, f.OrgID, domain.ExternalActionListOpts{})
		if err != nil {
			t.Fatalf("ListByOrgSystem: %v", err)
		}
		check("ListByOrgSystem", byOrg)
		byTeam, _, err := f.Stores.ExternalActions.ListByTeam(ctx, f.OrgID, f.TeamID, domain.ExternalActionListOpts{})
		if err != nil {
			t.Fatalf("ListByTeam: %v", err)
		}
		check("ListByTeam", byTeam)
		byConversation, _, err := f.Stores.ExternalActions.ListByConversation(ctx, f.OrgID, f.ConversationID, domain.ExternalActionListOpts{})
		if err != nil {
			t.Fatalf("ListByConversation: %v", err)
		}
		check("ListByConversation", byConversation)
	})

	t.Run("A_reference_to_a_row_the_org_does_not_hold_is_refused", func(t *testing.T) {
		f := mk(t)
		dangling := uuid.NewString()
		ghostEntity := action(f, domain.ActionCommentPosted, "octo/api#8")
		ghostEntity.EntityID = dangling
		if err := f.Stores.ExternalActions.RecordSystem(ctx, f.OrgID, ghostEntity); err == nil {
			t.Errorf("a row about entity %s, which does not exist, was recorded", dangling)
		}
		ghostRepo := action(f, domain.ActionBranchPushed, "octo/gone")
		ghostRepo.RepositoryID = dangling
		if err := f.Stores.ExternalActions.RecordSystem(ctx, f.OrgID, ghostRepo); err == nil {
			t.Errorf("a row about repository %s, which does not exist, was recorded", dangling)
		}
		rows, _, err := f.Stores.ExternalActions.ListByOrgSystem(ctx, f.OrgID, domain.ExternalActionListOpts{})
		if err != nil {
			t.Fatalf("ListByOrgSystem: %v", err)
		}
		if len(rows) != 0 {
			t.Errorf("refused writes left rows: %+v", rows)
		}
	})
}
