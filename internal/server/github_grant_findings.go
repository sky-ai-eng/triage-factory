package server

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// The two grant findings — what the reachable-repo mirror exists to make
// computable, served to the organization settings "GitHub access" panel:
//
//   - REACH WITHOUT PURPOSE: the App can reach a repository no team tracks. TF
//     holds write access to code nobody asked it to touch. The verb is on
//     GitHub — narrow the grant on the installation's settings page, or
//     uninstall.
//   - SCOPE DRIFT: a team tracks a repository outside the grant, so it is
//     silently unpolled. The verb is widening the grant on the same page, or
//     untracking the repository. This is the one that fails invisibly under a
//     selective grant: a repository created after the grant was chosen is
//     invisible forever, with no signal until a call fails.
//
// Both are computed server-side from the mirror, never by asking GitHub on
// page load — opening the panel is a read, and the refresh POST beside the
// status route is the deliberate gesture for a fresh answer. Both read only
// App-class rows: a PAT's reach is a fact about a person's token, not a grant
// TF holds, so a PAT org is refused rather than answered with an empty
// finding that would read as an all-clear.
//
// Neither is derived from the viewer's own GitHub permissions. Two admins with
// different GitHub access see byte-identical lists: delegation mints from the
// grant and never consults anyone's personal access, so a list narrowed to the
// viewer would describe nothing about capability and only mislead.

// grantFindingListRequest is the body of both list routes: the paging pair and
// no filters of its own — a finding is the whole set, and the total is the
// figure the panel renders beside its heading.
type grantFindingListRequest struct {
	httpx.PageRequest
}

// reachWithoutPurposeItem is one repository the grant reaches that nobody
// tracks. installation_id and account_login name the installation whose grant
// carries it, and settings_url is that installation's page on GitHub, where
// the grant is narrowed — the finding's verb, carried on the row so a headless
// caller has it without a second read. github_id is GitHub's id for the
// repository ("" when the listing carried none), which with installation_id
// tells one item from another; repository_id is the registry row TF holds for
// it, "" when it holds none.
type reachWithoutPurposeItem struct {
	InstallationID string `json:"installation_id" doc:"GitHub's id for the installation whose grant reaches the repository."`
	AccountLogin   string `json:"account_login" doc:"The login of the GitHub account the installation is on."`
	SettingsURL    string `json:"settings_url" doc:"The installation's settings page on GitHub, where the repositories it grants are chosen."`
	GitHubID       string `json:"github_id" doc:"GitHub's id for the repository, empty when GitHub's listing gave none."`
	RepositoryID   string `json:"repository_id" doc:"The repository's id in Triage Factory, empty when Triage Factory holds no record of it."`
	Owner          string `json:"owner" doc:"The account that owns the repository on GitHub."`
	Repo           string `json:"repo" doc:"The repository's name without its owner."`
	Slug           string `json:"slug" doc:"The repository's full name, as owner/name."`
	Private        bool   `json:"private" doc:"Whether the repository is private on GitHub."`
	HTMLURL        string `json:"html_url" doc:"The repository's page on GitHub."`
	// ObservedAt is when the refresh that observed this reach ran, RFC3339 —
	// the "as of" the finding is true at.
	ObservedAt string `json:"observed_at" doc:"When the installation refresh that found the repository in the grant ran, as an RFC 3339 timestamp."`
}

// scopeDriftItem is one tracked repository outside the grant. installation_id
// names the live installation on the repository's owner account when there is
// one — by construction a selective grant, since a grant of everything cannot
// drift and one of unknown width is not reported — and "" when no live
// installation covers the account at all, where the way out is connecting the
// account or untracking the repository rather than editing a grant.
// repository_id is the tracked registry row the item is about, and github_id
// its GitHub id ("" while TF has not learned it).
type scopeDriftItem struct {
	RepositoryID   string `json:"repository_id" doc:"The tracked repository's id in Triage Factory."`
	GitHubID       string `json:"github_id" doc:"GitHub's id for the repository, empty while it is not yet known."`
	Owner          string `json:"owner" doc:"The account that owns the repository on GitHub."`
	Repo           string `json:"repo" doc:"The repository's name without its owner."`
	Slug           string `json:"slug" doc:"The repository's full name, as owner/name."`
	InstallationID string `json:"installation_id" doc:"GitHub's id for the installation on the repository's owner account, whose grant leaves the repository out, or empty when no installation is on that account."`
	AccountLogin   string `json:"account_login" doc:"The login of the GitHub account the installation is on, empty when installation_id is."`
	SettingsURL    string `json:"settings_url" doc:"The installation's settings page on GitHub, where the repository can be added to the grant, empty when installation_id is."`
}

