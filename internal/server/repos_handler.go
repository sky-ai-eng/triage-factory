package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/auth"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/integrations"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// handleGitHubRepos returns the repositories the Settings picker offers. The
// source is tier-aware:
//
//   - App org (own App registered + active + ≥1 installation): the union of
//     each installation's repositories (GET /installation/repositories). A
//     GitHub App installation token can't call GET /user/repos — it 4xxs — and
//     under an App the only repos worth offering are the ones the App is
//     installed on.
//   - PAT-only org (no App / no installations): the authenticated user's repos
//     (GET /user/repos) — identical to the pre-App behavior.
func (s *Server) handleGitHubRepos(w http.ResponseWriter, r *http.Request) {
	orgID := OrgIDFrom(r.Context())
	userID := ClaimsFrom(r.Context()).Subject

	// Decide App-vs-PAT here rather than widening the resolver (which
	// deliberately hides which tier resolved): this handler is the one consumer
	// that must branch, so it reads the App registration + installations
	// itself.
	//
	// WHICH source to read is the org's credential class, not whether an App
	// registration happens to exist — a rowless org is a PAT org today and would
	// equally be an org on a deployment-level shared App, which this handler
	// would then render from the wrong source. An unreadable or unrecognised
	// class fails the request: the picker's whole job is to show the grant, and
	// showing the wrong org's idea of a grant is worse than showing an error.
	class, err := s.githubCredentialClass(r.Context(), orgID)
	if err != nil {
		reposLog.Error("resolve github credential class failed", "org", orgID, "error", err)
		internalError(w, "repos", err)
		return
	}

	var (
		app      *domain.OrgGitHubApp
		insts    []domain.OrgGitHubAppInstallation
		instsErr error
	)
	switch class {
	case domain.GitHubCredentialClassPAT:
		// Nothing to read: a PAT org has no App and no installations, and both
		// stay nil so the PAT path below runs exactly as it did pre-App.

	case domain.GitHubCredentialClassBYOApp:
		if s.githubApps != nil {
			// Both reads degrade to the PAT path on error rather than blanking the
			// picker, but log either failure — these silent points are what made the
			// original dead-end untraceable (TFAC-324). instsErr is also load-bearing
			// below: a failed installations read leaves insts nil, which must not
			// masquerade as a positive "installed on zero accounts".
			var appErr error
			app, appErr = s.githubApps.GetForOrgSystem(r.Context(), orgID)
			if appErr != nil {
				reposLog.Warn("read app registration failed, falling back to pat path", "org", orgID, "error", appErr)
			}
			insts, instsErr = s.githubApps.ListInstallationsForOrgSystem(r.Context(), orgID)
			if instsErr != nil {
				reposLog.Warn("list installations failed, falling back to pat path", "org", orgID, "error", instsErr)
			}
		}

	default:
		// Unreachable: githubCredentialClass already refused an unknown class.
		// Kept explicit so a class added to the type without a decision here
		// fails visibly rather than rendering the picker from the PAT source.
		reposLog.Error("unhandled github credential class", "org", orgID, "class", class)
		internalError(w, "repos", ErrUnknownGitHubCredentialClass)
		return
	}

	var repos []ghclient.UserRepo
	if app != nil && app.Active && len(insts) > 0 {
		var err error
		repos, err = s.installationReposUnion(r.Context(), orgID, insts)
		if err != nil {
			if errors.Is(err, ghclient.ErrNoGitHubCredentials) {
				reposLog.Warn("app installed but resolver produced no credentials for any installation", "org", orgID, "error", err)
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "GitHub not configured"})
				return
			}
			reposLog.Warn("fetch installation repos failed", "org", orgID, "error", err)
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "failed to fetch repos from GitHub" + localDetail(err)})
			return
		}
	} else {
		// PAT path — identical to the pre-App behavior. Credentials + per-org
		// base URL read through the app pool inside WithTx so SecretStore
		// decrypts under the user's claims and org_settings_select RLS is
		// enforced — same shape the rest of the settings surface
		// uses.
		var (
			creds  auth.Credentials
			orgSet domain.OrgSettings
		)
		if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
			creds, _ = integrations.Load(r.Context(), tx.Secrets, orgID)
			var lerr error
			orgSet, lerr = tx.Orgs.GetSettings(r.Context(), orgID)
			return lerr
		}); err != nil {
			internalError(w, "repos", err)
			return
		}
		if creds.GitHubPAT == "" || creds.GitHubURL == "" {
			// An active App installed on zero accounts dead-ends here in the
			// PAT fallback — no installation token to use, no PAT to borrow.
			// Surface that as its own 400 the picker can key install guidance
			// off, instead of the generic "not configured" that reads as
			// "add a PAT". This is the first-run dead-end TFAC-324 unblocks.
			//
			// A non-nil app here means the class switch above put it there, so
			// this message is only ever offered to an org that really is in the
			// BYO-App system — "install your App" is never shown to a PAT org
			// that merely failed to bind a token.
			//
			// instsErr == nil is load-bearing: only a *successful* read of zero
			// installations means "not installed". A failed read also leaves
			// insts empty, and reporting that as "not installed" would mislead
			// on a transient store error — so it falls through to the generic
			// message instead.
			if app != nil && app.Active && instsErr == nil && len(insts) == 0 {
				reposLog.Warn("app registered and active but installed on zero accounts, and no pat configured", "org", orgID)
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "GitHub App is not installed on any account"})
				return
			}
			reposLog.Warn("github not configured, no usable app installation and no pat", "org", orgID)
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "GitHub not configured"})
			return
		}
		baseURL := orgSet.GitHubBaseURL
		if baseURL == "" {
			baseURL = creds.GitHubURL
		}

		client := ghclient.NewClient(baseURL, creds.GitHubPAT)
		var err error
		repos, err = client.ListUserRepos(r.Context())
		if err != nil {
			reposLog.Warn("fetch user repos failed", "org", orgID, "error", err)
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "failed to fetch repos from GitHub" + localDetail(err)})
			return
		}
	}

	// Warm the reachable-repo enumeration cache so the
	// immediate-next team-repos PUT validates the selection against this
	// in-memory set in ~µs instead of re-enumerating the org. We just paid
	// for the enumeration; the gate shouldn't pay for it again. Only the
	// lowercased slug set is needed for the membership check, so we keep
	// that rather than the full UserRepo slice. TTL-bounded and evicted on
	// SetOnGitHubChanged.
	//
	// NB: warmed from whatever source produced `repos` — the App
	// installation-repos union for App orgs, ListUserRepos for PAT-only orgs.
	// Either way the cached set is exactly what the picker just showed, so the
	// hot-path gate can't reject a repo the user could actually have selected.
	slugs := make(map[string]struct{}, len(repos))
	for _, repo := range repos {
		if repo.FullName != "" {
			slugs[strings.ToLower(repo.FullName)] = struct{}{}
		}
	}
	s.reachableRepoCachePut(orgID, userID, slugs)

	writeJSON(w, http.StatusOK, repos)
}

