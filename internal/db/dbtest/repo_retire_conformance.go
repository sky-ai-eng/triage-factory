package dbtest

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/githubapp"
	"github.com/sky-ai-eng/triage-factory/internal/logging"
	"github.com/sky-ai-eng/triage-factory/internal/reporename"
)

// The ids the retire suite's repositories carry on GitHub: retireOldID is the
// repository TF holds octo/api for, retireNewID the one GitHub now serves
// under that name. retireOldPRNodeID is the node id of the held repository's
// pull request #12.
const (
	retireOldID       = "4100"
	retireNewID       = "4200"
	retireOldPRNodeID = "PR_kwDOretire12"
)

var retireLog = logging.Component("dbtest-retire")

// RunRepoRetireConformance covers a name GitHub has given to another
// repository: the row holding it is retired (RetireSystem) and keeps its
// history while it stops answering to the name, the repository now under the
// name registers as a row of its own, a retired row GitHub answers for again
// is restored through RenameSystem, and the observation trigger
// (reporename.Apply) decides between a rename and a retire by asking GitHub
// for the stored row's own id. It takes the rename suite's factory: the state
// it seeds is the same.
func RunRepoRetireConformance(t *testing.T, mk RepoRenameFactory) {
	t.Helper()
	ctx := context.Background()

	t.Run("Retire_frees_the_name_and_keeps_the_row", func(t *testing.T) {
		s, orgID, seed := mk(t)
		fx := seedRetireFixture(t, s, orgID, seed, "frees")
		seed.Task(t, fx.entityID, "frees")
		tasksBefore := seed.CountTasks(t)

		retired, err := s.Repos.RetireSystem(ctx, orgID, fx.repo.Ref())
		if err != nil || retired == nil {
			t.Fatalf("RetireSystem: got=%v err=%v", retired, err)
		}
		if retired.ID != fx.repo.ID || retired.RetiredAt == nil {
			t.Fatalf("retired row = %+v, want %s with retired_at set", retired, fx.repo.ID)
		}
		AssertWriteReturnedStoredRow(t, "RetireSystem", *retired, func() (*domain.Repository, error) {
			return s.Repos.GetSystem(ctx, orgID, retired.ID)
		})

		// By id it is still there, name and provider id intact.
		byID, err := s.Repos.Get(ctx, orgID, fx.repo.ID)
		if err != nil || byID.RetiredAt == nil || byID.Slug() != "octo/api" || byID.ExternalID != retireOldID {
			t.Errorf("Get by id = %+v, err=%v; want the retired octo/api row", byID, err)
		}
		// By name, and in every list, it is not.
		for _, read := range []struct {
			name string
			get  func(context.Context, string, domain.RepoRef) (*domain.Repository, error)
		}{{"GetByRef", s.Repos.GetByRef}, {"GetByRefSystem", s.Repos.GetByRefSystem}} {
			if got, err := read.get(ctx, orgID, repoRef("OCTO/api")); err != nil || got != nil {
				t.Errorf("%s(octo/api) = %+v, err=%v; a retired row answers to no name", read.name, got, err)
			}
		}
		if rows, _, err := s.Repos.List(ctx, orgID, TestGitHubHost, db.Unwindowed); err != nil || hasRepo(rows, fx.repo.ID) {
			t.Errorf("List = %v, err=%v; want the retired row left out", repoSlugs(rows), err)
		}
		if rows, err := s.Repos.ListSystem(ctx, orgID, TestGitHubHost); err != nil || hasRepo(rows, fx.repo.ID) {
			t.Errorf("ListSystem = %v, err=%v; want the retired row left out", repoSlugs(rows), err)
		}
		if rows, _, err := s.Repos.ListTrackedSystem(ctx, orgID, TestGitHubHost, db.Unwindowed); err != nil || hasRepo(rows, fx.repo.ID) {
			t.Errorf("ListTrackedSystem = %v, err=%v; want the retired row left out", repoSlugs(rows), err)
		}
		if rows, _, err := s.Repos.ListTrackedByTeamSystem(ctx, orgID, seed.TeamID, TestGitHubHost, db.Unwindowed); err != nil || hasRepo(rows, fx.repo.ID) {
			t.Errorf("ListTrackedByTeamSystem = %v, err=%v; want the retired row left out", repoSlugs(rows), err)
		}
		if names, err := s.Repos.ListTrackedNamesSystem(ctx, orgID, TestGitHubHost); err != nil || len(names) != 0 {
			t.Errorf("ListTrackedNamesSystem = %v, err=%v; nothing polls or profiles a retired row", names, err)
		}

		// Tracking is forward-only: the team keeps the row in its set, and the
		// set says it is retired. The name resolves to no row, so a caller
		// holding only the name never reaches the row's tracking.
		tracked, err := s.TeamGitHubRepos.ListForTeamSystem(ctx, seed.TeamID, TestGitHubHost)
		if err != nil || len(tracked) != 1 || tracked[0].RepositoryID != fx.repo.ID || tracked[0].RetiredAt == nil {
			t.Errorf("ListForTeamSystem = %+v, err=%v; want the retired row, marked", tracked, err)
		}
		if ok, err := s.TeamGitHubRepos.TracksRepositorySystem(ctx, seed.TeamID, fx.repo.ID); err != nil || !ok {
			t.Errorf("TracksRepositorySystem = %v, err=%v; want the team still tracking the row", ok, err)
		}
		if row, err := s.Repos.GetByRefSystem(ctx, orgID, repoRef("octo/api")); err != nil || row != nil {
			t.Errorf("GetByRefSystem(octo/api) = %+v, err=%v; the name is not the retired row's", row, err)
		}

		// Its active entities are closed, and answer to no key; by id they
		// are still there.
		ent, err := s.Entities.GetSystem(ctx, orgID, fx.entityID)
		if err != nil || ent == nil || ent.State != "closed" || ent.ClosedAt == nil {
			t.Errorf("entity = %+v, err=%v; want it closed", ent, err)
		}
		if got, err := s.Entities.GetBySourceSystem(ctx, orgID, "github", TestGitHubHost, "octo/api#12"); err != nil || got != nil {
			t.Errorf("entity by key = %+v, err=%v; a retired repository's entity answers to no key", got, err)
		}

		// Nothing else moved.
		if wt, err := s.ConversationWorktrees.GetByRepoRefSystem(ctx, orgID, fx.conversationID, fx.repo.ID, "pr-12"); err != nil || wt == nil {
			t.Errorf("worktree ledger row = %+v, err=%v; want it kept", wt, err)
		}
		if a, err := s.Deliverables.Get(ctx, orgID, fx.deliverableID); err != nil || a == nil || a.Target != "octo/api#12" {
			t.Errorf("deliverable = %+v, err=%v; want it kept as it was", a, err)
		}
		if got := seed.CountTasks(t); got != tasksBefore {
			t.Errorf("tasks went %d -> %d; a retire mints and closes none", tasksBefore, got)
		}

		ids, err := s.Repos.ListIdentitiesSystem(ctx, orgID, TestGitHubHost)
		if err != nil || len(ids) != 1 || !ids[0].Retired || ids[0].ExternalID != retireOldID {
			t.Errorf("ListIdentitiesSystem = %+v, err=%v; want the row, marked retired", ids, err)
		}
	})

	t.Run("Registration_refused_while_the_name_is_held_succeeds_once_it_is_retired", func(t *testing.T) {
		// The register-and-retry path: the new repository's registration is
		// refused while the deleted one's row holds the name, and succeeds as
		// a row of its own once that row is retired.
		s, orgID, seed := mk(t)
		fx := seedRetireFixture(t, s, orgID, seed, "register")
		incoming := domain.RepoRef{Host: TestGitHubHost, Owner: "octo", Repo: "api", ExternalID: retireNewID}

		if _, err := s.Repos.FindOrCreateByExternalID(ctx, orgID, incoming); !errors.Is(err, db.ErrRepoSlugOccupied) {
			t.Fatalf("register before the retire: err = %v, want ErrRepoSlugOccupied", err)
		}
		if _, err := s.Repos.RetireSystem(ctx, orgID, fx.repo.Ref()); err != nil {
			t.Fatalf("RetireSystem: %v", err)
		}
		reg, err := s.Repos.FindOrCreateByExternalID(ctx, orgID, incoming)
		if err != nil {
			t.Fatalf("register after the retire: %v", err)
		}
		if reg.ID == fx.repo.ID || reg.RetiredAt != nil || reg.ExternalID != retireNewID {
			t.Fatalf("registered row = %+v, want a new, live row carrying %s", reg, retireNewID)
		}
		AssertWriteReturnedStoredRow(t, "FindOrCreateByExternalID", reg, func() (*domain.Repository, error) {
			return s.Repos.Get(ctx, orgID, reg.ID)
		})

		// The name resolves to the new row, whatever path asks.
		if got, _ := s.Repos.GetByRef(ctx, orgID, repoRef("octo/api")); got == nil || got.ID != reg.ID {
			t.Errorf("GetByRef(octo/api) = %+v, want the new row", got)
		}
		if got, err := s.Repos.FindOrCreateByRefSystem(ctx, orgID, repoRef("Octo/API")); err != nil || got.ID != reg.ID {
			t.Errorf("FindOrCreateByRefSystem(Octo/API) = %+v, err=%v; want the new row", got, err)
		}
		if got, err := s.Repos.UpsertSystem(ctx, orgID, domain.Repository{
			Host: TestGitHubHost, Owner: "octo", Repo: "api", ExternalID: retireNewID, Description: "the new one",
		}); err != nil || got.ID != reg.ID {
			t.Errorf("UpsertSystem(octo/api) = %+v, err=%v; want it to land on the new row", got, err)
		}
		if old, _ := s.Repos.GetSystem(ctx, orgID, fx.repo.ID); old == nil || old.Description != "" || old.RetiredAt == nil {
			t.Errorf("retired row = %+v, want it untouched by the new row's upsert", old)
		}
		// The id lookup still reaches the retired row.
		if got, err := s.Repos.FindOrCreateByExternalID(ctx, orgID, fx.repo.Ref()); err != nil || got.ID != fx.repo.ID || got.RetiredAt == nil {
			t.Errorf("register the old id = %+v, err=%v; want the retired row, as it stands", got, err)
		}

		// The new repository's pull request #12 gets an entity of its own.
		ent, created, err := s.Entities.FindOrCreateSystem(ctx, orgID, "github", TestGitHubHost, "octo/api#12", "", "pr", "New #12",
			"https://github.com/octo/api/pull/12")
		if err != nil || !created || ent.ID == fx.entityID || ent.RepositoryID != reg.ID {
			t.Errorf("entity for the new #12 = %+v, created=%v, err=%v; want a new entity on the new row", ent, created, err)
		}

		ids, err := s.Repos.ListIdentitiesSystem(ctx, orgID, TestGitHubHost)
		if err != nil {
			t.Fatalf("ListIdentitiesSystem: %v", err)
		}
		retired := map[string]bool{}
		for _, id := range ids {
			retired[id.ExternalID] = id.Retired
		}
		if len(ids) != 2 || !retired[retireOldID] || retired[retireNewID] {
			t.Errorf("identities = %+v, want %s retired and %s live under one name", ids, retireOldID, retireNewID)
		}
	})

	t.Run("Retire_of_a_stale_candidate_is_a_no_op", func(t *testing.T) {
		s, orgID, seed := mk(t)
		fx := seedRetireFixture(t, s, orgID, seed, "stale")

		moved := fx.repo.Ref()
		moved.Repo = "api-renamed"
		if got, err := s.Repos.RetireSystem(ctx, orgID, moved); err != nil || got != nil {
			t.Errorf("retire under a slug the row is not stored under = %+v, err=%v; want a no-op", got, err)
		}
		unknown := fx.repo.Ref()
		unknown.ExternalID = "999"
		if got, err := s.Repos.RetireSystem(ctx, orgID, unknown); err != nil || got != nil {
			t.Errorf("retire of an id no row carries = %+v, err=%v; want a no-op", got, err)
		}
		if got, _ := s.Repos.GetByRef(ctx, orgID, repoRef("octo/api")); got == nil || got.RetiredAt != nil {
			t.Fatalf("octo/api = %+v after two no-ops, want it live", got)
		}

		if got, err := s.Repos.RetireSystem(ctx, orgID, fx.repo.Ref()); err != nil || got == nil {
			t.Fatalf("RetireSystem: got=%v err=%v", got, err)
		}
		if got, err := s.Repos.RetireSystem(ctx, orgID, fx.repo.Ref()); err != nil || got != nil {
			t.Errorf("second retire = %+v, err=%v; want a no-op", got, err)
		}

		bare := fx.repo.Ref()
		bare.ExternalID = ""
		if _, err := s.Repos.RetireSystem(ctx, orgID, bare); err == nil {
			t.Error("retire with no provider id succeeded; there is nothing to key on")
		}
		hostless := fx.repo.Ref()
		hostless.Host = ""
		if _, err := s.Repos.RetireSystem(ctx, orgID, hostless); !errors.Is(err, db.ErrRepoHostRequired) {
			t.Errorf("retire with no host: err = %v, want ErrRepoHostRequired", err)
		}
	})

	t.Run("Rename_restores_a_retired_row", func(t *testing.T) {
		s, orgID, seed := mk(t)
		fx := seedRetireFixture(t, s, orgID, seed, "restore")
		if _, err := s.Repos.RetireSystem(ctx, orgID, fx.repo.Ref()); err != nil {
			t.Fatalf("RetireSystem: %v", err)
		}

		// GitHub answers for the id again under the same name, and nothing
		// holds the name: the row gets it back.
		out, err := s.Repos.RenameSystem(ctx, orgID, fx.repo.Ref())
		if err != nil {
			t.Fatalf("RenameSystem: %v", err)
		}
		if !out.Restored || out.Renamed {
			t.Errorf("outcome = %+v, want a restore under the same name", out)
		}
		if got, _ := s.Repos.GetByRef(ctx, orgID, repoRef("octo/api")); got == nil || got.ID != fx.repo.ID || got.RetiredAt != nil {
			t.Fatalf("octo/api = %+v, want the restored row", got)
		}
		if ent, _ := s.Entities.GetSystem(ctx, orgID, fx.entityID); ent == nil || ent.State != "closed" {
			t.Errorf("entity = %+v, want it left closed; the poll reopens what is still open", ent)
		}
		if out, err := s.Repos.RenameSystem(ctx, orgID, fx.repo.Ref()); err != nil || out.Changed() {
			t.Errorf("second restore = %+v, err=%v; want the steady-state no-op", out, err)
		}

		// Retired again, and a new repository takes the name. The old id
		// cannot come back under it while the new row holds it...
		if _, err := s.Repos.RetireSystem(ctx, orgID, fx.repo.Ref()); err != nil {
			t.Fatalf("RetireSystem again: %v", err)
		}
		reg, err := s.Repos.FindOrCreateByExternalID(ctx, orgID, domain.RepoRef{Host: TestGitHubHost, Owner: "octo", Repo: "api", ExternalID: retireNewID})
		if err != nil {
			t.Fatalf("register the new repository: %v", err)
		}
		newEntity, _, err := s.Entities.FindOrCreateSystem(ctx, orgID, "github", TestGitHubHost, "octo/api#12", "", "pr", "New #12", "")
		if err != nil {
			t.Fatalf("seed the new repository's #12: %v", err)
		}
		if _, err := s.Repos.RenameSystem(ctx, orgID, fx.repo.Ref()); !errors.Is(err, db.ErrRepoSlugOccupied) {
			t.Fatalf("restore onto a held name: err = %v, want ErrRepoSlugOccupied", err)
		}

		// ...and comes back under the name GitHub reports for it instead.
		legacy := fx.repo.Ref()
		legacy.Repo = "api-legacy"
		out, err = s.Repos.RenameSystem(ctx, orgID, legacy)
		if err != nil {
			t.Fatalf("RenameSystem to octo/api-legacy: %v", err)
		}
		if !out.Restored || !out.Renamed || out.From != "octo/api" || out.To != "octo/api-legacy" {
			t.Errorf("outcome = %+v, want a restore that moved octo/api to octo/api-legacy", out)
		}
		if got, _ := s.Repos.GetByRef(ctx, orgID, repoRef("octo/api-legacy")); got == nil || got.ID != fx.repo.ID {
			t.Errorf("octo/api-legacy = %+v, want the restored row", got)
		}
		if got, _ := s.Repos.GetByRef(ctx, orgID, repoRef("octo/api")); got == nil || got.ID != reg.ID {
			t.Errorf("octo/api = %+v, want the new repository's row, untouched", got)
		}
		// Its own entities move; the new repository's do not.
		if ent, _ := s.Entities.GetSystem(ctx, orgID, fx.entityID); ent == nil || ent.SourceID != "octo/api-legacy#12" {
			t.Errorf("restored row's entity = %+v, want octo/api-legacy#12", ent)
		}
		if ent, _ := s.Entities.GetSystem(ctx, orgID, newEntity.ID); ent == nil || ent.SourceID != "octo/api#12" {
			t.Errorf("new repository's entity = %+v, want octo/api#12 untouched", ent)
		}
		// Its own deliverable, keyed on its pull request's node id, moves with
		// it too.
		if a, _ := s.Deliverables.Get(ctx, orgID, fx.deliverableID); a == nil || a.Target != "octo/api-legacy#12" {
			t.Errorf("deliverable = %+v, want it moved to octo/api-legacy#12 with the restored row", a)
		}
		// And so do its audit-ledger links, which reference its entity and its
		// registry row, while every record keeps the name it was made under.
		for key, want := range map[string]string{
			fx.prAction:   "https://github.com/octo/api-legacy/pull/12",
			fx.pushAction: "https://github.com/octo/api-legacy/tree/fix",
		} {
			if u, cur := seed.RawActionURL(t, key); cur != want || !strings.Contains(u, "/octo/api/") {
				t.Errorf("action %s = url %q current_url %q, want the record kept and the pointer at %q", key, u, cur, want)
			}
		}
	})

	t.Run("Renaming_the_holder_leaves_a_retired_namesake_alone", func(t *testing.T) {
		s, orgID, seed := mk(t)
		fx := seedRetireFixture(t, s, orgID, seed, "namesake")
		if _, err := s.Repos.RetireSystem(ctx, orgID, fx.repo.Ref()); err != nil {
			t.Fatalf("RetireSystem: %v", err)
		}
		reg, err := s.Repos.FindOrCreateByExternalID(ctx, orgID, domain.RepoRef{Host: TestGitHubHost, Owner: "octo", Repo: "api", ExternalID: retireNewID})
		if err != nil {
			t.Fatalf("register the new repository: %v", err)
		}
		newEntity, _, err := s.Entities.FindOrCreateSystem(ctx, orgID, "github", TestGitHubHost, "octo/api#12", "", "pr", "New #12", "")
		if err != nil {
			t.Fatalf("seed the new repository's #12: %v", err)
		}

		out, err := s.Repos.RenameSystem(ctx, orgID, domain.RepoRef{Host: TestGitHubHost, Owner: "octo", Repo: "api-next", ExternalID: retireNewID})
		if err != nil || !out.Renamed || out.Restored {
			t.Fatalf("RenameSystem = %+v, err=%v; want a plain rename", out, err)
		}
		if ent, _ := s.Entities.GetSystem(ctx, orgID, newEntity.ID); ent == nil || ent.SourceID != "octo/api-next#12" {
			t.Errorf("renamed repository's entity = %+v, want octo/api-next#12", ent)
		}
		if ent, _ := s.Entities.GetSystem(ctx, orgID, fx.entityID); ent == nil || ent.SourceID != "octo/api#12" {
			t.Errorf("retired row's entity = %+v, want octo/api#12; it is not the renamed repository's", ent)
		}
		// The retired row's audit-ledger links spell the renamed repository's
		// old name too, and stay: they are about the retired row.
		for _, key := range []string{fx.prAction, fx.pushAction} {
			if _, cur := seed.RawActionURL(t, key); cur != "" {
				t.Errorf("retired row's action %s current_url = %q, want it untouched", key, cur)
			}
		}
		if old, _ := s.Repos.GetSystem(ctx, orgID, fx.repo.ID); old == nil || old.Slug() != "octo/api" || old.RetiredAt == nil {
			t.Errorf("retired row = %+v, want it where it was", old)
		}
		if got, _ := s.Repos.GetByRef(ctx, orgID, repoRef("octo/api-next")); got == nil || got.ID != reg.ID {
			t.Errorf("octo/api-next = %+v, want the renamed row", got)
		}
	})

	// The observation trigger, end to end against the store: GitHub serves
	// the name TF holds for retireOldID under retireNewID, and Apply asks
	// GitHub what became of retireOldID.
	t.Run("Upsert_never_writes_a_retired_rows_id_onto_another_row", func(t *testing.T) {
		// GitHub serves the retired row's id under its old name again, and an
		// upsert under that name lands first. The row it creates, and then
		// updates, is not the retired one, so the id stays on the retired row
		// for a restore to pick up rather than failing the external-identity
		// index.
		s, orgID, seed := mk(t)
		fx := seedRetireFixture(t, s, orgID, seed, "upsert")
		if _, err := s.Repos.RetireSystem(ctx, orgID, fx.repo.Ref()); err != nil {
			t.Fatalf("RetireSystem: %v", err)
		}
		profile := domain.Repository{
			Source: domain.RepoSourceGitHub, Host: TestGitHubHost, Owner: "octo", Repo: "api",
			ExternalID: retireOldID, DefaultBranch: "main",
		}
		created, err := s.Repos.UpsertSystem(ctx, orgID, profile)
		if err != nil {
			t.Fatalf("Upsert creating a row under the retired name: %v", err)
		}
		updated, err := s.Repos.UpsertSystem(ctx, orgID, profile)
		if err != nil {
			t.Fatalf("Upsert updating that row: %v", err)
		}
		for _, got := range []domain.Repository{created, updated} {
			if got.ID == fx.repo.ID || got.ID != created.ID || got.ExternalID != "" || got.RetiredAt != nil {
				t.Errorf("upserted = %+v, want one new live row %s with no GitHub id", got, created.ID)
			}
		}
		if old, err := s.Repos.GetSystem(ctx, orgID, fx.repo.ID); err != nil || old.ExternalID != retireOldID || old.RetiredAt == nil {
			t.Errorf("retired row = %+v, err=%v; want it retired with its id", old, err)
		}
	})

	t.Run("Observation_retires_a_deleted_repository", func(t *testing.T) {
		s, orgID, seed := mk(t)
		fx := seedRetireFixture(t, s, orgID, seed, "observe-deleted")
		gh := serveGitHubRepositories(t, map[string]string{}, http.StatusNotFound)

		if n, _ := reporename.Apply(ctx, s.Repos, gh, retireLog, orgID, TestGitHubHost, reassignedObservation()); n != 1 {
			t.Fatalf("applied = %d, want 1", n)
		}
		if got, _ := s.Repos.GetSystem(ctx, orgID, fx.repo.ID); got == nil || got.RetiredAt == nil {
			t.Fatalf("stored row = %+v, want it retired", got)
		}
		if ent, _ := s.Entities.GetSystem(ctx, orgID, fx.entityID); ent == nil || ent.State != "closed" {
			t.Errorf("entity = %+v, want it closed", ent)
		}
		if _, err := s.Repos.FindOrCreateByExternalID(ctx, orgID, domain.RepoRef{Host: TestGitHubHost, Owner: "octo", Repo: "api", ExternalID: retireNewID}); err != nil {
			t.Errorf("register the new repository after the retire: %v", err)
		}
	})

	t.Run("Observation_renames_a_repository_renamed_away", func(t *testing.T) {
		// A PAT org's case: the name was freed by a rename and reused, which
		// GitHub does not redirect. Asking by the old id finds the rename.
		s, orgID, seed := mk(t)
		fx := seedRetireFixture(t, s, orgID, seed, "observe-renamed")
		gh := serveGitHubRepositories(t, map[string]string{retireOldID: "octo/api-v1"}, http.StatusNotFound)

		if n, _ := reporename.Apply(ctx, s.Repos, gh, retireLog, orgID, TestGitHubHost, reassignedObservation()); n != 1 {
			t.Fatalf("applied = %d, want 1", n)
		}
		got, _ := s.Repos.GetByRef(ctx, orgID, repoRef("octo/api-v1"))
		if got == nil || got.ID != fx.repo.ID || got.RetiredAt != nil {
			t.Fatalf("octo/api-v1 = %+v, want the stored row, renamed and live", got)
		}
		if got, _ := s.Repos.GetByRef(ctx, orgID, repoRef("octo/api")); got != nil {
			t.Errorf("octo/api = %+v, want the name free", got)
		}
		if ent, _ := s.Entities.GetSystem(ctx, orgID, fx.entityID); ent == nil || ent.State != "active" || ent.SourceID != "octo/api-v1#12" {
			t.Errorf("entity = %+v, want it active under octo/api-v1#12", ent)
		}
		if a, _ := s.Deliverables.Get(ctx, orgID, fx.deliverableID); a == nil || a.Target != "octo/api-v1#12" {
			t.Errorf("deliverable = %+v, want it moved with the repository", a)
		}
	})

	t.Run("Observation_never_retires_on_a_failed_request", func(t *testing.T) {
		s, orgID, seed := mk(t)
		fx := seedRetireFixture(t, s, orgID, seed, "observe-failed")
		gh := serveGitHubRepositories(t, map[string]string{}, http.StatusServiceUnavailable)

		if n, _ := reporename.Apply(ctx, s.Repos, gh, retireLog, orgID, TestGitHubHost, reassignedObservation()); n != 0 {
			t.Fatalf("applied = %d, want 0", n)
		}
		if got, _ := s.Repos.GetByRef(ctx, orgID, repoRef("octo/api")); got == nil || got.ID != fx.repo.ID || got.RetiredAt != nil {
			t.Errorf("octo/api = %+v, want the stored row untouched", got)
		}
		if ent, _ := s.Entities.GetSystem(ctx, orgID, fx.entityID); ent == nil || ent.State != "active" {
			t.Errorf("entity = %+v, want it untouched", ent)
		}
	})

	t.Run("Observation_restores_a_retired_row_GitHub_answers_for", func(t *testing.T) {
		s, orgID, seed := mk(t)
		fx := seedRetireFixture(t, s, orgID, seed, "observe-restore")
		if _, err := s.Repos.RetireSystem(ctx, orgID, fx.repo.Ref()); err != nil {
			t.Fatalf("RetireSystem: %v", err)
		}
		gh := serveGitHubRepositories(t, map[string]string{}, http.StatusNotFound)

		if n, _ := reporename.Apply(ctx, s.Repos, gh, retireLog, orgID, TestGitHubHost, []domain.RepoRef{
			{Source: domain.RepoSourceGitHub, Owner: "acme", Repo: "api", ExternalID: retireOldID},
		}); n != 1 {
			t.Fatalf("applied = %d, want 1", n)
		}
		if got, _ := s.Repos.GetByRef(ctx, orgID, repoRef("acme/api")); got == nil || got.ID != fx.repo.ID || got.RetiredAt != nil {
			t.Errorf("acme/api = %+v, want the stored row restored under it", got)
		}
	})
}

