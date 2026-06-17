package server

import (
	"context"
	"errors"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/auth"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/integrations"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// --------------------------------------------------------------------
// Caller GitHub-team membership — the "which of these teams am I on?"
// signal the onboarding wizard uses to pre-check the team admin's own
// GitHub teams in the github-groups mapping step (SKY-411/SKY-413).
//
// This is NOT a standalone endpoint. It rides on
// GET /api/settings/team/{id}/github-groups?include_membership=true (see
// handleTeamGitHubGroupsGet), so the wizard and the Settings editor read
// the *same* org-wide candidate list and differ only in pre-checking —
// the candidate set is no longer sourced from one user's perspective.
//
// Two sourcing paths, branched on deployment mode:
//   - Local (N=1): the org PAT *is* the user, so GET /user/teams answers
//     directly — one paginated call.
//   - Multi: the App installation token authenticates as the app, not a
//     person, so /user/teams can't be used. Instead we take the user's
//     host-verified GitHub login (captured by the Connect OAuth flow) and
//     ask GraphQL's organization.teams(userLogins:) connection per
//     configured-repo owner — O(orgs) queries, not a per-team probe.
//
// The write target is the existing PUT /api/settings/team/{id}/github-groups
// (replace-set, idempotent, team-admin gated).
// --------------------------------------------------------------------

// callerGitHubTeams returns the GitHub teams the requesting user personally
// belongs to, dispatched on deployment mode. Used only to set the Mine flag
// on github-group candidates (annotateGitHubGroupMembership) — best-effort,
// the caller tolerates an error and degrades to "nothing pre-checked."
func (s *Server) callerGitHubTeams(ctx context.Context, orgID, userID string) ([]ghclient.UserTeam, error) {
	if runmode.Current() == runmode.ModeMulti {
		return s.userTeamsMulti(ctx, orgID, userID)
	}
	return s.userTeamsLocal(ctx, orgID, userID)
}

// errNoGitHub signals the local path that no usable PAT is configured —
// surfaced to the caller as a 400, same as handleGitHubRepos.
var errNoGitHub = errors.New("GitHub not configured")

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
			// ErrNoGitHubCredentials just means this owner has no App
			// install and the org has no PAT — expected, skip quietly.
			// Anything else (a secret-store/vault read fault) is a real
			// backend failure worth a log line rather than a silent empty
			// list. Mirrors gitHubGroupCandidates.
			if !errors.Is(err, ghclient.ErrNoGitHubCredentials) {
				membershipLog.Warn("resolve github client failed, skipping owner", "owner", owner, "error", err)
			}
			continue
		}
		teams, err := client.ListUserTeamsInOrg(owner, login)
		if err != nil {
			// The owner may be a user account, or one we lack org-members
			// read on — skip, candidates only.
			membershipLog.Warn("list teams failed, skipping owner", "user", login, "owner", owner, "error", err)
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