// installationReposUnion lists each installation's repositories through the
// credential resolver (one tier-1 App-token client per installation account)
// and returns their union, deduped by lowercased full name and sorted stably
// by full name. This is the App-org replacement for ListUserRepos: an
// installation token can't call GET /user/repos, and the union is exactly the
// set of repos the org's App can act on.
//
// Per-installation failures are isolated — a bad mint or list on one account
// must not blank the whole picker, so they're logged and skipped while the
// rest still contribute. ErrNoGitHubCredentials is only surfaced when the
// resolver produced no client for any installation (genuine "not configured").
// But if clients resolved yet *every* list attempt failed and the union is
// empty, the failure is surfaced rather than returned as an empty list — an
// empty result there is a fetch error masquerading as "no repos" (and would
// also warm the reachable-repo cache with an empty set).
func (s *Server) installationReposUnion(ctx context.Context, orgID string, insts []domain.OrgGitHubAppInstallation) ([]ghclient.UserRepo, error) {
	if s.ghResolver == nil {
		return nil, ghclient.ErrNoGitHubCredentials
	}

	byName := make(map[string]ghclient.UserRepo)
	resolvedAny := false
	var lastListErr error
	for _, inst := range insts {
		client, err := s.ghResolver.ClientFor(ctx, orgID, inst.AccountLogin)
		if err != nil {
			if errors.Is(err, ghclient.ErrNoGitHubCredentials) {
				continue
			}
			// Real DB/vault/RLS failure — propagate so the handler can 502
			// rather than silently returning a partial union.
			return nil, err
		}
		resolvedAny = true

		repos, err := client.ListInstallationRepos(ctx)
		if err != nil {
			reposLog.Warn("list installation repos failed, skipping installation", "org", orgID, "account", inst.AccountLogin, "error", err)
			lastListErr = err
			continue
		}
		for _, repo := range repos {
			if repo.FullName == "" {
				continue
			}
			key := strings.ToLower(repo.FullName)
			if _, ok := byName[key]; !ok {
				byName[key] = repo
			}
		}
	}
	if !resolvedAny {
		return nil, ghclient.ErrNoGitHubCredentials
	}
	// Clients resolved but produced nothing, and at least one list errored:
	// surface the fetch failure instead of an empty (and misleading) picker.
	// A genuinely empty union with no errors (App installed on zero repos) is
	// a legitimate empty result and falls through.
	if len(byName) == 0 && lastListErr != nil {
		return nil, fmt.Errorf("list installation repositories for org %s: %w", orgID, lastListErr)
	}

	out := make([]ghclient.UserRepo, 0, len(byName))
	for _, repo := range byName {
		out = append(out, repo)
	}
	sort.SliceStable(out, func(i, j int) bool {
		return strings.ToLower(out[i].FullName) < strings.ToLower(out[j].FullName)
	})
	return out, nil
}

