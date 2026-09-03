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
// computable, served to the Workspace Settings "GitHub access" panel:
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
// TF holds, so a PAT workspace is refused rather than answered with an empty
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
// caller has it without a second read.
type reachWithoutPurposeItem struct {
	InstallationID string `json:"installation_id"`
	AccountLogin   string `json:"account_login"`
	SettingsURL    string `json:"settings_url"`
	Owner          string `json:"owner"`
	Repo           string `json:"repo"`
	Slug           string `json:"slug"`
	Private        bool   `json:"private"`
	HTMLURL        string `json:"html_url"`
	// ObservedAt is when the refresh that observed this reach ran, RFC3339 —
	// the "as of" the finding is true at.
	ObservedAt string `json:"observed_at"`
}

// scopeDriftItem is one tracked repository outside the grant. installation_id
// names the live installation on the repository's owner account when there is
// one — by construction a selective grant, since a grant of everything cannot
// drift and one of unknown width is not reported — and "" when no live
// installation covers the account at all, where the way out is connecting the
// account or untracking the repository rather than editing a grant.
type scopeDriftItem struct {
	Owner          string `json:"owner"`
	Repo           string `json:"repo"`
	Slug           string `json:"slug"`
	InstallationID string `json:"installation_id"`
	AccountLogin   string `json:"account_login"`
	SettingsURL    string `json:"settings_url"`
}

// grantFindingsClass is what both routes do before reading: authorize the
// admin and resolve the class the findings are computed under. A PAT
// workspace is a 404 — it holds no grant, so there is no finding resource to
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

// liveInstallationsByID indexes the org's live installations so a finding row
// can name the account and settings page of the installation it belongs to.
// System read: the admin gate authorized orgID, and the mirror the findings
// come from is read the same way.
func (s *Server) liveInstallationsByID(ctx context.Context, orgID string) (map[string]domain.OrgGitHubAppInstallation, error) {
	insts, err := s.githubApps.ListInstallationsForOrgSystem(ctx, orgID)
	if err != nil {
		return nil, err
	}
	byID := make(map[string]domain.OrgGitHubAppInstallation, len(insts))
	for _, inst := range insts {
		byID[inst.InstallationID] = inst
	}
	return byID, nil
}

// handleGitHubGrantReachWithoutPurposeList lists the repositories the org's
// App grant reaches that no team tracks.
//
// POST /api/orgs/{org_id}/github/grant/reach-without-purpose/list
func (s *Server) handleGitHubGrantReachWithoutPurposeList(w http.ResponseWriter, r *http.Request) {
	orgID, class, ok := s.grantFindingsClass(w, r)
	if !ok {
		return
	}
	var req grantFindingListRequest
	if !httpx.DecodeJSONStrict(w, r, &req) {
		return
	}
	var v httpx.Validation
	page := httpx.ResolvePage(&v, req.PageRequest, httpx.FilterFingerprint(struct{}{}), 0)
	if v.Flush(w, http.StatusBadRequest) {
		return
	}
	ctx := r.Context()
	rows, total, err := s.reachableRepos.ListReachWithoutPurposeSystem(ctx, orgID, class,
		db.ListOpts{Limit: page.Limit, Offset: page.Offset, CountOnly: page.CountOnly})
	if err != nil {
		internalError(w, "github-grant", err)
		return
	}
	byID, err := s.liveInstallationsByID(ctx, orgID)
	if err != nil {
		internalError(w, "github-grant", err)
		return
	}
	items := make([]reachWithoutPurposeItem, 0, len(rows))
	for _, row := range rows {
		inst := byID[row.InstallationID]
		items = append(items, reachWithoutPurposeItem{
			InstallationID: row.InstallationID,
			AccountLogin:   inst.AccountLogin,
			SettingsURL:    installationSettingsURL(inst),
			Owner:          row.Owner,
			Repo:           row.Repo,
			Slug:           row.Slug(),
			Private:        row.Private,
			HTMLURL:        row.HTMLURL,
			ObservedAt:     row.ObservedAt.UTC().Format(time.RFC3339),
		})
	}
	httpx.WriteList(w, page, items, total)
}

// handleGitHubGrantScopeDriftList lists the repositories some team tracks that
// the org's App grant does not contain.
//
// POST /api/orgs/{org_id}/github/grant/scope-drift/list
func (s *Server) handleGitHubGrantScopeDriftList(w http.ResponseWriter, r *http.Request) {
	orgID, class, ok := s.grantFindingsClass(w, r)
	if !ok {
		return
	}
	var req grantFindingListRequest
	if !httpx.DecodeJSONStrict(w, r, &req) {
		return
	}
	var v httpx.Validation
	page := httpx.ResolvePage(&v, req.PageRequest, httpx.FilterFingerprint(struct{}{}), 0)
	if v.Flush(w, http.StatusBadRequest) {
		return
	}
	ctx := r.Context()
	rows, total, err := s.reachableRepos.ListScopeDriftSystem(ctx, orgID, class,
		db.ListOpts{Limit: page.Limit, Offset: page.Offset, CountOnly: page.CountOnly})
	if err != nil {
		internalError(w, "github-grant", err)
		return
	}
	byID, err := s.liveInstallationsByID(ctx, orgID)
	if err != nil {
		internalError(w, "github-grant", err)
		return
	}
	items := make([]scopeDriftItem, 0, len(rows))
	for _, row := range rows {
		item := scopeDriftItem{Owner: row.Owner, Repo: row.Repo, Slug: row.Slug(), InstallationID: row.InstallationID}
		// An account no live installation covers names none: the row's id is
		// "" and stays "", and there is no page to send anyone to.
		if inst, covered := byID[row.InstallationID]; covered && row.InstallationID != "" {
			item.AccountLogin = inst.AccountLogin
			item.SettingsURL = installationSettingsURL(inst)
		}
		items = append(items, item)
	}
	httpx.WriteList(w, page, items, total)
}
