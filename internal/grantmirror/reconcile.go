// Package grantmirror keeps TF's picture of a GitHub App's reach correct by
// pull: which installations exist, how wide each one's grant is, and exactly
// which repositories each can touch.
//
// # Why pull, and why this is not a webhook feature
//
// GitHub does not auto-retry webhook deliveries at all. A lost
// `installation_repositories` delivery therefore leaves a push-maintained
// mirror stale permanently and invisibly, and local mode — behind NAT, with no
// delivery path whatsoever — never had one to lose. So the pull is the
// contract and webhooks are a latency optimization on top of it, which is also
// what lets both modes stop needing different truth.
//
// # What it produces
//
// Two states the org page must draw, neither derivable from anything else TF
// stores:
//
//   - REACH WITHOUT PURPOSE — the App can reach a repository no team tracks. TF
//     holds write access to code nobody asked it to touch.
//   - SCOPE DRIFT — a team tracks a repository outside the grant, so that
//     repository is silently unpolled.
//
// # The negative space
//
// A failed or partial fetch NEVER empties the mirror. Every failure arm below
// leaves the previous answer in place, because a mirror that silently empties
// renders reach without purpose as a false all-clear — strictly worse than a
// stale one. That is why a suspended installation is skipped rather than
// reconciled (403s are not evidence of a narrowed grant), why a truncated
// listing is refused rather than written, and why a per-installation failure
// touches nothing at all rather than writing what it managed to read.
//
// The mirror is also never consulted on the token-minting path. It exists for
// display and drift computation; minting continues through the resolver and
// GitHub's own enforcement, because moving the team boundary out of GitHub and
// into TF is exactly what the credential model forbids.
package grantmirror

import (
	"context"
	"fmt"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/github"
)

// grantLister is the half of a resolved GitHub client this package uses. Named
// as an interface so tests can drive the reconcile without an HTTP server, and
// narrow on purpose: the reconcile reads a grant and does nothing else with the
// credential it was handed.
type grantLister interface {
	ListInstallationReposComplete(ctx context.Context) ([]github.UserRepo, bool, error)
}

// clientSource resolves the per-installation client the grant is read through.
// It is satisfied by github.Resolver via resolverSource below.
type clientSource interface {
	grantListerFor(ctx context.Context, orgID, accountLogin string) (grantLister, error)
}

type resolverSource struct{ resolver github.Resolver }

func (s resolverSource) grantListerFor(ctx context.Context, orgID, accountLogin string) (grantLister, error) {
	return s.resolver.ClientFor(ctx, orgID, accountLogin)
}

// classResolver answers which credential class an org's reachable entries are
// keyed under. Satisfied by reachcache.ClassResolver, which is deliberately the
// same object the reads use: writer and reader keying one org differently is a
// mirror that reads as empty for an org that has one.
//
// Declared here rather than imported so this package keeps depending on nothing
// but the stores it writes through — the reachable cache drives this pass, not
// the other way round.
type classResolver interface {
	For(ctx context.Context, orgID string) (domain.GitHubCredentialClass, error)
}

// Reconciler refreshes one org's installation mirror. It is stateless — every
// credential is resolved fresh per pass through the resolver — so a config
// change that hot-swaps credentials is honored without rebuilding it, and one
// instance serves every org.
//
// # Cadence, and the deliberate absence of a TTL
//
// The pass runs once per org per GitHub poll cycle, so the ORG'S OWN POLL
// INTERVAL is the cadence. It carries no TTL of its own, unlike the repo
// profiler whose staleness gate protects a run of LLM calls: this costs one
// GET /app/installations plus one GET /installation/repositories per live
// installation — the second of which the same cycle is about to make anyway to
// compute its tracked-granted intersection. A TTL on top of a poll-driven
// trigger could only make the mirror STALER than the interval the operator
// configured, while adding a second number to reason about when the page says
// "as of".
//
// # The second caller
//
// internal/reachcache also drives this pass — it is the App tier's half of the
// reachable-repo refresh the repository picker and the team-repos write gate
// read — and that caller DOES gate on a TTL, because its other tier is a full
// PAT enumeration and one freshness contract across both tiers is the point.
// Nothing changes here: the TTL lives in the caller, this pass stays the single
// writer of App-tier entries, and running it twice is idempotent by
// construction (a replace, not a merge). It is deliberately the same Reconciler
// instance in both, so the two cadences cannot disagree about what an
// installation reaches.
type Reconciler struct {
	apps    db.GitHubAppsStore
	mirror  db.ReachableReposStore
	clients clientSource
	classes classResolver
}