// grantFindingsClass is what both routes do before reading: authorize the
// admin and resolve the class the findings are computed under. A PAT
// org is a 404 — it holds no grant, so there is no finding resource to
// list, and the answer is the same one the refresh route gives it. The class
// read is System (claims-free), like every class read beside it: the admin
// gate already authorized orgID.
//
// Admin rather than member, because the findings are the admin's to act on
// and both name repositories across every team in the org — a member on one
// team has no standing to read what another tracks.
func (s *Server) grantFindingsClass(w http.ResponseWriter, r *http.Request) (orgID string, class domain.GitHubCredentialClass, ok bool) {
	orgID, _, ok = s.az.RequireOrgAdmin(w, r)
	if !ok {
		return "", "", false
	}
	class, err := s.githubCredentialClass(r.Context(), orgID)
	if err != nil {
		if errors.Is(err, ErrUnknownGitHubCredentialClass) {
			githubAppLog.Error("unknown github credential class on grant findings read", "org", orgID)
		}
		internalError(w, "github-grant", err)
		return "", "", false
	}
	if !class.AppTier() {
		notFound(w, "github app grant")
		return "", "", false
	}
	return orgID, class, true
}

// liveInstallationsByID indexes the org's live installations on host so a
// finding row can name the account and settings page of the installation it
// belongs to. The findings themselves are read on host, so this is the set
// their installation ids come from. System read: the admin gate authorized
// orgID, and the mirror the findings come from is read the same way.
func (s *Server) liveInstallationsByID(ctx context.Context, orgID, host string) (map[string]domain.OrgGitHubAppInstallation, error) {
	insts, err := s.githubApps.ListInstallationsOnHostSystem(ctx, orgID, host)
	if err != nil {
		return nil, err
	}
	byID := make(map[string]domain.OrgGitHubAppInstallation, len(insts))
	for _, inst := range insts {
		byID[inst.InstallationID] = inst
	}
	return byID, nil
}

var opGitHubGrantReachWithoutPurposeList = httpx.NewListOp[grantFindingListRequest, reachWithoutPurposeItem](httpx.OpSpec{
	Pattern:     "POST /api/orgs/{org_id}/github/grant/reach-without-purpose/list",
	ID:          "list_github_grant_reach_without_purpose",
	Summary:     "List repositories the GitHub App reaches untracked",
	Description: "Returns one page of the repositories the organization's GitHub App installations can reach that no team tracks, as the last installation refresh found them. It reads stored state and asks GitHub nothing.",
	PathParams:  map[string]string{"org_id": "The organization's id."},
	Hints:       httpx.Hints{ReadOnly: httpx.HintYes, OpenWorld: httpx.HintNo},
})

// handleGitHubGrantReachWithoutPurposeList lists the repositories the org's
// App grant reaches that no team tracks.
//
// POST /api/orgs/{org_id}/github/grant/reach-without-purpose/list
func (s *Server) handleGitHubGrantReachWithoutPurposeList(w http.ResponseWriter, r *http.Request) {
	orgID, class, ok := s.grantFindingsClass(w, r)
	if !ok {
		return
	}
	req, ok := opGitHubGrantReachWithoutPurposeList.Decode(w, r)
	if !ok {
		return
	}
	var v httpx.Validation
	page := httpx.ResolvePage(&v, req.PageRequest, httpx.FilterFingerprint(struct{}{}), 0)
	if v.Flush(w, http.StatusBadRequest) {
		return
	}
	ctx := r.Context()
	host, err := s.orgGitHubHost(ctx, orgID)
	if err != nil {
		internalError(w, "github-grant", err)
		return
	}
	if host == "" {
		// No GitHub URL: nothing is reachable, so nothing is a finding.
		opGitHubGrantReachWithoutPurposeList.Write(w, page, []reachWithoutPurposeItem{}, 0)
		return
	}
	rows, total, err := s.reachableRepos.ListReachWithoutPurposeSystem(ctx, orgID, host, class,
		db.ListOpts{Limit: page.Limit, Offset: page.Offset, CountOnly: page.CountOnly})
	if err != nil {
		internalError(w, "github-grant", err)
		return
	}
	// The installations decorate rows; a count-only read has none, so it
	// skips the lookup rather than paying for a decoration nothing wears.
	items := make([]reachWithoutPurposeItem, 0, len(rows))
	if page.CountOnly {
		opGitHubGrantReachWithoutPurposeList.Write(w, page, items, total)
		return
	}
	byID, err := s.liveInstallationsByID(ctx, orgID, host)
	if err != nil {
		internalError(w, "github-grant", err)
		return
	}
	for _, row := range rows {
		inst := byID[row.InstallationID]
		items = append(items, reachWithoutPurposeItem{
			InstallationID: row.InstallationID,
			AccountLogin:   inst.AccountLogin,
			SettingsURL:    installationSettingsURL(inst),
			GitHubID:       row.ExternalID,
			RepositoryID:   row.RepositoryID,
			Owner:          row.Owner,
			Repo:           row.Repo,
			Slug:           row.Slug(),
			Private:        row.Private,
			HTMLURL:        row.HTMLURL,
			ObservedAt:     row.ObservedAt.UTC().Format(time.RFC3339),
		})
	}
	opGitHubGrantReachWithoutPurposeList.Write(w, page, items, total)
}

