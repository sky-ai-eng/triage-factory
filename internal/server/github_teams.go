package server

import (
	"context"
	"log"
	"net/http"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/auth"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/integrations"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// --------------------------------------------------------------------
// GET /api/github/user-teams — the authenticated user's GitHub teams,
// the candidate list the onboarding GitHub-team mapping step (SKY-411)
// presents so a user who is only review-requested via a team isn't left
// with an empty queue. Distinct from GET .../github-groups (which lists
// the *org's* teams as import candidates): this is "the teams I'm on."
//
// Two sourcing paths, branched on deployment mode:
//
//   - Local (N=1): the org PAT *is* the user, so GET /user/teams answers
//     directly — one paginated call carrying name + member_count.
//   - Multi: the App installation token authenticates as the app, not a
//     person, so /user/teams can't be used. Instead we take the user's
//     host-verified GitHub login (captured by the Connect OAuth flow) and
//     ask GraphQL's organization.teams(userLogins:) connection per
//     configured-repo owner — server-side membership filtering, O(orgs)
//     queries rather than a per-team probe.
//
// The write target is the existing PUT /api/settings/team/{id}/github-groups
// (replace-set, idempotent, team-admin gated) — the wizard POSTs the
// checked teams there. This endpoint is read-only.
// --------------------------------------------------------------------

// userTeamJSON is one of the caller's GitHub teams on the wire.
// MemberCount is best-effort and omitted when unknown (0) — it only
// drives the "broad team — noisy queue" hint, never routing.
type userTeamJSON struct {
	OrgSlug     string `json:"org_slug"`
	TeamSlug    string `json:"team_slug"`
	Name        string `json:"name"`
	MemberCount int    `json:"member_count,omitempty"`
}

func (s *Server) handleGitHubUserTeams(w http.ResponseWriter, r *http.Request) {
	orgID, ok := s.requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject

	var teams []ghclient.UserTeam
	var err error
	if runmode.Current() == runmode.ModeMulti {
		teams, err = s.userTeamsMulti(r.Context(), orgID, userID)
	} else {
		teams, err = s.userTeamsLocal(r.Context(), orgID, userID)
	}
	if err != nil {
		// errNoGitHub is the only client-facing error (a 400, matching
		// handleGitHubRepos). Everything else is an upstream/GitHub fault.
		if err == errNoGitHub {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "GitHub not configured"})
			return
		}
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "failed to fetch teams: " + err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, toUserTeamJSON(teams))
}

// errNoGitHub signals the local path that no usable PAT is configured —
// surfaced to the caller as a 400, same as handleGitHubRepos.
var errNoGitHub = errNoGitHubConfigured{}

type errNoGitHubConfigured struct{}

func (errNoGitHubConfigured) Error() string { return "GitHub not configured" }

// userTeamsLocal sources the lone user's teams via the org PAT and
// GET /user/teams. The PAT authenticates as the user in N=1, so this is
// the same call ListMyTeams makes for the tracker, enriched with name +
// member_count (which the full-team response already carries).
func (s *Server) userTeamsLocal(ctx context.Context, orgID, userID string) ([]ghclient.UserTeam, error) {
	var (
		creds  auth.Credentials
		orgSet domain.OrgSettings
	)
	if err := s.tx.WithTx(ctx, orgID, userID, func(tx db.TxStores) error {
		creds, _ = integrations.Load(ctx, tx.Secrets, orgID)
		var lerr error
		orgSet, lerr = tx.Orgs.GetSettings(ctx, orgID)
		return lerr
	}); err != nil {
		return nil, err
	}
	if creds.GitHubPAT == "" || creds.GitHubURL == "" {
		return nil, errNoGitHub
	}
	baseURL := orgSet.GitHubBaseURL
	if baseURL == "" {
		baseURL = creds.GitHubURL
	}
	return ghclient.NewClient(baseURL, creds.GitHubPAT).ListMyTeamsDetailed()
}

// userTeamsMulti reconstructs the caller's teams in multi mode. The
// Connect OAuth flow persists the user's GitHub *login* (not a reusable
// token), and the org credential is an App installation that isn't a
// person — so /user/teams is out. Instead: resolve the login, then for
// each configured-repo owner ask GraphQL which of that org's teams the
// login belongs to. Per-owner failures (a user-account owner, an org the
// token can't read org-members on) are skipped — best-effort, never
// fatal — so a single unreadable org doesn't blank the whole step.
func (s *Server) userTeamsMulti(ctx context.Context, orgID, userID string) ([]ghclient.UserTeam, error) {
	var (
		login string
		repos []domain.RepoProfile
	)
	if err := s.tx.WithTx(ctx, orgID, userID, func(tx db.TxStores) error {
		orgSet, lerr := tx.Orgs.GetSettings(ctx, orgID)
		if lerr != nil {
			return lerr
		}
		if host, okHost := resolveGitHubHost(orgSet.GitHubBaseURL); okHost {
			login, lerr = tx.Users.GetGitHubLogin(ctx, userID, host)
			if lerr != nil {
				return lerr
			}
		}
		repos, lerr = tx.Repos.List(ctx, orgID)
		return lerr
	}); err != nil {
		return nil, err
	}
	// No host-verified login → nothing to resolve. Degrade to an empty
	// list (the step shows its empty state / "configure later") rather
	// than erroring — the user can still Skip.
	if login == "" {
		return nil, nil
	}

	var out []ghclient.UserTeam
	seen := map[string]struct{}{}
	for _, owner := range distinctRepoOwners(repos) {
		client, err := s.ghResolver.ClientFor(ctx, orgID, owner)
		if err != nil {
			continue
		}
		teams, err := client.ListUserTeamsInOrg(owner, login)
		if err != nil {
			// The owner may be a user account, or one we lack org-members
			// read on — skip, candidates only.
			log.Printf("[user-teams] list teams for %s in org %s: %v", login, owner, err)
			continue
		}
		for _, t := range teams {
			key := strings.ToLower(t.OrgSlug + "/" + t.TeamSlug)
			if _, dup := seen[key]; dup {
				continue
			}
			seen[key] = struct{}{}
			out = append(out, t)
		}
	}
	return out, nil
}

func toUserTeamJSON(teams []ghclient.UserTeam) []userTeamJSON {
	out := make([]userTeamJSON, 0, len(teams))
	for _, t := range teams {
		out = append(out, userTeamJSON{
			OrgSlug:     t.OrgSlug,
			TeamSlug:    t.TeamSlug,
			Name:        t.Name,
			MemberCount: t.MemberCount,
		})
	}
	return out
}
