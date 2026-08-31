// Package reachcache keeps the reachable-repo mirror current: which
// repositories an org's GitHub credentials can reach, on either credential
// tier, refreshed on a TTL and on an explicit force.
//
// # One freshness contract, both tiers
//
// The repository picker and the team-repos write gate both read
// reachable_repositories, and neither should have to know how the org
// authenticates. A contract that varied by credential class would be the same
// inconsistency as a total_count that varied by credential class — so both tiers
// answer to the same TTL and the same force, and the picker is describable in
// one sentence regardless of tier.
//
// What differs is only the cost behind the contract:
//
//   - The App tier's refresh is effectively free. GET /installation/repositories
//     is already called once per installation per poll cycle to compute the
//     tracked-granted intersection, and grantmirror already mirrors that answer.
//     A refresh here just runs that same pass; in steady state the TTL gate finds
//     the mirror already fresh and does nothing at all.
//   - The PAT tier's refresh is new work — up to ~30 upstream pages on a
//     3,000-repo account — and it is what the TTL is really bounding. The TTL is
//     set in hours, not minutes, because it is the only thing standing between
//     "someone left Settings open" and a full re-enumeration against the
//     rate-limit budget the pollers need.
//
// # Never on the read path
//
// Nothing here runs inside a request. The picker answers from the mirror,
// stale or not, and triggers a refresh out of band; a first-ever open answers a
// readiness discriminator rather than an empty list, so "we have not looked yet"
// never renders as "you have no repositories". A multi-second enumeration inside
// the read is exactly what this package exists to move off it.
//
// # A refusal is also an answer, briefly
//
// A pass that completes and declines to write — a truncated enumeration kept
// out of the mirror rather than mirrored as if whole — arms an in-memory,
// TTL-length gate on further non-forced passes for that org. Without it, a
// mirror that never moves reads as stale to every read, and each read re-kicks
// the very enumeration that just declined: the unbounded loop the TTL exists
// to prevent, charged against the rate-limit budget the pollers need. The gate
// is in memory on purpose — the durable state stays honest (nothing was
// observed), a process restart retries once, and every force bypasses it, so
// the refresh control and a credential change punch through exactly as they
// punch through the TTL.
package reachcache

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/pkg/websocket"
)

// TTL bounds how long a mirror is served before a read kicks a refresh, and how
// long the team-repos write gate trusts it.
//
// Hours, not minutes, and the number is a rate-limit decision rather than a
// freshness preference: a PAT org's refresh is a full re-enumeration of the
// account, and a short TTL turns an idle Settings tab into a poll loop against
// the budget the actual pollers need. Six hours costs at most four
// enumerations a day per org, which is noise beside one poll cycle.
//
// The cost of a stale mirror is bounded and visible on both sides: a repository
// granted since the last refresh is not offered by the picker and is rejected by
// the write gate, and the fix — the picker's explicit refresh control, and the
// forced refresh every credential change fires — is one gesture rather than a
// wait. That is the trade the TTL is making, and it is why nothing here is
// allowed to be the ONLY way the mirror moves.
const TTL = 6 * time.Hour

// grantRefresher is the App tier's refresh, satisfied by
// grantmirror.Reconciler.RunOrg. Named as a function rather than taking the
// package so this one stays testable without an installation fixture, and so
// the dependency reads as what it is: "the App tier's refresh is the grant
// reconcile, unchanged".
type grantRefresher func(ctx context.Context, orgID string) error

// Refresher refreshes one org's reachable mirror. Every credential is resolved
// fresh per pass through the resolver, so a config change that hot-swaps
// credentials is honored without rebuilding it, and one instance serves every
// org. Its only in-process state is the per-org declined-pass stamp, which
// every force bypasses.
type Refresher struct {
	classes  *ClassResolver
	mirror   db.ReachableReposStore
	resolver github.Resolver
	grant    grantRefresher
	ws       *websocket.Hub

	// declined records, per org, when the last pass completed WITHOUT writing —
	// a truncated enumeration refused, or an org with nothing to enumerate
	// against. Such a pass leaves the mirror's stored state exactly as stale as
	// it found it, so without this stamp every subsequent read would re-kick the
	// same enumeration immediately. Guarded by declinedMu: one Refresher is
	// shared across every per-org runner.
	declinedMu sync.Mutex
	declined   map[string]time.Time

	// ttl and now are the seams the TTL tests drive. Defaulted in NewRefresher;
	// nothing in production sets them.
	ttl time.Duration
	now func() time.Time
}

