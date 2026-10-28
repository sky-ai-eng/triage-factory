package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/server/authz"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// --------------------------------------------------------------------
// /api/teams/{team_id}/github-groups — the GitHub twin of the per-team
// Jira project rules. GET (any team member) returns the team's current
// GitHub-team mappings plus the org's live GitHub teams as
// import-and-choose candidates; PUT (team admin) replace-sets the
// mappings, naming each GitHub team by GitHub's id. The {team_id} segment
// accepts a UUID or the literal "default", same as the sibling team
// routes.
// --------------------------------------------------------------------

// gitHubGroupJSON is the wire shape for one stored mapping row.
type gitHubGroupJSON struct {
	// GitHubTeamID is GitHub's id for the team: what the PUT names the
	// mapping by. GitHubOrgID is GitHub's id for the team's organization.
	// Both are "" on a local mapping stored before ids were recorded, until
	// the GitHub poll identifies it from the organization's team listing;
	// the PUT cannot name such a mapping, so a client keeps it by picking
	// its team from the candidates, which carry the ids.
	GitHubTeamID string `json:"github_team_id"`
	GitHubOrgID  string `json:"github_org_id"`
	// OrgLogin and TeamSlug are what GitHub called the organization and the
	// team when the mapping was saved or last reconciled, for display.
	OrgLogin string `json:"org_login"`
	TeamSlug string `json:"team_slug"`
}

