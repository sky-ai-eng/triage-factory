package db

import (
	"context"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// InstallationReposStore owns installation_repositories — the mirror of what
// each GitHub App installation can actually reach.
//
// # What the mirror is for
//
// The set of repositories an installation grants is not otherwise stored
// anywhere: the poller fetches it once per installation per cycle to compute
// the tracked-granted intersection and then discards it. Two states an operator
// needs are therefore underivable from anything TF keeps —
//
//   - REACH WITHOUT PURPOSE: the App can reach a repository no team tracks. TF
//     holds write access to code nobody asked it to touch.
//   - SCOPE DRIFT: a team tracks a repository outside the grant. That repository
//     is silently unpollable.
//
// Both are security findings, not cosmetics, which is why the mirror is
// maintained by pull rather than by webhook: GitHub does not auto-retry webhook
// deliveries at all, so one lost installation_repositories delivery would leave
// a push-only mirror permanently and invisibly wrong. Pull is also the only
// shape that works in local mode, where no delivery ever arrives.
//
// # It is a cache, not a registry
//
// Every row here is rebuilt from GitHub's answer on each successful reconcile
// and survives nothing. That is the opposite of the repositories table, which
// is a registry of TF entities that worktrees, entities, clone state and a
// hand-set base_branch hang off — and it is why grant membership is not a
// column there.
// A grant deliberately contains repositories nobody tracks; a repository row
// means TF works with the repository.
//
// # Who writes it
//
// Two system writers. The reconcile owns the CONTENT — it is the only thing
// that decides which repositories an installation reaches — and
// GitHubAppsStore.MarkInstallationRemoved owns the one write that is not about
// content at all: it deletes an installation's entries in the same transaction
// as the soft removal, because an uninstalled installation reaches nothing and
// the two writes are one fact. Nothing else writes this table.
//
// # Pool split (Postgres)
//
// Admin pool for every method. The RLS policies mirror the installation rows
// these hang off: app-pool members may SELECT, and every app-pool write is
// denied, because there is no user gesture that adds or removes a grant entry —
// that happens on GitHub and TF discovers it. Reads here are org-wide by
// construction (the grant is an org-level fact about the org's own App), which
// is the standing rule's explicitly-annotated system-job arm; the display reads
// a future org page will need are team-scoped through the derived-state queries
// below, both of which are anchored in the org's own tracked set.
//
// SQLite collapses to one connection and one org.
type InstallationReposStore interface {
	// ReplaceForInstallationSystem replaces one installation's mirror with
	// repos, atomically: after it returns, the stored set equals what was passed
	// — including removals, so a repository revoked from a selective grant
	// disappears.
	//
	// It is deliberately ALL-OR-NOTHING and deliberately never called with a
	// partial answer. A reconcile that cannot reach GitHub, or that got a
	// truncated listing, must not call this at all — the previous answer stands.
	// A mirror that silently empties renders reach without purpose as a false
	// all-clear, which is strictly worse than a stale one, and an empty repos
	// slice here is a legitimate state (an installation granted nothing) that
	// the store cannot tell apart from a caller that failed halfway.
	//
	// Rows are keyed on the case-folded slug, so two casings of one repository
	// in the same answer are one row rather than two.
	ReplaceForInstallationSystem(ctx context.Context, orgID, installationID string, repos []domain.InstallationRepository) error

	// ClearForInstallationSystem drops one installation's mirror. This is the
	// soft-removal path — an uninstalled installation grants nothing, and its
	// row lives on only as history. MarkInstallationRemoved does the same delete
	// inline (one transaction with the soft removal, so a `deleted` webhook
	// clears the grant at the moment it arrives); this is the standalone door
	// for a caller holding an installation it knows is gone. A hard delete of
	// the installation row takes the mirror with it through the FK.
	//
	// Not to be confused with a failed reconcile: this is called when the grant
	// is known to be gone, never when it could not be read. Which is exactly why
	// it validates its arguments and returns an error rather than treating a
	// malformed org or an empty installation id as "nothing to clear" — a
	// delete that silently matched no rows would report success for a grant
	// that is still on the page.
	ClearForInstallationSystem(ctx context.Context, orgID, installationID string) error

	// ListForOrgSystem returns every mirrored grant entry across the org's LIVE
	// installations, ordered by (installation, owner, repo). Removed
	// installations contribute nothing — their rows are cleared on removal, and
	// the join re-states that so a row surviving some future write path cannot
	// leak back into a display.
	ListForOrgSystem(ctx context.Context, orgID string) ([]domain.InstallationRepository, error)

	// ListReachWithoutPurposeSystem returns the grant entries no team in the org
	// tracks — repositories the App can reach for no reason TF can name.
	//
	// Tracking is read from team_github_repos (the union across every team),
	// never from repositories: that table is a superset, since get-or-create
	// mints a row for any repository an agent adds to a workspace, and counting
	// those as "tracked" would under-report the finding.
	ListReachWithoutPurposeSystem(ctx context.Context, orgID string) ([]domain.InstallationRepository, error)

	// ListScopeDriftSystem returns the repositories some team tracks that no
	// live installation's mirror contains — tracked, unreachable, and therefore
	// silently unpolled.
	//
	// Gated on the org having a non-empty mirror: an org with no App, or one
	// whose first reconcile has not landed, knows nothing about its grant, and
	// answering "everything you track is drifting" there would be a fabricated
	// finding rather than an unknown one. With no mirror at all the answer is
	// empty.
	ListScopeDriftSystem(ctx context.Context, orgID string) ([]domain.TeamGitHubRepo, error)
}
