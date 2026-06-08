package server

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/auth"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/integrations"
	"github.com/sky-ai-eng/triage-factory/internal/server/authz"
)

// --------------------------------------------------------------------
// /api/settings/team/{team_id}/repos — the per-team GitHub repo
// *tracking* selection (the source of truth that repo_profiles is the
// org-wide UNION of). GET (any team member) returns the team's tracked
// repo slugs; PUT (team admin) replace-sets them and reconciles
// repo_profiles. The {team_id} segment accepts a UUID or the literal
// "default", same as the sibling team-settings routes. The candidate
// list (the repos the user's token can see) is fetched separately via
// GET /api/github/repos, mirroring how the existing repo picker sources
// its options.
// --------------------------------------------------------------------

type teamReposResponse struct {
	// Repos are the team's tracked repos as "owner/repo" slugs.
	Repos []string `json:"repos"`
	// Role is the caller's role in this team ("admin"/"member"/""), so
	// the picker can disable Save for non-admins without a second call.
	Role string `json:"role"`
}

func (s *Server) handleTeamReposGet(w http.ResponseWriter, r *http.Request) {
	orgID, ok := s.requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	teamID, err := s.az.ResolveTeamID(r.Context(), orgID, userID, r.PathValue("team_id"))
	if err != nil {
		authz.WriteResolveError(w, "settings/team/repos", err)
		return
	}
	if !s.az.VerifyTeamInOrg(w, r, orgID, userID, teamID) {
		return
	}

	_, role, err := s.az.TeamMemberCountAndRole(r.Context(), orgID, userID, teamID)
	if err != nil {
		internalError(w, "settings/team/repos", err)
		return
	}

	var repos []domain.TeamGitHubRepo
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		repos, e = tx.TeamGitHubRepos.ListForTeam(r.Context(), teamID)
		return e
	}); err != nil {
		internalError(w, "settings/team/repos", err)
		return
	}

	writeJSON(w, http.StatusOK, teamReposResponse{
		Repos: repoSlugs(repos),
		Role:  role,
	})
}

func (s *Server) handleTeamReposPut(w http.ResponseWriter, r *http.Request) {
	orgID, ok := s.requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	teamID, err := s.az.ResolveTeamID(r.Context(), orgID, userID, r.PathValue("team_id"))
	if err != nil {
		authz.WriteResolveError(w, "settings/team/repos", err)
		return
	}
	if !s.az.VerifyTeamInOrg(w, r, orgID, userID, teamID) {
		return
	}
	if !s.az.RequireTeamAdmin(w, r, orgID, userID, teamID) {
		return
	}

	var req struct {
		Repos []string `json:"repos"`
	}
	if !decodeJSON(w, r, &req, "") {
		return
	}

	// Split + validate up front so a malformed slug surfaces as a 400
	// rather than an opaque 500 from inside the tx. ReplaceForTeam
	// re-normalizes idempotently.
	repos, err := domain.TeamGitHubReposFromSlugs(req.Repos)
	if err != nil {
		badRequest(w, err.Error())
		return
	}

	// Reachability is only checked for repos the team isn't already
	// tracking. A repo that became unreachable *after* being tracked
	// (creds revoked, App uninstalled) must stay removable — rejecting it
	// here would wedge the picker: the stale repo isn't in the reachable
	// option list to uncheck, yet every Save re-submits it, so the user
	// could never save a corrected set. Only *newly added* unreachable
	// repos are a user error worth blocking.
	var existing []domain.TeamGitHubRepo
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		existing, e = tx.TeamGitHubRepos.ListForTeam(r.Context(), teamID)
		return e
	}); err != nil {
		internalError(w, "settings/team/repos", err)
		return
	}
	added := newlyAddedRepos(existing, repos)
	if slug, reject := s.rejectUnreachableRepo(r.Context(), orgID, userID, added); reject {
		badRequest(w, "repo "+slug+" is not reachable by this org's GitHub credentials — install the GitHub App on it or grant the PAT access first")
		return
	}

	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		return tx.TeamGitHubRepos.ReplaceForTeam(r.Context(), orgID, teamID, repos)
	}); err != nil {
		internalError(w, "settings/team/repos", err)
		return
	}

	// Repo set changed → re-profile + restart pollers (the union the
	// poller reads from repo_profiles was just reconciled).
	if s.onGitHubChanged != nil {
		s.MarkJiraRestarted()
		go s.onGitHubChanged(orgID)
	}

	writeJSON(w, http.StatusOK, map[string]any{"status": "saved", "repos": len(repos)})
}

// repoSlugs renders tracked repos as "owner/repo" slugs for the wire.
func repoSlugs(repos []domain.TeamGitHubRepo) []string {
	out := make([]string, 0, len(repos))
	for _, r := range repos {
		out = append(out, r.Slug())
	}
	return out
}