var opGitHubGrantScopeDriftList = httpx.NewListOp[grantFindingListRequest, scopeDriftItem](httpx.OpSpec{
	Pattern:     "POST /api/orgs/{org_id}/github/grant/scope-drift/list",
	ID:          "list_github_grant_scope_drift",
	Summary:     "List tracked repositories the GitHub App cannot reach",
	Description: "Returns one page of the repositories a team tracks that the organization's GitHub App installations do not grant, as the last installation refresh found them, so they are not polled. It reads stored state and asks GitHub nothing.",
	PathParams:  map[string]string{"org_id": "The organization's id."},
	Hints:       httpx.Hints{ReadOnly: httpx.HintYes, OpenWorld: httpx.HintNo},
})

// handleGitHubGrantScopeDriftList lists the repositories some team tracks that
// the org's App grant does not contain.
//
// POST /api/orgs/{org_id}/github/grant/scope-drift/list
func (s *Server) handleGitHubGrantScopeDriftList(w http.ResponseWriter, r *http.Request) {
	orgID, class, ok := s.grantFindingsClass(w, r)
	if !ok {
		return
	}
	req, ok := opGitHubGrantScopeDriftList.Decode(w, r)
	if !ok {
		return
	}
	var v httpx.Validation
	page := httpx.ResolvePage(&v, req.PageRequest, httpx.FilterFingerprint(struct{}{}), 0)
	if v.Flush(w, http.StatusBadRequest) {
		return
	}
	ctx := r.Context()
	host, err := s.orgGitHubHost(ctx, orgID)
	if err != nil {
		internalError(w, "github-grant", err)
		return
	}
	if host == "" {
		opGitHubGrantScopeDriftList.Write(w, page, []scopeDriftItem{}, 0)
		return
	}
	rows, total, err := s.reachableRepos.ListScopeDriftSystem(ctx, orgID, host, class,
		db.ListOpts{Limit: page.Limit, Offset: page.Offset, CountOnly: page.CountOnly})
	if err != nil {
		internalError(w, "github-grant", err)
		return
	}
	// Same as the reach list: no rows, no decoration, no installation read.
	items := make([]scopeDriftItem, 0, len(rows))
	if page.CountOnly {
		opGitHubGrantScopeDriftList.Write(w, page, items, total)
		return
	}
	byID, err := s.liveInstallationsByID(ctx, orgID, host)
	if err != nil {
		internalError(w, "github-grant", err)
		return
	}
	for _, row := range rows {
		item := scopeDriftItem{
			RepositoryID: row.RepositoryID, GitHubID: row.ExternalID,
			Owner: row.Owner, Repo: row.Repo, Slug: row.Slug(), InstallationID: row.InstallationID,
		}
		// An account no live installation covers names none: the row's id is
		// "" and stays "", and there is no page to send anyone to.
		if inst, covered := byID[row.InstallationID]; covered && row.InstallationID != "" {
			item.AccountLogin = inst.AccountLogin
			item.SettingsURL = installationSettingsURL(inst)
		}
		items = append(items, item)
	}
	opGitHubGrantScopeDriftList.Write(w, page, items, total)
}
