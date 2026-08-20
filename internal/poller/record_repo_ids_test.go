package poller

import (
	"context"
	"errors"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
)

// A repository's id is what a rename does not move, so rename detection can
// only key on it — and a NULL id is treated as "not renamable", which means a
// repository whose id TF never recorded is one whose rename is never detected.
//
// The installation grant this cycle already fetched carries the id of every
// repo in it, so this is where the id is free. What the poller must get right
// is scope: the repos it records are the tracked∩granted intersection, because
// a repo nobody tracks has no row to write, and the ids come from the grant
// listing rather than any per-repo fetch.
func TestRecordRepoIDs_WritesTheIntersectionFromTheGrant(t *testing.T) {
	repos := &repoIDFillStore{}
	m := &Manager{repos: repos}

	grant := []ghclient.UserRepo{
		{ID: 1296269, FullName: "Octo/Engineering"}, // grant's casing differs from tracked
		{ID: 555, FullName: "octo/marketing"},
		{ID: 777, FullName: "octo/unconfigured"}, // granted, untracked — no row to write
		{FullName: "octo/idless"},                // no id in the payload
	}
	scoped := []string{"octo/engineering", "octo/marketing", "octo/idless"}

	m.recordRepoIDs(context.Background(), "org-1", scoped, grant)

	if len(repos.calls) != 1 {
		t.Fatalf("FillMissingExternalIDsSystem called %d times, want 1", len(repos.calls))
	}
	got := repos.calls[0]
	if got.orgID != "org-1" {
		t.Errorf("orgID = %q, want org-1", got.orgID)
	}

	bySlug := map[string]domain.RepoRef{}
	for _, ref := range got.refs {
		bySlug[ref.Slug()] = ref
	}
	if len(bySlug) != 2 {
		t.Fatalf("refs = %+v, want exactly the two tracked repos the grant had ids for", got.refs)
	}
	// The tracked spelling is what identifies the row; the grant supplies only
	// the id. (The store matches case-insensitively either way, but sending the
	// grant's casing would make the write look like a rename to a reader.)
	eng, ok := bySlug["octo/engineering"]
	if !ok {
		t.Fatalf("engineering missing from refs: %+v", got.refs)
	}
	if eng.ExternalID != "1296269" {
		t.Errorf("engineering external id = %q, want 1296269 from the grant", eng.ExternalID)
	}
	if eng.Source != domain.RepoSourceGitHub {
		t.Errorf("source = %q, want %q", eng.Source, domain.RepoSourceGitHub)
	}
	if _, ok := bySlug["octo/unconfigured"]; ok {
		t.Error("a granted-but-untracked repo was written; it has no row, and the poller must not create one")
	}
	if _, ok := bySlug["octo/idless"]; ok {
		t.Error("a repo the grant carried no id for was written; absent is not an id")
	}
}

// The fill is bookkeeping riding along on a poll cycle. If it fails, the cycle
// still has to poll — the id is free again next cycle, and a repository whose
// id is late is in exactly the state it was in before this existed.
func TestRecordRepoIDs_StoreFailureIsNotFatal(t *testing.T) {
	repos := &repoIDFillStore{err: errors.New("db is having a day")}
	m := &Manager{repos: repos}

	m.recordRepoIDs(context.Background(), "org-1",
		[]string{"octo/engineering"},
		[]ghclient.UserRepo{{ID: 1, FullName: "octo/engineering"}})

	if len(repos.calls) != 1 {
		t.Fatalf("store called %d times, want 1", len(repos.calls))
	}
	// Reaching here at all is the assertion: recordRepoIDs swallowed the error.
}

// Nothing to record means no round-trip: an org whose grant carries no ids for
// its tracked repos (or whose ids are all already stored, which reaches the
// store as a no-op) must not cost a statement per cycle for nothing.
func TestRecordRepoIDs_NoIDsMeansNoCall(t *testing.T) {
	repos := &repoIDFillStore{}
	m := &Manager{repos: repos}

	m.recordRepoIDs(context.Background(), "org-1",
		[]string{"octo/engineering"},
		[]ghclient.UserRepo{{FullName: "octo/engineering"}}) // no id

	if len(repos.calls) != 0 {
		t.Errorf("store called %d times with nothing to record, want 0", len(repos.calls))
	}
}

// A poller wired without a repo store (the shape some tests construct) must not
// panic on the way through.
func TestRecordRepoIDs_NilStore(t *testing.T) {
	m := &Manager{}
	m.recordRepoIDs(context.Background(), "org-1",
		[]string{"octo/engineering"},
		[]ghclient.UserRepo{{ID: 1, FullName: "octo/engineering"}})
}

type repoIDFillCall struct {
	orgID string
	refs  []domain.RepoRef
}

// repoIDFillStore embeds the interface (nil) so any method this path does
// not use panics loudly rather than silently returning a zero value.
type repoIDFillStore struct {
	db.RepositoryStore
	calls []repoIDFillCall
	err   error
}

func (s *repoIDFillStore) FillMissingExternalIDsSystem(_ context.Context, orgID string, refs []domain.RepoRef) (int, error) {
	s.calls = append(s.calls, repoIDFillCall{orgID: orgID, refs: refs})
	if s.err != nil {
		return 0, s.err
	}
	return len(refs), nil
}