// rejectUnreachableRepo is the write-time guard that keeps a team from
// tracking a repo this deployment's GitHub credentials can't reach (a
// typo'd slug, a stale client, a hand-crafted curl). It validates the
// input repos in two tiers, both bounded by *selection* size rather than
// *org* size (SKY-409):
//
//   - Tier 1 (hot path): the in-process enumeration cache the picker
//     warmed on its way out (handleGitHubRepos → reachableRepoCachePut).
//     A cache hit is a memory lookup — no GitHub call — and the cached
//     enumeration is authoritative for membership, so firstUnreachableRepo
//     decides directly.
//   - Tier 2 (cold path): on a miss (cache expired, never warmed, or
//     evicted by a creds rotation) we probe *only the selected slugs* via
//     per-repo GET /repos/{owner}/{repo}, concurrently, instead of
//     re-enumerating the whole org. See fanOutUnreachableRepo.
//
// Both tiers preserve the long-standing fail-OPEN posture: if we can't
// reach a conclusive "this repo does not exist for us" answer (no
// credentials, a GitHub outage, a 5xx), the write proceeds. That is not
// laxness — a user can't induce a GitHub outage, so every garbage input
// submittable under normal conditions is still hard-rejected, while we
// avoid coupling our write availability to GitHub's uptime. The residual
// (a write accepted while GitHub was unreachable) is caught by the poller,
// which no-ops on a repo it can't reach.
func (s *Server) rejectUnreachableRepo(ctx context.Context, orgID, userID string, repos []domain.TeamGitHubRepo) (string, bool) {
	if len(repos) == 0 {
		return "", false
	}
	// Tier 1: hot path. The cached picker enumeration is the local read
	// the old docstring anticipated — authoritative for membership, so a
	// hit checks against it directly (checked=true).
	if set, ok := s.reachableRepoCacheGet(orgID, userID); ok {
		return firstUnreachableRepo(set, true, repos)
	}
	// Tier 2: cold path. Probe only the selected slugs.
	return s.fanOutUnreachableRepo(ctx, orgID, userID, repos)
}

// newlyAddedRepos returns the entries of desired that are not already in
// existing, matched case-insensitively (GitHub owner/repo identifiers are
// case-insensitive, matching the gate's lower() comparison). These are the
// only repos the reachability guard validates — see handleTeamReposPut.
func newlyAddedRepos(existing, desired []domain.TeamGitHubRepo) []domain.TeamGitHubRepo {
	have := make(map[string]struct{}, len(existing))
	for _, r := range existing {
		have[strings.ToLower(r.Slug())] = struct{}{}
	}
	added := make([]domain.TeamGitHubRepo, 0, len(desired))
	for _, r := range desired {
		if _, ok := have[strings.ToLower(r.Slug())]; !ok {
			added = append(added, r)
		}
	}
	return added
}

// firstUnreachableRepo is the pure policy: given the reachable set and
// whether it's authoritative (checked), return the first repo not in it.
// checked=false → never reject (fail open). Matching is case-insensitive
// because GitHub repo identifiers are; the set is keyed lowercase.
func firstUnreachableRepo(reachable map[string]struct{}, checked bool, repos []domain.TeamGitHubRepo) (string, bool) {
	if !checked {
		return "", false
	}
	for _, r := range repos {
		if _, ok := reachable[strings.ToLower(r.Slug())]; !ok {
			return r.Slug(), true
		}
	}
	return "", false
}

// reachabilityFanOutCap bounds how many per-repo reachability probes run
// concurrently on the cold path. 20 in flight keeps a large selection from
// opening hundreds of sockets at once while still draining the selection in a
// handful of waves (200 repos / 20 → 10 waves).
const reachabilityFanOutCap = 20

// reachabilityCallTimeout caps each per-repo GET. A slow GHES instance must
// not let one probe stall the whole write; a timed-out probe is treated as
// indeterminate (fail open for that slug), same as a 5xx.
const reachabilityCallTimeout = 3 * time.Second

// fanOutUnreachableRepo is the cold-path reachability check: on a cache miss
// it probes only the *selected* slugs (not the whole org) by issuing per-repo
// GET /repos/{owner}/{repo} across every credential tier the deployment has,
// concurrently. It returns the first input-order slug that is *conclusively*
// unreachable by all of them.
//
// "Conclusively" carries the fail-open posture down to the per-slug level: a
// slug is rejected only when at least one credential returned a definitive
// 404/403 and none returned 200; if every credential was indeterminate
// (5xx/timeout) the slug fails open. With no credentials at all there are no
// clients, so nothing is rejected — matching the old reachableRepoSet
// checked=false behavior.
func (s *Server) fanOutUnreachableRepo(ctx context.Context, orgID, userID string, repos []domain.TeamGitHubRepo) (string, bool) {
	clients := s.reachabilityClients(ctx, orgID, userID)
	if len(clients) == 0 {
		// No credentials resolved → fail open (same as a checked=false
		// enumeration). A user can't induce this, so it isn't an escape hatch.
		return "", false
	}
	check := func(ctx context.Context, repo domain.TeamGitHubRepo) (reachable, conclusive bool) {
		return repoReachableByAny(ctx, clients, repo)
	}
	return firstUnreachableViaFanOut(ctx, repos, reachabilityFanOutCap, check)
}