// retireFixture is the state a repository leaves behind in TF: octo/api,
// tracked by the seed team under GitHub id retireOldID, with an active pull
// request entity, a worktree ledger row and a pull request deliverable.
type retireFixture struct {
	repo           domain.Repository
	entityID       string
	conversationID string
	deliverableID  string
	// prAction and pushAction are the dedup keys of the audit-ledger rows of
	// the pull request's creation (about its entity) and a branch push (about
	// the repository's registry row).
	prAction, pushAction string
}

func seedRetireFixture(t *testing.T, s db.Stores, orgID string, seed RepoRenameSeeder, suffix string) retireFixture {
	t.Helper()
	ctx := context.Background()
	if err := TrackRepos(ctx, s.Repos, s.TeamGitHubRepos, orgID, seed.TeamID, TestGitHubHost, []domain.TeamGitHubRepo{
		{Owner: "octo", Repo: "api"},
	}); err != nil {
		t.Fatalf("seed tracked octo/api: %v", err)
	}
	if filled, err := s.Repos.FillMissingExternalIDsSystem(ctx, orgID, []domain.RepoRef{
		{Host: TestGitHubHost, Owner: "octo", Repo: "api", ExternalID: retireOldID},
	}); err != nil || filled != 1 {
		t.Fatalf("seed octo/api's id: filled=%d err=%v", filled, err)
	}
	repo, err := s.Repos.GetByRefSystem(ctx, orgID, repoRef("octo/api"))
	if err != nil || repo == nil {
		t.Fatalf("resolve octo/api: got=%v err=%v", repo, err)
	}
	entity, _, err := s.Entities.FindOrCreateSystem(ctx, orgID, "github", TestGitHubHost, "octo/api#12", retireOldPRNodeID, "pr", "Old #12",
		"https://github.com/octo/api/pull/12")
	if err != nil {
		t.Fatalf("seed entity: %v", err)
	}
	conversationID := seed.Conversation(t, suffix)
	if _, _, err := s.ConversationWorktrees.Insert(ctx, orgID, domain.ConversationWorktree{
		ConversationID: conversationID, RepositoryID: repo.ID, Ref: "pr-12",
		Path: "/tmp/wt/" + conversationID + "/octo/api/pr-12",
	}); err != nil {
		t.Fatalf("seed worktree: %v", err)
	}
	deliverable, err := s.Deliverables.UpsertSystem(ctx, orgID, domain.Deliverable{
		TeamID: seed.TeamID, Provider: domain.ProviderGitHub, Kind: domain.DeliverableKindPullRequest, Scope: TestGitHubHost,
		Target: domain.PullRequestTarget("octo/api", 12), ExternalID: "12", NodeID: retireOldPRNodeID,
		State: domain.DeliverableStatePROpen, DedupKey: domain.PullRequestDedupKey(retireOldPRNodeID),
		EntityID: entity.ID,
	})
	if err != nil {
		t.Fatalf("seed deliverable: %v", err)
	}
	prAction, pushAction := "retire-created-"+suffix, "retire-pushed-"+suffix
	for _, act := range []domain.ExternalAction{
		{Action: domain.ActionPRCreated, Target: "octo/api#12", EntityID: entity.ID, URL: "https://github.com/octo/api/pull/12", DedupKey: prAction},
		{Action: domain.ActionBranchPushed, Target: "octo/api", RepositoryID: repo.ID, URL: "https://github.com/octo/api/tree/fix", DedupKey: pushAction},
	} {
		act.TeamID, act.Provider, act.Scope, act.Credential = seed.TeamID, domain.ProviderGitHub, TestGitHubHost, domain.CredentialGitHubApp
		act.PerformedBy = domain.PerformedByAgent
		if err := s.ExternalActions.RecordSystem(ctx, orgID, act); err != nil {
			t.Fatalf("seed ledger row %s: %v", act.DedupKey, err)
		}
	}
	return retireFixture{repo: *repo, entityID: entity.ID, conversationID: conversationID, deliverableID: deliverable.ID, prAction: prAction, pushAction: pushAction}
}