// NewRefresher builds a Refresher over the class resolver, the mirror, the
// GitHub client resolver, and the App tier's grant reconcile. ws may be nil —
// the refresh still writes, it just announces nothing.
func NewRefresher(classes *ClassResolver, mirror db.ReachableReposStore, resolver github.Resolver, grant func(ctx context.Context, orgID string) error, ws *websocket.Hub) *Refresher {
	return &Refresher{
		classes:  classes,
		mirror:   mirror,
		resolver: resolver,
		grant:    grant,
		ws:       ws,
		declined: make(map[string]time.Time),
		ttl:      TTL,
		now:      time.Now,
	}
}

// RunOrg refreshes one org's mirror if it is stale, or if force says to refresh
// regardless. It reports whether it actually wrote, so the caller can skip
// announcing a pass that did nothing.
//
// force is the explicit-refresh path: the picker's refresh control, and every
// credential change (a PAT rotation, an App install, a host repoint). Those move
// which repositories the org can reach WITHOUT moving the timestamp the TTL
// reads, so a mirror that looks fresh is wrong in fact — precisely the case a
// TTL cannot detect and a force must cover.
func (r *Refresher) RunOrg(ctx context.Context, orgID string, force bool) (bool, error) {
	if orgID == "" {
		return false, fmt.Errorf("refresh reachable repositories: empty org")
	}
	// The EFFECTIVE class, not the stored one: an org in the BYO-App system whose
	// App is not active borrows a PAT, and refreshing its grant would write
	// nothing while the picker waited for entries that were never coming.
	class, err := r.classes.For(ctx, orgID)
	if err != nil {
		return false, err
	}

	state, err := r.mirror.ReachableStateSystem(ctx, orgID, class)
	if err != nil {
		return false, fmt.Errorf("read reachable cache state: %w", err)
	}
	if !force && !state.StaleAt(r.now(), r.ttl) {
		return false, nil
	}
	// A pass that completed and declined to write left the stored state exactly
	// as stale as it found it, so the TTL gate above can never absorb the next
	// kick — this one does, for the same window a write would have bought.
	// Non-forced only: everything that changes what the enumeration would
	// return (a credential save, the picker's refresh control) forces.
	if !force && r.recentlyDeclined(orgID) {
		return false, nil
	}

	switch class {
	case domain.GitHubCredentialClassBYOApp, domain.GitHubCredentialClassManagedApp:
		// The App tier's refresh IS the grant reconcile — the same pass the poll
		// cycle runs, with the same never-empty-on-failure posture. Running it
		// here rather than duplicating the enumeration is what keeps the two
		// tiers' answers written by one writer apiece. Both App classes take it:
		// what differs between a workspace on its own App and one on the
		// deployment's is which installations the pass may create, which is the
		// reconcile's own decision and not a second refresh shape.
		if err := r.grant(ctx, orgID); err != nil {
			return false, fmt.Errorf("refresh app grant: %w", err)
		}
	case domain.GitHubCredentialClassPAT:
		refreshed, err := r.refreshPAT(ctx, orgID)
		if err != nil {
			return false, err
		}
		if !refreshed {
			r.markDeclined(orgID)
			return false, nil
		}
	default:
		// The resolver refuses a class this build does not know, so nothing
		// should reach here — and an arm that fell through would announce a
		// refresh that ran no enumeration at all.
		return false, fmt.Errorf("refresh reachable repositories: %w: org=%s class=%q", ErrUnknownCredentialClass, orgID, class)
	}

	r.clearDeclined(orgID)
	r.announce(orgID)
	return true, nil
}

// recentlyDeclined reports whether the last completed pass for orgID declined
// to write within the TTL window. The window is the TTL itself: a declined
// pass is a decision about the same enumeration a write would have dated, so
// it earns the same quiet period — and a stamp can never outlast a later
// successful write's freshness, because the TTL gate answers first for as long
// as that write is fresh. An expired stamp is deleted on the read that finds
// it expired: it has answered its last question.
func (r *Refresher) recentlyDeclined(orgID string) bool {
	r.declinedMu.Lock()
	defer r.declinedMu.Unlock()
	at, ok := r.declined[orgID]
	if !ok {
		return false
	}
	if r.now().Sub(at) >= r.ttl {
		delete(r.declined, orgID)
		return false
	}
	return true
}

