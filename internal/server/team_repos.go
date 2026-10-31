package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/sky-ai-eng/triage-factory/internal/auth"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/github/reachcache"
	"github.com/sky-ai-eng/triage-factory/internal/integrations"
	"github.com/sky-ai-eng/triage-factory/internal/server/authz"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// --------------------------------------------------------------------
// /api/teams/{team_id}/github-repos — the per-team GitHub repo
// *tracking* selection, and the source of truth for what TF polls and
// profiles. GET (any team member) returns the team's tracked repositories; PUT
// (team admin) replace-sets them. Untracking removes the tracking row alone:
// the registry row survives whatever still names it. The {team_id} segment
// resolves through authz.ResolveTeamID — a UUID, plus the literal "default"
// in local mode — same as the sibling team routes.
//
// **The set is named by registry id, never by name.** The GET hands each
// tracked repository back with its registry id, its GitHub id, its current
// owner/repo and whether it is retired, and the PUT takes the registry ids alone — so a rename on GitHub
// between a client's read and its save cannot make the body name a repository
// by a name it no longer has. A repository the registry holds no row for yet
// is one the picker offers by GitHub id (POST /api/github/repos/list): the
// client registers it by that id (POST /api/repos), and tracks the registry id
// that returns.
//
// A child collection of the team, named github-repos rather than repos: it
// sits beside jira-projects and github-groups, and the bare word would read
// as the top-level /api/repos registry, which is a different resource.
// --------------------------------------------------------------------

// trackedRepoJSON is one tracked repository on the wire.
type trackedRepoJSON struct {
	// ID is the registry row id: the handle the PUT names it by.
	ID string `json:"id"`
	// GitHubID is GitHub's own repository id, "" while TF has not learned it
	// for this row. A client matches picker candidates against it.
	GitHubID string `json:"github_id"`
	// Owner and Repo are the repository's current name as TF stores it, for
	// display.
	Owner string `json:"owner"`
	Repo  string `json:"repo"`
	// RetiredAt is when TF retired the registry row, null while it holds its
	// name. A retired row is a repository GitHub no longer serves to the org:
	// its name went to another repository, which is a different entry with an
	// id of its own. The team keeps tracking the retired row until a save
	// drops it, and nothing polls it.
	RetiredAt *string `json:"retired_at"`
}

type teamReposResponse struct {
	Repos []trackedRepoJSON `json:"repos"`
	// Role is the caller's role in this team ("admin"/"member"/""), so
	// the picker can disable Save for non-admins without a second call.
	Role string `json:"role"`
}

// teamReposPutRequest is the desired set. RepositoryIDs is a pointer because
// an absent field is not an empty set: a body that forgot it must not clear
// the team's tracking.
type teamReposPutRequest struct {
	RepositoryIDs *[]string `json:"repository_ids"`
}

// teamReposPutResponse echoes the set as stored, in the GET's shape, so a
// client renders the current names rather than what it last read.
type teamReposPutResponse struct {
	Repos []trackedRepoJSON `json:"repos"`
}

