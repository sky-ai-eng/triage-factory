package server

import (
	"context"
	"errors"
	"log"
	"net/http"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/server/authz"
)

// --------------------------------------------------------------------
// /api/settings/team/{team_id}/github-groups — the GitHub twin of the
// per-team Jira project rules. GET (any team member) returns the team's
// current GitHub-team mappings plus the org's live GitHub teams as
// import-and-choose candidates; PUT (team admin) replace-sets the
// mappings. The {team_id} segment accepts a UUID or the literal
// "default", same as the sibling team-settings routes.
// --------------------------------------------------------------------

// gitHubGroupJSON is the wire shape for one stored mapping row.
type gitHubGroupJSON struct {
	OrgLogin string `json:"org_login"`
	TeamSlug string `json:"team_slug"`
}

// gitHubGroupCandidateJSON is one live GitHub team the admin can assign
// to this TF team. Name + MemberCount ride along for display only — TF
// stores slugs, never names or membership. MemberCount drives the
// "broad team — noisy queue" hint and is best-effort (0/omitted = unknown).
type gitHubGroupCandidateJSON struct {
	OrgLogin    string `json:"org_login"`
	TeamSlug    string `json:"team_slug"`
	Name        string `json:"name"`
	MemberCount int    `json:"member_count,omitempty"`
	// Mine is true when the requesting user personally belongs to this
	// GitHub team. Populated only when the caller asks via
	// ?include_membership=true (the onboarding wizard, to pre-check the
	// admin's own teams); the Settings editor omits the flag and ignores
	// it. Pre-checking is the ONLY onboarding-specific behavior — the
	// candidate set itself is the same org-wide list both surfaces read.
	Mine bool `json:"mine,omitempty"`
}

type teamGitHubGroupsResponse struct {
	Groups     []gitHubGroupJSON          `json:"groups"`
	Candidates []gitHubGroupCandidateJSON `json:"candidates"`
	// Role is the caller's role in this team ("admin"/"member"/""), so
	// the editor can disable Save for non-admins without a second call.
	Role string `json:"role"`
	// CredentialsMissing is true when the team tracks repos but NOT ONE of
	// their owners had a resolvable GitHub credential (no App install and no
	// org PAT). Without this flag an empty candidate list is ambiguous — a
	// genuine "no teams" reads identical to "your GitHub credential is gone."
	// The setup wizard surfaces it as "reconnect GitHub" rather than a
	// silent empty checklist; the Settings editor can do the same. Omitted
	// (false) in the common healthy case.
	CredentialsMissing bool `json:"credentials_missing,omitempty"`
}

