package dbtest

import (
	"context"
	"errors"
	"fmt"
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

	// renamePRNodeID and renameNeighbourPRNodeID are the GraphQL node ids of
	// the fixture's pull requests, octo/api#18 and octo/api-gateway#4.
	renamePRNodeID          = "PR_kwDOrename18"
	renameNeighbourPRNodeID = "PR_kwDOneighbour4"
)

// RunRepoRenameConformance covers the rename contract every backend impl must
// hold: the multi-table rewrite lands everywhere or nowhere, a second run is a
// no-op rather than an error, and the two states that look like a rename but
// are not — a deleted repository whose name a new one claimed, and a
// repository TF has no provider id for — never move a row. A rename stays on
// its GitHub host: a same-named repository, entity or linked record on another
// host is never touched.
func RunRepoRenameConformance(t *testing.T, mk RepoRenameFactory) {
	t.Helper()
	ctx := context.Background()

	t.Run("Rename_rewrites_every_stored_reference", func(t *testing.T) {
		s, orgID, seed := mk(t)
		fx := seedRenameFixture(t, s, orgID, seed, "rewrite")

		out, err := s.Repos.RenameSystem(ctx, orgID, domain.RepoRef{
			Host:  TestGitHubHost,
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
		tracked, err := s.TeamGitHubRepos.ListForTeamSystem(ctx, seed.TeamID, TestGitHubHost)
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
		if stale, _ := s.Entities.GetBySourceSystem(ctx, orgID, "github", "https://github.com", renameOldSlug+"#18"); stale != nil {
			t.Errorf("entity still resolves under the old source id: %+v", stale)
		}
		ent, err := s.Entities.GetBySourceSystem(ctx, orgID, "github", "https://github.com", renameNewSlug+"#18")
		if err != nil || ent == nil {
			t.Fatalf("entity by new source id = %v, %v; want the same row", ent, err)
		}
		if ent.ID != fx.entityID {
			t.Errorf("entity id = %q, want %q — a rename renames, it never re-mints", ent.ID, fx.entityID)
		}
		// The neighbour shares the old slug as a prefix. If the rewrite had
		// matched on substring instead of on a boundary, this would have moved.
		if neighbour, _ := s.Entities.GetBySourceSystem(ctx, orgID, "github", "https://github.com", renameNeighbourSlug+"#4"); neighbour == nil {
			t.Errorf("entity for %s was rewritten; the slug boundary is what stops that", renameNeighbourSlug)
		}

		// The artifacts: the target moves, and the dedup key, which names
		// the pull request's node id or the repository's row id rather than
		// the name, does not.
		pr, err := s.Artifacts.Get(ctx, orgID, fx.prArtifactID)
		if err != nil || pr == nil {
			t.Fatalf("Artifacts.Get(pr): %v, %v", pr, err)
		}
		if pr.Target != renameNewSlug+"#18" {
			t.Errorf("pr target = %q, want %s#18", pr.Target, renameNewSlug)
		}
		if want := domain.PullRequestDedupKey(renamePRNodeID); pr.DedupKey != want {
			t.Errorf("pr dedup key = %q, want %q", pr.DedupKey, want)
		}
		review, err := s.Artifacts.Get(ctx, orgID, fx.reviewArtifactID)
		if err != nil || review == nil {
			t.Fatalf("Artifacts.Get(review): %v, %v", review, err)
		}
		if review.Target != renameNewSlug+"#18" {
			t.Errorf("review target = %q, want %s#18", review.Target, renameNewSlug)
		}
		if want := domain.ReviewDedupKey(renamePRNodeID, fx.conversationID); review.DedupKey != want {
			t.Errorf("review dedup key = %q, want %q", review.DedupKey, want)
		}
		branch, err := s.Artifacts.Get(ctx, orgID, fx.branchArtifactID)
		if err != nil || branch == nil {
			t.Fatalf("Artifacts.Get(branch): %v, %v", branch, err)
		}
		if branch.Target != renameNewSlug {
			t.Errorf("branch target = %q, want %s", branch.Target, renameNewSlug)
		}
		if want := domain.BranchDedupKey(fx.repositoryID, fx.branchRef); branch.DedupKey != want {
			t.Errorf("branch dedup key = %q, want %q", branch.DedupKey, want)
		}
		neighbourArtifact, err := s.Artifacts.Get(ctx, orgID, fx.neighbourArtifactID)
		if err != nil || neighbourArtifact == nil {
			t.Fatalf("Artifacts.Get(neighbour): %v, %v", neighbourArtifact, err)
		}
		if neighbourArtifact.Target != renameNeighbourSlug+"#4" {
			t.Errorf("neighbour artifact target = %q, want it untouched", neighbourArtifact.Target)
		}

		// The placement override — human intent, keyed by slug.
		if stale, _ := s.PlacementOverrides.Get(ctx, orgID, domain.PlacementKindRepo, TestGitHubHost, renameOldSlug); stale != nil {
			t.Errorf("placement override still keyed on the old slug: %+v", stale)
		}
		ov, err := s.PlacementOverrides.Get(ctx, orgID, domain.PlacementKindRepo, TestGitHubHost, renameNewSlug)
		if err != nil || ov == nil {
			t.Fatalf("PlacementOverrides.Get(new) = %v, %v; want the moved pin", ov, err)
		}
		if ov.Replicas != 3 {
			t.Errorf("placement replicas = %d, want 3 — the pin moves, its content does not", ov.Replicas)
		}
		if other, _ := s.PlacementOverrides.Get(ctx, orgID, "hostgroup", TestGitHubHost, renameOldSlug); other == nil {
			t.Errorf("the other-kind override moved; only key_kind='repo' rows hold a slug")
		}
	})

	t.Run("Rename_acts_only_within_the_host", func(t *testing.T) {
		// The same owner/repo on another GitHub host is another repository,
		// even when it carries the same provider id — ids are per-deployment
		// sequences. A rename on one host rewrites nothing of the other's:
		// not its registry row, not its entity, not an artifact or ledger row
		// recorded in its scope (with a link or without one), not its
		// placement pin.
		s, orgID, seed := mk(t)
		fx := seedRenameFixture(t, s, orgID, seed, "host")
		const gheURL = TestOtherGitHubHost + "/" + renameOldSlug + "/pull/18"

		if err := TrackRepos(ctx, s.Repos, s.TeamGitHubRepos, orgID, seed.TeamID, TestOtherGitHubHost, []domain.TeamGitHubRepo{
			{Owner: "octo", Repo: "api"},
		}); err != nil {
			t.Fatalf("track %s on %s: %v", renameOldSlug, TestOtherGitHubHost, err)
		}
		if filled, err := s.Repos.FillMissingExternalIDsSystem(ctx, orgID, []domain.RepoRef{
			{Host: TestOtherGitHubHost, Owner: "octo", Repo: "api", ExternalID: fx.externalID},
		}); err != nil || filled != 1 {
			t.Fatalf("seed ghe identity: filled=%d err=%v — the same id on another host is not a collision", filled, err)
		}
		gheRepo, err := s.Repos.GetByRefSystem(ctx, orgID, domain.RepoRefFromSlug(TestOtherGitHubHost, renameOldSlug))
		if err != nil || gheRepo == nil {
			t.Fatalf("resolve ghe repository: %v, %v", gheRepo, err)
		}
		// Node ids are per-deployment too: the ghe pull request #18 carries
		// the same one as the github.com fixture's.
		gheEntity, _, err := s.Entities.FindOrCreateSystem(ctx, orgID, "github", TestOtherGitHubHost, renameOldSlug+"#18", renamePRNodeID, "pr", "The ghe PR", gheURL)
		if err != nil {
			t.Fatalf("seed ghe entity: %v", err)
		}
		gheArtifact, err := s.Artifacts.UpsertSystem(ctx, orgID, linkArtifactEntity(t, s, orgID, domain.Artifact{
			TeamID: seed.TeamID, Provider: domain.ArtifactProviderGitHub, Kind: domain.ArtifactKindPullRequest,
			Scope:  TestOtherGitHubHost,
			Target: domain.PullRequestTarget(renameOldSlug, 19), ExternalID: "19", NodeID: "PR_kwDOghe19",
			URL:   TestOtherGitHubHost + "/" + renameOldSlug + "/pull/19",
			State: domain.ArtifactStatePROpen, DedupKey: domain.PullRequestDedupKey("PR_kwDOghe19"),
		}, "PR_kwDOghe19"))
		if err != nil {
			t.Fatalf("seed ghe artifact: %v", err)
		}
		// The same pull request number as the github.com fixture's, with no
		// link: only its scope says which host it is on, and it is the same
		// dedup key in another scope, so a second row.
		gheLinkless, err := s.Artifacts.UpsertSystem(ctx, orgID, linkArtifactEntity(t, s, orgID, domain.Artifact{
			TeamID: seed.TeamID, Provider: domain.ArtifactProviderGitHub, Kind: domain.ArtifactKindPullRequest,
			Scope:  TestOtherGitHubHost,
			Target: domain.PullRequestTarget(renameOldSlug, 18), ExternalID: "18", NodeID: renamePRNodeID,
			State: domain.ArtifactStatePROpen, DedupKey: domain.PullRequestDedupKey(renamePRNodeID),
		}, renamePRNodeID))
		if err != nil {
			t.Fatalf("seed linkless ghe artifact: %v", err)
		}
		if gheLinkless.ID == fx.prArtifactID {
			t.Fatalf("the ghe artifact landed on the github.com row %s: the same dedup key in two scopes is two rows", fx.prArtifactID)
		}
		gheActionKey := "rename-ghe-" + fx.movedActionKey
		if err := s.ExternalActions.RecordSystem(ctx, orgID, domain.ExternalAction{
			TeamID: seed.TeamID, Provider: "github", Scope: TestOtherGitHubHost, Action: domain.ActionPRCreated,
			Target: renameOldSlug + "#19", ExternalID: "19",
			URL:        TestOtherGitHubHost + "/" + renameOldSlug + "/pull/19",
			Credential: domain.CredentialGitHubApp, DedupKey: gheActionKey,
		}); err != nil {
			t.Fatalf("seed ghe action: %v", err)
		}
		if _, err := s.PlacementOverrides.Upsert(ctx, domain.PlacementOverride{
			OrgID: orgID, KeyKind: domain.PlacementKindRepo, Host: TestOtherGitHubHost, KeyValue: renameOldSlug, Replicas: 7,
		}); err != nil {
			t.Fatalf("seed ghe placement pin: %v", err)
		}

		out, err := s.Repos.RenameSystem(ctx, orgID, domain.RepoRef{
			Host:  TestGitHubHost,
			Owner: "octo", Repo: "platform-api", ExternalID: fx.externalID,
		})
		if err != nil {
			t.Fatalf("RenameSystem(github.com): %v", err)
		}
		if !out.Renamed {
			t.Fatalf("outcome = %+v, want the github.com rename to go through", out)
		}

		// github.com moved.
		if moved, _ := s.Repos.GetByRefSystem(ctx, orgID, repoRef(renameNewSlug)); moved == nil {
			t.Errorf("github.com repository did not move to %s", renameNewSlug)
		}
		// ghe did not.
		got, err := s.Repos.GetByRefSystem(ctx, orgID, domain.RepoRefFromSlug(TestOtherGitHubHost, renameOldSlug))
		if err != nil || got == nil || got.ID != gheRepo.ID || got.ExternalID != fx.externalID {
			t.Errorf("ghe repository after a github.com rename = %+v, %v; want %s still at %s", got, err, gheRepo.ID, renameOldSlug)
		}
		if moved, _ := s.Repos.GetByRefSystem(ctx, orgID, domain.RepoRefFromSlug(TestOtherGitHubHost, renameNewSlug)); moved != nil {
			t.Errorf("a ghe row answers to %s after a github.com rename: %+v", renameNewSlug, moved)
		}
		ent, err := s.Entities.GetBySourceSystem(ctx, orgID, "github", TestOtherGitHubHost, renameOldSlug+"#18")
		if err != nil || ent == nil || ent.ID != gheEntity.ID || ent.URL != gheURL {
			t.Errorf("ghe entity = %+v, %v; want it untouched under %s#18", ent, err, renameOldSlug)
		}
		if ent, _ := s.Entities.GetBySourceSystem(ctx, orgID, "github", TestOtherGitHubHost, renameNewSlug+"#18"); ent != nil {
			t.Errorf("a ghe entity answers to %s#18: %+v", renameNewSlug, ent)
		}
		art, err := s.Artifacts.Get(ctx, orgID, gheArtifact.ID)
		if err != nil || art == nil {
			t.Fatalf("Artifacts.Get(ghe): %v, %v", art, err)
		}
		if art.Target != renameOldSlug+"#19" || art.DedupKey != domain.PullRequestDedupKey("PR_kwDOghe19") {
			t.Errorf("ghe artifact = (target %q, key %q); want it untouched — it is recorded on another host", art.Target, art.DedupKey)
		}
		linkless, err := s.Artifacts.Get(ctx, orgID, gheLinkless.ID)
		if err != nil || linkless == nil {
			t.Fatalf("Artifacts.Get(linkless ghe): %v, %v", linkless, err)
		}
		if linkless.Target != renameOldSlug+"#18" || linkless.DedupKey != domain.PullRequestDedupKey(renamePRNodeID) || linkless.Scope != TestOtherGitHubHost {
			t.Errorf("linkless ghe artifact = %+v; want it untouched — its scope is another host", linkless)
		}
		if a := findActionByKey(t, s, orgID, gheActionKey); a.URL != TestOtherGitHubHost+"/"+renameOldSlug+"/pull/19" {
			t.Errorf("ghe action url = %q, want it untouched", a.URL)
		}
		if _, rawCurrent := seed.RawActionURL(t, gheActionKey); rawCurrent != "" {
			t.Errorf("ghe action current_url = %q, want NULL — its object never moved", rawCurrent)
		}
		if pin, _ := s.PlacementOverrides.Get(ctx, orgID, domain.PlacementKindRepo, TestOtherGitHubHost, renameOldSlug); pin == nil || pin.Replicas != 7 {
			t.Errorf("ghe pin on %s after a github.com rename = %+v; want it still there", renameOldSlug, pin)
		}
		if pin, _ := s.PlacementOverrides.Get(ctx, orgID, domain.PlacementKindRepo, TestOtherGitHubHost, renameNewSlug); pin != nil {
			t.Errorf("a ghe pin answers to %s after a github.com rename: %+v", renameNewSlug, pin)
		}
		if pin, _ := s.PlacementOverrides.Get(ctx, orgID, domain.PlacementKindRepo, TestGitHubHost, renameNewSlug); pin == nil || pin.Replicas != 3 {
			t.Errorf("github.com pin after its own rename = %+v; want it moved to %s", pin, renameNewSlug)
		}

		// And the other way round: renaming the ghe repository moves only it.
		out, err = s.Repos.RenameSystem(ctx, orgID, domain.RepoRef{
			Host:  TestOtherGitHubHost,
			Owner: "octo", Repo: "api-ghe", ExternalID: fx.externalID,
		})
		if err != nil || !out.Renamed {
			t.Fatalf("RenameSystem(ghe) = %+v, %v; want a rename", out, err)
		}
		if got, _ := s.Repos.GetByRefSystem(ctx, orgID, domain.RepoRefFromSlug(TestOtherGitHubHost, "octo/api-ghe")); got == nil || got.ID != gheRepo.ID {
			t.Errorf("ghe repository after its own rename = %+v; want row %s at octo/api-ghe", got, gheRepo.ID)
		}
		if got, _ := s.Repos.GetByRefSystem(ctx, orgID, repoRef(renameNewSlug)); got == nil || got.ExternalID != fx.externalID {
			t.Errorf("github.com repository after a ghe rename = %+v; want it still at %s", got, renameNewSlug)
		}
		if ent, _ := s.Entities.GetBySourceSystem(ctx, orgID, "github", TestOtherGitHubHost, "octo/api-ghe#18"); ent == nil || ent.ID != gheEntity.ID {
			t.Errorf("ghe entity after its own rename = %+v; want row %s under octo/api-ghe#18", ent, gheEntity.ID)
		}
		if ent, _ := s.Entities.GetBySourceSystem(ctx, orgID, "github", TestGitHubHost, renameNewSlug+"#18"); ent == nil || ent.ID != fx.entityID {
			t.Errorf("github.com entity after a ghe rename = %+v; want row %s under %s#18", ent, fx.entityID, renameNewSlug)
		}
		if got, _ := s.Artifacts.Get(ctx, orgID, gheLinkless.ID); got == nil || got.Target != "octo/api-ghe#18" || got.DedupKey != domain.PullRequestDedupKey(renamePRNodeID) {
			t.Errorf("linkless ghe artifact after its own rename = %+v; want it moved to octo/api-ghe#18 by its scope", got)
		}
		if got, _ := s.Artifacts.Get(ctx, orgID, fx.prArtifactID); got == nil || got.Target != renameNewSlug+"#18" {
			t.Errorf("github.com artifact after a ghe rename = %+v; want it still at %s#18", got, renameNewSlug)
		}
		if a := findActionByKey(t, s, orgID, gheActionKey); a.URL != TestOtherGitHubHost+"/octo/api-ghe/pull/19" {
			t.Errorf("ghe action url after its own rename = %q, want it moved to octo/api-ghe", a.URL)
		}
		if pin, _ := s.PlacementOverrides.Get(ctx, orgID, domain.PlacementKindRepo, TestOtherGitHubHost, "octo/api-ghe"); pin == nil || pin.Replicas != 7 {
			t.Errorf("ghe pin after its own rename = %+v; want it moved to octo/api-ghe", pin)
		}
		if pin, _ := s.PlacementOverrides.Get(ctx, orgID, domain.PlacementKindRepo, TestGitHubHost, renameNewSlug); pin == nil || pin.Replicas != 3 {
			t.Errorf("github.com pin after a ghe rename = %+v; want it still at %s", pin, renameNewSlug)
		}
	})

	t.Run("Rename_matches_a_context_path_exactly", func(t *testing.T) {
		// Two deployments served from one authority under context paths that
		// differ only in case are two hosts: the host is kept as configured, and
		// a case-sensitive server serves each under its own spelling. A rename
		// on one rewrites the artifacts recorded on it and leaves the other's,
		// which carry the same owner/repo. Its links are rewritten too: the
		// slug is read right after the context path, not at the head of the
		// link's path.
		s, orgID, seed := mk(t)
		const (
			upper = "https://example.com/GitHub"
			lower = "https://example.com/github"
		)
		if err := TrackRepos(ctx, s.Repos, s.TeamGitHubRepos, orgID, seed.TeamID, upper, []domain.TeamGitHubRepo{
			{Owner: "octo", Repo: "api"},
		}); err != nil {
			t.Fatalf("track %s on %s: %v", renameOldSlug, upper, err)
		}
		if filled, err := s.Repos.FillMissingExternalIDsSystem(ctx, orgID, []domain.RepoRef{
			{Host: upper, Owner: "octo", Repo: "api", ExternalID: "ctx-path-1"},
		}); err != nil || filled != 1 {
			t.Fatalf("seed identity on %s: filled=%d err=%v", upper, filled, err)
		}
		nodeID := func(number int) string { return fmt.Sprintf("PR_kwDOctx%d", number) }
		ownEntity, _, err := s.Entities.FindOrCreateSystem(ctx, orgID, "github", upper, renameOldSlug+"#22", nodeID(22), "pr", "A PR under a context path",
			upper+"/"+renameOldSlug+"/pull/22")
		if err != nil {
			t.Fatalf("seed entity on %s: %v", upper, err)
		}
		artifactOn := func(host string, number int) domain.Artifact {
			t.Helper()
			a, err := s.Artifacts.UpsertSystem(ctx, orgID, linkArtifactEntity(t, s, orgID, domain.Artifact{
				TeamID: seed.TeamID, Provider: domain.ArtifactProviderGitHub, Kind: domain.ArtifactKindPullRequest,
				Scope:  host,
				Target: domain.PullRequestTarget(renameOldSlug, number), ExternalID: fmt.Sprint(number), NodeID: nodeID(number),
				URL:   fmt.Sprintf("%s/%s/pull/%d", host, renameOldSlug, number),
				State: domain.ArtifactStatePROpen, DedupKey: domain.PullRequestDedupKey(nodeID(number)),
			}, nodeID(number)))
			if err != nil {
				t.Fatalf("seed artifact on %s: %v", host, err)
			}
			return a
		}
		other := artifactOn(lower, 21)
		own := artifactOn(upper, 22)
		actionOn := func(host string, number int) string {
			t.Helper()
			key := fmt.Sprintf("ctx-path-%d", number)
			if err := s.ExternalActions.RecordSystem(ctx, orgID, domain.ExternalAction{
				TeamID: seed.TeamID, Provider: domain.ArtifactProviderGitHub, Scope: host, Action: domain.ActionPRCreated,
				Target: domain.PullRequestTarget(renameOldSlug, number), ExternalID: fmt.Sprint(number),
				URL:        fmt.Sprintf("%s/%s/pull/%d", host, renameOldSlug, number),
				Credential: domain.CredentialGitHubApp, DedupKey: key,
			}); err != nil {
				t.Fatalf("seed action on %s: %v", host, err)
			}
			return key
		}
		otherAction := actionOn(lower, 21)
		ownAction := actionOn(upper, 22)

		out, err := s.Repos.RenameSystem(ctx, orgID, domain.RepoRef{
			Host: upper, Owner: "octo", Repo: "platform-api", ExternalID: "ctx-path-1",
		})
		if err != nil || !out.Renamed {
			t.Fatalf("RenameSystem(%s) = %+v, %v; want a rename", upper, out, err)
		}

		got, err := s.Artifacts.Get(ctx, orgID, other.ID)
		if err != nil || got == nil {
			t.Fatalf("Artifacts.Get(%s): %v, %v", lower, got, err)
		}
		if got.Target != domain.PullRequestTarget(renameOldSlug, 21) || got.DedupKey != domain.PullRequestDedupKey(nodeID(21)) {
			t.Errorf("artifact linked under %s = (target %q, key %q); want it untouched by a rename on %s", lower, got.Target, got.DedupKey, upper)
		}
		got, err = s.Artifacts.Get(ctx, orgID, own.ID)
		if err != nil || got == nil {
			t.Fatalf("Artifacts.Get(%s): %v, %v", upper, got, err)
		}
		if got.Target != domain.PullRequestTarget(renameNewSlug, 22) || got.DedupKey != domain.PullRequestDedupKey(nodeID(22)) {
			t.Errorf("artifact linked under %s = (target %q, key %q); want it moved to %s", upper, got.Target, got.DedupKey, renameNewSlug)
		}

		if a := findActionByKey(t, s, orgID, ownAction); a.URL != upper+"/"+renameNewSlug+"/pull/22" {
			t.Errorf("action url under %s = %q, want %s/%s/pull/22", upper, a.URL, upper, renameNewSlug)
		}
		if a := findActionByKey(t, s, orgID, otherAction); a.URL != lower+"/"+renameOldSlug+"/pull/21" {
			t.Errorf("action url under %s = %q, want it untouched", lower, a.URL)
		}
		ent, err := s.Entities.GetSystem(ctx, orgID, ownEntity.ID)
		if err != nil || ent == nil {
			t.Fatalf("Entities.GetSystem(%s): %v, %v", ownEntity.ID, ent, err)
		}
		if ent.SourceID != renameNewSlug+"#22" || ent.URL != upper+"/"+renameNewSlug+"/pull/22" {
			t.Errorf("entity under %s = (source id %q, url %q); want both moved to %s", upper, ent.SourceID, ent.URL, renameNewSlug)
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
			Host:  TestGitHubHost,
			Owner: "octo", Repo: "platform-api", ExternalID: fx.externalID,
		}); err != nil {
			t.Fatalf("RenameSystem: %v", err)
		}

		// The entity's link resolves under the new slug; the neighbour's —
		// which shares the old slug as a string prefix, in the URL too — is
		// untouched.
		ent, err := s.Entities.GetBySourceSystem(ctx, orgID, "github", "https://github.com", renameNewSlug+"#18")
		if err != nil || ent == nil {
			t.Fatalf("entity by new source id = %v, %v", ent, err)
		}
		if want := "https://github.com/" + renameNewSlug + "/pull/18"; ent.URL != want {
			t.Errorf("entity url = %q, want %q", ent.URL, want)
		}
		neighbour, _ := s.Entities.GetBySourceSystem(ctx, orgID, "github", "https://github.com", renameNeighbourSlug+"#4")
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
			Host:  TestGitHubHost,
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

	t.Run("Rename_moves_the_clone_url", func(t *testing.T) {
		// Cold restores, `workspace add` and the bare-clone bootstrap clone
		// from the stored clone URL, so a rename that left it would put the
		// old name back into every checkout made from it. Both forms GitHub
		// reports move; one that does not spell the old slug on the host, or
		// none at all, is left as it was.
		s, orgID, _ := mk(t)
		cases := []struct {
			owner, from, to, externalID string
			cloneURL, want              string
		}{
			{"octo", "https-repo", "https-repo-2", "201", "https://github.com/octo/https-repo.git", "https://github.com/octo/https-repo-2.git"},
			{"octo", "ssh-repo", "ssh-repo-2", "202", "git@github.com:octo/ssh-repo.git", "git@github.com:octo/ssh-repo-2.git"},
			{"octo", "mirrored", "mirrored-2", "203", "https://mirror.example/octo/mirrored.git", "https://mirror.example/octo/mirrored.git"},
			{"octo", "unprofiled", "unprofiled-2", "204", "", ""},
		}
		for _, c := range cases {
			if _, err := s.Repos.UpsertSystem(ctx, orgID, domain.Repository{
				Host: TestGitHubHost, Owner: c.owner, Repo: c.from, ExternalID: c.externalID, CloneURL: c.cloneURL,
			}); err != nil {
				t.Fatalf("seed %s/%s: %v", c.owner, c.from, err)
			}
			if _, err := s.Repos.RenameSystem(ctx, orgID, domain.RepoRef{
				Host: TestGitHubHost, Owner: c.owner, Repo: c.to, ExternalID: c.externalID,
			}); err != nil {
				t.Fatalf("RenameSystem %s/%s -> %s: %v", c.owner, c.from, c.to, err)
			}
			got, err := s.Repos.GetByRefSystem(ctx, orgID, repoRef(c.owner+"/"+c.to))
			if err != nil || got == nil {
				t.Fatalf("read %s/%s after the rename: got=%v err=%v", c.owner, c.to, got, err)
			}
			if got.CloneURL != c.want {
				t.Errorf("%s/%s clone url = %q, want %q", c.owner, c.to, got.CloneURL, c.want)
			}
		}
	})

	t.Run("Rename_is_idempotent", func(t *testing.T) {
		// The second detection of the same rename — a retry, a second poller,
		// the next cycle — finds the slug already current. That is a no-op and
		// a nil error, not a failure: losing is terminal and needs no retry.
		s, orgID, seed := mk(t)
		fx := seedRenameFixture(t, s, orgID, seed, "idempotent")
		observed := domain.RepoRef{Host: TestGitHubHost, Owner: "octo", Repo: "platform-api", ExternalID: fx.externalID}

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
		ent, _ := s.Entities.GetBySourceSystem(ctx, orgID, "github", "https://github.com", renameNewSlug+"#18")
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
			Host:  TestGitHubHost,
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
			Host:  TestGitHubHost,
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
		if ent, _ := s.Entities.GetBySourceSystem(ctx, orgID, "github", "https://github.com", renameOldSlug+"#18"); ent == nil {
			t.Errorf("the entity was rewritten; nothing about this observation is a rename")
		}

		// The same conclusion from the detection half, which is what the
		// pollers actually run before they ever call the store. What the
		// observation does report is that the stored row's name went to
		// another id, which is resolved by the stored row's own id.
		stored, err := s.Repos.ListIdentitiesSystem(ctx, orgID, TestGitHubHost)
		if err != nil {
			t.Fatalf("ListIdentitiesSystem: %v", err)
		}
		observed := []domain.RepoRef{{Host: TestGitHubHost, Source: "github", Owner: "octo", Repo: "api", ExternalID: "999000111"}}
		if got := domain.DetectRepoRenames(stored, observed); len(got) != 0 {
			t.Errorf("DetectRepoRenames = %+v, want none", got)
		}
		if got := domain.DetectReassignedRepoNames(stored, observed); len(got) != 1 || got[0].ExternalID != fx.externalID {
			t.Errorf("DetectReassignedRepoNames = %+v, want the stored octo/api row", got)
		}
	})

	t.Run("A_repository_with_no_external_id_is_never_renamed", func(t *testing.T) {
		s, orgID, seed := mk(t)
		// A row minted by name carries no id, which is exactly the id-less
		// state under test.
		if err := TrackRepos(ctx, s.Repos, s.TeamGitHubRepos, orgID, seed.TeamID, TestGitHubHost, []domain.TeamGitHubRepo{
			{Owner: "octo", Repo: "unidentified"},
		}); err != nil {
			t.Fatalf("seed id-less repository: %v", err)
		}

		// No id on the observation: nothing to key on, so nothing happens.
		out, err := s.Repos.RenameSystem(ctx, orgID, domain.RepoRef{Host: TestGitHubHost, Owner: "octo", Repo: "renamed"})
		if err != nil {
			t.Fatalf("RenameSystem with no observed id: %v", err)
		}
		if out.Renamed {
			t.Errorf("outcome = %+v, want no rename without an observed id", out)
		}

		// An id on the observation, none on the row: the row is not reachable
		// by identity, so it is not renamable in this direction either.
		out, err = s.Repos.RenameSystem(ctx, orgID, domain.RepoRef{
			Host:  TestGitHubHost,
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
			Host:  TestGitHubHost,
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
		// The occupant is a registry row nothing tracks any more, carrying
		// the identity the profiler recorded for it while it was live.
		if _, err := s.Repos.UpsertSystem(ctx, orgID, domain.Repository{
			Host:  TestGitHubHost,
			Owner: "octo", Repo: "platform-api", ExternalID: "555000555",
		}); err != nil {
			t.Fatalf("seed occupant: %v", err)
		}

		_, err := s.Repos.RenameSystem(ctx, orgID, domain.RepoRef{
			Host:  TestGitHubHost,
			Owner: "octo", Repo: "platform-api", ExternalID: fx.externalID,
		})
		if !errors.Is(err, db.ErrRepoSlugOccupied) {
			t.Fatalf("err = %v, want ErrRepoSlugOccupied", err)
		}
		// All tables or none: the refusal left every reference where it was.
		if got, _ := s.Repos.GetByRef(ctx, orgID, repoRef(renameOldSlug)); got == nil {
			t.Errorf("the repository row moved despite the refusal")
		}
		if ent, _ := s.Entities.GetBySourceSystem(ctx, orgID, "github", "https://github.com", renameOldSlug+"#18"); ent == nil {
			t.Errorf("the entity's source id moved despite the refusal")
		}
		tracked, _ := s.TeamGitHubRepos.ListForTeamSystem(ctx, seed.TeamID, TestGitHubHost)
		if !sameSet(trackedSlugs(tracked), []string{renameOldSlug, renameNeighbourSlug}) {
			t.Errorf("the tracked set moved despite the refusal: %v", trackedSlugs(tracked))
		}
	})

	t.Run("Rename_absorbs_a_bare_row_minted_under_the_new_name", func(t *testing.T) {
		// TF wrote a pull request under the new name before it observed the
		// rename: an agent touch through GitHub's redirect, or a GraphQL read
		// keyed by the current name. Creating that entity minted a bare
		// registry row under the new name. GitHub has now reported that the
		// name belongs to the renamed row's id, so the rename folds the bare
		// row in rather than refusing over it.
		s, orgID, seed := mk(t)
		fx := seedRenameFixture(t, s, orgID, seed, "absorb")
		renamed, err := s.Repos.GetByRefSystem(ctx, orgID, repoRef(renameOldSlug))
		if err != nil || renamed == nil {
			t.Fatalf("resolve %s: got=%v err=%v", renameOldSlug, renamed, err)
		}

		early, _, err := s.Entities.FindOrCreateSystem(ctx, orgID, "github", "https://github.com", renameNewSlug+"#25", "", "pr", "Written under the new name",
			"https://github.com/"+renameNewSlug+"/pull/25")
		if err != nil {
			t.Fatalf("seed the entity written under the new name: %v", err)
		}
		bare, err := s.Repos.GetByRefSystem(ctx, orgID, repoRef(renameNewSlug))
		if err != nil || bare == nil {
			t.Fatalf("resolve %s: got=%v err=%v; want the bare row the entity create minted", renameNewSlug, bare, err)
		}
		if bare.ID == renamed.ID || bare.ExternalID != "" || early.RepositoryID != bare.ID {
			t.Fatalf("premise: bare row = %+v, entity references %q; want a second, id-less row the entity references", bare, early.RepositoryID)
		}

		out, err := s.Repos.RenameSystem(ctx, orgID, domain.RepoRef{
			Host:  TestGitHubHost,
			Owner: "octo", Repo: "platform-api", ExternalID: fx.externalID,
		})
		if err != nil {
			t.Fatalf("RenameSystem: %v", err)
		}
		if !out.Renamed || out.From != renameOldSlug || out.To != renameNewSlug {
			t.Fatalf("outcome = %+v, want a rename %s -> %s", out, renameOldSlug, renameNewSlug)
		}

		moved, err := s.Repos.GetByRefSystem(ctx, orgID, repoRef(renameNewSlug))
		if err != nil || moved == nil {
			t.Fatalf("resolve %s after the rename: got=%v err=%v", renameNewSlug, moved, err)
		}
		if moved.ID != renamed.ID || moved.ExternalID != fx.externalID {
			t.Errorf("%s resolves to %+v, want the renamed row %s carrying %s", renameNewSlug, moved, renamed.ID, fx.externalID)
		}
		if got, err := s.Repos.GetSystem(ctx, orgID, bare.ID); !errors.Is(err, db.ErrNoSuchRepository) {
			t.Errorf("bare row after the rename = %+v, %v; want ErrNoSuchRepository", got, err)
		}

		// The entity written early keeps its id, key and link, and now
		// references the renamed row.
		got, err := s.Entities.GetBySourceSystem(ctx, orgID, "github", "https://github.com", renameNewSlug+"#25")
		if err != nil || got == nil {
			t.Fatalf("entity under the new name = %v, %v; want the early entity", got, err)
		}
		if got.ID != early.ID || got.RepositoryID != renamed.ID {
			t.Errorf("early entity = {id %s, repository %s}, want {id %s, repository %s}", got.ID, got.RepositoryID, early.ID, renamed.ID)
		}
		if want := "https://github.com/" + renameNewSlug + "/pull/25"; got.URL != want {
			t.Errorf("early entity url = %q, want %q unchanged", got.URL, want)
		}
		// The renamed repository's own entity moved as in any rename.
		pr, err := s.Entities.GetBySourceSystem(ctx, orgID, "github", "https://github.com", renameNewSlug+"#18")
		if err != nil || pr == nil || pr.ID != fx.entityID || pr.RepositoryID != renamed.ID {
			t.Errorf("renamed repository's entity = %+v, %v; want %s referencing %s", pr, err, fx.entityID, renamed.ID)
		}

		tracked, err := s.TeamGitHubRepos.ListForTeamSystem(ctx, seed.TeamID, TestGitHubHost)
		if err != nil {
			t.Fatalf("ListForTeamSystem: %v", err)
		}
		if got := trackedSlugs(tracked); !sameSet(got, []string{renameNewSlug, renameNeighbourSlug}) {
			t.Errorf("tracked set = %v, want [%s %s]", got, renameNewSlug, renameNeighbourSlug)
		}
	})

	t.Run("Rename_refuses_a_bare_occupant_a_team_tracks", func(t *testing.T) {
		// The same bare row an entity create mints, tracked: the row then
		// carries a team's decision, and deleting it would cascade that
		// tracking row away.
		s, orgID, seed := mk(t)
		fx := seedRenameFixture(t, s, orgID, seed, "bare-tracked")
		if err := TrackRepos(ctx, s.Repos, s.TeamGitHubRepos, orgID, seed.TeamID, TestGitHubHost, []domain.TeamGitHubRepo{
			{Owner: "octo", Repo: "api"},
			{Owner: "octo", Repo: "api-gateway"},
			{Owner: "octo", Repo: "platform-api"},
		}); err != nil {
			t.Fatalf("track the new name: %v", err)
		}
		bare, err := s.Repos.GetByRefSystem(ctx, orgID, repoRef(renameNewSlug))
		if err != nil || bare == nil || bare.ExternalID != "" {
			t.Fatalf("premise: %s = %+v, %v; want a tracked, id-less row", renameNewSlug, bare, err)
		}

		_, err = s.Repos.RenameSystem(ctx, orgID, domain.RepoRef{
			Host:  TestGitHubHost,
			Owner: "octo", Repo: "platform-api", ExternalID: fx.externalID,
		})
		if !errors.Is(err, db.ErrRepoSlugOccupied) {
			t.Fatalf("err = %v, want ErrRepoSlugOccupied", err)
		}

		if got, err := s.Repos.GetSystem(ctx, orgID, bare.ID); err != nil || got == nil {
			t.Errorf("the tracked occupant = %v, %v; want it kept", got, err)
		}
		if got, _ := s.Repos.GetByRef(ctx, orgID, repoRef(renameOldSlug)); got == nil {
			t.Error("the repository row moved despite the refusal")
		}
		tracked, _ := s.TeamGitHubRepos.ListForTeamSystem(ctx, seed.TeamID, TestGitHubHost)
		if want := []string{renameOldSlug, renameNeighbourSlug, renameNewSlug}; !sameSet(trackedSlugs(tracked), want) {
			t.Errorf("tracked set = %v, want %v untouched", trackedSlugs(tracked), want)
		}
	})

	t.Run("Rename_refuses_a_bare_occupant_a_worktree_references", func(t *testing.T) {
		// A run materialized a checkout of the bare row. The ledger FK has no
		// ON DELETE action, so the delete is refused, and the refusal reads as
		// the classified terminal state rather than a raw constraint error.
		s, orgID, seed := mk(t)
		fx := seedRenameFixture(t, s, orgID, seed, "bare-worktree")
		early, _, err := s.Entities.FindOrCreateSystem(ctx, orgID, "github", "https://github.com", renameNewSlug+"#25", "", "pr", "Written under the new name", "")
		if err != nil {
			t.Fatalf("seed the entity written under the new name: %v", err)
		}
		bare, err := s.Repos.GetByRefSystem(ctx, orgID, repoRef(renameNewSlug))
		if err != nil || bare == nil || bare.ExternalID != "" {
			t.Fatalf("premise: %s = %+v, %v; want an id-less row", renameNewSlug, bare, err)
		}
		conversationID := seed.Conversation(t, "bare-worktree-ledger")
		if _, _, err := s.ConversationWorktrees.Insert(ctx, orgID, domain.ConversationWorktree{
			ConversationID: conversationID, RepositoryID: bare.ID, Ref: "pr-25",
			Path: "/tmp/wt/" + conversationID + "/octo/platform-api/pr-25",
		}); err != nil {
			t.Fatalf("seed the occupant's worktree: %v", err)
		}

		_, err = s.Repos.RenameSystem(ctx, orgID, domain.RepoRef{
			Host:  TestGitHubHost,
			Owner: "octo", Repo: "platform-api", ExternalID: fx.externalID,
		})
		if !errors.Is(err, db.ErrRepoSlugOccupied) {
			t.Fatalf("err = %v, want ErrRepoSlugOccupied — a raw constraint violation reads as retryable", err)
		}

		// All tables or none: the entity re-point that ran before the delete
		// rolled back with it.
		if got, err := s.Repos.GetSystem(ctx, orgID, bare.ID); err != nil || got == nil {
			t.Errorf("the occupant = %v, %v; want it kept", got, err)
		}
		if got, _ := s.Entities.GetBySourceSystem(ctx, orgID, "github", "https://github.com", renameNewSlug+"#25"); got == nil || got.RepositoryID != bare.ID {
			t.Errorf("early entity = %+v, want it still referencing the occupant %s", got, bare.ID)
		}
		if got, _ := s.Entities.GetBySourceSystem(ctx, orgID, "github", "https://github.com", renameOldSlug+"#18"); got == nil {
			t.Error("the renamed repository's entity moved despite the refusal")
		}
		if early.RepositoryID != bare.ID {
			t.Fatalf("premise: early entity referenced %q, want the occupant %s", early.RepositoryID, bare.ID)
		}
		worktrees, err := s.ConversationWorktrees.ListSystem(ctx, orgID, conversationID)
		if err != nil {
			t.Fatalf("worktrees ListSystem: %v", err)
		}
		if len(worktrees) != 1 || worktrees[0].RepositoryID != bare.ID {
			t.Errorf("occupant's worktree ledger = %+v, want one row on %s", worktrees, bare.ID)
		}
	})

	t.Run("Rename_refuses_when_a_durable_entity_already_answers_to_the_target", func(t *testing.T) {
		// The reference tables absorb an orphan; entities must NOT. An entity
		// carries tasks, conversations and memory — deleting one to make room
		// for a rename destroys what forward-only tracking promises to keep.
		//
		// This is ordinary product state, not corruption: an entity carries
		// its slug rather than a reference to a repositories row, and nothing
		// that retires a repository row touches it, so a name can be spoken
		// for by records alone — a state the repository-row guard cannot
		// see. Refusing is the answer, classified so the caller treats it as
		// terminal rather than retrying it every poll cycle.
		s, orgID, _ := mk(t)

		// A PR recorded under the name before the rename was observed.
		// Creating it minted a bare registry row, which the rename absorbs;
		// the refusal comes from the entity key, and rolls the absorb back.
		orphan, _, err := s.Entities.FindOrCreateSystem(ctx, orgID, "github", "https://github.com", "octo/api#18", "", "pr", "the untracked repo's PR", "")
		if err != nil {
			t.Fatalf("seed the orphaned entity: %v", err)
		}

		// A live repository with a PR of the same number, which GitHub now
		// renames onto the freed name.
		if _, err := s.Repos.UpsertSystem(ctx, orgID, domain.Repository{
			Host:  TestGitHubHost,
			Owner: "octo", Repo: "legacy", ExternalID: "2",
		}); err != nil {
			t.Fatalf("seed the renaming repository: %v", err)
		}
		if _, _, err := s.Entities.FindOrCreateSystem(ctx, orgID, "github", "https://github.com", "octo/legacy#18", "", "pr", "the live repo's PR", ""); err != nil {
			t.Fatalf("seed the live entity: %v", err)
		}

		_, err = s.Repos.RenameSystem(ctx, orgID, domain.RepoRef{
			Host:  TestGitHubHost,
			Owner: "octo", Repo: "api", ExternalID: "2",
		})
		if !errors.Is(err, db.ErrRepoSlugOccupied) {
			t.Fatalf("err = %v, want ErrRepoSlugOccupied — a raw constraint violation reads as retryable", err)
		}

		// All tables or none: the refusal rolled the whole rewrite back.
		if got, _ := s.Repos.GetByRef(ctx, orgID, repoRef("octo/legacy")); got == nil {
			t.Error("the repository row moved despite the refusal")
		}
		if got, _ := s.Entities.GetBySourceSystem(ctx, orgID, "github", "https://github.com", "octo/legacy#18"); got == nil {
			t.Error("the live entity's source id moved despite the refusal")
		}
		kept, _ := s.Entities.GetBySourceSystem(ctx, orgID, "github", "https://github.com", "octo/api#18")
		if kept == nil || kept.ID != orphan.ID || kept.RepositoryID != orphan.RepositoryID {
			t.Errorf("the orphaned entity = %+v, want it untouched — a rename never destroys durable work", kept)
		}
		if got, err := s.Repos.GetSystem(ctx, orgID, orphan.RepositoryID); err != nil || got == nil {
			t.Errorf("the orphaned entity's registry row = %v, %v; want it kept by the rollback", got, err)
		}
	})

	t.Run("Rename_onto_a_retired_holders_name_is_applied", func(t *testing.T) {
		// A repository renamed onto a name a deleted repository held, which
		// recorded the same pull request number and branch. Their artifacts
		// are keyed on the pull requests' node ids and the repositories' row
		// ids, so the retired holder's rows speak for no name and the rename
		// goes through; each repository keeps its own rows.
		s, orgID, seed := mk(t)
		const ref = "refs/heads/fix"

		dead, err := s.Repos.UpsertSystem(ctx, orgID, domain.Repository{Host: TestGitHubHost, Owner: "octo", Repo: "api", ExternalID: "1"})
		if err != nil {
			t.Fatalf("seed the deleted repository: %v", err)
		}
		deadRows := recordRepoArtifacts(t, s, orgID, seed.TeamID, dead, 18, "PR_kwDOdead18", 501, ref)
		if err := RetireRepo(ctx, s.Repos, orgID, TestGitHubHost, "octo/api"); err != nil {
			t.Fatal(err)
		}

		live, err := s.Repos.UpsertSystem(ctx, orgID, domain.Repository{Host: TestGitHubHost, Owner: "octo", Repo: "legacy", ExternalID: "2"})
		if err != nil {
			t.Fatalf("seed the renaming repository: %v", err)
		}
		liveRows := recordRepoArtifacts(t, s, orgID, seed.TeamID, live, 18, "PR_kwDOlive18", 502, ref)

		out, err := s.Repos.RenameSystem(ctx, orgID, domain.RepoRef{Host: TestGitHubHost, Owner: "octo", Repo: "api", ExternalID: "2"})
		if err != nil || !out.Renamed {
			t.Fatalf("RenameSystem = %+v, %v; want the rename applied", out, err)
		}
		assertArtifactAt(t, s, orgID, liveRows.pr, "octo/api#18", domain.PullRequestDedupKey("PR_kwDOlive18"))
		assertArtifactAt(t, s, orgID, liveRows.review, "octo/api#18", domain.ReviewDedupKey("PR_kwDOlive18", liveRows.conversationID))
		assertArtifactAt(t, s, orgID, liveRows.comment, "octo/api#18", domain.GitHubCommentDedupKey(domain.GitHubIssueComment, 502))
		assertArtifactAt(t, s, orgID, liveRows.branch, "octo/api", domain.BranchDedupKey(live.ID, ref))
		assertArtifactAt(t, s, orgID, deadRows.pr, "octo/api#18", domain.PullRequestDedupKey("PR_kwDOdead18"))
		assertArtifactAt(t, s, orgID, deadRows.review, "octo/api#18", domain.ReviewDedupKey("PR_kwDOdead18", deadRows.conversationID))
		assertArtifactAt(t, s, orgID, deadRows.comment, "octo/api#18", domain.GitHubCommentDedupKey(domain.GitHubIssueComment, 501))
		assertArtifactAt(t, s, orgID, deadRows.branch, "octo/api", domain.BranchDedupKey(dead.ID, ref))
	})

	t.Run("Renaming_the_live_holder_leaves_a_retired_namesakes_artifacts", func(t *testing.T) {
		// A repository deleted and recreated under the same name: the retired
		// row and the live one both held octo/api, and both recorded pull
		// request #18, a comment on it and the same branch. Renaming the live
		// one moves its rows, selected by its entities and its registry id,
		// and leaves the retired one's under the name they were recorded
		// under — they are its history, not the live repository's.
		s, orgID, seed := mk(t)
		const ref = "refs/heads/fix"

		dead, err := s.Repos.UpsertSystem(ctx, orgID, domain.Repository{Host: TestGitHubHost, Owner: "octo", Repo: "api", ExternalID: "1"})
		if err != nil {
			t.Fatalf("seed the deleted repository: %v", err)
		}
		deadRows := recordRepoArtifacts(t, s, orgID, seed.TeamID, dead, 18, "PR_kwDOdead18", 501, ref)
		if err := RetireRepo(ctx, s.Repos, orgID, TestGitHubHost, "octo/api"); err != nil {
			t.Fatal(err)
		}
		live, err := s.Repos.UpsertSystem(ctx, orgID, domain.Repository{Host: TestGitHubHost, Owner: "octo", Repo: "api", ExternalID: "3"})
		if err != nil {
			t.Fatalf("seed the repository now under the name: %v", err)
		}
		if live.ID == dead.ID {
			t.Fatalf("the new repository landed on the retired row %s", dead.ID)
		}
		liveRows := recordRepoArtifacts(t, s, orgID, seed.TeamID, live, 18, "PR_kwDOlive18", 502, ref)
		if liveRows.pr == deadRows.pr || liveRows.review == deadRows.review || liveRows.comment == deadRows.comment || liveRows.branch == deadRows.branch {
			t.Fatalf("the new repository's #18 or branch landed on the retired one's rows: live=%+v dead=%+v", liveRows, deadRows)
		}

		out, err := s.Repos.RenameSystem(ctx, orgID, domain.RepoRef{Host: TestGitHubHost, Owner: "octo", Repo: "api-v2", ExternalID: "3"})
		if err != nil || !out.Renamed {
			t.Fatalf("RenameSystem = %+v, %v; want the rename applied", out, err)
		}
		assertArtifactAt(t, s, orgID, liveRows.pr, "octo/api-v2#18", domain.PullRequestDedupKey("PR_kwDOlive18"))
		assertArtifactAt(t, s, orgID, liveRows.review, "octo/api-v2#18", domain.ReviewDedupKey("PR_kwDOlive18", liveRows.conversationID))
		assertArtifactAt(t, s, orgID, liveRows.comment, "octo/api-v2#18", domain.GitHubCommentDedupKey(domain.GitHubIssueComment, 502))
		assertArtifactAt(t, s, orgID, liveRows.branch, "octo/api-v2", domain.BranchDedupKey(live.ID, ref))
		assertArtifactAt(t, s, orgID, deadRows.pr, "octo/api#18", domain.PullRequestDedupKey("PR_kwDOdead18"))
		assertArtifactAt(t, s, orgID, deadRows.review, "octo/api#18", domain.ReviewDedupKey("PR_kwDOdead18", deadRows.conversationID))
		assertArtifactAt(t, s, orgID, deadRows.comment, "octo/api#18", domain.GitHubCommentDedupKey(domain.GitHubIssueComment, 501))
		assertArtifactAt(t, s, orgID, deadRows.branch, "octo/api", domain.BranchDedupKey(dead.ID, ref))
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
			OrgID: orgID, KeyKind: domain.PlacementKindRepo, Host: TestGitHubHost, KeyValue: renameNewSlug, Replicas: 9,
		}); err != nil {
			t.Fatalf("seed target placement override: %v", err)
		}
		// Another host's pin on the target slug is no collision: it names a
		// different repository, and the move neither absorbs nor joins it.
		if _, err := s.PlacementOverrides.Upsert(ctx, domain.PlacementOverride{
			OrgID: orgID, KeyKind: domain.PlacementKindRepo, Host: TestOtherGitHubHost, KeyValue: renameNewSlug, Replicas: 5,
		}); err != nil {
			t.Fatalf("seed other-host placement override: %v", err)
		}

		out, err := s.Repos.RenameSystem(ctx, orgID, domain.RepoRef{
			Host:  TestGitHubHost,
			Owner: "octo", Repo: "platform-api", ExternalID: fx.externalID,
		})
		if err != nil {
			t.Fatalf("RenameSystem: %v", err)
		}
		if !out.Renamed {
			t.Fatalf("outcome = %+v, want the rename to go through", out)
		}

		ov, err := s.PlacementOverrides.Get(ctx, orgID, domain.PlacementKindRepo, TestGitHubHost, renameNewSlug)
		if err != nil || ov == nil {
			t.Fatalf("PlacementOverrides.Get(new) = %v, %v; want the moved pin", ov, err)
		}
		if ov.Replicas != 3 {
			t.Errorf("placement replicas = %d, want 3 — the LIVE repository's pin wins, not the orphan's", ov.Replicas)
		}
		if other, _ := s.PlacementOverrides.Get(ctx, orgID, domain.PlacementKindRepo, TestOtherGitHubHost, renameNewSlug); other == nil || other.Replicas != 5 {
			t.Errorf("the other host's pin on %s = %+v; want it untouched", renameNewSlug, other)
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
			Host:  TestGitHubHost,
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
		tracked, err := s.TeamGitHubRepos.ListForTeamSystem(ctx, seed.TeamID, TestGitHubHost)
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
			Host:  TestGitHubHost,
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
		s, orgID, seed := mk(t)
		// Both tracked, so both are bare rows to begin with; the poller's
		// grant enumeration then records an id for exactly one of them.
		if err := TrackRepos(ctx, s.Repos, s.TeamGitHubRepos, orgID, seed.TeamID, TestGitHubHost, []domain.TeamGitHubRepo{
			{Owner: "octo", Repo: "identified"},
			{Owner: "octo", Repo: "bare"},
		}); err != nil {
			t.Fatalf("seed tracked set: %v", err)
		}
		if filled, err := s.Repos.FillMissingExternalIDsSystem(ctx, orgID, []domain.RepoRef{
			{Host: TestGitHubHost, Owner: "octo", Repo: "identified", ExternalID: "1296269"},
		}); err != nil || filled != 1 {
			t.Fatalf("seed identified: filled=%d err=%v", filled, err)
		}

		got, err := s.Repos.ListIdentitiesSystem(ctx, orgID, TestGitHubHost)
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
	repositoryID        string
	prArtifactID        string
	reviewArtifactID    string
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

	if err := TrackRepos(ctx, s.Repos, s.TeamGitHubRepos, orgID, seed.TeamID, TestGitHubHost, []domain.TeamGitHubRepo{
		{Owner: "octo", Repo: "api"},
		{Owner: "octo", Repo: "api-gateway"},
	}); err != nil {
		t.Fatalf("seed tracked set: %v", err)
	}
	// The identity arrives the way it does for a row minted by name: bare,
	// and the poller's grant enumeration fills the id in.
	if filled, err := s.Repos.FillMissingExternalIDsSystem(ctx, orgID, []domain.RepoRef{
		{Host: TestGitHubHost, Owner: "octo", Repo: "api", ExternalID: externalID},
	}); err != nil || filled != 1 {
		t.Fatalf("seed repository identity: filled=%d err=%v", filled, err)
	}
	if err := setBaseBranch(ctx, s.Repos, orgID, renameOldSlug, "release"); err != nil {
		t.Fatalf("seed base branch: %v", err)
	}

	repo, err := s.Repos.GetByRefSystem(ctx, orgID, repoRef(renameOldSlug))
	if err != nil || repo == nil {
		t.Fatalf("resolve %s: got=%v err=%v", renameOldSlug, repo, err)
	}
	conversationID := seed.Conversation(t, suffix)
	if _, _, err := s.ConversationWorktrees.Insert(ctx, orgID, domain.ConversationWorktree{
		ConversationID: conversationID, RepositoryID: repo.ID, Ref: "pr-18",
		Path: "/tmp/wt/" + conversationID + "/octo/api/pr-18",
	}); err != nil {
		t.Fatalf("seed worktree: %v", err)
	}

	entity, _, err := s.Entities.FindOrCreateSystem(ctx, orgID, "github", "https://github.com", renameOldSlug+"#18", renamePRNodeID, "pr", "A pull request",
		"https://github.com/"+renameOldSlug+"/pull/18")
	if err != nil {
		t.Fatalf("seed entity: %v", err)
	}
	if _, _, err := s.Entities.FindOrCreateSystem(ctx, orgID, "github", "https://github.com", renameNeighbourSlug+"#4", renameNeighbourPRNodeID, "pr", "Neighbour PR",
		"https://github.com/"+renameNeighbourSlug+"/pull/4"); err != nil {
		t.Fatalf("seed neighbour entity: %v", err)
	}

	// Two ledger rows: one whose object the rename moves, one for the
	// neighbour as the boundary control. Deterministic dedup keys so the
	// assertions (and the raw column reads) can find them again.
	movedActionKey := "rename-moved-" + suffix
	neighbourActionKey := "rename-neighbour-" + suffix
	if err := s.ExternalActions.RecordSystem(ctx, orgID, domain.ExternalAction{
		TeamID: seed.TeamID, Provider: "github", Scope: TestGitHubHost, Action: domain.ActionPRCreated,
		Target: renameOldSlug + "#18", ExternalID: "18",
		URL:        "https://github.com/" + renameOldSlug + "/pull/18",
		Credential: domain.CredentialGitHubApp, DedupKey: movedActionKey,
		DetailJSON: `{"method":"POST","path":"/repos/` + renameOldSlug + `/pulls"}`,
	}); err != nil {
		t.Fatalf("seed moved action: %v", err)
	}
	if err := s.ExternalActions.RecordSystem(ctx, orgID, domain.ExternalAction{
		TeamID: seed.TeamID, Provider: "github", Scope: TestGitHubHost, Action: domain.ActionPRCreated,
		Target: renameNeighbourSlug + "#4", ExternalID: "4",
		URL:        "https://github.com/" + renameNeighbourSlug + "/pull/4",
		Credential: domain.CredentialGitHubApp, DedupKey: neighbourActionKey,
	}); err != nil {
		t.Fatalf("seed neighbour action: %v", err)
	}

	prArtifact, err := s.Artifacts.UpsertSystem(ctx, orgID, linkArtifactEntity(t, s, orgID, domain.Artifact{
		TeamID: seed.TeamID, Provider: domain.ArtifactProviderGitHub, Kind: domain.ArtifactKindPullRequest, Scope: TestGitHubHost,
		Target: domain.PullRequestTarget(renameOldSlug, 18), ExternalID: "18", NodeID: renamePRNodeID,
		State: domain.ArtifactStatePROpen, DedupKey: domain.PullRequestDedupKey(renamePRNodeID),
	}, renamePRNodeID))
	if err != nil {
		t.Fatalf("seed pr artifact: %v", err)
	}
	review := domain.NewReviewArtifact(renameOldSlug, 18, renamePRNodeID, "head-sha", conversationID)
	review.TeamID, review.Scope = seed.TeamID, TestGitHubHost
	reviewArtifact, err := s.Artifacts.UpsertSystem(ctx, orgID, linkArtifactEntity(t, s, orgID, review, renamePRNodeID))
	if err != nil {
		t.Fatalf("seed review artifact: %v", err)
	}
	branchArtifact, err := s.Artifacts.UpsertSystem(ctx, orgID, domain.Artifact{
		TeamID: seed.TeamID, Provider: domain.ArtifactProviderGit, Kind: domain.ArtifactKindBranch, Scope: TestGitHubHost,
		Target: renameOldSlug, ExternalID: branchRef, State: domain.ArtifactStateBranchPushed,
		DedupKey: domain.BranchDedupKey(repo.ID, branchRef),
	})
	if err != nil {
		t.Fatalf("seed branch artifact: %v", err)
	}
	neighbourArtifact, err := s.Artifacts.UpsertSystem(ctx, orgID, linkArtifactEntity(t, s, orgID, domain.Artifact{
		TeamID: seed.TeamID, Provider: domain.ArtifactProviderGitHub, Kind: domain.ArtifactKindPullRequest, Scope: TestGitHubHost,
		Target: domain.PullRequestTarget(renameNeighbourSlug, 4), ExternalID: "4", NodeID: renameNeighbourPRNodeID,
		State: domain.ArtifactStatePROpen, DedupKey: domain.PullRequestDedupKey(renameNeighbourPRNodeID),
	}, renameNeighbourPRNodeID))
	if err != nil {
		t.Fatalf("seed neighbour artifact: %v", err)
	}

	if _, err := s.PlacementOverrides.Upsert(ctx, domain.PlacementOverride{
		OrgID: orgID, KeyKind: domain.PlacementKindRepo, Host: TestGitHubHost, KeyValue: renameOldSlug, Replicas: 3,
	}); err != nil {
		t.Fatalf("seed placement override: %v", err)
	}
	// key_kind is a free-text column (no FK/CHECK), so an arbitrary other kind
	// sharing the same key_value column proves the rename only rewrites
	// key_kind='repo' rows.
	if _, err := s.PlacementOverrides.Upsert(ctx, domain.PlacementOverride{
		OrgID: orgID, KeyKind: "hostgroup", Host: TestGitHubHost, KeyValue: renameOldSlug, Replicas: 2,
	}); err != nil {
		t.Fatalf("seed other-kind placement override: %v", err)
	}

	return renameFixture{
		externalID:          externalID,
		conversationID:      conversationID,
		entityID:            entity.ID,
		repositoryID:        repo.ID,
		prArtifactID:        prArtifact.ID,
		reviewArtifactID:    reviewArtifact.ID,
		branchArtifactID:    branchArtifact.ID,
		neighbourArtifactID: neighbourArtifact.ID,
		branchRef:           branchRef,
		movedActionKey:      movedActionKey,
		neighbourActionKey:  neighbourActionKey,
	}
}

// repoArtifactRows names the pull request, review, comment and branch
// artifacts recordRepoArtifacts records, and the conversation the review
// belongs to.
type repoArtifactRows struct {
	pr, review, comment, branch string
	conversationID              string
}

// recordRepoArtifacts records, in repo on TestGitHubHost, pull request #number
// (node id nodeID) with its entity, a review draft of it, a comment on it
// (commentID) and a pushed branch ref, keyed and linked to the entity the way
// their writers record them, and returns their ids.
func recordRepoArtifacts(t *testing.T, s db.Stores, orgID, teamID string, repo domain.Repository, number int, nodeID string, commentID int64, ref string) repoArtifactRows {
	t.Helper()
	ctx := context.Background()
	slug := repo.Slug()
	entity, _, err := s.Entities.FindOrCreateSystem(ctx, orgID, "github", TestGitHubHost, domain.PullRequestTarget(slug, number), nodeID, "pr", "PR "+nodeID, "")
	if err != nil {
		t.Fatalf("seed entity for %s: %v", nodeID, err)
	}
	pr := domain.NewPullRequestArtifact(slug, number, nodeID, "fix", "main", "", "t", "b", true)
	pr.TeamID, pr.Scope, pr.EntityID = teamID, TestGitHubHost, entity.ID
	storedPR, err := s.Artifacts.UpsertSystem(ctx, orgID, pr)
	if err != nil {
		t.Fatalf("seed pr artifact %s: %v", nodeID, err)
	}
	conversationID := "conv-" + nodeID
	review := domain.NewReviewArtifact(slug, number, nodeID, "head-sha", conversationID)
	review.TeamID, review.Scope, review.EntityID = teamID, TestGitHubHost, entity.ID
	storedReview, err := s.Artifacts.UpsertSystem(ctx, orgID, review)
	if err != nil {
		t.Fatalf("seed review artifact %s: %v", nodeID, err)
	}
	storedComment, err := s.Artifacts.UpsertSystem(ctx, orgID, domain.Artifact{
		TeamID: teamID, Provider: domain.ArtifactProviderGitHub, Kind: domain.ArtifactKindComment, Scope: TestGitHubHost,
		Target: domain.PullRequestTarget(slug, number), State: domain.ArtifactStateCommentPosted,
		DedupKey: domain.GitHubCommentDedupKey(domain.GitHubIssueComment, commentID), EntityID: entity.ID,
	})
	if err != nil {
		t.Fatalf("seed comment artifact %d: %v", commentID, err)
	}
	branch, ok := domain.NewBranchArtifact(slug, ref, "abc123", true)
	if !ok {
		t.Fatalf("NewBranchArtifact refused %s %s", slug, ref)
	}
	branch.TeamID, branch.Scope = teamID, TestGitHubHost
	branch.DedupKey = domain.BranchDedupKey(repo.ID, ref)
	storedBranch, err := s.Artifacts.UpsertSystem(ctx, orgID, branch)
	if err != nil {
		t.Fatalf("seed branch artifact %s: %v", slug, err)
	}
	return repoArtifactRows{pr: storedPR.ID, review: storedReview.ID, comment: storedComment.ID, branch: storedBranch.ID, conversationID: conversationID}
}

// linkArtifactEntity returns a linked to the entity of its provider carrying
// externalID on its scope, as the exec funnel links an artifact, or unlinked
// when no entity carries it.
func linkArtifactEntity(t *testing.T, s db.Stores, orgID string, a domain.Artifact, externalID string) domain.Artifact {
	t.Helper()
	e, err := s.Entities.GetByExternalIDSystem(context.Background(), orgID, a.Provider, a.Scope, externalID)
	if err != nil {
		t.Fatalf("read the entity carrying %s on %s: %v", externalID, a.Scope, err)
	}
	if e != nil {
		a.EntityID = e.ID
	}
	return a
}

// assertArtifactAt requires artifact id to hold target and dedupKey.
func assertArtifactAt(t *testing.T, s db.Stores, orgID, id, target, dedupKey string) {
	t.Helper()
	got, err := s.Artifacts.Get(context.Background(), orgID, id)
	if err != nil || got == nil {
		t.Fatalf("Artifacts.Get(%s) = %v, %v", id, got, err)
	}
	if got.Target != target || got.DedupKey != dedupKey {
		t.Errorf("artifact %s (%s) = (target %q, key %q), want (%q, %q)", id, got.Kind, got.Target, got.DedupKey, target, dedupKey)
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
