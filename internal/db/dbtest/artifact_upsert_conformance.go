package dbtest

import (
	"context"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// RunArtifactUpsertConformance covers the rules ArtifactStore's upsert applies
// to every artifact kind on a conflict. Today that is details_json, which is
// preserve-on-empty: a write that passes no details keeps the ones the row
// holds, and a write that passes details replaces them. The rule is
// store-wide, so it is checked for each kind a writer gives details to, built
// by the constructors those writers use, through both Upsert and UpsertSystem.
// The factory is ExternalObjectScopeFactory's: a fresh, empty org and a team
// per call.
func RunArtifactUpsertConformance(t *testing.T, mk ExternalObjectScopeFactory) {
	t.Helper()
	ctx := context.Background()

	branch, ok := domain.NewBranchArtifact("octo/api", "refs/heads/feat", "abc123", true)
	if !ok {
		t.Fatal("NewBranchArtifact refused the fixture branch")
	}
	// The host sink keys a branch on its repository's registry row.
	branch.DedupKey = domain.BranchDedupKey("repo-octo-api", branch.ExternalID)
	kinds := []struct {
		name string
		a    domain.Artifact
		// replaced is a different, non-empty details payload for the same row.
		replaced string
	}{
		{
			name:     "pull_request",
			a:        domain.NewPullRequestArtifact("octo/api", 7, "PR_kw7", "feat", "main", "https://github.com/octo/api/pull/7", "Title", "Body", true),
			replaced: `{"node_id":"PR_kw7","head_branch":"feat","base":"main"}`,
		},
		{
			name:     "review",
			a:        domain.NewReviewArtifact("octo/api", 7, "PR_kw7", "abc123", ""),
			replaced: `{"head_sha":"def456"}`,
		},
		{
			name:     "branch",
			a:        branch,
			replaced: `{"sha":"def456","new":false}`,
		},
		{
			name: "jira issue",
			a: domain.Artifact{
				Provider: domain.ArtifactProviderJira, Kind: domain.ArtifactKindIssue,
				Target: "ENG-1", ExternalID: "10042", State: domain.ArtifactStateIssueCreated,
				DedupKey:    domain.ArtifactDedupKey(domain.ArtifactProviderJira, domain.ArtifactKindIssue, "10042", ""),
				DetailsJSON: `{"summary":"First"}`,
			},
			replaced: `{"summary":"Second"}`,
		},
		{
			name: "github reply",
			a: domain.Artifact{
				Provider: domain.ArtifactProviderGitHub, Kind: domain.ArtifactKindComment,
				Target: "octo/api#7", ExternalID: "11", State: domain.ArtifactStateCommentPosted,
				DedupKey:    domain.GitHubCommentDedupKey(domain.GitHubReviewComment, 11),
				DetailsJSON: `{"in_reply_to":10}`,
			},
			replaced: `{"in_reply_to":12}`,
		},
	}

	for _, door := range []struct {
		name   string
		upsert func(s db.ArtifactStore, orgID string, a domain.Artifact) (domain.Artifact, error)
	}{
		{"Upsert", func(s db.ArtifactStore, orgID string, a domain.Artifact) (domain.Artifact, error) {
			return s.Upsert(ctx, orgID, a)
		}},
		{"UpsertSystem", func(s db.ArtifactStore, orgID string, a domain.Artifact) (domain.Artifact, error) {
			return s.UpsertSystem(ctx, orgID, a)
		}},
	} {
		for _, k := range kinds {
			t.Run("Details_are_preserved_on_empty/"+door.name+"/"+k.name, func(t *testing.T) {
				s, orgID, teamID := mk(t)
				in := k.a
				in.TeamID, in.Scope = teamID, TestGitHubHost
				if in.DetailsJSON == "" {
					t.Fatalf("fixture %s carries no details; the case needs some to preserve", k.name)
				}
				first, err := door.upsert(s.Artifacts, orgID, in)
				if err != nil {
					t.Fatalf("%s (insert): %v", door.name, err)
				}

				// A later write that knows nothing of the details: they stay.
				bare := in
				bare.DetailsJSON = ""
				kept, err := door.upsert(s.Artifacts, orgID, bare)
				if err != nil {
					t.Fatalf("%s (no details): %v", door.name, err)
				}
				if kept.ID != first.ID || kept.DetailsJSON != first.DetailsJSON {
					t.Errorf("row after a write with no details = %q on %s, want %q kept on %s",
						kept.DetailsJSON, kept.ID, first.DetailsJSON, first.ID)
				}
				AssertWriteReturnedStoredRow(t, door.name+" (no details)", kept, func() (*domain.Artifact, error) {
					return s.Artifacts.Get(ctx, orgID, first.ID)
				})

				// A write that carries details replaces them.
				next := in
				next.DetailsJSON = k.replaced
				replaced, err := door.upsert(s.Artifacts, orgID, next)
				if err != nil {
					t.Fatalf("%s (new details): %v", door.name, err)
				}
				if replaced.ID != first.ID || replaced.DetailsJSON != k.replaced {
					t.Errorf("row after a write with details = %q, want %q", replaced.DetailsJSON, k.replaced)
				}
				AssertWriteReturnedStoredRow(t, door.name+" (new details)", replaced, func() (*domain.Artifact, error) {
					return s.Artifacts.Get(ctx, orgID, first.ID)
				})
			})
		}
	}
}
