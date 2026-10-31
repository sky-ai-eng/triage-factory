package tracker

import (
	"context"
	"testing"

	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// GitHub answers UNKNOWN for mergeable while it computes mergeability. The
// refresh stores the last known answer instead, so an UNKNOWN reading between
// two others is neither a conflict nor a resolution.

// refreshMergeableSequence stores octo/repo#7 with mergeable seeded, then
// refreshes it once per reading, GitHub answering each reading's mergeable in
// turn. It returns every transition the refreshes emitted and the mergeable
// each refresh stored.
func refreshMergeableSequence(t *testing.T, seeded string, readings ...string) (emitted []domain.Event, stored []string) {
	t.Helper()
	ctx := context.Background()
	database := newMigratedSQLite(t)
	stores := sqlitestore.New(database)
	entityID := seedReviewedPR(t, stores, domain.PRSnapshot{Mergeable: seeded})
	pub := &recordingPublisher{}
	tr := New(database, pub, stores.Tasks, stores.Entities, stores.Repos, stores.JiraProjects, stores.Deliverables, stores.ExternalActions, stores.EventQueue, runmode.LocalDefaultOrgID)

	for _, reading := range readings {
		reads := 0
		srv := reviewRefreshServer(t, `"mergeable":"`+reading+`"`, `{}`, &reads)
		if _, _, err := tr.RefreshGitHub(ctx, reviewTestHost, ghclient.NewClient(srv.URL, "tok"), domain.GitHubAccount{}, nil, nil); err != nil {
			t.Fatalf("refresh reading %s: %v", reading, err)
		}
		stored = append(stored, storedPRSnapshot(t, stores, entityID).Mergeable)
	}
	return pub.nonSystemEvents(), stored
}

func TestRefreshGitHub_UnknownBetweenConflictsEmitsConflictsOnce(t *testing.T) {
	emitted, stored := refreshMergeableSequence(t, "MERGEABLE", "CONFLICTING", "UNKNOWN", "CONFLICTING")
	if got := eventTypes(emitted); len(got) != 1 || got[0] != domain.EventGitHubPRConflicts {
		t.Errorf("transitions = %v, want one %s", got, domain.EventGitHubPRConflicts)
	}
	if stored[1] != "CONFLICTING" {
		t.Errorf("mergeable stored after the UNKNOWN reading = %q, want CONFLICTING", stored[1])
	}
}

func TestRefreshGitHub_UnknownBeforeMergeableEmitsConflictsResolved(t *testing.T) {
	emitted, _ := refreshMergeableSequence(t, "MERGEABLE", "CONFLICTING", "UNKNOWN", "MERGEABLE")
	want := []string{domain.EventGitHubPRConflicts, domain.EventGitHubPRConflictsResolved}
	if got := eventTypes(emitted); len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("transitions = %v, want %v", got, want)
	}
}