func (s *Server) handleTeamReposGet(w http.ResponseWriter, r *http.Request) {
	orgID, ok := s.requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	teamID, err := s.az.ResolveTeamID(r.Context(), orgID, userID, r.PathValue("team_id"))
	if err != nil {
		authz.WriteResolveError(w, "teams/github-repos", err)
		return
	}
	if !s.az.VerifyTeamInOrg(w, r, orgID, userID, teamID) {
		return
	}

	_, role, err := s.az.TeamMemberCountAndRole(r.Context(), orgID, userID, teamID)
	if err != nil {
		internalError(w, "teams/github-repos", err)
		return
	}

	host, err := s.orgGitHubHost(r.Context(), orgID)
	if err != nil {
		internalError(w, "teams/github-repos", err)
		return
	}

	// No GitHub URL, no host, and so nothing tracked on it.
	var repos []domain.TeamGitHubRepo
	if err := s.tx.WithReadTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		if host == "" {
			return nil
		}
		var e error
		repos, e = tx.TeamGitHubRepos.ListForTeam(r.Context(), teamID, host)
		return e
	}); err != nil {
		internalError(w, "teams/github-repos", err)
		return
	}

	writeJSON(w, http.StatusOK, teamReposResponse{
		Repos: trackedReposJSON(repos),
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
		authz.WriteResolveError(w, "teams/github-repos", err)
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

	var req teamReposPutRequest
	if !httpx.DecodeJSONStrict(w, r, &req) {
		return
	}
	ids, ok := parseRepositoryIDs(w, req.RepositoryIDs)
	if !ok {
		return
	}

	host, ok := s.requireOrgGitHubHost(r.Context(), w, orgID, "teams/github-repos")
	if !ok {
		return
	}

	// Every id the team does not track yet is resolved to its registry row,
	// and the ones that resolve are gated on reachability. An id the team
	// already tracks is neither: a repository that became unreachable *after*
	// being tracked (creds revoked, App uninstalled) must stay in a save, or
	// the picker wedges — every Save re-submits it, so the user could never
	// save a corrected set. Only an *added* repository is a user error worth
	// blocking.
	var (
		added    []domain.Repository
		addedAt  []int
		refusals httpx.Validation
	)
	if err := s.tx.WithReadTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		existing, err := tx.TeamGitHubRepos.ListForTeam(r.Context(), teamID, host)
		if err != nil {
			return err
		}
		tracked := make(map[string]bool, len(existing))
		for _, e := range existing {
			tracked[e.RepositoryID] = true
		}
		for i, id := range ids {
			if tracked[id] {
				continue
			}
			row, err := tx.Repos.Get(r.Context(), orgID, id)
			if errors.Is(err, db.ErrNoSuchRepository) || (err == nil && row.Host != host) {
				refusals.Invalid(repositoryIDField(i), "repository "+id+" is not a repository on this organization's GitHub host")
				continue
			}
			if err != nil {
				return err
			}
			added = append(added, *row)
			addedAt = append(addedAt, i)
		}
		return nil
	}); err != nil {
		internalError(w, "teams/github-repos", err)
		return
	}
	// A refusal names the repository by the id the caller sent, never by its
	// name: an added repository may be in none of the caller's teams' tracked
	// sets, and the registry read 404s such a row to anyone but an org admin.
	for j, bad := range s.unreachableRepos(r.Context(), orgID, userID, host, added) {
		if bad {
			refusals.Invalid(repositoryIDField(addedAt[j]),
				"repository "+added[j].ID+" is not reachable by this organization's GitHub credentials — install the GitHub App on it or grant the PAT access first")
		}
	}
	if refusals.Flush(w, http.StatusUnprocessableEntity) {
		return
	}

	var stored []domain.TeamGitHubRepo
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		if err := tx.TeamGitHubRepos.ReplaceForTeam(r.Context(), orgID, teamID, host, ids); err != nil {
			return err
		}
		var e error
		stored, e = tx.TeamGitHubRepos.ListForTeam(r.Context(), teamID, host)
		return e
	}); err != nil {
		// A repository deleted between the check above and the write.
		if errors.Is(err, db.ErrNoSuchRepository) {
			httpx.WriteErrors(w, http.StatusUnprocessableEntity, httpx.ErrorItem{
				Reason: httpx.ReasonInvalidField, Message: "a repository in the set no longer exists", Field: "repository_ids",
			})
			return
		}
		internalError(w, "teams/github-repos", err)
		return
	}

	// Repo set changed → re-due polling so the poller picks up the
	// reconciled tracked set now (onGitHubChanged evicts the reachable-repo
	// cache + PollSoon), and force a profiling pass so newly-tracked repos
	// are profiled immediately rather than after one poll interval. This is
	// also the path the "Re-profile" button takes (it re-PUTs the current
	// selection), so the force is what makes that button bypass the TTL.
	//
	// This is GitHub-only and no longer touches the Jira poller: onGitHubChanged
	// shed its old RestartAll (which used to bounce Jira too), so there's no
	// Jira readiness to reset here — a repo-set change leaves Jira snapshots
	// valid and /api/jira/stock ungated.
	if s.onGitHubChanged != nil {
		go s.onGitHubChanged(orgID)
	}
	if s.profilerTrigger != nil {
		s.profilerTrigger(orgID, true)
	}

	writeJSON(w, http.StatusOK, teamReposPutResponse{Repos: trackedReposJSON(stored)})
}