// gitHubGroupCandidateJSON is one live GitHub team the admin can assign
// to this TF team, named by GitHub's ids. Name + MemberCount ride along for
// display only — TF stores ids and the current handle, never names or
// membership. MemberCount drives the "broad team — noisy queue" hint and is
// best-effort (0/omitted = unknown).
type gitHubGroupCandidateJSON struct {
	GitHubTeamID string `json:"github_team_id"`
	GitHubOrgID  string `json:"github_org_id"`
	OrgLogin     string `json:"org_login"`
	TeamSlug     string `json:"team_slug"`
	Name         string `json:"name"`
	MemberCount  int    `json:"member_count,omitempty"`
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
		authz.WriteResolveError(w, "teams/github-groups", err)
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
		internalError(w, "teams/github-groups", err)
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
	//
	// The candidates are listed from the org's current GitHub host, and the
	// team's mappings are read on it: an org login on a host the org has left
	// names another organization.
	host, err := s.orgGitHubHost(r.Context(), orgID)
	if err != nil {
		internalError(w, "teams/github-groups", err)
		return
	}
	candidates := []gitHubGroupCandidateJSON{}
	credsMissing := false
	if role != "" {
		// The browse list degrades rather than failing: with the tracked
		// repositories unreadable the editor still edits existing mappings,
		// and an owner whose teams could not be listed just contributes none.
		if listing, err := s.gitHubGroupCandidates(r.Context(), orgID, host); err != nil {
			githubGroupsLog.Error("list github team candidates failed", "error", err)
		} else {
			candidates, credsMissing = listing.candidates, listing.credentialsMissing
		}
		if r.URL.Query().Get("include_membership") == "true" {
			s.annotateGitHubGroupMembership(r.Context(), orgID, userID, candidates)
		}
	}

	var groups []domain.TeamGitHubGroup
	if err := s.tx.WithReadTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		if host == "" {
			return nil // no GitHub URL: no mappings on a host
		}
		var e error
		groups, e = tx.TeamGitHubGroups.ListForTeam(r.Context(), teamID, host)
		return e
	}); err != nil {
		internalError(w, "teams/github-groups", err)
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
		authz.WriteResolveError(w, "teams/github-groups", err)
		return
	}
	if !s.az.VerifyTeamInOrg(w, r, orgID, userID, teamID) {
		return
	}
	// Block writes to an archived team (TFAC-448): team-settings family gates on
	// user_is_team_admin, so the archived filter on user_can_write_team doesn't apply.
	if !s.az.VerifyTeamNotArchived(w, r, orgID, userID, teamID) {
		return
	}
	if !s.az.RequireTeamAdmin(w, r, orgID, userID, teamID) {
		return
	}

	var req teamGitHubGroupsPutRequest
	if !httpx.DecodeJSONStrict(w, r, &req) {
		return
	}
	ids, ok := parseGitHubTeamIDs(w, req.GitHubTeamIDs)
	if !ok {
		return
	}

	// Saved on the org's current GitHub host, which is the host the candidates
	// were listed from; the team's mappings on any other host are kept.
	host, ok := s.requireOrgGitHubHost(r.Context(), w, orgID, "teams/github-groups")
	if !ok {
		return
	}

	// A team the mapping set already holds keeps its stored names and needs no
	// GitHub read: a team the credential can no longer list (App uninstalled,
	// the owner's repositories untracked) must stay in a save, or the editor
	// wedges on it. Only an added team is resolved, against the candidates the
	// editor offers, which carry GitHub's ids and current names.
	var existing []domain.TeamGitHubGroup
	if err := s.tx.WithReadTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		existing, e = tx.TeamGitHubGroups.ListForTeam(r.Context(), teamID, host)
		return e
	}); err != nil {
		internalError(w, "teams/github-groups", err)
		return
	}
	held := make(map[string]domain.TeamGitHubGroup, len(existing))
	for _, g := range existing {
		if g.GitHubTeamID != "" {
			held[g.GitHubTeamID] = g
		}
	}
	var (
		listing gitHubGroupListing
		listed  map[string]gitHubGroupCandidateJSON
		missing []int // indexes into ids of added teams no candidate carries
	)
	groups := make([]domain.TeamGitHubGroup, 0, len(ids))
	for i, id := range ids {
		if g, ok := held[id]; ok {
			groups = append(groups, g)
			continue
		}
		if listed == nil {
			var err error
			if listing, err = s.gitHubGroupCandidates(r.Context(), orgID, host); err != nil {
				internalError(w, "teams/github-groups", err)
				return
			}
			listed = make(map[string]gitHubGroupCandidateJSON, len(listing.candidates))
			for _, c := range listing.candidates {
				listed[c.GitHubTeamID] = c
			}
		}
		c, ok := listed[id]
		if !ok {
			missing = append(missing, i)
			continue
		}
		groups = append(groups, domain.TeamGitHubGroup{
			GitHubTeamID: c.GitHubTeamID, GitHubOrgID: c.GitHubOrgID, OrgLogin: c.OrgLogin, TeamSlug: c.TeamSlug,
		})
	}
	if len(missing) > 0 {
		// A team missing from the list is the caller's mistake only when every
		// owner was listed: one GitHub did not answer for may hold it.
		if len(listing.unlisted) > 0 {
			writeGitHubGroupsUnconfirmed(w, ids, missing, listing.unlisted)
			return
		}
		var refusals httpx.Validation
		for _, i := range missing {
			refusals.Invalid(gitHubTeamIDField(i), gitHubTeamNotListedMessage(ids[i], listing.uncredentialed))
		}
		refusals.Flush(w, http.StatusUnprocessableEntity)
		return
	}

	var stored []domain.TeamGitHubGroup
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		if err := tx.TeamGitHubGroups.SetForTeam(r.Context(), teamID, host, groups); err != nil {
			return err
		}
		var e error
		stored, e = tx.TeamGitHubGroups.ListForTeam(r.Context(), teamID, host)
		return e
	}); err != nil {
		internalError(w, "teams/github-groups", err)
		return
	}

	writeJSON(w, http.StatusOK, teamGitHubGroupsPutResponse{Groups: toGitHubGroupJSON(stored)})
}

// teamGitHubGroupsPutRequest is the desired set. GitHubTeamIDs is a pointer
// because an absent field is not an empty set: a body that forgot it must not
// clear the team's mappings.
type teamGitHubGroupsPutRequest struct {
	GitHubTeamIDs *[]string `json:"github_team_ids"`
}

// teamGitHubGroupsPutResponse echoes the set as stored, in the GET's shape.
type teamGitHubGroupsPutResponse struct {
	Groups []gitHubGroupJSON `json:"groups"`
}