// reassignedObservation is GitHub reporting octo/api under retireNewID.
func reassignedObservation() []domain.RepoRef {
	return []domain.RepoRef{{Source: domain.RepoSourceGitHub, Owner: "octo", Repo: "api", ExternalID: retireNewID}}
}

func hasRepo(rows []domain.Repository, id string) bool {
	for _, r := range rows {
		if r.ID == id {
			return true
		}
	}
	return false
}

func repoSlugs(rows []domain.Repository) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.Slug()
	}
	return out
}

// serveGitHubRepositories is a GitHub whose GET /repositories/{id} answers
// byID's full_name for a known id and status for any other, behind a resolver
// that hands every caller a client for it. A 5xx carries a Retry-After past
// what the client waits out, so it fails on the first attempt.
func serveGitHubRepositories(t *testing.T, byID map[string]string, status int) ghclient.Resolver {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, id, ok := strings.Cut(r.URL.Path, "/repositories/")
		if name, found := byID[id]; ok && found {
			_, _ = w.Write([]byte(`{"id":` + id + `,"full_name":"` + name + `"}`))
			return
		}
		if status >= 500 {
			w.Header().Set("Retry-After", "3600")
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"message":"` + http.StatusText(status) + `"}`))
	}))
	t.Cleanup(srv.Close)
	return githubAt{url: srv.URL}
}

// githubAt is a ghclient.Resolver that resolves every client against one URL
// with no credential.
type githubAt struct{ url string }

func (g githubAt) ClientFor(context.Context, string, string) (*ghclient.Client, error) {
	return ghclient.NewClient(g.url, ""), nil
}

func (g githubAt) ClientForInstallation(context.Context, string, domain.OrgGitHubAppInstallation) (*ghclient.Client, error) {
	return ghclient.NewClient(g.url, ""), nil
}

func (g githubAt) ClientForRepo(context.Context, string, string, string) (*ghclient.Client, error) {
	return ghclient.NewClient(g.url, ""), nil
}

func (githubAt) TokenFor(context.Context, string, string) (githubapp.Token, error) {
	return githubapp.Token{}, errors.New("no token in the conformance suite")
}

func (g githubAt) BaseURLFor(context.Context, string) (string, error) { return g.url, nil }

func (githubAt) OrgIdentityFor(context.Context, string) (string, string, bool) { return "", "", false }
