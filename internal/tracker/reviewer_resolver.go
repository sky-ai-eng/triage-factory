package tracker

import (
	"context"
	"log"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/db"
)

// ReviewerResolver answers "is this PR reviewer a TF-known identity?" for the
// org being polled. A PR's requested-reviewer list (PRSnapshot.ReviewRequests)
// mixes individual user logins (bare strings) with github teams ("org/slug");
// the tracker builds one resolver per refresh cycle (host resolved once) and
// the diff calls it per newly-added / newly-removed reviewer to decide whether
// to emit a per-reviewer review_requested / review_request_removed event.
//
// TF-known means: a user login that maps to ≥1 TF user on the org's GitHub
// host (the claims-free reverse identity lookup), or a github team that maps
// to ≥1 TF team (the github-team→TF-team mapping). Reviewers that aren't
// TF-known produce no event — they'd route to no one.
type ReviewerResolver interface {
	// KnownUser reports whether login maps to at least one TF user on the
	// org's GitHub host.
	KnownUser(login string) bool
	// KnownTeam reports whether the github team identified by orgLogin +
	// slug maps to at least one TF team in the org.
	KnownTeam(orgLogin, slug string) bool
}

// localReviewerResolver is the local-mode (N=1) path: the only TF-known
// identity is the lone user (their captured login) plus the teams they belong
// to on GitHub ("org/slug" form, from ListMyTeams). This preserves today's
// user-perspective behavior — the general per-reviewer mechanism degenerates
// to one event for the one user.
type localReviewerResolver struct {
	username  string
	userTeams map[string]struct{}
}

// NewLocalReviewerResolver builds the N=1 resolver from the lone user's login
// and their "org/slug" team memberships.
func NewLocalReviewerResolver(username string, userTeams []string) ReviewerResolver {
	set := make(map[string]struct{}, len(userTeams))
	for _, t := range userTeams {
		if t != "" {
			set[t] = struct{}{}
		}
	}
	return &localReviewerResolver{username: username, userTeams: set}
}

func (r *localReviewerResolver) KnownUser(login string) bool {
	return login != "" && login == r.username
}

func (r *localReviewerResolver) KnownTeam(orgLogin, slug string) bool {
	_, ok := r.userTeams[orgLogin+"/"+slug]
	return ok
}

// storeReviewerResolver is the multi-mode path: TF-known is decided against
// the claims-free store lookups (the reverse login→user_ids lookup for users,
// the github-team→TF-team mapping for teams). host is the org's normalized
// GitHub base URL (the same value the identity-capture writers stored); the
// store normalizes again internally so passing the raw org_settings value is
// fine.
type storeReviewerResolver struct {
	ctx    context.Context
	orgID  string
	host   string
	users  db.UsersStore
	groups db.TeamGitHubGroupsStore
}

// NewStoreReviewerResolver builds the multi-mode resolver. host comes from
// org_settings.github_base_url, resolved once per poll cycle.
func NewStoreReviewerResolver(ctx context.Context, orgID, host string, users db.UsersStore, groups db.TeamGitHubGroupsStore) ReviewerResolver {
	return &storeReviewerResolver{ctx: ctx, orgID: orgID, host: host, users: users, groups: groups}
}

func (r *storeReviewerResolver) KnownUser(login string) bool {
	if r.users == nil || login == "" {
		return false
	}
	ids, err := r.users.UserIDsForGitHubLoginSystem(r.ctx, r.host, login)
	if err != nil {
		log.Printf("[tracker] reviewer resolve: user lookup for %q on %q: %v", login, r.host, err)
		return false
	}
	return len(ids) > 0
}

func (r *storeReviewerResolver) KnownTeam(orgLogin, slug string) bool {
	if r.groups == nil {
		return false
	}
	teams, err := r.groups.TeamsForGroupSystem(r.ctx, r.orgID, orgLogin, slug)
	if err != nil {
		log.Printf("[tracker] reviewer resolve: team lookup for %q/%q in org %s: %v", orgLogin, slug, r.orgID, err)
		return false
	}
	return len(teams) > 0
}

// --- reviewer identity helpers ---------------------------------------------

// splitReviewerTeam classifies a PRSnapshot.ReviewRequests entry. The
// reviewRequests GraphQL emits teams in "org/slug" form (matching
// ListMyTeams), individuals as bare logins, so a slash is the discriminator —
// GitHub logins cannot contain one. Returns isTeam=false (and empty parts) for
// an individual-user entry.
func splitReviewerTeam(reviewer string) (orgLogin, slug string, isTeam bool) {
	i := strings.IndexByte(reviewer, '/')
	if i <= 0 || i >= len(reviewer)-1 {
		return "", "", false
	}
	return reviewer[:i], reviewer[i+1:], true
}

// resolveReviewer classifies a reviewer entry and reports whether it is
// TF-known via the resolver. On a match it returns the login (individual
// request) or the team "org/slug" (team request) for event metadata — exactly
// one is non-empty. A nil resolver, empty reviewer, or non-TF-known identity
// returns known=false.
func resolveReviewer(resolver ReviewerResolver, reviewer string) (login, team string, known bool) {
	if resolver == nil || reviewer == "" {
		return "", "", false
	}
	if orgLogin, slug, isTeam := splitReviewerTeam(reviewer); isTeam {
		if resolver.KnownTeam(orgLogin, slug) {
			return "", reviewer, true
		}
		return "", "", false
	}
	if resolver.KnownUser(reviewer) {
		return reviewer, "", true
	}
	return "", "", false
}

// reviewerDedupKey namespaces a reviewer identity so an individual login and a
// team slug can never collide as a task dedup_key. The namespaced value is the
// human-stable handle (login / slug), not the internal user-id set it resolves
// to, so the key is stable even when one login maps to several TF users.
func reviewerDedupKey(reviewer string) string {
	if _, _, isTeam := splitReviewerTeam(reviewer); isTeam {
		return "team:" + reviewer
	}
	return "user:" + reviewer
}

// reviewerFromDedupKey reverses reviewerDedupKey: "user:<login>" /
// "team:<org/slug>" → the bare reviewer handle. ok=false for an empty or
// unprefixed key (a legacy dedup_key="" task from before per-reviewer keying)
// so the per-reviewer reconcile leaves those alone.
func reviewerFromDedupKey(key string) (reviewer string, ok bool) {
	if s, found := strings.CutPrefix(key, "user:"); found {
		return s, true
	}
	if s, found := strings.CutPrefix(key, "team:"); found {
		return s, true
	}
	return "", false
}

// requestedIdentityFields splits a reviewer handle into the (login, team)
// metadata pair — team carries the "org/slug" verbatim, login the bare handle.
func requestedIdentityFields(reviewer string) (login, team string) {
	if _, _, isTeam := splitReviewerTeam(reviewer); isTeam {
		return "", reviewer
	}
	return reviewer, ""
}