// parseGitHubTeamIDs validates the PUT body's shape: the field present, and
// every entry one of GitHub's numeric ids in canonical form
// (domain.IsGitHubID), named once. Every fault is reported, and it writes the
// 400 on a false return. Whether an id names a team the org can map is the
// handler's next question.
func parseGitHubTeamIDs(w http.ResponseWriter, raw *[]string) ([]string, bool) {
	var v httpx.Validation
	if raw == nil {
		v.Missing("github_team_ids")
		v.Flush(w, http.StatusBadRequest)
		return nil, false
	}
	ids := make([]string, 0, len(*raw))
	first := make(map[string]int, len(*raw))
	for i, id := range *raw {
		field := gitHubTeamIDField(i)
		switch {
		case id == "":
			v.Missing(field)
			continue
		case !domain.IsGitHubID(id):
			v.Invalid(field, id+" is not a GitHub team id")
			continue
		}
		if j, dup := first[id]; dup {
			v.Invalid(field, fmt.Sprintf("GitHub team %s is already named at github_team_ids[%d]", id, j))
			continue
		}
		first[id] = i
		ids = append(ids, id)
	}
	if v.Flush(w, http.StatusBadRequest) {
		return nil, false
	}
	return ids, true
}

func gitHubTeamIDField(i int) string { return fmt.Sprintf("github_team_ids[%d]", i) }

// gitHubGroupListing is the org's live GitHub teams, with what kept the list
// from holding every team of every tracked repository's owner.
type gitHubGroupListing struct {
	candidates []gitHubGroupCandidateJSON
	// unlisted holds the owners whose teams could not be read — a client that
	// failed to resolve for a reason other than a missing credential, or a
	// failed team listing — in owner order. A team absent from candidates may
	// be one of theirs.
	unlisted []unlistedGitHubOwner
	// uncredentialed holds the owners no credential the org has connected
	// reaches (no App installation, no org PAT), in owner order. Their teams
	// cannot be listed with what the org has, which is a fact about its setup
	// rather than a fault.
	uncredentialed []string
	// credentialsMissing is true when the team tracks repos but EVERY owner is
	// uncredentialed — the org's GitHub access is gone, not "these orgs simply
	// have no teams." Callers surface it as a reconnect prompt instead of a
	// silent empty list.
	credentialsMissing bool
}

// unlistedGitHubOwner is an owner whose teams could not be read, and why.
type unlistedGitHubOwner struct {
	owner string
	err   error
}

// gitHubGroupCandidates fetches the org's live GitHub teams on host (one
// organization.teams query per distinct tracked-repo owner) as the flat
// candidate list the editor presents. Read-only — the deletion-reconcile
// prune runs in the GitHub poll cycle (poller.Manager.reconcileGitHubGroups),
// not here, so the GET stays a pure read. Stale mappings (a GitHub team
// deleted since the last poll) still surface in the editor flagged "not
// found" until the next cycle prunes them, and an admin can remove one
// immediately via the PUT.
//
// Credentials resolve through s.ghResolver.ClientFor, which mints an App
// installation token for the owner when one is installed (tier 1) and
// falls back to the org PAT (tier 3) — so App-only orgs (no PAT)
// can still import their teams. An owner that is a user account, or an
// organization the credential cannot see, lists cleanly with no teams.
//
// The error is a store failure, which leaves no list at all. An owner that
// could not be listed is not an error: it is recorded in the listing and its
// teams are left out, so the browse list stays partial while the PUT can tell
// "listed, and the team isn't there" from "could not list".
func (s *Server) gitHubGroupCandidates(ctx context.Context, orgID, host string) (gitHubGroupListing, error) {
	listing := gitHubGroupListing{candidates: []gitHubGroupCandidateJSON{}}
	if host == "" {
		return listing, nil // no GitHub URL: no tracked repos to derive owners from
	}
	// The repositories at least one team in the org tracks on its current
	// host: their owners are the ones the credential below lists teams of.
	// The registry also holds repositories no team tracks, whose owners are
	// not organizations the org works with. Unwindowed: the candidate set is
	// derived from every tracked repository, not browsed, so a page would
	// silently narrow what can be imported. Admin pool, so the list is the
	// same org-wide one whichever team asks (team_github_repos_select would
	// narrow it to the caller's teams); the rows stay here, and only the
	// owners' GitHub teams are returned.
	repos, _, err := s.allStores.Repos.ListTrackedSystem(ctx, orgID, host, db.Unwindowed)
	if err != nil {
		return gitHubGroupListing{}, fmt.Errorf("load tracked repos: %w", err)
	}

	owners := distinctRepoOwners(repos)
	for _, owner := range owners {
		client, err := s.ghResolver.ClientFor(ctx, orgID, owner)
		if errors.Is(err, ghclient.ErrNoGitHubCredentials) {
			// No App install for this owner and no org PAT — expected.
			listing.uncredentialed = append(listing.uncredentialed, owner)
			continue
		}
		if err != nil {
			githubGroupsLog.Warn("resolve github client failed, skipping owner", "owner", owner, "error", err)
			listing.unlisted = append(listing.unlisted, unlistedGitHubOwner{owner: owner, err: err})
			continue
		}
		teams, err := client.ListOrgTeamsDetailed(ctx, owner)
		if err != nil {
			githubGroupsLog.Warn("list teams failed, skipping owner", "owner", owner, "error", err)
			listing.unlisted = append(listing.unlisted, unlistedGitHubOwner{owner: owner, err: err})
			continue
		}
		login := strings.ToLower(owner)
		for _, t := range teams {
			// A mapping is stored by GitHub's ids, so a team listed without
			// them cannot be offered.
			if t.TeamSlug == "" || t.ID == "" || t.OrgID == "" {
				continue
			}
			listing.candidates = append(listing.candidates, gitHubGroupCandidateJSON{
				GitHubTeamID: t.ID,
				GitHubOrgID:  t.OrgID,
				OrgLogin:     login,
				TeamSlug:     strings.ToLower(t.TeamSlug),
				Name:         t.Name,
				MemberCount:  t.MemberCount,
			})
		}
	}
	listing.credentialsMissing = len(owners) > 0 && len(listing.uncredentialed) == len(owners)
	return listing, nil
}