// NewReconciler builds a Reconciler over the App-installation store, the grant
// mirror, the per-(org, account) GitHub client resolver — App installation token
// in multi, keychain PAT in local, exactly as every other GitHub-facing
// background pass resolves — and the class resolver that says which credential
// system an org's entries are keyed under.
func NewReconciler(apps db.GitHubAppsStore, mirror db.ReachableReposStore, resolver github.Resolver, classes classResolver) *Reconciler {
	return &Reconciler{apps: apps, mirror: mirror, clients: resolverSource{resolver: resolver}, classes: classes}
}

// RunOrg reconciles one org: first that the set of installations TF believes in
// is the set GitHub reports, then what each of those installations can reach.
//
// Existence comes first for a workspace that owns its App key, and for that
// workspace it is not optional. Every other thing that moves the mirror is
// event-shaped — a delivery, the Settings refresh button, the cutover preflight
// — so without a timer an org whose deliveries do not arrive reports zero
// installations, at which point the poll cycle reports degraded and skips
// entirely and the repository picker blanks. This is the timer, in both modes,
// and the call it spends is one the mirror wants regardless: GET
// /app/installations is also the only thing that reports each installation's
// repository_selection.
//
// A workspace on the deployment's shared App takes the second half only: with
// one key serving every workspace, the same call returns other tenants'
// installations, so which of them belong here is a fact the bind asserts and a
// refresh may never invent. The grant half then runs over what that workspace
// has already bound, and creates nothing.
//
// Per-installation failures do not abort the pass — one account's expired
// credential must not stop another account's grant from refreshing — but the
// first is returned once the loop finishes so the failure is visible to the
// caller rather than only to the log.
func (r *Reconciler) RunOrg(ctx context.Context, orgID string) error {
	// The class decides both halves of the preamble below and the key every
	// entry is written under, so it is resolved first and threaded down rather
	// than inferred per site. It is the EFFECTIVE class — an org whose App is
	// registered but not yet active borrows a PAT, and its reach is that PAT's.
	class, err := r.classes.For(ctx, orgID)
	if err != nil {
		return fmt.Errorf("resolve credential class: %w", err)
	}

	if class != domain.GitHubCredentialClassManagedApp {
		// A store failure here is the one thing that stops the pass: without a
		// trustworthy installation set, "GitHub no longer reports this" and "we
		// could not ask" are indistinguishable, and acting on the second as if it
		// were the first is how a mirror empties itself. A no-op for an org with no
		// registered App.
		if err := r.apps.BackfillInstallationsFromAPI(ctx, orgID); err != nil {
			return fmt.Errorf("reconcile installations: %w", err)
		}

		// A registered-but-inactive App is one staged mid PAT→App switch. Its
		// installations must still be discovered — the cutover preflight verifies
		// them before the bit flips, which is why the existence reconcile above has
		// no active gate — but its tier-1 mint is switched off, so every grant read
		// below would resolve to the org PAT and take a guaranteed 403 from an
		// endpoint that accepts installation tokens only. Asking anyway would spend
		// a request per installation per cycle to log an error that says nothing.
		app, err := r.apps.GetForOrgSystem(ctx, orgID)
		if err != nil {
			return fmt.Errorf("read app registration: %w", err)
		}
		if app == nil || !app.Active {
			return nil
		}
	}
	// A managed workspace takes neither of those steps, and the two skips are not
	// the same decision.
	//
	// Skipping the DISCOVERY is the point rather than an optimization: with one
	// key serving every workspace, GET /app/installations returns other tenants'
	// installations, and which of them belong here is a fact only the bind
	// asserts. (The registration read would answer nil regardless — such a
	// workspace holds no org_github_apps row and can hold none, that table being
	// one row per org with a UNIQUE app_id, so N orgs cannot each name the one
	// shared App.)
	//
	// Skipping the REFRESH is not the point, and it leaves a managed workspace's
	// installation set converging on webhook deliveries alone — which GitHub
	// never retries, and which for an account rename do not exist at all. A
	// scoped refresh that may safely run now exists
	// (GitHubAppsStore.RefreshManagedInstallations); what it still needs is a
	// caller shaped for one shared key, since a listing per org per cycle would
	// spend one rate budget N times over.
	// TODO(TFAC-935): refresh the managed installation set from one
	// deployment-wide listing per cycle, fanned out to the orgs that bound each
	// installation.

	// An org on the PAT tier has no grant to mirror at all: its reach is the
	// account enumeration the reachable cache owns, keyed under its own class.
	// Nothing below could write an entry it would ever read.
	if !class.AppTier() {
		return nil
	}

	insts, err := r.apps.ListInstallationsForOrgSystem(ctx, orgID)
	if err != nil {
		return fmt.Errorf("list installations: %w", err)
	}

	var firstErr error
	fail := func(err error) {
		if firstErr == nil {
			firstErr = err
		}
	}
	for _, inst := range insts {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := r.reconcileInstallation(ctx, orgID, class, inst); err != nil {
			grantMirrorLog.ErrorContext(ctx, "reconcile installation grant failed",
				"org", orgID, "installation", inst.InstallationID, "account", inst.AccountLogin, "error", err)
			fail(err)
		}
	}
	return firstErr
}

