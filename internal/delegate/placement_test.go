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
// returns a plan whose single preferred owner is `owner` when the key value
// matches wantKey (so the test can assert the (org, repo) key derivation).
type fakePlacement struct {
	enabled  bool
	wantKey  string
	owner    string
	lastKey  string
	lastHost string
}

func (f *fakePlacement) Enabled() bool { return f.enabled }

func (f *fakePlacement) Resolve(_ context.Context, _, _, host, keyValue string) (placement.Plan, error) {
	f.lastKey, f.lastHost = keyValue, host
	if keyValue != f.wantKey {
		return placement.Plan{Enabled: true}, nil
	}
	return placement.Plan{Enabled: true, PreferredSet: []string{f.owner}}, nil
}

func githubTask() domain.Task {
	return domain.Task{ID: "t1", EntitySource: "github", EntitySourceID: "acme/web#42"}
}

// placementSpawner returns a spawner whose stores hold the GitHub task's entity
// on host and the repository row its create registered, and that task.
func placementSpawner(t *testing.T, host string) (*Spawner, domain.Task) {
	t.Helper()
	s, task, _ := placementSpawnerWithDB(t, host)
	return s, task
}

// placementSpawnerWithDB is placementSpawner with the database behind it.
func placementSpawnerWithDB(t *testing.T, host string) (*Spawner, domain.Task, *sql.DB) {
	t.Helper()
	database := newDelegateTestDB(t)
	stores := sqlitestore.New(database)
	e, _, err := stores.Entities.FindOrCreate(context.Background(), runmode.LocalDefaultOrgID, "github", host, "acme/web#42", "", "pr", "T", host+"/acme/web/pull/42")
	if err != nil {
		t.Fatalf("seed entity: %v", err)
	}
	task := githubTask()
	task.EntityID = e.ID
	return &Spawner{entities: stores.Entities, repos: stores.Repos}, task, database
}

func TestPreferredExecutorFor_StampsRendezvousOwner(t *testing.T) {
	s, task := placementSpawner(t, "https://github.com")
	fp := &fakePlacement{enabled: true, wantKey: "acme/web", owner: "exec-owner"}
	s.SetPlacement(fp, db.ClaimPlacement{Enabled: true})

	got := s.preferredExecutorFor(context.Background(), runmode.LocalDefaultOrgID, task, "run-1")
	if got != "exec-owner" {
		t.Fatalf("preferred = %q, want exec-owner", got)
	}
	if fp.lastKey != "acme/web" {
		t.Fatalf("resolver keyed on %q, want the owner/repo 'acme/web'", fp.lastKey)
	}
	if fp.lastHost != "https://github.com" {
		t.Fatalf("resolver read the override on %q, want the entity's host", fp.lastHost)
	}
}

// TestPreferredExecutorFor_KeysOnTheRepositorysCurrentName pins that the key
// is the task's registry row's name, read through the id its entity
// references: a repository renamed with no key rewritten is keyed by its new
// name, the one its overrides are written under.
func TestPreferredExecutorFor_KeysOnTheRepositorysCurrentName(t *testing.T) {
	s, task, database := placementSpawnerWithDB(t, "https://github.com")
	e, err := s.entities.GetSystem(context.Background(), runmode.LocalDefaultOrgID, task.EntityID)
	if err != nil || e == nil {
		t.Fatalf("read entity: %v, %v", e, err)
	}
	dbseed.RenameRepositoryOnly(t, database, e.RepositoryID, "acme", "site")
	fp := &fakePlacement{enabled: true, wantKey: "acme/site", owner: "exec-owner"}
	s.SetPlacement(fp, db.ClaimPlacement{Enabled: true})

	if got := s.preferredExecutorFor(context.Background(), runmode.LocalDefaultOrgID, task, "run-1"); got != "exec-owner" {
		t.Fatalf("preferred = %q (keyed %q), want exec-owner keyed acme/site", got, fp.lastKey)
	}
}

// TestPreferredExecutorFor_ReadsTheOverrideOnTheEntitysHost pins that the
// override key's host is the task entity's scope — the host its repository
// lives on — not some other host the org knows.
func TestPreferredExecutorFor_ReadsTheOverrideOnTheEntitysHost(t *testing.T) {
	s, task := placementSpawner(t, "https://ghe.example.com")
	fp := &fakePlacement{enabled: true, wantKey: "acme/web", owner: "exec-owner"}
	s.SetPlacement(fp, db.ClaimPlacement{Enabled: true})

	if got := s.preferredExecutorFor(context.Background(), runmode.LocalDefaultOrgID, task, "run-1"); got != "exec-owner" {
		t.Fatalf("preferred = %q, want exec-owner", got)
	}
	if fp.lastHost != "https://ghe.example.com" {
		t.Fatalf("resolver read the override on %q, want https://ghe.example.com", fp.lastHost)
	}
}

// TestPreferredExecutorFor_UnreadableEntityStampsNothing pins that a task
// whose entity cannot be read gets no affinity rather than an override from a
// guessed host.
func TestPreferredExecutorFor_UnreadableEntityStampsNothing(t *testing.T) {
	s, task := placementSpawner(t, "https://github.com")
	task.EntityID = "no-such-entity"
	fp := &fakePlacement{enabled: true, wantKey: "acme/web", owner: "exec-owner"}
	s.SetPlacement(fp, db.ClaimPlacement{Enabled: true})

	if got := s.preferredExecutorFor(context.Background(), runmode.LocalDefaultOrgID, task, "run-1"); got != "" {
		t.Fatalf("preferred = %q, want no stamp for an unreadable entity", got)
	}
	if fp.lastKey != "" {
		t.Fatalf("the resolver was consulted for key %q with no host to read the override on", fp.lastKey)
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