// gitHubTeamNotListedMessage refuses an added team no candidate carries, every
// owner having been listed or having no credential. The owners with no
// credential are named, since connecting one is how their teams become
// addable.
func gitHubTeamNotListedMessage(id string, uncredentialed []string) string {
	msg := "GitHub team " + id + " is not a team of an organization this team's tracked repositories belong to, as this organization's GitHub credentials list them"
	if len(uncredentialed) > 0 {
		msg += "; no GitHub credential connected to this organization reaches " + strings.Join(uncredentialed, ", ") + ", whose teams were not listed"
	}
	return msg
}

// writeGitHubGroupsUnconfirmed answers a PUT whose added teams (ids at the
// missing indexes) were not among the owners that listed, when some owner's
// teams could not be read: the request may well be valid, so it is a 502
// rather than a refusal of the ids. Nothing is stored.
func writeGitHubGroupsUnconfirmed(w http.ResponseWriter, ids []string, missing []int, unlisted []unlistedGitHubOwner) {
	teams := make([]string, len(missing))
	for k, i := range missing {
		teams[k] = ids[i]
	}
	owners := make([]string, len(unlisted))
	for k, u := range unlisted {
		owners[k] = u.owner
	}
	noun := "GitHub team "
	if len(teams) > 1 {
		noun = "GitHub teams "
	}
	httpx.WriteErrors(w, http.StatusBadGateway, httpx.ErrorItem{
		Reason: httpx.ReasonUpstreamUnavailable,
		Message: "could not list the GitHub teams of " + strings.Join(owners, ", ") + ", so " + noun +
			strings.Join(teams, ", ") + " could not be confirmed and nothing was saved" + httpx.LocalDetail(unlisted[0].err),
	})
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
			githubGroupsLog.Warn("membership annotate failed", "error", err)
		}
		return
	}
	mine := make(map[string]bool, len(teams))
	for _, t := range teams {
		mine[t.ID] = t.ID != ""
	}
	for i := range candidates {
		if mine[candidates[i].GitHubTeamID] {
			candidates[i].Mine = true
		}
	}
}

// distinctRepoOwners returns the repos' owners, lowercased, de-duplicated,
// in first-seen order. These are the GitHub orgs whose teams the editor
// offers as candidates.
func distinctRepoOwners(repos []domain.Repository) []string {
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
		out = append(out, gitHubGroupJSON{
			GitHubTeamID: g.GitHubTeamID, GitHubOrgID: g.GitHubOrgID, OrgLogin: g.OrgLogin, TeamSlug: g.TeamSlug,
		})
	}
	return out
}