// reconcileInstallation refreshes one installation's grant, or leaves it
// exactly as it was. There is no third outcome: every early return here is a
// deliberate decision to keep the previous answer, and nothing writes a grant
// it cannot vouch for as complete.
func (r *Reconciler) reconcileInstallation(ctx context.Context, orgID string, class domain.GitHubCredentialClass, inst domain.OrgGitHubAppInstallation) error {
	// A suspended installation refuses every token minted from it, so the grant
	// cannot be read — but the grant still EXISTS, unchanged, and resumes intact
	// on unsuspend. Reconciling it would turn a 403 into "this installation now
	// reaches nothing", which is a false all-clear on the one finding that
	// matters most. Skipped, not failed: nothing here is wrong.
	if inst.Suspended() {
		return nil
	}

	client, err := r.clients.grantListerFor(ctx, orgID, inst.AccountLogin)
	if err != nil {
		return fmt.Errorf("resolve client: %w", err)
	}

	repos, complete, err := client.ListInstallationReposComplete(ctx)
	if err != nil {
		return fmt.Errorf("list installation repositories: %w", err)
	}
	if !complete {
		// A truncated listing read as a whole one makes every repository past
		// the cut look revoked. Refusing to write it costs a stale mirror;
		// writing it manufactures a finding.
		grantMirrorLog.WarnContext(ctx, "installation grant listing incomplete; keeping the previous mirror",
			"org", orgID, "installation", inst.InstallationID, "account", inst.AccountLogin, "read", len(repos))
		return nil
	}

	entries := make([]domain.ReachableRepository, 0, len(repos))
	for _, repo := range repos {
		owner, name, ok := splitFullName(repo.FullName)
		if !ok {
			// A grant entry TF cannot name is one it cannot store, and an answer
			// missing an entry is not the whole grant — which is the mirror's
			// entire contract. Writing the rest would persist a grant narrower
			// than the real one, understating reach and possibly hiding the very
			// finding the table exists for. Same posture as a truncated listing:
			// keep the previous answer, and say why.
			return fmt.Errorf("grant entry %q is not a repository slug; refusing to write a partial grant", repo.FullName)
		}
		entries = append(entries, domain.ReachableRepository{
			OrgID:          orgID,
			InstallationID: inst.InstallationID,
			Source:         domain.RepoSourceGitHub,
			Owner:          owner,
			Repo:           name,
			// The listing returns whole repository objects, so the id and the
			// display fields cost nothing here. The id is what lets a cache entry
			// and a registry row be matched when their slugs momentarily disagree
			// mid-rename; the rest is what the picker renders, which is why it is
			// mirrored rather than re-fetched at read time.
			ExternalID:  repo.ExternalID(),
			Description: repo.Description,
			Language:    repo.Language,
			HTMLURL:     repo.HTMLURL,
			PushedAt:    repo.PushedAt,
			Private:     repo.Private,
		})
	}

	// The org's own class, not a literal: the same installation loop serves a
	// workspace on its own App and one on the deployment's, and the class is what
	// the reads key on.
	return r.mirror.ReplaceForInstallationSystem(ctx, orgID, class, inst.InstallationID, entries)
}

// splitFullName splits GitHub's "owner/repo" into its halves. It refuses
// anything that is not exactly two non-empty segments rather than guessing:
// this string becomes half of a unique key, and a wrong guess would key a grant
// entry the tracked set can never match.
func splitFullName(fullName string) (owner, repo string, ok bool) {
	owner, repo, ok = strings.Cut(fullName, "/")
	if !ok || owner == "" || repo == "" || strings.Contains(repo, "/") {
		return "", "", false
	}
	return owner, repo, true
}