// markDeclined stamps orgID's declined pass, and sweeps every expired stamp
// while it holds the lock. The sweep is what keeps the map bounded: entries
// are only ever added here, and an org that never refreshes again never
// revisits its own stamp — so without it, a long-lived process would retain a
// stamp for every org that ever declined, long after those stamps stopped
// answering anything. Pruning at the growth point caps the map at the orgs
// declined within roughly one TTL window.
func (r *Refresher) markDeclined(orgID string) {
	r.declinedMu.Lock()
	defer r.declinedMu.Unlock()
	now := r.now()
	for org, at := range r.declined {
		if now.Sub(at) >= r.ttl {
			delete(r.declined, org)
		}
	}
	r.declined[orgID] = now
}

func (r *Refresher) clearDeclined(orgID string) {
	r.declinedMu.Lock()
	defer r.declinedMu.Unlock()
	delete(r.declined, orgID)
}

// refreshPAT re-enumerates GET /user/repos and replaces the org's pat-class
// entries. It reports false without an error for every outcome that must leave
// the previous answer standing.
func (r *Refresher) refreshPAT(ctx context.Context, orgID string) (bool, error) {
	host, err := r.resolver.BaseURLFor(ctx, orgID)
	if err != nil {
		return false, fmt.Errorf("resolve github host: %w", err)
	}
	if host == "" {
		// No host means nothing has been configured for this org yet. Not an
		// error: the picker answers "not configured" long before it gets here,
		// and a credential-change force on an org that just cleared its GitHub
		// settings lands exactly here.
		return false, nil
	}
	// Target is empty on purpose: this arm only runs for a PAT-class org, which
	// by construction has no active App, so the resolver's App branch cannot be
	// reached and there is no installation for a target to select.
	client, err := r.resolver.ClientFor(ctx, orgID, "")
	if err != nil {
		if errors.Is(err, github.ErrNoGitHubCredentials) {
			reachLog.InfoContext(ctx, "no github credentials; leaving the reachable mirror as it is", "org", orgID)
			return false, nil
		}
		return false, fmt.Errorf("resolve github client: %w", err)
	}

	repos, complete, err := client.ListUserReposComplete(ctx)
	if err != nil {
		return false, fmt.Errorf("list user repositories: %w", err)
	}
	if !complete {
		// A truncated enumeration written as a whole one makes every repository
		// past the cut unselectable in the picker and rejectable by the write
		// gate. Refusing to write it costs a stale mirror; writing it
		// manufactures a wrong answer.
		reachLog.WarnContext(ctx, "user repository listing incomplete; keeping the previous mirror",
			"org", orgID, "read", len(repos))
		return false, nil
	}

	entries := make([]domain.ReachableRepository, 0, len(repos))
	for _, repo := range repos {
		owner, name, ok := splitFullName(repo.FullName)
		if !ok {
			// A repository TF cannot name is one it cannot store, and an answer
			// missing an entry is not the whole reachable set — which is this
			// mirror's entire contract. Same posture as a truncated listing: keep
			// the previous answer, and say why.
			return false, fmt.Errorf("repository %q is not a slug; refusing to write a partial reachable set", repo.FullName)
		}
		entries = append(entries, domain.ReachableRepository{
			OrgID:       orgID,
			Source:      domain.RepoSourceGitHub,
			Owner:       owner,
			Repo:        name,
			ExternalID:  repo.ExternalID(),
			Description: repo.Description,
			Language:    repo.Language,
			HTMLURL:     repo.HTMLURL,
			PushedAt:    repo.PushedAt,
			Private:     repo.Private,
		})
	}
	if err := r.mirror.ReplaceForPATSystem(ctx, orgID, host, entries); err != nil {
		return false, fmt.Errorf("replace reachable repositories: %w", err)
	}
	return true, nil
}

// announce tells the org's open clients the mirror moved. Payload-free and
// org-scoped — the cheap tier: the picker refetches through the REST read, which
// carries whatever scoping that read has, so nothing here has to hand-mirror a
// read policy onto the wire. It is what turns the first-open "discovering
// repositories…" state into a list without the client polling for it.
func (r *Refresher) announce(orgID string) {
	if r.ws == nil {
		return
	}
	r.ws.Broadcast(websocket.Event{Type: "github_repos_updated", OrgID: orgID, Data: map[string]any{}})
}

// splitFullName splits GitHub's "owner/repo" into its halves. It refuses
// anything that is not exactly two non-empty segments rather than guessing:
// this string becomes half of a unique key, and a wrong guess would key an entry
// no selection can ever match.
func splitFullName(fullName string) (owner, repo string, ok bool) {
	owner, repo, ok = strings.Cut(fullName, "/")
	if !ok || owner == "" || repo == "" || strings.Contains(repo, "/") {
		return "", "", false
	}
	return owner, repo, true
}