// repoReachableByAny probes one slug across every credential tier, mirroring
// reachableRepoSet's old union semantics one repo at a time: reachable by the
// PAT *or* any App installation counts as reachable. Returns:
//
//   - (true, true)  — some client got a 200.
//   - (false, true) — no client got a 200 and at least one got a definitive
//     404/403 (conclusively unreachable).
//   - (false, false) — every client was indeterminate (5xx/timeout); the
//     caller fails open for this slug.
func repoReachableByAny(ctx context.Context, clients []*ghclient.Client, repo domain.TeamGitHubRepo) (reachable, conclusive bool) {
	anyConclusive := false
	for _, c := range clients {
		callCtx, cancel := context.WithTimeout(ctx, reachabilityCallTimeout)
		r, conc := c.CheckRepoAccess(callCtx, repo.Owner, repo.Repo)
		cancel()
		if r {
			return true, true
		}
		if conc {
			anyConclusive = true
		}
	}
	return false, anyConclusive
}

// reachabilityCheck reports, for a single slug, whether it is reachable and
// whether that answer is conclusive — see CheckRepoAccess. Factored out so the
// fan-out orchestration (firstUnreachableViaFanOut) is a pure function over an
// injectable check, testable without a live GitHub.
type reachabilityCheck func(ctx context.Context, repo domain.TeamGitHubRepo) (reachable, conclusive bool)

// firstUnreachableViaFanOut runs check over every repo concurrently (at most
// cap in flight) and returns the first *input-order* slug that is
// conclusively unreachable (reachable=false AND conclusive=true). Slugs whose
// check is indeterminate (conclusive=false) are never rejected — that is the
// per-slug fail-open posture. Input order is preserved for the returned slug
// (results are indexed, then scanned in order) so the rejection message is
// deterministic regardless of completion order.
func firstUnreachableViaFanOut(ctx context.Context, repos []domain.TeamGitHubRepo, maxInFlight int, check reachabilityCheck) (string, bool) {
	if maxInFlight < 1 {
		maxInFlight = 1
	}
	unreachable := make([]bool, len(repos))
	sem := make(chan struct{}, maxInFlight)
	var wg sync.WaitGroup
	for i, repo := range repos {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, repo domain.TeamGitHubRepo) {
			defer wg.Done()
			defer func() { <-sem }()
			r, conc := check(ctx, repo)
			unreachable[i] = conc && !r
		}(i, repo)
	}
	wg.Wait()
	for i, bad := range unreachable {
		if bad {
			return repos[i].Slug(), true
		}
	}
	return "", false
}

// reachabilityClients builds the GitHub clients the cold-path fan-out probes
// against, one per credential tier the org has — the org PAT plus each App
// installation. It mirrors the credential sourcing the old reachableRepoSet
// did (PAT from integrations.Load, installations via the resolver) but returns
// *clients* rather than enumerating repos through them, because the fan-out
// asks each client about specific slugs instead of listing the world.
//
// Credentials + base URL are read inside WithTx so SecretStore decrypts under
// the caller's claims. A read error or missing credential just yields fewer
// (or zero) clients — the caller fails open when the list is empty.
func (s *Server) reachabilityClients(ctx context.Context, orgID, userID string) []*ghclient.Client {
	var clients []*ghclient.Client

	// Org PAT source (mirrors handleGitHubRepos). Both errors are discarded
	// on purpose: a failed WithTx (DB/secret-store outage) or a failed
	// integrations.Load leaves creds zero-valued, so the PAT client is simply
	// skipped and we fall open — consistent with the function's "fewer/zero
	// clients on error" contract. The org settings error is folded into the
	// WithTx return so a settings miss still drops us to the keychain base URL.
	var (
		creds  auth.Credentials
		orgSet domain.OrgSettings
	)
	_ = s.tx.WithTx(ctx, orgID, userID, func(tx db.TxStores) error {
		creds, _ = integrations.Load(ctx, tx.Secrets, orgID)
		var e error
		orgSet, e = tx.Orgs.GetSettings(ctx, orgID)
		return e
	})
	if creds.GitHubPAT != "" {
		baseURL := orgSet.GitHubBaseURL
		if baseURL == "" {
			baseURL = creds.GitHubURL
		}
		if baseURL != "" {
			clients = append(clients, ghclient.NewClient(baseURL, creds.GitHubPAT))
		}
	}

	// App installation sources, one client per installed account. Each
	// installation token can reach a distinct repo set; probing all of them
	// preserves the old union reachability.
	if s.ghResolver != nil && s.githubApps != nil {
		insts, err := s.githubApps.ListInstallationsForOrgSystem(ctx, orgID)
		if err == nil {
			for _, inst := range insts {
				client, err := s.ghResolver.ClientFor(ctx, orgID, inst.AccountLogin)
				if err != nil {
					continue
				}
				clients = append(clients, client)
			}
		}
	}

	return clients
}
