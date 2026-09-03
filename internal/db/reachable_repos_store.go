package db

import (
	"context"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// ReachableReposStore owns reachable_repositories — the mirror of which
// repositories an org's GitHub credentials can actually reach, populated by
// every credential class and read by every consumer of that question.
//
// # What the mirror is for
//
// Three things, and they used to be three different mechanisms:
//
//   - THE PICKER. POST /api/github/repos/list proxied GET /user/repos on a PAT
//     org and GET /installation/repositories on an App org, which is why it was
//     the one list on the surface that could not report a total — for two
//     different reasons per tier. Served from here it is an ordinary
//     store-backed list with a real count and a server-side filter.
//   - THE WRITE GATE. The team-repos PUT validates a selection against what the
//     org can reach; it used to keep its own in-process, per-(org, user) map,
//     warmed as a side effect of the picker read, which died on restart and, in
//     multi mode, lived on whichever pod happened to serve the picker rather
//     than the one that later served the write.
//   - THE TWO APP-TIER FINDINGS, which are derivable from nothing else TF
//     stores:
//     REACH WITHOUT PURPOSE — the App can reach a repository no team tracks. TF
//     holds write access to code nobody asked it to touch.
//     SCOPE DRIFT — a team tracks a repository outside the grant. That
//     repository is silently unpolled.
//
// Both findings are security findings, not cosmetics, which is why the App half
// is maintained by pull rather than by webhook: GitHub does not auto-retry
// webhook deliveries at all, so one lost installation_repositories delivery
// would leave a push-only mirror permanently and invisibly wrong. Pull is also
// the only shape that works in local mode, where no delivery ever arrives.
//
// # It is a cache, not a registry
//
// Every row here is rebuilt from GitHub's answer on each successful refresh and
// survives nothing. That is the opposite of the repositories table, which is a
// registry of TF entities that worktrees, entities, clone state and a hand-set
// base_branch hang off — and it is why reachability is not a column there. A
// reachable entry deliberately includes repositories nobody tracks; a repository
// row means TF works with the repository.
//
// # Scoping, and why every read takes a class
//
// A row belongs to a credential CLASS — 'pat', or one of the App classes
// ('byo_app', 'managed_app') — and, within it, to a SCOPE: the installation for
// an App entry, the host for a PAT entry, which is the unit one refresh replaces
// atomically. Reads take the org's CURRENT class so an org that switched tiers is
// never served the reach it used to have; the previous tier's rows are inert
// until their next refresh replaces them, and they cost a row apiece until then.
//
// # Pool split (Postgres)
//
// Admin pool for every method. The RLS policies mirror the installation rows the
// App half hangs off: app-pool members may SELECT, and every app-pool write is
// denied, because there is no user gesture that adds or removes a reachable
// entry — that happens on GitHub and TF discovers it. Reads here are org-wide by
// construction and that is the standing rule's explicitly-annotated system-job
// arm rather than a gap: reachability is a fact about the ORG'S OWN credential,
// identical for every member of the org, and it is the same set the picker
// proxied straight from GitHub before this table existed. The team-scoped
// display reads a future org page needs are the derived-state queries below,
// both anchored in the org's own tracked set.
//
// SQLite collapses to one connection and one org.
type ReachableReposStore interface {
	// ReplaceForInstallationSystem replaces one installation's App-tier entries
	// with repos, atomically: after it returns, the stored set for that scope
	// equals what was passed — including removals, so a repository revoked from a
	// selective grant disappears. The same transaction records that this scope
	// was refreshed, which is what makes a grant of nothing distinguishable from
	// a refresh that never ran.
	//
	// class is the App-tier class the org's entries are keyed under — its own
	// registration or the deployment's shared App — and it is a parameter rather
	// than a value read off the rows because the scope marker carries it too:
	// reachable_scopes is keyed (org_id, credential_class, scope), and a grant of
	// nothing has no row to read a class from. Marking that scope under the wrong
	// class would leave an org whose grant is legitimately empty reading as
	// never-refreshed forever. Rows are stamped from it, so a caller cannot hand
	// one replace two classes' worth of entries.
	//
	// It is deliberately ALL-OR-NOTHING and deliberately never called with a
	// partial answer. A refresh that cannot reach GitHub, or that got a truncated
	// listing, must not call this at all — the previous answer stands. A mirror
	// that silently empties renders reach without purpose as a false all-clear,
	// which is strictly worse than a stale one, and an empty repos slice here is
	// a legitimate state (an installation granted nothing) that the store cannot
	// tell apart from a caller that failed halfway.
	//
	// Rows are keyed on the case-folded slug, so two casings of one repository in
	// the same answer are one row rather than two.
	//
	// Exempt from the returned-row rule: it reconciles a whole cache set in
	// one transaction, so there is no single row a return value could name.
	ReplaceForInstallationSystem(ctx context.Context, orgID string, class domain.GitHubCredentialClass, installationID string, repos []domain.ReachableRepository) error

	// ReplaceForPATSystem is the PAT tier's twin: it replaces the org's ENTIRE
	// pat-class set with repos observed against host.
	//
	// Whole-class rather than per-host, unlike the App tier's per-installation
	// scope, because an org authenticates against exactly one GitHub deployment
	// at a time. Rows left behind by a previous GitHubBaseURL are not a second
	// scope to keep — they are the old deployment's repositories, and serving
	// them beside the new host's would be answering with a reach the org does not
	// have.
	//
	// Same all-or-nothing contract as the App tier, and for the same reason: a
	// partial enumeration written as a whole one makes every repository past the
	// cut look unreachable, which the write gate would then reject.
	//
	// Exempt from the returned-row rule: it reconciles a whole cache set, same
	// as ReplaceForInstallationSystem.
	ReplaceForPATSystem(ctx context.Context, orgID, host string, repos []domain.ReachableRepository) error

	// ClearForInstallationSystem drops one installation's entries. This is the
	// soft-removal path — an uninstalled installation reaches nothing, and its
	// row lives on only as history. MarkInstallationRemoved does the same delete
	// inline (one transaction with the soft removal, so a `deleted` webhook
	// clears the reach at the moment it arrives); this is the standalone door for
	// a caller holding an installation it knows is gone. A hard delete of the
	// installation row takes its entries with it through the FK.
	//
	// Not to be confused with a failed refresh: this is called when the reach is
	// known to be gone, never when it could not be read. Which is exactly why it
	// validates its arguments and returns an error rather than treating a
	// malformed org or an empty installation id as "nothing to clear" — a delete
	// that silently matched no rows would report success for a grant that is
	// still on the page.
	//
	// It takes no class, unlike the replace beside it. An installation that is
	// gone reaches nothing under any class, and only the App classes carry an
	// installation at all, so the delete is addressed by installation and clears
	// whichever class observed it — the same delete MarkInstallationRemoved
	// writes inline, which has no class to be told either.
	//
	// Exempt from the returned-row rule: it clears a set, so there is no
	// single row a return value could name.
	ClearForInstallationSystem(ctx context.Context, orgID, installationID string) error

	// ListForOrgSystem returns every entry across the org's LIVE installations
	// under class, ordered by (installation, owner, repo). Removed installations
	// contribute nothing — their rows are cleared on removal, and the join
	// re-states that so a row surviving some future write path cannot leak back
	// into a display.
	//
	// class is the org's current App-tier class — its own registration or the
	// deployment's shared App — and a read takes it rather than matching every
	// App class at once for the reason every read here takes one: an org that
	// left one class for the other must never be served the reach it used to
	// have. The PAT class is refused: a PAT entry is not a grant entry, and a
	// caller asking for "the grant" of a PAT org has already gone wrong.
	ListForOrgSystem(ctx context.Context, orgID string, class domain.GitHubCredentialClass) ([]domain.ReachableRepository, error)

	// ListReachWithoutPurposeSystem returns one page of the entries no team in
	// the org tracks — repositories the App can reach for no reason TF can name —
	// with the total across every page. Same class rule as ListForOrgSystem.
	//
	// Tracking is read from team_github_repos (the union across every team),
	// never from repositories: that table is a superset, since get-or-create
	// mints a row for any repository an agent adds to a workspace, and counting
	// those as "tracked" would under-report the finding.
	//
	// Ordered by (installation, folded owner, folded repo), which the per-class
	// unique index makes a total order, so offset paging cannot drop or repeat a
	// row.
	ListReachWithoutPurposeSystem(ctx context.Context, orgID string, class domain.GitHubCredentialClass, opts ListOpts) ([]domain.ReachableRepository, int, error)

	// ListScopeDriftSystem returns one page of the repositories some team tracks
	// that no live installation's grant contains — tracked, unreachable, and
	// therefore silently unpolled — with the total across every page. Same
	// class rule as ListForOrgSystem.
	//
	// Three things keep it a finding rather than a fabrication:
	//
	//   - It is gated on the org having refreshed at least one scope under
	//     class. An org with no App, or one whose first refresh has not landed,
	//     knows nothing about its grant, and "not in the mirror" then describes
	//     everything it tracks. The gate reads the scope markers, not the entry
	//     rows, so a selective grant that genuinely contains nothing still
	//     reports the repositories tracked against it.
	//   - A repository whose owner account holds a live installation granting
	//     EVERY repository is never reported: that grant reaches everything the
	//     account owns, so a tracked repository there cannot sit outside it,
	//     and its absence from the mirror is staleness, not drift.
	//   - A repository whose owner account holds a live installation of unknown
	//     width is not reported either. Unknown is not "no"; it is "not learned
	//     yet", and the caller reads that off the installation.
	//
	// What remains is a tracked repository under a selective grant that does not
	// contain it, or on an account no live installation covers at all — each
	// naming the covering installation when there is one, so the finding can
	// point at the page where the grant is widened. Ordered by folded slug.
	ListScopeDriftSystem(ctx context.Context, orgID string, class domain.GitHubCredentialClass, opts ListOpts) ([]domain.ScopeDriftRepository, int, error)

	// ListReachableSystem is the picker's read: one page of the org's reachable
	// set for class, ordered by folded slug, with the filtered total.
	//
	// q is a case-insensitive substring matched against the slug, the description
	// and the language — the three fields the picker used to filter on in the
	// browser, moved server-side so the surface stops needing the whole set in
	// memory to search it. An empty q matches everything.
	//
	// The order is the folded slug and nothing else, which is a total order (the
	// per-class unique index proves no two rows tie on it) — so offset paging
	// cannot drop or repeat a row. It is deliberately not "most recently pushed",
	// which is what the PAT proxy inherited from GET /user/repos: that order ties
	// freely, needs a tiebreaker anyway, and is the wrong affordance for a list
	// with a search box over it.
	ListReachableSystem(ctx context.Context, orgID string, class domain.GitHubCredentialClass, q string, opts ListOpts) ([]domain.ReachableRepository, int, error)

	// ReachableStateSystem reports whether the org's cache for class has ever been
	// refreshed, how many entries it holds, and how old its stalest scope is —
	// without reading a row of the cache itself. It is what the refresh TTL gates
	// on, what the picker's readiness discriminator answers off, and what the
	// write gate consults before deciding to trust the cache.
	//
	// "Refreshed" and "has entries" are separate answers, and separating them is
	// the point: a scope that genuinely reaches nothing writes zero entries,
	// exactly like a refresh that never ran, so a count alone would leave an org
	// with an empty grant reading as un-looked-at forever — discovering in the
	// picker, and re-enumerating on every open. That is what the scope rows
	// (reachable_scopes) record, and it is the only thing they record.
	ReachableStateSystem(ctx context.Context, orgID string, class domain.GitHubCredentialClass) (domain.ReachableCacheState, error)

	// ReachableSlugsSystem returns which of slugs the org can reach under class,
	// as a set keyed by the LOWERCASED "owner/repo" — the shape the write gate's
	// membership check wants, since GitHub identifiers are case-insensitive.
	//
	// Bounded by the caller's selection rather than by org size: it asks about
	// the slugs it holds instead of enumerating the org to intersect afterwards.
	// An empty slugs argument returns an empty set without a query.
	ReachableSlugsSystem(ctx context.Context, orgID string, class domain.GitHubCredentialClass, slugs []string) (map[string]struct{}, error)
}
