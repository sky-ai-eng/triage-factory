package tracker

import (
	"context"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/domain/events"
)

// ReviewerResolver answers "is this PR reviewer a TF-known identity?" for the
// org being polled. A PR's requested-reviewer list
// (PRSnapshot.RequestedReviewers) mixes individual users (a login) with
// github teams ("org/slug"), each with GitHub's id when the poll read one; the
// tracker builds one resolver per refresh cycle (host resolved once) and the
// diff calls it per newly-added / newly-removed reviewer to decide whether to
// emit a per-reviewer review_requested / review_request_removed event. A
// mention names a user or team by its handle alone, so it asks with no id.
//
// TF-known means: a user that maps to ≥1 TF user on the org's GitHub host
// (the claims-free reverse identity lookup), or a github team that maps to ≥1
// TF team (the github-team→TF-team mapping). Reviewers that aren't TF-known
// produce no event — they'd route to no one.
type ReviewerResolver interface {
	// KnownUser reports whether the user maps to at least one TF user on the
	// org's GitHub host: by GitHub's id for the account when id is not "",
	// and by login when it is.
	KnownUser(id, login string) bool
	// KnownTeam reports whether the github team maps to at least one TF team
	// in the org: by GitHub's id for the team when id is not "", and by its
	// handle (orgLogin + slug) when it is.
	KnownTeam(id, orgLogin, slug string) bool
}

// localReviewerResolver is the local-mode (N=1) path: the only TF-known
// identity is the lone user (their bound GitHub account) plus the teams they
// belong to on GitHub ("org/slug" form, from ListMyTeams). This preserves the
// user-perspective behavior — the general per-reviewer mechanism degenerates
// to one event for the one user. The user is matched by GitHub's id when both
// the request and the binding carry one (domain.GitHubAccount.Is), by login
// otherwise. A team is matched by handle: ListMyTeams is read in the same
// cycle as the request, so the two spell a renamed team the same way. Both
// handle comparisons ignore case, as GitHub does and the store lookups behind
// the multi-mode resolver do: a mention is spelled however its writer typed
// it.
type localReviewerResolver struct {
	self      domain.GitHubAccount
	userTeams map[string]struct{}
}

// NewLocalReviewerResolver builds the N=1 resolver from the lone user's bound
// GitHub account and their "org/slug" team memberships.
func NewLocalReviewerResolver(self domain.GitHubAccount, userTeams []string) ReviewerResolver {
	set := make(map[string]struct{}, len(userTeams))
	for _, t := range userTeams {
		if t != "" {
			set[strings.ToLower(t)] = struct{}{}
		}
	}
	return &localReviewerResolver{self: self, userTeams: set}
}

func (r *localReviewerResolver) KnownUser(id, login string) bool {
	return r.self.Is(login, id)
}

func (r *localReviewerResolver) KnownTeam(_, orgLogin, slug string) bool {
	_, ok := r.userTeams[strings.ToLower(orgLogin+"/"+slug)]
	return ok
}