// isOrgAdmin reports whether userID is an org admin of orgID, short-circuiting
// to true under local mode (N=1 has a single implicit owner and no team
// boundary — mirrors the org-admin gates elsewhere in this package).
func (s *Server) isOrgAdmin(ctx context.Context, orgID, userID string) (bool, error) {
	if runmode.Current() == runmode.ModeLocal {
		return true, nil
	}
	return s.az.UserIsOrgAdmin(ctx, userID, orgID)
}

// repoAccessAllowed reports whether the caller may *read* the given repo
// (TFAC-559): an org admin always may; a non-admin member may only when the
// repo is tracked by at least one of their teams. Local mode (N=1) has no
// team boundary and is covered by isOrgAdmin's short-circuit.
//
// Mutations are a strictly narrower gate — see repoMutationAccess.
func (s *Server) repoAccessAllowed(ctx context.Context, orgID, userID, owner, repo string) (bool, error) {
	isAdmin, err := s.isOrgAdmin(ctx, orgID, userID)
	if err != nil {
		return false, err
	}
	if isAdmin {
		return true, nil
	}
	var tracked bool
	err = s.tx.WithTx(ctx, orgID, userID, func(tx db.TxStores) error {
		var e error
		tracked, e = tx.TeamGitHubRepos.TracksRepoViewerScoped(ctx, orgID, owner, repo)
		return e
	})
	return tracked, err
}

// repoWriteAccess is the outcome of the repo-mutation gate. Three-valued
// rather than a bool because "may not" splits into two different answers to
// the caller: a repo they can't see at all versus one they can see but may
// not change.
type repoWriteAccess int

const (
	// repoWriteInvisible — the repo is outside the caller's tracked set.
	// It isn't in their GET /api/repos list either, so the handler answers
	// 404 and discloses nothing about whether it exists. Deliberately the
	// zero value, so a repoWriteAccess that was never assigned denies
	// rather than permits.
	repoWriteInvisible repoWriteAccess = iota
	// repoWriteForbidden — the caller can see the repo but administers
	// none of the teams tracking it. 404 here would contradict their own
	// repo list, so this is an honest 403.
	repoWriteForbidden
	// repoWriteAllowed — org admin, or team admin of a tracking team.
	repoWriteAllowed
)

// repoMutationAccess resolves the write gate for a repo's org-wide
// configuration. repo_profiles carries no team_id, so a write by any member
// of any tracking team lands on every tracking team's runs — membership is
// therefore the read gate only, and mutating requires an admin: org admin,
// or team admin of at least one team that tracks the repo.
//
// There is no DB backstop for this. repo_profiles_all's RLS is org-wide by
// construction (it cannot express team scoping for an entity that has no
// team), so this predicate is the whole enforcement — keep it here, in front
// of every mutating repo handler.
//
// Local mode (N=1) resolves to repoWriteAllowed via isOrgAdmin's
// short-circuit, before any store call.
func (s *Server) repoMutationAccess(ctx context.Context, orgID, userID, owner, repo string) (repoWriteAccess, error) {
	isAdmin, err := s.isOrgAdmin(ctx, orgID, userID)
	if err != nil {
		return repoWriteInvisible, err
	}
	if isAdmin {
		return repoWriteAllowed, nil
	}

	var teamAdmin, tracked bool
	if err := s.tx.WithTx(ctx, orgID, userID, func(tx db.TxStores) error {
		var e error
		teamAdmin, e = tx.TeamGitHubRepos.TracksRepoViewerAdminScoped(ctx, orgID, owner, repo)
		if e != nil || teamAdmin {
			// A team admin of a tracking team is necessarily a member of
			// one, so the visibility read below would only confirm what we
			// already know.
			return e
		}
		tracked, e = tx.TeamGitHubRepos.TracksRepoViewerScoped(ctx, orgID, owner, repo)
		return e
	}); err != nil {
		return repoWriteInvisible, err
	}

	switch {
	case teamAdmin:
		return repoWriteAllowed, nil
	case tracked:
		return repoWriteForbidden, nil
	default:
		return repoWriteInvisible, nil
	}
}

