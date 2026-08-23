package dbtest

import (
	"context"
	"errors"
	"sort"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// RepoRenameFactory is what a per-backend test file hands to
// RunRepoRenameConformance. Returns:
//   - the WHOLE store bundle, not just RepositoryStore: the rewrite spans tables
//     six other stores own, and the assertions read each one back through its
//     own store so the harness never learns a backend's column layout,
//   - the orgID to pass to every call,
//   - a RepoRenameSeeder for the three fixtures no store mints — the team the
//     tracked set and artifacts hang off, a conversation for the worktree
//     ledger, and a task for the "a rename creates and closes nothing" case.
type RepoRenameFactory func(t *testing.T) (stores db.Stores, orgID string, seed RepoRenameSeeder)

// RepoRenameSeeder is the bag of fixtures the conformance suite cannot build
// through a store interface.
type RepoRenameSeeder struct {
	// TeamID owns the tracked-repo rows and the artifacts.
	TeamID string

	// Conversation inserts a conversation row so a worktree ledger entry has
	// something to hang off, and returns its id. suffix discriminates per
	// subtest so unique keys don't collide.
	Conversation func(t *testing.T, suffix string) string

	// Task inserts a task against entityID (plus whatever event/prompt FK
	// chain the backend's schema requires) and returns its id.
	Task func(t *testing.T, entityID, suffix string) string

	// CountTasks returns how many task rows the org has. The rename must not
	// move this number in either direction.
	CountTasks func(t *testing.T) int

	// RawActionURL reads one external_actions row's url and current_url
	// columns as stored, keyed by dedup_key. Raw on purpose, like
	// ReferenceRows: the store's reads serve COALESCE(current_url, url), which
	// is exactly the layer that would hide either a rewritten history column
	// or an unfilled pointer. current_url is returned as "" when NULL.
	RawActionURL func(t *testing.T, dedupKey string) (url, currentURL string)

	// ReferenceRows returns every category-A repository reference the org
	// holds, as the rows actually store them: the registry row id each of
	// team_github_repos and conversation_worktrees points at, tagged by
	// table and sorted. It is deliberately raw — the
	// point is to observe the stored value rather than the slug a store read
	// would resolve it back to, because a rename that rewrote one of these
	// would still read back correctly and only this snapshot would notice.
	ReferenceRows func(t *testing.T) []string
}

// oldSlug/newSlug are the rename under test throughout. renameNeighbourSlug shares
// oldSlug as a string prefix and must never be touched: it is the whole reason
// the rewrite matches on a slug boundary instead of on a substring.
const (
	renameOldSlug       = "octo/api"
	renameNewSlug       = "octo/platform-api"
	renameNeighbourSlug = "octo/api-gateway"
)

// RunRepoRenameConformance covers the rename contract every backend impl must
// hold: the multi-table rewrite lands everywhere or nowhere, a second run is a
// no-op rather than an error, and the two states that look like a rename but
// are not — a deleted repository whose name a new one claimed, and a
// repository TF has no provider id for — never move a row.
func RunRepoRenameConformance(t *testing.T, mk RepoRenameFactory) {
	t.Helper()
	ctx := context.Background()

	t.Run("Rename_rewrites_every_stored_reference", func(t *testing.T) {
		s, orgID, seed := mk(t)
		fx := seedRenameFixture(t, s, orgID, seed, "rewrite")

		out, err := s.Repos.RenameSystem(ctx, orgID, domain.RepoRef{
			Owner: "octo", Repo: "platform-api", ExternalID: fx.externalID,
		})
		if err != nil {
			t.Fatalf("RenameSystem: %v", err)
		}
		if !out.Renamed || out.From != renameOldSlug || out.To != renameNewSlug {
			t.Fatalf("outcome = %+v, want a rename %s -> %s", out, renameOldSlug, renameNewSlug)
		}

		// The repository row itself.
		if got, _ := s.Repos.GetByRef(ctx, orgID, repoRef(renameOldSlug)); got != nil {
			t.Errorf("repository still resolves under the old slug: %+v", got)
		}
		moved, err := s.Repos.GetByRef(ctx, orgID, repoRef(renameNewSlug))
		if err != nil || moved == nil {
			t.Fatalf("Get(%s) = %v, %v; want the moved row", renameNewSlug, moved, err)
		}
		if moved.ExternalID != fx.externalID {
			t.Errorf("external id = %q, want %q — a rename moves the name, never the identity", moved.ExternalID, fx.externalID)
		}
		if moved.BaseBranch != "release" {
			t.Errorf("base branch = %q, want release — a rename must not drop what the row already held", moved.BaseBranch)
		}

		// The tracked set.
		tracked, err := s.TeamGitHubRepos.ListForTeamSystem(ctx, seed.TeamID)
		if err != nil {
			t.Fatalf("ListForTeamSystem: %v", err)
		}
		if got := trackedSlugs(tracked); !sameSet(got, []string{renameNewSlug, renameNeighbourSlug}) {
			t.Errorf("tracked set = %v, want [%s %s]", got, renameNewSlug, renameNeighbourSlug)
		}

		// The worktree ledger.
		worktrees, err := s.ConversationWorktrees.ListSystem(ctx, orgID, fx.conversationID)
		if err != nil {
			t.Fatalf("worktrees ListSystem: %v", err)
		}
		if len(worktrees) != 1 || worktrees[0].RepoID != renameNewSlug {
			t.Errorf("worktree ledger = %+v, want one row at %s", worktrees, renameNewSlug)
		}

		// The entity's composite source id — same row, new name.
		if stale, _ := s.Entities.GetBySourceSystem(ctx, orgID, "github", renameOldSlug+"#18"); stale != nil {
			t.Errorf("entity still resolves under the old source id: %+v", stale)
		}
		ent, err := s.Entities.GetBySourceSystem(ctx, orgID, "github", renameNewSlug+"#18")
		if err != nil || ent == nil {
			t.Fatalf("entity by new source id = %v, %v; want the same row", ent, err)
		}
		if ent.ID != fx.entityID {
			t.Errorf("entity id = %q, want %q — a rename renames, it never re-mints", ent.ID, fx.entityID)
		}
		// The neighbour shares the old slug as a prefix. If the rewrite had
		// matched on substring instead of on a boundary, this would have moved.
		if neighbour, _ := s.Entities.GetBySourceSystem(ctx, orgID, "github", renameNeighbourSlug+"#4"); neighbour == nil {
			t.Errorf("entity for %s was rewritten; the slug boundary is what stops that", renameNeighbourSlug)
		}

		// The artifacts: target and dedup key both move, and a branch key's
		// anchor — which repeats the repository name — does not.
		pr, err := s.Artifacts.Get(ctx, orgID, fx.prArtifactID)
		if err != nil || pr == nil {
			t.Fatalf("Artifacts.Get(pr): %v, %v", pr, err)
		}
		if pr.Target != renameNewSlug+"#18" {
			t.Errorf("pr target = %q, want %s#18", pr.Target, renameNewSlug)
		}
		if want := domain.PullRequestDedupKey(renameNewSlug, 18); pr.DedupKey != want {
			t.Errorf("pr dedup key = %q, want %q", pr.DedupKey, want)
		}
		branch, err := s.Artifacts.Get(ctx, orgID, fx.branchArtifactID)
		if err != nil || branch == nil {
			t.Fatalf("Artifacts.Get(branch): %v, %v", branch, err)
		}
		if branch.Target != renameNewSlug {
			t.Errorf("branch target = %q, want %s", branch.Target, renameNewSlug)
		}
		if want := domain.ArtifactDedupKey(domain.ArtifactProviderGit, domain.ArtifactKindBranch, renameNewSlug, fx.branchRef); branch.DedupKey != want {
			t.Errorf("branch dedup key = %q, want %q — the anchor repeats the repo name and must not move", branch.DedupKey, want)
		}
		neighbourArtifact, err := s.Artifacts.Get(ctx, orgID, fx.neighbourArtifactID)
		if err != nil || neighbourArtifact == nil {
			t.Fatalf("Artifacts.Get(neighbour): %v, %v", neighbourArtifact, err)
		}
		if neighbourArtifact.Target != renameNeighbourSlug+"#4" {
			t.Errorf("neighbour artifact target = %q, want it untouched", neighbourArtifact.Target)
		}

		// The placement override — human intent, keyed by slug.
		if stale, _ := s.PlacementOverrides.Get(ctx, orgID, domain.PlacementKindRepo, renameOldSlug); stale != nil {
			t.Errorf("placement override still keyed on the old slug: %+v", stale)
		}
		ov, err := s.PlacementOverrides.Get(ctx, orgID, domain.PlacementKindRepo, renameNewSlug)
		if err != nil || ov == nil {
			t.Fatalf("PlacementOverrides.Get(new) = %v, %v; want the moved pin", ov, err)
		}
		if ov.Replicas != 3 {
			t.Errorf("placement replicas = %d, want 3 — the pin moves, its content does not", ov.Replicas)
		}
		if other, _ := s.PlacementOverrides.Get(ctx, orgID, "hostgroup", renameOldSlug); other == nil {
			t.Errorf("the other-kind override moved; only key_kind='repo' rows hold a slug")
		}
	})

	t.Run("Rename_moves_the_stored_links_and_freezes_the_record", func(t *testing.T) {
		// The stored-link half of the rewrite. An entity's URL and the audit
		// ledger's rendered link must resolve under the new slug — the GitHub
		// redirect they used to lean on stops the moment the freed name is
		// re-claimed by a different repository — while the ledger's RECORD of
		// each act stays byte-identical: target keeps naming what the act was
		// performed against, and the url column keeps the link as captured.
		s, orgID, seed := mk(t)
		fx := seedRenameFixture(t, s, orgID, seed, "links")

		movedBefore := findActionByKey(t, s, orgID, fx.movedActionKey)
		capturedURL := "https://github.com/" + renameOldSlug + "/pull/18"

		if _, err := s.Repos.RenameSystem(ctx, orgID, domain.RepoRef{
			Owner: "octo", Repo: "platform-api", ExternalID: fx.externalID,
		}); err != nil {
			t.Fatalf("RenameSystem: %v", err)
		}

		// The entity's link resolves under the new slug; the neighbour's —
		// which shares the old slug as a string prefix, in the URL too — is
		// untouched.
		ent, err := s.Entities.GetBySourceSystem(ctx, orgID, "github", renameNewSlug+"#18")
		if err != nil || ent == nil {
			t.Fatalf("entity by new source id = %v, %v", ent, err)
		}
		if want := "https://github.com/" + renameNewSlug + "/pull/18"; ent.URL != want {
			t.Errorf("entity url = %q, want %q", ent.URL, want)
		}
		neighbour, _ := s.Entities.GetBySourceSystem(ctx, orgID, "github", renameNeighbourSlug+"#4")
		if neighbour == nil || neighbour.URL != "https://github.com/"+renameNeighbourSlug+"/pull/4" {
			t.Errorf("neighbour entity = %+v, want its url untouched", neighbour)
		}

		// The ledger row's rendered link resolves under the new slug while
		// every record column still reads exactly as it was written.
		moved := findActionByKey(t, s, orgID, fx.movedActionKey)
		if want := "https://github.com/" + renameNewSlug + "/pull/18"; moved.URL != want {
			t.Errorf("action url = %q, want %q", moved.URL, want)
		}
		if moved.Target != renameOldSlug+"#18" {
			t.Errorf("action target = %q, want %s#18 — the record names what the act was performed against", moved.Target, renameOldSlug)
		}
		if moved.Action != movedBefore.Action || moved.DetailJSON != movedBefore.DetailJSON ||
			moved.Credential != movedBefore.Credential || !moved.OccurredAt.Equal(movedBefore.OccurredAt) {
			t.Errorf("record columns moved across the rename:\n before = %+v\n after  = %+v", movedBefore, moved)
		}
		if n := findActionByKey(t, s, orgID, fx.neighbourActionKey); n.URL != "https://github.com/"+renameNeighbourSlug+"/pull/4" {
			t.Errorf("neighbour action url = %q, want it untouched", n.URL)
		}

		// The columns as stored: url is the captured link, byte-identical;
		// current_url is the maintained pointer; the neighbour's pointer was
		// never filled.
		rawURL, rawCurrent := seed.RawActionURL(t, fx.movedActionKey)
		if rawURL != capturedURL {
			t.Errorf("stored url column = %q, want the captured %q — history is frozen", rawURL, capturedURL)
		}
		if want := "https://github.com/" + renameNewSlug + "/pull/18"; rawCurrent != want {
			t.Errorf("stored current_url = %q, want %q", rawCurrent, want)
		}
		if _, rawCurrent := seed.RawActionURL(t, fx.neighbourActionKey); rawCurrent != "" {
			t.Errorf("neighbour current_url = %q, want NULL — its object never moved", rawCurrent)
		}

		// A second rename chains off the pointer, not the captured link: the
		// served URL follows the object again while the history column still
		// holds the original capture.
		if _, err := s.Repos.RenameSystem(ctx, orgID, domain.RepoRef{
			Owner: "octo", Repo: "platform-api-v2", ExternalID: fx.externalID,
		}); err != nil {
			t.Fatalf("second RenameSystem: %v", err)
		}
		if got := findActionByKey(t, s, orgID, fx.movedActionKey); got.URL != "https://github.com/octo/platform-api-v2/pull/18" {
			t.Errorf("action url after the second rename = %q, want it chained to octo/platform-api-v2", got.URL)
		}
		if rawURL, _ := seed.RawActionURL(t, fx.movedActionKey); rawURL != capturedURL {
			t.Errorf("stored url column = %q after two renames, want the captured %q", rawURL, capturedURL)
		}
	})

	t.Run("Rename_is_idempotent", func(t *testing.T) {
		// The second detection of the same rename — a retry, a second poller,
		// the next cycle — finds the slug already current. That is a no-op and
		// a nil error, not a failure: losing is terminal and needs no retry.
		s, orgID, seed := mk(t)
		fx := seedRenameFixture(t, s, orgID, seed, "idempotent")
		observed := domain.RepoRef{Owner: "octo", Repo: "platform-api", ExternalID: fx.externalID}

		if _, err := s.Repos.RenameSystem(ctx, orgID, observed); err != nil {
			t.Fatalf("first RenameSystem: %v", err)
		}
		out, err := s.Repos.RenameSystem(ctx, orgID, observed)
		if err != nil {
			t.Fatalf("second RenameSystem: %v — a re-run must be a no-op, not an error", err)
		}
		if out.Renamed {
			t.Errorf("second run reported %+v, want no rename", out)
		}
		if got, _ := s.Repos.GetByRef(ctx, orgID, repoRef(renameNewSlug)); got == nil {
			t.Errorf("the repository moved away on the second run")
		}
		ent, _ := s.Entities.GetBySourceSystem(ctx, orgID, "github", renameNewSlug+"#18")
		if ent == nil {
			t.Errorf("the entity's source id was rewritten twice")
		}
	})

	t.Run("Casing_only_change_is_not_a_rename", func(t *testing.T) {
		// GitHub identifiers are case-insensitive and stored casing is sticky,
		// so a repository respelled in different case is the same slug. Moving
		// every reference in the product to say the same thing differently is
		// not a rename; it is churn.
		s, orgID, seed := mk(t)
		fx := seedRenameFixture(t, s, orgID, seed, "casing")

		out, err := s.Repos.RenameSystem(ctx, orgID, domain.RepoRef{
			Owner: "Octo", Repo: "API", ExternalID: fx.externalID,
		})
		if err != nil {
			t.Fatalf("RenameSystem: %v", err)
		}
		if out.Renamed {
			t.Errorf("outcome = %+v, want no rename for a casing-only change", out)
		}
		if got, _ := s.Repos.GetByRef(ctx, orgID, repoRef(renameOldSlug)); got == nil || got.Slug() != renameOldSlug {
			t.Errorf("stored casing changed to %+v; it is sticky", got)
		}
	})

	t.Run("Delete_and_recreate_under_the_same_name_is_not_a_rename", func(t *testing.T) {
		// The case that corrupts data if detection keys on the wrong thing.
		// The repository TF tracks is gone; a DIFFERENT repository now answers
		// to its name. The provider id differs, so the rename condition never
		// holds — and nothing may be rewritten onto the impostor.
		s, orgID, seed := mk(t)
		fx := seedRenameFixture(t, s, orgID, seed, "recreate")

		out, err := s.Repos.RenameSystem(ctx, orgID, domain.RepoRef{
			Owner: "octo", Repo: "api", ExternalID: "999000111",
		})
		if err != nil {
			t.Fatalf("RenameSystem: %v", err)
		}
		if out.Renamed {
			t.Fatalf("outcome = %+v, want no rename: a new repository under a freed name is not the old one", out)
		}
		if got, _ := s.Repos.GetByRef(ctx, orgID, repoRef(renameOldSlug)); got == nil || got.ExternalID != fx.externalID {
			t.Errorf("repository row = %+v, want the stored identity untouched", got)
		}
		if ent, _ := s.Entities.GetBySourceSystem(ctx, orgID, "github", renameOldSlug+"#18"); ent == nil {
			t.Errorf("the entity was rewritten; nothing about this observation is a rename")
		}

		// The same conclusion from the detection half, which is what the
		// pollers actually run before they ever call the store.
		stored := []domain.RepoRef{{Source: "github", Owner: "octo", Repo: "api", ExternalID: fx.externalID}}
		observed := []domain.RepoRef{{Source: "github", Owner: "octo", Repo: "api", ExternalID: "999000111"}}
		if got := domain.DetectRepoRenames(stored, observed); len(got) != 0 {
			t.Errorf("DetectRepoRenames = %+v, want none", got)
		}
	})

	t.Run("A_repository_with_no_external_id_is_never_renamed", func(t *testing.T) {
		s, orgID, seed := mk(t)
		_ = seed
		if _, err := s.Repos.GetOrCreateSystem(ctx, orgID, domain.RepoRef{Owner: "octo", Repo: "unidentified"}); err != nil {
			t.Fatalf("seed id-less repository: %v", err)
		}

		// No id on the observation: nothing to key on, so nothing happens.
		out, err := s.Repos.RenameSystem(ctx, orgID, domain.RepoRef{Owner: "octo", Repo: "renamed"})
		if err != nil {
			t.Fatalf("RenameSystem with no observed id: %v", err)
		}
		if out.Renamed {
			t.Errorf("outcome = %+v, want no rename without an observed id", out)
		}

		// An id on the observation, none on the row: the row is not reachable
		// by identity, so it is not renamable in this direction either.
		out, err = s.Repos.RenameSystem(ctx, orgID, domain.RepoRef{
			Owner: "octo", Repo: "renamed", ExternalID: "1296269",
		})
		if err != nil {
			t.Fatalf("RenameSystem against an id-less row: %v", err)
		}
		if out.Renamed {
			t.Errorf("outcome = %+v, want no rename against an id-less row", out)
		}
		if got, _ := s.Repos.GetByRef(ctx, orgID, repoRef("octo/unidentified")); got == nil {
			t.Errorf("the id-less row moved")
		}
	})

	t.Run("Rename_creates_closes_and_re_mints_nothing", func(t *testing.T) {
		// Forward-only tracking is unaffected by a rename: the same tasks
		// continue to exist against the same entities, now correctly named.
		s, orgID, seed := mk(t)
		fx := seedRenameFixture(t, s, orgID, seed, "no-task-churn")
		seed.Task(t, fx.entityID, "no-task-churn")

		before, err := s.Entities.ListActiveSystem(ctx, orgID, "github")
		if err != nil {
			t.Fatalf("ListActiveSystem: %v", err)
		}
		tasksBefore := seed.CountTasks(t)

		if _, err := s.Repos.RenameSystem(ctx, orgID, domain.RepoRef{
			Owner: "octo", Repo: "platform-api", ExternalID: fx.externalID,
		}); err != nil {
			t.Fatalf("RenameSystem: %v", err)
		}

		after, err := s.Entities.ListActiveSystem(ctx, orgID, "github")
		if err != nil {
			t.Fatalf("ListActiveSystem after: %v", err)
		}
		if len(after) != len(before) {
			t.Errorf("active entities went %d -> %d; a rename creates and closes none", len(before), len(after))
		}
		if got := seed.CountTasks(t); got != tasksBefore {
			t.Errorf("tasks went %d -> %d; a rename mints and retires none", tasksBefore, got)
		}
		if tasksBefore != 1 {
			t.Fatalf("fixture seeded %d tasks, want 1 — the count above has to be able to move", tasksBefore)
		}
	})

	t.Run("Rename_refuses_when_another_repository_holds_the_target_slug", func(t *testing.T) {
		// Two repositories cannot share a name upstream, so the occupant is a
		// row for a repository GitHub no longer has. Moving onto it would
		// collide with the identity index, and deleting it is not this
		// operation's call — so the attempt is refused, terminally.
		s, orgID, seed := mk(t)
		fx := seedRenameFixture(t, s, orgID, seed, "occupied")
		if _, err := s.Repos.GetOrCreateSystem(ctx, orgID, domain.RepoRef{
			Owner: "octo", Repo: "platform-api", ExternalID: "555000555",
		}); err != nil {
			t.Fatalf("seed occupant: %v", err)
		}

		_, err := s.Repos.RenameSystem(ctx, orgID, domain.RepoRef{
			Owner: "octo", Repo: "platform-api", ExternalID: fx.externalID,
		})
		if !errors.Is(err, db.ErrRepoSlugOccupied) {
			t.Fatalf("err = %v, want ErrRepoSlugOccupied", err)
		}
		// All tables or none: the refusal left every reference where it was.
		if got, _ := s.Repos.GetByRef(ctx, orgID, repoRef(renameOldSlug)); got == nil {
			t.Errorf("the repository row moved despite the refusal")
		}
		if ent, _ := s.Entities.GetBySourceSystem(ctx, orgID, "github", renameOldSlug+"#18"); ent == nil {
			t.Errorf("the entity's source id moved despite the refusal")
		}
		tracked, _ := s.TeamGitHubRepos.ListForTeamSystem(ctx, seed.TeamID)
		if !sameSet(trackedSlugs(tracked), []string{renameOldSlug, renameNeighbourSlug}) {
			t.Errorf("the tracked set moved despite the refusal: %v", trackedSlugs(tracked))
		}
	})

	t.Run("Rename_refuses_when_a_durable_entity_already_answers_to_the_target", func(t *testing.T) {
		// The reference tables absorb an orphan; entities and artifacts must
		// NOT. An entity carries tasks, conversations and memory, and an
		// artifact is the audit record of a real external object — deleting
		// either to make room for a rename destroys what forward-only tracking
		// promises to keep.
		//
		// This is ordinary product state, not corruption: untracking a
		// repository drops its repositories row and deliberately keeps its
		// entities, so the repository-row guard cannot see that the name is
		// still spoken for. Refusing is the answer, classified so the caller
		// treats it as terminal rather than retrying it every poll cycle.
		s, orgID, seed := mk(t)
		_ = seed

		// A tracked repository with a PR, then untracked. The entity survives.
		if err := s.Repos.SetConfigured(ctx, orgID, []string{"octo/api"}); err != nil {
			t.Fatalf("track the first repository: %v", err)
		}
		if _, err := s.Repos.GetOrCreateSystem(ctx, orgID, domain.RepoRef{
			Owner: "octo", Repo: "api", ExternalID: "1",
		}); err != nil {
			t.Fatalf("seed the first repository's identity: %v", err)
		}
		orphan, _, err := s.Entities.FindOrCreateSystem(ctx, orgID, "github", "octo/api#18", "pr", "the untracked repo's PR", "")
		if err != nil {
			t.Fatalf("seed the orphaned entity: %v", err)
		}
		if err := s.Repos.SetConfigured(ctx, orgID, []string{"octo/legacy"}); err != nil {
			t.Fatalf("untrack it: %v", err)
		}
		if got, _ := s.Entities.GetBySourceSystem(ctx, orgID, "github", "octo/api#18"); got == nil {
			t.Fatal("untracking deleted the entity; forward-only tracking says it is durable")
		}

		// A second, live repository with a PR of the same number, which GitHub
		// now renames onto the freed name.
		if _, err := s.Repos.GetOrCreateSystem(ctx, orgID, domain.RepoRef{
			Owner: "octo", Repo: "legacy", ExternalID: "2",
		}); err != nil {
			t.Fatalf("seed the renaming repository: %v", err)
		}
		if _, _, err := s.Entities.FindOrCreateSystem(ctx, orgID, "github", "octo/legacy#18", "pr", "the live repo's PR", ""); err != nil {
			t.Fatalf("seed the live entity: %v", err)
		}

		_, err = s.Repos.RenameSystem(ctx, orgID, domain.RepoRef{
			Owner: "octo", Repo: "api", ExternalID: "2",
		})
		if !errors.Is(err, db.ErrRepoSlugOccupied) {
			t.Fatalf("err = %v, want ErrRepoSlugOccupied — a raw constraint violation reads as retryable", err)
		}

		// All tables or none: the refusal rolled the whole rewrite back.
		if got, _ := s.Repos.GetByRef(ctx, orgID, repoRef("octo/legacy")); got == nil {
			t.Error("the repository row moved despite the refusal")
		}
		if got, _ := s.Entities.GetBySourceSystem(ctx, orgID, "github", "octo/legacy#18"); got == nil {
			t.Error("the live entity's source id moved despite the refusal")
		}
		kept, _ := s.Entities.GetBySourceSystem(ctx, orgID, "github", "octo/api#18")
		if kept == nil || kept.ID != orphan.ID {
			t.Errorf("the orphaned entity = %+v, want it untouched — a rename never destroys durable work", kept)
		}
	})

	t.Run("Rename_refuses_when_a_durable_artifact_already_answers_to_the_target", func(t *testing.T) {
		// The artifact half of the case above. It is a separate statement
		// rather than the same one: the collision lands on a different unique
		// index (artifacts.dedup_key, not entities.source_id) in a different
		// function, and the entity case cannot reach it — that rewrite fails
		// first. Seeded with no entities at all so the refusal is provably the
		// artifact index talking.
		s, orgID, seed := mk(t)

		if err := s.Repos.SetConfigured(ctx, orgID, []string{"octo/api"}); err != nil {
			t.Fatalf("track the first repository: %v", err)
		}
		if _, err := s.Repos.GetOrCreateSystem(ctx, orgID, domain.RepoRef{
			Owner: "octo", Repo: "api", ExternalID: "1",
		}); err != nil {
			t.Fatalf("seed the first repository's identity: %v", err)
		}
		orphan, err := s.Artifacts.UpsertSystem(ctx, orgID, domain.Artifact{
			TeamID: seed.TeamID, Provider: domain.ArtifactProviderGitHub, Kind: domain.ArtifactKindPullRequest,
			Target: domain.PullRequestTarget("octo/api", 18), ExternalID: "18",
			State: domain.ArtifactStatePROpen, DedupKey: domain.PullRequestDedupKey("octo/api", 18),
		})
		if err != nil {
			t.Fatalf("seed the orphaned artifact: %v", err)
		}
		if err := s.Repos.SetConfigured(ctx, orgID, []string{"octo/legacy"}); err != nil {
			t.Fatalf("untrack it: %v", err)
		}

		// The live repository whose PR of the same number would collide.
		if _, err := s.Repos.GetOrCreateSystem(ctx, orgID, domain.RepoRef{
			Owner: "octo", Repo: "legacy", ExternalID: "2",
		}); err != nil {
			t.Fatalf("seed the renaming repository: %v", err)
		}
		live, err := s.Artifacts.UpsertSystem(ctx, orgID, domain.Artifact{
			TeamID: seed.TeamID, Provider: domain.ArtifactProviderGitHub, Kind: domain.ArtifactKindPullRequest,
			Target: domain.PullRequestTarget("octo/legacy", 18), ExternalID: "18",
			State: domain.ArtifactStatePROpen, DedupKey: domain.PullRequestDedupKey("octo/legacy", 18),
		})
		if err != nil {
			t.Fatalf("seed the live artifact: %v", err)
		}

		_, err = s.Repos.RenameSystem(ctx, orgID, domain.RepoRef{
			Owner: "octo", Repo: "api", ExternalID: "2",
		})
		if !errors.Is(err, db.ErrRepoSlugOccupied) {
			t.Fatalf("err = %v, want ErrRepoSlugOccupied — a raw constraint violation reads as retryable", err)
		}

		// All tables or none.
		if got, _ := s.Repos.GetByRef(ctx, orgID, repoRef("octo/legacy")); got == nil {
			t.Error("the repository row moved despite the refusal")
		}
		if got, _ := s.Artifacts.Get(ctx, orgID, live.ID); got == nil ||
			got.DedupKey != domain.PullRequestDedupKey("octo/legacy", 18) {
			t.Errorf("the live artifact = %+v, want its dedup key unmoved", got)
		}
		if got, _ := s.Artifacts.Get(ctx, orgID, orphan.ID); got == nil ||
			got.DedupKey != domain.PullRequestDedupKey("octo/api", 18) {
			t.Errorf("the orphaned artifact = %+v, want it untouched — a rename never destroys an audit record", got)
		}
	})

	t.Run("Rename_absorbs_an_orphaned_placement_pin_on_the_target_slug", func(t *testing.T) {
		// A placement pin is a bare string an admin typed into a polymorphic
		// column, so it can name a repository nothing else has a row for. Such
		// a pin collides with the move on the override's own key, and the
		// repository-row guard above has already proved it is an orphan, so
		// the move absorbs it. The alternative is a rename that fails forever
		// over a row nothing points at.
		//
		// This case is down to placement pins alone. The tracked set and the
		// worktree ledger reference the registry row by id now, so "a
		// reference to a repository with no row" is not a state either can
		// reach — the foreign key is what refuses it, rather than a rewrite
		// having to clean up after it.
		s, orgID, seed := mk(t)
		fx := seedRenameFixture(t, s, orgID, seed, "orphan")

		if _, err := s.PlacementOverrides.Upsert(ctx, domain.PlacementOverride{
			OrgID: orgID, KeyKind: domain.PlacementKindRepo, KeyValue: renameNewSlug, Replicas: 9,
		}); err != nil {
			t.Fatalf("seed target placement override: %v", err)
		}

		out, err := s.Repos.RenameSystem(ctx, orgID, domain.RepoRef{
			Owner: "octo", Repo: "platform-api", ExternalID: fx.externalID,
		})
		if err != nil {
			t.Fatalf("RenameSystem: %v", err)
		}
		if !out.Renamed {
			t.Fatalf("outcome = %+v, want the rename to go through", out)
		}

		ov, err := s.PlacementOverrides.Get(ctx, orgID, domain.PlacementKindRepo, renameNewSlug)
		if err != nil || ov == nil {
			t.Fatalf("PlacementOverrides.Get(new) = %v, %v; want the moved pin", ov, err)
		}
		if ov.Replicas != 3 {
			t.Errorf("placement replicas = %d, want 3 — the LIVE repository's pin wins, not the orphan's", ov.Replicas)
		}
	})

	t.Run("Rename_rewrites_no_row_that_references_the_repository_by_id", func(t *testing.T) {
		// The inverse of the rewrite case above, and the proof the conversion
		// did its job. Every reference in category A — the tracked set and the
		// worktree ledger — points at the registry row's id, so a rename must
		// move the slug on that one row and leave both byte-identical. If
		// either still stored a slug, the snapshot would differ and this
		// would fail.
		s, orgID, seed := mk(t)
		fx := seedRenameFixture(t, s, orgID, seed, "byid")

		before := seed.ReferenceRows(t)
		if len(before) == 0 {
			t.Fatal("the fixture produced no repository references to snapshot")
		}

		out, err := s.Repos.RenameSystem(ctx, orgID, domain.RepoRef{
			Owner: "octo", Repo: "platform-api", ExternalID: fx.externalID,
		})
		if err != nil {
			t.Fatalf("RenameSystem: %v", err)
		}
		if !out.Renamed {
			t.Fatalf("outcome = %+v, want the rename to go through", out)
		}

		after := seed.ReferenceRows(t)
		if !equalStringSlice(before, after) {
			t.Errorf("references changed across the rename:\n before = %v\n after  = %v\nnothing that references a repository by id may move", before, after)
		}

		// And the references still resolve — to the new name, without having
		// been touched.
		tracked, err := s.TeamGitHubRepos.ListForTeamSystem(ctx, seed.TeamID)
		if err != nil {
			t.Fatalf("ListForTeamSystem: %v", err)
		}
		if got := trackedSlugs(tracked); !sameSet(got, []string{renameNewSlug, renameNeighbourSlug}) {
			t.Errorf("tracked set = %v, want it resolving to the new slug unrewritten", got)
		}
	})

	t.Run("Rename_of_a_repository_with_no_row_is_a_no_op", func(t *testing.T) {
		s, orgID, _ := mk(t)
		out, err := s.Repos.RenameSystem(ctx, orgID, domain.RepoRef{
			Owner: "ghost", Repo: "repo", ExternalID: "42",
		})
		if err != nil {
			t.Fatalf("RenameSystem: %v", err)
		}
		if out.Renamed {
			t.Errorf("outcome = %+v, want no rename", out)
		}
		if got, _ := s.Repos.GetByRef(ctx, orgID, repoRef("ghost/repo")); got != nil {
			t.Errorf("a rename created a repository row: %+v — it moves rows, it never mints one", got)
		}
	})

	t.Run("ListIdentities_returns_only_rows_that_carry_an_id", func(t *testing.T) {
		s, orgID, _ := mk(t)
		if _, err := s.Repos.GetOrCreateSystem(ctx, orgID, domain.RepoRef{
			Owner: "octo", Repo: "identified", ExternalID: "1296269",
		}); err != nil {
			t.Fatalf("seed identified: %v", err)
		}
		if _, err := s.Repos.GetOrCreateSystem(ctx, orgID, domain.RepoRef{Owner: "octo", Repo: "bare"}); err != nil {
			t.Fatalf("seed bare: %v", err)
		}

		got, err := s.Repos.ListIdentitiesSystem(ctx, orgID)
		if err != nil {
			t.Fatalf("ListIdentitiesSystem: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("identities = %+v, want only the row carrying an id", got)
		}
		if got[0].Slug() != "octo/identified" || got[0].ExternalID != "1296269" || got[0].Source != domain.RepoSourceGitHub {
			t.Errorf("identity = %+v, want octo/identified/github/1296269", got[0])
		}
	})
}

// renameFixture holds the ids the assertions read back by.
type renameFixture struct {
	externalID          string
	conversationID      string
	entityID            string
	prArtifactID        string
	branchArtifactID    string
	neighbourArtifactID string
	branchRef           string
	movedActionKey      string
	neighbourActionKey  string
}

// seedRenameFixture stages one repository referenced from every table that
// stores a slug, plus a neighbour repository whose slug has the first as a
// string prefix. The neighbour is the control: every assertion that it did not
// move is an assertion that the rewrite matched a repository rather than a
// substring.
func seedRenameFixture(t *testing.T, s db.Stores, orgID string, seed RepoRenameSeeder, suffix string) renameFixture {
	t.Helper()
	ctx := context.Background()
	const externalID = "1296269"
	branchRef := "refs/heads/octo/api-fix"

	if err := s.TeamGitHubRepos.ReplaceForTeam(ctx, orgID, seed.TeamID, []domain.TeamGitHubRepo{
		{Owner: "octo", Repo: "api"},
		{Owner: "octo", Repo: "api-gateway"},
	}); err != nil {
		t.Fatalf("seed tracked set: %v", err)
	}
	if _, err := s.Repos.GetOrCreateSystem(ctx, orgID, domain.RepoRef{
		Owner: "octo", Repo: "api", ExternalID: externalID,
	}); err != nil {
		t.Fatalf("seed repository identity: %v", err)
	}
	if err := setBaseBranch(ctx, s.Repos, orgID, renameOldSlug, "release"); err != nil {
		t.Fatalf("seed base branch: %v", err)
	}

	conversationID := seed.Conversation(t, suffix)
	if _, _, err := s.ConversationWorktrees.Insert(ctx, orgID, domain.ConversationWorktree{
		ConversationID: conversationID, RepoID: renameOldSlug, Ref: "pr-18",
		Path: "/tmp/wt/" + conversationID + "/octo/api/pr-18",
	}); err != nil {
		t.Fatalf("seed worktree: %v", err)
	}

	entity, _, err := s.Entities.FindOrCreateSystem(ctx, orgID, "github", renameOldSlug+"#18", "pr", "A pull request",
		"https://github.com/"+renameOldSlug+"/pull/18")
	if err != nil {
		t.Fatalf("seed entity: %v", err)
	}
	if _, _, err := s.Entities.FindOrCreateSystem(ctx, orgID, "github", renameNeighbourSlug+"#4", "pr", "Neighbour PR",
		"https://github.com/"+renameNeighbourSlug+"/pull/4"); err != nil {
		t.Fatalf("seed neighbour entity: %v", err)
	}

	// Two ledger rows: one whose object the rename moves, one for the
	// neighbour as the boundary control. Deterministic dedup keys so the
	// assertions (and the raw column reads) can find them again.
	movedActionKey := "rename-moved-" + suffix
	neighbourActionKey := "rename-neighbour-" + suffix
	if err := s.ExternalActions.RecordSystem(ctx, orgID, domain.ExternalAction{
		TeamID: seed.TeamID, Provider: "github", Action: domain.ActionPRCreated,
		Target: renameOldSlug + "#18", ExternalID: "18",
		URL:        "https://github.com/" + renameOldSlug + "/pull/18",
		Credential: domain.CredentialGitHubApp, DedupKey: movedActionKey,
		DetailJSON: `{"method":"POST","path":"/repos/` + renameOldSlug + `/pulls"}`,
	}); err != nil {
		t.Fatalf("seed moved action: %v", err)
	}
	if err := s.ExternalActions.RecordSystem(ctx, orgID, domain.ExternalAction{
		TeamID: seed.TeamID, Provider: "github", Action: domain.ActionPRCreated,
		Target: renameNeighbourSlug + "#4", ExternalID: "4",
		URL:        "https://github.com/" + renameNeighbourSlug + "/pull/4",
		Credential: domain.CredentialGitHubApp, DedupKey: neighbourActionKey,
	}); err != nil {
		t.Fatalf("seed neighbour action: %v", err)
	}

	prArtifact, err := s.Artifacts.UpsertSystem(ctx, orgID, domain.Artifact{
		TeamID: seed.TeamID, Provider: domain.ArtifactProviderGitHub, Kind: domain.ArtifactKindPullRequest,
		Target: domain.PullRequestTarget(renameOldSlug, 18), ExternalID: "18",
		State: domain.ArtifactStatePROpen, DedupKey: domain.PullRequestDedupKey(renameOldSlug, 18),
	})
	if err != nil {
		t.Fatalf("seed pr artifact: %v", err)
	}
	branchArtifact, err := s.Artifacts.UpsertSystem(ctx, orgID, domain.Artifact{
		TeamID: seed.TeamID, Provider: domain.ArtifactProviderGit, Kind: domain.ArtifactKindBranch,
		Target: renameOldSlug, ExternalID: branchRef, State: domain.ArtifactStateBranchPushed,
		DedupKey: domain.ArtifactDedupKey(domain.ArtifactProviderGit, domain.ArtifactKindBranch, renameOldSlug, branchRef),
	})
	if err != nil {
		t.Fatalf("seed branch artifact: %v", err)
	}
	neighbourArtifact, err := s.Artifacts.UpsertSystem(ctx, orgID, domain.Artifact{
		TeamID: seed.TeamID, Provider: domain.ArtifactProviderGitHub, Kind: domain.ArtifactKindPullRequest,
		Target: domain.PullRequestTarget(renameNeighbourSlug, 4), ExternalID: "4",
		State: domain.ArtifactStatePROpen, DedupKey: domain.PullRequestDedupKey(renameNeighbourSlug, 4),
	})
	if err != nil {
		t.Fatalf("seed neighbour artifact: %v", err)
	}

	if _, err := s.PlacementOverrides.Upsert(ctx, domain.PlacementOverride{
		OrgID: orgID, KeyKind: domain.PlacementKindRepo, KeyValue: renameOldSlug, Replicas: 3,
	}); err != nil {
		t.Fatalf("seed placement override: %v", err)
	}
	// key_kind is a free-text column (no FK/CHECK), so an arbitrary other kind
	// sharing the same key_value column proves the rename only rewrites
	// key_kind='repo' rows.
	if _, err := s.PlacementOverrides.Upsert(ctx, domain.PlacementOverride{
		OrgID: orgID, KeyKind: "hostgroup", KeyValue: renameOldSlug, Replicas: 2,
	}); err != nil {
		t.Fatalf("seed other-kind placement override: %v", err)
	}

	return renameFixture{
		externalID:          externalID,
		conversationID:      conversationID,
		entityID:            entity.ID,
		prArtifactID:        prArtifact.ID,
		branchArtifactID:    branchArtifact.ID,
		neighbourArtifactID: neighbourArtifact.ID,
		branchRef:           branchRef,
		movedActionKey:      movedActionKey,
		neighbourActionKey:  neighbourActionKey,
	}
}

// findActionByKey resolves one ledger row through the store's own read — the
// URL it returns is the served (coalesced) pointer, which is the value the
// action feed renders.
func findActionByKey(t *testing.T, s db.Stores, orgID, dedupKey string) domain.ExternalAction {
	t.Helper()
	// Unwindowed (zero Limit) and the total discarded: this scans for one known
	// dedup key rather than rendering a page.
	actions, _, err := s.ExternalActions.ListByOrgSystem(context.Background(), orgID, domain.ExternalActionListOpts{})
	if err != nil {
		t.Fatalf("ListByOrgSystem: %v", err)
	}
	for _, a := range actions {
		if a.DedupKey == dedupKey {
			return a
		}
	}
	t.Fatalf("no external action with dedup key %q", dedupKey)
	return domain.ExternalAction{}
}

func trackedSlugs(repos []domain.TeamGitHubRepo) []string {
	out := make([]string, 0, len(repos))
	for _, r := range repos {
		out = append(out, r.Owner+"/"+r.Repo)
	}
	return out
}

func sameSet(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	g := append([]string(nil), got...)
	w := append([]string(nil), want...)
	sort.Strings(g)
	sort.Strings(w)
	for i := range g {
		if g[i] != w[i] {
			return false
		}
	}
	return true
}