// storeReviewerResolver is the multi-mode path: TF-known is decided against
// the claims-free store lookups (the reverse id→user_ids or login→user_ids
// lookup for users, the github-team→TF-team mapping for teams). host is the
// org's effective GitHub host (db.EffectiveGitHubHost — the value the
// identity-capture writers stored and the team mappings are keyed under).
type storeReviewerResolver struct {
	// ctx is the poll cycle's context, stored deliberately: the resolver is a
	// short-lived per-cycle object whose KnownUser/KnownTeam calls happen
	// synchronously inside that same cycle. If the cycle is cancelled the
	// store lookups error and resolve to known=false — the conservative
	// outcome (suppress the event rather than mint a mis-routed task).
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

func (r *storeReviewerResolver) KnownUser(id, login string) bool {
	if r.users == nil || (id == "" && login == "") {
		return false
	}
	var (
		ids []string
		err error
	)
	if id != "" {
		ids, err = r.users.UserIDsForGitHubUserIDSystem(r.ctx, r.host, id, login)
	} else {
		ids, err = r.users.UserIDsForGitHubLoginSystem(r.ctx, r.host, login)
	}
	if err != nil {
		trackerLog.Warn("reviewer resolve: user lookup failed", "github_user_id", id, "login", login, "host", r.host, "error", err)
		return false
	}
	return len(ids) > 0
}

func (r *storeReviewerResolver) KnownTeam(id, orgLogin, slug string) bool {
	if r.groups == nil {
		return false
	}
	teams, err := r.groups.TeamsForGitHubTeamSystem(r.ctx, r.orgID, r.host, id, orgLogin, slug)
	if err != nil {
		trackerLog.Warn("reviewer resolve: team lookup failed", "github_team_id", id, "org_login", orgLogin, "slug", slug, "org", r.orgID, "error", err)
		return false
	}
	return len(teams) > 0
}

// --- reviewer identity helpers ---------------------------------------------

// handleReviewer is the reviewer a bare handle names: a mention, which body
// text spells as @login or @org/slug with no id. A slash is the team
// discriminator — GitHub logins cannot contain one.
func handleReviewer(handle string) domain.RequestedReviewer {
	return domain.RequestedReviewer{Handle: handle, Team: strings.Contains(handle, "/")}
}

// splitTeamHandle splits a team reviewer's "org/slug" handle. ok=false for a
// handle that is not one.
func splitTeamHandle(handle string) (orgLogin, slug string, ok bool) {
	i := strings.IndexByte(handle, '/')
	if i <= 0 || i >= len(handle)-1 {
		return "", "", false
	}
	return handle[:i], handle[i+1:], true
}

// resolveReviewer reports whether the reviewer is TF-known via the resolver,
// and on a match returns its handle for event metadata: the login of a user,
// or the "org/slug" of a team — exactly one is non-empty. A nil resolver, an
// empty reviewer, or a non-TF-known identity returns known=false.
func resolveReviewer(resolver ReviewerResolver, r domain.RequestedReviewer) (login, team string, known bool) {
	if resolver == nil || (r.Handle == "" && r.ID == "") {
		return "", "", false
	}
	if r.Team {
		orgLogin, slug, _ := splitTeamHandle(r.Handle)
		if r.ID == "" && slug == "" {
			return "", "", false
		}
		if resolver.KnownTeam(r.ID, orgLogin, slug) {
			return "", r.Handle, true
		}
		return "", "", false
	}
	if resolver.KnownUser(r.ID, r.Handle) {
		return r.Handle, "", true
	}
	return "", "", false
}

// reviewerDedupKey namespaces a reviewer identity into a task dedup_key: by
// GitHub's id when the reviewer carries one, so a rename keeps the key and the
// task it names, and by handle otherwise (a mention, or a request read without
// an id). The value is the GitHub identity, not the internal user-id set it
// resolves to, so the key is stable even when one account maps to several TF
// users. The formats live in the events package so the router can close by
// key without re-deriving the prefix.
func reviewerDedupKey(r domain.RequestedReviewer) string {
	switch {
	case r.Team && r.ID != "":
		return events.ReviewerDedupKeyTeamID(r.ID)
	case r.Team:
		return events.ReviewerDedupKeyTeam(r.Handle)
	case r.ID != "":
		return events.ReviewerDedupKeyUserID(r.ID)
	default:
		return events.ReviewerDedupKeyUser(r.Handle)
	}
}

// reviewerFromDedupKey reverses reviewerDedupKey: an id key gives a reviewer
// with its ID and no handle, a handle key one with its Handle and no ID.
// ok=false for an empty or unprefixed key (a dedup_key="" task from before
// per-reviewer keying) so the per-reviewer reconcile leaves those alone.
func reviewerFromDedupKey(key string) (r domain.RequestedReviewer, ok bool) {
	// Derive the prefixes from the shared key builders (applied to an empty
	// value) so this stays in lockstep with the format if it ever changes.
	if s, found := strings.CutPrefix(key, events.ReviewerDedupKeyUserID("")); found && s != "" {
		return domain.RequestedReviewer{ID: s}, true
	}
	if s, found := strings.CutPrefix(key, events.ReviewerDedupKeyTeamID("")); found && s != "" {
		return domain.RequestedReviewer{ID: s, Team: true}, true
	}
	if s, found := strings.CutPrefix(key, events.ReviewerDedupKeyUser("")); found && s != "" {
		return domain.RequestedReviewer{Handle: s}, true
	}
	if s, found := strings.CutPrefix(key, events.ReviewerDedupKeyTeam("")); found && s != "" {
		return domain.RequestedReviewer{Handle: s, Team: true}, true
	}
	return domain.RequestedReviewer{}, false
}

// requestedIdentityFields spreads a reviewer over the review-request
// metadata's four identity fields: the login or "org/slug" handle, and GitHub's
// id for the user or the team. Fields the reviewer does not carry stay empty.
func requestedIdentityFields(r domain.RequestedReviewer) (login, team, userID, teamID string) {
	if r.Team {
		return "", r.Handle, "", r.ID
	}
	return r.Handle, "", r.ID, ""
}