// handleRepoProfiles returns configured repo profiles from the DB. Org
// admins get the org-wide union; non-admin members get only the repos
// tracked by their own teams (TFAC-559) — repo_profiles is org-wide, so an
// unscoped read would leak every team's repos to a teammate on none of them.
// Local mode (N=1) has no team boundary and stays unscoped.
func (s *Server) handleRepoProfiles(w http.ResponseWriter, r *http.Request) {
	orgID, ok := s.requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject

	isAdmin, err := s.isOrgAdmin(r.Context(), orgID, userID)
	if err != nil {
		internalError(w, "repos", err)
		return
	}

	// canEdit mirrors the PATCH gate (repoMutationAccess) per row, so the
	// page can render a read-only base branch instead of a control that
	// 403s on use. The client must not re-derive this from role — org admin
	// and team admin are orthogonal, and only the server knows which teams
	// tracking a given repo the caller administers.
	var (
		profiles []domain.RepoProfile
		canEdit  []bool
	)
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		if isAdmin {
			profiles, e = tx.Repos.List(r.Context(), orgID)
		} else {
			profiles, e = tx.Repos.ListTeamScoped(r.Context(), orgID)
		}
		if e != nil {
			return e
		}
		canEdit = make([]bool, len(profiles))
		if isAdmin {
			for i := range canEdit {
				canEdit[i] = true
			}
			return nil
		}
		// One EXISTS per row. The list is already narrowed to the caller's
		// own teams' tracked repos, so this is bounded by their tracked set
		// and runs inside the single transaction the list read used.
		for i, p := range profiles {
			canEdit[i], e = tx.TeamGitHubRepos.TracksRepoViewerAdminScoped(r.Context(), orgID, p.Owner, p.Repo)
			if e != nil {
				return e
			}
		}
		return nil
	}); err != nil {
		internalError(w, "repos", err)
		return
	}

	type repoJSON struct {
		ID             string  `json:"id"`
		Owner          string  `json:"owner"`
		Repo           string  `json:"repo"`
		Description    string  `json:"description,omitempty"`
		HasReadme      bool    `json:"has_readme"`
		HasClaudeMd    bool    `json:"has_claude_md"`
		HasAgentsMd    bool    `json:"has_agents_md"`
		ProfileText    string  `json:"profile_text,omitempty"`
		DefaultBranch  string  `json:"default_branch,omitempty"`
		BaseBranch     string  `json:"base_branch,omitempty"`
		ProfiledAt     *string `json:"profiled_at,omitempty"`
		CloneStatus    string  `json:"clone_status,omitempty"`
		CloneError     string  `json:"clone_error,omitempty"`
		CloneErrorKind string  `json:"clone_error_kind,omitempty"`
		CanEdit        bool    `json:"can_edit"`
	}

	result := make([]repoJSON, len(profiles))
	for i, p := range profiles {
		result[i] = repoJSON{
			ID:             p.ID,
			Owner:          p.Owner,
			Repo:           p.Repo,
			Description:    p.Description,
			HasReadme:      p.HasReadme,
			HasClaudeMd:    p.HasClaudeMd,
			HasAgentsMd:    p.HasAgentsMd,
			ProfileText:    p.ProfileText,
			DefaultBranch:  p.DefaultBranch,
			BaseBranch:     p.BaseBranch,
			CloneStatus:    p.CloneStatus,
			CloneError:     p.CloneError,
			CloneErrorKind: p.CloneErrorKind,
			CanEdit:        canEdit[i],
		}
		if p.ProfiledAt != nil {
			t := p.ProfiledAt.UTC().Format(time.RFC3339)
			result[i].ProfiledAt = &t
		}
	}

	writeJSON(w, http.StatusOK, result)
}