func (s *Server) handleTeamGitHubGroupsGet(w http.ResponseWriter, r *http.Request) {
	orgID, ok := s.requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	teamID, err := s.az.ResolveTeamID(r.Context(), orgID, userID, r.PathValue("team_id"))
	if err != nil {
		authz.WriteResolveError(w, "settings/team/github-groups", err)
		return
	}
	if !s.az.VerifyTeamInOrg(w, r, orgID, userID, teamID) {
		return
	}

	// Resolve the caller's role first: candidates are fetched with the
	// org-level GitHub credential, NOT through RLS, so a non-member of
	// this team would otherwise enumerate the org's GitHub team names
	// through it even though the stored mappings (ListForTeam, below) are
	// RLS-empty for them. Gate the fetch on membership — role=="" means
	// the caller isn't on this team, so they get no candidates (and we
	// skip the GitHub API round-trip).
	_, role, err := s.az.TeamMemberCountAndRole(r.Context(), orgID, userID, teamID)
	if err != nil {
		internalError(w, "settings/team/github-groups", err)
		return
	}

	// Import-and-choose: fetch the org's live GitHub teams as candidates
	// (read-only). The deletion-reconcile prune runs in the GitHub poll
	// cycle, not here, so this GET never mutates — stale mappings just
	// show flagged "not found" until the next cycle prunes them.
	//
	// ?include_membership=true (the onboarding wizard) additionally flags
	// the candidates the caller personally belongs to, so the wizard can
	// pre-check the admin's own teams. The Settings editor omits it — same
	// candidate list, no per-user pre-check. Best-effort: a membership
	// fetch failure leaves every Mine=false, never blanks the candidates.
	candidates := []gitHubGroupCandidateJSON{}
	credsMissing := false
	if role != "" {
		candidates, credsMissing = s.gitHubGroupCandidates(r.Context(), orgID, userID)
		if r.URL.Query().Get("include_membership") == "true" {
			s.annotateGitHubGroupMembership(r.Context(), orgID, userID, candidates)
		}
	}

	var groups []domain.TeamGitHubGroup
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		groups, e = tx.TeamGitHubGroups.ListForTeam(r.Context(), teamID)
		return e
	}); err != nil {
		internalError(w, "settings/team/github-groups", err)
		return
	}

	resp := teamGitHubGroupsResponse{
		Groups:             toGitHubGroupJSON(groups),
		Candidates:         candidates,
		Role:               role,
		CredentialsMissing: credsMissing,
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleTeamGitHubGroupsPut(w http.ResponseWriter, r *http.Request) {
	orgID, ok := s.requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	teamID, err := s.az.ResolveTeamID(r.Context(), orgID, userID, r.PathValue("team_id"))
	if err != nil {
		authz.WriteResolveError(w, "settings/team/github-groups", err)
		return
	}
	if !s.az.VerifyTeamInOrg(w, r, orgID, userID, teamID) {
		return
	}
	if !s.az.RequireTeamAdmin(w, r, orgID, userID, teamID) {
		return
	}

	var req struct {
		Groups []gitHubGroupJSON `json:"groups"`
	}
	if !decodeJSON(w, r, &req, "") {
		return
	}

	groups := make([]domain.TeamGitHubGroup, 0, len(req.Groups))
	for _, g := range req.Groups {
		groups = append(groups, domain.TeamGitHubGroup{OrgLogin: g.OrgLogin, TeamSlug: g.TeamSlug})
	}
	// Validate up front so a half-specified group surfaces as a 400 rather
	// than an opaque 500 from inside the tx. SetForTeam re-normalizes
	// idempotently.
	if _, err := domain.NormalizeTeamGitHubGroups(groups); err != nil {
		badRequest(w, err.Error())
		return
	}

	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		return tx.TeamGitHubGroups.SetForTeam(r.Context(), teamID, groups)
	}); err != nil {
		internalError(w, "settings/team/github-groups", err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "saved"})
}

