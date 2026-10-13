package dbtest

import (
	"context"
	"errors"
	"maps"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// RunCommentArtifactConformance covers what a GitHub comment's artifact
// carries beyond the common columns: the comment's node id, which a later write
// that knows only the node id finds the row by, and whether TF posted the
// comment, which only ever turns on and is what tells TF's comments from
// people's. It also pins that a conversation comment and a review comment with
// the same number are two rows. The factory is ExternalObjectScopeFactory's: a
// fresh, empty org and a team per call.
func RunCommentArtifactConformance(t *testing.T, mk ExternalObjectScopeFactory) {
	t.Helper()
	ctx := context.Background()
	const (
		hostA = TestGitHubHost
		hostB = TestOtherGitHubHost
	)
	comment := func(teamID, scope string, space domain.GitHubCommentSpace, id int64) domain.Artifact {
		return domain.Artifact{
			TeamID: teamID, Provider: domain.ArtifactProviderGitHub, Kind: domain.ArtifactKindComment,
			Scope: scope, State: domain.ArtifactStateCommentPosted,
			DedupKey: domain.GitHubCommentDedupKey(space, id),
		}
	}

	t.Run("The_node_id_persists_and_finds_its_row_on_one_scope", func(t *testing.T) {
		s, orgID, teamID := mk(t)
		posted := comment(teamID, hostA, domain.GitHubIssueComment, 7)
		posted.Target, posted.ExternalID, posted.NodeID, posted.PostedByTF = "octo/api#3", "7", "IC_kwDOA7", true
		got, err := s.Artifacts.UpsertSystem(ctx, orgID, posted)
		if err != nil {
			t.Fatalf("UpsertSystem: %v", err)
		}
		if got.NodeID != "IC_kwDOA7" || !got.PostedByTF {
			t.Errorf("returned row = %+v, want node id IC_kwDOA7 and posted by TF", got)
		}
		AssertWriteReturnedStoredRow(t, "UpsertSystem", got, func() (*domain.Artifact, error) {
			return s.Artifacts.Get(ctx, orgID, got.ID)
		})

		// The same node id under another host is another object.
		other := comment(teamID, hostB, domain.GitHubIssueComment, 7)
		other.NodeID = "IC_kwDOA7"
		otherRow, err := s.Artifacts.UpsertSystem(ctx, orgID, other)
		if err != nil {
			t.Fatalf("UpsertSystem(%s): %v", hostB, err)
		}

		found, err := s.Artifacts.GetByNodeIDSystem(ctx, orgID, hostA, "IC_kwDOA7")
		if err != nil {
			t.Fatalf("GetByNodeIDSystem: %v", err)
		}
		if found == nil || found.ID != got.ID {
			t.Fatalf("GetByNodeIDSystem(%s) = %+v, want row %s", hostA, found, got.ID)
		}
		if found, _ := s.Artifacts.GetByNodeIDSystem(ctx, orgID, hostB, "IC_kwDOA7"); found == nil || found.ID != otherRow.ID {
			t.Errorf("GetByNodeIDSystem(%s) = %+v, want row %s", hostB, found, otherRow.ID)
		}
		for _, nodeID := range []string{"IC_kwDOA8", ""} {
			found, err := s.Artifacts.GetByNodeIDSystem(ctx, orgID, hostA, nodeID)
			if err != nil || found != nil {
				t.Errorf("GetByNodeIDSystem(%q) = %+v, %v; want no row", nodeID, found, err)
			}
		}
		if _, err := s.Artifacts.GetByNodeIDSystem(ctx, orgID, "", "IC_kwDOA7"); !errors.Is(err, db.ErrExternalObjectScopeRequired) {
			t.Errorf("GetByNodeIDSystem with no scope: err = %v, want ErrExternalObjectScopeRequired", err)
		}

		// A later write that does not know the node id keeps it.
		deleted := comment(teamID, hostA, domain.GitHubIssueComment, 7)
		deleted.State = domain.ArtifactStateCommentDeleted
		after, err := s.Artifacts.UpsertSystem(ctx, orgID, deleted)
		if err != nil {
			t.Fatalf("UpsertSystem (delete): %v", err)
		}
		if after.ID != got.ID || after.NodeID != "IC_kwDOA7" || after.State != domain.ArtifactStateCommentDeleted {
			t.Errorf("row after a write without a node id = %+v, want row %s deleted with its node id kept", after, got.ID)
		}

		// The insert-if-absent path writes both columns too.
		absent := comment(teamID, hostA, domain.GitHubReviewComment, 9)
		absent.NodeID, absent.PostedByTF = "PRRC_kwDOA9", true
		if inserted, err := s.Artifacts.InsertArtifactIfAbsentSystem(ctx, orgID, absent); err != nil || !inserted {
			t.Fatalf("InsertArtifactIfAbsentSystem = %v, %v; want an insert", inserted, err)
		}
		if found, _ := s.Artifacts.GetByNodeIDSystem(ctx, orgID, hostA, "PRRC_kwDOA9"); found == nil || !found.PostedByTF {
			t.Errorf("row inserted if absent = %+v, want its node id and posted_by_tf stored", found)
		}
	})

	t.Run("Posted_by_TF_only_turns_on", func(t *testing.T) {
		s, orgID, teamID := mk(t)
		posted := comment(teamID, hostA, domain.GitHubIssueComment, 7)
		posted.PostedByTF = true
		first, err := s.Artifacts.UpsertSystem(ctx, orgID, posted)
		if err != nil {
			t.Fatalf("UpsertSystem (post): %v", err)
		}
		// An edit does not know who posted the comment.
		edited, err := s.Artifacts.UpsertSystem(ctx, orgID, comment(teamID, hostA, domain.GitHubIssueComment, 7))
		if err != nil {
			t.Fatalf("UpsertSystem (edit): %v", err)
		}
		if edited.ID != first.ID || !edited.PostedByTF {
			t.Errorf("row after an edit = %+v, want row %s still posted by TF", edited, first.ID)
		}

		// A row an edit minted turns on when the post that preceded it is
		// recorded after it.
		late, err := s.Artifacts.UpsertSystem(ctx, orgID, comment(teamID, hostA, domain.GitHubIssueComment, 8))
		if err != nil {
			t.Fatalf("UpsertSystem (edit first): %v", err)
		}
		if late.PostedByTF {
			t.Errorf("row an edit minted = %+v, want it not posted by TF", late)
		}
		postedLate := comment(teamID, hostA, domain.GitHubIssueComment, 8)
		postedLate.PostedByTF = true
		if got, err := s.Artifacts.UpsertSystem(ctx, orgID, postedLate); err != nil || got.ID != late.ID || !got.PostedByTF {
			t.Errorf("row after the late post = %+v, %v; want row %s posted by TF", got, err, late.ID)
		}
	})

	t.Run("Posted_by_TF_keys_are_read_on_one_scope", func(t *testing.T) {
		s, orgID, teamID := mk(t)
		seed := func(scope string, space domain.GitHubCommentSpace, id int64, postedByTF bool) string {
			a := comment(teamID, scope, space, id)
			a.PostedByTF = postedByTF
			if _, err := s.Artifacts.UpsertSystem(ctx, orgID, a); err != nil {
				t.Fatalf("seed %s comment %d on %s: %v", space, id, scope, err)
			}
			return a.DedupKey
		}
		ours := seed(hostA, domain.GitHubIssueComment, 7, true)
		// A person's review comment the run edited, with the same number.
		theirs := seed(hostA, domain.GitHubReviewComment, 7, false)
		elsewhere := seed(hostB, domain.GitHubIssueComment, 8, true)
		absent := domain.GitHubCommentDedupKey(domain.GitHubIssueComment, 9)
		if ours == theirs {
			t.Fatalf("a conversation comment and a review comment numbered 7 share key %q", ours)
		}

		got, err := s.Artifacts.PostedByTFDedupKeysSystem(ctx, orgID, hostA, []string{ours, theirs, elsewhere, absent})
		if err != nil {
			t.Fatalf("PostedByTFDedupKeysSystem: %v", err)
		}
		if want := map[string]bool{ours: true}; !maps.Equal(got, want) {
			t.Errorf("posted by TF on %s = %v, want %v", hostA, got, want)
		}
		present, err := s.Artifacts.PresentDedupKeysSystem(ctx, orgID, hostA, []string{ours, theirs})
		if err != nil {
			t.Fatalf("PresentDedupKeysSystem: %v", err)
		}
		if want := map[string]bool{ours: true, theirs: true}; !maps.Equal(present, want) {
			t.Errorf("present on %s = %v, want both rows %v", hostA, present, want)
		}
		got, err = s.Artifacts.PostedByTFDedupKeysSystem(ctx, orgID, hostA, nil)
		if err != nil || len(got) != 0 {
			t.Errorf("PostedByTFDedupKeysSystem with no keys = %v, %v; want an empty set", got, err)
		}
		if _, err := s.Artifacts.PostedByTFDedupKeysSystem(ctx, orgID, "", []string{ours}); !errors.Is(err, db.ErrExternalObjectScopeRequired) {
			t.Errorf("PostedByTFDedupKeysSystem with no scope: err = %v, want ErrExternalObjectScopeRequired", err)
		}
	})
}
