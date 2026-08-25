package delegate

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/paths"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// retargetEntity re-points a fixture conversation's entity at another source,
// so one seeding path can produce the GitHub, Jira and Slack task shapes the
// resume dispatch has to tell apart. The task row carries no copy of either
// field — it reads them off this row through the store's join — so the update
// is all the retarget takes.
func retargetEntity(t *testing.T, database *sql.DB, conversationID, source, sourceID string) {
	t.Helper()
	if _, err := database.Exec(
		`UPDATE entities SET source = ?, source_id = ?
		   WHERE id = (SELECT entity_id FROM tasks
		                WHERE id = (SELECT task_id FROM conversations WHERE id = ?))`,
		source, sourceID, conversationID); err != nil {
		t.Fatalf("retarget entity for %s: %v", conversationID, err)
	}
}

// TestDispatchResumeClaim_SeedsRepoWithoutIssueSuffix pins the repo the cold
// rehydrate seeds git with. A GitHub entity is addressed "owner/repo#N", and
// the "#N" has to be gone before the owner/repo split: the repository row is
// keyed by the bare name, and so are the shared bare's path and the clone URL
// built when that row is missing — a name carrying the issue number misses the
// row and then asks the remote for a repository that does not exist, which
// fails the same way on every retry, so the follow-up never lands.
//
// It asserts the name that reached the repository lookup rather than an
// outcome: the rehydrate itself fails here for want of a real snapshot, and
// the seed is resolved before that.
//
// Jira and Slack look up nothing at all. Jira has no owner/repo to find, and
// Slack's "<channel>/<timestamp>" address would split into a plausible-looking
// pair that names no repository anyone has.
func TestDispatchResumeClaim_SeedsRepoWithoutIssueSuffix(t *testing.T) {
	cases := []struct {
		name        string
		source      string
		sourceID    string
		wantLookups []string
	}{
		{"github PR", "github", "acme/widgets#7", []string{"acme/widgets"}},
		{"jira", "jira", "SKY-123", nil},
		{"slack", "slack", "C024BE91L/1712345678.000200", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			paths.SetForTest(t, t.TempDir())
			s, database, conversationID, _ := setupAdvanceFixture(t, "seed-"+tc.source)
			repos := &seedRepositoryStore{profile: &domain.Repository{
				Owner: "acme", Repo: "widgets", CloneURL: "https://github.com/acme/widgets.git",
			}}
			s.repos = repos
			retargetEntity(t, database, conversationID, tc.source, tc.sourceID)

			// The torn-down tree the ticket is about: a worktree path that is
			// no longer on disk, with a durable snapshot so the follow-up is
			// accepted at enqueue and the claim cold-rehydrates.
			wireBlobStore(t, s)
			putTestSnapshot(t, s, blueprintRunIDForConversation(t, database, conversationID))
			if _, err := database.Exec(`UPDATE conversations SET status='open', worktree_path=? WHERE id=?`,
				filepath.Join(t.TempDir(), "reclaimed"), conversationID); err != nil {
				t.Fatalf("park open with a reclaimed worktree: %v", err)
			}

			if err := s.SendMessage(context.Background(), runmode.LocalDefaultOrgID, conversationID, runmode.LocalDefaultUserID, "carry on"); err != nil {
				t.Fatalf("SendMessage: %v", err)
			}
			claimAndDispatch(t, s, database)

			if len(repos.gotNames) != len(tc.wantLookups) {
				t.Fatalf("repository lookups = %v, want %v", repos.gotNames, tc.wantLookups)
			}
			for i, want := range tc.wantLookups {
				if repos.gotNames[i] != want {
					t.Errorf("repository lookup %d = %q, want %q", i, repos.gotNames[i], want)
				}
			}
		})
	}
}