// gitHubGroupCandidates fetches the org's live GitHub teams (one
// GET /orgs/{org}/teams per distinct configured-repo owner) and returns
// them as the flat candidate list the editor presents. Read-only — the
// deletion-reconcile prune runs in the GitHub poll cycle
// (poller.Manager.reconcileGitHubGroups), not here, so this GET stays a
// pure read. Stale mappings (a GitHub team deleted since the last poll)
// still surface in the editor flagged "not found" until the next cycle
// prunes them, and an admin can remove one immediately via the PUT.
//
// Credentials resolve through s.ghResolver.ClientFor, which mints an App
// installation token for the owner when one is installed (tier 1) and
// falls back to the org PAT (tier 3) — so App-only workspaces (no PAT)
// can still import their teams. Owners with no resolvable credential, or
// that the token can't read (a user account, a private org the token
// isn't in), are skipped. Returns nil when nothing is configured / the
// repo list can't be read — the editor degrades to "edit existing
// mappings only."
//
// The second return value (credsMissing) disambiguates the empty-candidate
// case: it is true when the team tracks repos but EVERY owner failed to
// resolve a credential with ErrNoGitHubCredentials — i.e. the org's GitHub
// access is gone, not "these orgs simply have no teams." A single owner that
// did resolve a credential flips it back to false (the access works; that
// owner just had nothing to import). Callers surface credsMissing as a
// reconnect prompt instead of a silent empty list.
func (s *Server) gitHubGroupCandidates(ctx context.Context, orgID, userID string) ([]gitHubGroupCandidateJSON, bool) {
	var repos []domain.RepoProfile
	if err := s.tx.WithTx(ctx, orgID, userID, func(tx db.TxStores) error {
		var e error
		repos, e = tx.Repos.List(ctx, orgID)
		return e
	}); err != nil {
		log.Printf("[github-groups] load configured repos: %v", err)
		return nil, false
	}

	owners := distinctRepoOwners(repos)
	out := []gitHubGroupCandidateJSON{}
	// Track whether the only thing standing between us and candidates is
	// missing credentials. Starts true once there's at least one owner, and
	// is cleared the moment any owner resolves a credential (regardless of
	// whether that owner had teams) — a working credential proves access
	// isn't the problem.
	credsMissing := len(owners) > 0
	for _, owner := range owners {
		client, err := s.ghResolver.ClientFor(ctx, orgID, owner)
		if err != nil {
			// ErrNoGitHubCredentials just means this owner has no App
			// install and the org has no PAT — expected, skip quietly but
			// leave credsMissing set so an all-missing run can surface it.
			if !errors.Is(err, ghclient.ErrNoGitHubCredentials) {
				log.Printf("[github-groups] resolve GitHub client for %s: %v", owner, err)
				// A non-creds resolve failure is an opaque infra error, not a
				// "reconnect GitHub" state — don't claim creds are missing.
				credsMissing = false
			}
			continue
		}
		credsMissing = false
		teams, err := client.ListOrgTeamsDetailed(owner)
		if err != nil {
			// The owner may be a user account (no teams) or one the org
			// credential can't read. Skip — candidates only.
			log.Printf("[github-groups] list teams for GitHub org %s: %v", owner, err)
			continue
		}
		login := strings.ToLower(owner)
		for _, t := range teams {
			if t.TeamSlug == "" {
				continue
			}
			out = append(out, gitHubGroupCandidateJSON{
				OrgLogin:    login,
				TeamSlug:    strings.ToLower(t.TeamSlug),
				Name:        t.Name,
				MemberCount: t.MemberCount,
			})
		}
	}
	return out, credsMissing
}

// annotateGitHubGroupMembership flags each candidate the requesting user
// personally belongs to (Mine=true), so the onboarding wizard can pre-check
// the admin's own GitHub teams. Best-effort by design: a membership-fetch
// failure (no PAT, no host-verified login) leaves every Mine=false and is
// logged, never fatal — the candidate list is the load-bearing part; the
// pre-check is a convenience the user can override.
func (s *Server) annotateGitHubGroupMembership(ctx context.Context, orgID, userID string, candidates []gitHubGroupCandidateJSON) {
	teams, err := s.callerGitHubTeams(ctx, orgID, userID)
	if err != nil {
		// errNoGitHub (local, no PAT) is the expected "can't resolve
		// membership" case — silent. Anything else is worth a log line.
		if !errors.Is(err, errNoGitHub) {
			log.Printf("[github-groups] membership annotate: %v", err)
		}
		return
	}
	mine := make(map[string]bool, len(teams))
	for _, t := range teams {
		mine[strings.ToLower(t.OrgSlug+"/"+t.TeamSlug)] = true
	}
	for i := range candidates {
		if mine[strings.ToLower(candidates[i].OrgLogin+"/"+candidates[i].TeamSlug)] {
			candidates[i].Mine = true
		}
	}
}

// distinctRepoOwners returns the configured repos' owners, lowercased,
// de-duplicated, in first-seen order. These are the GitHub orgs whose
// teams the editor offers as candidates.
func distinctRepoOwners(repos []domain.RepoProfile) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, p := range repos {
		owner := strings.ToLower(strings.TrimSpace(p.Owner))
		if owner == "" || seen[owner] {
			continue
		}
		seen[owner] = true
		out = append(out, owner)
	}
	return out
}

func toGitHubGroupJSON(groups []domain.TeamGitHubGroup) []gitHubGroupJSON {
	out := make([]gitHubGroupJSON, 0, len(groups))
	for _, g := range groups {
		out = append(out, gitHubGroupJSON{OrgLogin: g.OrgLogin, TeamSlug: g.TeamSlug})
	}
	return out
}
