package delegate

import (
	"context"
	"database/sql"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbseed"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/placement"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// fakePlacement is a stub placementResolver: Enabled is fixed, and Resolve
// returns a plan whose single preferred owner is `owner` when the repository
// id matches wantKey (so the test can assert the (org, repository) key
// derivation).
type fakePlacement struct {
	enabled bool
	wantKey string
	owner   string
	lastKey string
}

func (f *fakePlacement) Enabled() bool { return f.enabled }

func (f *fakePlacement) Resolve(_ context.Context, _, repositoryID string) (placement.Plan, error) {
	f.lastKey = repositoryID
	if repositoryID != f.wantKey {
		return placement.Plan{Enabled: true}, nil
	}
	return placement.Plan{Enabled: true, PreferredSet: []string{f.owner}}, nil
}

func githubTask() domain.Task {
	return domain.Task{ID: "t1", EntitySource: "github", EntitySourceID: "acme/web#42"}
}

// placementSpawner returns a spawner whose stores hold the GitHub task's entity
// on host and the repository row its create registered, that task, and the
// row's id.
func placementSpawner(t *testing.T, host string) (*Spawner, domain.Task, string) {
	t.Helper()
	s, task, repositoryID, _ := placementSpawnerWithDB(t, host)
	return s, task, repositoryID
}

// placementSpawnerWithDB is placementSpawner with the database behind it.
func placementSpawnerWithDB(t *testing.T, host string) (*Spawner, domain.Task, string, *sql.DB) {
	t.Helper()
	database := newDelegateTestDB(t)
	stores := sqlitestore.New(database)
	e, _, err := stores.Entities.FindOrCreate(context.Background(), runmode.LocalDefaultOrgID, "github", host, "acme/web#42", "", "pr", "T", host+"/acme/web/pull/42")
	if err != nil {
		t.Fatalf("seed entity: %v", err)
	}
	if e.RepositoryID == "" {
		t.Fatalf("the entity create registered no repository row: %+v", e)
	}
	task := githubTask()
	task.EntityID = e.ID
	return &Spawner{entities: stores.Entities, repos: stores.Repos}, task, e.RepositoryID, database
}

func TestPreferredExecutorFor_StampsRendezvousOwner(t *testing.T) {
	s, task, repositoryID := placementSpawner(t, "https://github.com")
	fp := &fakePlacement{enabled: true, wantKey: repositoryID, owner: "exec-owner"}
	s.SetPlacement(fp, db.ClaimPlacement{Enabled: true})

	got := s.preferredExecutorFor(context.Background(), runmode.LocalDefaultOrgID, task, "run-1")
	if got != "exec-owner" {
		t.Fatalf("preferred = %q, want exec-owner", got)
	}
	if fp.lastKey != repositoryID {
		t.Fatalf("resolver keyed on %q, want the registry row id %q", fp.lastKey, repositoryID)
	}
}

// TestPreferredExecutorFor_KeepsTheKeyAcrossARename pins that the key is the
// task's registry row id, read through the id its entity references, and not
// the row's name: a renamed repository stays on the owner its bare-clone
// cache, keyed by the same id, is warm on.
func TestPreferredExecutorFor_KeepsTheKeyAcrossARename(t *testing.T) {
	s, task, repositoryID, database := placementSpawnerWithDB(t, "https://github.com")
	dbseed.RenameRepositoryOnly(t, database, repositoryID, "acme", "site")
	fp := &fakePlacement{enabled: true, wantKey: repositoryID, owner: "exec-owner"}
	s.SetPlacement(fp, db.ClaimPlacement{Enabled: true})

	if got := s.preferredExecutorFor(context.Background(), runmode.LocalDefaultOrgID, task, "run-1"); got != "exec-owner" {
		t.Fatalf("preferred = %q (keyed %q), want exec-owner keyed %s", got, fp.lastKey, repositoryID)
	}
}

// TestPreferredExecutorFor_KeysOnTheEntitysOwnRepository pins that a task on
// another GitHub host is keyed on the registry row its entity references
// there: a same-named repository on another host is another row, and another
// key.
func TestPreferredExecutorFor_KeysOnTheEntitysOwnRepository(t *testing.T) {
	s, task, ghe, database := placementSpawnerWithDB(t, "https://ghe.example.com")
	dotcom, _, err := sqlitestore.New(database).Entities.FindOrCreate(context.Background(), runmode.LocalDefaultOrgID,
		"github", "https://github.com", "acme/web#42", "", "pr", "T", "https://github.com/acme/web/pull/42")
	if err != nil {
		t.Fatalf("seed github.com entity: %v", err)
	}
	if dotcom.RepositoryID == ghe {
		t.Fatalf("both hosts' acme/web share registry row %s", ghe)
	}
	fp := &fakePlacement{enabled: true, wantKey: ghe, owner: "exec-owner"}
	s.SetPlacement(fp, db.ClaimPlacement{Enabled: true})

	if got := s.preferredExecutorFor(context.Background(), runmode.LocalDefaultOrgID, task, "run-1"); got != "exec-owner" {
		t.Fatalf("preferred = %q (keyed %q), want exec-owner keyed on the ghe row %s", got, fp.lastKey, ghe)
	}
}

// TestPreferredExecutorFor_UnreadableEntityStampsNothing pins that a task
// whose entity cannot be read gets no affinity rather than an override from a
// guessed host.
func TestPreferredExecutorFor_UnreadableEntityStampsNothing(t *testing.T) {
	s, task, _ := placementSpawner(t, "https://github.com")
	task.EntityID = "no-such-entity"
	fp := &fakePlacement{enabled: true, wantKey: "acme/web", owner: "exec-owner"}
	s.SetPlacement(fp, db.ClaimPlacement{Enabled: true})

	if got := s.preferredExecutorFor(context.Background(), runmode.LocalDefaultOrgID, task, "run-1"); got != "" {
		t.Fatalf("preferred = %q, want no stamp for an unreadable entity", got)
	}
	if fp.lastKey != "" {
		t.Fatalf("the resolver was consulted for key %q with no repository to key on", fp.lastKey)
	}
}

func TestPreferredExecutorFor_DisabledResolverStampsNothing(t *testing.T) {
	s := &Spawner{}
	s.SetPlacement(&fakePlacement{enabled: false, wantKey: "acme/web", owner: "exec-owner"}, db.ClaimPlacement{})
	if got := s.preferredExecutorFor(context.Background(), "org1", githubTask(), "run-1"); got != "" {
		t.Fatalf("disabled placement must stamp nothing, got %q", got)
	}
}

func TestPreferredExecutorFor_NilResolverStampsNothing(t *testing.T) {
	s := &Spawner{}
	if got := s.preferredExecutorFor(context.Background(), "org1", githubTask(), "run-1"); got != "" {
		t.Fatalf("nil resolver must stamp nothing, got %q", got)
	}
}

func TestPreferredExecutorFor_NonGitHubTaskStampsNothing(t *testing.T) {
	s := &Spawner{}
	s.SetPlacement(&fakePlacement{enabled: true, wantKey: "irrelevant", owner: "exec-owner"}, db.ClaimPlacement{Enabled: true})
	jira := domain.Task{ID: "t2", EntitySource: "jira", EntitySourceID: "SKY-123"}
	if got := s.preferredExecutorFor(context.Background(), "org1", jira, "run-1"); got != "" {
		t.Fatalf("a non-repo (Jira) task has no placement key, want empty, got %q", got)
	}
}
