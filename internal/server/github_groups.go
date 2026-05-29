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
// to this TF team. Name rides along for display only — TF stores slugs,
// never names or membership.
type gitHubGroupCandidateJSON struct {
	OrgLogin string `json:"org_login"`
	TeamSlug string `json:"team_slug"`
	Name     string `json:"name"`
}

type teamGitHubGroupsResponse struct {
	Groups     []gitHubGroupJSON          `json:"groups"`
	Candidates []gitHubGroupCandidateJSON `json:"candidates"`
	// Role is the caller's role in this team ("admin"/"member"/""), so
	// the editor can disable Save for non-admins without a second call.
	Role string `json:"role"`
}

func (s *Server) handleTeamGitHubGroupsGet(w http.ResponseWriter, r *http.Request) {
	orgID, ok := s.requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	teamID, err := s.resolveTeamID(r.Context(), orgID, userID, r.PathValue("team_id"))
	if err != nil {
		writeResolveError(w, "settings/team/github-groups", err)
		return
	}
	if !s.verifyTeamInOrg(w, r, orgID, userID, teamID) {
		return
	}

	// Import-and-choose + deletion-reconcile floor: fetch the org's live
	// GitHub teams as candidates and prune mapping rows pointing at teams
	// that no longer exist (the lifecycle the ticket specs on editor
	// open). The fetch goes through the single org-level credential — App
	// installation token or the org's PAT, never a per-user token — so
	// it's a deterministic identity: every member's open sees the same
	// team set, and a mapping can only have been created for a team that
	// identity can see, so a present team is never wrongly pruned. The
	// one ambiguous case (an empty fetch — org cred is a user account, or
	// transient zero-visibility) is guarded against in
	// gitHubGroupCandidates so it never nukes the whole org's mappings.
	candidates := s.gitHubGroupCandidates(r.Context(), orgID, userID)

	var groups []domain.TeamGitHubGroup
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		groups, e = tx.TeamGitHubGroups.ListForTeam(r.Context(), teamID)
		return e
	}); err != nil {
		internalError(w, "settings/team/github-groups", err)
		return
	}

	_, role, err := s.teamMemberCountAndRole(r.Context(), orgID, userID, teamID)
	if err != nil {
		internalError(w, "settings/team/github-groups", err)
		return
	}

	resp := teamGitHubGroupsResponse{
		Groups:     toGitHubGroupJSON(groups),
		Candidates: candidates,
		Role:       role,
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleTeamGitHubGroupsPut(w http.ResponseWriter, r *http.Request) {
	orgID, ok := s.requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	teamID, err := s.resolveTeamID(r.Context(), orgID, userID, r.PathValue("team_id"))
	if err != nil {
		writeResolveError(w, "settings/team/github-groups", err)
		return
	}
	if !s.verifyTeamInOrg(w, r, orgID, userID, teamID) {
		return
	}
	if !s.requireTeamAdmin(w, r, orgID, userID, teamID) {
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
// GET /orgs/{org}/teams per distinct configured-repo owner), returns
// them as the flat candidate list the editor presents, and runs the
// deletion-reconcile floor — pruning mapping rows for teams the fetch
// no longer reports.
//
// Credentials resolve through s.ghResolver.ClientFor, which mints an App
// installation token for the owner when one is installed (tier 1) and
// falls back to the org PAT (tier 3) — so App-only workspaces (no PAT)
// can still import their teams. This is always the single org-level
// identity, never a per-user token, which is what makes the prune safe:
// the visibility is deterministic across all members, and the editor can
// only create mappings for teams that identity can see.
//
// Owners with no resolvable credential, or that the token can't read (a
// user account, a private org the token isn't in), are skipped — and
// crucially their mappings are NOT pruned, since a fetch failure must
// never read as "all teams deleted." Returns nil when nothing is
// configured / the repo list can't be read — the editor degrades to
// "edit existing mappings only."
func (s *Server) gitHubGroupCandidates(ctx context.Context, orgID, userID string) []gitHubGroupCandidateJSON {
	var repos []domain.RepoProfile
	if err := s.tx.WithTx(ctx, orgID, userID, func(tx db.TxStores) error {
		var e error
		repos, e = tx.Repos.List(ctx, orgID)
		return e
	}); err != nil {
		log.Printf("[github-groups] load configured repos: %v", err)
		return nil
	}

	out := []gitHubGroupCandidateJSON{}
	for _, owner := range distinctRepoOwners(repos) {
		client, err := s.ghResolver.ClientFor(ctx, orgID, owner)
		if err != nil {
			// ErrNoGitHubCredentials just means this owner has no App
			// install and the org has no PAT — expected, skip quietly.
			if !errors.Is(err, ghclient.ErrNoGitHubCredentials) {
				log.Printf("[github-groups] resolve GitHub client for %s: %v", owner, err)
			}
			continue
		}
		teams, err := client.ListOrgTeams(owner)
		if err != nil {
			// The owner may be a user account (no teams) or one the org
			// credential can't read. Skip — do NOT prune on a fetch error.
			log.Printf("[github-groups] list teams for GitHub org %s: %v", owner, err)
			continue
		}
		login := strings.ToLower(owner)
		slugs := make([]string, 0, len(teams))
		for _, t := range teams {
			if t.Slug == "" {
				continue
			}
			slugs = append(slugs, t.Slug)
			out = append(out, gitHubGroupCandidateJSON{
				OrgLogin: login,
				TeamSlug: strings.ToLower(t.Slug),
				Name:     t.Name,
			})
		}
		// Reconcile floor: drop mappings under this org login whose GitHub
		// team is gone. Guard the empty case — a zero-length fetch is the
		// one genuinely-ambiguous result (org cred is a user account, or
		// can see no teams), where "prune everything for this org" would
		// be wrong. A non-empty list is the org credential's deterministic
		// view, so a missing slug is a real deletion. Authoritative
		// real-time cleanup (the team:deleted webhook) is the optional
		// promptness layer on top.
		if len(slugs) == 0 {
			continue
		}
		if n, err := s.githubGroups.PruneMissingSystem(ctx, orgID, owner, slugs); err != nil {
			log.Printf("[github-groups] reconcile prune for GitHub org %s: %v", owner, err)
		} else if n > 0 {
			log.Printf("[github-groups] reconcile: pruned %d stale mapping(s) for deleted GitHub teams under %s", n, owner)
		}
	}
	return out
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