// handleRepoUpdate updates per-repo settings like base_branch. The write
// gate is narrower than the read gate: a repo profile is org-wide, so
// changing one changes behaviour for every team tracking it, and that takes
// an admin — org admin, or team admin of a tracking team. Plain members of a
// tracking team read the repo but can't repoint it.
func (s *Server) handleRepoUpdate(w http.ResponseWriter, r *http.Request) {
	orgID, ok := s.requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	owner := r.PathValue("owner")
	repo := r.PathValue("repo")
	if owner == "" || repo == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing owner/repo"})
		return
	}
	repoID := owner + "/" + repo

	access, err := s.repoMutationAccess(r.Context(), orgID, userID, owner, repo)
	if err != nil {
		internalError(w, "repos", err)
		return
	}
	// Every arm is spelled out, including the permitting one, and anything
	// unrecognized denies. This is the only enforcement point for an
	// org-wide write (RLS can't back it up), so a future enum member must
	// fail closed by default rather than reach the write by falling off the
	// end of the switch.
	switch access {
	case repoWriteAllowed:
		// Proceed to the write below.
	case repoWriteInvisible:
		// 404, not 403 — a repo outside the caller's tracked set doesn't
		// appear in their GET /api/repos list either, so don't disclose
		// its existence via a role-boundary error.
		notFound(w, "repo")
		return
	case repoWriteForbidden:
		// The inverse of the above: this repo *is* in the caller's own
		// GET /api/repos list, so 404 would read as a bug. Say plainly
		// that it's a permission boundary.
		writeJSON(w, http.StatusForbidden, map[string]string{
			"error": "changing repo settings requires org admin or team admin of a team tracking this repo",
		})
		return
	default:
		internalError(w, "repos", fmt.Errorf("unhandled repo write access %d for %s", access, repoID))
		return
	}

	// Use json.RawMessage to distinguish null (clear) from omitted (no change).
	// *string can't tell them apart — both decode to nil.
	var req struct {
		BaseBranch json.RawMessage `json:"base_branch,omitempty"`
	}
	if !decodeJSON(w, r, &req, "") {
		return
	}

	if req.BaseBranch != nil {
		var branch string
		if string(req.BaseBranch) == "null" {
			branch = "" // explicit null → clear
		} else if err := json.Unmarshal(req.BaseBranch, &branch); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid base_branch value"})
			return
		}
		if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
			return tx.Repos.UpdateBaseBranch(r.Context(), orgID, repoID, branch)
		}); err != nil {
			internalError(w, "repos", err)
			return
		}
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "updated"})
}

// handleRepoBranches returns branches for a repo, with optional search
// filtering. Gated the same as handleRepoProfiles/handleRepoUpdate
// (TFAC-559): a non-admin member can only list branches for a repo their own
// team tracks.
func (s *Server) handleRepoBranches(w http.ResponseWriter, r *http.Request) {
	owner := r.PathValue("owner")
	repo := r.PathValue("repo")
	if owner == "" || repo == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing owner/repo"})
		return
	}

	orgID, ok := s.requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject

	if allowed, err := s.repoAccessAllowed(r.Context(), orgID, userID, owner, repo); err != nil {
		internalError(w, "repos", err)
		return
	} else if !allowed {
		notFound(w, "repo")
		return
	}

	// Resolve per-repo (org App installation token → PAT) so App-only orgs
	// (no PAT) list branches through the installation client instead of 400-ing
	// on a nil global client. owner/repo are path values; a selective App
	// install that doesn't cover this repo falls through to the PAT.
	client, err := s.ghResolver.ClientForRepo(r.Context(), orgID, owner, repo)
	if err != nil {
		if errors.Is(err, ghclient.ErrNoGitHubCredentials) {
			reposLog.Warn("github not configured", "org", orgID, "owner", owner, "repo", repo, "error", err)
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "GitHub not configured"})
			return
		}
		internalError(w, "repos", err)
		return
	}

	query := r.URL.Query().Get("q")
	branches, err := client.ListBranches(r.Context(), owner, repo, query, 30)
	if err != nil {
		reposLog.Warn("fetch branches failed", "org", orgID, "owner", owner, "repo", repo, "error", err)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "failed to fetch branches from GitHub" + localDetail(err)})
		return
	}

	// Return just the names for simplicity
	names := make([]string, len(branches))
	for i, b := range branches {
		names[i] = b.Name
	}
	writeJSON(w, http.StatusOK, names)
}

// Repo *tracking* selection is per-team: writes go through
// PUT /api/settings/team/{id}/repos (handleTeamReposPut), which writes
// team_github_repos and reconciles the org-wide repo_profiles union. The
// old org-global POST /api/repos was removed in favor of that single,
// team-admin-gated entry point; GET /api/repos (the union profiles),
// PATCH /api/repos/{owner}/{repo} (base_branch), and GET
// /api/repos/{owner}/{repo}/branches remain, scoped per-caller.
//
// Reads (isOrgAdmin / repoAccessAllowed) go by membership: an org admin
// sees every repo, a member only the repos their own team(s) track. The
// PATCH gate (repoMutationAccess) is narrower still, because a repo profile
// is org-wide and has no team to write against: it takes an org admin or a
// team admin of a team that tracks the repo.