// parseRepositoryIDs validates the PUT body's shape: the field present, and
// every entry a registry id in canonical form, named once. Every fault is
// reported, and it writes the 400 on a false return. Whether an id names a
// repository is the handler's next question, answered against the store.
func parseRepositoryIDs(w http.ResponseWriter, raw *[]string) ([]string, bool) {
	var v httpx.Validation
	if raw == nil {
		v.Missing("repository_ids")
		v.Flush(w, http.StatusBadRequest)
		return nil, false
	}
	ids := make([]string, 0, len(*raw))
	first := make(map[string]int, len(*raw))
	for i, id := range *raw {
		field := repositoryIDField(i)
		if id == "" {
			v.Missing(field)
			continue
		}
		// Canonical form only: a registry id is minted lowercase-hyphenated
		// and stored as text in local mode, so any other spelling of the same
		// uuid would name no row there.
		if parsed, err := uuid.Parse(id); err != nil || parsed.String() != id {
			v.Invalid(field, id+" is not a repository id")
			continue
		}
		if j, dup := first[id]; dup {
			v.Invalid(field, fmt.Sprintf("repository %s is already named at repository_ids[%d]", id, j))
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

func repositoryIDField(i int) string { return fmt.Sprintf("repository_ids[%d]", i) }

// trackedReposJSON renders tracked repos for the wire.
func trackedReposJSON(repos []domain.TeamGitHubRepo) []trackedRepoJSON {
	out := make([]trackedRepoJSON, 0, len(repos))
	for _, r := range repos {
		out = append(out, trackedRepoJSON{
			ID: r.RepositoryID, GitHubID: r.ExternalID, Owner: r.Owner, Repo: r.Repo,
			RetiredAt: optRFC3339(r.RetiredAt),
		})
	}
	return out
}

// reachableGateMaxAge is how old the reachable mirror may be and still decide a
// write. It is stated here rather than inherited so the gate's staleness posture
// is a decision someone made, and the decision is: THE SAME WINDOW THE PICKER
// SERVES FROM.
//
// The gate's job is to reject what the user could not have seen, and the picker
// is the only place a selection comes from — so trusting the mirror exactly as
// far as the picker does is what makes the two agree. A stricter gate would not
// catch anything the picker let through; it would just re-prove, one GitHub call
// per selected repository, a membership the picker had already asserted from these
// same rows.
//
// What a stricter gate WOULD buy is catching a repository whose access was
// revoked mid-window, and that is deliberately not bought here: the cold path
// below already fails open on every inconclusive answer, so the gate has never
// been the thing that guarantees reachability — the poller is, by no-oping on a
// repo it cannot reach. What would change the decision is the reverse case
// getting expensive: if the mirror's TTL were lengthened to days, "reachable six
// hours ago" and "reachable last week" stop being the same claim, and the gate
// should then pin its own shorter window rather than follow.
const reachableGateMaxAge = reachcache.TTL

// unreachableRepos is the write-time guard that keeps a team from tracking a
// repository this deployment's GitHub credentials can't reach (a stale client,
// a hand-crafted curl, a registry row TF minted for an entity in a repository
// the credential never covered). It reports, index-aligned with repos, which
// are conclusively unreachable, in two tiers, both bounded by *selection* size
// rather than *org* size:
//
//   - Tier 1 (hot path): the durable reachable-repo mirror — the same rows the
//     picker listed the options from. It is one indexed read of just the
//     selected repositories, no GitHub call, and it is authoritative for
//     membership, so unreachableIn decides directly.
//   - Tier 2 (cold path): when the mirror is empty or older than
//     reachableGateMaxAge, probe *only the selected repositories* via per-repo
//     GET /repos/{owner}/{repo}, concurrently, instead of re-enumerating the
//     whole org. See fanOutUnreachableRepos.
//
// The mirror is authoritative because it is shared: every pod reads the same
// rows, across restarts, whichever pod served the picker.
//
// Both tiers preserve the long-standing fail-OPEN posture: if we can't
// reach a conclusive "this repo does not exist for us" answer (no
// credentials, a GitHub outage, a 5xx), the write proceeds. That is not
// laxness — a user can't induce a GitHub outage, so every garbage input
// submittable under normal conditions is still hard-rejected, while we
// avoid coupling our write availability to GitHub's uptime. The residual
// (a write accepted while GitHub was unreachable) is caught by the poller,
// which no-ops on a repo it can't reach.
func (s *Server) unreachableRepos(ctx context.Context, orgID, userID, host string, repos []domain.Repository) []bool {
	if len(repos) == 0 {
		return nil
	}
	if set, ok := s.reachableMirrorSet(ctx, orgID, host, repos); ok {
		return unreachableIn(set, true, repos)
	}
	// Tier 2: cold path. Probe only the selected repositories.
	return s.fanOutUnreachableRepos(ctx, orgID, userID, repos)
}

// reachSet is what the mirror says the org can reach of one selection: the
// GitHub ids it holds, and the lowercased slugs it holds.
type reachSet struct {
	ids   map[string]domain.ReachableRepository
	slugs map[string]struct{}
}

// reaches matches a registry row against the set by the id a rename does not
// move when the row carries one, and by folded name only when it does not —
// a row TF never learned an id for. A row whose id the mirror does not hold is
// not reached through its name: GitHub serves one repository per name, so a
// mirror entry under that name with another id is another repository.
func (s reachSet) reaches(r domain.Repository) bool {
	if r.ExternalID != "" {
		_, ok := s.ids[r.ExternalID]
		return ok
	}
	_, ok := s.slugs[strings.ToLower(r.Slug())]
	return ok
}

// reachableMirrorSet reads which of the selected repositories the mirror says
// the org can reach, or reports that the mirror is not fit to decide this
// write.
//
// Every failure arm falls through to the cold path rather than to a rejection:
// an unreadable class, an unreadable mirror, an empty one, or one past
// reachableGateMaxAge all mean "we don't know from here", and the per-repo
// probe is the thing that knows. Rejecting on any of them would turn a store
// hiccup into a user-facing "that repo doesn't exist".
func (s *Server) reachableMirrorSet(ctx context.Context, orgID, host string, repos []domain.Repository) (reachSet, bool) {
	class, err := s.reachableCredentialClass(ctx, orgID)
	if err != nil {
		reposLog.Warn("resolve reachable credential class failed; probing per repo instead", "org", orgID, "error", err)
		return reachSet{}, false
	}
	state, err := s.reachableRepos.ReachableStateSystem(ctx, orgID, host, class)
	if err != nil {
		reposLog.Warn("read reachable cache state failed; probing per repo instead", "org", orgID, "error", err)
		return reachSet{}, false
	}
	if state.StaleAt(time.Now(), reachableGateMaxAge) {
		// Ask for a refresh on the way past. This write is decided by the probe
		// either way, but the user is mid-flow and their next Save should not pay
		// for the same staleness twice.
		s.kickReachRefresh(orgID, false)
		return reachSet{}, false
	}
	var ids, slugs []string
	for _, r := range repos {
		if r.ExternalID != "" {
			ids = append(ids, r.ExternalID)
		} else {
			slugs = append(slugs, r.Slug())
		}
	}
	var set reachSet
	if set.ids, err = s.reachableRepos.ReachableByExternalIDsSystem(ctx, orgID, host, class, ids); err != nil {
		reposLog.Warn("read reachable ids failed; probing per repo instead", "org", orgID, "error", err)
		return reachSet{}, false
	}
	if set.slugs, err = s.reachableRepos.ReachableSlugsSystem(ctx, orgID, host, class, slugs); err != nil {
		reposLog.Warn("read reachable slugs failed; probing per repo instead", "org", orgID, "error", err)
		return reachSet{}, false
	}
	return set, true
}

// unreachableIn is the pure policy: given the reachable set and whether it's
// authoritative (checked), report which repos it does not reach.
// checked=false → never reject (fail open).
func unreachableIn(set reachSet, checked bool, repos []domain.Repository) []bool {
	out := make([]bool, len(repos))
	if !checked {
		return out
	}
	for i, r := range repos {
		out[i] = !set.reaches(r)
	}
	return out
}

// reachabilityFanOutCap bounds how many per-repo reachability probes run
// concurrently on the cold path. 20 in flight keeps a large selection from
// opening hundreds of sockets at once while still draining the selection in a
// handful of waves (200 repos / 20 → 10 waves).
const reachabilityFanOutCap = 20

// reachabilityCallTimeout caps each per-repo GET. A slow GHES instance must
// not let one probe stall the whole write; a timed-out probe is treated as
// indeterminate (fail open for that repository), same as a 5xx.
const reachabilityCallTimeout = 3 * time.Second

// fanOutUnreachableRepos is the cold-path reachability check: on a cache miss
// it probes only the *selected* repositories (not the whole org) by issuing
// per-repo GET /repos/{owner}/{repo} across every credential tier the
// deployment has, concurrently, and reports which are *conclusively*
// unreachable by all of them. The probe asks by the name TF stores, which
// still answers after a rename TF has not applied: GitHub redirects a
// renamed repository's old name.
//
// "Conclusively" carries the fail-open posture down to the per-repo level: a
// repository is rejected only when at least one credential returned a
// definitive 404/403 and none returned 200; if every credential was
// indeterminate (5xx/timeout) it fails open. With no credentials at all there
// are no clients, so nothing is rejected.
func (s *Server) fanOutUnreachableRepos(ctx context.Context, orgID, userID string, repos []domain.Repository) []bool {
	clients := s.reachabilityClients(ctx, orgID, userID)
	if len(clients) == 0 {
		// No credentials resolved → fail open. A user can't induce this, so it
		// isn't an escape hatch.
		return make([]bool, len(repos))
	}
	check := func(ctx context.Context, repo domain.Repository) (reachable, conclusive bool) {
		return repoReachableByAny(ctx, clients, repo)
	}
	return unreachableViaFanOut(ctx, repos, reachabilityFanOutCap, check)
}

// repoReachableByAny probes one repository across every credential tier:
// reachable by the PAT *or* any App installation counts as reachable. Returns:
//
//   - (true, true)  — some client got a 200.
//   - (false, true) — no client got a 200 and at least one got a definitive
//     404/403 (conclusively unreachable).
//   - (false, false) — every client was indeterminate (5xx/timeout); the
//     caller fails open for this repository.
func repoReachableByAny(ctx context.Context, clients []*ghclient.Client, repo domain.Repository) (reachable, conclusive bool) {
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

// reachabilityCheck reports, for a single repository, whether it is reachable
// and whether that answer is conclusive — see CheckRepoAccess. Factored out so
// the fan-out orchestration (unreachableViaFanOut) is a pure function over an
// injectable check, testable without a live GitHub.
type reachabilityCheck func(ctx context.Context, repo domain.Repository) (reachable, conclusive bool)

// unreachableViaFanOut runs check over every repo concurrently (at most
// maxInFlight in flight) and reports, index-aligned with repos, which are
// conclusively unreachable (reachable=false AND conclusive=true). A repository
// whose check is indeterminate (conclusive=false) is never rejected — that is
// the per-repo fail-open posture.
func unreachableViaFanOut(ctx context.Context, repos []domain.Repository, maxInFlight int, check reachabilityCheck) []bool {
	if maxInFlight < 1 {
		maxInFlight = 1
	}
	unreachable := make([]bool, len(repos))
	sem := make(chan struct{}, maxInFlight)
	var wg sync.WaitGroup
	for i, repo := range repos {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, repo domain.Repository) {
			defer wg.Done()
			defer func() { <-sem }()
			r, conc := check(ctx, repo)
			unreachable[i] = conc && !r
		}(i, repo)
	}
	wg.Wait()
	return unreachable
}

// reachabilityClients builds the GitHub clients the cold-path fan-out probes
// against, one per credential tier the org has — the org PAT plus each App
// installation (PAT from integrations.Load, installations via the resolver).
// It returns *clients* rather than enumerating repos through them, because the
// fan-out asks each client about specific repositories instead of listing the
// world.
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
	_ = s.tx.WithReadTx(ctx, orgID, userID, func(tx db.TxStores) error {
		creds, _ = integrations.Load(ctx, tx.Secrets, orgID)
		var e error
		orgSet, e = tx.Orgs.GetSettings(ctx, orgID)
		return e
	})
	if creds.GitHubPAT != "" && orgSet.GitHubBaseURL != "" {
		clients = append(clients, ghclient.NewClient(orgSet.GitHubBaseURL, creds.GitHubPAT).WithOrg(orgID).
			WithCredential(ghclient.CredentialPAT))
	}

	// App installation sources, one client per account installed on the org's
	// current host — the installations the resolver mints from. Each
	// installation token can reach a distinct repo set; probing all of them
	// preserves the old union reachability.
	if s.ghResolver != nil && s.githubApps != nil {
		insts, err := s.installationsOnOrgHost(ctx, orgID)
		if err == nil {
			for _, inst := range insts {
				client, err := s.ghResolver.ClientForInstallation(ctx, orgID, inst)
				if err != nil {
					continue
				}
				clients = append(clients, client)
			}
		}
	}

	return clients
}
